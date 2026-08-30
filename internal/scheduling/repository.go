package scheduling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

type Repository struct {
	store        *local.Store
	policy       Policy
	policySHA256 string
	mu           *sync.Mutex
}

var schedulingLocks sync.Map

func NewRepository(store *local.Store, policy Policy) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	digest, err := PolicyDigest(policy)
	if err != nil {
		return nil, fmt.Errorf("validate scheduling policy: %w", err)
	}
	value, _ := schedulingLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &Repository{
		store: store, policy: cloneValue(policy), policySHA256: digest,
		mu: value.(*sync.Mutex),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Submit(
	ctx context.Context,
	spec WorkloadSpec,
	mutation Mutation,
) (WorkloadRecord, error) {
	if err := contextError(ctx); err != nil {
		return WorkloadRecord{}, err
	}
	if err := spec.Validate(); err != nil {
		return WorkloadRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return WorkloadRecord{}, err
	}
	if !spec.SubmittedAt.Equal(mutation.At) {
		return WorkloadRecord{}, fmt.Errorf("submitted_at must equal mutation time")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return WorkloadRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		event, err := repository.decodeEnvelope(existing)
		if err != nil {
			return WorkloadRecord{}, err
		}
		if event.Type != eventSubmitted || event.Spec == nil ||
			!reflect.DeepEqual(*event.Spec, spec) ||
			!sameMutation(event, mutation) {
			return WorkloadRecord{}, idempotencyConflict(mutation.IdempotencyKey)
		}
		return state.record(spec.WorkloadID)
	}
	if _, exists := state.workloads[spec.WorkloadID]; exists {
		return WorkloadRecord{}, fmt.Errorf("%w: workload_id %q already exists",
			ErrConflict, spec.WorkloadID)
	}
	admission := state.admit(repository.policy, spec, mutation.At)
	event := schedulingEvent{
		SchemaVersion: schedulingEventSchema,
		PolicySHA256:  repository.policySHA256,
		Type:          eventSubmitted,
		Spec:          clonePointer(spec),
		Admission:     clonePointer(admission),
		Actor:         mutation.Actor,
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At,
	}
	if err := repository.append(mutation.IdempotencyKey, mutation.At, event); err != nil {
		return WorkloadRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return WorkloadRecord{}, err
	}
	return reloaded.record(spec.WorkloadID)
}

func (repository *Repository) Claim(
	ctx context.Context,
	request ClaimRequest,
) (Dispatch, error) {
	if err := contextError(ctx); err != nil {
		return Dispatch{}, err
	}
	if err := request.Validate(); err != nil {
		return Dispatch{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Dispatch{}, err
	}
	if existing, ok := state.events[request.IdempotencyKey]; ok {
		event, err := repository.decodeEnvelope(existing)
		if err != nil {
			return Dispatch{}, err
		}
		if event.Type != eventClaimed || event.Claim == nil ||
			!reflect.DeepEqual(event.Claim.Request, request) {
			return Dispatch{}, idempotencyConflict(request.IdempotencyKey)
		}
		record, err := state.record(event.Claim.Lease.WorkloadID)
		if err != nil {
			return Dispatch{}, err
		}
		return Dispatch{Spec: record.Spec, Lease: cloneValue(event.Claim.Lease)}, nil
	}
	workloadID, ok := state.selectCandidate(repository.policy, request)
	if !ok {
		return Dispatch{}, ErrNoWork
	}
	record := state.workloads[workloadID]
	generation := record.Generation + 1
	fencingToken := record.FencingToken + 1
	expiresAt := minTime(
		request.At.Add(repository.policy.LeaseDuration),
		record.Spec.ExecutionDeadline,
	)
	if !expiresAt.After(request.At) {
		return Dispatch{}, ErrNoWork
	}
	lease := DispatchLease{
		LeaseID:       fmt.Sprintf("%s-g%d", workloadID, generation),
		WorkloadID:    workloadID,
		WorkerID:      request.WorkerID,
		Attempt:       1,
		Generation:    generation,
		FencingToken:  fencingToken,
		AcquiredAt:    request.At,
		LastHeartbeat: request.At,
		ExpiresAt:     expiresAt,
	}
	event := schedulingEvent{
		SchemaVersion: schedulingEventSchema,
		PolicySHA256:  repository.policySHA256,
		Type:          eventClaimed,
		Claim: &claimFact{
			Request: cloneValue(request),
			Lease:   cloneValue(lease),
		},
		Actor:      request.WorkerID,
		Audit:      "dispatch lease claimed",
		OccurredAt: request.At,
	}
	if err := repository.append(request.IdempotencyKey, request.At, event); err != nil {
		return Dispatch{}, err
	}
	return Dispatch{Spec: cloneValue(record.Spec), Lease: lease}, nil
}

func (repository *Repository) Heartbeat(
	ctx context.Context,
	heartbeat Heartbeat,
) (WorkloadRecord, error) {
	if err := contextError(ctx); err != nil {
		return WorkloadRecord{}, err
	}
	if err := heartbeat.Validate(); err != nil {
		return WorkloadRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return WorkloadRecord{}, err
	}
	if existing, ok := state.events[heartbeat.IdempotencyKey]; ok {
		event, err := repository.decodeEnvelope(existing)
		if err != nil {
			return WorkloadRecord{}, err
		}
		if event.Type != eventHeartbeat || event.Heartbeat == nil ||
			!reflect.DeepEqual(event.Heartbeat.Heartbeat, heartbeat) {
			return WorkloadRecord{}, idempotencyConflict(heartbeat.IdempotencyKey)
		}
		return state.record(heartbeat.WorkloadID)
	}
	record, exists := state.workloads[heartbeat.WorkloadID]
	if !exists {
		return WorkloadRecord{}, fmt.Errorf("%w: workload %q",
			ErrNotFound, heartbeat.WorkloadID)
	}
	if err := validateHeartbeatBinding(record, heartbeat); err != nil {
		return WorkloadRecord{}, err
	}
	expiresAt := minTime(
		heartbeat.At.Add(repository.policy.LeaseDuration),
		record.Spec.ExecutionDeadline,
	)
	if !expiresAt.After(heartbeat.At) {
		return WorkloadRecord{}, ErrFenced
	}
	event := schedulingEvent{
		SchemaVersion: schedulingEventSchema,
		PolicySHA256:  repository.policySHA256,
		Type:          eventHeartbeat,
		Heartbeat: &HeartbeatFact{
			Heartbeat: cloneValue(heartbeat),
			ExpiresAt: expiresAt,
		},
		Actor:      heartbeat.WorkerID,
		Audit:      "dispatch lease heartbeat",
		OccurredAt: heartbeat.At,
	}
	if err := repository.append(heartbeat.IdempotencyKey, heartbeat.At, event); err != nil {
		return WorkloadRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return WorkloadRecord{}, err
	}
	return reloaded.record(heartbeat.WorkloadID)
}

// Complete durably records both accepted and fenced callbacks. A fenced
// callback returns ErrFenced after its rejection fact is appended.
func (repository *Repository) Complete(
	ctx context.Context,
	callback Callback,
) (WorkloadRecord, error) {
	if err := contextError(ctx); err != nil {
		return WorkloadRecord{}, err
	}
	if err := callback.Validate(); err != nil {
		return WorkloadRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return WorkloadRecord{}, err
	}
	if existing, ok := state.events[callback.IdempotencyKey]; ok {
		event, err := repository.decodeEnvelope(existing)
		if err != nil {
			return WorkloadRecord{}, err
		}
		if (event.Type != eventCallbackAccepted &&
			event.Type != eventCallbackRejected) ||
			event.Callback == nil ||
			!reflect.DeepEqual(event.Callback.Callback, callback) {
			return WorkloadRecord{}, idempotencyConflict(callback.IdempotencyKey)
		}
		record, err := state.record(callback.WorkloadID)
		if err != nil {
			return WorkloadRecord{}, err
		}
		if event.Type == eventCallbackRejected {
			return record, fmt.Errorf("%w: %s", ErrFenced, event.Callback.Reason)
		}
		return record, nil
	}
	record, exists := state.workloads[callback.WorkloadID]
	if !exists {
		return WorkloadRecord{}, fmt.Errorf("%w: workload %q",
			ErrNotFound, callback.WorkloadID)
	}
	reason := callbackRejectionReason(state, record, callback)
	eventType := eventCallbackAccepted
	if reason != "" {
		eventType = eventCallbackRejected
	}
	event := schedulingEvent{
		SchemaVersion: schedulingEventSchema,
		PolicySHA256:  repository.policySHA256,
		Type:          eventType,
		Callback: &CallbackFact{
			Callback: cloneValue(callback),
			Reason:   reason,
		},
		Actor:      callback.WorkerID,
		Audit:      "provider callback",
		OccurredAt: callback.OccurredAt,
	}
	if err := repository.append(
		callback.IdempotencyKey, callback.OccurredAt, event,
	); err != nil {
		return WorkloadRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return WorkloadRecord{}, err
	}
	result, err := reloaded.record(callback.WorkloadID)
	if err != nil {
		return WorkloadRecord{}, err
	}
	if reason != "" {
		return result, fmt.Errorf("%w: %s", ErrFenced, reason)
	}
	return result, nil
}

func (repository *Repository) CancelRun(
	ctx context.Context,
	runID string,
	reason string,
	mutation Mutation,
) ([]WorkloadRecord, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateID("run_id", runID); err != nil {
		return nil, err
	}
	if err := validateText("reason", reason, 1024, true); err != nil {
		return nil, err
	}
	if err := mutation.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		event, err := repository.decodeEnvelope(existing)
		if err != nil {
			return nil, err
		}
		if event.Type != eventRunCanceled || event.Cancel == nil ||
			event.Cancel.RunID != runID || event.Cancel.Reason != reason ||
			!sameMutation(event, mutation) {
			return nil, idempotencyConflict(mutation.IdempotencyKey)
		}
		return state.recordsForRun(runID), nil
	}
	if _, canceled := state.canceledRuns[runID]; canceled {
		return nil, fmt.Errorf("%w: run %q is already permanently canceled",
			ErrInvalidTransition, runID)
	}
	for _, record := range state.workloads {
		if record.Spec.RunID == runID && mutation.At.Before(record.UpdatedAt) {
			return nil, fmt.Errorf("%w: cancel time predates workload state",
				ErrInvalidTransition)
		}
	}
	event := schedulingEvent{
		SchemaVersion: schedulingEventSchema,
		PolicySHA256:  repository.policySHA256,
		Type:          eventRunCanceled,
		Cancel: &CancelFact{
			RunID: runID, Actor: mutation.Actor, Reason: reason,
			CanceledAt: mutation.At,
		},
		Actor:      mutation.Actor,
		Audit:      mutation.Audit,
		OccurredAt: mutation.At,
	}
	if err := repository.append(mutation.IdempotencyKey, mutation.At, event); err != nil {
		return nil, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return nil, err
	}
	return reloaded.recordsForRun(runID), nil
}

// Reconcile atomically records every timeout transition visible at mutation.At.
// Unknown outcomes and expired leases are fenced before being requeued.
func (repository *Repository) Reconcile(
	ctx context.Context,
	mutation Mutation,
) ([]WorkloadRecord, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := mutation.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		event, err := repository.decodeEnvelope(existing)
		if err != nil {
			return nil, err
		}
		if event.Type != eventReconciled || event.ReconcileActions == nil ||
			!sameMutation(event, mutation) {
			return nil, idempotencyConflict(mutation.IdempotencyKey)
		}
		return state.recordsForActions(event.ReconcileActions), nil
	}
	actions := state.reconcileActions(repository.policy, mutation.At)
	event := schedulingEvent{
		SchemaVersion:    schedulingEventSchema,
		PolicySHA256:     repository.policySHA256,
		Type:             eventReconciled,
		ReconcileActions: actions,
		Actor:            mutation.Actor,
		Audit:            mutation.Audit,
		OccurredAt:       mutation.At,
	}
	if err := repository.append(mutation.IdempotencyKey, mutation.At, event); err != nil {
		return nil, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return nil, err
	}
	return reloaded.recordsForActions(actions), nil
}

