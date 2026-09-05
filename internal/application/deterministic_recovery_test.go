package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/workflow"
)

func TestResumeReviewRecoversFrozenDiffAndSelection(t *testing.T) {
	for _, mode := range []reviewcore.TargetMode{reviewcore.TargetModeDiff, reviewcore.TargetModeSelection} {
		for _, createdOnly := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/created-only-%t", mode, createdOnly), func(t *testing.T) {
				first, runs, request, runID, snapshot, bundle := interruptedReviewFixture(t, mode, createdOnly)
				before, err := runs.Events(runID)
				if err != nil {
					t.Fatal(err)
				}
				if createdOnly && len(before) != 1 {
					t.Fatalf("created-only fixture has %d events", len(before))
				}
				// HEAD and worktree can change between processes. The replacement
				// source panics on every call, proving recovery uses frozen artifacts.
				writeFixture(t, request.RepositoryPath, "review.go", "package fixture\nfunc Changed() {}\n")
				commitFixture(t, request.RepositoryPath, "changed after durable review admission")
				dispatch := recoveryTestDispatch(runID, snapshot.ReviewInputRef.URI)
				second, err := NewService(panicTargetSource{}, runs, ServiceOptions{
					ConfigBundle: bundle, BuildIdentity: first.buildIdentity,
					DisableScheduling: true, ExecutionDispatch: &dispatch,
				})
				if err != nil {
					t.Fatal(err)
				}
				outcome, err := second.ResumeReview(t.Context(), runID, request.RepositoryPath)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Run.Status != runmodel.RunStatusSucceeded || outcome.Report == nil ||
					outcome.Report.Summary.Findings != 1 || outcome.Run.ExecutionSnapshotID != snapshot.ExecutionSnapshotID {
					t.Fatalf("recovered outcome = %+v", outcome)
				}
				if !createdOnly && len(outcome.Reused) != 3 {
					t.Fatalf("recovered prefix = %v, want three successful stages", outcome.Reused)
				}
				for _, attempt := range outcome.Run.StageAttempts {
					if createdOnly && attempt.Generation < 2 {
						t.Fatalf("new attempt lost current dispatch generation: %+v", attempt)
					}
				}
				if _, err := runs.LoadCommittedRunResult(runID); err != nil {
					t.Fatalf("recovered run failed closure verification: %v", err)
				}
			})
		}
	}
}

