// Package agentplan compiles a lifecycle-governed, frozen review closure into
// the canonical formal-agent stage input. Compilation is deliberately pure:
// callers must supply exact artifact bindings and already-loaded immutable
// values; this package performs no filesystem, network, clock, random, secret,
// or provider access.
package agentplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/targetmodel"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const AgentHypothesizeStageKind = "agent_hypothesize"

// ResolvedComponents contains only immutable, content-addressed bindings.
// Credentials and provider endpoints intentionally have no representation in
// this type and therefore cannot enter AgentStagePlan.
type ResolvedComponents struct {
	Agent            contractsv1alpha1.AgentStageComponentBinding
	Provider         contractsv1alpha1.AgentStageComponentBinding
	Model            contractsv1alpha1.AgentStageComponentBinding
	Runtime          contractsv1alpha1.AgentStageComponentBinding
	Prompt           contractsv1alpha1.AgentStageComponentBinding
	APIProtocol      contractsv1alpha1.AgentStageComponentBinding
	Skills           []contractsv1alpha1.AgentStageSkillBinding
	Knowledge        []contractsv1alpha1.AgentStageKnowledgeBinding
	ContextProviders []contractsv1alpha1.AgentStageContextProviderBinding
}

// CompileInput is the complete pure record-integrity boundary. A
// ConfigResolutionReceipt and governed projections are mandatory, but neither
// is a signature or proof of current authorization. The application layer must
// obtain the bundle and receipt from one trusted GovernedConfigProvider call,
// publish the exact frozen bytes, and authorize every governed binding before
// using this compiler.
type CompileInput struct {
	ReviewRunID string
	StageID     string

	ExecutionSnapshot           runmodel.ExecutionSnapshot
	ExecutionSnapshotRef        runmodel.ArtifactRef
	ExecutionSnapshotProjection GovernedArtifactProjection

	ConfigBundle                      reviewconfig.ConfigBundle
	ConfigBundleProjection            GovernedArtifactProjection
	ConfigResolutionReceipt           reviewconfig.ConfigResolutionReceipt
	ConfigResolutionReceiptRef        runmodel.ArtifactRef
	ConfigResolutionReceiptProjection GovernedArtifactProjection

	Workflow              workflow.Definition
	WorkflowProjection    GovernedArtifactProjection
	ReviewSpec            contractsv1alpha1.ReviewSpec
	ReviewSpecProjection  GovernedArtifactProjection
	Target                targetmodel.MaterializedTarget
	ReviewInput           reviewcore.ReviewInput
	ReviewInputProjection GovernedArtifactProjection
	Components            ResolvedComponents
	FormalReplay          *FormalReplayClosure
}

// FormalReplayClosure is the pure, pre-dispatch proof that a formal replay
// reuses one authoritative committed source and declares its exact config
// change. The repository remains responsible for the terminal source refs.
type FormalReplayClosure struct {
	SourceRun           runmodel.ReviewRun
	SourceRunRef        runmodel.ArtifactRef
	SourceSnapshot      runmodel.ExecutionSnapshot
	SourceReviewSpec    contractsv1alpha1.ReviewSpec
	SourceWorkflow      workflow.Definition
	SourceTarget        targetmodel.MaterializedTarget
	SourceReviewInput   reviewcore.ReviewInput
	SourceConfigBundle  reviewconfig.ConfigBundle
	SourceConfigReceipt reviewconfig.ConfigResolutionReceipt
	ChangeSet           runmodel.ReplayChangeSet
	ChangeSetRef        runmodel.ArtifactRef
}

// Compile validates every cross-artifact record closure and returns the
// canonical, sealed AgentStagePlan. It does not establish publication trust;
// the application entry owns that boundary. It never silently clamps
// authority or budget: any request outside the frozen workflow/snapshot
// ceiling is rejected.
func Compile(input CompileInput) (contractsv1alpha1.AgentStagePlan, error) {
	if input.ReviewRunID == "" || input.StageID == "" {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf(
			"review run id and stage id are required",
		)
	}
	if err := input.ExecutionSnapshot.Validate(); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf(
			"validate execution snapshot: %w",
			err,
		)
	}
	if err := input.ConfigBundle.Validate(); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("validate config bundle: %w", err)
	}
	if input.ConfigBundle.AgentReview == nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf(
			"config bundle has no agent_review policy",
		)
	}
	if err := input.ConfigResolutionReceipt.ValidateAgainst(input.ConfigBundle); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf(
			"validate published config resolution receipt: %w",
			err,
		)
	}
	if err := input.Workflow.Validate(); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("validate workflow: %w", err)
	}
	if err := input.ReviewSpec.Validate(); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("validate review spec: %w", err)
	}
	if err := input.Target.Validate(); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("validate materialized target: %w", err)
	}
	if err := input.ReviewInput.Validate(); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("validate review input: %w", err)
	}

	if err := validateFrozenArtifacts(input); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	if err := validateGovernedArtifactProjections(input); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	stage, err := findFormalAgentStage(input.Workflow, input.StageID)
	if err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	policy := *input.ConfigBundle.AgentReview
	if err := validateSnapshotProjection(input, policy); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	if err := validateComponentClosure(policy, input.ConfigBundle, input.Components); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	if err := validateAuthorityAndBudget(
		policy,
		input.ConfigBundle,
		stage,
		input.ExecutionSnapshot,
		input.Target,
		input.ReviewInput,
		input.Components,
	); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	effectiveBudget := effectiveAgentStageBudget(policy.Budget, stage.Budget)

	targetDigest, err := reviewcore.DigestReviewInput(input.ReviewInput)
	if err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("digest review input: %w", err)
	}
	stageDigest, err := digestStage(stage)
	if err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	rulePackBytes, err := json.Marshal(input.ConfigBundle.RulePack)
	if err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("marshal exact rule pack: %w", err)
	}
	plan := contractsv1alpha1.AgentStagePlan{
		SchemaVersion: contractsv1alpha1.AgentStagePlanSchemaVersion,
		ReviewRunID:   input.ReviewRunID,
		Stage: contractsv1alpha1.VersionedRef{
			ID:       stage.ID,
			Revision: stage.ImplementationRevision,
			SHA256:   stageDigest,
		},
		TargetDigest:  targetDigest,
		BuildIdentity: input.ExecutionSnapshot.BuildIdentity,

		ExecutionSnapshot:          input.ExecutionSnapshotProjection.Governed,
		ConfigBundle:               input.ConfigBundleProjection.Governed,
		ConfigResolutionReceiptRef: input.ConfigResolutionReceiptProjection.Governed,
		Workflow:                   input.WorkflowProjection.Governed,
		ReviewSpec:                 input.ReviewSpecProjection.Governed,
		ReviewInput:                input.ReviewInputProjection.Governed,

		AgentReviewPolicy: versionedRef(policy.ID, policy.Revision, policy.SHA256),
		RulePack: versionedRef(
			input.ConfigBundle.RulePack.ID,
			input.ConfigBundle.RulePack.Revision,
			input.ConfigBundle.RulePack.SHA256,
		),
		Normalization: versionedRef(
			policy.Normalization.ID,
			policy.Normalization.Revision,
			policy.Normalization.SHA256,
		),
		RulePackBase64:   base64.StdEncoding.EncodeToString(rulePackBytes),
		Agent:            input.Components.Agent,
		Provider:         input.Components.Provider,
		Model:            input.Components.Model,
		Runtime:          input.Components.Runtime,
		Prompt:           input.Components.Prompt,
		Skills:           slices.Clone(input.Components.Skills),
		Knowledge:        slices.Clone(input.Components.Knowledge),
		ContextProviders: slices.Clone(input.Components.ContextProviders),

		ModelAuthority: contractsv1alpha1.AgentStageModelAuthority{
			ModelEgress: string(policy.Authority.ModelEgress),
			APIProtocol: input.Components.APIProtocol,
		},
		ToolAuthority: contractsv1alpha1.AgentStageToolAuthority{
			Tools:              slices.Clone(policy.Authority.Tools),
			ToolNetwork:        string(policy.Authority.ToolNetwork),
			WorkspaceReads:     string(policy.Authority.WorkspaceReads),
			WorkspaceWrites:    string(policy.Authority.WorkspaceWrites),
			RemoteWrites:       string(policy.Authority.RemoteWrites),
			MaxDelegationDepth: uint32(policy.Authority.MaxDelegationDepth),
		},
		Budget: effectiveBudget,
		Retry: contractsv1alpha1.AgentStageRetryPolicy{
			MaxAttempts:    min(input.ConfigBundle.Budget.MaxAttempts, stage.Retry.MaxAttempts),
			BackoffMS:      stage.Retry.BackoffMS,
			RetryableCodes: slices.Clone(stage.Retry.RetryableCodes),
			UnknownOutcome: string(stage.Retry.UnknownOutcome),
		},
		OutputContract: contractsv1alpha1.AgentStagePlanOutputContract,
		Disposition:    contractsv1alpha1.AgentStageDispositionHypothesisOnly,
		SideEffects:    contractsv1alpha1.AgentStageSideEffectsDeny,
	}
	sealed, err := contractsv1alpha1.SealAgentStagePlan(plan)
	if err != nil {
		return contractsv1alpha1.AgentStagePlan{}, fmt.Errorf("seal AgentStagePlan: %w", err)
	}
	return sealed, nil
}

