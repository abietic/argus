package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
)

func TestMaterializeSelectionRejectsUnavailableLineRanges(t *testing.T) {
	tests := []struct {
		name    string
		content string
		start   uint32
		end     uint32
		want    string
	}{
		{
			name:    "past end of file",
			content: "package fixture\n",
			start:   2,
			end:     2,
			want:    "exceeds 1 frozen lines",
		},
		{
			name:    "empty file",
			content: "",
			start:   1,
			end:     1,
			want:    "exceeds 0 frozen lines",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositoryPath := newMaterializeRepository(t)
			writeFixture(t, repositoryPath, "review.go", test.content)
			revision := commitFixture(t, repositoryPath, "selection source")
			source, artifacts := newMaterializeHarness(t)

			_, err := Materialize(
				context.Background(),
				source,
				artifacts,
				ReviewRequest{
					RepositoryPath: repositoryPath,
					Mode:           reviewcore.TargetModeSelection,
					Revision:       revision,
					SelectionPath:  "review.go",
					StartLine:      test.start,
					EndLine:        test.end,
				},
				DefaultLocalConfig(),
			)
			if !errors.Is(err, ErrTargetNotReviewable) ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Materialize(selection) error = %v, want target-not-reviewable containing %q",
					err, test.want)
			}
		})
	}
}

func TestMaterializeSelectionFreezesMultipleEffectiveRanges(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	content := strings.Join([]string{
		"package fixture",
		"// TODO first selected range",
		"func untouched() {}",
		"// ARGUS_BUG second selected range",
		"// FIXME outside selection",
		"",
	}, "\n")
	writeFixture(t, repositoryPath, "review.go", content)
	revision := commitFixture(t, repositoryPath, "multi-range selection")
	source, artifacts := newMaterializeHarness(t)

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       revision,
			SelectionPath:  "review.go",
			SelectionRanges: []SelectionRange{
				{StartLine: 2, EndLine: 2},
				{StartLine: 4, EndLine: 4},
			},
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(multi-range selection) error = %v", err)
	}
	selection := materialized.Target.Snapshot.Selection
	if selection == nil ||
		selection.StartLine != 0 ||
		selection.EndLine != 0 ||
		len(selection.EffectiveRanges) != 2 ||
		len(materialized.Input.Regions) != 2 {
		t.Fatalf("multi-range frozen selector = %+v, regions = %+v",
			selection, materialized.Input.Regions)
	}
	selected, err := artifacts.ReadArtifact(*materialized.Target.SelectionContentRef)
	if err != nil {
		t.Fatalf("ReadArtifact(selection content) error = %v", err)
	}
	if string(selected) !=
		"// TODO first selected range\n// ARGUS_BUG second selected range\n" {
		t.Fatalf("multi-range selected bytes = %q", selected)
	}
	result, err := reviewcore.Run(context.Background(), materialized.Input, reviewcore.RunOptions{})
	if err != nil {
		t.Fatalf("Run(multi-range selection) error = %v", err)
	}
	if len(result.Report.Findings) != 2 {
		t.Fatalf("multi-range findings = %+v", result.Report.Findings)
	}
	for _, finding := range result.Report.Findings {
		if finding.StartLine == 5 {
			t.Fatalf("outside-selection line became finding: %+v", finding)
		}
	}
}

func TestMaterializeSelectionResolvesExactGoMethodSymbol(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	content := strings.Join([]string{
		"package fixture",
		"// FIXME outside symbol",
		"type Service struct{}",
		"func (Service) Review() {",
		"\t// ARGUS_BUG inside symbol",
		"}",
		"",
	}, "\n")
	writeFixture(t, repositoryPath, "review.go", content)
	revision := commitFixture(t, repositoryPath, "symbol selection")
	source, artifacts := newMaterializeHarness(t)

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       revision,
			SelectionPath:  "review.go",
			SelectionSymbol: &SymbolSelector{
				Language: "go", Kind: "method", QualifiedName: "Service.Review",
			},
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(symbol selection) error = %v", err)
	}
	selection := materialized.Target.Snapshot.Selection
	if selection == nil || selection.Symbol == nil ||
		selection.Symbol.QualifiedName != "Service.Review" ||
		len(selection.EffectiveRanges) != 1 ||
		selection.EffectiveRanges[0] != (SelectionRange{StartLine: 4, EndLine: 6}) ||
		len(materialized.Input.Regions) != 1 {
		t.Fatalf("symbol frozen selector = %+v, regions = %+v",
			selection, materialized.Input.Regions)
	}
	result, err := reviewcore.Run(context.Background(), materialized.Input, reviewcore.RunOptions{})
	if err != nil {
		t.Fatalf("Run(symbol selection) error = %v", err)
	}
	if len(result.Report.Findings) != 1 ||
		result.Report.Findings[0].StartLine != 5 {
		t.Fatalf("symbol findings = %+v", result.Report.Findings)
	}
}

func TestMaterializeSelectionRejectsMissingGoSymbol(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "review.go", "package fixture\nfunc Present() {}\n")
	revision := commitFixture(t, repositoryPath, "missing symbol")
	source, artifacts := newMaterializeHarness(t)

	_, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       revision,
			SelectionPath:  "review.go",
			SelectionSymbol: &SymbolSelector{
				Language: "go", Kind: "function", QualifiedName: "Missing",
			},
		},
		DefaultLocalConfig(),
	)
	if !errors.Is(err, ErrTargetNotReviewable) ||
		!strings.Contains(err.Error(), "resolved to 0 declarations") {
		t.Fatalf("Materialize(missing symbol) error = %v", err)
	}
}

func TestMaterializeSelectionRejectsOversizeOverlayWithoutTruncation(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "review.go", "old\n")
	revision := commitFixture(t, repositoryPath, "selection source")
	source, artifacts := newMaterializeHarness(t)
	config := DefaultLocalConfig()
	config.MaxFileContentBytes = 8
	overlay := strings.Repeat("x", 9)

	_, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       revision,
			SelectionPath:  "review.go",
			StartLine:      1,
			EndLine:        1,
			OverlayContent: &overlay,
		},
		config,
	)
	if !errors.Is(err, ErrTargetNotReviewable) ||
		!strings.Contains(err.Error(), "selection overlay is 9 bytes") {
		t.Fatalf("Materialize(oversize overlay) error = %v", err)
	}
}

func TestMaterializeSelectionRejectsCommitBackedCRLF(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	content := "package fixture\r\n// ARGUS_BUG committed\r\n"
	writeFixture(t, repositoryPath, "review.go", content)
	revision := commitFixture(t, repositoryPath, "CRLF selection source")
	source, artifacts := newMaterializeHarness(t)

	_, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       revision,
			SelectionPath:  "review.go",
			StartLine:      2,
			EndLine:        2,
		},
		DefaultLocalConfig(),
	)
	if !errors.Is(err, ErrTargetNotReviewable) ||
		!strings.Contains(
			err.Error(),
			string(gitadapter.ReasonNonCanonicalLineEndings),
		) {
		t.Fatalf("Materialize(CRLF selection) error = %v", err)
	}
}

func TestMaterializeScopeSkipsCRLFWithoutLeakingItIntoReviewCore(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	crlf := []byte("package fixture\r\n// ARGUS_BUG skipped CRLF\r\n")
	writeFixture(t, repositoryPath, "crlf.go", string(crlf))
	writeFixture(
		t,
		repositoryPath,
		"lf.go",
		"package fixture\n// ARGUS_BUG retained LF\n",
	)
	revision := commitFixture(t, repositoryPath, "mixed line endings")
	source, artifacts := newMaterializeHarness(t)

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeScope,
			Revision:       revision,
			Include:        []string{"**/*.go"},
			Exclude:        []string{},
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(CRLF scope) error = %v", err)
	}
	if materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		materialized.Target.Snapshot.Scope.MatchedFiles != 2 ||
		materialized.Target.Snapshot.Scope.IncludedFiles != 1 ||
		materialized.Target.Snapshot.Scope.SkippedFiles != 1 ||
		len(materialized.Input.Files) != 1 ||
		materialized.Input.Files[0].Path != "lf.go" ||
		materialized.Input.Files[0].Content == nil ||
		strings.ContainsRune(*materialized.Input.Files[0].Content, '\r') {
		t.Fatalf("CRLF scope = target %+v input %+v",
			materialized.Target, materialized.Input)
	}
	var crlfRef *TargetFileRef
	for index := range materialized.Target.FileRefs {
		if materialized.Target.FileRefs[index].Path == "crlf.go" {
			crlfRef = &materialized.Target.FileRefs[index]
			break
		}
	}
	if crlfRef == nil ||
		crlfRef.Completeness != gitadapter.CompletenessSkipped ||
		crlfRef.ContentRef != nil ||
		!hasMaterializationReason(
			crlfRef.Reasons,
			gitadapter.ReasonNonCanonicalLineEndings,
		) {
		t.Fatalf("CRLF target file ref = %+v", crlfRef)
	}

	var manifest ScopeManifest
	if err := artifacts.ReadJSONArtifact(materialized.Target.ManifestRef, &manifest); err != nil {
		t.Fatalf("read CRLF scope manifest: %v", err)
	}
	var crlfManifest *ScopeManifestFile
	for index := range manifest.Files {
		if manifest.Files[index].Path == "crlf.go" {
			crlfManifest = &manifest.Files[index]
			break
		}
	}
	rawDigest := sha256.Sum256(crlf)
	if crlfManifest == nil ||
		crlfManifest.Completeness != gitadapter.CompletenessSkipped ||
		crlfManifest.SHA256 != fmt.Sprintf("%x", rawDigest) ||
		crlfManifest.SizeBytes != int64(len(crlf)) ||
		!hasMaterializationReason(
			crlfManifest.Reasons,
			gitadapter.ReasonNonCanonicalLineEndings,
		) {
		t.Fatalf("CRLF manifest file = %+v", crlfManifest)
	}

	result, err := reviewcore.Run(
		context.Background(),
		materialized.Input,
		reviewcore.RunOptions{},
	)
	if err != nil {
		t.Fatalf("reviewcore.Run(CRLF-filtered input) error = %v", err)
	}
	if len(result.Report.Findings) != 1 ||
		result.Report.Findings[0].Path != "lf.go" {
		t.Fatalf("CRLF-filtered report = %+v", result.Report)
	}
}

