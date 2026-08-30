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

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	FormalCorpusBatchRequestSchemaVersion = "argus.formal_corpus_batch_request.v1alpha1"
	FormalCorpusBatchResultSchemaVersion  = "argus.formal_corpus_batch_result.v1alpha1"
	FormalCorpusExecutorTemplateContract  = "argus.formal_corpus_executor_template.v1alpha1"
	formalCorpusBatchEventSchemaVersion   = "argus.formal_corpus_batch_event.v1alpha1"
	formalCorpusBatchStream               = "evaluation/formal-corpus-batches"
)

type FormalCorpusBatchStatus string

const (
	FormalCorpusBatchRunning   FormalCorpusBatchStatus = "running"
	FormalCorpusBatchSucceeded FormalCorpusBatchStatus = "succeeded"
	FormalCorpusBatchFailed    FormalCorpusBatchStatus = "failed"
)

type FormalCorpusBatchCase struct {
	CaseID                string                           `json:"case_id"`
	ExpectedLabelRevision uint64                           `json:"expected_label_revision"`
	SourceReviewRunID     string                           `json:"source_review_run_id"`
	ExposureObservations  []ExposureObservation            `json:"exposure_observations"`
	DimensionScope        []contractsv1alpha1.VersionedRef `json:"dimension_scope,omitempty"`
}

type FormalCorpusBatchRequest struct {
	SchemaVersion       string                  `json:"schema_version"`
	BatchID             string                  `json:"batch_id"`
	EvaluationRunID     string                  `json:"evaluation_run_id"`
	EvaluatorRevision   string                  `json:"evaluator_revision"`
	ExecutorRevision    string                  `json:"executor_revision"`
	CorpusSnapshotRef   runmodel.ArtifactRef    `json:"corpus_snapshot_ref"`
	ExecutorTemplateRef *runmodel.ArtifactRef   `json:"executor_template_ref,omitempty"`
	MaxConcurrency      int                     `json:"max_concurrency"`
	Cases               []FormalCorpusBatchCase `json:"cases"`
	CreatedAt           time.Time               `json:"created_at"`
}

type FormalCorpusBatchCaseResult struct {
	CaseID             string               `json:"case_id"`
	SourceReviewRunID  string               `json:"source_review_run_id"`
	FormalReviewRunID  string               `json:"formal_review_run_id"`
	FormalReviewRunRef runmodel.ArtifactRef `json:"formal_review_run_ref"`
}

type FormalCorpusBatchResult struct {
	SchemaVersion   string                        `json:"schema_version"`
	BatchID         string                        `json:"batch_id"`
	EvaluationRunID string                        `json:"evaluation_run_id"`
	Cases           []FormalCorpusBatchCaseResult `json:"cases"`
	CompletedAt     time.Time                     `json:"completed_at"`
	CompletedBy     string                        `json:"completed_by"`
}

type FormalCorpusBatchFailure struct {
	CaseID     string    `json:"case_id,omitempty"`
	Code       string    `json:"code"`
	ObservedAt time.Time `json:"observed_at"`
}

