package analyticsadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	evaluationUsageMetricVersion         = "committed-worker-usage-v1"
	evaluationDimensionMetricVersion     = "committed-review-dimension-v1"
	evaluationDimensionRateVersion       = "committed-review-dimension-rate-v1"
	evaluationRepeatabilityMetricVersion = "committed-repeatability-v1"
	evaluationRateScale                  = uint8(6)
)

// EvaluationProjectionSource is the local anti-corruption adapter from
// governed Evaluation/Experiment ledgers to generic analytics ExperimentFact.
// It exports diagnostic token usage only; billing cost remains outside this
// adapter until an authoritative platform ledger exists.
type EvaluationProjectionSource struct {
	evaluations *evaluation.Repository
	runs        *runrepo.Repository
	access      evaluation.Access
}

func NewEvaluationProjectionSource(
	evaluations *evaluation.Repository,
	runs *runrepo.Repository,
	access evaluation.Access,
) (*EvaluationProjectionSource, error) {
	if evaluations == nil || runs == nil {
		return nil, fmt.Errorf("evaluation and run repositories are required")
	}
	if err := access.Validate(); err != nil {
		return nil, fmt.Errorf("validate evaluation projection access: %w", err)
	}
	return &EvaluationProjectionSource{evaluations: evaluations, runs: runs, access: access}, nil
}

func (source *EvaluationProjectionSource) EvaluationFacts(
	ctx context.Context,
	scope Scope,
	window analytics.TimeWindow,
) (EvaluationBatch, error) {
	if source == nil || source.evaluations == nil || source.runs == nil {
		return EvaluationBatch{}, fmt.Errorf("evaluation projection source is not configured")
	}
	if err := contextErr(ctx); err != nil {
		return EvaluationBatch{}, err
	}
	if err := scope.Validate(); err != nil {
		return EvaluationBatch{}, err
	}
	if err := window.Validate(); err != nil {
		return EvaluationBatch{}, err
	}
	experiments, err := source.evaluations.ListExperimentRuns(source.access)
	if err != nil {
		return EvaluationBatch{}, fmt.Errorf("list governed experiments: %w", err)
	}
	facts := make([]analytics.ExperimentFact, 0)
	for _, experimentRun := range experiments {
		if err := contextErr(ctx); err != nil {
			return EvaluationBatch{}, err
		}
		if experimentRun.RecordedAt.Before(window.StartInclusive) ||
			!experimentRun.RecordedAt.Before(window.EndExclusive) {
			continue
		}
		projected, err := source.projectUsageExperiment(scope, window, experimentRun)
		if err != nil {
			return EvaluationBatch{}, fmt.Errorf(
				"project experiment %q usage: %w", experimentRun.ExperimentRunID, err,
			)
		}
		facts = append(facts, projected...)
	}
	repeatabilityRuns, err := source.evaluations.ListRepeatabilityRuns(source.access)
	if err != nil {
		return EvaluationBatch{}, fmt.Errorf("list governed repeatability runs: %w", err)
	}
	repeatabilityFacts := make([]analytics.RepeatabilityFact, 0)
	for _, repeatabilityRun := range repeatabilityRuns {
		if err := contextErr(ctx); err != nil {
			return EvaluationBatch{}, err
		}
		if repeatabilityRun.RecordedAt.Before(window.StartInclusive) ||
			!repeatabilityRun.RecordedAt.Before(window.EndExclusive) {
			continue
		}
		projected, err := source.projectRepeatability(scope, window, repeatabilityRun)
		if err != nil {
			return EvaluationBatch{}, fmt.Errorf("project repeatability %q: %w", repeatabilityRun.RepeatabilityRunID, err)
		}
		repeatabilityFacts = append(repeatabilityFacts, projected...)
	}
	sort.Slice(facts, func(left, right int) bool { return facts[left].FactID < facts[right].FactID })
	sort.Slice(repeatabilityFacts, func(left, right int) bool {
		return repeatabilityFacts[left].FactID < repeatabilityFacts[right].FactID
	})
	batch := EvaluationBatch{
		Completeness:       analytics.CompletenessComplete,
		IncompleteReasons:  []string{},
		Facts:              facts,
		RepeatabilityFacts: repeatabilityFacts,
	}
	if err := batch.Validate(); err != nil {
		return EvaluationBatch{}, err
	}
	return batch, nil
}

type repeatabilityMetric struct {
	id, definition, unit string
	direction            analytics.MetricDirection
	amount               uint64
	availability         evaluation.MetricAvailability
}

