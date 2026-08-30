package formalreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/abietic/argus/internal/agentcomponentrepo"
	"github.com/abietic/argus/internal/configdefaults"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	LocalPiBootstrapSchemaVersion        = "argus.local_pi_bootstrap.v1alpha1"
	LocalPiComponentVariantSchemaVersion = "argus.local_pi_component_variant.v1alpha1"
	maxWorkerScriptBytes                 = int64(8 << 20)
	maxBuiltinSkillBytes                 = int64(64 << 10)
)

var builtinReviewSkillIDs = []string{
	"concurrency-data",
	"correctness",
	"error-contract",
	"resource-lifecycle",
	"security-contract",
	"transaction-state",
}

type LocalPiBootstrapOptions struct {
	NodePath              string
	WorkerScript          string
	ProviderProfile       string
	Model                 string
	PromptBundlePath      string
	ReviewSkillPaths      []string
	KnowledgePaths        []string
	NormalizationRevision string
	MaxFiles              int
	MaxGroups             int
	MaxHypotheses         int
	MaxModelCalls         int
	MaxToolCalls          int
	MaxTargetBytes        int64
	MaxGroupBytes         int64
	MaxOutputBytes        int64
	MaxOutputTokens       int64
	MaxCostMicros         int64
	TimeoutMS             int64
	MaxConcurrency        int
	MaxAttempts           int
}

type LocalPiBootstrap struct {
	SchemaVersion string                           `json:"schema_version"`
	Manifest      piexecution.RuntimeManifest      `json:"runtime_manifest"`
	Revision      reviewconfig.Revision            `json:"config_revision"`
	Publications  []agentcomponentrepo.Publication `json:"-"`
	Components    []LocalPiBootstrapComponent      `json:"components"`
}

type LocalPiBootstrapComponent struct {
	Ref      reviewconfig.VersionedRef `json:"ref"`
	Contract string                    `json:"contract"`
	Content  []byte                    `json:"content"`
}

// LocalPiGovernedComponent carries the exact subject-scoped registry evidence
// used to construct a filesystem-free replay variant. Content is duplicated
// into the credential-free executor template so restart validation can rebuild
// the same bootstrap; the registry binding is still re-resolved before use.
type LocalPiGovernedComponent struct {
	Contract string                                       `json:"contract"`
	Binding  contractsv1alpha1.AgentStageComponentBinding `json:"binding"`
	Content  []byte                                       `json:"content"`
}

type LocalPiComponentVariant struct {
	SchemaVersion string                     `json:"schema_version"`
	Variable      runmodel.ReplayVariable    `json:"variable"`
	Components    []LocalPiGovernedComponent `json:"components"`
}

func (variant LocalPiComponentVariant) Validate() error {
	if variant.SchemaVersion != LocalPiComponentVariantSchemaVersion {
		return fmt.Errorf("unsupported local Pi component variant schema %q", variant.SchemaVersion)
	}
	expectedContract := ""
	maximum := 0
	switch variant.Variable {
	case runmodel.ReplayVariablePrompt:
		expectedContract, maximum = contractsv1alpha1.AgentStagePlanPromptContract, 1
	case runmodel.ReplayVariableSkillPack:
		expectedContract, maximum = contractsv1alpha1.AgentStagePlanSkillContract,
			contractsv1alpha1.AgentReviewWorkerMaxSkillCount
	case runmodel.ReplayVariableKnowledgePack:
		expectedContract, maximum = contractsv1alpha1.AgentStagePlanKnowledgeContract,
			contractsv1alpha1.AgentReviewWorkerMaxKnowledgeCount
	default:
		return fmt.Errorf("unsupported governed component replay variable %q", variant.Variable)
	}
	if len(variant.Components) == 0 || len(variant.Components) > maximum ||
		(variant.Variable == runmodel.ReplayVariablePrompt && len(variant.Components) != 1) {
		return fmt.Errorf("governed component replay has an invalid component count")
	}
	previous := ""
	for index, component := range variant.Components {
		if component.Contract != expectedContract || component.Binding.Artifact.Contract != expectedContract {
			return fmt.Errorf("components[%d] has an invalid contract", index)
		}
		ref := reviewconfig.VersionedRef{
			ID: component.Binding.Ref.ID, Revision: component.Binding.Ref.Revision,
			SHA256: component.Binding.Ref.SHA256,
		}
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("components[%d] ref: %w", index, err)
		}
		if index > 0 && ref.ID <= previous {
			return fmt.Errorf("governed components must be uniquely sorted by id")
		}
		previous = ref.ID
		switch variant.Variable {
		case runmodel.ReplayVariablePrompt:
			if _, err := contractsv1alpha1.NewAgentReviewWorkerPromptBundle(
				component.Binding.Ref, component.Binding.Artifact, component.Content,
			); err != nil {
				return fmt.Errorf("components[%d]: %w", index, err)
			}
		case runmodel.ReplayVariableSkillPack:
			if _, err := contractsv1alpha1.NewAgentReviewWorkerSkill(
				component.Binding.Ref, component.Binding.Artifact, component.Content,
			); err != nil {
				return fmt.Errorf("components[%d]: %w", index, err)
			}
		case runmodel.ReplayVariableKnowledgePack:
			if _, err := contractsv1alpha1.NewAgentReviewWorkerKnowledge(
				component.Binding.Ref, component.Binding.Artifact, component.Content,
			); err != nil {
				return fmt.Errorf("components[%d]: %w", index, err)
			}
		}
	}
	return nil
}

