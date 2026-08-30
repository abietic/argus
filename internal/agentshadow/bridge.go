package agentshadow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"argus.local/argus/internal/agentshadowworker"
	"argus.local/argus/internal/pireviewmap"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	maxWorkerScriptBytes         = 16 << 20
	maxNodeBinaryBytes           = 512 << 20
	maxBuiltinSkillBytes         = 64 << 10
	maxWorkerReportBytes         = 16 << 20
	maxWorkerRequestBytes        = 96 << 20
	maxWorkerPackageBytes        = 64 << 20
	maxWorkerPackageFiles        = 256
	redactedWorkerFailureMessage = "worker reported a redacted failure"
)

var defaultLocalSkills = []string{
	"concurrency-data",
	"correctness",
	"error-contract",
	"resource-lifecycle",
	"security-contract",
	"transaction-state",
}

var localWorkerTools = []string{
	"list_files",
	"read_file",
	"search_code",
}

type LocalBridge struct {
	repository        *Repository
	service           *Service
	runner            agentshadowworker.Runner
	importShadow      func(context.Context, ImportRequest) (Result, error)
	completeExecution func(executionIntent, executionCompletion) (executionCompletion, bool, error)
}

func NewLocalBridge(
	repository *Repository,
	runner agentshadowworker.Runner,
) (*LocalBridge, error) {
	if repository == nil || runner == nil {
		return nil, fmt.Errorf("agent shadow repository and worker runner are required")
	}
	service, err := NewService(repository)
	if err != nil {
		return nil, err
	}
	return &LocalBridge{
		repository:        repository,
		service:           service,
		runner:            runner,
		importShadow:      service.Import,
		completeExecution: repository.completeExecution,
	}, nil
}

