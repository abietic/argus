package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/targetmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	EvaluationRunRequestSchemaVersion = "argus.evaluation_run_request.v1alpha1"
	EvaluationRunSchemaVersion        = "argus.evaluation_run.v1alpha1"
	evaluationRunEventSchemaVersion   = "argus.evaluation_run_event.v1alpha1"
	evaluationRunStream               = "evaluation/runs"
	EvaluationUsageAuthority          = "worker_self_report_diagnostic"
)

type EvaluationVerdict string

const (
	EvaluationPass         EvaluationVerdict = "pass"
	EvaluationFail         EvaluationVerdict = "fail"
	EvaluationInconclusive EvaluationVerdict = "inconclusive"
)

type EvaluationCaseRunBinding struct {
	CaseID                string `json:"case_id"`
	ExpectedLabelRevision uint64 `json:"expected_label_revision"`
	ReviewRunID           string `json:"review_run_id"`
	// DimensionScope is an evaluator-owned, exact-version applicability slice.
	// An empty slice preserves aggregate evaluation semantics. When populated,
	// every entry is evaluated independently from committed formal evidence.
	DimensionScope []contractsv1alpha1.VersionedRef `json:"dimension_scope,omitempty"`
	ApplyTrial     *ApplyTrial                      `json:"apply_trial,omitempty"`
}

// EvaluationRunRequest binds governed dataset labels to already committed
// formal ReviewRuns. Execution orchestration remains separate: recording an
// evaluation cannot start a model call or any remote side effect.
type EvaluationRunRequest struct {
	SchemaVersion     string                     `json:"schema_version"`
	EvaluationRunID   string                     `json:"evaluation_run_id"`
	EvaluatorRevision string                     `json:"evaluator_revision"`
	Bindings          []EvaluationCaseRunBinding `json:"bindings"`
	CreatedAt         time.Time                  `json:"created_at"`
}

type MetricAvailability struct {
	Available  bool   `json:"available"`
	ReasonCode string `json:"reason_code,omitempty"`
}

// EvaluationDimensionResult attributes only evidence that can be joined to
// one exact review dimension. Shared context work is intentionally excluded
// from its usage counters; verification work is joined through the governed
// hypothesis occurrence rather than the verifier's own dimension identity.
type EvaluationDimensionResult struct {
	Dimension                  contractsv1alpha1.VersionedRef                `json:"dimension"`
	CandidateCount             uint32                                        `json:"candidate_count"`
	RawCandidateCount          uint32                                        `json:"raw_candidate_count"`
	NormalizationRetainedCount uint32                                        `json:"normalization_retained_count"`
	MergedDuplicateCount       uint32                                        `json:"merged_duplicate_count"`
	RejectedInvalidCount       uint32                                        `json:"rejected_invalid_count"`
	ExcludedBudgetCount        uint32                                        `json:"excluded_budget_count"`
	ContextGapCount            uint32                                        `json:"context_gap_count"`
	ConfirmedCount             uint32                                        `json:"confirmed_count"`
	RejectedCount              uint32                                        `json:"rejected_count"`
	InconclusiveCount          uint32                                        `json:"inconclusive_count"`
	FindingCount               uint32                                        `json:"finding_count"`
	EvaluatedFindingCount      uint32                                        `json:"evaluated_finding_count"`
	ExpectedAnchorCount        uint32                                        `json:"expected_anchor_count"`
	MatchedAnchorCount         uint32                                        `json:"matched_anchor_count"`
	LocalizedFindingCount      uint32                                        `json:"localized_finding_count"`
	ReviewTaskCount            uint32                                        `json:"review_task_count"`
	VerificationTaskCount      uint32                                        `json:"verification_task_count"`
	TasksSucceeded             uint32                                        `json:"tasks_succeeded"`
	TasksFailed                uint32                                        `json:"tasks_failed"`
	TasksCanceled              uint32                                        `json:"tasks_canceled"`
	ModelTurnsStarted          uint64                                        `json:"model_turns_started"`
	ModelTurnsCompleted        uint64                                        `json:"model_turns_completed"`
	ToolCalls                  uint64                                        `json:"tool_calls"`
	CumulativeDurationMS       uint64                                        `json:"cumulative_duration_ms"`
	UsageCompleteness          contractsv1alpha1.AgentTokenUsageCompleteness `json:"usage_completeness"`
	UsageReceiptCount          uint64                                        `json:"usage_receipt_count"`
	UsageReportedReceipts      uint64                                        `json:"usage_reported_receipts"`
	UsagePartialReceipts       uint64                                        `json:"usage_partial_receipts"`
	UsageUnavailableReceipts   uint64                                        `json:"usage_unavailable_receipts"`
	InputTokens                uint64                                        `json:"input_tokens"`
	OutputTokens               uint64                                        `json:"output_tokens"`
	CacheReadTokens            uint64                                        `json:"cache_read_tokens"`
	CacheWriteTokens           uint64                                        `json:"cache_write_tokens"`
	ReasoningTokens            uint64                                        `json:"reasoning_tokens"`
	ReasoningReportedReceipts  uint64                                        `json:"reasoning_reported_receipts"`
	TotalTokens                uint64                                        `json:"total_tokens"`
	UsageAuthority             string                                        `json:"usage_authority"`
	Verdict                    EvaluationVerdict                             `json:"verdict"`
	ReasonCode                 string                                        `json:"reason_code"`
	ExecutionCoverage          MetricAvailability                            `json:"execution_coverage"`
	ContextCoverage            MetricAvailability                            `json:"context_coverage"`
	Localization               MetricAvailability                            `json:"localization"`
	Normalization              MetricAvailability                            `json:"normalization"`
	Usage                      MetricAvailability                            `json:"usage"`
}

type EvaluationCaseResult struct {
	CaseID                    string                                        `json:"case_id"`
	CaseType                  CaseType                                      `json:"case_type"`
	LabelRevision             uint64                                        `json:"label_revision"`
	LabelPolicyRevision       string                                        `json:"label_policy_revision"`
	ExpectedOutcome           ExpectedOutcome                               `json:"expected_outcome"`
	ExpectedCategory          string                                        `json:"expected_category,omitempty"`
	ReviewRunID               string                                        `json:"review_run_id"`
	ReviewRunRef              runmodel.ArtifactRef                          `json:"review_run_ref"`
	InputSnapshotRef          runmodel.ArtifactRef                          `json:"input_snapshot_ref"`
	GovernedReportRef         runmodel.ArtifactRef                          `json:"governed_report_ref"`
	VerificationLedgerRef     runmodel.ArtifactRef                          `json:"verification_ledger_ref"`
	CalibrationLedgerRef      runmodel.ArtifactRef                          `json:"calibration_ledger_ref"`
	SuppressionLedgerRef      runmodel.ArtifactRef                          `json:"suppression_ledger_ref"`
	HypothesisSetRef          *runmodel.ArtifactRef                         `json:"hypothesis_set_ref,omitempty"`
	RawCandidateCollectionRef *runmodel.ArtifactRef                         `json:"raw_candidate_collection_ref,omitempty"`
	ReportCompleteness        string                                        `json:"report_completeness"`
	TotalFindingCount         uint32                                        `json:"total_finding_count"`
	EvaluatedFindingCount     uint32                                        `json:"evaluated_finding_count"`
	ExpectedAnchorCount       uint32                                        `json:"expected_anchor_count"`
	MatchedAnchorCount        uint32                                        `json:"matched_anchor_count"`
	LocalizedFindingCount     uint32                                        `json:"localized_finding_count"`
	ExpectedSuppressionCount  uint32                                        `json:"expected_suppression_count"`
	ObservedSuppressionCount  uint32                                        `json:"observed_suppression_count"`
	SuppressedTargetCount     uint32                                        `json:"suppressed_target_count"`
	EscapedTargetCount        uint32                                        `json:"escaped_target_count"`
	InconclusiveTargetCount   uint32                                        `json:"inconclusive_target_count"`
	ApplyTrialID              string                                        `json:"apply_trial_id,omitempty"`
	ApplyEditScriptRef        *runmodel.ArtifactRef                         `json:"apply_edit_script_ref,omitempty"`
	ApplyChecksExecuted       uint32                                        `json:"apply_checks_executed"`
	ApplyChecksPassed         uint32                                        `json:"apply_checks_passed"`
	ApplyChecksFailed         uint32                                        `json:"apply_checks_failed"`
	ApplyAuthority            string                                        `json:"apply_authority"`
	UsageCompleteness         contractsv1alpha1.AgentTokenUsageCompleteness `json:"usage_completeness"`
	UsageReceiptCount         uint64                                        `json:"usage_receipt_count"`
	UsageReportedReceipts     uint64                                        `json:"usage_reported_receipts"`
	UsagePartialReceipts      uint64                                        `json:"usage_partial_receipts"`
	UsageUnavailableReceipts  uint64                                        `json:"usage_unavailable_receipts"`
	InputTokens               uint64                                        `json:"input_tokens"`
	OutputTokens              uint64                                        `json:"output_tokens"`
	CacheReadTokens           uint64                                        `json:"cache_read_tokens"`
	CacheWriteTokens          uint64                                        `json:"cache_write_tokens"`
	ReasoningTokens           uint64                                        `json:"reasoning_tokens"`
	ReasoningReportedReceipts uint64                                        `json:"reasoning_reported_receipts"`
	TotalTokens               uint64                                        `json:"total_tokens"`
	UsageAuthority            string                                        `json:"usage_authority"`
	Dimensions                []EvaluationDimensionResult                   `json:"dimensions"`
	Verdict                   EvaluationVerdict                             `json:"verdict"`
	ReasonCode                string                                        `json:"reason_code"`
	DefectPresence            MetricAvailability                            `json:"defect_presence"`
	Localization              MetricAvailability                            `json:"localization"`
	FilterEfficacy            MetricAvailability                            `json:"filter_efficacy"`
	ApplyFidelity             MetricAvailability                            `json:"apply_fidelity"`
	Usage                     MetricAvailability                            `json:"usage"`
}