func TestMaterializeScopeFreezesNoMatchingFilesAsExplicitCoverage(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "main.go", "package main\n")
	revision := commitFixture(t, repositoryPath, "scope source")
	source, artifacts := newMaterializeHarness(t)

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeScope,
			Revision:       revision,
			Include:        []string{"docs/**"},
			Exclude:        []string{},
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(no-match scope) error = %v", err)
	}
	scope := materialized.Target.Snapshot.Scope
	if materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		scope == nil ||
		scope.MatchedFiles != 0 ||
		scope.IncludedFiles != 0 ||
		scope.SkippedFiles != 0 ||
		len(materialized.Target.FileRefs) != 0 ||
		len(materialized.Input.Files) != 0 ||
		len(materialized.Input.Regions) != 0 ||
		!hasMaterializationReason(
			materialized.Target.Snapshot.CompletenessReason,
			gitadapter.ReasonNoMatchingFiles,
		) {
		t.Fatalf("no-match scope = target %+v input %+v",
			materialized.Target, materialized.Input)
	}
	var manifest ScopeManifest
	if err := artifacts.ReadJSONArtifact(materialized.Target.ManifestRef, &manifest); err != nil {
		t.Fatalf("read no-match scope manifest: %v", err)
	}
	if manifest.Coverage.ScannedFiles != 1 ||
		manifest.Coverage.MatchedFiles != 0 ||
		manifest.Coverage.IncludedFiles != 0 ||
		manifest.Coverage.SkippedFiles != 0 ||
		len(manifest.Files) != 0 ||
		!hasMaterializationReason(manifest.Reasons, gitadapter.ReasonNoMatchingFiles) {
		t.Fatalf("no-match scope manifest = %+v", manifest)
	}
}

func TestMaterializeScopePreservesPartialCoverageBeyondMaxFiles(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	for _, path := range []string{"a.go", "b.go", "nested/c.go"} {
		writeFixture(t, repositoryPath, path, "package fixture\n// TODO retained marker\n")
	}
	revision := commitFixture(t, repositoryPath, "scope source")
	source, artifacts := newMaterializeHarness(t)

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeScope,
			Revision:       revision,
			Include:        []string{"**/*.go"},
			Exclude:        []string{},
			MaxFiles:       2,
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(max-files scope) error = %v", err)
	}
	scope := materialized.Target.Snapshot.Scope
	if materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		scope == nil ||
		scope.MatchedFiles != 3 ||
		scope.IncludedFiles != 2 ||
		scope.SkippedFiles != 1 ||
		!hasMaterializationReason(
			materialized.Target.Snapshot.CompletenessReason,
			gitadapter.ReasonFileLimitExceeded,
		) {
		t.Fatalf("partial scope snapshot = %+v", materialized.Target.Snapshot)
	}
	if len(materialized.Target.FileRefs) != 2 ||
		materialized.Target.FileRefs[0].Path != "a.go" ||
		materialized.Target.FileRefs[1].Path != "b.go" ||
		len(materialized.Input.Files) != 2 ||
		len(materialized.Input.Regions) != 2 {
		t.Fatalf("retained scope target = %+v input = %+v",
			materialized.Target.FileRefs, materialized.Input)
	}

	var manifest ScopeManifest
	if err := artifacts.ReadJSONArtifact(materialized.Target.ManifestRef, &manifest); err != nil {
		t.Fatalf("read scope manifest: %v", err)
	}
	if manifest.Completeness != gitadapter.CompletenessPartial ||
		manifest.Coverage.ScannedFiles != 3 ||
		manifest.Coverage.MatchedFiles != 3 ||
		manifest.Coverage.IncludedFiles != 2 ||
		manifest.Coverage.SkippedFiles != 1 ||
		!hasMaterializationReason(manifest.Reasons, gitadapter.ReasonFileLimitExceeded) {
		t.Fatalf("partial scope manifest = %+v", manifest)
	}
}

func TestMaterializeScopeRecordsBinaryAndSymlinkAsExplicitlySkipped(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "binary.dat", "prefix\x00suffix")
	writeFixture(t, repositoryPath, "target.go", "package fixture\n")
	if err := os.Symlink("target.go", repositoryPath+"/linked.go"); err != nil {
		t.Fatalf("create symlink fixture: %v", err)
	}
	revision := commitFixture(t, repositoryPath, "scope source")
	source, artifacts := newMaterializeHarness(t)

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeScope,
			Revision:       revision,
			Include:        []string{"binary.dat", "linked.go"},
			Exclude:        []string{},
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(binary/symlink scope) error = %v", err)
	}
	if materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		materialized.Target.Snapshot.Scope.IncludedFiles != 0 ||
		materialized.Target.Snapshot.Scope.SkippedFiles != 2 ||
		len(materialized.Input.Files) != 0 ||
		len(materialized.Input.Regions) != 0 {
		t.Fatalf("skipped scope = target %+v input %+v",
			materialized.Target, materialized.Input)
	}
	if len(materialized.Target.FileRefs) != 2 {
		t.Fatalf("file refs = %+v", materialized.Target.FileRefs)
	}
	reasonsByPath := map[string]gitadapter.ReasonCode{
		"binary.dat": gitadapter.ReasonBinaryFile,
		"linked.go":  gitadapter.ReasonSymlinkNotAllowed,
	}
	for _, file := range materialized.Target.FileRefs {
		wantReason, exists := reasonsByPath[file.Path]
		if !exists {
			t.Fatalf("unexpected file ref = %+v", file)
		}
		if file.Completeness != gitadapter.CompletenessSkipped ||
			file.ContentRef != nil ||
			!hasMaterializationReason(file.Reasons, wantReason) {
			t.Fatalf("skipped file ref = %+v", file)
		}
	}

	var manifest ScopeManifest
	if err := artifacts.ReadJSONArtifact(materialized.Target.ManifestRef, &manifest); err != nil {
		t.Fatalf("read scope manifest: %v", err)
	}
	if manifest.Coverage.MatchedFiles != 2 ||
		manifest.Coverage.IncludedFiles != 0 ||
		manifest.Coverage.SkippedFiles != 2 ||
		len(manifest.Files) != 2 {
		t.Fatalf("skipped scope manifest = %+v", manifest)
	}
	for _, file := range manifest.Files {
		wantReason, exists := reasonsByPath[file.Path]
		if !exists {
			t.Fatalf("unexpected manifest file = %+v", file)
		}
		if file.Completeness != gitadapter.CompletenessSkipped ||
			file.SHA256 != "" && file.Path == "linked.go" ||
			!hasMaterializationReason(file.Reasons, wantReason) {
			t.Fatalf("skipped manifest file = %+v", file)
		}
		if file.Path == "binary.dat" && file.SHA256 == "" {
			t.Fatalf("binary manifest omitted digest evidence: %+v", file)
		}
	}
}

