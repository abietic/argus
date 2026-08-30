package gitadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	RevisionCaptureSchemaVersion  = "argus.git_revision_capture.v1alpha1"
	RevisionFileListSchemaVersion = "argus.git_revision_file_list.v1alpha1"
	MaxScopePatternBytes          = 1024
)

type RevisionRequest struct {
	RepositoryPath string
	RepositoryID   string
	Revision       string
}

// RevisionCapture freezes one caller-supplied revision to an exact commit and
// records the mutable working-tree state separately. RepositoryRoot is local
// runtime information; Repository and Revision form the portable identity.
type RevisionCapture struct {
	SchemaVersion  string             `json:"schema_version"`
	RepositoryRoot string             `json:"repository_root"`
	Repository     RepositorySnapshot `json:"repository"`
	Revision       RevisionSnapshot   `json:"revision"`
	DirtyState     DirtyState         `json:"dirty_state"`
	Reasons        []Reason           `json:"reasons"`
	CapturedAt     time.Time          `json:"captured_at"`
	CapturedBy     string             `json:"captured_by"`
	GitVersion     string             `json:"git_version"`
}

type RevisionState string

const (
	RevisionStateUnchanged RevisionState = "unchanged"
	RevisionStateMoved     RevisionState = "moved"
	RevisionStateMissing   RevisionState = "missing"
)

type RevisionRevalidation struct {
	Requested         string        `json:"requested"`
	ExpectedCommitOID string        `json:"expected_commit_oid"`
	ActualCommitOID   string        `json:"actual_commit_oid,omitempty"`
	State             RevisionState `json:"state"`
	Reasons           []Reason      `json:"reasons"`
}

type RevisionFileEntry struct {
	Mode      string `json:"mode"`
	Type      string `json:"type"`
	ObjectOID string `json:"object_oid"`
	Path      string `json:"path"`
	Language  string `json:"language"`
}

type RevisionFileCoverage struct {
	// ScannedFiles counts every recursive ls-tree entry, even after the
	// retention limit has been reached.
	ScannedFiles int `json:"scanned_files"`
	// MatchedFiles counts include matches after exclude-wins filtering.
	MatchedFiles int `json:"matched_files"`
	// RetainedFiles and SkippedFiles partition MatchedFiles exactly.
	RetainedFiles int `json:"retained_files"`
	SkippedFiles  int `json:"skipped_files"`
	// ExcludedFiles is the sum of request- and policy-excluded files. Files
	// outside the request include set are only reflected in ScannedFiles.
	ExcludedFiles        int `json:"excluded_files"`
	RequestExcludedFiles int `json:"request_excluded_files"`
	PolicyExcludedFiles  int `json:"policy_excluded_files"`
}

type RevisionFileList struct {
	SchemaVersion string               `json:"schema_version"`
	CommitOID     string               `json:"commit_oid"`
	Include       []string             `json:"include"`
	Exclude       []string             `json:"exclude"`
	PolicyInclude []string             `json:"policy_include"`
	PolicyExclude []string             `json:"policy_exclude"`
	Files         []RevisionFileEntry  `json:"files"`
	Coverage      RevisionFileCoverage `json:"coverage"`
	Completeness  Completeness         `json:"completeness"`
	Reasons       []Reason             `json:"reasons"`
}

// ScopeAdmission keeps caller-selected scope and resolved configuration policy
// distinct while applying their intersection before any retention quota.
// Request excludes win before policy evaluation; policy excludes then win.
type ScopeAdmission struct {
	Include       []string
	Exclude       []string
	PolicyInclude []string
	PolicyExclude []string
}

