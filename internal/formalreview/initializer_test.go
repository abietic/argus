package formalreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/piexecution"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestBuildLocalPiBootstrapInspectsRuntimeAndProducesValidConfig(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-v4-pro[1m]",
	})
	if err != nil {
		t.Fatalf("BuildLocalPiBootstrap() error = %v", err)
	}
	promptBytes, err := contractsv1alpha1.MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.Manifest.Prompt.SHA256 != digestBytes(promptBytes) {
		t.Fatalf("prompt digest = %q", bootstrap.Manifest.Prompt.SHA256)
	}
	if bootstrap.Manifest.Provider.ID != "deepseek-anthropic" {
		t.Fatalf("provider ID = %q, want worker provider identity", bootstrap.Manifest.Provider.ID)
	}
	if strings.ContainsAny(bootstrap.Manifest.Model.ID, "[]") ||
		!strings.HasPrefix(bootstrap.Manifest.Model.ID, "deepseek-anthropic-model-") {
		t.Fatalf("model component ID is not a safe derived identity: %q", bootstrap.Manifest.Model.ID)
	}
	if err := bootstrap.Revision.Validate(); err != nil {
		t.Fatalf("config Revision.Validate() error = %v", err)
	}
	if bootstrap.Revision.Patch.AgentReview == nil ||
		bootstrap.Revision.Patch.AgentReview.Normalization == nil ||
		bootstrap.Revision.Patch.AgentReview.Normalization.Revision !=
			contractsv1alpha1.CandidateNormalizationCurrentRevision ||
		bootstrap.Revision.Patch.AgentReview.Normalization.SHA256 !=
			bootstrap.Manifest.Agent.SHA256 {
		t.Fatalf("bootstrap normalization selector = %+v", bootstrap.Revision.Patch.AgentReview)
	}
	v1, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-v4-pro[1m]",
		NormalizationRevision: contractsv1alpha1.CandidateNormalizationRevisionV1,
	})
	if err != nil {
		t.Fatalf("BuildLocalPiBootstrap(v1) error = %v", err)
	}
	if v1.Revision.Patch.AgentReview.Normalization.Revision !=
		contractsv1alpha1.CandidateNormalizationRevisionV1 ||
		v1.Revision.Patch.AgentReview.Normalization.SHA256 != v1.Manifest.Agent.SHA256 {
		t.Fatalf("v1 normalization selector = %+v", v1.Revision.Patch.AgentReview.Normalization)
	}
	if len(bootstrap.Manifest.ReviewSkills) != 6 || len(bootstrap.Components) != 15 ||
		len(bootstrap.Publications) != len(bootstrap.Components) {
		t.Fatalf("bootstrap closure = %+v", bootstrap)
	}
	wantRevisions := map[string]string{
		"concurrency-data":   "builtin-v2",
		"correctness":        "builtin-v2",
		"error-contract":     "builtin-v1",
		"resource-lifecycle": "builtin-v1",
		"security-contract":  "builtin-v1",
		"transaction-state":  "builtin-v1",
	}
	for _, ref := range bootstrap.Manifest.ReviewSkills {
		if wantRevisions[ref.ID] != ref.Revision {
			t.Fatalf("builtin review skill ref = %+v", ref)
		}
		delete(wantRevisions, ref.ID)
	}
	if len(wantRevisions) != 0 {
		t.Fatalf("missing builtin review skills = %+v", wantRevisions)
	}
	var runtimeManifest piexecution.LocalRuntimeFileManifest
	var modelProfile contractsv1alpha1.ModelProfile
	for _, component := range bootstrap.Components {
		if component.Ref.ID == bootstrap.Manifest.Runtime.ID &&
			component.Ref.Revision == bootstrap.Manifest.Runtime.Revision &&
			component.Ref.SHA256 == bootstrap.Manifest.Runtime.SHA256 {
			if err := json.Unmarshal(component.Content, &runtimeManifest); err != nil {
				t.Fatal(err)
			}
		}
		if component.Ref.ID == bootstrap.Manifest.Model.ID &&
			component.Ref.Revision == bootstrap.Manifest.Model.Revision &&
			component.Ref.SHA256 == bootstrap.Manifest.Model.SHA256 {
			modelProfile, err = contractsv1alpha1.DecodeModelProfile(component.Content)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if runtimeManifest.Authority != piexecution.LocalRuntimeFileManifestAuthority ||
		len(runtimeManifest.Files) == 0 || len(runtimeManifest.Packages) == 0 {
		t.Fatalf("local runtime file manifest = %+v", runtimeManifest)
	}
	if modelProfile.ProviderID != "deepseek-anthropic" ||
		modelProfile.WireModel != "deepseek-v4-pro[1m]" {
		t.Fatalf("model profile did not preserve the exact wire contract: %+v", modelProfile)
	}
}

func TestBuildLocalPiBootstrapRejectsConcurrencyAboveGroupBudgetBeforeRuntimeInspection(t *testing.T) {
	_, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath:        "/definitely/missing/node",
		WorkerScript:    "/definitely/missing/worker.js",
		ProviderProfile: "deepseek-anthropic-env",
		Model:           "deepseek-chat",
		MaxGroups:       1,
		MaxConcurrency:  2,
	})
	if err == nil || !strings.Contains(err.Error(), "max_concurrency 2 must not exceed max_groups 1") {
		t.Fatalf("BuildLocalPiBootstrap() error = %v, want budget validation", err)
	}
}

