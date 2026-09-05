package reviewjob

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/abietic/argus/internal/reviewcore"
)

func TestFormalRequestIsSourceOnlyAndProfileIsExplicit(t *testing.T) {
	valid := Request{
		SchemaVersion: RequestSchemaVersion, ExecutionProfile: FormalPiExecutionProfile,
		SourceRunID: "run-source", ExecutionTimeoutSeconds: 1800,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("formal request rejected: %v", err)
	}
	mixed := valid
	mixed.RepositoryPath = "/tmp/repository"
	if err := mixed.Validate(); err == nil {
		t.Fatal("formal request accepted a second target authority")
	}
	missingProfile := valid
	missingProfile.ExecutionProfile = ""
	if err := missingProfile.Validate(); err == nil {
		t.Fatal("review request accepted an implicit execution profile")
	}
	for _, request := range []Request{
		func() Request { value := valid; overlay := ""; value.OverlayContent = &overlay; return value }(),
		func() Request { value := valid; value.Contexts = []reviewcore.ContextBinding{}; return value }(),
	} {
		if err := request.Validate(); err == nil {
			t.Fatal("formal request accepted independent overlay or context input")
		}
	}
}

func TestSelectionRequestPreservesFrozenInputsWithoutAliasing(t *testing.T) {
	overlay := "package example\n// unsaved buffer\n"
	request := Request{
		SchemaVersion: RequestSchemaVersion, ExecutionProfile: DeterministicExecutionProfile,
		RepositoryPath: t.TempDir(), Mode: "selection", Revision: "HEAD",
		SelectionPath: "review.go", StartLine: 1, EndLine: 2,
		OverlayContent: &overlay, Contexts: []reviewcore.ContextBinding{testContextBinding()},
		ExecutionTimeoutSeconds: 1800,
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var restored Request
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request, restored) {
		t.Fatal("frozen request changed across JSON round trip")
	}
	applicationRequest := request.ApplicationRequest()
	if !reflect.DeepEqual(applicationRequest.Contexts, request.Contexts) ||
		*applicationRequest.OverlayContent != overlay {
		t.Fatal("application request lost frozen inputs")
	}
	*applicationRequest.OverlayContent = "changed"
	applicationRequest.Contexts[0].Gap.ContextID = "changed"
	applicationRequest.Contexts[0].Gap.Coverage.Spans[0].Path = "changed.go"
	applicationRequest.Contexts[0].Gap.Coverage.Symbols[0] = "Changed"
	if *request.OverlayContent != overlay || request.Contexts[0].Gap.ContextID != "context-1" ||
		request.Contexts[0].Gap.Coverage.Spans[0].Path != "caller.go" ||
		request.Contexts[0].Gap.Coverage.Symbols[0] != "Caller" {
		t.Fatal("application request aliases durable request data")
	}
	for _, invalid := range []string{"text\r\n", "text\x00", string([]byte{0xff})} {
		request.OverlayContent = &invalid
		if err := request.Validate(); err == nil {
			t.Fatal("invalid overlay accepted")
		}
	}
	request.OverlayContent = &overlay
	request.Contexts = append(request.Contexts, testContextBinding())
	if err := request.Validate(); err == nil {
		t.Fatal("duplicate context accepted")
	}
	request.Contexts = []reviewcore.ContextBinding{{}}
	if err := request.Validate(); err == nil {
		t.Fatal("empty context union accepted")
	}
}

func TestAbsentFrozenInputsPreserveLegacyJSON(t *testing.T) {
	request := testRequest(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("overlay_content")) || bytes.Contains(data, []byte("contexts")) {
		t.Fatal("new absent fields changed legacy command JSON")
	}
	request.OverlayContent = new(string)
	if err := request.Validate(); err == nil {
		t.Fatal("diff accepted selection overlay")
	}
}

func testContextBinding() reviewcore.ContextBinding {
	return reviewcore.ContextBinding{Gap: &reviewcore.ContextGap{
		ContextID: "context-1", Kind: "repository_search", Revision: "commit-1",
		Digest: strings.Repeat("a", 64),
		Coverage: reviewcore.ContextCoverage{
			Spans:   []reviewcore.ContextSpan{{Path: "caller.go", StartLine: 1, EndLine: 2}},
			Symbols: []string{"Caller"},
		},
		Provenance: reviewcore.ContextProvenance{Provider: "local", ProducerID: "fixture", ProducerRevision: "1"},
		ReasonCode: "unavailable",
	}}
}
