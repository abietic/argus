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

const GovernedReviewReportSchemaVersion = "argus.governed_review_report.v1alpha1"

type GovernedCandidateDisposition string

const (
	GovernedCandidateConfirmed    GovernedCandidateDisposition = "confirmed"
	GovernedCandidateRejected     GovernedCandidateDisposition = "rejected"
	GovernedCandidateInconclusive GovernedCandidateDisposition = "inconclusive"
)

type GovernedFindingDecisionAction string

const GovernedFindingQueuedForHuman GovernedFindingDecisionAction = "queued_for_human"

// GovernedReviewCandidate preserves every canonical retained hypothesis after
// host admission. Disposition is a projection of the latest independent
// verification observation; it is not a Finding or publication decision.
type GovernedReviewCandidate struct {
	CandidateID string                       `json:"candidate_id"`
	Hypothesis  ReviewHypothesis             `json:"hypothesis"`
	Disposition GovernedCandidateDisposition `json:"disposition"`
	ReasonCode  string                       `json:"reason_code"`
}

type GovernedReviewFinding struct {
	FindingID    string                            `json:"finding_id"`
	Fingerprint  string                            `json:"fingerprint"`
	CandidateID  string                            `json:"candidate_id"`
	OccurrenceID string                            `json:"occurrence_id"`
	Dimension    VersionedRef                      `json:"dimension"`
	Category     string                            `json:"category"`
	Severity     HypothesisSeverity                `json:"severity"`
	Title        string                            `json:"title"`
	Description  string                            `json:"description"`
	Impact       string                            `json:"impact"`
	Anchor       HypothesisSourceAnchor            `json:"anchor"`
	Evidence     []HypothesisEvidence              `json:"evidence"`
	Suggestion   *string                           `json:"suggestion,omitempty"`
	Verification HypothesisVerificationObservation `json:"verification"`

	ConfidenceAvailable  bool   `json:"confidence_available"`
	ConfidenceReasonCode string `json:"confidence_reason_code"`
}

// GovernedFindingDecision remains distinct from Finding and Feedback. The
// first MVP action is intentionally closed to queued_for_human: provider and
// worker evidence can establish a review Finding but cannot grant publication.
type GovernedFindingDecision struct {
	DecisionID      string                        `json:"decision_id"`
	FindingID       string                        `json:"finding_id"`
	Sequence        uint32                        `json:"sequence"`
	PriorDecisionID string                        `json:"prior_decision_id"`
	Action          GovernedFindingDecisionAction `json:"action"`
	ReasonCode      string                        `json:"reason_code"`
	EvidenceIDs     []string                      `json:"evidence_ids"`
}

type GovernedReviewSummary struct {
	Candidates   uint32 `json:"candidates"`
	Confirmed    uint32 `json:"confirmed"`
	Rejected     uint32 `json:"rejected"`
	Inconclusive uint32 `json:"inconclusive"`
	Findings     uint32 `json:"findings"`
	QueuedHuman  uint32 `json:"queued_for_human"`
}

type GovernedReviewReport struct {
	SchemaVersion   string                    `json:"schema_version"`
	ReportID        string                    `json:"report_id"`
	ReviewRunID     string                    `json:"review_run_id"`
	ExecutionID     string                    `json:"execution_id"`
	HypothesisSetID string                    `json:"hypothesis_set_id"`
	TargetDigest    string                    `json:"target_digest"`
	Completeness    AgentReviewCompleteness   `json:"completeness"`
	Summary         GovernedReviewSummary     `json:"summary"`
	Candidates      []GovernedReviewCandidate `json:"candidates"`
	Findings        []GovernedReviewFinding   `json:"findings"`
	Decisions       []GovernedFindingDecision `json:"decisions"`
	GeneratedAt     time.Time                 `json:"generated_at"`
}

func GovernedCandidateID(hypothesisSetID, occurrenceID string) string {
	return "candidate-" + governedDigest([]string{hypothesisSetID, occurrenceID})[:24]
}

func GovernedFindingID(targetDigest string, hypothesis ReviewHypothesis) string {
	return "finding-" + governedDigest([]any{
		targetDigest,
		hypothesis.ClusterFingerprint,
		hypothesis.Anchor,
	})[:24]
}

func GovernedDecisionID(
	findingID string,
	sequence uint32,
	action GovernedFindingDecisionAction,
	reasonCode string,
	evidenceIDs []string,
) string {
	return "decision-" + governedDigest([]any{
		findingID, sequence, action, reasonCode, evidenceIDs,
	})[:24]
}

