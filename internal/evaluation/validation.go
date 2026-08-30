package evaluation

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (evaluationCase EvaluationCase) Validate() error {
	if err := evaluationCase.validateGoverned(); err != nil {
		return err
	}
	return evaluationCase.validateIngressSourcePolicy()
}

func (evaluationCase EvaluationCase) validateGoverned() error {
	if evaluationCase.SchemaVersion != EvaluationCaseSchemaVersion {
		return fmt.Errorf("unsupported evaluation case schema %q", evaluationCase.SchemaVersion)
	}
	if err := validateID("case_id", evaluationCase.CaseID); err != nil {
		return err
	}
	if err := evaluationCase.Type.Validate(); err != nil {
		return err
	}
	if err := evaluationCase.Provenance.Validate(); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	if err := evaluationCase.LicenseConsent.Validate(); err != nil {
		return fmt.Errorf("license_consent: %w", err)
	}
	if err := evaluationCase.Classification.Validate(); err != nil {
		return err
	}
	if err := validateText("owner", evaluationCase.Owner, 256, false); err != nil {
		return err
	}
	if err := validateArtifactRef("input_snapshot_ref", evaluationCase.InputSnapshotRef); err != nil {
		return err
	}
	if err := evaluationCase.Label.Validate(); err != nil {
		return fmt.Errorf("label: %w", err)
	}
	if err := validateLabelForCase(evaluationCase.Type, evaluationCase.Label); err != nil {
		return err
	}
	if err := validateID("label_policy_revision", evaluationCase.LabelPolicyRevision); err != nil {
		return err
	}
	if err := evaluationCase.ReviewState.Validate(); err != nil {
		return err
	}
	if err := evaluationCase.DatasetState.Validate(); err != nil {
		return err
	}
	if err := evaluationCase.Split.Validate(); err != nil {
		return err
	}
	if err := validateID("clone_group_id", evaluationCase.CloneGroupID); err != nil {
		return err
	}
	if evaluationCase.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	if evaluationCase.Provenance.CollectedAt.After(evaluationCase.CreatedAt) {
		return fmt.Errorf("provenance.collected_at must not be after created_at")
	}
	if err := evaluationCase.validateStateAndEligibility(); err != nil {
		return err
	}
	if err := evaluationCase.validateGovernedSourcePolicy(); err != nil {
		return err
	}
	return nil
}

func (provenance SourceProvenance) Validate() error {
	if err := provenance.Kind.Validate(); err != nil {
		return err
	}
	if err := validateID("repository_id", provenance.RepositoryID); err != nil {
		return err
	}
	if err := validateID("source_id", provenance.SourceID); err != nil {
		return err
	}
	if provenance.SourceRunID != "" {
		if err := validateID("source_run_id", provenance.SourceRunID); err != nil {
			return err
		}
	}
	if provenance.FindingID != "" {
		if err := validateID("finding_id", provenance.FindingID); err != nil {
			return err
		}
	}
	if provenance.ObservedAt.IsZero() || provenance.CollectedAt.IsZero() {
		return fmt.Errorf("observed_at and collected_at are required")
	}
	if provenance.ObservedAt.After(provenance.CollectedAt) {
		return fmt.Errorf("observed_at must not be after collected_at")
	}
	if err := validateArtifactRefs("evidence_refs", provenance.EvidenceRefs, true); err != nil {
		return err
	}
	switch provenance.Kind {
	case SourceHumanConfirmedFinding, SourceRejectedFalsePositive:
		if provenance.FindingID == "" {
			return fmt.Errorf("finding_id is required for source kind %q", provenance.Kind)
		}
	case SourceProductionFeedback:
		if provenance.SourceRunID == "" {
			return fmt.Errorf("source_run_id is required for production feedback")
		}
	case SourceIncidentMissedDefect:
		if provenance.SourceRunID == "" || provenance.FindingID != "" {
			return fmt.Errorf("incident missed defect requires source_run_id and no finding_id")
		}
	}
	return nil
}

func (license LicenseConsent) Validate() error {
	if err := validateText("license_id", license.LicenseID, 512, false); err != nil {
		return err
	}
	if err := license.Consent.Validate(); err != nil {
		return err
	}
	if license.AllowedUses == nil || len(license.AllowedUses) == 0 {
		return fmt.Errorf("allowed_uses must be a non-empty array")
	}
	for index, use := range license.AllowedUses {
		if err := use.Validate(); err != nil {
			return fmt.Errorf("allowed_uses[%d]: %w", index, err)
		}
		if index > 0 && license.AllowedUses[index-1] >= use {
			return fmt.Errorf("allowed_uses must be sorted and unique")
		}
	}
	if license.Restrictions == nil {
		return fmt.Errorf("restrictions must be an array")
	}
	for index, restriction := range license.Restrictions {
		if err := validateText(
			fmt.Sprintf("restrictions[%d]", index), restriction, 1024, true,
		); err != nil {
			return err
		}
		if index > 0 && license.Restrictions[index-1] >= restriction {
			return fmt.Errorf("restrictions must be sorted and unique")
		}
	}
	return nil
}

