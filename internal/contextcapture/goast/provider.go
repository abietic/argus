// Package goast captures deterministic Go symbol, type, and call facts from
// one exact Git commit. It never reads the mutable worktree.
package goast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"slices"
	"sort"
	"strings"

	"argus.local/argus/internal/contextcapture"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/source/gitadapter"
)

const (
	SchemaVersion    = "argus.context.go_ast.v1alpha1"
	ProviderID       = "argus-go-ast"
	ProviderRevision = "2"
	AdapterSHA256    = "f343930b884436b3cef6e35fda496e4780fa2edfc94285299f23db552821e961"
	// LegacyAdapterSHA256 remains executable for immutable deterministic runs
	// created before the adapter gained a publishable descriptor artifact.
	LegacyAdapterSHA256 = "d89854750360b6c97fcfbdcb4d1b8f22c0a63dd616efc7d95b5f2606ca937ac9"

	DefaultMaxFiles         = 2_000
	DefaultMaxFileBytes     = int64(2 << 20)
	DefaultMaxArtifactBytes = int64(512 << 10)
	DefaultMaxSymbols       = 4_096
	DefaultMaxCalls         = 8_192
	DefaultMaxCallPaths     = 2_048
	DefaultMaxCallPathDepth = 3
)

// Source is the exact-revision read boundary required by the provider.
type Source interface {
	ListFilesAtCommitWithAdmission(
		context.Context,
		string,
		string,
		gitadapter.ScopeAdmission,
		int,
	) (gitadapter.RevisionFileList, error)
	ReadFileAtCommit(
		context.Context,
		string,
		string,
		string,
		int64,
	) (gitadapter.FileContent, error)
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
	SchemaVersion    string     `json:"schema_version"`
	ProviderID       string     `json:"provider_id"`
	ProviderRevision string     `json:"provider_revision"`
	CommitOID        string     `json:"commit_oid"`
	ModulePath       string     `json:"module_path"`
	TargetPaths      []string   `json:"target_paths"`
	Symbols          []Symbol   `json:"symbols"`
	Calls            []Call     `json:"calls"`
	CallPaths        []CallPath `json:"call_paths"`
	Types            []TypeFact `json:"types"`
	Coverage         Coverage   `json:"coverage"`
	Gaps             []Gap      `json:"gaps"`
}

type Symbol struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Signature string `json:"signature"`
	Path      string `json:"path"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
	Target    bool   `json:"target"`
}

type Call struct {
	Caller     string `json:"caller"`
	Callee     string `json:"callee"`
	Path       string `json:"path"`
	Line       uint32 `json:"line"`
	Column     uint32 `json:"column"`
	Resolution string `json:"resolution"`
	Relation   string `json:"relation"`
}

// CallPath is a bounded, exact local-module path rooted at one target
// function or method. Upstream paths follow normal call order and end at the
// target; downstream paths begin at the target. Only go/types-resolved local
// edges participate, so name matches and unresolved dynamic dispatch are
// never promoted into a transitive path.
type CallPath struct {
	Direction string     `json:"direction"`
	Target    string     `json:"target"`
	Symbols   []string   `json:"symbols"`
	Sites     []CallSite `json:"sites"`
}

type CallSite struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Path   string `json:"path"`
	Line   uint32 `json:"line"`
	Column uint32 `json:"column"`
}

type TypeFact struct {
	Symbol     string `json:"symbol"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	Line       uint32 `json:"line"`
	Resolution string `json:"resolution"`
}

type Coverage struct {
	FilesDiscovered      int  `json:"files_discovered"`
	FilesParsed          int  `json:"files_parsed"`
	FilesFailed          int  `json:"files_failed"`
	TargetFilesRequested int  `json:"target_files_requested"`
	TargetFilesFound     int  `json:"target_files_found"`
	TypeGroupsChecked    int  `json:"type_groups_checked"`
	TypeGroupsWithErrors int  `json:"type_groups_with_errors"`
	SymbolsEmitted       int  `json:"symbols_emitted"`
	CallsEmitted         int  `json:"calls_emitted"`
	CallPathsEmitted     int  `json:"call_paths_emitted"`
	TypeFactsEmitted     int  `json:"type_facts_emitted"`
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
		return nil, fmt.Errorf("Go AST context source is required")
	}
	return &Provider{source: source}, nil
}

