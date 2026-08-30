// Package gocompile captures exact-revision Go compile evidence without
// executing repository code. Source bytes come only from an immutable Git
// object and compilation is delegated to an injected, versioned runner.
package gocompile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/abietic/argus/internal/contextcapture"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/source/gitadapter"
)

const (
	SchemaVersion       = "argus.context.go_compile.v1alpha1"
	ProviderID          = "argus-go-compile"
	ProviderRevision    = "1"
	AdapterSHA256       = "6edc39a47926bbe71f8b83e2bd6c796d812ae4f935b65bde30bae2f0d59e0e02"
	LegacyAdapterSHA256 = "fe2f13d7ccda18fdf53c1242b239e0f0fe24d8051b351502f3e30894702ea935"

	DefaultMaxFiles         = 2_000
	DefaultMaxFileBytes     = int64(2 << 20)
	DefaultMaxArtifactBytes = int64(512 << 10)
	DefaultMaxCommandOutput = int64(2 << 20)
	DefaultMaxDiagnostics   = 512
)

var diagnosticPattern = regexp.MustCompile(`^(?:\./)?([^:\n]+\.go):(\d+)(?::(\d+))?:\s*(.+)$`)

type Source interface {
	ListFilesAtCommitWithAdmission(
		context.Context, string, string, gitadapter.ScopeAdmission, int,
	) (gitadapter.RevisionFileList, error)
	ReadFileAtCommit(context.Context, string, string, string, int64) (gitadapter.FileContent, error)
}

type Runner interface {
	Identity() Toolchain
	Compile(context.Context, CompileRequest) (CompileResult, error)
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

type CompileRequest struct {
	Workspace      string
	Package        string
	OutputPath     string
	VendorMode     bool
	MaxOutputBytes int64
}

type CompileResult struct {
	ExitCode int
	Output   []byte
}

type Toolchain struct {
	GoVersion             string `json:"go_version"`
	GOOS                  string `json:"goos"`
	GOARCH                string `json:"goarch"`
	CGOEnabled            bool   `json:"cgo_enabled"`
	RepositoryCodeExecute bool   `json:"repository_code_executed"`
	ModuleNetwork         string `json:"module_network"`
	Authority             string `json:"authority"`
}

type Artifact struct {
	SchemaVersion    string          `json:"schema_version"`
	ProviderID       string          `json:"provider_id"`
	ProviderRevision string          `json:"provider_revision"`
	CommitOID        string          `json:"commit_oid"`
	TargetPaths      []string        `json:"target_paths"`
	Toolchain        Toolchain       `json:"toolchain"`
	Packages         []PackageResult `json:"packages"`
	Coverage         Coverage        `json:"coverage"`
	Gaps             []Gap           `json:"gaps"`
}

type PackageResult struct {
	Package         string       `json:"package"`
	Status          string       `json:"status"`
	Diagnostics     []Diagnostic `json:"diagnostics"`
	OutputSHA256    string       `json:"output_sha256"`
	OutputSizeBytes int64        `json:"output_size_bytes"`
	CommandExecuted bool         `json:"command_executed"`
	ExitCode        int          `json:"exit_code"`
	TestSourcesUsed bool         `json:"test_sources_included"`
}

type Diagnostic struct {
	Path    string `json:"path"`
	Line    uint32 `json:"line"`
	Column  uint32 `json:"column"`
	Message string `json:"message"`
}

type Coverage struct {
	FilesDiscovered      int  `json:"files_discovered"`
	FilesMaterialized    int  `json:"files_materialized"`
	FilesUnavailable     int  `json:"files_unavailable"`
	TargetFilesRequested int  `json:"target_files_requested"`
	TargetGoFilesFound   int  `json:"target_go_files_found"`
	PackagesRequested    int  `json:"packages_requested"`
	PackagesPassed       int  `json:"packages_passed"`
	PackagesFailed       int  `json:"packages_failed"`
	PackagesUnavailable  int  `json:"packages_unavailable"`
	DiagnosticsEmitted   int  `json:"diagnostics_emitted"`
	Truncated            bool `json:"truncated"`
}

type Gap struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Package string `json:"package,omitempty"`
}

