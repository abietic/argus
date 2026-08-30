package agentanalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/agentshadow"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var agentAnalyticsTestDigest = strings.Repeat("a", 64)

type fakeCommittedSource struct {
	results  []agentshadow.Result
	err      error
	requests []agentshadow.ListRequest
}

type fakeAttemptSource struct {
	*fakeCommittedSource
	attempts        []agentshadow.ExecutionAttempt
	attemptErr      error
	attemptRequests []agentshadow.ExecutionListRequest
	queryResults    []agentshadow.Result
	queryAttempts   []agentshadow.ExecutionAttempt
}

func (source *fakeAttemptSource) ListExecutions(
	_ context.Context,
	request agentshadow.ExecutionListRequest,
) ([]agentshadow.ExecutionAttempt, error) {
	source.attemptRequests = append(source.attemptRequests, request)
	if source.attemptErr != nil {
		return nil, source.attemptErr
	}
	return append([]agentshadow.ExecutionAttempt(nil), source.attempts...), nil
}

func (source *fakeAttemptSource) Query(
	_ context.Context,
	request agentshadow.QueryRequest,
) (agentshadow.Result, error) {
	for _, result := range append(
		append([]agentshadow.Result(nil), source.results...),
		source.queryResults...,
	) {
		if result.Record.TenantID == request.Scope.TenantID &&
			result.Record.WorkspaceID == request.Scope.WorkspaceID &&
			result.Manifest.ManifestID == request.ManifestID {
			return result, nil
		}
	}
	return agentshadow.Result{}, agentshadow.ErrNotFound
}

func (source *fakeAttemptSource) QueryExecution(
	_ context.Context,
	request agentshadow.ExecutionQueryRequest,
) (agentshadow.ExecutionAttempt, error) {
	for _, attempt := range append(
		append([]agentshadow.ExecutionAttempt(nil), source.attempts...),
		source.queryAttempts...,
	) {
		if attempt.Scope == request.Scope && attempt.ExecutionID == request.ExecutionID {
			return attempt, nil
		}
	}
	return agentshadow.ExecutionAttempt{}, agentshadow.ErrNotFound
}

func (source *fakeCommittedSource) List(
	_ context.Context,
	request agentshadow.ListRequest,
) ([]agentshadow.Result, error) {
	source.requests = append(source.requests, request)
	if source.err != nil {
		return nil, source.err
	}
	return append([]agentshadow.Result(nil), source.results...), nil
}

