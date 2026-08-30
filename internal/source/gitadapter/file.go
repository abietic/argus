package gitadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ConcurrentReadLimit advertises that exact-object reads are safe to execute
// concurrently. Callers remain responsible for applying their own tighter
// resource boundary.
func (adapter *Adapter) ConcurrentReadLimit() int {
	if adapter == nil || adapter.gitPath == "" {
		return 1
	}
	return 16
}

// ReadFileAtCommit reads an ordinary file blob from an exact commit. The
// repository-relative path is passed to ls-tree as a literal pathspec after
// "--"; cat-file receives only the resolved blob OID. The path is never joined
// with a revision expression.
func (adapter *Adapter) ReadFileAtCommit(
	ctx context.Context,
	repositoryRoot string,
	commitOID string,
	repositoryPath string,
	maxBytes int64,
) (FileContent, error) {
	if adapter == nil || adapter.gitPath == "" {
		return failedFileContent(commitOID, repositoryPath, ReasonGitCommandFailed, "adapter is not initialized"),
			&AdapterError{Code: ErrorGitUnavailable, Err: errors.New("adapter is not initialized")}
	}
	if ctx == nil {
		return failedFileContent(commitOID, repositoryPath, ReasonInvalidRequest, "context is nil"),
			&AdapterError{Code: ErrorInvalidRequest, Field: "context", Err: errors.New("must not be nil")}
	}
	if maxBytes < 0 {
		err := errors.New("max_bytes must not be negative")
		return failedFileContent(commitOID, repositoryPath, ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "max_bytes", Err: err}
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxFileBytes
	}
	if !isExactObjectOID(commitOID) {
		err := errors.New("commit_oid must be an exact lowercase 40- or 64-character object ID")
		return failedFileContent(commitOID, repositoryPath, ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "commit_oid", Err: err}
	}
	if !isPortableRepositoryPath(repositoryPath) {
		err := errors.New("path must satisfy the portable repository-relative path grammar")
		return failedFileContent(commitOID, repositoryPath, ReasonUnsafeRepositoryPath, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "path", Err: err}
	}

	canonicalPath, result, err := resolveRepositoryPath(repositoryRoot)
	if err != nil {
		return failedFileContent(
			commitOID,
			repositoryPath,
			result.Reasons[0].Code,
			result.Reasons[0].Detail,
		), err
	}
	root, objectFormat, err := adapter.inspectRepository(ctx, canonicalPath)
	if err != nil {
		return classifyFileCommandFailure(
			ctx, commitOID, repositoryPath, ReasonRepositoryNotGit,
			ErrorRepositoryNotGit, "repository_root", err,
		)
	}
	if root != canonicalPath {
		err := fmt.Errorf("repository_root resolves to a work-tree subdirectory")
		return failedFileContent(commitOID, repositoryPath, ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "repository_root", Err: err}
	}
	if objectFormat == "sha1" && len(commitOID) != 40 ||
		objectFormat == "sha256" && len(commitOID) != 64 {
		err := fmt.Errorf("commit_oid length does not match repository object format %s", objectFormat)
		return failedFileContent(commitOID, repositoryPath, ReasonInvalidRequest, err.Error()),
			&AdapterError{Code: ErrorInvalidRequest, Field: "commit_oid", Err: err}
	}
	resolvedCommit, err := adapter.resolveRevision(ctx, root, commitOID)
	if err != nil || resolvedCommit != commitOID {
		if ctx.Err() != nil {
			return failedFileContent(commitOID, repositoryPath, ReasonCancelled, ctx.Err().Error()),
				&AdapterError{Code: ErrorCancelled, Field: "commit_oid", Err: ctx.Err()}
		}
		if err == nil {
			err = errors.New("resolved commit does not match requested exact OID")
		}
		return failedFileContent(commitOID, repositoryPath, ReasonCommitMissing, err.Error()),
			&AdapterError{Code: ErrorCommitMissing, Field: "commit_oid", Err: err}
	}

	output, err := adapter.runOutput(
		ctx,
		root,
		"ls-tree",
		"-z",
		"--full-tree",
		commitOID,
		"--",
		repositoryPath,
	)
	if err != nil {
		return classifyFileCommandFailure(
			ctx, commitOID, repositoryPath, ReasonGitCommandFailed,
			ErrorGitCommandFailed, "ls_tree", err,
		)
	}
	entry, found, err := parseTreeEntry(output, repositoryPath)
	if err != nil {
		return failedFileContent(commitOID, repositoryPath, ReasonGitCommandFailed, err.Error()),
			&AdapterError{Code: ErrorGitCommandFailed, Field: "ls_tree", Err: err}
	}
	if !found {
		return failedFileContent(commitOID, repositoryPath, ReasonFileNotFound, "path does not exist at commit"), nil
	}
	content := FileContent{
		SchemaVersion: FileContentSchemaVersion,
		CommitOID:     commitOID,
		Path:          repositoryPath,
		Mode:          entry.mode,
		BlobOID:       entry.objectOID,
		Included:      false,
		Completeness:  CompletenessSkipped,
		Reasons:       []Reason{},
	}
	switch {
	case entry.mode == "120000":
		content.Reasons = []Reason{{Code: ReasonSymlinkNotAllowed}}
		return content, nil
	case entry.mode == "160000" || entry.objectType == "commit":
		content.Reasons = []Reason{{Code: ReasonSubmoduleNotAllowed}}
		return content, nil
	case entry.objectType != "blob" || entry.mode != "100644" && entry.mode != "100755":
		content.Reasons = []Reason{{
			Code:   ReasonNonRegularFile,
			Detail: fmt.Sprintf("unsupported tree entry mode=%s type=%s", entry.mode, entry.objectType),
		}}
		return content, nil
	}

	sizeOutput, err := adapter.runOutput(ctx, root, "cat-file", "-s", entry.objectOID)
	if err != nil {
		return classifyFileCommandFailure(
			ctx, commitOID, repositoryPath, ReasonGitCommandFailed,
			ErrorGitCommandFailed, "cat_file_size", err,
		)
	}
	blobSize, err := parseBlobSize(sizeOutput)
	if err != nil {
		return failedFileContent(
				commitOID,
				repositoryPath,
				ReasonGitCommandFailed,
				err.Error(),
			),
			&AdapterError{Code: ErrorGitCommandFailed, Field: "cat_file_size", Err: err}
	}
	content.SizeBytes = blobSize
	if blobSize > maxBytes {
		content.Completeness = CompletenessPartial
		content.Reasons = []Reason{{
			Code: ReasonFileSizeExceeded,
			Detail: fmt.Sprintf(
				"Git blob is %d bytes (max_bytes=%d); content was not streamed",
				blobSize,
				maxBytes,
			),
		}}
		return content, nil
	}

	collector := newContentCollector(maxBytes)
	if err := adapter.run(ctx, root, []string{"cat-file", "blob", entry.objectOID}, collector); err != nil {
		return classifyFileCommandFailure(
			ctx, commitOID, repositoryPath, ReasonGitCommandFailed,
			ErrorGitCommandFailed, "cat_file", err,
		)
	}
	content.SHA256 = collector.sha256()
	content.SizeBytes = collector.size
	if collector.size != blobSize {
		err := fmt.Errorf(
			"cat-file blob size %d differs from immutable metadata size %d",
			collector.size,
			blobSize,
		)
		return failedFileContent(
				commitOID,
				repositoryPath,
				ReasonGitCommandFailed,
				err.Error(),
			),
			&AdapterError{Code: ErrorGitCommandFailed, Field: "cat_file", Err: err}
	}
	switch {
	case collector.containsNUL:
		content.Completeness = CompletenessSkipped
		content.Reasons = []Reason{{Code: ReasonBinaryFile}}
	case collector.overflow:
		content.Completeness = CompletenessPartial
		content.Reasons = []Reason{{
			Code: ReasonFileSizeExceeded,
			Detail: fmt.Sprintf(
				"file is %d bytes (max_bytes=%d); no truncated prefix was retained",
				collector.size, maxBytes,
			),
		}}
	case !utf8.Valid(collector.data):
		content.Completeness = CompletenessSkipped
		content.Reasons = []Reason{{Code: ReasonUnsupportedEncoding}}
	case bytes.IndexByte(collector.data, '\r') >= 0:
		content.Completeness = CompletenessSkipped
		content.Reasons = []Reason{{
			Code:   ReasonNonCanonicalLineEndings,
			Detail: "file contains CR bytes; only LF text is reviewable",
		}}
	default:
		content.Included = true
		content.Completeness = CompletenessComplete
		content.Data = collector.data
	}
	return content, nil
}