func TestResumeReviewRejectsChangedAuthorityBeforeRepairingCreatedLifecycle(t *testing.T) {
	for _, changed := range []string{"config", "build", "dispatch", "canceled"} {
		t.Run(changed, func(t *testing.T) {
			first, runs, request, runID, snapshot, bundle := interruptedReviewFixture(t, reviewcore.TargetModeSelection, true)
			before, err := runs.Events(runID)
			if err != nil {
				t.Fatal(err)
			}
			dispatch := recoveryTestDispatch(runID, snapshot.ReviewInputRef.URI)
			options := ServiceOptions{ConfigBundle: bundle, BuildIdentity: first.buildIdentity,
				DisableScheduling: true, ExecutionDispatch: &dispatch}
			ctx := t.Context()
			switch changed {
			case "config":
				options.ConfigBundle.Context.InvocationID = "other-run"
				digest, err := reviewconfig.DigestBundle(options.ConfigBundle)
				if err != nil {
					t.Fatal(err)
				}
				options.ConfigBundle.SHA256 = digest
				options.ConfigBundle.BundleID = "bundle-" + digest[:24]
			case "build":
				options.BuildIdentity = "argus-unreviewed-build"
			case "dispatch":
				dispatch.Spec.RunID = "other-run"
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			service, err := NewService(panicTargetSource{}, runs, options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.ResumeReview(ctx, runID, request.RepositoryPath); err == nil {
				t.Fatal("changed recovery authority was admitted")
			}
			after, err := runs.Events(runID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected recovery changed the durable ledger")
			}
		})
	}
}

func TestResumeReviewRevalidatesAuthorityBeforeRepairStageAndFinalCommit(t *testing.T) {
	for _, boundary := range []string{"repair", "stage", "terminal"} {
		t.Run(boundary, func(t *testing.T) {
			first, runs, request, runID, snapshot, bundle := interruptedReviewFixture(t, reviewcore.TargetModeSelection, true)
			dispatch := recoveryTestDispatch(runID, snapshot.ReviewInputRef.URI)
			checks := 0
			service, err := NewService(panicTargetSource{}, runs, ServiceOptions{
				ConfigBundle: bundle, BuildIdentity: first.buildIdentity,
				DisableScheduling: true, ExecutionDispatch: &dispatch,
				ExecutionAuthority: func(context.Context, scheduling.Dispatch) error {
					checks++
					events, err := runs.Events(runID)
					if err != nil {
						return err
					}
					var last runrepo.RunEvent
					if err := json.Unmarshal(events[len(events)-1].Payload, &last); err != nil {
						return err
					}
					if boundary == "repair" && checks > 1 ||
						boundary == "stage" && last.EventType == runrepo.EventStageStarted ||
						boundary == "terminal" && last.EventType == runrepo.EventEvidenceRecorded &&
							last.StageID == string(reviewcore.StageExportEvaluation) {
						return scheduling.ErrFenced
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.ResumeReview(t.Context(), runID, request.RepositoryPath); !errors.Is(err, scheduling.ErrFenced) {
				t.Fatalf("recovery at %s did not fail fenced: %v", boundary, err)
			}
			events, err := runs.Events(runID)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "repair" && len(events) != 1 {
				t.Fatalf("fenced repair appended %d events", len(events)-1)
			}
			for _, envelope := range events {
				var event runrepo.RunEvent
				if err := json.Unmarshal(envelope.Payload, &event); err != nil {
					t.Fatal(err)
				}
				if event.EventType == runrepo.EventRunSucceeded || event.EventType == runrepo.EventRunFailed ||
					event.EventType == runrepo.EventRunCanceled ||
					boundary == "stage" && event.EventType == runrepo.EventStageSucceeded {
					t.Fatalf("stale lease appended authority: %+v", event)
				}
			}
			// A new current owner repairs the exact prefix and can finish the
			// same run even if its predecessor lost authority at final commit.
			service.executionAuthority = func(context.Context, scheduling.Dispatch) error { return nil }
			outcome, err := service.ResumeReview(t.Context(), runID, request.RepositoryPath)
			if err != nil || outcome.Run.Status != runmodel.RunStatusSucceeded {
				t.Fatalf("current owner could not resume: outcome=%+v error=%v", outcome, err)
			}
		})
	}
}

func TestCanceledReviewLeaseMayOnlyCloseItsOwnCanceledRun(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprintf("successor-%t", successor), func(t *testing.T) {
			repositoryPath, base, head := reviewFixture(t)
			runID := "run-explicit-cancel"
			dispatch := recoveryTestDispatch(runID, "artifact://local/sha256/"+strings.Repeat("a", 64))
			var active atomic.Bool
			active.Store(true)
			entered := make(chan struct{})
			service, runs := newTestService(t, ServiceOptions{
				IDs: &interruptedReviewIDs{runID: runID}, BuildIdentity: "argus-cancellation-test",
				ExecutionDispatch: &dispatch, ExecutorIdentity: "cancellation-test",
				ExecutorCapabilities: map[string]workflow.ExecutorCapabilities{"deterministic-local": {}},
				ExecuteStage: func(ctx context.Context, input reviewcore.ReviewInput, stage reviewcore.StageName, prior reviewcore.StageResult, policy reviewcore.RuntimePolicy) (reviewcore.StageResult, error) {
					if stage == reviewcore.StageDetect {
						close(entered)
						<-ctx.Done()
						return reviewcore.StageResult{}, ctx.Err()
					}
					return reviewcore.ExecuteStageWithPolicy(ctx, input, stage, prior, policy)
				},
				ExecutionAuthority: func(context.Context, scheduling.Dispatch) error {
					if !active.Load() {
						return scheduling.ErrFenced
					}
					return nil
				},
				ExecutionCancellationAuthority: func(context.Context, scheduling.Dispatch) error {
					if successor {
						return scheduling.ErrFenced
					}
					return nil
				},
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := service.Review(ctx, ReviewRequest{RepositoryPath: repositoryPath, BaseRevision: base, HeadRevision: head})
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("review exited before cancellation: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("review did not enter detect stage")
			}
			active.Store(false)
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancellation reported success")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("review did not terminate after cancellation")
			}
			run, err := runs.LoadRun(runID)
			if successor {
				if err == nil {
					t.Fatalf("old lease committed terminal after successor: %+v", run)
				}
			} else if err != nil || run.Status != runmodel.RunStatusCanceled {
				t.Fatalf("explicit cancellation did not close its run: run=%+v error=%v", run, err)
			}
		})
	}
}

func TestCancelReviewRepairsCancellationRacesWithoutExecutingStages(t *testing.T) {
	for _, boundary := range []string{"created", "stage_success", "terminal"} {
		t.Run(boundary, func(t *testing.T) {
			first, runs, request, runID, snapshot, bundle := interruptedReviewFixture(t, reviewcore.TargetModeSelection, true)
			dispatch := recoveryTestDispatch(runID, snapshot.ReviewInputRef.URI)
			service, err := NewService(panicTargetSource{}, runs, ServiceOptions{
				ConfigBundle: bundle, BuildIdentity: first.buildIdentity, DisableScheduling: true,
				ExecutionDispatch: &dispatch,
				ExecutionAuthority: func(context.Context, scheduling.Dispatch) error {
					events, err := runs.Events(runID)
					if err != nil {
						return err
					}
					var last runrepo.RunEvent
					if err := json.Unmarshal(events[len(events)-1].Payload, &last); err != nil {
						return err
					}
					if boundary == "stage_success" && last.EventType == runrepo.EventStageStarted ||
						boundary == "terminal" && last.EventType == runrepo.EventEvidenceRecorded &&
							last.StageID == string(reviewcore.StageExportEvaluation) {
						return scheduling.ErrFenced
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if boundary != "created" {
				if _, err := service.ResumeReview(t.Context(), runID, request.RepositoryPath); !errors.Is(err, scheduling.ErrFenced) {
					t.Fatalf("did not reach cancellation race: %v", err)
				}
			}
			before, err := runs.Events(runID)
			if err != nil {
				t.Fatal(err)
			}
			service.executionAuthority = func(context.Context, scheduling.Dispatch) error { return scheduling.ErrFenced }
			service.executionCancellationAuthority = func(context.Context, scheduling.Dispatch) error { return scheduling.ErrFenced }
			if _, err := service.CancelReview(t.Context(), runID, request.RepositoryPath); !errors.Is(err, scheduling.ErrFenced) {
				t.Fatalf("non-owner cancellation was admitted: %v", err)
			}
			afterDenied, err := runs.Events(runID)
			if err != nil || !reflect.DeepEqual(before, afterDenied) {
				t.Fatal("denied cancellation changed ledger")
			}
			service.executionCancellationAuthority = func(context.Context, scheduling.Dispatch) error { return nil }
			service.executeStage = func(context.Context, reviewcore.ReviewInput, reviewcore.StageName, reviewcore.StageResult, reviewcore.RuntimePolicy) (reviewcore.StageResult, error) {
				panic("cancellation must not execute stages")
			}
			outcome, err := service.CancelReview(t.Context(), runID, request.RepositoryPath)
			var runErr *RunError
			if !errors.As(err, &runErr) || outcome.Run.Status != runmodel.RunStatusCanceled {
				t.Fatalf("cancellation failed to close ledger: outcome=%+v error=%v", outcome, err)
			}
			after, err := runs.Events(runID)
			if err != nil {
				t.Fatal(err)
			}
			for _, envelope := range after[len(before):] {
				var event runrepo.RunEvent
				if err := json.Unmarshal(envelope.Payload, &event); err != nil {
					t.Fatal(err)
				}
				if event.EventType == runrepo.EventBindingRecorded || event.EventType == runrepo.EventStageSucceeded {
					t.Fatalf("cancel closure admitted new execution: %+v", event)
				}
			}
			committed, err := runs.LoadRun(runID)
			if err != nil || committed.Status != runmodel.RunStatusCanceled {
				t.Fatalf("invalid committed cancellation: %+v %v", committed, err)
			}
		})
	}
}

func interruptedReviewFixture(
	t *testing.T,
	mode reviewcore.TargetMode,
	createdOnly bool,
) (*Service, *runrepo.Repository, ReviewRequest, string, runmodel.ExecutionSnapshot, reviewconfig.ConfigBundle) {
	t.Helper()
	repositoryPath, base, head := reviewFixture(t)
	request := ReviewRequest{RepositoryPath: repositoryPath, Mode: mode}
	if mode == reviewcore.TargetModeDiff {
		request.BaseRevision, request.HeadRevision = base, head
	} else {
		request.Revision, request.SelectionPath = head, "review.go"
		request.StartLine, request.EndLine = 3, 4
	}
	runID := "run-deterministic-recovery"
	service, runs := newTestService(t, ServiceOptions{IDs: &interruptedReviewIDs{runID: runID}})
	if createdOnly {
		bundle, config, err := service.configForReview(t.Context(), runID, request)
		if err != nil {
			t.Fatal(err)
		}
		materialized, err := Materialize(t.Context(), service.source, runs, request, config)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := service.buildReviewSpec(runID, materialized, bundle)
		if err != nil {
			t.Fatal(err)
		}
		_, snapshot, err := service.persistExecutionSnapshot(runID, spec,
			materialized.TargetRef, materialized.InputRef, nil,
			[]runmodel.ArtifactRef{}, []runmodel.ArtifactRef{}, nil, nil, service.workflow, bundle)
		if err != nil {
			t.Fatal(err)
		}
		at, err := service.timestamp()
		if err != nil {
			t.Fatal(err)
		}
		if err := runs.AppendEvent(runID+"-created", at, runrepo.RunEvent{
			RunID: runID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusPending,
			EventType: runrepo.EventRunCreated, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := service.Review(t.Context(), request); !errors.Is(err, errInterruptedReview) {
			t.Fatalf("first review did not stop at the injected crash: %v", err)
		}
	}
	snapshot, err := runs.ExecutionSnapshotForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := runs.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	return service, runs, request, runID, snapshot, bundle
}

var errInterruptedReview = errors.New("injected process exit before fourth binding")

type interruptedReviewIDs struct {
	runID    string
	bindings int
	sequence sequenceIDs
}

func (ids *interruptedReviewIDs) New(prefix string) (string, error) {
	if prefix == "run" {
		return ids.runID, nil
	}
	if prefix == "binding" {
		ids.bindings++
		if ids.bindings == 4 {
			return "", errInterruptedReview
		}
	}
	return ids.sequence.New(prefix)
}

func recoveryTestDispatch(runID, inputRef string) scheduling.Dispatch {
	at := time.Now().UTC()
	return scheduling.Dispatch{
		Spec: scheduling.WorkloadSpec{
			SchemaVersion: scheduling.WorkloadSchemaVersion,
			WorkloadID:    runID + "-workload", RunID: runID, TenantID: "local",
			Class: scheduling.ClassInteractive, InputRef: inputRef,
			SubmittedAt: at, ExecutionDeadline: at.Add(time.Hour),
		},
		Lease: scheduling.DispatchLease{
			LeaseID: runID + "-lease-2", WorkloadID: runID + "-workload", WorkerID: "recovery-worker",
			Attempt: 2, Generation: 2, FencingToken: 2,
			AcquiredAt: at, LastHeartbeat: at, ExpiresAt: at.Add(time.Minute),
		},
	}
}
