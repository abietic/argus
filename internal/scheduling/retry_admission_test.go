package scheduling

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestDefaultPolicyReclaimsExpiredLeaseWithFreshBoundedQueueWindow(t *testing.T) {
	policy := DefaultLocalPolicy()
	repository, store := newSchedulingRepository(t, policy)
	spec := testWorkload("recover-default", "run-recover-default", "tenant", ClassInteractive, schedulingEpoch)
	spec.ExecutionDeadline = schedulingEpoch.Add(24 * time.Hour)
	initial := submitWorkload(t, repository, spec)
	first := claimWorkload(t, repository, "first-default-claim", "first-worker", []WorkloadClass{ClassInteractive}, schedulingEpoch.Add(time.Second))
	requeuedAt := first.Lease.ExpiresAt
	if !requeuedAt.After(initial.Admission.AdmissionDeadline) {
		t.Fatal("fixture did not cross initial admission deadline")
	}
	if _, err := repository.Reconcile(context.Background(), testMutation("expired-default-lease", requeuedAt)); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	pressure, err := repository.Pressure(requeuedAt.Add(time.Second))
	if err != nil || pressure.Stale.PendingRequiresReconcile != 0 {
		t.Fatalf("fresh retry marked stale: %+v %v", pressure.Stale, err)
	}
	second := claimWorkload(t, repository, "second-default-claim", "second-worker", []WorkloadClass{ClassInteractive}, requeuedAt.Add(time.Second))
	if second.Lease.Generation != 2 || second.Lease.FencingToken <= first.Lease.FencingToken {
		t.Fatalf("recovery did not claim generation two: %+v", second.Lease)
	}
	record, err := repository.Get(spec.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.Admission, initial.Admission) || !record.Spec.ExecutionDeadline.Equal(spec.ExecutionDeadline) {
		t.Fatal("recovery rewrote original admission or extended execution deadline")
	}
}

func TestRetryQueueTimesOutAfterFiveMinutesDespiteLateCallback(t *testing.T) {
	policy := DefaultLocalPolicy()
	repository, store := newSchedulingRepository(t, policy)
	spec := testWorkload("retry-timeout", "run-retry-timeout", "tenant", ClassInteractive, schedulingEpoch)
	submitWorkload(t, repository, spec)
	first := claimWorkload(t, repository, "retry-timeout-claim", "first-worker", []WorkloadClass{ClassInteractive}, schedulingEpoch.Add(time.Second))
	requeuedAt := first.Lease.ExpiresAt
	if _, err := repository.Reconcile(context.Background(), testMutation("retry-timeout-requeue", requeuedAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Complete(context.Background(), callbackFor(first, "stale-retry-callback", CallbackSucceeded, requeuedAt.Add(4*time.Minute))); !errors.Is(err, ErrFenced) {
		t.Fatalf("late old owner callback accepted: %v", err)
	}
	repository, err := NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := repository.Reconcile(context.Background(), testMutation("retry-before-deadline", requeuedAt.Add(5*time.Minute-time.Nanosecond))); err != nil || len(records) != 0 {
		t.Fatalf("retry expired too early: %+v %v", records, err)
	}
	records, err := repository.Reconcile(context.Background(), testMutation("retry-at-deadline", requeuedAt.Add(5*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != StateRejected || records[0].StateReason != ReasonAdmissionTimeout {
		t.Fatalf("late callback extended retry queue window: %+v", records)
	}
}

func TestLegacyRetryAdmissionTimeoutEventsRemainReplayable(t *testing.T) {
	policy := DefaultLocalPolicy()
	repository, store := newSchedulingRepository(t, policy)
	spec := testWorkload("legacy-retry", "run-legacy-retry", "tenant", ClassInteractive, schedulingEpoch)
	submitWorkload(t, repository, spec)
	first := claimWorkload(t, repository, "legacy-claim", "worker", []WorkloadClass{ClassInteractive}, schedulingEpoch.Add(time.Second))
	for index, at := range []time.Time{first.Lease.ExpiresAt, first.Lease.ExpiresAt.Add(time.Second)} {
		state, err := repository.load()
		if err != nil {
			t.Fatal(err)
		}
		event := schedulingEvent{
			SchemaVersion: schedulingEventSchema, PolicySHA256: repository.policySHA256,
			Type: eventReconciled, ReconcileActions: state.reconcileActions(policy, at),
			Actor: "legacy-worker", Audit: "legacy reconciliation without retry window version", OccurredAt: at,
		}
		key := "legacy-requeue"
		if index > 0 {
			key = "legacy-admission-expired"
		}
		if err := repository.append(state.sequence, key, at, event); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := NewRepository(store, policy)
	if err != nil {
		t.Fatalf("new binary rejected legacy retry events: %v", err)
	}
	record, err := restarted.Get(spec.WorkloadID)
	if err != nil || record.State != StateRejected || record.StateReason != ReasonAdmissionTimeout {
		t.Fatalf("legacy terminal facts changed: %+v %v", record, err)
	}
}
