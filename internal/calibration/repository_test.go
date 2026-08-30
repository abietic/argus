package calibration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var calibrationEpoch = time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)

type caseSourceStub struct {
	records map[string]evaluation.CaseRecord
}

func (source caseSourceStub) GetCase(id string, _ evaluation.Access) (evaluation.CaseRecord, error) {
	value, ok := source.records[id]
	if !ok {
		return evaluation.CaseRecord{}, evaluation.ErrNotFound
	}
	return value, nil
}
func (source caseSourceStub) AnnotationHistory(id string, _ evaluation.Access) ([]evaluation.CaseAnnotationEntry, error) {
	return []evaluation.CaseAnnotationEntry{{EventID: id + "-a", Reviewer: "reviewer-a"}, {EventID: id + "-b", Reviewer: "reviewer-b"}}, nil
}
func (source caseSourceStub) AdjudicationHistory(id string, _ evaluation.Access) ([]evaluation.CaseAdjudicationEntry, error) {
	label := source.records[id].CurrentLabel
	return []evaluation.CaseAdjudicationEntry{{EventID: id + "-judged", Adjudicator: "adjudicator", Adjudication: evaluation.CaseAdjudication{Outcome: evaluation.AdjudicationApprove, AnnotationEventIDs: []string{id + "-a", id + "-b"}, EvidenceRefs: []string{"artifact://local/evidence"}, SelectedLabel: &label}}}, nil
}

type runSourceStub struct {
	runs       map[string]runmodel.ReviewRun
	candidates map[string]contractsv1alpha1.GovernedReviewCandidate
	spec       []byte
}

func (source runSourceStub) LoadRun(id string) (runmodel.ReviewRun, error) {
	return source.runs[id], nil
}
func (source runSourceStub) LoadExecutionSnapshot(id string) (runmodel.ExecutionSnapshot, error) {
	return runmodel.ExecutionSnapshot{ExecutionSnapshotID: id, ReviewSpecRef: runmodel.ArtifactRef{URI: "artifact://local/spec", SHA256: testDigest, SizeBytes: int64(len(source.spec)), Contract: runmodel.ContractReviewSpec}}, nil
}
func (source runSourceStub) LoadCommittedRunResult(id string) (runrepo.CommittedRunResult, error) {
	candidate := source.candidates[id]
	return runrepo.CommittedRunResult{CandidateSet: &contractsv1alpha1.GovernedCandidateSet{Candidates: []contractsv1alpha1.GovernedReviewCandidate{candidate}}}, nil
}
func (source runSourceStub) ReadArtifact(runmodel.ArtifactRef) ([]byte, error) {
	return slices.Clone(source.spec), nil
}
func (source runSourceStub) CheckArtifactEligibility(runmodel.ArtifactRef, runrepo.ArtifactUse) error {
	return nil
}

func TestRepositoryFitPersistsAuditableCandidateWithoutPublication(t *testing.T) {
	repository, request, mutation := testRepository(t)
	run, err := repository.Fit(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("Fit() error = %v", err)
	}
	if !run.Report.Passed || run.ProfileCandidate.Status != "gate_passed" || run.ProfileCandidate.AutoPublished {
		t.Fatalf("run = %+v", run)
	}
	loaded, err := repository.Get(run.RunID, evaluation.Access{Actor: "operator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}})
	if err != nil || loaded.SHA256 != run.SHA256 {
		t.Fatalf("Get() = %+v, %v", loaded, err)
	}
	retry, err := repository.Fit(context.Background(), request, mutation)
	if err != nil || retry.SHA256 != run.SHA256 {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	request.ProfileRevision = "2"
	if _, err := repository.Fit(context.Background(), request, mutation); !errorsIs(err, ErrConflict) {
		t.Fatalf("conflicting retry error = %v", err)
	}
}

func TestRepositoryFitRejectsContaminationRawDriftAndSelfAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*FitRequest, *evaluation.Mutation)
	}{
		{"clone crossing", func(request *FitRequest, _ *evaluation.Mutation) {
			request.Validation[0].CaseID = request.Training[0].CaseID
			request.Validation[0].CaseGovernanceRevision = 2
			request.Validation[0].CaseLabelRevision = 1
		}},
		{"raw drift", func(request *FitRequest, _ *evaluation.Mutation) { request.Training[0].RawConfidencePPM++ }},
		{"fitting operator labels", func(request *FitRequest, mutation *evaluation.Mutation) { mutation.Actor = "reviewer-a" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, request, mutation := testRepository(t)
			test.mutate(&request, &mutation)
			if _, err := repository.Fit(context.Background(), request, mutation); err == nil {
				t.Fatal("Fit() unexpectedly succeeded")
			}
		})
	}
}

