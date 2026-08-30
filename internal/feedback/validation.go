package feedback

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxIDBytes       = 256
	maxRevisionBytes = 512
	maxSourceRefs    = 64
)

func (feedback Feedback) Validate() error {
	if feedback.SchemaVersion != FeedbackSchemaVersion {
		return fmt.Errorf("unsupported feedback schema %q", feedback.SchemaVersion)
	}
	for name, value := range map[string]string{
		"feedback_id":     feedback.FeedbackID,
		"finding_id":      feedback.FindingID,
		"run_id":          feedback.RunID,
		"idempotency_key": feedback.IdempotencyKey,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	switch feedback.Action {
	case FeedbackAccept, FeedbackDismiss, FeedbackWontFix, FeedbackOutdated,
		FeedbackNeedsDiscussion:
	default:
		return fmt.Errorf("unsupported feedback action %q", feedback.Action)
	}
	if err := feedback.Actor.Validate(); err != nil {
		return fmt.Errorf("actor: %w", err)
	}
	if err := feedback.Source.Validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if err := validateTimes(feedback.OccurredAt, feedback.RecordedAt); err != nil {
		return err
	}
	if err := validateSourceRefs(feedback.SourceRefs); err != nil {
		return err
	}
	if feedback.PriorFeedbackID != "" {
		if err := validateID("prior_feedback_id", feedback.PriorFeedbackID); err != nil {
			return err
		}
		if feedback.PriorFeedbackID == feedback.FeedbackID {
			return fmt.Errorf("prior_feedback_id must not reference feedback_id itself")
		}
	}
	return nil
}

func (outcome Outcome) Validate() error {
	if outcome.SchemaVersion != OutcomeSchemaVersion {
		return fmt.Errorf("unsupported outcome schema %q", outcome.SchemaVersion)
	}
	for name, value := range map[string]string{
		"outcome_id":      outcome.OutcomeID,
		"finding_id":      outcome.FindingID,
		"run_id":          outcome.RunID,
		"idempotency_key": outcome.IdempotencyKey,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	switch outcome.State {
	case OutcomeFixed, OutcomeRecurred, OutcomeEscaped, OutcomeUnknown:
	default:
		return fmt.Errorf("unsupported outcome state %q", outcome.State)
	}
	if err := outcome.Actor.Validate(); err != nil {
		return fmt.Errorf("actor: %w", err)
	}
	if err := outcome.Source.Validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if err := validateTimes(outcome.OccurredAt, outcome.RecordedAt); err != nil {
		return err
	}
	if err := outcome.Window.Validate(); err != nil {
		return fmt.Errorf("attribution_window: %w", err)
	}
	occurred := outcome.OccurredAt.UTC()
	if occurred.Before(outcome.Window.Start.UTC()) || occurred.After(outcome.Window.End.UTC()) {
		return fmt.Errorf("occurred_at must be within attribution_window")
	}
	if err := validateSourceRefs(outcome.SourceRefs); err != nil {
		return err
	}
	if outcome.PriorOutcomeID != "" {
		if err := validateID("prior_outcome_id", outcome.PriorOutcomeID); err != nil {
			return err
		}
		if outcome.PriorOutcomeID == outcome.OutcomeID {
			return fmt.Errorf("prior_outcome_id must not reference outcome_id itself")
		}
	}
	return nil
}

func (actor ActorRef) Validate() error {
	switch actor.Kind {
	case ActorHuman, ActorService:
	default:
		return fmt.Errorf("unsupported actor kind %q", actor.Kind)
	}
	return validateID("actor.id", actor.ID)
}

func (source Source) Validate() error {
	switch source.Kind {
	case SourceUserInterface, SourceAPI, SourceCodeHost, SourceCI, SourceIncident,
		SourceManual, SourceImport, SourceSystem:
	default:
		return fmt.Errorf("unsupported source kind %q", source.Kind)
	}
	return validateID("source.id", source.ID)
}

func (window AttributionWindow) Validate() error {
	if window.Start.IsZero() || window.End.IsZero() {
		return fmt.Errorf("start and end are required")
	}
	if window.End.Before(window.Start) {
		return fmt.Errorf("end must not precede start")
	}
	return nil
}

func (ref SourceRef) Validate() error {
	switch ref.Kind {
	case SourceRefArtifact, SourceRefEvent, SourceRefChange, SourceRefComment,
		SourceRefCIRun, SourceRefIncident, SourceRefIssue, SourceRefManualObservation:
	default:
		return fmt.Errorf("unsupported source ref kind %q", ref.Kind)
	}
	if err := validateID("source_ref.authority", ref.Authority); err != nil {
		return err
	}
	if err := validateID("source_ref.id", ref.ID); err != nil {
		return err
	}
	if ref.Revision != "" {
		if err := validateSafeText("source_ref.revision", ref.Revision, maxRevisionBytes); err != nil {
			return err
		}
	}
	if ref.SHA256 != "" {
		if err := validateSHA256("source_ref.sha256", ref.SHA256); err != nil {
			return err
		}
	}
	if ref.Kind == SourceRefArtifact && ref.SHA256 == "" {
		return fmt.Errorf("artifact source ref requires sha256")
	}
	return nil
}

func validateTimes(occurredAt, recordedAt time.Time) error {
	if occurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	if recordedAt.IsZero() {
		return fmt.Errorf("recorded_at is required")
	}
	if recordedAt.Before(occurredAt) {
		return fmt.Errorf("recorded_at must not precede occurred_at")
	}
	return nil
}

func validateSourceRefs(refs []SourceRef) error {
	if len(refs) == 0 {
		return fmt.Errorf("source_refs must be a non-empty array")
	}
	if len(refs) > maxSourceRefs {
		return fmt.Errorf("source_refs exceeds %d entries", maxSourceRefs)
	}
	keys := make([]string, len(refs))
	for index, ref := range refs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("source_refs[%d]: %w", index, err)
		}
		keys[index] = sourceRefKey(ref)
	}
	if !slices.IsSorted(keys) {
		return fmt.Errorf("source_refs must be sorted canonically")
	}
	for index := 1; index < len(keys); index++ {
		if keys[index] == keys[index-1] {
			return fmt.Errorf("source_refs contains duplicate entries")
		}
	}
	return nil
}

func sourceRefKey(ref SourceRef) string {
	return strings.Join([]string{
		string(ref.Kind), ref.Authority, ref.ID, ref.Revision, ref.SHA256,
	}, "\x00")
}

func validateID(name, value string) error {
	if err := validateSafeText(name, value, maxIDBytes); err != nil {
		return err
	}
	if strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%s must not contain path separators", name)
	}
	return nil
}

func validateSafeText(name, value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be a non-empty safe value of at most %d bytes", name, maxBytes)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	return nil
}