func (label Label) Validate() error {
	if err := label.ExpectedOutcome.Validate(); err != nil {
		return err
	}
	if label.Category != "" {
		if err := validateID("category", label.Category); err != nil {
			return err
		}
	}
	switch label.Severity {
	case "", "low", "medium", "high", "critical":
	default:
		return fmt.Errorf("unsupported severity %q", label.Severity)
	}
	if err := validateArtifactRefs("anchor_refs", label.AnchorRefs, false); err != nil {
		return err
	}
	if label.Anchors == nil {
		return fmt.Errorf("anchors must be an array")
	}
	previous := LabelAnchor{}
	for index, anchor := range label.Anchors {
		if err := anchor.validate(); err != nil {
			return fmt.Errorf("anchors[%d]: %w", index, err)
		}
		if index > 0 && !labelAnchorLess(previous, anchor) {
			return fmt.Errorf("anchors must be uniquely sorted")
		}
		previous = anchor
	}
	if label.SuppressionTargets == nil {
		return fmt.Errorf("suppression_targets must be an array")
	}
	previousFingerprint := ""
	for index, target := range label.SuppressionTargets {
		if err := validateSHA256("suppression_targets.cluster_fingerprint", target.ClusterFingerprint); err != nil {
			return fmt.Errorf("suppression_targets[%d]: %w", index, err)
		}
		if index > 0 && target.ClusterFingerprint <= previousFingerprint {
			return fmt.Errorf("suppression_targets must be uniquely sorted by cluster_fingerprint")
		}
		previousFingerprint = target.ClusterFingerprint
	}
	return nil
}

func (anchor LabelAnchor) validate() error {
	if anchor.Path == "" || path.IsAbs(anchor.Path) || path.Clean(anchor.Path) != anchor.Path ||
		strings.Contains(anchor.Path, "\\") || strings.HasPrefix(anchor.Path, "../") {
		return fmt.Errorf("path must be a safe repository-relative path")
	}
	if anchor.Side != "old" && anchor.Side != "new" && anchor.Side != "file" {
		return fmt.Errorf("unsupported side %q", anchor.Side)
	}
	if anchor.StartLine == 0 || anchor.EndLine < anchor.StartLine {
		return fmt.Errorf("invalid line range")
	}
	return validateSHA256("source_digest", anchor.SourceDigest)
}

func labelAnchorLess(left, right LabelAnchor) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.Side != right.Side {
		return left.Side < right.Side
	}
	if left.StartLine != right.StartLine {
		return left.StartLine < right.StartLine
	}
	if left.EndLine != right.EndLine {
		return left.EndLine < right.EndLine
	}
	return left.SourceDigest < right.SourceDigest
}

func (correction LabelCorrection) Validate() error {
	if correction.SchemaVersion != LabelCorrectionSchemaVersion {
		return fmt.Errorf("unsupported label correction schema %q", correction.SchemaVersion)
	}
	if err := validateID("case_id", correction.CaseID); err != nil {
		return err
	}
	if correction.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected_label_revision must be positive")
	}
	if err := correction.Label.Validate(); err != nil {
		return fmt.Errorf("label: %w", err)
	}
	if err := validateID("label_policy_revision", correction.LabelPolicyRevision); err != nil {
		return err
	}
	if err := validateText("reason", correction.Reason, 4096, true); err != nil {
		return err
	}
	return validateSortedIDs(
		"affected_experiment_ids", correction.AffectedExperimentIDs, false,
	)
}

func (annotation CaseAnnotation) Validate() error {
	if annotation.SchemaVersion != CaseAnnotationSchemaVersion {
		return fmt.Errorf("unsupported case annotation schema %q", annotation.SchemaVersion)
	}
	if err := validateID("case_id", annotation.CaseID); err != nil {
		return err
	}
	if annotation.ExpectedGovernanceRevision == 0 || annotation.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected governance and label revisions must be positive")
	}
	if err := annotation.Verdict.Validate(); err != nil {
		return err
	}
	switch annotation.Verdict {
	case AnnotationApprove:
		if annotation.ProposedLabel == nil {
			return fmt.Errorf("approve annotation requires proposed_label")
		}
		if err := annotation.ProposedLabel.Validate(); err != nil {
			return fmt.Errorf("proposed_label: %w", err)
		}
		if err := validateID("label_policy_revision", annotation.LabelPolicyRevision); err != nil {
			return err
		}
	case AnnotationReject:
		if annotation.ProposedLabel != nil || annotation.LabelPolicyRevision != "" {
			return fmt.Errorf("reject annotation must not propose a label or policy revision")
		}
	}
	if err := validateText("rationale", annotation.Rationale, 4096, true); err != nil {
		return err
	}
	if err := validateArtifactRefs("evidence_refs", annotation.EvidenceRefs, true); err != nil {
		return err
	}
	if annotation.ReviewedAt.IsZero() {
		return fmt.Errorf("reviewed_at is required")
	}
	return nil
}

func (assignment CaseReviewAssignment) Validate() error {
	if assignment.SchemaVersion != CaseReviewAssignmentSchemaVersion {
		return fmt.Errorf("unsupported case review assignment schema %q", assignment.SchemaVersion)
	}
	if err := validateID("case_id", assignment.CaseID); err != nil {
		return err
	}
	if assignment.ExpectedGovernanceRevision == 0 || assignment.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected governance and label revisions must be positive")
	}
	if err := validateSortedIDs("reviewer_ids", assignment.ReviewerIDs, true); err != nil {
		return err
	}
	if len(assignment.ReviewerIDs) < 2 || len(assignment.ReviewerIDs) > 8 {
		return fmt.Errorf("reviewer_ids must contain between two and eight reviewers")
	}
	if !assignment.Blind {
		return fmt.Errorf("review assignment must enable blind review")
	}
	if err := validateText("reason", assignment.Reason, 4096, true); err != nil {
		return err
	}
	if assignment.AssignedAt.IsZero() {
		return fmt.Errorf("assigned_at is required")
	}
	return nil
}

