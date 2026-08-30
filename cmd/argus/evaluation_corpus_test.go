package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

func TestEvaluationCorpusCLIExecutesTwoFormalCasesAndRecoversExactly(t *testing.T) {
	const providerSecret = "formal-corpus-provider-secret-must-not-persist"
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
	storePath := t.TempDir()
	configState := t.TempDir()
	firstSource := createFormalCorpusSourceRun(t, repositoryPath, storePath, "first")
	secondSource := createFormalCorpusSourceRun(t, repositoryPath, storePath, "second")

	state, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := runs.LoadRun(firstSource)
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := runs.LoadRun(secondSource)
	if err != nil {
		t.Fatal(err)
	}
	evaluationRepository, err := evaluation.New(state)
	if err != nil {
		t.Fatal(err)
	}
	caseAt := time.Now().UTC().Add(-10 * time.Minute)
	roles := []evaluation.Role{evaluation.RoleHoldoutMaintainer, evaluation.RoleHoldoutRunner}
	firstCase := testCLIEvaluationCase("formal-corpus-case-a", "formal-corpus-source-a", evaluation.SplitHoldout, caseAt)
	firstCase.InputSnapshotRef = firstRun.TargetSnapshotRef.URI
	secondCase := testCLIEvaluationCase("formal-corpus-case-b", "formal-corpus-source-b", evaluation.SplitHoldout, caseAt)
	secondCase.InputSnapshotRef = secondRun.TargetSnapshotRef.URI
	firstImport := testCLIExternalGovernedImport(t, firstCase)
	registeredAt := caseAt.Add(-time.Minute)
	if _, err := evaluationRepository.RegisterGovernanceTrustKey(context.Background(), evaluation.GovernanceTrustKeyRegistration{
		SchemaVersion: evaluation.GovernanceTrustKeyRegistrationSchemaVersion,
		Key:           firstImport.TrustedKey, RegisteredAt: registeredAt,
	}, evaluation.Mutation{
		IdempotencyKey: "formal-corpus-register-key", Actor: "formal-corpus-trust-admin",
		Roles: []evaluation.Role{evaluation.RoleGovernanceTrustAdmin}, Audit: "register formal corpus key", At: registeredAt,
	}); err != nil {
		t.Fatal(err)
	}
	for index, item := range []evaluation.ExternalGovernedCaseImport{firstImport, testCLIExternalGovernedImport(t, secondCase)} {
		if _, err := evaluationRepository.ImportGovernedCase(context.Background(), item, evaluation.Mutation{
			IdempotencyKey: []string{"formal-corpus-import-a", "formal-corpus-import-b"}[index],
			Actor:          "formal-corpus-runner", Roles: roles, Audit: "import governed formal corpus case", At: caseAt,
		}); err != nil {
			t.Fatal(err)
		}
	}

	observations := []evaluation.ExposureObservation{
		{Component: evaluation.ExposurePrompt, Status: evaluation.ExposureNotSeen, Revision: "prompt-1"},
		{Component: evaluation.ExposureRule, Status: evaluation.ExposureNotSeen, Revision: "rule-1"},
		{Component: evaluation.ExposureModel, Status: evaluation.ExposureNotSeen, Revision: "model-1"},
		{Component: evaluation.ExposureIndex, Status: evaluation.ExposureNotSeen, Revision: "index-1"},
	}
	batchAt := caseAt.Add(2 * time.Minute)
	accessPath := writeBatchJSON(t, "formal-corpus-access.json", evaluation.Access{Actor: "formal-corpus-runner", Roles: roles})
	snapshotRequest := evaluation.CorpusSnapshotRequest{
		SchemaVersion: evaluation.CorpusSnapshotRequestSchemaVersion,
		CorpusID:      "argus-self-bootstrap", Revision: "revision-1",
		Purpose: evaluation.CorpusPurposePromotionGate, Split: evaluation.SplitHoldout,
		Cases: []evaluation.CorpusSnapshotCaseRequest{
			{CaseID: firstCase.CaseID, ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1, SourceReviewRunID: firstSource},
			{CaseID: secondCase.CaseID, ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1, SourceReviewRunID: secondSource},
		},
		CreatedAt: batchAt.Add(-time.Minute),
	}
	snapshotRequestPath := writeBatchJSON(t, "formal-corpus-snapshot-request.json", snapshotRequest)
	var snapshotOutputBytes bytes.Buffer
	if err := runEvaluationCorpus(t.Context(), []string{
		"snapshot", "build", "--store", storePath, "--input", snapshotRequestPath,
		"--access", accessPath, "--json",
	}, &snapshotOutputBytes); err != nil {
		t.Fatalf("build corpus snapshot: %v", err)
	}
	var snapshotOutput corpusSnapshotOutput
	decodeCLIOutput(t, snapshotOutputBytes.Bytes(), &snapshotOutput)
	if len(snapshotOutput.Snapshot.Cases) != 2 || snapshotOutput.SnapshotRef.Contract != evaluation.CorpusSnapshotContract {
		t.Fatalf("corpus snapshot output=%+v", snapshotOutput)
	}
	request := evaluation.FormalCorpusBatchRequest{
		SchemaVersion: evaluation.FormalCorpusBatchRequestSchemaVersion,
		BatchID:       "formal-corpus-cli-batch", EvaluationRunID: "formal-corpus-cli-evaluation",
		EvaluatorRevision: "defect-presence-1", ExecutorRevision: localFormalCorpusExecutorRevision,
		CorpusSnapshotRef: snapshotOutput.SnapshotRef, MaxConcurrency: 1,
		Cases: []evaluation.FormalCorpusBatchCase{
			{CaseID: firstCase.CaseID, ExpectedLabelRevision: 1, SourceReviewRunID: firstSource, ExposureObservations: observations},
			{CaseID: secondCase.CaseID, ExpectedLabelRevision: 1, SourceReviewRunID: secondSource, ExposureObservations: observations},
		},
		CreatedAt: batchAt,
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: "formal-corpus-cli-intent", Actor: "formal-corpus-runner", Roles: roles,
		Audit: "execute governed formal corpus", At: batchAt,
	}
	requestPath := writeBatchJSON(t, "formal-corpus-request.json", request)
	mutationPath := writeBatchJSON(t, "formal-corpus-mutation.json", mutation)
	arguments := []string{
		"--store", storePath, "--input", requestPath, "--mutation", mutationPath, "--json", "--",
		"--config-state-dir", configState, "--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
	}
	formalFlags, err := parseFormalAgentRunFlags(append([]string{
		"--store", storePath, "--source-run", "placeholder-source", "--idempotency-key", "placeholder-key",
	}, arguments[argumentsIndexAfterSeparator(arguments):]...))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(formalFlags.options)
	if err != nil {
		t.Fatal(err)
	}
	workerRunner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	var first bytes.Buffer
	if err := runEvaluationCorpusExecutionWithRunner(t.Context(), arguments, &first, workerRunner); err != nil {
		t.Fatalf("formal corpus run: %v\n%s", err, first.String())
	}
	var output formalCorpusOutput
	decodeCLIOutput(t, first.Bytes(), &output)
	if len(output.Result.Cases) != 2 || output.Result.EvaluationRunID != request.EvaluationRunID || workerRunner.calls != 2 {
		t.Fatalf("formal corpus output=%+v calls=%d", output, workerRunner.calls)
	}
	evaluationRun, err := evaluationRepository.GetEvaluationRun(request.EvaluationRunID, evaluation.Access{Actor: mutation.Actor, Roles: roles})
	if err != nil || len(evaluationRun.Results) != 2 || evaluationRun.Summary.Passed != 2 {
		t.Fatalf("evaluation run=%+v error=%v", evaluationRun, err)
	}

	var shown bytes.Buffer
	if err := runEvaluationCorpus(t.Context(), []string{"show", "--store", storePath, "--batch", request.BatchID, "--access", accessPath, "--json"}, &shown); err != nil {
		t.Fatal(err)
	}
	var recordOutput formalCorpusRecordOutput
	decodeCLIOutput(t, shown.Bytes(), &recordOutput)
	if recordOutput.Record.Status != evaluation.FormalCorpusBatchSucceeded || recordOutput.Record.Result == nil ||
		len(recordOutput.Record.CompletedCases) != 2 || recordOutput.Record.Request.ExecutorTemplateRef == nil ||
		!reflect.DeepEqual(recordOutput.Record.Intent, mutation) {
		t.Fatalf("formal corpus record=%+v", recordOutput.Record)
	}
	templateBytes, err := runs.ReadArtifact(*recordOutput.Record.Request.ExecutorTemplateRef)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(templateBytes, []byte(providerSecret)) {
		t.Fatal("formal corpus template persisted provider credential")
	}

	var retry bytes.Buffer
	if err := runEvaluationCorpusExecutionWithRunner(t.Context(), arguments, &retry, workerRunner); err != nil {
		t.Fatal(err)
	}
	if retry.String() != first.String() || workerRunner.calls != 2 {
		t.Fatalf("retry changed output or reran provider: calls=%d", workerRunner.calls)
	}
	var resumed bytes.Buffer
	if err := runEvaluationCorpusResumeWithRunner(t.Context(), []string{"--store", storePath, "--batch", request.BatchID, "--access", accessPath, "--json"}, &resumed, workerRunner); err != nil {
		t.Fatal(err)
	}
	if resumed.String() != first.String() || workerRunner.calls != 2 {
		t.Fatalf("resume changed output or reran provider: calls=%d", workerRunner.calls)
	}
}

func createFormalCorpusSourceRun(t *testing.T, repositoryPath, storePath, marker string) string {
	t.Helper()
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n// "+marker+" clean review target\n")
	revision := commitCLITarget(t, repositoryPath, "formal corpus "+marker)
	var output bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"review", "--repo", repositoryPath, "--mode", "selection", "--revision", revision,
		"--path", "review.go", "--start-line", "2", "--end-line", "2", "--store", storePath, "--json",
	}, &output); err != nil {
		t.Fatalf("create formal corpus source %s: %v", marker, err)
	}
	var reviewed runOutput
	decodeCLIOutput(t, output.Bytes(), &reviewed)
	return reviewed.Run.RunID
}

func argumentsIndexAfterSeparator(arguments []string) int {
	for index, argument := range arguments {
		if argument == "--" {
			return index + 1
		}
	}
	return len(arguments)
}
