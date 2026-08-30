package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/store/local"
)

func TestRepositoryIdempotencyConflictAndRestart(t *testing.T) {
	t.Parallel()
	store, repository := newTestRepository(t)
	fact := validFeedback(time.Date(2026, 7, 27, 6, 0, 0, 0, time.FixedZone("CST", 8*60*60)))

	first, err := repository.AppendFeedback(context.Background(), fact)
	if err != nil {
		t.Fatalf("AppendFeedback() error = %v", err)
	}
	retried, err := repository.AppendFeedback(context.Background(), fact)
	if err != nil {
		t.Fatalf("AppendFeedback(retry) error = %v", err)
	}
	if !first.RecordedAt.Equal(retried.RecordedAt) || first.RecordedAt.Location() != time.UTC {
		t.Fatalf("retry did not return canonical persisted fact: %+v %+v", first, retried)
	}
	envelopes, err := store.ReadJSONL(ledgerStream)
	if err != nil || len(envelopes) != 1 {
		t.Fatalf("ledger after retry = %d events, error %v", len(envelopes), err)
	}

	conflictingKey := fact
	conflictingKey.Action = FeedbackDismiss
	if _, err := repository.AppendFeedback(context.Background(), conflictingKey); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting idempotency error = %v, want ErrConflict", err)
	}
	conflictingID := fact
	conflictingID.IdempotencyKey = "another-command"
	if _, err := repository.AppendFeedback(context.Background(), conflictingID); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting stable id error = %v, want ErrConflict", err)
	}
	outcome := validOutcome(fact.OccurredAt)
	outcome.IdempotencyKey = fact.IdempotencyKey
	if _, err := repository.AppendOutcome(context.Background(), outcome); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-kind idempotency error = %v, want ErrConflict", err)
	}

	reopenedStore, err := local.Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(reopenedStore)
	if err != nil {
		t.Fatalf("New(reopened) error = %v", err)
	}
	history, err := reopened.FeedbackByFinding(fact.FindingID)
	if err != nil || len(history) != 1 {
		t.Fatalf("reopened FeedbackByFinding() = %d, %v", len(history), err)
	}
	if history[0].FeedbackID != fact.FeedbackID {
		t.Fatalf("reopened feedback id = %q", history[0].FeedbackID)
	}
}