func (repository *Repository) Get(workloadID string) (WorkloadRecord, error) {
	if err := validateID("workload_id", workloadID); err != nil {
		return WorkloadRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return WorkloadRecord{}, err
	}
	return state.record(workloadID)
}

func (repository *Repository) List() ([]WorkloadRecord, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.workloads))
	for id := range state.workloads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	records := make([]WorkloadRecord, 0, len(ids))
	for _, id := range ids {
		records = append(records, cloneValue(state.workloads[id]))
	}
	return records, nil
}

// Timeline returns the sequence-ordered scheduling facts that affected one
// workload. Global reconcile/cancel events are narrowed to the matching action
// or run, so callers cannot use this query as an unrelated workload oracle.
func (repository *Repository) Timeline(
	workloadID string,
) ([]WorkloadTimelineEvent, error) {
	if err := validateID("workload_id", workloadID); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	record, exists := state.workloads[workloadID]
	if !exists {
		return nil, fmt.Errorf("%w: workload %q", ErrNotFound, workloadID)
	}
	envelopes := make([]local.Envelope, 0, len(state.events))
	for _, envelope := range state.events {
		envelopes = append(envelopes, envelope)
	}
	sort.Slice(envelopes, func(left, right int) bool {
		return envelopes[left].Sequence < envelopes[right].Sequence
	})
	result := make([]WorkloadTimelineEvent, 0)
	for _, envelope := range envelopes {
		event, decodeErr := repository.decodeEnvelope(envelope)
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: decode timeline event: %v", ErrCorrupt, decodeErr)
		}
		projected, matches := projectTimelineEvent(
			envelope, event, workloadID, record.Spec.RunID,
		)
		if matches {
			result = append(result, projected)
		}
	}
	return result, nil
}

