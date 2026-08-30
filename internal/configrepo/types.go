// Package configrepo persists immutable review configuration revisions and
// replays their append-only lifecycle into an active, mode-aware projection.
package configrepo

import (
	"errors"
	"time"

	"argus.local/argus/internal/reviewconfig"
)

const (
	storedRevisionSchemaVersion = "argus.config_stored_revision.v1alpha1"
	lifecycleEventSchemaVersion = "argus.config_lifecycle_event.v1alpha1"
	lifecycleStream             = "config/lifecycle"
)

var (
	ErrNotFound          = errors.New("config revision not found")
	ErrNoPublishedConfig = errors.New("no published config applies to the resolution context")
	ErrConflict          = errors.New("config revision conflict")
	ErrInvalidTransition = errors.New("invalid config lifecycle transition")
	ErrCorrupt           = errors.New("corrupt config lifecycle")
)

type Status string

const (
	StatusDraft      Status = "draft"
	StatusValidated  Status = "validated"
	StatusPublished  Status = "published"
	StatusSuperseded Status = "superseded"
	StatusRolledBack Status = "rolled_back"
)

type EventType string

const (
	EventCreated         EventType = "created"
	EventValidated       EventType = "validated"
	EventPublished       EventType = "published"
	EventRolloutAdvanced EventType = "rollout_advanced"
	EventRolledBack      EventType = "rolled_back"
)

// Mutation is mandatory audit metadata for every lifecycle change. At is part
// of the idempotent event identity: a retry must reuse the exact same values.
type Mutation struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

// Rollout controls one selector's deterministic assignment. A percentage below
// 100 requires a previous published baseline and a non-empty seed.
type Rollout struct {
	Percentage int    `json:"percentage"`
	Seed       string `json:"seed,omitempty"`
}

type Record struct {
	Revision  reviewconfig.Revision `json:"revision"`
	SHA256    string                `json:"sha256"`
	Status    Status                `json:"status"`
	CreatedAt time.Time             `json:"created_at"`
	CreatedBy string                `json:"created_by"`
	UpdatedAt time.Time             `json:"updated_at"`
	UpdatedBy string                `json:"updated_by"`
}

type AuditEntry struct {
	Sequence   uint64    `json:"sequence"`
	EventID    string    `json:"event_id"`
	Type       EventType `json:"type"`
	RevisionID string    `json:"revision_id"`
	Revision   string    `json:"revision"`
	SHA256     string    `json:"sha256"`
	Status     Status    `json:"status"`
	Actor      string    `json:"actor"`
	Audit      string    `json:"audit"`
	At         time.Time `json:"at"`
	Rollout    *Rollout  `json:"rollout,omitempty"`
}

// RecordDetail is one coherent lifecycle projection and its append-only audit
// history loaded under the same repository lock.
type RecordDetail struct {
	Record  Record       `json:"record"`
	History []AuditEntry `json:"history"`
}

type storedRevision struct {
	SchemaVersion string                `json:"schema_version"`
	SHA256        string                `json:"sha256"`
	Revision      reviewconfig.Revision `json:"revision"`
}

type lifecycleEvent struct {
	SchemaVersion string    `json:"schema_version"`
	Type          EventType `json:"type"`
	RevisionID    string    `json:"revision_id"`
	Revision      string    `json:"revision"`
	SHA256        string    `json:"sha256"`
	Actor         string    `json:"actor"`
	Audit         string    `json:"audit"`
	OccurredAt    time.Time `json:"occurred_at"`
	Rollout       *Rollout  `json:"rollout,omitempty"`
}

type revisionKey struct {
	ID       string
	Revision string
}

type activation struct {
	Key             revisionKey
	Percentage      int
	Seed            string
	PublishSequence uint64
}

type activeProjection struct {
	Baseline *activation
	Rollout  *activation
}

type publishFrame struct {
	Before activeProjection
}
