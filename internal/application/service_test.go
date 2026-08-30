package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestServiceReviewReplayCompareAndHistory(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if outcome.Run.Status != runmodel.RunStatusSucceeded ||
		outcome.Report == nil ||
		outcome.Report.Summary.Publish != 1 ||
		outcome.Report.Summary.Verified != 1 {
		t.Fatalf("review outcome = %+v", outcome)
	}
	if len(outcome.Run.StageAttempts) != 10 ||
		len(outcome.Run.Bindings) != 10 ||
		len(outcome.Run.Evidence) != 10 {
		t.Fatalf(
			"review execution facts = attempts %d bindings %d evidence %d",
			len(outcome.Run.StageAttempts), len(outcome.Run.Bindings), len(outcome.Run.Evidence),
		)
	}
	loaded, err := repository.LoadRun(outcome.Run.RunID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if loaded.JSONReportRef == nil || loaded.MarkdownReportRef == nil ||
		loaded.FindingSetRef == nil {
		t.Fatalf("loaded run is missing output refs: %+v", loaded)
	}
	snapshot, err := repository.LoadExecutionSnapshot(loaded.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	if len(snapshot.ReplayInputRefs) != 0 ||
		snapshot.RemoteWrites != "deny" ||
		snapshot.ToolPolicy.Network != "deny" ||
		snapshot.ToolPolicy.WorkspaceWrites != "deny" {
		t.Fatalf("review snapshot authority = %+v", snapshot)
	}

	replayed, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: outcome.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.Run.Status != runmodel.RunStatusSucceeded ||
		len(replayed.Reused) != 4 ||
		len(replayed.Run.StageAttempts) != 6 ||
		replayed.Report == nil ||
		replayed.Report.Findings[0].Fingerprint != outcome.Report.Findings[0].Fingerprint {
		t.Fatalf("replay outcome = %+v", replayed)
	}
	replaySnapshot, err := repository.LoadExecutionSnapshot(replayed.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("load replay snapshot: %v", err)
	}
	sourceRunRef, err := repository.CommittedRunRef(outcome.Run.RunID)
	if err != nil {
		t.Fatalf("load source run ref: %v", err)
	}
	if len(replaySnapshot.ReplayInputRefs) != 4 ||
		replaySnapshot.ReplaySourceRunRef == nil ||
		*replaySnapshot.ReplaySourceRunRef != sourceRunRef {
		t.Fatalf("replay lineage snapshot = %+v", replaySnapshot)
	}

	comparison, err := service.Compare(
		context.Background(),
		outcome.Run.RunID,
		replayed.Run.RunID,
	)
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	if len(comparison.Added) != 0 || len(comparison.Removed) != 0 ||
		len(comparison.Unchanged) != 1 {
		t.Fatalf("comparison = %+v", comparison)
	}
	history, err := service.History(10)
	if err != nil {
		t.Fatalf("History() error = %v", err)
	}
	if len(history) != 2 ||
		history[0].Status != runmodel.RunStatusSucceeded ||
		history[1].Status != runmodel.RunStatusSucceeded {
		t.Fatalf("history = %+v", history)
	}
}

func TestServiceReplayOfReplayMergesInheritedAndExecutedCheckpoints(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})

	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	firstReplay, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: original.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err != nil {
		t.Fatalf("first Replay() error = %v", err)
	}
	chained, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: firstReplay.Run.RunID,
		StartStage:  string(reviewcore.StageReport),
	})
	if err != nil {
		t.Fatalf("chained Replay() error = %v", err)
	}
	if chained.Run.Status != runmodel.RunStatusSucceeded ||
		len(chained.Reused) != 6 ||
		len(chained.Run.StageAttempts) != 4 {
		t.Fatalf("chained replay outcome = %+v", chained)
	}
	snapshot, err := repository.LoadExecutionSnapshot(chained.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot(chained) error = %v", err)
	}
	wantStages := []reviewcore.StageName{
		reviewcore.StageMaterializeTarget,
		reviewcore.StagePlanContext,
		reviewcore.StageDetect,
		reviewcore.StageNormalize,
		reviewcore.StageVerify,
		reviewcore.StageAdjudicate,
	}
	if len(snapshot.ReplayInputRefs) != len(wantStages) {
		t.Fatalf("chained replay refs = %d, want %d",
			len(snapshot.ReplayInputRefs), len(wantStages))
	}
	seen := make(map[runmodel.ArtifactRef]struct{}, len(snapshot.ReplayInputRefs))
	for index, ref := range snapshot.ReplayInputRefs {
		if _, duplicate := seen[ref]; duplicate {
			t.Fatalf("chained replay ref %d is duplicated: %+v", index, ref)
		}
		seen[ref] = struct{}{}
		data, err := repository.ReadArtifact(ref)
		if err != nil {
			t.Fatalf("ReadArtifact(replay ref %d) error = %v", index, err)
		}
		result, err := reviewcore.DecodeStageResult(data)
		if err != nil {
			t.Fatalf("DecodeStageResult(replay ref %d) error = %v", index, err)
		}
		if result.Stage != wantStages[index] {
			t.Fatalf("replay ref %d stage = %q, want %q",
				index, result.Stage, wantStages[index])
		}
	}
}