type EvaluationRunSummary struct {
	Cases          uint32 `json:"cases"`
	Passed         uint32 `json:"passed"`
	Failed         uint32 `json:"failed"`
	Inconclusive   uint32 `json:"inconclusive"`
	PositiveCases  uint32 `json:"positive_cases"`
	PositivePassed uint32 `json:"positive_passed"`
	NegativeCases  uint32 `json:"negative_cases"`
	NegativePassed uint32 `json:"negative_passed"`
}

type EvaluationRun struct {
	SchemaVersion     string                 `json:"schema_version"`
	EvaluationRunID   string                 `json:"evaluation_run_id"`
	EvaluatorRevision string                 `json:"evaluator_revision"`
	Results           []EvaluationCaseResult `json:"results"`
	Summary           EvaluationRunSummary   `json:"summary"`
	CreatedAt         time.Time              `json:"created_at"`
	RecordedAt        time.Time              `json:"recorded_at"`
	RecordedBy        string                 `json:"recorded_by"`
}

type EvaluationRunReader interface {
	LoadRun(runID string) (runmodel.ReviewRun, error)
	ExecutionSnapshotForRun(runID string) (runmodel.ExecutionSnapshot, error)
	ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error)
	ReadJSONArtifact(ref runmodel.ArtifactRef, out any) error
	CommittedRunRef(runID string) (runmodel.ArtifactRef, error)
	CheckArtifactEligibility(runmodel.ArtifactRef, runrepo.ArtifactUse) error
}

