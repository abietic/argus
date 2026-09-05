// Package scheduling defines Argus workload admission and durable dispatch
// semantics. It is provider-neutral and deliberately contains no Hailix,
// worker-runtime, transport, or ACP types.
package scheduling

import (
	"context"
	"errors"
	"time"
)

const (
	PolicySchemaVersion   = "argus.scheduling_policy.v1alpha1"
	WorkloadSchemaVersion = "argus.workload.v1alpha1"
	CallbackSchemaVersion = "argus.workload_callback.v1alpha1"
	schedulingEventSchema = "argus.scheduling_event.v1alpha1"
	schedulingLedger      = "scheduling/workloads"
	TimelineSchemaVersion = "argus.workload_timeline_event.v1alpha1"
)

var (
	ErrNotFound          = errors.New("scheduling workload not found")
	ErrConflict          = errors.New("scheduling mutation conflict")
	ErrNoWork            = errors.New("no eligible workload")
	ErrFenced            = errors.New("workload generation is fenced")
	ErrInvalidTransition = errors.New("invalid scheduling transition")
	ErrCorrupt           = errors.New("corrupt scheduling ledger")
)

type WorkloadClass string

const (
	ClassIncrementalMR WorkloadClass = "incremental_mr"
	ClassInteractive   WorkloadClass = "interactive"
	ClassFullScan      WorkloadClass = "full_scan"
	ClassEvalReplay    WorkloadClass = "eval_replay"
)

var workloadClassOrder = [...]WorkloadClass{
	ClassIncrementalMR,
	ClassInteractive,
	ClassFullScan,
	ClassEvalReplay,
}

func WorkloadClasses() []WorkloadClass {
	return append([]WorkloadClass(nil), workloadClassOrder[:]...)
}

type ClassPolicy struct {
	Class        WorkloadClass `json:"class"`
	PoolLimit    int           `json:"pool_limit"`
	QueueLimit   int           `json:"queue_limit"`
	BasePriority int           `json:"base_priority"`
}

type TenantQuota struct {
	TenantID     string `json:"tenant_id"`
	MaxQueued    int    `json:"max_queued"`
	MaxActive    int    `json:"max_active"`
	PriorityBias int    `json:"priority_bias"`
}

type Policy struct {
	SchemaVersion      string        `json:"schema_version"`
	Revision           string        `json:"revision"`
	GlobalQueueLimit   int           `json:"global_queue_limit"`
	GlobalActiveLimit  int           `json:"global_active_limit"`
	AgingInterval      time.Duration `json:"aging_interval"`
	AdmissionTimeout   time.Duration `json:"admission_timeout"`
	LeaseDuration      time.Duration `json:"lease_duration"`
	UnknownTimeout     time.Duration `json:"unknown_timeout"`
	Classes            []ClassPolicy `json:"classes"`
	DefaultTenantQuota TenantQuota   `json:"default_tenant_quota"`
	TenantQuotas       []TenantQuota `json:"tenant_quotas"`
}

type WorkloadSpec struct {
	SchemaVersion     string        `json:"schema_version"`
	WorkloadID        string        `json:"workload_id"`
	RunID             string        `json:"run_id"`
	TenantID          string        `json:"tenant_id"`
	Class             WorkloadClass `json:"class"`
	Priority          int           `json:"priority"`
	InputRef          string        `json:"input_ref"`
	SubmittedAt       time.Time     `json:"submitted_at"`
	ExecutionDeadline time.Time     `json:"execution_deadline"`
}

type AdmissionDecision string

const (
	AdmissionAdmitted  AdmissionDecision = "admitted"
	AdmissionQueued    AdmissionDecision = "queued"
	AdmissionRejected  AdmissionDecision = "rejected"
	AdmissionThrottled AdmissionDecision = "throttled"
)

