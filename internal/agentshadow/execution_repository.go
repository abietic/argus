package agentshadow

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	executionIntentSchemaVersion     = "argus.agent_review_execution_intent.v1alpha1"
	executionCompletionSchemaVersion = "argus.agent_review_execution_completion.v1alpha2"
	executionIntentRoot              = "agent-review-shadow/execution-intents/"
	executionCompletionRoot          = "agent-review-shadow/execution-completions/"
)

func (repository *Repository) acquireExecutionIntent(
	proposed executionIntent,
) (executionIntent, bool, error) {
	if err := validateExecutionIntent(proposed); err != nil {
		return executionIntent{}, false, err
	}
	objectID := executionIntentObjectID(
		Scope{TenantID: proposed.TenantID, WorkspaceID: proposed.WorkspaceID},
		proposed.IdempotencyKey,
	)
	if err := repository.store.PutJSON(objectID, proposed); err == nil {
		return proposed, true, nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return executionIntent{}, false, fmt.Errorf("persist worker execution intent: %w", err)
	}
	var existing executionIntent
	if err := repository.store.GetJSON(objectID, &existing); err != nil {
		return executionIntent{}, false, fmt.Errorf(
			"%w: load worker execution intent: %v",
			ErrCorrupt,
			err,
		)
	}
	if err := validateExecutionIntent(existing); err != nil {
		return executionIntent{}, false, fmt.Errorf(
			"%w: validate worker execution intent: %v",
			ErrCorrupt,
			err,
		)
	}
	if existing.TenantID != proposed.TenantID ||
		existing.WorkspaceID != proposed.WorkspaceID ||
		existing.IdempotencyKey != proposed.IdempotencyKey {
		return executionIntent{}, false, fmt.Errorf(
			"%w: worker execution intent scope changed",
			ErrCorrupt,
		)
	}
	if existing.SemanticDigest != proposed.SemanticDigest {
		return executionIntent{}, false, fmt.Errorf(
			"%w: idempotency key %q is bound to another worker execution request",
			ErrConflict,
			proposed.IdempotencyKey,
		)
	}
	return existing, false, nil
}

func (repository *Repository) loadExecutionCompletion(
	intent executionIntent,
) (executionCompletion, bool, error) {
	objectID := executionCompletionObjectID(
		Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID},
		intent.IdempotencyKey,
	)
	var completion executionCompletion
	if err := repository.store.GetJSON(objectID, &completion); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return executionCompletion{}, false, nil
		}
		return executionCompletion{}, false, fmt.Errorf(
			"%w: load worker execution completion: %v",
			ErrCorrupt,
			err,
		)
	}
	if err := validateExecutionCompletion(intent, completion); err != nil {
		return executionCompletion{}, false, fmt.Errorf(
			"%w: validate worker execution completion: %v",
			ErrCorrupt,
			err,
		)
	}
	return completion, true, nil
}

func (repository *Repository) completeExecution(
	intent executionIntent,
	completion executionCompletion,
) (executionCompletion, bool, error) {
	if !completion.RecordedAt.IsZero() {
		return executionCompletion{}, false, fmt.Errorf("worker execution completion recorded_at is host-owned")
	}
	// Reuse the first host observation for an exact semantic retry. RecordedAt
	// is deliberately excluded from caller input, otherwise a retry would
	// conflict merely because the host clock advanced.
	existing, exists, err := repository.loadExecutionCompletion(intent)
	if err != nil {
		return executionCompletion{}, false, err
	}
	if exists {
		completion.RecordedAt = existing.RecordedAt
		if err := validateExecutionCompletion(intent, completion); err != nil {
			return executionCompletion{}, false, err
		}
		if err := repository.validateSucceededExecutionCompletion(intent, existing); err != nil {
			return executionCompletion{}, false, fmt.Errorf(
				"%w: validate existing succeeded execution completion: %v",
				ErrCorrupt,
				err,
			)
		}
		if !reflect.DeepEqual(existing, completion) {
			return executionCompletion{}, false, fmt.Errorf(
				"%w: worker execution already has another completion",
				ErrConflict,
			)
		}
		return existing, false, nil
	}
	recordedAt, err := repository.normalizedNow()
	if err != nil {
		return executionCompletion{}, false, err
	}
	completion.RecordedAt = recordedAt
	if err := validateExecutionCompletion(intent, completion); err != nil {
		return executionCompletion{}, false, err
	}
	if err := repository.validateSucceededExecutionCompletion(intent, completion); err != nil {
		return executionCompletion{}, false, err
	}
	objectID := executionCompletionObjectID(
		Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID},
		intent.IdempotencyKey,
	)
	if err := repository.store.PutJSON(objectID, completion); err == nil {
		return completion, true, nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return executionCompletion{}, false, fmt.Errorf("persist worker execution completion: %w", err)
	}
	// Another writer may have committed after the read above. Rebind the
	// proposed completion to the winning host timestamp before comparing it.
	if err := repository.store.GetJSON(objectID, &existing); err != nil {
		return executionCompletion{}, false, fmt.Errorf(
			"%w: load existing worker execution completion: %v",
			ErrCorrupt,
			err,
		)
	}
	if err := validateExecutionCompletion(intent, existing); err != nil {
		return executionCompletion{}, false, fmt.Errorf(
			"%w: validate existing worker execution completion: %v",
			ErrCorrupt,
			err,
		)
	}
	completion.RecordedAt = existing.RecordedAt
	if err := validateExecutionCompletion(intent, completion); err != nil {
		return executionCompletion{}, false, err
	}
	if err := repository.validateSucceededExecutionCompletion(intent, existing); err != nil {
		return executionCompletion{}, false, fmt.Errorf(
			"%w: validate winning succeeded execution completion: %v",
			ErrCorrupt,
			err,
		)
	}
	if !reflect.DeepEqual(existing, completion) {
		return executionCompletion{}, false, fmt.Errorf(
			"%w: worker execution already has another completion",
			ErrConflict,
		)
	}
	return existing, false, nil
}

