package runrepo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/configdefaults"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/targetmodel"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestRepositoryArtifactRoundTrip(t *testing.T) {
	repository := newTestRepository(t)
	input := struct {
		Value string `json:"value"`
	}{Value: "checkpoint"}

	ref, err := repository.PutJSONArtifact("argus.test.v1", input)
	if err != nil {
		t.Fatalf("PutJSONArtifact() error = %v", err)
	}
	var output struct {
		Value string `json:"value"`
	}
	if err := repository.ReadJSONArtifact(ref, &output); err != nil {
		t.Fatalf("ReadJSONArtifact() error = %v", err)
	}
	if output != input {
		t.Fatalf("round trip = %+v, want %+v", output, input)
	}
}

func TestReadJSONArtifactRejectsDuplicateUnknownAndTrailingData(t *testing.T) {
	repository := newTestRepository(t)
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "nested duplicate",
			content: `{"value":"ok","nested":{"key":1,"key":2}}`,
			want:    "duplicate JSON field",
		},
		{
			name:    "unknown",
			content: `{"value":"ok","extra":true}`,
			want:    "unknown field",
		},
		{
			name:    "trailing",
			content: `{"value":"ok"} {"value":"other"}`,
			want:    "trailing",
		},
	}
	type document struct {
		Value  string `json:"value"`
		Nested *struct {
			Key int `json:"key"`
		} `json:"nested,omitempty"`
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref := putArtifact(t, repository, "argus.test.v1", test.content)
			var output document
			err := repository.ReadJSONArtifact(ref, &output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadJSONArtifact() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRepositoryFinalizeRunIsIdempotentAndAuthoritative(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	run := persistedTerminalRun(t, repository, now)
	started := RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: EventRunStarted, ExecutionSnapshotID: run.ExecutionSnapshotID,
	}
	if err := repository.AppendEvent("run-1-started", now, started); err != nil {
		t.Fatalf("AppendEvent(started) error = %v", err)
	}
	eventTime := now.Add(time.Second)
	firstRef, err := repository.FinalizeRun(TerminalEventID(run.RunID), eventTime, run)
	if err != nil {
		t.Fatalf("FinalizeRun() error = %v", err)
	}
	secondRef, err := repository.FinalizeRun(TerminalEventID(run.RunID), eventTime, run)
	if err != nil {
		t.Fatalf("idempotent FinalizeRun() error = %v", err)
	}
	if firstRef != secondRef {
		t.Fatalf("FinalizeRun() refs differ: %+v / %+v", firstRef, secondRef)
	}

	loaded, err := repository.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if loaded.RunID != run.RunID || loaded.Status != runmodel.RunStatusSucceeded {
		t.Fatalf("LoadRun() = %+v", loaded)
	}
	result, err := repository.LoadCommittedRunResult(run.RunID)
	if err != nil {
		t.Fatalf("LoadCommittedRunResult() error = %v", err)
	}
	if result.Run.RunID != run.RunID || result.Report == nil ||
		result.CandidateSet != nil || result.GovernedReport != nil {
		t.Fatalf("LoadCommittedRunResult() = %+v", result)
	}
	history, err := repository.History(10)
	if err != nil {
		t.Fatalf("History() error = %v", err)
	}
	if len(history) != 1 || history[0].Status != runmodel.RunStatusSucceeded ||
		history[0].LastEventType != EventRunSucceeded {
		t.Fatalf("History() = %+v", history)
	}
	events, err := repository.Events(run.RunID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	wantEvents := len(run.StageAttempts)*2 + 2
	if len(events) != wantEvents ||
		events[0].Sequence != 1 ||
		events[wantEvents-1].Sequence != uint64(wantEvents) {
		t.Fatalf("Events() = %+v", events)
	}
}

func TestImpactAtReturnsExactFrozenBindingsAndExplicitNonterminalGaps(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)
	run := persistedTerminalRun(t, repository, now)
	if err := repository.AppendEvent("run-1-impact-started", now, RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: EventRunStarted, ExecutionSnapshotID: run.ExecutionSnapshotID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FinalizeRun(
		TerminalEventID(run.RunID), now.Add(time.Millisecond), run,
	); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AppendEvent("run-without-snapshot-created", now.Add(time.Second), RunEvent{
		RunID: "run-without-snapshot", Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusPending, EventType: EventRunCreated,
	}); err != nil {
		t.Fatal(err)
	}

	tests := []ImpactSelector{
		{Kind: ImpactConfigBundle, ID: snapshot.Config.ID, Revision: snapshot.Config.Revision, SHA256: snapshot.Config.SHA256},
		{Kind: ImpactRulePack, ID: bundle.RulePack.ID, Revision: bundle.RulePack.Revision, SHA256: bundle.RulePack.SHA256},
		{Kind: ImpactWorkflow, ID: snapshot.Workflow.ID, Revision: snapshot.Workflow.Revision, SHA256: snapshot.Workflow.SHA256},
		{Kind: ImpactModel, ID: bundle.Execution.ModelProfile.ID, Revision: bundle.Execution.ModelProfile.Revision, SHA256: bundle.Execution.ModelProfile.SHA256},
	}
	for _, selector := range tests {
		result, err := repository.ImpactAt(selector, 0)
		if err != nil {
			t.Fatalf("ImpactAt(%s) error = %v", selector.Kind, err)
		}
		if len(result.Matches) != 1 || result.Matches[0].History.RunID != run.RunID ||
			len(result.Matches[0].Bindings) == 0 {
			t.Fatalf("ImpactAt(%+v) matches = %+v coverage=%+v", selector, result.Matches, result.Coverage)
		}
		if result.Coverage.Complete || result.Coverage.RunsResolved != 1 ||
			len(result.Coverage.Gaps) != 1 ||
			result.Coverage.Gaps[0].RunID != "run-without-snapshot" {
			t.Fatalf("ImpactAt(%s) coverage = %+v", selector.Kind, result.Coverage)
		}
	}

	miss, err := repository.ImpactAt(ImpactSelector{
		Kind: ImpactWorkflow, ID: snapshot.Workflow.ID, Revision: "another-revision",
	}, 0)
	if err != nil || len(miss.Matches) != 0 {
		t.Fatalf("revision mismatch result=%+v err=%v", miss, err)
	}
}

func TestMatchingImpactBindingsIncludesAppliedConfigRevisionAndFormalModel(t *testing.T) {
	bundle := reviewconfig.ConfigBundle{
		AppliedRevisions: []reviewconfig.SourceRef{{ID: "repository-policy", Revision: "7"}},
		AgentReview: &reviewconfig.AgentReviewPolicy{
			Model: reviewconfig.VersionedRef{ID: "deepseek-review", Revision: "2026-08", SHA256: strings.Repeat("a", 64)},
		},
	}
	configBindings := matchingImpactBindings(ImpactSelector{
		Kind: ImpactConfigRevision, ID: "repository-policy", Revision: "7",
	}, runmodel.ExecutionSnapshot{}, bundle)
	if len(configBindings) != 1 || configBindings[0].Source != "config_bundle.applied_revisions" {
		t.Fatalf("config revision bindings = %+v", configBindings)
	}
	modelBindings := matchingImpactBindings(ImpactSelector{
		Kind: ImpactModel, ID: "deepseek-review", Revision: "2026-08", SHA256: strings.Repeat("a", 64),
	}, runmodel.ExecutionSnapshot{}, bundle)
	if len(modelBindings) != 1 || modelBindings[0].Source != "config_bundle.agent_review.model" {
		t.Fatalf("formal model bindings = %+v", modelBindings)
	}
}

func TestImpactIndexIsIncrementalAndAvoidsConfigArtifactRescan(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, 8, 26, 7, 0, 0, 0, time.UTC)
	run := persistedTerminalRun(t, repository, now)
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ExecutionSnapshotID = "snapshot-impact-pending"
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	pendingRunID := "run-impact-pending"
	if err := repository.AppendEvent(pendingRunID+"-created", now.Add(time.Second), RunEvent{
		RunID: pendingRunID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusPending,
		EventType: EventRunCreated, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
	}); err != nil {
		t.Fatal(err)
	}
	index, err := repository.loadImpactIndex()
	if err != nil {
		t.Fatal(err)
	}
	fact, exists := index[pendingRunID]
	if !exists || fact.ExecutionSnapshotID != snapshot.ExecutionSnapshotID || len(fact.Bindings) < 4 {
		t.Fatalf("impact index fact = %+v exists=%t", fact, exists)
	}
	// Removing the source bundle proves the hot query uses the already frozen
	// reverse index rather than reopening every historical ConfigBundle.
	configPath := filepath.Join(
		repository.store.Root(), "artifacts", "sha256", snapshot.ConfigBundleRef.SHA256[:2],
		snapshot.ConfigBundleRef.SHA256,
	)
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	result, err := repository.ImpactAt(ImpactSelector{
		Kind: ImpactConfigBundle, ID: snapshot.Config.ID, Revision: snapshot.Config.Revision,
		SHA256: snapshot.Config.SHA256,
	}, 0)
	if err != nil || len(result.Matches) != 1 || result.Matches[0].History.RunID != pendingRunID {
		t.Fatalf("indexed ImpactAt() = %+v, %v", result, err)
	}
}

func TestImpactIndexBackfillsLegacyRunAndThenServesWithoutArtifact(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, 8, 26, 7, 30, 0, 0, time.UTC)
	run := persistedTerminalRun(t, repository, now)
	finalRef, err := repository.PutJSONArtifact(runmodel.ContractReviewRun, run)
	if err != nil {
		t.Fatal(err)
	}
	terminal := RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: run.Status,
		EventType: EventRunSucceeded, ExecutionSnapshotID: run.ExecutionSnapshotID,
		FinalRunRef: &finalRef,
	}
	if _, err := repository.store.AppendJSONL("run-index", local.Event{
		ID: TerminalEventID(run.RunID), Schema: RunEventSchemaVersion,
		Time: now.Add(time.Second), Payload: terminal,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	selector := ImpactSelector{
		Kind: ImpactWorkflow, ID: snapshot.Workflow.ID, Revision: snapshot.Workflow.Revision,
		SHA256: snapshot.Workflow.SHA256,
	}
	missing, err := repository.ImpactAt(selector, 0)
	if err != nil || len(missing.Matches) != 0 || missing.Coverage.Complete ||
		len(missing.Coverage.Gaps) != 1 ||
		missing.Coverage.Gaps[0].ReasonCode != "impact_index_unavailable" {
		t.Fatalf("pre-rebuild ImpactAt() = %+v, %v", missing, err)
	}
	if index, err := repository.loadImpactIndex(); err != nil || len(index) != 0 {
		t.Fatalf("GET mutated impact index: %+v, %v", index, err)
	}
	rebuild, err := repository.RebuildImpactIndex(t.Context())
	if err != nil || !rebuild.Complete || rebuild.AppendedFacts != 1 || len(rebuild.Gaps) != 0 {
		t.Fatalf("RebuildImpactIndex() = %+v, %v", rebuild, err)
	}
	first, err := repository.ImpactAt(selector, 0)
	if err != nil || len(first.Matches) != 1 {
		t.Fatalf("legacy backfill ImpactAt() = %+v, %v", first, err)
	}
	index, err := repository.loadImpactIndex()
	if err != nil || index[run.RunID].ExecutionSnapshotID != run.ExecutionSnapshotID {
		t.Fatalf("backfilled index = %+v, %v", index, err)
	}
	if err := os.Remove(filepath.Join(
		repository.store.Root(), "artifacts", "sha256", snapshot.ConfigBundleRef.SHA256[:2],
		snapshot.ConfigBundleRef.SHA256,
	)); err != nil {
		t.Fatal(err)
	}
	miss, err := repository.ImpactAt(ImpactSelector{
		Kind: ImpactModel, ID: "unrelated-model", Revision: "1",
	}, first.Watermark)
	if err != nil || len(miss.Matches) != 0 || !miss.Coverage.Complete {
		t.Fatalf("post-backfill unrelated ImpactAt() = %+v, %v", miss, err)
	}
}

func TestImpactIndexConflictPreventsNewAuthoritativeTerminal(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)
	run := persistedTerminalRun(t, repository, now)
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := repository.buildImpactIndexFact(run.RunID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fact.ExecutionSnapshotID = "conflicting-snapshot"
	if err := repository.appendImpactIndexFact(fact, snapshot.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FinalizeRun(
		TerminalEventID(run.RunID), now.Add(time.Second), run,
	); !errors.Is(err, local.ErrEventConflict) {
		t.Fatalf("FinalizeRun() error = %v, want impact index conflict", err)
	}
	if _, err := repository.LoadRun(run.RunID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting impact row admitted terminal run: %v", err)
	}
}

func TestImpactAtFailsClosedOnCorruptIndexFact(t *testing.T) {
	repository := newTestRepository(t)
	if _, err := repository.store.AppendJSONL(impactIndexStream, local.Event{
		ID: "run-corrupt-impact-index", Schema: ImpactIndexFactSchemaVersion,
		Time: time.Date(2026, 8, 26, 8, 30, 0, 0, time.UTC),
		Payload: ImpactIndexFact{
			SchemaVersion: ImpactIndexFactSchemaVersion, RunID: "run-corrupt",
			ExecutionSnapshotID: "snapshot-corrupt", Bindings: []ImpactBinding{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := repository.ImpactAt(ImpactSelector{
		Kind: ImpactModel, ID: "model", Revision: "1",
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "impact index fact must contain") {
		t.Fatalf("ImpactAt() error = %v", err)
	}
}

func TestRepositoryFinalizeRejectsDanglingArtifact(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	dangling := run.StageAttempts[0].OutputRef
	dangling.SHA256 = strings.Repeat("f", 64)
	dangling.URI = "artifact://local/sha256/" + dangling.SHA256
	run.StageAttempts[0].OutputRef = dangling
	run.Evidence[0].ArtifactRef = *dangling

	_, err := repository.FinalizeRun(TerminalEventID(run.RunID), now.Add(time.Second), run)
	if err == nil || !strings.Contains(err.Error(), "output_ref") {
		t.Fatalf("FinalizeRun() error = %v, want dangling output rejection", err)
	}
	if _, err := repository.LoadRun(run.RunID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadRun() after rejected finalize error = %v, want not exist", err)
	}
}

func TestHistoryAtReconstructsExactWatermarkAfterNewEvents(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	appendLifecycle := func(eventID string, at time.Time, event RunEvent) {
		t.Helper()
		if err := repository.AppendEvent(eventID, at, event); err != nil {
			t.Fatalf("AppendEvent(%s) error = %v", eventID, err)
		}
	}
	appendLifecycle("run-a-created", now, RunEvent{
		RunID: "run-a", Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusPending, EventType: EventRunCreated,
	})
	appendLifecycle("run-b-created", now.Add(time.Second), RunEvent{
		RunID: "run-b", Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusPending, EventType: EventRunCreated,
	})
	first, err := repository.HistoryAt(0)
	if err != nil {
		t.Fatalf("HistoryAt(latest) error = %v", err)
	}
	if first.Watermark != 2 || len(first.Entries) != 2 || first.Entries[0].RunID != "run-b" {
		t.Fatalf("first snapshot = %+v", first)
	}

	appendLifecycle("run-a-started", now.Add(2*time.Second), RunEvent{
		RunID: "run-a", Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusRunning, EventType: EventRunStarted,
	})
	latest, err := repository.HistoryAt(0)
	if err != nil {
		t.Fatalf("HistoryAt(updated latest) error = %v", err)
	}
	if latest.Watermark != 3 || latest.Entries[0].RunID != "run-a" ||
		latest.Entries[0].Status != runmodel.RunStatusRunning {
		t.Fatalf("updated snapshot = %+v", latest)
	}
	frozen, err := repository.HistoryAt(first.Watermark)
	if err != nil {
		t.Fatalf("HistoryAt(frozen) error = %v", err)
	}
	if !reflect.DeepEqual(frozen, first) {
		t.Fatalf("frozen snapshot = %+v, want %+v", frozen, first)
	}
	if _, err := repository.HistoryAt(4); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HistoryAt(future) error = %v, want os.ErrNotExist", err)
	}
}

func TestRepositoryFinalizeRejectsWrongContract(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	run.JSONReportRef.Contract = runmodel.ContractStageResult

	_, err := repository.FinalizeRun(TerminalEventID(run.RunID), now.Add(time.Second), run)
	if err == nil || !strings.Contains(err.Error(), "json_report_ref contract") {
		t.Fatalf("FinalizeRun() error = %v, want wrong contract rejection", err)
	}
}

func TestRepositoryFinalizeRejectsTargetMismatch(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	run.TargetSnapshotRef = putArtifact(
		t, repository, runmodel.ContractMaterializedTarget, `{"target":"different"}`,
	)

	_, err := repository.FinalizeRun(TerminalEventID(run.RunID), now.Add(time.Second), run)
	if err == nil || !strings.Contains(err.Error(), "target snapshot reference") {
		t.Fatalf("FinalizeRun() error = %v, want target mismatch rejection", err)
	}
}

func TestRepositoryFinalizeStrictlyRejectsLegacyAndUnknownMaterializedTargets(t *testing.T) {
	tests := []struct {
		name    string
		content func(t *testing.T, repository *Repository, run runmodel.ReviewRun) string
	}{
		{
			name: "legacy target facade",
			content: func(
				_ *testing.T,
				_ *Repository,
				_ runmodel.ReviewRun,
			) string {
				return `{"target":"legacy-facade"}`
			},
		},
		{
			name: "unknown root field",
			content: func(
				t *testing.T,
				repository *Repository,
				run runmodel.ReviewRun,
			) string {
				data, err := repository.ReadArtifact(run.TargetSnapshotRef)
				if err != nil {
					t.Fatalf("ReadArtifact(materialized target) error = %v", err)
				}
				return strings.TrimSuffix(string(data), "}") + `,"unknown":true}`
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t)
			now := time.Now().UTC()
			run := persistedTerminalRun(t, repository, now)
			ref := putArtifact(
				t,
				repository,
				runmodel.ContractMaterializedTarget,
				test.content(t, repository, run),
			)
			replaceRunTargetSnapshot(t, repository, &run, ref)

			_, err := repository.FinalizeRun(
				TerminalEventID(run.RunID),
				now.Add(time.Second),
				run,
			)
			if err == nil ||
				!strings.Contains(err.Error(), "decode materialized target") ||
				!strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("FinalizeRun() error = %v, want strict unknown-field rejection", err)
			}
		})
	}
}

func TestRepositoryFinalizeRejectsSemanticallyDamagedMaterializedTarget(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	var target targetmodel.MaterializedTarget
	if err := repository.ReadJSONArtifact(run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("ReadJSONArtifact(materialized target) error = %v", err)
	}
	tamperedContent, err := repository.PutArtifact(
		targetmodel.ContractFileContent,
		[]byte("package tampered\n"),
	)
	if err != nil {
		t.Fatalf("PutArtifact(tampered content) error = %v", err)
	}
	target.FileRefs[0].SHA256 = tamperedContent.SHA256
	target.FileRefs[0].SizeBytes = tamperedContent.SizeBytes
	target.FileRefs[0].ContentRef = &tamperedContent
	if err := target.Validate(); err != nil {
		t.Fatalf("tampered fixture should remain structurally valid: %v", err)
	}
	targetRef, err := repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		target,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(tampered target) error = %v", err)
	}
	replaceRunTargetSnapshot(t, repository, &run, targetRef)

	_, err = repository.FinalizeRun(
		TerminalEventID(run.RunID),
		now.Add(time.Second),
		run,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "frozen review input file") ||
		!strings.Contains(err.Error(), "not bound to materialized target") {
		t.Fatalf("FinalizeRun() error = %v, want semantic target closure rejection", err)
	}
}

func TestRepositoryFinalizeRejectsSelectionSelectorShapeTampering(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	replaceRunWithSelectionTarget(t, repository, &run)
	snapshot, _, _, spec := readFrozenClosure(t, repository, run)

	selection := spec.Target.Selection
	if selection == nil {
		t.Fatal("selection fixture is missing ReviewSpec selector")
	}
	selection.Ranges = []contractsv1alpha1.SelectionRange{{
		StartLine: selection.StartLine,
		EndLine:   selection.EndLine,
	}}
	selection.StartLine = 0
	selection.EndLine = 0
	if err := spec.Validate(); err != nil {
		t.Fatalf("semantically equivalent tampered ReviewSpec must remain valid: %v", err)
	}
	specRef, err := repository.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		t.Fatalf("PutJSONArtifact(tampered ReviewSpec) error = %v", err)
	}
	snapshot.ExecutionSnapshotID = "snapshot-selector-shape-" + specRef.SHA256[:12]
	snapshot.ReviewSpecRef = specRef
	snapshot.ReviewSpecSHA256 = specRef.SHA256
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(tampered selector) error = %v", err)
	}
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
	replaceRunConfigBundle(t, repository, &run, func(config *reviewconfig.ConfigBundle) {
		config.Context.Path = selection.Path
	})

	_, err = repository.FinalizeRun(
		TerminalEventID(run.RunID),
		now.Add(time.Second),
		run,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "selection does not match final run target") {
		t.Fatalf("FinalizeRun(selector shape tamper) error = %v", err)
	}
}

func TestRepositoryFinalizeRejectsContextClosureTampering(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	var target targetmodel.MaterializedTarget
	if err := repository.ReadJSONArtifact(run.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("ReadJSONArtifact(materialized target) error = %v", err)
	}
	target.Contexts = []reviewcore.ContextBinding{{
		Gap: &reviewcore.ContextGap{
			ContextID: "context-search-1",
			Kind:      "repository_search",
			Revision:  run.HeadRevision,
			Digest:    strings.Repeat("a", 64),
			Coverage: reviewcore.ContextCoverage{
				Spans: []reviewcore.ContextSpan{{
					Path: "context.go", StartLine: 1, EndLine: 3,
				}},
				Symbols: []string{},
			},
			Provenance: reviewcore.ContextProvenance{
				Provider: "local-test", ProducerID: "fixture",
				ProducerRevision: "1",
			},
			ReasonCode: "provider_unavailable",
		},
	}}
	if err := target.Validate(); err != nil {
		t.Fatalf("tampered context fixture should remain structurally valid: %v", err)
	}
	targetRef, err := repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		target,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(context-tampered target) error = %v", err)
	}
	replaceRunTargetSnapshot(t, repository, &run, targetRef)

	_, err = repository.FinalizeRun(
		TerminalEventID(run.RunID),
		now.Add(time.Second),
		run,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "contexts do not exactly match frozen review input") {
		t.Fatalf("FinalizeRun(context closure tamper) error = %v", err)
	}
}