// BuildLocalPiBootstrap inspects exact local runtime bytes and returns the one
// formal config revision plus every governed component publication needed by
// AgentStagePreparer. Credentials are represented only by a fixed env SecretRef
// and are never read or copied here.
func BuildLocalPiBootstrap(options LocalPiBootstrapOptions) (LocalPiBootstrap, error) {
	if !cleanAbsolute(options.NodePath) || !cleanAbsolute(options.WorkerScript) {
		return LocalPiBootstrap{}, fmt.Errorf("Pi node and worker script must be clean absolute paths")
	}
	maxGroups := positiveOr(options.MaxGroups, 8)
	maxConcurrency := positiveOr(options.MaxConcurrency, 4)
	if maxConcurrency > maxGroups {
		return LocalPiBootstrap{}, fmt.Errorf(
			"Pi budget max_concurrency %d must not exceed max_groups %d",
			maxConcurrency,
			maxGroups,
		)
	}
	if len(options.KnowledgePaths) > contractsv1alpha1.AgentReviewWorkerMaxKnowledgeCount {
		return LocalPiBootstrap{}, fmt.Errorf(
			"Pi knowledge packs exceed maximum of %d",
			contractsv1alpha1.AgentReviewWorkerMaxKnowledgeCount,
		)
	}
	providerID, err := localPiProviderID(options.ProviderProfile)
	if err != nil {
		return LocalPiBootstrap{}, err
	}
	if options.Model == "" {
		return LocalPiBootstrap{}, fmt.Errorf("Pi model is required")
	}
	if options.NormalizationRevision == "" {
		options.NormalizationRevision = contractsv1alpha1.CandidateNormalizationCurrentRevision
	}
	if _, err := contractsv1alpha1.CandidateNormalizationWorkflowRevision(
		options.NormalizationRevision,
	); err != nil {
		return LocalPiBootstrap{}, err
	}
	_, runtimeContent, runtimeSHA, err := piexecution.BuildLocalRuntimeFileManifest(
		context.Background(), options.NodePath, options.WorkerScript,
	)
	if err != nil {
		return LocalPiBootstrap{}, fmt.Errorf("inspect local Pi runtime files: %w", err)
	}
	workerBytes, workerSHA, err := readAndDigestRegularFile(
		options.WorkerScript, maxWorkerScriptBytes,
	)
	if err != nil {
		return LocalPiBootstrap{}, fmt.Errorf("inspect Pi worker script: %w", err)
	}
	runtimeRoot := filepath.Dir(filepath.Dir(options.WorkerScript))
	promptBytes := localPiPromptBundleBytes()
	promptRevision := contractsv1alpha1.DefaultAgentReviewPromptBundle().Revision
	if options.PromptBundlePath != "" {
		if !cleanAbsolute(options.PromptBundlePath) {
			return LocalPiBootstrap{}, fmt.Errorf("Pi prompt bundle must be a clean absolute path")
		}
		promptBytes, _, err = readAndDigestRegularFile(
			options.PromptBundlePath,
			contractsv1alpha1.AgentReviewPromptBundleMaxBytes,
		)
		if err != nil {
			return LocalPiBootstrap{}, fmt.Errorf("inspect Pi prompt bundle: %w", err)
		}
		promptBundle, decodeErr := contractsv1alpha1.DecodeAgentReviewPromptBundle(promptBytes)
		if decodeErr != nil {
			return LocalPiBootstrap{}, fmt.Errorf("validate Pi prompt bundle: %w", decodeErr)
		}
		promptRevision = promptBundle.Revision
	}
	promptSHA := digestBytes(promptBytes)

	providerContent := mustJSON(struct {
		Profile  string `json:"profile"`
		Protocol string `json:"protocol"`
	}{Profile: options.ProviderProfile, Protocol: "anthropic-messages"})
	modelProfile, err := contractsv1alpha1.NewModelProfile(providerID, options.Model)
	if err != nil {
		return LocalPiBootstrap{}, fmt.Errorf("validate Pi model profile: %w", err)
	}
	modelContent, err := contractsv1alpha1.MarshalModelProfile(modelProfile)
	if err != nil {
		return LocalPiBootstrap{}, fmt.Errorf("encode Pi model profile: %w", err)
	}
	apiContent := []byte(`{"protocol":"anthropic-messages","revision":"2023-06-01"}`)

	runtimeRef := configRef(
		"node-runtime",
		contentRevision("binary", runtimeSHA),
		runtimeContent,
	)
	if runtimeRef.SHA256 != runtimeSHA {
		return LocalPiBootstrap{}, fmt.Errorf("local Pi runtime manifest digest changed")
	}
	agentRef := configRef(
		"pi-agent",
		contentRevision("worker-v0", workerSHA),
		workerBytes,
	)
	providerRef := configRef(providerID, "v1", providerContent)
	modelRef := configRef(
		providerID+"-model-"+digestBytes(modelContent)[:16],
		"provider",
		modelContent,
	)
	promptRef := reviewconfig.VersionedRef{
		ID: "pi-review-prompts", Revision: promptRevision, SHA256: promptSHA,
	}
	apiRef := configRef("anthropic-messages", "2023-06-01", apiContent)

	components := []LocalPiBootstrapComponent{
		{Ref: runtimeRef, Contract: contractsv1alpha1.AgentStagePlanRuntimeContract, Content: runtimeContent},
		{Ref: agentRef, Contract: contractsv1alpha1.AgentStagePlanAgentContract, Content: workerBytes},
		{Ref: providerRef, Contract: contractsv1alpha1.AgentStagePlanProviderContract, Content: providerContent},
		{Ref: modelRef, Contract: contractsv1alpha1.AgentStagePlanModelContract, Content: modelContent},
		{Ref: promptRef, Contract: contractsv1alpha1.AgentStagePlanPromptContract, Content: promptBytes},
		{Ref: apiRef, Contract: contractsv1alpha1.AgentStagePlanAPIProtocolContract, Content: apiContent},
	}
	workerOwned := []struct {
		id    string
		phase reviewconfig.AgentSkillPhase
	}{
		{"directory-language", reviewconfig.AgentSkillPhaseGrouping},
		{"code-context", reviewconfig.AgentSkillPhaseContext},
		{"independent-verifier", reviewconfig.AgentSkillPhaseVerification},
	}
	skillDefinitions := make([]reviewconfig.AgentSkillPackDefinition, 0, 6)
	for _, owned := range workerOwned {
		ref := reviewconfig.VersionedRef{
			ID:       owned.id,
			Revision: contentRevision("v0", workerSHA),
			SHA256:   workerSHA,
		}
		components = append(components, LocalPiBootstrapComponent{
			Ref: ref, Contract: contractsv1alpha1.AgentStagePlanSkillContract,
			Content: slices.Clone(workerBytes),
		})
		skillDefinitions = append(skillDefinitions, reviewconfig.AgentSkillPackDefinition{
			ID: owned.id, Phase: owned.phase, Ref: ref,
		})
	}
	reviewSkillPaths := make(map[string]string, len(builtinReviewSkillIDs)+len(options.ReviewSkillPaths))
	builtinReviewSkillPaths := make(map[string]string, len(builtinReviewSkillIDs))
	for _, id := range builtinReviewSkillIDs {
		path := filepath.Join(runtimeRoot, "skills", id+".md")
		reviewSkillPaths[id] = path
		builtinReviewSkillPaths[id] = path
	}
	customReviewSkillIDs := make(map[string]struct{}, len(options.ReviewSkillPaths))
	for _, path := range options.ReviewSkillPaths {
		if !cleanAbsolute(path) {
			return LocalPiBootstrap{}, fmt.Errorf("Pi review skill must be a clean absolute path")
		}
		id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if _, duplicate := customReviewSkillIDs[id]; duplicate {
			return LocalPiBootstrap{}, fmt.Errorf("duplicate Pi review skill id %q", id)
		}
		customReviewSkillIDs[id] = struct{}{}
		reviewSkillPaths[id] = path
	}
	if len(reviewSkillPaths) > contractsv1alpha1.AgentReviewWorkerMaxSkillCount {
		return LocalPiBootstrap{}, fmt.Errorf(
			"Pi review skills exceed maximum of %d",
			contractsv1alpha1.AgentReviewWorkerMaxSkillCount,
		)
	}
	reviewSkillIDs := make([]string, 0, len(reviewSkillPaths))
	for id := range reviewSkillPaths {
		reviewSkillIDs = append(reviewSkillIDs, id)
	}
	sort.Strings(reviewSkillIDs)
	reviewRefs := make([]contractsv1alpha1.VersionedRef, 0, len(reviewSkillPaths))
	for _, id := range reviewSkillIDs {
		path := reviewSkillPaths[id]
		content, digest, readErr := readAndDigestRegularFile(path, maxBuiltinSkillBytes)
		if readErr != nil {
			return LocalPiBootstrap{}, fmt.Errorf("inspect Pi review skill %q: %w", path, readErr)
		}
		revision := "sha256-" + digest[:16]
		if builtinReviewSkillPaths[id] == path {
			revision, err = builtinSkillRevision(content)
			if err != nil {
				return LocalPiBootstrap{}, fmt.Errorf(
					"inspect Pi review skill %q revision: %w", path, err,
				)
			}
		}
		ref := configRef(id, revision, content)
		components = append(components, LocalPiBootstrapComponent{
			Ref: ref, Contract: contractsv1alpha1.AgentStagePlanSkillContract,
			Content: content,
		})
		skillDefinitions = append(skillDefinitions, reviewconfig.AgentSkillPackDefinition{
			ID: id, Phase: reviewconfig.AgentSkillPhaseReview, Ref: ref,
		})
		reviewRefs = append(reviewRefs, contractRef(ref))
	}
	sort.Slice(skillDefinitions, func(left, right int) bool {
		phaseOrder := map[reviewconfig.AgentSkillPhase]int{
			reviewconfig.AgentSkillPhaseGrouping:     0,
			reviewconfig.AgentSkillPhaseContext:      1,
			reviewconfig.AgentSkillPhaseReview:       2,
			reviewconfig.AgentSkillPhaseVerification: 3,
		}
		if phaseOrder[skillDefinitions[left].Phase] != phaseOrder[skillDefinitions[right].Phase] {
			return phaseOrder[skillDefinitions[left].Phase] < phaseOrder[skillDefinitions[right].Phase]
		}
		return skillDefinitions[left].ID < skillDefinitions[right].ID
	})
	sort.Slice(reviewRefs, func(left, right int) bool { return reviewRefs[left].ID < reviewRefs[right].ID })
	knowledgeDefinitions := make([]reviewconfig.AgentKnowledgePackDefinition, 0, len(options.KnowledgePaths))
	knowledgeRefs := make([]contractsv1alpha1.VersionedRef, 0, len(options.KnowledgePaths))
	seenKnowledge := make(map[string]struct{}, len(options.KnowledgePaths))
	for _, path := range options.KnowledgePaths {
		if !cleanAbsolute(path) {
			return LocalPiBootstrap{}, fmt.Errorf("Pi knowledge pack must be a clean absolute path")
		}
		content, digest, readErr := readAndDigestRegularFile(
			path, contractsv1alpha1.AgentReviewKnowledgeMaxBytes,
		)
		if readErr != nil {
			return LocalPiBootstrap{}, fmt.Errorf("inspect Pi knowledge pack %q: %w", path, readErr)
		}
		id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if _, duplicate := seenKnowledge[id]; duplicate {
			return LocalPiBootstrap{}, fmt.Errorf("duplicate Pi knowledge pack id %q", id)
		}
		seenKnowledge[id] = struct{}{}
		ref := configRef(id, "sha256-"+digest[:16], content)
		components = append(components, LocalPiBootstrapComponent{
			Ref: ref, Contract: contractsv1alpha1.AgentStagePlanKnowledgeContract,
			Content: content,
		})
		knowledgeDefinitions = append(knowledgeDefinitions, reviewconfig.AgentKnowledgePackDefinition{
			ID: id, Ref: ref,
		})
		knowledgeRefs = append(knowledgeRefs, contractRef(ref))
	}
	sort.Slice(knowledgeDefinitions, func(left, right int) bool {
		return knowledgeDefinitions[left].ID < knowledgeDefinitions[right].ID
	})
	sort.Slice(knowledgeRefs, func(left, right int) bool {
		return knowledgeRefs[left].ID < knowledgeRefs[right].ID
	})

	manifest := buildLocalPiRuntimeManifest(
		contractRef(runtimeRef), contractRef(agentRef), contractRef(providerRef),
		contractRef(modelRef), contractRef(promptRef), reviewRefs, knowledgeRefs,
	)

	definition := workflow.FormalAgentReviewDefinition()
	revision, err := formalPiConfigRevision(options, definition, runtimeRef, modelRef,
		agentRef, providerRef, promptRef, apiRef, skillDefinitions, knowledgeDefinitions)
	if err != nil {
		return LocalPiBootstrap{}, err
	}
	revision, err = versionFormalPiConfigRevision(revision)
	if err != nil {
		return LocalPiBootstrap{}, err
	}
	publications := make([]agentcomponentrepo.Publication, len(components))
	for index, component := range components {
		publications[index] = agentcomponentrepo.Publication{
			Ref: component.Ref, Contract: component.Contract,
			Content: slices.Clone(component.Content),
		}
	}
	return LocalPiBootstrap{
		SchemaVersion: LocalPiBootstrapSchemaVersion, Manifest: manifest,
		Revision: revision, Publications: publications, Components: components,
	}, nil
}

