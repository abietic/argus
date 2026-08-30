package hailixexecution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/execution"
	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestAdapterRecoversUnknownEnsureByExactLookup(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	client.loseFirstEnsureResponse = true
	adapter := newAdapter(t, client)

	if _, err := adapter.Ensure(context.Background(), request); err == nil {
		t.Fatal("Ensure() first response unexpectedly succeeded")
	}
	handle, found, err := adapter.Lookup(context.Background(), request)
	if err != nil || !found {
		t.Fatalf("Lookup() = (%+v, %v, %v)", handle, found, err)
	}
	if handle.ProviderHandle != client.receipt.ProviderHandle || client.starts != 1 {
		t.Fatalf("Lookup() handle=%+v starts=%d", handle, client.starts)
	}
	if _, err := contractsv1alpha1.DecodeStageExecutionRequest(
		client.lookup.CanonicalRequestJSON,
	); err != nil {
		t.Fatalf("Lookup() did not carry canonical StageExecutionRequest: %v", err)
	}
	if string(client.ensure.CanonicalRequestJSON) != string(client.lookup.CanonicalRequestJSON) {
		t.Fatal("Ensure and Lookup did not use identical canonical request bytes")
	}
	if client.ensure.Subject != clientSubject(validSubject()) ||
		client.lookup.Subject != clientSubject(validSubject()) {
		t.Fatal("Ensure and Lookup omitted the exact bound subject")
	}
}

func TestAdapterResolvesPinnedCapabilityForExactPlan(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	plan := validPlan(t)
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind: plan.Runtime.Ref.ID, RuntimeID: "hailix-runtime-1",
		RuntimeRevision: plan.Runtime.Ref.Revision,
		RuntimeSHA256:   plan.Runtime.Ref.SHA256, BuildIdentity: plan.BuildIdentity,
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:       append([]string(nil), plan.ToolAuthority.Tools...),
			ModelEgress:        plan.ModelAuthority.ModelEgress,
			ToolNetwork:        plan.ToolAuthority.ToolNetwork,
			WorkspaceReads:     plan.ToolAuthority.WorkspaceReads,
			WorkspaceWrites:    plan.ToolAuthority.WorkspaceWrites,
			RemoteWrites:       plan.ToolAuthority.RemoteWrites,
			MaxDelegationDepth: plan.ToolAuthority.MaxDelegationDepth,
		},
		Trust: testHailixExecutorTrust(),
	}
	var err error
	capability.SHA256, err = contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	client.capabilityReceipt = CapabilityReceipt{
		Capability: capability,
		Verifier: VerifierRef{
			ID: "hailix-platform-capability-verifier", Revision: "v1",
			SHA256: strings.Repeat("c", 64),
		},
	}
	adapter := newAdapter(t, client)
	got, err := adapter.ResolveAgentExecutorCapability(
		context.Background(), validSubject(), plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, capability) || client.capability.PlanID != plan.PlanID ||
		client.capability.PlanSHA256 != plan.SHA256 {
		t.Fatalf("ResolveAgentExecutorCapability() = %+v command=%+v", got, client.capability)
	}
	decoded, err := contractsv1alpha1.DecodeAgentStagePlan(
		client.capability.CanonicalPlanJSON,
	)
	if err != nil || decoded.PlanID != plan.PlanID {
		t.Fatalf("capability command plan = %+v, %v", decoded, err)
	}

	client.capabilityReceipt.Verifier.SHA256 = strings.Repeat("b", 64)
	if _, err := adapter.ResolveAgentExecutorCapability(
		context.Background(), validSubject(), plan,
	); !errors.Is(err, ErrClientContractViolation) {
		t.Fatalf("ResolveAgentExecutorCapability(unpinned) error = %v", err)
	}
}

func TestAdapterRejectsEnsureReceiptSubstitution(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	client.receipt.RequestSHA256 = strings.Repeat("b", 64)
	adapter := newAdapter(t, client)

	if _, err := adapter.Ensure(context.Background(), request); !errors.Is(err, ErrClientContractViolation) {
		t.Fatalf("Ensure() error = %v, want client contract violation", err)
	}
}

func TestAdapterRejectsEnsureReceiptSubjectSubstitution(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	client.receipt.Subject.RepositoryID = "repository-other"
	adapter := newAdapter(t, client)

	if _, err := adapter.Ensure(context.Background(), request); !errors.Is(err, ErrClientContractViolation) {
		t.Fatalf("Ensure() error = %v, want client contract violation", err)
	}
}

