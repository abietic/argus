package reviewshard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/scheduling"
)

const (
	defaultMaxFilesPerShard = 32
	defaultMaxShards        = 4096
)

type ArtifactStore interface {
	PutArtifact(string, []byte) (runmodel.ArtifactRef, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
}

// ScopeExecutor is the local adapter from application scope review to the
// durable shard ledger. It copies authority only from one already-claimed
// outer dispatch and never creates a nested scheduling workload.
type ScopeExecutor struct {
	repository *Repository
	artifacts  ArtifactStore
	dispatch   scheduling.Dispatch
	actor      string
	now        func() time.Time
	detector   ShardDetector
}

type ShardDetector interface {
	Detect(context.Context, reviewcore.ReviewInput, reviewcore.RuntimePolicy) (reviewcore.StageResult, error)
}

type DetectShardFunc func(context.Context, reviewcore.ReviewInput, reviewcore.RuntimePolicy) (reviewcore.StageResult, error)

func (function DetectShardFunc) Detect(
	ctx context.Context,
	input reviewcore.ReviewInput,
	policy reviewcore.RuntimePolicy,
) (reviewcore.StageResult, error) {
	return function(ctx, input, policy)
}

func NewScopeExecutor(
	repository *Repository,
	artifacts ArtifactStore,
	dispatch scheduling.Dispatch,
	actor string,
	now func() time.Time,
) (*ScopeExecutor, error) {
	return NewScopeExecutorWithDetector(repository, artifacts, dispatch, actor, now, nil)
}

func NewScopeExecutorWithDetector(
	repository *Repository,
	artifacts ArtifactStore,
	dispatch scheduling.Dispatch,
	actor string,
	now func() time.Time,
	detector ShardDetector,
) (*ScopeExecutor, error) {
	if repository == nil || artifacts == nil {
		return nil, fmt.Errorf("shard repository and artifact store are required")
	}
	if err := dispatch.Spec.Validate(); err != nil {
		return nil, fmt.Errorf("validate shard execution workload: %w", err)
	}
	if err := dispatch.Lease.Validate(); err != nil {
		return nil, fmt.Errorf("validate shard execution lease: %w", err)
	}
	if dispatch.Spec.WorkloadID != dispatch.Lease.WorkloadID ||
		dispatch.Spec.RunID == "" {
		return nil, fmt.Errorf("shard execution dispatch identity does not close")
	}
	if err := validateID("actor", actor); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if detector == nil {
		detector = DetectShardFunc(DetectShardWithCore)
	}
	return &ScopeExecutor{
		repository: repository, artifacts: artifacts, dispatch: dispatch,
		actor: actor, now: now, detector: detector,
	}, nil
}

func (executor *ScopeExecutor) PlanScope(
	ctx context.Context,
	request application.ScopeShardPlanRequest,
) (application.ScopeShardPlan, error) {
	if request.MaxInputBytes < 1 {
		return application.ScopeShardPlan{}, fmt.Errorf("max input bytes must be positive")
	}
	maxBytes := min(request.MaxInputBytes, int64(1<<30))
	limits := Limits{
		MaxFilesPerShard: defaultMaxFilesPerShard,
		MaxBytesPerShard: maxBytes, MaxInputBytesPerShard: maxBytes,
		MaxShards: defaultMaxShards,
	}
	if prior, err := executor.repository.Get(request.RunID); err == nil {
		if prior.Manifest.TargetDigest != request.InputRef.SHA256 ||
			prior.Manifest.ReviewInputRef != request.InputRef || prior.Manifest.Limits != limits {
			return application.ScopeShardPlan{}, fmt.Errorf("%w: existing shard plan differs", ErrConflict)
		}
		return scopePlan(prior), nil
	} else if !errors.Is(err, ErrNotFound) {
		return application.ScopeShardPlan{}, err
	}
	manifest, err := Plan(
		ctx, request.RunID, request.Input, request.InputRef, limits,
		request.CreatedAt, executor.artifacts,
	)
	if err != nil {
		return application.ScopeShardPlan{}, err
	}
	record, err := executor.repository.Create(ctx, manifest, Mutation{
		IdempotencyKey: request.RunID + "-shard-plan", Actor: executor.actor,
		Audit: "freeze exact scope shard manifest", At: request.CreatedAt,
	})
	if err != nil {
		return application.ScopeShardPlan{}, err
	}
	return scopePlan(record), nil
}

func (executor *ScopeExecutor) ScopeCoverage(
	ctx context.Context,
	runID string,
	manifestRef runmodel.ArtifactRef,
) (application.ScopeShardCoverage, error) {
	if err := checkContext(ctx); err != nil {
		return application.ScopeShardCoverage{}, err
	}
	record, err := executor.repository.Get(runID)
	if err != nil {
		return application.ScopeShardCoverage{}, err
	}
	if record.ManifestRef != manifestRef {
		return application.ScopeShardCoverage{}, fmt.Errorf("scope manifest reference differs from durable plan")
	}
	return scopeCoverage(record.Manifest), nil
}

func scopePlan(record Record) application.ScopeShardPlan {
	return application.ScopeShardPlan{
		ManifestRef: record.ManifestRef,
		Coverage:    scopeCoverage(record.Manifest),
	}
}

func scopeCoverage(manifest Manifest) application.ScopeShardCoverage {
	reasons := make([]string, 0, len(manifest.Gaps))
	for _, gap := range manifest.Gaps {
		reasons = append(reasons, gap.Path+":"+gap.ReasonCode)
	}
	return application.ScopeShardCoverage{Complete: len(reasons) == 0, ReasonCodes: reasons}
}

func (executor *ScopeExecutor) DetectScope(
	ctx context.Context,
	request application.ScopeShardDetectRequest,
) (reviewcore.StageResult, error) {
	if request.MaxConcurrency < 1 || request.MaxInputBytes < 1 || request.MaxOutputBytes < 1 {
		return reviewcore.StageResult{}, fmt.Errorf("scope detect budgets must be positive")
	}
	record, err := executor.repository.Get(request.RunID)
	if err != nil {
		return reviewcore.StageResult{}, err
	}
	if record.ManifestRef != request.ManifestRef ||
		record.Manifest.ReviewInputRef != request.InputRef ||
		record.Manifest.TargetDigest != request.InputRef.SHA256 {
		return reviewcore.StageResult{}, fmt.Errorf("scope shard plan does not bind exact execution input")
	}
	if executor.dispatch.Spec.RunID != request.RunID {
		return reviewcore.StageResult{}, fmt.Errorf("outer dispatch belongs to another review run")
	}
	if record.Aggregate != nil {
		return executor.mergeAggregate(ctx, request, record)
	}
	binding := GenerationBinding{
		SchemaVersion: GenerationSchemaVersion,
		RunID:         request.RunID, WorkloadID: executor.dispatch.Lease.WorkloadID,
		LeaseID: executor.dispatch.Lease.LeaseID, WorkerID: executor.dispatch.Lease.WorkerID,
		Attempt: executor.dispatch.Lease.Attempt, Generation: executor.dispatch.Lease.Generation,
		FencingToken: executor.dispatch.Lease.FencingToken,
		BoundAt:      executor.dispatch.Lease.AcquiredAt.UTC(),
	}
	record, err = executor.repository.BindGeneration(ctx, binding, Mutation{
		IdempotencyKey: fmt.Sprintf("%s-shard-generation-%d-%d", request.RunID, binding.Generation, binding.FencingToken),
		Actor:          executor.actor, Audit: "bind scope shards to outer scheduling generation",
		At: binding.BoundAt,
	})
	if err != nil {
		return reviewcore.StageResult{}, err
	}

	type shardFailure struct {
		ordinal int
		err     error
	}
	semaphore := make(chan struct{}, request.MaxConcurrency)
	failures := make(chan shardFailure, len(record.PendingShardIDs))
	var wait sync.WaitGroup
	for _, shardID := range record.PendingShardIDs {
		shard, exists := findShard(record.Manifest.Shards, shardID)
		if !exists {
			return reviewcore.StageResult{}, fmt.Errorf("pending shard %q is absent from manifest", shardID)
		}
		wait.Add(1)
		go func(shard Shard) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				failures <- shardFailure{ordinal: shard.Ordinal, err: ctx.Err()}
				return
			}
			if err := executor.executeShard(ctx, request, record, binding, shard); err != nil {
				failures <- shardFailure{ordinal: shard.Ordinal, err: err}
			}
		}(shard)
	}
	wait.Wait()
	close(failures)
	var first *shardFailure
	for failure := range failures {
		if first == nil || failure.ordinal < first.ordinal {
			copy := failure
			first = &copy
		}
	}
	if first != nil {
		return reviewcore.StageResult{}, fmt.Errorf("scope shard %d: %w", first.ordinal, first.err)
	}

	completedAt := executor.timestamp()
	record, err = executor.repository.Complete(ctx, request.RunID, Mutation{
		IdempotencyKey: request.RunID + "-shard-aggregate", Actor: executor.actor,
		Audit: "fan in exact scope shard checkpoints", At: completedAt,
	})
	if err != nil {
		return reviewcore.StageResult{}, err
	}
	return executor.mergeAggregate(ctx, request, record)
}

