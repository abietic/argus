package reviewshard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

var shardEpoch = time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)

func TestPlanFreezesDeterministicShardInputsAndExplicitGaps(t *testing.T) {
	store, runs := newShardTestStore(t)
	_ = store
	input := shardTestInput(t,
		shardTestFile{path: "a.go", content: "package p\n// TODO a\n"},
		shardTestFile{path: "b.go", content: "package p\n// TODO b\n"},
		shardTestFile{path: "c.go", content: "package p\n// TODO c\n"},
		shardTestFile{path: "missing.go", contentUnavailable: true},
	)
	inputRef := putShardInput(t, runs, input)
	limits := Limits{MaxFilesPerShard: 2, MaxBytesPerShard: 1 << 20, MaxInputBytesPerShard: 1 << 20, MaxShards: 1}
	first, err := Plan(context.Background(), "run-shard-plan", input, inputRef, limits, shardEpoch, runs)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	second, err := Plan(context.Background(), "run-shard-plan", input, inputRef, limits, shardEpoch, runs)
	if err != nil {
		t.Fatalf("Plan(retry) error = %v", err)
	}
	if !equalJSON(first, second) {
		t.Fatalf("same frozen input produced different manifests:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if len(first.Shards) != 1 || len(first.Shards[0].Files) != 2 ||
		first.Shards[0].Files[0].Path != "a.go" || first.Shards[0].Files[1].Path != "b.go" {
		t.Fatalf("planned executable shard = %+v", first.Shards)
	}
	if len(first.Gaps) != 2 || first.Gaps[0].Path != "c.go" ||
		first.Gaps[0].ReasonCode != "max_shards_exceeded" ||
		first.Gaps[1].Path != "missing.go" || first.Gaps[1].ReasonCode != "content_unavailable" {
		t.Fatalf("planned gaps = %+v", first.Gaps)
	}
	if first.Coverage.TotalFiles != 4 || first.Coverage.ExecutableFiles != 2 ||
		first.Coverage.SkippedFiles != 2 {
		t.Fatalf("coverage = %+v", first.Coverage)
	}
	data, err := runs.ReadArtifact(first.Shards[0].InputRef)
	if err != nil {
		t.Fatal(err)
	}
	shardInput, err := reviewcore.DecodeReviewInput(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(shardInput.Files) != 2 || len(shardInput.Contexts) != len(input.Contexts) {
		t.Fatalf("frozen shard input = %+v", shardInput)
	}
}

func TestRepositoryRecoversMissingShardsAndFencesOldGeneration(t *testing.T) {
	store, runs := newShardTestStore(t)
	input := shardTestInput(t,
		shardTestFile{path: "a.go", content: "package p\n// TODO a\n"},
		shardTestFile{path: "b.go", content: "package p\n// TODO b\n"},
		shardTestFile{path: "c.go", content: "package p\n// TODO c\n"},
	)
	inputRef := putShardInput(t, runs, input)
	manifest, err := Plan(context.Background(), "run-shard-recovery", input, inputRef,
		Limits{MaxFilesPerShard: 1, MaxBytesPerShard: 1 << 20, MaxInputBytesPerShard: 1 << 20, MaxShards: 8},
		shardEpoch, runs)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	record, err := repository.Create(context.Background(), manifest,
		shardMutation("shard-plan", shardEpoch))
	if err != nil {
		t.Fatal(err)
	}
	if len(record.PendingShardIDs) != 3 {
		t.Fatalf("initial pending shards = %+v", record.PendingShardIDs)
	}
	firstGeneration := shardGeneration(manifest.RunID, 1, 1, shardEpoch.Add(time.Second))
	if _, err := repository.BindGeneration(context.Background(), firstGeneration,
		shardMutation("bind-generation-1", firstGeneration.BoundAt)); err != nil {
		t.Fatal(err)
	}
	firstOutput := shardDetectOutput(t, runs, manifest.Shards[0])
	predating := successfulCheckpoint(
		manifest, 0, firstGeneration, 1, firstOutput, firstGeneration.BoundAt,
	)
	predating.StartedAt = firstGeneration.BoundAt.Add(-time.Nanosecond)
	if _, err := repository.RecordCheckpoint(context.Background(), predating,
		shardMutation("checkpoint-predates-generation", predating.CompletedAt)); !errors.Is(err, ErrFenced) {
		t.Fatalf("checkpoint predating generation error = %v, want ErrFenced", err)
	}
	firstCheckpoint := successfulCheckpoint(
		manifest, 0, firstGeneration, 1, firstOutput, shardEpoch.Add(2*time.Second),
	)
	if _, err := repository.RecordCheckpoint(context.Background(), firstCheckpoint,
		shardMutation("checkpoint-a-generation-1", firstCheckpoint.CompletedAt)); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Get(manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Completed) != 1 || len(recovered.PendingShardIDs) != 2 ||
		recovered.PendingShardIDs[0] != manifest.Shards[1].ShardID {
		t.Fatalf("recovered checkpoint projection = %+v", recovered)
	}
	secondGeneration := shardGeneration(manifest.RunID, 2, 2, shardEpoch.Add(3*time.Second))
	if _, err := restarted.BindGeneration(context.Background(), secondGeneration,
		shardMutation("bind-generation-2", secondGeneration.BoundAt)); err != nil {
		t.Fatal(err)
	}
	staleOutput := shardDetectOutput(t, runs, manifest.Shards[1])
	stale := successfulCheckpoint(
		manifest, 1, firstGeneration, 1, staleOutput, shardEpoch.Add(4*time.Second),
	)
	if _, err := repository.RecordCheckpoint(context.Background(), stale,
		shardMutation("stale-checkpoint-generation-1", stale.CompletedAt)); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale RecordCheckpoint() error = %v", err)
	}

	failed := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		RunID:         manifest.RunID, ShardID: manifest.Shards[1].ShardID,
		Attempt: 1, Generation: secondGeneration.Generation,
		FencingToken: secondGeneration.FencingToken, WorkerID: secondGeneration.WorkerID,
		Status: CheckpointFailed, ProcessedFiles: 0, ProcessedBytes: 0,
		Failure:   &Failure{Code: "transient", Message: "injected transient failure", Retryable: true},
		StartedAt: shardEpoch.Add(4 * time.Second), CompletedAt: shardEpoch.Add(5 * time.Second),
	}
	if _, err := restarted.RecordCheckpoint(context.Background(), failed,
		shardMutation("checkpoint-b-failed", failed.CompletedAt)); err != nil {
		t.Fatal(err)
	}
	for index := 1; index < len(manifest.Shards); index++ {
		output := shardDetectOutput(t, runs, manifest.Shards[index])
		attempt := 1
		if index == 1 {
			attempt = 2
		}
		checkpoint := successfulCheckpoint(
			manifest, index, secondGeneration, attempt, output,
			shardEpoch.Add(time.Duration(5+index)*time.Second),
		)
		if _, err := restarted.RecordCheckpoint(context.Background(), checkpoint,
			shardMutation(fmt.Sprintf("checkpoint-%d-generation-2", index), checkpoint.CompletedAt)); err != nil {
			t.Fatal(err)
		}
	}
	completedAt := shardEpoch.Add(10 * time.Second)
	terminal, err := restarted.Complete(context.Background(), manifest.RunID,
		shardMutation("complete-shard-plan", completedAt))
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Aggregate == nil || terminal.AggregateRef == nil ||
		terminal.Aggregate.Completeness != "complete" ||
		len(terminal.Aggregate.Outputs) != len(manifest.Shards) ||
		terminal.Aggregate.Outputs[0].Generation != 1 ||
		terminal.Aggregate.Outputs[1].Generation != 2 {
		t.Fatalf("terminal aggregate = %+v", terminal.Aggregate)
	}
	if _, err := restarted.RecordCheckpoint(context.Background(), firstCheckpoint,
		shardMutation("checkpoint-a-generation-1", firstCheckpoint.CompletedAt)); err != nil {
		t.Fatalf("exact checkpoint retry after terminal error = %v", err)
	}
}

func TestRepositoryCancellationIsPermanent(t *testing.T) {
	store, runs := newShardTestStore(t)
	input := shardTestInput(t,
		shardTestFile{path: "a.go", content: "package p\n// TODO a\n"},
	)
	manifest, err := Plan(context.Background(), "run-shard-cancel", input,
		putShardInput(t, runs, input),
		Limits{MaxFilesPerShard: 1, MaxBytesPerShard: 1 << 20, MaxInputBytesPerShard: 1 << 20, MaxShards: 2},
		shardEpoch, runs)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Create(context.Background(), manifest,
		shardMutation("cancel-plan", shardEpoch)); err != nil {
		t.Fatal(err)
	}
	generation := shardGeneration(manifest.RunID, 1, 1, shardEpoch.Add(time.Second))
	if _, err := repository.BindGeneration(context.Background(), generation,
		shardMutation("cancel-bind", generation.BoundAt)); err != nil {
		t.Fatal(err)
	}
	canceledAt := shardEpoch.Add(2 * time.Second)
	canceled, err := repository.Cancel(context.Background(), manifest.RunID, "operator canceled full scan",
		shardMutation("cancel-full-scan", canceledAt))
	if err != nil {
		t.Fatal(err)
	}
	if !canceled.Canceled || canceled.ActiveGeneration != nil {
		t.Fatalf("canceled record = %+v", canceled)
	}
	checkpoint := successfulCheckpoint(manifest, 0, generation, 1,
		shardDetectOutput(t, runs, manifest.Shards[0]), shardEpoch.Add(3*time.Second))
	if _, err := repository.RecordCheckpoint(context.Background(), checkpoint,
		shardMutation("post-cancel-checkpoint", checkpoint.CompletedAt)); !errors.Is(err, ErrCanceled) {
		t.Fatalf("post-cancel checkpoint error = %v", err)
	}
	if _, err := repository.BindGeneration(context.Background(),
		shardGeneration(manifest.RunID, 2, 2, shardEpoch.Add(4*time.Second)),
		shardMutation("post-cancel-bind", shardEpoch.Add(4*time.Second))); !errors.Is(err, ErrCanceled) {
		t.Fatalf("post-cancel generation error = %v", err)
	}
	if _, err := repository.Complete(context.Background(), manifest.RunID,
		shardMutation("post-cancel-complete", shardEpoch.Add(5*time.Second))); !errors.Is(err, ErrCanceled) {
		t.Fatalf("post-cancel complete error = %v", err)
	}
}

func TestConcurrentGenerationClaimsHaveOneWinner(t *testing.T) {
	store, runs := newShardTestStore(t)
	input := shardTestInput(t, shardTestFile{path: "a.go", content: "package p\n"})
	manifest, err := Plan(context.Background(), "run-shard-race", input,
		putShardInput(t, runs, input),
		Limits{MaxFilesPerShard: 1, MaxBytesPerShard: 1 << 20, MaxInputBytesPerShard: 1 << 20, MaxShards: 2},
		shardEpoch, runs)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := NewRepository(store)
	second, _ := NewRepository(store)
	if _, err := first.Create(context.Background(), manifest,
		shardMutation("race-plan", shardEpoch)); err != nil {
		t.Fatal(err)
	}
	bindings := []GenerationBinding{
		shardGeneration(manifest.RunID, 1, 1, shardEpoch.Add(time.Second)),
		shardGeneration(manifest.RunID, 1, 1, shardEpoch.Add(time.Second)),
	}
	bindings[1].LeaseID = "lease-competing"
	bindings[1].WorkerID = "worker-competing"
	repositories := []*Repository{first, second}
	var wg sync.WaitGroup
	errorsByIndex := make([]error, 2)
	for index := range repositories {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errorsByIndex[index] = repositories[index].BindGeneration(
				context.Background(), bindings[index],
				shardMutation(fmt.Sprintf("race-bind-%d", index), bindings[index].BoundAt),
			)
		}(index)
	}
	wg.Wait()
	successes := 0
	for _, err := range errorsByIndex {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrFenced) {
			t.Fatalf("unexpected concurrent claim error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent generation successes = %d, errors = %v", successes, errorsByIndex)
	}
}