func TestRepositoryFinalizeRejectsCompleteLedgerEvidenceForPartialTarget(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	run := persistedTerminalRunWithOptions(
		t,
		repository,
		now,
		terminalRunFixtureOptions{
			TargetCompleteness: gitadapter.CompletenessPartial,
			TargetReasons: []gitadapter.Reason{{
				Code:   gitadapter.ReasonNonCanonicalLineEndings,
				Detail: "a.go",
			}},
			// Both the final projection and the independently persisted ledger
			// deliberately claim complete evidence. The target remains partial.
			EvidenceCompleteness: runmodel.CompletenessComplete,
			EvidenceReasons:      []string{},
		},
	)

	_, err := repository.FinalizeRun(
		TerminalEventID(run.RunID),
		now.Add(time.Second),
		run,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "completeness does not match the frozen target") {
		t.Fatalf("FinalizeRun() error = %v, want frozen-target completeness rejection", err)
	}
}

func TestRepositoryFinalizeRejectsFrozenConfigTargetPolicyBypass(t *testing.T) {
	tests := []struct {
		name          string
		replaceTarget func(*testing.T, *Repository, *runmodel.ReviewRun)
		mutateConfig  func(*reviewconfig.ConfigBundle)
		want          string
	}{
		{
			name:          "allowed modes deny diff",
			replaceTarget: func(*testing.T, *Repository, *runmodel.ReviewRun) {},
			mutateConfig: func(config *reviewconfig.ConfigBundle) {
				config.Target.AllowedModes = []string{"scope", "selection"}
			},
			want: "target mode is denied by frozen config",
		},
		{
			name:          "diff path excluded",
			replaceTarget: func(*testing.T, *Repository, *runmodel.ReviewRun) {},
			mutateConfig: func(config *reviewconfig.ConfigBundle) {
				config.Target.Include = []string{"**"}
				config.Target.Exclude = []string{"a.go"}
			},
			want: `diff path "a.go" is denied by frozen config`,
		},
		{
			name:          "selection path outside include",
			replaceTarget: replaceRunWithSelectionTarget,
			mutateConfig: func(config *reviewconfig.ConfigBundle) {
				config.Context.Path = "a.go"
				config.Target.Include = []string{"internal/**"}
				config.Target.Exclude = []string{}
			},
			want: `selection path "a.go" is denied by frozen config`,
		},
		{
			name:          "scope retained content excluded",
			replaceTarget: replaceRunWithScopeTarget,
			mutateConfig: func(config *reviewconfig.ConfigBundle) {
				config.Target.Include = []string{"**"}
				config.Target.Exclude = []string{"a.go"}
			},
			want: `scope path "a.go" bypasses frozen config target policy`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t)
			now := time.Date(2026, time.July, 27, 11, 0, 0, 0, time.UTC)
			run := persistedTerminalRun(t, repository, now)
			test.replaceTarget(t, repository, &run)
			replaceRunConfigBundle(t, repository, &run, test.mutateConfig)

			_, err := repository.FinalizeRun(
				TerminalEventID(run.RunID),
				now.Add(time.Second),
				run,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("FinalizeRun() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRepositoryFinalizeRejectsCanonicalPatchManifestMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gitadapter.ChangeManifest, *targetmodel.MaterializedTarget)
	}{
		{
			name: "path",
			mutate: func(
				manifest *gitadapter.ChangeManifest,
				target *targetmodel.MaterializedTarget,
			) {
				manifest.Files[0].Path = "b.go"
				pathDigest := sha256.Sum256([]byte("b.go"))
				manifest.Files[0].PathSHA256 = formatDigest(pathDigest)
				target.FileRefs[0].Path = "b.go"
			},
		},
		{
			name: "hunk count",
			mutate: func(
				manifest *gitadapter.ChangeManifest,
				_ *targetmodel.MaterializedTarget,
			) {
				manifest.Files[0].HunkCount = 2
				manifest.Coverage.TotalHunks = 2
				manifest.Coverage.IncludedHunks = 2
			},
		},
		{
			name: "included flag",
			mutate: func(
				manifest *gitadapter.ChangeManifest,
				_ *targetmodel.MaterializedTarget,
			) {
				manifest.Files[0].Included = false
				manifest.Coverage.IncludedFiles = 0
				manifest.Coverage.SkippedFiles = 1
				manifest.Coverage.IncludedHunks = 0
				manifest.Coverage.SkippedHunks = 1
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t)
			now := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
			run := persistedTerminalRun(t, repository, now)
			replaceRunDiffManifest(t, repository, &run, test.mutate)

			_, err := repository.FinalizeRun(
				TerminalEventID(run.RunID),
				now.Add(time.Second),
				run,
			)
			if err == nil || !strings.Contains(err.Error(), "canonical patch") {
				t.Fatalf("FinalizeRun() error = %v, want canonical patch rejection", err)
			}
		})
	}
}

func TestRepositoryFinalizeRejectsFrozenInputOverConfigBudget(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, time.July, 27, 13, 0, 0, 0, time.UTC)
	run := persistedTerminalRun(t, repository, now)
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	var input reviewcore.ReviewInput
	if err := repository.ReadJSONArtifact(snapshot.ReviewInputRef, &input); err != nil {
		t.Fatalf("ReadJSONArtifact(review input) error = %v", err)
	}
	materializedBytes := int64(len(input.CanonicalPatch))
	for _, file := range input.Files {
		if file.Content != nil {
			materializedBytes += int64(len(*file.Content))
		}
	}
	if materializedBytes <= 1 {
		t.Fatalf("fixture materialized bytes = %d, want > 1", materializedBytes)
	}
	replaceRunConfigBundle(
		t,
		repository,
		&run,
		func(config *reviewconfig.ConfigBundle) {
			config.Budget.MaxInputBytes = materializedBytes - 1
		},
	)

	_, err = repository.FinalizeRun(
		TerminalEventID(run.RunID),
		now.Add(time.Second),
		run,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds config max_input_bytes") {
		t.Fatalf("FinalizeRun() error = %v, want aggregate input budget rejection", err)
	}
}