func (executor *ScopeExecutor) mergeAggregate(
	ctx context.Context,
	request application.ScopeShardDetectRequest,
	record Record,
) (reviewcore.StageResult, error) {
	if record.Aggregate == nil {
		return reviewcore.StageResult{}, fmt.Errorf("scope shard aggregate is not committed")
	}
	merged := make([]reviewcore.DetectShardResult, 0, len(record.Aggregate.Outputs))
	for _, output := range record.Aggregate.Outputs {
		shard, exists := findShard(record.Manifest.Shards, output.ShardID)
		if !exists {
			return reviewcore.StageResult{}, fmt.Errorf("aggregate references unknown shard %q", output.ShardID)
		}
		inputData, err := executor.artifacts.ReadArtifact(shard.InputRef)
		if err != nil {
			return reviewcore.StageResult{}, err
		}
		shardInput, err := reviewcore.DecodeReviewInput(inputData)
		if err != nil {
			return reviewcore.StageResult{}, err
		}
		outputData, err := executor.artifacts.ReadArtifact(output.OutputRef)
		if err != nil {
			return reviewcore.StageResult{}, err
		}
		result, err := reviewcore.DecodeStageResult(outputData)
		if err != nil {
			return reviewcore.StageResult{}, err
		}
		merged = append(merged, reviewcore.DetectShardResult{Input: shardInput, Result: result})
	}
	return reviewcore.MergeDetectShardResults(ctx, request.Input, request.Upstream, merged)
}

