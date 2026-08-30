package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/abietic/argus/internal/execution"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

var ErrAgentStageDispatchOutcomeUnknown = errors.New(
	"formal agent-stage dispatch outcome is unknown; exact Ensure may be retried",
)

var ErrAgentStageProviderContractViolation = errors.New(
	"formal agent execution provider violated exact idempotency",
)

var ErrAgentStageDispatchCanceled = errors.New(
	"formal agent-stage dispatch is canceled by its terminal winner",
)

// GovernedAgentStagePreparer is the S2a admission boundary consumed by the
// dispatcher. Every dispatch, including recovery, revalidates the admitted
// plan against its frozen closure before any provider operation.
type GovernedAgentStagePreparer interface {
	PrepareGovernedAgentStage(
		context.Context,
		AgentPlanningSubject,
		PrepareAgentStageCommand,
	) (PreparedAgentStage, error)
}

// AgentExecutorCapabilityResolver is a trusted runtime-selection port. The
// returned capability is still compared field-by-field with the admitted plan;
// a runtime selector cannot widen tool, model, filesystem, network, or
// delegation authority.
type AgentExecutorCapabilityResolver interface {
	ResolveAgentExecutorCapability(
		context.Context,
		AgentPlanningSubject,
		contractsv1alpha1.AgentStagePlan,
	) (contractsv1alpha1.ExecutorCapabilitySnapshot, error)
}

// AgentStageExecutionGateway is the provider-neutral execution boundary.
// EnsurePrepared is available only after preflight and a durable claim;
// claimed recovery first mints an opaque proof and then retries the exact
// persisted request under the provider's atomic idempotency contract without
// re-resolving governed input.
type AgentStageExecutionGateway interface {
	Preflight(
		context.Context,
		contractsv1alpha1.StageExecutionRequest,
	) (execution.PreparedEnsure, error)
	EnsurePrepared(
		context.Context,
		execution.PreparedEnsure,
	) (execution.Handle, error)
	PrepareClaimedEnsure(
		context.Context,
		contractsv1alpha1.StageExecutionRequest,
	) (execution.PreparedClaimedEnsure, error)
	EnsureClaimed(context.Context, execution.PreparedClaimedEnsure) (execution.Handle, error)
	Lookup(
		context.Context,
		contractsv1alpha1.StageExecutionRequest,
	) (execution.Handle, bool, error)
}

// AgentStageDispatchRepository owns the write-ahead dispatch authority and the
// exact provider acknowledgement. Claim must atomically return true to at most
// one caller for an intent; after a claim, recovery may only retry the exact
// provider Ensure primitive, which atomically creates or returns one handle.
type AgentStageDispatchRepository interface {
	PutLocalArtifact(context.Context, string, []byte) (runmodel.ArtifactRef, error)
	ReadLocalArtifact(context.Context, runmodel.ArtifactRef) ([]byte, error)
	LookupAgentStageDispatchIntent(
		context.Context,
		string,
		string,
		int,
		int,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageDispatchIntent, bool, error)
	ListClaimedAgentStageDispatchIntents(
		context.Context,
		string,
		runmodel.AgentPlanningSubject,
	) ([]runmodel.AgentStageDispatchIntent, error)
	ClaimAgentStageDispatchIntent(
		context.Context,
		runmodel.AgentStageDispatchIntent,
	) (bool, error)
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
	AppendAgentStageExecutionBinding(
		context.Context,
		runmodel.AgentStageExecutionBinding,
	) error
	LookupAgentStageTerminalGate(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageTerminalGate, bool, error)
	AppendAgentStageTerminalGate(context.Context, runmodel.AgentStageTerminalGate) error
}

type DispatchAgentStageCommand struct {
	ReviewRunID string
	StageID     string
	Attempt     int
}

type DispatchedAgentStage struct {
	Plan      contractsv1alpha1.AgentStagePlan
	Admission runmodel.AgentStagePlanAdmission
	Request   contractsv1alpha1.StageExecutionRequest
	Intent    runmodel.AgentStageDispatchIntent
	Binding   runmodel.AgentStageExecutionBinding
	Recovered bool
}

type AgentStageDispatcher struct {
	preparer     GovernedAgentStagePreparer
	authorities  AgentStageExecutionAuthorityResolver
	capabilities AgentExecutorCapabilityResolver
	gateway      AgentStageExecutionGateway
	repository   AgentStageDispatchRepository
	now          func() time.Time
}

func NewAgentStageDispatcher(
	preparer GovernedAgentStagePreparer,
	authorities AgentStageExecutionAuthorityResolver,
	capabilities AgentExecutorCapabilityResolver,
	gateway AgentStageExecutionGateway,
	repository AgentStageDispatchRepository,
	now func() time.Time,
) (*AgentStageDispatcher, error) {
	if preparer == nil || authorities == nil || capabilities == nil || gateway == nil ||
		repository == nil {
		return nil, fmt.Errorf(
			"agent stage preparer, execution authority, capability resolver, execution gateway, and dispatch repository are required",
		)
	}
	if now == nil {
		now = time.Now
	}
	return &AgentStageDispatcher{
		preparer: preparer, authorities: authorities, capabilities: capabilities, gateway: gateway,
		repository: repository, now: now,
	}, nil
}

