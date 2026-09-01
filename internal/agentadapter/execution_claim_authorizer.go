package agentadapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/execution"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

var ErrExecutionClaimAuthorityDenied = errors.New("formal execution claim authority denied")

// executionAuthorityRepository is the minimum durable ledger surface needed
// to independently authorize provider Ensure and Cancel calls. Repository
// implements it without exposing its underlying local store.
type executionAuthorityRepository interface {
	LookupAgentStageDispatchIntent(
		context.Context,
		string,
		string,
		int,
		int,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageDispatchIntent, bool, error)
	IsAgentStageDispatchIntentClaimed(
		context.Context,
		runmodel.AgentStageDispatchIntent,
	) (bool, error)
	LookupAgentStageExecutionBinding(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageExecutionBinding, bool, error)
	LookupAgentStageTerminalGate(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageTerminalGate, bool, error)
}

// ExecutionClaimAuthorizer is deliberately bound to one authenticated full
// Argus subject. StageExecutionRequest only carries tenant/workspace process
// coordinates; organization/repository isolation is supplied by this binding,
// never guessed by scanning ledgers or parsing IDs.
type ExecutionClaimAuthorizer struct {
	repository executionAuthorityRepository
	subject    runmodel.AgentPlanningSubject
}

var _ execution.DurableClaimAuthorizer = (*ExecutionClaimAuthorizer)(nil)

