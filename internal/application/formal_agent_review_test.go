package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"argus.local/argus/internal/execution"
	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestFormalAgentReviewOrchestratorRoutesSucceededResultToHypothesisIngress(t *testing.T) {
	fixture := formalOrchestrationFixture(contractsv1alpha1.StageExecutionSucceeded)
	orchestrator, err := NewFormalAgentReviewOrchestrator(
		fixture.dispatcher, fixture.executor, fixture.ingress,
	)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := orchestrator.Run(
		t.Context(),
		fixture.subject,
		RunFormalAgentReviewCommand{ReviewRunID: "run-1", StageID: "agent-review"},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Success == nil || outcome.Terminal != nil ||
		fixture.ingress.successCalls != 1 || fixture.ingress.terminalCalls != 0 {
		t.Fatalf("success route = %+v ingress=%+v", outcome, fixture.ingress)
	}
	if !reflect.DeepEqual(fixture.executor.handle, fixture.expectedHandle) {
		t.Fatalf("AwaitResult handle = %+v, want %+v", fixture.executor.handle, fixture.expectedHandle)
	}
	if string(fixture.ingress.callbackProof) != "durable-proof" {
		t.Fatalf("callback proof = %q", fixture.ingress.callbackProof)
	}
}

func TestFormalAgentReviewOrchestratorRoutesFailedAndCanceledWithoutHypotheses(t *testing.T) {
	for _, status := range []contractsv1alpha1.StageExecutionStatus{
		contractsv1alpha1.StageExecutionFailed,
		contractsv1alpha1.StageExecutionCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			fixture := formalOrchestrationFixture(status)
			orchestrator, err := NewFormalAgentReviewOrchestrator(
				fixture.dispatcher, fixture.executor, fixture.ingress,
			)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := orchestrator.Run(
				t.Context(), fixture.subject,
				RunFormalAgentReviewCommand{ReviewRunID: "run-1", StageID: "agent-review"},
			)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if outcome.Success != nil || outcome.Terminal == nil ||
				fixture.ingress.successCalls != 0 || fixture.ingress.terminalCalls != 1 {
				t.Fatalf("terminal route = %+v ingress=%+v", outcome, fixture.ingress)
			}
		})
	}
}

func TestFormalAgentReviewOrchestratorPreservesDispatchForUnknownCompletion(t *testing.T) {
	fixture := formalOrchestrationFixture(contractsv1alpha1.StageExecutionSucceeded)
	fixture.executor.err = errors.New("completion unknown")
	orchestrator, err := NewFormalAgentReviewOrchestrator(
		fixture.dispatcher, fixture.executor, fixture.ingress,
	)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := orchestrator.Run(
		t.Context(), fixture.subject,
		RunFormalAgentReviewCommand{ReviewRunID: "run-1", StageID: "agent-review"},
	)
	if err == nil || outcome.Dispatch.Binding.ProviderHandle == "" ||
		fixture.ingress.successCalls != 0 || fixture.ingress.terminalCalls != 0 ||
		len(fixture.dispatcher.commands) != 1 || fixture.dispatcher.commands[0].Attempt != 1 {
		t.Fatalf("unknown completion outcome=%+v error=%v ingress=%+v", outcome, err, fixture.ingress)
	}
}

