package agentshadow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"argus.local/argus/internal/store/local"
)

const (
	ExecutionHostFailureObservationSchemaVersion = "argus.agent_review_execution_host_failure.v1alpha1"
	executionHostFailureRoot                     = "agent-review-shadow/execution-host-failures/"
	maxExecutionHostFailureBytes                 = 4 << 10
)

// observeExecutionHostFailure records only a closed stage/reason pair. The
// originating error is deliberately not accepted by this API, preventing raw
// stderr, provider payloads, URLs, and credentials from entering persistence.
// A racing or repeated call retains the first valid immutable observation.
func (repository *Repository) observeExecutionHostFailure(
	intent executionIntent,
	stage ExecutionHostFailureStage,
	reasonCode ExecutionHostFailureCode,
) (ExecutionHostFailureObservation, error) {
	if err := validateExecutionIntent(intent); err != nil {
		return ExecutionHostFailureObservation{}, err
	}
	intentSHA256, err := canonicalObjectSHA256(intent)
	if err != nil {
		return ExecutionHostFailureObservation{}, fmt.Errorf("canonicalize worker execution intent: %w", err)
	}
	observedAt, clockErr := repository.normalizedNow()
	timeSource := ExecutionHostObservationClock
	if clockErr != nil || observedAt.Before(intent.AcceptedAt) {
		// The already-durable acceptance time is a safe host-owned lower bound.
		// It avoids inventing a wall-clock time when the injected host clock is
		// unavailable or regresses.
		observedAt = intent.AcceptedAt
		timeSource = ExecutionHostObservationAcceptedLowerBound
	}
	observation := ExecutionHostFailureObservation{
		SchemaVersion:  ExecutionHostFailureObservationSchemaVersion,
		Scope:          Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID},
		IdempotencyKey: intent.IdempotencyKey,
		SemanticDigest: intent.SemanticDigest,
		ExecutionID:    intent.Plan.ExecutionID,
		IntentSHA256:   intentSHA256,
		Stage:          stage,
		ReasonCode:     reasonCode,
		ObservedAt:     observedAt,
		TimeSource:     timeSource,
	}
	if err := validateExecutionHostFailureObservation(intent, observation); err != nil {
		return ExecutionHostFailureObservation{}, err
	}
	data, err := json.Marshal(observation)
	if err != nil {
		return ExecutionHostFailureObservation{}, fmt.Errorf("encode execution host failure observation: %w", err)
	}
	if len(data) > maxExecutionHostFailureBytes {
		return ExecutionHostFailureObservation{}, fmt.Errorf(
			"execution host failure observation exceeds %d bytes",
			maxExecutionHostFailureBytes,
		)
	}
	objectID := executionHostFailureObjectID(observation.Scope, observation.IdempotencyKey)
	if err := repository.store.PutJSON(objectID, observation); err == nil {
		return observation, nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return ExecutionHostFailureObservation{}, fmt.Errorf("persist execution host failure observation: %w", err)
	}
	existing, exists, err := repository.loadExecutionHostFailure(intent)
	if err != nil {
		return ExecutionHostFailureObservation{}, err
	}
	if !exists {
		return ExecutionHostFailureObservation{}, fmt.Errorf(
			"%w: immutable execution host failure disappeared",
			ErrCorrupt,
		)
	}
	return existing, nil
}

func (repository *Repository) loadExecutionHostFailure(
	intent executionIntent,
) (ExecutionHostFailureObservation, bool, error) {
	objectID := executionHostFailureObjectID(
		Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID},
		intent.IdempotencyKey,
	)
	var observation ExecutionHostFailureObservation
	if err := repository.store.GetJSON(objectID, &observation); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ExecutionHostFailureObservation{}, false, nil
		}
		return ExecutionHostFailureObservation{}, false, fmt.Errorf(
			"%w: load execution host failure observation: %v",
			ErrCorrupt,
			err,
		)
	}
	if err := validateExecutionHostFailureObservation(intent, observation); err != nil {
		return ExecutionHostFailureObservation{}, false, fmt.Errorf(
			"%w: validate execution host failure observation: %v",
			ErrCorrupt,
			err,
		)
	}
	return observation, true, nil
}

