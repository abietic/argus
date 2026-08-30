package pireviewmap

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"argus.local/argus/internal/reviewcore"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var frozenWorkerTools = []string{
	"list_files",
	"read_file",
	"search_code",
}

var ErrFormalReviewNoCompletedGroups = errors.New(
	"formal Pi report completed no review group",
)

const maxFormalWorkerCompletionGrace = 5 * time.Second

// FormalReviewRetryableError is emitted only after the complete worker report
// and its task receipts have passed host validation. Code is a bounded host
// taxonomy derived from every failed review task; the worker's free-form
// failure text is never propagated through this boundary.
type FormalReviewRetryableError struct {
	Code  string
	Cause error
}

func (failure *FormalReviewRetryableError) Error() string {
	return failure.Cause.Error()
}

func (failure *FormalReviewRetryableError) Unwrap() error {
	return failure.Cause
}

// FormalMapInput closes the outer formal request over the exact inner frozen
// worker exchange. AgentReviewWorkerRequest remains a subprocess transport
// contract; its non-attested classification does not weaken the outer
// platform callback and terminal admission performed by the application.
type FormalMapInput struct {
	Plan          contractsv1alpha1.AgentStagePlan
	Request       contractsv1alpha1.StageExecutionRequest
	ReviewInput   reviewcore.ReviewInput
	ModelProfile  []byte
	WorkerRequest contractsv1alpha1.AgentReviewWorkerRequest
	WorkerResult  contractsv1alpha1.AgentReviewWorkerResult
}

