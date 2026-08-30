package configrepo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/store/local"
)

func TestLifecycleRequiresValidationAndRollbackRestoresPreviousRevision(t *testing.T) {
	repository, store := newConfigRepository(t)
	first := testRevision("platform", "1", 100)
	second := testRevision("platform", "2", 200)
	createRevision(t, repository, first, 1)
	createRevision(t, repository, second, 2)

	if _, err := repository.Publish(
		context.Background(),
		first.ID,
		first.Revision,
		Rollout{Percentage: 100},
		testMutation("publish-draft", 3),
	); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Publish(draft) error = %v, want ErrInvalidTransition", err)
	}
	if events, err := store.ReadJSONL(lifecycleStream); err != nil || len(events) != 2 {
		t.Fatalf("invalid publish changed ledger: events=%d error=%v", len(events), err)
	}

	validateRevision(t, repository, first, 4)
	publishRevision(t, repository, first, Rollout{Percentage: 100}, 5)
	validateRevision(t, repository, second, 6)
	before, err := store.ReadJSONL(lifecycleStream)
	if err != nil {
		t.Fatal(err)
	}
	publishRevision(t, repository, second, Rollout{Percentage: 100}, 7)
	afterPublish, err := store.ReadJSONL(lifecycleStream)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPublish) != len(before)+1 {
		t.Fatalf("100%% replacement appended %d events, want exactly one",
			len(afterPublish)-len(before))
	}
	assertStatus(t, repository, first, StatusSuperseded)
	assertStatus(t, repository, second, StatusPublished)
	assertActiveRevision(t, repository, second)

	before = afterPublish
	rolledBack, err := repository.Rollback(
		context.Background(),
		second.ID,
		second.Revision,
		testMutation("rollback-second", 8),
	)
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if rolledBack.Status != StatusRolledBack {
		t.Fatalf("rollback status = %q", rolledBack.Status)
	}
	afterRollback, err := store.ReadJSONL(lifecycleStream)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRollback) != len(before)+1 {
		t.Fatalf("rollback appended %d events, want exactly one",
			len(afterRollback)-len(before))
	}
	assertStatus(t, repository, first, StatusPublished)
	assertStatus(t, repository, second, StatusRolledBack)
	assertActiveRevision(t, repository, first)

	firstHistory, err := repository.History(first.ID, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if got := historyStatuses(firstHistory); !reflect.DeepEqual(
		got,
		[]Status{
			StatusDraft,
			StatusValidated,
			StatusPublished,
			StatusSuperseded,
			StatusPublished,
		},
	) {
		t.Fatalf("first history statuses = %v", got)
	}
	secondHistory, err := repository.History(second.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if got := historyStatuses(secondHistory); !reflect.DeepEqual(
		got,
		[]Status{StatusDraft, StatusValidated, StatusPublished, StatusRolledBack},
	) {
		t.Fatalf("second history statuses = %v", got)
	}
	last := secondHistory[len(secondHistory)-1]
	if last.Actor != "actor-8" ||
		last.Audit != "audit event 8" ||
		!last.At.Equal(testTime(8)) {
		t.Fatalf("rollback audit = %+v", last)
	}
}

func TestPercentageRolloutAssignmentIsDeterministicAndFallsBack(t *testing.T) {
	repository, _ := newConfigRepository(t)
	baseline := testRevision("platform", "1", 100)
	candidate := testRevision("platform", "2", 200)
	createValidatePublish(t, repository, baseline, Rollout{Percentage: 100}, 1)
	createRevision(t, repository, candidate, 4)
	validateRevision(t, repository, candidate, 5)
	publishRevision(
		t,
		repository,
		candidate,
		Rollout{Percentage: 25, Seed: "rollout-seed"},
		6,
	)
	assertStatus(t, repository, baseline, StatusPublished)
	assertStatus(t, repository, candidate, StatusPublished)

	var hitContext, missContext *reviewconfig.ResolutionContext
	for index := 0; index < 1000 && (hitContext == nil || missContext == nil); index++ {
		context := testResolutionContext(fmt.Sprintf("invocation-%d", index))
		active, err := repository.ActiveRevisions(context)
		if err != nil {
			t.Fatalf("ActiveRevisions() error = %v", err)
		}
		if len(active) != 1 {
			t.Fatalf("active revisions = %+v", active)
		}
		if active[0].Revision == candidate.Revision {
			copy := context
			hitContext = &copy
		} else if active[0].Revision == baseline.Revision {
			copy := context
			missContext = &copy
		} else {
			t.Fatalf("unexpected active revision = %+v", active[0])
		}
	}
	if hitContext == nil || missContext == nil {
		t.Fatalf("did not observe both rollout hit and fallback")
	}
	for attempt := 0; attempt < 20; attempt++ {
		assertActiveRevisionForContext(t, repository, candidate, *hitContext)
		assertActiveRevisionForContext(t, repository, baseline, *missContext)
	}
	rolledBack, err := repository.Rollback(
		context.Background(),
		candidate.ID,
		candidate.Revision,
		testMutation("rollback-rollout", 7),
	)
	if err != nil || rolledBack.Status != StatusRolledBack {
		t.Fatalf("Rollback(rollout) = %+v, %v", rolledBack, err)
	}
	assertStatus(t, repository, baseline, StatusPublished)
	assertActiveRevisionForContext(t, repository, baseline, *hitContext)
	assertActiveRevisionForContext(t, repository, baseline, *missContext)
}

func TestRolloutAdvanceMonotonicallyWidensAndKeepsOriginalRollbackFrame(t *testing.T) {
	repository, store := newConfigRepository(t)
	baseline := testRevision("platform", "baseline", 100)
	candidate := testRevision("platform", "candidate", 200)
	createValidatePublish(t, repository, baseline, Rollout{Percentage: 100}, 1)
	createRevision(t, repository, candidate, 4)
	validateRevision(t, repository, candidate, 5)
	publishRevision(
		t,
		repository,
		candidate,
		Rollout{Percentage: 10, Seed: "stable-canary"},
		6,
	)

	var originalHit, expandedHit, miss *reviewconfig.ResolutionContext
	for index := 0; index < 10_000 && (originalHit == nil || expandedHit == nil || miss == nil); index++ {
		ctx := testResolutionContext(fmt.Sprintf("advance-invocation-%d", index))
		selector := selectorIdentity(candidate)
		bucket10 := rolloutHit(ctx, selector, activation{
			Key:        revisionKey{ID: candidate.ID, Revision: candidate.Revision},
			Percentage: 10, Seed: "stable-canary",
		})
		bucket40 := rolloutHit(ctx, selector, activation{
			Key:        revisionKey{ID: candidate.ID, Revision: candidate.Revision},
			Percentage: 40, Seed: "stable-canary",
		})
		switch {
		case bucket10 && originalHit == nil:
			copy := ctx
			originalHit = &copy
		case !bucket10 && bucket40 && expandedHit == nil:
			copy := ctx
			expandedHit = &copy
		case !bucket40 && miss == nil:
			copy := ctx
			miss = &copy
		}
	}
	if originalHit == nil || expandedHit == nil || miss == nil {
		t.Fatal("failed to find deterministic rollout buckets")
	}
	assertActiveRevisionForContext(t, repository, candidate, *originalHit)
	assertActiveRevisionForContext(t, repository, baseline, *expandedHit)
	assertActiveRevisionForContext(t, repository, baseline, *miss)

	advanced, err := repository.AdvanceRollout(
		context.Background(), candidate.ID, candidate.Revision,
		Rollout{Percentage: 40, Seed: "stable-canary"},
		testMutation("advance-candidate-40", 7),
	)
	if err != nil || advanced.Status != StatusPublished {
		t.Fatalf("AdvanceRollout(40) = %+v, %v", advanced, err)
	}
	assertActiveRevisionForContext(t, repository, candidate, *originalHit)
	assertActiveRevisionForContext(t, repository, candidate, *expandedHit)
	assertActiveRevisionForContext(t, repository, baseline, *miss)

	advanced, err = repository.AdvanceRollout(
		context.Background(), candidate.ID, candidate.Revision,
		Rollout{Percentage: 100, Seed: "stable-canary"},
		testMutation("advance-candidate-100", 8),
	)
	if err != nil || advanced.Status != StatusPublished {
		t.Fatalf("AdvanceRollout(100) = %+v, %v", advanced, err)
	}
	assertStatus(t, repository, baseline, StatusSuperseded)
	assertActiveRevisionForContext(t, repository, candidate, *originalHit)
	assertActiveRevisionForContext(t, repository, candidate, *expandedHit)
	assertActiveRevisionForContext(t, repository, candidate, *miss)

	rolledBack, err := repository.Rollback(
		context.Background(), candidate.ID, candidate.Revision,
		testMutation("rollback-advanced-candidate", 9),
	)
	if err != nil || rolledBack.Status != StatusRolledBack {
		t.Fatalf("Rollback(advanced) = %+v, %v", rolledBack, err)
	}
	assertStatus(t, repository, baseline, StatusPublished)
	assertActiveRevisionForContext(t, repository, baseline, *originalHit)
	assertActiveRevisionForContext(t, repository, baseline, *expandedHit)
	assertActiveRevisionForContext(t, repository, baseline, *miss)

	history, err := repository.History(candidate.ID, candidate.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 6 || history[3].Type != EventRolloutAdvanced ||
		history[3].Rollout == nil || history[3].Rollout.Percentage != 40 ||
		history[4].Type != EventRolloutAdvanced || history[4].Rollout == nil ||
		history[4].Rollout.Percentage != 100 || history[5].Type != EventRolledBack {
		t.Fatalf("candidate rollout history = %+v", history)
	}
	events, err := store.ReadJSONL(lifecycleStream)
	if err != nil || len(events) != 9 {
		t.Fatalf("lifecycle events = %d, %v", len(events), err)
	}
}

func TestRolloutAdvanceRejectsNonMonotonicSeedDriftAndInactiveTarget(t *testing.T) {
	repository, store := newConfigRepository(t)
	baseline := testRevision("platform", "baseline", 100)
	candidate := testRevision("platform", "candidate", 200)
	createValidatePublish(t, repository, baseline, Rollout{Percentage: 100}, 1)
	createRevision(t, repository, candidate, 4)
	validateRevision(t, repository, candidate, 5)
	publishRevision(t, repository, candidate, Rollout{Percentage: 20, Seed: "seed"}, 6)
	before, err := store.ReadJSONL(lifecycleStream)
	if err != nil {
		t.Fatal(err)
	}
	for index, rollout := range []Rollout{
		{Percentage: 20, Seed: "seed"},
		{Percentage: 10, Seed: "seed"},
		{Percentage: 30, Seed: "changed"},
	} {
		if _, err := repository.AdvanceRollout(
			context.Background(), candidate.ID, candidate.Revision, rollout,
			testMutation(fmt.Sprintf("invalid-advance-%d", index), 7+index),
		); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("AdvanceRollout(%+v) error = %v", rollout, err)
		}
	}
	if _, err := repository.AdvanceRollout(
		context.Background(), baseline.ID, baseline.Revision,
		Rollout{Percentage: 100, Seed: ""}, testMutation("advance-baseline", 10),
	); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("AdvanceRollout(baseline) error = %v", err)
	}
	after, err := store.ReadJSONL(lifecycleStream)
	if err != nil || len(after) != len(before) {
		t.Fatalf("invalid advances changed ledger: %d -> %d, %v", len(before), len(after), err)
	}
}

