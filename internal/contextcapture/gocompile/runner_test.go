package gocompile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLocalRunnerCompilesTestsWithoutExecutingRepositoryCode(t *testing.T) {
	runner, err := NewLocalRunner()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "repository-code-executed")
	writeRunnerFixture(t, workspace, "go.mod", "module example.com/fixture\n\ngo 1.26\n")
	writeRunnerFixture(t, workspace, "fixture.go", "package fixture\nfunc Value() int { return 1 }\n")
	writeRunnerFixture(t, workspace, "fixture_test.go", `package fixture
import (
  "os"
  "testing"
)
func TestValue(t *testing.T) { if Value() != 1 { t.Fatal("bad") } }
func TestMain(m *testing.M) { _ = os.WriteFile(`+strconvQuote(marker)+`, []byte("executed"), 0600); os.Exit(m.Run()) }
`)
	result, err := runner.Compile(t.Context(), CompileRequest{
		Workspace: workspace, Package: ".", OutputPath: filepath.Join(workspace, "out.test"),
		MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("compile failed: %s", result.Output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test -c executed repository TestMain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.test")); err != nil {
		t.Fatalf("compiled test binary missing: %v", err)
	}
}

func TestLocalRunnerReturnsCompileDiagnosticsAndDisablesModuleNetwork(t *testing.T) {
	runner, err := NewLocalRunner()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("compile failure", func(t *testing.T) {
		workspace := t.TempDir()
		writeRunnerFixture(t, workspace, "go.mod", "module example.com/fixture\n\ngo 1.26\n")
		writeRunnerFixture(t, workspace, "broken.go", "package fixture\nfunc Broken() { missing() }\n")
		result, err := runner.Compile(t.Context(), CompileRequest{
			Workspace: workspace, Package: ".", OutputPath: filepath.Join(workspace, "out.test"),
			MaxOutputBytes: 1 << 20,
		})
		if err != nil || result.ExitCode == 0 || !strings.Contains(string(result.Output), "undefined: missing") {
			t.Fatalf("compile result = %+v, %v", result, err)
		}
	})
	t.Run("offline dependency", func(t *testing.T) {
		workspace := t.TempDir()
		writeRunnerFixture(t, workspace, "go.mod", "module example.com/fixture\n\ngo 1.26\n")
		writeRunnerFixture(t, workspace, "fixture.go", "package fixture\nimport _ \"example.invalid/missing\"\n")
		result, err := runner.Compile(t.Context(), CompileRequest{
			Workspace: workspace, Package: ".", OutputPath: filepath.Join(workspace, "out.test"),
			MaxOutputBytes: 1 << 20,
		})
		if err != nil || result.ExitCode == 0 || !dependencyUnavailable(result.Output) {
			t.Fatalf("offline module result = %+v, %v", result, err)
		}
	})
}

func TestLocalRunnerHonorsCancellationAndOutputLimit(t *testing.T) {
	runner, err := NewLocalRunner()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	writeRunnerFixture(t, workspace, "go.mod", "module example.com/fixture\n\ngo 1.26\n")
	var source strings.Builder
	source.WriteString("package fixture\nfunc Broken() {\n")
	for index := range 100 {
		source.WriteString("missing" + string(rune('A'+index%26)) + "()\n")
	}
	source.WriteString("}\n")
	writeRunnerFixture(t, workspace, "broken.go", source.String())
	_, err = runner.Compile(t.Context(), CompileRequest{
		Workspace: workspace, Package: ".", OutputPath: filepath.Join(workspace, "out.test"),
		MaxOutputBytes: 64,
	})
	if !errors.Is(err, ErrCommandOutputLimit) {
		t.Fatalf("output limit error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = runner.Compile(canceled, CompileRequest{
		Workspace: workspace, Package: ".", OutputPath: filepath.Join(workspace, "out.test"),
		MaxOutputBytes: 1 << 20,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled compile error = %v", err)
	}
}

func writeRunnerFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func strconvQuote(value string) string { return strconv.Quote(value) }
