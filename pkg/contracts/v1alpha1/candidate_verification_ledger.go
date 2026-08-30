package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"
)

const CandidateVerificationLedgerSchemaVersion = "argus.candidate_verification_ledger.v1alpha1"

type CandidateVerificationFact struct {
	VerificationFactID string                            `json:"verification_fact_id"`
	CandidateID        string                            `json:"candidate_id"`
	OccurrenceID       string                            `json:"occurrence_id"`
	Observation        HypothesisVerificationObservation `json:"observation"`
}

type CandidateVerificationSummary struct {
	Candidates   uint32 `json:"candidates"`
	Confirmed    uint32 `json:"confirmed"`
	Rejected     uint32 `json:"rejected"`
	Inconclusive uint32 `json:"inconclusive"`
	Unverified   uint32 `json:"unverified"`
}

// CandidateVerificationLedger is the immutable verification authority for a
// formal ReviewRun. ReviewHypothesisSet observations remain execution evidence;
// Candidate and Finding verdict fields are only projections that must close
// against this independently content-addressed ledger.
type CandidateVerificationLedger struct {
	SchemaVersion        string                       `json:"schema_version"`
	VerificationLedgerID string                       `json:"verification_ledger_id"`
	ReviewRunID          string                       `json:"review_run_id"`
	ExecutionID          string                       `json:"execution_id"`
	HypothesisSetID      string                       `json:"hypothesis_set_id"`
	TargetDigest         string                       `json:"target_digest"`
	Completeness         AgentReviewCompleteness      `json:"completeness"`
	Summary              CandidateVerificationSummary `json:"summary"`
	Facts                []CandidateVerificationFact  `json:"facts"`
	GeneratedAt          time.Time                    `json:"generated_at"`
}

func CandidateVerificationFactID(candidateID string, observation HypothesisVerificationObservation) string {
	return "verification-fact-" + governedDigest([]any{candidateID, observation})[:24]
}

func SealCandidateVerificationLedger(
	ledger CandidateVerificationLedger,
) (CandidateVerificationLedger, error) {
	ledger.SchemaVersion = CandidateVerificationLedgerSchemaVersion
	ledger.VerificationLedgerID = ""
	digest, err := DigestCandidateVerificationLedger(ledger)
	if err != nil {
		return CandidateVerificationLedger{}, err
	}
	ledger.VerificationLedgerID = "verification-ledger-" + digest[:24]
	if err := ledger.Validate(); err != nil {
		return CandidateVerificationLedger{}, err
	}
	return ledger, nil
}

