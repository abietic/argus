package formalreview

import (
	"fmt"
	"slices"
	"strings"

	"github.com/abietic/argus/internal/reviewconfig"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// BuildGovernedReport performs the first deterministic downstream handoff from
// formally admitted hypotheses. It preserves every canonical hypothesis as a
// candidate, promotes only the latest independently-confirmed candidate to a
// Finding, and grants no publication authority: every initial FindingDecision
// is queued_for_human.
func BuildGovernedReport(
	set contractsv1alpha1.ReviewHypothesisSet,
) (contractsv1alpha1.GovernedReviewReport, error) {
	ledger, err := BuildCandidateVerificationLedger(set)
	if err != nil {
		return contractsv1alpha1.GovernedReviewReport{}, err
	}
	return BuildGovernedReportFromVerification(set, ledger)
}

func BuildGovernedReportFromVerification(
	set contractsv1alpha1.ReviewHypothesisSet,
	ledger contractsv1alpha1.CandidateVerificationLedger,
) (contractsv1alpha1.GovernedReviewReport, error) {
	if err := ledger.ValidateAgainstHypothesisSet(set); err != nil {
		return contractsv1alpha1.GovernedReviewReport{}, fmt.Errorf(
			"validate candidate verification ledger: %w", err,
		)
	}
	candidateSet, err := BuildGovernedCandidateSetFromVerification(set, ledger)
	if err != nil {
		return contractsv1alpha1.GovernedReviewReport{}, err
	}
	report := contractsv1alpha1.GovernedReviewReport{
		ReviewRunID: set.ReviewRunID, ExecutionID: set.ExecutionID,
		HypothesisSetID: set.HypothesisSetID, TargetDigest: set.TargetDigest,
		Completeness: set.Completeness,
		Candidates: append(
			[]contractsv1alpha1.GovernedReviewCandidate{}, candidateSet.Candidates...,
		),
		Findings:    []contractsv1alpha1.GovernedReviewFinding{},
		Decisions:   []contractsv1alpha1.GovernedFindingDecision{},
		GeneratedAt: set.GeneratedAt,
	}
	report.Summary.Candidates = candidateSet.Summary.Candidates
	report.Summary.Confirmed = candidateSet.Summary.Confirmed
	report.Summary.Rejected = candidateSet.Summary.Rejected
	report.Summary.Inconclusive = candidateSet.Summary.Inconclusive
	for _, candidate := range candidateSet.Candidates {
		if candidate.Disposition != contractsv1alpha1.GovernedCandidateConfirmed {
			continue
		}
		hypothesis := candidate.Hypothesis
		verification, found := ledger.Latest(candidate.CandidateID)
		if !found || verification.Observation.Verdict != contractsv1alpha1.HypothesisVerificationConfirmed {
			return contractsv1alpha1.GovernedReviewReport{}, fmt.Errorf(
				"confirmed candidate %q lacks confirmed verification", candidate.CandidateID,
			)
		}
		latest := verification.Observation
		findingID := contractsv1alpha1.GovernedFindingID(set.TargetDigest, hypothesis)
		finding := contractsv1alpha1.GovernedReviewFinding{
			FindingID: findingID, Fingerprint: hypothesis.ClusterFingerprint,
			CandidateID: candidate.CandidateID, OccurrenceID: hypothesis.OccurrenceID,
			Dimension: hypothesis.Dimension, Category: hypothesis.Category,
			Severity: hypothesis.Severity, Title: hypothesis.Title,
			Description: hypothesis.Description, Impact: hypothesis.Impact,
			Anchor:     hypothesis.Anchor,
			Evidence:   append([]contractsv1alpha1.HypothesisEvidence{}, hypothesis.Evidence...),
			Suggestion: cloneGovernedString(hypothesis.Suggestion), Verification: latest,
			ConfidenceAvailable: false, ConfidenceReasonCode: "not_calibrated",
		}
		report.Findings = append(report.Findings, finding)
		evidenceIDs := append([]string{}, latest.EvidenceIDs...)
		slices.Sort(evidenceIDs)
		decision := contractsv1alpha1.GovernedFindingDecision{
			FindingID: findingID, Sequence: 1,
			Action:     contractsv1alpha1.GovernedFindingQueuedForHuman,
			ReasonCode: "agent_confirmed_requires_human_review", EvidenceIDs: evidenceIDs,
		}
		decision.DecisionID = contractsv1alpha1.GovernedDecisionID(
			decision.FindingID, decision.Sequence, decision.Action,
			decision.ReasonCode, decision.EvidenceIDs,
		)
		report.Decisions = append(report.Decisions, decision)
	}
	slices.SortFunc(report.Candidates, func(left, right contractsv1alpha1.GovernedReviewCandidate) int {
		return strings.Compare(left.CandidateID, right.CandidateID)
	})
	slices.SortFunc(report.Findings, func(left, right contractsv1alpha1.GovernedReviewFinding) int {
		return strings.Compare(left.FindingID, right.FindingID)
	})
	slices.SortFunc(report.Decisions, func(left, right contractsv1alpha1.GovernedFindingDecision) int {
		return strings.Compare(left.DecisionID, right.DecisionID)
	})
	report.Summary.Candidates = uint32(len(report.Candidates))
	report.Summary.Findings = uint32(len(report.Findings))
	report.Summary.QueuedHuman = uint32(len(report.Decisions))
	sealed, err := contractsv1alpha1.SealGovernedReviewReport(report)
	if err != nil {
		return contractsv1alpha1.GovernedReviewReport{}, fmt.Errorf(
			"seal governed review report: %w", err,
		)
	}
	return sealed, nil
}

func BuildGovernedCandidateSet(
	set contractsv1alpha1.ReviewHypothesisSet,
) (contractsv1alpha1.GovernedCandidateSet, error) {
	ledger, err := BuildCandidateVerificationLedger(set)
	if err != nil {
		return contractsv1alpha1.GovernedCandidateSet{}, err
	}
	return BuildGovernedCandidateSetFromVerification(set, ledger)
}

func BuildGovernedCandidateSetFromVerification(
	set contractsv1alpha1.ReviewHypothesisSet,
	ledger contractsv1alpha1.CandidateVerificationLedger,
) (contractsv1alpha1.GovernedCandidateSet, error) {
	if err := set.Validate(); err != nil {
		return contractsv1alpha1.GovernedCandidateSet{}, fmt.Errorf(
			"validate admitted ReviewHypothesisSet: %w", err,
		)
	}
	if err := ledger.ValidateAgainstHypothesisSet(set); err != nil {
		return contractsv1alpha1.GovernedCandidateSet{}, fmt.Errorf(
			"validate candidate verification ledger: %w", err,
		)
	}
	candidates := contractsv1alpha1.GovernedCandidateSet{
		ReviewRunID: set.ReviewRunID, ExecutionID: set.ExecutionID,
		HypothesisSetID: set.HypothesisSetID, TargetDigest: set.TargetDigest,
		Completeness: set.Completeness,
		Candidates:   []contractsv1alpha1.GovernedReviewCandidate{},
		GeneratedAt:  set.GeneratedAt,
	}
	for _, hypothesis := range set.Hypotheses {
		candidateID := contractsv1alpha1.GovernedCandidateID(
			set.HypothesisSetID, hypothesis.OccurrenceID,
		)
		verification, found := ledger.Latest(candidateID)
		disposition := contractsv1alpha1.GovernedCandidateInconclusive
		reasonCode := "verification_missing"
		if found {
			latest := verification.Observation
			reasonCode = latest.ReasonCode
			switch latest.Verdict {
			case contractsv1alpha1.HypothesisVerificationConfirmed:
				disposition = contractsv1alpha1.GovernedCandidateConfirmed
			case contractsv1alpha1.HypothesisVerificationRejected:
				disposition = contractsv1alpha1.GovernedCandidateRejected
			case contractsv1alpha1.HypothesisVerificationInconclusive:
				disposition = contractsv1alpha1.GovernedCandidateInconclusive
			default:
				return contractsv1alpha1.GovernedCandidateSet{}, fmt.Errorf(
					"hypothesis %q has unsupported latest verdict %q",
					hypothesis.OccurrenceID, latest.Verdict,
				)
			}
		}
		candidates.Candidates = append(candidates.Candidates,
			contractsv1alpha1.GovernedReviewCandidate{
				CandidateID: candidateID,
				Hypothesis:  hypothesis, Disposition: disposition, ReasonCode: reasonCode,
			})
		switch disposition {
		case contractsv1alpha1.GovernedCandidateConfirmed:
			candidates.Summary.Confirmed++
		case contractsv1alpha1.GovernedCandidateRejected:
			candidates.Summary.Rejected++
		case contractsv1alpha1.GovernedCandidateInconclusive:
			candidates.Summary.Inconclusive++
		}
	}
	slices.SortFunc(candidates.Candidates, func(
		left, right contractsv1alpha1.GovernedReviewCandidate,
	) int {
		return strings.Compare(left.CandidateID, right.CandidateID)
	})
	candidates.Summary.Candidates = uint32(len(candidates.Candidates))
	sealed, err := contractsv1alpha1.SealGovernedCandidateSet(candidates)
	if err != nil {
		return contractsv1alpha1.GovernedCandidateSet{}, fmt.Errorf(
			"seal governed candidate set: %w", err,
		)
	}
	return sealed, nil
}

func BuildCandidateVerificationLedger(
	set contractsv1alpha1.ReviewHypothesisSet,
) (contractsv1alpha1.CandidateVerificationLedger, error) {
	if err := set.Validate(); err != nil {
		return contractsv1alpha1.CandidateVerificationLedger{}, fmt.Errorf(
			"validate admitted ReviewHypothesisSet: %w", err,
		)
	}
	ledger := contractsv1alpha1.CandidateVerificationLedger{
		ReviewRunID: set.ReviewRunID, ExecutionID: set.ExecutionID,
		HypothesisSetID: set.HypothesisSetID, TargetDigest: set.TargetDigest,
		Completeness: set.Completeness,
		Summary: contractsv1alpha1.CandidateVerificationSummary{
			Candidates: uint32(len(set.Hypotheses)),
		},
		Facts:       []contractsv1alpha1.CandidateVerificationFact{},
		GeneratedAt: set.GeneratedAt,
	}
	for _, hypothesis := range set.Hypotheses {
		candidateID := contractsv1alpha1.GovernedCandidateID(
			set.HypothesisSetID, hypothesis.OccurrenceID,
		)
		for _, observation := range hypothesis.Verification {
			ledger.Facts = append(ledger.Facts, contractsv1alpha1.CandidateVerificationFact{
				VerificationFactID: contractsv1alpha1.CandidateVerificationFactID(
					candidateID, observation,
				),
				CandidateID: candidateID, OccurrenceID: hypothesis.OccurrenceID,
				Observation: observation,
			})
		}
		latest, found := latestVerification(hypothesis.Verification)
		if !found {
			ledger.Summary.Unverified++
			continue
		}
		switch latest.Verdict {
		case contractsv1alpha1.HypothesisVerificationConfirmed:
			ledger.Summary.Confirmed++
		case contractsv1alpha1.HypothesisVerificationRejected:
			ledger.Summary.Rejected++
		case contractsv1alpha1.HypothesisVerificationInconclusive:
			ledger.Summary.Inconclusive++
		default:
			return contractsv1alpha1.CandidateVerificationLedger{}, fmt.Errorf(
				"hypothesis %q has unsupported latest verdict %q",
				hypothesis.OccurrenceID, latest.Verdict,
			)
		}
	}
	slices.SortFunc(ledger.Facts, func(
		left, right contractsv1alpha1.CandidateVerificationFact,
	) int {
		if compared := strings.Compare(left.CandidateID, right.CandidateID); compared != 0 {
			return compared
		}
		if left.Observation.Sequence < right.Observation.Sequence {
			return -1
		}
		if left.Observation.Sequence > right.Observation.Sequence {
			return 1
		}
		return 0
	})
	sealed, err := contractsv1alpha1.SealCandidateVerificationLedger(ledger)
	if err != nil {
		return contractsv1alpha1.CandidateVerificationLedger{}, fmt.Errorf(
			"seal candidate verification ledger: %w", err,
		)
	}
	return sealed, nil
}

func BuildFindingCalibrationLedger(
	report contractsv1alpha1.GovernedReviewReport,
	verification contractsv1alpha1.CandidateVerificationLedger,
) (contractsv1alpha1.FindingCalibrationLedger, error) {
	return BuildFindingCalibrationLedgerWithPolicy(report, verification, nil)
}

func BuildFindingCalibrationLedgerWithPolicy(
	report contractsv1alpha1.GovernedReviewReport,
	verification contractsv1alpha1.CandidateVerificationLedger,
	policy *reviewconfig.FindingGovernancePolicy,
) (contractsv1alpha1.FindingCalibrationLedger, error) {
	if err := report.ValidateAgainstVerificationLedger(verification); err != nil {
		return contractsv1alpha1.FindingCalibrationLedger{}, fmt.Errorf(
			"validate governed report verification projection: %w", err,
		)
	}
	if policy != nil {
		if err := policy.Validate(); err != nil {
			return contractsv1alpha1.FindingCalibrationLedger{}, fmt.Errorf("validate finding governance policy: %w", err)
		}
	}
	ledger := contractsv1alpha1.FindingCalibrationLedger{
		ReviewRunID: report.ReviewRunID, ExecutionID: report.ExecutionID,
		ReportID: report.ReportID, VerificationLedgerID: verification.VerificationLedgerID,
		TargetDigest: report.TargetDigest,
		Summary:      contractsv1alpha1.FindingCalibrationSummary{Findings: uint32(len(report.Findings))},
		Facts:        []contractsv1alpha1.FindingCalibrationFact{}, GeneratedAt: report.GeneratedAt,
	}
	if policy != nil {
		points := make([]contractsv1alpha1.FindingCalibrationPoint, len(policy.CalibrationProfile.Points))
		for index, point := range policy.CalibrationProfile.Points {
			points[index] = contractsv1alpha1.FindingCalibrationPoint{
				RawPPM: point.RawPPM, ConfidencePPM: point.ConfidencePPM,
			}
		}
		ledger.Profile = &contractsv1alpha1.FindingCalibrationProfile{
			SchemaVersion: policy.CalibrationProfile.SchemaVersion,
			ID:            policy.CalibrationProfile.ID, Revision: policy.CalibrationProfile.Revision,
			Points: points, SHA256: policy.CalibrationProfile.SHA256,
		}
		if err := ledger.Profile.Validate(); err != nil {
			return contractsv1alpha1.FindingCalibrationLedger{}, fmt.Errorf(
				"project finding calibration profile: %w", err,
			)
		}
	}
	candidates := make(map[string]contractsv1alpha1.GovernedReviewCandidate, len(report.Candidates))
	for _, candidate := range report.Candidates {
		candidates[candidate.CandidateID] = candidate
	}
	for _, finding := range report.Findings {
		verificationFact, found := verification.Latest(finding.CandidateID)
		if !found {
			return contractsv1alpha1.FindingCalibrationLedger{}, fmt.Errorf(
				"finding %q has no verification fact", finding.FindingID,
			)
		}
		fact := contractsv1alpha1.FindingCalibrationFact{
			FindingID: finding.FindingID, CandidateID: finding.CandidateID,
			VerificationFactID: verificationFact.VerificationFactID,
			Status:             contractsv1alpha1.FindingCalibrationUnavailable,
			ReasonCode:         finding.ConfidenceReasonCode,
		}
		candidate, exists := candidates[finding.CandidateID]
		if !exists {
			return contractsv1alpha1.FindingCalibrationLedger{}, fmt.Errorf("finding %q has no candidate", finding.FindingID)
		}
		if policy != nil {
			profileRef := ledger.Profile.Ref()
			fact.Profile = &profileRef
			if candidate.Hypothesis.RawConfidenceAvailable {
				confidence, err := ledger.Profile.Calibrate(candidate.Hypothesis.RawConfidencePPM)
				if err != nil {
					return contractsv1alpha1.FindingCalibrationLedger{}, err
				}
				fact.Status = contractsv1alpha1.FindingCalibrationCalibrated
				fact.ReasonCode = "piecewise_linear_calibrated"
				fact.RawScoreAvailable = true
				fact.RawScorePPM = candidate.Hypothesis.RawConfidencePPM
				fact.ConfidenceAvailable = true
				fact.ConfidencePPM = confidence
				ledger.Summary.Calibrated++
			} else {
				fact.ReasonCode = "raw_score_unavailable"
				ledger.Summary.Unavailable++
			}
		} else {
			ledger.Summary.Unavailable++
		}
		fact.CalibrationFactID = contractsv1alpha1.FindingCalibrationFactID(
			fact.FindingID, fact.VerificationFactID, fact.Status, fact.ReasonCode,
			fact.Profile, fact.RawScoreAvailable, fact.RawScorePPM,
			fact.ConfidenceAvailable, fact.ConfidencePPM,
		)
		ledger.Facts = append(ledger.Facts, fact)
	}
	slices.SortFunc(ledger.Facts, func(
		left, right contractsv1alpha1.FindingCalibrationFact,
	) int {
		return strings.Compare(left.FindingID, right.FindingID)
	})
	sealed, err := contractsv1alpha1.SealFindingCalibrationLedger(ledger)
	if err != nil {
		return contractsv1alpha1.FindingCalibrationLedger{}, fmt.Errorf(
			"seal finding calibration ledger: %w", err,
		)
	}
	return sealed, nil
}

func BuildFindingSuppressionLedger(
	report contractsv1alpha1.GovernedReviewReport,
	calibration contractsv1alpha1.FindingCalibrationLedger,
) (contractsv1alpha1.FindingSuppressionLedger, error) {
	return BuildFindingSuppressionLedgerWithPolicy(report, calibration, nil)
}

func BuildFindingSuppressionLedgerWithPolicy(
	report contractsv1alpha1.GovernedReviewReport,
	calibration contractsv1alpha1.FindingCalibrationLedger,
	policy *reviewconfig.FindingGovernancePolicy,
) (contractsv1alpha1.FindingSuppressionLedger, error) {
	if err := report.Validate(); err != nil {
		return contractsv1alpha1.FindingSuppressionLedger{}, err
	}
	if err := calibration.Validate(); err != nil {
		return contractsv1alpha1.FindingSuppressionLedger{}, err
	}
	if calibration.ReviewRunID != report.ReviewRunID ||
		calibration.ExecutionID != report.ExecutionID ||
		calibration.ReportID != report.ReportID ||
		calibration.TargetDigest != report.TargetDigest ||
		!calibration.GeneratedAt.Equal(report.GeneratedAt) ||
		len(calibration.Facts) != len(report.Findings) {
		return contractsv1alpha1.FindingSuppressionLedger{}, fmt.Errorf(
			"calibration ledger does not bind governed report",
		)
	}
	if policy != nil {
		if err := policy.Validate(); err != nil {
			return contractsv1alpha1.FindingSuppressionLedger{}, fmt.Errorf("validate finding governance policy: %w", err)
		}
		expectedProfile := contractsv1alpha1.VersionedRef{
			ID: policy.CalibrationProfile.ID, Revision: policy.CalibrationProfile.Revision,
			SHA256: policy.CalibrationProfile.SHA256,
		}
		for _, fact := range calibration.Facts {
			if fact.Profile == nil || *fact.Profile != expectedProfile {
				return contractsv1alpha1.FindingSuppressionLedger{}, fmt.Errorf(
					"calibration fact %q does not bind configured profile", fact.CalibrationFactID,
				)
			}
		}
	}
	calibrations := make(map[string]contractsv1alpha1.FindingCalibrationFact, len(calibration.Facts))
	for _, fact := range calibration.Facts {
		calibrations[fact.FindingID] = fact
	}
	decisions := make(map[string]contractsv1alpha1.GovernedFindingDecision, len(report.Decisions))
	for _, decision := range report.Decisions {
		decisions[decision.FindingID] = decision
	}
	ledger := contractsv1alpha1.FindingSuppressionLedger{
		ReviewRunID: report.ReviewRunID, ExecutionID: report.ExecutionID,
		ReportID: report.ReportID, CalibrationLedgerID: calibration.CalibrationLedgerID,
		TargetDigest:     report.TargetDigest,
		PolicyReasonCode: "not_configured",
		Summary:          contractsv1alpha1.FindingSuppressionSummary{Findings: uint32(len(report.Findings))},
		Facts:            []contractsv1alpha1.FindingSuppressionFact{}, GeneratedAt: report.GeneratedAt,
	}
	policyApplied := policy != nil
	if policy != nil {
		ledger.Policy = &contractsv1alpha1.VersionedRef{
			ID: policy.ID, Revision: policy.Revision, SHA256: policy.SHA256,
		}
		ledger.MinimumConfidencePPM = policy.MinimumConfidencePPM
		ledger.MaxFindings = uint32(policy.MaxFindings)
		for _, fact := range calibration.Facts {
			if !fact.ConfidenceAvailable {
				policyApplied = false
				break
			}
		}
		ledger.PolicyApplied = policyApplied
		if policyApplied {
			ledger.PolicyReasonCode = "policy_applied"
		} else {
			ledger.PolicyReasonCode = "calibration_unavailable"
		}
	}
	ranks := map[string]uint32{}
	actions := map[string]contractsv1alpha1.FindingSuppressionAction{}
	reasons := map[string]string{}
	if policyApplied {
		ordered := slices.Clone(calibration.Facts)
		slices.SortFunc(ordered, func(left, right contractsv1alpha1.FindingCalibrationFact) int {
			if left.ConfidencePPM > right.ConfidencePPM {
				return -1
			}
			if left.ConfidencePPM < right.ConfidencePPM {
				return 1
			}
			return strings.Compare(left.FindingID, right.FindingID)
		})
		eligibleRank := uint32(0)
		for index, fact := range ordered {
			ranks[fact.FindingID] = uint32(index + 1)
			actions[fact.FindingID] = contractsv1alpha1.FindingNotSuppressed
			reasons[fact.FindingID] = "selected_by_policy"
			if fact.ConfidencePPM < policy.MinimumConfidencePPM {
				actions[fact.FindingID] = contractsv1alpha1.FindingSuppressed
				reasons[fact.FindingID] = "below_confidence_threshold"
				continue
			}
			eligibleRank++
			if eligibleRank > uint32(policy.MaxFindings) {
				actions[fact.FindingID] = contractsv1alpha1.FindingSuppressed
				reasons[fact.FindingID] = "finding_limit_exceeded"
			}
		}
	}
	for _, finding := range report.Findings {
		calibrationFact, found := calibrations[finding.FindingID]
		decision, decided := decisions[finding.FindingID]
		if !found || !decided {
			return contractsv1alpha1.FindingSuppressionLedger{}, fmt.Errorf(
				"finding %q lacks calibration or initial decision", finding.FindingID,
			)
		}
		evidenceIDs := append([]string{}, decision.EvidenceIDs...)
		slices.Sort(evidenceIDs)
		action := contractsv1alpha1.FindingNotSuppressed
		reasonCode := "queued_for_human"
		rank := ranks[finding.FindingID]
		if policy != nil && !policyApplied {
			reasonCode = "calibration_unavailable_requires_human"
		} else if policyApplied {
			action, reasonCode = actions[finding.FindingID], reasons[finding.FindingID]
		}
		fact := contractsv1alpha1.FindingSuppressionFact{
			FindingID: finding.FindingID, CandidateID: finding.CandidateID,
			CalibrationFactID: calibrationFact.CalibrationFactID,
			Sequence:          1, RankAvailable: policyApplied, Rank: rank,
			Action: action, ReasonCode: reasonCode, EvidenceIDs: evidenceIDs,
		}
		fact.SuppressionFactID = contractsv1alpha1.FindingSuppressionFactID(
			fact.FindingID, fact.CalibrationFactID, fact.Sequence,
			fact.PriorSuppressionFactID, fact.RankAvailable, fact.Rank,
			fact.Action, fact.ReasonCode, fact.EvidenceIDs,
		)
		ledger.Facts = append(ledger.Facts, fact)
		if action == contractsv1alpha1.FindingSuppressed {
			ledger.Summary.Suppressed++
		} else {
			ledger.Summary.NotSuppressed++
		}
	}
	slices.SortFunc(ledger.Facts, func(
		left, right contractsv1alpha1.FindingSuppressionFact,
	) int {
		return strings.Compare(left.FindingID, right.FindingID)
	})
	sealed, err := contractsv1alpha1.SealFindingSuppressionLedger(ledger)
	if err != nil {
		return contractsv1alpha1.FindingSuppressionLedger{}, fmt.Errorf(
			"seal finding suppression ledger: %w", err,
		)
	}
	return sealed, nil
}

func RenderGovernedReportMarkdown(
	report contractsv1alpha1.GovernedReviewReport,
) (string, error) {
	return contractsv1alpha1.RenderGovernedReviewMarkdown(report)
}

func latestVerification(
	observations []contractsv1alpha1.HypothesisVerificationObservation,
) (contractsv1alpha1.HypothesisVerificationObservation, bool) {
	if len(observations) == 0 {
		return contractsv1alpha1.HypothesisVerificationObservation{}, false
	}
	latest := observations[0]
	for _, observation := range observations[1:] {
		if observation.Sequence > latest.Sequence {
			latest = observation
		}
	}
	return latest, true
}

func cloneGovernedString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
