// Package godeps captures deterministic Go package/import dependency facts
// from one exact Git commit. It never reads the mutable worktree or network.
package godeps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"argus.local/argus/internal/contextcapture"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/source/gitadapter"
)

const (
	SchemaVersion       = "argus.context.go_dependencies.v1alpha1"
	ProviderID          = "argus-go-dependencies"
	ProviderRevision    = "1"
	AdapterSHA256       = "6a94b881359a0e09d371822e83a812f732e0064844495b0948942881716a88ba"
	LegacyAdapterSHA256 = "83c20ff0bfdaf129bd8d958017ac7a71014fd14d4b60d63a76bce31ba576327d"

	DefaultMaxFiles         = 2_000
	DefaultMaxFileBytes     = int64(2 << 20)
	DefaultMaxArtifactBytes = int64(512 << 10)
	DefaultMaxPackages      = 2_048
	DefaultMaxImports       = 8_192
)

type Source interface {
	ListFilesAtCommitWithAdmission(
		context.Context, string, string, gitadapter.ScopeAdmission, int,
	) (gitadapter.RevisionFileList, error)
	ReadFileAtCommit(context.Context, string, string, string, int64) (gitadapter.FileContent, error)
}

type Request struct {
	RepositoryRoot   string
	CommitOID        string
	TargetPaths      []string
	PolicyInclude    []string
	PolicyExclude    []string
	MaxFiles         int
	MaxFileBytes     int64
	MaxArtifactBytes int64
}

type Artifact struct {
	SchemaVersion    string        `json:"schema_version"`
	ProviderID       string        `json:"provider_id"`
	ProviderRevision string        `json:"provider_revision"`
	CommitOID        string        `json:"commit_oid"`
	ModulePath       string        `json:"module_path"`
	TargetPaths      []string      `json:"target_paths"`
	Packages         []PackageFact `json:"packages"`
	Imports          []ImportFact  `json:"imports"`
	Coverage         Coverage      `json:"coverage"`
	Gaps             []Gap         `json:"gaps"`
}

type PackageFact struct {
	Directory string   `json:"directory"`
	Name      string   `json:"name"`
	Files     []string `json:"files"`
	Target    bool     `json:"target"`
}

type ImportFact struct {
	FromPackage       string `json:"from_package"`
	ImportPath        string `json:"import_path"`
	ResolvedDirectory string `json:"resolved_directory,omitempty"`
	Resolution        string `json:"resolution"`
	Path              string `json:"path"`
	Line              uint32 `json:"line"`
	Target            bool   `json:"target"`
}

type Coverage struct {
	GoFilesDiscovered    int  `json:"go_files_discovered"`
	GoFilesParsed        int  `json:"go_files_parsed"`
	GoFilesFailed        int  `json:"go_files_failed"`
	TargetFilesRequested int  `json:"target_files_requested"`
	TargetGoFilesFound   int  `json:"target_go_files_found"`
	PackagesEmitted      int  `json:"packages_emitted"`
	ImportsEmitted       int  `json:"imports_emitted"`
	Truncated            bool `json:"truncated"`
}

type Gap struct {
	Code string `json:"code"`
	Path string `json:"path,omitempty"`
	Line uint32 `json:"line,omitempty"`
}

type Result struct {
	Artifact Artifact
	Bytes    []byte
	Coverage reviewcore.ContextCoverage
}

type Provider struct{ source Source }

func New(source Source) (*Provider, error) {
	if source == nil {
		return nil, fmt.Errorf("Go dependency context source is required")
	}
	return &Provider{source: source}, nil
}

type parsedGoFile struct {
	path        string
	directory   string
	packageName string
	target      bool
	imports     []ImportFact
}

type packageAccumulator struct {
	name   string
	files  []string
	target bool
}

