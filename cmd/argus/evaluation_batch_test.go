package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/analytics"
	"argus.local/argus/internal/application"
	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/evaluation"
	feedbackdomain "argus.local/argus/internal/feedback"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/hailixexecution"
	"argus.local/argus/internal/platformapi"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestEvaluationBatchCLIExecutesFormalReplayAndIsExactIdempotent(t *testing.T) {
	const providerSecret = "argus-template-secret-must-not-persist"
	t.Setenv("ANTHROPIC_API_KEY", providerSecret)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worker); err != nil {
		t.Skipf("built Pi worker is unavailable: %v", err)
	}

	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n// TODO batch review\n")
	revision := commitCLITarget(t, repositoryPath, "evaluation batch target")
	storePath := t.TempDir()
	configState := t.TempDir()
	var reviewOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"review", "--repo", repositoryPath, "--mode", "selection",
		"--revision", revision, "--path", "review.go", "--start-line", "2", "--end-line", "2",
		"--context-provider", "go_ast", "--store", storePath, "--json",
	}, &reviewOutput); err != nil {
		t.Fatalf("create source review: %v", err)
	}
	var reviewed runOutput
	decodeCLIOutput(t, reviewOutput.Bytes(), &reviewed)
	if err := runWithIO(t.Context(), []string{
		"agent-review", "formal", "bootstrap", "--store", storePath,
		"--config-state-dir", configState, "--source-run", reviewed.Run.RunID,
		"--idempotency-key", "batch-formal-bootstrap", "--at", "2026-08-25T01:02:03Z",
		"--node", node, "--worker-script", worker, "--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat", "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("formal bootstrap: %v", err)
	}
	publishEvaluationBatchFindingGovernance(t, storePath, configState, reviewed.Run.RunID)
	runOptions, err := parseFormalAgentRunFlags([]string{
		"--store", storePath, "--config-state-dir", configState,
		"--source-run", reviewed.Run.RunID, "--idempotency-key", "batch-formal-baseline",
		"--node", node, "--worker-script", worker, "--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(runOptions.options)
	if err != nil {
		t.Fatal(err)
	}
	baselineWorker := &formalSucceededRunner{
		promptDigest: bootstrap.Manifest.Prompt.SHA256, confirmedFinding: true,
	}
	var formalOutput bytes.Buffer
	if err := executeFormalAgentRunWithRunner(t.Context(), runOptions, &formalOutput, baselineWorker); err != nil {
		t.Fatalf("formal baseline: %v\n%s", err, formalOutput.String())
	}
	var baseline formalAgentRunOutput
	decodeCLIOutput(t, formalOutput.Bytes(), &baseline)
	if baseline.ReviewRun == nil || baseline.Status != contractsv1alpha1.StageExecutionSucceeded {
		t.Fatalf("formal baseline output = %+v", baseline)
	}
	if baseline.Report == nil || len(baseline.Report.Findings) != 1 {
		t.Fatalf("formal baseline did not produce governed finding: %+v", baseline.Report)
	}

	state, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		t.Fatal(err)
	}
	baselineSnapshot, err := runs.ExecutionSnapshotForRun(baseline.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := runs.ReadJSONArtifact(baselineSnapshot.ReviewSpecRef, &spec); err != nil {
		t.Fatal(err)
	}
	frozen, err := loadFrozenFormalReplayConfig(runs, baseline.FormalRunID, baselineSnapshot,
		application.AgentPlanningSubject{
			TenantID: spec.TenantID, OrganizationID: "local", WorkspaceID: spec.WorkspaceID,
			RepositoryID: spec.Repository.RepositoryID,
		})
	if err != nil {
		t.Fatal(err)
	}
	variantBundle, _, err := formalreview.BuildBudgetReplayConfig(frozen.Bundle, frozen.Receipt, 120000)
	if err != nil {
		t.Fatal(err)
	}
	if baselineSnapshot.ConfigBundleRef.SHA256 == frozen.Bundle.SHA256 {
		t.Fatal("fixture unexpectedly collapsed config artifact digest and config self digest")
	}

	evaluationRepository, err := evaluation.New(state)
	if err != nil {
		t.Fatal(err)
	}
	caseAt := time.Now().UTC().Add(-10 * time.Minute)
	caseValue := testCLIEvaluationCase("batch-cli-case", "batch-cli-source", evaluation.SplitHoldout, caseAt)
	caseValue.InputSnapshotRef = baseline.ReviewRun.TargetSnapshotRef.URI
	roles := []evaluation.Role{evaluation.RoleHoldoutMaintainer, evaluation.RoleHoldoutRunner}
	governedImport := testCLIExternalGovernedImport(t, caseValue)
	registeredAt := caseAt.Add(-time.Minute)
	if _, err := evaluationRepository.RegisterGovernanceTrustKey(context.Background(), evaluation.GovernanceTrustKeyRegistration{
		SchemaVersion: evaluation.GovernanceTrustKeyRegistrationSchemaVersion,
		Key:           governedImport.TrustedKey, RegisteredAt: registeredAt,
	}, evaluation.Mutation{
		IdempotencyKey: "batch-cli-register-governance-key", Actor: "batch-cli-trust-admin",
		Roles: []evaluation.Role{evaluation.RoleGovernanceTrustAdmin},
		Audit: "register batch CLI governance key", At: registeredAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluationRepository.ImportGovernedCase(context.Background(), governedImport, evaluation.Mutation{
		IdempotencyKey: "batch-cli-create-case", Actor: "batch-cli-runner", Roles: roles,
		Audit: "create batch CLI holdout case", At: caseAt,
	}); err != nil {
		t.Fatal(err)
	}
	observations := []evaluation.ExposureObservation{
		{Component: evaluation.ExposurePrompt, Status: evaluation.ExposureNotSeen, Revision: "prompt-1"},
		{Component: evaluation.ExposureRule, Status: evaluation.ExposureNotSeen, Revision: "rule-1"},
		{Component: evaluation.ExposureModel, Status: evaluation.ExposureNotSeen, Revision: "model-1"},
		{Component: evaluation.ExposureIndex, Status: evaluation.ExposureNotSeen, Revision: "index-1"},
	}
	baselineEvaluationAt := caseAt.Add(2 * time.Minute)
	if _, err := evaluationRepository.RecordExposure(context.Background(), evaluation.Exposure{
		SchemaVersion: evaluation.ExposureSchemaVersion, EvaluationRunID: "batch-cli-baseline-evaluation",
		CaseID: caseValue.CaseID, Observations: observations, ObservedAt: baselineEvaluationAt.Add(-time.Minute),
	}, evaluation.Mutation{
		IdempotencyKey: "batch-cli-baseline-exposure", Actor: "batch-cli-runner", Roles: roles,
		Audit: "record baseline exposure", At: baselineEvaluationAt.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluationRepository.RecordEvaluationRun(context.Background(), evaluation.EvaluationRunRequest{
		SchemaVersion:   evaluation.EvaluationRunRequestSchemaVersion,
		EvaluationRunID: "batch-cli-baseline-evaluation", EvaluatorRevision: "presence-evaluator-1",
		Bindings: []evaluation.EvaluationCaseRunBinding{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1, ReviewRunID: baseline.FormalRunID,
		}}, CreatedAt: baselineEvaluationAt,
	}, evaluation.Mutation{
		IdempotencyKey: "batch-cli-record-baseline", Actor: "batch-cli-runner", Roles: roles,
		Audit: "record baseline evaluation", At: baselineEvaluationAt.Add(time.Minute),
	}, runs); err != nil {
		t.Fatalf("record baseline evaluation: %v", err)
	}

	batchAt := caseAt.Add(4 * time.Minute)
	batchRequest := evaluation.ExperimentBatchRequest{
		SchemaVersion: evaluation.ExperimentBatchRequestSchemaVersion, BatchID: "batch-cli-formal-replay",
		BaselineEvaluationRunID: "batch-cli-baseline-evaluation",
		VariantEvaluationRunID:  "batch-cli-variant-evaluation", ExperimentRunID: "batch-cli-experiment",
		ExperimentRevision: "batch-cli-experiment-1", Variable: runmodel.ReplayVariableBudget,
		ExecutorRevision: localFormalBatchExecutorRevision, MaxConcurrency: 1,
		Cases: []evaluation.ExperimentBatchCase{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1,
			BaselineReviewRunID:          baseline.FormalRunID,
			ExpectedBaselineConfigSHA256: frozen.Bundle.SHA256,
			ExpectedVariantConfigSHA256:  variantBundle.SHA256,
			ExposureObservations:         observations,
		}}, CreatedAt: batchAt,
	}
	requestPath := writeBatchJSON(t, "formal-batch-request.json", batchRequest)
	batchMutation := evaluation.Mutation{
		IdempotencyKey: "batch-cli-formal-intent", Actor: "batch-cli-runner", Roles: roles,
		Audit: "execute formal replay batch", At: batchAt,
	}
	mutationPath := writeBatchJSON(t, "formal-batch-mutation.json", batchMutation)
	arguments := []string{
		"--store", storePath, "--input", requestPath, "--mutation", mutationPath, "--json", "--",
		"--node", node, "--worker-script", worker, "--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "budget", "--timeout-ms", "120000",
	}
	replayWorker := &formalSucceededRunner{
		promptDigest: bootstrap.Manifest.Prompt.SHA256, confirmedFinding: true,
	}
	var first bytes.Buffer
	if err := runEvaluationBatchExecutionWithRunner(t.Context(), arguments, &first, replayWorker); err != nil {
		t.Fatalf("execute formal replay batch: %v\n%s", err, first.String())
	}
	var firstResult experimentBatchOutput
	decodeCLIOutput(t, first.Bytes(), &firstResult)
	if len(firstResult.Result.Cases) != 1 || firstResult.Result.ExperimentRunID != batchRequest.ExperimentRunID ||
		firstResult.Result.VariantEvaluationRunID != batchRequest.VariantEvaluationRunID || replayWorker.calls != 1 {
		t.Fatalf("formal batch result=%+v runner_calls=%d", firstResult, replayWorker.calls)
	}
	variantRun, err := runs.LoadRun(firstResult.Result.Cases[0].VariantReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if variantRun.SourceRunID != baseline.FormalRunID || variantRun.ReplayVariable != runmodel.ReplayVariableBudget {
		t.Fatalf("formal batch variant run = %+v", variantRun)
	}
	accessPath := writeBatchJSON(t, "formal-batch-access.json", evaluation.Access{
		Actor: "batch-cli-runner", Roles: roles,
	})
	var shown bytes.Buffer
	if err := runEvaluationBatch(t.Context(), []string{
		"show", "--store", storePath, "--batch", batchRequest.BatchID,
		"--access", accessPath, "--json",
	}, &shown); err != nil {
		t.Fatalf("show formal replay batch: %v", err)
	}
	var shownRecord experimentBatchRecordOutput
	decodeCLIOutput(t, shown.Bytes(), &shownRecord)
	if shownRecord.Record.Status != evaluation.ExperimentBatchSucceeded ||
		len(shownRecord.Record.CompletedCases) != 1 || shownRecord.Record.Result == nil ||
		shownRecord.Record.CompletedCases[0] != firstResult.Result.Cases[0] ||
		shownRecord.Record.Request.ExecutorTemplateRef == nil ||
		!reflect.DeepEqual(shownRecord.Record.Intent, batchMutation) {
		t.Fatalf("shown formal batch checkpoint closure = %+v", shownRecord.Record)
	}
	templateBytes, err := runs.ReadArtifact(*shownRecord.Record.Request.ExecutorTemplateRef)
	if err != nil {
		t.Fatalf("read frozen executor template: %v", err)
	}
	if bytes.Contains(templateBytes, []byte(providerSecret)) {
		t.Fatal("frozen executor template persisted provider credential bytes")
	}

	var retry bytes.Buffer
	if err := runEvaluationBatchExecutionWithRunner(t.Context(), arguments, &retry, replayWorker); err != nil {
		t.Fatalf("retry formal replay batch: %v", err)
	}
	if retry.String() != first.String() || replayWorker.calls != 1 {
		t.Fatalf("formal batch retry changed output or reran provider\nfirst=%s\nretry=%s\ncalls=%d",
			first.String(), retry.String(), replayWorker.calls)
	}
	var resumed bytes.Buffer
	if err := runEvaluationBatchResumeWithRunner(t.Context(), []string{
		"--store", storePath, "--batch", batchRequest.BatchID,
		"--access", accessPath, "--json",
	}, &resumed, replayWorker); err != nil {
		t.Fatalf("resume completed formal replay batch: %v", err)
	}
	if resumed.String() != first.String() || replayWorker.calls != 1 {
		t.Fatalf("formal batch resume changed output or reran provider\nfirst=%s\nresume=%s\ncalls=%d",
			first.String(), resumed.String(), replayWorker.calls)
	}

	asyncAt := batchAt.Add(2 * time.Minute)
	asyncRequest := batchRequest
	asyncRequest.BatchID = "batch-api-async-formal-replay"
	asyncRequest.VariantEvaluationRunID = "batch-api-async-variant-evaluation"
	asyncRequest.ExperimentRunID = "batch-api-async-experiment"
	asyncRequest.CreatedAt = asyncAt
	asyncRequest.ExecutorTemplateRef = shownRecord.Record.Request.ExecutorTemplateRef
	asyncIntent := evaluation.Mutation{
		IdempotencyKey: "batch-api-async-intent", Actor: "batch-cli-runner", Roles: roles,
		Audit: "admit async API experiment batch", At: asyncAt,
	}
	if _, err := evaluationRepository.BeginExperimentBatch(
		t.Context(), asyncRequest, asyncIntent,
	); err != nil {
		t.Fatalf("begin async API experiment batch: %v", err)
	}
	controller, err := newLocalEvaluationBatchController(
		t.Context(), evaluationRepository, runs, storePath, replayWorker,
	)
	if err != nil {
		t.Fatal(err)
	}
	resumeAt := time.Now().UTC()
	asyncResume := evaluation.Mutation{
		IdempotencyKey: "batch-api-async-resume", Actor: "batch-platform-operator", Roles: roles,
		Audit: "resume experiment batch through local platform", At: resumeAt,
	}
	accepted, err := controller.ResumeExperiment(
		t.Context(), asyncRequest.BatchID, asyncResume,
	)
	if err != nil || accepted.LastResume == nil || accepted.LastResume.Actor != asyncResume.Actor {
		t.Fatalf("ResumeExperiment() record=%+v error=%v", accepted, err)
	}
	controller.Wait()
	completedAsync, err := evaluationRepository.GetExperimentBatch(
		asyncRequest.BatchID, evaluation.Access{Actor: "batch-cli-runner", Roles: roles},
	)
	if err != nil || completedAsync.Status != evaluation.ExperimentBatchSucceeded ||
		completedAsync.Result == nil || replayWorker.calls != 2 ||
		completedAsync.Intent.Actor != asyncIntent.Actor || completedAsync.UpdatedBy != asyncIntent.Actor {
		t.Fatalf("async API experiment batch=%+v error=%v calls=%d",
			completedAsync, err, replayWorker.calls)
	}

	failingAt := asyncAt.Add(time.Minute)
	failingRequest := batchRequest
	failingRequest.BatchID = "batch-api-async-failure"
	failingRequest.VariantEvaluationRunID = "batch-api-failure-variant-evaluation"
	failingRequest.ExperimentRunID = "batch-api-failure-experiment"
	failingRequest.CreatedAt = failingAt
	failingRequest.ExecutorTemplateRef = shownRecord.Record.Request.ExecutorTemplateRef
	failingIntent := evaluation.Mutation{
		IdempotencyKey: "batch-api-failure-intent", Actor: "batch-cli-runner", Roles: roles,
		Audit: "admit failing async API experiment batch", At: failingAt,
	}
	if _, err := evaluationRepository.BeginExperimentBatch(
		t.Context(), failingRequest, failingIntent,
	); err != nil {
		t.Fatalf("begin failing async API experiment batch: %v", err)
	}
	failedWorker := &formalFailedRunner{}
	failingController, err := newLocalEvaluationBatchController(
		t.Context(), evaluationRepository, runs, storePath, failedWorker,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failingController.ResumeExperiment(t.Context(), failingRequest.BatchID, evaluation.Mutation{
		IdempotencyKey: "batch-api-failure-resume", Actor: "batch-platform-operator", Roles: roles,
		Audit: "resume failing batch through local platform", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("resume failing async API experiment batch: %v", err)
	}
	failingController.Wait()
	failedRecord, err := evaluationRepository.GetExperimentBatch(
		failingRequest.BatchID, evaluation.Access{Actor: "batch-cli-runner", Roles: roles},
	)
	if err != nil || failedRecord.Status != evaluation.ExperimentBatchRunning ||
		failedRecord.LastFailure == nil ||
		failedRecord.LastFailure.Code != evaluation.BatchAttemptExecutionFailed ||
		failedRecord.LastFailure.Generation != 1 || failedWorker.calls != 1 {
		t.Fatalf("failed async API experiment batch=%+v error=%v worker_calls=%d",
			failedRecord, err, failedWorker.calls)
	}
	failedRecordBytes, err := json.Marshal(failedRecord)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(failedRecordBytes, []byte("provider unavailable")) {
		t.Fatal("durable batch failure leaked raw provider error text")
	}

	repeatabilityAt := batchAt.Add(3 * time.Minute)
	repeatabilityRequest := evaluation.RepeatabilityBatchRequest{
		SchemaVersion: evaluation.RepeatabilityBatchRequestSchemaVersion,
		BatchID:       "batch-cli-repeatability", BaselineEvaluationRunID: "batch-cli-baseline-evaluation",
		ReplayEvaluationRunIDs: []string{"batch-cli-repeatability-evaluation"},
		RepeatabilityRunID:     "batch-cli-repeatability-run", RepeatabilityRevision: "stability-v1",
		ExecutorRevision: localFormalBatchExecutorRevision, MaxConcurrency: 1,
		Cases: []evaluation.RepeatabilityBatchCase{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1,
			BaselineReviewRunID:          baseline.FormalRunID,
			ExpectedBaselineConfigSHA256: frozen.Bundle.SHA256,
			ExposureObservations:         observations,
		}}, CreatedAt: repeatabilityAt,
	}
	repeatabilityRequestPath := writeBatchJSON(t, "repeatability-batch-request.json", repeatabilityRequest)
	repeatabilityMutation := evaluation.Mutation{
		IdempotencyKey: "batch-cli-repeatability-intent", Actor: "batch-cli-runner", Roles: roles,
		Audit: "execute exact replay repeatability batch", At: repeatabilityAt,
	}
	repeatabilityMutationPath := writeBatchJSON(t, "repeatability-batch-mutation.json", repeatabilityMutation)
	repeatabilityArguments := []string{
		"--store", storePath, "--input", repeatabilityRequestPath,
		"--mutation", repeatabilityMutationPath, "--json", "--",
		"--node", node, "--worker-script", worker, "--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
	}
	var repeatabilityOutput bytes.Buffer
	if err := runEvaluationRepeatabilityBatchExecutionWithRunner(
		t.Context(), repeatabilityArguments, &repeatabilityOutput, replayWorker,
	); err != nil {
		t.Fatalf("execute repeatability batch: %v\n%s", err, repeatabilityOutput.String())
	}
	var repeatabilityResult repeatabilityBatchOutput
	decodeCLIOutput(t, repeatabilityOutput.Bytes(), &repeatabilityResult)
	if repeatabilityResult.Result.RepeatabilityRunID != repeatabilityRequest.RepeatabilityRunID ||
		len(repeatabilityResult.Result.Cases) != 1 || replayWorker.calls != 3 {
		t.Fatalf("repeatability batch result=%+v runner_calls=%d", repeatabilityResult, replayWorker.calls)
	}
	var repeatabilityShown bytes.Buffer
	if err := runEvaluationRepeatabilityBatch(t.Context(), []string{
		"show", "--store", storePath, "--batch", repeatabilityRequest.BatchID,
		"--access", accessPath, "--json",
	}, &repeatabilityShown); err != nil {
		t.Fatalf("show repeatability batch: %v", err)
	}
	var repeatabilityRecord repeatabilityBatchRecordOutput
	decodeCLIOutput(t, repeatabilityShown.Bytes(), &repeatabilityRecord)
	if repeatabilityRecord.Record.Request.ExecutorTemplateRef == nil ||
		!reflect.DeepEqual(repeatabilityRecord.Record.Intent, repeatabilityMutation) {
		t.Fatalf("shown repeatability batch checkpoint closure = %+v", repeatabilityRecord.Record)
	}
	var repeatabilityResumed bytes.Buffer
	if err := runEvaluationRepeatabilityBatchResumeWithRunner(t.Context(), []string{
		"--store", storePath, "--batch", repeatabilityRequest.BatchID,
		"--access", accessPath, "--json",
	}, &repeatabilityResumed, replayWorker); err != nil {
		t.Fatalf("resume completed repeatability batch: %v", err)
	}
	if repeatabilityResumed.String() != repeatabilityOutput.String() || replayWorker.calls != 3 {
		t.Fatalf("repeatability resume changed output or reran provider\nfirst=%s\nresume=%s\ncalls=%d",
			repeatabilityOutput.String(), repeatabilityResumed.String(), replayWorker.calls)
	}

	// Platform submission freezes the API's configured formal profile, admits
	// intent before returning, and then executes on the service context. The
	// same caller command remains exactly idempotent and does not rerun Pi.
	platformController, err := newLocalEvaluationBatchController(
		t.Context(), evaluationRepository, runs, storePath, replayWorker, runOptions,
	)
	if err != nil {
		t.Fatal(err)
	}
	platformExperimentAt := batchAt.Add(4 * time.Minute)
	platformExperimentRequest := batchRequest
	platformExperimentRequest.BatchID = "batch-api-submit-budget"
	platformExperimentRequest.VariantEvaluationRunID = "batch-api-submit-budget-evaluation"
	platformExperimentRequest.ExperimentRunID = "batch-api-submit-budget-experiment"
	platformExperimentRequest.CreatedAt = platformExperimentAt
	platformExperimentRequest.ExecutorTemplateRef = nil
	platformExperimentMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-submit-budget-intent", Actor: "batch-platform-operator", Roles: roles,
		Audit: "submit budget experiment through local platform", At: platformExperimentAt,
	}
	platformBudgetVariant := platformapi.ExperimentBatchExecutionVariant{
		Variable: runmodel.ReplayVariableBudget, BudgetTimeoutMS: 120000,
	}
	admittedExperiment, err := platformController.SubmitExperiment(
		t.Context(), platformExperimentRequest, platformBudgetVariant, platformExperimentMutation,
	)
	if err != nil || admittedExperiment.Request.ExecutorTemplateRef == nil ||
		admittedExperiment.Status != evaluation.ExperimentBatchRunning ||
		admittedExperiment.Intent.Actor != platformExperimentMutation.Actor {
		t.Fatalf("platform budget admission=%+v error=%v", admittedExperiment, err)
	}
	platformController.Wait()
	platformExperiment, err := evaluationRepository.GetExperimentBatch(
		platformExperimentRequest.BatchID,
		evaluation.Access{Actor: platformExperimentMutation.Actor, Roles: roles},
	)
	if err != nil || platformExperiment.Status != evaluation.ExperimentBatchSucceeded ||
		platformExperiment.Result == nil || replayWorker.calls != 4 {
		t.Fatalf("platform budget batch=%+v error=%v runner_calls=%d",
			platformExperiment, err, replayWorker.calls)
	}
	retriedExperiment, err := platformController.SubmitExperiment(
		t.Context(), platformExperimentRequest, platformBudgetVariant, platformExperimentMutation,
	)
	if err != nil || retriedExperiment.Status != evaluation.ExperimentBatchSucceeded || replayWorker.calls != 4 {
		t.Fatalf("platform budget retry=%+v error=%v runner_calls=%d",
			retriedExperiment, err, replayWorker.calls)
	}

	governanceProfile, err := reviewconfig.SealCalibrationProfile(
		"batch-confidence", "2", []reviewconfig.CalibrationPoint{
			{RawPPM: 0, ConfidencePPM: 0},
			{RawPPM: 1_000_000, ConfidencePPM: 1_000_000},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	governancePolicy, err := reviewconfig.SealFindingGovernancePolicy(
		governanceProfile, 950_000, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	governanceBundle, _, err := formalreview.BuildFilterPolicyReplayConfig(
		frozen.Bundle, frozen.Receipt, governancePolicy,
	)
	if err != nil {
		t.Fatal(err)
	}
	governanceAt := time.Now().UTC()
	governanceRequest := batchRequest
	governanceRequest.Cases = append([]evaluation.ExperimentBatchCase{}, batchRequest.Cases...)
	governanceRequest.BatchID = "batch-api-submit-finding-governance"
	governanceRequest.VariantEvaluationRunID = "batch-api-submit-finding-governance-evaluation"
	governanceRequest.ExperimentRunID = "batch-api-submit-finding-governance-experiment"
	governanceRequest.ExperimentRevision = "finding-governance-v2"
	governanceRequest.Variable = runmodel.ReplayVariableFilterPolicy
	governanceRequest.CreatedAt = governanceAt
	governanceRequest.ExecutorTemplateRef = nil
	governanceRequest.Cases[0].ExpectedVariantConfigSHA256 = governanceBundle.SHA256
	governanceMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-submit-finding-governance-intent",
		Actor:          "batch-platform-operator", Roles: roles,
		Audit: "submit finding governance experiment through local platform", At: governanceAt,
	}
	governanceVariant := platformapi.ExperimentBatchExecutionVariant{
		Variable:          runmodel.ReplayVariableFilterPolicy,
		FindingGovernance: &governancePolicy,
	}
	governanceAdmission, err := platformController.SubmitExperiment(
		t.Context(), governanceRequest, governanceVariant, governanceMutation,
	)
	if err != nil || governanceAdmission.Request.ExecutorTemplateRef == nil {
		t.Fatalf("platform finding governance admission=%+v error=%v", governanceAdmission, err)
	}
	platformController.Wait()
	governanceBatch, err := evaluationRepository.GetExperimentBatch(
		governanceRequest.BatchID,
		evaluation.Access{Actor: governanceMutation.Actor, Roles: roles},
	)
	if err != nil || governanceBatch.Status != evaluation.ExperimentBatchSucceeded ||
		governanceBatch.Result == nil || len(governanceBatch.Result.Cases) != 1 || replayWorker.calls != 4 {
		_, evaluationErr := evaluationRepository.GetEvaluationRun(
			governanceRequest.VariantEvaluationRunID,
			evaluation.Access{Actor: governanceMutation.Actor, Roles: roles},
		)
		t.Fatalf("platform finding governance batch=%+v error=%v runner_calls=%d evaluation=%v",
			governanceBatch, err, replayWorker.calls, evaluationErr)
	}
	governanceRun, err := runs.LoadRun(governanceBatch.Result.Cases[0].VariantReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if governanceRun.ReplayFromStage != "finding_governance" ||
		governanceRun.ReplayVariable != runmodel.ReplayVariableFilterPolicy ||
		len(governanceRun.StageAttempts) != 0 || len(governanceRun.Bindings) != 0 ||
		governanceRun.AgentExecutionReceiptRef != nil ||
		governanceRun.GovernedReportRef == nil || baseline.ReviewRun.GovernedReportRef == nil ||
		*governanceRun.GovernedReportRef != *baseline.ReviewRun.GovernedReportRef ||
		governanceRun.SuppressionLedgerRef == nil ||
		*governanceRun.SuppressionLedgerRef == *baseline.ReviewRun.SuppressionLedgerRef {
		t.Fatalf("platform finding governance run closure=%+v", governanceRun)
	}
	governanceRetry, err := platformController.SubmitExperiment(
		t.Context(), governanceRequest, governanceVariant, governanceMutation,
	)
	if err != nil || governanceRetry.Status != evaluation.ExperimentBatchSucceeded || replayWorker.calls != 4 {
		t.Fatalf("platform finding governance retry=%+v error=%v runner_calls=%d",
			governanceRetry, err, replayWorker.calls)
	}

	platformRepeatabilityAt := platformExperimentAt.Add(time.Minute)
	platformRepeatabilityRequest := repeatabilityRequest
	platformRepeatabilityRequest.BatchID = "batch-api-submit-repeatability"
	platformRepeatabilityRequest.ReplayEvaluationRunIDs = []string{"batch-api-submit-repeatability-evaluation"}
	platformRepeatabilityRequest.RepeatabilityRunID = "batch-api-submit-repeatability-run"
	platformRepeatabilityRequest.CreatedAt = platformRepeatabilityAt
	platformRepeatabilityRequest.ExecutorTemplateRef = nil
	platformRepeatabilityMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-submit-repeatability-intent", Actor: "batch-platform-operator", Roles: roles,
		Audit: "submit exact repeatability through local platform", At: platformRepeatabilityAt,
	}
	admittedRepeatability, err := platformController.SubmitRepeatability(
		t.Context(), platformRepeatabilityRequest, platformRepeatabilityMutation,
	)
	if err != nil || admittedRepeatability.Request.ExecutorTemplateRef == nil ||
		admittedRepeatability.Status != evaluation.RepeatabilityBatchRunning {
		t.Fatalf("platform repeatability admission=%+v error=%v", admittedRepeatability, err)
	}
	platformController.Wait()
	platformRepeatability, err := evaluationRepository.GetRepeatabilityBatch(
		platformRepeatabilityRequest.BatchID,
		evaluation.Access{Actor: platformRepeatabilityMutation.Actor, Roles: roles},
	)
	if err != nil || platformRepeatability.Status != evaluation.RepeatabilityBatchSucceeded ||
		platformRepeatability.Result == nil || replayWorker.calls != 5 {
		t.Fatalf("platform repeatability batch=%+v error=%v runner_calls=%d",
			platformRepeatability, err, replayWorker.calls)
	}
	retriedRepeatability, err := platformController.SubmitRepeatability(
		t.Context(), platformRepeatabilityRequest, platformRepeatabilityMutation,
	)
	if err != nil || retriedRepeatability.Status != evaluation.RepeatabilityBatchSucceeded || replayWorker.calls != 5 {
		t.Fatalf("platform repeatability retry=%+v error=%v runner_calls=%d",
			retriedRepeatability, err, replayWorker.calls)
	}

	governedPrompt := contractsv1alpha1.DefaultAgentReviewPromptBundle()
	governedPrompt.Revision = "platform-prompt-v2"
	governedPrompt.ReviewSystemPrompt += "\nPrioritize state-machine invariant violations."
	governedPromptBytes, err := json.Marshal(governedPrompt)
	if err != nil {
		t.Fatal(err)
	}
	governedPromptDigest := sha256.Sum256(governedPromptBytes)
	governedPromptRef := reviewconfig.VersionedRef{
		ID: "pi-review-prompts", Revision: governedPrompt.Revision,
		SHA256: hex.EncodeToString(governedPromptDigest[:]),
	}
	promptPublishedAt := time.Now().UTC()
	componentPublisher, err := newLocalAgentComponentPublisher(state, runs)
	if err != nil {
		t.Fatal(err)
	}
	promptPublication, err := componentPublisher.PublishAgentComponent(
		t.Context(), platformapi.AgentComponentPublicationRequest{
			BaselineReviewRunID: baseline.FormalRunID,
			Contract:            contractsv1alpha1.AgentStagePlanPromptContract,
			Ref:                 governedPromptRef, Content: governedPromptBytes,
		}, platformapi.AgentComponentPublicationMutation{
			IdempotencyKey: "publish-platform-prompt-v2", Actor: "component-governor",
			Audit: "publish exact governed prompt for experiment", At: promptPublishedAt,
		},
	)
	if err != nil {
		t.Fatalf("publish governed prompt component: %v", err)
	}
	if promptPublication.PublishedBy != "component-governor" ||
		promptPublication.BaselineReviewRunID != baseline.FormalRunID {
		t.Fatalf("prompt publication lost principal or baseline binding: %+v", promptPublication)
	}
	governedPromptVariant := formalreview.LocalPiComponentVariant{
		SchemaVersion: formalreview.LocalPiComponentVariantSchemaVersion,
		Variable:      runmodel.ReplayVariablePrompt,
		Components: []formalreview.LocalPiGovernedComponent{{
			Contract: contractsv1alpha1.AgentStagePlanPromptContract,
			Binding:  promptPublication.Binding,
			Content:  governedPromptBytes,
		}},
	}
	governedPromptBootstrap, err := formalreview.BuildLocalPiBootstrapVariant(
		runOptions.options, governedPromptVariant,
	)
	if err != nil || governedPromptBootstrap.Manifest.Prompt.SHA256 != governedPromptRef.SHA256 {
		t.Fatalf("build governed prompt bootstrap=%+v error=%v", governedPromptBootstrap.Manifest, err)
	}
	governedPromptConfig, _, err := formalreview.BuildPromptReplayConfig(
		frozen.Bundle, frozen.Receipt, governedPromptRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	promptBatchAt := time.Now().UTC()
	promptBatchRequest := batchRequest
	promptBatchRequest.BatchID = "batch-api-submit-governed-prompt"
	promptBatchRequest.VariantEvaluationRunID = "batch-api-governed-prompt-evaluation"
	promptBatchRequest.ExperimentRunID = "batch-api-governed-prompt-experiment"
	promptBatchRequest.ExperimentRevision = "governed-prompt-v2"
	promptBatchRequest.Variable = runmodel.ReplayVariablePrompt
	promptBatchRequest.Cases[0].ExpectedVariantConfigSHA256 = governedPromptConfig.SHA256
	promptBatchRequest.CreatedAt = promptBatchAt
	promptBatchRequest.ExecutorTemplateRef = nil
	promptBatchMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-governed-prompt-intent", Actor: "batch-platform-operator", Roles: roles,
		Audit: "submit governed prompt experiment through local platform", At: promptBatchAt,
	}
	promptVariant := platformapi.ExperimentBatchExecutionVariant{
		Variable:      runmodel.ReplayVariablePrompt,
		ComponentRefs: []reviewconfig.VersionedRef{governedPromptRef},
	}
	replayWorker.promptDigest = governedPromptRef.SHA256
	admittedPrompt, err := platformController.SubmitExperiment(
		t.Context(), promptBatchRequest, promptVariant, promptBatchMutation,
	)
	if err != nil || admittedPrompt.Request.ExecutorTemplateRef == nil {
		t.Fatalf("platform governed prompt admission=%+v error=%v", admittedPrompt, err)
	}
	platformController.Wait()
	completedPrompt, err := evaluationRepository.GetExperimentBatch(
		promptBatchRequest.BatchID, evaluation.Access{Actor: promptBatchMutation.Actor, Roles: roles},
	)
	if err != nil || completedPrompt.Status != evaluation.ExperimentBatchSucceeded ||
		completedPrompt.Result == nil || replayWorker.calls != 6 {
		t.Fatalf("platform governed prompt batch=%+v error=%v runner_calls=%d",
			completedPrompt, err, replayWorker.calls)
	}
	if _, err := platformController.SubmitExperiment(
		t.Context(), promptBatchRequest, promptVariant, promptBatchMutation,
	); err != nil || replayWorker.calls != 6 {
		t.Fatalf("platform governed prompt retry error=%v runner_calls=%d", err, replayWorker.calls)
	}
	missingPromptRequest := promptBatchRequest
	missingPromptRequest.BatchID = "batch-api-missing-governed-prompt"
	missingPromptRequest.VariantEvaluationRunID = "batch-api-missing-prompt-evaluation"
	missingPromptRequest.ExperimentRunID = "batch-api-missing-prompt-experiment"
	missingPromptRequest.CreatedAt = time.Now().UTC()
	missingPromptRef := governedPromptRef
	missingPromptRef.Revision = "missing-prompt-v3"
	missingPromptRef.SHA256 = strings.Repeat("a", 64)
	_, err = platformController.SubmitExperiment(
		t.Context(), missingPromptRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable:      runmodel.ReplayVariablePrompt,
			ComponentRefs: []reviewconfig.VersionedRef{missingPromptRef},
		}, evaluation.Mutation{
			IdempotencyKey: "batch-api-missing-prompt-intent", Actor: "batch-platform-operator", Roles: roles,
			Audit: "reject unavailable governed prompt", At: missingPromptRequest.CreatedAt,
		},
	)
	if !errors.Is(err, evaluation.ErrInvalidTransition) || replayWorker.calls != 6 {
		t.Fatalf("missing governed prompt error=%v runner_calls=%d", err, replayWorker.calls)
	}

	governedSkillBytes := []byte("# Correctness\n\nReview state-machine invariants and prove every reported transition from exact code evidence.\n")
	governedSkillDigest := sha256.Sum256(governedSkillBytes)
	governedSkillRef := reviewconfig.VersionedRef{
		ID: "correctness", Revision: "platform-skill-v2",
		SHA256: hex.EncodeToString(governedSkillDigest[:]),
	}
	skillPublishedAt := time.Now().UTC()
	skillPublication, err := componentPublisher.PublishAgentComponent(
		t.Context(), platformapi.AgentComponentPublicationRequest{
			BaselineReviewRunID: baseline.FormalRunID,
			Contract:            contractsv1alpha1.AgentStagePlanSkillContract,
			Ref:                 governedSkillRef, Content: governedSkillBytes,
		}, platformapi.AgentComponentPublicationMutation{
			IdempotencyKey: "publish-platform-skill-v2", Actor: "component-governor",
			Audit: "publish exact governed review skill for experiment", At: skillPublishedAt,
		},
	)
	if err != nil {
		t.Fatalf("publish governed skill component: %v", err)
	}
	governedSkillVariant := formalreview.LocalPiComponentVariant{
		SchemaVersion: formalreview.LocalPiComponentVariantSchemaVersion,
		Variable:      runmodel.ReplayVariableSkillPack,
		Components: []formalreview.LocalPiGovernedComponent{{
			Contract: contractsv1alpha1.AgentStagePlanSkillContract,
			Binding:  skillPublication.Binding,
			Content:  governedSkillBytes,
		}},
	}
	governedSkillBootstrap, err := formalreview.BuildLocalPiBootstrapVariant(
		runOptions.options, governedSkillVariant,
	)
	if err != nil || governedSkillBootstrap.Manifest.ReviewSkills[1].SHA256 == bootstrap.Manifest.ReviewSkills[1].SHA256 {
		t.Fatalf("build governed skill bootstrap=%+v error=%v", governedSkillBootstrap.Manifest, err)
	}
	governedSkillConfig, _, err := formalreview.BuildSkillPackReplayConfig(
		frozen.Bundle, frozen.Receipt,
		governedSkillBootstrap.Revision.Patch.AgentReview.SkillPacks.Upsert,
	)
	if err != nil {
		t.Fatal(err)
	}
	skillBatchAt := time.Now().UTC()
	skillBatchRequest := batchRequest
	skillBatchRequest.BatchID = "batch-api-submit-governed-skill"
	skillBatchRequest.VariantEvaluationRunID = "batch-api-governed-skill-evaluation"
	skillBatchRequest.ExperimentRunID = "batch-api-governed-skill-experiment"
	skillBatchRequest.ExperimentRevision = "governed-skill-v2"
	skillBatchRequest.Variable = runmodel.ReplayVariableSkillPack
	skillBatchRequest.Cases[0].ExpectedVariantConfigSHA256 = governedSkillConfig.SHA256
	skillBatchRequest.CreatedAt = skillBatchAt
	skillBatchRequest.ExecutorTemplateRef = nil
	skillBatchMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-governed-skill-intent", Actor: "batch-platform-operator", Roles: roles,
		Audit: "submit governed review skill experiment through local platform", At: skillBatchAt,
	}
	replayWorker.promptDigest = bootstrap.Manifest.Prompt.SHA256
	admittedSkill, err := platformController.SubmitExperiment(
		t.Context(), skillBatchRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable:      runmodel.ReplayVariableSkillPack,
			ComponentRefs: []reviewconfig.VersionedRef{governedSkillRef},
		}, skillBatchMutation,
	)
	if err != nil || admittedSkill.Request.ExecutorTemplateRef == nil {
		t.Fatalf("platform governed skill admission=%+v error=%v", admittedSkill, err)
	}
	platformController.Wait()
	completedSkill, err := evaluationRepository.GetExperimentBatch(
		skillBatchRequest.BatchID, evaluation.Access{Actor: skillBatchMutation.Actor, Roles: roles},
	)
	if err != nil || completedSkill.Status != evaluation.ExperimentBatchSucceeded ||
		completedSkill.Result == nil || replayWorker.calls != 7 {
		t.Fatalf("platform governed skill batch=%+v error=%v runner_calls=%d",
			completedSkill, err, replayWorker.calls)
	}

	modelOptions := runOptions.options
	modelOptions.Model = "deepseek-reasoner"
	governedModelBootstrap, err := formalreview.BuildLocalPiBootstrap(modelOptions)
	if err != nil {
		t.Fatal(err)
	}
	governedModelRef := reviewconfig.VersionedRef{
		ID:       governedModelBootstrap.Manifest.Model.ID,
		Revision: governedModelBootstrap.Manifest.Model.Revision,
		SHA256:   governedModelBootstrap.Manifest.Model.SHA256,
	}
	governedModelConfig, _, err := formalreview.BuildModelReplayConfig(
		frozen.Bundle, frozen.Receipt, governedModelRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	modelBatchAt := time.Now().UTC()
	modelBatchRequest := batchRequest
	modelBatchRequest.BatchID = "batch-api-submit-exact-model"
	modelBatchRequest.VariantEvaluationRunID = "batch-api-model-evaluation"
	modelBatchRequest.ExperimentRunID = "batch-api-model-experiment"
	modelBatchRequest.ExperimentRevision = "deepseek-reasoner-v1"
	modelBatchRequest.Variable = runmodel.ReplayVariableModel
	modelBatchRequest.Cases[0].ExpectedVariantConfigSHA256 = governedModelConfig.SHA256
	modelBatchRequest.CreatedAt = modelBatchAt
	modelBatchRequest.ExecutorTemplateRef = nil
	modelBatchMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-model-intent", Actor: "batch-platform-operator", Roles: roles,
		Audit: "submit exact model experiment through local platform", At: modelBatchAt,
	}
	admittedModel, err := platformController.SubmitExperiment(
		t.Context(), modelBatchRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable: runmodel.ReplayVariableModel, Model: modelOptions.Model,
		}, modelBatchMutation,
	)
	if err != nil || admittedModel.Request.ExecutorTemplateRef == nil {
		t.Fatalf("platform model admission=%+v error=%v", admittedModel, err)
	}
	platformController.Wait()
	completedModel, err := evaluationRepository.GetExperimentBatch(
		modelBatchRequest.BatchID, evaluation.Access{Actor: modelBatchMutation.Actor, Roles: roles},
	)
	if err != nil || completedModel.Status != evaluation.ExperimentBatchSucceeded ||
		completedModel.Result == nil || replayWorker.calls != 8 {
		t.Fatalf("platform model batch=%+v error=%v runner_calls=%d",
			completedModel, err, replayWorker.calls)
	}
	if _, err := platformController.SubmitExperiment(
		t.Context(), modelBatchRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable: runmodel.ReplayVariableModel, Model: modelOptions.Model,
		}, modelBatchMutation,
	); err != nil || replayWorker.calls != 8 {
		t.Fatalf("platform model retry error=%v runner_calls=%d", err, replayWorker.calls)
	}

	workflowDefinition := workflow.FormalAgentReviewDefinition()
	workflowDefinition.Revision = "batch-stage-budget-v2"
	workflowDefinition.Stages = append([]workflow.Stage{}, workflowDefinition.Stages...)
	workflowDefinition.Stages[0].Budget.TimeoutMS = 60_000
	workflowDefinition.Stages[0].Budget.MaxConcurrency = 2
	workflowConfig, _, _, err := formalreview.BuildWorkflowReplayConfig(
		frozen.Bundle,
		frozen.Receipt,
		workflow.FormalAgentReviewDefinition(),
		workflowDefinition,
	)
	if err != nil {
		t.Fatal(err)
	}
	workflowBatchAt := time.Now().UTC()
	workflowBatchRequest := batchRequest
	workflowBatchRequest.BatchID = "batch-api-submit-workflow"
	workflowBatchRequest.VariantEvaluationRunID = "batch-api-workflow-evaluation"
	workflowBatchRequest.ExperimentRunID = "batch-api-workflow-experiment"
	workflowBatchRequest.ExperimentRevision = "stage-budget-v2"
	workflowBatchRequest.Variable = runmodel.ReplayVariableWorkflow
	workflowBatchRequest.Cases[0].ExpectedVariantConfigSHA256 = workflowConfig.SHA256
	workflowBatchRequest.CreatedAt = workflowBatchAt
	workflowBatchRequest.ExecutorTemplateRef = nil
	workflowBatchMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-workflow-intent", Actor: "batch-platform-operator", Roles: roles,
		Audit: "submit effective workflow budget experiment through local platform", At: workflowBatchAt,
	}
	replayWorker.expectedTimeoutMS = 60_000
	replayWorker.expectedConcurrency = 2
	admittedWorkflow, err := platformController.SubmitExperiment(
		t.Context(), workflowBatchRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable: runmodel.ReplayVariableWorkflow, WorkflowDefinition: &workflowDefinition,
		}, workflowBatchMutation,
	)
	if err != nil || admittedWorkflow.Request.ExecutorTemplateRef == nil {
		t.Fatalf("platform workflow admission=%+v error=%v", admittedWorkflow, err)
	}
	platformController.Wait()
	completedWorkflow, err := evaluationRepository.GetExperimentBatch(
		workflowBatchRequest.BatchID,
		evaluation.Access{Actor: workflowBatchMutation.Actor, Roles: roles},
	)
	if err != nil || completedWorkflow.Status != evaluation.ExperimentBatchSucceeded ||
		completedWorkflow.Result == nil || len(completedWorkflow.Result.Cases) != 1 ||
		replayWorker.calls != 9 {
		t.Fatalf("platform workflow batch=%+v error=%v runner_calls=%d",
			completedWorkflow, err, replayWorker.calls)
	}
	workflowRun, err := runs.LoadRun(completedWorkflow.Result.Cases[0].VariantReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	workflowSnapshot, err := runs.ExecutionSnapshotForRun(workflowRun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if workflowRun.ReplayVariable != runmodel.ReplayVariableWorkflow ||
		workflowSnapshot.WorkflowDefinitionRef == baselineSnapshot.WorkflowDefinitionRef ||
		workflowSnapshot.BuildIdentity != baselineSnapshot.BuildIdentity ||
		!reflect.DeepEqual(workflowSnapshot.ToolPolicy, baselineSnapshot.ToolPolicy) {
		t.Fatalf("platform workflow run=%+v snapshot=%+v", workflowRun, workflowSnapshot)
	}
	if _, err := platformController.SubmitExperiment(
		t.Context(), workflowBatchRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable: runmodel.ReplayVariableWorkflow, WorkflowDefinition: &workflowDefinition,
		}, workflowBatchMutation,
	); err != nil || replayWorker.calls != 9 {
		t.Fatalf("platform workflow retry error=%v runner_calls=%d", err, replayWorker.calls)
	}
	replayWorker.expectedTimeoutMS = 0
	replayWorker.expectedConcurrency = 0

	indexProviders, err := application.InvocationContextProviderDefinitions(
		[]string{"repository_search"},
	)
	if err != nil {
		t.Fatal(err)
	}
	indexConfig, _, err := formalreview.BuildIndexReplayConfig(
		frozen.Bundle, frozen.Receipt, indexProviders,
	)
	if err != nil {
		t.Fatal(err)
	}
	indexBatchAt := time.Now().UTC()
	indexBatchRequest := batchRequest
	indexBatchRequest.BatchID = "batch-api-submit-index"
	indexBatchRequest.VariantEvaluationRunID = "batch-api-index-evaluation"
	indexBatchRequest.ExperimentRunID = "batch-api-index-experiment"
	indexBatchRequest.ExperimentRevision = "repository-search-v1"
	indexBatchRequest.Variable = runmodel.ReplayVariableIndex
	indexBatchRequest.Cases[0].ExpectedVariantConfigSHA256 = indexConfig.SHA256
	indexBatchRequest.CreatedAt = indexBatchAt
	indexBatchRequest.ExecutorTemplateRef = nil
	indexBatchMutation := evaluation.Mutation{
		IdempotencyKey: "batch-api-index-intent", Actor: "batch-platform-operator", Roles: roles,
		Audit: "submit governed index experiment through local platform", At: indexBatchAt,
	}
	replayWorker.expectedContextKinds = []string{"repository_search"}
	admittedIndex, err := platformController.SubmitExperiment(
		t.Context(), indexBatchRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable: runmodel.ReplayVariableIndex, ContextProviders: indexProviders,
		}, indexBatchMutation,
	)
	if err != nil || admittedIndex.Request.ExecutorTemplateRef == nil {
		t.Fatalf("platform index admission=%+v error=%v", admittedIndex, err)
	}
	platformController.Wait()
	completedIndex, err := evaluationRepository.GetExperimentBatch(
		indexBatchRequest.BatchID,
		evaluation.Access{Actor: indexBatchMutation.Actor, Roles: roles},
	)
	if err != nil || completedIndex.Status != evaluation.ExperimentBatchSucceeded ||
		completedIndex.Result == nil || len(completedIndex.Result.Cases) != 1 ||
		replayWorker.calls != 10 {
		t.Fatalf("platform index batch=%+v error=%v runner_calls=%d",
			completedIndex, err, replayWorker.calls)
	}
	indexRun, err := runs.LoadRun(completedIndex.Result.Cases[0].VariantReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	indexSnapshot, err := runs.ExecutionSnapshotForRun(indexRun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if indexRun.ReplayVariable != runmodel.ReplayVariableIndex ||
		indexRun.ReplayFromStage != "materialize_target" ||
		indexSnapshot.TargetSnapshotRef == baselineSnapshot.TargetSnapshotRef ||
		indexSnapshot.ReviewInputRef == baselineSnapshot.ReviewInputRef ||
		len(indexSnapshot.ContextProviderReceiptRefs) != 1 {
		t.Fatalf("platform index run=%+v snapshot=%+v", indexRun, indexSnapshot)
	}
	if _, err := platformController.SubmitExperiment(
		t.Context(), indexBatchRequest, platformapi.ExperimentBatchExecutionVariant{
			Variable: runmodel.ReplayVariableIndex, ContextProviders: indexProviders,
		}, indexBatchMutation,
	); err != nil || replayWorker.calls != 10 {
		t.Fatalf("platform index retry error=%v runner_calls=%d", err, replayWorker.calls)
	}

	formalFindingID := baseline.Report.Findings[0].FindingID
	var findingOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"finding", "show", "--store", storePath, "--run", baseline.FormalRunID,
		"--finding", formalFindingID, "--json",
	}, &findingOutput); err != nil {
		t.Fatalf("show formal governed finding: %v", err)
	}
	var shownFinding findingShowOutput
	decodeCLIOutput(t, findingOutput.Bytes(), &shownFinding)
	if shownFinding.Finding != nil || shownFinding.GovernedFinding == nil ||
		shownFinding.GovernedFinding.FindingID != formalFindingID ||
		len(shownFinding.GovernedDecisions) != 1 {
		t.Fatalf("formal finding CLI output = %+v", shownFinding)
	}
	feedbackAt := batchAt.Add(time.Minute)
	feedbackPath := writeBatchJSON(t, "formal-feedback.json", feedbackdomain.Feedback{
		SchemaVersion: feedbackdomain.FeedbackSchemaVersion,
		FeedbackID:    "batch-cli-formal-feedback", FindingID: formalFindingID,
		RunID: baseline.FormalRunID, Action: feedbackdomain.FeedbackAccept,
		Actor:      feedbackdomain.ActorRef{Kind: feedbackdomain.ActorHuman, ID: "batch-cli-reviewer"},
		Source:     feedbackdomain.Source{Kind: feedbackdomain.SourceUserInterface, ID: "argus-local-ui"},
		OccurredAt: feedbackAt, RecordedAt: feedbackAt,
		SourceRefs: []feedbackdomain.SourceRef{{
			Kind: feedbackdomain.SourceRefManualObservation, Authority: "argus-local",
			ID: "batch-cli-feedback-observation",
		}},
		IdempotencyKey: "batch-cli-formal-feedback-key",
	})
	if err := runWithIO(t.Context(), []string{
		"feedback", "record", "--store", storePath, "--input", feedbackPath, "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("record formal finding feedback: %v", err)
	}
	outcomeAt := feedbackAt.Add(time.Minute)
	outcomePath := writeBatchJSON(t, "formal-outcome.json", feedbackdomain.Outcome{
		SchemaVersion: feedbackdomain.OutcomeSchemaVersion,
		OutcomeID:     "batch-cli-formal-outcome", FindingID: formalFindingID,
		RunID: baseline.FormalRunID, State: feedbackdomain.OutcomeFixed,
		Actor:      feedbackdomain.ActorRef{Kind: feedbackdomain.ActorHuman, ID: "batch-cli-reviewer"},
		Source:     feedbackdomain.Source{Kind: feedbackdomain.SourceManual, ID: "argus-local-review"},
		OccurredAt: outcomeAt, RecordedAt: outcomeAt,
		Window: feedbackdomain.AttributionWindow{Start: feedbackAt, End: outcomeAt},
		SourceRefs: []feedbackdomain.SourceRef{{
			Kind: feedbackdomain.SourceRefChange, Authority: "local-git",
			ID: "batch-cli-fix-change", Revision: revision,
		}},
		IdempotencyKey: "batch-cli-formal-outcome-key",
	})
	if err := runWithIO(t.Context(), []string{
		"outcome", "record", "--store", storePath, "--input", outcomePath, "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("record formal finding outcome: %v", err)
	}

	// Close the user-visible loop in the same store: the committed formal
	// receipts must flow through EvaluationRun/ExperimentRun into an immutable
	// dashboard snapshot without a hand-built analytics fact fixture.
	dashboardStart := caseAt.Add(-time.Minute)
	dashboardEnd := batchAt.Add(time.Hour)
	var dashboardOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"dashboard", "rebuild", "--store", storePath,
		"--snapshot", "batch-cli-dashboard", "--tenant", spec.TenantID,
		"--organization", "local", "--repository", spec.Repository.RepositoryID,
		"--start", dashboardStart.Format(time.RFC3339Nano),
		"--end", dashboardEnd.Format(time.RFC3339Nano),
		"--built-at", dashboardEnd.Add(time.Minute).Format(time.RFC3339Nano), "--json",
	}, &dashboardOutput); err != nil {
		t.Fatalf("rebuild dashboard from formal experiment: %v\n%s", err, dashboardOutput.String())
	}
	var dashboard dashboardSnapshotOutput
	decodeCLIOutput(t, dashboardOutput.Bytes(), &dashboard)
	foundUsageDelta := false
	for _, tile := range dashboard.Snapshot.Dashboard.Tiles {
		if tile.MetricID != "experiment.total_tokens.delta" {
			continue
		}
		foundUsageDelta = tile.Availability == analytics.TileObserved &&
			tile.Value != nil && tile.Value.Amount == 0 && tile.Value.Unit == "tokens"
	}
	if !foundUsageDelta {
		t.Fatalf("formal evaluation usage did not reach dashboard: %+v",
			dashboard.Snapshot.Dashboard.Tiles)
	}
	foundRepeatability := false
	for _, tile := range dashboard.Snapshot.Dashboard.Tiles {
		if tile.MetricID == "repeatability.finding_set_pairwise_jaccard.observation" &&
			tile.Availability == analytics.TileObserved && tile.Value != nil &&
			tile.Value.Amount == 1_000_000 && tile.Value.Scale == 6 {
			foundRepeatability = true
		}
	}
	if !foundRepeatability || len(dashboard.Snapshot.Facts.Repeatability) == 0 {
		t.Fatalf("formal repeatability did not reach dashboard: facts=%+v tiles=%+v",
			dashboard.Snapshot.Facts.Repeatability, dashboard.Snapshot.Dashboard.Tiles)
	}
	if len(dashboard.Snapshot.Facts.FeedbackOutcomes) != 1 ||
		dashboard.Snapshot.Facts.FeedbackOutcomes[0].FindingID != formalFindingID ||
		dashboard.Snapshot.Facts.FeedbackOutcomes[0].Feedback != analytics.FeedbackAccepted ||
		dashboard.Snapshot.Facts.FeedbackOutcomes[0].Outcome != analytics.OutcomeFixed {
		t.Fatalf("formal feedback/outcome did not reach analytics snapshot: %+v",
			dashboard.Snapshot.Facts.FeedbackOutcomes)
	}
}