func TestInvalidRollbackAndPercentagePublishDoNotAppend(t *testing.T) {
	t.Run("rollout without baseline", func(t *testing.T) {
		repository, store := newConfigRepository(t)
		revision := testRevision("platform", "1", 100)
		createRevision(t, repository, revision, 1)
		validateRevision(t, repository, revision, 2)
		_, err := repository.Publish(
			context.Background(),
			revision.ID,
			revision.Revision,
			Rollout{Percentage: 10, Seed: "seed"},
			testMutation("publish-partial", 3),
		)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("partial Publish() error = %v", err)
		}
		events, readErr := store.ReadJSONL(lifecycleStream)
		if readErr != nil || len(events) != 2 {
			t.Fatalf("invalid partial publish changed ledger: %d, %v", len(events), readErr)
		}
		assertStatus(t, repository, revision, StatusValidated)
	})

	t.Run("rollback without previous", func(t *testing.T) {
		repository, store := newConfigRepository(t)
		revision := testRevision("platform", "1", 100)
		createValidatePublish(t, repository, revision, Rollout{Percentage: 100}, 1)
		before, err := store.ReadJSONL(lifecycleStream)
		if err != nil {
			t.Fatal(err)
		}
		_, err = repository.Rollback(
			context.Background(),
			revision.ID,
			revision.Revision,
			testMutation("rollback-first", 4),
		)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("Rollback(first) error = %v", err)
		}
		after, readErr := store.ReadJSONL(lifecycleStream)
		if readErr != nil || len(after) != len(before) {
			t.Fatalf("invalid rollback changed ledger: %d -> %d, %v",
				len(before), len(after), readErr)
		}
		assertStatus(t, repository, revision, StatusPublished)
	})
}

