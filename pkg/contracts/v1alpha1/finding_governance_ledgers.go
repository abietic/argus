package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

const (
	FindingCalibrationLedgerSchemaVersion  = "argus.finding_calibration_ledger.v1alpha1"
	FindingSuppressionLedgerSchemaVersion  = "argus.finding_suppression_ledger.v1alpha1"
	FindingCalibrationProfileSchemaVersion = "argus.calibration_profile.v1alpha1"
	ConfidenceScalePPM                     = uint32(1_000_000)
)

type FindingCalibrationPoint struct {
	RawPPM        uint32 `json:"raw_ppm"`
	ConfidencePPM uint32 `json:"confidence_ppm"`
}

// FindingCalibrationProfile is embedded in the ledger so readers can verify
// the exact integer transformation without trusting the producing runtime.
type FindingCalibrationProfile struct {
	SchemaVersion string                    `json:"schema_version"`
	ID            string                    `json:"id"`
	Revision      string                    `json:"revision"`
	Points        []FindingCalibrationPoint `json:"points"`
	SHA256        string                    `json:"sha256"`
}

func (profile FindingCalibrationProfile) Ref() VersionedRef {
	return VersionedRef{ID: profile.ID, Revision: profile.Revision, SHA256: profile.SHA256}
}

type FindingCalibrationStatus string

const (
	FindingCalibrationCalibrated  FindingCalibrationStatus = "calibrated"
	FindingCalibrationUnavailable FindingCalibrationStatus = "unavailable"
)

type FindingCalibrationFact struct {
	CalibrationFactID   string                   `json:"calibration_fact_id"`
	FindingID           string                   `json:"finding_id"`
	CandidateID         string                   `json:"candidate_id"`
	VerificationFactID  string                   `json:"verification_fact_id"`
	Status              FindingCalibrationStatus `json:"status"`
	ReasonCode          string                   `json:"reason_code"`
	Profile             *VersionedRef            `json:"profile,omitempty"`
	RawScoreAvailable   bool                     `json:"raw_score_available"`
	RawScorePPM         uint32                   `json:"raw_score_ppm"`
	ConfidenceAvailable bool                     `json:"confidence_available"`
	ConfidencePPM       uint32                   `json:"confidence_ppm"`
}

type FindingCalibrationSummary struct {
	Findings    uint32 `json:"findings"`
	Calibrated  uint32 `json:"calibrated"`
	Unavailable uint32 `json:"unavailable"`
}

type FindingCalibrationLedger struct {
	SchemaVersion        string                     `json:"schema_version"`
	CalibrationLedgerID  string                     `json:"calibration_ledger_id"`
	ReviewRunID          string                     `json:"review_run_id"`
	ExecutionID          string                     `json:"execution_id"`
	ReportID             string                     `json:"report_id"`
	VerificationLedgerID string                     `json:"verification_ledger_id"`
	TargetDigest         string                     `json:"target_digest"`
	Profile              *FindingCalibrationProfile `json:"profile,omitempty"`
	Summary              FindingCalibrationSummary  `json:"summary"`
	Facts                []FindingCalibrationFact   `json:"facts"`
	GeneratedAt          time.Time                  `json:"generated_at"`
}

type FindingSuppressionAction string

const (
	FindingNotSuppressed FindingSuppressionAction = "not_suppressed"
	FindingSuppressed    FindingSuppressionAction = "suppressed"
)

type FindingSuppressionFact struct {
	SuppressionFactID      string                   `json:"suppression_fact_id"`
	FindingID              string                   `json:"finding_id"`
	CandidateID            string                   `json:"candidate_id"`
	CalibrationFactID      string                   `json:"calibration_fact_id"`
	Sequence               uint32                   `json:"sequence"`
	PriorSuppressionFactID string                   `json:"prior_suppression_fact_id"`
	RankAvailable          bool                     `json:"rank_available"`
	Rank                   uint32                   `json:"rank"`
	Action                 FindingSuppressionAction `json:"action"`
	ReasonCode             string                   `json:"reason_code"`
	EvidenceIDs            []string                 `json:"evidence_ids"`
}

