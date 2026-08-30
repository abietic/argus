package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
)

const (
	ExperimentBatchRequestSchemaVersion = "argus.experiment_batch_request.v1alpha1"
	ExperimentBatchResultSchemaVersion  = "argus.experiment_batch_result.v1alpha1"
	ReplayExecutorTemplateContract      = "argus.replay_executor_template.v1alpha1"
	experimentBatchEventSchemaVersion   = "argus.experiment_batch_event.v1alpha1"
	experimentBatchStream               = "evaluation/experiment-batches"
)

type ExperimentBatchStatus string

type BatchAttemptFailureCode string

const (
	ExperimentBatchRunning   ExperimentBatchStatus = "running"
	ExperimentBatchSucceeded ExperimentBatchStatus = "succeeded"

	BatchAttemptExecutionFailed BatchAttemptFailureCode = "execution_failed"
	BatchAttemptServiceStopping BatchAttemptFailureCode = "service_stopping"
)

// BatchAttemptFailure is a credential-safe durable observation. Raw provider,
// subprocess, path, or credential-bearing error text is deliberately excluded.
type BatchAttemptFailure struct {
	Code         BatchAttemptFailureCode `json:"code"`
	WorkerID     string                  `json:"worker_id"`
	LeaseID      string                  `json:"lease_id"`
	Generation   uint64                  `json:"generation"`
	FencingToken uint64                  `json:"fencing_token"`
	ObservedAt   time.Time               `json:"observed_at"`
}

func (failure BatchAttemptFailure) validate() error {
	if failure.Code != BatchAttemptExecutionFailed && failure.Code != BatchAttemptServiceStopping {
		return fmt.Errorf("unsupported batch attempt failure code %q", failure.Code)
	}
	if err := validateID("worker_id", failure.WorkerID); err != nil {
		return err
	}
	if err := validateID("lease_id", failure.LeaseID); err != nil {
		return err
	}
	if failure.Generation == 0 || failure.FencingToken == 0 || failure.ObservedAt.IsZero() || failure.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("batch attempt failure requires generation, fencing token, and UTC observed_at")
	}
	return nil
}

type ExperimentBatchCase struct {
	CaseID                       string                `json:"case_id"`
	ExpectedLabelRevision        uint64                `json:"expected_label_revision"`
	BaselineReviewRunID          string                `json:"baseline_review_run_id"`
	ExpectedBaselineConfigSHA256 string                `json:"expected_baseline_config_sha256"`
	ExpectedVariantConfigSHA256  string                `json:"expected_variant_config_sha256"`
	ExposureObservations         []ExposureObservation `json:"exposure_observations"`
}

type ExperimentBatchRequest struct {
	SchemaVersion           string                  `json:"schema_version"`
	BatchID                 string                  `json:"batch_id"`
	BaselineEvaluationRunID string                  `json:"baseline_evaluation_run_id"`
	VariantEvaluationRunID  string                  `json:"variant_evaluation_run_id"`
	ExperimentRunID         string                  `json:"experiment_run_id"`
	ExperimentRevision      string                  `json:"experiment_revision"`
	Variable                runmodel.ReplayVariable `json:"variable"`
	ExecutorRevision        string                  `json:"executor_revision"`
	ExecutorTemplateRef     *runmodel.ArtifactRef   `json:"executor_template_ref,omitempty"`
	MaxConcurrency          int                     `json:"max_concurrency"`
	Cases                   []ExperimentBatchCase   `json:"cases"`
	CreatedAt               time.Time               `json:"created_at"`
}

type ExperimentBatchCaseResult struct {
	CaseID              string               `json:"case_id"`
	BaselineReviewRunID string               `json:"baseline_review_run_id"`
	VariantReviewRunID  string               `json:"variant_review_run_id"`
	VariantReviewRunRef runmodel.ArtifactRef `json:"variant_review_run_ref"`
	ReplayChangeSetRef  runmodel.ArtifactRef `json:"replay_change_set_ref"`
}

type ExperimentBatchResult struct {
	SchemaVersion          string                      `json:"schema_version"`
	BatchID                string                      `json:"batch_id"`
	VariantEvaluationRunID string                      `json:"variant_evaluation_run_id"`
	ExperimentRunID        string                      `json:"experiment_run_id"`
	Cases                  []ExperimentBatchCaseResult `json:"cases"`
	CompletedAt            time.Time                   `json:"completed_at"`
	CompletedBy            string                      `json:"completed_by"`
}