func TestStrictDecodersRejectUnknownDuplicateAndTrailingJSON(t *testing.T) {
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion}
	data, _ := json.Marshal(manifest)
	unknown := append([]byte{}, data[:len(data)-1]...)
	unknown = append(unknown, []byte(`,"unknown":true}`)...)
	if _, err := DecodeManifest(unknown); err == nil {
		t.Fatal("DecodeManifest accepted unknown field")
	}
	duplicate := []byte(`{"schema_version":"a","schema_version":"b"}`)
	if _, err := DecodeManifest(duplicate); err == nil {
		t.Fatal("DecodeManifest accepted duplicate field")
	}
	if _, err := DecodeManifest(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("DecodeManifest accepted trailing JSON")
	}
}

type shardTestFile struct {
	path               string
	content            string
	contentUnavailable bool
}

func shardTestInput(t *testing.T, files ...shardTestFile) reviewcore.ReviewInput {
	t.Helper()
	entries := make([]reviewcore.FileManifestEntry, 0, len(files))
	regions := make([]reviewcore.ReviewRegion, 0, len(files))
	for _, file := range files {
		digest := shardDigest(file.content)
		var content *string
		if !file.contentUnavailable {
			value := file.content
			content = &value
			lines := uint32(1)
			for _, character := range file.content {
				if character == '\n' {
					lines++
				}
			}
			if len(file.content) > 0 && file.content[len(file.content)-1] == '\n' {
				lines--
			}
			if lines > 0 {
				regions = append(regions, reviewcore.ReviewRegion{
					Path: file.path, StartLine: 1, EndLine: lines, SHA256: digest,
				})
			}
		}
		entries = append(entries, reviewcore.FileManifestEntry{
			Path: file.path, SHA256: digest, SizeBytes: int64(len(file.content)), Content: content,
		})
	}
	input := reviewcore.ReviewInput{
		SchemaVersion: reviewcore.ReviewInputSchemaVersion,
		TargetID:      "target-shard-test", TargetMode: reviewcore.TargetModeScope,
		CanonicalPatch: "", Regions: regions, Files: entries,
		Contexts: []reviewcore.ContextBinding{},
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("test ReviewInput.Validate() error = %v", err)
	}
	return input
}

