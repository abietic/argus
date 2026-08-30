package feedback

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

const ledgerStream = "feedback-outcome-ledger"

var (
	ErrConflict          = errors.New("feedback ledger conflict")
	ErrCorrupt           = errors.New("corrupt feedback ledger")
	ErrInvalidCorrection = errors.New("invalid feedback ledger correction")
)

type Repository struct {
	store *local.Store
	mu    *sync.Mutex
}

var ledgerLocks sync.Map

func NewRepository(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	repository := &Repository{store: store, mu: sharedLedgerLock(store.Root())}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func sharedLedgerLock(root string) *sync.Mutex {
	lock, _ := ledgerLocks.LoadOrStore(root, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// New opens a Feedback/Outcome repository and verifies its complete ledger.
func New(store *local.Store) (*Repository, error) {
	return NewRepository(store)
}

func (repository *Repository) AppendFeedback(
	ctx context.Context,
	fact Feedback,
) (Feedback, error) {
	if err := checkContext(ctx); err != nil {
		return Feedback{}, err
	}
	fact = cloneFeedback(fact)
	fact.OccurredAt = fact.OccurredAt.UTC()
	fact.RecordedAt = fact.RecordedAt.UTC()
	if err := fact.Validate(); err != nil {
		return Feedback{}, err
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Feedback{}, err
	}
	if existing, ok := state.byIdempotency[fact.IdempotencyKey]; ok {
		if existing.Kind != FactFeedback || existing.Feedback == nil ||
			!exactJSONEqual(*existing.Feedback, fact) {
			return Feedback{}, conflictf(
				"idempotency key %q was already used", fact.IdempotencyKey,
			)
		}
		return cloneFeedback(*existing.Feedback), nil
	}
	if existing, ok := state.feedbackByID[fact.FeedbackID]; ok {
		return Feedback{}, conflictf(
			"feedback id %q was already recorded with idempotency key %q",
			fact.FeedbackID,
			existing.IdempotencyKey,
		)
	}
	if err := validateFeedbackCorrection(fact, state); err != nil {
		return Feedback{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Feedback{}, err
	}
	if _, err := repository.store.AppendJSONL(ledgerStream, local.Event{
		ID:      fact.IdempotencyKey,
		Schema:  FeedbackSchemaVersion,
		Time:    fact.RecordedAt,
		Payload: fact,
	}); err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return Feedback{}, conflictf(
				"idempotency key %q was already used", fact.IdempotencyKey,
			)
		}
		return Feedback{}, fmt.Errorf("append feedback: %w", err)
	}
	return cloneFeedback(fact), nil
}

func (repository *Repository) AppendOutcome(
	ctx context.Context,
	fact Outcome,
) (Outcome, error) {
	if err := checkContext(ctx); err != nil {
		return Outcome{}, err
	}
	fact = cloneOutcome(fact)
	fact.OccurredAt = fact.OccurredAt.UTC()
	fact.RecordedAt = fact.RecordedAt.UTC()
	fact.Window.Start = fact.Window.Start.UTC()
	fact.Window.End = fact.Window.End.UTC()
	if err := fact.Validate(); err != nil {
		return Outcome{}, err
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Outcome{}, err
	}
	if existing, ok := state.byIdempotency[fact.IdempotencyKey]; ok {
		if existing.Kind != FactOutcome || existing.Outcome == nil ||
			!exactJSONEqual(*existing.Outcome, fact) {
			return Outcome{}, conflictf(
				"idempotency key %q was already used", fact.IdempotencyKey,
			)
		}
		return cloneOutcome(*existing.Outcome), nil
	}
	if existing, ok := state.outcomeByID[fact.OutcomeID]; ok {
		return Outcome{}, conflictf(
			"outcome id %q was already recorded with idempotency key %q",
			fact.OutcomeID,
			existing.IdempotencyKey,
		)
	}
	if err := validateOutcomeCorrection(fact, state); err != nil {
		return Outcome{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Outcome{}, err
	}
	if _, err := repository.store.AppendJSONL(ledgerStream, local.Event{
		ID:      fact.IdempotencyKey,
		Schema:  OutcomeSchemaVersion,
		Time:    fact.RecordedAt,
		Payload: fact,
	}); err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return Outcome{}, conflictf(
				"idempotency key %q was already used", fact.IdempotencyKey,
			)
		}
		return Outcome{}, fmt.Errorf("append outcome: %w", err)
	}
	return cloneOutcome(fact), nil
}

func (repository *Repository) FeedbackByFinding(findingID string) ([]Feedback, error) {
	if err := validateID("finding_id", findingID); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]Feedback, 0)
	for _, entry := range state.entries {
		if entry.Feedback != nil && entry.Feedback.FindingID == findingID {
			result = append(result, cloneFeedback(*entry.Feedback))
		}
	}
	return result, nil
}