func (executor *ScopeExecutor) CancelScope(
	ctx context.Context,
	runID string,
	reason string,
	at time.Time,
) error {
	_, err := executor.repository.Cancel(ctx, runID, reason, Mutation{
		IdempotencyKey: runID + "-shard-cancel", Actor: executor.actor,
		Audit: "permanently cancel scope shard execution", At: at.UTC(),
	})
	return err
}

func (executor *ScopeExecutor) executeShard(
	ctx context.Context,
	request application.ScopeShardDetectRequest,
	record Record,
	binding GenerationBinding,
	shard Shard,
) error {
	startedAt := executor.timestamp()
	attempt := nextAttempt(record.CheckpointHistory, shard.ShardID, binding.Generation)
	checkpoint := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		RunID:         request.RunID, ShardID: shard.ShardID, Attempt: attempt,
		Generation: binding.Generation, FencingToken: binding.FencingToken,
		WorkerID: binding.WorkerID, StartedAt: startedAt,
	}
	var executeErr error
	if shard.InputRef.SizeBytes > request.MaxInputBytes {
		executeErr = fmt.Errorf("exact shard input is %d bytes, limit %d", shard.InputRef.SizeBytes, request.MaxInputBytes)
	} else {
		data, err := executor.artifacts.ReadArtifact(shard.InputRef)
		if err != nil {
			executeErr = err
		} else {
			input, err := reviewcore.DecodeReviewInput(data)
			if err != nil {
				executeErr = err
			} else {
				result, err := executor.detector.Detect(ctx, input, request.Policy)
				if err != nil {
					executeErr = err
				} else {
					outputData, err := json.Marshal(result)
					if err != nil {
						executeErr = err
					} else if int64(len(outputData)) > request.MaxOutputBytes {
						executeErr = fmt.Errorf("shard output is %d bytes, limit %d", len(outputData), request.MaxOutputBytes)
					} else {
						ref, err := executor.artifacts.PutArtifact(runmodel.ContractStageResult, outputData)
						if err != nil {
							executeErr = err
						} else {
							checkpoint.Status = CheckpointSucceeded
							checkpoint.OutputRef = &ref
							checkpoint.ProcessedFiles = len(shard.Files)
							checkpoint.ProcessedBytes = shard.FileBytes
						}
					}
				}
			}
		}
	}
	checkpoint.CompletedAt = executor.timestamp()
	if checkpoint.CompletedAt.Before(checkpoint.StartedAt) {
		checkpoint.CompletedAt = checkpoint.StartedAt
	}
	if executeErr != nil {
		checkpoint.Status = CheckpointFailed
		if errors.Is(executeErr, context.Canceled) || errors.Is(executeErr, context.DeadlineExceeded) {
			checkpoint.Status = CheckpointCanceled
		}
		checkpoint.Failure = &Failure{Code: "shard_execution_failed", Message: executeErr.Error(), Retryable: checkpoint.Status == CheckpointFailed}
	}
	_, recordErr := executor.repository.RecordCheckpoint(context.WithoutCancel(ctx), checkpoint, Mutation{
		IdempotencyKey: fmt.Sprintf("%s-%s-%d-%d-checkpoint", request.RunID, shard.ShardID, binding.Generation, attempt),
		Actor:          executor.actor, Audit: "record exact scope shard outcome", At: checkpoint.CompletedAt,
	})
	if recordErr != nil {
		return recordErr
	}
	return executeErr
}

