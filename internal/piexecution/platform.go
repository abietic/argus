package piexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"argus.local/argus/internal/agentshadowworker"
	"argus.local/argus/internal/application"
	"argus.local/argus/internal/execution"
	"argus.local/argus/internal/pireviewmap"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	intentSchema     = "argus.pi_execution_intent.v1alpha1"
	completionSchema = "argus.pi_execution_completion.v1alpha1"
	cancelSchema     = "argus.pi_execution_cancel.v1alpha1"
	maxWorkerInput   = int64(96 << 20)
)

var (
	ErrExecutionConflict = errors.New("Pi execution idempotency key conflicts with immutable input")
	ErrCompletionPending = errors.New("Pi execution completion is pending or unknown after restart")
)

type executionIntent struct {
	SchemaVersion        string
	RequestSHA256        string
	CreateIdempotencyKey string
	WorkerRequestSHA256  string
	Deadline             time.Time
	Handle               execution.Handle
}

type executionCompletion struct {
	SchemaVersion  string
	IntentID       string
	ProviderHandle string
	ResultJSON     []byte
	CallbackProof  string
}

type executionCancel struct {
	SchemaVersion string
	Request       execution.CancelRequest
}

type preparedExecution struct {
	request       contractsv1alpha1.StageExecutionRequest
	workerRequest contractsv1alpha1.AgentReviewWorkerRequest
	workerJSON    []byte
	plan          contractsv1alpha1.AgentStagePlan
	input         reviewcore.ReviewInput
	modelProfile  []byte
	handle        execution.Handle
	intentID      string
}

type runningExecution struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type Platform struct {
	store            *local.Store
	config           Config
	ledgerSub        runmodel.AgentPlanningSubject
	groupCheckpoints *piGroupCheckpointRepository

	mu      sync.Mutex
	running map[string]*runningExecution
}

func NewPlatform(store *local.Store, config Config) (*Platform, error) {
	if store == nil || config.Runner == nil || config.Artifacts == nil ||
		config.LocalArtifacts == nil {
		return nil, fmt.Errorf("Pi store, runner, governed artifact IO, and local artifact IO are required")
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: config.Subject.TenantID, OrganizationID: config.Subject.OrganizationID,
		WorkspaceID: config.Subject.WorkspaceID, RepositoryID: config.Subject.RepositoryID,
	}
	if err := ledgerSubject.Validate(); err != nil {
		return nil, fmt.Errorf("validate Pi platform subject: %w", err)
	}
	if config.NodePath == "" || config.WorkerScript == "" {
		return nil, fmt.Errorf("Pi node and worker script paths are required")
	}
	if err := validateRuntimeManifest(config.Manifest); err != nil {
		return nil, err
	}
	if err := validatePricing(config.Pricing); err != nil {
		return nil, err
	}
	if config.RuntimeFiles == nil {
		config.RuntimeFiles = LocalRuntimeFileVerifier{}
	}
	config.Environment = cloneEnvironment(config.Environment)
	groupCheckpoints, err := newPiGroupCheckpointRepository(store)
	if err != nil {
		return nil, err
	}
	return &Platform{
		store: store, config: config, ledgerSub: ledgerSubject,
		groupCheckpoints: groupCheckpoints,
		running:          make(map[string]*runningExecution),
	}, nil
}