func TestConcurrentCommandsAreIdempotentAndProjectionRemainsAtomic(t *testing.T) {
	repository, _ := newConfigRepository(t)
	base := testRevision("platform", "base", 100)
	createMutation := testMutation("create-base", 1)
	const callers = 20
	var wait sync.WaitGroup
	errorsByCaller := make(chan error, callers)
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := repository.Create(context.Background(), base, createMutation)
			errorsByCaller <- err
		}()
	}
	wait.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("idempotent concurrent Create() error = %v", err)
		}
	}
	history, err := repository.History(base.ID, base.Revision)
	if err != nil || len(history) != 1 {
		t.Fatalf("idempotent create history = %+v, %v", history, err)
	}

	validateMutation := testMutation("validate-base", 2)
	errorsByCaller = make(chan error, callers)
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := repository.ValidateRevision(
				context.Background(),
				base.ID,
				base.Revision,
				validateMutation,
			)
			errorsByCaller <- err
		}()
	}
	wait.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("idempotent concurrent ValidateRevision() error = %v", err)
		}
	}

	candidates := []reviewconfig.Revision{
		testRevision("platform", "candidate-1", 101),
		testRevision("platform", "candidate-2", 102),
		testRevision("platform", "candidate-3", 103),
	}
	for index, revision := range candidates {
		createRevision(t, repository, revision, 10+index*2)
		validateRevision(t, repository, revision, 11+index*2)
	}
	errorsByCaller = make(chan error, len(candidates))
	for index := range candidates {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			revision := candidates[index]
			_, err := repository.Publish(
				context.Background(),
				revision.ID,
				revision.Revision,
				Rollout{Percentage: 100},
				testMutation(fmt.Sprintf("publish-candidate-%d", index), 30+index),
			)
			errorsByCaller <- err
		}(index)
	}
	wait.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("concurrent Publish() error = %v", err)
		}
	}
	records, err := repository.List()
	if err != nil {
		t.Fatal(err)
	}
	published := 0
	for _, record := range records {
		if record.Status == StatusPublished {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("published records = %d, want one atomic selector projection: %+v",
			published, records)
	}
	active, err := repository.ActiveRevisions(testResolutionContext("invocation-1"))
	if err != nil || len(active) != 1 {
		t.Fatalf("active revisions = %+v, %v", active, err)
	}
}