func (request EvaluationRunRequest) Validate() error {
	if request.SchemaVersion != EvaluationRunRequestSchemaVersion {
		return fmt.Errorf("unsupported evaluation run request schema %q", request.SchemaVersion)
	}
	if err := validateID("evaluation_run_id", request.EvaluationRunID); err != nil {
		return err
	}
	if err := validateID("evaluator_revision", request.EvaluatorRevision); err != nil {
		return err
	}
	if request.Bindings == nil || len(request.Bindings) == 0 {
		return fmt.Errorf("bindings must be a non-empty array")
	}
	previous := ""
	for index, binding := range request.Bindings {
		if err := validateID("binding.case_id", binding.CaseID); err != nil {
			return fmt.Errorf("bindings[%d]: %w", index, err)
		}
		if binding.ExpectedLabelRevision == 0 {
			return fmt.Errorf("bindings[%d].expected_label_revision must be positive", index)
		}
		if err := validateID("binding.review_run_id", binding.ReviewRunID); err != nil {
			return fmt.Errorf("bindings[%d]: %w", index, err)
		}
		if binding.ApplyTrial != nil {
			if err := binding.ApplyTrial.Validate(); err != nil {
				return fmt.Errorf("bindings[%d].apply_trial: %w", index, err)
			}
			if binding.ApplyTrial.CaseID != binding.CaseID ||
				binding.ApplyTrial.LabelRevision != binding.ExpectedLabelRevision ||
				binding.ApplyTrial.ReviewRunID != binding.ReviewRunID {
				return fmt.Errorf("bindings[%d].apply_trial does not bind the case, label, and ReviewRun", index)
			}
			if binding.ApplyTrial.ExecutedAt.After(request.CreatedAt) {
				return fmt.Errorf("bindings[%d].apply_trial executed_at is after request creation", index)
			}
		}
		if err := validateEvaluationDimensionScope(binding.DimensionScope); err != nil {
			return fmt.Errorf("bindings[%d].dimension_scope: %w", index, err)
		}
		if index > 0 && binding.CaseID <= previous {
			return fmt.Errorf("bindings must be uniquely sorted by case_id")
		}
		previous = binding.CaseID
	}
	if request.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func (run EvaluationRun) Validate() error {
	if run.SchemaVersion != EvaluationRunSchemaVersion {
		return fmt.Errorf("unsupported evaluation run schema %q", run.SchemaVersion)
	}
	if err := validateID("evaluation_run_id", run.EvaluationRunID); err != nil {
		return err
	}
	if err := validateID("evaluator_revision", run.EvaluatorRevision); err != nil {
		return err
	}
	if run.Results == nil || len(run.Results) == 0 {
		return fmt.Errorf("results must be a non-empty array")
	}
	if run.CreatedAt.IsZero() || run.RecordedAt.IsZero() || run.RecordedAt.Before(run.CreatedAt) {
		return fmt.Errorf("created_at and non-decreasing recorded_at are required")
	}
	if err := validateText("recorded_by", run.RecordedBy, 256, false); err != nil {
		return err
	}
	var summary EvaluationRunSummary
	previous := ""
	for index, result := range run.Results {
		if err := result.validate(); err != nil {
			return fmt.Errorf("results[%d]: %w", index, err)
		}
		if index > 0 && result.CaseID <= previous {
			return fmt.Errorf("results must be uniquely sorted by case_id")
		}
		previous = result.CaseID
		summary.Cases++
		positive := isPositiveOutcome(result.ExpectedOutcome)
		negative := isNegativeOutcome(result.ExpectedOutcome)
		if positive {
			summary.PositiveCases++
		} else if negative {
			summary.NegativeCases++
		}
		switch result.Verdict {
		case EvaluationPass:
			summary.Passed++
			if positive {
				summary.PositivePassed++
			} else if negative {
				summary.NegativePassed++
			}
		case EvaluationFail:
			summary.Failed++
		case EvaluationInconclusive:
			summary.Inconclusive++
		default:
			return fmt.Errorf("results[%d] has unsupported verdict %q", index, result.Verdict)
		}
	}
	if run.Summary != summary {
		return fmt.Errorf("summary is not the exact result recomputation")
	}
	return nil
}

func (result EvaluationCaseResult) validate() error {
	if err := validateID("case_id", result.CaseID); err != nil {
		return err
	}
	if err := result.CaseType.Validate(); err != nil {
		return err
	}
	if result.LabelRevision == 0 {
		return fmt.Errorf("label_revision must be positive")
	}
	if err := validateID("label_policy_revision", result.LabelPolicyRevision); err != nil {
		return err
	}
	if err := result.ExpectedOutcome.Validate(); err != nil {
		return err
	}
	if !caseTypeAllowsOutcome(result.CaseType, result.ExpectedOutcome) {
		return fmt.Errorf("expected outcome %q is invalid for case type %q",
			result.ExpectedOutcome, result.CaseType)
	}
	if result.ExpectedCategory != "" {
		if err := validateID("expected_category", result.ExpectedCategory); err != nil {
			return err
		}
	}
	if err := validateID("review_run_id", result.ReviewRunID); err != nil {
		return err
	}
	for name, ref := range map[string]runmodel.ArtifactRef{
		"review_run_ref": result.ReviewRunRef, "input_snapshot_ref": result.InputSnapshotRef,
		"governed_report_ref":     result.GovernedReportRef,
		"verification_ledger_ref": result.VerificationLedgerRef,
		"calibration_ledger_ref":  result.CalibrationLedgerRef,
		"suppression_ledger_ref":  result.SuppressionLedgerRef,
	} {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if result.ReviewRunRef.Contract != runmodel.ContractReviewRun ||
		result.InputSnapshotRef.Contract != runmodel.ContractMaterializedTarget ||
		result.GovernedReportRef.Contract != runmodel.ContractGovernedReviewReport ||
		result.VerificationLedgerRef.Contract != runmodel.ContractCandidateVerificationLedger {
		return fmt.Errorf("result artifact contracts do not match evaluation inputs")
	}
	if result.CalibrationLedgerRef.Contract != runmodel.ContractFindingCalibrationLedger ||
		result.SuppressionLedgerRef.Contract != runmodel.ContractFindingSuppressionLedger {
		return fmt.Errorf("result governance ledger contracts do not match evaluation inputs")
	}
	for name, binding := range map[string]struct {
		ref      *runmodel.ArtifactRef
		contract string
	}{
		"hypothesis_set_ref": {
			ref: result.HypothesisSetRef, contract: runmodel.ContractReviewHypothesisSet,
		},
		"raw_candidate_collection_ref": {
			ref: result.RawCandidateCollectionRef, contract: runmodel.ContractAgentReviewRawCandidates,
		},
	} {
		if binding.ref == nil {
			continue
		}
		if err := binding.ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if binding.ref.Contract != binding.contract {
			return fmt.Errorf("%s has contract %q, want %q", name, binding.ref.Contract, binding.contract)
		}
	}
	if result.RawCandidateCollectionRef != nil && result.HypothesisSetRef == nil {
		return fmt.Errorf("raw_candidate_collection_ref requires hypothesis_set_ref")
	}
	if result.ReportCompleteness != string(contractsv1alpha1.AgentReviewComplete) &&
		result.ReportCompleteness != string(contractsv1alpha1.AgentReviewPartial) {
		return fmt.Errorf("unsupported report_completeness %q", result.ReportCompleteness)
	}
	if result.EvaluatedFindingCount > result.TotalFindingCount {
		return fmt.Errorf("evaluated_finding_count exceeds total_finding_count")
	}
	if result.CaseType == CaseFixValidation || result.CaseType == CaseWorkflowInvariant {
		if result.DefectPresence.Available || result.DefectPresence.ReasonCode != "metric_not_applicable" {
			return fmt.Errorf("defect_presence must be unavailable when not applicable")
		}
	} else if !result.DefectPresence.Available || result.DefectPresence.ReasonCode != "" {
		return fmt.Errorf("defect_presence must be available without a reason")
	}
	if result.MatchedAnchorCount > result.ExpectedAnchorCount ||
		result.LocalizedFindingCount > result.EvaluatedFindingCount {
		return fmt.Errorf("localization counts exceed their evidence populations")
	}
	if result.ObservedSuppressionCount > result.ExpectedSuppressionCount ||
		result.SuppressedTargetCount+result.EscapedTargetCount+
			result.InconclusiveTargetCount != result.ObservedSuppressionCount {
		return fmt.Errorf("filter efficacy counts do not bind their evidence population")
	}
	if result.ExpectedSuppressionCount == 0 {
		if result.FilterEfficacy.Available ||
			result.FilterEfficacy.ReasonCode != "structured_suppression_truth_unavailable" ||
			result.ObservedSuppressionCount != 0 {
			return fmt.Errorf("filter efficacy must be unavailable without structured suppression truth")
		}
	} else if result.ReportCompleteness != string(contractsv1alpha1.AgentReviewComplete) {
		if result.FilterEfficacy.Available ||
			result.FilterEfficacy.ReasonCode != "review_coverage_partial" ||
			result.ObservedSuppressionCount != 0 {
			return fmt.Errorf("filter efficacy must be unavailable for partial review coverage")
		}
	} else if result.ObservedSuppressionCount == result.ExpectedSuppressionCount {
		if !result.FilterEfficacy.Available || result.FilterEfficacy.ReasonCode != "" {
			return fmt.Errorf("filter efficacy must be available when every suppression target is observed")
		}
	} else if result.FilterEfficacy.Available ||
		result.FilterEfficacy.ReasonCode != "suppression_target_not_observed" {
		return fmt.Errorf("filter efficacy must be unavailable when a suppression target is not observed")
	}
	if result.ExpectedAnchorCount > 0 &&
		result.ReportCompleteness == string(contractsv1alpha1.AgentReviewComplete) {
		if !result.Localization.Available || result.Localization.ReasonCode != "" {
			return fmt.Errorf("localization must be available for complete structured-anchor evaluation")
		}
	} else {
		if result.Localization.Available || result.Localization.ReasonCode == "" ||
			result.MatchedAnchorCount != 0 || result.LocalizedFindingCount != 0 {
			return fmt.Errorf("localization must be unavailable without complete structured-anchor evidence")
		}
		if err := validateID("localization.reason_code", result.Localization.ReasonCode); err != nil {
			return err
		}
	}
	if result.ApplyChecksPassed+result.ApplyChecksFailed != result.ApplyChecksExecuted {
		return fmt.Errorf("apply check counts do not bind executed checks")
	}
	if result.UsageReportedReceipts+result.UsagePartialReceipts+
		result.UsageUnavailableReceipts != result.UsageReceiptCount {
		return fmt.Errorf("usage receipt counts do not bind their evidence population")
	}
	tokenTotal, ok := checkedEvaluationTokenSum(
		result.InputTokens,
		result.OutputTokens,
		result.CacheReadTokens,
		result.CacheWriteTokens,
	)
	if !ok || tokenTotal != result.TotalTokens || result.ReasoningTokens > result.OutputTokens ||
		result.ReasoningReportedReceipts > result.UsageReportedReceipts+result.UsagePartialReceipts {
		return fmt.Errorf("usage counters are internally inconsistent")
	}
	if result.UsageAuthority == "unavailable" {
		if result.UsageCompleteness != contractsv1alpha1.AgentTokenUsageUnavailable ||
			result.UsageReceiptCount != 0 || result.TotalTokens != 0 || result.Usage.Available ||
			result.Usage.ReasonCode != "usage_receipt_artifact_unavailable" {
			return fmt.Errorf("missing usage artifact must remain explicitly unavailable")
		}
	} else if result.UsageAuthority != EvaluationUsageAuthority {
		return fmt.Errorf("unsupported usage authority %q", result.UsageAuthority)
	} else if result.UsageReceiptCount > 0 &&
		result.UsageReportedReceipts == result.UsageReceiptCount {
		if result.UsageCompleteness != contractsv1alpha1.AgentTokenUsageProviderReported ||
			!result.Usage.Available || result.Usage.ReasonCode != "" {
			return fmt.Errorf("complete provider-reported receipt usage must be available")
		}
	} else {
		wantCompleteness := contractsv1alpha1.AgentTokenUsageUnavailable
		if result.UsageReportedReceipts > 0 || result.UsagePartialReceipts > 0 {
			wantCompleteness = contractsv1alpha1.AgentTokenUsagePartial
		}
		if result.UsageCompleteness != wantCompleteness || result.Usage.Available ||
			result.Usage.ReasonCode != "usage_receipts_incomplete" {
			return fmt.Errorf("incomplete receipt usage must remain unavailable")
		}
	}
	if result.Dimensions == nil {
		return fmt.Errorf("dimensions must be an explicit array")
	}
	previousDimension := ""
	for index, dimension := range result.Dimensions {
		if err := dimension.validate(result); err != nil {
			return fmt.Errorf("dimensions[%d]: %w", index, err)
		}
		if index > 0 && dimension.Dimension.ID <= previousDimension {
			return fmt.Errorf("dimensions must be uniquely sorted by dimension.id")
		}
		previousDimension = dimension.Dimension.ID
	}
	if result.CaseType != CaseFixValidation {
		if result.ApplyTrialID != "" || result.ApplyEditScriptRef != nil ||
			result.ApplyChecksExecuted != 0 || result.ApplyAuthority != "unavailable" ||
			result.ApplyFidelity.Available || result.ApplyFidelity.ReasonCode != "metric_not_applicable" {
			return fmt.Errorf("apply fidelity facts must be absent when not applicable")
		}
	} else {
		if err := validateID("apply_trial_id", result.ApplyTrialID); err != nil {
			return err
		}
		if result.ApplyEditScriptRef == nil || result.ApplyEditScriptRef.Contract != ApplyEditScriptContract {
			return fmt.Errorf("fix validation requires an edit script ref")
		}
		if err := result.ApplyEditScriptRef.Validate(); err != nil {
			return err
		}
		if result.ApplyAuthority != ApplyTrialAuthority {
			return fmt.Errorf("fix validation has unsupported apply authority")
		}
		if result.ApplyFidelity.Available {
			if result.ApplyFidelity.ReasonCode != "" || result.ApplyChecksExecuted == 0 ||
				(result.ApplyChecksFailed == 0 &&
					(result.ApplyChecksExecuted != 3 || result.ApplyChecksPassed != 3)) {
				return fmt.Errorf("available apply fidelity requires conclusive checks")
			}
		} else if result.ApplyFidelity.ReasonCode != "apply_evidence_partial" ||
			result.ApplyChecksFailed != 0 || result.ApplyChecksExecuted >= 3 {
			return fmt.Errorf("unavailable fix apply fidelity requires unfinished passing evidence")
		}
	}
	if err := validateID("reason_code", result.ReasonCode); err != nil {
		return err
	}
	wantVerdict, wantReason := scoreEvaluationResult(result)
	if result.Verdict != wantVerdict || result.ReasonCode != wantReason {
		return fmt.Errorf("verdict and reason_code are not the primary metric recomputation")
	}
	return nil
}

func DecodeEvaluationRunRequest(data []byte) (EvaluationRunRequest, error) {
	return decodeStrict(data, "EvaluationRunRequest", func(value EvaluationRunRequest) error {
		return value.Validate()
	})
}

func DecodeEvaluationRun(data []byte) (EvaluationRun, error) {
	return decodeStrict(data, "EvaluationRun", func(value EvaluationRun) error {
		return value.Validate()
	})
}

// RecordEvaluationRun performs a read-only, deterministic evaluation over
// committed formal outputs and then appends one immutable result event.
func (repository *Repository) RecordEvaluationRun(
	ctx context.Context,
	request EvaluationRunRequest,
	mutation Mutation,
	runs EvaluationRunReader,
) (EvaluationRun, error) {
	if err := checkContext(ctx); err != nil {
		return EvaluationRun{}, err
	}
	if err := request.Validate(); err != nil {
		return EvaluationRun{}, fmt.Errorf("validate evaluation run request: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return EvaluationRun{}, err
	}
	if runs == nil {
		return EvaluationRun{}, fmt.Errorf("review run repository is required")
	}
	if request.CreatedAt.After(mutation.At.UTC()) {
		return EvaluationRun{}, fmt.Errorf("evaluation run created_at must not be after mutation time")
	}

	repository.mu.Lock()
	state, err := repository.load()
	if err != nil {
		repository.mu.Unlock()
		return EvaluationRun{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		run, retryErr := evaluationRunIdempotentResult(existing, request, mutation)
		repository.mu.Unlock()
		return run, retryErr
	}
	caseRecords := make([]CaseRecord, len(request.Bindings))
	for index, binding := range request.Bindings {
		record, exists := state.cases[binding.CaseID]
		if !exists {
			repository.mu.Unlock()
			return EvaluationRun{}, fmt.Errorf("%w: case %q", ErrNotFound, binding.CaseID)
		}
		if err := authorizeExposure(record.CurrentCase(), mutation.Roles); err != nil {
			repository.mu.Unlock()
			return EvaluationRun{}, err
		}
		if err := validateEvaluationCaseAdmission(state, record, binding, request); err != nil {
			repository.mu.Unlock()
			return EvaluationRun{}, err
		}
		caseRecords[index] = cloneValue(record)
	}
	repository.mu.Unlock()

	results := make([]EvaluationCaseResult, 0, len(request.Bindings))
	for index, binding := range request.Bindings {
		if err := ctx.Err(); err != nil {
			return EvaluationRun{}, err
		}
		result, err := evaluateCommittedRun(runs, caseRecords[index], binding)
		if err != nil {
			return EvaluationRun{}, fmt.Errorf("evaluate case %q: %w", binding.CaseID, err)
		}
		results = append(results, result)
	}
	run := EvaluationRun{
		SchemaVersion: EvaluationRunSchemaVersion, EvaluationRunID: request.EvaluationRunID,
		EvaluatorRevision: request.EvaluatorRevision, Results: results,
		CreatedAt: request.CreatedAt.UTC(), RecordedAt: mutation.At.UTC(), RecordedBy: mutation.Actor,
	}
	run.Summary = summarizeEvaluationResults(results)
	if err := run.Validate(); err != nil {
		return EvaluationRun{}, fmt.Errorf("validate computed evaluation run: %w", err)
	}
	event := evaluationRunEvent{
		SchemaVersion: evaluationRunEventSchemaVersion, Request: request, Run: run,
		Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit,
		OccurredAt: mutation.At.UTC(),
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err = repository.load()
	if err != nil {
		return EvaluationRun{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		return evaluationRunIdempotentResult(existing, request, mutation)
	}
	for index, binding := range request.Bindings {
		current, exists := state.cases[binding.CaseID]
		if !exists || !reflect.DeepEqual(current, caseRecords[index]) {
			return EvaluationRun{}, fmt.Errorf("%w: case %q changed during evaluation",
				ErrInvalidTransition, binding.CaseID)
		}
		if err := validateEvaluationCaseAdmission(state, current, binding, request); err != nil {
			return EvaluationRun{}, err
		}
	}
	if _, exists := state.evaluationRuns[request.EvaluationRunID]; exists {
		return EvaluationRun{}, fmt.Errorf("%w: evaluation_run_id %q already exists",
			ErrConflict, request.EvaluationRunID)
	}
	if _, err := repository.appendEvaluationRun(mutation, event); err != nil {
		return EvaluationRun{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return EvaluationRun{}, err
	}
	return reloaded.evaluationRun(request.EvaluationRunID)
}

func evaluationRunIdempotentResult(
	existing storedEvent,
	request EvaluationRunRequest,
	mutation Mutation,
) (EvaluationRun, error) {
	if existing.stream != evaluationRunStream {
		return EvaluationRun{}, fmt.Errorf("%w: idempotency key %q is already used in %s",
			ErrConflict, mutation.IdempotencyKey, existing.stream)
	}
	var event evaluationRunEvent
	if err := decodeEventStrict(existing.envelope.Payload, &event); err != nil {
		return EvaluationRun{}, fmt.Errorf("%w: decode idempotent evaluation run: %v", ErrCorrupt, err)
	}
	if !reflect.DeepEqual(event.Request, request) || event.Actor != mutation.Actor ||
		!slices.Equal(event.Roles, mutation.Roles) || event.Audit != mutation.Audit ||
		!event.OccurredAt.Equal(mutation.At.UTC()) {
		return EvaluationRun{}, fmt.Errorf("%w: idempotency key %q has different input",
			ErrConflict, mutation.IdempotencyKey)
	}
	return cloneValue(event.Run), nil
}

func validateEvaluationCaseAdmission(
	state *projectionState,
	record CaseRecord,
	binding EvaluationCaseRunBinding,
	request EvaluationRunRequest,
) error {
	if record.CurrentLabelRevision != binding.ExpectedLabelRevision {
		return fmt.Errorf("%w: case %q label revision is %d, expected %d", ErrInvalidTransition,
			binding.CaseID, record.CurrentLabelRevision, binding.ExpectedLabelRevision)
	}
	if record.Case.Type == CaseFixValidation && binding.ApplyTrial == nil {
		return fmt.Errorf("%w: fix_validation case %q requires an apply trial",
			ErrInvalidTransition, binding.CaseID)
	}
	if record.Case.Type != CaseFixValidation && binding.ApplyTrial != nil {
		return fmt.Errorf("%w: apply trial is only valid for fix_validation case %q",
			ErrInvalidTransition, binding.CaseID)
	}
	if record.CurrentGovernance.ReviewState != ReviewApproved ||
		record.CurrentGovernance.DatasetState != DatasetActive ||
		!record.CurrentGovernance.Eligibility.Evaluation ||
		!slices.Contains(record.CurrentGovernance.LicenseConsent.AllowedUses, UseEvaluation) {
		return fmt.Errorf("%w: case %q is not active and eligible for evaluation",
			ErrInvalidTransition, binding.CaseID)
	}
	exposure, exists := state.exposures[exposureKey{RunID: request.EvaluationRunID, CaseID: binding.CaseID}]
	if !exists {
		return fmt.Errorf("%w: case %q has no exposure evidence for evaluation run %q",
			ErrInvalidTransition, binding.CaseID, request.EvaluationRunID)
	}
	if exposure.Exposure.ObservedAt.After(request.CreatedAt) {
		return fmt.Errorf("%w: case %q exposure was recorded after evaluation run creation",
			ErrInvalidTransition, binding.CaseID)
	}
	if record.CurrentGovernance.Split == SplitHoldout {
		for _, observation := range exposure.Exposure.Observations {
			if observation.Status != ExposureNotSeen {
				return fmt.Errorf("%w: holdout case %q was seen by %s revision %q",
					ErrContaminated, binding.CaseID, observation.Component, observation.Revision)
			}
		}
	}
	return nil
}

func evaluateCommittedRun(
	runs EvaluationRunReader,
	caseRecord CaseRecord,
	binding EvaluationCaseRunBinding,
) (EvaluationCaseResult, error) {
	run, err := runs.LoadRun(binding.ReviewRunID)
	if err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("load committed ReviewRun: %w", err)
	}
	if run.Status != runmodel.RunStatusSucceeded || run.GovernedReportRef == nil ||
		run.VerificationLedgerRef == nil || run.CalibrationLedgerRef == nil ||
		run.SuppressionLedgerRef == nil {
		return EvaluationCaseResult{}, fmt.Errorf("review run must be succeeded with complete governed ledgers")
	}
	snapshot, err := runs.ExecutionSnapshotForRun(run.RunID)
	if err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("load execution snapshot: %w", err)
	}
	if snapshot.RemoteWrites != "deny" || snapshot.ToolPolicy.RemoteWrites != "deny" {
		return EvaluationCaseResult{}, fmt.Errorf("evaluation input run did not deny remote writes")
	}
	if err := validateEvaluationTargetAgainstCase(
		runs,
		run,
		snapshot,
		caseRecord.Case.InputSnapshotRef,
	); err != nil {
		return EvaluationCaseResult{}, err
	}
	inputs := []struct {
		name string
		ref  runmodel.ArtifactRef
	}{
		{name: "target snapshot", ref: run.TargetSnapshotRef},
		{name: "governed report", ref: *run.GovernedReportRef},
		{name: "candidate verification ledger", ref: *run.VerificationLedgerRef},
		{name: "finding calibration ledger", ref: *run.CalibrationLedgerRef},
		{name: "finding suppression ledger", ref: *run.SuppressionLedgerRef},
	}
	if run.RawCandidateCollectionRef != nil {
		if run.HypothesisSetRef == nil {
			return EvaluationCaseResult{}, fmt.Errorf("raw candidate evidence requires a hypothesis set")
		}
		inputs = append(inputs,
			struct {
				name string
				ref  runmodel.ArtifactRef
			}{name: "hypothesis set", ref: *run.HypothesisSetRef},
			struct {
				name string
				ref  runmodel.ArtifactRef
			}{name: "raw candidate collection", ref: *run.RawCandidateCollectionRef},
		)
	}
	for _, input := range inputs {
		if err := runs.CheckArtifactEligibility(input.ref, runrepo.ArtifactUseEvaluation); err != nil {
			return EvaluationCaseResult{}, fmt.Errorf("%s is not evaluation eligible: %w", input.name, err)
		}
	}
	var report contractsv1alpha1.GovernedReviewReport
	if err := runs.ReadJSONArtifact(*run.GovernedReportRef, &report); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("read governed report: %w", err)
	}
	if err := report.Validate(); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("validate governed report: %w", err)
	}
	evidenceOwnerRunID, err := committedFormalEvidenceOwner(runs, run)
	if err != nil {
		return EvaluationCaseResult{}, err
	}
	if report.ReviewRunID != evidenceOwnerRunID {
		return EvaluationCaseResult{}, fmt.Errorf("governed report does not bind ReviewRun")
	}
	var verification contractsv1alpha1.CandidateVerificationLedger
	if err := runs.ReadJSONArtifact(*run.VerificationLedgerRef, &verification); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("read candidate verification ledger: %w", err)
	}
	if err := report.ValidateAgainstVerificationLedger(verification); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("validate report verification lineage: %w", err)
	}
	var calibration contractsv1alpha1.FindingCalibrationLedger
	if err := runs.ReadJSONArtifact(*run.CalibrationLedgerRef, &calibration); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("read finding calibration ledger: %w", err)
	}
	if err := calibration.ValidateAgainst(report, verification); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("validate finding calibration lineage: %w", err)
	}
	var suppression contractsv1alpha1.FindingSuppressionLedger
	if err := runs.ReadJSONArtifact(*run.SuppressionLedgerRef, &suppression); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("read finding suppression ledger: %w", err)
	}
	if err := suppression.ValidateAgainst(report, calibration); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("validate finding suppression lineage: %w", err)
	}
	runRef, err := runs.CommittedRunRef(run.RunID)
	if err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("load committed ReviewRun ref: %w", err)
	}
	if err := runs.CheckArtifactEligibility(runRef, runrepo.ArtifactUseEvaluation); err != nil {
		return EvaluationCaseResult{}, fmt.Errorf("committed ReviewRun is not evaluation eligible: %w", err)
	}
	evaluated := uint32(0)
	eligibleFindings := make([]contractsv1alpha1.GovernedReviewFinding, 0, len(report.Findings))
	for _, finding := range report.Findings {
		if caseRecord.CurrentLabel.Category == "" || finding.Category == caseRecord.CurrentLabel.Category {
			evaluated++
			eligibleFindings = append(eligibleFindings, finding)
		}
	}
	matchedAnchors, localizedFindings, localization := scoreLocalization(
		caseRecord.CurrentLabel.Anchors, eligibleFindings, report.Completeness,
	)
	observedSuppressions, suppressedTargets, escapedTargets, inconclusiveTargets, filterEfficacy :=
		scoreFilterEfficacyWithGovernance(
			caseRecord.CurrentLabel.SuppressionTargets, report.Candidates,
			suppression.Facts, report.Completeness,
		)
	applyFacts, err := evaluateApplyTrial(
		runs, binding.ApplyTrial, binding, run, report,
	)
	if err != nil {
		return EvaluationCaseResult{}, err
	}
	usageFacts, err := evaluateCommittedUsage(runs, run, report.ExecutionID)
	if err != nil {
		return EvaluationCaseResult{}, err
	}
	dimensions, err := evaluateDimensions(
		runs, run, report, evidenceOwnerRunID, caseRecord.CurrentLabel, binding.DimensionScope,
	)
	if err != nil {
		return EvaluationCaseResult{}, err
	}
	defectPresence := MetricAvailability{Available: true}
	if caseRecord.Case.Type == CaseFixValidation || caseRecord.Case.Type == CaseWorkflowInvariant {
		defectPresence = MetricAvailability{ReasonCode: "metric_not_applicable"}
	}
	result := EvaluationCaseResult{
		CaseID: binding.CaseID, CaseType: caseRecord.Case.Type,
		LabelRevision:       binding.ExpectedLabelRevision,
		LabelPolicyRevision: caseRecord.CurrentLabelPolicyRevision,
		ExpectedOutcome:     caseRecord.CurrentLabel.ExpectedOutcome,
		ExpectedCategory:    caseRecord.CurrentLabel.Category, ReviewRunID: run.RunID,
		ReviewRunRef: runRef, InputSnapshotRef: run.TargetSnapshotRef,
		GovernedReportRef: *run.GovernedReportRef, ReportCompleteness: string(report.Completeness),
		VerificationLedgerRef: *run.VerificationLedgerRef,
		CalibrationLedgerRef:  *run.CalibrationLedgerRef,
		SuppressionLedgerRef:  *run.SuppressionLedgerRef,
		TotalFindingCount:     uint32(len(report.Findings)), EvaluatedFindingCount: evaluated,
		ExpectedAnchorCount: uint32(len(caseRecord.CurrentLabel.Anchors)),
		MatchedAnchorCount:  matchedAnchors, LocalizedFindingCount: localizedFindings,
		ExpectedSuppressionCount: uint32(len(caseRecord.CurrentLabel.SuppressionTargets)),
		ObservedSuppressionCount: observedSuppressions,
		SuppressedTargetCount:    suppressedTargets, EscapedTargetCount: escapedTargets,
		InconclusiveTargetCount:   inconclusiveTargets,
		ApplyTrialID:              applyFacts.trialID,
		ApplyEditScriptRef:        applyFacts.editScriptRef,
		ApplyChecksExecuted:       applyFacts.executed,
		ApplyChecksPassed:         applyFacts.passed,
		ApplyChecksFailed:         applyFacts.failed,
		ApplyAuthority:            applyFacts.authority,
		UsageCompleteness:         usageFacts.completeness,
		UsageReceiptCount:         usageFacts.receipts,
		UsageReportedReceipts:     usageFacts.reported,
		UsagePartialReceipts:      usageFacts.partial,
		UsageUnavailableReceipts:  usageFacts.unavailable,
		InputTokens:               usageFacts.input,
		OutputTokens:              usageFacts.output,
		CacheReadTokens:           usageFacts.cacheRead,
		CacheWriteTokens:          usageFacts.cacheWrite,
		ReasoningTokens:           usageFacts.reasoning,
		ReasoningReportedReceipts: usageFacts.reasoningReported,
		TotalTokens:               usageFacts.total,
		UsageAuthority:            usageFacts.authority,
		Dimensions:                dimensions,
		DefectPresence:            defectPresence,
		Localization:              localization,
		FilterEfficacy:            filterEfficacy,
		ApplyFidelity:             applyFacts.availability,
		Usage:                     usageFacts.availability,
	}
	if run.HypothesisSetRef != nil {
		ref := *run.HypothesisSetRef
		result.HypothesisSetRef = &ref
	}
	if run.RawCandidateCollectionRef != nil {
		ref := *run.RawCandidateCollectionRef
		result.RawCandidateCollectionRef = &ref
	}
	result.Verdict, result.ReasonCode = scoreEvaluationResult(result)
	return result, nil
}

