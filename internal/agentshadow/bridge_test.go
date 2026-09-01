package agentshadow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/reviewcore"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type fakeShadowRunner struct {
	mu       sync.Mutex
	calls    int
	behavior func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error)
}

func TestLocalBridgePersistsExactPiTaskEvidence(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		report, completedAt := fakePiReport(t, request, "clean", 4)
		*fixture.now = completedAt.Add(time.Second)
		attachExactPiTaskEvidence(t, &report)
		reportData, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		digest := digestBytesRaw(reportData)
		result := contractsv1alpha1.AgentReviewWorkerResult{
			SchemaVersion: contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
			WorkItemID:    request.WorkItemID, Attempt: request.Attempt,
			Generation: request.Generation, FencingToken: request.FencingToken,
			IdempotencyKey: request.IdempotencyKey, CapabilitySHA256: request.Capability.SHA256,
			Status:       contractsv1alpha1.AgentReviewWorkerSucceeded,
			ReportSHA256: &digest, Report: reportData, CompletedAt: completedAt,
		}
		return json.Marshal(result)
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	result, err := bridge.Run(t.Context(), localBridgeRequest(fixture, node, script, "exact-task-evidence"))
	if err != nil {
		t.Fatal(err)
	}
	evidence := result.Result.TaskEvidenceCollection
	if evidence.Completeness != contractsv1alpha1.AgentReviewTaskEvidenceComplete ||
		len(evidence.TaskExecutions) != len(result.Result.ReceiptCollection.Receipts) ||
		evidence.TaskExecutions[0].SystemPrompt.Content == nil ||
		result.Result.Manifest.AgentTaskEvidenceRef != result.Result.Record.TaskEvidenceCollectionRef {
		t.Fatalf("exact task evidence was not persisted and bound: %+v", evidence)
	}
}

func attachExactPiTaskEvidence(t *testing.T, report *piReviewReport) {
	t.Helper()
	contents := func(value string) piTaskEvidenceContent {
		return piTaskEvidenceContent{
			SHA256: digestBytesRaw([]byte(value)), SizeBytes: uint64(len([]byte(value))), Content: &value,
		}
	}
	tasks := make([]piTaskExecutionEvidence, 0, len(report.Execution.Tasks))
	for index := range report.Execution.Tasks {
		observation := &report.Execution.Tasks[index]
		systemPrompt := "exact system prompt"
		userPrompt := "exact user prompt with value < limit && next > 0"
		pairData := []byte(`["exact system prompt","exact user prompt with value < limit && next > 0"]`)
		observation.PromptDigest = "sha256:" + digestBytesRaw(pairData)
		output := `{"accepted":true}`
		outputDigest := "sha256:" + digestBytesRaw([]byte(output))
		observation.OutputDigest = &outputDigest
		arguments := contents(`{"value":1}`)
		toolResult := contents(`{"accepted":true}`)
		isError := false
		outputContent := contents(output)
		tasks = append(tasks, piTaskExecutionEvidence{
			TaskID: observation.TaskID, TaskKind: observation.TaskKind, GroupID: observation.GroupID,
			SkillID: observation.SkillID, CandidateID: observation.CandidateID,
			SystemPrompt: contents(systemPrompt), UserPrompt: contents(userPrompt),
			Output: &outputContent, TerminalStatus: observation.TerminalStatus,
			Tools: []piTaskToolEvidence{{
				Sequence: 1, ToolName: observation.ToolNames[0], Arguments: arguments,
				Result: &toolResult, IsError: &isError,
			}},
		})
	}
	report.Execution.TaskEvidence = piTaskEvidenceCollection{
		SchemaVersion: "argus.pi-review.task_evidence.v0",
		Authority:     "diagnostic_only", ProvenanceClass: "worker_self_report",
		ContentPolicy: "exact_local_sensitive", Completeness: "complete",
		ReasonCodes: []string{}, Tasks: tasks,
	}
}

func (runner *fakeShadowRunner) Run(
	_ context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(request.Input)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	runner.mu.Lock()
	runner.calls++
	behavior := runner.behavior
	runner.mu.Unlock()
	output, err := behavior(workerRequest)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	return agentshadowworker.Result{Stdout: output}, nil
}

func (runner *fakeShadowRunner) callCount() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.calls
}