func TestAdapterRejectsCrossSubjectRequestBeforeClient(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	adapter := newAdapter(t, client)
	request.TenantID = "tenant-other"
	resealed, err := contractsv1alpha1.SealStageExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Ensure(context.Background(), resealed); err == nil {
		t.Fatal("Ensure() accepted cross-tenant request")
	}
	if client.ensureCalls != 0 {
		t.Fatal("Ensure() called client before subject validation")
	}
}

func TestAdapterAwaitsExactCanonicalTerminalAndVerifiesCallback(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	result := validResult(t, request)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	client.terminal = TerminalEnvelope{
		CanonicalResultJSON: resultJSON,
		CallbackProof:       []byte("signed-hailix-callback-statement"),
	}
	adapter := newAdapter(t, client)
	handle, err := adapter.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	got, proof, err := adapter.AwaitResult(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionID != request.ExecutionID || string(proof) != string(client.terminal.CallbackProof) {
		t.Fatalf("AwaitResult() = (%+v, %q)", got, proof)
	}
	if client.await.Subject != clientSubject(validSubject()) {
		t.Fatalf("AwaitExecution omitted bound subject: %+v", client.await)
	}
	proof[0] = 'X'
	if client.terminal.CallbackProof[0] == 'X' {
		t.Fatal("AwaitResult() leaked client-owned proof bytes")
	}

	binding := validBinding(t, request, handle)
	verifier, err := adapter.VerifyAgentStageResultCallback(
		context.Background(), validSubject(), binding, request.Capability.Trust, resultJSON,
		client.terminal.CallbackProof,
	)
	if err != nil {
		t.Fatal(err)
	}
	if verifier.ID != "hailix-platform-callback-verifier" ||
		client.verify.ResultSHA256 != shaHex(resultJSON) ||
		client.verify.ProofSHA256 != shaHex(client.terminal.CallbackProof) ||
		client.verify.Binding.BindingID != binding.BindingID ||
		client.verify.Binding.BindingSHA256 != binding.SHA256 {
		t.Fatalf("VerifyResultCallback mapping is incomplete: %+v / %+v", verifier, client.verify)
	}
}

func TestAdapterRejectsTerminalAndCallbackIdentityDrift(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	result := validResult(t, request)
	result.ExecutionID = "execution-substituted"
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	client.terminal = TerminalEnvelope{
		CanonicalResultJSON: resultJSON,
		CallbackProof:       []byte("signed-hailix-callback-statement"),
	}
	adapter := newAdapter(t, client)
	handle, err := adapter.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.AwaitResult(context.Background(), handle); !errors.Is(err, ErrClientContractViolation) {
		t.Fatalf("AwaitResult() error = %v, want client contract violation", err)
	}

	valid := validResult(t, request)
	validJSON, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	binding := validBinding(t, request, handle)
	client.callbackReceiptMutator = func(receipt *CallbackVerificationReceipt) {
		receipt.BindingSHA256 = strings.Repeat("c", 64)
	}
	if _, err := adapter.VerifyAgentStageResultCallback(
		context.Background(), validSubject(), binding, request.Capability.Trust, validJSON,
		[]byte("signed-hailix-callback-statement"),
	); !errors.Is(err, ErrClientContractViolation) {
		t.Fatalf("VerifyAgentStageResultCallback() error = %v", err)
	}
}

func TestAdapterMapsExactTerminalCancelAuthority(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	adapter := newAdapter(t, client)
	handle, err := adapter.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	cancel := validCancel(request, handle)
	if err := adapter.Cancel(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	if client.cancel.TerminalGateSHA256 != cancel.Authority.TerminalGateSHA256 ||
		client.cancel.IntentSHA256 != cancel.Authority.IntentSHA256 ||
		client.cancel.ProviderHandle != handle.ProviderHandle ||
		client.cancel.CancelIdempotencyKey != cancel.CancelIdempotencyKey ||
		client.cancel.Subject != clientSubject(validSubject()) {
		t.Fatalf("CancelExecution mapping = %+v", client.cancel)
	}

	escaped := cancel
	escaped.Authority.TenantID = "tenant-other"
	client.cancelCalls = 0
	if err := adapter.Cancel(context.Background(), escaped); err == nil {
		t.Fatal("Cancel() accepted a cross-tenant terminal authority")
	}
	if client.cancelCalls != 0 {
		t.Fatal("Cancel() called client before subject validation")
	}
}

func TestAdapterRejectsCrossSubjectCallbackBeforeClient(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	adapter := newAdapter(t, client)
	handle, err := adapter.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	result := validResult(t, request)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	binding := validBinding(t, request, handle)
	escaped := validSubject()
	escaped.RepositoryID = "repository-other"
	if _, err := adapter.VerifyAgentStageResultCallback(
		context.Background(), escaped, binding, request.Capability.Trust, resultJSON, []byte("proof"),
	); err == nil {
		t.Fatal("VerifyAgentStageResultCallback() accepted cross-repository subject")
	}
	if client.verifyCalls != 0 {
		t.Fatal("callback verifier client was called before subject validation")
	}
}

func TestAdapterRejectsCallbackWhenFrozenTrustDiffersFromPinnedRoot(t *testing.T) {
	request := validRequest(t)
	client := newFakeClient(t, request)
	adapter := newAdapter(t, client)
	handle, err := adapter.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	resultJSON, err := json.Marshal(validResult(t, request))
	if err != nil {
		t.Fatal(err)
	}
	trust := request.Capability.Trust
	trust.CallbackVerifier.ID = "other-callback-verifier"
	if _, err := adapter.VerifyAgentStageResultCallback(
		context.Background(), validSubject(), validBinding(t, request, handle), trust,
		resultJSON, []byte("signed-hailix-callback-statement"),
	); !errors.Is(err, ErrClientContractViolation) {
		t.Fatalf("VerifyAgentStageResultCallback() error = %v", err)
	}
	if client.verifyCalls != 0 {
		t.Fatal("callback verifier client was called before frozen trust validation")
	}
}

type fakeClient struct {
	capabilityReceipt       CapabilityReceipt
	capability              CapabilityCommand
	receipt                 EnsureReceipt
	terminal                TerminalEnvelope
	ensure                  EnsureCommand
	lookup                  LookupCommand
	cancel                  CancelCommand
	await                   AwaitCommand
	verify                  VerifyCallbackCommand
	starts                  int
	ensureCalls             int
	cancelCalls             int
	verifyCalls             int
	capabilityCalls         int
	loseFirstEnsureResponse bool
	callbackReceiptMutator  func(*CallbackVerificationReceipt)
}

func (client *fakeClient) GetCapabilitySnapshot(
	_ context.Context,
	command CapabilityCommand,
) (CapabilityReceipt, error) {
	client.capabilityCalls++
	client.capability = CapabilityCommand{
		Subject: command.Subject, PlanID: command.PlanID, PlanSHA256: command.PlanSHA256,
		CanonicalPlanJSON: append([]byte(nil), command.CanonicalPlanJSON...),
	}
	receipt := client.capabilityReceipt
	receipt.Subject = command.Subject
	receipt.PlanID = command.PlanID
	receipt.PlanSHA256 = command.PlanSHA256
	return receipt, nil
}

func newFakeClient(
	t *testing.T,
	request contractsv1alpha1.StageExecutionRequest,
) *fakeClient {
	t.Helper()
	return &fakeClient{receipt: EnsureReceipt{
		Subject:        clientSubject(validSubject()),
		ProviderHandle: "hailix/platform-executions/opaque-1",
		ExecutionID:    request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey:   request.IdempotencyKey,
		RequestSHA256:    request.RequestSHA256,
		CapabilitySHA256: request.Capability.SHA256,
	}}
}

func (client *fakeClient) EnsureExecution(
	_ context.Context,
	command EnsureCommand,
) (EnsureReceipt, error) {
	client.ensureCalls++
	client.ensure = EnsureCommand{
		Subject: command.Subject, CanonicalRequestJSON: append([]byte(nil), command.CanonicalRequestJSON...),
	}
	if client.starts == 0 {
		client.starts++
	}
	if client.loseFirstEnsureResponse && client.ensureCalls == 1 {
		return EnsureReceipt{}, errors.New("transport outcome unknown")
	}
	return client.receipt, nil
}

func (client *fakeClient) LookupExecution(
	_ context.Context,
	command LookupCommand,
) (EnsureReceipt, bool, error) {
	client.lookup = LookupCommand{
		Subject: command.Subject, CanonicalRequestJSON: append([]byte(nil), command.CanonicalRequestJSON...),
	}
	if client.starts == 0 {
		return EnsureReceipt{}, false, nil
	}
	return client.receipt, true, nil
}

func (client *fakeClient) CancelExecution(
	_ context.Context,
	command CancelCommand,
) error {
	client.cancelCalls++
	client.cancel = command
	return nil
}

func (client *fakeClient) AwaitExecution(
	_ context.Context,
	command AwaitCommand,
) (TerminalEnvelope, error) {
	client.await = command
	return TerminalEnvelope{
		CanonicalResultJSON: append([]byte(nil), client.terminal.CanonicalResultJSON...),
		CallbackProof:       append([]byte(nil), client.terminal.CallbackProof...),
	}, nil
}

func (client *fakeClient) VerifyResultCallback(
	_ context.Context,
	command VerifyCallbackCommand,
) (CallbackVerificationReceipt, error) {
	client.verifyCalls++
	client.verify = command
	receipt := CallbackVerificationReceipt{
		Subject: command.Subject, BindingID: command.Binding.BindingID,
		BindingSHA256:  command.Binding.BindingSHA256,
		ProviderHandle: command.Binding.ProviderHandle,
		ResultSHA256:   command.ResultSHA256, ProofSHA256: command.ProofSHA256,
		VerifierID:       "hailix-platform-callback-verifier",
		VerifierRevision: "v1", VerifierSHA256: strings.Repeat("d", 64),
	}
	if client.callbackReceiptMutator != nil {
		client.callbackReceiptMutator(&receipt)
	}
	return receipt, nil
}

func newAdapter(t *testing.T, client Client) *Adapter {
	t.Helper()
	adapter, err := New(client, validSubject(), TrustConfig{
		CapabilityVerifier: VerifierRef{
			ID: "hailix-platform-capability-verifier", Revision: "v1",
			SHA256: strings.Repeat("c", 64),
		},
		CallbackVerifier: VerifierRef{
			ID: "hailix-platform-callback-verifier", Revision: "v1",
			SHA256: strings.Repeat("d", 64),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func validPlan(t *testing.T) contractsv1alpha1.AgentStagePlan {
	t.Helper()
	data, err := os.ReadFile("../../examples/agent-stage-plan.json")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(data)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func validSubject() application.AgentPlanningSubject {
	return application.AgentPlanningSubject{
		TenantID: "tenant-1", OrganizationID: "organization-1",
		WorkspaceID: "workspace-1", RepositoryID: "repository-1",
	}
}

func testHailixExecutorTrust() contractsv1alpha1.ExecutorTrust {
	return contractsv1alpha1.ExecutorTrust{
		Authority: contractsv1alpha1.ExecutorTrustAuthorityPlatform,
		CapabilityVerifier: contractsv1alpha1.VersionedRef{
			ID: "hailix-platform-capability-verifier", Revision: "v1",
			SHA256: strings.Repeat("c", 64),
		},
		CallbackVerifier: contractsv1alpha1.VersionedRef{
			ID: "hailix-platform-callback-verifier", Revision: "v1",
			SHA256: strings.Repeat("d", 64),
		},
	}
}

func validRequest(t *testing.T) contractsv1alpha1.StageExecutionRequest {
	t.Helper()
	digest := strings.Repeat("a", 64)
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind: "eino-acp", RuntimeID: "eino-agent-review-safe",
		RuntimeRevision: "1", RuntimeSHA256: digest, BuildIdentity: "build-1",
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:       []string{},
			ModelEgress:        contractsv1alpha1.AgentStageModelEgressProviderBrokerOnly,
			ToolNetwork:        contractsv1alpha1.AgentStageSideEffectsDeny,
			WorkspaceReads:     contractsv1alpha1.AgentStageWorkspaceReadFrozenInputOnly,
			WorkspaceWrites:    contractsv1alpha1.AgentStageSideEffectsDeny,
			RemoteWrites:       contractsv1alpha1.AgentStageSideEffectsDeny,
			MaxDelegationDepth: 0,
		},
		Trust: testHailixExecutorTrust(),
	}
	var err error
	capability.SHA256, err = contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	artifact := func(name, contract string) contractsv1alpha1.ArtifactBinding {
		return contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:    "artifact://local/sha256/" + name,
				SHA256: digest, SizeBytes: 1,
			},
			Contract: contract,
		}
	}
	request := contractsv1alpha1.StageExecutionRequest{
		SchemaVersion: contractsv1alpha1.StageExecutionRequestSchemaVersion,
		ExecutionID:   "execution-1", ReviewRunID: "run-1",
		TenantID: "tenant-1", WorkspaceID: "workspace-1",
		WorkloadID: "workload-1", LeaseID: "lease-1", LeaseWorkerID: "worker-1",
		Stage:   contractsv1alpha1.VersionedRef{ID: "agent_hypothesize", Revision: "1", SHA256: digest},
		Attempt: 1, Generation: 1, FencingToken: 7,
		IdempotencyKey:    "execution-1-agent-hypothesize-1",
		Plan:              artifact("plan", contractsv1alpha1.AgentStagePlanSchemaVersion),
		ExecutionSnapshot: artifact("snapshot", contractsv1alpha1.StageExecutionSnapshotContract),
		ReviewInput:       artifact("input", contractsv1alpha1.StageExecutionReviewInputContract),
		Upstream:          []contractsv1alpha1.ArtifactBinding{},
		OutputContract:    contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		Capability:        capability,
		Deadline:          time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		SideEffects:       contractsv1alpha1.AgentStageSideEffectsDeny,
	}
	sealed, err := contractsv1alpha1.SealStageExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func validResult(
	t *testing.T,
	request contractsv1alpha1.StageExecutionRequest,
) contractsv1alpha1.StageExecutionResult {
	t.Helper()
	return contractsv1alpha1.StageExecutionResult{
		SchemaVersion: contractsv1alpha1.StageExecutionResultSchemaVersion,
		RequestSHA256: request.RequestSHA256, ExecutionID: request.ExecutionID,
		Attempt: request.Attempt, Generation: request.Generation,
		FencingToken: request.FencingToken, IdempotencyKey: request.IdempotencyKey,
		Status: contractsv1alpha1.StageExecutionSucceeded,
		Output: &contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:    "artifact://hailix/objects/result-1",
				SHA256: strings.Repeat("e", 64), SizeBytes: 32,
			},
			Contract: request.OutputContract,
		},
		Completeness: "complete", CompletenessNotes: []string{},
		CapabilitySHA256: request.Capability.SHA256,
		RecordedAt:       time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC),
	}
}

func validBinding(
	t *testing.T,
	request contractsv1alpha1.StageExecutionRequest,
	handle execution.Handle,
) runmodel.AgentStageExecutionBinding {
	t.Helper()
	digest := strings.Repeat("f", 64)
	subject := validSubject()
	binding, err := runmodel.SealAgentStageExecutionBinding(
		runmodel.AgentStageExecutionBinding{
			Subject: runmodel.AgentPlanningSubject{
				TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
				WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
			},
			ReviewRunID: request.ReviewRunID,
			Stage: runmodel.AgentStageRef{
				ID: request.Stage.ID, Revision: request.Stage.Revision,
				SHA256: request.Stage.SHA256,
			},
			WorkloadID: request.WorkloadID, LeaseID: request.LeaseID,
			LeaseWorker:  request.LeaseWorkerID,
			IntentID:     request.ReviewRunID + "-agent-stage-dispatch-aaaaaaaaaaaaaaaaaaaaaaaa",
			IntentSHA256: digest,
			RequestRef: runmodel.ArtifactRef{
				URI: "artifact://local/sha256/" + digest, SHA256: digest,
				SizeBytes: 512, Contract: runmodel.ContractStageExecutionRequest,
			},
			RequestSemanticSHA256: request.RequestSHA256,
			ExecutionID:           handle.ExecutionID, Attempt: handle.Attempt,
			Generation: handle.Generation, FencingToken: handle.FencingToken,
			CreateIdempotencyKey: handle.IdempotencyKey,
			ProviderHandle:       handle.ProviderHandle,
			CapabilitySHA256:     handle.CapabilitySHA256,
			RecordedAt:           time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func validCancel(
	request contractsv1alpha1.StageExecutionRequest,
	handle execution.Handle,
) execution.CancelRequest {
	digest := strings.Repeat("a", 64)
	return execution.CancelRequest{
		Authority: execution.TerminalCancelAuthority{
			TenantID: request.TenantID, WorkspaceID: request.WorkspaceID,
			ReviewRunID: request.ReviewRunID, StageID: request.Stage.ID,
			StageRevision: request.Stage.Revision, StageSHA256: request.Stage.SHA256,
			IntentID: "intent-1", IntentSHA256: digest,
			TerminalGateID: "terminal-gate-1", TerminalGateSHA256: digest,
			CancelIdempotencyKey: "cancel-execution-1",
		},
		ProviderHandle: handle.ProviderHandle, ExecutionID: handle.ExecutionID,
		Attempt: handle.Attempt, Generation: handle.Generation,
		FencingToken:         handle.FencingToken,
		CreateIdempotencyKey: handle.IdempotencyKey,
		CancelIdempotencyKey: "cancel-execution-1",
		RequestSHA256:        handle.RequestSHA256,
		CapabilitySHA256:     handle.CapabilitySHA256,
	}
}