func (source *EvaluationProjectionSource) projectRepeatability(
	scope Scope,
	window analytics.TimeWindow,
	run evaluation.RepeatabilityRun,
) ([]analytics.RepeatabilityFact, error) {
	runRef, err := analyticsSourceRef(
		analytics.SourceRepeatability, run.RepeatabilityRunID, run.RepeatabilityRevision, run,
	)
	if err != nil {
		return nil, err
	}
	evaluationIDs := append([]string{run.BaselineEvaluationRunID}, run.ReplayEvaluationRunIDs...)
	sourceRefs := []analytics.SourceRef{runRef}
	for _, id := range evaluationIDs {
		evaluationRun, err := source.evaluations.GetEvaluationRun(id, source.access)
		if err != nil {
			return nil, fmt.Errorf("load EvaluationRun %q: %w", id, err)
		}
		ref, err := analyticsSourceRef(
			analytics.SourceEvaluation, evaluationRun.EvaluationRunID,
			evaluationRun.EvaluatorRevision, evaluationRun,
		)
		if err != nil {
			return nil, err
		}
		sourceRefs = append(sourceRefs, ref)
	}
	sourceRefs = canonicalAnalyticsSourceRefs(sourceRefs)
	facts := make([]analytics.RepeatabilityFact, 0)
	for _, comparison := range run.Comparisons {
		baselineDimensions, matches, err := source.reviewRunDimensions(comparison.Samples[0].ReviewRunID, scope)
		if err != nil {
			return nil, err
		}
		for _, sample := range comparison.Samples[1:] {
			dimensions, sampleMatches, err := source.reviewRunDimensions(sample.ReviewRunID, scope)
			if err != nil {
				return nil, err
			}
			if sampleMatches != matches || dimensions != baselineDimensions {
				return nil, fmt.Errorf("case %q exact replay samples changed analytics dimensions", comparison.CaseID)
			}
		}
		if !matches {
			continue
		}
		caseMetrics := []repeatabilityMetric{
			{"finding_set_pairwise_jaccard", "Mean pairwise Jaccard similarity of stable Finding IDs across independently executed exact replays.", "ratio", analytics.HigherIsBetter, uint64(comparison.PairwiseJaccardPPM), comparison.FindingSetComparison},
			{"exact_finding_set_pair_rate", "Rate of independently executed sample pairs with exactly equal stable Finding ID sets.", "ratio", analytics.HigherIsBetter, uint64(comparison.ExactFindingSetPairRatePPM), comparison.FindingSetComparison},
			{"verdict_flip_pair_rate", "Rate of independently executed sample pairs whose committed evaluation verdict differs.", "ratio", analytics.LowerIsBetter, uint64(comparison.VerdictFlipPairRatePPM), evaluation.MetricAvailability{Available: true}},
			{"expected_anchor_hit_rate", "Rate of independently executed samples that hit at least one frozen expected anchor.", "ratio", analytics.HigherIsBetter, uint64(comparison.ExpectedAnchorHitRatePPM), comparison.ExpectedAnchorHit},
		}
		for _, metric := range caseMetrics {
			fact, err := buildRepeatabilityFact(run, comparison.CaseID, baselineDimensions, window, sourceRefs, uint64(comparison.SampleCount), metric)
			if err != nil {
				return nil, err
			}
			facts = append(facts, fact)
		}
		for _, dimension := range comparison.Dimensions {
			dimensions := baselineDimensions
			dimensions.ReviewDimension = dimension.Dimension.ID
			execution := dimension.ExecutionComparison
			metrics := []repeatabilityMetric{
				{"finding_set_pairwise_jaccard", "Mean pairwise Jaccard similarity of stable Finding IDs for the exact review dimension.", "ratio", analytics.HigherIsBetter, uint64(dimension.PairwiseJaccardPPM), dimension.FindingSetComparison},
				{"exact_finding_set_pair_rate", "Rate of exact Finding ID set equality for the exact review dimension.", "ratio", analytics.HigherIsBetter, uint64(dimension.ExactFindingSetPairRatePPM), dimension.FindingSetComparison},
				{"verdict_flip_pair_rate", "Pairwise committed verdict flip rate for the exact review dimension.", "ratio", analytics.LowerIsBetter, uint64(dimension.VerdictFlipPairRatePPM), evaluation.MetricAvailability{Available: true}},
				{"candidate_count_range", "Maximum minus minimum governed candidate count across exact-replay samples for the exact review dimension.", "candidates", analytics.LowerIsBetter, uint64(dimension.CandidateCountMax - dimension.CandidateCountMin), execution},
				{"raw_candidate_count_range", "Maximum minus minimum raw candidate count across exact-replay samples for the exact review dimension.", "candidates", analytics.LowerIsBetter, uint64(dimension.RawCandidateCountMax - dimension.RawCandidateCountMin), execution},
				{"context_gap_count_range", "Maximum minus minimum committed context gap count across exact-replay samples for the exact review dimension.", "gaps", analytics.LowerIsBetter, uint64(dimension.ContextGapCountMax - dimension.ContextGapCountMin), execution},
				{"failed_task_count_range", "Maximum minus minimum failed task count across exact-replay samples for the exact review dimension.", "tasks", analytics.LowerIsBetter, uint64(dimension.TasksFailedMax - dimension.TasksFailedMin), execution},
				{"cumulative_duration_ms_range", "Maximum minus minimum cumulative task duration across exact-replay samples for the exact review dimension.", "ms", analytics.LowerIsBetter, dimension.CumulativeDurationMSMax - dimension.CumulativeDurationMSMin, execution},
				{"total_tokens_range", "Maximum minus minimum diagnostic token total across exact-replay samples for the exact review dimension.", "tokens", analytics.LowerIsBetter, dimension.TotalTokensMax - dimension.TotalTokensMin, dimension.UsageComparison},
			}
			for _, metric := range metrics {
				fact, err := buildRepeatabilityFact(run, comparison.CaseID, dimensions, window, sourceRefs, uint64(dimension.SampleCount), metric)
				if err != nil {
					return nil, err
				}
				facts = append(facts, fact)
			}
		}
	}
	return facts, nil
}