type parsedFile struct {
	path string
	data []byte
	file *ast.File
	info *types.Info
}

type declaration struct {
	symbol Symbol
	object types.Object
	decl   ast.Node
	file   *parsedFile
	short  string
}

// Capture parses only bytes read through exact-commit Git operations. Partial
// parse/type coverage is retained as typed gaps rather than being promoted to
// complete semantic evidence.
func (provider *Provider) Capture(ctx context.Context, request Request) (Result, error) {
	if provider == nil || provider.source == nil || ctx == nil {
		return Result{}, fmt.Errorf("Go AST context dependencies are required")
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
		ctx,
		request.RepositoryRoot,
		request.CommitOID,
		gitadapter.ScopeAdmission{
			Include: []string{"**/*.go", "go.mod"}, Exclude: []string{},
			PolicyInclude: slices.Clone(request.PolicyInclude),
			PolicyExclude: slices.Clone(request.PolicyExclude),
		},
		request.MaxFiles,
	)
	if err != nil {
		return Result{}, fmt.Errorf("list exact Go files: %w", err)
	}
	if err := contextcapture.RequireExactRevision(
		"Go AST file listing", request.CommitOID, listing.CommitOID,
	); err != nil {
		return Result{}, err
	}

	artifact := Artifact{
		SchemaVersion: SchemaVersion, ProviderID: ProviderID,
		ProviderRevision: ProviderRevision, CommitOID: request.CommitOID,
		ModulePath: "", TargetPaths: targetPaths, Symbols: []Symbol{}, Calls: []Call{}, CallPaths: []CallPath{},
		Types: []TypeFact{}, Gaps: []Gap{},
		Coverage: Coverage{
			TargetFilesRequested: len(targetPaths),
		},
	}
	if listing.Completeness != gitadapter.CompletenessComplete {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "go_file_listing_partial"})
	}

	targetSet := make(map[string]struct{}, len(targetPaths))
	for _, targetPath := range targetPaths {
		targetSet[targetPath] = struct{}{}
	}
	foundTargets := make(map[string]struct{}, len(targetPaths))
	fset := token.NewFileSet()
	parsed := make([]*parsedFile, 0, len(listing.Files))
	for _, entry := range listing.Files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		content, readErr := provider.source.ReadFileAtCommit(
			ctx, request.RepositoryRoot, request.CommitOID, entry.Path,
			request.MaxFileBytes,
		)
		if readErr != nil {
			return Result{}, fmt.Errorf("read exact Go file %q: %w", entry.Path, readErr)
		}
		if err := contextcapture.RequireExactRevision(
			"Go AST file "+entry.Path, request.CommitOID, content.CommitOID,
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
		artifact.Coverage.FilesDiscovered++
		if !content.Included {
			artifact.Coverage.FilesFailed++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: fileGapCode(content), Path: entry.Path})
			continue
		}
		file, parseErr := parser.ParseFile(fset, entry.Path, content.Data, parser.SkipObjectResolution)
		if parseErr != nil {
			artifact.Coverage.FilesFailed++
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "parse_failed", Path: entry.Path})
			continue
		}
		parsed = append(parsed, &parsedFile{path: entry.Path, data: content.Data, file: file})
		artifact.Coverage.FilesParsed++
		if _, targeted := targetSet[entry.Path]; targeted {
			foundTargets[entry.Path] = struct{}{}
		}
	}
	if artifact.ModulePath == "" {
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "module_path_unavailable", Path: "go.mod"})
	}
	artifact.Coverage.TargetFilesFound = len(foundTargets)
	for _, targetPath := range targetPaths {
		if !strings.HasSuffix(targetPath, ".go") {
			continue
		}
		if _, found := foundTargets[targetPath]; !found {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "target_file_unavailable", Path: targetPath})
		}
	}

	typeCheckGroups(fset, parsed, artifact.ModulePath, &artifact)
	declarations, objectIDs := collectDeclarations(fset, parsed, targetSet)
	targetIDs, targetShort := targetIndexes(declarations)
	callFacts, relevantIDs := collectRelevantCalls(
		fset, parsed, declarations, objectIDs, targetIDs, targetShort,
	)
	callPaths, pathIDs, pathTruncated := collectExactCallPaths(
		fset, parsed, declarations, objectIDs, targetIDs,
	)
	for id := range pathIDs {
		relevantIDs[id] = struct{}{}
	}
	for id := range targetIDs {
		relevantIDs[id] = struct{}{}
	}

	for _, declaration := range declarations {
		if _, relevant := relevantIDs[declaration.symbol.ID]; !relevant {
			continue
		}
		artifact.Symbols = append(artifact.Symbols, declaration.symbol)
		artifact.Types = append(artifact.Types, typeFactFor(declaration))
	}
	artifact.Calls = callFacts
	artifact.CallPaths = callPaths
	if len(artifact.Symbols) > DefaultMaxSymbols {
		artifact.Symbols = artifact.Symbols[:DefaultMaxSymbols]
		retained := make(map[string]struct{}, len(artifact.Symbols))
		for _, symbol := range artifact.Symbols {
			retained[symbol.ID] = struct{}{}
		}
		artifact.CallPaths = slices.DeleteFunc(artifact.CallPaths, func(callPath CallPath) bool {
			for _, symbol := range callPath.Symbols {
				if _, ok := retained[symbol]; !ok {
					return true
				}
			}
			return false
		})
		artifact.Coverage.Truncated = true
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "symbol_fact_limit_exceeded"})
	}
	if len(artifact.Calls) > DefaultMaxCalls {
		artifact.Calls = artifact.Calls[:DefaultMaxCalls]
		artifact.Coverage.Truncated = true
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "call_fact_limit_exceeded"})
	}
	if pathTruncated {
		artifact.Coverage.Truncated = true
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "call_path_limit_exceeded"})
	}

	canonicalize(&artifact)
	data, err := marshalBounded(&artifact, request.MaxArtifactBytes)
	if err != nil {
		return Result{}, err
	}
	if err := artifact.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate Go AST context: %w", err)
	}
	coverage := contextCoverage(artifact, parsed, targetSet)
	return Result{Artifact: artifact, Bytes: data, Coverage: coverage}, nil
}