func TestRebuildQueryAndExportMixedDiagnosticExecutions(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	source := &fakeCommittedSource{results: []agentshadow.Result{
		testCommittedResult(t, "failure", window.StartInclusive.Add(30*time.Minute), "failure"),
		testCommittedResult(t, "clean", window.StartInclusive.Add(10*time.Minute), "clean"),
		testCommittedResult(t, "partial", window.StartInclusive.Add(20*time.Minute), "partial"),
	}}
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	request := RebuildRequest{
		SnapshotID: "mixed-agent-executions",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     window,
		GroupBy:    []DimensionName{DimensionModel},
		BuiltAt:    window.EndExclusive.Add(time.Minute),
	}

	first, err := adapter.Rebuild(t.Context(), request)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	second, err := adapter.Rebuild(t.Context(), request)
	if err != nil {
		t.Fatalf("idempotent Rebuild() error = %v", err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("idempotent rebuild changed immutable snapshot")
	}
	if len(source.requests) != 2 ||
		!source.requests[0].StartInclusive.Equal(window.StartInclusive) ||
		!source.requests[0].EndExclusive.Equal(window.EndExclusive) {
		t.Fatalf("source window requests = %+v", source.requests)
	}

	queryOnly, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	queried, err := queryOnly.Query(request.SnapshotID)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if !reflect.DeepEqual(queried, first) {
		t.Fatalf("queried snapshot differs from committed snapshot")
	}
	if len(queried.Facts.Executions) != 3 || len(queried.Facts.Tasks) != 6 ||
		len(queried.Facts.ToolUsage) != 9 {
		t.Fatalf("fact cardinalities = (%d, %d, %d)",
			len(queried.Facts.Executions),
			len(queried.Facts.Tasks),
			len(queried.Facts.ToolUsage),
		)
	}
	for _, fact := range queried.Facts.Executions {
		if fact.Model.ID != "deepseek-v4-pro[1m]" {
			t.Fatalf("opaque model id was rejected or changed: %+v", fact.Model)
		}
	}
	for metricID, want := range map[string]uint64{
		"agent_execution.total.count":    3,
		"agent_execution.complete.count": 1,
		"agent_execution.partial.count":  1,
		"agent_execution.failed.count":   1,
	} {
		tile := requireMetricTile(t, queried.Projection, metricID)
		if tile.Value == nil || *tile.Value != want ||
			tile.Qualifier != QualifierHostObserved {
			t.Fatalf("tile %q = %+v, want %d host-observed", metricID, tile, want)
		}
	}
	usage := requireMetricTile(t, queried.Projection, "agent_usage.input_tokens")
	if usage.Value == nil || *usage.Value != 310 || usage.SampleSize != 6 ||
		usage.Qualifier != QualifierLowerBound {
		t.Fatalf("mixed usage tile = %+v, want 310 lower-bound over six receipts", usage)
	}
	for _, tile := range queried.Projection.Tiles {
		if strings.Contains(tile.MetricID, "cost") {
			t.Fatalf("diagnostic projection estimated cost: %+v", tile)
		}
	}

	for _, test := range []struct {
		target ExportTarget
		format string
	}{
		{ExportFacts, CanonicalJSONExportFormat},
		{ExportFacts, CanonicalCSVExportFormat},
		{ExportProjection, CanonicalJSONExportFormat},
		{ExportProjection, CanonicalCSVExportFormat},
	} {
		bundle, exportErr := queryOnly.Export(request.SnapshotID, test.target, test.format)
		if exportErr != nil {
			t.Fatalf("Export(%q, %q) error = %v", test.target, test.format, exportErr)
		}
		if exportErr = bundle.Validate(); exportErr != nil {
			t.Fatalf("Export(%q, %q) invalid = %v", test.target, test.format, exportErr)
		}
		if test.target == ExportFacts && test.format == CanonicalJSONExportFormat {
			payload := string(bundle.Files[0].Data)
			if strings.Contains(payload, "\"raw_candidates\"") ||
				strings.Contains(payload, "finding_funnel") ||
				strings.Contains(payload, "value_observation") {
				t.Fatalf("diagnostic export polluted the formal finding/value funnel: %s", payload)
			}
		}
	}
}

func TestRebuildIncludesTerminalAndUnknownExecutionAttempts(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	success := testCommittedResult(t, "attempt-success", window.StartInclusive.Add(10*time.Minute), "clean")
	failedPlan := testCommittedResult(t, "attempt-failed", window.StartInclusive.Add(20*time.Minute), "clean")
	canceledPlan := testCommittedResult(t, "attempt-canceled", window.StartInclusive.Add(30*time.Minute), "clean")
	unknownPlan := testCommittedResult(t, "attempt-unknown", window.StartInclusive.Add(40*time.Minute), "clean")
	successAttempt := testExecutionAttempt(success, agentshadow.ExecutionStatusSucceeded, window.StartInclusive.Add(11*time.Minute))
	source := &fakeAttemptSource{
		// unknownPlan models the crash window where Import committed a result
		// but completeExecution did not. The immutable intent remains the
		// analytics authority, so that result must not fabricate success.
		fakeCommittedSource: &fakeCommittedSource{results: []agentshadow.Result{success, unknownPlan}},
		attempts: []agentshadow.ExecutionAttempt{
			successAttempt,
			testExecutionAttempt(failedPlan, agentshadow.ExecutionStatusFailed, window.StartInclusive.Add(21*time.Minute)),
			testExecutionAttempt(canceledPlan, agentshadow.ExecutionStatusCanceled, window.StartInclusive.Add(31*time.Minute)),
			testExecutionAttempt(unknownPlan, agentshadow.ExecutionStatusUnknownOutcome, unknownPlan.Plan.CreatedAt),
		},
	}
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Rebuild(t.Context(), RebuildRequest{
		SnapshotID: "execution-attempt-outcomes",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if len(snapshot.Facts.Executions) != 4 || len(snapshot.Facts.Tasks) != 2 ||
		len(snapshot.Facts.ToolUsage) != 4 || len(snapshot.SourceBindings) != 4 {
		t.Fatalf("attempt fact cardinalities = (%d,%d,%d,%d)",
			len(snapshot.Facts.Executions), len(snapshot.Facts.Tasks),
			len(snapshot.Facts.ToolUsage), len(snapshot.SourceBindings))
	}
	for metricID, want := range map[string]uint64{
		"agent_execution.total.count":           4,
		"agent_execution.complete.count":        1,
		"agent_execution.failed.count":          1,
		"agent_execution.canceled.count":        1,
		"agent_execution.unknown_outcome.count": 1,
	} {
		tile := requireMetricTile(t, snapshot.Projection, metricID)
		if tile.Value == nil || *tile.Value != want {
			t.Fatalf("metric %q = %+v, want %d", metricID, tile, want)
		}
	}
	duration := requireMetricTile(t, snapshot.Projection, "agent_execution.duration.p50")
	if duration.SampleSize != 1 {
		t.Fatalf("result-less attempts polluted worker duration sample: %+v", duration)
	}
	for _, fact := range snapshot.Facts.Executions {
		if fact.ExecutionID == success.Plan.ExecutionID {
			if !fact.ObservedAt.Equal(successAttempt.ObservedAt) || fact.Receipts != 2 {
				t.Fatalf("succeeded attempt lost rich result or attempt window: %+v", fact)
			}
			continue
		}
		if fact.ManifestID != "" || fact.ObservationID != "" || fact.Receipts != 0 {
			t.Fatalf("result-less attempt fabricated result evidence: %+v", fact)
		}
	}
	if _, err := ExportFactSetCSV(snapshot.Facts); err != nil {
		t.Fatalf("CSV export rejected attempt facts: %v", err)
	}
	if len(source.attemptRequests) != 1 ||
		!source.attemptRequests[0].StartInclusive.Equal(window.StartInclusive) ||
		!source.attemptRequests[0].EndExclusive.Equal(window.EndExclusive) {
		t.Fatalf("attempt source requests = %+v", source.attemptRequests)
	}
}

func TestUnknownAttemptBindsBoundedHostFailureDiagnosis(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	result := testCommittedResult(
		t,
		"host-failure-diagnosis",
		window.StartInclusive.Add(10*time.Minute),
		"clean",
	)
	attempt := testExecutionAttempt(
		result,
		agentshadow.ExecutionStatusUnknownOutcome,
		result.Plan.CreatedAt,
	)
	hostObservedAt := attempt.AcceptedAt.Add(5 * time.Second)
	hostFailure := agentshadow.ExecutionHostFailureObservation{
		SchemaVersion:  agentshadow.ExecutionHostFailureObservationSchemaVersion,
		Scope:          attempt.Scope,
		IdempotencyKey: attempt.IdempotencyKey,
		SemanticDigest: attempt.SemanticDigest,
		ExecutionID:    attempt.ExecutionID,
		IntentSHA256:   attempt.IntentSHA256,
		Stage:          agentshadow.ExecutionHostFailureWorkerRun,
		ReasonCode:     agentshadow.ExecutionHostFailureRunnerUnconfirmed,
		ObservedAt:     hostObservedAt,
		TimeSource:     agentshadow.ExecutionHostObservationClock,
	}
	hostData, err := json.Marshal(hostFailure)
	if err != nil {
		t.Fatal(err)
	}
	hostDigest := digestBytes(hostData)
	attempt.HostFailure = &hostFailure
	attempt.HostFailureObservationSHA256 = &hostDigest
	attempt.ObservedAt = hostObservedAt
	if err := attempt.Validate(); err != nil {
		t.Fatalf("host failure attempt fixture: %v", err)
	}

	request := RebuildRequest{
		SnapshotID: "host-failure-diagnosis",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     window,
		GroupBy:    []DimensionName{},
		BuiltAt:    window.EndExclusive,
	}
	facts, bindings, err := buildFactsWithAttempts(
		request,
		nil,
		[]agentshadow.ExecutionAttempt{attempt},
	)
	if err != nil {
		t.Fatalf("build host failure fact: %v", err)
	}
	if len(facts.Executions) != 1 ||
		!reflect.DeepEqual(
			facts.Executions[0].ReasonCodes,
			[]string{string(agentshadow.ExecutionHostFailureRunnerUnconfirmed)},
		) {
		t.Fatalf("host failure reason fact = %+v", facts.Executions)
	}
	if len(bindings) != 1 || bindings[0].Attempt == nil ||
		bindings[0].Attempt.HostFailure == nil ||
		bindings[0].Attempt.HostFailure.ObservationSHA256 != hostDigest ||
		bindings[0].Attempt.HostFailure.Observation.Stage !=
			agentshadow.ExecutionHostFailureWorkerRun {
		t.Fatalf("host failure source binding = %+v", bindings)
	}
	if err := bindings[0].reconcile(facts.Executions[0]); err != nil {
		t.Fatalf("host failure source did not reconcile: %v", err)
	}

	tampered := bindings[0]
	copyAttempt := *tampered.Attempt
	copyHostFailure := *copyAttempt.HostFailure
	copyHostFailure.Observation.Stage = agentshadow.ExecutionHostFailureShadowImport
	copyHostFailure.Observation.ReasonCode = agentshadow.ExecutionHostFailureImportUnconfirmed
	copyAttempt.HostFailure = &copyHostFailure
	tampered.Attempt = &copyAttempt
	if err := tampered.Validate(); err == nil {
		t.Fatal("source binding accepted a valid-pair diagnosis with a stale observation digest")
	}
}

func TestTerminalAttemptKeepsCompletionWindowWhenHostDiagnosisIsLater(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	result := testCommittedResult(
		t,
		"terminal-late-host-diagnosis",
		window.StartInclusive.Add(10*time.Minute),
		"clean",
	)
	attempt := testExecutionAttempt(
		result,
		agentshadow.ExecutionStatusSucceeded,
		result.Record.AcceptedAt,
	)
	terminalObservedAt := attempt.ObservedAt
	hostFailure := agentshadow.ExecutionHostFailureObservation{
		SchemaVersion:  agentshadow.ExecutionHostFailureObservationSchemaVersion,
		Scope:          attempt.Scope,
		IdempotencyKey: attempt.IdempotencyKey,
		SemanticDigest: attempt.SemanticDigest,
		ExecutionID:    attempt.ExecutionID,
		IntentSHA256:   attempt.IntentSHA256,
		Stage:          agentshadow.ExecutionHostFailureCompletionCommit,
		ReasonCode:     agentshadow.ExecutionHostFailureCompletionUnconfirmed,
		ObservedAt:     terminalObservedAt.Add(time.Minute),
		TimeSource:     agentshadow.ExecutionHostObservationClock,
	}
	hostData, err := json.Marshal(hostFailure)
	if err != nil {
		t.Fatal(err)
	}
	hostDigest := digestBytes(hostData)
	attempt.HostFailure = &hostFailure
	attempt.HostFailureObservationSHA256 = &hostDigest
	if err := attempt.Validate(); err != nil {
		t.Fatalf("terminal host failure fixture: %v", err)
	}
	request := RebuildRequest{
		SnapshotID: "terminal-late-host-diagnosis",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     window,
		GroupBy:    []DimensionName{},
		BuiltAt:    window.EndExclusive,
	}
	facts, bindings, err := buildFactsWithAttempts(
		request,
		[]agentshadow.Result{result},
		[]agentshadow.ExecutionAttempt{attempt},
	)
	if err != nil {
		t.Fatalf("build terminal host diagnosis: %v", err)
	}
	if len(facts.Executions) != 1 ||
		!facts.Executions[0].ObservedAt.Equal(terminalObservedAt) ||
		len(bindings) != 1 || bindings[0].Attempt == nil ||
		bindings[0].Attempt.HostFailure == nil ||
		!bindings[0].Attempt.HostFailure.Observation.ObservedAt.After(terminalObservedAt) {
		t.Fatalf("terminal host diagnosis changed execution window: %+v %+v", facts, bindings)
	}
	if err := bindings[0].Validate(); err != nil {
		t.Fatalf("terminal host diagnosis source binding: %v", err)
	}
}

func TestExecutionAttemptFactsRejectTamperDuplicateAndWindowLeak(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	result := testCommittedResult(t, "attempt-reject", window.StartInclusive.Add(10*time.Minute), "clean")
	valid := testExecutionAttempt(result, agentshadow.ExecutionStatusFailed, window.StartInclusive.Add(11*time.Minute))
	request := RebuildRequest{
		SnapshotID: "attempt-rejections",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
	}
	for name, attempts := range map[string][]agentshadow.ExecutionAttempt{
		"tampered intent digest": func() []agentshadow.ExecutionAttempt {
			tampered := valid
			tampered.IntentSHA256 = "tampered"
			return []agentshadow.ExecutionAttempt{tampered}
		}(),
		"duplicate execution": {valid, valid},
		"window leak": func() []agentshadow.ExecutionAttempt {
			outside := valid
			outside.ObservedAt = window.EndExclusive
			completed := window.EndExclusive
			outside.CompletedAt = &completed
			return []agentshadow.ExecutionAttempt{outside}
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := buildFactsWithAttempts(request, nil, attempts); err == nil {
				t.Fatal("tampered attempt source was accepted")
			}
		})
	}
	facts, bindings, err := buildFactsWithAttempts(request, nil, []agentshadow.ExecutionAttempt{valid})
	if err != nil {
		t.Fatal(err)
	}
	binding := bindings[0]
	resultBinding := ResultSourceBinding{}
	binding.Result = &resultBinding
	if err := binding.Validate(); err == nil {
		t.Fatal("source binding accepted both strict union arms")
	}
	if len(facts.Executions) != 1 {
		t.Fatalf("valid failure facts = %+v", facts)
	}
}

func TestAttemptWindowOwnsSucceededExecutionAcrossImportBoundary(t *testing.T) {
	attemptWindow := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
	}
	importWindow := TimeWindow{
		StartInclusive: attemptWindow.EndExclusive,
		EndExclusive:   attemptWindow.EndExclusive.Add(time.Hour),
	}
	result := testCommittedResult(
		t,
		"attempt-boundary",
		importWindow.StartInclusive.Add(time.Second),
		"clean",
	)
	attempt := testExecutionAttempt(
		result,
		agentshadow.ExecutionStatusSucceeded,
		attemptWindow.EndExclusive.Add(-time.Second),
	)
	source := &fakeAttemptSource{
		fakeCommittedSource: &fakeCommittedSource{results: []agentshadow.Result{}},
		attempts:            []agentshadow.ExecutionAttempt{attempt},
		queryResults:        []agentshadow.Result{result},
	}
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Rebuild(t.Context(), RebuildRequest{
		SnapshotID: "attempt-boundary", Scope: Scope{TenantID: "local", WorkspaceID: "local"},
		Window: attemptWindow, GroupBy: []DimensionName{},
		BuiltAt: result.Observation.RecordedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Facts.Executions) != 1 ||
		!snapshot.Facts.Executions[0].ObservedAt.Equal(attempt.ObservedAt) {
		t.Fatalf("attempt-window execution = %+v", snapshot.Facts.Executions)
	}

	// The inverse boundary must not count the same execution again merely
	// because its later import observation entered this window. In attempt-aware
	// production mode, attempt.ObservedAt is the unique window owner.
	source.results = []agentshadow.Result{result}
	source.attempts = []agentshadow.ExecutionAttempt{}
	source.queryAttempts = []agentshadow.ExecutionAttempt{attempt}
	source.queryResults = nil
	empty, err := adapter.Rebuild(t.Context(), RebuildRequest{
		SnapshotID: "import-boundary-filtered",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     importWindow, GroupBy: []DimensionName{}, BuiltAt: importWindow.EndExclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Facts.Executions) != 0 {
		t.Fatalf("import-time window double-counted execution without current attempt: %+v", empty.Facts.Executions)
	}
}

func TestAttemptAwareSourceKeepsLegacyCommittedImportWithoutIntent(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	result := testCommittedResult(t, "legacy-import", window.StartInclusive.Add(time.Minute), "clean")
	source := &fakeAttemptSource{
		fakeCommittedSource: &fakeCommittedSource{results: []agentshadow.Result{result}},
		attempts:            []agentshadow.ExecutionAttempt{},
	}
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Rebuild(t.Context(), RebuildRequest{
		SnapshotID: "legacy-import", Scope: Scope{TenantID: "local", WorkspaceID: "local"},
		Window: window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Facts.Executions) != 1 || len(snapshot.SourceBindings) != 1 ||
		snapshot.SourceBindings[0].Kind != SourceBindingResult {
		t.Fatalf("legacy committed import was not retained: %+v", snapshot)
	}
}

func TestRebuildRejectsSourceWindowLeak(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	source := &fakeCommittedSource{results: []agentshadow.Result{
		testCommittedResult(t, "outside", window.EndExclusive, "clean"),
	}}
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Rebuild(t.Context(), RebuildRequest{
		SnapshotID: "window-leak", Scope: Scope{TenantID: "local", WorkspaceID: "local"},
		Window: window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the host observation window") {
		t.Fatalf("Rebuild() error = %v, want host observation window rejection", err)
	}
}

func TestProjectionEmptyUnknownDurationOverflowAndDimensionTamper(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	empty, err := Project(FactSet{
		SchemaVersion: FactSetSchemaVersion,
		Window:        window,
		Executions:    []ExecutionFact{},
		Tasks:         []TaskExecutionFact{},
		ToolUsage:     []ToolUsageFact{},
	}, []DimensionName{})
	if err != nil {
		t.Fatalf("Project(empty) error = %v", err)
	}
	for _, metricID := range []string{
		"agent_execution.duration.p50",
		"agent_execution.duration.p95",
		"agent_usage.input_tokens",
	} {
		tile := requireMetricTile(t, empty, metricID)
		if tile.Value != nil || tile.SampleSize != 0 || tile.Availability != TileUnknown ||
			tile.Qualifier != QualifierUnavailable {
			t.Fatalf("empty tile %q fabricated an observation: %+v", metricID, tile)
		}
	}

	overflow := overflowFactSet(window)
	if err := overflow.Validate(); err != nil {
		t.Fatalf("overflow fixture must be valid per execution: %v", err)
	}
	if _, err := Project(overflow, []DimensionName{}); err == nil ||
		!strings.Contains(err.Error(), "input token count overflows") {
		t.Fatalf("Project(overflow) error = %v", err)
	}

	oneResult := testCommittedResult(
		t,
		"dimensions",
		window.StartInclusive.Add(time.Minute),
		"clean",
	)
	facts, _, err := buildFacts(RebuildRequest{
		SnapshotID: "dimensions", Scope: Scope{TenantID: "local", WorkspaceID: "local"},
		Window: window, GroupBy: []DimensionName{DimensionTenant, DimensionModel},
		BuiltAt: window.EndExclusive,
	}, []agentshadow.Result{oneResult})
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Project(facts, []DimensionName{DimensionTenant, DimensionModel})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("name", func(t *testing.T) {
		tampered := cloneProjection(t, projection)
		tampered.Tiles[0].Dimensions[0].Name = DimensionProvider
		if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "group_by") {
			t.Fatalf("tampered dimension name error = %v", err)
		}
	})
	t.Run("order", func(t *testing.T) {
		tampered := cloneProjection(t, projection)
		tampered.Tiles[0].Dimensions[0], tampered.Tiles[0].Dimensions[1] =
			tampered.Tiles[0].Dimensions[1], tampered.Tiles[0].Dimensions[0]
		if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "group_by") {
			t.Fatalf("tampered dimension order error = %v", err)
		}
	})
}

func TestProjectionSnapshotConflictAndTamper(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	source := &fakeCommittedSource{results: []agentshadow.Result{
		testCommittedResult(t, "first", window.StartInclusive.Add(time.Minute), "clean"),
	}}
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	request := RebuildRequest{
		SnapshotID: "immutable", Scope: Scope{TenantID: "local", WorkspaceID: "local"},
		Window: window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
	}
	if _, err := adapter.Rebuild(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	source.results = []agentshadow.Result{
		testCommittedResult(t, "second", window.StartInclusive.Add(2*time.Minute), "clean"),
	}
	if _, err := adapter.Rebuild(t.Context(), request); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("conflicting Rebuild() error = %v, want ErrProjectionConflict", err)
	}

	manifest, err := adapter.loadManifest(request.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(
		store.Root(),
		"artifacts",
		"sha256",
		manifest.SnapshotRef.SHA256[:2],
		manifest.SnapshotRef.SHA256,
	)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Query(request.SnapshotID); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatalf("tampered Query() error = %v, want ErrProjectionCorrupt", err)
	}
}

func TestDecodeProjectionSnapshotRejectsDuplicateFieldsAtEveryDepth(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	source := &fakeCommittedSource{results: []agentshadow.Result{
		testCommittedResult(t, "duplicate", window.StartInclusive.Add(time.Minute), "clean"),
	}}
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Rebuild(t.Context(), RebuildRequest{
		SnapshotID: "duplicate-json",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	topLevel := append(
		[]byte(`{"schema_version":"argus.duplicate",`),
		data[1:]...,
	)
	nested := bytes.Replace(
		data,
		[]byte(`"scope":{"tenant_id":"local","workspace_id":"local"}`),
		[]byte(`"scope":{"tenant_id":"local","tenant_id":"local","workspace_id":"local"}`),
		1,
	)
	if bytes.Equal(nested, data) {
		t.Fatal("nested duplicate fixture did not alter snapshot JSON")
	}
	for name, input := range map[string][]byte{
		"top-level": topLevel,
		"nested":    nested,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeProjectionSnapshot(input); err == nil ||
				!strings.Contains(err.Error(), "duplicate projection JSON field") {
				t.Fatalf("decodeProjectionSnapshot() error = %v", err)
			}
		})
	}
}

func TestQueryExplicitlyRejectsLegacyProjectionManifest(t *testing.T) {
	store, err := local.Open(filepath.Join(t.TempDir(), "analytics"))
	if err != nil {
		t.Fatal(err)
	}
	const snapshotID = "legacy-v1alpha1"
	if err := store.PutJSON(projectionManifestID(snapshotID), projectionManifest{
		SchemaVersion: "argus.agent_execution_projection_manifest.v1alpha1",
		SnapshotID:    snapshotID,
	}); err != nil {
		t.Fatal(err)
	}
	adapter, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Query(snapshotID)
	if !errors.Is(err, ErrProjectionCorrupt) ||
		!strings.Contains(err.Error(), "unsupported projection manifest schema") {
		t.Fatalf("legacy projection query error = %v", err)
	}
}

func TestProjectionMetricRegistryRejectsTampering(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	result := testCommittedResult(
		t,
		"registry",
		window.StartInclusive.Add(time.Minute),
		"partial",
	)
	facts, _, err := buildFacts(RebuildRequest{
		SnapshotID: "registry", Scope: Scope{TenantID: "local", WorkspaceID: "local"},
		Window: window, GroupBy: []DimensionName{DimensionModel},
		BuiltAt: window.EndExclusive,
	}, []agentshadow.Result{result})
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Project(facts, []DimensionName{DimensionModel})
	if err != nil {
		t.Fatal(err)
	}
	if tile := requireMetricTile(t, projection, "agent_usage.input_tokens"); tile.Qualifier != QualifierLowerBound {
		t.Fatalf("fixture lost lower-bound semantics: %+v", tile)
	}

	t.Run("unknown metric", func(t *testing.T) {
		tampered := cloneProjection(t, projection)
		tampered.Tiles[0].MetricID = "agent_unknown.count"
		tampered.Tiles[0].TileID = stableID(
			"agent-execution-tile",
			tileIdentityParts(tampered.Tiles[0])...,
		)
		if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "unknown metric") {
			t.Fatalf("unknown metric error = %v", err)
		}
	})
	t.Run("non-canonical tile id", func(t *testing.T) {
		tampered := cloneProjection(t, projection)
		tampered.Tiles[0].TileID = "agent-execution-tile-tampered"
		if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "not canonical") {
			t.Fatalf("non-canonical tile id error = %v", err)
		}
	})
	t.Run("missing registered metric", func(t *testing.T) {
		tampered := cloneProjection(t, projection)
		tampered.Tiles = tampered.Tiles[1:]
		if err := tampered.Validate(); err == nil ||
			!strings.Contains(err.Error(), "missing registered metric") {
			t.Fatalf("missing metric error = %v", err)
		}
	})
	t.Run("registry metadata", func(t *testing.T) {
		tampered := cloneProjection(t, projection)
		tampered.Tiles[0].Unit = "bytes"
		if err := tampered.Validate(); err == nil ||
			!strings.Contains(err.Error(), "metric registry") {
			t.Fatalf("registry metadata error = %v", err)
		}
	})
}

func TestExportManifestRequiresCanonicalDatasetFileContract(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	facts := FactSet{
		SchemaVersion: FactSetSchemaVersion, Window: window,
		Executions: []ExecutionFact{}, Tasks: []TaskExecutionFact{},
		ToolUsage: []ToolUsageFact{},
	}
	projection, err := Project(facts, []DimensionName{})
	if err != nil {
		t.Fatal(err)
	}
	factsJSON, err := ExportFactSetJSON(facts)
	if err != nil {
		t.Fatal(err)
	}
	factsCSV, err := ExportFactSetCSV(facts)
	if err != nil {
		t.Fatal(err)
	}
	projectionJSON, err := ExportProjectionJSON(projection)
	if err != nil {
		t.Fatal(err)
	}
	projectionCSV, err := ExportProjectionCSV(projection)
	if err != nil {
		t.Fatal(err)
	}
	for name, manifest := range map[string]ExportManifest{
		"facts-json":      factsJSON.Manifest,
		"facts-csv":       factsCSV.Manifest,
		"projection-json": projectionJSON.Manifest,
		"projection-csv":  projectionCSV.Manifest,
	} {
		t.Run(name+" valid", func(t *testing.T) {
			if err := manifest.Validate(); err != nil {
				t.Fatalf("generated manifest invalid: %v", err)
			}
		})
	}

	tests := []struct {
		name   string
		base   ExportManifest
		mutate func(*ExportManifest)
	}{
		{
			name: "wrong JSON filename", base: factsJSON.Manifest,
			mutate: func(manifest *ExportManifest) {
				manifest.Files[0].Name = "facts.json"
			},
		},
		{
			name: "wrong media type", base: projectionJSON.Manifest,
			mutate: func(manifest *ExportManifest) {
				manifest.Files[0].MediaType = "application/octet-stream"
			},
		},
		{
			name: "missing CSV table", base: factsCSV.Manifest,
			mutate: func(manifest *ExportManifest) {
				manifest.Files = manifest.Files[:2]
			},
		},
		{
			name: "extra CSV table", base: projectionCSV.Manifest,
			mutate: func(manifest *ExportManifest) {
				manifest.Files = append(manifest.Files, manifest.Files[0])
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := test.base
			manifest.Files = append([]ExportFileManifest(nil), test.base.Files...)
			test.mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("tampered export manifest was accepted")
			}
		})
	}
}