func NewExecutionClaimAuthorizer(
	repository executionAuthorityRepository,
	subject application.AgentPlanningSubject,
) (*ExecutionClaimAuthorizer, error) {
	if repository == nil {
		return nil, fmt.Errorf("execution authority repository is required")
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	if err := ledgerSubject.Validate(); err != nil {
		return nil, fmt.Errorf("validate execution authority subject: %w", err)
	}
	return &ExecutionClaimAuthorizer{repository: repository, subject: ledgerSubject}, nil
}

func (authorizer *ExecutionClaimAuthorizer) AuthorizeClaimedEnsure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if authorizer == nil || authorizer.repository == nil {
		return fmt.Errorf("%w: authorizer is not initialized", ErrExecutionClaimAuthorityDenied)
	}
	if err := request.Validate(); err != nil {
		return fmt.Errorf("%w: invalid exact request: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	if request.TenantID != authorizer.subject.TenantID ||
		request.WorkspaceID != authorizer.subject.WorkspaceID {
		return fmt.Errorf("%w: request escaped authenticated subject", ErrExecutionClaimAuthorityDenied)
	}
	intent, found, err := authorizer.repository.LookupAgentStageDispatchIntent(
		ctx,
		request.ReviewRunID,
		request.Stage.ID,
		request.Attempt,
		request.Generation,
		authorizer.subject,
	)
	if err != nil {
		return fmt.Errorf("%w: lookup exact dispatch intent: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	if !found {
		return fmt.Errorf("%w: exact dispatch intent is absent", ErrExecutionClaimAuthorityDenied)
	}
	if err := validateEnsureRequestAgainstIntent(request, intent); err != nil {
		return fmt.Errorf("%w: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	claimed, err := authorizer.repository.IsAgentStageDispatchIntentClaimed(ctx, intent)
	if err != nil {
		return fmt.Errorf("%w: inspect durable dispatch claim: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	if !claimed {
		return fmt.Errorf("%w: exact dispatch intent is not claimed", ErrExecutionClaimAuthorityDenied)
	}
	if _, terminal, err := authorizer.lookupTerminal(ctx, intent); err != nil {
		return err
	} else if terminal {
		return fmt.Errorf("%w: exact dispatch intent is already terminal", ErrExecutionClaimAuthorityDenied)
	}
	return checkContext(ctx)
}

func (authorizer *ExecutionClaimAuthorizer) AuthorizeTerminalCancel(
	ctx context.Context,
	request execution.CancelRequest,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if authorizer == nil || authorizer.repository == nil {
		return fmt.Errorf("%w: authorizer is not initialized", ErrExecutionClaimAuthorityDenied)
	}
	authority := request.Authority
	if authority.TenantID != authorizer.subject.TenantID ||
		authority.WorkspaceID != authorizer.subject.WorkspaceID {
		return fmt.Errorf("%w: cancel escaped authenticated subject", ErrExecutionClaimAuthorityDenied)
	}
	intent, found, err := authorizer.repository.LookupAgentStageDispatchIntent(
		ctx,
		authority.ReviewRunID,
		authority.StageID,
		request.Attempt,
		request.Generation,
		authorizer.subject,
	)
	if err != nil {
		return fmt.Errorf("%w: lookup cancel dispatch intent: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	if !found {
		return fmt.Errorf("%w: cancel dispatch intent is absent", ErrExecutionClaimAuthorityDenied)
	}
	if err := validateCancelRequestAgainstIntent(request, intent); err != nil {
		return fmt.Errorf("%w: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	claimed, err := authorizer.repository.IsAgentStageDispatchIntentClaimed(ctx, intent)
	if err != nil || !claimed {
		return fmt.Errorf("%w: cancel target is not durably claimed: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	binding, bindingFound, err := authorizer.repository.LookupAgentStageExecutionBinding(
		ctx,
		intent.ReviewRunID,
		intent.IntentID,
		authorizer.subject,
	)
	if err != nil || !bindingFound {
		return fmt.Errorf("%w: cancel execution binding is absent: %v", ErrExecutionClaimAuthorityDenied, err)
	}
	if err := binding.ValidateAgainstIntent(intent); err != nil ||
		binding.ProviderHandle != request.ProviderHandle ||
		binding.ExecutionID != request.ExecutionID || binding.Attempt != request.Attempt ||
		binding.Generation != request.Generation || binding.FencingToken != request.FencingToken ||
		binding.CreateIdempotencyKey != request.CreateIdempotencyKey ||
		binding.RequestSemanticSHA256 != request.RequestSHA256 ||
		binding.CapabilitySHA256 != request.CapabilitySHA256 {
		return fmt.Errorf("%w: cancel target differs from durable execution binding", ErrExecutionClaimAuthorityDenied)
	}
	gate, terminal, err := authorizer.lookupTerminal(ctx, intent)
	if err != nil {
		return err
	}
	if !terminal || gate.Kind != runmodel.AgentStageCancellationRequested ||
		gate.Cancellation == nil || gate.GateID != authority.TerminalGateID ||
		gate.SHA256 != authority.TerminalGateSHA256 ||
		gate.Cancellation.CancelIdempotencyKey != authority.CancelIdempotencyKey ||
		gate.Cancellation.CancelIdempotencyKey != request.CancelIdempotencyKey {
		return fmt.Errorf("%w: exact terminal cancellation gate is absent", ErrExecutionClaimAuthorityDenied)
	}
	return checkContext(ctx)
}

func (authorizer *ExecutionClaimAuthorizer) lookupTerminal(
	ctx context.Context,
	intent runmodel.AgentStageDispatchIntent,
) (runmodel.AgentStageTerminalGate, bool, error) {
	gate, found, err := authorizer.repository.LookupAgentStageTerminalGate(
		ctx,
		intent.ReviewRunID,
		intent.IntentID,
		authorizer.subject,
	)
	if err != nil {
		return runmodel.AgentStageTerminalGate{}, false, fmt.Errorf(
			"%w: lookup exact terminal gate: %v",
			ErrExecutionClaimAuthorityDenied,
			err,
		)
	}
	return gate, found, nil
}

func validateEnsureRequestAgainstIntent(
	request contractsv1alpha1.StageExecutionRequest,
	intent runmodel.AgentStageDispatchIntent,
) error {
	if request.ReviewRunID != intent.ReviewRunID ||
		request.Stage.ID != intent.Stage.ID || request.Stage.Revision != intent.Stage.Revision ||
		request.Stage.SHA256 != intent.Stage.SHA256 || request.WorkloadID != intent.WorkloadID ||
		request.LeaseID != intent.LeaseID || request.LeaseWorkerID != intent.LeaseWorker ||
		request.ExecutionID != intent.ExecutionID || request.Attempt != intent.Attempt ||
		request.Generation != intent.Generation || request.FencingToken != intent.FencingToken ||
		request.IdempotencyKey != intent.CreateIdempotencyKey ||
		request.RequestSHA256 != intent.RequestSemanticSHA256 ||
		request.Capability.SHA256 != intent.CapabilitySHA256 {
		return fmt.Errorf("request differs from exact durable dispatch intent")
	}
	return nil
}

func validateCancelRequestAgainstIntent(
	request execution.CancelRequest,
	intent runmodel.AgentStageDispatchIntent,
) error {
	authority := request.Authority
	if authority.ReviewRunID != intent.ReviewRunID || authority.StageID != intent.Stage.ID ||
		authority.StageRevision != intent.Stage.Revision || authority.StageSHA256 != intent.Stage.SHA256 ||
		authority.IntentID != intent.IntentID || authority.IntentSHA256 != intent.SHA256 ||
		request.ExecutionID != intent.ExecutionID || request.Attempt != intent.Attempt ||
		request.Generation != intent.Generation || request.FencingToken != intent.FencingToken ||
		request.CreateIdempotencyKey != intent.CreateIdempotencyKey ||
		request.CancelIdempotencyKey != intent.CancelIdempotencyKey ||
		request.RequestSHA256 != intent.RequestSemanticSHA256 ||
		request.CapabilitySHA256 != intent.CapabilitySHA256 {
		return fmt.Errorf("cancel request differs from exact durable dispatch intent")
	}
	return nil
}