func (dispatcher *AgentStageDispatcher) DispatchGovernedAgentStage(
	ctx context.Context,
	subject AgentPlanningSubject,
	command DispatchAgentStageCommand,
) (DispatchedAgentStage, error) {
	if ctx == nil {
		return DispatchedAgentStage{}, fmt.Errorf("context is required")
	}
	if dispatcher == nil || dispatcher.preparer == nil || dispatcher.authorities == nil ||
		dispatcher.capabilities == nil ||
		dispatcher.gateway == nil || dispatcher.repository == nil || dispatcher.now == nil {
		return DispatchedAgentStage{}, fmt.Errorf("agent stage dispatcher is not initialized")
	}
	if err := subject.validate(); err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("validate formal agent dispatch subject: %w", err)
	}
	ledgerSubject := planningLedgerSubject(subject)
	if err := ledgerSubject.Validate(); err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"validate artifact-compatible formal agent dispatch subject: %w",
			err,
		)
	}
	if err := validateLocalIdentity("review_run_id", command.ReviewRunID); err != nil {
		return DispatchedAgentStage{}, err
	}
	if command.StageID == "" {
		return DispatchedAgentStage{}, fmt.Errorf("stage_id is required")
	}
	if command.Attempt == 0 {
		command.Attempt = 1
	}
	if command.Attempt < 1 || uint64(command.Attempt) > maxAgentJSONSafeInteger {
		return DispatchedAgentStage{}, fmt.Errorf("attempt must be a JSON-safe positive integer")
	}
	authorityObservedAt := dispatcher.now().UTC()
	if authorityObservedAt.IsZero() {
		return DispatchedAgentStage{}, fmt.Errorf("agent dispatch clock returned zero time")
	}
	authority, err := dispatcher.authorities.CurrentAgentStageExecutionAuthority(
		ctx,
		subject,
		command.ReviewRunID,
		command.StageID,
	)
	if err != nil {
		if handled, recoveryErr := dispatcher.cancelExpiredClaimedAgentStageForCoordinate(
			ctx, ledgerSubject, command, authorityObservedAt,
		); handled || recoveryErr != nil {
			return DispatchedAgentStage{}, recoveryErr
		}
		return DispatchedAgentStage{}, fmt.Errorf(
			"resolve current formal agent execution authority: %w",
			err,
		)
	}
	authority, err = bindAgentStageAttempt(authority, command.Attempt)
	if err != nil {
		return DispatchedAgentStage{}, err
	}
	if err := authority.validate(authorityObservedAt); err != nil {
		if handled, recoveryErr := dispatcher.cancelExpiredClaimedAgentStageForCoordinate(
			ctx, ledgerSubject, command, authorityObservedAt,
		); handled || recoveryErr != nil {
			return DispatchedAgentStage{}, recoveryErr
		}
		return DispatchedAgentStage{}, fmt.Errorf(
			"validate current formal agent execution authority: %w",
			err,
		)
	}

	prepared, err := dispatcher.preparer.PrepareGovernedAgentStage(
		ctx,
		subject,
		PrepareAgentStageCommand{
			ReviewRunID: command.ReviewRunID,
			StageID:     command.StageID,
		},
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("prepare formal agent stage: %w", err)
	}
	if prepared.Admission.Subject != ledgerSubject ||
		prepared.Admission.ReviewRunID != command.ReviewRunID ||
		prepared.Admission.Stage.ID != command.StageID {
		return DispatchedAgentStage{}, fmt.Errorf(
			"prepared formal agent stage does not match authenticated dispatch coordinate",
		)
	}
	authority, err = dispatcher.revalidateAgentStageExecutionAuthority(
		ctx,
		subject,
		command.ReviewRunID,
		command.StageID,
		authority,
	)
	if err != nil {
		return DispatchedAgentStage{}, err
	}

	intent, found, err := dispatcher.repository.LookupAgentStageDispatchIntent(
		ctx,
		command.ReviewRunID,
		command.StageID,
		authority.Attempt,
		authority.Generation,
		ledgerSubject,
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("lookup formal agent dispatch intent: %w", err)
	}
	if found {
		if err := validateIntentExecutionAuthority(intent, authority); err != nil {
			return DispatchedAgentStage{}, err
		}
		return dispatcher.recoverAgentStageDispatch(ctx, subject, prepared, authority, intent)
	}

	capability, err := dispatcher.capabilities.ResolveAgentExecutorCapability(
		ctx,
		subject,
		prepared.Plan,
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("resolve formal agent executor capability: %w", err)
	}
	if err := validateExecutorCapabilityAgainstPlan(capability, prepared.Plan); err != nil {
		return DispatchedAgentStage{}, err
	}
	recordedAt := dispatcher.now().UTC()
	if recordedAt.IsZero() || recordedAt.Before(prepared.Admission.RecordedAt) {
		return DispatchedAgentStage{}, fmt.Errorf(
			"agent dispatch time must not precede the admitted plan",
		)
	}
	if err := authority.validate(recordedAt); err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"validate refreshed formal agent execution authority: %w",
			err,
		)
	}
	request, cancelKey, err := buildAgentStageExecutionRequest(
		prepared,
		subject,
		capability,
		authority,
		recordedAt,
	)
	if err != nil {
		return DispatchedAgentStage{}, err
	}
	providerEnsure, preparedRequest, err := dispatcher.preflightExactAgentStageEnsure(
		ctx,
		request,
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"preflight formal agent provider Ensure before durable claim: %w",
			err,
		)
	}
	// Persist the detached request carried by the opaque preflight proof. The
	// durable intent and the post-claim provider Ensure therefore bind the same
	// exact value even if a gateway implementation attempts to retain or mutate
	// caller-owned slices.
	request = preparedRequest
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("marshal formal StageExecutionRequest: %w", err)
	}
	requestRef, err := dispatcher.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractStageExecutionRequest,
		requestBytes,
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"persist formal StageExecutionRequest artifact: %w",
			err,
		)
	}
	if requestRef != localRefForBytes(runmodel.ContractStageExecutionRequest, requestBytes) {
		return DispatchedAgentStage{}, fmt.Errorf(
			"StageExecutionRequest artifact repository returned a mismatched ref",
		)
	}
	intent, err = runmodel.SealAgentStageDispatchIntent(
		runmodel.AgentStageDispatchIntent{
			Subject:               ledgerSubject,
			ReviewRunID:           prepared.Plan.ReviewRunID,
			Stage:                 admissionStage(prepared.Plan.Stage),
			WorkloadID:            authority.WorkloadID,
			LeaseID:               authority.LeaseID,
			LeaseWorker:           authority.LeaseWorker,
			AdmissionID:           prepared.Admission.AdmissionID,
			AdmissionSHA256:       prepared.Admission.SHA256,
			PlanID:                prepared.Plan.PlanID,
			PlanSemanticSHA256:    prepared.Plan.SHA256,
			RequestRef:            requestRef,
			RequestSemanticSHA256: request.RequestSHA256,
			CapabilitySHA256:      capability.SHA256,
			ExecutionID:           request.ExecutionID,
			Attempt:               request.Attempt,
			Generation:            request.Generation,
			FencingToken:          request.FencingToken,
			CreateIdempotencyKey:  request.IdempotencyKey,
			CancelIdempotencyKey:  cancelKey,
			RecordedAt:            recordedAt,
		},
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("seal formal agent dispatch intent: %w", err)
	}
	currentAuthority, err := dispatcher.revalidateAgentStageExecutionAuthority(
		ctx,
		subject,
		prepared.Plan.ReviewRunID,
		prepared.Plan.Stage.ID,
		authority,
	)
	if err != nil {
		return DispatchedAgentStage{}, err
	}
	if err := dispatcher.validateAgentStageCreateWindow(request, currentAuthority); err != nil {
		return DispatchedAgentStage{}, err
	}
	claimed, claimErr := dispatcher.repository.ClaimAgentStageDispatchIntent(ctx, intent)
	if claimErr != nil {
		return dispatcher.handleAgentStageClaimFailure(
			ctx,
			subject,
			prepared,
			authority,
			intent,
			claimErr,
		)
	}
	if !claimed {
		return dispatcher.recoverAgentStageDispatch(ctx, subject, prepared, authority, intent)
	}

	return dispatcher.createClaimedAgentStageAndCommitBinding(
		ctx,
		prepared,
		request,
		intent,
		providerEnsure,
		false,
	)
}