func buildRepeatabilityFact(
	run evaluation.RepeatabilityRun,
	caseID string,
	dimensions analytics.Dimensions,
	window analytics.TimeWindow,
	sources []analytics.SourceRef,
	sampleSize uint64,
	metric repeatabilityMetric,
) (analytics.RepeatabilityFact, error) {
	if metric.amount > math.MaxInt64 {
		return analytics.RepeatabilityFact{}, fmt.Errorf("repeatability metric %q exceeds analytics int64", metric.id)
	}
	scale := uint8(0)
	if metric.unit == "ratio" {
		scale = evaluationRateScale
	}
	completeness := analytics.CompletenessComplete
	reasons := []string{}
	if !metric.availability.Available {
		completeness = analytics.CompletenessPartial
		reason := metric.availability.ReasonCode
		if reason == "" {
			reason = "repeatability_metric_unavailable"
		}
		reasons = []string{reason}
	}
	fact := analytics.RepeatabilityFact{
		SchemaVersion:      analytics.RepeatabilityFactSchemaVersion,
		FactID:             stableEvaluationFactID("repeatability", run.RepeatabilityRunID, caseID, dimensions.ReviewDimension, metric.id),
		RepeatabilityRunID: run.RepeatabilityRunID, RepeatabilityRevision: run.RepeatabilityRevision,
		CaseID: caseID, MetricID: metric.id, MetricVersion: evaluationRepeatabilityMetricVersion,
		MetricDefinition: metric.definition, Direction: metric.direction,
		Value:      analytics.FixedPoint{Amount: int64(metric.amount), Scale: scale, Unit: metric.unit},
		SampleSize: sampleSize, Window: window, ResultCompleteness: completeness,
		IncompleteReasons: reasons, Dimensions: dimensions,
		SourceRefs: slices.Clone(sources), OccurredAt: run.RecordedAt,
	}
	if err := fact.Validate(); err != nil {
		return analytics.RepeatabilityFact{}, err
	}
	return fact, nil
}

type usageExperimentGroup struct {
	dimensions                     analytics.Dimensions
	comparisons                    uint64
	complete                       bool
	dimensionExecutionComplete     bool
	dimensionUsageComplete         bool
	dimensionNormalizationComplete bool
	dimensionContextComplete       bool
	baselineInput                  uint64
	variantInput                   uint64
	baselineOutput                 uint64
	variantOutput                  uint64
	baselineTotal                  uint64
	variantTotal                   uint64
	baselineDuration               uint64
	variantDuration                uint64
	baselineRawCandidates          uint64
	variantRawCandidates           uint64
	baselineMergedDuplicates       uint64
	variantMergedDuplicates        uint64
	baselineRejectedInvalid        uint64
	variantRejectedInvalid         uint64
	baselineExcludedBudget         uint64
	variantExcludedBudget          uint64
	baselineContextGaps            uint64
	variantContextGaps             uint64
	baselineSources                []analytics.SourceRef
	variantSources                 []analytics.SourceRef
}

