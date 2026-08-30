// Package reposearch captures bounded repository-search evidence from one
// exact Git commit. It never reads the mutable worktree and rejects credential-
// sensitive paths before reading their blobs.
package reposearch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/abietic/argus/internal/contextcapture"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/source/gitadapter"
)

const (
	SchemaVersion       = "argus.context.repository_search.v1alpha1"
	ProviderID          = "argus-repository-search"
	ProviderRevision    = "2"
	AdapterSHA256       = "69190b151bb7b60129e820bd0d687b4d0b79ed00b83915d0795ac5b5f274a66d"
	LegacyAdapterSHA256 = "479e3e609fbdf4b1ed25392b15ea9bc39afb6deb9b5ee2b697f44715cbb72639"

	DefaultMaxFiles           = 2_000
	DefaultMaxFileBytes       = int64(2 << 20)
	DefaultMaxArtifactBytes   = int64(512 << 10)
	DefaultMaxCandidateTerms  = 512
	DefaultMaxQueries         = 32
	DefaultMaxMatchesPerTerm  = 16
	DefaultMaxLineTextBytes   = 512
	DefaultSelectionHaloLines = uint32(32)
)

var (
	identifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,63}`)
	templateMarker    = regexp.MustCompile(`(?:^|[._-])(?:example|sample|template)(?:$|[._-])`)
	secretFilePattern = regexp.MustCompile(`(?:^|[._-])secrets?\.(?:json|ya?ml|toml)$`)
	serviceAccount    = regexp.MustCompile(`(?:^|[._-])service[-_]account(?:[._-]|$)`)
	applicationCreds  = regexp.MustCompile(`(?:^|[._-])application_default_credentials(?:[._-]|$)`)
	sensitiveState    = regexp.MustCompile(`\.(?:tfstate(?:\.backup)?|tfplan)$`)
	sensitiveKey      = regexp.MustCompile(`\.(?:pem|key|p12|pfx|jks|keystore|kdbx|secret|secrets)$`)
)

var ignoredTerms = map[string]struct{}{
	"break": {}, "case": {}, "catch": {}, "class": {}, "const": {}, "continue": {},
	"default": {}, "defer": {}, "delete": {}, "else": {}, "enum": {}, "error": {},
	"export": {}, "extends": {}, "false": {}, "finally": {}, "for": {}, "from": {},
	"func": {}, "function": {}, "global": {}, "goto": {}, "if": {}, "implements": {},
	"import": {}, "interface": {}, "lambda": {}, "module": {}, "namespace": {},
	"package": {}, "private": {}, "protected": {}, "public": {}, "range": {},
	"return": {}, "select": {}, "static": {}, "struct": {}, "switch": {}, "throw": {},
	"true": {}, "try": {}, "type": {}, "typeof": {}, "undefined": {}, "var": {},
	"while": {}, "with": {}, "yield": {}, "todo": {}, "fixme": {}, "string": {},
}

type Source interface {
	ListFilesAtCommitWithAdmission(
		context.Context, string, string, gitadapter.ScopeAdmission, int,
	) (gitadapter.RevisionFileList, error)
	ReadFileAtCommit(context.Context, string, string, string, int64) (gitadapter.FileContent, error)
}

// concurrentReadSource is an optional capability. Sources that do not
// explicitly advertise concurrent safety retain the original serial access
// semantics. The Git adapter opts in because each exact-object read owns its
// subprocesses and immutable result buffers.
type concurrentReadSource interface {
	ConcurrentReadLimit() int
}

type Request struct {
	RepositoryRoot   string
	CommitOID        string
	TargetPaths      []string
	TargetRanges     []TargetRange
	PolicyInclude    []string
	PolicyExclude    []string
	MaxFiles         int
	MaxFileBytes     int64
	MaxArtifactBytes int64
}

type TargetRange struct {
	Path      string `json:"path"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
}

