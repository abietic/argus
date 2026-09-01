package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentshadow"
	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/hailixexecution"
	"github.com/abietic/argus/internal/pireviewmap"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestFormalAgentBootstrapEndToEndIsExactIdempotent(t *testing.T) {
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
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "go.mod", "module example.com/formal-context\n\ngo 1.26\n")
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n// TODO review me\n")
	revision := commitCLITarget(t, repositoryPath, "formal bootstrap target")
	const contextMarker = "ARGUS_FORMAL_FROZEN_CONTEXT_MARKER"
	contextPath := filepath.Join(t.TempDir(), "codegraph-context.json")
	if err := os.WriteFile(
		contextPath,
		[]byte(`{"calls":[{"caller":"Review","callee":"validate"}],"types":["Review func() error"],"marker":"`+contextMarker+`"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	configState := t.TempDir()
	var reviewOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"review", "--repo", repositoryPath, "--mode", "selection",
		"--revision", revision, "--path", "review.go", "--start-line", "2", "--end-line", "2",
		"--context-provider", "go_ast",
		"--context-provider", "go_compile",
		"--context-provider", "repository_search",
		"--context-file", "codegraph@Review=" + contextPath,
		"--store", store, "--json",
	}, &reviewOutput); err != nil {
		t.Fatalf("create source review: %v", err)
	}
	var reviewed runOutput
	if err := json.Unmarshal(reviewOutput.Bytes(), &reviewed); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"agent-review", "formal", "bootstrap",
		"--store", store, "--config-state-dir", configState,
		"--source-run", reviewed.Run.RunID,
		"--idempotency-key", "formal-bootstrap-e2e",
		"--at", "2026-08-25T01:02:03Z",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--json",
	}
	var first bytes.Buffer
	if err := runWithIO(t.Context(), arguments, &first); err != nil {
		t.Fatalf("formal bootstrap: %v", err)
	}
	var second bytes.Buffer
	if err := runWithIO(t.Context(), arguments, &second); err != nil {
		t.Fatalf("formal bootstrap exact retry: %v", err)
	}
	if first.String() != second.String() ||
		!strings.Contains(first.String(), `"components":[`) ||
		!strings.Contains(first.String(), `"config_id":"formal-local-pi"`) ||
		!strings.Contains(first.String(), `"runtime_file_manifest_ref":`) {
		t.Fatalf("bootstrap output changed or incomplete\nfirst=%s\nsecond=%s", first.String(), second.String())
	}
	variantArguments := slices.Clone(arguments)
	for index := range variantArguments {
		switch variantArguments[index] {
		case "formal-bootstrap-e2e":
			variantArguments[index] = "formal-bootstrap-e2e-budget-v2"
		case "2026-08-25T01:02:03Z":
			variantArguments[index] = "2026-08-25T01:03:03Z"
		}
	}
	variantArguments = append(variantArguments, "--max-target-bytes", "8388608")
	if err := runWithIO(t.Context(), variantArguments, &bytes.Buffer{}); err != nil {
		t.Fatalf("formal bootstrap with reused components and new config revision: %v", err)
	}

	runOptions, err := parseFormalAgentRunFlags([]string{
		"--store", store, "--config-state-dir", configState,
		"--source-run", reviewed.Run.RunID,
		"--idempotency-key", "formal-run-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1",
		"--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
		"--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &formalFailedRunner{}
	var formalOutput bytes.Buffer
	err = executeFormalAgentRunWithRunner(t.Context(), runOptions, &formalOutput, runner)
	if err == nil || !strings.Contains(err.Error(), "ended with failed") {
		t.Fatalf("formal failed run error = %v", err)
	}
	var formalResult formalAgentRunOutput
	if decodeErr := json.Unmarshal(formalOutput.Bytes(), &formalResult); decodeErr != nil {
		t.Fatalf("decode formal output: %v\n%s", decodeErr, formalOutput.String())
	}
	if formalResult.Status != contractsv1alpha1.StageExecutionFailed ||
		formalResult.Failure == nil || formalResult.FormalRunID == "" ||
		formalResult.ReviewRun == nil || formalResult.ReviewRun.Status != "failed" ||
		formalResult.FinalRunRef == nil || formalResult.RecoveredFinalRun ||
		formalResult.RuntimeFileManifestRef == nil ||
		formalResult.TerminalAuthority != "failed_result_accepted" ||
		formalResult.WorkloadState != "failed" || runner.calls != 1 {
		t.Fatalf("formal terminal output=%+v runner_calls=%d", formalResult, runner.calls)
	}

	var retriedOutput bytes.Buffer
	err = executeFormalAgentRunWithRunner(t.Context(), runOptions, &retriedOutput, runner)
	if err == nil || !strings.Contains(err.Error(), "ended with failed") {
		t.Fatalf("formal failed retry error = %v", err)
	}
	var retried formalAgentRunOutput
	if decodeErr := json.Unmarshal(retriedOutput.Bytes(), &retried); decodeErr != nil {
		t.Fatalf("decode retried formal output: %v\n%s", decodeErr, retriedOutput.String())
	}
	if retried.ExecutionID != formalResult.ExecutionID || !retried.RecoveredDispatch ||
		!retried.RecoveredAdmission || !retried.RecoveredFinalRun ||
		retried.FinalRunRef == nil || *retried.FinalRunRef != *formalResult.FinalRunRef ||
		retried.RuntimeFileManifestRef == nil ||
		*retried.RuntimeFileManifestRef != *formalResult.RuntimeFileManifestRef ||
		runner.calls != 1 {
		t.Fatalf("formal terminal retry=%+v runner_calls=%d", retried, runner.calls)
	}

	stateStore, err := local.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	jobRuns, err := runrepo.New(stateStore)
	if err != nil {
		t.Fatal(err)
	}
	jobRepository, err := reviewjob.NewRepository(stateStore)
	if err != nil {
		t.Fatal(err)
	}
	workloads, err := scheduling.NewRepository(stateStore, scheduling.DefaultLocalPolicy())
	if err != nil {
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
	jobExecutor := &localReviewJobExecutor{
		deterministic: reviewjob.ExecuteFunc(func(
			context.Context, reviewjob.Command,
		) (application.RunOutcome, error) {
			return application.RunOutcome{}, errors.New("deterministic executor must not run")
		}),
		runner: runner, storePath: store,
	}
	jobService, err := reviewjob.NewService(
		jobRepository, workloads, jobRuns, configs, jobExecutor,
		"formal-job-test-worker", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	formalAdmission, err := newLocalFormalAdmissionValidator(stateStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobService.ConfigureFormalProfile(reviewjob.FormalProfile{
		Options: runOptions.options, Pricing: runOptions.pricing,
		Admission: formalAdmission,
	}); err != nil {
		t.Fatal(err)
	}
	jobContext, cancelJobs := context.WithCancel(t.Context())
	defer cancelJobs()
	if err := jobService.Start(jobContext); err != nil {
		t.Fatal(err)
	}
	job, err := jobService.Submit(t.Context(), reviewjob.Request{
		SchemaVersion:    reviewjob.RequestSchemaVersion,
		ExecutionProfile: reviewjob.FormalPiExecutionProfile,
		SourceRunID:      reviewed.Run.RunID, ExecutionTimeoutSeconds: 300,
	}, reviewjob.Mutation{
		IdempotencyKey: "formal-review-job-e2e", Actor: "formal-job-test",
		Audit: "verify claimed formal Pi ReviewJob", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("submit formal ReviewJob: %v", err)
	}
	// The claimed path freezes and revalidates the complete formal runtime before
	// invoking the runner. Under a full-package build that preparation can exceed
	// ten seconds even though the in-memory runner returns immediately. Wait for
	// any terminal state so an unexpected success/cancellation still fails fast.
	deadline := time.Now().Add(30 * time.Second)
	for !isFormalWorkloadTerminal(job.Workload.State) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		job, err = jobService.Get(job.JobID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if job.Workload.State != scheduling.StateFailed || job.Run == nil ||
		job.Run.Status != runmodel.RunStatusFailed || runner.calls != 2 ||
		job.RunID != formalreview.FormalRunID(reviewed.Run.RunID, "formal-review-job-e2e") ||
		job.Workload.Spec.WorkloadID != job.RunID+"-workload" {
		t.Fatalf("formal ReviewJob did not use one claimed workload: job=%+v runner_calls=%d", job, runner.calls)
	}

	showFlags, err := parseFormalAgentShowFlags([]string{
		"--store", store, "--formal-run", formalResult.FormalRunID, "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	var shownOutput bytes.Buffer
	err = executeFormalAgentShow(t.Context(), showFlags, &shownOutput)
	if err == nil || !strings.Contains(err.Error(), "ended with failed") {
		t.Fatalf("formal show failed terminal error = %v", err)
	}
	var shown formalAgentRunOutput
	if decodeErr := json.Unmarshal(shownOutput.Bytes(), &shown); decodeErr != nil {
		t.Fatalf("decode shown formal output: %v\n%s", decodeErr, shownOutput.String())
	}
	if shown.FormalRunID != formalResult.FormalRunID ||
		shown.ExecutionID != formalResult.ExecutionID || !shown.RecoveredAdmission ||
		!shown.RecoveredFinalRun || shown.ReviewRun == nil || shown.FinalRunRef == nil ||
		*shown.FinalRunRef != *formalResult.FinalRunRef || shown.RuntimeFileManifestRef == nil ||
		*shown.RuntimeFileManifestRef != *formalResult.RuntimeFileManifestRef {
		t.Fatalf("formal shown terminal = %+v", shown)
	}

	// A worker-level failure has no task diagnostics, while a valid report that
	// fails formal coverage must retain its task-level evidence and receipts in
	// the non-succeeded terminal gate. Those diagnostics remain absent from the
	// failed ReviewRun business projection and require the governed evidence
	// disclosure path for exact bytes.
	diagnosticOptions := runOptions
	diagnosticOptions.idempotencyKey = "formal-run-diagnostic-e2e"
	diagnosticBootstrap, err := formalreview.BuildLocalPiBootstrap(diagnosticOptions.options)
	if err != nil {
		t.Fatal(err)
	}
	diagnosticRunner := &formalRetryThenSucceededRunner{
		succeeded: formalSucceededRunner{promptDigest: diagnosticBootstrap.Manifest.Prompt.SHA256},
	}
	var diagnosticOutput bytes.Buffer
	err = executeFormalAgentRunWithRunner(
		t.Context(), diagnosticOptions, &diagnosticOutput, diagnosticRunner,
	)
	if err == nil || !strings.Contains(err.Error(), "ended with failed") {
		t.Fatalf("formal diagnostic failure error = %v", err)
	}
	var diagnostic formalAgentRunOutput
	if decodeErr := json.Unmarshal(diagnosticOutput.Bytes(), &diagnostic); decodeErr != nil {
		t.Fatalf("decode formal diagnostic output: %v\n%s", decodeErr, diagnosticOutput.String())
	}
	if diagnostic.Status != contractsv1alpha1.StageExecutionFailed ||
		diagnostic.TaskEvidence == nil ||
		diagnostic.TaskEvidence.Scope != "terminal_failure_diagnostics" ||
		diagnostic.TaskEvidence.Diagnostics == nil ||
		diagnostic.TaskEvidence.Diagnostics.ReviewTasks == 0 ||
		diagnostic.TaskEvidence.Diagnostics.TasksFailed == 0 ||
		diagnostic.ReviewRun == nil || diagnostic.ReviewRun.AgentTaskEvidenceRef != nil ||
		diagnostic.ReviewRun.AgentExecutionReceiptRef != nil || diagnostic.Hypotheses != nil {
		t.Fatalf("formal failed diagnostic projection = %+v", diagnostic)
	}
	var diagnosticShownOutput bytes.Buffer
	showErr := executeFormalAgentShow(t.Context(), formalAgentShowFlags{
		store: store, formalRun: diagnostic.FormalRunID, json: true,
	}, &diagnosticShownOutput)
	if showErr == nil || !strings.Contains(showErr.Error(), "ended with failed") {
		t.Fatalf("show formal diagnostic failure error = %v", showErr)
	}
	var diagnosticShown formalAgentRunOutput
	if decodeErr := json.Unmarshal(diagnosticShownOutput.Bytes(), &diagnosticShown); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if diagnosticShown.TaskEvidence == nil ||
		diagnosticShown.TaskEvidence.Scope != "terminal_failure_diagnostics" ||
		diagnosticShown.TaskEvidence.Diagnostics == nil ||
		diagnosticShown.TaskEvidence.Diagnostics.TasksFailed == 0 {
		t.Fatalf("formal show lost failure diagnostics: %+v", diagnosticShown)
	}
	var diagnosticEvidenceOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"agent-review", "formal", "evidence", "read",
		"--store", store, "--formal-run", diagnostic.FormalRunID,
		"--request-id", "formal-failure-diagnostics-read-e2e",
		"--actor", "formal-incident-debugger", "--purpose", "incident_investigation",
		"--at", time.Now().UTC().Format(time.RFC3339Nano),
		"--acknowledge-sensitive-output", "--json",
	}, &diagnosticEvidenceOutput); err != nil {
		t.Fatalf("read formal failure diagnostics: %v", err)
	}
	if !strings.Contains(diagnosticEvidenceOutput.String(), `"evidence":`) ||
		!strings.Contains(diagnosticEvidenceOutput.String(), `"receipts":`) ||
		!strings.Contains(diagnosticEvidenceOutput.String(), `"failure_reason_code":"provider_error"`) ||
		!strings.Contains(diagnosticEvidenceOutput.String(), `"access_proof":`) {
		t.Fatalf("formal failure diagnostic read is incomplete: %s", diagnosticEvidenceOutput.String())
	}
	baselineProfile, err := reviewconfig.SealCalibrationProfile(
		"formal-filter-e2e", "baseline-v1",
		[]reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 1_000_000}},
	)
	if err != nil {
		t.Fatal(err)
	}
	baselineMinimum, baselineMaximum := uint32(500_000), 8
	sourceSnapshot, err := jobRuns.ExecutionSnapshotForRun(reviewed.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := jobRuns.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		t.Fatal(err)
	}
	governanceRevision := reviewconfig.Revision{
		SchemaVersion: reviewconfig.RevisionSchemaVersion,
		ID:            "formal-e2e-finding-governance", Revision: "1",
		Scope: reviewconfig.ScopeRepository,
		Selector: reviewconfig.Selector{
			TenantID: sourceSpec.TenantID, OrganizationID: "local",
			RepositoryID: sourceSpec.Repository.RepositoryID,
		},
		Patch: reviewconfig.ConfigPatch{FindingGovernance: &reviewconfig.FindingGovernancePatch{
			CalibrationProfile: &baselineProfile, MinimumConfidencePPM: &baselineMinimum,
			MaxFindings: &baselineMaximum,
		}},
	}
	governanceAt := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	governanceMutation := func(suffix, audit string) configrepo.Mutation {
		return configrepo.Mutation{
			IdempotencyKey: "formal-e2e-governance-" + suffix,
			Actor:          "formal-e2e-config", Audit: audit, At: governanceAt,
		}
	}
	if _, err := configs.Create(
		t.Context(), governanceRevision, governanceMutation("create", "create formal E2E finding governance"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.ValidateRevision(
		t.Context(), governanceRevision.ID, governanceRevision.Revision,
		governanceMutation("validate", "validate formal E2E finding governance"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.Publish(
		t.Context(), governanceRevision.ID, governanceRevision.Revision,
		configrepo.Rollout{Percentage: 100},
		governanceMutation("publish", "publish formal E2E finding governance"),
	); err != nil {
		t.Fatal(err)
	}

	successOptions := runOptions
	successOptions.idempotencyKey = "formal-success-e2e"
	bootstrap, err := formalreview.BuildLocalPiBootstrap(successOptions.options)
	if err != nil {
		t.Fatal(err)
	}
	successRunner := &formalSucceededRunner{
		promptDigest:         bootstrap.Manifest.Prompt.SHA256,
		contextMarker:        contextMarker,
		expectedContextKinds: []string{"compile", "repository_search"},
	}
	var successOutput bytes.Buffer
	if err := executeFormalAgentRunWithRunner(
		t.Context(), successOptions, &successOutput, successRunner,
	); err != nil {
		t.Fatalf("formal succeeded run: %v\n%s", err, successOutput.String())
	}
	var succeeded formalAgentRunOutput
	if err := json.Unmarshal(successOutput.Bytes(), &succeeded); err != nil {
		t.Fatalf("decode succeeded formal output: %v\n%s", err, successOutput.String())
	}
	if succeeded.Status != contractsv1alpha1.StageExecutionSucceeded ||
		succeeded.Hypotheses == nil || len(succeeded.Hypotheses.Hypotheses) != 0 ||
		succeeded.CandidateSet == nil || len(succeeded.CandidateSet.Candidates) != 0 ||
		succeeded.CandidateSetRef == nil ||
		succeeded.VerificationLedger == nil || len(succeeded.VerificationLedger.Facts) != 0 ||
		succeeded.VerificationLedgerRef == nil ||
		succeeded.CalibrationLedger == nil || len(succeeded.CalibrationLedger.Facts) != 0 ||
		succeeded.CalibrationLedgerRef == nil ||
		succeeded.SuppressionLedger == nil || len(succeeded.SuppressionLedger.Facts) != 0 ||
		succeeded.SuppressionLedgerRef == nil ||
		succeeded.Report == nil || len(succeeded.Report.Candidates) != 0 ||
		succeeded.ReportRef == nil || succeeded.MarkdownReportRef == nil ||
		succeeded.ReviewRun == nil || succeeded.ReviewRun.Status != "succeeded" ||
		succeeded.ReviewRun.HypothesisSetRef == nil ||
		succeeded.ReviewRun.RawCandidateCollectionRef == nil ||
		succeeded.TaskEvidence == nil || succeeded.TaskEvidence.State != "active" ||
		succeeded.TaskEvidence.ExportPolicy != "deny" ||
		succeeded.ReviewRun.CandidateSetRef == nil ||
		succeeded.ReviewRun.VerificationLedgerRef == nil ||
		succeeded.ReviewRun.CalibrationLedgerRef == nil ||
		succeeded.ReviewRun.SuppressionLedgerRef == nil ||
		succeeded.ReviewRun.GovernedReportRef == nil ||
		succeeded.ReviewRun.GovernedMarkdownRef == nil || succeeded.FinalRunRef == nil ||
		succeeded.RuntimeFileManifestRef == nil ||
		succeeded.RecoveredFinalRun ||
		succeeded.TerminalAuthority != "succeeded_result_accepted" ||
		succeeded.WorkloadState != "succeeded" || successRunner.calls != 1 {
		t.Fatalf("formal succeeded output=%+v runner_calls=%d", succeeded, successRunner.calls)
	}
	rawCandidateBytes, err := jobRuns.ReadArtifact(*succeeded.ReviewRun.RawCandidateCollectionRef)
	if err != nil {
		t.Fatalf("read committed raw candidate evidence: %v", err)
	}
	rawCandidates, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawCandidateBytes)
	if err != nil || rawCandidates.ExecutionID != succeeded.ExecutionID ||
		len(rawCandidates.RawCandidates) != 0 {
		t.Fatalf("committed raw candidate evidence = %+v, error=%v", rawCandidates, err)
	}
	var candidateList bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"candidate", "list", "--store", store, "--run", succeeded.FormalRunID, "--json",
	}, &candidateList); err != nil {
		t.Fatalf("list formal candidates: %v", err)
	}
	var listedCandidates candidateListOutput
	if err := json.Unmarshal(candidateList.Bytes(), &listedCandidates); err != nil {
		t.Fatal(err)
	}
	if listedCandidates.CandidateSetID != succeeded.CandidateSet.CandidateSetID ||
		listedCandidates.VerificationLedgerID != succeeded.VerificationLedger.VerificationLedgerID ||
		len(listedCandidates.Candidates) != 0 || len(listedCandidates.VerificationFacts) != 0 {
		t.Fatalf("formal candidate list=%+v", listedCandidates)
	}
	var successRetry bytes.Buffer
	if err := executeFormalAgentRunWithRunner(
		t.Context(), successOptions, &successRetry, successRunner,
	); err != nil {
		t.Fatalf("formal succeeded exact retry: %v\n%s", err, successRetry.String())
	}
	var recoveredSuccess formalAgentRunOutput
	if err := json.Unmarshal(successRetry.Bytes(), &recoveredSuccess); err != nil {
		t.Fatal(err)
	}
	if recoveredSuccess.ExecutionID != succeeded.ExecutionID ||
		!recoveredSuccess.RecoveredDispatch || !recoveredSuccess.RecoveredAdmission ||
		recoveredSuccess.CandidateSetRef == nil ||
		*recoveredSuccess.CandidateSetRef != *succeeded.CandidateSetRef ||
		recoveredSuccess.VerificationLedgerRef == nil ||
		*recoveredSuccess.VerificationLedgerRef != *succeeded.VerificationLedgerRef ||
		recoveredSuccess.CalibrationLedgerRef == nil ||
		*recoveredSuccess.CalibrationLedgerRef != *succeeded.CalibrationLedgerRef ||
		recoveredSuccess.SuppressionLedgerRef == nil ||
		*recoveredSuccess.SuppressionLedgerRef != *succeeded.SuppressionLedgerRef ||
		recoveredSuccess.ReportRef == nil || *recoveredSuccess.ReportRef != *succeeded.ReportRef ||
		recoveredSuccess.MarkdownReportRef == nil ||
		*recoveredSuccess.MarkdownReportRef != *succeeded.MarkdownReportRef ||
		recoveredSuccess.ReviewRun == nil ||
		recoveredSuccess.ReviewRun.RawCandidateCollectionRef == nil ||
		*recoveredSuccess.ReviewRun.RawCandidateCollectionRef !=
			*succeeded.ReviewRun.RawCandidateCollectionRef ||
		!recoveredSuccess.RecoveredFinalRun || recoveredSuccess.FinalRunRef == nil ||
		*recoveredSuccess.FinalRunRef != *succeeded.FinalRunRef ||
		successRunner.calls != 1 {
		t.Fatalf("formal succeeded retry=%+v runner_calls=%d", recoveredSuccess, successRunner.calls)
	}

	retryConfigState := t.TempDir()
	if err := runWithIO(t.Context(), []string{
		"agent-review", "formal", "bootstrap",
		"--store", store, "--config-state-dir", retryConfigState,
		"--source-run", reviewed.Run.RunID,
		"--idempotency-key", "formal-retry-bootstrap-e2e",
		"--at", "2026-08-25T02:30:00Z",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--max-attempts", "2", "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("bootstrap formal multi-attempt config: %v", err)
	}
	retryOptions, err := parseFormalAgentRunFlags([]string{
		"--store", store, "--config-state-dir", retryConfigState,
		"--source-run", reviewed.Run.RunID,
		"--idempotency-key", "formal-retry-success-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1",
		"--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
		"--max-attempts", "2", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	retryBootstrap, err := formalreview.BuildLocalPiBootstrap(retryOptions.options)
	if err != nil {
		t.Fatal(err)
	}
	retryRunner := &formalRetryThenSucceededRunner{
		succeeded: formalSucceededRunner{
			promptDigest:         retryBootstrap.Manifest.Prompt.SHA256,
			contextMarker:        contextMarker,
			expectedContextKinds: []string{"compile", "repository_search"},
		},
	}
	var retryOutput bytes.Buffer
	if err := executeFormalAgentRunWithRunner(
		t.Context(), retryOptions, &retryOutput, retryRunner,
	); err != nil {
		t.Fatalf("formal retry then success: %v\n%s", err, retryOutput.String())
	}
	var retriedSuccess formalAgentRunOutput
	if err := json.Unmarshal(retryOutput.Bytes(), &retriedSuccess); err != nil {
		t.Fatalf("decode formal retry success: %v\n%s", err, retryOutput.String())
	}
	if retriedSuccess.Status != contractsv1alpha1.StageExecutionSucceeded ||
		retriedSuccess.ReviewRun == nil || retriedSuccess.ReviewRun.Status != runmodel.RunStatusSucceeded ||
		retriedSuccess.FinalRunRef == nil || retriedSuccess.RecoveredFinalRun ||
		retryRunner.calls != 2 || retryRunner.succeeded.calls != 1 ||
		len(retryRunner.requests) != 2 {
		t.Fatalf(
			"formal retry success=%+v runner_calls=%d success_calls=%d requests=%d",
			retriedSuccess, retryRunner.calls, retryRunner.succeeded.calls,
			len(retryRunner.requests),
		)
	}
	retryRun := retriedSuccess.ReviewRun
	if len(retryRun.StageAttempts) != 2 || len(retryRun.Bindings) != 2 ||
		len(retryRun.Evidence) != 1 {
		t.Fatalf("formal retry history is incomplete: %+v", retryRun)
	}
	firstAttempt, secondAttempt := retryRun.StageAttempts[0], retryRun.StageAttempts[1]
	firstBinding, secondBinding := retryRun.Bindings[0], retryRun.Bindings[1]
	if firstAttempt.Attempt != 1 || firstAttempt.Status != runmodel.StageStatusFailed ||
		firstAttempt.ErrorCode != "provider_error" || !firstAttempt.Retryable ||
		secondAttempt.Attempt != 2 || secondAttempt.Status != runmodel.StageStatusSucceeded ||
		secondAttempt.Generation != firstAttempt.Generation+1 ||
		firstBinding.Attempt != 1 || secondBinding.Attempt != 2 ||
		firstBinding.Generation != firstAttempt.Generation ||
		secondBinding.Generation != secondAttempt.Generation ||
		firstBinding.FencingToken != secondBinding.FencingToken ||
		firstBinding.BindingID == secondBinding.BindingID ||
		firstBinding.IdempotencyKey == secondBinding.IdempotencyKey ||
		retryRun.Evidence[0].BindingID != secondBinding.BindingID {
		t.Fatalf(
			"formal retry coordinates are not exact: attempts=%+v bindings=%+v evidence=%+v",
			retryRun.StageAttempts, retryRun.Bindings, retryRun.Evidence,
		)
	}
	for index, request := range retryRunner.requests {
		binding := retryRun.Bindings[index]
		if request.Attempt != binding.Attempt || request.Generation != binding.Generation ||
			request.FencingToken != binding.FencingToken ||
			request.IdempotencyKey != binding.IdempotencyKey {
			t.Fatalf("worker request[%d] does not match committed binding: request=%+v binding=%+v", index, request, binding)
		}
	}
	retryWorkload, err := workloads.Get(retriedSuccess.FormalRunID + "-workload")
	if err != nil {
		t.Fatal(err)
	}
	if retryWorkload.State != scheduling.StateSucceeded || retryWorkload.LastLease == nil ||
		retryWorkload.Attempt != 1 || retryWorkload.Generation != 1 ||
		retryWorkload.LastLease.Attempt != 1 || retryWorkload.LastLease.Generation != 1 ||
		retryWorkload.FencingToken != firstBinding.FencingToken ||
		retryWorkload.LastLease.FencingToken != firstBinding.FencingToken {
		t.Fatalf("formal retry changed the outer workload lease: %+v", retryWorkload)
	}
	var retryRecoveredOutput bytes.Buffer
	if err := executeFormalAgentRunWithRunner(
		t.Context(), retryOptions, &retryRecoveredOutput, retryRunner,
	); err != nil {
		t.Fatalf("formal retry success exact terminal retry: %v\n%s", err, retryRecoveredOutput.String())
	}
	var recoveredRetriedSuccess formalAgentRunOutput
	if err := json.Unmarshal(retryRecoveredOutput.Bytes(), &recoveredRetriedSuccess); err != nil {
		t.Fatal(err)
	}
	if recoveredRetriedSuccess.ExecutionID != retriedSuccess.ExecutionID ||
		!recoveredRetriedSuccess.RecoveredDispatch ||
		!recoveredRetriedSuccess.RecoveredAdmission ||
		!recoveredRetriedSuccess.RecoveredFinalRun ||
		recoveredRetriedSuccess.FinalRunRef == nil ||
		*recoveredRetriedSuccess.FinalRunRef != *retriedSuccess.FinalRunRef ||
		retryRunner.calls != 2 {
		t.Fatalf(
			"formal retry terminal recovery=%+v runner_calls=%d",
			recoveredRetriedSuccess, retryRunner.calls,
		)
	}
	replayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-exact-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1",
		"--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	driftedReplayOptions := replayOptions
	driftedReplayOptions.model = "deepseek-reasoner"
	driftedReplayOptions.options.Model = "deepseek-reasoner"
	driftedReplayOptions.idempotencyKey = "formal-exact-replay-drift-e2e"
	driftedReplayRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), driftedReplayOptions, &bytes.Buffer{}, driftedReplayRunner,
	); err == nil || !strings.Contains(err.Error(), "formal Pi plan differs from the selected immutable runtime manifest") {
		t.Fatalf("formal replay runtime drift error=%v", err)
	}
	if driftedReplayRunner.calls != 0 {
		t.Fatalf("formal replay runtime drift invoked runner %d times", driftedReplayRunner.calls)
	}
	var replayOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), replayOptions, &replayOutput, replayRunner,
	); err != nil {
		t.Fatalf("formal exact replay: %v\n%s", err, replayOutput.String())
	}
	var replayed formalAgentRunOutput
	if err := json.Unmarshal(replayOutput.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.ReviewRun == nil || replayed.ReviewRun.Kind != "replay" ||
		replayed.ReviewRun.SourceRunID != succeeded.FormalRunID ||
		replayed.ReviewRun.ReplayFromStage != "agent_hypothesize" ||
		replayed.ReviewRun.ReplayVariable != "none" || replayed.FinalRunRef == nil ||
		replayed.Status != contractsv1alpha1.StageExecutionSucceeded || replayRunner.calls != 1 {
		t.Fatalf("formal exact replay=%+v runner_calls=%d", replayed, replayRunner.calls)
	}
	var replayRetry bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), replayOptions, &replayRetry, replayRunner,
	); err != nil {
		t.Fatalf("formal exact replay retry: %v\n%s", err, replayRetry.String())
	}
	var recoveredReplay formalAgentRunOutput
	if err := json.Unmarshal(replayRetry.Bytes(), &recoveredReplay); err != nil {
		t.Fatal(err)
	}
	if !recoveredReplay.RecoveredDispatch || !recoveredReplay.RecoveredAdmission ||
		!recoveredReplay.RecoveredFinalRun || recoveredReplay.FinalRunRef == nil ||
		*recoveredReplay.FinalRunRef != *replayed.FinalRunRef || replayRunner.calls != 1 {
		t.Fatalf("formal exact replay retry=%+v runner_calls=%d", recoveredReplay, replayRunner.calls)
	}
	if succeeded.ReviewRun == nil || succeeded.ReviewRun.CompletedAt == nil {
		t.Fatalf("formal index replay source omitted completion time: %+v", succeeded.ReviewRun)
	}
	indexCreatedAt := succeeded.ReviewRun.CompletedAt.Add(time.Minute).UTC()
	indexReplayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-index-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
		"--change", "index", "--context-provider", "go_dependencies",
		"--at", indexCreatedAt.Format(time.RFC3339Nano), "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	indexRunner := &formalSucceededRunner{
		promptDigest:  bootstrap.Manifest.Prompt.SHA256,
		contextMarker: contextMarker,
		expectedContextKinds: []string{
			"compile", "dependency", "repository_search",
		},
	}
	var indexOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), indexReplayOptions, &indexOutput, indexRunner,
	); err != nil {
		t.Fatalf("formal index replay: %v\n%s", err, indexOutput.String())
	}
	var indexReplayed formalAgentRunOutput
	if err := json.Unmarshal(indexOutput.Bytes(), &indexReplayed); err != nil {
		t.Fatal(err)
	}
	if indexReplayed.ReviewRun == nil ||
		indexReplayed.ReviewRun.ReplayVariable != runmodel.ReplayVariableIndex ||
		indexReplayed.ReviewRun.ReplayFromStage != string(reviewcore.StageMaterializeTarget) ||
		indexReplayed.ReviewRun.SourceRunID != succeeded.FormalRunID ||
		indexReplayed.Status != contractsv1alpha1.StageExecutionSucceeded ||
		indexRunner.calls != 1 {
		t.Fatalf("formal index replay=%+v runner_calls=%d", indexReplayed, indexRunner.calls)
	}
	indexSourceSnapshot, err := jobRuns.ExecutionSnapshotForRun(succeeded.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	indexSnapshot, err := jobRuns.ExecutionSnapshotForRun(indexReplayed.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if indexSnapshot.TargetSnapshotRef == indexSourceSnapshot.TargetSnapshotRef ||
		indexSnapshot.ReviewInputRef == indexSourceSnapshot.ReviewInputRef ||
		len(indexSnapshot.ContextProviderReceiptRefs) != 1 ||
		indexSnapshot.ReplayChangeSetRef == nil ||
		!slices.Equal(indexSnapshot.RuntimeEvidenceRefs, indexSourceSnapshot.RuntimeEvidenceRefs) {
		t.Fatalf("formal index snapshot=%+v source=%+v", indexSnapshot, indexSourceSnapshot)
	}
	var indexChange runmodel.ReplayChangeSet
	if err := jobRuns.ReadJSONArtifact(*indexSnapshot.ReplayChangeSetRef, &indexChange); err != nil {
		t.Fatal(err)
	}
	if indexChange.Variable != runmodel.ReplayVariableIndex ||
		indexChange.StartStage != string(reviewcore.StageMaterializeTarget) ||
		indexChange.BaselineSHA256 == indexChange.VariantSHA256 ||
		len(indexChange.ChangedFields) == 0 {
		t.Fatalf("formal index change=%+v", indexChange)
	}
	var indexRetry bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), indexReplayOptions, &indexRetry, indexRunner,
	); err != nil {
		t.Fatalf("formal index replay retry: %v", err)
	}
	if indexRunner.calls != 1 {
		t.Fatalf("formal index replay retry invoked runner %d times", indexRunner.calls)
	}
	var indexComparisonOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"compare", "--store", store, "--baseline", succeeded.FormalRunID,
		"--variant", indexReplayed.FormalRunID, "--json",
	}, &indexComparisonOutput); err != nil {
		t.Fatalf("compare formal index replay: %v", err)
	}
	var indexComparison runmodel.RunComparison
	if err := json.Unmarshal(indexComparisonOutput.Bytes(), &indexComparison); err != nil {
		t.Fatal(err)
	}
	if len(indexComparison.CandidatesAdded) != 0 ||
		len(indexComparison.CandidatesRemoved) != 0 {
		t.Fatalf("formal index comparison treated target digest as candidate identity: %+v", indexComparison)
	}
	chainedOptions := replayOptions
	chainedOptions.sourceFormalRun = replayed.FormalRunID
	chainedOptions.idempotencyKey = "formal-exact-replay-chain-e2e"
	chainedRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	var chainedOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), chainedOptions, &chainedOutput, chainedRunner,
	); err != nil {
		t.Fatalf("chained formal exact replay: %v\n%s", err, chainedOutput.String())
	}
	var chained formalAgentRunOutput
	if err := json.Unmarshal(chainedOutput.Bytes(), &chained); err != nil {
		t.Fatal(err)
	}
	if chained.ReviewRun == nil || chained.ReviewRun.SourceRunID != replayed.FormalRunID ||
		chained.ReviewRun.ReplayRootRunID != succeeded.FormalRunID || chainedRunner.calls != 1 {
		t.Fatalf("chained formal replay=%+v runner_calls=%d", chained, chainedRunner.calls)
	}
	budgetReplayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-budget-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
		"--change", "budget", "--timeout-ms", "120000", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	budgetReplayRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	var budgetReplayOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), budgetReplayOptions, &budgetReplayOutput, budgetReplayRunner,
	); err != nil {
		t.Fatalf("formal budget replay: %v\n%s", err, budgetReplayOutput.String())
	}
	var budgetReplayed formalAgentRunOutput
	if err := json.Unmarshal(budgetReplayOutput.Bytes(), &budgetReplayed); err != nil {
		t.Fatal(err)
	}
	if budgetReplayed.ReviewRun == nil ||
		budgetReplayed.ReviewRun.ReplayVariable != runmodel.ReplayVariableBudget ||
		budgetReplayed.ReviewRun.SourceRunID != succeeded.FormalRunID ||
		budgetReplayRunner.calls != 1 {
		t.Fatalf("formal budget replay=%+v runner_calls=%d", budgetReplayed, budgetReplayRunner.calls)
	}
	state, err := local.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	runRepository, err := runrepo.New(state)
	if err != nil {
		t.Fatal(err)
	}
	budgetSnapshot, err := runRepository.ExecutionSnapshotForRun(budgetReplayed.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if budgetSnapshot.ToolPolicy.PerCallTimeoutMS != 120000 ||
		budgetSnapshot.ReplayChangeSetRef == nil ||
		budgetSnapshot.RemoteWrites != "deny" ||
		budgetSnapshot.ToolPolicy.RemoteWrites != "deny" {
		t.Fatalf("formal budget snapshot=%+v", budgetSnapshot)
	}
	var budgetChange runmodel.ReplayChangeSet
	if err := runRepository.ReadJSONArtifact(*budgetSnapshot.ReplayChangeSetRef, &budgetChange); err != nil {
		t.Fatal(err)
	}
	if budgetChange.Variable != runmodel.ReplayVariableBudget ||
		!slices.Equal(budgetChange.ChangedFields, formalreview.FormalBudgetReplayChangedFields()) ||
		budgetChange.BaselineSHA256 == budgetChange.VariantSHA256 {
		t.Fatalf("formal budget change=%+v", budgetChange)
	}
	workflowDefinition := workflow.FormalAgentReviewDefinition()
	workflowDefinition.Revision = "stage-budget-e2e-v2"
	workflowDefinition.Stages = slices.Clone(workflowDefinition.Stages)
	workflowDefinition.Stages[0].Budget.TimeoutMS = 60_000
	workflowDefinition.Stages[0].Budget.MaxConcurrency = 2
	workflowData, err := json.Marshal(workflowDefinition)
	if err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(t.TempDir(), "formal-workflow-replay.json")
	if err := os.WriteFile(workflowPath, workflowData, 0o600); err != nil {
		t.Fatal(err)
	}
	workflowCreatedAt := succeeded.ReviewRun.CompletedAt.Add(30 * time.Second).UTC()
	workflowReplayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-workflow-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--change", "workflow",
		"--workflow-definition", workflowPath,
		"--at", workflowCreatedAt.Format(time.RFC3339Nano), "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	workflowRunner := &formalSucceededRunner{
		promptDigest:      bootstrap.Manifest.Prompt.SHA256,
		expectedTimeoutMS: 60_000, expectedConcurrency: 2,
	}
	var workflowOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), workflowReplayOptions, &workflowOutput, workflowRunner,
	); err != nil {
		t.Fatalf("formal workflow replay: %v\n%s", err, workflowOutput.String())
	}
	var workflowReplayed formalAgentRunOutput
	if err := json.Unmarshal(workflowOutput.Bytes(), &workflowReplayed); err != nil {
		t.Fatal(err)
	}
	if workflowReplayed.ReviewRun == nil ||
		workflowReplayed.ReviewRun.ReplayVariable != runmodel.ReplayVariableWorkflow ||
		workflowReplayed.ReviewRun.SourceRunID != succeeded.FormalRunID || workflowRunner.calls != 1 {
		t.Fatalf("formal workflow replay=%+v runner_calls=%d", workflowReplayed, workflowRunner.calls)
	}
	workflowSnapshot, err := runRepository.ExecutionSnapshotForRun(workflowReplayed.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if workflowSnapshot.WorkflowDefinitionRef == indexSourceSnapshot.WorkflowDefinitionRef ||
		workflowSnapshot.Workflow == indexSourceSnapshot.Workflow ||
		workflowSnapshot.BuildIdentity != indexSourceSnapshot.BuildIdentity ||
		!reflect.DeepEqual(workflowSnapshot.ToolPolicy, indexSourceSnapshot.ToolPolicy) ||
		workflowSnapshot.ReplayChangeSetRef == nil {
		t.Fatalf("formal workflow snapshot=%+v source=%+v", workflowSnapshot, indexSourceSnapshot)
	}
	var workflowChange runmodel.ReplayChangeSet
	if err := runRepository.ReadJSONArtifact(
		*workflowSnapshot.ReplayChangeSetRef, &workflowChange,
	); err != nil {
		t.Fatal(err)
	}
	wantWorkflowFields := []string{
		"workflow.stages.agent_hypothesize.budget.max_concurrency",
		"workflow.stages.agent_hypothesize.budget.timeout_ms",
	}
	if workflowChange.Variable != runmodel.ReplayVariableWorkflow ||
		!slices.Equal(workflowChange.ChangedFields, wantWorkflowFields) {
		t.Fatalf("formal workflow change=%+v", workflowChange)
	}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), workflowReplayOptions, &bytes.Buffer{}, workflowRunner,
	); err != nil {
		t.Fatalf("formal workflow replay retry: %v", err)
	}
	if workflowRunner.calls != 1 {
		t.Fatalf("formal workflow replay retry invoked runner %d times", workflowRunner.calls)
	}
	workflowExactOptions := replayOptions
	workflowExactOptions.sourceFormalRun = workflowReplayed.FormalRunID
	workflowExactOptions.idempotencyKey = "formal-workflow-exact-chain-e2e"
	workflowExactRunner := &formalSucceededRunner{
		promptDigest:      bootstrap.Manifest.Prompt.SHA256,
		expectedTimeoutMS: 60_000, expectedConcurrency: 2,
	}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), workflowExactOptions, &bytes.Buffer{}, workflowExactRunner,
	); err != nil {
		t.Fatalf("exact replay from workflow variant: %v", err)
	}
	if workflowExactRunner.calls != 1 {
		t.Fatalf("workflow exact chain runner calls=%d", workflowExactRunner.calls)
	}
	var budgetRetry bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), budgetReplayOptions, &budgetRetry, budgetReplayRunner,
	); err != nil {
		t.Fatalf("formal budget replay retry: %v", err)
	}
	if budgetReplayRunner.calls != 1 {
		t.Fatalf("formal budget replay retry invoked runner %d times", budgetReplayRunner.calls)
	}
	conflictingBudgetOptions := budgetReplayOptions
	conflictingBudgetOptions.timeoutMS = 100000
	conflictingBudgetRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), conflictingBudgetOptions, &bytes.Buffer{}, conflictingBudgetRunner,
	); err == nil || !strings.Contains(err.Error(), "formal budget replay") {
		t.Fatalf("conflicting formal budget replay error=%v", err)
	}
	if conflictingBudgetRunner.calls != 0 {
		t.Fatalf("conflicting formal budget replay invoked runner %d times", conflictingBudgetRunner.calls)
	}
	var comparisonOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"compare", "--store", store, "--baseline", succeeded.FormalRunID,
		"--variant", budgetReplayed.FormalRunID, "--json",
	}, &comparisonOutput); err != nil {
		t.Fatalf("compare formal budget replay: %v", err)
	}
	var comparison runmodel.RunComparison
	if err := json.Unmarshal(comparisonOutput.Bytes(), &comparison); err != nil {
		t.Fatal(err)
	}
	if comparison.BaselineRunID != succeeded.FormalRunID ||
		comparison.VariantRunID != budgetReplayed.FormalRunID ||
		len(comparison.Added) != 0 || len(comparison.Removed) != 0 ||
		len(comparison.CandidatesAdded) != 0 || len(comparison.CandidatesRemoved) != 0 ||
		len(comparison.StageLatency) == 0 ||
		comparison.StageLatency[len(comparison.StageLatency)-1].StageID != "agent_hypothesize" {
		t.Fatalf("formal comparison=%+v", comparison)
	}
	filterProfile, err := reviewconfig.SealCalibrationProfile(
		"formal-filter-e2e", "v1",
		[]reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 1_000_000}},
	)
	if err != nil {
		t.Fatal(err)
	}
	filterPolicy, err := reviewconfig.SealFindingGovernancePolicy(filterProfile, 700_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	filterData, err := json.Marshal(filterPolicy)
	if err != nil {
		t.Fatal(err)
	}
	filterPath := filepath.Join(t.TempDir(), "finding-governance.json")
	if err := os.WriteFile(filterPath, filterData, 0o600); err != nil {
		t.Fatal(err)
	}
	filterReplayAt := succeeded.ReviewRun.CompletedAt.Add(time.Second).UTC().Format(time.RFC3339Nano)
	filterReplayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-filter-policy-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--change", "filter_policy",
		"--finding-governance", filterPath, "--at", filterReplayAt, "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	filterRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	var filterOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), filterReplayOptions, &filterOutput, filterRunner,
	); err != nil {
		t.Fatalf("formal finding_governance replay: %v\n%s", err, filterOutput.String())
	}
	var filterReplayed formalAgentRunOutput
	if err := json.Unmarshal(filterOutput.Bytes(), &filterReplayed); err != nil {
		t.Fatal(err)
	}
	if filterReplayed.ReviewRun == nil ||
		filterReplayed.ReviewRun.ReplayVariable != runmodel.ReplayVariableFilterPolicy ||
		filterReplayed.ReviewRun.ReplayFromStage != "finding_governance" ||
		filterReplayed.SuppressionLedger == nil || !filterReplayed.SuppressionLedger.PolicyApplied ||
		filterReplayed.SuppressionLedger.Policy == nil ||
		filterReplayed.SuppressionLedger.Policy.SHA256 != filterPolicy.SHA256 ||
		filterRunner.calls != 0 {
		t.Fatalf("formal finding_governance replay=%+v runner_calls=%d", filterReplayed, filterRunner.calls)
	}
	if filterReplayed.ReviewRun.RawCandidateCollectionRef != nil ||
		filterReplayed.ReviewRun.AgentTaskEvidenceRef != nil ||
		filterReplayed.ReviewRun.AgentExecutionReceiptRef != nil ||
		len(filterReplayed.ReviewRun.StageAttempts) != 0 ||
		len(filterReplayed.ReviewRun.Bindings) != 0 ||
		filterReplayed.ReviewRun.HypothesisSetRef == nil || succeeded.ReviewRun.HypothesisSetRef == nil ||
		*filterReplayed.ReviewRun.HypothesisSetRef != *succeeded.ReviewRun.HypothesisSetRef ||
		*filterReplayed.CandidateSetRef != *succeeded.CandidateSetRef ||
		*filterReplayed.VerificationLedgerRef != *succeeded.VerificationLedgerRef ||
		*filterReplayed.ReportRef != *succeeded.ReportRef ||
		*filterReplayed.MarkdownReportRef != *succeeded.MarkdownReportRef ||
		*filterReplayed.CalibrationLedgerRef == *succeeded.CalibrationLedgerRef ||
		*filterReplayed.SuppressionLedgerRef == *succeeded.SuppressionLedgerRef {
		t.Fatalf("formal finding_governance replay closure=%+v", filterReplayed.ReviewRun)
	}
	forgedExecution := *filterReplayed.ReviewRun
	forgedExecution.StageAttempts = append([]runmodel.StageAttempt{}, succeeded.ReviewRun.StageAttempts...)
	forgedExecution.Bindings = append([]runmodel.PlatformExecutionBinding{}, succeeded.ReviewRun.Bindings...)
	for index := range forgedExecution.Bindings {
		forgedExecution.Bindings[index].RunID = forgedExecution.RunID
	}
	forgedExecution.Evidence = append([]runmodel.RunEvidence{}, succeeded.ReviewRun.Evidence...)
	if _, err := runRepository.FinalizeRun(
		runrepo.TerminalEventID(forgedExecution.RunID), *forgedExecution.CompletedAt, forgedExecution,
	); err == nil || !strings.Contains(err.Error(), "invented provider execution facts") {
		t.Fatalf("finding_governance replay admitted forged execution facts: %v", err)
	}
	forgedReport := *filterReplayed.ReviewRun
	forgedReport.GovernedReportRef = replayed.ReportRef
	if _, err := runRepository.FinalizeRun(
		runrepo.TerminalEventID(forgedReport.RunID), *forgedReport.CompletedAt, forgedReport,
	); err == nil || !strings.Contains(err.Error(), "changed source governed report ref") {
		t.Fatalf("finding_governance replay admitted changed report ref: %v", err)
	}
	forgedCalibrationLedger := *filterReplayed.CalibrationLedger
	forgedCalibrationLedger.CalibrationLedgerID += "-forged"
	forgedCalibrationRef, err := runRepository.PutJSONArtifact(
		runmodel.ContractFindingCalibrationLedger, forgedCalibrationLedger,
	)
	if err != nil {
		t.Fatal(err)
	}
	forgedCalibration := *filterReplayed.ReviewRun
	forgedCalibration.CalibrationLedgerRef = &forgedCalibrationRef
	if _, err := runRepository.FinalizeRun(
		runrepo.TerminalEventID(forgedCalibration.RunID), *forgedCalibration.CompletedAt, forgedCalibration,
	); err == nil || !strings.Contains(err.Error(), "calibration_ledger_id") {
		t.Fatalf("finding_governance replay admitted forged calibration math: %v", err)
	}
	filterSnapshot, err := runRepository.ExecutionSnapshotForRun(filterReplayed.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	var filterChange runmodel.ReplayChangeSet
	if filterSnapshot.ReplayChangeSetRef == nil ||
		runRepository.ReadJSONArtifact(*filterSnapshot.ReplayChangeSetRef, &filterChange) != nil {
		t.Fatalf("formal finding_governance snapshot=%+v", filterSnapshot)
	}
	if filterSnapshot.BuildIdentity != bootstrap.Manifest.BuildIdentity ||
		filterChange.StartStage != "finding_governance" ||
		filterChange.Variable != runmodel.ReplayVariableFilterPolicy ||
		!slices.Equal(filterChange.ChangedFields, formalreview.FormalFilterPolicyReplayChangedFields()) {
		t.Fatalf("formal finding_governance snapshot=%+v change=%+v", filterSnapshot, filterChange)
	}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), filterReplayOptions, &bytes.Buffer{}, filterRunner,
	); err != nil {
		t.Fatalf("formal finding_governance replay retry: %v", err)
	}
	if filterRunner.calls != 0 {
		t.Fatalf("formal finding_governance replay retry invoked runner %d times", filterRunner.calls)
	}
	var shownGovernance bytes.Buffer
	if err := executeFormalAgentShow(t.Context(), formalAgentShowFlags{
		store: store, formalRun: filterReplayed.FormalRunID, json: true,
	}, &shownGovernance); err != nil {
		t.Fatalf("show finding_governance replay: %v", err)
	}
	var shownGovernanceOutput formalAgentRunOutput
	if err := json.Unmarshal(shownGovernance.Bytes(), &shownGovernanceOutput); err != nil ||
		shownGovernanceOutput.ReviewRun == nil ||
		shownGovernanceOutput.TerminalAuthority != "finding_governance_replay_derived" ||
		!shownGovernanceOutput.RecoveredFinalRun || shownGovernanceOutput.TaskEvidence != nil {
		t.Fatalf("shown finding_governance replay=%+v error=%v", shownGovernanceOutput, err)
	}

	chainedProfile, err := reviewconfig.SealCalibrationProfile(
		"formal-filter-e2e", "variant-v2",
		[]reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 900_000}},
	)
	if err != nil {
		t.Fatal(err)
	}
	chainedPolicy, err := reviewconfig.SealFindingGovernancePolicy(chainedProfile, 400_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	chainedPolicyData, err := json.Marshal(chainedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	chainedPolicyPath := filepath.Join(t.TempDir(), "finding-governance-v2.json")
	if err := os.WriteFile(chainedPolicyPath, chainedPolicyData, 0o600); err != nil {
		t.Fatal(err)
	}
	conflictOptions := filterReplayOptions
	conflictOptions.findingGovernance = chainedPolicyPath
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), conflictOptions, &bytes.Buffer{}, filterRunner,
	); err == nil || !strings.Contains(err.Error(), "conflicts with variant closure") {
		t.Fatalf("finding_governance conflicting retry error=%v", err)
	}
	if filterRunner.calls != 0 {
		t.Fatalf("finding_governance conflicting retry invoked runner %d times", filterRunner.calls)
	}
	governanceChainOptions := filterReplayOptions
	governanceChainOptions.sourceFormalRun = filterReplayed.FormalRunID
	governanceChainOptions.idempotencyKey = "formal-filter-policy-replay-chain-e2e"
	governanceChainOptions.findingGovernance = chainedPolicyPath
	governanceChainOptions.at = filterReplayed.ReviewRun.CompletedAt.Add(time.Second).UTC()
	var governanceChainOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), governanceChainOptions, &governanceChainOutput, filterRunner,
	); err != nil {
		t.Fatalf("chained finding_governance replay: %v", err)
	}
	var chainedGovernance formalAgentRunOutput
	if err := json.Unmarshal(governanceChainOutput.Bytes(), &chainedGovernance); err != nil {
		t.Fatal(err)
	}
	if chainedGovernance.ReviewRun == nil ||
		chainedGovernance.ReviewRun.SourceRunID != filterReplayed.FormalRunID ||
		chainedGovernance.ReviewRun.ReplayRootRunID != succeeded.FormalRunID ||
		*chainedGovernance.ReportRef != *filterReplayed.ReportRef ||
		*chainedGovernance.VerificationLedgerRef != *filterReplayed.VerificationLedgerRef ||
		*chainedGovernance.CalibrationLedgerRef == *filterReplayed.CalibrationLedgerRef ||
		*chainedGovernance.SuppressionLedgerRef == *filterReplayed.SuppressionLedgerRef ||
		filterRunner.calls != 0 {
		t.Fatalf("chained finding_governance replay=%+v runner_calls=%d",
			chainedGovernance, filterRunner.calls)
	}
	modelReplayAt := succeeded.ReviewRun.CompletedAt.Add(time.Second).UTC().Format(time.RFC3339Nano)
	modelReplayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-model-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-reasoner",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--change", "model",
		"--at", modelReplayAt, "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	modelReplayRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	var modelReplayOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), modelReplayOptions, &modelReplayOutput, modelReplayRunner,
	); err != nil {
		t.Fatalf("formal model replay: %v\n%s", err, modelReplayOutput.String())
	}
	var modelReplayed formalAgentRunOutput
	if err := json.Unmarshal(modelReplayOutput.Bytes(), &modelReplayed); err != nil {
		t.Fatal(err)
	}
	if modelReplayed.ReviewRun == nil ||
		modelReplayed.ReviewRun.ReplayVariable != runmodel.ReplayVariableModel ||
		modelReplayed.ReviewRun.SourceRunID != succeeded.FormalRunID || modelReplayRunner.calls != 1 {
		t.Fatalf("formal model replay=%+v runner_calls=%d", modelReplayed, modelReplayRunner.calls)
	}
	modelSnapshot, err := runRepository.ExecutionSnapshotForRun(modelReplayed.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	sourceFormalSnapshot, err := runRepository.ExecutionSnapshotForRun(succeeded.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if modelSnapshot.BuildIdentity == sourceFormalSnapshot.BuildIdentity ||
		!slices.Equal(modelSnapshot.RuntimeEvidenceRefs, sourceFormalSnapshot.RuntimeEvidenceRefs) ||
		!reflect.DeepEqual(modelSnapshot.ToolPolicy, sourceFormalSnapshot.ToolPolicy) ||
		modelSnapshot.RemoteWrites != "deny" || modelSnapshot.ReplayChangeSetRef == nil {
		t.Fatalf("formal model snapshot=%+v", modelSnapshot)
	}
	var modelChange runmodel.ReplayChangeSet
	if err := runRepository.ReadJSONArtifact(*modelSnapshot.ReplayChangeSetRef, &modelChange); err != nil {
		t.Fatal(err)
	}
	if modelChange.Variable != runmodel.ReplayVariableModel ||
		!slices.Equal(modelChange.ChangedFields, formalreview.FormalModelReplayChangedFields()) {
		t.Fatalf("formal model change=%+v", modelChange)
	}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), modelReplayOptions, &bytes.Buffer{}, modelReplayRunner,
	); err != nil {
		t.Fatalf("formal model replay retry: %v", err)
	}
	if modelReplayRunner.calls != 1 {
		t.Fatalf("formal model replay retry invoked runner %d times", modelReplayRunner.calls)
	}
	conflictingModelOptions := modelReplayOptions
	conflictingModelOptions.model = "deepseek-v4"
	conflictingModelOptions.options.Model = "deepseek-v4"
	conflictingModelRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), conflictingModelOptions, &bytes.Buffer{}, conflictingModelRunner,
	); err == nil || !strings.Contains(err.Error(), "formal model replay") {
		t.Fatalf("conflicting formal model replay error=%v", err)
	}
	if conflictingModelRunner.calls != 0 {
		t.Fatalf("conflicting formal model replay invoked runner %d times", conflictingModelRunner.calls)
	}
	promptMarker := "ARGUS_PROMPT_REPLAY_E2E"
	promptBundle := contractsv1alpha1.DefaultAgentReviewPromptBundle()
	promptBundle.Revision = "experiment-v1"
	promptBundle.ReviewSystemPrompt = promptMarker + ": prioritize state-transition defects."
	promptData, err := json.Marshal(promptBundle)
	if err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(t.TempDir(), "prompt-replay.json")
	if err := os.WriteFile(promptPath, promptData, 0o600); err != nil {
		t.Fatal(err)
	}
	promptReplayAt := succeeded.ReviewRun.CompletedAt.Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	promptReplayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-prompt-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--change", "prompt",
		"--prompt-bundle", promptPath, "--at", promptReplayAt, "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	promptReplayRunner := &formalSucceededRunner{
		promptDigest: formalTestSHA(promptData), promptMarker: promptMarker,
	}
	var promptReplayOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), promptReplayOptions, &promptReplayOutput, promptReplayRunner,
	); err != nil {
		t.Fatalf("formal prompt replay: %v\n%s", err, promptReplayOutput.String())
	}
	var promptReplayed formalAgentRunOutput
	if err := json.Unmarshal(promptReplayOutput.Bytes(), &promptReplayed); err != nil {
		t.Fatal(err)
	}
	if promptReplayed.ReviewRun == nil ||
		promptReplayed.ReviewRun.ReplayVariable != runmodel.ReplayVariablePrompt ||
		promptReplayed.ReviewRun.SourceRunID != succeeded.FormalRunID || promptReplayRunner.calls != 1 {
		t.Fatalf("formal prompt replay=%+v runner_calls=%d", promptReplayed, promptReplayRunner.calls)
	}
	promptSnapshot, err := runRepository.ExecutionSnapshotForRun(promptReplayed.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if promptSnapshot.BuildIdentity == sourceFormalSnapshot.BuildIdentity ||
		!slices.Equal(promptSnapshot.RuntimeEvidenceRefs, sourceFormalSnapshot.RuntimeEvidenceRefs) ||
		!reflect.DeepEqual(promptSnapshot.ToolPolicy, sourceFormalSnapshot.ToolPolicy) ||
		promptSnapshot.RemoteWrites != "deny" || promptSnapshot.ReplayChangeSetRef == nil {
		t.Fatalf("formal prompt snapshot=%+v", promptSnapshot)
	}
	var promptChange runmodel.ReplayChangeSet
	if err := runRepository.ReadJSONArtifact(*promptSnapshot.ReplayChangeSetRef, &promptChange); err != nil {
		t.Fatal(err)
	}
	if promptChange.Variable != runmodel.ReplayVariablePrompt ||
		!slices.Equal(promptChange.ChangedFields, formalreview.FormalPromptReplayChangedFields()) {
		t.Fatalf("formal prompt change=%+v", promptChange)
	}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), promptReplayOptions, &bytes.Buffer{}, promptReplayRunner,
	); err != nil {
		t.Fatalf("formal prompt replay retry: %v", err)
	}
	if promptReplayRunner.calls != 1 {
		t.Fatalf("formal prompt replay retry invoked runner %d times", promptReplayRunner.calls)
	}
	skillMarker := "ARGUS_SKILL_REPLAY_E2E"
	skillData := []byte("# Business state transition\n\nFind concrete invalid state transitions. " + skillMarker + "\n")
	skillPath := filepath.Join(t.TempDir(), "correctness.md")
	if err := os.WriteFile(skillPath, skillData, 0o600); err != nil {
		t.Fatal(err)
	}
	skillReplayAt := succeeded.ReviewRun.CompletedAt.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	skillReplayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-skill-replay-e2e",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--change", "skill_pack",
		"--review-skill", skillPath, "--at", skillReplayAt, "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	skillReplayRunner := &formalSucceededRunner{
		promptDigest: bootstrap.Manifest.Prompt.SHA256, skillMarker: skillMarker,
	}
	var skillReplayOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), skillReplayOptions, &skillReplayOutput, skillReplayRunner,
	); err != nil {
		t.Fatalf("formal skill_pack replay: %v\n%s", err, skillReplayOutput.String())
	}
	var skillReplayed formalAgentRunOutput
	if err := json.Unmarshal(skillReplayOutput.Bytes(), &skillReplayed); err != nil {
		t.Fatal(err)
	}
	if skillReplayed.ReviewRun == nil ||
		skillReplayed.ReviewRun.ReplayVariable != runmodel.ReplayVariableSkillPack ||
		skillReplayed.ReviewRun.SourceRunID != succeeded.FormalRunID || skillReplayRunner.calls != 1 {
		t.Fatalf("formal skill_pack replay=%+v runner_calls=%d", skillReplayed, skillReplayRunner.calls)
	}
	skillSnapshot, err := runRepository.ExecutionSnapshotForRun(skillReplayed.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if skillSnapshot.BuildIdentity == sourceFormalSnapshot.BuildIdentity ||
		!slices.Equal(skillSnapshot.RuntimeEvidenceRefs, sourceFormalSnapshot.RuntimeEvidenceRefs) ||
		!reflect.DeepEqual(skillSnapshot.ToolPolicy, sourceFormalSnapshot.ToolPolicy) ||
		skillSnapshot.RemoteWrites != "deny" || skillSnapshot.ReplayChangeSetRef == nil {
		t.Fatalf("formal skill_pack snapshot=%+v", skillSnapshot)
	}
	var skillChange runmodel.ReplayChangeSet
	if err := runRepository.ReadJSONArtifact(*skillSnapshot.ReplayChangeSetRef, &skillChange); err != nil {
		t.Fatal(err)
	}
	if skillChange.Variable != runmodel.ReplayVariableSkillPack ||
		!slices.Equal(skillChange.ChangedFields, formalreview.FormalSkillPackReplayChangedFields()) {
		t.Fatalf("formal skill_pack change=%+v", skillChange)
	}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), skillReplayOptions, &bytes.Buffer{}, skillReplayRunner,
	); err != nil {
		t.Fatalf("formal skill_pack replay retry: %v", err)
	}
	if skillReplayRunner.calls != 1 {
		t.Fatalf("formal skill_pack replay retry invoked runner %d times", skillReplayRunner.calls)
	}
	if err := os.WriteFile(
		skillPath,
		append(slices.Clone(skillData), []byte("Conflicting replacement.\n")...),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	conflictingSkillRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), skillReplayOptions, &bytes.Buffer{}, conflictingSkillRunner,
	); err == nil || !strings.Contains(err.Error(), "conflicts with variant closure") {
		t.Fatalf("conflicting formal skill_pack replay error=%v", err)
	}
	if conflictingSkillRunner.calls != 0 {
		t.Fatalf("conflicting formal skill_pack replay invoked runner %d times", conflictingSkillRunner.calls)
	}
	var unifiedShow bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"show", "--store", store, "--run", succeeded.FormalRunID, "--json",
	}, &unifiedShow); err != nil {
		t.Fatalf("show unified formal ReviewRun: %v", err)
	}
	var shownRun showOutput
	if err := json.Unmarshal(unifiedShow.Bytes(), &shownRun); err != nil {
		t.Fatal(err)
	}
	if shownRun.Run.RunID != succeeded.FormalRunID ||
		shownRun.Run.Status != "succeeded" || shownRun.GovernedReport == nil {
		t.Fatalf("unified formal show = %+v", shownRun)
	}
	var history bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"history", "--store", store, "--limit", "0", "--json",
	}, &history); err != nil {
		t.Fatalf("show unified formal history: %v", err)
	}
	if !strings.Contains(history.String(), `"run_id":"`+succeeded.FormalRunID+`"`) ||
		!strings.Contains(history.String(), `"last_event_type":"run.succeeded"`) {
		t.Fatalf("formal ReviewRun absent from history: %s", history.String())
	}

	accessAt := time.Now().UTC()
	readArguments := []string{
		"agent-review", "formal", "evidence", "read",
		"--store", store, "--formal-run", succeeded.FormalRunID,
		"--request-id", "formal-evidence-read-e2e",
		"--actor", "formal-debugger", "--purpose", "local_debug",
		"--at", accessAt.Format(time.RFC3339Nano),
		"--acknowledge-sensitive-output", "--json",
	}
	var evidenceOutput bytes.Buffer
	if err := runWithIO(t.Context(), readArguments, &evidenceOutput); err != nil {
		t.Fatalf("read formal task evidence: %v", err)
	}
	if !strings.Contains(evidenceOutput.String(), `"formal_run_id":"`+succeeded.FormalRunID+`"`) ||
		!strings.Contains(evidenceOutput.String(), `"access_proof":`) ||
		!strings.Contains(evidenceOutput.String(), `"content_policy":"exact_local_sensitive"`) {
		t.Fatalf("formal evidence read output is incomplete: %s", evidenceOutput.String())
	}
	revokeAt := time.Now().UTC()
	revokeArguments := []string{
		"agent-review", "formal", "evidence", "revoke",
		"--store", store, "--formal-run", succeeded.FormalRunID,
		"--idempotency-key", "formal-evidence-revoke-e2e",
		"--actor", "retention-operator", "--reason", "retention ended",
		"--at", revokeAt.Format(time.RFC3339Nano), "--json",
	}
	var revokeOutput bytes.Buffer
	if err := runWithIO(t.Context(), revokeArguments, &revokeOutput); err != nil {
		t.Fatalf("revoke formal task evidence: %v", err)
	}
	if !strings.Contains(revokeOutput.String(), `"state":"tombstoned"`) {
		t.Fatalf("formal evidence revoke output = %s", revokeOutput.String())
	}
	if err := runWithIO(t.Context(), readArguments, &bytes.Buffer{}); !errors.Is(err, artifactrepo.ErrTombstoned) {
		t.Fatalf("read revoked formal evidence error = %v, want ErrTombstoned", err)
	}
	var shownAfterRevoke bytes.Buffer
	if err := executeFormalAgentShow(t.Context(), formalAgentShowFlags{
		store: store, formalRun: succeeded.FormalRunID, json: true,
	}, &shownAfterRevoke); err != nil {
		t.Fatalf("show formal run after task evidence revoke: %v", err)
	}
	var afterRevoke formalAgentRunOutput
	if err := json.Unmarshal(shownAfterRevoke.Bytes(), &afterRevoke); err != nil {
		t.Fatal(err)
	}
	if afterRevoke.TaskEvidence == nil || afterRevoke.TaskEvidence.State != "tombstoned" ||
		afterRevoke.ReviewRun == nil || afterRevoke.ReviewRun.AgentTaskEvidenceRef == nil {
		t.Fatalf("formal history lost after evidence revoke: %+v", afterRevoke)
	}
}

func TestFormalKnowledgePackReplayIsExactIdempotent(t *testing.T) {
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
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n// TODO review me\n")
	revision := commitCLITarget(t, repositoryPath, "formal knowledge target")
	store, configState := t.TempDir(), t.TempDir()
	var reviewOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"review", "--repo", repositoryPath, "--mode", "selection",
		"--revision", revision, "--path", "review.go", "--start-line", "2", "--end-line", "2",
		"--store", store, "--json",
	}, &reviewOutput); err != nil {
		t.Fatal(err)
	}
	var reviewed runOutput
	if err := json.Unmarshal(reviewOutput.Bytes(), &reviewed); err != nil {
		t.Fatal(err)
	}
	baselineMarker := "ARGUS_BASELINE_KNOWLEDGE"
	baselinePath := filepath.Join(t.TempDir(), "repository-invariants.md")
	if err := os.WriteFile(
		baselinePath,
		[]byte("# Repository invariants\n\nBaseline ownership rule. "+baselineMarker+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	bootstrapArguments := []string{
		"agent-review", "formal", "bootstrap", "--store", store,
		"--config-state-dir", configState, "--source-run", reviewed.Run.RunID,
		"--idempotency-key", "formal-knowledge-bootstrap", "--at", "2026-08-25T04:00:00Z",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--knowledge", baselinePath, "--json",
	}
	if err := runWithIO(t.Context(), bootstrapArguments, &bytes.Buffer{}); err != nil {
		t.Fatalf("formal knowledge bootstrap: %v", err)
	}
	runOptions, err := parseFormalAgentRunFlags([]string{
		"--store", store, "--config-state-dir", configState,
		"--source-run", reviewed.Run.RunID, "--idempotency-key", "formal-knowledge-source",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--knowledge", baselinePath, "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineBootstrap, err := formalreview.BuildLocalPiBootstrap(runOptions.options)
	if err != nil {
		t.Fatal(err)
	}
	sourceRunner := &formalSucceededRunner{
		promptDigest:    baselineBootstrap.Manifest.Prompt.SHA256,
		knowledgeMarker: baselineMarker,
	}
	var sourceOutput bytes.Buffer
	if err := executeFormalAgentRunWithRunner(
		t.Context(), runOptions, &sourceOutput, sourceRunner,
	); err != nil {
		t.Fatalf("formal knowledge source: %v\n%s", err, sourceOutput.String())
	}
	var source formalAgentRunOutput
	if err := json.Unmarshal(sourceOutput.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	if source.ReviewRun == nil || source.ReviewRun.CompletedAt == nil {
		t.Fatalf("formal knowledge source has no completion time: %+v", source)
	}
	replayAt := source.ReviewRun.CompletedAt.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	variantMarker := "ARGUS_VARIANT_KNOWLEDGE"
	variantData := []byte("# Repository invariants\n\nVariant state transition rule. " + variantMarker + "\n")
	variantPath := filepath.Join(t.TempDir(), "repository-invariants.md")
	if err := os.WriteFile(variantPath, variantData, 0o600); err != nil {
		t.Fatal(err)
	}
	replayOptions, err := parseFormalAgentReplayFlags([]string{
		"--store", store, "--source-formal-run", source.FormalRunID,
		"--idempotency-key", "formal-knowledge-variant", "--node", node,
		"--worker-script", worker, "--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "knowledge_pack", "--knowledge", variantPath,
		"--at", replayAt, "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	variantRunner := &formalSucceededRunner{
		promptDigest:    baselineBootstrap.Manifest.Prompt.SHA256,
		knowledgeMarker: variantMarker,
	}
	var variantOutput bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), replayOptions, &variantOutput, variantRunner,
	); err != nil {
		t.Fatalf("formal knowledge_pack replay: %v\n%s", err, variantOutput.String())
	}
	var variant formalAgentRunOutput
	if err := json.Unmarshal(variantOutput.Bytes(), &variant); err != nil {
		t.Fatal(err)
	}
	if variant.ReviewRun == nil ||
		variant.ReviewRun.ReplayVariable != runmodel.ReplayVariableKnowledgePack ||
		variant.ReviewRun.SourceRunID != source.FormalRunID || variantRunner.calls != 1 {
		t.Fatalf("knowledge_pack replay=%+v calls=%d", variant, variantRunner.calls)
	}
	state, err := local.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		t.Fatal(err)
	}
	sourceSnapshot, err := runs.ExecutionSnapshotForRun(source.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	variantSnapshot, err := runs.ExecutionSnapshotForRun(variant.FormalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if variantSnapshot.BuildIdentity == sourceSnapshot.BuildIdentity ||
		!reflect.DeepEqual(variantSnapshot.ToolPolicy, sourceSnapshot.ToolPolicy) ||
		!reflect.DeepEqual(variantSnapshot.RuntimeEvidenceRefs, sourceSnapshot.RuntimeEvidenceRefs) ||
		variantSnapshot.RemoteWrites != "deny" || variantSnapshot.ReplayChangeSetRef == nil {
		t.Fatalf("knowledge_pack snapshot=%+v", variantSnapshot)
	}
	var change runmodel.ReplayChangeSet
	if err := runs.ReadJSONArtifact(*variantSnapshot.ReplayChangeSetRef, &change); err != nil {
		t.Fatal(err)
	}
	if change.Variable != runmodel.ReplayVariableKnowledgePack ||
		!slices.Equal(change.ChangedFields, formalreview.FormalKnowledgePackReplayChangedFields()) {
		t.Fatalf("knowledge_pack change=%+v", change)
	}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), replayOptions, &bytes.Buffer{}, variantRunner,
	); err != nil {
		t.Fatalf("knowledge_pack exact retry: %v", err)
	}
	if variantRunner.calls != 1 {
		t.Fatalf("knowledge_pack exact retry invoked runner %d times", variantRunner.calls)
	}
	if err := os.WriteFile(
		variantPath, append(slices.Clone(variantData), []byte("conflict\n")...), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	conflictingRunner := &formalSucceededRunner{promptDigest: baselineBootstrap.Manifest.Prompt.SHA256}
	if err := executeFormalAgentReplayWithRunner(
		t.Context(), replayOptions, &bytes.Buffer{}, conflictingRunner,
	); err == nil || !strings.Contains(err.Error(), "conflicts with variant closure") {
		t.Fatalf("conflicting knowledge_pack replay error=%v", err)
	}
	if conflictingRunner.calls != 0 {
		t.Fatalf("conflicting knowledge_pack replay invoked runner %d times", conflictingRunner.calls)
	}
}

func TestParseFormalAgentReplayFlagsRejectsIncompleteOrUnsupportedVariant(t *testing.T) {
	required := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
	}
	for name, extra := range map[string][]string{
		"budget without timeout":      {"--change", "budget"},
		"timeout without budget":      {"--timeout-ms", "1000"},
		"model without time":          {"--change", "model"},
		"model with timeout":          {"--change", "model", "--timeout-ms", "1000"},
		"time without model":          {"--at", "2026-08-25T03:04:05Z"},
		"prompt without bundle":       {"--change", "prompt", "--at", "2026-08-25T03:04:05Z"},
		"prompt without time":         {"--change", "prompt", "--prompt-bundle", "/tmp/prompt.json"},
		"bundle without prompt":       {"--prompt-bundle", "/tmp/prompt.json", "--at", "2026-08-25T03:04:05Z"},
		"prompt with timeout":         {"--change", "prompt", "--prompt-bundle", "/tmp/prompt.json", "--at", "2026-08-25T03:04:05Z", "--timeout-ms", "1000"},
		"skill without file":          {"--change", "skill_pack", "--at", "2026-08-25T03:04:05Z"},
		"skill without time":          {"--change", "skill_pack", "--review-skill", "/tmp/correctness.md"},
		"file without skill":          {"--review-skill", "/tmp/correctness.md", "--at", "2026-08-25T03:04:05Z"},
		"skill with timeout":          {"--change", "skill_pack", "--review-skill", "/tmp/correctness.md", "--at", "2026-08-25T03:04:05Z", "--timeout-ms", "1000"},
		"skill with prompt":           {"--change", "skill_pack", "--review-skill", "/tmp/correctness.md", "--prompt-bundle", "/tmp/prompt.json", "--at", "2026-08-25T03:04:05Z"},
		"knowledge without file":      {"--change", "knowledge_pack", "--at", "2026-08-25T03:04:05Z"},
		"knowledge without time":      {"--change", "knowledge_pack", "--knowledge", "/tmp/rules.md"},
		"knowledge with timeout":      {"--change", "knowledge_pack", "--knowledge", "/tmp/rules.md", "--at", "2026-08-25T03:04:05Z", "--timeout-ms", "1000"},
		"knowledge with prompt":       {"--change", "knowledge_pack", "--knowledge", "/tmp/rules.md", "--prompt-bundle", "/tmp/prompt.json", "--at", "2026-08-25T03:04:05Z"},
		"rule without pack":           {"--change", "rule_pack", "--at", "2026-08-25T03:04:05Z"},
		"rule without time":           {"--change", "rule_pack", "--rule-pack", "/tmp/rule-pack.json"},
		"rule with timeout":           {"--change", "rule_pack", "--rule-pack", "/tmp/rule-pack.json", "--at", "2026-08-25T03:04:05Z", "--timeout-ms", "1000"},
		"workflow without definition": {"--change", "workflow", "--at", "2026-08-25T03:04:05Z"},
		"workflow without time":       {"--change", "workflow", "--workflow-definition", "/tmp/workflow.json"},
		"workflow with timeout":       {"--change", "workflow", "--workflow-definition", "/tmp/workflow.json", "--at", "2026-08-25T03:04:05Z", "--timeout-ms", "1000"},
		"index without provider":      {"--change", "index", "--at", "2026-08-25T03:04:05Z"},
		"index without time":          {"--change", "index", "--context-provider", "go_ast"},
		"index with timeout":          {"--change", "index", "--context-provider", "go_ast", "--at", "2026-08-25T03:04:05Z", "--timeout-ms", "1000"},
	} {
		t.Run(name, func(t *testing.T) {
			arguments := append(slices.Clone(required), extra...)
			if _, err := parseFormalAgentReplayFlags(arguments); err == nil ||
				!strings.Contains(err.Error(), "supports exact mode") {
				t.Fatalf("parseFormalAgentReplayFlags() error=%v", err)
			}
		})
	}
}

func TestParseFormalAgentRunFlagsAdmitsOnlyCompleteHailixHTTPTransport(t *testing.T) {
	base := []string{
		"--store", t.TempDir(), "--config-state-dir", t.TempDir(),
		"--source-run", "source-run", "--idempotency-key", "formal-run",
		"--node", "/usr/bin/node", "--worker-script", "/tmp/worker.js",
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
	}
	hailix := []string{
		"--execution-backend", "hailix-http",
		"--hailix-base-url", "https://hailix.example.test/platform/",
		"--hailix-capability-verifier-id", "capability-verifier",
		"--hailix-capability-verifier-revision", "v1",
		"--hailix-capability-verifier-sha256", strings.Repeat("c", 64),
		"--hailix-callback-verifier-id", "callback-verifier",
		"--hailix-callback-verifier-revision", "v1",
		"--hailix-callback-verifier-sha256", strings.Repeat("d", 64),
	}
	options, err := parseFormalAgentRunFlags(append(slices.Clone(base), hailix...))
	if err != nil {
		t.Fatal(err)
	}
	if options.transport.Backend != formalExecutionBackendHailixHTTP ||
		options.transport.HailixBaseURL != "https://hailix.example.test/platform/" {
		t.Fatalf("Hailix transport flags = %+v", options.transport)
	}

	incomplete := append(slices.Clone(base), hailix[:len(hailix)-2]...)
	if _, err := parseFormalAgentRunFlags(incomplete); err == nil ||
		!strings.Contains(err.Error(), "--hailix-callback-verifier-sha256 is required") {
		t.Fatalf("incomplete Hailix flags error = %v", err)
	}
	localWithHailix := append(slices.Clone(base), "--hailix-base-url", "https://hailix.example.test/")
	if _, err := parseFormalAgentRunFlags(localWithHailix); err == nil ||
		!strings.Contains(err.Error(), "valid only with --execution-backend hailix-http") {
		t.Fatalf("local Hailix-only flags error = %v", err)
	}
	unknown := append(slices.Clone(base), "--execution-backend", "direct-task")
	if _, err := parseFormalAgentRunFlags(unknown); err == nil ||
		!strings.Contains(err.Error(), "must be local-pi or hailix-http") {
		t.Fatalf("unknown backend error = %v", err)
	}
}

func TestConfiguredHailixBackendFailsBeforeTransportWhenCredentialIsMissing(t *testing.T) {
	t.Setenv(hailixexecution.BearerTokenEnvironment, "")
	t.Setenv(hailixexecution.CredentialRevisionEnvironment, "credential-revision-1")
	options := formalExecutionTransportFlags{
		Backend:                      formalExecutionBackendHailixHTTP,
		HailixBaseURL:                "https://hailix.example.test/platform/",
		HailixCapabilityVerifierID:   "capability-verifier",
		HailixCapabilityVerifierRev:  "v1",
		HailixCapabilityVerifierHash: strings.Repeat("c", 64),
		HailixCallbackVerifierID:     "callback-verifier",
		HailixCallbackVerifierRev:    "v1",
		HailixCallbackVerifierHash:   strings.Repeat("d", 64),
	}
	_, err := buildConfiguredFormalExecutionBackend(
		context.Background(), options, application.AgentPlanningSubject{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			WorkspaceID: "workspace-1", RepositoryID: "repository-1",
		},
	)
	if err == nil || !strings.Contains(err.Error(), hailixexecution.BearerTokenEnvironment) ||
		strings.Contains(err.Error(), "credential-revision-1") {
		t.Fatalf("missing credential error = %v", err)
	}
}

func TestParseFormalAgentReplayFlagsAcceptsWorkflowDefinition(t *testing.T) {
	arguments := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "workflow", "--workflow-definition", "/tmp/workflow.json",
		"--at", "2026-08-27T03:04:05Z",
	}
	options, err := parseFormalAgentReplayFlags(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if options.change != runmodel.ReplayVariableWorkflow ||
		options.workflowDefinition != "/tmp/workflow.json" || options.at.IsZero() {
		t.Fatalf("workflow replay flags=%+v", options)
	}
}

func TestParseFormalAgentReplayFlagsRejectsRelativeWorkflowDefinition(t *testing.T) {
	arguments := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "workflow", "--workflow-definition", "workflow.json",
		"--at", "2026-08-27T03:04:05Z",
	}
	if _, err := parseFormalAgentReplayFlags(arguments); err == nil ||
		!strings.Contains(err.Error(), "--workflow-definition must be a clean absolute path") {
		t.Fatalf("parseFormalAgentReplayFlags() error=%v", err)
	}
}

func TestParseFormalAgentReplayFlagsRejectsRulePackWithoutRuleChange(t *testing.T) {
	arguments := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--rule-pack", "/tmp/rule-pack.json", "--at", "2026-08-25T03:04:05Z",
	}
	if _, err := parseFormalAgentReplayFlags(arguments); err == nil ||
		!strings.Contains(err.Error(), "--rule-pack is valid only with --change rule_pack") {
		t.Fatalf("parseFormalAgentReplayFlags() error=%v", err)
	}
}

func TestParseFormalAgentReplayFlagsRejectsRelativeRulePack(t *testing.T) {
	arguments := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "rule_pack", "--rule-pack", "rule-pack.json",
		"--at", "2026-08-25T03:04:05Z",
	}
	if _, err := parseFormalAgentReplayFlags(arguments); err == nil ||
		!strings.Contains(err.Error(), "--rule-pack must be a clean absolute path") {
		t.Fatalf("parseFormalAgentReplayFlags() error=%v", err)
	}
}

func TestParseFormalAgentReplayFlagsRejectsRelativePromptBundle(t *testing.T) {
	arguments := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "prompt", "--prompt-bundle", "prompt.json",
		"--at", "2026-08-25T03:04:05Z",
	}
	if _, err := parseFormalAgentReplayFlags(arguments); err == nil ||
		!strings.Contains(err.Error(), "--prompt-bundle must be a clean absolute path") {
		t.Fatalf("parseFormalAgentReplayFlags() error=%v", err)
	}
}

func TestParseFormalAgentReplayFlagsRejectsRelativeReviewSkill(t *testing.T) {
	arguments := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "skill_pack", "--review-skill", "correctness.md",
		"--at", "2026-08-25T03:04:05Z",
	}
	if _, err := parseFormalAgentReplayFlags(arguments); err == nil ||
		!strings.Contains(err.Error(), "--review-skill must be a clean absolute path") {
		t.Fatalf("parseFormalAgentReplayFlags() error=%v", err)
	}
}

func TestParseFormalAgentReplayFlagsRejectsRelativeKnowledge(t *testing.T) {
	arguments := []string{
		"--store", t.TempDir(), "--source-formal-run", "formal-run-1",
		"--idempotency-key", "replay-1", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "profile-1",
		"--model", "model-1", "--input-micros-per-million", "1",
		"--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
		"--change", "knowledge_pack", "--knowledge", "rules.md",
		"--at", "2026-08-25T03:04:05Z",
	}
	if _, err := parseFormalAgentReplayFlags(arguments); err == nil ||
		!strings.Contains(err.Error(), "--knowledge must be a clean absolute path") {
		t.Fatalf("parseFormalAgentReplayFlags() error=%v", err)
	}
}

func TestFormalRunAndBootstrapRejectRelativeKnowledgePaths(t *testing.T) {
	runArguments := []string{
		"--store", t.TempDir(), "--config-state-dir", t.TempDir(),
		"--source-run", "source-run", "--idempotency-key", "run-key",
		"--node", "/usr/bin/node", "--worker-script", "/tmp/worker.js",
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--knowledge", "repository-rules.md",
	}
	if _, err := parseFormalAgentRunFlags(runArguments); err == nil ||
		!strings.Contains(err.Error(), "--knowledge must be a clean absolute path") {
		t.Fatalf("parseFormalAgentRunFlags() error=%v", err)
	}
	bootstrapArguments := []string{
		"--store", t.TempDir(), "--config-state-dir", t.TempDir(),
		"--source-run", "source-run", "--idempotency-key", "bootstrap-key",
		"--at", "2026-08-25T03:04:05Z", "--node", "/usr/bin/node",
		"--worker-script", "/tmp/worker.js", "--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat", "--knowledge", "repository-rules.md",
	}
	if _, err := parseFormalAgentBootstrapFlags(bootstrapArguments); err == nil ||
		!strings.Contains(err.Error(), "--knowledge must be a clean absolute path") {
		t.Fatalf("parseFormalAgentBootstrapFlags() error=%v", err)
	}
}

type formalFailedRunner struct{ calls int }

type formalRetryThenSucceededRunner struct {
	calls     int
	requests  []contractsv1alpha1.AgentReviewWorkerRequest
	succeeded formalSucceededRunner
}

func (runner *formalRetryThenSucceededRunner) Run(
	ctx context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	runner.calls++
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(request.Input)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	runner.requests = append(runner.requests, workerRequest)
	if runner.calls > 1 {
		return runner.succeeded.Run(ctx, request)
	}
	inputBytes, err := base64.StdEncoding.DecodeString(workerRequest.ReviewInputBase64)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	var input reviewcore.ReviewInput
	if err := json.Unmarshal(inputBytes, &input); err != nil {
		return agentshadowworker.Result{}, err
	}
	report, completedAt, err := cleanFormalPiReport(
		workerRequest, input, runner.succeeded.promptDigest, false,
	)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	report.Status = "failed"
	report.Coverage.GroupsReviewed = 0
	report.Coverage.ReviewTasksSucceeded = 0
	providerError := "provider_error"
	usageUnavailable := "provider_turn_incomplete"
	for index := range report.Execution.Tasks {
		task := &report.Execution.Tasks[index]
		if task.TaskKind != "review" {
			continue
		}
		task.ProviderTurnsCompleted = 0
		task.ToolCalls = 0
		task.ToolNames = []string{}
		task.ToolUsage = []pireviewmap.PiToolUsage{}
		task.Usage = pireviewmap.PiUsage{
			Completeness: "unavailable", UnavailableReasonCode: &usageUnavailable,
		}
		task.TerminalStatus = "failed"
		task.ErrorCode = &providerError
		task.OutputDigest = nil
	}
	reportData, err := json.Marshal(report)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	reportDigest := formalTestSHA(reportData)
	result := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion:    contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:       workerRequest.WorkItemID,
		Attempt:          workerRequest.Attempt,
		Generation:       workerRequest.Generation,
		FencingToken:     workerRequest.FencingToken,
		IdempotencyKey:   workerRequest.IdempotencyKey,
		CapabilitySHA256: workerRequest.Capability.SHA256,
		Status:           contractsv1alpha1.AgentReviewWorkerSucceeded,
		ReportSHA256:     &reportDigest,
		Report:           reportData,
		CompletedAt:      completedAt,
	}
	if err := result.Validate(); err != nil {
		return agentshadowworker.Result{}, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	return agentshadowworker.Result{
		Stdout: data, StartedAt: workerRequest.Plan.CreatedAt, FinishedAt: completedAt,
	}, nil
}

func (runner *formalFailedRunner) Run(
	_ context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	runner.calls++
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(request.Input)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	completedAt := time.Now().UTC()
	if completedAt.Before(workerRequest.Plan.CreatedAt) {
		completedAt = workerRequest.Plan.CreatedAt
	}
	result := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:    workerRequest.WorkItemID, Attempt: workerRequest.Attempt,
		Generation: workerRequest.Generation, FencingToken: workerRequest.FencingToken,
		IdempotencyKey:   workerRequest.IdempotencyKey,
		CapabilitySHA256: workerRequest.Capability.SHA256,
		Status:           contractsv1alpha1.AgentReviewWorkerFailed,
		Failure: &contractsv1alpha1.AgentReviewWorkerFailure{
			Code: "provider_unavailable", Message: "provider unavailable", Retryable: true,
		},
		CompletedAt: completedAt,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	return agentshadowworker.Result{
		Stdout: data, StartedAt: workerRequest.Plan.CreatedAt, FinishedAt: completedAt,
	}, nil
}

type formalSucceededRunner struct {
	calls                int
	promptDigest         string
	confirmedFinding     bool
	promptMarker         string
	skillMarker          string
	knowledgeMarker      string
	contextMarker        string
	expectedContextKinds []string
	expectedTimeoutMS    int64
	expectedConcurrency  int
}

func (runner *formalSucceededRunner) Run(
	_ context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	runner.calls++
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(request.Input)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	if workerRequest.PromptBundle.Ref.SHA256 != runner.promptDigest {
		return agentshadowworker.Result{}, errors.New("formal worker prompt digest drifted")
	}
	if runner.expectedTimeoutMS > 0 &&
		workerRequest.Plan.Budget.TimeoutMS != uint64(runner.expectedTimeoutMS) {
		return agentshadowworker.Result{}, fmt.Errorf(
			"formal worker timeout=%d want=%d",
			workerRequest.Plan.Budget.TimeoutMS,
			runner.expectedTimeoutMS,
		)
	}
	if runner.expectedConcurrency > 0 &&
		workerRequest.Plan.Budget.MaxConcurrency != uint32(runner.expectedConcurrency) {
		return agentshadowworker.Result{}, fmt.Errorf(
			"formal worker concurrency=%d want=%d",
			workerRequest.Plan.Budget.MaxConcurrency,
			runner.expectedConcurrency,
		)
	}
	if runner.promptMarker != "" {
		bundle, _, decodeErr := contractsv1alpha1.DecodeAgentReviewWorkerPromptBundle(
			workerRequest.PromptBundle,
		)
		if decodeErr != nil || !strings.Contains(bundle.ReviewSystemPrompt, runner.promptMarker) {
			return agentshadowworker.Result{}, errors.New("formal worker prompt content drifted")
		}
	}
	if runner.skillMarker != "" {
		found := false
		for _, skill := range workerRequest.ReviewSkills {
			content, decodeErr := contractsv1alpha1.DecodeAgentReviewWorkerSkill(skill)
			if decodeErr != nil {
				return agentshadowworker.Result{}, decodeErr
			}
			if strings.Contains(string(content), runner.skillMarker) {
				found = true
			}
		}
		if !found {
			return agentshadowworker.Result{}, errors.New("formal worker review skill content drifted")
		}
	}
	if runner.knowledgeMarker != "" {
		found := false
		for _, pack := range workerRequest.KnowledgePacks {
			content, decodeErr := contractsv1alpha1.DecodeAgentReviewWorkerKnowledge(pack)
			if decodeErr != nil {
				return agentshadowworker.Result{}, decodeErr
			}
			if strings.Contains(string(content), runner.knowledgeMarker) {
				found = true
			}
		}
		if !found {
			return agentshadowworker.Result{}, errors.New("formal worker knowledge content drifted")
		}
	}
	if runner.contextMarker != "" {
		found := false
		for _, contextArtifact := range workerRequest.ContextArtifacts {
			content, decodeErr := contractsv1alpha1.DecodeAgentReviewWorkerContextArtifact(
				contextArtifact,
			)
			if decodeErr != nil {
				return agentshadowworker.Result{}, decodeErr
			}
			if strings.Contains(string(content), runner.contextMarker) {
				found = true
			}
		}
		if !found {
			return agentshadowworker.Result{}, errors.New("formal worker context content drifted")
		}
	}
	for _, expectedKind := range runner.expectedContextKinds {
		found := false
		for _, contextArtifact := range workerRequest.ContextArtifacts {
			if contextArtifact.Kind == expectedKind {
				found = true
			}
		}
		if !found {
			return agentshadowworker.Result{}, fmt.Errorf("formal worker automatic context kind %q missing", expectedKind)
		}
	}
	inputBytes, err := base64.StdEncoding.DecodeString(workerRequest.ReviewInputBase64)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	var input reviewcore.ReviewInput
	if err := json.Unmarshal(inputBytes, &input); err != nil {
		return agentshadowworker.Result{}, err
	}
	if err := input.Validate(); err != nil {
		return agentshadowworker.Result{}, err
	}
	report, completedAt, err := cleanFormalPiReport(
		workerRequest, input, runner.promptDigest, runner.confirmedFinding,
	)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	reportData, err := json.Marshal(report)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	reportDigest := formalTestSHA(reportData)
	result := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:    workerRequest.WorkItemID, Attempt: workerRequest.Attempt,
		Generation: workerRequest.Generation, FencingToken: workerRequest.FencingToken,
		IdempotencyKey:   workerRequest.IdempotencyKey,
		CapabilitySHA256: workerRequest.Capability.SHA256,
		Status:           contractsv1alpha1.AgentReviewWorkerSucceeded,
		ReportSHA256:     &reportDigest, Report: reportData, CompletedAt: completedAt,
	}
	if err := result.Validate(); err != nil {
		return agentshadowworker.Result{}, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	return agentshadowworker.Result{
		Stdout: data, StartedAt: workerRequest.Plan.CreatedAt, FinishedAt: completedAt,
	}, nil
}

func cleanFormalPiReport(
	request contractsv1alpha1.AgentReviewWorkerRequest,
	input reviewcore.ReviewInput,
	promptDigest string,
	confirmedFinding bool,
) (pireviewmap.PiReviewReport, time.Time, error) {
	generatedAt := request.Plan.CreatedAt
	completedAt := time.Now().UTC()
	if completedAt.Before(request.Plan.CreatedAt) {
		completedAt = request.Plan.CreatedAt
	}
	if completedAt.After(request.Deadline) {
		completedAt = request.Deadline
	}
	providerProfile, err := pireviewmap.PiProfileForProvider(request.Plan.Provider.ID)
	if err != nil {
		return pireviewmap.PiReviewReport{}, time.Time{}, err
	}
	workflowRevision, err := contractsv1alpha1.CandidateNormalizationWorkflowRevision(
		request.Plan.Normalization.Revision,
	)
	if err != nil {
		return pireviewmap.PiReviewReport{}, time.Time{}, err
	}
	files := make([]pireviewmap.PiTargetFile, 0, len(input.Files))
	paths := make([]string, 0, len(input.Files))
	patches := make([]string, 0, len(input.Files))
	for _, file := range input.Files {
		if file.Content == nil {
			continue
		}
		patch, status, changedLines, patchErr := pireviewmap.FrozenPiFilePatch(input, file.Path)
		if patchErr != nil {
			return pireviewmap.PiReviewReport{}, time.Time{}, patchErr
		}
		targetDigest := "sha256:" + file.SHA256
		patchDigest := "sha256:" + formalTestSHA([]byte(patch))
		files = append(files, pireviewmap.PiTargetFile{
			Path: file.Path, Status: status, Digest: file.SHA256,
			ChangedLines: changedLines, TargetDigest: &targetDigest, PatchDigest: &patchDigest,
		})
		paths = append(paths, file.Path)
		patches = append(patches, patch)
	}
	if len(files) == 0 {
		return pireviewmap.PiReviewReport{}, time.Time{}, errors.New("clean formal fixture needs one frozen file")
	}
	groupID := "group-001-clean"
	groups := []pireviewmap.PiSnapshotGroup{{
		ID: groupID, Key: "formal-clean", PatchDigest: formalTestSHA([]byte(strings.Join(patches, "\n\n"))),
		Files: paths,
	}}
	skills := make([]pireviewmap.PiSnapshotSkill, 0, len(request.Plan.ReviewDimensions))
	for _, skill := range request.Plan.ReviewDimensions {
		skills = append(skills, pireviewmap.PiSnapshotSkill{
			ID: skill.ID, Revision: skill.Revision, Digest: "sha256:" + skill.SHA256, Bytes: 1,
		})
	}
	knowledge := make([]pireviewmap.PiSnapshotKnowledge, len(request.Plan.Knowledge))
	for index, ref := range request.Plan.Knowledge {
		knowledge[index] = pireviewmap.PiSnapshotKnowledge{
			ID: ref.ID, Digest: "sha256:" + ref.SHA256,
			Bytes: uint64(request.KnowledgePacks[index].Artifact.Ref.SizeBytes),
		}
	}
	tasks := []pireviewmap.PiTaskObservation{
		cleanFormalTask(request.Plan.CreatedAt, completedAt, groupID, "context", nil, "context-clean"),
	}
	for index := range request.Plan.ReviewDimensions {
		skillID := request.Plan.ReviewDimensions[index].ID
		tasks = append(tasks, cleanFormalTask(
			request.Plan.CreatedAt, completedAt, groupID, "review", &skillID, "review-clean-"+skillID,
		))
	}
	canonicalPatchDigest := "sha256:" + formalTestSHA([]byte(input.CanonicalPatch))
	zeroUsage := formalZeroPiUsage()
	report := pireviewmap.PiReviewReport{
		SchemaVersion: pireviewmap.PiReviewReportSchemaVersion,
		Status:        "complete", Provider: request.Plan.Provider.ID,
		ProviderProfile: providerProfile, Model: request.Plan.Model.ID,
		Target: pireviewmap.PiTarget{
			Kind: "files", Repository: "memory://review-input/" + pireviewmap.JSEncodeURIComponent(input.TargetID),
			Digest: inputDigest(request), CapturedAt: generatedAt, GeneratedAt: &generatedAt,
			CanonicalPatchDigest: &canonicalPatchDigest, Files: files, Skipped: []pireviewmap.PiSkipped{},
		},
		Coverage: pireviewmap.PiCoverage{
			GroupsTotal: 1, GroupsReviewed: 1,
			ReviewTasksTotal:     uint32(len(request.Plan.ReviewDimensions)),
			ReviewTasksSucceeded: uint32(len(request.Plan.ReviewDimensions)),
			VerificationEnabled:  true, VerificationTasksTotal: 0,
			VerificationTasksSucceeded: 0, FilesIncluded: uint32(len(files)),
			Skipped: []pireviewmap.PiSkipped{}, ContextGaps: []string{},
			Failures: []pireviewmap.PiFailure{},
		},
		Summary:  pireviewmap.PiSummary{},
		Findings: []pireviewmap.PiCandidate{}, Candidates: []pireviewmap.PiCandidate{},
		RawCandidates:          []pireviewmap.PiRawCandidate{},
		NormalizationDecisions: []pireviewmap.PiNormalizationDecision{},
		Execution: pireviewmap.PiExecutionEnvelope{
			Authority: "diagnostic_only", ProvenanceClass: "worker_self_report",
			TaskEvidence: pireviewmap.PiTaskEvidenceCollection{
				SchemaVersion: "argus.pi-review.task_evidence.v0",
				Authority:     "diagnostic_only", ProvenanceClass: "worker_self_report",
				ContentPolicy: "exact_local_sensitive", Completeness: "partial",
				ReasonCodes: []string{"task_evidence_unavailable"}, Tasks: []pireviewmap.PiTaskExecutionEvidence{},
			},
			Snapshot: pireviewmap.PiExecutionSnapshot{
				SchemaVersion:        "argus.pi-review.execution_snapshot.v0",
				SnapshotDigest:       "sha256:" + formalTestSHA([]byte(request.WorkItemID)),
				CreatedAt:            request.Plan.CreatedAt,
				Target:               pireviewmap.PiSnapshotTarget{Kind: "files", Digest: inputDigest(request)},
				WorkflowRevision:     workflowRevision,
				PromptBundleRevision: request.PromptBundle.Ref.Revision,
				PromptBundleDigest:   promptDigest,
				Grouping: pireviewmap.PiSnapshotGrouping{
					ImplementationRevision: "directory-language-v0", Groups: groups,
				},
				Skills: skills, Knowledge: knowledge,
				Runtime: pireviewmap.PiSnapshotRuntime{
					Implementation: "@argus/pi-review", Version: "0.1.0",
					Node: "test", PiAgentCore: "0.84.1", PiAI: "0.84.1",
				},
				Provider: pireviewmap.PiSnapshotProvider{
					Provider: request.Plan.Provider.ID, Profile: providerProfile,
					Protocol: "anthropic-messages", Model: request.Plan.Model.ID,
				},
				ToolPolicy: pireviewmap.PiSnapshotToolPolicy{
					Mode: "read_only", AllowedTools: append([]string{}, request.Plan.ToolPolicy.AllowedTools...),
				},
				Budgets: pireviewmap.PiSnapshotBudgets{
					Concurrency:         request.Plan.Budget.MaxConcurrency,
					MaxToolCallsPerTask: request.Plan.Budget.MaxToolCalls,
					MaxFiles:            request.Plan.Budget.MaxFiles, MaxGroups: request.Plan.Budget.MaxGroups,
					MaxGroupBytes:          request.Plan.Budget.MaxGroupBytes,
					MaxTargetBytes:         request.Plan.Budget.MaxTargetBytes,
					MaxCandidates:          request.Plan.Budget.MaxCandidates,
					MaxProviderTurns:       request.Plan.Budget.MaxModelCalls,
					MaxOutputTokensPerTurn: request.Plan.Budget.MaxOutputTokens,
					TaskTimeoutMS:          request.Plan.Budget.TimeoutMS,
				},
				VerificationPolicy: "independent_required",
				Replayability: pireviewmap.PiReplayability{
					Status: "non_replayable", Reasons: []string{"direct_provider_execution_not_platform_attested"},
				},
			},
			Tasks: tasks, Usage: zeroUsage,
		},
	}
	if confirmedFinding {
		if err := addCleanFormalConfirmedFinding(&report, request, input, groupID, completedAt); err != nil {
			return pireviewmap.PiReviewReport{}, time.Time{}, err
		}
	}
	return report, completedAt, nil
}

func addCleanFormalConfirmedFinding(
	report *pireviewmap.PiReviewReport,
	request contractsv1alpha1.AgentReviewWorkerRequest,
	input reviewcore.ReviewInput,
	groupID string,
	completedAt time.Time,
) error {
	if report == nil || len(input.Files) == 0 || input.Files[0].Content == nil ||
		len(request.Plan.ReviewDimensions) == 0 {
		return errors.New("formal confirmed fixture requires a frozen file and review dimension")
	}
	lines := strings.Split(strings.TrimSuffix(*input.Files[0].Content, "\n"), "\n")
	if len(lines) < 2 {
		return errors.New("formal confirmed fixture requires a second source line")
	}
	anchor := pireviewmap.PiSourceAnchor{
		Path: input.Files[0].Path, Side: "file", StartLine: 2, EndLine: 2,
	}
	claim := pireviewmap.PiCandidateClaim{
		Category: "correctness", Severity: "high", Title: "unfinished review-sensitive logic",
		Description: "the selected TODO marks an unfinished correctness path",
		Impact:      "the intended behavior can remain unimplemented",
		Anchor:      anchor,
		Evidence: []pireviewmap.PiEvidence{{
			Statement: "the frozen selected line contains the unfinished marker",
			Anchor:    anchor, Excerpt: lines[1],
		}},
	}
	fingerprint, err := pireviewmap.PiClusterFingerprint(claim)
	if err != nil {
		return err
	}
	candidateID := "candidate-" + fingerprint[:16]
	skill := pireviewmap.PiSkillRef{
		ID:       request.Plan.ReviewDimensions[0].ID,
		Revision: request.Plan.ReviewDimensions[0].Revision,
	}
	raw := pireviewmap.PiRawCandidate{
		RawCandidateID: "raw-confirmed-fixture-1", GroupID: groupID,
		Skill: skill, Ordinal: 0, Claim: claim,
	}
	verification := pireviewmap.PiVerification{
		CandidateID: candidateID, Verdict: "confirmed", ReasonCode: "frozen_marker_present",
		Explanation: "the exact frozen line contains the review-sensitive marker",
		Evidence:    claim.Evidence,
	}
	candidate := pireviewmap.PiCandidate{
		PiCandidateClaim: claim, ID: candidateID, Fingerprint: fingerprint,
		GroupID: groupID, Skill: skill, Verification: &verification,
	}
	report.RawCandidates = []pireviewmap.PiRawCandidate{raw}
	report.NormalizationDecisions = []pireviewmap.PiNormalizationDecision{{
		RawCandidateID: raw.RawCandidateID, Action: "retained",
		ReasonCode: "normalized_candidate_retained", NormalizedCandidateID: &candidateID,
	}}
	report.Candidates = []pireviewmap.PiCandidate{candidate}
	report.Findings = []pireviewmap.PiCandidate{candidate}
	report.Summary = pireviewmap.PiSummary{Candidates: 1, Confirmed: 1}
	report.Coverage.VerificationTasksTotal = 1
	report.Coverage.VerificationTasksSucceeded = 1
	verificationData, err := json.Marshal(verification)
	if err != nil {
		return err
	}
	task := cleanFormalTask(
		request.Plan.CreatedAt, completedAt, groupID, "verification", nil,
		"verification-clean-"+candidateID,
	)
	task.CandidateID = &candidateID
	outputDigest := "sha256:" + formalTestSHA(verificationData)
	task.OutputDigest = &outputDigest
	report.Execution.Tasks = append(report.Execution.Tasks, task)
	return nil
}

func cleanFormalTask(
	createdAt time.Time,
	completedAt time.Time,
	groupID string,
	kind string,
	skillID *string,
	taskID string,
) pireviewmap.PiTaskObservation {
	outputDigest := "sha256:" + formalTestSHA([]byte(taskID+"-output"))
	tool := "submit_context"
	if kind == "review" {
		tool = "submit_candidates"
	} else if kind == "verification" {
		tool = "submit_verdict"
	}
	return pireviewmap.PiTaskObservation{
		TaskID: taskID, TaskKind: kind, GroupID: groupID, SkillID: skillID,
		PromptDigest:         "sha256:" + formalTestSHA([]byte(taskID+"-prompt")),
		ProviderTurnsStarted: 1, ProviderTurnsCompleted: 1,
		ToolCalls: 1, ToolNames: []string{tool},
		ToolUsage: []pireviewmap.PiToolUsage{{ToolID: tool, InvocationCount: 1}},
		Usage:     formalZeroPiUsage(), StartedAt: createdAt,
		FinishedAt: completedAt, DurationMS: uint64(completedAt.Sub(createdAt) / time.Millisecond),
		TerminalStatus: "succeeded", OutputDigest: &outputDigest,
	}
}

func formalZeroPiUsage() pireviewmap.PiUsage {
	zero := uint64(0)
	return pireviewmap.PiUsage{
		Completeness: "provider_reported", InputTokens: &zero, OutputTokens: &zero,
		CacheReadTokens: &zero, CacheWriteTokens: &zero, TotalTokens: &zero,
	}
}

func inputDigest(request contractsv1alpha1.AgentReviewWorkerRequest) string {
	return request.Plan.TargetDigest
}

func formalTestSHA(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestAgentReviewHelpAndFlagParsing(t *testing.T) {
	var output bytes.Buffer
	if err := runWithIO(t.Context(), []string{"agent-review", "--help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "agent-review run") ||
		!strings.Contains(output.String(), "agent-review show") {
		t.Fatalf("agent-review help = %q", output.String())
	}

	store := filepath.Join(t.TempDir(), "store")
	options, err := parseAgentReviewRunFlags([]string{
		"--store", store,
		"--source-run", "source-run-1",
		"--idempotency-key", "agent-review-1",
		"--node", "/absolute/node",
		"--worker-script", "/absolute/pi-review/dist/worker.js",
		"--provider-profile", "deepseek-anthropic-env",
		"--model", "deepseek-chat",
		"--skill", "correctness",
		"--skill", "security-contract",
		"--max-candidates", "7",
		"--allow-partial",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.store != store || options.budget.MaxCandidates != 7 ||
		!options.allowPartial ||
		!reflectStringSlices([]string(options.skills), []string{"correctness", "security-contract"}) {
		t.Fatalf("parsed run flags = %+v", options)
	}
	show, err := parseAgentReviewShowFlags([]string{
		"--store", store, "--manifest-id", "agent-review-manifest-1", "--json",
	})
	if err != nil || show.manifestID != "agent-review-manifest-1" || !show.json {
		t.Fatalf("parsed show flags = %+v, %v", show, err)
	}
	if _, err := parseAgentReviewRunFlags([]string{"--store", "relative"}); err == nil ||
		!strings.Contains(err.Error(), "clean absolute") {
		t.Fatalf("relative store error = %v", err)
	}
}

func TestAgentReviewFailedRunPrintsQueryableAttemptBeforeError(t *testing.T) {
	attempt := agentshadow.ExecutionAttempt{
		Status:      agentshadow.ExecutionStatusUnknownOutcome,
		ExecutionID: "agent-review-execution-fixture",
		SourceRunID: "run-fixture",
		ObservedAt:  time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
	}
	result := agentshadow.LocalRunResult{
		Attempt: &attempt,
		Result: agentshadow.Result{
			Manifest: contractsv1alpha1.AgentReviewResultManifest{
				ManifestID: "unconfirmed-manifest", Status: contractsv1alpha1.AgentReviewRunComplete,
			},
			Observation: contractsv1alpha1.AgentReviewObservation{
				ObservationID: "unconfirmed-observation",
			},
		},
		Reused: true,
	}
	var output bytes.Buffer
	if err := writeAgentReviewRunAttempt(&output, true, "/tmp/argus-store", result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"outcome_acknowledged":false`) {
		t.Fatalf("JSON attempt omitted negative outcome acknowledgement: %s", output.String())
	}
	var decoded agentReviewRunAttemptOutput
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode attempt output: %v\n%s", err, output.String())
	}
	if decoded.Attempt.ExecutionID != attempt.ExecutionID || !decoded.Reused ||
		decoded.OutcomeAcknowledged ||
		decoded.StorePath != "/tmp/argus-store" ||
		decoded.UnconfirmedCommittedResult == nil ||
		decoded.UnconfirmedCommittedResult.ManifestID != "unconfirmed-manifest" {
		t.Fatalf("attempt output = %+v", decoded)
	}
	output.Reset()
	if err := writeAgentReviewRunAttempt(&output, false, "/tmp/argus-store", result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "outcome_acknowledged=false") {
		t.Fatalf("plain attempt omitted negative outcome acknowledgement: %q", output.String())
	}
}

