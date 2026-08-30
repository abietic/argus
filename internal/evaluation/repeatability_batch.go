package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	RepeatabilityBatchRequestSchemaVersion = "argus.repeatability_batch_request.v1alpha1"
	RepeatabilityBatchResultSchemaVersion  = "argus.repeatability_batch_result.v1alpha1"
	repeatabilityBatchEventSchemaVersion   = "argus.repeatability_batch_event.v1alpha1"
	repeatabilityBatchStream               = "evaluation/repeatability-batches"
)

type RepeatabilityBatchStatus string

const (
	RepeatabilityBatchRunning   RepeatabilityBatchStatus = "running"
	RepeatabilityBatchSucceeded RepeatabilityBatchStatus = "succeeded"
)

type RepeatabilityBatchCase struct {
	CaseID                       string                `json:"case_id"`
	ExpectedLabelRevision        uint64                `json:"expected_label_revision"`
	BaselineReviewRunID          string                `json:"baseline_review_run_id"`
	ExpectedBaselineConfigSHA256 string                `json:"expected_baseline_config_sha256"`
	ExposureObservations         []ExposureObservation `json:"exposure_observations"`
}

// RepeatabilityBatchRequest freezes the complete sample matrix before any
// provider call. ReplayEvaluationRunIDs are sample identities and must be
// uniquely sorted; each sample executes every case directly from its baseline.
type RepeatabilityBatchRequest struct {
	SchemaVersion           string                   `json:"schema_version"`
	BatchID                 string                   `json:"batch_id"`
	BaselineEvaluationRunID string                   `json:"baseline_evaluation_run_id"`
	ReplayEvaluationRunIDs  []string                 `json:"replay_evaluation_run_ids"`
	RepeatabilityRunID      string                   `json:"repeatability_run_id"`
	RepeatabilityRevision   string                   `json:"repeatability_revision"`
	ExecutorRevision        string                   `json:"executor_revision"`
	ExecutorTemplateRef     *runmodel.ArtifactRef    `json:"executor_template_ref,omitempty"`
	MaxConcurrency          int                      `json:"max_concurrency"`
	Cases                   []RepeatabilityBatchCase `json:"cases"`
	CreatedAt               time.Time                `json:"created_at"`
}

type RepeatabilityBatchCaseResult struct {
	EvaluationRunID     string               `json:"evaluation_run_id"`
	CaseID              string               `json:"case_id"`
	BaselineReviewRunID string               `json:"baseline_review_run_id"`
	ReplayReviewRunID   string               `json:"replay_review_run_id"`
	ReplayReviewRunRef  runmodel.ArtifactRef `json:"replay_review_run_ref"`
	ReplayChangeSetRef  runmodel.ArtifactRef `json:"replay_change_set_ref"`
}

type RepeatabilityBatchResult struct {
	SchemaVersion          string                         `json:"schema_version"`
	BatchID                string                         `json:"batch_id"`
	ReplayEvaluationRunIDs []string                       `json:"replay_evaluation_run_ids"`
	RepeatabilityRunID     string                         `json:"repeatability_run_id"`
	Cases                  []RepeatabilityBatchCaseResult `json:"cases"`
	CompletedAt            time.Time                      `json:"completed_at"`
	CompletedBy            string                         `json:"completed_by"`
}

