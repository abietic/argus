package reviewcore

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
)

const (
	RuleUnfinishedWork    = "unfinished-work"
	RuleExplicitBugMarker = "explicit-bug-marker"
)

// RuntimeRule is the executable subset of a versioned rule definition. The
// application layer is responsible for freezing the source configuration; the
// deterministic core accepts only the fields that can change its behavior.
type RuntimeRule struct {
	RuleID           string
	DetectorID       string
	DetectorRevision string
	Enabled          bool
	Severity         string
	Languages        []string
	PathPrefixes     []string
}

// RuntimePolicy is the complete policy required by the deterministic baseline.
// Rules and their nested sets must be explicit so an omitted configuration
// cannot silently fall back to broader behavior.
type RuntimePolicy struct {
	Rules                      []RuntimeRule
	RequiredEvidenceKinds      []EvidenceKind
	MinimumIndependentEvidence int
	AllowPartialEvidence       bool
	TargetComplete             bool
	MinimumSeverity            string
	HumanReviewInconclusive    bool
}

// DefaultRuntimePolicy preserves the behavior of Run, ExecuteStage, Detect,
// Normalize, and Adjudicate. It returns fresh slices so callers cannot mutate a
// process-global default.
func DefaultRuntimePolicy() RuntimePolicy {
	return RuntimePolicy{
		Rules: []RuntimeRule{
			{
				RuleID: RuleExplicitBugMarker, DetectorID: detectorID,
				DetectorRevision: detectorRevision, Enabled: true, Severity: "high",
				Languages: []string{"go"}, PathPrefixes: []string{},
			},
			{
				RuleID: RuleUnfinishedWork, DetectorID: detectorID,
				DetectorRevision: detectorRevision, Enabled: true, Severity: "info",
				Languages: []string{"go"}, PathPrefixes: []string{},
			},
			{
				RuleID: RuleGoContextCancelDiscarded, DetectorID: DetectorGoASTID,
				DetectorRevision: DetectorGoASTRevision, Enabled: true, Severity: "high",
				Languages: []string{"go"}, PathPrefixes: []string{},
			},
		},
		RequiredEvidenceKinds: []EvidenceKind{
			EvidenceFileContent,
			EvidencePatchLine,
			EvidenceTargetLine,
		},
		MinimumIndependentEvidence: 1,
		AllowPartialEvidence:       false,
		TargetComplete:             true,
		MinimumSeverity:            "info",
		HumanReviewInconclusive:    true,
	}
}

func (policy RuntimePolicy) Validate() error {
	if policy.Rules == nil {
		return fmt.Errorf("runtime policy rules must be an explicit array")
	}
	if !validSeverity(policy.MinimumSeverity) {
		return fmt.Errorf("runtime policy minimum severity %q is unsupported", policy.MinimumSeverity)
	}
	if policy.RequiredEvidenceKinds == nil || len(policy.RequiredEvidenceKinds) == 0 {
		return fmt.Errorf(
			"runtime policy required evidence kinds must be an explicit non-empty sorted array",
		)
	}
	if !slices.IsSorted(policy.RequiredEvidenceKinds) {
		return fmt.Errorf("runtime policy required evidence kinds must be sorted")
	}
	for index, kind := range policy.RequiredEvidenceKinds {
		if !knownEvidenceKind(kind) {
			return fmt.Errorf("runtime policy evidence kind %q is unsupported", kind)
		}
		if index > 0 && kind == policy.RequiredEvidenceKinds[index-1] {
			return fmt.Errorf("runtime policy required evidence kinds contain duplicate %q", kind)
		}
	}
	if policy.MinimumIndependentEvidence <= 0 {
		return fmt.Errorf("runtime policy minimum independent evidence must be positive")
	}
	seen := make(map[string]struct{}, len(policy.Rules))
	for index, rule := range policy.Rules {
		if err := rule.validate(index); err != nil {
			return err
		}
		if _, duplicate := seen[rule.RuleID]; duplicate {
			return fmt.Errorf("runtime policy contains duplicate rule %q", rule.RuleID)
		}
		seen[rule.RuleID] = struct{}{}
		if rule.Enabled && !supportedRuntimeRule(rule.RuleID) {
			return fmt.Errorf("enabled runtime rule %q is unsupported", rule.RuleID)
		}
		descriptor, registered := LookupRuleDescriptor(rule.RuleID)
		if rule.Enabled && (!registered ||
			rule.DetectorID != descriptor.DetectorID ||
			rule.DetectorRevision != descriptor.DetectorRevision) {
			return fmt.Errorf(
				"enabled runtime rule %q uses unsupported detector %q@%q",
				rule.RuleID,
				rule.DetectorID,
				rule.DetectorRevision,
			)
		}
	}
	return nil
}

