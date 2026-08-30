package gitadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaterializeDiffBuildsExactSnapshotAndCanonicalPatch(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n\nfunc value() int { return 1 }\n")
	base := commitAll(t, repository, "base")

	writeFile(t, repository, "main.go", "package main\n\nfunc value() int {\n\treturn 2\n}\n")
	writeFile(t, repository, "README.md", "# fixture\n")
	head := commitAll(t, repository, "head")

	adapter := newTestAdapter(t)
	capturedAt := time.Date(2026, 7, 26, 12, 0, 0, 123, time.UTC)
	adapter.now = func() time.Time { return capturedAt }
	request := Request{
		RepositoryPath: repository,
		RepositoryID:   "fixture-repository",
		BaseRevision:   base,
		HeadRevision:   head,
	}

	result, err := adapter.MaterializeDiff(context.Background(), request)
	if err != nil {
		t.Fatalf("MaterializeDiff() error = %v", err)
	}
	if result.Completeness != CompletenessComplete {
		t.Fatalf("completeness = %q, reasons = %+v", result.Completeness, result.Reasons)
	}
	if result.Snapshot == nil || result.Manifest == nil {
		t.Fatal("MaterializeDiff() did not return snapshot and manifest")
	}
	if result.Snapshot.Base.CommitOID != base || result.Snapshot.Head.CommitOID != head {
		t.Fatalf(
			"resolved revisions = %s..%s, want %s..%s",
			result.Snapshot.Base.CommitOID,
			result.Snapshot.Head.CommitOID,
			base,
			head,
		)
	}
	if result.Snapshot.DirtyState != DirtyStateClean {
		t.Fatalf("dirty_state = %q, want clean", result.Snapshot.DirtyState)
	}
	if result.Manifest.Coverage.TotalFiles != 2 ||
		result.Manifest.Coverage.IncludedFiles != 2 ||
		result.Manifest.Coverage.SkippedFiles != 0 {
		t.Fatalf("file coverage = %+v, want two included files", result.Manifest.Coverage)
	}
	if result.Manifest.Coverage.TotalHunks < 2 ||
		result.Manifest.Coverage.TotalHunks != result.Manifest.Coverage.IncludedHunks {
		t.Fatalf("hunk coverage = %+v, want all hunks included", result.Manifest.Coverage)
	}

	mainChange := requireFileChange(t, result.Manifest.Files, "main.go")
	if mainChange.Status != ChangeModified || mainChange.Language != "go" || mainChange.HunkCount == 0 {
		t.Fatalf("main.go change = %+v", mainChange)
	}
	readmeChange := requireFileChange(t, result.Manifest.Files, "README.md")
	if readmeChange.Status != ChangeAdded || readmeChange.Language != "markdown" ||
		readmeChange.HunkCount == 0 {
		t.Fatalf("README.md change = %+v", readmeChange)
	}
	if !result.Patch.Included || len(result.Patch.Data) == 0 {
		t.Fatal("canonical patch was not included")
	}
	if !bytes.Contains(result.Patch.Data, []byte("diff --git a/main.go b/main.go")) ||
		!bytes.Contains(result.Patch.Data, []byte("+\treturn 2")) {
		t.Fatalf("canonical patch does not contain expected change:\n%s", result.Patch.Data)
	}
	patchDigest := sha256.Sum256(result.Patch.Data)
	if result.Patch.SHA256 != fmt.Sprintf("%x", patchDigest) ||
		result.Patch.SizeBytes != int64(len(result.Patch.Data)) {
		t.Fatalf("patch identity = %s/%d, want exact bytes", result.Patch.SHA256, result.Patch.SizeBytes)
	}
	manifestDigest, err := DigestManifest(*result.Manifest)
	if err != nil {
		t.Fatalf("DigestManifest() error = %v", err)
	}
	if result.Snapshot.ManifestSHA256 != manifestDigest {
		t.Fatalf("manifest digest = %s, want %s", result.Snapshot.ManifestSHA256, manifestDigest)
	}
	snapshotDigest, err := DigestTargetSnapshot(*result.Snapshot)
	if err != nil {
		t.Fatalf("DigestTargetSnapshot() error = %v", err)
	}
	if result.Snapshot.SHA256 != snapshotDigest {
		t.Fatalf("snapshot digest = %s, want %s", result.Snapshot.SHA256, snapshotDigest)
	}

	second, err := adapter.MaterializeDiff(context.Background(), request)
	if err != nil {
		t.Fatalf("second MaterializeDiff() error = %v", err)
	}
	if !bytes.Equal(result.Patch.Data, second.Patch.Data) ||
		result.Patch.SHA256 != second.Patch.SHA256 ||
		result.Snapshot.TargetSnapshotID != second.Snapshot.TargetSnapshotID ||
		result.Snapshot.SHA256 != second.Snapshot.SHA256 {
		t.Fatal("same exact target did not produce stable patch and snapshot identities")
	}
}

