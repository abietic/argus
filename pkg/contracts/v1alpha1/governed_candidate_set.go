package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

const GovernedCandidateSetSchemaVersion = "argus.governed_candidate_set.v1alpha1"

type GovernedCandidateSummary struct {
	Candidates   uint32 `json:"candidates"`
	Confirmed    uint32 `json:"confirmed"`
	Rejected     uint32 `json:"rejected"`
	Inconclusive uint32 `json:"inconclusive"`
}

// GovernedCandidateSet is the immutable candidate ledger head for one formal
// ReviewRun. It deliberately excludes Findings and Decisions so rejected and
// inconclusive review hypotheses remain independently addressable facts.
type GovernedCandidateSet struct {
	SchemaVersion   string                    `json:"schema_version"`
	CandidateSetID  string                    `json:"candidate_set_id"`
	ReviewRunID     string                    `json:"review_run_id"`
	ExecutionID     string                    `json:"execution_id"`
	HypothesisSetID string                    `json:"hypothesis_set_id"`
	TargetDigest    string                    `json:"target_digest"`
	Completeness    AgentReviewCompleteness   `json:"completeness"`
	Summary         GovernedCandidateSummary  `json:"summary"`
	Candidates      []GovernedReviewCandidate `json:"candidates"`
	GeneratedAt     time.Time                 `json:"generated_at"`
}

func SealGovernedCandidateSet(set GovernedCandidateSet) (GovernedCandidateSet, error) {
	set.SchemaVersion = GovernedCandidateSetSchemaVersion
	set.CandidateSetID = ""
	digest, err := DigestGovernedCandidateSet(set)
	if err != nil {
		return GovernedCandidateSet{}, err
	}
	set.CandidateSetID = "candidate-set-" + digest[:24]
	if err := set.Validate(); err != nil {
		return GovernedCandidateSet{}, err
	}
	return set, nil
}

