package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/configdefaults"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/contextprovider"
	"github.com/abietic/argus/internal/identity"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/reviewshard"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
)

const physicalScopeReviewJobHelperEnvironment = "ARGUS_PHYSICAL_SCOPE_REVIEW_JOB_HELPER"

type physicalScopeReviewJobFixture struct {
	storePath      string
	configPath     string
	repositoryPath string
	revision       string
	service        *reviewjob.Service
	scheduler      *scheduling.Repository
	runs           *runrepo.Repository
	shards         *reviewshard.Repository
	job            reviewjob.Record
}

func TestDeterministicScopeReviewJobRecoversAfterPhysicalWorkerProcessKill(t *testing.T) {
	if mode := os.Getenv(physicalScopeReviewJobHelperEnvironment); mode != "" {
		runPhysicalScopeReviewJobHelper(t, mode)
		return
	}
	fixture := preparePhysicalScopeReviewJob(t)
	marker := filepath.Join(t.TempDir(), "scope-shards-blocked")
	first := physicalScopeReviewJobCommand(t, "block", fixture, marker)
	var firstOutput bytes.Buffer
	first.Stdout = &firstOutput
	first.Stderr = &firstOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitForRegularFile(marker, 20*time.Second); err != nil {
		_ = first.Process.Kill()
		_, _ = first.Process.Wait()
		t.Fatalf("generation 1 never blocked inside shard detector: %v\n%s", err, firstOutput.String())
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		record, err := fixture.shards.Get(fixture.job.RunID)
		if err == nil && len(record.Completed) == 1 && len(record.PendingShardIDs) == 2 {
			break
		}
		if time.Now().After(deadline) {
			_ = first.Process.Kill()
			_, _ = first.Process.Wait()
			t.Fatalf("generation 1 did not persist exactly one shard: record=%+v error=%v\n%s", record, err, firstOutput.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	beforeKill, err := fixture.service.Get(fixture.job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeKill.Workload.State != scheduling.StateLeased ||
		beforeKill.Workload.Generation != 1 || beforeKill.Workload.ActiveLease == nil ||
		beforeKill.Run != nil {
		t.Fatalf("pre-kill deterministic scope authority = %+v", beforeKill)
	}
	firstLease := *beforeKill.Workload.ActiveLease
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	state, err := first.Process.Wait()
	if err != nil || state.Success() {
		t.Fatalf("physical worker kill state=%v error=%v", state, err)
	}
	if _, err := fixture.scheduler.Reconcile(t.Context(), scheduling.Mutation{
		IdempotencyKey: "physical-scope-expire-generation-1",
		Actor:          "physical-scope-test", Audit: "expire physically killed deterministic scope worker",
		At: firstLease.ExpiresAt,
	}); err != nil {
		t.Fatalf("reconcile killed scope worker: %v", err)
	}

	second := physicalScopeReviewJobCommand(t, "resume", fixture, "")
	secondOutput, err := second.CombinedOutput()
	if err != nil {
		t.Fatalf("generation 2 scope worker failed: %v\n%s", err, secondOutput)
	}
	terminal, err := fixture.service.Get(fixture.job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Workload.State != scheduling.StateSucceeded || terminal.Workload.Generation != 2 ||
		terminal.Run == nil || terminal.Run.Status != runmodel.RunStatusSucceeded {
		t.Fatalf("generation 2 did not commit deterministic scope success: %+v", terminal)
	}
	committed, err := fixture.runs.LoadCommittedRunResult(fixture.job.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Report == nil || committed.Report.Summary.Findings != 65 ||
		committed.Report.Summary.Verified != 65 {
		t.Fatalf("recovered deterministic report = %+v", committed.Report)
	}
	shardRecord, err := fixture.shards.Get(fixture.job.RunID)
	if err != nil {
		t.Fatal(err)
	}
	generationCounts := map[int]int{}
	for _, checkpoint := range shardRecord.Completed {
		generationCounts[checkpoint.Generation]++
	}
	if shardRecord.Aggregate == nil || len(shardRecord.Completed) != 3 ||
		generationCounts[1] != 1 || generationCounts[2] != 2 {
		t.Fatalf("physical recovered shard provenance = %+v", shardRecord.Completed)
	}

	late, err := fixture.scheduler.Complete(t.Context(), scheduling.Callback{
		SchemaVersion:  scheduling.CallbackSchemaVersion,
		IdempotencyKey: "physical-scope-stale-generation-1-callback",
		LeaseID:        firstLease.LeaseID, WorkloadID: firstLease.WorkloadID,
		WorkerID: firstLease.WorkerID, Attempt: firstLease.Attempt,
		Generation: firstLease.Generation, FencingToken: firstLease.FencingToken,
		Status: scheduling.CallbackSucceeded,
		OutputRefs: []string{
			"artifact://local/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		OccurredAt: time.Now().UTC(),
	})
	if !errors.Is(err, scheduling.ErrFenced) || late.State != scheduling.StateSucceeded ||
		late.Generation != 2 {
		t.Fatalf("stale generation callback changed terminal scope authority: record=%+v error=%v", late, err)
	}
}

func preparePhysicalScopeReviewJob(t *testing.T) physicalScopeReviewJobFixture {
	t.Helper()
	repositoryPath := newCLITargetRepository(t)
	for index := 0; index < 65; index++ {
		writeCLITargetFile(
			t, repositoryPath, fmt.Sprintf("pkg/file-%03d.go", index),
			"package pkg\n// TODO physical scope recovery\n",
		)
	}
	revision := commitCLITarget(t, repositoryPath, "physical deterministic scope recovery")
	storePath := t.TempDir()
	configPath := t.TempDir()
	service, scheduler, runs, shards, err := newPhysicalScopeReviewJobService(
		storePath, configPath, "physical-scope-parent", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	publishPhysicalScopeConfig(t, configPath)
	job, err := service.Submit(t.Context(), reviewjob.Request{
		SchemaVersion:    reviewjob.RequestSchemaVersion,
		ExecutionProfile: reviewjob.DeterministicExecutionProfile,
		RepositoryPath:   repositoryPath, Mode: string(reviewcore.TargetModeScope),
		Revision: revision, Include: []string{"pkg/**"}, Exclude: []string{},
		SelectionRanges: nil, ExecutionTimeoutSeconds: 60,
	}, reviewjob.Mutation{
		IdempotencyKey: "physical-deterministic-scope-job",
		Actor:          "physical-scope-operator", Audit: "prove physical deterministic scope recovery",
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return physicalScopeReviewJobFixture{
		storePath: storePath, configPath: configPath,
		repositoryPath: repositoryPath, revision: revision,
		service: service, scheduler: scheduler, runs: runs, shards: shards, job: job,
	}
}

func newPhysicalScopeReviewJobService(
	storePath string,
	configPath string,
	workerID string,
	detector reviewshard.ShardDetector,
) (*reviewjob.Service, *scheduling.Repository, *runrepo.Repository, *reviewshard.Repository, error) {
	store, err := local.Open(storePath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	configStore, err := local.Open(configPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	configs, err := configrepo.New(configStore)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	jobs, err := reviewjob.NewRepository(store)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	scheduler, err := scheduling.NewRepository(store, physicalSchedulingPolicy())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	shards, err := reviewshard.NewRepository(store)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	source, err := gitadapter.New()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	contexts, err := contextprovider.NewLocalExecutor(source)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	claimed := func(
		ctx context.Context,
		command reviewjob.Command,
		dispatch scheduling.Dispatch,
	) (application.RunOutcome, error) {
		ids := &reviewJobIDGenerator{runID: command.RunID, fallback: identity.NewGenerator()}
		var scopeExecutor *reviewshard.ScopeExecutor
		var err error
		if detector == nil {
			scopeExecutor, err = reviewshard.NewScopeExecutor(
				shards, runs, dispatch, workerID, time.Now,
			)
		} else {
			scopeExecutor, err = reviewshard.NewScopeExecutorWithDetector(
				shards, runs, dispatch, workerID, time.Now, detector,
			)
		}
		if err != nil {
			return application.RunOutcome{}, err
		}
		service, err := application.NewService(source, runs, application.ServiceOptions{
			ConfigBundle: command.ConfigBundle, IDs: ids,
			BuildIdentity: "argus-physical-scope", DisableScheduling: true,
			ContextProviders: contexts, ScopeShards: scopeExecutor, ExecutionDispatch: &dispatch,
		})
		if err != nil {
			return application.RunOutcome{}, err
		}
		history, err := runs.History(0)
		if err != nil {
			return application.RunOutcome{}, err
		}
		for _, entry := range history {
			if entry.RunID == command.RunID {
				return service.ResumeScopeReview(ctx, command.RunID, command.Request.RepositoryPath)
			}
		}
		return service.Review(ctx, command.Request.ApplicationRequest())
	}
	executor := &localReviewJobExecutor{
		deterministic: reviewjob.ExecuteFunc(func(context.Context, reviewjob.Command) (application.RunOutcome, error) {
			return application.RunOutcome{}, errors.New("physical scope executor requires a claimed dispatch")
		}),
		deterministicClaimed: claimed,
	}
	service, err := reviewjob.NewService(
		jobs, scheduler, runs, configs, executor, workerID, time.Now,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return service, scheduler, runs, shards, nil
}

func publishPhysicalScopeConfig(t *testing.T, configPath string) {
	t.Helper()
	store, err := local.Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := configrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	config := application.DefaultLocalConfig()
	revision, err := configdefaults.Revision(configdefaults.Options{
		ID: "physical-scope", Revision: "1", MaxFiles: 1000,
		MaxPatchBytes: config.MaxPatchBytes, MaxInputBytes: config.MaxMaterializedBytes,
		MaxOutputBytes: workflow.DefaultReviewDefinition().Stages[0].Budget.MaxOutputBytes,
		MaxAttempts:    config.MaxAttempts,
		AllowedModes:   []string{"diff", "scope", "selection"},
		TargetInclude:  []string{"**"}, TargetExclude: []string{},
	}, workflow.DefaultReviewDefinition())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	mutation := func(action string, offset time.Duration) configrepo.Mutation {
		return configrepo.Mutation{
			IdempotencyKey: "physical-scope-config-" + action,
			Actor:          "physical-scope-config-owner", Audit: action + " physical scope config",
			At: base.Add(offset),
		}
	}
	if _, err := repository.Create(t.Context(), revision, mutation("create", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ValidateRevision(
		t.Context(), revision.ID, revision.Revision, mutation("validate", time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Publish(
		t.Context(), revision.ID, revision.Revision, configrepo.Rollout{Percentage: 100},
		mutation("publish", 2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
}

func physicalScopeReviewJobCommand(
	t *testing.T,
	mode string,
	fixture physicalScopeReviewJobFixture,
	marker string,
) *exec.Cmd {
	t.Helper()
	command := exec.Command(
		os.Args[0], "-test.run=^TestDeterministicScopeReviewJobRecoversAfterPhysicalWorkerProcessKill$",
	)
	command.Env = append(os.Environ(),
		physicalScopeReviewJobHelperEnvironment+"="+mode,
		"ARGUS_PHYSICAL_SCOPE_STORE="+fixture.storePath,
		"ARGUS_PHYSICAL_SCOPE_CONFIG="+fixture.configPath,
		"ARGUS_PHYSICAL_SCOPE_MARKER="+marker,
	)
	return command
}

func runPhysicalScopeReviewJobHelper(t *testing.T, mode string) {
	var detector reviewshard.ShardDetector
	if mode == "block" {
		var calls atomic.Int32
		var markerOnce sync.Once
		marker := os.Getenv("ARGUS_PHYSICAL_SCOPE_MARKER")
		detector = reviewshard.DetectShardFunc(func(
			ctx context.Context,
			input reviewcore.ReviewInput,
			policy reviewcore.RuntimePolicy,
		) (reviewcore.StageResult, error) {
			if calls.Add(1) == 1 {
				return reviewshard.DetectShardWithCore(ctx, input, policy)
			}
			var markerErr error
			markerOnce.Do(func() {
				markerErr = os.WriteFile(marker, []byte("blocked\n"), 0o600)
			})
			if markerErr != nil {
				return reviewcore.StageResult{}, markerErr
			}
			<-ctx.Done()
			return reviewcore.StageResult{}, ctx.Err()
		})
	} else if mode != "resume" {
		t.Fatalf("unknown physical scope helper mode %q", mode)
	}
	service, _, _, _, err := newPhysicalScopeReviewJobService(
		os.Getenv("ARGUS_PHYSICAL_SCOPE_STORE"),
		os.Getenv("ARGUS_PHYSICAL_SCOPE_CONFIG"),
		"physical-scope-"+mode+"-worker", detector,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if mode == "block" {
		select {}
	}
	records, err := service.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("physical scope helper jobs = %d, want 1", len(records))
	}
	// The observer must not expire before the immutable job execution budget.
	// Loaded Linux CI runners can legitimately take longer than the local
	// 30-second lease to resume and aggregate all scope shards.
	observerBudget := time.Duration(records[0].Request.ExecutionTimeoutSeconds)*time.Second +
		30*time.Second
	deadline := time.Now().Add(observerBudget)
	for time.Now().Before(deadline) {
		records, err = service.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 && records[0].Workload.State == scheduling.StateSucceeded &&
			records[0].Run != nil && records[0].Run.Status == runmodel.RunStatusSucceeded {
			cancel()
			service.Wait()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf(
		"generation 2 deterministic scope worker did not reach succeeded terminal within %s",
		observerBudget,
	)
}