func TestMaterializeDiffIgnoresMutableInfoAttributes(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "review.go", "package fixture\n")
	base := commitAll(t, repository, "base")
	writeFile(t, repository, "review.go", "package fixture\n// ARGUS_BUG\n")
	head := commitAll(t, repository, "head")
	adapter := newTestAdapter(t)
	request := Request{
		RepositoryPath: repository,
		BaseRevision:   base,
		HeadRevision:   head,
	}
	before, err := adapter.MaterializeDiff(context.Background(), request)
	if err != nil {
		t.Fatalf("MaterializeDiff(before attributes) error = %v", err)
	}
	attributes := filepath.Join(repository, ".git", "info", "attributes")
	if err := os.WriteFile(attributes, []byte("review.go -diff\n"), 0o600); err != nil {
		t.Fatalf("write info/attributes: %v", err)
	}
	after, err := adapter.MaterializeDiff(context.Background(), request)
	if err != nil {
		t.Fatalf("MaterializeDiff(after attributes) error = %v", err)
	}
	if before.Patch.SHA256 != after.Patch.SHA256 ||
		!bytes.Equal(before.Patch.Data, after.Patch.Data) ||
		before.Manifest.PatchSHA256 != after.Manifest.PatchSHA256 {
		t.Fatal("mutable .git/info/attributes changed an exact-commit capture")
	}
	if !bytes.Contains(after.Patch.Data, []byte("+// ARGUS_BUG")) {
		t.Fatalf("isolated patch lost text hunk:\n%s", after.Patch.Data)
	}
}

func TestMaterializeDiffSameRevisionIsExplicitlySkipped(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n")
	revision := commitAll(t, repository, "initial")

	result, err := newTestAdapter(t).MaterializeDiff(context.Background(), Request{
		RepositoryPath: repository,
		BaseRevision:   revision,
		HeadRevision:   revision,
	})
	if err != nil {
		t.Fatalf("MaterializeDiff() error = %v", err)
	}
	if result.Completeness != CompletenessSkipped || !result.HasReason(ReasonNoChanges) {
		t.Fatalf("result = %+v, want explicit no_changes skip", result)
	}
	if result.Manifest == nil || result.Manifest.Coverage.TotalFiles != 0 {
		t.Fatalf("manifest = %+v, want empty file coverage", result.Manifest)
	}
	if !result.Patch.Included || len(result.Patch.Data) != 0 ||
		result.Patch.SHA256 != sha256Hex(nil) {
		t.Fatalf("empty patch = %+v, want included exact empty artifact", result.Patch)
	}
}

func TestMaterializeDiffClassifiesMissingRevision(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "main.go", "package main\n")
	base := commitAll(t, repository, "initial")

	result, err := newTestAdapter(t).MaterializeDiff(context.Background(), Request{
		RepositoryPath: repository,
		BaseRevision:   base,
		HeadRevision:   "refs/heads/does-not-exist",
	})
	if err == nil {
		t.Fatal("MaterializeDiff() accepted a missing head revision")
	}
	code, ok := ErrorCodeOf(err)
	if !ok || code != ErrorHeadRevisionMissing {
		t.Fatalf("error code = %q/%v, want %q: %v", code, ok, ErrorHeadRevisionMissing, err)
	}
	if result.Completeness != CompletenessSkipped || !result.HasReason(ReasonHeadRevisionMissing) {
		t.Fatalf("result = %+v, want explicit missing-revision skip", result)
	}
	if result.Snapshot != nil || result.Manifest != nil {
		t.Fatal("missing revision unexpectedly produced a target snapshot")
	}
}