func TestMaterializeSelectionFreezesSymbolicRevisionBeforeRefAndWorktreeMove(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	oldContent := "package fixture\n// ARGUS_BUG old commit\nfunc Old() {}\n"
	writeFixture(t, repositoryPath, "review.go", oldContent)
	oldRevision := commitFixture(t, repositoryPath, "old selection")
	adapter, artifacts := newMaterializeHarness(t)
	source := &afterCaptureSource{
		TargetSource: adapter,
		afterCapture: func() {
			writeFixture(t, repositoryPath, "review.go", "package fixture\nfunc New() {}\n")
			_ = commitFixture(t, repositoryPath, "move main")
			writeFixture(t, repositoryPath, "review.go", "package fixture\nfunc Dirty() {}\n")
		},
	}

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       "main",
			SelectionPath:  "review.go",
			StartLine:      2,
			EndLine:        2,
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(moved selection) error = %v", err)
	}
	if materialized.Target.Snapshot.Head.Requested != "main" ||
		materialized.Target.Snapshot.Head.CommitOID != oldRevision ||
		materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		!hasMaterializationReason(
			materialized.Target.Snapshot.CompletenessReason,
			gitadapter.ReasonRevisionMoved,
		) {
		t.Fatalf("moved selection snapshot = %+v", materialized.Target.Snapshot)
	}
	if len(materialized.Input.Files) != 1 ||
		materialized.Input.Files[0].Content == nil ||
		*materialized.Input.Files[0].Content != oldContent {
		t.Fatalf("selection read mutable worktree/ref content: %+v", materialized.Input.Files)
	}
	selected, err := artifacts.ReadArtifact(*materialized.Target.SelectionContentRef)
	if err != nil {
		t.Fatalf("read selection content: %v", err)
	}
	if string(selected) != "// ARGUS_BUG old commit\n" {
		t.Fatalf("selected bytes = %q", selected)
	}
}

func TestMaterializeScopeFreezesSymbolicRevisionBeforeRefAndWorktreeMove(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	oldContent := "package fixture\n// TODO old commit\n"
	writeFixture(t, repositoryPath, "old.go", oldContent)
	oldRevision := commitFixture(t, repositoryPath, "old scope")
	adapter, artifacts := newMaterializeHarness(t)
	source := &afterCaptureSource{
		TargetSource: adapter,
		afterCapture: func() {
			if err := os.Remove(repositoryPath + "/old.go"); err != nil {
				t.Fatalf("remove old scope file: %v", err)
			}
			writeFixture(t, repositoryPath, "new.go", "package fixture\n// TODO new commit\n")
			_ = commitFixture(t, repositoryPath, "move main")
			writeFixture(t, repositoryPath, "dirty.go", "package fixture\n")
		},
	}

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeScope,
			Revision:       "main",
			Include:        []string{"**/*.go"},
			Exclude:        []string{},
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(moved scope) error = %v", err)
	}
	if materialized.Target.Snapshot.Head.CommitOID != oldRevision ||
		materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		!hasMaterializationReason(
			materialized.Target.Snapshot.CompletenessReason,
			gitadapter.ReasonRevisionMoved,
		) {
		t.Fatalf("moved scope snapshot = %+v", materialized.Target.Snapshot)
	}
	if len(materialized.Input.Files) != 1 ||
		materialized.Input.Files[0].Path != "old.go" ||
		materialized.Input.Files[0].Content == nil ||
		*materialized.Input.Files[0].Content != oldContent {
		t.Fatalf("scope read mutable worktree/ref tree: %+v", materialized.Input.Files)
	}
}

func TestMaterializeRejectsRequestLimitBypassBeforeCallingSource(t *testing.T) {
	config := DefaultLocalConfig()
	_, artifacts := newMaterializeHarness(t)
	tests := []struct {
		name    string
		request ReviewRequest
		want    string
	}{
		{
			name: "max files",
			request: ReviewRequest{
				RepositoryPath: t.TempDir(),
				Mode:           reviewcore.TargetModeScope,
				Revision:       "main",
				Include:        []string{"**/*.go"},
				Exclude:        []string{},
				MaxFiles:       config.MaxFiles + 1,
			},
			want: "max_files",
		},
		{
			name: "max patch bytes",
			request: ReviewRequest{
				RepositoryPath: t.TempDir(),
				Mode:           reviewcore.TargetModeDiff,
				BaseRevision:   "main^",
				HeadRevision:   "main",
				MaxPatchBytes:  config.MaxPatchBytes + 1,
			},
			want: "max_patch_bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Materialize(
				context.Background(),
				panicTargetSource{},
				artifacts,
				test.request,
				config,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) ||
				!strings.Contains(err.Error(), "hard limit") {
				t.Fatalf("Materialize(limit bypass) error = %v", err)
			}
		})
	}
}

