package evaluation

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestRecordRepeatabilityRunRequiresDirectExactReplaysAndPersists(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	caseValue := testActiveCase("experiment-case", "repo-repeatability", SplitTest, testEpoch)
	targetRef := evaluationArtifactRef("repeatability-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)

	evaluationIDs := []string{"evaluation-repeatability-baseline", "evaluation-repeatability-replay-a", "evaluation-repeatability-replay-b"}
	createdTimes := []time.Time{
		caseValue.CreatedAt.Add(2 * time.Minute),
		caseValue.CreatedAt.Add(3 * time.Minute),
		caseValue.CreatedAt.Add(4 * time.Minute),
	}
	for index, id := range evaluationIDs {
		exposure := testExposure(id, caseValue.CaseID, createdTimes[index].Add(-time.Minute), ExposureNotSeen)
		if _, err := repository.RecordExposure(
			context.Background(), exposure,
			testMutation("exposure-"+id, exposure.ObservedAt.Add(10*time.Second), RoleDatasetCurator),
		); err != nil {
			t.Fatal(err)
		}
	}

	baselineReader := newEvaluationRunReaderStub(t, "review-repeatability-baseline", targetRef, contractsv1alpha1.AgentReviewComplete)
	baselineRun := baselineReader.runs["review-repeatability-baseline"]
	baselineRun.Kind = runmodel.RunKindReview
	baselineReader.runs[baselineRun.RunID] = baselineRun
	baselineSnapshot := exactRepeatabilitySnapshot("review-repeatability-baseline")
	baselineReader.snapshots[baselineRun.RunID] = baselineSnapshot
	setApplySuggestionReport(t, baselineReader, baselineRun.RunID, "guard the pointer")

	replayAReader := newEvaluationRunReaderStub(t, "review-repeatability-a", targetRef, contractsv1alpha1.AgentReviewComplete)
	configureExactRepeatabilityReplay(t, replayAReader, baselineReader, baselineRun.RunID, "review-repeatability-a")
	replayBReader := newEvaluationRunReaderStub(t, "review-repeatability-b", targetRef, contractsv1alpha1.AgentReviewComplete)
	configureExactRepeatabilityReplay(t, replayBReader, baselineReader, baselineRun.RunID, "review-repeatability-b")
	setApplySuggestionReport(t, replayBReader, "review-repeatability-b", "guard the pointer")

	baselineEvaluation := recordTestEvaluationRun(t, repository, baselineReader, evaluationIDs[0], baselineRun.RunID, createdTimes[0])
	replayAEvaluation := recordTestEvaluationRun(t, repository, replayAReader, evaluationIDs[1], "review-repeatability-a", createdTimes[1])
	replayBEvaluation := recordTestEvaluationRun(t, repository, replayBReader, evaluationIDs[2], "review-repeatability-b", createdTimes[2])
	reader := mergeEvaluationReaders(baselineReader, replayAReader, replayBReader)
	request := RepeatabilityRunRequest{
		SchemaVersion:      RepeatabilityRunRequestSchemaVersion,
		RepeatabilityRunID: "repeatability-run-1", RepeatabilityRevision: "repeatability-v1",
		BaselineEvaluationRunID: baselineEvaluation.EvaluationRunID,
		ReplayEvaluationRunIDs:  []string{replayAEvaluation.EvaluationRunID, replayBEvaluation.EvaluationRunID},
		CreatedAt:               createdTimes[2].Add(time.Minute),
	}
	mutation := testMutation("record-repeatability", request.CreatedAt.Add(time.Minute), RoleDatasetCurator)
	run, err := repository.RecordRepeatabilityRun(context.Background(), request, mutation, reader)
	if err != nil {
		t.Fatalf("RecordRepeatabilityRun() error = %v", err)
	}
	comparison := run.Comparisons[0]
	if comparison.SampleCount != 3 || comparison.PairCount != 3 ||
		!comparison.FindingSetComparison.Available || comparison.PairwiseJaccardPPM != 333333 ||
		comparison.ExactFindingSetPairCount != 1 || comparison.ExactFindingSetPairRatePPM != 333333 ||
		comparison.FindingPresenceSampleCount != 2 || comparison.FindingPresenceRatePPM != 666667 ||
		comparison.VerdictFlipPairCount != 2 || comparison.VerdictFlipPairRatePPM != 666667 ||
		comparison.Stable || run.Summary.StableCases != 0 || run.Summary.UnstableCases != 1 ||
		run.Summary.PairwiseJaccardPPM != 333333 || run.Summary.VerdictFlipPairRatePPM != 666667 {
		t.Fatalf("repeatability metrics = %+v; summary = %+v", comparison, run.Summary)
	}

	tampered := run
	tampered.Comparisons = append([]RepeatabilityCaseComparison(nil), run.Comparisons...)
	tampered.Comparisons[0].PairwiseJaccardPPM++
	if err := tampered.Validate(); err == nil {
		t.Fatal("RepeatabilityRun.Validate() accepted a non-recomputed Jaccard value")
	}
	retry, err := repository.RecordRepeatabilityRun(context.Background(), request, mutation, reader)
	if err != nil || !reflect.DeepEqual(retry, run) {
		t.Fatalf("RecordRepeatabilityRun(idempotent) = %+v, %v", retry, err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	access := Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}}
	loaded, err := restarted.GetRepeatabilityRun(run.RepeatabilityRunID, access)
	if err != nil || !reflect.DeepEqual(loaded, run) {
		t.Fatalf("GetRepeatabilityRun(restart) = %+v, %v", loaded, err)
	}
	listed, err := restarted.ListRepeatabilityRuns(access)
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0], run) {
		t.Fatalf("ListRepeatabilityRuns(restart) = %+v, %v", listed, err)
	}

	wrongReader := mergeEvaluationReaders(baselineReader, replayAReader, replayBReader)
	wrong := wrongReader.runs["review-repeatability-a"]
	wrong.ReplayVariable = runmodel.ReplayVariablePrompt
	wrongReader.runs[wrong.RunID] = wrong
	wrongRequest := request
	wrongRequest.RepeatabilityRunID = "repeatability-run-wrong-variable"
	_, err = repository.RecordRepeatabilityRun(
		context.Background(), wrongRequest,
		testMutation("record-repeatability-wrong", mutation.At.Add(time.Minute), RoleDatasetCurator),
		wrongReader,
	)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordRepeatabilityRun(wrong variable) error = %v", err)
	}

	driftReader := mergeEvaluationReaders(baselineReader, replayAReader, replayBReader)
	driftSnapshot := driftReader.snapshots["review-repeatability-a"]
	driftSnapshot.BuildIdentity = "different-build"
	driftReader.snapshots["review-repeatability-a"] = driftSnapshot
	driftRequest := request
	driftRequest.RepeatabilityRunID = "repeatability-run-drift"
	_, err = repository.RecordRepeatabilityRun(
		context.Background(), driftRequest,
		testMutation("record-repeatability-drift", mutation.At.Add(2*time.Minute), RoleDatasetCurator),
		driftReader,
	)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordRepeatabilityRun(execution drift) error = %v", err)
	}
}

