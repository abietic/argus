package evaluation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type replayCaseExecutorStub struct {
	mu    sync.Mutex
	runs  map[string]string
	calls []ReplayCaseExecutionRequest
	hook  func(ReplayCaseExecutionRequest) error
}

type blockingBatchExecutor struct {
	mu        sync.Mutex
	runs      map[string]string
	started   chan string
	release   chan struct{}
	active    int
	maxActive int
}

type failNthLoadReader struct {
	delegate EvaluationRunReader
	mu       sync.Mutex
	calls    int
	failAt   int
	failed   bool
}

func (reader *failNthLoadReader) LoadRun(id string) (runmodel.ReviewRun, error) {
	reader.mu.Lock()
	reader.calls++
	shouldFail := !reader.failed && reader.calls == reader.failAt
	if shouldFail {
		reader.failed = true
	}
	reader.mu.Unlock()
	if shouldFail {
		return runmodel.ReviewRun{}, fmt.Errorf("injected post-checkpoint read failure")
	}
	return reader.delegate.LoadRun(id)
}

func (reader *failNthLoadReader) ExecutionSnapshotForRun(id string) (runmodel.ExecutionSnapshot, error) {
	return reader.delegate.ExecutionSnapshotForRun(id)
}

func (reader *failNthLoadReader) ReadJSONArtifact(ref runmodel.ArtifactRef, out any) error {
	return reader.delegate.ReadJSONArtifact(ref, out)
}

func (reader *failNthLoadReader) ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	return reader.delegate.ReadArtifact(ref)
}

func (reader *failNthLoadReader) CommittedRunRef(id string) (runmodel.ArtifactRef, error) {
	return reader.delegate.CommittedRunRef(id)
}

func (reader *failNthLoadReader) CheckArtifactEligibility(
	ref runmodel.ArtifactRef,
	use runrepo.ArtifactUse,
) error {
	return reader.delegate.CheckArtifactEligibility(ref, use)
}