func (source *EvaluationProjectionSource) projectUsageExperiment(
	scope Scope,
	window analytics.TimeWindow,
	experimentRun evaluation.ExperimentRun,
) ([]analytics.ExperimentFact, error) {
	baselineEvaluation, err := source.evaluations.GetEvaluationRun(
		experimentRun.BaselineEvaluationRunID, source.access,
	)
	if err != nil {
		return nil, fmt.Errorf("load baseline EvaluationRun: %w", err)
	}
	variantEvaluation, err := source.evaluations.GetEvaluationRun(
		experimentRun.VariantEvaluationRunID, source.access,
	)
	if err != nil {
		return nil, fmt.Errorf("load variant EvaluationRun: %w", err)
	}
	experimentRef, err := analyticsSourceRef(
		analytics.SourceExperiment,
		experimentRun.ExperimentRunID,
		experimentRun.ExperimentRevision,
		experimentRun,
	)
	if err != nil {
		return nil, err
	}
	baselineEvaluationRef, err := analyticsSourceRef(
		analytics.SourceEvaluation,
		baselineEvaluation.EvaluationRunID,
		baselineEvaluation.EvaluatorRevision,
		baselineEvaluation,
	)
	if err != nil {
		return nil, err
	}
	variantEvaluationRef, err := analyticsSourceRef(
		analytics.SourceEvaluation,
		variantEvaluation.EvaluationRunID,
		variantEvaluation.EvaluatorRevision,
		variantEvaluation,
	)
	if err != nil {
		return nil, err
	}

	groups := make(map[string]*usageExperimentGroup)
	for _, comparison := range experimentRun.Comparisons {
		baselineDimensions, baselineMatches, err := source.reviewRunDimensions(
			comparison.BaselineReviewRunID, scope,
		)
		if err != nil {
			return nil, err
		}
		_, variantMatches, err := source.reviewRunDimensions(
			comparison.VariantReviewRunID, scope,
		)
		if err != nil {
			return nil, err
		}
		if baselineMatches != variantMatches {
			return nil, fmt.Errorf("baseline and variant ReviewRun scope differ")
		}
		if !baselineMatches {
			continue
		}
		key := strings.Join([]string{
			baselineDimensions.TenantID,
			baselineDimensions.OrganizationID,
			baselineDimensions.RepositoryID,
			baselineDimensions.WorkflowRevision,
			baselineDimensions.ConfigRevision,
		}, "\x00")
		group := groups[key]
		if group == nil {
			group = &usageExperimentGroup{
				dimensions:      baselineDimensions,
				complete:        true,
				baselineSources: []analytics.SourceRef{experimentRef, baselineEvaluationRef},
				variantSources:  []analytics.SourceRef{experimentRef, variantEvaluationRef},
			}
			groups[key] = group
		}
		group.comparisons++
		group.baselineSources = append(group.baselineSources, analytics.SourceRef{
			Kind: analytics.SourceReviewRun, ID: comparison.BaselineReviewRunID,
			Revision: runmodel.RunSchemaVersion, SHA256: comparison.BaselineReviewRunRef.SHA256,
		})
		group.variantSources = append(group.variantSources, analytics.SourceRef{
			Kind: analytics.SourceReviewRun, ID: comparison.VariantReviewRunID,
			Revision: runmodel.RunSchemaVersion, SHA256: comparison.VariantReviewRunRef.SHA256,
		})
		if !comparison.BaselineUsage.Available || !comparison.VariantUsage.Available {
			group.complete = false
			continue
		}
		for target, value := range map[*uint64]uint64{
			&group.baselineInput:  comparison.BaselineInputTokens,
			&group.variantInput:   comparison.VariantInputTokens,
			&group.baselineOutput: comparison.BaselineOutputTokens,
			&group.variantOutput:  comparison.VariantOutputTokens,
			&group.baselineTotal:  comparison.BaselineTotalTokens,
			&group.variantTotal:   comparison.VariantTotalTokens,
		} {
			if value > math.MaxUint64-*target {
				return nil, fmt.Errorf("token aggregation overflows uint64")
			}
			*target += value
		}
		for _, dimension := range comparison.Dimensions {
			dimensionValues := baselineDimensions
			dimensionValues.ReviewDimension = dimension.DimensionID
			dimensionKey := strings.Join([]string{
				dimensionValues.TenantID,
				dimensionValues.OrganizationID,
				dimensionValues.RepositoryID,
				dimensionValues.WorkflowRevision,
				dimensionValues.ConfigRevision,
				dimensionValues.ReviewDimension,
			}, "\x00")
			dimensionGroup := groups[dimensionKey]
			if dimensionGroup == nil {
				dimensionGroup = &usageExperimentGroup{
					dimensions:                     dimensionValues,
					dimensionExecutionComplete:     true,
					dimensionUsageComplete:         true,
					dimensionNormalizationComplete: true,
					dimensionContextComplete:       true,
					baselineSources:                []analytics.SourceRef{experimentRef, baselineEvaluationRef},
					variantSources:                 []analytics.SourceRef{experimentRef, variantEvaluationRef},
				}
				groups[dimensionKey] = dimensionGroup
			}
			dimensionGroup.comparisons++
			dimensionGroup.baselineSources = append(
				dimensionGroup.baselineSources,
				analytics.SourceRef{
					Kind: analytics.SourceReviewRun, ID: comparison.BaselineReviewRunID,
					Revision: runmodel.RunSchemaVersion, SHA256: comparison.BaselineReviewRunRef.SHA256,
				},
			)
			dimensionGroup.variantSources = append(
				dimensionGroup.variantSources,
				analytics.SourceRef{
					Kind: analytics.SourceReviewRun, ID: comparison.VariantReviewRunID,
					Revision: runmodel.RunSchemaVersion, SHA256: comparison.VariantReviewRunRef.SHA256,
				},
			)
			if !dimension.BaselineExecutionCoverage.Available ||
				!dimension.VariantExecutionCoverage.Available {
				dimensionGroup.dimensionExecutionComplete = false
			}
			if !dimension.Usage.Available {
				dimensionGroup.dimensionUsageComplete = false
			} else {
				if err := addEvaluationMetric(
					&dimensionGroup.baselineTotal, dimension.BaselineTotalTokens,
				); err != nil {
					return nil, err
				}
				if err := addEvaluationMetric(
					&dimensionGroup.variantTotal, dimension.VariantTotalTokens,
				); err != nil {
					return nil, err
				}
			}
			if !dimension.Normalization.Available {
				dimensionGroup.dimensionNormalizationComplete = false
			} else {
				for target, value := range map[*uint64]uint32{
					&dimensionGroup.baselineRawCandidates:    dimension.BaselineRawCandidateCount,
					&dimensionGroup.variantRawCandidates:     dimension.VariantRawCandidateCount,
					&dimensionGroup.baselineMergedDuplicates: dimension.BaselineMergedDuplicateCount,
					&dimensionGroup.variantMergedDuplicates:  dimension.VariantMergedDuplicateCount,
					&dimensionGroup.baselineRejectedInvalid:  dimension.BaselineRejectedInvalidCount,
					&dimensionGroup.variantRejectedInvalid:   dimension.VariantRejectedInvalidCount,
					&dimensionGroup.baselineExcludedBudget:   dimension.BaselineExcludedBudgetCount,
					&dimensionGroup.variantExcludedBudget:    dimension.VariantExcludedBudgetCount,
				} {
					if err := addEvaluationMetric(target, uint64(value)); err != nil {
						return nil, err
					}
				}
			}
			if !dimension.ContextCoverage.Available {
				dimensionGroup.dimensionContextComplete = false
			} else {
				if err := addEvaluationMetric(
					&dimensionGroup.baselineContextGaps, uint64(dimension.BaselineContextGapCount),
				); err != nil {
					return nil, err
				}
				if err := addEvaluationMetric(
					&dimensionGroup.variantContextGaps, uint64(dimension.VariantContextGapCount),
				); err != nil {
					return nil, err
				}
			}
			if err := addEvaluationMetric(
				&dimensionGroup.baselineDuration, dimension.BaselineCumulativeDurationMS,
			); err != nil {
				return nil, err
			}
			if err := addEvaluationMetric(
				&dimensionGroup.variantDuration, dimension.VariantCumulativeDurationMS,
			); err != nil {
				return nil, err
			}
		}
	}

	facts := make([]analytics.ExperimentFact, 0, len(groups)*6)
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		if group.dimensions.ReviewDimension != "" {
			metricPairs := []struct {
				id            string
				definition    string
				unit          string
				baseline      uint64
				variant       uint64
				complete      bool
				partialReason string
			}{
				{
					"review_dimension_total_tokens",
					"Diagnostic tokens attributable to the exact review dimension; shared context is excluded.",
					"tokens", group.baselineTotal, group.variantTotal,
					group.dimensionExecutionComplete && group.dimensionUsageComplete,
					"review_dimension_execution_or_usage_incomplete",
				},
				{
					"review_dimension_cumulative_duration_ms",
					"Cumulative review and occurrence-joined verification duration for the exact review dimension.",
					"ms", group.baselineDuration, group.variantDuration,
					group.dimensionExecutionComplete, "review_dimension_execution_incomplete",
				},
				{
					"review_dimension_merged_duplicate_count",
					"Raw candidates merged into an existing canonical occurrence for the exact review dimension.",
					"candidates", group.baselineMergedDuplicates, group.variantMergedDuplicates,
					group.dimensionNormalizationComplete, "review_dimension_normalization_incomplete",
				},
				{
					"review_dimension_rejected_invalid_count",
					"Raw candidates rejected by host normalization as invalid for the exact review dimension.",
					"candidates", group.baselineRejectedInvalid, group.variantRejectedInvalid,
					group.dimensionNormalizationComplete, "review_dimension_normalization_incomplete",
				},
				{
					"review_dimension_excluded_budget_count",
					"Valid normalized candidates excluded by the frozen candidate budget for the exact review dimension.",
					"candidates", group.baselineExcludedBudget, group.variantExcludedBudget,
					group.dimensionNormalizationComplete, "review_dimension_normalization_incomplete",
				},
				{
					"review_dimension_context_gap_count",
					"Committed context coverage gaps applicable to executed review groups for the exact review dimension.",
					"gaps", group.baselineContextGaps, group.variantContextGaps,
					group.dimensionContextComplete, "review_dimension_context_coverage_incomplete",
				},
			}
			for _, metric := range metricPairs {
				pair, err := dimensionExperimentFactPair(
					experimentRun, window, group, metric.id, metric.definition,
					metric.unit, metric.baseline, metric.variant,
					metric.complete, metric.partialReason,
				)
				if err != nil {
					return nil, err
				}
				facts = append(facts, pair...)
			}
			ratePairs := []struct {
				id         string
				definition string
				baseline   uint64
				variant    uint64
			}{
				{
					"review_dimension_merged_duplicate_rate",
					"Raw merged-duplicate candidates divided by all raw candidates for the exact review dimension; an operational redundancy signal, not standalone review quality.",
					group.baselineMergedDuplicates, group.variantMergedDuplicates,
				},
				{
					"review_dimension_rejected_invalid_rate",
					"Host-rejected invalid candidates divided by all raw candidates for the exact review dimension.",
					group.baselineRejectedInvalid, group.variantRejectedInvalid,
				},
				{
					"review_dimension_excluded_budget_rate",
					"Budget-excluded candidates divided by all raw candidates for the exact review dimension.",
					group.baselineExcludedBudget, group.variantExcludedBudget,
				},
			}
			for _, metric := range ratePairs {
				pair, err := dimensionRateExperimentFactPair(
					experimentRun, window, group, metric.id, metric.definition,
					metric.baseline, metric.variant,
				)
				if err != nil {
					return nil, err
				}
				facts = append(facts, pair...)
			}
			continue
		}
		metrics := []struct {
			id         string
			definition string
			baseline   uint64
			variant    uint64
		}{
			{"input_tokens", "Total diagnostic input tokens across the exact experiment case set.", group.baselineInput, group.variantInput},
			{"output_tokens", "Total diagnostic output tokens across the exact experiment case set.", group.baselineOutput, group.variantOutput},
			{"total_tokens", "Total diagnostic input, output, and cache tokens across the exact experiment case set.", group.baselineTotal, group.variantTotal},
		}
		for _, metric := range metrics {
			pair, err := usageExperimentFactPair(
				experimentRun, window, group, metric.id, metric.definition,
				metric.baseline, metric.variant,
			)
			if err != nil {
				return nil, err
			}
			facts = append(facts, pair...)
		}
	}
	return facts, nil
}