// CaptureRevision resolves a safe revision once and records the exact commit
// plus capture metadata. It accepts only the canonical repository root, not an
// arbitrary work-tree subdirectory.
func (adapter *Adapter) CaptureRevision(
	ctx context.Context,
	request RevisionRequest,
) (RevisionCapture, error) {
	if err := validateAdapterContext(adapter, ctx); err != nil {
		return RevisionCapture{}, err
	}
	if err := validateRevision("revision", request.Revision); err != nil {
		return RevisionCapture{}, &AdapterError{
			Code: ErrorInvalidRequest, Field: "revision", Err: err,
		}
	}
	if err := validateRepositoryID(request.RepositoryID); err != nil {
		return RevisionCapture{}, &AdapterError{
			Code: ErrorInvalidRequest, Field: "repository_id", Err: err,
		}
	}
	root, objectFormat, err := adapter.openExactRepository(ctx, request.RepositoryPath)
	if err != nil {
		return RevisionCapture{}, err
	}
	commitOID, err := adapter.resolveRevision(ctx, root, request.Revision)
	if err != nil {
		if ctx.Err() != nil {
			return RevisionCapture{}, &AdapterError{
				Code: ErrorCancelled, Field: "revision", Err: ctx.Err(),
			}
		}
		return RevisionCapture{}, &AdapterError{
			Code: ErrorRevisionMissing, Field: "revision", Err: err,
		}
	}
	dirtyState, dirtyReason, err := adapter.readDirtyState(ctx, root)
	if err != nil {
		if ctx.Err() != nil {
			return RevisionCapture{}, &AdapterError{
				Code: ErrorCancelled, Field: "dirty_state", Err: ctx.Err(),
			}
		}
		return RevisionCapture{}, &AdapterError{
			Code: ErrorGitCommandFailed, Field: "dirty_state", Err: err,
		}
	}
	gitVersion, err := adapter.version(ctx)
	if err != nil {
		return RevisionCapture{}, &AdapterError{
			Code: ErrorGitCommandFailed, Field: "git_version", Err: err,
		}
	}
	repositoryID := request.RepositoryID
	if repositoryID == "" {
		repositoryID = localRepositoryID(root)
	}
	reasons := []Reason{}
	if dirtyReason != nil {
		reasons = append(reasons, *dirtyReason)
	}
	return RevisionCapture{
		SchemaVersion:  RevisionCaptureSchemaVersion,
		RepositoryRoot: root,
		Repository: RepositorySnapshot{
			Kind:         "local_git",
			RepositoryID: repositoryID,
			ObjectFormat: objectFormat,
		},
		Revision: RevisionSnapshot{
			Requested: request.Revision,
			CommitOID: commitOID,
		},
		DirtyState: dirtyState,
		Reasons:    reasons,
		CapturedAt: adapter.now().UTC(),
		CapturedBy: capturedBy,
		GitVersion: gitVersion,
	}, nil
}

// RevalidateRevision compares the current resolution of requested with an
// earlier exact commit. A deleted ref is an explicit missing state rather than
// being conflated with an unchanged revision.
func (adapter *Adapter) RevalidateRevision(
	ctx context.Context,
	repositoryRoot string,
	requested string,
	expectedCommitOID string,
) (RevisionRevalidation, error) {
	result := RevisionRevalidation{
		Requested:         requested,
		ExpectedCommitOID: expectedCommitOID,
		Reasons:           []Reason{},
	}
	if err := validateAdapterContext(adapter, ctx); err != nil {
		return result, err
	}
	if err := validateRevision("requested", requested); err != nil {
		return result, &AdapterError{
			Code: ErrorInvalidRequest, Field: "requested", Err: err,
		}
	}
	root, objectFormat, err := adapter.openExactRepository(ctx, repositoryRoot)
	if err != nil {
		return result, err
	}
	if !exactOIDMatchesFormat(expectedCommitOID, objectFormat) {
		return result, &AdapterError{
			Code:  ErrorInvalidRequest,
			Field: "expected_commit_oid",
			Err: fmt.Errorf(
				"expected_commit_oid must be an exact lowercase %s object ID",
				objectFormat,
			),
		}
	}
	actual, err := adapter.resolveRevision(ctx, root, requested)
	if err != nil {
		if ctx.Err() != nil {
			return result, &AdapterError{
				Code: ErrorCancelled, Field: "requested", Err: ctx.Err(),
			}
		}
		result.State = RevisionStateMissing
		result.Reasons = []Reason{{
			Code: ReasonRevisionMissing,
			Detail: fmt.Sprintf(
				"requested revision no longer resolves; expected=%s",
				expectedCommitOID,
			),
		}}
		return result, nil
	}
	result.ActualCommitOID = actual
	if actual == expectedCommitOID {
		result.State = RevisionStateUnchanged
		return result, nil
	}
	result.State = RevisionStateMoved
	result.Reasons = []Reason{{
		Code: ReasonRevisionMoved,
		Detail: fmt.Sprintf(
			"requested revision moved from %s to %s",
			expectedCommitOID,
			actual,
		),
	}}
	return result, nil
}

