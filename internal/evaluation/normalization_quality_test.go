package evaluation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runmodel"
)

func TestEvaluateNormalizationQualityMeasuresFalseMergeWithoutHidingContraction(t *testing.T) {
	oracle := normalizationOracleFixture(t)
	oracleRef := evaluationArtifactRef("normalization-oracle", NormalizationOracleContract)
	binding := normalizationBindingFixture(oracle, oracleRef)
	prediction := NormalizationPrediction{
		CaseID: oracle.CaseID, OracleRef: oracleRef, PolicyRevision: "workflow-v2",
		PredictedEquivalenceClasses: []NormalizationEquivalenceClass{{
			ClassID: "predicted-all", RawCandidateIDs: []string{"raw-a", "raw-b", "raw-c", "raw-d"},
		}},
	}
	run, err := EvaluateNormalizationQuality(
		"quality-run-1", "workflow-v2", []NormalizationOracle{oracle},
		[]NormalizationOracleBinding{binding}, []NormalizationPrediction{prediction},
		time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	result := run.Cases[0]
	if result.TrueDuplicatePairs != 3 || result.FalseDuplicatePairs != 3 ||
		result.MissedDuplicatePairs != 0 || result.TrueDistinctPairs != 0 ||
		result.PairwisePrecision.ValuePPM != 500_000 || result.PairwiseRecall.ValuePPM != 1_000_000 ||
		result.FalseMergeRate.ValuePPM != 500_000 || result.UniqueClaimDelta != -1 ||
		result.ExactPartitionMatch || result.PolicyExposed {
		t.Fatalf("false-merge quality result = %+v", result)
	}
	if run.Summary.IndependentEvidenceCases != 1 || run.Summary.PolicyExposedCases != 0 ||
		run.Summary.PairwisePrecision.ValuePPM != 500_000 {
		t.Fatalf("quality summary = %+v", run.Summary)
	}
}

func TestEvaluateNormalizationQualityMeasuresMissedMergeAndExposure(t *testing.T) {
	oracle := normalizationOracleFixture(t)
	oracle.Adjudication.ObservedPolicyRevisions = []string{"workflow-v2"}
	oracleRef := evaluationArtifactRef("normalization-oracle-exposed", NormalizationOracleContract)
	binding := normalizationBindingFixture(oracle, oracleRef)
	prediction := NormalizationPrediction{
		CaseID: oracle.CaseID, OracleRef: oracleRef, PolicyRevision: "workflow-v2",
		PredictedEquivalenceClasses: []NormalizationEquivalenceClass{
			{ClassID: "predicted-ab", RawCandidateIDs: []string{"raw-a", "raw-b"}},
			{ClassID: "predicted-c", RawCandidateIDs: []string{"raw-c"}},
			{ClassID: "predicted-d", RawCandidateIDs: []string{"raw-d"}},
		},
	}
	run, err := EvaluateNormalizationQuality(
		"quality-run-2", "workflow-v2", []NormalizationOracle{oracle},
		[]NormalizationOracleBinding{binding}, []NormalizationPrediction{prediction},
		time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	result := run.Cases[0]
	if result.TrueDuplicatePairs != 1 || result.MissedDuplicatePairs != 2 ||
		result.FalseDuplicatePairs != 0 || result.PairwisePrecision.ValuePPM != 1_000_000 ||
		result.PairwiseRecall.ValuePPM != 333_333 || !result.PolicyExposed ||
		run.Summary.PolicyExposedCases != 1 || run.Summary.IndependentEvidenceCases != 0 {
		t.Fatalf("missed-merge quality result = %+v summary=%+v", result, run.Summary)
	}
}

func TestNormalizationOracleRejectsIncompletePartitionAndSelfAuthority(t *testing.T) {
	oracle := normalizationOracleFixture(t)
	oracle.EquivalenceClasses[0].RawCandidateIDs = []string{"raw-a", "raw-b"}
	if err := oracle.Validate(); err == nil || !strings.Contains(err.Error(), "exactly partition") {
		t.Fatalf("incomplete partition error = %v", err)
	}
	oracle = normalizationOracleFixture(t)
	oracle.Adjudication.Authority = "argus"
	if err := oracle.Validate(); err == nil || !strings.Contains(err.Error(), "independent") {
		t.Fatalf("self authority error = %v", err)
	}
}

func TestNormalizationQualityRunDecodeRejectsTamperedPairMetric(t *testing.T) {
	oracle := normalizationOracleFixture(t)
	oracleRef := evaluationArtifactRef("normalization-oracle-decode", NormalizationOracleContract)
	binding := normalizationBindingFixture(oracle, oracleRef)
	prediction := NormalizationPrediction{
		CaseID: oracle.CaseID, OracleRef: oracleRef, PolicyRevision: "workflow-v2",
		PredictedEquivalenceClasses: oracle.EquivalenceClasses,
	}
	run, err := EvaluateNormalizationQuality(
		"quality-run-decode", "workflow-v2", []NormalizationOracle{oracle},
		[]NormalizationOracleBinding{binding}, []NormalizationPrediction{prediction},
		time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	run.Cases[0].PairwisePrecision.ValuePPM--
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeNormalizationQualityRun(data); err == nil || !strings.Contains(err.Error(), "ratio metrics") {
		t.Fatalf("tampered metric decode error = %v", err)
	}
}

func normalizationOracleFixture(t *testing.T) NormalizationOracle {
	t.Helper()
	adjudicatedAt := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	oracle := NormalizationOracle{
		SchemaVersion: NormalizationOracleSchemaVersion, OracleID: "oracle-case-dev",
		CorpusSnapshotRef: evaluationArtifactRef("normalization-corpus", CorpusSnapshotContract),
		CaseID:            "case-dev", Split: SplitDev, LabelRevision: 3,
		SourceReviewRunID:         "formal-run-dev",
		SourceReviewRunRef:        evaluationArtifactRef("normalization-source-run", runmodel.ContractReviewRun),
		RawCandidateCollectionRef: evaluationArtifactRef("normalization-raw", runmodel.ContractAgentReviewRawCandidates),
		TargetDigest:              testDigest("normalization-target"),
		EligibleRawCandidateIDs:   []string{"raw-a", "raw-b", "raw-c", "raw-d"},
		EquivalenceClasses: []NormalizationEquivalenceClass{
			{ClassID: "claim-approval-transition", RawCandidateIDs: []string{"raw-a", "raw-b", "raw-c"}},
			{ClassID: "claim-distinct-resource-leak", RawCandidateIDs: []string{"raw-d"}},
		},
		Adjudication: NormalizationOracleAdjudication{
			Authority: "independent-review-board", Revision: "board-v1",
			ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, AdjudicatorID: "adjudicator-c",
			EvidenceRefs:            []runmodel.ArtifactRef{evaluationArtifactRef("normalization-oracle-evidence", "argus.external_normalization_oracle_evidence.v1alpha1")},
			ObservedPolicyRevisions: []string{}, AdjudicatedAt: adjudicatedAt,
		},
		CreatedAt: adjudicatedAt.Add(time.Minute),
	}
	if err := oracle.Validate(); err != nil {
		t.Fatalf("fixture oracle: %v", err)
	}
	return oracle
}

func normalizationBindingFixture(oracle NormalizationOracle, ref runmodel.ArtifactRef) NormalizationOracleBinding {
	return NormalizationOracleBinding{
		OracleID: oracle.OracleID, CaseID: oracle.CaseID, Revision: 1,
		RegisteredEventID: "register-" + oracle.OracleID, OracleRef: ref,
		AttestationID:  "attest-" + oracle.OracleID,
		TrustAuthority: "independent-review-board", TrustKeyID: "normalization-key",
		TrustKeyRevision: "key-v1", TrustKeyRegisteredEventID: "register-normalization-key",
	}
}
