package scheduling

import (
	"testing"
	"time"
)

func TestDefaultLocalPolicyIsValidAndReturnsFreshState(t *testing.T) {
	first := DefaultLocalPolicy()
	if err := first.Validate(); err != nil {
		t.Fatalf("DefaultLocalPolicy().Validate() error = %v", err)
	}
	digest, err := PolicyDigest(first)
	if err != nil {
		t.Fatalf("PolicyDigest() error = %v", err)
	}
	if digest == "" {
		t.Fatal("PolicyDigest() returned an empty digest")
	}

	first.Classes[0].PoolLimit = 99
	first.TenantQuotas = append(first.TenantQuotas, TenantQuota{
		TenantID: "mutated", MaxQueued: 1, MaxActive: 1,
	})
	second := DefaultLocalPolicy()
	if second.Classes[0].PoolLimit != 1 || len(second.TenantQuotas) != 0 {
		t.Fatalf("DefaultLocalPolicy() shares mutable state: %+v", second)
	}
}

func TestPinnedClaimPreservesQueueOrderingAndIsIdempotent(t *testing.T) {
	repository, _ := newSchedulingRepository(t, testPolicy())
	first := testWorkload(
		"pinned-first", "run-first", "tenant-a",
		ClassInteractive, schedulingEpoch,
	)
	second := testWorkload(
		"pinned-second", "run-second", "tenant-b",
		ClassInteractive, schedulingEpoch.Add(time.Millisecond),
	)
	submitWorkload(t, repository, first)
	submitWorkload(t, repository, second)

	request := ClaimRequest{
		IdempotencyKey: "claim-pinned-second",
		WorkloadID:     second.WorkloadID,
		WorkerID:       "local-worker",
		SupportedClasses: []WorkloadClass{
			ClassInteractive,
		},
		At: schedulingEpoch.Add(time.Second),
	}
	if _, err := repository.Claim(t.Context(), request); err != ErrNoWork {
		t.Fatalf("Claim(out of order) error = %v, want %v", err, ErrNoWork)
	}

	request.IdempotencyKey = "claim-pinned-first"
	request.WorkloadID = first.WorkloadID
	dispatch, err := repository.Claim(t.Context(), request)
	if err != nil {
		t.Fatalf("Claim(first) error = %v", err)
	}
	if dispatch.Spec.WorkloadID != first.WorkloadID {
		t.Fatalf("Claim(first) workload = %q", dispatch.Spec.WorkloadID)
	}
	again, err := repository.Claim(t.Context(), request)
	if err != nil {
		t.Fatalf("Claim(first idempotent) error = %v", err)
	}
	if again != dispatch {
		t.Fatalf("idempotent dispatch changed: got %+v want %+v", again, dispatch)
	}
}
