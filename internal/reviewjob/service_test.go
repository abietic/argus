package reviewjob

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/configdefaults"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
)

func TestSubmitFreezesConfigAndIsIdempotentAcrossLaterPublication(t *testing.T) {
	service, configs, _ := newJobService(t, ExecuteFunc(func(
		context.Context,
		Command,
	) (application.RunOutcome, error) {
		return application.RunOutcome{}, errors.New("worker is not started")
	}))
	request := testRequest(t)
	request.Contexts = []reviewcore.ContextBinding{}
	mutation := testMutation("submit-review-1", 10)
	first, err := service.Submit(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if first.Workload.State != scheduling.StatePending ||
		first.ConfigBundleRef.URI == "" || first.ConfigReceiptRef.URI == "" {
		t.Fatalf("first record = %+v", first)
	}

	publishConfig(t, configs, "runtime", "2", 20, 64)
	retried, err := service.Submit(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("idempotent Submit() after config drift error = %v", err)
	}
	if retried.JobID != first.JobID || retried.RunID != first.RunID ||
		retried.CommandRef != first.CommandRef ||
		retried.ConfigBundleRef != first.ConfigBundleRef {
		t.Fatalf("retry changed frozen identity: first=%+v retry=%+v", first, retried)
	}

	changed := request
	changed.HeadRevision = "different-head"
	if _, err := service.Submit(context.Background(), changed, mutation); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotent Submit() error = %v, want ErrConflict", err)
	}
}

func TestWorkerCompletesAcceptedJobWithoutRequestContext(t *testing.T) {
	var artifactRef runmodel.ArtifactRef
	var store *local.Store
	service, _, openedStore := newJobService(t, ExecuteFunc(func(
		_ context.Context,
		command Command,
	) (application.RunOutcome, error) {
		stored, err := store.PutArtifact([]byte("committed run placeholder"))
		if err != nil {
			return application.RunOutcome{}, err
		}
		artifactRef = runmodel.ArtifactRef{
			URI: stored.URI, SHA256: stored.SHA256, SizeBytes: stored.SizeBytes,
			Contract: runmodel.ContractReviewRun,
		}
		return application.RunOutcome{
			Run:      runmodel.ReviewRun{RunID: command.RunID, Status: runmodel.RunStatusSucceeded},
			FinalRef: artifactRef,
		}, nil
	}))
	store = openedStore
	workerContext, stop := context.WithCancel(context.Background())
	defer stop()
	if err := service.Start(workerContext); err != nil {
		t.Fatal(err)
	}
	requestContext, cancelRequest := context.WithCancel(context.Background())
	record, err := service.Submit(requestContext, testRequest(t), testMutation("async-review", 30))
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	completed := awaitState(t, service, record.JobID, scheduling.StateSucceeded)
	if completed.Workload.Terminal == nil ||
		len(completed.Workload.Terminal.OutputRefs) != 1 ||
		completed.Workload.Terminal.OutputRefs[0] != artifactRef.URI {
		t.Fatalf("completed workload = %+v", completed.Workload)
	}
	timeline, err := service.Timeline(record.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) < 3 || timeline[0].Type != "submitted" ||
		timeline[1].Type != "claimed" ||
		timeline[len(timeline)-1].Type != "callback_accepted" {
		t.Fatalf("job timeline = %+v", timeline)
	}
}

