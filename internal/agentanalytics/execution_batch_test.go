package agentanalytics

import (
	"context"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/agentshadow"
	"argus.local/argus/internal/store/local"
)

// QueryExecutions upgrades the shared attempt-aware fixture to the production
// batch seam. Existing QueryExecution remains on the fixture intentionally so
// tests also prove rebuild no longer selects the old per-row port.
func (source *fakeAttemptSource) QueryExecutions(
	_ context.Context,
	request agentshadow.ExecutionBatchQueryRequest,
) (agentshadow.ExecutionBatchQueryResult, error) {
	if err := request.Validate(); err != nil {
		return agentshadow.ExecutionBatchQueryResult{}, err
	}
	available := make(map[string]agentshadow.ExecutionAttempt)
	for _, attempt := range append(
		append([]agentshadow.ExecutionAttempt(nil), source.attempts...),
		source.queryAttempts...,
	) {
		if attempt.Scope == request.Scope {
			available[attempt.ExecutionID] = attempt
		}
	}
	response := agentshadow.ExecutionBatchQueryResult{
		Attempts:            []agentshadow.ExecutionAttempt{},
		MissingExecutionIDs: []string{},
	}
	for _, executionID := range request.ExecutionIDs {
		if attempt, exists := available[executionID]; exists {
			response.Attempts = append(response.Attempts, attempt)
		} else {
			response.MissingExecutionIDs = append(response.MissingExecutionIDs, executionID)
		}
	}
	return response, nil
}

type countingBatchAttemptSource struct {
	results       []agentshadow.Result
	current       []agentshadow.ExecutionAttempt
	batchResponse agentshadow.ExecutionBatchQueryResult
	batchCalls    int
	batchRequests []agentshadow.ExecutionBatchQueryRequest
	singleCalls   int
}

func (source *countingBatchAttemptSource) List(
	_ context.Context,
	_ agentshadow.ListRequest,
) ([]agentshadow.Result, error) {
	return append([]agentshadow.Result(nil), source.results...), nil
}

func (source *countingBatchAttemptSource) ListExecutions(
	_ context.Context,
	_ agentshadow.ExecutionListRequest,
) ([]agentshadow.ExecutionAttempt, error) {
	return append([]agentshadow.ExecutionAttempt(nil), source.current...), nil
}

func (source *countingBatchAttemptSource) QueryExecutions(
	_ context.Context,
	request agentshadow.ExecutionBatchQueryRequest,
) (agentshadow.ExecutionBatchQueryResult, error) {
	source.batchCalls++
	source.batchRequests = append(source.batchRequests, request)
	return source.batchResponse, nil
}

func (source *countingBatchAttemptSource) QueryExecution(
	_ context.Context,
	_ agentshadow.ExecutionQueryRequest,
) (agentshadow.ExecutionAttempt, error) {
	source.singleCalls++
	return agentshadow.ExecutionAttempt{}, agentshadow.ErrNotFound
}

func TestAttemptAwareRebuildUsesOneBatchLookupForAllUnownedResults(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	// The old execution was completed just before this window, while its import
	// observation entered the window. It must not be counted a second time.
	oldResult := testCommittedResult(
		t, "batch-old-window", window.StartInclusive.Add(2*time.Minute), "clean",
	)
	oldAttempt := testExecutionAttempt(
		oldResult,
		agentshadow.ExecutionStatusSucceeded,
		window.StartInclusive.Add(-10*time.Second),
	)
	legacy := testCommittedResult(
		t, "batch-legacy", window.StartInclusive.Add(10*time.Minute), "clean",
	)
	source := &countingBatchAttemptSource{
		results: []agentshadow.Result{legacy, oldResult},
		batchResponse: agentshadow.ExecutionBatchQueryResult{
			Attempts:            []agentshadow.ExecutionAttempt{oldAttempt},
			MissingExecutionIDs: []string{legacy.Plan.ExecutionID},
		},
	}
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Rebuild(t.Context(), RebuildRequest{
		SnapshotID: "batch-complexity",
		Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
		Window:     window,
		GroupBy:    []DimensionName{},
		BuiltAt:    window.EndExclusive,
	})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if source.batchCalls != 1 || source.singleCalls != 0 {
		t.Fatalf("lookup calls = batch %d, single %d; want 1,0", source.batchCalls, source.singleCalls)
	}
	if len(source.batchRequests) != 1 ||
		len(source.batchRequests[0].ExecutionIDs) != 2 ||
		source.batchRequests[0].ExecutionIDs[0] != legacy.Plan.ExecutionID ||
		source.batchRequests[0].ExecutionIDs[1] != oldResult.Plan.ExecutionID {
		t.Fatalf("batch requests = %+v", source.batchRequests)
	}
	if len(snapshot.Facts.Executions) != 1 ||
		snapshot.Facts.Executions[0].ExecutionID != legacy.Plan.ExecutionID ||
		snapshot.SourceBindings[0].Kind != SourceBindingResult {
		t.Fatalf("legacy/other-window classification = %+v", snapshot)
	}
}