func validateEvaluationTargetAgainstCase(
	runs EvaluationRunReader,
	run runmodel.ReviewRun,
	snapshot runmodel.ExecutionSnapshot,
	caseInputSnapshotURI string,
) error {
	if run.TargetSnapshotRef.URI == caseInputSnapshotURI {
		return nil
	}
	variantBytes, err := runs.ReadArtifact(run.TargetSnapshotRef)
	if err != nil {
		return fmt.Errorf("read index replay target snapshot: %w", err)
	}
	variantTarget, err := targetmodel.DecodeMaterializedTarget(variantBytes)
	if err != nil {
		return fmt.Errorf("decode index replay target snapshot: %w", err)
	}
	variantTarget.Contexts = nil
	seen := make(map[string]struct{})
	for depth := 0; depth < 64; depth++ {
		if _, duplicate := seen[run.RunID]; duplicate {
			return fmt.Errorf("index replay target lineage contains a cycle")
		}
		seen[run.RunID] = struct{}{}
		if run.Kind != runmodel.RunKindReplay ||
			run.ReplayVariable != runmodel.ReplayVariableIndex ||
			run.SourceRunID == "" || snapshot.ReplaySourceRunRef == nil ||
			snapshot.ReplayChangeSetRef == nil {
			return fmt.Errorf(
				"target snapshot %q does not match case input_snapshot_ref",
				run.TargetSnapshotRef.URI,
			)
		}
		committedSourceRef, err := runs.CommittedRunRef(run.SourceRunID)
		if err != nil || committedSourceRef != *snapshot.ReplaySourceRunRef {
			return fmt.Errorf("index replay target lineage does not bind committed source")
		}
		var change runmodel.ReplayChangeSet
		if err := runs.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
			return fmt.Errorf("read index replay target change set: %w", err)
		}
		if err := change.Validate(); err != nil ||
			change.Variable != runmodel.ReplayVariableIndex ||
			change.SourceRunID != run.SourceRunID ||
			change.StartStage != "materialize_target" || change.RemoteWrites != "deny" {
			return fmt.Errorf("index replay target change set does not bind atomic index replay")
		}
		sourceRun, err := runs.LoadRun(run.SourceRunID)
		if err != nil || sourceRun.Status != runmodel.RunStatusSucceeded {
			return fmt.Errorf("load succeeded index replay target source")
		}
		sourceBytes, err := runs.ReadArtifact(sourceRun.TargetSnapshotRef)
		if err != nil {
			return fmt.Errorf("read index replay source target snapshot: %w", err)
		}
		sourceTarget, err := targetmodel.DecodeMaterializedTarget(sourceBytes)
		if err != nil {
			return fmt.Errorf("decode index replay source target snapshot: %w", err)
		}
		sourceTarget.Contexts = nil
		if !reflect.DeepEqual(sourceTarget, variantTarget) {
			return fmt.Errorf("index replay changed evaluation target outside contexts")
		}
		if sourceRun.TargetSnapshotRef.URI == caseInputSnapshotURI {
			return nil
		}
		sourceSnapshot, err := runs.ExecutionSnapshotForRun(sourceRun.RunID)
		if err != nil {
			return fmt.Errorf("load index replay source execution snapshot: %w", err)
		}
		run, snapshot, variantTarget = sourceRun, sourceSnapshot, sourceTarget
	}
	return fmt.Errorf("index replay target lineage exceeds maximum depth")
}

