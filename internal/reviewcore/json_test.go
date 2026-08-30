package reviewcore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictJSONDecodersRejectUnknownDuplicateAndTrailingData(t *testing.T) {
	input := testInput("decode.go", "package sample", "// TODO decode")
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	unknown := []byte(strings.TrimSuffix(string(encoded), "}") + `,"unknown":true}`)
	if _, err := DecodeReviewInput(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	duplicate := []byte(strings.TrimSuffix(string(encoded), "}") + `,"target_id":"other"}`)
	if _, err := DecodeReviewInput(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate field error = %v", err)
	}
	if _, err := DecodeReviewInput(append(encoded, []byte(` {}`)...)); err == nil ||
		!strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing data error = %v", err)
	}
}

func TestStrictStageDecoderRoundTripAndNestedUnknownField(t *testing.T) {
	input := testInput("stage.go", "package sample", "// TODO stage")
	stage, err := Detect(context.Background(), input)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	encoded, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStageResult(encoded)
	if err != nil {
		t.Fatalf("DecodeStageResult() error = %v", err)
	}
	if decoded.ArtifactDigest != stage.ArtifactDigest {
		t.Fatal("stage digest changed after strict JSON round trip")
	}
	unknown := []byte(strings.Replace(
		string(encoded),
		`"output":{`,
		`"output":{"unknown":true,`,
		1,
	))
	if _, err := DecodeStageResult(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("nested unknown field error = %v", err)
	}
}

func TestReviewInputRejectsNonCanonicalManifestAndPatch(t *testing.T) {
	first := testInput("z.go", "package sample")
	secondContent := "package sample\n"
	first.Files = append(first.Files, FileManifestEntry{
		Path: "a.go", SHA256: digestString(secondContent),
		SizeBytes: int64(len(secondContent)), Content: &secondContent,
	})
	if err := first.Validate(); err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("unsorted manifest error = %v", err)
	}

	withCRLF := testInput("line.go", "package sample")
	withCRLF.CanonicalPatch = strings.ReplaceAll(withCRLF.CanonicalPatch, "\n", "\r\n")
	if err := withCRLF.Validate(); err == nil || !strings.Contains(err.Error(), "use LF") {
		t.Fatalf("CRLF patch error = %v", err)
	}
}

func TestReviewInputRejectsImplicitContextArray(t *testing.T) {
	input := testInput("context.go", "package sample", "// TODO context")
	input.Contexts = nil
	if err := input.Validate(); err == nil || !strings.Contains(err.Error(), "contexts must be an explicit array") {
		t.Fatalf("nil contexts error = %v", err)
	}
}

func TestReviewInputStrictlyDecodesContextRefAndRejectsTraversal(t *testing.T) {
	input := testInput("review.go", "package sample", "// TODO selected")
	contextDigest := digestString("context")
	input.Contexts = []ContextBinding{{
		Ref: &ContextRef{
			ContextID: "context-1", Kind: "repository_search", Revision: "commit-1",
			Digest: contextDigest,
			Coverage: ContextCoverage{
				Spans:   []ContextSpan{{Path: "other.go", StartLine: 1, EndLine: 2}},
				Symbols: []string{},
			},
			Provenance: ContextProvenance{
				Provider: "local-test", ProducerID: "fixture", ProducerRevision: "1",
			},
			ArtifactURI: "artifact://argus/contexts/1",
			Contract:    "argus.context.repository_search.v1alpha1",
			SizeBytes:   int64(len("context")),
		},
	}}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReviewInput(encoded); err != nil {
		t.Fatalf("DecodeReviewInput(context ref) error = %v", err)
	}
	unknown := []byte(strings.Replace(
		string(encoded),
		`"provenance":{`,
		`"provenance":{"unknown":true,`,
		1,
	))
	if _, err := DecodeReviewInput(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("nested context unknown-field error = %v", err)
	}
	input.Contexts[0].Ref.ArtifactURI = "artifact://argus/../secret"
	if err := input.Validate(); err == nil ||
		!strings.Contains(err.Error(), "traverse artifact namespaces") {
		t.Fatalf("context traversal error = %v", err)
	}
}