func TestRepeatabilityRunRequestRejectsAmbiguousSampleIdentity(t *testing.T) {
	request := RepeatabilityRunRequest{
		SchemaVersion:      RepeatabilityRunRequestSchemaVersion,
		RepeatabilityRunID: "repeatability-request", RepeatabilityRevision: "v1",
		BaselineEvaluationRunID: "evaluation-baseline",
		ReplayEvaluationRunIDs:  []string{"evaluation-b", "evaluation-a"},
		CreatedAt:               testEpoch,
	}
	if err := request.Validate(); err == nil {
		t.Fatal("RepeatabilityRunRequest.Validate() accepted unsorted replay IDs")
	}
	request.ReplayEvaluationRunIDs = []string{"evaluation-baseline"}
	if err := request.Validate(); err == nil {
		t.Fatal("RepeatabilityRunRequest.Validate() accepted baseline as replay sample")
	}
}

func TestRepeatabilityPartialReportMakesFindingStabilityUnavailable(t *testing.T) {
	comparison := RepeatabilityCaseComparison{
		CaseID: "case-partial", CaseType: CasePositiveLocalized, LabelRevision: 1,
		LabelPolicyRevision: "label-v1", ExpectedOutcome: OutcomeDefectPresent,
		Samples: []RepeatabilitySample{
			repeatabilitySampleFixture("evaluation-a", "review-a", string(contractsv1alpha1.AgentReviewComplete), []string{}),
			repeatabilitySampleFixture("evaluation-b", "review-b", string(contractsv1alpha1.AgentReviewPartial), []string{}),
		},
		Dimensions: []RepeatabilityDimensionComparison{},
	}
	comparison = calculateRepeatabilityCase(comparison)
	if comparison.FindingSetComparison.Available ||
		comparison.FindingSetComparison.ReasonCode != "incomplete_governed_report" || comparison.Stable {
		t.Fatalf("partial comparison = %+v", comparison)
	}
	summary := summarizeRepeatability([]RepeatabilityCaseComparison{comparison})
	if summary.FindingSetStability.Available || summary.IndeterminateCases != 1 ||
		summary.PairwiseJaccardPPM != 0 || summary.ExactFindingSetPairCount != 0 {
		t.Fatalf("partial summary = %+v", summary)
	}
}

