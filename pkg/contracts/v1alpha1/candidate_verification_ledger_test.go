package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestCandidateVerificationLedgerClosesExactHypothesisObservations(t *testing.T) {
	set := validReviewHypothesisSet(t)
	hypothesis := set.Hypotheses[0]
	candidateID := GovernedCandidateID(set.HypothesisSetID, hypothesis.OccurrenceID)
	observation := hypothesis.Verification[0]
	ledger, err := SealCandidateVerificationLedger(CandidateVerificationLedger{
		ReviewRunID: set.ReviewRunID, ExecutionID: set.ExecutionID,
		HypothesisSetID: set.HypothesisSetID, TargetDigest: set.TargetDigest,
		Completeness: set.Completeness,
		Summary: CandidateVerificationSummary{
			Candidates: 1, Confirmed: 1,
		},
		Facts: []CandidateVerificationFact{{
			VerificationFactID: CandidateVerificationFactID(candidateID, observation),
			CandidateID:        candidateID, OccurrenceID: hypothesis.OccurrenceID,
			Observation: observation,
		}},
		GeneratedAt: set.GeneratedAt,
	})
	if err != nil {
		t.Fatalf("SealCandidateVerificationLedger() error = %v", err)
	}
	if err := ledger.ValidateAgainstHypothesisSet(set); err != nil {
		t.Fatalf("ValidateAgainstHypothesisSet() error = %v", err)
	}
	latest, found := ledger.Latest(candidateID)
	if !found || latest.VerificationFactID != ledger.Facts[0].VerificationFactID {
		t.Fatalf("Latest() = %+v, %t", latest, found)
	}

	tampered := ledger
	tampered.Facts = append([]CandidateVerificationFact(nil), ledger.Facts...)
	tampered.Facts[0].Observation.ReasonCode = "substituted_reason"
	if _, err := SealCandidateVerificationLedger(tampered); err == nil {
		t.Fatal("SealCandidateVerificationLedger() accepted a stale fact identity")
	}

	data, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unexpected"] = true
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCandidateVerificationLedger(data); err == nil {
		t.Fatal("DecodeCandidateVerificationLedger() accepted an unknown field")
	}
}

func TestCandidateVerificationLedgerPreservesUnverifiedCandidate(t *testing.T) {
	set := validReviewHypothesisSet(t)
	set.Hypotheses[0].Verification = []HypothesisVerificationObservation{}
	set.Completeness = AgentReviewPartial
	set.CompletenessReasons = []string{"verification_incomplete"}
	set.Coverage.Gaps = append(set.Coverage.Gaps, AgentReviewCoverageGap{
		GapID: "gap-verification-ledger", Phase: AgentReviewCoverageVerification,
		SubjectID: set.Hypotheses[0].OccurrenceID, ReasonCode: "verification_not_started",
	})
	ledger, err := SealCandidateVerificationLedger(CandidateVerificationLedger{
		ReviewRunID: set.ReviewRunID, ExecutionID: set.ExecutionID,
		HypothesisSetID: set.HypothesisSetID, TargetDigest: set.TargetDigest,
		Completeness: set.Completeness,
		Summary:      CandidateVerificationSummary{Candidates: 1, Unverified: 1},
		Facts:        []CandidateVerificationFact{}, GeneratedAt: set.GeneratedAt,
	})
	if err != nil {
		t.Fatalf("SealCandidateVerificationLedger() error = %v", err)
	}
	if err := ledger.ValidateAgainstHypothesisSet(set); err != nil {
		t.Fatalf("ValidateAgainstHypothesisSet() error = %v", err)
	}
}