// ListFilesAtCommit streams the complete recursive tree for one exact commit.
// Include/exclude matching happens in Argus rather than through Git pathspecs.
// This compatibility entry point applies no additional target policy. New
// materialization code must use ListFilesAtCommitWithAdmission.
func (adapter *Adapter) ListFilesAtCommit(
	ctx context.Context,
	repositoryRoot string,
	exactCommitOID string,
	include []string,
	exclude []string,
	maxFiles int,
) (RevisionFileList, error) {
	return adapter.ListFilesAtCommitWithAdmission(
		ctx,
		repositoryRoot,
		exactCommitOID,
		ScopeAdmission{
			Include:       include,
			Exclude:       exclude,
			PolicyInclude: []string{"**"},
			PolicyExclude: []string{},
		},
		maxFiles,
	)
}

// ListFilesAtCommitWithAdmission applies both the request scope and the
// resolved, frozen target policy before maxFiles. This prevents denied paths
// from consuming the quota and hiding later authorized files.
func (adapter *Adapter) ListFilesAtCommitWithAdmission(
	ctx context.Context,
	repositoryRoot string,
	exactCommitOID string,
	admission ScopeAdmission,
	maxFiles int,
) (RevisionFileList, error) {
	result := RevisionFileList{
		SchemaVersion: RevisionFileListSchemaVersion,
		CommitOID:     exactCommitOID,
		Include:       cloneStrings(admission.Include),
		Exclude:       cloneStrings(admission.Exclude),
		PolicyInclude: cloneStrings(admission.PolicyInclude),
		PolicyExclude: cloneStrings(admission.PolicyExclude),
		Files:         []RevisionFileEntry{},
		Reasons:       []Reason{},
	}
	if err := validateAdapterContext(adapter, ctx); err != nil {
		return result, err
	}
	if maxFiles < 0 {
		return result, &AdapterError{
			Code: ErrorInvalidRequest, Field: "max_files",
			Err: errors.New("max_files must not be negative"),
		}
	}
	if maxFiles == 0 {
		maxFiles = DefaultMaxFiles
	}
	requestInclude, normalizedInclude, err := compileScopePatterns(
		admission.Include,
		true,
	)
	if err != nil {
		return result, &AdapterError{
			Code: ErrorInvalidRequest, Field: "include", Err: err,
		}
	}
	requestExclude, normalizedExclude, err := compileScopePatterns(
		admission.Exclude,
		false,
	)
	if err != nil {
		return result, &AdapterError{
			Code: ErrorInvalidRequest, Field: "exclude", Err: err,
		}
	}
	policyInclude, normalizedPolicyInclude, err := compileScopePatterns(
		admission.PolicyInclude,
		true,
	)
	if err != nil {
		return result, &AdapterError{
			Code: ErrorInvalidRequest, Field: "policy_include", Err: err,
		}
	}
	policyExclude, normalizedPolicyExclude, err := compileScopePatterns(
		admission.PolicyExclude,
		false,
	)
	if err != nil {
		return result, &AdapterError{
			Code: ErrorInvalidRequest, Field: "policy_exclude", Err: err,
		}
	}
	result.Include = normalizedInclude
	result.Exclude = normalizedExclude
	result.PolicyInclude = normalizedPolicyInclude
	result.PolicyExclude = normalizedPolicyExclude

	root, objectFormat, err := adapter.openExactRepository(ctx, repositoryRoot)
	if err != nil {
		return result, err
	}
	if !exactOIDMatchesFormat(exactCommitOID, objectFormat) {
		return result, &AdapterError{
			Code: ErrorInvalidRequest, Field: "exact_commit_oid",
			Err: fmt.Errorf(
				"exact_commit_oid must be an exact lowercase %s object ID",
				objectFormat,
			),
		}
	}
	resolved, err := adapter.resolveRevision(ctx, root, exactCommitOID)
	if err != nil || resolved != exactCommitOID {
		if ctx.Err() != nil {
			return result, &AdapterError{
				Code: ErrorCancelled, Field: "exact_commit_oid", Err: ctx.Err(),
			}
		}
		if err == nil {
			err = errors.New("resolved commit does not match exact_commit_oid")
		}
		return result, &AdapterError{
			Code: ErrorCommitMissing, Field: "exact_commit_oid", Err: err,
		}
	}

	collector := newRevisionFileCollector(
		objectFormat,
		requestInclude,
		requestExclude,
		policyInclude,
		policyExclude,
		maxFiles,
	)
	if err := adapter.run(
		ctx,
		root,
		[]string{
			"ls-tree",
			"-r",
			"-z",
			"--full-tree",
			exactCommitOID,
			"--",
		},
		collector,
	); err != nil {
		if ctx.Err() != nil {
			return result, &AdapterError{
				Code: ErrorCancelled, Field: "ls_tree", Err: ctx.Err(),
			}
		}
		return result, &AdapterError{
			Code: ErrorGitCommandFailed, Field: "ls_tree", Err: err,
		}
	}
	if err := collector.finish(); err != nil {
		return result, &AdapterError{
			Code: ErrorGitCommandFailed, Field: "ls_tree", Err: err,
		}
	}
	result.Files = collector.files
	result.Coverage = collector.coverage
	switch {
	case result.Coverage.MatchedFiles == 0:
		result.Completeness = CompletenessSkipped
		result.Reasons = []Reason{{Code: ReasonNoMatchingFiles}}
	case result.Coverage.SkippedFiles != 0:
		result.Completeness = CompletenessPartial
		result.Reasons = []Reason{{
			Code: ReasonFileLimitExceeded,
			Detail: fmt.Sprintf(
				"retained %d of %d matched files (max_files=%d)",
				result.Coverage.RetainedFiles,
				result.Coverage.MatchedFiles,
				maxFiles,
			),
		}}
	default:
		result.Completeness = CompletenessComplete
	}
	return result, nil
}