func (dispatcher *AgentStageDispatcher) cancelExpiredClaimedAgentStageForCoordinate(
	ctx context.Context,
	subject runmodel.AgentPlanningSubject,
	command DispatchAgentStageCommand,
	observedAt time.Time,
) (bool, error) {
	intents, err := dispatcher.repository.ListClaimedAgentStageDispatchIntents(
		ctx,
		command.ReviewRunID,
		subject,
	)
	if err != nil {
		return false, fmt.Errorf("list claimed dispatches after authority failure: %w", err)
	}
	for index := len(intents) - 1; index >= 0; index-- {
		intent := intents[index]
		if intent.Stage.ID != command.StageID {
			continue
		}
		requestBytes, readErr := dispatcher.repository.ReadLocalArtifact(ctx, intent.RequestRef)
		if readErr != nil {
			return false, fmt.Errorf("read claimed request after authority failure: %w", readErr)
		}
		if localRefForBytes(runmodel.ContractStageExecutionRequest, requestBytes) != intent.RequestRef {
			return false, fmt.Errorf("claimed request bytes do not match dispatch intent")
		}
		request, decodeErr := contractsv1alpha1.DecodeStageExecutionRequest(requestBytes)
		if decodeErr != nil {
			return false, fmt.Errorf("decode claimed request after authority failure: %w", decodeErr)
		}
		if request.RequestSHA256 != intent.RequestSemanticSHA256 ||
			request.ExecutionID != intent.ExecutionID ||
			request.IdempotencyKey != intent.CreateIdempotencyKey {
			return false, fmt.Errorf("claimed request identity differs from dispatch intent")
		}
		if request.Deadline.After(observedAt) {
			continue
		}
		return true, dispatcher.cancelExpiredClaimedAgentStage(
			ctx,
			intent,
			request,
			observedAt,
		)
	}
	return false, nil
}

