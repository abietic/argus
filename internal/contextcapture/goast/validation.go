package goast

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var exactCommitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func DecodeArtifact(data []byte) (Artifact, error) {
	if len(data) == 0 || int64(len(data)) > DefaultMaxArtifactBytes {
		return Artifact{}, fmt.Errorf("Go AST context bytes must be non-empty and at most %d bytes", DefaultMaxArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact Artifact
	if err := decoder.Decode(&artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode Go AST context: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return Artifact{}, fmt.Errorf("decode Go AST context trailing data: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func (artifact Artifact) Validate() error {
	if artifact.SchemaVersion != SchemaVersion || artifact.ProviderID != ProviderID ||
		artifact.ProviderRevision != ProviderRevision {
		return fmt.Errorf("Go AST context identity is unsupported")
	}
	if !exactCommitOID.MatchString(artifact.CommitOID) {
		return fmt.Errorf("commit_oid must be an exact lowercase Git object ID")
	}
	if artifact.ModulePath == "" {
		if !hasGap(artifact.Gaps, "module_path_unavailable") {
			return fmt.Errorf("empty module_path requires module_path_unavailable gap")
		}
	} else if !validModulePath(artifact.ModulePath) {
		return fmt.Errorf("module_path is invalid")
	}
	if artifact.TargetPaths == nil || artifact.Symbols == nil || artifact.Calls == nil ||
		artifact.CallPaths == nil || artifact.Types == nil || artifact.Gaps == nil {
		return fmt.Errorf("Go AST context collections must be explicit arrays")
	}
	if err := validateSortedPaths("target_paths", artifact.TargetPaths); err != nil {
		return err
	}
	targets := make(map[string]struct{}, len(artifact.TargetPaths))
	for _, target := range artifact.TargetPaths {
		targets[target] = struct{}{}
	}
	previousSymbol := ""
	symbols := make(map[string]struct{}, len(artifact.Symbols))
	targetSymbols := make(map[string]struct{})
	for index, symbol := range artifact.Symbols {
		if !validText(symbol.ID) || !validText(symbol.Name) || !validText(symbol.Signature) {
			return fmt.Errorf("symbols[%d] contains invalid text", index)
		}
		switch symbol.Kind {
		case "function", "method", "type":
		default:
			return fmt.Errorf("symbols[%d] has unsupported kind %q", index, symbol.Kind)
		}
		if !validRepositoryPath(symbol.Path) || symbol.StartLine == 0 || symbol.EndLine < symbol.StartLine {
			return fmt.Errorf("symbols[%d] has invalid source coverage", index)
		}
		if symbol.ID <= previousSymbol {
			return fmt.Errorf("symbols must be strictly sorted by id")
		}
		previousSymbol = symbol.ID
		symbols[symbol.ID] = struct{}{}
		if symbol.Target {
			if _, ok := targets[symbol.Path]; !ok {
				return fmt.Errorf("target symbol %q is outside target_paths", symbol.ID)
			}
			targetSymbols[symbol.ID] = struct{}{}
		}
	}
	previousCall := Call{}
	for index, call := range artifact.Calls {
		if !validText(call.Caller) || !validText(call.Callee) ||
			!validRepositoryPath(call.Path) || call.Line == 0 || call.Column == 0 {
			return fmt.Errorf("calls[%d] is invalid", index)
		}
		switch call.Resolution {
		case "go_types_exact", "go_types_external", "name_match", "unresolved":
		default:
			return fmt.Errorf("calls[%d] has unsupported resolution %q", index, call.Resolution)
		}
		switch call.Relation {
		case "within_target", "caller_of_target", "possible_caller_of_target", "callee_of_target":
		default:
			return fmt.Errorf("calls[%d] has unsupported relation %q", index, call.Relation)
		}
		if index > 0 && !callLess(previousCall, call) {
			return fmt.Errorf("calls must be strictly sorted")
		}
		previousCall = call
	}
	previousPath := CallPath{}
	for index, path := range artifact.CallPaths {
		if err := path.validate(symbols, targetSymbols); err != nil {
			return fmt.Errorf("call_paths[%d]: %w", index, err)
		}
		if index > 0 && !callPathLess(previousPath, path) {
			return fmt.Errorf("call_paths must be strictly sorted")
		}
		previousPath = path
	}
	previousType := ""
	for index, fact := range artifact.Types {
		if !validText(fact.Symbol) || !validText(fact.Type) ||
			!validRepositoryPath(fact.Path) || fact.Line == 0 {
			return fmt.Errorf("types[%d] is invalid", index)
		}
		if fact.Resolution != "go_types" && fact.Resolution != "syntax" {
			return fmt.Errorf("types[%d] has unsupported resolution %q", index, fact.Resolution)
		}
		if fact.Symbol <= previousType {
			return fmt.Errorf("types must be strictly sorted by symbol")
		}
		previousType = fact.Symbol
	}
	if err := artifact.Coverage.validate(artifact); err != nil {
		return err
	}
	previousGap := Gap{}
	for index, gap := range artifact.Gaps {
		if !validText(gap.Code) || gap.Path != "" && !validRepositoryPath(gap.Path) {
			return fmt.Errorf("gaps[%d] is invalid", index)
		}
		if index > 0 && !gapLess(previousGap, gap) {
			return fmt.Errorf("gaps must be strictly sorted")
		}
		previousGap = gap
	}
	if artifact.Coverage.Truncated && !hasGap(artifact.Gaps, "artifact_budget_exceeded") &&
		!hasGap(artifact.Gaps, "symbol_fact_limit_exceeded") &&
		!hasGap(artifact.Gaps, "call_fact_limit_exceeded") &&
		!hasGap(artifact.Gaps, "call_path_limit_exceeded") {
		return fmt.Errorf("truncated coverage requires an explicit limit gap")
	}
	return nil
}

func (coverage Coverage) validate(artifact Artifact) error {
	values := []int{
		coverage.FilesDiscovered, coverage.FilesParsed, coverage.FilesFailed,
		coverage.TargetFilesRequested, coverage.TargetFilesFound,
		coverage.TypeGroupsChecked, coverage.TypeGroupsWithErrors,
		coverage.SymbolsEmitted, coverage.CallsEmitted, coverage.TypeFactsEmitted,
		coverage.CallPathsEmitted,
	}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("coverage counters must not be negative")
		}
	}
	if coverage.FilesParsed+coverage.FilesFailed > coverage.FilesDiscovered ||
		coverage.TargetFilesFound > coverage.TargetFilesRequested ||
		coverage.TypeGroupsWithErrors > coverage.TypeGroupsChecked {
		return fmt.Errorf("coverage counters are inconsistent")
	}
	if coverage.SymbolsEmitted != len(artifact.Symbols) ||
		coverage.CallsEmitted != len(artifact.Calls) ||
		coverage.CallPathsEmitted != len(artifact.CallPaths) ||
		coverage.TypeFactsEmitted != len(artifact.Types) {
		return fmt.Errorf("coverage emitted counters do not match fact arrays")
	}
	return nil
}

func (callPath CallPath) validate(symbols, targetSymbols map[string]struct{}) error {
	if callPath.Direction != "upstream" && callPath.Direction != "downstream" {
		return fmt.Errorf("direction is unsupported")
	}
	if _, ok := targetSymbols[callPath.Target]; !ok {
		return fmt.Errorf("target is not an emitted target symbol")
	}
	if len(callPath.Symbols) < 3 || len(callPath.Symbols) > DefaultMaxCallPathDepth+1 ||
		len(callPath.Sites) != len(callPath.Symbols)-1 {
		return fmt.Errorf("path must contain two to %d exact calls", DefaultMaxCallPathDepth)
	}
	if callPath.Direction == "downstream" && callPath.Symbols[0] != callPath.Target ||
		callPath.Direction == "upstream" && callPath.Symbols[len(callPath.Symbols)-1] != callPath.Target {
		return fmt.Errorf("target does not match path direction")
	}
	seen := make(map[string]struct{}, len(callPath.Symbols))
	for index, symbol := range callPath.Symbols {
		if _, ok := symbols[symbol]; !ok {
			return fmt.Errorf("symbols[%d] is not emitted", index)
		}
		if _, exists := seen[symbol]; exists {
			return fmt.Errorf("symbols must form a simple path")
		}
		seen[symbol] = struct{}{}
	}
	for index, site := range callPath.Sites {
		if site.Caller != callPath.Symbols[index] || site.Callee != callPath.Symbols[index+1] ||
			!validRepositoryPath(site.Path) || site.Line == 0 || site.Column == 0 {
			return fmt.Errorf("sites[%d] does not bind its adjacent symbols", index)
		}
	}
	return nil
}

func validateSortedPaths(name string, values []string) error {
	if !slices.IsSorted(values) {
		return fmt.Errorf("%s must be sorted", name)
	}
	for index, value := range values {
		if !validRepositoryPath(value) {
			return fmt.Errorf("%s[%d] is invalid", name, index)
		}
		if index > 0 && value == values[index-1] {
			return fmt.Errorf("%s contains duplicate %q", name, value)
		}
	}
	return nil
}

func validRepositoryPath(value string) bool {
	return value != "" && utf8.ValidString(value) && validText(value) &&
		!path.IsAbs(value) && path.Clean(value) == value && value != "." &&
		value != ".." && !strings.HasPrefix(value, "../") &&
		!strings.ContainsAny(value, `\:`)
}

func validText(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 4096 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func callLess(left, right Call) bool {
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
}

func gapLess(left, right Gap) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	return left.Code < right.Code
}
