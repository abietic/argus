package piexecution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/agentshadowworker"
	"argus.local/argus/internal/application"
	"argus.local/argus/internal/execution"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestPlatformEnsureIsExactIdempotentAndVerifiesDurableCallback(t *testing.T) {
	fixture := newPlatformFixture(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &platformRunnerStub{mode: "failed"}
	platform, err := NewPlatform(store, fixture.config(runner))
	if err != nil {
		t.Fatal(err)
	}

	const callers = 24
	handles := make(chan execution.Handle, callers)
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			handle, ensureErr := platform.Ensure(context.Background(), fixture.request)
			handles <- handle
			errs <- ensureErr
		}()
	}
	wait.Wait()
	close(handles)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
	}
	var handle execution.Handle
	for candidate := range handles {
		if handle.ProviderHandle == "" {
			handle = candidate
		} else if candidate != handle {
			t.Fatalf("Ensure() handles differ: %+v / %+v", handle, candidate)
		}
	}
	result, proof, err := platform.AwaitResult(context.Background(), handle)
	if err != nil {
		t.Fatalf("AwaitResult() error = %v", err)
	}
	if result.Status != contractsv1alpha1.StageExecutionFailed || result.Failure == nil ||
		result.Failure.Code != "pi_worker_failed" || result.Failure.Retryable ||
		len(proof) == 0 || runner.Calls() != 1 {
		t.Fatalf("result=%+v proof=%q runner calls=%d", result, proof, runner.Calls())
	}
	lookedUp, found, err := platform.Lookup(context.Background(), fixture.request)
	if err != nil || !found || lookedUp != handle {
		t.Fatalf("Lookup() = (%+v, %v, %v)", lookedUp, found, err)
	}

	canonical, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	binding := runmodel.AgentStageExecutionBinding{
		Subject: fixture.ledgerSubject, ProviderHandle: handle.ProviderHandle,
		ExecutionID: handle.ExecutionID, CreateIdempotencyKey: handle.IdempotencyKey,
		RequestSemanticSHA256: handle.RequestSHA256,
		CapabilitySHA256:      handle.CapabilitySHA256,
	}
	verifier, err := platform.VerifyAgentStageResultCallback(
		context.Background(),
		fixture.subject,
		binding,
		fixture.request.Capability.Trust,
		canonical,
		proof,
	)
	if err != nil || verifier.ID != callbackVerifierID {
		t.Fatalf("VerifyAgentStageResultCallback() = (%+v, %v)", verifier, err)
	}
	if _, err := platform.VerifyAgentStageResultCallback(
		context.Background(), fixture.subject, binding, fixture.request.Capability.Trust,
		canonical, []byte("changed-proof"),
	); err == nil {
		t.Fatal("callback verifier accepted changed proof")
	}
	changedTrust := fixture.request.Capability.Trust
	changedTrust.CallbackVerifier.ID = "other-callback-verifier"
	if _, err := platform.VerifyAgentStageResultCallback(
		context.Background(), fixture.subject, binding, changedTrust, canonical, proof,
	); err == nil {
		t.Fatal("callback verifier accepted trust outside the frozen local Pi root")
	}
}

