package scheduling

import "time"

// DefaultLocalPolicy is the bounded policy used by the synchronous local CLI
// coordinator. It is intentionally small and provider-neutral: it does not
// claim to model Hailix capacity, remote worker pools, or stage fan-out.
//
// A fresh value is returned on every call so callers cannot mutate global
// policy state.
func DefaultLocalPolicy() Policy {
	return Policy{
		SchemaVersion:     PolicySchemaVersion,
		Revision:          "local-run-coordinator-1",
		GlobalQueueLimit:  64,
		GlobalActiveLimit: 4,
		AgingInterval:     time.Minute,
		AdmissionTimeout:  5 * time.Minute,
		LeaseDuration:     30 * time.Minute,
		UnknownTimeout:    10 * time.Minute,
		Classes: []ClassPolicy{
			{
				Class:        ClassIncrementalMR,
				PoolLimit:    1,
				QueueLimit:   32,
				BasePriority: 80,
			},
			{
				Class:        ClassInteractive,
				PoolLimit:    1,
				QueueLimit:   16,
				BasePriority: 100,
			},
			{
				Class:        ClassFullScan,
				PoolLimit:    1,
				QueueLimit:   8,
				BasePriority: 10,
			},
			{
				Class:        ClassEvalReplay,
				PoolLimit:    1,
				QueueLimit:   16,
				BasePriority: 20,
			},
		},
		DefaultTenantQuota: TenantQuota{
			MaxQueued: 64,
			MaxActive: 4,
		},
		TenantQuotas: []TenantQuota{},
	}
}