func TestExplicitCancelFencesAndCancelsRunningJob(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan error, 1)
	service, _, _ := newJobService(t, ExecuteFunc(func(
		ctx context.Context,
		_ Command,
	) (application.RunOutcome, error) {
		close(started)
		<-ctx.Done()
		exited <- ctx.Err()
		return application.RunOutcome{}, ctx.Err()
	}))
	workerContext, stop := context.WithCancel(context.Background())
	defer stop()
	if err := service.Start(workerContext); err != nil {
		t.Fatal(err)
	}
	mutation := testMutation("cancelable-review", 40)
	record, err := service.Submit(context.Background(), testRequest(t), mutation)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	canceled, err := service.Cancel(context.Background(), record.JobID, CancelCommand{
		Reason: "operator canceled test job",
		Mutation: Mutation{
			IdempotencyKey: "cancel-review-job", Actor: mutation.Actor,
			Audit: "cancel test review job", At: time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Workload.State != scheduling.StateCanceled || !canceled.Workload.RunCanceled {
		t.Fatalf("canceled workload = %+v", canceled.Workload)
	}
	select {
	case err := <-exited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("executor exit = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not observe explicit cancellation")
	}
}

func TestRecoveredJobDoesNotDuplicateAnOrphanedNonterminalRun(t *testing.T) {
	var executions atomic.Int32
	service, _, _ := newJobService(t, ExecuteFunc(func(
		context.Context,
		Command,
	) (application.RunOutcome, error) {
		executions.Add(1)
		return application.RunOutcome{}, nil
	}))
	record, err := service.Submit(
		context.Background(), testRequest(t), testMutation("orphaned-review", 50),
	)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	if err := service.runs.AppendEvent(record.RunID+"-created", createdAt, runrepo.RunEvent{
		RunID: record.RunID, Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusPending, EventType: runrepo.EventRunCreated,
	}); err != nil {
		t.Fatal(err)
	}
	workerContext, stop := context.WithCancel(context.Background())
	defer stop()
	if err := service.Start(workerContext); err != nil {
		t.Fatal(err)
	}
	failed := awaitState(t, service, record.JobID, scheduling.StateFailed)
	if executions.Load() != 0 || failed.Workload.StateReason != "orphaned_nonterminal_run" {
		t.Fatalf("executor calls=%d workload=%+v", executions.Load(), failed.Workload)
	}
}

func TestWorkerRecoversIntentPersistedBeforeSchedulingFailure(t *testing.T) {
	var store *local.Store
	service, _, openedStore := newJobService(t, ExecuteFunc(func(
		_ context.Context,
		command Command,
	) (application.RunOutcome, error) {
		stored, err := store.PutArtifact([]byte("recovered result"))
		if err != nil {
			return application.RunOutcome{}, err
		}
		return application.RunOutcome{
			Run: runmodel.ReviewRun{RunID: command.RunID, Status: runmodel.RunStatusSucceeded},
			FinalRef: runmodel.ArtifactRef{
				URI: stored.URI, SHA256: stored.SHA256, SizeBytes: stored.SizeBytes,
				Contract: runmodel.ContractReviewRun,
			},
		}, nil
	}))
	store = openedStore
	base := service.scheduler
	flaky := &failFirstSubmitScheduler{Scheduler: base}
	service.scheduler = flaky
	mutation := testMutation("intent-before-schedule", 60)
	_, err := service.Submit(context.Background(), testRequest(t), mutation)
	if err == nil || !errors.Is(err, errInjectedSchedulingFailure) {
		t.Fatalf("first Submit() error = %v", err)
	}
	jobID, _ := deterministicIDs(mutation.Actor, mutation.IdempotencyKey)
	if _, err := service.repository.Get(jobID); err != nil {
		t.Fatalf("durable intent missing after scheduling failure: %v", err)
	}
	workerContext, stop := context.WithCancel(context.Background())
	defer stop()
	if err := service.Start(workerContext); err != nil {
		t.Fatal(err)
	}
	awaitState(t, service, jobID, scheduling.StateSucceeded)
}

func TestStartJobsBoundsDispatchAndReconciliationAndWaitsForExecution(t *testing.T) {
	started := make(chan string, 2)
	canceled := make(chan struct{})
	release := make(chan struct{})
	service, _, _ := newJobService(t, ExecuteFunc(func(ctx context.Context, command Command) (application.RunOutcome, error) {
		started <- command.JobID
		<-ctx.Done()
		close(canceled)
		<-release
		return application.RunOutcome{}, ctx.Err()
	}))
	oldMutation := testMutation("unrelated-expired-review", 0)
	oldMutation.At = time.Now().UTC().Add(-time.Hour)
	unrelated, err := service.Submit(context.Background(), testRequest(t), oldMutation)
	if err != nil {
		t.Fatal(err)
	}
	pendingMutation := testMutation("unrelated-pending-review", 0)
	pendingMutation.At = time.Now().UTC().Add(-time.Second)
	pending, err := service.Submit(context.Background(), testRequest(t), pendingMutation)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := service.Submit(context.Background(), testRequest(t), testMutation("selected-review", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.StartJobs(ctx, []string{selected.JobID}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		if id != selected.JobID {
			t.Fatalf("executed unrelated job %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("selected job did not start")
	}
	other, err := service.Get(unrelated.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Workload.State != scheduling.StatePending || other.Workload.Generation != 0 {
		t.Fatalf("bounded worker mutated unrelated expired workload: %+v", other.Workload)
	}
	otherPending, err := service.Get(pending.JobID)
	if err != nil || otherPending.Workload.State != scheduling.StatePending || otherPending.Workload.Generation != 0 {
		t.Fatalf("bounded worker mutated unrelated pending workload: %+v %v", otherPending.Workload, err)
	}
	if err := service.Start(ctx); err == nil {
		t.Fatal("coordinator started twice")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not observe coordinator cancellation")
	}
	waited := make(chan struct{})
	go func() { service.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned before executor finished its terminal writes")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not drain completed executor")
	}
	other, err = service.Get(unrelated.JobID)
	if err != nil || other.Workload.State != scheduling.StatePending {
		t.Fatalf("shutdown mutated unrelated job: %+v %v", other, err)
	}
}

func TestStartJobsRejectsInvalidOrUnknownScopeBeforeStarting(t *testing.T) {
	service, _, _ := newJobService(t, ExecuteFunc(func(context.Context, Command) (application.RunOutcome, error) {
		return application.RunOutcome{}, nil
	}))
	record, err := service.Submit(context.Background(), testRequest(t), testMutation("scoped-validation", 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]string{nil, {}, {""}, {"job-missing"}, {record.JobID, record.JobID}} {
		if err := service.StartJobs(context.Background(), ids); err == nil {
			t.Fatalf("invalid scope accepted: %v", ids)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.StartJobs(ctx, []string{record.JobID}); err != nil {
		t.Fatal(err)
	}
	cancel()
	service.Wait()
}

func newJobService(
	t *testing.T,
	executor Executor,
) (*Service, *configrepo.Repository, *local.Store) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configStore, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configs, err := configrepo.New(configStore)
	if err != nil {
		t.Fatal(err)
	}
	publishConfig(t, configs, "runtime", "1", 0, 32)
	jobs, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(jobs, scheduler, runs, configs, executor, "review-job-test-worker", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return service, configs, store
}

func publishConfig(
	t *testing.T,
	repository *configrepo.Repository,
	id string,
	revision string,
	offset int,
	maxFiles int,
) {
	t.Helper()
	config := application.DefaultLocalConfig()
	value, err := configdefaults.Revision(configdefaults.Options{
		ID: id, Revision: revision, MaxFiles: maxFiles,
		MaxPatchBytes:  config.MaxPatchBytes,
		MaxInputBytes:  config.MaxMaterializedBytes,
		MaxOutputBytes: workflow.DefaultReviewDefinition().Stages[0].Budget.MaxOutputBytes,
		MaxAttempts:    config.MaxAttempts,
		AllowedModes:   []string{"diff", "scope", "selection"},
		TargetInclude:  []string{"**"}, TargetExclude: []string{},
	}, workflow.DefaultReviewDefinition())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour + time.Duration(offset)*time.Second)
	mutation := func(action string, delta time.Duration) configrepo.Mutation {
		return configrepo.Mutation{
			IdempotencyKey: action + "-" + id + "-" + revision,
			Actor:          "config-owner", Audit: action + " test config",
			At: base.Add(delta),
		}
	}
	if _, err := repository.Create(context.Background(), value, mutation("create", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ValidateRevision(context.Background(), id, revision, mutation("validate", time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Publish(
		context.Background(), id, revision, configrepo.Rollout{Percentage: 100},
		mutation("publish", 2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
}

func testRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		SchemaVersion: RequestSchemaVersion, ExecutionProfile: DeterministicExecutionProfile,
		RepositoryPath: t.TempDir(), Mode: "diff",
		BaseRevision: "base", HeadRevision: "head",
		SelectionRanges: nil, Include: nil, Exclude: nil,
		ExecutionTimeoutSeconds: 1800,
	}
}

func testMutation(key string, offset int) Mutation {
	return Mutation{
		IdempotencyKey: key, Actor: "local-review-operator",
		Audit: "submit test review", At: time.Now().UTC().Add(time.Duration(offset) * time.Millisecond),
	}
}

func awaitState(
	t *testing.T,
	service *Service,
	jobID string,
	want scheduling.WorkloadState,
) Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, err := service.Get(jobID)
		if err == nil && record.Workload.State == want {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, err := service.Get(jobID)
	t.Fatalf("job state = %+v, error=%v; want %s", record.Workload, err, want)
	return Record{}
}

var errInjectedSchedulingFailure = errors.New("injected scheduling failure")

type failFirstSubmitScheduler struct {
	Scheduler
	failed atomic.Bool
}

func (scheduler *failFirstSubmitScheduler) Submit(
	ctx context.Context,
	spec scheduling.WorkloadSpec,
	mutation scheduling.Mutation,
) (scheduling.WorkloadRecord, error) {
	if scheduler.failed.CompareAndSwap(false, true) {
		return scheduling.WorkloadRecord{}, errInjectedSchedulingFailure
	}
	return scheduler.Scheduler.Submit(ctx, spec, mutation)
}
