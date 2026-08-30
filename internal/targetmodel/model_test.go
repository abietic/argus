package targetmodel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
)

func TestDecodeMaterializedTargetRejectsUnknownDuplicateAndTrailingJSON(t *testing.T) {
	target := validSelectionTarget(t)
	data, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeMaterializedTarget(data); err != nil ||
		decoded.Snapshot.TargetSnapshotID != target.Snapshot.TargetSnapshotID {
		t.Fatalf("DecodeMaterializedTarget(valid) = %+v, %v", decoded, err)
	}
	unknown := []byte(strings.TrimSuffix(string(data), "}") + `,"unknown":true}`)
	if _, err := DecodeMaterializedTarget(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	duplicate := []byte(strings.TrimSuffix(string(data), "}") +
		`,"schema_version":"argus.materialized_target.v1alpha1"}`)
	if _, err := DecodeMaterializedTarget(duplicate); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON field") {
		t.Fatalf("duplicate field error = %v", err)
	}
	if _, err := DecodeMaterializedTarget(append(data, []byte(` {}`)...)); err == nil ||
		!strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing JSON error = %v", err)
	}
	nullContexts := []byte(strings.Replace(
		string(data),
		`"contexts":[]`,
		`"contexts":null`,
		1,
	))
	if _, err := DecodeMaterializedTarget(nullContexts); err == nil ||
		!strings.Contains(err.Error(), "contexts must be an explicit array") {
		t.Fatalf("null contexts error = %v", err)
	}
}

func TestValidateScopePatternUsesExecutableGrammar(t *testing.T) {
	for _, pattern := range []string{"**", "src/*.go", "源码/**/*.go"} {
		if err := ValidateScopePattern("scope", pattern); err != nil {
			t.Fatalf("ValidateScopePattern(%q) error = %v", pattern, err)
		}
	}
	for _, pattern := range []string{
		"src/***",
		"src/?.go",
		"src/[ab].go",
		strings.Repeat("a", gitadapter.MaxScopePatternBytes+1),
	} {
		if err := ValidateScopePattern("scope", pattern); err == nil {
			t.Fatalf("ValidateScopePattern(%q) accepted unsupported grammar", pattern)
		}
	}
}

func TestMaterializedSelectionRejectsFileRefIdentityTampering(t *testing.T) {
	tests := []struct {
		name string
		edit func(*MaterializedTarget)
	}{
		{
			name: "path",
			edit: func(target *MaterializedTarget) {
				target.FileRefs[0].Path = "other.go"
			},
		},
		{
			name: "digest",
			edit: func(target *MaterializedTarget) {
				digest := strings.Repeat("e", 64)
				ref := testTargetArtifactRef(
					ContractFileContent,
					digest,
					target.FileRefs[0].SizeBytes,
				)
				target.FileRefs[0].SHA256 = digest
				target.FileRefs[0].ContentRef = &ref
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := validSelectionTarget(t)
			test.edit(&target)
			err := target.Validate()
			if err == nil ||
				!strings.Contains(err.Error(), "selection file_ref does not bind") {
				t.Fatalf("MaterializedTarget.Validate() error = %v", err)
			}
		})
	}
}

func TestMaterializedTargetRejectsImplicitContextArray(t *testing.T) {
	target := validSelectionTarget(t)
	target.Contexts = nil
	if err := target.Validate(); err == nil ||
		!strings.Contains(err.Error(), "contexts must be an explicit array") {
		t.Fatalf("MaterializedTarget.Validate() error = %v", err)
	}
}

func TestMaterializedScopeRejectsIncludedCountAndIncompleteContentRef(t *testing.T) {
	t.Run("included count", func(t *testing.T) {
		target := validScopeTarget(t)
		target.FileRefs[0].Completeness = gitadapter.CompletenessSkipped
		target.FileRefs[0].ContentRef = nil
		target.FileRefs[0].Reasons = []gitadapter.Reason{{
			Code: gitadapter.ReasonFileSizeExceeded,
		}}

		err := target.Validate()
		if err == nil ||
			!strings.Contains(err.Error(), "complete file_refs do not match included coverage") {
			t.Fatalf("MaterializedTarget.Validate() error = %v", err)
		}
	})

	t.Run("incomplete content ref", func(t *testing.T) {
		target := validScopeTarget(t)
		ref := testTargetArtifactRef(
			ContractFileContent,
			target.FileRefs[1].SHA256,
			target.FileRefs[1].SizeBytes,
		)
		target.FileRefs[1].ContentRef = &ref

		err := target.Validate()
		if err == nil ||
			!strings.Contains(err.Error(), "incomplete file_ref") {
			t.Fatalf("MaterializedTarget.Validate() error = %v", err)
		}
	})
}

