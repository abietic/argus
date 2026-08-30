package platformapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/reviewjob"
	"argus.local/argus/internal/scheduling"
)

func TestReviewJobAPIInjectsPrincipalAndKeepsReadExecutePermissionsSeparate(t *testing.T) {
	service := &reviewJobServiceStub{}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "review-operator", Roles: []evaluation.Role{},
		Permissions:     []Permission{PermissionReviewExecute, PermissionReviewRead},
		ProfileRevision: "review-operator-v1",
	}
	handler, err := NewHandler(Services{ReviewJobs: service}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	command := ReviewJobSubmitCommand{
		SchemaVersion: ReviewJobSubmitCommandSchemaVersion,
		Request: reviewjob.Request{
			SchemaVersion:    reviewjob.RequestSchemaVersion,
			ExecutionProfile: reviewjob.DeterministicExecutionProfile,
			RepositoryPath:   t.TempDir(), Mode: "diff",
			BaseRevision: "base", HeadRevision: "head",
			ExecutionTimeoutSeconds: 1800,
		},
		Mutation: MutationInput{
			SchemaVersion:  MutationInputSchemaVersion,
			IdempotencyKey: "submit-review-job", Audit: "submit review job", At: at,
		},
	}
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodPost, "/v1/review-jobs", bytes.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status=%d body=%s", response.Code, response.Body.String())
	}
	if service.submitted.Actor != principal.Actor || service.submitted.IdempotencyKey != command.Mutation.IdempotencyKey {
		t.Fatalf("injected mutation = %+v", service.submitted)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedRequest(http.MethodGet, "/v1/review-jobs", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	timeline := httptest.NewRecorder()
	handler.ServeHTTP(timeline, authenticatedRequest(
		http.MethodGet, "/v1/review-jobs/job-test/timeline?limit=1", nil,
	))
	if timeline.Code != http.StatusOK || !bytes.Contains(timeline.Body.Bytes(), []byte(`"type":"submitted"`)) {
		t.Fatalf("timeline status=%d body=%s", timeline.Code, timeline.Body.String())
	}

	cancelBody, _ := json.Marshal(ReviewJobCancelCommand{
		SchemaVersion: ReviewJobCancelCommandSchemaVersion,
		Reason:        "operator canceled",
		Mutation: MutationInput{
			SchemaVersion:  MutationInputSchemaVersion,
			IdempotencyKey: "cancel-review-job", Audit: "cancel review job", At: at.Add(time.Second),
		},
	})
	canceled := httptest.NewRecorder()
	handler.ServeHTTP(canceled, authenticatedRequest(
		http.MethodPost, "/v1/review-jobs/job-test/cancel", bytes.NewReader(cancelBody),
	))
	if canceled.Code != http.StatusOK || service.canceled.Mutation.Actor != principal.Actor {
		t.Fatalf("cancel status=%d body=%s mutation=%+v", canceled.Code, canceled.Body.String(), service.canceled.Mutation)
	}

	readOnly := principal
	readOnly.Permissions = []Permission{PermissionReviewRead}
	readHandler, err := NewHandler(Services{ReviewJobs: service}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := httptest.NewRecorder()
	readHandler.ServeHTTP(forbidden, authenticatedRequest(http.MethodPost, "/v1/review-jobs", bytes.NewReader(body)))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("read-only submit status=%d", forbidden.Code)
	}

	executeOnly := principal
	executeOnly.Permissions = []Permission{PermissionReviewExecute}
	executeHandler, err := NewHandler(Services{ReviewJobs: service}, executeOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenRead := httptest.NewRecorder()
	executeHandler.ServeHTTP(forbiddenRead, authenticatedRequest(http.MethodGet, "/v1/review-jobs", nil))
	if forbiddenRead.Code != http.StatusForbidden {
		t.Fatalf("execute-only list status=%d", forbiddenRead.Code)
	}
}

func TestReviewJobAPIRejectsCallerActorAndMapsMissing(t *testing.T) {
	service := &reviewJobServiceStub{getErr: reviewjob.ErrNotFound}
	handler, err := NewHandler(Services{ReviewJobs: service}, Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "review-reader", Roles: []evaluation.Role{},
		Permissions:     []Permission{PermissionReviewExecute, PermissionReviewRead},
		ProfileRevision: "review-reader-v1",
	}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, authenticatedRequest(http.MethodGet, "/v1/review-jobs/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}

	malformed := []byte(`{"schema_version":"argus.local_api_review_job_submit_command.v1alpha1","request":{},"mutation":{},"actor":"spoofed"}`)
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, authenticatedRequest(http.MethodPost, "/v1/review-jobs", bytes.NewReader(malformed)))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("caller actor status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}

func TestReviewJobAPIAcceptsFormalSourceRunProfileWithoutTargetFields(t *testing.T) {
	service := &reviewJobServiceStub{}
	handler, err := NewHandler(Services{ReviewJobs: service}, Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "formal-operator", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionReviewExecute}, ProfileRevision: "formal-operator-v1",
	}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(ReviewJobSubmitCommand{
		SchemaVersion: ReviewJobSubmitCommandSchemaVersion,
		Request: reviewjob.Request{
			SchemaVersion:    reviewjob.RequestSchemaVersion,
			ExecutionProfile: reviewjob.FormalPiExecutionProfile,
			SourceRunID:      "run-succeeded-source", ExecutionTimeoutSeconds: 1800,
		},
		Mutation: MutationInput{
			SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "formal-submit",
			Audit: "submit formal job", At: time.Now().UTC(),
		},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/review-jobs", bytes.NewReader(body),
	))
	if response.Code != http.StatusAccepted ||
		service.request.ExecutionProfile != reviewjob.FormalPiExecutionProfile ||
		service.request.SourceRunID != "run-succeeded-source" {
		t.Fatalf("formal submit status=%d body=%s request=%+v",
			response.Code, response.Body.String(), service.request)
	}
}

type reviewJobServiceStub struct {
	submitted reviewjob.Mutation
	request   reviewjob.Request
	canceled  reviewjob.CancelCommand
	getErr    error
}

func (service *reviewJobServiceStub) Submit(
	_ context.Context,
	request reviewjob.Request,
	mutation reviewjob.Mutation,
) (reviewjob.Record, error) {
	service.submitted = mutation
	service.request = request
	return reviewjob.Record{
		SchemaVersion: reviewjob.RecordSchemaVersion,
		JobID:         "job-test", RunID: "run-test", Request: request,
		ExecutionProfile: request.ExecutionProfile,
		Workload:         scheduling.WorkloadRecord{State: scheduling.StatePending},
		SubmittedBy:      mutation.Actor, SubmittedAt: mutation.At,
	}, nil
}

func (service *reviewJobServiceStub) Get(jobID string) (reviewjob.Record, error) {
	if service.getErr != nil {
		return reviewjob.Record{}, service.getErr
	}
	return reviewjob.Record{SchemaVersion: reviewjob.RecordSchemaVersion, JobID: jobID}, nil
}

func (service *reviewJobServiceStub) List() ([]reviewjob.Record, error) {
	return []reviewjob.Record{{SchemaVersion: reviewjob.RecordSchemaVersion, JobID: "job-test"}}, nil
}

func (service *reviewJobServiceStub) Timeline(
	string,
) ([]scheduling.WorkloadTimelineEvent, error) {
	return []scheduling.WorkloadTimelineEvent{{
		SchemaVersion: scheduling.TimelineSchemaVersion, Sequence: 1,
		EventID: "submitted", Type: "submitted", OccurredAt: time.Now().UTC(),
	}}, nil
}

func (service *reviewJobServiceStub) Cancel(
	_ context.Context,
	jobID string,
	command reviewjob.CancelCommand,
) (reviewjob.Record, error) {
	service.canceled = command
	if jobID == "missing" {
		return reviewjob.Record{}, errors.New("unexpected missing")
	}
	return reviewjob.Record{SchemaVersion: reviewjob.RecordSchemaVersion, JobID: jobID}, nil
}