func addEvaluationMetric(target *uint64, value uint64) error {
	if value > math.MaxUint64-*target {
		return fmt.Errorf("dimension metric aggregation overflows uint64")
	}
	*target += value
	return nil
}

func (source *EvaluationProjectionSource) reviewRunDimensions(
	runID string,
	scope Scope,
) (analytics.Dimensions, bool, error) {
	run, err := source.runs.LoadRun(runID)
	if err != nil {
		return analytics.Dimensions{}, false, fmt.Errorf("load ReviewRun %q: %w", runID, err)
	}
	snapshot, err := source.runs.ExecutionSnapshotForRun(runID)
	if err != nil {
		return analytics.Dimensions{}, false, fmt.Errorf("load ReviewRun %q snapshot: %w", runID, err)
	}
	specBytes, err := source.runs.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return analytics.Dimensions{}, false, fmt.Errorf("read ReviewRun %q spec: %w", runID, err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specBytes)
	if err != nil {
		return analytics.Dimensions{}, false, fmt.Errorf("decode ReviewRun %q spec: %w", runID, err)
	}
	if run.RunID != runID || run.ExecutionSnapshotID != snapshot.ExecutionSnapshotID {
		return analytics.Dimensions{}, false, fmt.Errorf("ReviewRun %q snapshot binding changed", runID)
	}
	dimensions := analytics.Dimensions{
		TenantID: spec.TenantID, OrganizationID: "local",
		RepositoryID:     spec.Repository.RepositoryID,
		WorkflowRevision: snapshot.Workflow.Revision,
		ConfigRevision:   snapshot.Config.Revision,
	}
	matches := dimensions.TenantID == scope.TenantID &&
		dimensions.OrganizationID == scope.OrganizationID &&
		dimensions.RepositoryID == scope.RepositoryID
	return dimensions, matches, nil
}