func TestScopeManifestRejectsCoverageAndFileTampering(t *testing.T) {
	target := validScopeTarget(t)
	if err := validScopeManifest().ValidateAgainst(target.Snapshot); err != nil {
		t.Fatalf("valid ScopeManifest.ValidateAgainst() error = %v", err)
	}
	tests := []struct {
		name string
		edit func(*ScopeManifest)
		want string
	}{
		{
			name: "snapshot coverage mismatch",
			edit: func(manifest *ScopeManifest) {
				manifest.Coverage.IncludedFiles = 0
				manifest.Coverage.SkippedFiles = 2
			},
			want: "coverage does not match target snapshot",
		},
		{
			name: "retained file coverage",
			edit: func(manifest *ScopeManifest) {
				manifest.Files = []ScopeManifestFile{}
			},
			want: "retained files do not match coverage",
		},
		{
			name: "exclusion coverage",
			edit: func(manifest *ScopeManifest) {
				manifest.Coverage.PolicyExcludedFiles = 1
			},
			want: "coverage is inconsistent",
		},
		{
			name: "Git identity",
			edit: func(manifest *ScopeManifest) {
				manifest.Files[0].ObjectOID = strings.Repeat("z", 40)
			},
			want: "invalid Git identity",
		},
		{
			name: "file digest",
			edit: func(manifest *ScopeManifest) {
				manifest.Files[0].SHA256 = "not-a-digest"
			},
			want: "lowercase SHA-256 digest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validScopeManifest()
			test.edit(&manifest)
			err := manifest.ValidateAgainst(target.Snapshot)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ScopeManifest.ValidateAgainst() error = %v, want %q",
					err, test.want)
			}
		})
	}
}

func TestSelectionManifestRejectsSnapshotFieldTampering(t *testing.T) {
	target := validSelectionTarget(t)
	manifest := validSelectionManifest()
	if err := manifest.ValidateAgainst(target.Snapshot); err != nil {
		t.Fatalf("valid SelectionManifest.ValidateAgainst() error = %v", err)
	}
	for name, edit := range map[string]func(*SelectionManifest){
		"path": func(manifest *SelectionManifest) {
			manifest.Path = "other.go"
		},
		"digest": func(manifest *SelectionManifest) {
			manifest.FileSHA256 = strings.Repeat("e", 64)
		},
		"effective ranges": func(manifest *SelectionManifest) {
			manifest.EffectiveRanges = []SelectionRange{{
				StartLine: 2,
				EndLine:   3,
			}}
		},
		"symbol selector": func(manifest *SelectionManifest) {
			manifest.Symbol = &SymbolSelector{
				Language: "go", Kind: "function", QualifiedName: "Review",
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			tampered := validSelectionManifest()
			edit(&tampered)
			err := tampered.ValidateAgainst(target.Snapshot)
			if err == nil ||
				!strings.Contains(err.Error(), "does not exactly match target snapshot") {
				t.Fatalf("SelectionManifest.ValidateAgainst() error = %v", err)
			}
		})
	}
}

func validSelectionTarget(t *testing.T) MaterializedTarget {
	t.Helper()
	fileDigest := strings.Repeat("a", 64)
	selectionDigest := strings.Repeat("b", 64)
	manifestDigest := strings.Repeat("c", 64)
	snapshot, err := SealTargetSnapshot(TargetSnapshot{
		SchemaVersion: TargetSnapshotSchemaVersion,
		Mode:          reviewcore.TargetModeSelection,
		Repository: gitadapter.RepositorySnapshot{
			Kind: "local_git", RepositoryID: "repository-1", ObjectFormat: "sha1",
		},
		Base:           testRevision(),
		Head:           testRevision(),
		ManifestSHA256: manifestDigest,
		Selection: &SelectionSnapshot{
			Path:                   "review.go",
			StartLine:              2,
			EndLine:                2,
			SourceKind:             SelectionSourceCommit,
			FileSHA256:             fileDigest,
			FileSizeBytes:          20,
			SelectionContentSHA256: selectionDigest,
			SelectionSizeBytes:     10,
		},
		DirtyState:         gitadapter.DirtyStateClean,
		Completeness:       gitadapter.CompletenessComplete,
		CompletenessReason: []gitadapter.Reason{},
		CapturedAt:         testCapturedAt(),
		CapturedBy:         "argus-test",
		GitVersion:         "git version test",
	})
	if err != nil {
		t.Fatalf("SealTargetSnapshot(selection) error = %v", err)
	}
	fileRef := testTargetArtifactRef(ContractFileContent, fileDigest, 20)
	selectionRef := testTargetArtifactRef(
		ContractSelectionContent,
		selectionDigest,
		10,
	)
	target := MaterializedTarget{
		SchemaVersion:       MaterializedTargetSchemaVersion,
		Snapshot:            snapshot,
		ManifestRef:         testTargetArtifactRef(ContractSelectionManifest, manifestDigest, 100),
		SelectionContentRef: &selectionRef,
		Contexts:            []reviewcore.ContextBinding{},
		FileRefs: []TargetFileRef{{
			Path:         "review.go",
			SHA256:       fileDigest,
			SizeBytes:    20,
			ContentRef:   &fileRef,
			Completeness: gitadapter.CompletenessComplete,
			Reasons:      []gitadapter.Reason{},
		}},
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("valid selection target error = %v", err)
	}
	return target
}

