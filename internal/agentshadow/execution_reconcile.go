package agentshadow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

// ReconcileExecution safely closes only the crash window where a complete
// shadow import is already committed but its execution completion is absent.
// It never invokes a worker or provider. The boolean is true only when this
// call observed an unknown attempt and established its succeeded completion.
func (service *Service) ReconcileExecution(
	ctx context.Context,
	request ExecutionReconcileRequest,
) (ExecutionAttempt, bool, error) {
	if service == nil || service.repository == nil {
		return ExecutionAttempt{}, false, fmt.Errorf("agent review shadow service is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return ExecutionAttempt{}, false, err
	}
	if err := validateScope(request.Scope); err != nil {
		return ExecutionAttempt{}, false, err
	}
	if err := validateIdentifier("execution_id", request.ExecutionID); err != nil {
		return ExecutionAttempt{}, false, err
	}
	intents, err := service.repository.listExecutionIntents(ctx, request.Scope)
	if err != nil {
		return ExecutionAttempt{}, false, err
	}
	if err := service.repository.validateExecutionHostFailureDirectory(ctx); err != nil {
		return ExecutionAttempt{}, false, err
	}
	// Validate before matching so an orphan/forged authorization cannot be hidden by
	// making its execution intent undiscoverable. reconcileExecutionIntent
	// repeats this at the shared LocalBridge retry boundary.
	if err := service.repository.validateExecutionImportAuthorizationDirectory(ctx); err != nil {
		return ExecutionAttempt{}, false, err
	}
	var matched *executionIntent
	for index := range intents {
		if intents[index].Plan.ExecutionID != request.ExecutionID {
			continue
		}
		if matched != nil {
			return ExecutionAttempt{}, false, fmt.Errorf(
				"%w: execution id %q is bound to multiple intents",
				ErrCorrupt,
				request.ExecutionID,
			)
		}
		matched = &intents[index]
	}
	if matched == nil {
		return ExecutionAttempt{}, false, ErrNotFound
	}
	attempt, _, reconciled, err := service.reconcileExecutionIntent(ctx, *matched)
	return attempt, reconciled, err
}

func (service *Service) reconcileExecutionIntent(
	ctx context.Context,
	intent executionIntent,
) (ExecutionAttempt, *Result, bool, error) {
	// This is also called directly by LocalBridge retries, so the strict
	// directory validation belongs here rather than only on the public
	// ReconcileExecution entrypoint. An unrelated forged/orphan authorization cannot
	// be hidden merely by resolving an exact idempotency key.
	if err := service.repository.validateExecutionImportAuthorizationDirectory(ctx); err != nil {
		return ExecutionAttempt{}, nil, false, err
	}
	// Validate all already-visible execution state before writing anything.
	attempt, result, err := service.projectExecutionAttempt(ctx, intent)
	if err != nil {
		return ExecutionAttempt{}, nil, false, err
	}
	if attempt.Status != ExecutionStatusUnknownOutcome {
		return attempt, result, false, nil
	}

	committed, exists, err := service.findCommittedResultForExecutionIntent(ctx, intent)
	if err != nil {
		return ExecutionAttempt{}, nil, false, err
	}
	if !exists {
		return attempt, nil, false, nil
	}
	completion := executionCompletion{
		SchemaVersion:  executionCompletionSchemaVersion,
		TenantID:       intent.TenantID,
		WorkspaceID:    intent.WorkspaceID,
		IdempotencyKey: intent.IdempotencyKey,
		SemanticDigest: intent.SemanticDigest,
		Status:         contractsv1alpha1.AgentReviewWorkerSucceeded,
		ManifestID:     committed.Manifest.ManifestID,
		// mapPiReviewReport copied the strictly bound worker CompletedAt into
		// the committed hypothesis set. Reconciliation recovers that exact
		// value instead of fabricating a wall-clock completion time.
		CompletedAt: committed.HypothesisSet.GeneratedAt,
	}
	_, created, err := service.repository.completeExecution(intent, completion)
	if err != nil {
		if _, observationErr := service.repository.observeExecutionHostFailure(
			intent,
			ExecutionHostFailureCompletionCommit,
			ExecutionHostFailureCompletionUnconfirmed,
		); observationErr != nil {
			err = fmt.Errorf("%w; persist bounded host failure observation: %v", err, observationErr)
		}
		return ExecutionAttempt{}, nil, false, fmt.Errorf("reconcile worker execution completion: %w", err)
	}
	reconciledAttempt, reconciledResult, err := service.projectExecutionAttempt(ctx, intent)
	if err != nil {
		return ExecutionAttempt{}, nil, false, err
	}
	if reconciledAttempt.Status != ExecutionStatusSucceeded || reconciledResult == nil ||
		reconciledResult.Manifest.ManifestID != committed.Manifest.ManifestID {
		return ExecutionAttempt{}, nil, false, fmt.Errorf(
			"%w: reconciled execution did not close over the committed result",
			ErrCorrupt,
		)
	}
	return reconciledAttempt, reconciledResult, created, nil
}

// findCommittedResultForExecutionIntent resolves only a pre-import immutable
// LocalBridge authorization and its exact expected committed manifest. An
// authorization without a committed ImportRecord remains unknown; a direct
// Service.Import without authorization cannot close an execution attempt.
func (service *Service) findCommittedResultForExecutionIntent(
	ctx context.Context,
	intent executionIntent,
) (Result, bool, error) {
	scope := Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID}
	authorization, exists, err := service.repository.loadExecutionImportAuthorization(intent)
	if err != nil {
		return Result{}, false, err
	}
	if !exists {
		return Result{}, false, nil
	}
	queried, err := service.Query(ctx, QueryRequest{
		Scope: scope, ManifestID: authorization.ExpectedManifestID,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Result{}, false, nil
		}
		return Result{}, false, fmt.Errorf(
			"revalidate authorized LocalBridge reconciliation import %q: %w",
			authorization.ExpectedManifestID,
			err,
		)
	}
	if err := validateAuthorizedCommittedResult(intent, authorization, queried); err != nil {
		return Result{}, false, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return queried, true, nil
}

func validateCommittedResultForExecutionIntent(
	intent executionIntent,
	result Result,
) error {
	if result.Record.TenantID != intent.TenantID ||
		result.Record.WorkspaceID != intent.WorkspaceID ||
		result.Record.IdempotencyKey != intent.IdempotencyKey ||
		!reflect.DeepEqual(result.Plan, intent.Plan) {
		return fmt.Errorf("committed result does not bind the exact execution intent and plan")
	}
	if result.Manifest.ManifestID != result.Record.ManifestID ||
		result.Manifest.PlanID != intent.Plan.PlanID ||
		result.Manifest.SourceRunID != intent.Plan.SourceRunID ||
		result.Manifest.ExecutionID != intent.Plan.ExecutionID ||
		result.Manifest.ReviewRunID != intent.Plan.ReviewRunID {
		return fmt.Errorf("committed result manifest identities do not bind the execution intent")
	}
	if result.Record.AcceptedAt.Before(intent.AcceptedAt) ||
		!result.Observation.RecordedAt.Equal(result.Record.AcceptedAt) ||
		result.HypothesisSet.GeneratedAt.Before(intent.AcceptedAt) ||
		result.HypothesisSet.GeneratedAt.After(result.Record.AcceptedAt) ||
		result.HypothesisSet.GeneratedAt.After(intent.AcceptedAt.Add(
			time.Duration(intent.Plan.Budget.TimeoutMS)*time.Millisecond,
		)) {
		return fmt.Errorf("committed result timing does not close the execution intent")
	}
	return nil
}