func validateAdapterContext(adapter *Adapter, ctx context.Context) error {
	if adapter == nil || adapter.gitPath == "" {
		return &AdapterError{
			Code: ErrorGitUnavailable, Err: errors.New("adapter is not initialized"),
		}
	}
	if ctx == nil {
		return &AdapterError{
			Code: ErrorInvalidRequest, Field: "context", Err: errors.New("must not be nil"),
		}
	}
	if err := ctx.Err(); err != nil {
		return &AdapterError{Code: ErrorCancelled, Field: "context", Err: err}
	}
	return nil
}

func (adapter *Adapter) openExactRepository(
	ctx context.Context,
	repositoryPath string,
) (string, string, error) {
	canonical, _, err := resolveRepositoryPath(repositoryPath)
	if err != nil {
		return "", "", err
	}
	root, objectFormat, err := adapter.inspectRepository(ctx, canonical)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", &AdapterError{
				Code: ErrorCancelled, Field: "repository_path", Err: ctx.Err(),
			}
		}
		return "", "", &AdapterError{
			Code: ErrorRepositoryNotGit, Field: "repository_path", Err: err,
		}
	}
	if root != canonical {
		return "", "", &AdapterError{
			Code: ErrorInvalidRequest, Field: "repository_path",
			Err: errors.New("repository_path must be the canonical Git work-tree root"),
		}
	}
	return root, objectFormat, nil
}

func localRepositoryID(root string) string {
	digest := sha256.Sum256([]byte(root))
	return fmt.Sprintf("local-%x", digest[:16])
}

func exactOIDMatchesFormat(oid string, objectFormat string) bool {
	switch objectFormat {
	case "sha1":
		return len(oid) == 40 && isLowerHex(oid)
	case "sha256":
		return len(oid) == 64 && isLowerHex(oid)
	default:
		return false
	}
}

type scopeMatcher struct {
	expressions []*regexp.Regexp
}

func (matcher scopeMatcher) matches(repositoryPath string) bool {
	for _, expression := range matcher.expressions {
		if expression.MatchString(repositoryPath) {
			return true
		}
	}
	return false
}

func compileScopePatterns(
	patterns []string,
	defaultAll bool,
) (scopeMatcher, []string, error) {
	normalized := cloneStrings(patterns)
	if len(normalized) == 0 {
		if defaultAll {
			normalized = []string{"**"}
		} else {
			return scopeMatcher{expressions: []*regexp.Regexp{}}, []string{}, nil
		}
	}
	expressions := make([]*regexp.Regexp, 0, len(normalized))
	for index, pattern := range normalized {
		expression, err := compileScopePattern(pattern)
		if err != nil {
			return scopeMatcher{}, nil, fmt.Errorf("pattern %d %q: %w", index, pattern, err)
		}
		expressions = append(expressions, expression)
	}
	return scopeMatcher{expressions: expressions}, normalized, nil
}

