package evaluation

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type repeatabilityReplayExecutorStub struct {
	mu       sync.Mutex
	runs     map[string]string
	failures map[string]int
	calls    []ReplayCaseExecutionRequest
}

func (stub *repeatabilityReplayExecutorStub) ExecuteReplay(
	_ context.Context,
	request ReplayCaseExecutionRequest,
) (string, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls = append(stub.calls, request)
	if stub.failures[request.IdempotencyKey] > 0 {
		stub.failures[request.IdempotencyKey]--
		return "", fmt.Errorf("injected replay failure")
	}
	runID, exists := stub.runs[request.IdempotencyKey]
	if !exists {
		return "", fmt.Errorf("unexpected replay key %q", request.IdempotencyKey)
	}
	return runID, nil
}

func TestRepeatabilityBatchRunnerRecoversSampleCheckpointsAndClosesRun(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	caseValue := testActiveCase("repeatability-batch-case", "repo-repeatability-batch", SplitTest, testEpoch)
	targetRef := evaluationArtifactRef("repeatability-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)

	baselineCreated := caseValue.CreatedAt.Add(2 * time.Minute)
	if _, err := repository.RecordExposure(
		context.Background(),
		testExposure("repeatability-batch-baseline-evaluation", caseValue.CaseID,
			baselineCreated.Add(-time.Minute), ExposureNotSeen),
		testMutation("repeatability-batch-baseline-exposure", baselineCreated.Add(-30*time.Second), RoleDatasetCurator),
	); err != nil {
		t.Fatal(err)
	}
	baselineReader := newEvaluationRunReaderStub(
		t, "repeatability-batch-baseline-review", targetRef, contractsv1alpha1.AgentReviewComplete,
	)
	baselineRun := baselineReader.runs["repeatability-batch-baseline-review"]
	baselineRun.Kind = runmodel.RunKindReview
	baselineReader.runs[baselineRun.RunID] = baselineRun
	baselineSnapshot := exactRepeatabilitySnapshot(baselineRun.RunID)
	baselineReader.snapshots[baselineRun.RunID] = baselineSnapshot
	baselineReader.configs[baselineSnapshot.ConfigBundleRef.URI] = reviewconfig.ConfigBundle{
		SHA256: baselineSnapshot.Config.SHA256,
	}
	baselineEvaluation, err := repository.RecordEvaluationRun(
		context.Background(), EvaluationRunRequest{
			SchemaVersion:     EvaluationRunRequestSchemaVersion,
			EvaluationRunID:   "repeatability-batch-baseline-evaluation",
			EvaluatorRevision: "presence-evaluator-1",
			Bindings: []EvaluationCaseRunBinding{{
				CaseID: caseValue.CaseID, ExpectedLabelRevision: 1, ReviewRunID: baselineRun.RunID,
			}},
			CreatedAt: baselineCreated,
		},
		testMutation("repeatability-batch-record-baseline", baselineCreated.Add(time.Minute), RoleDatasetCurator),
		baselineReader,
	)
	if err != nil {
		t.Fatalf("RecordEvaluationRun(baseline) error = %v", err)
	}

	replayAReader := newEvaluationRunReaderStub(
		t, "repeatability-batch-replay-a", targetRef, contractsv1alpha1.AgentReviewComplete,
	)
	configureExactRepeatabilityReplay(t, replayAReader, baselineReader, baselineRun.RunID, "repeatability-batch-replay-a")
	replayASnapshot := replayAReader.snapshots["repeatability-batch-replay-a"]
	replayAReader.configs[replayASnapshot.ConfigBundleRef.URI] = reviewconfig.ConfigBundle{SHA256: replayASnapshot.Config.SHA256}
	replayBReader := newEvaluationRunReaderStub(
		t, "repeatability-batch-replay-b", targetRef, contractsv1alpha1.AgentReviewComplete,
	)
	configureExactRepeatabilityReplay(t, replayBReader, baselineReader, baselineRun.RunID, "repeatability-batch-replay-b")
	replayBSnapshot := replayBReader.snapshots["repeatability-batch-replay-b"]
	replayBReader.configs[replayBSnapshot.ConfigBundleRef.URI] = reviewconfig.ConfigBundle{SHA256: replayBSnapshot.Config.SHA256}
	reader := mergeEvaluationReaders(baselineReader, replayAReader, replayBReader)

	createdAt := baselineCreated.Add(5 * time.Minute)
	request := RepeatabilityBatchRequest{
		SchemaVersion:           RepeatabilityBatchRequestSchemaVersion,
		BatchID:                 "repeatability-batch-1",
		BaselineEvaluationRunID: baselineEvaluation.EvaluationRunID,
		ReplayEvaluationRunIDs: []string{
			"repeatability-batch-evaluation-a", "repeatability-batch-evaluation-b",
		},
		RepeatabilityRunID:    "repeatability-batch-run-1",
		RepeatabilityRevision: "repeatability-batch-v1",
		ExecutorRevision:      "formal-pi-exact-replay-1",
		ExecutorTemplateRef:   testReplayExecutorTemplateRef(), MaxConcurrency: 1,
		Cases: []RepeatabilityBatchCase{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1,
			BaselineReviewRunID:          baselineRun.RunID,
			ExpectedBaselineConfigSHA256: baselineSnapshot.Config.SHA256,
			ExposureObservations:         testExposure("ignored", caseValue.CaseID, createdAt, ExposureNotSeen).Observations,
		}},
		CreatedAt: createdAt,
	}
	replayAKey := stableRepeatabilityBatchKey(
		request.BatchID, "replay",
		repeatabilityBatchCaseKey(request.ReplayEvaluationRunIDs[0], caseValue.CaseID),
	)
	replayBKey := stableRepeatabilityBatchKey(
		request.BatchID, "replay",
		repeatabilityBatchCaseKey(request.ReplayEvaluationRunIDs[1], caseValue.CaseID),
	)
	executor := &repeatabilityReplayExecutorStub{
		runs: map[string]string{
			replayAKey: "repeatability-batch-replay-a",
			replayBKey: "repeatability-batch-replay-b",
		},
		failures: map[string]int{replayBKey: 1},
	}
	var clockMu sync.Mutex
	now := createdAt.Add(10 * time.Minute)
	nextNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		now = now.Add(time.Second)
		return now
	}
	setNow := func(value time.Time) {
		clockMu.Lock()
		now = value
		clockMu.Unlock()
	}
	mutation := testMutation("repeatability-batch-intent", createdAt, RoleDatasetCurator)
	firstRunner, err := NewRepeatabilityBatchRunner(
		repository, reader, executor, nextNow, "repeatability-worker-1", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstRunner.Run(context.Background(), request, mutation); err == nil {
		t.Fatal("RepeatabilityBatchRunner.Run() unexpectedly survived injected second-sample failure")
	} else {
		t.Logf("first run failed as injected: %v", err)
	}
	partial, err := repository.GetRepeatabilityBatch(
		request.BatchID, Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}},
	)
	if err != nil || len(partial.CompletedCases) != 1 ||
		partial.CompletedCases[0].EvaluationRunID != request.ReplayEvaluationRunIDs[0] {
		t.Fatalf("partial repeatability batch = %+v, %v", partial, err)
	}

	setNow(createdAt.Add(3 * time.Hour))
	secondRunner, err := NewRepeatabilityBatchRunner(
		repository, reader, executor, nextNow, "repeatability-worker-2", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := secondRunner.Run(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("RepeatabilityBatchRunner.Run(recovery) error = %v", err)
	}
	if len(result.Cases) != 2 || !reflect.DeepEqual(result.ReplayEvaluationRunIDs, request.ReplayEvaluationRunIDs) ||
		result.RepeatabilityRunID != request.RepeatabilityRunID {
		t.Fatalf("repeatability batch result = %+v", result)
	}
	executor.mu.Lock()
	calls := append([]ReplayCaseExecutionRequest(nil), executor.calls...)
	executor.mu.Unlock()
	if len(calls) != 3 || calls[0].IdempotencyKey != replayAKey ||
		calls[1].IdempotencyKey != replayBKey || calls[2].IdempotencyKey != replayBKey ||
		calls[0].ExecutorTemplateRef != *request.ExecutorTemplateRef {
		t.Fatalf("replay calls = %+v", calls)
	}
	access := Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}}
	repeatability, err := repository.GetRepeatabilityRun(request.RepeatabilityRunID, access)
	if err != nil || repeatability.Summary.Cases != 1 || repeatability.Summary.StableCases != 1 ||
		repeatability.Summary.PairwiseJaccardPPM != repeatabilityRateScale {
		t.Fatalf("RepeatabilityRun = %+v, %v", repeatability, err)
	}
	for _, id := range request.ReplayEvaluationRunIDs {
		if _, err := repository.GetEvaluationRun(id, access); err != nil {
			t.Fatalf("GetEvaluationRun(%q) error = %v", id, err)
		}
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.GetRepeatabilityBatch(request.BatchID, access)
	if err != nil || restored.Status != RepeatabilityBatchSucceeded ||
		restored.Result == nil || !reflect.DeepEqual(*restored.Result, result) {
		t.Fatalf("GetRepeatabilityBatch(restart) = %+v, %v", restored, err)
	}
	listedBatches, err := restarted.ListRepeatabilityBatches(access)
	if err != nil || len(listedBatches) != 1 ||
		listedBatches[0].Request.BatchID != request.BatchID ||
		listedBatches[0].Status != RepeatabilityBatchSucceeded {
		t.Fatalf("ListRepeatabilityBatches(restart) = %+v, %v", listedBatches, err)
	}
	retry, err := secondRunner.Run(context.Background(), request, mutation)
	if err != nil || !reflect.DeepEqual(retry, result) {
		t.Fatalf("RepeatabilityBatchRunner.Run(terminal retry) = %+v, %v", retry, err)
	}
	executor.mu.Lock()
	callCountAfterRetry := len(executor.calls)
	executor.mu.Unlock()
	if callCountAfterRetry != len(calls) {
		t.Fatalf("terminal retry executed provider again: %d -> %d", len(calls), callCountAfterRetry)
	}
}