// BuildFormalWorkerRequest lowers one immutable formal plan/request/input into
// the narrow frozen Pi stdio protocol. The worker envelope remains explicitly
// non-attested; provider authenticity and terminal authority stay outside it
// in Argus' PlatformPort callback and admission ledgers.
func BuildFormalWorkerRequest(
	plan contractsv1alpha1.AgentStagePlan,
	request contractsv1alpha1.StageExecutionRequest,
	reviewInput reviewcore.ReviewInput,
	modelProfileBytes []byte,
	promptBundleBytes []byte,
	reviewSkillBytes [][]byte,
	knowledgePackBytes [][]byte,
	contextArtifactBytes [][]byte,
	rulePackBytes []byte,
) (contractsv1alpha1.AgentReviewWorkerRequest, error) {
	if err := plan.Validate(); err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"validate formal AgentStagePlan: %w",
			err,
		)
	}
	if err := request.Validate(); err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"validate formal StageExecutionRequest: %w",
			err,
		)
	}
	if err := reviewInput.Validate(); err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"validate frozen ReviewInput: %w",
			err,
		)
	}
	if base64.StdEncoding.EncodeToString(rulePackBytes) != plan.RulePackBase64 {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"formal Pi rule pack bytes do not match AgentStagePlan",
		)
	}
	if err := validateFrozenWorkerBudgetRange(plan.Budget); err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	modelProfile, err := decodeFormalModelProfile(plan, modelProfileBytes)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	if plan.Budget.MaxTargetBytes <= 0 || plan.Budget.MaxOutputBytes <= 0 {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"formal Pi IO budgets must be positive",
		)
	}
	if uint64(plan.Budget.MaxTargetBytes) > uint64(1<<53-1) ||
		uint64(plan.Budget.MaxOutputBytes) > uint64(1<<53-1) {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"formal Pi IO budgets must be JSON-safe integers",
		)
	}
	review, grouping, contextSkill, verifier, err := formalPiSkills(plan)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	planData, err := json.Marshal(plan)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	if request.Plan.Ref.SHA256 != digestBytesRaw(planData) ||
		request.Plan.Ref.SizeBytes != int64(len(planData)) ||
		request.ReviewRunID != plan.ReviewRunID || request.Stage != plan.Stage ||
		request.ExecutionSnapshot != plan.ExecutionSnapshot ||
		request.ReviewInput != plan.ReviewInput || request.OutputContract != plan.OutputContract ||
		request.SideEffects != plan.SideEffects || len(request.Upstream) != 0 {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"formal StageExecutionRequest does not bind the exact AgentStagePlan",
		)
	}
	if err := validateFormalExecutorCapability(plan, request.Capability); err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	inputData, err := json.Marshal(reviewInput)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"marshal canonical frozen ReviewInput: %w",
			err,
		)
	}
	targetDigest, err := reviewcore.DigestReviewInput(reviewInput)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	if targetDigest != plan.TargetDigest || targetDigest != request.ReviewInput.Ref.SHA256 ||
		int64(len(inputData)) != request.ReviewInput.Ref.SizeBytes {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"frozen ReviewInput does not bind the formal target",
		)
	}
	contextArtifacts, err := buildFormalContextArtifacts(reviewInput, contextArtifactBytes)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	promptBundle, err := contractsv1alpha1.NewAgentReviewWorkerPromptBundle(
		plan.Prompt.Ref,
		plan.Prompt.Artifact,
		promptBundleBytes,
	)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"bind formal Pi prompt bundle: %w",
			err,
		)
	}
	if len(reviewSkillBytes) != len(review) {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"formal Pi review skill bytes do not match review skill bindings",
		)
	}
	reviewSkillBundles := make([]contractsv1alpha1.AgentReviewWorkerSkill, len(review))
	reviewIndex := 0
	for _, skill := range plan.Skills {
		if skill.Phase != contractsv1alpha1.AgentStageSkillPhaseReview {
			continue
		}
		reviewSkillBundles[reviewIndex], err = contractsv1alpha1.NewAgentReviewWorkerSkill(
			skill.Ref,
			skill.Artifact,
			reviewSkillBytes[reviewIndex],
		)
		if err != nil {
			return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
				"bind formal Pi review skill %q: %w",
				skill.ID,
				err,
			)
		}
		reviewIndex++
	}
	if len(knowledgePackBytes) != len(plan.Knowledge) {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"formal Pi knowledge bytes do not match knowledge bindings",
		)
	}
	knowledgeBundles := make([]contractsv1alpha1.AgentReviewWorkerKnowledge, len(plan.Knowledge))
	knowledgeRefs := make([]contractsv1alpha1.VersionedRef, len(plan.Knowledge))
	for index, binding := range plan.Knowledge {
		knowledgeBundles[index], err = contractsv1alpha1.NewAgentReviewWorkerKnowledge(
			binding.Ref, binding.Artifact, knowledgePackBytes[index],
		)
		if err != nil {
			return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
				"bind formal Pi knowledge %q: %w", binding.ID, err,
			)
		}
		knowledgeRefs[index] = binding.Ref
	}
	createdAt := request.Deadline.Add(-time.Duration(plan.Budget.TimeoutMS) * time.Millisecond)
	if createdAt.IsZero() || !request.Deadline.After(createdAt) {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"formal worker creation time cannot be derived from immutable deadline",
		)
	}
	workerDeadline, err := formalWorkerDeadline(createdAt, request.Deadline)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	workerPlan := contractsv1alpha1.AgentReviewPlan{
		SchemaVersion: contractsv1alpha1.AgentReviewPlanSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.ReviewRunID,
		ExecutionID: request.ExecutionID, ReviewRunID: plan.ReviewRunID,
		ExecutionSnapshotRef: request.ExecutionSnapshot,
		ReviewInputRef:       request.ReviewInput, TargetDigest: plan.TargetDigest,
		RulePack: plan.RulePack,
		Implementation: contractsv1alpha1.VersionedRef{
			ID: "pi-review-worker", Revision: "v0", SHA256: plan.Agent.Ref.SHA256,
		},
		Normalization:     plan.Normalization,
		Grouping:          grouping.Ref,
		ContextDimensions: []contractsv1alpha1.VersionedRef{contextSkill.Ref},
		ReviewDimensions:  append([]contractsv1alpha1.VersionedRef{}, review...),
		Verifier:          verifier.Ref, Knowledge: knowledgeRefs,
		Runtime: plan.Runtime.Ref,
		Profile: contractsv1alpha1.VersionedRef{
			ID: "local-shadow-stdio", Revision: "v1",
			SHA256: plan.ModelAuthority.APIProtocol.Ref.SHA256,
		},
		Agent: plan.Agent.Ref, Provider: plan.Provider.Ref,
		Model: contractsv1alpha1.VersionedRef{
			ID: modelProfile.WireModel, Revision: plan.Model.Ref.Revision,
			SHA256: plan.Model.Ref.SHA256,
		},
		APIProtocol: contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
		ToolPolicy: contractsv1alpha1.AgentReviewToolPolicy{
			AllowedTools:    append([]string{}, plan.ToolAuthority.Tools...),
			ToolNetwork:     plan.ToolAuthority.ToolNetwork,
			WorkspaceWrites: plan.ToolAuthority.WorkspaceWrites,
			RemoteWrites:    plan.ToolAuthority.RemoteWrites,
		},
		Budget: formalWorkerBudget(plan.Budget), VerificationRequired: true,
		ExecutionClass: contractsv1alpha1.AgentReviewExecutionLocalDirectProviderShadow,
		Attestation:    contractsv1alpha1.AgentReviewAttestationNonAttested,
		SideEffects:    plan.SideEffects, CreatedAt: createdAt,
	}
	capability := contractsv1alpha1.AgentReviewWorkerCapability{
		Protocol:       contractsv1alpha1.AgentReviewWorkerProtocolStdio,
		FrozenInput:    contractsv1alpha1.AgentReviewWorkerFrozenInputBase64,
		MaxInputBytes:  uint64(plan.Budget.MaxTargetBytes),
		MaxOutputBytes: uint64(plan.Budget.MaxOutputBytes),
	}
	capability.SHA256, err = contractsv1alpha1.DigestAgentReviewWorkerCapability(capability)
	if err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, err
	}
	workerRequest := contractsv1alpha1.AgentReviewWorkerRequest{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerRequestSchemaVersion,
		WorkItemID:    request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, Capability: capability,
		Deadline: workerDeadline, Plan: workerPlan,
		RulePackBase64:   base64.StdEncoding.EncodeToString(rulePackBytes),
		PromptBundle:     promptBundle,
		ReviewSkills:     reviewSkillBundles,
		KnowledgePacks:   knowledgeBundles,
		ContextArtifacts: contextArtifacts,
		CheckpointScopeSHA256: digestBytesRaw([]byte(
			request.Plan.Ref.SHA256 + "\n" + request.ReviewInput.Ref.SHA256,
		)),
		GroupCheckpoints:  []contractsv1alpha1.AgentReviewWorkerGroupCheckpoint{},
		ReviewInputBase64: base64.StdEncoding.EncodeToString(inputData),
	}
	if err := workerRequest.Validate(); err != nil {
		return contractsv1alpha1.AgentReviewWorkerRequest{}, fmt.Errorf(
			"validate frozen Pi worker request: %w",
			err,
		)
	}
	return workerRequest, nil
}

