package platformapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/training"
)

type trainingServiceStub struct {
	record   training.Record
	mutation evaluation.Mutation
}

type trainingExportServiceStub struct {
	record   training.ExportRecord
	mutation evaluation.Mutation
}

type trainingJobServiceStub struct {
	record       training.JobRecord
	prepareInput training.JobPrepareRequest
	observeInput training.JobObservationRequest
	mutation     evaluation.Mutation
}

func (stub *trainingJobServiceStub) Prepare(_ context.Context, request training.JobPrepareRequest, mutation evaluation.Mutation) (training.JobRecord, error) {
	stub.prepareInput, stub.mutation = request, mutation
	return stub.record, nil
}
func (stub *trainingJobServiceStub) Observe(_ context.Context, request training.JobObservationRequest, mutation evaluation.Mutation) (training.JobRecord, error) {
	stub.observeInput, stub.mutation = request, mutation
	return stub.record, nil
}
func (stub *trainingJobServiceStub) Get(string, evaluation.Access) (training.JobRecord, error) {
	return stub.record, nil
}
func (stub *trainingJobServiceStub) List(evaluation.Access) ([]training.JobRecord, error) {
	return []training.JobRecord{stub.record}, nil
}

func (stub *trainingExportServiceStub) Build(_ context.Context, request training.ExportRequest, mutation evaluation.Mutation) (training.ExportRecord, error) {
	stub.mutation = mutation
	result := stub.record
	result.Request = request
	result.Mutation = mutation
	return result, nil
}

func (stub *trainingExportServiceStub) Get(string, evaluation.Access) (training.ExportRecord, error) {
	return stub.record, nil
}

func (stub *trainingExportServiceStub) List(evaluation.Access) ([]training.ExportRecord, error) {
	return []training.ExportRecord{stub.record}, nil
}

func (stub *trainingServiceStub) Materialize(_ context.Context, request training.MaterializationRequest, mutation evaluation.Mutation) (training.Record, error) {
	stub.mutation = mutation
	result := stub.record
	result.Request = request
	result.Mutation = mutation
	return result, nil
}
func (stub *trainingServiceStub) Get(string, evaluation.Access) (training.Record, error) {
	return stub.record, nil
}
func (stub *trainingServiceStub) List(evaluation.Access) ([]training.Record, error) {
	return []training.Record{stub.record}, nil
}

