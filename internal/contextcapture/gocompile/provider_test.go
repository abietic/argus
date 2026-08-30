package gocompile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"argus.local/argus/internal/contextcapture"
	"argus.local/argus/internal/source/gitadapter"
)

const compileTestCommit = "1111111111111111111111111111111111111111"

type compileSource struct {
	order         []string
	files         map[string][]byte
	unavailable   map[string]gitadapter.ReasonCode
	completeness  gitadapter.Completeness
	commits       []string
	listingCommit string
}

func (source *compileSource) ListFilesAtCommitWithAdmission(
	_ context.Context,
	_ string,
	commit string,
	_ gitadapter.ScopeAdmission,
	_ int,
) (gitadapter.RevisionFileList, error) {
	source.commits = append(source.commits, commit)
	entries := make([]gitadapter.RevisionFileEntry, len(source.order))
	for index, file := range source.order {
		entries[index] = gitadapter.RevisionFileEntry{Path: file, Type: "blob", Mode: "100644"}
	}
	completeness := source.completeness
	if completeness == "" {
		completeness = gitadapter.CompletenessComplete
	}
	if source.listingCommit != "" {
		commit = source.listingCommit
	}
	return gitadapter.RevisionFileList{
		SchemaVersion: gitadapter.RevisionFileListSchemaVersion, CommitOID: commit,
		Include: []string{"**/*.go", "go.mod", "go.sum", "vendor/modules.txt"}, Exclude: []string{},
		PolicyInclude: []string{"**"}, PolicyExclude: []string{}, Files: entries,
		Coverage: gitadapter.RevisionFileCoverage{
			ScannedFiles: len(entries), MatchedFiles: len(entries), RetainedFiles: len(entries),
		},
		Completeness: completeness, Reasons: []gitadapter.Reason{},
	}, nil
}

func TestCaptureRejectsCompileListingRevisionMismatchBeforeMaterialization(t *testing.T) {
	observed := strings.Repeat("2", 40)
	source := &compileSource{
		order: []string{"go.mod"}, files: map[string][]byte{"go.mod": []byte("module example.com/project\n")},
		listingCommit: observed,
	}
	runner := &compileRunner{results: map[string]CompileResult{}}
	provider, _ := New(source, runner)
	_, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: compileTestCommit, TargetPaths: []string{"main.go"},
	})
	var mismatch *contextcapture.RevisionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != compileTestCommit || mismatch.Observed != observed ||
		len(source.commits) != 1 || len(runner.requests) != 0 {
		t.Fatalf("revision mismatch err=%v mismatch=%+v commits=%v runs=%v", err, mismatch, source.commits, runner.requests)
	}
}

func (source *compileSource) ReadFileAtCommit(
	_ context.Context,
	_ string,
	commit string,
	file string,
	_ int64,
) (gitadapter.FileContent, error) {
	source.commits = append(source.commits, commit)
	data, exists := source.files[file]
	if !exists {
		return gitadapter.FileContent{}, fmt.Errorf("missing fixture %s", file)
	}
	digest := sha256.Sum256(data)
	content := gitadapter.FileContent{
		SchemaVersion: gitadapter.FileContentSchemaVersion, CommitOID: commit,
		Path: file, Mode: "100644", BlobOID: strings.Repeat("a", 40),
		SHA256: fmt.Sprintf("%x", digest[:]), SizeBytes: int64(len(data)),
		Included: true, Completeness: gitadapter.CompletenessComplete,
		Reasons: []gitadapter.Reason{}, Data: bytes.Clone(data),
	}
	if reason := source.unavailable[file]; reason != "" {
		content.Included = false
		content.Completeness = gitadapter.CompletenessSkipped
		content.Reasons = []gitadapter.Reason{{Code: reason}}
		content.Data = nil
	}
	return content, nil
}

type compileRunner struct {
	results  map[string]CompileResult
	requests []CompileRequest
}

func (runner *compileRunner) Identity() Toolchain {
	return Toolchain{
		GoVersion: "go1.26.4", GOOS: "darwin", GOARCH: "arm64",
		ModuleNetwork: "disabled_goproxy_off", Authority: "local_host_unattested",
	}
}

func (runner *compileRunner) Compile(_ context.Context, request CompileRequest) (CompileResult, error) {
	runner.requests = append(runner.requests, request)
	info, err := os.Stat(filepath.Join(request.Workspace, "go.mod"))
	if err != nil {
		return CompileResult{}, err
	}
	if !info.Mode().IsRegular() {
		return CompileResult{}, fmt.Errorf("materialized go.mod is not regular")
	}
	return runner.results[request.Package], nil
}

