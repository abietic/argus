package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"argus.local/argus/internal/execution"
	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const historicalAgentCancelTimeout = 30 * time.Second

// AgentStageHistoricalRepository exposes only immutable, already-claimed
// execution facts. It deliberately has no Claim/Create method.
type AgentStageHistoricalRepository interface {
	ListClaimedAgentStageDispatchIntents(
		context.Context,
		string,
		runmodel.AgentPlanningSubject,
	) ([]runmodel.AgentStageDispatchIntent, error)
	LookupAgentStagePlanAdmission(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStagePlanAdmission, bool, error)
	ReadLocalArtifact(context.Context, runmodel.ArtifactRef) ([]byte, error)
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

// AgentStageHistoricalExecutionGateway is intentionally narrower than the
// dispatch gateway: historical repair has no compile-time path to Create.
type AgentStageHistoricalExecutionGateway interface {
	Lookup(
		context.Context,
		contractsv1alpha1.StageExecutionRequest,
	) (execution.Handle, bool, error)
	PrepareCancel(
		context.Context,
		execution.Handle,
		execution.TerminalCancelAuthority,
	) (execution.PreparedCancel, error)
	CancelPrepared(context.Context, execution.PreparedCancel) error
}

// AgentStageHistoricalIntentDisposition is a trusted scheduling decision.
// Stale=false means the exact claim is still the active scheduling lease and
// must not be canceled. A stale reason becomes immutable cancellation audit.
type AgentStageHistoricalIntentDisposition struct {
	Stale  bool
	Reason string
}

// AgentStageHistoricalIntentStalenessResolver compares a claimed intent with
// the authoritative workload state. A later local intent is insufficient:
// the latest claim can itself be canceled, terminal, expired, or reassigned.
type AgentStageHistoricalIntentStalenessResolver interface {
	ClassifyAgentStageHistoricalIntent(
		context.Context,
		AgentPlanningSubject,
		runmodel.AgentStageDispatchIntent,
	) (AgentStageHistoricalIntentDisposition, error)
}

type ReconcileHistoricalAgentStagesCommand struct {
	ReviewRunID string
}

type HistoricalAgentStageReconciliation struct {
	Intent           runmodel.AgentStageDispatchIntent
	Terminal         runmodel.AgentStageTerminalGate
	Binding          *runmodel.AgentStageExecutionBinding
	ProviderFound    bool
	CancelDelivered  bool
	AlreadyCompleted bool
	Current          bool
	Reason           string
	Error            string
}

type HistoricalAgentStageReconciliationReport struct {
	Items []HistoricalAgentStageReconciliation
}

// AgentStageHistoricalReconciler repairs every stale claimed execution,
// including a latest local generation whose workload was canceled, expired,
// completed, or reassigned. It can only Lookup an exact historical request
// and deliver an idempotent Cancel; it can never Create.
type AgentStageHistoricalReconciler struct {
	repository AgentStageHistoricalRepository
	staleness  AgentStageHistoricalIntentStalenessResolver
	gateway    AgentStageHistoricalExecutionGateway
	actor      string
	now        func() time.Time
}

func NewAgentStageHistoricalReconciler(
	repository AgentStageHistoricalRepository,
	staleness AgentStageHistoricalIntentStalenessResolver,
	gateway AgentStageHistoricalExecutionGateway,
	actor string,
	now func() time.Time,
) (*AgentStageHistoricalReconciler, error) {
	if repository == nil || staleness == nil || gateway == nil {
		return nil, fmt.Errorf(
			"historical repository, staleness resolver, and execution gateway are required",
		)
	}
	if err := validateLocalIdentity("recovery_actor", actor); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &AgentStageHistoricalReconciler{
		repository: repository,
		staleness:  staleness,
		gateway:    gateway,
		actor:      actor,
		now:        now,
	}, nil
}

func (reconciler *AgentStageHistoricalReconciler) ReconcileSupersededAgentStages(
	ctx context.Context,
	subject AgentPlanningSubject,
	command ReconcileHistoricalAgentStagesCommand,
) (HistoricalAgentStageReconciliationReport, error) {
	report := HistoricalAgentStageReconciliationReport{
		Items: []HistoricalAgentStageReconciliation{},
	}
	if ctx == nil {
		return report, fmt.Errorf("context is required")
	}
	if reconciler == nil || reconciler.repository == nil || reconciler.staleness == nil ||
		reconciler.gateway == nil || reconciler.actor == "" || reconciler.now == nil {
		return report, fmt.Errorf("historical agent-stage reconciler is not initialized")
	}
	if err := subject.validate(); err != nil {
		return report, fmt.Errorf("validate historical reconciliation subject: %w", err)
	}
	ledgerSubject := planningLedgerSubject(subject)
	if err := ledgerSubject.Validate(); err != nil {
		return report, fmt.Errorf("validate historical reconciliation ledger subject: %w", err)
	}
	if err := validateLocalIdentity("review_run_id", command.ReviewRunID); err != nil {
		return report, err
	}
	intents, err := reconciler.repository.ListClaimedAgentStageDispatchIntents(
		ctx,
		command.ReviewRunID,
		ledgerSubject,
	)
	if err != nil {
		return report, fmt.Errorf("list claimed agent-stage intents: %w", err)
	}
	reconcileErrors := make([]error, 0)
	for _, intent := range intents {
		item, reconcileErr := reconciler.reconcileHistoricalAgentStage(
			ctx,
			subject,
			ledgerSubject,
			command,
			intent,
		)
		if reconcileErr != nil {
			item.Error = reconcileErr.Error()
			reconcileErrors = append(
				reconcileErrors,
				fmt.Errorf("reconcile claimed intent %q: %w", intent.IntentID, reconcileErr),
			)
		}
		report.Items = append(report.Items, item)
	}
	return report, errors.Join(reconcileErrors...)
}

func (reconciler *AgentStageHistoricalReconciler) reconcileHistoricalAgentStage(
	ctx context.Context,
	subject AgentPlanningSubject,
	ledgerSubject runmodel.AgentPlanningSubject,
	command ReconcileHistoricalAgentStagesCommand,
	intent runmodel.AgentStageDispatchIntent,
) (HistoricalAgentStageReconciliation, error) {
	item := HistoricalAgentStageReconciliation{Intent: intent}
	if intent.Subject != ledgerSubject || intent.ReviewRunID != command.ReviewRunID {
		return item, fmt.Errorf("claimed intent escaped authenticated reconciliation scope")
	}
	admission, found, err := reconciler.repository.LookupAgentStagePlanAdmission(
		ctx,
		intent.ReviewRunID,
		intent.Stage.ID,
		ledgerSubject,
	)
	if err != nil {
		return item, fmt.Errorf("lookup claimed intent admission: %w", err)
	}
	if !found {
		return item, fmt.Errorf("claimed intent has no exact plan admission")
	}
	if err := intent.ValidateAgainstAdmission(admission); err != nil {
		return item, err
	}
	requestBytes, err := reconciler.repository.ReadLocalArtifact(ctx, intent.RequestRef)
	if err != nil {
		return item, fmt.Errorf("read claimed StageExecutionRequest: %w", err)
	}
	if localRefForBytes(runmodel.ContractStageExecutionRequest, requestBytes) != intent.RequestRef {
		return item, fmt.Errorf("claimed StageExecutionRequest bytes do not match intent ref")
	}
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(requestBytes)
	if err != nil {
		return item, fmt.Errorf("strictly decode claimed StageExecutionRequest: %w", err)
	}
	if err := validateHistoricalAgentStageRequest(request, intent, admission); err != nil {
		return item, err
	}

	terminal, terminalFound, err := reconciler.repository.LookupAgentStageTerminalGate(
		ctx,
		intent.ReviewRunID,
		intent.IntentID,
		ledgerSubject,
	)
	if err != nil {
		return item, fmt.Errorf("lookup historical terminal winner: %w", err)
	}
	if terminalFound && terminal.Kind == runmodel.AgentStageSucceededResultAccepted {
		item.Terminal = terminal
		item.AlreadyCompleted = true
		return item, nil
	}
	if !terminalFound {
		disposition, classifyErr := reconciler.staleness.ClassifyAgentStageHistoricalIntent(
			ctx,
			subject,
			intent,
		)
		if classifyErr != nil {
			return item, fmt.Errorf("classify claimed scheduling authority: %w", classifyErr)
		}
		if !disposition.Stale {
			if disposition.Reason != "" {
				return item, fmt.Errorf("current scheduling disposition must not carry a reason")
			}
			item.Current = true
			return item, nil
		}
		if disposition.Reason == "" {
			return item, fmt.Errorf("stale scheduling disposition requires a reason")
		}
		requestedAt := reconciler.now().UTC()
		if requestedAt.IsZero() || requestedAt.Before(intent.RecordedAt) {
			return item, fmt.Errorf("historical reconciliation time predates dispatch intent")
		}
		terminal, err = runmodel.SealAgentStageTerminalGate(
			runmodel.AgentStageTerminalGate{
				SchemaVersion:         runmodel.AgentStageTerminalGateSchemaVersion,
				Kind:                  runmodel.AgentStageCancellationRequested,
				Subject:               ledgerSubject,
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
					Actor:                reconciler.actor,
					Reason:               disposition.Reason,
					RequestedAt:          requestedAt,
				},
			},
		)
		if err != nil {
			return item, fmt.Errorf("seal historical cancellation gate: %w", err)
		}
		if appendErr := reconciler.repository.AppendAgentStageTerminalGate(
			ctx,
			terminal,
		); appendErr != nil {
			winner, winnerFound, lookupErr := reconciler.repository.LookupAgentStageTerminalGate(
				ctx,
				intent.ReviewRunID,
				intent.IntentID,
				ledgerSubject,
			)
			if lookupErr != nil || !winnerFound {
				return item, fmt.Errorf(
					"append historical cancellation gate: %v; reload winner: %v",
					appendErr,
					lookupErr,
				)
			}
			if winner.Kind == runmodel.AgentStageSucceededResultAccepted {
				item.Terminal = winner
				item.AlreadyCompleted = true
				return item, nil
			}
			terminal = winner
		}
	}
	if terminal.Kind != runmodel.AgentStageCancellationRequested || terminal.Cancellation == nil ||
		terminal.Cancellation.CancelIdempotencyKey != intent.CancelIdempotencyKey {
		return item, fmt.Errorf("claimed intent has an invalid terminal cancellation winner")
	}
	item.Terminal = terminal
	item.Reason = terminal.Cancellation.Reason

	binding, bindingFound, err := reconciler.repository.LookupAgentStageExecutionBinding(
		ctx,
		intent.ReviewRunID,
		intent.IntentID,
		ledgerSubject,
	)
	if err != nil {
		return item, fmt.Errorf("lookup historical execution binding: %w", err)
	}
	if !bindingFound {
		handle, providerFound, lookupErr := reconciler.gateway.Lookup(ctx, request)
		if lookupErr != nil {
			return item, fmt.Errorf("lookup stale provider execution: %w", lookupErr)
		}
		item.ProviderFound = providerFound
		if !providerFound {
			return item, nil
		}
		recordedAt := reconciler.now().UTC()
		if recordedAt.IsZero() {
			return item, fmt.Errorf("historical binding time is required")
		}
		binding, err = sealHistoricalAgentStageBinding(intent, handle, recordedAt)
		if err != nil {
			return item, err
		}
		if appendErr := reconciler.repository.AppendAgentStageExecutionBinding(
			ctx,
			binding,
		); appendErr != nil {
			winner, winnerFound, lookupErr := reconciler.repository.LookupAgentStageExecutionBinding(
				ctx,
				intent.ReviewRunID,
				intent.IntentID,
				ledgerSubject,
			)
			if lookupErr != nil || !winnerFound || winner.ValidateAgainstIntent(intent) != nil {
				return item, fmt.Errorf(
					"persist historical execution binding: %v; reload winner: %v",
					appendErr,
					lookupErr,
				)
			}
			if !runmodel.SameAgentStageExecutionDecision(winner, binding) {
				return item, fmt.Errorf(
					"%w: exact Lookup returned provider handle or binding tuple different from durable winner",
					ErrAgentStageProviderContractViolation,
				)
			}
			binding = winner
		}
	} else {
		if err := binding.ValidateAgainstIntent(intent); err != nil {
			return item, err
		}
		item.ProviderFound = true
	}
	item.Binding = &binding
	handle := execution.Handle{
		ProviderHandle:   binding.ProviderHandle,
		ExecutionID:      binding.ExecutionID,
		Attempt:          binding.Attempt,
		Generation:       binding.Generation,
		FencingToken:     binding.FencingToken,
		IdempotencyKey:   binding.CreateIdempotencyKey,
		RequestSHA256:    binding.RequestSemanticSHA256,
		CapabilitySHA256: binding.CapabilitySHA256,
	}
	cancelContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		historicalAgentCancelTimeout,
	)
	defer cancel()
	preparedCancel, err := reconciler.gateway.PrepareCancel(
		cancelContext,
		handle,
		execution.TerminalCancelAuthority{
			TenantID: intent.Subject.TenantID, WorkspaceID: intent.Subject.WorkspaceID,
			ReviewRunID: intent.ReviewRunID,
			StageID:     intent.Stage.ID, StageRevision: intent.Stage.Revision,
			StageSHA256: intent.Stage.SHA256,
			IntentID:    intent.IntentID, IntentSHA256: intent.SHA256,
			TerminalGateID: terminal.GateID, TerminalGateSHA256: terminal.SHA256,
			CancelIdempotencyKey: intent.CancelIdempotencyKey,
		},
	)
	if err != nil {
		return item, fmt.Errorf("authorize exact stale provider cancellation: %w", err)
	}
	if err := reconciler.gateway.CancelPrepared(cancelContext, preparedCancel); err != nil {
		return item, fmt.Errorf("deliver exact stale provider cancellation: %w", err)
	}
	item.CancelDelivered = true
	return item, nil
}

