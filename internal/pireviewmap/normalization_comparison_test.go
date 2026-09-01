package pireviewmap

import (
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestBuildNormalizationPolicyComparisonPreservesPairedDiagnosticFacts(t *testing.T) {
	collection := comparisonRawCollection(t)
	comparison, err := BuildNormalizationPolicyComparison(
		"comparison-dev",
		[]NormalizationPolicyComparisonInput{{
			CaseID: "case-dev", SourceReviewRunRef: comparisonRef(runmodel.ContractReviewRun, "b"),
			RawCandidateCollectionRef: comparisonRef(runmodel.ContractAgentReviewRawCandidates, "c"),
			RawCandidates:             collection,
		}},
		time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Authority != "diagnostic_only" || comparison.Disposition != "comparison_only" ||
		comparison.BaselinePolicy != normalizationRecordedPolicy ||
		comparison.VariantPolicy != NormalizationPreviewPolicyRevision {
		t.Fatalf("comparison authority = %+v", comparison)
	}
	if comparison.Summary.BaselineRetained != 2 || comparison.Summary.VariantRetained != 1 ||
		comparison.Summary.RetainedDelta != -1 || comparison.Summary.ChangedDecisions != 1 ||
		len(comparison.Cases) != 1 || len(comparison.Cases[0].VariantClusters) != 1 {
		t.Fatalf("comparison summary = %+v", comparison)
	}
}

func TestBuildNormalizationPolicyComparisonRejectsDuplicateCaseIDs(t *testing.T) {
	collection := comparisonRawCollection(t)
	input := NormalizationPolicyComparisonInput{
		CaseID: "case-dev", SourceReviewRunRef: comparisonRef(runmodel.ContractReviewRun, "b"),
		RawCandidateCollectionRef: comparisonRef(runmodel.ContractAgentReviewRawCandidates, "c"),
		RawCandidates:             collection,
	}
	_, err := BuildNormalizationPolicyComparison(
		"comparison-dev", []NormalizationPolicyComparisonInput{input, input}, time.Now().UTC(),
	)
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate case error = %v", err)
	}
}

func comparisonRawCollection(t *testing.T) contractsv1alpha1.AgentReviewRawCandidateCollection {
	t.Helper()
	descriptions := []string{
		"applyDecision unconditionally assigns next to approval.state and actorId to approval.decidedBy with no guard for invalid transitions. A fixture already approved by alice is passed with rejected and bob and expects an exception. The function returns normally, changes the state, and replaces the original decider.",
		"applyDecision has no transition guard and assigns approval.state to next and approval.decidedBy to actorId. A fixture already approved by alice is passed with rejected and bob and expects an exception. The function returns normally, changes the state, and replaces the original decider.",
	}
	collection := contractsv1alpha1.AgentReviewRawCandidateCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        "plan-comparison", SourceRunID: "source-comparison", ExecutionID: "execution-comparison",
		ReviewRunID: "review-comparison", TargetDigest: strings.Repeat("a", 64),
		Authority:     contractsv1alpha1.AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition:   contractsv1alpha1.AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: []contractsv1alpha1.AgentReviewRawCandidatePayload{},
	}
	for index, description := range descriptions {
		claim := contractsv1alpha1.AgentReviewRawCandidateClaim{
			Category: "state transition", Severity: contractsv1alpha1.HypothesisSeverityHigh,
			Title: "applyDecision overwrites an existing terminal decision", Description: description,
			Impact: "The approval record is corrupted.",
			Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
				Path: "src/approval.ts", Side: contractsv1alpha1.HypothesisAnchorFile, StartLine: 14, EndLine: 15,
			},
			Evidence: []contractsv1alpha1.AgentReviewRawCandidateEvidence{{
				Statement: "Both fields are assigned without a guard.",
				Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
					Path: "src/approval.ts", Side: contractsv1alpha1.HypothesisAnchorFile, StartLine: 14, EndLine: 15,
				},
				Excerpt: "approval.state = next;\napproval.decidedBy = actorId;",
			}},
		}
		digest, err := contractsv1alpha1.DigestAgentReviewRawCandidateClaim(claim)
		if err != nil {
			t.Fatal(err)
		}
		collection.RawCandidates = append(collection.RawCandidates, contractsv1alpha1.AgentReviewRawCandidatePayload{
			RawCandidateID: []string{"raw-one", "raw-two"}[index], GroupID: "group-comparison",
			Dimension: contractsv1alpha1.VersionedRef{
				ID: []string{"correctness", "error-contract"}[index], Revision: "builtin-v1",
				SHA256: strings.Repeat([]string{"d", "e"}[index], 64),
			},
			Ordinal: uint32(index), ClaimDigest: digest,
			Action:     contractsv1alpha1.HypothesisNormalizationRetained,
			ReasonCode: "normalized_candidate_retained", Claim: claim,
		})
	}
	return collection
}

func comparisonRef(contract, fill string) runmodel.ArtifactRef {
	digest := strings.Repeat(fill, 64)
	return runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + digest, SHA256: digest, SizeBytes: 1, Contract: contract,
	}
}
