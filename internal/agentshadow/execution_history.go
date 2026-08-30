package agentshadow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const maxExecutionRecords = 100_000

// QueryExecution returns the execution attempt visible to the requested
// scope. Execution IDs in other scopes are intentionally indistinguishable
// from missing executions.
func (service *Service) QueryExecution(
	ctx context.Context,
	request ExecutionQueryRequest,
) (ExecutionAttempt, error) {
	if service == nil || service.repository == nil {
		return ExecutionAttempt{}, fmt.Errorf("agent review shadow service is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return ExecutionAttempt{}, err
	}
	if err := validateScope(request.Scope); err != nil {
		return ExecutionAttempt{}, err
	}
	if err := validateIdentifier("execution_id", request.ExecutionID); err != nil {
		return ExecutionAttempt{}, err
	}
	intents, err := service.repository.listExecutionIntents(ctx, request.Scope)
	if err != nil {
		return ExecutionAttempt{}, err
	}
	if err := service.repository.validateExecutionHostFailureDirectory(ctx); err != nil {
		return ExecutionAttempt{}, err
	}
	if err := service.repository.validateExecutionImportAuthorizationDirectory(ctx); err != nil {
		return ExecutionAttempt{}, err
	}
	var matched *executionIntent
	for index := range intents {
		if intents[index].Plan.ExecutionID != request.ExecutionID {
			continue
		}
		if matched != nil {
			return ExecutionAttempt{}, fmt.Errorf(
				"%w: execution id %q is bound to multiple intents",
				ErrCorrupt,
				request.ExecutionID,
			)
		}
		matched = &intents[index]
	}
	if matched == nil {
		return ExecutionAttempt{}, ErrNotFound
	}
	attempt, _, err := service.projectExecutionAttempt(ctx, *matched)
	return attempt, err
}

// ListExecutions returns strict attempt projections ordered by host-observed
// time and execution ID. Every succeeded attempt is resolved through Query,
// which revalidates the complete manifest, artifact, frozen-input, receipt,
// and observation closure before it becomes visible here.
func (service *Service) ListExecutions(
	ctx context.Context,
	request ExecutionListRequest,
) ([]ExecutionAttempt, error) {
	if service == nil || service.repository == nil {
		return nil, fmt.Errorf("agent review shadow service is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	intents, err := service.repository.listExecutionIntents(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	if err := service.repository.validateExecutionHostFailureDirectory(ctx); err != nil {
		return nil, err
	}
	if err := service.repository.validateExecutionImportAuthorizationDirectory(ctx); err != nil {
		return nil, err
	}
	attempts := make([]ExecutionAttempt, 0, len(intents))
	seenExecutionIDs := make(map[string]struct{}, len(intents))
	for _, intent := range intents {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		if _, exists := seenExecutionIDs[intent.Plan.ExecutionID]; exists {
			return nil, fmt.Errorf(
				"%w: execution id %q is bound to multiple intents",
				ErrCorrupt,
				intent.Plan.ExecutionID,
			)
		}
		seenExecutionIDs[intent.Plan.ExecutionID] = struct{}{}
		attempt, _, err := service.projectExecutionAttempt(ctx, intent)
		if err != nil {
			return nil, fmt.Errorf(
				"project execution %q: %w",
				intent.Plan.ExecutionID,
				err,
			)
		}
		if attempt.ObservedAt.Before(request.StartInclusive) ||
			!attempt.ObservedAt.Before(request.EndExclusive) {
			continue
		}
		attempts = append(attempts, attempt)
	}
	slices.SortFunc(attempts, func(left, right ExecutionAttempt) int {
		if compared := left.ObservedAt.Compare(right.ObservedAt); compared != 0 {
			return compared
		}
		return strings.Compare(left.ExecutionID, right.ExecutionID)
	})
	return attempts, nil
}

func (request ExecutionListRequest) Validate() error {
	if err := validateScope(request.Scope); err != nil {
		return err
	}
	for name, value := range map[string]time.Time{
		"start_inclusive": request.StartInclusive,
		"end_exclusive":   request.EndExclusive,
	} {
		if value.IsZero() {
			return fmt.Errorf("%s is required", name)
		}
		_, offset := value.Zone()
		if offset != 0 {
			return fmt.Errorf("%s must use UTC", name)
		}
	}
	if !request.StartInclusive.Before(request.EndExclusive) {
		return fmt.Errorf("execution observation window must be non-empty and increasing")
	}
	return nil
}

// listExecutionIntents treats filenames as untrusted discovery hints. Each
// object is strictly decoded and must recompute to its exact immutable path
// before scope filtering, so forged paths cannot smuggle another scope into a
// caller's result. readStrictObjectDirectory rejects symlinks and unexpected
// directory entries.
func (repository *Repository) listExecutionIntents(
	ctx context.Context,
	scope Scope,
) ([]executionIntent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	directory := filepath.Join(
		repository.store.Root(),
		"immutable",
		filepath.FromSlash(strings.TrimSuffix(executionIntentRoot, "/")),
	)
	entries, err := readStrictObjectDirectory(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []executionIntent{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: enumerate worker execution intents: %v", ErrCorrupt, err)
	}
	if len(entries) > maxExecutionRecords {
		return nil, fmt.Errorf("worker execution intents exceed %d records", maxExecutionRecords)
	}
	intents := make([]executionIntent, 0, len(entries))
	for _, name := range entries {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		objectID := executionIntentRoot + strings.TrimSuffix(name, ".json")
		var intent executionIntent
		if err := repository.store.GetJSON(objectID, &intent); err != nil {
			return nil, fmt.Errorf("%w: load worker execution intent: %v", ErrCorrupt, err)
		}
		if err := validateExecutionIntent(intent); err != nil {
			return nil, fmt.Errorf("%w: validate worker execution intent: %v", ErrCorrupt, err)
		}
		intentScope := Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID}
		if executionIntentObjectID(intentScope, intent.IdempotencyKey) != objectID {
			return nil, fmt.Errorf("%w: worker execution intent path does not bind its scope and idempotency key", ErrCorrupt)
		}
		if intentScope != scope {
			continue
		}
		intents = append(intents, intent)
	}
	slices.SortFunc(intents, func(left, right executionIntent) int {
		if compared := left.AcceptedAt.Compare(right.AcceptedAt); compared != 0 {
			return compared
		}
		return strings.Compare(left.Plan.ExecutionID, right.Plan.ExecutionID)
	})
	return intents, nil
}

// projectExecutionAttempt resolves a single already-authorized intent. The
// optional Result is returned only for a succeeded completion and lets the
// bridge reuse the same strict closure validation without querying twice.
func (service *Service) projectExecutionAttempt(
	ctx context.Context,
	intent executionIntent,
) (ExecutionAttempt, *Result, error) {
	if err := contextError(ctx); err != nil {
		return ExecutionAttempt{}, nil, err
	}
	if err := validateExecutionIntent(intent); err != nil {
		return ExecutionAttempt{}, nil, fmt.Errorf("%w: validate worker execution intent: %v", ErrCorrupt, err)
	}
	intentSHA256, err := canonicalObjectSHA256(intent)
	if err != nil {
		return ExecutionAttempt{}, nil, fmt.Errorf("canonicalize worker execution intent: %w", err)
	}
	attempt := ExecutionAttempt{
		SchemaVersion:  ExecutionAttemptSchemaVersion,
		Scope:          Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID},
		Status:         ExecutionStatusUnknownOutcome,
		IdempotencyKey: intent.IdempotencyKey,
		SemanticDigest: intent.SemanticDigest,
		IntentSHA256:   intentSHA256,
		Plan:           intent.Plan,
		PlanID:         intent.Plan.PlanID,
		SourceRunID:    intent.Plan.SourceRunID,
		ExecutionID:    intent.Plan.ExecutionID,
		ReviewRunID:    intent.Plan.ReviewRunID,
		Runtime: ExecutionRuntimeDigests{
			WorkerRequestSHA256: intent.WorkerRequestSHA256,
			NodeSHA256:          intent.NodeSHA256,
			WorkerScriptSHA256:  intent.WorkerScriptSHA256,
			WorkerPackageSHA256: intent.WorkerPackageSHA256,
		},
		AcceptedAt: intent.AcceptedAt,
		ObservedAt: intent.AcceptedAt,
	}
	hostFailure, hasHostFailure, err := service.repository.loadExecutionHostFailure(intent)
	if err != nil {
		return ExecutionAttempt{}, nil, err
	}
	if hasHostFailure {
		hostFailureSHA256, err := canonicalObjectSHA256(hostFailure)
		if err != nil {
			return ExecutionAttempt{}, nil, fmt.Errorf("canonicalize execution host failure observation: %w", err)
		}
		attempt.HostFailure = &hostFailure
		attempt.HostFailureObservationSHA256 = &hostFailureSHA256
		attempt.ObservedAt = hostFailure.ObservedAt
	}
	completion, exists, err := service.repository.loadExecutionCompletion(intent)
	if err != nil {
		return ExecutionAttempt{}, nil, err
	}
	if !exists {
		if err := validateExecutionAttempt(attempt); err != nil {
			return ExecutionAttempt{}, nil, fmt.Errorf("%w: project worker execution attempt: %v", ErrCorrupt, err)
		}
		return attempt, nil, nil
	}
	completionSHA256, err := canonicalObjectSHA256(completion)
	if err != nil {
		return ExecutionAttempt{}, nil, fmt.Errorf("canonicalize worker execution completion: %w", err)
	}
	attempt.CompletionSHA256 = &completionSHA256
	completedAt := completion.CompletedAt
	attempt.CompletedAt = &completedAt
	// Window ownership is host-observed. Worker CompletedAt remains available
	// as non-attested execution evidence but cannot move an attempt between
	// diagnostic windows.
	attempt.ObservedAt = completion.RecordedAt
	switch completion.Status {
	case contractsv1alpha1.AgentReviewWorkerSucceeded:
		attempt.Status = ExecutionStatusSucceeded
		result, err := service.Query(ctx, QueryRequest{
			Scope: attempt.Scope, ManifestID: completion.ManifestID,
		})
		if err != nil {
			return ExecutionAttempt{}, nil, fmt.Errorf("revalidate succeeded execution manifest: %w", err)
		}
		if result.Record.TenantID != intent.TenantID ||
			result.Record.WorkspaceID != intent.WorkspaceID ||
			result.Record.IdempotencyKey != intent.IdempotencyKey ||
			result.Manifest.ManifestID != completion.ManifestID ||
			!reflect.DeepEqual(result.Plan, intent.Plan) {
			return ExecutionAttempt{}, nil, fmt.Errorf(
				"%w: succeeded execution manifest does not bind its exact intent",
				ErrCorrupt,
			)
		}
		authorization, hasAuthorization, err := service.repository.loadExecutionImportAuthorization(intent)
		if err != nil {
			return ExecutionAttempt{}, nil, err
		}
		if !hasAuthorization {
			return ExecutionAttempt{}, nil, fmt.Errorf(
				"%w: succeeded execution has no LocalBridge import authorization",
				ErrCorrupt,
			)
		}
		if err := validateAuthorizedCommittedResult(intent, authorization, result); err != nil {
			return ExecutionAttempt{}, nil, fmt.Errorf(
				"%w: succeeded execution changed from its import authorization: %v",
				ErrCorrupt,
				err,
			)
		}
		if result.Record.AcceptedAt.After(completion.RecordedAt) {
			return ExecutionAttempt{}, nil, fmt.Errorf(
				"%w: succeeded result commit is later than its host completion observation",
				ErrCorrupt,
			)
		}
		manifest := result.Manifest
		attempt.Manifest = &manifest
		if err := validateExecutionAttempt(attempt); err != nil {
			return ExecutionAttempt{}, nil, fmt.Errorf("%w: project worker execution attempt: %v", ErrCorrupt, err)
		}
		return attempt, &result, nil
	case contractsv1alpha1.AgentReviewWorkerFailed:
		attempt.Status = ExecutionStatusFailed
	case contractsv1alpha1.AgentReviewWorkerCanceled:
		attempt.Status = ExecutionStatusCanceled
	default:
		return ExecutionAttempt{}, nil, fmt.Errorf("%w: unsupported worker completion status %q", ErrCorrupt, completion.Status)
	}
	failure := *completion.Failure
	attempt.Failure = &failure
	if err := validateExecutionAttempt(attempt); err != nil {
		return ExecutionAttempt{}, nil, fmt.Errorf("%w: project worker execution attempt: %v", ErrCorrupt, err)
	}
	return attempt, nil, nil
}

func validateExecutionAttempt(attempt ExecutionAttempt) error {
	if attempt.SchemaVersion != ExecutionAttemptSchemaVersion {
		return fmt.Errorf("unsupported execution attempt schema %q", attempt.SchemaVersion)
	}
	if err := validateScope(attempt.Scope); err != nil {
		return err
	}
	if err := validateIdempotencyKey(attempt.IdempotencyKey); err != nil {
		return err
	}
	if err := attempt.Plan.Validate(); err != nil {
		return fmt.Errorf("validate attempt plan: %w", err)
	}
	if attempt.PlanID != attempt.Plan.PlanID ||
		attempt.SourceRunID != attempt.Plan.SourceRunID ||
		attempt.ExecutionID != attempt.Plan.ExecutionID ||
		attempt.ReviewRunID != attempt.Plan.ReviewRunID {
		return fmt.Errorf("execution attempt identities do not bind its plan")
	}
	for name, value := range map[string]string{
		"semantic_digest":       attempt.SemanticDigest,
		"intent_sha256":         attempt.IntentSHA256,
		"worker_request_sha256": attempt.Runtime.WorkerRequestSHA256,
		"node_sha256":           attempt.Runtime.NodeSHA256,
		"worker_script_sha256":  attempt.Runtime.WorkerScriptSHA256,
		"worker_package_sha256": attempt.Runtime.WorkerPackageSHA256,
	} {
		if !lowerSHA256(value) {
			return fmt.Errorf("%s must be a lowercase SHA-256", name)
		}
	}
	if attempt.AcceptedAt.IsZero() || !attempt.AcceptedAt.Equal(attempt.Plan.CreatedAt) {
		return fmt.Errorf("execution attempt has invalid accepted_at")
	}
	if attempt.ObservedAt.IsZero() {
		return fmt.Errorf("execution attempt observed_at is required")
	}
	for name, value := range map[string]time.Time{
		"accepted_at": attempt.AcceptedAt,
		"observed_at": attempt.ObservedAt,
	} {
		_, offset := value.Zone()
		if offset != 0 {
			return fmt.Errorf("%s must use UTC", name)
		}
	}
	if (attempt.HostFailure == nil) != (attempt.HostFailureObservationSHA256 == nil) {
		return fmt.Errorf("execution host failure observation and digest must be present together")
	}
	if attempt.HostFailure != nil {
		if attempt.HostFailure.SchemaVersion != ExecutionHostFailureObservationSchemaVersion {
			return fmt.Errorf("unsupported execution host failure schema %q", attempt.HostFailure.SchemaVersion)
		}
		if !lowerSHA256(*attempt.HostFailureObservationSHA256) {
			return fmt.Errorf("host failure observation digest must be a lowercase SHA-256")
		}
		if attempt.HostFailure.Scope != attempt.Scope ||
			attempt.HostFailure.IdempotencyKey != attempt.IdempotencyKey ||
			attempt.HostFailure.SemanticDigest != attempt.SemanticDigest ||
			attempt.HostFailure.ExecutionID != attempt.ExecutionID ||
			attempt.HostFailure.IntentSHA256 != attempt.IntentSHA256 {
			return fmt.Errorf("execution host failure observation does not bind the attempt")
		}
		if err := validateExecutionHostFailureTaxonomy(
			attempt.HostFailure.Stage,
			attempt.HostFailure.ReasonCode,
		); err != nil {
			return err
		}
		if attempt.HostFailure.ObservedAt.IsZero() ||
			attempt.HostFailure.ObservedAt.Before(attempt.AcceptedAt) {
			return fmt.Errorf("execution host failure observation has invalid time")
		}
		_, offset := attempt.HostFailure.ObservedAt.Zone()
		if offset != 0 {
			return fmt.Errorf("execution host failure observed_at must use UTC")
		}
		switch attempt.HostFailure.TimeSource {
		case ExecutionHostObservationClock:
		case ExecutionHostObservationAcceptedLowerBound:
			if !attempt.HostFailure.ObservedAt.Equal(attempt.AcceptedAt) {
				return fmt.Errorf("execution host failure lower-bound time changed")
			}
		default:
			return fmt.Errorf("unsupported execution host observation time source %q", attempt.HostFailure.TimeSource)
		}
		observationSHA256, err := canonicalObjectSHA256(*attempt.HostFailure)
		if err != nil || observationSHA256 != *attempt.HostFailureObservationSHA256 {
			return fmt.Errorf("execution host failure observation digest changed")
		}
	}
	if attempt.Status == ExecutionStatusUnknownOutcome {
		if attempt.CompletionSHA256 != nil || attempt.CompletedAt != nil ||
			attempt.Failure != nil || attempt.Manifest != nil {
			return fmt.Errorf("unknown outcome must not expose terminal evidence")
		}
		wantObservedAt := attempt.AcceptedAt
		if attempt.HostFailure != nil {
			wantObservedAt = attempt.HostFailure.ObservedAt
		}
		if !attempt.ObservedAt.Equal(wantObservedAt) {
			return fmt.Errorf("unknown outcome has invalid host observation time")
		}
		return nil
	}
	if attempt.CompletionSHA256 == nil || !lowerSHA256(*attempt.CompletionSHA256) ||
		attempt.CompletedAt == nil || attempt.ObservedAt.Before(attempt.AcceptedAt) ||
		attempt.CompletedAt.Before(attempt.AcceptedAt) ||
		attempt.CompletedAt.After(attempt.ObservedAt) {
		return fmt.Errorf("terminal execution attempt has invalid completion evidence")
	}
	_, offset := attempt.CompletedAt.Zone()
	if offset != 0 {
		return fmt.Errorf("completed_at must use UTC")
	}
	switch attempt.Status {
	case ExecutionStatusSucceeded:
		if attempt.Failure != nil || attempt.Manifest == nil {
			return fmt.Errorf("succeeded execution attempt requires manifest only")
		}
		if attempt.CompletedAt.After(attempt.AcceptedAt.Add(
			time.Duration(attempt.Plan.Budget.TimeoutMS) * time.Millisecond,
		)) {
			return fmt.Errorf("succeeded execution attempt exceeded its deadline")
		}
		if attempt.Manifest.PlanID != attempt.PlanID ||
			attempt.Manifest.SourceRunID != attempt.SourceRunID ||
			attempt.Manifest.ExecutionID != attempt.ExecutionID ||
			attempt.Manifest.ReviewRunID != attempt.ReviewRunID {
			return fmt.Errorf("execution attempt manifest identities changed")
		}
	case ExecutionStatusFailed, ExecutionStatusCanceled:
		if attempt.Manifest != nil || attempt.Failure == nil {
			return fmt.Errorf("failed/canceled execution attempt requires failure only")
		}
		if attempt.Failure.Message != redactedWorkerFailureMessage {
			return fmt.Errorf("execution attempt failure is not redacted")
		}
	default:
		return fmt.Errorf("unsupported execution attempt status %q", attempt.Status)
	}
	return nil
}

// Validate lets downstream diagnostic adapters reject forged or stale
// projections without copying the shadow repository's closure rules.
func (attempt ExecutionAttempt) Validate() error {
	return validateExecutionAttempt(attempt)
}

func canonicalObjectSHA256(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBytesRaw(data), nil
}