func TestRepeatabilityDimensionRecomputesFindingExecutionAndUsageRanges(t *testing.T) {
	dimension := contractsv1alpha1.VersionedRef{
		ID: "correctness", Revision: "v1", SHA256: evaluationDigest("correctness-v1"),
	}
	sample := func(
		evaluationRunID, reviewRunID string,
		findingIDs []string,
		candidateCount, rawCandidateCount, contextGapCount, tasksFailed uint32,
		durationMS, totalTokens uint64,
		usage MetricAvailability,
		verdict EvaluationVerdict,
	) RepeatabilityDimensionSample {
		return RepeatabilityDimensionSample{
			EvaluationRunID: evaluationRunID, ReviewRunID: reviewRunID, Dimension: dimension,
			ReportCompleteness: string(contractsv1alpha1.AgentReviewComplete),
			FindingIDs:         findingIDs, FindingCount: uint32(len(findingIDs)),
			CandidateCount: candidateCount, RawCandidateCount: rawCandidateCount,
			ContextGapCount: contextGapCount, TasksFailed: tasksFailed,
			CumulativeDurationMS: durationMS, TotalTokens: totalTokens,
			ExecutionCoverage: MetricAvailability{Available: true}, Usage: usage, Verdict: verdict,
		}
	}
	comparison := calculateRepeatabilityDimension(RepeatabilityDimensionComparison{
		Dimension: dimension,
		Samples: []RepeatabilityDimensionSample{
			sample("evaluation-a", "review-a", []string{"finding-x", "finding-y"}, 2, 3, 0, 0, 100, 1_000, MetricAvailability{Available: true}, EvaluationPass),
			sample("evaluation-b", "review-b", []string{"finding-x"}, 1, 2, 1, 1, 140, 1_400, MetricAvailability{Available: true}, EvaluationPass),
			sample("evaluation-c", "review-c", []string{"finding-x", "finding-z"}, 2, 4, 0, 0, 90, 900, MetricAvailability{ReasonCode: "usage_unavailable"}, EvaluationFail),
		},
	})
	if err := comparison.validate(); err != nil {
		t.Fatalf("RepeatabilityDimensionComparison.validate() error = %v", err)
	}
	if comparison.SampleCount != 3 || comparison.PairCount != 3 ||
		comparison.PairwiseJaccardPPM != 444444 || comparison.ExactFindingSetPairCount != 0 ||
		comparison.FindingPresenceRatePPM != repeatabilityRateScale ||
		comparison.VerdictFlipPairCount != 2 || comparison.VerdictFlipPairRatePPM != 666667 ||
		comparison.CandidateCountMin != 1 || comparison.CandidateCountMax != 2 ||
		comparison.RawCandidateCountMin != 2 || comparison.RawCandidateCountMax != 4 ||
		comparison.ContextGapCountMin != 0 || comparison.ContextGapCountMax != 1 ||
		comparison.TasksFailedMin != 0 || comparison.TasksFailedMax != 1 ||
		comparison.CumulativeDurationMSMin != 90 || comparison.CumulativeDurationMSMax != 140 ||
		comparison.TotalTokensMin != 900 || comparison.TotalTokensMax != 1_400 ||
		!comparison.ExecutionComparison.Available || comparison.UsageComparison.Available ||
		comparison.UsageComparison.ReasonCode != "dimension_usage_sample_unavailable" || comparison.Stable {
		t.Fatalf("dimension comparison = %+v", comparison)
	}
	tampered := comparison
	tampered.TotalTokensMax++
	if err := tampered.validate(); err == nil {
		t.Fatal("dimension comparison accepted a non-recomputed token range")
	}
}