func (repository *Repository) OutcomesByFinding(findingID string) ([]Outcome, error) {
	if err := validateID("finding_id", findingID); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]Outcome, 0)
	for _, entry := range state.entries {
		if entry.Outcome != nil && entry.Outcome.FindingID == findingID {
			result = append(result, cloneOutcome(*entry.Outcome))
		}
	}
	return result, nil
}

// ByFinding returns both ledgers in their authoritative append order.
func (repository *Repository) ByFinding(findingID string) ([]Entry, error) {
	if err := validateID("finding_id", findingID); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]Entry, 0)
	for _, entry := range state.entries {
		switch {
		case entry.Feedback != nil && entry.Feedback.FindingID == findingID:
			result = append(result, cloneEntry(entry))
		case entry.Outcome != nil && entry.Outcome.FindingID == findingID:
			result = append(result, cloneEntry(entry))
		}
	}
	return result, nil
}

type ledgerState struct {
	entries       []Entry
	byIdempotency map[string]Entry
	feedbackByID  map[string]Feedback
	outcomeByID   map[string]Outcome
}

func (repository *Repository) load() (ledgerState, error) {
	state := ledgerState{
		entries:       make([]Entry, 0),
		byIdempotency: make(map[string]Entry),
		feedbackByID:  make(map[string]Feedback),
		outcomeByID:   make(map[string]Outcome),
	}
	envelopes, err := repository.store.ReadJSONL(ledgerStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return ledgerState{}, fmt.Errorf("%w: read ledger: %v", ErrCorrupt, err)
	}
	for _, envelope := range envelopes {
		entry := Entry{Sequence: envelope.Sequence}
		switch envelope.Schema {
		case FeedbackSchemaVersion:
			fact, err := DecodeFeedbackJSON(envelope.Payload)
			if err != nil {
				return ledgerState{}, corruptf(
					"sequence %d feedback payload: %v", envelope.Sequence, err,
				)
			}
			if envelope.ID != fact.IdempotencyKey ||
				!envelope.Time.Equal(fact.RecordedAt) {
				return ledgerState{}, corruptf(
					"sequence %d feedback envelope does not match payload", envelope.Sequence,
				)
			}
			if _, duplicate := state.feedbackByID[fact.FeedbackID]; duplicate {
				return ledgerState{}, corruptf(
					"duplicate feedback id %q", fact.FeedbackID,
				)
			}
			if err := validateFeedbackCorrection(fact, state); err != nil {
				return ledgerState{}, corruptf(
					"sequence %d: %v", envelope.Sequence, err,
				)
			}
			entry.Kind = FactFeedback
			entry.Feedback = pointerToFeedback(fact)
			state.feedbackByID[fact.FeedbackID] = cloneFeedback(fact)
		case OutcomeSchemaVersion:
			fact, err := DecodeOutcomeJSON(envelope.Payload)
			if err != nil {
				return ledgerState{}, corruptf(
					"sequence %d outcome payload: %v", envelope.Sequence, err,
				)
			}
			if envelope.ID != fact.IdempotencyKey ||
				!envelope.Time.Equal(fact.RecordedAt) {
				return ledgerState{}, corruptf(
					"sequence %d outcome envelope does not match payload", envelope.Sequence,
				)
			}
			if _, duplicate := state.outcomeByID[fact.OutcomeID]; duplicate {
				return ledgerState{}, corruptf(
					"duplicate outcome id %q", fact.OutcomeID,
				)
			}
			if err := validateOutcomeCorrection(fact, state); err != nil {
				return ledgerState{}, corruptf(
					"sequence %d: %v", envelope.Sequence, err,
				)
			}
			entry.Kind = FactOutcome
			entry.Outcome = pointerToOutcome(fact)
			state.outcomeByID[fact.OutcomeID] = cloneOutcome(fact)
		default:
			return ledgerState{}, corruptf(
				"sequence %d has unsupported schema %q",
				envelope.Sequence,
				envelope.Schema,
			)
		}
		if _, duplicate := state.byIdempotency[envelope.ID]; duplicate {
			return ledgerState{}, corruptf(
				"duplicate idempotency key %q", envelope.ID,
			)
		}
		state.byIdempotency[envelope.ID] = cloneEntry(entry)
		state.entries = append(state.entries, cloneEntry(entry))
	}
	return state, nil
}