func (repository *Repository) validateSucceededExecutionCompletion(
	intent executionIntent,
	completion executionCompletion,
) error {
	if completion.Status != contractsv1alpha1.AgentReviewWorkerSucceeded {
		return nil
	}
	authorization, exists, err := repository.loadExecutionImportAuthorization(intent)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("succeeded worker execution has no LocalBridge import authorization")
	}
	if authorization.ExpectedManifestID != completion.ManifestID {
		return fmt.Errorf("succeeded worker completion manifest changed from its LocalBridge authorization")
	}
	record, err := repository.loadRecord(authorization.ExpectedManifestID)
	if err != nil {
		return fmt.Errorf("load succeeded worker import record: %w", err)
	}
	if record.TenantID != intent.TenantID || record.WorkspaceID != intent.WorkspaceID ||
		record.IdempotencyKey != intent.IdempotencyKey ||
		record.InputDigest != authorization.ImportInputDigest ||
		record.ManifestID != authorization.ExpectedManifestID {
		return fmt.Errorf("succeeded worker import record changed from its LocalBridge authorization")
	}
	if record.AcceptedAt.After(completion.RecordedAt) {
		return fmt.Errorf(
			"host clock regressed before the committed shadow import; refusing succeeded completion",
		)
	}
	return nil
}

func validateExecutionIntent(intent executionIntent) error {
	if intent.SchemaVersion != executionIntentSchemaVersion {
		return fmt.Errorf("unsupported worker execution intent schema %q", intent.SchemaVersion)
	}
	if err := validateScope(Scope{
		TenantID:    intent.TenantID,
		WorkspaceID: intent.WorkspaceID,
	}); err != nil {
		return err
	}
	if err := validateIdempotencyKey(intent.IdempotencyKey); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"semantic_digest":       intent.SemanticDigest,
		"worker_request_sha256": intent.WorkerRequestSHA256,
		"node_sha256":           intent.NodeSHA256,
		"worker_script_sha256":  intent.WorkerScriptSHA256,
		"worker_package_sha256": intent.WorkerPackageSHA256,
	} {
		if !lowerSHA256(value) {
			return fmt.Errorf("%s must be a lowercase SHA-256", name)
		}
	}
	if err := intent.Plan.Validate(); err != nil {
		return fmt.Errorf("validate worker execution plan: %w", err)
	}
	if !intent.AcceptedAt.Equal(intent.Plan.CreatedAt) {
		return fmt.Errorf("worker intent accepted_at must equal plan.created_at")
	}
	return nil
}

func validateExecutionCompletion(
	intent executionIntent,
	completion executionCompletion,
) error {
	if completion.SchemaVersion != executionCompletionSchemaVersion {
		return fmt.Errorf(
			"unsupported worker execution completion schema %q",
			completion.SchemaVersion,
		)
	}
	if completion.TenantID != intent.TenantID ||
		completion.WorkspaceID != intent.WorkspaceID ||
		completion.IdempotencyKey != intent.IdempotencyKey ||
		completion.SemanticDigest != intent.SemanticDigest {
		return fmt.Errorf("worker completion does not bind its exact intent")
	}
	if completion.CompletedAt.IsZero() || completion.RecordedAt.IsZero() ||
		completion.CompletedAt.Before(intent.AcceptedAt) ||
		completion.RecordedAt.Before(intent.AcceptedAt) ||
		completion.CompletedAt.After(completion.RecordedAt) {
		return fmt.Errorf("worker completion has invalid time binding")
	}
	if completion.Status == contractsv1alpha1.AgentReviewWorkerSucceeded &&
		completion.CompletedAt.After(intent.AcceptedAt.Add(
			time.Duration(intent.Plan.Budget.TimeoutMS)*time.Millisecond,
		)) {
		return fmt.Errorf("succeeded worker completion exceeded its deadline")
	}
	for name, value := range map[string]time.Time{
		"completed_at": completion.CompletedAt,
		"recorded_at":  completion.RecordedAt,
	} {
		_, offset := value.Zone()
		if offset != 0 {
			return fmt.Errorf("worker completion %s must use UTC", name)
		}
	}
	switch completion.Status {
	case contractsv1alpha1.AgentReviewWorkerSucceeded:
		if completion.ManifestID == "" || completion.Failure != nil {
			return fmt.Errorf("succeeded worker completion requires manifest_id only")
		}
	case contractsv1alpha1.AgentReviewWorkerFailed,
		contractsv1alpha1.AgentReviewWorkerCanceled:
		if completion.ManifestID != "" || completion.Failure == nil {
			return fmt.Errorf("failed/canceled worker completion requires failure only")
		}
		if err := validateIdentifier("worker failure code", completion.Failure.Code); err != nil {
			return err
		}
		if completion.Failure.Message != redactedWorkerFailureMessage {
			return fmt.Errorf("worker completion failure message is not redacted")
		}
	default:
		return fmt.Errorf("unsupported worker completion status %q", completion.Status)
	}
	return nil
}

func executionIntentObjectID(scope Scope, idempotencyKey string) string {
	return executionIntentRoot + digestStrings(
		scope.TenantID,
		scope.WorkspaceID,
		idempotencyKey,
	)
}

func executionCompletionObjectID(scope Scope, idempotencyKey string) string {
	return executionCompletionRoot + digestStrings(
		scope.TenantID,
		scope.WorkspaceID,
		idempotencyKey,
	)
}