func normalizeRequest(request Request) Request {
	if request.MaxFiles == 0 {
		request.MaxFiles = DefaultMaxFiles
	}
	if request.MaxFileBytes == 0 {
		request.MaxFileBytes = DefaultMaxFileBytes
	}
	if request.MaxArtifactBytes == 0 {
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
	result := slices.Clone(values)
	sort.Strings(result)
	for index, value := range result {
		if value == "" || path.IsAbs(value) || path.Clean(value) != value || strings.Contains(value, "\\") {
			return nil, fmt.Errorf("target path %d must be a clean repository-relative path", index)
		}
		if index > 0 && value == result[index-1] {
			return nil, fmt.Errorf("target paths contain duplicate %q", value)
		}
	}
	return result, nil
}

func fileGapCode(content gitadapter.FileContent) string {
	if len(content.Reasons) == 0 {
		return "file_unavailable"
	}
	return "file_" + string(content.Reasons[0].Code)
}

func parseModulePath(data []byte) string {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) == 2 && fields[0] == "module" && validModulePath(fields[1]) {
			return fields[1]
		}
	}
	return ""
}

func validModulePath(value string) bool {
	if value == "" || len(value) > 512 || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\\:@") {
		return false
	}
	for _, character := range value {
		if character <= ' ' || character == 0x7f {
			return false
		}
	}
	return true
}

type groupKey struct{ directory, packageName string }

type exactTypeGroup struct {
	key        groupKey
	importPath string
	files      []*parsedFile
	info       *types.Info
	pkg        *types.Package
	checkErr   error
	checking   bool
	checked    bool
	hadError   bool
}

type exactRevisionImporter struct {
	fset     *token.FileSet
	groups   map[string]*exactTypeGroup
	standard types.Importer
}

