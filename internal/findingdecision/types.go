// Package findingdecision owns append-only human/authorized decisions made
// after a Finding's immutable initial adjudication. It never mutates the
// source FindingSet or GovernedReviewReport and does not perform publication.
package findingdecision

import "time"

const (
	RequestSchemaVersion     = "argus.finding_decision_request.v1alpha1"
	DecisionSchemaVersion    = "argus.finding_decision.v1alpha1"
	MutationSchemaVersion    = "argus.finding_decision_mutation.v1alpha1"
	RootSchemaVersion        = "argus.finding_decision_root.v1alpha1"
	LedgerEventSchemaVersion = "argus.finding_decision_ledger_event.v1alpha1"
)

type Action string

const (
	ActionPublish     Action = "publish"
	ActionReject      Action = "reject"
	ActionHumanReview Action = "human_review"
)

type Role string

const (
	RoleFindingReviewer     Role = "finding_reviewer"
	RolePublicationApprover Role = "publication_approver"
)

type ActorKind string

const (
	ActorHuman   ActorKind = "human"
	ActorService ActorKind = "service"
)

type Actor struct {
	Kind ActorKind `json:"kind"`
	ID   string    `json:"id"`
}

type EvidenceRef struct {
	Authority string `json:"authority"`
	ID        string `json:"id"`
	Revision  string `json:"revision,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type Request struct {
	SchemaVersion string        `json:"schema_version"`
	RunID         string        `json:"run_id"`
	FindingID     string        `json:"finding_id"`
	Action        Action        `json:"action"`
	ReasonCode    string        `json:"reason_code"`
	EvidenceRefs  []EvidenceRef `json:"evidence_refs"`
	OccurredAt    time.Time     `json:"occurred_at"`
}

type Mutation struct {
	SchemaVersion  string    `json:"schema_version"`
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          Actor     `json:"actor"`
	Roles          []Role    `json:"roles"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

// Root binds the mutable decision chain to the exact immutable initial
// decision and source artifact. It is resolved by the control plane, never by
// a caller-supplied CLI descriptor.
type Root struct {
	SchemaVersion     string `json:"schema_version"`
	RunID             string `json:"run_id"`
	FindingID         string `json:"finding_id"`
	InitialDecisionID string `json:"initial_decision_id"`
	SourceContract    string `json:"source_contract"`
	SourceSHA256      string `json:"source_sha256"`
}

type Decision struct {
	SchemaVersion     string        `json:"schema_version"`
	DecisionID        string        `json:"decision_id"`
	RunID             string        `json:"run_id"`
	FindingID         string        `json:"finding_id"`
	Sequence          uint32        `json:"sequence"`
	PriorDecisionID   string        `json:"prior_decision_id"`
	InitialDecisionID string        `json:"initial_decision_id"`
	SourceContract    string        `json:"source_contract"`
	SourceSHA256      string        `json:"source_sha256"`
	Action            Action        `json:"action"`
	ReasonCode        string        `json:"reason_code"`
	EvidenceRefs      []EvidenceRef `json:"evidence_refs"`
	OccurredAt        time.Time     `json:"occurred_at"`
	RecordedAt        time.Time     `json:"recorded_at"`
	RecordedBy        Actor         `json:"recorded_by"`
	Roles             []Role        `json:"roles"`
	Audit             string        `json:"audit"`
	IdempotencyKey    string        `json:"idempotency_key"`
}
