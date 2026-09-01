package evaluation

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestRecordExperimentRunRequiresExactReplayLineageAndPersists(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	caseValue := testActiveCase("experiment-case", "repo-experiment", SplitTest, testEpoch)
	targetRef := evaluationArtifactRef("experiment-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)

	baseCreated := caseValue.CreatedAt.Add(2 * time.Minute)
	variantCreated := baseCreated.Add(time.Minute)
	for _, exposure := range []Exposure{
		testExposure("evaluation-baseline", caseValue.CaseID,
			baseCreated.Add(-time.Minute), ExposureNotSeen),
		testExposure("evaluation-variant", caseValue.CaseID,
			variantCreated.Add(-time.Minute), ExposureNotSeen),
	} {
		if _, err := repository.RecordExposure(context.Background(), exposure,
			testMutation("exposure-"+exposure.EvaluationRunID,
				exposure.ObservedAt.Add(10*time.Second), RoleDatasetCurator)); err != nil {
			t.Fatal(err)
		}
	}
	baselineReader := newEvaluationRunReaderStub(t, "review-baseline", targetRef,
		contractsv1alpha1.AgentReviewComplete)
	baselineReader.runs["review-baseline"] = func() runmodel.ReviewRun {
		run := baselineReader.runs["review-baseline"]
		run.Kind = runmodel.RunKindReview
		return run
	}()
	attachUsageReceipts(
		t, baselineReader, "review-baseline",
		contractsv1alpha1.AgentTokenUsageProviderReported, 100, 50,
	)
	variantReader := newEvaluationRunReaderStub(t, "review-variant", targetRef,
		contractsv1alpha1.AgentReviewComplete)
	variantRun := variantReader.runs["review-variant"]
	variantRun.Kind = runmodel.RunKindReplay
	variantRun.SourceRunID = "review-baseline"
	variantRun.ReplayVariable = runmodel.ReplayVariablePrompt
	variantFinishedAt := variantRun.StageAttempts[0].StartedAt.Add(70 * time.Millisecond)
	variantRun.StageAttempts[0].FinishedAt = &variantFinishedAt
	variantRun.StageAttempts[0].DurationMS = 70
	variantReader.runs["review-variant"] = variantRun
	attachUsageReceipts(
		t, variantReader, "review-variant",
		contractsv1alpha1.AgentTokenUsageProviderReported, 80, 40,
	)
	changeRef := evaluationArtifactRef("review-variant-change", runmodel.ContractReplayChangeSet)
	variantSnapshot := variantReader.snapshots["review-variant"]
	variantSnapshot.ReplayChangeSetRef = &changeRef
	variantReader.snapshots["review-variant"] = variantSnapshot
	variantReader.changes[changeRef.URI] = runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     "experiment-review-variant", SourceRunID: "review-baseline",
		RootRunID: "review-baseline", StartStage: "agent_hypothesize",
		Variable:       runmodel.ReplayVariablePrompt,
		BaselineSHA256: evaluationDigest("baseline-config"),
		VariantSHA256:  evaluationDigest("variant-config"),
		ChangedFields:  []string{"agent_review.prompt"}, RemoteWrites: "deny",
		CreatedAt: testEpoch.Add(time.Hour),
	}

	baseline := recordTestEvaluationRun(t, repository, baselineReader,
		"evaluation-baseline", "review-baseline", baseCreated)
	variant := recordTestEvaluationRun(t, repository, variantReader,
		"evaluation-variant", "review-variant", variantCreated)
	reader := mergeEvaluationReaders(baselineReader, variantReader)
	request := ExperimentRunRequest{
		SchemaVersion: ExperimentRunRequestSchemaVersion, ExperimentRunID: "experiment-run-1",
		ExperimentRevision:      "experiment-revision-1",
		BaselineEvaluationRunID: baseline.EvaluationRunID,
		VariantEvaluationRunID:  variant.EvaluationRunID,
		Variable:                runmodel.ReplayVariablePrompt, CreatedAt: variantCreated.Add(time.Minute),
	}
	mutation := testMutation("record-experiment", request.CreatedAt.Add(time.Minute), RoleDatasetCurator)
	experiment, err := repository.RecordExperimentRun(context.Background(), request, mutation, reader)
	if err != nil {
		t.Fatalf("RecordExperimentRun() error = %v", err)
	}
	if experiment.Summary.Cases != 1 || experiment.Summary.Unchanged != 1 ||
		experiment.Comparisons[0].Transition != ExperimentUnchanged ||
		experiment.Cost.Available || experiment.Cost.ReasonCode == "" ||
		!experiment.Latency.Available || experiment.Latency.ReasonCode != "" ||
		experiment.Comparisons[0].BaselineDurationMS != 100 ||
		experiment.Comparisons[0].VariantDurationMS != 70 ||
		experiment.Comparisons[0].LatencyDeltaMS != -30 ||
		experiment.Comparisons[0].LatencyAuthority != "review_run_stage_attempts" ||
		!experiment.Comparisons[0].Localization.Available ||
		experiment.Comparisons[0].ExpectedAnchorCount != 1 ||
		experiment.Comparisons[0].MatchedAnchorDelta != 0 ||
		experiment.Comparisons[0].FilterEfficacy.Available ||
		experiment.Comparisons[0].FilterEfficacy.ReasonCode != "paired_filter_efficacy_unavailable" ||
		!experiment.Usage.Available || !experiment.Comparisons[0].Usage.Available ||
		experiment.Comparisons[0].InputTokenDelta != -20 ||
		experiment.Comparisons[0].OutputTokenDelta != -10 ||
		experiment.Comparisons[0].TotalTokenDelta != -30 ||
		experiment.Cost.ReasonCode != "authoritative_billing_unavailable" {
		t.Fatalf("experiment = %+v", experiment)
	}
	tamperedLatency := experiment
	tamperedLatency.Comparisons = append([]ExperimentCaseComparison(nil), experiment.Comparisons...)
	tamperedLatency.Comparisons[0].LatencyDeltaMS++
	if err := tamperedLatency.Validate(); err == nil {
		t.Fatal("ExperimentRun.Validate() accepted a latency delta not supported by durations")
	}
	tamperedLocalization := experiment
	tamperedLocalization.Comparisons = append([]ExperimentCaseComparison(nil), experiment.Comparisons...)
	tamperedLocalization.Comparisons[0].MatchedAnchorDelta++
	if err := tamperedLocalization.Validate(); err == nil {
		t.Fatal("ExperimentRun.Validate() accepted a localization delta not supported by counts")
	}
	tamperedUsage := experiment
	tamperedUsage.Comparisons = append([]ExperimentCaseComparison(nil), experiment.Comparisons...)
	tamperedUsage.Comparisons[0].TotalTokenDelta++
	if err := tamperedUsage.Validate(); err == nil {
		t.Fatal("ExperimentRun.Validate() accepted a token delta not supported by receipt facts")
	}
	filterBaseline := baseline
	filterBaseline.Results = append([]EvaluationCaseResult(nil), baseline.Results...)
	filterBaseline.Results[0].ExpectedSuppressionCount = 1
	filterBaseline.Results[0].ObservedSuppressionCount = 1
	filterBaseline.Results[0].EscapedTargetCount = 1
	filterBaseline.Results[0].FilterEfficacy = MetricAvailability{Available: true}
	filterVariant := variant
	filterVariant.Results = append([]EvaluationCaseResult(nil), variant.Results...)
	filterVariant.Results[0].ExpectedSuppressionCount = 1
	filterVariant.Results[0].ObservedSuppressionCount = 1
	filterVariant.Results[0].SuppressedTargetCount = 1
	filterVariant.Results[0].FilterEfficacy = MetricAvailability{Available: true}
	filterComparisons, err := buildExperimentComparisons(
		reader, filterBaseline, filterVariant, runmodel.ReplayVariablePrompt,
	)
	if err != nil || len(filterComparisons) != 1 ||
		!filterComparisons[0].FilterEfficacy.Available ||
		filterComparisons[0].SuppressedTargetDelta != 1 ||
		filterComparisons[0].EscapedTargetDelta != -1 {
		t.Fatalf("buildExperimentComparisons(filter efficacy) = %+v, %v", filterComparisons, err)
	}
	tamperedFilter := filterComparisons[0]
	tamperedFilter.SuppressedTargetDelta++
	if err := tamperedFilter.validate(); err == nil {
		t.Fatal("ExperimentCaseComparison.validate() accepted an unsupported suppression delta")
	}
	applyComparison := experiment.Comparisons[0]
	applyComparison.CaseType = CaseFixValidation
	applyComparison.BaselineApplyChecksPassed = 3
	applyComparison.VariantApplyChecksPassed = 2
	applyComparison.ApplyChecksPassedDelta = -1
	applyComparison.BaselineApplyChecksFailed = 0
	applyComparison.VariantApplyChecksFailed = 1
	applyComparison.ApplyChecksFailedDelta = 1
	applyComparison.BaselineApplyAuthority = ApplyTrialAuthority
	applyComparison.VariantApplyAuthority = ApplyTrialAuthority
	applyComparison.BaselineApplyFidelity = MetricAvailability{Available: true}
	applyComparison.VariantApplyFidelity = MetricAvailability{Available: true}
	applyComparison.ApplyFidelity = MetricAvailability{Available: true}
	if err := applyComparison.validate(); err != nil {
		t.Fatalf("ExperimentCaseComparison.validate(apply) error = %v", err)
	}
	applyComparison.ApplyChecksFailedDelta++
	if err := applyComparison.validate(); err == nil {
		t.Fatal("ExperimentCaseComparison.validate() accepted an unsupported apply delta")
	}
	retry, err := repository.RecordExperimentRun(context.Background(), request, mutation, reader)
	if err != nil || !reflect.DeepEqual(retry, experiment) {
		t.Fatalf("RecordExperimentRun(idempotent) = %+v, %v", retry, err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.GetExperimentRun(experiment.ExperimentRunID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || !reflect.DeepEqual(loaded, experiment) {
		t.Fatalf("GetExperimentRun(restart) = %+v, %v", loaded, err)
	}
	listed, err := restarted.ListExperimentRuns(
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}},
	)
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0], experiment) {
		t.Fatalf("ListExperimentRuns(restart) = %+v, %v", listed, err)
	}

	missingLatencyReader := mergeEvaluationReaders(baselineReader, variantReader)
	missingLatencyRun := missingLatencyReader.runs["review-variant"]
	missingLatencyRun.StageAttempts = nil
	missingLatencyReader.runs["review-variant"] = missingLatencyRun
	missingLatency := request
	missingLatency.ExperimentRunID = "experiment-run-missing-latency"
	_, err = repository.RecordExperimentRun(context.Background(), missingLatency,
		testMutation("record-missing-latency", mutation.At.Add(time.Minute), RoleDatasetCurator),
		missingLatencyReader)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordExperimentRun(missing latency evidence) error = %v", err)
	}

	wrongVariable := request
	wrongVariable.ExperimentRunID = "experiment-run-wrong-variable"
	wrongVariable.Variable = runmodel.ReplayVariableModel
	_, err = repository.RecordExperimentRun(context.Background(), wrongVariable,
		testMutation("record-wrong-variable", mutation.At.Add(time.Minute), RoleDatasetCurator), reader)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordExperimentRun(wrong variable) error = %v", err)
	}
}