type Artifact struct {
	SchemaVersion    string        `json:"schema_version"`
	ProviderID       string        `json:"provider_id"`
	ProviderRevision string        `json:"provider_revision"`
	CommitOID        string        `json:"commit_oid"`
	TargetPaths      []string      `json:"target_paths"`
	TargetRanges     []TargetRange `json:"target_ranges"`
	QueryHaloLines   uint32        `json:"query_halo_lines"`
	Queries          []Query       `json:"queries"`
	Matches          []Match       `json:"matches"`
	Coverage         Coverage      `json:"coverage"`
	Gaps             []Gap         `json:"gaps"`
}

type Query struct {
	Term                  string   `json:"term"`
	SourcePaths           []string `json:"source_paths"`
	TargetOccurrences     int      `json:"target_occurrences"`
	RepositoryOccurrences int      `json:"repository_occurrences"`
	MatchedFiles          int      `json:"matched_files"`
}

type Match struct {
	Term              string `json:"term"`
	Path              string `json:"path"`
	Line              uint32 `json:"line"`
	Column            uint32 `json:"column"`
	LineSHA256        string `json:"line_sha256"`
	LineText          string `json:"line_text"`
	LineTextStart     uint32 `json:"line_text_start_column"`
	LineTextTruncated bool   `json:"line_text_truncated"`
}

type Coverage struct {
	FilesMatched          int  `json:"files_matched"`
	FilesRetained         int  `json:"files_retained"`
	FilesRead             int  `json:"files_read"`
	FilesUnavailable      int  `json:"files_unavailable"`
	FilesSensitiveSkipped int  `json:"files_sensitive_skipped"`
	TargetFilesRequested  int  `json:"target_files_requested"`
	TargetFilesFound      int  `json:"target_files_found"`
	CandidateTerms        int  `json:"candidate_terms"`
	QueriesEmitted        int  `json:"queries_emitted"`
	RepositoryOccurrences int  `json:"repository_occurrences"`
	MatchesEmitted        int  `json:"matches_emitted"`
	Truncated             bool `json:"truncated"`
}

type Gap struct {
	Code string `json:"code"`
	Path string `json:"path,omitempty"`
	Term string `json:"term,omitempty"`
}

type Result struct {
	Artifact Artifact
	Bytes    []byte
	Coverage reviewcore.ContextCoverage
}

type Provider struct{ source Source }

func New(source Source) (*Provider, error) {
	if source == nil {
		return nil, fmt.Errorf("repository search source is required")
	}
	return &Provider{source: source}, nil
}

type textFile struct {
	path   string
	data   []byte
	target bool
}

type repositoryFileRead struct {
	content gitadapter.FileContent
	err     error
}

type queryStats struct {
	term              string
	sourcePaths       map[string]struct{}
	targetOccurrences int
	repositoryMatches int
	matchedFiles      map[string]struct{}
	matches           []Match
}

