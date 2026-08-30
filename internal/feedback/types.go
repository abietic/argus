// Package feedback owns the user-feedback and engineering-outcome ledgers.
//
// Feedback and Outcome are evidence about a Finding. They are not
// FindingDecision records and never mutate the Finding or an earlier ledger
// fact. A correction is a new fact linked through PriorFeedbackID or
// PriorOutcomeID.
package feedback

import "time"

const (
	FeedbackSchemaVersion = "argus.feedback.v1alpha1"
	OutcomeSchemaVersion  = "argus.outcome.v1alpha1"
)

type FeedbackAction string

const (
	FeedbackAccept          FeedbackAction = "accept"
	FeedbackDismiss         FeedbackAction = "dismiss"
	FeedbackWontFix         FeedbackAction = "wont_fix"
	FeedbackOutdated        FeedbackAction = "outdated"
	FeedbackNeedsDiscussion FeedbackAction = "needs_discussion"
)

type OutcomeState string

const (
	OutcomeFixed    OutcomeState = "fixed"
	OutcomeRecurred OutcomeState = "recurred"
	OutcomeEscaped  OutcomeState = "escaped"
	OutcomeUnknown  OutcomeState = "unknown"
)

type ActorKind string

const (
	ActorHuman   ActorKind = "human"
	ActorService ActorKind = "service"
)

// ActorRef is the principal that asserted the fact, not merely the transport
// that delivered it.
type ActorRef struct {
	Kind ActorKind `json:"kind"`
	ID   string    `json:"id"`
}

type SourceKind string

const (
	SourceUserInterface SourceKind = "user_interface"
	SourceAPI           SourceKind = "api"
	SourceCodeHost      SourceKind = "code_host"
	SourceCI            SourceKind = "ci"
	SourceIncident      SourceKind = "incident"
	SourceManual        SourceKind = "manual"
	SourceImport        SourceKind = "import"
	SourceSystem        SourceKind = "system"
)

// Source identifies the channel/integration that recorded the assertion.
// Actor and Source intentionally remain separate: a human can act through a
// code-host integration, while a service can import a human-authored event.
type Source struct {
	Kind SourceKind `json:"kind"`
	ID   string     `json:"id"`
}

type SourceRefKind string

const (
	SourceRefArtifact          SourceRefKind = "artifact"
	SourceRefEvent             SourceRefKind = "event"
	SourceRefChange            SourceRefKind = "change"
	SourceRefComment           SourceRefKind = "comment"
	SourceRefCIRun             SourceRefKind = "ci_run"
	SourceRefIncident          SourceRefKind = "incident"
	SourceRefIssue             SourceRefKind = "issue"
	SourceRefManualObservation SourceRefKind = "manual_observation"
)

// SourceRef binds a fact to independently inspectable provenance. Authority
// names the owning system and ID is opaque within that authority. SHA256 is
// mandatory for immutable artifacts and optional for externally versioned
// records.
type SourceRef struct {
	Kind      SourceRefKind `json:"kind"`
	Authority string        `json:"authority"`
	ID        string        `json:"id"`
	Revision  string        `json:"revision,omitempty"`
	SHA256    string        `json:"sha256,omitempty"`
}

type AttributionWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type Feedback struct {
	SchemaVersion   string         `json:"schema_version"`
	FeedbackID      string         `json:"feedback_id"`
	FindingID       string         `json:"finding_id"`
	RunID           string         `json:"run_id"`
	Action          FeedbackAction `json:"action"`
	Actor           ActorRef       `json:"actor"`
	Source          Source         `json:"source"`
	OccurredAt      time.Time      `json:"occurred_at"`
	RecordedAt      time.Time      `json:"recorded_at"`
	SourceRefs      []SourceRef    `json:"source_refs"`
	PriorFeedbackID string         `json:"prior_feedback_id,omitempty"`
	IdempotencyKey  string         `json:"idempotency_key"`
}

type Outcome struct {
	SchemaVersion  string            `json:"schema_version"`
	OutcomeID      string            `json:"outcome_id"`
	FindingID      string            `json:"finding_id"`
	RunID          string            `json:"run_id"`
	State          OutcomeState      `json:"state"`
	Actor          ActorRef          `json:"actor"`
	Source         Source            `json:"source"`
	OccurredAt     time.Time         `json:"occurred_at"`
	RecordedAt     time.Time         `json:"recorded_at"`
	Window         AttributionWindow `json:"attribution_window"`
	SourceRefs     []SourceRef       `json:"source_refs"`
	PriorOutcomeID string            `json:"prior_outcome_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

// EvaluationUse deliberately has no "gold" value. Production feedback can
// enter a governed candidate pool, but cannot become a gold label directly.
type EvaluationUse string

const EvaluationUseCandidateOnly EvaluationUse = "candidate_only"

type EvaluationCandidateEligibility struct {
	Use                      EvaluationUse `json:"use"`
	FeedbackID               string        `json:"feedback_id"`
	FindingID                string        `json:"finding_id"`
	RunID                    string        `json:"run_id"`
	GovernanceReviewRequired bool          `json:"governance_review_required"`
	ReasonCodes              []string      `json:"reason_codes"`
}

// EvaluationCandidateEligibility returns the only evaluation projection a
// Feedback fact can produce directly. Governance must independently review
// provenance, consent, classification, and label policy before case creation.
func (feedback Feedback) EvaluationCandidateEligibility() EvaluationCandidateEligibility {
	reason := "feedback_requires_label_governance"
	switch feedback.Action {
	case FeedbackAccept:
		reason = "accept_is_candidate_positive_evidence"
	case FeedbackDismiss:
		reason = "dismiss_is_candidate_negative_evidence"
	case FeedbackWontFix:
		reason = "wont_fix_is_not_a_correctness_label"
	case FeedbackOutdated:
		reason = "outdated_is_target_staleness_evidence"
	case FeedbackNeedsDiscussion:
		reason = "needs_discussion_is_ambiguous_evidence"
	}
	return EvaluationCandidateEligibility{
		Use:                      EvaluationUseCandidateOnly,
		FeedbackID:               feedback.FeedbackID,
		FindingID:                feedback.FindingID,
		RunID:                    feedback.RunID,
		GovernanceReviewRequired: true,
		ReasonCodes: []string{
			reason,
			"production_feedback_cannot_be_direct_gold",
		},
	}
}

type FactKind string

const (
	FactFeedback FactKind = "feedback"
	FactOutcome  FactKind = "outcome"
)

// Entry preserves the authoritative cross-ledger append order while keeping
// Feedback and Outcome as distinct typed facts.
type Entry struct {
	Sequence uint64
	Kind     FactKind
	Feedback *Feedback
	Outcome  *Outcome
}
