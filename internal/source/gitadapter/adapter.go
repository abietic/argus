package gitadapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const capturedBy = "argus/internal/source/gitadapter"

type Adapter struct {
	gitPath string
	now     func() time.Time
}

func New() (*Adapter, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, &AdapterError{Code: ErrorGitUnavailable, Err: err}
	}
	return &Adapter{
		gitPath: gitPath,
		now:     time.Now,
	}, nil
}

// MaterializeDiff freezes a commit-to-commit target. Git is invoked only with
// resolved object IDs after admission; no revision supplied by the caller is
// ever interpreted as a diff option.
func (adapter *Adapter) MaterializeDiff(
	ctx context.Context,
	request Request,
) (Result, error) {
	if adapter == nil || adapter.gitPath == "" {
		return failedResult(ReasonGitCommandFailed, "adapter is not initialized"),
			&AdapterError{Code: ErrorGitUnavailable, Err: errors.New("adapter is not initialized")}
	}
	if ctx == nil {
		return failedResult(ReasonInvalidRequest, "context is nil"),
			&AdapterError{Code: ErrorInvalidRequest, Field: "context", Err: errors.New("must not be nil")}
	}
	limits, err := normalizeLimits(request.Limits)
	if err != nil {
		return failedResult(ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "limits", Err: err}
	}
	if err := validateRevision("base_revision", request.BaseRevision); err != nil {
		return failedResult(ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "base_revision", Err: err}
	}
	if err := validateRevision("head_revision", request.HeadRevision); err != nil {
		return failedResult(ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "head_revision", Err: err}
	}
	if err := validateRepositoryID(request.RepositoryID); err != nil {
		return failedResult(ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "repository_id", Err: err}
	}

	repositoryPath, result, err := resolveRepositoryPath(request.RepositoryPath)
	if err != nil {
		return result, err
	}
	root, objectFormat, err := adapter.inspectRepository(ctx, repositoryPath)
	if err != nil {
		return classifyCommandFailure(ctx, ReasonRepositoryNotGit, ErrorRepositoryNotGit, "repository_path", err)
	}
	gitVersion, err := adapter.version(ctx)
	if err != nil {
		return classifyCommandFailure(ctx, ReasonGitCommandFailed, ErrorGitCommandFailed, "git_version", err)
	}
	repositoryID := request.RepositoryID
	if repositoryID == "" {
		digest := sha256.Sum256([]byte(root))
		repositoryID = fmt.Sprintf("local-%x", digest[:16])
	}

	baseOID, err := adapter.resolveRevision(ctx, root, request.BaseRevision)
	if err != nil {
		return classifyRevisionFailure(ctx, ReasonBaseRevisionMissing, ErrorBaseRevisionMissing, "base_revision", err)
	}
	headOID, err := adapter.resolveRevision(ctx, root, request.HeadRevision)
	if err != nil {
		return classifyRevisionFailure(ctx, ReasonHeadRevisionMissing, ErrorHeadRevisionMissing, "head_revision", err)
	}

	dirtyState, dirtyReason, err := adapter.readDirtyState(ctx, root)
	if err != nil {
		return classifyCommandFailure(ctx, ReasonGitCommandFailed, ErrorGitCommandFailed, "dirty_state", err)
	}
	reasons := make([]Reason, 0, 5)
	if dirtyReason != nil {
		reasons = append(reasons, *dirtyReason)
	}
	objectView, cleanupObjectView, err := adapter.newIsolatedObjectView(
		ctx,
		root,
		objectFormat,
	)
	if err != nil {
		return classifyCommandFailure(
			ctx,
			ReasonGitCommandFailed,
			ErrorGitCommandFailed,
			"object_view",
			err,
		)
	}
	defer cleanupObjectView()
	objectViewArg := "--git-dir=" + objectView

	nameStatus := newNameStatusCollector(limits.MaxFiles)
	if baseOID != headOID {
		args := append([]string{objectViewArg}, canonicalNameStatusArgs(baseOID, headOID)...)
		if err := adapter.run(ctx, root, args, nameStatus); err != nil {
			return classifyCommandFailure(ctx, ReasonGitCommandFailed, ErrorGitCommandFailed, "name_status", err)
		}
		if err := nameStatus.finish(); err != nil {
			return failedResult(ReasonGitCommandFailed, err.Error()),
				&AdapterError{Code: ErrorGitCommandFailed, Field: "name_status", Err: err}
		}
	}

	patch := newPatchCollector(limits.MaxPatchBytes, limits.MaxFiles)
	if baseOID != headOID {
		args := append([]string{objectViewArg}, canonicalPatchArgs(baseOID, headOID)...)
		if err := adapter.run(ctx, root, args, patch); err != nil {
			if patch.overflow || errors.Is(err, errPatchBudgetExceeded) {
				detail := fmt.Sprintf(
					"canonical patch exceeded max_patch_bytes=%d; generation stopped without retaining a prefix",
					limits.MaxPatchBytes,
				)
				return failedResult(ReasonPatchSizeExceeded, detail),
					&AdapterError{
						Code: ErrorBudgetExceeded, Field: "max_patch_bytes",
						Err: errPatchBudgetExceeded,
					}
			}
			return classifyCommandFailure(ctx, ReasonGitCommandFailed, ErrorGitCommandFailed, "patch", err)
		}
	}
	patch.finish()

	files := nameStatus.files
	for index := range files {
		if index < len(patch.fileHunks) {
			files[index].HunkCount = patch.fileHunks[index]
		}
	}
	if nameStatus.total > len(files) {
		reasons = append(reasons, Reason{
			Code: ReasonFileLimitExceeded,
			Detail: fmt.Sprintf(
				"retained %d of %d changed files (max_files=%d)",
				len(files), nameStatus.total, limits.MaxFiles,
			),
		})
	}
	unsafePathCount := 0
	for _, file := range files {
		if containsReasonCode(file.SkippedReasons, ReasonUnsafeRepositoryPath) {
			unsafePathCount++
		}
	}
	if unsafePathCount != 0 {
		reasons = append(reasons, Reason{
			Code: ReasonUnsafeRepositoryPath,
			Detail: fmt.Sprintf(
				"%d retained changed file entries violate the portable repository path grammar",
				unsafePathCount,
			),
		})
	}
	if patch.overflow {
		reasons = append(reasons, Reason{
			Code: ReasonPatchSizeExceeded,
			Detail: fmt.Sprintf(
				"canonical patch is %d bytes (max_patch_bytes=%d); no truncated prefix was retained",
				patch.size, limits.MaxPatchBytes,
			),
		})
		for index := range files {
			files[index].Included = false
			files[index].SkippedReasons = append(files[index].SkippedReasons, ReasonPatchSizeExceeded)
		}
	}
	if patch.fileHeaders != nameStatus.total {
		reasons = append(reasons, Reason{
			Code: ReasonManifestPatchMismatch,
			Detail: fmt.Sprintf(
				"name-status reported %d files but canonical patch contained %d file headers",
				nameStatus.total, patch.fileHeaders,
			),
		})
	}
	if nameStatus.total == 0 {
		reasons = append(reasons, Reason{Code: ReasonNoChanges})
	}

	staleReason, err := adapter.detectRevisionMovement(
		ctx, root, request.BaseRevision, baseOID, request.HeadRevision, headOID,
	)
	if err != nil {
		return classifyCommandFailure(ctx, ReasonGitCommandFailed, ErrorGitCommandFailed, "revision_recheck", err)
	}
	if staleReason != nil {
		reasons = append(reasons, *staleReason)
	}

	completeness := classifyCompleteness(nameStatus.total, reasons)
	includedFiles, includedHunks := coverageIncluded(files, patch.overflow)
	coverage := Coverage{
		TotalFiles:      nameStatus.total,
		IncludedFiles:   includedFiles,
		SkippedFiles:    nameStatus.total - includedFiles,
		TotalHunks:      patch.totalHunks,
		IncludedHunks:   includedHunks,
		SkippedHunks:    patch.totalHunks - includedHunks,
		DiffFileHeaders: patch.fileHeaders,
	}
	patchArtifact := PatchArtifact{
		FormatVersion: CanonicalPatchVersion,
		SHA256:        patch.sha256(),
		SizeBytes:     patch.size,
		Included:      !patch.overflow,
	}
	if patchArtifact.Included {
		patchArtifact.Data = patch.data
	}
	manifest := ChangeManifest{
		SchemaVersion: ChangeManifestSchemaVersion,
		BaseCommitOID: baseOID,
		HeadCommitOID: headOID,
		PatchSHA256:   patchArtifact.SHA256,
		PatchSize:     patchArtifact.SizeBytes,
		Files:         files,
		Coverage:      coverage,
		Completeness:  completeness,
		Reasons:       cloneReasons(reasons),
	}
	manifestDigest, err := DigestManifest(manifest)
	if err != nil {
		return failedResult(ReasonGitCommandFailed, err.Error()),
			&AdapterError{Code: ErrorGitCommandFailed, Field: "manifest", Err: err}
	}
	snapshot := TargetSnapshot{
		SchemaVersion: TargetSnapshotSchemaVersion,
		Repository: RepositorySnapshot{
			Kind:         "local_git",
			RepositoryID: repositoryID,
			ObjectFormat: objectFormat,
		},
		Base: RevisionSnapshot{
			Requested: request.BaseRevision,
			CommitOID: baseOID,
		},
		Head: RevisionSnapshot{
			Requested: request.HeadRevision,
			CommitOID: headOID,
		},
		ManifestSHA256:     manifestDigest,
		PatchSHA256:        patchArtifact.SHA256,
		PatchSizeBytes:     patchArtifact.SizeBytes,
		CanonicalPatch:     CanonicalPatchVersion,
		DirtyState:         dirtyState,
		Completeness:       completeness,
		CompletenessReason: cloneReasons(reasons),
		CapturedAt:         adapter.now().UTC(),
		CapturedBy:         capturedBy,
		GitVersion:         gitVersion,
	}
	snapshot.TargetSnapshotID, err = semanticTargetID(snapshot)
	if err != nil {
		return failedResult(ReasonGitCommandFailed, err.Error()),
			&AdapterError{Code: ErrorGitCommandFailed, Field: "snapshot_identity", Err: err}
	}
	snapshot.SHA256, err = DigestTargetSnapshot(snapshot)
	if err != nil {
		return failedResult(ReasonGitCommandFailed, err.Error()),
			&AdapterError{Code: ErrorGitCommandFailed, Field: "snapshot", Err: err}
	}
	return Result{
		RepositoryRoot: root,
		Snapshot:       &snapshot,
		Manifest:       &manifest,
		Patch:          patchArtifact,
		Completeness:   completeness,
		Reasons:        cloneReasons(reasons),
	}, nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	if limits.MaxPatchBytes < 0 {
		return Limits{}, fmt.Errorf("max_patch_bytes must not be negative")
	}
	if limits.MaxFiles < 0 {
		return Limits{}, fmt.Errorf("max_files must not be negative")
	}
	if limits.MaxPatchBytes == 0 {
		limits.MaxPatchBytes = DefaultMaxPatchBytes
	}
	if limits.MaxFiles == 0 {
		limits.MaxFiles = DefaultMaxFiles
	}
	return limits, nil
}

