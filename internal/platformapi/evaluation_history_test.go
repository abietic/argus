package platformapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abietic/argus/internal/evaluation"
)

type evaluationHistoryStub struct {
	access               evaluation.Access
	evaluations          []evaluation.EvaluationRun
	experiments          []evaluation.ExperimentRun
	repeatability        []evaluation.RepeatabilityRun
	experimentBatches    []evaluation.ExperimentBatchRecord
	repeatabilityBatches []evaluation.RepeatabilityBatchRecord
}

func (stub *evaluationHistoryStub) remember(access evaluation.Access) { stub.access = access }

func (stub *evaluationHistoryStub) ListEvaluationRuns(access evaluation.Access) ([]evaluation.EvaluationRun, error) {
	stub.remember(access)
	return stub.evaluations, nil
}

func (stub *evaluationHistoryStub) GetEvaluationRun(id string, access evaluation.Access) (evaluation.EvaluationRun, error) {
	stub.remember(access)
	for _, record := range stub.evaluations {
		if record.EvaluationRunID == id {
			return record, nil
		}
	}
	return evaluation.EvaluationRun{}, evaluation.ErrNotFound
}

func (stub *evaluationHistoryStub) ListExperimentRuns(access evaluation.Access) ([]evaluation.ExperimentRun, error) {
	stub.remember(access)
	return stub.experiments, nil
}

func (stub *evaluationHistoryStub) GetExperimentRun(id string, access evaluation.Access) (evaluation.ExperimentRun, error) {
	stub.remember(access)
	for _, record := range stub.experiments {
		if record.ExperimentRunID == id {
			return record, nil
		}
	}
	return evaluation.ExperimentRun{}, evaluation.ErrNotFound
}

func (stub *evaluationHistoryStub) ListRepeatabilityRuns(access evaluation.Access) ([]evaluation.RepeatabilityRun, error) {
	stub.remember(access)
	return stub.repeatability, nil
}

func (stub *evaluationHistoryStub) GetRepeatabilityRun(id string, access evaluation.Access) (evaluation.RepeatabilityRun, error) {
	stub.remember(access)
	for _, record := range stub.repeatability {
		if record.RepeatabilityRunID == id {
			return record, nil
		}
	}
	return evaluation.RepeatabilityRun{}, evaluation.ErrNotFound
}

func (stub *evaluationHistoryStub) ListExperimentBatches(access evaluation.Access) ([]evaluation.ExperimentBatchRecord, error) {
	stub.remember(access)
	return stub.experimentBatches, nil
}

func (stub *evaluationHistoryStub) GetExperimentBatch(id string, access evaluation.Access) (evaluation.ExperimentBatchRecord, error) {
	stub.remember(access)
	for _, record := range stub.experimentBatches {
		if record.Request.BatchID == id {
			return record, nil
		}
	}
	return evaluation.ExperimentBatchRecord{}, evaluation.ErrNotFound
}

func (stub *evaluationHistoryStub) ListRepeatabilityBatches(access evaluation.Access) ([]evaluation.RepeatabilityBatchRecord, error) {
	stub.remember(access)
	return stub.repeatabilityBatches, nil
}

func (stub *evaluationHistoryStub) GetRepeatabilityBatch(id string, access evaluation.Access) (evaluation.RepeatabilityBatchRecord, error) {
	stub.remember(access)
	for _, record := range stub.repeatabilityBatches {
		if record.Request.BatchID == id {
			return record, nil
		}
	}
	return evaluation.RepeatabilityBatchRecord{}, evaluation.ErrNotFound
}