type RepeatabilityBatchLease struct {
	LeaseID      string    `json:"lease_id"`
	BatchID      string    `json:"batch_id"`
	WorkerID     string    `json:"worker_id"`
	Generation   uint64    `json:"generation"`
	FencingToken uint64    `json:"fencing_token"`
	ClaimedAt    time.Time `json:"claimed_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type RepeatabilityBatchRecord struct {
	Request        RepeatabilityBatchRequest      `json:"request"`
	Intent         Mutation                       `json:"intent"`
	LastResume     *Mutation                      `json:"last_resume,omitempty"`
	LastFailure    *BatchAttemptFailure           `json:"last_failure,omitempty"`
	Status         RepeatabilityBatchStatus       `json:"status"`
	CompletedCases []RepeatabilityBatchCaseResult `json:"completed_cases"`
	Result         *RepeatabilityBatchResult      `json:"result,omitempty"`
	ActiveLease    *RepeatabilityBatchLease       `json:"active_lease,omitempty"`
	LastLease      *RepeatabilityBatchLease       `json:"last_lease,omitempty"`
	UpdatedAt      time.Time                      `json:"updated_at"`
	UpdatedBy      string                         `json:"updated_by"`
}

type repeatabilityBatchEventType string

const (
	repeatabilityBatchIntentRecorded   repeatabilityBatchEventType = "intent_recorded"
	repeatabilityBatchResumeRequested  repeatabilityBatchEventType = "resume_requested"
	repeatabilityBatchAttemptFailed    repeatabilityBatchEventType = "attempt_failed"
	repeatabilityBatchLeaseClaimed     repeatabilityBatchEventType = "lease_claimed"
	repeatabilityBatchLeaseRenewed     repeatabilityBatchEventType = "lease_renewed"
	repeatabilityBatchCaseCompleted    repeatabilityBatchEventType = "case_completed"
	repeatabilityBatchTerminalRecorded repeatabilityBatchEventType = "terminal_recorded"
)

type repeatabilityBatchEvent struct {
	SchemaVersion  string                        `json:"schema_version"`
	Type           repeatabilityBatchEventType   `json:"type"`
	Request        *RepeatabilityBatchRequest    `json:"request,omitempty"`
	BatchID        string                        `json:"batch_id,omitempty"`
	CaseResult     *RepeatabilityBatchCaseResult `json:"case_result,omitempty"`
	Result         *RepeatabilityBatchResult     `json:"result,omitempty"`
	Lease          *RepeatabilityBatchLease      `json:"lease,omitempty"`
	AttemptFailure *BatchAttemptFailure          `json:"attempt_failure,omitempty"`
	Actor          string                        `json:"actor"`
	Roles          []Role                        `json:"roles"`
	Audit          string                        `json:"audit"`
	OccurredAt     time.Time                     `json:"occurred_at"`
}

func (repository *Repository) RecordRepeatabilityBatchAttemptFailure(
	ctx context.Context,
	batchID string,
	lease RepeatabilityBatchLease,
	failure BatchAttemptFailure,
	mutation Mutation,
) (RepeatabilityBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := lease.validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := failure.validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if lease.BatchID != batchID || failure.WorkerID != lease.WorkerID ||
		failure.LeaseID != lease.LeaseID || failure.Generation != lease.Generation ||
		failure.FencingToken != lease.FencingToken ||
		!failure.ObservedAt.Equal(mutation.At.UTC()) || mutation.Actor != failure.WorkerID {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: attempt failure does not bind exact lease and observer", ErrInvalidTransition)
	}
	event := repeatabilityBatchEvent{
		SchemaVersion:  repeatabilityBatchEventSchemaVersion,
		Type:           repeatabilityBatchAttemptFailed,
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
		return RepeatabilityBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, repeatabilityBatchStream,
			repeatabilityBatchEventSchemaVersion, event, mutation); err != nil {
			return RepeatabilityBatchRecord{}, err
		}
		return state.repeatabilityBatch(batchID)
	}
	record, exists := state.repeatabilityBatches[batchID]
	if !exists || record.Status != RepeatabilityBatchRunning || record.ActiveLease == nil {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch has no active attempt", ErrInvalidTransition)
	}
	if !sameRepeatabilityBatchLeaseIdentity(*record.ActiveLease, lease) {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch attempt is stale", ErrInvalidTransition)
	}
	baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, mutation.Roles); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if _, err := repository.appendRepeatabilityBatch(
		mutation, event, state.streamSequences[repeatabilityBatchStream],
	); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	return reloaded.repeatabilityBatch(batchID)
}

func (lease RepeatabilityBatchLease) validate() error {
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

func (request RepeatabilityBatchRequest) Validate() error {
	if request.SchemaVersion != RepeatabilityBatchRequestSchemaVersion {
		return fmt.Errorf("unsupported repeatability batch request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"batch_id":                   request.BatchID,
		"baseline_evaluation_run_id": request.BaselineEvaluationRunID,
		"repeatability_run_id":       request.RepeatabilityRunID,
		"repeatability_revision":     request.RepeatabilityRevision,
		"executor_revision":          request.ExecutorRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.ReplayEvaluationRunIDs == nil || len(request.ReplayEvaluationRunIDs) == 0 ||
		len(request.ReplayEvaluationRunIDs) >= maxRepeatabilitySamples {
		return fmt.Errorf("replay_evaluation_run_ids must contain between 1 and %d entries", maxRepeatabilitySamples-1)
	}
	previousEvaluation := ""
	for index, id := range request.ReplayEvaluationRunIDs {
		if err := validateID("replay_evaluation_run_ids", id); err != nil {
			return fmt.Errorf("replay_evaluation_run_ids[%d]: %w", index, err)
		}
		if id == request.BaselineEvaluationRunID {
			return fmt.Errorf("baseline evaluation run cannot be a replay sample")
		}
		if index > 0 && id <= previousEvaluation {
			return fmt.Errorf("replay_evaluation_run_ids must be uniquely sorted")
		}
		previousEvaluation = id
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
	previousCase := ""
	for index, item := range request.Cases {
		if err := item.validate(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		if index > 0 && item.CaseID <= previousCase {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
		previousCase = item.CaseID
	}
	if request.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func (item RepeatabilityBatchCase) validate() error {
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

func (result RepeatabilityBatchCaseResult) validate() error {
	for name, value := range map[string]string{
		"evaluation_run_id": result.EvaluationRunID, "case_id": result.CaseID,
		"baseline_review_run_id": result.BaselineReviewRunID,
		"replay_review_run_id":   result.ReplayReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if result.ReplayReviewRunRef.Contract != runmodel.ContractReviewRun ||
		result.ReplayChangeSetRef.Contract != runmodel.ContractReplayChangeSet {
		return fmt.Errorf("invalid replay evidence contracts")
	}
	if err := result.ReplayReviewRunRef.Validate(); err != nil {
		return fmt.Errorf("replay_review_run_ref: %w", err)
	}
	if err := result.ReplayChangeSetRef.Validate(); err != nil {
		return fmt.Errorf("replay_change_set_ref: %w", err)
	}
	return nil
}

func (result RepeatabilityBatchResult) Validate() error {
	if result.SchemaVersion != RepeatabilityBatchResultSchemaVersion {
		return fmt.Errorf("unsupported repeatability batch result schema %q", result.SchemaVersion)
	}
	for name, value := range map[string]string{
		"batch_id": result.BatchID, "repeatability_run_id": result.RepeatabilityRunID,
		"completed_by": result.CompletedBy,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if result.ReplayEvaluationRunIDs == nil || len(result.ReplayEvaluationRunIDs) == 0 {
		return fmt.Errorf("replay_evaluation_run_ids must be a non-empty array")
	}
	previousEvaluation := ""
	for index, id := range result.ReplayEvaluationRunIDs {
		if err := validateID("replay_evaluation_run_ids", id); err != nil {
			return err
		}
		if index > 0 && id <= previousEvaluation {
			return fmt.Errorf("replay_evaluation_run_ids must be uniquely sorted")
		}
		previousEvaluation = id
	}
	if result.Cases == nil || len(result.Cases) == 0 {
		return fmt.Errorf("cases must be a non-empty array")
	}
	previousKey := ""
	for index, item := range result.Cases {
		if err := item.validate(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		key := repeatabilityBatchCaseKey(item.EvaluationRunID, item.CaseID)
		if index > 0 && key <= previousKey {
			return fmt.Errorf("cases must be uniquely sorted by evaluation_run_id and case_id")
		}
		previousKey = key
	}
	if result.CompletedAt.IsZero() {
		return fmt.Errorf("completed_at is required")
	}
	return nil
}

func DecodeRepeatabilityBatchRequest(data []byte) (RepeatabilityBatchRequest, error) {
	return decodeStrict(data, "RepeatabilityBatchRequest", func(value RepeatabilityBatchRequest) error {
		return value.Validate()
	})
}

func DecodeRepeatabilityBatchResult(data []byte) (RepeatabilityBatchResult, error) {
	return decodeStrict(data, "RepeatabilityBatchResult", func(value RepeatabilityBatchResult) error {
		return value.Validate()
	})
}

func (repository *Repository) BeginRepeatabilityBatch(
	ctx context.Context,
	request RepeatabilityBatchRequest,
	mutation Mutation,
) (RepeatabilityBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := requireReplayExecutorTemplateRef(request.ExecutorTemplateRef); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return RepeatabilityBatchRecord{}, fmt.Errorf("batch created_at must equal mutation time")
	}
	event := repeatabilityBatchEvent{
		SchemaVersion: repeatabilityBatchEventSchemaVersion, Type: repeatabilityBatchIntentRecorded,
		Request: clonePointer(request), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles),
		Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, repeatabilityBatchStream,
			repeatabilityBatchEventSchemaVersion, event, mutation); err != nil {
			return RepeatabilityBatchRecord{}, err
		}
		return state.repeatabilityBatch(request.BatchID)
	}
	if _, exists := state.repeatabilityBatches[request.BatchID]; exists {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: batch_id %q already exists", ErrConflict, request.BatchID)
	}
	if err := validateRepeatabilityBatchOutputIdentities(state, request, false); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	baseline, exists := state.evaluationRuns[request.BaselineEvaluationRunID]
	if !exists {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: baseline EvaluationRun", ErrNotFound)
	}
	if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, mutation.Roles); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := validateRepeatabilityBatchAgainstBaseline(request, baseline); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if _, err := repository.appendRepeatabilityBatch(
		mutation, event, state.streamSequences[repeatabilityBatchStream],
	); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	return reloaded.repeatabilityBatch(request.BatchID)
}

func (repository *Repository) RequestRepeatabilityBatchResume(
	ctx context.Context,
	batchID string,
	mutation Mutation,
) (RepeatabilityBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	event := repeatabilityBatchEvent{
		SchemaVersion: repeatabilityBatchEventSchemaVersion,
		Type:          repeatabilityBatchResumeRequested,
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
		return RepeatabilityBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, repeatabilityBatchStream,
			repeatabilityBatchEventSchemaVersion, event, mutation); err != nil {
			return RepeatabilityBatchRecord{}, err
		}
		return state.repeatabilityBatch(batchID)
	}
	record, exists := state.repeatabilityBatches[batchID]
	if !exists {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch", ErrNotFound)
	}
	if record.Status != RepeatabilityBatchRunning {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch is terminal", ErrInvalidTransition)
	}
	if mutation.At.UTC().Before(record.UpdatedAt) {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: resume request predates batch state", ErrInvalidTransition)
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch baseline", ErrCorrupt)
	}
	if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, mutation.Roles); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if _, err := repository.appendRepeatabilityBatch(
		mutation, event, state.streamSequences[repeatabilityBatchStream],
	); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	return reloaded.repeatabilityBatch(batchID)
}

func (repository *Repository) ClaimRepeatabilityBatch(
	ctx context.Context,
	batchID string,
	workerID string,
	leaseDuration time.Duration,
	mutation Mutation,
) (RepeatabilityBatchLease, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if err := validateID("worker_id", workerID); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if leaseDuration < time.Second || leaseDuration > time.Hour {
		return RepeatabilityBatchLease{}, fmt.Errorf("lease duration must be between one second and one hour")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RepeatabilityBatchLease{}, err
	}
	record, exists := state.repeatabilityBatches[batchID]
	if !exists || record.Status != RepeatabilityBatchRunning {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: batch is missing or terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: only intent actor may claim batch", ErrUnauthorized)
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: repeatability batch %q baseline EvaluationRun is missing", ErrCorrupt, batchID)
	}
	if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, mutation.Roles); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if record.ActiveLease != nil && record.ActiveLease.ExpiresAt.After(mutation.At.UTC()) {
		if record.ActiveLease.WorkerID == workerID {
			return cloneValue(*record.ActiveLease), nil
		}
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: batch has an active lease", ErrConflict)
	}
	generation, token := uint64(1), uint64(1)
	if record.LastLease != nil {
		generation = record.LastLease.Generation + 1
		token = record.LastLease.FencingToken + 1
	}
	lease := RepeatabilityBatchLease{
		LeaseID: fmt.Sprintf("%s-g%d", batchID, generation), BatchID: batchID,
		WorkerID: workerID, Generation: generation, FencingToken: token,
		ClaimedAt: mutation.At.UTC(), ExpiresAt: mutation.At.UTC().Add(leaseDuration),
	}
	event := repeatabilityBatchEvent{
		SchemaVersion: repeatabilityBatchEventSchemaVersion, Type: repeatabilityBatchLeaseClaimed,
		BatchID: batchID, Lease: &lease, Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles),
		Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	if _, err := repository.appendRepeatabilityBatch(
		mutation, event, state.streamSequences[repeatabilityBatchStream],
	); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	return lease, nil
}

func validateRepeatabilityBatchLease(
	record RepeatabilityBatchRecord,
	lease RepeatabilityBatchLease,
	at time.Time,
) error {
	if record.ActiveLease == nil || !sameRepeatabilityBatchLeaseIdentity(*record.ActiveLease, lease) ||
		lease.ExpiresAt.After(record.ActiveLease.ExpiresAt) || at.UTC().After(record.ActiveLease.ExpiresAt) {
		return fmt.Errorf("%w: repeatability batch lease is stale", ErrInvalidTransition)
	}
	return nil
}

func sameRepeatabilityBatchLeaseIdentity(left, right RepeatabilityBatchLease) bool {
	return left.LeaseID == right.LeaseID && left.BatchID == right.BatchID &&
		left.WorkerID == right.WorkerID && left.Generation == right.Generation &&
		left.FencingToken == right.FencingToken && left.ClaimedAt.Equal(right.ClaimedAt)
}

func (repository *Repository) RenewRepeatabilityBatchLease(
	ctx context.Context,
	lease RepeatabilityBatchLease,
	leaseDuration time.Duration,
	mutation Mutation,
) (RepeatabilityBatchLease, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if err := lease.validate(); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	if leaseDuration < time.Second || leaseDuration > time.Hour {
		return RepeatabilityBatchLease{}, fmt.Errorf("lease duration must be between one second and one hour")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RepeatabilityBatchLease{}, err
	}
	record, exists := state.repeatabilityBatches[lease.BatchID]
	if !exists || record.Status != RepeatabilityBatchRunning {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: batch is missing or terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: lease renewal authority differs", ErrUnauthorized)
	}
	if record.ActiveLease == nil || *record.ActiveLease != lease || mutation.At.UTC().After(lease.ExpiresAt) {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: repeatability batch lease is stale", ErrInvalidTransition)
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: repeatability batch %q baseline EvaluationRun is missing", ErrCorrupt, lease.BatchID)
	}
	if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, mutation.Roles); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	renewed := lease
	renewed.ExpiresAt = mutation.At.UTC().Add(leaseDuration)
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		return RepeatabilityBatchLease{}, fmt.Errorf("%w: lease renewal must extend expiry", ErrInvalidTransition)
	}
	event := repeatabilityBatchEvent{
		SchemaVersion: repeatabilityBatchEventSchemaVersion, Type: repeatabilityBatchLeaseRenewed,
		BatchID: lease.BatchID, Lease: &renewed, Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	if _, err := repository.appendRepeatabilityBatch(
		mutation, event, state.streamSequences[repeatabilityBatchStream],
	); err != nil {
		return RepeatabilityBatchLease{}, err
	}
	return renewed, nil
}

func (repository *Repository) RecordRepeatabilityBatchCase(
	ctx context.Context,
	batchID string,
	result RepeatabilityBatchCaseResult,
	lease RepeatabilityBatchLease,
	mutation Mutation,
) (RepeatabilityBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := result.validate(); err != nil {
		return RepeatabilityBatchRecord{}, fmt.Errorf("validate repeatability batch case result: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	event := repeatabilityBatchEvent{
		SchemaVersion: repeatabilityBatchEventSchemaVersion, Type: repeatabilityBatchCaseCompleted,
		BatchID: batchID, CaseResult: clonePointer(result), Lease: clonePointer(lease),
		Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit,
		OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, repeatabilityBatchStream,
			repeatabilityBatchEventSchemaVersion, event, mutation); err != nil {
			return RepeatabilityBatchRecord{}, err
		}
		return state.repeatabilityBatch(batchID)
	}
	record, exists := state.repeatabilityBatches[batchID]
	if !exists || record.Status != RepeatabilityBatchRunning || record.Result != nil {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: batch is missing or terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: only intent actor may checkpoint batch", ErrUnauthorized)
	}
	if err := validateRepeatabilityBatchLease(record, lease, mutation.At); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, mutation.Roles); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := validateRepeatabilityBatchCaseCheckpoint(record, result); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if _, err := repository.appendRepeatabilityBatch(
		mutation, event, state.streamSequences[repeatabilityBatchStream],
	); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	return reloaded.repeatabilityBatch(batchID)
}

func (repository *Repository) CompleteRepeatabilityBatch(
	ctx context.Context,
	result RepeatabilityBatchResult,
	lease RepeatabilityBatchLease,
	mutation Mutation,
) (RepeatabilityBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := result.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if !result.CompletedAt.Equal(mutation.At.UTC()) || result.CompletedBy != mutation.Actor {
		return RepeatabilityBatchRecord{}, fmt.Errorf("batch completion must equal mutation authority")
	}
	event := repeatabilityBatchEvent{
		SchemaVersion: repeatabilityBatchEventSchemaVersion, Type: repeatabilityBatchTerminalRecorded,
		Result: clonePointer(result), Lease: clonePointer(lease), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, repeatabilityBatchStream,
			repeatabilityBatchEventSchemaVersion, event, mutation); err != nil {
			return RepeatabilityBatchRecord{}, err
		}
		return state.repeatabilityBatch(result.BatchID)
	}
	record, exists := state.repeatabilityBatches[result.BatchID]
	if !exists || record.Status != RepeatabilityBatchRunning {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: batch is missing or terminal", ErrInvalidTransition)
	}
	if mutation.Actor != record.UpdatedBy {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: only intent actor may complete batch", ErrUnauthorized)
	}
	if err := validateRepeatabilityBatchLease(record, lease, mutation.At); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if !slices.Equal(result.ReplayEvaluationRunIDs, record.Request.ReplayEvaluationRunIDs) ||
		result.RepeatabilityRunID != record.Request.RepeatabilityRunID ||
		len(result.Cases) != len(record.Request.Cases)*len(record.Request.ReplayEvaluationRunIDs) ||
		!reflect.DeepEqual(result.Cases, record.CompletedCases) {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: batch terminal does not bind intent and checkpoints", ErrInvalidTransition)
	}
	for _, id := range result.ReplayEvaluationRunIDs {
		if _, exists := state.evaluationRuns[id]; !exists {
			return RepeatabilityBatchRecord{}, fmt.Errorf("%w: replay EvaluationRun %q", ErrNotFound, id)
		}
	}
	if _, exists := state.repeatabilityRuns[result.RepeatabilityRunID]; !exists {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: RepeatabilityRun", ErrNotFound)
	}
	if _, err := repository.appendRepeatabilityBatch(
		mutation, event, state.streamSequences[repeatabilityBatchStream],
	); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	return reloaded.repeatabilityBatch(result.BatchID)
}

func (repository *Repository) GetRepeatabilityBatch(
	id string,
	access Access,
) (RepeatabilityBatchRecord, error) {
	if err := validateID("batch_id", id); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	if err := access.Validate(); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	record, err := state.repeatabilityBatch(id)
	if err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
	if !exists {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch %q baseline EvaluationRun is missing", ErrCorrupt, id)
	}
	if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, access.Roles); err != nil {
		return RepeatabilityBatchRecord{}, err
	}
	return record, nil
}

// ListRepeatabilityBatches returns only batches whose baseline evaluation
// cases are visible to the caller. The full sample matrix stays bound to the
// authorized baseline rather than being exposed through an ungoverned index.
func (repository *Repository) ListRepeatabilityBatches(
	access Access,
) ([]RepeatabilityBatchRecord, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.repeatabilityBatches))
	for id, record := range state.repeatabilityBatches {
		baseline, exists := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if !exists {
			return nil, fmt.Errorf("%w: repeatability batch %q baseline EvaluationRun is missing", ErrCorrupt, id)
		}
		if authErr := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, access.Roles); authErr == nil {
			ids = append(ids, id)
		} else if !errors.Is(authErr, ErrUnauthorized) {
			return nil, authErr
		}
	}
	sort.Strings(ids)
	result := make([]RepeatabilityBatchRecord, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneValue(state.repeatabilityBatches[id]))
	}
	return result, nil
}

func validateRepeatabilityBatchAgainstBaseline(
	request RepeatabilityBatchRequest,
	baseline EvaluationRun,
) error {
	if len(request.Cases) != len(baseline.Results) {
		return fmt.Errorf("%w: batch cases do not match baseline EvaluationRun", ErrInvalidTransition)
	}
	for index, item := range request.Cases {
		result := baseline.Results[index]
		if result.CaseType == CaseFixValidation {
			return fmt.Errorf("%w: fix_validation is not admitted to repeatability batch", ErrInvalidTransition)
		}
		if item.CaseID != result.CaseID || item.ExpectedLabelRevision != result.LabelRevision ||
			item.BaselineReviewRunID != result.ReviewRunID {
			return fmt.Errorf("%w: batch case %q does not bind baseline result", ErrInvalidTransition, item.CaseID)
		}
	}
	return nil
}

func validateRepeatabilityBatchCaseCheckpoint(
	record RepeatabilityBatchRecord,
	result RepeatabilityBatchCaseResult,
) error {
	if !slices.Contains(record.Request.ReplayEvaluationRunIDs, result.EvaluationRunID) {
		return fmt.Errorf("%w: checkpoint evaluation sample is not requested", ErrInvalidTransition)
	}
	caseIndex := sort.Search(len(record.Request.Cases), func(index int) bool {
		return record.Request.Cases[index].CaseID >= result.CaseID
	})
	if caseIndex == len(record.Request.Cases) ||
		record.Request.Cases[caseIndex].CaseID != result.CaseID ||
		record.Request.Cases[caseIndex].BaselineReviewRunID != result.BaselineReviewRunID {
		return fmt.Errorf("%w: checkpoint does not bind a requested case", ErrInvalidTransition)
	}
	key := repeatabilityBatchCaseKey(result.EvaluationRunID, result.CaseID)
	completedIndex := sort.Search(len(record.CompletedCases), func(index int) bool {
		return repeatabilityBatchCaseResultKey(record.CompletedCases[index]) >= key
	})
	if completedIndex < len(record.CompletedCases) &&
		repeatabilityBatchCaseResultKey(record.CompletedCases[completedIndex]) == key {
		return fmt.Errorf("%w: sample case %q already has a checkpoint", ErrConflict, key)
	}
	for _, completed := range record.CompletedCases {
		if completed.ReplayReviewRunID == result.ReplayReviewRunID {
			return fmt.Errorf("%w: replay ReviewRun %q is already used by another sample", ErrConflict, result.ReplayReviewRunID)
		}
	}
	return nil
}

func validateRepeatabilityBatchOutputIdentities(
	state *projectionState,
	request RepeatabilityBatchRequest,
	allowMaterialized bool,
) error {
	if !allowMaterialized {
		for _, id := range request.ReplayEvaluationRunIDs {
			if _, exists := state.evaluationRuns[id]; exists {
				return fmt.Errorf("%w: replay EvaluationRun %q already exists", ErrConflict, id)
			}
		}
		if _, exists := state.repeatabilityRuns[request.RepeatabilityRunID]; exists {
			return fmt.Errorf("%w: RepeatabilityRun %q already exists", ErrConflict, request.RepeatabilityRunID)
		}
	}
	for _, record := range state.repeatabilityBatches {
		if record.Request.BatchID == request.BatchID {
			continue
		}
		if record.Request.RepeatabilityRunID == request.RepeatabilityRunID {
			return fmt.Errorf("%w: RepeatabilityRun output is reserved by batch %q", ErrConflict, record.Request.BatchID)
		}
		for _, id := range request.ReplayEvaluationRunIDs {
			if slices.Contains(record.Request.ReplayEvaluationRunIDs, id) {
				return fmt.Errorf("%w: replay EvaluationRun %q is reserved by batch %q", ErrConflict, id, record.Request.BatchID)
			}
		}
	}
	return nil
}

func (state *projectionState) applyRepeatabilityBatch(envelope local.Envelope) error {
	if envelope.Schema != repeatabilityBatchEventSchemaVersion {
		return fmt.Errorf("unsupported repeatability batch event schema %q", envelope.Schema)
	}
	var event repeatabilityBatchEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode repeatability batch event: %w", err)
	}
	if event.SchemaVersion != repeatabilityBatchEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
		return err
	}
	switch event.Type {
	case repeatabilityBatchIntentRecorded:
		if event.Request == nil || event.CaseResult != nil || event.Result != nil || event.Lease != nil || event.AttemptFailure != nil || event.BatchID != "" {
			return fmt.Errorf("intent event must contain only request")
		}
		if err := event.Request.Validate(); err != nil {
			return err
		}
		if !event.Request.CreatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("batch request created_at does not match event")
		}
		if _, exists := state.repeatabilityBatches[event.Request.BatchID]; exists {
			return fmt.Errorf("duplicate repeatability batch")
		}
		if err := validateRepeatabilityBatchOutputIdentities(state, *event.Request, true); err != nil {
			return err
		}
		baseline, exists := state.evaluationRuns[event.Request.BaselineEvaluationRunID]
		if !exists {
			return fmt.Errorf("repeatability batch references missing baseline EvaluationRun")
		}
		if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, event.Roles); err != nil {
			return err
		}
		if err := validateRepeatabilityBatchAgainstBaseline(*event.Request, baseline); err != nil {
			return err
		}
		state.repeatabilityBatches[event.Request.BatchID] = RepeatabilityBatchRecord{
			Request: cloneValue(*event.Request),
			Intent: Mutation{
				IdempotencyKey: envelope.ID, Actor: event.Actor, Roles: slices.Clone(event.Roles),
				Audit: event.Audit, At: event.OccurredAt,
			},
			Status:         RepeatabilityBatchRunning,
			CompletedCases: []RepeatabilityBatchCaseResult{}, UpdatedAt: event.OccurredAt,
			UpdatedBy: event.Actor,
		}
	case repeatabilityBatchResumeRequested:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease != nil || event.AttemptFailure != nil || event.BatchID == "" {
			return fmt.Errorf("resume_requested event shape is invalid")
		}
		record, exists := state.repeatabilityBatches[event.BatchID]
		if !exists || record.Status != RepeatabilityBatchRunning {
			return fmt.Errorf("resume request references missing or terminal batch")
		}
		if event.OccurredAt.Before(record.UpdatedAt) {
			return fmt.Errorf("resume request predates batch state")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, event.Roles); err != nil {
			return err
		}
		record.LastResume = &Mutation{
			IdempotencyKey: envelope.ID, Actor: event.Actor, Roles: slices.Clone(event.Roles),
			Audit: event.Audit, At: event.OccurredAt,
		}
		record.UpdatedAt = event.OccurredAt
		state.repeatabilityBatches[event.BatchID] = record
	case repeatabilityBatchAttemptFailed:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease == nil || event.AttemptFailure == nil || event.BatchID == "" {
			return fmt.Errorf("attempt_failed event shape is invalid")
		}
		if err := event.Lease.validate(); err != nil {
			return err
		}
		if err := event.AttemptFailure.validate(); err != nil {
			return err
		}
		record, exists := state.repeatabilityBatches[event.BatchID]
		if !exists || record.Status != RepeatabilityBatchRunning || record.ActiveLease == nil ||
			*record.ActiveLease != *event.Lease {
			return fmt.Errorf("attempt failure references missing, terminal, or stale batch lease")
		}
		failure := event.AttemptFailure
		if event.Actor != failure.WorkerID || event.Lease.WorkerID != failure.WorkerID ||
			event.Lease.LeaseID != failure.LeaseID || event.Lease.Generation != failure.Generation ||
			event.Lease.FencingToken != failure.FencingToken ||
			!event.OccurredAt.Equal(failure.ObservedAt) || event.OccurredAt.Before(record.UpdatedAt) {
			return fmt.Errorf("attempt failure does not bind exact lease and observation")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, event.Roles); err != nil {
			return err
		}
		record.LastFailure = clonePointer(*failure)
		record.UpdatedAt = event.OccurredAt
		state.repeatabilityBatches[event.BatchID] = record
	case repeatabilityBatchLeaseClaimed:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease == nil || event.AttemptFailure != nil || event.BatchID == "" {
			return fmt.Errorf("lease_claimed event shape is invalid")
		}
		if err := event.Lease.validate(); err != nil {
			return err
		}
		record, exists := state.repeatabilityBatches[event.BatchID]
		if !exists || record.Status != RepeatabilityBatchRunning || event.Lease.BatchID != event.BatchID {
			return fmt.Errorf("lease claim references missing or terminal batch")
		}
		if event.Actor != record.UpdatedBy || !event.Lease.ClaimedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("lease claim authority or time differs")
		}
		if record.ActiveLease != nil && record.ActiveLease.ExpiresAt.After(event.OccurredAt) {
			return fmt.Errorf("lease claim overlaps active lease")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, event.Roles); err != nil {
			return err
		}
		wantGeneration, wantToken := uint64(1), uint64(1)
		if record.LastLease != nil {
			wantGeneration = record.LastLease.Generation + 1
			wantToken = record.LastLease.FencingToken + 1
		}
		if event.Lease.Generation != wantGeneration || event.Lease.FencingToken != wantToken {
			return fmt.Errorf("lease generation or fencing token is not monotonic")
		}
		record.ActiveLease = clonePointer(*event.Lease)
		record.LastLease = clonePointer(*event.Lease)
		record.UpdatedAt = event.OccurredAt
		state.repeatabilityBatches[event.BatchID] = record
	case repeatabilityBatchLeaseRenewed:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease == nil || event.AttemptFailure != nil || event.BatchID == "" {
			return fmt.Errorf("lease_renewed event shape is invalid")
		}
		if err := event.Lease.validate(); err != nil {
			return err
		}
		record, exists := state.repeatabilityBatches[event.BatchID]
		if !exists || record.Status != RepeatabilityBatchRunning || record.ActiveLease == nil {
			return fmt.Errorf("lease renewal references missing, terminal, or unclaimed batch")
		}
		previous := *record.ActiveLease
		if event.Actor != record.UpdatedBy || event.Lease.BatchID != event.BatchID ||
			event.Lease.LeaseID != previous.LeaseID || event.Lease.WorkerID != previous.WorkerID ||
			event.Lease.Generation != previous.Generation || event.Lease.FencingToken != previous.FencingToken ||
			!event.Lease.ClaimedAt.Equal(previous.ClaimedAt) || event.OccurredAt.After(previous.ExpiresAt) ||
			!event.Lease.ExpiresAt.After(previous.ExpiresAt) {
			return fmt.Errorf("lease renewal does not extend exact active lease")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, event.Roles); err != nil {
			return err
		}
		record.ActiveLease = clonePointer(*event.Lease)
		record.LastLease = clonePointer(*event.Lease)
		record.UpdatedAt = event.OccurredAt
		state.repeatabilityBatches[event.BatchID] = record
	case repeatabilityBatchCaseCompleted:
		if event.Request != nil || event.CaseResult == nil || event.Result != nil || event.Lease == nil || event.AttemptFailure != nil || event.BatchID == "" {
			return fmt.Errorf("case_completed event shape is invalid")
		}
		if err := event.CaseResult.validate(); err != nil {
			return err
		}
		record, exists := state.repeatabilityBatches[event.BatchID]
		if !exists || record.Status != RepeatabilityBatchRunning || record.Result != nil {
			return fmt.Errorf("batch checkpoint references missing or terminal intent")
		}
		if event.Actor != record.UpdatedBy {
			return fmt.Errorf("batch checkpoint actor differs from intent actor")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, event.Roles); err != nil {
			return err
		}
		if err := validateRepeatabilityBatchLease(record, *event.Lease, event.OccurredAt); err != nil {
			return err
		}
		if err := validateRepeatabilityBatchCaseCheckpoint(record, *event.CaseResult); err != nil {
			return err
		}
		record.CompletedCases = append(record.CompletedCases, cloneValue(*event.CaseResult))
		record.CompletedCases = sortedRepeatabilityBatchResults(record.CompletedCases)
		record.UpdatedAt = event.OccurredAt
		state.repeatabilityBatches[event.BatchID] = record
	case repeatabilityBatchTerminalRecorded:
		if event.Result == nil || event.Request != nil || event.CaseResult != nil || event.Lease == nil || event.AttemptFailure != nil || event.BatchID != "" {
			return fmt.Errorf("terminal event shape is invalid")
		}
		if err := event.Result.Validate(); err != nil {
			return err
		}
		if !event.Result.CompletedAt.Equal(event.OccurredAt) || event.Result.CompletedBy != event.Actor {
			return fmt.Errorf("batch result completion does not match event")
		}
		record, exists := state.repeatabilityBatches[event.Result.BatchID]
		if !exists || record.Status != RepeatabilityBatchRunning || record.Result != nil {
			return fmt.Errorf("batch terminal references missing or terminal intent")
		}
		if event.Actor != record.UpdatedBy {
			return fmt.Errorf("batch terminal actor differs from intent actor")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeEvaluationRuns(state, []EvaluationRun{baseline}, event.Roles); err != nil {
			return err
		}
		if err := validateRepeatabilityBatchLease(record, *event.Lease, event.OccurredAt); err != nil {
			return err
		}
		if !slices.Equal(event.Result.ReplayEvaluationRunIDs, record.Request.ReplayEvaluationRunIDs) ||
			event.Result.RepeatabilityRunID != record.Request.RepeatabilityRunID ||
			len(event.Result.Cases) != len(record.Request.Cases)*len(record.Request.ReplayEvaluationRunIDs) ||
			!reflect.DeepEqual(event.Result.Cases, record.CompletedCases) {
			return fmt.Errorf("batch terminal does not bind request and checkpoints")
		}
		for _, id := range event.Result.ReplayEvaluationRunIDs {
			if _, exists := state.evaluationRuns[id]; !exists {
				return fmt.Errorf("batch terminal references missing replay EvaluationRun")
			}
		}
		if _, exists := state.repeatabilityRuns[event.Result.RepeatabilityRunID]; !exists {
			return fmt.Errorf("batch terminal references missing RepeatabilityRun")
		}
		record.Status = RepeatabilityBatchSucceeded
		record.Result = clonePointer(*event.Result)
		record.ActiveLease = nil
		record.UpdatedAt = event.OccurredAt
		state.repeatabilityBatches[event.Result.BatchID] = record
	default:
		return fmt.Errorf("unsupported repeatability batch event type %q", event.Type)
	}
	return nil
}

// RepeatabilityBatchRunner orchestrates Argus evaluation samples. It does not
// own a general worker runtime; remote execution remains behind ReplayCaseExecutor.
type RepeatabilityBatchRunner struct {
	repository    *Repository
	runs          EvaluationRunReader
	executor      ReplayCaseExecutor
	now           func() time.Time
	workerID      string
	leaseDuration time.Duration
	clockMu       sync.Mutex
}

type repeatabilityBatchLeaseGuard struct {
	mu    sync.RWMutex
	lease RepeatabilityBatchLease
	err   error
}

func NewRepeatabilityBatchRunner(
	repository *Repository,
	runs EvaluationRunReader,
	executor ReplayCaseExecutor,
	now func() time.Time,
	workerID string,
	leaseDuration time.Duration,
) (*RepeatabilityBatchRunner, error) {
	if repository == nil || runs == nil || executor == nil || now == nil ||
		validateID("worker_id", workerID) != nil || leaseDuration < time.Second || leaseDuration > time.Hour {
		return nil, fmt.Errorf("repeatability batch dependencies are required")
	}
	return &RepeatabilityBatchRunner{
		repository: repository, runs: runs, executor: executor, now: now,
		workerID: workerID, leaseDuration: leaseDuration,
	}, nil
}

func (runner *RepeatabilityBatchRunner) nowUTC() time.Time {
	runner.clockMu.Lock()
	defer runner.clockMu.Unlock()
	return runner.now().UTC()
}

func (guard *repeatabilityBatchLeaseGuard) current() (RepeatabilityBatchLease, error) {
	guard.mu.RLock()
	defer guard.mu.RUnlock()
	return cloneValue(guard.lease), guard.err
}

func (guard *repeatabilityBatchLeaseGuard) update(lease RepeatabilityBatchLease) {
	guard.mu.Lock()
	guard.lease = cloneValue(lease)
	guard.mu.Unlock()
}

func (guard *repeatabilityBatchLeaseGuard) fail(err error) {
	guard.mu.Lock()
	if guard.err == nil {
		guard.err = err
	}
	guard.mu.Unlock()
}

func (runner *RepeatabilityBatchRunner) Run(
	ctx context.Context,
	request RepeatabilityBatchRequest,
	mutation Mutation,
) (RepeatabilityBatchResult, error) {
	if err := request.Validate(); err != nil {
		return RepeatabilityBatchResult{}, err
	}
	if err := requireReplayExecutorTemplateRef(request.ExecutorTemplateRef); err != nil {
		return RepeatabilityBatchResult{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return RepeatabilityBatchResult{}, fmt.Errorf("batch created_at must equal intent mutation time")
	}
	record, err := runner.repository.BeginRepeatabilityBatch(ctx, request, mutation)
	if err != nil {
		return RepeatabilityBatchResult{}, err
	}
	if record.Result != nil {
		return cloneValue(*record.Result), nil
	}
	claimAt := runner.nowUTC()
	lease, err := runner.repository.ClaimRepeatabilityBatch(
		ctx, request.BatchID, runner.workerID, runner.leaseDuration,
		derivedRepeatabilityBatchMutation(mutation, request.BatchID, "claim", runner.workerID, claimAt),
	)
	if err != nil {
		return RepeatabilityBatchResult{}, err
	}
	workerContext, leaseGuard, stopHeartbeat := runner.startLeaseHeartbeat(ctx, request, mutation, lease)
	defer stopHeartbeat()
	access := Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	baseline, err := runner.repository.GetEvaluationRun(request.BaselineEvaluationRunID, access)
	if err != nil {
		return RepeatabilityBatchResult{}, fmt.Errorf("load baseline EvaluationRun: %w", err)
	}
	if err := validateRepeatabilityBatchAgainstBaseline(request, baseline); err != nil {
		return RepeatabilityBatchResult{}, err
	}
	for _, evaluationRunID := range request.ReplayEvaluationRunIDs {
		for _, item := range request.Cases {
			exposure := Exposure{
				SchemaVersion: ExposureSchemaVersion, EvaluationRunID: evaluationRunID,
				CaseID: item.CaseID, Observations: slices.Clone(item.ExposureObservations),
				ObservedAt: request.CreatedAt.UTC(),
			}
			if _, err := runner.repository.RecordExposure(workerContext, exposure,
				derivedRepeatabilityBatchMutation(
					mutation, request.BatchID, "exposure",
					repeatabilityBatchCaseKey(evaluationRunID, item.CaseID),
					request.CreatedAt.UTC(),
				)); err != nil {
				return RepeatabilityBatchResult{}, fmt.Errorf("record exposure for sample %q case %q: %w", evaluationRunID, item.CaseID, err)
			}
		}
	}
	caseResults, err := runner.executeAndCheckpointCases(
		workerContext, request, mutation, leaseGuard.current, record.CompletedCases,
	)
	if err != nil {
		return RepeatabilityBatchResult{}, err
	}
	for _, evaluationRunID := range request.ReplayEvaluationRunIDs {
		bindings := make([]EvaluationCaseRunBinding, 0, len(request.Cases))
		for caseIndex, item := range request.Cases {
			result, exists := findRepeatabilityBatchResult(caseResults, evaluationRunID, item.CaseID)
			if !exists {
				return RepeatabilityBatchResult{}, fmt.Errorf("missing completed sample case %q/%q", evaluationRunID, item.CaseID)
			}
			dimensionScope := make([]contractsv1alpha1.VersionedRef, 0, len(baseline.Results[caseIndex].Dimensions))
			for _, dimension := range baseline.Results[caseIndex].Dimensions {
				dimensionScope = append(dimensionScope, dimension.Dimension)
			}
			bindings = append(bindings, EvaluationCaseRunBinding{
				CaseID: item.CaseID, ExpectedLabelRevision: item.ExpectedLabelRevision,
				ReviewRunID: result.ReplayReviewRunID, DimensionScope: dimensionScope,
			})
		}
		if _, err := runner.resolveOrRecordReplayEvaluation(
			workerContext, request, mutation, access, evaluationRunID, bindings, baseline.EvaluatorRevision,
		); err != nil {
			return RepeatabilityBatchResult{}, err
		}
	}
	repeatability, err := runner.resolveOrRecordRepeatability(
		workerContext, request, mutation, access,
	)
	if err != nil {
		return RepeatabilityBatchResult{}, err
	}
	if err := stopHeartbeat(); err != nil {
		return RepeatabilityBatchResult{}, err
	}
	completedAt := runner.nowUTC()
	currentLease, err := leaseGuard.current()
	if err != nil {
		return RepeatabilityBatchResult{}, err
	}
	currentLease, err = runner.repository.RenewRepeatabilityBatchLease(
		ctx, currentLease, runner.leaseDuration,
		derivedRepeatabilityBatchMutation(
			mutation, request.BatchID, "heartbeat", fmt.Sprintf("%d", completedAt.UnixNano()), completedAt,
		),
	)
	if err != nil {
		return RepeatabilityBatchResult{}, fmt.Errorf("renew repeatability batch lease before terminal: %w", err)
	}
	result := RepeatabilityBatchResult{
		SchemaVersion: RepeatabilityBatchResultSchemaVersion, BatchID: request.BatchID,
		ReplayEvaluationRunIDs: slices.Clone(request.ReplayEvaluationRunIDs),
		RepeatabilityRunID:     repeatability.RepeatabilityRunID, Cases: caseResults,
		CompletedAt: completedAt, CompletedBy: mutation.Actor,
	}
	completed, err := runner.repository.CompleteRepeatabilityBatch(
		ctx, result, currentLease,
		derivedRepeatabilityBatchMutation(mutation, request.BatchID, "terminal", "all", completedAt),
	)
	if err != nil {
		return RepeatabilityBatchResult{}, err
	}
	return cloneValue(*completed.Result), nil
}

func (runner *RepeatabilityBatchRunner) startLeaseHeartbeat(
	ctx context.Context,
	request RepeatabilityBatchRequest,
	mutation Mutation,
	lease RepeatabilityBatchLease,
) (context.Context, *repeatabilityBatchLeaseGuard, func() error) {
	workerContext, cancel := context.WithCancel(ctx)
	guard := &repeatabilityBatchLeaseGuard{lease: cloneValue(lease)}
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
			renewed, err := runner.repository.RenewRepeatabilityBatchLease(
				workerContext, current, runner.leaseDuration,
				derivedRepeatabilityBatchMutation(
					mutation, request.BatchID, "heartbeat", fmt.Sprintf("%d", at.UnixNano()), at,
				),
			)
			if err != nil {
				guard.fail(fmt.Errorf("renew repeatability batch lease: %w", err))
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

type repeatabilityBatchJob struct {
	evaluationRunID string
	caseIndex       int
}

func (runner *RepeatabilityBatchRunner) executeAndCheckpointCases(
	ctx context.Context,
	request RepeatabilityBatchRequest,
	mutation Mutation,
	currentLease func() (RepeatabilityBatchLease, error),
	completed []RepeatabilityBatchCaseResult,
) ([]RepeatabilityBatchCaseResult, error) {
	completedByKey := make(map[string]RepeatabilityBatchCaseResult, len(completed))
	for _, result := range completed {
		if err := result.validate(); err != nil {
			return nil, fmt.Errorf("invalid durable sample checkpoint: %w", err)
		}
		completedByKey[repeatabilityBatchCaseResultKey(result)] = result
	}
	allJobs := repeatabilityBatchJobs(request)
	results := make([]RepeatabilityBatchCaseResult, len(allJobs))
	missing := make([]int, 0, len(allJobs)-len(completed))
	for index, job := range allJobs {
		key := repeatabilityBatchCaseKey(job.evaluationRunID, request.Cases[job.caseIndex].CaseID)
		if result, exists := completedByKey[key]; exists {
			results[index] = result
			continue
		}
		missing = append(missing, index)
	}
	if len(completedByKey) != len(completed) || len(completed)+len(missing) != len(allJobs) {
		return nil, fmt.Errorf("durable sample checkpoints do not match batch intent")
	}
	if len(missing) == 0 {
		return results, nil
	}
	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	workerCount := min(request.MaxConcurrency, len(missing))
	jobs := make(chan int)
	errorsByIndex := make([]error, len(missing))
	newResults := make([]RepeatabilityBatchCaseResult, len(missing))
	var wait sync.WaitGroup
	for range workerCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for position := range jobs {
				jobIndex := missing[position]
				job := allJobs[jobIndex]
				item := request.Cases[job.caseIndex]
				result, err := runner.executeCase(workerContext, request, job.evaluationRunID, item)
				if err == nil {
					lease, leaseErr := currentLease()
					if leaseErr != nil {
						err = leaseErr
					} else {
						checkpointAt := runner.nowUTC()
						_, err = runner.repository.RecordRepeatabilityBatchCase(
							workerContext, request.BatchID, result, lease,
							derivedRepeatabilityBatchMutation(
								mutation, request.BatchID, "checkpoint",
								repeatabilityBatchCaseKey(job.evaluationRunID, item.CaseID), checkpointAt,
							),
						)
					}
				}
				newResults[position], errorsByIndex[position] = result, err
				if err != nil {
					cancel()
				}
			}
		}()
	}
	scheduled := 0
schedule:
	for position := range missing {
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
		if errorsByIndex[position] != nil {
			job := allJobs[missing[position]]
			return nil, fmt.Errorf("execute exact replay for sample %q case %q: %w",
				job.evaluationRunID, request.Cases[job.caseIndex].CaseID, errorsByIndex[position])
		}
	}
	if scheduled != len(missing) {
		return nil, workerContext.Err()
	}
	for position, result := range newResults {
		results[missing[position]] = result
	}
	return results, nil
}

func (runner *RepeatabilityBatchRunner) executeCase(
	ctx context.Context,
	request RepeatabilityBatchRequest,
	evaluationRunID string,
	item RepeatabilityBatchCase,
) (RepeatabilityBatchCaseResult, error) {
	baselineRun, err := runner.runs.LoadRun(item.BaselineReviewRunID)
	if err != nil || baselineRun.Kind != runmodel.RunKindReview || baselineRun.Status != runmodel.RunStatusSucceeded {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("baseline is not a succeeded non-replay ReviewRun")
	}
	baselineRef, err := runner.runs.CommittedRunRef(item.BaselineReviewRunID)
	if err != nil {
		return RepeatabilityBatchCaseResult{}, err
	}
	baselineSnapshot, err := runner.runs.ExecutionSnapshotForRun(item.BaselineReviewRunID)
	if err != nil {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("load baseline execution snapshot: %w", err)
	}
	var baselineConfig reviewconfig.ConfigBundle
	if err := runner.runs.ReadJSONArtifact(baselineSnapshot.ConfigBundleRef, &baselineConfig); err != nil ||
		baselineConfig.SHA256 != item.ExpectedBaselineConfigSHA256 ||
		baselineSnapshot.Config.SHA256 != item.ExpectedBaselineConfigSHA256 {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("baseline config digest does not match batch intent")
	}
	replayRunID, err := runner.executor.ExecuteReplay(ctx, ReplayCaseExecutionRequest{
		BatchID: request.BatchID, CaseID: item.CaseID,
		IdempotencyKey: stableRepeatabilityBatchKey(
			request.BatchID, "replay", repeatabilityBatchCaseKey(evaluationRunID, item.CaseID),
		),
		SourceReviewRunID: item.BaselineReviewRunID, Variable: runmodel.ReplayVariableNone,
		ExecutorRevision:            request.ExecutorRevision,
		ExecutorTemplateRef:         *request.ExecutorTemplateRef,
		ExpectedVariantConfigSHA256: item.ExpectedBaselineConfigSHA256,
	})
	if err != nil {
		return RepeatabilityBatchCaseResult{}, err
	}
	replay, err := runner.runs.LoadRun(replayRunID)
	if err != nil || replay.Kind != runmodel.RunKindReplay || replay.Status != runmodel.RunStatusSucceeded ||
		replay.SourceRunID != item.BaselineReviewRunID || replay.ReplayRootRunID != item.BaselineReviewRunID ||
		replay.ReplayVariable != runmodel.ReplayVariableNone {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("executor result is not a direct exact replay")
	}
	replayRef, err := runner.runs.CommittedRunRef(replayRunID)
	if err != nil {
		return RepeatabilityBatchCaseResult{}, err
	}
	replaySnapshot, err := runner.runs.ExecutionSnapshotForRun(replayRunID)
	if err != nil || replaySnapshot.ReplaySourceRunRef == nil || replaySnapshot.ReplayChangeSetRef == nil {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("exact replay lineage evidence is missing")
	}
	if *replaySnapshot.ReplaySourceRunRef != baselineRef || len(replaySnapshot.ReplayInputRefs) != 0 ||
		!sameExactExecutionSnapshot(baselineSnapshot, replaySnapshot) {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("exact replay changed frozen execution inputs")
	}
	var replayConfig reviewconfig.ConfigBundle
	if err := runner.runs.ReadJSONArtifact(replaySnapshot.ConfigBundleRef, &replayConfig); err != nil ||
		replayConfig.SHA256 != item.ExpectedBaselineConfigSHA256 {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("exact replay config digest changed")
	}
	var change runmodel.ReplayChangeSet
	if err := runner.runs.ReadJSONArtifact(*replaySnapshot.ReplayChangeSetRef, &change); err != nil {
		return RepeatabilityBatchCaseResult{}, err
	}
	if err := change.Validate(); err != nil || change.SourceRunID != item.BaselineReviewRunID ||
		change.RootRunID != item.BaselineReviewRunID || change.ParentReplayRunID != "" ||
		change.Variable != runmodel.ReplayVariableNone ||
		change.BaselineSHA256 != item.ExpectedBaselineConfigSHA256 ||
		change.VariantSHA256 != item.ExpectedBaselineConfigSHA256 || change.RemoteWrites != "deny" {
		return RepeatabilityBatchCaseResult{}, fmt.Errorf("exact replay change set does not match batch intent")
	}
	return RepeatabilityBatchCaseResult{
		EvaluationRunID: evaluationRunID, CaseID: item.CaseID,
		BaselineReviewRunID: item.BaselineReviewRunID, ReplayReviewRunID: replayRunID,
		ReplayReviewRunRef: replayRef, ReplayChangeSetRef: *replaySnapshot.ReplayChangeSetRef,
	}, nil
}

func (runner *RepeatabilityBatchRunner) resolveOrRecordReplayEvaluation(
	ctx context.Context,
	request RepeatabilityBatchRequest,
	mutation Mutation,
	access Access,
	evaluationRunID string,
	bindings []EvaluationCaseRunBinding,
	evaluatorRevision string,
) (EvaluationRun, error) {
	if existing, err := runner.repository.GetEvaluationRun(evaluationRunID, access); err == nil {
		if existing.EvaluatorRevision != evaluatorRevision || !evaluationBindingsMatch(existing, bindings) {
			return EvaluationRun{}, fmt.Errorf("%w: existing replay EvaluationRun differs", ErrConflict)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return EvaluationRun{}, err
	}
	createdAt := runner.nowUTC()
	return runner.repository.RecordEvaluationRun(ctx, EvaluationRunRequest{
		SchemaVersion: EvaluationRunRequestSchemaVersion, EvaluationRunID: evaluationRunID,
		EvaluatorRevision: evaluatorRevision, Bindings: bindings, CreatedAt: request.CreatedAt.UTC(),
	}, derivedRepeatabilityBatchMutation(
		mutation, request.BatchID, "evaluation", evaluationRunID, createdAt,
	), runner.runs)
}

func (runner *RepeatabilityBatchRunner) resolveOrRecordRepeatability(
	ctx context.Context,
	request RepeatabilityBatchRequest,
	mutation Mutation,
	access Access,
) (RepeatabilityRun, error) {
	if existing, err := runner.repository.GetRepeatabilityRun(request.RepeatabilityRunID, access); err == nil {
		if existing.BaselineEvaluationRunID != request.BaselineEvaluationRunID ||
			!slices.Equal(existing.ReplayEvaluationRunIDs, request.ReplayEvaluationRunIDs) ||
			existing.RepeatabilityRevision != request.RepeatabilityRevision {
			return RepeatabilityRun{}, fmt.Errorf("%w: existing RepeatabilityRun differs", ErrConflict)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return RepeatabilityRun{}, err
	}
	createdAt := runner.nowUTC()
	return runner.repository.RecordRepeatabilityRun(ctx, RepeatabilityRunRequest{
		SchemaVersion:           RepeatabilityRunRequestSchemaVersion,
		RepeatabilityRunID:      request.RepeatabilityRunID,
		RepeatabilityRevision:   request.RepeatabilityRevision,
		BaselineEvaluationRunID: request.BaselineEvaluationRunID,
		ReplayEvaluationRunIDs:  slices.Clone(request.ReplayEvaluationRunIDs),
		CreatedAt:               request.CreatedAt.UTC(),
	}, derivedRepeatabilityBatchMutation(
		mutation, request.BatchID, "repeatability", "all", createdAt,
	), runner.runs)
}

func repeatabilityBatchJobs(request RepeatabilityBatchRequest) []repeatabilityBatchJob {
	jobs := make([]repeatabilityBatchJob, 0, len(request.ReplayEvaluationRunIDs)*len(request.Cases))
	for _, evaluationRunID := range request.ReplayEvaluationRunIDs {
		for caseIndex := range request.Cases {
			jobs = append(jobs, repeatabilityBatchJob{evaluationRunID: evaluationRunID, caseIndex: caseIndex})
		}
	}
	return jobs
}

func findRepeatabilityBatchResult(
	results []RepeatabilityBatchCaseResult,
	evaluationRunID string,
	caseID string,
) (RepeatabilityBatchCaseResult, bool) {
	key := repeatabilityBatchCaseKey(evaluationRunID, caseID)
	index := sort.Search(len(results), func(index int) bool {
		return repeatabilityBatchCaseResultKey(results[index]) >= key
	})
	if index == len(results) || repeatabilityBatchCaseResultKey(results[index]) != key {
		return RepeatabilityBatchCaseResult{}, false
	}
	return results[index], true
}

func derivedRepeatabilityBatchMutation(
	base Mutation,
	batchID string,
	kind string,
	unitID string,
	at time.Time,
) Mutation {
	return Mutation{
		IdempotencyKey: stableRepeatabilityBatchKey(batchID, kind, unitID),
		Actor:          base.Actor, Roles: slices.Clone(base.Roles),
		Audit: "repeatability batch " + kind, At: at.UTC(),
	}
}

func stableRepeatabilityBatchKey(batchID, kind, unitID string) string {
	sum := sha256.Sum256([]byte(batchID + "\x00" + kind + "\x00" + unitID))
	return "repeatability-batch-" + kind + "-" + hex.EncodeToString(sum[:12])
}

func repeatabilityBatchCaseKey(evaluationRunID, caseID string) string {
	return evaluationRunID + "\x00" + caseID
}

func repeatabilityBatchCaseResultKey(result RepeatabilityBatchCaseResult) string {
	return repeatabilityBatchCaseKey(result.EvaluationRunID, result.CaseID)
}

func sortedRepeatabilityBatchResults(
	results []RepeatabilityBatchCaseResult,
) []RepeatabilityBatchCaseResult {
	copyResults := slices.Clone(results)
	sort.Slice(copyResults, func(left, right int) bool {
		return repeatabilityBatchCaseResultKey(copyResults[left]) <
			repeatabilityBatchCaseResultKey(copyResults[right])
	})
	return copyResults
}