type FormalCorpusBatchRecord struct {
	Request        FormalCorpusBatchRequest      `json:"request"`
	Intent         Mutation                      `json:"intent"`
	Status         FormalCorpusBatchStatus       `json:"status"`
	CompletedCases []FormalCorpusBatchCaseResult `json:"completed_cases"`
	FinalizingAt   *time.Time                    `json:"finalizing_at,omitempty"`
	Result         *FormalCorpusBatchResult      `json:"result,omitempty"`
	Failure        *FormalCorpusBatchFailure     `json:"failure,omitempty"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

type FormalCorpusCaseExecutionRequest struct {
	BatchID             string
	CaseID              string
	SourceReviewRunID   string
	IdempotencyKey      string
	ExecutorRevision    string
	ExecutorTemplateRef runmodel.ArtifactRef
}

type FormalCorpusCaseExecutor interface {
	ExecuteFormalCorpusCase(context.Context, FormalCorpusCaseExecutionRequest) (string, error)
}

type FormalCorpusBatchRunner struct {
	repository *Repository
	runs       EvaluationRunReader
	executor   FormalCorpusCaseExecutor
	now        func() time.Time
}

func (request FormalCorpusBatchRequest) Validate() error {
	if request.SchemaVersion != FormalCorpusBatchRequestSchemaVersion {
		return fmt.Errorf("unsupported formal corpus batch request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"batch_id": request.BatchID, "evaluation_run_id": request.EvaluationRunID,
		"evaluator_revision": request.EvaluatorRevision, "executor_revision": request.ExecutorRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := validateCorpusSnapshotRef(request.CorpusSnapshotRef); err != nil {
		return err
	}
	if request.ExecutorTemplateRef != nil {
		if err := validateFormalCorpusExecutorTemplateRef(*request.ExecutorTemplateRef); err != nil {
			return err
		}
	}
	if request.MaxConcurrency < 1 || request.MaxConcurrency > 16 {
		return fmt.Errorf("max_concurrency must be between 1 and 16")
	}
	if len(request.Cases) < 1 || len(request.Cases) > 1024 {
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
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func validateCorpusSnapshotRef(ref runmodel.ArtifactRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("corpus_snapshot_ref: %w", err)
	}
	if ref.Contract != CorpusSnapshotContract {
		return fmt.Errorf("corpus_snapshot_ref contract is %q, want %q", ref.Contract, CorpusSnapshotContract)
	}
	return nil
}

func (item FormalCorpusBatchCase) validate() error {
	if err := validateID("case_id", item.CaseID); err != nil {
		return err
	}
	if item.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected_label_revision must be positive")
	}
	if err := validateID("source_review_run_id", item.SourceReviewRunID); err != nil {
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
	if err := validateEvaluationDimensionScope(item.DimensionScope); err != nil {
		return fmt.Errorf("dimension_scope: %w", err)
	}
	return nil
}

func validateFormalCorpusExecutorTemplateRef(ref runmodel.ArtifactRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("executor_template_ref: %w", err)
	}
	if ref.Contract != FormalCorpusExecutorTemplateContract {
		return fmt.Errorf("executor_template_ref contract is %q, want %q", ref.Contract, FormalCorpusExecutorTemplateContract)
	}
	return nil
}

func requireFormalCorpusExecutorTemplateRef(ref *runmodel.ArtifactRef) error {
	if ref == nil {
		return fmt.Errorf("executor_template_ref is required for durable formal corpus admission")
	}
	return validateFormalCorpusExecutorTemplateRef(*ref)
}

func (result FormalCorpusBatchCaseResult) validate() error {
	for name, value := range map[string]string{
		"case_id": result.CaseID, "source_review_run_id": result.SourceReviewRunID,
		"formal_review_run_id": result.FormalReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := result.FormalReviewRunRef.Validate(); err != nil {
		return fmt.Errorf("formal_review_run_ref: %w", err)
	}
	if result.FormalReviewRunRef.Contract != runmodel.ContractReviewRun {
		return fmt.Errorf("formal_review_run_ref must reference a ReviewRun")
	}
	return nil
}

func (result FormalCorpusBatchResult) Validate() error {
	if result.SchemaVersion != FormalCorpusBatchResultSchemaVersion {
		return fmt.Errorf("unsupported formal corpus batch result schema %q", result.SchemaVersion)
	}
	for name, value := range map[string]string{
		"batch_id": result.BatchID, "evaluation_run_id": result.EvaluationRunID,
		"completed_by": result.CompletedBy,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if len(result.Cases) == 0 {
		return fmt.Errorf("cases must be non-empty")
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
	if result.CompletedAt.IsZero() || result.CompletedAt.Location() != time.UTC {
		return fmt.Errorf("completed_at must be non-zero UTC")
	}
	return nil
}

func (failure FormalCorpusBatchFailure) validate() error {
	if failure.CaseID != "" {
		if err := validateID("case_id", failure.CaseID); err != nil {
			return err
		}
	}
	if err := validateID("code", failure.Code); err != nil {
		return err
	}
	if failure.ObservedAt.IsZero() || failure.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("failure observed_at must be non-zero UTC")
	}
	return nil
}

func DecodeFormalCorpusBatchRequest(data []byte) (FormalCorpusBatchRequest, error) {
	return decodeStrict(data, "FormalCorpusBatchRequest", func(value FormalCorpusBatchRequest) error {
		return value.Validate()
	})
}

func DecodeFormalCorpusBatchResult(data []byte) (FormalCorpusBatchResult, error) {
	return decodeStrict(data, "FormalCorpusBatchResult", func(value FormalCorpusBatchResult) error {
		return value.Validate()
	})
}

type formalCorpusBatchEventType string

const (
	formalCorpusBatchStarted    formalCorpusBatchEventType = "started"
	formalCorpusCaseCompleted   formalCorpusBatchEventType = "case_completed"
	formalCorpusBatchFinalizing formalCorpusBatchEventType = "finalizing"
	formalCorpusBatchCompleted  formalCorpusBatchEventType = "completed"
	formalCorpusBatchFailed     formalCorpusBatchEventType = "failed"
)

type formalCorpusBatchEvent struct {
	SchemaVersion string                       `json:"schema_version"`
	Type          formalCorpusBatchEventType   `json:"type"`
	BatchID       string                       `json:"batch_id"`
	Request       *FormalCorpusBatchRequest    `json:"request,omitempty"`
	CaseResult    *FormalCorpusBatchCaseResult `json:"case_result,omitempty"`
	FinalizingAt  *time.Time                   `json:"finalizing_at,omitempty"`
	Result        *FormalCorpusBatchResult     `json:"result,omitempty"`
	Failure       *FormalCorpusBatchFailure    `json:"failure,omitempty"`
	Actor         string                       `json:"actor"`
	Roles         []Role                       `json:"roles"`
	Audit         string                       `json:"audit"`
	OccurredAt    time.Time                    `json:"occurred_at"`
}

type formalCorpusProjection struct {
	byID     map[string]FormalCorpusBatchRecord
	events   map[string]local.Envelope
	sequence uint64
}

func newFormalCorpusProjection() formalCorpusProjection {
	return formalCorpusProjection{byID: map[string]FormalCorpusBatchRecord{}, events: map[string]local.Envelope{}}
}

func (repository *Repository) loadFormalCorpusBatches() (formalCorpusProjection, error) {
	state := newFormalCorpusProjection()
	envelopes, err := readOptionalStream(repository.store, formalCorpusBatchStream)
	if err != nil {
		return state, fmt.Errorf("%w: read formal corpus batch stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range envelopes {
		if envelope.Schema != formalCorpusBatchEventSchemaVersion {
			return state, corruptf("formal corpus event %q has unsupported schema", envelope.ID)
		}
		var event formalCorpusBatchEvent
		if err := decodeEventStrict(envelope.Payload, &event); err != nil {
			return state, corruptf("decode formal corpus event %q: %v", envelope.ID, err)
		}
		if event.SchemaVersion != envelope.Schema || !event.OccurredAt.Equal(envelope.Time.UTC()) {
			return state, corruptf("formal corpus event %q envelope mismatch", envelope.ID)
		}
		if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
			return state, corruptf("formal corpus event %q audit: %v", envelope.ID, err)
		}
		if _, exists := state.events[envelope.ID]; exists {
			return state, corruptf("duplicate formal corpus event id %q", envelope.ID)
		}
		if err := state.applyFormalCorpusEvent(event); err != nil {
			return state, corruptf("apply formal corpus event %q: %v", envelope.ID, err)
		}
		if event.Type == formalCorpusBatchStarted {
			record := state.byID[event.BatchID]
			record.Intent.IdempotencyKey = envelope.ID
			state.byID[event.BatchID] = record
		}
		state.events[envelope.ID] = envelope
		state.sequence = envelope.Sequence
	}
	return state, nil
}

func (state *formalCorpusProjection) applyFormalCorpusEvent(event formalCorpusBatchEvent) error {
	if err := validateID("batch_id", event.BatchID); err != nil {
		return err
	}
	record, exists := state.byID[event.BatchID]
	switch event.Type {
	case formalCorpusBatchStarted:
		if exists || event.Request == nil || event.CaseResult != nil || event.FinalizingAt != nil || event.Result != nil || event.Failure != nil {
			return fmt.Errorf("invalid started event")
		}
		if err := event.Request.Validate(); err != nil || event.Request.BatchID != event.BatchID || !event.Request.CreatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("invalid started request: %v", err)
		}
		state.byID[event.BatchID] = FormalCorpusBatchRecord{
			Request: cloneValue(*event.Request),
			Intent:  Mutation{IdempotencyKey: "projection", Actor: event.Actor, Roles: slices.Clone(event.Roles), Audit: event.Audit, At: event.OccurredAt},
			Status:  FormalCorpusBatchRunning, CompletedCases: []FormalCorpusBatchCaseResult{}, UpdatedAt: event.OccurredAt,
		}
		return nil
	case formalCorpusCaseCompleted:
		if !exists || record.Status != FormalCorpusBatchRunning || record.FinalizingAt != nil || event.CaseResult == nil || event.Request != nil || event.Result != nil || event.Failure != nil {
			return fmt.Errorf("invalid case_completed event")
		}
		if err := event.CaseResult.validate(); err != nil {
			return err
		}
		requestCase, ok := formalCorpusRequestCase(record.Request, event.CaseResult.CaseID)
		if !ok || requestCase.SourceReviewRunID != event.CaseResult.SourceReviewRunID {
			return fmt.Errorf("case result does not bind request")
		}
		if _, ok := formalCorpusCompletedCase(record.CompletedCases, event.CaseResult.CaseID); ok {
			return fmt.Errorf("duplicate completed case")
		}
		record.CompletedCases = append(record.CompletedCases, cloneValue(*event.CaseResult))
		sort.Slice(record.CompletedCases, func(i, j int) bool { return record.CompletedCases[i].CaseID < record.CompletedCases[j].CaseID })
	case formalCorpusBatchFinalizing:
		if !exists || record.Status != FormalCorpusBatchRunning || record.FinalizingAt != nil || event.FinalizingAt == nil || event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Failure != nil || len(record.CompletedCases) != len(record.Request.Cases) {
			return fmt.Errorf("invalid finalizing event")
		}
		at := event.FinalizingAt.UTC()
		if !at.Equal(event.OccurredAt) {
			return fmt.Errorf("finalizing time differs from event time")
		}
		record.FinalizingAt = &at
	case formalCorpusBatchCompleted:
		if !exists || record.Status != FormalCorpusBatchRunning || record.FinalizingAt == nil || event.Result == nil || event.Request != nil || event.CaseResult != nil || event.Failure != nil {
			return fmt.Errorf("invalid completed event")
		}
		if err := event.Result.Validate(); err != nil || event.Result.BatchID != event.BatchID || event.Result.EvaluationRunID != record.Request.EvaluationRunID || !event.Result.CompletedAt.Equal(*record.FinalizingAt) || !reflect.DeepEqual(event.Result.Cases, record.CompletedCases) {
			return fmt.Errorf("completed result does not bind projection: %v", err)
		}
		record.Status = FormalCorpusBatchSucceeded
		record.Result = clonePointer(*event.Result)
	case formalCorpusBatchFailed:
		if !exists || record.Status != FormalCorpusBatchRunning || event.Failure == nil || event.Request != nil || event.CaseResult != nil || event.FinalizingAt != nil || event.Result != nil {
			return fmt.Errorf("invalid failed event")
		}
		if err := event.Failure.validate(); err != nil || !event.Failure.ObservedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("invalid batch failure: %v", err)
		}
		record.Status = FormalCorpusBatchFailed
		record.Failure = clonePointer(*event.Failure)
	default:
		return fmt.Errorf("unsupported event type %q", event.Type)
	}
	if !event.OccurredAt.Before(record.UpdatedAt) {
		record.UpdatedAt = event.OccurredAt
	}
	state.byID[event.BatchID] = record
	return nil
}

func formalCorpusRequestCase(request FormalCorpusBatchRequest, caseID string) (FormalCorpusBatchCase, bool) {
	index, found := slices.BinarySearchFunc(request.Cases, caseID, func(item FormalCorpusBatchCase, id string) int {
		if item.CaseID < id {
			return -1
		}
		if item.CaseID > id {
			return 1
		}
		return 0
	})
	if !found {
		return FormalCorpusBatchCase{}, false
	}
	return request.Cases[index], true
}

func formalCorpusCompletedCase(items []FormalCorpusBatchCaseResult, caseID string) (FormalCorpusBatchCaseResult, bool) {
	index, found := slices.BinarySearchFunc(items, caseID, func(item FormalCorpusBatchCaseResult, id string) int {
		if item.CaseID < id {
			return -1
		}
		if item.CaseID > id {
			return 1
		}
		return 0
	})
	if !found {
		return FormalCorpusBatchCaseResult{}, false
	}
	return items[index], true
}

func (repository *Repository) appendFormalCorpusEvent(mutation Mutation, event formalCorpusBatchEvent, sequence uint64) error {
	_, err := repository.store.AppendJSONLAtSequence(formalCorpusBatchStream, sequence, local.Event{
		ID: mutation.IdempotencyKey, Schema: formalCorpusBatchEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if errors.Is(err, local.ErrEventConflict) {
		return fmt.Errorf("%w: formal corpus stream changed concurrently", ErrConflict)
	}
	return err
}

func (repository *Repository) BeginFormalCorpusBatch(ctx context.Context, request FormalCorpusBatchRequest, mutation Mutation) (FormalCorpusBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := requireFormalCorpusExecutorTemplateRef(request.ExecutorTemplateRef); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return FormalCorpusBatchRecord{}, fmt.Errorf("batch created_at must equal intent mutation time")
	}
	event := formalCorpusBatchEvent{SchemaVersion: formalCorpusBatchEventSchemaVersion, Type: formalCorpusBatchStarted, BatchID: request.BatchID, Request: clonePointer(request), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadFormalCorpusBatches()
	if err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if envelope, ok := state.events[mutation.IdempotencyKey]; ok {
		var stored formalCorpusBatchEvent
		if decodeEventStrict(envelope.Payload, &stored) != nil || !reflect.DeepEqual(stored, event) {
			return FormalCorpusBatchRecord{}, fmt.Errorf("%w: idempotency key has different input", ErrConflict)
		}
		return cloneValue(state.byID[request.BatchID]), nil
	}
	if _, ok := state.byID[request.BatchID]; ok {
		return FormalCorpusBatchRecord{}, fmt.Errorf("%w: batch_id already exists", ErrConflict)
	}
	if err := repository.appendFormalCorpusEvent(mutation, event, state.sequence); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	reloaded, err := repository.loadFormalCorpusBatches()
	if err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	record := reloaded.byID[request.BatchID]
	record.Intent = cloneValue(mutation)
	return cloneValue(record), nil
}

func (repository *Repository) RecordFormalCorpusCase(ctx context.Context, batchID string, result FormalCorpusBatchCaseResult, mutation Mutation) (FormalCorpusBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := result.validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	event := formalCorpusBatchEvent{SchemaVersion: formalCorpusBatchEventSchemaVersion, Type: formalCorpusCaseCompleted, BatchID: batchID, CaseResult: clonePointer(result), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	return repository.appendFormalCorpusMutation(event, mutation)
}

func (repository *Repository) BeginFormalCorpusFinalization(ctx context.Context, batchID string, mutation Mutation) (FormalCorpusBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	at := mutation.At.UTC()
	event := formalCorpusBatchEvent{SchemaVersion: formalCorpusBatchEventSchemaVersion, Type: formalCorpusBatchFinalizing, BatchID: batchID, FinalizingAt: &at, Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: at}
	return repository.appendFormalCorpusMutation(event, mutation)
}

func (repository *Repository) CompleteFormalCorpusBatch(ctx context.Context, result FormalCorpusBatchResult, mutation Mutation) (FormalCorpusBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := result.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	event := formalCorpusBatchEvent{SchemaVersion: formalCorpusBatchEventSchemaVersion, Type: formalCorpusBatchCompleted, BatchID: result.BatchID, Result: clonePointer(result), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	return repository.appendFormalCorpusMutation(event, mutation)
}

func (repository *Repository) FailFormalCorpusBatch(ctx context.Context, batchID string, failure FormalCorpusBatchFailure, mutation Mutation) (FormalCorpusBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := validateID("batch_id", batchID); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := failure.validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if !failure.ObservedAt.Equal(mutation.At.UTC()) {
		return FormalCorpusBatchRecord{}, fmt.Errorf("failure time must equal mutation time")
	}
	event := formalCorpusBatchEvent{SchemaVersion: formalCorpusBatchEventSchemaVersion, Type: formalCorpusBatchFailed, BatchID: batchID, Failure: clonePointer(failure), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	return repository.appendFormalCorpusMutation(event, mutation)
}

func (repository *Repository) appendFormalCorpusMutation(event formalCorpusBatchEvent, mutation Mutation) (FormalCorpusBatchRecord, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadFormalCorpusBatches()
	if err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if envelope, ok := state.events[mutation.IdempotencyKey]; ok {
		var stored formalCorpusBatchEvent
		if decodeEventStrict(envelope.Payload, &stored) != nil || !reflect.DeepEqual(stored, event) {
			return FormalCorpusBatchRecord{}, fmt.Errorf("%w: idempotency key has different input", ErrConflict)
		}
		return cloneValue(state.byID[event.BatchID]), nil
	}
	if _, ok := state.byID[event.BatchID]; !ok {
		return FormalCorpusBatchRecord{}, fmt.Errorf("%w: formal corpus batch", ErrNotFound)
	}
	probe := state
	if err := probe.applyFormalCorpusEvent(event); err != nil {
		return FormalCorpusBatchRecord{}, fmt.Errorf("%w: %v", ErrInvalidTransition, err)
	}
	if err := repository.appendFormalCorpusEvent(mutation, event, state.sequence); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	reloaded, err := repository.loadFormalCorpusBatches()
	if err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	return cloneValue(reloaded.byID[event.BatchID]), nil
}

func (repository *Repository) GetFormalCorpusBatch(batchID string, access Access) (FormalCorpusBatchRecord, error) {
	if err := validateID("batch_id", batchID); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	if err := access.Validate(); err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadFormalCorpusBatches()
	if err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	record, ok := state.byID[batchID]
	if !ok {
		return FormalCorpusBatchRecord{}, fmt.Errorf("%w: formal corpus batch", ErrNotFound)
	}
	dataset, err := repository.load()
	if err != nil {
		return FormalCorpusBatchRecord{}, err
	}
	for _, item := range record.Request.Cases {
		caseRecord, exists := dataset.cases[item.CaseID]
		if !exists || authorizeCaseRead(caseRecord.CurrentCase(), access.Roles) != nil {
			return FormalCorpusBatchRecord{}, fmt.Errorf("%w: formal corpus batch", ErrUnauthorized)
		}
	}
	return cloneValue(record), nil
}

func NewFormalCorpusBatchRunner(repository *Repository, runs EvaluationRunReader, executor FormalCorpusCaseExecutor, now func() time.Time) (*FormalCorpusBatchRunner, error) {
	if repository == nil || runs == nil || executor == nil || now == nil {
		return nil, fmt.Errorf("formal corpus batch dependencies are required")
	}
	return &FormalCorpusBatchRunner{repository: repository, runs: runs, executor: executor, now: now}, nil
}

func (runner *FormalCorpusBatchRunner) Run(ctx context.Context, request FormalCorpusBatchRequest, mutation Mutation) (FormalCorpusBatchResult, error) {
	if err := request.Validate(); err != nil {
		return FormalCorpusBatchResult{}, err
	}
	if err := requireFormalCorpusExecutorTemplateRef(request.ExecutorTemplateRef); err != nil {
		return FormalCorpusBatchResult{}, err
	}
	record, err := runner.repository.BeginFormalCorpusBatch(ctx, request, mutation)
	if err != nil {
		return FormalCorpusBatchResult{}, err
	}
	if record.Result != nil {
		return cloneValue(*record.Result), nil
	}
	if record.Failure != nil {
		return FormalCorpusBatchResult{}, fmt.Errorf("formal corpus batch failed: %s", record.Failure.Code)
	}
	if err := runner.preflight(ctx, request, mutation); err != nil {
		return FormalCorpusBatchResult{}, runner.fail(ctx, request, mutation, "", "formal_corpus_preflight_failed", err)
	}
	for _, item := range request.Cases {
		exposure := Exposure{SchemaVersion: ExposureSchemaVersion, EvaluationRunID: request.EvaluationRunID, CaseID: item.CaseID, Observations: slices.Clone(item.ExposureObservations), ObservedAt: request.CreatedAt}
		if _, err := runner.repository.RecordExposure(ctx, exposure, derivedFormalCorpusMutation(mutation, request.BatchID, "exposure", item.CaseID, request.CreatedAt)); err != nil {
			return FormalCorpusBatchResult{}, runner.fail(ctx, request, mutation, item.CaseID, "formal_corpus_exposure_failed", err)
		}
	}
	results, err := runner.executeCases(ctx, request, mutation, record.CompletedCases)
	if err != nil {
		return FormalCorpusBatchResult{}, err
	}
	record, err = runner.repository.GetFormalCorpusBatch(request.BatchID, Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)})
	if err != nil {
		return FormalCorpusBatchResult{}, err
	}
	finalizingAt := record.FinalizingAt
	if finalizingAt == nil {
		at := runner.now().UTC()
		record, err = runner.repository.BeginFormalCorpusFinalization(ctx, request.BatchID, derivedFormalCorpusMutation(mutation, request.BatchID, "finalizing", "all", at))
		if err != nil {
			return FormalCorpusBatchResult{}, err
		}
		finalizingAt = record.FinalizingAt
	}
	bindings := make([]EvaluationCaseRunBinding, len(results))
	for index, result := range results {
		bindings[index] = EvaluationCaseRunBinding{CaseID: result.CaseID, ExpectedLabelRevision: request.Cases[index].ExpectedLabelRevision, ReviewRunID: result.FormalReviewRunID, DimensionScope: slices.Clone(request.Cases[index].DimensionScope)}
	}
	evaluationMutation := derivedFormalCorpusMutation(mutation, request.BatchID, "evaluation", "all", *finalizingAt)
	if _, err := runner.repository.RecordEvaluationRun(ctx, EvaluationRunRequest{SchemaVersion: EvaluationRunRequestSchemaVersion, EvaluationRunID: request.EvaluationRunID, EvaluatorRevision: request.EvaluatorRevision, Bindings: bindings, CreatedAt: request.CreatedAt}, evaluationMutation, runner.runs); err != nil {
		return FormalCorpusBatchResult{}, err
	}
	result := FormalCorpusBatchResult{SchemaVersion: FormalCorpusBatchResultSchemaVersion, BatchID: request.BatchID, EvaluationRunID: request.EvaluationRunID, Cases: results, CompletedAt: *finalizingAt, CompletedBy: mutation.Actor}
	completed, err := runner.repository.CompleteFormalCorpusBatch(ctx, result, derivedFormalCorpusMutation(mutation, request.BatchID, "terminal", "all", *finalizingAt))
	if err != nil {
		return FormalCorpusBatchResult{}, err
	}
	return cloneValue(*completed.Result), nil
}

func (runner *FormalCorpusBatchRunner) preflight(ctx context.Context, request FormalCorpusBatchRequest, mutation Mutation) error {
	access := Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	if err := runner.runs.CheckArtifactEligibility(request.CorpusSnapshotRef, runrepo.ArtifactUseEvaluation); err != nil {
		return fmt.Errorf("corpus snapshot is not evaluation eligible: %w", err)
	}
	data, err := runner.runs.ReadArtifact(request.CorpusSnapshotRef)
	if err != nil {
		return fmt.Errorf("read corpus snapshot: %w", err)
	}
	corpus, err := DecodeCorpusSnapshot(data)
	if err != nil {
		return fmt.Errorf("decode corpus snapshot: %w", err)
	}
	if corpus.CreatedAt.After(request.CreatedAt) || len(corpus.Cases) != len(request.Cases) {
		return fmt.Errorf("formal corpus request does not bind the exact frozen corpus membership")
	}
	for _, item := range request.Cases {
		if err := ctx.Err(); err != nil {
			return err
		}
		corpusIndex, found := slices.BinarySearchFunc(corpus.Cases, item.CaseID, func(frozen CorpusSnapshotCase, id string) int {
			if frozen.CaseID < id {
				return -1
			}
			if frozen.CaseID > id {
				return 1
			}
			return 0
		})
		if !found {
			return fmt.Errorf("case %q is absent from the frozen corpus", item.CaseID)
		}
		frozen := corpus.Cases[corpusIndex]
		if frozen.LabelRevision != item.ExpectedLabelRevision || frozen.SourceReviewRunID != item.SourceReviewRunID {
			return fmt.Errorf("case %q request differs from the frozen corpus", item.CaseID)
		}
		record, err := runner.repository.GetCase(item.CaseID, access)
		if err != nil {
			return fmt.Errorf("case %q: %w", item.CaseID, err)
		}
		current := record.CurrentCase()
		caseSHA, digestErr := EvaluationCaseSHA256(current)
		if digestErr != nil {
			return fmt.Errorf("case %q current digest: %w", item.CaseID, digestErr)
		}
		if record.CurrentGovernance.Revision != frozen.GovernanceRevision ||
			record.CurrentLabelRevision != item.ExpectedLabelRevision ||
			caseSHA != frozen.CurrentCaseSHA256 ||
			current.Split != corpus.Split || frozen.Split != corpus.Split ||
			record.CurrentGovernance.ReviewState != ReviewApproved || record.CurrentGovernance.DatasetState != DatasetActive || !record.CurrentGovernance.Eligibility.Evaluation || !slices.Contains(record.CurrentGovernance.LicenseConsent.AllowedUses, UseEvaluation) {
			return fmt.Errorf("case %q is not active at the expected evaluation label", item.CaseID)
		}
		if current.Type == CaseFixValidation {
			return fmt.Errorf("case %q requires an apply trial and is unsupported by formal corpus run", item.CaseID)
		}
		if err := authorizeExposure(current, mutation.Roles); err != nil {
			return fmt.Errorf("case %q exposure: %w", item.CaseID, err)
		}
		if corpus.Split == SplitHoldout {
			history, historyErr := runner.repository.ExposureHistory(item.CaseID, access)
			if historyErr != nil {
				return fmt.Errorf("case %q exposure history: %w", item.CaseID, historyErr)
			}
			if historyErr := rejectSeenExposure(item.CaseID, history); historyErr != nil {
				return historyErr
			}
		}
		run, err := runner.runs.LoadRun(item.SourceReviewRunID)
		if err != nil {
			return fmt.Errorf("source run %q: %w", item.SourceReviewRunID, err)
		}
		if run.Status != runmodel.RunStatusSucceeded || run.TargetSnapshotRef.URI != current.InputSnapshotRef ||
			run.TargetSnapshotRef != frozen.TargetSnapshotRef {
			return fmt.Errorf("source run %q does not bind case %q input snapshot", item.SourceReviewRunID, item.CaseID)
		}
		runRef, err := runner.runs.CommittedRunRef(run.RunID)
		if err != nil || runRef != frozen.SourceReviewRunRef {
			return fmt.Errorf("source run %q committed closure differs from corpus snapshot", item.SourceReviewRunID)
		}
		snapshot, err := runner.runs.ExecutionSnapshotForRun(run.RunID)
		if err != nil {
			return fmt.Errorf("source run %q snapshot: %w", item.SourceReviewRunID, err)
		}
		if snapshot.RemoteWrites != "deny" || snapshot.ToolPolicy.RemoteWrites != "deny" {
			return fmt.Errorf("source run %q does not deny remote writes", item.SourceReviewRunID)
		}
		executionSHA, err := runmodel.DigestJSON(snapshot)
		if err != nil || executionSHA != frozen.ExecutionSnapshotSHA256 {
			return fmt.Errorf("source run %q ExecutionSnapshot differs from corpus snapshot", item.SourceReviewRunID)
		}
	}
	return nil
}

func (runner *FormalCorpusBatchRunner) executeCases(ctx context.Context, request FormalCorpusBatchRequest, mutation Mutation, completed []FormalCorpusBatchCaseResult) ([]FormalCorpusBatchCaseResult, error) {
	results := make([]FormalCorpusBatchCaseResult, len(request.Cases))
	completedByID := make(map[string]FormalCorpusBatchCaseResult, len(completed))
	for _, item := range completed {
		completedByID[item.CaseID] = item
	}
	type indexedResult struct {
		index  int
		result FormalCorpusBatchCaseResult
		err    error
	}
	jobs := make(chan int)
	outcomes := make(chan indexedResult, len(request.Cases))
	workers := request.MaxConcurrency
	if workers > len(request.Cases) {
		workers = len(request.Cases)
	}
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				item := request.Cases[index]
				if existing, ok := completedByID[item.CaseID]; ok {
					outcomes <- indexedResult{index: index, result: existing}
					continue
				}
				key := stableFormalCorpusCaseKey(request.BatchID, item.CaseID)
				runID, err := runner.executor.ExecuteFormalCorpusCase(ctx, FormalCorpusCaseExecutionRequest{BatchID: request.BatchID, CaseID: item.CaseID, SourceReviewRunID: item.SourceReviewRunID, IdempotencyKey: key, ExecutorRevision: request.ExecutorRevision, ExecutorTemplateRef: *request.ExecutorTemplateRef})
				if err != nil {
					outcomes <- indexedResult{index: index, err: err}
					continue
				}
				run, err := runner.runs.LoadRun(runID)
				if err != nil {
					outcomes <- indexedResult{index: index, err: err}
					continue
				}
				sourceRun, sourceErr := runner.runs.LoadRun(item.SourceReviewRunID)
				formalSnapshot, formalSnapshotErr := runner.runs.ExecutionSnapshotForRun(runID)
				sourceSnapshot, sourceSnapshotErr := runner.runs.ExecutionSnapshotForRun(item.SourceReviewRunID)
				sourceMatch := sourceErr == nil && formalSnapshotErr == nil && sourceSnapshotErr == nil &&
					run.Kind == runmodel.RunKindReview && run.SourceRunID == "" &&
					run.TargetSnapshotRef == sourceRun.TargetSnapshotRef &&
					formalSnapshot.TargetSnapshotRef == sourceSnapshot.TargetSnapshotRef &&
					formalSnapshot.ReviewInputRef == sourceSnapshot.ReviewInputRef
				if run.Status != runmodel.RunStatusSucceeded || !sourceMatch || run.GovernedReportRef == nil || run.VerificationLedgerRef == nil || run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil {
					outcomes <- indexedResult{index: index, err: fmt.Errorf(
						"formal run %q is not a complete succeeded result: status=%s source_match=%t report=%t verification=%t calibration=%t suppression=%t",
						runID, run.Status, sourceMatch,
						run.GovernedReportRef != nil, run.VerificationLedgerRef != nil,
						run.CalibrationLedgerRef != nil, run.SuppressionLedgerRef != nil,
					)}
					continue
				}
				ref, err := runner.runs.CommittedRunRef(runID)
				if err != nil {
					outcomes <- indexedResult{index: index, err: err}
					continue
				}
				result := FormalCorpusBatchCaseResult{CaseID: item.CaseID, SourceReviewRunID: item.SourceReviewRunID, FormalReviewRunID: runID, FormalReviewRunRef: ref}
				at := runner.now().UTC()
				if _, err := runner.repository.RecordFormalCorpusCase(ctx, request.BatchID, result, derivedFormalCorpusMutation(mutation, request.BatchID, "case", item.CaseID, at)); err != nil {
					outcomes <- indexedResult{index: index, err: err}
					continue
				}
				outcomes <- indexedResult{index: index, result: result}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range request.Cases {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { group.Wait(); close(outcomes) }()
	var first indexedResult
	hasFailure := false
	for outcome := range outcomes {
		if outcome.err != nil && !hasFailure {
			first, hasFailure = outcome, true
		}
		if outcome.err == nil {
			results[outcome.index] = outcome.result
		}
	}
	if hasFailure {
		item := request.Cases[first.index]
		return nil, runner.fail(ctx, request, mutation, item.CaseID, "formal_corpus_case_failed", first.err)
	}
	return results, nil
}

func (runner *FormalCorpusBatchRunner) fail(ctx context.Context, request FormalCorpusBatchRequest, mutation Mutation, caseID, code string, cause error) error {
	at := runner.now().UTC()
	failure := FormalCorpusBatchFailure{CaseID: caseID, Code: code, ObservedAt: at}
	_, recordErr := runner.repository.FailFormalCorpusBatch(ctx, request.BatchID, failure, derivedFormalCorpusMutation(mutation, request.BatchID, "failure", caseID+"-"+code, at))
	if recordErr != nil {
		return errors.Join(cause, fmt.Errorf("record formal corpus failure: %w", recordErr))
	}
	return cause
}

func derivedFormalCorpusMutation(intent Mutation, batchID, phase, item string, at time.Time) Mutation {
	return Mutation{IdempotencyKey: stableFormalCorpusCaseKey(batchID, phase+"\x00"+item), Actor: intent.Actor, Roles: slices.Clone(intent.Roles), Audit: "formal corpus " + phase, At: at.UTC()}
}

func stableFormalCorpusCaseKey(batchID, item string) string {
	digest := sha256.Sum256([]byte(batchID + "\x00" + item))
	return "formal-corpus-" + hex.EncodeToString(digest[:12])
}