func decodeFormalModelProfile(
	plan contractsv1alpha1.AgentStagePlan,
	content []byte,
) (contractsv1alpha1.ModelProfile, error) {
	if plan.Model.Artifact.Contract != contractsv1alpha1.ModelProfileSchemaVersion {
		return contractsv1alpha1.ModelProfile{}, fmt.Errorf(
			"formal model artifact contract must be %q",
			contractsv1alpha1.ModelProfileSchemaVersion,
		)
	}
	if int64(len(content)) != plan.Model.Artifact.Ref.SizeBytes ||
		digestBytesRaw(content) != plan.Model.Artifact.Ref.SHA256 ||
		plan.Model.Ref.SHA256 != plan.Model.Artifact.Ref.SHA256 {
		return contractsv1alpha1.ModelProfile{}, fmt.Errorf(
			"formal model profile bytes do not bind their component artifact",
		)
	}
	profile, err := contractsv1alpha1.DecodeModelProfile(content)
	if err != nil {
		return contractsv1alpha1.ModelProfile{}, fmt.Errorf(
			"decode formal model profile: %w",
			err,
		)
	}
	if profile.ProviderID != plan.Provider.Ref.ID {
		return contractsv1alpha1.ModelProfile{}, fmt.Errorf(
			"formal model profile provider does not bind the selected provider component",
		)
	}
	return profile, nil
}

func buildFormalContextArtifacts(
	reviewInput reviewcore.ReviewInput,
	contents [][]byte,
) ([]contractsv1alpha1.AgentReviewWorkerContextArtifact, error) {
	refCount := 0
	for _, binding := range reviewInput.Contexts {
		if binding.Ref != nil {
			refCount++
		}
	}
	if len(contents) != refCount {
		return nil, fmt.Errorf(
			"formal context artifact bytes count %d does not match frozen refs %d",
			len(contents),
			refCount,
		)
	}
	transports := make([]contractsv1alpha1.AgentReviewWorkerContextArtifact, 0, refCount)
	contentIndex := 0
	for index, binding := range reviewInput.Contexts {
		if binding.Ref == nil {
			continue
		}
		ref := binding.Ref
		transport, err := contractsv1alpha1.NewAgentReviewWorkerContextArtifact(
			ref.ContextID,
			ref.Kind,
			ref.Revision,
			contractsv1alpha1.ArtifactBinding{
				Ref: contractsv1alpha1.ContentRef{
					URI:       ref.ArtifactURI,
					SHA256:    ref.Digest,
					SizeBytes: ref.SizeBytes,
				},
				Contract: ref.Contract,
			},
			contents[contentIndex],
		)
		if err != nil {
			return nil, fmt.Errorf(
				"bind formal context artifact %d %q: %w",
				index,
				ref.ContextID,
				err,
			)
		}
		transports = append(transports, transport)
		contentIndex++
	}
	return transports, nil
}

// MapFormalReviewResult strictly maps one succeeded frozen Pi worker report
// into the formal ReviewHypothesisSet contract. It does not publish artifacts,
// admit a callback, or decide the result/cancel terminal race.
func MapFormalReviewResult(
	input FormalMapInput,
) (contractsv1alpha1.ReviewHypothesisSet, error) {
	artifacts, err := MapFormalReviewArtifacts(input)
	return artifacts.Hypotheses, err
}

type FormalReviewArtifacts struct {
	Hypotheses    contractsv1alpha1.ReviewHypothesisSet
	RawCandidates contractsv1alpha1.AgentReviewRawCandidateCollection
	Receipts      contractsv1alpha1.AgentExecutionReceiptCollection
	TaskEvidence  contractsv1alpha1.AgentReviewTaskEvidenceCollection
}