func TestCompareEvaluationVerdictsDoesNotRankInconclusiveAsQuality(t *testing.T) {
	tests := []struct {
		before, after EvaluationVerdict
		transition    ExperimentTransition
	}{
		{EvaluationFail, EvaluationPass, ExperimentImproved},
		{EvaluationPass, EvaluationFail, ExperimentRegressed},
		{EvaluationPass, EvaluationPass, ExperimentUnchanged},
		{EvaluationInconclusive, EvaluationPass, ExperimentInconclusive},
		{EvaluationFail, EvaluationInconclusive, ExperimentInconclusive},
	}
	for _, test := range tests {
		transition, _ := compareEvaluationVerdicts(test.before, test.after)
		if transition != test.transition {
			t.Fatalf("compareEvaluationVerdicts(%q, %q) = %q", test.before, test.after, transition)
		}
	}
}

func recordTestEvaluationRun(
	t *testing.T,
	repository *Repository,
	reader *evaluationRunReaderStub,
	evaluationRunID string,
	reviewRunID string,
	createdAt time.Time,
) EvaluationRun {
	t.Helper()
	run, err := repository.RecordEvaluationRun(context.Background(), EvaluationRunRequest{
		SchemaVersion: EvaluationRunRequestSchemaVersion, EvaluationRunID: evaluationRunID,
		EvaluatorRevision: "presence-evaluator-1",
		Bindings: []EvaluationCaseRunBinding{{
			CaseID: "experiment-case", ExpectedLabelRevision: 1, ReviewRunID: reviewRunID,
		}}, CreatedAt: createdAt,
	}, testMutation("record-"+evaluationRunID, createdAt.Add(30*time.Second), RoleDatasetCurator), reader)
	if err != nil {
		t.Fatalf("RecordEvaluationRun(%s) error = %v", evaluationRunID, err)
	}
	return run
}