func validateFeedbackCorrection(fact Feedback, state ledgerState) error {
	if fact.PriorFeedbackID == "" {
		return nil
	}
	prior, exists := state.feedbackByID[fact.PriorFeedbackID]
	if !exists {
		return invalidCorrectionf(
			"prior feedback %q does not exist", fact.PriorFeedbackID,
		)
	}
	if prior.FindingID != fact.FindingID || prior.RunID != fact.RunID {
		return invalidCorrectionf(
			"prior feedback %q belongs to another finding or run",
			fact.PriorFeedbackID,
		)
	}
	if fact.RecordedAt.Before(prior.RecordedAt) {
		return invalidCorrectionf(
			"feedback correction was recorded before its prior fact",
		)
	}
	return nil
}

func validateOutcomeCorrection(fact Outcome, state ledgerState) error {
	if fact.PriorOutcomeID == "" {
		return nil
	}
	prior, exists := state.outcomeByID[fact.PriorOutcomeID]
	if !exists {
		return invalidCorrectionf(
			"prior outcome %q does not exist", fact.PriorOutcomeID,
		)
	}
	if prior.FindingID != fact.FindingID || prior.RunID != fact.RunID {
		return invalidCorrectionf(
			"prior outcome %q belongs to another finding or run",
			fact.PriorOutcomeID,
		)
	}
	if fact.RecordedAt.Before(prior.RecordedAt) {
		return invalidCorrectionf(
			"outcome correction was recorded before its prior fact",
		)
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}

func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

func invalidCorrectionf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCorrection, fmt.Sprintf(format, args...))
}

func cloneFeedback(fact Feedback) Feedback {
	fact.SourceRefs = append([]SourceRef(nil), fact.SourceRefs...)
	return fact
}

func cloneOutcome(fact Outcome) Outcome {
	fact.SourceRefs = append([]SourceRef(nil), fact.SourceRefs...)
	return fact
}

func pointerToFeedback(fact Feedback) *Feedback {
	copy := cloneFeedback(fact)
	return &copy
}

func pointerToOutcome(fact Outcome) *Outcome {
	copy := cloneOutcome(fact)
	return &copy
}

func cloneEntry(entry Entry) Entry {
	copy := Entry{Sequence: entry.Sequence, Kind: entry.Kind}
	if entry.Feedback != nil {
		copy.Feedback = pointerToFeedback(*entry.Feedback)
	}
	if entry.Outcome != nil {
		copy.Outcome = pointerToOutcome(*entry.Outcome)
	}
	return copy
}

// exactJSONEqual is intentionally kept local to the repository. JSON
// equality strips monotonic clock components and protects future composite
// fields from reflect-specific equality surprises.
func exactJSONEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
