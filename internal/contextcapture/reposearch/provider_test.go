package reposearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"argus.local/argus/internal/contextcapture"
	"argus.local/argus/internal/source/gitadapter"
)

const repositorySearchTestCommit = "1111111111111111111111111111111111111111"

type repositorySearchSource struct {
	order         []string
	files         map[string][]byte
	unavailable   map[string]gitadapter.ReasonCode
	matchedFiles  int
	partial       bool
	reads         []string
	admission     gitadapter.ScopeAdmission
	listingCommit string
}

type concurrentRepositorySearchSource struct {
	*repositorySearchSource
	active    atomic.Int32
	maxActive atomic.Int32
}

func (source *concurrentRepositorySearchSource) ConcurrentReadLimit() int { return 4 }

func (source *concurrentRepositorySearchSource) ReadFileAtCommit(
	_ context.Context,
	_ string,
	commit string,
	file string,
	_ int64,
) (gitadapter.FileContent, error) {
	active := source.active.Add(1)
	defer source.active.Add(-1)
	for {
		observed := source.maxActive.Load()
		if active <= observed || source.maxActive.CompareAndSwap(observed, active) {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	data := source.files[file]
	return gitadapter.FileContent{
		SchemaVersion: gitadapter.FileContentSchemaVersion,
		CommitOID:     commit, Path: file, Mode: "100644", BlobOID: strings.Repeat("a", 40),
		SHA256: strings.Repeat("b", 64), SizeBytes: int64(len(data)), Included: true,
		Completeness: gitadapter.CompletenessComplete, Reasons: []gitadapter.Reason{}, Data: data,
	}, nil
}

func (source *repositorySearchSource) ListFilesAtCommitWithAdmission(
	_ context.Context,
	_ string,
	commit string,
	admission gitadapter.ScopeAdmission,
	_ int,
) (gitadapter.RevisionFileList, error) {
	source.admission = admission
	entries := make([]gitadapter.RevisionFileEntry, 0, len(source.order))
	for _, file := range source.order {
		entries = append(entries, gitadapter.RevisionFileEntry{Path: file, Type: "blob", Mode: "100644"})
	}
	matched := source.matchedFiles
	if matched == 0 {
		matched = len(entries)
	}
	completeness := gitadapter.CompletenessComplete
	if source.partial {
		completeness = gitadapter.CompletenessPartial
	}
	if source.listingCommit != "" {
		commit = source.listingCommit
	}
	return gitadapter.RevisionFileList{
		SchemaVersion: gitadapter.RevisionFileListSchemaVersion,
		CommitOID:     commit, Include: []string{"**"}, Exclude: []string{},
		PolicyInclude: slices.Clone(admission.PolicyInclude),
		PolicyExclude: slices.Clone(admission.PolicyExclude), Files: entries,
		Coverage: gitadapter.RevisionFileCoverage{
			ScannedFiles: matched, MatchedFiles: matched, RetainedFiles: len(entries),
			SkippedFiles: matched - len(entries),
		}, Completeness: completeness, Reasons: []gitadapter.Reason{},
	}, nil
}

func TestCaptureRejectsRepositoryListingRevisionMismatchBeforeBlobReads(t *testing.T) {
	observed := strings.Repeat("2", 40)
	source := &repositorySearchSource{
		order: []string{"target.go"}, files: map[string][]byte{"target.go": []byte("package target\n")},
		listingCommit: observed,
	}
	provider, _ := New(source)
	_, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: repositorySearchTestCommit,
		TargetPaths: []string{"target.go"},
	})
	var mismatch *contextcapture.RevisionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != repositorySearchTestCommit ||
		mismatch.Observed != observed || len(source.reads) != 0 {
		t.Fatalf("revision mismatch err=%v mismatch=%+v reads=%v", err, mismatch, source.reads)
	}
}