func (bridge *LocalBridge) Run(
	ctx context.Context,
	request LocalRunRequest,
) (LocalRunResult, error) {
	if ctx == nil || bridge == nil || bridge.repository == nil ||
		bridge.service == nil || bridge.runner == nil || bridge.importShadow == nil ||
		bridge.completeExecution == nil {
		return LocalRunResult{}, fmt.Errorf("initialized local agent review bridge is required")
	}
	if err := ctx.Err(); err != nil {
		return LocalRunResult{}, err
	}
	if err := validateIdentifier("source_run_id", request.SourceRunID); err != nil {
		return LocalRunResult{}, err
	}
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return LocalRunResult{}, err
	}

	prepared, err := bridge.prepare(ctx, request)
	if err != nil {
		return LocalRunResult{}, err
	}
	intent, acquired, err := bridge.repository.acquireExecutionIntent(prepared.intent)
	if err != nil {
		return LocalRunResult{}, err
	}
	if !acquired {
		return bridge.resolveExistingCompletion(ctx, intent)
	}

	deadlineContext, cancel := context.WithDeadline(ctx, prepared.workerRequest.Deadline)
	defer cancel()
	processResult, err := bridge.runner.Run(deadlineContext, agentshadowworker.Request{
		NodePath: request.NodePath, ScriptPath: request.WorkerScript,
		Input: prepared.workerRequestData, Environment: cloneStrings(request.Environment),
		MaxInputBytes:  maxWorkerRequestBytes,
		MaxOutputBytes: int64(prepared.workerRequest.Capability.MaxOutputBytes) + 64<<10,
		MaxStderrBytes: agentshadowworker.DefaultMaxStderrBytes,
	})
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx,
			intent,
			LocalRunResult{},
			ExecutionHostFailureWorkerRun,
			ExecutionHostFailureRunnerUnconfirmed,
			fmt.Errorf(
				"worker execution ended without an accepted result; retry is unsafe: %w",
				err,
			),
		)
	}
	workerResult, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(processResult.Stdout)
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx,
			intent,
			LocalRunResult{},
			ExecutionHostFailureResultDecode,
			ExecutionHostFailureResultRejected,
			fmt.Errorf("reject worker result: %w", err),
		)
	}
	if err := contractsv1alpha1.ValidateAgentReviewWorkerResultBinding(
		prepared.workerRequest,
		workerResult,
	); err != nil {
		return bridge.returnPostIntentFailure(
			ctx,
			intent,
			LocalRunResult{},
			ExecutionHostFailureResultBinding,
			ExecutionHostFailureBindingRejected,
			fmt.Errorf("reject stale or mismatched worker result: %w", err),
		)
	}
	if workerResult.Status != contractsv1alpha1.AgentReviewWorkerSucceeded {
		completion := failedExecutionCompletion(intent, workerResult)
		if _, _, err := bridge.completeExecution(intent, completion); err != nil {
			return bridge.returnPostIntentFailure(
				ctx,
				intent,
				LocalRunResult{},
				ExecutionHostFailureCompletionCommit,
				ExecutionHostFailureCompletionUnconfirmed,
				err,
			)
		}
		return bridge.returnWithAttempt(ctx, intent, LocalRunResult{
			OutcomeAcknowledged: true,
		}, &WorkerReportedError{
			Status:  workerResult.Status,
			Failure: *completion.Failure,
		})
	}

	artifacts, err := pireviewmap.MapShadowReviewReportWithArtifacts(
		prepared.plan,
		prepared.reviewInput,
		workerResult.CompletedAt,
		workerResult.Report,
	)
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx,
			intent,
			LocalRunResult{},
			ExecutionHostFailureEvidenceMapping,
			ExecutionHostFailureMappingRejected,
			fmt.Errorf("map worker report to shadow evidence: %w", err),
		)
	}
	set := artifacts.Hypotheses
	receipts := artifacts.Receipts
	rawCandidates := artifacts.RawCandidates
	taskEvidence := artifacts.TaskEvidence
	planData, err := json.Marshal(prepared.plan)
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx, intent, LocalRunResult{},
			ExecutionHostFailureEvidenceEncoding,
			ExecutionHostFailureEncodingFailed,
			err,
		)
	}
	setData, err := json.Marshal(set)
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx, intent, LocalRunResult{},
			ExecutionHostFailureEvidenceEncoding,
			ExecutionHostFailureEncodingFailed,
			err,
		)
	}
	receiptData, err := json.Marshal(receipts)
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx, intent, LocalRunResult{},
			ExecutionHostFailureEvidenceEncoding,
			ExecutionHostFailureEncodingFailed,
			err,
		)
	}
	rawCandidateData, err := json.Marshal(rawCandidates)
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx, intent, LocalRunResult{},
			ExecutionHostFailureEvidenceEncoding,
			ExecutionHostFailureEncodingFailed,
			err,
		)
	}
	taskEvidenceData, err := json.Marshal(taskEvidence)
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx, intent, LocalRunResult{},
			ExecutionHostFailureEvidenceEncoding,
			ExecutionHostFailureEncodingFailed,
			err,
		)
	}
	if _, err := bridge.authorizeExecutionImport(
		intent, planData, setData, rawCandidateData, taskEvidenceData, receiptData,
	); err != nil {
		return bridge.returnPostIntentFailure(
			ctx,
			intent,
			LocalRunResult{},
			ExecutionHostFailureShadowImport,
			ExecutionHostFailureImportUnconfirmed,
			fmt.Errorf("authorize mapped shadow evidence import: %w", err),
		)
	}
	imported, err := bridge.importShadow(ctx, ImportRequest{
		Scope:          Scope{TenantID: LocalTenantID, WorkspaceID: LocalWorkspaceID},
		IdempotencyKey: request.IdempotencyKey,
		Plan:           planData, HypothesisSet: setData,
		RawCandidateCollection: rawCandidateData,
		TaskEvidenceCollection: taskEvidenceData, ReceiptCollection: receiptData,
	})
	if err != nil {
		return bridge.returnPostIntentFailure(
			ctx,
			intent,
			LocalRunResult{},
			ExecutionHostFailureShadowImport,
			ExecutionHostFailureImportUnconfirmed,
			fmt.Errorf("import mapped shadow evidence: %w", err),
		)
	}
	completion := executionCompletion{
		SchemaVersion: executionCompletionSchemaVersion,
		TenantID:      LocalTenantID, WorkspaceID: LocalWorkspaceID,
		IdempotencyKey: request.IdempotencyKey,
		SemanticDigest: intent.SemanticDigest,
		Status:         contractsv1alpha1.AgentReviewWorkerSucceeded,
		ManifestID:     imported.Manifest.ManifestID,
		CompletedAt:    workerResult.CompletedAt,
	}
	if _, _, err := bridge.completeExecution(intent, completion); err != nil {
		return bridge.returnPostIntentFailure(
			ctx,
			intent,
			LocalRunResult{Result: imported},
			ExecutionHostFailureCompletionCommit,
			ExecutionHostFailureCompletionUnconfirmed,
			err,
		)
	}
	return bridge.returnWithAttempt(
		ctx,
		intent,
		LocalRunResult{Result: imported, OutcomeAcknowledged: true},
		nil,
	)
}