func TestRepositoryFinalizeRejectsTargetOverConfigMaxFiles(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, time.July, 27, 14, 0, 0, 0, time.UTC)
	run := persistedTerminalRun(t, repository, now)
	appendCompleteTargetFile(t, repository, &run, "b.go", "package b\n")
	replaceRunConfigBundle(
		t,
		repository,
		&run,
		func(config *reviewconfig.ConfigBundle) {
			config.Target.MaxFiles = 1
		},
	)

	_, err := repository.FinalizeRun(
		TerminalEventID(run.RunID),
		now.Add(time.Second),
		run,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds frozen config max_files") {
		t.Fatalf("FinalizeRun() error = %v, want max_files rejection", err)
	}
}

func TestRepositoryFinalizeRejectsLedgerProjectionMismatch(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	run.Bindings[0].RuntimeID = "self-declared-runtime"

	_, err := repository.FinalizeRun(TerminalEventID(run.RunID), now.Add(time.Second), run)
	if err == nil || !strings.Contains(err.Error(), "does not exactly match ledger") {
		t.Fatalf("FinalizeRun() error = %v, want independent ledger rejection", err)
	}
}

func TestRepositoryFinalizeRejectsInvalidOutputContent(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	invalidFindingSet := putArtifact(
		t, repository, runmodel.ContractFindingSet,
		`{"schema_version":"argus.finding_set.v1alpha1","target_digest":"`+
			strings.Repeat("0", 64)+`","findings":[],"decisions":[]}`,
	)
	run.FindingSetRef = &invalidFindingSet

	_, err := repository.FinalizeRun(TerminalEventID(run.RunID), now.Add(time.Second), run)
	if err == nil || !strings.Contains(err.Error(), "finding set schema or target") {
		t.Fatalf("FinalizeRun() error = %v, want finding content rejection", err)
	}
}