func validScopeTarget(t *testing.T) MaterializedTarget {
	t.Helper()
	manifestDigest := strings.Repeat("c", 64)
	reasons := []gitadapter.Reason{{
		Code:   gitadapter.ReasonFileSizeExceeded,
		Detail: "b.go exceeded the retained content limit",
	}}
	snapshot, err := SealTargetSnapshot(TargetSnapshot{
		SchemaVersion: TargetSnapshotSchemaVersion,
		Mode:          reviewcore.TargetModeScope,
		Repository: gitadapter.RepositorySnapshot{
			Kind: "local_git", RepositoryID: "repository-1", ObjectFormat: "sha1",
		},
		Base:           testRevision(),
		Head:           testRevision(),
		ManifestSHA256: manifestDigest,
		Scope: &ScopeSnapshot{
			Include:       []string{"**/*.go"},
			Exclude:       []string{},
			MatchedFiles:  2,
			IncludedFiles: 1,
			SkippedFiles:  1,
		},
		DirtyState:         gitadapter.DirtyStateClean,
		Completeness:       gitadapter.CompletenessPartial,
		CompletenessReason: reasons,
		CapturedAt:         testCapturedAt(),
		CapturedBy:         "argus-test",
		GitVersion:         "git version test",
	})
	if err != nil {
		t.Fatalf("SealTargetSnapshot(scope) error = %v", err)
	}
	completeDigest := strings.Repeat("a", 64)
	completeRef := testTargetArtifactRef(
		ContractFileContent,
		completeDigest,
		10,
	)
	target := MaterializedTarget{
		SchemaVersion: MaterializedTargetSchemaVersion,
		Snapshot:      snapshot,
		ManifestRef:   testTargetArtifactRef(ContractScopeManifest, manifestDigest, 200),
		Contexts:      []reviewcore.ContextBinding{},
		FileRefs: []TargetFileRef{
			{
				Path:         "a.go",
				SHA256:       completeDigest,
				SizeBytes:    10,
				ContentRef:   &completeRef,
				Completeness: gitadapter.CompletenessComplete,
				Reasons:      []gitadapter.Reason{},
			},
			{
				Path:         "b.go",
				SHA256:       strings.Repeat("b", 64),
				SizeBytes:    100,
				Completeness: gitadapter.CompletenessPartial,
				Reasons:      reasons,
			},
		},
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("valid scope target error = %v", err)
	}
	return target
}

func validSelectionManifest() SelectionManifest {
	return SelectionManifest{
		SchemaVersion:          ContractSelectionManifest,
		Repository:             testRepository(),
		Revision:               testRevision(),
		Path:                   "review.go",
		StartLine:              2,
		EndLine:                2,
		SourceKind:             SelectionSourceCommit,
		FileSHA256:             strings.Repeat("a", 64),
		FileSizeBytes:          20,
		SelectionContentSHA256: strings.Repeat("b", 64),
		SelectionSizeBytes:     10,
		Completeness:           gitadapter.CompletenessComplete,
		Reasons:                []gitadapter.Reason{},
	}
}

func validScopeManifest() ScopeManifest {
	reasons := []gitadapter.Reason{{
		Code:   gitadapter.ReasonFileSizeExceeded,
		Detail: "b.go exceeded the retained content limit",
	}}
	return ScopeManifest{
		SchemaVersion: ContractScopeManifest,
		Repository:    testRepository(),
		Revision:      testRevision(),
		Include:       []string{"**/*.go"},
		Exclude:       []string{},
		Files: []ScopeManifestFile{
			{
				Path:         "a.go",
				Mode:         "100644",
				ObjectType:   "blob",
				ObjectOID:    strings.Repeat("e", 40),
				Language:     "go",
				SHA256:       strings.Repeat("a", 64),
				SizeBytes:    10,
				Completeness: gitadapter.CompletenessComplete,
				Reasons:      []gitadapter.Reason{},
			},
			{
				Path:         "b.go",
				Mode:         "100644",
				ObjectType:   "blob",
				ObjectOID:    strings.Repeat("f", 40),
				Language:     "go",
				SHA256:       strings.Repeat("b", 64),
				SizeBytes:    100,
				Completeness: gitadapter.CompletenessPartial,
				Reasons:      reasons,
			},
		},
		Coverage: ScopeCoverage{
			ScannedFiles:  2,
			MatchedFiles:  2,
			IncludedFiles: 1,
			SkippedFiles:  1,
		},
		Completeness: gitadapter.CompletenessPartial,
		Reasons:      reasons,
	}
}

func testRepository() gitadapter.RepositorySnapshot {
	return gitadapter.RepositorySnapshot{
		Kind: "local_git", RepositoryID: "repository-1", ObjectFormat: "sha1",
	}
}

func testRevision() gitadapter.RevisionSnapshot {
	return gitadapter.RevisionSnapshot{
		Requested: "main",
		CommitOID: strings.Repeat("d", 40),
	}
}

func testCapturedAt() time.Time {
	return time.Date(2026, time.July, 27, 1, 0, 0, 0, time.UTC)
}

func testTargetArtifactRef(
	contract string,
	digest string,
	size int64,
) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI:       "artifact://local/sha256/" + digest,
		SHA256:    digest,
		SizeBytes: size,
		Contract:  contract,
	}
}
