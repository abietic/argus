package scheduling

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPressureSnapshotReconcilesCapacityFairnessAndStaleWork(t *testing.T) {
	policy := testPolicy()
	policy.AdmissionTimeout = 30 * time.Minute
	policy.TenantQuotas = []TenantQuota{{TenantID: "tenant-z", MaxQueued: 1, MaxActive: 1, PriorityBias: 5}}
	repository, _ := newSchedulingRepository(t, policy)
	fullOne := testWorkload("full-active", "run-full-active", "tenant-a", ClassFullScan, schedulingEpoch)
	submitWorkload(t, repository, fullOne)
	claimWorkload(t, repository, "claim-full-active", "worker-full", []WorkloadClass{ClassFullScan}, schedulingEpoch.Add(time.Minute))
	fullTwo := testWorkload("full-pending", "run-full-pending", "tenant-b", ClassFullScan, schedulingEpoch.Add(2*time.Minute))
	interactive := testWorkload("interactive-pending", "run-interactive", "tenant-c", ClassInteractive, schedulingEpoch.Add(3*time.Minute))
	submitWorkload(t, repository, fullTwo)
	submitWorkload(t, repository, interactive)

	observedAt := schedulingEpoch.Add(time.Hour)
	snapshot, err := repository.Pressure(observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != PressureSnapshotSchemaVersion || snapshot.LedgerSequence != 4 ||
		snapshot.Global.QueueDepth != 2 || snapshot.Global.ActiveDepth != 1 ||
		snapshot.Global.OldestPendingWaitMS != 58*time.Minute.Milliseconds() ||
		snapshot.Admissions.Admitted != 2 || snapshot.Admissions.Queued != 1 ||
		snapshot.States.Pending != 2 || snapshot.States.Leased != 1 ||
		snapshot.Stale.PendingRequiresReconcile != 2 || snapshot.Stale.ActiveRequiresReconcile != 1 {
		t.Fatalf("pressure snapshot = %+v", snapshot)
	}
	if snapshot.Classes[1].Class != ClassInteractive || snapshot.Classes[1].Capacity.QueueDepth != 1 ||
		snapshot.Classes[1].Capacity.State != CapacityBusy ||
		snapshot.Classes[2].Class != ClassFullScan || snapshot.Classes[2].Capacity.ActiveDepth != 1 ||
		snapshot.Classes[2].Capacity.QueueDepth != 1 || snapshot.Classes[2].Capacity.State != CapacityBusy {
		t.Fatalf("class pressure = %+v", snapshot.Classes)
	}
	if len(snapshot.Tenants) != 4 || snapshot.Tenants[3].TenantID != "tenant-z" ||
		snapshot.Tenants[3].PriorityBias != 5 || snapshot.Tenants[3].Capacity.QueueDepth != 0 {
		t.Fatalf("tenant pressure = %+v", snapshot.Tenants)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePressureSnapshot(data)
	if err != nil || decoded.PolicySHA256 != snapshot.PolicySHA256 {
		t.Fatalf("DecodePressureSnapshot() = %+v, %v", decoded, err)
	}
	if _, err := repository.Pressure(schedulingEpoch.Add(2 * time.Minute)); err == nil {
		t.Fatal("Pressure accepted an observation before the latest event")
	}
}

func TestPressureSnapshotEmptyLedgerAndStrictDecoder(t *testing.T) {
	repository, _ := newSchedulingRepository(t, testPolicy())
	snapshot, err := repository.Pressure(schedulingEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LedgerSequence != 0 || snapshot.Global.State != CapacityAvailable ||
		len(snapshot.Classes) != 4 || len(snapshot.Tenants) != 0 {
		t.Fatalf("empty pressure snapshot = %+v", snapshot)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append([]byte(`{"schema_version":"duplicate",`), data[1:]...)
	if _, err := DecodePressureSnapshot(duplicate); err == nil {
		t.Fatal("DecodePressureSnapshot accepted duplicate JSON fields")
	}
	snapshot.Global.QueueDepth = 1
	if err := snapshot.Validate(); err == nil {
		t.Fatal("PressureSnapshot.Validate accepted unreconciled global depth")
	}
}
