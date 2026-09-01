package agentadapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/execution"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestExecutionClaimAuthorizerClosesClaimSubjectAndTerminalCancel(t *testing.T) {
	subject := testSubject()
	request, intent, binding, terminal := executionAuthorityFixture(t, subject)
	repository := &executionAuthorityRepositoryStub{
		intent: intent, binding: binding, claimed: true,
	}
	authorizer, err := NewExecutionClaimAuthorizer(repository, subject)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizer.AuthorizeClaimedEnsure(context.Background(), request); err != nil {
		t.Fatalf("AuthorizeClaimedEnsure() error = %v", err)
	}

	repository.terminal = &terminal
	if err := authorizer.AuthorizeClaimedEnsure(
		context.Background(), request,
	); !errors.Is(err, ErrExecutionClaimAuthorityDenied) {
		t.Fatalf("terminal AuthorizeClaimedEnsure() error = %v", err)
	}
	cancel := exactCancelRequest(intent, binding, terminal)
	if err := authorizer.AuthorizeTerminalCancel(context.Background(), cancel); err != nil {
		t.Fatalf("AuthorizeTerminalCancel() error = %v", err)
	}

	changed := cancel
	changed.ProviderHandle = "provider/executions/drifted"
	if err := authorizer.AuthorizeTerminalCancel(
		context.Background(), changed,
	); !errors.Is(err, ErrExecutionClaimAuthorityDenied) {
		t.Fatalf("provider-handle drift error = %v", err)
	}
	changed = cancel
	changed.Authority.TerminalGateSHA256 = strings.Repeat("9", 64)
	if err := authorizer.AuthorizeTerminalCancel(
		context.Background(), changed,
	); !errors.Is(err, ErrExecutionClaimAuthorityDenied) {
		t.Fatalf("terminal-gate drift error = %v", err)
	}

	foreign := subject
	foreign.RepositoryID = "repository-2"
	foreignAuthorizer, err := NewExecutionClaimAuthorizer(repository, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := foreignAuthorizer.AuthorizeClaimedEnsure(
		context.Background(), request,
	); !errors.Is(err, ErrExecutionClaimAuthorityDenied) {
		t.Fatalf("cross-repository Ensure error = %v", err)
	}
}

type executionAuthorityRepositoryStub struct {
	intent   runmodel.AgentStageDispatchIntent
	binding  runmodel.AgentStageExecutionBinding
	terminal *runmodel.AgentStageTerminalGate
	claimed  bool
}

func (repository *executionAuthorityRepositoryStub) LookupAgentStageDispatchIntent(
	_ context.Context,
	runID string,
	stageID string,
	attempt int,
	generation int,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageDispatchIntent, bool, error) {
	intent := repository.intent
	if intent.ReviewRunID != runID || intent.Stage.ID != stageID ||
		intent.Attempt != attempt || intent.Generation != generation || intent.Subject != subject {
		return runmodel.AgentStageDispatchIntent{}, false, nil
	}
	return intent, true, nil
}

func (repository *executionAuthorityRepositoryStub) IsAgentStageDispatchIntentClaimed(
	_ context.Context,
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	if intent != repository.intent {
		return false, errors.New("intent mismatch")
	}
	return repository.claimed, nil
}

func (repository *executionAuthorityRepositoryStub) LookupAgentStageExecutionBinding(
	_ context.Context,
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageExecutionBinding, bool, error) {
	if repository.binding.ReviewRunID != runID || repository.binding.IntentID != intentID ||
		repository.binding.Subject != subject {
		return runmodel.AgentStageExecutionBinding{}, false, nil
	}
	return repository.binding, true, nil
}

func (repository *executionAuthorityRepositoryStub) LookupAgentStageTerminalGate(
	_ context.Context,
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageTerminalGate, bool, error) {
	if repository.terminal == nil {
		return runmodel.AgentStageTerminalGate{}, false, nil
	}
	gate := *repository.terminal
	if gate.ReviewRunID != runID || gate.IntentID != intentID || gate.Subject != subject {
		return runmodel.AgentStageTerminalGate{}, false, nil
	}
	return gate, true, nil
}