func (dispatcher *AgentStageDispatcher) recoverAgentStageDispatch(
	ctx context.Context,
	subject AgentPlanningSubject,
	prepared PreparedAgentStage,
	authority AgentStageExecutionAuthority,
	intent runmodel.AgentStageDispatchIntent,
) (DispatchedAgentStage, error) {
	if intent.Subject != planningLedgerSubject(subject) {
		return DispatchedAgentStage{}, fmt.Errorf(
			"formal agent dispatch intent belongs to another authenticated subject",
		)
	}
	if err := intent.ValidateAgainstAdmission(prepared.Admission); err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"validate formal agent dispatch intent against admission: %w",
			err,
		)
	}
	if err := validateIntentExecutionAuthority(intent, authority); err != nil {
		return DispatchedAgentStage{}, err
	}
	if err := dispatcher.rejectCanceledAgentStageIntent(ctx, intent); err != nil {
		return DispatchedAgentStage{}, err
	}
	requestBytes, err := dispatcher.repository.ReadLocalArtifact(ctx, intent.RequestRef)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("read formal StageExecutionRequest: %w", err)
	}
	if localRefForBytes(runmodel.ContractStageExecutionRequest, requestBytes) != intent.RequestRef {
		return DispatchedAgentStage{}, fmt.Errorf(
			"formal StageExecutionRequest bytes do not match dispatch intent ref",
		)
	}
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(requestBytes)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("decode formal StageExecutionRequest: %w", err)
	}
	if err := validateRequestAgainstIntentAndPlan(request, intent, prepared); err != nil {
		return DispatchedAgentStage{}, err
	}
	binding, found, err := dispatcher.repository.LookupAgentStageExecutionBinding(
		ctx,
		intent.ReviewRunID,
		intent.IntentID,
		intent.Subject,
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf("lookup formal agent execution binding: %w", err)
	}
	if found {
		if err := binding.ValidateAgainstIntent(intent); err != nil {
			return DispatchedAgentStage{}, err
		}
		return DispatchedAgentStage{
			Plan: prepared.Plan, Admission: prepared.Admission, Request: request,
			Intent: intent, Binding: binding, Recovered: true,
		}, nil
	}
	observedAt := dispatcher.now().UTC()
	if observedAt.IsZero() {
		return DispatchedAgentStage{}, fmt.Errorf("agent dispatch recovery clock returned zero time")
	}
	if !request.Deadline.After(observedAt) {
		return DispatchedAgentStage{}, dispatcher.cancelExpiredClaimedAgentStage(
			ctx,
			intent,
			request,
			observedAt,
		)
	}
	currentAuthority, err := dispatcher.revalidateAgentStageExecutionAuthority(
		ctx,
		subject,
		intent.ReviewRunID,
		intent.Stage.ID,
		authority,
	)
	if err != nil {
		return DispatchedAgentStage{}, err
	}
	authority = currentAuthority
	claimed, err := dispatcher.repository.IsAgentStageDispatchIntentClaimed(ctx, intent)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"inspect formal agent dispatch claim: %w",
			err,
		)
	}
	if !claimed {
		providerEnsure, _, preflightErr := dispatcher.preflightExactAgentStageEnsure(
			ctx,
			request,
		)
		if preflightErr != nil {
			return DispatchedAgentStage{}, fmt.Errorf(
				"preflight persisted unclaimed formal agent dispatch: %w",
				preflightErr,
			)
		}
		currentAuthority, refreshErr := dispatcher.revalidateAgentStageExecutionAuthority(
			ctx,
			subject,
			intent.ReviewRunID,
			intent.Stage.ID,
			authority,
		)
		if refreshErr != nil {
			return DispatchedAgentStage{}, refreshErr
		}
		if err := dispatcher.validateAgentStageCreateWindow(request, currentAuthority); err != nil {
			return DispatchedAgentStage{}, err
		}
		won, claimErr := dispatcher.repository.ClaimAgentStageDispatchIntent(ctx, intent)
		if claimErr != nil {
			return dispatcher.handleAgentStageClaimFailure(
				ctx,
				subject,
				prepared,
				authority,
				intent,
				claimErr,
			)
		}
		if won {
			return dispatcher.createClaimedAgentStageAndCommitBinding(
				ctx,
				prepared,
				request,
				intent,
				providerEnsure,
				true,
			)
		}
	}

	// A claimed request is safe to resubmit only because PlatformPort.Ensure is
	// an atomic exact-idempotent create-or-lookup primitive. Recovery does not
	// rerun Preflight and therefore cannot be blocked by later governed-input
	// quarantine or authorization drift.
	providerContext, cancel := context.WithDeadline(
		context.WithoutCancel(ctx),
		request.Deadline,
	)
	defer cancel()
	if err := dispatcher.rejectCanceledAgentStageIntent(providerContext, intent); err != nil {
		return DispatchedAgentStage{}, err
	}
	claimedEnsure, err := dispatcher.gateway.PrepareClaimedEnsure(providerContext, request)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: authorize exact claimed Ensure recovery: %w",
			ErrAgentStageDispatchOutcomeUnknown,
			err,
		)
	}
	// The claim proof can be minted before a concurrent terminal winner. Check
	// the terminal ledger once more immediately before consuming it.
	if err := dispatcher.rejectCanceledAgentStageIntent(providerContext, intent); err != nil {
		return DispatchedAgentStage{}, err
	}
	handle, err := dispatcher.gateway.EnsureClaimed(providerContext, claimedEnsure)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: provider exact Ensure recovery: %w",
			ErrAgentStageDispatchOutcomeUnknown,
			err,
		)
	}
	return dispatcher.commitAgentStageExecutionBinding(
		providerContext,
		prepared,
		request,
		intent,
		handle,
		true,
	)
}