func testCommittedResult(
	t *testing.T,
	suffix string,
	observedAt time.Time,
	outcome string,
) agentshadow.Result {
	t.Helper()
	ref := func(id string) contractsv1alpha1.VersionedRef {
		return contractsv1alpha1.VersionedRef{
			ID: id, Revision: "1", SHA256: agentAnalyticsTestDigest,
		}
	}
	binding := func(name, contract string) contractsv1alpha1.ArtifactBinding {
		return contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:    "artifact://local/" + name + "-" + suffix,
				SHA256: agentAnalyticsTestDigest, SizeBytes: 10,
			},
			Contract: contract,
		}
	}
	planCreatedAt := observedAt.Add(-3 * time.Minute)
	plan := contractsv1alpha1.AgentReviewPlan{
		SchemaVersion: contractsv1alpha1.AgentReviewPlanSchemaVersion,
		PlanID:        "plan-" + suffix, SourceRunID: "source-run-" + suffix,
		ExecutionID: "execution-" + suffix, ReviewRunID: "review-run-" + suffix,
		ExecutionSnapshotRef: binding("execution-snapshot", contractsv1alpha1.AgentReviewExecutionSnapshotContract),
		ReviewInputRef:       binding("review-input", contractsv1alpha1.AgentReviewInputContract),
		TargetDigest:         agentAnalyticsTestDigest,
		RulePack:             ref("review-rules"),
		Implementation:       ref("pi-review"), Grouping: ref("path-grouping"),
		Normalization: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationCurrentRevision,
			SHA256:   agentAnalyticsTestDigest,
		},
		ContextDimensions: []contractsv1alpha1.VersionedRef{ref("call-context")},
		ReviewDimensions:  []contractsv1alpha1.VersionedRef{ref("correctness")},
		Verifier:          ref("independent-verifier"),
		Knowledge:         []contractsv1alpha1.VersionedRef{},
		Runtime:           ref("node-runtime"), Profile: ref("local-shadow"),
		Agent: ref("pi-agent"), Provider: ref("deepseek-anthropic-env"),
		Model:       ref("deepseek-v4-pro[1m]"),
		APIProtocol: contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
		ToolPolicy: contractsv1alpha1.AgentReviewToolPolicy{
			AllowedTools: []string{"list_files", "read_file", "search_code"},
			ToolNetwork:  "deny", WorkspaceWrites: "deny", RemoteWrites: "deny",
		},
		Budget: contractsv1alpha1.AgentReviewBudget{
			MaxFiles: 10, MaxGroups: 10, MaxCandidates: 10, MaxModelCalls: 10,
			MaxToolCalls: 10, MaxOutputTokens: 4096, MaxGroupBytes: 100000,
			MaxTargetBytes: 200000, TimeoutMS: 60000, MaxConcurrency: 2,
		},
		VerificationRequired: true,
		ExecutionClass:       contractsv1alpha1.AgentReviewExecutionLocalDirectProviderShadow,
		Attestation:          contractsv1alpha1.AgentReviewAttestationNonAttested,
		SideEffects:          "deny", CreatedAt: planCreatedAt,
	}

	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:   contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "hypothesis-set-" + suffix, PlanID: plan.PlanID,
		SourceRunID: plan.SourceRunID, ExecutionID: plan.ExecutionID,
		ReviewRunID: plan.ReviewRunID, TargetDigest: plan.TargetDigest,
		Completeness:           contractsv1alpha1.AgentReviewComplete,
		CompletenessReasons:    []string{},
		NormalizationDecisions: []contractsv1alpha1.HypothesisNormalizationDecision{},
		Hypotheses:             []contractsv1alpha1.ReviewHypothesis{},
		DedupClusters:          []contractsv1alpha1.HypothesisDedupCluster{},
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			GroupsTotal: 1, GroupsReviewed: 1, ReviewTasksTotal: 1,
			ReviewTasksSucceeded: 1, FilesIncluded: 1,
			Gaps: []contractsv1alpha1.AgentReviewCoverageGap{},
		},
		GeneratedAt: planCreatedAt.Add(5 * time.Second),
	}
	contextReceipt := testReceipt(plan, suffix, contractsv1alpha1.AgentTaskContext)
	reviewReceipt := testReceipt(plan, suffix, contractsv1alpha1.AgentTaskReview)
	reasonCodes := []string{}
	switch outcome {
	case "clean":
	case "partial":
		set.Completeness = contractsv1alpha1.AgentReviewPartial
		set.CompletenessReasons = []string{"review_failed"}
		set.Coverage.GroupsReviewed = 0
		set.Coverage.ReviewTasksSucceeded = 0
		set.Coverage.Gaps = []contractsv1alpha1.AgentReviewCoverageGap{{
			GapID: "gap-review", Phase: contractsv1alpha1.AgentReviewCoverageReview,
			SubjectID: "group-1", ReasonCode: "review_failed",
		}}
		makeReceiptFailed(&reviewReceipt, "review_failed", "partial")
		reasonCodes = []string{"provider_partial", "review_failed"}
	case "failure":
		set.Completeness = contractsv1alpha1.AgentReviewPartial
		set.CompletenessReasons = []string{"review_failed"}
		set.Coverage.GroupsReviewed = 0
		set.Coverage.ReviewTasksSucceeded = 0
		set.Coverage.Gaps = []contractsv1alpha1.AgentReviewCoverageGap{{
			GapID: "gap-review", Phase: contractsv1alpha1.AgentReviewCoverageReview,
			SubjectID: "group-1", ReasonCode: "review_failed",
		}}
		makeReceiptFailed(&contextReceipt, "provider_failed", "unavailable")
		makeReceiptFailed(&reviewReceipt, "provider_failed", "unavailable")
		reasonCodes = []string{"provider_failed", "provider_usage_unavailable", "review_failed"}
	default:
		t.Fatalf("unsupported fixture outcome %q", outcome)
	}
	collection := contractsv1alpha1.AgentExecutionReceiptCollection{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		Receipts: []contractsv1alpha1.AgentExecutionReceipt{contextReceipt, reviewReceipt},
	}
	summary, status, err := contractsv1alpha1.SummarizeAgentReviewResult(
		set,
		collection.Receipts,
	)
	if err != nil {
		t.Fatalf("summarize fixture: %v", err)
	}
	planRef := binding("agent-review-plan", contractsv1alpha1.AgentReviewPlanSchemaVersion)
	setRef := binding("hypothesis-set", contractsv1alpha1.ReviewHypothesisSetSchemaVersion)
	rawRef := binding("raw-candidate-collection", contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion)
	taskEvidenceRef := binding("task-evidence-collection", contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion)
	receiptRef := binding("receipt-collection", contractsv1alpha1.AgentExecutionReceiptCollectionContract)
	manifestRef := binding("result-manifest", contractsv1alpha1.AgentReviewResultManifestSchemaVersion)
	manifest := contractsv1alpha1.AgentReviewResultManifest{
		SchemaVersion: contractsv1alpha1.AgentReviewResultManifestSchemaVersion,
		ManifestID:    "manifest-" + suffix, PlanID: plan.PlanID,
		HypothesisSetID: set.HypothesisSetID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest: plan.TargetDigest, ExecutionSnapshotRef: plan.ExecutionSnapshotRef,
		ReviewInputRef: plan.ReviewInputRef, AgentReviewPlanRef: planRef,
		HypothesisSetRef: setRef, RawCandidateCollectionRef: rawRef,
		AgentTaskEvidenceRef:     taskEvidenceRef,
		AgentExecutionReceiptRef: receiptRef,
		ProducerClass:            "argus_go_host", Disposition: "shadow_only",
		Status: status, Summary: summary, RecordedAt: observedAt.Add(-time.Second),
	}
	observation := contractsv1alpha1.AgentReviewObservation{
		SchemaVersion: contractsv1alpha1.AgentReviewObservationSchemaVersion,
		ObservationID: "observation-" + suffix, ManifestID: manifest.ManifestID,
		SourceRunID: plan.SourceRunID, ExecutionID: plan.ExecutionID,
		ReviewRunID: plan.ReviewRunID, Sequence: 1,
		Kind:          contractsv1alpha1.AgentReviewObservationShadowResultRecorded,
		ProducerClass: "argus_go_host", Disposition: "shadow_only", Status: status,
		Agent: plan.Agent, Provider: plan.Provider, Model: plan.Model,
		Summary: summary, DurationMS: uint64((3 * time.Minute).Milliseconds()),
		ReasonCodes: reasonCodes, RecordedAt: observedAt,
	}
	taskEvidence := contractsv1alpha1.AgentReviewTaskEvidenceCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest:   plan.TargetDigest,
		Authority:      contractsv1alpha1.AgentReviewTaskEvidenceAuthority,
		Provenance:     contractsv1alpha1.AgentReviewTaskEvidenceProvenance,
		Disposition:    contractsv1alpha1.AgentReviewTaskEvidenceDisposition,
		ContentPolicy:  contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		Completeness:   contractsv1alpha1.AgentReviewTaskEvidencePartial,
		ReasonCodes:    []string{contractsv1alpha1.AgentReviewTaskEvidenceUnavailable},
		TaskExecutions: []contractsv1alpha1.AgentReviewTaskExecutionEvidence{},
	}
	result := agentshadow.Result{
		Record: agentshadow.ImportRecord{
			SchemaVersion: agentshadow.ImportRecordSchemaVersion,
			TenantID:      "local", WorkspaceID: "local", IdempotencyKey: "import-" + suffix,
			InputDigest: agentAnalyticsTestDigest, AcceptedAt: observedAt,
			ManifestID: manifest.ManifestID, ObservationID: observation.ObservationID,
			ObservationStream:  "agent-review-observations/" + suffix,
			AgentReviewPlanRef: planRef, HypothesisSetRef: setRef,
			RawCandidateCollectionRef: rawRef,
			TaskEvidenceCollectionRef: taskEvidenceRef,
			ReceiptCollectionRef:      receiptRef, ManifestRef: manifestRef,
		},
		Plan: plan, HypothesisSet: set, TaskEvidenceCollection: taskEvidence,
		ReceiptCollection: collection,
		Manifest:          manifest, Observation: observation,
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("fixture plan: %v", err)
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("fixture set: %v", err)
	}
	if err := collection.Validate(); err != nil {
		t.Fatalf("fixture receipts: %v", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(taskEvidence, plan, collection); err != nil {
		t.Fatalf("fixture task evidence: %v", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewResultManifestBindings(
		manifest,
		plan,
		set,
		collection,
	); err != nil {
		t.Fatalf("fixture manifest binding: %v", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewObservationBinding(
		observation,
		manifest,
		plan,
	); err != nil {
		t.Fatalf("fixture observation binding: %v", err)
	}
	return result
}

func testExecutionAttempt(
	result agentshadow.Result,
	status agentshadow.ExecutionStatus,
	observedAt time.Time,
) agentshadow.ExecutionAttempt {
	attempt := agentshadow.ExecutionAttempt{
		SchemaVersion: agentshadow.ExecutionAttemptSchemaVersion,
		Scope: agentshadow.Scope{
			TenantID: result.Record.TenantID, WorkspaceID: result.Record.WorkspaceID,
		},
		Status: status, IdempotencyKey: result.Record.IdempotencyKey,
		SemanticDigest: agentAnalyticsTestDigest, IntentSHA256: agentAnalyticsTestDigest,
		Plan: result.Plan, PlanID: result.Plan.PlanID, SourceRunID: result.Plan.SourceRunID,
		ExecutionID: result.Plan.ExecutionID, ReviewRunID: result.Plan.ReviewRunID,
		Runtime: agentshadow.ExecutionRuntimeDigests{
			WorkerRequestSHA256: agentAnalyticsTestDigest,
			NodeSHA256:          agentAnalyticsTestDigest,
			WorkerScriptSHA256:  agentAnalyticsTestDigest,
			WorkerPackageSHA256: agentAnalyticsTestDigest,
		},
		AcceptedAt: result.Plan.CreatedAt, ObservedAt: observedAt,
	}
	if status == agentshadow.ExecutionStatusUnknownOutcome {
		attempt.ObservedAt = attempt.AcceptedAt
		return attempt
	}
	completionDigest := agentAnalyticsTestDigest
	completedAt := attempt.AcceptedAt.Add(30 * time.Second)
	attempt.CompletionSHA256 = &completionDigest
	attempt.CompletedAt = &completedAt
	if status == agentshadow.ExecutionStatusSucceeded {
		manifest := result.Manifest
		attempt.Manifest = &manifest
		return attempt
	}
	attempt.Failure = &contractsv1alpha1.AgentReviewWorkerFailure{
		Code: "worker_" + string(status), Message: "worker reported a redacted failure",
	}
	return attempt
}

func testReceipt(
	plan contractsv1alpha1.AgentReviewPlan,
	suffix string,
	role contractsv1alpha1.AgentTaskRole,
) contractsv1alpha1.AgentExecutionReceipt {
	roleName := string(role)
	dimension := plan.ContextDimensions[0]
	terminalTool := "submit_context"
	taskOrder := "1"
	if role == contractsv1alpha1.AgentTaskReview {
		dimension = plan.ReviewDimensions[0]
		terminalTool = "submit_candidates"
		taskOrder = "2"
	}
	promptDigest := agentAnalyticsTestDigest
	outputDigest := agentAnalyticsTestDigest
	reasoning := uint64(10)
	return contractsv1alpha1.AgentExecutionReceipt{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptSchemaVersion,
		ReceiptID:     "receipt-" + taskOrder + "-" + roleName + "-" + suffix,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TaskID:  "task-" + taskOrder + "-" + roleName + "-" + suffix,
		GroupID: "group-1", TaskRole: role, Dimension: dimension,
		Runtime: plan.Runtime, Profile: plan.Profile, Agent: plan.Agent,
		Provider: plan.Provider, Model: plan.Model, APIProtocol: plan.APIProtocol,
		ProvenanceClass: contractsv1alpha1.AgentReceiptProvenanceWorkerSelfReport,
		Authority:       contractsv1alpha1.AgentReceiptAuthorityDiagnosticOnly,
		Status:          contractsv1alpha1.AgentTaskSucceeded,
		PromptDigest:    &promptDigest, OutputDigest: &outputDigest,
		StartedAt:         plan.CreatedAt.Add(20 * time.Second),
		FinishedAt:        plan.CreatedAt.Add(30 * time.Second),
		ModelTurnsStarted: 1, ModelTurnsCompleted: 1, ToolCalls: 2,
		ToolUsage: []contractsv1alpha1.AgentToolUsage{
			{ToolID: "read_file", InvocationCount: 1},
			{ToolID: terminalTool, InvocationCount: 1},
		},
		Usage: contractsv1alpha1.AgentTokenUsage{
			Completeness: contractsv1alpha1.AgentTokenUsageProviderReported,
			InputTokens:  100, OutputTokens: 50, ReasoningTokens: &reasoning,
			TotalTokens: 150,
		},
	}
}

func makeReceiptFailed(
	receipt *contractsv1alpha1.AgentExecutionReceipt,
	failureReason string,
	usageKind string,
) {
	receipt.Status = contractsv1alpha1.AgentTaskFailed
	receipt.FailureReasonCode = &failureReason
	receipt.OutputDigest = nil
	receipt.ModelTurnsCompleted = 0
	receipt.ToolCalls = 1
	receipt.ToolUsage = receipt.ToolUsage[:1]
	switch usageKind {
	case "partial":
		reason := "provider_partial"
		receipt.Usage = contractsv1alpha1.AgentTokenUsage{
			Completeness:          contractsv1alpha1.AgentTokenUsagePartial,
			UnavailableReasonCode: &reason,
			InputTokens:           10, OutputTokens: 5, TotalTokens: 15,
		}
	case "unavailable":
		reason := "provider_usage_unavailable"
		receipt.Usage = contractsv1alpha1.AgentTokenUsage{
			Completeness:          contractsv1alpha1.AgentTokenUsageUnavailable,
			UnavailableReasonCode: &reason,
		}
	}
}

func overflowFactSet(window TimeWindow) FactSet {
	ref := contractsv1alpha1.VersionedRef{
		ID: "deepseek-v4-pro[1m]", Revision: "1", SHA256: agentAnalyticsTestDigest,
	}
	executions := make([]ExecutionFact, 0, 2)
	tasks := make([]TaskExecutionFact, 0, 2)
	for _, suffix := range []string{"a", "b"} {
		executionFactID := "execution-fact-" + suffix
		executions = append(executions, ExecutionFact{
			SchemaVersion: ExecutionFactSchemaVersion, FactID: executionFactID,
			ObservationID: "observation-" + suffix, ManifestID: "manifest-" + suffix,
			SourceRunID: "source-" + suffix, ExecutionID: "execution-" + suffix,
			ReviewRunID: "review-" + suffix, TenantID: "local", WorkspaceID: "local",
			Agent: ref, Provider: ref, Model: ref,
			APIProtocol:    contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
			ExecutionClass: contractsv1alpha1.AgentReviewExecutionLocalDirectProviderShadow,
			Attestation:    contractsv1alpha1.AgentReviewAttestationNonAttested,
			Disposition:    ShadowDisposition, Status: ExecutionStatusComplete,
			DurationMS: 1000, ReasonCodes: []string{}, Receipts: 1, TasksSucceeded: 1,
			Usage: ExecutionUsage{
				Completeness:     contractsv1alpha1.AgentTokenUsageProviderReported,
				ReportedReceipts: 1, InputTokens: math.MaxUint64, TotalTokens: math.MaxUint64,
			},
			ProvenanceClass: HostObservationProvenance, Authority: DiagnosticAuthority,
			ObservedAt: window.StartInclusive.Add(time.Minute),
		})
		tasks = append(tasks, TaskExecutionFact{
			SchemaVersion: TaskFactSchemaVersion, FactID: "task-fact-" + suffix,
			ExecutionFactID: executionFactID, ReceiptID: "receipt-" + suffix,
			TaskID: "task-" + suffix, GroupID: "group-1", Role: contractsv1alpha1.AgentTaskContext,
			Dimension: ref, Runtime: ref, Profile: ref, Agent: ref, Provider: ref, Model: ref,
			APIProtocol: contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
			Status:      contractsv1alpha1.AgentTaskSucceeded,
			StartedAt:   window.StartInclusive, FinishedAt: window.StartInclusive.Add(time.Second),
			DurationMS: 1000,
			Usage: contractsv1alpha1.AgentTokenUsage{
				Completeness: contractsv1alpha1.AgentTokenUsageProviderReported,
				InputTokens:  math.MaxUint64, TotalTokens: math.MaxUint64,
			},
			ProvenanceClass: WorkerSelfReportProvenance, Authority: DiagnosticAuthority,
			ObservedAt: window.StartInclusive.Add(time.Minute),
		})
	}
	return FactSet{
		SchemaVersion: FactSetSchemaVersion, Window: window,
		Executions: executions, Tasks: tasks, ToolUsage: []ToolUsageFact{},
	}
}

func requireMetricTile(t *testing.T, projection Projection, metricID string) MetricTile {
	t.Helper()
	for _, tile := range projection.Tiles {
		if tile.MetricID == metricID {
			return tile
		}
	}
	t.Fatalf("metric tile %q not found", metricID)
	return MetricTile{}
}

func cloneProjection(t *testing.T, projection Projection) Projection {
	t.Helper()
	data, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var result Projection
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
