package training

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"argus.local/argus/internal/runmodel"
)

const (
	StrictRedactionPolicySchemaVersion = "argus.training_strict_redaction_policy.v1alpha1"
	StrictRedactionPolicyID            = "argus-strict-text"
	StrictRedactionPolicyRevision      = "1"
	RedactionReplacement               = "[ARGUS_REDACTED]"
	maximumRedactionArtifactBytes      = int64(1 << 20)
)

var strictRuleIDs = []string{
	"assignment_secret",
	"aws_access_key",
	"bearer_credential",
	"email_address",
	"github_token",
	"ip_address",
	"jwt",
	"openai_anthropic_key",
	"pem_private_key",
	"slack_token",
	"url_userinfo",
	"user_home_path",
}

type StrictRedactionPolicy struct {
	SchemaVersion    string   `json:"schema_version"`
	ID               string   `json:"id"`
	Revision         string   `json:"revision"`
	RuleIDs          []string `json:"rule_ids"`
	Replacement      string   `json:"replacement"`
	MaxArtifactBytes int64    `json:"max_artifact_bytes"`
	SHA256           string   `json:"sha256"`
}

type RedactionHit struct {
	RuleID string `json:"rule_id"`
	Count  uint32 `json:"count"`
}

type RedactionResult struct {
	Content []byte         `json:"-"`
	Hits    []RedactionHit `json:"hits"`
}

func DefaultStrictRedactionPolicy() StrictRedactionPolicy {
	policy := StrictRedactionPolicy{SchemaVersion: StrictRedactionPolicySchemaVersion, ID: StrictRedactionPolicyID, Revision: StrictRedactionPolicyRevision, RuleIDs: slices.Clone(strictRuleIDs), Replacement: RedactionReplacement, MaxArtifactBytes: maximumRedactionArtifactBytes}
	digest, err := digestStrictRedactionPolicy(policy)
	if err != nil {
		panic(err)
	}
	policy.SHA256 = digest
	return policy
}