// BuildLocalPiBootstrapVariant applies already-published prompt, review-skill,
// or knowledge bytes to the trusted server profile. It never accepts or opens
// a component path. IDs/order/phases stay fixed so the later replay config
// builder can prove that exactly one behavior dimension changed.
func BuildLocalPiBootstrapVariant(
	options LocalPiBootstrapOptions,
	variant LocalPiComponentVariant,
) (LocalPiBootstrap, error) {
	if err := variant.Validate(); err != nil {
		return LocalPiBootstrap{}, err
	}
	bootstrap, err := BuildLocalPiBootstrap(options)
	if err != nil {
		return LocalPiBootstrap{}, err
	}
	if bootstrap.Revision.Patch.AgentReview == nil {
		return LocalPiBootstrap{}, fmt.Errorf("local Pi bootstrap has no agent review patch")
	}
	patch := bootstrap.Revision.Patch.AgentReview
	changed := false
	switch variant.Variable {
	case runmodel.ReplayVariablePrompt:
		component := variant.Components[0]
		ref := componentReviewConfigRef(component)
		if ref.ID != bootstrap.Manifest.Prompt.ID {
			return LocalPiBootstrap{}, fmt.Errorf("governed prompt must preserve prompt id %q", bootstrap.Manifest.Prompt.ID)
		}
		if patch.Prompt == nil || *patch.Prompt == ref {
			return LocalPiBootstrap{}, fmt.Errorf("governed prompt replay requires one changed prompt")
		}
		if !replaceLocalPiBootstrapComponent(
			&bootstrap, contractsv1alpha1.AgentStagePlanPromptContract, *patch.Prompt, component,
		) {
			return LocalPiBootstrap{}, fmt.Errorf("local Pi bootstrap omitted baseline prompt component")
		}
		patch.Prompt = clonePointer(ref)
		bootstrap.Manifest.Prompt = contractRef(ref)
		changed = true
	case runmodel.ReplayVariableSkillPack:
		if patch.SkillPacks == nil {
			return LocalPiBootstrap{}, fmt.Errorf("local Pi bootstrap omitted governed skill definitions")
		}
		for _, component := range variant.Components {
			ref := componentReviewConfigRef(component)
			found := false
			for index := range patch.SkillPacks.Upsert {
				definition := &patch.SkillPacks.Upsert[index]
				if definition.ID != ref.ID {
					continue
				}
				found = true
				if definition.Phase != reviewconfig.AgentSkillPhaseReview {
					return LocalPiBootstrap{}, fmt.Errorf("governed skill %q is not a review-phase dimension", ref.ID)
				}
				if definition.Ref == ref {
					return LocalPiBootstrap{}, fmt.Errorf("governed skill %q does not change its baseline ref", ref.ID)
				}
				if !replaceLocalPiBootstrapComponent(
					&bootstrap, contractsv1alpha1.AgentStagePlanSkillContract, definition.Ref, component,
				) {
					return LocalPiBootstrap{}, fmt.Errorf("local Pi bootstrap omitted baseline skill %q", ref.ID)
				}
				definition.Ref = ref
				changed = true
				break
			}
			if !found {
				return LocalPiBootstrap{}, fmt.Errorf("governed skill %q is not present in the server profile", ref.ID)
			}
		}
		bootstrap.Manifest.ReviewSkills = reviewRefsFromDefinitions(patch.SkillPacks.Upsert)
	case runmodel.ReplayVariableKnowledgePack:
		if patch.KnowledgePacks == nil {
			return LocalPiBootstrap{}, fmt.Errorf("local Pi bootstrap omitted governed knowledge definitions")
		}
		for _, component := range variant.Components {
			ref := componentReviewConfigRef(component)
			found := false
			for index := range patch.KnowledgePacks.Upsert {
				definition := &patch.KnowledgePacks.Upsert[index]
				if definition.ID != ref.ID {
					continue
				}
				found = true
				if definition.Ref == ref {
					return LocalPiBootstrap{}, fmt.Errorf("governed knowledge %q does not change its baseline ref", ref.ID)
				}
				if !replaceLocalPiBootstrapComponent(
					&bootstrap, contractsv1alpha1.AgentStagePlanKnowledgeContract, definition.Ref, component,
				) {
					return LocalPiBootstrap{}, fmt.Errorf("local Pi bootstrap omitted baseline knowledge %q", ref.ID)
				}
				definition.Ref = ref
				changed = true
				break
			}
			if !found {
				return LocalPiBootstrap{}, fmt.Errorf("governed knowledge %q is not present in the server profile", ref.ID)
			}
		}
		bootstrap.Manifest.Knowledge = knowledgeRefsFromDefinitions(patch.KnowledgePacks.Upsert)
	}
	if !changed {
		return LocalPiBootstrap{}, fmt.Errorf("governed component replay requires at least one changed ref")
	}
	bootstrap.Manifest = buildLocalPiRuntimeManifest(
		bootstrap.Manifest.Runtime, bootstrap.Manifest.Agent, bootstrap.Manifest.Provider,
		bootstrap.Manifest.Model, bootstrap.Manifest.Prompt,
		bootstrap.Manifest.ReviewSkills, bootstrap.Manifest.Knowledge,
	)
	bootstrap.Revision, err = versionFormalPiConfigRevision(bootstrap.Revision)
	if err != nil {
		return LocalPiBootstrap{}, err
	}
	bootstrap.Publications = make([]agentcomponentrepo.Publication, len(bootstrap.Components))
	for index, component := range bootstrap.Components {
		bootstrap.Publications[index] = agentcomponentrepo.Publication{
			Ref: component.Ref, Contract: component.Contract,
			Content: slices.Clone(component.Content),
		}
	}
	return bootstrap, nil
}