func TestBuildLocalPiBootstrapVariantUsesGovernedKnowledgeBytesWithoutVariantPath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	knowledgePath := filepath.Join(t.TempDir(), "payments-domain.md")
	if err := os.WriteFile(knowledgePath, []byte("baseline payment state knowledge"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
		KnowledgePaths: []string{knowledgePath},
	}
	baseline, err := BuildLocalPiBootstrap(options)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("variant payment state knowledge with exact refund invariants")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	ref := contractsv1alpha1.VersionedRef{
		ID: "payments-domain", Revision: "platform-knowledge-v2", SHA256: sha,
	}
	variant := LocalPiComponentVariant{
		SchemaVersion: LocalPiComponentVariantSchemaVersion,
		Variable:      runmodel.ReplayVariableKnowledgePack,
		Components: []LocalPiGovernedComponent{{
			Contract: contractsv1alpha1.AgentStagePlanKnowledgeContract,
			Binding: contractsv1alpha1.AgentStageComponentBinding{
				Ref: ref,
				Artifact: contractsv1alpha1.ArtifactBinding{
					Ref: contractsv1alpha1.ContentRef{
						URI: "artifact://local/sha256/" + sha, SHA256: sha,
						SizeBytes: int64(len(content)),
					},
					Contract: contractsv1alpha1.AgentStagePlanKnowledgeContract,
				},
			},
			Content: content,
		}},
	}
	selected, err := BuildLocalPiBootstrapVariant(options, variant)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Manifest.Knowledge) != 1 || selected.Manifest.Knowledge[0] != ref ||
		selected.Manifest.BuildIdentity == baseline.Manifest.BuildIdentity ||
		selected.Revision.Patch.AgentReview == nil ||
		selected.Revision.Patch.AgentReview.KnowledgePacks == nil ||
		selected.Revision.Patch.AgentReview.KnowledgePacks.Upsert[0].Ref != (reviewconfig.VersionedRef{
			ID: ref.ID, Revision: ref.Revision, SHA256: ref.SHA256,
		}) {
		t.Fatalf("governed knowledge bootstrap=%+v revision=%+v", selected.Manifest, selected.Revision)
	}
	if baseline.Manifest.Knowledge[0].SHA256 == selected.Manifest.Knowledge[0].SHA256 {
		t.Fatal("governed knowledge variant did not change exact content identity")
	}
}

func TestBuildLocalPiBootstrapPublishesExactCustomReviewSkill(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(t.TempDir(), "business-correctness.md")
	skillContent := []byte("# Business correctness\n\nFind violations of the governed business invariant.\n")
	if err := os.WriteFile(skillPath, skillContent, 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
		ReviewSkillPaths: []string{skillPath},
	})
	if err != nil {
		t.Fatalf("BuildLocalPiBootstrap(custom skill) error = %v", err)
	}
	if len(bootstrap.Manifest.ReviewSkills) != len(builtinReviewSkillIDs)+1 ||
		bootstrap.Revision.Patch.AgentReview == nil ||
		bootstrap.Revision.Patch.AgentReview.SkillPacks == nil {
		t.Fatalf("custom review skill closure = %+v", bootstrap)
	}
	definitions := bootstrap.Revision.Patch.AgentReview.SkillPacks.Upsert
	found := false
	for _, definition := range definitions {
		if definition.Phase == reviewconfig.AgentSkillPhaseReview &&
			definition.Ref.SHA256 == digestBytes(skillContent) {
			found = true
		}
	}
	if !found {
		t.Fatalf("custom review skill missing from governed config: %+v", definitions)
	}
}