func (platform *Platform) Ensure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, error) {
	if ctx == nil {
		return execution.Handle{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return execution.Handle{}, err
	}
	if err := platform.verifyRuntimeFiles(ctx); err != nil {
		return execution.Handle{}, fmt.Errorf("verify local Pi runtime files: %w", err)
	}
	prepared, err := platform.prepare(ctx, request)
	if err != nil {
		return execution.Handle{}, err
	}
	if err := platform.groupCheckpoints.BindGeneration(ctx, request); err != nil {
		return execution.Handle{}, fmt.Errorf("bind Pi group checkpoint generation: %w", err)
	}
	reusable, resumedWindowStart, err := platform.groupCheckpoints.reusableBefore(ctx, request)
	if err != nil {
		return execution.Handle{}, fmt.Errorf("load reusable Pi group checkpoints: %w", err)
	}
	prepared.workerRequest.GroupCheckpoints = reusable
	if !resumedWindowStart.IsZero() &&
		resumedWindowStart.Before(prepared.workerRequest.Plan.CreatedAt) {
		prepared.workerRequest.Plan.CreatedAt = resumedWindowStart
	}
	if err := prepared.workerRequest.Validate(); err != nil {
		return execution.Handle{}, fmt.Errorf("validate resumed Pi worker request: %w", err)
	}
	prepared.workerJSON, err = json.Marshal(prepared.workerRequest)
	if err != nil {
		return execution.Handle{}, err
	}
	if int64(len(prepared.workerJSON)) > maxWorkerInput {
		return execution.Handle{}, fmt.Errorf("formal Pi resumed worker request exceeds host limit")
	}
	intent := executionIntent{
		SchemaVersion:        intentSchema,
		RequestSHA256:        request.RequestSHA256,
		CreateIdempotencyKey: request.IdempotencyKey,
		WorkerRequestSHA256:  shaHex(prepared.workerJSON),
		Deadline:             request.Deadline,
		Handle:               prepared.handle,
	}
	won := false
	if err := platform.store.PutJSON(prepared.intentID, intent); err == nil {
		won = true
	} else if errors.Is(err, local.ErrImmutableExists) {
		var existing executionIntent
		if loadErr := platform.store.GetJSON(prepared.intentID, &existing); loadErr != nil {
			return execution.Handle{}, fmt.Errorf("load existing Pi execution intent: %w", loadErr)
		}
		if existing != intent {
			return execution.Handle{}, ErrExecutionConflict
		}
	} else {
		return execution.Handle{}, fmt.Errorf("persist Pi execution intent: %w", err)
	}
	if won {
		platform.start(prepared)
	}
	return prepared.handle, nil
}

func (platform *Platform) Lookup(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, bool, error) {
	if ctx == nil {
		return execution.Handle{}, false, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return execution.Handle{}, false, err
	}
	if err := request.Validate(); err != nil {
		return execution.Handle{}, false, err
	}
	intentID := intentPath(request.IdempotencyKey)
	var intent executionIntent
	if err := platform.store.GetJSON(intentID, &intent); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return execution.Handle{}, false, nil
		}
		return execution.Handle{}, false, err
	}
	if err := validateStoredIntent(request, intent); err != nil {
		return execution.Handle{}, false, err
	}
	return intent.Handle, true, nil
}

func (platform *Platform) Cancel(
	ctx context.Context,
	request execution.CancelRequest,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	intentID := intentPath(request.CreateIdempotencyKey)
	var intent executionIntent
	if err := platform.store.GetJSON(intentID, &intent); err != nil {
		return fmt.Errorf("load Pi cancel target intent: %w", err)
	}
	if request.ProviderHandle != intent.Handle.ProviderHandle ||
		request.ExecutionID != intent.Handle.ExecutionID ||
		request.Attempt != intent.Handle.Attempt ||
		request.Generation != intent.Handle.Generation ||
		request.FencingToken != intent.Handle.FencingToken ||
		request.CreateIdempotencyKey != intent.Handle.IdempotencyKey ||
		request.RequestSHA256 != intent.Handle.RequestSHA256 ||
		request.CapabilitySHA256 != intent.Handle.CapabilitySHA256 ||
		request.Authority.TenantID != platform.ledgerSub.TenantID ||
		request.Authority.WorkspaceID != platform.ledgerSub.WorkspaceID ||
		request.CancelIdempotencyKey != request.Authority.CancelIdempotencyKey {
		return ErrExecutionConflict
	}
	cancelID := cancelPath(request.CancelIdempotencyKey)
	record := executionCancel{SchemaVersion: cancelSchema, Request: request}
	if err := platform.store.PutJSON(cancelID, record); err != nil {
		if !errors.Is(err, local.ErrImmutableExists) {
			return fmt.Errorf("persist Pi cancel intent: %w", err)
		}
		var existing executionCancel
		if loadErr := platform.store.GetJSON(cancelID, &existing); loadErr != nil {
			return fmt.Errorf("load Pi cancel intent: %w", loadErr)
		}
		if existing != record {
			return ErrExecutionConflict
		}
	}
	recordedAt := time.Now().UTC()
	if deadline, err := platform.requestDeadline(intentID); err == nil && recordedAt.After(deadline) {
		recordedAt = deadline
	}
	result := terminalResult(
		intent.Handle,
		contractsv1alpha1.StageExecutionCanceled,
		"execution_canceled",
		"formal Pi execution was canceled",
		false,
		recordedAt,
	)
	if _, err := platform.commitCompletion(intentID, intent.Handle, result); err != nil {
		return err
	}
	platform.mu.Lock()
	active := platform.running[intentID]
	platform.mu.Unlock()
	if active != nil {
		active.cancel()
	}
	return nil
}

