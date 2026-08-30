package agentanalytics

import (
	"context"
	"fmt"
	"reflect"

	"argus.local/argus/internal/agentshadow"
)

// executionAttemptBatchSource is mandatory once a source advertises attempt
// history. Falling back to QueryExecution would rescan the entire immutable
// intent directory once per committed result and makes rebuild complexity
// depend on results*intents.
type executionAttemptBatchSource interface {
	QueryExecutions(
		context.Context,
		agentshadow.ExecutionBatchQueryRequest,
	) (agentshadow.ExecutionBatchQueryResult, error)
}

type executionBatchIndex struct {
	attempts map[string]agentshadow.ExecutionAttempt
	missing  map[string]struct{}
}

func (adapter *Adapter) resolveSucceededAttemptResults(
	ctx context.Context,
	request RebuildRequest,
	results []agentshadow.Result,
	attempts []agentshadow.ExecutionAttempt,
) ([]agentshadow.Result, error) {
	batchSource, canBatchQuery := adapter.source.(executionAttemptBatchSource)
	if !canBatchQuery {
		return nil, fmt.Errorf(
			"attempt-aware shadow source must implement bounded batch execution lookup",
		)
	}
	querySource, canQueryResult := adapter.source.(committedResultQuerySource)

	currentAttempts := make(map[string]agentshadow.ExecutionAttempt, len(attempts))
	for index, attempt := range attempts {
		if err := validateExecutionAttempt(request, attempt); err != nil {
			return nil, fmt.Errorf("execution attempts[%d]: %w", index, err)
		}
		if _, duplicate := currentAttempts[attempt.ExecutionID]; duplicate {
			return nil, fmt.Errorf(
				"execution attempts[%d] duplicates execution_id %q",
				index,
				attempt.ExecutionID,
			)
		}
		currentAttempts[attempt.ExecutionID] = attempt
	}

	// Only results not already owned by a current-window attempt need a global
	// lookup. De-duplicating IDs here keeps the batch request strict while the
	// regular fact builder remains responsible for rejecting duplicate result
	// commits with complete manifest evidence.
	lookupIDs := make([]string, 0, len(results))
	seenLookupIDs := make(map[string]struct{}, len(results))
	for _, result := range results {
		executionID := result.Plan.ExecutionID
		if _, current := currentAttempts[executionID]; current {
			continue
		}
		if _, duplicate := seenLookupIDs[executionID]; duplicate {
			continue
		}
		seenLookupIDs[executionID] = struct{}{}
		lookupIDs = append(lookupIDs, executionID)
	}

	global := executionBatchIndex{
		attempts: make(map[string]agentshadow.ExecutionAttempt, len(lookupIDs)),
		missing:  make(map[string]struct{}, len(lookupIDs)),
	}
	if len(lookupIDs) > 0 {
		response, err := batchSource.QueryExecutions(
			ctx,
			agentshadow.ExecutionBatchQueryRequest{
				Scope: agentshadow.Scope{
					TenantID:    request.Scope.TenantID,
					WorkspaceID: request.Scope.WorkspaceID,
				},
				ExecutionIDs: lookupIDs,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("batch query shadow executions: %w", err)
		}
		global, err = indexExecutionBatchResponse(request.Scope, lookupIDs, response)
		if err != nil {
			return nil, fmt.Errorf("validate batch execution lookup: %w", err)
		}
	}

	resolved := make([]agentshadow.Result, 0, len(results))
	resultByExecutionID := make(map[string]struct{}, len(results))
	for _, result := range results {
		executionID := result.Plan.ExecutionID
		if currentAttempt, current := currentAttempts[executionID]; current {
			if currentAttempt.Status == agentshadow.ExecutionStatusSucceeded {
				resolved = append(resolved, result)
				resultByExecutionID[executionID] = struct{}{}
			}
			// A current non-succeeded attempt owns the execution observation;
			// an import crash window must not fabricate terminal success.
			continue
		}

		globalAttempt, hasIntent := global.attempts[executionID]
		if !hasIntent {
			if _, legacy := global.missing[executionID]; !legacy {
				return nil, fmt.Errorf(
					"batch execution lookup omitted execution %q",
					executionID,
				)
			}
			// Direct Service.Import results predate execution intents. Their
			// committed observation remains the unique legacy window owner.
			resolved = append(resolved, result)
			resultByExecutionID[executionID] = struct{}{}
			continue
		}
		if !reflect.DeepEqual(globalAttempt.Plan, result.Plan) {
			return nil, fmt.Errorf(
				"batch query execution %q returned another plan",
				executionID,
			)
		}
		switch globalAttempt.Status {
		case agentshadow.ExecutionStatusSucceeded:
			if globalAttempt.Manifest == nil ||
				globalAttempt.Manifest.ManifestID != result.Manifest.ManifestID {
				return nil, fmt.Errorf(
					"execution %q succeeded with another result manifest",
					executionID,
				)
			}
		case agentshadow.ExecutionStatusUnknownOutcome:
			// Import may be committed while terminal completion is absent.
		case agentshadow.ExecutionStatusFailed, agentshadow.ExecutionStatusCanceled:
			return nil, fmt.Errorf(
				"execution %q has a committed result and incompatible terminal status %q",
				executionID,
				globalAttempt.Status,
			)
		default:
			return nil, fmt.Errorf(
				"execution %q has unsupported status %q",
				executionID,
				globalAttempt.Status,
			)
		}
		if request.Window.contains(globalAttempt.ObservedAt) {
			return nil, fmt.Errorf(
				"execution attempt source omitted current-window execution %q",
				executionID,
			)
		}
		// An attempt in another window owns this execution. Its later or
		// earlier import observation must not count the same execution twice.
	}

	for _, attempt := range attempts {
		if attempt.Status != agentshadow.ExecutionStatusSucceeded {
			continue
		}
		if _, exists := resultByExecutionID[attempt.ExecutionID]; exists {
			continue
		}
		if attempt.Manifest == nil {
			return nil, fmt.Errorf(
				"succeeded execution attempt %q has no result manifest",
				attempt.ExecutionID,
			)
		}
		if !canQueryResult {
			return nil, fmt.Errorf(
				"succeeded execution attempt %q is outside the committed result list and the source cannot query it",
				attempt.ExecutionID,
			)
		}
		result, err := querySource.Query(ctx, agentshadow.QueryRequest{
			Scope:      attempt.Scope,
			ManifestID: attempt.Manifest.ManifestID,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"query succeeded execution %q result: %w",
				attempt.ExecutionID,
				err,
			)
		}
		resolved = append(resolved, result)
		resultByExecutionID[result.Plan.ExecutionID] = struct{}{}
	}
	return resolved, nil
}

func indexExecutionBatchResponse(
	scope Scope,
	requestedIDs []string,
	response agentshadow.ExecutionBatchQueryResult,
) (executionBatchIndex, error) {
	requested := make(map[string]struct{}, len(requestedIDs))
	for _, executionID := range requestedIDs {
		if _, duplicate := requested[executionID]; duplicate {
			return executionBatchIndex{}, fmt.Errorf(
				"request duplicates execution id %q",
				executionID,
			)
		}
		requested[executionID] = struct{}{}
	}
	index := executionBatchIndex{
		attempts: make(map[string]agentshadow.ExecutionAttempt, len(response.Attempts)),
		missing:  make(map[string]struct{}, len(response.MissingExecutionIDs)),
	}
	for responseIndex, attempt := range response.Attempts {
		if err := attempt.Validate(); err != nil {
			return executionBatchIndex{}, fmt.Errorf(
				"attempts[%d] closure: %w",
				responseIndex,
				err,
			)
		}
		if attempt.Scope.TenantID != scope.TenantID ||
			attempt.Scope.WorkspaceID != scope.WorkspaceID {
			return executionBatchIndex{}, fmt.Errorf(
				"attempts[%d] escapes requested scope",
				responseIndex,
			)
		}
		if _, wanted := requested[attempt.ExecutionID]; !wanted {
			return executionBatchIndex{}, fmt.Errorf(
				"attempts[%d] returned unrequested execution id %q",
				responseIndex,
				attempt.ExecutionID,
			)
		}
		if _, duplicate := index.attempts[attempt.ExecutionID]; duplicate {
			return executionBatchIndex{}, fmt.Errorf(
				"attempts[%d] duplicates execution id %q",
				responseIndex,
				attempt.ExecutionID,
			)
		}
		index.attempts[attempt.ExecutionID] = attempt
	}
	for responseIndex, executionID := range response.MissingExecutionIDs {
		if _, wanted := requested[executionID]; !wanted {
			return executionBatchIndex{}, fmt.Errorf(
				"missing_execution_ids[%d] returned unrequested execution id %q",
				responseIndex,
				executionID,
			)
		}
		if _, matched := index.attempts[executionID]; matched {
			return executionBatchIndex{}, fmt.Errorf(
				"execution id %q is both matched and missing",
				executionID,
			)
		}
		if _, duplicate := index.missing[executionID]; duplicate {
			return executionBatchIndex{}, fmt.Errorf(
				"missing_execution_ids[%d] duplicates execution id %q",
				responseIndex,
				executionID,
			)
		}
		index.missing[executionID] = struct{}{}
	}
	for _, executionID := range requestedIDs {
		_, matched := index.attempts[executionID]
		_, missing := index.missing[executionID]
		if matched == missing {
			return executionBatchIndex{}, fmt.Errorf(
				"execution id %q must be exactly one of matched or missing",
				executionID,
			)
		}
	}
	return index, nil
}
