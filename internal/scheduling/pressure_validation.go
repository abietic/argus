package scheduling

import (
	"fmt"
	"slices"
	"strings"
)

func DecodePressureSnapshot(data []byte) (PressureSnapshot, error) {
	return decodeStrict(data, "PressureSnapshot", func(value PressureSnapshot) error {
		return value.Validate()
	})
}

func (snapshot PressureSnapshot) Validate() error {
	if snapshot.SchemaVersion != PressureSnapshotSchemaVersion {
		return fmt.Errorf("unsupported pressure snapshot schema %q", snapshot.SchemaVersion)
	}
	if err := validateID("policy_revision", snapshot.PolicyRevision); err != nil {
		return err
	}
	if len(snapshot.PolicySHA256) != 64 || strings.Trim(snapshot.PolicySHA256, "0123456789abcdef") != "" {
		return fmt.Errorf("policy_sha256 must be lowercase SHA-256")
	}
	if !isUTC(snapshot.ObservedAt) || snapshot.Classes == nil || snapshot.Tenants == nil {
		return fmt.Errorf("pressure snapshot timestamp and collections are required")
	}
	if err := snapshot.Global.Validate(); err != nil {
		return fmt.Errorf("global capacity: %w", err)
	}
	if len(snapshot.Classes) != len(workloadClassOrder) {
		return fmt.Errorf("classes must contain all workload classes")
	}
	classQueue, classActive := 0, 0
	classOldest := int64(0)
	for index, class := range snapshot.Classes {
		if class.Class != workloadClassOrder[index] || class.BasePriority < -1000 || class.BasePriority > 1000 {
			return fmt.Errorf("classes[%d] identity is invalid", index)
		}
		if err := class.Capacity.Validate(); err != nil {
			return fmt.Errorf("classes[%d]: %w", index, err)
		}
		classQueue += class.Capacity.QueueDepth
		classActive += class.Capacity.ActiveDepth
		classOldest = max(classOldest, class.Capacity.OldestPendingWaitMS)
	}
	if classQueue != snapshot.Global.QueueDepth || classActive != snapshot.Global.ActiveDepth ||
		classOldest != snapshot.Global.OldestPendingWaitMS {
		return fmt.Errorf("class capacity does not reconcile with global capacity")
	}
	tenantQueue, tenantActive := 0, 0
	tenantOldest := int64(0)
	for index, tenant := range snapshot.Tenants {
		if err := validateID("tenant_id", tenant.TenantID); err != nil ||
			index > 0 && snapshot.Tenants[index-1].TenantID >= tenant.TenantID ||
			tenant.PriorityBias < -1000 || tenant.PriorityBias > 1000 {
			return fmt.Errorf("tenants[%d] identity is invalid", index)
		}
		if err := tenant.Capacity.Validate(); err != nil {
			return fmt.Errorf("tenants[%d]: %w", index, err)
		}
		tenantQueue += tenant.Capacity.QueueDepth
		tenantActive += tenant.Capacity.ActiveDepth
		tenantOldest = max(tenantOldest, tenant.Capacity.OldestPendingWaitMS)
	}
	if tenantQueue != snapshot.Global.QueueDepth || tenantActive != snapshot.Global.ActiveDepth ||
		tenantOldest != snapshot.Global.OldestPendingWaitMS {
		return fmt.Errorf("tenant capacity does not reconcile with global capacity")
	}
	values := []int{
		snapshot.Admissions.Admitted, snapshot.Admissions.Queued, snapshot.Admissions.Rejected,
		snapshot.Admissions.Throttled, snapshot.States.Pending, snapshot.States.Leased,
		snapshot.States.Unknown, snapshot.States.Succeeded, snapshot.States.Failed,
		snapshot.States.Canceled, snapshot.States.Rejected, snapshot.States.Throttled,
		snapshot.Stale.PendingRequiresReconcile, snapshot.Stale.ActiveRequiresReconcile,
	}
	if slices.ContainsFunc(values, func(value int) bool { return value < 0 }) {
		return fmt.Errorf("pressure counters must not be negative")
	}
	admissions := snapshot.Admissions.Admitted + snapshot.Admissions.Queued +
		snapshot.Admissions.Rejected + snapshot.Admissions.Throttled
	states := snapshot.States.Pending + snapshot.States.Leased + snapshot.States.Unknown +
		snapshot.States.Succeeded + snapshot.States.Failed + snapshot.States.Canceled +
		snapshot.States.Rejected + snapshot.States.Throttled
	if admissions != states || snapshot.States.Pending != snapshot.Global.QueueDepth ||
		snapshot.States.Leased+snapshot.States.Unknown != snapshot.Global.ActiveDepth ||
		snapshot.Stale.PendingRequiresReconcile > snapshot.States.Pending ||
		snapshot.Stale.ActiveRequiresReconcile > snapshot.States.Leased+snapshot.States.Unknown {
		return fmt.Errorf("pressure counters do not reconcile")
	}
	return nil
}

func (bucket PressureBucket) Validate() error {
	if bucket.QueueLimit < 1 || bucket.ActiveLimit < 1 || bucket.QueueDepth < 0 ||
		bucket.ActiveDepth < 0 || bucket.OldestPendingWaitMS < 0 ||
		bucket.QueueDepth > bucket.QueueLimit || bucket.ActiveDepth > bucket.ActiveLimit {
		return fmt.Errorf("capacity limits or depths are invalid")
	}
	expected := makePressureBucket(bucket.QueueLimit, bucket.ActiveLimit, pressureAccumulator{
		queueDepth: bucket.QueueDepth, activeDepth: bucket.ActiveDepth, oldestWait: bucket.OldestPendingWaitMS,
	}).State
	if bucket.State != expected {
		return fmt.Errorf("capacity state %q does not match depths", bucket.State)
	}
	if bucket.QueueDepth == 0 && bucket.OldestPendingWaitMS != 0 {
		return fmt.Errorf("oldest pending wait requires queued work")
	}
	return nil
}