func TestImmutableConflictAndIdempotencyConflictFailClosed(t *testing.T) {
	repository, _ := newConfigRepository(t)
	original := testRevision("platform", "1", 100)
	createRevision(t, repository, original, 1)
	mutated := testRevision("platform", "1", 999)

	if _, err := repository.Create(
		context.Background(),
		mutated,
		testMutation("create-mutated", 2),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("Create(mutated identity) error = %v", err)
	}
	if _, err := repository.Create(
		context.Background(),
		original,
		Mutation{
			IdempotencyKey: "create-platform-1",
			Actor:          "different-actor",
			Audit:          "different audit",
			At:             testTime(1),
		},
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("Create(conflicting idempotency payload) error = %v", err)
	}
	record, err := repository.Get(original.ID, original.Revision)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := reviewconfig.DigestRevision(original)
	if err != nil {
		t.Fatal(err)
	}
	if record.SHA256 != wantDigest || record.Revision.Patch.Target.MaxFiles == nil ||
		*record.Revision.Patch.Target.MaxFiles != 100 {
		t.Fatalf("immutable record changed = %+v", record)
	}
}

func TestRestartRebuildsLifecycleProjectionAndHistory(t *testing.T) {
	root := t.TempDir()
	store, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	first := testRevision("platform", "1", 100)
	second := testRevision("platform", "2", 200)
	createValidatePublish(t, repository, first, Rollout{Percentage: 100}, 1)
	createRevision(t, repository, second, 4)
	validateRevision(t, repository, second, 5)
	publishRevision(t, repository, second, Rollout{Percentage: 40, Seed: "seed"}, 6)
	beforeList, err := repository.List()
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := repository.History(second.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(reopenedStore)
	if err != nil {
		t.Fatalf("New(reopened) error = %v", err)
	}
	afterList, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := reopened.History(second.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeList, afterList) ||
		!reflect.DeepEqual(beforeHistory, afterHistory) {
		t.Fatalf("restart projection differs:\nbefore=%+v/%+v\nafter=%+v/%+v",
			beforeList, beforeHistory, afterList, afterHistory)
	}
	for index := 0; index < 50; index++ {
		context := testResolutionContext(fmt.Sprintf("restart-%d", index))
		before, err := repository.ActiveRevisions(context)
		if err != nil {
			t.Fatal(err)
		}
		after, err := reopened.ActiveRevisions(context)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("restart assignment differs for %+v: %+v / %+v",
				context, before, after)
		}
	}
}

