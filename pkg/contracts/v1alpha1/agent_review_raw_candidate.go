package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	AgentReviewRawCandidateCollectionSchemaVersion   = "argus.agent_review_raw_candidate_collection.v1alpha1"
	AgentReviewRawCandidateAuthorityWorkerSelfReport = "worker_self_report"
	AgentReviewRawCandidateDispositionShadowOnly     = "shadow_only"

	agentReviewMaxRawCandidates = 5120
)

// AgentReviewRawCandidateCollection is a bounded diagnostic artifact. It
// preserves the exact typed claim submitted by the worker before host
// normalization, including claims later rejected as invalid or excluded by a
// candidate budget. The payload is evidence about model output, not trusted
// source evidence and never a Finding or gold label.
type AgentReviewRawCandidateCollection struct {
	SchemaVersion string                           `json:"schema_version"`
	PlanID        string                           `json:"plan_id"`
	SourceRunID   string                           `json:"source_run_id"`
	ExecutionID   string                           `json:"execution_id"`
	ReviewRunID   string                           `json:"review_run_id"`
	TargetDigest  string                           `json:"target_digest"`
	Authority     string                           `json:"authority"`
	Disposition   string                           `json:"disposition"`
	RawCandidates []AgentReviewRawCandidatePayload `json:"raw_candidates"`
}

type AgentReviewRawCandidatePayload struct {
	RawCandidateID string                        `json:"raw_candidate_id"`
	GroupID        string                        `json:"group_id"`
	Dimension      VersionedRef                  `json:"dimension"`
	Ordinal        uint32                        `json:"ordinal"`
	ClaimDigest    string                        `json:"claim_digest"`
	Action         HypothesisNormalizationAction `json:"action"`
	ReasonCode     string                        `json:"reason_code"`
	Claim          AgentReviewRawCandidateClaim  `json:"claim"`
}

type AgentReviewRawCandidateClaim struct {
	Category         string                              `json:"category"`
	Severity         HypothesisSeverity                  `json:"severity"`
	RawConfidencePPM *uint32                             `json:"raw_confidence_ppm,omitempty"`
	Title            string                              `json:"title"`
	Description      string                              `json:"description"`
	Impact           string                              `json:"impact"`
	Anchor           AgentReviewRawCandidateSourceAnchor `json:"anchor"`
	Evidence         []AgentReviewRawCandidateEvidence   `json:"evidence"`
	Suggestion       *string                             `json:"suggestion,omitempty"`
}

// AgentReviewRawCandidateSourceAnchor is deliberately not a trusted
// HypothesisSourceAnchor: it has no host-attested source digest and may be the
// reason the candidate was rejected.
type AgentReviewRawCandidateSourceAnchor struct {
	Path      string               `json:"path"`
	Side      HypothesisAnchorSide `json:"side"`
	StartLine uint32               `json:"start_line"`
	EndLine   uint32               `json:"end_line"`
}

type AgentReviewRawCandidateEvidence struct {
	Statement string                              `json:"statement"`
	Anchor    AgentReviewRawCandidateSourceAnchor `json:"anchor"`
	Excerpt   string                              `json:"excerpt"`
}

func DecodeAgentReviewRawCandidateCollection(data []byte) (AgentReviewRawCandidateCollection, error) {
	var value AgentReviewRawCandidateCollection
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return AgentReviewRawCandidateCollection{}, fmt.Errorf(
			"decode AgentReviewRawCandidateCollection: %w", err,
		)
	}
	if err := value.Validate(); err != nil {
		return AgentReviewRawCandidateCollection{}, err
	}
	return value, nil
}

func (collection AgentReviewRawCandidateCollection) Validate() error {
	if collection.SchemaVersion != AgentReviewRawCandidateCollectionSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentReviewRawCandidateCollection schema %q",
			collection.SchemaVersion,
		)
	}
	for name, value := range map[string]string{
		"plan_id": collection.PlanID, "source_run_id": collection.SourceRunID,
		"execution_id": collection.ExecutionID, "review_run_id": collection.ReviewRunID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", collection.TargetDigest); err != nil {
		return err
	}
	if collection.Authority != AgentReviewRawCandidateAuthorityWorkerSelfReport {
		return fmt.Errorf("authority must be worker_self_report")
	}
	if collection.Disposition != AgentReviewRawCandidateDispositionShadowOnly {
		return fmt.Errorf("disposition must be shadow_only")
	}
	if collection.RawCandidates == nil {
		return fmt.Errorf("raw_candidates must be an explicit array")
	}
	if len(collection.RawCandidates) > agentReviewMaxRawCandidates {
		return fmt.Errorf("raw_candidates exceeds %d entries", agentReviewMaxRawCandidates)
	}
	previous := ""
	for index, candidate := range collection.RawCandidates {
		if err := candidate.validate(index); err != nil {
			return err
		}
		if index > 0 && candidate.RawCandidateID <= previous {
			return fmt.Errorf("raw_candidates must be uniquely sorted by raw_candidate_id")
		}
		previous = candidate.RawCandidateID
	}
	return nil
}