func parseBlobSize(output []byte) (int64, error) {
	raw := strings.TrimSuffix(string(output), "\n")
	if raw == "" || strings.TrimSpace(raw) != raw {
		return 0, errors.New("git cat-file -s returned invalid whitespace")
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("git cat-file -s returned invalid size %q", raw)
	}
	return size, nil
}

type treeEntry struct {
	mode       string
	objectType string
	objectOID  string
}

func parseTreeEntry(output []byte, expectedPath string) (treeEntry, bool, error) {
	if len(output) == 0 {
		return treeEntry{}, false, nil
	}
	if output[len(output)-1] != 0 {
		return treeEntry{}, false, errors.New("ls-tree output is not NUL terminated")
	}
	records := bytes.Split(output[:len(output)-1], []byte{0})
	if len(records) != 1 {
		return treeEntry{}, false, fmt.Errorf("ls-tree returned %d records for one literal path", len(records))
	}
	metadata, rawPath, found := bytes.Cut(records[0], []byte{'\t'})
	if !found || string(rawPath) != expectedPath {
		return treeEntry{}, false, errors.New("ls-tree returned a mismatched path")
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 3 {
		return treeEntry{}, false, errors.New("ls-tree returned invalid metadata")
	}
	if _, err := strconv.ParseUint(fields[0], 8, 32); err != nil {
		return treeEntry{}, false, errors.New("ls-tree returned an invalid mode")
	}
	if !isExactObjectOID(fields[2]) {
		return treeEntry{}, false, errors.New("ls-tree returned an invalid object ID")
	}
	return treeEntry{
		mode:       fields[0],
		objectType: fields[1],
		objectOID:  fields[2],
	}, true, nil
}

func isExactObjectOID(oid string) bool {
	return (len(oid) == 40 || len(oid) == 64) && isLowerHex(oid)
}

func failedFileContent(
	commitOID string,
	path string,
	code ReasonCode,
	detail string,
) FileContent {
	return FileContent{
		SchemaVersion: FileContentSchemaVersion,
		CommitOID:     commitOID,
		Path:          path,
		Included:      false,
		Completeness:  CompletenessSkipped,
		Reasons:       []Reason{{Code: code, Detail: detail}},
	}
}

func classifyFileCommandFailure(
	ctx context.Context,
	commitOID string,
	path string,
	reason ReasonCode,
	code ErrorCode,
	field string,
	err error,
) (FileContent, error) {
	if ctx.Err() != nil {
		return failedFileContent(commitOID, path, ReasonCancelled, ctx.Err().Error()),
			&AdapterError{Code: ErrorCancelled, Field: field, Err: ctx.Err()}
	}
	return failedFileContent(commitOID, path, reason, err.Error()),
		&AdapterError{Code: code, Field: field, Err: err}
}