// MapFormalReviewArtifacts maps and cross-validates both the business
// hypothesis output and Argus-owned sensitive task evidence. Publication and
// terminal admission remain platform/application responsibilities.
func MapFormalReviewArtifacts(input FormalMapInput) (FormalReviewArtifacts, error) {
	if err := validateFormalMapInput(input); err != nil {
		return FormalReviewArtifacts{}, err
	}
	report, err := decodePiReviewReport(input.WorkerResult.Report)
	if err != nil {
		return FormalReviewArtifacts{}, err
	}
	if err := validatePiReportPlanBinding(
		input.WorkerRequest.Plan,
		input.ReviewInput,
		input.WorkerResult.CompletedAt,
		report,
		input.Plan.Prompt.Ref.Revision,
		input.Plan.Prompt.Ref.SHA256,
		formalAvailableContextIDs(input.WorkerRequest.ContextArtifacts),
	); err != nil {
		return FormalReviewArtifacts{}, err
	}
	// Diagnostic receipts and exact task evidence are mapped before business
	// candidates. A report may contain an invalid fingerprint, normalization
	// decision, source anchor, or coverage projection while still carrying a
	// valid task-execution account. Retaining that account lets the failed
	// terminal explain the rejection without admitting any Candidate,
	// Hypothesis, Finding, or ReviewRun evidence.
	diagnosticReceipts, err := mapPiDiagnosticReceiptCollection(
		input.WorkerRequest.Plan,
		input.WorkerResult.CompletedAt,
		report,
	)
	if err != nil {
		return FormalReviewArtifacts{}, err
	}
	diagnosticTaskEvidence, err := mapPiTaskEvidenceCollection(
		input.WorkerRequest.Plan,
		diagnosticReceipts,
		report.Execution,
	)
	if err != nil {
		return FormalReviewArtifacts{}, err
	}
	artifacts := FormalReviewArtifacts{
		Receipts: diagnosticReceipts, TaskEvidence: diagnosticTaskEvidence,
	}
	set, receipts, err := mapPiReviewReport(
		input.WorkerRequest.Plan,
		input.ReviewInput,
		input.WorkerResult.CompletedAt,
		input.WorkerResult.Report,
		input.Plan.Prompt.Ref.Revision,
		input.Plan.Prompt.Ref.SHA256,
		formalAvailableContextIDs(input.WorkerRequest.ContextArtifacts),
	)
	if err != nil {
		return artifacts, err
	}
	artifacts.Hypotheses = set
	artifacts.Receipts = receipts
	rawCandidates, err := mapPiRawCandidateCollection(input.WorkerRequest.Plan, set, report)
	if err != nil {
		return artifacts, err
	}
	artifacts.RawCandidates = rawCandidates
	if err := requireFormalReviewedCoverage(set); err != nil {
		if code, retryable := formalNoReviewedGroupRetryCode(receipts); retryable {
			return artifacts, &FormalReviewRetryableError{
				Code: code, Cause: err,
			}
		}
		return artifacts, err
	}
	if set.PlanID != input.Plan.PlanID ||
		set.SourceRunID != input.Plan.ReviewRunID ||
		set.ReviewRunID != input.Plan.ReviewRunID ||
		set.ExecutionID != input.Request.ExecutionID ||
		set.TargetDigest != input.Plan.TargetDigest {
		return FormalReviewArtifacts{}, fmt.Errorf(
			"mapped ReviewHypothesisSet escaped the formal plan, run, execution, or target",
		)
	}
	if len(set.Hypotheses) > input.Plan.Budget.MaxHypotheses {
		return FormalReviewArtifacts{}, fmt.Errorf(
			"mapped hypotheses exceed formal max_hypotheses",
		)
	}
	encoded, err := json.Marshal(set)
	if err != nil {
		return FormalReviewArtifacts{}, fmt.Errorf(
			"marshal mapped formal ReviewHypothesisSet: %w",
			err,
		)
	}
	if int64(len(encoded)) > input.Plan.Budget.MaxOutputBytes {
		return FormalReviewArtifacts{}, fmt.Errorf(
			"mapped ReviewHypothesisSet exceeds formal max_output_bytes",
		)
	}
	return artifacts, nil
}

func formalNoReviewedGroupRetryCode(
	receipts contractsv1alpha1.AgentExecutionReceiptCollection,
) (string, bool) {
	reviewTasks := 0
	sawTimeout := false
	for _, receipt := range receipts.Receipts {
		if receipt.TaskRole != contractsv1alpha1.AgentTaskReview {
			continue
		}
		reviewTasks++
		if receipt.Status == contractsv1alpha1.AgentTaskSucceeded ||
			receipt.FailureReasonCode == nil {
			return "", false
		}
		switch *receipt.FailureReasonCode {
		case "provider_error":
		case "timeout":
			sawTimeout = true
		default:
			return "", false
		}
	}
	if reviewTasks == 0 {
		return "", false
	}
	if sawTimeout {
		return "deadline_exceeded", true
	}
	return "provider_error", true
}

