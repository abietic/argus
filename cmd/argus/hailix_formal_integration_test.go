package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentadapter"
	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/hailixexecution"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestFormalReviewRecoversDurableDispatchThroughHailixHTTP(t *testing.T) {
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
	writeCLITargetFile(t, repositoryPath, "go.mod", "module example.com/hailix-formal\n\ngo 1.26\n")
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n// TODO review me\n")
	revision := commitCLITarget(t, repositoryPath, "Hailix formal transport target")
	storePath := t.TempDir()
	configState := t.TempDir()
	var reviewOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"review", "--repo", repositoryPath, "--mode", "selection",
		"--revision", revision, "--path", "review.go", "--start-line", "2", "--end-line", "2",
		"--store", storePath, "--json",
	}, &reviewOutput); err != nil {
		t.Fatal(err)
	}
	var reviewed runOutput
	if err := json.Unmarshal(reviewOutput.Bytes(), &reviewed); err != nil {
		t.Fatal(err)
	}
	bootstrapArguments := []string{
		"agent-review", "formal", "bootstrap",
		"--store", storePath, "--config-state-dir", configState,
		"--source-run", reviewed.Run.RunID, "--idempotency-key", "hailix-http-bootstrap",
		"--at", "2026-08-26T01:02:03Z", "--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat", "--json",
	}
	if err := runWithIO(t.Context(), bootstrapArguments, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	runArguments := []string{
		"--store", storePath, "--config-state-dir", configState,
		"--source-run", reviewed.Run.RunID, "--idempotency-key", "hailix-http-formal-run",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--timeout-ms", "120000", "--json",
	}

	stateStore, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(stateStore)
	if err != nil {
		t.Fatal(err)
	}
	sourceSnapshot, err := runs.ExecutionSnapshotForRun(reviewed.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		t.Fatal(err)
	}
	subject := application.AgentPlanningSubject{
		TenantID: sourceSpec.TenantID, OrganizationID: "local",
		WorkspaceID: sourceSpec.WorkspaceID, RepositoryID: sourceSpec.Repository.RepositoryID,
	}
	governedArtifacts, err := artifactrepo.Open(stateStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentRepository, err := agentadapter.New(runs, governedArtifacts, artifactrepo.DefaultAuthority)
	if err != nil {
		t.Fatal(err)
	}

	fixture := &hailixHTTPFormalFixture{
		t: t, runs: runs, artifacts: agentRepository, subject: subject,
		proof: []byte("signed-hailix-formal-callback-proof"),
	}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()
	t.Setenv(hailixexecution.BearerTokenEnvironment, "hailix-formal-test-token")
	t.Setenv(hailixexecution.CredentialRevisionEnvironment, "credential-revision-1")
	runArguments = append(runArguments,
		"--execution-backend", "hailix-http",
		"--hailix-base-url", server.URL+"/api/",
		"--hailix-capability-verifier-id", "hailix-platform-capability-verifier",
		"--hailix-capability-verifier-revision", "v1",
		"--hailix-capability-verifier-sha256", strings.Repeat("c", 64),
		"--hailix-callback-verifier-id", "hailix-platform-callback-verifier",
		"--hailix-callback-verifier-revision", "v1",
		"--hailix-callback-verifier-sha256", strings.Repeat("d", 64),
	)
	runOptions, err := parseFormalAgentRunFlags(runArguments)
	if err != nil {
		t.Fatal(err)
	}
	runner := &forbiddenFormalRunner{}

	firstErr := executeFormalAgentRunMode(
		t.Context(), runOptions, &bytes.Buffer{}, runner, nil, nil, nil, nil, nil,
	)
	if !errors.Is(firstErr, hailixexecution.ErrHTTPOutcomeUnknown) {
		t.Fatalf("first formal run error = %v", firstErr)
	}
	fixture.mutex.Lock()
	firstEnsure := append([]byte(nil), fixture.firstEnsure...)
	firstEnsureCalls := fixture.ensureCalls
	firstStarts := fixture.providerStarts
	fixture.mutex.Unlock()
	if firstEnsureCalls != 1 || firstStarts != 1 || len(firstEnsure) == 0 {
		t.Fatalf("first dispatch ensure_calls=%d provider_starts=%d", firstEnsureCalls, firstStarts)
	}

	var recoveredOutput bytes.Buffer
	if err := executeFormalAgentRunMode(
		t.Context(), runOptions, &recoveredOutput, runner, nil, nil, nil, nil, nil,
	); err != nil {
		t.Fatalf("recover formal Hailix dispatch: %v\n%s", err, recoveredOutput.String())
	}
	var recovered formalAgentRunOutput
	if err := json.Unmarshal(recoveredOutput.Bytes(), &recovered); err != nil {
		t.Fatal(err)
	}
	fixture.mutex.Lock()
	secondEnsure := append([]byte(nil), fixture.lastEnsure...)
	ensureCalls := fixture.ensureCalls
	providerStarts := fixture.providerStarts
	awaitCalls := fixture.awaitCalls
	verifyCalls := fixture.verifyCalls
	fixture.mutex.Unlock()
	if !bytes.Equal(firstEnsure, secondEnsure) || ensureCalls != 2 || providerStarts != 1 ||
		awaitCalls != 1 || verifyCalls != 1 || !recovered.RecoveredDispatch ||
		recovered.Status != contractsv1alpha1.StageExecutionSucceeded ||
		recovered.ReviewRun == nil || recovered.ReviewRun.Status != runmodel.RunStatusSucceeded ||
		recovered.FinalRunRef == nil || runner.calls != 0 {
		t.Fatalf(
			"recovered=%+v ensure_calls=%d starts=%d await=%d verify=%d runner=%d exact=%t",
			recovered, ensureCalls, providerStarts, awaitCalls, verifyCalls, runner.calls,
			bytes.Equal(firstEnsure, secondEnsure),
		)
	}

	var terminalRetryOutput bytes.Buffer
	if err := executeFormalAgentRunMode(
		t.Context(), runOptions, &terminalRetryOutput, runner, nil, nil, nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	var terminalRetry formalAgentRunOutput
	if err := json.Unmarshal(terminalRetryOutput.Bytes(), &terminalRetry); err != nil {
		t.Fatal(err)
	}
	fixture.mutex.Lock()
	finalEnsureCalls := fixture.ensureCalls
	finalAwaitCalls := fixture.awaitCalls
	finalVerifyCalls := fixture.verifyCalls
	fixture.mutex.Unlock()
	if finalEnsureCalls != ensureCalls || finalAwaitCalls != awaitCalls ||
		finalVerifyCalls != verifyCalls || !terminalRetry.RecoveredFinalRun ||
		terminalRetry.FinalRunRef == nil || *terminalRetry.FinalRunRef != *recovered.FinalRunRef {
		t.Fatalf(
			"terminal retry=%+v ensure=%d await=%d verify=%d",
			terminalRetry,
			finalEnsureCalls,
			finalAwaitCalls,
			finalVerifyCalls,
		)
	}
}

type hailixHTTPFormalFixture struct {
	t         *testing.T
	runs      *runrepo.Repository
	artifacts *agentadapter.Repository
	subject   application.AgentPlanningSubject
	proof     []byte

	mutex          sync.Mutex
	plan           contractsv1alpha1.AgentStagePlan
	request        contractsv1alpha1.StageExecutionRequest
	firstEnsure    []byte
	lastEnsure     []byte
	ensureCalls    int
	providerStarts int
	awaitCalls     int
	verifyCalls    int
}

func (fixture *hailixHTTPFormalFixture) serveHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodPost ||
		request.Header.Get("Authorization") != "Bearer hailix-formal-test-token" ||
		request.Header.Get("X-Hailix-Platform-Execution-Version") !=
			hailixexecution.HTTPContractSchemaVersion {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	switch request.URL.Path {
	case "/api/v1/platform-executions/capabilities:resolve":
		fixture.resolveCapability(writer, request)
	case "/api/v1/platform-executions:ensure":
		fixture.ensure(writer, request)
	case "/api/v1/platform-executions:await":
		fixture.await(writer, request)
	case "/api/v1/platform-executions/callbacks:verify":
		fixture.verify(writer, request)
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func (fixture *hailixHTTPFormalFixture) resolveCapability(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var command hailixCapabilityRequest
	if err := decodeHailixTestJSON(request, &command); err != nil {
		fixture.t.Errorf("decode capability command: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(command.CanonicalPlan)
	if err != nil || plan.PlanID != command.PlanID || plan.SHA256 != command.PlanSHA256 {
		fixture.t.Errorf("capability plan mismatch: %+v %v", command, err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind: plan.Runtime.Ref.ID, RuntimeID: "hailix-formal-runtime-1",
		RuntimeRevision: plan.Runtime.Ref.Revision, RuntimeSHA256: plan.Runtime.Ref.SHA256,
		BuildIdentity: plan.BuildIdentity,
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools: append([]string(nil), plan.ToolAuthority.Tools...),
			ModelEgress:  plan.ModelAuthority.ModelEgress, ToolNetwork: plan.ToolAuthority.ToolNetwork,
			WorkspaceReads:     plan.ToolAuthority.WorkspaceReads,
			WorkspaceWrites:    plan.ToolAuthority.WorkspaceWrites,
			RemoteWrites:       plan.ToolAuthority.RemoteWrites,
			MaxDelegationDepth: plan.ToolAuthority.MaxDelegationDepth,
		},
		Trust: contractsv1alpha1.ExecutorTrust{
			Authority: contractsv1alpha1.ExecutorTrustAuthorityPlatform,
			CapabilityVerifier: contractsv1alpha1.VersionedRef{
				ID: "hailix-platform-capability-verifier", Revision: "v1",
				SHA256: strings.Repeat("c", 64),
			},
			CallbackVerifier: contractsv1alpha1.VersionedRef{
				ID: "hailix-platform-callback-verifier", Revision: "v1",
				SHA256: strings.Repeat("d", 64),
			},
		},
	}
	capability.SHA256, err = contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		fixture.t.Error(err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	fixture.mutex.Lock()
	fixture.plan = plan
	fixture.mutex.Unlock()
	writeHailixTestJSON(fixture.t, writer, map[string]any{
		"schema_version": hailixexecution.HTTPContractSchemaVersion,
		"receipt": map[string]any{
			"subject": command.Subject, "plan_id": command.PlanID,
			"plan_sha256": command.PlanSHA256, "capability": capability,
			"verifier": map[string]any{
				"id": "hailix-platform-capability-verifier", "revision": "v1",
				"sha256": strings.Repeat("c", 64),
			},
		},
	})
}

func (fixture *hailixHTTPFormalFixture) ensure(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var command hailixExecutionRequest
	if err := decodeHailixTestJSON(request, &command); err != nil {
		fixture.t.Errorf("decode ensure command: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	decoded, err := contractsv1alpha1.DecodeStageExecutionRequest(command.CanonicalRequest)
	if err != nil || request.Header.Get("Idempotency-Key") != decoded.IdempotencyKey {
		fixture.t.Errorf("ensure request mismatch: %+v %v", command, err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	canonical := append([]byte(nil), command.CanonicalRequest...)
	fixture.mutex.Lock()
	fixture.ensureCalls++
	if fixture.providerStarts == 0 {
		fixture.providerStarts++
		fixture.request = decoded
		fixture.firstEnsure = canonical
	} else if !reflect.DeepEqual(fixture.request, decoded) ||
		!bytes.Equal(fixture.firstEnsure, canonical) {
		fixture.mutex.Unlock()
		fixture.t.Error("exact ensure recovery changed the immutable request")
		writer.WriteHeader(http.StatusConflict)
		return
	}
	fixture.lastEnsure = canonical
	ensureCall := fixture.ensureCalls
	fixture.mutex.Unlock()
	if ensureCall == 1 {
		writer.WriteHeader(http.StatusServiceUnavailable)
		writeHailixTestJSON(fixture.t, writer, map[string]any{
			"schema_version": hailixexecution.HTTPContractSchemaVersion,
			"error": map[string]any{
				"code": "response_lost_after_commit", "message": "retry exact ensure",
				"retryable": true,
			},
		})
		return
	}
	writeHailixTestJSON(fixture.t, writer, fixture.ensureReceipt(command.Subject, decoded))
}

func (fixture *hailixHTTPFormalFixture) await(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var command hailixAwaitRequest
	if err := decodeHailixTestJSON(request, &command); err != nil {
		fixture.t.Errorf("decode await command: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	fixture.mutex.Lock()
	fixture.awaitCalls++
	plan := fixture.plan
	executionRequest := fixture.request
	fixture.mutex.Unlock()
	if command.ExecutionID != executionRequest.ExecutionID ||
		command.ProviderHandle != "hailix-formal-handle-1" {
		fixture.t.Errorf("await escaped exact binding: %+v", command)
		writer.WriteHeader(http.StatusConflict)
		return
	}
	now := time.Now().UTC()
	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:   contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "hailix-formal-hypothesis-set-1", PlanID: plan.PlanID,
		SourceRunID: plan.ReviewRunID, ExecutionID: executionRequest.ExecutionID,
		ReviewRunID: plan.ReviewRunID, TargetDigest: plan.TargetDigest,
		Completeness:        contractsv1alpha1.AgentReviewComplete,
		CompletenessReasons: []string{}, NormalizationDecisions: []contractsv1alpha1.HypothesisNormalizationDecision{},
		Hypotheses:    []contractsv1alpha1.ReviewHypothesis{},
		DedupClusters: []contractsv1alpha1.HypothesisDedupCluster{},
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			Gaps: []contractsv1alpha1.AgentReviewCoverageGap{},
		},
		GeneratedAt: now,
	}
	setJSON, err := json.Marshal(set)
	if err != nil {
		fixture.t.Error(err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, err := contractsv1alpha1.DecodeReviewHypothesisSet(setJSON); err != nil {
		fixture.t.Error(err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	localRef, err := fixture.runs.PutArtifact(contractsv1alpha1.ReviewHypothesisSetSchemaVersion, setJSON)
	if err != nil {
		fixture.t.Error(err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	output, err := fixture.artifacts.ProjectReadOnly(
		request.Context(), fixture.subject, application.AgentArtifactProjectionRequest{
			Role: "stage-output", ReviewRunID: executionRequest.ReviewRunID,
			StageID: executionRequest.Stage.ID, Source: localRef,
			ExactContent: setJSON, FrozenAt: now,
		},
	)
	if err != nil {
		fixture.t.Error(err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	result := contractsv1alpha1.StageExecutionResult{
		SchemaVersion: contractsv1alpha1.StageExecutionResultSchemaVersion,
		RequestSHA256: executionRequest.RequestSHA256, ExecutionID: executionRequest.ExecutionID,
		Attempt: executionRequest.Attempt, Generation: executionRequest.Generation,
		FencingToken: executionRequest.FencingToken, IdempotencyKey: executionRequest.IdempotencyKey,
		Status: contractsv1alpha1.StageExecutionSucceeded, Output: &output,
		Completeness: "complete", CompletenessNotes: []string{},
		CapabilitySHA256: executionRequest.Capability.SHA256, RecordedAt: now,
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		fixture.t.Error(err)
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeHailixTestJSON(fixture.t, writer, map[string]any{
		"schema_version":        hailixexecution.HTTPContractSchemaVersion,
		"canonical_result":      json.RawMessage(resultJSON),
		"callback_proof_base64": base64.StdEncoding.EncodeToString(fixture.proof),
	})
}

func (fixture *hailixHTTPFormalFixture) verify(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var command hailixVerifyRequest
	if err := decodeHailixTestJSON(request, &command); err != nil {
		fixture.t.Errorf("decode callback verification: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	proof, err := base64.StdEncoding.Strict().DecodeString(command.CallbackProofBase64)
	if err != nil || !bytes.Equal(proof, fixture.proof) {
		fixture.t.Errorf("callback proof mismatch: %q %v", command.CallbackProofBase64, err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	fixture.mutex.Lock()
	fixture.verifyCalls++
	fixture.mutex.Unlock()
	writeHailixTestJSON(fixture.t, writer, map[string]any{
		"schema_version": hailixexecution.HTTPContractSchemaVersion,
		"receipt": map[string]any{
			"subject": command.Subject, "binding_id": command.Binding.BindingID,
			"binding_sha256":  command.Binding.BindingSHA256,
			"provider_handle": command.Binding.ProviderHandle,
			"result_sha256":   command.ResultSHA256, "proof_sha256": command.ProofSHA256,
			"verifier_id": "hailix-platform-callback-verifier", "verifier_revision": "v1",
			"verifier_sha256": strings.Repeat("d", 64),
		},
	})
}

func (fixture *hailixHTTPFormalFixture) ensureReceipt(
	subject hailixTestSubject,
	request contractsv1alpha1.StageExecutionRequest,
) map[string]any {
	return map[string]any{
		"schema_version": hailixexecution.HTTPContractSchemaVersion,
		"receipt": map[string]any{
			"subject": subject, "provider_handle": "hailix-formal-handle-1",
			"execution_id": request.ExecutionID, "attempt": request.Attempt,
			"generation": request.Generation, "fencing_token": request.FencingToken,
			"idempotency_key": request.IdempotencyKey, "request_sha256": request.RequestSHA256,
			"capability_sha256": request.Capability.SHA256,
		},
	}
}

type hailixTestSubject struct {
	TenantID       string `json:"tenant_id"`
	OrganizationID string `json:"organization_id"`
	WorkspaceID    string `json:"workspace_id"`
	RepositoryID   string `json:"repository_id"`
}

type hailixCapabilityRequest struct {
	SchemaVersion string            `json:"schema_version"`
	Subject       hailixTestSubject `json:"subject"`
	PlanID        string            `json:"plan_id"`
	PlanSHA256    string            `json:"plan_sha256"`
	CanonicalPlan json.RawMessage   `json:"canonical_plan"`
}

type hailixExecutionRequest struct {
	SchemaVersion    string            `json:"schema_version"`
	Subject          hailixTestSubject `json:"subject"`
	CanonicalRequest json.RawMessage   `json:"canonical_request"`
}

type hailixAwaitRequest struct {
	SchemaVersion    string            `json:"schema_version"`
	Subject          hailixTestSubject `json:"subject"`
	ProviderHandle   string            `json:"provider_handle"`
	ExecutionID      string            `json:"execution_id"`
	Attempt          int               `json:"attempt"`
	Generation       int               `json:"generation"`
	FencingToken     uint64            `json:"fencing_token"`
	IdempotencyKey   string            `json:"idempotency_key"`
	RequestSHA256    string            `json:"request_sha256"`
	CapabilitySHA256 string            `json:"capability_sha256"`
}

type hailixVerifyRequest struct {
	SchemaVersion       string                          `json:"schema_version"`
	Subject             hailixTestSubject               `json:"subject"`
	Binding             hailixexecution.CallbackBinding `json:"binding"`
	CanonicalResult     json.RawMessage                 `json:"canonical_result"`
	ResultSHA256        string                          `json:"result_sha256"`
	CallbackProofBase64 string                          `json:"callback_proof_base64"`
	ProofSHA256         string                          `json:"proof_sha256"`
}

type forbiddenFormalRunner struct {
	calls int
}

func (runner *forbiddenFormalRunner) Run(
	context.Context,
	agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	runner.calls++
	return agentshadowworker.Result{}, fmt.Errorf("local Pi runner must not execute for Hailix backend")
}

func decodeHailixTestJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeHailixTestJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode Hailix fixture response: %v", err)
	}
}