func validateFrozenArtifacts(input CompileInput) error {
	checks := []struct {
		name     string
		ref      runmodel.ArtifactRef
		contract string
		value    any
	}{
		{
			"execution snapshot",
			input.ExecutionSnapshotRef,
			runmodel.ContractExecutionSnapshot,
			input.ExecutionSnapshot,
		},
		{
			"materialized target",
			input.ExecutionSnapshot.TargetSnapshotRef,
			runmodel.ContractMaterializedTarget,
			input.Target,
		},
		{
			"config bundle",
			input.ExecutionSnapshot.ConfigBundleRef,
			runmodel.ContractConfigBundle,
			input.ConfigBundle,
		},
		{
			"config resolution receipt",
			input.ConfigResolutionReceiptRef,
			runmodel.ContractConfigResolutionReceipt,
			input.ConfigResolutionReceipt,
		},
		{
			"workflow",
			input.ExecutionSnapshot.WorkflowDefinitionRef,
			runmodel.ContractWorkflowDefinition,
			input.Workflow,
		},
		{
			"review spec",
			input.ExecutionSnapshot.ReviewSpecRef,
			runmodel.ContractReviewSpec,
			input.ReviewSpec,
		},
		{
			"review input",
			input.ExecutionSnapshot.ReviewInputRef,
			runmodel.ContractReviewInput,
			input.ReviewInput,
		},
	}
	for _, check := range checks {
		if err := validateJSONArtifact(check.name, check.ref, check.contract, check.value); err != nil {
			return err
		}
	}
	return nil
}

func validateGovernedArtifactProjections(input CompileInput) error {
	checks := []struct {
		name       string
		projection GovernedArtifactProjection
		local      runmodel.ArtifactRef
		contract   string
	}{
		{
			"execution snapshot",
			input.ExecutionSnapshotProjection,
			input.ExecutionSnapshotRef,
			runmodel.ContractExecutionSnapshot,
		},
		{
			"config bundle",
			input.ConfigBundleProjection,
			input.ExecutionSnapshot.ConfigBundleRef,
			runmodel.ContractConfigBundle,
		},
		{
			"config resolution receipt",
			input.ConfigResolutionReceiptProjection,
			input.ConfigResolutionReceiptRef,
			runmodel.ContractConfigResolutionReceipt,
		},
		{
			"workflow",
			input.WorkflowProjection,
			input.ExecutionSnapshot.WorkflowDefinitionRef,
			runmodel.ContractWorkflowDefinition,
		},
		{
			"review spec",
			input.ReviewSpecProjection,
			input.ExecutionSnapshot.ReviewSpecRef,
			runmodel.ContractReviewSpec,
		},
		{
			"review input",
			input.ReviewInputProjection,
			input.ExecutionSnapshot.ReviewInputRef,
			runmodel.ContractReviewInput,
		},
	}
	var commonNamespace *governedArtifactNamespace
	for _, check := range checks {
		namespace, err := validateGovernedArtifactProjection(
			check.name+" governed projection",
			check.projection,
		)
		if err != nil {
			return err
		}
		if check.projection.Local != check.local {
			return fmt.Errorf(
				"%s governed projection does not bind its exact frozen local artifact",
				check.name,
			)
		}
		if check.projection.Local.Contract != check.contract {
			return fmt.Errorf(
				"%s governed projection contract is %q, want %q",
				check.name,
				check.projection.Local.Contract,
				check.contract,
			)
		}
		if commonNamespace == nil {
			copy := namespace
			commonNamespace = &copy
			continue
		}
		if namespace != *commonNamespace {
			return fmt.Errorf(
				"governed artifact projections cross authority, tenant, or workspace namespaces",
			)
		}
	}
	return nil
}

func validateJSONArtifact(name string, ref runmodel.ArtifactRef, contract string, value any) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("%s artifact ref: %w", name, err)
	}
	if ref.Contract != contract {
		return fmt.Errorf("%s artifact contract is %q, want %q", name, ref.Contract, contract)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s artifact: %w", name, err)
	}
	digest := sha256.Sum256(data)
	wantDigest := hex.EncodeToString(digest[:])
	if ref.SHA256 != wantDigest || ref.SizeBytes != int64(len(data)) {
		return fmt.Errorf("%s artifact ref does not bind the exact supplied content", name)
	}
	return nil
}

func findFormalAgentStage(definition workflow.Definition, stageID string) (workflow.Stage, error) {
	for _, stage := range definition.Stages {
		if stage.ID != stageID {
			continue
		}
		if stage.Kind != AgentHypothesizeStageKind {
			return workflow.Stage{}, fmt.Errorf(
				"stage %q kind is %q, want %q",
				stageID,
				stage.Kind,
				AgentHypothesizeStageKind,
			)
		}
		if stage.InputContract != contractsv1alpha1.AgentStagePlanReviewInputContract ||
			stage.OutputContract != contractsv1alpha1.AgentStagePlanOutputContract {
			return workflow.Stage{}, fmt.Errorf(
				"stage %q must consume ReviewInput and produce ReviewHypothesisSet",
				stageID,
			)
		}
		if stage.SideEffect != workflow.SideEffectNone {
			return workflow.Stage{}, fmt.Errorf("stage %q must deny side effects", stageID)
		}
		if stage.ReplayPolicy != workflow.ReplayPolicyExact {
			return workflow.Stage{}, fmt.Errorf("stage %q must use exact replay", stageID)
		}
		if stage.AuthorityCeiling == nil {
			return workflow.Stage{}, fmt.Errorf("stage %q has no authority ceiling", stageID)
		}
		if len(stage.DependsOn) != 0 {
			return workflow.Stage{}, fmt.Errorf(
				"S1 agent stage %q must not depend on unbound upstream stages",
				stageID,
			)
		}
		return stage, nil
	}
	return workflow.Stage{}, fmt.Errorf("workflow has no stage %q", stageID)
}

