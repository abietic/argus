package publication

import "time"

const (
	GrantRequestSchemaVersion     = "argus.publication_grant_request.v1alpha1"
	GrantMutationSchemaVersion    = "argus.publication_grant_mutation.v1alpha1"
	GrantSchemaVersion            = "argus.publication_grant.v1alpha1"
	GrantReservationSchemaVersion = "argus.publication_grant_reservation.v1alpha1"
)

const GrantMaxLifetime = 24 * time.Hour

type GrantActorKind string

const (
	GrantActorHuman   GrantActorKind = "human"
	GrantActorService GrantActorKind = "service"
)

type GrantActor struct {
	Kind GrantActorKind `json:"kind"`
	ID   string         `json:"id"`
}

type GrantRole string

const GrantRolePublicationApprover GrantRole = "publication_approver"

// GrantRequest is the narrow post-review authorization request. All source,
// repository, Decision, and snapshot bindings are resolved by control-plane.
type GrantRequest struct {
	SchemaVersion              string    `json:"schema_version"`
	GrantID                    string    `json:"grant_id"`
	PublicationID              string    `json:"publication_id"`
	PublicationIdempotencyKey  string    `json:"publication_idempotency_key"`
	RunID                      string    `json:"run_id"`
	FindingID                  string    `json:"finding_id"`
	Provider                   string    `json:"provider"`
	RepositoryID               string    `json:"repository_id"`
	ChangeKind                 string    `json:"change_kind"`
	ChangeID                   string    `json:"change_id"`
	Channel                    string    `json:"channel"`
	ExpectedPermissionRevision string    `json:"expected_permission_revision"`
	OccurredAt                 time.Time `json:"occurred_at"`
	ExpiresAt                  time.Time `json:"expires_at"`
}

type GrantMutation struct {
	SchemaVersion  string      `json:"schema_version"`
	IdempotencyKey string      `json:"idempotency_key"`
	Actor          GrantActor  `json:"actor"`
	Roles          []GrantRole `json:"roles"`
	Audit          string      `json:"audit"`
	At             time.Time   `json:"at"`
}

// GrantBinding contains only control-plane-derived immutable facts.
type GrantBinding struct {
	TenantID              string     `json:"tenant_id"`
	WorkspaceID           string     `json:"workspace_id"`
	DecisionID            string     `json:"decision_id"`
	Provider              string     `json:"provider"`
	RepositoryID          string     `json:"repository_id"`
	BaseRevision          string     `json:"base_revision"`
	ExpectedHeadRevision  string     `json:"expected_head_revision"`
	TargetSnapshotRef     ContentRef `json:"target_snapshot_ref"`
	ConfigBundleRef       ContentRef `json:"config_bundle_ref"`
	FindingSourceRef      ContentRef `json:"finding_source_ref"`
	FindingSourceContract string     `json:"finding_source_contract"`
}

// Grant authorizes exactly one publication identity. It is distinct from the
// review ExecutionSnapshot and never expands Agent/tool authority.
type Grant struct {
	SchemaVersion string `json:"schema_version"`
	GrantSHA256   string `json:"grant_sha256"`

	GrantID                    string `json:"grant_id"`
	PublicationID              string `json:"publication_id"`
	PublicationIdempotencyKey  string `json:"publication_idempotency_key"`
	RunID                      string `json:"run_id"`
	FindingID                  string `json:"finding_id"`
	ChangeKind                 string `json:"change_kind"`
	ChangeID                   string `json:"change_id"`
	Channel                    string `json:"channel"`
	ExpectedPermissionRevision string `json:"expected_permission_revision"`

	TenantID              string     `json:"tenant_id"`
	WorkspaceID           string     `json:"workspace_id"`
	DecisionID            string     `json:"decision_id"`
	Provider              string     `json:"provider"`
	RepositoryID          string     `json:"repository_id"`
	BaseRevision          string     `json:"base_revision"`
	ExpectedHeadRevision  string     `json:"expected_head_revision"`
	TargetSnapshotRef     ContentRef `json:"target_snapshot_ref"`
	ConfigBundleRef       ContentRef `json:"config_bundle_ref"`
	FindingSourceRef      ContentRef `json:"finding_source_ref"`
	FindingSourceContract string     `json:"finding_source_contract"`

	OccurredAt     time.Time   `json:"occurred_at"`
	GrantedAt      time.Time   `json:"granted_at"`
	ExpiresAt      time.Time   `json:"expires_at"`
	GrantedBy      GrantActor  `json:"granted_by"`
	Roles          []GrantRole `json:"roles"`
	Audit          string      `json:"audit"`
	IdempotencyKey string      `json:"idempotency_key"`
}

// GrantReservation is the durable single-use admission fact written before
// publication service records its requested event.
type GrantReservation struct {
	SchemaVersion  string    `json:"schema_version"`
	GrantID        string    `json:"grant_id"`
	GrantSHA256    string    `json:"grant_sha256"`
	PublicationID  string    `json:"publication_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	ReservedAt     time.Time `json:"reserved_at"`
}