func (source *repositorySearchSource) ReadFileAtCommit(
	_ context.Context,
	_ string,
	commit string,
	file string,
	_ int64,
) (gitadapter.FileContent, error) {
	source.reads = append(source.reads, file)
	if reason := source.unavailable[file]; reason != "" {
		return gitadapter.FileContent{
			SchemaVersion: gitadapter.FileContentSchemaVersion,
			CommitOID:     commit, Path: file, Mode: "100644", BlobOID: strings.Repeat("a", 40),
			Included: false, Completeness: gitadapter.CompletenessSkipped,
			Reasons: []gitadapter.Reason{{Code: reason}},
		}, nil
	}
	data := source.files[file]
	return gitadapter.FileContent{
		SchemaVersion: gitadapter.FileContentSchemaVersion,
		CommitOID:     commit, Path: file, Mode: "100644", BlobOID: strings.Repeat("a", 40),
		SHA256: strings.Repeat("b", 64), SizeBytes: int64(len(data)), Included: true,
		Completeness: gitadapter.CompletenessComplete, Reasons: []gitadapter.Reason{}, Data: data,
	}, nil
}

func TestCaptureProducesDeterministicExactRepositoryMatches(t *testing.T) {
	files := map[string][]byte{
		"src/handler.go": []byte("package src\nfunc HandleOrder(request OrderRequest) error { return ValidateOrder(request) }\n"),
		"src/model.go":   []byte("package src\ntype OrderRequest struct { AccountID string }\n"),
		"src/service.go": []byte("package src\nfunc ValidateOrder(request OrderRequest) error { return nil }\n"),
		"docs/usage.md":  []byte("HandleOrder calls ValidateOrder with an OrderRequest.\n"),
	}
	firstSource := &repositorySearchSource{
		order: []string{"src/service.go", "src/handler.go", "docs/usage.md", "src/model.go"}, files: files,
	}
	secondSource := &repositorySearchSource{
		order: []string{"src/model.go", "docs/usage.md", "src/handler.go", "src/service.go"}, files: files,
	}
	request := Request{
		RepositoryRoot: "/repo", CommitOID: repositorySearchTestCommit,
		TargetPaths: []string{"src/handler.go"}, PolicyInclude: []string{"**"},
		PolicyExclude: []string{"vendor/**"}, MaxFiles: 20, MaxFileBytes: 4096,
		MaxArtifactBytes: DefaultMaxArtifactBytes,
	}
	firstProvider, _ := New(firstSource)
	secondProvider, _ := New(secondSource)
	first, err := firstProvider.Capture(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondProvider.Capture(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatalf("listing order changed artifact:\n%s\n%s", first.Bytes, second.Bytes)
	}
	if !slices.Equal(firstSource.admission.Include, []string{"**"}) ||
		!slices.Equal(firstSource.admission.PolicyExclude, []string{"vendor/**"}) {
		t.Fatalf("repository admission = %+v", firstSource.admission)
	}
	if first.Artifact.CommitOID != repositorySearchTestCommit ||
		first.Artifact.Coverage.TargetFilesFound != 1 || len(first.Artifact.Queries) != 3 {
		t.Fatalf("repository search artifact = %+v", first.Artifact)
	}
	terms := []string{}
	for _, query := range first.Artifact.Queries {
		terms = append(terms, query.Term)
		if !slices.Equal(query.SourcePaths, []string{"src/handler.go"}) || query.RepositoryOccurrences == 0 {
			t.Fatalf("query = %+v", query)
		}
	}
	if !slices.Equal(terms, []string{"HandleOrder", "OrderRequest", "ValidateOrder"}) {
		t.Fatalf("query terms = %v", terms)
	}
	for _, match := range first.Artifact.Matches {
		if match.Path == "src/handler.go" || match.LineSHA256 == "" || match.Line == 0 || match.Column == 0 {
			t.Fatalf("match = %+v", match)
		}
	}
	if len(first.Coverage.Spans) != 1 || first.Coverage.Spans[0].Path != "src/handler.go" ||
		!slices.Contains(first.Coverage.Symbols, "repository_search:ValidateOrder") {
		t.Fatalf("context coverage = %+v", first.Coverage)
	}
	decoded, err := DecodeArtifact(first.Bytes)
	if err != nil || decoded.CommitOID != repositorySearchTestCommit {
		t.Fatalf("DecodeArtifact() = %+v, %v", decoded, err)
	}
}

func TestCaptureSelectionRangesBoundQueryTermsToHalo(t *testing.T) {
	targetLines := make([]string, 100)
	for index := range targetLines {
		targetLines[index] = "// filler"
	}
	targetLines[0] = "func OutsideHeader() {}"
	targetLines[19] = "func InsideHalo() {}"
	targetLines[49] = "func SelectedToken() {}"
	targetLines[99] = "func OutsideTail() {}"
	files := map[string][]byte{
		"target.go":  []byte(strings.Join(targetLines, "\n") + "\n"),
		"callers.go": []byte("func Calls() { OutsideHeader(); InsideHalo(); SelectedToken(); OutsideTail() }\n"),
	}
	provider, _ := New(&repositorySearchSource{
		order: []string{"target.go", "callers.go"}, files: files,
	})
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: repositorySearchTestCommit,
		TargetPaths:   []string{"target.go"},
		TargetRanges:  []TargetRange{{Path: "target.go", StartLine: 50, EndLine: 50}},
		PolicyInclude: []string{"**"}, PolicyExclude: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	terms := make([]string, len(result.Artifact.Queries))
	for index, query := range result.Artifact.Queries {
		terms[index] = query.Term
	}
	if !slices.Contains(terms, "InsideHalo") || !slices.Contains(terms, "SelectedToken") ||
		slices.Contains(terms, "OutsideHeader") || slices.Contains(terms, "OutsideTail") {
		t.Fatalf("selection-bounded query terms = %v", terms)
	}
	if result.Artifact.QueryHaloLines != DefaultSelectionHaloLines ||
		!reflect.DeepEqual(result.Artifact.TargetRanges, []TargetRange{{Path: "target.go", StartLine: 50, EndLine: 50}}) ||
		len(result.Coverage.Spans) != 1 || result.Coverage.Spans[0].StartLine != 50 || result.Coverage.Spans[0].EndLine != 50 {
		t.Fatalf("selection provenance artifact=%+v coverage=%+v", result.Artifact, result.Coverage)
	}
	if _, err := DecodeArtifact(result.Bytes); err != nil {
		t.Fatalf("DecodeArtifact(selection) error = %v", err)
	}
}

func TestCaptureUsesOnlyExplicitlyAdvertisedBoundedConcurrency(t *testing.T) {
	files := map[string][]byte{"target.go": []byte("func ValidateOrder() {}\n")}
	order := []string{"target.go"}
	for index := range 8 {
		name := fmt.Sprintf("pkg/file-%d.go", index)
		order = append(order, name)
		files[name] = []byte("func Caller() { ValidateOrder() }\n")
	}
	source := &concurrentRepositorySearchSource{repositorySearchSource: &repositorySearchSource{
		order: order, files: files,
	}}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: repositorySearchTestCommit,
		TargetPaths: []string{"target.go"}, PolicyInclude: []string{"**"}, PolicyExclude: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.maxActive.Load() < 2 || result.Artifact.Coverage.FilesRead != len(order) {
		t.Fatalf("max concurrent reads=%d coverage=%+v", source.maxActive.Load(), result.Artifact.Coverage)
	}
}

func TestCaptureRejectsSensitivePathsBeforeBlobRead(t *testing.T) {
	source := &repositorySearchSource{
		order: []string{"src/handler.go", ".env", "secrets.yaml", ".env.example"},
		files: map[string][]byte{
			"src/handler.go": []byte("func ValidateOrder() {}\n"),
			".env":           []byte("ValidateOrder=secret\n"),
			"secrets.yaml":   []byte("ValidateOrder: secret\n"),
			".env.example":   []byte("ValidateOrder=placeholder\n"),
		},
	}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: repositorySearchTestCommit,
		TargetPaths: []string{"src/handler.go"}, PolicyInclude: []string{"**"}, PolicyExclude: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(source.reads, ".env") || slices.Contains(source.reads, "secrets.yaml") ||
		!slices.Contains(source.reads, ".env.example") {
		t.Fatalf("sensitive reads = %v", source.reads)
	}
	if result.Artifact.Coverage.FilesSensitiveSkipped != 2 ||
		!hasGap(result.Artifact.Gaps, "sensitive_environment_file") ||
		!hasGap(result.Artifact.Gaps, "sensitive_credential_file") {
		t.Fatalf("sensitive coverage/gaps = %+v %+v", result.Artifact.Coverage, result.Artifact.Gaps)
	}
	for _, match := range result.Artifact.Matches {
		if match.Path == ".env" || match.Path == "secrets.yaml" {
			t.Fatalf("sensitive match leaked: %+v", match)
		}
	}
}

func TestCaptureDeclaresListingMatchAndArtifactBudgets(t *testing.T) {
	largeLine := strings.Repeat("x", 600) + " ValidateOrder\n"
	files := map[string][]byte{"target.go": []byte("func ValidateOrder() {}\n")}
	order := []string{"target.go"}
	for index := range 20 {
		name := "pkg/file" + string(rune('a'+index)) + ".go"
		order = append(order, name)
		files[name] = []byte(largeLine)
	}
	source := &repositorySearchSource{order: order, files: files, matchedFiles: len(order) + 3, partial: true}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: repositorySearchTestCommit,
		TargetPaths: []string{"target.go"}, PolicyInclude: []string{"**"}, PolicyExclude: []string{},
		MaxFiles: len(order), MaxFileBytes: 4096, MaxArtifactBytes: 1_800,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bytes) > 1_800 || !result.Artifact.Coverage.Truncated ||
		!hasGap(result.Artifact.Gaps, "source_listing_partial") ||
		!hasGap(result.Artifact.Gaps, "match_budget_exceeded") ||
		!hasGap(result.Artifact.Gaps, "artifact_budget_exceeded") {
		t.Fatalf("bounded artifact = coverage=%+v gaps=%+v bytes=%d", result.Artifact.Coverage, result.Artifact.Gaps, len(result.Bytes))
	}
	for _, match := range result.Artifact.Matches {
		if match.LineTextTruncated && (!strings.Contains(match.LineText, match.Term) || match.LineTextStart <= 1) {
			t.Fatalf("truncated match lost its term window: %+v", match)
		}
	}
	if _, err := DecodeArtifact(result.Bytes); err != nil {
		t.Fatalf("bounded artifact no longer strict: %v", err)
	}
}