func (bridge *LocalBridge) resolveExistingCompletion(
	ctx context.Context,
	intent executionIntent,
) (LocalRunResult, error) {
	attempt, result, _, err := bridge.service.reconcileExecutionIntent(ctx, intent)
	if err != nil {
		return bridge.returnWithAttempt(
			ctx,
			intent,
			LocalRunResult{Reused: true},
			err,
		)
	}
	localResult := LocalRunResult{Attempt: &attempt, Reused: true}
	switch attempt.Status {
	case ExecutionStatusUnknownOutcome:
		return localResult, ErrUnknownOutcome
	case ExecutionStatusFailed:
		localResult.OutcomeAcknowledged = true
		return localResult, &WorkerReportedError{
			Status:  contractsv1alpha1.AgentReviewWorkerFailed,
			Failure: *attempt.Failure,
		}
	case ExecutionStatusCanceled:
		localResult.OutcomeAcknowledged = true
		return localResult, &WorkerReportedError{
			Status:  contractsv1alpha1.AgentReviewWorkerCanceled,
			Failure: *attempt.Failure,
		}
	case ExecutionStatusSucceeded:
		if result == nil {
			return LocalRunResult{}, fmt.Errorf("%w: succeeded execution has no shadow result", ErrCorrupt)
		}
		localResult.Result = *result
		localResult.OutcomeAcknowledged = true
		return localResult, nil
	default:
		return LocalRunResult{}, fmt.Errorf("%w: unsupported projected execution status %q", ErrCorrupt, attempt.Status)
	}
}

func (bridge *LocalBridge) returnPostIntentFailure(
	ctx context.Context,
	intent executionIntent,
	result LocalRunResult,
	stage ExecutionHostFailureStage,
	reasonCode ExecutionHostFailureCode,
	primary error,
) (LocalRunResult, error) {
	if primary == nil {
		return LocalRunResult{}, fmt.Errorf("post-intent failure requires a primary error")
	}
	// This call did not receive a successful terminal-outcome acknowledgement.
	// Keep that fact false even if a strict projection discovers that an
	// unknown-outcome write actually became visible before returning its error.
	result.OutcomeAcknowledged = false
	if _, err := bridge.repository.observeExecutionHostFailure(intent, stage, reasonCode); err != nil {
		primary = fmt.Errorf("%w; persist bounded host failure observation: %v", primary, err)
	}
	return bridge.returnWithAttempt(ctx, intent, result, primary)
}

// returnWithAttempt preserves the primary bridge error while attaching the
// strict state visible after intent acquisition. If projection itself fails,
// wrapping with %w keeps errors.Is/errors.As behavior for the primary failure.
func (bridge *LocalBridge) returnWithAttempt(
	ctx context.Context,
	intent executionIntent,
	result LocalRunResult,
	primary error,
) (LocalRunResult, error) {
	attempt, _, err := bridge.service.projectExecutionAttempt(
		context.WithoutCancel(ctx),
		intent,
	)
	if err != nil {
		result.OutcomeAcknowledged = false
		if primary != nil {
			return result, fmt.Errorf("%w; project execution attempt: %v", primary, err)
		}
		return result, err
	}
	result.Attempt = &attempt
	if result.OutcomeAcknowledged && attempt.Status == ExecutionStatusUnknownOutcome {
		result.OutcomeAcknowledged = false
	}
	return result, primary
}

type preparedLocalRun struct {
	plan              contractsv1alpha1.AgentReviewPlan
	workerRequest     contractsv1alpha1.AgentReviewWorkerRequest
	workerRequestData []byte
	reviewInput       reviewcore.ReviewInput
	intent            executionIntent
}

