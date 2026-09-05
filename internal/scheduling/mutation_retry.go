package scheduling

import (
	"context"
	"errors"
	"fmt"
)

var errProjectionAdvanced = errors.New("scheduling projection advanced concurrently")

const maximumProjectionRetries = 64

// Every mutation is computed from one ledger sequence and atomically appended
// at that sequence. Process-local locks alone cannot protect concurrent CLI
// workers. Only a sequence conflict is retryable: intent conflicts and rejected
// transitions retain their original meaning.
func retryProjection[T any](ctx context.Context, mutate func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt < maximumProjectionRetries; attempt++ {
		if err := contextError(ctx); err != nil {
			return zero, err
		}
		value, err := mutate()
		if !errors.Is(err, errProjectionAdvanced) {
			return value, err
		}
	}
	return zero, fmt.Errorf("%w: concurrent scheduling updates exhausted %d projection retries", ErrConflict, maximumProjectionRetries)
}

func (repository *Repository) Submit(ctx context.Context, spec WorkloadSpec, mutation Mutation) (WorkloadRecord, error) {
	return retryProjection(ctx, func() (WorkloadRecord, error) { return repository.submitOnce(ctx, spec, mutation) })
}

func (repository *Repository) Claim(ctx context.Context, request ClaimRequest) (Dispatch, error) {
	return retryProjection(ctx, func() (Dispatch, error) { return repository.claimOnce(ctx, request) })
}

func (repository *Repository) Heartbeat(ctx context.Context, heartbeat Heartbeat) (WorkloadRecord, error) {
	return retryProjection(ctx, func() (WorkloadRecord, error) { return repository.heartbeatOnce(ctx, heartbeat) })
}

// Complete persists accepted and fenced callbacks; an exact retry of a rejected
// callback still returns ErrFenced with the original rejection evidence.
func (repository *Repository) Complete(ctx context.Context, callback Callback) (WorkloadRecord, error) {
	return retryProjection(ctx, func() (WorkloadRecord, error) { return repository.completeOnce(ctx, callback) })
}

func (repository *Repository) CancelRun(ctx context.Context, runID, reason string, mutation Mutation) ([]WorkloadRecord, error) {
	return retryProjection(ctx, func() ([]WorkloadRecord, error) { return repository.cancelRunOnce(ctx, runID, reason, mutation) })
}

func (repository *Repository) reconcile(ctx context.Context, ids []string, mutation Mutation) ([]WorkloadRecord, error) {
	return retryProjection(ctx, func() ([]WorkloadRecord, error) { return repository.reconcileOnce(ctx, ids, mutation) })
}