func TestMaterializeDiffRejectsNonRepositoryAndUnsafeInputs(t *testing.T) {
	adapter := newTestAdapter(t)
	nonRepository := t.TempDir()
	result, err := adapter.MaterializeDiff(context.Background(), Request{
		RepositoryPath: nonRepository,
		BaseRevision:   "HEAD~1",
		HeadRevision:   "HEAD",
	})
	if code, ok := ErrorCodeOf(err); !ok || code != ErrorRepositoryNotGit {
		t.Fatalf("non-repository error = %v (%q/%v), want repository_not_git", err, code, ok)
	}
	if !result.HasReason(ReasonRepositoryNotGit) {
		t.Fatalf("non-repository result = %+v", result)
	}

	repository := newTestRepository(t)
	result, err = adapter.MaterializeDiff(context.Background(), Request{
		RepositoryPath: repository,
		BaseRevision:   "--no-index",
		HeadRevision:   "HEAD",
	})
	if code, ok := ErrorCodeOf(err); !ok || code != ErrorInvalidRequest {
		t.Fatalf("unsafe revision error = %v (%q/%v), want invalid_request", err, code, ok)
	}
	if !result.HasReason(ReasonInvalidRequest) {
		t.Fatalf("unsafe revision result = %+v", result)
	}

	result, err = adapter.MaterializeDiff(context.Background(), Request{
		RepositoryPath: ".",
		BaseRevision:   "HEAD~1",
		HeadRevision:   "HEAD",
	})
	if code, ok := ErrorCodeOf(err); !ok || code != ErrorInvalidRequest {
		t.Fatalf("relative path error = %v (%q/%v), want invalid_request", err, code, ok)
	}
	if !result.HasReason(ReasonInvalidRequest) {
		t.Fatalf("relative path result = %+v", result)
	}
}

func TestMaterializeDiffStopsOversizePatchAtHardBoundary(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "payload.txt", "small\n")
	base := commitAll(t, repository, "base")
	writeFile(t, repository, "payload.txt", strings.Repeat("a long changed line\n", 128))
	head := commitAll(t, repository, "head")

	adapter := newTestAdapter(t)
	result, err := adapter.MaterializeDiff(context.Background(), Request{
		RepositoryPath: repository,
		BaseRevision:   base,
		HeadRevision:   head,
		Limits:         Limits{MaxPatchBytes: 32},
	})
	if code, ok := ErrorCodeOf(err); !ok || code != ErrorBudgetExceeded {
		t.Fatalf(
			"MaterializeDiff() error = %v (%q/%v), want budget_exceeded",
			err,
			code,
			ok,
		)
	}
	if !result.HasReason(ReasonPatchSizeExceeded) ||
		result.Snapshot != nil ||
		result.Manifest != nil ||
		result.Patch.Data != nil {
		t.Fatalf("oversize result = %+v, want bounded failure without partial artifacts", result)
	}
}

func TestMaterializeDiffParsesRename(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "old.go", "package fixture\n\nconst Value = 1\n")
	base := commitAll(t, repository, "base")
	if err := os.Rename(
		filepath.Join(repository, "old.go"),
		filepath.Join(repository, "new.go"),
	); err != nil {
		t.Fatalf("rename fixture file: %v", err)
	}
	head := commitAll(t, repository, "rename")

	result, err := newTestAdapter(t).MaterializeDiff(context.Background(), Request{
		RepositoryPath: repository,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("MaterializeDiff() error = %v", err)
	}
	if result.Completeness != CompletenessComplete || result.Manifest == nil ||
		len(result.Manifest.Files) != 1 {
		t.Fatalf("rename result = %+v", result)
	}
	change := result.Manifest.Files[0]
	if change.Status != ChangeRenamed || change.OldPath != "old.go" || change.Path != "new.go" ||
		change.SimilarityPercent == nil || *change.SimilarityPercent != 100 ||
		change.Language != "go" {
		t.Fatalf("rename change = %+v", change)
	}
	if change.HunkCount != 0 || result.Manifest.Coverage.TotalHunks != 0 {
		t.Fatalf("pure rename unexpectedly has textual hunks: %+v", result.Manifest.Coverage)
	}
}