func TestFormalAgentReviewOrchestratorRetriesOnlyPolicyAuthorizedFailure(t *testing.T) {
	fixture := formalOrchestrationFixture(contractsv1alpha1.StageExecutionSucceeded)
	first := fixture.dispatcher.result
	first.Plan.Retry = contractsv1alpha1.AgentStageRetryPolicy{
		MaxAttempts: 2, BackoffMS: 0,
		RetryableCodes: []string{"provider_error"}, UnknownOutcome: "reconcile",
	}
	second := first
	second.Request.ExecutionID = "execution-2"
	second.Request.Attempt = 2
	second.Request.Generation = 3
	second.Request.IdempotencyKey = "ensure-2"
	second.Binding.ProviderHandle = "pi-local/execution-2"
	fixture.dispatcher.results = []DispatchedAgentStage{first, second}
	fixture.executor.results = []contractsv1alpha1.StageExecutionResult{
		{Status: contractsv1alpha1.StageExecutionFailed, Failure: &contractsv1alpha1.StageExecutionFailure{
			Code: "provider_error", Message: "temporary", Retryable: true,
		}},
		{Status: contractsv1alpha1.StageExecutionSucceeded},
	}
	orchestrator, err := NewFormalAgentReviewOrchestrator(
		fixture.dispatcher, fixture.executor, fixture.ingress,
	)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := orchestrator.Run(
		t.Context(), fixture.subject,
		RunFormalAgentReviewCommand{ReviewRunID: "run-1", StageID: "agent-review"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Success == nil || outcome.Dispatch.Request.Attempt != 2 ||
		fixture.ingress.terminalCalls != 1 || fixture.ingress.successCalls != 1 ||
		len(fixture.dispatcher.commands) != 2 ||
		fixture.dispatcher.commands[0].Attempt != 1 || fixture.dispatcher.commands[1].Attempt != 2 {
		t.Fatalf("retry outcome=%+v commands=%+v ingress=%+v", outcome, fixture.dispatcher.commands, fixture.ingress)
	}
}

type formalOrchestrationTestFixture struct {
	subject        AgentPlanningSubject
	dispatcher     *formalDispatcherStub
	executor       *formalExecutorStub
	ingress        *formalIngressStub
	expectedHandle execution.Handle
}

func formalOrchestrationFixture(
	status contractsv1alpha1.StageExecutionStatus,
) formalOrchestrationTestFixture {
	request := contractsv1alpha1.StageExecutionRequest{
		ExecutionID: "execution-1", Attempt: 1, Generation: 2, FencingToken: 3,
		IdempotencyKey: "ensure-1", RequestSHA256: formalTestDigest("request"),
		Capability: contractsv1alpha1.ExecutorCapabilitySnapshot{
			SHA256: formalTestDigest("capability"),
		},
	}
	dispatched := DispatchedAgentStage{
		Request: request,
		Binding: runmodel.AgentStageExecutionBinding{
			ProviderHandle: "pi-local/execution-1",
		},
	}
	result := contractsv1alpha1.StageExecutionResult{Status: status}
	return formalOrchestrationTestFixture{
		subject: AgentPlanningSubject{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			WorkspaceID: "workspace-1", RepositoryID: "repository-1",
		},
		dispatcher: &formalDispatcherStub{result: dispatched},
		executor: &formalExecutorStub{
			result: result, proof: []byte("durable-proof"),
		},
		ingress: &formalIngressStub{},
		expectedHandle: execution.Handle{
			ProviderHandle: "pi-local/execution-1", ExecutionID: "execution-1",
			Attempt: 1, Generation: 2, FencingToken: 3,
			IdempotencyKey: "ensure-1", RequestSHA256: formalTestDigest("request"),
			CapabilitySHA256: formalTestDigest("capability"),
		},
	}
}

func formalTestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type formalDispatcherStub struct {
	result   DispatchedAgentStage
	results  []DispatchedAgentStage
	commands []DispatchAgentStageCommand
	err      error
}

func (stub *formalDispatcherStub) DispatchGovernedAgentStage(
	_ context.Context,
	_ AgentPlanningSubject,
	command DispatchAgentStageCommand,
) (DispatchedAgentStage, error) {
	stub.commands = append(stub.commands, command)
	if len(stub.results) > 0 {
		result := stub.results[0]
		stub.results = stub.results[1:]
		return result, stub.err
	}
	return stub.result, stub.err
}

type formalExecutorStub struct {
	handle  execution.Handle
	result  contractsv1alpha1.StageExecutionResult
	results []contractsv1alpha1.StageExecutionResult
	proof   []byte
	err     error
}

func (stub *formalExecutorStub) AwaitResult(
	_ context.Context,
	handle execution.Handle,
) (contractsv1alpha1.StageExecutionResult, []byte, error) {
	stub.handle = handle
	if len(stub.results) > 0 {
		result := stub.results[0]
		stub.results = stub.results[1:]
		return result, append([]byte(nil), stub.proof...), stub.err
	}
	return stub.result, append([]byte(nil), stub.proof...), stub.err
}

type formalIngressStub struct {
	successCalls  int
	terminalCalls int
	callbackProof []byte
}

func (stub *formalIngressStub) AdmitGovernedAgentStageResult(
	_ context.Context,
	_ AgentPlanningSubject,
	command AdmitAgentStageResultCommand,
) (AdmittedAgentStageResult, error) {
	stub.successCalls++
	stub.callbackProof = append([]byte(nil), command.CallbackProof...)
	return AdmittedAgentStageResult{}, nil
}

func (stub *formalIngressStub) AdmitGovernedAgentStageTerminalOutcome(
	_ context.Context,
	_ AgentPlanningSubject,
	command AdmitAgentStageResultCommand,
) (AdmittedAgentStageTerminalOutcome, error) {
	stub.terminalCalls++
	stub.callbackProof = append([]byte(nil), command.CallbackProof...)
	return AdmittedAgentStageTerminalOutcome{}, nil
}