func DecodeStrictRedactionPolicy(data []byte) (StrictRedactionPolicy, error) {
	var policy StrictRedactionPolicy
	if err := decodeStrict(data, &policy); err != nil {
		return policy, fmt.Errorf("decode strict redaction policy: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return policy, fmt.Errorf("validate strict redaction policy: %w", err)
	}
	return policy, nil
}

func (policy StrictRedactionPolicy) Validate() error {
	if policy.SchemaVersion != StrictRedactionPolicySchemaVersion || policy.ID != StrictRedactionPolicyID || policy.Revision != StrictRedactionPolicyRevision {
		return fmt.Errorf("unsupported strict redaction policy identity")
	}
	if !slices.Equal(policy.RuleIDs, strictRuleIDs) || policy.Replacement != RedactionReplacement || policy.MaxArtifactBytes != maximumRedactionArtifactBytes {
		return fmt.Errorf("strict redaction policy parameters are not the closed built-in policy")
	}
	digest, err := digestStrictRedactionPolicy(policy)
	if err != nil {
		return err
	}
	if policy.SHA256 != digest {
		return fmt.Errorf("strict redaction policy SHA-256 does not match content")
	}
	return nil
}

func digestStrictRedactionPolicy(policy StrictRedactionPolicy) (string, error) {
	copy := policy
	copy.SHA256 = ""
	return runmodel.DigestJSON(copy)
}

type redactionRule struct {
	id    string
	apply func(string) (string, uint32)
}

var strictRedactionRules = []redactionRule{
	{id: "pem_private_key", apply: replacePattern(regexp.MustCompile(`(?s)-----BEGIN[ ]+(?:RSA[ ]+|EC[ ]+|OPENSSH[ ]+)?PRIVATE[ ]+KEY-----.*?-----END[ ]+(?:RSA[ ]+|EC[ ]+|OPENSSH[ ]+)?PRIVATE[ ]+KEY-----`), func(string) string { return RedactionReplacement })},
	{id: "assignment_secret", apply: replacePattern(regexp.MustCompile(`(?i)(api[_-]?key|client[_-]?secret|password|passwd|private[_-]?key|secret|token)([ \t]*[:=][ \t]*)([^\s,;]+)`), func(match string) string {
		if strings.Contains(match, RedactionReplacement) {
			return match
		}
		indices := regexp.MustCompile(`(?i)(api[_-]?key|client[_-]?secret|password|passwd|private[_-]?key|secret|token)([ \t]*[:=][ \t]*)`).FindStringIndex(match)
		if indices == nil {
			return RedactionReplacement
		}
		return match[:indices[1]] + RedactionReplacement
	})},
	{id: "bearer_credential", apply: replacePattern(regexp.MustCompile(`(?i)Bearer[ \t]+[A-Za-z0-9._~+/=-]{8,}`), func(match string) string {
		if strings.Contains(match, RedactionReplacement) {
			return match
		}
		return "Bearer " + RedactionReplacement
	})},
	{id: "url_userinfo", apply: replacePattern(regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]+:[^\s/@]+@`), func(match string) string {
		separator := strings.Index(match, "://")
		if separator < 0 {
			return RedactionReplacement
		}
		return match[:separator+3] + RedactionReplacement + "@"
	})},
	{id: "openai_anthropic_key", apply: replacePattern(regexp.MustCompile(`(?:sk-ant-[A-Za-z0-9_-]{12,}|sk-[A-Za-z0-9_-]{16,})`), func(string) string { return RedactionReplacement })},
	{id: "github_token", apply: replacePattern(regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), func(string) string { return RedactionReplacement })},
	{id: "slack_token", apply: replacePattern(regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), func(string) string { return RedactionReplacement })},
	{id: "aws_access_key", apply: replacePattern(regexp.MustCompile(`(?:AKIA|ASIA)[A-Z0-9]{16}`), func(string) string { return RedactionReplacement })},
	{id: "jwt", apply: replacePattern(regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), func(string) string { return RedactionReplacement })},
	{id: "email_address", apply: replacePattern(regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_`+"`"+`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+`), func(string) string { return "[ARGUS_REDACTED_EMAIL]" })},
	{id: "ip_address", apply: replacePattern(regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`), func(string) string { return "[ARGUS_REDACTED_IP]" })},
	{id: "user_home_path", apply: replacePattern(regexp.MustCompile(`(?:/(?:Users|home)/[^/\s]+/|[A-Za-z]:\\Users\\[^\\\s]+\\)`), func(match string) string {
		if strings.Contains(match, "\\") {
			return `C:\Users\[ARGUS_REDACTED_USER]\`
		}
		if strings.HasPrefix(match, "/home/") {
			return "/home/[ARGUS_REDACTED_USER]/"
		}
		return "/Users/[ARGUS_REDACTED_USER]/"
	})},
}

func RedactStrictText(content []byte, policy StrictRedactionPolicy) (RedactionResult, error) {
	if err := policy.Validate(); err != nil {
		return RedactionResult{}, err
	}
	if int64(len(content)) > policy.MaxArtifactBytes {
		return RedactionResult{}, fmt.Errorf("text artifact exceeds strict redaction limit")
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return RedactionResult{}, fmt.Errorf("strict redaction accepts UTF-8 text without NUL bytes only")
	}
	value := string(content)
	counts := map[string]uint32{}
	for _, rule := range strictRedactionRules {
		var count uint32
		value, count = rule.apply(value)
		if count > 0 {
			counts[rule.id] += count
		}
	}
	// Reapply the exact closed detector set. A second material change proves a
	// sensitive token survived the first pass and therefore fails closed.
	probe := value
	for _, rule := range strictRedactionRules {
		next, count := rule.apply(probe)
		if count > 0 {
			return RedactionResult{}, fmt.Errorf("strict redaction residual matched rule %q", rule.id)
		}
		probe = next
	}
	hits := make([]RedactionHit, 0, len(counts))
	for _, id := range strictRuleIDs {
		if count := counts[id]; count > 0 {
			hits = append(hits, RedactionHit{RuleID: id, Count: count})
		}
	}
	return RedactionResult{Content: []byte(value), Hits: hits}, nil
}

func replacePattern(pattern *regexp.Regexp, replacement func(string) string) func(string) (string, uint32) {
	return func(value string) (string, uint32) {
		var count uint32
		result := pattern.ReplaceAllStringFunc(value, func(match string) string {
			replaced := replacement(match)
			if replaced != match {
				count++
			}
			return replaced
		})
		return result, count
	}
}
