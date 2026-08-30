// Package artifactrepo provides the Argus-owned authorization and integrity
// boundary for logical artifact references. Provider URIs are identifiers, not
// directly fetchable locations.
package artifactrepo

import (
	"errors"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

const (
	metadataSchemaVersion = "argus.artifact_metadata.v1alpha1"
	eventSchemaVersion    = "argus.artifact_event.v1alpha1"
	mutationSchemaVersion = "argus.artifact_mutation.v1alpha1"

	DefaultAuthority = "argus-local"
)

var (
	ErrUnauthorized  = errors.New("artifact access is unauthorized")
	ErrNotFound      = errors.New("artifact metadata not found")
	ErrConflict      = errors.New("artifact mutation conflicts with history")
	ErrQuarantined   = errors.New("artifact is quarantined")
	ErrTombstoned    = errors.New("artifact is tombstoned")
	ErrUseDenied     = errors.New("artifact use is denied")
	ErrAuditConflict = errors.New("artifact access audit conflicts with history")
	ErrCorrupt       = errors.New("artifact metadata is corrupt")
)

type Use string

const (
	UseRead        Use = "read"
	UsePublication Use = "publication"
	UseEvaluation  Use = "evaluation"
	UseTraining    Use = "training"
	UseExport      Use = "export"
	// UseSensitiveProcess is reserved for trusted host validation and bounded
	// derivation. It must never be used as a user-facing disclosure path.
	UseSensitiveProcess Use = "sensitive_process"
	// UseSensitiveRead is an explicit disclosure of sensitive bytes. Resolve
	// deliberately rejects it; callers must use ResolveSensitive so an audit
	// receipt is durably accepted before any bytes are returned.
	UseSensitiveRead Use = "sensitive_read"
)

const (
	RoleIntegrityOperator  = "artifact_integrity_operator"
	RolePublicationUse     = "artifact_publication_use"
	RoleEvaluationUse      = "artifact_evaluation_use"
	RoleTrainingUse        = "artifact_training_use"
	RoleExportUse          = "artifact_export_use"
	RoleSensitiveProcessor = "artifact_sensitive_processor"
	RoleSensitiveReader    = "artifact_sensitive_reader"
)

type SensitiveAccessPurpose string

const (
	SensitiveAccessLocalDebug            SensitiveAccessPurpose = "local_debug"
	SensitiveAccessEvaluationReplay      SensitiveAccessPurpose = "evaluation_replay"
	SensitiveAccessIncidentInvestigation SensitiveAccessPurpose = "incident_investigation"
)

type SensitiveAccessRequest struct {
	RequestID string                 `json:"request_id"`
	Actor     string                 `json:"actor"`
	Purpose   SensitiveAccessPurpose `json:"purpose"`
	At        time.Time              `json:"at"`
}

// SensitiveAccessReceipt is an immutable authorization fact. Persistence of
// this receipt precedes content delivery; therefore a durable receipt proves
// authorization was granted, while it deliberately does not claim that the
// caller successfully consumed the returned bytes.
type SensitiveAccessReceipt struct {
	SchemaVersion string                 `json:"schema_version"`
	ReceiptID     string                 `json:"receipt_id"`
	RequestID     string                 `json:"request_id"`
	Authority     string                 `json:"authority"`
	TenantID      string                 `json:"tenant_id"`
	WorkspaceID   string                 `json:"workspace_id"`
	ObjectID      string                 `json:"object_id"`
	Contract      string                 `json:"contract"`
	ContentSHA256 string                 `json:"content_sha256"`
	SizeBytes     int64                  `json:"size_bytes"`
	Actor         string                 `json:"actor"`
	Purpose       SensitiveAccessPurpose `json:"purpose"`
	AuthorizedAt  time.Time              `json:"authorized_at"`
}

type SensitiveAccessProof struct {
	Receipt       SensitiveAccessReceipt `json:"receipt"`
	AuditStream   string                 `json:"audit_stream"`
	AuditSequence uint64                 `json:"audit_sequence"`
}

type State string

const (
	StateActive      State = "active"
	StateQuarantined State = "quarantined"
	StateTombstoned  State = "tombstoned"
)

type Subject struct {
	TenantID    string   `json:"tenant_id"`
	WorkspaceID string   `json:"workspace_id"`
	Roles       []string `json:"roles"`
}

type Ref struct {
	URI       string `json:"uri"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Contract  string `json:"contract"`
}

type PutRequest struct {
	Authority   string
	TenantID    string
	WorkspaceID string
	Contract    string
	AllowedUses []Use
	Content     []byte
	Mutation    Mutation
}

type Mutation struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

type Metadata struct {
	SchemaVersion string            `json:"schema_version"`
	ObjectID      string            `json:"object_id"`
	Authority     string            `json:"authority"`
	TenantID      string            `json:"tenant_id"`
	WorkspaceID   string            `json:"workspace_id"`
	Contract      string            `json:"contract"`
	AllowedUses   []Use             `json:"allowed_uses"`
	Content       local.ArtifactRef `json:"content"`
	CreatedBy     string            `json:"created_by"`
	CreatedAt     time.Time         `json:"created_at"`
}

type Record struct {
	Metadata          Metadata  `json:"metadata"`
	State             State     `json:"state"`
	StateReason       string    `json:"state_reason,omitempty"`
	StateChangedAt    time.Time `json:"state_changed_at"`
	LastMutationActor string    `json:"last_mutation_actor"`
}

type eventType string

const (
	eventCreated            eventType = "created"
	eventQuarantined        eventType = "quarantined"
	eventQuarantineReleased eventType = "quarantine_released"
	eventTombstoned         eventType = "tombstoned"
)

type artifactEvent struct {
	SchemaVersion string    `json:"schema_version"`
	Type          eventType `json:"type"`
	Metadata      *Metadata `json:"metadata,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Mutation      Mutation  `json:"mutation"`
}

type mutationOperation string

const (
	mutationPut               mutationOperation = "put"
	mutationQuarantine        mutationOperation = "quarantine"
	mutationReleaseQuarantine mutationOperation = "release_quarantine"
	mutationTombstone         mutationOperation = "tombstone"
)

// mutationBinding reserves one idempotency key within an authority,
// tenant, and workspace. Object streams remain independently append-only,
// while this binding prevents a retry key from being reused for another
// object or operation.
type mutationBinding struct {
	SchemaVersion string            `json:"schema_version"`
	Operation     mutationOperation `json:"operation"`
	Authority     string            `json:"authority"`
	TenantID      string            `json:"tenant_id"`
	WorkspaceID   string            `json:"workspace_id"`
	ObjectID      string            `json:"object_id"`
	Reason        string            `json:"reason,omitempty"`
	Mutation      Mutation          `json:"mutation"`
}