func TestServiceReplayRejectsCrossBuildLineage(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{
		BuildIdentity: "argus-build-a",
	})
	original, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatalf("gitadapter.New() error = %v", err)
	}
	otherBuild, err := NewService(source, repository, ServiceOptions{
		IDs:               &sequenceIDs{},
		Now:               time.Now,
		BuildIdentity:     "argus-build-b",
		DisableScheduling: true,
	})
	if err != nil {
		t.Fatalf("NewService(other build) error = %v", err)
	}
	_, err = otherBuild.Replay(context.Background(), ReplayRequest{
		SourceRunID: original.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err == nil || !strings.Contains(err.Error(), "build identity") {
		t.Fatalf("Replay(cross build) error = %v, want build identity rejection", err)
	}
}

func TestServiceAdmissionRejectsBroaderExecutorAuthority(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	service.executorCapabilities["deterministic-local"] = workflow.ExecutorCapabilities{
		RemoteWrite: true,
	}

	_, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err == nil || !strings.Contains(err.Error(), "admit review execution") {
		t.Fatalf("Review() error = %v, want admission rejection", err)
	}
	history, historyErr := repository.History(10)
	if historyErr != nil {
		t.Fatalf("History() error = %v", historyErr)
	}
	if len(history) != 0 {
		t.Fatalf("admission-rejected review created run history: %+v", history)
	}
}

func TestServiceSelectionUsesExactCommitAndReplaysSameTarget(t *testing.T) {
	repositoryPath, _, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		Mode:           reviewcore.TargetModeSelection,
		Revision:       head,
		SelectionPath:  "review.go",
		StartLine:      3,
		EndLine:        3,
	})
	if err != nil {
		t.Fatalf("Review(selection) error = %v", err)
	}
	if outcome.Run.TargetMode != runmodel.TargetModeSelection ||
		outcome.Run.BaseRevision != head || outcome.Run.HeadRevision != head ||
		outcome.Report == nil || outcome.Report.Summary.Verified != 1 ||
		outcome.Report.Findings[0].StartLine != 3 {
		t.Fatalf("selection outcome = %+v", outcome)
	}
	var target MaterializedTarget
	if err := repository.ReadJSONArtifact(outcome.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("read selection target: %v", err)
	}
	if target.Snapshot.Selection == nil ||
		target.Snapshot.Selection.SourceKind != selectionSourceCommit ||
		target.SelectionContentRef == nil || target.PatchRef != nil {
		t.Fatalf("selection target = %+v", target)
	}

	replayed, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: outcome.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err != nil {
		t.Fatalf("Replay(selection) error = %v", err)
	}
	if replayed.Run.TargetMode != runmodel.TargetModeSelection ||
		replayed.Report == nil || replayed.Report.Summary.Verified != 1 {
		t.Fatalf("selection replay = %+v", replayed)
	}
}