func TestRepositoryFinalizeRejectsReviewSpecMismatch(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	run.BaseRevision = "other-base"

	_, err := repository.FinalizeRun(TerminalEventID(run.RunID), now.Add(time.Second), run)
	if err == nil || !strings.Contains(err.Error(), "materialized target identity") {
		t.Fatalf("FinalizeRun() error = %v, want ReviewSpec mismatch rejection", err)
	}
}

func TestRepositoryFinalizeRejectsReplayLineageTampering(t *testing.T) {
	tests := []struct {
		name string
		edit func(
			t *testing.T,
			repository *Repository,
			run *runmodel.ReviewRun,
			snapshot *runmodel.ExecutionSnapshot,
		)
		want string
	}{
		{
			name: "missing inherited checkpoint",
			edit: func(
				_ *testing.T,
				_ *Repository,
				_ *runmodel.ReviewRun,
				snapshot *runmodel.ExecutionSnapshot,
			) {
				snapshot.ReplayInputRefs = snapshot.ReplayInputRefs[:1]
			},
			want: "exactly cover the workflow prefix",
		},
		{
			name: "swapped inherited checkpoints",
			edit: func(
				_ *testing.T,
				_ *Repository,
				_ *runmodel.ReviewRun,
				snapshot *runmodel.ExecutionSnapshot,
			) {
				snapshot.ReplayInputRefs[0], snapshot.ReplayInputRefs[1] =
					snapshot.ReplayInputRefs[1], snapshot.ReplayInputRefs[0]
			},
			want: "ordered workflow prefix",
		},
		{
			name: "cross build",
			edit: func(
				_ *testing.T,
				_ *Repository,
				_ *runmodel.ReviewRun,
				snapshot *runmodel.ExecutionSnapshot,
			) {
				snapshot.BuildIdentity = "argus-other-build"
			},
			want: "build identity",
		},
		{
			name: "review spec authorization tamper",
			edit: func(
				t *testing.T,
				repository *Repository,
				_ *runmodel.ReviewRun,
				snapshot *runmodel.ExecutionSnapshot,
			) {
				var spec contractsv1alpha1.ReviewSpec
				if err := repository.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
					t.Fatalf("ReadJSONArtifact(ReviewSpec) error = %v", err)
				}
				spec.TenantID = "other-tenant"
				ref, err := repository.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
				if err != nil {
					t.Fatalf("PutJSONArtifact(tampered ReviewSpec) error = %v", err)
				}
				snapshot.ReviewSpecRef = ref
				snapshot.ReviewSpecSHA256 = ref.SHA256
			},
			want: "config bundle context does not match review authorization",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t)
			now := time.Date(2026, time.July, 27, 8, 0, 0, 0, time.UTC)
			source := persistedTerminalRun(t, repository, now)
			sourceRef, err := repository.FinalizeRun(
				TerminalEventID(source.RunID),
				now.Add(time.Second),
				source,
			)
			if err != nil {
				t.Fatalf("FinalizeRun(source) error = %v", err)
			}
			replay, snapshot := persistedReplayRun(
				t,
				repository,
				source,
				sourceRef,
				"replay-1",
				reviewcore.StageVerify,
				now.Add(2*time.Second),
			)
			test.edit(t, repository, &replay, &snapshot)
			snapshot.ExecutionSnapshotID = "snapshot-" +
				strings.ReplaceAll(test.name, " ", "-")
			if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
				t.Fatalf("SaveExecutionSnapshot(tampered) error = %v", err)
			}
			replay.ExecutionSnapshotID = snapshot.ExecutionSnapshotID

			_, err = repository.FinalizeRun(
				TerminalEventID(replay.RunID),
				now.Add(3*time.Second),
				replay,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("FinalizeRun(tampered replay) error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRepositoryFinalizeRejectsChainedReplaySourceReferenceTamper(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC)
	original := persistedTerminalRun(t, repository, now)
	originalRef, err := repository.FinalizeRun(
		TerminalEventID(original.RunID),
		now.Add(time.Second),
		original,
	)
	if err != nil {
		t.Fatalf("FinalizeRun(original) error = %v", err)
	}
	firstReplay, _ := persistedReplayRun(
		t,
		repository,
		original,
		originalRef,
		"replay-1",
		reviewcore.StageVerify,
		now.Add(2*time.Second),
	)
	firstReplayRef, err := repository.FinalizeRun(
		TerminalEventID(firstReplay.RunID),
		now.Add(3*time.Second),
		firstReplay,
	)
	if err != nil {
		t.Fatalf("FinalizeRun(first replay) error = %v", err)
	}
	chained, snapshot := persistedReplayRun(
		t,
		repository,
		firstReplay,
		firstReplayRef,
		"replay-2",
		reviewcore.StageReport,
		now.Add(4*time.Second),
	)
	snapshot.ExecutionSnapshotID = "snapshot-replay-2-tampered-source"
	snapshot.ReplaySourceRunRef = &originalRef
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(tampered chained replay) error = %v", err)
	}
	chained.ExecutionSnapshotID = snapshot.ExecutionSnapshotID

	_, err = repository.FinalizeRun(
		TerminalEventID(chained.RunID),
		now.Add(5*time.Second),
		chained,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "source reference does not match authoritative") {
		t.Fatalf("FinalizeRun(tampered chained replay) error = %v", err)
	}
}

