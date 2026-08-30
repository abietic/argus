package application

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"argus.local/argus/internal/execution"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

// FormalAgentStageDispatcher is the narrow dispatch port consumed by the
// synchronous local orchestration boundary. The production implementation is
// AgentStageDispatcher; keeping the boundary narrow makes the state-machine
// ordering independently testable.
type FormalAgentStageDispatcher interface {
	DispatchGovernedAgentStage(
		context.Context,
		AgentPlanningSubject,
		DispatchAgentStageCommand,
	) (DispatchedAgentStage, error)
}

// FormalAgentStageExecutor waits for the one durable result associated with a
// provider handle. It must not manufacture a result from process exit alone.
type FormalAgentStageExecutor interface {
	AwaitResult(
		context.Context,
		execution.Handle,
	) (contractsv1alpha1.StageExecutionResult, []byte, error)
}

// FormalAgentStageResultIngress owns both success and operational-terminal
// admission. Failed or canceled provider execution is never converted into an
// empty hypothesis set.
type FormalAgentStageResultIngress interface {
	AdmitGovernedAgentStageResult(
		context.Context,
		AgentPlanningSubject,
		AdmitAgentStageResultCommand,
	) (AdmittedAgentStageResult, error)
	AdmitGovernedAgentStageTerminalOutcome(
		context.Context,
		AgentPlanningSubject,
		AdmitAgentStageResultCommand,
	) (AdmittedAgentStageTerminalOutcome, error)
}

type RunFormalAgentReviewCommand struct {
	ReviewRunID string
	StageID     string
}

// FormalAgentReviewOutcome is a closed union: exactly one admitted result is
// set. Dispatch facts are included so a caller can report and reconcile the
// immutable execution coordinate without re-resolving mutable configuration.
type FormalAgentReviewOutcome struct {
	Dispatch DispatchedAgentStage
	Success  *AdmittedAgentStageResult
	Terminal *AdmittedAgentStageTerminalOutcome
}

// TerminalResult returns the one admitted provider result, or nil for a
// partially constructed outcome returned alongside an orchestration error.
func (outcome FormalAgentReviewOutcome) TerminalResult() *contractsv1alpha1.StageExecutionResult {
	if outcome.Success != nil {
		return &outcome.Success.Result
	}
	if outcome.Terminal != nil {
		return &outcome.Terminal.Result
	}
	return nil
}

type FormalAgentReviewOrchestrator struct {
	dispatcher FormalAgentStageDispatcher
	executor   FormalAgentStageExecutor
	ingress    FormalAgentStageResultIngress
}

func NewFormalAgentReviewOrchestrator(
	dispatcher FormalAgentStageDispatcher,
	executor FormalAgentStageExecutor,
	ingress FormalAgentStageResultIngress,
) (*FormalAgentReviewOrchestrator, error) {
	if dispatcher == nil || executor == nil || ingress == nil {
		return nil, fmt.Errorf("formal agent dispatcher, executor, and result ingress are required")
	}
	return &FormalAgentReviewOrchestrator{
		dispatcher: dispatcher,
		executor:   executor,
		ingress:    ingress,
	}, nil
}