func buildLocalPiRuntimeManifest(
	runtime contractsv1alpha1.VersionedRef,
	agent contractsv1alpha1.VersionedRef,
	provider contractsv1alpha1.VersionedRef,
	model contractsv1alpha1.VersionedRef,
	prompt contractsv1alpha1.VersionedRef,
	reviewSkills []contractsv1alpha1.VersionedRef,
	knowledge []contractsv1alpha1.VersionedRef,
) piexecution.RuntimeManifest {
	descriptor := mustJSON(struct {
		RuntimeFilesSHA256 string                           `json:"runtime_files_sha256"`
		WorkerSHA256       string                           `json:"worker_sha256"`
		Runtime            contractsv1alpha1.VersionedRef   `json:"runtime"`
		Agent              contractsv1alpha1.VersionedRef   `json:"agent"`
		Provider           contractsv1alpha1.VersionedRef   `json:"provider"`
		Model              contractsv1alpha1.VersionedRef   `json:"model"`
		Prompt             contractsv1alpha1.VersionedRef   `json:"prompt"`
		ReviewSkills       []contractsv1alpha1.VersionedRef `json:"review_skills"`
		Knowledge          []contractsv1alpha1.VersionedRef `json:"knowledge"`
	}{
		RuntimeFilesSHA256: runtime.SHA256, WorkerSHA256: agent.SHA256,
		Runtime: runtime, Agent: agent, Provider: provider, Model: model, Prompt: prompt,
		ReviewSkills: slices.Clone(reviewSkills), Knowledge: slices.Clone(knowledge),
	})
	digest := digestBytes(descriptor)
	return piexecution.RuntimeManifest{
		Runtime: runtime, Agent: agent, Provider: provider, Model: model, Prompt: prompt,
		ReviewSkills: slices.Clone(reviewSkills), Knowledge: slices.Clone(knowledge),
		BuildIdentity: "pi-local-" + digest[:24], SHA256: digest,
	}
}