const (
	ReasonCapacityAvailable = "capacity_available"
	ReasonCapacityBusy      = "capacity_busy"
	ReasonRunCanceled       = "run_canceled"
	ReasonGlobalQueueFull   = "global_queue_full"
	ReasonClassQueueFull    = "class_queue_full"
	ReasonTenantQueueFull   = "tenant_queue_full"
	ReasonAdmissionTimeout  = "admission_timeout"
	ReasonExecutionDeadline = "execution_deadline"
	ReasonStaleGeneration   = "stale_generation"
	ReasonLeaseExpired      = "lease_expired"
	ReasonLeaseNotActive    = "lease_not_active"
)

type AdmissionFact struct {
	Decision          AdmissionDecision `json:"decision"`
	Reason            string            `json:"reason"`
	GlobalQueueDepth  int               `json:"global_queue_depth"`
	ClassQueueDepth   int               `json:"class_queue_depth"`
	TenantQueueDepth  int               `json:"tenant_queue_depth"`
	AdmissionDeadline time.Time         `json:"admission_deadline"`
	RecordedAt        time.Time         `json:"recorded_at"`
}

type WorkloadState string

const (
	StatePending   WorkloadState = "pending"
	StateLeased    WorkloadState = "leased"
	StateUnknown   WorkloadState = "unknown"
	StateSucceeded WorkloadState = "succeeded"
	StateFailed    WorkloadState = "failed"
	StateCanceled  WorkloadState = "canceled"
	StateRejected  WorkloadState = "rejected"
	StateThrottled WorkloadState = "throttled"
)