func (executor *blockingBatchExecutor) ExecuteReplay(
	ctx context.Context,
	request ReplayCaseExecutionRequest,
) (string, error) {
	executor.mu.Lock()
	executor.active++
	if executor.active > executor.maxActive {
		executor.maxActive = executor.active
	}
	executor.mu.Unlock()
	executor.started <- request.CaseID
	select {
	case <-executor.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	executor.mu.Lock()
	executor.active--
	executor.mu.Unlock()
	return executor.runs[request.CaseID], nil
}

func (stub *replayCaseExecutorStub) ExecuteReplay(
	_ context.Context,
	request ReplayCaseExecutionRequest,
) (string, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls = append(stub.calls, request)
	if stub.hook != nil {
		if err := stub.hook(request); err != nil {
			return "", err
		}
	}
	return stub.runs[request.CaseID], nil
}

func TestExperimentBatchRunnerPersistsIntentExposureAndRecoverableTerminal(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	caseValue := testActiveCase("batch-case", "repo-batch", SplitTest, testEpoch)
	targetRef := evaluationArtifactRef("batch-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)
	baselineCreated := caseValue.CreatedAt.Add(2 * time.Minute)
	if _, err := repository.RecordExposure(context.Background(),
		testExposure("batch-baseline-evaluation", caseValue.CaseID,
			baselineCreated.Add(-time.Minute), ExposureNotSeen),
		testMutation("batch-baseline-exposure", baselineCreated.Add(-30*time.Second),
			RoleDatasetCurator)); err != nil {
		t.Fatal(err)
	}
	baselineReader := newEvaluationRunReaderStub(t, "batch-baseline-review", targetRef,
		contractsv1alpha1.AgentReviewComplete)
	baselineRun := baselineReader.runs["batch-baseline-review"]
	baselineRun.Kind = runmodel.RunKindReview
	baselineReader.runs[baselineRun.RunID] = baselineRun
	baselineConfigSHA := evaluationDigest("batch-baseline-config")
	baselineSnapshot := baselineReader.snapshots[baselineRun.RunID]
	baselineSnapshot.ConfigBundleRef = evaluationArtifactRef(
		"batch-baseline-config", runmodel.ContractConfigBundle)
	baselineSnapshot.ConfigBundleRef.SHA256 = baselineConfigSHA
	baselineSnapshot.ConfigBundleRef.URI = "artifact://local/sha256/" + baselineConfigSHA
	baselineReader.snapshots[baselineRun.RunID] = baselineSnapshot
	baselineReader.configs[baselineSnapshot.ConfigBundleRef.URI] = reviewconfig.ConfigBundle{
		SHA256: baselineConfigSHA,
	}
	baselineEvaluation, err := repository.RecordEvaluationRun(context.Background(), EvaluationRunRequest{
		SchemaVersion:   EvaluationRunRequestSchemaVersion,
		EvaluationRunID: "batch-baseline-evaluation", EvaluatorRevision: "presence-evaluator-1",
		Bindings: []EvaluationCaseRunBinding{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1, ReviewRunID: baselineRun.RunID,
		}}, CreatedAt: baselineCreated,
	}, testMutation("batch-record-baseline", baselineCreated.Add(time.Minute), RoleDatasetCurator), baselineReader)
	if err != nil {
		t.Fatalf("RecordEvaluationRun(baseline) error = %v", err)
	}

	variantReader := newEvaluationRunReaderStub(t, "batch-variant-review", targetRef,
		contractsv1alpha1.AgentReviewComplete)
	variantRun := variantReader.runs["batch-variant-review"]
	variantRun.Kind = runmodel.RunKindReplay
	variantRun.SourceRunID = baselineRun.RunID
	variantRun.ReplayVariable = runmodel.ReplayVariablePrompt
	variantReader.runs[variantRun.RunID] = variantRun
	variantConfigSHA := evaluationDigest("batch-variant-config")
	changeRef := evaluationArtifactRef("batch-variant-change", runmodel.ContractReplayChangeSet)
	variantSnapshot := variantReader.snapshots[variantRun.RunID]
	variantSnapshot.ConfigBundleRef = evaluationArtifactRef(
		"batch-variant-config", runmodel.ContractConfigBundle)
	variantSnapshot.ConfigBundleRef.SHA256 = variantConfigSHA
	variantSnapshot.ConfigBundleRef.URI = "artifact://local/sha256/" + variantConfigSHA
	variantSnapshot.ReplayChangeSetRef = &changeRef
	variantReader.snapshots[variantRun.RunID] = variantSnapshot
	variantReader.configs[variantSnapshot.ConfigBundleRef.URI] = reviewconfig.ConfigBundle{
		SHA256: variantConfigSHA,
	}
	variantReader.changes[changeRef.URI] = runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     "batch-variant", SourceRunID: baselineRun.RunID, RootRunID: baselineRun.RunID,
		StartStage: "agent_hypothesize", Variable: runmodel.ReplayVariablePrompt,
		BaselineSHA256: baselineConfigSHA, VariantSHA256: variantConfigSHA,
		ChangedFields: []string{"agent_review.prompt"}, RemoteWrites: "deny",
		CreatedAt: baselineCreated.Add(time.Minute),
	}
	reader := mergeEvaluationReaders(baselineReader, variantReader)
	flakyReader := &failNthLoadReader{delegate: reader, failAt: 2}
	executor := &replayCaseExecutorStub{runs: map[string]string{caseValue.CaseID: variantRun.RunID}}
	executionStarted := make(chan struct{})
	releaseExecution := make(chan struct{})
	executor.hook = func(_ ReplayCaseExecutionRequest) error {
		exposures, err := repository.ExposureHistory(caseValue.CaseID,
			Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
		if err != nil {
			return err
		}
		for _, entry := range exposures {
			if entry.Exposure.EvaluationRunID == "batch-variant-evaluation" {
				close(executionStarted)
				<-releaseExecution
				return nil
			}
		}
		return fmt.Errorf("variant exposure was not recorded before replay execution")
	}
	now := baselineCreated.Add(10 * time.Minute)
	var nowMu sync.Mutex
	nextNow := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		now = now.Add(time.Second)
		return now
	}
	setNow := func(value time.Time) {
		nowMu.Lock()
		now = value
		nowMu.Unlock()
	}
	runner, err := NewExperimentBatchRunner(repository, flakyReader, executor, nextNow,
		"batch-test-worker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := baselineCreated.Add(5 * time.Minute)
	request := ExperimentBatchRequest{
		SchemaVersion: ExperimentBatchRequestSchemaVersion, BatchID: "experiment-batch-1",
		BaselineEvaluationRunID: baselineEvaluation.EvaluationRunID,
		VariantEvaluationRunID:  "batch-variant-evaluation",
		ExperimentRunID:         "batch-experiment-run", ExperimentRevision: "batch-experiment-revision-1",
		Variable: runmodel.ReplayVariablePrompt, ExecutorRevision: "formal-pi-replay-1",
		ExecutorTemplateRef: testReplayExecutorTemplateRef(),
		MaxConcurrency:      2,
		Cases: []ExperimentBatchCase{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1,
			BaselineReviewRunID:          baselineRun.RunID,
			ExpectedBaselineConfigSHA256: baselineConfigSHA,
			ExpectedVariantConfigSHA256:  variantConfigSHA,
			ExposureObservations: testExposure("ignored", caseValue.CaseID,
				createdAt, ExposureNotSeen).Observations,
		}}, CreatedAt: createdAt,
	}
	mutation := testMutation("batch-intent", createdAt, RoleDatasetCurator)
	unbound := request
	unbound.ExecutorTemplateRef = nil
	if _, err := repository.BeginExperimentBatch(
		context.Background(), unbound, mutation,
	); err == nil || !strings.Contains(err.Error(), "executor_template_ref is required") {
		t.Fatalf("BeginExperimentBatch(unbound template) error = %v", err)
	}
	firstRunError := make(chan error, 1)
	go func() {
		_, runErr := runner.Run(context.Background(), request, mutation)
		firstRunError <- runErr
	}()
	<-executionStarted
	competingRunner, err := NewExperimentBatchRunner(repository, flakyReader, executor, nextNow,
		"batch-competing-worker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := competingRunner.Run(context.Background(), request, mutation); !errors.Is(err, ErrConflict) {
		t.Fatalf("ExperimentBatchRunner.Run(concurrent competing lease) error = %v", err)
	}
	close(releaseExecution)
	if err := <-firstRunError; err == nil ||
		!strings.Contains(err.Error(), "injected post-checkpoint read failure") {
		t.Fatalf("ExperimentBatchRunner.Run(post-checkpoint failure) error = %v", err)
	}
	checkpointed, err := repository.GetExperimentBatch(request.BatchID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || checkpointed.Status != ExperimentBatchRunning ||
		len(checkpointed.CompletedCases) != 1 || len(executor.calls) != 1 ||
		checkpointed.ActiveLease == nil || checkpointed.ActiveLease.Generation != 1 ||
		checkpointed.ActiveLease.FencingToken != 1 {
		t.Fatalf("checkpointed batch = %+v, %v calls=%d", checkpointed, err, len(executor.calls))
	}
	resumeMutation := Mutation{
		IdempotencyKey: "batch-resume-request", Actor: "batch-resume-operator",
		Roles: []Role{RoleDatasetCurator}, Audit: "resume checkpointed experiment batch",
		At: checkpointed.UpdatedAt.Add(time.Minute),
	}
	resumed, err := repository.RequestExperimentBatchResume(
		context.Background(), request.BatchID, resumeMutation,
	)
	if err != nil || resumed.LastResume == nil ||
		!reflect.DeepEqual(*resumed.LastResume, resumeMutation) ||
		resumed.Intent.Actor != mutation.Actor || resumed.UpdatedBy != mutation.Actor {
		t.Fatalf("RequestExperimentBatchResume() record = %+v, %v", resumed, err)
	}
	retriedResume, err := repository.RequestExperimentBatchResume(
		context.Background(), request.BatchID, resumeMutation,
	)
	if err != nil || !reflect.DeepEqual(retriedResume, resumed) {
		t.Fatalf("RequestExperimentBatchResume(idempotent) = %+v, %v", retriedResume, err)
	}
	staleLease := *checkpointed.ActiveLease
	renewAt := staleLease.ClaimedAt.Add(10 * time.Minute)
	renewedLease, err := repository.RenewExperimentBatchLease(
		context.Background(), staleLease, time.Hour,
		testMutation("batch-renew-1", renewAt, RoleDatasetCurator),
	)
	if err != nil || renewedLease.Generation != staleLease.Generation ||
		renewedLease.FencingToken != staleLease.FencingToken ||
		!renewedLease.ExpiresAt.After(staleLease.ExpiresAt) {
		t.Fatalf("RenewExperimentBatchLease() = %+v, %v", renewedLease, err)
	}
	_, err = repository.RecordExperimentBatchCase(
		context.Background(), request.BatchID, checkpointed.CompletedCases[0], staleLease,
		testMutation("batch-stale-checkpoint", renewAt.Add(time.Second), RoleDatasetCurator),
	)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("RecordExperimentBatchCase(renewed same-token duplicate) error = %v", err)
	}
	setNow(renewAt.Add(time.Minute))
	if _, err := competingRunner.Run(context.Background(), request, mutation); !errors.Is(err, ErrConflict) {
		t.Fatalf("ExperimentBatchRunner.Run(active competing lease) error = %v", err)
	}
	setNow(renewedLease.ExpiresAt.Add(time.Minute))
	result, err := competingRunner.Run(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("ExperimentBatchRunner.Run(recover) error = %v", err)
	}
	if result.BatchID != request.BatchID || len(result.Cases) != 1 ||
		result.Cases[0].VariantReviewRunID != variantRun.RunID {
		t.Fatalf("batch result = %+v", result)
	}
	if len(executor.calls) != 1 || executor.calls[0].IdempotencyKey == "" ||
		executor.calls[0].ExpectedVariantConfigSHA256 != variantConfigSHA ||
		executor.calls[0].ExecutorTemplateRef != *request.ExecutorTemplateRef {
		t.Fatalf("executor calls = %+v", executor.calls)
	}
	completedRecord, err := repository.GetExperimentBatch(request.BatchID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || completedRecord.ActiveLease != nil || completedRecord.LastLease == nil ||
		completedRecord.LastLease.Generation != 2 || completedRecord.LastLease.FencingToken != 2 {
		t.Fatalf("completed batch lease = %+v, %v", completedRecord, err)
	}
	retry, err := runner.Run(context.Background(), request, mutation)
	if err != nil || retry.BatchID != result.BatchID || len(executor.calls) != 1 {
		t.Fatalf("ExperimentBatchRunner.Run(retry) = %+v, %v calls=%d", retry, err, len(executor.calls))
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	record, err := restarted.GetExperimentBatch(request.BatchID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || record.Status != ExperimentBatchSucceeded || record.Result == nil {
		t.Fatalf("GetExperimentBatch(restart) = %+v, %v", record, err)
	}
	listedBatches, err := restarted.ListExperimentBatches(
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}},
	)
	if err != nil || len(listedBatches) != 1 ||
		listedBatches[0].Request.BatchID != request.BatchID ||
		listedBatches[0].Status != ExperimentBatchSucceeded {
		t.Fatalf("ListExperimentBatches(restart) = %+v, %v", listedBatches, err)
	}
	exposures, err := restarted.ExposureHistory(caseValue.CaseID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || len(exposures) != 2 ||
		exposures[1].Exposure.EvaluationRunID != request.VariantEvaluationRunID {
		t.Fatalf("ExposureHistory() = %+v, %v", exposures, err)
	}
}

func TestExperimentBatchRunnerBoundsParallelReplayAndKeepsCaseOrder(t *testing.T) {
	reader := &evaluationRunReaderStub{
		runs: make(map[string]runmodel.ReviewRun), snapshots: make(map[string]runmodel.ExecutionSnapshot),
		reports: make(map[string]contractsv1alpha1.GovernedReviewReport),
		changes: make(map[string]runmodel.ReplayChangeSet), configs: make(map[string]reviewconfig.ConfigBundle),
		runRefs: make(map[string]runmodel.ArtifactRef),
	}
	executor := &blockingBatchExecutor{
		runs: make(map[string]string), started: make(chan string, 3), release: make(chan struct{}),
	}
	request := ExperimentBatchRequest{
		SchemaVersion: ExperimentBatchRequestSchemaVersion, BatchID: "bounded-batch",
		BaselineEvaluationRunID: "bounded-baseline", VariantEvaluationRunID: "bounded-variant",
		ExperimentRunID: "bounded-experiment", ExperimentRevision: "bounded-revision",
		Variable: runmodel.ReplayVariablePrompt, ExecutorRevision: "executor-1",
		ExecutorTemplateRef: testReplayExecutorTemplateRef(),
		MaxConcurrency:      2, Cases: []ExperimentBatchCase{}, CreatedAt: testEpoch,
	}
	for index := 1; index <= 3; index++ {
		caseID := fmt.Sprintf("case-%d", index)
		baselineID := fmt.Sprintf("baseline-%d", index)
		variantID := fmt.Sprintf("variant-%d", index)
		baselineSHA := evaluationDigest(baselineID + "-config")
		variantSHA := evaluationDigest(variantID + "-config")
		changeRef := evaluationArtifactRef(variantID+"-change", runmodel.ContractReplayChangeSet)
		reader.runs[baselineID] = runmodel.ReviewRun{
			RunID: baselineID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusSucceeded,
		}
		reader.runs[variantID] = runmodel.ReviewRun{
			RunID: variantID, Kind: runmodel.RunKindReplay, SourceRunID: baselineID,
			ReplayVariable: runmodel.ReplayVariablePrompt, Status: runmodel.RunStatusSucceeded,
		}
		baselineConfigRef := evaluationArtifactRef(baselineID+"-config", runmodel.ContractConfigBundle)
		variantConfigRef := evaluationArtifactRef(variantID+"-config", runmodel.ContractConfigBundle)
		reader.snapshots[baselineID] = runmodel.ExecutionSnapshot{ConfigBundleRef: baselineConfigRef}
		reader.snapshots[variantID] = runmodel.ExecutionSnapshot{
			ConfigBundleRef: variantConfigRef, ReplayChangeSetRef: &changeRef,
		}
		reader.configs[baselineConfigRef.URI] = reviewconfig.ConfigBundle{SHA256: baselineSHA}
		reader.configs[variantConfigRef.URI] = reviewconfig.ConfigBundle{SHA256: variantSHA}
		reader.runRefs[variantID] = evaluationArtifactRef(variantID+"-run", runmodel.ContractReviewRun)
		reader.changes[changeRef.URI] = runmodel.ReplayChangeSet{
			SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
			Namespace:     "namespace-" + variantID, SourceRunID: baselineID, RootRunID: baselineID,
			StartStage: "agent_hypothesize", Variable: runmodel.ReplayVariablePrompt,
			BaselineSHA256: baselineSHA, VariantSHA256: variantSHA,
			ChangedFields: []string{"agent_review.prompt"}, RemoteWrites: "deny", CreatedAt: testEpoch,
		}
		executor.runs[caseID] = variantID
		request.Cases = append(request.Cases, ExperimentBatchCase{
			CaseID: caseID, ExpectedLabelRevision: 1, BaselineReviewRunID: baselineID,
			ExpectedBaselineConfigSHA256: baselineSHA, ExpectedVariantConfigSHA256: variantSHA,
			ExposureObservations: testExposure("ignored", caseID, testEpoch, ExposureNotSeen).Observations,
		})
	}
	runner := &ExperimentBatchRunner{runs: reader, executor: executor}
	type outcome struct {
		results []ExperimentBatchCaseResult
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := runner.executeCases(context.Background(), request)
		done <- outcome{results: results, err: err}
	}()
	<-executor.started
	<-executor.started
	select {
	case third := <-executor.started:
		t.Fatalf("third case %q started before a bounded worker was released", third)
	default:
	}
	close(executor.release)
	result := <-done
	if result.err != nil {
		t.Fatalf("executeCases() error = %v", result.err)
	}
	if len(result.results) != 3 || result.results[0].CaseID != "case-1" ||
		result.results[1].CaseID != "case-2" || result.results[2].CaseID != "case-3" {
		t.Fatalf("executeCases() result order = %+v", result.results)
	}
	executor.mu.Lock()
	maxActive := executor.maxActive
	executor.mu.Unlock()
	if maxActive != 2 {
		t.Fatalf("max active executions = %d, want 2", maxActive)
	}
}