func TestServiceSelectionFreezesOverlayInsteadOfCommitContent(t *testing.T) {
	repositoryPath, _, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	overlay := "package fixture\n// TODO overlay-only\nfunc Review() {}\n"

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		Mode:           reviewcore.TargetModeSelection,
		Revision:       head,
		SelectionPath:  "review.go",
		StartLine:      2,
		EndLine:        2,
		OverlayContent: &overlay,
	})
	if err != nil {
		t.Fatalf("Review(selection overlay) error = %v", err)
	}
	if outcome.Report == nil || outcome.Report.Summary.Verified != 1 ||
		outcome.Report.Findings[0].RuleID != "unfinished-work" {
		t.Fatalf("overlay outcome = %+v", outcome)
	}
	var target MaterializedTarget
	if err := repository.ReadJSONArtifact(outcome.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("read overlay target: %v", err)
	}
	if target.Snapshot.Selection.SourceKind != selectionSourceOverlay {
		t.Fatalf("overlay target = %+v", target.Snapshot.Selection)
	}
}

func TestServiceSelectionPersistsMultiRangeClosure(t *testing.T) {
	repositoryPath, _, _ := reviewFixture(t)
	writeFixture(
		t,
		repositoryPath,
		"review.go",
		"package fixture\n// TODO first\nfunc middle() {}\n// ARGUS_BUG second\n",
	)
	revision := commitFixture(t, repositoryPath, "multi-range selection")
	service, repository := newTestService(t, ServiceOptions{})

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		Mode:           reviewcore.TargetModeSelection,
		Revision:       revision,
		SelectionPath:  "review.go",
		SelectionRanges: []SelectionRange{
			{StartLine: 2, EndLine: 2},
			{StartLine: 4, EndLine: 4},
		},
	})
	if err != nil {
		t.Fatalf("Review(multi-range selection) error = %v", err)
	}
	if outcome.Report == nil || len(outcome.Report.Findings) != 2 {
		t.Fatalf("multi-range outcome = %+v", outcome)
	}
	snapshot, err := repository.LoadExecutionSnapshot(outcome.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := repository.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		t.Fatalf("ReadJSONArtifact(ReviewSpec) error = %v", err)
	}
	var target MaterializedTarget
	if err := repository.ReadJSONArtifact(outcome.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("ReadJSONArtifact(MaterializedTarget) error = %v", err)
	}
	var input reviewcore.ReviewInput
	if err := repository.ReadJSONArtifact(snapshot.ReviewInputRef, &input); err != nil {
		t.Fatalf("ReadJSONArtifact(ReviewInput) error = %v", err)
	}
	if spec.Target.Selection == nil ||
		len(spec.Target.Selection.Ranges) != 2 ||
		target.Snapshot.Selection == nil ||
		len(target.Snapshot.Selection.EffectiveRanges) != 2 ||
		len(input.Regions) != 2 {
		t.Fatalf("multi-range closure spec=%+v target=%+v input=%+v",
			spec.Target.Selection, target.Snapshot.Selection, input.Regions)
	}
}

func TestServiceSelectionPersistsResolvedSymbolClosure(t *testing.T) {
	repositoryPath, _, _ := reviewFixture(t)
	writeFixture(
		t,
		repositoryPath,
		"review.go",
		"package fixture\ntype Service struct{}\nfunc (Service) Review() {\n// ARGUS_BUG selected\n}\n",
	)
	revision := commitFixture(t, repositoryPath, "symbol selection")
	service, repository := newTestService(t, ServiceOptions{})

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		Mode:           reviewcore.TargetModeSelection,
		Revision:       revision,
		SelectionPath:  "review.go",
		SelectionSymbol: &SymbolSelector{
			Language: "go", Kind: "method", QualifiedName: "Service.Review",
		},
	})
	if err != nil {
		t.Fatalf("Review(symbol selection) error = %v", err)
	}
	if outcome.Report == nil || len(outcome.Report.Findings) != 1 ||
		outcome.Report.Findings[0].StartLine != 4 {
		t.Fatalf("symbol outcome = %+v", outcome)
	}
	snapshot, err := repository.LoadExecutionSnapshot(outcome.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := repository.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		t.Fatalf("ReadJSONArtifact(ReviewSpec) error = %v", err)
	}
	var target MaterializedTarget
	if err := repository.ReadJSONArtifact(outcome.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("ReadJSONArtifact(MaterializedTarget) error = %v", err)
	}
	if spec.Target.Selection == nil || spec.Target.Selection.Symbol == nil ||
		spec.Target.Selection.Symbol.QualifiedName != "Service.Review" ||
		target.Snapshot.Selection == nil ||
		len(target.Snapshot.Selection.EffectiveRanges) != 1 ||
		target.Snapshot.Selection.EffectiveRanges[0] !=
			(SelectionRange{StartLine: 3, EndLine: 5}) {
		t.Fatalf("symbol closure spec=%+v target=%+v",
			spec.Target.Selection, target.Snapshot.Selection)
	}
}