func (dispatcher *AgentStageDispatcher) preflightExactAgentStageEnsure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (
	execution.PreparedEnsure,
	contractsv1alpha1.StageExecutionRequest,
	error,
) {
	prepared, err := dispatcher.gateway.Preflight(ctx, request)
	if err != nil {
		return execution.PreparedEnsure{}, contractsv1alpha1.StageExecutionRequest{}, err
	}
	preparedRequest := prepared.Request()
	if prepared.RequestSHA256() != request.RequestSHA256 ||
		preparedRequest.RequestSHA256 != request.RequestSHA256 ||
		!reflect.DeepEqual(preparedRequest, request) {
		return execution.PreparedEnsure{}, contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"provider Ensure preflight did not preserve the exact formal request",
		)
	}
	return prepared, preparedRequest, nil
}

func (dispatcher *AgentStageDispatcher) createClaimedAgentStageAndCommitBinding(
	callerContext context.Context,
	prepared PreparedAgentStage,
	request contractsv1alpha1.StageExecutionRequest,
	intent runmodel.AgentStageDispatchIntent,
	providerEnsure execution.PreparedEnsure,
	recovered bool,
) (DispatchedAgentStage, error) {
	// Once the durable claim commits, caller transport cancellation must not
	// strand an exact claimed intent when the caller transport disappears.
	// Formal run cancellation is represented by scheduling/terminal facts, not
	// by this ephemeral context. The detached operation remains hard-bounded by
	// the immutable absolute request deadline; process restart can safely retry
	// the provider's exact-idempotent Ensure primitive.
	observedAt := dispatcher.now().UTC()
	if observedAt.IsZero() || !request.Deadline.After(observedAt) {
		if observedAt.IsZero() {
			return DispatchedAgentStage{}, fmt.Errorf("agent dispatch ensure clock returned zero time")
		}
		return DispatchedAgentStage{}, dispatcher.cancelExpiredClaimedAgentStage(
			callerContext,
			intent,
			request,
			observedAt,
		)
	}
	providerContext, cancel := context.WithDeadline(
		context.WithoutCancel(callerContext),
		request.Deadline,
	)
	defer cancel()
	if err := dispatcher.rejectCanceledAgentStageIntent(providerContext, intent); err != nil {
		return DispatchedAgentStage{}, err
	}
	handle, err := dispatcher.gateway.EnsurePrepared(providerContext, providerEnsure)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: provider Ensure after durable claim: %w",
			ErrAgentStageDispatchOutcomeUnknown,
			err,
		)
	}
	return dispatcher.commitAgentStageExecutionBinding(
		providerContext,
		prepared,
		request,
		intent,
		handle,
		recovered,
	)
}

func (dispatcher *AgentStageDispatcher) rejectCanceledAgentStageIntent(
	ctx context.Context,
	intent runmodel.AgentStageDispatchIntent,
) error {
	gate, found, err := dispatcher.repository.LookupAgentStageTerminalGate(
		ctx,
		intent.ReviewRunID,
		intent.IntentID,
		intent.Subject,
	)
	if err != nil {
		return fmt.Errorf("lookup formal agent-stage terminal winner: %w", err)
	}
	if !found {
		return nil
	}
	if gate.Kind == runmodel.AgentStageCancellationRequested {
		return fmt.Errorf(
			"%w: cancellation gate %q",
			ErrAgentStageDispatchCanceled,
			gate.GateID,
		)
	}
	return fmt.Errorf(
		"formal agent-stage terminal completion %q already exists before dispatch recovery",
		gate.GateID,
	)
}

func (dispatcher *AgentStageDispatcher) cancelExpiredClaimedAgentStage(
	ctx context.Context,
	intent runmodel.AgentStageDispatchIntent,
	request contractsv1alpha1.StageExecutionRequest,
	requestedAt time.Time,
) error {
	if requestedAt.IsZero() || requestedAt.Location() != time.UTC ||
		requestedAt.Before(intent.RecordedAt) || request.Deadline.After(requestedAt) {
		return fmt.Errorf("invalid expired claimed agent-stage cancellation observation")
	}
	gate, err := runmodel.SealAgentStageTerminalGate(runmodel.AgentStageTerminalGate{
		SchemaVersion:         runmodel.AgentStageTerminalGateSchemaVersion,
		Kind:                  runmodel.AgentStageCancellationRequested,
		Subject:               intent.Subject,
		ReviewRunID:           intent.ReviewRunID,
		Stage:                 intent.Stage,
		AdmissionID:           intent.AdmissionID,
		AdmissionSHA256:       intent.AdmissionSHA256,
		IntentID:              intent.IntentID,
		IntentSHA256:          intent.SHA256,
		WorkloadID:            intent.WorkloadID,
		LeaseID:               intent.LeaseID,
		LeaseWorker:           intent.LeaseWorker,
		ExecutionID:           intent.ExecutionID,
		Attempt:               intent.Attempt,
		Generation:            intent.Generation,
		FencingToken:          intent.FencingToken,
		RequestRef:            intent.RequestRef,
		RequestSemanticSHA256: intent.RequestSemanticSHA256,
		RequestDeadline:       request.Deadline,
		CapabilitySHA256:      intent.CapabilitySHA256,
		Cancellation: &runmodel.AgentStageCancellationRequest{
			CancelIdempotencyKey: intent.CancelIdempotencyKey,
			Actor:                "argus-agent-dispatcher",
			Reason:               "immutable provider ensure deadline expired before execution binding",
			RequestedAt:          requestedAt,
		},
	})
	if err != nil {
		return fmt.Errorf("seal expired claimed agent-stage cancellation: %w", err)
	}
	operationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := dispatcher.repository.AppendAgentStageTerminalGate(operationContext, gate); err != nil {
		winner, found, lookupErr := dispatcher.repository.LookupAgentStageTerminalGate(
			operationContext,
			intent.ReviewRunID,
			intent.IntentID,
			intent.Subject,
		)
		if lookupErr != nil || !found {
			return fmt.Errorf(
				"%w: persist expired dispatch cancellation: %v; reload winner: %v",
				ErrAgentStageDispatchOutcomeUnknown,
				err,
				lookupErr,
			)
		}
		if winner.Kind != runmodel.AgentStageCancellationRequested {
			return fmt.Errorf(
				"formal agent-stage terminal completion %q won expired dispatch cancellation",
				winner.GateID,
			)
		}
		gate = winner
	}
	return fmt.Errorf(
		"%w: immutable deadline %s; cancellation gate %q",
		ErrAgentStageDispatchCanceled,
		request.Deadline.Format(time.RFC3339Nano),
		gate.GateID,
	)
}

