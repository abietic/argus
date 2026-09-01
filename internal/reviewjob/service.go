package reviewjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type Service struct {
	repository *Repository
	scheduler  Scheduler
	runs       *runrepo.Repository
	configs    ConfigResolver
	executor   Executor
	formal     *FormalProfile
	workerID   string
	now        func() time.Time

	notify chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// ConfigureFormalProfile enables formal_pi_review_v1 admission. It is kept
// separate from construction so existing deterministic-only embedders remain
// explicit and cannot accidentally expose a partially configured Pi profile.
func (service *Service) ConfigureFormalProfile(profile FormalProfile) error {
	if service == nil {
		return fmt.Errorf("review job service is not initialized")
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(profile.Options)
	if err != nil {
		return fmt.Errorf("validate formal Pi profile: %w", err)
	}
	if _, err := piexecution.NewCapabilityResolver(bootstrap.Manifest, profile.Pricing); err != nil {
		return fmt.Errorf("validate formal Pi pricing: %w", err)
	}
	if profile.Admission == nil {
		return fmt.Errorf("formal Pi admission validator is required")
	}
	copy := profile
	copy.Options.ReviewSkillPaths = slices.Clone(profile.Options.ReviewSkillPaths)
	copy.Options.KnowledgePaths = slices.Clone(profile.Options.KnowledgePaths)
	service.formal = &copy
	return nil
}

func NewService(
	repository *Repository,
	scheduler Scheduler,
	runs *runrepo.Repository,
	configs ConfigResolver,
	executor Executor,
	workerID string,
	now func() time.Time,
) (*Service, error) {
	if repository == nil || scheduler == nil || runs == nil || configs == nil || executor == nil {
		return nil, fmt.Errorf("review job repository, scheduler, runs, config resolver, and executor are required")
	}
	if err := validateID("worker_id", workerID); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Service{
		repository: repository, scheduler: scheduler, runs: runs,
		configs: configs, executor: executor, workerID: workerID, now: now,
		notify: make(chan struct{}, 1), done: make(chan struct{}),
		active: make(map[string]context.CancelFunc),
	}, nil
}

func (service *Service) Submit(
	ctx context.Context,
	request Request,
	mutation Mutation,
) (Record, error) {
	if err := request.Validate(); err != nil {
		return Record{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Record{}, err
	}
	if existing, command, err := service.repository.GetByIdempotency(
		mutation.Actor,
		mutation.IdempotencyKey,
	); err == nil {
		if !reflect.DeepEqual(command.Request, request) || command.Audit != mutation.Audit ||
			!command.SubmittedAt.Equal(mutation.At) {
			return Record{}, fmt.Errorf("%w: idempotency key was reused with different intent", ErrConflict)
		}
		if _, err := service.ensureScheduled(ctx, existing); err != nil {
			return Record{}, err
		}
		service.signal()
		return service.Get(existing.JobID)
	} else if !errors.Is(err, ErrNotFound) {
		return Record{}, err
	}

	var submission Submission
	var err error
	if request.ExecutionProfile == FormalPiExecutionProfile {
		submission, err = service.submitFormal(ctx, request, mutation)
	} else {
		_, runID := deterministicIDs(mutation.Actor, mutation.IdempotencyKey)
		applicationRequest := request.ApplicationRequest()
		resolution, resolutionErr := application.ReviewResolutionContext(runID, applicationRequest)
		if resolutionErr != nil {
			return Record{}, resolutionErr
		}
		bundle, receipt, resolutionErr := service.configs.ResolvePublishedWithReceipt(ctx, resolution)
		if resolutionErr != nil {
			return Record{}, fmt.Errorf("resolve exact published review config: %w", resolutionErr)
		}
		if validationErr := application.ValidateReviewRequestAgainstBundle(applicationRequest, bundle); validationErr != nil {
			return Record{}, validationErr
		}
		submission, _, err = service.repository.Create(ctx, request, mutation, bundle, receipt)
	}
	if err != nil {
		return Record{}, err
	}
	if _, err := service.ensureScheduled(ctx, submission); err != nil {
		return Record{}, err
	}
	service.signal()
	return service.Get(submission.JobID)
}

func (service *Service) submitFormal(
	ctx context.Context,
	request Request,
	mutation Mutation,
) (Submission, error) {
	if service.formal == nil {
		return Submission{}, fmt.Errorf("formal_pi_review_v1 is not configured on this API process")
	}
	sourceRun, err := service.runs.LoadRun(request.SourceRunID)
	if err != nil {
		return Submission{}, fmt.Errorf("load formal source run: %w", err)
	}
	if sourceRun.Status != runmodel.RunStatusSucceeded {
		return Submission{}, fmt.Errorf("formal source run must be succeeded")
	}
	if _, err := service.runs.CommittedRunRef(request.SourceRunID); err != nil {
		return Submission{}, fmt.Errorf("resolve committed formal source run: %w", err)
	}
	snapshot, err := service.runs.ExecutionSnapshotForRun(request.SourceRunID)
	if err != nil {
		return Submission{}, fmt.Errorf("load formal source snapshot: %w", err)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := service.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return Submission{}, fmt.Errorf("load formal source ReviewSpec: %w", err)
	}
	runID := formalreview.FormalRunID(request.SourceRunID, mutation.IdempotencyKey)
	resolution := reviewconfig.ResolutionContext{
		TenantID: spec.TenantID, OrganizationID: "local",
		RepositoryID: spec.Repository.RepositoryID, InvocationID: runID,
	}
	if spec.Target.Mode == contractsv1alpha1.ReviewModeSelection {
		resolution.Path = spec.Target.Selection.Path
	}
	bundle, receipt, err := service.configs.ResolvePublishedWithReceipt(ctx, resolution)
	if err != nil {
		return Submission{}, fmt.Errorf("resolve exact published formal config: %w", err)
	}
	if bundle.AgentReview == nil {
		return Submission{}, fmt.Errorf("published formal config has no agent_review policy")
	}
	definition := workflow.FormalAgentReviewDefinition()
	digest, err := workflow.DigestDefinition(definition)
	if err != nil {
		return Submission{}, err
	}
	if bundle.Workflow.Definition.ID != definition.ID ||
		bundle.Workflow.Definition.Revision != definition.Revision ||
		bundle.Workflow.Definition.SHA256 != digest {
		return Submission{}, fmt.Errorf("published formal config does not select the formal workflow")
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(service.formal.Options)
	if err != nil {
		return Submission{}, fmt.Errorf("freeze formal Pi runtime: %w", err)
	}
	subject := application.AgentPlanningSubject{
		TenantID: spec.TenantID, OrganizationID: "local",
		WorkspaceID: spec.WorkspaceID, RepositoryID: spec.Repository.RepositoryID,
	}
	if err := service.formal.Admission.ValidateFormalAdmission(ctx, subject, bootstrap); err != nil {
		return Submission{}, fmt.Errorf("validate governed formal components before admission: %w", err)
	}
	binding := FormalRuntimeBinding{
		Options: service.formal.Options, Bootstrap: bootstrap, Pricing: service.formal.Pricing,
	}
	submission, _, err := service.repository.CreateFormal(
		ctx, request, mutation, bundle, receipt, binding,
	)
	return submission, err
}

func (service *Service) Get(jobID string) (Record, error) {
	submission, err := service.repository.Get(jobID)
	if err != nil {
		return Record{}, err
	}
	workload, err := service.scheduler.Get(submission.WorkloadID)
	if err != nil {
		return Record{}, err
	}
	command, err := service.repository.LoadCommand(submission.CommandRef)
	if err != nil {
		return Record{}, err
	}
	if command.JobID != submission.JobID || command.RunID != submission.RunID ||
		command.Request.SchemaVersion != submission.Request.SchemaVersion ||
		!reflect.DeepEqual(command.Request, submission.Request) {
		return Record{}, fmt.Errorf("%w: command does not bind submission", ErrCorrupt)
	}
	if err := verifyJSONRef(command.ConfigBundle, submission.ConfigBundleRef); err != nil {
		return Record{}, fmt.Errorf("%w: config bundle ref: %v", ErrCorrupt, err)
	}
	if err := verifyJSONRef(command.ConfigReceipt, submission.ConfigReceiptRef); err != nil {
		return Record{}, fmt.Errorf("%w: config receipt ref: %v", ErrCorrupt, err)
	}
	record := Record{
		SchemaVersion: RecordSchemaVersion,
		JobID:         submission.JobID, RunID: submission.RunID,
		ExecutionProfile: command.ExecutionProfile,
		Request:          submission.Request, CommandRef: submission.CommandRef,
		ConfigBundleRef:  submission.ConfigBundleRef,
		ConfigReceiptRef: submission.ConfigReceiptRef,
		Workload:         workload, SubmittedBy: submission.Actor,
		Audit: submission.Audit, SubmittedAt: submission.SubmittedAt,
	}
	if run, loadErr := service.runs.LoadRun(submission.RunID); loadErr == nil {
		record.Run = &run
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return Record{}, loadErr
	}
	return record, nil
}

func (service *Service) List() ([]Record, error) {
	submissions, err := service.repository.List()
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(submissions))
	for _, submission := range submissions {
		record, err := service.Get(submission.JobID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (service *Service) Timeline(
	jobID string,
) ([]scheduling.WorkloadTimelineEvent, error) {
	submission, err := service.repository.Get(jobID)
	if err != nil {
		return nil, err
	}
	return service.scheduler.Timeline(submission.WorkloadID)
}

func (service *Service) Cancel(
	ctx context.Context,
	jobID string,
	command CancelCommand,
) (Record, error) {
	if err := command.Validate(); err != nil {
		return Record{}, err
	}
	submission, err := service.repository.Get(jobID)
	if err != nil {
		return Record{}, err
	}
	if command.Mutation.Actor != submission.Actor {
		return Record{}, fmt.Errorf("%w: only the submitting principal may cancel the local job", ErrConflict)
	}
	_, err = service.scheduler.CancelRun(ctx, submission.RunID, command.Reason, scheduling.Mutation{
		IdempotencyKey: command.Mutation.IdempotencyKey,
		Actor:          command.Mutation.Actor, Audit: command.Mutation.Audit, At: command.Mutation.At,
	})
	if err != nil {
		return Record{}, err
	}
	service.mu.Lock()
	if cancel := service.active[submission.WorkloadID]; cancel != nil {
		cancel()
	}
	service.mu.Unlock()
	return service.Get(jobID)
}

func (service *Service) Start(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	go func() {
		defer close(service.done)
		service.loop(ctx)
	}()
	service.signal()
	return nil
}

// Wait blocks until the coordinator loop has observed cancellation and stopped
// issuing repository mutations. Composition roots use it before releasing a
// temporary store or exiting a process.
func (service *Service) Wait() {
	if service != nil {
		<-service.done
	}
}

func (service *Service) ensureScheduled(
	ctx context.Context,
	submission Submission,
) (scheduling.WorkloadRecord, error) {
	class, err := classForRequest(submission.Request)
	if err != nil {
		return scheduling.WorkloadRecord{}, err
	}
	deadline := submission.SubmittedAt.Add(
		time.Duration(submission.Request.ExecutionTimeoutSeconds) * time.Second,
	)
	record, err := service.scheduler.Submit(ctx, scheduling.WorkloadSpec{
		SchemaVersion: scheduling.WorkloadSchemaVersion,
		WorkloadID:    submission.WorkloadID, RunID: submission.RunID,
		TenantID: "local", Class: class, Priority: 0,
		InputRef:    submission.CommandRef.URI,
		SubmittedAt: submission.SubmittedAt, ExecutionDeadline: deadline,
	}, scheduling.Mutation{
		IdempotencyKey: submission.JobID + "-schedule",
		Actor:          submission.Actor, Audit: "admit immutable local review job",
		At: submission.SubmittedAt,
	})
	if err != nil {
		return scheduling.WorkloadRecord{}, fmt.Errorf("schedule review job: %w", err)
	}
	if record.Spec.InputRef != submission.CommandRef.URI || record.Spec.RunID != submission.RunID {
		return scheduling.WorkloadRecord{}, fmt.Errorf("%w: scheduler returned mismatched review job", ErrCorrupt)
	}
	return record, nil
}

func (service *Service) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			service.cancelAll()
			return
		case <-ticker.C:
		case <-service.notify:
		}
		service.reconcile(ctx)
		service.dispatch(ctx)
	}
}

func (service *Service) reconcile(ctx context.Context) {
	at := service.now().UTC()
	if at.IsZero() {
		return
	}
	_, _ = service.scheduler.Reconcile(ctx, scheduling.Mutation{
		IdempotencyKey: "review-job-reconcile-" + at.Format("20060102t150405.000000000z"),
		Actor:          service.workerID, Audit: "reconcile local review job scheduling timeouts", At: at,
	})
}

func (service *Service) dispatch(ctx context.Context) {
	submissions, err := service.repository.List()
	if err != nil {
		return
	}
	for _, submission := range submissions {
		record, getErr := service.scheduler.Get(submission.WorkloadID)
		if errors.Is(getErr, scheduling.ErrNotFound) {
			record, getErr = service.ensureScheduled(ctx, submission)
		}
		if getErr != nil || record.State != scheduling.StatePending {
			continue
		}
		service.mu.Lock()
		_, active := service.active[submission.WorkloadID]
		service.mu.Unlock()
		if active {
			continue
		}
		class, classErr := classForRequest(submission.Request)
		if classErr != nil {
			continue
		}
		at := service.now().UTC()
		dispatch, claimErr := service.scheduler.Claim(ctx, scheduling.ClaimRequest{
			IdempotencyKey: fmt.Sprintf("%s-claim-g%d", submission.JobID, record.Generation+1),
			WorkloadID:     submission.WorkloadID, WorkerID: service.workerID,
			SupportedClasses: []scheduling.WorkloadClass{class}, At: at,
		})
		if claimErr != nil {
			continue
		}
		executionContext, cancel := context.WithDeadline(ctx, dispatch.Spec.ExecutionDeadline)
		service.mu.Lock()
		service.active[submission.WorkloadID] = cancel
		service.mu.Unlock()
		go service.execute(executionContext, cancel, submission, dispatch)
	}
}

func (service *Service) execute(
	ctx context.Context,
	cancel context.CancelFunc,
	submission Submission,
	dispatch scheduling.Dispatch,
) {
	defer func() {
		cancel()
		service.mu.Lock()
		delete(service.active, submission.WorkloadID)
		service.mu.Unlock()
		service.signal()
	}()
	command, err := service.repository.LoadCommand(submission.CommandRef)
	if err != nil {
		service.completeFailed(dispatch, "command_corrupt")
		return
	}
	claimedExecutor, canExecuteClaimed := service.executor.(ClaimedExecutor)
	if terminal, exists, nonterminal := service.runAuthority(submission.RunID); exists {
		service.completeFromRun(dispatch, terminal)
		return
	} else if nonterminal && (!canExecuteClaimed || !claimedExecutor.CanResumeNonterminal(command)) {
		service.completeFailed(dispatch, "orphaned_nonterminal_run")
		return
	}
	heartbeatDone := make(chan struct{})
	go service.heartbeat(ctx, cancel, dispatch, heartbeatDone)
	var outcome application.RunOutcome
	var executeErr error
	if canExecuteClaimed {
		outcome, executeErr = claimedExecutor.ExecuteClaimed(ctx, command, dispatch)
	} else {
		outcome, executeErr = service.executor.Execute(ctx, command)
	}
	close(heartbeatDone)
	if current, getErr := service.scheduler.Get(submission.WorkloadID); getErr == nil &&
		current.State == scheduling.StateCanceled {
		return
	}
	if outcome.Run.RunID != "" && outcome.Run.RunID != submission.RunID {
		service.completeFailed(dispatch, "executor_run_mismatch")
		return
	}
	if outcome.Run.Status == runmodel.RunStatusSucceeded && outcome.FinalRef.URI != "" {
		service.complete(dispatch, scheduling.CallbackSucceeded, []string{outcome.FinalRef.URI}, "")
		return
	}
	failureCode := "review_execution_failed"
	if outcome.Run.Failure != nil && outcome.Run.Failure.Code != "" {
		failureCode = outcome.Run.Failure.Code
	} else if errors.Is(executeErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		failureCode = scheduling.ReasonExecutionDeadline
	} else if errors.Is(executeErr, context.Canceled) {
		failureCode = "review_execution_canceled"
	}
	service.completeFailed(dispatch, failureCode)
}

func (service *Service) heartbeat(
	ctx context.Context,
	cancel context.CancelFunc,
	dispatch scheduling.Dispatch,
	done <-chan struct{},
) {
	interval := 5 * time.Minute
	remaining := time.Until(dispatch.Lease.ExpiresAt)
	if remaining > 0 && remaining/3 < interval {
		interval = remaining / 3
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	counter := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			counter++
			at := service.now().UTC()
			_, err := service.scheduler.Heartbeat(ctx, scheduling.Heartbeat{
				IdempotencyKey: fmt.Sprintf("%s-heartbeat-g%d-%d", dispatch.Spec.WorkloadID, dispatch.Lease.Generation, counter),
				LeaseID:        dispatch.Lease.LeaseID, WorkloadID: dispatch.Lease.WorkloadID,
				WorkerID: dispatch.Lease.WorkerID, Attempt: dispatch.Lease.Attempt,
				Generation: dispatch.Lease.Generation, FencingToken: dispatch.Lease.FencingToken,
				At: at,
			})
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func (service *Service) runAuthority(runID string) (runmodel.ReviewRun, bool, bool) {
	if run, err := service.runs.LoadRun(runID); err == nil {
		return run, true, false
	} else if !errors.Is(err, os.ErrNotExist) {
		return runmodel.ReviewRun{}, false, true
	}
	history, err := service.runs.History(0)
	if err != nil {
		return runmodel.ReviewRun{}, false, true
	}
	for _, entry := range history {
		if entry.RunID == runID {
			return runmodel.ReviewRun{}, false, true
		}
	}
	return runmodel.ReviewRun{}, false, false
}

func (service *Service) completeFromRun(dispatch scheduling.Dispatch, run runmodel.ReviewRun) {
	switch run.Status {
	case runmodel.RunStatusSucceeded:
		ref, err := service.runs.CommittedRunRef(run.RunID)
		if err != nil {
			service.completeFailed(dispatch, "committed_run_ref_unavailable")
			return
		}
		service.complete(dispatch, scheduling.CallbackSucceeded, []string{ref.URI}, "")
	case runmodel.RunStatusFailed, runmodel.RunStatusCanceled:
		code := "run_failed"
		if run.Failure != nil && run.Failure.Code != "" {
			code = run.Failure.Code
		}
		service.completeFailed(dispatch, code)
	default:
		service.completeFailed(dispatch, "orphaned_nonterminal_run")
	}
}

func (service *Service) completeFailed(dispatch scheduling.Dispatch, code string) {
	service.complete(dispatch, scheduling.CallbackFailed, []string{}, code)
}

func (service *Service) complete(
	dispatch scheduling.Dispatch,
	status scheduling.CallbackStatus,
	outputRefs []string,
	failureCode string,
) {
	at := service.now().UTC()
	callback := scheduling.Callback{
		SchemaVersion:  scheduling.CallbackSchemaVersion,
		IdempotencyKey: fmt.Sprintf("%s-callback-g%d-%s", dispatch.Spec.WorkloadID, dispatch.Lease.Generation, status),
		LeaseID:        dispatch.Lease.LeaseID, WorkloadID: dispatch.Lease.WorkloadID,
		WorkerID: dispatch.Lease.WorkerID, Attempt: dispatch.Lease.Attempt,
		Generation: dispatch.Lease.Generation, FencingToken: dispatch.Lease.FencingToken,
		Status: status, OutputRefs: slices.Clone(outputRefs), FailureCode: failureCode,
		OccurredAt: at,
	}
	terminalContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = service.scheduler.Complete(terminalContext, callback)
}

func (service *Service) signal() {
	select {
	case service.notify <- struct{}{}:
	default:
	}
}

func (service *Service) cancelAll() {
	service.mu.Lock()
	defer service.mu.Unlock()
	for _, cancel := range service.active {
		cancel()
	}
}

func classForMode(mode string) (scheduling.WorkloadClass, error) {
	switch reviewcore.TargetMode(mode) {
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

func classForRequest(request Request) (scheduling.WorkloadClass, error) {
	if request.ExecutionProfile == FormalPiExecutionProfile {
		return scheduling.ClassInteractive, nil
	}
	return classForMode(request.Mode)
}

func verifyJSONRef(value any, ref runmodel.ArtifactRef) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	encoded := hex.EncodeToString(digest[:])
	if ref.URI != "artifact://local/sha256/"+encoded || ref.SHA256 != encoded ||
		ref.SizeBytes != int64(len(data)) {
		return fmt.Errorf("artifact ref does not bind exact JSON bytes")
	}
	return nil
}
