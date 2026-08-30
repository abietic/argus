package scheduling

import (
	"fmt"
	"sort"
	"time"
)

const PressureSnapshotSchemaVersion = "argus.workload_pressure_snapshot.v1alpha1"

type CapacityState string

const (
	CapacityAvailable CapacityState = "available"
	CapacityBusy      CapacityState = "busy"
	CapacityQueueFull CapacityState = "queue_full"
)

type PressureBucket struct {
	QueueLimit          int           `json:"queue_limit"`
	QueueDepth          int           `json:"queue_depth"`
	ActiveLimit         int           `json:"active_limit"`
	ActiveDepth         int           `json:"active_depth"`
	OldestPendingWaitMS int64         `json:"oldest_pending_wait_ms"`
	State               CapacityState `json:"state"`
}

type ClassPressure struct {
	Class        WorkloadClass  `json:"class"`
	BasePriority int            `json:"base_priority"`
	Capacity     PressureBucket `json:"capacity"`
}

type TenantPressure struct {
	TenantID     string         `json:"tenant_id"`
	PriorityBias int            `json:"priority_bias"`
	Capacity     PressureBucket `json:"capacity"`
}

type AdmissionCounters struct {
	Admitted  int `json:"admitted"`
	Queued    int `json:"queued"`
	Rejected  int `json:"rejected"`
	Throttled int `json:"throttled"`
}