func DigestCandidateVerificationLedger(ledger CandidateVerificationLedger) (string, error) {
	copy := ledger
	copy.VerificationLedgerID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal CandidateVerificationLedger digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func DecodeCandidateVerificationLedger(data []byte) (CandidateVerificationLedger, error) {
	var ledger CandidateVerificationLedger
	if err := decodeAgentReviewShadowJSON(data, &ledger); err != nil {
		return CandidateVerificationLedger{}, fmt.Errorf("decode CandidateVerificationLedger: %w", err)
	}
	if err := ledger.Validate(); err != nil {
		return CandidateVerificationLedger{}, err
	}
	return ledger, nil
}

func (ledger CandidateVerificationLedger) Validate() error {
	if ledger.SchemaVersion != CandidateVerificationLedgerSchemaVersion {
		return fmt.Errorf("unsupported CandidateVerificationLedger schema %q", ledger.SchemaVersion)
	}
	for name, value := range map[string]string{
		"verification_ledger_id": ledger.VerificationLedgerID,
		"review_run_id":          ledger.ReviewRunID,
		"execution_id":           ledger.ExecutionID,
		"hypothesis_set_id":      ledger.HypothesisSetID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", ledger.TargetDigest); err != nil {
		return err
	}
	if ledger.Completeness != AgentReviewComplete && ledger.Completeness != AgentReviewPartial {
		return fmt.Errorf("unsupported verification ledger completeness %q", ledger.Completeness)
	}
	if ledger.Facts == nil {
		return fmt.Errorf("facts must be an explicit array")
	}
	if err := validateAgentReviewTime("generated_at", ledger.GeneratedAt); err != nil {
		return err
	}
	previousCandidate := ""
	var previousSequence uint32
	latest := make(map[string]CandidateVerificationFact)
	occurrences := make(map[string]string)
	for index, fact := range ledger.Facts {
		for name, value := range map[string]string{
			"verification_fact_id": fact.VerificationFactID,
			"candidate_id":         fact.CandidateID,
			"occurrence_id":        fact.OccurrenceID,
		} {
			if err := requireIdentifier("facts."+name, value); err != nil {
				return fmt.Errorf("facts[%d]: %w", index, err)
			}
		}
		if fact.CandidateID != GovernedCandidateID(ledger.HypothesisSetID, fact.OccurrenceID) {
			return fmt.Errorf("facts[%d] candidate identity does not bind occurrence", index)
		}
		expectedSequence := uint32(1)
		if fact.CandidateID == previousCandidate {
			expectedSequence = previousSequence + 1
		}
		if err := validateLedgerVerificationObservation(
			fact.Observation, fmt.Sprintf("facts[%d].observation", index), expectedSequence,
		); err != nil {
			return err
		}
		if fact.VerificationFactID != CandidateVerificationFactID(fact.CandidateID, fact.Observation) {
			return fmt.Errorf("facts[%d] identity does not bind canonical observation", index)
		}
		if index > 0 && (fact.CandidateID < previousCandidate ||
			(fact.CandidateID == previousCandidate && fact.Observation.Sequence <= previousSequence)) {
			return fmt.Errorf("facts must be uniquely sorted by candidate_id and sequence")
		}
		if prior, exists := occurrences[fact.OccurrenceID]; exists && prior != fact.CandidateID {
			return fmt.Errorf("occurrence %q maps to multiple candidates", fact.OccurrenceID)
		}
		occurrences[fact.OccurrenceID] = fact.CandidateID
		latest[fact.CandidateID] = fact
		previousCandidate = fact.CandidateID
		previousSequence = fact.Observation.Sequence
	}
	var confirmed, rejected, inconclusive uint32
	for _, fact := range latest {
		switch fact.Observation.Verdict {
		case HypothesisVerificationConfirmed:
			confirmed++
		case HypothesisVerificationRejected:
			rejected++
		case HypothesisVerificationInconclusive:
			inconclusive++
		default:
			return fmt.Errorf("unsupported verification verdict %q", fact.Observation.Verdict)
		}
	}
	if ledger.Summary.Confirmed != confirmed || ledger.Summary.Rejected != rejected ||
		ledger.Summary.Inconclusive != inconclusive ||
		ledger.Summary.Candidates != confirmed+rejected+inconclusive+ledger.Summary.Unverified {
		return fmt.Errorf("verification summary is not the fact recomputation")
	}
	digest, err := DigestCandidateVerificationLedger(ledger)
	if err != nil {
		return err
	}
	if ledger.VerificationLedgerID != "verification-ledger-"+digest[:24] {
		return fmt.Errorf("verification_ledger_id does not match canonical facts")
	}
	return nil
}

func validateLedgerVerificationObservation(
	observation HypothesisVerificationObservation,
	name string,
	expectedSequence uint32,
) error {
	if err := requireIdentifier(name+".observation_id", observation.ObservationID); err != nil {
		return err
	}
	if observation.Sequence != expectedSequence {
		return fmt.Errorf("%s.sequence must be contiguous and start at one", name)
	}
	if err := observation.Verifier.validate(name + ".verifier"); err != nil {
		return err
	}
	switch observation.Verdict {
	case HypothesisVerificationConfirmed, HypothesisVerificationRejected,
		HypothesisVerificationInconclusive:
	default:
		return fmt.Errorf("%s has unsupported verdict %q", name, observation.Verdict)
	}
	if err := requireIdentifier(name+".reason_code", observation.ReasonCode); err != nil {
		return err
	}
	if err := requireBoundedAgentReviewText(name+".explanation", observation.Explanation, 8192, true); err != nil {
		return err
	}
	if observation.EvidenceIDs == nil || !slices.IsSorted(observation.EvidenceIDs) {
		return fmt.Errorf("%s.evidence_ids must be an explicit sorted array", name)
	}
	for index, evidenceID := range observation.EvidenceIDs {
		if err := requireIdentifier(fmt.Sprintf("%s.evidence_ids[%d]", name, index), evidenceID); err != nil {
			return err
		}
		if index > 0 && evidenceID == observation.EvidenceIDs[index-1] {
			return fmt.Errorf("%s.evidence_ids contains duplicate %q", name, evidenceID)
		}
	}
	if observation.Verdict == HypothesisVerificationConfirmed && len(observation.EvidenceIDs) == 0 {
		return fmt.Errorf("%s confirmed verdict requires exact evidence", name)
	}
	return nil
}

func (ledger CandidateVerificationLedger) Latest(candidateID string) (CandidateVerificationFact, bool) {
	index := sort.Search(len(ledger.Facts), func(index int) bool {
		return ledger.Facts[index].CandidateID >= candidateID
	})
	if index == len(ledger.Facts) || ledger.Facts[index].CandidateID != candidateID {
		return CandidateVerificationFact{}, false
	}
	latest := ledger.Facts[index]
	for index++; index < len(ledger.Facts) && ledger.Facts[index].CandidateID == candidateID; index++ {
		latest = ledger.Facts[index]
	}
	return latest, true
}

func (ledger CandidateVerificationLedger) ValidateAgainstHypothesisSet(set ReviewHypothesisSet) error {
	if err := set.Validate(); err != nil {
		return fmt.Errorf("source ReviewHypothesisSet: %w", err)
	}
	if err := ledger.Validate(); err != nil {
		return err
	}
	if ledger.ReviewRunID != set.ReviewRunID || ledger.ExecutionID != set.ExecutionID ||
		ledger.HypothesisSetID != set.HypothesisSetID || ledger.TargetDigest != set.TargetDigest ||
		ledger.Completeness != set.Completeness || !ledger.GeneratedAt.Equal(set.GeneratedAt) ||
		ledger.Summary.Candidates != uint32(len(set.Hypotheses)) {
		return fmt.Errorf("verification ledger identity does not bind exact ReviewHypothesisSet")
	}
	want := make([]CandidateVerificationFact, 0)
	for _, hypothesis := range set.Hypotheses {
		candidateID := GovernedCandidateID(set.HypothesisSetID, hypothesis.OccurrenceID)
		for _, observation := range hypothesis.Verification {
			want = append(want, CandidateVerificationFact{
				VerificationFactID: CandidateVerificationFactID(candidateID, observation),
				CandidateID:        candidateID, OccurrenceID: hypothesis.OccurrenceID,
				Observation: observation,
			})
		}
	}
	sort.Slice(want, func(left, right int) bool {
		if want[left].CandidateID != want[right].CandidateID {
			return want[left].CandidateID < want[right].CandidateID
		}
		return want[left].Observation.Sequence < want[right].Observation.Sequence
	})
	if !reflect.DeepEqual(ledger.Facts, want) {
		return fmt.Errorf("verification ledger changed or omitted source observations")
	}
	return nil
}