func TestRepositoryRejectsDuplicateEvidenceAndWrongFencing(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		edit func(*runmodel.ReviewRun)
		want string
	}{
		{
			name: "duplicate evidence",
			edit: func(run *runmodel.ReviewRun) {
				duplicate := run.Evidence[0]
				duplicate.EvidenceID = "evidence-2"
				run.Evidence = append(run.Evidence, duplicate)
			},
			want: "duplicate binding",
		},
		{
			name: "wrong fencing",
			edit: func(run *runmodel.ReviewRun) {
				run.Evidence[0].FencingToken++
			},
			want: "fencing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t)
			run := persistedTerminalRun(t, repository, now)
			test.edit(&run)
			_, err := repository.FinalizeRun(
				TerminalEventID(run.RunID), now.Add(time.Second), run,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("FinalizeRun() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRepositoryTerminalStatusCannotRollback(t *testing.T) {
	repository := newTestRepository(t)
	now := time.Now().UTC()
	run := persistedTerminalRun(t, repository, now)
	if _, err := repository.FinalizeRun(
		TerminalEventID(run.RunID), now.Add(time.Second), run,
	); err != nil {
		t.Fatalf("FinalizeRun() error = %v", err)
	}
	rollback := RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: EventRunStarted,
	}
	if err := repository.AppendEvent(
		"run-1-late-started", now.Add(2*time.Second), rollback,
	); err == nil || !strings.Contains(err.Error(), "already terminal") {
		t.Fatalf("AppendEvent(rollback) error = %v, want terminal rejection", err)
	}
	history, err := repository.History(1)
	if err != nil {
		t.Fatalf("History() error = %v", err)
	}
	if len(history) != 1 || history[0].Status != runmodel.RunStatusSucceeded {
		t.Fatalf("History() = %+v, want succeeded", history)
	}
}

func TestRepositoryRejectsNonTerminalFinalRun(t *testing.T) {
	repository := newTestRepository(t)
	run := runmodel.ReviewRun{RunID: "run-1", Status: runmodel.RunStatusRunning}
	if _, err := repository.FinalizeRun(
		TerminalEventID(run.RunID), time.Now().UTC(), run,
	); err == nil {
		t.Fatal("FinalizeRun() accepted a running run")
	}
}

func TestAppendEventRequiresRunNamespace(t *testing.T) {
	repository := newTestRepository(t)
	event := RunEvent{
		RunID: "run-1", Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusRunning, EventType: EventRunStarted,
	}
	if err := repository.AppendEvent("other-started", time.Now().UTC(), event); err == nil {
		t.Fatal("AppendEvent() accepted a foreign event namespace")
	}
}

func newTestRepository(t *testing.T) *Repository {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatalf("local.Open() error = %v", err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return repository
}

func persistedTerminalRun(
	t *testing.T,
	repository *Repository,
	now time.Time,
) runmodel.ReviewRun {
	t.Helper()
	return persistedTerminalRunWithOptions(
		t,
		repository,
		now,
		terminalRunFixtureOptions{},
	)
}

type terminalRunFixtureOptions struct {
	TargetCompleteness   gitadapter.Completeness
	TargetReasons        []gitadapter.Reason
	EvidenceCompleteness runmodel.Completeness
	EvidenceReasons      []string
}

func persistedTerminalRunWithOptions(
	t *testing.T,
	repository *Repository,
	now time.Time,
	options terminalRunFixtureOptions,
) runmodel.ReviewRun {
	t.Helper()
	if options.TargetCompleteness == "" {
		options.TargetCompleteness = gitadapter.CompletenessComplete
		options.TargetReasons = []gitadapter.Reason{}
	}
	if options.EvidenceCompleteness == "" {
		options.EvidenceCompleteness = runmodel.CompletenessComplete
		options.EvidenceReasons = []string{}
	}
	baseOID := strings.Repeat("a", 40)
	headOID := strings.Repeat("b", 40)
	input := testReviewInput()
	patchRef := putArtifact(
		t, repository, runmodel.ContractCanonicalPatch, input.CanonicalPatch,
	)
	fileContent := []byte(*input.Files[0].Content)
	fileRef, err := repository.PutArtifact(targetmodel.ContractFileContent, fileContent)
	if err != nil {
		t.Fatalf("PutArtifact(file content) error = %v", err)
	}
	pathDigest := sha256.Sum256([]byte("a.go"))
	manifest := gitadapter.ChangeManifest{
		SchemaVersion: gitadapter.ChangeManifestSchemaVersion,
		BaseCommitOID: baseOID,
		HeadCommitOID: headOID,
		PatchSHA256:   patchRef.SHA256,
		PatchSize:     patchRef.SizeBytes,
		Files: []gitadapter.FileChange{{
			Status: gitadapter.ChangeAdded, Path: "a.go",
			PathSHA256:     formatDigest(pathDigest),
			Language:       "go",
			HunkCount:      1,
			Included:       true,
			SkippedReasons: []gitadapter.ReasonCode{},
		}},
		Coverage: gitadapter.Coverage{
			TotalFiles: 1, IncludedFiles: 1,
			TotalHunks: 1, IncludedHunks: 1, DiffFileHeaders: 1,
		},
		Completeness: gitadapter.CompletenessComplete,
		Reasons:      []gitadapter.Reason{},
	}
	manifestRef, err := repository.PutJSONArtifact(
		targetmodel.ContractChangeManifest,
		manifest,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(change manifest) error = %v", err)
	}
	targetSnapshot, err := targetmodel.SealTargetSnapshot(targetmodel.TargetSnapshot{
		SchemaVersion: targetmodel.TargetSnapshotSchemaVersion,
		Mode:          reviewcore.TargetModeDiff,
		Repository: gitadapter.RepositorySnapshot{
			Kind: "local_git", RepositoryID: "repository-1", ObjectFormat: "sha1",
		},
		Base:           gitadapter.RevisionSnapshot{Requested: "base", CommitOID: baseOID},
		Head:           gitadapter.RevisionSnapshot{Requested: "head", CommitOID: headOID},
		ManifestSHA256: manifestRef.SHA256,
		Diff: &targetmodel.DiffSnapshot{
			PatchSHA256:    patchRef.SHA256,
			PatchSizeBytes: patchRef.SizeBytes,
			PatchFormat:    gitadapter.CanonicalPatchVersion,
		},
		DirtyState:         gitadapter.DirtyStateClean,
		Completeness:       options.TargetCompleteness,
		CompletenessReason: append([]gitadapter.Reason{}, options.TargetReasons...),
		CapturedAt:         now,
		CapturedBy:         "argus-test",
		GitVersion:         "git version test",
	})
	if err != nil {
		t.Fatalf("SealTargetSnapshot() error = %v", err)
	}
	target := targetmodel.MaterializedTarget{
		SchemaVersion: targetmodel.MaterializedTargetSchemaVersion,
		Snapshot:      targetSnapshot,
		ManifestRef:   manifestRef,
		PatchRef:      &patchRef,
		Contexts:      []reviewcore.ContextBinding{},
		FileRefs: []targetmodel.TargetFileRef{{
			Path: "a.go", Status: gitadapter.ChangeAdded,
			SHA256: fileRef.SHA256, SizeBytes: fileRef.SizeBytes,
			ContentRef: &fileRef, Completeness: gitadapter.CompletenessComplete,
			Reasons: []gitadapter.Reason{},
		}},
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("MaterializedTarget.Validate() error = %v", err)
	}
	targetRef, err := repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		target,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(materialized target) error = %v", err)
	}
	input.TargetID = targetSnapshot.TargetSnapshotID
	inputRef := putArtifact(t, repository, runmodel.ContractReviewInput, mustJSON(t, input))
	result, err := reviewcore.Run(context.Background(), input, reviewcore.RunOptions{})
	if err != nil {
		t.Fatalf("reviewcore.Run() error = %v", err)
	}
	stageRefs := make([]runmodel.ArtifactRef, len(result.Stages))
	for index, stage := range result.Stages {
		stageRefs[index] = putArtifact(
			t, repository, runmodel.ContractStageResult, mustJSON(t, stage),
		)
	}
	findingRef := putArtifact(
		t, repository, runmodel.ContractFindingSet, mustJSON(t, findingSetProjection{
			SchemaVersion: runmodel.ContractFindingSet,
			TargetDigest:  result.Report.TargetDigest,
			Findings:      result.Report.Findings,
			Decisions:     result.Report.Decisions,
		}),
	)
	jsonReportRef := putArtifact(
		t, repository, runmodel.ContractJSONReport, string(result.ReportJSON),
	)
	markdownReportRef := putArtifact(
		t, repository, runmodel.ContractMarkdownReport, result.ReportMarkdown,
	)
	definition := workflow.DefaultReviewDefinition()
	workflowRef, err := repository.PutJSONArtifact(
		runmodel.ContractWorkflowDefinition, definition,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(workflow) error = %v", err)
	}
	config, err := configdefaults.Bundle(configdefaults.Options{
		ID: "local-default", Revision: "1",
		MaxFiles: 1000, MaxPatchBytes: 16 << 20,
		MaxInputBytes: 16 << 20, MaxOutputBytes: 16 << 20,
		MaxAttempts: 2,
		Context: reviewconfig.ResolutionContext{
			TenantID: "local", OrganizationID: "local",
			RepositoryID: "repository-1", InvocationID: "run-1",
		},
	}, definition)
	if err != nil {
		t.Fatalf("configdefaults.Bundle() error = %v", err)
	}
	configRef, err := repository.PutJSONArtifact(runmodel.ContractConfigBundle, config)
	if err != nil {
		t.Fatalf("PutJSONArtifact(config) error = %v", err)
	}
	spec := contractsv1alpha1.ReviewSpec{
		SchemaVersion:  contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:      "run-1",
		IdempotencyKey: "run-1",
		TenantID:       "local",
		WorkspaceID:    "local",
		Repository: contractsv1alpha1.RepositoryRef{
			Provider: "local-git", RepositoryID: "repository-1",
		},
		Target: contractsv1alpha1.ReviewTarget{
			Mode: contractsv1alpha1.ReviewModeDiff,
			Diff: &contractsv1alpha1.DiffTarget{
				BaseRevision: baseOID, HeadRevision: headOID,
				Patch: contractsv1alpha1.ContentRef{
					URI: patchRef.URI, SHA256: patchRef.SHA256, SizeBytes: patchRef.SizeBytes,
				},
			},
		},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID:       config.BundleID,
			Revision: config.SHA256[:16],
			SHA256:   configRef.SHA256,
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowRef.SHA256,
		},
		RequestedOutput: []string{"findings", "report", "trace"},
		RemoteWrites:    "deny",
	}
	reviewSpecRef, err := repository.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		t.Fatalf("PutJSONArtifact(review spec) error = %v", err)
	}
	snapshot := runmodel.ExecutionSnapshot{
		SchemaVersion:         runmodel.SnapshotSchemaVersion,
		ExecutionSnapshotID:   "snapshot-1",
		ReviewSpecSHA256:      reviewSpecRef.SHA256,
		ReviewSpecRef:         reviewSpecRef,
		TargetSnapshotRef:     targetRef,
		ReviewInputRef:        inputRef,
		ReplayInputRefs:       []runmodel.ArtifactRef{},
		WorkflowDefinitionRef: workflowRef,
		ConfigBundleRef:       configRef,
		Workflow: runmodel.WorkflowRef{
			ID: definition.ID, Revision: definition.Revision,
			SHA256: workflowRef.SHA256,
		},
		Config: runmodel.PolicyRef{
			ID: config.BundleID, Revision: config.SHA256[:16],
			SHA256: config.SHA256,
		},
		RuntimeProfile: config.Execution.AgentProfile.ID + "@" +
			config.Execution.AgentProfile.Revision,
		BuildIdentity: "argus-test",
		ToolPolicy: runmodel.ToolInvocationPolicy{
			AllowedTools:       []string{},
			Network:            "deny",
			WorkspaceWrites:    "deny",
			RemoteWrites:       "deny",
			PerCallTimeoutMS:   30_000,
			MaxOutputBytes:     config.Budget.MaxOutputBytes,
			MaxConcurrency:     config.Budget.MaxConcurrency,
			MaxDelegationDepth: 0,
		},
		RemoteWrites: "deny",
		CreatedAt:    now,
	}
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot() error = %v", err)
	}
	run := runmodel.ReviewRun{
		SchemaVersion:       runmodel.RunSchemaVersion,
		RunID:               "run-1",
		Kind:                runmodel.RunKindReview,
		RepositoryPath:      "/tmp/repository",
		TargetMode:          runmodel.TargetModeDiff,
		BaseRevision:        baseOID,
		HeadRevision:        headOID,
		Status:              runmodel.RunStatusSucceeded,
		ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		TargetSnapshotRef:   targetRef,
		StageAttempts:       []runmodel.StageAttempt{},
		Bindings:            []runmodel.PlatformExecutionBinding{},
		Evidence:            []runmodel.RunEvidence{},
		FindingSetRef:       &findingRef,
		JSONReportRef:       &jsonReportRef,
		MarkdownReportRef:   &markdownReportRef,
		CreatedAt:           now,
		StartedAt:           &now,
		CompletedAt:         &now,
	}
	for index, stage := range result.Stages {
		stageID := string(stage.Stage)
		bindingID := "binding-" + stageID
		idempotencyKey := "run-1-" + stageID + "-1"
		inputRefs := []runmodel.ArtifactRef{inputRef}
		if index > 0 {
			inputRefs = append(inputRefs, stageRefs[index-1])
		}
		outputRef := stageRefs[index]
		run.StageAttempts = append(run.StageAttempts, runmodel.StageAttempt{
			StageID: stageID, Attempt: 1, Generation: 1, BindingID: bindingID,
			Status: runmodel.StageStatusSucceeded, InputRefs: inputRefs,
			OutputRef: &outputRef, StartedAt: now, FinishedAt: &now,
		})
		run.Bindings = append(run.Bindings, runmodel.PlatformExecutionBinding{
			BindingID: bindingID, RunID: run.RunID, StageID: stageID,
			Attempt: 1, Generation: 1, IdempotencyKey: idempotencyKey,
			FencingToken: uint64(index + 1), RuntimeKind: "local", RuntimeID: "runtime-1",
			CreatedAt: now,
		})
		run.Evidence = append(run.Evidence, runmodel.RunEvidence{
			EvidenceID: "evidence-" + stageID, BindingID: bindingID, StageID: stageID,
			Attempt: 1, Generation: 1, IdempotencyKey: idempotencyKey,
			FencingToken: uint64(index + 1), ArtifactRef: outputRef,
			Completeness: options.EvidenceCompleteness,
			CompletenessNotes: append(
				[]string{},
				options.EvidenceReasons...,
			),
			RecordedAt: now,
		})
		binding := run.Bindings[len(run.Bindings)-1]
		if err := repository.AppendEvent(
			run.RunID+"-binding-"+stageID, now, RunEvent{
				RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
				EventType: EventBindingRecorded, Binding: &binding,
			},
		); err != nil {
			t.Fatalf("AppendEvent(binding %s) error = %v", stageID, err)
		}
		evidence := run.Evidence[len(run.Evidence)-1]
		if err := repository.AppendEvent(
			run.RunID+"-evidence-"+stageID, now, RunEvent{
				RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
				EventType: EventEvidenceRecorded, Evidence: &evidence,
			},
		); err != nil {
			t.Fatalf("AppendEvent(evidence %s) error = %v", stageID, err)
		}
	}
	return run
}