func DigestGovernedCandidateSet(set GovernedCandidateSet) (string, error) {
	copy := set
	copy.CandidateSetID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal GovernedCandidateSet digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func DecodeGovernedCandidateSet(data []byte) (GovernedCandidateSet, error) {
	var set GovernedCandidateSet
	if err := decodeAgentReviewShadowJSON(data, &set); err != nil {
		return GovernedCandidateSet{}, fmt.Errorf("decode GovernedCandidateSet: %w", err)
	}
	if err := set.Validate(); err != nil {
		return GovernedCandidateSet{}, err
	}
	return set, nil
}

func (set GovernedCandidateSet) Validate() error {
	if set.SchemaVersion != GovernedCandidateSetSchemaVersion {
		return fmt.Errorf("unsupported GovernedCandidateSet schema %q", set.SchemaVersion)
	}
	for name, value := range map[string]string{
		"candidate_set_id": set.CandidateSetID, "review_run_id": set.ReviewRunID,
		"execution_id": set.ExecutionID, "hypothesis_set_id": set.HypothesisSetID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", set.TargetDigest); err != nil {
		return err
	}
	if set.Candidates == nil {
		return fmt.Errorf("candidates must be an explicit array")
	}
	if set.Completeness != AgentReviewComplete && set.Completeness != AgentReviewPartial {
		return fmt.Errorf("unsupported candidate set completeness %q", set.Completeness)
	}
	if err := validateAgentReviewTime("generated_at", set.GeneratedAt); err != nil {
		return err
	}
	var confirmed, rejected, inconclusive uint32
	previousID := ""
	for index, candidate := range set.Candidates {
		if err := candidate.Hypothesis.validate(index); err != nil {
			return err
		}
		wantID := GovernedCandidateID(set.HypothesisSetID, candidate.Hypothesis.OccurrenceID)
		if candidate.CandidateID != wantID {
			return fmt.Errorf("candidate %d identity does not bind its hypothesis", index)
		}
		if index > 0 && candidate.CandidateID <= previousID {
			return fmt.Errorf("candidates must be uniquely sorted by candidate_id")
		}
		previousID = candidate.CandidateID
		if err := requireIdentifier("candidate.reason_code", candidate.ReasonCode); err != nil {
			return err
		}
		latest, found := latestGovernedVerification(candidate.Hypothesis.Verification)
		wantDisposition := GovernedCandidateInconclusive
		wantReason := "verification_missing"
		if found {
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
	}
	wantSummary := GovernedCandidateSummary{
		Candidates: uint32(len(set.Candidates)), Confirmed: confirmed,
		Rejected: rejected, Inconclusive: inconclusive,
	}
	if set.Summary != wantSummary {
		return fmt.Errorf("candidate summary is not the fact recomputation")
	}
	digest, err := DigestGovernedCandidateSet(set)
	if err != nil {
		return err
	}
	if set.CandidateSetID != "candidate-set-"+digest[:24] {
		return fmt.Errorf("candidate_set_id does not match canonical candidate facts")
	}
	return nil
}

func (set GovernedCandidateSet) ValidateAgainstHypothesisSet(source ReviewHypothesisSet) error {
	if err := source.Validate(); err != nil {
		return fmt.Errorf("source ReviewHypothesisSet: %w", err)
	}
	if err := set.Validate(); err != nil {
		return err
	}
	if set.ReviewRunID != source.ReviewRunID || set.ExecutionID != source.ExecutionID ||
		set.HypothesisSetID != source.HypothesisSetID || set.TargetDigest != source.TargetDigest ||
		set.Completeness != source.Completeness || !set.GeneratedAt.Equal(source.GeneratedAt) ||
		len(set.Candidates) != len(source.Hypotheses) {
		return fmt.Errorf("candidate set identity does not bind exact ReviewHypothesisSet")
	}
	byOccurrence := make(map[string]GovernedReviewCandidate, len(set.Candidates))
	for _, candidate := range set.Candidates {
		byOccurrence[candidate.Hypothesis.OccurrenceID] = candidate
	}
	for _, hypothesis := range source.Hypotheses {
		candidate, exists := byOccurrence[hypothesis.OccurrenceID]
		if !exists || !reflect.DeepEqual(candidate.Hypothesis, hypothesis) {
			return fmt.Errorf("candidate set changed or omitted hypothesis %q", hypothesis.OccurrenceID)
		}
	}
	return nil
}

// ValidateAgainstVerificationLedger proves that candidate dispositions are
// read-only projections of the independent verification authority.
func (set GovernedCandidateSet) ValidateAgainstVerificationLedger(
	ledger CandidateVerificationLedger,
) error {
	if err := set.Validate(); err != nil {
		return err
	}
	if err := ledger.Validate(); err != nil {
		return err
	}
	if set.ReviewRunID != ledger.ReviewRunID || set.ExecutionID != ledger.ExecutionID ||
		set.HypothesisSetID != ledger.HypothesisSetID || set.TargetDigest != ledger.TargetDigest ||
		set.Completeness != ledger.Completeness || !set.GeneratedAt.Equal(ledger.GeneratedAt) ||
		set.Summary.Candidates != ledger.Summary.Candidates ||
		set.Summary.Confirmed != ledger.Summary.Confirmed ||
		set.Summary.Rejected != ledger.Summary.Rejected ||
		set.Summary.Inconclusive != ledger.Summary.Inconclusive {
		return fmt.Errorf("candidate set identity or summary does not bind verification ledger")
	}
	for _, candidate := range set.Candidates {
		fact, found := ledger.Latest(candidate.CandidateID)
		wantDisposition := GovernedCandidateInconclusive
		wantReason := "verification_missing"
		if found {
			wantReason = fact.Observation.ReasonCode
			switch fact.Observation.Verdict {
			case HypothesisVerificationConfirmed:
				wantDisposition = GovernedCandidateConfirmed
			case HypothesisVerificationRejected:
				wantDisposition = GovernedCandidateRejected
			case HypothesisVerificationInconclusive:
				wantDisposition = GovernedCandidateInconclusive
			}
		}
		if candidate.Disposition != wantDisposition || candidate.ReasonCode != wantReason {
			return fmt.Errorf("candidate %q changed verification ledger projection", candidate.CandidateID)
		}
	}
	return nil
}