func TestCaptureBindsExactCompileResultsAndIsDeterministic(t *testing.T) {
	files := map[string][]byte{
		"go.mod":      []byte("module example.com/project\n\ngo 1.26\n"),
		"a/a.go":      []byte("package a\nfunc A() {}\n"),
		"b/b.go":      []byte("package b\nfunc B() { missing() }\n"),
		"b/b_test.go": []byte("package b\nfunc helper() {}\n"),
	}
	request := Request{
		RepositoryRoot: "/repo", CommitOID: compileTestCommit,
		TargetPaths: []string{"b/b.go", "a/a.go"}, PolicyInclude: []string{"**"},
		PolicyExclude: []string{}, MaxFiles: 20, MaxFileBytes: 4096,
	}
	newCapture := func(order []string) (Result, *compileSource, *compileRunner) {
		source := &compileSource{order: order, files: files}
		runner := &compileRunner{results: map[string]CompileResult{
			"./a": {ExitCode: 0, Output: []byte("? example.com/project/a [no test files]\n")},
			"./b": {ExitCode: 1, Output: []byte("# example.com/project/b\nb/b.go:2:12: undefined: missing\n")},
		}}
		provider, err := New(source, runner)
		if err != nil {
			t.Fatal(err)
		}
		result, err := provider.Capture(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return result, source, runner
	}
	first, firstSource, firstRunner := newCapture([]string{"b/b_test.go", "go.mod", "a/a.go", "b/b.go"})
	second, secondSource, _ := newCapture([]string{"a/a.go", "b/b.go", "go.mod", "b/b_test.go"})
	if !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatalf("listing order changed compile artifact:\n%s\n%s", first.Bytes, second.Bytes)
	}
	for _, commit := range append(firstSource.commits, secondSource.commits...) {
		if commit != compileTestCommit {
			t.Fatalf("provider read mutable revision %q", commit)
		}
	}
	if len(firstRunner.requests) != 2 || firstRunner.requests[0].Package != "./a" ||
		firstRunner.requests[1].Package != "./b" || first.Artifact.Coverage.PackagesPassed != 1 ||
		first.Artifact.Coverage.PackagesFailed != 1 || first.Artifact.Coverage.DiagnosticsEmitted != 1 {
		t.Fatalf("compile execution/coverage = %+v / %+v", firstRunner.requests, first.Artifact.Coverage)
	}
	failed := first.Artifact.Packages[1]
	if failed.Status != "failed" || len(failed.Diagnostics) != 1 ||
		failed.Diagnostics[0].Path != "b/b.go" || failed.Diagnostics[0].Line != 2 ||
		!failed.CommandExecuted || failed.ExitCode != 1 || !failed.TestSourcesUsed {
		t.Fatalf("failed package evidence = %+v", failed)
	}
	if len(first.Coverage.Spans) != 2 ||
		!slices.Equal(first.Coverage.Symbols, []string{"go_compile:a:passed", "go_compile:b:failed"}) {
		t.Fatalf("context coverage = %+v", first.Coverage)
	}
}