func persistedReplayRun(
	t *testing.T,
	repository *Repository,
	source runmodel.ReviewRun,
	sourceRef runmodel.ArtifactRef,
	runID string,
	start reviewcore.StageName,
	now time.Time,
) (runmodel.ReviewRun, runmodel.ExecutionSnapshot) {
	t.Helper()
	sourceSnapshot, err := repository.LoadExecutionSnapshot(source.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot(source) error = %v", err)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := repository.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		t.Fatalf("ReadJSONArtifact(source ReviewSpec) error = %v", err)
	}
	sourceSpec.RequestID = runID
	sourceSpec.IdempotencyKey = runID
	specRef, err := repository.PutJSONArtifact(runmodel.ContractReviewSpec, sourceSpec)
	if err != nil {
		t.Fatalf("PutJSONArtifact(replay ReviewSpec) error = %v", err)
	}
	stageOrder := []reviewcore.StageName{
		reviewcore.StageMaterializeTarget,
		reviewcore.StagePlanContext,
		reviewcore.StageDetect,
		reviewcore.StageNormalize,
		reviewcore.StageVerify,
		reviewcore.StageAdjudicate,
		reviewcore.StageReport,
		reviewcore.StagePublish,
		reviewcore.StageCaptureFeedback,
		reviewcore.StageExportEvaluation,
	}
	startIndex := -1
	for index, stage := range stageOrder {
		if stage == start {
			startIndex = index
			break
		}
	}
	if startIndex < 0 {
		t.Fatalf("unsupported replay fixture start stage %q", start)
	}
	attemptByStage := make(map[string]runmodel.StageAttempt, len(source.StageAttempts))
	bindingByID := make(map[string]runmodel.PlatformExecutionBinding, len(source.Bindings))
	evidenceByBinding := make(map[string]runmodel.RunEvidence, len(source.Evidence))
	for _, attempt := range source.StageAttempts {
		attemptByStage[attempt.StageID] = attempt
	}
	for _, binding := range source.Bindings {
		bindingByID[binding.BindingID] = binding
	}
	for _, evidence := range source.Evidence {
		evidenceByBinding[evidence.BindingID] = evidence
	}
	checkpointByStage := make(map[string]runmodel.ArtifactRef, len(stageOrder))
	for index, ref := range sourceSnapshot.ReplayInputRefs {
		data, err := repository.ReadArtifact(ref)
		if err != nil {
			t.Fatalf("ReadArtifact(source replay checkpoint %d) error = %v", index, err)
		}
		result, err := reviewcore.DecodeStageResult(data)
		if err != nil {
			t.Fatalf("DecodeStageResult(source replay checkpoint %d) error = %v", index, err)
		}
		if _, duplicate := checkpointByStage[string(result.Stage)]; duplicate {
			t.Fatalf("source fixture duplicates %s checkpoint", result.Stage)
		}
		checkpointByStage[string(result.Stage)] = ref
	}
	for stageID, attempt := range attemptByStage {
		if attempt.Status != runmodel.StageStatusSucceeded || attempt.OutputRef == nil {
			continue
		}
		if _, duplicate := checkpointByStage[stageID]; duplicate {
			t.Fatalf("source fixture reuses and executes %s", stageID)
		}
		checkpointByStage[stageID] = *attempt.OutputRef
	}
	replayRefs := make([]runmodel.ArtifactRef, 0, startIndex)
	for _, stage := range stageOrder[:startIndex] {
		ref, exists := checkpointByStage[string(stage)]
		if !exists {
			t.Fatalf("source fixture has no %s output", stage)
		}
		replayRefs = append(replayRefs, ref)
	}
	replaySourceRef := sourceRef
	snapshot := sourceSnapshot
	snapshot.ExecutionSnapshotID = "snapshot-" + runID
	snapshot.ReviewSpecSHA256 = specRef.SHA256
	snapshot.ReviewSpecRef = specRef
	snapshot.ReplayInputRefs = replayRefs
	snapshot.ReplaySourceRunRef = &replaySourceRef
	rootRunID := source.RunID
	parentReplayRunID := ""
	if source.Kind == runmodel.RunKindReplay {
		rootRunID = source.ReplayRootRunID
		parentReplayRunID = source.RunID
	}
	change := runmodel.ReplayChangeSet{
		SchemaVersion:     runmodel.ReplayChangeSetSchemaVersion,
		Namespace:         runID,
		SourceRunID:       source.RunID,
		RootRunID:         rootRunID,
		ParentReplayRunID: parentReplayRunID,
		StartStage:        string(start),
		Variable:          runmodel.ReplayVariableNone,
		BaselineSHA256:    sourceSnapshot.Config.SHA256,
		VariantSHA256:     sourceSnapshot.Config.SHA256,
		ChangedFields:     []string{},
		RemoteWrites:      "deny",
		CreatedAt:         now,
	}
	changeRef, err := repository.PutJSONArtifact(
		runmodel.ContractReplayChangeSet,
		change,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(replay change set) error = %v", err)
	}
	snapshot.ReplayChangeSetRef = &changeRef
	snapshot.CreatedAt = now
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(replay) error = %v", err)
	}

	run := source
	run.RunID = runID
	run.Kind = runmodel.RunKindReplay
	run.SourceRunID = source.RunID
	run.ReplayFromStage = string(start)
	run.ReplayRootRunID = rootRunID
	run.ReplayNamespace = runID
	run.ReplayVariable = runmodel.ReplayVariableNone
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
	run.StageAttempts = []runmodel.StageAttempt{}
	run.Bindings = []runmodel.PlatformExecutionBinding{}
	run.Evidence = []runmodel.RunEvidence{}
	run.CreatedAt = now
	run.StartedAt = &now
	run.CompletedAt = &now
	for _, stage := range stageOrder[startIndex:] {
		sourceAttempt, exists := attemptByStage[string(stage)]
		if !exists {
			t.Fatalf("source fixture has no %s attempt", stage)
		}
		sourceBinding, exists := bindingByID[sourceAttempt.BindingID]
		if !exists {
			t.Fatalf("source fixture has no %s binding", stage)
		}
		sourceEvidence, exists := evidenceByBinding[sourceAttempt.BindingID]
		if !exists {
			t.Fatalf("source fixture has no %s evidence", stage)
		}
		bindingID := runID + "-binding-" + string(stage)
		idempotencyKey := runID + "-" + string(stage) + "-1"
		attempt := sourceAttempt
		attempt.BindingID = bindingID
		attempt.InputRefs = append([]runmodel.ArtifactRef(nil), sourceAttempt.InputRefs...)
		binding := sourceBinding
		binding.BindingID = bindingID
		binding.RunID = runID
		binding.IdempotencyKey = idempotencyKey
		binding.CreatedAt = now
		evidence := sourceEvidence
		evidence.EvidenceID = runID + "-evidence-" + string(stage)
		evidence.BindingID = bindingID
		evidence.IdempotencyKey = idempotencyKey
		evidence.RecordedAt = now
		run.StageAttempts = append(run.StageAttempts, attempt)
		run.Bindings = append(run.Bindings, binding)
		run.Evidence = append(run.Evidence, evidence)
		if err := repository.AppendEvent(
			runID+"-binding-"+string(stage),
			now,
			RunEvent{
				RunID: runID, Kind: runmodel.RunKindReplay,
				Status:    runmodel.RunStatusRunning,
				EventType: EventBindingRecorded, Binding: &binding,
			},
		); err != nil {
			t.Fatalf("AppendEvent(replay binding %s) error = %v", stage, err)
		}
		if err := repository.AppendEvent(
			runID+"-evidence-"+string(stage),
			now,
			RunEvent{
				RunID: runID, Kind: runmodel.RunKindReplay,
				Status:    runmodel.RunStatusRunning,
				EventType: EventEvidenceRecorded, Evidence: &evidence,
			},
		); err != nil {
			t.Fatalf("AppendEvent(replay evidence %s) error = %v", stage, err)
		}
	}
	return run, snapshot
}

