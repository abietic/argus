// Package publication owns Argus' provider-neutral remote publication
// protocol. It deliberately separates a Finding decision from the external
// comment side effect and records every side-effect boundary durably.
package publication

import (
	"context"
	"time"
)

const (
	RequestSchemaVersion      = "argus.publication_request.v1alpha1"
	IntentSchemaVersion       = "argus.publication_intent.v1alpha1"
	RevalidationSchemaVersion = "argus.publication_revalidation.v1alpha1"
	ProviderRequestSchema     = "argus.provider_publication_request.v1alpha1"
	ProviderResultSchema      = "argus.provider_publication_result.v1alpha1"
	LedgerEventSchemaVersion  = "argus.publication_ledger_event.v1alpha1"

	FindingSourceFindingSet           = "argus.finding_set.v1alpha1"
	FindingSourceGovernedReviewReport = "argus.governed_review_report.v1alpha1+json"
)

// Intent contains only caller-selectable publication fields. Finding source,
// target, message, repository identity, and Decision authority are resolved
// from committed Argus facts by the control plane.
type Intent struct {
	SchemaVersion string    `json:"schema_version"`
	GrantID       string    `json:"grant_id"`
	CreatedAt     time.Time `json:"created_at"`
}

type RunKind string

const (
	RunKindReview RunKind = "review"
	RunKindReplay RunKind = "replay"
)

type RunStatus string

const (
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCanceled  RunStatus = "canceled"
)

type TargetMode string

const (
	TargetModeDiff      TargetMode = "diff"
	TargetModeSelection TargetMode = "selection"
	TargetModeScope     TargetMode = "scope"
)

