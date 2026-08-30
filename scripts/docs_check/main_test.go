package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckMarkdownSkipsGeneratedAndDependencyDirectories(t *testing.T) {
	root := t.TempDir()
	dependency := filepath.Join(root, "runtime", "node_modules", "dependency")
	if err := os.MkdirAll(dependency, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dependency, "README.md"),
		[]byte("[missing](./CONTRIBUTING.md)\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := checkMarkdownRoot(root); err != nil {
		t.Fatalf("dependency Markdown must not enter the repository docs gate: %v", err)
	}

	owned := filepath.Join(root, "owned.md")
	if err := os.WriteFile(owned, []byte("[missing](./missing.md)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkMarkdownRoot(root); err == nil || !strings.Contains(err.Error(), "broken link") {
		t.Fatalf("repository-owned broken link must fail, got %v", err)
	}
}