// committedFormalEvidenceOwner returns the ReviewRun that originally produced
// the provider-backed formal facts. A formal filter-policy replay is a derived
// projection and may reuse those exact immutable refs, but no other replay may
// detach artifacts from its own ReviewRun identity.
func committedFormalEvidenceOwner(
	runs EvaluationRunReader,
	run runmodel.ReviewRun,
) (string, error) {
	if run.Kind != runmodel.RunKindReplay ||
		!run.ReplayVariable.IsFilterPolicy() {
		return run.RunID, nil
	}
	source, err := runs.LoadRun(run.SourceRunID)
	if err != nil {
		return "", fmt.Errorf("load finding_governance evidence source: %w", err)
	}
	if source.Status != runmodel.RunStatusSucceeded {
		return "", fmt.Errorf("finding_governance evidence source is not succeeded")
	}
	for name, pair := range map[string][2]*runmodel.ArtifactRef{
		"hypothesis set":      {run.HypothesisSetRef, source.HypothesisSetRef},
		"candidate set":       {run.CandidateSetRef, source.CandidateSetRef},
		"verification ledger": {run.VerificationLedgerRef, source.VerificationLedgerRef},
		"governed report":     {run.GovernedReportRef, source.GovernedReportRef},
		"governed markdown":   {run.GovernedMarkdownRef, source.GovernedMarkdownRef},
	} {
		if pair[0] == nil || pair[1] == nil || *pair[0] != *pair[1] {
			return "", fmt.Errorf("finding_governance replay changed source %s", name)
		}
	}
	var report contractsv1alpha1.GovernedReviewReport
	if err := runs.ReadJSONArtifact(*source.GovernedReportRef, &report); err != nil {
		return "", fmt.Errorf("read finding_governance source report: %w", err)
	}
	return report.ReviewRunID, nil
}

