package v1alpha1

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAgentReviewWorkerContextArtifactBindsExactContent(t *testing.T) {
	request := validAgentReviewWorkerRequest(t)
	content := []byte(`{"calls":[{"caller":"Serve","callee":"Handle"}]}`)
	digest := workerTestDigest(content)
	transport, err := NewAgentReviewWorkerContextArtifact(
		"codegraph-change-context",
		"codegraph",
		"commit-a",
		ArtifactBinding{
			Ref: ContentRef{
				URI:    "artifact://test/contexts/codegraph-change-context",
				SHA256: digest, SizeBytes: int64(len(content)),
			},
			Contract: "argus.context.codegraph.v1alpha1",
		},
		content,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.ContextArtifacts = []AgentReviewWorkerContextArtifact{transport}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	substituted := request
	substituted.ContextArtifacts = append(
		[]AgentReviewWorkerContextArtifact{}, request.ContextArtifacts...,
	)
	substituted.ContextArtifacts[0].ContentBase64 = base64.StdEncoding.EncodeToString(
		[]byte("substituted context"),
	)
	if err := substituted.Validate(); err == nil ||
		(!strings.Contains(err.Error(), "size") && !strings.Contains(err.Error(), "SHA-256")) {
		t.Fatalf("Validate() accepted context substitution: %v", err)
	}
}
