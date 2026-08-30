package pireviewmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"

	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	NormalizationPolicyComparisonSchemaVersion = "argus.normalization_policy_comparison.v1alpha1"
	NormalizationPolicyComparisonContract      = NormalizationPolicyComparisonSchemaVersion + "+json"
	normalizationRecordedPolicy                = "recorded_execution_decisions"
)

// NormalizationPolicyComparison is an immutable, diagnostic-only paired
// comparison over committed raw-candidate facts. It never changes the source
// ReviewRuns and is not an EvaluationRun or promotion result.
type NormalizationPolicyComparison struct {
	SchemaVersion  string                               `json:"schema_version"`
	ComparisonID   string                               `json:"comparison_id"`
	Authority      string                               `json:"authority"`
	Disposition    string                               `json:"disposition"`
	BaselinePolicy string                               `json:"baseline_policy"`
	VariantPolicy  string                               `json:"variant_policy"`
	Cases          []NormalizationPolicyComparisonCase  `json:"cases"`
	Summary        NormalizationPolicyComparisonSummary `json:"summary"`
	CreatedAt      time.Time                            `json:"created_at"`
}

type NormalizationPolicyComparisonInput struct {
	CaseID                    string
	SourceReviewRunRef        runmodel.ArtifactRef
	RawCandidateCollectionRef runmodel.ArtifactRef
	RawCandidates             contractsv1alpha1.AgentReviewRawCandidateCollection
}

type NormalizationPolicyComparisonCase struct {
	CaseID                    string                        `json:"case_id"`
	SourceReviewRunID         string                        `json:"source_review_run_id"`
	SourceReviewRunRef        runmodel.ArtifactRef          `json:"source_review_run_ref"`
	RawCandidateCollectionRef runmodel.ArtifactRef          `json:"raw_candidate_collection_ref"`
	TargetDigest              string                        `json:"target_digest"`
	Baseline                  NormalizationPreviewSummary   `json:"baseline"`
	Variant                   NormalizationPreviewSummary   `json:"variant"`
	RetainedDelta             int                           `json:"retained_delta"`
	MergedExactDelta          int                           `json:"merged_exact_delta"`
	MergedSemanticDelta       int                           `json:"merged_semantic_delta"`
	ChangedDecisions          []NormalizationDecisionChange `json:"changed_decisions"`
	VariantClusters           []NormalizationPreviewCluster `json:"variant_clusters"`
}

type NormalizationDecisionChange struct {
	RawCandidateID string `json:"raw_candidate_id"`
	BaselineAction string `json:"baseline_action"`
	BaselineReason string `json:"baseline_reason_code"`
	VariantAction  string `json:"variant_action"`
	VariantReason  string `json:"variant_reason_code"`
	CanonicalRawID string `json:"canonical_raw_candidate_id,omitempty"`
}

type NormalizationPolicyComparisonSummary struct {
	Cases                  int `json:"cases"`
	RawCandidates          int `json:"raw_candidates"`
	EligibleCandidates     int `json:"eligible_candidates"`
	BaselineRetained       int `json:"baseline_retained"`
	VariantRetained        int `json:"variant_retained"`
	RetainedDelta          int `json:"retained_delta"`
	BaselineMergedExact    int `json:"baseline_merged_exact"`
	VariantMergedExact     int `json:"variant_merged_exact"`
	MergedExactDelta       int `json:"merged_exact_delta"`
	BaselineMergedSemantic int `json:"baseline_merged_semantic"`
	VariantMergedSemantic  int `json:"variant_merged_semantic"`
	MergedSemanticDelta    int `json:"merged_semantic_delta"`
	ChangedDecisions       int `json:"changed_decisions"`
}

func DecodeNormalizationPolicyComparison(data []byte) (NormalizationPolicyComparison, error) {
	var value NormalizationPolicyComparison
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return NormalizationPolicyComparison{}, fmt.Errorf("decode NormalizationPolicyComparison: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return NormalizationPolicyComparison{}, fmt.Errorf("decode NormalizationPolicyComparison: multiple JSON values are not allowed")
		}
		return NormalizationPolicyComparison{}, fmt.Errorf("decode NormalizationPolicyComparison trailing JSON: %w", err)
	}
	if err := value.Validate(); err != nil {
		return NormalizationPolicyComparison{}, fmt.Errorf("validate NormalizationPolicyComparison: %w", err)
	}
	return value, nil
}

