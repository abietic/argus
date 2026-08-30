package scheduling

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLeaseHeartbeatRestartTimeoutReassignAndLateCallbackFence(t *testing.T) {
	policy := testPolicy()
	repository, store := newSchedulingRepository(t, policy)
	spec := testWorkload(
		"recover", "run-recover", "tenant-a", ClassIncrementalMR, schedulingEpoch,
	)
	submitWorkload(t, repository, spec)
	first := claimWorkload(
		t, repository, "claim-recover-1", "worker-1",
		[]WorkloadClass{ClassIncrementalMR}, schedulingEpoch.Add(time.Minute),
	)
	restarted, err := NewRepository(store, policy)
	if err != nil {
		t.Fatalf("NewRepository(restart) error = %v", err)
	}
	heartbeat := Heartbeat{
		IdempotencyKey: "heartbeat-1",
		LeaseID:        first.Lease.LeaseID, WorkloadID: first.Lease.WorkloadID,
		WorkerID: first.Lease.WorkerID, Attempt: first.Lease.Attempt,
		Generation: first.Lease.Generation, FencingToken: first.Lease.FencingToken,
		At: schedulingEpoch.Add(5 * time.Minute),
	}
	afterHeartbeat, err := restarted.Heartbeat(context.Background(), heartbeat)
	if err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if !afterHeartbeat.ActiveLease.ExpiresAt.Equal(
		schedulingEpoch.Add(15 * time.Minute),
	) {
		t.Fatalf("heartbeat expiry = %s", afterHeartbeat.ActiveLease.ExpiresAt)
	}
	if _, err := restarted.Heartbeat(context.Background(), heartbeat); err != nil {
		t.Fatalf("Heartbeat(idempotent) error = %v", err)
	}
	actions, err := restarted.Reconcile(
		context.Background(),
		testMutation("reconcile-expired", schedulingEpoch.Add(15*time.Minute)),
	)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(actions) != 1 || actions[0].State != StatePending ||
		actions[0].StateReason != string(ActionLeaseExpired) {
		t.Fatalf("reconcile projection = %+v", actions)
	}
	second := claimWorkload(
		t, restarted, "claim-recover-2", "worker-2",
		[]WorkloadClass{ClassIncrementalMR}, schedulingEpoch.Add(16*time.Minute),
	)
	if second.Lease.Generation != first.Lease.Generation+1 ||
		second.Lease.FencingToken != first.Lease.FencingToken+1 {
		t.Fatalf("reassigned lease = %+v, first = %+v", second.Lease, first.Lease)
	}

	late := callbackFor(
		first, "callback-late", CallbackSucceeded,
		schedulingEpoch.Add(9*time.Minute),
	)
	lateRecord, err := restarted.Complete(context.Background(), late)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("Complete(late) error = %v", err)
	}
	if lateRecord.State != StateLeased || len(lateRecord.CallbackRejections) != 1 ||
		lateRecord.CallbackRejections[0].Reason != ReasonStaleGeneration {
		t.Fatalf("late callback projection = %+v", lateRecord)
	}
	if _, err := restarted.Complete(context.Background(), late); !errors.Is(err, ErrFenced) {
		t.Fatalf("Complete(idempotent late) error = %v", err)
	}

	success := callbackFor(
		second, "callback-success", CallbackSucceeded,
		schedulingEpoch.Add(17*time.Minute),
	)
	completed, err := restarted.Complete(context.Background(), success)
	if err != nil {
		t.Fatalf("Complete(success) error = %v", err)
	}
	if completed.State != StateSucceeded || completed.Terminal == nil {
		t.Fatalf("completed projection = %+v", completed)
	}
	if _, err := restarted.Complete(context.Background(), success); err != nil {
		t.Fatalf("Complete(idempotent success) error = %v", err)
	}
	conflict := success
	conflict.OutputRefs = []string{"artifact://local/different-output"}
	if _, err := restarted.Complete(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("Complete(conflicting callback id) error = %v", err)
	}
	again, err := NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	record, err := again.Get(spec.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != StateSucceeded || len(record.CallbackRejections) != 1 {
		t.Fatalf("restart terminal projection = %+v", record)
	}
	timeline, err := again.Timeline(spec.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{
		"submitted", "claimed", "heartbeat", "reconciled", "claimed",
		"callback_rejected", "callback_accepted",
	}
	if len(timeline) != len(wantTypes) {
		t.Fatalf("timeline = %+v", timeline)
	}
	for index, want := range wantTypes {
		if timeline[index].Type != want ||
			(index > 0 && timeline[index].Sequence <= timeline[index-1].Sequence) {
			t.Fatalf("timeline[%d] = %+v, want type %s", index, timeline[index], want)
		}
	}
	if timeline[3].Reconciled[0].Type != ActionLeaseExpired ||
		timeline[4].Lease.Generation != 2 ||
		timeline[5].Callback.Reason != ReasonStaleGeneration ||
		timeline[6].Callback.Callback.Status != CallbackSucceeded {
		t.Fatalf("timeline lost scheduling evidence: %+v", timeline)
	}
}

func TestUnknownOutcomeReconcilesToNewGeneration(t *testing.T) {
	policy := testPolicy()
	repository, _ := newSchedulingRepository(t, policy)
	spec := testWorkload(
		"unknown", "run-unknown", "tenant-a", ClassEvalReplay, schedulingEpoch,
	)
	submitWorkload(t, repository, spec)
	first := claimWorkload(
		t, repository, "claim-unknown-1", "worker-1",
		[]WorkloadClass{ClassEvalReplay}, schedulingEpoch.Add(time.Minute),
	)
	unknown := callbackFor(
		first, "callback-unknown", CallbackUnknown,
		schedulingEpoch.Add(2*time.Minute),
	)
	record, err := repository.Complete(context.Background(), unknown)
	if err != nil {
		t.Fatalf("Complete(unknown) error = %v", err)
	}
	if record.State != StateUnknown || record.UnknownSince == nil {
		t.Fatalf("unknown projection = %+v", record)
	}
	actions, err := repository.Reconcile(
		context.Background(),
		testMutation(
			"reconcile-unknown",
			schedulingEpoch.Add(2*time.Minute+policy.UnknownTimeout),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].State != StatePending ||
		actions[0].StateReason != string(ActionUnknownExpired) {
		t.Fatalf("unknown reconcile projection = %+v", actions)
	}
	second := claimWorkload(
		t, repository, "claim-unknown-2", "worker-2",
		[]WorkloadClass{ClassEvalReplay}, schedulingEpoch.Add(8*time.Minute),
	)
	if second.Lease.Generation != 2 || second.Lease.FencingToken != 2 {
		t.Fatalf("unknown reassign lease = %+v", second.Lease)
	}
	definitiveOld := callbackFor(
		first, "callback-old-definitive", CallbackSucceeded,
		schedulingEpoch.Add(3*time.Minute),
	)
	record, err = repository.Complete(context.Background(), definitiveOld)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("Complete(old definitive) error = %v", err)
	}
	if record.State != StateLeased ||
		record.CallbackRejections[len(record.CallbackRejections)-1].Reason != ReasonStaleGeneration {
		t.Fatalf("old definitive projection = %+v", record)
	}
}

func TestCancelPermanentlyFencesActiveAndFutureWorkloads(t *testing.T) {
	repository, _ := newSchedulingRepository(t, testPolicy())
	spec := testWorkload(
		"cancel", "run-cancel", "tenant-a", ClassInteractive, schedulingEpoch,
	)
	submitWorkload(t, repository, spec)
	dispatch := claimWorkload(
		t, repository, "claim-cancel", "worker",
		[]WorkloadClass{ClassInteractive}, schedulingEpoch.Add(time.Minute),
	)
	canceled, err := repository.CancelRun(
		context.Background(), spec.RunID, "user requested cancellation",
		testMutation("cancel-run", schedulingEpoch.Add(2*time.Minute)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(canceled) != 1 || canceled[0].State != StateCanceled ||
		!canceled[0].RunCanceled || canceled[0].ActiveLease != nil {
		t.Fatalf("cancel projection = %+v", canceled)
	}
	late := callbackFor(
		dispatch, "callback-after-cancel", CallbackSucceeded,
		schedulingEpoch.Add(3*time.Minute),
	)
	record, err := repository.Complete(context.Background(), late)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("Complete(after cancel) error = %v", err)
	}
	if record.State != StateCanceled ||
		record.CallbackRejections[0].Reason != ReasonRunCanceled {
		t.Fatalf("post-cancel callback projection = %+v", record)
	}
	future := testWorkload(
		"future", spec.RunID, "tenant-a", ClassInteractive,
		schedulingEpoch.Add(4*time.Minute),
	)
	futureRecord := submitWorkload(t, repository, future)
	if futureRecord.State != StateRejected || !futureRecord.RunCanceled ||
		futureRecord.Admission.Reason != ReasonRunCanceled {
		t.Fatalf("future canceled-run admission = %+v", futureRecord)
	}
	if _, err := repository.Claim(context.Background(), ClaimRequest{
		IdempotencyKey: "claim-canceled-run", WorkerID: "worker-2",
		SupportedClasses: []WorkloadClass{ClassInteractive},
		At:               schedulingEpoch.Add(5 * time.Minute),
	}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("Claim(canceled run) error = %v", err)
	}
}

func TestCancelCallbackRaceAlwaysLeavesPermanentRunFence(t *testing.T) {
	for iteration := range 20 {
		repository, store := newSchedulingRepository(t, testPolicy())
		second, err := NewRepository(store, testPolicy())
		if err != nil {
			t.Fatal(err)
		}
		spec := testWorkload(
			"race-"+string(rune('a'+iteration)),
			"run-race-"+string(rune('a'+iteration)),
			"tenant-a", ClassIncrementalMR, schedulingEpoch,
		)
		submitWorkload(t, repository, spec)
		dispatch := claimWorkload(
			t, repository, "claim-"+spec.WorkloadID, "worker",
			[]WorkloadClass{ClassIncrementalMR}, schedulingEpoch.Add(time.Minute),
		)
		callback := callbackFor(
			dispatch, "callback-"+spec.WorkloadID, CallbackSucceeded,
			schedulingEpoch.Add(2*time.Minute),
		)
		cancelMutation := testMutation(
			"cancel-"+spec.RunID, schedulingEpoch.Add(2*time.Minute),
		)
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			_, _ = repository.Complete(context.Background(), callback)
		}()
		go func() {
			defer wait.Done()
			_, _ = second.CancelRun(
				context.Background(), spec.RunID, "race cancellation", cancelMutation,
			)
		}()
		wait.Wait()
		if _, err := repository.RunCancellation(spec.RunID); err != nil {
			t.Fatalf("iteration %d has no permanent cancel fence: %v", iteration, err)
		}
		record, err := repository.Get(spec.WorkloadID)
		if err != nil {
			t.Fatal(err)
		}
		if !record.RunCanceled ||
			(record.State != StateCanceled && record.State != StateSucceeded) {
			t.Fatalf("iteration %d race projection = %+v", iteration, record)
		}
		future := testWorkload(
			"future-"+spec.WorkloadID, spec.RunID, "tenant-a",
			ClassIncrementalMR, schedulingEpoch.Add(3*time.Minute),
		)
		futureRecord := submitWorkload(t, repository, future)
		if futureRecord.State != StateRejected ||
			futureRecord.Admission.Reason != ReasonRunCanceled {
			t.Fatalf("iteration %d future projection = %+v", iteration, futureRecord)
		}
	}
}