func (rule RuntimeRule) validate(index int) error {
	name := fmt.Sprintf("runtime policy rule %d", index)
	if err := validateRuntimeToken(rule.RuleID); err != nil {
		return fmt.Errorf("%s id: %w", name, err)
	}
	if err := validateRuntimeToken(rule.DetectorID); err != nil {
		return fmt.Errorf("%s detector id: %w", name, err)
	}
	if err := validateRuntimeToken(rule.DetectorRevision); err != nil {
		return fmt.Errorf("%s detector revision: %w", name, err)
	}
	if !validSeverity(rule.Severity) {
		return fmt.Errorf("%s severity %q is unsupported", name, rule.Severity)
	}
	if rule.Languages == nil || len(rule.Languages) == 0 {
		return fmt.Errorf("%s languages must be an explicit non-empty sorted array", name)
	}
	if !slices.IsSorted(rule.Languages) {
		return fmt.Errorf("%s languages must be sorted", name)
	}
	for languageIndex, language := range rule.Languages {
		if err := validateRuntimeToken(language); err != nil {
			return fmt.Errorf("%s language %d: %w", name, languageIndex, err)
		}
		if languageIndex > 0 && language == rule.Languages[languageIndex-1] {
			return fmt.Errorf("%s languages contain duplicate %q", name, language)
		}
	}
	if rule.PathPrefixes == nil {
		return fmt.Errorf("%s path prefixes must be an explicit sorted array", name)
	}
	if !slices.IsSorted(rule.PathPrefixes) {
		return fmt.Errorf("%s path prefixes must be sorted", name)
	}
	for prefixIndex, prefix := range rule.PathPrefixes {
		if prefix != "" {
			if err := validateRepositoryPath("runtime rule path prefix", prefix); err != nil {
				return fmt.Errorf("%s path prefix %d: %w", name, prefixIndex, err)
			}
			if strings.ContainsAny(prefix, "*?[") || path.Clean(prefix) != prefix {
				return fmt.Errorf("%s path prefix %q must be literal", name, prefix)
			}
		}
		if prefixIndex > 0 && prefix == rule.PathPrefixes[prefixIndex-1] {
			return fmt.Errorf("%s path prefixes contain duplicate %q", name, prefix)
		}
	}
	return nil
}

func validateRuntimeToken(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
		return fmt.Errorf("%q must be a non-empty safe identifier", value)
	}
	for _, character := range value {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) &&
			!strings.ContainsRune("._~-", character) {
			return fmt.Errorf("%q must be a non-empty safe identifier", value)
		}
	}
	return nil
}

func validSeverity(value string) bool {
	switch value {
	case "info", "low", "medium", "high", "critical":
		return true
	default:
		return false
	}
}

func supportedRuntimeRule(ruleID string) bool {
	_, registered := LookupRuleDescriptor(ruleID)
	return registered
}

func knownEvidenceKind(kind EvidenceKind) bool {
	switch kind {
	case EvidencePatchLine, EvidenceTargetLine, EvidenceFileContent:
		return true
	default:
		return false
	}
}

func runtimeRuleFor(policy RuntimePolicy, ruleID string) (RuntimeRule, bool) {
	for _, rule := range policy.Rules {
		if rule.RuleID == ruleID {
			return rule, true
		}
	}
	return RuntimeRule{}, false
}

func runtimeRuleApplies(rule RuntimeRule, repositoryPath string) bool {
	if !rule.Enabled || languageForPath(repositoryPath) != "go" ||
		!slices.Contains(rule.Languages, "go") {
		return false
	}
	if len(rule.PathPrefixes) == 0 {
		return true
	}
	for _, prefix := range rule.PathPrefixes {
		if prefix == "" || repositoryPath == prefix ||
			strings.HasPrefix(repositoryPath, prefix+"/") {
			return true
		}
	}
	return false
}

func languageForPath(repositoryPath string) string {
	if strings.HasSuffix(repositoryPath, ".go") {
		return "go"
	}
	return ""
}

func severityAtLeast(actual, minimum string) bool {
	return severityRank(actual) >= severityRank(minimum)
}

func severityRank(value string) int {
	switch value {
	case "info":
		return 0
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "critical":
		return 4
	default:
		return -1
	}
}

func hasMinimumIndependentEvidence(finding Finding, policy RuntimePolicy) bool {
	accepted := make(map[EvidenceKind]struct{}, len(policy.RequiredEvidenceKinds))
	for _, kind := range policy.RequiredEvidenceKinds {
		accepted[kind] = struct{}{}
	}
	type evidenceSource struct {
		Kind         EvidenceKind
		SourceDigest string
	}
	independent := make(map[evidenceSource]struct{}, len(finding.Evidence))
	for _, evidence := range finding.Evidence {
		if _, recognized := accepted[evidence.Kind]; !recognized {
			continue
		}
		independent[evidenceSource{
			Kind: evidence.Kind, SourceDigest: evidence.SourceDigest,
		}] = struct{}{}
	}
	return len(independent) >= policy.MinimumIndependentEvidence
}