func SealGovernedReviewReport(
	report GovernedReviewReport,
) (GovernedReviewReport, error) {
	report.SchemaVersion = GovernedReviewReportSchemaVersion
	report.ReportID = ""
	digest, err := DigestGovernedReviewReport(report)
	if err != nil {
		return GovernedReviewReport{}, err
	}
	report.ReportID = "review-report-" + digest[:24]
	if err := report.Validate(); err != nil {
		return GovernedReviewReport{}, err
	}
	return report, nil
}

func DigestGovernedReviewReport(report GovernedReviewReport) (string, error) {
	copy := report
	copy.ReportID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal GovernedReviewReport digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func DecodeGovernedReviewReport(data []byte) (GovernedReviewReport, error) {
	var report GovernedReviewReport
	if err := decodeAgentReviewShadowJSON(data, &report); err != nil {
		return GovernedReviewReport{}, fmt.Errorf("decode GovernedReviewReport: %w", err)
	}
	if err := report.Validate(); err != nil {
		return GovernedReviewReport{}, err
	}
	return report, nil
}

func (report GovernedReviewReport) Validate() error {
	if report.SchemaVersion != GovernedReviewReportSchemaVersion {
		return fmt.Errorf("unsupported GovernedReviewReport schema %q", report.SchemaVersion)
	}
	for name, value := range map[string]string{
		"report_id": report.ReportID, "review_run_id": report.ReviewRunID,
		"execution_id": report.ExecutionID, "hypothesis_set_id": report.HypothesisSetID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", report.TargetDigest); err != nil {
		return err
	}
	if report.Candidates == nil || report.Findings == nil || report.Decisions == nil {
		return fmt.Errorf("candidates, findings, and decisions must be explicit arrays")
	}
	if report.Completeness != AgentReviewComplete && report.Completeness != AgentReviewPartial {
		return fmt.Errorf("unsupported governed report completeness %q", report.Completeness)
	}
	if err := validateAgentReviewTime("generated_at", report.GeneratedAt); err != nil {
		return err
	}

	candidates := make(map[string]GovernedReviewCandidate, len(report.Candidates))
	var confirmed, rejected, inconclusive uint32
	previousCandidate := ""
	for index, candidate := range report.Candidates {
		if err := candidate.Hypothesis.validate(index); err != nil {
			return err
		}
		wantID := GovernedCandidateID(report.HypothesisSetID, candidate.Hypothesis.OccurrenceID)
		if candidate.CandidateID != wantID {
			return fmt.Errorf("candidate %d identity does not bind its hypothesis", index)
		}
		if index > 0 && candidate.CandidateID <= previousCandidate {
			return fmt.Errorf("candidates must be uniquely sorted by candidate_id")
		}
		previousCandidate = candidate.CandidateID
		if err := requireIdentifier("candidate.reason_code", candidate.ReasonCode); err != nil {
			return err
		}
		latest, hasVerification := latestGovernedVerification(candidate.Hypothesis.Verification)
		wantDisposition := GovernedCandidateInconclusive
		wantReason := "verification_missing"
		if hasVerification {
			wantReason = latest.ReasonCode
			switch latest.Verdict {
			case HypothesisVerificationConfirmed:
				wantDisposition = GovernedCandidateConfirmed
			case HypothesisVerificationRejected:
				wantDisposition = GovernedCandidateRejected
			case HypothesisVerificationInconclusive:
				wantDisposition = GovernedCandidateInconclusive
			}
		}
		if candidate.Disposition != wantDisposition || candidate.ReasonCode != wantReason {
			return fmt.Errorf("candidate disposition is not the latest verification projection")
		}
		switch candidate.Disposition {
		case GovernedCandidateConfirmed:
			confirmed++
		case GovernedCandidateRejected:
			rejected++
		case GovernedCandidateInconclusive:
			inconclusive++
		default:
			return fmt.Errorf("candidate has unsupported disposition %q", candidate.Disposition)
		}
		candidates[candidate.CandidateID] = candidate
	}

	findings := make(map[string]GovernedReviewFinding, len(report.Findings))
	previousFinding := ""
	for index, finding := range report.Findings {
		candidate, exists := candidates[finding.CandidateID]
		if !exists || candidate.Disposition != GovernedCandidateConfirmed {
			return fmt.Errorf("finding %d does not reference a confirmed candidate", index)
		}
		hypothesis := candidate.Hypothesis
		wantID := GovernedFindingID(report.TargetDigest, hypothesis)
		if finding.FindingID != wantID || finding.Fingerprint != hypothesis.ClusterFingerprint ||
			finding.OccurrenceID != hypothesis.OccurrenceID || finding.Dimension != hypothesis.Dimension ||
			finding.Category != hypothesis.Category || finding.Severity != hypothesis.Severity ||
			finding.Title != hypothesis.Title || finding.Description != hypothesis.Description ||
			finding.Impact != hypothesis.Impact || finding.Anchor != hypothesis.Anchor ||
			!reflect.DeepEqual(finding.Evidence, hypothesis.Evidence) ||
			!reflect.DeepEqual(finding.Suggestion, hypothesis.Suggestion) {
			return fmt.Errorf("finding %d is not an exact confirmed hypothesis projection", index)
		}
		latest, ok := latestGovernedVerification(hypothesis.Verification)
		if !ok || latest.Verdict != HypothesisVerificationConfirmed ||
			!reflect.DeepEqual(finding.Verification, latest) {
			return fmt.Errorf("finding %d lacks the exact latest confirmed verification", index)
		}
		if finding.ConfidenceAvailable || finding.ConfidenceReasonCode != "not_calibrated" {
			return fmt.Errorf("MVP governed finding must explicitly mark confidence unavailable")
		}
		if index > 0 && finding.FindingID <= previousFinding {
			return fmt.Errorf("findings must be uniquely sorted by finding_id")
		}
		previousFinding = finding.FindingID
		findings[finding.FindingID] = finding
	}
	if len(findings) != int(confirmed) {
		return fmt.Errorf("every confirmed candidate must produce exactly one finding")
	}

	previousDecision := ""
	decided := make(map[string]struct{}, len(report.Decisions))
	for index, decision := range report.Decisions {
		finding, exists := findings[decision.FindingID]
		if !exists || decision.Sequence != 1 || decision.PriorDecisionID != "" ||
			decision.Action != GovernedFindingQueuedForHuman ||
			decision.ReasonCode != "agent_confirmed_requires_human_review" {
			return fmt.Errorf("decision %d is outside MVP human-review authority", index)
		}
		wantEvidence := governedEvidenceIDs(finding.Verification.EvidenceIDs)
		if !slices.Equal(decision.EvidenceIDs, wantEvidence) {
			return fmt.Errorf("decision %d evidence does not bind finding verification", index)
		}
		wantID := GovernedDecisionID(
			decision.FindingID, decision.Sequence, decision.Action,
			decision.ReasonCode, decision.EvidenceIDs,
		)
		if decision.DecisionID != wantID {
			return fmt.Errorf("decision %d identity does not match canonical fields", index)
		}
		if index > 0 && decision.DecisionID <= previousDecision {
			return fmt.Errorf("decisions must be uniquely sorted by decision_id")
		}
		if _, duplicate := decided[decision.FindingID]; duplicate {
			return fmt.Errorf("finding %q has multiple initial decisions", decision.FindingID)
		}
		previousDecision = decision.DecisionID
		decided[decision.FindingID] = struct{}{}
	}
	if len(decided) != len(findings) {
		return fmt.Errorf("every finding must have exactly one initial decision")
	}
	wantSummary := GovernedReviewSummary{
		Candidates: uint32(len(candidates)), Confirmed: confirmed, Rejected: rejected,
		Inconclusive: inconclusive, Findings: uint32(len(findings)),
		QueuedHuman: uint32(len(decided)),
	}
	if report.Summary != wantSummary {
		return fmt.Errorf("governed review summary is not the fact recomputation")
	}
	digest, err := DigestGovernedReviewReport(report)
	if err != nil {
		return err
	}
	if report.ReportID != "review-report-"+digest[:24] {
		return fmt.Errorf("report_id does not match canonical report facts")
	}
	return nil
}

// ValidateAgainstHypothesisSet closes the governed report over the exact
// admitted source artifact rather than accepting a self-consistent substitute.
func (report GovernedReviewReport) ValidateAgainstHypothesisSet(
	set ReviewHypothesisSet,
) error {
	if err := set.Validate(); err != nil {
		return fmt.Errorf("source ReviewHypothesisSet: %w", err)
	}
	if err := report.Validate(); err != nil {
		return err
	}
	if report.ReviewRunID != set.ReviewRunID || report.ExecutionID != set.ExecutionID ||
		report.HypothesisSetID != set.HypothesisSetID || report.TargetDigest != set.TargetDigest ||
		report.Completeness != set.Completeness || !report.GeneratedAt.Equal(set.GeneratedAt) ||
		len(report.Candidates) != len(set.Hypotheses) {
		return fmt.Errorf("governed report identity does not bind exact ReviewHypothesisSet")
	}
	candidates := make(map[string]GovernedReviewCandidate, len(report.Candidates))
	for _, candidate := range report.Candidates {
		candidates[candidate.Hypothesis.OccurrenceID] = candidate
	}
	for _, hypothesis := range set.Hypotheses {
		candidate, exists := candidates[hypothesis.OccurrenceID]
		if !exists || !reflect.DeepEqual(candidate.Hypothesis, hypothesis) {
			return fmt.Errorf(
				"governed report changed or omitted hypothesis %q", hypothesis.OccurrenceID,
			)
		}
	}
	return nil
}

// ValidateAgainstVerificationLedger closes every report verdict, promoted
// Finding, and initial decision over the independent verification authority.
func (report GovernedReviewReport) ValidateAgainstVerificationLedger(
	ledger CandidateVerificationLedger,
) error {
	if err := report.Validate(); err != nil {
		return err
	}
	if err := ledger.Validate(); err != nil {
		return err
	}
	if report.ReviewRunID != ledger.ReviewRunID || report.ExecutionID != ledger.ExecutionID ||
		report.HypothesisSetID != ledger.HypothesisSetID || report.TargetDigest != ledger.TargetDigest ||
		report.Completeness != ledger.Completeness || !report.GeneratedAt.Equal(ledger.GeneratedAt) ||
		report.Summary.Candidates != ledger.Summary.Candidates ||
		report.Summary.Confirmed != ledger.Summary.Confirmed ||
		report.Summary.Rejected != ledger.Summary.Rejected ||
		report.Summary.Inconclusive != ledger.Summary.Inconclusive {
		return fmt.Errorf("governed report identity or summary does not bind verification ledger")
	}
	findings := make(map[string]GovernedReviewFinding, len(report.Findings))
	for _, finding := range report.Findings {
		fact, found := ledger.Latest(finding.CandidateID)
		if !found || fact.Observation.Verdict != HypothesisVerificationConfirmed ||
			!reflect.DeepEqual(finding.Verification, fact.Observation) {
			return fmt.Errorf("finding %q changed verification ledger projection", finding.FindingID)
		}
		findings[finding.FindingID] = finding
	}
	for _, decision := range report.Decisions {
		finding, exists := findings[decision.FindingID]
		if !exists {
			return fmt.Errorf("decision %q has no ledger-backed finding", decision.DecisionID)
		}
		fact, found := ledger.Latest(finding.CandidateID)
		if !found || !slices.Equal(decision.EvidenceIDs, governedEvidenceIDs(fact.Observation.EvidenceIDs)) {
			return fmt.Errorf("decision %q changed verification ledger evidence", decision.DecisionID)
		}
	}
	return nil
}

func RenderGovernedReviewMarkdown(report GovernedReviewReport) (string, error) {
	if err := report.Validate(); err != nil {
		return "", err
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Argus Governed Review\n\n")
	fmt.Fprintf(&builder, "- Run: `%s`\n", report.ReviewRunID)
	fmt.Fprintf(&builder, "- Completeness: `%s`\n", report.Completeness)
	fmt.Fprintf(&builder, "- Candidates: %d\n", report.Summary.Candidates)
	fmt.Fprintf(&builder, "- Findings queued for human review: %d\n\n", report.Summary.QueuedHuman)
	if len(report.Findings) == 0 {
		builder.WriteString("No governed findings.\n")
		return builder.String(), nil
	}
	for _, finding := range report.Findings {
		fmt.Fprintf(&builder, "## [%s] %s\n\n", finding.Severity, finding.Title)
		fmt.Fprintf(&builder, "`%s:%d` · `%s`\n\n", finding.Anchor.Path, finding.Anchor.StartLine, finding.Category)
		fmt.Fprintf(&builder, "%s\n\nImpact: %s\n\n", finding.Description, finding.Impact)
		fmt.Fprintf(&builder, "Decision: `queued_for_human` (%s)\n\n", finding.Verification.ReasonCode)
	}
	return builder.String(), nil
}

func latestGovernedVerification(
	observations []HypothesisVerificationObservation,
) (HypothesisVerificationObservation, bool) {
	if len(observations) == 0 {
		return HypothesisVerificationObservation{}, false
	}
	latest := observations[0]
	for _, observation := range observations[1:] {
		if observation.Sequence > latest.Sequence {
			latest = observation
		}
	}
	return latest, true
}

func governedEvidenceIDs(values []string) []string {
	result := append([]string{}, values...)
	slices.Sort(result)
	return result
}

func governedDigest(value any) string {
	data, _ := json.Marshal(value)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