func (bridge *LocalBridge) prepare(
	ctx context.Context,
	request LocalRunRequest,
) (preparedLocalRun, error) {
	if err := validateProviderEnvironment(
		request.ProviderProfile,
		request.Model,
		request.Environment,
	); err != nil {
		return preparedLocalRun{}, err
	}
	if err := validateLocalWorkerBudget(request.Budget); err != nil {
		return preparedLocalRun{}, err
	}
	nodeDigest, _, err := digestRegularFile(
		"node",
		request.NodePath,
		maxNodeBinaryBytes,
	)
	if err != nil {
		return preparedLocalRun{}, err
	}
	scriptDigest, _, err := digestRegularFile(
		"worker script",
		request.WorkerScript,
		maxWorkerScriptBytes,
	)
	if err != nil {
		return preparedLocalRun{}, err
	}
	packageDigest, err := digestWorkerPackageManifest(request.WorkerScript)
	if err != nil {
		return preparedLocalRun{}, err
	}
	skills, skillContents, err := loadBuiltinSkills(request.WorkerScript, request.Skills)
	if err != nil {
		return preparedLocalRun{}, err
	}

	sourceRun, err := bridge.repository.runs.LoadRun(request.SourceRunID)
	if err != nil {
		return preparedLocalRun{}, fmt.Errorf("load committed source run: %w", err)
	}
	if sourceRun.Status != runmodel.RunStatusSucceeded {
		return preparedLocalRun{}, fmt.Errorf(
			"source run status is %q; local agent review requires a succeeded committed run",
			sourceRun.Status,
		)
	}
	snapshot, err := bridge.repository.runs.LoadExecutionSnapshot(
		sourceRun.ExecutionSnapshotID,
	)
	if err != nil {
		return preparedLocalRun{}, fmt.Errorf("load committed execution snapshot: %w", err)
	}
	if snapshot.TargetSnapshotRef != sourceRun.TargetSnapshotRef {
		return preparedLocalRun{}, fmt.Errorf("source run target does not match its execution snapshot")
	}
	if _, err := bridge.repository.runs.ReadArtifact(snapshot.TargetSnapshotRef); err != nil {
		return preparedLocalRun{}, fmt.Errorf("read committed materialized target: %w", err)
	}
	reviewInputData, err := bridge.repository.runs.ReadArtifact(snapshot.ReviewInputRef)
	if err != nil {
		return preparedLocalRun{}, fmt.Errorf("read committed review input: %w", err)
	}
	reviewInput, err := reviewcore.DecodeReviewInput(reviewInputData)
	if err != nil {
		return preparedLocalRun{}, err
	}
	targetDigest, err := reviewcore.DigestReviewInput(reviewInput)
	if err != nil {
		return preparedLocalRun{}, err
	}
	if targetDigest != snapshot.ReviewInputRef.SHA256 {
		return preparedLocalRun{}, fmt.Errorf(
			"committed review input artifact is not the canonical ReviewInput encoding",
		)
	}
	if len(reviewInputData) > math.MaxInt64 ||
		uint64(len(reviewInputData)) > request.Budget.MaxTargetBytes {
		return preparedLocalRun{}, fmt.Errorf("review input exceeds max_target_bytes")
	}
	if uint64(len(reviewInput.Files)) > uint64(request.Budget.MaxFiles) {
		return preparedLocalRun{}, fmt.Errorf("review input exceeds max_files")
	}
	snapshotData, err := json.Marshal(snapshot)
	if err != nil {
		return preparedLocalRun{}, fmt.Errorf("marshal committed execution snapshot: %w", err)
	}
	snapshotRef, err := bridge.repository.runs.PutArtifact(
		runmodel.SnapshotSchemaVersion,
		snapshotData,
	)
	if err != nil {
		return preparedLocalRun{}, fmt.Errorf("artifactize committed execution snapshot: %w", err)
	}

	createdAt, err := bridge.repository.normalizedNow()
	if err != nil {
		return preparedLocalRun{}, err
	}
	executionID := "agent-review-execution-" + digestStrings(
		LocalTenantID,
		LocalWorkspaceID,
		request.SourceRunID,
		request.IdempotencyKey,
	)[:24]
	planID := "agent-review-plan-" + digestStrings(executionID)[:24]
	providerBase := providerBaseURL(request.ProviderProfile, request.Environment)
	rulePackRef, rulePackBytes, err := localShadowRulePack()
	if err != nil {
		return preparedLocalRun{}, err
	}
	plan := contractsv1alpha1.AgentReviewPlan{
		SchemaVersion: contractsv1alpha1.AgentReviewPlanSchemaVersion,
		PlanID:        planID, SourceRunID: sourceRun.RunID,
		ExecutionID: executionID, ReviewRunID: sourceRun.RunID,
		ExecutionSnapshotRef: bindingFromRunRef(snapshotRef),
		ReviewInputRef:       bindingFromRunRef(snapshot.ReviewInputRef),
		TargetDigest:         targetDigest,
		RulePack:             rulePackRef,
		Implementation:       versionedRef("pi-review-worker", "v0", packageDigest),
		Normalization:        versionedRef(contractsv1alpha1.CandidateNormalizationImplementationID, contractsv1alpha1.CandidateNormalizationCurrentRevision, packageDigest),
		Grouping:             versionedRef("directory-language", "v0", packageDigest),
		ContextDimensions: []contractsv1alpha1.VersionedRef{
			versionedRef("code-context", "v0", packageDigest),
		},
		ReviewDimensions: skills,
		Verifier:         versionedRef("independent-verifier", "v0", packageDigest),
		Knowledge:        []contractsv1alpha1.VersionedRef{},
		Runtime:          versionedRef("node-runtime", "binary", nodeDigest),
		Profile: versionedRef(
			"local-shadow-stdio",
			"v1",
			digestText("local-shadow-stdio-v1\x00"+request.ProviderProfile+"\x00"+providerBase),
		),
		Agent: versionedRef("pi-agent", "worker-v0", packageDigest),
		Provider: versionedRef(
			providerID(request.ProviderProfile),
			"v1",
			digestText(request.ProviderProfile+"\x00"+providerBase),
		),
		Model:       versionedRef(request.Model, "provider", digestText(request.Model)),
		APIProtocol: contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
		ToolPolicy: contractsv1alpha1.AgentReviewToolPolicy{
			AllowedTools: slices.Clone(localWorkerTools),
			ToolNetwork:  "deny", WorkspaceWrites: "deny", RemoteWrites: "deny",
		},
		Budget: request.Budget, VerificationRequired: true,
		ExecutionClass: contractsv1alpha1.AgentReviewExecutionLocalDirectProviderShadow,
		Attestation:    contractsv1alpha1.AgentReviewAttestationNonAttested,
		SideEffects:    "deny", CreatedAt: createdAt,
	}
	if err := plan.Validate(); err != nil {
		return preparedLocalRun{}, err
	}
	capability := contractsv1alpha1.AgentReviewWorkerCapability{
		Protocol:       contractsv1alpha1.AgentReviewWorkerProtocolStdio,
		FrozenInput:    contractsv1alpha1.AgentReviewWorkerFrozenInputBase64,
		MaxInputBytes:  request.Budget.MaxTargetBytes,
		MaxOutputBytes: maxWorkerReportBytes,
	}
	capability.SHA256, err = contractsv1alpha1.DigestAgentReviewWorkerCapability(capability)
	if err != nil {
		return preparedLocalRun{}, err
	}
	promptBytes, err := contractsv1alpha1.MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		return preparedLocalRun{}, err
	}
	promptDigest := digestBytesRaw(promptBytes)
	promptBundle, err := contractsv1alpha1.NewAgentReviewWorkerPromptBundle(
		contractsv1alpha1.VersionedRef{
			ID: "pi-review-prompts", Revision: "v0", SHA256: promptDigest,
		},
		contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:    "artifact://local-shadow/prompt-bundles/" + promptDigest,
				SHA256: promptDigest, SizeBytes: int64(len(promptBytes)),
			},
			Contract: contractsv1alpha1.AgentStagePlanPromptContract,
		},
		promptBytes,
	)
	if err != nil {
		return preparedLocalRun{}, err
	}
	reviewSkills := make([]contractsv1alpha1.AgentReviewWorkerSkill, len(skills))
	for index, skill := range skills {
		reviewSkills[index], err = contractsv1alpha1.NewAgentReviewWorkerSkill(
			skill,
			contractsv1alpha1.ArtifactBinding{
				Ref: contractsv1alpha1.ContentRef{
					URI:    "artifact://local-shadow/review-skills/" + skill.SHA256,
					SHA256: skill.SHA256, SizeBytes: int64(len(skillContents[index])),
				},
				Contract: contractsv1alpha1.AgentStagePlanSkillContract,
			},
			skillContents[index],
		)
		if err != nil {
			return preparedLocalRun{}, err
		}
	}
	workerRequest := contractsv1alpha1.AgentReviewWorkerRequest{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerRequestSchemaVersion,
		WorkItemID:    executionID, Attempt: 1, Generation: 1,
		FencingToken:     fencingToken(executionID),
		IdempotencyKey:   request.IdempotencyKey,
		Capability:       capability,
		Deadline:         createdAt.Add(time.Duration(request.Budget.TimeoutMS) * time.Millisecond),
		Plan:             plan,
		RulePackBase64:   base64.StdEncoding.EncodeToString(rulePackBytes),
		PromptBundle:     promptBundle,
		ReviewSkills:     reviewSkills,
		KnowledgePacks:   []contractsv1alpha1.AgentReviewWorkerKnowledge{},
		ContextArtifacts: []contractsv1alpha1.AgentReviewWorkerContextArtifact{},
		CheckpointScopeSHA256: digestBytesRaw([]byte(
			plan.ExecutionSnapshotRef.Ref.SHA256 + "\n" + plan.ReviewInputRef.Ref.SHA256,
		)),
		GroupCheckpoints:  []contractsv1alpha1.AgentReviewWorkerGroupCheckpoint{},
		ReviewInputBase64: base64.StdEncoding.EncodeToString(reviewInputData),
	}
	if err := workerRequest.Validate(); err != nil {
		return preparedLocalRun{}, err
	}
	workerRequestData, err := json.Marshal(workerRequest)
	if err != nil {
		return preparedLocalRun{}, err
	}
	if len(workerRequestData) > maxWorkerRequestBytes {
		return preparedLocalRun{}, fmt.Errorf("worker request exceeds %d bytes", maxWorkerRequestBytes)
	}
	semanticDigest, err := digestLocalRunSemantics(
		request,
		snapshotRef,
		snapshot.ReviewInputRef,
		targetDigest,
		nodeDigest,
		scriptDigest,
		packageDigest,
		skills,
		providerBase,
	)
	if err != nil {
		return preparedLocalRun{}, err
	}
	intent := executionIntent{
		SchemaVersion: executionIntentSchemaVersion,
		TenantID:      LocalTenantID, WorkspaceID: LocalWorkspaceID,
		IdempotencyKey:      request.IdempotencyKey,
		SemanticDigest:      semanticDigest,
		WorkerRequestSHA256: digestBytesRaw(workerRequestData),
		NodeSHA256:          nodeDigest, WorkerScriptSHA256: scriptDigest,
		WorkerPackageSHA256: packageDigest,
		Plan:                plan, AcceptedAt: createdAt,
	}
	return preparedLocalRun{
		plan: plan, workerRequest: workerRequest, workerRequestData: workerRequestData,
		reviewInput: reviewInput, intent: intent,
	}, nil
}