type usageEvaluationFacts struct {
	completeness      contractsv1alpha1.AgentTokenUsageCompleteness
	receipts          uint64
	reported          uint64
	partial           uint64
	unavailable       uint64
	input             uint64
	output            uint64
	cacheRead         uint64
	cacheWrite        uint64
	reasoning         uint64
	reasoningReported uint64
	total             uint64
	authority         string
	availability      MetricAvailability
}

func evaluateCommittedUsage(
	runs EvaluationRunReader,
	run runmodel.ReviewRun,
	executionID string,
) (usageEvaluationFacts, error) {
	missing := usageEvaluationFacts{
		completeness: contractsv1alpha1.AgentTokenUsageUnavailable,
		authority:    "unavailable",
		availability: MetricAvailability{ReasonCode: "usage_receipt_artifact_unavailable"},
	}
	collection, err := readCommittedExecutionReceipts(runs, run, executionID)
	if err != nil {
		return usageEvaluationFacts{}, err
	}
	if collection == nil {
		return missing, nil
	}
	return aggregateUsageReceipts(collection.Receipts)
}

func readCommittedExecutionReceipts(
	runs EvaluationRunReader,
	run runmodel.ReviewRun,
	executionID string,
) (*contractsv1alpha1.AgentExecutionReceiptCollection, error) {
	if run.AgentExecutionReceiptRef == nil {
		return nil, nil
	}
	ref := *run.AgentExecutionReceiptRef
	if err := ref.Validate(); err != nil || ref.Contract != runmodel.ContractAgentExecutionReceipts {
		return nil, fmt.Errorf("invalid committed agent execution receipt ref")
	}
	data, err := runs.ReadArtifact(ref)
	if err != nil {
		return nil, fmt.Errorf("read committed agent execution receipts: %w", err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != ref.SizeBytes || hex.EncodeToString(digest[:]) != ref.SHA256 {
		return nil, fmt.Errorf("committed agent execution receipt bytes changed")
	}
	collection, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(data)
	if err != nil {
		return nil, fmt.Errorf("strictly decode committed agent execution receipts: %w", err)
	}
	if collection.SourceRunID != run.RunID || collection.ReviewRunID != run.RunID ||
		collection.ExecutionID != executionID {
		return nil, fmt.Errorf("committed agent execution receipts escaped run or execution")
	}
	return &collection, nil
}

func aggregateUsageReceipts(
	receipts []contractsv1alpha1.AgentExecutionReceipt,
) (usageEvaluationFacts, error) {
	facts := usageEvaluationFacts{
		completeness: contractsv1alpha1.AgentTokenUsageUnavailable,
		receipts:     uint64(len(receipts)), authority: EvaluationUsageAuthority,
		availability: MetricAvailability{ReasonCode: "usage_receipts_incomplete"},
	}
	for index, receipt := range receipts {
		var ok bool
		if facts.input, ok = checkedEvaluationTokenAdd(facts.input, receipt.Usage.InputTokens); !ok {
			return usageEvaluationFacts{}, fmt.Errorf("receipt[%d] input token total overflows", index)
		}
		if facts.output, ok = checkedEvaluationTokenAdd(facts.output, receipt.Usage.OutputTokens); !ok {
			return usageEvaluationFacts{}, fmt.Errorf("receipt[%d] output token total overflows", index)
		}
		if facts.cacheRead, ok = checkedEvaluationTokenAdd(facts.cacheRead, receipt.Usage.CacheReadTokens); !ok {
			return usageEvaluationFacts{}, fmt.Errorf("receipt[%d] cache-read token total overflows", index)
		}
		if facts.cacheWrite, ok = checkedEvaluationTokenAdd(facts.cacheWrite, receipt.Usage.CacheWriteTokens); !ok {
			return usageEvaluationFacts{}, fmt.Errorf("receipt[%d] cache-write token total overflows", index)
		}
		if facts.total, ok = checkedEvaluationTokenAdd(facts.total, receipt.Usage.TotalTokens); !ok {
			return usageEvaluationFacts{}, fmt.Errorf("receipt[%d] token total overflows", index)
		}
		switch receipt.Usage.Completeness {
		case contractsv1alpha1.AgentTokenUsageProviderReported:
			facts.reported++
		case contractsv1alpha1.AgentTokenUsagePartial:
			facts.partial++
		default:
			facts.unavailable++
		}
		if receipt.Usage.ReasoningTokens != nil {
			facts.reasoningReported++
			if facts.reasoning, ok = checkedEvaluationTokenAdd(facts.reasoning, *receipt.Usage.ReasoningTokens); !ok {
				return usageEvaluationFacts{}, fmt.Errorf("receipt[%d] reasoning token total overflows", index)
			}
		}
	}
	if facts.receipts > 0 && facts.reported == facts.receipts {
		facts.completeness = contractsv1alpha1.AgentTokenUsageProviderReported
		facts.availability = MetricAvailability{Available: true}
	} else if facts.reported > 0 || facts.partial > 0 {
		facts.completeness = contractsv1alpha1.AgentTokenUsagePartial
	}
	return facts, nil
}

func checkedEvaluationTokenAdd(left, right uint64) (uint64, bool) {
	if right > math.MaxUint64-left {
		return 0, false
	}
	return left + right, true
}

func checkedEvaluationTokenSum(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		var ok bool
		if total, ok = checkedEvaluationTokenAdd(total, value); !ok {
			return 0, false
		}
	}
	return total, true
}

type applyEvaluationFacts struct {
	trialID       string
	editScriptRef *runmodel.ArtifactRef
	executed      uint32
	passed        uint32
	failed        uint32
	authority     string
	availability  MetricAvailability
}

func evaluateApplyTrial(
	runs EvaluationRunReader,
	trial *ApplyTrial,
	binding EvaluationCaseRunBinding,
	run runmodel.ReviewRun,
	report contractsv1alpha1.GovernedReviewReport,
) (applyEvaluationFacts, error) {
	if trial == nil {
		return applyEvaluationFacts{
			authority:    "unavailable",
			availability: MetricAvailability{ReasonCode: "metric_not_applicable"},
		}, nil
	}
	if err := trial.Validate(); err != nil {
		return applyEvaluationFacts{}, fmt.Errorf("validate apply trial: %w", err)
	}
	if trial.CaseID != binding.CaseID || trial.LabelRevision != binding.ExpectedLabelRevision ||
		trial.ReviewRunID != run.RunID || trial.InputSnapshotRef != run.TargetSnapshotRef ||
		run.GovernedReportRef == nil || trial.GovernedReportRef != *run.GovernedReportRef ||
		trial.ExecutedAt.Before(report.GeneratedAt) {
		return applyEvaluationFacts{}, fmt.Errorf("apply trial does not bind exact evaluation inputs")
	}
	var matched *contractsv1alpha1.GovernedReviewFinding
	for index := range report.Findings {
		finding := &report.Findings[index]
		if finding.FindingID == trial.FindingID {
			matched = finding
			break
		}
	}
	if matched == nil || matched.Fingerprint != trial.FindingFingerprint || matched.Suggestion == nil {
		return applyEvaluationFacts{}, fmt.Errorf("apply trial does not bind a governed Finding suggestion")
	}
	suggestionDigest := sha256.Sum256([]byte(*matched.Suggestion))
	if hex.EncodeToString(suggestionDigest[:]) != trial.SuggestionSHA256 {
		return applyEvaluationFacts{}, fmt.Errorf("apply trial suggestion digest does not bind the Finding")
	}
	if err := verifyApplyArtifact(runs, trial.EditScriptRef); err != nil {
		return applyEvaluationFacts{}, fmt.Errorf("verify apply edit script: %w", err)
	}
	facts := projectApplyTrial(trial)
	for _, check := range trial.Checks {
		if err := verifyApplyArtifact(runs, check.EvidenceRef); err != nil {
			return applyEvaluationFacts{}, fmt.Errorf("verify %s evidence: %w", check.Kind, err)
		}
	}
	return facts, nil
}

func projectApplyTrial(trial *ApplyTrial) applyEvaluationFacts {
	if trial == nil {
		return applyEvaluationFacts{
			authority:    "unavailable",
			availability: MetricAvailability{ReasonCode: "metric_not_applicable"},
		}
	}
	facts := applyEvaluationFacts{
		trialID: trial.TrialID, editScriptRef: clonePointer(trial.EditScriptRef),
		authority: trial.Authority, executed: uint32(len(trial.Checks)),
	}
	for _, check := range trial.Checks {
		if check.Status == ApplyCheckPassed {
			facts.passed++
		} else {
			facts.failed++
		}
	}
	if trial.Completeness == ApplyTrialConclusive {
		facts.availability = MetricAvailability{Available: true}
	} else {
		facts.availability = MetricAvailability{ReasonCode: "apply_evidence_partial"}
	}
	return facts
}

func verifyApplyArtifact(runs EvaluationRunReader, ref runmodel.ArtifactRef) error {
	data, err := runs.ReadArtifact(ref)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != ref.SizeBytes || hex.EncodeToString(digest[:]) != ref.SHA256 {
		return fmt.Errorf("artifact bytes do not bind declared digest and size")
	}
	return nil
}

func scoreFilterEfficacy(
	expected []SuppressionTarget,
	candidates []contractsv1alpha1.GovernedReviewCandidate,
	completeness contractsv1alpha1.AgentReviewCompleteness,
) (uint32, uint32, uint32, uint32, MetricAvailability) {
	return scoreFilterEfficacyWithGovernance(expected, candidates, nil, completeness)
}

func scoreFilterEfficacyWithGovernance(
	expected []SuppressionTarget,
	candidates []contractsv1alpha1.GovernedReviewCandidate,
	suppressionFacts []contractsv1alpha1.FindingSuppressionFact,
	completeness contractsv1alpha1.AgentReviewCompleteness,
) (uint32, uint32, uint32, uint32, MetricAvailability) {
	if len(expected) == 0 {
		return 0, 0, 0, 0, MetricAvailability{
			ReasonCode: "structured_suppression_truth_unavailable",
		}
	}
	if completeness != contractsv1alpha1.AgentReviewComplete {
		return 0, 0, 0, 0, MetricAvailability{ReasonCode: "review_coverage_partial"}
	}
	dispositions := make(map[string]contractsv1alpha1.GovernedCandidateDisposition, len(candidates))
	candidateIDs := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		dispositions[candidate.Hypothesis.ClusterFingerprint] = candidate.Disposition
		candidateIDs[candidate.Hypothesis.ClusterFingerprint] = candidate.CandidateID
	}
	suppressedCandidates := make(map[string]bool, len(suppressionFacts))
	for _, fact := range suppressionFacts {
		suppressedCandidates[fact.CandidateID] = fact.Action == contractsv1alpha1.FindingSuppressed
	}
	var observed, suppressed, escaped, inconclusive uint32
	for _, target := range expected {
		disposition, exists := dispositions[target.ClusterFingerprint]
		if !exists {
			continue
		}
		observed++
		switch disposition {
		case contractsv1alpha1.GovernedCandidateRejected:
			suppressed++
		case contractsv1alpha1.GovernedCandidateConfirmed:
			if suppressedCandidates[candidateIDs[target.ClusterFingerprint]] {
				suppressed++
			} else {
				escaped++
			}
		case contractsv1alpha1.GovernedCandidateInconclusive:
			inconclusive++
		}
	}
	if observed != uint32(len(expected)) {
		return observed, suppressed, escaped, inconclusive, MetricAvailability{
			ReasonCode: "suppression_target_not_observed",
		}
	}
	return observed, suppressed, escaped, inconclusive, MetricAvailability{Available: true}
}