// AwaitResult returns only a durably committed formal StageExecutionResult and
// its local immutable-ledger callback proof. A persisted started intent without
// completion is unknown after restart and is never rerun implicitly.
func (platform *Platform) AwaitResult(
	ctx context.Context,
	handle execution.Handle,
) (contractsv1alpha1.StageExecutionResult, []byte, error) {
	if ctx == nil {
		return contractsv1alpha1.StageExecutionResult{}, nil, fmt.Errorf("context is required")
	}
	intentID := intentPath(handle.IdempotencyKey)
	for {
		completion, found, err := platform.loadCompletion(intentID)
		if err != nil {
			return contractsv1alpha1.StageExecutionResult{}, nil, err
		}
		if found {
			if completion.ProviderHandle != handle.ProviderHandle {
				return contractsv1alpha1.StageExecutionResult{}, nil, ErrExecutionConflict
			}
			result, err := contractsv1alpha1.DecodeStageExecutionResult(completion.ResultJSON)
			if err != nil {
				return contractsv1alpha1.StageExecutionResult{}, nil, err
			}
			return result, []byte(completion.CallbackProof), nil
		}
		platform.mu.Lock()
		active := platform.running[intentID]
		platform.mu.Unlock()
		if active == nil {
			return contractsv1alpha1.StageExecutionResult{}, nil, ErrCompletionPending
		}
		select {
		case <-ctx.Done():
			return contractsv1alpha1.StageExecutionResult{}, nil, ctx.Err()
		case <-active.done:
		}
	}
}