func validateSnapshotProjection(
	input CompileInput,
	policy reviewconfig.AgentReviewPolicy,
) error {
	snapshot := input.ExecutionSnapshot
	bundle := input.ConfigBundle
	workflowDigest, err := workflow.DigestDefinition(input.Workflow)
	if err != nil {
		return fmt.Errorf("digest workflow: %w", err)
	}
	if snapshot.Workflow != (runmodel.WorkflowRef{
		ID: input.Workflow.ID, Revision: input.Workflow.Revision, SHA256: workflowDigest,
	}) {
		return fmt.Errorf("execution snapshot does not bind the exact workflow identity")
	}
	if bundle.Workflow.Definition != (reviewconfig.VersionedRef{
		ID: input.Workflow.ID, Revision: input.Workflow.Revision, SHA256: workflowDigest,
	}) {
		return fmt.Errorf("config bundle does not bind the exact workflow identity")
	}
	if snapshot.Config.ID != bundle.BundleID ||
		snapshot.Config.Revision != bundle.SHA256[:16] ||
		snapshot.Config.SHA256 != bundle.SHA256 {
		return fmt.Errorf("execution snapshot does not bind the exact config identity")
	}
	if input.ReviewRunID != input.ReviewSpec.RequestID {
		return fmt.Errorf("review run id does not match ReviewSpec request_id")
	}
	expectedConfigInvocation := input.ReviewRunID
	if input.FormalReplay != nil {
		expectedConfigInvocation = bundle.Context.InvocationID
	}
	if bundle.Context.TenantID != input.ReviewSpec.TenantID ||
		bundle.Context.RepositoryID != input.ReviewSpec.Repository.RepositoryID ||
		bundle.Context.InvocationID != expectedConfigInvocation {
		return fmt.Errorf("config resolution context does not match ReviewSpec authorization")
	}
	if string(input.ReviewInput.TargetMode) != string(input.ReviewSpec.Target.Mode) {
		return fmt.Errorf("ReviewInput target mode does not match ReviewSpec")
	}
	if err := validateTargetClosure(
		input.Target,
		input.ReviewSpec,
		input.ReviewInput,
		bundle,
	); err != nil {
		return err
	}
	if input.ReviewSpec.Target.Mode == contractsv1alpha1.ReviewModeSelection {
		if bundle.Context.Path != input.ReviewSpec.Target.Selection.Path {
			return fmt.Errorf("config resolution path does not match selected path")
		}
	} else if bundle.Context.Path != "" {
		return fmt.Errorf("non-selection config resolution path must be empty")
	}
	if input.ReviewSpec.WorkflowRef != (contractsv1alpha1.VersionedRef{
		ID: snapshot.Workflow.ID, Revision: snapshot.Workflow.Revision, SHA256: snapshot.Workflow.SHA256,
	}) {
		return fmt.Errorf("ReviewSpec does not bind the execution snapshot workflow")
	}
	if input.ReviewSpec.ConfigBundleRef != (contractsv1alpha1.VersionedRef{
		ID:       snapshot.Config.ID,
		Revision: snapshot.Config.Revision,
		SHA256:   snapshot.ConfigBundleRef.SHA256,
	}) {
		return fmt.Errorf("ReviewSpec does not bind the execution snapshot config artifact")
	}
	if snapshot.ReviewSpecSHA256 != snapshot.ReviewSpecRef.SHA256 {
		return fmt.Errorf("execution snapshot ReviewSpec digest closure changed")
	}
	if !reflect.DeepEqual(snapshot.ToolPolicy.AllowedTools, bundle.Execution.AllowedTools) ||
		snapshot.ToolPolicy.Network != "deny" ||
		snapshot.ToolPolicy.WorkspaceWrites != "deny" ||
		snapshot.ToolPolicy.RemoteWrites != "deny" ||
		snapshot.ToolPolicy.MaxDelegationDepth != 0 ||
		snapshot.RemoteWrites != "deny" ||
		snapshot.ToolPolicy.PerCallTimeoutMS != bundle.Budget.StageTimeoutMS ||
		snapshot.ToolPolicy.MaxOutputBytes != bundle.Budget.MaxOutputBytes ||
		snapshot.ToolPolicy.MaxConcurrency != bundle.Budget.MaxConcurrency {
		return fmt.Errorf("config bundle is not exactly projected into execution snapshot authority")
	}
	if snapshot.RuntimeProfile != bundle.Execution.AgentProfile.ID+"@"+
		bundle.Execution.AgentProfile.Revision {
		return fmt.Errorf("execution snapshot runtime profile does not match config bundle")
	}
	if err := validateFormalReplayClosure(input); err != nil {
		return err
	}
	if policy.SHA256 == "" {
		return fmt.Errorf("agent review policy has no immutable identity")
	}
	return nil
}

func validateFormalReplayClosure(input CompileInput) error {
	snapshot := input.ExecutionSnapshot
	if input.FormalReplay == nil {
		if len(snapshot.ReplayInputRefs) != 0 || snapshot.ReplaySourceRunRef != nil ||
			snapshot.ReplayChangeSetRef != nil {
			return fmt.Errorf("formal AgentStagePlan replay requires a formal replay closure")
		}
		return nil
	}
	replay := *input.FormalReplay
	if len(snapshot.ReplayInputRefs) != 0 || snapshot.ReplaySourceRunRef == nil ||
		snapshot.ReplayChangeSetRef == nil || *snapshot.ReplaySourceRunRef != replay.SourceRunRef ||
		*snapshot.ReplayChangeSetRef != replay.ChangeSetRef {
		return fmt.Errorf("formal replay refs do not match execution snapshot lineage")
	}
	if err := replay.SourceRun.Validate(); err != nil || replay.SourceRun.Status != runmodel.RunStatusSucceeded {
		return fmt.Errorf("formal replay requires a valid succeeded source run")
	}
	if err := replay.SourceSnapshot.Validate(); err != nil {
		return fmt.Errorf("validate formal replay source snapshot: %w", err)
	}
	if err := replay.SourceReviewSpec.Validate(); err != nil {
		return fmt.Errorf("validate formal replay source ReviewSpec: %w", err)
	}
	if err := replay.SourceConfigBundle.Validate(); err != nil {
		return fmt.Errorf("validate formal replay source ConfigBundle: %w", err)
	}
	if err := replay.SourceConfigReceipt.ValidateAgainst(replay.SourceConfigBundle); err != nil {
		return fmt.Errorf("validate formal replay source config receipt: %w", err)
	}
	if err := replay.ChangeSet.Validate(); err != nil {
		return fmt.Errorf("validate formal replay change set: %w", err)
	}
	rootRunID := replay.SourceRun.RunID
	parentReplayRunID := ""
	if replay.SourceRun.Kind == runmodel.RunKindReplay {
		rootRunID = replay.SourceRun.ReplayRootRunID
		parentReplayRunID = replay.SourceRun.RunID
	}
	if replay.ChangeSet.Namespace != input.ReviewRunID ||
		replay.ChangeSet.SourceRunID != replay.SourceRun.RunID ||
		replay.ChangeSet.RootRunID != rootRunID ||
		replay.ChangeSet.ParentReplayRunID != parentReplayRunID ||
		(replay.ChangeSet.StartStage != input.StageID &&
			!(replay.ChangeSet.Variable == runmodel.ReplayVariableIndex &&
				replay.ChangeSet.StartStage == string(reviewcore.StageMaterializeTarget))) ||
		replay.ChangeSet.RemoteWrites != "deny" ||
		replay.ChangeSet.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		replay.ChangeSet.VariantSHA256 != input.ConfigBundle.SHA256 {
		return fmt.Errorf("formal replay lineage or config digests changed")
	}
	indexReplay := replay.ChangeSet.Variable == runmodel.ReplayVariableIndex
	workflowReplay := replay.ChangeSet.Variable == runmodel.ReplayVariableWorkflow
	if replay.SourceRun.ExecutionSnapshotID != replay.SourceSnapshot.ExecutionSnapshotID ||
		(!indexReplay && replay.SourceRun.TargetSnapshotRef != snapshot.TargetSnapshotRef) ||
		(!indexReplay && replay.SourceSnapshot.TargetSnapshotRef != snapshot.TargetSnapshotRef) ||
		(!indexReplay && replay.SourceSnapshot.ReviewInputRef != snapshot.ReviewInputRef) ||
		(!workflowReplay && replay.SourceSnapshot.WorkflowDefinitionRef != snapshot.WorkflowDefinitionRef) ||
		(!workflowReplay && replay.SourceSnapshot.Workflow != snapshot.Workflow) ||
		replay.SourceSnapshot.RuntimeProfile != snapshot.RuntimeProfile ||
		!slices.Equal(replay.SourceSnapshot.RuntimeEvidenceRefs, snapshot.RuntimeEvidenceRefs) ||
		replay.SourceSnapshot.RemoteWrites != snapshot.RemoteWrites {
		return fmt.Errorf("formal replay changed frozen source input, workflow, runtime, or authority")
	}
	sourceSpec := replay.SourceReviewSpec
	variantSpec := input.ReviewSpec
	sourceSpec.RequestID, sourceSpec.IdempotencyKey = "", ""
	variantSpec.RequestID, variantSpec.IdempotencyKey = "", ""
	switch replay.ChangeSet.Variable {
	case runmodel.ReplayVariableNone:
		if len(replay.ChangeSet.ChangedFields) != 0 ||
			replay.SourceSnapshot.BuildIdentity != snapshot.BuildIdentity ||
			replay.SourceSnapshot.ConfigBundleRef != snapshot.ConfigBundleRef ||
			replay.SourceSnapshot.Config != snapshot.Config ||
			!reflect.DeepEqual(replay.SourceConfigBundle, input.ConfigBundle) ||
			!reflect.DeepEqual(replay.SourceConfigReceipt, input.ConfigResolutionReceipt) ||
			!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, snapshot.ToolPolicy) {
			return fmt.Errorf("formal exact replay changed config or runtime authority")
		}
	case runmodel.ReplayVariableBudget:
		if replay.SourceSnapshot.BuildIdentity != snapshot.BuildIdentity {
			return fmt.Errorf("formal budget replay changed build identity")
		}
		if err := validateFormalBudgetReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
	case runmodel.ReplayVariableModel:
		if err := validateFormalModelReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
	case runmodel.ReplayVariablePrompt:
		if err := validateFormalPromptReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
	case runmodel.ReplayVariableSkillPack:
		if err := validateFormalSkillPackReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
	case runmodel.ReplayVariableKnowledgePack:
		if err := validateFormalKnowledgePackReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
	case runmodel.ReplayVariableRulePack:
		if err := validateFormalRulePackReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
	case runmodel.ReplayVariableWorkflow:
		if err := validateFormalWorkflowReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
		variantSpec.WorkflowRef = sourceSpec.WorkflowRef
	case runmodel.ReplayVariableIndex:
		if err := validateFormalIndexReplayConfig(input, replay); err != nil {
			return err
		}
		variantSpec.ConfigBundleRef = sourceSpec.ConfigBundleRef
	default:
		return fmt.Errorf("formal replay variable %q is not executable", replay.ChangeSet.Variable)
	}
	if !reflect.DeepEqual(sourceSpec, variantSpec) {
		return fmt.Errorf("formal replay changed ReviewSpec authorization or review intent")
	}
	return nil
}

func validateFormalRulePackReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	wantFields := []string{"rule_pack"}
	if !slices.Equal(replay.ChangeSet.ChangedFields, wantFields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal rule_pack replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableRulePack) ||
		!slices.Equal(binding.ChangedFields, wantFields) {
		return fmt.Errorf("formal rule_pack replay derived receipt escaped source config")
	}
	source, variant := replay.SourceConfigBundle, input.ConfigBundle
	sourcePack, variantPack := source.RulePack, variant.RulePack
	source.RulePack, variant.RulePack = reviewconfig.RulePack{}, reviewconfig.RulePack{}
	source.BundleID, source.SHA256 = "", ""
	variant.BundleID, variant.SHA256 = "", ""
	if reflect.DeepEqual(sourcePack, variantPack) || !reflect.DeepEqual(source, variant) ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) ||
		replay.SourceSnapshot.BuildIdentity != input.ExecutionSnapshot.BuildIdentity {
		return fmt.Errorf("formal rule_pack replay changed undeclared config or runtime authority")
	}
	return nil
}

func validateFormalWorkflowReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	fields, earliest, err := workflow.DiffReplayExecutionPolicy(
		replay.SourceWorkflow, input.Workflow,
	)
	if err != nil || earliest != input.StageID ||
		!slices.Equal(replay.ChangeSet.ChangedFields, fields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal workflow replay lacks one exact executable workflow diff")
	}
	for _, field := range fields {
		if !strings.HasPrefix(field, "workflow.stages."+input.StageID+".budget.") &&
			!strings.HasPrefix(field, "workflow.stages."+input.StageID+".retry.") {
			return fmt.Errorf("formal workflow replay changed a non-budget/retry execution field")
		}
	}
	if replay.SourceConfigBundle.AgentReview == nil {
		return fmt.Errorf("formal workflow replay source has no agent_review policy")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableWorkflow) ||
		!slices.Equal(binding.ChangedFields, fields) {
		return fmt.Errorf("formal workflow replay derived receipt escaped source config")
	}
	sourceConfig, variantConfig := replay.SourceConfigBundle, input.ConfigBundle
	sourceWorkflow, variantWorkflow := sourceConfig.Workflow, variantConfig.Workflow
	sourceConfig.Workflow, variantConfig.Workflow = reviewconfig.WorkflowPolicy{}, reviewconfig.WorkflowPolicy{}
	sourceConfig.BundleID, sourceConfig.SHA256 = "", ""
	variantConfig.BundleID, variantConfig.SHA256 = "", ""
	if reflect.DeepEqual(sourceWorkflow, variantWorkflow) ||
		!reflect.DeepEqual(sourceConfig, variantConfig) ||
		replay.SourceSnapshot.BuildIdentity != input.ExecutionSnapshot.BuildIdentity ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) {
		return fmt.Errorf("formal workflow replay changed undeclared config or runtime authority")
	}
	sourceDigest, err := workflow.DigestDefinition(replay.SourceWorkflow)
	if err != nil {
		return err
	}
	variantDigest, err := workflow.DigestDefinition(input.Workflow)
	if err != nil {
		return err
	}
	if replay.SourceSnapshot.Workflow != (runmodel.WorkflowRef{
		ID: replay.SourceWorkflow.ID, Revision: replay.SourceWorkflow.Revision, SHA256: sourceDigest,
	}) || input.ExecutionSnapshot.Workflow != (runmodel.WorkflowRef{
		ID: input.Workflow.ID, Revision: input.Workflow.Revision, SHA256: variantDigest,
	}) || sourceWorkflow.Definition != (reviewconfig.VersionedRef{
		ID: replay.SourceWorkflow.ID, Revision: replay.SourceWorkflow.Revision, SHA256: sourceDigest,
	}) || variantWorkflow.Definition != (reviewconfig.VersionedRef{
		ID: input.Workflow.ID, Revision: input.Workflow.Revision, SHA256: variantDigest,
	}) {
		return fmt.Errorf("formal workflow replay identity does not bind exact definitions")
	}
	budget := replay.SourceConfigBundle.AgentReview.Budget
	if !workflow.ReplayChangesEffectivePolicy(
		replay.SourceWorkflow,
		input.Workflow,
		workflow.RuntimeLimits{
			TimeoutMS:      budget.TimeoutMS,
			MaxInputBytes:  budget.MaxTargetBytes,
			MaxOutputBytes: budget.MaxOutputBytes,
			MaxAttempts:    replay.SourceConfigBundle.Budget.MaxAttempts,
			MaxConcurrency: budget.MaxConcurrency,
		},
	) {
		return fmt.Errorf("formal workflow replay does not change effective AgentStagePlan budget")
	}
	return nil
}

func validateFormalIndexReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	fields, err := reviewconfig.DiffReplayIndexPolicy(
		replay.SourceConfigBundle.Execution, input.ConfigBundle.Execution,
	)
	if err != nil || !slices.Equal(replay.ChangeSet.ChangedFields, fields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal index replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableIndex) ||
		!slices.Equal(binding.ChangedFields, fields) {
		return fmt.Errorf("formal index replay derived receipt escaped source config")
	}
	sourceConfig, variantConfig := replay.SourceConfigBundle, input.ConfigBundle
	sourceConfig.Execution.ContextProviders = nil
	variantConfig.Execution.ContextProviders = nil
	sourceConfig.BundleID, sourceConfig.SHA256 = "", ""
	variantConfig.BundleID, variantConfig.SHA256 = "", ""
	if !reflect.DeepEqual(sourceConfig, variantConfig) ||
		replay.SourceSnapshot.BuildIdentity != input.ExecutionSnapshot.BuildIdentity ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) ||
		reflect.DeepEqual(replay.SourceSnapshot.ContextProviderReceiptRefs,
			input.ExecutionSnapshot.ContextProviderReceiptRefs) ||
		replay.SourceSnapshot.TargetSnapshotRef == input.ExecutionSnapshot.TargetSnapshotRef ||
		replay.SourceSnapshot.ReviewInputRef == input.ExecutionSnapshot.ReviewInputRef ||
		len(input.ExecutionSnapshot.ReplayInputRefs) != 0 {
		return fmt.Errorf("formal index replay changed undeclared config or runtime authority")
	}
	sourceTarget, variantTarget := replay.SourceTarget, input.Target
	sourceTarget.Contexts = nil
	variantTarget.Contexts = nil
	if !reflect.DeepEqual(sourceTarget, variantTarget) {
		return fmt.Errorf("formal index replay changed target outside contexts")
	}
	sourceInput := replay.SourceReviewInput
	variantInput := input.ReviewInput
	sourceInput.Contexts = nil
	variantInput.Contexts = nil
	if !reflect.DeepEqual(sourceInput, variantInput) ||
		!reflect.DeepEqual(input.Target.Contexts, input.ReviewInput.Contexts) {
		return fmt.Errorf("formal index replay target and ReviewInput contexts differ")
	}
	return nil
}

func validateFormalSkillPackReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	wantFields := []string{"agent_review.skill_packs"}
	if !slices.Equal(replay.ChangeSet.ChangedFields, wantFields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal skill_pack replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableSkillPack) ||
		!slices.Equal(binding.ChangedFields, wantFields) {
		return fmt.Errorf("formal skill_pack replay derived receipt escaped source config")
	}
	source, variant := replay.SourceConfigBundle, input.ConfigBundle
	if source.Context != variant.Context || !slices.Equal(source.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(source.Target, variant.Target) ||
		!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(source.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(source.Execution, variant.Execution) ||
		!reflect.DeepEqual(source.Budget, variant.Budget) ||
		!reflect.DeepEqual(source.Verification, variant.Verification) ||
		!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(source.Publication, variant.Publication) ||
		!reflect.DeepEqual(source.Data, variant.Data) ||
		!reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(source.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(source.Explain, variant.Explain) ||
		source.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal skill_pack replay changed a non-skill config fact")
	}
	sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
	sourceSkills, variantSkills := sourcePolicy.SkillPacks, variantPolicy.SkillPacks
	sourcePolicy.SkillPacks, variantPolicy.SkillPacks = nil, nil
	sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if len(variantSkills) == 0 || reflect.DeepEqual(sourceSkills, variantSkills) ||
		!reflect.DeepEqual(sourcePolicy, variantPolicy) ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) ||
		replay.SourceSnapshot.BuildIdentity == input.ExecutionSnapshot.BuildIdentity {
		return fmt.Errorf("formal skill_pack replay changed undeclared skill/runtime authority")
	}
	return nil
}

func validateFormalFilterPolicyReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	wantFields := []string{"finding_governance"}
	if !replay.ChangeSet.Variable.IsFilterPolicy() ||
		!slices.Equal(replay.ChangeSet.ChangedFields, wantFields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal filter_policy replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(replay.ChangeSet.Variable) ||
		!slices.Equal(binding.ChangedFields, wantFields) {
		return fmt.Errorf("formal filter_policy replay derived receipt escaped source config")
	}
	source, variant := replay.SourceConfigBundle, input.ConfigBundle
	sourcePolicy, variantPolicy := source.FindingGovernance, variant.FindingGovernance
	source.FindingGovernance, variant.FindingGovernance = nil, nil
	source.BundleID, source.SHA256 = "", ""
	variant.BundleID, variant.SHA256 = "", ""
	if sourcePolicy == nil || variantPolicy == nil || reflect.DeepEqual(sourcePolicy, variantPolicy) ||
		!reflect.DeepEqual(source, variant) ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) ||
		replay.SourceSnapshot.BuildIdentity != input.ExecutionSnapshot.BuildIdentity {
		return fmt.Errorf("formal filter_policy replay changed undeclared config or runtime authority")
	}
	return nil
}

func validateFormalKnowledgePackReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	wantFields := []string{"agent_review.knowledge_packs"}
	if !slices.Equal(replay.ChangeSet.ChangedFields, wantFields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal knowledge_pack replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableKnowledgePack) ||
		!slices.Equal(binding.ChangedFields, wantFields) {
		return fmt.Errorf("formal knowledge_pack replay derived receipt escaped source config")
	}
	source, variant := replay.SourceConfigBundle, input.ConfigBundle
	if source.Context != variant.Context || !slices.Equal(source.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(source.Target, variant.Target) ||
		!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(source.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(source.Execution, variant.Execution) ||
		!reflect.DeepEqual(source.Budget, variant.Budget) ||
		!reflect.DeepEqual(source.Verification, variant.Verification) ||
		!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(source.Publication, variant.Publication) ||
		!reflect.DeepEqual(source.Data, variant.Data) ||
		!reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(source.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(source.Explain, variant.Explain) ||
		source.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal knowledge_pack replay changed a non-knowledge config fact")
	}
	sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
	sourceKnowledge, variantKnowledge := sourcePolicy.KnowledgePacks, variantPolicy.KnowledgePacks
	sourcePolicy.KnowledgePacks, variantPolicy.KnowledgePacks = nil, nil
	sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if len(variantKnowledge) == 0 || reflect.DeepEqual(sourceKnowledge, variantKnowledge) ||
		!reflect.DeepEqual(sourcePolicy, variantPolicy) ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) ||
		replay.SourceSnapshot.BuildIdentity == input.ExecutionSnapshot.BuildIdentity {
		return fmt.Errorf("formal knowledge_pack replay changed undeclared knowledge/runtime authority")
	}
	return nil
}

func validateFormalPromptReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	wantFields := []string{"agent_review.prompt"}
	if !slices.Equal(replay.ChangeSet.ChangedFields, wantFields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal prompt replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariablePrompt) ||
		!slices.Equal(binding.ChangedFields, wantFields) {
		return fmt.Errorf("formal prompt replay derived receipt escaped source config")
	}
	source, variant := replay.SourceConfigBundle, input.ConfigBundle
	if source.Context != variant.Context || !slices.Equal(source.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(source.Target, variant.Target) ||
		!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(source.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(source.Execution, variant.Execution) ||
		!reflect.DeepEqual(source.Budget, variant.Budget) ||
		!reflect.DeepEqual(source.Verification, variant.Verification) ||
		!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(source.Publication, variant.Publication) ||
		!reflect.DeepEqual(source.Data, variant.Data) ||
		!reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(source.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(source.Explain, variant.Explain) ||
		source.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal prompt replay changed a non-prompt config fact")
	}
	sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
	sourcePrompt, variantPrompt := sourcePolicy.Prompt, variantPolicy.Prompt
	sourcePolicy.Prompt, variantPolicy.Prompt = reviewconfig.VersionedRef{}, reviewconfig.VersionedRef{}
	sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if sourcePrompt == variantPrompt || !reflect.DeepEqual(sourcePolicy, variantPolicy) ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) ||
		replay.SourceSnapshot.BuildIdentity == input.ExecutionSnapshot.BuildIdentity {
		return fmt.Errorf("formal prompt replay changed undeclared prompt/runtime authority")
	}
	return nil
}

func validateFormalModelReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	wantFields := []string{"agent_review.model"}
	if !slices.Equal(replay.ChangeSet.ChangedFields, wantFields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal model replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableModel) ||
		!slices.Equal(binding.ChangedFields, wantFields) {
		return fmt.Errorf("formal model replay derived receipt escaped source config")
	}
	source, variant := replay.SourceConfigBundle, input.ConfigBundle
	if source.Context != variant.Context || !slices.Equal(source.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(source.Target, variant.Target) ||
		!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(source.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(source.Execution, variant.Execution) ||
		!reflect.DeepEqual(source.Budget, variant.Budget) ||
		!reflect.DeepEqual(source.Verification, variant.Verification) ||
		!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(source.Publication, variant.Publication) ||
		!reflect.DeepEqual(source.Data, variant.Data) ||
		!reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(source.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(source.Explain, variant.Explain) ||
		source.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal model replay changed a non-model config fact")
	}
	sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
	sourceModel, variantModel := sourcePolicy.Model, variantPolicy.Model
	sourcePolicy.Model, variantPolicy.Model = reviewconfig.VersionedRef{}, reviewconfig.VersionedRef{}
	sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if sourceModel == variantModel || !reflect.DeepEqual(sourcePolicy, variantPolicy) ||
		!reflect.DeepEqual(replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy) ||
		replay.SourceSnapshot.BuildIdentity == input.ExecutionSnapshot.BuildIdentity {
		return fmt.Errorf("formal model replay changed undeclared model/runtime authority")
	}
	return nil
}

func validateFormalBudgetReplayConfig(input CompileInput, replay FormalReplayClosure) error {
	wantFields := []string{"agent_review.budget.timeout_ms", "budget.stage_timeout_ms"}
	if !slices.Equal(replay.ChangeSet.ChangedFields, wantFields) ||
		input.ConfigResolutionReceipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant ||
		input.ConfigResolutionReceipt.ReplayVariant == nil {
		return fmt.Errorf("formal budget replay lacks an exact derived config receipt")
	}
	binding := input.ConfigResolutionReceipt.ReplayVariant
	if binding.SourceReceiptID != replay.SourceConfigReceipt.ReceiptID ||
		binding.SourceReceiptSHA256 != replay.SourceConfigReceipt.SHA256 ||
		binding.BaselineBundleID != replay.SourceConfigBundle.BundleID ||
		binding.BaselineSHA256 != replay.SourceConfigBundle.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableBudget) ||
		!slices.Equal(binding.ChangedFields, wantFields) {
		return fmt.Errorf("formal budget replay derived receipt escaped source config")
	}
	source, variant := replay.SourceConfigBundle, input.ConfigBundle
	if source.Context != variant.Context || !slices.Equal(source.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(source.Target, variant.Target) ||
		!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(source.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(source.Execution, variant.Execution) ||
		!reflect.DeepEqual(source.Verification, variant.Verification) ||
		!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(source.Publication, variant.Publication) ||
		!reflect.DeepEqual(source.Data, variant.Data) ||
		!reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(source.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(source.Explain, variant.Explain) ||
		source.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal budget replay changed a non-budget config fact")
	}
	sourceBudget, variantBudget := source.Budget, variant.Budget
	sourceBudget.StageTimeoutMS, variantBudget.StageTimeoutMS = 0, 0
	if source.Budget.StageTimeoutMS == variant.Budget.StageTimeoutMS ||
		!reflect.DeepEqual(sourceBudget, variantBudget) {
		return fmt.Errorf("formal budget replay changed an undeclared outer budget")
	}
	sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
	sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
	sourcePolicy.Budget.TimeoutMS, variantPolicy.Budget.TimeoutMS = 0, 0
	if source.AgentReview.Budget.TimeoutMS == variant.AgentReview.Budget.TimeoutMS ||
		!reflect.DeepEqual(sourcePolicy, variantPolicy) ||
		variant.Budget.StageTimeoutMS != variant.AgentReview.Budget.TimeoutMS {
		return fmt.Errorf("formal budget replay changed an undeclared agent budget")
	}
	sourceTools, variantTools := replay.SourceSnapshot.ToolPolicy, input.ExecutionSnapshot.ToolPolicy
	sourceTools.PerCallTimeoutMS, variantTools.PerCallTimeoutMS = 0, 0
	if !reflect.DeepEqual(sourceTools, variantTools) ||
		input.ExecutionSnapshot.ToolPolicy.PerCallTimeoutMS != variant.Budget.StageTimeoutMS {
		return fmt.Errorf("formal budget replay changed non-timeout execution authority")
	}
	return nil
}

func validateComponentClosure(
	policy reviewconfig.AgentReviewPolicy,
	bundle reviewconfig.ConfigBundle,
	components ResolvedComponents,
) error {
	for _, component := range []struct {
		name string
		got  contractsv1alpha1.AgentStageComponentBinding
		want reviewconfig.VersionedRef
	}{
		{"agent", components.Agent, policy.Agent},
		{"provider", components.Provider, policy.Provider},
		{"model", components.Model, policy.Model},
		{"runtime", components.Runtime, bundle.Execution.AgentProfile},
		{"prompt", components.Prompt, policy.Prompt},
		{"api protocol", components.APIProtocol, policy.APIProtocol},
	} {
		if component.got.Ref != versionedRef(
			component.want.ID,
			component.want.Revision,
			component.want.SHA256,
		) {
			return fmt.Errorf("%s component does not match agent review policy", component.name)
		}
	}
	if len(components.Skills) != len(policy.SkillPacks) {
		return fmt.Errorf("resolved skills do not close the agent review policy")
	}
	for index, configured := range policy.SkillPacks {
		binding := components.Skills[index]
		if binding.ID != configured.ID ||
			binding.Phase != contractsv1alpha1.AgentStageSkillPhase(configured.Phase) ||
			binding.Ref != versionedRef(
				configured.Ref.ID,
				configured.Ref.Revision,
				configured.Ref.SHA256,
			) {
			return fmt.Errorf(
				"resolved skill at index %d does not preserve agent review policy order",
				index,
			)
		}
	}
	if len(components.Knowledge) != len(policy.KnowledgePacks) {
		return fmt.Errorf("resolved knowledge does not close the agent review policy")
	}
	for index, configured := range policy.KnowledgePacks {
		binding := components.Knowledge[index]
		if binding.ID != configured.ID || binding.Ref != versionedRef(
			configured.Ref.ID,
			configured.Ref.Revision,
			configured.Ref.SHA256,
		) {
			return fmt.Errorf(
				"resolved knowledge at index %d does not preserve agent review policy order",
				index,
			)
		}
	}
	if len(components.ContextProviders) != len(bundle.Execution.ContextProviders) {
		return fmt.Errorf("resolved context providers do not close execution policy")
	}
	for index, configured := range bundle.Execution.ContextProviders {
		binding := components.ContextProviders[index]
		if binding.ID != configured.ID || binding.Revision != configured.Revision ||
			binding.Kind != configured.Kind ||
			binding.Adapter.Ref != versionedRef(
				configured.Adapter.ID,
				configured.Adapter.Revision,
				configured.Adapter.SHA256,
			) {
			return fmt.Errorf(
				"resolved context provider at index %d does not preserve execution policy order",
				index,
			)
		}
	}
	return nil
}

func validateAuthorityAndBudget(
	policy reviewconfig.AgentReviewPolicy,
	bundle reviewconfig.ConfigBundle,
	stage workflow.Stage,
	snapshot runmodel.ExecutionSnapshot,
	target targetmodel.MaterializedTarget,
	reviewInput reviewcore.ReviewInput,
	components ResolvedComponents,
) error {
	ceiling := stage.AuthorityCeiling
	if ceiling == nil {
		return fmt.Errorf("agent stage has no authority ceiling")
	}
	if string(policy.Authority.ModelEgress) != ceiling.ModelEgress ||
		string(policy.Authority.ToolNetwork) != ceiling.ToolNetwork ||
		string(policy.Authority.WorkspaceReads) != ceiling.WorkspaceReads ||
		string(policy.Authority.WorkspaceWrites) != ceiling.WorkspaceWrites ||
		string(policy.Authority.RemoteWrites) != ceiling.RemoteWrites ||
		policy.Authority.MaxDelegationDepth > ceiling.MaxDelegationDepth {
		return fmt.Errorf("agent review authority exceeds workflow stage ceiling")
	}
	for _, tool := range policy.Authority.Tools {
		if !slices.Contains(ceiling.AllowedTools, tool) ||
			!slices.Contains(snapshot.ToolPolicy.AllowedTools, tool) {
			return fmt.Errorf("agent review tool %q exceeds frozen authority ceiling", tool)
		}
	}
	budget := effectiveAgentStageBudget(policy.Budget, stage.Budget)
	if budget.MaxModelCalls > ceiling.MaxModelCalls || budget.MaxToolCalls > ceiling.MaxToolCalls {
		return fmt.Errorf("agent review call budget exceeds workflow authority ceiling")
	}
	if budget.TimeoutMS > snapshot.ToolPolicy.PerCallTimeoutMS ||
		budget.MaxOutputBytes > snapshot.ToolPolicy.MaxOutputBytes ||
		budget.MaxConcurrency > snapshot.ToolPolicy.MaxConcurrency {
		return fmt.Errorf("agent review resource budget exceeds frozen stage/snapshot ceiling")
	}
	if len(target.FileRefs) > budget.MaxFiles || len(reviewInput.Files) > budget.MaxFiles {
		return fmt.Errorf("review input file count exceeds agent review budget")
	}
	frozenReviewBytes := snapshot.ReviewInputRef.SizeBytes
	for _, binding := range reviewInput.Contexts {
		if binding.Ref == nil {
			continue
		}
		var err error
		frozenReviewBytes, err = addSize(frozenReviewBytes, binding.Ref.SizeBytes)
		if err != nil {
			return fmt.Errorf("frozen review/context input size: %w", err)
		}
	}
	if frozenReviewBytes > budget.MaxTargetBytes {
		return fmt.Errorf("frozen review input exceeds agent review target byte budget")
	}
	closureBytes := frozenReviewBytes
	componentBindings := []contractsv1alpha1.AgentStageComponentBinding{
		components.Agent,
		components.Provider,
		components.Model,
		components.Runtime,
		components.Prompt,
		components.APIProtocol,
	}
	for _, skill := range components.Skills {
		componentBindings = append(componentBindings, contractsv1alpha1.AgentStageComponentBinding{
			Ref: skill.Ref, Artifact: skill.Artifact,
		})
	}
	for _, knowledge := range components.Knowledge {
		componentBindings = append(
			componentBindings,
			contractsv1alpha1.AgentStageComponentBinding{
				Ref: knowledge.Ref, Artifact: knowledge.Artifact,
			},
		)
	}
	for _, provider := range components.ContextProviders {
		componentBindings = append(componentBindings, provider.Adapter)
	}
	for _, binding := range componentBindings {
		var err error
		closureBytes, err = addSize(closureBytes, binding.Artifact.Ref.SizeBytes)
		if err != nil {
			return fmt.Errorf("formal agent input closure size: %w", err)
		}
	}
	if closureBytes > stage.Budget.MaxInputBytes ||
		closureBytes > bundle.Budget.MaxInputBytes {
		return fmt.Errorf("formal agent component/input closure exceeds frozen input byte budget")
	}
	return nil
}

// effectiveAgentStageBudget closes the two independently versioned budget
// layers into the exact worker request. AgentReviewPolicy supplies the product
// limits while WorkflowDefinition may tighten the resources owned by its
// stage. Keeping the effective values in AgentStagePlan makes workflow replay
// observable, sealed and independently verifiable by the Pi mapper.
func effectiveAgentStageBudget(
	policy reviewconfig.AgentBudget,
	stage workflow.StageBudget,
) contractsv1alpha1.AgentStageBudget {
	return contractsv1alpha1.AgentStageBudget{
		MaxFiles:        policy.MaxFiles,
		MaxGroups:       policy.MaxGroups,
		MaxHypotheses:   policy.MaxHypotheses,
		MaxModelCalls:   policy.MaxModelCalls,
		MaxToolCalls:    policy.MaxToolCalls,
		MaxTargetBytes:  min(policy.MaxTargetBytes, stage.MaxInputBytes),
		MaxGroupBytes:   policy.MaxGroupBytes,
		MaxOutputBytes:  min(policy.MaxOutputBytes, stage.MaxOutputBytes),
		MaxOutputTokens: policy.MaxOutputTokens,
		MaxCostMicros:   policy.MaxCostMicros,
		TimeoutMS:       min(policy.TimeoutMS, stage.TimeoutMS),
		MaxConcurrency:  min(policy.MaxConcurrency, stage.MaxConcurrency),
	}
}

func addSize(left, right int64) (int64, error) {
	if left < 0 || right < 0 || right > int64(^uint64(0)>>1)-left {
		return 0, fmt.Errorf("size overflow")
	}
	return left + right, nil
}

func validateTargetClosure(
	target targetmodel.MaterializedTarget,
	spec contractsv1alpha1.ReviewSpec,
	input reviewcore.ReviewInput,
	bundle reviewconfig.ConfigBundle,
) error {
	if target.Snapshot.TargetSnapshotID != input.TargetID ||
		string(target.Snapshot.Mode) != string(input.TargetMode) ||
		string(target.Snapshot.Mode) != string(spec.Target.Mode) {
		return fmt.Errorf("materialized target identity/mode does not match ReviewSpec and ReviewInput")
	}
	if !reflect.DeepEqual(target.Contexts, input.Contexts) {
		return fmt.Errorf("materialized contexts do not exactly match ReviewInput")
	}
	if spec.Repository.Provider != "local-git" ||
		target.Snapshot.Repository.Kind != "local_git" ||
		spec.Repository.RepositoryID != target.Snapshot.Repository.RepositoryID {
		return fmt.Errorf("materialized repository does not match ReviewSpec")
	}
	if !slices.Contains(bundle.Target.AllowedModes, string(target.Snapshot.Mode)) {
		return fmt.Errorf("materialized target mode is denied by frozen config")
	}
	if len(target.FileRefs) > bundle.Target.MaxFiles {
		return fmt.Errorf("materialized target exceeds frozen max_files")
	}
	for _, file := range target.FileRefs {
		allowed, err := gitadapter.ScopeIncludesPath(
			file.Path,
			bundle.Target.Include,
			bundle.Target.Exclude,
		)
		if err != nil {
			return fmt.Errorf("evaluate frozen target policy for %q: %w", file.Path, err)
		}
		if !allowed {
			return fmt.Errorf("materialized path %q is denied by frozen target policy", file.Path)
		}
	}
	if err := validateFrozenFiles(target.FileRefs, input.Files); err != nil {
		return err
	}

	switch target.Snapshot.Mode {
	case reviewcore.TargetModeDiff:
		if spec.Target.Diff == nil || target.Snapshot.Diff == nil || target.PatchRef == nil ||
			spec.Target.Diff.BaseRevision != target.Snapshot.Base.CommitOID ||
			spec.Target.Diff.HeadRevision != target.Snapshot.Head.CommitOID {
			return fmt.Errorf("ReviewSpec diff target does not match materialized target")
		}
		patchRef := artifactRefFromContent(
			spec.Target.Diff.Patch,
			runmodel.ContractCanonicalPatch,
		)
		if patchRef != *target.PatchRef ||
			patchRef.SHA256 != sha256Hex([]byte(input.CanonicalPatch)) ||
			patchRef.SizeBytes != int64(len(input.CanonicalPatch)) {
			return fmt.Errorf("canonical patch does not close ReviewSpec, target, and ReviewInput")
		}
		inspection, err := reviewcore.InspectCanonicalPatch(
			context.Background(),
			input.CanonicalPatch,
		)
		if err != nil {
			return fmt.Errorf("inspect canonical patch: %w", err)
		}
		if len(inspection.Files) != len(target.FileRefs) {
			return fmt.Errorf("canonical patch does not exactly cover materialized file refs")
		}
		for index, file := range inspection.Files {
			if !file.PathKnown || file.Path != target.FileRefs[index].Path {
				return fmt.Errorf("canonical patch path coverage differs from materialized target")
			}
		}
	case reviewcore.TargetModeSelection:
		selection := spec.Target.Selection
		snapshot := target.Snapshot.Selection
		if selection == nil || snapshot == nil || target.SelectionContentRef == nil ||
			selection.Revision != target.Snapshot.Head.CommitOID ||
			target.Snapshot.Base.CommitOID != target.Snapshot.Head.CommitOID ||
			selection.Path != snapshot.Path ||
			!selectionSelectorMatchesSnapshot(*selection, *snapshot) ||
			!selectionRegionsMatchSnapshot(input.Regions, *snapshot) {
			return fmt.Errorf("ReviewSpec selection does not match materialized target")
		}
		selected, err := selectedRegionsContent(input)
		if err != nil {
			return fmt.Errorf("reconstruct frozen selection: %w", err)
		}
		contentRef := artifactRefFromContent(
			selection.Content,
			runmodel.ContractSelectionContent,
		)
		if contentRef != *target.SelectionContentRef ||
			contentRef.SHA256 != sha256Hex(selected) ||
			contentRef.SizeBytes != int64(len(selected)) {
			return fmt.Errorf("selection content does not close ReviewSpec, target, and ReviewInput")
		}
	case reviewcore.TargetModeScope:
		scope := spec.Target.Scope
		snapshot := target.Snapshot.Scope
		if scope == nil || snapshot == nil ||
			scope.Revision != target.Snapshot.Head.CommitOID ||
			target.Snapshot.Base.CommitOID != target.Snapshot.Head.CommitOID ||
			!reflect.DeepEqual(scope.Include, snapshot.Include) ||
			!reflect.DeepEqual(scope.Exclude, snapshot.Exclude) {
			return fmt.Errorf("ReviewSpec scope does not match materialized target")
		}
		for _, file := range target.FileRefs {
			allowed, err := gitadapter.ScopeIncludesPath(file.Path, scope.Include, scope.Exclude)
			if err != nil {
				return fmt.Errorf("evaluate ReviewSpec scope for %q: %w", file.Path, err)
			}
			if !allowed {
				return fmt.Errorf("materialized path %q is outside ReviewSpec scope", file.Path)
			}
		}
	default:
		return fmt.Errorf("unsupported materialized target mode %q", target.Snapshot.Mode)
	}
	return nil
}

func validateFrozenFiles(
	targetFiles []targetmodel.TargetFileRef,
	inputFiles []reviewcore.FileManifestEntry,
) error {
	targetByPath := make(map[string]targetmodel.TargetFileRef, len(targetFiles))
	for _, file := range targetFiles {
		targetByPath[file.Path] = file
	}
	inputByPath := make(map[string]reviewcore.FileManifestEntry, len(inputFiles))
	for _, file := range inputFiles {
		targetFile, exists := targetByPath[file.Path]
		if !exists || targetFile.SHA256 != file.SHA256 ||
			targetFile.SizeBytes != file.SizeBytes || targetFile.ContentRef == nil ||
			targetFile.ContentRef.SHA256 != file.SHA256 ||
			targetFile.ContentRef.SizeBytes != file.SizeBytes ||
			targetFile.ContentRef.Contract != targetmodel.ContractFileContent {
			return fmt.Errorf("ReviewInput file %q is not bound by materialized target", file.Path)
		}
		inputByPath[file.Path] = file
	}
	for _, file := range targetFiles {
		if file.SHA256 == "" {
			continue
		}
		if _, exists := inputByPath[file.Path]; !exists {
			return fmt.Errorf("materialized file %q is absent from ReviewInput", file.Path)
		}
	}
	return nil
}

func artifactRefFromContent(
	ref contractsv1alpha1.ContentRef,
	contract string,
) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes, Contract: contract,
	}
}

