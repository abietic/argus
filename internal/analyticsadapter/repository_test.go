package analyticsadapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/application"
	feedbackdomain "github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/publication"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/targetmodel"
)

type analyticsPublicationAuthorizer struct{}

func (analyticsPublicationAuthorizer) AuthorizePublication(
	context.Context,
	publication.Request,
) error {
	return nil
}

func TestRebuildPersistsDigestBoundSnapshotAndQueriesWithoutLedgers(t *testing.T) {
	stateRoot := t.TempDir()
	store, runs, ledger, run, repositoryID := analyticsFixture(t, stateRoot)
	evaluations := &staticEvaluationSource{batch: EvaluationBatch{
		Completeness:       analytics.CompletenessComplete,
		IncompleteReasons:  []string{},
		Facts:              []analytics.ExperimentFact{},
		RepeatabilityFacts: []analytics.RepeatabilityFact{},
	}}
	publications, err := publication.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(runs, ledger, store, evaluations, publications)
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRebuildRequest("snapshot-1", repositoryID)
	snapshot, err := adapter.Rebuild(context.Background(), request)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if evaluations.calls != 1 {
		t.Fatalf("evaluation source calls = %d, want 1", evaluations.calls)
	}
	if len(snapshot.Facts.ReviewRuns) != 1 ||
		snapshot.Facts.ReviewRuns[0].RunID != run.Run.RunID ||
		len(snapshot.RunBindings) != 1 {
		t.Fatalf("snapshot run facts = %+v", snapshot)
	}
	if len(snapshot.Facts.FeedbackOutcomes) != 0 {
		t.Fatalf("feedback projections = %+v", snapshot.Facts.FeedbackOutcomes)
	}
	if len(snapshot.Facts.Findings) == 0 ||
		snapshot.Facts.Findings[0].PublicationEligibility !=
			analytics.EligibilityPublishEligible ||
		snapshot.Facts.Findings[0].Publication !=
			analytics.PublicationNotReached ||
		snapshot.Facts.ReviewRuns[0].Funnel.Published != 0 {
		t.Fatalf("decision eligibility was confused with publication: %+v", snapshot.Facts)
	}
	if snapshot.Facts.Completeness != analytics.CompletenessComplete {
		t.Fatalf("fact completeness = %+v", snapshot.Facts)
	}
	if snapshot.Coverage[5].Source != CoveragePublication ||
		snapshot.Coverage[5].Completeness != analytics.CompletenessComplete {
		t.Fatalf("publication coverage = %+v", snapshot.Coverage)
	}
	if _, err := os.Stat(
		filepath.Join(stateRoot, "streams", "analytics", "projection-index.jsonl"),
	); !os.IsNotExist(err) {
		t.Fatalf("projection index JSONL unexpectedly exists: %v", err)
	}

	// Remove every authoritative append-only ledger after materialization.
	// Query must still succeed because it reads the immutable manifest and
	// content-addressed projection artifact only.
	if err := os.Rename(
		filepath.Join(stateRoot, "streams"),
		filepath.Join(stateRoot, "authoritative-ledgers-offline"),
	); err != nil {
		t.Fatalf("take ledgers offline: %v", err)
	}
	queryOnly, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := queryOnly.Query(request.SnapshotID)
	if err != nil {
		t.Fatalf("Query() with ledgers offline error = %v", err)
	}
	if !reflect.DeepEqual(loaded, snapshot) {
		t.Fatalf("loaded snapshot differs from rebuilt snapshot")
	}
	latest, err := queryOnly.QueryLatest(Selector{
		Scope: request.Scope, Window: request.Window, GroupBy: request.GroupBy,
	})
	if err != nil {
		t.Fatalf("QueryLatest() with ledgers offline error = %v", err)
	}
	if latest.SnapshotID != request.SnapshotID {
		t.Fatalf("latest snapshot = %q", latest.SnapshotID)
	}
	for _, format := range []string{
		analytics.CanonicalJSONExportFormat,
		analytics.CanonicalCSVExportFormat,
	} {
		bundle, err := queryOnly.Export(request.SnapshotID, ExportDashboard, format)
		if err != nil {
			t.Fatalf("Export(%s) error = %v", format, err)
		}
		if bundle.Manifest.Parquet ||
			len(bundle.Manifest.Limitations) != 1 ||
			bundle.Manifest.Limitations[0] != "parquet_not_emitted_no_runtime_dependency" {
			t.Fatalf("export capability manifest = %+v", bundle.Manifest)
		}
	}
}