func requireFormalReviewedCoverage(set contractsv1alpha1.ReviewHypothesisSet) error {
	if set.Coverage.GroupsTotal > 0 && set.Coverage.GroupsReviewed == 0 {
		return ErrFormalReviewNoCompletedGroups
	}
	return nil
}

func formalAvailableContextIDs(
	artifacts []contractsv1alpha1.AgentReviewWorkerContextArtifact,
) map[string]struct{} {
	available := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		available[artifact.ContextID] = struct{}{}
	}
	return available
}

func validateFormalMapInput(input FormalMapInput) error {
	if err := input.Plan.Validate(); err != nil {
		return fmt.Errorf("validate formal AgentStagePlan: %w", err)
	}
	if err := input.Request.Validate(); err != nil {
		return fmt.Errorf("validate formal StageExecutionRequest: %w", err)
	}
	if err := input.ReviewInput.Validate(); err != nil {
		return fmt.Errorf("validate frozen ReviewInput: %w", err)
	}
	if err := input.WorkerRequest.Validate(); err != nil {
		return fmt.Errorf("validate frozen Pi worker request: %w", err)
	}
	if err := input.WorkerResult.Validate(); err != nil {
		return fmt.Errorf("validate frozen Pi worker result: %w", err)
	}
	if _, err := decodeFormalModelProfile(input.Plan, input.ModelProfile); err != nil {
		return err
	}
	if err := contractsv1alpha1.ValidateAgentReviewWorkerResultBinding(
		input.WorkerRequest,
		input.WorkerResult,
	); err != nil {
		return fmt.Errorf("bind frozen Pi worker result: %w", err)
	}
	if input.WorkerResult.Status != contractsv1alpha1.AgentReviewWorkerSucceeded {
		return fmt.Errorf(
			"formal hypothesis mapping requires a succeeded Pi worker result, got %q",
			input.WorkerResult.Status,
		)
	}
	canonicalPlan, err := json.Marshal(input.Plan)
	if err != nil {
		return fmt.Errorf("marshal formal AgentStagePlan: %w", err)
	}
	if input.Request.Plan.Ref.SHA256 != digestBytesRaw(canonicalPlan) ||
		input.Request.Plan.Ref.SizeBytes != int64(len(canonicalPlan)) {
		return fmt.Errorf("formal StageExecutionRequest does not bind the exact AgentStagePlan bytes")
	}
	if input.Request.ReviewRunID != input.Plan.ReviewRunID ||
		input.Request.Stage != input.Plan.Stage ||
		input.Request.ExecutionSnapshot != input.Plan.ExecutionSnapshot ||
		input.Request.ReviewInput != input.Plan.ReviewInput ||
		input.Request.OutputContract != input.Plan.OutputContract ||
		input.Request.SideEffects != input.Plan.SideEffects ||
		len(input.Request.Upstream) != 0 {
		return fmt.Errorf("formal StageExecutionRequest does not bind the exact AgentStagePlan")
	}
	if err := validateFormalExecutorCapability(input.Plan, input.Request.Capability); err != nil {
		return err
	}
	targetDigest, err := reviewcore.DigestReviewInput(input.ReviewInput)
	if err != nil {
		return fmt.Errorf("digest frozen ReviewInput: %w", err)
	}
	if targetDigest != input.Plan.TargetDigest ||
		targetDigest != input.Request.ReviewInput.Ref.SHA256 {
		return fmt.Errorf("frozen ReviewInput does not bind the formal target digest")
	}
	canonicalInput, err := json.Marshal(input.ReviewInput)
	if err != nil {
		return fmt.Errorf("marshal frozen ReviewInput: %w", err)
	}
	workerInput, err := base64.StdEncoding.DecodeString(input.WorkerRequest.ReviewInputBase64)
	if err != nil || !bytes.Equal(workerInput, canonicalInput) ||
		base64.StdEncoding.EncodeToString(workerInput) != input.WorkerRequest.ReviewInputBase64 {
		return fmt.Errorf("Pi worker request does not embed the exact canonical ReviewInput")
	}
	if input.WorkerRequest.PromptBundle.Ref != input.Plan.Prompt.Ref ||
		input.WorkerRequest.PromptBundle.Artifact != input.Plan.Prompt.Artifact {
		return fmt.Errorf("Pi worker request does not bind the formal prompt component")
	}
	reviewBindings := make([]contractsv1alpha1.AgentStageSkillBinding, 0, len(input.Plan.Skills))
	for _, skill := range input.Plan.Skills {
		if skill.Phase == contractsv1alpha1.AgentStageSkillPhaseReview {
			reviewBindings = append(reviewBindings, skill)
		}
	}
	if len(input.WorkerRequest.ReviewSkills) != len(reviewBindings) {
		return fmt.Errorf("Pi worker request does not bind all formal review skills")
	}
	for index, skill := range input.WorkerRequest.ReviewSkills {
		if skill.Ref != reviewBindings[index].Ref ||
			skill.Artifact != reviewBindings[index].Artifact {
			return fmt.Errorf("Pi worker request review skill does not bind formal component")
		}
	}
	if len(input.WorkerRequest.KnowledgePacks) != len(input.Plan.Knowledge) {
		return fmt.Errorf("Pi worker request does not bind all formal knowledge")
	}
	for index, pack := range input.WorkerRequest.KnowledgePacks {
		if pack.Ref != input.Plan.Knowledge[index].Ref ||
			pack.Artifact != input.Plan.Knowledge[index].Artifact {
			return fmt.Errorf("Pi worker request knowledge does not bind formal component")
		}
	}
	return validateFormalWorkerPlan(input)
}

