package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/scheduling"
)

const (
	defaultRunWorkloadTimeout = 30 * time.Minute
	maxRunWorkloadTimeout     = 24 * time.Hour
)

// runWorkload is one run-level durable coordinator lease. Stage execution
// remains sequential and in-process; the lease only admits, fences, and
// records the terminal result of the whole run.
type runWorkload struct {
	dispatch scheduling.Dispatch
}

type workloadStartError struct {
	code string
	err  error
}

func (err *workloadStartError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return err.code + ": " + err.err.Error()
}

func (err *workloadStartError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.err
}

func workloadClassForReview(mode reviewcore.TargetMode) (scheduling.WorkloadClass, error) {
	switch mode {
	case reviewcore.TargetModeDiff:
		return scheduling.ClassIncrementalMR, nil
	case reviewcore.TargetModeSelection:
		return scheduling.ClassInteractive, nil
	case reviewcore.TargetModeScope:
		return scheduling.ClassFullScan, nil
	default:
		return "", fmt.Errorf("unsupported review target mode %q", mode)
	}
}

func (service *Service) beginRunWorkload(
	ctx context.Context,
	run runmodel.ReviewRun,
	inputRef runmodel.ArtifactRef,
	class scheduling.WorkloadClass,
) (*runWorkload, error) {
	if service.workloads == nil {
		return nil, nil
	}
	submittedAt, err := service.timestamp()
	if err != nil {
		return nil, err
	}
	spec := scheduling.WorkloadSpec{
		SchemaVersion:     scheduling.WorkloadSchemaVersion,
		WorkloadID:        run.RunID + "-workload",
		RunID:             run.RunID,
		TenantID:          localTenantID,
		Class:             class,
		Priority:          0,
		InputRef:          inputRef.URI,
		SubmittedAt:       submittedAt,
		ExecutionDeadline: submittedAt.Add(service.workloadTimeout),
	}
	record, err := service.workloads.Submit(ctx, spec, scheduling.Mutation{
		IdempotencyKey: run.RunID + "-workload-submit",
		Actor:          service.workerID,
		Audit:          "Argus run-level coordinator admission",
		At:             submittedAt,
	})
	if err != nil {
		return nil, &workloadStartError{code: "scheduling_submit_failed", err: err}
	}
	if record.Spec != spec {
		return nil, &workloadStartError{
			code: "scheduling_admission_mismatch",
			err:  fmt.Errorf("scheduler returned a different frozen workload spec"),
		}
	}
	switch record.Admission.Decision {
	case scheduling.AdmissionRejected:
		return nil, &workloadStartError{
			code: "scheduling_admission_rejected",
			err: fmt.Errorf(
				"workload %q rejected: %s",
				spec.WorkloadID,
				record.Admission.Reason,
			),
		}
	case scheduling.AdmissionThrottled:
		return nil, &workloadStartError{
			code: "scheduling_admission_throttled",
			err: fmt.Errorf(
				"workload %q throttled: %s",
				spec.WorkloadID,
				record.Admission.Reason,
			),
		}
	case scheduling.AdmissionAdmitted, scheduling.AdmissionQueued:
		if record.State != scheduling.StatePending {
			return nil, &workloadStartError{
				code: "scheduling_admission_mismatch",
				err: fmt.Errorf(
					"workload %q admission %q has state %q",
					spec.WorkloadID,
					record.Admission.Decision,
					record.State,
				),
			}
		}
	default:
		return nil, &workloadStartError{
			code: "scheduling_admission_mismatch",
			err: fmt.Errorf(
				"workload %q has unsupported admission %q",
				spec.WorkloadID,
				record.Admission.Decision,
			),
		}
	}

	claimedAt, err := service.timestamp()
	if err != nil {
		return nil, err
	}
	dispatch, err := service.workloads.Claim(ctx, scheduling.ClaimRequest{
		IdempotencyKey:   run.RunID + "-workload-claim",
		WorkloadID:       spec.WorkloadID,
		WorkerID:         service.workerID,
		SupportedClasses: []scheduling.WorkloadClass{class},
		At:               claimedAt,
	})
	if err != nil {
		service.cancelUnclaimedWorkload(run.RunID, claimedAt)
		code := "scheduling_claim_failed"
		if errors.Is(err, scheduling.ErrNoWork) {
			code = "scheduling_claim_unavailable"
		}
		return nil, &workloadStartError{code: code, err: err}
	}
	if err := validateRunDispatch(spec, service.workerID, dispatch); err != nil {
		service.cancelUnclaimedWorkload(run.RunID, claimedAt)
		return nil, &workloadStartError{
			code: "scheduling_dispatch_mismatch",
			err:  err,
		}
	}
	return &runWorkload{dispatch: dispatch}, nil
}

