package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStrictSchedulingJSONAndPolicyDigest(t *testing.T) {
	policy := testPolicy()
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(data); err != nil {
		t.Fatalf("DecodePolicy(valid) error = %v", err)
	}
	unknown := append(
		append([]byte(nil), data[:len(data)-1]...),
		[]byte(`,"unknown":true}`)...,
	)
	if _, err := DecodePolicy(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodePolicy(unknown) error = %v", err)
	}
	duplicate := append(
		append([]byte(nil), data[:len(data)-1]...),
		[]byte(`,"revision":"policy-1"}`)...,
	)
	if _, err := DecodePolicy(duplicate); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON field") {
		t.Fatalf("DecodePolicy(duplicate) error = %v", err)
	}

	spec := testWorkload(
		"json", "run-json", "tenant-a", ClassInteractive, schedulingEpoch,
	)
	specData, _ := json.Marshal(spec)
	if _, err := DecodeWorkloadSpec(specData); err != nil {
		t.Fatalf("DecodeWorkloadSpec() error = %v", err)
	}
	callback := Callback{
		SchemaVersion:  CallbackSchemaVersion,
		IdempotencyKey: "callback-json", LeaseID: "lease-json",
		WorkloadID: "json", WorkerID: "worker", Attempt: 1, Generation: 1,
		FencingToken: 1, Status: CallbackUnknown, OutputRefs: []string{},
		OccurredAt: schedulingEpoch,
	}
	callbackData, _ := json.Marshal(callback)
	if _, err := DecodeCallback(callbackData); err != nil {
		t.Fatalf("DecodeCallback() error = %v", err)
	}
}

func TestPolicyChangeCannotReplayExistingLedger(t *testing.T) {
	policy := testPolicy()
	repository, store := newSchedulingRepository(t, policy)
	submitWorkload(t, repository, testWorkload(
		"policy", "run-policy", "tenant-a", ClassInteractive, schedulingEpoch,
	))
	changed := policy
	changed.Revision = "policy-2"
	if _, err := NewRepository(store, changed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NewRepository(changed policy) error = %v", err)
	}
}

func TestAdmissionTimeoutAndEmptyReconcileAreDurableAndIdempotent(t *testing.T) {
	policy := testPolicy()
	policy.AdmissionTimeout = time.Hour
	repository, store := newSchedulingRepository(t, policy)
	spec := testWorkload(
		"timeout", "run-timeout", "tenant-a", ClassFullScan, schedulingEpoch,
	)
	submitWorkload(t, repository, spec)
	mutation := testMutation("reconcile-timeout", schedulingEpoch.Add(time.Hour))
	records, err := repository.Reconcile(context.Background(), mutation)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != StateRejected ||
		records[0].StateReason != ReasonAdmissionTimeout {
		t.Fatalf("admission timeout projection = %+v", records)
	}
	if _, err := repository.Reconcile(context.Background(), mutation); err != nil {
		t.Fatalf("Reconcile(idempotent) error = %v", err)
	}
	restarted, err := NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	emptyMutation := testMutation(
		"reconcile-empty", schedulingEpoch.Add(2*time.Hour),
	)
	empty, err := restarted.Reconcile(context.Background(), emptyMutation)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty reconcile returned %+v", empty)
	}
	if _, err := restarted.Reconcile(context.Background(), emptyMutation); err != nil {
		t.Fatalf("empty Reconcile(idempotent) error = %v", err)
	}
	conflict := emptyMutation
	conflict.Audit = "different"
	if _, err := restarted.Reconcile(
		context.Background(), conflict,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("Reconcile(conflicting idempotency key) error = %v", err)
	}
}