func TestLocalBridgeMapsCleanDefectAndPartialReports(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{}
	runner.behavior = func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		mode := "clean"
		if strings.Contains(request.IdempotencyKey, "defect") {
			mode = "defect"
		}
		if strings.Contains(request.IdempotencyKey, "partial") {
			mode = "partial"
		}
		if strings.Contains(request.IdempotencyKey, "invalid") {
			mode = "invalid"
		}
		output := fakeSucceededWorkerResult(t, request, mode, 4)
		*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
		return output, nil
	}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}

	clean, err := bridge.Run(t.Context(), localBridgeRequest(fixture, node, script, "bridge-clean"))
	if err != nil {
		t.Fatalf("clean bridge run: %v", err)
	}
	if clean.Reused || clean.Result.HypothesisSet.Completeness != contractsv1alpha1.AgentReviewComplete ||
		len(clean.Result.HypothesisSet.Hypotheses) != 0 ||
		len(clean.Result.ReceiptCollection.Receipts) != 1+len(defaultLocalSkills) {
		t.Fatalf("clean mapped result = %+v", clean)
	}

	defectRequest := localBridgeRequest(fixture, node, script, "bridge-defect")
	defect, err := bridge.Run(t.Context(), defectRequest)
	if err != nil {
		t.Fatalf("defect bridge run: %v", err)
	}
	if defect.Result.HypothesisSet.Completeness != contractsv1alpha1.AgentReviewComplete ||
		len(defect.Result.HypothesisSet.Hypotheses) != 1 ||
		len(defect.Result.HypothesisSet.Hypotheses[0].Verification) != 1 ||
		defect.Result.HypothesisSet.Hypotheses[0].Verification[0].Verdict !=
			contractsv1alpha1.HypothesisVerificationConfirmed {
		t.Fatalf("defect mapped result = %+v", defect.Result.HypothesisSet)
	}

	partial, err := bridge.Run(t.Context(), localBridgeRequest(fixture, node, script, "bridge-partial"))
	if err != nil {
		t.Fatalf("partial bridge run: %v", err)
	}
	if partial.Result.HypothesisSet.Completeness != contractsv1alpha1.AgentReviewPartial ||
		len(partial.Result.HypothesisSet.Hypotheses) != 1 ||
		len(partial.Result.HypothesisSet.Hypotheses[0].Verification) != 0 ||
		len(partial.Result.HypothesisSet.Coverage.Gaps) == 0 {
		t.Fatalf("partial verifier failure was not retained as hypothesis+gap: %+v", partial.Result.HypothesisSet)
	}

	invalid, err := bridge.Run(t.Context(), localBridgeRequest(fixture, node, script, "bridge-invalid"))
	if err != nil {
		t.Fatalf("invalid-candidate bridge run: %v", err)
	}
	if invalid.Result.HypothesisSet.Completeness != contractsv1alpha1.AgentReviewPartial ||
		len(invalid.Result.HypothesisSet.Hypotheses) != 0 ||
		len(invalid.Result.RawCandidateCollection.RawCandidates) != 1 ||
		invalid.Result.RawCandidateCollection.RawCandidates[0].Action !=
			contractsv1alpha1.HypothesisNormalizationRejectedInvalid ||
		invalid.Result.RawCandidateCollection.RawCandidates[0].Claim.Title !=
			"possible nil dereference" ||
		invalid.Result.Manifest.RawCandidateCollectionRef !=
			invalid.Result.Record.RawCandidateCollectionRef {
		t.Fatalf("rejected-invalid payload was not governed and queryable: %+v", invalid.Result)
	}

	reused, err := bridge.Run(t.Context(), defectRequest)
	if err != nil || !reused.Reused || !reflect.DeepEqual(reused.Result, defect.Result) {
		t.Fatalf("idempotent bridge reuse = %+v, %v", reused, err)
	}
	if runner.callCount() != 4 {
		t.Fatalf("worker calls = %d, want one per committed key", runner.callCount())
	}
}

func TestLocalBridgeConflictsWhenRequestOrRuntimePackageChanges(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		output := fakeSucceededWorkerResult(t, request, "clean", 4)
		*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
		return output, nil
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "bridge-conflict")
	if _, err := bridge.Run(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	changedModel := request
	changedModel.Model = "deepseek-other-model"
	if _, err := bridge.Run(t.Context(), changedModel); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed model error = %v, want ErrConflict", err)
	}
	protocol := filepath.Join(filepath.Dir(script), "protocol.js")
	if err := os.WriteFile(protocol, []byte("export const revision = 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Run(t.Context(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed imported runtime module error = %v, want ErrConflict", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("conflicting retry spawned worker %d times", runner.callCount())
	}
}

