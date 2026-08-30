package scheduling

import (
	"context"
	"fmt"
	"testing"
	"time"

	"argus.local/argus/internal/store/local"
)

var schedulingEpoch = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func testPolicy() Policy {
	return Policy{
		SchemaVersion:     PolicySchemaVersion,
		Revision:          "policy-1",
		GlobalQueueLimit:  16,
		GlobalActiveLimit: 5,
		AgingInterval:     time.Minute,
		AdmissionTimeout:  24 * time.Hour,
		LeaseDuration:     10 * time.Minute,
		UnknownTimeout:    5 * time.Minute,
		Classes: []ClassPolicy{
			{Class: ClassIncrementalMR, PoolLimit: 2, QueueLimit: 8, BasePriority: 80},
			{Class: ClassInteractive, PoolLimit: 1, QueueLimit: 4, BasePriority: 100},
			{Class: ClassFullScan, PoolLimit: 1, QueueLimit: 2, BasePriority: 10},
			{Class: ClassEvalReplay, PoolLimit: 1, QueueLimit: 2, BasePriority: 20},
		},
		DefaultTenantQuota: TenantQuota{MaxQueued: 8, MaxActive: 2},
		TenantQuotas:       []TenantQuota{},
	}
}

func newSchedulingRepository(
	t *testing.T,
	policy Policy,
) (*Repository, *local.Store) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	return repository, store
}

func testWorkload(
	id string,
	runID string,
	tenant string,
	class WorkloadClass,
	at time.Time,
) WorkloadSpec {
	return WorkloadSpec{
		SchemaVersion: WorkloadSchemaVersion,
		WorkloadID:    id, RunID: runID, TenantID: tenant, Class: class,
		InputRef:    "artifact://local/input-" + id,
		SubmittedAt: at, ExecutionDeadline: at.Add(48 * time.Hour),
	}
}

func testMutation(id string, at time.Time) Mutation {
	return Mutation{
		IdempotencyKey: id, Actor: "test-actor", Audit: "test audit", At: at,
	}
}

func submitWorkload(
	t *testing.T,
	repository *Repository,
	spec WorkloadSpec,
) WorkloadRecord {
	t.Helper()
	record, err := repository.Submit(
		context.Background(), spec,
		testMutation("submit-"+spec.WorkloadID, spec.SubmittedAt),
	)
	if err != nil {
		t.Fatalf("Submit(%s) error = %v", spec.WorkloadID, err)
	}
	return record
}

func claimWorkload(
	t *testing.T,
	repository *Repository,
	id string,
	worker string,
	classes []WorkloadClass,
	at time.Time,
) Dispatch {
	t.Helper()
	dispatch, err := repository.Claim(context.Background(), ClaimRequest{
		IdempotencyKey: id, WorkerID: worker,
		SupportedClasses: classes, At: at,
	})
	if err != nil {
		t.Fatalf("Claim(%s) error = %v", id, err)
	}
	return dispatch
}

func callbackFor(
	dispatch Dispatch,
	id string,
	status CallbackStatus,
	at time.Time,
) Callback {
	callback := Callback{
		SchemaVersion:  CallbackSchemaVersion,
		IdempotencyKey: id,
		LeaseID:        dispatch.Lease.LeaseID,
		WorkloadID:     dispatch.Lease.WorkloadID,
		WorkerID:       dispatch.Lease.WorkerID,
		Attempt:        dispatch.Lease.Attempt,
		Generation:     dispatch.Lease.Generation,
		FencingToken:   dispatch.Lease.FencingToken,
		Status:         status,
		OutputRefs:     []string{},
		OccurredAt:     at,
	}
	switch status {
	case CallbackSucceeded:
		callback.OutputRefs = []string{
			fmt.Sprintf("artifact://local/output-%s", dispatch.Spec.WorkloadID),
		}
	case CallbackFailed:
		callback.FailureCode = "worker_failed"
	}
	return callback
}