func usageExperimentFactPair(
	experimentRun evaluation.ExperimentRun,
	window analytics.TimeWindow,
	group *usageExperimentGroup,
	metricID string,
	definition string,
	baseline uint64,
	variant uint64,
) ([]analytics.ExperimentFact, error) {
	if baseline > math.MaxInt64 || variant > math.MaxInt64 {
		return nil, fmt.Errorf("%s token total exceeds analytics int64", metricID)
	}
	completeness := analytics.CompletenessComplete
	reasons := []string{}
	if !group.complete {
		completeness = analytics.CompletenessPartial
		reasons = []string{"worker_reported_usage_incomplete"}
	}
	baselineSources := canonicalAnalyticsSourceRefs(group.baselineSources)
	variantSources := canonicalAnalyticsSourceRefs(group.variantSources)
	base := analytics.ExperimentFact{
		SchemaVersion:      analytics.ExperimentFactSchemaVersion,
		ExperimentID:       experimentRun.ExperimentRunID,
		ExperimentRevision: experimentRun.ExperimentRevision,
		MetricID:           metricID, MetricVersion: evaluationUsageMetricVersion,
		MetricDefinition: definition, Direction: analytics.LowerIsBetter,
		SampleSize: group.comparisons, Window: window,
		ResultCompleteness: completeness, IncompleteReasons: slices.Clone(reasons),
		Dimensions: group.dimensions, OccurredAt: experimentRun.RecordedAt,
	}
	baselineFact := base
	baselineFact.Arm = analytics.ExperimentBaseline
	baselineFact.VariantID = "baseline"
	baselineFact.Value = analytics.FixedPoint{Amount: int64(baseline), Unit: "tokens"}
	baselineFact.SourceRefs = baselineSources
	baselineFact.FactID = stableEvaluationFactID(
		experimentRun.ExperimentRunID, metricID, "baseline", dimensionsKey(group.dimensions),
	)
	variantFact := base
	variantFact.Arm = analytics.ExperimentVariant
	variantFact.VariantID = experimentRun.VariantEvaluationRunID
	variantFact.Value = analytics.FixedPoint{Amount: int64(variant), Unit: "tokens"}
	variantFact.SourceRefs = variantSources
	variantFact.FactID = stableEvaluationFactID(
		experimentRun.ExperimentRunID, metricID, variantFact.VariantID, dimensionsKey(group.dimensions),
	)
	return []analytics.ExperimentFact{baselineFact, variantFact}, nil
}