func validateRunDispatch(
	spec scheduling.WorkloadSpec,
	workerID string,
	dispatch scheduling.Dispatch,
) error {
	if dispatch.Spec != spec {
		return fmt.Errorf("dispatch does not bind the exact admitted workload spec")
	}
	if err := dispatch.Lease.Validate(); err != nil {
		return fmt.Errorf("validate dispatch lease: %w", err)
	}
	lease := dispatch.Lease
	if lease.WorkloadID != spec.WorkloadID ||
		lease.WorkerID != workerID ||
		lease.Attempt != 1 ||
		lease.Generation != 1 ||
		lease.FencingToken != 1 {
		return fmt.Errorf(
			"first local dispatch has unexpected identity, attempt, generation, or fencing token",
		)
	}
	return nil
}

func (service *Service) cancelUnclaimedWorkload(runID string, at time.Time) {
	terminalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = service.workloads.CancelRun(
		terminalCtx,
		runID,
		"local coordinator could not claim admitted workload",
		scheduling.Mutation{
			IdempotencyKey: runID + "-workload-cancel",
			Actor:          service.workerID,
			Audit:          "Argus run-level coordinator claim failure",
			At:             at,
		},
	)
}

func (service *Service) heartbeatRunWorkload(
	ctx context.Context,
	workload *runWorkload,
	stage reviewcore.StageName,
	attempt int,
	phase string,
) error {
	if workload == nil {
		return nil
	}
	at, err := service.timestamp()
	if err != nil {
		return err
	}
	lease := workload.dispatch.Lease
	record, err := service.workloads.Heartbeat(ctx, scheduling.Heartbeat{
		IdempotencyKey: fmt.Sprintf(
			"%s-workload-heartbeat-%s-%d-%s",
			workload.dispatch.Spec.RunID,
			stage,
			attempt,
			phase,
		),
		LeaseID:      lease.LeaseID,
		WorkloadID:   lease.WorkloadID,
		WorkerID:     lease.WorkerID,
		Attempt:      lease.Attempt,
		Generation:   lease.Generation,
		FencingToken: lease.FencingToken,
		At:           at,
	})
	if err != nil {
		return err
	}
	if record.State != scheduling.StateLeased || record.ActiveLease == nil {
		return fmt.Errorf(
			"%w: heartbeat returned non-leased workload state %q",
			scheduling.ErrFenced,
			record.State,
		)
	}
	active := *record.ActiveLease
	if active.LeaseID != lease.LeaseID ||
		active.WorkloadID != lease.WorkloadID ||
		active.WorkerID != lease.WorkerID ||
		active.Attempt != lease.Attempt ||
		active.Generation != lease.Generation ||
		active.FencingToken != lease.FencingToken {
		return fmt.Errorf(
			"%w: heartbeat changed the exact dispatch identity",
			scheduling.ErrFenced,
		)
	}
	workload.dispatch.Lease = active
	return nil
}

