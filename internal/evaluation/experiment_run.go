package evaluation

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	ExperimentRunRequestSchemaVersion = "argus.experiment_run_request.v1alpha1"
	ExperimentRunSchemaVersion        = "argus.experiment_run.v1alpha1"
	ExperimentLatencyStageAttempts    = "review_run_stage_attempts"
	ExperimentLatencyPostprocessOnly  = "unavailable_postprocessing_only"
	experimentRunEventSchemaVersion   = "argus.experiment_run_event.v1alpha1"
	experimentRunStream               = "evaluation/experiments"
)

type ExperimentTransition string

const (
	ExperimentImproved     ExperimentTransition = "improved"
	ExperimentRegressed    ExperimentTransition = "regressed"
	ExperimentUnchanged    ExperimentTransition = "unchanged"
	ExperimentInconclusive ExperimentTransition = "inconclusive"
)

type ExperimentRunRequest struct {
	SchemaVersion           string                  `json:"schema_version"`
	ExperimentRunID         string                  `json:"experiment_run_id"`
	ExperimentRevision      string                  `json:"experiment_revision"`
	BaselineEvaluationRunID string                  `json:"baseline_evaluation_run_id"`
	VariantEvaluationRunID  string                  `json:"variant_evaluation_run_id"`
	Variable                runmodel.ReplayVariable `json:"variable"`
	CreatedAt               time.Time               `json:"created_at"`
}

// EvaluationDimensionComparison pairs the same semantic review dimension by
// ID while preserving each side's exact version. This lets a skill-pack replay
// compare builtin-v1 with builtin-v2 without pretending the artifacts are the
// same component.
type EvaluationDimensionComparison struct {
	DimensionID                  string                         `json:"dimension_id"`
	BaselineDimension            contractsv1alpha1.VersionedRef `json:"baseline_dimension"`
	VariantDimension             contractsv1alpha1.VersionedRef `json:"variant_dimension"`
	BaselineCandidateCount       uint32                         `json:"baseline_candidate_count"`
	VariantCandidateCount        uint32                         `json:"variant_candidate_count"`
	CandidateDelta               int64                          `json:"candidate_delta"`
	BaselineRawCandidateCount    uint32                         `json:"baseline_raw_candidate_count"`
	VariantRawCandidateCount     uint32                         `json:"variant_raw_candidate_count"`
	RawCandidateDelta            int64                          `json:"raw_candidate_delta"`
	BaselineRetainedCount        uint32                         `json:"baseline_normalization_retained_count"`
	VariantRetainedCount         uint32                         `json:"variant_normalization_retained_count"`
	RetainedDelta                int64                          `json:"normalization_retained_delta"`
	BaselineMergedDuplicateCount uint32                         `json:"baseline_merged_duplicate_count"`
	VariantMergedDuplicateCount  uint32                         `json:"variant_merged_duplicate_count"`
	MergedDuplicateDelta         int64                          `json:"merged_duplicate_delta"`
	BaselineRejectedInvalidCount uint32                         `json:"baseline_rejected_invalid_count"`
	VariantRejectedInvalidCount  uint32                         `json:"variant_rejected_invalid_count"`
	RejectedInvalidDelta         int64                          `json:"rejected_invalid_delta"`
	BaselineExcludedBudgetCount  uint32                         `json:"baseline_excluded_budget_count"`
	VariantExcludedBudgetCount   uint32                         `json:"variant_excluded_budget_count"`
	ExcludedBudgetDelta          int64                          `json:"excluded_budget_delta"`
	BaselineContextGapCount      uint32                         `json:"baseline_context_gap_count"`
	VariantContextGapCount       uint32                         `json:"variant_context_gap_count"`
	ContextGapDelta              int64                          `json:"context_gap_delta"`
	BaselineFindingCount         uint32                         `json:"baseline_finding_count"`
	VariantFindingCount          uint32                         `json:"variant_finding_count"`
	FindingDelta                 int64                          `json:"finding_delta"`
	BaselineEvaluatedFindings    uint32                         `json:"baseline_evaluated_finding_count"`
	VariantEvaluatedFindings     uint32                         `json:"variant_evaluated_finding_count"`
	EvaluatedFindingDelta        int64                          `json:"evaluated_finding_delta"`
	BaselineCumulativeDurationMS uint64                         `json:"baseline_cumulative_duration_ms"`
	VariantCumulativeDurationMS  uint64                         `json:"variant_cumulative_duration_ms"`
	CumulativeDurationDeltaMS    int64                          `json:"cumulative_duration_delta_ms"`
	BaselineTotalTokens          uint64                         `json:"baseline_total_tokens"`
	VariantTotalTokens           uint64                         `json:"variant_total_tokens"`
	TotalTokenDelta              int64                          `json:"total_token_delta"`
	BaselineExecutionCoverage    MetricAvailability             `json:"baseline_execution_coverage"`
	VariantExecutionCoverage     MetricAvailability             `json:"variant_execution_coverage"`
	BaselineNormalization        MetricAvailability             `json:"baseline_normalization"`
	VariantNormalization         MetricAvailability             `json:"variant_normalization"`
	Normalization                MetricAvailability             `json:"normalization"`
	BaselineContextCoverage      MetricAvailability             `json:"baseline_context_coverage"`
	VariantContextCoverage       MetricAvailability             `json:"variant_context_coverage"`
	ContextCoverage              MetricAvailability             `json:"context_coverage"`
	BaselineUsage                MetricAvailability             `json:"baseline_usage"`
	VariantUsage                 MetricAvailability             `json:"variant_usage"`
	Usage                        MetricAvailability             `json:"usage"`
	BaselineVerdict              EvaluationVerdict              `json:"baseline_verdict"`
	VariantVerdict               EvaluationVerdict              `json:"variant_verdict"`
	Transition                   ExperimentTransition           `json:"transition"`
	ReasonCode                   string                         `json:"reason_code"`
}

