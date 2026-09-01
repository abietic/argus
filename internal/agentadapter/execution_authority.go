package agentadapter

import (
	"context"
	"fmt"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/scheduling"
)

type schedulingWorkloadReader interface {
	Get(string) (scheduling.WorkloadRecord, error)
}

// SchedulingExecutionAuthorityResolver projects the current Argus scheduling
// lease into the formal-agent execution authority port. It never claims or
// renews a lease: dispatch may only consume the active run authority selected
// by the workload coordinator.
type SchedulingExecutionAuthorityResolver struct {
	workloads schedulingWorkloadReader
	workerID  string
	now       func() time.Time
}

var _ application.AgentStageExecutionAuthorityResolver = (*SchedulingExecutionAuthorityResolver)(nil)
var _ application.AgentStageHistoricalIntentStalenessResolver = (*SchedulingExecutionAuthorityResolver)(nil)

func NewSchedulingExecutionAuthorityResolver(
	workloads schedulingWorkloadReader,
	workerID string,
	now func() time.Time,
) (*SchedulingExecutionAuthorityResolver, error) {
	if workloads == nil {
		return nil, fmt.Errorf("scheduling workload reader is required")
	}
	if workerID == "" {
		return nil, fmt.Errorf("expected scheduling worker identity is required")
	}
	if now == nil {
		now = time.Now
	}
	return &SchedulingExecutionAuthorityResolver{
		workloads: workloads,
		workerID:  workerID,
		now:       now,
	}, nil
}

func (resolver *SchedulingExecutionAuthorityResolver) CurrentAgentStageExecutionAuthority(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	runID string,
	stageID string,
) (application.AgentStageExecutionAuthority, error) {
	if err := checkContext(ctx); err != nil {
		return application.AgentStageExecutionAuthority{}, err
	}
	if resolver == nil || resolver.workloads == nil || resolver.workerID == "" ||
		resolver.now == nil {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"scheduling execution authority resolver is not initialized",
		)
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	if err := ledgerSubject.Validate(); err != nil {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"validate formal agent scheduling subject: %w",
			err,
		)
	}
	if runID == "" || stageID == "" {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"review run and stage are required for scheduling authority",
		)
	}
	record, err := resolver.workloads.Get(runID + "-workload")
	if err != nil {
		return application.AgentStageExecutionAuthority{}, err
	}
	if err := checkContext(ctx); err != nil {
		return application.AgentStageExecutionAuthority{}, err
	}
	if err := record.Spec.Validate(); err != nil {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"validate current scheduling workload: %w",
			err,
		)
	}
	if record.Spec.WorkloadID != runID+"-workload" || record.Spec.RunID != runID ||
		record.Spec.TenantID != subject.TenantID {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"current scheduling workload does not match authenticated run subject",
		)
	}
	if record.RunCanceled || record.State != scheduling.StateLeased ||
		record.ActiveLease == nil || record.Terminal != nil {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"formal agent execution requires a non-canceled active scheduling lease",
		)
	}
	lease := *record.ActiveLease
	if err := lease.Validate(); err != nil {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"validate current scheduling lease: %w",
			err,
		)
	}
	observedAt := resolver.now().UTC()
	if observedAt.IsZero() || !lease.ExpiresAt.After(observedAt) ||
		!record.Spec.ExecutionDeadline.After(observedAt) {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"current scheduling lease or workload deadline is expired",
		)
	}
	if lease.WorkloadID != record.Spec.WorkloadID || lease.Generation != record.Generation ||
		lease.FencingToken != record.FencingToken || lease.Attempt != record.Attempt ||
		lease.WorkerID != resolver.workerID {
		return application.AgentStageExecutionAuthority{}, fmt.Errorf(
			"current scheduling lease does not match the worker-owned workload generation fence",
		)
	}
	return application.AgentStageExecutionAuthority{
		WorkloadID: lease.WorkloadID, LeaseID: lease.LeaseID,
		LeaseWorker: lease.WorkerID, Attempt: lease.Attempt,
		Generation: lease.Generation, FencingToken: lease.FencingToken,
		AcquiredAt: lease.AcquiredAt, ExpiresAt: lease.ExpiresAt,
	}, nil
}

