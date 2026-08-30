// Package targetmodel owns the immutable, mode-aware review target contract.
// It is shared by application materialization and persistence closure checks so
// infrastructure never needs to import the application layer.
package targetmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/source/gitadapter"
)

const (
	MaterializedTargetSchemaVersion = "argus.materialized_target.v1alpha1"
	TargetSnapshotSchemaVersion     = "argus.target_snapshot.v1alpha1"

	ContractChangeManifest     = "argus.git_change_manifest.v1alpha1"
	ContractSelectionManifest  = "argus.git_selection_manifest.v1alpha1"
	ContractScopeManifest      = "argus.git_scope_manifest.v1alpha1"
	ContractCanonicalPatch     = "argus.git_patch.v1alpha1"
	ContractSelectionContent   = "argus.selection_content.v1alpha1"
	ContractFileContent        = "argus.git_file_content.v1alpha1"
	ContractMaterializedTarget = MaterializedTargetSchemaVersion

	SelectionSourceCommit  = "commit"
	SelectionSourceOverlay = "overlay"
)

// TargetSnapshot is the immutable authorization boundary consumed by the
// review workflow. Selection and scope bind Base and Head to one exact commit.
type TargetSnapshot struct {
	SchemaVersion      string                        `json:"schema_version"`
	TargetSnapshotID   string                        `json:"target_snapshot_id"`
	SHA256             string                        `json:"sha256"`
	Mode               reviewcore.TargetMode         `json:"mode"`
	Repository         gitadapter.RepositorySnapshot `json:"repository"`
	Base               gitadapter.RevisionSnapshot   `json:"base"`
	Head               gitadapter.RevisionSnapshot   `json:"head"`
	ManifestSHA256     string                        `json:"manifest_sha256"`
	Diff               *DiffSnapshot                 `json:"diff,omitempty"`
	Selection          *SelectionSnapshot            `json:"selection,omitempty"`
	Scope              *ScopeSnapshot                `json:"scope,omitempty"`
	DirtyState         gitadapter.DirtyState         `json:"dirty_state"`
	Completeness       gitadapter.Completeness       `json:"completeness"`
	CompletenessReason []gitadapter.Reason           `json:"completeness_reasons"`
	CapturedAt         time.Time                     `json:"captured_at"`
	CapturedBy         string                        `json:"captured_by"`
	GitVersion         string                        `json:"git_version"`
}

type DiffSnapshot struct {
	PatchSHA256    string `json:"patch_sha256"`
	PatchSizeBytes int64  `json:"patch_size_bytes"`
	PatchFormat    string `json:"patch_format"`
}

type SelectionRange struct {
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
}

type SymbolSelector struct {
	Language      string `json:"language"`
	Kind          string `json:"kind"`
	QualifiedName string `json:"qualified_name"`
}

type SelectionSnapshot struct {
	Path string `json:"path"`

	// StartLine/EndLine are non-zero only when the admitted request used the
	// legacy single-range shape. EffectiveRanges is always explicit and is the
	// sole line authorization consumed by the workflow.
	StartLine       uint32           `json:"start_line,omitempty"`
	EndLine         uint32           `json:"end_line,omitempty"`
	EffectiveRanges []SelectionRange `json:"effective_ranges"`
	Symbol          *SymbolSelector  `json:"symbol,omitempty"`

	SourceKind             string `json:"source_kind"`
	FileSHA256             string `json:"file_sha256"`
	FileSizeBytes          int64  `json:"file_size_bytes"`
	SelectionContentSHA256 string `json:"selection_content_sha256"`
	SelectionSizeBytes     int64  `json:"selection_size_bytes"`
}

type ScopeSnapshot struct {
	Include       []string `json:"include"`
	Exclude       []string `json:"exclude"`
	MatchedFiles  int      `json:"matched_files"`
	IncludedFiles int      `json:"included_files"`
	SkippedFiles  int      `json:"skipped_files"`
}

type MaterializedTarget struct {
	SchemaVersion       string                      `json:"schema_version"`
	Snapshot            TargetSnapshot              `json:"snapshot"`
	ManifestRef         runmodel.ArtifactRef        `json:"manifest_ref"`
	PatchRef            *runmodel.ArtifactRef       `json:"patch_ref,omitempty"`
	SelectionContentRef *runmodel.ArtifactRef       `json:"selection_content_ref,omitempty"`
	FileRefs            []TargetFileRef             `json:"file_refs"`
	Contexts            []reviewcore.ContextBinding `json:"contexts"`
}