func exactRepeatabilitySnapshot(seed string) runmodel.ExecutionSnapshot {
	configDigest := evaluationDigest("repeatability-config")
	return runmodel.ExecutionSnapshot{
		TargetSnapshotRef:     evaluationArtifactRef("repeatability-target", runmodel.ContractMaterializedTarget),
		ReviewInputRef:        evaluationArtifactRef("repeatability-input", runmodel.ContractReviewInput),
		WorkflowDefinitionRef: evaluationArtifactRef("repeatability-workflow", runmodel.ContractWorkflowDefinition),
		ConfigBundleRef:       evaluationArtifactRef("repeatability-config", runmodel.ContractConfigBundle),
		Workflow:              runmodel.WorkflowRef{ID: "review-workflow", Revision: "v1", SHA256: evaluationDigest("repeatability-workflow-ref")},
		Config:                runmodel.PolicyRef{ID: "review-config", Revision: "v1", SHA256: configDigest},
		RuntimeProfile:        "pi-local", BuildIdentity: "argus-test", RemoteWrites: "deny",
		ToolPolicy:          runmodel.ToolInvocationPolicy{AllowedTools: []string{}, RemoteWrites: "deny"},
		ReplayInputRefs:     []runmodel.ArtifactRef{},
		ExecutionSnapshotID: seed + "-snapshot",
	}
}

func configureExactRepeatabilityReplay(
	t *testing.T,
	replayReader *evaluationRunReaderStub,
	baselineReader *evaluationRunReaderStub,
	baselineRunID string,
	replayRunID string,
) {
	t.Helper()
	replay := replayReader.runs[replayRunID]
	replay.Kind = runmodel.RunKindReplay
	replay.SourceRunID = baselineRunID
	replay.ReplayRootRunID = baselineRunID
	replay.ReplayVariable = runmodel.ReplayVariableNone
	replayReader.runs[replayRunID] = replay

	baselineRef := baselineReader.runRefs[baselineRunID]
	changeRef := evaluationArtifactRef(replayRunID+"-change", runmodel.ContractReplayChangeSet)
	snapshot := exactRepeatabilitySnapshot(replayRunID)
	snapshot.ReplaySourceRunRef = &baselineRef
	snapshot.ReplayChangeSetRef = &changeRef
	replayReader.snapshots[replayRunID] = snapshot
	replayReader.changes[changeRef.URI] = runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     replayRunID, SourceRunID: baselineRunID, RootRunID: baselineRunID,
		StartStage: "agent_hypothesize", Variable: runmodel.ReplayVariableNone,
		BaselineSHA256: snapshot.Config.SHA256, VariantSHA256: snapshot.Config.SHA256,
		ChangedFields: []string{}, RemoteWrites: "deny", CreatedAt: testEpoch.Add(time.Hour),
	}
}

func repeatabilitySampleFixture(
	evaluationRunID string,
	reviewRunID string,
	completeness string,
	findingIDs []string,
) RepeatabilitySample {
	return RepeatabilitySample{
		EvaluationRunID: evaluationRunID, ReviewRunID: reviewRunID,
		ReviewRunRef:       evaluationArtifactRef(reviewRunID+"-run", runmodel.ContractReviewRun),
		GovernedReportRef:  evaluationArtifactRef(reviewRunID+"-report", runmodel.ContractGovernedReviewReport),
		ReportCompleteness: completeness, FindingIDs: findingIDs,
		FindingCount: uint32(len(findingIDs)), Localization: MetricAvailability{ReasonCode: "localization_unavailable"},
		Verdict: EvaluationInconclusive,
	}
}