func validateRevision(name, revision string) error {
	if revision == "" || revision != strings.TrimSpace(revision) {
		return fmt.Errorf("%s must be non-empty and trimmed", name)
	}
	if len(revision) > 1024 || strings.HasPrefix(revision, "-") {
		return fmt.Errorf("%s is not a safe Git revision", name)
	}
	for _, character := range revision {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("%s contains an unsafe character", name)
		}
	}
	return nil
}

func validateRepositoryID(repositoryID string) error {
	if repositoryID == "" {
		return nil
	}
	if len(repositoryID) > 512 || repositoryID != strings.TrimSpace(repositoryID) {
		return fmt.Errorf("repository_id must be trimmed and at most 512 bytes")
	}
	for _, character := range repositoryID {
		if unicode.IsControl(character) {
			return fmt.Errorf("repository_id contains a control character")
		}
	}
	return nil
}

func resolveRepositoryPath(repositoryPath string) (string, Result, error) {
	if repositoryPath == "" || !filepath.IsAbs(repositoryPath) ||
		filepath.Clean(repositoryPath) != repositoryPath {
		err := errors.New("repository_path must be a clean absolute path")
		return "", failedResult(ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "repository_path", Err: err}
	}
	for _, character := range repositoryPath {
		if unicode.IsControl(character) {
			err := errors.New("repository_path contains a control character")
			return "", failedResult(ReasonInvalidRequest, err.Error()),
				&AdapterError{Code: ErrorInvalidRequest, Field: "repository_path", Err: err}
		}
	}
	info, err := os.Stat(repositoryPath)
	if err != nil {
		code := ErrorRepositoryNotFound
		reason := ReasonRepositoryNotFound
		if !errors.Is(err, os.ErrNotExist) {
			code = ErrorInvalidRequest
			reason = ReasonInvalidRequest
		}
		return "", failedResult(reason, err.Error()),
			&AdapterError{Code: code, Field: "repository_path", Err: err}
	}
	if !info.IsDir() {
		err := errors.New("repository_path is not a directory")
		return "", failedResult(ReasonRepositoryNotDir, err.Error()),
			&AdapterError{Code: ErrorRepositoryNotDir, Field: "repository_path", Err: err}
	}
	canonicalPath, err := filepath.EvalSymlinks(repositoryPath)
	if err != nil {
		return "", failedResult(ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "repository_path", Err: err}
	}
	if !filepath.IsAbs(canonicalPath) {
		err := errors.New("canonical repository_path is not absolute")
		return "", failedResult(ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "repository_path", Err: err}
	}
	return filepath.Clean(canonicalPath), Result{}, nil
}