type TargetFileRef struct {
	Path         string                  `json:"path"`
	Status       gitadapter.ChangeStatus `json:"status,omitempty"`
	SHA256       string                  `json:"sha256,omitempty"`
	SizeBytes    int64                   `json:"size_bytes"`
	ContentRef   *runmodel.ArtifactRef   `json:"content_ref,omitempty"`
	Completeness gitadapter.Completeness `json:"completeness"`
	Reasons      []gitadapter.Reason     `json:"reasons"`
}

type SelectionManifest struct {
	SchemaVersion string                        `json:"schema_version"`
	Repository    gitadapter.RepositorySnapshot `json:"repository"`
	Revision      gitadapter.RevisionSnapshot   `json:"revision"`
	Path          string                        `json:"path"`

	StartLine       uint32           `json:"start_line,omitempty"`
	EndLine         uint32           `json:"end_line,omitempty"`
	EffectiveRanges []SelectionRange `json:"effective_ranges"`
	Symbol          *SymbolSelector  `json:"symbol,omitempty"`

	SourceKind             string                  `json:"source_kind"`
	FileSHA256             string                  `json:"file_sha256"`
	FileSizeBytes          int64                   `json:"file_size_bytes"`
	SelectionContentSHA256 string                  `json:"selection_content_sha256"`
	SelectionSizeBytes     int64                   `json:"selection_size_bytes"`
	Completeness           gitadapter.Completeness `json:"completeness"`
	Reasons                []gitadapter.Reason     `json:"reasons"`
}

type ScopeManifest struct {
	SchemaVersion string                        `json:"schema_version"`
	Repository    gitadapter.RepositorySnapshot `json:"repository"`
	Revision      gitadapter.RevisionSnapshot   `json:"revision"`
	Include       []string                      `json:"include"`
	Exclude       []string                      `json:"exclude"`
	Files         []ScopeManifestFile           `json:"files"`
	Coverage      ScopeCoverage                 `json:"coverage"`
	Completeness  gitadapter.Completeness       `json:"completeness"`
	Reasons       []gitadapter.Reason           `json:"reasons"`
}

type ScopeManifestFile struct {
	Path         string                  `json:"path"`
	Mode         string                  `json:"mode"`
	ObjectType   string                  `json:"object_type"`
	ObjectOID    string                  `json:"object_oid"`
	Language     string                  `json:"language"`
	SHA256       string                  `json:"sha256,omitempty"`
	SizeBytes    int64                   `json:"size_bytes"`
	Completeness gitadapter.Completeness `json:"completeness"`
	Reasons      []gitadapter.Reason     `json:"reasons"`
}

type ScopeCoverage struct {
	ScannedFiles         int `json:"scanned_files"`
	MatchedFiles         int `json:"matched_files"`
	IncludedFiles        int `json:"included_files"`
	SkippedFiles         int `json:"skipped_files"`
	ExcludedFiles        int `json:"excluded_files"`
	RequestExcludedFiles int `json:"request_excluded_files"`
	PolicyExcludedFiles  int `json:"policy_excluded_files"`
}

