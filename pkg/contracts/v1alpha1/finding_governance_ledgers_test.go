package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFindingGovernanceLedgersSealDecodeAndCloseEmptyReport(t *testing.T) {
	generatedAt := time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC)
	verification, err := SealCandidateVerificationLedger(CandidateVerificationLedger{
		ReviewRunID: "formal-run-ledger", ExecutionID: "execution-ledger",
		HypothesisSetID: "hypothesis-set-ledger", TargetDigest: strings.Repeat("a", 64),
		Completeness: AgentReviewComplete, Summary: CandidateVerificationSummary{},
		Facts: []CandidateVerificationFact{}, GeneratedAt: generatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := SealGovernedReviewReport(GovernedReviewReport{
		ReviewRunID: verification.ReviewRunID, ExecutionID: verification.ExecutionID,
		HypothesisSetID: verification.HypothesisSetID, TargetDigest: verification.TargetDigest,
		Completeness: AgentReviewComplete, Candidates: []GovernedReviewCandidate{},
		Findings: []GovernedReviewFinding{}, Decisions: []GovernedFindingDecision{},
		GeneratedAt: generatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	calibration, err := SealFindingCalibrationLedger(FindingCalibrationLedger{
		ReviewRunID: report.ReviewRunID, ExecutionID: report.ExecutionID,
		ReportID: report.ReportID, VerificationLedgerID: verification.VerificationLedgerID,
		TargetDigest: report.TargetDigest, Facts: []FindingCalibrationFact{}, GeneratedAt: generatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := calibration.ValidateAgainst(report, verification); err != nil {
		t.Fatalf("calibration.ValidateAgainst() error = %v", err)
	}
	suppression, err := SealFindingSuppressionLedger(FindingSuppressionLedger{
		ReviewRunID: report.ReviewRunID, ExecutionID: report.ExecutionID,
		ReportID: report.ReportID, CalibrationLedgerID: calibration.CalibrationLedgerID,
		TargetDigest: report.TargetDigest, PolicyReasonCode: "not_configured",
		Facts: []FindingSuppressionFact{}, GeneratedAt: generatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := suppression.ValidateAgainst(report, calibration); err != nil {
		t.Fatalf("suppression.ValidateAgainst() error = %v", err)
	}
	for name, value := range map[string]struct {
		marshal func() ([]byte, error)
		decode  func([]byte) error
	}{
		"calibration": {
			marshal: func() ([]byte, error) { return json.Marshal(calibration) },
			decode: func(data []byte) error {
				_, err := DecodeFindingCalibrationLedger(data)
				return err
			},
		},
		"suppression": {
			marshal: func() ([]byte, error) { return json.Marshal(suppression) },
			decode: func(data []byte) error {
				_, err := DecodeFindingSuppressionLedger(data)
				return err
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := value.marshal()
			if err != nil {
				t.Fatal(err)
			}
			if err := value.decode(data); err != nil {
				t.Fatalf("decode canonical ledger: %v", err)
			}
			unknown := []byte(strings.Replace(string(data), "{", `{"unknown":true,`, 1))
			if err := value.decode(unknown); err == nil {
				t.Fatal("strict decoder accepted unknown field")
			}
		})
	}
}

func TestFindingCalibrationAndSuppressionRejectInventedFacts(t *testing.T) {
	calibrationProfile := FindingCalibrationProfile{
		SchemaVersion: FindingCalibrationProfileSchemaVersion,
		ID:            "calibration-profile", Revision: "1",
		Points: []FindingCalibrationPoint{
			{RawPPM: 0, ConfidencePPM: 0},
			{RawPPM: ConfidenceScalePPM, ConfidencePPM: ConfidenceScalePPM},
		},
	}
	var err error
	calibrationProfile.SHA256, err = DigestFindingCalibrationProfile(calibrationProfile)
	if err != nil {
		t.Fatal(err)
	}
	profile := calibrationProfile.Ref()
	calibrationFact := FindingCalibrationFact{
		FindingID: "finding-one", CandidateID: "candidate-one",
		VerificationFactID: "verification-fact-one",
		Status:             FindingCalibrationCalibrated, ReasonCode: "isotonic_calibrated",
		Profile: &profile, RawScoreAvailable: true, RawScorePPM: 700_000,
		ConfidenceAvailable: true, ConfidencePPM: 700_000,
	}
	calibrationFact.CalibrationFactID = FindingCalibrationFactID(
		calibrationFact.FindingID, calibrationFact.VerificationFactID,
		calibrationFact.Status, calibrationFact.ReasonCode, calibrationFact.Profile,
		calibrationFact.RawScoreAvailable, calibrationFact.RawScorePPM,
		calibrationFact.ConfidenceAvailable, calibrationFact.ConfidencePPM,
	)
	ledger, err := SealFindingCalibrationLedger(FindingCalibrationLedger{
		ReviewRunID: "formal-run-one", ExecutionID: "execution-one",
		ReportID: "review-report-one", VerificationLedgerID: "verification-ledger-one",
		TargetDigest: strings.Repeat("a", 64), Profile: &calibrationProfile,
		Summary: FindingCalibrationSummary{Findings: 1, Calibrated: 1},
		Facts:   []FindingCalibrationFact{calibrationFact}, GeneratedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tampered := ledger
	tampered.Facts = append([]FindingCalibrationFact(nil), ledger.Facts...)
	tampered.Facts[0].ConfidencePPM++
	if _, err := SealFindingCalibrationLedger(tampered); err == nil {
		t.Fatal("calibration ledger accepted stale fact identity")
	}
	forged := ledger
	forged.Facts = append([]FindingCalibrationFact(nil), ledger.Facts...)
	forged.Facts[0].ConfidencePPM++
	forged.Facts[0].CalibrationFactID = FindingCalibrationFactID(
		forged.Facts[0].FindingID, forged.Facts[0].VerificationFactID,
		forged.Facts[0].Status, forged.Facts[0].ReasonCode, forged.Facts[0].Profile,
		forged.Facts[0].RawScoreAvailable, forged.Facts[0].RawScorePPM,
		forged.Facts[0].ConfidenceAvailable, forged.Facts[0].ConfidencePPM,
	)
	if _, err := SealFindingCalibrationLedger(forged); err == nil ||
		!strings.Contains(err.Error(), "profile recomputation") {
		t.Fatalf("calibration ledger accepted forged arithmetic: %v", err)
	}

	suppressionFact := FindingSuppressionFact{
		FindingID: calibrationFact.FindingID, CandidateID: calibrationFact.CandidateID,
		CalibrationFactID: calibrationFact.CalibrationFactID,
		Sequence:          1, RankAvailable: true, Rank: 1,
		Action: FindingSuppressed, ReasonCode: "policy_threshold",
		EvidenceIDs: []string{"evidence-one"},
	}
	suppressionFact.SuppressionFactID = FindingSuppressionFactID(
		suppressionFact.FindingID, suppressionFact.CalibrationFactID,
		suppressionFact.Sequence, suppressionFact.PriorSuppressionFactID,
		suppressionFact.RankAvailable, suppressionFact.Rank,
		suppressionFact.Action, suppressionFact.ReasonCode, suppressionFact.EvidenceIDs,
	)
	suppression, err := SealFindingSuppressionLedger(FindingSuppressionLedger{
		ReviewRunID: ledger.ReviewRunID, ExecutionID: ledger.ExecutionID,
		ReportID: ledger.ReportID, CalibrationLedgerID: ledger.CalibrationLedgerID,
		TargetDigest: ledger.TargetDigest, Policy: &profile,
		PolicyApplied: true, PolicyReasonCode: "policy_applied",
		Summary: FindingSuppressionSummary{Findings: 1, Suppressed: 1},
		Facts:   []FindingSuppressionFact{suppressionFact}, GeneratedAt: ledger.GeneratedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	tamperedSuppression := suppression
	tamperedSuppression.Facts = append([]FindingSuppressionFact(nil), suppression.Facts...)
	tamperedSuppression.Facts[0].ReasonCode = "substituted_reason"
	if _, err := SealFindingSuppressionLedger(tamperedSuppression); err == nil {
		t.Fatal("suppression ledger accepted stale fact identity")
	}
}
