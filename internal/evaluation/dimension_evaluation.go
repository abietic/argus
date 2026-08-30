package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func validateEvaluationDimensionRef(
	name string,
	ref contractsv1alpha1.VersionedRef,
) error {
	if err := validateID(name+".id", ref.ID); err != nil {
		return err
	}
	if err := validateID(name+".revision", ref.Revision); err != nil {
		return err
	}
	if err := validateSHA256(name+".sha256", ref.SHA256); err != nil {
		return err
	}
	return nil
}

func validateEvaluationDimensionScope(scope []contractsv1alpha1.VersionedRef) error {
	if len(scope) > contractsv1alpha1.AgentReviewWorkerMaxSkillCount {
		return fmt.Errorf(
			"dimensions exceeds %d entries",
			contractsv1alpha1.AgentReviewWorkerMaxSkillCount,
		)
	}
	previous := ""
	for index, dimension := range scope {
		if err := validateEvaluationDimensionRef(
			fmt.Sprintf("dimensions[%d]", index), dimension,
		); err != nil {
			return err
		}
		if index > 0 && dimension.ID <= previous {
			return fmt.Errorf("dimensions must be uniquely sorted by id")
		}
		previous = dimension.ID
	}
	return nil
}

func evaluateDimensions(
	runs EvaluationRunReader,
	run runmodel.ReviewRun,
	report contractsv1alpha1.GovernedReviewReport,
	evidenceOwnerRunID string,
	label Label,
	scope []contractsv1alpha1.VersionedRef,
) ([]EvaluationDimensionResult, error) {
	results := make([]EvaluationDimensionResult, 0, len(scope))
	if len(scope) == 0 {
		return results, nil
	}
	receiptCollection, err := readCommittedExecutionReceipts(
		runs, run, report.ExecutionID,
	)
	if err != nil {
		return nil, fmt.Errorf("read dimension execution receipts: %w", err)
	}
	rawCandidates, hypothesisSet, err := readCommittedDimensionEvidence(
		runs, run, report, evidenceOwnerRunID,
	)
	if err != nil {
		return nil, fmt.Errorf("read dimension hypothesis evidence: %w", err)
	}
	receipts := []contractsv1alpha1.AgentExecutionReceipt{}
	if receiptCollection != nil {
		receipts = receiptCollection.Receipts
	}
	dimensionByOccurrence := make(map[string]contractsv1alpha1.VersionedRef, len(report.Candidates))
	for _, candidate := range report.Candidates {
		dimensionByOccurrence[candidate.Hypothesis.OccurrenceID] = candidate.Hypothesis.Dimension
	}
	for _, dimension := range scope {
		result, err := evaluateDimension(
			dimension, label, report, hypothesisSet.Coverage.Gaps,
			receipts, dimensionByOccurrence, rawCandidates,
		)
		if err != nil {
			return nil, fmt.Errorf("evaluate dimension %q: %w", dimension.ID, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func evaluateDimension(
	dimension contractsv1alpha1.VersionedRef,
	label Label,
	report contractsv1alpha1.GovernedReviewReport,
	coverageGaps []contractsv1alpha1.AgentReviewCoverageGap,
	receipts []contractsv1alpha1.AgentExecutionReceipt,
	dimensionByOccurrence map[string]contractsv1alpha1.VersionedRef,
	rawCandidates *contractsv1alpha1.AgentReviewRawCandidateCollection,
) (EvaluationDimensionResult, error) {
	result := EvaluationDimensionResult{
		Dimension:           dimension,
		ExpectedAnchorCount: uint32(len(label.Anchors)),
		Normalization: MetricAvailability{
			ReasonCode: "raw_candidate_dimension_evidence_unavailable",
		},
	}
	if rawCandidates != nil {
		result.Normalization = MetricAvailability{Available: true}
		for _, candidate := range rawCandidates.RawCandidates {
			if candidate.Dimension != dimension {
				continue
			}
			result.RawCandidateCount++
			switch candidate.Action {
			case contractsv1alpha1.HypothesisNormalizationRetained:
				result.NormalizationRetainedCount++
			case contractsv1alpha1.HypothesisNormalizationMergedDuplicate:
				result.MergedDuplicateCount++
			case contractsv1alpha1.HypothesisNormalizationRejectedInvalid:
				result.RejectedInvalidCount++
			case contractsv1alpha1.HypothesisNormalizationExcludedBudget:
				result.ExcludedBudgetCount++
			}
		}
	}
	dimensionCandidates := make([]contractsv1alpha1.GovernedReviewCandidate, 0)
	for _, candidate := range report.Candidates {
		if candidate.Hypothesis.Dimension != dimension {
			continue
		}
		dimensionCandidates = append(dimensionCandidates, candidate)
		switch candidate.Disposition {
		case contractsv1alpha1.GovernedCandidateConfirmed:
			result.ConfirmedCount++
		case contractsv1alpha1.GovernedCandidateRejected:
			result.RejectedCount++
		case contractsv1alpha1.GovernedCandidateInconclusive:
			result.InconclusiveCount++
		}
	}
	result.CandidateCount = uint32(len(dimensionCandidates))
	dimensionFindings := make([]contractsv1alpha1.GovernedReviewFinding, 0)
	for _, finding := range report.Findings {
		if finding.Dimension != dimension {
			continue
		}
		result.FindingCount++
		if label.Category == "" || finding.Category == label.Category {
			result.EvaluatedFindingCount++
			dimensionFindings = append(dimensionFindings, finding)
		}
	}
	result.MatchedAnchorCount, result.LocalizedFindingCount, result.Localization =
		scoreLocalization(label.Anchors, dimensionFindings, report.Completeness)

	attributedReceipts := make([]contractsv1alpha1.AgentExecutionReceipt, 0)
	reviewGroups := make(map[string]struct{})
	contextGroupByTask := make(map[string]string)
	knownGroups := make(map[string]struct{})
	for _, receipt := range receipts {
		knownGroups[receipt.GroupID] = struct{}{}
		if receipt.TaskRole == contractsv1alpha1.AgentTaskContext {
			contextGroupByTask[receipt.TaskID] = receipt.GroupID
		}
	}
	for _, receipt := range receipts {
		attributed := false
		switch receipt.TaskRole {
		case contractsv1alpha1.AgentTaskReview:
			if receipt.Dimension == dimension {
				result.ReviewTaskCount++
				reviewGroups[receipt.GroupID] = struct{}{}
				attributed = true
			}
		case contractsv1alpha1.AgentTaskVerification:
			if receipt.HypothesisOccurrenceID != nil &&
				dimensionByOccurrence[*receipt.HypothesisOccurrenceID] == dimension {
				result.VerificationTaskCount++
				attributed = true
			}
		}
		if !attributed {
			continue
		}
		attributedReceipts = append(attributedReceipts, receipt)
		switch receipt.Status {
		case contractsv1alpha1.AgentTaskSucceeded:
			result.TasksSucceeded++
		case contractsv1alpha1.AgentTaskFailed:
			result.TasksFailed++
		case contractsv1alpha1.AgentTaskCanceled:
			result.TasksCanceled++
		}
		var ok bool
		if result.ModelTurnsStarted, ok = checkedEvaluationTokenAdd(
			result.ModelTurnsStarted, uint64(receipt.ModelTurnsStarted),
		); !ok {
			return EvaluationDimensionResult{}, fmt.Errorf("model turns started overflow")
		}
		if result.ModelTurnsCompleted, ok = checkedEvaluationTokenAdd(
			result.ModelTurnsCompleted, uint64(receipt.ModelTurnsCompleted),
		); !ok {
			return EvaluationDimensionResult{}, fmt.Errorf("model turns completed overflow")
		}
		if result.ToolCalls, ok = checkedEvaluationTokenAdd(
			result.ToolCalls, uint64(receipt.ToolCalls),
		); !ok {
			return EvaluationDimensionResult{}, fmt.Errorf("tool calls overflow")
		}
		duration := receipt.FinishedAt.Sub(receipt.StartedAt) / time.Millisecond
		if duration < 0 {
			return EvaluationDimensionResult{}, fmt.Errorf("receipt duration is negative")
		}
		if result.CumulativeDurationMS, ok = checkedEvaluationTokenAdd(
			result.CumulativeDurationMS, uint64(duration),
		); !ok {
			return EvaluationDimensionResult{}, fmt.Errorf("cumulative duration overflow")
		}
	}

	if result.ReviewTaskCount == 0 {
		result.ExecutionCoverage = MetricAvailability{
			ReasonCode: "dimension_review_receipt_missing",
		}
	} else if result.TasksFailed > 0 || result.TasksCanceled > 0 {
		result.ExecutionCoverage = MetricAvailability{
			ReasonCode: "dimension_review_tasks_incomplete",
		}
	} else {
		result.ExecutionCoverage = MetricAvailability{Available: true}
	}
	result.ContextCoverage = MetricAvailability{ReasonCode: "dimension_review_receipt_missing"}
	if result.ReviewTaskCount > 0 {
		result.ContextCoverage = MetricAvailability{Available: true}
		for _, gap := range coverageGaps {
			if gap.Phase != contractsv1alpha1.AgentReviewCoverageContext {
				continue
			}
			_, groupMatch := reviewGroups[gap.SubjectID]
			if !groupMatch {
				if contextGroup, taskMatch := contextGroupByTask[gap.SubjectID]; taskMatch {
					_, groupMatch = reviewGroups[contextGroup]
				} else if _, knownGroup := knownGroups[gap.SubjectID]; !knownGroup {
					// Context/provider gaps that are not scoped to a frozen group
					// apply to every dimension that actually had a review task.
					groupMatch = true
				}
			}
			if groupMatch {
				result.ContextGapCount++
			}
		}
	}

	usage := usageEvaluationFacts{
		completeness: contractsv1alpha1.AgentTokenUsageUnavailable,
		authority:    "unavailable",
		availability: MetricAvailability{ReasonCode: "dimension_usage_receipts_unavailable"},
	}
	if len(attributedReceipts) > 0 {
		var err error
		usage, err = aggregateUsageReceipts(attributedReceipts)
		if err != nil {
			return EvaluationDimensionResult{}, err
		}
	}
	projectDimensionUsage(&result, usage)
	result.Verdict, result.ReasonCode = scoreDimensionResult(result, report.Completeness, label.ExpectedOutcome)
	return result, nil
}

func projectDimensionUsage(result *EvaluationDimensionResult, usage usageEvaluationFacts) {
	result.UsageCompleteness = usage.completeness
	result.UsageReceiptCount = usage.receipts
	result.UsageReportedReceipts = usage.reported
	result.UsagePartialReceipts = usage.partial
	result.UsageUnavailableReceipts = usage.unavailable
	result.InputTokens = usage.input
	result.OutputTokens = usage.output
	result.CacheReadTokens = usage.cacheRead
	result.CacheWriteTokens = usage.cacheWrite
	result.ReasoningTokens = usage.reasoning
	result.ReasoningReportedReceipts = usage.reasoningReported
	result.TotalTokens = usage.total
	result.UsageAuthority = usage.authority
	result.Usage = usage.availability
}

func scoreDimensionResult(
	result EvaluationDimensionResult,
	completeness contractsv1alpha1.AgentReviewCompleteness,
	expected ExpectedOutcome,
) (EvaluationVerdict, string) {
	if !result.ExecutionCoverage.Available {
		return EvaluationInconclusive, result.ExecutionCoverage.ReasonCode
	}
	return scoreDefectPresence(expected, result.EvaluatedFindingCount, string(completeness))
}

func (result EvaluationDimensionResult) validate(parent EvaluationCaseResult) error {
	if err := validateEvaluationDimensionRef("dimension", result.Dimension); err != nil {
		return err
	}
	if result.ConfirmedCount+result.RejectedCount+result.InconclusiveCount != result.CandidateCount {
		return fmt.Errorf("candidate disposition counts do not bind candidate_count")
	}
	if result.FindingCount > result.ConfirmedCount || result.EvaluatedFindingCount > result.FindingCount {
		return fmt.Errorf("finding counts exceed governed evidence populations")
	}
	if result.ExpectedAnchorCount != parent.ExpectedAnchorCount ||
		result.MatchedAnchorCount > result.ExpectedAnchorCount ||
		result.LocalizedFindingCount > result.EvaluatedFindingCount {
		return fmt.Errorf("localization counts do not bind the parent label and findings")
	}
	if result.ReviewTaskCount+result.VerificationTaskCount !=
		result.TasksSucceeded+result.TasksFailed+result.TasksCanceled {
		return fmt.Errorf("task status counts do not bind attributed tasks")
	}
	wantCoverage := MetricAvailability{Available: true}
	if result.ReviewTaskCount == 0 {
		wantCoverage = MetricAvailability{ReasonCode: "dimension_review_receipt_missing"}
	} else if result.TasksFailed > 0 || result.TasksCanceled > 0 {
		wantCoverage = MetricAvailability{ReasonCode: "dimension_review_tasks_incomplete"}
	}
	if result.ExecutionCoverage != wantCoverage {
		return fmt.Errorf("execution_coverage is not the task receipt recomputation")
	}
	wantContextCoverage := MetricAvailability{Available: true}
	if result.ReviewTaskCount == 0 {
		wantContextCoverage = MetricAvailability{ReasonCode: "dimension_review_receipt_missing"}
	}
	if result.ContextCoverage != wantContextCoverage ||
		(!result.ContextCoverage.Available && result.ContextGapCount != 0) {
		return fmt.Errorf("context_coverage is not bound to dimension review evidence")
	}
	if result.ModelTurnsCompleted > result.ModelTurnsStarted {
		return fmt.Errorf("model turn counters are internally inconsistent")
	}
	if result.UsageReportedReceipts+result.UsagePartialReceipts+
		result.UsageUnavailableReceipts != result.UsageReceiptCount {
		return fmt.Errorf("usage receipt counts do not bind attributed receipts")
	}
	tokenTotal, ok := checkedEvaluationTokenSum(
		result.InputTokens, result.OutputTokens, result.CacheReadTokens, result.CacheWriteTokens,
	)
	if !ok || tokenTotal != result.TotalTokens || result.ReasoningTokens > result.OutputTokens ||
		result.ReasoningReportedReceipts > result.UsageReportedReceipts+result.UsagePartialReceipts {
		return fmt.Errorf("usage counters are internally inconsistent")
	}
	if result.UsageReceiptCount == 0 {
		if result.UsageAuthority != "unavailable" ||
			result.UsageCompleteness != contractsv1alpha1.AgentTokenUsageUnavailable ||
			result.Usage.Available || result.Usage.ReasonCode != "dimension_usage_receipts_unavailable" {
			return fmt.Errorf("missing dimension usage must remain explicitly unavailable")
		}
	} else if result.UsageAuthority != EvaluationUsageAuthority {
		return fmt.Errorf("unsupported dimension usage authority %q", result.UsageAuthority)
	} else if result.UsageReportedReceipts == result.UsageReceiptCount {
		if result.UsageCompleteness != contractsv1alpha1.AgentTokenUsageProviderReported ||
			!result.Usage.Available || result.Usage.ReasonCode != "" {
			return fmt.Errorf("complete dimension usage must be available")
		}
	} else {
		wantCompleteness := contractsv1alpha1.AgentTokenUsageUnavailable
		if result.UsageReportedReceipts > 0 || result.UsagePartialReceipts > 0 {
			wantCompleteness = contractsv1alpha1.AgentTokenUsagePartial
		}
		if result.UsageCompleteness != wantCompleteness || result.Usage.Available ||
			result.Usage.ReasonCode != "usage_receipts_incomplete" {
			return fmt.Errorf("incomplete dimension usage must remain unavailable")
		}
	}
	if result.NormalizationRetainedCount+result.MergedDuplicateCount+
		result.RejectedInvalidCount+result.ExcludedBudgetCount != result.RawCandidateCount {
		return fmt.Errorf("normalization counts do not bind raw_candidate_count")
	}
	if result.Normalization.Available {
		if result.Normalization.ReasonCode != "" {
			return fmt.Errorf("available normalization must not carry a reason")
		}
	} else if result.Normalization.ReasonCode != "raw_candidate_dimension_evidence_unavailable" ||
		result.RawCandidateCount != 0 {
		return fmt.Errorf("missing raw candidate evidence must remain explicitly unavailable")
	}
	if parent.ReportCompleteness == string(contractsv1alpha1.AgentReviewComplete) &&
		parent.ExpectedAnchorCount > 0 {
		if !result.Localization.Available || result.Localization.ReasonCode != "" {
			return fmt.Errorf("dimension localization must be available for complete structured evidence")
		}
	} else if result.Localization.Available || result.Localization.ReasonCode == "" ||
		result.MatchedAnchorCount != 0 || result.LocalizedFindingCount != 0 {
		return fmt.Errorf("dimension localization must be unavailable without complete structured evidence")
	}
	wantVerdict, wantReason := scoreDimensionResult(
		result, contractsv1alpha1.AgentReviewCompleteness(parent.ReportCompleteness), parent.ExpectedOutcome,
	)
	if result.Verdict != wantVerdict || result.ReasonCode != wantReason {
		return fmt.Errorf("verdict and reason_code are not the dimension metric recomputation")
	}
	return nil
}

func readCommittedDimensionEvidence(
	runs EvaluationRunReader,
	run runmodel.ReviewRun,
	report contractsv1alpha1.GovernedReviewReport,
	evidenceOwnerRunID string,
) (*contractsv1alpha1.AgentReviewRawCandidateCollection, contractsv1alpha1.ReviewHypothesisSet, error) {
	if run.HypothesisSetRef == nil {
		return nil, contractsv1alpha1.ReviewHypothesisSet{}, fmt.Errorf(
			"dimension evaluation requires a committed hypothesis set",
		)
	}
	readExact := func(ref runmodel.ArtifactRef, contract string) ([]byte, error) {
		if err := ref.Validate(); err != nil || ref.Contract != contract {
			return nil, fmt.Errorf("invalid committed %s ref", contract)
		}
		data, err := runs.ReadArtifact(ref)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != ref.SizeBytes || hex.EncodeToString(digest[:]) != ref.SHA256 {
			return nil, fmt.Errorf("committed %s bytes changed", contract)
		}
		return data, nil
	}
	setBytes, err := readExact(*run.HypothesisSetRef, runmodel.ContractReviewHypothesisSet)
	if err != nil {
		return nil, contractsv1alpha1.ReviewHypothesisSet{}, err
	}
	set, err := contractsv1alpha1.DecodeReviewHypothesisSet(setBytes)
	if err != nil {
		return nil, contractsv1alpha1.ReviewHypothesisSet{}, err
	}
	if set.ReviewRunID != evidenceOwnerRunID || set.ExecutionID != report.ExecutionID ||
		set.HypothesisSetID != report.HypothesisSetID || set.TargetDigest != report.TargetDigest {
		return nil, contractsv1alpha1.ReviewHypothesisSet{}, fmt.Errorf(
			"hypothesis set escaped governed report lineage",
		)
	}
	if run.RawCandidateCollectionRef == nil {
		return nil, set, nil
	}
	rawBytes, err := readExact(
		*run.RawCandidateCollectionRef, runmodel.ContractAgentReviewRawCandidates,
	)
	if err != nil {
		return nil, contractsv1alpha1.ReviewHypothesisSet{}, err
	}
	raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawBytes)
	if err != nil {
		return nil, contractsv1alpha1.ReviewHypothesisSet{}, err
	}
	allowed := make([]contractsv1alpha1.VersionedRef, 0)
	seen := make(map[contractsv1alpha1.VersionedRef]struct{})
	for _, candidate := range raw.RawCandidates {
		if _, exists := seen[candidate.Dimension]; exists {
			continue
		}
		seen[candidate.Dimension] = struct{}{}
		allowed = append(allowed, candidate.Dimension)
	}
	if err := contractsv1alpha1.ValidateAgentReviewRawCandidateSetBindings(raw, set, allowed); err != nil {
		return nil, contractsv1alpha1.ReviewHypothesisSet{}, err
	}
	return &raw, set, nil
}