func componentReviewConfigRef(component LocalPiGovernedComponent) reviewconfig.VersionedRef {
	return reviewconfig.VersionedRef{
		ID: component.Binding.Ref.ID, Revision: component.Binding.Ref.Revision,
		SHA256: component.Binding.Ref.SHA256,
	}
}

func replaceLocalPiBootstrapComponent(
	bootstrap *LocalPiBootstrap,
	contract string,
	baseline reviewconfig.VersionedRef,
	replacement LocalPiGovernedComponent,
) bool {
	for index, component := range bootstrap.Components {
		if component.Contract != contract || component.Ref != baseline {
			continue
		}
		bootstrap.Components[index] = LocalPiBootstrapComponent{
			Ref: componentReviewConfigRef(replacement), Contract: contract,
			Content: slices.Clone(replacement.Content),
		}
		return true
	}
	return false
}

func reviewRefsFromDefinitions(
	definitions []reviewconfig.AgentSkillPackDefinition,
) []contractsv1alpha1.VersionedRef {
	refs := make([]contractsv1alpha1.VersionedRef, 0, len(definitions))
	for _, definition := range definitions {
		if definition.Phase == reviewconfig.AgentSkillPhaseReview {
			refs = append(refs, contractRef(definition.Ref))
		}
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].ID < refs[right].ID })
	return refs
}

