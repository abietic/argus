package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/workflow"
)

func TestServiceReplayFreezesSingleRulePackVariantAndNamespace(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceBundle := loadRunBundle(t, repository, original.Run)
	variant := sourceBundle
	rules := append([]reviewconfig.RuleDefinition(nil), sourceBundle.RulePack.Rules...)
	for index := range rules {
		if rules[index].ID == reviewcore.RuleExplicitBugMarker {
			rules[index].Enabled = false
		}
	}
	variant.RulePack, err = reviewconfig.SealRulePack(
		sourceBundle.RulePack.ID,
		"variant-1",
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	resealConfigBundle(t, &variant)

	replayed, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID:         original.Run.RunID,
		StartStage:          string(reviewcore.StageDetect),
		Variable:            runmodel.ReplayVariableRulePack,
		VariantConfigBundle: &variant,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Run.ReplayRootRunID != original.Run.RunID ||
		replayed.Run.ReplayNamespace != replayed.Run.RunID ||
		replayed.Run.ReplayVariable != runmodel.ReplayVariableRulePack {
		t.Fatalf("replay lineage = %+v", replayed.Run)
	}
	if replayed.Report == nil || len(replayed.Report.Findings) != 0 {
		t.Fatalf("variant report = %+v, want no explicit-bug finding", replayed.Report)
	}
	snapshot, err := repository.LoadExecutionSnapshot(replayed.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ReplayChangeSetRef == nil {
		t.Fatal("variant snapshot has no replay change set")
	}
	var change runmodel.ReplayChangeSet
	if err := repository.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		t.Fatal(err)
	}
	if err := change.Validate(); err != nil {
		t.Fatal(err)
	}
	if change.Variable != runmodel.ReplayVariableRulePack ||
		!strings.Contains(strings.Join(change.ChangedFields, ","), "rule_pack") ||
		change.BaselineSHA256 != sourceBundle.SHA256 ||
		change.VariantSHA256 != variant.SHA256 {
		t.Fatalf("change set = %+v", change)
	}
	comparison, err := service.Compare(
		context.Background(), original.Run.RunID, replayed.Run.RunID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(comparison.Removed) != 1 || len(comparison.Added) != 0 {
		t.Fatalf("comparison = %+v", comparison)
	}
}

func TestServiceReplayRejectsReusedAffectedCheckpointAndMultiVariableChange(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := loadRunBundle(t, repository, original.Run)
	variant := source
	rules := append([]reviewconfig.RuleDefinition(nil), source.RulePack.Rules...)
	for index := range rules {
		if rules[index].ID == reviewcore.RuleExplicitBugMarker {
			rules[index].Enabled = false
		}
	}
	variant.RulePack, err = reviewconfig.SealRulePack(
		source.RulePack.ID, "variant-1", rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	resealConfigBundle(t, &variant)
	_, err = service.Replay(context.Background(), ReplayRequest{
		SourceRunID:         original.Run.RunID,
		StartStage:          string(reviewcore.StageVerify),
		Variable:            runmodel.ReplayVariableRulePack,
		VariantConfigBundle: &variant,
	})
	if err == nil || !strings.Contains(err.Error(), "affected checkpoints cannot be reused") {
		t.Fatalf("Replay error = %v", err)
	}

	variant.Verification.MinimumIndependentEvidence++
	resealConfigBundle(t, &variant)
	_, err = service.Replay(context.Background(), ReplayRequest{
		SourceRunID:         original.Run.RunID,
		StartStage:          string(reviewcore.StageDetect),
		Variable:            runmodel.ReplayVariableRulePack,
		VariantConfigBundle: &variant,
	})
	if err == nil || !strings.Contains(err.Error(), "more than one atomic variable") {
		t.Fatalf("Replay multi-variable error = %v", err)
	}
}

func TestServiceReplayExecutesExactIndexVariantAndFreezesNewContextEvidence(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	providerCalls := 0
	service, repository := newTestService(t, ServiceOptions{
		ContextProviders: contextProviderExecutorFunc(func(
			_ context.Context,
			request ContextProviderExecutionRequest,
		) (ContextProviderCapture, error) {
			providerCalls++
			return ContextProviderCapture{
				Contract: "argus.context.fixture.v1alpha1",
				Content: []byte(fmt.Sprintf(
					`{"provider":%q,"revision":%q}`,
					request.Definition.ID,
					request.Definition.Revision,
				)),
				Revision: request.CommitOID,
				Coverage: reviewcore.ContextCoverage{
					Spans: []reviewcore.ContextSpan{{
						Path: "review.go", StartLine: 1, EndLine: 1,
					}},
					Symbols: []string{},
				},
			}, nil
		}),
	})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
		ContextProviderIDs: []string{"go_ast"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceSnapshot, err := repository.LoadExecutionSnapshot(original.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	source := loadRunBundle(t, repository, original.Run)
	variant := source
	variant.Execution.ContextProviders = slices.Clone(source.Execution.ContextProviders)
	variant.Execution.ContextProviders[0].Revision = "2"
	resealConfigBundle(t, &variant)

	replayed, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID:         original.Run.RunID,
		StartStage:          string(reviewcore.StageMaterializeTarget),
		Variable:            runmodel.ReplayVariableIndex,
		VariantConfigBundle: &variant,
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls != 2 || len(replayed.Reused) != 0 ||
		replayed.Run.ReplayVariable != runmodel.ReplayVariableIndex {
		t.Fatalf("index replay execution = calls %d, reused %v, run %+v", providerCalls, replayed.Reused, replayed.Run)
	}
	variantSnapshot, err := repository.LoadExecutionSnapshot(replayed.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(
		sourceSnapshot.ContextProviderReceiptRefs,
		variantSnapshot.ContextProviderReceiptRefs,
	) || sourceSnapshot.ReviewInputRef == variantSnapshot.ReviewInputRef ||
		sourceSnapshot.TargetSnapshotRef == variantSnapshot.TargetSnapshotRef {
		t.Fatalf("index replay did not freeze variant input evidence: source=%+v variant=%+v", sourceSnapshot, variantSnapshot)
	}
	var change runmodel.ReplayChangeSet
	if variantSnapshot.ReplayChangeSetRef == nil {
		t.Fatal("index replay has no change set")
	}
	if err := repository.ReadJSONArtifact(*variantSnapshot.ReplayChangeSetRef, &change); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(change.ChangedFields, []string{
		"execution.context_providers.go-ast-exact.revision",
	}) {
		t.Fatalf("index replay changed fields = %v", change.ChangedFields)
	}
	if _, err := repository.LoadRun(replayed.Run.RunID); err != nil {
		t.Fatalf("whole-closure LoadRun() error = %v", err)
	}
	comparison, err := service.Compare(
		context.Background(),
		original.Run.RunID,
		replayed.Run.RunID,
	)
	if err != nil {
		t.Fatalf("Compare(index replay) error = %v", err)
	}
	if len(comparison.Unchanged) != 1 || len(comparison.Added) != 0 ||
		len(comparison.Removed) != 0 || len(comparison.CandidatesUnchanged) != 1 ||
		len(comparison.CandidatesAdded) != 0 || len(comparison.CandidatesRemoved) != 0 {
		t.Fatalf("index replay comparison = %+v", comparison)
	}

	_, err = service.Replay(context.Background(), ReplayRequest{
		SourceRunID:         original.Run.RunID,
		StartStage:          string(reviewcore.StageDetect),
		Variable:            runmodel.ReplayVariableIndex,
		VariantConfigBundle: &variant,
	})
	if err == nil || !strings.Contains(err.Error(), "affected checkpoints cannot be reused") {
		t.Fatalf("late index replay error = %v", err)
	}
}

func TestServiceReplayIndexRequiresProviderExecutorBeforeRunCreation(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := loadRunBundle(t, repository, original.Run)
	variant := source
	variant.Execution.ContextProviders = []reviewconfig.ContextProviderDefinition{{
		ID: "index-provider", Revision: "1", Kind: "go_ast",
		Adapter: reviewconfig.VersionedRef{
			ID: "index-adapter", Revision: "1", SHA256: strings.Repeat("a", 64),
		},
	}}
	resealConfigBundle(t, &variant)

	_, err = service.Replay(context.Background(), ReplayRequest{
		SourceRunID:         original.Run.RunID,
		StartStage:          string(reviewcore.StageMaterializeTarget),
		Variable:            runmodel.ReplayVariableIndex,
		VariantConfigBundle: &variant,
	})
	if err == nil || !strings.Contains(err.Error(), "requires the version-pinned context provider executor") {
		t.Fatalf("missing index executor error = %v", err)
	}
	history, historyErr := service.History(100)
	if historyErr != nil {
		t.Fatal(historyErr)
	}
	if len(history) != 1 || history[0].RunID != original.Run.RunID {
		t.Fatalf("failed index admission created a run: %+v", history)
	}
}

func TestServiceReplayExecutesExactWorkflowVariant(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	detectCalls := 0
	service, repository := newTestService(t, ServiceOptions{
		ExecutorIdentity: "workflow-replay-test-executor",
		BuildIdentity:    "workflow-replay-test-build",
		ExecutorCapabilities: map[string]workflow.ExecutorCapabilities{
			"deterministic-local": {},
		},
		ExecuteStage: func(
			ctx context.Context,
			input reviewcore.ReviewInput,
			stage reviewcore.StageName,
			upstream reviewcore.StageResult,
			policy reviewcore.RuntimePolicy,
		) (reviewcore.StageResult, error) {
			if stage == reviewcore.StageDetect {
				detectCalls++
				if detectCalls == 2 {
					return reviewcore.StageResult{}, &RetryableStageError{
						Code: "temporary", Err: errors.New("variant detector failure"),
					}
				}
			}
			return reviewcore.ExecuteStageWithPolicy(ctx, input, stage, upstream, policy)
		},
	})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := loadRunBundle(t, repository, original.Run)
	variantWorkflow := workflow.DefaultReviewDefinition()
	variantWorkflow.Revision = "workflow-variant-1"
	for index := range variantWorkflow.Stages {
		if variantWorkflow.Stages[index].ID == string(reviewcore.StageDetect) {
			variantWorkflow.Stages[index].Retry.RetryableCodes = []string{"stage_timeout"}
		}
	}
	workflowDigest, err := workflow.DigestDefinition(variantWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	variant := source
	variant.Workflow.Definition = reviewconfig.VersionedRef{
		ID: variantWorkflow.ID, Revision: variantWorkflow.Revision, SHA256: workflowDigest,
	}
	resealConfigBundle(t, &variant)

	replayed, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: original.Run.RunID, StartStage: string(reviewcore.StageDetect),
		Variable: runmodel.ReplayVariableWorkflow, VariantConfigBundle: &variant,
		VariantWorkflow: &variantWorkflow,
	})
	var runErr *RunError
	if !errors.As(err, &runErr) || replayed.Run.Status != runmodel.RunStatusFailed {
		t.Fatalf("Replay(workflow variant) = %+v, %v", replayed, err)
	}
	if detectCalls != 2 || len(replayed.Run.StageAttempts) != 1 ||
		replayed.Run.StageAttempts[0].Retryable {
		t.Fatalf("workflow retryable_codes was not executed: calls=%d attempts=%+v", detectCalls, replayed.Run.StageAttempts)
	}
	snapshot, err := repository.LoadExecutionSnapshot(replayed.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Workflow.ID != variantWorkflow.ID ||
		snapshot.Workflow.Revision != variantWorkflow.Revision ||
		snapshot.Workflow.SHA256 != workflowDigest {
		t.Fatalf("variant workflow was not frozen in snapshot: %+v", snapshot.Workflow)
	}
	workflowBytes, err := repository.ReadArtifact(snapshot.WorkflowDefinitionRef)
	if err != nil {
		t.Fatal(err)
	}
	frozenWorkflow, err := workflow.DecodeDefinition(workflowBytes)
	if err != nil || frozenWorkflow.Revision != variantWorkflow.Revision {
		t.Fatalf("frozen workflow = %+v, %v", frozenWorkflow, err)
	}
	var change runmodel.ReplayChangeSet
	if snapshot.ReplayChangeSetRef == nil {
		t.Fatal("workflow replay snapshot has no change set")
	}
	if err := repository.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		t.Fatal(err)
	}
	if change.Variable != runmodel.ReplayVariableWorkflow ||
		!slices.Contains(change.ChangedFields, "workflow.stages.detect.retry.retryable_codes") {
		t.Fatalf("workflow replay change set = %+v", change)
	}
}

func TestServiceReplayRejectsWorkflowIdentityOnlyAndMissingDefinition(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := loadRunBundle(t, repository, original.Run)
	variantWorkflow := workflow.DefaultReviewDefinition()
	variantWorkflow.Revision = "identity-only-variant"
	digest, err := workflow.DigestDefinition(variantWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	variant := source
	variant.Workflow.Definition = reviewconfig.VersionedRef{
		ID: variantWorkflow.ID, Revision: variantWorkflow.Revision, SHA256: digest,
	}
	resealConfigBundle(t, &variant)

	_, err = service.Replay(context.Background(), ReplayRequest{
		SourceRunID: original.Run.RunID, StartStage: string(reviewcore.StageDetect),
		Variable: runmodel.ReplayVariableWorkflow, VariantConfigBundle: &variant,
	})
	if err == nil || !strings.Contains(err.Error(), "requires one exact variant WorkflowDefinition") {
		t.Fatalf("missing workflow definition error = %v", err)
	}
	_, err = service.Replay(context.Background(), ReplayRequest{
		SourceRunID: original.Run.RunID, StartStage: string(reviewcore.StageDetect),
		Variable: runmodel.ReplayVariableWorkflow, VariantConfigBundle: &variant,
		VariantWorkflow: &variantWorkflow,
	})
	if err == nil || !strings.Contains(err.Error(), "only identity") {
		t.Fatalf("identity-only workflow replay error = %v", err)
	}
}

func TestServiceReplayRejectsWorkflowInactiveCeilingOnly(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := loadRunBundle(t, repository, original.Run)
	variantWorkflow := workflow.DefaultReviewDefinition()
	variantWorkflow.Revision = "inactive-ceiling-variant"
	variantWorkflow.Stages[0].Budget.TimeoutMS++
	digest, err := workflow.DigestDefinition(variantWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	variant := source
	variant.Workflow.Definition = reviewconfig.VersionedRef{
		ID: variantWorkflow.ID, Revision: variantWorkflow.Revision, SHA256: digest,
	}
	resealConfigBundle(t, &variant)

	_, err = service.Replay(context.Background(), ReplayRequest{
		SourceRunID: original.Run.RunID, StartStage: string(reviewcore.StagePlanContext),
		Variable: runmodel.ReplayVariableWorkflow, VariantConfigBundle: &variant,
		VariantWorkflow: &variantWorkflow,
	})
	if err == nil || !strings.Contains(err.Error(), "does not change effective scheduler behavior") {
		t.Fatalf("inactive workflow ceiling error = %v", err)
	}
}

func TestServiceExactReplayFreezesNoneChangeAndChainedRoot(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: original.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: first.Run.RunID,
		StartStage:  string(reviewcore.StageReport),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Run.ReplayRootRunID != original.Run.RunID ||
		second.Run.SourceRunID != first.Run.RunID ||
		second.Run.ReplayVariable != runmodel.ReplayVariableNone {
		t.Fatalf("chained replay lineage = %+v", second.Run)
	}
	snapshot, err := repository.LoadExecutionSnapshot(second.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var change runmodel.ReplayChangeSet
	if snapshot.ReplayChangeSetRef == nil {
		t.Fatal("exact replay has no change set")
	}
	if err := repository.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		t.Fatal(err)
	}
	if change.ParentReplayRunID != first.Run.RunID ||
		change.RootRunID != original.Run.RunID ||
		change.BaselineSHA256 != change.VariantSHA256 ||
		len(change.ChangedFields) != 0 {
		t.Fatalf("exact replay change set = %+v", change)
	}
}

func loadRunBundle(
	t *testing.T,
	repository interface {
		LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error)
		ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
	},
	run runmodel.ReviewRun,
) reviewconfig.ConfigBundle {
	t.Helper()
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}