func (dispatcher *AgentStageDispatcher) handleAgentStageClaimFailure(
	ctx context.Context,
	subject AgentPlanningSubject,
	prepared PreparedAgentStage,
	authority AgentStageExecutionAuthority,
	intent runmodel.AgentStageDispatchIntent,
	claimErr error,
) (DispatchedAgentStage, error) {
	winner, found, lookupErr := dispatcher.repository.LookupAgentStageDispatchIntent(
		ctx,
		intent.ReviewRunID,
		intent.Stage.ID,
		intent.Attempt,
		intent.Generation,
		intent.Subject,
	)
	if lookupErr != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: claim formal agent dispatch intent: %v; reload intent: %v",
			ErrAgentStageDispatchOutcomeUnknown,
			claimErr,
			lookupErr,
		)
	}
	if !found {
		return DispatchedAgentStage{}, fmt.Errorf(
			"claim formal agent dispatch intent before provider Ensure: %w",
			claimErr,
		)
	}
	claimed, stateErr := dispatcher.repository.IsAgentStageDispatchIntentClaimed(ctx, winner)
	if stateErr != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: claim formal agent dispatch intent: %v; inspect durable claim: %v",
			ErrAgentStageDispatchOutcomeUnknown,
			claimErr,
			stateErr,
		)
	}
	if !claimed {
		return DispatchedAgentStage{}, fmt.Errorf(
			"claim formal agent dispatch intent before provider Ensure: %w",
			claimErr,
		)
	}
	return dispatcher.recoverAgentStageDispatch(ctx, subject, prepared, authority, winner)
}

func (dispatcher *AgentStageDispatcher) commitAgentStageExecutionBinding(
	ctx context.Context,
	prepared PreparedAgentStage,
	request contractsv1alpha1.StageExecutionRequest,
	intent runmodel.AgentStageDispatchIntent,
	handle execution.Handle,
	recovered bool,
) (DispatchedAgentStage, error) {
	recordedAt := dispatcher.now().UTC()
	if recordedAt.IsZero() {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: execution binding clock returned zero time",
			ErrAgentStageDispatchOutcomeUnknown,
		)
	}
	if !request.Deadline.After(recordedAt) {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: execution binding observation crossed immutable request deadline",
			ErrAgentStageDispatchOutcomeUnknown,
		)
	}
	binding, err := runmodel.SealAgentStageExecutionBinding(
		runmodel.AgentStageExecutionBinding{
			Subject:               intent.Subject,
			ReviewRunID:           intent.ReviewRunID,
			Stage:                 intent.Stage,
			WorkloadID:            intent.WorkloadID,
			LeaseID:               intent.LeaseID,
			LeaseWorker:           intent.LeaseWorker,
			IntentID:              intent.IntentID,
			IntentSHA256:          intent.SHA256,
			RequestRef:            intent.RequestRef,
			RequestSemanticSHA256: request.RequestSHA256,
			ExecutionID:           handle.ExecutionID,
			Attempt:               handle.Attempt,
			Generation:            handle.Generation,
			FencingToken:          handle.FencingToken,
			CreateIdempotencyKey:  handle.IdempotencyKey,
			ProviderHandle:        handle.ProviderHandle,
			CapabilitySHA256:      handle.CapabilitySHA256,
			RecordedAt:            recordedAt,
		},
	)
	if err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: seal execution binding: %v",
			ErrAgentStageDispatchOutcomeUnknown,
			err,
		)
	}
	if err := binding.ValidateAgainstIntent(intent); err != nil {
		return DispatchedAgentStage{}, fmt.Errorf(
			"%w: validate execution binding: %v",
			ErrAgentStageDispatchOutcomeUnknown,
			err,
		)
	}
	if err := dispatcher.repository.AppendAgentStageExecutionBinding(ctx, binding); err != nil {
		winner, found, lookupErr := dispatcher.repository.LookupAgentStageExecutionBinding(
			ctx,
			intent.ReviewRunID,
			intent.IntentID,
			intent.Subject,
		)
		if lookupErr == nil && found && winner.ValidateAgainstIntent(intent) == nil &&
			runmodel.SameAgentStageExecutionDecision(winner, binding) {
			binding = winner
			recovered = true
		} else if lookupErr == nil && found && winner.ValidateAgainstIntent(intent) == nil {
			return DispatchedAgentStage{}, fmt.Errorf(
				"%w: exact Ensure returned provider handle or binding tuple different from durable winner",
				ErrAgentStageProviderContractViolation,
			)
		} else {
			return DispatchedAgentStage{}, fmt.Errorf(
				"%w: persist execution binding: %v",
				ErrAgentStageDispatchOutcomeUnknown,
				err,
			)
		}
	}
	return DispatchedAgentStage{
		Plan: prepared.Plan, Admission: prepared.Admission, Request: request,
		Intent: intent, Binding: binding, Recovered: recovered,
	}, nil
}