func TestRepositoryCorrectionsAppendWithoutOverwritingHistory(t *testing.T) {
	t.Parallel()
	store, repository := newTestRepository(t)
	start := time.Date(2026, 7, 27, 7, 0, 0, 0, time.UTC)
	original := validFeedback(start)
	if _, err := repository.AppendFeedback(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	correction := validFeedback(start.Add(time.Minute))
	correction.FeedbackID = "feedback-2"
	correction.Action = FeedbackDismiss
	correction.PriorFeedbackID = original.FeedbackID
	correction.IdempotencyKey = "record-feedback-2"
	if _, err := repository.AppendFeedback(context.Background(), correction); err != nil {
		t.Fatalf("AppendFeedback(correction) error = %v", err)
	}

	originalOutcome := validOutcome(start.Add(2 * time.Minute))
	if _, err := repository.AppendOutcome(context.Background(), originalOutcome); err != nil {
		t.Fatal(err)
	}
	outcomeCorrection := validOutcome(start.Add(3 * time.Minute))
	outcomeCorrection.OutcomeID = "outcome-2"
	outcomeCorrection.State = OutcomeRecurred
	outcomeCorrection.PriorOutcomeID = originalOutcome.OutcomeID
	outcomeCorrection.IdempotencyKey = "record-outcome-2"
	if _, err := repository.AppendOutcome(context.Background(), outcomeCorrection); err != nil {
		t.Fatalf("AppendOutcome(correction) error = %v", err)
	}

	feedbackHistory, err := repository.FeedbackByFinding(original.FindingID)
	if err != nil || len(feedbackHistory) != 2 {
		t.Fatalf("FeedbackByFinding() = %d, %v", len(feedbackHistory), err)
	}
	if feedbackHistory[0].Action != FeedbackAccept ||
		feedbackHistory[1].PriorFeedbackID != feedbackHistory[0].FeedbackID ||
		feedbackHistory[1].Action != FeedbackDismiss {
		t.Fatalf("feedback correction history = %#v", feedbackHistory)
	}
	outcomeHistory, err := repository.OutcomesByFinding(original.FindingID)
	if err != nil || len(outcomeHistory) != 2 {
		t.Fatalf("OutcomesByFinding() = %d, %v", len(outcomeHistory), err)
	}
	if outcomeHistory[0].State != OutcomeFixed ||
		outcomeHistory[1].PriorOutcomeID != outcomeHistory[0].OutcomeID ||
		outcomeHistory[1].State != OutcomeRecurred {
		t.Fatalf("outcome correction history = %#v", outcomeHistory)
	}
	combined, err := repository.ByFinding(original.FindingID)
	if err != nil || len(combined) != 4 {
		t.Fatalf("ByFinding() = %d, %v", len(combined), err)
	}
	for index, entry := range combined {
		if entry.Sequence != uint64(index+1) {
			t.Fatalf("entry[%d] sequence = %d", index, entry.Sequence)
		}
	}

	// Mutating a query result cannot mutate the authoritative ledger.
	feedbackHistory[0].SourceRefs[0].ID = "mutated"
	reloaded, err := repository.FeedbackByFinding(original.FindingID)
	if err != nil || reloaded[0].SourceRefs[0].ID != "comment-1" {
		t.Fatalf("query result aliased repository state: %#v, %v", reloaded, err)
	}
	envelopes, err := store.ReadJSONL(ledgerStream)
	if err != nil || len(envelopes) != 4 {
		t.Fatalf("ledger has %d events, error %v", len(envelopes), err)
	}
}

func TestRepositoryRejectsInvalidCorrectionWithoutAppending(t *testing.T) {
	t.Parallel()
	store, repository := newTestRepository(t)
	start := time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC)
	fact := validFeedback(start)
	fact.FeedbackID = "feedback-correction"
	fact.PriorFeedbackID = "missing-feedback"
	if _, err := repository.AppendFeedback(context.Background(), fact); !errors.Is(err, ErrInvalidCorrection) {
		t.Fatalf("missing prior feedback error = %v, want ErrInvalidCorrection", err)
	}
	if events, err := store.ReadJSONL(ledgerStream); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid correction ledger = %#v, %v; want not found", events, err)
	}

	original := validFeedback(start)
	if _, err := repository.AppendFeedback(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	wrongFinding := validFeedback(start.Add(time.Minute))
	wrongFinding.FeedbackID = "feedback-wrong-finding"
	wrongFinding.FindingID = "finding-2"
	wrongFinding.PriorFeedbackID = original.FeedbackID
	wrongFinding.IdempotencyKey = "record-feedback-wrong-finding"
	if _, err := repository.AppendFeedback(context.Background(), wrongFinding); !errors.Is(err, ErrInvalidCorrection) {
		t.Fatalf("foreign prior feedback error = %v, want ErrInvalidCorrection", err)
	}
	events, err := store.ReadJSONL(ledgerStream)
	if err != nil || len(events) != 1 {
		t.Fatalf("invalid correction changed ledger: %d, %v", len(events), err)
	}
}

