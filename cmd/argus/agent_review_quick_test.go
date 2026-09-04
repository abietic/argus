package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abietic/argus/internal/formalreview"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestAgentReviewQuickRunsAndResumesExactFormalResult(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "go.mod", "module example.com/quick-review\n\ngo 1.26\n")
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n// TODO review me\n")
	revision := commitCLITarget(t, repositoryPath, "quick review target")
	store := t.TempDir()
	configState := t.TempDir()

	baseArgs := []string{
		"--store", store, "--config-state-dir", configState,
		"--idempotency-key", "quick-e2e", "--at", "2026-09-04T01:02:03Z",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--json",
	}
	firstArgs := append(append([]string(nil), baseArgs...),
		"--", "--repo", repositoryPath, "--mode", "selection",
		"--revision", revision, "--path", "review.go", "--start-line", "2", "--end-line", "2",
	)
	firstOptions, err := parseAgentReviewQuickFlags(firstArgs)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(firstOptions.formal.options)
	if err != nil {
		t.Fatal(err)
	}
	runner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256}
	var firstBuffer bytes.Buffer
	if err := executeAgentReviewQuickWithRunner(t.Context(), firstOptions, &firstBuffer, runner); err != nil {
		t.Fatalf("first quick review: %v", err)
	}
	var first agentReviewQuickOutput
	if err := json.Unmarshal(firstBuffer.Bytes(), &first); err != nil {
		t.Fatalf("decode first quick output: %v\n%s", err, firstBuffer.String())
	}
	if first.Phase != "succeeded" || first.Source == nil || first.SourceRunID == "" ||
		first.ResumeSourceRun != first.SourceRunID || first.Bootstrap == nil ||
		first.Formal == nil || first.Formal.Status != contractsv1alpha1.StageExecutionSucceeded ||
		runner.calls != 1 {
		t.Fatalf("first quick output=%+v runner_calls=%d", first, runner.calls)
	}

	resumeArgs := append(append([]string(nil), baseArgs...), "--source-run", first.SourceRunID)
	resumeOptions, err := parseAgentReviewQuickFlags(resumeArgs)
	if err != nil {
		t.Fatal(err)
	}
	var resumeBuffer bytes.Buffer
	if err := executeAgentReviewQuickWithRunner(t.Context(), resumeOptions, &resumeBuffer, runner); err != nil {
		t.Fatalf("resume quick review: %v", err)
	}
	var resumed agentReviewQuickOutput
	if err := json.Unmarshal(resumeBuffer.Bytes(), &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Source != nil || resumed.SourceRunID != first.SourceRunID || resumed.Formal == nil ||
		resumed.Formal.FormalRunID != first.Formal.FormalRunID ||
		!resumed.Formal.RecoveredDispatch || !resumed.Formal.RecoveredAdmission ||
		!resumed.Formal.RecoveredFinalRun || runner.calls != 1 {
		t.Fatalf("resumed quick output=%+v runner_calls=%d", resumed, runner.calls)
	}

	humanArgs := append(append([]string(nil), baseArgs[:len(baseArgs)-1]...),
		"--source-run", first.SourceRunID,
	)
	humanOptions, err := parseAgentReviewQuickFlags(humanArgs)
	if err != nil {
		t.Fatal(err)
	}
	var humanBuffer bytes.Buffer
	if err := executeAgentReviewQuickWithRunner(t.Context(), humanOptions, &humanBuffer, runner); err != nil {
		t.Fatalf("human-readable quick resume: %v", err)
	}
	if !strings.Contains(humanBuffer.String(), "phase=succeeded") ||
		!strings.Contains(humanBuffer.String(), "source_run="+first.SourceRunID) || runner.calls != 1 {
		t.Fatalf("human-readable output=%q runner_calls=%d", humanBuffer.String(), runner.calls)
	}
}

func TestAgentReviewQuickFailedFormalKeepsResumeCoordinate(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n// TODO review me\n")
	revision := commitCLITarget(t, repositoryPath, "failed quick target")
	arguments := []string{
		"--store", t.TempDir(), "--config-state-dir", t.TempDir(),
		"--idempotency-key", "quick-failed", "--at", "2026-09-04T02:03:04Z",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--json", "--",
		"--repo", repositoryPath, "--mode", "selection", "--revision", revision,
		"--path", "review.go", "--start-line", "2", "--end-line", "2",
	}
	options, err := parseAgentReviewQuickFlags(arguments)
	if err != nil {
		t.Fatal(err)
	}
	runner := &formalFailedRunner{}
	var outputBuffer bytes.Buffer
	err = executeAgentReviewQuickWithRunner(t.Context(), options, &outputBuffer, runner)
	if err == nil || !strings.Contains(err.Error(), "ended with failed") {
		t.Fatalf("failed quick review error=%v", err)
	}
	var output agentReviewQuickOutput
	if decodeErr := json.Unmarshal(outputBuffer.Bytes(), &output); decodeErr != nil {
		t.Fatalf("decode failed quick output: %v\n%s", decodeErr, outputBuffer.String())
	}
	if output.Phase != "formal" || output.SourceRunID == "" ||
		output.ResumeSourceRun != output.SourceRunID || output.Source == nil ||
		output.Bootstrap == nil || output.Formal == nil ||
		output.Formal.Status != contractsv1alpha1.StageExecutionFailed || runner.calls != 1 {
		t.Fatalf("failed quick output=%+v runner_calls=%d", output, runner.calls)
	}
}

func TestParseAgentReviewQuickRejectsAmbiguousTargets(t *testing.T) {
	base := []string{
		"--store", "/tmp/argus-store", "--config-state-dir", "/tmp/argus-config",
		"--idempotency-key", "quick-parse", "--at", "2026-09-04T01:02:03Z",
		"--node", "/usr/bin/node", "--worker-script", "/tmp/worker.js",
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4",
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing target", args: base, want: "after -- are required"},
		{name: "reserved store", args: append(append([]string(nil), base...), "--", "--store", "/tmp/other"), want: "--store is owned"},
		{name: "resume target", args: append(append([]string(nil), base...), "--source-run", "run-1", "--", "--repo", "/tmp/repo"), want: "not allowed with --source-run"},
		{name: "invalid at", args: replaceArgumentValue(base, "--at", "tomorrow"), want: "--at must be RFC3339"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseAgentReviewQuickFlags(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error=%v want contains %q", err, test.want)
			}
		})
	}
}

func replaceArgumentValue(arguments []string, name string, value string) []string {
	replaced := append([]string(nil), arguments...)
	for index := range replaced {
		if replaced[index] == name && index+1 < len(replaced) {
			replaced[index+1] = value
			return replaced
		}
	}
	return replaced
}