func TestMaterializeDiffMarksNonPortableGitPathAsSkipped(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "safe.go", "package fixture\n")
	base := commitAll(t, repository, "base")
	writeFile(t, repository, "bad:name.go", "package fixture\n")
	head := commitAll(t, repository, "unsafe path")

	result, err := newTestAdapter(t).MaterializeDiff(context.Background(), Request{
		RepositoryPath: repository,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("MaterializeDiff() error = %v", err)
	}
	if result.Completeness != CompletenessPartial ||
		!result.HasReason(ReasonUnsafeRepositoryPath) {
		t.Fatalf("result = %+v, want unsafe_repository_path partial", result)
	}
	if result.Manifest == nil || len(result.Manifest.Files) != 1 {
		t.Fatalf("manifest = %+v, want one changed file", result.Manifest)
	}
	change := result.Manifest.Files[0]
	if change.Path != "bad:name.go" || change.PathSHA256 == "" || change.Included ||
		!containsReasonCode(change.SkippedReasons, ReasonUnsafeRepositoryPath) {
		t.Fatalf("unsafe path change = %+v", change)
	}
	if result.Manifest.Coverage.TotalFiles != 1 ||
		result.Manifest.Coverage.IncludedFiles != 0 ||
		result.Manifest.Coverage.SkippedFiles != 1 ||
		result.Manifest.Coverage.IncludedHunks != 0 ||
		result.Manifest.Coverage.SkippedHunks != result.Manifest.Coverage.TotalHunks {
		t.Fatalf("unsafe path coverage = %+v", result.Manifest.Coverage)
	}
}

func TestDetectRevisionMovement(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "fixture.go", "package fixture\n")
	base := commitAll(t, repository, "base")
	writeFile(t, repository, "fixture.go", "package fixture\n\nconst Value = 1\n")
	capturedHead := commitAll(t, repository, "captured head")
	writeFile(t, repository, "fixture.go", "package fixture\n\nconst Value = 2\n")
	_ = commitAll(t, repository, "moved head")

	reason, err := newTestAdapter(t).detectRevisionMovement(
		context.Background(),
		repository,
		base,
		base,
		"main",
		capturedHead,
	)
	if err != nil {
		t.Fatalf("detectRevisionMovement() error = %v", err)
	}
	if reason == nil || reason.Code != ReasonRevisionMoved {
		t.Fatalf("reason = %+v, want revision_moved_during_capture", reason)
	}
}

