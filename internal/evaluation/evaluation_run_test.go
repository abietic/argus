package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type evaluationRunReaderStub struct {
	runs             map[string]runmodel.ReviewRun
	snapshots        map[string]runmodel.ExecutionSnapshot
	reports          map[string]contractsv1alpha1.GovernedReviewReport
	verification     map[string]contractsv1alpha1.CandidateVerificationLedger
	calibrations     map[string]contractsv1alpha1.FindingCalibrationLedger
	suppressions     map[string]contractsv1alpha1.FindingSuppressionLedger
	changes          map[string]runmodel.ReplayChangeSet
	configs          map[string]reviewconfig.ConfigBundle
	artifacts        map[string][]byte
	runRefs          map[string]runmodel.ArtifactRef
	onLoad           func()
	eligibilityError error
}

func (stub *evaluationRunReaderStub) CheckArtifactEligibility(
	runmodel.ArtifactRef,
	runrepo.ArtifactUse,
) error {
	return stub.eligibilityError
}

func (stub *evaluationRunReaderStub) ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	data, exists := stub.artifacts[ref.URI]
	if !exists {
		return nil, errors.New("missing artifact")
	}
	return append([]byte(nil), data...), nil
}

func (stub *evaluationRunReaderStub) LoadRun(id string) (runmodel.ReviewRun, error) {
	if stub.onLoad != nil {
		hook := stub.onLoad
		stub.onLoad = nil
		hook()
	}
	run, exists := stub.runs[id]
	if !exists {
		return runmodel.ReviewRun{}, errors.New("missing run")
	}
	return run, nil
}

func (stub *evaluationRunReaderStub) ExecutionSnapshotForRun(id string) (runmodel.ExecutionSnapshot, error) {
	snapshot, exists := stub.snapshots[id]
	if !exists {
		return runmodel.ExecutionSnapshot{}, errors.New("missing snapshot")
	}
	return snapshot, nil
}

func (stub *evaluationRunReaderStub) ReadJSONArtifact(ref runmodel.ArtifactRef, out any) error {
	switch target := out.(type) {
	case *contractsv1alpha1.GovernedReviewReport:
		report, exists := stub.reports[ref.URI]
		if !exists {
			return errors.New("missing report")
		}
		*target = report
		return nil
	case *contractsv1alpha1.CandidateVerificationLedger:
		ledger, exists := stub.verification[ref.URI]
		if !exists {
			return errors.New("missing verification ledger")
		}
		*target = ledger
		return nil
	case *contractsv1alpha1.FindingCalibrationLedger:
		ledger, exists := stub.calibrations[ref.URI]
		if !exists {
			return errors.New("missing calibration ledger")
		}
		*target = ledger
		return nil
	case *contractsv1alpha1.FindingSuppressionLedger:
		ledger, exists := stub.suppressions[ref.URI]
		if !exists {
			return errors.New("missing suppression ledger")
		}
		*target = ledger
		return nil
	case *runmodel.ReplayChangeSet:
		change, exists := stub.changes[ref.URI]
		if !exists {
			return errors.New("missing replay change set")
		}
		*target = change
		return nil
	case *reviewconfig.ConfigBundle:
		config, exists := stub.configs[ref.URI]
		if !exists {
			return errors.New("missing config bundle")
		}
		*target = config
		return nil
	default:
		return fmt.Errorf("unexpected output %T", out)
	}
}

func (stub *evaluationRunReaderStub) CommittedRunRef(id string) (runmodel.ArtifactRef, error) {
	ref, exists := stub.runRefs[id]
	if !exists {
		return runmodel.ArtifactRef{}, errors.New("missing run ref")
	}
	return ref, nil
}