func TestScopedReconcilePreservesOtherWorkloadsAndBindsScopeOnRestart(t *testing.T) {
	policy := testPolicy()
	policy.AdmissionTimeout = time.Hour
	repository, store := newSchedulingRepository(t, policy)
	for _, id := range []string{"selected", "unrelated"} {
		submitWorkload(t, repository, testWorkload(id, "run-"+id, "tenant-a", ClassFullScan, schedulingEpoch))
	}
	mutation := testMutation("scoped-reconcile", schedulingEpoch.Add(time.Hour))
	records, err := repository.ReconcileWorkloads(context.Background(), []string{"selected"}, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Spec.WorkloadID != "selected" || records[0].State != StateRejected {
		t.Fatalf("scoped reconcile result = %+v", records)
	}
	restarted, err := NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	other, err := restarted.Get("unrelated")
	if err != nil || other.State != StatePending {
		t.Fatalf("unrelated workload changed: %+v %v", other, err)
	}
	if _, err := restarted.ReconcileWorkloads(context.Background(), []string{"selected"}, mutation); err != nil {
		t.Fatalf("exact retry after restart: %v", err)
	}
	if _, err := restarted.ReconcileWorkloads(context.Background(), []string{"unrelated"}, mutation); !errors.Is(err, ErrConflict) {
		t.Fatalf("different scope reused key: %v", err)
	}
	if _, err := restarted.Reconcile(context.Background(), mutation); !errors.Is(err, ErrConflict) {
		t.Fatalf("global scope reused bounded key: %v", err)
	}
	for _, ids := range [][]string{nil, {}, {""}, {"missing"}, {"selected", "selected"}} {
		if _, err := restarted.ReconcileWorkloads(context.Background(), ids, testMutation("invalid-scope", mutation.At)); err == nil {
			t.Fatalf("invalid scope accepted: %v", ids)
		}
	}
	if _, err := restarted.Reconcile(context.Background(), testMutation("global-after-scoped", mutation.At)); err != nil {
		t.Fatalf("global reconciliation after scoped event: %v", err)
	}
}

func TestScopedClaimKeepsScopeOrderingAndGlobalCapacity(t *testing.T) {
	repository, store := newSchedulingRepository(t, testPolicy())
	for index, id := range []string{"unrelated", "selected-first", "selected-next"} {
		submitWorkload(t, repository, testWorkload(id, "run-"+id, "tenant-a", ClassInteractive,
			schedulingEpoch.Add(time.Duration(index)*time.Second)))
	}
	request := ClaimRequest{
		IdempotencyKey: "scoped-claim", WorkerID: "worker",
		AllowedWorkloadIDs: []string{"selected-first", "selected-next"},
		SupportedClasses:   []WorkloadClass{ClassInteractive}, At: schedulingEpoch.Add(time.Minute),
	}
	request.WorkloadID = "selected-next"
	if _, err := repository.Claim(context.Background(), request); !errors.Is(err, ErrNoWork) {
		t.Fatalf("claim bypassed ordering inside scope: %v", err)
	}
	request.WorkloadID = "selected-first"
	dispatch, err := repository.Claim(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Spec.WorkloadID != "selected-first" {
		t.Fatalf("claimed outside scope: %+v", dispatch)
	}
	restarted, err := NewRepository(store, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Claim(context.Background(), request); err != nil {
		t.Fatalf("exact scoped claim retry: %v", err)
	}
	other, err := restarted.Get("unrelated")
	if err != nil || other.State != StatePending || other.Generation != 0 {
		t.Fatalf("unrelated workload changed: %+v %v", other, err)
	}
	request.IdempotencyKey = "next-scope-claim"
	request.WorkloadID = "selected-next"
	request.AllowedWorkloadIDs = []string{"selected-next"}
	if _, err := restarted.Claim(context.Background(), request); !errors.Is(err, ErrNoWork) {
		t.Fatalf("scoped claim bypassed active class capacity: %v", err)
	}
	if _, err := restarted.Complete(context.Background(), callbackFor(dispatch, "selected-complete", CallbackSucceeded,
		request.At.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	request.At = request.At.Add(2 * time.Second)
	if _, err := restarted.Claim(context.Background(), request); err != nil {
		t.Fatalf("released capacity did not permit scope: %v", err)
	}
	before, err := store.ReadJSONL(schedulingLedger)
	if err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = "unknown-scope"
	request.WorkloadID = "missing"
	request.AllowedWorkloadIDs = []string{"missing"}
	if _, err := restarted.Claim(context.Background(), request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown scope was not rejected: %v", err)
	}
	after, err := store.ReadJSONL(schedulingLedger)
	if err != nil || len(before) != len(after) {
		t.Fatalf("invalid scoped claim wrote an event: %d -> %d %v", len(before), len(after), err)
	}
}
