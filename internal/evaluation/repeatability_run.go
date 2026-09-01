package evaluation

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	RepeatabilityRunRequestSchemaVersion        = "argus.repeatability_run_request.v1alpha1"
	RepeatabilityRunSchemaVersion               = "argus.repeatability_run.v1alpha1"
	repeatabilityRunEventSchemaVersion          = "argus.repeatability_run_event.v1alpha1"
	repeatabilityRunStream                      = "evaluation/repeatability-runs"
	repeatabilityRateScale               uint32 = 1_000_000
	maxRepeatabilitySamples                     = 32
)

// RepeatabilityRunRequest compares one baseline EvaluationRun with independently
// executed, direct exact replays. ReplayEvaluationRunIDs is sorted so request
// identity does not depend on caller ordering.
type RepeatabilityRunRequest struct {
	SchemaVersion           string    `json:"schema_version"`
	RepeatabilityRunID      string    `json:"repeatability_run_id"`
	RepeatabilityRevision   string    `json:"repeatability_revision"`
	BaselineEvaluationRunID string    `json:"baseline_evaluation_run_id"`
	ReplayEvaluationRunIDs  []string  `json:"replay_evaluation_run_ids"`
	CreatedAt               time.Time `json:"created_at"`
}

type RepeatabilitySample struct {
	EvaluationRunID    string               `json:"evaluation_run_id"`
	ReviewRunID        string               `json:"review_run_id"`
	ReviewRunRef       runmodel.ArtifactRef `json:"review_run_ref"`
	GovernedReportRef  runmodel.ArtifactRef `json:"governed_report_ref"`
	ReportCompleteness string               `json:"report_completeness"`
	FindingIDs         []string             `json:"finding_ids"`
	FindingCount       uint32               `json:"finding_count"`
	MatchedAnchorCount uint32               `json:"matched_anchor_count"`
	Localization       MetricAvailability   `json:"localization"`
	Verdict            EvaluationVerdict    `json:"verdict"`
}

type RepeatabilityDimensionSample struct {
	EvaluationRunID      string                         `json:"evaluation_run_id"`
	ReviewRunID          string                         `json:"review_run_id"`
	Dimension            contractsv1alpha1.VersionedRef `json:"dimension"`
	ReportCompleteness   string                         `json:"report_completeness"`
	FindingIDs           []string                       `json:"finding_ids"`
	FindingCount         uint32                         `json:"finding_count"`
	CandidateCount       uint32                         `json:"candidate_count"`
	RawCandidateCount    uint32                         `json:"raw_candidate_count"`
	ContextGapCount      uint32                         `json:"context_gap_count"`
	TasksSucceeded       uint32                         `json:"tasks_succeeded"`
	TasksFailed          uint32                         `json:"tasks_failed"`
	TasksCanceled        uint32                         `json:"tasks_canceled"`
	CumulativeDurationMS uint64                         `json:"cumulative_duration_ms"`
	TotalTokens          uint64                         `json:"total_tokens"`
	ExecutionCoverage    MetricAvailability             `json:"execution_coverage"`
	Usage                MetricAvailability             `json:"usage"`
	Verdict              EvaluationVerdict              `json:"verdict"`
}

type RepeatabilityDimensionComparison struct {
	Dimension                  contractsv1alpha1.VersionedRef `json:"dimension"`
	Samples                    []RepeatabilityDimensionSample `json:"samples"`
	SampleCount                uint32                         `json:"sample_count"`
	PairCount                  uint32                         `json:"pair_count"`
	FindingSetComparison       MetricAvailability             `json:"finding_set_comparison"`
	PairwiseJaccardPPM         uint32                         `json:"pairwise_jaccard_ppm"`
	ExactFindingSetPairCount   uint32                         `json:"exact_finding_set_pair_count"`
	ExactFindingSetPairRatePPM uint32                         `json:"exact_finding_set_pair_rate_ppm"`
	FindingPresenceSampleCount uint32                         `json:"finding_presence_sample_count"`
	FindingPresenceRatePPM     uint32                         `json:"finding_presence_rate_ppm"`
	VerdictFlipPairCount       uint32                         `json:"verdict_flip_pair_count"`
	VerdictFlipPairRatePPM     uint32                         `json:"verdict_flip_pair_rate_ppm"`
	CandidateCountMin          uint32                         `json:"candidate_count_min"`
	CandidateCountMax          uint32                         `json:"candidate_count_max"`
	RawCandidateCountMin       uint32                         `json:"raw_candidate_count_min"`
	RawCandidateCountMax       uint32                         `json:"raw_candidate_count_max"`
	ContextGapCountMin         uint32                         `json:"context_gap_count_min"`
	ContextGapCountMax         uint32                         `json:"context_gap_count_max"`
	TasksFailedMin             uint32                         `json:"tasks_failed_min"`
	TasksFailedMax             uint32                         `json:"tasks_failed_max"`
	CumulativeDurationMSMin    uint64                         `json:"cumulative_duration_ms_min"`
	CumulativeDurationMSMax    uint64                         `json:"cumulative_duration_ms_max"`
	TotalTokensMin             uint64                         `json:"total_tokens_min"`
	TotalTokensMax             uint64                         `json:"total_tokens_max"`
	ExecutionComparison        MetricAvailability             `json:"execution_comparison"`
	UsageComparison            MetricAvailability             `json:"usage_comparison"`
	Stable                     bool                           `json:"stable"`
}

type RepeatabilityCaseComparison struct {
	CaseID                       string                             `json:"case_id"`
	CaseType                     CaseType                           `json:"case_type"`
	LabelRevision                uint64                             `json:"label_revision"`
	LabelPolicyRevision          string                             `json:"label_policy_revision"`
	ExpectedOutcome              ExpectedOutcome                    `json:"expected_outcome"`
	ExpectedCategory             string                             `json:"expected_category,omitempty"`
	ExpectedAnchorCount          uint32                             `json:"expected_anchor_count"`
	Samples                      []RepeatabilitySample              `json:"samples"`
	Dimensions                   []RepeatabilityDimensionComparison `json:"dimensions"`
	SampleCount                  uint32                             `json:"sample_count"`
	PairCount                    uint32                             `json:"pair_count"`
	FindingSetComparison         MetricAvailability                 `json:"finding_set_comparison"`
	PairwiseJaccardPPM           uint32                             `json:"pairwise_jaccard_ppm"`
	ExactFindingSetPairCount     uint32                             `json:"exact_finding_set_pair_count"`
	ExactFindingSetPairRatePPM   uint32                             `json:"exact_finding_set_pair_rate_ppm"`
	FindingPresenceSampleCount   uint32                             `json:"finding_presence_sample_count"`
	FindingPresenceRatePPM       uint32                             `json:"finding_presence_rate_ppm"`
	ExpectedAnchorHit            MetricAvailability                 `json:"expected_anchor_hit"`
	ExpectedAnchorHitSampleCount uint32                             `json:"expected_anchor_hit_sample_count"`
	ExpectedAnchorHitRatePPM     uint32                             `json:"expected_anchor_hit_rate_ppm"`
	FindingCountMin              uint32                             `json:"finding_count_min"`
	FindingCountMax              uint32                             `json:"finding_count_max"`
	VerdictFlipPairCount         uint32                             `json:"verdict_flip_pair_count"`
	VerdictFlipPairRatePPM       uint32                             `json:"verdict_flip_pair_rate_ppm"`
	Stable                       bool                               `json:"stable"`
}

