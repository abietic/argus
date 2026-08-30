package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/contextprovider"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const physicalReviewJobHelperEnvironment = "ARGUS_PHYSICAL_REVIEW_JOB_HELPER"

const (
	livePhysicalReviewJobEnvironment = "ARGUS_LIVE_DEEPSEEK_CHECKPOINT_RESTART"
	physicalLiveModeEnvironment      = "ARGUS_PHYSICAL_REVIEW_JOB_LIVE_MODE"
	physicalLiveHelperEnvironment    = "ARGUS_PHYSICAL_REVIEW_JOB_LIVE_HELPER"
)

type physicalReviewJobFixture struct {
	node        string
	worker      string
	storePath   string
	configPath  string
	sourceRunID string
	options     formalreview.LocalPiBootstrapOptions
	service     *reviewjob.Service
	job         reviewjob.Record
}

func preparePhysicalReviewJob(t *testing.T, suffix string) physicalReviewJobFixture {
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
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\nfunc Divide(a, b int) int { return a / b }\n")
	revision := commitCLITarget(t, repositoryPath, "physical ReviewJob "+suffix)
	storePath := t.TempDir()
	configPath := t.TempDir()
	stateStore, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceRuns, err := runrepo.New(stateStore)
	if err != nil {
		t.Fatal(err)
	}
	gitSource, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	contextExecutor, err := contextprovider.NewLocalExecutor(gitSource)
	if err != nil {
		t.Fatal(err)
	}
	sourceService, err := application.NewService(gitSource, sourceRuns, application.ServiceOptions{
		DisableScheduling: true, ContextProviders: contextExecutor,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := sourceService.Review(t.Context(), application.ReviewRequest{
		RepositoryPath: repositoryPath, Mode: reviewcore.TargetModeSelection,
		Revision: revision, SelectionPath: "review.go", StartLine: 2, EndLine: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	options := physicalFormalOptions(node, worker)
	if err := executeFormalAgentBootstrap(t.Context(), formalAgentBootstrapFlags{
		store: storePath, configState: configPath, sourceRun: source.Run.RunID,
		idempotencyKey: "physical-" + suffix + "-bootstrap", at: "2026-08-25T12:00:00Z",
		node: node, workerScript: worker, providerProfile: "deepseek-anthropic-env",
		model: options.Model, json: true, options: options,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("formal bootstrap: %v", err)
	}
	service, err := newPhysicalReviewJobService(
		storePath, configPath, "physical-parent-admitter", &formalFailedRunner{}, options,
	)
	if err != nil {
		t.Fatal(err)
	}
	executionTimeoutSeconds := uint32(120)
	if os.Getenv(physicalLiveModeEnvironment) == "1" {
		executionTimeoutSeconds = 480
	}
	job, err := service.Submit(t.Context(), reviewjob.Request{
		SchemaVersion:    reviewjob.RequestSchemaVersion,
		ExecutionProfile: reviewjob.FormalPiExecutionProfile,
		SourceRunID:      source.Run.RunID, ExecutionTimeoutSeconds: executionTimeoutSeconds,
	}, reviewjob.Mutation{
		IdempotencyKey: "physical-" + suffix + "-formal-job",
		Actor:          "physical-review-operator",
		Audit:          "prove physical formal worker " + suffix,
		At:             time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("submit physical %s job: %v", suffix, err)
	}
	return physicalReviewJobFixture{
		node: node, worker: worker, storePath: storePath, configPath: configPath,
		sourceRunID: source.Run.RunID, options: options, service: service, job: job,
	}
}

// TestLiveFormalReviewJobRecoversDeepSeekCheckpointAfterPhysicalKill is an
// opt-in provider acceptance. It is deliberately excluded from make verify:
// it spends provider budget and requires the exact DeepSeek Anthropic-compatible
// environment. Generation 1 is stopped only after the real Pi worker has
// durably recorded both the complete context/review group checkpoint and the
// first cumulative candidate-verification checkpoint. Generation 2 must reuse
// both without re-running that verifier, finish any still-pending candidates,
// and commit a succeeded ReviewRun. The old generation is then proven fenced.
func TestLiveFormalReviewJobRecoversDeepSeekCheckpointAfterPhysicalKill(t *testing.T) {
	if mode := os.Getenv(physicalLiveHelperEnvironment); mode != "" {
		runLivePhysicalReviewJobHelper(t, mode)
		return
	}
	if os.Getenv(livePhysicalReviewJobEnvironment) != "1" {
		t.Skip("set ARGUS_LIVE_DEEPSEEK_CHECKPOINT_RESTART=1 to spend provider budget")
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			t.Skipf("%s is required for live DeepSeek checkpoint acceptance", name)
		}
	}
	t.Setenv(physicalLiveModeEnvironment, "1")
	fixture := preparePhysicalReviewJob(t, "live-restart")
	marker := filepath.Join(t.TempDir(), "live-checkpoint")
	first := livePhysicalReviewJobCommand(t, "block-after-checkpoint", fixture, marker)
	var firstOutput bytes.Buffer
	first.Stdout = &firstOutput
	first.Stderr = &firstOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	firstWaited := false
	defer func() {
		if first.Process != nil && !firstWaited {
			_ = first.Process.Kill()
			_, _ = first.Process.Wait()
		}
	}()
	if err := waitForRegularFileOrExecutorError(marker, 210*time.Second); err != nil {
		diagnostic, diagnosticErr := openPhysicalJobRecord(
			fixture.storePath, fixture.configPath, fixture.job.JobID, fixture.options,
		)
		executionError, _ := os.ReadFile(marker + ".error")
		t.Fatalf(
			"live worker never durably recorded a verification checkpoint: %v\nchild=%s\nexecutor=%s\nrecord=%s\nrecord_error=%v",
			err, firstOutput.String(), executionError,
			physicalReviewJobDiagnostic(diagnostic), diagnosticErr,
		)
	}
	checkpointGeneration, checkpointCount, checkpointRevision, err := inspectPhysicalCheckpointLedger(fixture.storePath)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointGeneration != 1 || checkpointCount != 2 || checkpointRevision != 1 {
		t.Fatalf(
			"generation 1 durable checkpoint ledger = generation %d count %d latest revision %d",
			checkpointGeneration, checkpointCount, checkpointRevision,
		)
	}
	beforeKill, err := openPhysicalJobRecord(
		fixture.storePath, fixture.configPath, fixture.job.JobID, fixture.options,
	)
	if err != nil {
		t.Fatal(err)
	}
	if beforeKill.Workload.State != scheduling.StateLeased ||
		beforeKill.Workload.Generation != 1 || beforeKill.Workload.ActiveLease == nil ||
		beforeKill.Run != nil {
		t.Fatalf("live pre-kill job authority = %+v", beforeKill)
	}
	// The verification checkpoint can be the worker's final progress event.
	// In that case Node may exit while the generation-owning ReviewJob process
	// is still blocked in the durable checkpoint callback. Child cleanup is
	// therefore best-effort; the required crash boundary is the leased worker
	// process whose unadmitted result must not reach terminal authority.
	children, _ := waitForChildProcessIDs(first.Process.Pid, time.Second)
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	processState, err := first.Process.Wait()
	firstWaited = true
	if err != nil {
		t.Fatalf("wait for physically killed live worker: %v", err)
	}
	if processState.Success() {
		t.Fatal("physically killed live worker reported a successful exit")
	}
	for _, pid := range children {
		process, findErr := os.FindProcess(pid)
		if findErr == nil {
			_ = process.Kill()
		}
	}

	expirePhysicalReviewJobLease(t, fixture.storePath, beforeKill)
	resumeMarker := marker + ".generation-2"
	second := livePhysicalReviewJobCommand(t, "resume", fixture, resumeMarker)
	secondOutput, err := second.CombinedOutput()
	if err != nil {
		diagnostic, diagnosticErr := openPhysicalJobRecord(
			fixture.storePath, fixture.configPath, fixture.job.JobID, fixture.options,
		)
		executionError, _ := os.ReadFile(resumeMarker + ".error")
		workerSummary, _ := os.ReadFile(resumeMarker + ".worker-summary.json")
		t.Fatalf(
			"live generation 2 worker failed: %v\nchild=%s\nexecutor=%s\nworker_summary=%s\nrecord=%s\nrecord_error=%v",
			err, secondOutput, executionError, workerSummary,
			physicalReviewJobDiagnostic(diagnostic), diagnosticErr,
		)
	}
	if err := waitForRegularFile(resumeMarker+".reused", 5*time.Second); err != nil {
		t.Fatalf("generation 2 did not report durable verifier reuse: %v\n%s", err, secondOutput)
	}
	terminal, err := openPhysicalJobRecord(
		fixture.storePath, fixture.configPath, fixture.job.JobID, fixture.options,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Workload.State != scheduling.StateSucceeded || terminal.Run == nil ||
		terminal.Run.Status != runmodel.RunStatusSucceeded || terminal.Workload.Generation != 2 {
		t.Fatalf("live restarted formal job did not succeed on generation 2: %+v", terminal)
	}
	physicalStore, err := local.Open(fixture.storePath)
	if err != nil {
		t.Fatal(err)
	}
	physicalScheduler, err := scheduling.NewRepository(physicalStore, physicalSchedulingPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lateRecord, err := physicalScheduler.Complete(t.Context(), scheduling.Callback{
		SchemaVersion:  scheduling.CallbackSchemaVersion,
		IdempotencyKey: "physical-live-restart-stale-generation-one-callback",
		LeaseID:        beforeKill.Workload.ActiveLease.LeaseID,
		WorkloadID:     beforeKill.Workload.ActiveLease.WorkloadID,
		WorkerID:       beforeKill.Workload.ActiveLease.WorkerID,
		Attempt:        beforeKill.Workload.ActiveLease.Attempt,
		Generation:     beforeKill.Workload.ActiveLease.Generation,
		FencingToken:   beforeKill.Workload.ActiveLease.FencingToken,
		Status:         scheduling.CallbackSucceeded,
		OutputRefs:     []string{"artifact://local/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		OccurredAt:     time.Now().UTC(),
	})
	if !errors.Is(err, scheduling.ErrFenced) {
		t.Fatalf("live generation 1 callback after generation 2 terminal error = %v", err)
	}
	if lateRecord.State != scheduling.StateSucceeded || lateRecord.Generation != 2 ||
		len(lateRecord.CallbackRejections) == 0 ||
		lateRecord.CallbackRejections[len(lateRecord.CallbackRejections)-1].Reason != scheduling.ReasonStaleGeneration {
		t.Fatalf("stale live callback changed generation 2 terminal authority: %+v", lateRecord)
	}
}

func TestFormalReviewJobRecoversAfterPhysicalWorkerProcessKill(t *testing.T) {
	if mode := os.Getenv(physicalReviewJobHelperEnvironment); mode != "" {
		runPhysicalReviewJobHelper(t, mode)
		return
	}
	fixture := preparePhysicalReviewJob(t, "restart")
	node, worker := fixture.node, fixture.worker
	storePath, configPath := fixture.storePath, fixture.configPath
	bootstrapOptions := fixture.options
	service, job := fixture.service, fixture.job
	marker := filepath.Join(t.TempDir(), "runner-entered")
	first := physicalReviewJobCommand(t, "block", storePath, configPath, fixture.sourceRunID, node, worker, marker)
	var firstOutput bytes.Buffer
	first.Stdout = &firstOutput
	first.Stderr = &firstOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitForRegularFile(marker, 20*time.Second); err != nil {
		_ = first.Process.Kill()
		_, _ = first.Process.Wait()
		diagnostic, diagnosticErr := openPhysicalJobRecord(
			storePath, configPath, job.JobID, bootstrapOptions,
		)
		executionError, _ := os.ReadFile(marker + ".error")
		t.Fatalf("first worker never entered provider runner: %v\nchild=%s\nexecutor=%s\nrecord=%+v\nrecord_error=%v",
			err, firstOutput.String(), executionError, diagnostic, diagnosticErr)
	}
	beforeKill, err := openPhysicalJobRecord(storePath, configPath, job.JobID, bootstrapOptions)
	if err != nil {
		t.Fatal(err)
	}
	if beforeKill.Workload.State != scheduling.StateLeased ||
		beforeKill.Workload.Generation != 1 || beforeKill.Run != nil {
		t.Fatalf("pre-kill job authority = %+v", beforeKill)
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	processState, err := first.Process.Wait()
	if err != nil {
		t.Fatalf("wait for physically killed worker: %v", err)
	}
	if processState.Success() {
		t.Fatal("physically killed worker reported a successful exit")
	}
	if beforeKill.Workload.ActiveLease == nil {
		t.Fatal("pre-kill workload lost its active lease")
	}
	physicalStore, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	physicalScheduler, err := scheduling.NewRepository(physicalStore, physicalSchedulingPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(beforeKill.Workload.ActiveLease.ExpiresAt); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-t.Context().Done():
			timer.Stop()
			t.Fatal(t.Context().Err())
		}
	}
	reconcileAt := time.Now().UTC()
	if reconcileAt.Before(beforeKill.Workload.ActiveLease.ExpiresAt) {
		reconcileAt = beforeKill.Workload.ActiveLease.ExpiresAt
	}
	if _, err := physicalScheduler.Reconcile(t.Context(), scheduling.Mutation{
		IdempotencyKey: "physical-restart-expire-lease", Actor: "physical-restart-test",
		Audit: "advance authoritative clock beyond killed worker lease",
		At:    reconcileAt,
	}); err != nil {
		t.Fatalf("expire killed worker lease: %v", err)
	}
	resumeMarker := marker + ".resumed"
	second := physicalReviewJobCommand(t, "resume", storePath, configPath, fixture.sourceRunID, node, worker, resumeMarker)
	secondOutput, err := second.CombinedOutput()
	if err != nil {
		diagnostic, diagnosticErr := openPhysicalJobRecord(
			storePath, configPath, job.JobID, bootstrapOptions,
		)
		executionError, _ := os.ReadFile(resumeMarker + ".error")
		t.Fatalf(
			"restart worker failed: %v\nchild=%s\nexecutor=%s\nrecord=%+v\nrecord_error=%v",
			err, secondOutput, executionError, diagnostic, diagnosticErr,
		)
	}
	if err := waitForRegularFile(resumeMarker, 5*time.Second); err != nil {
		t.Fatalf("generation 2 did not receive generation 1 Pi group checkpoint: %v", err)
	}
	terminal, err := openPhysicalJobRecord(storePath, configPath, job.JobID, bootstrapOptions)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Workload.State != scheduling.StateFailed || terminal.Run == nil ||
		terminal.Run.Status != runmodel.RunStatusFailed || terminal.Workload.Generation != 2 {
		t.Fatalf("restarted formal job did not close on generation 2: %+v", terminal)
	}
	lateRecord, err := physicalScheduler.Complete(t.Context(), scheduling.Callback{
		SchemaVersion:  scheduling.CallbackSchemaVersion,
		IdempotencyKey: "physical-restart-stale-generation-one-callback",
		LeaseID:        beforeKill.Workload.ActiveLease.LeaseID,
		WorkloadID:     beforeKill.Workload.ActiveLease.WorkloadID,
		WorkerID:       beforeKill.Workload.ActiveLease.WorkerID,
		Attempt:        beforeKill.Workload.ActiveLease.Attempt,
		Generation:     beforeKill.Workload.ActiveLease.Generation,
		FencingToken:   beforeKill.Workload.ActiveLease.FencingToken,
		Status:         scheduling.CallbackSucceeded,
		OutputRefs:     []string{"artifact://local/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		OccurredAt:     time.Now().UTC(),
	})
	if !errors.Is(err, scheduling.ErrFenced) {
		t.Fatalf("generation 1 callback after generation 2 terminal error = %v", err)
	}
	if lateRecord.State != scheduling.StateFailed || lateRecord.Generation != 2 ||
		len(lateRecord.CallbackRejections) == 0 ||
		lateRecord.CallbackRejections[len(lateRecord.CallbackRejections)-1].Reason != scheduling.ReasonStaleGeneration {
		t.Fatalf("stale callback changed recovered terminal authority: %+v", lateRecord)
	}
	timeline, err := service.Timeline(job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var expired, reclaimed, terminalCallback, staleCallbackRejected bool
	for _, event := range timeline {
		for _, action := range event.Reconciled {
			if action.Type == scheduling.ActionLeaseExpired {
				expired = true
			}
		}
		if event.Lease != nil && event.Lease.Generation == 2 {
			reclaimed = true
		}
		if event.Callback != nil && event.Callback.Callback.Generation == 2 &&
			event.Callback.Callback.Status == scheduling.CallbackFailed {
			terminalCallback = true
		}
		if event.Callback != nil && event.Callback.Callback.Generation == 1 &&
			event.Callback.Reason == scheduling.ReasonStaleGeneration {
			staleCallbackRejected = true
		}
	}
	if !expired || !reclaimed || !terminalCallback || !staleCallbackRejected {
		t.Fatalf("physical recovery timeline incomplete: %+v", timeline)
	}
}

func TestFormalReviewJobPropagatesCrossProcessCancellation(t *testing.T) {
	fixture := preparePhysicalReviewJob(t, "cross-process-cancel")
	marker := filepath.Join(t.TempDir(), "runner-entered")
	worker := physicalReviewJobCommand(
		t, "block", fixture.storePath, fixture.configPath, fixture.sourceRunID,
		fixture.node, fixture.worker, marker,
	)
	var workerOutput bytes.Buffer
	worker.Stdout = &workerOutput
	worker.Stderr = &workerOutput
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = worker.Process.Kill()
		_, _ = worker.Process.Wait()
	}()
	if err := waitForRegularFile(marker, 20*time.Second); err != nil {
		t.Fatalf("worker never entered provider runner: %v\n%s", err, workerOutput.String())
	}
	beforeCancel, err := openPhysicalJobRecord(
		fixture.storePath, fixture.configPath, fixture.job.JobID, fixture.options,
	)
	if err != nil {
		t.Fatal(err)
	}
	if beforeCancel.Workload.State != scheduling.StateLeased ||
		beforeCancel.Workload.ActiveLease == nil {
		t.Fatalf("pre-cancel workload is not actively leased: %+v", beforeCancel.Workload)
	}
	canceled, err := fixture.service.Cancel(t.Context(), fixture.job.JobID, reviewjob.CancelCommand{
		Reason: "prove cross-process cancellation propagation",
		Mutation: reviewjob.Mutation{
			IdempotencyKey: "physical-cross-process-cancel-command",
			Actor:          "physical-review-operator",
			Audit:          "cancel from a process without the worker active context",
			At:             time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatalf("cancel physical ReviewJob: %v", err)
	}
	if canceled.Workload.State != scheduling.StateCanceled || !canceled.Workload.RunCanceled {
		t.Fatalf("cancel did not establish a permanent workload fence: %+v", canceled.Workload)
	}
	if err := waitForRegularFile(marker+".canceled", 30*time.Second); err != nil {
		t.Fatalf("remote worker did not observe cancellation through its lease heartbeat: %v\n%s", err, workerOutput.String())
	}
	afterCancel, err := openPhysicalJobRecord(
		fixture.storePath, fixture.configPath, fixture.job.JobID, fixture.options,
	)
	if err != nil {
		t.Fatal(err)
	}
	if afterCancel.Workload.State != scheduling.StateCanceled ||
		!afterCancel.Workload.RunCanceled || afterCancel.Workload.ActiveLease != nil {
		t.Fatalf("worker cancellation changed permanent authority: %+v", afterCancel.Workload)
	}
	timeline, err := fixture.service.Timeline(fixture.job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var cancellationRecorded bool
	for _, event := range timeline {
		if event.Cancellation != nil {
			cancellationRecorded = true
		}
		if event.Callback != nil && event.Callback.Reason == "" {
			t.Fatalf("callback was accepted after permanent cancellation: %+v", event.Callback)
		}
	}
	if !cancellationRecorded {
		t.Fatalf("physical cancellation is absent from scheduling timeline: %+v", timeline)
	}
}

func physicalReviewJobCommand(
	t *testing.T,
	mode string,
	storePath string,
	configPath string,
	sourceRunID string,
	node string,
	worker string,
	marker string,
) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestFormalReviewJobRecoversAfterPhysicalWorkerProcessKill$")
	command.Env = append(os.Environ(),
		physicalReviewJobHelperEnvironment+"="+mode,
		"ARGUS_PHYSICAL_STORE="+storePath,
		"ARGUS_PHYSICAL_CONFIG="+configPath,
		"ARGUS_PHYSICAL_SOURCE_RUN="+sourceRunID,
		"ARGUS_PHYSICAL_NODE="+node,
		"ARGUS_PHYSICAL_WORKER="+worker,
		"ARGUS_PHYSICAL_MARKER="+marker,
	)
	return command
}

func livePhysicalReviewJobCommand(
	t *testing.T,
	mode string,
	fixture physicalReviewJobFixture,
	marker string,
) *exec.Cmd {
	t.Helper()
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestLiveFormalReviewJobRecoversDeepSeekCheckpointAfterPhysicalKill$",
	)
	command.Env = append(os.Environ(),
		physicalLiveHelperEnvironment+"="+mode,
		physicalLiveModeEnvironment+"=1",
		"ARGUS_PHYSICAL_STORE="+fixture.storePath,
		"ARGUS_PHYSICAL_CONFIG="+fixture.configPath,
		"ARGUS_PHYSICAL_SOURCE_RUN="+fixture.sourceRunID,
		"ARGUS_PHYSICAL_NODE="+fixture.node,
		"ARGUS_PHYSICAL_WORKER="+fixture.worker,
		"ARGUS_PHYSICAL_MARKER="+marker,
	)
	return command
}

func runLivePhysicalReviewJobHelper(t *testing.T, mode string) {
	options := physicalFormalOptions(
		os.Getenv("ARGUS_PHYSICAL_NODE"), os.Getenv("ARGUS_PHYSICAL_WORKER"),
	)
	runner := &physicalLiveObservingRunner{
		inner:  agentshadowworker.NewSubprocessRunner(),
		marker: os.Getenv("ARGUS_PHYSICAL_MARKER"),
	}
	switch mode {
	case "block-after-checkpoint":
		runner.blockAfterCheckpoint = true
	case "resume":
	default:
		t.Fatalf("unknown live helper mode %q", mode)
	}
	service, err := newPhysicalReviewJobService(
		os.Getenv("ARGUS_PHYSICAL_STORE"), os.Getenv("ARGUS_PHYSICAL_CONFIG"),
		"physical-live-"+mode+"-worker", runner, options,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if mode == "block-after-checkpoint" {
		select {}
	}
	deadline := time.Now().Add(210 * time.Second)
	for time.Now().Before(deadline) {
		records, listErr := service.List()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(records) == 1 && records[0].Workload.State == scheduling.StateSucceeded &&
			records[0].Run != nil && records[0].Run.Status == runmodel.RunStatusSucceeded {
			cancel()
			service.Wait()
			return
		}
		if len(records) == 1 {
			switch records[0].Workload.State {
			case scheduling.StateFailed, scheduling.StateCanceled,
				scheduling.StateRejected, scheduling.StateThrottled:
				t.Fatalf(
					"live restarted formal worker reached non-success terminal: %s",
					physicalReviewJobDiagnostic(records[0]),
				)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("live restarted formal worker did not reach a committed success terminal")
}

func physicalReviewJobDiagnostic(record reviewjob.Record) string {
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Sprintf("<diagnostic encoding failed: %v>", err)
	}
	return string(encoded)
}

type physicalLiveObservingRunner struct {
	inner                agentshadowworker.Runner
	marker               string
	blockAfterCheckpoint bool
}

func (runner *physicalLiveObservingRunner) Run(
	ctx context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	if runner == nil || runner.inner == nil || runner.marker == "" {
		return agentshadowworker.Result{}, fmt.Errorf("live physical runner is not configured")
	}
	original := request.ProgressLine
	request.ProgressLine = func(line []byte) error {
		if original != nil {
			if err := original(line); err != nil {
				return err
			}
		}
		var progress struct {
			Phase      string `json:"phase"`
			Message    string `json:"message"`
			Checkpoint *struct {
				CheckpointRevision uint64 `json:"checkpoint_revision"`
			} `json:"checkpoint"`
		}
		if err := json.Unmarshal(line, &progress); err != nil {
			return err
		}
		if strings.Contains(progress.Message, "reusing durable verification checkpoint") {
			if err := os.WriteFile(runner.marker+".reused", []byte("reused\n"), 0o600); err != nil {
				return err
			}
		}
		if runner.blockAfterCheckpoint && progress.Phase == "checkpoint" &&
			progress.Checkpoint != nil && progress.Checkpoint.CheckpointRevision > 0 {
			// The original callback has returned, so the checkpoint is already
			// fsynced by the host before this marker becomes observable.
			if err := os.WriteFile(runner.marker, []byte("checkpoint-durable\n"), 0o600); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	result, err := runner.inner.Run(ctx, request)
	summary, summaryErr := livePhysicalWorkerSummary(result)
	if summaryErr != nil {
		summary = []byte(fmt.Sprintf(
			`{"diagnostic_error":%q}`,
			summaryErr.Error(),
		))
	}
	if writeErr := os.WriteFile(
		runner.marker+".worker-summary.json",
		summary,
		0o600,
	); err == nil && writeErr != nil {
		err = writeErr
	}
	if runner.blockAfterCheckpoint {
		if writeErr := os.WriteFile(
			runner.marker+".worker-complete",
			[]byte("worker completed before verification checkpoint\n"),
			0o600,
		); err == nil && writeErr != nil {
			err = writeErr
		}
	}
	return result, err
}

func livePhysicalWorkerSummary(result agentshadowworker.Result) ([]byte, error) {
	workerResult, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(result.Stdout)
	if err != nil {
		return nil, err
	}
	type failure struct {
		GroupID     string `json:"groupId"`
		Phase       string `json:"phase"`
		SkillID     string `json:"skillId,omitempty"`
		CandidateID string `json:"candidateId,omitempty"`
		Error       string `json:"error"`
	}
	type task struct {
		TaskID        string `json:"taskId"`
		TaskKind      string `json:"taskKind"`
		GroupID       string `json:"groupId"`
		SkillID       string `json:"skillId,omitempty"`
		CandidateID   string `json:"candidateId,omitempty"`
		TerminalState string `json:"terminalStatus"`
		ErrorCode     string `json:"errorCode,omitempty"`
	}
	var report struct {
		Status  string `json:"status"`
		Summary struct {
			Candidates   int `json:"candidates"`
			Confirmed    int `json:"confirmed"`
			Rejected     int `json:"rejected"`
			Inconclusive int `json:"inconclusive"`
		} `json:"summary"`
		Coverage struct {
			GroupsTotal                int       `json:"groupsTotal"`
			GroupsReviewed             int       `json:"groupsReviewed"`
			ReviewTasksTotal           int       `json:"reviewTasksTotal"`
			ReviewTasksSucceeded       int       `json:"reviewTasksSucceeded"`
			VerificationTasksTotal     int       `json:"verificationTasksTotal"`
			VerificationTasksSucceeded int       `json:"verificationTasksSucceeded"`
			Failures                   []failure `json:"failures"`
		} `json:"coverage"`
		Candidates             []json.RawMessage `json:"candidates"`
		RawCandidates          []json.RawMessage `json:"rawCandidates"`
		NormalizationDecisions []json.RawMessage `json:"normalizationDecisions"`
		Execution              struct {
			Tasks        []task `json:"tasks"`
			TaskEvidence struct {
				Completeness string            `json:"completeness"`
				ReasonCodes  []string          `json:"reasonCodes"`
				Tasks        []json.RawMessage `json:"tasks"`
			} `json:"taskEvidence"`
		} `json:"execution"`
	}
	if len(workerResult.Report) > 0 {
		if err := json.Unmarshal(workerResult.Report, &report); err != nil {
			return nil, err
		}
	}
	summary := struct {
		WorkerStatus               contractsv1alpha1.AgentReviewWorkerStatus   `json:"worker_status"`
		WorkerFailure              *contractsv1alpha1.AgentReviewWorkerFailure `json:"worker_failure,omitempty"`
		ReportStatus               string                                      `json:"report_status,omitempty"`
		Summary                    any                                         `json:"summary,omitempty"`
		Coverage                   any                                         `json:"coverage,omitempty"`
		RawCandidates              int                                         `json:"raw_candidates"`
		NormalizedCandidates       int                                         `json:"normalized_candidates"`
		NormalizationDecisionCount int                                         `json:"normalization_decisions"`
		Tasks                      []task                                      `json:"tasks,omitempty"`
		TaskEvidenceCompleteness   string                                      `json:"task_evidence_completeness,omitempty"`
		TaskEvidenceReasonCodes    []string                                    `json:"task_evidence_reason_codes,omitempty"`
		TaskEvidenceCount          int                                         `json:"task_evidence_count"`
	}{
		WorkerStatus: workerResult.Status, WorkerFailure: workerResult.Failure,
		ReportStatus: report.Status, Summary: report.Summary, Coverage: report.Coverage,
		RawCandidates: len(report.RawCandidates), NormalizedCandidates: len(report.Candidates),
		NormalizationDecisionCount: len(report.NormalizationDecisions),
		Tasks:                      report.Execution.Tasks,
		TaskEvidenceCompleteness:   report.Execution.TaskEvidence.Completeness,
		TaskEvidenceReasonCodes:    report.Execution.TaskEvidence.ReasonCodes,
		TaskEvidenceCount:          len(report.Execution.TaskEvidence.Tasks),
	}
	return json.MarshalIndent(summary, "", "  ")
}

func inspectPhysicalCheckpointLedger(storePath string) (int, int, uint64, error) {
	paths, err := filepath.Glob(filepath.Join(
		storePath, "streams", "pi", "group-checkpoints", "*.jsonl",
	))
	if err != nil {
		return 0, 0, 0, err
	}
	if len(paths) != 1 {
		return 0, 0, 0, fmt.Errorf("physical checkpoint stream count = %d", len(paths))
	}
	content, err := os.ReadFile(paths[0])
	if err != nil {
		return 0, 0, 0, err
	}
	generation, count := 0, 0
	var latestRevision uint64
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		var envelope local.Envelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			return 0, 0, 0, fmt.Errorf("decode physical checkpoint envelope: %w", err)
		}
		var event struct {
			Type               string `json:"type"`
			Generation         int    `json:"generation"`
			CheckpointRevision uint64 `json:"checkpoint_revision"`
		}
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return 0, 0, 0, fmt.Errorf("decode physical checkpoint event: %w", err)
		}
		if event.Type == "checkpoint_recorded" {
			if count > 0 && event.Generation != generation {
				return 0, 0, 0, fmt.Errorf("physical checkpoints span multiple generations")
			}
			generation = event.Generation
			count++
			latestRevision = event.CheckpointRevision
		}
	}
	return generation, count, latestRevision, nil
}

func waitForChildProcessIDs(parentPID int, timeout time.Duration) ([]int, error) {
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		return nil, fmt.Errorf("pgrep is required for physical subprocess cleanup: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, commandErr := exec.Command(pgrep, "-P", strconv.Itoa(parentPID)).Output()
		if commandErr == nil {
			fields := strings.Fields(string(output))
			result := make([]int, 0, len(fields))
			for _, field := range fields {
				pid, parseErr := strconv.Atoi(field)
				if parseErr != nil || pid <= 0 {
					return nil, fmt.Errorf("invalid child process id %q", field)
				}
				result = append(result, pid)
			}
			if len(result) > 0 {
				return result, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out locating a child of process %d", parentPID)
}

func expirePhysicalReviewJobLease(t *testing.T, storePath string, record reviewjob.Record) {
	t.Helper()
	if record.Workload.ActiveLease == nil {
		t.Fatal("physical workload has no active lease")
	}
	physicalStore, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	physicalScheduler, err := scheduling.NewRepository(physicalStore, physicalSchedulingPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(record.Workload.ActiveLease.ExpiresAt); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-t.Context().Done():
			timer.Stop()
			t.Fatal(t.Context().Err())
		}
	}
	reconcileAt := time.Now().UTC()
	if reconcileAt.Before(record.Workload.ActiveLease.ExpiresAt) {
		reconcileAt = record.Workload.ActiveLease.ExpiresAt
	}
	if _, err := physicalScheduler.Reconcile(t.Context(), scheduling.Mutation{
		IdempotencyKey: "physical-live-restart-expire-lease",
		Actor:          "physical-live-restart-test",
		Audit:          "advance authoritative clock beyond killed live worker lease",
		At:             reconcileAt,
	}); err != nil {
		t.Fatalf("expire killed live worker lease: %v", err)
	}
}

func runPhysicalReviewJobHelper(t *testing.T, mode string) {
	options := physicalFormalOptions(
		os.Getenv("ARGUS_PHYSICAL_NODE"), os.Getenv("ARGUS_PHYSICAL_WORKER"),
	)
	var runner agentshadowworker.Runner
	switch mode {
	case "block":
		runner = physicalBlockingRunner{marker: os.Getenv("ARGUS_PHYSICAL_MARKER")}
	case "resume":
		runner = physicalCheckpointResumeRunner{marker: os.Getenv("ARGUS_PHYSICAL_MARKER")}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	service, err := newPhysicalReviewJobService(
		os.Getenv("ARGUS_PHYSICAL_STORE"), os.Getenv("ARGUS_PHYSICAL_CONFIG"),
		"physical-"+mode+"-worker", runner, options,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if mode == "block" {
		select {}
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		records, listErr := service.List()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(records) == 1 && records[0].Workload.State == scheduling.StateFailed &&
			records[0].Run != nil && records[0].Run.Status == runmodel.RunStatusFailed {
			cancel()
			service.Wait()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("restarted formal worker did not reach a committed failed terminal")
}

type physicalBlockingRunner struct{ marker string }

func (runner physicalBlockingRunner) Run(
	ctx context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	if runner.marker == "" {
		return agentshadowworker.Result{}, fmt.Errorf("physical runner marker is required")
	}
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(request.Input)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	content := []byte(fmt.Sprintf(
		`{"schemaVersion":"argus.pi-review.group_checkpoint.v1","checkpointRevision":0,"checkpointScopeSha256":%q,"group":{"id":"group-001-physical"}}`,
		workerRequest.CheckpointScopeSHA256,
	))
	digest := sha256.Sum256(content)
	checkpoint := contractsv1alpha1.AgentReviewWorkerGroupCheckpoint{
		GroupID: "group-001-physical", CheckpointScopeSHA256: workerRequest.CheckpointScopeSHA256,
		ContentSHA256: fmt.Sprintf("%x", digest[:]), SizeBytes: uint64(len(content)),
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
	progress, err := json.Marshal(map[string]any{
		"schema_version": "argus.agent_review_worker_progress.v1alpha1",
		"work_item_id":   workerRequest.WorkItemID, "phase": "checkpoint",
		"message": "physical group checkpoint", "observed_at": time.Now().UTC(),
		"group_id": checkpoint.GroupID, "checkpoint": checkpoint,
	})
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	if request.ProgressLine == nil {
		return agentshadowworker.Result{}, fmt.Errorf("physical runner checkpoint callback is unavailable")
	}
	if err := request.ProgressLine(progress); err != nil {
		return agentshadowworker.Result{}, err
	}
	if err := os.WriteFile(runner.marker, []byte("entered\n"), 0o600); err != nil {
		return agentshadowworker.Result{}, err
	}
	<-ctx.Done()
	if err := os.WriteFile(runner.marker+".canceled", []byte("canceled\n"), 0o600); err != nil {
		return agentshadowworker.Result{}, err
	}
	return agentshadowworker.Result{}, ctx.Err()
}

type physicalCheckpointResumeRunner struct{ marker string }

func (runner physicalCheckpointResumeRunner) Run(
	ctx context.Context,
	request agentshadowworker.Request,
) (agentshadowworker.Result, error) {
	workerRequest, err := contractsv1alpha1.DecodeAgentReviewWorkerRequest(request.Input)
	if err != nil {
		return agentshadowworker.Result{}, err
	}
	if len(workerRequest.GroupCheckpoints) != 1 ||
		workerRequest.GroupCheckpoints[0].GroupID != "group-001-physical" {
		return agentshadowworker.Result{}, fmt.Errorf(
			"generation 2 worker received checkpoints %+v", workerRequest.GroupCheckpoints,
		)
	}
	if runner.marker != "" {
		if err := os.WriteFile(runner.marker, []byte("resumed\n"), 0o600); err != nil {
			return agentshadowworker.Result{}, err
		}
	}
	return (&formalFailedRunner{}).Run(ctx, request)
}

func newPhysicalReviewJobService(
	storePath string,
	configPath string,
	workerID string,
	runner agentshadowworker.Runner,
	formalOptions formalreview.LocalPiBootstrapOptions,
) (*reviewjob.Service, error) {
	store, err := local.Open(storePath)
	if err != nil {
		return nil, err
	}
	configsStore, err := local.Open(configPath)
	if err != nil {
		return nil, err
	}
	configs, err := configrepo.New(configsStore)
	if err != nil {
		return nil, err
	}
	jobs, err := reviewjob.NewRepository(store)
	if err != nil {
		return nil, err
	}
	workloads, err := scheduling.NewRepository(store, physicalSchedulingPolicy())
	if err != nil {
		return nil, err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, err
	}
	admission, err := newLocalFormalAdmissionValidator(store)
	if err != nil {
		return nil, err
	}
	localExecutor := &localReviewJobExecutor{
		deterministic: reviewjob.ExecuteFunc(func(
			context.Context, reviewjob.Command,
		) (application.RunOutcome, error) {
			return application.RunOutcome{}, errors.New("deterministic executor is unavailable")
		}),
		runner: runner, storePath: storePath, workloads: workloads,
	}
	var executor reviewjob.Executor = localExecutor
	if diagnosticPath := os.Getenv("ARGUS_PHYSICAL_MARKER"); diagnosticPath != "" {
		executor = &physicalDiagnosticExecutor{inner: localExecutor, errorPath: diagnosticPath + ".error"}
	}
	service, err := reviewjob.NewService(
		jobs, workloads, runs, configs, executor, workerID, time.Now,
	)
	if err != nil {
		return nil, err
	}
	if err := service.ConfigureFormalProfile(reviewjob.FormalProfile{
		Options: formalOptions,
		Pricing: piexecution.PricingCeiling{
			InputMicrosPerMillionTokens: 1, OutputMicrosPerMillionTokens: 1,
			MaximumBytesPerInputToken: 4,
		},
		Admission: admission,
	}); err != nil {
		return nil, err
	}
	return service, nil
}

type physicalDiagnosticExecutor struct {
	inner     *localReviewJobExecutor
	errorPath string
}

func (executor *physicalDiagnosticExecutor) Execute(
	ctx context.Context,
	command reviewjob.Command,
) (application.RunOutcome, error) {
	return executor.inner.Execute(ctx, command)
}

func (executor *physicalDiagnosticExecutor) CanResumeNonterminal(command reviewjob.Command) bool {
	return executor.inner.CanResumeNonterminal(command)
}

func (executor *physicalDiagnosticExecutor) ExecuteClaimed(
	ctx context.Context,
	command reviewjob.Command,
	dispatch scheduling.Dispatch,
) (application.RunOutcome, error) {
	outcome, err := executor.inner.ExecuteClaimed(ctx, command, dispatch)
	if err != nil {
		_ = os.WriteFile(executor.errorPath, []byte(err.Error()), 0o600)
	}
	return outcome, err
}

func openPhysicalJobRecord(
	storePath string,
	configPath string,
	jobID string,
	formalOptions formalreview.LocalPiBootstrapOptions,
) (reviewjob.Record, error) {
	service, err := newPhysicalReviewJobService(
		storePath, configPath, "physical-observer", &formalFailedRunner{}, formalOptions,
	)
	if err != nil {
		return reviewjob.Record{}, err
	}
	return service.Get(jobID)
}

func physicalFormalOptions(node string, worker string) formalreview.LocalPiBootstrapOptions {
	options := formalreview.LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
		MaxFiles: 8, MaxGroups: 2, MaxHypotheses: 8,
		MaxModelCalls: 16, MaxToolCalls: 8,
		MaxTargetBytes: 16 << 20, MaxGroupBytes: 64 << 10,
		MaxOutputBytes: 1 << 20, MaxOutputTokens: 2048,
		MaxCostMicros: 1_000_000, TimeoutMS: 30_000, MaxConcurrency: 2,
	}
	if os.Getenv(physicalLiveModeEnvironment) == "1" {
		options.Model = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
		options.MaxGroups = 6
		options.MaxModelCalls = 20
		options.TimeoutMS = 180_000
		options.MaxConcurrency = 4
	}
	return options
}

func physicalSchedulingPolicy() scheduling.Policy {
	policy := scheduling.DefaultLocalPolicy()
	policy.Revision = "physical-restart-test-1"
	policy.AgingInterval = 100 * time.Millisecond
	policy.AdmissionTimeout = 90 * time.Second
	// The StageExecutionRequest deadline is capped by the current lease. Five
	// seconds is too short for a child Go test process to start under a loaded
	// full-suite run. The test waits for this lease to expire in wall-clock time
	// before generation 2 starts, preserving monotonic scheduling time.
	policy.LeaseDuration = 30 * time.Second
	policy.UnknownTimeout = time.Second
	if os.Getenv(physicalLiveModeEnvironment) == "1" {
		policy.Revision = "physical-live-restart-test-2"
		policy.AdmissionTimeout = 7 * time.Minute
		// The live checkpoint now waits through one real verifier in addition
		// to context and six review dimensions. Keep the immutable stage
		// deadline below the lease while leaving enough room for the local Pi
		// adapter's bounded callback-admission reserve.
		policy.LeaseDuration = 185 * time.Second
	}
	return policy
}

func waitForRegularFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", strings.TrimSpace(path))
}

func waitForRegularFileOrExecutorError(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if failure, failureErr := os.ReadFile(path + ".error"); failureErr == nil {
			return fmt.Errorf("executor stopped before marker: %s", strings.TrimSpace(string(failure)))
		} else if !errors.Is(failureErr, os.ErrNotExist) {
			return failureErr
		}
		if completed, completedErr := os.ReadFile(path + ".worker-complete"); completedErr == nil {
			// The runner emits this just before the formal mapper and terminal
			// admission execute. Give that bounded local path time to publish its
			// richer failure before returning the worker-level diagnostic.
			settleDeadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(settleDeadline) {
				if failure, failureErr := os.ReadFile(path + ".error"); failureErr == nil {
					return fmt.Errorf(
						"executor stopped before marker: %s",
						strings.TrimSpace(string(failure)),
					)
				}
				time.Sleep(20 * time.Millisecond)
			}
			summary, _ := os.ReadFile(path + ".worker-summary.json")
			return fmt.Errorf(
				"%s; summary=%s",
				strings.TrimSpace(string(completed)),
				strings.TrimSpace(string(summary)),
			)
		} else if !errors.Is(completedErr, os.ErrNotExist) {
			return completedErr
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", strings.TrimSpace(path))
}