func (target TargetSnapshot) Validate() error {
	if target.SchemaVersion != TargetSnapshotSchemaVersion ||
		target.TargetSnapshotID == "" || target.SHA256 == "" {
		return fmt.Errorf("target snapshot identity is invalid")
	}
	if target.Repository.Kind == "" || target.Repository.RepositoryID == "" ||
		target.Repository.ObjectFormat == "" ||
		target.Base.Requested == "" || target.Base.CommitOID == "" ||
		target.Head.Requested == "" || target.Head.CommitOID == "" {
		return fmt.Errorf("target snapshot repository and revisions are incomplete")
	}
	if !exactOIDMatchesFormat(target.Base.CommitOID, target.Repository.ObjectFormat) ||
		!exactOIDMatchesFormat(target.Head.CommitOID, target.Repository.ObjectFormat) {
		return fmt.Errorf("target snapshot revisions do not match repository object format")
	}
	if err := ValidateDigest("manifest_sha256", target.ManifestSHA256); err != nil {
		return err
	}
	payloads := 0
	if target.Diff != nil {
		payloads++
	}
	if target.Selection != nil {
		payloads++
	}
	if target.Scope != nil {
		payloads++
	}
	if payloads != 1 {
		return fmt.Errorf("target snapshot requires exactly one mode payload")
	}
	switch target.Mode {
	case reviewcore.TargetModeDiff:
		if target.Diff == nil || target.Base.CommitOID == target.Head.CommitOID {
			return fmt.Errorf("diff snapshot requires distinct revisions and diff payload")
		}
		if err := ValidateDigest("diff.patch_sha256", target.Diff.PatchSHA256); err != nil {
			return err
		}
		if target.Diff.PatchSizeBytes <= 0 || target.Diff.PatchFormat == "" {
			return fmt.Errorf("diff snapshot patch identity is invalid")
		}
	case reviewcore.TargetModeSelection:
		if target.Selection == nil || target.Base.CommitOID != target.Head.CommitOID {
			return fmt.Errorf("selection snapshot requires one exact revision")
		}
		if err := target.Selection.validate(); err != nil {
			return err
		}
	case reviewcore.TargetModeScope:
		if target.Scope == nil || target.Base.CommitOID != target.Head.CommitOID {
			return fmt.Errorf("scope snapshot requires one exact revision")
		}
		if err := target.Scope.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported target snapshot mode %q", target.Mode)
	}
	if err := validateCompleteness(
		"target snapshot",
		target.Completeness,
		target.CompletenessReason,
	); err != nil {
		return err
	}
	switch target.DirtyState {
	case gitadapter.DirtyStateClean, gitadapter.DirtyStateDirty:
	case gitadapter.DirtyStateUnknown:
		if !hasReason(target.CompletenessReason, gitadapter.ReasonDirtyStateUnknown) {
			return fmt.Errorf("unknown dirty state requires dirty_state_unknown reason")
		}
	default:
		return fmt.Errorf("unsupported target dirty state %q", target.DirtyState)
	}
	if target.CapturedAt.IsZero() ||
		target.CapturedBy == "" || target.GitVersion == "" {
		return fmt.Errorf("target snapshot capture metadata is incomplete")
	}
	targetID, err := semanticTargetID(target)
	if err != nil {
		return err
	}
	if target.TargetSnapshotID != targetID {
		return fmt.Errorf("target_snapshot_id does not match semantic target fields")
	}
	digest, err := DigestTargetSnapshot(target)
	if err != nil {
		return err
	}
	if digest != target.SHA256 {
		return fmt.Errorf("target snapshot digest does not match its fields")
	}
	return nil
}

func (selection SelectionSnapshot) validate() error {
	if err := ValidateRepositoryPath("selection.path", selection.Path); err != nil {
		return err
	}
	if err := ValidateSelectionRanges(
		"selection.effective_ranges",
		selection.EffectiveRanges,
	); err != nil {
		return err
	}
	legacy := selection.StartLine != 0 || selection.EndLine != 0
	if legacy {
		if selection.StartLine == 0 || selection.EndLine < selection.StartLine ||
			len(selection.EffectiveRanges) != 1 ||
			selection.EffectiveRanges[0] != (SelectionRange{
				StartLine: selection.StartLine,
				EndLine:   selection.EndLine,
			}) {
			return fmt.Errorf("selection legacy range does not match effective_ranges")
		}
	}
	if selection.Symbol != nil {
		if legacy || len(selection.EffectiveRanges) != 1 {
			return fmt.Errorf("symbol selection requires one resolved effective range")
		}
		if err := selection.Symbol.Validate(); err != nil {
			return err
		}
	}
	if selection.SourceKind != SelectionSourceCommit &&
		selection.SourceKind != SelectionSourceOverlay {
		return fmt.Errorf("selection source kind %q is invalid", selection.SourceKind)
	}
	for name, digest := range map[string]string{
		"selection.file_sha256":    selection.FileSHA256,
		"selection.content_sha256": selection.SelectionContentSHA256,
	} {
		if err := ValidateDigest(name, digest); err != nil {
			return err
		}
	}
	if selection.FileSizeBytes < 0 || selection.SelectionSizeBytes <= 0 {
		return fmt.Errorf("selection content sizes are invalid")
	}
	return nil
}

func (scope ScopeSnapshot) validate() error {
	if scope.Include == nil || len(scope.Include) == 0 || scope.Exclude == nil {
		return fmt.Errorf("scope patterns are incomplete")
	}
	for name, patterns := range map[string][]string{
		"scope.include": scope.Include,
		"scope.exclude": scope.Exclude,
	} {
		for _, pattern := range patterns {
			if err := ValidateScopePattern(name, pattern); err != nil {
				return err
			}
		}
	}
	if scope.MatchedFiles < 0 || scope.IncludedFiles < 0 || scope.SkippedFiles < 0 ||
		scope.IncludedFiles+scope.SkippedFiles != scope.MatchedFiles {
		return fmt.Errorf("scope file coverage is inconsistent")
	}
	return nil
}