func TestPortableRepositoryPathGrammar(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "internal/source/file.go", want: true},
		{path: "../outside.go", want: false},
		{path: "a/../outside.go", want: false},
		{path: "bad:name.go", want: false},
		{path: `bad\name.go`, want: false},
		{path: "bad\nname.go", want: false},
		{path: "/absolute.go", want: false},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%q", test.path), func(t *testing.T) {
			if got := isPortableRepositoryPath(test.path); got != test.want {
				t.Fatalf("isPortableRepositoryPath(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

func TestReadFileAtCommitReturnsExactBlobNotWorkingTree(t *testing.T) {
	repository := newTestRepository(t)
	committed := []byte("package fixture\n\nconst Value = 1\n")
	writeBytes(t, repository, "fixture.go", committed)
	revision := commitAll(t, repository, "source")
	writeFile(t, repository, "fixture.go", "package fixture\n\nconst Value = 999\n")

	content, err := newTestAdapter(t).ReadFileAtCommit(
		context.Background(),
		repository,
		revision,
		"fixture.go",
		0,
	)
	if err != nil {
		t.Fatalf("ReadFileAtCommit() error = %v", err)
	}
	if !content.Included || content.Completeness != CompletenessComplete ||
		content.Mode != "100644" || content.BlobOID == "" {
		t.Fatalf("content metadata = %+v", content)
	}
	if !bytes.Equal(content.Data, committed) {
		t.Fatalf("content = %q, want exact committed bytes %q", content.Data, committed)
	}
	digest := sha256.Sum256(committed)
	if content.SHA256 != fmt.Sprintf("%x", digest) ||
		content.SizeBytes != int64(len(committed)) {
		t.Fatalf("content identity = %s/%d, want %x/%d", content.SHA256, content.SizeBytes, digest, len(committed))
	}
}

func TestReadFileAtCommitRejectsCRLFAndPreservesRawIdentity(t *testing.T) {
	repository := newTestRepository(t)
	committed := []byte("package fixture\r\n// ARGUS_BUG committed\r\n")
	writeBytes(t, repository, "fixture.go", committed)
	revision := commitAll(t, repository, "CRLF source")

	content, err := newTestAdapter(t).ReadFileAtCommit(
		context.Background(),
		repository,
		revision,
		"fixture.go",
		0,
	)
	if err != nil {
		t.Fatalf("ReadFileAtCommit(CRLF) error = %v", err)
	}
	if content.Included ||
		content.Completeness != CompletenessSkipped ||
		!content.HasReason(ReasonNonCanonicalLineEndings) ||
		content.Data != nil {
		t.Fatalf("CRLF content = %+v", content)
	}
	digest := sha256.Sum256(committed)
	if content.SHA256 != fmt.Sprintf("%x", digest) ||
		content.SizeBytes != int64(len(committed)) {
		t.Fatalf(
			"CRLF identity = %s/%d, want raw committed identity %x/%d",
			content.SHA256,
			content.SizeBytes,
			digest,
			len(committed),
		)
	}
}

func TestReadFileAtCommitRejectsTraversalBinaryAndOversizeWithoutPrefixes(t *testing.T) {
	repository := newTestRepository(t)
	text := []byte(strings.Repeat("reviewable line\n", 32))
	binary := []byte{0x01, 0x02, 0x00, 0x03, 0xff}
	invalidUTF8 := []byte{0xff, 0xfe, 'x'}
	writeBytes(t, repository, "large.go", text)
	writeBytes(t, repository, "asset.bin", binary)
	writeBytes(t, repository, "invalid.go", invalidUTF8)
	revision := commitAll(t, repository, "files")
	adapter := newTestAdapter(t)

	traversal, err := adapter.ReadFileAtCommit(
		context.Background(), repository, revision, "../outside.go", 0,
	)
	if code, ok := ErrorCodeOf(err); !ok || code != ErrorInvalidRequest {
		t.Fatalf("traversal error = %v (%q/%v), want invalid_request", err, code, ok)
	}
	if traversal.Included || !traversal.HasReason(ReasonUnsafeRepositoryPath) {
		t.Fatalf("traversal result = %+v", traversal)
	}

	binaryContent, err := adapter.ReadFileAtCommit(
		context.Background(), repository, revision, "asset.bin", 0,
	)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if binaryContent.Included ||
		binaryContent.Completeness != CompletenessSkipped ||
		!binaryContent.HasReason(ReasonBinaryFile) ||
		binaryContent.Data != nil {
		t.Fatalf("binary content = %+v", binaryContent)
	}
	binaryDigest := sha256.Sum256(binary)
	if binaryContent.SHA256 != fmt.Sprintf("%x", binaryDigest) ||
		binaryContent.SizeBytes != int64(len(binary)) {
		t.Fatalf("binary identity = %s/%d, want full exact stream", binaryContent.SHA256, binaryContent.SizeBytes)
	}

	invalidContent, err := adapter.ReadFileAtCommit(
		context.Background(), repository, revision, "invalid.go", 0,
	)
	if err != nil {
		t.Fatalf("read invalid UTF-8: %v", err)
	}
	if invalidContent.Included ||
		invalidContent.Completeness != CompletenessSkipped ||
		!invalidContent.HasReason(ReasonUnsupportedEncoding) ||
		invalidContent.Data != nil {
		t.Fatalf("invalid UTF-8 content = %+v", invalidContent)
	}
	invalidDigest := sha256.Sum256(invalidUTF8)
	if invalidContent.SHA256 != fmt.Sprintf("%x", invalidDigest) ||
		invalidContent.SizeBytes != int64(len(invalidUTF8)) {
		t.Fatalf("invalid UTF-8 identity = %s/%d, want full exact stream", invalidContent.SHA256, invalidContent.SizeBytes)
	}

	oversize, err := adapter.ReadFileAtCommit(
		context.Background(), repository, revision, "large.go", 16,
	)
	if err != nil {
		t.Fatalf("read oversize file: %v", err)
	}
	if oversize.Included ||
		oversize.Completeness != CompletenessPartial ||
		!oversize.HasReason(ReasonFileSizeExceeded) ||
		oversize.Data != nil {
		t.Fatalf("oversize content = %+v", oversize)
	}
	if oversize.SHA256 != "" ||
		oversize.SizeBytes != int64(len(text)) ||
		oversize.BlobOID == "" {
		t.Fatalf(
			"oversize identity = sha256 %q blob %q size %d, want blob OID plus metadata size",
			oversize.SHA256,
			oversize.BlobOID,
			oversize.SizeBytes,
		)
	}
}

func TestReadFileAtCommitRejectsSymlinkAndSubmodule(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "target.go", "package fixture\n")
	base := commitAll(t, repository, "base")
	if err := os.Symlink("target.go", filepath.Join(repository, "linked.go")); err != nil {
		t.Fatalf("create symlink fixture: %v", err)
	}
	runTestGit(t, repository, "add", "--all")
	runTestGit(t, repository, "update-index", "--add", "--cacheinfo", "160000", base, "vendor/dependency")
	runTestGit(t, repository, "commit", "-q", "-m", "special entries")
	revision := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
	adapter := newTestAdapter(t)

	symlink, err := adapter.ReadFileAtCommit(
		context.Background(), repository, revision, "linked.go", 0,
	)
	if err != nil {
		t.Fatalf("read symlink: %v", err)
	}
	if symlink.Included || !symlink.HasReason(ReasonSymlinkNotAllowed) ||
		symlink.Mode != "120000" {
		t.Fatalf("symlink result = %+v", symlink)
	}

	submodule, err := adapter.ReadFileAtCommit(
		context.Background(), repository, revision, "vendor/dependency", 0,
	)
	if err != nil {
		t.Fatalf("read submodule: %v", err)
	}
	if submodule.Included || !submodule.HasReason(ReasonSubmoduleNotAllowed) ||
		submodule.Mode != "160000" {
		t.Fatalf("submodule result = %+v", submodule)
	}
}

func newTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	adapter, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return adapter
}

func newTestRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	runTestGit(t, repository, "init", "-q", "-b", "main")
	runTestGit(t, repository, "config", "user.name", "Argus Test")
	runTestGit(t, repository, "config", "user.email", "argus@example.invalid")
	runTestGit(t, repository, "config", "commit.gpgsign", "false")
	return repository
}

func commitAll(t *testing.T, repository, message string) string {
	t.Helper()
	runTestGit(t, repository, "add", "--all")
	runTestGit(t, repository, "commit", "-q", "-m", message)
	return strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
}

func writeFile(t *testing.T, repository, path, content string) {
	t.Helper()
	writeBytes(t, repository, path, []byte(content))
}

func writeBytes(t *testing.T, repository, path string, content []byte) {
	t.Helper()
	fullPath := filepath.Join(repository, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(fullPath, content, 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}

func runTestGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", args...)
	command.Dir = repository
	command.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func requireFileChange(t *testing.T, files []FileChange, path string) FileChange {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("file change %q not found in %+v", path, files)
	return FileChange{}
}