func TestRebuildWithoutPublicationSourceKeepsEligibilityButMarksOutcomeUnknown(
	t *testing.T,
) {
	store, runs, ledger, _, repositoryID := analyticsFixture(t, t.TempDir())
	adapter, err := New(runs, ledger, store, &staticEvaluationSource{
		batch: EvaluationBatch{
			Completeness:       analytics.CompletenessComplete,
			IncompleteReasons:  []string{},
			Facts:              []analytics.ExperimentFact{},
			RepeatabilityFacts: []analytics.RepeatabilityFact{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Rebuild(
		context.Background(),
		fixtureRebuildRequest("snapshot-publication-source-absent", repositoryID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Facts.Findings) == 0 {
		t.Fatal("fixture produced no finding facts")
	}
	finding := snapshot.Facts.Findings[0]
	if finding.PublicationEligibility != analytics.EligibilityPublishEligible ||
		finding.Publication != analytics.PublicationUnknown ||
		finding.PublicationRef != nil ||
		finding.ResultCompleteness != analytics.CompletenessPartial ||
		snapshot.Facts.ReviewRuns[0].Funnel.Published != 0 ||
		len(snapshot.Facts.FeedbackOutcomes) != 0 ||
		snapshot.Facts.Completeness != analytics.CompletenessPartial {
		t.Fatalf("publication source absence was overclaimed: %+v", snapshot.Facts)
	}
	if snapshot.Coverage[5].Source != CoveragePublication ||
		snapshot.Coverage[5].Completeness != analytics.CompletenessUnknown ||
		!slices.Contains(
			snapshot.Coverage[5].IncompleteReasons,
			"publication_repository_not_configured",
		) {
		t.Fatalf("publication source coverage = %+v", snapshot.Coverage)
	}
}

func TestRebuildIsConcurrentIdempotentAndSnapshotIDConflictsFailClosed(t *testing.T) {
	store, runs, ledger, _, repositoryID := analyticsFixture(t, t.TempDir())
	adapter, err := New(runs, ledger, store, &staticEvaluationSource{batch: EvaluationBatch{
		Completeness:       analytics.CompletenessComplete,
		IncompleteReasons:  []string{},
		Facts:              []analytics.ExperimentFact{},
		RepeatabilityFacts: []analytics.RepeatabilityFact{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := fixtureRebuildRequest("snapshot-concurrent", repositoryID)
	var wait sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, rebuildErr := adapter.Rebuild(context.Background(), request)
			errors <- rebuildErr
		}()
	}
	wait.Wait()
	close(errors)
	for rebuildErr := range errors {
		if rebuildErr != nil {
			t.Fatalf("concurrent Rebuild() error = %v", rebuildErr)
		}
	}

	conflict := request
	conflict.BuiltAt = conflict.BuiltAt.Add(time.Second)
	if _, err := adapter.Rebuild(context.Background(), conflict); err == nil ||
		!strings.Contains(err.Error(), ErrProjectionConflict.Error()) {
		t.Fatalf("conflicting Rebuild() error = %v", err)
	}
}

func TestQueryLatestUsesImmutableManifests(t *testing.T) {
	store, runs, ledger, _, repositoryID := analyticsFixture(t, t.TempDir())
	adapter, err := New(runs, ledger, store, &staticEvaluationSource{batch: EvaluationBatch{
		Completeness:       analytics.CompletenessComplete,
		IncompleteReasons:  []string{},
		Facts:              []analytics.ExperimentFact{},
		RepeatabilityFacts: []analytics.RepeatabilityFact{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	first := fixtureRebuildRequest("snapshot-a", repositoryID)
	second := first
	second.SnapshotID = "snapshot-b"
	second.BuiltAt = first.BuiltAt.Add(time.Second)
	if _, err := adapter.Rebuild(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Rebuild(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	latest, err := adapter.QueryLatest(Selector{
		Scope: first.Scope, Window: first.Window, GroupBy: first.GroupBy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if latest.SnapshotID != second.SnapshotID {
		t.Fatalf("latest snapshot = %q, want %q", latest.SnapshotID, second.SnapshotID)
	}
}

func TestRebuildJoinsIndependentFeedbackAndOutcomeLedgers(t *testing.T) {
	store, runs, ledger, run, repositoryID := analyticsFixture(t, t.TempDir())
	if run.Report == nil || len(run.Report.Findings) != 1 {
		t.Fatalf("fixture report = %+v", run.Report)
	}
	findingID := run.Report.Findings[0].ID
	runID := run.Run.RunID
	feedbackAt := time.Date(2026, time.July, 27, 9, 30, 0, 0, time.UTC)
	feedbackFact := feedbackdomain.Feedback{
		SchemaVersion: feedbackdomain.FeedbackSchemaVersion,
		FeedbackID:    "feedback-1",
		FindingID:     findingID,
		RunID:         runID,
		Action:        feedbackdomain.FeedbackAccept,
		Actor: feedbackdomain.ActorRef{
			Kind: feedbackdomain.ActorHuman, ID: "reviewer-1",
		},
		Source: feedbackdomain.Source{
			Kind: feedbackdomain.SourceManual, ID: "analytics-test",
		},
		OccurredAt: feedbackAt,
		RecordedAt: feedbackAt,
		SourceRefs: []feedbackdomain.SourceRef{{
			Kind:      feedbackdomain.SourceRefManualObservation,
			Authority: "argus-test",
			ID:        "feedback-evidence-1",
		}},
		IdempotencyKey: "feedback-idempotency-1",
	}
	if _, err := ledger.AppendFeedback(context.Background(), feedbackFact); err != nil {
		t.Fatal(err)
	}
	outcomeAt := feedbackAt.Add(10 * time.Minute)
	outcomeFact := feedbackdomain.Outcome{
		SchemaVersion: feedbackdomain.OutcomeSchemaVersion,
		OutcomeID:     "outcome-1",
		FindingID:     findingID,
		RunID:         runID,
		State:         feedbackdomain.OutcomeFixed,
		Actor: feedbackdomain.ActorRef{
			Kind: feedbackdomain.ActorHuman, ID: "engineer-1",
		},
		Source: feedbackdomain.Source{
			Kind: feedbackdomain.SourceManual, ID: "analytics-test",
		},
		OccurredAt: outcomeAt,
		RecordedAt: outcomeAt,
		Window: feedbackdomain.AttributionWindow{
			Start: feedbackAt, End: outcomeAt,
		},
		SourceRefs: []feedbackdomain.SourceRef{{
			Kind:      feedbackdomain.SourceRefManualObservation,
			Authority: "argus-test",
			ID:        "outcome-evidence-1",
		}},
		IdempotencyKey: "outcome-idempotency-1",
	}
	if _, err := ledger.AppendOutcome(context.Background(), outcomeFact); err != nil {
		t.Fatal(err)
	}
	publications := publishedPublicationRepository(
		t,
		store,
		runs,
		run,
		feedbackAt.Add(-10*time.Minute),
	)
	adapter, err := New(runs, ledger, store, &staticEvaluationSource{batch: EvaluationBatch{
		Completeness:       analytics.CompletenessComplete,
		IncompleteReasons:  []string{},
		Facts:              []analytics.ExperimentFact{},
		RepeatabilityFacts: []analytics.RepeatabilityFact{},
	}}, publications)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Rebuild(
		context.Background(),
		fixtureRebuildRequest("snapshot-feedback", repositoryID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Facts.FeedbackOutcomes) != 1 {
		t.Fatalf("feedback outcome facts = %+v", snapshot.Facts.FeedbackOutcomes)
	}
	projection := snapshot.Facts.FeedbackOutcomes[0]
	if projection.Feedback != analytics.FeedbackAccepted ||
		projection.Outcome != analytics.OutcomeFixed ||
		projection.FeedbackRef == nil ||
		projection.OutcomeRef == nil ||
		projection.FeedbackRef.Kind != analytics.SourceFeedback ||
		projection.OutcomeRef.Kind != analytics.SourceOutcome {
		t.Fatalf("feedback/outcome projection = %+v", projection)
	}
	if snapshot.Facts.ReviewRuns[0].Funnel.Published != 1 ||
		snapshot.Facts.Findings[0].PublicationRef == nil ||
		snapshot.Facts.Findings[0].PublicationAt == nil {
		t.Fatalf("published provider evidence = %+v", snapshot.Facts)
	}
}

func TestPublicationObservationRequiresExactEvidenceAndRespectsWindow(
	t *testing.T,
) {
	store, runs, _, outcome, _ := analyticsFixture(t, t.TempDir())
	observedAt := time.Date(2026, time.July, 27, 9, 20, 0, 0, time.UTC)
	repository := publishedPublicationRepository(
		t,
		store,
		runs,
		outcome,
		observedAt,
	)
	finding := outcome.Report.Findings[0]
	decision := outcome.Report.Decisions[0]
	records, err := repository.ByFinding(outcome.Run.RunID, finding.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("publication records = %+v, %v", records, err)
	}
	adapter, err := New(runs, nil, store, nil, repository)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := adapter.loadFrozenRun(outcome.Run)
	if err != nil {
		t.Fatal(err)
	}
	events := records[0].Events
	if len(events) != 3 ||
		events[0].Type != publication.EventRequested ||
		events[1].Type != publication.EventDispatchStarted ||
		events[2].Type != publication.EventPublished {
		t.Fatalf("published event closure = %+v", events)
	}
	for _, test := range []struct {
		name string
		end  time.Time
		want analytics.PublicationState
	}{
		{
			name: "before request",
			end:  events[0].OccurredAt,
			want: analytics.PublicationNotReached,
		},
		{
			name: "requested only",
			end:  events[1].OccurredAt,
			want: analytics.PublicationRequested,
		},
		{
			name: "dispatching",
			end:  events[2].OccurredAt,
			want: analytics.PublicationDispatching,
		},
		{
			name: "published",
			end:  events[2].OccurredAt.Add(time.Nanosecond),
			want: analytics.PublicationPublished,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation, err := observePublication(
				records,
				frozen,
				finding,
				decision,
				test.end,
			)
			if err != nil {
				t.Fatal(err)
			}
			if observation.state != test.want {
				t.Fatalf("publication state = %q, want %q", observation.state, test.want)
			}
			if test.want == analytics.PublicationPublished &&
				(observation.ref == nil || observation.publishedAt == nil) {
				t.Fatalf("published observation lacks evidence: %+v", observation)
			}
		})
	}

	corrupt, err := repository.ByFinding(outcome.Run.RunID, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	corrupt[0].ProviderResult.CommentID = ""
	corrupt[0].Events[len(corrupt[0].Events)-1].ProviderResult.CommentID = ""
	if _, err := observePublication(
		corrupt,
		frozen,
		finding,
		decision,
		observedAt.Add(time.Second),
	); err == nil {
		t.Fatal("publication observation accepted missing provider comment evidence")
	}

	wrongDecision, err := repository.ByFinding(outcome.Run.RunID, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrongDecision[0].Request.DecisionID = "different-decision"
	if _, err := observePublication(
		wrongDecision,
		frozen,
		finding,
		decision,
		observedAt.Add(time.Second),
	); err == nil {
		t.Fatal("publication observation accepted a mismatched decision binding")
	}
}

type analyticsPublicationProvider struct {
	revalidation publication.Revalidation
	result       publication.ProviderResult
}

func (provider *analyticsPublicationProvider) Revalidate(
	context.Context,
	publication.Request,
) (publication.Revalidation, error) {
	return provider.revalidation, nil
}

func (provider *analyticsPublicationProvider) Publish(
	context.Context,
	publication.ProviderRequest,
) (publication.ProviderResult, error) {
	return provider.result, nil
}

func (provider *analyticsPublicationProvider) Lookup(
	context.Context,
	publication.ProviderRequest,
) (publication.ProviderResult, error) {
	return publication.ProviderResult{
		SchemaVersion:  publication.ProviderResultSchema,
		Status:         publication.ProviderResultNotFound,
		IdempotencyKey: provider.result.IdempotencyKey,
		ObservedAt:     provider.result.ObservedAt,
	}, nil
}

func publishedPublicationRepository(
	t *testing.T,
	store *local.Store,
	runs *runrepo.Repository,
	outcome application.RunOutcome,
	observedAt time.Time,
) *publication.Repository {
	t.Helper()
	if outcome.Report == nil || len(outcome.Report.Findings) != 1 ||
		len(outcome.Report.Decisions) != 1 ||
		outcome.Run.FindingSetRef == nil {
		t.Fatalf("publication fixture report = %+v", outcome.Report)
	}
	execution, err := runs.LoadExecutionSnapshot(outcome.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var target targetmodel.MaterializedTarget
	if err := runs.ReadJSONArtifact(execution.TargetSnapshotRef, &target); err != nil {
		t.Fatal(err)
	}
	finding := outcome.Report.Findings[0]
	decision := outcome.Report.Decisions[0]
	contentRef := func(ref runmodel.ArtifactRef) publication.ContentRef {
		return publication.ContentRef{
			URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
		}
	}
	request := publication.Request{
		SchemaVersion:         publication.RequestSchemaVersion,
		PublicationID:         "publication-" + finding.ID,
		IdempotencyKey:        "publication-key-" + finding.ID,
		GrantID:               "grant-" + finding.ID,
		GrantSHA256:           strings.Repeat("d", 64),
		TenantID:              "local",
		WorkspaceID:           "local",
		RunID:                 outcome.Run.RunID,
		RunKind:               publication.RunKindReview,
		RunStatus:             publication.RunStatusSucceeded,
		TargetMode:            publication.TargetModeDiff,
		Provider:              "local-git",
		RepositoryID:          target.Snapshot.Repository.RepositoryID,
		ChangeKind:            "pull_request",
		ChangeID:              "42",
		BaseRevision:          outcome.Run.BaseRevision,
		ExpectedHeadRevision:  outcome.Run.HeadRevision,
		TargetSnapshotRef:     contentRef(execution.TargetSnapshotRef),
		ConfigBundleRef:       contentRef(execution.ConfigBundleRef),
		FindingSourceRef:      contentRef(*outcome.Run.FindingSetRef),
		FindingSourceContract: publication.FindingSourceFindingSet,
		FindingID:             finding.ID,
		Fingerprint:           finding.Fingerprint,
		DecisionID:            decision.ID,
		TargetDigest:          finding.TargetDigest,
		Anchor: publication.StableAnchor{
			Path:         finding.Path,
			Side:         "head",
			StartLine:    finding.StartLine,
			EndLine:      finding.EndLine,
			TargetDigest: finding.TargetDigest,
		},
		Channel:                    "pull_request_inline",
		Message:                    "Published from the analytics projection fixture.",
		RemoteWrites:               "allow",
		ExpectedPermissionRevision: "permission-local-1",
		GrantExpiresAt:             observedAt.Add(time.Hour),
		CreatedAt:                  observedAt.Add(-time.Minute),
	}
	provider := &analyticsPublicationProvider{
		revalidation: publication.Revalidation{
			SchemaVersion:        publication.RevalidationSchemaVersion,
			ProviderBaseRevision: request.BaseRevision,
			ProviderHeadRevision: request.ExpectedHeadRevision,
			PermissionGranted:    true,
			PermissionRevision:   request.ExpectedPermissionRevision,
			ConfigBundleSHA256:   request.ConfigBundleRef.SHA256,
			FindingSourceSHA256:  request.FindingSourceRef.SHA256,
			FindingCurrent:       true,
			AnchorCurrent:        true,
			CheckedAt:            observedAt.Add(-2 * time.Second),
		},
		result: publication.ProviderResult{
			SchemaVersion:     publication.ProviderResultSchema,
			Status:            publication.ProviderResultPublished,
			IdempotencyKey:    request.IdempotencyKey,
			ProviderRequestID: "provider-request-" + finding.ID,
			CommentID:         "comment-" + finding.ID,
			ObservedAt:        observedAt,
		},
	}
	repository, err := publication.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	clockAt := observedAt.Add(-3 * time.Second)
	service, err := publication.NewService(
		repository,
		provider,
		analyticsPublicationAuthorizer{},
		func() time.Time {
			value := clockAt
			clockAt = clockAt.Add(time.Second)
			return value
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.Publish(context.Background(), request)
	if err != nil || record.State != publication.StatePublished {
		t.Fatalf("publish fixture = %+v, %v", record, err)
	}
	return repository
}

type staticEvaluationSource struct {
	mu    sync.Mutex
	calls int
	batch EvaluationBatch
}

func (source *staticEvaluationSource) EvaluationFacts(
	context.Context,
	Scope,
	analytics.TimeWindow,
) (EvaluationBatch, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	return source.batch, nil
}

type fixtureIDs struct {
	mu   sync.Mutex
	next int
}

func (ids *fixtureIDs) New(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return prefix + "-" + time.Date(
		2026, time.July, 27, 0, 0, ids.next, 0, time.UTC,
	).Format("150405"), nil
}

type fixtureClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fixtureClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(time.Millisecond)
	return clock.now
}

func analyticsFixture(
	t *testing.T,
	stateRoot string,
) (
	*local.Store,
	*runrepo.Repository,
	*feedbackdomain.Repository,
	application.RunOutcome,
	string,
) {
	t.Helper()
	store, err := local.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	clock := &fixtureClock{
		now: time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC),
	}
	service, err := application.NewService(source, runs, application.ServiceOptions{
		IDs: &fixtureIDs{}, Now: clock.Now, BuildIdentity: "analytics-test",
		DisableScheduling: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath, base, head := analyticsGitFixture(t)
	run, err := service.Review(context.Background(), application.ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := feedbackdomain.New(store)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := runs.LoadExecutionSnapshot(run.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var target targetmodel.MaterializedTarget
	if err := runs.ReadJSONArtifact(execution.TargetSnapshotRef, &target); err != nil {
		t.Fatal(err)
	}
	return store, runs, ledger, run, target.Snapshot.Repository.RepositoryID
}

func fixtureRebuildRequest(snapshotID, repositoryID string) RebuildRequest {
	return RebuildRequest{
		SnapshotID: snapshotID,
		Scope: Scope{
			TenantID: "local", OrganizationID: "local", RepositoryID: repositoryID,
		},
		Window: analytics.TimeWindow{
			StartInclusive: time.Date(2026, time.July, 27, 8, 0, 0, 0, time.UTC),
			EndExclusive:   time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC),
		},
		GroupBy: []analytics.DimensionName{},
		BuiltAt: time.Date(2026, time.July, 27, 11, 0, 0, 0, time.UTC),
	}
}

func analyticsGitFixture(t *testing.T) (string, string, string) {
	t.Helper()
	repository := t.TempDir()
	runGit(t, repository, "init", "-q", "-b", "main")
	runGit(t, repository, "config", "user.name", "Argus Analytics Test")
	runGit(t, repository, "config", "user.email", "argus@example.invalid")
	runGit(t, repository, "config", "commit.gpgsign", "false")
	writeTestFile(t, repository, "review.go", "package fixture\n\nfunc Review() {}\n")
	base := commitTestFile(t, repository, "base")
	writeTestFile(
		t,
		repository,
		"review.go",
		"package fixture\n\n// ARGUS_BUG: analytics fixture\nfunc Review() {}\n",
	)
	head := commitTestFile(t, repository, "head")
	return repository, base, head
}

func commitTestFile(t *testing.T, repository, message string) string {
	t.Helper()
	runGit(t, repository, "add", "--all")
	runGit(t, repository, "commit", "-q", "-m", message)
	return strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
}

func writeTestFile(t *testing.T, repository, path, content string) {
	t.Helper()
	fullPath := filepath.Join(repository, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", arguments...)
	command.Dir = repository
	command.Env = append(
		os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