func typeCheckGroups(fset *token.FileSet, files []*parsedFile, modulePath string, artifact *Artifact) {
	groupFiles := make(map[groupKey][]*parsedFile)
	for _, file := range files {
		key := groupKey{directory: path.Dir(file.path), packageName: file.file.Name.Name}
		groupFiles[key] = append(groupFiles[key], file)
	}
	keys := make([]groupKey, 0, len(groupFiles))
	for key := range groupFiles {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].directory == keys[j].directory {
			return keys[i].packageName < keys[j].packageName
		}
		return keys[i].directory < keys[j].directory
	})
	groupsByDirectory := make(map[string][]*exactTypeGroup)
	allGroups := make([]*exactTypeGroup, 0, len(keys))
	for _, key := range keys {
		group := groupFiles[key]
		sort.Slice(group, func(i, j int) bool { return group[i].path < group[j].path })
		info := &types.Info{
			Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		for _, file := range group {
			file.info = info
		}
		current := &exactTypeGroup{key: key, files: group, info: info}
		groupsByDirectory[key.directory] = append(groupsByDirectory[key.directory], current)
		allGroups = append(allGroups, current)
	}
	exactImporter := &exactRevisionImporter{
		fset: fset, groups: map[string]*exactTypeGroup{}, standard: importer.Default(),
	}
	if modulePath != "" {
		directories := make([]string, 0, len(groupsByDirectory))
		for directory := range groupsByDirectory {
			directories = append(directories, directory)
		}
		sort.Strings(directories)
		for _, directory := range directories {
			primary := primaryTypeGroup(groupsByDirectory[directory])
			if primary == nil {
				gapPath := directory
				if gapPath == "." {
					gapPath = ""
				}
				artifact.Gaps = append(artifact.Gaps, Gap{Code: "local_package_ambiguous", Path: gapPath})
				continue
			}
			primary.importPath = moduleImportPath(modulePath, directory)
			exactImporter.groups[primary.importPath] = primary
		}
	}
	for _, group := range allGroups {
		if group.importPath == "" {
			group.importPath = "argus.exact/" + group.key.directory + "/" + group.key.packageName
		}
		_, _ = exactImporter.check(group)
	}
	artifact.Coverage.TypeGroupsChecked = len(allGroups)
	for _, group := range allGroups {
		if !group.hadError {
			continue
		}
		artifact.Coverage.TypeGroupsWithErrors++
		groupPath := group.key.directory
		if groupPath == "." {
			groupPath = ""
		}
		artifact.Gaps = append(artifact.Gaps, Gap{Code: "type_check_partial", Path: groupPath})
	}
}

func primaryTypeGroup(candidates []*exactTypeGroup) *exactTypeGroup {
	var primary *exactTypeGroup
	for _, candidate := range candidates {
		if strings.HasSuffix(candidate.key.packageName, "_test") {
			continue
		}
		if primary != nil {
			return nil
		}
		primary = candidate
	}
	if primary == nil && len(candidates) == 1 {
		return candidates[0]
	}
	return primary
}

func moduleImportPath(modulePath, directory string) string {
	if directory == "." {
		return modulePath
	}
	return modulePath + "/" + directory
}

func (value *exactRevisionImporter) Import(importPath string) (*types.Package, error) {
	if group := value.groups[importPath]; group != nil {
		pkg, err := value.check(group)
		if pkg != nil {
			return pkg, nil
		}
		return nil, err
	}
	first := importPath
	if index := strings.IndexByte(first, '/'); index >= 0 {
		first = first[:index]
	}
	if strings.Contains(first, ".") {
		return nil, fmt.Errorf("external import %q is unavailable in exact-revision type checking", importPath)
	}
	return value.standard.Import(importPath)
}

func (value *exactRevisionImporter) check(group *exactTypeGroup) (pkg *types.Package, err error) {
	if group.checked {
		return group.pkg, group.checkErr
	}
	if group.checking {
		group.hadError = true
		return nil, fmt.Errorf("exact local import cycle at %q", group.importPath)
	}
	group.checking = true
	defer func() {
		group.checking = false
		group.checked = true
		if recover() != nil {
			group.hadError = true
			pkg = nil
			err = fmt.Errorf("panic while type checking exact package %q", group.importPath)
		}
		group.checkErr = err
	}()
	asts := make([]*ast.File, len(group.files))
	for index, file := range group.files {
		asts[index] = file.file
	}
	config := types.Config{Importer: value, Error: func(error) { group.hadError = true }}
	group.pkg, err = config.Check(group.importPath, value.fset, asts, group.info)
	if err != nil {
		group.hadError = true
	}
	return group.pkg, err
}