func (provider *Provider) Capture(ctx context.Context, request Request) (Result, error) {
	if provider == nil || provider.source == nil || ctx == nil {
		return Result{}, fmt.Errorf("repository search dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	request = normalizeRequest(request)
	if request.RepositoryRoot == "" || request.CommitOID == "" {
		return Result{}, fmt.Errorf("repository root and exact commit OID are required")
	}
	targetPaths, err := normalizePaths(request.TargetPaths)
	if err != nil {
		return Result{}, err
	}
	targetRanges, err := normalizeTargetRanges(request.TargetRanges, targetPaths)
	if err != nil {
		return Result{}, err
	}
	listing, err := provider.source.ListFilesAtCommitWithAdmission(
		ctx, request.RepositoryRoot, request.CommitOID,
		gitadapter.ScopeAdmission{
			Include: []string{"**"}, Exclude: []string{},
			PolicyInclude: slices.Clone(request.PolicyInclude),
			PolicyExclude: slices.Clone(request.PolicyExclude),
		}, request.MaxFiles,
	)
	if err != nil {
		return Result{}, fmt.Errorf("list exact repository search inputs: %w", err)
	}
	if err := contextcapture.RequireExactRevision(
		"repository search file listing", request.CommitOID, listing.CommitOID,
	); err != nil {
		return Result{}, err
	}
	artifact := Artifact{
		SchemaVersion: SchemaVersion, ProviderID: ProviderID,
		ProviderRevision: ProviderRevision, CommitOID: request.CommitOID,
		TargetPaths: targetPaths, TargetRanges: targetRanges,
		QueryHaloLines: 0, Queries: []Query{}, Matches: []Match{}, Gaps: []Gap{},
		Coverage: Coverage{
			FilesMatched:  listing.Coverage.MatchedFiles,
			FilesRetained: len(listing.Files), TargetFilesRequested: len(targetPaths),
		},
	}
	if len(targetRanges) > 0 {
		artifact.QueryHaloLines = DefaultSelectionHaloLines
	}
	if listing.Completeness != gitadapter.CompletenessComplete {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "source_listing_partial"})
		artifact.Coverage.Truncated = true
	}
	targetSet := make(map[string]struct{}, len(targetPaths))
	for _, targetPath := range targetPaths {
		targetSet[targetPath] = struct{}{}
	}
	entries := slices.Clone(listing.Files)
	sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
	files := make([]textFile, 0, len(entries))
	foundTargets := make(map[string]struct{}, len(targetPaths))
	readEntries := make([]gitadapter.RevisionFileEntry, 0, len(entries))
	for _, entry := range entries {
		if reason := sensitivePathReason(entry.Path); reason != "" {
			artifact.Coverage.FilesSensitiveSkipped++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: reason, Path: entry.Path})
			continue
		}
		readEntries = append(readEntries, entry)
	}
	reads := provider.readFiles(ctx, request, readEntries)
	for index, entry := range readEntries {
		content, readErr := reads[index].content, reads[index].err
		if readErr != nil {
			return Result{}, fmt.Errorf("read exact repository search input %q: %w", entry.Path, readErr)
		}
		if err := contextcapture.RequireExactRevision(
			"repository search file "+entry.Path, request.CommitOID, content.CommitOID,
		); err != nil {
			return Result{}, err
		}
		if !content.Included {
			artifact.Coverage.FilesUnavailable++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: fileGapCode(content), Path: entry.Path})
			continue
		}
		artifact.Coverage.FilesRead++
		_, targeted := targetSet[entry.Path]
		files = append(files, textFile{path: entry.Path, data: bytes.Clone(content.Data), target: targeted})
		if targeted {
			foundTargets[entry.Path] = struct{}{}
		}
	}
	artifact.Coverage.TargetFilesFound = len(foundTargets)
	for _, targetPath := range targetPaths {
		if _, found := foundTargets[targetPath]; !found {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "target_file_unavailable", Path: targetPath})
		}
	}

	stats := make(map[string]*queryStats)
	candidateTruncated := false
	rangesByPath := targetRangesByPath(targetRanges)
	for _, file := range files {
		if !file.target {
			continue
		}
		queryData := file.data
		if ranges := rangesByPath[file.path]; len(ranges) > 0 {
			queryData, err = boundedTargetWindow(file.data, ranges, DefaultSelectionHaloLines)
			if err != nil {
				return Result{}, fmt.Errorf("target range %q: %w", file.path, err)
			}
		}
		for _, match := range identifierPattern.FindAll(queryData, -1) {
			term := string(match)
			if !usefulTerm(term) {
				continue
			}
			stat := stats[term]
			if stat == nil {
				if len(stats) == DefaultMaxCandidateTerms {
					candidateTruncated = true
					continue
				}
				stat = &queryStats{term: term, sourcePaths: map[string]struct{}{}, matchedFiles: map[string]struct{}{}, matches: []Match{}}
				stats[term] = stat
			}
			stat.targetOccurrences++
			stat.sourcePaths[file.path] = struct{}{}
		}
	}
	artifact.Coverage.CandidateTerms = len(stats)
	if candidateTruncated {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "query_candidate_budget_exceeded"})
		artifact.Coverage.Truncated = true
	}
	if len(stats) == 0 {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "target_no_search_terms"})
	}

	for _, file := range files {
		if file.target {
			continue
		}
		lines := bytes.Split(file.data, []byte{'\n'})
		for lineIndex, line := range lines {
			for _, location := range identifierPattern.FindAllIndex(line, -1) {
				term := string(line[location[0]:location[1]])
				stat := stats[term]
				if stat == nil {
					continue
				}
				stat.repositoryMatches++
				stat.matchedFiles[file.path] = struct{}{}
				if len(stat.matches) == DefaultMaxMatchesPerTerm {
					continue
				}
				digest := sha256.Sum256(line)
				lineText, lineTextStart, truncated := boundedLineText(line, location[0], location[1])
				stat.matches = append(stat.matches, Match{
					Term: term, Path: file.path, Line: uint32(lineIndex + 1), Column: uint32(location[0] + 1),
					LineSHA256: hex.EncodeToString(digest[:]), LineText: lineText,
					LineTextStart: lineTextStart, LineTextTruncated: truncated,
				})
			}
		}
	}
	selected := make([]*queryStats, 0, len(stats))
	for _, stat := range stats {
		if stat.repositoryMatches > 0 {
			selected = append(selected, stat)
		}
	}
	sort.Slice(selected, func(left, right int) bool { return rankedQueryLess(selected[left], selected[right]) })
	if len(selected) > DefaultMaxQueries {
		selected = selected[:DefaultMaxQueries]
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "query_budget_exceeded"})
		artifact.Coverage.Truncated = true
	}
	if len(stats) != 0 && len(selected) == 0 {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "no_repository_matches"})
	}
	for _, stat := range selected {
		sourcePaths := mapKeys(stat.sourcePaths)
		artifact.Queries = append(artifact.Queries, Query{
			Term: stat.term, SourcePaths: sourcePaths, TargetOccurrences: stat.targetOccurrences,
			RepositoryOccurrences: stat.repositoryMatches, MatchedFiles: len(stat.matchedFiles),
		})
		artifact.Matches = append(artifact.Matches, stat.matches...)
		if stat.repositoryMatches > len(stat.matches) {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "match_budget_exceeded", Term: stat.term})
			artifact.Coverage.Truncated = true
		}
	}
	canonicalize(&artifact)
	data, err := marshalBounded(&artifact, request.MaxArtifactBytes)
	if err != nil {
		return Result{}, err
	}
	if err := artifact.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate repository search context: %w", err)
	}
	return Result{
		Artifact: artifact, Bytes: data,
		Coverage: contextCoverage(artifact, files, foundTargets),
	}, nil
}