func TestLocalBridgeOrphanIntentIsNeverRerun(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("transport disappeared after spawn")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "bridge-orphan")
	if _, err := bridge.Run(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "retry is unsafe") {
		t.Fatalf("first orphan error = %v", err)
	}
	if _, err := bridge.Run(t.Context(), request); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("orphan retry error = %v, want ErrUnknownOutcome", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("orphan retry spawned %d workers", runner.callCount())
	}
}

func TestLocalBridgePersistsOnlyRedactedWorkerFailure(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	const marker = "secret-provider-payload"
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		output := fakeFailedWorkerResult(t, request, marker)
		*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
		return output, nil
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "bridge-failed")
	for attempt := 0; attempt < 2; attempt++ {
		_, err := bridge.Run(t.Context(), request)
		var reported *WorkerReportedError
		if !errors.As(err, &reported) || strings.Contains(err.Error(), marker) ||
			reported.Failure.Message != redactedWorkerFailureMessage {
			t.Fatalf("worker failure attempt %d = %#v, %v", attempt+1, reported, err)
		}
	}
	if runner.callCount() != 1 {
		t.Fatalf("terminal worker failure retried %d times", runner.callCount())
	}
	var intent executionIntent
	if err := fixture.store.GetJSON(executionIntentObjectID(fixture.scope, request.IdempotencyKey), &intent); err != nil {
		t.Fatal(err)
	}
	var completion executionCompletion
	if err := fixture.store.GetJSON(executionCompletionObjectID(fixture.scope, request.IdempotencyKey), &completion); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), marker) || completion.Failure.Message != redactedWorkerFailureMessage {
		t.Fatalf("untrusted failure leaked into completion: %s", encoded)
	}
	firstRecordedAt := completion.RecordedAt
	completion.RecordedAt = time.Time{}
	*fixture.now = firstRecordedAt.Add(time.Minute)
	reusedCompletion, created, err := fixture.repository.completeExecution(intent, completion)
	if err != nil || created || !reusedCompletion.RecordedAt.Equal(firstRecordedAt) {
		t.Fatalf("exact completion retry = %+v, created=%v, %v", reusedCompletion, created, err)
	}
}

func TestLocalBridgeConcurrentRequestSpawnsOnlyOneWorker(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		close(entered)
		<-release
		output := fakeSucceededWorkerResult(t, request, "clean", 4)
		*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
		return output, nil
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "bridge-concurrent")
	first := make(chan error, 1)
	go func() {
		_, err := bridge.Run(context.Background(), request)
		first <- err
	}()
	<-entered
	if _, err := bridge.Run(t.Context(), request); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("concurrent follower error = %v, want ErrUnknownOutcome", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("worker owner failed: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("concurrent request spawned %d workers", runner.callCount())
	}
}