type Result struct {
	Artifact Artifact
	Bytes    []byte
	Coverage reviewcore.ContextCoverage
}

type Provider struct {
	source Source
	runner Runner
}

func New(source Source, runner Runner) (*Provider, error) {
	if source == nil || runner == nil {
		return nil, fmt.Errorf("Go compile context source and runner are required")
	}
	identity := runner.Identity()
	if err := identity.Validate(); err != nil {
		return nil, fmt.Errorf("Go compile runner identity: %w", err)
	}
	return &Provider{source: source, runner: runner}, nil
}

func (provider *Provider) Capture(ctx context.Context, request Request) (Result, error) {
	if provider == nil || provider.source == nil || provider.runner == nil || ctx == nil {
		return Result{}, fmt.Errorf("Go compile context dependencies are required")
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
	listing, err := provider.source.ListFilesAtCommitWithAdmission(
		ctx, request.RepositoryRoot, request.CommitOID,
		gitadapter.ScopeAdmission{
			Include: []string{"**/*.go", "**/*.s", "**/*.syso", "go.mod", "go.sum", "vendor/modules.txt"},
			Exclude: []string{}, PolicyInclude: slices.Clone(request.PolicyInclude),
			PolicyExclude: slices.Clone(request.PolicyExclude),
		}, request.MaxFiles,
	)
	if err != nil {
		return Result{}, fmt.Errorf("list exact Go compile inputs: %w", err)
	}
	if err := contextcapture.RequireExactRevision(
		"Go compile file listing", request.CommitOID, listing.CommitOID,
	); err != nil {
		return Result{}, err
	}
	artifact := Artifact{
		SchemaVersion: SchemaVersion, ProviderID: ProviderID,
		ProviderRevision: ProviderRevision, CommitOID: request.CommitOID,
		TargetPaths: targetPaths, Toolchain: provider.runner.Identity(),
		Packages: []PackageResult{}, Gaps: []Gap{},
		Coverage: Coverage{
			FilesDiscovered: len(listing.Files), TargetFilesRequested: len(targetPaths),
		},
	}
	if listing.Completeness != gitadapter.CompletenessComplete {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "source_listing_partial"})
	}

	workspace, err := os.MkdirTemp("", "argus-go-compile-")
	if err != nil {
		return Result{}, fmt.Errorf("create Go compile workspace: %w", err)
	}
	defer os.RemoveAll(workspace)
	if err := os.Chmod(workspace, 0o700); err != nil {
		return Result{}, fmt.Errorf("protect Go compile workspace: %w", err)
	}

	foundTargets := make(map[string]struct{})
	materialized := make(map[string][]byte, len(listing.Files))
	moduleFound, vendorFound := false, false
	unsupportedInputs := false
	for _, entry := range listing.Files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		content, readErr := provider.source.ReadFileAtCommit(
			ctx, request.RepositoryRoot, request.CommitOID, entry.Path, request.MaxFileBytes,
		)
		if readErr != nil {
			return Result{}, fmt.Errorf("read exact Go compile input %q: %w", entry.Path, readErr)
		}
		if err := contextcapture.RequireExactRevision(
			"Go compile file "+entry.Path, request.CommitOID, content.CommitOID,
		); err != nil {
			return Result{}, err
		}
		if !content.Included {
			artifact.Coverage.FilesUnavailable++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: fileGapCode(content), Path: entry.Path})
			continue
		}
		if strings.HasSuffix(entry.Path, ".syso") {
			artifact.Coverage.FilesUnavailable++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "precompiled_object_not_supported", Path: entry.Path})
			continue
		}
		if err := writeMaterializedFile(workspace, entry.Path, content.Data); err != nil {
			return Result{}, err
		}
		materialized[entry.Path] = bytes.Clone(content.Data)
		artifact.Coverage.FilesMaterialized++
		moduleFound = moduleFound || entry.Path == "go.mod"
		vendorFound = vendorFound || entry.Path == "vendor/modules.txt"
		if strings.HasSuffix(entry.Path, ".go") {
			for _, gap := range unsupportedSourceGaps(entry.Path, content.Data) {
				artifact.Gaps = append(artifact.Gaps, gap)
				unsupportedInputs = true
			}
		}
	}
	if module, exists := materialized["go.mod"]; exists && hasUnsafeLocalReplace(module) {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "local_replace_not_supported", Path: "go.mod"})
		unsupportedInputs = true
	}
	for _, targetPath := range targetPaths {
		if !strings.HasSuffix(targetPath, ".go") {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "target_language_unsupported", Path: targetPath})
			continue
		}
		if _, exists := materialized[targetPath]; exists {
			foundTargets[targetPath] = struct{}{}
		} else {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "target_file_unavailable", Path: targetPath})
		}
	}
	artifact.Coverage.TargetGoFilesFound = len(foundTargets)
	packages := targetPackages(targetPaths, foundTargets)
	artifact.Coverage.PackagesRequested = len(packages)

	sourceComplete := listing.Completeness == gitadapter.CompletenessComplete &&
		artifact.Coverage.FilesUnavailable == 0 && !unsupportedInputs
	if !moduleFound {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "module_file_unavailable", Path: "go.mod"})
	}
	buildDir := filepath.Join(workspace, ".argus-build")
	if err := os.Mkdir(buildDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create Go compile output directory: %w", err)
	}
	for index, packagePath := range packages {
		if !sourceComplete || !moduleFound {
			artifact.Packages = append(artifact.Packages, PackageResult{
				Package: packagePath, Status: "unavailable", Diagnostics: []Diagnostic{},
				OutputSHA256: emptySHA256(), OutputSizeBytes: 0, CommandExecuted: false,
				ExitCode: -1, TestSourcesUsed: false,
			})
			artifact.Coverage.PackagesUnavailable++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "compile_input_incomplete", Package: packagePath})
			continue
		}
		compiled, compileErr := provider.runner.Compile(ctx, CompileRequest{
			Workspace: workspace, Package: packagePath,
			OutputPath: filepath.Join(buildDir, fmt.Sprintf("package-%04d.test", index)),
			VendorMode: vendorFound, MaxOutputBytes: DefaultMaxCommandOutput,
		})
		if compileErr != nil {
			return Result{}, fmt.Errorf("compile exact package %q: %w", packagePath, compileErr)
		}
		packageResult := packageResult(workspace, packagePath, compiled)
		artifact.Packages = append(artifact.Packages, packageResult)
		switch packageResult.Status {
		case "passed":
			artifact.Coverage.PackagesPassed++
		case "failed":
			artifact.Coverage.PackagesFailed++
			if len(packageResult.Diagnostics) == 0 {
				artifact.Gaps = append(artifact.Gaps, Gap{Code: "compile_failure_unlocalized", Package: packagePath})
			}
		case "unavailable":
			artifact.Coverage.PackagesUnavailable++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "dependency_unavailable", Package: packagePath})
		}
	}
	canonicalize(&artifact)
	data, err := marshalBounded(&artifact, request.MaxArtifactBytes)
	if err != nil {
		return Result{}, err
	}
	if err := artifact.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate Go compile context: %w", err)
	}
	return Result{
		Artifact: artifact, Bytes: data,
		Coverage: contextCoverage(artifact, materialized, foundTargets),
	}, nil
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