// ScopeIncludesPath applies the exact include/exclude grammar used by
// ListFilesAtCommit. It lets later admission and persistence checks prove that
// a frozen path stayed inside the originally authorized scope.
func ScopeIncludesPath(
	repositoryPath string,
	include []string,
	exclude []string,
) (bool, error) {
	return ScopeAdmissionIncludesPath(repositoryPath, ScopeAdmission{
		Include:       include,
		Exclude:       exclude,
		PolicyInclude: []string{"**"},
		PolicyExclude: []string{},
	})
}

// ScopeAdmissionIncludesPath applies the same two-layer matcher as
// ListFilesAtCommitWithAdmission. Portability is checked only after a path
// matches the final authorized target, so an unsafe path outside that target
// cannot fail an otherwise valid review.
func ScopeAdmissionIncludesPath(
	repositoryPath string,
	admission ScopeAdmission,
) (bool, error) {
	requestInclude, _, err := compileScopePatterns(admission.Include, true)
	if err != nil {
		return false, fmt.Errorf("compile include patterns: %w", err)
	}
	requestExclude, _, err := compileScopePatterns(admission.Exclude, false)
	if err != nil {
		return false, fmt.Errorf("compile exclude patterns: %w", err)
	}
	policyInclude, _, err := compileScopePatterns(admission.PolicyInclude, true)
	if err != nil {
		return false, fmt.Errorf("compile policy include patterns: %w", err)
	}
	policyExclude, _, err := compileScopePatterns(admission.PolicyExclude, false)
	if err != nil {
		return false, fmt.Errorf("compile policy exclude patterns: %w", err)
	}
	allowed := requestInclude.matches(repositoryPath) &&
		!requestExclude.matches(repositoryPath) &&
		policyInclude.matches(repositoryPath) &&
		!policyExclude.matches(repositoryPath)
	if !allowed {
		return false, nil
	}
	if !isPortableRepositoryPath(repositoryPath) {
		return false, fmt.Errorf("authorized path must be a portable repository-relative path")
	}
	return true, nil
}

func compileScopePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || len(pattern) > MaxScopePatternBytes ||
		!utf8.ValidString(pattern) || pattern != strings.TrimSpace(pattern) {
		return nil, errors.New("pattern must be non-empty, valid UTF-8, trimmed, and within size limit")
	}
	if path.IsAbs(pattern) || strings.ContainsAny(pattern, `\:`) ||
		strings.ContainsAny(pattern, "?[]") ||
		strings.Contains(pattern, "//") {
		return nil, errors.New("pattern must be a portable repository-relative glob")
	}
	for _, character := range pattern {
		if unicode.IsControl(character) {
			return nil, errors.New("pattern contains a control character")
		}
	}
	segments := strings.Split(pattern, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, errors.New("pattern contains an unsafe path segment")
		}
		if strings.Contains(segment, "***") {
			return nil, errors.New("pattern contains an ambiguous star run")
		}
	}

	var expression strings.Builder
	expression.WriteByte('^')
	characters := []rune(pattern)
	for index := 0; index < len(characters); {
		switch {
		case index+2 < len(characters) &&
			characters[index] == '*' &&
			characters[index+1] == '*' &&
			characters[index+2] == '/':
			expression.WriteString("(?:.*/)?")
			index += 3
		case index+1 < len(characters) &&
			characters[index] == '*' &&
			characters[index+1] == '*':
			expression.WriteString(".*")
			index += 2
		case characters[index] == '*':
			expression.WriteString("[^/]*")
			index++
		default:
			expression.WriteString(regexp.QuoteMeta(string(characters[index])))
			index++
		}
	}
	expression.WriteByte('$')
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}
	return compiled, nil
}

type revisionFileCollector struct {
	objectFormat   string
	requestInclude scopeMatcher
	requestExclude scopeMatcher
	policyInclude  scopeMatcher
	policyExclude  scopeMatcher
	maxFiles       int

	token    []byte
	files    []RevisionFileEntry
	coverage RevisionFileCoverage
	parseErr error
}