type ExperimentBatchLease struct {
	LeaseID      string    `json:"lease_id"`
	BatchID      string    `json:"batch_id"`
	WorkerID     string    `json:"worker_id"`
	Generation   uint64    `json:"generation"`
	FencingToken uint64    `json:"fencing_token"`
	ClaimedAt    time.Time `json:"claimed_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type ExperimentBatchRecord struct {
	Request        ExperimentBatchRequest      `json:"request"`
	Intent         Mutation                    `json:"intent"`
	LastResume     *Mutation                   `json:"last_resume,omitempty"`
	LastFailure    *BatchAttemptFailure        `json:"last_failure,omitempty"`
	Status         ExperimentBatchStatus       `json:"status"`
	CompletedCases []ExperimentBatchCaseResult `json:"completed_cases"`
	Result         *ExperimentBatchResult      `json:"result,omitempty"`
	ActiveLease    *ExperimentBatchLease       `json:"active_lease,omitempty"`
	LastLease      *ExperimentBatchLease       `json:"last_lease,omitempty"`
	UpdatedAt      time.Time                   `json:"updated_at"`
	UpdatedBy      string                      `json:"updated_by"`
}

func (repository *Repository) RecordExperimentBatchAttemptFailure(
	ctx context.Context,
	batchID string,
	lease ExperimentBatchLease,
	failure BatchAttemptFailure,
	mutation Mutation,
) (ExperimentBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := lease.validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := failure.validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if lease.BatchID != batchID || failure.WorkerID != lease.WorkerID ||
		failure.LeaseID != lease.LeaseID || failure.Generation != lease.Generation ||
		failure.FencingToken != lease.FencingToken ||
		!failure.ObservedAt.Equal(mutation.At.UTC()) || mutation.Actor != failure.WorkerID {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: attempt failure does not bind exact lease and observer", ErrInvalidTransition)
	}
	event := experimentBatchEvent{
		SchemaVersion:  experimentBatchEventSchemaVersion,
		Type:           experimentBatchAttemptFailed,
		BatchID:        batchID,
		Lease:          clonePointer(lease),
		AttemptFailure: clonePointer(failure),
		Actor:          mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit,
		OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, experimentBatchStream,
			experimentBatchEventSchemaVersion, event, mutation); err != nil {
			return ExperimentBatchRecord{}, err
		}
		return state.experimentBatch(batchID)
	}
	record, exists := state.experimentBatches[batchID]
	if !exists || record.Status != ExperimentBatchRunning || record.ActiveLease == nil {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch has no active attempt", ErrInvalidTransition)
	}
	if !sameExperimentBatchLeaseIdentity(*record.ActiveLease, lease) {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch attempt is stale", ErrInvalidTransition)
	}
	baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if err := authorizeExperimentRuns(state, baseline, baseline, mutation.Roles); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if _, err := repository.appendExperimentBatch(
		mutation, event, state.streamSequences[experimentBatchStream],
	); err != nil {
		return ExperimentBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	return reloaded.experimentBatch(batchID)
}

func (lease ExperimentBatchLease) validate() error {
	for name, value := range map[string]string{
		"lease_id": lease.LeaseID, "batch_id": lease.BatchID, "worker_id": lease.WorkerID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if lease.Generation == 0 || lease.FencingToken == 0 {
		return fmt.Errorf("generation and fencing_token must be positive")
	}
	if lease.ClaimedAt.IsZero() || !lease.ExpiresAt.After(lease.ClaimedAt) {
		return fmt.Errorf("lease requires an increasing claim window")
	}
	return nil
}

func (request ExperimentBatchRequest) Validate() error {
	if request.SchemaVersion != ExperimentBatchRequestSchemaVersion {
		return fmt.Errorf("unsupported experiment batch request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"batch_id": request.BatchID, "baseline_evaluation_run_id": request.BaselineEvaluationRunID,
		"variant_evaluation_run_id": request.VariantEvaluationRunID,
		"experiment_run_id":         request.ExperimentRunID, "experiment_revision": request.ExperimentRevision,
		"executor_revision": request.ExecutorRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.BaselineEvaluationRunID == request.VariantEvaluationRunID {
		return fmt.Errorf("baseline and variant evaluation run IDs must differ")
	}
	if err := request.Variable.Validate(); err != nil {
		return err
	}
	if request.Variable == runmodel.ReplayVariableNone {
		return fmt.Errorf("batch variable must declare one changed dimension")
	}
	if request.ExecutorTemplateRef != nil {
		if err := validateReplayExecutorTemplateRef(*request.ExecutorTemplateRef); err != nil {
			return err
		}
	}
	if request.MaxConcurrency < 1 || request.MaxConcurrency > 16 {
		return fmt.Errorf("max_concurrency must be between 1 and 16")
	}
	if request.Cases == nil || len(request.Cases) == 0 || len(request.Cases) > 1024 {
		return fmt.Errorf("cases must contain between 1 and 1024 entries")
	}
	previous := ""
	for index, item := range request.Cases {
		if err := item.validate(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		if index > 0 && item.CaseID <= previous {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
		previous = item.CaseID
	}
	if request.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func validateReplayExecutorTemplateRef(ref runmodel.ArtifactRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("executor_template_ref: %w", err)
	}
	if ref.Contract != ReplayExecutorTemplateContract {
		return fmt.Errorf(
			"executor_template_ref contract is %q, want %q",
			ref.Contract, ReplayExecutorTemplateContract,
		)
	}
	return nil
}

func requireReplayExecutorTemplateRef(ref *runmodel.ArtifactRef) error {
	if ref == nil {
		return fmt.Errorf("executor_template_ref is required for durable batch admission")
	}
	return validateReplayExecutorTemplateRef(*ref)
}

func (item ExperimentBatchCase) validate() error {
	if err := validateID("case_id", item.CaseID); err != nil {
		return err
	}
	if item.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected_label_revision must be positive")
	}
	if err := validateID("baseline_review_run_id", item.BaselineReviewRunID); err != nil {
		return err
	}
	if err := validateSHA256("expected_baseline_config_sha256", item.ExpectedBaselineConfigSHA256); err != nil {
		return err
	}
	if err := validateSHA256("expected_variant_config_sha256", item.ExpectedVariantConfigSHA256); err != nil {
		return err
	}
	if item.ExpectedBaselineConfigSHA256 == item.ExpectedVariantConfigSHA256 {
		return fmt.Errorf("expected variant config must differ from baseline")
	}
	exposure := Exposure{
		SchemaVersion: ExposureSchemaVersion, EvaluationRunID: "validation-run",
		CaseID: item.CaseID, Observations: item.ExposureObservations,
		ObservedAt: time.Unix(1, 0).UTC(),
	}
	if err := exposure.Validate(); err != nil {
		return fmt.Errorf("exposure_observations: %w", err)
	}
	return nil
}

func (result ExperimentBatchResult) Validate() error {
	if result.SchemaVersion != ExperimentBatchResultSchemaVersion {
		return fmt.Errorf("unsupported experiment batch result schema %q", result.SchemaVersion)
	}
	for name, value := range map[string]string{
		"batch_id": result.BatchID, "variant_evaluation_run_id": result.VariantEvaluationRunID,
		"experiment_run_id": result.ExperimentRunID, "completed_by": result.CompletedBy,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if result.Cases == nil || len(result.Cases) == 0 {
		return fmt.Errorf("cases must be a non-empty array")
	}
	previous := ""
	for index, item := range result.Cases {
		if err := item.validate(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		if index > 0 && item.CaseID <= previous {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
		previous = item.CaseID
	}
	if result.CompletedAt.IsZero() {
		return fmt.Errorf("completed_at is required")
	}
	return nil
}

func (result ExperimentBatchCaseResult) validate() error {
	for name, value := range map[string]string{
		"case_id": result.CaseID, "baseline_review_run_id": result.BaselineReviewRunID,
		"variant_review_run_id": result.VariantReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if result.VariantReviewRunRef.Contract != runmodel.ContractReviewRun ||
		result.ReplayChangeSetRef.Contract != runmodel.ContractReplayChangeSet {
		return fmt.Errorf("invalid evidence contracts")
	}
	if err := result.VariantReviewRunRef.Validate(); err != nil {
		return fmt.Errorf("variant_review_run_ref: %w", err)
	}
	if err := result.ReplayChangeSetRef.Validate(); err != nil {
		return fmt.Errorf("replay_change_set_ref: %w", err)
	}
	return nil
}

func DecodeExperimentBatchRequest(data []byte) (ExperimentBatchRequest, error) {
	return decodeStrict(data, "ExperimentBatchRequest", func(value ExperimentBatchRequest) error {
		return value.Validate()
	})
}

func DecodeExperimentBatchResult(data []byte) (ExperimentBatchResult, error) {
	return decodeStrict(data, "ExperimentBatchResult", func(value ExperimentBatchResult) error {
		return value.Validate()
	})
}

func (repository *Repository) BeginExperimentBatch(
	ctx context.Context,
	request ExperimentBatchRequest,
	mutation Mutation,
) (ExperimentBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := requireReplayExecutorTemplateRef(request.ExecutorTemplateRef); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return ExperimentBatchRecord{}, fmt.Errorf("batch created_at must equal mutation time")
	}
	event := experimentBatchEvent{
		SchemaVersion: experimentBatchEventSchemaVersion, Type: experimentBatchIntentRecorded,
		Request: clonePointer(request), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles),
		Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, experimentBatchStream,
			experimentBatchEventSchemaVersion, event, mutation); err != nil {
			return ExperimentBatchRecord{}, err
		}
		return state.experimentBatch(request.BatchID)
	}
	if _, exists := state.experimentBatches[request.BatchID]; exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: batch_id %q already exists", ErrConflict, request.BatchID)
	}
	baseline, exists := state.evaluationRuns[request.BaselineEvaluationRunID]
	if !exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: baseline EvaluationRun", ErrNotFound)
	}
	if err := authorizeExperimentRuns(state, baseline, baseline, mutation.Roles); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := validateBatchAgainstBaseline(request, baseline); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if _, err := repository.appendExperimentBatch(mutation, event, state.streamSequences[experimentBatchStream]); err != nil {
		return ExperimentBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	return reloaded.experimentBatch(request.BatchID)
}

// RequestExperimentBatchResume records the operator action without changing
// the immutable intent authority used by claims, checkpoints, and terminal.
func (repository *Repository) RequestExperimentBatchResume(
	ctx context.Context,
	batchID string,
	mutation Mutation,
) (ExperimentBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	event := experimentBatchEvent{
		SchemaVersion: experimentBatchEventSchemaVersion,
		Type:          experimentBatchResumeRequested,
		BatchID:       batchID,
		Actor:         mutation.Actor,
		Roles:         slices.Clone(mutation.Roles),
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, experimentBatchStream,
			experimentBatchEventSchemaVersion, event, mutation); err != nil {
			return ExperimentBatchRecord{}, err
		}
		return state.experimentBatch(batchID)
	}
	record, exists := state.experimentBatches[batchID]
	if !exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch", ErrNotFound)
	}
	if record.Status != ExperimentBatchRunning {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch is terminal", ErrInvalidTransition)
	}
	if mutation.At.UTC().Before(record.UpdatedAt) {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: resume request predates batch state", ErrInvalidTransition)
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch baseline", ErrCorrupt)
	}
	if err := authorizeExperimentRuns(state, baseline, baseline, mutation.Roles); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if _, err := repository.appendExperimentBatch(
		mutation, event, state.streamSequences[experimentBatchStream],
	); err != nil {
		return ExperimentBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	return reloaded.experimentBatch(batchID)
}

func (repository *Repository) ClaimExperimentBatch(
	ctx context.Context, batchID, workerID string, leaseDuration time.Duration, mutation Mutation,
) (ExperimentBatchLease, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentBatchLease{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return ExperimentBatchLease{}, err
	}
	if err := validateID("worker_id", workerID); err != nil {
		return ExperimentBatchLease{}, err
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentBatchLease{}, err
	}
	if leaseDuration < time.Second || leaseDuration > time.Hour {
		return ExperimentBatchLease{}, fmt.Errorf("lease duration must be between one second and one hour")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchLease{}, err
	}
	record, exists := state.experimentBatches[batchID]
	if !exists || record.Status != ExperimentBatchRunning {
		return ExperimentBatchLease{}, fmt.Errorf("%w: batch is missing or terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy {
		return ExperimentBatchLease{}, fmt.Errorf("%w: only intent actor may claim batch", ErrUnauthorized)
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return ExperimentBatchLease{}, fmt.Errorf("%w: experiment batch %q baseline EvaluationRun is missing", ErrCorrupt, batchID)
	}
	if err := authorizeExperimentRuns(state, baseline, baseline, mutation.Roles); err != nil {
		return ExperimentBatchLease{}, err
	}
	if record.ActiveLease != nil && record.ActiveLease.ExpiresAt.After(mutation.At.UTC()) {
		if record.ActiveLease.WorkerID == workerID {
			return cloneValue(*record.ActiveLease), nil
		}
		return ExperimentBatchLease{}, fmt.Errorf("%w: batch has an active lease", ErrConflict)
	}
	generation := uint64(1)
	token := uint64(1)
	if record.LastLease != nil {
		generation = record.LastLease.Generation + 1
		token = record.LastLease.FencingToken + 1
	}
	lease := ExperimentBatchLease{LeaseID: fmt.Sprintf("%s-g%d", batchID, generation), BatchID: batchID, WorkerID: workerID, Generation: generation, FencingToken: token, ClaimedAt: mutation.At.UTC(), ExpiresAt: mutation.At.UTC().Add(leaseDuration)}
	event := experimentBatchEvent{SchemaVersion: experimentBatchEventSchemaVersion, Type: experimentBatchLeaseClaimed, BatchID: batchID, Lease: &lease, Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	if _, err := repository.appendExperimentBatch(mutation, event, state.streamSequences[experimentBatchStream]); err != nil {
		return ExperimentBatchLease{}, err
	}
	return lease, nil
}

func validateBatchLease(record ExperimentBatchRecord, lease ExperimentBatchLease, at time.Time) error {
	if record.ActiveLease == nil || !sameExperimentBatchLeaseIdentity(*record.ActiveLease, lease) ||
		lease.ExpiresAt.After(record.ActiveLease.ExpiresAt) || at.UTC().After(record.ActiveLease.ExpiresAt) {
		return fmt.Errorf("%w: experiment batch lease is stale", ErrInvalidTransition)
	}
	return nil
}

func sameExperimentBatchLeaseIdentity(left, right ExperimentBatchLease) bool {
	return left.LeaseID == right.LeaseID && left.BatchID == right.BatchID &&
		left.WorkerID == right.WorkerID && left.Generation == right.Generation &&
		left.FencingToken == right.FencingToken && left.ClaimedAt.Equal(right.ClaimedAt)
}

func (repository *Repository) RenewExperimentBatchLease(
	ctx context.Context,
	lease ExperimentBatchLease,
	leaseDuration time.Duration,
	mutation Mutation,
) (ExperimentBatchLease, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentBatchLease{}, err
	}
	if err := lease.validate(); err != nil {
		return ExperimentBatchLease{}, err
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentBatchLease{}, err
	}
	if leaseDuration < time.Second || leaseDuration > time.Hour {
		return ExperimentBatchLease{}, fmt.Errorf("lease duration must be between one second and one hour")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchLease{}, err
	}
	record, exists := state.experimentBatches[lease.BatchID]
	if !exists || record.Status != ExperimentBatchRunning {
		return ExperimentBatchLease{}, fmt.Errorf("%w: batch is missing or terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy || lease.WorkerID == "" {
		return ExperimentBatchLease{}, fmt.Errorf("%w: lease renewal authority differs", ErrUnauthorized)
	}
	if record.ActiveLease == nil || *record.ActiveLease != lease || mutation.At.UTC().After(lease.ExpiresAt) {
		return ExperimentBatchLease{}, fmt.Errorf("%w: experiment batch lease is stale", ErrInvalidTransition)
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return ExperimentBatchLease{}, fmt.Errorf("%w: experiment batch %q baseline EvaluationRun is missing", ErrCorrupt, lease.BatchID)
	}
	if err := authorizeExperimentRuns(state, baseline, baseline, mutation.Roles); err != nil {
		return ExperimentBatchLease{}, err
	}
	renewed := lease
	renewed.ExpiresAt = mutation.At.UTC().Add(leaseDuration)
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		return ExperimentBatchLease{}, fmt.Errorf("%w: lease renewal must extend expiry", ErrInvalidTransition)
	}
	event := experimentBatchEvent{
		SchemaVersion: experimentBatchEventSchemaVersion, Type: experimentBatchLeaseRenewed,
		BatchID: lease.BatchID, Lease: &renewed, Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	if _, err := repository.appendExperimentBatch(
		mutation, event, state.streamSequences[experimentBatchStream],
	); err != nil {
		return ExperimentBatchLease{}, err
	}
	return renewed, nil
}

func (repository *Repository) CompleteExperimentBatch(
	ctx context.Context,
	result ExperimentBatchResult,
	lease ExperimentBatchLease,
	mutation Mutation,
) (ExperimentBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := result.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if !result.CompletedAt.Equal(mutation.At.UTC()) || result.CompletedBy != mutation.Actor {
		return ExperimentBatchRecord{}, fmt.Errorf("batch completion must equal mutation authority")
	}
	event := experimentBatchEvent{
		SchemaVersion: experimentBatchEventSchemaVersion, Type: experimentBatchTerminalRecorded,
		Result: clonePointer(result), Lease: clonePointer(lease), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles),
		Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, experimentBatchStream,
			experimentBatchEventSchemaVersion, event, mutation); err != nil {
			return ExperimentBatchRecord{}, err
		}
		return state.experimentBatch(result.BatchID)
	}
	record, exists := state.experimentBatches[result.BatchID]
	if !exists || record.Status != ExperimentBatchRunning {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: batch is missing or already terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: only the batch intent actor may complete it",
			ErrUnauthorized)
	}
	if err := validateBatchLease(record, lease, mutation.At); err != nil {
		return ExperimentBatchRecord{}, err
	}
	baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if err := authorizeExperimentRuns(state, baseline, baseline, mutation.Roles); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if _, exists := state.evaluationRuns[result.VariantEvaluationRunID]; !exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: variant EvaluationRun", ErrNotFound)
	}
	if _, exists := state.experimentRuns[result.ExperimentRunID]; !exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: ExperimentRun", ErrNotFound)
	}
	if result.VariantEvaluationRunID != record.Request.VariantEvaluationRunID ||
		result.ExperimentRunID != record.Request.ExperimentRunID ||
		len(result.Cases) != len(record.Request.Cases) {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: batch result does not bind intent", ErrInvalidTransition)
	}
	if !slices.EqualFunc(record.CompletedCases, result.Cases,
		func(left, right ExperimentBatchCaseResult) bool { return left == right }) {
		return ExperimentBatchRecord{}, fmt.Errorf(
			"%w: batch terminal does not match durable case checkpoints", ErrInvalidTransition,
		)
	}
	if _, err := repository.appendExperimentBatch(mutation, event, state.streamSequences[experimentBatchStream]); err != nil {
		return ExperimentBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	return reloaded.experimentBatch(result.BatchID)
}

// RecordExperimentBatchCase checkpoints one fully host-revalidated replay.
// It is append-only and deliberately separate from the terminal aggregate so
// a process restart does not need to invoke an already completed provider
// execution again.
func (repository *Repository) RecordExperimentBatchCase(
	ctx context.Context,
	batchID string,
	result ExperimentBatchCaseResult,
	lease ExperimentBatchLease,
	mutation Mutation,
) (ExperimentBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := result.validate(); err != nil {
		return ExperimentBatchRecord{}, fmt.Errorf("validate experiment batch case result: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	event := experimentBatchEvent{
		SchemaVersion: experimentBatchEventSchemaVersion, Type: experimentBatchCaseCompleted,
		BatchID: batchID, CaseResult: clonePointer(result), Lease: clonePointer(lease), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, experimentBatchStream,
			experimentBatchEventSchemaVersion, event, mutation); err != nil {
			return ExperimentBatchRecord{}, err
		}
		return state.experimentBatch(batchID)
	}
	record, exists := state.experimentBatches[batchID]
	if !exists || record.Status != ExperimentBatchRunning || record.Result != nil {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: batch is missing or terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy {
		return ExperimentBatchRecord{}, fmt.Errorf(
			"%w: only the batch intent actor may checkpoint it", ErrUnauthorized,
		)
	}
	if err := validateBatchLease(record, lease, mutation.At); err != nil {
		return ExperimentBatchRecord{}, err
	}
	baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if err := authorizeExperimentRuns(state, baseline, baseline, mutation.Roles); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := validateBatchCaseCheckpoint(record, result); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if _, err := repository.appendExperimentBatch(mutation, event, state.streamSequences[experimentBatchStream]); err != nil {
		return ExperimentBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	return reloaded.experimentBatch(batchID)
}

func validateBatchCaseCheckpoint(
	record ExperimentBatchRecord,
	result ExperimentBatchCaseResult,
) error {
	index := sort.Search(len(record.Request.Cases), func(index int) bool {
		return record.Request.Cases[index].CaseID >= result.CaseID
	})
	if index == len(record.Request.Cases) || record.Request.Cases[index].CaseID != result.CaseID ||
		record.Request.Cases[index].BaselineReviewRunID != result.BaselineReviewRunID {
		return fmt.Errorf("%w: checkpoint does not bind a requested case", ErrInvalidTransition)
	}
	completedIndex := sort.Search(len(record.CompletedCases), func(index int) bool {
		return record.CompletedCases[index].CaseID >= result.CaseID
	})
	if completedIndex < len(record.CompletedCases) &&
		record.CompletedCases[completedIndex].CaseID == result.CaseID {
		return fmt.Errorf("%w: case %q already has a checkpoint", ErrConflict, result.CaseID)
	}
	return nil
}

func (repository *Repository) GetExperimentBatch(
	id string,
	access Access,
) (ExperimentBatchRecord, error) {
	if err := validateID("batch_id", id); err != nil {
		return ExperimentBatchRecord{}, err
	}
	if err := access.Validate(); err != nil {
		return ExperimentBatchRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	record, err := state.experimentBatch(id)
	if err != nil {
		return ExperimentBatchRecord{}, err
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch %q baseline EvaluationRun is missing", ErrCorrupt, id)
	}
	if err := authorizeExperimentRuns(state, baseline, baseline, access.Roles); err != nil {
		return ExperimentBatchRecord{}, err
	}
	return record, nil
}

// ListExperimentBatches returns only batches whose baseline evaluation cases
// are visible to the caller. Batch records are sorted by immutable batch ID so
// adapters can paginate deterministically without interpreting hidden cases.
func (repository *Repository) ListExperimentBatches(
	access Access,
) ([]ExperimentBatchRecord, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.experimentBatches))
	for id, record := range state.experimentBatches {
		baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if !exists {
			return nil, fmt.Errorf("%w: experiment batch %q baseline EvaluationRun is missing", ErrCorrupt, id)
		}
		if authErr := authorizeExperimentRuns(state, baseline, baseline, access.Roles); authErr == nil {
			ids = append(ids, id)
		} else if !errors.Is(authErr, ErrUnauthorized) {
			return nil, authErr
		}
	}
	sort.Strings(ids)
	result := make([]ExperimentBatchRecord, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneValue(state.experimentBatches[id]))
	}
	return result, nil
}

type ReplayCaseExecutionRequest struct {
	BatchID                     string
	CaseID                      string
	IdempotencyKey              string
	SourceReviewRunID           string
	Variable                    runmodel.ReplayVariable
	ExecutorRevision            string
	ExecutorTemplateRef         runmodel.ArtifactRef
	ExpectedVariantConfigSHA256 string
}

type ReplayCaseExecutor interface {
	ExecuteReplay(ctx context.Context, request ReplayCaseExecutionRequest) (string, error)
}

type ExperimentBatchRunner struct {
	repository    *Repository
	runs          EvaluationRunReader
	executor      ReplayCaseExecutor
	now           func() time.Time
	workerID      string
	leaseDuration time.Duration
	clockMu       sync.Mutex
}

func (runner *ExperimentBatchRunner) nowUTC() time.Time {
	runner.clockMu.Lock()
	defer runner.clockMu.Unlock()
	return runner.now().UTC()
}

type experimentBatchLeaseGuard struct {
	mu    sync.RWMutex
	lease ExperimentBatchLease
	err   error
}

func (guard *experimentBatchLeaseGuard) current() (ExperimentBatchLease, error) {
	guard.mu.RLock()
	defer guard.mu.RUnlock()
	return cloneValue(guard.lease), guard.err
}

func (guard *experimentBatchLeaseGuard) update(lease ExperimentBatchLease) {
	guard.mu.Lock()
	guard.lease = cloneValue(lease)
	guard.mu.Unlock()
}

func (guard *experimentBatchLeaseGuard) fail(err error) {
	guard.mu.Lock()
	if guard.err == nil {
		guard.err = err
	}
	guard.mu.Unlock()
}

func NewExperimentBatchRunner(
	repository *Repository,
	runs EvaluationRunReader,
	executor ReplayCaseExecutor,
	now func() time.Time,
	workerID string,
	leaseDuration time.Duration,
) (*ExperimentBatchRunner, error) {
	if repository == nil || runs == nil || executor == nil || now == nil || validateID("worker_id", workerID) != nil || leaseDuration < time.Second || leaseDuration > time.Hour {
		return nil, fmt.Errorf("experiment batch dependencies are required")
	}
	return &ExperimentBatchRunner{repository: repository, runs: runs, executor: executor, now: now, workerID: workerID, leaseDuration: leaseDuration}, nil
}

func (runner *ExperimentBatchRunner) Run(
	ctx context.Context,
	request ExperimentBatchRequest,
	mutation Mutation,
) (ExperimentBatchResult, error) {
	if err := request.Validate(); err != nil {
		return ExperimentBatchResult{}, err
	}
	if err := requireReplayExecutorTemplateRef(request.ExecutorTemplateRef); err != nil {
		return ExperimentBatchResult{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return ExperimentBatchResult{}, fmt.Errorf("batch created_at must equal intent mutation time")
	}
	record, err := runner.repository.BeginExperimentBatch(ctx, request, mutation)
	if err != nil {
		return ExperimentBatchResult{}, err
	}
	if record.Result != nil {
		return cloneValue(*record.Result), nil
	}
	claimAt := runner.nowUTC()
	lease, err := runner.repository.ClaimExperimentBatch(ctx, request.BatchID, runner.workerID, runner.leaseDuration,
		derivedBatchMutation(mutation, request.BatchID, "claim", runner.workerID, claimAt))
	if err != nil {
		return ExperimentBatchResult{}, err
	}
	workerContext, leaseGuard, stopHeartbeat := runner.startBatchLeaseHeartbeat(ctx, request, mutation, lease)
	defer stopHeartbeat()
	access := Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	baseline, err := runner.repository.GetEvaluationRun(request.BaselineEvaluationRunID, access)
	if err != nil {
		return ExperimentBatchResult{}, fmt.Errorf("load baseline EvaluationRun: %w", err)
	}
	if err := validateBatchAgainstBaseline(request, baseline); err != nil {
		return ExperimentBatchResult{}, err
	}
	for _, item := range request.Cases {
		exposure := Exposure{
			SchemaVersion: ExposureSchemaVersion, EvaluationRunID: request.VariantEvaluationRunID,
			CaseID: item.CaseID, Observations: slices.Clone(item.ExposureObservations),
			ObservedAt: request.CreatedAt.UTC(),
		}
		if _, err := runner.repository.RecordExposure(workerContext, exposure, derivedBatchMutation(
			mutation, request.BatchID, "exposure", item.CaseID, request.CreatedAt.UTC(),
		)); err != nil {
			return ExperimentBatchResult{}, fmt.Errorf("record exposure for case %q: %w", item.CaseID, err)
		}
	}

	caseResults, err := runner.executeAndCheckpointCases(
		workerContext, request, mutation, leaseGuard.current, record.CompletedCases,
	)
	if err != nil {
		return ExperimentBatchResult{}, err
	}
	bindings := make([]EvaluationCaseRunBinding, 0, len(caseResults))
	for index, result := range caseResults {
		bindings = append(bindings, EvaluationCaseRunBinding{
			CaseID: result.CaseID, ExpectedLabelRevision: request.Cases[index].ExpectedLabelRevision,
			ReviewRunID: result.VariantReviewRunID,
		})
	}
	variantEvaluation, err := runner.resolveOrRecordVariantEvaluation(
		workerContext, request, mutation, access, bindings, baseline.EvaluatorRevision,
	)
	if err != nil {
		return ExperimentBatchResult{}, err
	}
	experiment, err := runner.resolveOrRecordExperiment(
		workerContext, request, mutation, access, variantEvaluation,
	)
	if err != nil {
		return ExperimentBatchResult{}, err
	}
	if err := stopHeartbeat(); err != nil {
		return ExperimentBatchResult{}, err
	}
	completedAt := runner.nowUTC()
	currentLease, err := leaseGuard.current()
	if err != nil {
		return ExperimentBatchResult{}, err
	}
	currentLease, err = runner.repository.RenewExperimentBatchLease(
		ctx, currentLease, runner.leaseDuration,
		derivedBatchMutation(mutation, request.BatchID, "heartbeat", fmt.Sprintf("%d", completedAt.UnixNano()), completedAt),
	)
	if err != nil {
		return ExperimentBatchResult{}, fmt.Errorf("renew batch lease before terminal: %w", err)
	}
	leaseGuard.update(currentLease)
	result := ExperimentBatchResult{
		SchemaVersion: ExperimentBatchResultSchemaVersion, BatchID: request.BatchID,
		VariantEvaluationRunID: variantEvaluation.EvaluationRunID,
		ExperimentRunID:        experiment.ExperimentRunID, Cases: caseResults,
		CompletedAt: completedAt, CompletedBy: mutation.Actor,
	}
	terminalMutation := derivedBatchMutation(
		mutation, request.BatchID, "terminal", "all", completedAt,
	)
	completed, err := runner.repository.CompleteExperimentBatch(ctx, result, currentLease, terminalMutation)
	if err != nil {
		return ExperimentBatchResult{}, err
	}
	return cloneValue(*completed.Result), nil
}

func (runner *ExperimentBatchRunner) startBatchLeaseHeartbeat(
	ctx context.Context,
	request ExperimentBatchRequest,
	mutation Mutation,
	lease ExperimentBatchLease,
) (context.Context, *experimentBatchLeaseGuard, func() error) {
	workerContext, cancel := context.WithCancel(ctx)
	guard := &experimentBatchLeaseGuard{lease: cloneValue(lease)}
	done := make(chan struct{})
	stopped := make(chan struct{})
	interval := runner.leaseDuration / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		renew := func() bool {
			current, err := guard.current()
			if err != nil {
				return false
			}
			at := runner.nowUTC()
			renewed, err := runner.repository.RenewExperimentBatchLease(
				workerContext, current, runner.leaseDuration,
				derivedBatchMutation(mutation, request.BatchID, "heartbeat", fmt.Sprintf("%d", at.UnixNano()), at),
			)
			if err != nil {
				guard.fail(fmt.Errorf("renew experiment batch lease: %w", err))
				cancel()
				return false
			}
			guard.update(renewed)
			return true
		}
		if !renew() {
			return
		}
		for {
			select {
			case <-done:
				return
			case <-workerContext.Done():
				guard.fail(workerContext.Err())
				return
			case <-ticker.C:
				if !renew() {
					return
				}
			}
		}
	}()
	var once sync.Once
	stop := func() error {
		once.Do(func() { close(done); <-stopped; cancel() })
		_, err := guard.current()
		return err
	}
	return workerContext, guard, stop
}

func validateBatchAgainstBaseline(request ExperimentBatchRequest, baseline EvaluationRun) error {
	if len(request.Cases) != len(baseline.Results) {
		return fmt.Errorf("%w: batch cases do not match baseline EvaluationRun", ErrInvalidTransition)
	}
	for index, item := range request.Cases {
		result := baseline.Results[index]
		if item.CaseID != result.CaseID || item.ExpectedLabelRevision != result.LabelRevision ||
			item.BaselineReviewRunID != result.ReviewRunID {
			return fmt.Errorf("%w: batch case %q does not bind baseline result",
				ErrInvalidTransition, item.CaseID)
		}
	}
	return nil
}

func (runner *ExperimentBatchRunner) executeCases(
	ctx context.Context,
	request ExperimentBatchRequest,
) ([]ExperimentBatchCaseResult, error) {
	return runner.executeCaseIndexes(ctx, request, allBatchCaseIndexes(request), nil)
}

func (runner *ExperimentBatchRunner) executeAndCheckpointCases(
	ctx context.Context,
	request ExperimentBatchRequest,
	mutation Mutation,
	currentLease func() (ExperimentBatchLease, error),
	completed []ExperimentBatchCaseResult,
) ([]ExperimentBatchCaseResult, error) {
	results := make([]ExperimentBatchCaseResult, len(request.Cases))
	completedByID := make(map[string]ExperimentBatchCaseResult, len(completed))
	for _, result := range completed {
		if err := result.validate(); err != nil {
			return nil, fmt.Errorf("invalid durable case checkpoint: %w", err)
		}
		completedByID[result.CaseID] = result
	}
	missing := make([]int, 0, len(request.Cases)-len(completed))
	for index, item := range request.Cases {
		if result, exists := completedByID[item.CaseID]; exists {
			if result.BaselineReviewRunID != item.BaselineReviewRunID {
				return nil, fmt.Errorf("durable case checkpoint differs from batch intent")
			}
			results[index] = result
			continue
		}
		missing = append(missing, index)
	}
	if len(completedByID) != len(completed) || len(completed)+len(missing) != len(request.Cases) {
		return nil, fmt.Errorf("durable case checkpoints do not match batch intent")
	}
	if len(missing) == 0 {
		return results, nil
	}
	newResults, err := runner.executeCaseIndexes(
		ctx, request, missing,
		func(index int, result ExperimentBatchCaseResult) error {
			lease, leaseErr := currentLease()
			if leaseErr != nil {
				return leaseErr
			}
			checkpointAt := runner.nowUTC()
			_, checkpointErr := runner.repository.RecordExperimentBatchCase(
				ctx, request.BatchID, result, lease,
				derivedBatchMutation(
					mutation, request.BatchID, "checkpoint", request.Cases[index].CaseID, checkpointAt,
				),
			)
			return checkpointErr
		},
	)
	if err != nil {
		return nil, err
	}
	for index, result := range newResults {
		results[missing[index]] = result
	}
	return results, nil
}

func allBatchCaseIndexes(request ExperimentBatchRequest) []int {
	indexes := make([]int, len(request.Cases))
	for index := range request.Cases {
		indexes[index] = index
	}
	return indexes
}

func (runner *ExperimentBatchRunner) executeCaseIndexes(
	ctx context.Context,
	request ExperimentBatchRequest,
	indexes []int,
	checkpoint func(index int, result ExperimentBatchCaseResult) error,
) ([]ExperimentBatchCaseResult, error) {
	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	workerCount := min(request.MaxConcurrency, len(indexes))
	jobs := make(chan int)
	results := make([]ExperimentBatchCaseResult, len(indexes))
	errorsByPosition := make([]error, len(indexes))
	var wait sync.WaitGroup
	for range workerCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for position := range jobs {
				index := indexes[position]
				result, err := runner.executeCase(workerContext, request, request.Cases[index])
				if err == nil && checkpoint != nil {
					err = checkpoint(index, result)
				}
				results[position], errorsByPosition[position] = result, err
				if err != nil {
					cancel()
				}
			}
		}()
	}
	scheduled := 0
schedule:
	for position := range indexes {
		select {
		case jobs <- position:
			scheduled++
		case <-workerContext.Done():
			break schedule
		}
	}
	close(jobs)
	wait.Wait()
	for position := 0; position < scheduled; position++ {
		if errorsByPosition[position] != nil {
			index := indexes[position]
			return nil, fmt.Errorf("execute replay for case %q: %w",
				request.Cases[index].CaseID, errorsByPosition[position])
		}
	}
	if scheduled != len(indexes) {
		return nil, workerContext.Err()
	}
	return results, nil
}

func (runner *ExperimentBatchRunner) executeCase(
	ctx context.Context,
	request ExperimentBatchRequest,
	item ExperimentBatchCase,
) (ExperimentBatchCaseResult, error) {
	baselineSnapshot, err := runner.runs.ExecutionSnapshotForRun(item.BaselineReviewRunID)
	if err != nil {
		return ExperimentBatchCaseResult{}, fmt.Errorf("load baseline execution snapshot: %w", err)
	}
	var baselineConfig reviewconfig.ConfigBundle
	if err := runner.runs.ReadJSONArtifact(baselineSnapshot.ConfigBundleRef, &baselineConfig); err != nil ||
		baselineConfig.SHA256 != item.ExpectedBaselineConfigSHA256 {
		return ExperimentBatchCaseResult{}, fmt.Errorf("baseline config digest does not match batch intent")
	}
	variantRunID, err := runner.executor.ExecuteReplay(ctx, ReplayCaseExecutionRequest{
		BatchID: request.BatchID, CaseID: item.CaseID,
		IdempotencyKey:    stableBatchKey(request.BatchID, "replay", item.CaseID),
		SourceReviewRunID: item.BaselineReviewRunID, Variable: request.Variable,
		ExecutorRevision:            request.ExecutorRevision,
		ExecutorTemplateRef:         *request.ExecutorTemplateRef,
		ExpectedVariantConfigSHA256: item.ExpectedVariantConfigSHA256,
	})
	if err != nil {
		return ExperimentBatchCaseResult{}, err
	}
	variantRun, err := runner.runs.LoadRun(variantRunID)
	if err != nil || variantRun.Status != runmodel.RunStatusSucceeded ||
		variantRun.Kind != runmodel.RunKindReplay || variantRun.SourceRunID != item.BaselineReviewRunID ||
		variantRun.ReplayVariable != request.Variable {
		return ExperimentBatchCaseResult{}, fmt.Errorf("executor result is not the declared committed replay")
	}
	variantRef, err := runner.runs.CommittedRunRef(variantRunID)
	if err != nil {
		return ExperimentBatchCaseResult{}, err
	}
	variantSnapshot, err := runner.runs.ExecutionSnapshotForRun(variantRunID)
	if err != nil || variantSnapshot.ReplayChangeSetRef == nil {
		return ExperimentBatchCaseResult{}, fmt.Errorf("variant replay change set is missing")
	}
	var variantConfig reviewconfig.ConfigBundle
	if err := runner.runs.ReadJSONArtifact(variantSnapshot.ConfigBundleRef, &variantConfig); err != nil ||
		variantConfig.SHA256 != item.ExpectedVariantConfigSHA256 {
		return ExperimentBatchCaseResult{}, fmt.Errorf("variant config digest does not match batch intent")
	}
	var change runmodel.ReplayChangeSet
	if err := runner.runs.ReadJSONArtifact(*variantSnapshot.ReplayChangeSetRef, &change); err != nil {
		return ExperimentBatchCaseResult{}, err
	}
	if err := change.Validate(); err != nil || change.SourceRunID != item.BaselineReviewRunID ||
		change.Variable != request.Variable || change.BaselineSHA256 != item.ExpectedBaselineConfigSHA256 ||
		change.VariantSHA256 != item.ExpectedVariantConfigSHA256 || change.RemoteWrites != "deny" {
		return ExperimentBatchCaseResult{}, fmt.Errorf("variant replay change set does not match batch intent")
	}
	return ExperimentBatchCaseResult{
		CaseID: item.CaseID, BaselineReviewRunID: item.BaselineReviewRunID,
		VariantReviewRunID: variantRunID, VariantReviewRunRef: variantRef,
		ReplayChangeSetRef: *variantSnapshot.ReplayChangeSetRef,
	}, nil
}

func (runner *ExperimentBatchRunner) resolveOrRecordVariantEvaluation(
	ctx context.Context,
	request ExperimentBatchRequest,
	mutation Mutation,
	access Access,
	bindings []EvaluationCaseRunBinding,
	evaluatorRevision string,
) (EvaluationRun, error) {
	if existing, err := runner.repository.GetEvaluationRun(request.VariantEvaluationRunID, access); err == nil {
		if !evaluationBindingsMatch(existing, bindings) {
			return EvaluationRun{}, fmt.Errorf("%w: existing variant EvaluationRun differs", ErrConflict)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return EvaluationRun{}, err
	}
	createdAt := runner.nowUTC()
	return runner.repository.RecordEvaluationRun(ctx, EvaluationRunRequest{
		SchemaVersion:   EvaluationRunRequestSchemaVersion,
		EvaluationRunID: request.VariantEvaluationRunID, EvaluatorRevision: evaluatorRevision,
		Bindings: bindings, CreatedAt: request.CreatedAt.UTC(),
	}, derivedBatchMutation(mutation, request.BatchID, "evaluation", "all", createdAt), runner.runs)
}

func (runner *ExperimentBatchRunner) resolveOrRecordExperiment(
	ctx context.Context,
	request ExperimentBatchRequest,
	mutation Mutation,
	access Access,
	variant EvaluationRun,
) (ExperimentRun, error) {
	if existing, err := runner.repository.GetExperimentRun(request.ExperimentRunID, access); err == nil {
		if existing.BaselineEvaluationRunID != request.BaselineEvaluationRunID ||
			existing.VariantEvaluationRunID != variant.EvaluationRunID || existing.Variable != request.Variable {
			return ExperimentRun{}, fmt.Errorf("%w: existing ExperimentRun differs", ErrConflict)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return ExperimentRun{}, err
	}
	completedAt := runner.nowUTC()
	return runner.repository.RecordExperimentRun(ctx, ExperimentRunRequest{
		SchemaVersion:   ExperimentRunRequestSchemaVersion,
		ExperimentRunID: request.ExperimentRunID, ExperimentRevision: request.ExperimentRevision,
		BaselineEvaluationRunID: request.BaselineEvaluationRunID,
		VariantEvaluationRunID:  request.VariantEvaluationRunID,
		Variable:                request.Variable, CreatedAt: request.CreatedAt.UTC(),
	}, derivedBatchMutation(mutation, request.BatchID, "experiment", "all", completedAt), runner.runs)
}

func evaluationBindingsMatch(run EvaluationRun, bindings []EvaluationCaseRunBinding) bool {
	if len(run.Results) != len(bindings) {
		return false
	}
	for index, binding := range bindings {
		result := run.Results[index]
		if result.CaseID != binding.CaseID || result.LabelRevision != binding.ExpectedLabelRevision ||
			result.ReviewRunID != binding.ReviewRunID {
			return false
		}
	}
	return true
}

func derivedBatchMutation(
	base Mutation,
	batchID, kind, caseID string,
	at time.Time,
) Mutation {
	return Mutation{
		IdempotencyKey: stableBatchKey(batchID, kind, caseID), Actor: base.Actor,
		Roles: slices.Clone(base.Roles), Audit: "experiment batch " + kind, At: at.UTC(),
	}
}

func stableBatchKey(batchID, kind, caseID string) string {
	sum := sha256.Sum256([]byte(batchID + "\x00" + kind + "\x00" + caseID))
	return "batch-" + kind + "-" + hex.EncodeToString(sum[:12])
}

func sortedBatchResults(results []ExperimentBatchCaseResult) []ExperimentBatchCaseResult {
	copyResults := slices.Clone(results)
	sort.Slice(copyResults, func(left, right int) bool { return copyResults[left].CaseID < copyResults[right].CaseID })
	return copyResults
}