func projectTimelineEvent(
	envelope local.Envelope,
	event schedulingEvent,
	workloadID string,
	runID string,
) (WorkloadTimelineEvent, bool) {
	projected := WorkloadTimelineEvent{
		SchemaVersion: TimelineSchemaVersion, Sequence: envelope.Sequence,
		EventID: envelope.ID, Type: string(event.Type), PolicySHA256: event.PolicySHA256,
		OccurredAt: event.OccurredAt, Actor: event.Actor, Audit: event.Audit,
	}
	switch event.Type {
	case eventSubmitted:
		if event.Spec == nil || event.Spec.WorkloadID != workloadID {
			return WorkloadTimelineEvent{}, false
		}
		projected.Admission = clonePointer(*event.Admission)
	case eventClaimed:
		if event.Claim == nil || event.Claim.Lease.WorkloadID != workloadID {
			return WorkloadTimelineEvent{}, false
		}
		projected.Lease = clonePointer(event.Claim.Lease)
	case eventHeartbeat:
		if event.Heartbeat == nil || event.Heartbeat.Heartbeat.WorkloadID != workloadID {
			return WorkloadTimelineEvent{}, false
		}
		projected.Heartbeat = clonePointer(cloneValue(*event.Heartbeat))
	case eventCallbackAccepted, eventCallbackRejected:
		if event.Callback == nil || event.Callback.Callback.WorkloadID != workloadID {
			return WorkloadTimelineEvent{}, false
		}
		projected.Callback = clonePointer(cloneValue(*event.Callback))
	case eventRunCanceled:
		if event.Cancel == nil || event.Cancel.RunID != runID {
			return WorkloadTimelineEvent{}, false
		}
		projected.Cancellation = clonePointer(*event.Cancel)
	case eventReconciled:
		for _, action := range event.ReconcileActions {
			if action.WorkloadID == workloadID {
				projected.Reconciled = append(projected.Reconciled, action)
			}
		}
		if len(projected.Reconciled) == 0 {
			return WorkloadTimelineEvent{}, false
		}
	default:
		return WorkloadTimelineEvent{}, false
	}
	return projected, true
}