type ExperimentCaseComparison struct {
	CaseID                       string                          `json:"case_id"`
	CaseType                     CaseType                        `json:"case_type"`
	LabelRevision                uint64                          `json:"label_revision"`
	BaselineReviewRunID          string                          `json:"baseline_review_run_id"`
	VariantReviewRunID           string                          `json:"variant_review_run_id"`
	BaselineReviewRunRef         runmodel.ArtifactRef            `json:"baseline_review_run_ref"`
	VariantReviewRunRef          runmodel.ArtifactRef            `json:"variant_review_run_ref"`
	ReplayChangeSetRef           runmodel.ArtifactRef            `json:"replay_change_set_ref"`
	BaselineConfigSHA256         string                          `json:"baseline_config_sha256"`
	VariantConfigSHA256          string                          `json:"variant_config_sha256"`
	BaselineDurationMS           int64                           `json:"baseline_duration_ms"`
	VariantDurationMS            int64                           `json:"variant_duration_ms"`
	LatencyDeltaMS               int64                           `json:"latency_delta_ms"`
	LatencyAuthority             string                          `json:"latency_authority"`
	ExpectedAnchorCount          uint32                          `json:"expected_anchor_count"`
	BaselineMatchedAnchors       uint32                          `json:"baseline_matched_anchor_count"`
	VariantMatchedAnchors        uint32                          `json:"variant_matched_anchor_count"`
	MatchedAnchorDelta           int64                           `json:"matched_anchor_delta"`
	BaselineLocalization         MetricAvailability              `json:"baseline_localization"`
	VariantLocalization          MetricAvailability              `json:"variant_localization"`
	Localization                 MetricAvailability              `json:"localization"`
	ExpectedSuppressionCount     uint32                          `json:"expected_suppression_count"`
	BaselineObservedSuppressions uint32                          `json:"baseline_observed_suppressions"`
	VariantObservedSuppressions  uint32                          `json:"variant_observed_suppressions"`
	BaselineSuppressedTargets    uint32                          `json:"baseline_suppressed_targets"`
	VariantSuppressedTargets     uint32                          `json:"variant_suppressed_targets"`
	SuppressedTargetDelta        int64                           `json:"suppressed_target_delta"`
	BaselineEscapedTargets       uint32                          `json:"baseline_escaped_targets"`
	VariantEscapedTargets        uint32                          `json:"variant_escaped_targets"`
	EscapedTargetDelta           int64                           `json:"escaped_target_delta"`
	BaselineInconclusiveTargets  uint32                          `json:"baseline_inconclusive_targets"`
	VariantInconclusiveTargets   uint32                          `json:"variant_inconclusive_targets"`
	BaselineFilterEfficacy       MetricAvailability              `json:"baseline_filter_efficacy"`
	VariantFilterEfficacy        MetricAvailability              `json:"variant_filter_efficacy"`
	FilterEfficacy               MetricAvailability              `json:"filter_efficacy"`
	BaselineApplyChecksPassed    uint32                          `json:"baseline_apply_checks_passed"`
	VariantApplyChecksPassed     uint32                          `json:"variant_apply_checks_passed"`
	ApplyChecksPassedDelta       int64                           `json:"apply_checks_passed_delta"`
	BaselineApplyChecksFailed    uint32                          `json:"baseline_apply_checks_failed"`
	VariantApplyChecksFailed     uint32                          `json:"variant_apply_checks_failed"`
	ApplyChecksFailedDelta       int64                           `json:"apply_checks_failed_delta"`
	BaselineApplyAuthority       string                          `json:"baseline_apply_authority"`
	VariantApplyAuthority        string                          `json:"variant_apply_authority"`
	BaselineApplyFidelity        MetricAvailability              `json:"baseline_apply_fidelity"`
	VariantApplyFidelity         MetricAvailability              `json:"variant_apply_fidelity"`
	ApplyFidelity                MetricAvailability              `json:"apply_fidelity"`
	BaselineUsageAuthority       string                          `json:"baseline_usage_authority"`
	VariantUsageAuthority        string                          `json:"variant_usage_authority"`
	BaselineUsage                MetricAvailability              `json:"baseline_usage"`
	VariantUsage                 MetricAvailability              `json:"variant_usage"`
	Usage                        MetricAvailability              `json:"usage"`
	BaselineInputTokens          uint64                          `json:"baseline_input_tokens"`
	VariantInputTokens           uint64                          `json:"variant_input_tokens"`
	InputTokenDelta              int64                           `json:"input_token_delta"`
	BaselineOutputTokens         uint64                          `json:"baseline_output_tokens"`
	VariantOutputTokens          uint64                          `json:"variant_output_tokens"`
	OutputTokenDelta             int64                           `json:"output_token_delta"`
	BaselineTotalTokens          uint64                          `json:"baseline_total_tokens"`
	VariantTotalTokens           uint64                          `json:"variant_total_tokens"`
	TotalTokenDelta              int64                           `json:"total_token_delta"`
	BaselineVerdict              EvaluationVerdict               `json:"baseline_verdict"`
	VariantVerdict               EvaluationVerdict               `json:"variant_verdict"`
	Transition                   ExperimentTransition            `json:"transition"`
	ReasonCode                   string                          `json:"reason_code"`
	Dimensions                   []EvaluationDimensionComparison `json:"dimensions"`
}

type ExperimentRunSummary struct {
	Cases        uint32 `json:"cases"`
	Improved     uint32 `json:"improved"`
	Regressed    uint32 `json:"regressed"`
	Unchanged    uint32 `json:"unchanged"`
	Inconclusive uint32 `json:"inconclusive"`
	PassDelta    int64  `json:"pass_delta"`
	FailDelta    int64  `json:"fail_delta"`
}

type ExperimentRun struct {
	SchemaVersion           string                     `json:"schema_version"`
	ExperimentRunID         string                     `json:"experiment_run_id"`
	ExperimentRevision      string                     `json:"experiment_revision"`
	BaselineEvaluationRunID string                     `json:"baseline_evaluation_run_id"`
	VariantEvaluationRunID  string                     `json:"variant_evaluation_run_id"`
	EvaluatorRevision       string                     `json:"evaluator_revision"`
	Variable                runmodel.ReplayVariable    `json:"variable"`
	Comparisons             []ExperimentCaseComparison `json:"comparisons"`
	Summary                 ExperimentRunSummary       `json:"summary"`
	Quality                 MetricAvailability         `json:"quality"`
	Cost                    MetricAvailability         `json:"cost"`
	Usage                   MetricAvailability         `json:"usage"`
	Latency                 MetricAvailability         `json:"latency"`
	Instability             MetricAvailability         `json:"instability"`
	CreatedAt               time.Time                  `json:"created_at"`
	RecordedAt              time.Time                  `json:"recorded_at"`
	RecordedBy              string                     `json:"recorded_by"`
}