func TestServiceFreezesContextRefWithoutExpandingSelectionAnchor(t *testing.T) {
	repositoryPath, _, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	contextData := []byte("repository search result")
	contextRef, err := repository.PutArtifact(
		"argus.context.repository_search.v1alpha1",
		contextData,
	)
	if err != nil {
		t.Fatalf("PutArtifact(context) error = %v", err)
	}

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		Mode:           reviewcore.TargetModeSelection,
		Revision:       head,
		SelectionPath:  "review.go",
		StartLine:      3,
		EndLine:        3,
		Contexts: []reviewcore.ContextBinding{{
			Ref: &reviewcore.ContextRef{
				ContextID: "context-search-1",
				Kind:      "repository_search",
				Revision:  head,
				Digest:    contextRef.SHA256,
				Coverage: reviewcore.ContextCoverage{
					Spans: []reviewcore.ContextSpan{{
						Path: "review.go", StartLine: 1, EndLine: 2,
					}},
					Symbols: []string{},
				},
				Provenance: reviewcore.ContextProvenance{
					Provider: "local-test", ProducerID: "fixture",
					ProducerRevision: "1",
				},
				ArtifactURI: contextRef.URI,
				Contract:    contextRef.Contract,
				SizeBytes:   contextRef.SizeBytes,
			},
		}},
	})
	if err != nil {
		t.Fatalf("Review(selection with context) error = %v", err)
	}
	if outcome.Report == nil || len(outcome.Report.Findings) != 1 ||
		outcome.Report.Findings[0].StartLine != 3 {
		t.Fatalf("context-expanded selection outcome = %+v", outcome)
	}
	snapshot, err := repository.LoadExecutionSnapshot(outcome.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	var target MaterializedTarget
	if err := repository.ReadJSONArtifact(outcome.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("ReadJSONArtifact(MaterializedTarget) error = %v", err)
	}
	var input reviewcore.ReviewInput
	if err := repository.ReadJSONArtifact(snapshot.ReviewInputRef, &input); err != nil {
		t.Fatalf("ReadJSONArtifact(ReviewInput) error = %v", err)
	}
	if len(target.Contexts) != 1 || len(input.Contexts) != 1 ||
		target.Contexts[0].Ref == nil || input.Contexts[0].Ref == nil ||
		target.Contexts[0].Ref.Digest != contextRef.SHA256 ||
		input.Contexts[0].Ref.Digest != contextRef.SHA256 ||
		len(input.Regions) != 1 || input.Regions[0].StartLine != 3 {
		t.Fatalf("frozen context closure target=%+v input=%+v",
			target.Contexts, input)
	}
}

func TestServiceScopeEnumeratesExactTreeAndHonorsExclude(t *testing.T) {
	repositoryPath, _, _ := reviewFixture(t)
	writeFixture(t, repositoryPath, "pkg/service.go",
		"package pkg\n// ARGUS_BUG production file\n")
	writeFixture(t, repositoryPath, "pkg/service_test.go",
		"package pkg\n// ARGUS_BUG excluded test file\n")
	revision := commitFixture(t, repositoryPath, "scope")
	writeFixture(t, repositoryPath, "pkg/service.go", "package pkg\n")
	service, repository := newTestService(t, ServiceOptions{})

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		Mode:           reviewcore.TargetModeScope,
		Revision:       revision,
		Include:        []string{"pkg/**"},
		Exclude:        []string{"**/*_test.go"},
	})
	if err != nil {
		t.Fatalf("Review(scope) error = %v", err)
	}
	if outcome.Run.TargetMode != runmodel.TargetModeScope ||
		outcome.Report == nil || outcome.Report.Summary.Verified != 1 ||
		len(outcome.Report.Findings) != 1 ||
		outcome.Report.Findings[0].Path != "pkg/service.go" {
		t.Fatalf("scope outcome = %+v", outcome)
	}
	var target MaterializedTarget
	if err := repository.ReadJSONArtifact(outcome.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("read scope target: %v", err)
	}
	if target.Snapshot.Scope == nil ||
		target.Snapshot.Scope.MatchedFiles != 1 ||
		target.Snapshot.Scope.IncludedFiles != 1 ||
		target.PatchRef != nil || target.SelectionContentRef != nil {
		t.Fatalf("scope target = %+v", target)
	}
}

