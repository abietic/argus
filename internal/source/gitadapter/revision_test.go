package gitadapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureRevisionFreezesExactCommitAndDirtyState(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n")
	revision := commitAll(t, repository, "initial")
	writeFile(t, repository, "main.go", "package main\n\nconst Dirty = true\n")

	capture, err := newTestAdapter(t).CaptureRevision(context.Background(), RevisionRequest{
		RepositoryPath: repository,
		Revision:       "main",
	})
	if err != nil {
		t.Fatalf("CaptureRevision() error = %v", err)
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if capture.SchemaVersion != RevisionCaptureSchemaVersion ||
		capture.RepositoryRoot != canonicalRepository ||
		capture.Repository.Kind != "local_git" ||
		!strings.HasPrefix(capture.Repository.RepositoryID, "local-") ||
		capture.Repository.ObjectFormat != "sha1" {
		t.Fatalf("capture repository = %+v", capture)
	}
	if capture.Revision.Requested != "main" ||
		capture.Revision.CommitOID != revision ||
		capture.DirtyState != DirtyStateDirty {
		t.Fatalf("capture revision = %+v", capture)
	}
	if capture.Reasons == nil || !capture.CapturedAt.Equal(capture.CapturedAt.UTC()) ||
		capture.CapturedAt.IsZero() || capture.CapturedBy != capturedBy ||
		capture.GitVersion == "" {
		t.Fatalf("capture metadata = %+v", capture)
	}
}

func TestCaptureRevisionRejectsNonRootUnsafeAndMissingRevision(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "nested/main.go", "package nested\n")
	_ = commitAll(t, repository, "initial")
	adapter := newTestAdapter(t)

	tests := []struct {
		name    string
		request RevisionRequest
		code    ErrorCode
	}{
		{
			name: "relative repository",
			request: RevisionRequest{
				RepositoryPath: ".", Revision: "HEAD",
			},
			code: ErrorInvalidRequest,
		},
		{
			name: "work tree subdirectory",
			request: RevisionRequest{
				RepositoryPath: filepath.Join(repository, "nested"), Revision: "HEAD",
			},
			code: ErrorInvalidRequest,
		},
		{
			name: "unsafe revision",
			request: RevisionRequest{
				RepositoryPath: repository, Revision: "--all",
			},
			code: ErrorInvalidRequest,
		},
		{
			name: "missing revision",
			request: RevisionRequest{
				RepositoryPath: repository, Revision: "refs/heads/missing",
			},
			code: ErrorRevisionMissing,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := adapter.CaptureRevision(context.Background(), test.request)
			if code, ok := ErrorCodeOf(err); !ok || code != test.code {
				t.Fatalf("CaptureRevision() error = %v (%q/%v), want %q",
					err, code, ok, test.code)
			}
		})
	}
}