func newRevisionFileCollector(
	objectFormat string,
	requestInclude scopeMatcher,
	requestExclude scopeMatcher,
	policyInclude scopeMatcher,
	policyExclude scopeMatcher,
	maxFiles int,
) *revisionFileCollector {
	return &revisionFileCollector{
		objectFormat:   objectFormat,
		requestInclude: requestInclude,
		requestExclude: requestExclude,
		policyInclude:  policyInclude,
		policyExclude:  policyExclude,
		maxFiles:       maxFiles,
		files:          make([]RevisionFileEntry, 0, min(maxFiles, 256)),
	}
}

func (collector *revisionFileCollector) Write(data []byte) (int, error) {
	originalLength := len(data)
	if collector.parseErr != nil {
		return originalLength, nil
	}
	for len(data) > 0 {
		index := bytes.IndexByte(data, 0)
		if index < 0 {
			collector.appendToken(data)
			return originalLength, nil
		}
		collector.appendToken(data[:index])
		if collector.parseErr != nil {
			return originalLength, nil
		}
		collector.consumeToken(collector.token)
		collector.token = collector.token[:0]
		if collector.parseErr != nil {
			return originalLength, nil
		}
		data = data[index+1:]
	}
	return originalLength, nil
}

func (collector *revisionFileCollector) appendToken(data []byte) {
	if len(collector.token)+len(data) > maxGitPathBytes {
		collector.parseErr = fmt.Errorf("git ls-tree record exceeds %d bytes", maxGitPathBytes)
		collector.token = nil
		return
	}
	collector.token = append(collector.token, data...)
}

func (collector *revisionFileCollector) consumeToken(token []byte) {
	metadata, rawPath, found := bytes.Cut(token, []byte{'\t'})
	if !found || len(rawPath) == 0 {
		collector.parseErr = errors.New("git ls-tree returned an invalid record")
		return
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 3 {
		collector.parseErr = errors.New("git ls-tree returned invalid metadata")
		return
	}
	if _, err := strconv.ParseUint(fields[0], 8, 32); err != nil {
		collector.parseErr = errors.New("git ls-tree returned an invalid mode")
		return
	}
	if fields[1] != "blob" && fields[1] != "commit" {
		collector.parseErr = fmt.Errorf("git ls-tree returned unsupported object type %q", fields[1])
		return
	}
	if !exactOIDMatchesFormat(fields[2], collector.objectFormat) {
		collector.parseErr = errors.New("git ls-tree returned an invalid object ID")
		return
	}
	repositoryPath := string(rawPath)
	collector.coverage.ScannedFiles++
	if !collector.requestInclude.matches(repositoryPath) {
		return
	}
	if collector.requestExclude.matches(repositoryPath) {
		collector.coverage.ExcludedFiles++
		collector.coverage.RequestExcludedFiles++
		return
	}
	if !collector.policyInclude.matches(repositoryPath) ||
		collector.policyExclude.matches(repositoryPath) {
		collector.coverage.ExcludedFiles++
		collector.coverage.PolicyExcludedFiles++
		return
	}
	if !utf8.Valid(rawPath) || !isPortableRepositoryPath(repositoryPath) {
		collector.parseErr = fmt.Errorf(
			"git ls-tree returned an unsafe authorized repository path with sha256=%s",
			sha256Hex(rawPath),
		)
		return
	}
	collector.coverage.MatchedFiles++
	if len(collector.files) >= collector.maxFiles {
		collector.coverage.SkippedFiles++
		return
	}
	collector.files = append(collector.files, RevisionFileEntry{
		Mode:      fields[0],
		Type:      fields[1],
		ObjectOID: fields[2],
		Path:      repositoryPath,
		Language:  DetectLanguage(repositoryPath),
	})
	collector.coverage.RetainedFiles++
}

func (collector *revisionFileCollector) finish() error {
	if collector.parseErr != nil {
		return collector.parseErr
	}
	if len(collector.token) != 0 {
		return errors.New("git ls-tree output ended mid-record")
	}
	if collector.coverage.MatchedFiles !=
		collector.coverage.RetainedFiles+collector.coverage.SkippedFiles {
		return errors.New("git ls-tree coverage invariant is broken")
	}
	if collector.coverage.ExcludedFiles !=
		collector.coverage.RequestExcludedFiles+
			collector.coverage.PolicyExcludedFiles {
		return errors.New("git ls-tree exclusion coverage invariant is broken")
	}
	return nil
}

func cloneStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}
