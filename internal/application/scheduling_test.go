package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
)

func TestNewServiceRequiresCoordinatorOrExplicitSchedulingDisable(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(source, runs, ServiceOptions{}); err == nil ||
		!strings.Contains(err.Error(), "workload coordinator is required") {
		t.Fatalf("NewService(no scheduling decision) error = %v", err)
	}
	if _, err := NewService(source, runs, ServiceOptions{
		DisableScheduling: true,
	}); err != nil {
		t.Fatalf("NewService(explicit disable) error = %v", err)
	}
	workloads, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(source, runs, ServiceOptions{
		Workloads:         workloads,
		WorkerID:          "test-worker",
		DisableScheduling: true,
	}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("NewService(conflicting scheduling mode) error = %v", err)
	}
}

func TestServiceCoordinatesSuccessfulRunThroughDurableWorkload(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, runs, workloads := newScheduledTestService(
		t,
		scheduling.DefaultLocalPolicy(),
		ServiceOptions{},
	)
	outcome, err := service.Review(t.Context(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	record, err := workloads.Get(outcome.Run.RunID + "-workload")
	if err != nil {
		t.Fatalf("Get(workload) error = %v", err)
	}
	snapshot, err := runs.LoadExecutionSnapshot(outcome.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	if record.Spec.RunID != outcome.Run.RunID ||
		record.Spec.TenantID != localTenantID ||
		record.Spec.Class != scheduling.ClassIncrementalMR ||
		record.Spec.InputRef != snapshot.ReviewInputRef.URI ||
		record.Spec.ExecutionDeadline.Sub(record.Spec.SubmittedAt) !=
			defaultRunWorkloadTimeout {
		t.Fatalf("frozen workload spec = %+v", record.Spec)
	}
	if record.Admission.Decision != scheduling.AdmissionAdmitted ||
		record.State != scheduling.StateSucceeded ||
		record.LastLease == nil ||
		record.Terminal == nil ||
		record.Terminal.Status != scheduling.CallbackSucceeded ||
		len(record.Terminal.OutputRefs) != 1 ||
		record.Terminal.OutputRefs[0] != outcome.FinalRef.URI {
		t.Fatalf("terminal workload projection = %+v", record)
	}
	for _, binding := range outcome.Run.Bindings {
		if binding.FencingToken != record.LastLease.FencingToken ||
			binding.RuntimeKind != "local-run-coordinator" ||
			binding.RuntimeID != record.LastLease.WorkerID+"-"+record.LastLease.LeaseID {
			t.Fatalf("stage binding is not derived from run lease: %+v", binding)
		}
	}

	// Terminal recording is safe to retry because callback identity and time
	// are derived from the immutable terminal run.
	workload := &runWorkload{dispatch: scheduling.Dispatch{
		Spec:  record.Spec,
		Lease: *record.LastLease,
	}}
	if err := service.recordWorkloadTerminal(outcome, workload); err != nil {
		t.Fatalf("recordWorkloadTerminal(idempotent) error = %v", err)
	}
	again, err := workloads.Get(record.Spec.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Terminal == nil ||
		again.Terminal.CallbackID != record.Terminal.CallbackID ||
		len(again.CallbackRejections) != 0 {
		t.Fatalf("idempotent terminal projection changed: %+v", again)
	}

	replay, err := service.Replay(t.Context(), ReplayRequest{
		SourceRunID: outcome.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	replayRecord, err := workloads.Get(replay.Run.RunID + "-workload")
	if err != nil {
		t.Fatalf("Get(replay workload) error = %v", err)
	}
	replaySnapshot, err := runs.LoadExecutionSnapshot(replay.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if replayRecord.Spec.Class != scheduling.ClassEvalReplay ||
		replayRecord.Spec.InputRef != replaySnapshot.ReviewInputRef.URI ||
		replayRecord.State != scheduling.StateSucceeded {
		t.Fatalf("replay workload projection = %+v", replayRecord)
	}
}

func TestServiceDoesNotExecuteRejectedOrThrottledWorkload(t *testing.T) {
	tests := []struct {
		name         string
		mutatePolicy func(*scheduling.Policy)
		wantDecision scheduling.AdmissionDecision
		wantCode     string
	}{
		{
			name: "rejected",
			mutatePolicy: func(policy *scheduling.Policy) {
				policy.GlobalQueueLimit = 1
				for index := range policy.Classes {
					policy.Classes[index].QueueLimit = 1
				}
			},
			wantDecision: scheduling.AdmissionRejected,
			wantCode:     "scheduling_admission_rejected",
		},
		{
			name: "throttled",
			mutatePolicy: func(policy *scheduling.Policy) {
				policy.DefaultTenantQuota.MaxQueued = 1
			},
			wantDecision: scheduling.AdmissionThrottled,
			wantCode:     "scheduling_admission_throttled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositoryPath, base, head := reviewFixture(t)
			policy := scheduling.DefaultLocalPolicy()
			test.mutatePolicy(&policy)
			service, runs, workloads := newScheduledTestService(
				t,
				policy,
				ServiceOptions{},
			)
			fillerAt := time.Date(2026, time.July, 26, 12, 59, 0, 0, time.UTC)
			_, err := workloads.Submit(t.Context(), scheduling.WorkloadSpec{
				SchemaVersion:     scheduling.WorkloadSchemaVersion,
				WorkloadID:        "queue-filler",
				RunID:             "queue-filler-run",
				TenantID:          localTenantID,
				Class:             scheduling.ClassIncrementalMR,
				InputRef:          "artifact://local/queue-filler",
				SubmittedAt:       fillerAt,
				ExecutionDeadline: fillerAt.Add(time.Hour),
			}, scheduling.Mutation{
				IdempotencyKey: "queue-filler-submit",
				Actor:          "test-worker",
				Audit:          "fill bounded queue",
				At:             fillerAt,
			})
			if err != nil {
				t.Fatalf("Submit(filler) error = %v", err)
			}

			outcome, err := service.Review(t.Context(), ReviewRequest{
				RepositoryPath: repositoryPath,
				BaseRevision:   base,
				HeadRevision:   head,
			})
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Failure.Code != test.wantCode {
				t.Fatalf("Review() error = %v, want %s RunError", err, test.wantCode)
			}
			if outcome.Run.Status != runmodel.RunStatusFailed ||
				len(outcome.Run.StageAttempts) != 0 ||
				len(outcome.Run.Bindings) != 0 {
				t.Fatalf("rejected run executed stages: %+v", outcome.Run)
			}
			record, getErr := workloads.Get(outcome.Run.RunID + "-workload")
			if getErr != nil {
				t.Fatalf("Get(workload) error = %v", getErr)
			}
			if record.Admission.Decision != test.wantDecision ||
				record.ActiveLease != nil {
				t.Fatalf("admission projection = %+v", record)
			}
			persisted, loadErr := runs.LoadRun(outcome.Run.RunID)
			if loadErr != nil || persisted.Status != runmodel.RunStatusFailed {
				t.Fatalf("persisted rejected run = %+v error=%v", persisted, loadErr)
			}
		})
	}
}

func TestServiceCancellationPermanentlyFencesRunWorkload(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	var scheduled []reviewcore.StageName
	service, _, workloads := newScheduledTestService(
		t,
		scheduling.DefaultLocalPolicy(),
		ServiceOptions{
			ExecutorIdentity: "scheduled-cancel-executor",
			BuildIdentity:    "argus-scheduled-cancel-test",
			ExecutorCapabilities: map[string]workflow.ExecutorCapabilities{
				"deterministic-local": {},
			},
			ExecuteStage: func(
				ctx context.Context,
				input reviewcore.ReviewInput,
				stage reviewcore.StageName,
				upstream reviewcore.StageResult,
				policy reviewcore.RuntimePolicy,
			) (reviewcore.StageResult, error) {
				scheduled = append(scheduled, stage)
				if stage == reviewcore.StageVerify {
					return reviewcore.StageResult{}, context.Canceled
				}
				return reviewcore.ExecuteStageWithPolicy(
					ctx,
					input,
					stage,
					upstream,
					policy,
				)
			},
		},
	)
	outcome, err := service.Review(t.Context(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Failure.Code != "canceled" {
		t.Fatalf("Review() error = %v, want canceled RunError", err)
	}
	record, getErr := workloads.Get(outcome.Run.RunID + "-workload")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if record.State != scheduling.StateCanceled ||
		!record.RunCanceled ||
		record.ActiveLease != nil {
		t.Fatalf("canceled workload projection = %+v", record)
	}
	if len(scheduled) != 5 || scheduled[len(scheduled)-1] != reviewcore.StageVerify {
		t.Fatalf("stages continued after cancellation: %v", scheduled)
	}
}

func TestServiceFailureCallbackMatchesTerminalRun(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, _, workloads := newScheduledTestService(
		t,
		scheduling.DefaultLocalPolicy(),
		ServiceOptions{
			ExecutorIdentity: "scheduled-failure-executor",
			BuildIdentity:    "argus-scheduled-failure-test",
			ExecutorCapabilities: map[string]workflow.ExecutorCapabilities{
				"deterministic-local": {},
			},
			ExecuteStage: func(
				ctx context.Context,
				input reviewcore.ReviewInput,
				stage reviewcore.StageName,
				upstream reviewcore.StageResult,
				policy reviewcore.RuntimePolicy,
			) (reviewcore.StageResult, error) {
				if stage == reviewcore.StageDetect {
					return reviewcore.StageResult{}, errors.New("detector failed permanently")
				}
				return reviewcore.ExecuteStageWithPolicy(
					ctx,
					input,
					stage,
					upstream,
					policy,
				)
			},
		},
	)
	outcome, err := service.Review(t.Context(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	var runErr *RunError
	if !errors.As(err, &runErr) || outcome.Run.Status != runmodel.RunStatusFailed {
		t.Fatalf("Review() outcome=%+v error=%v", outcome.Run, err)
	}
	record, getErr := workloads.Get(outcome.Run.RunID + "-workload")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if record.State != scheduling.StateFailed ||
		record.Terminal == nil ||
		record.Terminal.Status != scheduling.CallbackFailed ||
		record.Terminal.FailureCode != runErr.Failure.Code ||
		record.Terminal.RecordedAt != *outcome.Run.CompletedAt {
		t.Fatalf("failed workload projection = %+v run failure=%+v", record, runErr.Failure)
	}
}

func TestServiceRejectsStaleGenerationBeforeAcceptingStageOutput(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	policy := scheduling.DefaultLocalPolicy()
	policy.LeaseDuration = 2 * time.Millisecond
	workloads, err := scheduling.NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	clock := &incrementingClock{
		value: time.Date(2026, time.July, 26, 13, 0, 0, 0, time.UTC),
	}
	var firstDispatch scheduling.Dispatch
	var staleRecord scheduling.WorkloadRecord
	var staleCallbackErr error
	service, err := NewService(source, runs, ServiceOptions{
		IDs:              &sequenceIDs{},
		Now:              clock.Now,
		Workloads:        workloads,
		WorkerID:         "argus-local-test-worker",
		ExecutorIdentity: "stale-generation-executor",
		BuildIdentity:    "argus-stale-generation-test",
		ExecutorCapabilities: map[string]workflow.ExecutorCapabilities{
			"deterministic-local": {},
		},
		ExecuteStage: func(
			ctx context.Context,
			input reviewcore.ReviewInput,
			stage reviewcore.StageName,
			upstream reviewcore.StageResult,
			runtimePolicy reviewcore.RuntimePolicy,
		) (reviewcore.StageResult, error) {
			records, listErr := workloads.List()
			if listErr != nil {
				return reviewcore.StageResult{}, listErr
			}
			if len(records) != 1 || records[0].ActiveLease == nil {
				return reviewcore.StageResult{}, errors.New(
					"expected one active run workload",
				)
			}
			record := records[0]
			firstDispatch = scheduling.Dispatch{
				Spec:  record.Spec,
				Lease: *record.ActiveLease,
			}
			expiredAt := record.ActiveLease.ExpiresAt
			if _, reconcileErr := workloads.Reconcile(ctx, scheduling.Mutation{
				IdempotencyKey: "force-expire-generation-1",
				Actor:          "test-reconciler",
				Audit:          "force stale generation",
				At:             expiredAt,
			}); reconcileErr != nil {
				return reviewcore.StageResult{}, reconcileErr
			}
			if _, claimErr := workloads.Claim(ctx, scheduling.ClaimRequest{
				IdempotencyKey:   "claim-generation-2",
				WorkloadID:       record.Spec.WorkloadID,
				WorkerID:         "replacement-worker",
				SupportedClasses: []scheduling.WorkloadClass{record.Spec.Class},
				At:               expiredAt,
			}); claimErr != nil {
				return reviewcore.StageResult{}, claimErr
			}
			staleRecord, staleCallbackErr = workloads.Complete(ctx, scheduling.Callback{
				SchemaVersion:  scheduling.CallbackSchemaVersion,
				IdempotencyKey: "stale-generation-callback",
				LeaseID:        firstDispatch.Lease.LeaseID,
				WorkloadID:     firstDispatch.Lease.WorkloadID,
				WorkerID:       firstDispatch.Lease.WorkerID,
				Attempt:        firstDispatch.Lease.Attempt,
				Generation:     firstDispatch.Lease.Generation,
				FencingToken:   firstDispatch.Lease.FencingToken,
				Status:         scheduling.CallbackSucceeded,
				OutputRefs:     []string{"artifact://local/stale-output"},
				OccurredAt:     expiredAt,
			})
			return reviewcore.ExecuteStageWithPolicy(
				ctx,
				input,
				stage,
				upstream,
				runtimePolicy,
			)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Review(t.Context(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	var runErr *RunError
	if !errors.As(err, &runErr) ||
		runErr.Failure.Code != "scheduling_lease_fenced" {
		t.Fatalf("Review() error = %v, want scheduling_lease_fenced", err)
	}
	if outcome.Run.Status != runmodel.RunStatusCanceled ||
		len(outcome.Run.StageAttempts) != 1 ||
		outcome.Run.StageAttempts[0].Status != runmodel.StageStatusCanceled ||
		len(outcome.Run.Evidence) != 0 {
		t.Fatalf("stale generation output was accepted: %+v", outcome.Run)
	}
	if len(staleRecord.CallbackRejections) != 1 ||
		!errors.Is(staleCallbackErr, scheduling.ErrFenced) ||
		staleRecord.CallbackRejections[0].Reason != scheduling.ReasonStaleGeneration {
		t.Fatalf(
			"stale callback projection = %+v error=%v",
			staleRecord,
			staleCallbackErr,
		)
	}
	finalRecord, getErr := workloads.Get(outcome.Run.RunID + "-workload")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if finalRecord.State != scheduling.StateCanceled || !finalRecord.RunCanceled {
		t.Fatalf("stale generation run was not permanently fenced: %+v", finalRecord)
	}
}

func newScheduledTestService(
	t *testing.T,
	policy scheduling.Policy,
	options ServiceOptions,
) (*Service, *runrepo.Repository, *scheduling.Repository) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	workloads, err := scheduling.NewRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	if options.IDs == nil {
		options.IDs = &sequenceIDs{}
	}
	if options.Now == nil {
		clock := &incrementingClock{
			value: time.Date(2026, time.July, 26, 13, 0, 0, 0, time.UTC),
		}
		options.Now = clock.Now
	}
	options.Workloads = workloads
	options.WorkerID = "argus-local-test-worker"
	service, err := NewService(source, runs, options)
	if err != nil {
		t.Fatal(err)
	}
	return service, runs, workloads
}
