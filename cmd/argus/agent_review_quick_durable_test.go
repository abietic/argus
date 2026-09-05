package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestDurableQuickBootstrapManifestDigestSurvivesJSONRoundTrip(t *testing.T) {
	// A manifest behind an interface would decode as map[string]any and
	// re-encode with a different object-key order, falsely signaling corruption.
	before := quickReviewPlan{Bootstrap: formalAgentBootstrapOutput{Manifest: piexecution.RuntimeManifest{}}}
	encoded, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var after quickReviewPlan
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatal(err)
	}
	if quickDigest(before) != quickDigest(after) {
		t.Fatalf("manifest roundtrip changed digest: before=%T after=%T", before.Bootstrap.Manifest, after.Bootstrap.Manifest)
	}
}

func TestDurableQuickOriginalCommandRetryKeepsFrozenHEAD(t *testing.T) {
	fixture := newDurableQuickFixture(t)
	runner := &formalSucceededRunner{promptDigest: fixture.bootstrap.Manifest.Prompt.SHA256}
	first, err := executeDurableQuickTest(t, fixture.options, runner)
	if err != nil {
		t.Fatalf("first quick command: %v", err)
	}
	writeCLITargetFile(t, fixture.repository, "review.go", "package fixture\n// HEAD changed after initial review\n")
	moved := commitCLITarget(t, fixture.repository, "move HEAD after durable review")
	if moved == fixture.revision {
		t.Fatal("HEAD did not move")
	}
	second, err := executeDurableQuickTest(t, fixture.options, runner)
	if err != nil {
		t.Fatalf("repeat original quick command: %v", err)
	}
	if first.SourceRunID == "" || second.SourceRunID != first.SourceRunID ||
		first.Formal == nil || second.Formal == nil ||
		second.Formal.FormalRunID != first.Formal.FormalRunID ||
		second.IntentID != first.IntentID || second.SourceJobID != first.SourceJobID ||
		second.FormalJobID != first.FormalJobID || runner.calls != 1 {
		t.Fatalf("original retry changed execution: first=%+v second=%+v calls=%d", first, second, runner.calls)
	}
	store, err := local.Open(fixture.options.formal.store)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := loadQuickImmutable[quickReviewIntent](store, first.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Request.Revision != fixture.revision {
		t.Fatalf("frozen revision = %q, want %q", intent.Request.Revision, fixture.revision)
	}
	jobs, err := reviewjob.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	_, command, err := jobs.GetByIdempotency(quickActor, fixture.options.rootKey+"-source")
	if err != nil {
		t.Fatal(err)
	}
	if command.Request.Revision != fixture.revision {
		t.Fatalf("source job read moving HEAD: %+v", command.Request)
	}
}

func TestDurableQuickConcurrentFirstCommandsShareOneExecution(t *testing.T) {
	fixture := newDurableQuickFixture(t)
	runner := &synchronizedQuickRunner{runner: formalSucceededRunner{
		promptDigest: fixture.bootstrap.Manifest.Prompt.SHA256,
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	type result struct {
		output agentReviewQuickOutput
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			var buffer bytes.Buffer
			err := executeAgentReviewQuickWithRunner(ctx, fixture.options, &buffer, runner)
			var output agentReviewQuickOutput
			if buffer.Len() > 0 {
				err = errors.Join(err, json.Unmarshal(buffer.Bytes(), &output))
			}
			results <- result{output: output, err: err}
		}()
	}
	close(start)
	var completed []result
	for range 2 {
		select {
		case value := <-results:
			completed = append(completed, value)
		case <-ctx.Done():
			t.Fatalf("concurrent quick did not complete: %v", ctx.Err())
		}
	}
	for _, value := range completed {
		if value.err != nil || value.output.Phase != "succeeded" || value.output.Formal == nil {
			t.Fatalf("concurrent first command: output=%+v error=%v", value.output, value.err)
		}
	}
	first, second := completed[0].output, completed[1].output
	if first.IntentID != second.IntentID || first.SourceRunID != second.SourceRunID ||
		first.SourceJobID != second.SourceJobID || first.FormalJobID != second.FormalJobID ||
		first.Formal.FormalRunID != second.Formal.FormalRunID || runner.callCount() != 1 {
		t.Fatalf("concurrent calls did not share execution: first=%+v second=%+v calls=%d", first, second, runner.callCount())
	}
}

type synchronizedQuickRunner struct {
	mu     sync.Mutex
	runner formalSucceededRunner
}

func (runner *synchronizedQuickRunner) Run(ctx context.Context, request agentshadowworker.Request) (agentshadowworker.Result, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.runner.Run(ctx, request)
}

func (runner *synchronizedQuickRunner) callCount() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.runner.calls
}

func TestDurableQuickSameKeyRejectsChangedArgumentsBeforeExecution(t *testing.T) {
	fixture := newDurableQuickFixture(t)
	store, err := local.Open(fixture.options.formal.store)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := freezeQuickIntent(t.Context(), store, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saveQuickImmutable(store, quickIntentID(fixture.options.rootKey), intent); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		flag  string
		value string
	}{
		{name: "model", flag: "--model", value: "different-model"},
		{name: "publication_time", flag: "--at", value: "2026-09-05T01:02:04Z"},
		{name: "pricing", flag: "--input-micros-per-million", value: "2"},
		{name: "selection", flag: "--end-line", value: "3"},
		{name: "repository_ref", flag: "--revision", value: fixture.revision},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed, err := parseAgentReviewQuickFlags(replaceArgumentValue(fixture.args, test.flag, test.value))
			if err != nil {
				t.Fatal(err)
			}
			runner := &formalFailedRunner{}
			_, err = executeDurableQuickTest(t, changed, runner)
			if !errors.Is(err, reviewjob.ErrConflict) || runner.calls != 0 {
				t.Fatalf("changed intent error=%v calls=%d", err, runner.calls)
			}
		})
	}
	if _, err := loadQuickImmutable[quickReviewPlan](store, quickIntentID(fixture.options.rootKey)+"-plan"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected intent wrote execution plan: %v", err)
	}
}