type RepeatabilityRunSummary struct {
	Cases                      uint32             `json:"cases"`
	SamplesPerCase             uint32             `json:"samples_per_case"`
	PairsPerCase               uint32             `json:"pairs_per_case"`
	StableCases                uint32             `json:"stable_cases"`
	UnstableCases              uint32             `json:"unstable_cases"`
	IndeterminateCases         uint32             `json:"indeterminate_cases"`
	FindingSetStability        MetricAvailability `json:"finding_set_stability"`
	PairwiseJaccardPPM         uint32             `json:"pairwise_jaccard_ppm"`
	ExactFindingSetPairCount   uint64             `json:"exact_finding_set_pair_count"`
	ExactFindingSetPairRatePPM uint32             `json:"exact_finding_set_pair_rate_ppm"`
	VerdictFlipPairCount       uint64             `json:"verdict_flip_pair_count"`
	VerdictFlipPairRatePPM     uint32             `json:"verdict_flip_pair_rate_ppm"`
}

type RepeatabilityRun struct {
	SchemaVersion           string                        `json:"schema_version"`
	RepeatabilityRunID      string                        `json:"repeatability_run_id"`
	RepeatabilityRevision   string                        `json:"repeatability_revision"`
	BaselineEvaluationRunID string                        `json:"baseline_evaluation_run_id"`
	ReplayEvaluationRunIDs  []string                      `json:"replay_evaluation_run_ids"`
	EvaluatorRevision       string                        `json:"evaluator_revision"`
	Comparisons             []RepeatabilityCaseComparison `json:"comparisons"`
	Summary                 RepeatabilityRunSummary       `json:"summary"`
	CreatedAt               time.Time                     `json:"created_at"`
	RecordedAt              time.Time                     `json:"recorded_at"`
	RecordedBy              string                        `json:"recorded_by"`
}