func scoreLocalization(
	expected []LabelAnchor,
	findings []contractsv1alpha1.GovernedReviewFinding,
	completeness contractsv1alpha1.AgentReviewCompleteness,
) (uint32, uint32, MetricAvailability) {
	if len(expected) == 0 {
		return 0, 0, MetricAvailability{ReasonCode: "structured_anchor_truth_unavailable"}
	}
	if completeness != contractsv1alpha1.AgentReviewComplete {
		return 0, 0, MetricAvailability{ReasonCode: "review_coverage_partial"}
	}
	matched := make([]bool, len(expected))
	var localized uint32
	for _, finding := range findings {
		findingMatched := false
		for index, anchor := range expected {
			actual := finding.Anchor
			if anchor.Path == actual.Path && anchor.Side == string(actual.Side) &&
				anchor.SourceDigest == actual.SourceDigest &&
				anchor.StartLine <= actual.EndLine && actual.StartLine <= anchor.EndLine {
				matched[index] = true
				findingMatched = true
			}
		}
		if findingMatched {
			localized++
		}
	}
	var matchedCount uint32
	for _, value := range matched {
		if value {
			matchedCount++
		}
	}
	return matchedCount, localized, MetricAvailability{Available: true}
}

func scoreDefectPresence(
	expected ExpectedOutcome,
	findingCount uint32,
	completeness string,
) (EvaluationVerdict, string) {
	if completeness != string(contractsv1alpha1.AgentReviewComplete) {
		return EvaluationInconclusive, "review_coverage_partial"
	}
	if isPositiveOutcome(expected) {
		if findingCount > 0 {
			return EvaluationPass, "expected_defect_detected"
		}
		return EvaluationFail, "expected_defect_missed"
	}
	if findingCount == 0 {
		return EvaluationPass, "expected_clean_preserved"
	}
	return EvaluationFail, "unexpected_finding_detected"
}