func validateHistoricalAgentStageRequest(
	request contractsv1alpha1.StageExecutionRequest,
	intent runmodel.AgentStageDispatchIntent,
	admission runmodel.AgentStagePlanAdmission,
) error {
	if request.RequestSHA256 != intent.RequestSemanticSHA256 ||
		request.ExecutionID != intent.ExecutionID ||
		request.ReviewRunID != intent.ReviewRunID ||
		request.TenantID != intent.Subject.TenantID ||
		request.WorkspaceID != intent.Subject.WorkspaceID ||
		request.WorkloadID != intent.WorkloadID || request.LeaseID != intent.LeaseID ||
		request.LeaseWorkerID != intent.LeaseWorker || request.Stage.ID != intent.Stage.ID ||
		request.Stage.Revision != intent.Stage.Revision || request.Stage.SHA256 != intent.Stage.SHA256 ||
		request.Attempt != intent.Attempt || request.Generation != intent.Generation ||
		request.FencingToken != intent.FencingToken ||
		request.IdempotencyKey != intent.CreateIdempotencyKey ||
		request.Capability.SHA256 != intent.CapabilitySHA256 ||
		request.Plan != bindingFromAdmission(admission.Plan.Governed) {
		return fmt.Errorf("historical StageExecutionRequest does not match exact claimed intent")
	}
	return nil
}

func sealHistoricalAgentStageBinding(
	intent runmodel.AgentStageDispatchIntent,
	handle execution.Handle,
	recordedAt time.Time,
) (runmodel.AgentStageExecutionBinding, error) {
	if recordedAt.Before(intent.RecordedAt) {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"historical binding time predates dispatch intent",
		)
	}
	binding, err := runmodel.SealAgentStageExecutionBinding(
		runmodel.AgentStageExecutionBinding{
			SchemaVersion:         runmodel.AgentStageExecutionBindingSchemaVersion,
			Subject:               intent.Subject,
			ReviewRunID:           intent.ReviewRunID,
			Stage:                 intent.Stage,
			WorkloadID:            intent.WorkloadID,
			LeaseID:               intent.LeaseID,
			LeaseWorker:           intent.LeaseWorker,
			IntentID:              intent.IntentID,
			IntentSHA256:          intent.SHA256,
			RequestRef:            intent.RequestRef,
			RequestSemanticSHA256: intent.RequestSemanticSHA256,
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
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"seal historical execution binding: %w",
			err,
		)
	}
	if err := binding.ValidateAgainstIntent(intent); err != nil {
		return runmodel.AgentStageExecutionBinding{}, err
	}
	return binding, nil
}
