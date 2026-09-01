package reviewshard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
)

func TestScopeExecutorPlansParallelCheckpointsAndFansInFullTarget(t *testing.T) {
	store, runs := newShardTestStore(t)
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-scope-executor"
	files := make([]shardTestFile, 65)
	for index := range files {
		files[index] = shardTestFile{
			path: fmt.Sprintf("file-%03d.go", index), content: "package p\n// TODO shard\n",
		}
	}
	input := shardTestInput(t, files...)
	inputRef := putShardInput(t, runs, input)
	dispatch := shardDispatch(runID, inputRef.URI, shardEpoch)
	clock := &shardTestClock{next: shardEpoch.Add(2 * time.Second)}
	executor, err := NewScopeExecutor(repository, runs, dispatch, "scope-worker", clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := executor.PlanScope(context.Background(), application.ScopeShardPlanRequest{
		RunID: runID, Input: input, InputRef: inputRef,
		MaxInputBytes: 1 << 20, CreatedAt: shardEpoch.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestRef := plan.ManifestRef
	materialized, err := reviewcore.ExecuteStageWithPolicy(
		context.Background(), input, reviewcore.StageMaterializeTarget,
		reviewcore.StageResult{}, reviewcore.DefaultRuntimePolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := reviewcore.ExecuteStageWithPolicy(
		context.Background(), input, reviewcore.StagePlanContext,
		materialized, reviewcore.DefaultRuntimePolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	detected, err := executor.DetectScope(context.Background(), application.ScopeShardDetectRequest{
		RunID: runID, Input: input, InputRef: inputRef, ManifestRef: manifestRef,
		Upstream: planned, Policy: reviewcore.DefaultRuntimePolicy(),
		MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxConcurrency: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if detected.Stage != reviewcore.StageDetect || detected.TargetDigest != inputRef.SHA256 ||
		detected.InputDigest != planned.ArtifactDigest || len(detected.Output.CandidateFindings) != 65 {
		t.Fatalf("fan-in result = %+v", detected)
	}
	for _, candidate := range detected.Output.CandidateFindings {
		if candidate.TargetDigest != inputRef.SHA256 {
			t.Fatalf("candidate retained shard-local target: %+v", candidate)
		}
	}
	record, err := repository.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Aggregate == nil || len(record.Completed) != len(record.Manifest.Shards) ||
		len(record.PendingShardIDs) != 0 || len(record.Manifest.Shards) != 3 {
		t.Fatalf("durable scope record = %+v", record)
	}
	secondDispatch := shardDispatch(runID, inputRef.URI, shardEpoch.Add(10*time.Second))
	secondDispatch.Lease.LeaseID = "lease-scope-2"
	secondDispatch.Lease.WorkerID = "scope-worker-2"
	secondDispatch.Lease.Attempt = 2
	secondDispatch.Lease.Generation = 2
	secondDispatch.Lease.FencingToken = 2
	restarted, err := NewScopeExecutor(repository, runs, secondDispatch, "scope-worker-2", clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.DetectScope(context.Background(), application.ScopeShardDetectRequest{
		RunID: runID, Input: input, InputRef: inputRef, ManifestRef: manifestRef,
		Upstream: planned, Policy: reviewcore.DefaultRuntimePolicy(),
		MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxConcurrency: 3,
	})
	if err != nil || replayed.ArtifactDigest != detected.ArtifactDigest {
		t.Fatalf("aggregate crash-window replay = %+v error=%v", replayed, err)
	}
}

func TestPlanRecordsInputArtifactOverheadAsExplicitGap(t *testing.T) {
	_, runs := newShardTestStore(t)
	input := shardTestInput(t,
		shardTestFile{path: "a.go", content: "package p\n"},
	)
	inputRef := putShardInput(t, runs, input)
	manifest, err := Plan(context.Background(), "run-input-overhead", input, inputRef, Limits{
		MaxFilesPerShard: 1, MaxBytesPerShard: 1 << 20,
		MaxInputBytesPerShard: 1, MaxShards: 2,
	}, shardEpoch, runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Shards) != 0 || len(manifest.Gaps) != 1 ||
		manifest.Gaps[0].ReasonCode != "shard_input_bytes_exceeded" {
		t.Fatalf("overhead coverage = %+v", manifest)
	}
}

func TestScopeReviewRecoversOnlyMissingShardsAndFencesAbandonedGeneration(t *testing.T) {
	repositoryPath, revision := scopeRecoveryGitFixture(t, 65)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	shards, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-scope-physical-recovery"
	firstDispatch := shardDispatch(runID, "artifact://local/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", shardEpoch)
	blocked := make(chan struct{}, 4)
	release := make(chan struct{})
	var detectorCalls atomic.Int32
	firstDetector := DetectShardFunc(func(
		ctx context.Context,
		input reviewcore.ReviewInput,
		policy reviewcore.RuntimePolicy,
	) (reviewcore.StageResult, error) {
		if detectorCalls.Add(1) == 1 {
			return DetectShardWithCore(ctx, input, policy)
		}
		blocked <- struct{}{}
		<-release
		return DetectShardWithCore(ctx, input, policy)
	})
	firstExecutor, err := NewScopeExecutorWithDetector(
		shards, runs, firstDispatch, "scope-worker-1", time.Now, firstDetector,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstService, err := application.NewService(source, runs, application.ServiceOptions{
		IDs:           &scopeRecoveryIDs{runID: runID},
		BuildIdentity: "argus-scope-recovery-test", DisableScheduling: true,
		ExecutionDispatch: &firstDispatch, ScopeShards: firstExecutor,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstService.Review(context.Background(), application.ReviewRequest{
			RepositoryPath: repositoryPath, Mode: reviewcore.TargetModeScope,
			Revision: revision, Include: []string{"pkg/**"}, Exclude: []string{},
		})
		firstDone <- err
	}()
	for index := 0; index < 1; index++ {
		select {
		case <-blocked:
		case err := <-firstDone:
			t.Fatalf("generation 1 exited before blocking shards: %v", err)
		case <-time.After(30 * time.Second):
			record, recordErr := shards.Get(runID)
			t.Fatalf("generation 1 did not enter blocked shard detectors: record=%+v error=%v", record, recordErr)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		record, getErr := shards.Get(runID)
		if getErr == nil && len(record.Completed) == 1 && len(record.PendingShardIDs) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("generation 1 did not persist one checkpoint: record=%+v error=%v", record, getErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot, err := runs.ExecutionSnapshotForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := runs.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		t.Fatal(err)
	}

	secondDispatch := shardDispatch(runID, firstDispatch.Spec.InputRef, shardEpoch.Add(10*time.Second))
	secondDispatch.Lease.LeaseID = "lease-scope-2"
	secondDispatch.Lease.WorkerID = "scope-worker-2"
	secondDispatch.Lease.Attempt = 2
	secondDispatch.Lease.Generation = 2
	secondDispatch.Lease.FencingToken = 2
	secondExecutor, err := NewScopeExecutor(shards, runs, secondDispatch, "scope-worker-2", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	secondService, err := application.NewService(source, runs, application.ServiceOptions{
		ConfigBundle: bundle, IDs: &scopeRecoveryIDs{runID: runID},
		BuildIdentity: "argus-scope-recovery-test", DisableScheduling: true,
		ExecutionDispatch: &secondDispatch, ScopeShards: secondExecutor,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := secondService.ResumeScopeReview(context.Background(), runID, repositoryPath)
	if err != nil {
		close(release)
		t.Fatalf("ResumeScopeReview() error = %v", err)
	}
	if outcome.Run.Status != runmodel.RunStatusSucceeded || outcome.Report == nil ||
		outcome.Report.Summary.Findings != 65 {
		close(release)
		t.Fatalf("recovered outcome = %+v", outcome)
	}
	record, err := shards.Get(runID)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	generationCounts := map[int]int{}
	for _, checkpoint := range record.Completed {
		generationCounts[checkpoint.Generation]++
	}
	if record.Aggregate == nil || len(record.Completed) != 3 ||
		generationCounts[1] != 1 || generationCounts[2] != 2 {
		close(release)
		t.Fatalf("mixed-generation recovery checkpoints = %+v", record.Completed)
	}
	close(release)
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("abandoned generation unexpectedly committed after generation 2")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("abandoned generation did not exit after release")
	}
	committed, err := runs.LoadRun(runID)
	if err != nil || committed.Status != runmodel.RunStatusSucceeded {
		t.Fatalf("generation 2 terminal authority = %+v error=%v", committed, err)
	}
}

type shardTestClock struct {
	mu   sync.Mutex
	next time.Time
}

type scopeRecoveryIDs struct {
	mu       sync.Mutex
	runID    string
	sequence int
}

func (ids *scopeRecoveryIDs) New(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if prefix == "run" {
		return ids.runID, nil
	}
	ids.sequence++
	return fmt.Sprintf("%s-recovery-%04d", prefix, ids.sequence), nil
}

func scopeRecoveryGitFixture(t *testing.T, files int) (string, string) {
	t.Helper()
	repository := t.TempDir()
	for _, arguments := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Argus Test"},
		{"config", "user.email", "argus@example.invalid"},
		{"config", "commit.gpgsign", "false"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	for index := 0; index < files; index++ {
		path := filepath.Join(repository, "pkg", fmt.Sprintf("file-%03d.go", index))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package pkg\n// TODO recover\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, arguments := range [][]string{{"add", "."}, {"commit", "-q", "-m", "scope recovery"}} {
		command := exec.Command("git", arguments...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = repository
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return repository, strings.TrimSpace(string(output))
}

func (clock *shardTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	result := clock.next
	clock.next = clock.next.Add(time.Millisecond)
	return result
}

func shardDispatch(runID string, inputURI string, acquiredAt time.Time) scheduling.Dispatch {
	return scheduling.Dispatch{
		Spec: scheduling.WorkloadSpec{
			SchemaVersion: scheduling.WorkloadSchemaVersion,
			WorkloadID:    runID + "-workload", RunID: runID, TenantID: "tenant-local",
			Class: scheduling.ClassFullScan, InputRef: inputURI,
			SubmittedAt:       acquiredAt.Add(-time.Second),
			ExecutionDeadline: acquiredAt.Add(time.Hour),
		},
		Lease: scheduling.DispatchLease{
			LeaseID: "lease-scope-1", WorkloadID: runID + "-workload", WorkerID: "scope-worker",
			Attempt: 1, Generation: 1, FencingToken: 1,
			AcquiredAt: acquiredAt, LastHeartbeat: acquiredAt,
			ExpiresAt: acquiredAt.Add(time.Minute),
		},
	}
}