func (provider *Provider) Capture(ctx context.Context, request Request) (Result, error) {
	if provider == nil || provider.source == nil || ctx == nil {
		return Result{}, fmt.Errorf("Go dependency context dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	request = normalizeRequest(request)
	if request.RepositoryRoot == "" || request.CommitOID == "" {
		return Result{}, fmt.Errorf("repository root and exact commit OID are required")
	}
	targetPaths, err := normalizeTargetPaths(request.TargetPaths)
	if err != nil {
		return Result{}, err
	}
	listing, err := provider.source.ListFilesAtCommitWithAdmission(
		ctx, request.RepositoryRoot, request.CommitOID,
		gitadapter.ScopeAdmission{
			Include: []string{"**/*.go", "go.mod"}, Exclude: []string{},
			PolicyInclude: slices.Clone(request.PolicyInclude),
			PolicyExclude: slices.Clone(request.PolicyExclude),
		},
		request.MaxFiles,
	)
	if err != nil {
		return Result{}, fmt.Errorf("list exact Go dependency inputs: %w", err)
	}
	if err := contextcapture.RequireExactRevision(
		"Go dependency file listing", request.CommitOID, listing.CommitOID,
	); err != nil {
		return Result{}, err
	}
	artifact := Artifact{
		SchemaVersion: SchemaVersion, ProviderID: ProviderID,
		ProviderRevision: ProviderRevision, CommitOID: request.CommitOID,
		ModulePath: "", TargetPaths: targetPaths, Packages: []PackageFact{},
		Imports: []ImportFact{}, Gaps: []Gap{},
		Coverage: Coverage{TargetFilesRequested: len(targetPaths)},
	}
	if listing.Completeness != gitadapter.CompletenessComplete {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "dependency_file_listing_partial"})
	}
	targetSet := make(map[string]struct{}, len(targetPaths))
	for _, targetPath := range targetPaths {
		targetSet[targetPath] = struct{}{}
		if !strings.HasSuffix(targetPath, ".go") {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "target_language_unsupported", Path: targetPath})
		}
	}
	foundTargets := make(map[string]struct{})
	parsedFiles := make([]parsedGoFile, 0, len(listing.Files))
	for _, entry := range listing.Files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		content, readErr := provider.source.ReadFileAtCommit(
			ctx, request.RepositoryRoot, request.CommitOID, entry.Path, request.MaxFileBytes,
		)
		if readErr != nil {
			return Result{}, fmt.Errorf("read exact dependency input %q: %w", entry.Path, readErr)
		}
		if err := contextcapture.RequireExactRevision(
			"Go dependency file "+entry.Path, request.CommitOID, content.CommitOID,
		); err != nil {
			return Result{}, err
		}
		if entry.Path == "go.mod" {
			if content.Included {
				artifact.ModulePath = parseModulePath(content.Data)
			} else {
				artifact.Gaps = append(artifact.Gaps, Gap{Code: fileGapCode(content), Path: entry.Path})
			}
			continue
		}
		if !strings.HasSuffix(entry.Path, ".go") {
			continue
		}
		artifact.Coverage.GoFilesDiscovered++
		if !content.Included {
			artifact.Coverage.GoFilesFailed++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: fileGapCode(content), Path: entry.Path})
			continue
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, entry.Path, content.Data, parser.ImportsOnly)
		if parseErr != nil {
			artifact.Coverage.GoFilesFailed++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "parse_failed", Path: entry.Path})
			continue
		}
		_, targeted := targetSet[entry.Path]
		if targeted {
			foundTargets[entry.Path] = struct{}{}
		}
		directory := path.Dir(entry.Path)
		parsed := parsedGoFile{
			path: entry.Path, directory: directory, packageName: file.Name.Name,
			target: targeted, imports: []ImportFact{},
		}
		for _, spec := range file.Imports {
			importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil || importPath == "" {
				artifact.Gaps = append(artifact.Gaps, Gap{
					Code: "import_path_invalid", Path: entry.Path,
					Line: uint32(fset.Position(spec.Pos()).Line),
				})
				continue
			}
			parsed.imports = append(parsed.imports, ImportFact{
				FromPackage: directory, ImportPath: importPath,
				Path: entry.Path, Line: uint32(fset.Position(spec.Pos()).Line), Target: targeted,
			})
		}
		parsedFiles = append(parsedFiles, parsed)
		artifact.Coverage.GoFilesParsed++
	}
	artifact.Coverage.TargetGoFilesFound = len(foundTargets)
	for _, targetPath := range targetPaths {
		if strings.HasSuffix(targetPath, ".go") {
			if _, found := foundTargets[targetPath]; !found {
				artifact.Gaps = append(artifact.Gaps, Gap{Code: "target_file_unavailable", Path: targetPath})
			}
		}
	}
	if artifact.ModulePath == "" {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "module_path_unavailable", Path: "go.mod"})
	}
	buildDependencyFacts(parsedFiles, &artifact)
	data, err := marshalBounded(&artifact, request.MaxArtifactBytes)
	if err != nil {
		return Result{}, err
	}
	coverage := contextCoverage(artifact)
	if err := artifact.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate Go dependency context: %w", err)
	}
	return Result{Artifact: artifact, Bytes: data, Coverage: coverage}, nil
}