func (adjudication CaseAdjudication) Validate() error {
	if adjudication.SchemaVersion != CaseAdjudicationSchemaVersion {
		return fmt.Errorf("unsupported case adjudication schema %q", adjudication.SchemaVersion)
	}
	if err := validateID("case_id", adjudication.CaseID); err != nil {
		return err
	}
	if adjudication.ExpectedGovernanceRevision == 0 || adjudication.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected governance and label revisions must be positive")
	}
	if err := validateSortedIDs("annotation_event_ids", adjudication.AnnotationEventIDs, true); err != nil {
		return err
	}
	if len(adjudication.AnnotationEventIDs) < 2 {
		return fmt.Errorf("annotation_event_ids must contain at least two independent reviews")
	}
	if err := adjudication.Outcome.Validate(); err != nil {
		return err
	}
	switch adjudication.Outcome {
	case AdjudicationApprove:
		if adjudication.SelectedLabel == nil {
			return fmt.Errorf("approve adjudication requires selected_label")
		}
		if err := adjudication.SelectedLabel.Validate(); err != nil {
			return fmt.Errorf("selected_label: %w", err)
		}
		if err := validateID("label_policy_revision", adjudication.LabelPolicyRevision); err != nil {
			return err
		}
	case AdjudicationReject:
		if adjudication.SelectedLabel != nil || adjudication.LabelPolicyRevision != "" {
			return fmt.Errorf("reject adjudication must not select a label or policy revision")
		}
	}
	if err := validateText("rationale", adjudication.Rationale, 4096, true); err != nil {
		return err
	}
	if err := validateArtifactRefs("evidence_refs", adjudication.EvidenceRefs, true); err != nil {
		return err
	}
	if adjudication.AdjudicatedAt.IsZero() {
		return fmt.Errorf("adjudicated_at is required")
	}
	return nil
}

func (activation CaseActivation) Validate() error {
	if activation.SchemaVersion != CaseActivationSchemaVersion {
		return fmt.Errorf("unsupported case activation schema %q", activation.SchemaVersion)
	}
	if err := validateID("case_id", activation.CaseID); err != nil {
		return err
	}
	if activation.ExpectedGovernanceRevision == 0 {
		return fmt.Errorf("expected_governance_revision must be positive")
	}
	if err := activation.Split.Validate(); err != nil {
		return err
	}
	if activation.Split == SplitUnassigned {
		return fmt.Errorf("activation requires an assigned split")
	}
	if err := activation.LicenseConsent.Validate(); err != nil {
		return fmt.Errorf("license_consent: %w", err)
	}
	if err := validateText("reason", activation.Reason, 4096, true); err != nil {
		return err
	}
	if activation.ActivatedAt.IsZero() {
		return fmt.Errorf("activated_at is required")
	}
	return nil
}

func (reopen CaseReopen) Validate() error {
	if reopen.SchemaVersion != CaseReopenSchemaVersion {
		return fmt.Errorf("unsupported case reopen schema %q", reopen.SchemaVersion)
	}
	if err := validateID("case_id", reopen.CaseID); err != nil {
		return err
	}
	if reopen.ExpectedGovernanceRevision == 0 {
		return fmt.Errorf("expected_governance_revision must be positive")
	}
	if err := validateText("reason", reopen.Reason, 4096, true); err != nil {
		return err
	}
	if err := validateArtifactRefs("evidence_refs", reopen.EvidenceRefs, true); err != nil {
		return err
	}
	if err := validateSortedIDs("affected_experiment_ids", reopen.AffectedExperimentIDs, false); err != nil {
		return err
	}
	if reopen.ReopenedAt.IsZero() {
		return fmt.Errorf("reopened_at is required")
	}
	return nil
}

func (exposure Exposure) Validate() error {
	if exposure.SchemaVersion != ExposureSchemaVersion {
		return fmt.Errorf("unsupported exposure schema %q", exposure.SchemaVersion)
	}
	if err := validateID("evaluation_run_id", exposure.EvaluationRunID); err != nil {
		return err
	}
	if err := validateID("case_id", exposure.CaseID); err != nil {
		return err
	}
	if len(exposure.Observations) != len(exposureComponentOrder) {
		return fmt.Errorf("observations must contain prompt, rule, model, and index exactly once")
	}
	for index, expected := range exposureComponentOrder {
		observation := exposure.Observations[index]
		if observation.Component != expected {
			return fmt.Errorf("observations[%d].component is %q, want %q",
				index, observation.Component, expected)
		}
		if err := observation.Status.Validate(); err != nil {
			return fmt.Errorf("observations[%d]: %w", index, err)
		}
		if err := validateID(
			fmt.Sprintf("observations[%d].revision", index), observation.Revision,
		); err != nil {
			return err
		}
	}
	if exposure.ObservedAt.IsZero() {
		return fmt.Errorf("observed_at is required")
	}
	return nil
}

var exposureComponentOrder = [...]ExposureComponent{
	ExposurePrompt,
	ExposureRule,
	ExposureModel,
	ExposureIndex,
}

func (mutation Mutation) Validate() error {
	if err := validateText("idempotency_key", mutation.IdempotencyKey, 256, false); err != nil {
		return err
	}
	if err := validateText("actor", mutation.Actor, 256, false); err != nil {
		return err
	}
	if err := validateRoles(mutation.Roles); err != nil {
		return err
	}
	if err := validateText("audit", mutation.Audit, 4096, true); err != nil {
		return err
	}
	if mutation.At.IsZero() {
		return fmt.Errorf("mutation time is required")
	}
	return nil
}

func (access Access) Validate() error {
	if err := validateText("actor", access.Actor, 256, false); err != nil {
		return err
	}
	return validateRoles(access.Roles)
}

