package scheduling

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAdmissionRecordsAdmittedQueuedRejectedAndThrottledFacts(t *testing.T) {
	t.Run("class saturation", func(t *testing.T) {
		policy := testPolicy()
		policy.Classes[1].QueueLimit = 2
		repository, _ := newSchedulingRepository(t, policy)
		first := submitWorkload(t, repository,
			testWorkload("interactive-1", "run-1", "tenant-a", ClassInteractive, schedulingEpoch))
		second := submitWorkload(t, repository,
			testWorkload("interactive-2", "run-2", "tenant-b", ClassInteractive, schedulingEpoch.Add(1)))
		third := submitWorkload(t, repository,
			testWorkload("interactive-3", "run-3", "tenant-c", ClassInteractive, schedulingEpoch.Add(2)))
		if first.Admission.Decision != AdmissionAdmitted ||
			first.Admission.Reason != ReasonCapacityAvailable {
			t.Fatalf("first admission = %+v", first.Admission)
		}
		if second.Admission.Decision != AdmissionQueued ||
			second.Admission.Reason != ReasonCapacityBusy {
			t.Fatalf("second admission = %+v", second.Admission)
		}
		if third.Admission.Decision != AdmissionRejected ||
			third.Admission.Reason != ReasonClassQueueFull ||
			third.State != StateRejected {
			t.Fatalf("third admission = %+v state=%q", third.Admission, third.State)
		}
	})

	t.Run("tenant throttle", func(t *testing.T) {
		policy := testPolicy()
		policy.DefaultTenantQuota.MaxQueued = 1
		repository, _ := newSchedulingRepository(t, policy)
		submitWorkload(t, repository,
			testWorkload("tenant-1", "run-1", "tenant-a", ClassIncrementalMR, schedulingEpoch))
		second := submitWorkload(t, repository,
			testWorkload("tenant-2", "run-2", "tenant-a", ClassIncrementalMR, schedulingEpoch.Add(1)))
		if second.Admission.Decision != AdmissionThrottled ||
			second.Admission.Reason != ReasonTenantQueueFull ||
			second.State != StateThrottled {
			t.Fatalf("tenant throttle = %+v state=%q", second.Admission, second.State)
		}
	})

	t.Run("global rejection", func(t *testing.T) {
		policy := testPolicy()
		policy.GlobalQueueLimit = 1
		for index := range policy.Classes {
			policy.Classes[index].QueueLimit = 1
		}
		repository, _ := newSchedulingRepository(t, policy)
		submitWorkload(t, repository,
			testWorkload("global-1", "run-1", "tenant-a", ClassIncrementalMR, schedulingEpoch))
		second := submitWorkload(t, repository,
			testWorkload("global-2", "run-2", "tenant-b", ClassInteractive, schedulingEpoch.Add(1)))
		if second.Admission.Decision != AdmissionRejected ||
			second.Admission.Reason != ReasonGlobalQueueFull {
			t.Fatalf("global rejection = %+v", second.Admission)
		}
	})
}

func TestClassPoolsKeepInteractiveIndependentFromFullScan(t *testing.T) {
	repository, _ := newSchedulingRepository(t, testPolicy())
	fullOne := testWorkload("full-1", "run-full-1", "tenant-a", ClassFullScan, schedulingEpoch)
	submitWorkload(t, repository, fullOne)
	fullDispatch := claimWorkload(
		t, repository, "claim-full-1", "full-worker",
		[]WorkloadClass{ClassFullScan}, schedulingEpoch.Add(1),
	)
	if fullDispatch.Spec.WorkloadID != fullOne.WorkloadID {
		t.Fatalf("full dispatch = %q", fullDispatch.Spec.WorkloadID)
	}
	fullTwo := testWorkload(
		"full-2", "run-full-2", "tenant-b", ClassFullScan, schedulingEpoch.Add(2),
	)
	queued := submitWorkload(t, repository, fullTwo)
	if queued.Admission.Decision != AdmissionQueued {
		t.Fatalf("second full scan decision = %q", queued.Admission.Decision)
	}
	interactive := testWorkload(
		"interactive", "run-interactive", "tenant-c",
		ClassInteractive, schedulingEpoch.Add(3),
	)
	submitWorkload(t, repository, interactive)
	dispatch := claimWorkload(
		t, repository, "claim-interactive", "online-worker",
		[]WorkloadClass{ClassInteractive}, schedulingEpoch.Add(4),
	)
	if dispatch.Spec.WorkloadID != interactive.WorkloadID {
		t.Fatalf("interactive dispatch = %q", dispatch.Spec.WorkloadID)
	}
	if _, err := repository.Claim(context.Background(), ClaimRequest{
		IdempotencyKey: "claim-full-saturated", WorkerID: "full-worker-2",
		SupportedClasses: []WorkloadClass{ClassFullScan}, At: schedulingEpoch.Add(5),
	}); err != ErrNoWork {
		t.Fatalf("Claim(full saturated) error = %v", err)
	}
}

