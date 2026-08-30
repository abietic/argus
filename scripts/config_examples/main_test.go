package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormativeExamplesMatchCurrentConstructors(t *testing.T) {
	t.Parallel()
	revision, bundle, agentRevision, agentBundle, err := exampleValues()
	if err != nil {
		t.Fatalf("exampleValues() error = %v", err)
	}
	receipt, err := exampleResolutionReceipt(agentRevision, agentBundle)
	if err != nil {
		t.Fatalf("exampleResolutionReceipt() error = %v", err)
	}
	budgetReceipt, err := exampleBudgetReceipt(agentBundle, receipt)
	if err != nil {
		t.Fatalf("exampleBudgetReceipt() error = %v", err)
	}
	for _, example := range []struct {
		name     string
		contract string
		value    any
	}{
		{name: "config-revision.json", contract: "config-revision", value: revision},
		{name: "config-bundle.json", contract: "config-bundle", value: bundle},
		{name: "config-revision.agent-review.json", contract: "config-revision-agent-review", value: agentRevision},
		{name: "config-bundle.agent-review.json", contract: "config-bundle-agent-review", value: agentBundle},
		{name: "config-resolution-receipt.json", contract: "config-resolution-receipt", value: receipt},
		{name: "config-resolution-receipt.replay-budget.json", contract: "config-resolution-receipt-replay-budget", value: budgetReceipt},
	} {
		want, err := json.MarshalIndent(example.value, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", example.name, err)
		}
		want = append(want, '\n')
		path := filepath.Join("..", "..", "examples", example.name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf(
				"%s is stale; regenerate with go run ./scripts/config_examples -contract %s",
				path,
				example.contract,
			)
		}
	}
}