func buildAgentStageExecutionRequest(
	prepared PreparedAgentStage,
	subject AgentPlanningSubject,
	capability contractsv1alpha1.ExecutorCapabilitySnapshot,
	authority AgentStageExecutionAuthority,
	recordedAt time.Time,
) (contractsv1alpha1.StageExecutionRequest, string, error) {
	if err := authority.validate(recordedAt); err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, "", err
	}
	identity := agentDispatchIdentity(prepared.Admission, authority)
	timeoutMS := prepared.Plan.Budget.TimeoutMS
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if timeoutMS <= 0 || int64(timeout/time.Millisecond) != timeoutMS {
		return contractsv1alpha1.StageExecutionRequest{}, "", fmt.Errorf(
			"formal agent timeout cannot be represented safely",
		)
	}
	deadline := recordedAt.Add(timeout)
	if !deadline.After(recordedAt) {
		return contractsv1alpha1.StageExecutionRequest{}, "", fmt.Errorf(
			"formal agent deadline overflowed",
		)
	}
	if authority.ExpiresAt.Before(deadline) {
		deadline = authority.ExpiresAt
	}
	request := contractsv1alpha1.StageExecutionRequest{
		SchemaVersion:     contractsv1alpha1.StageExecutionRequestSchemaVersion,
		ExecutionID:       prepared.Plan.ReviewRunID + "-agent-execution-" + identity[:24],
		ReviewRunID:       prepared.Plan.ReviewRunID,
		TenantID:          subject.TenantID,
		WorkspaceID:       subject.WorkspaceID,
		WorkloadID:        authority.WorkloadID,
		LeaseID:           authority.LeaseID,
		LeaseWorkerID:     authority.LeaseWorker,
		Stage:             prepared.Plan.Stage,
		Attempt:           authority.Attempt,
		Generation:        authority.Generation,
		FencingToken:      authority.FencingToken,
		IdempotencyKey:    "agent-create-" + identity,
		Plan:              bindingFromAdmission(prepared.Admission.Plan.Governed),
		ExecutionSnapshot: prepared.Plan.ExecutionSnapshot,
		ReviewInput:       prepared.Plan.ReviewInput,
		Upstream:          []contractsv1alpha1.ArtifactBinding{},
		OutputContract:    prepared.Plan.OutputContract,
		Capability:        capability,
		Deadline:          deadline,
		SideEffects:       prepared.Plan.SideEffects,
	}
	sealed, err := contractsv1alpha1.SealStageExecutionRequest(request)
	if err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, "", fmt.Errorf(
			"seal formal StageExecutionRequest: %w",
			err,
		)
	}
	return sealed, "agent-cancel-" + identity, nil
}

func validateRequestAgainstIntentAndPlan(
	request contractsv1alpha1.StageExecutionRequest,
	intent runmodel.AgentStageDispatchIntent,
	prepared PreparedAgentStage,
) error {
	if request.RequestSHA256 != intent.RequestSemanticSHA256 ||
		request.ExecutionID != intent.ExecutionID ||
		request.WorkloadID != intent.WorkloadID ||
		request.LeaseID != intent.LeaseID ||
		request.LeaseWorkerID != intent.LeaseWorker ||
		request.Attempt != intent.Attempt ||
		request.Generation != intent.Generation ||
		request.FencingToken != intent.FencingToken ||
		request.IdempotencyKey != intent.CreateIdempotencyKey ||
		request.Capability.SHA256 != intent.CapabilitySHA256 ||
		request.ReviewRunID != prepared.Plan.ReviewRunID ||
		request.TenantID != intent.Subject.TenantID ||
		request.WorkspaceID != intent.Subject.WorkspaceID ||
		request.Stage != prepared.Plan.Stage ||
		request.Plan != bindingFromAdmission(prepared.Admission.Plan.Governed) ||
		request.ExecutionSnapshot != prepared.Plan.ExecutionSnapshot ||
		request.ReviewInput != prepared.Plan.ReviewInput ||
		request.OutputContract != prepared.Plan.OutputContract ||
		request.SideEffects != prepared.Plan.SideEffects ||
		len(request.Upstream) != 0 {
		return fmt.Errorf(
			"persisted StageExecutionRequest does not match exact intent and admitted plan",
		)
	}
	if err := validateExecutorCapabilityAgainstPlan(request.Capability, prepared.Plan); err != nil {
		return err
	}
	return nil
}