func (repository *Repository) RunCancellation(runID string) (CancelFact, error) {
	if err := validateID("run_id", runID); err != nil {
		return CancelFact{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CancelFact{}, err
	}
	fact, exists := state.canceledRuns[runID]
	if !exists {
		return CancelFact{}, fmt.Errorf("%w: run cancellation %q", ErrNotFound, runID)
	}
	return cloneValue(fact), nil
}

type schedulingEventType string

const (
	eventSubmitted        schedulingEventType = "submitted"
	eventClaimed          schedulingEventType = "claimed"
	eventHeartbeat        schedulingEventType = "heartbeat"
	eventCallbackAccepted schedulingEventType = "callback_accepted"
	eventCallbackRejected schedulingEventType = "callback_rejected"
	eventRunCanceled      schedulingEventType = "run_canceled"
	eventReconciled       schedulingEventType = "reconciled"
)

type claimFact struct {
	Request ClaimRequest  `json:"request"`
	Lease   DispatchLease `json:"lease"`
}

type HeartbeatFact struct {
	Heartbeat Heartbeat `json:"heartbeat"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CallbackFact struct {
	Callback Callback `json:"callback"`
	Reason   string   `json:"reason,omitempty"`
}

type schedulingEvent struct {
	SchemaVersion    string              `json:"schema_version"`
	PolicySHA256     string              `json:"policy_sha256"`
	Type             schedulingEventType `json:"type"`
	Spec             *WorkloadSpec       `json:"spec,omitempty"`
	Admission        *AdmissionFact      `json:"admission,omitempty"`
	Claim            *claimFact          `json:"claim,omitempty"`
	Heartbeat        *HeartbeatFact      `json:"heartbeat,omitempty"`
	Callback         *CallbackFact       `json:"callback,omitempty"`
	Cancel           *CancelFact         `json:"cancel,omitempty"`
	ReconcileActions []ReconcileAction   `json:"reconcile_actions"`
	Actor            string              `json:"actor"`
	Audit            string              `json:"audit"`
	OccurredAt       time.Time           `json:"occurred_at"`
}

type projectionState struct {
	workloads    map[string]WorkloadRecord
	canceledRuns map[string]CancelFact
	events       map[string]local.Envelope
}

func newProjectionState() *projectionState {
	return &projectionState{
		workloads:    make(map[string]WorkloadRecord),
		canceledRuns: make(map[string]CancelFact),
		events:       make(map[string]local.Envelope),
	}
}

func (repository *Repository) load() (*projectionState, error) {
	envelopes, err := repository.store.ReadJSONL(schedulingLedger)
	if errors.Is(err, os.ErrNotExist) {
		return newProjectionState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read scheduling ledger: %v", ErrCorrupt, err)
	}
	state := newProjectionState()
	for _, envelope := range envelopes {
		if _, duplicate := state.events[envelope.ID]; duplicate {
			return nil, corruptf("duplicate event id %q", envelope.ID)
		}
		if err := repository.apply(state, envelope); err != nil {
			return nil, fmt.Errorf("%w: event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
		state.events[envelope.ID] = envelope
	}
	return state, nil
}

func (repository *Repository) apply(
	state *projectionState,
	envelope local.Envelope,
) error {
	event, err := repository.decodeEnvelope(envelope)
	if err != nil {
		return err
	}
	switch event.Type {
	case eventSubmitted:
		if event.Spec == nil || event.Admission == nil || event.Claim != nil ||
			event.Heartbeat != nil || event.Callback != nil || event.Cancel != nil ||
			event.ReconcileActions != nil {
			return fmt.Errorf("submitted event has invalid payload shape")
		}
		if err := event.Spec.Validate(); err != nil {
			return err
		}
		if err := event.Admission.Validate(); err != nil {
			return err
		}
		if !event.Spec.SubmittedAt.Equal(event.OccurredAt) ||
			!event.Admission.RecordedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("submission timestamps do not match event time")
		}
		if _, exists := state.workloads[event.Spec.WorkloadID]; exists {
			return fmt.Errorf("duplicate workload_id %q", event.Spec.WorkloadID)
		}
		expected := state.admit(repository.policy, *event.Spec, event.OccurredAt)
		if !reflect.DeepEqual(expected, *event.Admission) {
			return fmt.Errorf("admission fact does not match bounded queue projection")
		}
		record := newWorkloadRecord(
			*event.Spec, *event.Admission, event.OccurredAt,
		)
		if _, canceled := state.canceledRuns[event.Spec.RunID]; canceled {
			record.RunCanceled = true
		}
		state.workloads[event.Spec.WorkloadID] = record
	case eventClaimed:
		if event.Claim == nil || event.Spec != nil || event.Admission != nil ||
			event.Heartbeat != nil || event.Callback != nil || event.Cancel != nil ||
			event.ReconcileActions != nil {
			return fmt.Errorf("claimed event has invalid payload shape")
		}
		if err := event.Claim.Request.Validate(); err != nil {
			return err
		}
		if err := event.Claim.Lease.Validate(); err != nil {
			return err
		}
		if !event.Claim.Request.At.Equal(event.OccurredAt) ||
			event.Actor != event.Claim.Request.WorkerID {
			return fmt.Errorf("claim request does not match event audit")
		}
		selected, ok := state.selectCandidate(repository.policy, event.Claim.Request)
		if !ok || selected != event.Claim.Lease.WorkloadID {
			return fmt.Errorf("claim does not match deterministic queue selection")
		}
		record := state.workloads[selected]
		expected := expectedLease(repository.policy, record, event.Claim.Request)
		if !reflect.DeepEqual(expected, event.Claim.Lease) {
			return fmt.Errorf("claim lease does not match generation and fencing projection")
		}
		record.State = StateLeased
		record.StateReason = "lease_active"
		record.Attempt = expected.Attempt
		record.Generation = expected.Generation
		record.FencingToken = expected.FencingToken
		record.ActiveLease = clonePointer(expected)
		record.LastLease = clonePointer(expected)
		record.UnknownSince = nil
		record.UpdatedAt = event.OccurredAt
		state.workloads[selected] = record
	case eventHeartbeat:
		if event.Heartbeat == nil || event.Spec != nil || event.Admission != nil ||
			event.Claim != nil || event.Callback != nil || event.Cancel != nil ||
			event.ReconcileActions != nil {
			return fmt.Errorf("heartbeat event has invalid payload shape")
		}
		heartbeat := event.Heartbeat.Heartbeat
		if err := heartbeat.Validate(); err != nil {
			return err
		}
		if event.Actor != heartbeat.WorkerID ||
			!event.OccurredAt.Equal(heartbeat.At) {
			return fmt.Errorf("heartbeat does not match event audit")
		}
		record, exists := state.workloads[heartbeat.WorkloadID]
		if !exists {
			return fmt.Errorf("heartbeat references unknown workload")
		}
		if err := validateHeartbeatBinding(record, heartbeat); err != nil {
			return err
		}
		expectedExpiry := minTime(
			heartbeat.At.Add(repository.policy.LeaseDuration),
			record.Spec.ExecutionDeadline,
		)
		if !expectedExpiry.Equal(event.Heartbeat.ExpiresAt) ||
			!event.OccurredAt.Equal(heartbeat.At) {
			return fmt.Errorf("heartbeat expiry or time is invalid")
		}
		lease := *record.ActiveLease
		lease.LastHeartbeat = heartbeat.At
		lease.ExpiresAt = expectedExpiry
		record.ActiveLease = clonePointer(lease)
		record.LastLease = clonePointer(lease)
		record.UpdatedAt = event.OccurredAt
		state.workloads[heartbeat.WorkloadID] = record
	case eventCallbackAccepted, eventCallbackRejected:
		if event.Callback == nil || event.Spec != nil || event.Admission != nil ||
			event.Claim != nil || event.Heartbeat != nil || event.Cancel != nil ||
			event.ReconcileActions != nil {
			return fmt.Errorf("callback event has invalid payload shape")
		}
		callback := event.Callback.Callback
		if err := callback.Validate(); err != nil {
			return err
		}
		if event.Actor != callback.WorkerID ||
			!event.OccurredAt.Equal(callback.OccurredAt) {
			return fmt.Errorf("callback does not match event audit")
		}
		record, exists := state.workloads[callback.WorkloadID]
		if !exists {
			return fmt.Errorf("callback references unknown workload")
		}
		expectedReason := callbackRejectionReason(state, record, callback)
		if event.Type == eventCallbackRejected {
			if expectedReason == "" || event.Callback.Reason != expectedReason {
				return fmt.Errorf("callback rejection reason does not match current fence")
			}
			record.CallbackRejections = append(
				record.CallbackRejections,
				CallbackRejection{
					CallbackID: callback.IdempotencyKey,
					Reason:     expectedReason,
					RejectedAt: event.OccurredAt,
				},
			)
			if event.OccurredAt.After(record.UpdatedAt) {
				record.UpdatedAt = event.OccurredAt
			}
			state.workloads[callback.WorkloadID] = record
			break
		}
		if expectedReason != "" || event.Callback.Reason != "" {
			return fmt.Errorf("accepted callback violates current fence")
		}
		switch callback.Status {
		case CallbackSucceeded:
			record.State = StateSucceeded
			record.StateReason = string(CallbackSucceeded)
		case CallbackFailed:
			record.State = StateFailed
			record.StateReason = callback.FailureCode
		case CallbackUnknown:
			record.State = StateUnknown
			record.StateReason = string(CallbackUnknown)
			unknownSince := event.OccurredAt
			record.UnknownSince = &unknownSince
		}
		if callback.Status != CallbackUnknown {
			record.ActiveLease = nil
			record.UnknownSince = nil
			record.Terminal = &TerminalFact{
				CallbackID:  callback.IdempotencyKey,
				Status:      callback.Status,
				OutputRefs:  slices.Clone(callback.OutputRefs),
				FailureCode: callback.FailureCode,
				RecordedAt:  event.OccurredAt,
			}
		}
		record.UpdatedAt = event.OccurredAt
		state.workloads[callback.WorkloadID] = record
	case eventRunCanceled:
		if event.Cancel == nil || event.Spec != nil || event.Admission != nil ||
			event.Claim != nil || event.Heartbeat != nil || event.Callback != nil ||
			event.ReconcileActions != nil {
			return fmt.Errorf("run_canceled event has invalid payload shape")
		}
		if err := validateID("run_id", event.Cancel.RunID); err != nil {
			return err
		}
		if err := validateText("cancel reason", event.Cancel.Reason, 1024, true); err != nil {
			return err
		}
		if event.Cancel.Actor != event.Actor ||
			!event.Cancel.CanceledAt.Equal(event.OccurredAt) {
			return fmt.Errorf("cancel fact does not match event audit")
		}
		if _, exists := state.canceledRuns[event.Cancel.RunID]; exists {
			return fmt.Errorf("run already canceled")
		}
		for _, record := range state.workloads {
			if record.Spec.RunID == event.Cancel.RunID &&
				event.OccurredAt.Before(record.UpdatedAt) {
				return fmt.Errorf("cancel time predates workload state")
			}
		}
		state.canceledRuns[event.Cancel.RunID] = cloneValue(*event.Cancel)
		for id, record := range state.workloads {
			if record.Spec.RunID != event.Cancel.RunID {
				continue
			}
			record.RunCanceled = true
			switch record.State {
			case StatePending, StateLeased, StateUnknown:
				record.State = StateCanceled
				record.StateReason = ReasonRunCanceled
				record.ActiveLease = nil
				record.UnknownSince = nil
			}
			record.UpdatedAt = event.OccurredAt
			state.workloads[id] = record
		}
	case eventReconciled:
		if event.ReconcileActions == nil || event.Spec != nil || event.Admission != nil ||
			event.Claim != nil || event.Heartbeat != nil || event.Callback != nil ||
			event.Cancel != nil {
			return fmt.Errorf("reconciled event has invalid payload shape")
		}
		expected := state.reconcileActions(repository.policy, event.OccurredAt)
		if !reflect.DeepEqual(expected, event.ReconcileActions) {
			return fmt.Errorf("reconcile actions do not match timeout projection")
		}
		for _, action := range event.ReconcileActions {
			record := state.workloads[action.WorkloadID]
			switch action.Type {
			case ActionAdmissionExpired:
				record.State = StateRejected
				record.StateReason = ReasonAdmissionTimeout
			case ActionExecutionExpired:
				record.State = StateFailed
				record.StateReason = ReasonExecutionDeadline
				record.ActiveLease = nil
				record.UnknownSince = nil
				record.Terminal = &TerminalFact{
					CallbackID:  "reconciler",
					Status:      CallbackFailed,
					OutputRefs:  []string{},
					FailureCode: ReasonExecutionDeadline,
					RecordedAt:  event.OccurredAt,
				}
			case ActionLeaseExpired, ActionUnknownExpired:
				record.State = StatePending
				record.StateReason = string(action.Type)
				record.ActiveLease = nil
				record.UnknownSince = nil
			default:
				return fmt.Errorf("unsupported reconcile action %q", action.Type)
			}
			record.UpdatedAt = event.OccurredAt
			state.workloads[action.WorkloadID] = record
		}
	default:
		return fmt.Errorf("unsupported scheduling event type %q", event.Type)
	}
	return nil
}

func (repository *Repository) decodeEnvelope(
	envelope local.Envelope,
) (schedulingEvent, error) {
	if envelope.Schema != schedulingEventSchema {
		return schedulingEvent{}, fmt.Errorf("unsupported event schema %q", envelope.Schema)
	}
	var event schedulingEvent
	if err := decodeStrictJSON(envelope.Payload, &event); err != nil {
		return schedulingEvent{}, err
	}
	if event.SchemaVersion != schedulingEventSchema ||
		event.PolicySHA256 != repository.policySHA256 {
		return schedulingEvent{}, fmt.Errorf("event schema or scheduling policy digest mismatch")
	}
	if err := validateText("actor", event.Actor, 256, false); err != nil {
		return schedulingEvent{}, err
	}
	if err := validateText("audit", event.Audit, 4096, true); err != nil {
		return schedulingEvent{}, err
	}
	if !isUTC(event.OccurredAt) || !event.OccurredAt.Equal(envelope.Time) {
		return schedulingEvent{}, fmt.Errorf("event time does not match envelope")
	}
	return event, nil
}

func (repository *Repository) append(
	idempotencyKey string,
	at time.Time,
	event schedulingEvent,
) error {
	_, err := repository.store.AppendJSONL(schedulingLedger, local.Event{
		ID: idempotencyKey, Schema: schedulingEventSchema, Time: at, Payload: event,
	})
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return idempotencyConflict(idempotencyKey)
		}
		return fmt.Errorf("append scheduling event: %w", err)
	}
	return nil
}

func (state *projectionState) admit(
	policy Policy,
	spec WorkloadSpec,
	at time.Time,
) AdmissionFact {
	globalQueue, classQueue, tenantQueue := state.queueDepths(spec)
	deadline := minTime(at.Add(policy.AdmissionTimeout), spec.ExecutionDeadline)
	decision := AdmissionQueued
	reason := ReasonCapacityBusy
	if _, canceled := state.canceledRuns[spec.RunID]; canceled {
		decision, reason = AdmissionRejected, ReasonRunCanceled
	} else {
		quota := policy.tenantQuota(spec.TenantID)
		classPolicy := policy.classPolicy(spec.Class)
		switch {
		case tenantQueue >= quota.MaxQueued:
			decision, reason = AdmissionThrottled, ReasonTenantQueueFull
		case globalQueue >= policy.GlobalQueueLimit:
			decision, reason = AdmissionRejected, ReasonGlobalQueueFull
		case classQueue >= classPolicy.QueueLimit:
			decision, reason = AdmissionRejected, ReasonClassQueueFull
		default:
			globalActive, classActive, tenantActive := state.activeDepths(spec)
			if globalActive+globalQueue < policy.GlobalActiveLimit &&
				classActive+classQueue < classPolicy.PoolLimit &&
				tenantActive+tenantQueue < quota.MaxActive {
				decision, reason = AdmissionAdmitted, ReasonCapacityAvailable
			}
		}
	}
	return AdmissionFact{
		Decision: decision, Reason: reason,
		GlobalQueueDepth: globalQueue, ClassQueueDepth: classQueue,
		TenantQueueDepth:  tenantQueue,
		AdmissionDeadline: deadline, RecordedAt: at,
	}
}

func newWorkloadRecord(
	spec WorkloadSpec,
	admission AdmissionFact,
	at time.Time,
) WorkloadRecord {
	state := StatePending
	switch admission.Decision {
	case AdmissionRejected:
		state = StateRejected
	case AdmissionThrottled:
		state = StateThrottled
	}
	return WorkloadRecord{
		Spec: cloneValue(spec), Admission: cloneValue(admission), State: state,
		StateReason:        admission.Reason,
		CallbackRejections: []CallbackRejection{}, UpdatedAt: at,
	}
}

func (state *projectionState) queueDepths(
	spec WorkloadSpec,
) (global int, class int, tenant int) {
	for _, record := range state.workloads {
		if record.State != StatePending {
			continue
		}
		global++
		if record.Spec.Class == spec.Class {
			class++
		}
		if record.Spec.TenantID == spec.TenantID {
			tenant++
		}
	}
	return
}

func (state *projectionState) activeDepths(
	spec WorkloadSpec,
) (global int, class int, tenant int) {
	for _, record := range state.workloads {
		if record.State != StateLeased && record.State != StateUnknown {
			continue
		}
		global++
		if record.Spec.Class == spec.Class {
			class++
		}
		if record.Spec.TenantID == spec.TenantID {
			tenant++
		}
	}
	return
}

func (state *projectionState) selectCandidate(
	policy Policy,
	request ClaimRequest,
) (string, bool) {
	globalActive := 0
	classActive := make(map[WorkloadClass]int)
	tenantActive := make(map[string]int)
	for _, record := range state.workloads {
		if record.State != StateLeased && record.State != StateUnknown {
			continue
		}
		globalActive++
		classActive[record.Spec.Class]++
		tenantActive[record.Spec.TenantID]++
	}
	if globalActive >= policy.GlobalActiveLimit {
		return "", false
	}
	type candidate struct {
		id           string
		score        int64
		tenantActive int
		submittedAt  time.Time
	}
	candidates := make([]candidate, 0)
	for id, record := range state.workloads {
		if record.State != StatePending ||
			!slices.Contains(request.SupportedClasses, record.Spec.Class) ||
			!request.At.Before(record.Admission.AdmissionDeadline) ||
			!request.At.Before(record.Spec.ExecutionDeadline) ||
			request.At.Before(record.Spec.SubmittedAt) ||
			request.At.Before(record.UpdatedAt) {
			continue
		}
		classPolicy := policy.classPolicy(record.Spec.Class)
		quota := policy.tenantQuota(record.Spec.TenantID)
		if classActive[record.Spec.Class] >= classPolicy.PoolLimit ||
			tenantActive[record.Spec.TenantID] >= quota.MaxActive {
			continue
		}
		wait := request.At.Sub(record.Spec.SubmittedAt)
		age := int64(wait / policy.AgingInterval)
		score := int64(classPolicy.BasePriority + record.Spec.Priority + quota.PriorityBias)
		score += age
		candidates = append(candidates, candidate{
			id: id, score: score, tenantActive: tenantActive[record.Spec.TenantID],
			submittedAt: record.Spec.SubmittedAt,
		})
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].score != candidates[right].score {
			return candidates[left].score > candidates[right].score
		}
		if candidates[left].tenantActive != candidates[right].tenantActive {
			return candidates[left].tenantActive < candidates[right].tenantActive
		}
		if !candidates[left].submittedAt.Equal(candidates[right].submittedAt) {
			return candidates[left].submittedAt.Before(candidates[right].submittedAt)
		}
		return candidates[left].id < candidates[right].id
	})
	if request.WorkloadID != "" && candidates[0].id != request.WorkloadID {
		return "", false
	}
	return candidates[0].id, true
}

func expectedLease(
	policy Policy,
	record WorkloadRecord,
	request ClaimRequest,
) DispatchLease {
	generation := record.Generation + 1
	return DispatchLease{
		LeaseID:    fmt.Sprintf("%s-g%d", record.Spec.WorkloadID, generation),
		WorkloadID: record.Spec.WorkloadID, WorkerID: request.WorkerID,
		Attempt: 1, Generation: generation,
		FencingToken: record.FencingToken + 1,
		AcquiredAt:   request.At, LastHeartbeat: request.At,
		ExpiresAt: minTime(
			request.At.Add(policy.LeaseDuration), record.Spec.ExecutionDeadline,
		),
	}
}

func validateHeartbeatBinding(record WorkloadRecord, heartbeat Heartbeat) error {
	if record.State != StateLeased || record.ActiveLease == nil {
		return fmt.Errorf("%w: workload has no active dispatch lease", ErrFenced)
	}
	lease := record.ActiveLease
	if heartbeat.LeaseID != lease.LeaseID ||
		heartbeat.WorkerID != lease.WorkerID ||
		heartbeat.Attempt != lease.Attempt ||
		heartbeat.Generation != lease.Generation ||
		heartbeat.FencingToken != lease.FencingToken {
		return fmt.Errorf("%w: heartbeat does not match exact lease", ErrFenced)
	}
	if heartbeat.At.Before(lease.LastHeartbeat) || !heartbeat.At.Before(lease.ExpiresAt) ||
		!heartbeat.At.Before(record.Spec.ExecutionDeadline) ||
		record.RunCanceled {
		return fmt.Errorf("%w: heartbeat arrived outside active lease", ErrFenced)
	}
	return nil
}

func callbackRejectionReason(
	state *projectionState,
	record WorkloadRecord,
	callback Callback,
) string {
	if record.RunCanceled {
		return ReasonRunCanceled
	}
	lease := record.ActiveLease
	if lease == nil {
		if record.LastLease != nil &&
			(callback.Generation != record.LastLease.Generation ||
				callback.FencingToken != record.LastLease.FencingToken ||
				callback.LeaseID != record.LastLease.LeaseID) {
			return ReasonStaleGeneration
		}
		return ReasonLeaseNotActive
	}
	if callback.LeaseID != lease.LeaseID ||
		callback.WorkerID != lease.WorkerID ||
		callback.Attempt != lease.Attempt ||
		callback.Generation != lease.Generation ||
		callback.FencingToken != lease.FencingToken {
		return ReasonStaleGeneration
	}
	if callback.OccurredAt.Before(lease.LastHeartbeat) ||
		!callback.OccurredAt.Before(lease.ExpiresAt) ||
		!callback.OccurredAt.Before(record.Spec.ExecutionDeadline) {
		return ReasonLeaseExpired
	}
	if _, canceled := state.canceledRuns[record.Spec.RunID]; canceled {
		return ReasonRunCanceled
	}
	if record.State != StateLeased && record.State != StateUnknown {
		return ReasonLeaseNotActive
	}
	if record.State == StateUnknown && callback.Status == CallbackUnknown {
		return ReasonLeaseNotActive
	}
	return ""
}

func (state *projectionState) reconcileActions(
	policy Policy,
	at time.Time,
) []ReconcileAction {
	ids := make([]string, 0, len(state.workloads))
	for id := range state.workloads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	actions := make([]ReconcileAction, 0)
	for _, id := range ids {
		record := state.workloads[id]
		action := ReconcileAction{
			WorkloadID: id, Generation: record.Generation,
			FencingToken: record.FencingToken,
		}
		switch record.State {
		case StatePending:
			switch {
			case !at.Before(record.Spec.ExecutionDeadline):
				action.Type = ActionExecutionExpired
			case !at.Before(record.Admission.AdmissionDeadline):
				action.Type = ActionAdmissionExpired
			}
		case StateLeased:
			switch {
			case !at.Before(record.Spec.ExecutionDeadline):
				action.Type = ActionExecutionExpired
			case record.ActiveLease != nil && !at.Before(record.ActiveLease.ExpiresAt):
				action.Type = ActionLeaseExpired
			}
		case StateUnknown:
			switch {
			case !at.Before(record.Spec.ExecutionDeadline):
				action.Type = ActionExecutionExpired
			case record.UnknownSince != nil &&
				!at.Before(record.UnknownSince.Add(policy.UnknownTimeout)):
				action.Type = ActionUnknownExpired
			}
		}
		if action.Type != "" {
			actions = append(actions, action)
		}
	}
	return actions
}

func (state *projectionState) record(workloadID string) (WorkloadRecord, error) {
	record, exists := state.workloads[workloadID]
	if !exists {
		return WorkloadRecord{}, fmt.Errorf("%w: workload %q", ErrNotFound, workloadID)
	}
	return cloneValue(record), nil
}

func (state *projectionState) recordsForRun(runID string) []WorkloadRecord {
	records := make([]WorkloadRecord, 0)
	for _, record := range state.workloads {
		if record.Spec.RunID == runID {
			records = append(records, cloneValue(record))
		}
	}
	sort.Slice(records, func(left, right int) bool {
		return records[left].Spec.WorkloadID < records[right].Spec.WorkloadID
	})
	return records
}

func (state *projectionState) recordsForActions(
	actions []ReconcileAction,
) []WorkloadRecord {
	records := make([]WorkloadRecord, 0, len(actions))
	for _, action := range actions {
		if record, exists := state.workloads[action.WorkloadID]; exists {
			records = append(records, cloneValue(record))
		}
	}
	return records
}

func sameMutation(event schedulingEvent, mutation Mutation) bool {
	return event.Actor == mutation.Actor &&
		event.Audit == mutation.Audit &&
		event.OccurredAt.Equal(mutation.At)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}

func idempotencyConflict(key string) error {
	return fmt.Errorf("%w: idempotency key %q", ErrConflict, key)
}

func corruptf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, arguments...))
}

func decodeStrictJSON(data []byte, out any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func clonePointer[T any](value T) *T {
	cloned := cloneValue(value)
	return &cloned
}

func cloneValue[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		panic(err)
	}
	return result
}