type ContentRef struct {
	URI       string `json:"uri"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type StableAnchor struct {
	Path         string `json:"path"`
	Side         string `json:"side"`
	StartLine    uint32 `json:"start_line"`
	EndLine      uint32 `json:"end_line"`
	TargetDigest string `json:"target_digest"`
}

// Request is the immutable Argus-owned intent to project one already-decided
// Finding to a remote code platform. It grants comment publication only; it
// never grants suggestion application or working-copy mutation.
type Request struct {
	SchemaVersion  string `json:"schema_version"`
	PublicationID  string `json:"publication_id"`
	IdempotencyKey string `json:"idempotency_key"`
	GrantID        string `json:"grant_id"`
	GrantSHA256    string `json:"grant_sha256"`

	TenantID    string     `json:"tenant_id"`
	WorkspaceID string     `json:"workspace_id"`
	RunID       string     `json:"run_id"`
	RunKind     RunKind    `json:"run_kind"`
	RunStatus   RunStatus  `json:"run_status"`
	TargetMode  TargetMode `json:"target_mode"`

	Provider             string `json:"provider"`
	RepositoryID         string `json:"repository_id"`
	ChangeKind           string `json:"change_kind"`
	ChangeID             string `json:"change_id"`
	BaseRevision         string `json:"base_revision"`
	ExpectedHeadRevision string `json:"expected_head_revision"`

	TargetSnapshotRef     ContentRef `json:"target_snapshot_ref"`
	ConfigBundleRef       ContentRef `json:"config_bundle_ref"`
	FindingSourceRef      ContentRef `json:"finding_source_ref"`
	FindingSourceContract string     `json:"finding_source_contract"`

	FindingID    string       `json:"finding_id"`
	Fingerprint  string       `json:"fingerprint"`
	DecisionID   string       `json:"decision_id"`
	TargetDigest string       `json:"target_digest"`
	Anchor       StableAnchor `json:"anchor"`
	Channel      string       `json:"channel"`
	Message      string       `json:"message"`
	RemoteWrites string       `json:"remote_writes"`

	ExpectedPermissionRevision string    `json:"expected_permission_revision"`
	GrantExpiresAt             time.Time `json:"grant_expires_at"`
	CreatedAt                  time.Time `json:"created_at"`
}

// Revalidation is a fresh provider/platform observation made immediately
// before dispatch. Every expected identity is compared exactly by Service.
type Revalidation struct {
	SchemaVersion string `json:"schema_version"`

	ProviderBaseRevision string    `json:"provider_base_revision"`
	ProviderHeadRevision string    `json:"provider_head_revision"`
	PermissionGranted    bool      `json:"permission_granted"`
	PermissionRevision   string    `json:"permission_revision"`
	ConfigBundleSHA256   string    `json:"config_bundle_sha256"`
	FindingSourceSHA256  string    `json:"finding_source_sha256"`
	FindingCurrent       bool      `json:"finding_current"`
	AnchorCurrent        bool      `json:"anchor_current"`
	CheckedAt            time.Time `json:"checked_at"`
}

type ProviderRequest struct {
	SchemaVersion      string       `json:"schema_version"`
	PublicationID      string       `json:"publication_id"`
	IdempotencyKey     string       `json:"idempotency_key"`
	GrantID            string       `json:"grant_id"`
	Provider           string       `json:"provider"`
	RepositoryID       string       `json:"repository_id"`
	ChangeKind         string       `json:"change_kind"`
	ChangeID           string       `json:"change_id"`
	BaseRevision       string       `json:"base_revision"`
	HeadRevision       string       `json:"head_revision"`
	FindingID          string       `json:"finding_id"`
	Fingerprint        string       `json:"fingerprint"`
	DecisionID         string       `json:"decision_id"`
	PermissionRevision string       `json:"permission_revision"`
	Anchor             StableAnchor `json:"anchor"`
	Channel            string       `json:"channel"`
	Message            string       `json:"message"`
	RequestSHA256      string       `json:"request_sha256"`
}

type ProviderResultStatus string

const (
	ProviderResultPublished ProviderResultStatus = "published"
	ProviderResultNotFound  ProviderResultStatus = "not_found"
	ProviderResultRejected  ProviderResultStatus = "rejected"
	ProviderResultUnknown   ProviderResultStatus = "unknown"
)

type ProviderResult struct {
	SchemaVersion     string               `json:"schema_version"`
	Status            ProviderResultStatus `json:"status"`
	IdempotencyKey    string               `json:"idempotency_key"`
	ProviderRequestID string               `json:"provider_request_id,omitempty"`
	CommentID         string               `json:"comment_id,omitempty"`
	ReasonCode        string               `json:"reason_code,omitempty"`
	ObservedAt        time.Time            `json:"observed_at"`
}

type EventType string

const (
	EventRequested           EventType = "requested"
	EventDispatchStarted     EventType = "dispatch_started"
	EventPublished           EventType = "published"
	EventRejected            EventType = "rejected"
	EventOutcomeUnknown      EventType = "outcome_unknown"
	EventReconciledPublished EventType = "reconciled_published"
	EventReconciledNotFound  EventType = "reconciled_not_found"
)

type LedgerEvent struct {
	SchemaVersion   string           `json:"schema_version"`
	EventID         string           `json:"event_id"`
	Type            EventType        `json:"type"`
	PublicationID   string           `json:"publication_id"`
	RequestSHA256   string           `json:"request_sha256"`
	Request         *Request         `json:"request,omitempty"`
	Revalidation    *Revalidation    `json:"revalidation,omitempty"`
	ProviderRequest *ProviderRequest `json:"provider_request,omitempty"`
	ProviderResult  *ProviderResult  `json:"provider_result,omitempty"`
	ReasonCode      string           `json:"reason_code,omitempty"`
	OccurredAt      time.Time        `json:"occurred_at"`
}

type State string

const (
	StateRequested   State = "requested"
	StateDispatching State = "dispatching"
	StatePublished   State = "published"
	StateRejected    State = "rejected"
	StateUnknown     State = "unknown"
	StateNotFound    State = "not_found"
)

type Record struct {
	RequestSHA256   string           `json:"request_sha256"`
	Request         Request          `json:"request"`
	State           State            `json:"state"`
	Revalidation    *Revalidation    `json:"revalidation,omitempty"`
	ProviderRequest *ProviderRequest `json:"provider_request,omitempty"`
	ProviderResult  *ProviderResult  `json:"provider_result,omitempty"`
	Events          []LedgerEvent    `json:"events"`
}

type Provider interface {
	Revalidate(context.Context, Request) (Revalidation, error)
	Publish(context.Context, ProviderRequest) (ProviderResult, error)
	Lookup(context.Context, ProviderRequest) (ProviderResult, error)
}

// RequestAuthorizer revalidates Argus-owned publication authority against the
// current committed Finding/Decision/config state. Provider permission checks
// remain separate and are performed by Provider.Revalidate.
type RequestAuthorizer interface {
	AuthorizePublication(context.Context, Request) error
}