func localShadowRulePack() (contractsv1alpha1.VersionedRef, []byte, error) {
	pack := struct {
		SchemaVersion string          `json:"schema_version"`
		ID            string          `json:"id"`
		Revision      string          `json:"revision"`
		Rules         json.RawMessage `json:"rules"`
		SHA256        string          `json:"sha256"`
	}{
		SchemaVersion: "argus.rule_pack.v1alpha1",
		ID:            "local-shadow-review-rules",
		Revision:      "v0",
		Rules:         json.RawMessage(`[]`),
	}
	semantic, err := json.Marshal(pack)
	if err != nil {
		return contractsv1alpha1.VersionedRef{}, nil, err
	}
	pack.SHA256 = digestBytesRaw(semantic)
	content, err := json.Marshal(pack)
	if err != nil {
		return contractsv1alpha1.VersionedRef{}, nil, err
	}
	return contractsv1alpha1.VersionedRef{
		ID: pack.ID, Revision: pack.Revision, SHA256: pack.SHA256,
	}, content, nil
}

func failedExecutionCompletion(
	intent executionIntent,
	result contractsv1alpha1.AgentReviewWorkerResult,
) executionCompletion {
	failure := contractsv1alpha1.AgentReviewWorkerFailure{
		Code: result.Failure.Code, Message: redactedWorkerFailureMessage,
		Retryable: result.Failure.Retryable,
	}
	return executionCompletion{
		SchemaVersion: executionCompletionSchemaVersion,
		TenantID:      intent.TenantID, WorkspaceID: intent.WorkspaceID,
		IdempotencyKey: intent.IdempotencyKey, SemanticDigest: intent.SemanticDigest,
		Status: result.Status, Failure: &failure, CompletedAt: result.CompletedAt,
	}
}

