package training

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/runrepo"
)

type trainingJobFixture struct {
	trainingFixture
	exporter *Exporter
	service  *JobService
	request  JobPrepareRequest
	mutation evaluation.Mutation
}

func newTrainingJobFixture(t *testing.T) trainingJobFixture {
	t.Helper()
	fixture := newTrainingFixture(t)
	manifest, err := fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	exporter, err := NewExporter(fixture.store, fixture.repository, fixture.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	exportAt := fixture.mutation.At.Add(time.Minute)
	exportRequest := ExportRequest{SchemaVersion: ExportRequestSchemaVersion, ExportID: "training-export-1", ManifestID: manifest.Manifest.ManifestID, ManifestRef: manifest.ManifestRef, Policy: DefaultStrictRedactionPolicy(), CreatedAt: exportAt}
	exportMutation := evaluation.Mutation{IdempotencyKey: "training-export-1", Actor: "curator-2", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "build export", At: exportAt}
	export, err := exporter.Build(context.Background(), exportRequest, exportMutation)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewJobService(fixture.store, exporter, fixture.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	at := exportAt.Add(time.Minute)
	request := JobPrepareRequest{
		SchemaVersion: JobPrepareRequestSchemaVersion, JobID: "training-job-1",
		ExportID: export.Bundle.ExportID, ExportBundleRef: export.BundleRef,
		ProviderID: "provider-example", ProviderProfileRevision: "profile-1",
		BaseModel:       TrainingComponentRef{ID: "base-model", Revision: "revision-1", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Objective:       TrainingObjectiveSFT,
		Hyperparameters: JobHyperparameters{Epochs: 3, BatchSize: 8, LearningRateMicros: 2000, Seed: 42},
		CreatedAt:       at,
	}
	mutation := evaluation.Mutation{IdempotencyKey: "training-job-prepare-1", Actor: "curator-3", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "prepare external training job", At: at}
	return trainingJobFixture{trainingFixture: fixture, exporter: exporter, service: service, request: request, mutation: mutation}
}

func jobReceipt(t *testing.T, providerID, externalID string, status JobStatus, output string, at time.Time) ProviderJobReceipt {
	t.Helper()
	receipt, err := sealProviderJobReceipt(ProviderJobReceipt{ProviderID: providerID, ExternalJobID: externalID, Status: status, OutputModelID: output, Authority: TrainingReceiptAuthority, ContainsSecret: false, ObservedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestTrainingJobPrepareObserveExactRetryAndRestore(t *testing.T) {
	fixture := newTrainingJobFixture(t)
	prepared, err := fixture.service.Prepare(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Plan.Status != JobStatusPrepared || prepared.Plan.PromotionEligible || prepared.Plan.RemoteSideEffects != "deny" {
		t.Fatalf("prepared plan = %+v", prepared.Plan)
	}
	retry, err := fixture.service.Prepare(context.Background(), fixture.request, fixture.mutation)
	if err != nil || retry.PlanRef != prepared.PlanRef {
		t.Fatalf("prepare retry = %+v, %v", retry, err)
	}
	submittedAt := fixture.mutation.At.Add(time.Minute)
	submittedRequest := JobObservationRequest{SchemaVersion: JobObservationRequestSchemaVersion, JobID: fixture.request.JobID, ExpectedPlanSHA256: prepared.Plan.SHA256, Receipt: jobReceipt(t, fixture.request.ProviderID, "external-job-1", JobStatusSubmitted, "", submittedAt), ObservedAt: submittedAt}
	submittedMutation := evaluation.Mutation{IdempotencyKey: "training-job-submit-1", Actor: "curator-3", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "record external submission", At: submittedAt}
	submitted, err := fixture.service.Observe(context.Background(), submittedRequest, submittedMutation)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := submittedAt.Add(time.Minute)
	completedRequest := JobObservationRequest{SchemaVersion: JobObservationRequestSchemaVersion, JobID: fixture.request.JobID, ExpectedPlanSHA256: submitted.Plan.SHA256, Receipt: jobReceipt(t, fixture.request.ProviderID, "external-job-1", JobStatusSucceeded, "fine-tuned-model-1", completedAt), ObservedAt: completedAt}
	completedMutation := evaluation.Mutation{IdempotencyKey: "training-job-complete-1", Actor: "curator-4", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "record external completion", At: completedAt}
	completed, err := fixture.service.Observe(context.Background(), completedRequest, completedMutation)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Plan.Status != JobStatusSucceeded || completed.Plan.PromotionEligible || len(completed.Plan.Observations) != 2 {
		t.Fatalf("completed plan = %+v", completed.Plan)
	}
	restarted, err := NewJobService(fixture.store, fixture.exporter, fixture.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.Get(fixture.request.JobID, evaluation.Access{Actor: "curator-4", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}})
	if err != nil || loaded.Plan.SHA256 != completed.Plan.SHA256 {
		t.Fatalf("restored = %+v, %v", loaded, err)
	}
}

func TestTrainingJobRejectsCASConflictInvalidTransitionAndQuarantinedReceipt(t *testing.T) {
	fixture := newTrainingJobFixture(t)
	prepared, err := fixture.service.Prepare(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	at := fixture.mutation.At.Add(time.Minute)
	terminalFirst := JobObservationRequest{SchemaVersion: JobObservationRequestSchemaVersion, JobID: fixture.request.JobID, ExpectedPlanSHA256: prepared.Plan.SHA256, Receipt: jobReceipt(t, fixture.request.ProviderID, "external-job-1", JobStatusSucceeded, "model-1", at), ObservedAt: at}
	mutation := evaluation.Mutation{IdempotencyKey: "terminal-first", Actor: "curator-3", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "invalid", At: at}
	if _, err := fixture.service.Observe(context.Background(), terminalFirst, mutation); !errors.Is(err, ErrJobInvalidTransition) {
		t.Fatalf("terminal-first error = %v", err)
	}
	submittedRequest := terminalFirst
	submittedRequest.Receipt = jobReceipt(t, fixture.request.ProviderID, "external-job-1", JobStatusSubmitted, "", at)
	mutation.IdempotencyKey = "submit"
	submitted, err := fixture.service.Observe(context.Background(), submittedRequest, mutation)
	if err != nil {
		t.Fatal(err)
	}
	stale := JobObservationRequest{SchemaVersion: JobObservationRequestSchemaVersion, JobID: fixture.request.JobID, ExpectedPlanSHA256: prepared.Plan.SHA256, Receipt: jobReceipt(t, fixture.request.ProviderID, "external-job-1", JobStatusFailed, "", at.Add(time.Minute)), ObservedAt: at.Add(time.Minute)}
	staleMutation := mutation
	staleMutation.IdempotencyKey, staleMutation.At = "stale", stale.ObservedAt
	if _, err := fixture.service.Observe(context.Background(), stale, staleMutation); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("stale error = %v", err)
	}
	completed := stale
	completed.ExpectedPlanSHA256 = submitted.Plan.SHA256
	staleMutation.IdempotencyKey = "complete"
	record, err := fixture.service.Observe(context.Background(), completed, staleMutation)
	if err != nil {
		t.Fatal(err)
	}
	lastRef := record.Plan.Observations[len(record.Plan.Observations)-1].ReceiptRef
	_, err = fixture.artifacts.QuarantineArtifact(context.Background(), lastRef, "test receipt quarantine", runrepo.ArtifactIntegrityMutation{IdempotencyKey: "quarantine-job-receipt", Actor: "integrity-admin", Audit: "test", At: staleMutation.At.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewJobService(fixture.store, fixture.exporter, fixture.artifacts); !errors.Is(err, ErrJobCorrupt) {
		t.Fatalf("reopen error = %v, want ErrJobCorrupt", err)
	}
}