func (variant PromotionVariant) Validate() error {
	if variant.SchemaVersion != PromotionVariantSchemaVersion {
		return fmt.Errorf("unsupported promotion variant schema %q", variant.SchemaVersion)
	}
	if err := validateID("variant_id", variant.VariantID); err != nil {
		return err
	}
	if err := variant.Component.Validate(); err != nil {
		return err
	}
	if err := validateID("revision", variant.Revision); err != nil {
		return err
	}
	if err := validateID("rollback_revision", variant.RollbackRevision); err != nil {
		return fmt.Errorf("rollback revision is required: %w", err)
	}
	if variant.Revision == variant.RollbackRevision {
		return fmt.Errorf("rollback_revision must differ from revision")
	}
	if err := validateID("policy_revision", variant.PolicyRevision); err != nil {
		return err
	}
	if err := variant.Origin.Validate(); err != nil {
		return err
	}
	if variant.Origin == PromotionProductionFeedback {
		return fmt.Errorf("production feedback cannot register or activate a promotion variant")
	}
	if err := validateText("owner", variant.Owner, 256, false); err != nil {
		return err
	}
	if variant.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	if variant.ManagedBinding != nil {
		if err := variant.ManagedBinding.Validate(); err != nil {
			return fmt.Errorf("managed_binding: %w", err)
		}
		switch variant.ManagedBinding.Kind {
		case PromotionManagementCalibrationConfig:
			if variant.Component != PromotionFilter || variant.Origin != PromotionExperiment {
				return fmt.Errorf("managed calibration promotion must be an experiment filter variant")
			}
			if variant.Revision != variant.ManagedBinding.ConfigRevision {
				return fmt.Errorf("managed promotion revision must equal bound config revision")
			}
		case PromotionManagementNormalizationPolicy:
			if variant.Component != PromotionWorkflow || variant.Origin != PromotionExperiment {
				return fmt.Errorf("managed normalization promotion must be an experiment workflow variant")
			}
			if variant.Revision != variant.ManagedBinding.NormalizationPolicyRevision {
				return fmt.Errorf("managed promotion revision must equal bound normalization policy revision")
			}
			if variant.RollbackRevision != "argus-pi-review-workflow-"+variant.ManagedBinding.NormalizationRollbackRevision {
				return fmt.Errorf("managed promotion rollback revision must equal bound normalization rollback implementation")
			}
		}
	}
	return nil
}

func (binding PromotionManagementBinding) Validate() error {
	calibrationOnlyValues := []string{
		binding.CalibrationRunID, binding.CalibrationRunSHA256, binding.ProfileCandidateID,
		binding.ProfileCandidateSHA256, binding.CalibrationReportSHA256,
	}
	normalizationValues := []string{
		binding.NormalizationPolicyRevision, binding.NormalizationImplementationID,
		binding.NormalizationImplementationRevision, binding.NormalizationImplementationSHA256,
		binding.NormalizationRollbackRevision, binding.NormalizationRollbackSHA256,
		binding.NormalizationGatePolicyID,
		binding.NormalizationGatePolicyRevision, binding.NormalizationGatePolicySHA256,
		binding.NormalizationGatePolicyURI,
	}
	switch binding.Kind {
	case PromotionManagementCalibrationConfig:
		if slices.ContainsFunc(normalizationValues, func(value string) bool { return value != "" }) {
			return fmt.Errorf("calibration management binding must not contain normalization fields")
		}
		if binding.BaselineConfigRevisionID != "" || binding.BaselineConfigRevision != "" ||
			binding.BaselineConfigRevisionSHA256 != "" || binding.VariantBundleSHA256 != "" ||
			binding.NormalizationGatePolicySizeBytes != 0 {
			return fmt.Errorf("calibration management binding must not contain normalization baseline config fields")
		}
		for name, value := range map[string]string{
			"calibration_run_id": binding.CalibrationRunID, "profile_candidate_id": binding.ProfileCandidateID,
			"config_revision_id": binding.ConfigRevisionID, "config_revision": binding.ConfigRevision,
		} {
			if err := validateID(name, value); err != nil {
				return err
			}
		}
		for name, value := range map[string]string{
			"calibration_run_sha256": binding.CalibrationRunSHA256, "profile_candidate_sha256": binding.ProfileCandidateSHA256,
			"calibration_report_sha256": binding.CalibrationReportSHA256, "baseline_bundle_sha256": binding.BaselineBundleSHA256,
			"config_revision_sha256": binding.ConfigRevisionSHA256,
		} {
			if !sha256Pattern.MatchString(value) {
				return fmt.Errorf("%s must be a lowercase SHA-256", name)
			}
		}
	case PromotionManagementNormalizationPolicy:
		if slices.ContainsFunc(calibrationOnlyValues, func(value string) bool { return value != "" }) {
			return fmt.Errorf("normalization management binding must not contain calibration fields")
		}
		for name, value := range map[string]string{
			"baseline_config_revision_id":           binding.BaselineConfigRevisionID,
			"baseline_config_revision":              binding.BaselineConfigRevision,
			"config_revision_id":                    binding.ConfigRevisionID,
			"config_revision":                       binding.ConfigRevision,
			"normalization_policy_revision":         binding.NormalizationPolicyRevision,
			"normalization_implementation_id":       binding.NormalizationImplementationID,
			"normalization_implementation_revision": binding.NormalizationImplementationRevision,
			"normalization_rollback_revision":       binding.NormalizationRollbackRevision,
			"normalization_gate_policy_id":          binding.NormalizationGatePolicyID,
			"normalization_gate_policy_revision":    binding.NormalizationGatePolicyRevision,
		} {
			if err := validateID(name, value); err != nil {
				return err
			}
		}
		for name, value := range map[string]string{
			"baseline_bundle_sha256":              binding.BaselineBundleSHA256,
			"variant_bundle_sha256":               binding.VariantBundleSHA256,
			"baseline_config_revision_sha256":     binding.BaselineConfigRevisionSHA256,
			"config_revision_sha256":              binding.ConfigRevisionSHA256,
			"normalization_implementation_sha256": binding.NormalizationImplementationSHA256,
			"normalization_rollback_sha256":       binding.NormalizationRollbackSHA256,
			"normalization_gate_policy_sha256":    binding.NormalizationGatePolicySHA256,
		} {
			if !sha256Pattern.MatchString(value) {
				return fmt.Errorf("%s must be a lowercase SHA-256", name)
			}
		}
		if binding.NormalizationImplementationID != "candidate-normalization" ||
			binding.NormalizationImplementationRevision == binding.NormalizationRollbackRevision ||
			binding.NormalizationImplementationSHA256 != binding.NormalizationRollbackSHA256 {
			return fmt.Errorf("normalization implementation binding is invalid")
		}
		if binding.NormalizationPolicyRevision != "argus-pi-review-workflow-"+binding.NormalizationImplementationRevision {
			return fmt.Errorf("normalization policy revision does not match implementation revision")
		}
		if err := validateArtifactRef("normalization_gate_policy_uri", binding.NormalizationGatePolicyURI); err != nil ||
			binding.NormalizationGatePolicyURI != "artifact://local/sha256/"+binding.NormalizationGatePolicySHA256 ||
			binding.NormalizationGatePolicySizeBytes <= 0 {
			return fmt.Errorf("normalization gate policy artifact binding is invalid")
		}
	default:
		return fmt.Errorf("unsupported management kind %q", binding.Kind)
	}
	return nil
}

