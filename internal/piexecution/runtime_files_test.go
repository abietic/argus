package piexecution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalRuntimeFileManifestBindsProductionDependencyBytes(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "node")
	worker := filepath.Join(root, "runtime", "dist", "worker.js")
	writeRuntimeFixtureFile(t, node, "node-binary")
	writeRuntimeFixtureFile(t, worker, "import 'runtime-dependency';")
	writeRuntimeFixtureFile(t, filepath.Join(root, "runtime", "dist", "helper.js"), "export const value = 1;")
	writeRuntimeFixtureFile(t, filepath.Join(root, "runtime", "package.json"), `{"type":"module"}`)
	writeRuntimeFixtureFile(t, filepath.Join(root, "runtime", "package-lock.json"), `{
  "lockfileVersion": 3,
  "packages": {
    "": {"dependencies": {"runtime-dependency": "1.0.0"}},
    "node_modules/runtime-dependency": {
      "version": "1.0.0",
      "integrity": "sha512-fixture"
    }
  }
}`)
	dependency := filepath.Join(root, "runtime", "node_modules", "runtime-dependency", "index.js")
	writeRuntimeFixtureFile(t, dependency, "export default 1;")

	manifest, data, digest, err := BuildLocalRuntimeFileManifest(
		t.Context(), node, worker,
	)
	if err != nil {
		t.Fatalf("BuildLocalRuntimeFileManifest() error = %v", err)
	}
	if manifest.Authority != LocalRuntimeFileManifestAuthority || len(data) == 0 ||
		len(manifest.Packages) != 1 || len(manifest.Files) != 5 {
		t.Fatalf("runtime manifest = %+v", manifest)
	}
	if err := (LocalRuntimeFileVerifier{}).Verify(t.Context(), node, worker, digest); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	writeRuntimeFixtureFile(t, dependency, "export default 2;")
	if err := (LocalRuntimeFileVerifier{}).Verify(t.Context(), node, worker, digest); err == nil ||
		!strings.Contains(err.Error(), "manifest changed") {
		t.Fatalf("Verify() drift error = %v", err)
	}
}

func TestLocalRuntimeFileManifestRejectsMissingAndSymlinkDependency(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "node")
	worker := filepath.Join(root, "runtime", "dist", "worker.js")
	writeRuntimeFixtureFile(t, node, "node-binary")
	writeRuntimeFixtureFile(t, worker, "export {};")
	writeRuntimeFixtureFile(t, filepath.Join(root, "runtime", "package.json"), `{}`)
	writeRuntimeFixtureFile(t, filepath.Join(root, "runtime", "package-lock.json"), `{
  "lockfileVersion": 3,
  "packages": {
    "": {},
    "node_modules/runtime-dependency": {"version": "1.0.0"}
  }
}`)
	if _, _, _, err := BuildLocalRuntimeFileManifest(t.Context(), node, worker); err == nil ||
		!strings.Contains(err.Error(), "runtime-dependency") {
		t.Fatalf("missing dependency error = %v", err)
	}
	dependencyRoot := filepath.Join(root, "runtime", "node_modules", "runtime-dependency")
	if err := os.MkdirAll(dependencyRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.js")
	writeRuntimeFixtureFile(t, target, "outside")
	if err := os.Symlink(target, filepath.Join(dependencyRoot, "index.js")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLocalRuntimeFileManifest(t.Context(), node, worker); err == nil ||
		!strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink dependency error = %v", err)
	}
}

func TestLocalRuntimeFileManifestHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, _, err := BuildLocalRuntimeFileManifest(ctx, "/node", "/runtime/dist/worker.js")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled manifest error = %v", err)
	}
}

func writeRuntimeFixtureFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