func TestFormalBatchExecutorTemplateFreezesHailixTransportWithoutCredential(t *testing.T) {
	const bearer = "batch-hailix-secret-must-not-persist"
	t.Setenv(hailixexecution.BearerTokenEnvironment, bearer)
	t.Setenv(hailixexecution.CredentialRevisionEnvironment, "credential-revision-7")
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	formal, err := parseFormalAgentReplayFlags([]string{
		"--store", t.TempDir(), "--source-formal-run", "source-placeholder",
		"--idempotency-key", "idempotency-placeholder", "--node", node,
		"--worker-script", worker, "--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat", "--input-micros-per-million", "1",
		"--output-micros-per-million", "2", "--max-bytes-per-input-token", "4",
		"--execution-backend", "hailix-http",
		"--hailix-base-url", "https://hailix.example.test/platform/",
		"--hailix-capability-verifier-id", "capability-verifier",
		"--hailix-capability-verifier-revision", "v1",
		"--hailix-capability-verifier-sha256", strings.Repeat("c", 64),
		"--hailix-callback-verifier-id", "callback-verifier",
		"--hailix-callback-verifier-revision", "v1",
		"--hailix-callback-verifier-sha256", strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := local.Open(formal.store)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := persistLocalFormalBatchExecutorTemplate(runs, formal)
	if err != nil {
		t.Fatal(err)
	}
	data, err := runs.ReadArtifact(ref)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(bearer)) || bytes.Contains(data, []byte("credential-revision-7")) {
		t.Fatal("frozen Hailix batch template persisted request-time credential")
	}
	executor, err := loadLocalFormalBatchExecutor(runs, formal.store, ref, &forbiddenFormalRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if executor.template.transport != formal.transport ||
		executor.template.transport.Backend != formalExecutionBackendHailixHTTP {
		t.Fatalf("loaded Hailix batch transport = %+v", executor.template.transport)
	}
}

func TestLocalFormalBatchExecutorRejectsTemplateMismatchBeforeProvider(t *testing.T) {
	executor := &localFormalBatchExecutor{
		templateRef: runmodel.ArtifactRef{URI: "artifact://admitted"},
		runner:      &formalSucceededRunner{},
	}
	worker := executor.runner.(*formalSucceededRunner)
	_, err := executor.ExecuteReplay(t.Context(), evaluation.ReplayCaseExecutionRequest{
		ExecutorRevision:    localFormalBatchExecutorRevision,
		ExecutorTemplateRef: runmodel.ArtifactRef{URI: "artifact://different"},
	})
	if err == nil || !strings.Contains(err.Error(), "differs from admitted intent") {
		t.Fatalf("ExecuteReplay(template mismatch) error = %v", err)
	}
	if worker.calls != 0 {
		t.Fatalf("template mismatch called provider %d times", worker.calls)
	}
}

func TestPlatformBatchSubmitRequiresConfiguredFormalProfileBeforeProvider(t *testing.T) {
	state, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := evaluation.New(state)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		t.Fatal(err)
	}
	worker := &formalSucceededRunner{}
	controller, err := newLocalEvaluationBatchController(
		t.Context(), repository, runs, state.Root(), worker,
	)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	request := evaluation.ExperimentBatchRequest{
		SchemaVersion: evaluation.ExperimentBatchRequestSchemaVersion,
		BatchID:       "platform-formal-disabled", BaselineEvaluationRunID: "baseline-evaluation",
		VariantEvaluationRunID: "variant-evaluation", ExperimentRunID: "experiment-run",
		ExperimentRevision: "budget-v1", Variable: runmodel.ReplayVariableBudget,
		ExecutorRevision: localFormalBatchExecutorRevision, MaxConcurrency: 1,
		Cases: []evaluation.ExperimentBatchCase{{
			CaseID: "case-1", ExpectedLabelRevision: 1, BaselineReviewRunID: "baseline-review",
			ExpectedBaselineConfigSHA256: strings.Repeat("e", 64),
			ExpectedVariantConfigSHA256:  strings.Repeat("f", 64),
			ExposureObservations: []evaluation.ExposureObservation{
				{Component: evaluation.ExposurePrompt, Status: evaluation.ExposureNotSeen, Revision: "prompt-1"},
				{Component: evaluation.ExposureRule, Status: evaluation.ExposureNotSeen, Revision: "rule-1"},
				{Component: evaluation.ExposureModel, Status: evaluation.ExposureNotSeen, Revision: "model-1"},
				{Component: evaluation.ExposureIndex, Status: evaluation.ExposureNotSeen, Revision: "index-1"},
			},
		}},
		CreatedAt: at,
	}
	_, err = controller.SubmitExperiment(t.Context(), request, platformapi.ExperimentBatchExecutionVariant{
		Variable: runmodel.ReplayVariableBudget, BudgetTimeoutMS: 120000,
	}, evaluation.Mutation{
		IdempotencyKey: "platform-formal-disabled-intent", Actor: "platform-operator",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
		Audit: "reject submit without formal profile", At: at,
	})
	if !errors.Is(err, evaluation.ErrInvalidTransition) ||
		!strings.Contains(err.Error(), "formal Pi profile is not configured") {
		t.Fatalf("SubmitBudgetExperiment(no formal profile) error = %v", err)
	}
	if worker.calls != 0 {
		t.Fatalf("formal-disabled submit called provider %d times", worker.calls)
	}
}

func TestEvaluationBatchCLIRejectsTemplateAuthorityAndVariableDrift(t *testing.T) {
	if err := runEvaluationBatchExecution(t.Context(), []string{"--store", t.TempDir()}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "flags are required after --") {
		t.Fatalf("runEvaluationBatchExecution(no separator) error = %v", err)
	}
	store := t.TempDir()
	createdAt := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	request := evaluation.ExperimentBatchRequest{
		SchemaVersion: evaluation.ExperimentBatchRequestSchemaVersion,
		BatchID:       "cli-batch", BaselineEvaluationRunID: "cli-baseline-evaluation",
		VariantEvaluationRunID: "cli-variant-evaluation", ExperimentRunID: "cli-experiment",
		ExperimentRevision: "cli-experiment-1", Variable: runmodel.ReplayVariablePrompt,
		ExecutorRevision: localFormalBatchExecutorRevision, MaxConcurrency: 1,
		Cases: []evaluation.ExperimentBatchCase{{
			CaseID: "cli-case", ExpectedLabelRevision: 1, BaselineReviewRunID: "cli-baseline-review",
			ExpectedBaselineConfigSHA256: strings.Repeat("a", 64),
			ExpectedVariantConfigSHA256:  strings.Repeat("b", 64),
			ExposureObservations: []evaluation.ExposureObservation{
				{Component: evaluation.ExposurePrompt, Status: evaluation.ExposureNotSeen, Revision: "prompt-2"},
				{Component: evaluation.ExposureRule, Status: evaluation.ExposureNotSeen, Revision: "rule-1"},
				{Component: evaluation.ExposureModel, Status: evaluation.ExposureNotSeen, Revision: "model-1"},
				{Component: evaluation.ExposureIndex, Status: evaluation.ExposureNotSeen, Revision: "index-1"},
			},
		}}, CreatedAt: createdAt,
	}
	requestPath := writeBatchJSON(t, "request.json", request)
	mutationPath := writeBatchJSON(t, "mutation.json", evaluation.Mutation{
		IdempotencyKey: "cli-batch-intent", Actor: "cli-runner",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "CLI batch test", At: createdAt,
	})
	base := []string{
		"--store", store, "--input", requestPath, "--mutation", mutationPath, "--",
	}
	forbidden := append(append([]string{}, base...), "--store", store)
	if err := runEvaluationBatchExecution(t.Context(), forbidden, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "must not set --store") {
		t.Fatalf("runEvaluationBatchExecution(forbidden authority) error = %v", err)
	}
	template := []string{
		"--node", filepath.Join(store, "node"), "--worker-script", filepath.Join(store, "worker.js"),
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--change", "budget", "--timeout-ms", "1000",
	}
	mismatch := append(append([]string{}, base...), template...)
	if err := runEvaluationBatchExecution(t.Context(), mismatch, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "template changes \"budget\", batch declares \"prompt\"") {
		t.Fatalf("runEvaluationBatchExecution(variable drift) error = %v", err)
	}

	request.ExecutorRevision = "untrusted-executor"
	requestPath = writeBatchJSON(t, "bad-executor-request.json", request)
	promptTemplate := []string{
		"--node", filepath.Join(store, "node"), "--worker-script", filepath.Join(store, "worker.js"),
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--change", "prompt",
		"--prompt-bundle", filepath.Join(store, "prompt.json"), "--at", "2026-08-25T10:00:00Z",
	}
	badExecutor := append([]string{
		"--store", store, "--input", requestPath, "--mutation", mutationPath, "--",
	}, promptTemplate...)
	if err := runEvaluationBatchExecution(t.Context(), badExecutor, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "executor_revision must be") {
		t.Fatalf("runEvaluationBatchExecution(executor drift) error = %v", err)
	}
}

func TestEvaluationHelpListsBatchCommands(t *testing.T) {
	var output bytes.Buffer
	if err := runWithIO(t.Context(), []string{"evaluation", "--help"}, &output); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"evaluation batch run", "evaluation batch show",
		"evaluation repeatability batch run", "evaluation repeatability batch show",
	} {
		if !strings.Contains(output.String(), command) {
			t.Fatalf("evaluation help omitted %q: %s", command, output.String())
		}
	}
}