func (result GateResult) Validate() error {
	if result.SchemaVersion != GateResultSchemaVersion {
		return fmt.Errorf("unsupported gate result schema %q", result.SchemaVersion)
	}
	if err := validateID("variant_id", result.VariantID); err != nil {
		return err
	}
	if err := result.Gate.Validate(); err != nil {
		return err
	}
	if err := result.Outcome.Validate(); err != nil {
		return err
	}
	if err := result.Evidence.Validate(result.Gate, result.Outcome); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	return validateText("summary", result.Summary, 4096, true)
}

func (evidence GateEvidence) Validate(gate PromotionGate, outcome GateOutcome) error {
	if err := validateArtifactRefs("refs", evidence.Refs, true); err != nil {
		return err
	}
	if err := evidence.Basis.Validate(); err != nil {
		return err
	}
	if evidence.SafetyEventCount < 0 {
		return fmt.Errorf("safety_event_count must not be negative")
	}
	switch outcome {
	case GatePass:
		if !evidence.ChecksPassed {
			return fmt.Errorf("pass requires checks_passed")
		}
		if evidence.Basis == EvidenceJudgeOnly {
			return fmt.Errorf("judge-only evidence cannot pass a promotion gate")
		}
	case GateFail, GateInconclusive:
		if evidence.ChecksPassed {
			return fmt.Errorf("%s result cannot claim checks_passed", outcome)
		}
	}
	if gate == GateFixedHoldout {
		if (evidence.EvaluationRunID == "") == (evidence.NormalizationQualityRunID == "") {
			return fmt.Errorf("fixed_holdout requires exactly one evaluation or normalization quality run ID")
		}
		if evidence.EvaluationRunID != "" {
			if err := validateID("evaluation_run_id", evidence.EvaluationRunID); err != nil {
				return err
			}
		}
		if evidence.NormalizationQualityRunID != "" {
			if err := validateID("normalization_quality_run_id", evidence.NormalizationQualityRunID); err != nil {
				return err
			}
		}
		if err := validateSortedIDs("holdout_case_ids", evidence.HoldoutCaseIDs, true); err != nil {
			return err
		}
	} else if evidence.HoldoutCaseIDs == nil || len(evidence.HoldoutCaseIDs) != 0 {
		return fmt.Errorf("holdout_case_ids must be an empty array outside fixed_holdout")
	}
	if evidence.NormalizationQualityRunID != "" && gate != GateTargetedRegression && gate != GateFixedHoldout {
		return fmt.Errorf("normalization_quality_run_id is only valid for targeted_regression or fixed_holdout")
	}
	if (gate == GateShadowTraffic || gate == GateCanary) &&
		outcome == GatePass && evidence.SafetyEventCount != 0 {
		return fmt.Errorf("%s pass requires zero safety events", gate)
	}
	if gate == GateRollbackMonitor {
		if outcome == GatePass && !evidence.RollbackVerified {
			return fmt.Errorf("rollback_monitor pass requires rollback_verified")
		}
	} else if evidence.RollbackVerified {
		return fmt.Errorf("rollback_verified is only valid for rollback_monitor")
	}
	if gate == GateAuthorization {
		if outcome == GatePass {
			if evidence.Authorization == nil {
				return fmt.Errorf("promotion_authorization pass requires authorization")
			}
			if err := evidence.Authorization.Validate(); err != nil {
				return err
			}
		} else if evidence.Authorization != nil {
			return fmt.Errorf("non-pass authorization result must not claim authorization")
		}
	} else if evidence.Authorization != nil {
		return fmt.Errorf("authorization is only valid for promotion_authorization")
	}
	return nil
}

func (authorization PromotionAuthorization) Validate() error {
	switch authorization.Kind {
	case "human":
		if err := validateText("authorized_by", authorization.AuthorizedBy, 256, false); err != nil {
			return err
		}
		if authorization.PolicyRevision != "" {
			return fmt.Errorf("human authorization must not set policy_revision")
		}
	case "policy":
		if err := validateText("authorized_by", authorization.AuthorizedBy, 256, false); err != nil {
			return err
		}
		if err := validateID("authorization.policy_revision", authorization.PolicyRevision); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported authorization kind %q", authorization.Kind)
	}
	return nil
}