func dimensionExperimentFactPair(
	experimentRun evaluation.ExperimentRun,
	window analytics.TimeWindow,
	group *usageExperimentGroup,
	metricID string,
	definition string,
	unit string,
	baseline uint64,
	variant uint64,
	complete bool,
	partialReason string,
) ([]analytics.ExperimentFact, error) {
	if baseline > math.MaxInt64 || variant > math.MaxInt64 {
		return nil, fmt.Errorf("%s total exceeds analytics int64", metricID)
	}
	completeness := analytics.CompletenessComplete
	reasons := []string{}
	if !complete {
		completeness = analytics.CompletenessPartial
		reasons = []string{partialReason}
	}
	base := analytics.ExperimentFact{
		SchemaVersion: analytics.ExperimentFactSchemaVersion,
		ExperimentID:  experimentRun.ExperimentRunID, ExperimentRevision: experimentRun.ExperimentRevision,
		MetricID: metricID, MetricVersion: evaluationDimensionMetricVersion,
		MetricDefinition: definition, Direction: analytics.LowerIsBetter,
		SampleSize: group.comparisons, Window: window,
		ResultCompleteness: completeness, IncompleteReasons: slices.Clone(reasons),
		Dimensions: group.dimensions, OccurredAt: experimentRun.RecordedAt,
	}
	baselineFact := base
	baselineFact.Arm = analytics.ExperimentBaseline
	baselineFact.VariantID = "baseline"
	baselineFact.Value = analytics.FixedPoint{Amount: int64(baseline), Unit: unit}
	baselineFact.SourceRefs = canonicalAnalyticsSourceRefs(group.baselineSources)
	baselineFact.FactID = stableEvaluationFactID(
		experimentRun.ExperimentRunID, metricID, "baseline", dimensionsKey(group.dimensions),
	)
	variantFact := base
	variantFact.Arm = analytics.ExperimentVariant
	variantFact.VariantID = experimentRun.VariantEvaluationRunID
	variantFact.Value = analytics.FixedPoint{Amount: int64(variant), Unit: unit}
	variantFact.SourceRefs = canonicalAnalyticsSourceRefs(group.variantSources)
	variantFact.FactID = stableEvaluationFactID(
		experimentRun.ExperimentRunID, metricID, variantFact.VariantID, dimensionsKey(group.dimensions),
	)
	return []analytics.ExperimentFact{baselineFact, variantFact}, nil
}