func TestMaterializeDiffRejectsSpoofedSourceCapture(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "review.go", "package fixture\n")
	baseRevision := commitFixture(t, repositoryPath, "base")
	writeFixture(t, repositoryPath, "review.go", "package fixture\n// changed\n")
	headRevision := commitFixture(t, repositoryPath, "head")
	otherRepository := newMaterializeRepository(t)
	adapter, artifacts := newMaterializeHarness(t)

	redigest := func(result *gitadapter.Result) {
		digest, err := gitadapter.DigestTargetSnapshot(*result.Snapshot)
		if err != nil {
			t.Fatalf("DigestTargetSnapshot() error = %v", err)
		}
		result.Snapshot.SHA256 = digest
	}
	tests := []struct {
		name   string
		mutate func(*gitadapter.Result)
		want   string
	}{
		{
			name: "other repository root",
			mutate: func(result *gitadapter.Result) {
				result.RepositoryRoot = otherRepository
			},
			want: "requested canonical root",
		},
		{
			name: "spoofed repository identity",
			mutate: func(result *gitadapter.Result) {
				result.Snapshot.Repository.RepositoryID = "spoofed"
				redigest(result)
			},
			want: "repository identity",
		},
		{
			name: "other requested base",
			mutate: func(result *gitadapter.Result) {
				result.Snapshot.Base.Requested = "other"
				redigest(result)
			},
			want: "requested revisions",
		},
		{
			name: "other requested head",
			mutate: func(result *gitadapter.Result) {
				result.Snapshot.Head.Requested = "other"
				redigest(result)
			},
			want: "requested revisions",
		},
		{
			name: "snapshot digest",
			mutate: func(result *gitadapter.Result) {
				result.Snapshot.SHA256 = strings.Repeat("0", sha256.Size*2)
			},
			want: "snapshot digest",
		},
		{
			name: "manifest digest",
			mutate: func(result *gitadapter.Result) {
				result.Manifest.Files[0].Path = "other.go"
			},
			want: "manifest digest",
		},
		{
			name: "patch data",
			mutate: func(result *gitadapter.Result) {
				result.Patch.Data[0] ^= 1
			},
			want: "patch sha256/size/data",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &adversarialTargetSource{
				TargetSource:     adapter,
				mutateDiffResult: test.mutate,
			}
			_, err := Materialize(
				context.Background(),
				source,
				artifacts,
				ReviewRequest{
					RepositoryPath: repositoryPath,
					Mode:           reviewcore.TargetModeDiff,
					BaseRevision:   baseRevision,
					HeadRevision:   headRevision,
				},
				DefaultLocalConfig(),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Materialize(spoofed diff) error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMaterializeDiffRejectsSpoofedHeadFileContent(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "review.go", "package fixture\n")
	baseRevision := commitFixture(t, repositoryPath, "base")
	writeFixture(t, repositoryPath, "review.go", "package fixture\n// changed\n")
	headRevision := commitFixture(t, repositoryPath, "head")
	adapter, artifacts := newMaterializeHarness(t)
	otherOID := strings.Repeat("a", len(headRevision))

	tests := []struct {
		name   string
		mutate func(*gitadapter.FileContent)
		want   string
	}{
		{
			name: "schema",
			mutate: func(content *gitadapter.FileContent) {
				content.SchemaVersion = "unsupported"
			},
			want: "unsupported schema",
		},
		{
			name: "other commit",
			mutate: func(content *gitadapter.FileContent) {
				content.CommitOID = otherOID
			},
			want: "want admitted commit",
		},
		{
			name: "other path",
			mutate: func(content *gitadapter.FileContent) {
				content.Path = "other.go"
			},
			want: `want "review.go"`,
		},
		{
			name: "completeness conflict",
			mutate: func(content *gitadapter.FileContent) {
				content.Completeness = gitadapter.CompletenessPartial
			},
			want: "included content must be complete",
		},
		{
			name: "digest mismatch",
			mutate: func(content *gitadapter.FileContent) {
				content.SHA256 = strings.Repeat("0", sha256.Size*2)
			},
			want: "sha256/size/data are inconsistent",
		},
		{
			name: "non regular mode",
			mutate: func(content *gitadapter.FileContent) {
				content.Mode = "120000"
			},
			want: "invalid mode/blob_oid",
		},
		{
			name: "blob does not identify bytes",
			mutate: func(content *gitadapter.FileContent) {
				content.BlobOID = otherOID
			},
			want: "blob_oid does not match its data",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &adversarialTargetSource{
				TargetSource:  adapter,
				mutateContent: test.mutate,
			}
			_, err := Materialize(
				context.Background(),
				source,
				artifacts,
				ReviewRequest{
					RepositoryPath: repositoryPath,
					Mode:           reviewcore.TargetModeDiff,
					BaseRevision:   baseRevision,
					HeadRevision:   headRevision,
				},
				DefaultLocalConfig(),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Materialize(spoofed diff content) error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMaterializeSelectionRejectsSourceIdentitySpoofing(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "review.go", "package fixture\n")
	revision := commitFixture(t, repositoryPath, "selection source")
	adapter, artifacts := newMaterializeHarness(t)
	otherOID := strings.Repeat("a", len(revision))

	tests := []struct {
		name          string
		overlay       bool
		mutateCapture func(*gitadapter.RevisionCapture)
		mutateContent func(*gitadapter.FileContent)
		want          string
	}{
		{
			name: "capture schema",
			mutateCapture: func(capture *gitadapter.RevisionCapture) {
				capture.SchemaVersion = "unsupported"
			},
			want: "revision capture",
		},
		{
			name:    "overlay remains bound to capture",
			overlay: true,
			mutateCapture: func(capture *gitadapter.RevisionCapture) {
				capture.Revision.Requested = "other"
			},
			want: "requested revision",
		},
		{
			name: "file schema",
			mutateContent: func(content *gitadapter.FileContent) {
				content.SchemaVersion = "unsupported"
			},
			want: "selected file capture",
		},
		{
			name: "other commit",
			mutateContent: func(content *gitadapter.FileContent) {
				content.CommitOID = otherOID
			},
			want: "want admitted commit",
		},
		{
			name: "other path",
			mutateContent: func(content *gitadapter.FileContent) {
				content.Path = "other.go"
			},
			want: `want "review.go"`,
		},
		{
			name: "included completeness conflict",
			mutateContent: func(content *gitadapter.FileContent) {
				content.Completeness = gitadapter.CompletenessPartial
			},
			want: "included content must be complete",
		},
		{
			name: "digest mismatch",
			mutateContent: func(content *gitadapter.FileContent) {
				content.SHA256 = strings.Repeat("0", sha256.Size*2)
			},
			want: "sha256/size/data are inconsistent",
		},
		{
			name: "size mismatch",
			mutateContent: func(content *gitadapter.FileContent) {
				content.SizeBytes++
			},
			want: "sha256/size/data are inconsistent",
		},
		{
			name: "blob does not identify bytes",
			mutateContent: func(content *gitadapter.FileContent) {
				content.BlobOID = otherOID
			},
			want: "blob_oid does not match its data",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &adversarialTargetSource{
				TargetSource:  adapter,
				mutateCapture: test.mutateCapture,
				mutateContent: test.mutateContent,
			}
			request := ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeSelection,
				Revision:       revision,
				SelectionPath:  "review.go",
				StartLine:      1,
				EndLine:        1,
			}
			if test.overlay {
				overlay := "package overlay\n"
				request.OverlayContent = &overlay
			}
			_, err := Materialize(
				context.Background(),
				source,
				artifacts,
				request,
				DefaultLocalConfig(),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Materialize(spoofed selection) error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMaterializeSelectionAndScopeBindCaptureToRequestedRepository(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "a.go", "package fixture\n")
	revision := commitFixture(t, repositoryPath, "source")
	otherRepository := newMaterializeRepository(t)
	adapter, artifacts := newMaterializeHarness(t)
	modes := []struct {
		name    string
		request ReviewRequest
	}{
		{
			name: "selection",
			request: ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeSelection,
				Revision:       revision,
				SelectionPath:  "a.go",
				StartLine:      1,
				EndLine:        1,
			},
		},
		{
			name: "scope",
			request: ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeScope,
				Revision:       revision,
				Include:        []string{"**/*.go"},
				Exclude:        []string{},
			},
		},
	}
	mutations := []struct {
		name   string
		mutate func(*gitadapter.RevisionCapture)
		want   string
	}{
		{
			name: "other repository root",
			mutate: func(capture *gitadapter.RevisionCapture) {
				capture.RepositoryRoot = otherRepository
			},
			want: "requested canonical root",
		},
		{
			name: "spoofed repository identity",
			mutate: func(capture *gitadapter.RevisionCapture) {
				capture.Repository.RepositoryID = "spoofed"
			},
			want: "repository identity",
		},
	}
	for _, mode := range modes {
		for _, mutation := range mutations {
			t.Run(mode.name+"/"+mutation.name, func(t *testing.T) {
				source := &adversarialTargetSource{
					TargetSource:  adapter,
					mutateCapture: mutation.mutate,
				}
				_, err := Materialize(
					context.Background(),
					source,
					artifacts,
					mode.request,
					DefaultLocalConfig(),
				)
				if err == nil || !strings.Contains(err.Error(), mutation.want) {
					t.Fatalf(
						"Materialize(spoofed %s capture) error = %v, want %q",
						mode.name,
						err,
						mutation.want,
					)
				}
			})
		}
	}
}

func TestMaterializeSelectionAcceptsCanonicalizedSymlinkRepository(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "a.go", "package fixture\n")
	revision := commitFixture(t, repositoryPath, "source")
	aliasRoot := t.TempDir()
	aliasPath := aliasRoot + "/repository"
	if err := os.Symlink(repositoryPath, aliasPath); err != nil {
		t.Fatalf("create repository symlink: %v", err)
	}
	adapter, artifacts := newMaterializeHarness(t)

	materialized, err := Materialize(
		context.Background(),
		adapter,
		artifacts,
		ReviewRequest{
			RepositoryPath: aliasPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       revision,
			SelectionPath:  "a.go",
			StartLine:      1,
			EndLine:        1,
		},
		DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatalf("Materialize(canonicalized symlink repository) error = %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(repositoryPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(repositoryPath) error = %v", err)
	}
	if materialized.RepositoryRoot != wantRoot {
		t.Fatalf("RepositoryRoot = %q, want %q", materialized.RepositoryRoot, wantRoot)
	}
}

func TestMaterializeScopeRejectsSpoofedListing(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "a.go", "package fixture\n")
	writeFixture(t, repositoryPath, "b.go", "package fixture\n")
	revision := commitFixture(t, repositoryPath, "scope source")
	adapter, artifacts := newMaterializeHarness(t)
	otherOID := strings.Repeat("a", len(revision))

	tests := []struct {
		name   string
		mutate func(*gitadapter.RevisionFileList)
		want   string
	}{
		{
			name: "schema",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.SchemaVersion = "unsupported"
			},
			want: "unsupported schema",
		},
		{
			name: "other commit",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.CommitOID = otherOID
			},
			want: "want captured commit",
		},
		{
			name: "changed include",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.Include = []string{"**"}
			},
			want: "include/exclude",
		},
		{
			name: "changed exclude",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.Exclude = []string{"a.go"}
			},
			want: "include/exclude",
		},
		{
			name: "changed policy include",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.PolicyInclude = []string{"src/**"}
			},
			want: "include/exclude",
		},
		{
			name: "changed policy exclude",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.PolicyExclude = []string{"a.go"}
			},
			want: "include/exclude",
		},
		{
			name: "coverage",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.Coverage.RetainedFiles++
			},
			want: "coverage is inconsistent",
		},
		{
			name: "exclusion coverage",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.Coverage.PolicyExcludedFiles++
			},
			want: "coverage is inconsistent",
		},
		{
			name: "unsorted",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.Files[0], listing.Files[1] = listing.Files[1], listing.Files[0]
			},
			want: "uniquely sorted",
		},
		{
			name: "duplicate",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.Files[1] = listing.Files[0]
			},
			want: "uniquely sorted",
		},
		{
			name: "file outside requested scope",
			mutate: func(listing *gitadapter.RevisionFileList) {
				listing.Files[1].Path = "z.txt"
				listing.Files[1].Language = gitadapter.DetectLanguage("z.txt")
			},
			want: "outside the admitted scope",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &adversarialTargetSource{
				TargetSource:  adapter,
				mutateListing: test.mutate,
			}
			_, err := Materialize(
				context.Background(),
				source,
				artifacts,
				ReviewRequest{
					RepositoryPath: repositoryPath,
					Mode:           reviewcore.TargetModeScope,
					Revision:       revision,
					Include:        []string{"**/*.go"},
					Exclude:        []string{},
				},
				DefaultLocalConfig(),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Materialize(spoofed listing) error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("source cannot mutate admitted pattern slices", func(t *testing.T) {
		request := ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeScope,
			Revision:       revision,
			Include:        []string{"**/*.go"},
			Exclude:        []string{},
		}
		source := &adversarialTargetSource{
			TargetSource: adapter,
			mutateScopeArguments: func(include []string, _ []string) {
				include[0] = "**"
			},
		}
		_, err := Materialize(
			context.Background(),
			source,
			artifacts,
			request,
			DefaultLocalConfig(),
		)
		if err == nil || !strings.Contains(err.Error(), "include/exclude") {
			t.Fatalf("Materialize(mutated scope arguments) error = %v", err)
		}
		if request.Include[0] != "**/*.go" {
			t.Fatalf("source mutated caller-owned request: %+v", request.Include)
		}
	})

	t.Run("source cannot mutate resolved policy slices", func(t *testing.T) {
		config := DefaultLocalConfig()
		config.TargetInclude = []string{"**/*.go"}
		source := &adversarialTargetSource{
			TargetSource: adapter,
			mutateScopeAdmission: func(admission *gitadapter.ScopeAdmission) {
				admission.PolicyInclude[0] = "**"
			},
		}
		_, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeScope,
				Revision:       revision,
				Include:        []string{"**/*.go"},
				Exclude:        []string{},
			},
			config,
		)
		if err == nil || !strings.Contains(err.Error(), "include/exclude") {
			t.Fatalf("Materialize(mutated scope policy) error = %v", err)
		}
		if config.TargetInclude[0] != "**/*.go" {
			t.Fatalf("source mutated caller-owned config: %+v", config.TargetInclude)
		}
	})
}