func TestServicePersistsRetryLineageBeforeSuccess(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	var calls int
	service, repository := newTestService(t, ServiceOptions{
		ExecutorIdentity: "test-retry-wrapper",
		BuildIdentity:    "argus-test-retry",
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
				calls++
				if calls == 1 {
					return reviewcore.StageResult{}, &RetryableStageError{
						Code: "temporary",
						Err:  errors.New("transient detector failure"),
					}
				}
			}
			return reviewcore.ExecuteStageWithPolicy(ctx, input, stage, upstream, policy)
		},
	})
	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if len(outcome.Run.StageAttempts) != 11 ||
		outcome.Run.StageAttempts[2].Status != runmodel.StageStatusFailed ||
		!outcome.Run.StageAttempts[2].Retryable ||
		outcome.Run.StageAttempts[3].Status != runmodel.StageStatusSucceeded {
		t.Fatalf("retry attempts = %+v", outcome.Run.StageAttempts)
	}
	if len(outcome.Run.Bindings) != 11 || len(outcome.Run.Evidence) != 10 {
		t.Fatalf("retry facts = bindings %d evidence %d",
			len(outcome.Run.Bindings), len(outcome.Run.Evidence))
	}
	if _, err := repository.LoadRun(outcome.Run.RunID); err != nil {
		t.Fatalf("LoadRun(retried) error = %v", err)
	}
}

func TestServiceCancellationStopsSchedulingAndCommitsCanceledRun(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	var scheduled []reviewcore.StageName
	service, repository := newTestService(t, ServiceOptions{
		ExecutorIdentity: "test-cancel-wrapper",
		BuildIdentity:    "argus-test-cancel",
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
			scheduled = append(scheduled, stage)
			if stage == reviewcore.StageVerify {
				return reviewcore.StageResult{}, context.Canceled
			}
			return reviewcore.ExecuteStageWithPolicy(ctx, input, stage, upstream, policy)
		},
	})
	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Failure.Code != "canceled" {
		t.Fatalf("Review() error = %v, want canceled RunError", err)
	}
	if outcome.Run.Status != runmodel.RunStatusCanceled ||
		strings.Join(stageStrings(scheduled), ",") !=
			"materialize_target,plan_context,detect,normalize,verify" {
		t.Fatalf("canceled outcome = %+v, scheduled = %v", outcome, scheduled)
	}
	loaded, loadErr := repository.LoadRun(outcome.Run.RunID)
	if loadErr != nil {
		t.Fatalf("LoadRun(canceled) error = %v", loadErr)
	}
	if loaded.Status != runmodel.RunStatusCanceled {
		t.Fatalf("loaded status = %q", loaded.Status)
	}
}

func TestServiceEnforcesFrozenStageTimeout(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	repositoryRoot, err := canonicalRepositoryRoot(repositoryPath)
	if err != nil {
		t.Fatalf("canonicalRepositoryRoot() error = %v", err)
	}
	bundle, err := DefaultConfigBundle(
		DefaultLocalConfig(),
		workflow.DefaultReviewDefinition(),
		reviewconfig.ResolutionContext{
			TenantID: localTenantID, OrganizationID: localOrganizationID,
			RepositoryID: expectedLocalRepositoryID(repositoryRoot),
			InvocationID: "run-0001",
		},
	)
	if err != nil {
		t.Fatalf("DefaultConfigBundle() error = %v", err)
	}
	bundle.Budget.StageTimeoutMS = 1
	bundle.Budget.MaxAttempts = 1
	resealConfigBundle(t, &bundle)
	service, _ := newTestService(t, ServiceOptions{
		ConfigBundle:     bundle,
		ExecutorIdentity: "test-timeout-wrapper",
		BuildIdentity:    "argus-test-timeout",
		ExecutorCapabilities: map[string]workflow.ExecutorCapabilities{
			"deterministic-local": {},
		},
		ExecuteStage: func(
			ctx context.Context,
			_ reviewcore.ReviewInput,
			_ reviewcore.StageName,
			_ reviewcore.StageResult,
			_ reviewcore.RuntimePolicy,
		) (reviewcore.StageResult, error) {
			<-ctx.Done()
			return reviewcore.StageResult{}, ctx.Err()
		},
	})

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Failure.Code != "stage_timeout" {
		t.Fatalf("Review() error = %v, want stage_timeout RunError", err)
	}
	if outcome.Run.Status != runmodel.RunStatusFailed ||
		len(outcome.Run.StageAttempts) != bundle.Budget.MaxAttempts {
		t.Fatalf("timed out outcome = %+v", outcome.Run)
	}
}