func (candidate AgentReviewRawCandidatePayload) validate(index int) error {
	name := fmt.Sprintf("raw_candidates[%d]", index)
	if err := requireIdentifier(name+".raw_candidate_id", candidate.RawCandidateID); err != nil {
		return err
	}
	if err := requireIdentifier(name+".group_id", candidate.GroupID); err != nil {
		return err
	}
	if err := candidate.Dimension.validate(name + ".dimension"); err != nil {
		return err
	}
	if err := requireSHA256(name+".claim_digest", candidate.ClaimDigest); err != nil {
		return err
	}
	if err := requireIdentifier(name+".reason_code", candidate.ReasonCode); err != nil {
		return err
	}
	switch candidate.Action {
	case HypothesisNormalizationRetained:
		if candidate.ReasonCode != "normalized_candidate_retained" {
			return fmt.Errorf("%s retained action has invalid reason_code", name)
		}
	case HypothesisNormalizationMergedDuplicate:
		if candidate.ReasonCode != "duplicate_fingerprint" &&
			candidate.ReasonCode != "semantic_duplicate" {
			return fmt.Errorf("%s merged_duplicate action has invalid reason_code", name)
		}
	case HypothesisNormalizationRejectedInvalid:
		if candidate.ReasonCode != "invalid_candidate" {
			return fmt.Errorf("%s rejected_invalid action has invalid reason_code", name)
		}
	case HypothesisNormalizationExcludedBudget:
		if candidate.ReasonCode != "candidate_budget_exceeded" {
			return fmt.Errorf("%s excluded_budget action has invalid reason_code", name)
		}
	default:
		return fmt.Errorf("%s has unsupported action %q", name, candidate.Action)
	}
	if err := candidate.Claim.validate(name + ".claim"); err != nil {
		return err
	}
	digest, err := DigestAgentReviewRawCandidateClaim(candidate.Claim)
	if err != nil {
		return err
	}
	if digest != candidate.ClaimDigest {
		return fmt.Errorf("%s claim_digest does not bind the canonical claim", name)
	}
	return nil
}

func (claim AgentReviewRawCandidateClaim) validate(name string) error {
	if claim.RawConfidencePPM != nil && *claim.RawConfidencePPM > ConfidenceScalePPM {
		return fmt.Errorf("%s.raw_confidence_ppm exceeds ppm scale", name)
	}
	if err := requireBoundedAgentReviewText(name+".category", claim.Category, 100, true); err != nil {
		return err
	}
	switch claim.Severity {
	case HypothesisSeverityCritical, HypothesisSeverityHigh,
		HypothesisSeverityMedium, HypothesisSeverityLow:
	default:
		return fmt.Errorf("%s has unsupported severity %q", name, claim.Severity)
	}
	for field, bounded := range map[string]struct {
		value string
		max   int
	}{
		"title": {claim.Title, 240}, "description": {claim.Description, 4000},
		"impact": {claim.Impact, 2000},
	} {
		if err := requireBoundedAgentReviewText(name+"."+field, bounded.value, bounded.max, true); err != nil {
			return err
		}
	}
	if err := claim.Anchor.validate(name + ".anchor"); err != nil {
		return err
	}
	if claim.Evidence == nil || len(claim.Evidence) == 0 || len(claim.Evidence) > 12 {
		return fmt.Errorf("%s.evidence must contain 1..12 entries", name)
	}
	for index, evidence := range claim.Evidence {
		evidenceName := fmt.Sprintf("%s.evidence[%d]", name, index)
		if err := requireBoundedAgentReviewText(evidenceName+".statement", evidence.Statement, 2000, true); err != nil {
			return err
		}
		if err := evidence.Anchor.validate(evidenceName + ".anchor"); err != nil {
			return err
		}
		if err := requireBoundedAgentReviewText(evidenceName+".excerpt", evidence.Excerpt, 4000, true); err != nil {
			return err
		}
	}
	if claim.Suggestion != nil {
		if err := requireBoundedAgentReviewText(name+".suggestion", *claim.Suggestion, 3000, true); err != nil {
			return err
		}
	}
	return nil
}

func (anchor AgentReviewRawCandidateSourceAnchor) validate(name string) error {
	if err := requireBoundedAgentReviewText(name+".path", anchor.Path, 4096, true); err != nil {
		return err
	}
	if strings.ContainsRune(anchor.Path, '\x00') {
		return fmt.Errorf("%s.path contains NUL", name)
	}
	switch anchor.Side {
	case HypothesisAnchorOld, HypothesisAnchorNew, HypothesisAnchorFile:
	default:
		return fmt.Errorf("%s has unsupported side %q", name, anchor.Side)
	}
	if anchor.StartLine == 0 || anchor.EndLine < anchor.StartLine {
		return fmt.Errorf("%s has invalid line range", name)
	}
	return nil
}

func DigestAgentReviewRawCandidateClaim(claim AgentReviewRawCandidateClaim) (string, error) {
	data, err := json.Marshal(claim)
	if err != nil {
		return "", fmt.Errorf("marshal raw candidate claim: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