func knowledgeRefsFromDefinitions(
	definitions []reviewconfig.AgentKnowledgePackDefinition,
) []contractsv1alpha1.VersionedRef {
	refs := make([]contractsv1alpha1.VersionedRef, len(definitions))
	for index, definition := range definitions {
		refs[index] = contractRef(definition.Ref)
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].ID < refs[right].ID })
	return refs
}

func clonePointer[T any](value T) *T {
	return &value
}

func localPiProviderID(profile string) (string, error) {
	switch profile {
	case "deepseek-anthropic-env":
		return "deepseek-anthropic", nil
	case "anthropic-official":
		return "anthropic", nil
	default:
		return "", fmt.Errorf("unsupported Pi provider profile %q", profile)
	}
}

func formalPiConfigRevision(
	options LocalPiBootstrapOptions,
	definition workflow.Definition,
	runtimeRef reviewconfig.VersionedRef,
	modelRef reviewconfig.VersionedRef,
	agentRef reviewconfig.VersionedRef,
	providerRef reviewconfig.VersionedRef,
	promptRef reviewconfig.VersionedRef,
	apiRef reviewconfig.VersionedRef,
	skills []reviewconfig.AgentSkillPackDefinition,
	knowledge []reviewconfig.AgentKnowledgePackDefinition,
) (reviewconfig.Revision, error) {
	maxFiles := positiveOr(options.MaxFiles, 32)
	maxGroups := positiveOr(options.MaxGroups, 8)
	maxHypotheses := positiveOr(options.MaxHypotheses, 32)
	maxModelCalls := positiveOr(options.MaxModelCalls, 96)
	maxToolCalls := positiveOr(options.MaxToolCalls, 24)
	maxTargetBytes := positiveInt64Or(options.MaxTargetBytes, 4<<20)
	maxGroupBytes := positiveInt64Or(options.MaxGroupBytes, 64<<10)
	maxOutputBytes := positiveInt64Or(options.MaxOutputBytes, 1<<20)
	maxOutputTokens := positiveInt64Or(options.MaxOutputTokens, 8192)
	maxCostMicros := positiveInt64Or(options.MaxCostMicros, 1_000_000)
	timeoutMS := positiveInt64Or(options.TimeoutMS, 180_000)
	maxConcurrency := positiveOr(options.MaxConcurrency, 4)
	maxAttempts := positiveOr(options.MaxAttempts, 1)
	revision, err := configdefaults.Revision(configdefaults.Options{
		ID: "formal-local-pi", Revision: "1", MaxFiles: maxFiles,
		MaxPatchBytes: maxTargetBytes, MaxInputBytes: maxTargetBytes,
		MaxOutputBytes: maxOutputBytes, MaxAttempts: maxAttempts,
	}, definition)
	if err != nil {
		return reviewconfig.Revision{}, err
	}
	modelEgress := reviewconfig.AgentModelEgressProviderBrokerOnly
	workspaceReads := reviewconfig.AgentWorkspaceReadsFrozenInputOnly
	deny := reviewconfig.PermissionDeny
	delegation := 0
	revision.Patch.Execution.AgentProfile = &runtimeRef
	revision.Patch.Execution.ModelProfile = &modelRef
	revision.Patch.Execution.AllowedTools = &reviewconfig.SetPatch{
		Add: []string{"list_files", "read_file", "search_code"}, Remove: []string{},
	}
	maxTokens := maxOutputTokens
	if int64(maxModelCalls) <= (1<<63-1)/maxOutputTokens {
		maxTokens = int64(maxModelCalls) * maxOutputTokens
	}
	revision.Patch.Budget.StageTimeoutMS = &timeoutMS
	revision.Patch.Budget.MaxConcurrency = &maxConcurrency
	revision.Patch.Budget.MaxCostMicros = &maxCostMicros
	revision.Patch.Budget.MaxTokens = &maxTokens
	revision.Patch.AgentReview = &reviewconfig.AgentReviewPatch{
		Agent: &agentRef,
		Normalization: &reviewconfig.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: options.NormalizationRevision,
			SHA256:   agentRef.SHA256,
		},
		Provider: &providerRef, Model: &modelRef, Prompt: &promptRef,
		ModelCredential: &reviewconfig.SecretRef{URI: "secret://env/anthropic-api-key"},
		SkillPacks: &reviewconfig.OrderedAgentSkillPackPatch{
			Upsert: slices.Clone(skills), Remove: []string{},
		},
		KnowledgePacks: &reviewconfig.OrderedAgentKnowledgePackPatch{
			Upsert: slices.Clone(knowledge), Remove: []string{},
		},
		APIProtocol: &apiRef,
		Authority: &reviewconfig.AgentAuthorityPatch{
			ModelEgress: &modelEgress, ToolNetwork: &deny, WorkspaceReads: &workspaceReads,
			WorkspaceWrites: &deny, RemoteWrites: &deny,
			MaxDelegationDepth: &delegation,
			Tools: &reviewconfig.SetPatch{
				Add: []string{"list_files", "read_file", "search_code"}, Remove: []string{},
			},
		},
		Budget: &reviewconfig.AgentBudgetPatch{
			MaxFiles: &maxFiles, MaxGroups: &maxGroups, MaxHypotheses: &maxHypotheses,
			MaxModelCalls: &maxModelCalls, MaxToolCalls: &maxToolCalls,
			MaxTargetBytes: &maxTargetBytes, MaxGroupBytes: &maxGroupBytes,
			MaxOutputBytes: &maxOutputBytes, MaxOutputTokens: &maxOutputTokens,
			MaxCostMicros: &maxCostMicros, TimeoutMS: &timeoutMS,
			MaxConcurrency: &maxConcurrency,
		},
	}
	if err := revision.Validate(); err != nil {
		return reviewconfig.Revision{}, fmt.Errorf("validate formal Pi config revision: %w", err)
	}
	return revision, nil
}