func writeMaterializedFile(root, repositoryPath string, data []byte) error {
	if !validRepositoryPath(repositoryPath) {
		return fmt.Errorf("materialized path %q is invalid", repositoryPath)
	}
	destination := filepath.Join(root, filepath.FromSlash(repositoryPath))
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("materialized path %q escaped workspace", repositoryPath)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create materialized parent: %w", err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return fmt.Errorf("write materialized source: %w", err)
	}
	return nil
}

func targetPackages(targets []string, found map[string]struct{}) []string {
	set := map[string]struct{}{}
	for _, target := range targets {
		if _, exists := found[target]; !exists || !strings.HasSuffix(target, ".go") {
			continue
		}
		directory := path.Dir(target)
		if directory == "." {
			set["."] = struct{}{}
		} else {
			set["./"+directory] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func packageResult(workspace, packagePath string, compiled CompileResult) PackageResult {
	digest := sha256.Sum256(compiled.Output)
	result := PackageResult{
		Package: packagePath, Diagnostics: parseDiagnostics(workspace, compiled.Output),
		OutputSHA256: hex.EncodeToString(digest[:]), OutputSizeBytes: int64(len(compiled.Output)),
		CommandExecuted: true, ExitCode: compiled.ExitCode, TestSourcesUsed: true,
	}
	switch {
	case compiled.ExitCode == 0:
		result.Status = "passed"
	case dependencyUnavailable(compiled.Output):
		result.Status = "unavailable"
	default:
		result.Status = "failed"
	}
	return result
}

func parseDiagnostics(workspace string, output []byte) []Diagnostic {
	text := strings.ReplaceAll(string(output), filepath.ToSlash(workspace)+"/", "")
	result := make([]Diagnostic, 0)
	for _, line := range strings.Split(text, "\n") {
		match := diagnosticPattern.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil || !validRepositoryPath(match[1]) || !validMessage(match[4]) {
			continue
		}
		lineNumber, lineErr := strconv.ParseUint(match[2], 10, 32)
		column := uint64(0)
		var columnErr error
		if match[3] != "" {
			column, columnErr = strconv.ParseUint(match[3], 10, 32)
		}
		if lineErr != nil || columnErr != nil || lineNumber == 0 {
			continue
		}
		message := strings.TrimSpace(match[4])
		if len(message) > 2_048 {
			message = message[:2_048]
		}
		result = append(result, Diagnostic{
			Path: match[1], Line: uint32(lineNumber), Column: uint32(column), Message: message,
		})
		if len(result) == DefaultMaxDiagnostics {
			break
		}
	}
	sort.Slice(result, func(left, right int) bool { return diagnosticKey(result[left]) < diagnosticKey(result[right]) })
	return slices.Compact(result)
}

func dependencyUnavailable(output []byte) bool {
	value := strings.ToLower(string(output))
	markers := []string{
		"module lookup disabled by goproxy=off", "goproxy=off", "cannot find module providing package",
		"no required module provides package", "inconsistent vendoring", "updates to go.mod needed",
	}
	return slices.ContainsFunc(markers, func(marker string) bool { return strings.Contains(value, marker) })
}

func unsupportedSourceGaps(repositoryPath string, data []byte) []Gap {
	file, _ := parser.ParseFile(token.NewFileSet(), repositoryPath, data, parser.ImportsOnly|parser.ParseComments)
	if file == nil {
		// The compile command is the authority for ordinary syntax errors. A
		// partial ImportsOnly parse cannot safely prove cgo/embed absence.
		return []Gap{{Code: "source_directive_scan_failed", Path: repositoryPath}}
	}
	gaps := []Gap{}
	for _, imported := range file.Imports {
		value, unquoteErr := strconv.Unquote(imported.Path.Value)
		if unquoteErr == nil && value == "C" {
			gaps = append(gaps, Gap{Code: "cgo_not_supported", Path: repositoryPath})
			break
		}
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(strings.TrimSpace(comment.Text), "//go:embed") {
				gaps = append(gaps, Gap{Code: "embedded_assets_not_materialized", Path: repositoryPath})
				return gaps
			}
		}
	}
	return gaps
}

func hasUnsafeLocalReplace(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		if !strings.Contains(line, "=>") {
			continue
		}
		parts := strings.Fields(line)
		for index, part := range parts {
			if part != "=>" || index+1 >= len(parts) {
				continue
			}
			target := parts[index+1]
			if strings.HasPrefix(target, ".") || strings.HasPrefix(target, "/") ||
				filepath.IsAbs(target) {
				return true
			}
		}
	}
	return false
}

func canonicalize(artifact *Artifact) {
	sort.Slice(artifact.Packages, func(left, right int) bool {
		return artifact.Packages[left].Package < artifact.Packages[right].Package
	})
	for index := range artifact.Packages {
		sort.Slice(artifact.Packages[index].Diagnostics, func(left, right int) bool {
			return diagnosticKey(artifact.Packages[index].Diagnostics[left]) <
				diagnosticKey(artifact.Packages[index].Diagnostics[right])
		})
		artifact.Packages[index].Diagnostics = slices.Compact(artifact.Packages[index].Diagnostics)
	}
	sort.Slice(artifact.Gaps, func(left, right int) bool { return gapKey(artifact.Gaps[left]) < gapKey(artifact.Gaps[right]) })
	artifact.Gaps = slices.Compact(artifact.Gaps)
	artifact.Coverage.DiagnosticsEmitted = 0
	for _, packageResult := range artifact.Packages {
		artifact.Coverage.DiagnosticsEmitted += len(packageResult.Diagnostics)
	}
}

func marshalBounded(artifact *Artifact, limit int64) ([]byte, error) {
	for {
		canonicalize(artifact)
		data, err := json.Marshal(artifact)
		if err != nil {
			return nil, fmt.Errorf("marshal Go compile context: %w", err)
		}
		if int64(len(data)) <= limit {
			return data, nil
		}
		artifact.Coverage.Truncated = true
		if !hasGap(artifact.Gaps, "artifact_budget_exceeded") {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "artifact_budget_exceeded"})
		}
		removed := false
		for index := len(artifact.Packages) - 1; index >= 0; index-- {
			if len(artifact.Packages[index].Diagnostics) != 0 {
				artifact.Packages[index].Diagnostics = artifact.Packages[index].Diagnostics[:len(artifact.Packages[index].Diagnostics)-1]
				removed = true
				break
			}
		}
		if !removed {
			return nil, fmt.Errorf("Go compile context identity exceeds max artifact bytes %d", limit)
		}
	}
}

