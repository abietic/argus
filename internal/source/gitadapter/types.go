package gitadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	TargetSnapshotSchemaVersion = "argus.target_snapshot.git_diff.v1alpha1"
	ChangeManifestSchemaVersion = "argus.git_change_manifest.v1alpha1"
	CanonicalPatchVersion       = "argus.git_patch.v1alpha1"
	FileContentSchemaVersion    = "argus.git_file_content.v1alpha1"

	DefaultMaxPatchBytes = int64(8 << 20)
	DefaultMaxFiles      = 10_000
	DefaultMaxFileBytes  = int64(2 << 20)
)

type Completeness string

const (
	CompletenessComplete Completeness = "complete"
	CompletenessPartial  Completeness = "partial"
	CompletenessSkipped  Completeness = "skipped"
)

type DirtyState string

const (
	DirtyStateClean   DirtyState = "clean"
	DirtyStateDirty   DirtyState = "dirty"
	DirtyStateUnknown DirtyState = "unknown"
)

type ReasonCode string

const (
	ReasonInvalidRequest            ReasonCode = "invalid_request"
	ReasonRepositoryNotFound        ReasonCode = "repository_not_found"
	ReasonRepositoryNotDir          ReasonCode = "repository_not_directory"
	ReasonRepositoryNotGit          ReasonCode = "repository_not_git"
	ReasonBaseRevisionMissing       ReasonCode = "base_revision_missing"
	ReasonHeadRevisionMissing       ReasonCode = "head_revision_missing"
	ReasonNoChanges                 ReasonCode = "no_changes"
	ReasonPatchSizeExceeded         ReasonCode = "patch_size_exceeded"
	ReasonFileLimitExceeded         ReasonCode = "file_limit_exceeded"
	ReasonUnsafeRepositoryPath      ReasonCode = "unsafe_repository_path"
	ReasonRevisionMoved             ReasonCode = "revision_moved_during_capture"
	ReasonDirtyStateUnknown         ReasonCode = "dirty_state_unknown"
	ReasonManifestPatchMismatch     ReasonCode = "manifest_patch_mismatch"
	ReasonRevisionMissing           ReasonCode = "revision_missing"
	ReasonCommitMissing             ReasonCode = "commit_missing"
	ReasonNoMatchingFiles           ReasonCode = "no_matching_files"
	ReasonFileNotFound              ReasonCode = "file_not_found"
	ReasonSymlinkNotAllowed         ReasonCode = "symlink_not_allowed"
	ReasonSubmoduleNotAllowed       ReasonCode = "submodule_not_allowed"
	ReasonNonRegularFile            ReasonCode = "non_regular_file"
	ReasonBinaryFile                ReasonCode = "binary_file"
	ReasonUnsupportedEncoding       ReasonCode = "unsupported_text_encoding"
	ReasonNonCanonicalLineEndings   ReasonCode = "non_canonical_line_endings"
	ReasonFileSizeExceeded          ReasonCode = "file_size_exceeded"
	ReasonMaterializedBytesExceeded ReasonCode = "materialized_bytes_exceeded"
	ReasonTargetPolicyExcluded      ReasonCode = "target_policy_excluded"
	ReasonGitCommandFailed          ReasonCode = "git_command_failed"
	ReasonCancelled                 ReasonCode = "cancelled"
)

type Reason struct {
	Code   ReasonCode `json:"code"`
	Detail string     `json:"detail,omitempty"`
}

type ErrorCode string

const (
	ErrorInvalidRequest      ErrorCode = "invalid_request"
	ErrorGitUnavailable      ErrorCode = "git_unavailable"
	ErrorRepositoryNotFound  ErrorCode = "repository_not_found"
	ErrorRepositoryNotDir    ErrorCode = "repository_not_directory"
	ErrorRepositoryNotGit    ErrorCode = "repository_not_git"
	ErrorBaseRevisionMissing ErrorCode = "base_revision_missing"
	ErrorHeadRevisionMissing ErrorCode = "head_revision_missing"
	ErrorRevisionMissing     ErrorCode = "revision_missing"
	ErrorCommitMissing       ErrorCode = "commit_missing"
	ErrorBudgetExceeded      ErrorCode = "budget_exceeded"
	ErrorGitCommandFailed    ErrorCode = "git_command_failed"
	ErrorCancelled           ErrorCode = "cancelled"
)

// AdapterError classifies failures callers need to distinguish without parsing
// Git's human-readable stderr.
type AdapterError struct {
	Code  ErrorCode
	Field string
	Err   error
}

