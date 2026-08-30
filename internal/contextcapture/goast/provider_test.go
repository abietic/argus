package goast

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"argus.local/argus/internal/contextcapture"
	"argus.local/argus/internal/source/gitadapter"
)

const exactCommit = "1111111111111111111111111111111111111111"

type fakeSource struct {
	files         map[string]string
	reverse       bool
	commits       []string
	listingCommit string
}

func (source *fakeSource) ListFilesAtCommitWithAdmission(
	_ context.Context,
	_ string,
	commit string,
	_ gitadapter.ScopeAdmission,
	_ int,
) (gitadapter.RevisionFileList, error) {
	source.commits = append(source.commits, commit)
	paths := make([]string, 0, len(source.files))
	for path := range source.files {
		if strings.HasSuffix(path, ".go") || path == "go.mod" {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	if source.reverse {
		slices.Reverse(paths)
	}
	entries := make([]gitadapter.RevisionFileEntry, len(paths))
	for index, path := range paths {
		language := "go"
		if path == "go.mod" {
			language = "text"
		}
		entries[index] = gitadapter.RevisionFileEntry{
			Mode: "100644", Type: "blob", ObjectOID: exactCommit,
			Path: path, Language: language,
		}
	}
	if source.listingCommit != "" {
		commit = source.listingCommit
	}
	return gitadapter.RevisionFileList{
		SchemaVersion: gitadapter.RevisionFileListSchemaVersion,
		CommitOID:     commit, Include: []string{"**/*.go", "go.mod"}, Exclude: []string{},
		PolicyInclude: []string{"**"}, PolicyExclude: []string{}, Files: entries,
		Coverage: gitadapter.RevisionFileCoverage{
			ScannedFiles: len(entries), MatchedFiles: len(entries), RetainedFiles: len(entries),
		},
		Completeness: gitadapter.CompletenessComplete, Reasons: []gitadapter.Reason{},
	}, nil
}

func TestCaptureRejectsASTListingRevisionMismatchBeforeFileReads(t *testing.T) {
	observed := strings.Repeat("2", 40)
	source := &fakeSource{
		files: map[string]string{"target.go": "package target\n"}, listingCommit: observed,
	}
	provider, _ := New(source)
	_, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit, TargetPaths: []string{"target.go"},
	})
	var mismatch *contextcapture.RevisionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != exactCommit || mismatch.Observed != observed ||
		len(source.commits) != 1 {
		t.Fatalf("revision mismatch err=%v mismatch=%+v commits=%v", err, mismatch, source.commits)
	}
}

func (source *fakeSource) ReadFileAtCommit(
	_ context.Context,
	_ string,
	commit string,
	path string,
	_ int64,
) (gitadapter.FileContent, error) {
	source.commits = append(source.commits, commit)
	data, ok := source.files[path]
	if !ok {
		return gitadapter.FileContent{}, fmt.Errorf("missing %s", path)
	}
	digest := sha256.Sum256([]byte(data))
	return gitadapter.FileContent{
		SchemaVersion: gitadapter.FileContentSchemaVersion,
		CommitOID:     commit, Path: path, Mode: "100644", BlobOID: exactCommit,
		SHA256: fmt.Sprintf("%x", digest[:]), SizeBytes: int64(len(data)),
		Included: true, Completeness: gitadapter.CompletenessComplete,
		Reasons: []gitadapter.Reason{}, Data: []byte(data),
	}, nil
}

func TestCaptureProducesExactTypeAndCallerCalleeFactsDeterministically(t *testing.T) {
	files := map[string]string{
		"review/service.go": `package review
type Service struct{}
func validate(input string) bool { return input != "" }
func (Service) Review(input string) bool { return validate(input) }
`,
		"review/handler.go": `package review
func Handler(service Service, input string) bool { return service.Review(input) }
`,
	}
	firstSource := &fakeSource{files: files}
	firstProvider, err := New(firstSource)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit,
		TargetPaths: []string{"review/service.go"},
	}
	first, err := firstProvider.Capture(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	secondSource := &fakeSource{files: files, reverse: true}
	secondProvider, _ := New(secondSource)
	second, err := secondProvider.Capture(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Bytes) != string(second.Bytes) {
		t.Fatalf("capture changed with listing order\nfirst=%s\nsecond=%s", first.Bytes, second.Bytes)
	}
	for _, commit := range append(firstSource.commits, secondSource.commits...) {
		if commit != exactCommit {
			t.Fatalf("source read mutable revision %q", commit)
		}
	}
	if first.Artifact.CommitOID != exactCommit ||
		first.Artifact.Coverage.TargetFilesFound != 1 ||
		len(first.Coverage.Spans) != 1 ||
		first.Coverage.Spans[0].Path != "review/service.go" {
		t.Fatalf("capture identity/coverage = %+v / %+v", first.Artifact, first.Coverage)
	}
	assertCall(t, first.Artifact.Calls, "review/review.Service.Review", "review/review.validate", "within_target", "go_types_exact")
	assertCall(t, first.Artifact.Calls, "review/review.Handler", "review/review.Service.Review", "caller_of_target", "go_types_exact")
	var reviewType TypeFact
	for _, fact := range first.Artifact.Types {
		if fact.Symbol == "review/review.Service.Review" {
			reviewType = fact
		}
	}
	if reviewType.Resolution != "go_types" || !strings.Contains(reviewType.Type, "func(input string) bool") {
		t.Fatalf("review type fact = %+v", reviewType)
	}
}