func buildDependencyFacts(files []parsedGoFile, artifact *Artifact) {
	packages := make(map[string]*packageAccumulator)
	for _, file := range files {
		current := packages[file.directory]
		if current == nil {
			current = &packageAccumulator{name: file.packageName, files: []string{}}
			packages[file.directory] = current
		} else if current.name != file.packageName {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "package_name_conflict", Path: file.path})
		}
		current.files = append(current.files, file.path)
		current.target = current.target || file.target
	}
	directories := make([]string, 0, len(packages))
	for directory := range packages {
		directories = append(directories, directory)
	}
	slices.Sort(directories)
	for _, directory := range directories {
		if len(artifact.Packages) >= DefaultMaxPackages {
			artifact.Coverage.Truncated = true
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "package_limit_exceeded"})
			break
		}
		current := packages[directory]
		slices.Sort(current.files)
		artifact.Packages = append(artifact.Packages, PackageFact{
			Directory: directory, Name: current.name,
			Files: current.files, Target: current.target,
		})
	}
	for _, file := range files {
		for _, dependency := range file.imports {
			dependency.Resolution, dependency.ResolvedDirectory = resolveImport(
				artifact.ModulePath, dependency.ImportPath, packages,
			)
			artifact.Imports = append(artifact.Imports, dependency)
		}
	}
	sort.Slice(artifact.Imports, func(left, right int) bool {
		return importKey(artifact.Imports[left]) < importKey(artifact.Imports[right])
	})
	if len(artifact.Imports) > DefaultMaxImports {
		artifact.Imports = artifact.Imports[:DefaultMaxImports]
		artifact.Coverage.Truncated = true
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "import_limit_exceeded"})
	}
	artifact.Coverage.PackagesEmitted = len(artifact.Packages)
	artifact.Coverage.ImportsEmitted = len(artifact.Imports)
	sortGaps(artifact.Gaps)
}

func resolveImport(
	modulePath string,
	importPath string,
	packages map[string]*packageAccumulator,
) (string, string) {
	if modulePath != "" && (importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")) {
		directory := strings.TrimPrefix(importPath, modulePath)
		directory = strings.TrimPrefix(directory, "/")
		if directory == "" {
			directory = "."
		}
		if _, exists := packages[directory]; exists {
			return "internal_exact", directory
		}
		return "internal_unresolved", directory
	}
	first := importPath
	if index := strings.IndexByte(first, '/'); index >= 0 {
		first = first[:index]
	}
	if !strings.Contains(first, ".") {
		return "stdlib", ""
	}
	return "external", ""
}

func marshalBounded(artifact *Artifact, maxBytes int64) ([]byte, error) {
	artifact.Coverage.PackagesEmitted = len(artifact.Packages)
	artifact.Coverage.ImportsEmitted = len(artifact.Imports)
	for {
		sortGaps(artifact.Gaps)
		data, err := json.Marshal(artifact)
		if err != nil {
			return nil, fmt.Errorf("marshal Go dependency context: %w", err)
		}
		if int64(len(data)) <= maxBytes {
			return data, nil
		}
		artifact.Coverage.Truncated = true
		if !hasGap(artifact.Gaps, "artifact_budget_exceeded") {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "artifact_budget_exceeded"})
		}
		switch {
		case len(artifact.Imports) > 0:
			artifact.Imports = artifact.Imports[:len(artifact.Imports)-1]
			artifact.Coverage.ImportsEmitted = len(artifact.Imports)
		case len(artifact.Packages) > 0:
			artifact.Packages = artifact.Packages[:len(artifact.Packages)-1]
			artifact.Coverage.PackagesEmitted = len(artifact.Packages)
		default:
			return nil, fmt.Errorf("Go dependency context identity exceeds artifact budget")
		}
	}
}