func mergeEvaluationReaders(readers ...*evaluationRunReaderStub) *evaluationRunReaderStub {
	merged := &evaluationRunReaderStub{
		runs: make(map[string]runmodel.ReviewRun), snapshots: make(map[string]runmodel.ExecutionSnapshot),
		reports:      make(map[string]contractsv1alpha1.GovernedReviewReport),
		verification: make(map[string]contractsv1alpha1.CandidateVerificationLedger),
		calibrations: make(map[string]contractsv1alpha1.FindingCalibrationLedger),
		suppressions: make(map[string]contractsv1alpha1.FindingSuppressionLedger),
		changes:      make(map[string]runmodel.ReplayChangeSet),
		configs:      make(map[string]reviewconfig.ConfigBundle),
		artifacts:    make(map[string][]byte),
		runRefs:      make(map[string]runmodel.ArtifactRef),
	}
	for _, reader := range readers {
		for key, value := range reader.runs {
			merged.runs[key] = value
		}
		for key, value := range reader.snapshots {
			merged.snapshots[key] = value
		}
		for key, value := range reader.reports {
			merged.reports[key] = value
		}
		for key, value := range reader.verification {
			merged.verification[key] = value
		}
		for key, value := range reader.calibrations {
			merged.calibrations[key] = value
		}
		for key, value := range reader.suppressions {
			merged.suppressions[key] = value
		}
		for key, value := range reader.changes {
			merged.changes[key] = value
		}
		for key, value := range reader.configs {
			merged.configs[key] = value
		}
		for key, value := range reader.artifacts {
			merged.artifacts[key] = append([]byte(nil), value...)
		}
		for key, value := range reader.runRefs {
			merged.runRefs[key] = value
		}
	}
	return merged
}