func TestRevalidateRevisionDistinguishesUnchangedMovedAndMissing(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n")
	first := commitAll(t, repository, "first")
	runTestGit(t, repository, "tag", "captured", first)
	adapter := newTestAdapter(t)

	unchanged, err := adapter.RevalidateRevision(
		context.Background(), repository, "main", first,
	)
	if err != nil {
		t.Fatalf("RevalidateRevision(unchanged) error = %v", err)
	}
	if unchanged.State != RevisionStateUnchanged ||
		unchanged.ActualCommitOID != first || len(unchanged.Reasons) != 0 {
		t.Fatalf("unchanged = %+v", unchanged)
	}

	writeFile(t, repository, "main.go", "package main\n\nconst Value = 2\n")
	second := commitAll(t, repository, "second")
	moved, err := adapter.RevalidateRevision(
		context.Background(), repository, "main", first,
	)
	if err != nil {
		t.Fatalf("RevalidateRevision(moved) error = %v", err)
	}
	if moved.State != RevisionStateMoved || moved.ActualCommitOID != second ||
		len(moved.Reasons) != 1 || moved.Reasons[0].Code != ReasonRevisionMoved {
		t.Fatalf("moved = %+v", moved)
	}

	runTestGit(t, repository, "tag", "-d", "captured")
	missing, err := adapter.RevalidateRevision(
		context.Background(), repository, "captured", first,
	)
	if err != nil {
		t.Fatalf("RevalidateRevision(missing) error = %v", err)
	}
	if missing.State != RevisionStateMissing || missing.ActualCommitOID != "" ||
		len(missing.Reasons) != 1 || missing.Reasons[0].Code != ReasonRevisionMissing {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestListFilesAtCommitMatchesGlobstarAndExcludeWins(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "root.go", "package root\n")
	writeFile(t, repository, "src/a.go", "package src\n")
	writeFile(t, repository, "src/deep/b.go", "package deep\n")
	writeFile(t, repository, "src/deep/README.md", "# deep\n")
	writeFile(t, repository, "vendor/ignored.go", "package ignored\n")
	revision := commitAll(t, repository, "tree")

	result, err := newTestAdapter(t).ListFilesAtCommit(
		context.Background(),
		repository,
		revision,
		[]string{"src/**/*.go"},
		[]string{"src/deep/**"},
		10,
	)
	if err != nil {
		t.Fatalf("ListFilesAtCommit() error = %v", err)
	}
	if result.Completeness != CompletenessComplete ||
		len(result.Files) != 1 ||
		result.Files[0].Path != "src/a.go" ||
		result.Files[0].Language != "go" ||
		result.Files[0].Mode != "100644" ||
		result.Files[0].Type != "blob" ||
		result.Files[0].ObjectOID == "" {
		t.Fatalf("result files = %+v", result)
	}
	if result.Coverage.ScannedFiles != 5 ||
		result.Coverage.MatchedFiles != 1 ||
		result.Coverage.RetainedFiles != 1 ||
		result.Coverage.SkippedFiles != 0 ||
		result.Coverage.ExcludedFiles != 1 {
		t.Fatalf("coverage = %+v", result.Coverage)
	}
}

func TestListFilesAtCommitCountsFullStreamBeyondRetentionLimit(t *testing.T) {
	repository := newTestRepository(t)
	for _, file := range []string{
		"a.go", "b.go", "nested/c.go", "nested/d.go", "nested/e.go",
	} {
		writeFile(t, repository, file, "package fixture\n")
	}
	revision := commitAll(t, repository, "many files")

	result, err := newTestAdapter(t).ListFilesAtCommit(
		context.Background(),
		repository,
		revision,
		nil,
		nil,
		2,
	)
	if err != nil {
		t.Fatalf("ListFilesAtCommit() error = %v", err)
	}
	if result.Completeness != CompletenessPartial ||
		len(result.Reasons) != 1 ||
		result.Reasons[0].Code != ReasonFileLimitExceeded ||
		len(result.Files) != 2 {
		t.Fatalf("partial result = %+v", result)
	}
	if result.Coverage.ScannedFiles != 5 ||
		result.Coverage.MatchedFiles != 5 ||
		result.Coverage.RetainedFiles != 2 ||
		result.Coverage.SkippedFiles != 3 {
		t.Fatalf("coverage = %+v", result.Coverage)
	}
	if result.Files[0].Path != "a.go" || result.Files[1].Path != "b.go" {
		t.Fatalf("retained files = %+v", result.Files)
	}
}

func TestListFilesAtCommitAppliesRequestAndPolicyBeforeRetentionLimit(t *testing.T) {
	repository := newTestRepository(t)
	for _, file := range []string{
		"review/a-request-denied/no.go",
		"review/b-policy-denied/no.go",
		"review/z-allowed/ok.go",
	} {
		writeFile(t, repository, file, "package fixture\n")
	}
	revision := commitAll(t, repository, "pre-quota admission")

	result, err := newTestAdapter(t).ListFilesAtCommitWithAdmission(
		context.Background(),
		repository,
		revision,
		ScopeAdmission{
			Include:       []string{"review/**"},
			Exclude:       []string{"review/a-request-denied/**"},
			PolicyInclude: []string{"review/**"},
			PolicyExclude: []string{"review/b-policy-denied/**"},
		},
		1,
	)
	if err != nil {
		t.Fatalf("ListFilesAtCommitWithAdmission() error = %v", err)
	}
	if result.Completeness != CompletenessComplete ||
		len(result.Files) != 1 ||
		result.Files[0].Path != "review/z-allowed/ok.go" {
		t.Fatalf("pre-quota result = %+v", result)
	}
	if result.Coverage.ScannedFiles != 3 ||
		result.Coverage.MatchedFiles != 1 ||
		result.Coverage.RetainedFiles != 1 ||
		result.Coverage.SkippedFiles != 0 ||
		result.Coverage.ExcludedFiles != 2 ||
		result.Coverage.RequestExcludedFiles != 1 ||
		result.Coverage.PolicyExcludedFiles != 1 {
		t.Fatalf("pre-quota coverage = %+v", result.Coverage)
	}
}

func TestListFilesAtCommitUsesExactTreeNotWorkingTree(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "committed.go", "package fixture\n")
	revision := commitAll(t, repository, "committed")
	writeFile(t, repository, "untracked.go", "package fixture\n")

	result, err := newTestAdapter(t).ListFilesAtCommit(
		context.Background(), repository, revision, []string{"**"}, nil, 10,
	)
	if err != nil {
		t.Fatalf("ListFilesAtCommit() error = %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "committed.go" {
		t.Fatalf("exact tree files = %+v", result.Files)
	}
}

func TestListFilesAtCommitNoMatchesIsExplicitlySkipped(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n")
	revision := commitAll(t, repository, "initial")

	result, err := newTestAdapter(t).ListFilesAtCommit(
		context.Background(), repository, revision, []string{"docs/**"}, nil, 10,
	)
	if err != nil {
		t.Fatalf("ListFilesAtCommit() error = %v", err)
	}
	if result.Completeness != CompletenessSkipped ||
		len(result.Reasons) != 1 ||
		result.Reasons[0].Code != ReasonNoMatchingFiles ||
		result.Coverage.ScannedFiles != 1 ||
		result.Coverage.MatchedFiles != 0 ||
		len(result.Files) != 0 {
		t.Fatalf("no-match result = %+v", result)
	}
}

func TestListFilesAtCommitRejectsUnsafeInputsAndTreePaths(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "safe.go", "package safe\n")
	revision := commitAll(t, repository, "safe")
	adapter := newTestAdapter(t)

	for _, pattern := range []string{
		"", "../**", "/absolute/**", `bad\path/**`, "bad:path/**",
		"a//b", "a/../b", "bad\npath", "src/***", "src/?.go",
		"src/[ab].go", strings.Repeat("a", MaxScopePatternBytes+1),
	} {
		t.Run("pattern_"+strings.ReplaceAll(pattern, "/", "_"), func(t *testing.T) {
			_, err := adapter.ListFilesAtCommit(
				context.Background(), repository, revision, []string{pattern}, nil, 10,
			)
			if code, ok := ErrorCodeOf(err); !ok || code != ErrorInvalidRequest {
				t.Fatalf("unsafe pattern %q error = %v (%q/%v)", pattern, err, code, ok)
			}
		})
	}
	if _, err := adapter.ListFilesAtCommit(
		context.Background(), repository, "HEAD", nil, nil, 10,
	); err == nil {
		t.Fatal("ListFilesAtCommit() accepted a symbolic commit")
	}
	if _, err := adapter.ListFilesAtCommit(
		context.Background(), repository, revision, nil, nil, -1,
	); err == nil {
		t.Fatal("ListFilesAtCommit() accepted a negative max_files")
	}

	writeFile(t, repository, "bad:name.go", "package bad\n")
	unsafeRevision := commitAll(t, repository, "unsafe tree path")
	_, err := adapter.ListFilesAtCommit(
		context.Background(), repository, unsafeRevision, nil, nil, 10,
	)
	if code, ok := ErrorCodeOf(err); !ok || code != ErrorGitCommandFailed {
		t.Fatalf("unsafe tree path error = %v (%q/%v), want git_command_failed", err, code, ok)
	}
}