func TestCaptureProducesBoundedExactUpstreamAndDownstreamCallPaths(t *testing.T) {
	source := &fakeSource{files: map[string]string{
		"target.go": `package sample
func Target() { First() }
`,
		"downstream.go": `package sample
func First() { Second() }
func Second() { Third() }
func Third() { Fourth() }
func Fourth() {}
`,
		"upstream.go": `package sample
func Entry() { Middle() }
func Middle() { Target() }
`,
	}}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit, TargetPaths: []string{"target.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCallPath(t, result.Artifact.CallPaths, "downstream", "sample.Target", []string{
		"sample.Target", "sample.First", "sample.Second", "sample.Third",
	})
	assertCallPath(t, result.Artifact.CallPaths, "upstream", "sample.Target", []string{
		"sample.Entry", "sample.Middle", "sample.Target",
	})
	for _, callPath := range result.Artifact.CallPaths {
		if len(callPath.Sites) > DefaultMaxCallPathDepth || slices.Contains(callPath.Symbols, "sample.Fourth") {
			t.Fatalf("unbounded call path = %+v", callPath)
		}
		for _, site := range callPath.Sites {
			if site.Line == 0 || site.Column == 0 {
				t.Fatalf("call path lost exact call site = %+v", callPath)
			}
		}
	}
	if result.Artifact.Coverage.CallPathsEmitted != len(result.Artifact.CallPaths) {
		t.Fatalf("call path coverage = %+v", result.Artifact.Coverage)
	}
	if _, err := DecodeArtifact(result.Bytes); err != nil {
		t.Fatalf("decode call path artifact: %v", err)
	}
	mutated := result.Artifact
	mutated.CallPaths = slices.Clone(result.Artifact.CallPaths)
	mutated.CallPaths[0].Sites = slices.Clone(mutated.CallPaths[0].Sites)
	mutated.CallPaths[0].Sites[0].Callee = mutated.CallPaths[0].Target
	data, err := json.Marshal(mutated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeArtifact(data); err == nil {
		t.Fatal("call path accepted a site that does not bind adjacent symbols")
	}
}