func scoreEvaluationResult(result EvaluationCaseResult) (EvaluationVerdict, string) {
	switch result.CaseType {
	case CaseFixValidation:
		if !result.ApplyFidelity.Available {
			return EvaluationInconclusive, "apply_evidence_partial"
		}
		observedValid := result.ApplyChecksFailed == 0
		expectedValid := result.ExpectedOutcome == OutcomeFixValid
		if observedValid == expectedValid {
			if expectedValid {
				return EvaluationPass, "expected_fix_validated"
			}
			return EvaluationPass, "expected_fix_invalidated"
		}
		return EvaluationFail, "fix_validity_mismatch"
	case CaseWorkflowInvariant:
		return EvaluationInconclusive, "workflow_invariant_evidence_unavailable"
	default:
		return scoreDefectPresence(
			result.ExpectedOutcome, result.EvaluatedFindingCount, result.ReportCompleteness,
		)
	}
}

func isPositiveOutcome(outcome ExpectedOutcome) bool {
	switch outcome {
	case OutcomeDefectPresent, OutcomeMissedDefect:
		return true
	default:
		return false
	}
}

func isNegativeOutcome(outcome ExpectedOutcome) bool {
	return outcome == OutcomeClean || outcome == OutcomeFalsePositive
}

func summarizeEvaluationResults(results []EvaluationCaseResult) EvaluationRunSummary {
	copyResults := slices.Clone(results)
	sort.Slice(copyResults, func(left, right int) bool { return copyResults[left].CaseID < copyResults[right].CaseID })
	var summary EvaluationRunSummary
	for _, result := range copyResults {
		summary.Cases++
		positive := isPositiveOutcome(result.ExpectedOutcome)
		negative := isNegativeOutcome(result.ExpectedOutcome)
		if positive {
			summary.PositiveCases++
		} else if negative {
			summary.NegativeCases++
		}
		switch result.Verdict {
		case EvaluationPass:
			summary.Passed++
			if positive {
				summary.PositivePassed++
			} else if negative {
				summary.NegativePassed++
			}
		case EvaluationFail:
			summary.Failed++
		case EvaluationInconclusive:
			summary.Inconclusive++
		}
	}
	return summary
}

func (repository *Repository) GetEvaluationRun(
	runID string,
	access Access,
) (EvaluationRun, error) {
	if err := validateID("evaluation_run_id", runID); err != nil {
		return EvaluationRun{}, err
	}
	if err := access.Validate(); err != nil {
		return EvaluationRun{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return EvaluationRun{}, err
	}
	run, err := state.evaluationRun(runID)
	if err != nil {
		return EvaluationRun{}, err
	}
	for _, result := range run.Results {
		record := state.cases[result.CaseID]
		if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
			return EvaluationRun{}, err
		}
	}
	return run, nil
}

func (repository *Repository) ListEvaluationRuns(access Access) ([]EvaluationRun, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.evaluationRuns))
	for id, run := range state.evaluationRuns {
		visible := true
		for _, result := range run.Results {
			if err := authorizeCaseRead(state.cases[result.CaseID].Case, access.Roles); err != nil {
				visible = false
				break
			}
		}
		if visible {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]EvaluationRun, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneValue(state.evaluationRuns[id]))
	}
	return result, nil
}