func TestMaterializeScopeRejectsContentOutsideListingIdentity(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "a.go", "package fixture\n")
	revision := commitFixture(t, repositoryPath, "scope source")
	adapter, artifacts := newMaterializeHarness(t)
	otherOID := strings.Repeat("a", len(revision))

	tests := []struct {
		name   string
		mutate func(*gitadapter.FileContent)
		want   string
	}{
		{
			name: "schema",
			mutate: func(content *gitadapter.FileContent) {
				content.SchemaVersion = "unsupported"
			},
			want: "unsupported schema",
		},
		{
			name: "other commit",
			mutate: func(content *gitadapter.FileContent) {
				content.CommitOID = otherOID
			},
			want: "want admitted commit",
		},
		{
			name: "other path",
			mutate: func(content *gitadapter.FileContent) {
				content.Path = "other.go"
			},
			want: `want "a.go"`,
		},
		{
			name: "other mode",
			mutate: func(content *gitadapter.FileContent) {
				content.Mode = "100755"
			},
			want: "mode/blob_oid",
		},
		{
			name: "other blob",
			mutate: func(content *gitadapter.FileContent) {
				content.BlobOID = otherOID
			},
			want: "mode/blob_oid",
		},
		{
			name: "digest mismatch",
			mutate: func(content *gitadapter.FileContent) {
				content.SHA256 = strings.Repeat("0", sha256.Size*2)
			},
			want: "sha256/size/data are inconsistent",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &adversarialTargetSource{
				TargetSource:  adapter,
				mutateContent: test.mutate,
			}
			_, err := Materialize(
				context.Background(),
				source,
				artifacts,
				ReviewRequest{
					RepositoryPath: repositoryPath,
					Mode:           reviewcore.TargetModeScope,
					Revision:       revision,
					Include:        []string{"**/*.go"},
					Exclude:        []string{},
				},
				DefaultLocalConfig(),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Materialize(spoofed scope content) error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMaterializeRejectsSpoofedRevisionRevalidation(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "a.go", "package fixture\n")
	revision := commitFixture(t, repositoryPath, "revalidation source")
	adapter, artifacts := newMaterializeHarness(t)
	otherOID := strings.Repeat("a", len(revision))

	mutations := []struct {
		name   string
		mutate func(*gitadapter.RevisionRevalidation)
		want   string
	}{
		{
			name: "requested revision",
			mutate: func(result *gitadapter.RevisionRevalidation) {
				result.Requested = "other"
			},
			want: "requested/expected commit",
		},
		{
			name: "expected commit",
			mutate: func(result *gitadapter.RevisionRevalidation) {
				result.ExpectedCommitOID = otherOID
			},
			want: "requested/expected commit",
		},
		{
			name: "unchanged actual commit",
			mutate: func(result *gitadapter.RevisionRevalidation) {
				result.ActualCommitOID = otherOID
			},
			want: "unchanged revalidation",
		},
		{
			name: "moved without reason",
			mutate: func(result *gitadapter.RevisionRevalidation) {
				result.State = gitadapter.RevisionStateMoved
				result.ActualCommitOID = otherOID
				result.Reasons = []gitadapter.Reason{}
			},
			want: "moved revalidation",
		},
		{
			name: "moved to expected commit",
			mutate: func(result *gitadapter.RevisionRevalidation) {
				result.State = gitadapter.RevisionStateMoved
				result.Reasons = []gitadapter.Reason{{
					Code: gitadapter.ReasonRevisionMoved,
				}}
			},
			want: "moved revalidation",
		},
		{
			name: "missing with actual commit",
			mutate: func(result *gitadapter.RevisionRevalidation) {
				result.State = gitadapter.RevisionStateMissing
				result.Reasons = []gitadapter.Reason{{
					Code: gitadapter.ReasonRevisionMissing,
				}}
			},
			want: "missing revalidation",
		},
		{
			name: "missing without reason",
			mutate: func(result *gitadapter.RevisionRevalidation) {
				result.State = gitadapter.RevisionStateMissing
				result.ActualCommitOID = ""
				result.Reasons = []gitadapter.Reason{}
			},
			want: "missing revalidation",
		},
	}
	modes := []struct {
		name    string
		request ReviewRequest
	}{
		{
			name: "selection",
			request: ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeSelection,
				Revision:       revision,
				SelectionPath:  "a.go",
				StartLine:      1,
				EndLine:        1,
			},
		},
		{
			name: "scope",
			request: ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeScope,
				Revision:       revision,
				Include:        []string{"**/*.go"},
				Exclude:        []string{},
			},
		},
	}
	for _, mode := range modes {
		for _, mutation := range mutations {
			t.Run(mode.name+"/"+mutation.name, func(t *testing.T) {
				source := &adversarialTargetSource{
					TargetSource:       adapter,
					mutateRevalidation: mutation.mutate,
				}
				_, err := Materialize(
					context.Background(),
					source,
					artifacts,
					mode.request,
					DefaultLocalConfig(),
				)
				if err == nil || !strings.Contains(err.Error(), mutation.want) {
					t.Fatalf(
						"Materialize(spoofed %s revalidation) error = %v, want %q",
						mode.name,
						err,
						mutation.want,
					)
				}
			})
		}
	}
}

func TestMaterializeScopeEnforcesAggregateMaterializedBytes(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	firstContent := "package fixture\n// retained\n"
	writeFixture(t, repositoryPath, "a.go", firstContent)
	writeFixture(t, repositoryPath, "b.go", "package fixture\n// skipped b\n")
	writeFixture(t, repositoryPath, "c.go", "package fixture\n// skipped c\n")
	revision := commitFixture(t, repositoryPath, "aggregate scope budget")
	source, artifacts := newMaterializeHarness(t)
	config := DefaultLocalConfig()
	config.MaxMaterializedBytes = int64(len(firstContent))

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeScope,
			Revision:       revision,
			Include:        []string{"**/*.go"},
			Exclude:        []string{},
		},
		config,
	)
	if err != nil {
		t.Fatalf("Materialize(aggregate scope budget) error = %v", err)
	}
	if materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		!hasMaterializationReason(
			materialized.Target.Snapshot.CompletenessReason,
			gitadapter.ReasonMaterializedBytesExceeded,
		) {
		t.Fatalf("aggregate-budget scope snapshot = %+v", materialized.Target.Snapshot)
	}
	if len(materialized.Input.Files) != 1 ||
		materialized.Input.Files[0].Path != "a.go" ||
		materialized.Input.Files[0].Content == nil ||
		*materialized.Input.Files[0].Content != firstContent {
		t.Fatalf("aggregate-budget review input = %+v", materialized.Input)
	}
	var inputBytes int64
	for _, file := range materialized.Input.Files {
		if file.Content != nil {
			inputBytes += int64(len(*file.Content))
		}
	}
	if inputBytes > config.MaxMaterializedBytes {
		t.Fatalf(
			"review input retained %d bytes, max_materialized_bytes=%d",
			inputBytes,
			config.MaxMaterializedBytes,
		)
	}
	if len(materialized.Target.FileRefs) != 3 {
		t.Fatalf("aggregate-budget file refs = %+v", materialized.Target.FileRefs)
	}
	for index, path := range []string{"b.go", "c.go"} {
		file := materialized.Target.FileRefs[index+1]
		if file.Path != path ||
			file.ContentRef != nil ||
			file.Completeness != gitadapter.CompletenessSkipped ||
			!hasMaterializationReason(
				file.Reasons,
				gitadapter.ReasonMaterializedBytesExceeded,
			) {
			t.Fatalf("aggregate-budget skipped file = %+v", file)
		}
	}
	scope := materialized.Target.Snapshot.Scope
	if scope == nil ||
		scope.MatchedFiles != 3 ||
		scope.IncludedFiles != 1 ||
		scope.SkippedFiles != 2 {
		t.Fatalf("aggregate-budget scope coverage = %+v", scope)
	}
}

