package main

import (
	"errors"
	"testing"
	"time"

	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
)

func TestLocalReviewJobCancellationCannotAuthorizeSuccessOrAnotherOwner(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workloads, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	_, err = workloads.Submit(t.Context(), scheduling.WorkloadSpec{
		SchemaVersion: scheduling.WorkloadSchemaVersion, WorkloadID: "authority-workload", RunID: "authority-run",
		TenantID: "local", Class: scheduling.ClassInteractive, InputRef: "artifact://local/authority-input",
		SubmittedAt: at, ExecutionDeadline: at.Add(time.Hour),
	}, scheduling.Mutation{IdempotencyKey: "authority-submit", Actor: "test", Audit: "test exact lease", At: at})
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := workloads.Claim(t.Context(), scheduling.ClaimRequest{
		IdempotencyKey: "authority-claim", WorkloadID: "authority-workload", WorkerID: "authority-worker",
		SupportedClasses: []scheduling.WorkloadClass{scheduling.ClassInteractive}, At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLocalReviewJobAuthority(t.Context(), workloads, dispatch); err != nil {
		t.Fatal(err)
	}
	if err := validateLocalReviewJobCancellation(t.Context(), workloads, dispatch); !errors.Is(err, scheduling.ErrFenced) {
		t.Fatalf("active lease cannot authorize canceled closure: %v", err)
	}
	if _, err := workloads.CancelRun(t.Context(), dispatch.Spec.RunID, "operator cancellation", scheduling.Mutation{
		IdempotencyKey: "authority-cancel", Actor: "test", Audit: "test permanent cancellation", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateLocalReviewJobAuthority(t.Context(), workloads, dispatch); !errors.Is(err, scheduling.ErrFenced) {
		t.Fatalf("canceled lease authorized ordinary execution: %v", err)
	}
	if err := validateLocalReviewJobCancellation(t.Context(), workloads, dispatch); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*scheduling.Dispatch){
		func(d *scheduling.Dispatch) { d.Lease.Generation++ },
		func(d *scheduling.Dispatch) { d.Lease.WorkerID = "other-worker" },
		func(d *scheduling.Dispatch) { d.Lease.FencingToken++ },
		func(d *scheduling.Dispatch) { d.Spec.RunID = "other-run" },
	} {
		other := dispatch
		change(&other)
		if err := validateLocalReviewJobCancellation(t.Context(), workloads, other); !errors.Is(err, scheduling.ErrFenced) {
			t.Fatalf("different owner authorized canceled closure: %v", err)
		}
	}
}