type localRunSemantics struct {
	SourceRunID         string                              `json:"source_run_id"`
	ExecutionSnapshot   runmodel.ArtifactRef                `json:"execution_snapshot_ref"`
	ReviewInput         runmodel.ArtifactRef                `json:"review_input_ref"`
	TargetDigest        string                              `json:"target_digest"`
	NodeSHA256          string                              `json:"node_sha256"`
	WorkerScriptSHA256  string                              `json:"worker_script_sha256"`
	WorkerPackageSHA256 string                              `json:"worker_package_sha256"`
	ProviderProfile     string                              `json:"provider_profile"`
	ProviderBaseURL     string                              `json:"provider_base_url"`
	Model               string                              `json:"model"`
	Skills              []contractsv1alpha1.VersionedRef    `json:"skills"`
	Budget              contractsv1alpha1.AgentReviewBudget `json:"budget"`
}

func digestLocalRunSemantics(
	request LocalRunRequest,
	snapshotRef runmodel.ArtifactRef,
	reviewInputRef runmodel.ArtifactRef,
	targetDigest string,
	nodeDigest string,
	scriptDigest string,
	packageDigest string,
	skills []contractsv1alpha1.VersionedRef,
	providerBase string,
) (string, error) {
	data, err := json.Marshal(localRunSemantics{
		SourceRunID:       request.SourceRunID,
		ExecutionSnapshot: snapshotRef, ReviewInput: reviewInputRef,
		TargetDigest: targetDigest, NodeSHA256: nodeDigest,
		WorkerScriptSHA256:  scriptDigest,
		WorkerPackageSHA256: packageDigest,
		ProviderProfile:     request.ProviderProfile, ProviderBaseURL: providerBase,
		Model: request.Model, Skills: skills, Budget: request.Budget,
	})
	if err != nil {
		return "", err
	}
	return digestBytesRaw(data), nil
}

type workerPackageManifestEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// digestWorkerPackageManifest binds execution reuse to the bounded runtime
// package that the entrypoint can import. node_modules remains deliberately
// outside this non-attested M1.3.1 closure; package-lock.json binds its declared
// dependency graph while the plan continues to advertise non-attested status.
func digestWorkerPackageManifest(workerScript string) (string, error) {
	root := filepath.Dir(filepath.Dir(workerScript))
	dist := filepath.Join(root, "dist")
	relativeScript, err := filepath.Rel(dist, workerScript)
	if err != nil || relativeScript == "." || strings.HasPrefix(relativeScript, ".."+string(os.PathSeparator)) ||
		filepath.IsAbs(relativeScript) || filepath.Ext(relativeScript) != ".js" {
		return "", fmt.Errorf("worker script must be a direct JavaScript entrypoint under the package dist directory")
	}
	entries, err := os.ReadDir(dist)
	if err != nil {
		return "", fmt.Errorf("read worker dist package: %w", err)
	}
	paths := []string{"package-lock.json", "package.json"}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".js" {
			paths = append(paths, filepath.Join("dist", entry.Name()))
		}
	}
	slices.Sort(paths)
	if len(paths) > maxWorkerPackageFiles {
		return "", fmt.Errorf("worker package manifest exceeds %d files", maxWorkerPackageFiles)
	}
	foundScript := false
	manifest := make([]workerPackageManifestEntry, 0, len(paths))
	var total int64
	for _, relative := range paths {
		path := filepath.Join(root, relative)
		digest, size, err := digestRegularFile("worker package file", path, maxWorkerScriptBytes)
		if err != nil {
			return "", err
		}
		total += size
		if total > maxWorkerPackageBytes {
			return "", fmt.Errorf("worker package exceeds %d bytes", maxWorkerPackageBytes)
		}
		if path == workerScript {
			foundScript = true
		}
		manifest = append(manifest, workerPackageManifestEntry{
			Path: filepath.ToSlash(relative), Bytes: size, SHA256: digest,
		})
	}
	if !foundScript {
		return "", fmt.Errorf("worker script is not included in the bounded runtime package manifest")
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return digestBytesRaw(data), nil
}

func loadBuiltinSkills(
	workerScript string,
	requested []string,
) ([]contractsv1alpha1.VersionedRef, [][]byte, error) {
	selectors := slices.Clone(requested)
	if len(selectors) == 0 {
		selectors = slices.Clone(defaultLocalSkills)
	}
	slices.Sort(selectors)
	if len(selectors) == 0 || len(selectors) > len(defaultLocalSkills) {
		return nil, nil, fmt.Errorf("one to %d builtin skills are required", len(defaultLocalSkills))
	}
	root := filepath.Dir(filepath.Dir(workerScript))
	refs := make([]contractsv1alpha1.VersionedRef, 0, len(selectors))
	contents := make([][]byte, 0, len(selectors))
	for index, selector := range selectors {
		if index > 0 && selector == selectors[index-1] {
			return nil, nil, fmt.Errorf("duplicate skill %q", selector)
		}
		if !slices.Contains(defaultLocalSkills, selector) {
			return nil, nil, fmt.Errorf("unsupported local builtin skill %q", selector)
		}
		content, digest, err := readAndDigestRegularFile(
			"builtin skill",
			filepath.Join(root, "skills", selector+".md"),
			maxBuiltinSkillBytes,
		)
		if err != nil {
			return nil, nil, err
		}
		revision, err := builtinSkillRevision(content)
		if err != nil {
			return nil, nil, fmt.Errorf("builtin skill %q: %w", selector, err)
		}
		refs = append(refs, versionedRef(selector, revision, digest))
		contents = append(contents, content)
	}
	return refs, contents, nil
}

func builtinSkillRevision(content []byte) (string, error) {
	const prefix = "Revision:"
	var revision string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if revision != "" {
			return "", fmt.Errorf("must declare exactly one Revision line")
		}
		revision = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	}
	if !validBuiltinSkillRevision(revision) {
		return "", fmt.Errorf("invalid revision metadata")
	}
	return revision, nil
}