func TestPiMapperRejectsForgedFrozenEvidenceAndCounters(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	bridge, err := NewLocalBridge(fixture.repository, &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("not called")
	}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := bridge.prepare(t.Context(), localBridgeRequest(fixture, node, script, "mapper-base"))
	if err != nil {
		t.Fatal(err)
	}
	base, completedAt := fakePiReport(t, prepared.workerRequest, "defect", 4)

	tests := []struct {
		name   string
		mutate func(*contractsv1alpha1.AgentReviewPlan, *reviewcore.ReviewInput, *piReviewReport)
		want   string
	}{
		{name: "target patch digest", mutate: func(_ *contractsv1alpha1.AgentReviewPlan, _ *reviewcore.ReviewInput, report *piReviewReport) {
			forged := "sha256:" + strings.Repeat("f", 64)
			report.Target.Files[0].PatchDigest = &forged
		}, want: "does not bind frozen ReviewInput"},
		{name: "receipt tool counter", mutate: func(_ *contractsv1alpha1.AgentReviewPlan, _ *reviewcore.ReviewInput, report *piReviewReport) {
			report.Execution.Tasks[0].ToolCalls++
		}, want: "tool usage is inconsistent"},
		{name: "coverage counter", mutate: func(_ *contractsv1alpha1.AgentReviewPlan, _ *reviewcore.ReviewInput, report *piReviewReport) {
			report.Coverage.ReviewTasksSucceeded--
		}, want: "coverage counters"},
		{name: "verdict output digest", mutate: func(_ *contractsv1alpha1.AgentReviewPlan, _ *reviewcore.ReviewInput, report *piReviewReport) {
			for index := range report.Execution.Tasks {
				if report.Execution.Tasks[index].TaskKind == "verification" {
					forged := "sha256:" + strings.Repeat("f", 64)
					report.Execution.Tasks[index].OutputDigest = &forged
				}
			}
		}, want: "exact verdict JSON"},
		{name: "group file omission", mutate: func(_ *contractsv1alpha1.AgentReviewPlan, _ *reviewcore.ReviewInput, report *piReviewReport) {
			report.Execution.Snapshot.Grouping.Groups[0].Files = []string{}
		}, want: "invalid group"},
		{name: "unavailable file omission", mutate: func(plan *contractsv1alpha1.AgentReviewPlan, input *reviewcore.ReviewInput, report *piReviewReport) {
			input.Files = append(input.Files, reviewcore.FileManifestEntry{
				Path: "zz-unavailable.go", SHA256: digestText(""), SizeBytes: 0, Content: nil,
			})
			bindMutatedReviewInput(t, plan, *input, report)
		}, want: "skipped entries"},
		{name: "frozen context omission", mutate: func(plan *contractsv1alpha1.AgentReviewPlan, input *reviewcore.ReviewInput, report *piReviewReport) {
			input.Contexts = append(input.Contexts, reviewcore.ContextBinding{Gap: &reviewcore.ContextGap{
				ContextID: "context-missing", Kind: "symbol", Revision: "v1", Digest: strings.Repeat("c", 64),
				Coverage:   reviewcore.ContextCoverage{Spans: []reviewcore.ContextSpan{}, Symbols: []string{"demo.run"}},
				Provenance: reviewcore.ContextProvenance{Provider: "fixture", ProducerID: "fixture", ProducerRevision: "v1"},
				ReasonCode: "capture_unavailable",
			}})
			bindMutatedReviewInput(t, plan, *input, report)
		}, want: "required frozen context gap"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := prepared.plan
			input := prepared.reviewInput
			report := clonePiReport(t, base)
			test.mutate(&plan, &input, &report)
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := mapPiReviewReport(plan, input, completedAt, data); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("map forged report error = %v, want %q", err, test.want)
			}
		})
	}

	anchorReport, anchorCompletedAt := fakePiReport(t, prepared.workerRequest, "defect", 1)
	anchorData, err := json.Marshal(anchorReport)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mapPiReviewReport(prepared.plan, prepared.reviewInput, anchorCompletedAt, anchorData); err == nil ||
		!strings.Contains(err.Error(), "outside frozen target authorization") {
		t.Fatalf("forged candidate anchor error = %v", err)
	}

	partial, partialCompletedAt := fakePiReport(t, prepared.workerRequest, "partial", 4)
	for index := range partial.Execution.Tasks {
		if partial.Execution.Tasks[index].TaskKind == "verification" {
			forged := "sha256:" + strings.Repeat("e", 64)
			partial.Execution.Tasks[index].OutputDigest = &forged
		}
	}
	partialData, err := json.Marshal(partial)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mapPiReviewReport(prepared.plan, prepared.reviewInput, partialCompletedAt, partialData); err == nil ||
		!strings.Contains(err.Error(), "non-succeeded Pi task") {
		t.Fatalf("failed task output digest error = %v", err)
	}
}