// validateExecutionHostFailureDirectory treats directory entries as untrusted
// discovery hints. Every object must bind its exact path and a real immutable
// intent, even when it belongs to another scope, so forged paths and orphan
// observations cannot be hidden by scope filtering.
func (repository *Repository) validateExecutionHostFailureDirectory(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	directory := filepath.Join(
		repository.store.Root(),
		"immutable",
		filepath.FromSlash(strings.TrimSuffix(executionHostFailureRoot, "/")),
	)
	entries, err := readStrictObjectDirectory(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: enumerate execution host failure observations: %v", ErrCorrupt, err)
	}
	if len(entries) > maxExecutionRecords {
		return fmt.Errorf("execution host failure observations exceed %d records", maxExecutionRecords)
	}
	for _, name := range entries {
		if err := contextError(ctx); err != nil {
			return err
		}
		objectID := executionHostFailureRoot + strings.TrimSuffix(name, ".json")
		var observation ExecutionHostFailureObservation
		if err := repository.store.GetJSON(objectID, &observation); err != nil {
			return fmt.Errorf("%w: load execution host failure observation: %v", ErrCorrupt, err)
		}
		if executionHostFailureObjectID(observation.Scope, observation.IdempotencyKey) != objectID {
			return fmt.Errorf(
				"%w: execution host failure path does not bind its scope and idempotency key",
				ErrCorrupt,
			)
		}
		var intent executionIntent
		if err := repository.store.GetJSON(
			executionIntentObjectID(observation.Scope, observation.IdempotencyKey),
			&intent,
		); err != nil {
			return fmt.Errorf("%w: execution host failure has no valid intent: %v", ErrCorrupt, err)
		}
		if err := validateExecutionIntent(intent); err != nil {
			return fmt.Errorf("%w: execution host failure intent: %v", ErrCorrupt, err)
		}
		if err := validateExecutionHostFailureObservation(intent, observation); err != nil {
			return fmt.Errorf("%w: execution host failure observation: %v", ErrCorrupt, err)
		}
	}
	return nil
}

func validateExecutionHostFailureObservation(
	intent executionIntent,
	observation ExecutionHostFailureObservation,
) error {
	if observation.SchemaVersion != ExecutionHostFailureObservationSchemaVersion {
		return fmt.Errorf("unsupported execution host failure schema %q", observation.SchemaVersion)
	}
	intentScope := Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID}
	if err := validateScope(observation.Scope); err != nil {
		return err
	}
	if observation.Scope != intentScope ||
		observation.IdempotencyKey != intent.IdempotencyKey ||
		observation.SemanticDigest != intent.SemanticDigest ||
		observation.ExecutionID != intent.Plan.ExecutionID {
		return fmt.Errorf("execution host failure does not bind its exact intent")
	}
	if err := validateIdempotencyKey(observation.IdempotencyKey); err != nil {
		return err
	}
	if !lowerSHA256(observation.SemanticDigest) || !lowerSHA256(observation.IntentSHA256) {
		return fmt.Errorf("execution host failure digests must be lowercase SHA-256")
	}
	intentSHA256, err := canonicalObjectSHA256(intent)
	if err != nil {
		return err
	}
	if observation.IntentSHA256 != intentSHA256 {
		return fmt.Errorf("execution host failure intent digest changed")
	}
	if err := validateExecutionHostFailureTaxonomy(observation.Stage, observation.ReasonCode); err != nil {
		return err
	}
	if observation.ObservedAt.IsZero() || observation.ObservedAt.Before(intent.AcceptedAt) {
		return fmt.Errorf("execution host failure has invalid observed_at")
	}
	_, offset := observation.ObservedAt.Zone()
	if offset != 0 {
		return fmt.Errorf("execution host failure observed_at must use UTC")
	}
	switch observation.TimeSource {
	case ExecutionHostObservationClock:
	case ExecutionHostObservationAcceptedLowerBound:
		if !observation.ObservedAt.Equal(intent.AcceptedAt) {
			return fmt.Errorf("accepted_at lower-bound observation changed time")
		}
	default:
		return fmt.Errorf("unsupported execution host observation time source %q", observation.TimeSource)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		return fmt.Errorf("encode execution host failure observation: %w", err)
	}
	if len(data) > maxExecutionHostFailureBytes {
		return fmt.Errorf("execution host failure observation exceeds %d bytes", maxExecutionHostFailureBytes)
	}
	return nil
}

func validateExecutionHostFailureTaxonomy(
	stage ExecutionHostFailureStage,
	reasonCode ExecutionHostFailureCode,
) error {
	want := map[ExecutionHostFailureStage]ExecutionHostFailureCode{
		ExecutionHostFailureWorkerRun:        ExecutionHostFailureRunnerUnconfirmed,
		ExecutionHostFailureResultDecode:     ExecutionHostFailureResultRejected,
		ExecutionHostFailureResultBinding:    ExecutionHostFailureBindingRejected,
		ExecutionHostFailureEvidenceMapping:  ExecutionHostFailureMappingRejected,
		ExecutionHostFailureEvidenceEncoding: ExecutionHostFailureEncodingFailed,
		ExecutionHostFailureShadowImport:     ExecutionHostFailureImportUnconfirmed,
		ExecutionHostFailureCompletionCommit: ExecutionHostFailureCompletionUnconfirmed,
	}
	if expected, exists := want[stage]; !exists || expected != reasonCode {
		return fmt.Errorf("unsupported execution host failure taxonomy %q/%q", stage, reasonCode)
	}
	return nil
}

func executionHostFailureObjectID(scope Scope, idempotencyKey string) string {
	return executionHostFailureRoot + digestStrings(
		scope.TenantID,
		scope.WorkspaceID,
		idempotencyKey,
	)
}