func (provider *Provider) readFiles(
	ctx context.Context,
	request Request,
	entries []gitadapter.RevisionFileEntry,
) []repositoryFileRead {
	results := make([]repositoryFileRead, len(entries))
	if len(entries) == 0 {
		return results
	}
	limit := 1
	if source, ok := provider.source.(concurrentReadSource); ok {
		limit = source.ConcurrentReadLimit()
	}
	if limit < 1 {
		limit = 1
	}
	limit = min(limit, 16, len(entries))
	if limit == 1 {
		for index, entry := range entries {
			results[index].content, results[index].err = provider.source.ReadFileAtCommit(
				ctx, request.RepositoryRoot, request.CommitOID, entry.Path, request.MaxFileBytes,
			)
		}
		return results
	}

	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(limit)
	for range limit {
		go func() {
			defer workers.Done()
			for index := range jobs {
				entry := entries[index]
				results[index].content, results[index].err = provider.source.ReadFileAtCommit(
					ctx, request.RepositoryRoot, request.CommitOID, entry.Path, request.MaxFileBytes,
				)
			}
		}()
	}
	for index := range entries {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}

func normalizeRequest(request Request) Request {
	if request.MaxFiles <= 0 {
		request.MaxFiles = DefaultMaxFiles
	}
	if request.MaxFileBytes <= 0 {
		request.MaxFileBytes = DefaultMaxFileBytes
	}
	if request.MaxArtifactBytes <= 0 {
		request.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if request.PolicyInclude == nil {
		request.PolicyInclude = []string{"**"}
	}
	if request.PolicyExclude == nil {
		request.PolicyExclude = []string{}
	}
	return request
}

func normalizePaths(values []string) ([]string, error) {
	if values == nil {
		return nil, fmt.Errorf("target paths must be an explicit array")
	}
	result := slices.Clone(values)
	sort.Strings(result)
	for index, value := range result {
		if !validRepositoryPath(value) {
			return nil, fmt.Errorf("target_paths[%d] is invalid", index)
		}
		if index > 0 && value == result[index-1] {
			return nil, fmt.Errorf("target paths contain duplicate %q", value)
		}
	}
	return result, nil
}

func normalizeTargetRanges(values []TargetRange, targetPaths []string) ([]TargetRange, error) {
	if values == nil {
		return []TargetRange{}, nil
	}
	result := slices.Clone(values)
	sort.Slice(result, func(left, right int) bool { return targetRangeLess(result[left], result[right]) })
	targets := make(map[string]struct{}, len(targetPaths))
	for _, targetPath := range targetPaths {
		targets[targetPath] = struct{}{}
	}
	for index, lineRange := range result {
		if _, exists := targets[lineRange.Path]; !exists || lineRange.StartLine == 0 || lineRange.EndLine < lineRange.StartLine {
			return nil, fmt.Errorf("target_ranges[%d] must bind a valid target path and line range", index)
		}
		if index > 0 && lineRange.Path == result[index-1].Path && lineRange.StartLine <= result[index-1].EndLine {
			return nil, fmt.Errorf("target ranges must be non-overlapping and uniquely sorted")
		}
	}
	return result, nil
}

func targetRangeLess(left, right TargetRange) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.StartLine != right.StartLine {
		return left.StartLine < right.StartLine
	}
	return left.EndLine < right.EndLine
}

func targetRangesByPath(values []TargetRange) map[string][]TargetRange {
	result := make(map[string][]TargetRange)
	for _, lineRange := range values {
		result[lineRange.Path] = append(result[lineRange.Path], lineRange)
	}
	return result
}

func boundedTargetWindow(data []byte, ranges []TargetRange, halo uint32) ([]byte, error) {
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	lineCount := uint32(len(lines))
	type window struct{ start, end uint32 }
	windows := make([]window, 0, len(ranges))
	for _, lineRange := range ranges {
		if lineRange.EndLine > lineCount {
			return nil, fmt.Errorf("line range %d-%d exceeds %d lines", lineRange.StartLine, lineRange.EndLine, lineCount)
		}
		start := uint32(1)
		if lineRange.StartLine > halo {
			start = lineRange.StartLine - halo
		}
		end := min(lineCount, lineRange.EndLine+halo)
		if len(windows) > 0 && start <= windows[len(windows)-1].end+1 {
			windows[len(windows)-1].end = max(windows[len(windows)-1].end, end)
			continue
		}
		windows = append(windows, window{start: start, end: end})
	}
	var result bytes.Buffer
	for _, current := range windows {
		for index := current.start; index <= current.end; index++ {
			result.Write(lines[index-1])
			result.WriteByte('\n')
		}
	}
	return result.Bytes(), nil
}

func usefulTerm(term string) bool {
	if len(term) < 4 || len(term) > 64 {
		return false
	}
	if _, ignored := ignoredTerms[strings.ToLower(term)]; ignored {
		return false
	}
	// Generic lexical search cannot distinguish declarations from prose or
	// local variables. Keep only structurally distinctive identifiers; lower-
	// case language-specific symbols belong in an AST/LSP provider.
	return strings.Contains(term, "_") || strings.IndexFunc(term, func(character rune) bool {
		return character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
	}) >= 0
}

func rankedQueryLess(left, right *queryStats) bool {
	if len(left.matchedFiles) != len(right.matchedFiles) {
		return len(left.matchedFiles) < len(right.matchedFiles)
	}
	if left.repositoryMatches != right.repositoryMatches {
		return left.repositoryMatches < right.repositoryMatches
	}
	if len(left.term) != len(right.term) {
		return len(left.term) > len(right.term)
	}
	return left.term < right.term
}

func boundedLineText(line []byte, matchStart, matchEnd int) (string, uint32, bool) {
	if len(line) <= DefaultMaxLineTextBytes {
		return string(line), 1, false
	}
	start := max(0, matchStart-DefaultMaxLineTextBytes/4)
	end := min(len(line), start+DefaultMaxLineTextBytes)
	if end == len(line) {
		start = max(0, end-DefaultMaxLineTextBytes)
	}
	for start < matchStart && !utf8.RuneStart(line[start]) {
		start++
	}
	for end > matchEnd && !utf8.Valid(line[start:end]) {
		end--
	}
	return string(line[start:end]), uint32(start + 1), true
}

func canonicalize(artifact *Artifact) {
	sort.Slice(artifact.Queries, func(left, right int) bool { return artifact.Queries[left].Term < artifact.Queries[right].Term })
	for index := range artifact.Queries {
		sort.Strings(artifact.Queries[index].SourcePaths)
		artifact.Queries[index].SourcePaths = slices.Compact(artifact.Queries[index].SourcePaths)
	}
	sort.Slice(artifact.Matches, func(left, right int) bool {
		return matchKey(artifact.Matches[left]) < matchKey(artifact.Matches[right])
	})
	artifact.Matches = slices.Compact(artifact.Matches)
	sort.Slice(artifact.Gaps, func(left, right int) bool { return gapKey(artifact.Gaps[left]) < gapKey(artifact.Gaps[right]) })
	artifact.Gaps = slices.Compact(artifact.Gaps)
	artifact.Coverage.QueriesEmitted = len(artifact.Queries)
	artifact.Coverage.RepositoryOccurrences = 0
	for _, query := range artifact.Queries {
		artifact.Coverage.RepositoryOccurrences += query.RepositoryOccurrences
	}
	artifact.Coverage.MatchesEmitted = len(artifact.Matches)
}

func marshalBounded(artifact *Artifact, limit int64) ([]byte, error) {
	for {
		canonicalize(artifact)
		data, err := json.Marshal(artifact)
		if err != nil {
			return nil, fmt.Errorf("marshal repository search context: %w", err)
		}
		if int64(len(data)) <= limit {
			return data, nil
		}
		artifact.Coverage.Truncated = true
		if !hasGap(artifact.Gaps, "artifact_budget_exceeded") {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "artifact_budget_exceeded"})
		}
		if len(artifact.Matches) > 0 {
			artifact.Matches = artifact.Matches[:len(artifact.Matches)-1]
			continue
		}
		if len(artifact.Queries) > 0 {
			artifact.Queries = artifact.Queries[:len(artifact.Queries)-1]
			continue
		}
		return nil, fmt.Errorf("repository search identity exceeds max artifact bytes %d", limit)
	}
}