func TestEvaluationRepeatabilityBatchCLIRejectsAuthorityVariableAndExecutorDrift(t *testing.T) {
	if err := runEvaluationRepeatabilityBatchExecution(
		t.Context(), []string{"--store", t.TempDir()}, &bytes.Buffer{},
	); err == nil || !strings.Contains(err.Error(), "flags are required after --") {
		t.Fatalf("runEvaluationRepeatabilityBatchExecution(no separator) error = %v", err)
	}
	store := t.TempDir()
	createdAt := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	observations := []evaluation.ExposureObservation{
		{Component: evaluation.ExposurePrompt, Status: evaluation.ExposureNotSeen, Revision: "prompt-1"},
		{Component: evaluation.ExposureRule, Status: evaluation.ExposureNotSeen, Revision: "rule-1"},
		{Component: evaluation.ExposureModel, Status: evaluation.ExposureNotSeen, Revision: "model-1"},
		{Component: evaluation.ExposureIndex, Status: evaluation.ExposureNotSeen, Revision: "index-1"},
	}
	request := evaluation.RepeatabilityBatchRequest{
		SchemaVersion: evaluation.RepeatabilityBatchRequestSchemaVersion,
		BatchID:       "repeatability-cli-batch", BaselineEvaluationRunID: "repeatability-cli-baseline",
		ReplayEvaluationRunIDs: []string{"repeatability-cli-replay"},
		RepeatabilityRunID:     "repeatability-cli-run", RepeatabilityRevision: "repeatability-cli-v1",
		ExecutorRevision: localFormalBatchExecutorRevision, MaxConcurrency: 1,
		Cases: []evaluation.RepeatabilityBatchCase{{
			CaseID: "repeatability-cli-case", ExpectedLabelRevision: 1,
			BaselineReviewRunID:          "repeatability-cli-baseline-review",
			ExpectedBaselineConfigSHA256: strings.Repeat("a", 64),
			ExposureObservations:         observations,
		}}, CreatedAt: createdAt,
	}
	requestPath := writeBatchJSON(t, "repeatability-request.json", request)
	mutationPath := writeBatchJSON(t, "repeatability-mutation.json", evaluation.Mutation{
		IdempotencyKey: "repeatability-cli-intent", Actor: "repeatability-cli-runner",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
		Audit: "CLI repeatability batch test", At: createdAt,
	})
	base := []string{"--store", store, "--input", requestPath, "--mutation", mutationPath, "--"}
	forbidden := append(append([]string{}, base...), "--store", store)
	if err := runEvaluationRepeatabilityBatchExecution(t.Context(), forbidden, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "must not set --store") {
		t.Fatalf("repeatability batch forbidden authority error = %v", err)
	}
	template := []string{
		"--node", filepath.Join(store, "node"), "--worker-script", filepath.Join(store, "worker.js"),
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
	}
	variant := append(append([]string{}, base...), append(template, "--change", "budget", "--timeout-ms", "1000")...)
	if err := runEvaluationRepeatabilityBatchExecution(t.Context(), variant, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "requires exact replay") {
		t.Fatalf("repeatability batch variable drift error = %v", err)
	}
	request.ExecutorRevision = "untrusted-executor"
	requestPath = writeBatchJSON(t, "repeatability-bad-executor.json", request)
	badExecutor := append([]string{
		"--store", store, "--input", requestPath, "--mutation", mutationPath, "--",
	}, template...)
	if err := runEvaluationRepeatabilityBatchExecution(t.Context(), badExecutor, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "executor_revision must be") {
		t.Fatalf("repeatability batch executor drift error = %v", err)
	}
}