func collectDeclarations(
	fset *token.FileSet,
	files []*parsedFile,
	targetSet map[string]struct{},
) ([]declaration, map[types.Object]string) {
	declarations := []declaration{}
	baseCounts := map[string]int{}
	for _, file := range files {
		for _, decl := range file.file.Decls {
			switch value := decl.(type) {
			case *ast.FuncDecl:
				baseCounts[symbolBaseID(file, value)]++
			case *ast.GenDecl:
				for _, spec := range value.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok {
						baseCounts[typeBaseID(file, typeSpec)]++
					}
				}
			}
		}
	}
	objectIDs := map[types.Object]string{}
	for _, file := range files {
		_, targeted := targetSet[file.path]
		for _, decl := range file.file.Decls {
			switch value := decl.(type) {
			case *ast.FuncDecl:
				base := symbolBaseID(file, value)
				id := disambiguateID(base, baseCounts[base], file.path, fset.Position(value.Pos()).Line)
				object := file.info.Defs[value.Name]
				symbol := symbolForNode(fset, file.path, id, value.Name.Name, funcKind(value), value, object, targeted)
				declarations = append(declarations, declaration{symbol: symbol, object: object, decl: value, file: file, short: value.Name.Name})
				if object != nil {
					objectIDs[object] = id
				}
			case *ast.GenDecl:
				for _, spec := range value.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					base := typeBaseID(file, typeSpec)
					id := disambiguateID(base, baseCounts[base], file.path, fset.Position(typeSpec.Pos()).Line)
					object := file.info.Defs[typeSpec.Name]
					symbol := symbolForNode(fset, file.path, id, typeSpec.Name.Name, "type", typeSpec, object, targeted)
					declarations = append(declarations, declaration{symbol: symbol, object: object, decl: typeSpec, file: file, short: typeSpec.Name.Name})
					if object != nil {
						objectIDs[object] = id
					}
				}
			}
		}
	}
	sort.Slice(declarations, func(i, j int) bool { return declarations[i].symbol.ID < declarations[j].symbol.ID })
	return declarations, objectIDs
}

func packageID(file *parsedFile) string {
	directory := path.Dir(file.path)
	if directory == "." {
		return file.file.Name.Name
	}
	return directory + "/" + file.file.Name.Name
}

func symbolBaseID(file *parsedFile, declaration *ast.FuncDecl) string {
	prefix := packageID(file) + "."
	if declaration.Recv != nil && len(declaration.Recv.List) != 0 {
		prefix += receiverName(declaration.Recv.List[0].Type) + "."
	}
	return prefix + declaration.Name.Name
}

func typeBaseID(file *parsedFile, declaration *ast.TypeSpec) string {
	return packageID(file) + "." + declaration.Name.Name
}

func receiverName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return receiverName(value.X)
	case *ast.IndexExpr:
		return receiverName(value.X)
	case *ast.IndexListExpr:
		return receiverName(value.X)
	default:
		return "receiver"
	}
}

func disambiguateID(base string, count int, filePath string, line int) string {
	if count == 1 {
		return base
	}
	return fmt.Sprintf("%s@%s:%d", base, filePath, line)
}

func funcKind(declaration *ast.FuncDecl) string {
	if declaration.Recv != nil {
		return "method"
	}
	return "function"
}

func symbolForNode(fset *token.FileSet, filePath, id, name, kind string, node ast.Node, object types.Object, target bool) Symbol {
	start := fset.Position(node.Pos()).Line
	end := fset.Position(node.End()).Line
	signature, resolution := syntaxString(fset, node), "syntax"
	_ = resolution
	if object != nil && object.Type() != nil {
		signature = stableTypeString(object.Type())
	}
	return Symbol{ID: id, Name: name, Kind: kind, Signature: signature, Path: filePath, StartLine: uint32(start), EndLine: uint32(end), Target: target}
}

func syntaxString(fset *token.FileSet, node ast.Node) string {
	var buffer bytes.Buffer
	if err := format.Node(&buffer, fset, node); err != nil {
		return "unavailable"
	}
	value := strings.Join(strings.Fields(buffer.String()), " ")
	if len(value) > 1024 {
		return value[:1024]
	}
	return value
}

func stableTypeString(value types.Type) string {
	result := types.TypeString(value, func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		return pkg.Path()
	})
	if len(result) > 1024 {
		return result[:1024]
	}
	return result
}