func selectionSelectorMatchesSnapshot(
	selection contractsv1alpha1.SelectionTarget,
	snapshot targetmodel.SelectionSnapshot,
) bool {
	switch {
	case selection.StartLine != 0 || selection.EndLine != 0:
		return snapshot.StartLine == selection.StartLine &&
			snapshot.EndLine == selection.EndLine && snapshot.Symbol == nil &&
			len(snapshot.EffectiveRanges) == 1 &&
			snapshot.EffectiveRanges[0] == (targetmodel.SelectionRange{
				StartLine: selection.StartLine, EndLine: selection.EndLine,
			})
	case selection.Symbol != nil:
		return snapshot.StartLine == 0 && snapshot.EndLine == 0 &&
			snapshot.Symbol != nil &&
			snapshot.Symbol.Language == selection.Symbol.Language &&
			snapshot.Symbol.Kind == selection.Symbol.Kind &&
			snapshot.Symbol.QualifiedName == selection.Symbol.QualifiedName
	default:
		if snapshot.StartLine != 0 || snapshot.EndLine != 0 || snapshot.Symbol != nil ||
			len(selection.Ranges) != len(snapshot.EffectiveRanges) {
			return false
		}
		for index, lineRange := range selection.Ranges {
			if snapshot.EffectiveRanges[index] != (targetmodel.SelectionRange{
				StartLine: lineRange.StartLine, EndLine: lineRange.EndLine,
			}) {
				return false
			}
		}
		return true
	}
}