type DispatchLease struct {
	LeaseID       string    `json:"lease_id"`
	WorkloadID    string    `json:"workload_id"`
	WorkerID      string    `json:"worker_id"`
	Attempt       int       `json:"attempt"`
	Generation    int       `json:"generation"`
	FencingToken  uint64    `json:"fencing_token"`
	AcquiredAt    time.Time `json:"acquired_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type Dispatch struct {
	Spec  WorkloadSpec  `json:"spec"`
	Lease DispatchLease `json:"lease"`
}

type ClaimRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	// WorkloadID pins a local/synchronous claim to one already-admitted
	// workload. It does not bypass capacity or queue ordering: the pinned
	// workload must still be the next eligible candidate for the advertised
	// classes. Provider workers normally leave this empty.
	WorkloadID string `json:"workload_id,omitempty"`
	// AllowedWorkloadIDs restricts candidate ordering to a bounded worker's
	// explicit scope. Global, class and tenant active limits still apply.
	AllowedWorkloadIDs []string        `json:"allowed_workload_ids,omitempty"`
	WorkerID           string          `json:"worker_id"`
	SupportedClasses   []WorkloadClass `json:"supported_classes"`
	At                 time.Time       `json:"at"`
}

type Heartbeat struct {
	IdempotencyKey string    `json:"idempotency_key"`
	LeaseID        string    `json:"lease_id"`
	WorkloadID     string    `json:"workload_id"`
	WorkerID       string    `json:"worker_id"`
	Attempt        int       `json:"attempt"`
	Generation     int       `json:"generation"`
	FencingToken   uint64    `json:"fencing_token"`
	At             time.Time `json:"at"`
}

type CallbackStatus string

const (
	CallbackSucceeded CallbackStatus = "succeeded"
	CallbackFailed    CallbackStatus = "failed"
	CallbackUnknown   CallbackStatus = "unknown"
)

type Callback struct {
	SchemaVersion  string         `json:"schema_version"`
	IdempotencyKey string         `json:"idempotency_key"`
	LeaseID        string         `json:"lease_id"`
	WorkloadID     string         `json:"workload_id"`
	WorkerID       string         `json:"worker_id"`
	Attempt        int            `json:"attempt"`
	Generation     int            `json:"generation"`
	FencingToken   uint64         `json:"fencing_token"`
	Status         CallbackStatus `json:"status"`
	OutputRefs     []string       `json:"output_refs"`
	FailureCode    string         `json:"failure_code,omitempty"`
	OccurredAt     time.Time      `json:"occurred_at"`
}

type CallbackRejection struct {
	CallbackID string    `json:"callback_id"`
	Reason     string    `json:"reason"`
	RejectedAt time.Time `json:"rejected_at"`
}

type CancelFact struct {
	RunID      string    `json:"run_id"`
	Actor      string    `json:"actor"`
	Reason     string    `json:"reason"`
	CanceledAt time.Time `json:"canceled_at"`
}

type TerminalFact struct {
	CallbackID  string         `json:"callback_id"`
	Status      CallbackStatus `json:"status"`
	OutputRefs  []string       `json:"output_refs"`
	FailureCode string         `json:"failure_code,omitempty"`
	RecordedAt  time.Time      `json:"recorded_at"`
}

type WorkloadRecord struct {
	// retryQueuedAt is rebuilt from versioned reconciliation events. It is
	// not a second persisted authority and must not move on rejected callbacks.
	retryQueuedAt      time.Time
	Spec               WorkloadSpec        `json:"spec"`
	Admission          AdmissionFact       `json:"admission"`
	State              WorkloadState       `json:"state"`
	StateReason        string              `json:"state_reason"`
	Attempt            int                 `json:"attempt"`
	Generation         int                 `json:"generation"`
	FencingToken       uint64              `json:"fencing_token"`
	ActiveLease        *DispatchLease      `json:"active_lease,omitempty"`
	LastLease          *DispatchLease      `json:"last_lease,omitempty"`
	UnknownSince       *time.Time          `json:"unknown_since,omitempty"`
	Terminal           *TerminalFact       `json:"terminal,omitempty"`
	RunCanceled        bool                `json:"run_canceled"`
	CallbackRejections []CallbackRejection `json:"callback_rejections"`
	UpdatedAt          time.Time           `json:"updated_at"`
}

// WorkloadTimelineEvent is a read-only, workload-scoped projection of the
// authoritative append-only scheduling ledger. It intentionally preserves the
// original sequence and execution coordinates instead of inventing a second
// mutable trace state.
type WorkloadTimelineEvent struct {
	SchemaVersion string            `json:"schema_version"`
	Sequence      uint64            `json:"sequence"`
	EventID       string            `json:"event_id"`
	Type          string            `json:"type"`
	PolicySHA256  string            `json:"policy_sha256"`
	OccurredAt    time.Time         `json:"occurred_at"`
	Actor         string            `json:"actor"`
	Audit         string            `json:"audit"`
	Admission     *AdmissionFact    `json:"admission,omitempty"`
	Lease         *DispatchLease    `json:"lease,omitempty"`
	Heartbeat     *HeartbeatFact    `json:"heartbeat,omitempty"`
	Callback      *CallbackFact     `json:"callback,omitempty"`
	Cancellation  *CancelFact       `json:"cancellation,omitempty"`
	Reconciled    []ReconcileAction `json:"reconciled,omitempty"`
}

type Mutation struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

type ReconcileActionType string

const (
	ActionAdmissionExpired ReconcileActionType = "admission_expired"
	ActionExecutionExpired ReconcileActionType = "execution_expired"
	ActionLeaseExpired     ReconcileActionType = "lease_expired"
	ActionUnknownExpired   ReconcileActionType = "unknown_expired"
)

type ReconcileAction struct {
	WorkloadID   string              `json:"workload_id"`
	Type         ReconcileActionType `json:"type"`
	Generation   int                 `json:"generation"`
	FencingToken uint64              `json:"fencing_token"`
}

// WorkloadPort is the Argus-owned scheduling boundary. A future Hailix adapter
// may drive these operations, but no provider-specific runtime state enters
// this contract.
type WorkloadPort interface {
	Submit(context.Context, WorkloadSpec, Mutation) (WorkloadRecord, error)
	Claim(context.Context, ClaimRequest) (Dispatch, error)
	Heartbeat(context.Context, Heartbeat) (WorkloadRecord, error)
	Complete(context.Context, Callback) (WorkloadRecord, error)
	CancelRun(context.Context, string, string, Mutation) ([]WorkloadRecord, error)
	Reconcile(context.Context, Mutation) ([]WorkloadRecord, error)
}
