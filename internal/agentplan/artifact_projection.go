package agentplan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// GovernedArtifactProjection binds one exact local run artifact to its
// execution-facing, tenant/workspace-scoped governed alias. The two references
// identify the same immutable bytes and contract; only their publication
// namespaces differ.
//
// Local remains part of the run repository's frozen record closure. Governed
// is the only reference that may enter an AgentStagePlan execution closure.
type GovernedArtifactProjection struct {
	Local    runmodel.ArtifactRef              `json:"local"`
	Governed contractsv1alpha1.ArtifactBinding `json:"governed"`
}

// Validate fails closed unless Local is a strict runmodel content-addressed
// reference, Governed is a canonical authority/tenant/workspace/object URI,
// and both sides bind the exact same digest, size, and contract. Syntax and
// content equality do not prove publication or current authorization; the
// application layer must resolve Governed against the authenticated subject.
func (projection GovernedArtifactProjection) Validate() error {
	_, err := validateGovernedArtifactProjection("governed artifact projection", projection)
	return err
}

type governedArtifactNamespace struct {
	authority   string
	tenantID    string
	workspaceID string
}

func validateGovernedArtifactProjection(
	name string,
	projection GovernedArtifactProjection,
) (governedArtifactNamespace, error) {
	if err := projection.Local.Validate(); err != nil {
		return governedArtifactNamespace{}, fmt.Errorf("%s local ref: %w", name, err)
	}
	namespace, err := validateGovernedArtifactBinding(name+" governed binding", projection.Governed)
	if err != nil {
		return governedArtifactNamespace{}, err
	}
	if projection.Governed.Ref.SHA256 != projection.Local.SHA256 ||
		projection.Governed.Ref.SizeBytes != projection.Local.SizeBytes ||
		projection.Governed.Contract != projection.Local.Contract {
		return governedArtifactNamespace{}, fmt.Errorf(
			"%s local and governed refs must bind the exact same digest, size, and contract",
			name,
		)
	}
	return namespace, nil
}

func validateGovernedArtifactBinding(
	name string,
	binding contractsv1alpha1.ArtifactBinding,
) (governedArtifactNamespace, error) {
	if !validProjectionText(binding.Contract, 256) {
		return governedArtifactNamespace{}, fmt.Errorf("%s contract is invalid", name)
	}
	if !lowerSHA256(binding.Ref.SHA256) {
		return governedArtifactNamespace{}, fmt.Errorf(
			"%s sha256 must be a lowercase SHA-256 digest",
			name,
		)
	}
	if binding.Ref.SizeBytes <= 0 {
		return governedArtifactNamespace{}, fmt.Errorf("%s size_bytes must be positive", name)
	}
	if binding.Ref.URI == "" || len(binding.Ref.URI) > 1024 ||
		binding.Ref.URI != strings.TrimSpace(binding.Ref.URI) ||
		!utf8.ValidString(binding.Ref.URI) {
		return governedArtifactNamespace{}, fmt.Errorf(
			"%s URI must be a canonical governed artifact URI",
			name,
		)
	}

	parsed, err := url.Parse(binding.Ref.URI)
	if err != nil {
		return governedArtifactNamespace{}, fmt.Errorf("%s parse URI: %w", name, err)
	}
	if parsed.Scheme != "artifact" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.ForceQuery || parsed.RawPath != "" {
		return governedArtifactNamespace{}, fmt.Errorf(
			"%s URI must be a canonical governed artifact URI",
			name,
		)
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 6 || parts[0] != "tenants" || parts[2] != "workspaces" ||
		parts[4] != "objects" {
		return governedArtifactNamespace{}, fmt.Errorf(
			"%s URI must use tenants/<tenant>/workspaces/<workspace>/objects/<object> namespace",
			name,
		)
	}
	namespace := governedArtifactNamespace{
		authority: parsed.Host, tenantID: parts[1], workspaceID: parts[3],
	}
	if !validProjectionAuthority(namespace.authority) ||
		namespace.authority != strings.ToLower(namespace.authority) ||
		!validProjectionID(namespace.tenantID) ||
		!validProjectionID(namespace.workspaceID) ||
		!lowerSHA256(parts[5]) {
		return governedArtifactNamespace{}, fmt.Errorf(
			"%s URI contains an invalid authority, tenant, workspace, or object identity",
			name,
		)
	}
	canonicalPath := "/tenants/" + namespace.tenantID +
		"/workspaces/" + namespace.workspaceID + "/objects/" + parts[5]
	canonicalURI := "artifact://" + namespace.authority + canonicalPath
	if parsed.Path != canonicalPath || parsed.EscapedPath() != canonicalPath ||
		binding.Ref.URI != canonicalURI {
		return governedArtifactNamespace{}, fmt.Errorf(
			"%s URI must be canonical",
			name,
		)
	}
	return namespace, nil
}

func validProjectionAuthority(value string) bool {
	if !validProjectionID(value) {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' {
			continue
		}
		if (character == '.' || character == '-') && index > 0 && index < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

func validProjectionID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." ||
		value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("._~-", character) {
			continue
		}
		return false
	}
	return true
}

func validProjectionText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func lowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}