func localPiPromptBundleBytes() []byte {
	data, err := contractsv1alpha1.MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		panic(fmt.Sprintf("marshal built-in Pi prompt bundle: %v", err))
	}
	return data
}

func builtinSkillRevision(content []byte) (string, error) {
	const prefix = "Revision:"
	var revision string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if revision != "" {
			return "", fmt.Errorf("must declare exactly one Revision line")
		}
		revision = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	}
	if !validBuiltinSkillRevision(revision) {
		return "", fmt.Errorf("invalid revision metadata")
	}
	return revision, nil
}

func validBuiltinSkillRevision(revision string) bool {
	const prefix = "builtin-v"
	if !strings.HasPrefix(revision, prefix) || len(revision) == len(prefix) ||
		revision[len(prefix)] == '0' {
		return false
	}
	for _, value := range revision[len(prefix):] {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func digestRegularFile(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("path is not a regular file")
	}
	if limit > 0 && info.Size() > limit {
		return "", fmt.Errorf("file exceeds %d bytes", limit)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readAndDigestRegularFile(path string, limit int64) ([]byte, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > limit {
		return nil, "", fmt.Errorf("path must be a bounded regular non-symlink file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return data, digestBytes(data), nil
}

func configRef(id string, revision string, content []byte) reviewconfig.VersionedRef {
	return reviewconfig.VersionedRef{ID: id, Revision: revision, SHA256: digestBytes(content)}
}

func versionFormalPiConfigRevision(
	revision reviewconfig.Revision,
) (reviewconfig.Revision, error) {
	// The revision identity belongs to the complete config semantics, not to the
	// runtime build manifest. Use one stable placeholder while hashing to avoid
	// a self-referential digest, then install its content-derived identifier.
	revision.Revision = "content-addressed"
	data, err := json.Marshal(revision)
	if err != nil {
		return reviewconfig.Revision{}, fmt.Errorf("encode formal Pi config revision identity: %w", err)
	}
	revision.Revision = digestBytes(data)[:16]
	if err := revision.Validate(); err != nil {
		return reviewconfig.Revision{}, fmt.Errorf("validate versioned formal Pi config revision: %w", err)
	}
	return revision, nil
}

func contentRevision(base, digest string) string {
	return base + "-" + digest[:16]
}

func contractRef(ref reviewconfig.VersionedRef) contractsv1alpha1.VersionedRef {
	return contractsv1alpha1.VersionedRef{
		ID: ref.ID, Revision: ref.Revision, SHA256: ref.SHA256,
	}
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func positiveOr(value int, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func positiveInt64Or(value int64, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}