func (evaluationCase EvaluationCase) validateStateAndEligibility() error {
	allowed := make(map[UseScope]bool, len(evaluationCase.LicenseConsent.AllowedUses))
	for _, use := range evaluationCase.LicenseConsent.AllowedUses {
		allowed[use] = true
	}
	if evaluationCase.Eligibility.Evaluation && !allowed[UseEvaluation] {
		return fmt.Errorf("evaluation eligibility is not licensed")
	}
	if evaluationCase.Eligibility.Training && !allowed[UseTraining] {
		return fmt.Errorf("training eligibility is not licensed")
	}
	if evaluationCase.Eligibility.Promotion && !allowed[UsePromotion] {
		return fmt.Errorf("promotion eligibility is not licensed")
	}
	switch evaluationCase.DatasetState {
	case DatasetCandidatePool:
		if evaluationCase.Split != SplitUnassigned {
			return fmt.Errorf("candidate_pool case must be unassigned")
		}
		if evaluationCase.Eligibility != (Eligibility{}) {
			return fmt.Errorf("candidate_pool case cannot be evaluation, training, promotion, or production-distribution eligible")
		}
		if !allowed[UseCandidatePool] {
			return fmt.Errorf("candidate_pool use is not licensed")
		}
	case DatasetGold:
		if evaluationCase.ReviewState != ReviewApproved {
			return fmt.Errorf("gold case requires approved review state")
		}
		if evaluationCase.Split != SplitUnassigned {
			return fmt.Errorf("gold case must remain unassigned until activated")
		}
		if evaluationCase.Eligibility.Evaluation || evaluationCase.Eligibility.Training ||
			evaluationCase.Eligibility.Promotion || evaluationCase.Eligibility.ProductionDistribution {
			return fmt.Errorf("unassigned gold case cannot yet be eligible")
		}
	case DatasetActive:
		if evaluationCase.ReviewState != ReviewApproved {
			return fmt.Errorf("active case requires approved review state")
		}
		if evaluationCase.Split == SplitUnassigned {
			return fmt.Errorf("active case requires a split")
		}
		if !evaluationCase.Eligibility.Evaluation {
			return fmt.Errorf("active case requires evaluation eligibility")
		}
	case DatasetRetired:
		if evaluationCase.Split != SplitUnassigned || evaluationCase.Eligibility != (Eligibility{}) {
			return fmt.Errorf("retired case must be unassigned and ineligible")
		}
	}
	if evaluationCase.Split == SplitHoldout {
		if evaluationCase.DatasetState != DatasetActive {
			return fmt.Errorf("holdout case must be active")
		}
		if evaluationCase.Eligibility.Training {
			return fmt.Errorf("holdout case cannot be training eligible")
		}
		if !evaluationCase.Eligibility.Promotion {
			return fmt.Errorf("holdout case must be promotion eligible")
		}
	}
	return nil
}

func (evaluationCase EvaluationCase) validateGovernedSourcePolicy() error {
	switch evaluationCase.Provenance.Kind {
	case SourceHumanConfirmedFinding:
		if evaluationCase.Type != CasePositiveLocalized {
			return fmt.Errorf("human-confirmed finding must create positive_localized case")
		}
	case SourceRejectedFalsePositive:
		if evaluationCase.Type != CaseFalsePositiveRegression {
			return fmt.Errorf("rejected false positive must create false_positive_regression case")
		}
	case SourceIncidentMissedDefect:
		if evaluationCase.Type != CaseMissedDefectRegression {
			return fmt.Errorf("incident missed defect must create missed_defect_regression case")
		}
	case SourceReviewedBugFixPair:
		if evaluationCase.Type != CaseFixValidation {
			return fmt.Errorf("reviewed bug-fix pair must create fix_validation case")
		}
	case SourceMutation:
		if evaluationCase.Type != CaseMutationDiagnostic {
			return fmt.Errorf("mutation source must create mutation_diagnostic case")
		}
	case SourceSynthetic:
		if evaluationCase.Type != CaseMutationDiagnostic &&
			evaluationCase.Type != CaseWorkflowInvariant &&
			evaluationCase.Type != CaseNegativeClean {
			return fmt.Errorf("synthetic source is not valid for case type %q", evaluationCase.Type)
		}
	}
	if evaluationCase.Provenance.Kind == SourceMutation ||
		evaluationCase.Provenance.Kind == SourceSynthetic {
		if evaluationCase.Eligibility.ProductionDistribution {
			return fmt.Errorf("%s case cannot represent production distribution",
				evaluationCase.Provenance.Kind)
		}
	}
	return nil
}

func (evaluationCase EvaluationCase) validateIngressSourcePolicy() error {
	if evaluationCase.Provenance.Kind == SourceProductionFeedback &&
		(evaluationCase.DatasetState != DatasetCandidatePool ||
			evaluationCase.Split != SplitUnassigned ||
			evaluationCase.Eligibility != (Eligibility{})) {
		return fmt.Errorf("production feedback may only enter the unassigned, ineligible candidate pool")
	}
	return nil
}

func validateLabelForCase(caseType CaseType, label Label) error {
	allowed := caseTypeAllowsOutcome(caseType, label.ExpectedOutcome)
	switch caseType {
	case CasePositiveLocalized:
		if allowed && (len(label.AnchorRefs) == 0 || len(label.Anchors) == 0) {
			return fmt.Errorf("positive_localized label requires at least one anchor")
		}
	case CaseNegativeClean:
	case CaseFalsePositiveRegression:
		if allowed && len(label.SuppressionTargets) == 0 {
			return fmt.Errorf("false_positive_regression label requires at least one suppression target")
		}
	case CaseMissedDefectRegression:
		if allowed && (len(label.AnchorRefs) == 0 || len(label.Anchors) == 0) {
			return fmt.Errorf("missed_defect_regression label requires at least one anchor")
		}
	case CaseFixValidation:
	case CaseMutationDiagnostic:
	case CaseWorkflowInvariant:
	}
	if !allowed {
		return fmt.Errorf("expected outcome %q is invalid for case type %q",
			label.ExpectedOutcome, caseType)
	}
	if caseType != CaseFalsePositiveRegression && len(label.SuppressionTargets) != 0 {
		return fmt.Errorf("suppression targets are only valid for false_positive_regression cases")
	}
	if label.ExpectedOutcome == OutcomeDefectPresent ||
		label.ExpectedOutcome == OutcomeMissedDefect {
		if label.Category == "" || label.Severity == "" {
			return fmt.Errorf("%s label requires category and severity", caseType)
		}
	}
	return nil
}

