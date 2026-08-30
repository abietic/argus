package v1alpha1

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAgentReviewWorkerKnowledgeBindsExactContentAndPlanIdentity(t *testing.T) {
	request := validAgentReviewWorkerRequest(t)
	content := []byte("# Repository invariant\n\nOwnership must survive every state transition.\n")
	digest := workerTestDigest(content)
	ref := VersionedRef{ID: "repository-invariants", Revision: "v1", SHA256: digest}
	transport, err := NewAgentReviewWorkerKnowledge(
		ref,
		ArtifactBinding{
			Ref: ContentRef{
				URI:    "artifact://test/knowledge/repository-invariants",
				SHA256: digest, SizeBytes: int64(len(content)),
			},
			Contract: AgentStagePlanKnowledgeContract,
		},
		content,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Plan.Knowledge = []VersionedRef{ref}
	request.KnowledgePacks = []AgentReviewWorkerKnowledge{transport}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	substituted := request
	substituted.KnowledgePacks = append([]AgentReviewWorkerKnowledge{}, request.KnowledgePacks...)
	substituted.KnowledgePacks[0].ContentBase64 = base64.StdEncoding.EncodeToString(
		[]byte("# Substituted\n\nDifferent business invariant.\n"),
	)
	if err := substituted.Validate(); err == nil ||
		(!strings.Contains(err.Error(), "size") && !strings.Contains(err.Error(), "SHA-256")) {
		t.Fatalf("Validate() accepted knowledge substitution: %v", err)
	}

	revisionDrift := request
	revisionDrift.KnowledgePacks = append([]AgentReviewWorkerKnowledge{}, request.KnowledgePacks...)
	revisionDrift.KnowledgePacks[0].Ref.Revision = "changed-v1"
	if err := revisionDrift.Validate(); err == nil ||
		!strings.Contains(err.Error(), "plan.knowledge") {
		t.Fatalf("Validate() accepted knowledge revision drift: %v", err)
	}

	missing := request
	missing.KnowledgePacks = []AgentReviewWorkerKnowledge{}
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "plan.knowledge") {
		t.Fatalf("Validate() accepted missing governed knowledge: %v", err)
	}
}