func validateFormalExecutorCapability(
	plan contractsv1alpha1.AgentStagePlan,
	capability contractsv1alpha1.ExecutorCapabilitySnapshot,
) error {
	wantAuthority := contractsv1alpha1.ExecutionAuthority{
		AllowedTools:       append([]string{}, plan.ToolAuthority.Tools...),
		ModelEgress:        plan.ModelAuthority.ModelEgress,
		ToolNetwork:        plan.ToolAuthority.ToolNetwork,
		WorkspaceReads:     plan.ToolAuthority.WorkspaceReads,
		WorkspaceWrites:    plan.ToolAuthority.WorkspaceWrites,
		RemoteWrites:       plan.ToolAuthority.RemoteWrites,
		MaxDelegationDepth: plan.ToolAuthority.MaxDelegationDepth,
	}
	if capability.RuntimeID != plan.Runtime.Ref.ID ||
		capability.RuntimeRevision != plan.Runtime.Ref.Revision ||
		capability.RuntimeSHA256 != plan.Runtime.Ref.SHA256 ||
		capability.BuildIdentity != plan.BuildIdentity ||
		!slices.Equal(capability.Authority.AllowedTools, wantAuthority.AllowedTools) ||
		capability.Authority.ModelEgress != wantAuthority.ModelEgress ||
		capability.Authority.ToolNetwork != wantAuthority.ToolNetwork ||
		capability.Authority.WorkspaceReads != wantAuthority.WorkspaceReads ||
		capability.Authority.WorkspaceWrites != wantAuthority.WorkspaceWrites ||
		capability.Authority.RemoteWrites != wantAuthority.RemoteWrites ||
		capability.Authority.MaxDelegationDepth != wantAuthority.MaxDelegationDepth {
		return fmt.Errorf("formal executor capability does not equal AgentStagePlan authority")
	}
	return nil
}