func contextCoverage(artifact Artifact, files []textFile, found map[string]struct{}) reviewcore.ContextCoverage {
	fileByPath := make(map[string][]byte, len(files))
	for _, file := range files {
		fileByPath[file.path] = file.data
	}
	spans := []reviewcore.ContextSpan{}
	if len(artifact.TargetRanges) > 0 {
		for _, lineRange := range artifact.TargetRanges {
			if _, exists := found[lineRange.Path]; exists {
				spans = append(spans, reviewcore.ContextSpan{
					Path: lineRange.Path, StartLine: lineRange.StartLine, EndLine: lineRange.EndLine,
				})
			}
		}
	}
	for _, target := range artifact.TargetPaths {
		if len(artifact.TargetRanges) > 0 {
			continue
		}
		if _, exists := found[target]; !exists {
			continue
		}
		data := fileByPath[target]
		lines := uint32(bytes.Count(data, []byte{'\n'}))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
		}
		if lines > 0 {
			spans = append(spans, reviewcore.ContextSpan{Path: target, StartLine: 1, EndLine: lines})
		}
	}
	symbols := make([]string, 0, len(artifact.Queries))
	for _, query := range artifact.Queries {
		symbols = append(symbols, "repository_search:"+query.Term)
	}
	if len(spans) == 0 && len(symbols) == 0 {
		symbols = []string{"repository_search:coverage"}
	}
	sort.Slice(spans, func(left, right int) bool { return spans[left].Path < spans[right].Path })
	sort.Strings(symbols)
	return reviewcore.ContextCoverage{Spans: spans, Symbols: symbols}
}

