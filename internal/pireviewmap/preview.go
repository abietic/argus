package pireviewmap

import (
	"fmt"
	"slices"
	"strings"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const NormalizationPreviewPolicyRevision = "argus-pi-review-workflow-v2"

// NormalizationPreview is a read-only diagnostic projection over committed raw
// candidate facts. It does not create a ReviewRun, Hypothesis, Finding,
// Decision, Evaluation label, or replay claim.
type NormalizationPreview struct {
	SchemaVersion     string                         `json:"schema_version"`
	Authority         string                         `json:"authority"`
	Disposition       string                         `json:"disposition"`
	PolicyRevision    string                         `json:"policy_revision"`
	SourceReviewRunID string                         `json:"source_review_run_id"`
	TargetDigest      string                         `json:"target_digest"`
	Summary           NormalizationPreviewSummary    `json:"summary"`
	Decisions         []NormalizationPreviewDecision `json:"decisions"`
	Clusters          []NormalizationPreviewCluster  `json:"clusters"`
}

type NormalizationPreviewSummary struct {
	RawCandidates      int `json:"raw_candidates"`
	EligibleCandidates int `json:"eligible_candidates"`
	Retained           int `json:"retained"`
	MergedExact        int `json:"merged_exact"`
	MergedSemantic     int `json:"merged_semantic"`
	Ineligible         int `json:"ineligible"`
}

type NormalizationPreviewDecision struct {
	RawCandidateID          string                                          `json:"raw_candidate_id"`
	Dimension               contractsv1alpha1.VersionedRef                  `json:"dimension"`
	PriorAction             contractsv1alpha1.HypothesisNormalizationAction `json:"prior_action"`
	PriorReasonCode         string                                          `json:"prior_reason_code"`
	PreviewAction           string                                          `json:"preview_action"`
	PreviewReasonCode       string                                          `json:"preview_reason_code"`
	CanonicalRawCandidateID string                                          `json:"canonical_raw_candidate_id,omitempty"`
}

type NormalizationPreviewCluster struct {
	CanonicalRawCandidateID string   `json:"canonical_raw_candidate_id"`
	Fingerprint             string   `json:"fingerprint"`
	RawCandidateIDs         []string `json:"raw_candidate_ids"`
	DimensionIDs            []string `json:"dimension_ids"`
}

type normalizationPreviewCandidate struct {
	payload     contractsv1alpha1.AgentReviewRawCandidatePayload
	claim       piCandidateClaim
	fingerprint string
}

// PreviewNormalization recomputes the current deterministic normalization
// relation from an already validated committed raw-candidate collection. Only
// claims previously admitted as retained or merged are eligible; invalid and
// budget-excluded worker claims remain visible but are never promoted by this
// diagnostic path.
func PreviewNormalization(
	collection contractsv1alpha1.AgentReviewRawCandidateCollection,
) (NormalizationPreview, error) {
	return PreviewNormalizationForRevision(collection, NormalizationPreviewPolicyRevision)
}

// PreviewNormalizationForRevision recomputes one exact, implemented policy
// revision. It is used by offline evaluation and by activation gates; unknown
// or future revisions fail closed instead of silently falling back to latest.
func PreviewNormalizationForRevision(
	collection contractsv1alpha1.AgentReviewRawCandidateCollection,
	policyRevision string,
) (NormalizationPreview, error) {
	if err := collection.Validate(); err != nil {
		return NormalizationPreview{}, err
	}
	implementationRevision, err := normalizationImplementationRevision(policyRevision)
	if err != nil {
		return NormalizationPreview{}, err
	}
	preview := NormalizationPreview{
		SchemaVersion: "argus.normalization_preview.v1alpha1",
		Authority:     "diagnostic_only", Disposition: "preview_only",
		PolicyRevision:    policyRevision,
		SourceReviewRunID: collection.ReviewRunID, TargetDigest: collection.TargetDigest,
		Summary:   NormalizationPreviewSummary{RawCandidates: len(collection.RawCandidates)},
		Decisions: make([]NormalizationPreviewDecision, 0, len(collection.RawCandidates)),
		Clusters:  []NormalizationPreviewCluster{},
	}
	eligible := make([]normalizationPreviewCandidate, 0, len(collection.RawCandidates))
	ineligible := make([]NormalizationPreviewDecision, 0)
	for _, payload := range collection.RawCandidates {
		decision := NormalizationPreviewDecision{
			RawCandidateID: payload.RawCandidateID, Dimension: payload.Dimension,
			PriorAction: payload.Action, PriorReasonCode: payload.ReasonCode,
		}
		if payload.Action != contractsv1alpha1.HypothesisNormalizationRetained &&
			payload.Action != contractsv1alpha1.HypothesisNormalizationMergedDuplicate {
			decision.PreviewAction = "ineligible"
			decision.PreviewReasonCode = "prior_candidate_not_host_admitted"
			ineligible = append(ineligible, decision)
			continue
		}
		claim := previewPiClaim(payload.Claim)
		fingerprint, err := piClusterFingerprint(claim)
		if err != nil {
			return NormalizationPreview{}, fmt.Errorf("fingerprint raw candidate %q: %w", payload.RawCandidateID, err)
		}
		eligible = append(eligible, normalizationPreviewCandidate{
			payload: payload, claim: claim, fingerprint: fingerprint,
		})
	}
	preview.Summary.EligibleCandidates = len(eligible)
	preview.Summary.Ineligible = len(ineligible)
	slices.SortFunc(eligible, compareNormalizationPreviewCandidates)
	winners := make([]normalizationPreviewCandidate, 0, len(eligible))
	clusters := make(map[string]*NormalizationPreviewCluster)
	for _, entry := range eligible {
		var canonical *normalizationPreviewCandidate
		reason := ""
		for index := range winners {
			winner := &winners[index]
			if entry.fingerprint == winner.fingerprint {
				canonical, reason = winner, "duplicate_fingerprint"
				break
			}
			if piSemanticDuplicateForRevision(
				implementationRevision,
				entry.payload.GroupID,
				entry.claim,
				previewPiCandidate(*winner),
			) {
				canonical, reason = winner, "semantic_duplicate"
				break
			}
		}
		decision := NormalizationPreviewDecision{
			RawCandidateID: entry.payload.RawCandidateID, Dimension: entry.payload.Dimension,
			PriorAction: entry.payload.Action, PriorReasonCode: entry.payload.ReasonCode,
		}
		if canonical != nil {
			decision.PreviewAction = "merged_duplicate"
			decision.PreviewReasonCode = reason
			decision.CanonicalRawCandidateID = canonical.payload.RawCandidateID
			cluster := clusters[canonical.payload.RawCandidateID]
			cluster.RawCandidateIDs = append(cluster.RawCandidateIDs, entry.payload.RawCandidateID)
			cluster.DimensionIDs = append(cluster.DimensionIDs, entry.payload.Dimension.ID)
			if reason == "duplicate_fingerprint" {
				preview.Summary.MergedExact++
			} else {
				preview.Summary.MergedSemantic++
			}
		} else {
			winners = append(winners, entry)
			decision.PreviewAction = "retained"
			decision.PreviewReasonCode = "normalized_candidate_retained"
			clusters[entry.payload.RawCandidateID] = &NormalizationPreviewCluster{
				CanonicalRawCandidateID: entry.payload.RawCandidateID,
				Fingerprint:             entry.fingerprint,
				RawCandidateIDs:         []string{entry.payload.RawCandidateID},
				DimensionIDs:            []string{entry.payload.Dimension.ID},
			}
			preview.Summary.Retained++
		}
		preview.Decisions = append(preview.Decisions, decision)
	}
	preview.Decisions = append(preview.Decisions, ineligible...)
	slices.SortFunc(preview.Decisions, func(left, right NormalizationPreviewDecision) int {
		return strings.Compare(left.RawCandidateID, right.RawCandidateID)
	})
	for _, winner := range winners {
		cluster := clusters[winner.payload.RawCandidateID]
		slices.Sort(cluster.RawCandidateIDs)
		slices.Sort(cluster.DimensionIDs)
		cluster.DimensionIDs = slices.Compact(cluster.DimensionIDs)
		preview.Clusters = append(preview.Clusters, *cluster)
	}
	return preview, nil
}

func normalizationImplementationRevision(policyRevision string) (string, error) {
	switch policyRevision {
	case "argus-pi-review-workflow-v0":
		return contractsv1alpha1.CandidateNormalizationRevisionV0, nil
	case "argus-pi-review-workflow-v1":
		return contractsv1alpha1.CandidateNormalizationRevisionV1, nil
	case "argus-pi-review-workflow-v2":
		return contractsv1alpha1.CandidateNormalizationRevisionV2, nil
	default:
		return "", fmt.Errorf("unsupported normalization policy revision %q", policyRevision)
	}
}

func compareNormalizationPreviewCandidates(left, right normalizationPreviewCandidate) int {
	severityRank := map[contractsv1alpha1.HypothesisSeverity]int{
		contractsv1alpha1.HypothesisSeverityCritical: 0,
		contractsv1alpha1.HypothesisSeverityHigh:     1,
		contractsv1alpha1.HypothesisSeverityMedium:   2,
		contractsv1alpha1.HypothesisSeverityLow:      3,
	}
	if delta := severityRank[left.payload.Claim.Severity] - severityRank[right.payload.Claim.Severity]; delta != 0 {
		return delta
	}
	if order := strings.Compare(left.payload.Claim.Anchor.Path, right.payload.Claim.Anchor.Path); order != 0 {
		return order
	}
	if left.payload.Claim.Anchor.StartLine < right.payload.Claim.Anchor.StartLine {
		return -1
	}
	if left.payload.Claim.Anchor.StartLine > right.payload.Claim.Anchor.StartLine {
		return 1
	}
	if order := strings.Compare(left.payload.Claim.Title, right.payload.Claim.Title); order != 0 {
		return order
	}
	if order := strings.Compare(left.payload.Dimension.ID, right.payload.Dimension.ID); order != 0 {
		return order
	}
	return strings.Compare(left.payload.RawCandidateID, right.payload.RawCandidateID)
}

func previewPiCandidate(candidate normalizationPreviewCandidate) piCandidate {
	return piCandidate{PiCandidateClaim: candidate.claim, Fingerprint: candidate.fingerprint,
		GroupID: candidate.payload.GroupID,
		Skill:   piSkillRef{ID: candidate.payload.Dimension.ID, Revision: candidate.payload.Dimension.Revision}}
}

func previewPiClaim(claim contractsv1alpha1.AgentReviewRawCandidateClaim) piCandidateClaim {
	evidence := make([]piEvidence, 0, len(claim.Evidence))
	for _, item := range claim.Evidence {
		evidence = append(evidence, piEvidence{Statement: item.Statement,
			Anchor: previewPiAnchor(item.Anchor), Excerpt: item.Excerpt})
	}
	return piCandidateClaim{Category: claim.Category, Severity: string(claim.Severity),
		RawConfidencePPM: claim.RawConfidencePPM, Title: claim.Title,
		Description: claim.Description, Impact: claim.Impact,
		Anchor: previewPiAnchor(claim.Anchor), Evidence: evidence, Suggestion: claim.Suggestion}
}

func previewPiAnchor(anchor contractsv1alpha1.AgentReviewRawCandidateSourceAnchor) piSourceAnchor {
	return piSourceAnchor{Path: anchor.Path, Side: string(anchor.Side),
		StartLine: anchor.StartLine, EndLine: anchor.EndLine}
}