func TestAgentReviewWorkerCrossLanguageCanceledRoundTrip(t *testing.T) {
	if os.Getenv("ARGUS_PI_WORKER_SMOKE") != "1" {
		t.Skip("set ARGUS_PI_WORKER_SMOKE=1 after pi-build to run the Go/TS stdio smoke")
	}
	fixture := newShadowFixture(t)
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workerScript, err := filepath.Abs(filepath.Join(
		workingDirectory,
		"..", "..", "runtime", "pi-review", "dist", "worker.js",
	))
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for cross-language worker smoke")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	node, err = filepath.EvalSymlinks(node)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := NewLocalBridge(fixture.repository, agentshadowworker.NewSubprocessRunner())
	if err != nil {
		t.Fatal(err)
	}
	localRequest := localBridgeRequest(fixture, node, workerScript, "worker-cross-language-canceled")
	prepared, err := bridge.prepare(t.Context(), localRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.workerRequest.Deadline.Before(time.Now()) {
		t.Fatal("cross-language smoke requires its fixture deadline to have elapsed")
	}
	processResult, err := agentshadowworker.NewSubprocessRunner().Run(t.Context(), agentshadowworker.Request{
		NodePath: node, ScriptPath: workerScript, Input: prepared.workerRequestData,
		Environment: localRequest.Environment, MaxInputBytes: maxWorkerRequestBytes,
		MaxOutputBytes: int64(prepared.workerRequest.Capability.MaxOutputBytes) + 64<<10,
		MaxStderrBytes: agentshadowworker.DefaultMaxStderrBytes,
	})
	if err != nil {
		t.Fatalf("run built TypeScript worker: %v", err)
	}
	result, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(processResult.Stdout)
	if err != nil {
		t.Fatalf("Go decode TypeScript worker result: %v", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewWorkerResultBinding(prepared.workerRequest, result); err != nil {
		t.Fatalf("cross-language worker binding: %v", err)
	}
	if result.Status != contractsv1alpha1.AgentReviewWorkerCanceled ||
		result.Failure == nil || result.Failure.Code != "deadline_exceeded" {
		t.Fatalf("expired worker result = %+v", result)
	}
}

func bindMutatedReviewInput(
	t *testing.T,
	plan *contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	report *piReviewReport,
) {
	t.Helper()
	digest, err := reviewcore.DigestReviewInput(input)
	if err != nil {
		t.Fatal(err)
	}
	plan.TargetDigest = digest
	report.Target.Digest = digest
	report.Execution.Snapshot.Target.Digest = digest
}

func localBridgeRequest(
	fixture *shadowFixture,
	node string,
	script string,
	key string,
) LocalRunRequest {
	return LocalRunRequest{
		SourceRunID: fixture.plan.SourceRunID, IdempotencyKey: key,
		NodePath: node, WorkerScript: script,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
		Budget: contractsv1alpha1.AgentReviewBudget{
			MaxFiles: 100, MaxGroups: 32, MaxCandidates: 100, MaxModelCalls: 100,
			MaxToolCalls: 20, MaxOutputTokens: 4096, MaxGroupBytes: 1 << 20,
			MaxTargetBytes: 4 << 20, TimeoutMS: 60_000, MaxConcurrency: 4,
		},
		Environment: map[string]string{
			"LANG": "C", "PATH": "/usr/bin", "ANTHROPIC_API_KEY": "test-only",
			"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
		},
	}
}

func fakeWorkerPackage(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	skills := filepath.Join(root, "skills")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "package.json"):      `{"name":"fake-pi-worker"}`,
		filepath.Join(root, "package-lock.json"): `{"lockfileVersion":3}`,
		filepath.Join(dist, "worker.js"):         "import './protocol.js';\n",
		filepath.Join(dist, "protocol.js"):       "export const revision = 1;\n",
		filepath.Join(root, "node-bin"):          "fake-node-binary\n",
	}
	for _, skill := range defaultLocalSkills {
		revision := "builtin-v1"
		if skill == "concurrency-data" || skill == "correctness" {
			revision = "builtin-v2"
		}
		files[filepath.Join(skills, skill+".md")] =
			"# " + skill + "\n\nRevision: " + revision + "\n"
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "node-bin"), filepath.Join(dist, "worker.js")
}

func fakeSucceededWorkerResult(
	t *testing.T,
	request contractsv1alpha1.AgentReviewWorkerRequest,
	mode string,
	anchorLine uint32,
) []byte {
	t.Helper()
	report, completedAt := fakePiReport(t, request, mode, anchorLine)
	reportData, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest := digestBytesRaw(reportData)
	result := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:    request.WorkItemID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, CapabilitySHA256: request.Capability.SHA256,
		Status:       contractsv1alpha1.AgentReviewWorkerSucceeded,
		ReportSHA256: &reportDigest, Report: reportData, CompletedAt: completedAt,
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fakeFailedWorkerResult(
	t *testing.T,
	request contractsv1alpha1.AgentReviewWorkerRequest,
	message string,
) []byte {
	t.Helper()
	result := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:    request.WorkItemID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, CapabilitySHA256: request.Capability.SHA256,
		Status: contractsv1alpha1.AgentReviewWorkerFailed,
		Failure: &contractsv1alpha1.AgentReviewWorkerFailure{
			Code: "provider_error", Message: message, Retryable: true,
		},
		CompletedAt: request.Plan.CreatedAt.Add(10 * time.Second),
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fakePiReport(
	t *testing.T,
	request contractsv1alpha1.AgentReviewWorkerRequest,
	mode string,
	anchorLine uint32,
) (piReviewReport, time.Time) {
	t.Helper()
	inputData, err := base64.StdEncoding.DecodeString(request.ReviewInputBase64)
	if err != nil {
		t.Fatal(err)
	}
	input, err := reviewcore.DecodeReviewInput(inputData)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Files) != 1 || input.Files[0].Content == nil {
		t.Fatalf("unexpected bridge fixture input: %+v", input)
	}
	patch, status, changedLines, err := frozenPiFilePatch(input, input.Files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := request.Plan.CreatedAt
	generatedAt := createdAt.Add(time.Second)
	completedAt := createdAt.Add(10 * time.Second)
	patchDigest := "sha256:" + digestText(patch)
	targetDigest := "sha256:" + input.Files[0].SHA256
	canonicalPatchDigest := "sha256:" + digestText(input.CanonicalPatch)
	groupID := "group-001-fixture"
	group := piSnapshotGroup{
		ID: groupID, Key: "internal:.go", PatchDigest: digestText(patch),
		Files: []string{input.Files[0].Path},
	}
	skills := make([]piSnapshotSkill, 0, len(request.Plan.ReviewDimensions))
	for _, skill := range request.Plan.ReviewDimensions {
		skills = append(skills, piSnapshotSkill{
			ID: skill.ID, Revision: skill.Revision, Digest: "sha256:" + skill.SHA256,
			Bytes: 1,
		})
	}
	providerProfile, err := piProfileForProvider(request.Plan.Provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	report := piReviewReport{
		SchemaVersion: piReviewReportSchemaVersion, Status: "complete",
		Provider: request.Plan.Provider.ID, ProviderProfile: providerProfile,
		Model: request.Plan.Model.ID,
		Target: piTarget{
			Kind:       map[bool]string{true: "commit_diff", false: "files"}[input.TargetMode == reviewcore.TargetModeDiff],
			Repository: "memory://review-input/" + jsEncodeURIComponent(input.TargetID),
			Digest:     request.Plan.TargetDigest, CapturedAt: generatedAt,
			GeneratedAt: &generatedAt, CanonicalPatchDigest: &canonicalPatchDigest,
			Files: []piTargetFile{{
				Path: input.Files[0].Path, Status: status, Digest: input.Files[0].SHA256,
				ChangedLines: changedLines, TargetDigest: &targetDigest, PatchDigest: &patchDigest,
			}},
			Skipped: []piSkipped{},
		},
		Coverage: piCoverage{
			GroupsTotal: 1, GroupsReviewed: 1,
			ReviewTasksTotal: uint32(len(skills)), ReviewTasksSucceeded: uint32(len(skills)),
			VerificationEnabled: true, FilesIncluded: 1,
			Skipped: []piSkipped{}, ContextGaps: []string{}, Failures: []piFailure{},
		},
		Summary:                piSummary{},
		Findings:               []piCandidate{},
		Candidates:             []piCandidate{},
		RawCandidates:          []piRawCandidate{},
		NormalizationDecisions: []piNormalizationDecision{},
		Execution: piExecutionEnvelope{
			Authority: "diagnostic_only", ProvenanceClass: "worker_self_report",
			TaskEvidence: piTaskEvidenceCollection{
				SchemaVersion: "argus.pi-review.task_evidence.v0",
				Authority:     "diagnostic_only", ProvenanceClass: "worker_self_report",
				ContentPolicy: "exact_local_sensitive", Completeness: "partial",
				ReasonCodes: []string{"task_evidence_unavailable"}, Tasks: []piTaskExecutionEvidence{},
			},
			Snapshot: piExecutionSnapshot{
				SchemaVersion:  "argus.pi-review.execution_snapshot.v0",
				SnapshotDigest: "sha256:" + strings.Repeat("d", 64), CreatedAt: createdAt,
				Target:               piSnapshotTarget{Kind: map[bool]string{true: "commit_diff", false: "files"}[input.TargetMode == reviewcore.TargetModeDiff], Digest: request.Plan.TargetDigest},
				WorkflowRevision:     "argus-pi-review-workflow-v2",
				PromptBundleRevision: "argus-pi-review-prompts-v0", PromptBundleDigest: strings.Repeat("e", 64),
				Grouping: piSnapshotGrouping{ImplementationRevision: "directory-language-v0", Groups: []piSnapshotGroup{group}},
				Skills:   skills, Knowledge: []piSnapshotKnowledge{},
				Runtime:    piSnapshotRuntime{Implementation: "@argus/pi-review", Version: "0.1.0", Node: "v24", PiAgentCore: "0.84.1", PiAI: "0.84.1"},
				Provider:   piSnapshotProvider{Provider: request.Plan.Provider.ID, Profile: providerProfile, Protocol: "anthropic-messages", Model: request.Plan.Model.ID},
				ToolPolicy: piSnapshotToolPolicy{Mode: "read_only", AllowedTools: append([]string{}, request.Plan.ToolPolicy.AllowedTools...)},
				Budgets: piSnapshotBudgets{
					Concurrency: request.Plan.Budget.MaxConcurrency, MaxToolCallsPerTask: request.Plan.Budget.MaxToolCalls,
					MaxFiles: request.Plan.Budget.MaxFiles, MaxGroups: request.Plan.Budget.MaxGroups,
					MaxGroupBytes: request.Plan.Budget.MaxGroupBytes, MaxTargetBytes: request.Plan.Budget.MaxTargetBytes,
					MaxCandidates: request.Plan.Budget.MaxCandidates, MaxProviderTurns: request.Plan.Budget.MaxModelCalls,
					MaxOutputTokensPerTurn: request.Plan.Budget.MaxOutputTokens, TaskTimeoutMS: request.Plan.Budget.TimeoutMS,
				},
				VerificationPolicy: "independent_required", Replayability: piReplayability{Status: "non_replayable", Reasons: []string{"direct_provider_execution_not_platform_attested"}},
			},
			Tasks: []piTaskObservation{}, Usage: observedZeroPiUsage(),
		},
	}
	report.Execution.Tasks = append(report.Execution.Tasks, succeededPiTask(createdAt, groupID, "context", nil, nil, "submit_context", strings.Repeat("1", 64)))
	for _, skill := range request.Plan.ReviewDimensions {
		skillID := skill.ID
		report.Execution.Tasks = append(report.Execution.Tasks, succeededPiTask(createdAt, groupID, "review", &skillID, nil, "submit_candidates", strings.Repeat("2", 64)))
	}
	if mode == "clean" {
		return report, completedAt
	}

	claim := piCandidateClaim{
		Category: "correctness", Severity: "high", Title: "possible nil dereference",
		Description: "value can be nil", Impact: "request panic",
		Anchor: piSourceAnchor{Path: input.Files[0].Path, Side: "file", StartLine: anchorLine, EndLine: anchorLine},
		Evidence: []piEvidence{{
			Statement: "the dereference occurs on this source line",
			Anchor:    piSourceAnchor{Path: input.Files[0].Path, Side: "file", StartLine: anchorLine, EndLine: anchorLine},
			Excerpt:   lineAt(*input.Files[0].Content, anchorLine),
		}},
	}
	fingerprint, err := piClusterFingerprint(claim)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := "candidate-" + fingerprint[:16]
	skill := piSkillRef{ID: request.Plan.ReviewDimensions[0].ID, Revision: request.Plan.ReviewDimensions[0].Revision}
	raw := piRawCandidate{RawCandidateID: "raw-fixture-1", GroupID: groupID, Skill: skill, Ordinal: 0, Claim: claim}
	candidate := piCandidate{PiCandidateClaim: claim, ID: candidateID, Fingerprint: fingerprint, GroupID: groupID, Skill: skill}
	report.RawCandidates = []piRawCandidate{raw}
	report.NormalizationDecisions = []piNormalizationDecision{{
		RawCandidateID: raw.RawCandidateID, Action: "retained", ReasonCode: "normalized_candidate_retained", NormalizedCandidateID: &candidateID,
	}}
	if mode == "invalid" {
		report.Status = "partial"
		report.NormalizationDecisions = []piNormalizationDecision{{
			RawCandidateID: raw.RawCandidateID,
			Action:         "rejected_invalid",
			ReasonCode:     "invalid_candidate",
		}}
		report.Coverage.Failures = []piFailure{{
			GroupID: groupID, Phase: "review", SkillID: &skill.ID,
			Error: "invalid candidate discarded: anchor outside frozen target",
		}}
		return report, completedAt
	}
	report.Summary.Candidates = 1
	report.Coverage.VerificationTasksTotal = 1
	if mode == "partial" {
		report.Status = "partial"
		report.Candidates = []piCandidate{candidate}
		errorCode := "provider_error"
		prompt := "sha256:" + strings.Repeat("3", 64)
		report.Execution.Tasks = append(report.Execution.Tasks, piTaskObservation{
			TaskID: "agent-task-verification-failed", TaskKind: "verification", GroupID: groupID,
			CandidateID: &candidateID, PromptDigest: prompt,
			ProviderTurnsStarted: 1, ProviderTurnsCompleted: 0,
			ToolNames: []string{}, ToolUsage: []piToolUsage{}, Usage: unavailablePiUsage(),
			StartedAt: createdAt.Add(2 * time.Second), FinishedAt: createdAt.Add(3 * time.Second),
			DurationMS: 1000, TerminalStatus: "failed", ErrorCode: &errorCode,
		})
		report.Coverage.Failures = []piFailure{{GroupID: groupID, Phase: "verification", CandidateID: &candidateID, Error: "provider failed"}}
		return report, completedAt
	}
	verification := piVerification{
		CandidateID: candidateID, Verdict: "confirmed", ReasonCode: "path_reachable",
		Explanation: "the frozen line contains the dereference", Evidence: claim.Evidence,
	}
	candidate.Verification = &verification
	report.Candidates = []piCandidate{candidate}
	report.Findings = []piCandidate{candidate}
	report.Summary.Confirmed = 1
	report.Coverage.VerificationTasksSucceeded = 1
	verificationData, err := json.Marshal(verification)
	if err != nil {
		t.Fatal(err)
	}
	report.Execution.Tasks = append(report.Execution.Tasks, succeededPiTask(
		createdAt, groupID, "verification", nil, &candidateID, "submit_verdict", digestBytesRaw(verificationData),
	))
	return report, completedAt
}

func succeededPiTask(
	createdAt time.Time,
	groupID string,
	kind string,
	skillID *string,
	candidateID *string,
	tool string,
	outputDigest string,
) piTaskObservation {
	identity := kind
	if skillID != nil {
		identity += "-" + *skillID
	}
	return piTaskObservation{
		TaskID: "agent-task-" + identity, TaskKind: kind, GroupID: groupID,
		SkillID: skillID, CandidateID: candidateID,
		PromptDigest:         "sha256:" + strings.Repeat("a", 64),
		ProviderTurnsStarted: 1, ProviderTurnsCompleted: 1,
		ToolCalls: 1, ToolNames: []string{tool},
		ToolUsage: []piToolUsage{{ToolID: tool, InvocationCount: 1}},
		Usage:     observedZeroPiUsage(), StartedAt: createdAt.Add(2 * time.Second),
		FinishedAt: createdAt.Add(3 * time.Second), DurationMS: 1000,
		TerminalStatus: "succeeded", OutputDigest: &[]string{"sha256:" + outputDigest}[0],
	}
}

func observedZeroPiUsage() piUsage {
	zero := uint64(0)
	return piUsage{
		Completeness: "provider_reported", InputTokens: &zero, OutputTokens: &zero,
		CacheReadTokens: &zero, CacheWriteTokens: &zero, TotalTokens: &zero,
	}
}

func unavailablePiUsage() piUsage {
	reason := "provider_turn_incomplete"
	return piUsage{Completeness: "unavailable", UnavailableReasonCode: &reason}
}

func lineAt(content string, line uint32) string {
	lines := strings.Split(content, "\n")
	if line == 0 || int(line) > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[line-1])
}

func clonePiReport(t *testing.T, report piReviewReport) piReviewReport {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := decodePiReviewReport(data)
	if err != nil {
		t.Fatal(err)
	}
	return cloned
}