func selectionRegionsMatchSnapshot(
	regions []reviewcore.ReviewRegion,
	snapshot targetmodel.SelectionSnapshot,
) bool {
	if len(regions) != len(snapshot.EffectiveRanges) {
		return false
	}
	for index, region := range regions {
		if region.Path != snapshot.Path || region.SHA256 != snapshot.FileSHA256 ||
			region.StartLine != snapshot.EffectiveRanges[index].StartLine ||
			region.EndLine != snapshot.EffectiveRanges[index].EndLine {
			return false
		}
	}
	return true
}

func selectedRegionsContent(input reviewcore.ReviewInput) ([]byte, error) {
	var selected bytes.Buffer
	files := make(map[string]reviewcore.FileManifestEntry, len(input.Files))
	for _, file := range input.Files {
		files[file.Path] = file
	}
	for _, region := range input.Regions {
		file, exists := files[region.Path]
		if !exists || file.Content == nil || file.SHA256 != region.SHA256 {
			return nil, fmt.Errorf("selection region has no exact frozen file")
		}
		lines := strings.Split(*file.Content, "\n")
		if strings.HasSuffix(*file.Content, "\n") {
			lines = lines[:len(lines)-1]
		}
		if region.StartLine == 0 || region.EndLine < region.StartLine ||
			uint64(region.EndLine) > uint64(len(lines)) {
			return nil, fmt.Errorf("selection region exceeds frozen file content")
		}
		selected.WriteString(strings.Join(lines[region.StartLine-1:region.EndLine], "\n"))
		selected.WriteByte('\n')
	}
	return selected.Bytes(), nil
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func digestStage(stage workflow.Stage) (string, error) {
	data, err := json.Marshal(stage)
	if err != nil {
		return "", fmt.Errorf("marshal agent stage: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func versionedRef(id, revision, digest string) contractsv1alpha1.VersionedRef {
	return contractsv1alpha1.VersionedRef{ID: id, Revision: revision, SHA256: digest}
}