func TestBuildLocalPiBootstrapPublishesExactGovernedKnowledge(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	knowledgePath := filepath.Join(t.TempDir(), "repository-invariants.md")
	knowledgeContent := []byte("# Repository invariants\n\nOwnership must survive every state transition.\n")
	if err := os.WriteFile(knowledgePath, knowledgeContent, 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
		KnowledgePaths: []string{knowledgePath},
	})
	if err != nil {
		t.Fatalf("BuildLocalPiBootstrap(knowledge) error = %v", err)
	}
	if len(bootstrap.Manifest.Knowledge) != 1 ||
		bootstrap.Manifest.Knowledge[0].ID != "repository-invariants" ||
		bootstrap.Manifest.Knowledge[0].SHA256 != digestBytes(knowledgeContent) ||
		bootstrap.Revision.Patch.AgentReview == nil ||
		bootstrap.Revision.Patch.AgentReview.KnowledgePacks == nil ||
		len(bootstrap.Revision.Patch.AgentReview.KnowledgePacks.Upsert) != 1 {
		t.Fatalf("governed knowledge closure = %+v", bootstrap)
	}
	found := false
	for _, component := range bootstrap.Components {
		if contractRef(component.Ref) == bootstrap.Manifest.Knowledge[0] &&
			component.Contract == contractsv1alpha1.AgentStagePlanKnowledgeContract &&
			bytes.Equal(component.Content, knowledgeContent) {
			found = true
		}
	}
	if !found {
		t.Fatalf("exact governed knowledge component missing: %+v", bootstrap.Components)
	}
}

func TestBuildModelReplayConfigBindsOnlyGovernedModelComponent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineProfile, err := reviewconfig.SealCalibrationProfile(
		"evaluation-profile", "baseline-v1",
		[]reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 1_000_000}},
	)
	if err != nil {
		t.Fatal(err)
	}
	baselineMinimum, baselineMaximum := uint32(500_000), 8
	base.Revision.Patch.FindingGovernance = &reviewconfig.FindingGovernancePatch{
		CalibrationProfile: &baselineProfile, MinimumConfidencePPM: &baselineMinimum,
		MaxFindings: &baselineMaximum,
	}
	baseline, err := reviewconfig.Resolve(reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "repository-1",
		Path: "review.go", InvocationID: "formal-model-source",
	}, []reviewconfig.Revision{base.Revision})
	if err != nil {
		t.Fatal(err)
	}
	revisionDigest, err := reviewconfig.DigestRevision(base.Revision)
	if err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := reviewconfig.NewConfigResolutionReceipt(
		baseline,
		[]reviewconfig.PublishedRevisionBinding{{
			Source: baseline.AppliedRevisions[0], RevisionSHA256: revisionDigest,
			PublishEventID: "publish-model-source", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: formalDigest,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-reasoner",
	})
	if err != nil {
		t.Fatal(err)
	}
	model := reviewconfig.VersionedRef{
		ID: selected.Manifest.Model.ID, Revision: selected.Manifest.Model.Revision,
		SHA256: selected.Manifest.Model.SHA256,
	}
	variant, receipt, err := BuildModelReplayConfig(baseline, sourceReceipt, model)
	if err != nil {
		t.Fatal(err)
	}
	if variant.AgentReview == nil || variant.AgentReview.Model != model ||
		receipt.ReplayVariant == nil || receipt.ReplayVariant.Variable != "model" ||
		!slices.Equal(receipt.ReplayVariant.ChangedFields, FormalModelReplayChangedFields()) {
		t.Fatalf("model variant=%+v receipt=%+v", variant.AgentReview, receipt)
	}
	if err := validateFormalModelVariant(baseline, variant, receipt); err != nil {
		t.Fatal(err)
	}
	tampered := receipt
	binding := *receipt.ReplayVariant
	binding.Variable = "budget"
	tampered.ReplayVariant = &binding
	tampered.ReceiptID, tampered.SHA256 = "", ""
	digest, err := reviewconfig.DigestConfigResolutionReceipt(tampered)
	if err != nil {
		t.Fatal(err)
	}
	tampered.SHA256 = digest
	tampered.ReceiptID = "config-resolution-" + digest[:24]
	if err := validateFormalModelVariant(baseline, variant, tampered); err == nil ||
		!strings.Contains(err.Error(), "atomic change") {
		t.Fatalf("tampered model receipt error=%v", err)
	}
}

