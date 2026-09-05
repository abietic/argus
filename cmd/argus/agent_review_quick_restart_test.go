package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"

	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
)

// The worker is physically killed after a checkpoint fsync. A shorter, pinned
// test lease expires in real wall-clock time; no provider credentials are used.
func TestDurableQuickOriginalCommandResumesAfterPhysicalKill(t *testing.T) {
	if encoded := os.Getenv("ARGUS_QUICK_RESTART_TEST_ARGS"); encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var args []string
		if err := json.Unmarshal(data, &args); err != nil {
			t.Fatal(err)
		}
		options, err := parseAgentReviewQuickFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := executeAgentReviewQuickWithPolicy(t.Context(), options, &output,
			physicalBlockingRunner{marker: os.Getenv("ARGUS_QUICK_RESTART_TEST_MARKER")}, physicalSchedulingPolicy()); err != nil {
			t.Fatalf("child quick: %v; output=%s", err, output.String())
		}
		t.Fatal("blocking quick unexpectedly completed")
	}
	fixture := newDurableQuickFixture(t)
	marker := filepath.Join(t.TempDir(), "checkpoint-durable")
	args, err := json.Marshal(fixture.args)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestDurableQuickOriginalCommandResumesAfterPhysicalKill$", "-test.count=1")
	child.Env = append(os.Environ(), "ARGUS_QUICK_RESTART_TEST_ARGS="+base64.StdEncoding.EncodeToString(args),
		"ARGUS_QUICK_RESTART_TEST_MARKER="+marker)
	var childOutput bytes.Buffer
	child.Stdout, child.Stderr = &childOutput, &childOutput
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	if err := waitForRegularFile(marker, 40*time.Second); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		waited = true
		t.Fatalf("quick never persisted checkpoint: %v; child=%s", err, childOutput.String())
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("SIGKILL worker exited successfully")
	}
	waited = true
	store, err := local.Open(fixture.options.formal.store)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := reviewjob.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	source, sourceCommand, err := jobs.GetByIdempotency(quickActor, fixture.options.rootKey+"-source")
	if err != nil {
		t.Fatal(err)
	}
	formal, _, err := jobs.GetByIdempotency(quickActor, fixture.options.rootKey+"-run")
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := scheduling.NewRepository(store, physicalSchedulingPolicy())
	if err != nil {
		t.Fatal(err)
	}
	before, err := scheduler.Get(formal.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != scheduling.StateLeased || before.Generation != 1 || before.ActiveLease == nil {
		t.Fatalf("pre-recovery authority=%+v", before)
	}
	if delay := time.Until(before.ActiveLease.ExpiresAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	if _, err := scheduler.ReconcileWorkloads(t.Context(), []string{formal.WorkloadID}, scheduling.Mutation{
		IdempotencyKey: "quick-test-expire-killed-owner", Actor: "quick-restart-test",
		Audit: "wall-clock lease expiry after physical SIGKILL", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	writeCLITargetFile(t, fixture.repository, "review.go", "package fixture\n// moved after worker crash\n")
	commitCLITarget(t, fixture.repository, "move HEAD during quick recovery")
	resumedMarker := marker + ".resumed"
	execute := func(runner agentshadowworker.Runner) (agentReviewQuickOutput, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		var output bytes.Buffer
		err := executeAgentReviewQuickWithPolicy(ctx, fixture.options, &output, runner, physicalSchedulingPolicy())
		var result agentReviewQuickOutput
		if decodeErr := json.Unmarshal(output.Bytes(), &result); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return result, err
	}
	result, runErr := execute(physicalCheckpointResumeRunner{marker: resumedMarker})
	if runErr == nil || result.SourceRunID != source.RunID || result.Formal == nil {
		t.Fatalf("resumed result=%+v error=%v", result, runErr)
	}
	if _, err := os.Stat(resumedMarker); err != nil {
		t.Fatalf("checkpoint not delivered: %v", err)
	}
	terminal, err := scheduler.Get(formal.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != scheduling.StateFailed || terminal.Generation != 2 {
		t.Fatalf("terminal=%+v", terminal)
	}
	_, afterSource, err := jobs.GetByIdempotency(quickActor, fixture.options.rootKey+"-source")
	if err != nil {
		t.Fatal(err)
	}
	if quickDigest(afterSource) != quickDigest(sourceCommand) {
		t.Fatal("crash recovery replaced source command")
	}
	retryRunner := &formalFailedRunner{}
	_, _ = execute(retryRunner)
	if retryRunner.calls != 0 {
		t.Fatal("terminal retry reran provider")
	}
	_, err = scheduler.Complete(t.Context(), scheduling.Callback{
		SchemaVersion: scheduling.CallbackSchemaVersion, IdempotencyKey: "quick-stale-callback",
		LeaseID: before.ActiveLease.LeaseID, WorkloadID: before.ActiveLease.WorkloadID,
		WorkerID: before.ActiveLease.WorkerID, Attempt: before.ActiveLease.Attempt,
		Generation: before.ActiveLease.Generation, FencingToken: before.ActiveLease.FencingToken,
		Status: scheduling.CallbackFailed, FailureCode: "killed_owner", OutputRefs: []string{}, OccurredAt: time.Now().UTC(),
	})
	if !errors.Is(err, scheduling.ErrFenced) {
		t.Fatalf("stale callback error=%v", err)
	}
}
