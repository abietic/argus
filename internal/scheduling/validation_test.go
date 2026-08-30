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