func executionAuthorityFixture(
	t *testing.T,
	subject application.AgentPlanningSubject,
) (
	contractsv1alpha1.StageExecutionRequest,
	runmodel.AgentStageDispatchIntent,
	runmodel.AgentStageExecutionBinding,
	runmodel.AgentStageTerminalGate,
) {
	t.Helper()
	digest := strings.Repeat("a", 64)
	artifact := func(contract string) contractsv1alpha1.ArtifactBinding {
		return contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:    "artifact://argus-local/tenants/tenant-1/workspaces/workspace-1/sha256/" + digest,
				SHA256: digest, SizeBytes: 1,
			},
			Contract: contract,
		}
	}
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind: "pi", RuntimeID: "pi-local", RuntimeRevision: "1",
		RuntimeSHA256: digest, BuildIdentity: "argus-test",
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:    []string{},
			ModelEgress:     contractsv1alpha1.AgentStageModelEgressProviderBrokerOnly,
			ToolNetwork:     contractsv1alpha1.AgentStageSideEffectsDeny,
			WorkspaceReads:  contractsv1alpha1.AgentStageWorkspaceReadFrozenInputOnly,
			WorkspaceWrites: contractsv1alpha1.AgentStageSideEffectsDeny,
			RemoteWrites:    contractsv1alpha1.AgentStageSideEffectsDeny,
		},
		Trust: contractsv1alpha1.ExecutorTrust{
			Authority: contractsv1alpha1.ExecutorTrustAuthorityLocalHost,
			CapabilityVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-capability-verifier", Revision: "1", SHA256: digest,
			},
			CallbackVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-callback-verifier", Revision: "1", SHA256: digest,
			},
		},
	}
	var err error
	capability.SHA256, err = contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	request, err := contractsv1alpha1.SealStageExecutionRequest(
		contractsv1alpha1.StageExecutionRequest{
			SchemaVersion: contractsv1alpha1.StageExecutionRequestSchemaVersion,
			ExecutionID:   "execution-1", ReviewRunID: "run-1",
			TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID,
			WorkloadID: "run-1-workload", LeaseID: "run-1-workload-g1",
			LeaseWorkerID: "worker-1",
			Stage: contractsv1alpha1.VersionedRef{
				ID: "agent_hypothesize", Revision: "1", SHA256: digest,
			},
			Attempt: 1, Generation: 1, FencingToken: 1,
			IdempotencyKey:    "create-execution-1",
			Plan:              artifact(contractsv1alpha1.AgentStagePlanSchemaVersion),
			ExecutionSnapshot: artifact(contractsv1alpha1.StageExecutionSnapshotContract),
			ReviewInput:       artifact(contractsv1alpha1.StageExecutionReviewInputContract),
			Upstream:          []contractsv1alpha1.ArtifactBinding{},
			OutputContract:    contractsv1alpha1.AgentStagePlanOutputContract,
			Capability:        capability,
			Deadline:          time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
			SideEffects:       contractsv1alpha1.AgentStageSideEffectsDeny,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	admissionID, err := runmodel.AgentStagePlanAdmissionID(request.ReviewRunID, request.Stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := runmodel.SealAgentStageDispatchIntent(runmodel.AgentStageDispatchIntent{
		SchemaVersion: runmodel.AgentStageDispatchIntentSchemaVersion,
		Subject:       ledgerSubject, ReviewRunID: request.ReviewRunID,
		Stage:      runmodel.AgentStageRef{ID: request.Stage.ID, Revision: request.Stage.Revision, SHA256: request.Stage.SHA256},
		WorkloadID: request.WorkloadID, LeaseID: request.LeaseID, LeaseWorker: request.LeaseWorkerID,
		AdmissionID: admissionID, AdmissionSHA256: digest,
		PlanID: "agent-stage-plan-" + digest[:24], PlanSemanticSHA256: digest,
		RequestRef: runmodel.ArtifactRef{
			URI: "artifact://local/sha256/" + digest, SHA256: digest, SizeBytes: 1,
			Contract: runmodel.ContractStageExecutionRequest,
		},
		RequestSemanticSHA256: request.RequestSHA256,
		CapabilitySHA256:      request.Capability.SHA256,
		ExecutionID:           request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		CreateIdempotencyKey: request.IdempotencyKey,
		CancelIdempotencyKey: "cancel-execution-1",
		RecordedAt:           time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runmodel.SealAgentStageExecutionBinding(runmodel.AgentStageExecutionBinding{
		SchemaVersion: runmodel.AgentStageExecutionBindingSchemaVersion,
		Subject:       ledgerSubject, ReviewRunID: intent.ReviewRunID, Stage: intent.Stage,
		WorkloadID: intent.WorkloadID, LeaseID: intent.LeaseID, LeaseWorker: intent.LeaseWorker,
		IntentID: intent.IntentID, IntentSHA256: intent.SHA256,
		RequestRef: intent.RequestRef, RequestSemanticSHA256: intent.RequestSemanticSHA256,
		ExecutionID: intent.ExecutionID, Attempt: intent.Attempt, Generation: intent.Generation,
		FencingToken: intent.FencingToken, CreateIdempotencyKey: intent.CreateIdempotencyKey,
		ProviderHandle: "provider/executions/opaque-1", CapabilitySHA256: intent.CapabilitySHA256,
		RecordedAt: intent.RecordedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := runmodel.SealAgentStageTerminalGate(runmodel.AgentStageTerminalGate{
		SchemaVersion: runmodel.AgentStageTerminalGateSchemaVersion,
		Kind:          runmodel.AgentStageCancellationRequested,
		Subject:       ledgerSubject, ReviewRunID: intent.ReviewRunID, Stage: intent.Stage,
		AdmissionID: intent.AdmissionID, AdmissionSHA256: intent.AdmissionSHA256,
		IntentID: intent.IntentID, IntentSHA256: intent.SHA256,
		WorkloadID: intent.WorkloadID, LeaseID: intent.LeaseID, LeaseWorker: intent.LeaseWorker,
		ExecutionID: intent.ExecutionID, Attempt: intent.Attempt, Generation: intent.Generation,
		FencingToken: intent.FencingToken, RequestRef: intent.RequestRef,
		RequestSemanticSHA256: intent.RequestSemanticSHA256,
		RequestDeadline:       request.Deadline, CapabilitySHA256: intent.CapabilitySHA256,
		Cancellation: &runmodel.AgentStageCancellationRequest{
			CancelIdempotencyKey: intent.CancelIdempotencyKey,
			Actor:                "worker-1", Reason: "scheduling lease superseded",
			RequestedAt: binding.RecordedAt.Add(time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return request, intent, binding, gate
}

func exactCancelRequest(
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	gate runmodel.AgentStageTerminalGate,
) execution.CancelRequest {
	return execution.CancelRequest{
		Authority: execution.TerminalCancelAuthority{
			TenantID: intent.Subject.TenantID, WorkspaceID: intent.Subject.WorkspaceID,
			ReviewRunID: intent.ReviewRunID,
			StageID:     intent.Stage.ID, StageRevision: intent.Stage.Revision, StageSHA256: intent.Stage.SHA256,
			IntentID: intent.IntentID, IntentSHA256: intent.SHA256,
			TerminalGateID: gate.GateID, TerminalGateSHA256: gate.SHA256,
			CancelIdempotencyKey: intent.CancelIdempotencyKey,
		},
		ProviderHandle: binding.ProviderHandle, ExecutionID: binding.ExecutionID,
		Attempt: binding.Attempt, Generation: binding.Generation, FencingToken: binding.FencingToken,
		CreateIdempotencyKey: binding.CreateIdempotencyKey,
		CancelIdempotencyKey: intent.CancelIdempotencyKey,
		RequestSHA256:        binding.RequestSemanticSHA256, CapabilitySHA256: binding.CapabilitySHA256,
	}
}