func TestRepositoryFitRecordsFailedGate(t *testing.T) {
	repository, request, mutation := testRepository(t)
	request.GatePolicy.MaximumValidationBrierPPM = 0
	request.GatePolicy.MaximumValidationECEPPM = 0
	run, err := repository.Fit(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("Fit() error = %v", err)
	}
	if run.Report.Passed || run.ProfileCandidate.Status != "gate_failed" || len(run.Report.ReasonCodes) == 0 {
		t.Fatalf("failed gate run = %+v", run)
	}
}

func testRepository(t *testing.T) (*Repository, FitRequest, evaluation.Mutation) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := contractsv1alpha1.ReviewSpec{SchemaVersion: contractsv1alpha1.ReviewSpecSchemaVersion, RequestID: "request", IdempotencyKey: "review-key", TenantID: "tenant", WorkspaceID: "workspace", Repository: contractsv1alpha1.RepositoryRef{Provider: "local-git", RepositoryID: "repo"}, Target: contractsv1alpha1.ReviewTarget{Mode: contractsv1alpha1.ReviewModeScope, Scope: &contractsv1alpha1.ScopeTarget{Revision: "abc", Include: []string{"**"}, Exclude: []string{}}}, ConfigBundleRef: contractsv1alpha1.VersionedRef{ID: "config", Revision: "1", SHA256: testDigest}, WorkflowRef: contractsv1alpha1.VersionedRef{ID: "workflow", Revision: "1", SHA256: testDigest}, RequestedOutput: []string{"findings"}, RemoteWrites: "deny"}
	specBytes, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeReviewSpec(specBytes); err != nil {
		t.Fatalf("test spec invalid: %v", err)
	}
	records, runs, candidates := map[string]evaluation.CaseRecord{}, map[string]runmodel.ReviewRun{}, map[string]contractsv1alpha1.GovernedReviewCandidate{}
	observations := make([]Observation, 0, 4)
	for index, raw := range []uint32{100_000, 900_000, 200_000, 800_000} {
		id, runID, truth := fmt.Sprintf("case-%d", index), fmt.Sprintf("review-%d", index), TruthFalsePositive
		if index%2 == 1 {
			truth = TruthValidDefect
		}
		label := evaluation.Label{ExpectedOutcome: evaluation.OutcomeClean, Anchors: []evaluation.LabelAnchor{}, AnchorRefs: []string{}, SuppressionTargets: []evaluation.SuppressionTarget{}}
		caseType := evaluation.CaseNegativeClean
		anchor := contractsv1alpha1.HypothesisSourceAnchor{Path: "pkg/x.go", Side: contractsv1alpha1.HypothesisAnchorNew, StartLine: 10, EndLine: 10, SourceDigest: testDigest}
		if truth == TruthValidDefect {
			caseType = evaluation.CasePositiveLocalized
			label = evaluation.Label{ExpectedOutcome: evaluation.OutcomeDefectPresent, Category: "correctness", Severity: "high", Anchors: []evaluation.LabelAnchor{{Path: "pkg/x.go", Side: "new", StartLine: 10, EndLine: 10, SourceDigest: testDigest}}, AnchorRefs: []string{"artifact://local/anchor"}, SuppressionTargets: []evaluation.SuppressionTarget{}}
		}
		split := evaluation.SplitTrain
		if index >= 2 {
			split = evaluation.SplitDev
		}
		current := evaluation.EvaluationCase{CaseID: id, Type: caseType, Provenance: evaluation.SourceProvenance{RepositoryID: "repo", SourceRunID: runID}, InputSnapshotRef: "artifact://local/target-" + runID, Label: label, ReviewState: evaluation.ReviewApproved, DatasetState: evaluation.DatasetActive, Split: split, CloneGroupID: "clone-" + id, Eligibility: evaluation.Eligibility{Evaluation: true, Training: split == evaluation.SplitTrain}}
		records[id] = evaluation.CaseRecord{Case: current, CurrentGovernance: evaluation.CaseGovernance{Revision: 2, ReviewState: current.ReviewState, DatasetState: current.DatasetState, Split: current.Split, Eligibility: current.Eligibility}, CurrentLabel: label, CurrentLabelRevision: 1}
		candidateID := "candidate-" + fmt.Sprint(index)
		candidates[runID] = contractsv1alpha1.GovernedReviewCandidate{CandidateID: candidateID, Hypothesis: contractsv1alpha1.ReviewHypothesis{OccurrenceID: "occurrence", ClusterFingerprint: testDigest, Dimension: contractsv1alpha1.VersionedRef{ID: "correctness", Revision: "1", SHA256: testDigest}, Category: "correctness", RawConfidenceAvailable: true, RawConfidencePPM: raw, Anchor: anchor}}
		runs[runID] = runmodel.ReviewRun{RunID: runID, Status: runmodel.RunStatusSucceeded, ExecutionSnapshotID: "snapshot-" + runID, TargetSnapshotRef: runmodel.ArtifactRef{URI: current.InputSnapshotRef}, CandidateSetRef: &runmodel.ArtifactRef{URI: "artifact://local/candidates-" + runID, SHA256: testDigest, Contract: runmodel.ContractGovernedCandidateSet}}
		observations = append(observations, Observation{ObservationID: "observation-" + fmt.Sprint(index), CaseID: id, CaseGovernanceRevision: 2, CaseLabelRevision: 1, ReviewRunID: runID, CandidateID: candidateID, ClusterFingerprint: testDigest, RepositoryID: "repo", DimensionID: "correctness", DimensionRevision: "1", DimensionSHA256: testDigest, RawConfidencePPM: raw, Truth: truth, Authority: LabelAuthority{Kind: AuthorityHumanAdjudication, ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, AdjudicatorID: "adjudicator", EvidenceRefs: []string{"artifact://local/evidence"}, Statement: IndependentAuthorityStatement}})
	}
	repository, err := New(store, caseSourceStub{records}, runSourceStub{runs, candidates, specBytes})
	if err != nil {
		t.Fatal(err)
	}
	policy := GatePolicy{MinimumTrainingSamples: 2, MinimumValidationSamples: 2, MinimumSliceSamples: 1, MaximumValidationBrierPPM: 1_000_000, MaximumValidationECEPPM: 1_000_000, MaximumSliceECEPPM: 1_000_000, MaximumLabelRateDriftPPM: 1_000_000, MaximumMeanRawDriftPPM: 1_000_000}
	request := FitRequest{SchemaVersion: FitRequestSchemaVersion, RunID: "calibration-run", ProfileID: "profile", ProfileRevision: "1", Training: observations[:2], Validation: observations[2:], GatePolicy: policy, CreatedAt: calibrationEpoch}
	mutation := evaluation.Mutation{IdempotencyKey: "fit-key", Actor: "operator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "fit governed calibration", At: calibrationEpoch}
	return repository, request, mutation
}

func errorsIs(err error, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		value, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = value.Unwrap()
	}
	return false
}