func TestRecordEvaluationRunBindsLabelSnapshotReportAndPersists(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	caseValue := testActiveCase("eval-positive", "repo-eval", SplitTest, testEpoch)
	targetRef := evaluationArtifactRef("target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)

	createdAt := caseValue.CreatedAt.Add(2 * time.Minute)
	exposure := testExposure("evaluation-run-1", caseValue.CaseID,
		createdAt.Add(-time.Minute), ExposureNotSeen)
	if _, err := repository.RecordExposure(context.Background(), exposure,
		testMutation("eval-exposure", createdAt.Add(-30*time.Second), RoleDatasetCurator)); err != nil {
		t.Fatalf("RecordExposure() error = %v", err)
	}
	reader := newEvaluationRunReaderStub(t, "review-run-1", targetRef,
		contractsv1alpha1.AgentReviewComplete)
	attachUsageReceipts(
		t, reader, "review-run-1", contractsv1alpha1.AgentTokenUsageProviderReported, 100, 50,
	)
	request := EvaluationRunRequest{
		SchemaVersion: EvaluationRunRequestSchemaVersion, EvaluationRunID: "evaluation-run-1",
		EvaluatorRevision: "presence-evaluator-1",
		Bindings: []EvaluationCaseRunBinding{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1, ReviewRunID: "review-run-1",
		}},
		CreatedAt: createdAt,
	}
	mutation := testMutation("record-evaluation-run", createdAt.Add(time.Minute), RoleDatasetCurator)
	reader.eligibilityError = runrepo.ErrArtifactQuarantined
	if _, err := repository.RecordEvaluationRun(
		context.Background(), request, mutation, reader,
	); !errors.Is(err, runrepo.ErrArtifactQuarantined) {
		t.Fatalf("RecordEvaluationRun(quarantined input) error = %v", err)
	}
	reader.eligibilityError = nil
	run, err := repository.RecordEvaluationRun(context.Background(), request, mutation, reader)
	if err != nil {
		t.Fatalf("RecordEvaluationRun() error = %v", err)
	}
	if run.Summary.Cases != 1 || run.Summary.Failed != 1 ||
		run.Summary.PositiveCases != 1 || run.Results[0].ReasonCode != "expected_defect_missed" {
		t.Fatalf("evaluation run = %+v", run)
	}
	if run.Results[0].InputSnapshotRef != targetRef ||
		run.Results[0].GovernedReportRef != *reader.runs["review-run-1"].GovernedReportRef ||
		!run.Results[0].Localization.Available || run.Results[0].Localization.ReasonCode != "" ||
		run.Results[0].ExpectedAnchorCount != 1 || run.Results[0].MatchedAnchorCount != 0 ||
		run.Results[0].LocalizedFindingCount != 0 {
		t.Fatalf("evaluation result closure = %+v", run.Results[0])
	}
	if !run.Results[0].Usage.Available ||
		run.Results[0].UsageAuthority != EvaluationUsageAuthority ||
		run.Results[0].UsageCompleteness != contractsv1alpha1.AgentTokenUsageProviderReported ||
		run.Results[0].UsageReceiptCount != 1 || run.Results[0].InputTokens != 100 ||
		run.Results[0].OutputTokens != 50 || run.Results[0].TotalTokens != 150 {
		t.Fatalf("evaluation usage facts = %+v", run.Results[0])
	}
	tampered := run
	tampered.Results = append([]EvaluationCaseResult(nil), run.Results...)
	tampered.Results[0].Verdict = EvaluationPass
	tampered.Results[0].ReasonCode = "expected_defect_detected"
	tampered.Summary = summarizeEvaluationResults(tampered.Results)
	if err := tampered.Validate(); err == nil {
		t.Fatal("EvaluationRun.Validate() accepted a verdict not supported by finding facts")
	}

	retry, err := repository.RecordEvaluationRun(context.Background(), request, mutation, reader)
	if err != nil || !reflect.DeepEqual(retry, run) {
		t.Fatalf("RecordEvaluationRun(idempotent) = %+v, %v", retry, err)
	}
	correction := LabelCorrection{
		SchemaVersion: LabelCorrectionSchemaVersion, CaseID: caseValue.CaseID,
		ExpectedLabelRevision: 1, Label: testDefectLabel("high"),
		LabelPolicyRevision: "label-policy-2", Reason: "post-run correction",
		AffectedExperimentIDs: []string{request.EvaluationRunID},
	}
	if _, err := repository.CorrectLabel(context.Background(), correction,
		testMutation("post-run-correction", mutation.At.Add(time.Minute), RoleDatasetCurator)); err != nil {
		t.Fatalf("CorrectLabel() error = %v", err)
	}
	retryAfterCorrection, err := repository.RecordEvaluationRun(
		context.Background(), request, mutation, reader)
	if err != nil || !reflect.DeepEqual(retryAfterCorrection, run) {
		t.Fatalf("RecordEvaluationRun(idempotent after correction) = %+v, %v",
			retryAfterCorrection, err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.GetEvaluationRun(run.EvaluationRunID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || !reflect.DeepEqual(loaded, run) {
		t.Fatalf("GetEvaluationRun(restart) = %+v, %v", loaded, err)
	}
	listed, err := restarted.ListEvaluationRuns(
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || len(listed) != 1 || listed[0].EvaluationRunID != run.EvaluationRunID {
		t.Fatalf("ListEvaluationRuns() = %+v, %v", listed, err)
	}
}

func TestEvaluateCommittedUsagePreservesPartialFactsAndRejectsChangedBytes(t *testing.T) {
	targetRef := evaluationArtifactRef("usage-target", runmodel.ContractMaterializedTarget)
	reader := newEvaluationRunReaderStub(
		t, "review-run-usage", targetRef, contractsv1alpha1.AgentReviewComplete,
	)
	attachUsageReceipts(
		t, reader, "review-run-usage", contractsv1alpha1.AgentTokenUsagePartial, 100, 50,
	)
	facts, err := evaluateCommittedUsage(reader, reader.runs["review-run-usage"], "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	if facts.availability.Available || facts.availability.ReasonCode != "usage_receipts_incomplete" ||
		facts.completeness != contractsv1alpha1.AgentTokenUsagePartial || facts.partial != 1 ||
		facts.input != 100 || facts.output != 50 || facts.total != 150 {
		t.Fatalf("partial usage facts = %+v", facts)
	}
	ref := *reader.runs["review-run-usage"].AgentExecutionReceiptRef
	reader.artifacts[ref.URI] = append(reader.artifacts[ref.URI], '\n')
	if _, err := evaluateCommittedUsage(
		reader, reader.runs["review-run-usage"], "execution-1",
	); err == nil || !strings.Contains(err.Error(), "bytes changed") {
		t.Fatalf("changed receipt bytes error = %v", err)
	}
}

func TestRecordEvaluationRunScoresExactApplyTrialForFixValidation(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	caseValue := testActiveCase("apply-case", "repo-apply", SplitTest, testEpoch)
	caseValue.Provenance.Kind = SourceReviewedBugFixPair
	caseValue.Provenance.FindingID = ""
	caseValue.Type = CaseFixValidation
	caseValue.Label = Label{
		ExpectedOutcome: OutcomeFixValid,
		Anchors:         []LabelAnchor{}, AnchorRefs: []string{},
		SuppressionTargets: []SuppressionTarget{},
	}
	targetRef := evaluationArtifactRef("apply-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)
	createdAt := testEpoch.Add(2 * time.Hour)
	reader := newEvaluationRunReaderStub(t, "review-run-apply", targetRef,
		contractsv1alpha1.AgentReviewComplete)
	finding := setApplySuggestionReport(t, reader, "review-run-apply", "replace with guarded access")
	trial := ApplyTrial{
		SchemaVersion: ApplyTrialSchemaVersion, TrialID: "trial-apply-1",
		CaseID: caseValue.CaseID, LabelRevision: 1, ReviewRunID: "review-run-apply",
		FindingID: finding.FindingID, FindingFingerprint: finding.Fingerprint,
		InputSnapshotRef:  targetRef,
		GovernedReportRef: *reader.runs["review-run-apply"].GovernedReportRef,
		SuggestionSHA256:  evaluationDigest(*finding.Suggestion),
		EditScriptRef:     addApplyArtifact(reader, "edit-script", ApplyEditScriptContract),
		Authority:         ApplyTrialAuthority, Completeness: ApplyTrialConclusive,
		ReasonCodes: []string{}, ExecutedAt: testEpoch.Add(90 * time.Minute),
	}
	for _, kind := range []ApplyCheckKind{ApplyCheckDryRun, ApplyCheckCompile, ApplyCheckTest} {
		trial.Checks = append(trial.Checks, ApplyCheck{
			Kind: kind, Status: ApplyCheckPassed,
			CommandSHA256: evaluationDigest("command-" + string(kind)),
			EvidenceRef:   addApplyArtifact(reader, "evidence-"+string(kind), ApplyCheckContract),
			DurationMS:    10,
		})
	}
	request := EvaluationRunRequest{
		SchemaVersion: EvaluationRunRequestSchemaVersion, EvaluationRunID: "evaluation-run-apply",
		EvaluatorRevision: "apply-evaluator-1",
		Bindings: []EvaluationCaseRunBinding{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1,
			ReviewRunID: "review-run-apply", ApplyTrial: &trial,
		}}, CreatedAt: createdAt,
	}
	if _, err := repository.RecordExposure(context.Background(),
		testExposure(request.EvaluationRunID, caseValue.CaseID,
			createdAt.Add(-time.Minute), ExposureNotSeen),
		testMutation("apply-exposure", createdAt.Add(-30*time.Second), RoleDatasetCurator)); err != nil {
		t.Fatal(err)
	}
	run, err := repository.RecordEvaluationRun(context.Background(), request,
		testMutation("apply-evaluation", createdAt.Add(time.Minute), RoleDatasetCurator), reader)
	if err != nil {
		t.Fatalf("RecordEvaluationRun(apply) error = %v", err)
	}
	result := run.Results[0]
	if result.CaseType != CaseFixValidation || result.Verdict != EvaluationPass ||
		result.ReasonCode != "expected_fix_validated" || result.DefectPresence.Available ||
		result.DefectPresence.ReasonCode != "metric_not_applicable" ||
		!result.ApplyFidelity.Available || result.ApplyAuthority != ApplyTrialAuthority ||
		result.ApplyChecksExecuted != 3 || result.ApplyChecksPassed != 3 ||
		result.ApplyChecksFailed != 0 || result.ApplyTrialID != trial.TrialID ||
		result.ApplyEditScriptRef == nil || *result.ApplyEditScriptRef != trial.EditScriptRef {
		t.Fatalf("apply evaluation result = %+v", result)
	}

	missingTrial := request
	missingTrial.EvaluationRunID = "evaluation-run-apply-missing"
	missingTrial.Bindings = append([]EvaluationCaseRunBinding(nil), request.Bindings...)
	missingTrial.Bindings[0].ApplyTrial = nil
	if _, err := repository.RecordExposure(context.Background(),
		testExposure(missingTrial.EvaluationRunID, caseValue.CaseID,
			createdAt.Add(-time.Minute), ExposureNotSeen),
		testMutation("apply-missing-exposure", createdAt.Add(-20*time.Second), RoleDatasetCurator)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RecordEvaluationRun(context.Background(), missingTrial,
		testMutation("apply-missing-evaluation", createdAt.Add(2*time.Minute), RoleDatasetCurator),
		reader); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordEvaluationRun(missing apply trial) error = %v", err)
	}

	tampered := request
	tampered.EvaluationRunID = "evaluation-run-apply-tampered"
	tampered.Bindings = append([]EvaluationCaseRunBinding(nil), request.Bindings...)
	tamperedTrial := trial
	tamperedTrial.SuggestionSHA256 = evaluationDigest("different suggestion")
	tampered.Bindings[0].ApplyTrial = &tamperedTrial
	if _, err := repository.RecordExposure(context.Background(),
		testExposure(tampered.EvaluationRunID, caseValue.CaseID,
			createdAt.Add(-time.Minute), ExposureNotSeen),
		testMutation("apply-tampered-exposure", createdAt.Add(-10*time.Second), RoleDatasetCurator)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RecordEvaluationRun(context.Background(), tampered,
		testMutation("apply-tampered-evaluation", createdAt.Add(3*time.Minute), RoleDatasetCurator),
		reader); err == nil || !strings.Contains(err.Error(), "suggestion digest") {
		t.Fatalf("RecordEvaluationRun(tampered suggestion) error = %v", err)
	}
}

func TestScoreLocalizationRequiresDigestBoundRangeOverlap(t *testing.T) {
	expected := []LabelAnchor{{
		Path: "internal/review.go", Side: "new", StartLine: 10, EndLine: 12,
		SourceDigest: evaluationDigest("source-a"),
	}, {
		Path: "internal/review.go", Side: "new", StartLine: 20, EndLine: 20,
		SourceDigest: evaluationDigest("source-a"),
	}}
	findings := []contractsv1alpha1.GovernedReviewFinding{{
		Anchor: contractsv1alpha1.HypothesisSourceAnchor{
			Path: "internal/review.go", Side: contractsv1alpha1.HypothesisAnchorNew,
			StartLine: 11, EndLine: 13, SourceDigest: evaluationDigest("source-a"),
		},
	}, {
		Anchor: contractsv1alpha1.HypothesisSourceAnchor{
			Path: "internal/review.go", Side: contractsv1alpha1.HypothesisAnchorNew,
			StartLine: 20, EndLine: 20, SourceDigest: evaluationDigest("wrong-source"),
		},
	}}
	matched, localized, availability := scoreLocalization(
		expected, findings, contractsv1alpha1.AgentReviewComplete,
	)
	if matched != 1 || localized != 1 || !availability.Available || availability.ReasonCode != "" {
		t.Fatalf("scoreLocalization() = %d, %d, %+v", matched, localized, availability)
	}
	matched, localized, availability = scoreLocalization(
		expected, findings, contractsv1alpha1.AgentReviewPartial,
	)
	if matched != 0 || localized != 0 || availability.Available ||
		availability.ReasonCode != "review_coverage_partial" {
		t.Fatalf("scoreLocalization(partial) = %d, %d, %+v", matched, localized, availability)
	}
}

func TestScoreFilterEfficacyRequiresObservedGovernedDisposition(t *testing.T) {
	fingerprints := []string{
		evaluationDigest("suppression-a"),
		evaluationDigest("suppression-b"),
		evaluationDigest("suppression-c"),
	}
	expected := make([]SuppressionTarget, 0, len(fingerprints))
	candidates := make([]contractsv1alpha1.GovernedReviewCandidate, 0, len(fingerprints))
	dispositions := []contractsv1alpha1.GovernedCandidateDisposition{
		contractsv1alpha1.GovernedCandidateRejected,
		contractsv1alpha1.GovernedCandidateConfirmed,
		contractsv1alpha1.GovernedCandidateInconclusive,
	}
	for index, fingerprint := range fingerprints {
		expected = append(expected, SuppressionTarget{ClusterFingerprint: fingerprint})
		candidates = append(candidates, contractsv1alpha1.GovernedReviewCandidate{
			CandidateID: fmt.Sprintf("candidate-%d", index+1),
			Hypothesis:  contractsv1alpha1.ReviewHypothesis{ClusterFingerprint: fingerprint},
			Disposition: dispositions[index],
		})
	}
	observed, suppressed, escaped, inconclusive, availability := scoreFilterEfficacyWithGovernance(
		expected[1:2], candidates, []contractsv1alpha1.FindingSuppressionFact{{
			CandidateID: candidates[1].CandidateID, Action: contractsv1alpha1.FindingSuppressed,
		}}, contractsv1alpha1.AgentReviewComplete,
	)
	if observed != 1 || suppressed != 1 || escaped != 0 || inconclusive != 0 || !availability.Available {
		t.Fatalf("scoreFilterEfficacyWithGovernance() = %d, %d, %d, %d, %+v",
			observed, suppressed, escaped, inconclusive, availability)
	}
	observed, suppressed, escaped, inconclusive, availability = scoreFilterEfficacy(
		expected, candidates, contractsv1alpha1.AgentReviewComplete,
	)
	if observed != 3 || suppressed != 1 || escaped != 1 || inconclusive != 1 ||
		!availability.Available || availability.ReasonCode != "" {
		t.Fatalf("scoreFilterEfficacy() = %d, %d, %d, %d, %+v",
			observed, suppressed, escaped, inconclusive, availability)
	}
	observed, suppressed, escaped, inconclusive, availability = scoreFilterEfficacy(
		expected, candidates[:2], contractsv1alpha1.AgentReviewComplete,
	)
	if observed != 2 || suppressed != 1 || escaped != 1 || inconclusive != 0 ||
		availability.Available || availability.ReasonCode != "suppression_target_not_observed" {
		t.Fatalf("scoreFilterEfficacy(unobserved) = %d, %d, %d, %d, %+v",
			observed, suppressed, escaped, inconclusive, availability)
	}
	observed, suppressed, escaped, inconclusive, availability = scoreFilterEfficacy(
		expected, candidates, contractsv1alpha1.AgentReviewPartial,
	)
	if observed != 0 || suppressed != 0 || escaped != 0 || inconclusive != 0 ||
		availability.Available || availability.ReasonCode != "review_coverage_partial" {
		t.Fatalf("scoreFilterEfficacy(partial) = %d, %d, %d, %d, %+v",
			observed, suppressed, escaped, inconclusive, availability)
	}
}

func TestRecordEvaluationRunRevalidatesLabelBeforeCommit(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	caseValue := testActiveCase("eval-race", "repo-eval", SplitTest, testEpoch)
	targetRef := evaluationArtifactRef("race-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleDatasetCurator)
	createdAt := caseValue.CreatedAt.Add(2 * time.Minute)
	request := EvaluationRunRequest{
		SchemaVersion: EvaluationRunRequestSchemaVersion, EvaluationRunID: "evaluation-run-race",
		EvaluatorRevision: "presence-evaluator-1",
		Bindings: []EvaluationCaseRunBinding{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1, ReviewRunID: "review-run-race",
		}}, CreatedAt: createdAt,
	}
	if _, err := repository.RecordExposure(context.Background(),
		testExposure(request.EvaluationRunID, caseValue.CaseID,
			createdAt.Add(-time.Minute), ExposureNotSeen),
		testMutation("race-exposure", createdAt.Add(-30*time.Second), RoleDatasetCurator)); err != nil {
		t.Fatal(err)
	}
	reader := newEvaluationRunReaderStub(t, "review-run-race", targetRef,
		contractsv1alpha1.AgentReviewComplete)
	var correctionErr error
	reader.onLoad = func() {
		_, correctionErr = repository.CorrectLabel(context.Background(), LabelCorrection{
			SchemaVersion: LabelCorrectionSchemaVersion, CaseID: caseValue.CaseID,
			ExpectedLabelRevision: 1, Label: testDefectLabel("high"),
			LabelPolicyRevision: "label-policy-2", Reason: "concurrent correction",
			AffectedExperimentIDs: []string{request.EvaluationRunID},
		}, testMutation("race-correction", createdAt.Add(10*time.Second), RoleDatasetCurator))
	}
	_, err := repository.RecordEvaluationRun(context.Background(), request,
		testMutation("race-record", createdAt.Add(time.Minute), RoleDatasetCurator), reader)
	if correctionErr != nil {
		t.Fatalf("concurrent CorrectLabel() error = %v", correctionErr)
	}
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordEvaluationRun(label raced) error = %v", err)
	}
	if _, getErr := repository.GetEvaluationRun(request.EvaluationRunID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}}); !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("GetEvaluationRun(uncommitted) error = %v", getErr)
	}
}

func TestRecordEvaluationRunFailsClosedOnAdmissionAndCoverage(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	caseValue := testActiveCase("eval-guard", "repo-eval", SplitHoldout, testEpoch)
	targetRef := evaluationArtifactRef("guard-target", runmodel.ContractMaterializedTarget)
	caseValue.InputSnapshotRef = targetRef.URI
	createCase(t, repository, caseValue, RoleHoldoutMaintainer)
	createdAt := caseValue.CreatedAt.Add(2 * time.Minute)
	reader := newEvaluationRunReaderStub(t, "review-run-guard", targetRef,
		contractsv1alpha1.AgentReviewPartial)
	request := EvaluationRunRequest{
		SchemaVersion: EvaluationRunRequestSchemaVersion, EvaluationRunID: "evaluation-run-guard",
		EvaluatorRevision: "presence-evaluator-1",
		Bindings: []EvaluationCaseRunBinding{{
			CaseID: caseValue.CaseID, ExpectedLabelRevision: 1, ReviewRunID: "review-run-guard",
		}}, CreatedAt: createdAt,
	}
	mutation := testMutation("record-evaluation-guard", createdAt.Add(time.Minute), RoleHoldoutRunner)
	if _, err := repository.RecordEvaluationRun(context.Background(), request, mutation, reader); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordEvaluationRun(missing exposure) error = %v", err)
	}
	seen := testExposure(request.EvaluationRunID, caseValue.CaseID,
		createdAt.Add(-time.Minute), ExposureSeen)
	if _, err := repository.RecordExposure(context.Background(), seen,
		testMutation("guard-seen", createdAt.Add(-30*time.Second), RoleHoldoutRunner)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RecordEvaluationRun(context.Background(), request, mutation, reader); !errors.Is(err, ErrContaminated) {
		t.Fatalf("RecordEvaluationRun(contaminated) error = %v", err)
	}
}

func TestScoreDefectPresenceKeepsIncompleteAndDimensionsSeparate(t *testing.T) {
	tests := []struct {
		name         string
		expected     ExpectedOutcome
		findings     uint32
		completeness string
		verdict      EvaluationVerdict
		reason       string
	}{
		{"positive hit", OutcomeDefectPresent, 1, "complete", EvaluationPass, "expected_defect_detected"},
		{"positive miss", OutcomeMissedDefect, 0, "complete", EvaluationFail, "expected_defect_missed"},
		{"negative clean", OutcomeClean, 0, "complete", EvaluationPass, "expected_clean_preserved"},
		{"negative false positive", OutcomeFalsePositive, 2, "complete", EvaluationFail, "unexpected_finding_detected"},
		{"partial", OutcomeDefectPresent, 1, "partial", EvaluationInconclusive, "review_coverage_partial"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict, reason := scoreDefectPresence(test.expected, test.findings, test.completeness)
			if verdict != test.verdict || reason != test.reason {
				t.Fatalf("scoreDefectPresence() = %q, %q", verdict, reason)
			}
		})
	}
}

func TestScoreEvaluationResultUsesApplyEvidenceForFixCases(t *testing.T) {
	tests := []struct {
		name    string
		result  EvaluationCaseResult
		verdict EvaluationVerdict
		reason  string
	}{
		{
			name: "valid fix passes all checks",
			result: EvaluationCaseResult{CaseType: CaseFixValidation, ExpectedOutcome: OutcomeFixValid,
				ApplyChecksPassed: 3, ApplyFidelity: MetricAvailability{Available: true}},
			verdict: EvaluationPass, reason: "expected_fix_validated",
		},
		{
			name: "invalid fix is rejected by a check",
			result: EvaluationCaseResult{CaseType: CaseFixValidation, ExpectedOutcome: OutcomeFixInvalid,
				ApplyChecksPassed: 1, ApplyChecksFailed: 1,
				ApplyFidelity: MetricAvailability{Available: true}},
			verdict: EvaluationPass, reason: "expected_fix_invalidated",
		},
		{
			name: "valid fix expectation mismatches failed check",
			result: EvaluationCaseResult{CaseType: CaseFixValidation, ExpectedOutcome: OutcomeFixValid,
				ApplyChecksFailed: 1, ApplyFidelity: MetricAvailability{Available: true}},
			verdict: EvaluationFail, reason: "fix_validity_mismatch",
		},
		{
			name: "partial apply evidence is inconclusive",
			result: EvaluationCaseResult{CaseType: CaseFixValidation, ExpectedOutcome: OutcomeFixValid,
				ApplyFidelity: MetricAvailability{ReasonCode: "apply_evidence_partial"}},
			verdict: EvaluationInconclusive, reason: "apply_evidence_partial",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict, reason := scoreEvaluationResult(test.result)
			if verdict != test.verdict || reason != test.reason {
				t.Fatalf("scoreEvaluationResult() = %q, %q", verdict, reason)
			}
		})
	}
}