func TestAgentReviewSucceededButUnacknowledgedOutputIsExplicit(t *testing.T) {
	attempt := agentshadow.ExecutionAttempt{
		Status:      agentshadow.ExecutionStatusSucceeded,
		ExecutionID: "execution-succeeded-unacknowledged",
		SourceRunID: "run-fixture",
		ObservedAt:  time.Date(2026, 8, 24, 2, 3, 4, 0, time.UTC),
	}
	result := agentshadow.LocalRunResult{Attempt: &attempt, OutcomeAcknowledged: false}
	for _, jsonOutput := range []bool{false, true} {
		var output bytes.Buffer
		if err := writeAgentReviewRunAttempt(
			&output,
			jsonOutput,
			"/tmp/argus-store",
			result,
		); err != nil {
			t.Fatal(err)
		}
		want := "outcome_acknowledged=false"
		if jsonOutput {
			want = `"outcome_acknowledged":false`
		}
		if !strings.Contains(output.String(), want) {
			t.Fatalf("succeeded-but-unacknowledged output %q omitted %q", output.String(), want)
		}
	}
}

func TestAgentReviewAcknowledgedResultOutputIsExplicit(t *testing.T) {
	result := agentshadow.LocalRunResult{
		OutcomeAcknowledged: true,
		Result: agentshadow.Result{
			Manifest: contractsv1alpha1.AgentReviewResultManifest{
				Status: contractsv1alpha1.AgentReviewRunComplete, ManifestID: "manifest-acknowledged",
			},
			HypothesisSet: contractsv1alpha1.ReviewHypothesisSet{
				Hypotheses: []contractsv1alpha1.ReviewHypothesis{},
			},
		},
	}
	for _, jsonOutput := range []bool{false, true} {
		var output bytes.Buffer
		if err := writeAgentReviewRunResult(&output, jsonOutput, false, result); err != nil {
			t.Fatal(err)
		}
		want := "outcome_acknowledged=true"
		if jsonOutput {
			want = `"outcome_acknowledged":true`
		}
		if !strings.Contains(output.String(), want) {
			t.Fatalf("acknowledged result output %q omitted %q", output.String(), want)
		}
	}
}