func (adapter *Adapter) inspectRepository(
	ctx context.Context,
	repositoryPath string,
) (string, string, error) {
	inside, err := adapter.runOutput(ctx, repositoryPath, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", "", err
	}
	if trimLine(inside) != "true" {
		return "", "", errors.New("path is not inside a Git work tree")
	}
	rootOutput, err := adapter.runOutput(ctx, repositoryPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	root := trimLine(rootOutput)
	if root == "" || !filepath.IsAbs(root) {
		return "", "", errors.New("Git returned an invalid work-tree root")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("canonicalize Git work-tree root: %w", err)
	}
	relative, err := filepath.Rel(root, repositoryPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("Git work-tree root does not contain repository_path")
	}
	formatOutput, err := adapter.runOutput(ctx, root, "rev-parse", "--show-object-format")
	if err != nil {
		return "", "", err
	}
	objectFormat := trimLine(formatOutput)
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return "", "", fmt.Errorf("unsupported Git object format %q", objectFormat)
	}
	return filepath.Clean(root), objectFormat, nil
}

func (adapter *Adapter) version(ctx context.Context) (string, error) {
	output, err := adapter.runOutput(ctx, "", "version")
	if err != nil {
		return "", err
	}
	version := trimLine(output)
	if version == "" || strings.Contains(version, "\n") {
		return "", errors.New("Git returned an invalid version")
	}
	return version, nil
}