func TestCaptureDoesNotCompileIncompleteExactInput(t *testing.T) {
	files := map[string][]byte{
		"go.mod":       []byte("module example.com/project\n"),
		"pkg/crlf.go":  []byte("package pkg\r\n"),
		"pkg/main.go":  []byte("package pkg\nfunc Value() {}\n"),
		"pkg/other.go": []byte("package pkg\n"),
	}
	source := &compileSource{
		order: []string{"go.mod", "pkg/crlf.go", "pkg/main.go", "pkg/other.go"}, files: files,
		unavailable: map[string]gitadapter.ReasonCode{
			"pkg/other.go": gitadapter.ReasonFileSizeExceeded,
			"pkg/crlf.go":  gitadapter.ReasonNonCanonicalLineEndings,
		},
	}
	runner := &compileRunner{results: map[string]CompileResult{}}
	provider, _ := New(source, runner)
	result, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: compileTestCommit,
		TargetPaths: []string{"pkg/main.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 0 || len(result.Artifact.Packages) != 1 ||
		result.Artifact.Packages[0].Status != "unavailable" ||
		!hasGap(result.Artifact.Gaps, "compile_input_incomplete") ||
		!hasGap(result.Artifact.Gaps, "file_non_canonical_line_endings") {
		t.Fatalf("incomplete input was compiled or hidden: requests=%+v artifact=%+v", runner.requests, result.Artifact)
	}
}

func TestCaptureClassifiesOfflineDependencyAsUnavailable(t *testing.T) {
	files := map[string][]byte{
		"go.mod":  []byte("module example.com/project\n"),
		"main.go": []byte("package project\n"),
	}
	source := &compileSource{order: []string{"go.mod", "main.go"}, files: files}
	runner := &compileRunner{results: map[string]CompileResult{
		".": {ExitCode: 1, Output: []byte("no required module provides package example.invalid/missing; module lookup disabled by GOPROXY=off\n")},
	}}
	provider, _ := New(source, runner)
	result, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: compileTestCommit, TargetPaths: []string{"main.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifact.Packages[0].Status != "unavailable" ||
		!hasGap(result.Artifact.Gaps, "dependency_unavailable") {
		t.Fatalf("offline dependency classification = %+v", result.Artifact)
	}
}

func TestCaptureRefusesInputsThatCouldEscapeCompileOnlyBoundary(t *testing.T) {
	tests := []struct {
		name  string
		files map[string][]byte
		order []string
		gap   string
	}{
		{
			name: "embed requires unmaterialized assets",
			files: map[string][]byte{
				"go.mod":  []byte("module example.com/project\n"),
				"main.go": []byte("package project\nimport _ \"embed\"\n//go:embed secret.txt\nvar data string\n"),
			},
			order: []string{"go.mod", "main.go"}, gap: "embedded_assets_not_materialized",
		},
		{
			name: "cgo toolchain execution is denied",
			files: map[string][]byte{
				"go.mod":  []byte("module example.com/project\n"),
				"main.go": []byte("package project\nimport \"C\"\n"),
			},
			order: []string{"go.mod", "main.go"}, gap: "cgo_not_supported",
		},
		{
			name: "local replace cannot read outside workspace",
			files: map[string][]byte{
				"go.mod":  []byte("module example.com/project\nreplace example.com/dep => ../ambient\n"),
				"main.go": []byte("package project\n"),
			},
			order: []string{"go.mod", "main.go"}, gap: "local_replace_not_supported",
		},
		{
			name: "precompiled objects are denied",
			files: map[string][]byte{
				"go.mod":  []byte("module example.com/project\n"),
				"main.go": []byte("package project\n"), "unsafe.syso": []byte("binary"),
			},
			order: []string{"go.mod", "main.go", "unsafe.syso"}, gap: "precompiled_object_not_supported",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &compileSource{order: test.order, files: test.files}
			runner := &compileRunner{results: map[string]CompileResult{}}
			provider, _ := New(source, runner)
			result, err := provider.Capture(t.Context(), Request{
				RepositoryRoot: "/repo", CommitOID: compileTestCommit,
				TargetPaths: []string{"main.go"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(runner.requests) != 0 || !hasGap(result.Artifact.Gaps, test.gap) ||
				result.Artifact.Packages[0].Status != "unavailable" {
				t.Fatalf("unsafe input was not failed closed: requests=%+v artifact=%+v", runner.requests, result.Artifact)
			}
		})
	}
}

func TestCaptureEnforcesArtifactBudgetWithoutInvalidJSON(t *testing.T) {
	files := map[string][]byte{
		"go.mod":  []byte("module example.com/project\n"),
		"main.go": []byte("package project\n"),
	}
	var output strings.Builder
	for index := range 200 {
		fmt.Fprintf(&output, "main.go:%d:1: compile diagnostic number %04d with bounded detail\n", index+1, index)
	}
	source := &compileSource{order: []string{"go.mod", "main.go"}, files: files}
	runner := &compileRunner{results: map[string]CompileResult{
		".": {ExitCode: 1, Output: []byte(output.String())},
	}}
	provider, _ := New(source, runner)
	result, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: compileTestCommit, TargetPaths: []string{"main.go"},
		MaxArtifactBytes: 2_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bytes) > 2_000 || !result.Artifact.Coverage.Truncated ||
		!hasGap(result.Artifact.Gaps, "artifact_budget_exceeded") {
		t.Fatalf("bounded artifact = bytes=%d coverage=%+v gaps=%+v", len(result.Bytes), result.Artifact.Coverage, result.Artifact.Gaps)
	}
	if _, err := DecodeArtifact(result.Bytes); err != nil {
		t.Fatalf("decode bounded artifact: %v", err)
	}
}

func TestDecodeArtifactRejectsUnknownAndInconsistentCoverage(t *testing.T) {
	artifact := validCompileArtifact()
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeArtifact(data); err != nil {
		t.Fatalf("decode valid artifact: %v", err)
	}
	unknown := append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeArtifact(unknown); err == nil {
		t.Fatal("unknown field was accepted")
	}
	artifact.Coverage.PackagesPassed++
	data, _ = json.Marshal(artifact)
	if _, err := DecodeArtifact(data); err == nil {
		t.Fatal("inconsistent coverage was accepted")
	}
}

func validCompileArtifact() Artifact {
	return Artifact{
		SchemaVersion: SchemaVersion, ProviderID: ProviderID, ProviderRevision: ProviderRevision,
		CommitOID: compileTestCommit, TargetPaths: []string{"main.go"},
		Toolchain: Toolchain{
			GoVersion: "go1.26.4", GOOS: "darwin", GOARCH: "arm64",
			ModuleNetwork: "disabled_goproxy_off", Authority: "local_host_unattested",
		},
		Packages: []PackageResult{{
			Package: ".", Status: "passed", Diagnostics: []Diagnostic{},
			OutputSHA256: emptySHA256(), OutputSizeBytes: 0,
			CommandExecuted: true, ExitCode: 0, TestSourcesUsed: true,
		}},
		Coverage: Coverage{
			FilesDiscovered: 2, FilesMaterialized: 2, TargetFilesRequested: 1,
			TargetGoFilesFound: 1, PackagesRequested: 1, PackagesPassed: 1,
		},
		Gaps: []Gap{},
	}
}