func (service *Service) finalizeScheduledTerminal(
	run runmodel.ReviewRun,
	status runmodel.RunStatus,
	failure *runmodel.Failure,
	report *reviewcore.Report,
	markdown string,
	workload *runWorkload,
) (RunOutcome, error) {
	// The callback must reference the immutable committed run artifact, so the
	// run commit precedes the scheduling callback. Callback identity/time are
	// deterministic for safe retry, but this local coordinator does not claim
	// automatic crash recovery across the two durable streams.
	outcome, runErr := service.finalizeTerminal(run, status, failure, report, markdown)
	if outcome.FinalRef.URI == "" || workload == nil {
		return outcome, runErr
	}
	schedulingErr := service.recordWorkloadTerminal(outcome, workload)
	switch {
	case runErr == nil:
		return outcome, schedulingErr
	case schedulingErr == nil:
		return outcome, runErr
	default:
		return outcome, errors.Join(runErr, schedulingErr)
	}
}

func (service *Service) recordWorkloadTerminal(
	outcome RunOutcome,
	workload *runWorkload,
) error {
	completedAt := *outcome.Run.CompletedAt
	terminalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if outcome.Run.Status == runmodel.RunStatusCanceled {
		records, err := service.workloads.CancelRun(
			terminalCtx,
			outcome.Run.RunID,
			"Argus run reached canceled terminal state",
			scheduling.Mutation{
				IdempotencyKey: outcome.Run.RunID + "-workload-cancel",
				Actor:          service.workerID,
				Audit:          "Argus run terminal cancellation",
				At:             completedAt,
			},
		)
		if err != nil {
			if errors.Is(err, scheduling.ErrInvalidTransition) &&
				strings.Contains(err.Error(), "already permanently canceled") {
				// Another actor may have already installed the permanent run
				// cancellation fence. The terminal intent is satisfied.
				return nil
			}
			return fmt.Errorf("record scheduling cancellation: %w", err)
		}
		for _, record := range records {
			if record.Spec.WorkloadID == workload.dispatch.Spec.WorkloadID &&
				record.RunCanceled &&
				record.State == scheduling.StateCanceled {
				return nil
			}
		}
		return fmt.Errorf("scheduling cancellation did not fence the exact workload")
	}

	callback := scheduling.Callback{
		SchemaVersion:  scheduling.CallbackSchemaVersion,
		IdempotencyKey: outcome.Run.RunID + "-workload-callback-" + string(outcome.Run.Status),
		LeaseID:        workload.dispatch.Lease.LeaseID,
		WorkloadID:     workload.dispatch.Lease.WorkloadID,
		WorkerID:       workload.dispatch.Lease.WorkerID,
		Attempt:        workload.dispatch.Lease.Attempt,
		Generation:     workload.dispatch.Lease.Generation,
		FencingToken:   workload.dispatch.Lease.FencingToken,
		OutputRefs:     []string{},
		OccurredAt:     completedAt,
	}
	expectedState := scheduling.StateFailed
	switch outcome.Run.Status {
	case runmodel.RunStatusSucceeded:
		callback.Status = scheduling.CallbackSucceeded
		callback.OutputRefs = []string{outcome.FinalRef.URI}
		expectedState = scheduling.StateSucceeded
	case runmodel.RunStatusFailed:
		callback.Status = scheduling.CallbackFailed
		callback.FailureCode = "run_failed"
		if outcome.Run.Failure != nil && outcome.Run.Failure.Code != "" {
			callback.FailureCode = outcome.Run.Failure.Code
		}
	default:
		return fmt.Errorf(
			"unsupported run terminal status %q for scheduling callback",
			outcome.Run.Status,
		)
	}
	record, err := service.workloads.Complete(terminalCtx, callback)
	if err != nil {
		return fmt.Errorf("record scheduling terminal callback: %w", err)
	}
	if record.Spec != workload.dispatch.Spec ||
		record.State != expectedState ||
		record.Terminal == nil ||
		record.Terminal.CallbackID != callback.IdempotencyKey ||
		record.Terminal.Status != callback.Status {
		return fmt.Errorf("scheduler terminal projection does not match the exact run callback")
	}
	return nil
}

func workloadFailure(err error) *runmodel.Failure {
	code := "scheduling_start_failed"
	var startErr *workloadStartError
	if errors.As(err, &startErr) {
		code = startErr.code
	}
	return &runmodel.Failure{
		Code:      code,
		Message:   err.Error(),
		Retryable: false,
	}
}