func (adapter *Adapter) resolveRevision(
	ctx context.Context,
	root string,
	revision string,
) (string, error) {
	output, err := adapter.runOutput(
		ctx,
		root,
		"rev-parse",
		"--verify",
		"--end-of-options",
		revision+"^{commit}",
	)
	if err != nil {
		return "", err
	}
	oid := trimLine(output)
	expectedLength := 40
	if len(oid) == 64 {
		expectedLength = 64
	}
	if len(oid) != expectedLength || !isLowerHex(oid) {
		return "", fmt.Errorf("Git returned an invalid commit object ID")
	}
	return oid, nil
}

func (adapter *Adapter) readDirtyState(
	ctx context.Context,
	root string,
) (DirtyState, *Reason, error) {
	output := &anyOutputWriter{}
	err := adapter.run(ctx, root, []string{
		"status",
		"--porcelain=v1",
		"-z",
		"--untracked-files=normal",
		"--ignore-submodules=all",
	}, output)
	if err == nil {
		if output.any {
			return DirtyStateDirty, nil, nil
		}
		return DirtyStateClean, nil, nil
	}
	if ctx.Err() != nil {
		return DirtyStateUnknown, nil, ctx.Err()
	}
	return DirtyStateUnknown, &Reason{
		Code:   ReasonDirtyStateUnknown,
		Detail: "git status failed; commit-to-commit diff remains exact",
	}, nil
}