func targetIndexes(declarations []declaration) (map[string]struct{}, map[string][]string) {
	ids := map[string]struct{}{}
	short := map[string][]string{}
	for _, declaration := range declarations {
		if !declaration.symbol.Target {
			continue
		}
		ids[declaration.symbol.ID] = struct{}{}
		short[declaration.short] = append(short[declaration.short], declaration.symbol.ID)
	}
	for name := range short {
		sort.Strings(short[name])
	}
	return ids, short
}

func collectRelevantCalls(
	fset *token.FileSet,
	files []*parsedFile,
	declarations []declaration,
	objectIDs map[types.Object]string,
	targetIDs map[string]struct{},
	targetShort map[string][]string,
) ([]Call, map[string]struct{}) {
	declByNode := map[*ast.FuncDecl]declaration{}
	for _, declaration := range declarations {
		if function, ok := declaration.decl.(*ast.FuncDecl); ok {
			declByNode[function] = declaration
		}
	}
	relevant := map[string]struct{}{}
	calls := []Call{}
	for _, file := range files {
		for _, top := range file.file.Decls {
			function, ok := top.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			caller := declByNode[function]
			_, callerTarget := targetIDs[caller.symbol.ID]
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee, short, resolution, localID := resolveCall(file.info, call.Fun, objectIDs, fset)
				_, exactTarget := targetIDs[localID]
				relation := "callee_of_target"
				include := callerTarget
				if callerTarget && exactTarget {
					include = true
					relation = "within_target"
				} else if exactTarget {
					include = true
					relation = "caller_of_target"
				} else if candidates := targetShort[short]; !callerTarget && len(candidates) != 0 {
					include = true
					relation = "possible_caller_of_target"
					if len(candidates) == 1 {
						callee = candidates[0]
						resolution = "name_match"
						localID = candidates[0]
					}
				}
				if !include {
					return true
				}
				position := fset.Position(call.Pos())
				calls = append(calls, Call{Caller: caller.symbol.ID, Callee: callee, Path: file.path, Line: uint32(position.Line), Column: uint32(position.Column), Resolution: resolution, Relation: relation})
				relevant[caller.symbol.ID] = struct{}{}
				if localID != "" {
					relevant[localID] = struct{}{}
				}
				return true
			})
		}
	}
	sort.Slice(calls, func(i, j int) bool {
		left, right := calls[i], calls[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Column != right.Column {
			return left.Column < right.Column
		}
		if left.Caller != right.Caller {
			return left.Caller < right.Caller
		}
		if left.Callee != right.Callee {
			return left.Callee < right.Callee
		}
		if left.Resolution != right.Resolution {
			return left.Resolution < right.Resolution
		}
		return left.Relation < right.Relation
	})
	return calls, relevant
}