func TestMaterializeDiffRetainsPatchAndSkipsFileBeyondAggregateBudget(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "review.go", "package fixture\n")
	baseRevision := commitFixture(t, repositoryPath, "aggregate diff base")
	writeFixture(
		t,
		repositoryPath,
		"review.go",
		"package fixture\n// changed content retained only as metadata\n",
	)
	headRevision := commitFixture(t, repositoryPath, "aggregate diff head")
	source, artifacts := newMaterializeHarness(t)
	capture, err := source.MaterializeDiff(
		context.Background(),
		gitadapter.Request{
			RepositoryPath: repositoryPath,
			BaseRevision:   baseRevision,
			HeadRevision:   headRevision,
			Limits: gitadapter.Limits{
				MaxPatchBytes: gitadapter.DefaultMaxPatchBytes,
				MaxFiles:      gitadapter.DefaultMaxFiles,
			},
		},
	)
	if err != nil {
		t.Fatalf("capture aggregate diff fixture: %v", err)
	}
	if !capture.Patch.Included || capture.Patch.SizeBytes < 1 {
		t.Fatalf("aggregate diff fixture patch = %+v", capture.Patch)
	}
	config := DefaultLocalConfig()
	config.MaxMaterializedBytes = capture.Patch.SizeBytes

	materialized, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeDiff,
			BaseRevision:   baseRevision,
			HeadRevision:   headRevision,
		},
		config,
	)
	if err != nil {
		t.Fatalf("Materialize(aggregate diff budget) error = %v", err)
	}
	if materialized.Target.PatchRef == nil ||
		materialized.Target.PatchRef.SizeBytes != capture.Patch.SizeBytes ||
		materialized.Input.CanonicalPatch != string(capture.Patch.Data) {
		t.Fatalf(
			"aggregate-budget patch = ref %+v input bytes %d",
			materialized.Target.PatchRef,
			len(materialized.Input.CanonicalPatch),
		)
	}
	if materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
		!hasMaterializationReason(
			materialized.Target.Snapshot.CompletenessReason,
			gitadapter.ReasonMaterializedBytesExceeded,
		) {
		t.Fatalf("aggregate-budget diff snapshot = %+v", materialized.Target.Snapshot)
	}
	if len(materialized.Target.FileRefs) != 1 ||
		materialized.Target.FileRefs[0].ContentRef != nil ||
		materialized.Target.FileRefs[0].Completeness != gitadapter.CompletenessSkipped ||
		!hasMaterializationReason(
			materialized.Target.FileRefs[0].Reasons,
			gitadapter.ReasonMaterializedBytesExceeded,
		) ||
		len(materialized.Input.Files) != 1 ||
		materialized.Input.Files[0].Path != materialized.Target.FileRefs[0].Path ||
		materialized.Input.Files[0].Content != nil {
		t.Fatalf(
			"aggregate-budget diff files = target %+v input %+v",
			materialized.Target.FileRefs,
			materialized.Input.Files,
		)
	}
	inputBytes := int64(len(materialized.Input.CanonicalPatch))
	for _, file := range materialized.Input.Files {
		if file.Content != nil {
			inputBytes += int64(len(*file.Content))
		}
	}
	if inputBytes > config.MaxMaterializedBytes {
		t.Fatalf(
			"diff review input retained %d bytes, max_materialized_bytes=%d",
			inputBytes,
			config.MaxMaterializedBytes,
		)
	}
}

func TestMaterializeSelectionRejectsAggregateMaterializedBytes(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	content := "package fixture\n// selection exceeds aggregate budget\n"
	writeFixture(t, repositoryPath, "review.go", content)
	revision := commitFixture(t, repositoryPath, "aggregate selection budget")
	source, artifacts := newMaterializeHarness(t)
	config := DefaultLocalConfig()
	config.MaxMaterializedBytes = int64(len(content) - 1)

	_, err := Materialize(
		context.Background(),
		source,
		artifacts,
		ReviewRequest{
			RepositoryPath: repositoryPath,
			Mode:           reviewcore.TargetModeSelection,
			Revision:       revision,
			SelectionPath:  "review.go",
			StartLine:      1,
			EndLine:        1,
		},
		config,
	)
	if !errors.Is(err, ErrTargetNotReviewable) ||
		!strings.Contains(err.Error(), "max_materialized_bytes") {
		t.Fatalf("Materialize(aggregate selection budget) error = %v", err)
	}
}

func TestMaterializeRejectsDeniedModeBeforeCallingSource(t *testing.T) {
	config := DefaultLocalConfig()
	config.AllowedModes = []reviewcore.TargetMode{reviewcore.TargetModeScope}
	_, artifacts := newMaterializeHarness(t)

	_, err := Materialize(
		context.Background(),
		panicTargetSource{},
		artifacts,
		ReviewRequest{
			RepositoryPath: t.TempDir(),
			Mode:           reviewcore.TargetModeDiff,
			BaseRevision:   "main^",
			HeadRevision:   "main",
		},
		config,
	)
	if err == nil ||
		!strings.Contains(err.Error(), `target mode "diff" is denied`) {
		t.Fatalf("Materialize(denied mode) error = %v", err)
	}
}

