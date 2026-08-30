package formalreview

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"argus.local/argus/internal/reviewconfig"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestBuildGovernedReportPromotesOnlyConfirmedCandidateAndQueuesHuman(t *testing.T) {
	set := governedHypothesisSet(t, contractsv1alpha1.HypothesisVerificationConfirmed)
	ledger, err := BuildCandidateVerificationLedger(set)
	if err != nil {
		t.Fatalf("BuildCandidateVerificationLedger() error = %v", err)
	}
	if ledger.Summary.Confirmed != 1 || len(ledger.Facts) != 1 {
		t.Fatalf("verification ledger = %+v", ledger)
	}
	report, err := BuildGovernedReportFromVerification(set, ledger)
	if err != nil {
		t.Fatalf("BuildGovernedReport() error = %v", err)
	}
	if len(report.Candidates) != 1 || len(report.Findings) != 1 || len(report.Decisions) != 1 ||
		report.Candidates[0].Disposition != contractsv1alpha1.GovernedCandidateConfirmed ||
		report.Decisions[0].Action != contractsv1alpha1.GovernedFindingQueuedForHuman ||
		report.Findings[0].ConfidenceAvailable ||
		report.Findings[0].ConfidenceReasonCode != "not_calibrated" {
		t.Fatalf("governed report = %+v", report)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("GovernedReviewReport.Validate() error = %v", err)
	}
	if err := report.ValidateAgainstVerificationLedger(ledger); err != nil {
		t.Fatalf("ValidateAgainstVerificationLedger() error = %v", err)
	}
	calibration, err := BuildFindingCalibrationLedger(report, ledger)
	if err != nil {
		t.Fatalf("BuildFindingCalibrationLedger() error = %v", err)
	}
	if len(calibration.Facts) != 1 ||
		calibration.Facts[0].Status != contractsv1alpha1.FindingCalibrationUnavailable ||
		calibration.Facts[0].ReasonCode != "not_calibrated" {
		t.Fatalf("calibration ledger = %+v", calibration)
	}
	suppression, err := BuildFindingSuppressionLedger(report, calibration)
	if err != nil {
		t.Fatalf("BuildFindingSuppressionLedger() error = %v", err)
	}
	if len(suppression.Facts) != 1 ||
		suppression.Facts[0].Action != contractsv1alpha1.FindingNotSuppressed ||
		suppression.Facts[0].ReasonCode != "queued_for_human" {
		t.Fatalf("suppression ledger = %+v", suppression)
	}
	if err := suppression.ValidateAgainst(report, calibration); err != nil {
		t.Fatalf("suppression.ValidateAgainst() error = %v", err)
	}
	profile, err := reviewconfig.SealCalibrationProfile("missing-raw-profile", "1", []reviewconfig.CalibrationPoint{
		{RawPPM: 0, ConfidencePPM: 0},
		{RawPPM: 1_000_000, ConfidencePPM: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := reviewconfig.SealFindingGovernancePolicy(profile, 500_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	calibration, err = BuildFindingCalibrationLedgerWithPolicy(report, ledger, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if calibration.Facts[0].Status != contractsv1alpha1.FindingCalibrationUnavailable ||
		calibration.Facts[0].ReasonCode != "raw_score_unavailable" || calibration.Profile == nil {
		t.Fatalf("configured missing-raw calibration = %+v", calibration)
	}
	suppression, err = BuildFindingSuppressionLedgerWithPolicy(report, calibration, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if suppression.PolicyApplied || suppression.PolicyReasonCode != "calibration_unavailable" ||
		suppression.Facts[0].Action != contractsv1alpha1.FindingNotSuppressed ||
		suppression.Facts[0].ReasonCode != "calibration_unavailable_requires_human" {
		t.Fatalf("configured missing-raw suppression = %+v", suppression)
	}
	markdown, err := RenderGovernedReportMarkdown(report)
	if err != nil || markdown == "" {
		t.Fatalf("RenderGovernedReportMarkdown() = %q, %v", markdown, err)
	}
}

func TestBuildGovernedReportPreservesRejectedCandidateWithoutFinding(t *testing.T) {
	set := governedHypothesisSet(t, contractsv1alpha1.HypothesisVerificationRejected)
	candidates, err := BuildGovernedCandidateSet(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates.Candidates) != 1 || candidates.Summary.Rejected != 1 ||
		candidates.Candidates[0].Disposition != contractsv1alpha1.GovernedCandidateRejected {
		t.Fatalf("rejected governed candidate set = %+v", candidates)
	}
	if err := candidates.ValidateAgainstHypothesisSet(set); err != nil {
		t.Fatalf("GovernedCandidateSet.ValidateAgainstHypothesisSet() error = %v", err)
	}
	report, err := BuildGovernedReport(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 1 || len(report.Findings) != 0 || len(report.Decisions) != 0 ||
		report.Candidates[0].Disposition != contractsv1alpha1.GovernedCandidateRejected {
		t.Fatalf("rejected governed report = %+v", report)
	}
}

func TestFindingGovernanceCalibratesRanksAndPreservesSuppressedFindings(t *testing.T) {
	set := governedHypothesisSet(t, contractsv1alpha1.HypothesisVerificationConfirmed)
	set.Hypotheses[0].RawConfidenceAvailable = true
	set.Hypotheses[0].RawConfidencePPM = 800_000
	second := set.Hypotheses[0]
	second.OccurrenceID = "occurrence-2"
	second.ClusterFingerprint = governanceTestDigest("cluster-2")
	second.RawConfidencePPM = 900_000
	second.Evidence = append([]contractsv1alpha1.HypothesisEvidence(nil), second.Evidence...)
	second.Evidence[0].EvidenceID = "evidence-2"
	second.Evidence[0].EvidenceDigest = ""
	var err error
	second.Evidence[0].EvidenceDigest, err = contractsv1alpha1.DigestHypothesisEvidence(second.Evidence[0])
	if err != nil {
		t.Fatal(err)
	}
	second.Verification = append([]contractsv1alpha1.HypothesisVerificationObservation(nil), second.Verification...)
	second.Verification[0].ObservationID = "verification-2"
	second.Verification[0].EvidenceIDs = []string{"evidence-2"}
	set.Hypotheses = append(set.Hypotheses, second)
	occurrenceID := second.OccurrenceID
	set.NormalizationDecisions = append(set.NormalizationDecisions, contractsv1alpha1.HypothesisNormalizationDecision{
		RawCandidateID: "raw-2", ClaimDigest: governanceTestDigest("claim-2"),
		Action:     contractsv1alpha1.HypothesisNormalizationRetained,
		ReasonCode: "normalized_candidate_retained", OccurrenceID: &occurrenceID,
	})
	set.DedupClusters = append(set.DedupClusters, contractsv1alpha1.HypothesisDedupCluster{
		ClusterID: "cluster-2", Fingerprint: second.ClusterFingerprint,
		CanonicalOccurrenceID: second.OccurrenceID, OccurrenceIDs: []string{second.OccurrenceID},
	})
	if err := set.Validate(); err != nil {
		t.Fatalf("expanded fixture Validate() error = %v", err)
	}
	verification, err := BuildCandidateVerificationLedger(set)
	if err != nil {
		t.Fatal(err)
	}
	report, err := BuildGovernedReportFromVerification(set, verification)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := reviewconfig.SealCalibrationProfile("review-confidence", "1", []reviewconfig.CalibrationPoint{
		{RawPPM: 0, ConfidencePPM: 0},
		{RawPPM: 800_000, ConfidencePPM: 650_000},
		{RawPPM: 1_000_000, ConfidencePPM: 900_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := reviewconfig.SealFindingGovernancePolicy(profile, 600_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	calibration, err := BuildFindingCalibrationLedgerWithPolicy(report, verification, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if calibration.Summary.Calibrated != 2 || calibration.Summary.Unavailable != 0 {
		t.Fatalf("calibration summary = %+v", calibration.Summary)
	}
	suppression, err := BuildFindingSuppressionLedgerWithPolicy(report, calibration, &policy)
	if err != nil {
		t.Fatal(err)
	}
	if !suppression.PolicyApplied || suppression.Summary.NotSuppressed != 1 ||
		suppression.Summary.Suppressed != 1 || len(suppression.Facts) != len(report.Findings) {
		t.Fatalf("suppression ledger = %+v", suppression)
	}
	for _, fact := range suppression.Facts {
		if fact.Rank == 1 && (fact.Action != contractsv1alpha1.FindingNotSuppressed || fact.ReasonCode != "selected_by_policy") {
			t.Fatalf("rank one was not selected: %+v", fact)
		}
		if fact.Rank == 2 && (fact.Action != contractsv1alpha1.FindingSuppressed || fact.ReasonCode != "finding_limit_exceeded") {
			t.Fatalf("rank two was not preserved as a suppressed fact: %+v", fact)
		}
	}
	if err := suppression.ValidateAgainst(report, calibration); err != nil {
		t.Fatalf("ValidateAgainst() error = %v", err)
	}
	forged := suppression
	forged.Facts = append([]contractsv1alpha1.FindingSuppressionFact(nil), suppression.Facts...)
	for index := range forged.Facts {
		if forged.Facts[index].Rank != 2 {
			continue
		}
		forged.Facts[index].Action = contractsv1alpha1.FindingNotSuppressed
		forged.Facts[index].ReasonCode = "selected_by_policy"
		forged.Facts[index].SuppressionFactID = contractsv1alpha1.FindingSuppressionFactID(
			forged.Facts[index].FindingID, forged.Facts[index].CalibrationFactID,
			forged.Facts[index].Sequence, forged.Facts[index].PriorSuppressionFactID,
			forged.Facts[index].RankAvailable, forged.Facts[index].Rank,
			forged.Facts[index].Action, forged.Facts[index].ReasonCode,
			forged.Facts[index].EvidenceIDs,
		)
	}
	forged.Summary.NotSuppressed, forged.Summary.Suppressed = 2, 0
	forged, err = contractsv1alpha1.SealFindingSuppressionLedger(forged)
	if err != nil {
		t.Fatalf("seal identity-consistent forged suppression: %v", err)
	}
	if err := forged.ValidateAgainst(report, calibration); err == nil {
		t.Fatal("ValidateAgainst accepted an identity-consistent policy violation")
	}
}

func governanceTestDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func governedHypothesisSet(
	t *testing.T,
	verdict contractsv1alpha1.HypothesisVerificationVerdict,
) contractsv1alpha1.ReviewHypothesisSet {
	t.Helper()
	digest := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	anchor := contractsv1alpha1.HypothesisSourceAnchor{
		Path: "review.go", Side: contractsv1alpha1.HypothesisAnchorFile,
		StartLine: 2, EndLine: 2, SourceDigest: digest("source"),
	}
	evidence := contractsv1alpha1.HypothesisEvidence{
		EvidenceID: "evidence-1", Statement: "the selected line contains a risky operation",
		Anchor: anchor, Excerpt: "return *possiblyNilPointer", EvidenceDigest: "",
	}
	var err error
	evidence.EvidenceDigest, err = contractsv1alpha1.DigestHypothesisEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID := "occurrence-1"
	fingerprint := digest("cluster")
	hypothesis := contractsv1alpha1.ReviewHypothesis{
		OccurrenceID: occurrenceID, ClusterFingerprint: fingerprint,
		GroupID: "group-1",
		Dimension: contractsv1alpha1.VersionedRef{
			ID: "correctness", Revision: "builtin-v1", SHA256: digest("skill"),
		},
		Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
		Title: "possible nil dereference", Description: "pointer may be nil",
		Impact: "request can panic", Anchor: anchor,
		Evidence: []contractsv1alpha1.HypothesisEvidence{evidence},
		Verification: []contractsv1alpha1.HypothesisVerificationObservation{{
			ObservationID: "verification-1", Sequence: 1,
			Verifier: contractsv1alpha1.VersionedRef{
				ID: "independent-verifier", Revision: "v0", SHA256: digest("verifier"),
			},
			Verdict: verdict, ReasonCode: "path_reachable",
			Explanation: "the frozen path reaches the dereference",
			EvidenceIDs: []string{evidence.EvidenceID},
		}},
	}
	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:   contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "hypothesis-set-1", PlanID: "plan-1",
		SourceRunID: "formal-run-1", ExecutionID: "execution-1",
		ReviewRunID: "formal-run-1", TargetDigest: digest("target"),
		Completeness:        contractsv1alpha1.AgentReviewComplete,
		CompletenessReasons: []string{},
		NormalizationDecisions: []contractsv1alpha1.HypothesisNormalizationDecision{{
			RawCandidateID: "raw-1", ClaimDigest: digest("claim"),
			Action:     contractsv1alpha1.HypothesisNormalizationRetained,
			ReasonCode: "normalized_candidate_retained", OccurrenceID: &occurrenceID,
		}},
		Hypotheses: []contractsv1alpha1.ReviewHypothesis{hypothesis},
		DedupClusters: []contractsv1alpha1.HypothesisDedupCluster{{
			ClusterID: "cluster-1", Fingerprint: fingerprint,
			CanonicalOccurrenceID: occurrenceID, OccurrenceIDs: []string{occurrenceID},
		}},
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			GroupsTotal: 1, GroupsReviewed: 1, ReviewTasksTotal: 1,
			ReviewTasksSucceeded: 1, FilesIncluded: 1,
			Gaps: []contractsv1alpha1.AgentReviewCoverageGap{},
		},
		GeneratedAt: time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("fixture Validate() error = %v", err)
	}
	return set
}