func newEvaluationRunReaderStub(
	t *testing.T,
	runID string,
	targetRef runmodel.ArtifactRef,
	completeness contractsv1alpha1.AgentReviewCompleteness,
) *evaluationRunReaderStub {
	t.Helper()
	report, err := contractsv1alpha1.SealGovernedReviewReport(
		contractsv1alpha1.GovernedReviewReport{
			ReviewRunID: runID, ExecutionID: "execution-1", HypothesisSetID: "hypothesis-set-1",
			TargetDigest: evaluationDigest("target-digest"), Completeness: completeness,
			Candidates:  []contractsv1alpha1.GovernedReviewCandidate{},
			Findings:    []contractsv1alpha1.GovernedReviewFinding{},
			Decisions:   []contractsv1alpha1.GovernedFindingDecision{},
			GeneratedAt: testEpoch.Add(time.Hour),
		},
	)
	if err != nil {
		t.Fatalf("SealGovernedReviewReport() error = %v", err)
	}
	reportRef := evaluationArtifactRef(runID+"-report", runmodel.ContractGovernedReviewReport)
	ledger, err := contractsv1alpha1.SealCandidateVerificationLedger(
		contractsv1alpha1.CandidateVerificationLedger{
			ReviewRunID: runID, ExecutionID: report.ExecutionID,
			HypothesisSetID: report.HypothesisSetID, TargetDigest: report.TargetDigest,
			Completeness: report.Completeness,
			Summary:      contractsv1alpha1.CandidateVerificationSummary{},
			Facts:        []contractsv1alpha1.CandidateVerificationFact{}, GeneratedAt: report.GeneratedAt,
		},
	)
	if err != nil {
		t.Fatalf("SealCandidateVerificationLedger() error = %v", err)
	}
	ledgerRef := evaluationArtifactRef(runID+"-verification", runmodel.ContractCandidateVerificationLedger)
	calibration, err := formalreview.BuildFindingCalibrationLedger(report, ledger)
	if err != nil {
		t.Fatalf("BuildFindingCalibrationLedger() error = %v", err)
	}
	suppression, err := formalreview.BuildFindingSuppressionLedger(report, calibration)
	if err != nil {
		t.Fatalf("BuildFindingSuppressionLedger() error = %v", err)
	}
	calibrationRef := evaluationArtifactRef(runID+"-calibration", runmodel.ContractFindingCalibrationLedger)
	suppressionRef := evaluationArtifactRef(runID+"-suppression", runmodel.ContractFindingSuppressionLedger)
	runRef := evaluationArtifactRef(runID+"-run", runmodel.ContractReviewRun)
	startedAt := testEpoch.Add(30 * time.Minute)
	finishedAt := startedAt.Add(100 * time.Millisecond)
	run := runmodel.ReviewRun{
		RunID: runID, Status: runmodel.RunStatusSucceeded, TargetSnapshotRef: targetRef,
		GovernedReportRef: &reportRef, VerificationLedgerRef: &ledgerRef,
		CalibrationLedgerRef: &calibrationRef, SuppressionLedgerRef: &suppressionRef,
		StageAttempts: []runmodel.StageAttempt{{
			StageID: "agent_hypothesize", Attempt: 1, Generation: 1,
			BindingID: "binding-1", Status: runmodel.StageStatusSucceeded,
			StartedAt: startedAt, FinishedAt: &finishedAt, DurationMS: 100,
		}},
	}
	return &evaluationRunReaderStub{
		runs: map[string]runmodel.ReviewRun{runID: run},
		snapshots: map[string]runmodel.ExecutionSnapshot{runID: {
			RemoteWrites: "deny", ToolPolicy: runmodel.ToolInvocationPolicy{RemoteWrites: "deny"},
		}},
		reports:      map[string]contractsv1alpha1.GovernedReviewReport{reportRef.URI: report},
		verification: map[string]contractsv1alpha1.CandidateVerificationLedger{ledgerRef.URI: ledger},
		calibrations: map[string]contractsv1alpha1.FindingCalibrationLedger{calibrationRef.URI: calibration},
		suppressions: map[string]contractsv1alpha1.FindingSuppressionLedger{suppressionRef.URI: suppression},
		changes:      map[string]runmodel.ReplayChangeSet{},
		configs:      map[string]reviewconfig.ConfigBundle{},
		artifacts:    map[string][]byte{},
		runRefs:      map[string]runmodel.ArtifactRef{runID: runRef},
	}
}