// Run dispatches or recovers the exact formal stage, waits for its durable
// provider result, and admits that result through the authenticated terminal
// gate. An error before admission is safe to retry with the same run/stage:
// dispatch, provider Ensure, callback verification, and terminal append are
// all exact-idempotent below this boundary.
func (orchestrator *FormalAgentReviewOrchestrator) Run(
	ctx context.Context,
	subject AgentPlanningSubject,
	command RunFormalAgentReviewCommand,
) (FormalAgentReviewOutcome, error) {
	if ctx == nil {
		return FormalAgentReviewOutcome{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return FormalAgentReviewOutcome{}, err
	}
	if orchestrator == nil || orchestrator.dispatcher == nil ||
		orchestrator.executor == nil || orchestrator.ingress == nil {
		return FormalAgentReviewOutcome{}, fmt.Errorf("formal agent review orchestrator is not initialized")
	}
	if err := subject.validate(); err != nil {
		return FormalAgentReviewOutcome{}, fmt.Errorf("validate formal review subject: %w", err)
	}
	if err := validateLocalIdentity("review_run_id", command.ReviewRunID); err != nil {
		return FormalAgentReviewOutcome{}, err
	}
	if command.StageID == "" {
		return FormalAgentReviewOutcome{}, fmt.Errorf("stage_id is required")
	}

	for attempt := 1; ; attempt++ {
		dispatched, err := orchestrator.dispatcher.DispatchGovernedAgentStage(
			ctx,
			subject,
			DispatchAgentStageCommand{
				ReviewRunID: command.ReviewRunID,
				StageID:     command.StageID,
				Attempt:     attempt,
			},
		)
		if err != nil {
			return FormalAgentReviewOutcome{}, fmt.Errorf("dispatch formal agent review: %w", err)
		}
		handle := execution.Handle{
			ProviderHandle:   dispatched.Binding.ProviderHandle,
			ExecutionID:      dispatched.Request.ExecutionID,
			Attempt:          dispatched.Request.Attempt,
			Generation:       dispatched.Request.Generation,
			FencingToken:     dispatched.Request.FencingToken,
			IdempotencyKey:   dispatched.Request.IdempotencyKey,
			RequestSHA256:    dispatched.Request.RequestSHA256,
			CapabilitySHA256: dispatched.Request.Capability.SHA256,
		}
		result, callbackProof, err := orchestrator.executor.AwaitResult(ctx, handle)
		if err != nil {
			return FormalAgentReviewOutcome{Dispatch: dispatched}, fmt.Errorf(
				"await durable formal agent result: %w", err,
			)
		}
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return FormalAgentReviewOutcome{Dispatch: dispatched}, fmt.Errorf(
				"marshal formal agent result: %w", err,
			)
		}
		admit := AdmitAgentStageResultCommand{
			ReviewRunID:   command.ReviewRunID,
			StageID:       command.StageID,
			ResultJSON:    resultJSON,
			CallbackProof: callbackProof,
		}
		outcome := FormalAgentReviewOutcome{Dispatch: dispatched}
		switch result.Status {
		case contractsv1alpha1.StageExecutionSucceeded:
			success, admitErr := orchestrator.ingress.AdmitGovernedAgentStageResult(
				ctx, subject, admit,
			)
			if admitErr != nil {
				return outcome, fmt.Errorf("admit formal agent success: %w", admitErr)
			}
			outcome.Success = &success
			return outcome, nil
		case contractsv1alpha1.StageExecutionFailed,
			contractsv1alpha1.StageExecutionCanceled:
			terminal, admitErr := orchestrator.ingress.AdmitGovernedAgentStageTerminalOutcome(
				ctx, subject, admit,
			)
			if admitErr != nil {
				return outcome, fmt.Errorf("admit formal agent terminal outcome: %w", admitErr)
			}
			outcome.Terminal = &terminal
			if !formalAgentAttemptMayRetry(dispatched.Plan, result) ||
				attempt >= dispatched.Plan.Retry.MaxAttempts {
				return outcome, nil
			}
			if err := waitFormalAgentRetry(ctx, dispatched.Plan.Retry.BackoffMS); err != nil {
				return outcome, err
			}
		default:
			return outcome, fmt.Errorf("unsupported formal agent result status %q", result.Status)
		}
	}
}

func formalAgentAttemptMayRetry(
	plan contractsv1alpha1.AgentStagePlan,
	result contractsv1alpha1.StageExecutionResult,
) bool {
	return result.Status == contractsv1alpha1.StageExecutionFailed &&
		result.Failure != nil && result.Failure.Retryable &&
		slices.Contains(plan.Retry.RetryableCodes, result.Failure.Code)
}

func waitFormalAgentRetry(ctx context.Context, backoffMS int64) error {
	if backoffMS <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(time.Duration(backoffMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