func TestTenantFairnessAndStarvationAging(t *testing.T) {
	t.Run("tenant fairness", func(t *testing.T) {
		repository, _ := newSchedulingRepository(t, testPolicy())
		for _, spec := range []WorkloadSpec{
			testWorkload("a-1", "run-a-1", "tenant-a", ClassIncrementalMR, schedulingEpoch),
			testWorkload("a-2", "run-a-2", "tenant-a", ClassIncrementalMR, schedulingEpoch),
			testWorkload("b-1", "run-b-1", "tenant-b", ClassIncrementalMR, schedulingEpoch),
		} {
			submitWorkload(t, repository, spec)
		}
		first := claimWorkload(
			t, repository, "fair-claim-1", "worker-1",
			[]WorkloadClass{ClassIncrementalMR}, schedulingEpoch.Add(1),
		)
		if first.Spec.WorkloadID != "a-1" {
			t.Fatalf("first fair dispatch = %q", first.Spec.WorkloadID)
		}
		second := claimWorkload(
			t, repository, "fair-claim-2", "worker-2",
			[]WorkloadClass{ClassIncrementalMR}, schedulingEpoch.Add(2),
		)
		if second.Spec.WorkloadID != "b-1" {
			t.Fatalf("second fair dispatch = %q, want tenant-b", second.Spec.WorkloadID)
		}
	})

	t.Run("aging", func(t *testing.T) {
		repository, _ := newSchedulingRepository(t, testPolicy())
		old := testWorkload("old-full", "run-old", "tenant-a", ClassFullScan, schedulingEpoch)
		newOnline := testWorkload(
			"new-interactive", "run-new", "tenant-b",
			ClassInteractive, schedulingEpoch.Add(101*time.Minute),
		)
		submitWorkload(t, repository, old)
		submitWorkload(t, repository, newOnline)
		dispatch := claimWorkload(
			t, repository, "aging-claim", "mixed-worker",
			[]WorkloadClass{ClassInteractive, ClassFullScan},
			schedulingEpoch.Add(102*time.Minute),
		)
		if dispatch.Spec.WorkloadID != old.WorkloadID {
			t.Fatalf("aged dispatch = %q, want %q",
				dispatch.Spec.WorkloadID, old.WorkloadID)
		}
	})
}

func TestConcurrentRepositoriesRespectBoundedQueue(t *testing.T) {
	policy := testPolicy()
	policy.Classes[2].QueueLimit = 2
	policy.DefaultTenantQuota.MaxQueued = 20
	first, store := newSchedulingRepository(t, policy)
	second, err := NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	repositories := []*Repository{first, second}
	const submissions = 20
	var wait sync.WaitGroup
	for index := range submissions {
		wait.Add(1)
		go func() {
			defer wait.Done()
			spec := testWorkload(
				"concurrent-"+string(rune('a'+index)),
				"run-concurrent-"+string(rune('a'+index)),
				"tenant-"+string(rune('a'+index)),
				ClassFullScan,
				schedulingEpoch.Add(time.Duration(index)),
			)
			_, _ = repositories[index%2].Submit(
				context.Background(), spec,
				testMutation("submit-"+spec.WorkloadID, spec.SubmittedAt),
			)
		}()
	}
	wait.Wait()
	records, err := first.List()
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, record := range records {
		if record.State == StatePending {
			pending++
		}
	}
	if pending != policy.Classes[2].QueueLimit {
		t.Fatalf("pending full_scan workloads = %d, want %d",
			pending, policy.Classes[2].QueueLimit)
	}
}