// ClassifyAgentStageHistoricalIntent compares an immutable claimed intent with
// the current workload record. It never infers staleness merely from another
// local intent: canceled, terminal, expired, and reassigned latest claims must
// also be recoverable, while an exact active lease must never be canceled.
func (resolver *SchedulingExecutionAuthorityResolver) ClassifyAgentStageHistoricalIntent(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	intent runmodel.AgentStageDispatchIntent,
) (application.AgentStageHistoricalIntentDisposition, error) {
	if err := checkContext(ctx); err != nil {
		return application.AgentStageHistoricalIntentDisposition{}, err
	}
	if resolver == nil || resolver.workloads == nil || resolver.now == nil {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"scheduling execution authority resolver is not initialized",
		)
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	if err := ledgerSubject.Validate(); err != nil {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"validate historical scheduling subject: %w",
			err,
		)
	}
	if err := intent.Validate(); err != nil {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"validate historical dispatch intent: %w",
			err,
		)
	}
	if intent.Subject != ledgerSubject {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"historical dispatch intent escaped authenticated subject",
		)
	}
	record, err := resolver.workloads.Get(intent.WorkloadID)
	if err != nil {
		return application.AgentStageHistoricalIntentDisposition{}, err
	}
	if err := checkContext(ctx); err != nil {
		return application.AgentStageHistoricalIntentDisposition{}, err
	}
	if err := record.Spec.Validate(); err != nil {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"validate historical scheduling workload: %w",
			err,
		)
	}
	if record.Spec.WorkloadID != intent.WorkloadID ||
		record.Spec.RunID != intent.ReviewRunID ||
		record.Spec.TenantID != intent.Subject.TenantID {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"historical scheduling workload does not match claimed intent",
		)
	}
	observedAt := resolver.now().UTC()
	if observedAt.IsZero() {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"historical scheduling observation time is required",
		)
	}
	stale := func(reason string) application.AgentStageHistoricalIntentDisposition {
		return application.AgentStageHistoricalIntentDisposition{Stale: true, Reason: reason}
	}
	if record.RunCanceled {
		return stale("authoritative scheduling workload was canceled"), nil
	}
	if record.Terminal != nil || record.State == scheduling.StateSucceeded ||
		record.State == scheduling.StateFailed || record.State == scheduling.StateCanceled ||
		record.State == scheduling.StateRejected || record.State == scheduling.StateThrottled {
		return stale("authoritative scheduling workload is terminal"), nil
	}
	if !record.Spec.ExecutionDeadline.After(observedAt) {
		return stale("authoritative scheduling workload deadline expired"), nil
	}
	if (record.State != scheduling.StateLeased && record.State != scheduling.StateUnknown) ||
		record.ActiveLease == nil {
		return stale("authoritative scheduling workload has no active lease"), nil
	}
	lease := *record.ActiveLease
	if err := lease.Validate(); err != nil {
		return application.AgentStageHistoricalIntentDisposition{}, fmt.Errorf(
			"validate historical scheduling lease: %w",
			err,
		)
	}
	if !lease.ExpiresAt.After(observedAt) {
		return stale("authoritative scheduling lease expired"), nil
	}
	stageOffset := intent.Attempt - 1
	if lease.WorkloadID != intent.WorkloadID || lease.LeaseID != intent.LeaseID ||
		lease.WorkerID != intent.LeaseWorker || intent.Attempt < 1 ||
		intent.Generation != lease.Generation+stageOffset ||
		lease.FencingToken != intent.FencingToken ||
		record.Generation != lease.Generation || record.Attempt != lease.Attempt ||
		record.FencingToken != intent.FencingToken {
		return stale("authoritative scheduling lease was reassigned"), nil
	}
	return application.AgentStageHistoricalIntentDisposition{}, nil
}
