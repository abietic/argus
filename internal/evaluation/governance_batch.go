package evaluation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

const (
	GovernanceBatchRequestSchemaVersion = "argus.governance_batch_request.v1alpha1"
	GovernanceBatchResultSchemaVersion  = "argus.governance_batch_result.v1alpha1"
	governanceBatchEventSchemaVersion   = "argus.governance_batch_event.v1alpha1"
	governanceBatchStream               = "evaluation/governance-batches"
	maxGovernanceBatchItems             = 500
)

type GovernanceBatchOperation string

const (
	GovernanceBatchAssign     GovernanceBatchOperation = "assign"
	GovernanceBatchAnnotate   GovernanceBatchOperation = "annotate"
	GovernanceBatchAdjudicate GovernanceBatchOperation = "adjudicate"
	GovernanceBatchActivate   GovernanceBatchOperation = "activate"
	GovernanceBatchReopen     GovernanceBatchOperation = "reopen"
)

type GovernanceBatchItem struct {
	EventID      string                `json:"event_id"`
	Assignment   *CaseReviewAssignment `json:"assignment,omitempty"`
	Annotation   *CaseAnnotation       `json:"annotation,omitempty"`
	Adjudication *CaseAdjudication     `json:"adjudication,omitempty"`
	Activation   *CaseActivation       `json:"activation,omitempty"`
	Reopen       *CaseReopen           `json:"reopen,omitempty"`
}

type GovernanceBatchRequest struct {
	SchemaVersion   string                   `json:"schema_version"`
	BatchID         string                   `json:"batch_id"`
	Operation       GovernanceBatchOperation `json:"operation"`
	Items           []GovernanceBatchItem    `json:"items"`
	TerminalEventID string                   `json:"terminal_event_id"`
	SubmittedAt     time.Time                `json:"submitted_at"`
}

type GovernanceBatchStatus string

const (
	GovernanceBatchRunning   GovernanceBatchStatus = "running"
	GovernanceBatchSucceeded GovernanceBatchStatus = "succeeded"
	GovernanceBatchFailed    GovernanceBatchStatus = "failed"
)

type GovernanceBatchItemState string

const (
	GovernanceBatchItemSucceeded  GovernanceBatchItemState = "succeeded"
	GovernanceBatchItemFailed     GovernanceBatchItemState = "failed"
	GovernanceBatchItemNotStarted GovernanceBatchItemState = "not_started"
)

type GovernanceBatchFailureReason string

const (
	GovernanceBatchFailureUnauthorized      GovernanceBatchFailureReason = "unauthorized"
	GovernanceBatchFailureNotFound          GovernanceBatchFailureReason = "not_found"
	GovernanceBatchFailureConflict          GovernanceBatchFailureReason = "conflict"
	GovernanceBatchFailureInvalidTransition GovernanceBatchFailureReason = "invalid_transition"
)

type GovernanceBatchItemResult struct {
	EventID string                   `json:"event_id"`
	CaseID  string                   `json:"case_id"`
	State   GovernanceBatchItemState `json:"state"`
}

type GovernanceBatchResult struct {
	SchemaVersion string                       `json:"schema_version"`
	BatchID       string                       `json:"batch_id"`
	Status        GovernanceBatchStatus        `json:"status"`
	Items         []GovernanceBatchItemResult  `json:"items"`
	FailedItemID  string                       `json:"failed_item_id,omitempty"`
	FailureReason GovernanceBatchFailureReason `json:"failure_reason,omitempty"`
	CompletedAt   time.Time                    `json:"completed_at"`
}