func putArtifact(
	t *testing.T,
	repository *Repository,
	contract string,
	content string,
) runmodel.ArtifactRef {
	t.Helper()
	ref, err := repository.PutArtifact(contract, []byte(content))
	if err != nil {
		t.Fatalf("PutArtifact(%s) error = %v", contract, err)
	}
	return ref
}

func replaceRunTargetSnapshot(
	t *testing.T,
	repository *Repository,
	run *runmodel.ReviewRun,
	targetRef runmodel.ArtifactRef,
) {
	t.Helper()
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	snapshot.ExecutionSnapshotID = "snapshot-target-" + targetRef.SHA256[:12]
	snapshot.TargetSnapshotRef = targetRef
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(replacement) error = %v", err)
	}
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
	run.TargetSnapshotRef = targetRef
}

func replaceRunConfigBundle(
	t *testing.T,
	repository *Repository,
	run *runmodel.ReviewRun,
	mutate func(*reviewconfig.ConfigBundle),
) {
	t.Helper()
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	var config reviewconfig.ConfigBundle
	if err := repository.ReadJSONArtifact(snapshot.ConfigBundleRef, &config); err != nil {
		t.Fatalf("ReadJSONArtifact(config bundle) error = %v", err)
	}
	mutate(&config)
	config.SHA256 = ""
	config.BundleID = ""
	digest, err := reviewconfig.DigestBundle(config)
	if err != nil {
		t.Fatalf("DigestBundle(config) error = %v", err)
	}
	config.SHA256 = digest
	config.BundleID = "bundle-" + digest[:24]
	if err := config.Validate(); err != nil {
		t.Fatalf("mutated ConfigBundle.Validate() error = %v", err)
	}
	configRef, err := repository.PutJSONArtifact(runmodel.ContractConfigBundle, config)
	if err != nil {
		t.Fatalf("PutJSONArtifact(config bundle) error = %v", err)
	}

	var spec contractsv1alpha1.ReviewSpec
	if err := repository.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		t.Fatalf("ReadJSONArtifact(ReviewSpec) error = %v", err)
	}
	spec.ConfigBundleRef = contractsv1alpha1.VersionedRef{
		ID:       config.BundleID,
		Revision: config.SHA256[:16],
		SHA256:   configRef.SHA256,
	}
	specRef, err := repository.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		t.Fatalf("PutJSONArtifact(ReviewSpec) error = %v", err)
	}

	snapshot.ExecutionSnapshotID = "snapshot-config-" + configRef.SHA256[:12]
	snapshot.ReviewSpecRef = specRef
	snapshot.ReviewSpecSHA256 = specRef.SHA256
	snapshot.ConfigBundleRef = configRef
	snapshot.Config = runmodel.PolicyRef{
		ID:       config.BundleID,
		Revision: config.SHA256[:16],
		SHA256:   config.SHA256,
	}
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(config replacement) error = %v", err)
	}
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
}

func replaceRunWithSelectionTarget(
	t *testing.T,
	repository *Repository,
	run *runmodel.ReviewRun,
) {
	t.Helper()
	snapshot, target, input, spec := readFrozenClosure(t, repository, *run)
	file := target.FileRefs[0]
	if file.ContentRef == nil {
		t.Fatal("selection fixture requires retained file content")
	}
	content, err := repository.ReadArtifact(*file.ContentRef)
	if err != nil {
		t.Fatalf("ReadArtifact(selection file) error = %v", err)
	}
	selectionRef, err := repository.PutArtifact(
		targetmodel.ContractSelectionContent,
		content,
	)
	if err != nil {
		t.Fatalf("PutArtifact(selection content) error = %v", err)
	}
	revision := target.Snapshot.Head
	manifest := targetmodel.SelectionManifest{
		SchemaVersion:          targetmodel.ContractSelectionManifest,
		Repository:             target.Snapshot.Repository,
		Revision:               revision,
		Path:                   file.Path,
		StartLine:              1,
		EndLine:                1,
		SourceKind:             targetmodel.SelectionSourceCommit,
		FileSHA256:             file.SHA256,
		FileSizeBytes:          file.SizeBytes,
		SelectionContentSHA256: selectionRef.SHA256,
		SelectionSizeBytes:     selectionRef.SizeBytes,
		Completeness:           gitadapter.CompletenessComplete,
		Reasons:                []gitadapter.Reason{},
	}
	manifestRef, err := repository.PutJSONArtifact(
		targetmodel.ContractSelectionManifest,
		manifest,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(selection manifest) error = %v", err)
	}
	targetSnapshot, err := targetmodel.SealTargetSnapshot(targetmodel.TargetSnapshot{
		SchemaVersion:  targetmodel.TargetSnapshotSchemaVersion,
		Mode:           reviewcore.TargetModeSelection,
		Repository:     target.Snapshot.Repository,
		Base:           revision,
		Head:           revision,
		ManifestSHA256: manifestRef.SHA256,
		Selection: &targetmodel.SelectionSnapshot{
			Path:                   file.Path,
			StartLine:              1,
			EndLine:                1,
			SourceKind:             targetmodel.SelectionSourceCommit,
			FileSHA256:             file.SHA256,
			FileSizeBytes:          file.SizeBytes,
			SelectionContentSHA256: selectionRef.SHA256,
			SelectionSizeBytes:     selectionRef.SizeBytes,
		},
		DirtyState:         target.Snapshot.DirtyState,
		Completeness:       gitadapter.CompletenessComplete,
		CompletenessReason: []gitadapter.Reason{},
		CapturedAt:         target.Snapshot.CapturedAt,
		CapturedBy:         target.Snapshot.CapturedBy,
		GitVersion:         target.Snapshot.GitVersion,
	})
	if err != nil {
		t.Fatalf("SealTargetSnapshot(selection) error = %v", err)
	}
	target = targetmodel.MaterializedTarget{
		SchemaVersion:       targetmodel.MaterializedTargetSchemaVersion,
		Snapshot:            targetSnapshot,
		ManifestRef:         manifestRef,
		SelectionContentRef: &selectionRef,
		FileRefs:            target.FileRefs,
		Contexts:            target.Contexts,
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("selection MaterializedTarget.Validate() error = %v", err)
	}
	targetRef, err := repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		target,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(selection target) error = %v", err)
	}

	input.TargetID = targetSnapshot.TargetSnapshotID
	input.TargetMode = reviewcore.TargetModeSelection
	input.CanonicalPatch = ""
	input.Regions = []reviewcore.ReviewRegion{{
		Path: file.Path, StartLine: 1, EndLine: 1, SHA256: file.SHA256,
	}}
	if err := input.Validate(); err != nil {
		t.Fatalf("selection ReviewInput.Validate() error = %v", err)
	}
	inputRef, err := repository.PutJSONArtifact(runmodel.ContractReviewInput, input)
	if err != nil {
		t.Fatalf("PutJSONArtifact(selection input) error = %v", err)
	}
	spec.Target = contractsv1alpha1.ReviewTarget{
		Mode: contractsv1alpha1.ReviewModeSelection,
		Selection: &contractsv1alpha1.SelectionTarget{
			Revision: revision.CommitOID,
			Path:     file.Path, StartLine: 1, EndLine: 1,
			Content: contractsv1alpha1.ContentRef{
				URI: selectionRef.URI, SHA256: selectionRef.SHA256,
				SizeBytes: selectionRef.SizeBytes,
			},
		},
	}
	specRef, err := repository.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		t.Fatalf("PutJSONArtifact(selection ReviewSpec) error = %v", err)
	}

	snapshot.ExecutionSnapshotID = "snapshot-selection-" + targetRef.SHA256[:12]
	snapshot.TargetSnapshotRef = targetRef
	snapshot.ReviewInputRef = inputRef
	snapshot.ReviewSpecRef = specRef
	snapshot.ReviewSpecSHA256 = specRef.SHA256
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(selection) error = %v", err)
	}
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
	run.TargetSnapshotRef = targetRef
	run.TargetMode = runmodel.TargetModeSelection
	run.BaseRevision = revision.CommitOID
	run.HeadRevision = revision.CommitOID
}