func TestDurableQuickFrozenOverlayAndContextSurviveInputFileChanges(t *testing.T) {
	fixture := newDurableQuickFixture(t)
	overlayPath := filepath.Join(t.TempDir(), "overlay.go")
	contextPath := filepath.Join(t.TempDir(), "context.json")
	const overlay = "package fixture\n// FROZEN_OVERLAY_MARKER\n"
	const contextMarker = "FROZEN_EXTERNAL_CONTEXT_MARKER"
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contextPath, []byte(`{"marker":"`+contextMarker+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append(append([]string(nil), fixture.args...),
		"--overlay-file", overlayPath, "--context-file", "codegraph@Review="+contextPath)
	options, err := parseAgentReviewQuickFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	store, err := local.Open(options.formal.store)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := freezeQuickIntent(t.Context(), store, options)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Request.OverlayContent == nil || *intent.Request.OverlayContent != overlay ||
		len(intent.Request.Contexts) != 1 || intent.Request.Contexts[0].Ref == nil {
		t.Fatalf("intent did not freeze explicit inputs: %+v", intent.Request)
	}
	if _, err := saveQuickImmutable(store, quickIntentID(options.rootKey), intent); err != nil {
		t.Fatal(err)
	}
	// The acknowledged intent must not reopen these paths during its first
	// execution after restart, nor recapture the then-current HEAD.
	if err := os.WriteFile(overlayPath, []byte("package fixture\n// MUTATED_OVERLAY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contextPath, []byte(`{"marker":"MUTATED_CONTEXT"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCLITargetFile(t, fixture.repository, "review.go", "package fixture\n// MUTATED_HEAD\n")
	commitCLITarget(t, fixture.repository, "mutate inputs after durable intent")
	runner := &formalSucceededRunner{
		promptDigest: fixture.bootstrap.Manifest.Prompt.SHA256, contextMarker: contextMarker,
	}
	first, err := executeDurableQuickTest(t, options, runner)
	if err != nil {
		t.Fatalf("execute frozen intent: %v", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runs.ExecutionSnapshotForRun(first.SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	var input reviewcore.ReviewInput
	if err := runs.ReadJSONArtifact(snapshot.ReviewInputRef, &input); err != nil {
		t.Fatal(err)
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inputJSON), "FROZEN_OVERLAY_MARKER") ||
		strings.Contains(string(inputJSON), "MUTATED_OVERLAY") || strings.Contains(string(inputJSON), "MUTATED_HEAD") {
		t.Fatalf("source input did not use frozen overlay: %s", inputJSON)
	}
	if err := os.Remove(overlayPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(contextPath); err != nil {
		t.Fatal(err)
	}
	second, err := executeDurableQuickTest(t, options, runner)
	if err != nil || second.SourceRunID != first.SourceRunID || runner.calls != 1 {
		t.Fatalf("retry after input files disappeared: output=%+v error=%v calls=%d", second, err, runner.calls)
	}
}

func TestDurableQuickTerminalFailureRetryDoesNotCallProviderAgain(t *testing.T) {
	fixture := newDurableQuickFixture(t)
	runner := &formalFailedRunner{}
	first, firstErr := executeDurableQuickTest(t, fixture.options, runner)
	second, secondErr := executeDurableQuickTest(t, fixture.options, runner)
	if firstErr == nil || secondErr == nil ||
		!strings.Contains(firstErr.Error(), "ended with failed") ||
		!strings.Contains(secondErr.Error(), "ended with failed") {
		t.Fatalf("terminal failure was not retained: first=%v second=%v", firstErr, secondErr)
	}
	if first.Formal == nil || second.Formal == nil ||
		first.Formal.Status != contractsv1alpha1.StageExecutionFailed ||
		second.Formal.Status != contractsv1alpha1.StageExecutionFailed ||
		first.Formal.FormalRunID != second.Formal.FormalRunID ||
		first.SourceRunID != second.SourceRunID || runner.calls != 1 {
		t.Fatalf("terminal retry lost evidence or repeated provider: first=%+v second=%+v calls=%d", first, second, runner.calls)
	}
}

func TestDurableQuickPlanDoesNotResolveChangedConfigLedgerOnRetry(t *testing.T) {
	fixture := newDurableQuickFixture(t)
	store, err := local.Open(fixture.options.formal.store)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := freezeQuickIntent(t.Context(), store, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	id := quickIntentID(fixture.options.rootKey)
	first, err := freezeQuickPlan(t.Context(), store, id, intent)
	if err != nil {
		t.Fatal(err)
	}
	configStore, err := local.Open(filepath.Join(fixture.options.formal.configState, "quick", id, "source"))
	if err != nil {
		t.Fatal(err)
	}
	configs, err := configrepo.New(configStore)
	if err != nil {
		t.Fatal(err)
	}
	variant := intent.SourceRevision
	variant.Revision = "changed-after-quick-freeze"
	target := *variant.Patch.Target
	limit := *target.MaxFiles + 1
	target.MaxFiles = &limit
	variant.Patch.Target = &target
	if err := resolveOrPublishFormalConfig(t.Context(), configs, variant,
		bootstrapConfigTestMutation("changed-source-config", intent.Identity.PublicationAt.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	second, err := freezeQuickPlan(t.Context(), store, id, intent)
	if err != nil {
		t.Fatal(err)
	}
	if quickDigest(first) != quickDigest(second) {
		t.Fatal("quick retry re-resolved configuration from a changed ledger")
	}
}

func TestDurableQuickRuntimeDriftFailsClosedAndTerminalRetryUsesStoredResult(t *testing.T) {
	fixture := newDurableQuickFixture(t)
	knowledgePath := filepath.Join(t.TempDir(), "repository-invariants.md")
	const frozenKnowledge = "# Repository invariants\n\nFROZEN_KNOWLEDGE_MARKER\n"
	writeKnowledge := func(content string) {
		t.Helper()
		if err := os.WriteFile(knowledgePath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeKnowledge(frozenKnowledge)
	formalArgs, reviewArgs, _ := splitQuickArguments(fixture.args)
	args := append(append(append([]string(nil), formalArgs...), "--knowledge", knowledgePath, "--"), reviewArgs...)
	options, err := parseAgentReviewQuickFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	store, err := local.Open(options.formal.store)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := freezeQuickIntent(t.Context(), store, options)
	if err != nil {
		t.Fatal(err)
	}
	id := quickIntentID(options.rootKey)
	if _, err := saveQuickImmutable(store, id, intent); err != nil {
		t.Fatal(err)
	}
	runner := &formalSucceededRunner{
		promptDigest: fixture.bootstrap.Manifest.Prompt.SHA256, knowledgeMarker: "FROZEN_KNOWLEDGE_MARKER",
	}
	writeKnowledge("# Repository invariants\n\nMUTATED_KNOWLEDGE\n")
	beforePlan, err := executeDurableQuickTest(t, options, runner)
	if err == nil || !strings.Contains(err.Error(), "runtime changed after intent was frozen") ||
		beforePlan.Source != nil || runner.calls != 0 {
		t.Fatalf("pre-plan drift was not rejected: output=%+v error=%v calls=%d", beforePlan, err, runner.calls)
	}
	writeKnowledge(frozenKnowledge)
	if _, err := freezeQuickPlan(t.Context(), store, id, intent); err != nil {
		t.Fatal(err)
	}
	writeKnowledge("# Repository invariants\n\nMUTATED_KNOWLEDGE\n")
	beforeFormal, err := executeDurableQuickTest(t, options, runner)
	if err == nil || !strings.Contains(err.Error(), "runtime drifted before formal admission") ||
		beforeFormal.Source == nil || beforeFormal.Formal != nil || runner.calls != 0 {
		t.Fatalf("pre-admission drift was not rejected: output=%+v error=%v calls=%d", beforeFormal, err, runner.calls)
	}
	writeKnowledge(frozenKnowledge)
	completed, err := executeDurableQuickTest(t, options, runner)
	if err != nil || completed.Phase != "succeeded" || completed.Formal == nil || runner.calls != 1 {
		t.Fatalf("restore original runtime did not resume: output=%+v error=%v calls=%d", completed, err, runner.calls)
	}
	if err := os.Remove(knowledgePath); err != nil {
		t.Fatal(err)
	}
	retried, err := executeDurableQuickTest(t, options, runner)
	if err != nil || retried.Formal == nil ||
		retried.Formal.FormalRunID != completed.Formal.FormalRunID || runner.calls != 1 {
		t.Fatalf("terminal retry reopened runtime inputs: output=%+v error=%v calls=%d", retried, err, runner.calls)
	}
}

type durableQuickFixture struct {
	args       []string
	options    agentReviewQuickFlags
	bootstrap  formalreview.LocalPiBootstrap
	repository string
	revision   string
}

func newDurableQuickFixture(t *testing.T) durableQuickFixture {
	t.Helper()
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
	repository := newCLITargetRepository(t)
	writeCLITargetFile(t, repository, "go.mod", "module example.com/durable-quick\n\ngo 1.26\n")
	writeCLITargetFile(t, repository, "review.go", "package fixture\n// TODO review me\n")
	revision := commitCLITarget(t, repository, "durable quick target")
	args := []string{
		"--store", t.TempDir(), "--config-state-dir", t.TempDir(),
		"--idempotency-key", "durable-quick", "--at", "2026-09-05T01:02:03Z",
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--input-micros-per-million", "1", "--output-micros-per-million", "1",
		"--max-bytes-per-input-token", "4", "--json", "--",
		"--repo", repository, "--mode", "selection", "--revision", "HEAD",
		"--path", "review.go", "--start-line", "2", "--end-line", "2",
	}
	options, err := parseAgentReviewQuickFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(options.formal.options)
	if err != nil {
		t.Fatal(err)
	}
	return durableQuickFixture{args: args, options: options, bootstrap: bootstrap, repository: repository, revision: revision}
}

func executeDurableQuickTest(t *testing.T, options agentReviewQuickFlags, runner agentshadowworker.Runner) (agentReviewQuickOutput, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	var buffer bytes.Buffer
	err := executeAgentReviewQuickWithRunner(ctx, options, &buffer, runner)
	var output agentReviewQuickOutput
	if buffer.Len() > 0 {
		if decodeErr := json.Unmarshal(buffer.Bytes(), &output); decodeErr != nil {
			t.Fatalf("decode durable quick output: %v\n%s", decodeErr, buffer.String())
		}
	}
	return output, err
}