func TestMaterializeAppliesResolvedTargetPolicy(t *testing.T) {
	t.Run("selection", func(t *testing.T) {
		repositoryPath := newMaterializeRepository(t)
		writeFixture(t, repositoryPath, "src/ok.go", "package fixture\n")
		writeFixture(t, repositoryPath, "src/generated/no.go", "package generated\n")
		revision := commitFixture(t, repositoryPath, "selection target policy")
		source, artifacts := newMaterializeHarness(t)
		config := DefaultLocalConfig()
		config.TargetInclude = []string{"src/**"}
		config.TargetExclude = []string{"src/generated/**"}

		if _, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeSelection,
				Revision:       revision,
				SelectionPath:  "src/ok.go",
				StartLine:      1,
				EndLine:        1,
			},
			config,
		); err != nil {
			t.Fatalf("Materialize(policy-allowed selection) error = %v", err)
		}
		_, err := Materialize(
			context.Background(),
			panicTargetSource{},
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeSelection,
				Revision:       revision,
				SelectionPath:  "src/generated/no.go",
				StartLine:      1,
				EndLine:        1,
			},
			config,
		)
		if !errors.Is(err, ErrTargetNotReviewable) ||
			!strings.Contains(err.Error(), "denied by target policy") {
			t.Fatalf("Materialize(policy-denied selection) error = %v", err)
		}
	})

	t.Run("scope", func(t *testing.T) {
		repositoryPath := newMaterializeRepository(t)
		writeFixture(t, repositoryPath, "outside.go", "package outside\n")
		writeFixture(t, repositoryPath, "src/generated/no.go", "package generated\n")
		writeFixture(t, repositoryPath, "src/ok.go", "package fixture\n")
		revision := commitFixture(t, repositoryPath, "scope target policy")
		source, artifacts := newMaterializeHarness(t)
		config := DefaultLocalConfig()
		config.TargetInclude = []string{"src/**"}
		config.TargetExclude = []string{"src/generated/**"}

		materialized, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeScope,
				Revision:       revision,
				Include:        []string{"**/*.go"},
				Exclude:        []string{},
			},
			config,
		)
		if err != nil {
			t.Fatalf("Materialize(policy-filtered scope) error = %v", err)
		}
		if len(materialized.Input.Files) != 1 ||
			materialized.Input.Files[0].Path != "src/ok.go" ||
			materialized.Target.Snapshot.Completeness != gitadapter.CompletenessComplete ||
			len(materialized.Target.FileRefs) != 1 ||
			materialized.Target.FileRefs[0].Path != "src/ok.go" {
			t.Fatalf(
				"policy-filtered scope = target %+v input %+v",
				materialized.Target,
				materialized.Input,
			)
		}
		var manifest ScopeManifest
		if err := artifacts.ReadJSONArtifact(
			materialized.Target.ManifestRef,
			&manifest,
		); err != nil {
			t.Fatalf("read policy-filtered scope manifest: %v", err)
		}
		if manifest.Coverage.ScannedFiles != 3 ||
			manifest.Coverage.MatchedFiles != 1 ||
			manifest.Coverage.ExcludedFiles != 2 ||
			manifest.Coverage.RequestExcludedFiles != 0 ||
			manifest.Coverage.PolicyExcludedFiles != 2 {
			t.Fatalf("policy-filtered scope coverage = %+v", manifest.Coverage)
		}
	})

	t.Run("scope request and policy exclusions precede quota", func(t *testing.T) {
		repositoryPath := newMaterializeRepository(t)
		writeFixture(
			t,
			repositoryPath,
			"review/a-request-denied/no.go",
			"package denied\n",
		)
		writeFixture(
			t,
			repositoryPath,
			"review/b-policy-denied/no.go",
			"package denied\n",
		)
		writeFixture(
			t,
			repositoryPath,
			"review/z-allowed/ok.go",
			"package allowed\n",
		)
		revision := commitFixture(t, repositoryPath, "pre-quota scope policy")
		source, artifacts := newMaterializeHarness(t)
		config := DefaultLocalConfig()
		config.TargetInclude = []string{"review/**"}
		config.TargetExclude = []string{"review/b-policy-denied/**"}

		materialized, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeScope,
				Revision:       revision,
				Include:        []string{"review/**"},
				Exclude:        []string{"review/a-request-denied/**"},
				MaxFiles:       1,
			},
			config,
		)
		if err != nil {
			t.Fatalf("Materialize(pre-quota scope policy) error = %v", err)
		}
		if materialized.Target.Snapshot.Completeness !=
			gitadapter.CompletenessComplete ||
			len(materialized.Input.Files) != 1 ||
			materialized.Input.Files[0].Path != "review/z-allowed/ok.go" {
			t.Fatalf("pre-quota scope materialization = %+v", materialized)
		}
		var manifest ScopeManifest
		if err := artifacts.ReadJSONArtifact(
			materialized.Target.ManifestRef,
			&manifest,
		); err != nil {
			t.Fatalf("read pre-quota scope manifest: %v", err)
		}
		if manifest.Coverage.MatchedFiles != 1 ||
			manifest.Coverage.IncludedFiles != 1 ||
			manifest.Coverage.SkippedFiles != 0 ||
			manifest.Coverage.ExcludedFiles != 2 ||
			manifest.Coverage.RequestExcludedFiles != 1 ||
			manifest.Coverage.PolicyExcludedFiles != 1 {
			t.Fatalf("pre-quota scope coverage = %+v", manifest.Coverage)
		}
	})

	t.Run("diff denied path", func(t *testing.T) {
		repositoryPath := newMaterializeRepository(t)
		writeFixture(t, repositoryPath, "docs/no.go", "package docs\n")
		baseRevision := commitFixture(t, repositoryPath, "diff policy base")
		writeFixture(t, repositoryPath, "docs/no.go", "package docs\n// changed\n")
		headRevision := commitFixture(t, repositoryPath, "diff policy head")
		source, artifacts := newMaterializeHarness(t)
		config := DefaultLocalConfig()
		config.TargetExclude = []string{"docs/**"}

		_, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeDiff,
				BaseRevision:   baseRevision,
				HeadRevision:   headRevision,
			},
			config,
		)
		if !errors.Is(err, ErrTargetNotReviewable) ||
			!strings.Contains(err.Error(), `"docs/no.go" is denied`) {
			t.Fatalf("Materialize(policy-denied diff) error = %v", err)
		}
	})

	t.Run("diff file limit derives exact partial patch", func(t *testing.T) {
		repositoryPath := newMaterializeRepository(t)
		for _, path := range []string{"a.go", "b.go"} {
			writeFixture(t, repositoryPath, path, "package fixture\n")
		}
		baseRevision := commitFixture(t, repositoryPath, "limited diff policy base")
		for _, path := range []string{"a.go", "b.go"} {
			writeFixture(t, repositoryPath, path, "package fixture\n// changed\n")
		}
		headRevision := commitFixture(t, repositoryPath, "limited diff policy head")
		source, artifacts := newMaterializeHarness(t)
		config := DefaultLocalConfig()
		config.TargetInclude = []string{"**/*.go"}

		materialized, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeDiff,
				BaseRevision:   baseRevision,
				HeadRevision:   headRevision,
				MaxFiles:       1,
			},
			config,
		)
		if err != nil {
			t.Fatalf("Materialize(restricted partial diff) error = %v", err)
		}
		if materialized.Target.Snapshot.Completeness != gitadapter.CompletenessPartial ||
			len(materialized.Target.FileRefs) != 1 ||
			materialized.Target.FileRefs[0].Path != "a.go" ||
			strings.Contains(materialized.Input.CanonicalPatch, "b.go") {
			t.Fatalf("restricted partial diff = %+v", materialized)
		}
	})
}

func TestMaterializeDiffRejectsSourceThatExceedsEffectiveLimits(t *testing.T) {
	t.Run("patch bytes", func(t *testing.T) {
		repositoryPath := newMaterializeRepository(t)
		writeFixture(t, repositoryPath, "review.go", "package fixture\n")
		baseRevision := commitFixture(t, repositoryPath, "patch limit base")
		writeFixture(
			t,
			repositoryPath,
			"review.go",
			"package fixture\n"+strings.Repeat("// changed content\n", 128),
		)
		headRevision := commitFixture(t, repositoryPath, "patch limit head")
		adapter, artifacts := newMaterializeHarness(t)
		full, err := adapter.MaterializeDiff(
			context.Background(),
			gitadapter.Request{
				RepositoryPath: repositoryPath,
				BaseRevision:   baseRevision,
				HeadRevision:   headRevision,
				Limits: gitadapter.Limits{
					MaxPatchBytes: gitadapter.DefaultMaxPatchBytes,
					MaxFiles:      gitadapter.DefaultMaxFiles,
				},
			},
		)
		if err != nil {
			t.Fatalf("capture patch limit fixture: %v", err)
		}
		effectiveLimit := full.Patch.SizeBytes - 1
		if !full.Patch.Included || effectiveLimit < 1 {
			t.Fatalf("patch limit fixture = %+v", full.Patch)
		}
		source := &adversarialTargetSource{
			TargetSource: adapter,
			mutateDiffRequest: func(request *gitadapter.Request) {
				request.Limits.MaxPatchBytes = gitadapter.DefaultMaxPatchBytes
			},
		}

		_, err = Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeDiff,
				BaseRevision:   baseRevision,
				HeadRevision:   headRevision,
				MaxPatchBytes:  effectiveLimit,
			},
			DefaultLocalConfig(),
		)
		if err == nil ||
			!strings.Contains(err.Error(), "canonical patch sha256/size/data") {
			t.Fatalf("Materialize(source exceeded patch limit) error = %v", err)
		}
	})

	t.Run("max files", func(t *testing.T) {
		repositoryPath := newMaterializeRepository(t)
		for _, path := range []string{"a.go", "b.go"} {
			writeFixture(t, repositoryPath, path, "package fixture\n")
		}
		baseRevision := commitFixture(t, repositoryPath, "file limit base")
		for _, path := range []string{"a.go", "b.go"} {
			writeFixture(t, repositoryPath, path, "package fixture\n// changed\n")
		}
		headRevision := commitFixture(t, repositoryPath, "file limit head")
		adapter, artifacts := newMaterializeHarness(t)
		source := &adversarialTargetSource{
			TargetSource: adapter,
			mutateDiffRequest: func(request *gitadapter.Request) {
				request.Limits.MaxFiles = gitadapter.DefaultMaxFiles
			},
		}

		_, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeDiff,
				BaseRevision:   baseRevision,
				HeadRevision:   headRevision,
				MaxFiles:       1,
			},
			DefaultLocalConfig(),
		)
		if err == nil ||
			!strings.Contains(err.Error(), "exceeds admitted max_files=1") {
			t.Fatalf("Materialize(source exceeded file limit) error = %v", err)
		}
	})
}