func replaceRunWithScopeTarget(
	t *testing.T,
	repository *Repository,
	run *runmodel.ReviewRun,
) {
	t.Helper()
	snapshot, target, input, spec := readFrozenClosure(t, repository, *run)
	file := target.FileRefs[0]
	revision := target.Snapshot.Head
	manifest := targetmodel.ScopeManifest{
		SchemaVersion: targetmodel.ContractScopeManifest,
		Repository:    target.Snapshot.Repository,
		Revision:      revision,
		Include:       []string{"**"},
		Exclude:       []string{},
		Files: []targetmodel.ScopeManifestFile{{
			Path: file.Path, Mode: "100644", ObjectType: "blob",
			ObjectOID: strings.Repeat("c", 40), Language: "go",
			SHA256: file.SHA256, SizeBytes: file.SizeBytes,
			Completeness: gitadapter.CompletenessComplete,
			Reasons:      []gitadapter.Reason{},
		}},
		Coverage: targetmodel.ScopeCoverage{
			ScannedFiles: 1, MatchedFiles: 1, IncludedFiles: 1,
			SkippedFiles: 0, ExcludedFiles: 0,
		},
		Completeness: gitadapter.CompletenessComplete,
		Reasons:      []gitadapter.Reason{},
	}
	manifestRef, err := repository.PutJSONArtifact(
		targetmodel.ContractScopeManifest,
		manifest,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(scope manifest) error = %v", err)
	}
	targetSnapshot, err := targetmodel.SealTargetSnapshot(targetmodel.TargetSnapshot{
		SchemaVersion:  targetmodel.TargetSnapshotSchemaVersion,
		Mode:           reviewcore.TargetModeScope,
		Repository:     target.Snapshot.Repository,
		Base:           revision,
		Head:           revision,
		ManifestSHA256: manifestRef.SHA256,
		Scope: &targetmodel.ScopeSnapshot{
			Include: []string{"**"}, Exclude: []string{},
			MatchedFiles: 1, IncludedFiles: 1, SkippedFiles: 0,
		},
		DirtyState:         target.Snapshot.DirtyState,
		Completeness:       gitadapter.CompletenessComplete,
		CompletenessReason: []gitadapter.Reason{},
		CapturedAt:         target.Snapshot.CapturedAt,
		CapturedBy:         target.Snapshot.CapturedBy,
		GitVersion:         target.Snapshot.GitVersion,
	})
	if err != nil {
		t.Fatalf("SealTargetSnapshot(scope) error = %v", err)
	}
	target = targetmodel.MaterializedTarget{
		SchemaVersion: targetmodel.MaterializedTargetSchemaVersion,
		Snapshot:      targetSnapshot,
		ManifestRef:   manifestRef,
		FileRefs:      target.FileRefs,
		Contexts:      target.Contexts,
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("scope MaterializedTarget.Validate() error = %v", err)
	}
	targetRef, err := repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		target,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(scope target) error = %v", err)
	}

	input.TargetID = targetSnapshot.TargetSnapshotID
	input.TargetMode = reviewcore.TargetModeScope
	input.CanonicalPatch = ""
	input.Regions = []reviewcore.ReviewRegion{{
		Path: file.Path, StartLine: 1, EndLine: 1, SHA256: file.SHA256,
	}}
	if err := input.Validate(); err != nil {
		t.Fatalf("scope ReviewInput.Validate() error = %v", err)
	}
	inputRef, err := repository.PutJSONArtifact(runmodel.ContractReviewInput, input)
	if err != nil {
		t.Fatalf("PutJSONArtifact(scope input) error = %v", err)
	}
	spec.Target = contractsv1alpha1.ReviewTarget{
		Mode: contractsv1alpha1.ReviewModeScope,
		Scope: &contractsv1alpha1.ScopeTarget{
			Revision: revision.CommitOID,
			Include:  []string{"**"},
			Exclude:  []string{},
		},
	}
	specRef, err := repository.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		t.Fatalf("PutJSONArtifact(scope ReviewSpec) error = %v", err)
	}

	snapshot.ExecutionSnapshotID = "snapshot-scope-" + targetRef.SHA256[:12]
	snapshot.TargetSnapshotRef = targetRef
	snapshot.ReviewInputRef = inputRef
	snapshot.ReviewSpecRef = specRef
	snapshot.ReviewSpecSHA256 = specRef.SHA256
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(scope) error = %v", err)
	}
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
	run.TargetSnapshotRef = targetRef
	run.TargetMode = runmodel.TargetModeScope
	run.BaseRevision = revision.CommitOID
	run.HeadRevision = revision.CommitOID
}

func replaceRunDiffManifest(
	t *testing.T,
	repository *Repository,
	run *runmodel.ReviewRun,
	mutate func(*gitadapter.ChangeManifest, *targetmodel.MaterializedTarget),
) {
	t.Helper()
	snapshot, target, input, _ := readFrozenClosure(t, repository, *run)
	var manifest gitadapter.ChangeManifest
	if err := repository.ReadJSONArtifact(target.ManifestRef, &manifest); err != nil {
		t.Fatalf("ReadJSONArtifact(change manifest) error = %v", err)
	}
	mutate(&manifest, &target)
	manifestRef, err := repository.PutJSONArtifact(
		targetmodel.ContractChangeManifest,
		manifest,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(change manifest) error = %v", err)
	}
	target.Snapshot.ManifestSHA256 = manifestRef.SHA256
	target.Snapshot, err = targetmodel.SealTargetSnapshot(target.Snapshot)
	if err != nil {
		t.Fatalf("SealTargetSnapshot(diff replacement) error = %v", err)
	}
	target.ManifestRef = manifestRef
	if err := targetmodel.ValidateDiffManifest(manifest, target.Snapshot); err != nil {
		t.Fatalf("replacement diff manifest should remain valid: %v", err)
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("replacement MaterializedTarget.Validate() error = %v", err)
	}
	targetRef, err := repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		target,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(diff target) error = %v", err)
	}
	input.TargetID = target.Snapshot.TargetSnapshotID
	inputRef, err := repository.PutJSONArtifact(runmodel.ContractReviewInput, input)
	if err != nil {
		t.Fatalf("PutJSONArtifact(diff input) error = %v", err)
	}
	snapshot.ExecutionSnapshotID = "snapshot-diff-" + targetRef.SHA256[:12]
	snapshot.TargetSnapshotRef = targetRef
	snapshot.ReviewInputRef = inputRef
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(diff replacement) error = %v", err)
	}
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
	run.TargetSnapshotRef = targetRef
}

func appendCompleteTargetFile(
	t *testing.T,
	repository *Repository,
	run *runmodel.ReviewRun,
	filePath string,
	content string,
) {
	t.Helper()
	snapshot, target, _, _ := readFrozenClosure(t, repository, *run)
	contentRef, err := repository.PutArtifact(
		targetmodel.ContractFileContent,
		[]byte(content),
	)
	if err != nil {
		t.Fatalf("PutArtifact(additional file content) error = %v", err)
	}
	target.FileRefs = append(target.FileRefs, targetmodel.TargetFileRef{
		Path: filePath, Status: gitadapter.ChangeAdded,
		SHA256: contentRef.SHA256, SizeBytes: contentRef.SizeBytes,
		ContentRef: &contentRef, Completeness: gitadapter.CompletenessComplete,
		Reasons: []gitadapter.Reason{},
	})
	if err := target.Validate(); err != nil {
		t.Fatalf("expanded MaterializedTarget.Validate() error = %v", err)
	}
	targetRef, err := repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		target,
	)
	if err != nil {
		t.Fatalf("PutJSONArtifact(expanded target) error = %v", err)
	}
	snapshot.ExecutionSnapshotID = "snapshot-files-" + targetRef.SHA256[:12]
	snapshot.TargetSnapshotRef = targetRef
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot(expanded target) error = %v", err)
	}
	run.ExecutionSnapshotID = snapshot.ExecutionSnapshotID
	run.TargetSnapshotRef = targetRef
}

func readFrozenClosure(
	t *testing.T,
	repository *Repository,
	run runmodel.ReviewRun,
) (
	runmodel.ExecutionSnapshot,
	targetmodel.MaterializedTarget,
	reviewcore.ReviewInput,
	contractsv1alpha1.ReviewSpec,
) {
	t.Helper()
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	var target targetmodel.MaterializedTarget
	if err := repository.ReadJSONArtifact(snapshot.TargetSnapshotRef, &target); err != nil {
		t.Fatalf("ReadJSONArtifact(materialized target) error = %v", err)
	}
	var input reviewcore.ReviewInput
	if err := repository.ReadJSONArtifact(snapshot.ReviewInputRef, &input); err != nil {
		t.Fatalf("ReadJSONArtifact(review input) error = %v", err)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := repository.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		t.Fatalf("ReadJSONArtifact(ReviewSpec) error = %v", err)
	}
	return snapshot, target, input, spec
}

func testReviewInput() reviewcore.ReviewInput {
	content := "package a\n"
	digest := sha256.Sum256([]byte(content))
	return reviewcore.ReviewInput{
		SchemaVersion: reviewcore.ReviewInputSchemaVersion,
		TargetID:      "target-placeholder",
		TargetMode:    reviewcore.TargetModeDiff,
		CanonicalPatch: "diff --git a/a.go b/a.go\n" +
			"new file mode 100644\n" +
			"--- /dev/null\n" +
			"+++ b/a.go\n" +
			"@@ -0,0 +1 @@\n" +
			"+package a\n",
		Regions:  []reviewcore.ReviewRegion{},
		Contexts: []reviewcore.ContextBinding{},
		Files: []reviewcore.FileManifestEntry{{
			Path: "a.go", SHA256: formatDigest(digest),
			SizeBytes: int64(len(content)), Content: &content,
		}},
	}
}

func formatDigest(digest [sha256.Size]byte) string {
	const hexadecimal = "0123456789abcdef"
	encoded := make([]byte, sha256.Size*2)
	for index, value := range digest {
		encoded[index*2] = hexadecimal[value>>4]
		encoded[index*2+1] = hexadecimal[value&0x0f]
	}
	return string(encoded)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(content)
}
