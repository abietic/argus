package analytics

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (window TimeWindow) Validate() error {
	if err := validateUTC("start_inclusive", window.StartInclusive); err != nil {
		return err
	}
	if err := validateUTC("end_exclusive", window.EndExclusive); err != nil {
		return err
	}
	if !window.StartInclusive.Before(window.EndExclusive) {
		return fmt.Errorf("time window must be non-empty and increasing")
	}
	return nil
}

func (window TimeWindow) contains(value time.Time) bool {
	return !value.Before(window.StartInclusive) && value.Before(window.EndExclusive)
}

func (dimensions Dimensions) Validate() error {
	for name, value := range map[string]string{
		"tenant_id":         dimensions.TenantID,
		"organization_id":   dimensions.OrganizationID,
		"repository_id":     dimensions.RepositoryID,
		"workflow_revision": dimensions.WorkflowRevision,
		"config_revision":   dimensions.ConfigRevision,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"language":         dimensions.Language,
		"rule_id":          dimensions.RuleID,
		"review_dimension": dimensions.ReviewDimension,
	} {
		if value != "" {
			if err := validateIdentifier(name, value); err != nil {
				return err
			}
		}
	}
	if dimensions.Path != "" {
		if err := validateRepositoryPath("path", dimensions.Path); err != nil {
			return err
		}
	}
	return nil
}

func (ref SourceRef) Validate() error {
	switch ref.Kind {
	case SourceReviewRun, SourceStage, SourceFinding, SourcePublication,
		SourceFeedback, SourceOutcome, SourceExperiment, SourceRepeatability, SourceEvaluation,
		SourceBaseline, SourceModelEstimate:
	default:
		return fmt.Errorf("unsupported source kind %q", ref.Kind)
	}
	if err := validateReferenceID("source_ref.id", ref.ID); err != nil {
		return err
	}
	if err := validateIdentifier("source_ref.revision", ref.Revision); err != nil {
		return err
	}
	return validateSHA256("source_ref.sha256", ref.SHA256)
}

func (summary FunnelSummary) Validate() error {
	if summary.Normalized > summary.Candidates {
		return fmt.Errorf("normalized count exceeds candidate count")
	}
	if summary.Verified > summary.Normalized {
		return fmt.Errorf("verified count exceeds normalized count")
	}
	if summary.Published > summary.Verified {
		return fmt.Errorf("published count exceeds verified count")
	}
	return nil
}

func (fact ReviewRunFact) Validate() error {
	if fact.SchemaVersion != ReviewRunFactSchemaVersion {
		return fmt.Errorf("unsupported review run fact schema %q", fact.SchemaVersion)
	}
	if err := validateIdentifier("fact_id", fact.FactID); err != nil {
		return err
	}
	if err := validateIdentifier("run_id", fact.RunID); err != nil {
		return err
	}
	if err := fact.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	switch fact.Status {
	case RunStatusStarted, RunStatusSucceeded, RunStatusFailed, RunStatusCanceled:
	default:
		return fmt.Errorf("unsupported run status %q", fact.Status)
	}
	if err := validateCompleteness(
		"review_run", fact.ResultCompleteness, fact.IncompleteReasons,
	); err != nil {
		return err
	}
	if fact.ResultCompleteness == CompletenessComplete && fact.Status != RunStatusSucceeded {
		return fmt.Errorf("only a succeeded run may claim a complete review result")
	}
	if fact.Funnel != nil {
		if err := fact.Funnel.Validate(); err != nil {
			return fmt.Errorf("funnel: %w", err)
		}
	} else if fact.ResultCompleteness == CompletenessComplete {
		return fmt.Errorf("complete review result requires an explicit funnel summary")
	}
	if err := validateUTC("started_at", fact.StartedAt); err != nil {
		return err
	}
	if err := validateUTC("occurred_at", fact.OccurredAt); err != nil {
		return err
	}
	if fact.Status == RunStatusStarted {
		if fact.FinishedAt != nil {
			return fmt.Errorf("started review run must not have finished_at")
		}
		if !fact.OccurredAt.Equal(fact.StartedAt) {
			return fmt.Errorf("started review run must occur at started_at")
		}
		return nil
	}
	if fact.FinishedAt == nil {
		return fmt.Errorf("terminal review run requires finished_at")
	}
	if err := validateUTC("finished_at", *fact.FinishedAt); err != nil {
		return err
	}
	if fact.FinishedAt.Before(fact.StartedAt) {
		return fmt.Errorf("finished_at precedes started_at")
	}
	if !fact.OccurredAt.Equal(*fact.FinishedAt) {
		return fmt.Errorf("terminal review run must occur at finished_at")
	}
	return nil
}

