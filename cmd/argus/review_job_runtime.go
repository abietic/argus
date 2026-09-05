package main

import (
	"context"
	"fmt"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/contextprovider"
	"github.com/abietic/argus/internal/identity"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/reviewshard"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
)

// Both the API and quick use this composition root. ReviewJob owns leases;
// application owns review semantics and never starts a nested scheduler here.
func newLocalReviewJobExecutor(store *local.Store, runs *runrepo.Repository, workloads *scheduling.Repository, workerID string, runner agentshadowworker.Runner) (*localReviewJobExecutor, error) {
	source, err := gitadapter.New()
	if err != nil {
		return nil, err
	}
	shardRepository, err := reviewshard.NewRepository(store)
	if err != nil {
		return nil, err
	}
	contexts, err := contextprovider.NewLocalExecutor(source)
	if err != nil {
		return nil, err
	}
	unclaimed := reviewjob.ExecuteFunc(func(ctx context.Context, command reviewjob.Command) (application.RunOutcome, error) {
		service, err := application.NewService(source, runs, application.ServiceOptions{
			ConfigBundle:  command.ConfigBundle,
			IDs:           &reviewJobIDGenerator{runID: command.RunID, fallback: identity.NewGenerator()},
			BuildIdentity: "argus-" + version, DisableScheduling: true, ContextProviders: contexts,
		})
		if err != nil {
			return application.RunOutcome{}, err
		}
		return service.Review(ctx, command.Request.ApplicationRequest())
	})
	claimed := func(ctx context.Context, command reviewjob.Command, dispatch scheduling.Dispatch) (outcome application.RunOutcome, executeErr error) {
		shards, err := reviewshard.NewScopeExecutor(shardRepository, runs, dispatch, workerID, nil)
		if err != nil {
			return application.RunOutcome{}, err
		}
		service, err := application.NewService(source, runs, application.ServiceOptions{
			ConfigBundle:  command.ConfigBundle,
			IDs:           &reviewJobIDGenerator{runID: command.RunID, fallback: identity.NewGenerator()},
			BuildIdentity: "argus-" + version, DisableScheduling: true, ContextProviders: contexts,
			ScopeShards: shards, ExecutionDispatch: &dispatch,
			ExecutionAuthority: func(ctx context.Context, expected scheduling.Dispatch) error {
				return validateLocalReviewJobAuthority(ctx, workloads, expected)
			},
			ExecutionCancellationAuthority: func(ctx context.Context, expected scheduling.Dispatch) error {
				return validateLocalReviewJobCancellation(ctx, workloads, expected)
			},
		})
		if err != nil {
			return application.RunOutcome{}, err
		}
		defer func() {
			closingContext := context.WithoutCancel(ctx)
			if executeErr == nil || validateLocalReviewJobCancellation(closingContext, workloads, dispatch) != nil {
				return
			}
			// A cancel may race a success write or run.started. Close only the
			// already-persisted lineage, without starting or re-running work.
			if canceled, err := service.CancelReview(closingContext, command.RunID, command.Request.RepositoryPath); err == nil {
				outcome = canceled
			}
		}()
		history, err := runs.History(0)
		if err != nil {
			return application.RunOutcome{}, err
		}
		for _, entry := range history {
			if entry.RunID == command.RunID {
				return service.ResumeReview(ctx, command.RunID, command.Request.RepositoryPath)
			}
		}
		return service.Review(ctx, command.Request.ApplicationRequest())
	}
	return &localReviewJobExecutor{deterministic: unclaimed, deterministicClaimed: claimed,
		runner: runner, storePath: store.Root(), workloads: workloads}, nil
}

func validateLocalReviewJobAuthority(ctx context.Context, workloads *scheduling.Repository, dispatch scheduling.Dispatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := workloads.Get(dispatch.Spec.WorkloadID)
	if err != nil {
		return err
	}
	lease := record.ActiveLease
	if record.State != scheduling.StateLeased || record.RunCanceled || lease == nil ||
		record.Spec.RunID != dispatch.Spec.RunID || lease.LeaseID != dispatch.Lease.LeaseID ||
		lease.WorkerID != dispatch.Lease.WorkerID || lease.Generation != dispatch.Lease.Generation ||
		lease.Attempt != dispatch.Lease.Attempt || lease.FencingToken != dispatch.Lease.FencingToken ||
		!time.Now().Before(lease.ExpiresAt) {
		return fmt.Errorf("%w: review job execution no longer owns its lease", scheduling.ErrFenced)
	}
	return nil
}

// A permanent scheduler cancellation may only close the canceled ReviewRun;
// it cannot authorize another stage, success, or a superseded lease owner.
func validateLocalReviewJobCancellation(ctx context.Context, workloads *scheduling.Repository, dispatch scheduling.Dispatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := workloads.Get(dispatch.Spec.WorkloadID)
	if err != nil {
		return err
	}
	lease := record.LastLease
	if record.State != scheduling.StateCanceled || !record.RunCanceled || record.ActiveLease != nil ||
		record.Spec.RunID != dispatch.Spec.RunID || record.Generation != dispatch.Lease.Generation || lease == nil ||
		lease.LeaseID != dispatch.Lease.LeaseID || lease.WorkerID != dispatch.Lease.WorkerID ||
		lease.Generation != dispatch.Lease.Generation || lease.Attempt != dispatch.Lease.Attempt ||
		lease.FencingToken != dispatch.Lease.FencingToken {
		return fmt.Errorf("%w: review job cannot close cancellation for this lease", scheduling.ErrFenced)
	}
	return nil
}