func (platform *Platform) prepare(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (preparedExecution, error) {
	if err := request.Validate(); err != nil {
		return preparedExecution{}, fmt.Errorf("validate formal Pi request: %w", err)
	}
	if request.TenantID != platform.ledgerSub.TenantID ||
		request.WorkspaceID != platform.ledgerSub.WorkspaceID {
		return preparedExecution{}, fmt.Errorf("formal Pi request escaped bound subject")
	}
	planData, err := platform.config.Artifacts.Resolve(ctx, request.Plan)
	if err != nil {
		return preparedExecution{}, fmt.Errorf("resolve formal Pi plan: %w", err)
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(planData)
	if err != nil {
		return preparedExecution{}, err
	}
	if err := validatePlanAgainstManifest(plan, platform.config.Manifest, platform.config.Pricing); err != nil {
		return preparedExecution{}, err
	}
	rulePackData, err := base64.StdEncoding.Strict().DecodeString(plan.RulePackBase64)
	if err != nil {
		return preparedExecution{}, fmt.Errorf("decode formal Pi plan rule pack: %w", err)
	}
	inputData, err := platform.config.Artifacts.Resolve(ctx, request.ReviewInput)
	if err != nil {
		return preparedExecution{}, fmt.Errorf("resolve frozen ReviewInput: %w", err)
	}
	input, err := reviewcore.DecodeReviewInput(inputData)
	if err != nil {
		return preparedExecution{}, err
	}
	modelProfileData, err := platform.config.Artifacts.Resolve(ctx, plan.Model.Artifact)
	if err != nil {
		return preparedExecution{}, fmt.Errorf("resolve formal Pi model profile: %w", err)
	}
	promptBundleData, err := platform.config.Artifacts.Resolve(ctx, plan.Prompt.Artifact)
	if err != nil {
		return preparedExecution{}, fmt.Errorf("resolve formal Pi prompt bundle: %w", err)
	}
	reviewSkillData := make([][]byte, 0, len(plan.Skills))
	for _, skill := range plan.Skills {
		if skill.Phase != contractsv1alpha1.AgentStageSkillPhaseReview {
			continue
		}
		content, resolveErr := platform.config.Artifacts.Resolve(ctx, skill.Artifact)
		if resolveErr != nil {
			return preparedExecution{}, fmt.Errorf(
				"resolve formal Pi review skill %q: %w",
				skill.ID,
				resolveErr,
			)
		}
		reviewSkillData = append(reviewSkillData, content)
	}
	knowledgePackData := make([][]byte, len(plan.Knowledge))
	for index, binding := range plan.Knowledge {
		content, resolveErr := platform.config.Artifacts.Resolve(ctx, binding.Artifact)
		if resolveErr != nil {
			return preparedExecution{}, fmt.Errorf(
				"resolve formal Pi knowledge %q: %w", binding.ID, resolveErr,
			)
		}
		knowledgePackData[index] = content
	}
	contextArtifactData := make([][]byte, 0, len(input.Contexts))
	for index, binding := range input.Contexts {
		if binding.Ref == nil {
			continue
		}
		ref := binding.Ref
		content, resolveErr := platform.config.LocalArtifacts.ReadArtifact(
			runmodel.ArtifactRef{
				URI: ref.ArtifactURI, SHA256: ref.Digest,
				SizeBytes: ref.SizeBytes, Contract: ref.Contract,
			},
		)
		if resolveErr != nil {
			return preparedExecution{}, fmt.Errorf(
				"resolve frozen context artifact %d %q: %w",
				index,
				ref.ContextID,
				resolveErr,
			)
		}
		contextArtifactData = append(contextArtifactData, content)
	}
	workerRequest, err := pireviewmap.BuildFormalWorkerRequest(
		plan,
		request,
		input,
		modelProfileData,
		promptBundleData,
		reviewSkillData,
		knowledgePackData,
		contextArtifactData,
		rulePackData,
	)
	if err != nil {
		return preparedExecution{}, err
	}
	workerJSON, err := json.Marshal(workerRequest)
	if err != nil {
		return preparedExecution{}, err
	}
	if int64(len(workerJSON)) > maxWorkerInput {
		return preparedExecution{}, fmt.Errorf("formal Pi worker request exceeds host limit")
	}
	handle := execution.Handle{
		ProviderHandle: "pi-local/" + request.ExecutionID,
		ExecutionID:    request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, RequestSHA256: request.RequestSHA256,
		CapabilitySHA256: request.Capability.SHA256,
	}
	return preparedExecution{
		request: request, workerRequest: workerRequest, workerJSON: workerJSON,
		plan: plan, input: input, modelProfile: bytes.Clone(modelProfileData),
		handle: handle, intentID: intentPath(request.IdempotencyKey),
	}, nil
}

func (platform *Platform) start(prepared preparedExecution) {
	runContext, cancel := context.WithDeadline(context.Background(), prepared.request.Deadline)
	active := &runningExecution{cancel: cancel, done: make(chan struct{})}
	platform.mu.Lock()
	platform.running[prepared.intentID] = active
	platform.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			platform.mu.Lock()
			delete(platform.running, prepared.intentID)
			close(active.done)
			platform.mu.Unlock()
		}()
		platform.execute(runContext, prepared)
	}()
}