func dimensionRateExperimentFactPair(
	experimentRun evaluation.ExperimentRun,
	window analytics.TimeWindow,
	group *usageExperimentGroup,
	metricID string,
	definition string,
	baselineNumerator uint64,
	variantNumerator uint64,
) ([]analytics.ExperimentFact, error) {
	if baselineNumerator > group.baselineRawCandidates ||
		variantNumerator > group.variantRawCandidates {
		return nil, fmt.Errorf("%s numerator exceeds raw candidate denominator", metricID)
	}
	baselineAmount, baselineDenominator := scaledEvaluationRate(
		baselineNumerator, group.baselineRawCandidates,
	)
	variantAmount, variantDenominator := scaledEvaluationRate(
		variantNumerator, group.variantRawCandidates,
	)
	base := analytics.ExperimentFact{
		SchemaVersion: analytics.ExperimentFactSchemaVersion,
		ExperimentID:  experimentRun.ExperimentRunID, ExperimentRevision: experimentRun.ExperimentRevision,
		MetricID: metricID, MetricVersion: evaluationDimensionRateVersion,
		MetricDefinition: definition, Direction: analytics.LowerIsBetter,
		SampleSize: group.comparisons, Window: window,
		Dimensions: group.dimensions, OccurredAt: experimentRun.RecordedAt,
	}
	build := func(
		arm analytics.ExperimentArm,
		variantID string,
		amount int64,
		denominatorAvailable bool,
		sources []analytics.SourceRef,
	) analytics.ExperimentFact {
		fact := base
		fact.Arm = arm
		fact.VariantID = variantID
		fact.Value = analytics.FixedPoint{Amount: amount, Scale: evaluationRateScale, Unit: "ratio"}
		fact.ResultCompleteness = analytics.CompletenessComplete
		fact.IncompleteReasons = []string{}
		if !group.dimensionNormalizationComplete {
			fact.ResultCompleteness = analytics.CompletenessPartial
			fact.IncompleteReasons = []string{"review_dimension_normalization_incomplete"}
		} else if !denominatorAvailable {
			fact.ResultCompleteness = analytics.CompletenessPartial
			fact.IncompleteReasons = []string{"review_dimension_raw_candidate_denominator_zero"}
		}
		fact.SourceRefs = canonicalAnalyticsSourceRefs(sources)
		fact.FactID = stableEvaluationFactID(
			experimentRun.ExperimentRunID, metricID, variantID, dimensionsKey(group.dimensions),
		)
		return fact
	}
	return []analytics.ExperimentFact{
		build(
			analytics.ExperimentBaseline, "baseline", baselineAmount,
			baselineDenominator, group.baselineSources,
		),
		build(
			analytics.ExperimentVariant, experimentRun.VariantEvaluationRunID, variantAmount,
			variantDenominator, group.variantSources,
		),
	}, nil
}

func scaledEvaluationRate(numerator uint64, denominator uint64) (int64, bool) {
	if denominator == 0 {
		return 0, false
	}
	const scale = uint64(1_000_000)
	// numerator is a subset of denominator for every normalization fate, so
	// quotient/remainder arithmetic avoids multiplication overflow.
	quotient := numerator / denominator
	remainder := numerator % denominator
	return int64(quotient*scale + remainder*scale/denominator), true
}

func analyticsSourceRef(
	kind analytics.SourceKind,
	id string,
	revision string,
	value any,
) (analytics.SourceRef, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return analytics.SourceRef{}, err
	}
	digest := sha256.Sum256(data)
	return analytics.SourceRef{
		Kind: kind, ID: id, Revision: revision, SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func canonicalAnalyticsSourceRefs(refs []analytics.SourceRef) []analytics.SourceRef {
	result := slices.Clone(refs)
	sort.Slice(result, func(left, right int) bool {
		return analyticsSourceRefKey(result[left]) < analyticsSourceRefKey(result[right])
	})
	return slices.CompactFunc(result, func(left, right analytics.SourceRef) bool { return left == right })
}

func analyticsSourceRefKey(ref analytics.SourceRef) string {
	return strings.Join([]string{string(ref.Kind), ref.ID, ref.Revision, ref.SHA256}, "\x00")
}

func dimensionsKey(dimensions analytics.Dimensions) string {
	return strings.Join([]string{
		dimensions.TenantID, dimensions.OrganizationID, dimensions.RepositoryID,
		dimensions.WorkflowRevision, dimensions.ConfigRevision, dimensions.ReviewDimension,
	}, "\x00")
}

func stableEvaluationFactID(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}
