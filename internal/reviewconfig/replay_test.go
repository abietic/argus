package reviewconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestDiffReplayIndexPolicyReturnsExactProviderFields(t *testing.T) {
	baseline := ExecutionPolicy{
		AgentProfile: VersionedRef{ID: "agent", Revision: "1", SHA256: strings.Repeat("a", 64)},
		ModelProfile: VersionedRef{ID: "model", Revision: "1", SHA256: strings.Repeat("b", 64)},
		AllowedTools: []string{}, ContextProviderMaxConcurrency: 2,
		ContextProviders: []ContextProviderDefinition{{
			ID: "ast", Revision: "1", Kind: "go_ast",
			Adapter: VersionedRef{ID: "go-ast", Revision: "1", SHA256: strings.Repeat("c", 64)},
		}},
	}
	variant := baseline
	variant.ContextProviders = []ContextProviderDefinition{{
		ID: "ast", Revision: "2", Kind: "go_ast",
		Adapter: VersionedRef{ID: "go-ast", Revision: "2", SHA256: strings.Repeat("d", 64)},
	}, {
		ID: "search", Revision: "1", Kind: "repository_search",
		Adapter: VersionedRef{ID: "search", Revision: "1", SHA256: strings.Repeat("e", 64)},
	}}
	fields, err := DiffReplayIndexPolicy(baseline, variant)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"execution.context_providers.ast.adapter.revision",
		"execution.context_providers.ast.adapter.sha256",
		"execution.context_providers.ast.revision",
		"execution.context_providers.order",
		"execution.context_providers.search.added",
	}
	if !slices.Equal(fields, want) {
		t.Fatalf("fields = %v, want %v", fields, want)
	}
}

func TestDiffReplayIndexPolicyRejectsUnchangedAndOtherExecutionChanges(t *testing.T) {
	baseline := ExecutionPolicy{
		AgentProfile: VersionedRef{ID: "agent", Revision: "1", SHA256: strings.Repeat("a", 64)},
		ModelProfile: VersionedRef{ID: "model", Revision: "1", SHA256: strings.Repeat("b", 64)},
		AllowedTools: []string{}, ContextProviders: []ContextProviderDefinition{},
		ContextProviderMaxConcurrency: 1,
	}
	if _, err := DiffReplayIndexPolicy(baseline, baseline); err == nil ||
		!strings.Contains(err.Error(), "does not change") {
		t.Fatalf("unchanged error = %v", err)
	}
	variant := baseline
	variant.ContextProviderMaxConcurrency = 2
	variant.ContextProviders = []ContextProviderDefinition{{
		ID: "search", Revision: "1", Kind: "repository_search",
		Adapter: VersionedRef{ID: "search", Revision: "1", SHA256: strings.Repeat("c", 64)},
	}}
	if _, err := DiffReplayIndexPolicy(baseline, variant); err == nil ||
		!strings.Contains(err.Error(), "concurrency policy must remain frozen") {
		t.Fatalf("concurrency error = %v", err)
	}
}