func sensitivePathReason(value string) string {
	normalized := strings.ToLower(strings.ReplaceAll(value, "\\", "/"))
	segments := strings.FieldsFunc(normalized, func(character rune) bool { return character == '/' })
	base := normalized
	if len(segments) > 0 {
		base = segments[len(segments)-1]
	}
	if templateMarker.MatchString(base) {
		return ""
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == ".envrc" ||
		strings.HasPrefix(base, ".envrc.") || base == ".dev.vars" || strings.HasPrefix(base, ".dev.vars.") ||
		slices.Contains(segments, ".direnv") {
		return "sensitive_environment_file"
	}
	credentialNames := []string{
		".netrc", ".npmrc", ".pypirc", ".git-credentials", ".vault-token", "credentials.json",
		"auth.json", "id_rsa", "id_ed25519", ".terraformrc", "terraform.rc", "credentials.tfrc.json",
	}
	if slices.Contains(credentialNames, base) || secretFilePattern.MatchString(base) ||
		serviceAccount.MatchString(base) || applicationCreds.MatchString(base) ||
		slices.Contains(segments, ".aws") && base == "credentials" ||
		slices.Contains(segments, ".docker") && base == "config.json" ||
		slices.Contains(segments, ".kube") && base == "config" {
		return "sensitive_credential_file"
	}
	if sensitiveState.MatchString(base) {
		return "sensitive_state_file"
	}
	if sensitiveKey.MatchString(base) {
		return "sensitive_key_file"
	}
	return ""
}

func fileGapCode(content gitadapter.FileContent) string {
	if len(content.Reasons) == 0 {
		return "file_unavailable"
	}
	return "file_" + string(content.Reasons[0].Code)
}

func mapKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func matchKey(value Match) string {
	return fmt.Sprintf("%s\x00%s\x00%010d\x00%010d", value.Term, value.Path, value.Line, value.Column)
}

func gapKey(value Gap) string { return value.Code + "\x00" + value.Path + "\x00" + value.Term }

func hasGap(gaps []Gap, code string) bool {
	return slices.ContainsFunc(gaps, func(gap Gap) bool { return gap.Code == code })
}

func validRepositoryPath(value string) bool {
	return value != "" && utf8.ValidString(value) && !path.IsAbs(value) &&
		path.Clean(value) == value && value != "." && value != ".." &&
		!strings.HasPrefix(value, "../") && !strings.Contains(value, "\\") && validText(value)
}

func validText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == 0 || character == '\r' || character == '\n' || character < 0x20 && character != '\t' {
			return false
		}
	}
	return true
}
