package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentReviewPromptBundleRoundTripAndWorkerBinding(t *testing.T) {
	t.Parallel()
	data, err := MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := DecodeAgentReviewPromptBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	digest := workerTestDigest(data)
	transport, err := NewAgentReviewWorkerPromptBundle(
		VersionedRef{ID: "pi-review-prompts", Revision: bundle.Revision, SHA256: digest},
		ArtifactBinding{
			Ref: ContentRef{
				URI: "artifact://test/prompt-bundles/default", SHA256: digest,
				SizeBytes: int64(len(data)),
			},
			Contract: AgentStagePlanPromptContract,
		},
		data,
	)
	if err != nil {
		t.Fatal(err)
	}
	decoded, exact, err := DecodeAgentReviewWorkerPromptBundle(transport)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != bundle || string(exact) != string(data) {
		t.Fatalf("prompt bundle round trip drifted: %+v", decoded)
	}
}

func TestAgentReviewPromptBundleFailsClosed(t *testing.T) {
	t.Parallel()
	data, err := MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	unknown := make(map[string]any, len(raw)+1)
	for key, value := range raw {
		unknown[key] = value
	}
	unknown["ambient_override"] = true
	unknownData, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "unknown field", data: unknownData, want: "unknown field"},
		{name: "duplicate field", data: []byte(strings.Replace(string(data), `"revision":"v0"`, `"revision":"v0","revision":"v1"`, 1)), want: "duplicate"},
		{name: "explicit null", data: []byte(strings.Replace(string(data), `"revision":"v0"`, `"revision":null`, 1)), want: "null"},
		{name: "outer whitespace", data: []byte(strings.Replace(string(data), `"context_system_prompt":"`, `"context_system_prompt":" `, 1)), want: "bounded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeAgentReviewPromptBundle(test.data); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeAgentReviewPromptBundle() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentReviewWorkerPromptBundleRejectsSubstitutionAndRevisionDrift(t *testing.T) {
	t.Parallel()
	data, err := MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		t.Fatal(err)
	}
	digest := workerTestDigest(data)
	transport, err := NewAgentReviewWorkerPromptBundle(
		VersionedRef{ID: "pi-review-prompts", Revision: "v0", SHA256: digest},
		ArtifactBinding{
			Ref: ContentRef{
				URI: "artifact://test/prompt-bundles/default", SHA256: digest,
				SizeBytes: int64(len(data)),
			},
			Contract: AgentStagePlanPromptContract,
		},
		data,
	)
	if err != nil {
		t.Fatal(err)
	}

	substituted := transport
	substituted.ContentBase64 = transport.ContentBase64[:len(transport.ContentBase64)-4] + "AAAA"
	if err := substituted.Validate(); err == nil {
		t.Fatal("prompt bundle accepted substituted bytes")
	}
	revisionDrift := transport
	revisionDrift.Ref.Revision = "v1"
	if err := revisionDrift.Validate(); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("revision drift error = %v", err)
	}
}