func writeBatchJSON(t *testing.T, name string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func publishEvaluationBatchFindingGovernance(
	t *testing.T,
	storePath string,
	configState string,
	sourceRunID string,
) {
	t.Helper()
	profile, err := reviewconfig.SealCalibrationProfile(
		"batch-confidence", "1", []reviewconfig.CalibrationPoint{
			{RawPPM: 0, ConfidencePPM: 0},
			{RawPPM: 1_000_000, ConfidencePPM: 1_000_000},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	minimum, maximum := uint32(500_000), 8
	runStore, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(runStore)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runs.ExecutionSnapshotForRun(sourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		t.Fatal(err)
	}
	configStore, err := local.Open(configState)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := configrepo.New(configStore)
	if err != nil {
		t.Fatal(err)
	}
	revision := reviewconfig.Revision{
		SchemaVersion: reviewconfig.RevisionSchemaVersion,
		ID:            "batch-finding-governance", Revision: "1", Scope: reviewconfig.ScopeRepository,
		Selector: reviewconfig.Selector{
			TenantID: spec.TenantID, OrganizationID: "local",
			RepositoryID: spec.Repository.RepositoryID,
		},
		Patch: reviewconfig.ConfigPatch{FindingGovernance: &reviewconfig.FindingGovernancePatch{
			CalibrationProfile: &profile, MinimumConfidencePPM: &minimum, MaxFindings: &maximum,
		}},
	}
	at := time.Date(2026, 8, 25, 1, 3, 0, 0, time.UTC)
	mutation := func(suffix, audit string) configrepo.Mutation {
		return configrepo.Mutation{
			IdempotencyKey: "batch-finding-governance-" + suffix,
			Actor:          "batch-config", Audit: audit, At: at,
		}
	}
	if _, err := configs.Create(
		t.Context(), revision, mutation("create", "create batch finding governance"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.ValidateRevision(
		t.Context(), revision.ID, revision.Revision,
		mutation("validate", "validate batch finding governance"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.Publish(
		t.Context(), revision.ID, revision.Revision, configrepo.Rollout{Percentage: 100},
		mutation("publish", "publish batch finding governance"),
	); err != nil {
		t.Fatal(err)
	}
}