func TestEvaluationHistoryAPIListsPagesAndReadsDistinctRunAndBatchFacts(t *testing.T) {
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)
	stub := &evaluationHistoryStub{
		evaluations: []evaluation.EvaluationRun{
			{EvaluationRunID: "evaluation-a", RecordedAt: now},
			{EvaluationRunID: "evaluation-b", RecordedAt: now.Add(time.Minute)},
			{EvaluationRunID: "evaluation-c", RecordedAt: now.Add(2 * time.Minute)},
		},
		experiments:   []evaluation.ExperimentRun{{ExperimentRunID: "experiment-a", RecordedAt: now}},
		repeatability: []evaluation.RepeatabilityRun{{RepeatabilityRunID: "repeatability-a", RecordedAt: now}},
		experimentBatches: []evaluation.ExperimentBatchRecord{{
			Request: evaluation.ExperimentBatchRequest{BatchID: "experiment-batch-a"},
			Status:  evaluation.ExperimentBatchRunning,
		}},
		repeatabilityBatches: []evaluation.RepeatabilityBatchRecord{{
			Request: evaluation.RepeatabilityBatchRequest{BatchID: "repeatability-batch-a"},
			Status:  evaluation.RepeatabilityBatchSucceeded,
		}},
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "evaluation-history-reader",
		Roles:       []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions: []Permission{PermissionEvaluationRead}, ProfileRevision: "history-reader-v1",
	}
	handler, err := NewHandler(Services{EvaluationHistory: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, authenticatedRequest(http.MethodGet, "/v1/evaluation/evaluation-runs?limit=2", nil))
	var firstPage struct {
		Data struct {
			Items      []evaluation.EvaluationRun `json:"items"`
			NextCursor string                     `json:"next_cursor"`
		} `json:"data"`
	}
	decodeResponse(t, first.Body.Bytes(), &firstPage)
	if first.Code != http.StatusOK || len(firstPage.Data.Items) != 2 ||
		firstPage.Data.Items[0].EvaluationRunID != "evaluation-a" || firstPage.Data.NextCursor == "" {
		t.Fatalf("first page status=%d page=%+v", first.Code, firstPage)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, authenticatedRequest(http.MethodGet,
		"/v1/evaluation/evaluation-runs?limit=2&cursor="+firstPage.Data.NextCursor, nil))
	var secondPage struct {
		Data struct {
			Items []evaluation.EvaluationRun `json:"items"`
		} `json:"data"`
	}
	decodeResponse(t, second.Body.Bytes(), &secondPage)
	if second.Code != http.StatusOK || len(secondPage.Data.Items) != 1 ||
		secondPage.Data.Items[0].EvaluationRunID != "evaluation-c" {
		t.Fatalf("second page status=%d page=%+v", second.Code, secondPage)
	}

	checks := []struct {
		list string
		show string
		want string
	}{
		{"/v1/evaluation/experiment-runs", "/v1/evaluation/experiment-run?experiment_run_id=experiment-a", "experiment-a"},
		{"/v1/evaluation/repeatability-runs", "/v1/evaluation/repeatability-run?repeatability_run_id=repeatability-a", "repeatability-a"},
		{"/v1/evaluation/experiment-batches", "/v1/evaluation/experiment-batch?batch_id=experiment-batch-a", "experiment-batch-a"},
		{"/v1/evaluation/repeatability-batches", "/v1/evaluation/repeatability-batch?batch_id=repeatability-batch-a", "repeatability-batch-a"},
	}
	for _, check := range checks {
		for _, target := range []string{check.list, check.show} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, target, nil))
			if response.Code != http.StatusOK || !containsJSONText(response.Body.Bytes(), check.want) {
				t.Fatalf("GET %s status=%d body=%s", target, response.Code, response.Body.String())
			}
		}
	}
	if stub.access.Actor != principal.Actor || len(stub.access.Roles) != 1 ||
		stub.access.Roles[0] != evaluation.RoleDatasetCurator {
		t.Fatalf("history access = %+v, want process principal", stub.access)
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, authenticatedRequest(http.MethodGet,
		"/v1/evaluation/evaluation-run?evaluation_run_id=missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
	badQuery := httptest.NewRecorder()
	handler.ServeHTTP(badQuery, authenticatedRequest(http.MethodGet,
		"/v1/evaluation/experiment-runs?unknown=true", nil))
	if badQuery.Code != http.StatusBadRequest {
		t.Fatalf("bad query status=%d body=%s", badQuery.Code, badQuery.Body.String())
	}
}

func TestEvaluationHistoryAPIFailsClosedWithoutPermissionOrService(t *testing.T) {
	stub := &evaluationHistoryStub{}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "dashboard-only-reader",
		Roles: []evaluation.Role{}, Permissions: []Permission{PermissionDashboardRead},
		ProfileRevision: "dashboard-only-v1",
	}
	handler, err := NewHandler(Services{EvaluationHistory: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := httptest.NewRecorder()
	handler.ServeHTTP(forbidden, authenticatedRequest(http.MethodGet,
		"/v1/evaluation/evaluation-runs", nil))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}

	evaluationPrincipal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "evaluation-reader",
		Roles:       []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions: []Permission{PermissionEvaluationRead}, ProfileRevision: "evaluation-reader-v1",
	}
	unavailable, err := NewHandler(Services{EvaluationHistory: &errorEvaluationHistoryStub{}}, evaluationPrincipal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	unavailable.ServeHTTP(response, authenticatedRequest(http.MethodGet,
		"/v1/evaluation/evaluation-runs", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt history status=%d body=%s", response.Code, response.Body.String())
	}
}

type errorEvaluationHistoryStub struct{ evaluationHistoryStub }

func (errorEvaluationHistoryStub) ListEvaluationRuns(evaluation.Access) ([]evaluation.EvaluationRun, error) {
	return nil, errors.Join(evaluation.ErrCorrupt, errors.New("injected corrupt history"))
}

func containsJSONText(data []byte, text string) bool {
	for index := 0; index+len(text) <= len(data); index++ {
		if string(data[index:index+len(text)]) == text {
			return true
		}
	}
	return false
}