func evaluationArtifactRef(seed string, contract string) runmodel.ArtifactRef {
	digest := evaluationDigest(seed)
	return runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + digest, SHA256: digest,
		SizeBytes: int64(len(seed)), Contract: contract,
	}
}

func evaluationDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func attachUsageReceipts(
	t *testing.T,
	reader *evaluationRunReaderStub,
	runID string,
	completeness contractsv1alpha1.AgentTokenUsageCompleteness,
	inputTokens uint64,
	outputTokens uint64,
) {
	t.Helper()
	reasoning := uint64(10)
	promptDigest := evaluationDigest("usage-prompt")
	outputDigest := evaluationDigest("usage-output")
	versioned := func(id string) contractsv1alpha1.VersionedRef {
		return contractsv1alpha1.VersionedRef{
			ID: id, Revision: "1", SHA256: evaluationDigest("usage-" + id),
		}
	}
	usage := contractsv1alpha1.AgentTokenUsage{
		Completeness: completeness, InputTokens: inputTokens, OutputTokens: outputTokens,
		ReasoningTokens: &reasoning, TotalTokens: inputTokens + outputTokens,
	}
	if completeness == contractsv1alpha1.AgentTokenUsagePartial {
		reason := "provider_usage_partial"
		usage.UnavailableReasonCode = &reason
	}
	receipt := contractsv1alpha1.AgentExecutionReceipt{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptSchemaVersion,
		ReceiptID:     "receipt-usage-1", PlanID: "plan-usage-1", SourceRunID: runID,
		ExecutionID: "execution-1", ReviewRunID: runID,
		TaskID: "task-usage-1", GroupID: "group-usage-1",
		TaskRole:  contractsv1alpha1.AgentTaskContext,
		Dimension: versioned("call-context"), Runtime: versioned("node-runtime"),
		Profile: versioned("local-shadow"), Agent: versioned("pi-agent"),
		Provider: versioned("deepseek-anthropic-env"), Model: versioned("deepseek-model"),
		APIProtocol:     contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
		ProvenanceClass: contractsv1alpha1.AgentReceiptProvenanceWorkerSelfReport,
		Authority:       contractsv1alpha1.AgentReceiptAuthorityDiagnosticOnly,
		Status:          contractsv1alpha1.AgentTaskSucceeded,
		PromptDigest:    &promptDigest, OutputDigest: &outputDigest,
		StartedAt: testEpoch, FinishedAt: testEpoch.Add(time.Second),
		ModelTurnsStarted: 1, ModelTurnsCompleted: 1, ToolCalls: 1,
		ToolUsage: []contractsv1alpha1.AgentToolUsage{{
			ToolID: "submit_context", InvocationCount: 1,
		}},
		Usage: usage,
	}
	collection := contractsv1alpha1.AgentExecutionReceiptCollection{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        "plan-usage-1", SourceRunID: runID, ExecutionID: "execution-1",
		ReviewRunID: runID, Receipts: []contractsv1alpha1.AgentExecutionReceipt{receipt},
	}
	data, err := json.Marshal(collection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(data); err != nil {
		t.Fatalf("usage receipt fixture is invalid: %v", err)
	}
	digest := evaluationDigest(string(data))
	ref := runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + digest, SHA256: digest,
		SizeBytes: int64(len(data)), Contract: runmodel.ContractAgentExecutionReceipts,
	}
	run := reader.runs[runID]
	run.AgentExecutionReceiptRef = &ref
	reader.runs[runID] = run
	reader.artifacts[ref.URI] = data
}