func collectExactCallPaths(
	fset *token.FileSet,
	files []*parsedFile,
	declarations []declaration,
	objectIDs map[types.Object]string,
	targetIDs map[string]struct{},
) ([]CallPath, map[string]struct{}, bool) {
	declByNode := map[*ast.FuncDecl]declaration{}
	for _, declaration := range declarations {
		if function, ok := declaration.decl.(*ast.FuncDecl); ok {
			declByNode[function] = declaration
		}
	}
	edges := []CallSite{}
	for _, file := range files {
		for _, top := range file.file.Decls {
			function, ok := top.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			caller := declByNode[function]
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee, _, resolution, localID := resolveCall(file.info, call.Fun, objectIDs, fset)
				if resolution != "go_types_exact" || localID == "" {
					return true
				}
				position := fset.Position(call.Pos())
				edges = append(edges, CallSite{
					Caller: caller.symbol.ID, Callee: callee, Path: file.path,
					Line: uint32(position.Line), Column: uint32(position.Column),
				})
				return true
			})
		}
	}
	sort.Slice(edges, func(i, j int) bool { return callSiteLess(edges[i], edges[j]) })
	downstream := map[string][]CallSite{}
	upstream := map[string][]CallSite{}
	for _, edge := range edges {
		downstream[edge.Caller] = append(downstream[edge.Caller], edge)
		upstream[edge.Callee] = append(upstream[edge.Callee], edge)
	}
	targets := make([]string, 0, len(targetIDs))
	for target := range targetIDs {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	paths := []CallPath{}
	relevant := map[string]struct{}{}
	truncated := false
	appendPath := func(direction, target string, symbols []string, sites []CallSite) bool {
		if len(sites) < 2 {
			return true
		}
		if len(paths) >= DefaultMaxCallPaths {
			truncated = true
			return false
		}
		pathSymbols := slices.Clone(symbols)
		pathSites := slices.Clone(sites)
		if direction == "upstream" {
			slices.Reverse(pathSymbols)
			slices.Reverse(pathSites)
		}
		paths = append(paths, CallPath{Direction: direction, Target: target, Symbols: pathSymbols, Sites: pathSites})
		for _, symbol := range pathSymbols {
			relevant[symbol] = struct{}{}
		}
		return true
	}
	var walk func(string, string, string, []string, []CallSite, map[string]struct{}) bool
	walk = func(direction, target, current string, symbols []string, sites []CallSite, seen map[string]struct{}) bool {
		if len(sites) >= DefaultMaxCallPathDepth {
			return true
		}
		next := downstream[current]
		if direction == "upstream" {
			next = upstream[current]
		}
		for _, edge := range next {
			nextSymbol := edge.Callee
			if direction == "upstream" {
				nextSymbol = edge.Caller
			}
			if _, exists := seen[nextSymbol]; exists {
				continue
			}
			nextSymbols := append(slices.Clone(symbols), nextSymbol)
			nextSites := append(slices.Clone(sites), edge)
			if !appendPath(direction, target, nextSymbols, nextSites) {
				return false
			}
			nextSeen := make(map[string]struct{}, len(seen)+1)
			for symbol := range seen {
				nextSeen[symbol] = struct{}{}
			}
			nextSeen[nextSymbol] = struct{}{}
			if !walk(direction, target, nextSymbol, nextSymbols, nextSites, nextSeen) {
				return false
			}
		}
		return true
	}
	for _, target := range targets {
		for _, direction := range []string{"downstream", "upstream"} {
			if !walk(direction, target, target, []string{target}, nil, map[string]struct{}{target: {}}) {
				break
			}
		}
		if truncated {
			break
		}
	}
	sort.Slice(paths, func(i, j int) bool { return callPathLess(paths[i], paths[j]) })
	return paths, relevant, truncated
}

func callSiteLess(left, right CallSite) bool {
	if left.Caller != right.Caller {
		return left.Caller < right.Caller
	}
	if left.Callee != right.Callee {
		return left.Callee < right.Callee
	}
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	return left.Column < right.Column
}

func callPathLess(left, right CallPath) bool {
	if left.Direction != right.Direction {
		return left.Direction < right.Direction
	}
	if left.Target != right.Target {
		return left.Target < right.Target
	}
	leftSymbols, rightSymbols := strings.Join(left.Symbols, "\x00"), strings.Join(right.Symbols, "\x00")
	if leftSymbols != rightSymbols {
		return leftSymbols < rightSymbols
	}
	for index := range min(len(left.Sites), len(right.Sites)) {
		if left.Sites[index] != right.Sites[index] {
			return callSiteLess(left.Sites[index], right.Sites[index])
		}
	}
	return len(left.Sites) < len(right.Sites)
}

func resolveCall(info *types.Info, expression ast.Expr, objectIDs map[types.Object]string, fset *token.FileSet) (string, string, string, string) {
	var object types.Object
	short := ""
	switch value := expression.(type) {
	case *ast.Ident:
		short = value.Name
		object = info.Uses[value]
	case *ast.SelectorExpr:
		short = value.Sel.Name
		if selection := info.Selections[value]; selection != nil {
			object = selection.Obj()
		} else {
			object = info.Uses[value.Sel]
		}
	}
	if id := objectIDs[object]; id != "" {
		return id, short, "go_types_exact", id
	}
	if object != nil {
		name := object.Name()
		if object.Pkg() != nil {
			name = object.Pkg().Path() + "." + name
		}
		return name, short, "go_types_external", ""
	}
	return syntaxString(fset, expression), short, "unresolved", ""
}

func typeFactFor(declaration declaration) TypeFact {
	value, resolution := declaration.symbol.Signature, "syntax"
	if declaration.object != nil && declaration.object.Type() != nil {
		value, resolution = stableTypeString(declaration.object.Type()), "go_types"
	}
	return TypeFact{Symbol: declaration.symbol.ID, Type: value, Path: declaration.symbol.Path, Line: declaration.symbol.StartLine, Resolution: resolution}
}