func TestTrainingRoutesAreAuthenticatedAuthorizedStrictAndInjectPrincipal(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	stub := &trainingServiceStub{record: training.Record{Manifest: training.DatasetManifest{ManifestID: "training-manifest-aaaaaaaaaaaaaaaaaaaaaaaa", DatasetID: "dataset-1", DatasetRevision: "revision-1"}}}
	principal := Principal{SchemaVersion: PrincipalSchemaVersion, Actor: "training-curator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Permissions: []Permission{PermissionEvaluationRead, PermissionEvaluationWrite}, ProfileRevision: "training-v1"}
	handler, err := NewHandler(Services{Training: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/v1/training/manifests", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauth.Code)
	}
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedRequest(http.MethodGet, "/v1/training/manifests", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	query := httptest.NewRecorder()
	handler.ServeHTTP(query, authenticatedRequest(http.MethodGet, "/v1/training/manifest?manifest_id=training-manifest-aaaaaaaaaaaaaaaaaaaaaaaa", nil))
	if query.Code != http.StatusOK {
		t.Fatalf("query status=%d body=%s", query.Code, query.Body.String())
	}
	ref := runmodel.ArtifactRef{URI: "artifact://local/sha256/" + strings.Repeat("a", 64), SHA256: strings.Repeat("a", 64), SizeBytes: 1, Contract: "argus.test.v1"}
	command := TrainingMaterializeCommand{SchemaVersion: TrainingMaterializeCommandSchemaVersion, Request: training.MaterializationRequest{SchemaVersion: training.MaterializationRequestSchemaVersion, DatasetID: "dataset-1", DatasetRevision: "revision-1", RepositoryID: "repository-1", ConfigBundleRef: runmodel.ArtifactRef{URI: "artifact://local/sha256/" + strings.Repeat("b", 64), SHA256: strings.Repeat("b", 64), SizeBytes: 1, Contract: runmodel.ContractConfigBundle}, Cases: []training.CaseBinding{{CaseID: "case-1", ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1, ArtifactRefs: []runmodel.ArtifactRef{ref}, Authority: training.LabelAuthority{Kind: training.AuthorityHumanAdjudication, ReviewerIDs: []string{"reviewer-1"}, AdjudicatorID: "adjudicator-1", EvidenceRefs: []string{}, Statement: training.IndependentAuthorityStatement}}}, CreatedAt: now}, Mutation: MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "training-api-1", Audit: "materialize training manifest", At: now}}
	body, _ := json.Marshal(command)
	post := authenticatedRequest(http.MethodPost, "/v1/training/manifests", strings.NewReader(string(body)))
	post.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusCreated {
		t.Fatalf("post status=%d body=%s", response.Code, response.Body.String())
	}
	if stub.mutation.Actor != principal.Actor || len(stub.mutation.Roles) != 1 || stub.mutation.Roles[0] != evaluation.RoleDatasetCurator {
		t.Fatalf("principal was not injected: %+v", stub.mutation)
	}
	unknown := authenticatedRequest(http.MethodPost, "/v1/training/manifests", strings.NewReader(`{"schema_version":"argus.local_api_training_materialize_command.v1alpha1","unexpected":true}`))
	unknown.Header.Set("Content-Type", "application/json")
	strict := httptest.NewRecorder()
	handler.ServeHTTP(strict, unknown)
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict status=%d body=%s", strict.Code, strict.Body.String())
	}
	readOnly := principal
	readOnly.Permissions = []Permission{PermissionEvaluationRead}
	readHandler, err := NewHandler(Services{Training: stub}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodPost, "/v1/training/manifests", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	readHandler.ServeHTTP(denied, request)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("read-only post status=%d", denied.Code)
	}
}

func TestTrainingExportRoutesAreAuthenticatedAuthorizedStrictAndInjectPrincipal(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	manifestRef := runmodel.ArtifactRef{
		URI:       "artifact://local/sha256/" + strings.Repeat("a", 64),
		SHA256:    strings.Repeat("a", 64),
		SizeBytes: 1,
		Contract:  runmodel.ContractTrainingDatasetManifest,
	}
	stub := &trainingExportServiceStub{record: training.ExportRecord{Bundle: training.ExportBundle{ExportID: "training-export-1"}}}
	principal := Principal{
		SchemaVersion:   PrincipalSchemaVersion,
		Actor:           "training-curator",
		Roles:           []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions:     []Permission{PermissionEvaluationRead, PermissionEvaluationWrite},
		ProfileRevision: "training-export-v1",
	}
	handler, err := NewHandler(Services{TrainingExports: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/v1/training/exports", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauth.Code)
	}
	for _, path := range []string{
		"/v1/training/exports",
		"/v1/training/export?export_id=training-export-1",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	command := TrainingExportBuildCommand{
		SchemaVersion: TrainingExportBuildCommandSchemaVersion,
		Request: training.ExportRequest{
			SchemaVersion: training.ExportRequestSchemaVersion,
			ExportID:      "training-export-1",
			ManifestID:    "training-manifest-aaaaaaaaaaaaaaaaaaaaaaaa",
			ManifestRef:   manifestRef,
			Policy:        training.DefaultStrictRedactionPolicy(),
			CreatedAt:     now,
		},
		Mutation: MutationInput{
			SchemaVersion:  MutationInputSchemaVersion,
			IdempotencyKey: "training-export-api-1",
			Audit:          "build governed training export",
			At:             now,
		},
	}
	body, _ := json.Marshal(command)
	post := authenticatedRequest(http.MethodPost, "/v1/training/exports", strings.NewReader(string(body)))
	post.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusCreated {
		t.Fatalf("post status=%d body=%s", response.Code, response.Body.String())
	}
	if stub.mutation.Actor != principal.Actor || len(stub.mutation.Roles) != 1 || stub.mutation.Roles[0] != evaluation.RoleDatasetCurator {
		t.Fatalf("principal was not injected: %+v", stub.mutation)
	}
	unknown := authenticatedRequest(http.MethodPost, "/v1/training/exports", strings.NewReader(`{"schema_version":"argus.local_api_training_export_build_command.v1alpha1","unexpected":true}`))
	unknown.Header.Set("Content-Type", "application/json")
	strict := httptest.NewRecorder()
	handler.ServeHTTP(strict, unknown)
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict status=%d body=%s", strict.Code, strict.Body.String())
	}
	readOnly := principal
	readOnly.Permissions = []Permission{PermissionEvaluationRead}
	readHandler, err := NewHandler(Services{TrainingExports: stub}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodPost, "/v1/training/exports", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	readHandler.ServeHTTP(denied, request)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("read-only post status=%d", denied.Code)
	}
}

func TestTrainingJobRoutesInjectPrincipalAndKeepObservationSeparate(t *testing.T) {
	now := time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC)
	bundleRef := runmodel.ArtifactRef{URI: "artifact://local/sha256/" + strings.Repeat("b", 64), SHA256: strings.Repeat("b", 64), SizeBytes: 1, Contract: runmodel.ContractTrainingExportBundle}
	stub := &trainingJobServiceStub{record: training.JobRecord{Plan: training.JobPlan{Request: training.JobPrepareRequest{JobID: "training-job-1"}}}}
	principal := Principal{SchemaVersion: PrincipalSchemaVersion, Actor: "training-curator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Permissions: []Permission{PermissionEvaluationRead, PermissionEvaluationWrite}, ProfileRevision: "training-job-v1"}
	handler, err := NewHandler(Services{TrainingJobs: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/training/jobs", "/v1/training/job?job_id=training-job-1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	prepare := TrainingJobPrepareCommand{SchemaVersion: TrainingJobPrepareCommandSchemaVersion, Request: training.JobPrepareRequest{
		SchemaVersion: training.JobPrepareRequestSchemaVersion, JobID: "training-job-1", ExportID: "training-export-1", ExportBundleRef: bundleRef,
		ProviderID: "provider-example", ProviderProfileRevision: "profile-1", BaseModel: training.TrainingComponentRef{ID: "base-model", Revision: "revision-1", SHA256: strings.Repeat("a", 64)},
		Objective: training.TrainingObjectiveSFT, Hyperparameters: training.JobHyperparameters{Epochs: 3, BatchSize: 8, LearningRateMicros: 2000, Seed: 42}, CreatedAt: now,
	}, Mutation: MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "prepare-job-1", Audit: "prepare job", At: now}}
	body, _ := json.Marshal(prepare)
	request := authenticatedRequest(http.MethodPost, "/v1/training/jobs", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || stub.prepareInput.JobID != "training-job-1" || stub.mutation.Actor != principal.Actor {
		t.Fatalf("prepare status=%d input=%+v mutation=%+v body=%s", response.Code, stub.prepareInput, stub.mutation, response.Body.String())
	}
	receipt := training.ProviderJobReceipt{SchemaVersion: training.ProviderJobReceiptSchemaVersion, ProviderID: "provider-example", ExternalJobID: "external-job-1", Status: training.JobStatusSubmitted, Authority: training.TrainingReceiptAuthority, ContainsSecret: false, ObservedAt: now.Add(time.Minute)}
	receiptDigest, _ := runmodel.DigestJSON(receipt)
	receipt.SHA256 = receiptDigest
	observe := TrainingJobObserveCommand{SchemaVersion: TrainingJobObserveCommandSchemaVersion, Request: training.JobObservationRequest{SchemaVersion: training.JobObservationRequestSchemaVersion, JobID: "training-job-1", ExpectedPlanSHA256: strings.Repeat("c", 64), Receipt: receipt, ObservedAt: receipt.ObservedAt}, Mutation: MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "observe-job-1", Audit: "observe job", At: receipt.ObservedAt}}
	body, _ = json.Marshal(observe)
	request = authenticatedRequest(http.MethodPost, "/v1/training/job/observations", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || stub.observeInput.Receipt.ExternalJobID != "external-job-1" || stub.mutation.Actor != principal.Actor {
		t.Fatalf("observe status=%d input=%+v mutation=%+v body=%s", response.Code, stub.observeInput, stub.mutation, response.Body.String())
	}
	unknown := authenticatedRequest(http.MethodPost, "/v1/training/job/observations", strings.NewReader(`{"schema_version":"argus.local_api_training_job_observe_command.v1alpha1","unexpected":true}`))
	unknown.Header.Set("Content-Type", "application/json")
	strict := httptest.NewRecorder()
	handler.ServeHTTP(strict, unknown)
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict status=%d body=%s", strict.Code, strict.Body.String())
	}
}