func validateFormalWorkerPlan(input FormalMapInput) error {
	formal := input.Plan
	worker := input.WorkerRequest.Plan
	modelProfile, err := decodeFormalModelProfile(formal, input.ModelProfile)
	if err != nil {
		return err
	}
	if err := validateFrozenWorkerBudgetRange(formal.Budget); err != nil {
		return err
	}
	review, grouping, contextSkill, verifier, err := formalPiSkills(formal)
	if err != nil {
		return err
	}
	if err := validateFormalContextArtifactClosure(
		input.ReviewInput,
		input.WorkerRequest.ContextArtifacts,
	); err != nil {
		return err
	}
	if worker.PlanID != formal.PlanID || worker.SourceRunID != formal.ReviewRunID ||
		worker.ReviewRunID != formal.ReviewRunID ||
		worker.ExecutionID != input.Request.ExecutionID ||
		worker.TargetDigest != formal.TargetDigest ||
		worker.ExecutionSnapshotRef != input.Request.ExecutionSnapshot ||
		worker.ReviewInputRef != input.Request.ReviewInput {
		return fmt.Errorf("Pi worker plan does not bind formal plan, run, execution, and inputs")
	}
	wantWorkerDeadline, err := formalWorkerDeadline(
		worker.CreatedAt,
		input.Request.Deadline,
	)
	if err != nil {
		return err
	}
	if input.WorkerRequest.WorkItemID != input.Request.ExecutionID ||
		input.WorkerRequest.Attempt != input.Request.Attempt ||
		input.WorkerRequest.Generation != input.Request.Generation ||
		input.WorkerRequest.FencingToken != input.Request.FencingToken ||
		input.WorkerRequest.IdempotencyKey != input.Request.IdempotencyKey ||
		!input.WorkerRequest.Deadline.Equal(wantWorkerDeadline) {
		return fmt.Errorf("Pi worker request does not preserve the formal execution fence")
	}
	if worker.Implementation.ID != "pi-review-worker" ||
		worker.Implementation.Revision != "v0" ||
		worker.Grouping != grouping.Ref ||
		len(worker.ContextDimensions) != 1 ||
		worker.ContextDimensions[0] != contextSkill.Ref ||
		worker.Verifier != verifier.Ref ||
		!slices.Equal(worker.Knowledge, formalKnowledgeRefs(formal.Knowledge)) ||
		worker.Agent != formal.Agent.Ref ||
		worker.Runtime != formal.Runtime.Ref ||
		worker.Provider != formal.Provider.Ref ||
		worker.Model.Revision != formal.Model.Ref.Revision ||
		worker.Model.SHA256 != formal.Model.Ref.SHA256 ||
		worker.Model.ID != modelProfile.WireModel {
		return fmt.Errorf("Pi worker plan does not implement the exact formal component closure")
	}
	if worker.Agent.ID != "pi-agent" ||
		!formalBuildRevision(worker.Agent, "worker-v0") ||
		worker.Runtime.ID != "node-runtime" ||
		!formalBuildRevision(worker.Runtime, "binary") ||
		worker.Profile.ID != "local-shadow-stdio" || worker.Profile.Revision != "v1" ||
		worker.Grouping.ID != "directory-language" ||
		!formalBuildRevision(worker.Grouping, "v0") ||
		worker.ContextDimensions[0].ID != "code-context" ||
		!formalBuildRevision(worker.ContextDimensions[0], "v0") ||
		worker.Verifier.ID != "independent-verifier" ||
		!formalBuildRevision(worker.Verifier, "v0") ||
		worker.Provider.Revision != "v1" || worker.Model.Revision != "provider" {
		return fmt.Errorf("formal component refs are unsupported by the frozen Pi worker")
	}
	if worker.Implementation.SHA256 != worker.Agent.SHA256 ||
		worker.Implementation.SHA256 != worker.Grouping.SHA256 ||
		worker.Implementation.SHA256 != worker.ContextDimensions[0].SHA256 ||
		worker.Implementation.SHA256 != worker.Verifier.SHA256 {
		return fmt.Errorf("frozen Pi worker-owned components do not bind one implementation")
	}
	if !slices.Equal(worker.ReviewDimensions, review) {
		return fmt.Errorf("Pi worker review dimensions do not equal formal review skills")
	}
	if !slices.Equal(formal.ToolAuthority.Tools, frozenWorkerTools) ||
		!slices.Equal(worker.ToolPolicy.AllowedTools, frozenWorkerTools) ||
		worker.ToolPolicy.ToolNetwork != formal.ToolAuthority.ToolNetwork ||
		worker.ToolPolicy.WorkspaceWrites != formal.ToolAuthority.WorkspaceWrites ||
		worker.ToolPolicy.RemoteWrites != formal.ToolAuthority.RemoteWrites {
		return fmt.Errorf("Pi worker tool policy does not equal formal read-only authority")
	}
	if worker.Budget != formalWorkerBudget(formal.Budget) ||
		input.WorkerRequest.Capability.MaxInputBytes != uint64(formal.Budget.MaxTargetBytes) ||
		input.WorkerRequest.Capability.MaxOutputBytes > uint64(formal.Budget.MaxOutputBytes) {
		return fmt.Errorf("Pi worker budgets do not equal the enforceable formal budget")
	}
	if !worker.VerificationRequired || worker.SideEffects != formal.SideEffects {
		return fmt.Errorf("Pi worker verification or side-effect policy differs from formal plan")
	}
	return nil
}

// formalWorkerDeadline reserves a deterministic slice of the immutable stage
// window for worker-result decoding, callback authentication, and durable
// terminal admission. It remains part of the exact formal lowering contract:
// retries derive identical bytes, while arbitrary early or late worker
// deadlines are rejected by the mapper.
func formalWorkerDeadline(
	createdAt time.Time,
	stageDeadline time.Time,
) (time.Time, error) {
	window := stageDeadline.Sub(createdAt)
	if window <= 0 {
		return time.Time{}, fmt.Errorf(
			"formal Pi stage deadline must follow plan creation",
		)
	}
	grace := window / 10
	if grace > maxFormalWorkerCompletionGrace {
		grace = maxFormalWorkerCompletionGrace
	}
	deadline := stageDeadline.Add(-grace)
	if !deadline.After(createdAt) || !deadline.Before(stageDeadline) {
		return time.Time{}, fmt.Errorf(
			"formal Pi worker completion reserve exhausted execution window",
		)
	}
	return deadline, nil
}

func formalBuildRevision(ref contractsv1alpha1.VersionedRef, base string) bool {
	return ref.Revision == base ||
		ref.Revision == base+"-"+ref.SHA256[:16]
}