type GovernanceBatchRecord struct {
	IntentEventID string                 `json:"intent_event_id"`
	Request       GovernanceBatchRequest `json:"request"`
	Status        GovernanceBatchStatus  `json:"status"`
	Result        *GovernanceBatchResult `json:"result,omitempty"`
	Actor         string                 `json:"actor"`
	Roles         []Role                 `json:"roles"`
	Audit         string                 `json:"audit"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

type governanceBatchEventType string

const (
	governanceBatchIntentRecorded   governanceBatchEventType = "intent_recorded"
	governanceBatchTerminalRecorded governanceBatchEventType = "terminal_recorded"
)

type governanceBatchEvent struct {
	SchemaVersion string                   `json:"schema_version"`
	Type          governanceBatchEventType `json:"type"`
	Request       *GovernanceBatchRequest  `json:"request,omitempty"`
	Result        *GovernanceBatchResult   `json:"result,omitempty"`
	Actor         string                   `json:"actor"`
	Roles         []Role                   `json:"roles"`
	Audit         string                   `json:"audit"`
	OccurredAt    time.Time                `json:"occurred_at"`
}

func (operation GovernanceBatchOperation) Validate() error {
	switch operation {
	case GovernanceBatchAssign, GovernanceBatchAnnotate, GovernanceBatchAdjudicate,
		GovernanceBatchActivate, GovernanceBatchReopen:
		return nil
	default:
		return fmt.Errorf("unsupported governance batch operation %q", operation)
	}
}

func (item GovernanceBatchItem) payloadCount() int {
	count := 0
	for _, present := range []bool{
		item.Assignment != nil, item.Annotation != nil, item.Adjudication != nil,
		item.Activation != nil, item.Reopen != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func (item GovernanceBatchItem) caseID() string {
	switch {
	case item.Assignment != nil:
		return item.Assignment.CaseID
	case item.Annotation != nil:
		return item.Annotation.CaseID
	case item.Adjudication != nil:
		return item.Adjudication.CaseID
	case item.Activation != nil:
		return item.Activation.CaseID
	case item.Reopen != nil:
		return item.Reopen.CaseID
	default:
		return ""
	}
}

func (item GovernanceBatchItem) occurredAt() time.Time {
	switch {
	case item.Assignment != nil:
		return item.Assignment.AssignedAt
	case item.Annotation != nil:
		return item.Annotation.ReviewedAt
	case item.Adjudication != nil:
		return item.Adjudication.AdjudicatedAt
	case item.Activation != nil:
		return item.Activation.ActivatedAt
	case item.Reopen != nil:
		return item.Reopen.ReopenedAt
	default:
		return time.Time{}
	}
}

func (item GovernanceBatchItem) Validate(operation GovernanceBatchOperation) error {
	if err := validateText("event_id", item.EventID, 256, false); err != nil {
		return err
	}
	if item.payloadCount() != 1 {
		return fmt.Errorf("item must contain exactly one governance payload")
	}
	var err error
	switch operation {
	case GovernanceBatchAssign:
		if item.Assignment == nil {
			return fmt.Errorf("assign batch item requires assignment")
		}
		err = item.Assignment.Validate()
	case GovernanceBatchAnnotate:
		if item.Annotation == nil {
			return fmt.Errorf("annotate batch item requires annotation")
		}
		err = item.Annotation.Validate()
	case GovernanceBatchAdjudicate:
		if item.Adjudication == nil {
			return fmt.Errorf("adjudicate batch item requires adjudication")
		}
		err = item.Adjudication.Validate()
	case GovernanceBatchActivate:
		if item.Activation == nil {
			return fmt.Errorf("activate batch item requires activation")
		}
		err = item.Activation.Validate()
	case GovernanceBatchReopen:
		if item.Reopen == nil {
			return fmt.Errorf("reopen batch item requires reopen")
		}
		err = item.Reopen.Validate()
	default:
		return operation.Validate()
	}
	return err
}

func (request GovernanceBatchRequest) Validate() error {
	if request.SchemaVersion != GovernanceBatchRequestSchemaVersion {
		return fmt.Errorf("unsupported governance batch request schema %q", request.SchemaVersion)
	}
	if err := validateID("batch_id", request.BatchID); err != nil {
		return err
	}
	if err := request.Operation.Validate(); err != nil {
		return err
	}
	if len(request.Items) == 0 || len(request.Items) > maxGovernanceBatchItems {
		return fmt.Errorf("items must contain between one and %d entries", maxGovernanceBatchItems)
	}
	if err := validateText("terminal_event_id", request.TerminalEventID, 256, false); err != nil {
		return err
	}
	if request.SubmittedAt.IsZero() || request.SubmittedAt.Location() != time.UTC {
		return fmt.Errorf("submitted_at must be UTC")
	}
	caseIDs := make(map[string]struct{}, len(request.Items))
	for index, item := range request.Items {
		if err := item.Validate(request.Operation); err != nil {
			return fmt.Errorf("items[%d]: %w", index, err)
		}
		if index > 0 && request.Items[index-1].EventID >= item.EventID {
			return fmt.Errorf("items must be sorted by unique event_id")
		}
		if item.EventID == request.TerminalEventID {
			return fmt.Errorf("item event_id must differ from terminal_event_id")
		}
		if !item.occurredAt().Equal(request.SubmittedAt) || item.occurredAt().Location() != time.UTC {
			return fmt.Errorf("items[%d] time must equal submitted_at UTC", index)
		}
		caseID := item.caseID()
		if _, duplicate := caseIDs[caseID]; duplicate {
			return fmt.Errorf("items must reference unique case_id values")
		}
		caseIDs[caseID] = struct{}{}
	}
	return nil
}

func (result GovernanceBatchResult) Validate(request GovernanceBatchRequest) error {
	if err := result.validateStandalone(); err != nil {
		return err
	}
	if result.BatchID != request.BatchID || len(result.Items) != len(request.Items) {
		return fmt.Errorf("governance batch result does not bind request")
	}
	if !result.CompletedAt.Equal(request.SubmittedAt) {
		return fmt.Errorf("completed_at must equal request submitted_at")
	}
	for index, item := range result.Items {
		requestItem := request.Items[index]
		if item.EventID != requestItem.EventID || item.CaseID != requestItem.caseID() {
			return fmt.Errorf("result item %d does not bind request", index)
		}
	}
	return nil
}

func (result GovernanceBatchResult) validateStandalone() error {
	if result.SchemaVersion != GovernanceBatchResultSchemaVersion {
		return fmt.Errorf("unsupported governance batch result schema %q", result.SchemaVersion)
	}
	if err := validateID("batch_id", result.BatchID); err != nil {
		return err
	}
	if len(result.Items) == 0 || len(result.Items) > maxGovernanceBatchItems {
		return fmt.Errorf("items must contain between one and %d entries", maxGovernanceBatchItems)
	}
	if result.CompletedAt.IsZero() || result.CompletedAt.Location() != time.UTC {
		return fmt.Errorf("completed_at must be UTC")
	}
	failureIndex := -1
	for index, item := range result.Items {
		if err := validateText(fmt.Sprintf("items[%d].event_id", index), item.EventID, 256, false); err != nil {
			return err
		}
		if err := validateID(fmt.Sprintf("items[%d].case_id", index), item.CaseID); err != nil {
			return err
		}
		if index > 0 && result.Items[index-1].EventID >= item.EventID {
			return fmt.Errorf("items must be sorted by unique event_id")
		}
		switch item.State {
		case GovernanceBatchItemSucceeded:
			if failureIndex >= 0 {
				return fmt.Errorf("succeeded item cannot follow failed item")
			}
		case GovernanceBatchItemFailed:
			if failureIndex >= 0 {
				return fmt.Errorf("batch result may contain only one failed item")
			}
			failureIndex = index
		case GovernanceBatchItemNotStarted:
			if failureIndex < 0 {
				return fmt.Errorf("not_started item requires an earlier failed item")
			}
		default:
			return fmt.Errorf("unsupported batch item state %q", item.State)
		}
	}
	switch result.Status {
	case GovernanceBatchSucceeded:
		if failureIndex >= 0 || result.FailedItemID != "" || result.FailureReason != "" {
			return fmt.Errorf("succeeded batch cannot contain failure fields")
		}
	case GovernanceBatchFailed:
		if failureIndex < 0 || result.FailedItemID != result.Items[failureIndex].EventID {
			return fmt.Errorf("failed batch must identify its failed item")
		}
		switch result.FailureReason {
		case GovernanceBatchFailureUnauthorized, GovernanceBatchFailureNotFound,
			GovernanceBatchFailureConflict, GovernanceBatchFailureInvalidTransition:
		default:
			return fmt.Errorf("unsupported governance batch failure reason %q", result.FailureReason)
		}
	default:
		return fmt.Errorf("terminal governance batch status must be succeeded or failed")
	}
	return nil
}

func DecodeGovernanceBatchRequest(data []byte) (GovernanceBatchRequest, error) {
	return decodeStrict(data, "GovernanceBatchRequest", func(value GovernanceBatchRequest) error {
		return value.Validate()
	})
}

func DecodeGovernanceBatchResult(data []byte) (GovernanceBatchResult, error) {
	return decodeStrict(data, "GovernanceBatchResult", func(value GovernanceBatchResult) error {
		return value.validateStandalone()
	})
}

func (repository *Repository) RunGovernanceBatch(
	ctx context.Context,
	request GovernanceBatchRequest,
	mutation Mutation,
) (GovernanceBatchRecord, error) {
	if err := checkContext(ctx); err != nil {
		return GovernanceBatchRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return GovernanceBatchRecord{}, fmt.Errorf("validate governance batch request: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return GovernanceBatchRecord{}, err
	}
	if !mutation.At.UTC().Equal(request.SubmittedAt) {
		return GovernanceBatchRecord{}, fmt.Errorf("mutation time must equal batch submitted_at")
	}
	if mutation.IdempotencyKey == request.TerminalEventID {
		return GovernanceBatchRecord{}, fmt.Errorf("intent idempotency key must differ from terminal_event_id")
	}
	for _, item := range request.Items {
		if item.EventID == mutation.IdempotencyKey {
			return GovernanceBatchRecord{}, fmt.Errorf("item event_id must differ from intent idempotency key")
		}
	}
	record, err := repository.ensureGovernanceBatchIntent(request, mutation)
	if err != nil || record.Status != GovernanceBatchRunning {
		return record, err
	}
	results := make([]GovernanceBatchItemResult, len(request.Items))
	for index, item := range request.Items {
		results[index] = GovernanceBatchItemResult{
			EventID: item.EventID, CaseID: item.caseID(), State: GovernanceBatchItemNotStarted,
		}
	}
	for index, item := range request.Items {
		itemMutation := mutation
		itemMutation.IdempotencyKey = item.EventID
		if err := repository.runGovernanceBatchItem(ctx, request.Operation, item, itemMutation); err != nil {
			if repository.eventExists(item.EventID) {
				record, getErr := repository.GetGovernanceBatch(
					request.BatchID, Access{Actor: mutation.Actor, Roles: mutation.Roles},
				)
				if getErr != nil {
					return GovernanceBatchRecord{}, errors.Join(err, getErr)
				}
				return record, err
			}
			reason, terminal := classifyGovernanceBatchFailure(err)
			if !terminal {
				record, getErr := repository.GetGovernanceBatch(
					request.BatchID, Access{Actor: mutation.Actor, Roles: mutation.Roles},
				)
				if getErr != nil {
					return GovernanceBatchRecord{}, errors.Join(err, getErr)
				}
				return record, err
			}
			results[index].State = GovernanceBatchItemFailed
			result := GovernanceBatchResult{
				SchemaVersion: GovernanceBatchResultSchemaVersion, BatchID: request.BatchID,
				Status: GovernanceBatchFailed, Items: results, FailedItemID: item.EventID,
				FailureReason: reason, CompletedAt: request.SubmittedAt,
			}
			return repository.recordGovernanceBatchTerminal(request, result, mutation)
		}
		results[index].State = GovernanceBatchItemSucceeded
	}
	result := GovernanceBatchResult{
		SchemaVersion: GovernanceBatchResultSchemaVersion, BatchID: request.BatchID,
		Status: GovernanceBatchSucceeded, Items: results, CompletedAt: request.SubmittedAt,
	}
	return repository.recordGovernanceBatchTerminal(request, result, mutation)
}

func (repository *Repository) eventExists(eventID string) bool {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return false
	}
	_, exists := state.events[eventID]
	return exists
}

func (repository *Repository) runGovernanceBatchItem(
	ctx context.Context,
	operation GovernanceBatchOperation,
	item GovernanceBatchItem,
	mutation Mutation,
) error {
	switch operation {
	case GovernanceBatchAssign:
		_, err := repository.AssignCaseReview(ctx, *item.Assignment, mutation)
		return err
	case GovernanceBatchAnnotate:
		_, err := repository.RecordCaseAnnotation(ctx, *item.Annotation, mutation)
		return err
	case GovernanceBatchAdjudicate:
		_, err := repository.AdjudicateCase(ctx, *item.Adjudication, mutation)
		return err
	case GovernanceBatchActivate:
		_, err := repository.ActivateCase(ctx, *item.Activation, mutation)
		return err
	case GovernanceBatchReopen:
		_, err := repository.ReopenCase(ctx, *item.Reopen, mutation)
		return err
	default:
		return operation.Validate()
	}
}

func classifyGovernanceBatchFailure(err error) (GovernanceBatchFailureReason, bool) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return GovernanceBatchFailureUnauthorized, true
	case errors.Is(err, ErrNotFound):
		return GovernanceBatchFailureNotFound, true
	case errors.Is(err, ErrInvalidTransition):
		return GovernanceBatchFailureInvalidTransition, true
	case errors.Is(err, ErrConflict):
		if strings.Contains(err.Error(), "stream changed concurrently") {
			return "", false
		}
		return GovernanceBatchFailureConflict, true
	default:
		return "", false
	}
}

func (repository *Repository) ensureGovernanceBatchIntent(
	request GovernanceBatchRequest,
	mutation Mutation,
) (GovernanceBatchRecord, error) {
	event := governanceBatchEvent{
		SchemaVersion: governanceBatchEventSchemaVersion, Type: governanceBatchIntentRecorded,
		Request: clonePointer(request), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles),
		Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return GovernanceBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, governanceBatchStream, governanceBatchEventSchemaVersion, event, mutation,
		); err != nil {
			return GovernanceBatchRecord{}, err
		}
		return state.governanceBatch(request.BatchID)
	}
	if _, exists := state.governanceBatches[request.BatchID]; exists {
		return GovernanceBatchRecord{}, fmt.Errorf("%w: governance batch %q already exists", ErrConflict, request.BatchID)
	}
	if _, exists := state.events[request.TerminalEventID]; exists {
		return GovernanceBatchRecord{}, fmt.Errorf("%w: terminal_event_id %q is already used", ErrConflict, request.TerminalEventID)
	}
	for _, item := range request.Items {
		if _, exists := state.events[item.EventID]; exists {
			return GovernanceBatchRecord{}, fmt.Errorf("%w: item event_id %q is already used", ErrConflict, item.EventID)
		}
	}
	if _, err := repository.appendGovernanceBatch(mutation, event, state.streamSequences[governanceBatchStream]); err != nil {
		return GovernanceBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return GovernanceBatchRecord{}, err
	}
	return reloaded.governanceBatch(request.BatchID)
}

func (repository *Repository) recordGovernanceBatchTerminal(
	request GovernanceBatchRequest,
	result GovernanceBatchResult,
	baseMutation Mutation,
) (GovernanceBatchRecord, error) {
	if err := result.Validate(request); err != nil {
		return GovernanceBatchRecord{}, err
	}
	mutation := baseMutation
	mutation.IdempotencyKey = request.TerminalEventID
	event := governanceBatchEvent{
		SchemaVersion: governanceBatchEventSchemaVersion, Type: governanceBatchTerminalRecorded,
		Result: clonePointer(result), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles),
		Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return GovernanceBatchRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, governanceBatchStream, governanceBatchEventSchemaVersion, event, mutation,
		); err != nil {
			return GovernanceBatchRecord{}, err
		}
		return state.governanceBatch(request.BatchID)
	}
	record, err := state.governanceBatch(request.BatchID)
	if err != nil {
		return GovernanceBatchRecord{}, err
	}
	if record.Status != GovernanceBatchRunning {
		return GovernanceBatchRecord{}, fmt.Errorf("%w: governance batch is already terminal", ErrInvalidTransition)
	}
	if _, err := repository.appendGovernanceBatch(mutation, event, state.streamSequences[governanceBatchStream]); err != nil {
		return GovernanceBatchRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return GovernanceBatchRecord{}, err
	}
	return reloaded.governanceBatch(request.BatchID)
}

func (repository *Repository) GetGovernanceBatch(batchID string, access Access) (GovernanceBatchRecord, error) {
	if err := validateID("batch_id", batchID); err != nil {
		return GovernanceBatchRecord{}, err
	}
	if err := access.Validate(); err != nil {
		return GovernanceBatchRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return GovernanceBatchRecord{}, err
	}
	record, err := state.governanceBatch(batchID)
	if err != nil {
		return GovernanceBatchRecord{}, err
	}
	if err := state.authorizeGovernanceBatchRead(record, access); err != nil {
		return GovernanceBatchRecord{}, err
	}
	return record, nil
}

func (repository *Repository) ListGovernanceBatches(access Access) ([]GovernanceBatchRecord, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.governanceBatches))
	for id, record := range state.governanceBatches {
		if state.authorizeGovernanceBatchRead(record, access) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]GovernanceBatchRecord, 0, len(ids))
	for _, id := range ids {
		record, err := state.governanceBatch(id)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func (state *projectionState) governanceBatch(batchID string) (GovernanceBatchRecord, error) {
	record, exists := state.governanceBatches[batchID]
	if !exists {
		return GovernanceBatchRecord{}, fmt.Errorf("%w: governance batch %q", ErrNotFound, batchID)
	}
	return cloneValue(record), nil
}

func (state *projectionState) authorizeGovernanceBatchRead(record GovernanceBatchRecord, access Access) error {
	if record.Actor == access.Actor {
		return nil
	}
	if record.Request.Operation == GovernanceBatchAnnotate && record.Actor != access.Actor &&
		hasRole(access.Roles, RoleDatasetReviewer) &&
		!hasRole(access.Roles, RoleDatasetCurator) && !hasRole(access.Roles, RoleDatasetAdjudicator) {
		return fmt.Errorf("%w: blind annotation batch belongs to another reviewer", ErrUnauthorized)
	}
	for _, item := range record.Request.Items {
		caseRecord, exists := state.cases[item.caseID()]
		if !exists {
			return fmt.Errorf("%w: governance batch case", ErrNotFound)
		}
		if err := authorizeCaseRead(caseRecord.CurrentCase(), access.Roles); err != nil {
			return err
		}
	}
	return nil
}

func (state *projectionState) applyGovernanceBatch(envelope local.Envelope) error {
	if envelope.Schema != governanceBatchEventSchemaVersion {
		return fmt.Errorf("unsupported governance batch event schema %q", envelope.Schema)
	}
	var event governanceBatchEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode governance batch event: %w", err)
	}
	if event.SchemaVersion != governanceBatchEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
		return err
	}
	switch event.Type {
	case governanceBatchIntentRecorded:
		if event.Request == nil || event.Result != nil {
			return fmt.Errorf("intent_recorded must contain only request")
		}
		if err := event.Request.Validate(); err != nil {
			return err
		}
		if !event.Request.SubmittedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("batch submitted_at does not match event time")
		}
		if event.Request.TerminalEventID == envelope.ID {
			return fmt.Errorf("terminal_event_id must differ from intent event")
		}
		for _, item := range event.Request.Items {
			if item.EventID == envelope.ID {
				return fmt.Errorf("item event_id must differ from intent event")
			}
		}
		if _, exists := state.governanceBatches[event.Request.BatchID]; exists {
			return fmt.Errorf("duplicate governance batch %q", event.Request.BatchID)
		}
		state.governanceBatches[event.Request.BatchID] = GovernanceBatchRecord{
			IntentEventID: envelope.ID, Request: cloneValue(*event.Request), Status: GovernanceBatchRunning,
			Actor: event.Actor, Roles: slices.Clone(event.Roles), Audit: event.Audit, UpdatedAt: event.OccurredAt,
		}
	case governanceBatchTerminalRecorded:
		if event.Result == nil || event.Request != nil {
			return fmt.Errorf("terminal_recorded must contain only result")
		}
		record, exists := state.governanceBatches[event.Result.BatchID]
		if !exists {
			return fmt.Errorf("terminal references unknown governance batch")
		}
		if envelope.ID != record.Request.TerminalEventID || record.Status != GovernanceBatchRunning ||
			event.Actor != record.Actor || !slices.Equal(event.Roles, record.Roles) || event.Audit != record.Audit {
			return fmt.Errorf("governance batch terminal does not bind intent")
		}
		if err := event.Result.Validate(record.Request); err != nil {
			return err
		}
		for index, itemResult := range event.Result.Items {
			stored, exists := state.events[itemResult.EventID]
			switch itemResult.State {
			case GovernanceBatchItemSucceeded:
				if !exists || stored.stream != datasetStream ||
					!governanceBatchItemMatchesStored(
						record.Request.Items[index], record.Request.Operation, stored.envelope,
						record.Actor, record.Roles, record.Audit, record.Request.SubmittedAt,
					) {
					return fmt.Errorf("succeeded governance item %d has no dataset event", index)
				}
			case GovernanceBatchItemFailed, GovernanceBatchItemNotStarted:
				if exists {
					return fmt.Errorf("non-succeeded governance item %d has an event", index)
				}
			}
		}
		record.Status = event.Result.Status
		record.Result = clonePointer(*event.Result)
		record.UpdatedAt = event.OccurredAt
		state.governanceBatches[event.Result.BatchID] = record
	default:
		return fmt.Errorf("unsupported governance batch event type %q", event.Type)
	}
	return nil
}

func governanceBatchItemMatchesStored(
	item GovernanceBatchItem,
	operation GovernanceBatchOperation,
	envelope local.Envelope,
	actor string,
	roles []Role,
	audit string,
	at time.Time,
) bool {
	if envelope.Schema != datasetEventSchemaVersion || envelope.ID != item.EventID ||
		!envelope.Time.UTC().Equal(at) {
		return false
	}
	var event datasetEvent
	if decodeEventStrict(envelope.Payload, &event) != nil || event.Actor != actor ||
		!slices.Equal(event.Roles, roles) || event.Audit != audit || !event.OccurredAt.Equal(at) {
		return false
	}
	switch operation {
	case GovernanceBatchAssign:
		return event.Type == datasetCaseReviewAssigned && event.Assignment != nil &&
			reflect.DeepEqual(*event.Assignment, *item.Assignment)
	case GovernanceBatchAnnotate:
		return event.Type == datasetCaseAnnotated && event.Annotation != nil &&
			reflect.DeepEqual(*event.Annotation, *item.Annotation)
	case GovernanceBatchAdjudicate:
		return event.Type == datasetCaseAdjudicated && event.Adjudication != nil &&
			reflect.DeepEqual(*event.Adjudication, *item.Adjudication)
	case GovernanceBatchActivate:
		return event.Type == datasetCaseActivated && event.Activation != nil &&
			reflect.DeepEqual(*event.Activation, *item.Activation)
	case GovernanceBatchReopen:
		return event.Type == datasetCaseReopened && event.Reopen != nil &&
			reflect.DeepEqual(*event.Reopen, *item.Reopen)
	default:
		return false
	}
}

func (repository *Repository) appendGovernanceBatch(
	mutation Mutation,
	event governanceBatchEvent,
	expectedSequence uint64,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONLAtSequence(governanceBatchStream, expectedSequence, local.Event{
		ID: mutation.IdempotencyKey, Schema: governanceBatchEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return local.Envelope{}, fmt.Errorf("%w: governance batch stream changed concurrently", ErrConflict)
		}
		return local.Envelope{}, fmt.Errorf("append governance batch event: %w", err)
	}
	return envelope, nil
}