func TestRepositoryQueriesOnlyRequestedFinding(t *testing.T) {
	t.Parallel()
	_, repository := newTestRepository(t)
	start := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	first := validFeedback(start)
	second := validFeedback(start.Add(time.Minute))
	second.FeedbackID = "feedback-other"
	second.FindingID = "finding-other"
	second.IdempotencyKey = "record-feedback-other"
	outcome := validOutcome(start.Add(2 * time.Minute))
	for _, fact := range []Feedback{first, second} {
		if _, err := repository.AppendFeedback(context.Background(), fact); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.AppendOutcome(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}

	entries, err := repository.ByFinding(first.FindingID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ByFinding(first) = %d, %v", len(entries), err)
	}
	if entries[0].Kind != FactFeedback || entries[1].Kind != FactOutcome {
		t.Fatalf("ByFinding kinds = %q, %q", entries[0].Kind, entries[1].Kind)
	}
	missing, err := repository.ByFinding("finding-missing")
	if err != nil || missing == nil || len(missing) != 0 {
		t.Fatalf("ByFinding(missing) = %#v, %v; want explicit empty slice", missing, err)
	}
}

func TestRepositoryConcurrentRetryAppendsOnce(t *testing.T) {
	t.Parallel()
	store, firstRepository := newTestRepository(t)
	secondRepository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	repositories := []*Repository{firstRepository, secondRepository}
	fact := validFeedback(time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC))

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := repositories[index%len(repositories)].AppendFeedback(context.Background(), fact)
			errs <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent retry error = %v", err)
		}
	}
	events, err := store.ReadJSONL(ledgerStream)
	if err != nil || len(events) != 1 {
		t.Fatalf("concurrent retry appended %d events, error %v", len(events), err)
	}
}

func TestRepositoryConcurrentBusinessIDConflictAppendsOneFact(t *testing.T) {
	t.Parallel()
	store, firstRepository := newTestRepository(t)
	secondRepository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	first := validFeedback(time.Date(2026, 7, 27, 10, 30, 0, 0, time.UTC))
	second := first
	second.Action = FeedbackDismiss
	second.IdempotencyKey = "record-feedback-conflict"

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, command := range []struct {
		repository *Repository
		fact       Feedback
	}{
		{repository: firstRepository, fact: first},
		{repository: secondRepository, fact: second},
	} {
		command := command
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := command.repository.AppendFeedback(context.Background(), command.fact)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent business id command error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results: successes=%d conflicts=%d", successes, conflicts)
	}
	events, err := store.ReadJSONL(ledgerStream)
	if err != nil || len(events) != 1 {
		t.Fatalf("business id conflict appended %d events, error %v", len(events), err)
	}
}

func TestOutcomeIdempotencyAndConflict(t *testing.T) {
	t.Parallel()
	store, repository := newTestRepository(t)
	fact := validOutcome(time.Date(2026, 7, 27, 10, 45, 0, 0, time.UTC))
	if _, err := repository.AppendOutcome(context.Background(), fact); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AppendOutcome(context.Background(), fact); err != nil {
		t.Fatalf("AppendOutcome(retry) error = %v", err)
	}
	conflicting := fact
	conflicting.State = OutcomeUnknown
	if _, err := repository.AppendOutcome(context.Background(), conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("AppendOutcome(conflict) error = %v, want ErrConflict", err)
	}
	events, err := store.ReadJSONL(ledgerStream)
	if err != nil || len(events) != 1 {
		t.Fatalf("outcome retry appended %d events, error %v", len(events), err)
	}
}

func TestRepositoryRejectsStrictJSONCorruptionOnOpen(t *testing.T) {
	t.Parallel()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fact := validFeedback(time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC))
	data, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := store.AppendJSONL(ledgerStream, local.Event{
		ID: fact.IdempotencyKey, Schema: FeedbackSchemaVersion,
		Time: fact.RecordedAt, Payload: json.RawMessage(data),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(store); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("New(corrupt ledger) error = %v, want ErrCorrupt", err)
	}
}

func TestRepositoryRequiresLiveContext(t *testing.T) {
	t.Parallel()
	_, repository := newTestRepository(t)
	fact := validFeedback(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC))
	if _, err := repository.AppendFeedback(nil, fact); err == nil {
		t.Fatal("AppendFeedback(nil context) unexpectedly succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.AppendFeedback(ctx, fact); !errors.Is(err, context.Canceled) {
		t.Fatalf("AppendFeedback(canceled) error = %v", err)
	}
}

func newTestRepository(t *testing.T) (*local.Store, *Repository) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatalf("local.Open() error = %v", err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return store, repository
}