func TestAttemptAwareRebuildRejectsCurrentOmissionAndOldSingleLookupPort(t *testing.T) {
	window := TimeWindow{
		StartInclusive: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}
	result := testCommittedResult(
		t, "batch-current-omission", window.StartInclusive.Add(20*time.Minute), "clean",
	)
	current := testExecutionAttempt(
		result,
		agentshadow.ExecutionStatusSucceeded,
		window.StartInclusive.Add(19*time.Minute),
	)
	t.Run("current omission", func(t *testing.T) {
		source := &countingBatchAttemptSource{
			results: []agentshadow.Result{result},
			batchResponse: agentshadow.ExecutionBatchQueryResult{
				Attempts:            []agentshadow.ExecutionAttempt{current},
				MissingExecutionIDs: []string{},
			},
		}
		store, err := local.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := New(source, store)
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.Rebuild(t.Context(), RebuildRequest{
			SnapshotID: "batch-current-omission",
			Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
			Window:     window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
		})
		if err == nil || !strings.Contains(err.Error(), "omitted current-window") {
			t.Fatalf("current omission error = %v", err)
		}
		if source.batchCalls != 1 || source.singleCalls != 0 {
			t.Fatalf("lookup calls = batch %d, single %d", source.batchCalls, source.singleCalls)
		}
	})

	t.Run("single lookup only", func(t *testing.T) {
		source := &singleLookupOnlyAttemptSource{results: []agentshadow.Result{result}}
		store, err := local.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := New(source, store)
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.Rebuild(t.Context(), RebuildRequest{
			SnapshotID: "single-port-rejected",
			Scope:      Scope{TenantID: "local", WorkspaceID: "local"},
			Window:     window, GroupBy: []DimensionName{}, BuiltAt: window.EndExclusive,
		})
		if err == nil || !strings.Contains(err.Error(), "must implement bounded batch") {
			t.Fatalf("single-port source error = %v", err)
		}
		if source.singleCalls != 0 {
			t.Fatalf("legacy QueryExecution called %d times", source.singleCalls)
		}
	})
}

type singleLookupOnlyAttemptSource struct {
	results     []agentshadow.Result
	singleCalls int
}

func (source *singleLookupOnlyAttemptSource) List(
	_ context.Context,
	_ agentshadow.ListRequest,
) ([]agentshadow.Result, error) {
	return append([]agentshadow.Result(nil), source.results...), nil
}

func (*singleLookupOnlyAttemptSource) ListExecutions(
	context.Context,
	agentshadow.ExecutionListRequest,
) ([]agentshadow.ExecutionAttempt, error) {
	return []agentshadow.ExecutionAttempt{}, nil
}

func (source *singleLookupOnlyAttemptSource) QueryExecution(
	context.Context,
	agentshadow.ExecutionQueryRequest,
) (agentshadow.ExecutionAttempt, error) {
	source.singleCalls++
	return agentshadow.ExecutionAttempt{}, agentshadow.ErrNotFound
}

func TestExecutionBatchResponseRejectsTamperAndNonPartition(t *testing.T) {
	windowStart := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	result := testCommittedResult(t, "batch-response", windowStart.Add(20*time.Minute), "clean")
	attempt := testExecutionAttempt(
		result,
		agentshadow.ExecutionStatusSucceeded,
		windowStart.Add(19*time.Minute),
	)
	scope := Scope{TenantID: "local", WorkspaceID: "local"}
	requested := []string{attempt.ExecutionID}
	tests := map[string]agentshadow.ExecutionBatchQueryResult{
		"tampered closure": func() agentshadow.ExecutionBatchQueryResult {
			forged := attempt
			forged.IntentSHA256 = "forged"
			return agentshadow.ExecutionBatchQueryResult{Attempts: []agentshadow.ExecutionAttempt{forged}}
		}(),
		"both matched and missing": {
			Attempts:            []agentshadow.ExecutionAttempt{attempt},
			MissingExecutionIDs: []string{attempt.ExecutionID},
		},
		"omitted": {},
		"unrequested": {
			MissingExecutionIDs: []string{"execution-unrequested"},
		},
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := indexExecutionBatchResponse(scope, requested, response); err == nil {
				t.Fatalf("batch response was accepted: %+v", response)
			}
		})
	}
}