func validateExecutorCapabilityAgainstPlan(
	capability contractsv1alpha1.ExecutorCapabilitySnapshot,
	plan contractsv1alpha1.AgentStagePlan,
) error {
	if err := capability.Validate(); err != nil {
		return fmt.Errorf("validate formal agent executor capability: %w", err)
	}
	wantAuthority := contractsv1alpha1.ExecutionAuthority{
		AllowedTools:       slices.Clone(plan.ToolAuthority.Tools),
		ModelEgress:        plan.ModelAuthority.ModelEgress,
		ToolNetwork:        plan.ToolAuthority.ToolNetwork,
		WorkspaceReads:     plan.ToolAuthority.WorkspaceReads,
		WorkspaceWrites:    plan.ToolAuthority.WorkspaceWrites,
		RemoteWrites:       plan.ToolAuthority.RemoteWrites,
		MaxDelegationDepth: plan.ToolAuthority.MaxDelegationDepth,
	}
	if capability.RuntimeKind != plan.Runtime.Ref.ID ||
		capability.RuntimeRevision != plan.Runtime.Ref.Revision ||
		capability.RuntimeSHA256 != plan.Runtime.Ref.SHA256 ||
		capability.BuildIdentity != plan.BuildIdentity ||
		!reflect.DeepEqual(capability.Authority, wantAuthority) {
		return fmt.Errorf(
			"executor capability does not exactly implement the admitted runtime and authority",
		)
	}
	return nil
}

func validateIntentExecutionAuthority(
	intent runmodel.AgentStageDispatchIntent,
	authority AgentStageExecutionAuthority,
) error {
	if intent.WorkloadID != authority.WorkloadID ||
		intent.LeaseID != authority.LeaseID ||
		intent.LeaseWorker != authority.LeaseWorker ||
		intent.Attempt != authority.Attempt ||
		intent.Generation != authority.Generation ||
		intent.FencingToken != authority.FencingToken {
		return fmt.Errorf(
			"formal agent dispatch intent is fenced by another workload lease generation",
		)
	}
	return nil
}

func (dispatcher *AgentStageDispatcher) revalidateAgentStageExecutionAuthority(
	ctx context.Context,
	subject AgentPlanningSubject,
	runID string,
	stageID string,
	expected AgentStageExecutionAuthority,
) (AgentStageExecutionAuthority, error) {
	observedAt := dispatcher.now().UTC()
	if observedAt.IsZero() {
		return AgentStageExecutionAuthority{}, fmt.Errorf(
			"agent dispatch authority clock returned zero time",
		)
	}
	current, err := dispatcher.authorities.CurrentAgentStageExecutionAuthority(
		ctx,
		subject,
		runID,
		stageID,
	)
	if err != nil {
		return AgentStageExecutionAuthority{}, fmt.Errorf(
			"revalidate formal agent execution authority: %w",
			err,
		)
	}
	current, err = bindAgentStageAttempt(current, expected.Attempt)
	if err != nil {
		return AgentStageExecutionAuthority{}, err
	}
	if err := current.validate(observedAt); err != nil {
		return AgentStageExecutionAuthority{}, fmt.Errorf(
			"validate reloaded formal agent execution authority: %w",
			err,
		)
	}
	if !expected.sameLeaseIdentity(current) || current.ExpiresAt.Before(expected.ExpiresAt) {
		return AgentStageExecutionAuthority{}, fmt.Errorf(
			"formal agent execution authority changed generation before provider operation",
		)
	}
	return current, nil
}

func bindAgentStageAttempt(
	authority AgentStageExecutionAuthority,
	attempt int,
) (AgentStageExecutionAuthority, error) {
	if attempt < 1 || uint64(attempt) > maxAgentJSONSafeInteger {
		return AgentStageExecutionAuthority{}, fmt.Errorf("stage attempt must be a JSON-safe positive integer")
	}
	offset := attempt - 1
	if authority.Generation > int(maxAgentJSONSafeInteger)-offset {
		return AgentStageExecutionAuthority{}, fmt.Errorf("stage generation exceeds JSON-safe integer range")
	}
	authority.Attempt = attempt
	authority.Generation += offset
	return authority, nil
}

func (dispatcher *AgentStageDispatcher) validateAgentStageCreateWindow(
	request contractsv1alpha1.StageExecutionRequest,
	authority AgentStageExecutionAuthority,
) error {
	observedAt := dispatcher.now().UTC()
	if observedAt.IsZero() || !request.Deadline.After(observedAt) {
		return fmt.Errorf("formal agent StageExecutionRequest deadline expired before Create claim")
	}
	if request.WorkloadID != authority.WorkloadID || request.LeaseID != authority.LeaseID ||
		request.LeaseWorkerID != authority.LeaseWorker ||
		request.Attempt != authority.Attempt || request.Generation != authority.Generation ||
		request.FencingToken != authority.FencingToken ||
		request.Deadline.After(authority.ExpiresAt) {
		return fmt.Errorf(
			"formal agent StageExecutionRequest is outside the current execution authority window",
		)
	}
	return nil
}

func agentDispatchIdentity(
	admission runmodel.AgentStagePlanAdmission,
	authority AgentStageExecutionAuthority,
) string {
	digest := sha256.New()
	for _, value := range []string{
		admission.Subject.TenantID,
		admission.Subject.OrganizationID,
		admission.Subject.WorkspaceID,
		admission.Subject.RepositoryID,
		admission.ReviewRunID,
		admission.Stage.ID,
		admission.Stage.Revision,
		admission.Stage.SHA256,
		admission.AdmissionID,
		admission.SHA256,
		admission.PlanSemanticSHA256,
		authority.WorkloadID,
		authority.LeaseID,
		authority.LeaseWorker,
		fmt.Sprintf("%d", authority.Attempt),
		fmt.Sprintf("%d", authority.Generation),
		fmt.Sprintf("%d", authority.FencingToken),
	} {
		_, _ = fmt.Fprintf(digest, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(digest.Sum(nil))
}