func TestCapturePreservesUnavailableTargetAndNoMatchGaps(t *testing.T) {
	source := &repositorySearchSource{
		order: []string{"target.go", "other.go", "crlf.go"},
		files: map[string][]byte{
			"target.go": []byte("func ValidateOrder() {}\n"),
			"other.go":  []byte("package other\n"),
			"crlf.go":   []byte("package crlf\r\n"),
		},
		unavailable: map[string]gitadapter.ReasonCode{
			"target.go": gitadapter.ReasonFileSizeExceeded,
			"crlf.go":   gitadapter.ReasonNonCanonicalLineEndings,
		},
	}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: repositorySearchTestCommit,
		TargetPaths: []string{"target.go"}, PolicyInclude: []string{"**"}, PolicyExclude: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifact.Coverage.TargetFilesFound != 0 ||
		!hasGap(result.Artifact.Gaps, "file_file_size_exceeded") ||
		!hasGap(result.Artifact.Gaps, "file_non_canonical_line_endings") ||
		!hasGap(result.Artifact.Gaps, "target_file_unavailable") ||
		!hasGap(result.Artifact.Gaps, "target_no_search_terms") {
		t.Fatalf("unavailable target = %+v %+v", result.Artifact.Coverage, result.Artifact.Gaps)
	}
}

func TestCaptureHonorsCancellationAndDecodeIsStrict(t *testing.T) {
	provider, _ := New(&repositorySearchSource{order: []string{}, files: map[string][]byte{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Capture(ctx, Request{}); err == nil {
		t.Fatal("Capture accepted canceled context")
	}
	artifact := Artifact{
		SchemaVersion: SchemaVersion, ProviderID: ProviderID, ProviderRevision: ProviderRevision,
		CommitOID: repositorySearchTestCommit, TargetPaths: []string{}, TargetRanges: []TargetRange{},
		QueryHaloLines: 0, Queries: []Query{}, Matches: []Match{},
		Coverage: Coverage{}, Gaps: []Gap{{Code: "target_no_search_terms"}},
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeArtifact(data); err == nil {
		t.Fatal("DecodeArtifact accepted an unknown field")
	}
}