func (fact StageFact) Validate() error {
	if fact.SchemaVersion != StageFactSchemaVersion {
		return fmt.Errorf("unsupported stage fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id":        fact.FactID,
		"run_id":         fact.RunID,
		"stage_id":       fact.StageID,
		"stage_revision": fact.StageRevision,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if fact.Attempt == 0 {
		return fmt.Errorf("attempt must be positive")
	}
	if err := fact.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	switch fact.Status {
	case StageStatusStarted, StageStatusSucceeded, StageStatusFailed, StageStatusCanceled:
	default:
		return fmt.Errorf("unsupported stage status %q", fact.Status)
	}
	if err := validateCompleteness(
		"stage", fact.ResultCompleteness, fact.IncompleteReasons,
	); err != nil {
		return err
	}
	if fact.ResultCompleteness == CompletenessComplete && fact.Status != StageStatusSucceeded {
		return fmt.Errorf("only a succeeded stage may claim a complete result")
	}
	if fact.Status == StageStatusStarted && fact.DurationMicros != 0 {
		return fmt.Errorf("started stage must have zero duration")
	}
	return validateUTC("occurred_at", fact.OccurredAt)
}

func (fact ContextProviderFact) Validate() error {
	if fact.SchemaVersion != ContextProviderFactSchemaVersion {
		return fmt.Errorf("unsupported context provider fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id": fact.FactID, "run_id": fact.RunID, "receipt_id": fact.ReceiptID,
		"provider_id": fact.ProviderID, "provider_revision": fact.ProviderRevision,
		"kind": fact.Kind, "adapter_id": fact.AdapterID,
		"adapter_revision": fact.AdapterRevision, "context_id": fact.ContextID,
		"authority": fact.Authority,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"receipt_sha256":          fact.ReceiptSHA256,
		"receipt_artifact_sha256": fact.ReceiptArtifactSHA256,
		"adapter_sha256":          fact.AdapterSHA256,
		"request_sha256":          fact.RequestSHA256,
		"context_digest":          fact.ContextDigest,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if fact.Authority != "local_host_observation" {
		return fmt.Errorf("unsupported context provider authority %q", fact.Authority)
	}
	switch fact.BindingMode {
	case ContextProviderExecuted, ContextProviderReused:
	default:
		return fmt.Errorf("unsupported context provider binding mode %q", fact.BindingMode)
	}
	switch fact.Status {
	case ContextProviderSucceeded:
		if fact.ReasonCode != "" || fact.ContextContract == "" {
			return fmt.Errorf("succeeded provider fact requires a contract and no reason code")
		}
		if err := validateReferenceID("context_contract", fact.ContextContract); err != nil {
			return err
		}
	case ContextProviderGap:
		if err := validateIdentifier("reason_code", fact.ReasonCode); err != nil {
			return err
		}
		if fact.ContextContract != "" {
			return fmt.Errorf("gap provider fact must not claim a context contract")
		}
	default:
		return fmt.Errorf("unsupported context provider status %q", fact.Status)
	}
	if fact.TimeoutMicros == 0 {
		return fmt.Errorf("timeout_micros must be positive")
	}
	if err := fact.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	return validateUTC("occurred_at", fact.OccurredAt)
}

func (fact FindingFunnelFact) Validate() error {
	if fact.SchemaVersion != FindingFunnelFactSchemaVersion {
		return fmt.Errorf("unsupported finding funnel fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id":      fact.FactID,
		"run_id":       fact.RunID,
		"candidate_id": fact.CandidateID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := fact.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	if fact.Dimensions.Language == "" || fact.Dimensions.RuleID == "" ||
		fact.Dimensions.Path == "" {
		return fmt.Errorf("finding dimensions require language, rule_id, and path")
	}
	if err := validateCompleteness(
		"finding", fact.ResultCompleteness, fact.IncompleteReasons,
	); err != nil {
		return err
	}
	switch fact.Verification {
	case VerificationNotReached, VerificationVerified, VerificationRejected,
		VerificationInconclusive, VerificationUnknown:
	default:
		return fmt.Errorf("unsupported verification state %q", fact.Verification)
	}
	switch fact.Publication {
	case PublicationNotReached, PublicationRequested, PublicationDispatching,
		PublicationPublished, PublicationRejected, PublicationUnknown,
		PublicationNotFound:
	default:
		return fmt.Errorf("unsupported publication state %q", fact.Publication)
	}
	switch fact.PublicationEligibility {
	case EligibilityNotReached, EligibilityPublishEligible,
		EligibilitySuppressed, EligibilityHumanQueue:
	default:
		return fmt.Errorf(
			"unsupported publication eligibility %q",
			fact.PublicationEligibility,
		)
	}
	if !fact.Normalized {
		if fact.FindingID != "" || fact.Verification != VerificationNotReached ||
			fact.PublicationEligibility != EligibilityNotReached ||
			fact.Publication != PublicationNotReached ||
			fact.PublicationRef != nil || fact.PublicationAt != nil {
			return fmt.Errorf("unnormalized candidate cannot have downstream finding states")
		}
	} else {
		if err := validateIdentifier("finding_id", fact.FindingID); err != nil {
			return err
		}
		if fact.Verification == VerificationNotReached &&
			fact.ResultCompleteness == CompletenessComplete {
			return fmt.Errorf("complete normalized finding must reach verification")
		}
		if fact.PublicationEligibility == EligibilityNotReached &&
			fact.ResultCompleteness == CompletenessComplete {
			return fmt.Errorf(
				"complete normalized finding must have a publication eligibility decision",
			)
		}
	}
	if fact.Verification == VerificationUnknown &&
		fact.ResultCompleteness == CompletenessComplete {
		return fmt.Errorf("complete finding cannot have unknown verification")
	}
	if fact.Publication == PublicationPublished &&
		fact.Verification != VerificationVerified {
		return fmt.Errorf("published finding must be verified")
	}
	if fact.Publication == PublicationPublished &&
		fact.PublicationEligibility != EligibilityPublishEligible {
		return fmt.Errorf("published finding must bind publish eligibility")
	}
	if fact.PublicationEligibility == EligibilityPublishEligible &&
		fact.Verification != VerificationVerified {
		return fmt.Errorf("publish eligibility requires verified evidence")
	}
	if fact.PublicationEligibility == EligibilityHumanQueue &&
		fact.Verification != VerificationInconclusive &&
		fact.Verification != VerificationVerified {
		return fmt.Errorf("human queue requires verified or inconclusive evidence")
	}
	if fact.Verification == VerificationRejected &&
		fact.PublicationEligibility != EligibilitySuppressed {
		return fmt.Errorf("rejected finding must be decision-suppressed")
	}
	if fact.PublicationRef != nil {
		if err := fact.PublicationRef.Validate(); err != nil {
			return fmt.Errorf("publication_ref: %w", err)
		}
		if fact.PublicationRef.Kind != SourcePublication {
			return fmt.Errorf("publication_ref must bind publication evidence")
		}
	} else if fact.Publication != PublicationNotReached &&
		!(fact.Publication == PublicationUnknown &&
			fact.ResultCompleteness != CompletenessComplete) {
		return fmt.Errorf(
			"publication state %q requires ledger evidence",
			fact.Publication,
		)
	}
	if fact.Publication == PublicationNotReached && fact.PublicationRef != nil {
		return fmt.Errorf("not_reached publication must not carry ledger evidence")
	}
	if fact.Publication == PublicationPublished {
		if fact.PublicationRef == nil || fact.PublicationAt == nil {
			return fmt.Errorf("published finding requires provider publication evidence")
		}
		if err := validateUTC("publication_at", *fact.PublicationAt); err != nil {
			return err
		}
	} else if fact.PublicationAt != nil {
		return fmt.Errorf("only a published finding may carry publication_at")
	}
	return validateUTC("occurred_at", fact.OccurredAt)
}

func (fact FeedbackOutcomeFact) Validate() error {
	if fact.SchemaVersion != FeedbackOutcomeFactSchemaVersion {
		return fmt.Errorf("unsupported feedback/outcome fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id":    fact.FactID,
		"run_id":     fact.RunID,
		"finding_id": fact.FindingID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := fact.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	if err := validateCompleteness(
		"feedback_outcome", fact.ResultCompleteness, fact.IncompleteReasons,
	); err != nil {
		return err
	}
	switch fact.Feedback {
	case FeedbackNoFeedback, FeedbackAccepted, FeedbackDismissed, FeedbackWontFix,
		FeedbackOutdated, FeedbackUnknown:
	default:
		return fmt.Errorf("unsupported feedback state %q", fact.Feedback)
	}
	switch fact.Outcome {
	case OutcomeNoOutcome, OutcomeFixed, OutcomeRecurred, OutcomeEscaped, OutcomeUnknown:
	default:
		return fmt.Errorf("unsupported outcome state %q", fact.Outcome)
	}
	if fact.Feedback == FeedbackNoFeedback {
		if fact.FeedbackRef != nil || fact.FeedbackAt != nil {
			return fmt.Errorf("no_feedback must not claim a feedback source or time")
		}
	} else if fact.Feedback == FeedbackUnknown {
		if fact.FeedbackAt != nil {
			return fmt.Errorf("unknown feedback must not claim an event time")
		}
	} else if fact.FeedbackRef == nil || fact.FeedbackAt == nil {
		return fmt.Errorf("%s feedback requires feedback_ref and feedback_at", fact.Feedback)
	}
	if fact.FeedbackRef != nil {
		if err := fact.FeedbackRef.Validate(); err != nil {
			return fmt.Errorf("feedback_ref: %w", err)
		}
		if fact.FeedbackRef.Kind != SourceFeedback {
			return fmt.Errorf("feedback_ref must have kind %q", SourceFeedback)
		}
	}
	if fact.FeedbackAt != nil {
		if err := validateUTC("feedback_at", *fact.FeedbackAt); err != nil {
			return err
		}
		if fact.FeedbackAt.After(fact.OccurredAt) {
			return fmt.Errorf("feedback_at must not follow occurred_at")
		}
	}
	if fact.Outcome == OutcomeNoOutcome {
		if fact.OutcomeRef != nil || fact.OutcomeAt != nil {
			return fmt.Errorf("no_outcome must not claim an outcome source or time")
		}
	} else if fact.Outcome == OutcomeUnknown {
		if fact.OutcomeAt != nil {
			return fmt.Errorf("unknown outcome must not claim an event time")
		}
	} else if fact.OutcomeRef == nil || fact.OutcomeAt == nil {
		return fmt.Errorf("%s outcome requires outcome_ref and outcome_at", fact.Outcome)
	}
	if fact.OutcomeRef != nil {
		if err := fact.OutcomeRef.Validate(); err != nil {
			return fmt.Errorf("outcome_ref: %w", err)
		}
		if fact.OutcomeRef.Kind != SourceOutcome {
			return fmt.Errorf("outcome_ref must have kind %q", SourceOutcome)
		}
	}
	if fact.OutcomeAt != nil {
		if err := validateUTC("outcome_at", *fact.OutcomeAt); err != nil {
			return err
		}
		if fact.OutcomeAt.After(fact.OccurredAt) {
			return fmt.Errorf("outcome_at must not follow occurred_at")
		}
	}
	if fact.ResultCompleteness == CompletenessComplete &&
		(fact.Feedback == FeedbackUnknown || fact.Outcome == OutcomeUnknown) {
		return fmt.Errorf("complete feedback/outcome fact cannot contain unknown state")
	}
	return validateUTC("occurred_at", fact.OccurredAt)
}

func (value FixedPoint) Validate() error {
	if value.Scale > 9 {
		return fmt.Errorf("fixed-point scale %d exceeds 9", value.Scale)
	}
	return validateIdentifier("fixed-point unit", value.Unit)
}

func (fact ExperimentFact) Validate() error {
	if fact.SchemaVersion != ExperimentFactSchemaVersion {
		return fmt.Errorf("unsupported experiment fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id":             fact.FactID,
		"experiment_id":       fact.ExperimentID,
		"experiment_revision": fact.ExperimentRevision,
		"variant_id":          fact.VariantID,
		"metric_id":           fact.MetricID,
		"metric_version":      fact.MetricVersion,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := validateDefinition(fact.MetricDefinition); err != nil {
		return fmt.Errorf("metric_definition: %w", err)
	}
	switch fact.Arm {
	case ExperimentBaseline, ExperimentVariant:
	default:
		return fmt.Errorf("unsupported experiment arm %q", fact.Arm)
	}
	switch fact.Direction {
	case HigherIsBetter, LowerIsBetter:
	default:
		return fmt.Errorf("unsupported metric direction %q", fact.Direction)
	}
	if err := fact.Value.Validate(); err != nil {
		return fmt.Errorf("value: %w", err)
	}
	if fact.SampleSize == 0 {
		return fmt.Errorf("experiment sample_size must be positive")
	}
	if err := fact.Window.Validate(); err != nil {
		return fmt.Errorf("experiment window: %w", err)
	}
	if err := validateCompleteness(
		"experiment", fact.ResultCompleteness, fact.IncompleteReasons,
	); err != nil {
		return err
	}
	if err := fact.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	if err := validateSourceRefs(fact.SourceRefs, true); err != nil {
		return fmt.Errorf("source_refs: %w", err)
	}
	return validateUTC("occurred_at", fact.OccurredAt)
}

func (fact RepeatabilityFact) Validate() error {
	if fact.SchemaVersion != RepeatabilityFactSchemaVersion {
		return fmt.Errorf("unsupported repeatability fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id": fact.FactID, "repeatability_run_id": fact.RepeatabilityRunID,
		"repeatability_revision": fact.RepeatabilityRevision, "case_id": fact.CaseID,
		"metric_id": fact.MetricID, "metric_version": fact.MetricVersion,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := validateDefinition(fact.MetricDefinition); err != nil {
		return fmt.Errorf("metric_definition: %w", err)
	}
	switch fact.Direction {
	case HigherIsBetter, LowerIsBetter:
	default:
		return fmt.Errorf("unsupported metric direction %q", fact.Direction)
	}
	if err := fact.Value.Validate(); err != nil {
		return fmt.Errorf("value: %w", err)
	}
	if fact.SampleSize < 2 {
		return fmt.Errorf("repeatability sample_size must be at least two")
	}
	if err := fact.Window.Validate(); err != nil {
		return fmt.Errorf("repeatability window: %w", err)
	}
	if err := validateCompleteness("repeatability", fact.ResultCompleteness, fact.IncompleteReasons); err != nil {
		return err
	}
	if err := fact.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	if err := validateSourceRefs(fact.SourceRefs, true); err != nil {
		return fmt.Errorf("source_refs: %w", err)
	}
	if !containsSourceKind(fact.SourceRefs, SourceRepeatability) ||
		!containsSourceKind(fact.SourceRefs, SourceEvaluation) {
		return fmt.Errorf("repeatability source_refs must include repeatability and evaluation evidence")
	}
	return validateUTC("occurred_at", fact.OccurredAt)
}

func (baseline Baseline) Validate() error {
	if err := validateIdentifier("baseline.cohort_id", baseline.CohortID); err != nil {
		return err
	}
	if err := validateIdentifier("baseline.revision", baseline.Revision); err != nil {
		return err
	}
	if err := baseline.Window.Validate(); err != nil {
		return fmt.Errorf("baseline.window: %w", err)
	}
	if err := baseline.Value.Validate(); err != nil {
		return fmt.Errorf("baseline.value: %w", err)
	}
	if baseline.SampleSize == 0 {
		return fmt.Errorf("baseline.sample_size must be positive")
	}
	if err := validateSourceRefs(baseline.SourceRefs, true); err != nil {
		return fmt.Errorf("baseline.source_refs: %w", err)
	}
	if !containsSourceKind(baseline.SourceRefs, SourceBaseline) {
		return fmt.Errorf("baseline.source_refs must include immutable baseline evidence")
	}
	return nil
}

func (uncertainty Uncertainty) Validate(point FixedPoint) error {
	if err := uncertainty.Lower.Validate(); err != nil {
		return fmt.Errorf("lower: %w", err)
	}
	if err := uncertainty.Upper.Validate(); err != nil {
		return fmt.Errorf("upper: %w", err)
	}
	if !fixedCompatible(uncertainty.Lower, point) ||
		!fixedCompatible(uncertainty.Upper, point) {
		return fmt.Errorf("uncertainty bounds must use the net value unit and scale")
	}
	if uncertainty.Lower.Amount > point.Amount ||
		point.Amount > uncertainty.Upper.Amount {
		return fmt.Errorf("uncertainty bounds must contain net value")
	}
	if uncertainty.ConfidenceBPS == 0 || uncertainty.ConfidenceBPS > 10_000 {
		return fmt.Errorf("confidence_bps must be within 1..10000")
	}
	if err := validateIdentifier("uncertainty.method", uncertainty.Method); err != nil {
		return err
	}
	return validateIdentifier("uncertainty.revision", uncertainty.Revision)
}

func (costs CostBreakdown) values() []FixedPoint {
	return []FixedPoint{
		costs.ModelCompute,
		costs.HumanVerification,
		costs.FalsePositive,
		costs.PlatformOperations,
		costs.Storage,
	}
}

func (observation ValueObservation) Validate() error {
	if observation.SchemaVersion != ValueObservationSchemaVersion {
		return fmt.Errorf(
			"unsupported value observation schema %q",
			observation.SchemaVersion,
		)
	}
	if err := validateIdentifier("observation_id", observation.ObservationID); err != nil {
		return err
	}
	if err := observation.Dimensions.Validate(); err != nil {
		return fmt.Errorf("dimensions: %w", err)
	}
	if err := validateIdentifier("metric_id", observation.MetricID); err != nil {
		return err
	}
	if _, ok := evidenceTierRank(observation.EvidenceTier); !ok {
		return fmt.Errorf("unsupported evidence tier %q", observation.EvidenceTier)
	}
	if err := observation.Baseline.Validate(); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	if err := observation.ObservationWindow.Validate(); err != nil {
		return fmt.Errorf("observation_window: %w", err)
	}
	if err := observation.AttributionWindow.Validate(); err != nil {
		return fmt.Errorf("attribution_window: %w", err)
	}
	if observation.SampleSize == 0 {
		return fmt.Errorf("sample_size must be positive")
	}
	if err := observation.GrossValue.Validate(); err != nil {
		return fmt.Errorf("gross_value: %w", err)
	}
	if !fixedCompatible(observation.Baseline.Value, observation.GrossValue) {
		return fmt.Errorf("baseline and gross value must use the same unit and scale")
	}
	var totalCost int64
	for index, cost := range observation.Costs.values() {
		if err := cost.Validate(); err != nil {
			return fmt.Errorf("cost[%d]: %w", index, err)
		}
		if !fixedCompatible(cost, observation.GrossValue) {
			return fmt.Errorf("all costs must use the gross value unit and scale")
		}
		if cost.Amount < 0 {
			return fmt.Errorf("cost[%d] must not be negative", index)
		}
		next, err := checkedAdd(totalCost, cost.Amount)
		if err != nil {
			return fmt.Errorf("sum costs: %w", err)
		}
		totalCost = next
	}
	if err := observation.NetValue.Validate(); err != nil {
		return fmt.Errorf("net_value: %w", err)
	}
	if !fixedCompatible(observation.NetValue, observation.GrossValue) {
		return fmt.Errorf("net and gross value must use the same unit and scale")
	}
	expectedNet, err := checkedSubtract(observation.GrossValue.Amount, totalCost)
	if err != nil {
		return fmt.Errorf("calculate net value: %w", err)
	}
	if observation.NetValue.Amount != expectedNet {
		return fmt.Errorf(
			"net value %d does not equal gross value %d minus all costs %d",
			observation.NetValue.Amount,
			observation.GrossValue.Amount,
			totalCost,
		)
	}
	if err := observation.NetValueUncertainty.Validate(observation.NetValue); err != nil {
		return fmt.Errorf("net_value_uncertainty: %w", err)
	}
	if err := validateSourceRefs(observation.SourceRefs, true); err != nil {
		return fmt.Errorf("source_refs: %w", err)
	}
	requiredKind := map[EvidenceTier]SourceKind{
		EvidenceTierE0: SourceModelEstimate,
		EvidenceTierE1: SourceFeedback,
		EvidenceTierE2: SourceOutcome,
	}[observation.EvidenceTier]
	if observation.EvidenceTier == EvidenceTierE3 {
		if !containsSourceKind(observation.SourceRefs, SourceExperiment) &&
			!containsSourceKind(observation.SourceRefs, SourceEvaluation) {
			return fmt.Errorf("E3 value requires experiment or evaluation evidence")
		}
	} else if !containsSourceKind(observation.SourceRefs, requiredKind) {
		return fmt.Errorf(
			"%s value requires %s evidence",
			observation.EvidenceTier,
			requiredKind,
		)
	}
	if err := validateIdentifier(
		"attribution_policy_revision",
		observation.AttributionPolicyRevision,
	); err != nil {
		return err
	}
	if err := validateUTC("observed_at", observation.ObservedAt); err != nil {
		return err
	}
	if observation.ObservedAt.Before(observation.ObservationWindow.EndExclusive) {
		return fmt.Errorf("observed_at precedes completion of observation_window")
	}
	return nil
}

func (facts FactSet) Validate() error {
	if facts.SchemaVersion != FactSetSchemaVersion {
		return fmt.Errorf("unsupported fact set schema %q", facts.SchemaVersion)
	}
	if err := facts.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if err := validateCompleteness(
		"fact_set", facts.Completeness, facts.IncompleteReasons,
	); err != nil {
		return err
	}
	if facts.ReviewRuns == nil || facts.Stages == nil || facts.ContextProviders == nil || facts.Findings == nil ||
		facts.FeedbackOutcomes == nil || facts.FindingLineages == nil || facts.Experiments == nil || facts.Repeatability == nil ||
		facts.ValueObservations == nil {
		return fmt.Errorf("all fact collections must be explicit arrays")
	}

	factIDs := make(map[string]string)
	runs := make(map[string]ReviewRunFact, len(facts.ReviewRuns))
	for index, fact := range facts.ReviewRuns {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("review_runs[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "review_run"); err != nil {
			return err
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("review_runs[%d] occurred outside fact set window", index)
		}
		if _, exists := runs[fact.RunID]; exists {
			return fmt.Errorf("duplicate review run %q", fact.RunID)
		}
		runs[fact.RunID] = fact
	}

	stageKeys := make(map[string]struct{}, len(facts.Stages))
	for index, fact := range facts.Stages {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("stages[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "stage"); err != nil {
			return err
		}
		run, exists := runs[fact.RunID]
		if !exists {
			return fmt.Errorf("stages[%d] references unknown run %q", index, fact.RunID)
		}
		if !sameRunDimensions(run.Dimensions, fact.Dimensions) {
			return fmt.Errorf("stages[%d] dimensions do not match its run", index)
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("stages[%d] occurred outside fact set window", index)
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", fact.RunID, fact.StageID, fact.Attempt)
		if _, exists := stageKeys[key]; exists {
			return fmt.Errorf("duplicate stage attempt %q", key)
		}
		stageKeys[key] = struct{}{}
	}

	providerBindings := make(map[string]struct{}, len(facts.ContextProviders))
	for index, fact := range facts.ContextProviders {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("context_providers[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "context_provider"); err != nil {
			return err
		}
		run, exists := runs[fact.RunID]
		if !exists {
			return fmt.Errorf("context_providers[%d] references unknown run %q", index, fact.RunID)
		}
		if !sameRunDimensions(run.Dimensions, fact.Dimensions) {
			return fmt.Errorf("context_providers[%d] dimensions do not match its run", index)
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("context_providers[%d] occurred outside fact set window", index)
		}
		key := runEntityKey(fact.RunID, fact.ReceiptID)
		if _, exists := providerBindings[key]; exists {
			return fmt.Errorf("duplicate context provider receipt %q in run %q", fact.ReceiptID, fact.RunID)
		}
		providerBindings[key] = struct{}{}
	}

	funnelByRun := make(map[string]FunnelSummary)
	findings := make(map[string]FindingFunnelFact)
	candidates := make(map[string]struct{})
	for index, fact := range facts.Findings {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("finding_funnel[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "finding_funnel"); err != nil {
			return err
		}
		run, exists := runs[fact.RunID]
		if !exists {
			return fmt.Errorf(
				"finding_funnel[%d] references unknown run %q",
				index,
				fact.RunID,
			)
		}
		if !sameRunDimensions(run.Dimensions, fact.Dimensions) {
			return fmt.Errorf("finding_funnel[%d] dimensions do not match its run", index)
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("finding_funnel[%d] occurred outside fact set window", index)
		}
		if fact.PublicationAt != nil &&
			!facts.Window.contains(*fact.PublicationAt) {
			return fmt.Errorf(
				"finding_funnel[%d] publication occurred outside fact set window",
				index,
			)
		}
		candidateKey := runEntityKey(fact.RunID, fact.CandidateID)
		if _, exists := candidates[candidateKey]; exists {
			return fmt.Errorf(
				"duplicate candidate %q in run %q",
				fact.CandidateID,
				fact.RunID,
			)
		}
		candidates[candidateKey] = struct{}{}
		summary := funnelByRun[fact.RunID]
		summary.Candidates++
		if fact.Normalized {
			findingKey := runEntityKey(fact.RunID, fact.FindingID)
			if existing, exists := findings[findingKey]; exists {
				if existing.Verification != fact.Verification ||
					existing.PublicationEligibility != fact.PublicationEligibility ||
					existing.Publication != fact.Publication ||
					!sourceRefPointersEqual(
						existing.PublicationRef,
						fact.PublicationRef,
					) ||
					!timePointersEqual(existing.PublicationAt, fact.PublicationAt) ||
					existing.ResultCompleteness != fact.ResultCompleteness ||
					!slices.Equal(existing.IncompleteReasons, fact.IncompleteReasons) ||
					existing.Dimensions != fact.Dimensions {
					return fmt.Errorf(
						"candidate facts disagree on normalized finding %q in run %q",
						fact.FindingID,
						fact.RunID,
					)
				}
				funnelByRun[fact.RunID] = summary
				continue
			}
			findings[findingKey] = fact
			summary.Normalized++
			if fact.Verification == VerificationVerified {
				summary.Verified++
			}
			if fact.Publication == PublicationPublished {
				summary.Published++
			}
		}
		funnelByRun[fact.RunID] = summary
	}

	for runID, run := range runs {
		observed := funnelByRun[runID]
		if run.Funnel == nil {
			if observed != (FunnelSummary{}) {
				return fmt.Errorf("run %q omits a funnel summary but has funnel facts", runID)
			}
			continue
		}
		if facts.Completeness == CompletenessComplete {
			if observed != *run.Funnel {
				return fmt.Errorf(
					"run %q funnel does not reconcile: fact=%+v run=%+v",
					runID,
					observed,
					*run.Funnel,
				)
			}
		} else if exceedsSummary(observed, *run.Funnel) {
			return fmt.Errorf("run %q observed funnel exceeds authoritative summary", runID)
		}
	}

	feedbackByFinding := make(map[string]FeedbackOutcomeFact)
	for index, fact := range facts.FeedbackOutcomes {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("feedback_outcomes[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "feedback_outcome"); err != nil {
			return err
		}
		findingKey := runEntityKey(fact.RunID, fact.FindingID)
		finding, exists := findings[findingKey]
		if !exists {
			return fmt.Errorf(
				"feedback_outcomes[%d] references unknown finding %q in run %q",
				index,
				fact.FindingID,
				fact.RunID,
			)
		}
		published := finding.Publication == PublicationPublished
		platformHumanQueue := finding.Publication == PublicationNotReached &&
			finding.PublicationEligibility == EligibilityHumanQueue
		if !published && !platformHumanQueue {
			return fmt.Errorf(
				"feedback_outcomes[%d] references a finding outside published or platform human-queue observation",
				index,
			)
		}
		if platformHumanQueue && fact.Feedback == FeedbackNoFeedback &&
			fact.Outcome == OutcomeNoOutcome {
			return fmt.Errorf(
				"feedback_outcomes[%d] invents an unobserved human-queue response",
				index,
			)
		}
		if fact.Dimensions != finding.Dimensions {
			return fmt.Errorf("feedback_outcomes[%d] dimensions do not match finding", index)
		}
		if published && fact.FeedbackAt != nil &&
			fact.FeedbackAt.Before(*finding.PublicationAt) {
			return fmt.Errorf(
				"feedback_outcomes[%d] feedback precedes publication",
				index,
			)
		}
		if published && fact.OutcomeAt != nil &&
			fact.OutcomeAt.Before(*finding.PublicationAt) {
			return fmt.Errorf(
				"feedback_outcomes[%d] outcome precedes publication",
				index,
			)
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("feedback_outcomes[%d] occurred outside fact set window", index)
		}
		if _, exists := feedbackByFinding[findingKey]; exists {
			return fmt.Errorf(
				"duplicate feedback/outcome projection for %q in run %q",
				fact.FindingID,
				fact.RunID,
			)
		}
		feedbackByFinding[findingKey] = fact
	}

	lineageRelations := make(map[string]struct{}, len(facts.FindingLineages))
	for index, fact := range facts.FindingLineages {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("finding_lineages[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "finding_lineage"); err != nil {
			return err
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("finding_lineages[%d] occurred outside fact set window", index)
		}
		key := runEntityKey(fact.LineageID, fact.RelationID)
		if _, exists := lineageRelations[key]; exists {
			return fmt.Errorf("duplicate lineage relation %q in %q", fact.RelationID, fact.LineageID)
		}
		lineageRelations[key] = struct{}{}
	}

	for index, fact := range facts.Experiments {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("experiments[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "experiment"); err != nil {
			return err
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("experiments[%d] occurred outside fact set window", index)
		}
	}
	if err := validateExperimentReconciliation(facts.Experiments, facts.Completeness); err != nil {
		return err
	}
	repeatabilityKeys := make(map[string]struct{}, len(facts.Repeatability))
	for index, fact := range facts.Repeatability {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("repeatability[%d]: %w", index, err)
		}
		if err := registerFactID(factIDs, fact.FactID, "repeatability"); err != nil {
			return err
		}
		if !facts.Window.contains(fact.OccurredAt) {
			return fmt.Errorf("repeatability[%d] occurred outside fact set window", index)
		}
		key := strings.Join([]string{fact.RepeatabilityRunID, fact.CaseID, fact.Dimensions.ReviewDimension, fact.MetricID}, "\x00")
		if _, exists := repeatabilityKeys[key]; exists {
			return fmt.Errorf("duplicate repeatability metric %q", key)
		}
		repeatabilityKeys[key] = struct{}{}
	}

	observations := make(map[string]struct{}, len(facts.ValueObservations))
	for index, observation := range facts.ValueObservations {
		if err := observation.Validate(); err != nil {
			return fmt.Errorf("value_observations[%d]: %w", index, err)
		}
		if err := registerFactID(
			factIDs,
			observation.ObservationID,
			"value_observation",
		); err != nil {
			return err
		}
		if _, exists := observations[observation.ObservationID]; exists {
			return fmt.Errorf("duplicate value observation %q", observation.ObservationID)
		}
		observations[observation.ObservationID] = struct{}{}
		if !facts.Window.contains(observation.ObservedAt) {
			return fmt.Errorf(
				"value_observations[%d] observed outside fact set window",
				index,
			)
		}
	}
	return nil
}

func (fact FindingLineageFact) Validate() error {
	if fact.SchemaVersion != FindingLineageFactSchemaVersion {
		return fmt.Errorf("unsupported finding lineage fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id": fact.FactID, "lineage_id": fact.LineageID, "relation_id": fact.RelationID,
		"reason_code": fact.ReasonCode, "policy_id": fact.PolicyID, "policy_revision": fact.PolicyRevision,
		"baseline_run_id": fact.BaselineRunID, "variant_run_id": fact.VariantRunID,
		"tenant_id": fact.TenantID, "organization_id": fact.OrganizationID,
		"workspace_id": fact.WorkspaceID, "repository_id": fact.RepositoryID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if fact.BaselineRunID == fact.VariantRunID {
		return fmt.Errorf("lineage fact requires distinct source runs")
	}
	for name, value := range map[string]string{
		"lineage_artifact_sha256": fact.LineageArtifactSHA256,
		"family_key":              fact.FamilyKey, "policy_sha256": fact.PolicySHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	switch fact.AncestryAuthority {
	case "caller_order_unverified":
		if fact.AncestryEvidenceSHA256 != "" || fact.RenameMappingCount != 0 || fact.RelationMethod == "git_rename_family" {
			return fmt.Errorf("caller-order lineage fact cannot claim Git evidence")
		}
	case "local_git_object_graph":
		if err := validateSHA256("ancestry_evidence_sha256", fact.AncestryEvidenceSHA256); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported ancestry authority %q", fact.AncestryAuthority)
	}
	if fact.BaselineFindingIDs == nil || fact.VariantFindingIDs == nil ||
		!slices.IsSorted(fact.BaselineFindingIDs) || !slices.IsSorted(fact.VariantFindingIDs) ||
		hasAdjacentDuplicate(fact.BaselineFindingIDs) || hasAdjacentDuplicate(fact.VariantFindingIDs) {
		return fmt.Errorf("lineage finding IDs must be explicit sorted unique arrays")
	}
	b, v := len(fact.BaselineFindingIDs), len(fact.VariantFindingIDs)
	switch fact.RelationType {
	case "continued":
		if b != 1 || v != 1 {
			return fmt.Errorf("continued lineage fact must be one-to-one")
		}
		if fact.RelationMethod != "exact_fingerprint" && fact.RelationMethod != "stable_family" && fact.RelationMethod != "git_rename_family" {
			return fmt.Errorf("continued lineage fact requires conservative match evidence")
		}
	case "split":
		if b != 1 || v < 2 {
			return fmt.Errorf("split lineage fact must be one-to-many")
		}
		if fact.RelationMethod != "stable_family" && fact.RelationMethod != "git_rename_family" {
			return fmt.Errorf("split lineage fact requires family match evidence")
		}
	case "merged":
		if b < 2 || v != 1 {
			return fmt.Errorf("merged lineage fact must be many-to-one")
		}
		if fact.RelationMethod != "stable_family" && fact.RelationMethod != "git_rename_family" {
			return fmt.Errorf("merged lineage fact requires family match evidence")
		}
	case "introduced":
		if b != 0 || v != 1 {
			return fmt.Errorf("introduced lineage fact must contain one variant finding")
		}
		if fact.RelationMethod != "unmatched" {
			return fmt.Errorf("introduced lineage fact must be unmatched")
		}
	case "resolved":
		if b != 1 || v != 0 {
			return fmt.Errorf("resolved lineage fact must contain one baseline finding")
		}
		if fact.RelationMethod != "unmatched" {
			return fmt.Errorf("resolved lineage fact must be unmatched")
		}
	default:
		return fmt.Errorf("unsupported lineage relation type %q", fact.RelationType)
	}
	switch fact.RelationMethod {
	case "exact_fingerprint", "stable_family", "git_rename_family", "unmatched":
	default:
		return fmt.Errorf("unsupported lineage relation method %q", fact.RelationMethod)
	}
	if err := validateUTC("occurred_at", fact.OccurredAt); err != nil {
		return err
	}
	return nil
}

func hasAdjacentDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}

func validateExperimentReconciliation(
	facts []ExperimentFact,
	completeness Completeness,
) error {
	type group struct {
		baseline *ExperimentFact
		variants map[string]struct{}
	}
	groups := make(map[string]*group)
	for index := range facts {
		fact := &facts[index]
		key := strings.Join(
			[]string{
				fact.ExperimentID,
				fact.ExperimentRevision,
				fact.MetricID,
				dimensionsStableKey(fact.Dimensions),
			},
			"\x00",
		)
		current := groups[key]
		if current == nil {
			current = &group{variants: make(map[string]struct{})}
			groups[key] = current
		}
		if fact.Arm == ExperimentBaseline {
			if current.baseline != nil {
				return fmt.Errorf("experiment metric %q has multiple baselines", key)
			}
			current.baseline = fact
			continue
		}
		if _, exists := current.variants[fact.VariantID]; exists {
			return fmt.Errorf(
				"experiment metric %q has duplicate variant %q",
				key,
				fact.VariantID,
			)
		}
		current.variants[fact.VariantID] = struct{}{}
	}
	for key, current := range groups {
		if current.baseline == nil {
			if completeness == CompletenessComplete {
				return fmt.Errorf("experiment metric %q has no baseline", key)
			}
			continue
		}
		if len(current.variants) == 0 && completeness == CompletenessComplete {
			return fmt.Errorf("experiment metric %q has no variant", key)
		}
		for index := range facts {
			fact := facts[index]
			factKey := strings.Join(
				[]string{
					fact.ExperimentID,
					fact.ExperimentRevision,
					fact.MetricID,
					dimensionsStableKey(fact.Dimensions),
				},
				"\x00",
			)
			if factKey != key || fact.Arm != ExperimentVariant {
				continue
			}
			baseline := current.baseline
			if fact.VariantID == baseline.VariantID {
				return fmt.Errorf(
					"experiment metric %q reuses baseline variant_id %q",
					key,
					fact.VariantID,
				)
			}
			if fact.MetricVersion != baseline.MetricVersion ||
				fact.MetricDefinition != baseline.MetricDefinition ||
				fact.Direction != baseline.Direction ||
				!fixedCompatible(fact.Value, baseline.Value) ||
				fact.Window != baseline.Window ||
				fact.Dimensions != baseline.Dimensions {
				return fmt.Errorf(
					"experiment variant %q is not comparable with its baseline",
					fact.VariantID,
				)
			}
		}
	}
	return nil
}

func (policy ROIPolicy) Validate() error {
	if policy.SchemaVersion != ROIPolicySchemaVersion {
		return fmt.Errorf("unsupported ROI policy schema %q", policy.SchemaVersion)
	}
	for name, value := range map[string]string{
		"policy_id":                            policy.PolicyID,
		"revision":                             policy.Revision,
		"required_attribution_policy_revision": policy.RequiredAttributionPolicyRevision,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if policy.MinimumEvidenceTier != EvidenceTierE2 &&
		policy.MinimumEvidenceTier != EvidenceTierE3 {
		return fmt.Errorf("ROI policy minimum evidence tier must be E2 or E3")
	}
	if policy.MinimumSampleSize == 0 {
		return fmt.Errorf("minimum_sample_size must be positive")
	}
	if err := policy.MinimumNetValue.Validate(); err != nil {
		return fmt.Errorf("minimum_net_value: %w", err)
	}
	if policy.MinimumConfidenceBPS == 0 ||
		policy.MinimumConfidenceBPS > 10_000 {
		return fmt.Errorf("minimum_confidence_bps must be within 1..10000")
	}
	return nil
}

func (projection DashboardProjection) Validate() error {
	if projection.SchemaVersion != DashboardProjectionSchemaVersion {
		return fmt.Errorf(
			"unsupported dashboard projection schema %q",
			projection.SchemaVersion,
		)
	}
	if err := projection.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if projection.GroupBy == nil || projection.Tiles == nil {
		return fmt.Errorf("group_by and tiles must be explicit arrays")
	}
	if err := validateGroupBy(projection.GroupBy); err != nil {
		return err
	}
	previous := ""
	for index, tile := range projection.Tiles {
		if err := tile.Validate(); err != nil {
			return fmt.Errorf("tiles[%d]: %w", index, err)
		}
		key := tileSortKey(tile)
		if index > 0 && key <= previous {
			return fmt.Errorf("tiles must be uniquely sorted")
		}
		previous = key
	}
	return nil
}

func (tile DashboardTile) Validate() error {
	if err := validateSHA256("tile_id", tile.TileID); err != nil {
		return err
	}
	if err := validateIdentifier("metric_id", tile.MetricID); err != nil {
		return err
	}
	if err := validateIdentifier("metric_version", tile.MetricVersion); err != nil {
		return err
	}
	if err := validateDefinition(tile.Definition); err != nil {
		return err
	}
	if err := tile.Window.Validate(); err != nil {
		return err
	}
	if tile.Dimensions == nil || tile.Warnings == nil {
		return fmt.Errorf("dimensions and warnings must be explicit arrays")
	}
	seenDimensions := make(map[DimensionName]struct{})
	for _, dimension := range tile.Dimensions {
		if err := validateDimensionName(dimension.Name, true); err != nil {
			return err
		}
		if err := validateDimensionValue(dimension.Value); err != nil {
			return err
		}
		if _, exists := seenDimensions[dimension.Name]; exists {
			return fmt.Errorf("duplicate tile dimension %q", dimension.Name)
		}
		seenDimensions[dimension.Name] = struct{}{}
	}
	if err := validateSortedCodes("warnings", tile.Warnings, true); err != nil {
		return err
	}
	switch tile.Availability {
	case TileObserved:
		if tile.Value == nil {
			return fmt.Errorf("observed tile requires value")
		}
		if err := tile.Value.Validate(); err != nil {
			return fmt.Errorf("value: %w", err)
		}
		if tile.Qualifier != ValueExact && tile.Qualifier != ValueLowerBound &&
			tile.Qualifier != ValueSampleOnly {
			return fmt.Errorf("observed tile has unsupported qualifier %q", tile.Qualifier)
		}
	case TileUnknown:
		if tile.Value != nil {
			return fmt.Errorf("unknown tile must not contain a value")
		}
		if tile.Qualifier != ValueUnavailable {
			return fmt.Errorf("unknown tile must use unavailable qualifier")
		}
	default:
		return fmt.Errorf("unsupported tile availability %q", tile.Availability)
	}
	return nil
}

func (manifest ExportManifest) Validate() error {
	if manifest.SchemaVersion != ExportManifestSchemaVersion {
		return fmt.Errorf("unsupported export manifest schema %q", manifest.SchemaVersion)
	}
	if manifest.DatasetSchemaVersion == "" {
		return fmt.Errorf("dataset_schema_version is required")
	}
	switch manifest.Format {
	case CanonicalJSONExportFormat, CanonicalCSVExportFormat,
		CanonicalParquetExportFormat:
	default:
		return fmt.Errorf("unsupported export format %q", manifest.Format)
	}
	if err := manifest.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if manifest.Parquet != (manifest.Format == CanonicalParquetExportFormat) {
		return fmt.Errorf("parquet capability must match export format")
	}
	if manifest.Parquet {
		if manifest.DatasetSchemaVersion != FactSetSchemaVersion {
			return fmt.Errorf("Parquet export requires the FactSet dataset contract")
		}
		if manifest.Limitations == nil || len(manifest.Limitations) != 0 {
			return fmt.Errorf("Parquet export requires an explicit empty limitation array")
		}
	} else if !slices.Equal(
		manifest.Limitations,
		[]string{parquetNotEmittedLimitation},
	) {
		return fmt.Errorf("non-Parquet limitation must be explicit")
	}
	if manifest.Files == nil || len(manifest.Files) == 0 {
		return fmt.Errorf("files must be a non-empty explicit array")
	}
	previous := ""
	var rows uint64
	for index, file := range manifest.Files {
		if err := validateExportFileManifest(file); err != nil {
			return fmt.Errorf("files[%d]: %w", index, err)
		}
		if manifest.Parquet {
			expectedContracts := map[string]string{
				"context_providers.parquet":  ContextProviderParquetSchemaVersion,
				"experiments.parquet":        ExperimentParquetSchemaVersion,
				"repeatability.parquet":      RepeatabilityParquetSchemaVersion,
				"feedback_outcomes.parquet":  FeedbackOutcomeParquetSchemaVersion,
				"finding_funnel.parquet":     FindingFunnelParquetSchemaVersion,
				"finding_lineages.parquet":   FindingLineageParquetSchemaVersion,
				"review_runs.parquet":        ReviewRunParquetSchemaVersion,
				"stages.parquet":             StageParquetSchemaVersion,
				"value_observations.parquet": ValueObservationParquetSchemaVersion,
			}
			expectedContract, ok := expectedContracts[file.Name]
			if !ok || file.Contract != expectedContract || file.Ref != file.Name ||
				file.MediaType != "application/vnd.apache.parquet" {
				return fmt.Errorf(
					"Parquet file %q does not match its table contract/ref",
					file.Name,
				)
			}
		} else if file.Contract != "" || file.Ref != "" {
			return fmt.Errorf("non-Parquet files must not claim table contract/ref")
		}
		if index > 0 && file.Name <= previous {
			return fmt.Errorf("export files must be uniquely sorted by name")
		}
		previous = file.Name
		next, overflow := addUint64(rows, file.RowCount)
		if overflow {
			return fmt.Errorf("export row count overflows uint64")
		}
		rows = next
	}
	if manifest.Format == CanonicalJSONExportFormat {
		if len(manifest.Files) != 1 {
			return fmt.Errorf("canonical JSON export must contain exactly one file")
		}
		if manifest.Files[0].RowCount != manifest.FactCount {
			return fmt.Errorf("canonical JSON row count must equal fact_count")
		}
	} else if rows != manifest.FactCount {
		return fmt.Errorf("tabular row counts do not equal fact_count")
	}
	if manifest.Parquet && len(manifest.Files) != 9 {
		return fmt.Errorf("Parquet FactSet export must contain exactly nine tables")
	}
	return nil
}

func validateExportFileManifest(file ExportFileManifest) error {
	if file.Name == "" || strings.Contains(file.Name, "/") ||
		file.Name != path.Base(file.Name) {
		return fmt.Errorf("invalid export file name %q", file.Name)
	}
	if file.MediaType == "" {
		return fmt.Errorf("media_type is required")
	}
	if file.SizeBytes < 0 {
		return fmt.Errorf("size_bytes must not be negative")
	}
	return validateSHA256("sha256", file.SHA256)
}

func validateCompleteness(
	name string,
	completeness Completeness,
	reasons []string,
) error {
	switch completeness {
	case CompletenessComplete:
		if reasons == nil || len(reasons) != 0 {
			return fmt.Errorf("%s complete state requires an explicit empty reason array", name)
		}
		return validateSortedCodes(name+".incomplete_reasons", reasons, true)
	case CompletenessPartial, CompletenessUnknown:
		if len(reasons) == 0 {
			return fmt.Errorf("%s %s state requires reasons", name, completeness)
		}
	default:
		return fmt.Errorf("%s has unsupported completeness %q", name, completeness)
	}
	return validateSortedCodes(name+".incomplete_reasons", reasons, false)
}

func validateSourceRefs(refs []SourceRef, requireNonEmpty bool) error {
	if refs == nil || requireNonEmpty && len(refs) == 0 {
		return fmt.Errorf("must be a non-empty explicit array")
	}
	previous := ""
	for index, ref := range refs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("[%d]: %w", index, err)
		}
		key := sourceRefKey(ref)
		if index > 0 && key <= previous {
			return fmt.Errorf("must be uniquely sorted")
		}
		previous = key
	}
	return nil
}

func sourceRefKey(ref SourceRef) string {
	return strings.Join(
		[]string{string(ref.Kind), ref.ID, ref.Revision, ref.SHA256},
		"\x00",
	)
}

func sourceRefPointersEqual(left, right *SourceRef) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func timePointersEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func containsSourceKind(refs []SourceRef, kind SourceKind) bool {
	return slices.ContainsFunc(refs, func(ref SourceRef) bool {
		return ref.Kind == kind
	})
}

func validateGroupBy(groupBy []DimensionName) error {
	seen := make(map[DimensionName]struct{})
	for _, name := range groupBy {
		if err := validateDimensionName(name, false); err != nil {
			return err
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("group_by contains duplicate dimension %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateDimensionName(name DimensionName, projectionOnly bool) error {
	switch name {
	case DimensionTenant, DimensionOrganization, DimensionRepository,
		DimensionLanguage, DimensionRule, DimensionPath, DimensionWorkflowRevision,
		DimensionConfigRevision, DimensionReviewDimension:
		return nil
	case DimensionExperiment, DimensionExperimentArm, DimensionVariant,
		DimensionRepeatability, DimensionEvaluationCase,
		DimensionContextProvider, DimensionContextKind, DimensionContextGapReason,
		DimensionLineageRelation, DimensionLineageMethod, DimensionLineagePolicy:
		if projectionOnly {
			return nil
		}
	}
	return fmt.Errorf("unsupported dimension %q", name)
}

func validateDimensionValue(value string) error {
	if value == UnknownDimensionValue {
		return nil
	}
	return validateReferenceID("dimension value", value)
}

func sameRunDimensions(run Dimensions, child Dimensions) bool {
	return run.TenantID == child.TenantID &&
		run.OrganizationID == child.OrganizationID &&
		run.RepositoryID == child.RepositoryID &&
		run.ReviewDimension == child.ReviewDimension &&
		run.WorkflowRevision == child.WorkflowRevision &&
		run.ConfigRevision == child.ConfigRevision
}

func runEntityKey(runID, entityID string) string {
	return runID + "\x00" + entityID
}

func exceedsSummary(observed FunnelSummary, expected FunnelSummary) bool {
	return observed.Candidates > expected.Candidates ||
		observed.Normalized > expected.Normalized ||
		observed.Verified > expected.Verified ||
		observed.Published > expected.Published
}

func registerFactID(ids map[string]string, id, kind string) error {
	if previous, exists := ids[id]; exists {
		return fmt.Errorf("fact id %q is reused by %s and %s", id, previous, kind)
	}
	ids[id] = kind
	return nil
}

func validateIdentifier(name, value string) error {
	if value == "" || len(value) > 255 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be a non-empty identifier", name)
	}
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) ||
			strings.ContainsRune("._:@+-", char) {
			continue
		}
		return fmt.Errorf("%s contains unsupported character %q", name, char)
	}
	return nil
}

func validateReferenceID(name, value string) error {
	if value == "" || len(value) > 2048 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be a non-empty reference", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func validateDefinition(value string) error {
	if value == "" || len(value) > 1024 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("must be non-empty UTF-8 text of at most 1024 bytes")
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\t' {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

func validateRepositoryPath(name, value string) error {
	if err := validateReferenceID(name, value); err != nil {
		return err
	}
	if strings.Contains(value, "\\") || strings.HasPrefix(value, "/") ||
		path.Clean(value) != value || value == "." ||
		strings.HasPrefix(value, "../") {
		return fmt.Errorf("%s must be a clean repository-relative path", name)
	}
	return nil
}

func validateUTC(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	_, offset := value.Zone()
	if offset != 0 {
		return fmt.Errorf("%s must use UTC", name)
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != 64 || value != strings.ToLower(value) {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	return nil
}

func validateSortedCodes(name string, values []string, allowEmpty bool) error {
	if values == nil {
		return fmt.Errorf("%s must be an explicit array", name)
	}
	if !allowEmpty && len(values) == 0 {
		return fmt.Errorf("%s must not be empty", name)
	}
	previous := ""
	for index, value := range values {
		if err := validateIdentifier(fmt.Sprintf("%s[%d]", name, index), value); err != nil {
			return err
		}
		if index > 0 && value <= previous {
			return fmt.Errorf("%s must be uniquely sorted", name)
		}
		previous = value
	}
	return nil
}

func fixedCompatible(left, right FixedPoint) bool {
	return left.Scale == right.Scale && left.Unit == right.Unit
}

func evidenceTierRank(tier EvidenceTier) (int, bool) {
	switch tier {
	case EvidenceTierE0:
		return 0, true
	case EvidenceTierE1:
		return 1, true
	case EvidenceTierE2:
		return 2, true
	case EvidenceTierE3:
		return 3, true
	default:
		return 0, false
	}
}

func checkedAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right ||
		right < 0 && left < math.MinInt64-right {
		return 0, errors.New("int64 overflow")
	}
	return left + right, nil
}

func checkedSubtract(left, right int64) (int64, error) {
	if right == math.MinInt64 {
		if left >= 0 {
			return 0, errors.New("int64 overflow")
		}
		return left - right, nil
	}
	return checkedAdd(left, -right)
}

func addUint64(left, right uint64) (uint64, bool) {
	if math.MaxUint64-left < right {
		return 0, true
	}
	return left + right, false
}