type FindingSuppressionSummary struct {
	Findings      uint32 `json:"findings"`
	NotSuppressed uint32 `json:"not_suppressed"`
	Suppressed    uint32 `json:"suppressed"`
}

type FindingSuppressionLedger struct {
	SchemaVersion        string                    `json:"schema_version"`
	SuppressionLedgerID  string                    `json:"suppression_ledger_id"`
	ReviewRunID          string                    `json:"review_run_id"`
	ExecutionID          string                    `json:"execution_id"`
	ReportID             string                    `json:"report_id"`
	CalibrationLedgerID  string                    `json:"calibration_ledger_id"`
	TargetDigest         string                    `json:"target_digest"`
	Policy               *VersionedRef             `json:"policy,omitempty"`
	PolicyApplied        bool                      `json:"policy_applied"`
	PolicyReasonCode     string                    `json:"policy_reason_code"`
	MinimumConfidencePPM uint32                    `json:"minimum_confidence_ppm"`
	MaxFindings          uint32                    `json:"max_findings"`
	Summary              FindingSuppressionSummary `json:"summary"`
	Facts                []FindingSuppressionFact  `json:"facts"`
	GeneratedAt          time.Time                 `json:"generated_at"`
}

func FindingCalibrationFactID(
	findingID string,
	verificationFactID string,
	status FindingCalibrationStatus,
	reasonCode string,
	profile *VersionedRef,
	rawScoreAvailable bool,
	rawScorePPM uint32,
	confidenceAvailable bool,
	confidencePPM uint32,
) string {
	return "calibration-fact-" + governedDigest([]any{
		findingID, verificationFactID, status, reasonCode, profile,
		rawScoreAvailable, rawScorePPM, confidenceAvailable, confidencePPM,
	})[:24]
}

func FindingSuppressionFactID(
	findingID string,
	calibrationFactID string,
	sequence uint32,
	priorID string,
	rankAvailable bool,
	rank uint32,
	action FindingSuppressionAction,
	reasonCode string,
	evidenceIDs []string,
) string {
	return "suppression-fact-" + governedDigest([]any{
		findingID, calibrationFactID, sequence, priorID, rankAvailable, rank,
		action, reasonCode, evidenceIDs,
	})[:24]
}

func SealFindingCalibrationLedger(
	ledger FindingCalibrationLedger,
) (FindingCalibrationLedger, error) {
	ledger.SchemaVersion = FindingCalibrationLedgerSchemaVersion
	ledger.CalibrationLedgerID = ""
	digest, err := DigestFindingCalibrationLedger(ledger)
	if err != nil {
		return FindingCalibrationLedger{}, err
	}
	ledger.CalibrationLedgerID = "calibration-ledger-" + digest[:24]
	if err := ledger.Validate(); err != nil {
		return FindingCalibrationLedger{}, err
	}
	return ledger, nil
}