func (e *AdapterError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Field != "" {
		return fmt.Sprintf("%s (%s): %v", e.Code, e.Field, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *AdapterError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func ErrorCodeOf(err error) (ErrorCode, bool) {
	var adapterError *AdapterError
	if !errors.As(err, &adapterError) {
		return "", false
	}
	return adapterError.Code, true
}

type Limits struct {
	// MaxPatchBytes is a hard streaming boundary. Once crossed, Git is stopped
	// and the adapter returns budget_exceeded without exposing a prefix.
	MaxPatchBytes int64
	// MaxFiles is the maximum number of per-file entries retained. Total and
	// skipped counts remain explicit when the limit is crossed.
	MaxFiles int
}

type Request struct {
	RepositoryPath string
	// RepositoryID is a caller-owned stable identity. If empty, the adapter
	// derives a local-only identity from the canonical repository root.
	RepositoryID string
	BaseRevision string
	HeadRevision string
	Limits       Limits
}

type RevisionSnapshot struct {
	Requested string `json:"requested"`
	CommitOID string `json:"commit_oid"`
}

type RepositorySnapshot struct {
	Kind         string `json:"kind"`
	RepositoryID string `json:"repository_id"`
	ObjectFormat string `json:"object_format"`
}

type PatchArtifact struct {
	FormatVersion string `json:"format_version"`
	SHA256        string `json:"sha256"`
	SizeBytes     int64  `json:"size_bytes"`
	Included      bool   `json:"included"`
	// Data is present only when Included is true. It is deliberately excluded
	// from metadata JSON; the digest and size above identify its exact bytes.
	Data []byte `json:"-"`
}

type ChangeStatus string

const (
	ChangeAdded      ChangeStatus = "added"
	ChangeModified   ChangeStatus = "modified"
	ChangeDeleted    ChangeStatus = "deleted"
	ChangeRenamed    ChangeStatus = "renamed"
	ChangeCopied     ChangeStatus = "copied"
	ChangeTypeChange ChangeStatus = "type_changed"
	ChangeUnmerged   ChangeStatus = "unmerged"
	ChangeUnknown    ChangeStatus = "unknown"
)

type FileChange struct {
	Status            ChangeStatus `json:"status"`
	Path              string       `json:"path"`
	PathSHA256        string       `json:"path_sha256"`
	OldPath           string       `json:"old_path,omitempty"`
	OldPathSHA256     string       `json:"old_path_sha256,omitempty"`
	SimilarityPercent *int         `json:"similarity_percent,omitempty"`
	Language          string       `json:"language"`
	HunkCount         int          `json:"hunk_count"`
	Included          bool         `json:"included"`
	SkippedReasons    []ReasonCode `json:"skipped_reasons"`
}

type Coverage struct {
	TotalFiles      int `json:"total_files"`
	IncludedFiles   int `json:"included_files"`
	SkippedFiles    int `json:"skipped_files"`
	TotalHunks      int `json:"total_hunks"`
	IncludedHunks   int `json:"included_hunks"`
	SkippedHunks    int `json:"skipped_hunks"`
	DiffFileHeaders int `json:"diff_file_headers"`
}

type ChangeManifest struct {
	SchemaVersion string       `json:"schema_version"`
	BaseCommitOID string       `json:"base_commit_oid"`
	HeadCommitOID string       `json:"head_commit_oid"`
	PatchSHA256   string       `json:"patch_sha256"`
	PatchSize     int64        `json:"patch_size_bytes"`
	Files         []FileChange `json:"files"`
	Coverage      Coverage     `json:"coverage"`
	Completeness  Completeness `json:"completeness"`
	Reasons       []Reason     `json:"reasons"`
}

type TargetSnapshot struct {
	SchemaVersion      string             `json:"schema_version"`
	TargetSnapshotID   string             `json:"target_snapshot_id"`
	SHA256             string             `json:"sha256"`
	Repository         RepositorySnapshot `json:"repository"`
	Base               RevisionSnapshot   `json:"base"`
	Head               RevisionSnapshot   `json:"head"`
	ManifestSHA256     string             `json:"manifest_sha256"`
	PatchSHA256        string             `json:"patch_sha256"`
	PatchSizeBytes     int64              `json:"patch_size_bytes"`
	CanonicalPatch     string             `json:"canonical_patch_version"`
	DirtyState         DirtyState         `json:"dirty_state"`
	Completeness       Completeness       `json:"completeness"`
	CompletenessReason []Reason           `json:"completeness_reasons"`
	CapturedAt         time.Time          `json:"captured_at"`
	CapturedBy         string             `json:"captured_by"`
	GitVersion         string             `json:"git_version"`
}

type Result struct {
	// RepositoryRoot is runtime-only local information and is not part of the
	// content-addressed TargetSnapshot.
	RepositoryRoot string          `json:"-"`
	Snapshot       *TargetSnapshot `json:"snapshot,omitempty"`
	Manifest       *ChangeManifest `json:"manifest,omitempty"`
	Patch          PatchArtifact   `json:"patch"`
	Completeness   Completeness    `json:"completeness"`
	Reasons        []Reason        `json:"reasons"`
}

type FileContent struct {
	SchemaVersion string       `json:"schema_version"`
	CommitOID     string       `json:"commit_oid"`
	Path          string       `json:"path"`
	Mode          string       `json:"mode,omitempty"`
	BlobOID       string       `json:"blob_oid,omitempty"`
	SHA256        string       `json:"sha256,omitempty"`
	SizeBytes     int64        `json:"size_bytes"`
	Included      bool         `json:"included"`
	Completeness  Completeness `json:"completeness"`
	Reasons       []Reason     `json:"reasons"`
	Data          []byte       `json:"-"`
}

func (content FileContent) HasReason(code ReasonCode) bool {
	for _, reason := range content.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func (result Result) HasReason(code ReasonCode) bool {
	for _, reason := range result.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

// DigestManifest returns the SHA-256 of the canonical JSON representation.
// ChangeManifest contains no maps, so encoding/json provides stable field and
// slice ordering for the same adapter version.
func DigestManifest(manifest ChangeManifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal change manifest: %w", err)
	}
	return sha256Hex(data), nil
}

// DigestTargetSnapshot hashes the complete snapshot with SHA256 cleared. This
// avoids a self-referential digest while covering TargetSnapshotID and all
// capture metadata.
func DigestTargetSnapshot(snapshot TargetSnapshot) (string, error) {
	snapshot.SHA256 = ""
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("marshal target snapshot: %w", err)
	}
	return sha256Hex(data), nil
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