func TestCaptureResolvesExactCallPathsAcrossLocalModulePackages(t *testing.T) {
	provider, _ := New(&fakeSource{files: map[string]string{
		"go.mod": "module example.com/project\n\ngo 1.26\n",
		"internal/service/service.go": `package service
import "example.com/project/internal/helper"
func Target() { helper.Middle() }
`,
		"internal/helper/helper.go": `package helper
func Middle() { Leaf() }
func Leaf() {}
`,
		"cmd/app/main.go": `package main
import "example.com/project/internal/service"
func Entry() { Middle() }
func Middle() { service.Target() }
`,
	}})
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit,
		TargetPaths: []string{"internal/service/service.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifact.ModulePath != "example.com/project" ||
		result.Artifact.Coverage.TypeGroupsWithErrors != 0 {
		t.Fatalf("module type coverage = %+v gaps=%+v", result.Artifact.Coverage, result.Artifact.Gaps)
	}
	assertCallPath(t, result.Artifact.CallPaths, "downstream", "internal/service/service.Target", []string{
		"internal/service/service.Target", "internal/helper/helper.Middle", "internal/helper/helper.Leaf",
	})
	assertCallPath(t, result.Artifact.CallPaths, "upstream", "internal/service/service.Target", []string{
		"cmd/app/main.Entry", "cmd/app/main.Middle", "internal/service/service.Target",
	})
}

func TestCaptureDoesNotResolveExternalPackagesFromAmbientModuleCache(t *testing.T) {
	provider, _ := New(&fakeSource{files: map[string]string{
		"go.mod": "module example.com/project\n\ngo 1.26\nrequire github.com/google/uuid v1.6.0\n",
		"target.go": `package project
import "github.com/google/uuid"
func Target() uuid.UUID { return uuid.Nil }
`,
	}})
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit, TargetPaths: []string{"target.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifact.Coverage.TypeGroupsWithErrors != 1 ||
		!hasGap(result.Artifact.Gaps, "type_check_partial") {
		t.Fatalf("external package unexpectedly resolved from ambient state: coverage=%+v gaps=%+v",
			result.Artifact.Coverage, result.Artifact.Gaps)
	}
	if len(result.Artifact.CallPaths) != 0 {
		t.Fatalf("external package produced exact local paths: %+v", result.Artifact.CallPaths)
	}
}

func TestCaptureDistinguishesRepeatedCallsOnOneLineByColumn(t *testing.T) {
	provider, _ := New(&fakeSource{files: map[string]string{
		"target.go": "package sample\nfunc Target() { Helper(); Helper() }\n",
		"helper.go": "package sample\nfunc Helper() {}\n",
	}})
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit, TargetPaths: []string{"target.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifact.Calls) != 2 ||
		result.Artifact.Calls[0].Column == result.Artifact.Calls[1].Column {
		t.Fatalf("same-line calls = %+v", result.Artifact.Calls)
	}
}

func TestCaptureReportsCallPathSaturationWithoutBreakingClosure(t *testing.T) {
	const width = 48
	var target, graph strings.Builder
	target.WriteString("package sample\nfunc Target() {\n")
	graph.WriteString("package sample\n")
	for index := range width {
		fmt.Fprintf(&target, "Branch%d()\n", index)
	}
	target.WriteString("}\n")
	for branch := range width {
		fmt.Fprintf(&graph, "func Branch%d() {\n", branch)
		for leaf := range width {
			fmt.Fprintf(&graph, "Leaf%d()\n", leaf)
		}
		graph.WriteString("}\n")
	}
	for leaf := range width {
		fmt.Fprintf(&graph, "func Leaf%d() {}\n", leaf)
	}
	provider, _ := New(&fakeSource{files: map[string]string{
		"target.go": target.String(), "graph.go": graph.String(),
	}})
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit, TargetPaths: []string{"target.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Artifact.Coverage.Truncated || !hasGap(result.Artifact.Gaps, "call_path_limit_exceeded") ||
		len(result.Artifact.CallPaths) > DefaultMaxCallPaths {
		t.Fatalf("call path saturation = paths=%d coverage=%+v gaps=%+v",
			len(result.Artifact.CallPaths), result.Artifact.Coverage, result.Artifact.Gaps)
	}
	if _, err := DecodeArtifact(result.Bytes); err != nil {
		t.Fatalf("decode saturated call path artifact: %v", err)
	}
}

func TestCaptureRecordsParseAndTypeCoverageGaps(t *testing.T) {
	source := &fakeSource{files: map[string]string{
		"target.go": "package sample\nfunc Target() { Missing() }\n",
		"broken.go": "package sample\nfunc broken(\n",
	}}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit,
		TargetPaths: []string{"target.go", "deleted.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifact.Coverage.FilesFailed != 1 ||
		result.Artifact.Coverage.TypeGroupsWithErrors != 1 ||
		!hasGap(result.Artifact.Gaps, "parse_failed") ||
		!hasGap(result.Artifact.Gaps, "type_check_partial") ||
		!hasGap(result.Artifact.Gaps, "target_file_unavailable") {
		t.Fatalf("partial coverage = %+v gaps=%+v", result.Artifact.Coverage, result.Artifact.Gaps)
	}
}

func TestCaptureEnforcesArtifactBudgetWithoutTruncatedJSON(t *testing.T) {
	var sourceText strings.Builder
	sourceText.WriteString("package sample\n")
	for index := range 80 {
		fmt.Fprintf(&sourceText, "func Function%d() { Function0() }\n", index)
	}
	source := &fakeSource{files: map[string]string{"target.go": sourceText.String()}}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit,
		TargetPaths: []string{"target.go"}, MaxArtifactBytes: 8 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bytes) > 8<<10 || !result.Artifact.Coverage.Truncated ||
		!hasGap(result.Artifact.Gaps, "artifact_budget_exceeded") {
		t.Fatalf("bounded result size=%d coverage=%+v gaps=%+v", len(result.Bytes), result.Artifact.Coverage, result.Artifact.Gaps)
	}
}

func TestDecodeArtifactRejectsUnknownAndInconsistentFacts(t *testing.T) {
	source := &fakeSource{files: map[string]string{
		"target.go": "package sample\nfunc Target() {}\n",
	}}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: exactCommit,
		TargetPaths: []string{"target.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeArtifact(result.Bytes); err != nil {
		t.Fatalf("decode valid artifact: %v", err)
	}
	unknown := strings.TrimSuffix(string(result.Bytes), "}") + `,"unknown":true}`
	if _, err := DecodeArtifact([]byte(unknown)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	mutated := result.Artifact
	mutated.Coverage.SymbolsEmitted++
	data, err := json.Marshal(mutated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeArtifact(data); err == nil {
		t.Fatal("inconsistent coverage was accepted")
	}
}

func assertCall(t *testing.T, calls []Call, caller, callee, relation, resolution string) {
	t.Helper()
	for _, call := range calls {
		if call.Caller == caller && call.Callee == callee &&
			call.Relation == relation && call.Resolution == resolution {
			return
		}
	}
	t.Fatalf("call %s -> %s (%s/%s) not found in %+v", caller, callee, relation, resolution, calls)
}

func assertCallPath(t *testing.T, paths []CallPath, direction, target string, symbols []string) {
	t.Helper()
	for _, callPath := range paths {
		if callPath.Direction == direction && callPath.Target == target && slices.Equal(callPath.Symbols, symbols) {
			return
		}
	}
	t.Fatalf("call path %s %s %v not found in %+v", direction, target, symbols, paths)
}
