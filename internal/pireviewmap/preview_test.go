package pireviewmap

import (
	"strings"
	"testing"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestPreviewNormalizationReclustersCommittedRawClaimsWithoutPromotion(t *testing.T) {
	claims := []struct {
		id, dimension, title, description string
	}{
		{
			id: "raw-a", dimension: "correctness",
			title:       "applyDecision accepts invalid transitions and overwrites decidedBy instead of rejecting them",
			description: "applyDecision unconditionally assigns next to approval.state and actorId to approval.decidedBy with no guard for invalid state transitions or already-decided records. A fixture with state approved and decidedBy alice is passed to applyDecision with rejected and bob inside assert.throws. This implementation does not throw and does not preserve the prior value, so the already-approved record is silently changed to rejected and the original decider is overwritten.",
		},
		{
			id: "raw-b", dimension: "error-contract",
			title:       "applyDecision silently applies an illegal rejection and mutates the approval instead of throwing",
			description: "applyDecision has no transition guard. For any supplied Approval and next state it sets approval.state to next, sets approval.decidedBy to actorId, and returns the same object. A test exercises applyDecision with rejected and bob where approval is already state approved and decidedBy alice, and expects this call to throw and leave decidedBy as alice. The implementation returns normally and overwrites both fields, so an already-decided approval is silently flipped to rejected and the original decider is lost.",
		},
		{
			id: "raw-c", dimension: "concurrency-data",
			title:       "applyDecision overwrites an existing decision without a transition guard or CAS",
			description: "applyDecision assigns approval.state to next and approval.decidedBy to actorId unconditionally and returns the same mutable object. There is no check on the current state and no compare-and-set or generation guard, so any caller can overwrite an earlier decision. After an approval is recorded with decidedBy alice, applyDecision with rejected and bob changes the state to rejected and replaces decidedBy with bob instead of rejecting the invalid transition. The test expects that exact call to throw and for decidedBy to remain alice, which the current implementation violates.",
		},
	}
	collection := contractsv1alpha1.AgentReviewRawCandidateCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        "plan-preview", SourceRunID: "run-preview", ExecutionID: "execution-preview",
		ReviewRunID: "review-preview", TargetDigest: strings.Repeat("a", 64),
		Authority:     contractsv1alpha1.AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition:   contractsv1alpha1.AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: []contractsv1alpha1.AgentReviewRawCandidatePayload{},
	}
	for index, item := range claims {
		claim := contractsv1alpha1.AgentReviewRawCandidateClaim{
			Category: item.dimension, Severity: contractsv1alpha1.HypothesisSeverityHigh,
			Title: item.title, Description: item.description, Impact: "The approval record is corrupted.",
			Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
				Path: "src/approval.ts", Side: contractsv1alpha1.HypothesisAnchorFile,
				StartLine: 14, EndLine: 16,
			},
			Evidence: []contractsv1alpha1.AgentReviewRawCandidateEvidence{{
				Statement: "The function mutates state and decidedBy without a guard.",
				Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
					Path: "src/approval.ts", Side: contractsv1alpha1.HypothesisAnchorFile,
					StartLine: 14, EndLine: 16,
				},
				Excerpt: "approval.state = next;\napproval.decidedBy = actorId;\nreturn approval;",
			}},
		}
		digest, err := contractsv1alpha1.DigestAgentReviewRawCandidateClaim(claim)
		if err != nil {
			t.Fatal(err)
		}
		collection.RawCandidates = append(collection.RawCandidates,
			contractsv1alpha1.AgentReviewRawCandidatePayload{
				RawCandidateID: item.id, GroupID: "group-preview",
				Dimension: contractsv1alpha1.VersionedRef{ID: item.dimension, Revision: "1", SHA256: strings.Repeat(string(rune('b'+index)), 64)},
				Ordinal:   uint32(index), ClaimDigest: digest,
				Action:     contractsv1alpha1.HypothesisNormalizationRetained,
				ReasonCode: "normalized_candidate_retained", Claim: claim,
			})
	}

	preview, err := PreviewNormalization(collection)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Authority != "diagnostic_only" || preview.Disposition != "preview_only" ||
		preview.PolicyRevision != NormalizationPreviewPolicyRevision ||
		preview.Summary.Retained != 1 || preview.Summary.MergedSemantic != 2 ||
		len(preview.Clusters) != 1 || len(preview.Clusters[0].RawCandidateIDs) != 3 {
		t.Fatalf("normalization preview = %+v", preview)
	}
	v0, err := PreviewNormalizationForRevision(collection, "argus-pi-review-workflow-v0")
	if err != nil {
		t.Fatal(err)
	}
	if v0.Summary.Retained != 3 || v0.Summary.MergedSemantic != 0 {
		t.Fatalf("v0 normalization preview = %+v", v0.Summary)
	}
	v1, err := PreviewNormalizationForRevision(collection, "argus-pi-review-workflow-v1")
	if err != nil {
		t.Fatal(err)
	}
	if v1.Summary.Retained != 3 || v1.Summary.MergedSemantic != 0 {
		t.Fatalf("v1 normalization preview = %+v", v1.Summary)
	}
	if _, err := PreviewNormalizationForRevision(collection, "argus-pi-review-workflow-v3"); err == nil {
		t.Fatal("preview admitted an unimplemented normalization revision")
	}
}