func canonicalize(artifact *Artifact) {
	sort.Slice(artifact.Symbols, func(i, j int) bool { return artifact.Symbols[i].ID < artifact.Symbols[j].ID })
	sort.Slice(artifact.Types, func(i, j int) bool { return artifact.Types[i].Symbol < artifact.Types[j].Symbol })
	sort.Slice(artifact.Gaps, func(i, j int) bool {
		if artifact.Gaps[i].Path != artifact.Gaps[j].Path {
			return artifact.Gaps[i].Path < artifact.Gaps[j].Path
		}
		if artifact.Gaps[i].Line != artifact.Gaps[j].Line {
			return artifact.Gaps[i].Line < artifact.Gaps[j].Line
		}
		return artifact.Gaps[i].Code < artifact.Gaps[j].Code
	})
	if len(artifact.Gaps) > 1 {
		unique := artifact.Gaps[:1]
		for _, gap := range artifact.Gaps[1:] {
			if gap != unique[len(unique)-1] {
				unique = append(unique, gap)
			}
		}
		artifact.Gaps = unique
	}
	artifact.Coverage.SymbolsEmitted = len(artifact.Symbols)
	artifact.Coverage.CallsEmitted = len(artifact.Calls)
	artifact.Coverage.CallPathsEmitted = len(artifact.CallPaths)
	artifact.Coverage.TypeFactsEmitted = len(artifact.Types)
}

func marshalBounded(artifact *Artifact, limit int64) ([]byte, error) {
	for {
		canonicalize(artifact)
		data, err := json.Marshal(artifact)
		if err != nil {
			return nil, fmt.Errorf("marshal Go AST context: %w", err)
		}
		if int64(len(data)) <= limit {
			return data, nil
		}
		artifact.Coverage.Truncated = true
		if !hasGap(artifact.Gaps, "artifact_budget_exceeded") {
			artifact.Gaps = append(artifact.Gaps, Gap{Code: "artifact_budget_exceeded"})
		}
		switch {
		case len(artifact.CallPaths) > 0:
			artifact.CallPaths = artifact.CallPaths[:len(artifact.CallPaths)-1]
		case len(artifact.Calls) > 0:
			artifact.Calls = artifact.Calls[:len(artifact.Calls)-1]
		case len(artifact.Types) > 0:
			artifact.Types = artifact.Types[:len(artifact.Types)-1]
		case hasNonTargetSymbol(artifact.Symbols):
			for index := len(artifact.Symbols) - 1; index >= 0; index-- {
				if !artifact.Symbols[index].Target {
					artifact.Symbols = slices.Delete(artifact.Symbols, index, index+1)
					break
				}
			}
		case len(artifact.Symbols) > 0:
			artifact.Symbols = artifact.Symbols[:len(artifact.Symbols)-1]
		default:
			return nil, fmt.Errorf("Go AST context identity exceeds max artifact bytes %d", limit)
		}
	}
}

func hasGap(gaps []Gap, code string) bool {
	for _, gap := range gaps {
		if gap.Code == code {
			return true
		}
	}
	return false
}
func hasNonTargetSymbol(symbols []Symbol) bool {
	for _, symbol := range symbols {
		if !symbol.Target {
			return true
		}
	}
	return false
}

func contextCoverage(artifact Artifact, files []*parsedFile, targetSet map[string]struct{}) reviewcore.ContextCoverage {
	spans := []reviewcore.ContextSpan{}
	for _, file := range files {
		if _, targeted := targetSet[file.path]; !targeted {
			continue
		}
		lines := uint32(bytes.Count(file.data, []byte{'\n'}))
		if len(file.data) != 0 && file.data[len(file.data)-1] != '\n' {
			lines++
		}
		if lines != 0 {
			spans = append(spans, reviewcore.ContextSpan{Path: file.path, StartLine: 1, EndLine: lines})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Path < spans[j].Path })
	symbols := []string{}
	for _, symbol := range artifact.Symbols {
		if symbol.Target {
			symbols = append(symbols, symbol.ID)
		}
	}
	sort.Strings(symbols)
	if len(spans) == 0 && len(symbols) == 0 {
		symbols = []string{"go_ast:coverage"}
	}
	return reviewcore.ContextCoverage{Spans: spans, Symbols: symbols}
}