func TestServiceMarksOversizeFileEvidencePartialAndHumanReview(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	config := DefaultLocalConfig()
	config.MaxFileContentBytes = 16
	service, _ := newTestService(t, ServiceOptions{Config: config})

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if outcome.Report == nil ||
		outcome.Report.Summary.Inconclusive != 1 ||
		outcome.Report.Summary.HumanReview != 1 {
		t.Fatalf("partial report = %+v", outcome.Report)
	}
	for _, evidence := range outcome.Run.Evidence {
		if evidence.Completeness != runmodel.CompletenessPartial ||
			len(evidence.CompletenessNotes) == 0 {
			t.Fatalf("partial evidence = %+v", evidence)
		}
	}
}

func newTestService(
	t *testing.T,
	options ServiceOptions,
) (*Service, *runrepo.Repository) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatalf("local.Open() error = %v", err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		t.Fatalf("runrepo.New() error = %v", err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatalf("gitadapter.New() error = %v", err)
	}
	if options.IDs == nil {
		options.IDs = &sequenceIDs{}
	}
	if options.Now == nil {
		clock := &incrementingClock{
			value: time.Date(2026, time.July, 26, 13, 0, 0, 0, time.UTC),
		}
		options.Now = clock.Now
	}
	if options.Workloads == nil {
		options.DisableScheduling = true
	}
	service, err := NewService(source, repository, options)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service, repository
}

type sequenceIDs struct {
	mu       sync.Mutex
	sequence int
}

func (ids *sequenceIDs) New(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.sequence++
	return fmt.Sprintf("%s-%04d", prefix, ids.sequence), nil
}

type incrementingClock struct {
	mu    sync.Mutex
	value time.Time
}

func (clock *incrementingClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.value = clock.value.Add(time.Millisecond)
	return clock.value
}

func reviewFixture(t *testing.T) (string, string, string) {
	t.Helper()
	repository := t.TempDir()
	runGit(t, repository, "init", "-q", "-b", "main")
	runGit(t, repository, "config", "user.name", "Argus Test")
	runGit(t, repository, "config", "user.email", "argus@example.invalid")
	runGit(t, repository, "config", "commit.gpgsign", "false")
	writeFixture(t, repository, "review.go", "package fixture\n\nfunc Review() {}\n")
	base := commitFixture(t, repository, "base")
	writeFixture(
		t,
		repository,
		"review.go",
		"package fixture\n\n// ARGUS_BUG: verified fixture\nfunc Review() {}\n",
	)
	head := commitFixture(t, repository, "head")
	writeFixture(t, repository, "review.go", "package fixture\n\nfunc Review() {}\n")
	return repository, base, head
}

func commitFixture(t *testing.T, repository string, message string) string {
	t.Helper()
	runGit(t, repository, "add", "--all")
	runGit(t, repository, "commit", "-q", "-m", message)
	return strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
}

func writeFixture(t *testing.T, repository string, path string, content string) {
	t.Helper()
	fullPath := filepath.Join(repository, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func runGit(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", arguments...)
	command.Dir = repository
	command.Env = append(
		os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func stageStrings(stages []reviewcore.StageName) []string {
	values := make([]string, len(stages))
	for index, stage := range stages {
		values[index] = string(stage)
	}
	return values
}
