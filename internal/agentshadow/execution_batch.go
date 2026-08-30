package agentshadow

import (
	"context"
	"fmt"
)

// ExecutionBatchQueryRequest resolves a bounded set of execution IDs inside
// one explicit scope. ExecutionIDs must be unique so callers cannot create
// ambiguous result cardinality or amplify closure validation accidentally.
type ExecutionBatchQueryRequest struct {
	Scope        Scope
	ExecutionIDs []string
}

// ExecutionBatchQueryResult partitions every requested execution ID into one
// fully validated attempt or the missing set. Attempts and missing IDs retain
// request order. An execution in another scope is deliberately reported as
// missing, exactly like an execution that does not exist.
type ExecutionBatchQueryResult struct {
	Attempts            []ExecutionAttempt
	MissingExecutionIDs []string
}

func (request ExecutionBatchQueryRequest) Validate() error {
	if err := validateScope(request.Scope); err != nil {
		return err
	}
	if len(request.ExecutionIDs) > maxExecutionRecords {
		return fmt.Errorf(
			"execution batch exceeds %d execution ids",
			maxExecutionRecords,
		)
	}
	seen := make(map[string]struct{}, len(request.ExecutionIDs))
	for index, executionID := range request.ExecutionIDs {
		if err := validateIdentifier("execution_id", executionID); err != nil {
			return fmt.Errorf("execution_ids[%d]: %w", index, err)
		}
		if _, duplicate := seen[executionID]; duplicate {
			return fmt.Errorf(
				"execution_ids[%d] duplicates execution id %q",
				index,
				executionID,
			)
		}
		seen[executionID] = struct{}{}
	}
	return nil
}

// QueryExecutions performs one strict intent-directory scan and then resolves
// only the requested, scope-visible intents. Each matched attempt goes through
// projectExecutionAttempt, including the complete result/artifact/frozen-input
// closure for succeeded executions.
func (service *Service) QueryExecutions(
	ctx context.Context,
	request ExecutionBatchQueryRequest,
) (ExecutionBatchQueryResult, error) {
	if service == nil || service.repository == nil {
		return ExecutionBatchQueryResult{}, fmt.Errorf("agent review shadow service is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return ExecutionBatchQueryResult{}, err
	}
	if err := request.Validate(); err != nil {
		return ExecutionBatchQueryResult{}, err
	}
	if len(request.ExecutionIDs) == 0 {
		return ExecutionBatchQueryResult{
			Attempts:            []ExecutionAttempt{},
			MissingExecutionIDs: []string{},
		}, nil
	}

	intents, err := service.repository.listExecutionIntents(ctx, request.Scope)
	if err != nil {
		return ExecutionBatchQueryResult{}, err
	}
	if err := service.repository.validateExecutionHostFailureDirectory(ctx); err != nil {
		return ExecutionBatchQueryResult{}, err
	}
	if err := service.repository.validateExecutionImportAuthorizationDirectory(ctx); err != nil {
		return ExecutionBatchQueryResult{}, err
	}
	requested := make(map[string]struct{}, len(request.ExecutionIDs))
	for _, executionID := range request.ExecutionIDs {
		requested[executionID] = struct{}{}
	}
	matched := make(map[string]executionIntent, len(request.ExecutionIDs))
	for _, intent := range intents {
		executionID := intent.Plan.ExecutionID
		if _, wanted := requested[executionID]; !wanted {
			continue
		}
		if _, duplicate := matched[executionID]; duplicate {
			return ExecutionBatchQueryResult{}, fmt.Errorf(
				"%w: execution id %q is bound to multiple intents",
				ErrCorrupt,
				executionID,
			)
		}
		matched[executionID] = intent
	}

	result := ExecutionBatchQueryResult{
		Attempts:            make([]ExecutionAttempt, 0, len(matched)),
		MissingExecutionIDs: make([]string, 0, len(request.ExecutionIDs)-len(matched)),
	}
	for _, executionID := range request.ExecutionIDs {
		if err := contextError(ctx); err != nil {
			return ExecutionBatchQueryResult{}, err
		}
		intent, exists := matched[executionID]
		if !exists {
			result.MissingExecutionIDs = append(result.MissingExecutionIDs, executionID)
			continue
		}
		attempt, _, err := service.projectExecutionAttempt(ctx, intent)
		if err != nil {
			return ExecutionBatchQueryResult{}, fmt.Errorf(
				"project execution %q: %w",
				executionID,
				err,
			)
		}
		result.Attempts = append(result.Attempts, attempt)
	}
	return result, nil
}