func newShardTestStore(t *testing.T) (*local.Store, *runrepo.Repository) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, runs
}

func putShardInput(t *testing.T, runs *runrepo.Repository, input reviewcore.ReviewInput) runmodel.ArtifactRef {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runs.PutArtifact(runmodel.ContractReviewInput, data)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func shardDetectOutput(
	t *testing.T,
	runs *runrepo.Repository,
	shard Shard,
) runmodel.ArtifactRef {
	t.Helper()
	data, err := runs.ReadArtifact(shard.InputRef)
	if err != nil {
		t.Fatal(err)
	}
	input, err := reviewcore.DecodeReviewInput(data)
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := reviewcore.ExecuteStage(context.Background(), input,
		reviewcore.StageMaterializeTarget, reviewcore.StageResult{})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := reviewcore.ExecuteStage(context.Background(), input,
		reviewcore.StagePlanContext, materialized)
	if err != nil {
		t.Fatal(err)
	}
	detected, err := reviewcore.ExecuteStage(context.Background(), input,
		reviewcore.StageDetect, planned)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(detected)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runs.PutArtifact(runmodel.ContractStageResult, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func shardGeneration(runID string, generation int, token uint64, at time.Time) GenerationBinding {
	return GenerationBinding{
		SchemaVersion: GenerationSchemaVersion,
		RunID:         runID, WorkloadID: runID + "-workload",
		LeaseID: fmt.Sprintf("lease-%d", generation), WorkerID: fmt.Sprintf("worker-%d", generation),
		Attempt: generation, Generation: generation, FencingToken: token, BoundAt: at,
	}
}

func successfulCheckpoint(
	manifest Manifest,
	shardIndex int,
	generation GenerationBinding,
	attempt int,
	output runmodel.ArtifactRef,
	completedAt time.Time,
) Checkpoint {
	shard := manifest.Shards[shardIndex]
	return Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		RunID:         manifest.RunID, ShardID: shard.ShardID,
		Attempt: attempt, Generation: generation.Generation,
		FencingToken: generation.FencingToken, WorkerID: generation.WorkerID,
		Status: CheckpointSucceeded, OutputRef: &output,
		ProcessedFiles: len(shard.Files), ProcessedBytes: shard.FileBytes,
		StartedAt: completedAt.Add(-time.Second), CompletedAt: completedAt,
	}
}

func shardMutation(id string, at time.Time) Mutation {
	return Mutation{
		IdempotencyKey: id, Actor: "shard-test-operator",
		Audit: "test durable review shard lifecycle", At: at.UTC(),
	}
}

func shardDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func equalJSON(left, right any) bool {
	leftData, _ := json.Marshal(left)
	rightData, _ := json.Marshal(right)
	return string(leftData) == string(rightData)
}