func TestAgentReviewShowWiringReturnsNotFound(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	err := runWithIO(t.Context(), []string{
		"agent-review", "show", "--store", store, "--manifest-id", "missing-manifest",
	}, &bytes.Buffer{})
	if !errors.Is(err, agentshadow.ErrNotFound) {
		t.Fatalf("agent-review show error = %v, want ErrNotFound", err)
	}
}

func TestAgentReviewRunExitSemanticsPrintBeforePartialOrFailedError(t *testing.T) {
	for _, test := range []struct {
		name         string
		status       contractsv1alpha1.AgentReviewRunStatus
		allowPartial bool
		wantError    string
	}{
		{name: "complete", status: contractsv1alpha1.AgentReviewRunComplete},
		{name: "partial defaults nonzero", status: contractsv1alpha1.AgentReviewRunPartial, wantError: "coverage is partial"},
		{name: "partial explicitly allowed", status: contractsv1alpha1.AgentReviewRunPartial, allowPartial: true},
		{name: "failed", status: contractsv1alpha1.AgentReviewRunFailed, wantError: "agent review failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := agentshadow.LocalRunResult{OutcomeAcknowledged: true, Result: agentshadow.Result{
				Manifest: contractsv1alpha1.AgentReviewResultManifest{
					Status: test.status, ManifestID: "manifest-fixture",
				},
				HypothesisSet: contractsv1alpha1.ReviewHypothesisSet{
					Hypotheses: []contractsv1alpha1.ReviewHypothesis{},
				},
			}}
			var output bytes.Buffer
			err := writeAgentReviewRunResult(&output, false, test.allowPartial, result)
			if !strings.Contains(output.String(), "manifest=manifest-fixture") ||
				!strings.Contains(output.String(), "outcome_acknowledged=true") {
				t.Fatalf("result was not printed before status error: %q", output.String())
			}
			if test.wantError == "" && err != nil {
				t.Fatalf("unexpected status error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("status error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func reflectStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
