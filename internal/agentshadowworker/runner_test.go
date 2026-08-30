package agentshadowworker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSubprocessRunnerRejectsRelativeAndSymlinkExecutables(t *testing.T) {
	node := testNodePath(t)
	script := testNodeScript(t, "process.stdout.write('{}')")
	runner := NewSubprocessRunner()
	request := testRunnerRequest(node, script)

	request.NodePath = "node"
	if _, err := runner.Run(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "clean and absolute") {
		t.Fatalf("relative node error = %v", err)
	}

	request = testRunnerRequest(node, script)
	symlink := filepath.Join(t.TempDir(), "worker-link.cjs")
	if err := os.Symlink(script, symlink); err != nil {
		t.Fatal(err)
	}
	request.ScriptPath = symlink
	if _, err := runner.Run(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "regular non-symlink") {
		t.Fatalf("symlink script error = %v", err)
	}
}

func TestSubprocessRunnerUsesExplicitEnvironmentAllowlist(t *testing.T) {
	node := testNodePath(t)
	script := testNodeScript(t, `process.stdout.write(JSON.stringify(process.env))`)
	runner := NewSubprocessRunner()
	request := testRunnerRequest(node, script)
	request.Environment = map[string]string{"HOME": "/secret"}
	if _, err := runner.Run(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), `"HOME" is not allowed`) {
		t.Fatalf("disallowed environment error = %v", err)
	}

	request.Environment = map[string]string{
		"LANG": "C", "PATH": "/usr/bin", "ANTHROPIC_API_KEY": "test-key",
	}
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Stdout)
	for _, expected := range []string{`"LANG":"C"`, `"PATH":"/usr/bin"`, `"ANTHROPIC_API_KEY":"test-key"`} {
		if !strings.Contains(output, expected) {
			t.Fatalf("explicit environment %s missing from %s", expected, output)
		}
	}
	if strings.Contains(output, `"HOME"`) {
		t.Fatalf("host environment leaked into worker: %s", output)
	}
}

func TestSubprocessRunnerBoundsInputStdoutAndStderr(t *testing.T) {
	node := testNodePath(t)
	runner := NewSubprocessRunner()

	request := testRunnerRequest(node, testNodeScript(t, "process.stdout.write('x'.repeat(4096))"))
	request.MaxOutputBytes = 32
	if _, err := runner.Run(context.Background(), request); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("stdout limit error = %v", err)
	}

	request = testRunnerRequest(node, testNodeScript(t, "process.stderr.write('x'.repeat(4096))"))
	request.MaxStderrBytes = 32
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("bounded stderr progress made valid worker fail: %v", err)
	}
	if result.StderrBytes < 4096 || result.StderrSHA256 == "" {
		t.Fatalf("stderr truncation metadata = %#v", result)
	}

	request = testRunnerRequest(node, testNodeScript(t, "process.stdout.write('{}')"))
	request.Input = []byte("oversized")
	request.MaxInputBytes = 2
	if _, err := runner.Run(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "worker input exceeds") {
		t.Fatalf("stdin limit error = %v", err)
	}
}

func TestSubprocessRunnerDoesNotReflectStderr(t *testing.T) {
	node := testNodePath(t)
	marker := "provider-secret-marker"
	script := testNodeScript(t, "process.stderr.write('"+marker+"'); process.exit(2)")
	result, err := NewSubprocessRunner().Run(
		context.Background(),
		testRunnerRequest(node, script),
	)
	if !errors.Is(err, ErrWorkerExit) {
		t.Fatalf("worker exit error = %v", err)
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(string(result.Stdout), marker) {
		t.Fatalf("stderr marker leaked through worker error/result: %v %#v", err, result)
	}
	if result.StderrBytes == 0 || result.StderrSHA256 == "" {
		t.Fatalf("stderr diagnostic metadata missing: %#v", result)
	}
}

func TestSubprocessRunnerStreamsCompleteProgressLinesAndPropagatesCallbackFailure(t *testing.T) {
	node := testNodePath(t)
	script := testNodeScript(t, `process.stderr.write('{"phase":"one"}\n{"phase":"two"}\n'); process.stdout.write('{}')`)
	request := testRunnerRequest(node, script)
	lines := []string{}
	request.ProgressLine = func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	}
	if _, err := NewSubprocessRunner().Run(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != `{"phase":"one"}` || lines[1] != `{"phase":"two"}` {
		t.Fatalf("progress lines = %#v", lines)
	}

	request = testRunnerRequest(node, script)
	request.ProgressLine = func([]byte) error { return errors.New("checkpoint rejected") }
	if _, err := NewSubprocessRunner().Run(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "checkpoint rejected") {
		t.Fatalf("progress callback failure = %v", err)
	}
}

func TestSubprocessRunnerCancellationTerminatesProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group signaling is Unix-specific")
	}
	node := testNodePath(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := testNodeScript(t, `
const {spawn} = require('node:child_process');
const fs = require('node:fs');
const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], {stdio: 'ignore'});
fs.writeFileSync(`+strconv.Quote(pidFile)+`, String(child.pid));
setInterval(() => {}, 1000);
`)
	runner := NewSubprocessRunner()
	runner.TerminationWait = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := runner.Run(ctx, testRunnerRequest(node, script))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}

	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("worker did not record child pid: %v", err)
	}
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker child process %d survived process-group cancellation", pid)
}

func testRunnerRequest(node, script string) Request {
	return Request{
		NodePath: node, ScriptPath: script, Input: []byte("{}"),
		MaxInputBytes: 1024, MaxOutputBytes: 1024, MaxStderrBytes: 1024,
	}
}

func testNodePath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for subprocess runner tests")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func testNodeScript(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.cjs")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