// DetectShardWithCore executes the deterministic shard-local materialize,
// context-plan, and detect prefix. It is exported as the baseline detector so
// physical worker tests and future adapters can wrap the same contract.
func DetectShardWithCore(
	ctx context.Context,
	input reviewcore.ReviewInput,
	policy reviewcore.RuntimePolicy,
) (reviewcore.StageResult, error) {
	materialized, err := reviewcore.ExecuteStageWithPolicy(
		ctx, input, reviewcore.StageMaterializeTarget, reviewcore.StageResult{}, policy,
	)
	if err != nil {
		return reviewcore.StageResult{}, err
	}
	planned, err := reviewcore.ExecuteStageWithPolicy(
		ctx, input, reviewcore.StagePlanContext, materialized, policy,
	)
	if err != nil {
		return reviewcore.StageResult{}, err
	}
	return reviewcore.ExecuteStageWithPolicy(ctx, input, reviewcore.StageDetect, planned, policy)
}

func nextAttempt(history []Checkpoint, shardID string, generation int) int {
	result := 1
	for _, checkpoint := range history {
		if checkpoint.ShardID == shardID && checkpoint.Generation == generation && checkpoint.Attempt >= result {
			result = checkpoint.Attempt + 1
		}
	}
	return result
}

func (executor *ScopeExecutor) timestamp() time.Time {
	return executor.now().UTC()
}

var _ application.ScopeShardPort = (*ScopeExecutor)(nil)