func addApplyArtifact(
	reader *evaluationRunReaderStub,
	content string,
	contract string,
) runmodel.ArtifactRef {
	ref := evaluationArtifactRef(content, contract)
	reader.artifacts[ref.URI] = []byte(content)
	return ref
}

func setApplySuggestionReport(
	t *testing.T,
	reader *evaluationRunReaderStub,
	runID string,
	suggestion string,
) contractsv1alpha1.GovernedReviewFinding {
	t.Helper()
	run := reader.runs[runID]
	anchor := contractsv1alpha1.HypothesisSourceAnchor{
		Path: "internal/review.go", Side: contractsv1alpha1.HypothesisAnchorNew,
		StartLine: 10, EndLine: 11, SourceDigest: evaluationDigest("apply-source"),
	}
	evidence := contractsv1alpha1.HypothesisEvidence{
		EvidenceID: "evidence-apply", Statement: "the guarded access is required",
		Anchor: anchor, Excerpt: "return *possiblyNil",
	}
	var err error
	evidence.EvidenceDigest, err = contractsv1alpha1.DigestHypothesisEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	hypothesis := contractsv1alpha1.ReviewHypothesis{
		OccurrenceID: "occurrence-apply", ClusterFingerprint: evaluationDigest("apply-fingerprint"),
		GroupID: "group-apply",
		Dimension: contractsv1alpha1.VersionedRef{
			ID: "correctness", Revision: "builtin-v1", SHA256: evaluationDigest("apply-skill"),
		},
		Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
		Title: "possible nil dereference", Description: "pointer may be nil",
		Impact: "request can panic", Anchor: anchor,
		Evidence: []contractsv1alpha1.HypothesisEvidence{evidence}, Suggestion: &suggestion,
		Verification: []contractsv1alpha1.HypothesisVerificationObservation{{
			ObservationID: "verification-apply", Sequence: 1,
			Verifier: contractsv1alpha1.VersionedRef{
				ID: "independent-verifier", Revision: "v1", SHA256: evaluationDigest("verifier"),
			},
			Verdict:    contractsv1alpha1.HypothesisVerificationConfirmed,
			ReasonCode: "path_reachable", Explanation: "the nil path is reachable",
			EvidenceIDs: []string{evidence.EvidenceID},
		}},
	}
	candidateID := contractsv1alpha1.GovernedCandidateID("hypothesis-set-apply", hypothesis.OccurrenceID)
	findingID := contractsv1alpha1.GovernedFindingID(evaluationDigest("target-digest"), hypothesis)
	finding := contractsv1alpha1.GovernedReviewFinding{
		FindingID: findingID, Fingerprint: hypothesis.ClusterFingerprint,
		CandidateID: candidateID, OccurrenceID: hypothesis.OccurrenceID,
		Dimension: hypothesis.Dimension, Category: hypothesis.Category, Severity: hypothesis.Severity,
		Title: hypothesis.Title, Description: hypothesis.Description, Impact: hypothesis.Impact,
		Anchor: hypothesis.Anchor, Evidence: hypothesis.Evidence, Suggestion: hypothesis.Suggestion,
		Verification: hypothesis.Verification[0], ConfidenceAvailable: false,
		ConfidenceReasonCode: "not_calibrated",
	}
	decision := contractsv1alpha1.GovernedFindingDecision{
		FindingID: findingID, Sequence: 1,
		Action:      contractsv1alpha1.GovernedFindingQueuedForHuman,
		ReasonCode:  "agent_confirmed_requires_human_review",
		EvidenceIDs: []string{evidence.EvidenceID},
	}
	decision.DecisionID = contractsv1alpha1.GovernedDecisionID(
		decision.FindingID, decision.Sequence, decision.Action,
		decision.ReasonCode, decision.EvidenceIDs,
	)
	report, err := contractsv1alpha1.SealGovernedReviewReport(
		contractsv1alpha1.GovernedReviewReport{
			ReviewRunID: runID, ExecutionID: "execution-apply",
			HypothesisSetID: "hypothesis-set-apply", TargetDigest: evaluationDigest("target-digest"),
			Completeness: contractsv1alpha1.AgentReviewComplete,
			Summary: contractsv1alpha1.GovernedReviewSummary{
				Candidates: 1, Confirmed: 1, Findings: 1, QueuedHuman: 1,
			},
			Candidates: []contractsv1alpha1.GovernedReviewCandidate{{
				CandidateID: candidateID, Hypothesis: hypothesis,
				Disposition: contractsv1alpha1.GovernedCandidateConfirmed,
				ReasonCode:  "path_reachable",
			}},
			Findings:    []contractsv1alpha1.GovernedReviewFinding{finding},
			Decisions:   []contractsv1alpha1.GovernedFindingDecision{decision},
			GeneratedAt: testEpoch.Add(time.Hour),
		},
	)
	if err != nil {
		t.Fatalf("SealGovernedReviewReport(apply) error = %v", err)
	}
	observation := hypothesis.Verification[0]
	ledger, err := contractsv1alpha1.SealCandidateVerificationLedger(
		contractsv1alpha1.CandidateVerificationLedger{
			ReviewRunID: runID, ExecutionID: report.ExecutionID,
			HypothesisSetID: report.HypothesisSetID, TargetDigest: report.TargetDigest,
			Completeness: report.Completeness,
			Summary: contractsv1alpha1.CandidateVerificationSummary{
				Candidates: 1, Confirmed: 1,
			},
			Facts: []contractsv1alpha1.CandidateVerificationFact{{
				VerificationFactID: contractsv1alpha1.CandidateVerificationFactID(candidateID, observation),
				CandidateID:        candidateID, OccurrenceID: hypothesis.OccurrenceID,
				Observation: observation,
			}},
			GeneratedAt: report.GeneratedAt,
		},
	)
	if err != nil {
		t.Fatalf("SealCandidateVerificationLedger(apply) error = %v", err)
	}
	ledgerRef := evaluationArtifactRef(runID+"-verification-apply", runmodel.ContractCandidateVerificationLedger)
	calibration, err := formalreview.BuildFindingCalibrationLedger(report, ledger)
	if err != nil {
		t.Fatalf("BuildFindingCalibrationLedger(apply) error = %v", err)
	}
	suppression, err := formalreview.BuildFindingSuppressionLedger(report, calibration)
	if err != nil {
		t.Fatalf("BuildFindingSuppressionLedger(apply) error = %v", err)
	}
	calibrationRef := evaluationArtifactRef(runID+"-calibration-apply", runmodel.ContractFindingCalibrationLedger)
	suppressionRef := evaluationArtifactRef(runID+"-suppression-apply", runmodel.ContractFindingSuppressionLedger)
	run.VerificationLedgerRef = &ledgerRef
	run.CalibrationLedgerRef = &calibrationRef
	run.SuppressionLedgerRef = &suppressionRef
	reader.runs[runID] = run
	reader.verification[ledgerRef.URI] = ledger
	reader.calibrations[calibrationRef.URI] = calibration
	reader.suppressions[suppressionRef.URI] = suppression
	reader.reports[run.GovernedReportRef.URI] = report
	return report.Findings[0]
}