func validBuiltinSkillRevision(revision string) bool {
	const prefix = "builtin-v"
	if !strings.HasPrefix(revision, prefix) || len(revision) == len(prefix) ||
		revision[len(prefix)] == '0' {
		return false
	}
	for _, value := range revision[len(prefix):] {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

func readAndDigestRegularFile(label, path string, maximum int64) ([]byte, string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", fmt.Errorf("%s path must be clean and absolute", label)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("inspect %s: %w", label, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximum {
		return nil, "", fmt.Errorf(
			"%s must be a non-empty regular non-symlink file bounded to %d bytes",
			label,
			maximum,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s changed while opening", label)
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) != after.Size() || int64(len(content)) > maximum {
		return nil, "", fmt.Errorf("%s changed while reading", label)
	}
	digest := sha256.Sum256(content)
	return content, hex.EncodeToString(digest[:]), nil
}

func validateProviderEnvironment(
	profile string,
	model string,
	environment map[string]string,
) error {
	if strings.TrimSpace(model) == "" || model != strings.TrimSpace(model) {
		return fmt.Errorf("model is required and must be trimmed")
	}
	if strings.TrimSpace(environment["ANTHROPIC_API_KEY"]) == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY is required")
	}
	switch profile {
	case "deepseek-anthropic-env":
		if environment["ANTHROPIC_BASE_URL"] != "https://api.deepseek.com/anthropic" {
			return fmt.Errorf(
				"deepseek-anthropic-env requires the exact https://api.deepseek.com/anthropic base URL",
			)
		}
	case "anthropic-official":
		if environment["ANTHROPIC_BASE_URL"] != "" {
			return fmt.Errorf("anthropic-official forbids ANTHROPIC_BASE_URL override")
		}
	default:
		return fmt.Errorf("unsupported provider profile %q", profile)
	}
	return nil
}

func validateLocalWorkerBudget(budget contractsv1alpha1.AgentReviewBudget) error {
	if budget.MaxTargetBytes > 64<<20 || budget.MaxGroupBytes > 1<<20 ||
		budget.MaxFiles > 1000 || budget.MaxGroups > 256 ||
		budget.MaxCandidates > 1000 || budget.MaxModelCalls > 2000 ||
		budget.MaxToolCalls > 100 || budget.MaxOutputTokens > 32768 ||
		budget.TimeoutMS > uint64(time.Hour/time.Millisecond) ||
		budget.MaxConcurrency > 16 {
		return fmt.Errorf("agent review budget exceeds frozen worker admission limits")
	}
	return nil
}

func providerBaseURL(profile string, environment map[string]string) string {
	if profile == "deepseek-anthropic-env" {
		return environment["ANTHROPIC_BASE_URL"]
	}
	return "https://api.anthropic.com"
}

func providerID(profile string) string {
	if profile == "deepseek-anthropic-env" {
		return "deepseek-anthropic"
	}
	return "anthropic"
}

func digestRegularFile(
	label string,
	path string,
	maximum int64,
) (string, int64, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", 0, fmt.Errorf("%s path must be clean and absolute", label)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", 0, fmt.Errorf("inspect %s: %w", label, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximum {
		return "", 0, fmt.Errorf(
			"%s must be a non-empty regular non-symlink file bounded to %d bytes",
			label,
			maximum,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s changed while opening", label)
	}
	digest := sha256.New()
	if err := copyExactDigest(digest, file, after.Size(), maximum); err != nil {
		return "", 0, fmt.Errorf("digest %s: %w", label, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), after.Size(), nil
}

func copyExactDigest(digest hash.Hash, source io.Reader, size, maximum int64) error {
	written, err := io.Copy(digest, io.LimitReader(source, maximum+1))
	if err != nil {
		return err
	}
	if written != size || written > maximum {
		return fmt.Errorf("file size changed while reading")
	}
	return nil
}

func versionedRef(id, revision, digest string) contractsv1alpha1.VersionedRef {
	return contractsv1alpha1.VersionedRef{ID: id, Revision: revision, SHA256: digest}
}

func fencingToken(executionID string) uint64 {
	digest := sha256.Sum256([]byte(executionID))
	value := uint64(0)
	for _, current := range digest[:8] {
		value = value<<8 | uint64(current)
	}
	value &= (1 << 53) - 1
	if value == 0 {
		return 1
	}
	return value
}

func digestText(value string) string { return digestBytesRaw([]byte(value)) }

func digestBytesRaw(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