func contextCoverage(
	artifact Artifact,
	materialized map[string][]byte,
	found map[string]struct{},
) reviewcore.ContextCoverage {
	spans := []reviewcore.ContextSpan{}
	for _, target := range artifact.TargetPaths {
		if _, exists := found[target]; !exists {
			continue
		}
		data := materialized[target]
		lines := uint32(bytes.Count(data, []byte{'\n'}))
		if len(data) != 0 && data[len(data)-1] != '\n' {
			lines++
		}
		if lines != 0 {
			spans = append(spans, reviewcore.ContextSpan{Path: target, StartLine: 1, EndLine: lines})
		}
	}
	symbols := []string{}
	for _, compiled := range artifact.Packages {
		symbols = append(symbols, "go_compile:"+strings.TrimPrefix(compiled.Package, "./")+":"+compiled.Status)
	}
	if len(spans) == 0 && len(symbols) == 0 {
		symbols = []string{"go_compile:coverage"}
	}
	sort.Slice(spans, func(left, right int) bool { return spans[left].Path < spans[right].Path })
	sort.Strings(symbols)
	return reviewcore.ContextCoverage{Spans: spans, Symbols: symbols}
}

func fileGapCode(content gitadapter.FileContent) string {
	if len(content.Reasons) == 0 {
		return "file_unavailable"
	}
	return "file_" + string(content.Reasons[0].Code)
}

func emptySHA256() string {
	digest := sha256.Sum256(nil)
	return hex.EncodeToString(digest[:])
}

func diagnosticKey(value Diagnostic) string {
	return fmt.Sprintf("%s\x00%010d\x00%010d\x00%s", value.Path, value.Line, value.Column, value.Message)
}

func gapKey(value Gap) string { return value.Code + "\x00" + value.Path + "\x00" + value.Package }

func hasGap(gaps []Gap, code string) bool {
	return slices.ContainsFunc(gaps, func(gap Gap) bool { return gap.Code == code })
}

func validRepositoryPath(value string) bool {
	return value != "" && utf8.ValidString(value) && !path.IsAbs(value) &&
		path.Clean(value) == value && value != "." && value != ".." &&
		!strings.HasPrefix(value, "../") && !strings.Contains(value, "\\") && validMessage(value)
}

func validMessage(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == 0 || character == '\r' || character == '\n' || character < 0x20 && character != '\t' {
			return false
		}
	}
	return true
}