func TestListFilesAtCommitIgnoresUnsafePathOutsideFinalAdmission(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "bad:name.go", "package bad\n")
	writeFile(t, repository, "src/safe.go", "package safe\n")
	revision := commitAll(t, repository, "unsafe path outside target")

	result, err := newTestAdapter(t).ListFilesAtCommitWithAdmission(
		context.Background(),
		repository,
		revision,
		ScopeAdmission{
			Include:       []string{"**"},
			Exclude:       []string{},
			PolicyInclude: []string{"src/**"},
			PolicyExclude: []string{},
		},
		1,
	)
	if err != nil {
		t.Fatalf("ListFilesAtCommitWithAdmission() error = %v", err)
	}
	if result.Completeness != CompletenessComplete ||
		len(result.Files) != 1 ||
		result.Files[0].Path != "src/safe.go" ||
		result.Coverage.PolicyExcludedFiles != 1 {
		t.Fatalf("unsafe-outside-target result = %+v", result)
	}
}

func TestRevisionOperationsHonorCancellation(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n")
	revision := commitAll(t, repository, "initial")
	adapter := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := adapter.CaptureRevision(ctx, RevisionRequest{
		RepositoryPath: repository, Revision: revision,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CaptureRevision(canceled) error = %v", err)
	}
	if _, err := adapter.ListFilesAtCommit(
		ctx, repository, revision, nil, nil, 10,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListFilesAtCommit(canceled) error = %v", err)
	}
}

func TestCompileScopePatternStarDoesNotCrossDirectory(t *testing.T) {
	expression, err := compileScopePattern("src/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !expression.MatchString("src/a.go") ||
		expression.MatchString("src/deep/a.go") {
		t.Fatalf("star expression %q has wrong directory semantics", expression)
	}
	globstar, err := compileScopePattern("src/**/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !globstar.MatchString("src/a.go") ||
		!globstar.MatchString("src/deep/a.go") ||
		globstar.MatchString("other/a.go") {
		t.Fatalf("globstar expression %q has wrong directory semantics", globstar)
	}
	unicodePattern, err := compileScopePattern("源码/**/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !unicodePattern.MatchString("源码/入口.go") ||
		!unicodePattern.MatchString("源码/内部/实现.go") {
		t.Fatalf("unicode expression %q has wrong semantics", unicodePattern)
	}
}

func TestRevisionFileCollectorParsesChunkedNULTerminatedStream(t *testing.T) {
	include, _, err := compileScopePatterns([]string{"**"}, true)
	if err != nil {
		t.Fatal(err)
	}
	exclude, _, err := compileScopePatterns(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	collector := newRevisionFileCollector(
		"sha1",
		include,
		exclude,
		include,
		exclude,
		10,
	)
	stream := "100644 blob " + strings.Repeat("a", 40) + "\t目录/main.go\x00" +
		"100755 blob " + strings.Repeat("b", 40) + "\tscript.sh\x00"
	for _, chunk := range []string{
		stream[:7],
		stream[7:53],
		stream[53 : len(stream)-3],
		stream[len(stream)-3:],
	} {
		if _, err := collector.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := collector.finish(); err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if collector.coverage.ScannedFiles != 2 ||
		collector.coverage.RetainedFiles != 2 ||
		collector.files[0].Path != "目录/main.go" ||
		collector.files[1].Mode != "100755" {
		t.Fatalf("collector = coverage %+v files %+v", collector.coverage, collector.files)
	}
}

func TestCaptureRevisionCanonicalizesSymlinkedRoot(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n")
	_ = commitAll(t, repository, "initial")
	link := filepath.Join(t.TempDir(), "repository-link")
	if err := os.Symlink(repository, link); err != nil {
		t.Fatal(err)
	}
	capture, err := newTestAdapter(t).CaptureRevision(context.Background(), RevisionRequest{
		RepositoryPath: link,
		Revision:       "HEAD",
	})
	if err != nil {
		t.Fatalf("CaptureRevision(symlink root) error = %v", err)
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if capture.RepositoryRoot != canonicalRepository {
		t.Fatalf("canonical root = %q, want %q", capture.RepositoryRoot, canonicalRepository)
	}
}