func (manifest SelectionManifest) ValidateAgainst(snapshot TargetSnapshot) error {
	if manifest.SchemaVersion != ContractSelectionManifest {
		return fmt.Errorf("unsupported selection manifest schema %q", manifest.SchemaVersion)
	}
	if snapshot.Mode != reviewcore.TargetModeSelection || snapshot.Selection == nil {
		return fmt.Errorf("selection manifest requires a selection target snapshot")
	}
	if manifest.Repository != snapshot.Repository || manifest.Revision != snapshot.Head ||
		snapshot.Base != snapshot.Head {
		return fmt.Errorf("selection manifest repository or revision does not match target snapshot")
	}
	selection := snapshot.Selection
	manifestRanges := manifest.EffectiveRanges
	if len(manifestRanges) == 0 && manifest.StartLine != 0 {
		manifestRanges = []SelectionRange{{
			StartLine: manifest.StartLine,
			EndLine:   manifest.EndLine,
		}}
	}
	if manifest.Path != selection.Path ||
		manifest.StartLine != selection.StartLine ||
		manifest.EndLine != selection.EndLine ||
		!slices.Equal(manifestRanges, selection.EffectiveRanges) ||
		!equalSymbolSelector(manifest.Symbol, selection.Symbol) ||
		manifest.SourceKind != selection.SourceKind ||
		manifest.FileSHA256 != selection.FileSHA256 ||
		manifest.FileSizeBytes != selection.FileSizeBytes ||
		manifest.SelectionContentSHA256 != selection.SelectionContentSHA256 ||
		manifest.SelectionSizeBytes != selection.SelectionSizeBytes {
		return fmt.Errorf("selection manifest does not exactly match target snapshot")
	}
	if manifest.Completeness != snapshot.Completeness ||
		!slices.Equal(manifest.Reasons, snapshot.CompletenessReason) {
		return fmt.Errorf("selection manifest completeness does not match target snapshot")
	}
	if manifest.Reasons == nil {
		return fmt.Errorf("selection manifest reasons must be an explicit array")
	}
	if err := ValidateSelectionRanges(
		"selection manifest effective_ranges",
		manifestRanges,
	); err != nil {
		return err
	}
	return nil
}

func (selector SymbolSelector) Validate() error {
	if selector.Language != "go" {
		return fmt.Errorf("symbol selector language must be %q in v1alpha1", "go")
	}
	switch selector.Kind {
	case "function", "method", "type":
	default:
		return fmt.Errorf("symbol selector kind %q is unsupported", selector.Kind)
	}
	parts := strings.Split(selector.QualifiedName, ".")
	expectedParts := 1
	if selector.Kind == "method" {
		expectedParts = 2
	}
	if len(parts) != expectedParts {
		return fmt.Errorf("symbol selector qualified_name does not match kind %q", selector.Kind)
	}
	for _, part := range parts {
		if !portableSymbolIdentifier(part) {
			return fmt.Errorf("symbol selector qualified_name contains a non-portable identifier")
		}
	}
	return nil
}

func ValidateSelectionRanges(name string, ranges []SelectionRange) error {
	if len(ranges) == 0 || len(ranges) > 128 {
		return fmt.Errorf("%s must contain between 1 and 128 ranges", name)
	}
	var previousEnd uint32
	for index, lineRange := range ranges {
		if lineRange.StartLine == 0 || lineRange.EndLine < lineRange.StartLine {
			return fmt.Errorf("%s[%d] is invalid", name, index)
		}
		if index > 0 && lineRange.StartLine <= previousEnd {
			return fmt.Errorf("%s must be sorted and non-overlapping", name)
		}
		previousEnd = lineRange.EndLine
	}
	return nil
}

func portableSymbolIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character == '_' ||
			index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func equalSymbolSelector(left, right *SymbolSelector) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (manifest ScopeManifest) ValidateAgainst(snapshot TargetSnapshot) error {
	if manifest.SchemaVersion != ContractScopeManifest {
		return fmt.Errorf("unsupported scope manifest schema %q", manifest.SchemaVersion)
	}
	if snapshot.Mode != reviewcore.TargetModeScope || snapshot.Scope == nil {
		return fmt.Errorf("scope manifest requires a scope target snapshot")
	}
	if manifest.Repository != snapshot.Repository || manifest.Revision != snapshot.Head ||
		snapshot.Base != snapshot.Head {
		return fmt.Errorf("scope manifest repository or revision does not match target snapshot")
	}
	scope := snapshot.Scope
	if !slices.Equal(manifest.Include, scope.Include) ||
		!slices.Equal(manifest.Exclude, scope.Exclude) {
		return fmt.Errorf("scope manifest patterns do not match target snapshot")
	}
	if manifest.Coverage.ScannedFiles < 0 ||
		manifest.Coverage.MatchedFiles < 0 ||
		manifest.Coverage.IncludedFiles < 0 ||
		manifest.Coverage.SkippedFiles < 0 ||
		manifest.Coverage.ExcludedFiles < 0 ||
		manifest.Coverage.RequestExcludedFiles < 0 ||
		manifest.Coverage.PolicyExcludedFiles < 0 ||
		manifest.Coverage.ExcludedFiles !=
			manifest.Coverage.RequestExcludedFiles+
				manifest.Coverage.PolicyExcludedFiles ||
		manifest.Coverage.IncludedFiles+manifest.Coverage.SkippedFiles !=
			manifest.Coverage.MatchedFiles ||
		manifest.Coverage.MatchedFiles+manifest.Coverage.ExcludedFiles >
			manifest.Coverage.ScannedFiles {
		return fmt.Errorf("scope manifest coverage is inconsistent")
	}
	if manifest.Coverage.MatchedFiles != scope.MatchedFiles ||
		manifest.Coverage.IncludedFiles != scope.IncludedFiles ||
		manifest.Coverage.SkippedFiles != scope.SkippedFiles {
		return fmt.Errorf("scope manifest coverage does not match target snapshot")
	}
	if manifest.Files == nil || len(manifest.Files) < manifest.Coverage.IncludedFiles ||
		len(manifest.Files) > manifest.Coverage.MatchedFiles {
		return fmt.Errorf("scope manifest retained files do not match coverage")
	}
	completeFiles := 0
	for index, file := range manifest.Files {
		if err := ValidateRepositoryPath("scope manifest file path", file.Path); err != nil {
			return err
		}
		if index > 0 && file.Path <= manifest.Files[index-1].Path {
			return fmt.Errorf("scope manifest files must be uniquely sorted by path")
		}
		if file.Mode == "" ||
			file.ObjectType != "blob" && file.ObjectType != "commit" ||
			!exactOIDMatchesFormat(file.ObjectOID, snapshot.Repository.ObjectFormat) {
			return fmt.Errorf("scope manifest file %q has invalid Git identity", file.Path)
		}
		if file.SizeBytes < 0 || file.Reasons == nil {
			return fmt.Errorf("scope manifest file %q has invalid content metadata", file.Path)
		}
		if err := validateCompleteness(
			fmt.Sprintf("scope manifest file %q", file.Path),
			file.Completeness,
			file.Reasons,
		); err != nil {
			return err
		}
		switch file.Completeness {
		case gitadapter.CompletenessComplete:
			completeFiles++
			if err := ValidateDigest("scope manifest file sha256", file.SHA256); err != nil {
				return err
			}
		case gitadapter.CompletenessPartial, gitadapter.CompletenessSkipped:
			if file.SHA256 != "" {
				if err := ValidateDigest("scope manifest file sha256", file.SHA256); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf(
				"scope manifest file %q has unsupported completeness %q",
				file.Path,
				file.Completeness,
			)
		}
	}
	if completeFiles != manifest.Coverage.IncludedFiles {
		return fmt.Errorf("scope manifest complete files do not match included coverage")
	}
	if err := validateCompleteness(
		"scope manifest",
		manifest.Completeness,
		manifest.Reasons,
	); err != nil {
		return err
	}
	if manifest.Completeness != snapshot.Completeness ||
		!slices.Equal(manifest.Reasons, snapshot.CompletenessReason) {
		return fmt.Errorf("scope manifest completeness does not match target snapshot")
	}
	if manifest.Coverage.SkippedFiles > 0 &&
		manifest.Completeness == gitadapter.CompletenessComplete {
		return fmt.Errorf("scope manifest with skipped files cannot be complete")
	}
	if manifest.Coverage.MatchedFiles == 0 &&
		!hasReason(manifest.Reasons, gitadapter.ReasonNoMatchingFiles) {
		return fmt.Errorf("empty scope match requires no_matching_files reason")
	}
	return nil
}

func ValidateDiffManifest(
	manifest gitadapter.ChangeManifest,
	snapshot TargetSnapshot,
) error {
	if manifest.SchemaVersion != gitadapter.ChangeManifestSchemaVersion {
		return fmt.Errorf("unsupported change manifest schema %q", manifest.SchemaVersion)
	}
	if snapshot.Mode != reviewcore.TargetModeDiff || snapshot.Diff == nil {
		return fmt.Errorf("change manifest requires a diff target snapshot")
	}
	if manifest.BaseCommitOID != snapshot.Base.CommitOID ||
		manifest.HeadCommitOID != snapshot.Head.CommitOID ||
		manifest.PatchSHA256 != snapshot.Diff.PatchSHA256 ||
		manifest.PatchSize != snapshot.Diff.PatchSizeBytes {
		return fmt.Errorf("change manifest does not exactly match target snapshot")
	}
	if err := validateCompleteness(
		"change manifest",
		manifest.Completeness,
		manifest.Reasons,
	); err != nil {
		return err
	}
	if len(snapshot.CompletenessReason) < len(manifest.Reasons) ||
		!slices.Equal(
			snapshot.CompletenessReason[:len(manifest.Reasons)],
			manifest.Reasons,
		) {
		return fmt.Errorf("change manifest reasons are not a prefix of target completeness reasons")
	}
	if manifest.Completeness == gitadapter.CompletenessComplete &&
		len(snapshot.CompletenessReason) == 0 {
		if snapshot.Completeness != gitadapter.CompletenessComplete {
			return fmt.Errorf("complete change manifest requires a complete target")
		}
	} else if snapshot.Completeness == gitadapter.CompletenessComplete {
		return fmt.Errorf("incomplete change capture cannot produce a complete target")
	}
	if manifest.Files == nil || manifest.Reasons == nil {
		return fmt.Errorf("change manifest files and reasons must be explicit arrays")
	}
	coverage := manifest.Coverage
	if coverage.TotalFiles < 0 || coverage.IncludedFiles < 0 ||
		coverage.SkippedFiles < 0 || coverage.TotalHunks < 0 ||
		coverage.IncludedHunks < 0 || coverage.SkippedHunks < 0 ||
		coverage.DiffFileHeaders < 0 ||
		coverage.IncludedFiles+coverage.SkippedFiles != coverage.TotalFiles ||
		coverage.IncludedHunks+coverage.SkippedHunks != coverage.TotalHunks ||
		len(manifest.Files) > coverage.TotalFiles {
		return fmt.Errorf("change manifest coverage is inconsistent")
	}
	includedFiles := 0
	includedHunks := 0
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		if err := ValidateRepositoryPath("change manifest file path", file.Path); err != nil {
			return err
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return fmt.Errorf("change manifest contains duplicate path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		pathDigest := sha256.Sum256([]byte(file.Path))
		if file.PathSHA256 != hex.EncodeToString(pathDigest[:]) || file.HunkCount < 0 ||
			file.SkippedReasons == nil {
			return fmt.Errorf("change manifest file %q metadata is inconsistent", file.Path)
		}
		if file.Included {
			includedFiles++
			includedHunks += file.HunkCount
		}
	}
	if includedFiles != coverage.IncludedFiles || includedHunks != coverage.IncludedHunks {
		return fmt.Errorf("change manifest retained files do not match coverage")
	}
	return nil
}

func SealTargetSnapshot(snapshot TargetSnapshot) (TargetSnapshot, error) {
	snapshot.TargetSnapshotID = ""
	snapshot.SHA256 = ""
	if snapshot.Selection != nil &&
		len(snapshot.Selection.EffectiveRanges) == 0 &&
		snapshot.Selection.StartLine != 0 {
		snapshot.Selection.EffectiveRanges = []SelectionRange{{
			StartLine: snapshot.Selection.StartLine,
			EndLine:   snapshot.Selection.EndLine,
		}}
	}
	targetID, err := semanticTargetID(snapshot)
	if err != nil {
		return TargetSnapshot{}, err
	}
	snapshot.TargetSnapshotID = targetID
	snapshot.SHA256, err = DigestTargetSnapshot(snapshot)
	if err != nil {
		return TargetSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return TargetSnapshot{}, err
	}
	return snapshot, nil
}

func semanticTargetID(snapshot TargetSnapshot) (string, error) {
	identity := struct {
		Mode           reviewcore.TargetMode
		Repository     gitadapter.RepositorySnapshot
		Base           gitadapter.RevisionSnapshot
		Head           gitadapter.RevisionSnapshot
		ManifestSHA256 string
		Diff           *DiffSnapshot
		Selection      *SelectionSnapshot
		Scope          *ScopeSnapshot
		Completeness   gitadapter.Completeness
		Reasons        []gitadapter.Reason
	}{
		Mode: snapshot.Mode, Repository: snapshot.Repository, Base: snapshot.Base, Head: snapshot.Head,
		ManifestSHA256: snapshot.ManifestSHA256, Diff: snapshot.Diff,
		Selection: snapshot.Selection, Scope: snapshot.Scope,
		Completeness: snapshot.Completeness, Reasons: snapshot.CompletenessReason,
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal target snapshot identity: %w", err)
	}
	identityDigest := sha256.Sum256(data)
	return fmt.Sprintf("target_%x", identityDigest[:16]), nil
}

func DigestTargetSnapshot(snapshot TargetSnapshot) (string, error) {
	snapshot.SHA256 = ""
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("marshal target snapshot: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (target MaterializedTarget) Validate() error {
	if target.SchemaVersion != MaterializedTargetSchemaVersion {
		return fmt.Errorf("unsupported materialized target schema %q", target.SchemaVersion)
	}
	if err := target.Snapshot.Validate(); err != nil {
		return fmt.Errorf("validate target snapshot: %w", err)
	}
	if err := target.ManifestRef.Validate(); err != nil {
		return fmt.Errorf("manifest_ref: %w", err)
	}
	if target.ManifestRef.SHA256 != target.Snapshot.ManifestSHA256 {
		return fmt.Errorf("manifest_ref does not bind the target snapshot")
	}
	if target.Contexts == nil {
		return fmt.Errorf("contexts must be an explicit array")
	}
	previousContextID := ""
	for index, binding := range target.Contexts {
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("contexts[%d]: %w", index, err)
		}
		contextID := binding.ContextID()
		if index > 0 && contextID <= previousContextID {
			return fmt.Errorf("contexts must be uniquely sorted by context_id")
		}
		previousContextID = contextID
	}
	switch target.Snapshot.Mode {
	case reviewcore.TargetModeDiff:
		if target.ManifestRef.Contract != ContractChangeManifest ||
			target.PatchRef == nil || target.SelectionContentRef != nil {
			return fmt.Errorf("diff materialization has invalid artifact roles")
		}
		if err := target.PatchRef.Validate(); err != nil {
			return fmt.Errorf("patch_ref: %w", err)
		}
		if target.PatchRef.Contract != ContractCanonicalPatch ||
			target.PatchRef.SHA256 != target.Snapshot.Diff.PatchSHA256 ||
			target.PatchRef.SizeBytes != target.Snapshot.Diff.PatchSizeBytes {
			return fmt.Errorf("patch_ref does not bind the snapshot patch")
		}
	case reviewcore.TargetModeSelection:
		if target.ManifestRef.Contract != ContractSelectionManifest ||
			target.PatchRef != nil || target.SelectionContentRef == nil {
			return fmt.Errorf("selection materialization has invalid artifact roles")
		}
		if err := target.SelectionContentRef.Validate(); err != nil {
			return fmt.Errorf("selection_content_ref: %w", err)
		}
		if target.SelectionContentRef.Contract != ContractSelectionContent ||
			target.SelectionContentRef.SHA256 != target.Snapshot.Selection.SelectionContentSHA256 ||
			target.SelectionContentRef.SizeBytes != target.Snapshot.Selection.SelectionSizeBytes {
			return fmt.Errorf("selection_content_ref does not bind the selected bytes")
		}
	case reviewcore.TargetModeScope:
		if target.ManifestRef.Contract != ContractScopeManifest ||
			target.PatchRef != nil || target.SelectionContentRef != nil {
			return fmt.Errorf("scope materialization has invalid artifact roles")
		}
	}
	if target.FileRefs == nil {
		return fmt.Errorf("file_refs must be an explicit array")
	}
	if !sort.SliceIsSorted(target.FileRefs, func(left, right int) bool {
		return target.FileRefs[left].Path < target.FileRefs[right].Path
	}) {
		return fmt.Errorf("file_refs must be sorted by path")
	}
	for index, file := range target.FileRefs {
		if err := ValidateRepositoryPath("file_refs.path", file.Path); err != nil {
			return err
		}
		if index > 0 && file.Path == target.FileRefs[index-1].Path {
			return fmt.Errorf("file_refs contains a duplicate path")
		}
		if file.SizeBytes < 0 {
			return fmt.Errorf("file_ref %q size must not be negative", file.Path)
		}
		if file.SHA256 != "" {
			if err := ValidateDigest("file_refs.sha256", file.SHA256); err != nil {
				return err
			}
		}
		if err := validateCompleteness(
			fmt.Sprintf("file_ref %q", file.Path),
			file.Completeness,
			file.Reasons,
		); err != nil {
			return err
		}
		switch file.Completeness {
		case gitadapter.CompletenessComplete:
			if file.ContentRef == nil || file.SHA256 == "" {
				return fmt.Errorf("complete file_ref %q requires exact retained content", file.Path)
			}
		case gitadapter.CompletenessPartial, gitadapter.CompletenessSkipped:
			if file.ContentRef != nil {
				return fmt.Errorf("incomplete file_ref %q must not retain content", file.Path)
			}
		default:
			return fmt.Errorf("file_ref %q has unsupported completeness %q", file.Path, file.Completeness)
		}
		if file.ContentRef != nil {
			if err := file.ContentRef.Validate(); err != nil {
				return fmt.Errorf("file_refs[%d].content_ref: %w", index, err)
			}
			if file.ContentRef.Contract != ContractFileContent {
				return fmt.Errorf("file_refs[%d] has unexpected content contract", index)
			}
			if file.ContentRef.SHA256 != file.SHA256 ||
				file.ContentRef.SizeBytes != file.SizeBytes {
				return fmt.Errorf("file_refs[%d] content does not match file identity", index)
			}
		}
	}
	if target.Snapshot.Mode == reviewcore.TargetModeSelection {
		if len(target.FileRefs) != 1 {
			return fmt.Errorf("selection materialization requires exactly one file_ref")
		}
		file := target.FileRefs[0]
		if file.Path != target.Snapshot.Selection.Path ||
			file.SHA256 != target.Snapshot.Selection.FileSHA256 ||
			file.SizeBytes != target.Snapshot.Selection.FileSizeBytes ||
			file.Completeness != gitadapter.CompletenessComplete ||
			file.ContentRef == nil ||
			file.ContentRef.SHA256 != target.Snapshot.Selection.FileSHA256 ||
			file.ContentRef.SizeBytes != target.Snapshot.Selection.FileSizeBytes {
			return fmt.Errorf("selection file_ref does not bind the selected source file")
		}
	}
	if target.Snapshot.Mode == reviewcore.TargetModeScope {
		if len(target.FileRefs) > target.Snapshot.Scope.MatchedFiles {
			return fmt.Errorf("scope file_refs exceed matched coverage")
		}
		completeFiles := 0
		for _, file := range target.FileRefs {
			if file.Completeness == gitadapter.CompletenessComplete {
				completeFiles++
			}
		}
		if completeFiles != target.Snapshot.Scope.IncludedFiles {
			return fmt.Errorf("scope complete file_refs do not match included coverage")
		}
		if target.Snapshot.Scope.SkippedFiles > 0 &&
			target.Snapshot.Completeness == gitadapter.CompletenessComplete {
			return fmt.Errorf("scope target with skipped files cannot be complete")
		}
		if target.Snapshot.Scope.MatchedFiles == 0 &&
			!hasReason(target.Snapshot.CompletenessReason, gitadapter.ReasonNoMatchingFiles) {
			return fmt.Errorf("empty scope target requires no_matching_files reason")
		}
	}
	if target.Snapshot.Completeness == gitadapter.CompletenessComplete {
		for _, file := range target.FileRefs {
			if target.Snapshot.Mode == reviewcore.TargetModeDiff &&
				file.Status == gitadapter.ChangeDeleted {
				continue
			}
			if file.Completeness != gitadapter.CompletenessComplete {
				return fmt.Errorf("complete target contains incomplete file_ref %q", file.Path)
			}
		}
	}
	return nil
}

func validateCompleteness(
	name string,
	state gitadapter.Completeness,
	reasons []gitadapter.Reason,
) error {
	if reasons == nil {
		return fmt.Errorf("%s reasons must be an explicit array", name)
	}
	for index, reason := range reasons {
		if reason.Code == "" {
			return fmt.Errorf("%s reason %d has an empty code", name, index)
		}
	}
	switch state {
	case gitadapter.CompletenessComplete:
		if len(reasons) != 0 {
			return fmt.Errorf("%s is complete but contains reasons", name)
		}
	case gitadapter.CompletenessPartial, gitadapter.CompletenessSkipped:
		if len(reasons) == 0 {
			return fmt.Errorf("%s is %s but contains no reason", name, state)
		}
	default:
		return fmt.Errorf("%s has unsupported completeness %q", name, state)
	}
	return nil
}

func hasReason(reasons []gitadapter.Reason, code gitadapter.ReasonCode) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func ValidateDigest(name, value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	return nil
}

func ValidateRepositoryPath(name, value string) error {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, `\:`) ||
		value != path.Clean(value) || path.IsAbs(value) || value == "." || value == ".." ||
		strings.HasPrefix(value, "../") {
		return fmt.Errorf("%s must be a portable repository-relative path", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func ValidateScopePattern(name, value string) error {
	if value == "" || len(value) > gitadapter.MaxScopePatternBytes ||
		value != strings.TrimSpace(value) || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\\:?[]") || strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s pattern %q is unsafe", name, value)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s pattern %q contains a control character", name, value)
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." ||
			strings.Contains(segment, "***") {
			return fmt.Errorf("%s pattern %q contains an unsafe segment", name, value)
		}
	}
	return nil
}

func exactOIDMatchesFormat(oid string, objectFormat string) bool {
	length := 0
	switch objectFormat {
	case "sha1":
		length = 40
	case "sha256":
		length = 64
	default:
		return false
	}
	if len(oid) != length || strings.ToLower(oid) != oid {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}