func (platform *Platform) execute(ctx context.Context, prepared preparedExecution) {
	if err := platform.verifyRuntimeFiles(ctx); err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_runtime_files_changed",
			"formal Pi runtime files changed after dispatch",
			false,
			boundedResultTime(time.Now().UTC(), prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	workerMaximum := prepared.workerRequest.Capability.MaxOutputBytes
	var maxOutput int64
	if workerMaximum > uint64(^uint64(0)>>1)-(64<<10) {
		maxOutput = int64(^uint64(0) >> 1)
	} else {
		maxOutput = int64(workerMaximum + 64<<10)
	}
	processResult, runErr := platform.config.Runner.Run(ctx, agentshadowworker.Request{
		NodePath: platform.config.NodePath, ScriptPath: platform.config.WorkerScript,
		Input:         bytes.Clone(prepared.workerJSON),
		Environment:   cloneEnvironment(platform.config.Environment),
		MaxInputBytes: maxWorkerInput, MaxOutputBytes: maxOutput,
		MaxStderrBytes: agentshadowworker.DefaultMaxStderrBytes,
		ProgressLine: func(line []byte) error {
			return platform.recordWorkerProgress(ctx, prepared.request, line)
		},
	})
	if runErr != nil {
		status := contractsv1alpha1.StageExecutionFailed
		code := "pi_worker_unconfirmed"
		message := "formal Pi worker ended without an accepted result"
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = contractsv1alpha1.StageExecutionCanceled
			code = "execution_canceled"
			message = "formal Pi execution was canceled or reached its deadline"
		}
		recordedAt := boundedResultTime(time.Now().UTC(), prepared.request.Deadline)
		result := terminalResult(prepared.handle, status, code, message, false, recordedAt)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	workerResult, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(processResult.Stdout)
	if err != nil || contractsv1alpha1.ValidateAgentReviewWorkerResultBinding(
		prepared.workerRequest,
		workerResult,
	) != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_worker_result_rejected",
			"formal Pi worker returned an invalid or mismatched result",
			false,
			boundedResultTime(processResult.FinishedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	if workerResult.Status != contractsv1alpha1.AgentReviewWorkerSucceeded {
		status := contractsv1alpha1.StageExecutionFailed
		if workerResult.Status == contractsv1alpha1.AgentReviewWorkerCanceled {
			status = contractsv1alpha1.StageExecutionCanceled
		}
		code := "pi_worker_" + string(workerResult.Status)
		message := "formal Pi worker reported a redacted terminal outcome"
		retryable := false
		// A worker result is self-report, so it cannot introduce a new retry
		// reason. Preserve only a bounded identifier that the immutable Plan
		// explicitly authorized and only when the authenticated worker marked
		// that exact failed outcome retryable. The free-form message remains
		// redacted and cancellations never become worker-directed retries.
		if workerResult.Status == contractsv1alpha1.AgentReviewWorkerFailed &&
			workerResult.Failure != nil && workerResult.Failure.Retryable &&
			slices.Contains(prepared.plan.Retry.RetryableCodes, workerResult.Failure.Code) {
			code = workerResult.Failure.Code
			message = "formal Pi worker reported a plan-authorized retryable terminal outcome"
			retryable = true
		}
		result := terminalResult(
			prepared.handle,
			status,
			code,
			message,
			retryable,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	artifacts, err := pireviewmap.MapFormalReviewArtifacts(pireviewmap.FormalMapInput{
		Plan: prepared.plan, Request: prepared.request, ReviewInput: prepared.input,
		ModelProfile:  prepared.modelProfile,
		WorkerRequest: prepared.workerRequest, WorkerResult: workerResult,
	})
	if err != nil {
		code := "pi_evidence_mapping_rejected"
		message := boundedFailureMessage("formal Pi worker evidence failed host mapping: ", err)
		retryable := false
		var classified *pireviewmap.FormalReviewRetryableError
		if errors.As(err, &classified) &&
			slices.Contains(prepared.plan.Retry.RetryableCodes, classified.Code) {
			code = classified.Code
			message = "formal Pi report proved only plan-authorized transient review failures"
			retryable = true
		} else if errors.Is(err, pireviewmap.ErrFormalReviewNoCompletedGroups) {
			code = "pi_no_review_completed"
			message = "formal Pi execution completed no review group"
		}
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			code,
			message,
			retryable,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		if taskEvidence, receipts, diagnosticErr := platform.publishFailedReviewDiagnostics(
			context.WithoutCancel(ctx),
			artifacts,
			workerResult.CompletedAt,
		); diagnosticErr != nil {
			originatingCode := result.Failure.Code
			result = terminalResult(
				prepared.handle,
				contractsv1alpha1.StageExecutionFailed,
				"pi_failure_diagnostic_persistence_failed",
				"formal Pi failure diagnostics could not be persisted",
				false,
				boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
			)
			result.CompletenessNotes = []string{
				originatingCode,
				"pi_failure_diagnostic_persistence_failed",
			}
		} else if taskEvidence != nil && receipts != nil {
			result.AgentTaskEvidence = taskEvidence
			result.AgentExecutionReceipts = receipts
		}
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	set := artifacts.Hypotheses
	setJSON, err := json.Marshal(set)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_evidence_encoding_failed",
			"formal Pi mapped evidence could not be encoded",
			false,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	rawCandidateJSON, err := json.Marshal(artifacts.RawCandidates)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_raw_candidate_encoding_failed",
			"formal Pi raw candidate evidence could not be encoded",
			false,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	taskEvidenceJSON, err := json.Marshal(artifacts.TaskEvidence)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_task_evidence_encoding_failed",
			"formal Pi task evidence could not be encoded",
			false,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	receiptJSON, err := json.Marshal(artifacts.Receipts)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_receipt_evidence_encoding_failed",
			"formal Pi execution receipt evidence could not be encoded",
			false,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	output, err := platform.config.Artifacts.Publish(
		context.WithoutCancel(ctx),
		contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		setJSON,
		"pi-output-"+shaHex(setJSON)[:24],
		workerResult.CompletedAt,
	)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_output_persistence_failed",
			"formal Pi output could not be persisted",
			true,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	rawCandidateEvidence, err := platform.config.Artifacts.Publish(
		context.WithoutCancel(ctx),
		contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		rawCandidateJSON,
		"pi-raw-candidates-"+shaHex(rawCandidateJSON)[:24],
		workerResult.CompletedAt,
	)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_raw_candidate_persistence_failed",
			"formal Pi raw candidate evidence could not be persisted",
			true,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	taskEvidence, err := platform.config.Artifacts.Publish(
		context.WithoutCancel(ctx),
		contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		taskEvidenceJSON,
		"pi-task-evidence-"+shaHex(taskEvidenceJSON)[:24],
		workerResult.CompletedAt,
	)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_task_evidence_persistence_failed",
			"formal Pi task evidence could not be persisted",
			true,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	receiptEvidence, err := platform.config.Artifacts.Publish(
		context.WithoutCancel(ctx),
		contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		receiptJSON,
		"pi-receipts-"+shaHex(receiptJSON)[:24],
		workerResult.CompletedAt,
	)
	if err != nil {
		result := terminalResult(
			prepared.handle,
			contractsv1alpha1.StageExecutionFailed,
			"pi_receipt_evidence_persistence_failed",
			"formal Pi execution receipt evidence could not be persisted",
			true,
			boundedResultTime(workerResult.CompletedAt, prepared.request.Deadline),
		)
		_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
		return
	}
	result := contractsv1alpha1.StageExecutionResult{
		SchemaVersion: contractsv1alpha1.StageExecutionResultSchemaVersion,
		RequestSHA256: prepared.request.RequestSHA256,
		ExecutionID:   prepared.request.ExecutionID, Attempt: prepared.request.Attempt,
		Generation: prepared.request.Generation, FencingToken: prepared.request.FencingToken,
		IdempotencyKey: prepared.request.IdempotencyKey,
		Status:         contractsv1alpha1.StageExecutionSucceeded, Output: &output,
		AgentRawCandidates:     &rawCandidateEvidence,
		AgentTaskEvidence:      &taskEvidence,
		AgentExecutionReceipts: &receiptEvidence,
		Completeness:           string(set.Completeness),
		CompletenessNotes:      append([]string{}, set.CompletenessReasons...),
		CapabilitySHA256:       prepared.request.Capability.SHA256,
		RecordedAt:             workerResult.CompletedAt,
	}
	_, _ = platform.commitCompletion(prepared.intentID, prepared.handle, result)
}

func (platform *Platform) publishFailedReviewDiagnostics(
	ctx context.Context,
	artifacts pireviewmap.FormalReviewArtifacts,
	recordedAt time.Time,
) (*contractsv1alpha1.ArtifactBinding, *contractsv1alpha1.ArtifactBinding, error) {
	if artifacts.TaskEvidence.SchemaVersion == "" || artifacts.Receipts.SchemaVersion == "" {
		return nil, nil, nil
	}
	taskEvidenceJSON, err := json.Marshal(artifacts.TaskEvidence)
	if err != nil {
		return nil, nil, fmt.Errorf("encode failed formal task evidence: %w", err)
	}
	receiptJSON, err := json.Marshal(artifacts.Receipts)
	if err != nil {
		return nil, nil, fmt.Errorf("encode failed formal execution receipts: %w", err)
	}
	taskEvidence, err := platform.config.Artifacts.Publish(
		ctx,
		contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		taskEvidenceJSON,
		"pi-failed-task-evidence-"+shaHex(taskEvidenceJSON)[:24],
		recordedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("publish failed formal task evidence: %w", err)
	}
	receipts, err := platform.config.Artifacts.Publish(
		ctx,
		contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		receiptJSON,
		"pi-failed-receipts-"+shaHex(receiptJSON)[:24],
		recordedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("publish failed formal execution receipts: %w", err)
	}
	return &taskEvidence, &receipts, nil
}

func (platform *Platform) verifyRuntimeFiles(ctx context.Context) error {
	if platform == nil || platform.config.RuntimeFiles == nil {
		return fmt.Errorf("local Pi runtime file verifier is unavailable")
	}
	return platform.config.RuntimeFiles.Verify(
		ctx,
		platform.config.NodePath,
		platform.config.WorkerScript,
		platform.config.Manifest.Runtime.SHA256,
	)
}

func boundedFailureMessage(prefix string, err error) string {
	message := prefix
	if err != nil {
		message += err.Error()
	}
	const maximum = 4096
	if len(message) <= maximum {
		return message
	}
	for len(message) > maximum {
		_, size := utf8.DecodeLastRuneInString(message)
		if size <= 0 {
			return message[:maximum]
		}
		message = message[:len(message)-size]
	}
	return message
}

func (platform *Platform) commitCompletion(
	intentID string,
	handle execution.Handle,
	result contractsv1alpha1.StageExecutionResult,
) (executionCompletion, error) {
	canonical, err := json.Marshal(result)
	if err != nil {
		return executionCompletion{}, err
	}
	if _, err := contractsv1alpha1.DecodeStageExecutionResult(canonical); err != nil {
		return executionCompletion{}, fmt.Errorf("validate canonical Pi StageExecutionResult: %w", err)
	}
	proof := "pi-local-ledger-" + shaHex(
		[]byte(intentID+"\x00"+handle.ProviderHandle+"\x00"+shaHex(canonical)+"\x00"+platform.config.Manifest.SHA256),
	)
	record := executionCompletion{
		SchemaVersion: completionSchema, IntentID: intentID,
		ProviderHandle: handle.ProviderHandle, ResultJSON: canonical, CallbackProof: proof,
	}
	path := completionPath(intentID)
	if err := platform.store.PutJSON(path, record); err == nil {
		return record, nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return executionCompletion{}, fmt.Errorf("persist Pi execution completion: %w", err)
	}
	existing, found, err := platform.loadCompletion(intentID)
	if err != nil || !found {
		return executionCompletion{}, fmt.Errorf("reload Pi execution completion: %w", err)
	}
	if sameCompletion(existing, record) ||
		(existingResultStatus(existing) == contractsv1alpha1.StageExecutionCanceled ||
			result.Status == contractsv1alpha1.StageExecutionCanceled) {
		return existing, nil
	}
	return executionCompletion{}, fmt.Errorf("%w: Pi completion differs for one execution", ErrExecutionConflict)
}

func (platform *Platform) loadCompletion(
	intentID string,
) (executionCompletion, bool, error) {
	var completion executionCompletion
	if err := platform.store.GetJSON(completionPath(intentID), &completion); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return executionCompletion{}, false, nil
		}
		return executionCompletion{}, false, err
	}
	if completion.SchemaVersion != completionSchema || completion.IntentID != intentID ||
		completion.ProviderHandle == "" || len(completion.ResultJSON) == 0 ||
		!strings.HasPrefix(completion.CallbackProof, "pi-local-ledger-") {
		return executionCompletion{}, false, fmt.Errorf("corrupt Pi execution completion")
	}
	return completion, true, nil
}

func validateStoredIntent(
	request contractsv1alpha1.StageExecutionRequest,
	intent executionIntent,
) error {
	want := execution.Handle{
		ProviderHandle: "pi-local/" + request.ExecutionID,
		ExecutionID:    request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, RequestSHA256: request.RequestSHA256,
		CapabilitySHA256: request.Capability.SHA256,
	}
	if intent.SchemaVersion != intentSchema || intent.RequestSHA256 != request.RequestSHA256 ||
		intent.CreateIdempotencyKey != request.IdempotencyKey ||
		!intent.Deadline.Equal(request.Deadline) || intent.Handle != want {
		return ErrExecutionConflict
	}
	return nil
}

func terminalResult(
	handle execution.Handle,
	status contractsv1alpha1.StageExecutionStatus,
	code string,
	message string,
	retryable bool,
	recordedAt time.Time,
) contractsv1alpha1.StageExecutionResult {
	return contractsv1alpha1.StageExecutionResult{
		SchemaVersion: contractsv1alpha1.StageExecutionResultSchemaVersion,
		RequestSHA256: handle.RequestSHA256, ExecutionID: handle.ExecutionID,
		Attempt: handle.Attempt, Generation: handle.Generation,
		FencingToken: handle.FencingToken, IdempotencyKey: handle.IdempotencyKey,
		Status: status,
		Failure: &contractsv1alpha1.StageExecutionFailure{
			Code: code, Message: message, Retryable: retryable,
		},
		Completeness: "partial", CompletenessNotes: []string{code},
		CapabilitySHA256: handle.CapabilitySHA256, RecordedAt: recordedAt,
	}
}

func (platform *Platform) requestDeadline(intentID string) (time.Time, error) {
	var intent executionIntent
	if err := platform.store.GetJSON(intentID, &intent); err != nil {
		return time.Time{}, err
	}
	if intent.Deadline.IsZero() || intent.Deadline.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("Pi execution intent has invalid deadline")
	}
	return intent.Deadline, nil
}

func boundedResultTime(observed time.Time, deadline time.Time) time.Time {
	observed = observed.UTC()
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	if observed.After(deadline) {
		return deadline
	}
	return observed
}

func intentPath(key string) string {
	return "pi-execution/intents/" + shaHex([]byte(key))
}

func completionPath(intentID string) string {
	return "pi-execution/completions/" + shaHex([]byte(intentID))
}

func cancelPath(key string) string {
	return "pi-execution/cancels/" + shaHex([]byte(key))
}

func shaHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneEnvironment(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func sameCompletion(left executionCompletion, right executionCompletion) bool {
	return left.SchemaVersion == right.SchemaVersion && left.IntentID == right.IntentID &&
		left.ProviderHandle == right.ProviderHandle && left.CallbackProof == right.CallbackProof &&
		bytes.Equal(left.ResultJSON, right.ResultJSON)
}

func existingResultStatus(record executionCompletion) contractsv1alpha1.StageExecutionStatus {
	result, err := contractsv1alpha1.DecodeStageExecutionResult(record.ResultJSON)
	if err != nil {
		return ""
	}
	return result.Status
}

var _ application.AgentStageResultCallbackVerifier = (*Platform)(nil)

func (platform *Platform) VerifyAgentStageResultCallback(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	binding runmodel.AgentStageExecutionBinding,
	trust contractsv1alpha1.ExecutorTrust,
	canonicalResult []byte,
	proof []byte,
) (runmodel.AgentStageResultCallbackVerifierRef, error) {
	if ctx == nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, err
	}
	if planningSubject(subject) != platform.ledgerSub || binding.Subject != platform.ledgerSub {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"Pi callback escaped bound subject",
		)
	}
	if err := trust.Validate(); err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"validate frozen Pi executor trust: %w",
			err,
		)
	}
	wantCapabilityVerifier := contractsv1alpha1.VersionedRef{
		ID: capabilityVerifierID, Revision: "v1", SHA256: platform.config.Manifest.SHA256,
	}
	wantCallbackVerifier := contractsv1alpha1.VersionedRef{
		ID: callbackVerifierID, Revision: "v1", SHA256: platform.config.Manifest.SHA256,
	}
	if trust.Authority != contractsv1alpha1.ExecutorTrustAuthorityLocalHost ||
		trust.CapabilityVerifier != wantCapabilityVerifier ||
		trust.CallbackVerifier != wantCallbackVerifier {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"frozen request trust differs from local Pi trust root",
		)
	}
	intentID := intentPath(binding.CreateIdempotencyKey)
	var intent executionIntent
	if err := platform.store.GetJSON(intentID, &intent); err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, err
	}
	if intent.Handle.ProviderHandle != binding.ProviderHandle ||
		intent.Handle.ExecutionID != binding.ExecutionID ||
		intent.Handle.RequestSHA256 != binding.RequestSemanticSHA256 ||
		intent.Handle.CapabilitySHA256 != binding.CapabilitySHA256 {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"Pi callback binding differs from provider intent",
		)
	}
	completion, found, err := platform.loadCompletion(intentID)
	if err != nil || !found {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"Pi completion ledger has no callback decision",
		)
	}
	if !bytes.Equal(completion.ResultJSON, canonicalResult) ||
		!bytes.Equal([]byte(completion.CallbackProof), proof) {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"Pi callback result or proof differs from immutable completion",
		)
	}
	return runmodel.AgentStageResultCallbackVerifierRef{
		ID: callbackVerifierID, Revision: "v1", SHA256: platform.config.Manifest.SHA256,
	}, nil
}

func planningSubject(subject application.AgentPlanningSubject) runmodel.AgentPlanningSubject {
	return runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
}
