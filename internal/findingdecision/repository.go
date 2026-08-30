package findingdecision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"argus.local/argus/internal/store/local"
)

const ledgerStream = "finding-decision-ledger"

var (
	ErrConflict          = errors.New("finding decision conflict")
	ErrCorrupt           = errors.New("corrupt finding decision ledger")
	ErrInvalidTransition = errors.New("invalid finding decision transition")
	ErrUnauthorized      = errors.New("unauthorized finding decision")
)

type ledgerEvent struct {
	SchemaVersion string   `json:"schema_version"`
	Request       Request  `json:"request"`
	Mutation      Mutation `json:"mutation"`
	Root          Root     `json:"root"`
	Decision      Decision `json:"decision"`
}

type Repository struct {
	store *local.Store
	mu    *sync.Mutex
}

var ledgerLocks sync.Map

func New(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	lock, _ := ledgerLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &Repository{store: store, mu: lock.(*sync.Mutex)}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Record(
	ctx context.Context,
	request Request,
	mutation Mutation,
	root Root,
) (Decision, error) {
	if ctx == nil {
		return Decision{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	request = cloneRequest(request)
	mutation = cloneMutation(mutation)
	if err := request.Validate(); err != nil {
		return Decision{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Decision{}, err
	}
	if err := root.Validate(); err != nil {
		return Decision{}, err
	}
	if request.RunID != root.RunID || request.FindingID != root.FindingID {
		return Decision{}, invalidTransitionf("request does not match source root")
	}
	if mutation.At.Before(request.OccurredAt) {
		return Decision{}, invalidTransitionf("recording time precedes occurrence time")
	}
	if err := authorize(request.Action, mutation.Roles); err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Decision{}, err
	}
	if existing, ok := state.byIdempotency[mutation.IdempotencyKey]; ok {
		if !exactJSONEqual(existing.Request, request) ||
			!exactJSONEqual(existing.Mutation, mutation) ||
			!exactJSONEqual(existing.Root, root) {
			return Decision{}, conflictf("idempotency key %q was already used", mutation.IdempotencyKey)
		}
		return cloneDecision(existing.Decision), nil
	}

	chainKey := findingKey(root.RunID, root.FindingID)
	sequence := uint32(2)
	priorID := root.InitialDecisionID
	if chain := state.chains[chainKey]; len(chain) > 0 {
		latest := chain[len(chain)-1]
		if !exactJSONEqual(latest.Root(), root) {
			return Decision{}, invalidTransitionf("source root changed for finding decision chain")
		}
		sequence = latest.Sequence + 1
		priorID = latest.DecisionID
	}
	decision := Decision{
		SchemaVersion: DecisionSchemaVersion,
		RunID:         root.RunID, FindingID: root.FindingID,
		Sequence: sequence, PriorDecisionID: priorID,
		InitialDecisionID: root.InitialDecisionID,
		SourceContract:    root.SourceContract, SourceSHA256: root.SourceSHA256,
		Action: request.Action, ReasonCode: request.ReasonCode,
		EvidenceRefs: append([]EvidenceRef(nil), request.EvidenceRefs...),
		OccurredAt:   request.OccurredAt, RecordedAt: mutation.At,
		RecordedBy: mutation.Actor, Roles: append([]Role(nil), mutation.Roles...),
		Audit: mutation.Audit, IdempotencyKey: mutation.IdempotencyKey,
	}
	decision.DecisionID, err = decisionID(decision)
	if err != nil {
		return Decision{}, err
	}
	if err := decision.Validate(); err != nil {
		return Decision{}, err
	}
	event := ledgerEvent{
		SchemaVersion: LedgerEventSchemaVersion,
		Request:       request, Mutation: mutation, Root: root, Decision: decision,
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	_, err = repository.store.AppendJSONLAtSequence(
		ledgerStream,
		uint64(len(state.events)),
		local.Event{
			ID: mutation.IdempotencyKey, Schema: LedgerEventSchemaVersion,
			Time: mutation.At, Payload: event,
		},
	)
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return Decision{}, conflictf("ledger changed while recording %q", mutation.IdempotencyKey)
		}
		return Decision{}, fmt.Errorf("append finding decision: %w", err)
	}
	return cloneDecision(decision), nil
}

func (repository *Repository) List(runID, findingID string) ([]Decision, error) {
	if err := validateID("run_id", runID); err != nil {
		return nil, err
	}
	if err := validateID("finding_id", findingID); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	chain := state.chains[findingKey(runID, findingID)]
	result := make([]Decision, len(chain))
	for index := range chain {
		result[index] = cloneDecision(chain[index])
	}
	return result, nil
}

type ledgerState struct {
	events        []ledgerEvent
	byIdempotency map[string]ledgerEvent
	byDecisionID  map[string]Decision
	chains        map[string][]Decision
}

func (repository *Repository) load() (ledgerState, error) {
	state := ledgerState{
		events:        make([]ledgerEvent, 0),
		byIdempotency: make(map[string]ledgerEvent),
		byDecisionID:  make(map[string]Decision),
		chains:        make(map[string][]Decision),
	}
	envelopes, err := repository.store.ReadJSONL(ledgerStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return ledgerState{}, corruptf("read ledger: %v", err)
	}
	for _, envelope := range envelopes {
		if envelope.Schema != LedgerEventSchemaVersion {
			return ledgerState{}, corruptf("sequence %d has unsupported schema %q", envelope.Sequence, envelope.Schema)
		}
		var event ledgerEvent
		if err := decodeStrictJSON(envelope.Payload, &event); err != nil {
			return ledgerState{}, corruptf("sequence %d payload: %v", envelope.Sequence, err)
		}
		if err := validateLedgerEvent(event); err != nil {
			return ledgerState{}, corruptf("sequence %d: %v", envelope.Sequence, err)
		}
		if envelope.ID != event.Mutation.IdempotencyKey || !envelope.Time.Equal(event.Mutation.At) {
			return ledgerState{}, corruptf("sequence %d envelope does not match payload", envelope.Sequence)
		}
		if _, exists := state.byIdempotency[event.Mutation.IdempotencyKey]; exists {
			return ledgerState{}, corruptf("duplicate idempotency key %q", event.Mutation.IdempotencyKey)
		}
		if _, exists := state.byDecisionID[event.Decision.DecisionID]; exists {
			return ledgerState{}, corruptf("duplicate decision id %q", event.Decision.DecisionID)
		}
		key := findingKey(event.Root.RunID, event.Root.FindingID)
		chain := state.chains[key]
		if len(chain) == 0 {
			if event.Decision.Sequence != 2 || event.Decision.PriorDecisionID != event.Root.InitialDecisionID {
				return ledgerState{}, corruptf("decision chain does not start from its initial decision")
			}
		} else {
			latest := chain[len(chain)-1]
			if !exactJSONEqual(latest.Root(), event.Root) ||
				event.Decision.Sequence != latest.Sequence+1 ||
				event.Decision.PriorDecisionID != latest.DecisionID {
				return ledgerState{}, corruptf("decision chain is not contiguous or changed root")
			}
		}
		cloned := cloneLedgerEvent(event)
		state.events = append(state.events, cloned)
		state.byIdempotency[event.Mutation.IdempotencyKey] = cloned
		state.byDecisionID[event.Decision.DecisionID] = cloneDecision(event.Decision)
		state.chains[key] = append(chain, cloneDecision(event.Decision))
	}
	return state, nil
}

func validateLedgerEvent(event ledgerEvent) error {
	if event.SchemaVersion != LedgerEventSchemaVersion {
		return fmt.Errorf("unsupported ledger event schema %q", event.SchemaVersion)
	}
	if err := event.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := event.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if err := event.Root.Validate(); err != nil {
		return fmt.Errorf("root: %w", err)
	}
	if err := event.Decision.Validate(); err != nil {
		return fmt.Errorf("decision: %w", err)
	}
	if event.Request.RunID != event.Root.RunID || event.Request.FindingID != event.Root.FindingID ||
		event.Mutation.At.Before(event.Request.OccurredAt) {
		return fmt.Errorf("request or mutation does not match source root")
	}
	if !exactJSONEqual(event.Decision.Root(), event.Root) ||
		event.Decision.Action != event.Request.Action ||
		event.Decision.ReasonCode != event.Request.ReasonCode ||
		!exactJSONEqual(event.Decision.EvidenceRefs, event.Request.EvidenceRefs) ||
		!event.Decision.OccurredAt.Equal(event.Request.OccurredAt) ||
		!event.Decision.RecordedAt.Equal(event.Mutation.At) ||
		!exactJSONEqual(event.Decision.RecordedBy, event.Mutation.Actor) ||
		!exactJSONEqual(event.Decision.Roles, event.Mutation.Roles) ||
		event.Decision.Audit != event.Mutation.Audit ||
		event.Decision.IdempotencyKey != event.Mutation.IdempotencyKey {
		return fmt.Errorf("decision does not match request, mutation, and root")
	}
	return nil
}

func findingKey(runID, findingID string) string { return runID + "\x00" + findingID }

func cloneRequest(request Request) Request {
	request.EvidenceRefs = append([]EvidenceRef(nil), request.EvidenceRefs...)
	return request
}

func cloneMutation(mutation Mutation) Mutation {
	mutation.Roles = append([]Role(nil), mutation.Roles...)
	return mutation
}

func cloneDecision(decision Decision) Decision {
	decision.EvidenceRefs = append([]EvidenceRef(nil), decision.EvidenceRefs...)
	decision.Roles = append([]Role(nil), decision.Roles...)
	return decision
}

func cloneLedgerEvent(event ledgerEvent) ledgerEvent {
	event.Request = cloneRequest(event.Request)
	event.Mutation = cloneMutation(event.Mutation)
	event.Decision = cloneDecision(event.Decision)
	return event
}

func exactJSONEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func conflictf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, arguments...))
}

func corruptf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, arguments...))
}

func invalidTransitionf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, arguments...))
}