func (adapter *Adapter) detectRevisionMovement(
	ctx context.Context,
	root string,
	baseRevision string,
	baseOID string,
	headRevision string,
	headOID string,
) (*Reason, error) {
	currentBase, baseErr := adapter.resolveRevision(ctx, root, baseRevision)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	currentHead, headErr := adapter.resolveRevision(ctx, root, headRevision)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if baseErr == nil && headErr == nil && currentBase == baseOID && currentHead == headOID {
		return nil, nil
	}
	detail := fmt.Sprintf(
		"requested revisions changed or disappeared after resolving base=%s head=%s",
		baseOID, headOID,
	)
	return &Reason{Code: ReasonRevisionMoved, Detail: detail}, nil
}

func canonicalNameStatusArgs(baseOID, headOID string) []string {
	return []string{
		"diff",
		"--name-status",
		"-z",
		"--find-renames=50%",
		"--diff-filter=ACDMRTUXB",
		baseOID,
		headOID,
		"--",
	}
}

func canonicalPatchArgs(baseOID, headOID string) []string {
	return []string{
		"diff",
		"--binary",
		"--full-index",
		"--no-color",
		"--no-ext-diff",
		"--no-textconv",
		"--find-renames=50%",
		"--diff-algorithm=myers",
		"--no-indent-heuristic",
		"--unified=3",
		"--src-prefix=a/",
		"--dst-prefix=b/",
		baseOID,
		headOID,
		"--",
	}
}

func coverageIncluded(files []FileChange, patchOverflow bool) (int, int) {
	if patchOverflow {
		return 0, 0
	}
	included := 0
	hunks := 0
	for _, file := range files {
		if file.Included {
			included++
			hunks += file.HunkCount
		}
	}
	return included, hunks
}

func classifyCompleteness(totalFiles int, reasons []Reason) Completeness {
	if totalFiles == 0 {
		for _, reason := range reasons {
			if reason.Code != ReasonNoChanges {
				return CompletenessPartial
			}
		}
		return CompletenessSkipped
	}
	if len(reasons) != 0 {
		return CompletenessPartial
	}
	return CompletenessComplete
}

func semanticTargetID(snapshot TargetSnapshot) (string, error) {
	identity := struct {
		Repository     RepositorySnapshot
		Base           RevisionSnapshot
		Head           RevisionSnapshot
		ManifestSHA256 string
		PatchSHA256    string
		Completeness   Completeness
		Reasons        []Reason
	}{
		Repository:     snapshot.Repository,
		Base:           snapshot.Base,
		Head:           snapshot.Head,
		ManifestSHA256: snapshot.ManifestSHA256,
		PatchSHA256:    snapshot.PatchSHA256,
		Completeness:   snapshot.Completeness,
		Reasons:        snapshot.CompletenessReason,
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal target identity: %w", err)
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("target_%x", digest[:16]), nil
}

func containsReasonCode(reasons []ReasonCode, code ReasonCode) bool {
	for _, reason := range reasons {
		if reason == code {
			return true
		}
	}
	return false
}

func isLowerHex(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func trimLine(output []byte) string {
	return strings.TrimSuffix(string(output), "\n")
}

func cloneReasons(reasons []Reason) []Reason {
	return append([]Reason{}, reasons...)
}

func failedResult(code ReasonCode, detail string) Result {
	reason := Reason{Code: code, Detail: detail}
	return Result{
		Patch: PatchArtifact{
			FormatVersion: CanonicalPatchVersion,
			Included:      false,
		},
		Completeness: CompletenessSkipped,
		Reasons:      []Reason{reason},
	}
}

func classifyRevisionFailure(
	ctx context.Context,
	reason ReasonCode,
	code ErrorCode,
	field string,
	err error,
) (Result, error) {
	if ctx.Err() != nil {
		return failedResult(ReasonCancelled, ctx.Err().Error()),
			&AdapterError{Code: ErrorCancelled, Field: field, Err: ctx.Err()}
	}
	return failedResult(reason, err.Error()), &AdapterError{Code: code, Field: field, Err: err}
}

func classifyCommandFailure(
	ctx context.Context,
	reason ReasonCode,
	code ErrorCode,
	field string,
	err error,
) (Result, error) {
	if ctx.Err() != nil {
		return failedResult(ReasonCancelled, ctx.Err().Error()),
			&AdapterError{Code: ErrorCancelled, Field: field, Err: ctx.Err()}
	}
	return failedResult(reason, err.Error()), &AdapterError{Code: code, Field: field, Err: err}
}