func (request ExperimentRunRequest) Validate() error {
	if request.SchemaVersion != ExperimentRunRequestSchemaVersion {
		return fmt.Errorf("unsupported experiment run request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"experiment_run_id": request.ExperimentRunID, "experiment_revision": request.ExperimentRevision,
		"baseline_evaluation_run_id": request.BaselineEvaluationRunID,
		"variant_evaluation_run_id":  request.VariantEvaluationRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.BaselineEvaluationRunID == request.VariantEvaluationRunID {
		return fmt.Errorf("baseline and variant evaluation runs must differ")
	}
	if err := request.Variable.Validate(); err != nil {
		return err
	}
	if request.Variable == runmodel.ReplayVariableNone {
		return fmt.Errorf("experiment variable must declare one changed dimension")
	}
	if request.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func (run ExperimentRun) Validate() error {
	if run.SchemaVersion != ExperimentRunSchemaVersion {
		return fmt.Errorf("unsupported experiment run schema %q", run.SchemaVersion)
	}
	request := ExperimentRunRequest{
		SchemaVersion: ExperimentRunRequestSchemaVersion, ExperimentRunID: run.ExperimentRunID,
		ExperimentRevision:      run.ExperimentRevision,
		BaselineEvaluationRunID: run.BaselineEvaluationRunID,
		VariantEvaluationRunID:  run.VariantEvaluationRunID, Variable: run.Variable,
		CreatedAt: run.CreatedAt,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if err := validateID("evaluator_revision", run.EvaluatorRevision); err != nil {
		return err
	}
	if run.Comparisons == nil || len(run.Comparisons) == 0 {
		return fmt.Errorf("comparisons must be a non-empty array")
	}
	if run.RecordedAt.IsZero() || run.RecordedAt.Before(run.CreatedAt) {
		return fmt.Errorf("non-decreasing recorded_at is required")
	}
	if err := validateText("recorded_by", run.RecordedBy, 256, false); err != nil {
		return err
	}
	previous := ""
	for index, comparison := range run.Comparisons {
		if err := comparison.validate(); err != nil {
			return fmt.Errorf("comparisons[%d]: %w", index, err)
		}
		if index > 0 && comparison.CaseID <= previous {
			return fmt.Errorf("comparisons must be uniquely sorted by case_id")
		}
		previous = comparison.CaseID
	}
	if run.Summary != summarizeExperiment(run.Comparisons) {
		return fmt.Errorf("summary is not the exact comparison recomputation")
	}
	if !run.Quality.Available || run.Quality.ReasonCode != "" {
		return fmt.Errorf("quality must be available")
	}
	wantLatency := experimentLatencyAvailability(run.Comparisons)
	if run.Latency != wantLatency {
		return fmt.Errorf("latency is not the exact comparison availability")
	}
	wantUsage := MetricAvailability{Available: true}
	for _, comparison := range run.Comparisons {
		if !comparison.Usage.Available {
			wantUsage = MetricAvailability{ReasonCode: "paired_usage_unavailable"}
			break
		}
	}
	if run.Usage != wantUsage {
		return fmt.Errorf("usage is not the exact paired comparison availability")
	}
	for name, metric := range map[string]MetricAvailability{
		"cost": run.Cost, "instability": run.Instability,
	} {
		if metric.Available || metric.ReasonCode == "" {
			return fmt.Errorf("%s must be unavailable with a reason", name)
		}
		if err := validateID(name+".reason_code", metric.ReasonCode); err != nil {
			return err
		}
	}
	return nil
}

func (comparison ExperimentCaseComparison) validate() error {
	for name, value := range map[string]string{
		"case_id": comparison.CaseID, "baseline_review_run_id": comparison.BaselineReviewRunID,
		"variant_review_run_id": comparison.VariantReviewRunID, "reason_code": comparison.ReasonCode,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if comparison.LabelRevision == 0 {
		return fmt.Errorf("label_revision must be positive")
	}
	if err := comparison.CaseType.Validate(); err != nil {
		return err
	}
	for name, ref := range map[string]runmodel.ArtifactRef{
		"baseline_review_run_ref": comparison.BaselineReviewRunRef,
		"variant_review_run_ref":  comparison.VariantReviewRunRef,
		"replay_change_set_ref":   comparison.ReplayChangeSetRef,
	} {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if comparison.BaselineReviewRunRef.Contract != runmodel.ContractReviewRun ||
		comparison.VariantReviewRunRef.Contract != runmodel.ContractReviewRun ||
		comparison.ReplayChangeSetRef.Contract != runmodel.ContractReplayChangeSet {
		return fmt.Errorf("comparison artifact contracts do not match replay evidence")
	}
	if err := validateSHA256("baseline_config_sha256", comparison.BaselineConfigSHA256); err != nil {
		return err
	}
	if err := validateSHA256("variant_config_sha256", comparison.VariantConfigSHA256); err != nil {
		return err
	}
	if comparison.BaselineConfigSHA256 == comparison.VariantConfigSHA256 {
		return fmt.Errorf("variant config digest must differ from baseline")
	}
	if comparison.BaselineDurationMS < 0 || comparison.VariantDurationMS < 0 {
		return fmt.Errorf("latency durations must not be negative")
	}
	switch comparison.LatencyAuthority {
	case ExperimentLatencyStageAttempts:
		if comparison.LatencyDeltaMS != comparison.VariantDurationMS-comparison.BaselineDurationMS {
			return fmt.Errorf("latency is not the exact committed StageAttempt recomputation")
		}
	case ExperimentLatencyPostprocessOnly:
		if comparison.VariantDurationMS != 0 || comparison.LatencyDeltaMS != 0 {
			return fmt.Errorf("post-processing replay must not invent provider latency")
		}
	default:
		return fmt.Errorf("unsupported latency authority %q", comparison.LatencyAuthority)
	}
	if comparison.BaselineMatchedAnchors > comparison.ExpectedAnchorCount ||
		comparison.VariantMatchedAnchors > comparison.ExpectedAnchorCount {
		return fmt.Errorf("matched anchor counts exceed expected anchors")
	}
	for name, metric := range map[string]MetricAvailability{
		"baseline_localization": comparison.BaselineLocalization,
		"variant_localization":  comparison.VariantLocalization,
	} {
		if metric.Available == (metric.ReasonCode != "") {
			return fmt.Errorf("%s availability and reason are inconsistent", name)
		}
		if metric.ReasonCode != "" {
			if err := validateID(name+".reason_code", metric.ReasonCode); err != nil {
				return err
			}
		}
	}
	if comparison.BaselineLocalization.Available && comparison.VariantLocalization.Available {
		if !comparison.Localization.Available || comparison.Localization.ReasonCode != "" ||
			comparison.MatchedAnchorDelta != int64(comparison.VariantMatchedAnchors)-int64(comparison.BaselineMatchedAnchors) {
			return fmt.Errorf("localization comparison is not the exact paired recomputation")
		}
	} else if comparison.Localization.Available ||
		comparison.Localization.ReasonCode != "paired_localization_unavailable" ||
		comparison.MatchedAnchorDelta != 0 {
		return fmt.Errorf("localization comparison must be unavailable without paired facts")
	}
	if comparison.BaselineObservedSuppressions > comparison.ExpectedSuppressionCount ||
		comparison.VariantObservedSuppressions > comparison.ExpectedSuppressionCount ||
		comparison.BaselineSuppressedTargets+comparison.BaselineEscapedTargets+
			comparison.BaselineInconclusiveTargets != comparison.BaselineObservedSuppressions ||
		comparison.VariantSuppressedTargets+comparison.VariantEscapedTargets+
			comparison.VariantInconclusiveTargets != comparison.VariantObservedSuppressions {
		return fmt.Errorf("filter efficacy comparison counts exceed their evidence populations")
	}
	for name, metric := range map[string]MetricAvailability{
		"baseline_filter_efficacy": comparison.BaselineFilterEfficacy,
		"variant_filter_efficacy":  comparison.VariantFilterEfficacy,
	} {
		if metric.Available == (metric.ReasonCode != "") {
			return fmt.Errorf("%s availability and reason are inconsistent", name)
		}
		if metric.ReasonCode != "" {
			if err := validateID(name+".reason_code", metric.ReasonCode); err != nil {
				return err
			}
		}
	}
	if comparison.BaselineFilterEfficacy.Available && comparison.VariantFilterEfficacy.Available {
		if !comparison.FilterEfficacy.Available || comparison.FilterEfficacy.ReasonCode != "" ||
			comparison.SuppressedTargetDelta != int64(comparison.VariantSuppressedTargets)-
				int64(comparison.BaselineSuppressedTargets) ||
			comparison.EscapedTargetDelta != int64(comparison.VariantEscapedTargets)-
				int64(comparison.BaselineEscapedTargets) {
			return fmt.Errorf("filter efficacy comparison is not the exact paired recomputation")
		}
	} else if comparison.FilterEfficacy.Available ||
		comparison.FilterEfficacy.ReasonCode != "paired_filter_efficacy_unavailable" ||
		comparison.SuppressedTargetDelta != 0 || comparison.EscapedTargetDelta != 0 {
		return fmt.Errorf("filter efficacy comparison must be unavailable without paired facts")
	}
	for name, metric := range map[string]MetricAvailability{
		"baseline_apply_fidelity": comparison.BaselineApplyFidelity,
		"variant_apply_fidelity":  comparison.VariantApplyFidelity,
	} {
		if metric.Available == (metric.ReasonCode != "") {
			return fmt.Errorf("%s availability and reason are inconsistent", name)
		}
		if metric.ReasonCode != "" {
			if err := validateID(name+".reason_code", metric.ReasonCode); err != nil {
				return err
			}
		}
	}
	if comparison.BaselineApplyFidelity.Available && comparison.VariantApplyFidelity.Available {
		if !comparison.ApplyFidelity.Available || comparison.ApplyFidelity.ReasonCode != "" ||
			comparison.ApplyChecksPassedDelta != int64(comparison.VariantApplyChecksPassed)-
				int64(comparison.BaselineApplyChecksPassed) ||
			comparison.ApplyChecksFailedDelta != int64(comparison.VariantApplyChecksFailed)-
				int64(comparison.BaselineApplyChecksFailed) ||
			comparison.BaselineApplyAuthority != ApplyTrialAuthority ||
			comparison.VariantApplyAuthority != ApplyTrialAuthority {
			return fmt.Errorf("apply fidelity comparison is not the exact paired recomputation")
		}
	} else if comparison.ApplyFidelity.Available ||
		comparison.ApplyFidelity.ReasonCode != "paired_apply_fidelity_unavailable" ||
		comparison.ApplyChecksPassedDelta != 0 || comparison.ApplyChecksFailedDelta != 0 {
		return fmt.Errorf("apply fidelity comparison must be unavailable without paired facts")
	}
	for name, metric := range map[string]MetricAvailability{
		"baseline_usage": comparison.BaselineUsage,
		"variant_usage":  comparison.VariantUsage,
	} {
		if metric.Available == (metric.ReasonCode != "") {
			return fmt.Errorf("%s availability and reason are inconsistent", name)
		}
		if metric.ReasonCode != "" {
			if err := validateID(name+".reason_code", metric.ReasonCode); err != nil {
				return err
			}
		}
	}
	if comparison.BaselineUsageAuthority != "unavailable" &&
		comparison.BaselineUsageAuthority != EvaluationUsageAuthority {
		return fmt.Errorf("unsupported baseline usage authority")
	}
	if comparison.VariantUsageAuthority != "unavailable" &&
		comparison.VariantUsageAuthority != EvaluationUsageAuthority {
		return fmt.Errorf("unsupported variant usage authority")
	}
	if comparison.BaselineUsage.Available && comparison.VariantUsage.Available {
		inputDelta, inputOK := checkedExperimentTokenDelta(
			comparison.VariantInputTokens, comparison.BaselineInputTokens,
		)
		outputDelta, outputOK := checkedExperimentTokenDelta(
			comparison.VariantOutputTokens, comparison.BaselineOutputTokens,
		)
		totalDelta, totalOK := checkedExperimentTokenDelta(
			comparison.VariantTotalTokens, comparison.BaselineTotalTokens,
		)
		if !inputOK || !outputOK || !totalOK || !comparison.Usage.Available ||
			comparison.Usage.ReasonCode != "" ||
			comparison.BaselineUsageAuthority != EvaluationUsageAuthority ||
			comparison.VariantUsageAuthority != EvaluationUsageAuthority ||
			comparison.InputTokenDelta != inputDelta ||
			comparison.OutputTokenDelta != outputDelta ||
			comparison.TotalTokenDelta != totalDelta {
			return fmt.Errorf("usage comparison is not the exact paired recomputation")
		}
	} else if comparison.Usage.Available ||
		comparison.Usage.ReasonCode != "paired_usage_unavailable" ||
		comparison.InputTokenDelta != 0 || comparison.OutputTokenDelta != 0 ||
		comparison.TotalTokenDelta != 0 {
		return fmt.Errorf("usage comparison must be unavailable without paired facts")
	}
	for name, verdict := range map[string]EvaluationVerdict{
		"baseline_verdict": comparison.BaselineVerdict, "variant_verdict": comparison.VariantVerdict,
	} {
		switch verdict {
		case EvaluationPass, EvaluationFail, EvaluationInconclusive:
		default:
			return fmt.Errorf("unsupported %s %q", name, verdict)
		}
	}
	wantTransition, wantReason := compareEvaluationVerdicts(
		comparison.BaselineVerdict, comparison.VariantVerdict,
	)
	if comparison.Transition != wantTransition || comparison.ReasonCode != wantReason {
		return fmt.Errorf("transition is not the exact verdict recomputation")
	}
	if comparison.Dimensions == nil {
		return fmt.Errorf("dimensions must be an explicit array")
	}
	previousDimension := ""
	for index, dimension := range comparison.Dimensions {
		if err := dimension.validate(); err != nil {
			return fmt.Errorf("dimensions[%d]: %w", index, err)
		}
		if index > 0 && dimension.DimensionID <= previousDimension {
			return fmt.Errorf("dimensions must be uniquely sorted by dimension_id")
		}
		previousDimension = dimension.DimensionID
	}
	return nil
}

func (comparison EvaluationDimensionComparison) validate() error {
	if err := validateID("dimension_id", comparison.DimensionID); err != nil {
		return err
	}
	if err := validateEvaluationDimensionRef("baseline_dimension", comparison.BaselineDimension); err != nil {
		return err
	}
	if err := validateEvaluationDimensionRef("variant_dimension", comparison.VariantDimension); err != nil {
		return err
	}
	if comparison.BaselineDimension.ID != comparison.DimensionID ||
		comparison.VariantDimension.ID != comparison.DimensionID {
		return fmt.Errorf("exact dimension refs must share dimension_id")
	}
	if comparison.CandidateDelta != int64(comparison.VariantCandidateCount)-
		int64(comparison.BaselineCandidateCount) ||
		comparison.RawCandidateDelta != int64(comparison.VariantRawCandidateCount)-
			int64(comparison.BaselineRawCandidateCount) ||
		comparison.RetainedDelta != int64(comparison.VariantRetainedCount)-
			int64(comparison.BaselineRetainedCount) ||
		comparison.MergedDuplicateDelta != int64(comparison.VariantMergedDuplicateCount)-
			int64(comparison.BaselineMergedDuplicateCount) ||
		comparison.RejectedInvalidDelta != int64(comparison.VariantRejectedInvalidCount)-
			int64(comparison.BaselineRejectedInvalidCount) ||
		comparison.ExcludedBudgetDelta != int64(comparison.VariantExcludedBudgetCount)-
			int64(comparison.BaselineExcludedBudgetCount) ||
		comparison.ContextGapDelta != int64(comparison.VariantContextGapCount)-
			int64(comparison.BaselineContextGapCount) ||
		comparison.FindingDelta != int64(comparison.VariantFindingCount)-
			int64(comparison.BaselineFindingCount) ||
		comparison.EvaluatedFindingDelta != int64(comparison.VariantEvaluatedFindings)-
			int64(comparison.BaselineEvaluatedFindings) {
		return fmt.Errorf("candidate, normalization, and finding deltas are not exact recomputations")
	}
	wantNormalization := MetricAvailability{ReasonCode: "paired_dimension_normalization_unavailable"}
	if comparison.BaselineNormalization.Available && comparison.VariantNormalization.Available {
		wantNormalization = MetricAvailability{Available: true}
	}
	if comparison.Normalization != wantNormalization {
		return fmt.Errorf("dimension normalization comparison is not the paired recomputation")
	}
	wantContextCoverage := MetricAvailability{ReasonCode: "paired_dimension_context_coverage_unavailable"}
	if comparison.BaselineContextCoverage.Available &&
		comparison.VariantContextCoverage.Available {
		wantContextCoverage = MetricAvailability{Available: true}
	}
	if comparison.ContextCoverage != wantContextCoverage {
		return fmt.Errorf("dimension context coverage comparison is not the paired recomputation")
	}
	durationDelta, durationOK := checkedExperimentTokenDelta(
		comparison.VariantCumulativeDurationMS, comparison.BaselineCumulativeDurationMS,
	)
	if !durationOK || comparison.CumulativeDurationDeltaMS != durationDelta {
		return fmt.Errorf("cumulative duration delta exceeds int64 or is inconsistent")
	}
	wantUsage := MetricAvailability{ReasonCode: "paired_dimension_usage_unavailable"}
	wantTokenDelta := int64(0)
	if comparison.BaselineUsage.Available && comparison.VariantUsage.Available {
		var ok bool
		wantTokenDelta, ok = checkedExperimentTokenDelta(
			comparison.VariantTotalTokens, comparison.BaselineTotalTokens,
		)
		if !ok {
			return fmt.Errorf("dimension token delta exceeds int64")
		}
		wantUsage = MetricAvailability{Available: true}
	}
	if comparison.Usage != wantUsage || comparison.TotalTokenDelta != wantTokenDelta {
		return fmt.Errorf("dimension usage comparison is not the paired recomputation")
	}
	for name, metric := range map[string]MetricAvailability{
		"baseline_execution_coverage": comparison.BaselineExecutionCoverage,
		"variant_execution_coverage":  comparison.VariantExecutionCoverage,
		"baseline_normalization":      comparison.BaselineNormalization,
		"variant_normalization":       comparison.VariantNormalization,
		"normalization":               comparison.Normalization,
		"baseline_context_coverage":   comparison.BaselineContextCoverage,
		"variant_context_coverage":    comparison.VariantContextCoverage,
		"context_coverage":            comparison.ContextCoverage,
		"baseline_usage":              comparison.BaselineUsage,
		"variant_usage":               comparison.VariantUsage,
	} {
		if metric.Available == (metric.ReasonCode != "") {
			return fmt.Errorf("%s availability and reason are inconsistent", name)
		}
		if metric.ReasonCode != "" {
			if err := validateID(name+".reason_code", metric.ReasonCode); err != nil {
				return err
			}
		}
	}
	wantTransition, wantReason := compareEvaluationVerdicts(
		comparison.BaselineVerdict, comparison.VariantVerdict,
	)
	if comparison.Transition != wantTransition || comparison.ReasonCode != wantReason {
		return fmt.Errorf("dimension transition is not the verdict recomputation")
	}
	return nil
}

func validateExperimentAgainstEvaluationRuns(
	experiment ExperimentRun,
	baseline EvaluationRun,
	variant EvaluationRun,
) error {
	if experiment.EvaluatorRevision != baseline.EvaluatorRevision ||
		baseline.EvaluatorRevision != variant.EvaluatorRevision ||
		len(experiment.Comparisons) != len(baseline.Results) ||
		len(baseline.Results) != len(variant.Results) {
		return fmt.Errorf("experiment does not bind evaluator and case counts")
	}
	for index, comparison := range experiment.Comparisons {
		before := baseline.Results[index]
		after := variant.Results[index]
		wantDimensions, err := buildEvaluationDimensionComparisons(before, after)
		if err != nil || !reflect.DeepEqual(comparison.Dimensions, wantDimensions) {
			return fmt.Errorf("experiment comparison %d does not bind exact dimension results", index)
		}
		if comparison.CaseID != before.CaseID || before.CaseID != after.CaseID ||
			comparison.CaseType != before.CaseType || before.CaseType != after.CaseType ||
			comparison.LabelRevision != before.LabelRevision || before.LabelRevision != after.LabelRevision ||
			comparison.BaselineReviewRunID != before.ReviewRunID ||
			comparison.VariantReviewRunID != after.ReviewRunID ||
			comparison.BaselineReviewRunRef != before.ReviewRunRef ||
			comparison.VariantReviewRunRef != after.ReviewRunRef ||
			comparison.BaselineVerdict != before.Verdict || comparison.VariantVerdict != after.Verdict ||
			comparison.ExpectedAnchorCount != before.ExpectedAnchorCount ||
			before.ExpectedAnchorCount != after.ExpectedAnchorCount ||
			comparison.BaselineMatchedAnchors != before.MatchedAnchorCount ||
			comparison.VariantMatchedAnchors != after.MatchedAnchorCount ||
			comparison.BaselineLocalization != before.Localization ||
			comparison.VariantLocalization != after.Localization ||
			comparison.ExpectedSuppressionCount != before.ExpectedSuppressionCount ||
			before.ExpectedSuppressionCount != after.ExpectedSuppressionCount ||
			comparison.BaselineObservedSuppressions != before.ObservedSuppressionCount ||
			comparison.VariantObservedSuppressions != after.ObservedSuppressionCount ||
			comparison.BaselineSuppressedTargets != before.SuppressedTargetCount ||
			comparison.VariantSuppressedTargets != after.SuppressedTargetCount ||
			comparison.BaselineEscapedTargets != before.EscapedTargetCount ||
			comparison.VariantEscapedTargets != after.EscapedTargetCount ||
			comparison.BaselineInconclusiveTargets != before.InconclusiveTargetCount ||
			comparison.VariantInconclusiveTargets != after.InconclusiveTargetCount ||
			comparison.BaselineFilterEfficacy != before.FilterEfficacy ||
			comparison.VariantFilterEfficacy != after.FilterEfficacy ||
			comparison.BaselineApplyChecksPassed != before.ApplyChecksPassed ||
			comparison.VariantApplyChecksPassed != after.ApplyChecksPassed ||
			comparison.BaselineApplyChecksFailed != before.ApplyChecksFailed ||
			comparison.VariantApplyChecksFailed != after.ApplyChecksFailed ||
			comparison.BaselineApplyAuthority != before.ApplyAuthority ||
			comparison.VariantApplyAuthority != after.ApplyAuthority ||
			comparison.BaselineApplyFidelity != before.ApplyFidelity ||
			comparison.VariantApplyFidelity != after.ApplyFidelity ||
			comparison.BaselineUsageAuthority != before.UsageAuthority ||
			comparison.VariantUsageAuthority != after.UsageAuthority ||
			comparison.BaselineUsage != before.Usage || comparison.VariantUsage != after.Usage ||
			comparison.BaselineInputTokens != before.InputTokens ||
			comparison.VariantInputTokens != after.InputTokens ||
			comparison.BaselineOutputTokens != before.OutputTokens ||
			comparison.VariantOutputTokens != after.OutputTokens ||
			comparison.BaselineTotalTokens != before.TotalTokens ||
			comparison.VariantTotalTokens != after.TotalTokens ||
			before.LabelPolicyRevision != after.LabelPolicyRevision ||
			before.ExpectedOutcome != after.ExpectedOutcome || before.ExpectedCategory != after.ExpectedCategory {
			return fmt.Errorf("experiment comparison %d does not bind exact evaluation results", index)
		}
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != 64 {
		return fmt.Errorf("%s must be a lowercase SHA-256", name)
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return fmt.Errorf("%s must be a lowercase SHA-256", name)
		}
	}
	return nil
}

func DecodeExperimentRunRequest(data []byte) (ExperimentRunRequest, error) {
	return decodeStrict(data, "ExperimentRunRequest", func(value ExperimentRunRequest) error {
		return value.Validate()
	})
}

func DecodeExperimentRun(data []byte) (ExperimentRun, error) {
	return decodeStrict(data, "ExperimentRun", func(value ExperimentRun) error {
		return value.Validate()
	})
}

func (repository *Repository) RecordExperimentRun(
	ctx context.Context,
	request ExperimentRunRequest,
	mutation Mutation,
	runs EvaluationRunReader,
) (ExperimentRun, error) {
	if err := checkContext(ctx); err != nil {
		return ExperimentRun{}, err
	}
	if err := request.Validate(); err != nil {
		return ExperimentRun{}, fmt.Errorf("validate experiment run request: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return ExperimentRun{}, err
	}
	if request.CreatedAt.After(mutation.At.UTC()) {
		return ExperimentRun{}, fmt.Errorf("experiment created_at must not be after mutation time")
	}
	if runs == nil {
		return ExperimentRun{}, fmt.Errorf("review run repository is required")
	}

	repository.mu.Lock()
	state, err := repository.load()
	if err != nil {
		repository.mu.Unlock()
		return ExperimentRun{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		run, retryErr := experimentIdempotentResult(existing, request, mutation)
		repository.mu.Unlock()
		return run, retryErr
	}
	baseline, baselineExists := state.evaluationRuns[request.BaselineEvaluationRunID]
	variant, variantExists := state.evaluationRuns[request.VariantEvaluationRunID]
	if !baselineExists || !variantExists {
		repository.mu.Unlock()
		return ExperimentRun{}, fmt.Errorf("%w: baseline or variant EvaluationRun is missing", ErrNotFound)
	}
	if err := authorizeExperimentRuns(state, baseline, variant, mutation.Roles); err != nil {
		repository.mu.Unlock()
		return ExperimentRun{}, err
	}
	repository.mu.Unlock()

	comparisons, err := buildExperimentComparisons(runs, baseline, variant, request.Variable)
	if err != nil {
		return ExperimentRun{}, err
	}
	experiment := ExperimentRun{
		SchemaVersion: ExperimentRunSchemaVersion, ExperimentRunID: request.ExperimentRunID,
		ExperimentRevision:      request.ExperimentRevision,
		BaselineEvaluationRunID: request.BaselineEvaluationRunID,
		VariantEvaluationRunID:  request.VariantEvaluationRunID,
		EvaluatorRevision:       baseline.EvaluatorRevision, Variable: request.Variable,
		Comparisons: comparisons, Summary: summarizeExperiment(comparisons),
		Quality:     MetricAvailability{Available: true},
		Cost:        MetricAvailability{ReasonCode: "authoritative_billing_unavailable"},
		Usage:       experimentUsageAvailability(comparisons),
		Latency:     experimentLatencyAvailability(comparisons),
		Instability: MetricAvailability{ReasonCode: "repeated_run_sample_unavailable"},
		CreatedAt:   request.CreatedAt.UTC(), RecordedAt: mutation.At.UTC(), RecordedBy: mutation.Actor,
	}
	if err := experiment.Validate(); err != nil {
		return ExperimentRun{}, err
	}
	event := experimentRunEvent{
		SchemaVersion: experimentRunEventSchemaVersion, Request: request, Run: experiment,
		Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit,
		OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err = repository.load()
	if err != nil {
		return ExperimentRun{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		return experimentIdempotentResult(existing, request, mutation)
	}
	currentBaseline, baselineExists := state.evaluationRuns[request.BaselineEvaluationRunID]
	currentVariant, variantExists := state.evaluationRuns[request.VariantEvaluationRunID]
	if !baselineExists || !variantExists || !reflect.DeepEqual(currentBaseline, baseline) ||
		!reflect.DeepEqual(currentVariant, variant) {
		return ExperimentRun{}, fmt.Errorf("%w: evaluation inputs changed during experiment", ErrInvalidTransition)
	}
	if _, exists := state.experimentRuns[request.ExperimentRunID]; exists {
		return ExperimentRun{}, fmt.Errorf("%w: experiment_run_id %q already exists",
			ErrConflict, request.ExperimentRunID)
	}
	if _, err := repository.appendExperimentRun(mutation, event); err != nil {
		return ExperimentRun{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return ExperimentRun{}, err
	}
	return reloaded.experimentRun(request.ExperimentRunID)
}

func buildExperimentComparisons(
	runs EvaluationRunReader,
	baseline EvaluationRun,
	variant EvaluationRun,
	variable runmodel.ReplayVariable,
) ([]ExperimentCaseComparison, error) {
	if baseline.EvaluatorRevision != variant.EvaluatorRevision || len(baseline.Results) != len(variant.Results) {
		return nil, fmt.Errorf("%w: evaluation runs do not share evaluator and case set", ErrInvalidTransition)
	}
	comparisons := make([]ExperimentCaseComparison, 0, len(baseline.Results))
	for index, before := range baseline.Results {
		after := variant.Results[index]
		if before.CaseID != after.CaseID || before.CaseType != after.CaseType ||
			before.LabelRevision != after.LabelRevision ||
			before.LabelPolicyRevision != after.LabelPolicyRevision ||
			before.ExpectedOutcome != after.ExpectedOutcome || before.ExpectedCategory != after.ExpectedCategory {
			return nil, fmt.Errorf("%w: case or label mismatch at %q", ErrInvalidTransition, before.CaseID)
		}
		dimensionComparisons, err := buildEvaluationDimensionComparisons(before, after)
		if err != nil {
			return nil, fmt.Errorf("%w: case %q dimension comparison: %v",
				ErrInvalidTransition, before.CaseID, err)
		}
		baselineRun, err := runs.LoadRun(before.ReviewRunID)
		if err != nil {
			return nil, fmt.Errorf("load baseline ReviewRun %q: %w", before.ReviewRunID, err)
		}
		variantRun, err := runs.LoadRun(after.ReviewRunID)
		if err != nil {
			return nil, fmt.Errorf("load variant ReviewRun %q: %w", after.ReviewRunID, err)
		}
		if variantRun.Kind != runmodel.RunKindReplay || variantRun.SourceRunID != before.ReviewRunID ||
			variantRun.ReplayVariable != variable {
			return nil, fmt.Errorf("%w: variant run %q is not a %s replay of baseline %q",
				ErrInvalidTransition, after.ReviewRunID, variable, before.ReviewRunID)
		}
		if baselineRun.RunID != before.ReviewRunID || baselineRun.Status != runmodel.RunStatusSucceeded {
			return nil, fmt.Errorf("%w: baseline ReviewRun is not succeeded", ErrInvalidTransition)
		}
		baselineDuration, err := committedRunDurationMS(baselineRun)
		if err != nil {
			return nil, fmt.Errorf("%w: compute baseline ReviewRun latency: %v",
				ErrInvalidTransition, err)
		}
		variantDuration := int64(0)
		latencyDelta := int64(0)
		latencyAuthority := ExperimentLatencyPostprocessOnly
		if !variable.IsFilterPolicy() {
			variantDuration, err = committedRunDurationMS(variantRun)
			if err != nil {
				return nil, fmt.Errorf("%w: compute variant ReviewRun latency: %v",
					ErrInvalidTransition, err)
			}
			latencyDelta = variantDuration - baselineDuration
			latencyAuthority = ExperimentLatencyStageAttempts
		}
		baselineRef, err := runs.CommittedRunRef(before.ReviewRunID)
		if err != nil || baselineRef != before.ReviewRunRef {
			return nil, fmt.Errorf("%w: baseline committed ReviewRun ref changed", ErrInvalidTransition)
		}
		variantRef, err := runs.CommittedRunRef(after.ReviewRunID)
		if err != nil || variantRef != after.ReviewRunRef {
			return nil, fmt.Errorf("%w: variant committed ReviewRun ref changed", ErrInvalidTransition)
		}
		variantSnapshot, err := runs.ExecutionSnapshotForRun(after.ReviewRunID)
		if err != nil || variantSnapshot.ReplayChangeSetRef == nil {
			return nil, fmt.Errorf("%w: variant replay has no immutable change set", ErrInvalidTransition)
		}
		var change runmodel.ReplayChangeSet
		if err := runs.ReadJSONArtifact(*variantSnapshot.ReplayChangeSetRef, &change); err != nil {
			return nil, fmt.Errorf("read replay change set: %w", err)
		}
		if err := change.Validate(); err != nil {
			return nil, fmt.Errorf("%w: invalid replay change set: %v", ErrInvalidTransition, err)
		}
		if change.SourceRunID != before.ReviewRunID || change.Variable != variable ||
			change.RemoteWrites != "deny" {
			return nil, fmt.Errorf("%w: replay change set does not prove declared direct variant",
				ErrInvalidTransition)
		}
		transition, reason := compareEvaluationVerdicts(before.Verdict, after.Verdict)
		localization := MetricAvailability{ReasonCode: "paired_localization_unavailable"}
		localizationDelta := int64(0)
		if before.Localization.Available && after.Localization.Available {
			localization = MetricAvailability{Available: true}
			localizationDelta = int64(after.MatchedAnchorCount) - int64(before.MatchedAnchorCount)
		}
		filterEfficacy := MetricAvailability{ReasonCode: "paired_filter_efficacy_unavailable"}
		suppressedTargetDelta := int64(0)
		escapedTargetDelta := int64(0)
		if before.FilterEfficacy.Available && after.FilterEfficacy.Available {
			filterEfficacy = MetricAvailability{Available: true}
			suppressedTargetDelta = int64(after.SuppressedTargetCount) -
				int64(before.SuppressedTargetCount)
			escapedTargetDelta = int64(after.EscapedTargetCount) -
				int64(before.EscapedTargetCount)
		}
		applyFidelity := MetricAvailability{ReasonCode: "paired_apply_fidelity_unavailable"}
		applyPassedDelta := int64(0)
		applyFailedDelta := int64(0)
		if before.ApplyFidelity.Available && after.ApplyFidelity.Available {
			applyFidelity = MetricAvailability{Available: true}
			applyPassedDelta = int64(after.ApplyChecksPassed) - int64(before.ApplyChecksPassed)
			applyFailedDelta = int64(after.ApplyChecksFailed) - int64(before.ApplyChecksFailed)
		}
		usage := MetricAvailability{ReasonCode: "paired_usage_unavailable"}
		inputTokenDelta := int64(0)
		outputTokenDelta := int64(0)
		totalTokenDelta := int64(0)
		if before.Usage.Available && after.Usage.Available {
			var ok bool
			inputTokenDelta, ok = checkedExperimentTokenDelta(after.InputTokens, before.InputTokens)
			if !ok {
				return nil, fmt.Errorf("%w: input token delta exceeds int64", ErrInvalidTransition)
			}
			outputTokenDelta, ok = checkedExperimentTokenDelta(after.OutputTokens, before.OutputTokens)
			if !ok {
				return nil, fmt.Errorf("%w: output token delta exceeds int64", ErrInvalidTransition)
			}
			totalTokenDelta, ok = checkedExperimentTokenDelta(after.TotalTokens, before.TotalTokens)
			if !ok {
				return nil, fmt.Errorf("%w: total token delta exceeds int64", ErrInvalidTransition)
			}
			usage = MetricAvailability{Available: true}
		}
		comparisons = append(comparisons, ExperimentCaseComparison{
			CaseID: before.CaseID, CaseType: before.CaseType, LabelRevision: before.LabelRevision,
			BaselineReviewRunID: before.ReviewRunID, VariantReviewRunID: after.ReviewRunID,
			BaselineReviewRunRef: before.ReviewRunRef, VariantReviewRunRef: after.ReviewRunRef,
			ReplayChangeSetRef:   *variantSnapshot.ReplayChangeSetRef,
			BaselineConfigSHA256: change.BaselineSHA256, VariantConfigSHA256: change.VariantSHA256,
			BaselineDurationMS: baselineDuration, VariantDurationMS: variantDuration,
			LatencyDeltaMS:               latencyDelta,
			LatencyAuthority:             latencyAuthority,
			ExpectedAnchorCount:          before.ExpectedAnchorCount,
			BaselineMatchedAnchors:       before.MatchedAnchorCount,
			VariantMatchedAnchors:        after.MatchedAnchorCount,
			MatchedAnchorDelta:           localizationDelta,
			BaselineLocalization:         before.Localization,
			VariantLocalization:          after.Localization,
			Localization:                 localization,
			ExpectedSuppressionCount:     before.ExpectedSuppressionCount,
			BaselineObservedSuppressions: before.ObservedSuppressionCount,
			VariantObservedSuppressions:  after.ObservedSuppressionCount,
			BaselineSuppressedTargets:    before.SuppressedTargetCount,
			VariantSuppressedTargets:     after.SuppressedTargetCount,
			SuppressedTargetDelta:        suppressedTargetDelta,
			BaselineEscapedTargets:       before.EscapedTargetCount,
			VariantEscapedTargets:        after.EscapedTargetCount,
			EscapedTargetDelta:           escapedTargetDelta,
			BaselineInconclusiveTargets:  before.InconclusiveTargetCount,
			VariantInconclusiveTargets:   after.InconclusiveTargetCount,
			BaselineFilterEfficacy:       before.FilterEfficacy,
			VariantFilterEfficacy:        after.FilterEfficacy,
			FilterEfficacy:               filterEfficacy,
			BaselineApplyChecksPassed:    before.ApplyChecksPassed,
			VariantApplyChecksPassed:     after.ApplyChecksPassed,
			ApplyChecksPassedDelta:       applyPassedDelta,
			BaselineApplyChecksFailed:    before.ApplyChecksFailed,
			VariantApplyChecksFailed:     after.ApplyChecksFailed,
			ApplyChecksFailedDelta:       applyFailedDelta,
			BaselineApplyAuthority:       before.ApplyAuthority,
			VariantApplyAuthority:        after.ApplyAuthority,
			BaselineApplyFidelity:        before.ApplyFidelity,
			VariantApplyFidelity:         after.ApplyFidelity,
			ApplyFidelity:                applyFidelity,
			BaselineUsageAuthority:       before.UsageAuthority,
			VariantUsageAuthority:        after.UsageAuthority,
			BaselineUsage:                before.Usage,
			VariantUsage:                 after.Usage,
			Usage:                        usage,
			BaselineInputTokens:          before.InputTokens,
			VariantInputTokens:           after.InputTokens,
			InputTokenDelta:              inputTokenDelta,
			BaselineOutputTokens:         before.OutputTokens,
			VariantOutputTokens:          after.OutputTokens,
			OutputTokenDelta:             outputTokenDelta,
			BaselineTotalTokens:          before.TotalTokens,
			VariantTotalTokens:           after.TotalTokens,
			TotalTokenDelta:              totalTokenDelta,
			BaselineVerdict:              before.Verdict, VariantVerdict: after.Verdict,
			Transition: transition, ReasonCode: reason,
			Dimensions: dimensionComparisons,
		})
	}
	return comparisons, nil
}

func buildEvaluationDimensionComparisons(
	baseline EvaluationCaseResult,
	variant EvaluationCaseResult,
) ([]EvaluationDimensionComparison, error) {
	if len(baseline.Dimensions) != len(variant.Dimensions) {
		return nil, fmt.Errorf("baseline and variant dimension scopes differ")
	}
	comparisons := make([]EvaluationDimensionComparison, 0, len(baseline.Dimensions))
	for index, before := range baseline.Dimensions {
		after := variant.Dimensions[index]
		if before.Dimension.ID != after.Dimension.ID {
			return nil, fmt.Errorf("dimension id mismatch at index %d", index)
		}
		durationDelta, ok := checkedExperimentTokenDelta(
			after.CumulativeDurationMS, before.CumulativeDurationMS,
		)
		if !ok {
			return nil, fmt.Errorf("dimension %q cumulative duration delta exceeds int64", before.Dimension.ID)
		}
		usage := MetricAvailability{ReasonCode: "paired_dimension_usage_unavailable"}
		tokenDelta := int64(0)
		if before.Usage.Available && after.Usage.Available {
			tokenDelta, ok = checkedExperimentTokenDelta(after.TotalTokens, before.TotalTokens)
			if !ok {
				return nil, fmt.Errorf("dimension %q token delta exceeds int64", before.Dimension.ID)
			}
			usage = MetricAvailability{Available: true}
		}
		normalization := MetricAvailability{ReasonCode: "paired_dimension_normalization_unavailable"}
		if before.Normalization.Available && after.Normalization.Available {
			normalization = MetricAvailability{Available: true}
		}
		contextCoverage := MetricAvailability{ReasonCode: "paired_dimension_context_coverage_unavailable"}
		if before.ContextCoverage.Available && after.ContextCoverage.Available {
			contextCoverage = MetricAvailability{Available: true}
		}
		transition, reason := compareEvaluationVerdicts(before.Verdict, after.Verdict)
		comparison := EvaluationDimensionComparison{
			DimensionID:       before.Dimension.ID,
			BaselineDimension: before.Dimension, VariantDimension: after.Dimension,
			BaselineCandidateCount:    before.CandidateCount,
			VariantCandidateCount:     after.CandidateCount,
			CandidateDelta:            int64(after.CandidateCount) - int64(before.CandidateCount),
			BaselineRawCandidateCount: before.RawCandidateCount,
			VariantRawCandidateCount:  after.RawCandidateCount,
			RawCandidateDelta: int64(after.RawCandidateCount) -
				int64(before.RawCandidateCount),
			BaselineRetainedCount: before.NormalizationRetainedCount,
			VariantRetainedCount:  after.NormalizationRetainedCount,
			RetainedDelta: int64(after.NormalizationRetainedCount) -
				int64(before.NormalizationRetainedCount),
			BaselineMergedDuplicateCount: before.MergedDuplicateCount,
			VariantMergedDuplicateCount:  after.MergedDuplicateCount,
			MergedDuplicateDelta: int64(after.MergedDuplicateCount) -
				int64(before.MergedDuplicateCount),
			BaselineRejectedInvalidCount: before.RejectedInvalidCount,
			VariantRejectedInvalidCount:  after.RejectedInvalidCount,
			RejectedInvalidDelta: int64(after.RejectedInvalidCount) -
				int64(before.RejectedInvalidCount),
			BaselineExcludedBudgetCount: before.ExcludedBudgetCount,
			VariantExcludedBudgetCount:  after.ExcludedBudgetCount,
			ExcludedBudgetDelta: int64(after.ExcludedBudgetCount) -
				int64(before.ExcludedBudgetCount),
			BaselineContextGapCount: before.ContextGapCount,
			VariantContextGapCount:  after.ContextGapCount,
			ContextGapDelta: int64(after.ContextGapCount) -
				int64(before.ContextGapCount),
			BaselineFindingCount:      before.FindingCount,
			VariantFindingCount:       after.FindingCount,
			FindingDelta:              int64(after.FindingCount) - int64(before.FindingCount),
			BaselineEvaluatedFindings: before.EvaluatedFindingCount,
			VariantEvaluatedFindings:  after.EvaluatedFindingCount,
			EvaluatedFindingDelta: int64(after.EvaluatedFindingCount) -
				int64(before.EvaluatedFindingCount),
			BaselineCumulativeDurationMS: before.CumulativeDurationMS,
			VariantCumulativeDurationMS:  after.CumulativeDurationMS,
			CumulativeDurationDeltaMS:    durationDelta,
			BaselineTotalTokens:          before.TotalTokens,
			VariantTotalTokens:           after.TotalTokens,
			TotalTokenDelta:              tokenDelta,
			BaselineExecutionCoverage:    before.ExecutionCoverage,
			VariantExecutionCoverage:     after.ExecutionCoverage,
			BaselineNormalization:        before.Normalization,
			VariantNormalization:         after.Normalization,
			Normalization:                normalization,
			BaselineContextCoverage:      before.ContextCoverage,
			VariantContextCoverage:       after.ContextCoverage,
			ContextCoverage:              contextCoverage,
			BaselineUsage:                before.Usage,
			VariantUsage:                 after.Usage,
			Usage:                        usage,
			BaselineVerdict:              before.Verdict,
			VariantVerdict:               after.Verdict,
			Transition:                   transition,
			ReasonCode:                   reason,
		}
		if err := comparison.validate(); err != nil {
			return nil, err
		}
		comparisons = append(comparisons, comparison)
	}
	return comparisons, nil
}

func checkedExperimentTokenDelta(after, before uint64) (int64, bool) {
	if after >= before {
		delta := after - before
		if delta > math.MaxInt64 {
			return 0, false
		}
		return int64(delta), true
	}
	delta := before - after
	if delta > math.MaxInt64 {
		return 0, false
	}
	return -int64(delta), true
}

func experimentUsageAvailability(comparisons []ExperimentCaseComparison) MetricAvailability {
	for _, comparison := range comparisons {
		if !comparison.Usage.Available {
			return MetricAvailability{ReasonCode: "paired_usage_unavailable"}
		}
	}
	return MetricAvailability{Available: true}
}

func experimentLatencyAvailability(comparisons []ExperimentCaseComparison) MetricAvailability {
	for _, comparison := range comparisons {
		if comparison.LatencyAuthority != ExperimentLatencyStageAttempts {
			return MetricAvailability{ReasonCode: "postprocessing_has_no_provider_latency"}
		}
	}
	return MetricAvailability{Available: true}
}

func committedRunDurationMS(run runmodel.ReviewRun) (int64, error) {
	if len(run.StageAttempts) == 0 {
		return 0, fmt.Errorf("ReviewRun has no StageAttempt latency evidence")
	}
	var total int64
	for index, attempt := range run.StageAttempts {
		if attempt.StartedAt.IsZero() || attempt.FinishedAt == nil ||
			attempt.FinishedAt.Before(attempt.StartedAt) || attempt.DurationMS < 0 {
			return 0, fmt.Errorf("StageAttempt %d has incomplete terminal timing", index)
		}
		if attempt.DurationMS > math.MaxInt64-total {
			return 0, fmt.Errorf("StageAttempt latency total overflows int64")
		}
		total += attempt.DurationMS
	}
	return total, nil
}

func compareEvaluationVerdicts(before, after EvaluationVerdict) (ExperimentTransition, string) {
	if before == EvaluationInconclusive || after == EvaluationInconclusive {
		return ExperimentInconclusive, "inconclusive_evaluation_present"
	}
	if before == after {
		return ExperimentUnchanged, "quality_verdict_unchanged"
	}
	if before == EvaluationFail && after == EvaluationPass {
		return ExperimentImproved, "quality_verdict_improved"
	}
	return ExperimentRegressed, "quality_verdict_regressed"
}

func summarizeExperiment(comparisons []ExperimentCaseComparison) ExperimentRunSummary {
	copyComparisons := slices.Clone(comparisons)
	sort.Slice(copyComparisons, func(left, right int) bool {
		return copyComparisons[left].CaseID < copyComparisons[right].CaseID
	})
	var summary ExperimentRunSummary
	for _, comparison := range copyComparisons {
		summary.Cases++
		switch comparison.Transition {
		case ExperimentImproved:
			summary.Improved++
		case ExperimentRegressed:
			summary.Regressed++
		case ExperimentUnchanged:
			summary.Unchanged++
		case ExperimentInconclusive:
			summary.Inconclusive++
		}
		if comparison.BaselineVerdict == EvaluationPass {
			summary.PassDelta--
		} else if comparison.BaselineVerdict == EvaluationFail {
			summary.FailDelta--
		}
		if comparison.VariantVerdict == EvaluationPass {
			summary.PassDelta++
		} else if comparison.VariantVerdict == EvaluationFail {
			summary.FailDelta++
		}
	}
	return summary
}

func authorizeExperimentRuns(
	state *projectionState,
	baseline EvaluationRun,
	variant EvaluationRun,
	roles []Role,
) error {
	for _, run := range []EvaluationRun{baseline, variant} {
		for _, result := range run.Results {
			record, exists := state.cases[result.CaseID]
			if !exists {
				return fmt.Errorf("%w: experiment references unknown case", ErrCorrupt)
			}
			if err := authorizeExposure(record.Case, roles); err != nil {
				return err
			}
		}
	}
	return nil
}

func experimentIdempotentResult(
	existing storedEvent,
	request ExperimentRunRequest,
	mutation Mutation,
) (ExperimentRun, error) {
	if existing.stream != experimentRunStream {
		return ExperimentRun{}, fmt.Errorf("%w: idempotency key %q is already used in %s",
			ErrConflict, mutation.IdempotencyKey, existing.stream)
	}
	var event experimentRunEvent
	if err := decodeEventStrict(existing.envelope.Payload, &event); err != nil {
		return ExperimentRun{}, fmt.Errorf("%w: decode idempotent experiment: %v", ErrCorrupt, err)
	}
	if !reflect.DeepEqual(event.Request, request) || event.Actor != mutation.Actor ||
		!slices.Equal(event.Roles, mutation.Roles) || event.Audit != mutation.Audit ||
		!event.OccurredAt.Equal(mutation.At.UTC()) {
		return ExperimentRun{}, fmt.Errorf("%w: idempotency key %q has different input",
			ErrConflict, mutation.IdempotencyKey)
	}
	return cloneValue(event.Run), nil
}

func (repository *Repository) GetExperimentRun(id string, access Access) (ExperimentRun, error) {
	if err := validateID("experiment_run_id", id); err != nil {
		return ExperimentRun{}, err
	}
	if err := access.Validate(); err != nil {
		return ExperimentRun{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExperimentRun{}, err
	}
	run, err := state.experimentRun(id)
	if err != nil {
		return ExperimentRun{}, err
	}
	baseline := state.evaluationRuns[run.BaselineEvaluationRunID]
	variant := state.evaluationRuns[run.VariantEvaluationRunID]
	if err := authorizeExperimentRuns(state, baseline, variant, access.Roles); err != nil {
		return ExperimentRun{}, err
	}
	return run, nil
}

func (repository *Repository) ListExperimentRuns(access Access) ([]ExperimentRun, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.experimentRuns))
	for id, run := range state.experimentRuns {
		baseline := state.evaluationRuns[run.BaselineEvaluationRunID]
		variant := state.evaluationRuns[run.VariantEvaluationRunID]
		if authorizeExperimentRuns(state, baseline, variant, access.Roles) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]ExperimentRun, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneValue(state.experimentRuns[id]))
	}
	return result, nil
}