func caseTypeAllowsOutcome(caseType CaseType, outcome ExpectedOutcome) bool {
	switch caseType {
	case CasePositiveLocalized:
		return outcome == OutcomeDefectPresent
	case CaseNegativeClean:
		return outcome == OutcomeClean
	case CaseFalsePositiveRegression:
		return outcome == OutcomeFalsePositive
	case CaseMissedDefectRegression:
		return outcome == OutcomeMissedDefect
	case CaseFixValidation:
		return outcome == OutcomeFixValid || outcome == OutcomeFixInvalid
	case CaseMutationDiagnostic:
		return outcome == OutcomeDefectPresent || outcome == OutcomeClean
	case CaseWorkflowInvariant:
		return outcome == OutcomeInvariantPass || outcome == OutcomeInvariantFail
	default:
		return false
	}
}

// ValidateDatasetContamination fails when active cases cross repository,
// chronological, or semantic-clone split boundaries.
func ValidateDatasetContamination(cases []EvaluationCase) error {
	seenCases := make(map[string]struct{}, len(cases))
	repositorySplits := make(map[string]Split)
	cloneSplits := make(map[string]Split)
	type bounds struct {
		min time.Time
		max time.Time
	}
	timeBounds := make(map[Split]bounds)
	for index, evaluationCase := range cases {
		if err := evaluationCase.validateGoverned(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		if _, duplicate := seenCases[evaluationCase.CaseID]; duplicate {
			return fmt.Errorf("%w: duplicate case_id %q", ErrContaminated, evaluationCase.CaseID)
		}
		seenCases[evaluationCase.CaseID] = struct{}{}
		if evaluationCase.DatasetState != DatasetActive {
			continue
		}
		if previous, exists := repositorySplits[evaluationCase.Provenance.RepositoryID]; exists &&
			previous != evaluationCase.Split {
			return fmt.Errorf("%w: repository %q crosses %s and %s",
				ErrContaminated, evaluationCase.Provenance.RepositoryID, previous, evaluationCase.Split)
		}
		repositorySplits[evaluationCase.Provenance.RepositoryID] = evaluationCase.Split
		if previous, exists := cloneSplits[evaluationCase.CloneGroupID]; exists &&
			previous != evaluationCase.Split {
			return fmt.Errorf("%w: clone group %q crosses %s and %s",
				ErrContaminated, evaluationCase.CloneGroupID, previous, evaluationCase.Split)
		}
		cloneSplits[evaluationCase.CloneGroupID] = evaluationCase.Split
		current := timeBounds[evaluationCase.Split]
		observed := evaluationCase.Provenance.ObservedAt.UTC()
		if current.min.IsZero() || observed.Before(current.min) {
			current.min = observed
		}
		if current.max.IsZero() || observed.After(current.max) {
			current.max = observed
		}
		timeBounds[evaluationCase.Split] = current
	}
	ordered := []Split{SplitTrain, SplitDev, SplitTest, SplitHoldout}
	var previousMax time.Time
	var previousSplit Split
	for _, split := range ordered {
		current, exists := timeBounds[split]
		if !exists {
			continue
		}
		if !previousMax.IsZero() && !previousMax.Before(current.min) {
			return fmt.Errorf("%w: %s maximum time %s is not before %s minimum time %s",
				ErrContaminated, previousSplit, previousMax.Format(time.RFC3339Nano),
				split, current.min.Format(time.RFC3339Nano))
		}
		previousMax = current.max
		previousSplit = split
	}
	return nil
}

func (caseType CaseType) Validate() error {
	switch caseType {
	case CasePositiveLocalized, CaseNegativeClean, CaseFalsePositiveRegression,
		CaseMissedDefectRegression, CaseFixValidation, CaseMutationDiagnostic,
		CaseWorkflowInvariant:
		return nil
	default:
		return fmt.Errorf("unsupported case type %q", caseType)
	}
}

func (kind SourceKind) Validate() error {
	switch kind {
	case SourceHumanConfirmedFinding, SourceRejectedFalsePositive,
		SourceIncidentMissedDefect, SourceReviewedBugFixPair, SourceMutation,
		SourceSynthetic, SourceProductionFeedback:
		return nil
	default:
		return fmt.Errorf("unsupported source kind %q", kind)
	}
}

func (classification Classification) Validate() error {
	switch classification {
	case ClassificationPublic, ClassificationInternal, ClassificationConfidential,
		ClassificationRestricted:
		return nil
	default:
		return fmt.Errorf("unsupported classification %q", classification)
	}
}

func (consent ConsentBasis) Validate() error {
	switch consent {
	case ConsentExplicit, ConsentContractual, ConsentAuthorizedInternal,
		ConsentPublicLicense, ConsentSynthetic:
		return nil
	default:
		return fmt.Errorf("unsupported consent basis %q", consent)
	}
}

func (scope UseScope) Validate() error {
	switch scope {
	case UseCandidatePool, UseEvaluation, UseTraining, UsePromotion:
		return nil
	default:
		return fmt.Errorf("unsupported allowed use %q", scope)
	}
}

func (state ReviewState) Validate() error {
	switch state {
	case ReviewPending, ReviewInReview, ReviewApproved, ReviewRejected:
		return nil
	default:
		return fmt.Errorf("unsupported review state %q", state)
	}
}

func (state DatasetState) Validate() error {
	switch state {
	case DatasetCandidatePool, DatasetGold, DatasetActive, DatasetRetired:
		return nil
	default:
		return fmt.Errorf("unsupported dataset state %q", state)
	}
}

func (split Split) Validate() error {
	switch split {
	case SplitUnassigned, SplitTrain, SplitDev, SplitTest, SplitHoldout:
		return nil
	default:
		return fmt.Errorf("unsupported split %q", split)
	}
}

func (outcome ExpectedOutcome) Validate() error {
	switch outcome {
	case OutcomeDefectPresent, OutcomeClean, OutcomeFalsePositive,
		OutcomeMissedDefect, OutcomeFixValid, OutcomeFixInvalid,
		OutcomeInvariantPass, OutcomeInvariantFail:
		return nil
	default:
		return fmt.Errorf("unsupported expected outcome %q", outcome)
	}
}

func (status ExposureStatus) Validate() error {
	switch status {
	case ExposureSeen, ExposureNotSeen:
		return nil
	default:
		return fmt.Errorf("unsupported exposure status %q", status)
	}
}

func (verdict AnnotationVerdict) Validate() error {
	switch verdict {
	case AnnotationApprove, AnnotationReject:
		return nil
	default:
		return fmt.Errorf("unsupported annotation verdict %q", verdict)
	}
}

func (outcome AdjudicationOutcome) Validate() error {
	switch outcome {
	case AdjudicationApprove, AdjudicationReject:
		return nil
	default:
		return fmt.Errorf("unsupported adjudication outcome %q", outcome)
	}
}

func (component PromotionComponent) Validate() error {
	switch component {
	case PromotionRulePack, PromotionPrompt, PromotionModel, PromotionWorkflow,
		PromotionFilter:
		return nil
	default:
		return fmt.Errorf("unsupported promotion component %q", component)
	}
}

func (origin PromotionOrigin) Validate() error {
	switch origin {
	case PromotionConfigurationChange, PromotionExperiment, PromotionProductionFeedback:
		return nil
	default:
		return fmt.Errorf("unsupported promotion origin %q", origin)
	}
}

func (gate PromotionGate) Validate() error {
	if slices.Contains(promotionGateOrder[:], gate) {
		return nil
	}
	return fmt.Errorf("unsupported promotion gate %q", gate)
}

func (outcome GateOutcome) Validate() error {
	switch outcome {
	case GatePass, GateFail, GateInconclusive:
		return nil
	default:
		return fmt.Errorf("unsupported gate outcome %q", outcome)
	}
}

func (basis EvidenceBasis) Validate() error {
	switch basis {
	case EvidenceDeterministic, EvidenceHumanCalibrated, EvidenceJudgeOnly:
		return nil
	default:
		return fmt.Errorf("unsupported evidence basis %q", basis)
	}
}

func validateRoles(roles []Role) error {
	if roles == nil || len(roles) == 0 {
		return fmt.Errorf("roles must be a non-empty array")
	}
	for index, role := range roles {
		switch role {
		case RoleDatasetCurator, RoleFeedbackIngest, RoleHoldoutMaintainer,
			RoleHoldoutRunner, RolePromotionOperator, RolePromotionApprover,
			RoleDatasetReviewer, RoleDatasetAdjudicator, RoleIncidentIngest,
			RoleProbeIngest, RoleGovernanceTrustAdmin:
		default:
			return fmt.Errorf("roles[%d] contains unsupported role %q", index, role)
		}
		if index > 0 && roles[index-1] >= role {
			return fmt.Errorf("roles must be sorted and unique")
		}
	}
	return nil
}

func hasRole(roles []Role, wanted Role) bool {
	return slices.Contains(roles, wanted)
}

func validateArtifactRefs(name string, refs []string, requireNonEmpty bool) error {
	if refs == nil {
		return fmt.Errorf("%s must be an array", name)
	}
	if requireNonEmpty && len(refs) == 0 {
		return fmt.Errorf("%s must be non-empty", name)
	}
	for index, ref := range refs {
		if err := validateArtifactRef(fmt.Sprintf("%s[%d]", name, index), ref); err != nil {
			return err
		}
		if index > 0 && refs[index-1] >= ref {
			return fmt.Errorf("%s must be sorted and unique", name)
		}
	}
	return nil
}

func validateArtifactRef(name, ref string) error {
	if len(ref) > 2048 || ref != strings.TrimSpace(ref) || !utf8.ValidString(ref) {
		return fmt.Errorf("%s is invalid", name)
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return fmt.Errorf("%s: parse artifact reference: %w", name, err)
	}
	if parsed.Scheme != "artifact" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must use artifact://authority/path", name)
	}
	if parsed.Path == "" || parsed.Path == "/" || path.Clean(parsed.Path) != parsed.Path ||
		strings.Contains(parsed.Path, "..") || parsed.EscapedPath() != parsed.Path {
		return fmt.Errorf("%s contains an unsafe artifact path", name)
	}
	for _, character := range parsed.Host + parsed.Path {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("%s contains unsafe characters", name)
		}
	}
	return nil
}

func validateSortedIDs(name string, values []string, requireNonEmpty bool) error {
	if values == nil {
		return fmt.Errorf("%s must be an array", name)
	}
	if requireNonEmpty && len(values) == 0 {
		return fmt.Errorf("%s must be non-empty", name)
	}
	for index, value := range values {
		if err := validateID(fmt.Sprintf("%s[%d]", name, index), value); err != nil {
			return err
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("%s must be sorted and unique", name)
		}
	}
	return nil
}

func validateID(name, value string) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be non-empty, trimmed UTF-8 within 256 bytes", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) ||
			!(unicode.IsLetter(character) || unicode.IsDigit(character) ||
				strings.ContainsRune("._~:/@+-", character)) {
			return fmt.Errorf("%s contains unsafe characters", name)
		}
	}
	return nil
}

func validateText(name, value string, maxBytes int, allowSpace bool) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be non-empty, trimmed UTF-8 within %d bytes", name, maxBytes)
	}
	for _, character := range value {
		if unicode.IsControl(character) || !allowSpace && unicode.IsSpace(character) {
			return fmt.Errorf("%s contains unsafe characters", name)
		}
	}
	return nil
}