func TestSemanticDuplicatePolicyRevisionsAreDistinctAndClosed(t *testing.T) {
	anchor := piSourceAnchor{Path: "src/approval.ts", Side: "file", StartLine: 14, EndLine: 16}
	evidence := []piEvidence{{
		Statement: "The same changed statements are the root cause.", Anchor: anchor,
		Excerpt: "approval.state = next;",
	}}
	left := piCandidateClaim{
		Title:       "applyDecision applies invalid transition and overwrites approval state",
		Description: "A concrete description.", Anchor: anchor, Evidence: evidence,
	}
	right := piCandidate{
		PiCandidateClaim: piCandidateClaim{
			Title:       "applyDecision permits invalid transition and mutates state without guard",
			Description: "Another concrete description.", Anchor: anchor, Evidence: evidence,
		},
		GroupID: "group-1",
	}
	if piSemanticDuplicateForRevision(
		contractsv1alpha1.CandidateNormalizationRevisionV0,
		"group-1",
		left,
		right,
	) {
		t.Fatal("v0 admitted the v1 identifier/evidence extension")
	}
	for _, revision := range []string{
		contractsv1alpha1.CandidateNormalizationRevisionV1,
		contractsv1alpha1.CandidateNormalizationRevisionV2,
	} {
		if !piSemanticDuplicateForRevision(revision, "group-1", left, right) {
			t.Fatalf("%s did not admit its implemented semantic relation", revision)
		}
	}
}

func TestPreviewNormalizationKeepsPriorInvalidClaimIneligible(t *testing.T) {
	claim := contractsv1alpha1.AgentReviewRawCandidateClaim{
		Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
		Title: "Invalid worker claim", Description: "This claim was not admitted by the source host.",
		Impact:   "Unknown.",
		Anchor:   contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{Path: "src/x.ts", Side: contractsv1alpha1.HypothesisAnchorFile, StartLine: 1, EndLine: 1},
		Evidence: []contractsv1alpha1.AgentReviewRawCandidateEvidence{{Statement: "Untrusted.", Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{Path: "src/x.ts", Side: contractsv1alpha1.HypothesisAnchorFile, StartLine: 1, EndLine: 1}, Excerpt: "x"}},
	}
	digest, err := contractsv1alpha1.DigestAgentReviewRawCandidateClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	collection := contractsv1alpha1.AgentReviewRawCandidateCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        "plan-invalid", SourceRunID: "run-invalid", ExecutionID: "execution-invalid",
		ReviewRunID: "review-invalid", TargetDigest: strings.Repeat("a", 64),
		Authority:   contractsv1alpha1.AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition: contractsv1alpha1.AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: []contractsv1alpha1.AgentReviewRawCandidatePayload{{
			RawCandidateID: "raw-invalid", GroupID: "group-invalid",
			Dimension:   contractsv1alpha1.VersionedRef{ID: "correctness", Revision: "1", SHA256: strings.Repeat("b", 64)},
			ClaimDigest: digest, Action: contractsv1alpha1.HypothesisNormalizationRejectedInvalid,
			ReasonCode: "invalid_candidate", Claim: claim,
		}},
	}
	preview, err := PreviewNormalization(collection)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Ineligible != 1 || preview.Summary.Retained != 0 ||
		preview.Decisions[0].PreviewReasonCode != "prior_candidate_not_host_admitted" {
		t.Fatalf("invalid candidate preview = %+v", preview)
	}
}