func TestRepeatabilityBatchRejectsStaleLeaseCheckpoint(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	caseValue := testActiveCase("repeatability-lease-case", "repo-repeatability-lease", SplitTest, testEpoch)
	targetRef := evaluationArtifactRef("repeatability-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)
	baselineCreated := caseValue.CreatedAt.Add(2 * time.Minute)
	if _, err := repository.RecordExposure(
		context.Background(),
		testExposure("repeatability-lease-baseline-evaluation", caseValue.CaseID, baselineCreated.Add(-time.Minute), ExposureNotSeen),
		testMutation("repeatability-lease-baseline-exposure", baselineCreated.Add(-30*time.Second), RoleDatasetCurator),
	); err != nil {
		t.Fatal(err)
	}
	reader := newEvaluationRunReaderStub(t, "repeatability-lease-baseline-review", targetRef, contractsv1alpha1.AgentReviewComplete)
	baselineRun := reader.runs["repeatability-lease-baseline-review"]
	baselineRun.Kind = runmodel.RunKindReview
	reader.runs[baselineRun.RunID] = baselineRun
	baseline, err := repository.RecordEvaluationRun(
		context.Background(), EvaluationRunRequest{
			SchemaVersion:     EvaluationRunRequestSchemaVersion,
			EvaluationRunID:   "repeatability-lease-baseline-evaluation",
			EvaluatorRevision: "presence-evaluator-1",
			Bindings:          []EvaluationCaseRunBinding{{CaseID: caseValue.CaseID, ExpectedLabelRevision: 1, ReviewRunID: baselineRun.RunID}},
			CreatedAt:         baselineCreated,
		},
		testMutation("repeatability-lease-record-baseline", baselineCreated.Add(time.Minute), RoleDatasetCurator), reader,
	)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := baselineCreated.Add(5 * time.Minute)
	request := RepeatabilityBatchRequest{
		SchemaVersion: RepeatabilityBatchRequestSchemaVersion, BatchID: "repeatability-lease-batch",
		BaselineEvaluationRunID: baseline.EvaluationRunID,
		ReplayEvaluationRunIDs:  []string{"repeatability-lease-evaluation"},
		RepeatabilityRunID:      "repeatability-lease-run", RepeatabilityRevision: "v1",
		ExecutorRevision:    "executor-v1",
		ExecutorTemplateRef: testReplayExecutorTemplateRef(), MaxConcurrency: 1,
		Cases: []RepeatabilityBatchCase{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1,
			BaselineReviewRunID:          baselineRun.RunID,
			ExpectedBaselineConfigSHA256: evaluationDigest("repeatability-config"),
			ExposureObservations:         testExposure("ignored", caseValue.CaseID, createdAt, ExposureNotSeen).Observations,
		}}, CreatedAt: createdAt,
	}
	mutation := testMutation("repeatability-lease-intent", createdAt, RoleDatasetCurator)
	unbound := request
	unbound.ExecutorTemplateRef = nil
	if _, err := repository.BeginRepeatabilityBatch(
		context.Background(), unbound, mutation,
	); err == nil || !strings.Contains(err.Error(), "executor_template_ref is required") {
		t.Fatalf("BeginRepeatabilityBatch(unbound template) error = %v", err)
	}
	if _, err := repository.BeginRepeatabilityBatch(context.Background(), request, mutation); err != nil {
		t.Fatal(err)
	}
	resumeMutation := Mutation{
		IdempotencyKey: "repeatability-resume-request", Actor: "repeatability-resume-operator",
		Roles: []Role{RoleDatasetCurator}, Audit: "resume repeatability batch",
		At: createdAt.Add(30 * time.Second),
	}
	resumed, err := repository.RequestRepeatabilityBatchResume(
		context.Background(), request.BatchID, resumeMutation,
	)
	if err != nil || resumed.LastResume == nil ||
		!reflect.DeepEqual(*resumed.LastResume, resumeMutation) ||
		resumed.Intent.Actor != mutation.Actor || resumed.UpdatedBy != mutation.Actor {
		t.Fatalf("RequestRepeatabilityBatchResume() record = %+v, %v", resumed, err)
	}
	if retried, retryErr := repository.RequestRepeatabilityBatchResume(
		context.Background(), request.BatchID, resumeMutation,
	); retryErr != nil || !reflect.DeepEqual(retried, resumed) {
		t.Fatalf("RequestRepeatabilityBatchResume(idempotent) = %+v, %v", retried, retryErr)
	}
	leaseOne, err := repository.ClaimRepeatabilityBatch(
		context.Background(), request.BatchID, "worker-one", time.Second,
		derivedRepeatabilityBatchMutation(mutation, request.BatchID, "claim", "worker-one", createdAt.Add(time.Minute)),
	)
	if err != nil {
		t.Fatal(err)
	}
	leaseTwo, err := repository.ClaimRepeatabilityBatch(
		context.Background(), request.BatchID, "worker-two", time.Second,
		derivedRepeatabilityBatchMutation(mutation, request.BatchID, "claim", "worker-two", createdAt.Add(2*time.Minute)),
	)
	if err != nil {
		t.Fatal(err)
	}
	failureAt := createdAt.Add(2*time.Minute + 500*time.Millisecond)
	failure := BatchAttemptFailure{
		Code: BatchAttemptExecutionFailed, WorkerID: leaseTwo.WorkerID, LeaseID: leaseTwo.LeaseID,
		Generation: leaseTwo.Generation, FencingToken: leaseTwo.FencingToken, ObservedAt: failureAt,
	}
	failed, err := repository.RecordRepeatabilityBatchAttemptFailure(
		context.Background(), request.BatchID, leaseTwo, failure, Mutation{
			IdempotencyKey: "repeatability-attempt-failed", Actor: leaseTwo.WorkerID,
			Roles: []Role{RoleDatasetCurator}, Audit: "record bounded repeatability attempt failure",
			At: failureAt,
		},
	)
	if err != nil || failed.LastFailure == nil || *failed.LastFailure != failure ||
		failed.Intent.Actor != mutation.Actor || failed.UpdatedBy != mutation.Actor {
		t.Fatalf("RecordRepeatabilityBatchAttemptFailure() = %+v, %v", failed, err)
	}
	checkpoint := RepeatabilityBatchCaseResult{
		EvaluationRunID: request.ReplayEvaluationRunIDs[0], CaseID: caseValue.CaseID,
		BaselineReviewRunID: baselineRun.RunID, ReplayReviewRunID: "stale-replay-run",
		ReplayReviewRunRef: evaluationArtifactRef("stale-replay-run", runmodel.ContractReviewRun),
		ReplayChangeSetRef: evaluationArtifactRef("stale-replay-change", runmodel.ContractReplayChangeSet),
	}
	_, err = repository.RecordRepeatabilityBatchCase(
		context.Background(), request.BatchID, checkpoint, leaseOne,
		derivedRepeatabilityBatchMutation(mutation, request.BatchID, "checkpoint", "stale", createdAt.Add(3*time.Minute)),
	)
	if err == nil {
		t.Fatal("RecordRepeatabilityBatchCase() accepted stale generation lease")
	}
}