func DigestFindingCalibrationLedger(ledger FindingCalibrationLedger) (string, error) {
	copy := ledger
	copy.CalibrationLedgerID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal FindingCalibrationLedger digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func DigestFindingCalibrationProfile(profile FindingCalibrationProfile) (string, error) {
	copy := profile
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal FindingCalibrationProfile digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (profile FindingCalibrationProfile) Validate() error {
	if profile.SchemaVersion != FindingCalibrationProfileSchemaVersion {
		return fmt.Errorf("unsupported FindingCalibrationProfile schema %q", profile.SchemaVersion)
	}
	if err := requireIdentifier("profile.id", profile.ID); err != nil {
		return err
	}
	if err := requireIdentifier("profile.revision", profile.Revision); err != nil {
		return err
	}
	if len(profile.Points) < 2 {
		return fmt.Errorf("profile.points requires at least two points")
	}
	var previousRaw, previousConfidence uint32
	for index, point := range profile.Points {
		if point.RawPPM > ConfidenceScalePPM || point.ConfidencePPM > ConfidenceScalePPM {
			return fmt.Errorf("profile.points[%d] exceeds ppm scale", index)
		}
		if index > 0 && (point.RawPPM <= previousRaw || point.ConfidencePPM < previousConfidence) {
			return fmt.Errorf("profile.points must be strictly ordered and monotonic")
		}
		previousRaw, previousConfidence = point.RawPPM, point.ConfidencePPM
	}
	if profile.Points[0].RawPPM != 0 || profile.Points[len(profile.Points)-1].RawPPM != ConfidenceScalePPM {
		return fmt.Errorf("profile.points must cover raw ppm endpoints")
	}
	digest, err := DigestFindingCalibrationProfile(profile)
	if err != nil {
		return err
	}
	if profile.SHA256 != digest {
		return fmt.Errorf("profile digest does not match its content")
	}
	return nil
}

func (profile FindingCalibrationProfile) Calibrate(raw uint32) (uint32, error) {
	if err := profile.Validate(); err != nil {
		return 0, err
	}
	if raw > ConfidenceScalePPM {
		return 0, fmt.Errorf("raw score exceeds ppm scale")
	}
	for index := 1; index < len(profile.Points); index++ {
		upper := profile.Points[index]
		if raw > upper.RawPPM {
			continue
		}
		lower := profile.Points[index-1]
		if raw == upper.RawPPM {
			return upper.ConfidencePPM, nil
		}
		rawOffset := uint64(raw - lower.RawPPM)
		rawSpan := uint64(upper.RawPPM - lower.RawPPM)
		confidenceSpan := uint64(upper.ConfidencePPM - lower.ConfidencePPM)
		return lower.ConfidencePPM + uint32(confidenceSpan*rawOffset/rawSpan), nil
	}
	return 0, fmt.Errorf("raw score is outside calibration profile")
}

func DecodeFindingCalibrationLedger(data []byte) (FindingCalibrationLedger, error) {
	var ledger FindingCalibrationLedger
	if err := decodeAgentReviewShadowJSON(data, &ledger); err != nil {
		return FindingCalibrationLedger{}, fmt.Errorf("decode FindingCalibrationLedger: %w", err)
	}
	if err := ledger.Validate(); err != nil {
		return FindingCalibrationLedger{}, err
	}
	return ledger, nil
}

func (ledger FindingCalibrationLedger) Validate() error {
	if ledger.SchemaVersion != FindingCalibrationLedgerSchemaVersion {
		return fmt.Errorf("unsupported FindingCalibrationLedger schema %q", ledger.SchemaVersion)
	}
	for name, value := range map[string]string{
		"calibration_ledger_id":  ledger.CalibrationLedgerID,
		"review_run_id":          ledger.ReviewRunID,
		"execution_id":           ledger.ExecutionID,
		"report_id":              ledger.ReportID,
		"verification_ledger_id": ledger.VerificationLedgerID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", ledger.TargetDigest); err != nil {
		return err
	}
	if ledger.Profile != nil {
		if err := ledger.Profile.Validate(); err != nil {
			return err
		}
	}
	if ledger.Facts == nil {
		return fmt.Errorf("facts must be an explicit array")
	}
	if err := validateAgentReviewTime("generated_at", ledger.GeneratedAt); err != nil {
		return err
	}
	previous := ""
	var calibrated, unavailable uint32
	for index, fact := range ledger.Facts {
		for name, value := range map[string]string{
			"calibration_fact_id":  fact.CalibrationFactID,
			"finding_id":           fact.FindingID,
			"candidate_id":         fact.CandidateID,
			"verification_fact_id": fact.VerificationFactID,
		} {
			if err := requireIdentifier("facts."+name, value); err != nil {
				return fmt.Errorf("facts[%d]: %w", index, err)
			}
		}
		if index > 0 && fact.FindingID <= previous {
			return fmt.Errorf("facts must be uniquely sorted by finding_id")
		}
		previous = fact.FindingID
		if err := requireIdentifier("facts.reason_code", fact.ReasonCode); err != nil {
			return fmt.Errorf("facts[%d]: %w", index, err)
		}
		if fact.RawScorePPM > ConfidenceScalePPM || fact.ConfidencePPM > ConfidenceScalePPM {
			return fmt.Errorf("facts[%d] score exceeds ppm scale", index)
		}
		switch fact.Status {
		case FindingCalibrationUnavailable:
			unavailable++
			if fact.RawScoreAvailable || fact.RawScorePPM != 0 ||
				fact.ConfidenceAvailable || fact.ConfidencePPM != 0 {
				return fmt.Errorf("facts[%d] unavailable calibration invented score", index)
			}
			if fact.Profile != nil {
				if err := fact.Profile.validate(fmt.Sprintf("facts[%d].profile", index)); err != nil {
					return err
				}
			}
		case FindingCalibrationCalibrated:
			calibrated++
			if fact.Profile == nil || !fact.RawScoreAvailable || !fact.ConfidenceAvailable {
				return fmt.Errorf("facts[%d] calibrated result requires profile and scores", index)
			}
			if err := fact.Profile.validate(fmt.Sprintf("facts[%d].profile", index)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("facts[%d] has unsupported status %q", index, fact.Status)
		}
		if ledger.Profile == nil {
			if fact.Profile != nil {
				return fmt.Errorf("facts[%d] references a profile absent from the ledger", index)
			}
		} else {
			profileRef := ledger.Profile.Ref()
			if fact.Profile == nil || *fact.Profile != profileRef {
				return fmt.Errorf("facts[%d] does not bind the ledger profile", index)
			}
			if fact.Status == FindingCalibrationCalibrated {
				wantConfidence, err := ledger.Profile.Calibrate(fact.RawScorePPM)
				if err != nil {
					return fmt.Errorf("facts[%d]: %w", index, err)
				}
				if fact.ConfidencePPM != wantConfidence {
					return fmt.Errorf("facts[%d] confidence is not the profile recomputation", index)
				}
			} else if fact.ReasonCode != "raw_score_unavailable" {
				return fmt.Errorf("facts[%d] configured profile has an invalid unavailable reason", index)
			}
		}
		wantID := FindingCalibrationFactID(
			fact.FindingID, fact.VerificationFactID, fact.Status, fact.ReasonCode,
			fact.Profile, fact.RawScoreAvailable, fact.RawScorePPM,
			fact.ConfidenceAvailable, fact.ConfidencePPM,
		)
		if fact.CalibrationFactID != wantID {
			return fmt.Errorf("facts[%d] identity does not bind canonical calibration", index)
		}
	}
	want := FindingCalibrationSummary{
		Findings: uint32(len(ledger.Facts)), Calibrated: calibrated, Unavailable: unavailable,
	}
	if ledger.Summary != want {
		return fmt.Errorf("calibration summary is not the fact recomputation")
	}
	digest, err := DigestFindingCalibrationLedger(ledger)
	if err != nil {
		return err
	}
	if ledger.CalibrationLedgerID != "calibration-ledger-"+digest[:24] {
		return fmt.Errorf("calibration_ledger_id does not match canonical facts")
	}
	return nil
}

func (ledger FindingCalibrationLedger) ValidateAgainst(
	report GovernedReviewReport,
	verification CandidateVerificationLedger,
) error {
	if err := report.ValidateAgainstVerificationLedger(verification); err != nil {
		return err
	}
	if err := ledger.Validate(); err != nil {
		return err
	}
	if ledger.ReviewRunID != report.ReviewRunID || ledger.ExecutionID != report.ExecutionID ||
		ledger.ReportID != report.ReportID ||
		ledger.VerificationLedgerID != verification.VerificationLedgerID ||
		ledger.TargetDigest != report.TargetDigest || !ledger.GeneratedAt.Equal(report.GeneratedAt) ||
		len(ledger.Facts) != len(report.Findings) {
		return fmt.Errorf("calibration ledger identity does not bind report and verification ledger")
	}
	facts := make(map[string]FindingCalibrationFact, len(ledger.Facts))
	for _, fact := range ledger.Facts {
		facts[fact.FindingID] = fact
	}
	for _, finding := range report.Findings {
		fact, exists := facts[finding.FindingID]
		verificationFact, found := verification.Latest(finding.CandidateID)
		candidate, candidateExists := governedCandidateByID(report.Candidates, finding.CandidateID)
		if !exists || !found || !candidateExists || fact.CandidateID != finding.CandidateID ||
			fact.VerificationFactID != verificationFact.VerificationFactID ||
			(fact.Status == FindingCalibrationCalibrated &&
				(!candidate.Hypothesis.RawConfidenceAvailable ||
					candidate.Hypothesis.RawConfidencePPM != fact.RawScorePPM)) ||
			(fact.Status == FindingCalibrationUnavailable && fact.Profile != nil &&
				candidate.Hypothesis.RawConfidenceAvailable) ||
			(fact.Status == FindingCalibrationUnavailable && fact.Profile == nil &&
				fact.ReasonCode != finding.ConfidenceReasonCode) {
			return fmt.Errorf("calibration fact does not bind finding %q", finding.FindingID)
		}
	}
	return nil
}

func governedCandidateByID(
	candidates []GovernedReviewCandidate,
	candidateID string,
) (GovernedReviewCandidate, bool) {
	for _, candidate := range candidates {
		if candidate.CandidateID == candidateID {
			return candidate, true
		}
	}
	return GovernedReviewCandidate{}, false
}

func SealFindingSuppressionLedger(
	ledger FindingSuppressionLedger,
) (FindingSuppressionLedger, error) {
	ledger.SchemaVersion = FindingSuppressionLedgerSchemaVersion
	ledger.SuppressionLedgerID = ""
	digest, err := DigestFindingSuppressionLedger(ledger)
	if err != nil {
		return FindingSuppressionLedger{}, err
	}
	ledger.SuppressionLedgerID = "suppression-ledger-" + digest[:24]
	if err := ledger.Validate(); err != nil {
		return FindingSuppressionLedger{}, err
	}
	return ledger, nil
}

func DigestFindingSuppressionLedger(ledger FindingSuppressionLedger) (string, error) {
	copy := ledger
	copy.SuppressionLedgerID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal FindingSuppressionLedger digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func DecodeFindingSuppressionLedger(data []byte) (FindingSuppressionLedger, error) {
	var ledger FindingSuppressionLedger
	if err := decodeAgentReviewShadowJSON(data, &ledger); err != nil {
		return FindingSuppressionLedger{}, fmt.Errorf("decode FindingSuppressionLedger: %w", err)
	}
	if err := ledger.Validate(); err != nil {
		return FindingSuppressionLedger{}, err
	}
	return ledger, nil
}

func (ledger FindingSuppressionLedger) Validate() error {
	if ledger.SchemaVersion != FindingSuppressionLedgerSchemaVersion {
		return fmt.Errorf("unsupported FindingSuppressionLedger schema %q", ledger.SchemaVersion)
	}
	for name, value := range map[string]string{
		"suppression_ledger_id": ledger.SuppressionLedgerID,
		"review_run_id":         ledger.ReviewRunID,
		"execution_id":          ledger.ExecutionID,
		"report_id":             ledger.ReportID,
		"calibration_ledger_id": ledger.CalibrationLedgerID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", ledger.TargetDigest); err != nil {
		return err
	}
	if ledger.MinimumConfidencePPM > ConfidenceScalePPM {
		return fmt.Errorf("minimum_confidence_ppm exceeds ppm scale")
	}
	if ledger.Policy == nil {
		if ledger.PolicyApplied || ledger.PolicyReasonCode != "not_configured" ||
			ledger.MinimumConfidencePPM != 0 || ledger.MaxFindings != 0 {
			return fmt.Errorf("disabled suppression policy invented bounds")
		}
	} else {
		if err := ledger.Policy.validate("policy"); err != nil {
			return err
		}
		if ledger.PolicyApplied && ledger.PolicyReasonCode != "policy_applied" {
			return fmt.Errorf("applied suppression policy requires policy_applied reason")
		}
		if !ledger.PolicyApplied && ledger.PolicyReasonCode != "calibration_unavailable" {
			return fmt.Errorf("deferred suppression policy requires calibration_unavailable reason")
		}
	}
	if ledger.Facts == nil {
		return fmt.Errorf("facts must be an explicit array")
	}
	if err := validateAgentReviewTime("generated_at", ledger.GeneratedAt); err != nil {
		return err
	}
	previous := ""
	var notSuppressed, suppressed uint32
	for index, fact := range ledger.Facts {
		for name, value := range map[string]string{
			"suppression_fact_id": fact.SuppressionFactID,
			"finding_id":          fact.FindingID,
			"candidate_id":        fact.CandidateID,
			"calibration_fact_id": fact.CalibrationFactID,
		} {
			if err := requireIdentifier("facts."+name, value); err != nil {
				return fmt.Errorf("facts[%d]: %w", index, err)
			}
		}
		if index > 0 && fact.FindingID <= previous {
			return fmt.Errorf("facts must be uniquely sorted by finding_id")
		}
		previous = fact.FindingID
		if fact.Sequence != 1 || fact.PriorSuppressionFactID != "" {
			return fmt.Errorf("facts[%d] MVP suppression sequence must start at one", index)
		}
		if !fact.RankAvailable && fact.Rank != 0 {
			return fmt.Errorf("facts[%d] unavailable rank must be zero", index)
		}
		if !ledger.PolicyApplied && fact.RankAvailable {
			return fmt.Errorf("facts[%d] unapplied policy must not invent rank", index)
		}
		if ledger.PolicyApplied && (!fact.RankAvailable || fact.Rank == 0) {
			return fmt.Errorf("facts[%d] enabled policy requires a positive rank", index)
		}
		if err := requireIdentifier("facts.reason_code", fact.ReasonCode); err != nil {
			return fmt.Errorf("facts[%d]: %w", index, err)
		}
		if fact.EvidenceIDs == nil || !slices.IsSorted(fact.EvidenceIDs) {
			return fmt.Errorf("facts[%d].evidence_ids must be an explicit sorted array", index)
		}
		for evidenceIndex, evidenceID := range fact.EvidenceIDs {
			if err := requireIdentifier("facts.evidence_id", evidenceID); err != nil {
				return fmt.Errorf("facts[%d].evidence_ids[%d]: %w", index, evidenceIndex, err)
			}
			if evidenceIndex > 0 && evidenceID == fact.EvidenceIDs[evidenceIndex-1] {
				return fmt.Errorf("facts[%d].evidence_ids contains duplicates", index)
			}
		}
		switch fact.Action {
		case FindingNotSuppressed:
			notSuppressed++
		case FindingSuppressed:
			suppressed++
		default:
			return fmt.Errorf("facts[%d] has unsupported action %q", index, fact.Action)
		}
		wantID := FindingSuppressionFactID(
			fact.FindingID, fact.CalibrationFactID, fact.Sequence,
			fact.PriorSuppressionFactID, fact.RankAvailable, fact.Rank,
			fact.Action, fact.ReasonCode, fact.EvidenceIDs,
		)
		if fact.SuppressionFactID != wantID {
			return fmt.Errorf("facts[%d] identity does not bind canonical suppression", index)
		}
	}
	want := FindingSuppressionSummary{
		Findings: uint32(len(ledger.Facts)), NotSuppressed: notSuppressed, Suppressed: suppressed,
	}
	if ledger.Summary != want {
		return fmt.Errorf("suppression summary is not the fact recomputation")
	}
	digest, err := DigestFindingSuppressionLedger(ledger)
	if err != nil {
		return err
	}
	if ledger.SuppressionLedgerID != "suppression-ledger-"+digest[:24] {
		return fmt.Errorf("suppression_ledger_id does not match canonical facts")
	}
	return nil
}

func (ledger FindingSuppressionLedger) ValidateAgainst(
	report GovernedReviewReport,
	calibration FindingCalibrationLedger,
) error {
	if err := calibration.Validate(); err != nil {
		return err
	}
	if err := ledger.Validate(); err != nil {
		return err
	}
	if ledger.ReviewRunID != report.ReviewRunID || ledger.ExecutionID != report.ExecutionID ||
		ledger.ReportID != report.ReportID ||
		ledger.CalibrationLedgerID != calibration.CalibrationLedgerID ||
		ledger.TargetDigest != report.TargetDigest || !ledger.GeneratedAt.Equal(report.GeneratedAt) ||
		len(ledger.Facts) != len(report.Findings) {
		return fmt.Errorf("suppression ledger identity does not bind report and calibration ledger")
	}
	calibrations := make(map[string]FindingCalibrationFact, len(calibration.Facts))
	for _, fact := range calibration.Facts {
		calibrations[fact.FindingID] = fact
	}
	decisions := make(map[string]GovernedFindingDecision, len(report.Decisions))
	for _, decision := range report.Decisions {
		decisions[decision.FindingID] = decision
	}
	for _, fact := range ledger.Facts {
		calibrationFact, calibrated := calibrations[fact.FindingID]
		decision, decided := decisions[fact.FindingID]
		if !calibrated || !decided || fact.CandidateID != calibrationFact.CandidateID ||
			fact.CalibrationFactID != calibrationFact.CalibrationFactID ||
			!reflect.DeepEqual(fact.EvidenceIDs, decision.EvidenceIDs) {
			return fmt.Errorf("suppression fact does not bind governed decision for %q", fact.FindingID)
		}
	}
	if !ledger.PolicyApplied {
		wantReason := "queued_for_human"
		if ledger.Policy != nil {
			wantReason = "calibration_unavailable_requires_human"
		}
		for _, fact := range ledger.Facts {
			if fact.Action != FindingNotSuppressed || fact.ReasonCode != wantReason ||
				fact.RankAvailable || fact.Rank != 0 {
				return fmt.Errorf("unapplied suppression policy changed finding %q", fact.FindingID)
			}
		}
		return nil
	}
	ordered := slices.Clone(ledger.Facts)
	slices.SortFunc(ordered, func(left, right FindingSuppressionFact) int {
		leftCalibration := calibrations[left.FindingID]
		rightCalibration := calibrations[right.FindingID]
		if leftCalibration.ConfidencePPM > rightCalibration.ConfidencePPM {
			return -1
		}
		if leftCalibration.ConfidencePPM < rightCalibration.ConfidencePPM {
			return 1
		}
		return strings.Compare(left.FindingID, right.FindingID)
	})
	eligibleRank := uint32(0)
	for index, fact := range ordered {
		calibrationFact := calibrations[fact.FindingID]
		if !calibrationFact.ConfidenceAvailable || fact.Rank != uint32(index+1) {
			return fmt.Errorf("suppression rank does not bind calibrated finding %q", fact.FindingID)
		}
		wantAction := FindingNotSuppressed
		wantReason := "selected_by_policy"
		if calibrationFact.ConfidencePPM < ledger.MinimumConfidencePPM {
			wantAction, wantReason = FindingSuppressed, "below_confidence_threshold"
		} else {
			eligibleRank++
			if eligibleRank > ledger.MaxFindings {
				wantAction, wantReason = FindingSuppressed, "finding_limit_exceeded"
			}
		}
		if fact.Action != wantAction || fact.ReasonCode != wantReason {
			return fmt.Errorf("suppression action is not the policy recomputation for %q", fact.FindingID)
		}
	}
	return nil
}