func TestNewFailsClosedOnUnknownLifecyclePayload(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := testTime(1)
	_, err = store.AppendJSONL(lifecycleStream, local.Event{
		ID: "unknown-event", Schema: lifecycleEventSchemaVersion, Time: now,
		Payload: map[string]any{
			"schema_version": lifecycleEventSchemaVersion,
			"type":           EventCreated,
			"revision_id":    "platform",
			"revision":       "1",
			"sha256":         testDigest,
			"actor":          "actor",
			"audit":          "audit",
			"occurred_at":    now,
			"unknown":        true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(store); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("New(corrupt lifecycle) error = %v, want ErrCorrupt", err)
	}
}

const testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newConfigRepository(t *testing.T) (*Repository, *local.Store) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatalf("local.Open() error = %v", err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return repository, store
}

func testRevision(
	id string,
	version string,
	maxFiles int,
) reviewconfig.Revision {
	return reviewconfig.Revision{
		SchemaVersion: reviewconfig.RevisionSchemaVersion,
		ID:            id,
		Revision:      version,
		Scope:         reviewconfig.ScopePlatform,
		Selector:      reviewconfig.Selector{},
		Patch: reviewconfig.ConfigPatch{
			Target: &reviewconfig.TargetPatch{MaxFiles: &maxFiles},
		},
	}
}

func testMutation(id string, offset int) Mutation {
	return Mutation{
		IdempotencyKey: id,
		Actor:          fmt.Sprintf("actor-%d", offset),
		Audit:          fmt.Sprintf("audit event %d", offset),
		At:             testTime(offset),
	}
}

func testTime(offset int) time.Time {
	return time.Date(2026, time.July, 27, 2, 0, offset, 0, time.UTC)
}

func testResolutionContext(invocation string) reviewconfig.ResolutionContext {
	return reviewconfig.ResolutionContext{
		TenantID:       "tenant-1",
		OrganizationID: "organization-1",
		RepositoryID:   "repository-1",
		Path:           "internal/review.go",
		InvocationID:   invocation,
	}
}

func createRevision(
	t *testing.T,
	repository *Repository,
	revision reviewconfig.Revision,
	offset int,
) {
	t.Helper()
	record, err := repository.Create(
		context.Background(),
		revision,
		testMutation("create-"+revision.ID+"-"+revision.Revision, offset),
	)
	if err != nil {
		t.Fatalf("Create(%s@%s) error = %v", revision.ID, revision.Revision, err)
	}
	if record.Status != StatusDraft {
		t.Fatalf("created status = %q", record.Status)
	}
}

func validateRevision(
	t *testing.T,
	repository *Repository,
	revision reviewconfig.Revision,
	offset int,
) {
	t.Helper()
	record, err := repository.ValidateRevision(
		context.Background(),
		revision.ID,
		revision.Revision,
		testMutation("validate-"+revision.ID+"-"+revision.Revision, offset),
	)
	if err != nil {
		t.Fatalf("ValidateRevision(%s@%s) error = %v",
			revision.ID, revision.Revision, err)
	}
	if record.Status != StatusValidated {
		t.Fatalf("validated status = %q", record.Status)
	}
}

func publishRevision(
	t *testing.T,
	repository *Repository,
	revision reviewconfig.Revision,
	rollout Rollout,
	offset int,
) {
	t.Helper()
	record, err := repository.Publish(
		context.Background(),
		revision.ID,
		revision.Revision,
		rollout,
		testMutation("publish-"+revision.ID+"-"+revision.Revision, offset),
	)
	if err != nil {
		t.Fatalf("Publish(%s@%s) error = %v", revision.ID, revision.Revision, err)
	}
	if record.Status != StatusPublished {
		t.Fatalf("published status = %q", record.Status)
	}
}

func createValidatePublish(
	t *testing.T,
	repository *Repository,
	revision reviewconfig.Revision,
	rollout Rollout,
	startOffset int,
) {
	t.Helper()
	createRevision(t, repository, revision, startOffset)
	validateRevision(t, repository, revision, startOffset+1)
	publishRevision(t, repository, revision, rollout, startOffset+2)
}

func assertStatus(
	t *testing.T,
	repository *Repository,
	revision reviewconfig.Revision,
	want Status,
) {
	t.Helper()
	record, err := repository.Get(revision.ID, revision.Revision)
	if err != nil {
		t.Fatalf("Get(%s@%s) error = %v", revision.ID, revision.Revision, err)
	}
	if record.Status != want {
		t.Fatalf("%s@%s status = %q, want %q",
			revision.ID, revision.Revision, record.Status, want)
	}
}

func assertActiveRevision(
	t *testing.T,
	repository *Repository,
	want reviewconfig.Revision,
) {
	t.Helper()
	assertActiveRevisionForContext(
		t,
		repository,
		want,
		testResolutionContext("invocation-1"),
	)
}

func assertActiveRevisionForContext(
	t *testing.T,
	repository *Repository,
	want reviewconfig.Revision,
	context reviewconfig.ResolutionContext,
) {
	t.Helper()
	active, err := repository.ActiveRevisions(context)
	if err != nil {
		t.Fatalf("ActiveRevisions() error = %v", err)
	}
	if len(active) != 1 ||
		active[0].ID != want.ID ||
		active[0].Revision != want.Revision {
		t.Fatalf("active revisions = %+v, want %s@%s",
			active, want.ID, want.Revision)
	}
}

func historyStatuses(history []AuditEntry) []Status {
	result := make([]Status, len(history))
	for index, entry := range history {
		result[index] = entry.Status
	}
	return result
}