func TestMaterializeRejectsUnknownDirtyStateWithoutReason(t *testing.T) {
	repositoryPath := newMaterializeRepository(t)
	writeFixture(t, repositoryPath, "a.go", "package fixture\n")
	baseRevision := commitFixture(t, repositoryPath, "unknown dirty base")
	writeFixture(t, repositoryPath, "a.go", "package fixture\n// changed\n")
	headRevision := commitFixture(t, repositoryPath, "unknown dirty head")
	adapter, artifacts := newMaterializeHarness(t)

	t.Run("diff", func(t *testing.T) {
		source := &adversarialTargetSource{
			TargetSource: adapter,
			mutateDiffResult: func(result *gitadapter.Result) {
				result.Snapshot.DirtyState = gitadapter.DirtyStateUnknown
				result.Snapshot.Completeness = gitadapter.CompletenessComplete
				result.Snapshot.CompletenessReason = []gitadapter.Reason{}
				result.Completeness = gitadapter.CompletenessComplete
				result.Reasons = []gitadapter.Reason{}
				digest, err := gitadapter.DigestTargetSnapshot(*result.Snapshot)
				if err != nil {
					t.Fatalf("DigestTargetSnapshot(unknown dirty diff): %v", err)
				}
				result.Snapshot.SHA256 = digest
			},
		}

		_, err := Materialize(
			context.Background(),
			source,
			artifacts,
			ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           reviewcore.TargetModeDiff,
				BaseRevision:   baseRevision,
				HeadRevision:   headRevision,
			},
			DefaultLocalConfig(),
		)
		if err == nil ||
			!strings.Contains(err.Error(), "unknown dirty_state requires") {
			t.Fatalf("Materialize(diff unknown dirty without reason) error = %v", err)
		}
	})

	for _, mode := range []reviewcore.TargetMode{
		reviewcore.TargetModeSelection,
		reviewcore.TargetModeScope,
	} {
		t.Run(string(mode), func(t *testing.T) {
			source := &adversarialTargetSource{
				TargetSource: adapter,
				mutateCapture: func(capture *gitadapter.RevisionCapture) {
					capture.DirtyState = gitadapter.DirtyStateUnknown
					capture.Reasons = []gitadapter.Reason{}
				},
			}
			request := ReviewRequest{
				RepositoryPath: repositoryPath,
				Mode:           mode,
				Revision:       headRevision,
			}
			if mode == reviewcore.TargetModeSelection {
				request.SelectionPath = "a.go"
				request.StartLine = 1
				request.EndLine = 1
			} else {
				request.Include = []string{"**/*.go"}
				request.Exclude = []string{}
			}

			_, err := Materialize(
				context.Background(),
				source,
				artifacts,
				request,
				DefaultLocalConfig(),
			)
			if err == nil ||
				!strings.Contains(err.Error(), "unknown dirty_state requires") {
				t.Fatalf(
					"Materialize(%s unknown dirty without reason) error = %v",
					mode,
					err,
				)
			}
		})
	}
}

type adversarialTargetSource struct {
	TargetSource
	mutateDiffRequest    func(*gitadapter.Request)
	mutateDiffResult     func(*gitadapter.Result)
	mutateCapture        func(*gitadapter.RevisionCapture)
	mutateScopeArguments func([]string, []string)
	mutateScopeAdmission func(*gitadapter.ScopeAdmission)
	mutateListing        func(*gitadapter.RevisionFileList)
	mutateContent        func(*gitadapter.FileContent)
	mutateRevalidation   func(*gitadapter.RevisionRevalidation)
}

func (source *adversarialTargetSource) MaterializeDiff(
	ctx context.Context,
	request gitadapter.Request,
) (gitadapter.Result, error) {
	if source.mutateDiffRequest != nil {
		source.mutateDiffRequest(&request)
	}
	result, err := source.TargetSource.MaterializeDiff(ctx, request)
	if err == nil && source.mutateDiffResult != nil {
		source.mutateDiffResult(&result)
	}
	return result, err
}

func (source *adversarialTargetSource) CaptureRevision(
	ctx context.Context,
	request gitadapter.RevisionRequest,
) (gitadapter.RevisionCapture, error) {
	capture, err := source.TargetSource.CaptureRevision(ctx, request)
	if err == nil && source.mutateCapture != nil {
		source.mutateCapture(&capture)
	}
	return capture, err
}

func (source *adversarialTargetSource) RevalidateRevision(
	ctx context.Context,
	repositoryRoot string,
	requested string,
	expectedCommitOID string,
) (gitadapter.RevisionRevalidation, error) {
	result, err := source.TargetSource.RevalidateRevision(
		ctx,
		repositoryRoot,
		requested,
		expectedCommitOID,
	)
	if err == nil && source.mutateRevalidation != nil {
		source.mutateRevalidation(&result)
	}
	return result, err
}

func (source *adversarialTargetSource) ListFilesAtCommit(
	ctx context.Context,
	repositoryRoot string,
	commitOID string,
	include []string,
	exclude []string,
	maxFiles int,
) (gitadapter.RevisionFileList, error) {
	if source.mutateScopeArguments != nil {
		source.mutateScopeArguments(include, exclude)
	}
	listing, err := source.TargetSource.ListFilesAtCommit(
		ctx,
		repositoryRoot,
		commitOID,
		include,
		exclude,
		maxFiles,
	)
	if err == nil && source.mutateListing != nil {
		source.mutateListing(&listing)
	}
	return listing, err
}

func (source *adversarialTargetSource) ListFilesAtCommitWithAdmission(
	ctx context.Context,
	repositoryRoot string,
	commitOID string,
	admission gitadapter.ScopeAdmission,
	maxFiles int,
) (gitadapter.RevisionFileList, error) {
	if source.mutateScopeArguments != nil {
		source.mutateScopeArguments(admission.Include, admission.Exclude)
	}
	if source.mutateScopeAdmission != nil {
		source.mutateScopeAdmission(&admission)
	}
	delegate, ok := source.TargetSource.(scopeAdmissionTargetSource)
	if !ok {
		return gitadapter.RevisionFileList{}, fmt.Errorf(
			"wrapped source does not support scope admission",
		)
	}
	listing, err := delegate.ListFilesAtCommitWithAdmission(
		ctx,
		repositoryRoot,
		commitOID,
		admission,
		maxFiles,
	)
	if err == nil && source.mutateListing != nil {
		source.mutateListing(&listing)
	}
	return listing, err
}

func (source *adversarialTargetSource) ReadFileAtCommit(
	ctx context.Context,
	repositoryRoot string,
	commitOID string,
	path string,
	maxBytes int64,
) (gitadapter.FileContent, error) {
	content, err := source.TargetSource.ReadFileAtCommit(
		ctx,
		repositoryRoot,
		commitOID,
		path,
		maxBytes,
	)
	if err == nil && source.mutateContent != nil {
		source.mutateContent(&content)
	}
	return content, err
}

type panicTargetSource struct{}

func (panicTargetSource) MaterializeDiff(
	context.Context,
	gitadapter.Request,
) (gitadapter.Result, error) {
	panic("source called after request exceeded local hard limit")
}

func (panicTargetSource) CaptureRevision(
	context.Context,
	gitadapter.RevisionRequest,
) (gitadapter.RevisionCapture, error) {
	panic("source called after request exceeded local hard limit")
}

func (panicTargetSource) RevalidateRevision(
	context.Context,
	string,
	string,
	string,
) (gitadapter.RevisionRevalidation, error) {
	panic("source called after request exceeded local hard limit")
}

func (panicTargetSource) ListFilesAtCommit(
	context.Context,
	string,
	string,
	[]string,
	[]string,
	int,
) (gitadapter.RevisionFileList, error) {
	panic("source called after request exceeded local hard limit")
}

func (panicTargetSource) ReadFileAtCommit(
	context.Context,
	string,
	string,
	string,
	int64,
) (gitadapter.FileContent, error) {
	panic("source called after request exceeded local hard limit")
}

type afterCaptureSource struct {
	TargetSource
	afterCapture func()
}

func (source *afterCaptureSource) CaptureRevision(
	ctx context.Context,
	request gitadapter.RevisionRequest,
) (gitadapter.RevisionCapture, error) {
	capture, err := source.TargetSource.CaptureRevision(ctx, request)
	if err == nil && source.afterCapture != nil {
		callback := source.afterCapture
		source.afterCapture = nil
		callback()
	}
	return capture, err
}

func (source *afterCaptureSource) ListFilesAtCommitWithAdmission(
	ctx context.Context,
	repositoryRoot string,
	commitOID string,
	admission gitadapter.ScopeAdmission,
	maxFiles int,
) (gitadapter.RevisionFileList, error) {
	delegate, ok := source.TargetSource.(scopeAdmissionTargetSource)
	if !ok {
		return gitadapter.RevisionFileList{}, fmt.Errorf(
			"wrapped source does not support scope admission",
		)
	}
	return delegate.ListFilesAtCommitWithAdmission(
		ctx,
		repositoryRoot,
		commitOID,
		admission,
		maxFiles,
	)
}

func newMaterializeHarness(
	t *testing.T,
) (*gitadapter.Adapter, *runrepo.Repository) {
	t.Helper()
	source, err := gitadapter.New()
	if err != nil {
		t.Fatalf("gitadapter.New() error = %v", err)
	}
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatalf("local.Open() error = %v", err)
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		t.Fatalf("runrepo.New() error = %v", err)
	}
	return source, artifacts
}

func newMaterializeRepository(t *testing.T) string {
	t.Helper()
	repositoryPath := t.TempDir()
	runGit(t, repositoryPath, "init", "-q", "-b", "main")
	runGit(t, repositoryPath, "config", "user.name", "Argus Test")
	runGit(t, repositoryPath, "config", "user.email", "argus@example.invalid")
	runGit(t, repositoryPath, "config", "commit.gpgsign", "false")
	return repositoryPath
}

func hasMaterializationReason(
	reasons []gitadapter.Reason,
	code gitadapter.ReasonCode,
) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}