func validateFormalContextArtifactClosure(
	reviewInput reviewcore.ReviewInput,
	artifacts []contractsv1alpha1.AgentReviewWorkerContextArtifact,
) error {
	index := 0
	for _, binding := range reviewInput.Contexts {
		if binding.Ref == nil {
			continue
		}
		if index >= len(artifacts) {
			return fmt.Errorf("Pi worker omitted a frozen context artifact")
		}
		ref := binding.Ref
		artifact := artifacts[index]
		if artifact.ContextID != ref.ContextID ||
			artifact.Kind != ref.Kind ||
			artifact.Revision != ref.Revision ||
			artifact.Artifact.Ref.URI != ref.ArtifactURI ||
			artifact.Artifact.Ref.SHA256 != ref.Digest ||
			artifact.Artifact.Ref.SizeBytes != ref.SizeBytes ||
			artifact.Artifact.Contract != ref.Contract {
			return fmt.Errorf(
				"Pi worker context artifact %d does not bind frozen ReviewInput context %q",
				index,
				ref.ContextID,
			)
		}
		index++
	}
	if index != len(artifacts) {
		return fmt.Errorf("Pi worker added context artifacts outside frozen ReviewInput")
	}
	return nil
}

func formalKnowledgeRefs(
	bindings []contractsv1alpha1.AgentStageKnowledgeBinding,
) []contractsv1alpha1.VersionedRef {
	refs := make([]contractsv1alpha1.VersionedRef, len(bindings))
	for index, binding := range bindings {
		refs[index] = binding.Ref
	}
	return refs
}

func validateFrozenWorkerBudgetRange(budget contractsv1alpha1.AgentStageBudget) error {
	// These are the public stdio admission limits enforced by
	// AgentReviewPlan.Validate and runtime/pi-review. Check before narrowing the
	// wider formal integer types so an oversized formal plan cannot wrap into an
	// apparently valid worker budget.
	if budget.MaxFiles > 1_000 || budget.MaxGroups > 256 ||
		budget.MaxHypotheses > 1_000 || budget.MaxModelCalls > 2_000 ||
		budget.MaxToolCalls > 100 || budget.MaxOutputTokens > 32_768 ||
		budget.MaxGroupBytes > 1<<20 || budget.MaxTargetBytes > 64<<20 ||
		budget.TimeoutMS > 3_600_000 || budget.MaxConcurrency > 16 {
		return fmt.Errorf("formal budget exceeds frozen Pi worker admission limits")
	}
	return nil
}

func formalPiSkills(
	plan contractsv1alpha1.AgentStagePlan,
) (
	[]contractsv1alpha1.VersionedRef,
	contractsv1alpha1.AgentStageSkillBinding,
	contractsv1alpha1.AgentStageSkillBinding,
	contractsv1alpha1.AgentStageSkillBinding,
	error,
) {
	review := make([]contractsv1alpha1.VersionedRef, 0, len(plan.Skills))
	var grouping, contextSkill, verifier contractsv1alpha1.AgentStageSkillBinding
	for _, skill := range plan.Skills {
		switch skill.Phase {
		case contractsv1alpha1.AgentStageSkillPhaseGrouping:
			if grouping.ID != "" {
				return nil, grouping, contextSkill, verifier,
					fmt.Errorf("frozen Pi formal execution requires exactly one grouping skill")
			}
			grouping = skill
		case contractsv1alpha1.AgentStageSkillPhaseContext:
			if contextSkill.ID != "" {
				return nil, grouping, contextSkill, verifier,
					fmt.Errorf("frozen Pi formal execution requires exactly one context skill")
			}
			contextSkill = skill
		case contractsv1alpha1.AgentStageSkillPhaseReview:
			review = append(review, skill.Ref)
		case contractsv1alpha1.AgentStageSkillPhaseVerification:
			if verifier.ID != "" {
				return nil, grouping, contextSkill, verifier,
					fmt.Errorf("frozen Pi formal execution requires exactly one verification skill")
			}
			verifier = skill
		}
	}
	if grouping.ID == "" || contextSkill.ID == "" || verifier.ID == "" ||
		len(review) == 0 {
		return nil, grouping, contextSkill, verifier, fmt.Errorf(
			"frozen Pi formal execution requires grouping, context, review, and verification skills",
		)
	}
	return review, grouping, contextSkill, verifier, nil
}

func formalWorkerBudget(
	budget contractsv1alpha1.AgentStageBudget,
) contractsv1alpha1.AgentReviewBudget {
	return contractsv1alpha1.AgentReviewBudget{
		MaxFiles:        uint32(budget.MaxFiles),
		MaxGroups:       uint32(budget.MaxGroups),
		MaxCandidates:   uint32(budget.MaxHypotheses),
		MaxModelCalls:   uint32(budget.MaxModelCalls),
		MaxToolCalls:    uint32(budget.MaxToolCalls),
		MaxOutputTokens: uint32(budget.MaxOutputTokens),
		MaxGroupBytes:   uint64(budget.MaxGroupBytes),
		MaxTargetBytes:  uint64(budget.MaxTargetBytes),
		TimeoutMS:       uint64(budget.TimeoutMS),
		MaxConcurrency:  uint32(budget.MaxConcurrency),
	}
}
