package agentadapter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
)

func TestSchedulingExecutionAuthorityResolverReturnsOnlyCurrentActiveLease(t *testing.T) {
	epoch := time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workloads, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	spec := scheduling.WorkloadSpec{
		SchemaVersion: scheduling.WorkloadSchemaVersion,
		WorkloadID:    "run-1-workload", RunID: "run-1", TenantID: "tenant-1",
		Class: scheduling.ClassIncrementalMR, InputRef: "artifact://local/input-1",
		SubmittedAt: epoch, ExecutionDeadline: epoch.Add(time.Hour),
	}
	if _, err := workloads.Submit(context.Background(), spec, scheduling.Mutation{
		IdempotencyKey: "submit-run-1", Actor: "worker-1", Audit: "test submit", At: epoch,
	}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := workloads.Claim(context.Background(), scheduling.ClaimRequest{
		IdempotencyKey: "claim-run-1", WorkloadID: spec.WorkloadID,
		WorkerID: "worker-1", SupportedClasses: []scheduling.WorkloadClass{
			scheduling.ClassIncrementalMR,
		}, At: epoch.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewSchedulingExecutionAuthorityResolver(
		workloads,
		"worker-1",
		func() time.Time { return epoch.Add(2 * time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	subject := application.AgentPlanningSubject{
		TenantID: "tenant-1", OrganizationID: "organization-1",
		WorkspaceID: "workspace-1", RepositoryID: "repository-1",
	}
	authority, err := resolver.CurrentAgentStageExecutionAuthority(
		context.Background(), subject, "run-1", "agent_hypothesize",
	)
	if err != nil {
		t.Fatalf("CurrentAgentStageExecutionAuthority() error = %v", err)
	}
	if authority.WorkloadID != dispatch.Lease.WorkloadID ||
		authority.LeaseID != dispatch.Lease.LeaseID ||
		authority.LeaseWorker != dispatch.Lease.WorkerID ||
		authority.Attempt != dispatch.Lease.Attempt ||
		authority.Generation != dispatch.Lease.Generation ||
		authority.FencingToken != dispatch.Lease.FencingToken {
		t.Fatalf("authority = %+v, lease = %+v", authority, dispatch.Lease)
	}
	intent := historicalAuthorityIntentFixture(t, subject, dispatch.Lease, epoch.Add(time.Second))
	disposition, err := resolver.ClassifyAgentStageHistoricalIntent(
		context.Background(),
		subject,
		intent,
	)
	if err != nil || disposition.Stale || disposition.Reason != "" {
		t.Fatalf("current intent disposition = %+v, error = %v", disposition, err)
	}
	foreignWorker, err := NewSchedulingExecutionAuthorityResolver(
		workloads,
		"worker-2",
		func() time.Time { return epoch.Add(2 * time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreignWorker.CurrentAgentStageExecutionAuthority(
		context.Background(), subject, "run-1", "agent_hypothesize",
	); err == nil {
		t.Fatal("a different worker consumed another worker's active lease")
	}

	if _, err := workloads.CancelRun(
		context.Background(),
		"run-1",
		"user canceled",
		scheduling.Mutation{
			IdempotencyKey: "cancel-run-1", Actor: "user-1",
			Audit: "test cancel", At: epoch.Add(3 * time.Second),
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.CurrentAgentStageExecutionAuthority(
		context.Background(), subject, "run-1", "agent_hypothesize",
	); err == nil {
		t.Fatal("canceled workload retained formal execution authority")
	}
	disposition, err = resolver.ClassifyAgentStageHistoricalIntent(
		context.Background(),
		subject,
		intent,
	)
	if err != nil || !disposition.Stale ||
		disposition.Reason != "authoritative scheduling workload was canceled" {
		t.Fatalf("canceled intent disposition = %+v, error = %v", disposition, err)
	}
}

func TestSchedulingExecutionAuthorityResolverRejectsForeignTenant(t *testing.T) {
	epoch := time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)
	reader := &authorityWorkloadReaderStub{record: scheduling.WorkloadRecord{
		Spec: scheduling.WorkloadSpec{
			SchemaVersion: scheduling.WorkloadSchemaVersion,
			WorkloadID:    "run-1-workload", RunID: "run-1", TenantID: "tenant-2",
			Class: scheduling.ClassIncrementalMR, InputRef: "artifact://local/input-1",
			SubmittedAt: epoch, ExecutionDeadline: epoch.Add(time.Hour),
		},
	}}
	resolver, err := NewSchedulingExecutionAuthorityResolver(
		reader,
		"worker-1",
		func() time.Time { return epoch },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.CurrentAgentStageExecutionAuthority(
		context.Background(),
		application.AgentPlanningSubject{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			WorkspaceID: "workspace-1", RepositoryID: "repository-1",
		},
		"run-1",
		"agent_hypothesize",
	)
	if err == nil {
		t.Fatal("foreign-tenant workload supplied execution authority")
	}
}

type authorityWorkloadReaderStub struct {
	record scheduling.WorkloadRecord
	err    error
}

func (reader *authorityWorkloadReaderStub) Get(string) (scheduling.WorkloadRecord, error) {
	return reader.record, reader.err
}

func historicalAuthorityIntentFixture(
	t *testing.T,
	subject application.AgentPlanningSubject,
	lease scheduling.DispatchLease,
	recordedAt time.Time,
) runmodel.AgentStageDispatchIntent {
	t.Helper()
	stage := runmodel.AgentStageRef{
		ID: "agent_hypothesize", Revision: "1", SHA256: strings.Repeat("a", 64),
	}
	admissionID, err := runmodel.AgentStagePlanAdmissionID("run-1", stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	planSHA := strings.Repeat("b", 64)
	intent, err := runmodel.SealAgentStageDispatchIntent(
		runmodel.AgentStageDispatchIntent{
			SchemaVersion: runmodel.AgentStageDispatchIntentSchemaVersion,
			Subject: runmodel.AgentPlanningSubject{
				TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
				WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
			},
			ReviewRunID: "run-1", Stage: stage,
			WorkloadID: lease.WorkloadID, LeaseID: lease.LeaseID, LeaseWorker: lease.WorkerID,
			AdmissionID: admissionID, AdmissionSHA256: strings.Repeat("c", 64),
			PlanID: "agent-stage-plan-" + planSHA[:24], PlanSemanticSHA256: planSHA,
			RequestRef: runmodel.ArtifactRef{
				URI:    "artifact://local/sha256/" + strings.Repeat("d", 64),
				SHA256: strings.Repeat("d", 64), SizeBytes: 1,
				Contract: runmodel.ContractStageExecutionRequest,
			},
			RequestSemanticSHA256: strings.Repeat("e", 64),
			CapabilitySHA256:      strings.Repeat("f", 64),
			ExecutionID:           "execution-1",
			Attempt:               lease.Attempt,
			Generation:            lease.Generation,
			FencingToken:          lease.FencingToken,
			CreateIdempotencyKey:  "create-execution-1",
			CancelIdempotencyKey:  "cancel-execution-1",
			RecordedAt:            recordedAt,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