func TestBuildFilterPolicyReplayConfigBindsOnlyFindingGovernance(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineProfile, err := reviewconfig.SealCalibrationProfile(
		"evaluation-profile", "baseline-v1",
		[]reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 1_000_000}},
	)
	if err != nil {
		t.Fatal(err)
	}
	baselineMinimum, baselineMaximum := uint32(500_000), 8
	base.Revision.Patch.FindingGovernance = &reviewconfig.FindingGovernancePatch{
		CalibrationProfile: &baselineProfile, MinimumConfidencePPM: &baselineMinimum,
		MaxFindings: &baselineMaximum,
	}
	baseline, err := reviewconfig.Resolve(reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "repository-1",
		Path: "review.go", InvocationID: "formal-filter-source",
	}, []reviewconfig.Revision{base.Revision})
	if err != nil {
		t.Fatal(err)
	}
	revisionDigest, err := reviewconfig.DigestRevision(base.Revision)
	if err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := reviewconfig.NewConfigResolutionReceipt(
		baseline,
		[]reviewconfig.PublishedRevisionBinding{{
			Source: baseline.AppliedRevisions[0], RevisionSHA256: revisionDigest,
			PublishEventID: "publish-filter-source", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: formalDigest,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := reviewconfig.SealCalibrationProfile(
		"evaluation-profile", "v1",
		[]reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 900_000}},
	)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := reviewconfig.SealFindingGovernancePolicy(profile, 600_000, 3)
	if err != nil {
		t.Fatal(err)
	}
	variant, receipt, err := BuildFilterPolicyReplayConfig(baseline, sourceReceipt, policy)
	if err != nil {
		t.Fatal(err)
	}
	if variant.FindingGovernance == nil || !reflect.DeepEqual(*variant.FindingGovernance, policy) ||
		receipt.ReplayVariant == nil ||
		receipt.ReplayVariant.Variable != string(runmodel.ReplayVariableFilterPolicy) ||
		!slices.Equal(receipt.ReplayVariant.ChangedFields, FormalFilterPolicyReplayChangedFields()) {
		t.Fatalf("filter variant=%+v receipt=%+v", variant.FindingGovernance, receipt)
	}
	if err := validateFormalFilterPolicyVariantForVariable(
		baseline, variant, receipt, runmodel.ReplayVariableFilterPolicy,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildFilterPolicyReplayConfig(variant, receipt, policy); err == nil ||
		!strings.Contains(err.Error(), "requires one changed") {
		t.Fatalf("unchanged filter policy error=%v", err)
	}
	legacyVariant, legacyReceipt, err := BuildFindingGovernanceReplayConfig(
		baseline, sourceReceipt, policy,
	)
	if err != nil || legacyVariant.SHA256 != variant.SHA256 ||
		legacyReceipt.ReplayVariant == nil ||
		legacyReceipt.ReplayVariant.Variable != string(runmodel.ReplayVariableFindingGovernance) {
		t.Fatalf("legacy filter compatibility variant=%+v receipt=%+v error=%v",
			legacyVariant, legacyReceipt, err)
	}
	if err := validateFormalFilterPolicyVariant(baseline, legacyVariant, legacyReceipt); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRulePackReplayConfigBindsOnlySealedRulePack(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := reviewconfig.Resolve(reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "repository-1",
		Path: "review.go", InvocationID: "formal-rule-source",
	}, []reviewconfig.Revision{base.Revision})
	if err != nil {
		t.Fatal(err)
	}
	revisionDigest, err := reviewconfig.DigestRevision(base.Revision)
	if err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := reviewconfig.NewConfigResolutionReceipt(
		baseline,
		[]reviewconfig.PublishedRevisionBinding{{
			Source: baseline.AppliedRevisions[0], RevisionSHA256: revisionDigest,
			PublishEventID: "publish-rule-source", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: formalDigest,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := reviewconfig.SealRulePack("agent-review-rules", "2", []reviewconfig.RuleDefinition{{
		ID: "ownership-transition", Revision: "1", Kind: "agent",
		Detector: reviewconfig.VersionedRef{
			ID: "pi-review", Revision: "1", SHA256: strings.Repeat("a", 64),
		},
		Languages: []string{"go"}, PathPrefixes: []string{},
		EvidenceKinds: []string{"file_content", "target_line"},
		Severity:      "high", Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	variant, receipt, err := BuildRulePackReplayConfig(baseline, sourceReceipt, pack)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(variant.RulePack, pack) || receipt.ReplayVariant == nil ||
		receipt.ReplayVariant.Variable != string(runmodel.ReplayVariableRulePack) ||
		!slices.Equal(receipt.ReplayVariant.ChangedFields, FormalRulePackReplayChangedFields()) {
		t.Fatalf("rule variant=%+v receipt=%+v", variant.RulePack, receipt)
	}
	if err := validateFormalRulePackVariant(baseline, variant, receipt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildRulePackReplayConfig(variant, receipt, pack); err == nil ||
		!strings.Contains(err.Error(), "requires one changed") {
		t.Fatalf("unchanged rule pack error=%v", err)
	}
}

func TestBuildWorkflowReplayConfigBindsEffectiveStageBudgetOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := reviewconfig.Resolve(reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "repository-1",
		Path: "review.go", InvocationID: "formal-workflow-source",
	}, []reviewconfig.Revision{base.Revision})
	if err != nil {
		t.Fatal(err)
	}
	revisionDigest, err := reviewconfig.DigestRevision(base.Revision)
	if err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := reviewconfig.NewConfigResolutionReceipt(
		baseline,
		[]reviewconfig.PublishedRevisionBinding{{
			Source: baseline.AppliedRevisions[0], RevisionSHA256: revisionDigest,
			PublishEventID: "publish-workflow-source", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 27, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: formalDigest,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	baselineDefinition := workflow.FormalAgentReviewDefinition()
	variantDefinition := baselineDefinition
	variantDefinition.Revision = "workflow-budget-2"
	variantDefinition.Stages = slices.Clone(baselineDefinition.Stages)
	variantDefinition.Stages[0].Budget.TimeoutMS = baseline.AgentReview.Budget.TimeoutMS / 2
	variant, receipt, fields, err := BuildWorkflowReplayConfig(
		baseline, sourceReceipt, baselineDefinition, variantDefinition,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantFields := []string{"workflow.stages.agent_hypothesize.budget.timeout_ms"}
	if !slices.Equal(fields, wantFields) || receipt.ReplayVariant == nil ||
		receipt.ReplayVariant.Variable != string(runmodel.ReplayVariableWorkflow) ||
		!slices.Equal(receipt.ReplayVariant.ChangedFields, wantFields) {
		t.Fatalf("workflow fields=%v receipt=%+v", fields, receipt)
	}
	digest, err := workflow.DigestDefinition(variantDefinition)
	if err != nil {
		t.Fatal(err)
	}
	if variant.Workflow.Definition != (reviewconfig.VersionedRef{
		ID: variantDefinition.ID, Revision: variantDefinition.Revision, SHA256: digest,
	}) {
		t.Fatalf("workflow config binding=%+v", variant.Workflow.Definition)
	}
	identityOnly := baselineDefinition
	identityOnly.Revision = "identity-only"
	if _, _, _, err := BuildWorkflowReplayConfig(
		baseline, sourceReceipt, baselineDefinition, identityOnly,
	); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("identity-only workflow error=%v", err)
	}
	structural := variantDefinition
	structural.Stages = slices.Clone(variantDefinition.Stages)
	structural.Stages[0].Executor = "untrusted-executor"
	if _, _, _, err := BuildWorkflowReplayConfig(
		baseline, sourceReceipt, baselineDefinition, structural,
	); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("structural workflow error=%v", err)
	}
}

func TestBuildPromptReplayConfigBindsOnlyOperationalPromptComponent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := reviewconfig.Resolve(reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "repository-1",
		Path: "review.go", InvocationID: "formal-prompt-source",
	}, []reviewconfig.Revision{base.Revision})
	if err != nil {
		t.Fatal(err)
	}
	revisionDigest, err := reviewconfig.DigestRevision(base.Revision)
	if err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := reviewconfig.NewConfigResolutionReceipt(
		baseline,
		[]reviewconfig.PublishedRevisionBinding{{
			Source: baseline.AppliedRevisions[0], RevisionSHA256: revisionDigest,
			PublishEventID: "publish-prompt-source", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: formalDigest,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	prompt := contractsv1alpha1.DefaultAgentReviewPromptBundle()
	prompt.Revision = "experiment-v1"
	prompt.ReviewSystemPrompt = "ARGUS_PROMPT_VARIANT: prioritize concrete state-transition defects."
	promptData, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(t.TempDir(), "prompt.json")
	if err := os.WriteFile(promptPath, promptData, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker, PromptBundlePath: promptPath,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	promptRef := reviewconfig.VersionedRef{
		ID: selected.Manifest.Prompt.ID, Revision: selected.Manifest.Prompt.Revision,
		SHA256: selected.Manifest.Prompt.SHA256,
	}
	variant, receipt, err := BuildPromptReplayConfig(baseline, sourceReceipt, promptRef)
	if err != nil {
		t.Fatal(err)
	}
	if variant.AgentReview == nil || variant.AgentReview.Prompt != promptRef ||
		receipt.ReplayVariant == nil || receipt.ReplayVariant.Variable != "prompt" ||
		!slices.Equal(receipt.ReplayVariant.ChangedFields, FormalPromptReplayChangedFields()) ||
		selected.Manifest.BuildIdentity == base.Manifest.BuildIdentity {
		t.Fatalf("prompt variant=%+v receipt=%+v", variant.AgentReview, receipt)
	}
	if err := validateFormalPromptVariant(baseline, variant, receipt); err != nil {
		t.Fatal(err)
	}
	tampered := receipt
	binding := *receipt.ReplayVariant
	binding.Variable = "model"
	tampered.ReplayVariant = &binding
	tampered.ReceiptID, tampered.SHA256 = "", ""
	digest, err := reviewconfig.DigestConfigResolutionReceipt(tampered)
	if err != nil {
		t.Fatal(err)
	}
	tampered.SHA256 = digest
	tampered.ReceiptID = "config-resolution-" + digest[:24]
	if err := validateFormalPromptVariant(baseline, variant, tampered); err == nil ||
		!strings.Contains(err.Error(), "atomic change") {
		t.Fatalf("tampered prompt receipt error=%v", err)
	}
}

func TestBuildSkillPackReplayConfigBindsOnlyGovernedReviewSkills(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := reviewconfig.Resolve(reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "repository-1",
		Path: "review.go", InvocationID: "formal-skill-source",
	}, []reviewconfig.Revision{base.Revision})
	if err != nil {
		t.Fatal(err)
	}
	revisionDigest, err := reviewconfig.DigestRevision(base.Revision)
	if err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := reviewconfig.NewConfigResolutionReceipt(
		baseline,
		[]reviewconfig.PublishedRevisionBinding{{
			Source: baseline.AppliedRevisions[0], RevisionSHA256: revisionDigest,
			PublishEventID: "publish-skill-source", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: formalDigest,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(t.TempDir(), "correctness.md")
	if err := os.WriteFile(
		skillPath,
		[]byte("# Correctness\n\nFind invalid governed state transitions.\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	selected, err := BuildLocalPiBootstrap(LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker, ReviewSkillPaths: []string{skillPath},
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	skills := selected.Revision.Patch.AgentReview.SkillPacks.Upsert
	variant, receipt, err := BuildSkillPackReplayConfig(baseline, sourceReceipt, skills)
	if err != nil {
		t.Fatal(err)
	}
	if variant.AgentReview == nil ||
		receipt.ReplayVariant == nil ||
		receipt.ReplayVariant.Variable != string(runmodel.ReplayVariableSkillPack) ||
		!slices.Equal(receipt.ReplayVariant.ChangedFields, FormalSkillPackReplayChangedFields()) ||
		selected.Manifest.BuildIdentity == base.Manifest.BuildIdentity {
		t.Fatalf("skill variant=%+v receipt=%+v", variant.AgentReview, receipt)
	}
	if err := validateFormalSkillPackVariant(baseline, variant, receipt); err != nil {
		t.Fatal(err)
	}
}

func TestInitializerRecoversFrozenRunBeforeConfigResolution(t *testing.T) {
	command := InitializeCommand{
		SourceRunID: "source-run", IdempotencyKey: "request-key",
		BuildIdentity: "build-1", RuntimeProfile: "node-runtime@binary",
		RuntimeEvidenceRefs: []runmodel.ArtifactRef{
			formalArtifact("artifact://local/sha256/"+formalDigest, runmodel.ContractLocalRuntimeFileManifest),
		},
	}
	runID := FormalRunID(command.SourceRunID, command.IdempotencyKey)
	sourceRef := formalArtifact("artifact://local/source-run", runmodel.ContractReviewRun)
	spec := formalReviewSpec(runID)
	specRef := formalArtifact("artifact://local/formal-spec", runmodel.ContractReviewSpec)
	snapshot := runmodel.ExecutionSnapshot{
		ExecutionSnapshotID: runID + "-snapshot", ReviewSpecRef: specRef,
		TargetSnapshotRef:   formalArtifact("artifact://local/target", runmodel.ContractMaterializedTarget),
		ReviewInputRef:      formalArtifact("artifact://local/input", runmodel.ContractReviewInput),
		BuildIdentity:       command.BuildIdentity,
		RuntimeProfile:      command.RuntimeProfile,
		RuntimeEvidenceRefs: command.RuntimeEvidenceRefs,
	}
	repository := &formalRunRepositoryStub{
		sourceRef: sourceRef,
		source: runmodel.ExecutionSnapshot{
			ExecutionSnapshotID: "source-snapshot",
			TargetSnapshotRef:   snapshot.TargetSnapshotRef,
			ReviewInputRef:      snapshot.ReviewInputRef,
		},
		existing: snapshot, spec: spec,
	}
	provider := &formalConfigProviderStub{}
	initializer, err := NewInitializer(
		repository, provider, workflow.FormalAgentReviewDefinition(),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initializer.Initialize(
		t.Context(),
		application.AgentPlanningSubject{
			TenantID: "local", OrganizationID: "local", WorkspaceID: "local",
			RepositoryID: "repository-1",
		},
		command,
	)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if !result.Recovered || result.RunID != runID || provider.calls != 0 ||
		repository.writes != 0 {
		t.Fatalf("recovered result=%+v provider_calls=%d writes=%d", result, provider.calls, repository.writes)
	}
}

func TestInitializerRejectsRecoveredRunWithChangedRuntimeBinding(t *testing.T) {
	command := InitializeCommand{
		SourceRunID: "source-run", IdempotencyKey: "request-key",
		BuildIdentity: "build-1", RuntimeProfile: "node-runtime@binary",
		RuntimeEvidenceRefs: []runmodel.ArtifactRef{
			formalArtifact("artifact://local/sha256/"+formalDigest, runmodel.ContractLocalRuntimeFileManifest),
		},
	}
	runID := FormalRunID(command.SourceRunID, command.IdempotencyKey)
	sourceRef := formalArtifact("artifact://local/source-run", runmodel.ContractReviewRun)
	repository := &formalRunRepositoryStub{
		sourceRef: sourceRef,
		source: runmodel.ExecutionSnapshot{
			ExecutionSnapshotID: "source-snapshot",
			TargetSnapshotRef: formalArtifact(
				"artifact://local/target", runmodel.ContractMaterializedTarget,
			),
			ReviewInputRef: formalArtifact(
				"artifact://local/input", runmodel.ContractReviewInput,
			),
		},
		existing: runmodel.ExecutionSnapshot{
			ExecutionSnapshotID: runID + "-snapshot",
			TargetSnapshotRef: formalArtifact(
				"artifact://local/target", runmodel.ContractMaterializedTarget,
			),
			ReviewInputRef: formalArtifact(
				"artifact://local/input", runmodel.ContractReviewInput,
			),
			BuildIdentity: "another-build", RuntimeProfile: command.RuntimeProfile,
		},
		spec: formalReviewSpec(runID),
	}
	initializer, err := NewInitializer(
		repository, &formalConfigProviderStub{}, workflow.FormalAgentReviewDefinition(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = initializer.Initialize(
		t.Context(),
		application.AgentPlanningSubject{
			TenantID: "local", OrganizationID: "local", WorkspaceID: "local",
			RepositoryID: "repository-1",
		},
		command,
	)
	if err == nil {
		t.Fatal("Initialize() accepted changed runtime binding")
	}
}

type formalRunRepositoryStub struct {
	sourceRef runmodel.ArtifactRef
	source    runmodel.ExecutionSnapshot
	existing  runmodel.ExecutionSnapshot
	spec      contractsv1alpha1.ReviewSpec
	writes    int
}

func (stub *formalRunRepositoryStub) CommittedRunRef(string) (runmodel.ArtifactRef, error) {
	return stub.sourceRef, nil
}

func (stub *formalRunRepositoryStub) ExecutionSnapshotForRun(
	runID string,
) (runmodel.ExecutionSnapshot, error) {
	if runID == "source-run" {
		return stub.source, nil
	}
	if stub.existing.ExecutionSnapshotID == "" {
		return runmodel.ExecutionSnapshot{}, errors.New("not found")
	}
	return stub.existing, nil
}

func (stub *formalRunRepositoryStub) LoadRun(string) (runmodel.ReviewRun, error) {
	return runmodel.ReviewRun{}, errors.New("unexpected LoadRun")
}

func (stub *formalRunRepositoryStub) ReadJSONArtifact(_ runmodel.ArtifactRef, out any) error {
	pointer, ok := out.(*contractsv1alpha1.ReviewSpec)
	if !ok {
		return errors.New("unexpected artifact type")
	}
	*pointer = stub.spec
	return nil
}

func (stub *formalRunRepositoryStub) PutJSONArtifact(
	string,
	any,
) (runmodel.ArtifactRef, error) {
	stub.writes++
	return runmodel.ArtifactRef{}, errors.New("unexpected PutJSONArtifact")
}

func (stub *formalRunRepositoryStub) SaveExecutionSnapshot(runmodel.ExecutionSnapshot) error {
	stub.writes++
	return errors.New("unexpected SaveExecutionSnapshot")
}

func (stub *formalRunRepositoryStub) AppendEvent(
	string,
	time.Time,
	runrepo.RunEvent,
) error {
	stub.writes++
	return errors.New("unexpected AppendEvent")
}

type formalConfigProviderStub struct{ calls int }

func (stub *formalConfigProviderStub) ResolvePublished(
	context.Context,
	reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, error) {
	stub.calls++
	return reviewconfig.ConfigBundle{}, errors.New("unexpected config resolution")
}

func (stub *formalConfigProviderStub) ResolvePublishedWithReceipt(
	context.Context,
	reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	stub.calls++
	return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
		errors.New("unexpected config resolution")
}

func formalReviewSpec(runID string) contractsv1alpha1.ReviewSpec {
	return contractsv1alpha1.ReviewSpec{
		SchemaVersion: contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:     runID, IdempotencyKey: runID,
		TenantID: "local", WorkspaceID: "local",
		Repository: contractsv1alpha1.RepositoryRef{
			Provider: "local-git", RepositoryID: "repository-1",
		},
		Target: contractsv1alpha1.ReviewTarget{
			Mode: contractsv1alpha1.ReviewModeDiff,
			Diff: &contractsv1alpha1.DiffTarget{
				BaseRevision: "base", HeadRevision: "head",
				Patch: contractsv1alpha1.ContentRef{
					URI: "artifact://local/patch", SHA256: formalDigest,
					SizeBytes: 1,
				},
			},
		},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID: "bundle", Revision: "1", SHA256: formalDigest,
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: "formal-pi-agent-review", Revision: "1", SHA256: formalDigest,
		},
		RequestedOutput: []string{"findings", "report", "trace"},
		RemoteWrites:    "deny",
	}
}

const formalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func formalArtifact(uri string, contract string) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI: uri, SHA256: formalDigest, SizeBytes: 1, Contract: contract,
	}
}
