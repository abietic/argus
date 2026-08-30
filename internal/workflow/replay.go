package workflow

import (
	"fmt"
	"reflect"
	"slices"
)

// RuntimeLimits is the frozen outer runtime policy that tightens every stage
// workflow ceiling. A workflow experiment must change the effective scheduler
// behavior under these exact limits, not merely move an inactive ceiling.
type RuntimeLimits struct {
	TimeoutMS      int64
	MaxInputBytes  int64
	MaxOutputBytes int64
	MaxAttempts    int
	MaxConcurrency int
}

// DiffReplayExecutionPolicy admits a single workflow-version experiment while
// freezing the executable graph and authority. Only fields consumed by the
// deterministic stage scheduler may vary. It returns exact sorted field paths
// and the earliest affected stage so callers cannot reuse an affected prefix.
func DiffReplayExecutionPolicy(
	baseline Definition,
	variant Definition,
) ([]string, string, error) {
	if err := baseline.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate baseline workflow: %w", err)
	}
	if err := variant.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate variant workflow: %w", err)
	}
	if baseline.ID != variant.ID {
		return nil, "", fmt.Errorf("workflow replay must preserve workflow_id")
	}
	if baseline.Revision == variant.Revision {
		return nil, "", fmt.Errorf("workflow replay must use a new revision")
	}
	if len(baseline.Stages) != len(variant.Stages) {
		return nil, "", fmt.Errorf("workflow replay cannot add or remove stages")
	}

	fields := make([]string, 0)
	earliest := ""
	for index := range baseline.Stages {
		left := baseline.Stages[index]
		right := variant.Stages[index]
		leftPolicy := left
		rightPolicy := right
		leftPolicy.Budget, rightPolicy.Budget = StageBudget{}, StageBudget{}
		leftPolicy.Retry.MaxAttempts, rightPolicy.Retry.MaxAttempts = 0, 0
		leftPolicy.Retry.BackoffMS, rightPolicy.Retry.BackoffMS = 0, 0
		leftPolicy.Retry.RetryableCodes, rightPolicy.Retry.RetryableCodes = nil, nil
		if !reflect.DeepEqual(leftPolicy, rightPolicy) {
			return nil, "", fmt.Errorf(
				"workflow replay changed immutable graph, executor, authority, contract, or failure semantics at stage %q",
				left.ID,
			)
		}
		prefix := "workflow.stages." + left.ID
		before := len(fields)
		if left.Budget.TimeoutMS != right.Budget.TimeoutMS {
			fields = append(fields, prefix+".budget.timeout_ms")
		}
		if left.Budget.MaxInputBytes != right.Budget.MaxInputBytes {
			fields = append(fields, prefix+".budget.max_input_bytes")
		}
		if left.Budget.MaxOutputBytes != right.Budget.MaxOutputBytes {
			fields = append(fields, prefix+".budget.max_output_bytes")
		}
		if left.Budget.MaxConcurrency != right.Budget.MaxConcurrency {
			fields = append(fields, prefix+".budget.max_concurrency")
		}
		if left.Retry.MaxAttempts != right.Retry.MaxAttempts {
			fields = append(fields, prefix+".retry.max_attempts")
		}
		if left.Retry.BackoffMS != right.Retry.BackoffMS {
			fields = append(fields, prefix+".retry.backoff_ms")
		}
		if !slices.Equal(left.Retry.RetryableCodes, right.Retry.RetryableCodes) {
			fields = append(fields, prefix+".retry.retryable_codes")
		}
		if len(fields) > before && earliest == "" {
			earliest = left.ID
		}
	}
	if len(fields) == 0 {
		return nil, "", fmt.Errorf(
			"workflow replay changes only identity or fields not consumed by the scheduler",
		)
	}
	slices.Sort(fields)
	return fields, earliest, nil
}

// ReplayChangesEffectivePolicy reports whether an admitted workflow diff can
// affect execution or its retry evidence under the frozen outer limits.
func ReplayChangesEffectivePolicy(
	baseline Definition,
	variant Definition,
	limits RuntimeLimits,
) bool {
	for index := range baseline.Stages {
		left := baseline.Stages[index]
		right := variant.Stages[index]
		if min(left.Budget.TimeoutMS, limits.TimeoutMS) !=
			min(right.Budget.TimeoutMS, limits.TimeoutMS) ||
			min(left.Budget.MaxInputBytes, limits.MaxInputBytes) !=
				min(right.Budget.MaxInputBytes, limits.MaxInputBytes) ||
			min(left.Budget.MaxOutputBytes, limits.MaxOutputBytes) !=
				min(right.Budget.MaxOutputBytes, limits.MaxOutputBytes) ||
			min(left.Budget.MaxConcurrency, limits.MaxConcurrency) !=
				min(right.Budget.MaxConcurrency, limits.MaxConcurrency) ||
			min(left.Retry.MaxAttempts, limits.MaxAttempts) !=
				min(right.Retry.MaxAttempts, limits.MaxAttempts) ||
			!slices.Equal(left.Retry.RetryableCodes, right.Retry.RetryableCodes) {
			return true
		}
		effectiveAttempts := min(right.Retry.MaxAttempts, limits.MaxAttempts)
		if left.Retry.BackoffMS != right.Retry.BackoffMS &&
			effectiveAttempts > 1 && len(right.Retry.RetryableCodes) > 0 {
			return true
		}
	}
	return false
}