func (comparison NormalizationPolicyComparison) Validate() error {
	if comparison.SchemaVersion != NormalizationPolicyComparisonSchemaVersion ||
		comparison.Authority != "diagnostic_only" || comparison.Disposition != "comparison_only" ||
		comparison.BaselinePolicy != normalizationRecordedPolicy ||
		comparison.VariantPolicy != NormalizationPreviewPolicyRevision {
		return fmt.Errorf("normalization comparison has unsupported schema or authority")
	}
	if strings.TrimSpace(comparison.ComparisonID) == "" || strings.ContainsAny(comparison.ComparisonID, " \t\r\n/\\") {
		return fmt.Errorf("comparison_id must be a non-empty path-safe ID")
	}
	if len(comparison.Cases) == 0 || len(comparison.Cases) > 1024 {
		return fmt.Errorf("cases must contain between 1 and 1024 entries")
	}
	if comparison.CreatedAt.IsZero() || comparison.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	recomputed := NormalizationPolicyComparisonSummary{}
	previous := ""
	for index, item := range comparison.Cases {
		if item.CaseID == "" || (index > 0 && item.CaseID <= previous) {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
		previous = item.CaseID
		if item.SourceReviewRunID == "" || item.TargetDigest == "" {
			return fmt.Errorf("case %q is missing source identity", item.CaseID)
		}
		if err := item.SourceReviewRunRef.Validate(); err != nil || item.SourceReviewRunRef.Contract != runmodel.ContractReviewRun {
			return fmt.Errorf("case %q has invalid ReviewRun ref", item.CaseID)
		}
		if err := item.RawCandidateCollectionRef.Validate(); err != nil || item.RawCandidateCollectionRef.Contract != runmodel.ContractAgentReviewRawCandidates {
			return fmt.Errorf("case %q has invalid raw collection ref", item.CaseID)
		}
		if item.RetainedDelta != item.Variant.Retained-item.Baseline.Retained ||
			item.MergedExactDelta != item.Variant.MergedExact-item.Baseline.MergedExact ||
			item.MergedSemanticDelta != item.Variant.MergedSemantic-item.Baseline.MergedSemantic {
			return fmt.Errorf("case %q contains inconsistent deltas", item.CaseID)
		}
		accumulateNormalizationComparison(&recomputed, item)
	}
	recomputed.RetainedDelta = recomputed.VariantRetained - recomputed.BaselineRetained
	recomputed.MergedExactDelta = recomputed.VariantMergedExact - recomputed.BaselineMergedExact
	recomputed.MergedSemanticDelta = recomputed.VariantMergedSemantic - recomputed.BaselineMergedSemantic
	if !reflect.DeepEqual(recomputed, comparison.Summary) {
		return fmt.Errorf("summary does not match case facts")
	}
	return nil
}

func BuildNormalizationPolicyComparison(
	comparisonID string,
	inputs []NormalizationPolicyComparisonInput,
	createdAt time.Time,
) (NormalizationPolicyComparison, error) {
	if strings.TrimSpace(comparisonID) == "" || strings.ContainsAny(comparisonID, " \t\r\n/\\") {
		return NormalizationPolicyComparison{}, fmt.Errorf("comparison_id must be a non-empty path-safe ID")
	}
	if len(inputs) == 0 || len(inputs) > 1024 {
		return NormalizationPolicyComparison{}, fmt.Errorf("comparison inputs must contain between 1 and 1024 cases")
	}
	if createdAt.IsZero() || createdAt.Location() != time.UTC {
		return NormalizationPolicyComparison{}, fmt.Errorf("created_at must be non-zero UTC")
	}
	inputs = slices.Clone(inputs)
	slices.SortFunc(inputs, func(left, right NormalizationPolicyComparisonInput) int {
		return strings.Compare(left.CaseID, right.CaseID)
	})
	comparison := NormalizationPolicyComparison{
		SchemaVersion: NormalizationPolicyComparisonSchemaVersion,
		ComparisonID:  comparisonID, Authority: "diagnostic_only", Disposition: "comparison_only",
		BaselinePolicy: normalizationRecordedPolicy, VariantPolicy: NormalizationPreviewPolicyRevision,
		Cases: []NormalizationPolicyComparisonCase{}, CreatedAt: createdAt,
	}
	previousCaseID := ""
	for _, input := range inputs {
		if strings.TrimSpace(input.CaseID) == "" || strings.ContainsAny(input.CaseID, " \t\r\n/\\") {
			return NormalizationPolicyComparison{}, fmt.Errorf("case_id must be a non-empty path-safe ID")
		}
		if input.CaseID == previousCaseID {
			return NormalizationPolicyComparison{}, fmt.Errorf("case_id %q is duplicated", input.CaseID)
		}
		previousCaseID = input.CaseID
		if err := input.SourceReviewRunRef.Validate(); err != nil {
			return NormalizationPolicyComparison{}, fmt.Errorf("case %q source ReviewRun ref: %w", input.CaseID, err)
		}
		if input.SourceReviewRunRef.Contract != runmodel.ContractReviewRun {
			return NormalizationPolicyComparison{}, fmt.Errorf("case %q source ref is not a ReviewRun", input.CaseID)
		}
		if err := input.RawCandidateCollectionRef.Validate(); err != nil {
			return NormalizationPolicyComparison{}, fmt.Errorf("case %q raw collection ref: %w", input.CaseID, err)
		}
		if input.RawCandidateCollectionRef.Contract != runmodel.ContractAgentReviewRawCandidates {
			return NormalizationPolicyComparison{}, fmt.Errorf("case %q raw ref has contract %q", input.CaseID, input.RawCandidateCollectionRef.Contract)
		}
		if err := input.RawCandidates.Validate(); err != nil {
			return NormalizationPolicyComparison{}, fmt.Errorf("case %q raw collection: %w", input.CaseID, err)
		}
		preview, err := PreviewNormalization(input.RawCandidates)
		if err != nil {
			return NormalizationPolicyComparison{}, fmt.Errorf("case %q preview: %w", input.CaseID, err)
		}
		baseline := recordedNormalizationSummary(input.RawCandidates)
		changes := normalizationDecisionChanges(preview.Decisions)
		item := NormalizationPolicyComparisonCase{
			CaseID: input.CaseID, SourceReviewRunID: input.RawCandidates.ReviewRunID,
			SourceReviewRunRef:        input.SourceReviewRunRef,
			RawCandidateCollectionRef: input.RawCandidateCollectionRef,
			TargetDigest:              input.RawCandidates.TargetDigest,
			Baseline:                  baseline, Variant: preview.Summary,
			RetainedDelta:       preview.Summary.Retained - baseline.Retained,
			MergedExactDelta:    preview.Summary.MergedExact - baseline.MergedExact,
			MergedSemanticDelta: preview.Summary.MergedSemantic - baseline.MergedSemantic,
			ChangedDecisions:    changes, VariantClusters: preview.Clusters,
		}
		comparison.Cases = append(comparison.Cases, item)
		accumulateNormalizationComparison(&comparison.Summary, item)
	}
	comparison.Summary.RetainedDelta = comparison.Summary.VariantRetained - comparison.Summary.BaselineRetained
	comparison.Summary.MergedExactDelta = comparison.Summary.VariantMergedExact - comparison.Summary.BaselineMergedExact
	comparison.Summary.MergedSemanticDelta = comparison.Summary.VariantMergedSemantic - comparison.Summary.BaselineMergedSemantic
	if err := comparison.Validate(); err != nil {
		return NormalizationPolicyComparison{}, err
	}
	return comparison, nil
}

func recordedNormalizationSummary(collection contractsv1alpha1.AgentReviewRawCandidateCollection) NormalizationPreviewSummary {
	summary := NormalizationPreviewSummary{RawCandidates: len(collection.RawCandidates)}
	for _, candidate := range collection.RawCandidates {
		switch candidate.Action {
		case contractsv1alpha1.HypothesisNormalizationRetained:
			summary.EligibleCandidates++
			summary.Retained++
		case contractsv1alpha1.HypothesisNormalizationMergedDuplicate:
			summary.EligibleCandidates++
			if candidate.ReasonCode == "duplicate_fingerprint" {
				summary.MergedExact++
			} else {
				summary.MergedSemantic++
			}
		default:
			summary.Ineligible++
		}
	}
	return summary
}

func normalizationDecisionChanges(decisions []NormalizationPreviewDecision) []NormalizationDecisionChange {
	changes := make([]NormalizationDecisionChange, 0)
	for _, decision := range decisions {
		if string(decision.PriorAction) == decision.PreviewAction && decision.PriorReasonCode == decision.PreviewReasonCode {
			continue
		}
		changes = append(changes, NormalizationDecisionChange{
			RawCandidateID: decision.RawCandidateID,
			BaselineAction: string(decision.PriorAction), BaselineReason: decision.PriorReasonCode,
			VariantAction: decision.PreviewAction, VariantReason: decision.PreviewReasonCode,
			CanonicalRawID: decision.CanonicalRawCandidateID,
		})
	}
	return changes
}

func accumulateNormalizationComparison(summary *NormalizationPolicyComparisonSummary, item NormalizationPolicyComparisonCase) {
	summary.Cases++
	summary.RawCandidates += item.Baseline.RawCandidates
	summary.EligibleCandidates += item.Baseline.EligibleCandidates
	summary.BaselineRetained += item.Baseline.Retained
	summary.VariantRetained += item.Variant.Retained
	summary.BaselineMergedExact += item.Baseline.MergedExact
	summary.VariantMergedExact += item.Variant.MergedExact
	summary.BaselineMergedSemantic += item.Baseline.MergedSemantic
	summary.VariantMergedSemantic += item.Variant.MergedSemantic
	summary.ChangedDecisions += len(item.ChangedDecisions)
}