func TestPlatformRejectsRuntimeFileDriftBeforeDispatch(t *testing.T) {
	fixture := newPlatformFixture(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &platformRunnerStub{mode: "failed"}
	config := fixture.config(runner)
	config.RuntimeFiles = runtimeFileVerifierStub{err: errors.New("runtime bytes changed")}
	platform, err := NewPlatform(store, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Ensure(t.Context(), fixture.request); err == nil ||
		!strings.Contains(err.Error(), "runtime bytes changed") {
		t.Fatalf("Ensure() runtime drift error = %v", err)
	}
	if runner.Calls() != 0 {
		t.Fatalf("runtime drift invoked worker %d times", runner.Calls())
	}
}

func TestPlatformRejectsMissingOrSubstitutedPromptArtifactBeforeDispatch(t *testing.T) {
	fixture := newPlatformFixture(t)
	delete(fixture.artifacts.values, fixture.plan.Prompt.Artifact.Ref.URI)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &platformRunnerStub{mode: "failed"}
	platform, err := NewPlatform(store, fixture.config(runner))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Ensure(t.Context(), fixture.request); err == nil ||
		!strings.Contains(err.Error(), "resolve formal Pi prompt bundle") {
		t.Fatalf("Ensure() prompt artifact error = %v", err)
	}
	if runner.Calls() != 0 {
		t.Fatalf("prompt artifact rejection invoked worker %d times", runner.Calls())
	}
}

func TestPlatformClosesDispatchWhenRuntimeFilesDriftBeforeProcessStart(t *testing.T) {
	fixture := newPlatformFixture(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &platformRunnerStub{mode: "failed"}
	config := fixture.config(runner)
	config.RuntimeFiles = &runtimeFileVerifierSequence{
		errors: []error{nil, errors.New("runtime changed after dispatch")},
	}
	platform, err := NewPlatform(store, config)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := platform.Ensure(t.Context(), fixture.request)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	result, _, err := platform.AwaitResult(t.Context(), handle)
	if err != nil {
		t.Fatalf("AwaitResult() error = %v", err)
	}
	if result.Status != contractsv1alpha1.StageExecutionFailed || result.Failure == nil ||
		result.Failure.Code != "pi_runtime_files_changed" || runner.Calls() != 0 {
		t.Fatalf("runtime drift result=%+v runner_calls=%d", result, runner.Calls())
	}
}

func TestPlatformRestartNeverRerunsStartedUnknownAndCancelWins(t *testing.T) {
	fixture := newPlatformFixture(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstRunner := &platformRunnerStub{mode: "block", started: make(chan struct{})}
	first, err := NewPlatform(store, fixture.config(firstRunner))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Ensure(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	<-firstRunner.started

	secondRunner := &platformRunnerStub{mode: "failed"}
	second, err := NewPlatform(store, fixture.config(secondRunner))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := second.Ensure(context.Background(), fixture.request)
	if err != nil || recovered != handle || secondRunner.Calls() != 0 {
		t.Fatalf("restart Ensure() = (%+v, %v), starts=%d", recovered, err, secondRunner.Calls())
	}
	if _, _, err := second.AwaitResult(
		context.Background(), handle,
	); !errors.Is(err, ErrCompletionPending) {
		t.Fatalf("restart AwaitResult() error = %v", err)
	}

	cancel := execution.CancelRequest{
		Authority: execution.TerminalCancelAuthority{
			TenantID: fixture.subject.TenantID, WorkspaceID: fixture.subject.WorkspaceID,
			ReviewRunID: fixture.request.ReviewRunID,
			StageID:     fixture.request.Stage.ID, StageRevision: fixture.request.Stage.Revision,
			StageSHA256: fixture.request.Stage.SHA256,
			IntentID:    "intent", IntentSHA256: strings.Repeat("8", 64),
			TerminalGateID: "terminal", TerminalGateSHA256: strings.Repeat("9", 64),
			CancelIdempotencyKey: "cancel-formal-execution",
		},
		ProviderHandle: handle.ProviderHandle, ExecutionID: handle.ExecutionID,
		Attempt: handle.Attempt, Generation: handle.Generation,
		FencingToken:         handle.FencingToken,
		CreateIdempotencyKey: handle.IdempotencyKey,
		CancelIdempotencyKey: "cancel-formal-execution",
		RequestSHA256:        handle.RequestSHA256, CapabilitySHA256: handle.CapabilitySHA256,
	}
	if err := first.Cancel(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	if err := first.Cancel(context.Background(), cancel); err != nil {
		t.Fatalf("Cancel(exact retry) error = %v", err)
	}
	result, _, err := first.AwaitResult(context.Background(), handle)
	if err != nil || result.Status != contractsv1alpha1.StageExecutionCanceled {
		t.Fatalf("canceled AwaitResult() = (%+v, %v)", result, err)
	}
}

func TestPlatformReusesPriorGenerationPiGroupCheckpointAndFencesLateWriter(t *testing.T) {
	fixture := newPlatformFixture(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(fmt.Sprintf(
		`{"schemaVersion":"argus.pi-review.group_checkpoint.v1","checkpointRevision":0,"checkpointScopeSha256":%q,"group":{"id":"group-001-checkpoint"}}`,
		checkpointScope(fixture.request),
	))
	checkpoint := contractsv1alpha1.AgentReviewWorkerGroupCheckpoint{
		GroupID: "group-001-checkpoint", CheckpointScopeSHA256: checkpointScope(fixture.request),
		ContentSHA256: shaHex(content), SizeBytes: uint64(len(content)),
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
	firstRunner := &platformRunnerStub{mode: "failed", checkpoint: &checkpoint}
	first, err := NewPlatform(store, fixture.config(firstRunner))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Ensure(t.Context(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.AwaitResult(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	firstRequests := firstRunner.Requests()
	if len(firstRequests) != 1 {
		t.Fatalf("generation 1 worker requests = %+v", firstRequests)
	}

	secondRequest := fixture.request
	secondRequest.ExecutionID = "formal-execution-generation-2"
	secondRequest.Generation = 2
	secondRequest.FencingToken = 2
	secondRequest.LeaseID = "run-1-workload-g2"
	secondRequest.IdempotencyKey = "create-formal-execution-generation-2"
	secondRequest.Deadline = fixture.request.Deadline.Add(time.Minute)
	secondRequest.RequestSHA256 = ""
	secondRequest, err = contractsv1alpha1.SealStageExecutionRequest(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondRunner := &platformRunnerStub{mode: "failed"}
	second, err := NewPlatform(store, fixture.config(secondRunner))
	if err != nil {
		t.Fatal(err)
	}
	secondHandle, err := second.Ensure(t.Context(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := second.AwaitResult(t.Context(), secondHandle); err != nil {
		t.Fatal(err)
	}
	requests := secondRunner.Requests()
	if len(requests) != 1 || len(requests[0].GroupCheckpoints) != 1 ||
		requests[0].GroupCheckpoints[0] != checkpoint {
		t.Fatalf("generation 2 worker checkpoints = %+v", requests)
	}
	freshGenerationStart := secondRequest.Deadline.Add(
		-time.Duration(fixture.plan.Budget.TimeoutMS) * time.Millisecond,
	)
	if !requests[0].Plan.CreatedAt.Before(freshGenerationStart) ||
		requests[0].Plan.CreatedAt.Before(firstRequests[0].Plan.CreatedAt) {
		t.Fatalf(
			"resumed worker logical window = %s, first=%s fresh_generation=%s",
			requests[0].Plan.CreatedAt, firstRequests[0].Plan.CreatedAt, freshGenerationStart,
		)
	}
	late := checkpoint
	late.GroupID = "group-002-late"
	lateContent := []byte(fmt.Sprintf(
		`{"schemaVersion":"argus.pi-review.group_checkpoint.v1","checkpointRevision":0,"checkpointScopeSha256":%q,"group":{"id":"group-002-late"}}`,
		checkpointScope(fixture.request),
	))
	late.ContentSHA256 = shaHex(lateContent)
	late.SizeBytes = uint64(len(lateContent))
	late.ContentBase64 = base64.StdEncoding.EncodeToString(lateContent)
	if err := first.groupCheckpoints.Record(t.Context(), fixture.request, late); !errors.Is(err, errPiGroupCheckpointFenced) {
		t.Fatalf("late generation 1 checkpoint error = %v", err)
	}
}

func TestPiGroupCheckpointRepositoryKeepsLatestMonotonicRevision(t *testing.T) {
	fixture := newPlatformFixture(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := newPiGroupCheckpointRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.BindGeneration(t.Context(), fixture.request); err != nil {
		t.Fatal(err)
	}
	checkpoint := func(revision uint64, marker string) contractsv1alpha1.AgentReviewWorkerGroupCheckpoint {
		content := []byte(fmt.Sprintf(
			`{"schemaVersion":"argus.pi-review.group_checkpoint.v1","checkpointRevision":%d,"checkpointScopeSha256":%q,"group":{"id":"group-001"},"marker":%q}`,
			revision, checkpointScope(fixture.request), marker,
		))
		return contractsv1alpha1.AgentReviewWorkerGroupCheckpoint{
			GroupID: "group-001", CheckpointRevision: revision,
			CheckpointScopeSHA256: checkpointScope(fixture.request),
			ContentSHA256:         shaHex(content),
			SizeBytes:             uint64(len(content)),
			ContentBase64:         base64.StdEncoding.EncodeToString(content),
		}
	}
	revision0 := checkpoint(0, "context-review")
	revision1 := checkpoint(1, "one-verifier")
	revision2 := checkpoint(2, "two-verifiers")
	if err := repository.Record(t.Context(), fixture.request, revision0); err != nil {
		t.Fatal(err)
	}
	// Revision 2 may reach the host before revision 1 when concurrent verifier
	// callbacks finish together. The cumulative later revision must win.
	if err := repository.Record(t.Context(), fixture.request, revision2); err != nil {
		t.Fatal(err)
	}
	if err := repository.Record(t.Context(), fixture.request, revision1); err != nil {
		t.Fatalf("late lower revision should be a safe no-op: %v", err)
	}
	if err := repository.Record(t.Context(), fixture.request, revision2); err != nil {
		t.Fatalf("exact latest retry should be idempotent: %v", err)
	}
	conflict := checkpoint(2, "conflicting-two-verifiers")
	if err := repository.Record(t.Context(), fixture.request, conflict); err == nil ||
		!strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("same-revision conflict error = %v", err)
	}

	next := fixture.request
	next.Generation = 2
	next.FencingToken = 2
	next.ExecutionID = "formal-execution-checkpoint-revision-2"
	next.LeaseID = "run-1-checkpoint-revision-g2"
	reusable, err := repository.ReusableBefore(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(reusable) != 1 || reusable[0] != revision2 {
		t.Fatalf("reusable latest checkpoint = %+v", reusable)
	}
}

func TestCapabilityResolverFailsClosedWhenWorstCaseCostIsUnenforceable(t *testing.T) {
	fixture := newPlatformFixture(t)
	resolver, err := NewCapabilityResolver(fixture.manifest, fixture.pricing)
	if err != nil {
		t.Fatal(err)
	}
	plan := fixture.plan
	plan.Budget.MaxCostMicros = 1
	plan, err = contractsv1alpha1.SealAgentStagePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveAgentExecutorCapability(
		context.Background(), fixture.subject, plan,
	); err == nil || !strings.Contains(err.Error(), "worst-case cost") {
		t.Fatalf("ResolveAgentExecutorCapability() error = %v", err)
	}
}

type platformRunnerStub struct {
	mu         sync.Mutex
	calls      int
	mode       string
	started    chan struct{}
	checkpoint *contractsv1alpha1.AgentReviewWorkerGroupCheckpoint
	requests   []contractsv1alpha1.AgentReviewWorkerRequest
}

func (runner *platformRunnerStub) Calls() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.calls
}

func (runner *platformRunnerStub) Requests() []contractsv1alpha1.AgentReviewWorkerRequest {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]contractsv1alpha1.AgentReviewWorkerRequest{}, runner.requests...)
}

func (runner *platformRunnerStub) Run(
	ctx context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	runner.mu.Lock()
	runner.calls++
	runner.mu.Unlock()
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(request.Input)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	runner.mu.Lock()
	runner.requests = append(runner.requests, workerRequest)
	checkpoint := runner.checkpoint
	runner.mu.Unlock()
	if checkpoint != nil && request.ProgressLine != nil {
		progress, marshalErr := json.Marshal(map[string]any{
			"schema_version": "argus.agent_review_worker_progress.v1alpha1",
			"work_item_id":   workerRequest.WorkItemID,
			"phase":          "checkpoint", "message": "checkpoint",
			"observed_at": time.Now().UTC(), "group_id": checkpoint.GroupID,
			"checkpoint": checkpoint,
		})
		if marshalErr != nil {
			return agentshadowworker.Result{}, marshalErr
		}
		if progressErr := request.ProgressLine(progress); progressErr != nil {
			return agentshadowworker.Result{}, progressErr
		}
	}
	if runner.mode == "block" {
		if runner.started != nil {
			close(runner.started)
		}
		<-ctx.Done()
		return agentshadowworker.Result{}, ctx.Err()
	}
	workerResult := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:    workerRequest.WorkItemID, Attempt: workerRequest.Attempt,
		Generation: workerRequest.Generation, FencingToken: workerRequest.FencingToken,
		IdempotencyKey:   workerRequest.IdempotencyKey,
		CapabilitySHA256: workerRequest.Capability.SHA256,
		Status:           contractsv1alpha1.AgentReviewWorkerFailed,
		Failure: &contractsv1alpha1.AgentReviewWorkerFailure{
			Code: "provider_failed", Message: "provider failed", Retryable: true,
		},
		CompletedAt: workerRequest.Plan.CreatedAt.Add(time.Second),
	}
	data, err := json.Marshal(workerResult)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	return agentshadowworker.Result{
		Stdout: data, StartedAt: workerRequest.Plan.CreatedAt,
		FinishedAt: workerResult.CompletedAt,
	}, nil
}

type platformArtifactStub struct {
	values map[string][]byte
}

func (artifacts *platformArtifactStub) ReadArtifact(
	ref runmodel.ArtifactRef,
) ([]byte, error) {
	data, found := artifacts.values[ref.URI]
	if !found || shaHex(data) != ref.SHA256 || int64(len(data)) != ref.SizeBytes {
		return nil, errors.New("local artifact not found or mismatched")
	}
	return append([]byte{}, data...), nil
}

func (artifacts *platformArtifactStub) Resolve(
	_ context.Context,
	binding contractsv1alpha1.ArtifactBinding,
) ([]byte, error) {
	data, found := artifacts.values[binding.Ref.URI]
	if !found || shaHex(data) != binding.Ref.SHA256 || int64(len(data)) != binding.Ref.SizeBytes {
		return nil, errors.New("artifact not found or mismatched")
	}
	return append([]byte{}, data...), nil
}

func (artifacts *platformArtifactStub) Publish(
	_ context.Context,
	contract string,
	data []byte,
	_ string,
	_ time.Time,
) (contractsv1alpha1.ArtifactBinding, error) {
	digest := shaHex(data)
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI:    "artifact://governed/output/" + digest,
			SHA256: digest, SizeBytes: int64(len(data)),
		},
		Contract: contract,
	}, nil
}

type platformFixture struct {
	subject       application.AgentPlanningSubject
	ledgerSubject runmodel.AgentPlanningSubject
	manifest      RuntimeManifest
	pricing       PricingCeiling
	plan          contractsv1alpha1.AgentStagePlan
	request       contractsv1alpha1.StageExecutionRequest
	artifacts     *platformArtifactStub
}

type runtimeFileVerifierStub struct {
	err error
}

func (verifier runtimeFileVerifierStub) Verify(
	context.Context,
	string,
	string,
	string,
) error {
	return verifier.err
}

type runtimeFileVerifierSequence struct {
	mu     sync.Mutex
	errors []error
}

func (verifier *runtimeFileVerifierSequence) Verify(
	context.Context,
	string,
	string,
	string,
) error {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if len(verifier.errors) == 0 {
		return nil
	}
	err := verifier.errors[0]
	verifier.errors = verifier.errors[1:]
	return err
}

func (fixture platformFixture) config(runner agentshadowworker.Runner) Config {
	return Config{
		Subject: fixture.subject, NodePath: "/node", WorkerScript: "/worker.js",
		Environment: map[string]string{}, Manifest: fixture.manifest,
		Pricing: fixture.pricing, Runner: runner, Artifacts: fixture.artifacts,
		LocalArtifacts: fixture.artifacts,
		RuntimeFiles:   runtimeFileVerifierStub{},
	}
}

func newPlatformFixture(t *testing.T) platformFixture {
	t.Helper()
	subject := application.AgentPlanningSubject{
		TenantID: "tenant-1", OrganizationID: "organization-1",
		WorkspaceID: "workspace-1", RepositoryID: "repository-1",
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	content := "package fixture\n\nfunc value() int { return 1 }\n"
	fileDigest := shaHex([]byte(content))
	input := reviewcore.ReviewInput{
		SchemaVersion: reviewcore.ReviewInputSchemaVersion,
		TargetID:      "formal-target", TargetMode: reviewcore.TargetModeScope,
		CanonicalPatch: "",
		Regions: []reviewcore.ReviewRegion{{
			Path: "fixture.go", StartLine: 1, EndLine: 3, SHA256: fileDigest,
		}},
		Files: []reviewcore.FileManifestEntry{{
			Path: "fixture.go", SHA256: fileDigest, SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []reviewcore.ContextBinding{},
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	targetDigest, err := reviewcore.DigestReviewInput(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := func(value string) string { return shaHex([]byte(value)) }
	implementationDigest := digest("pi-worker")
	promptBytes, err := contractsv1alpha1.MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		t.Fatal(err)
	}
	promptDigest := shaHex(promptBytes)
	modelProfile, err := contractsv1alpha1.NewModelProfile(
		"deepseek-anthropic",
		"deepseek-v4-pro[1m]",
	)
	if err != nil {
		t.Fatal(err)
	}
	modelProfileBytes, err := contractsv1alpha1.MarshalModelProfile(modelProfile)
	if err != nil {
		t.Fatal(err)
	}
	modelProfileDigest := shaHex(modelProfileBytes)
	reviewSkillBytes := []byte("# Correctness\n\nFind concrete correctness defects.\n")
	reviewSkill := contractsv1alpha1.VersionedRef{
		ID: "correctness", Revision: "builtin-v1", SHA256: shaHex(reviewSkillBytes),
	}
	rulePack, err := reviewconfig.SealRulePack("formal-rules", "1", []reviewconfig.RuleDefinition{{
		ID: "correctness", Revision: "1", Kind: "agent",
		Detector: reviewconfig.VersionedRef{
			ID: "pi-review", Revision: "1", SHA256: digest("pi-review-rule"),
		},
		Languages: []string{"go"}, PathPrefixes: []string{},
		EvidenceKinds: []string{"file_content", "target_line"},
		Severity:      "high", Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	rulePackData, err := json.Marshal(rulePack)
	if err != nil {
		t.Fatal(err)
	}
	manifest := RuntimeManifest{
		Runtime: contractsv1alpha1.VersionedRef{
			ID: "node-runtime", Revision: "binary", SHA256: digest("node"),
		},
		BuildIdentity: "pi-test-build",
		Agent: contractsv1alpha1.VersionedRef{
			ID: "pi-agent", Revision: "worker-v0", SHA256: implementationDigest,
		},
		Provider: contractsv1alpha1.VersionedRef{
			ID: "deepseek-anthropic", Revision: "v1", SHA256: digest("provider"),
		},
		Model: contractsv1alpha1.VersionedRef{
			ID:       "deepseek-anthropic-model-" + modelProfileDigest[:16],
			Revision: "provider", SHA256: modelProfileDigest,
		},
		Prompt: contractsv1alpha1.VersionedRef{
			ID: "pi-review-prompts", Revision: "v0", SHA256: promptDigest,
		},
		ReviewSkills: []contractsv1alpha1.VersionedRef{reviewSkill},
		SHA256:       digest("runtime-manifest"),
	}
	component := func(
		ref contractsv1alpha1.VersionedRef,
		contract string,
	) contractsv1alpha1.AgentStageComponentBinding {
		return contractsv1alpha1.AgentStageComponentBinding{
			Ref: ref,
			Artifact: contractsv1alpha1.ArtifactBinding{
				Ref: contractsv1alpha1.ContentRef{
					URI:    "artifact://governed/component/" + ref.ID,
					SHA256: ref.SHA256, SizeBytes: 1,
				},
				Contract: contract,
			},
		}
	}
	skill := func(
		id string,
		phase contractsv1alpha1.AgentStageSkillPhase,
		ref contractsv1alpha1.VersionedRef,
	) contractsv1alpha1.AgentStageSkillBinding {
		binding := component(ref, contractsv1alpha1.AgentStagePlanSkillContract)
		return contractsv1alpha1.AgentStageSkillBinding{
			ID: id, Phase: phase, Ref: binding.Ref, Artifact: binding.Artifact,
		}
	}
	artifact := func(name, checksum, contract string, size int64) contractsv1alpha1.ArtifactBinding {
		return contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:    "artifact://governed/input/" + name,
				SHA256: checksum, SizeBytes: size,
			},
			Contract: contract,
		}
	}
	plan := contractsv1alpha1.AgentStagePlan{
		ReviewRunID: "run-1",
		Stage: contractsv1alpha1.VersionedRef{
			ID: "agent_hypothesize", Revision: "1", SHA256: digest("stage"),
		},
		TargetDigest: targetDigest, BuildIdentity: manifest.BuildIdentity,
		ExecutionSnapshot:          artifact("snapshot", digest("snapshot"), contractsv1alpha1.AgentStagePlanExecutionSnapshotContract, 1),
		ConfigBundle:               artifact("config", digest("config"), contractsv1alpha1.AgentStagePlanConfigBundleContract, 1),
		ConfigResolutionReceiptRef: artifact("receipt", digest("receipt"), contractsv1alpha1.AgentStagePlanConfigResolutionReceiptContract, 1),
		Workflow:                   artifact("workflow", digest("workflow"), contractsv1alpha1.AgentStagePlanWorkflowContract, 1),
		ReviewSpec:                 artifact("spec", digest("spec"), contractsv1alpha1.AgentStagePlanReviewSpecContract, 1),
		ReviewInput:                artifact("review-input", targetDigest, contractsv1alpha1.AgentStagePlanReviewInputContract, int64(len(inputJSON))),
		AgentReviewPolicy: contractsv1alpha1.VersionedRef{
			ID: "formal-review", Revision: "1", SHA256: digest("policy"),
		},
		RulePack: contractsv1alpha1.VersionedRef{
			ID: rulePack.ID, Revision: rulePack.Revision, SHA256: rulePack.SHA256,
		},
		Normalization: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationCurrentRevision,
			SHA256:   manifest.Agent.SHA256,
		},
		RulePackBase64: base64.StdEncoding.EncodeToString(rulePackData),
		Agent:          component(manifest.Agent, contractsv1alpha1.AgentStagePlanAgentContract),
		Provider:       component(manifest.Provider, contractsv1alpha1.AgentStagePlanProviderContract),
		Model:          component(manifest.Model, contractsv1alpha1.AgentStagePlanModelContract),
		Runtime:        component(manifest.Runtime, contractsv1alpha1.AgentStagePlanRuntimeContract),
		Prompt:         component(manifest.Prompt, contractsv1alpha1.AgentStagePlanPromptContract),
		Skills: []contractsv1alpha1.AgentStageSkillBinding{
			skill("grouping", contractsv1alpha1.AgentStageSkillPhaseGrouping, contractsv1alpha1.VersionedRef{ID: "directory-language", Revision: "v0", SHA256: implementationDigest}),
			skill("context", contractsv1alpha1.AgentStageSkillPhaseContext, contractsv1alpha1.VersionedRef{ID: "code-context", Revision: "v0", SHA256: implementationDigest}),
			skill("review", contractsv1alpha1.AgentStageSkillPhaseReview, reviewSkill),
			skill("verification", contractsv1alpha1.AgentStageSkillPhaseVerification, contractsv1alpha1.VersionedRef{ID: "independent-verifier", Revision: "v0", SHA256: implementationDigest}),
		},
		Knowledge:        []contractsv1alpha1.AgentStageKnowledgeBinding{},
		ContextProviders: []contractsv1alpha1.AgentStageContextProviderBinding{},
		ModelAuthority: contractsv1alpha1.AgentStageModelAuthority{
			ModelEgress: contractsv1alpha1.AgentStageModelEgressProviderBrokerOnly,
			APIProtocol: component(
				contractsv1alpha1.VersionedRef{ID: "anthropic-messages", Revision: "2023-06-01", SHA256: digest("protocol")},
				contractsv1alpha1.AgentStagePlanAPIProtocolContract,
			),
		},
		ToolAuthority: contractsv1alpha1.AgentStageToolAuthority{
			Tools:       []string{"list_files", "read_file", "search_code"},
			ToolNetwork: "deny", WorkspaceReads: "frozen_input_only",
			WorkspaceWrites: "deny", RemoteWrites: "deny",
		},
		Budget: contractsv1alpha1.AgentStageBudget{
			MaxFiles: 10, MaxGroups: 4, MaxHypotheses: 10, MaxModelCalls: 20,
			MaxToolCalls: 10, MaxTargetBytes: 1 << 20, MaxGroupBytes: 1 << 18,
			MaxOutputBytes: 1 << 20, MaxOutputTokens: 4096,
			MaxCostMicros: 1_000_000, TimeoutMS: 60_000, MaxConcurrency: 2,
		},
		OutputContract: contractsv1alpha1.AgentStagePlanOutputContract,
		Disposition:    contractsv1alpha1.AgentStageDispositionHypothesisOnly,
		SideEffects:    contractsv1alpha1.AgentStageSideEffectsDeny,
	}
	plan.Prompt.Artifact.Ref.SizeBytes = int64(len(promptBytes))
	plan.Model.Artifact.Ref.SizeBytes = int64(len(modelProfileBytes))
	plan.Skills[2].Artifact.Ref.SizeBytes = int64(len(reviewSkillBytes))
	plan, err = contractsv1alpha1.SealAgentStagePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	pricing := PricingCeiling{
		InputMicrosPerMillionTokens: 1, OutputMicrosPerMillionTokens: 1,
		MaximumBytesPerInputToken: 1,
	}
	resolver, err := NewCapabilityResolver(manifest, pricing)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := resolver.ResolveAgentExecutorCapability(context.Background(), subject, plan)
	if err != nil {
		t.Fatal(err)
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	request, err := contractsv1alpha1.SealStageExecutionRequest(
		contractsv1alpha1.StageExecutionRequest{
			SchemaVersion: contractsv1alpha1.StageExecutionRequestSchemaVersion,
			ExecutionID:   "formal-execution", ReviewRunID: plan.ReviewRunID,
			TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID,
			WorkloadID: "run-1-workload", LeaseID: "run-1-workload-g1",
			LeaseWorkerID: "worker-1", Stage: plan.Stage,
			Attempt: 1, Generation: 1, FencingToken: 1,
			IdempotencyKey:    "create-formal-execution",
			Plan:              artifact("plan", shaHex(planJSON), contractsv1alpha1.AgentStagePlanSchemaVersion, int64(len(planJSON))),
			ExecutionSnapshot: plan.ExecutionSnapshot, ReviewInput: plan.ReviewInput,
			Upstream:       []contractsv1alpha1.ArtifactBinding{},
			OutputContract: plan.OutputContract, Capability: capability,
			Deadline: time.Now().UTC().Add(time.Minute), SideEffects: plan.SideEffects,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return platformFixture{
		subject: subject, ledgerSubject: ledgerSubject, manifest: manifest,
		pricing: pricing, plan: plan, request: request,
		artifacts: &platformArtifactStub{values: map[string][]byte{
			request.Plan.Ref.URI:            planJSON,
			request.ReviewInput.Ref.URI:     inputJSON,
			plan.Model.Artifact.Ref.URI:     modelProfileBytes,
			plan.Prompt.Artifact.Ref.URI:    promptBytes,
			plan.Skills[2].Artifact.Ref.URI: reviewSkillBytes,
		}},
	}
}