type StateCounters struct {
	Pending   int `json:"pending"`
	Leased    int `json:"leased"`
	Unknown   int `json:"unknown"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
	Rejected  int `json:"rejected"`
	Throttled int `json:"throttled"`
}

type StaleCounters struct {
	PendingRequiresReconcile int `json:"pending_requires_reconcile"`
	ActiveRequiresReconcile  int `json:"active_requires_reconcile"`
}

// PressureSnapshot is a read-only projection of the authoritative workload
// ledger. It never runs reconciliation implicitly: expired pending/active
// records remain visible in Stale until an explicit Reconcile mutation occurs.
type PressureSnapshot struct {
	SchemaVersion  string            `json:"schema_version"`
	PolicyRevision string            `json:"policy_revision"`
	PolicySHA256   string            `json:"policy_sha256"`
	LedgerSequence uint64            `json:"ledger_sequence"`
	ObservedAt     time.Time         `json:"observed_at"`
	Global         PressureBucket    `json:"global"`
	Classes        []ClassPressure   `json:"classes"`
	Tenants        []TenantPressure  `json:"tenants"`
	Admissions     AdmissionCounters `json:"admissions"`
	States         StateCounters     `json:"states"`
	Stale          StaleCounters     `json:"stale"`
}

type pressureAccumulator struct {
	queueDepth  int
	activeDepth int
	oldestWait  int64
}

// Pressure returns a point-in-time metrics projection without changing
// workload state. observedAt must not precede the latest committed event.
func (repository *Repository) Pressure(observedAt time.Time) (PressureSnapshot, error) {
	if repository == nil || repository.store == nil {
		return PressureSnapshot{}, fmt.Errorf("scheduling repository is required")
	}
	if !isUTC(observedAt) {
		return PressureSnapshot{}, fmt.Errorf("observed_at must be a non-zero UTC timestamp")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return PressureSnapshot{}, err
	}
	latest := time.Time{}
	sequence := uint64(0)
	for _, envelope := range state.events {
		if envelope.Sequence > sequence {
			sequence = envelope.Sequence
		}
		if envelope.Time.After(latest) {
			latest = envelope.Time
		}
	}
	if !latest.IsZero() && observedAt.Before(latest) {
		return PressureSnapshot{}, fmt.Errorf("observed_at precedes the latest scheduling event")
	}

	global := pressureAccumulator{}
	classes := make(map[WorkloadClass]*pressureAccumulator, len(workloadClassOrder))
	for _, class := range workloadClassOrder {
		classes[class] = &pressureAccumulator{}
	}
	tenantIDs := make(map[string]struct{})
	for _, quota := range repository.policy.TenantQuotas {
		tenantIDs[quota.TenantID] = struct{}{}
	}
	tenants := make(map[string]*pressureAccumulator)
	snapshot := PressureSnapshot{
		SchemaVersion: PressureSnapshotSchemaVersion, PolicyRevision: repository.policy.Revision,
		PolicySHA256: repository.policySHA256, LedgerSequence: sequence, ObservedAt: observedAt,
		Classes: []ClassPressure{}, Tenants: []TenantPressure{},
	}
	for _, record := range state.workloads {
		tenantIDs[record.Spec.TenantID] = struct{}{}
		if tenants[record.Spec.TenantID] == nil {
			tenants[record.Spec.TenantID] = &pressureAccumulator{}
		}
		switch record.Admission.Decision {
		case AdmissionAdmitted:
			snapshot.Admissions.Admitted++
		case AdmissionQueued:
			snapshot.Admissions.Queued++
		case AdmissionRejected:
			snapshot.Admissions.Rejected++
		case AdmissionThrottled:
			snapshot.Admissions.Throttled++
		}
		switch record.State {
		case StatePending:
			snapshot.States.Pending++
			wait := observedAt.Sub(record.Spec.SubmittedAt).Milliseconds()
			global.queueDepth++
			classes[record.Spec.Class].queueDepth++
			tenants[record.Spec.TenantID].queueDepth++
			global.oldestWait = max(global.oldestWait, wait)
			classes[record.Spec.Class].oldestWait = max(classes[record.Spec.Class].oldestWait, wait)
			tenants[record.Spec.TenantID].oldestWait = max(tenants[record.Spec.TenantID].oldestWait, wait)
			if !observedAt.Before(record.Admission.AdmissionDeadline) ||
				!observedAt.Before(record.Spec.ExecutionDeadline) {
				snapshot.Stale.PendingRequiresReconcile++
			}
		case StateLeased, StateUnknown:
			if record.State == StateLeased {
				snapshot.States.Leased++
			} else {
				snapshot.States.Unknown++
			}
			global.activeDepth++
			classes[record.Spec.Class].activeDepth++
			tenants[record.Spec.TenantID].activeDepth++
			lease := record.ActiveLease
			if lease == nil {
				lease = record.LastLease
			}
			if lease == nil || !observedAt.Before(lease.ExpiresAt) ||
				!observedAt.Before(record.Spec.ExecutionDeadline) {
				snapshot.Stale.ActiveRequiresReconcile++
			}
		case StateSucceeded:
			snapshot.States.Succeeded++
		case StateFailed:
			snapshot.States.Failed++
		case StateCanceled:
			snapshot.States.Canceled++
		case StateRejected:
			snapshot.States.Rejected++
		case StateThrottled:
			snapshot.States.Throttled++
		}
	}
	snapshot.Global = makePressureBucket(
		repository.policy.GlobalQueueLimit, repository.policy.GlobalActiveLimit, global,
	)
	for _, classPolicy := range repository.policy.Classes {
		snapshot.Classes = append(snapshot.Classes, ClassPressure{
			Class: classPolicy.Class, BasePriority: classPolicy.BasePriority,
			Capacity: makePressureBucket(classPolicy.QueueLimit, classPolicy.PoolLimit, *classes[classPolicy.Class]),
		})
	}
	orderedTenants := make([]string, 0, len(tenantIDs))
	for tenantID := range tenantIDs {
		orderedTenants = append(orderedTenants, tenantID)
		if tenants[tenantID] == nil {
			tenants[tenantID] = &pressureAccumulator{}
		}
	}
	sort.Strings(orderedTenants)
	for _, tenantID := range orderedTenants {
		quota := repository.policy.tenantQuota(tenantID)
		snapshot.Tenants = append(snapshot.Tenants, TenantPressure{
			TenantID: tenantID, PriorityBias: quota.PriorityBias,
			Capacity: makePressureBucket(quota.MaxQueued, quota.MaxActive, *tenants[tenantID]),
		})
	}
	if err := snapshot.Validate(); err != nil {
		return PressureSnapshot{}, fmt.Errorf("validate workload pressure snapshot: %w", err)
	}
	return snapshot, nil
}

func makePressureBucket(queueLimit, activeLimit int, current pressureAccumulator) PressureBucket {
	state := CapacityAvailable
	if current.queueDepth >= queueLimit {
		state = CapacityQueueFull
	} else if current.activeDepth+current.queueDepth >= activeLimit {
		state = CapacityBusy
	}
	return PressureBucket{
		QueueLimit: queueLimit, QueueDepth: current.queueDepth,
		ActiveLimit: activeLimit, ActiveDepth: current.activeDepth,
		OldestPendingWaitMS: current.oldestWait, State: state,
	}
}