func contextCoverage(artifact Artifact) reviewcore.ContextCoverage {
	spans := make([]reviewcore.ContextSpan, 0)
	symbols := make([]string, 0)
	seenSpans := make(map[string]struct{})
	seenSymbols := make(map[string]struct{})
	for _, targetPath := range artifact.TargetPaths {
		if !strings.HasSuffix(targetPath, ".go") ||
			hasPathGap(artifact.Gaps, "target_file_unavailable", targetPath) {
			continue
		}
		key := targetPath + ":1"
		seenSpans[key] = struct{}{}
		spans = append(spans, reviewcore.ContextSpan{
			Path: targetPath, StartLine: 1, EndLine: 1,
		})
	}
	for _, dependency := range artifact.Imports {
		if dependency.Target {
			key := fmt.Sprintf("%s:%d", dependency.Path, dependency.Line)
			if _, exists := seenSpans[key]; !exists {
				seenSpans[key] = struct{}{}
				spans = append(spans, reviewcore.ContextSpan{
					Path: dependency.Path, StartLine: dependency.Line, EndLine: dependency.Line,
				})
			}
		}
		symbol := "import:" + dependency.ImportPath
		if _, exists := seenSymbols[symbol]; !exists {
			seenSymbols[symbol] = struct{}{}
			symbols = append(symbols, symbol)
		}
	}
	sort.Slice(spans, func(left, right int) bool {
		if spans[left].Path != spans[right].Path {
			return spans[left].Path < spans[right].Path
		}
		return spans[left].StartLine < spans[right].StartLine
	})
	slices.Sort(symbols)
	return reviewcore.ContextCoverage{Spans: spans, Symbols: symbols}
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

func normalizeTargetPaths(values []string) ([]string, error) {
	if values == nil {
		return nil, fmt.Errorf("target paths must be an explicit array")
	}
	result := slices.Clone(values)
	slices.Sort(result)
	for index, value := range result {
		if err := validateRepositoryPath(value); err != nil {
			return nil, fmt.Errorf("target_paths[%d]: %w", index, err)
		}
		if index > 0 && value == result[index-1] {
			return nil, fmt.Errorf("target paths contain duplicate %q", value)
		}
	}
	return result, nil
}

func parseModulePath(data []byte) string {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) == 2 && fields[0] == "module" && validText(fields[1], 512) {
			return fields[1]
		}
	}
	return ""
}

func fileGapCode(content gitadapter.FileContent) string {
	if len(content.Reasons) == 0 {
		return "file_unavailable"
	}
	return "file_" + string(content.Reasons[0].Code)
}

func importKey(value ImportFact) string {
	return strings.Join([]string{
		value.FromPackage, value.ImportPath, value.Path,
		fmt.Sprintf("%010d", value.Line), value.Resolution,
	}, "\x00")
}

func sortGaps(values []Gap) {
	sort.Slice(values, func(left, right int) bool {
		return gapKey(values[left]) < gapKey(values[right])
	})
}

func gapKey(value Gap) string {
	return fmt.Sprintf("%s\x00%s\x00%010d", value.Code, value.Path, value.Line)
}

func hasGap(values []Gap, code string) bool {
	return slices.ContainsFunc(values, func(value Gap) bool { return value.Code == code })
}

func hasPathGap(values []Gap, code string, targetPath string) bool {
	return slices.ContainsFunc(values, func(value Gap) bool {
		return value.Code == code && value.Path == targetPath
	})
}

func validText(value string, limit int) bool {
	if value == "" || len(value) > limit || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}
