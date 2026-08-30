package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const ReviewSpecSchemaVersion = "argus.review_spec.v1alpha1"

var platformArtifactURI = regexp.MustCompile(
	`^artifact://[A-Za-z0-9]([A-Za-z0-9.-]{0,126}[A-Za-z0-9])?/[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*$`,
)

type ReviewMode string

const (
	ReviewModeDiff      ReviewMode = "diff"
	ReviewModeSelection ReviewMode = "selection"
	ReviewModeScope     ReviewMode = "scope"
)

type RepositoryRef struct {
	Provider     string `json:"provider"`
	RepositoryID string `json:"repository_id"`
}

type ContentRef struct {
	URI       string `json:"uri"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type VersionedRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

type DiffTarget struct {
	BaseRevision string     `json:"base_revision"`
	HeadRevision string     `json:"head_revision"`
	Patch        ContentRef `json:"patch"`
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

type SelectionTarget struct {
	Revision string `json:"revision"`
	Path     string `json:"path"`

	// StartLine/EndLine retain the original single-range wire shape. New
	// callers should use Ranges, or Symbol when the frozen source can resolve a
	// stable language symbol. Exactly one selector form is allowed.
	StartLine uint32           `json:"start_line,omitempty"`
	EndLine   uint32           `json:"end_line,omitempty"`
	Ranges    []SelectionRange `json:"ranges,omitempty"`
	Symbol    *SymbolSelector  `json:"symbol,omitempty"`
	Content   ContentRef       `json:"content"`
}

type ScopeTarget struct {
	Revision string   `json:"revision"`
	Include  []string `json:"include"`
	Exclude  []string `json:"exclude"`
}

// ReviewTarget is a discriminated union. Exactly one payload matching Mode is
// required.
type ReviewTarget struct {
	Mode      ReviewMode       `json:"mode"`
	Diff      *DiffTarget      `json:"diff,omitempty"`
	Selection *SelectionTarget `json:"selection,omitempty"`
	Scope     *ScopeTarget     `json:"scope,omitempty"`
}

type ReviewSpec struct {
	SchemaVersion   string        `json:"schema_version"`
	RequestID       string        `json:"request_id"`
	IdempotencyKey  string        `json:"idempotency_key"`
	TenantID        string        `json:"tenant_id"`
	WorkspaceID     string        `json:"workspace_id"`
	Repository      RepositoryRef `json:"repository"`
	Target          ReviewTarget  `json:"target"`
	ConfigBundleRef VersionedRef  `json:"config_bundle_ref"`
	WorkflowRef     VersionedRef  `json:"workflow_ref"`
	RequestedOutput []string      `json:"requested_outputs"`
	RemoteWrites    string        `json:"remote_writes"`
}

func DecodeReviewSpec(data []byte) (ReviewSpec, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return ReviewSpec{}, err
	}
	var spec ReviewSpec
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return ReviewSpec{}, fmt.Errorf("decode ReviewSpec: %w", err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return ReviewSpec{}, err
	}
	if err := spec.Validate(); err != nil {
		return ReviewSpec{}, err
	}
	return spec, nil
}

func (spec ReviewSpec) Validate() error {
	if spec.SchemaVersion != ReviewSpecSchemaVersion {
		return fmt.Errorf("unsupported ReviewSpec schema %q", spec.SchemaVersion)
	}
	for name, value := range map[string]string{
		"request_id": spec.RequestID, "idempotency_key": spec.IdempotencyKey,
		"tenant_id": spec.TenantID, "workspace_id": spec.WorkspaceID,
		"repository.provider":      spec.Repository.Provider,
		"repository.repository_id": spec.Repository.RepositoryID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := spec.ConfigBundleRef.validate("config_bundle_ref"); err != nil {
		return err
	}
	if err := spec.WorkflowRef.validate("workflow_ref"); err != nil {
		return err
	}
	if err := spec.Target.validate(); err != nil {
		return err
	}
	if spec.RemoteWrites != "deny" {
		return fmt.Errorf("remote_writes must be %q in v1alpha1", "deny")
	}
	if len(spec.RequestedOutput) == 0 {
		return fmt.Errorf("requested_outputs must not be empty")
	}
	seen := make(map[string]struct{}, len(spec.RequestedOutput))
	for index, output := range spec.RequestedOutput {
		switch output {
		case "report", "findings", "trace":
		default:
			return fmt.Errorf("requested_outputs[%d] has unsupported value %q", index, output)
		}
		if _, ok := seen[output]; ok {
			return fmt.Errorf("requested_outputs contains duplicate %q", output)
		}
		seen[output] = struct{}{}
	}
	return nil
}

func DigestReviewSpec(spec ReviewSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal ReviewSpec: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (target ReviewTarget) validate() error {
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
		return fmt.Errorf("target must contain exactly one mode payload")
	}
	switch target.Mode {
	case ReviewModeDiff:
		if target.Diff == nil {
			return fmt.Errorf("target.diff is required for mode %q", target.Mode)
		}
		return target.Diff.validate()
	case ReviewModeSelection:
		if target.Selection == nil {
			return fmt.Errorf("target.selection is required for mode %q", target.Mode)
		}
		return target.Selection.validate()
	case ReviewModeScope:
		if target.Scope == nil {
			return fmt.Errorf("target.scope is required for mode %q", target.Mode)
		}
		return target.Scope.validate()
	default:
		return fmt.Errorf("unsupported review target mode %q", target.Mode)
	}
}

func (target DiffTarget) validate() error {
	if err := requireIdentifier("target.diff.base_revision", target.BaseRevision); err != nil {
		return err
	}
	if err := requireIdentifier("target.diff.head_revision", target.HeadRevision); err != nil {
		return err
	}
	if target.BaseRevision == target.HeadRevision {
		return fmt.Errorf("target.diff base_revision and head_revision must differ")
	}
	return target.Patch.validate("target.diff.patch", true)
}

func (target SelectionTarget) validate() error {
	if err := requireIdentifier("target.selection.revision", target.Revision); err != nil {
		return err
	}
	if err := requireRepositoryPath("target.selection.path", target.Path); err != nil {
		return err
	}
	selectorForms := 0
	if target.StartLine != 0 || target.EndLine != 0 {
		selectorForms++
		if target.StartLine == 0 || target.EndLine < target.StartLine {
			return fmt.Errorf("target.selection legacy line range is invalid")
		}
	}
	if len(target.Ranges) > 0 {
		selectorForms++
		if err := validateSelectionRanges("target.selection.ranges", target.Ranges); err != nil {
			return err
		}
	} else if target.Ranges != nil {
		return fmt.Errorf("target.selection.ranges must not be an empty array")
	}
	if target.Symbol != nil {
		selectorForms++
		if err := target.Symbol.validate("target.selection.symbol"); err != nil {
			return err
		}
	}
	if selectorForms != 1 {
		return fmt.Errorf("target.selection must contain exactly one selector form")
	}
	return target.Content.validate("target.selection.content", true)
}

// EffectiveRanges returns the exact caller-authorized ranges when the selector
// is range based. Symbol selectors are resolved against the frozen revision by
// the target materializer and therefore return nil here.
func (target SelectionTarget) EffectiveRanges() []SelectionRange {
	if target.StartLine != 0 || target.EndLine != 0 {
		return []SelectionRange{{StartLine: target.StartLine, EndLine: target.EndLine}}
	}
	return append([]SelectionRange(nil), target.Ranges...)
}

func validateSelectionRanges(name string, ranges []SelectionRange) error {
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

func (selector SymbolSelector) validate(name string) error {
	if selector.Language != "go" {
		return fmt.Errorf("%s.language must be %q in v1alpha1", name, "go")
	}
	switch selector.Kind {
	case "function", "method", "type":
	default:
		return fmt.Errorf("%s.kind is unsupported", name)
	}
	if err := requireIdentifier(name+".qualified_name", selector.QualifiedName); err != nil {
		return err
	}
	parts := strings.Split(selector.QualifiedName, ".")
	expectedParts := 1
	if selector.Kind == "method" {
		expectedParts = 2
	}
	if len(parts) != expectedParts {
		return fmt.Errorf("%s.qualified_name does not match symbol kind %q", name, selector.Kind)
	}
	for _, part := range parts {
		if !portableSymbolIdentifier(part) {
			return fmt.Errorf("%s.qualified_name must use portable identifier segments", name)
		}
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

func (target ScopeTarget) validate() error {
	if err := requireIdentifier("target.scope.revision", target.Revision); err != nil {
		return err
	}
	if len(target.Include) == 0 {
		return fmt.Errorf("target.scope.include must not be empty")
	}
	if target.Exclude == nil {
		return fmt.Errorf("target.scope.exclude must be an explicit array")
	}
	seen := make(map[string]struct{}, len(target.Include)+len(target.Exclude))
	for name, patterns := range map[string][]string{
		"target.scope.include": target.Include,
		"target.scope.exclude": target.Exclude,
	} {
		for index, pattern := range patterns {
			if err := requireSafePattern(fmt.Sprintf("%s[%d]", name, index), pattern); err != nil {
				return err
			}
			key := name + "\x00" + pattern
			if _, ok := seen[key]; ok {
				return fmt.Errorf("%s contains duplicate %q", name, pattern)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func (ref ContentRef) validate(name string, nonEmpty bool) error {
	if !platformArtifactURI.MatchString(ref.URI) {
		return fmt.Errorf("%s.uri must be a platform-issued artifact URI", name)
	}
	artifactPath := strings.SplitN(strings.TrimPrefix(ref.URI, "artifact://"), "/", 2)[1]
	for _, segment := range strings.Split(artifactPath, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("%s.uri must not traverse artifact namespaces", name)
		}
	}
	if err := requireSHA256(name+".sha256", ref.SHA256); err != nil {
		return err
	}
	if ref.SizeBytes < 0 || nonEmpty && ref.SizeBytes == 0 {
		return fmt.Errorf("%s.size_bytes is invalid", name)
	}
	return nil
}

func (ref VersionedRef) validate(name string) error {
	if err := requireIdentifier(name+".id", ref.ID); err != nil {
		return err
	}
	if err := requireIdentifier(name+".revision", ref.Revision); err != nil {
		return err
	}
	return requireSHA256(name+".sha256", ref.SHA256)
}

func requireIdentifier(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be a non-empty trimmed string of at most 256 bytes", name)
	}
	if containsControl(value) {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}

func requireSHA256(name, value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256 hex digest", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%s must be a lowercase SHA-256 hex digest", name)
	}
	return nil
}

func requireRepositoryPath(name, value string) error {
	if value == "" || !utf8.ValidString(value) || containsControl(value) ||
		strings.ContainsAny(value, `\:`) || value != path.Clean(value) ||
		path.IsAbs(value) || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%s must be a clean repository-relative path", name)
	}
	return nil
}

func requireSafePattern(name, value string) error {
	if value == "" || len(value) > 1024 ||
		value != strings.TrimSpace(value) || !utf8.ValidString(value) ||
		containsControl(value) || strings.ContainsAny(value, `\:?[]`) ||
		strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s must be a non-empty repository-relative pattern", name)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." ||
			strings.Contains(segment, "***") {
			return fmt.Errorf("%s must not traverse parent directories", name)
		}
	}
	return nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing ReviewSpec data: %w", err)
	}
	return fmt.Errorf("ReviewSpec contains multiple JSON values")
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(decoder, "$"); err != nil {
		return fmt.Errorf("decode ReviewSpec structure: %w", err)
	}
	if _, err := decoder.Token(); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing ReviewSpec data: %w", err)
	}
	return fmt.Errorf("ReviewSpec contains multiple JSON values")
}

func walkJSONValue(decoder *json.Decoder, location string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s contains a non-string object key", location)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%s contains duplicate field %q", location, key)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder, location+"."+key); err != nil {
				return err
			}
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil {
			return closingErr
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("%s object is not closed", location)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
			index++
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil {
			return closingErr
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("%s array is not closed", location)
		}
	default:
		return fmt.Errorf("%s starts with unexpected delimiter %q", location, delimiter)
	}
	return nil
}