func (request RepeatabilityRunRequest) Validate() error {
	if request.SchemaVersion != RepeatabilityRunRequestSchemaVersion {
		return fmt.Errorf("unsupported repeatability run request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"repeatability_run_id":       request.RepeatabilityRunID,
		"repeatability_revision":     request.RepeatabilityRevision,
		"baseline_evaluation_run_id": request.BaselineEvaluationRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.ReplayEvaluationRunIDs == nil || len(request.ReplayEvaluationRunIDs) == 0 {
		return fmt.Errorf("replay_evaluation_run_ids must be a non-empty array")
	}
	if len(request.ReplayEvaluationRunIDs)+1 > maxRepeatabilitySamples {
		return fmt.Errorf("repeatability run supports at most %d samples", maxRepeatabilitySamples)
	}
	previous := ""
	for index, id := range request.ReplayEvaluationRunIDs {
		if err := validateID("replay_evaluation_run_ids", id); err != nil {
			return fmt.Errorf("replay_evaluation_run_ids[%d]: %w", index, err)
		}
		if id == request.BaselineEvaluationRunID {
			return fmt.Errorf("baseline evaluation run cannot also be a replay sample")
		}
		if index > 0 && id <= previous {
			return fmt.Errorf("replay_evaluation_run_ids must be uniquely sorted")
		}
		previous = id
	}
	if request.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func (run RepeatabilityRun) Validate() error {
	if run.SchemaVersion != RepeatabilityRunSchemaVersion {
		return fmt.Errorf("unsupported repeatability run schema %q", run.SchemaVersion)
	}
	request := RepeatabilityRunRequest{
		SchemaVersion:           RepeatabilityRunRequestSchemaVersion,
		RepeatabilityRunID:      run.RepeatabilityRunID,
		RepeatabilityRevision:   run.RepeatabilityRevision,
		BaselineEvaluationRunID: run.BaselineEvaluationRunID,
		ReplayEvaluationRunIDs:  run.ReplayEvaluationRunIDs,
		CreatedAt:               run.CreatedAt,
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
	previous := ""
	for index, comparison := range run.Comparisons {
		if err := comparison.validate(); err != nil {
			return fmt.Errorf("comparisons[%d]: %w", index, err)
		}
		if len(comparison.Samples) != len(run.ReplayEvaluationRunIDs)+1 ||
			comparison.Samples[0].EvaluationRunID != run.BaselineEvaluationRunID {
			return fmt.Errorf("comparison %q does not bind the declared sample runs", comparison.CaseID)
		}
		for sampleIndex, id := range run.ReplayEvaluationRunIDs {
			if comparison.Samples[sampleIndex+1].EvaluationRunID != id {
				return fmt.Errorf("comparison %q changed replay sample ordering", comparison.CaseID)
			}
		}
		if index > 0 && comparison.CaseID <= previous {
			return fmt.Errorf("comparisons must be uniquely sorted by case_id")
		}
		previous = comparison.CaseID
	}
	if run.Summary != summarizeRepeatability(run.Comparisons) {
		return fmt.Errorf("summary is not the exact repeatability recomputation")
	}
	if run.RecordedAt.IsZero() || run.RecordedAt.Before(run.CreatedAt) {
		return fmt.Errorf("non-decreasing recorded_at is required")
	}
	return validateText("recorded_by", run.RecordedBy, 256, false)
}

func (comparison RepeatabilityCaseComparison) validate() error {
	if err := validateID("case_id", comparison.CaseID); err != nil {
		return err
	}
	if err := comparison.CaseType.Validate(); err != nil {
		return err
	}
	if comparison.LabelRevision == 0 {
		return fmt.Errorf("label_revision must be positive")
	}
	if err := validateID("label_policy_revision", comparison.LabelPolicyRevision); err != nil {
		return err
	}
	if err := comparison.ExpectedOutcome.Validate(); err != nil {
		return err
	}
	if comparison.Samples == nil || len(comparison.Samples) < 2 {
		return fmt.Errorf("samples must contain a baseline and at least one replay")
	}
	seenEvaluationRuns := make(map[string]struct{}, len(comparison.Samples))
	seenReviewRuns := make(map[string]struct{}, len(comparison.Samples))
	for index, sample := range comparison.Samples {
		if err := sample.validate(); err != nil {
			return fmt.Errorf("samples[%d]: %w", index, err)
		}
		if _, exists := seenEvaluationRuns[sample.EvaluationRunID]; exists {
			return fmt.Errorf("samples must use distinct EvaluationRuns")
		}
		seenEvaluationRuns[sample.EvaluationRunID] = struct{}{}
		if _, exists := seenReviewRuns[sample.ReviewRunID]; exists {
			return fmt.Errorf("samples must use distinct independently executed ReviewRuns")
		}
		seenReviewRuns[sample.ReviewRunID] = struct{}{}
	}
	if comparison.Dimensions == nil {
		return fmt.Errorf("dimensions must be an explicit array")
	}
	previousDimension := ""
	for index, dimension := range comparison.Dimensions {
		if err := dimension.validate(); err != nil {
			return fmt.Errorf("dimensions[%d]: %w", index, err)
		}
		if len(dimension.Samples) != len(comparison.Samples) {
			return fmt.Errorf("dimensions[%d] changed sample count", index)
		}
		for sampleIndex, sample := range dimension.Samples {
			caseSample := comparison.Samples[sampleIndex]
			if sample.EvaluationRunID != caseSample.EvaluationRunID ||
				sample.ReviewRunID != caseSample.ReviewRunID ||
				sample.ReportCompleteness != caseSample.ReportCompleteness {
				return fmt.Errorf("dimensions[%d] changed sample identity or completeness", index)
			}
		}
		if index > 0 && dimension.Dimension.ID <= previousDimension {
			return fmt.Errorf("dimensions must be uniquely sorted by dimension id")
		}
		previousDimension = dimension.Dimension.ID
	}
	want := calculateRepeatabilityCase(comparison)
	if !reflect.DeepEqual(comparison, want) {
		return fmt.Errorf("repeatability metrics are not the exact sample recomputation")
	}
	return nil
}

func (comparison RepeatabilityDimensionComparison) validate() error {
	if err := validateEvaluationDimensionRef("dimension", comparison.Dimension); err != nil {
		return err
	}
	if comparison.Samples == nil || len(comparison.Samples) < 2 {
		return fmt.Errorf("dimension samples must contain a baseline and at least one replay")
	}
	seenEvaluationRuns := make(map[string]struct{}, len(comparison.Samples))
	seenReviewRuns := make(map[string]struct{}, len(comparison.Samples))
	for index, sample := range comparison.Samples {
		if err := sample.validate(); err != nil {
			return fmt.Errorf("samples[%d]: %w", index, err)
		}
		if sample.Dimension != comparison.Dimension {
			return fmt.Errorf("dimension sample changed exact dimension ref")
		}
		if _, exists := seenEvaluationRuns[sample.EvaluationRunID]; exists {
			return fmt.Errorf("dimension samples must use distinct EvaluationRuns")
		}
		seenEvaluationRuns[sample.EvaluationRunID] = struct{}{}
		if _, exists := seenReviewRuns[sample.ReviewRunID]; exists {
			return fmt.Errorf("dimension samples must use distinct ReviewRuns")
		}
		seenReviewRuns[sample.ReviewRunID] = struct{}{}
	}
	want := calculateRepeatabilityDimension(comparison)
	if !reflect.DeepEqual(comparison, want) {
		return fmt.Errorf("dimension repeatability metrics are not the exact sample recomputation")
	}
	return nil
}

func (sample RepeatabilityDimensionSample) validate() error {
	for name, value := range map[string]string{
		"evaluation_run_id": sample.EvaluationRunID,
		"review_run_id":     sample.ReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := validateEvaluationDimensionRef("dimension", sample.Dimension); err != nil {
		return err
	}
	if sample.ReportCompleteness != string(contractsv1alpha1.AgentReviewComplete) &&
		sample.ReportCompleteness != string(contractsv1alpha1.AgentReviewPartial) {
		return fmt.Errorf("unsupported report_completeness %q", sample.ReportCompleteness)
	}
	if sample.FindingIDs == nil || uint32(len(sample.FindingIDs)) != sample.FindingCount {
		return fmt.Errorf("finding_ids must explicitly bind finding_count")
	}
	previous := ""
	for index, id := range sample.FindingIDs {
		if err := validateID("finding_ids", id); err != nil {
			return err
		}
		if index > 0 && id <= previous {
			return fmt.Errorf("finding_ids must be uniquely sorted")
		}
		previous = id
	}
	for name, availability := range map[string]MetricAvailability{
		"execution_coverage": sample.ExecutionCoverage,
		"usage":              sample.Usage,
	} {
		if availability.Available == (availability.ReasonCode != "") {
			return fmt.Errorf("%s availability and reason are inconsistent", name)
		}
		if availability.ReasonCode != "" {
			if err := validateID(name+".reason_code", availability.ReasonCode); err != nil {
				return err
			}
		}
	}
	switch sample.Verdict {
	case EvaluationPass, EvaluationFail, EvaluationInconclusive:
	default:
		return fmt.Errorf("unsupported verdict %q", sample.Verdict)
	}
	return nil
}

func (sample RepeatabilitySample) validate() error {
	for name, value := range map[string]string{
		"evaluation_run_id": sample.EvaluationRunID,
		"review_run_id":     sample.ReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := sample.ReviewRunRef.Validate(); err != nil {
		return fmt.Errorf("review_run_ref: %w", err)
	}
	if err := sample.GovernedReportRef.Validate(); err != nil {
		return fmt.Errorf("governed_report_ref: %w", err)
	}
	if sample.ReviewRunRef.Contract != runmodel.ContractReviewRun ||
		sample.GovernedReportRef.Contract != runmodel.ContractGovernedReviewReport {
		return fmt.Errorf("sample artifact contracts do not bind review evidence")
	}
	if sample.ReportCompleteness != string(contractsv1alpha1.AgentReviewComplete) &&
		sample.ReportCompleteness != string(contractsv1alpha1.AgentReviewPartial) {
		return fmt.Errorf("unsupported report_completeness %q", sample.ReportCompleteness)
	}
	if sample.FindingIDs == nil || uint32(len(sample.FindingIDs)) != sample.FindingCount {
		return fmt.Errorf("finding_ids must explicitly bind finding_count")
	}
	previous := ""
	for index, id := range sample.FindingIDs {
		if err := validateID("finding_ids", id); err != nil {
			return err
		}
		if index > 0 && id <= previous {
			return fmt.Errorf("finding_ids must be uniquely sorted")
		}
		previous = id
	}
	if sample.Localization.Available == (sample.Localization.ReasonCode != "") {
		return fmt.Errorf("localization availability and reason are inconsistent")
	}
	if sample.Localization.ReasonCode != "" {
		if err := validateID("localization.reason_code", sample.Localization.ReasonCode); err != nil {
			return err
		}
	}
	switch sample.Verdict {
	case EvaluationPass, EvaluationFail, EvaluationInconclusive:
	default:
		return fmt.Errorf("unsupported verdict %q", sample.Verdict)
	}
	return nil
}

func DecodeRepeatabilityRunRequest(data []byte) (RepeatabilityRunRequest, error) {
	return decodeStrict(data, "RepeatabilityRunRequest", func(value RepeatabilityRunRequest) error {
		return value.Validate()
	})
}

func DecodeRepeatabilityRun(data []byte) (RepeatabilityRun, error) {
	return decodeStrict(data, "RepeatabilityRun", func(value RepeatabilityRun) error {
		return value.Validate()
	})
}

func (repository *Repository) RecordRepeatabilityRun(
	ctx context.Context,
	request RepeatabilityRunRequest,
	mutation Mutation,
	runs EvaluationRunReader,
) (RepeatabilityRun, error) {
	if err := checkContext(ctx); err != nil {
		return RepeatabilityRun{}, err
	}
	if err := request.Validate(); err != nil {
		return RepeatabilityRun{}, fmt.Errorf("validate repeatability run request: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return RepeatabilityRun{}, err
	}
	if request.CreatedAt.After(mutation.At.UTC()) {
		return RepeatabilityRun{}, fmt.Errorf("repeatability created_at must not be after mutation time")
	}
	if runs == nil {
		return RepeatabilityRun{}, fmt.Errorf("review run repository is required")
	}

	repository.mu.Lock()
	state, err := repository.load()
	if err != nil {
		repository.mu.Unlock()
		return RepeatabilityRun{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		run, retryErr := repeatabilityIdempotentResult(existing, request, mutation)
		repository.mu.Unlock()
		return run, retryErr
	}
	evaluationRuns, err := loadRepeatabilityEvaluationRuns(state, request)
	if err != nil {
		repository.mu.Unlock()
		return RepeatabilityRun{}, err
	}
	if err := authorizeEvaluationRuns(state, evaluationRuns, mutation.Roles); err != nil {
		repository.mu.Unlock()
		return RepeatabilityRun{}, err
	}
	repository.mu.Unlock()

	comparisons, err := buildRepeatabilityComparisons(runs, evaluationRuns)
	if err != nil {
		return RepeatabilityRun{}, err
	}
	repeatability := RepeatabilityRun{
		SchemaVersion:           RepeatabilityRunSchemaVersion,
		RepeatabilityRunID:      request.RepeatabilityRunID,
		RepeatabilityRevision:   request.RepeatabilityRevision,
		BaselineEvaluationRunID: request.BaselineEvaluationRunID,
		ReplayEvaluationRunIDs:  slices.Clone(request.ReplayEvaluationRunIDs),
		EvaluatorRevision:       evaluationRuns[0].EvaluatorRevision,
		Comparisons:             comparisons,
		Summary:                 summarizeRepeatability(comparisons),
		CreatedAt:               request.CreatedAt.UTC(), RecordedAt: mutation.At.UTC(), RecordedBy: mutation.Actor,
	}
	if err := repeatability.Validate(); err != nil {
		return RepeatabilityRun{}, fmt.Errorf("validate computed repeatability run: %w", err)
	}
	event := repeatabilityRunEvent{
		SchemaVersion: repeatabilityRunEventSchemaVersion, Request: request, Run: repeatability,
		Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit,
		OccurredAt: mutation.At.UTC(),
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err = repository.load()
	if err != nil {
		return RepeatabilityRun{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		return repeatabilityIdempotentResult(existing, request, mutation)
	}
	current, err := loadRepeatabilityEvaluationRuns(state, request)
	if err != nil || !reflect.DeepEqual(current, evaluationRuns) {
		return RepeatabilityRun{}, fmt.Errorf("%w: evaluation inputs changed during repeatability analysis", ErrInvalidTransition)
	}
	if _, exists := state.repeatabilityRuns[request.RepeatabilityRunID]; exists {
		return RepeatabilityRun{}, fmt.Errorf("%w: repeatability_run_id %q already exists", ErrConflict, request.RepeatabilityRunID)
	}
	if _, err := repository.appendRepeatabilityRun(mutation, event); err != nil {
		return RepeatabilityRun{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return RepeatabilityRun{}, err
	}
	return reloaded.repeatabilityRun(request.RepeatabilityRunID)
}

func buildRepeatabilityComparisons(
	runs EvaluationRunReader,
	evaluationRuns []EvaluationRun,
) ([]RepeatabilityCaseComparison, error) {
	baselineEvaluation := evaluationRuns[0]
	for _, candidate := range evaluationRuns[1:] {
		if candidate.EvaluatorRevision != baselineEvaluation.EvaluatorRevision ||
			len(candidate.Results) != len(baselineEvaluation.Results) {
			return nil, fmt.Errorf("%w: evaluation runs do not share evaluator and case set", ErrInvalidTransition)
		}
	}
	comparisons := make([]RepeatabilityCaseComparison, 0, len(baselineEvaluation.Results))
	for caseIndex, baselineResult := range baselineEvaluation.Results {
		if baselineResult.CaseType == CaseFixValidation {
			return nil, fmt.Errorf("%w: fix_validation is not admitted to review repeatability because ApplyTrial is an independent variable", ErrInvalidTransition)
		}
		baselineRun, err := runs.LoadRun(baselineResult.ReviewRunID)
		if err != nil {
			return nil, fmt.Errorf("load baseline ReviewRun %q: %w", baselineResult.ReviewRunID, err)
		}
		if baselineRun.Kind != runmodel.RunKindReview || baselineRun.Status != runmodel.RunStatusSucceeded {
			return nil, fmt.Errorf("%w: repeatability baseline %q must be a succeeded non-replay review", ErrInvalidTransition, baselineRun.RunID)
		}
		baselineRef, err := runs.CommittedRunRef(baselineResult.ReviewRunID)
		if err != nil || baselineRef != baselineResult.ReviewRunRef {
			return nil, fmt.Errorf("%w: baseline committed ReviewRun ref changed", ErrInvalidTransition)
		}
		baselineSnapshot, err := runs.ExecutionSnapshotForRun(baselineResult.ReviewRunID)
		if err != nil {
			return nil, fmt.Errorf("load baseline execution snapshot: %w", err)
		}

		comparison := RepeatabilityCaseComparison{
			CaseID: baselineResult.CaseID, CaseType: baselineResult.CaseType,
			LabelRevision:       baselineResult.LabelRevision,
			LabelPolicyRevision: baselineResult.LabelPolicyRevision,
			ExpectedOutcome:     baselineResult.ExpectedOutcome,
			ExpectedCategory:    baselineResult.ExpectedCategory,
			ExpectedAnchorCount: baselineResult.ExpectedAnchorCount,
			Samples:             make([]RepeatabilitySample, 0, len(evaluationRuns)),
			Dimensions:          make([]RepeatabilityDimensionComparison, 0, len(baselineResult.Dimensions)),
		}
		reports := make([]contractsv1alpha1.GovernedReviewReport, 0, len(evaluationRuns))
		seenReviewRuns := map[string]struct{}{baselineResult.ReviewRunID: {}}
		for sampleIndex, evaluationRun := range evaluationRuns {
			result := evaluationRun.Results[caseIndex]
			if !sameRepeatabilityCase(baselineResult, result) {
				return nil, fmt.Errorf("%w: case or frozen label mismatch at %q", ErrInvalidTransition, baselineResult.CaseID)
			}
			if sampleIndex > 0 {
				if _, exists := seenReviewRuns[result.ReviewRunID]; exists {
					return nil, fmt.Errorf("%w: case %q reuses ReviewRun %q as an independent sample", ErrInvalidTransition, baselineResult.CaseID, result.ReviewRunID)
				}
				seenReviewRuns[result.ReviewRunID] = struct{}{}
				if err := validateDirectExactReplay(runs, baselineRun, baselineRef, baselineSnapshot, result); err != nil {
					return nil, fmt.Errorf("case %q replay %q: %w", baselineResult.CaseID, result.ReviewRunID, err)
				}
			}
			sample, report, err := loadRepeatabilitySample(runs, evaluationRun.EvaluationRunID, result)
			if err != nil {
				return nil, fmt.Errorf("case %q sample %q: %w", baselineResult.CaseID, evaluationRun.EvaluationRunID, err)
			}
			comparison.Samples = append(comparison.Samples, sample)
			reports = append(reports, report)
		}
		for dimensionIndex, baselineDimension := range baselineResult.Dimensions {
			dimensionComparison := RepeatabilityDimensionComparison{
				Dimension: baselineDimension.Dimension,
				Samples:   make([]RepeatabilityDimensionSample, 0, len(evaluationRuns)),
			}
			for sampleIndex, evaluationRun := range evaluationRuns {
				result := evaluationRun.Results[caseIndex]
				dimension := result.Dimensions[dimensionIndex]
				findingIDs := make([]string, 0, dimension.FindingCount)
				for _, finding := range reports[sampleIndex].Findings {
					if finding.Dimension == dimension.Dimension {
						findingIDs = append(findingIDs, finding.FindingID)
					}
				}
				sort.Strings(findingIDs)
				if uint32(len(findingIDs)) != dimension.FindingCount {
					return nil, fmt.Errorf("%w: case %q dimension %q finding count does not bind governed report", ErrInvalidTransition, baselineResult.CaseID, dimension.Dimension.ID)
				}
				dimensionComparison.Samples = append(dimensionComparison.Samples, RepeatabilityDimensionSample{
					EvaluationRunID:      evaluationRun.EvaluationRunID,
					ReviewRunID:          result.ReviewRunID,
					Dimension:            dimension.Dimension,
					ReportCompleteness:   result.ReportCompleteness,
					FindingIDs:           findingIDs,
					FindingCount:         dimension.FindingCount,
					CandidateCount:       dimension.CandidateCount,
					RawCandidateCount:    dimension.RawCandidateCount,
					ContextGapCount:      dimension.ContextGapCount,
					TasksSucceeded:       dimension.TasksSucceeded,
					TasksFailed:          dimension.TasksFailed,
					TasksCanceled:        dimension.TasksCanceled,
					CumulativeDurationMS: dimension.CumulativeDurationMS,
					TotalTokens:          dimension.TotalTokens,
					ExecutionCoverage:    dimension.ExecutionCoverage,
					Usage:                dimension.Usage,
					Verdict:              dimension.Verdict,
				})
			}
			comparison.Dimensions = append(comparison.Dimensions, calculateRepeatabilityDimension(dimensionComparison))
		}
		comparison = calculateRepeatabilityCase(comparison)
		comparisons = append(comparisons, comparison)
	}
	return comparisons, nil
}

func validateDirectExactReplay(
	runs EvaluationRunReader,
	baseline runmodel.ReviewRun,
	baselineRef runmodel.ArtifactRef,
	baselineSnapshot runmodel.ExecutionSnapshot,
	result EvaluationCaseResult,
) error {
	replay, err := runs.LoadRun(result.ReviewRunID)
	if err != nil {
		return fmt.Errorf("load ReviewRun: %w", err)
	}
	if replay.Kind != runmodel.RunKindReplay || replay.Status != runmodel.RunStatusSucceeded ||
		replay.SourceRunID != baseline.RunID || replay.ReplayRootRunID != baseline.RunID ||
		replay.ReplayVariable != runmodel.ReplayVariableNone {
		return fmt.Errorf("%w: sample is not a succeeded direct exact replay of baseline %q", ErrInvalidTransition, baseline.RunID)
	}
	replayRef, err := runs.CommittedRunRef(result.ReviewRunID)
	if err != nil || replayRef != result.ReviewRunRef {
		return fmt.Errorf("%w: committed replay ReviewRun ref changed", ErrInvalidTransition)
	}
	snapshot, err := runs.ExecutionSnapshotForRun(result.ReviewRunID)
	if err != nil || snapshot.ReplaySourceRunRef == nil || snapshot.ReplayChangeSetRef == nil {
		return fmt.Errorf("%w: exact replay lacks immutable lineage evidence", ErrInvalidTransition)
	}
	if *snapshot.ReplaySourceRunRef != baselineRef || len(snapshot.ReplayInputRefs) != 0 ||
		!sameExactExecutionSnapshot(baselineSnapshot, snapshot) {
		return fmt.Errorf("%w: replay changed frozen execution inputs or policy", ErrInvalidTransition)
	}
	var change runmodel.ReplayChangeSet
	if err := runs.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		return fmt.Errorf("read replay change set: %w", err)
	}
	if err := change.Validate(); err != nil {
		return fmt.Errorf("%w: invalid replay change set: %v", ErrInvalidTransition, err)
	}
	if change.SourceRunID != baseline.RunID || change.RootRunID != baseline.RunID ||
		change.ParentReplayRunID != "" || change.Variable != runmodel.ReplayVariableNone ||
		change.BaselineSHA256 != baselineSnapshot.Config.SHA256 ||
		change.VariantSHA256 != baselineSnapshot.Config.SHA256 ||
		len(change.ChangedFields) != 0 || change.RemoteWrites != "deny" {
		return fmt.Errorf("%w: change set does not prove a direct zero-variable replay", ErrInvalidTransition)
	}
	return nil
}

func sameExactExecutionSnapshot(baseline, replay runmodel.ExecutionSnapshot) bool {
	return baseline.TargetSnapshotRef == replay.TargetSnapshotRef &&
		baseline.ReviewInputRef == replay.ReviewInputRef &&
		reflect.DeepEqual(baseline.ReviewShardManifestRef, replay.ReviewShardManifestRef) &&
		baseline.WorkflowDefinitionRef == replay.WorkflowDefinitionRef &&
		baseline.ConfigBundleRef == replay.ConfigBundleRef &&
		baseline.Workflow == replay.Workflow && baseline.Config == replay.Config &&
		baseline.RuntimeProfile == replay.RuntimeProfile &&
		baseline.BuildIdentity == replay.BuildIdentity &&
		reflect.DeepEqual(baseline.ToolPolicy, replay.ToolPolicy) &&
		baseline.RemoteWrites == replay.RemoteWrites &&
		reflect.DeepEqual(baseline.RuntimeEvidenceRefs, replay.RuntimeEvidenceRefs) &&
		reflect.DeepEqual(baseline.ContextProviderReceiptRefs, replay.ContextProviderReceiptRefs)
}

func sameRepeatabilityCase(baseline, candidate EvaluationCaseResult) bool {
	return baseline.CaseID == candidate.CaseID && baseline.CaseType == candidate.CaseType &&
		baseline.LabelRevision == candidate.LabelRevision &&
		baseline.LabelPolicyRevision == candidate.LabelPolicyRevision &&
		baseline.ExpectedOutcome == candidate.ExpectedOutcome &&
		baseline.ExpectedCategory == candidate.ExpectedCategory &&
		baseline.ExpectedAnchorCount == candidate.ExpectedAnchorCount &&
		baseline.InputSnapshotRef == candidate.InputSnapshotRef &&
		sameRepeatabilityDimensionScope(baseline.Dimensions, candidate.Dimensions)
}

func sameRepeatabilityDimensionScope(left, right []EvaluationDimensionResult) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Dimension != right[index].Dimension {
			return false
		}
	}
	return true
}

func loadRepeatabilitySample(
	runs EvaluationRunReader,
	evaluationRunID string,
	result EvaluationCaseResult,
) (RepeatabilitySample, contractsv1alpha1.GovernedReviewReport, error) {
	committedRef, err := runs.CommittedRunRef(result.ReviewRunID)
	if err != nil || committedRef != result.ReviewRunRef {
		return RepeatabilitySample{}, contractsv1alpha1.GovernedReviewReport{}, fmt.Errorf("%w: committed ReviewRun ref changed", ErrInvalidTransition)
	}
	var report contractsv1alpha1.GovernedReviewReport
	if err := runs.ReadJSONArtifact(result.GovernedReportRef, &report); err != nil {
		return RepeatabilitySample{}, contractsv1alpha1.GovernedReviewReport{}, fmt.Errorf("read governed report: %w", err)
	}
	if err := report.Validate(); err != nil {
		return RepeatabilitySample{}, contractsv1alpha1.GovernedReviewReport{}, fmt.Errorf("%w: invalid governed report: %v", ErrInvalidTransition, err)
	}
	if report.ReviewRunID != result.ReviewRunID ||
		string(report.Completeness) != result.ReportCompleteness ||
		uint32(len(report.Findings)) != result.TotalFindingCount {
		return RepeatabilitySample{}, contractsv1alpha1.GovernedReviewReport{}, fmt.Errorf("%w: governed report does not bind evaluation result", ErrInvalidTransition)
	}
	findingIDs := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findingIDs = append(findingIDs, finding.FindingID)
	}
	sort.Strings(findingIDs)
	return RepeatabilitySample{
		EvaluationRunID: evaluationRunID, ReviewRunID: result.ReviewRunID,
		ReviewRunRef: result.ReviewRunRef, GovernedReportRef: result.GovernedReportRef,
		ReportCompleteness: result.ReportCompleteness, FindingIDs: findingIDs,
		FindingCount: uint32(len(findingIDs)), MatchedAnchorCount: result.MatchedAnchorCount,
		Localization: result.Localization, Verdict: result.Verdict,
	}, report, nil
}

func calculateRepeatabilityDimension(value RepeatabilityDimensionComparison) RepeatabilityDimensionComparison {
	result := value
	result.SampleCount = uint32(len(result.Samples))
	result.PairCount = result.SampleCount * (result.SampleCount - 1) / 2
	result.FindingSetComparison = MetricAvailability{Available: true}
	result.PairwiseJaccardPPM = 0
	result.ExactFindingSetPairCount = 0
	result.ExactFindingSetPairRatePPM = 0
	result.FindingPresenceSampleCount = 0
	result.FindingPresenceRatePPM = 0
	result.VerdictFlipPairCount = 0
	result.VerdictFlipPairRatePPM = 0
	result.CandidateCountMin = ^uint32(0)
	result.CandidateCountMax = 0
	result.RawCandidateCountMin = ^uint32(0)
	result.RawCandidateCountMax = 0
	result.ContextGapCountMin = ^uint32(0)
	result.ContextGapCountMax = 0
	result.TasksFailedMin = ^uint32(0)
	result.TasksFailedMax = 0
	result.CumulativeDurationMSMin = ^uint64(0)
	result.CumulativeDurationMSMax = 0
	result.TotalTokensMin = ^uint64(0)
	result.TotalTokensMax = 0
	result.ExecutionComparison = MetricAvailability{Available: true}
	result.UsageComparison = MetricAvailability{Available: true}
	result.Stable = false

	var jaccardTotal uint64
	for _, sample := range result.Samples {
		if sample.ReportCompleteness != string(contractsv1alpha1.AgentReviewComplete) {
			result.FindingSetComparison = MetricAvailability{ReasonCode: "incomplete_governed_report"}
		}
		if !sample.ExecutionCoverage.Available {
			result.ExecutionComparison = MetricAvailability{ReasonCode: "dimension_execution_sample_unavailable"}
		}
		if !sample.Usage.Available {
			result.UsageComparison = MetricAvailability{ReasonCode: "dimension_usage_sample_unavailable"}
		}
		if sample.FindingCount > 0 {
			result.FindingPresenceSampleCount++
		}
		result.CandidateCountMin = min(result.CandidateCountMin, sample.CandidateCount)
		result.CandidateCountMax = max(result.CandidateCountMax, sample.CandidateCount)
		result.RawCandidateCountMin = min(result.RawCandidateCountMin, sample.RawCandidateCount)
		result.RawCandidateCountMax = max(result.RawCandidateCountMax, sample.RawCandidateCount)
		result.ContextGapCountMin = min(result.ContextGapCountMin, sample.ContextGapCount)
		result.ContextGapCountMax = max(result.ContextGapCountMax, sample.ContextGapCount)
		result.TasksFailedMin = min(result.TasksFailedMin, sample.TasksFailed)
		result.TasksFailedMax = max(result.TasksFailedMax, sample.TasksFailed)
		result.CumulativeDurationMSMin = min(result.CumulativeDurationMSMin, sample.CumulativeDurationMS)
		result.CumulativeDurationMSMax = max(result.CumulativeDurationMSMax, sample.CumulativeDurationMS)
		result.TotalTokensMin = min(result.TotalTokensMin, sample.TotalTokens)
		result.TotalTokensMax = max(result.TotalTokensMax, sample.TotalTokens)
	}
	if len(result.Samples) == 0 {
		result.CandidateCountMin = 0
		result.RawCandidateCountMin = 0
		result.ContextGapCountMin = 0
		result.TasksFailedMin = 0
		result.CumulativeDurationMSMin = 0
		result.TotalTokensMin = 0
	}
	for left := 0; left < len(result.Samples); left++ {
		for right := left + 1; right < len(result.Samples); right++ {
			before, after := result.Samples[left], result.Samples[right]
			if before.Verdict != after.Verdict {
				result.VerdictFlipPairCount++
			}
			jaccardTotal += uint64(findingSetJaccardPPM(before.FindingIDs, after.FindingIDs))
			if slices.Equal(before.FindingIDs, after.FindingIDs) {
				result.ExactFindingSetPairCount++
			}
		}
	}
	result.VerdictFlipPairRatePPM = repeatabilityRate(result.VerdictFlipPairCount, result.PairCount)
	if result.FindingSetComparison.Available {
		result.PairwiseJaccardPPM = roundedRepeatabilityMean(jaccardTotal, uint64(result.PairCount))
		result.ExactFindingSetPairRatePPM = repeatabilityRate(result.ExactFindingSetPairCount, result.PairCount)
		result.FindingPresenceRatePPM = repeatabilityRate(result.FindingPresenceSampleCount, result.SampleCount)
		result.Stable = result.ExactFindingSetPairCount == result.PairCount && result.VerdictFlipPairCount == 0
	} else {
		result.PairwiseJaccardPPM = 0
		result.ExactFindingSetPairCount = 0
		result.ExactFindingSetPairRatePPM = 0
		result.FindingPresenceSampleCount = 0
		result.FindingPresenceRatePPM = 0
	}
	return result
}

func calculateRepeatabilityCase(value RepeatabilityCaseComparison) RepeatabilityCaseComparison {
	result := value
	result.SampleCount = 0
	result.PairCount = 0
	result.FindingSetComparison = MetricAvailability{}
	result.PairwiseJaccardPPM = 0
	result.ExactFindingSetPairCount = 0
	result.ExactFindingSetPairRatePPM = 0
	result.FindingPresenceSampleCount = 0
	result.FindingPresenceRatePPM = 0
	result.ExpectedAnchorHit = MetricAvailability{}
	result.ExpectedAnchorHitSampleCount = 0
	result.ExpectedAnchorHitRatePPM = 0
	result.FindingCountMin = 0
	result.FindingCountMax = 0
	result.VerdictFlipPairCount = 0
	result.VerdictFlipPairRatePPM = 0
	result.Stable = false
	result.SampleCount = uint32(len(result.Samples))
	result.PairCount = result.SampleCount * (result.SampleCount - 1) / 2
	result.FindingSetComparison = MetricAvailability{Available: true}
	result.ExpectedAnchorHit = MetricAvailability{Available: true}
	result.FindingCountMin = ^uint32(0)
	var jaccardTotal uint64
	for _, sample := range result.Samples {
		if sample.FindingCount < result.FindingCountMin {
			result.FindingCountMin = sample.FindingCount
		}
		if sample.FindingCount > result.FindingCountMax {
			result.FindingCountMax = sample.FindingCount
		}
		if sample.ReportCompleteness != string(contractsv1alpha1.AgentReviewComplete) {
			result.FindingSetComparison = MetricAvailability{ReasonCode: "incomplete_governed_report"}
		}
		if sample.FindingCount > 0 {
			result.FindingPresenceSampleCount++
		}
		if result.ExpectedAnchorCount > 0 && sample.Localization.Available && sample.MatchedAnchorCount > 0 {
			result.ExpectedAnchorHitSampleCount++
		}
		if result.ExpectedAnchorCount == 0 || !sample.Localization.Available {
			result.ExpectedAnchorHit = MetricAvailability{ReasonCode: "expected_anchor_sample_unavailable"}
		}
	}
	if result.FindingCountMin == ^uint32(0) {
		result.FindingCountMin = 0
	}
	for left := 0; left < len(result.Samples); left++ {
		for right := left + 1; right < len(result.Samples); right++ {
			before, after := result.Samples[left], result.Samples[right]
			if before.Verdict != after.Verdict {
				result.VerdictFlipPairCount++
			}
			jaccard := findingSetJaccardPPM(before.FindingIDs, after.FindingIDs)
			jaccardTotal += uint64(jaccard)
			if slices.Equal(before.FindingIDs, after.FindingIDs) {
				result.ExactFindingSetPairCount++
			}
		}
	}
	result.VerdictFlipPairRatePPM = repeatabilityRate(result.VerdictFlipPairCount, result.PairCount)
	if result.FindingSetComparison.Available {
		result.PairwiseJaccardPPM = roundedRepeatabilityMean(jaccardTotal, uint64(result.PairCount))
		result.ExactFindingSetPairRatePPM = repeatabilityRate(result.ExactFindingSetPairCount, result.PairCount)
		result.FindingPresenceRatePPM = repeatabilityRate(result.FindingPresenceSampleCount, result.SampleCount)
		result.Stable = result.ExactFindingSetPairCount == result.PairCount && result.VerdictFlipPairCount == 0
	} else {
		result.PairwiseJaccardPPM = 0
		result.ExactFindingSetPairCount = 0
		result.ExactFindingSetPairRatePPM = 0
		result.FindingPresenceSampleCount = 0
		result.FindingPresenceRatePPM = 0
		result.Stable = false
	}
	if result.ExpectedAnchorHit.Available {
		result.ExpectedAnchorHitRatePPM = repeatabilityRate(result.ExpectedAnchorHitSampleCount, result.SampleCount)
	} else {
		result.ExpectedAnchorHitSampleCount = 0
		result.ExpectedAnchorHitRatePPM = 0
	}
	return result
}

func findingSetJaccardPPM(left, right []string) uint32 {
	if len(left) == 0 && len(right) == 0 {
		return repeatabilityRateScale
	}
	leftIndex, rightIndex := 0, 0
	var intersection, union uint32
	for leftIndex < len(left) || rightIndex < len(right) {
		switch {
		case leftIndex >= len(left):
			union++
			rightIndex++
		case rightIndex >= len(right):
			union++
			leftIndex++
		case left[leftIndex] == right[rightIndex]:
			intersection++
			union++
			leftIndex++
			rightIndex++
		case left[leftIndex] < right[rightIndex]:
			union++
			leftIndex++
		default:
			union++
			rightIndex++
		}
	}
	return repeatabilityRate(intersection, union)
}

func repeatabilityRate(numerator, denominator uint32) uint32 {
	return repeatabilityRate64(uint64(numerator), uint64(denominator))
}

func repeatabilityRate64(numerator, denominator uint64) uint32 {
	if denominator == 0 {
		return 0
	}
	return uint32((numerator*uint64(repeatabilityRateScale) + denominator/2) / denominator)
}

func roundedRepeatabilityMean(total, count uint64) uint32 {
	if count == 0 {
		return 0
	}
	return uint32((total + count/2) / count)
}

func summarizeRepeatability(comparisons []RepeatabilityCaseComparison) RepeatabilityRunSummary {
	var summary RepeatabilityRunSummary
	if len(comparisons) == 0 {
		return summary
	}
	summary.Cases = uint32(len(comparisons))
	summary.SamplesPerCase = comparisons[0].SampleCount
	summary.PairsPerCase = comparisons[0].PairCount
	summary.FindingSetStability = MetricAvailability{Available: true}
	var jaccardTotal uint64
	for _, comparison := range comparisons {
		summary.VerdictFlipPairCount += uint64(comparison.VerdictFlipPairCount)
		if !comparison.FindingSetComparison.Available {
			summary.IndeterminateCases++
			summary.FindingSetStability = MetricAvailability{ReasonCode: "incomplete_governed_report"}
			continue
		}
		if comparison.Stable {
			summary.StableCases++
		} else {
			summary.UnstableCases++
		}
		summary.ExactFindingSetPairCount += uint64(comparison.ExactFindingSetPairCount)
		jaccardTotal += uint64(comparison.PairwiseJaccardPPM) * uint64(comparison.PairCount)
	}
	totalPairs := uint64(summary.Cases) * uint64(summary.PairsPerCase)
	summary.VerdictFlipPairRatePPM = repeatabilityRate64(summary.VerdictFlipPairCount, totalPairs)
	if summary.FindingSetStability.Available {
		summary.PairwiseJaccardPPM = roundedRepeatabilityMean(jaccardTotal, totalPairs)
		summary.ExactFindingSetPairRatePPM = repeatabilityRate64(summary.ExactFindingSetPairCount, totalPairs)
	} else {
		summary.PairwiseJaccardPPM = 0
		summary.ExactFindingSetPairCount = 0
		summary.ExactFindingSetPairRatePPM = 0
	}
	return summary
}

func loadRepeatabilityEvaluationRuns(
	state *projectionState,
	request RepeatabilityRunRequest,
) ([]EvaluationRun, error) {
	ids := append([]string{request.BaselineEvaluationRunID}, request.ReplayEvaluationRunIDs...)
	runs := make([]EvaluationRun, 0, len(ids))
	for _, id := range ids {
		run, exists := state.evaluationRuns[id]
		if !exists {
			return nil, fmt.Errorf("%w: evaluation run %q", ErrNotFound, id)
		}
		runs = append(runs, cloneValue(run))
	}
	return runs, nil
}

func authorizeEvaluationRuns(state *projectionState, runs []EvaluationRun, roles []Role) error {
	for _, run := range runs {
		for _, result := range run.Results {
			record, exists := state.cases[result.CaseID]
			if !exists {
				return fmt.Errorf("%w: repeatability references unknown case", ErrCorrupt)
			}
			if err := authorizeExposure(record.CurrentCase(), roles); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRepeatabilityAgainstEvaluationRuns(
	run RepeatabilityRun,
	evaluationRuns []EvaluationRun,
) error {
	if len(evaluationRuns) != len(run.ReplayEvaluationRunIDs)+1 ||
		run.EvaluatorRevision != evaluationRuns[0].EvaluatorRevision ||
		len(run.Comparisons) != len(evaluationRuns[0].Results) {
		return fmt.Errorf("repeatability run does not bind evaluator and case counts")
	}
	for index, comparison := range run.Comparisons {
		if len(comparison.Dimensions) != len(evaluationRuns[0].Results[index].Dimensions) {
			return fmt.Errorf("repeatability comparison %d changed dimension count", index)
		}
		for sampleIndex, evaluationRun := range evaluationRuns {
			result := evaluationRun.Results[index]
			sample := comparison.Samples[sampleIndex]
			if !sameRepeatabilityCase(evaluationRuns[0].Results[index], result) ||
				sample.EvaluationRunID != evaluationRun.EvaluationRunID ||
				sample.ReviewRunID != result.ReviewRunID || sample.ReviewRunRef != result.ReviewRunRef ||
				sample.GovernedReportRef != result.GovernedReportRef ||
				sample.ReportCompleteness != result.ReportCompleteness ||
				sample.FindingCount != result.TotalFindingCount ||
				sample.MatchedAnchorCount != result.MatchedAnchorCount ||
				sample.Localization != result.Localization || sample.Verdict != result.Verdict {
				return fmt.Errorf("repeatability comparison %d does not bind exact evaluation results", index)
			}
			for dimensionIndex, dimensionComparison := range comparison.Dimensions {
				dimensionResult := result.Dimensions[dimensionIndex]
				if dimensionComparison.Dimension != dimensionResult.Dimension ||
					len(dimensionComparison.Samples) != len(evaluationRuns) {
					return fmt.Errorf("repeatability comparison %d changed exact dimension scope", index)
				}
				dimensionSample := dimensionComparison.Samples[sampleIndex]
				if dimensionSample.EvaluationRunID != evaluationRun.EvaluationRunID ||
					dimensionSample.ReviewRunID != result.ReviewRunID ||
					dimensionSample.Dimension != dimensionResult.Dimension ||
					dimensionSample.ReportCompleteness != result.ReportCompleteness ||
					dimensionSample.FindingCount != dimensionResult.FindingCount ||
					dimensionSample.CandidateCount != dimensionResult.CandidateCount ||
					dimensionSample.RawCandidateCount != dimensionResult.RawCandidateCount ||
					dimensionSample.ContextGapCount != dimensionResult.ContextGapCount ||
					dimensionSample.TasksSucceeded != dimensionResult.TasksSucceeded ||
					dimensionSample.TasksFailed != dimensionResult.TasksFailed ||
					dimensionSample.TasksCanceled != dimensionResult.TasksCanceled ||
					dimensionSample.CumulativeDurationMS != dimensionResult.CumulativeDurationMS ||
					dimensionSample.TotalTokens != dimensionResult.TotalTokens ||
					dimensionSample.ExecutionCoverage != dimensionResult.ExecutionCoverage ||
					dimensionSample.Usage != dimensionResult.Usage ||
					dimensionSample.Verdict != dimensionResult.Verdict {
					return fmt.Errorf("repeatability comparison %d dimension %d does not bind exact evaluation result", index, dimensionIndex)
				}
			}
		}
	}
	return nil
}

func repeatabilityIdempotentResult(
	existing storedEvent,
	request RepeatabilityRunRequest,
	mutation Mutation,
) (RepeatabilityRun, error) {
	if existing.stream != repeatabilityRunStream {
		return RepeatabilityRun{}, fmt.Errorf("%w: idempotency key %q is already used in %s", ErrConflict, mutation.IdempotencyKey, existing.stream)
	}
	var event repeatabilityRunEvent
	if err := decodeEventStrict(existing.envelope.Payload, &event); err != nil {
		return RepeatabilityRun{}, fmt.Errorf("%w: decode idempotent repeatability run: %v", ErrCorrupt, err)
	}
	if !reflect.DeepEqual(event.Request, request) || event.Actor != mutation.Actor ||
		!slices.Equal(event.Roles, mutation.Roles) || event.Audit != mutation.Audit ||
		!event.OccurredAt.Equal(mutation.At.UTC()) {
		return RepeatabilityRun{}, fmt.Errorf("%w: idempotency key %q has different input", ErrConflict, mutation.IdempotencyKey)
	}
	return cloneValue(event.Run), nil
}

func (repository *Repository) GetRepeatabilityRun(id string, access Access) (RepeatabilityRun, error) {
	if err := validateID("repeatability_run_id", id); err != nil {
		return RepeatabilityRun{}, err
	}
	if err := access.Validate(); err != nil {
		return RepeatabilityRun{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RepeatabilityRun{}, err
	}
	run, err := state.repeatabilityRun(id)
	if err != nil {
		return RepeatabilityRun{}, err
	}
	request := RepeatabilityRunRequest{BaselineEvaluationRunID: run.BaselineEvaluationRunID, ReplayEvaluationRunIDs: run.ReplayEvaluationRunIDs}
	evaluationRuns, err := loadRepeatabilityEvaluationRuns(state, request)
	if err != nil {
		return RepeatabilityRun{}, err
	}
	if err := authorizeEvaluationRuns(state, evaluationRuns, access.Roles); err != nil {
		return RepeatabilityRun{}, err
	}
	return run, nil
}

func (repository *Repository) ListRepeatabilityRuns(access Access) ([]RepeatabilityRun, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.repeatabilityRuns))
	for id, run := range state.repeatabilityRuns {
		request := RepeatabilityRunRequest{BaselineEvaluationRunID: run.BaselineEvaluationRunID, ReplayEvaluationRunIDs: run.ReplayEvaluationRunIDs}
		evaluationRuns, loadErr := loadRepeatabilityEvaluationRuns(state, request)
		if loadErr == nil && authorizeEvaluationRuns(state, evaluationRuns, access.Roles) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]RepeatabilityRun, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneValue(state.repeatabilityRuns[id]))
	}
	return result, nil
}
