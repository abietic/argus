package evaluation

import (
	"testing"
	"time"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestEvaluateDimensionRequiresExactExecutionProofAndAttributesUsage(t *testing.T) {
	dimension := contractsv1alpha1.VersionedRef{
		ID: "correctness", Revision: "builtin-v2", SHA256: evaluationDigest("correctness-v2"),
	}
	receipt := evaluationDimensionReceipt(
		dimension, contractsv1alpha1.AgentTaskReview, nil, 40, 10,
	)
	result, err := evaluateDimension(
		dimension,
		testDefectLabel("high"),
		contractsv1alpha1.GovernedReviewReport{
			Completeness: contractsv1alpha1.AgentReviewComplete,
		},
		nil,
		[]contractsv1alpha1.AgentExecutionReceipt{receipt},
		map[string]contractsv1alpha1.VersionedRef{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ExecutionCoverage.Available || result.ReviewTaskCount != 1 ||
		result.Verdict != EvaluationFail || result.ReasonCode != "expected_defect_missed" {
		t.Fatalf("dimension quality = %+v", result)
	}
	if !result.Usage.Available || result.UsageReceiptCount != 1 ||
		result.InputTokens != 40 || result.OutputTokens != 10 || result.TotalTokens != 50 ||
		result.CumulativeDurationMS != 250 {
		t.Fatalf("dimension usage = %+v", result)
	}
	if result.Normalization.Available ||
		result.Normalization.ReasonCode != "raw_candidate_dimension_evidence_unavailable" {
		t.Fatalf("dimension normalization = %+v", result.Normalization)
	}
	parent := EvaluationCaseResult{
		ExpectedOutcome:     OutcomeDefectPresent,
		ExpectedAnchorCount: uint32(len(testDefectLabel("high").Anchors)),
		ReportCompleteness:  string(contractsv1alpha1.AgentReviewComplete),
	}
	if err := result.validate(parent); err != nil {
		t.Fatalf("dimension result validation = %v", err)
	}

	withoutReceipt, err := evaluateDimension(
		dimension,
		testDefectLabel("high"),
		contractsv1alpha1.GovernedReviewReport{
			Completeness: contractsv1alpha1.AgentReviewComplete,
		},
		nil,
		nil,
		map[string]contractsv1alpha1.VersionedRef{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if withoutReceipt.Verdict != EvaluationInconclusive ||
		withoutReceipt.ReasonCode != "dimension_review_receipt_missing" ||
		withoutReceipt.Usage.ReasonCode != "dimension_usage_receipts_unavailable" {
		t.Fatalf("unproven dimension = %+v", withoutReceipt)
	}
}

func TestEvaluateDimensionJoinsVerifierByHypothesisOccurrence(t *testing.T) {
	dimension := contractsv1alpha1.VersionedRef{
		ID: "security-contract", Revision: "builtin-v1", SHA256: evaluationDigest("security-v1"),
	}
	verifier := contractsv1alpha1.VersionedRef{
		ID: "independent-verifier", Revision: "builtin-v1", SHA256: evaluationDigest("verifier-v1"),
	}
	occurrenceID := "occurrence-security-1"
	reviewReceipt := evaluationDimensionReceipt(
		dimension, contractsv1alpha1.AgentTaskReview, nil, 30, 5,
	)
	verificationReceipt := evaluationDimensionReceipt(
		verifier, contractsv1alpha1.AgentTaskVerification, &occurrenceID, 20, 5,
	)
	verificationReceipt.ReceiptID = "receipt-verification"
	verificationReceipt.TaskID = "task-verification"
	verificationReceipt.ToolUsage[0].ToolID = "submit_verdict"
	report := contractsv1alpha1.GovernedReviewReport{
		Completeness: contractsv1alpha1.AgentReviewComplete,
		Candidates: []contractsv1alpha1.GovernedReviewCandidate{{
			Hypothesis: contractsv1alpha1.ReviewHypothesis{
				OccurrenceID: occurrenceID, Dimension: dimension,
			},
			Disposition: contractsv1alpha1.GovernedCandidateRejected,
		}},
	}
	result, err := evaluateDimension(
		dimension,
		Label{
			ExpectedOutcome: OutcomeClean, Anchors: []LabelAnchor{},
			AnchorRefs: []string{}, SuppressionTargets: []SuppressionTarget{},
		},
		report,
		nil,
		[]contractsv1alpha1.AgentExecutionReceipt{reviewReceipt, verificationReceipt},
		map[string]contractsv1alpha1.VersionedRef{occurrenceID: dimension},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewTaskCount != 1 || result.VerificationTaskCount != 1 ||
		result.CandidateCount != 1 || result.RejectedCount != 1 ||
		result.UsageReceiptCount != 2 || result.TotalTokens != 60 {
		t.Fatalf("joined dimension evidence = %+v", result)
	}
}

func TestEvaluateDimensionAttributesOnlyGlobalAndReviewGroupContextGaps(t *testing.T) {
	dimension := contractsv1alpha1.VersionedRef{
		ID: "resource-lifecycle", Revision: "builtin-v1",
		SHA256: evaluationDigest("resource-lifecycle-v1"),
	}
	reviewReceipt := evaluationDimensionReceipt(
		dimension, contractsv1alpha1.AgentTaskReview, nil, 30, 5,
	)
	contextDimension := contractsv1alpha1.VersionedRef{
		ID: "code-context", Revision: "builtin-v1", SHA256: evaluationDigest("context-v1"),
	}
	contextReceipt := evaluationDimensionReceipt(
		contextDimension, contractsv1alpha1.AgentTaskContext, nil, 100, 20,
	)
	contextReceipt.ReceiptID = "receipt-context"
	contextReceipt.TaskID = "task-context"
	contextReceipt.ToolUsage[0].ToolID = "submit_context"
	otherContext := contextReceipt
	otherContext.ReceiptID = "receipt-context-other"
	otherContext.TaskID = "task-context-other"
	otherContext.GroupID = "group-other"
	coverage := []contractsv1alpha1.AgentReviewCoverageGap{
		{GapID: "gap-global", Phase: contractsv1alpha1.AgentReviewCoverageContext,
			SubjectID: "repository-context", ReasonCode: "provider_gap"},
		{GapID: "gap-group", Phase: contractsv1alpha1.AgentReviewCoverageContext,
			SubjectID: reviewReceipt.GroupID, ReasonCode: "group_gap"},
		{GapID: "gap-task", Phase: contractsv1alpha1.AgentReviewCoverageContext,
			SubjectID: contextReceipt.TaskID, ReasonCode: "context_task_failed"},
		{GapID: "gap-other", Phase: contractsv1alpha1.AgentReviewCoverageContext,
			SubjectID: otherContext.GroupID, ReasonCode: "other_group_gap"},
		{GapID: "gap-review", Phase: contractsv1alpha1.AgentReviewCoverageReview,
			SubjectID: reviewReceipt.TaskID, ReasonCode: "review_gap"},
	}
	result, err := evaluateDimension(
		dimension,
		Label{ExpectedOutcome: OutcomeClean, Anchors: []LabelAnchor{},
			AnchorRefs: []string{}, SuppressionTargets: []SuppressionTarget{}},
		contractsv1alpha1.GovernedReviewReport{Completeness: contractsv1alpha1.AgentReviewComplete},
		coverage,
		[]contractsv1alpha1.AgentExecutionReceipt{reviewReceipt, contextReceipt, otherContext},
		map[string]contractsv1alpha1.VersionedRef{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ContextCoverage.Available || result.ContextGapCount != 3 ||
		result.UsageReceiptCount != 1 || result.TotalTokens != 35 {
		t.Fatalf("dimension context gap attribution = %+v", result)
	}
}

func TestEvaluationDimensionScopeRequiresSortedExactVersions(t *testing.T) {
	valid := contractsv1alpha1.VersionedRef{
		ID: "correctness", Revision: "builtin-v2", SHA256: evaluationDigest("correctness-v2"),
	}
	if err := validateEvaluationDimensionScope([]contractsv1alpha1.VersionedRef{valid}); err != nil {
		t.Fatal(err)
	}
	duplicate := []contractsv1alpha1.VersionedRef{valid, valid}
	if err := validateEvaluationDimensionScope(duplicate); err == nil {
		t.Fatal("duplicate dimension scope was accepted")
	}
	invalid := valid
	invalid.SHA256 = "not-a-digest"
	if err := validateEvaluationDimensionScope([]contractsv1alpha1.VersionedRef{invalid}); err == nil {
		t.Fatal("invalid exact dimension digest was accepted")
	}
}

func TestBuildEvaluationDimensionComparisonsPreservesExactVersions(t *testing.T) {
	baselineDimension := contractsv1alpha1.VersionedRef{
		ID: "correctness", Revision: "builtin-v1", SHA256: evaluationDigest("correctness-v1"),
	}
	variantDimension := contractsv1alpha1.VersionedRef{
		ID: "correctness", Revision: "builtin-v2", SHA256: evaluationDigest("correctness-v2"),
	}
	baseline := EvaluationCaseResult{Dimensions: []EvaluationDimensionResult{{
		Dimension: baselineDimension, CandidateCount: 2, FindingCount: 1,
		RawCandidateCount: 3, NormalizationRetainedCount: 2, MergedDuplicateCount: 1,
		ContextGapCount:       2,
		EvaluatedFindingCount: 1, CumulativeDurationMS: 500, TotalTokens: 100,
		ExecutionCoverage: MetricAvailability{Available: true},
		ContextCoverage:   MetricAvailability{Available: true},
		Normalization:     MetricAvailability{Available: true},
		Usage:             MetricAvailability{Available: true},
		Verdict:           EvaluationFail,
	}}}
	variant := EvaluationCaseResult{Dimensions: []EvaluationDimensionResult{{
		Dimension: variantDimension, CandidateCount: 3, FindingCount: 2,
		RawCandidateCount: 5, NormalizationRetainedCount: 3,
		MergedDuplicateCount: 1, RejectedInvalidCount: 1,
		ContextGapCount:       1,
		EvaluatedFindingCount: 2, CumulativeDurationMS: 450, TotalTokens: 80,
		ExecutionCoverage: MetricAvailability{Available: true},
		ContextCoverage:   MetricAvailability{Available: true},
		Normalization:     MetricAvailability{Available: true},
		Usage:             MetricAvailability{Available: true},
		Verdict:           EvaluationPass,
	}}}
	comparisons, err := buildEvaluationDimensionComparisons(baseline, variant)
	if err != nil {
		t.Fatal(err)
	}
	if len(comparisons) != 1 || comparisons[0].DimensionID != "correctness" ||
		comparisons[0].BaselineDimension.Revision != "builtin-v1" ||
		comparisons[0].VariantDimension.Revision != "builtin-v2" ||
		comparisons[0].CandidateDelta != 1 || comparisons[0].FindingDelta != 1 ||
		comparisons[0].RawCandidateDelta != 2 || comparisons[0].RetainedDelta != 1 ||
		comparisons[0].MergedDuplicateDelta != 0 ||
		comparisons[0].RejectedInvalidDelta != 1 ||
		!comparisons[0].Normalization.Available ||
		comparisons[0].ContextGapDelta != -1 ||
		!comparisons[0].ContextCoverage.Available ||
		comparisons[0].CumulativeDurationDeltaMS != -50 ||
		comparisons[0].TotalTokenDelta != -20 ||
		comparisons[0].Transition != ExperimentImproved {
		t.Fatalf("dimension comparisons = %+v", comparisons)
	}
	variant.Dimensions[0].Dimension.ID = "security-contract"
	if _, err := buildEvaluationDimensionComparisons(baseline, variant); err == nil {
		t.Fatal("dimension ID mismatch was accepted")
	}
}

func evaluationDimensionReceipt(
	dimension contractsv1alpha1.VersionedRef,
	role contractsv1alpha1.AgentTaskRole,
	occurrenceID *string,
	inputTokens uint64,
	outputTokens uint64,
) contractsv1alpha1.AgentExecutionReceipt {
	versioned := func(id string) contractsv1alpha1.VersionedRef {
		return contractsv1alpha1.VersionedRef{
			ID: id, Revision: "1", SHA256: evaluationDigest("dimension-receipt-" + id),
		}
	}
	promptDigest := evaluationDigest("dimension-prompt")
	outputDigest := evaluationDigest("dimension-output")
	toolID := "submit_candidates"
	if role == contractsv1alpha1.AgentTaskVerification {
		toolID = "submit_verdict"
	}
	return contractsv1alpha1.AgentExecutionReceipt{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptSchemaVersion,
		ReceiptID:     "receipt-review", PlanID: "plan-dimension", SourceRunID: "review-run-dimension",
		ExecutionID: "execution-dimension", ReviewRunID: "review-run-dimension",
		TaskID: "task-review", GroupID: "group-dimension",
		HypothesisOccurrenceID: occurrenceID,
		TaskRole:               role,
		Dimension:              dimension,
		Runtime:                versioned("node-runtime"), Profile: versioned("local-shadow"),
		Agent: versioned("pi-agent"), Provider: versioned("deepseek-anthropic-env"),
		Model:           versioned("deepseek-model"),
		APIProtocol:     contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
		ProvenanceClass: contractsv1alpha1.AgentReceiptProvenanceWorkerSelfReport,
		Authority:       contractsv1alpha1.AgentReceiptAuthorityDiagnosticOnly,
		Status:          contractsv1alpha1.AgentTaskSucceeded,
		PromptDigest:    &promptDigest, OutputDigest: &outputDigest,
		StartedAt: testEpoch, FinishedAt: testEpoch.Add(250 * time.Millisecond),
		ModelTurnsStarted: 1, ModelTurnsCompleted: 1, ToolCalls: 1,
		ToolUsage: []contractsv1alpha1.AgentToolUsage{{
			ToolID: toolID, InvocationCount: 1,
		}},
		Usage: contractsv1alpha1.AgentTokenUsage{
			Completeness: contractsv1alpha1.AgentTokenUsageProviderReported,
			InputTokens:  inputTokens, OutputTokens: outputTokens,
			TotalTokens: inputTokens + outputTokens,
		},
	}
}
