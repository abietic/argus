package contextprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/reviewconfig"
)

func TestLocalAdapterArtifactsMatchInvocationDefinitions(t *testing.T) {
	definitions, err := application.InvocationContextProviderDefinitions([]string{
		"repository_search", "go_ast", "go_dependencies", "go_compile",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		content, err := LocalAdapterArtifact(definition)
		if err != nil {
			t.Fatalf("LocalAdapterArtifact(%s) error = %v", definition.ID, err)
		}
		digest := sha256.Sum256(content)
		if got := hex.EncodeToString(digest[:]); got != definition.Adapter.SHA256 {
			t.Fatalf("LocalAdapterArtifact(%s) digest = %s, want %s", definition.ID, got, definition.Adapter.SHA256)
		}
	}
}

func TestLocalAdapterArtifactRejectsUnpublishedOrTamperedIdentity(t *testing.T) {
	for _, definition := range []reviewconfig.ContextProviderDefinition{
		{
			ID: "custom", Revision: "1", Kind: "go_ast",
			Adapter: reviewconfig.VersionedRef{
				ID: "custom-adapter", Revision: "1",
				SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
		{
			ID: "go-ast", Revision: "1", Kind: "go_ast",
			Adapter: reviewconfig.VersionedRef{
				ID: "argus-go-ast", Revision: "2",
				SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
	} {
		if _, err := LocalAdapterArtifact(definition); err == nil {
			t.Fatalf("LocalAdapterArtifact(%+v) admitted an unpublished identity", definition)
		}
	}
}

func TestAdapterDigestCompatibilityAcceptsOnlyCurrentOrLegacy(t *testing.T) {
	if !adapterDigestMatches("current", "current", "legacy") ||
		!adapterDigestMatches("legacy", "current", "legacy") ||
		adapterDigestMatches("unknown", "current", "legacy") {
		t.Fatal("adapter digest compatibility boundary changed")
	}
}
