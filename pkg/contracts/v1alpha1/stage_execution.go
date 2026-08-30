package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

const (
	StageExecutionRequestSchemaVersion = "argus.stage_execution_request.v1alpha1"
	StageExecutionResultSchemaVersion  = "argus.stage_execution_result.v1alpha1"

	StageExecutionSnapshotContract              = AgentStagePlanExecutionSnapshotContract
	StageExecutionReviewInputContract           = AgentStagePlanReviewInputContract
	StageExecutionTraceManifestContract         = "hailix.trace_manifest.v1alpha1"
	StageExecutionAgentRawCandidateContract     = AgentReviewRawCandidateCollectionSchemaVersion
	StageExecutionAgentTaskEvidenceContract     = AgentReviewTaskEvidenceCollectionSchemaVersion
	StageExecutionAgentExecutionReceiptContract = AgentExecutionReceiptCollectionSchemaVersion
	ExecutorTrustAuthorityLocalHost             = "local_host"
	ExecutorTrustAuthorityPlatform              = "platform"
	stageExecutionMaxJSONInteger                = uint64(1<<53 - 1)
	stageExecutionMaxCompletenessNotes          = 32
)

type ArtifactBinding struct {
	Ref      ContentRef `json:"ref"`
	Contract string     `json:"contract"`
}

type ExecutionAuthority struct {
	AllowedTools       []string `json:"allowed_tools"`
	ModelEgress        string   `json:"model_egress"`
	ToolNetwork        string   `json:"tool_network"`
	WorkspaceReads     string   `json:"workspace_reads"`
	WorkspaceWrites    string   `json:"workspace_writes"`
	RemoteWrites       string   `json:"remote_writes"`
	MaxDelegationDepth uint32   `json:"max_delegation_depth"`
}

type ExecutorCapabilitySnapshot struct {
	RuntimeKind     string             `json:"runtime_kind"`
	RuntimeID       string             `json:"runtime_id"`
	RuntimeRevision string             `json:"runtime_revision"`
	RuntimeSHA256   string             `json:"runtime_sha256"`
	BuildIdentity   string             `json:"build_identity"`
	Authority       ExecutionAuthority `json:"authority"`
	Trust           ExecutorTrust      `json:"trust"`
	SHA256          string             `json:"sha256"`
}

// ExecutorTrust freezes the capability and terminal-callback trust domain
// into the capability digest. It does not by itself prove an attestation: a
// platform adapter must still verify the corresponding receipts. Binding both
// verifier refs prevents an unknown-outcome recovery from silently switching
// to a different execution trust domain while reusing the same request.
type ExecutorTrust struct {
	Authority          string       `json:"authority"`
	CapabilityVerifier VersionedRef `json:"capability_verifier"`
	CallbackVerifier   VersionedRef `json:"callback_verifier"`
}

// Validate rejects incomplete or unsupported execution trust domains. The
// trust value is embedded in the immutable capability and request identities;
// callback admission uses this method again before comparing an actual
// verifier receipt with the frozen request.
func (trust ExecutorTrust) Validate() error {
	return trust.validate()
}

type StageExecutionRequest struct {
	SchemaVersion     string                     `json:"schema_version"`
	RequestSHA256     string                     `json:"request_sha256"`
	ExecutionID       string                     `json:"execution_id"`
	ReviewRunID       string                     `json:"review_run_id"`
	TenantID          string                     `json:"tenant_id"`
	WorkspaceID       string                     `json:"workspace_id"`
	WorkloadID        string                     `json:"workload_id"`
	LeaseID           string                     `json:"lease_id"`
	LeaseWorkerID     string                     `json:"lease_worker_id"`
	Stage             VersionedRef               `json:"stage"`
	Attempt           int                        `json:"attempt"`
	Generation        int                        `json:"generation"`
	FencingToken      uint64                     `json:"fencing_token"`
	IdempotencyKey    string                     `json:"idempotency_key"`
	Plan              ArtifactBinding            `json:"plan"`
	ExecutionSnapshot ArtifactBinding            `json:"execution_snapshot"`
	ReviewInput       ArtifactBinding            `json:"review_input"`
	Upstream          []ArtifactBinding          `json:"upstream"`
	OutputContract    string                     `json:"output_contract"`
	Capability        ExecutorCapabilitySnapshot `json:"capability"`
	Deadline          time.Time                  `json:"deadline"`
	SideEffects       string                     `json:"side_effects"`
}

type StageExecutionStatus string

const (
	StageExecutionSucceeded StageExecutionStatus = "succeeded"
	StageExecutionFailed    StageExecutionStatus = "failed"
	StageExecutionCanceled  StageExecutionStatus = "canceled"
)

type StageExecutionFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type StageExecutionResult struct {
	SchemaVersion          string                 `json:"schema_version"`
	RequestSHA256          string                 `json:"request_sha256"`
	ExecutionID            string                 `json:"execution_id"`
	Attempt                int                    `json:"attempt"`
	Generation             int                    `json:"generation"`
	FencingToken           uint64                 `json:"fencing_token"`
	IdempotencyKey         string                 `json:"idempotency_key"`
	Status                 StageExecutionStatus   `json:"status"`
	Output                 *ArtifactBinding       `json:"output,omitempty"`
	TraceManifest          *ArtifactBinding       `json:"trace_manifest,omitempty"`
	AgentRawCandidates     *ArtifactBinding       `json:"agent_raw_candidates,omitempty"`
	AgentTaskEvidence      *ArtifactBinding       `json:"agent_task_evidence,omitempty"`
	AgentExecutionReceipts *ArtifactBinding       `json:"agent_execution_receipts,omitempty"`
	Failure                *StageExecutionFailure `json:"failure,omitempty"`
	Completeness           string                 `json:"completeness"`
	CompletenessNotes      []string               `json:"completeness_notes"`
	CapabilitySHA256       string                 `json:"capability_sha256"`
	RecordedAt             time.Time              `json:"recorded_at"`
}

// SealStageExecutionRequest derives the exact request identity without
// canonicalizing any caller-owned ordered fields. In particular, Upstream is
// hashed in the order supplied by the caller.
func SealStageExecutionRequest(request StageExecutionRequest) (StageExecutionRequest, error) {
	request = cloneStageExecutionRequest(request)
	request.RequestSHA256 = ""
	if err := request.validateContent(); err != nil {
		return StageExecutionRequest{}, err
	}
	digest, err := digestStageExecutionRequest(request)
	if err != nil {
		return StageExecutionRequest{}, err
	}
	request.RequestSHA256 = digest
	if err := request.Validate(); err != nil {
		return StageExecutionRequest{}, fmt.Errorf("validate sealed StageExecutionRequest: %w", err)
	}
	return request, nil
}

func DecodeStageExecutionRequest(data []byte) (StageExecutionRequest, error) {
	var request StageExecutionRequest
	if err := decodeStageExecutionJSON(data, &request); err != nil {
		return StageExecutionRequest{}, fmt.Errorf("decode StageExecutionRequest: %w", err)
	}
	if err := request.Validate(); err != nil {
		return StageExecutionRequest{}, err
	}
	return request, nil
}

func DecodeStageExecutionResult(data []byte) (StageExecutionResult, error) {
	var result StageExecutionResult
	if err := decodeStageExecutionJSON(data, &result); err != nil {
		return StageExecutionResult{}, fmt.Errorf("decode StageExecutionResult: %w", err)
	}
	if err := result.Validate(); err != nil {
		return StageExecutionResult{}, err
	}
	return result, nil
}

func ValidateStageExecutionResultBinding(
	request StageExecutionRequest,
	result StageExecutionResult,
) error {
	if err := request.Validate(); err != nil {
		return fmt.Errorf("validate execution request: %w", err)
	}
	if err := result.Validate(); err != nil {
		return fmt.Errorf("validate execution result: %w", err)
	}
	if result.RequestSHA256 != request.RequestSHA256 {
		return fmt.Errorf("execution result request_sha256 does not match exact request")
	}
	if result.ExecutionID != request.ExecutionID ||
		result.Attempt != request.Attempt ||
		result.Generation != request.Generation ||
		result.FencingToken != request.FencingToken ||
		result.IdempotencyKey != request.IdempotencyKey ||
		result.CapabilitySHA256 != request.Capability.SHA256 {
		return fmt.Errorf("execution result does not match exact request fencing and capability")
	}
	if result.Output != nil && result.Output.Contract != request.OutputContract {
		return fmt.Errorf("execution result output contract does not match request")
	}
	return nil
}

func decodeStageExecutionJSON(data []byte, target any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return rejectTrailingJSON(decoder)
}

func (request StageExecutionRequest) Validate() error {
	if err := request.validateContent(); err != nil {
		return err
	}
	if err := requireSHA256("request_sha256", request.RequestSHA256); err != nil {
		return err
	}
	digest, err := digestStageExecutionRequest(request)
	if err != nil {
		return err
	}
	if request.RequestSHA256 != digest {
		return fmt.Errorf("request_sha256 does not match StageExecutionRequest content")
	}
	return nil
}

func (request StageExecutionRequest) validateContent() error {
	if request.SchemaVersion != StageExecutionRequestSchemaVersion {
		return fmt.Errorf("unsupported StageExecutionRequest schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"execution_id": request.ExecutionID, "review_run_id": request.ReviewRunID,
		"tenant_id": request.TenantID, "workspace_id": request.WorkspaceID,
		"workload_id": request.WorkloadID, "lease_id": request.LeaseID,
		"lease_worker_id": request.LeaseWorkerID,
		"idempotency_key": request.IdempotencyKey,
		"output_contract": request.OutputContract,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := request.Stage.validate("stage"); err != nil {
		return err
	}
	if request.Attempt < 1 || request.Generation < 1 || request.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if uint64(request.Attempt) > stageExecutionMaxJSONInteger ||
		uint64(request.Generation) > stageExecutionMaxJSONInteger ||
		request.FencingToken > stageExecutionMaxJSONInteger {
		return fmt.Errorf(
			"attempt, generation, and fencing_token must be JSON safe integers",
		)
	}
	if err := validateStageExecutionArtifactBinding(request.Plan, "plan", true); err != nil {
		return err
	}
	if request.Plan.Contract != AgentStagePlanSchemaVersion {
		return fmt.Errorf("plan.contract must be %q", AgentStagePlanSchemaVersion)
	}
	if err := validateStageExecutionArtifactBinding(
		request.ExecutionSnapshot,
		"execution_snapshot",
		true,
	); err != nil {
		return err
	}
	if request.ExecutionSnapshot.Contract != StageExecutionSnapshotContract {
		return fmt.Errorf(
			"execution_snapshot.contract must be %q",
			StageExecutionSnapshotContract,
		)
	}
	if err := validateStageExecutionArtifactBinding(
		request.ReviewInput,
		"review_input",
		true,
	); err != nil {
		return err
	}
	if request.ReviewInput.Contract != StageExecutionReviewInputContract {
		return fmt.Errorf(
			"review_input.contract must be %q",
			StageExecutionReviewInputContract,
		)
	}
	if request.Upstream == nil {
		return fmt.Errorf("upstream must be an explicit array")
	}
	for index, binding := range request.Upstream {
		if err := validateStageExecutionArtifactBinding(
			binding,
			fmt.Sprintf("upstream[%d]", index),
			true,
		); err != nil {
			return err
		}
	}
	if err := request.Capability.Validate(); err != nil {
		return err
	}
	if request.Deadline.IsZero() || request.Deadline.Location() != time.UTC {
		return fmt.Errorf("deadline must be a non-zero UTC timestamp")
	}
	if request.SideEffects != AgentStageSideEffectsDeny {
		return fmt.Errorf("stage side effects must be denied")
	}
	return nil
}

func (result StageExecutionResult) Validate() error {
	if result.SchemaVersion != StageExecutionResultSchemaVersion {
		return fmt.Errorf("unsupported StageExecutionResult schema %q", result.SchemaVersion)
	}
	for name, value := range map[string]string{
		"execution_id": result.ExecutionID, "idempotency_key": result.IdempotencyKey,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if result.Attempt < 1 || result.Generation < 1 || result.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if uint64(result.Attempt) > stageExecutionMaxJSONInteger ||
		uint64(result.Generation) > stageExecutionMaxJSONInteger ||
		result.FencingToken > stageExecutionMaxJSONInteger {
		return fmt.Errorf(
			"attempt, generation, and fencing_token must be JSON safe integers",
		)
	}
	if err := requireSHA256("request_sha256", result.RequestSHA256); err != nil {
		return err
	}
	if err := requireSHA256("capability_sha256", result.CapabilitySHA256); err != nil {
		return err
	}
	if result.CompletenessNotes == nil {
		return fmt.Errorf("completeness_notes must be an explicit array")
	}
	if len(result.CompletenessNotes) > stageExecutionMaxCompletenessNotes {
		return fmt.Errorf(
			"completeness_notes must contain at most %d entries",
			stageExecutionMaxCompletenessNotes,
		)
	}
	for index, note := range result.CompletenessNotes {
		if err := requireIdentifier(
			fmt.Sprintf("completeness_notes[%d]", index),
			note,
		); err != nil {
			return err
		}
	}
	switch result.Completeness {
	case "complete":
		if len(result.CompletenessNotes) != 0 {
			return fmt.Errorf("complete result must not have completeness notes")
		}
	case "partial":
		if len(result.CompletenessNotes) == 0 {
			return fmt.Errorf("partial result must explain its completeness")
		}
	default:
		return fmt.Errorf("unsupported result completeness %q", result.Completeness)
	}
	if result.RecordedAt.IsZero() || result.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("recorded_at must be a non-zero UTC timestamp")
	}
	switch result.Status {
	case StageExecutionSucceeded:
		if result.Output == nil || result.Failure != nil {
			return fmt.Errorf("succeeded result requires output and forbids failure")
		}
	case StageExecutionFailed, StageExecutionCanceled:
		if result.Output != nil || result.AgentRawCandidates != nil || result.Failure == nil {
			return fmt.Errorf("failed/canceled result requires failure and forbids output/raw candidate evidence")
		}
		if (result.AgentTaskEvidence == nil) != (result.AgentExecutionReceipts == nil) {
			return fmt.Errorf("failed/canceled diagnostic task evidence and receipts must be present together")
		}
		if err := result.Failure.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported stage execution status %q", result.Status)
	}
	if result.Output != nil {
		if err := validateStageExecutionArtifactBinding(
			*result.Output,
			"output",
			true,
		); err != nil {
			return err
		}
	}
	if result.TraceManifest != nil {
		if err := validateStageExecutionArtifactBinding(
			*result.TraceManifest,
			"trace_manifest",
			true,
		); err != nil {
			return err
		}
		if result.TraceManifest.Contract != StageExecutionTraceManifestContract {
			return fmt.Errorf(
				"trace_manifest.contract must be %q",
				StageExecutionTraceManifestContract,
			)
		}
	}
	if result.AgentRawCandidates != nil {
		if err := validateStageExecutionArtifactBinding(
			*result.AgentRawCandidates,
			"agent_raw_candidates",
			true,
		); err != nil {
			return err
		}
		if result.AgentRawCandidates.Contract != StageExecutionAgentRawCandidateContract {
			return fmt.Errorf(
				"agent_raw_candidates.contract must be %q",
				StageExecutionAgentRawCandidateContract,
			)
		}
	}
	if result.AgentTaskEvidence != nil {
		if err := validateStageExecutionArtifactBinding(
			*result.AgentTaskEvidence,
			"agent_task_evidence",
			true,
		); err != nil {
			return err
		}
		if result.AgentTaskEvidence.Contract != StageExecutionAgentTaskEvidenceContract {
			return fmt.Errorf(
				"agent_task_evidence.contract must be %q",
				StageExecutionAgentTaskEvidenceContract,
			)
		}
	}
	if result.AgentExecutionReceipts != nil {
		if err := validateStageExecutionArtifactBinding(
			*result.AgentExecutionReceipts,
			"agent_execution_receipts",
			true,
		); err != nil {
			return err
		}
		if result.AgentExecutionReceipts.Contract != StageExecutionAgentExecutionReceiptContract {
			return fmt.Errorf(
				"agent_execution_receipts.contract must be %q",
				StageExecutionAgentExecutionReceiptContract,
			)
		}
	}
	return nil
}

func validateStageExecutionArtifactBinding(
	binding ArtifactBinding,
	name string,
	nonEmpty bool,
) error {
	if err := binding.validate(name, nonEmpty); err != nil {
		return err
	}
	if uint64(binding.Ref.SizeBytes) > stageExecutionMaxJSONInteger {
		return fmt.Errorf("%s.ref.size_bytes must be a JSON safe integer", name)
	}
	return nil
}

// cloneStageExecutionRequest detaches the only mutable aggregate values in the
// transport contract. Keeping explicit empty slices non-nil preserves the
// contract distinction between [] and null.
func cloneStageExecutionRequest(request StageExecutionRequest) StageExecutionRequest {
	request.Upstream = cloneStageExecutionSlice(request.Upstream)
	request.Capability.Authority.AllowedTools = cloneStageExecutionSlice(
		request.Capability.Authority.AllowedTools,
	)
	return request
}

func cloneStageExecutionSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	return append(make([]T, 0, len(values)), values...)
}

func digestStageExecutionRequest(request StageExecutionRequest) (string, error) {
	request.RequestSHA256 = ""
	data, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("marshal StageExecutionRequest: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (binding ArtifactBinding) validate(name string, nonEmpty bool) error {
	if err := requireIdentifier(name+".contract", binding.Contract); err != nil {
		return err
	}
	return binding.Ref.validate(name+".ref", nonEmpty)
}

func (capability ExecutorCapabilitySnapshot) Validate() error {
	for name, value := range map[string]string{
		"capability.runtime_kind":     capability.RuntimeKind,
		"capability.runtime_id":       capability.RuntimeID,
		"capability.runtime_revision": capability.RuntimeRevision,
		"capability.build_identity":   capability.BuildIdentity,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := capability.Authority.validate(); err != nil {
		return err
	}
	if err := capability.Trust.validate(); err != nil {
		return err
	}
	if err := requireSHA256("capability.runtime_sha256", capability.RuntimeSHA256); err != nil {
		return err
	}
	if err := requireSHA256("capability.sha256", capability.SHA256); err != nil {
		return err
	}
	digest, err := DigestExecutorCapability(capability)
	if err != nil {
		return err
	}
	if capability.SHA256 != digest {
		return fmt.Errorf("capability sha256 does not match its content")
	}
	return nil
}

func (trust ExecutorTrust) validate() error {
	switch trust.Authority {
	case ExecutorTrustAuthorityLocalHost, ExecutorTrustAuthorityPlatform:
	default:
		return fmt.Errorf("unsupported capability trust authority %q", trust.Authority)
	}
	if err := validateAgentStageExactVersionedRef(
		trust.CapabilityVerifier,
		"capability.trust.capability_verifier",
	); err != nil {
		return err
	}
	if err := validateAgentStageExactVersionedRef(
		trust.CallbackVerifier,
		"capability.trust.callback_verifier",
	); err != nil {
		return err
	}
	return nil
}

func DigestExecutorCapability(
	capability ExecutorCapabilitySnapshot,
) (string, error) {
	copy := capability
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal executor capability: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (authority ExecutionAuthority) validate() error {
	if authority.AllowedTools == nil || !slices.IsSorted(authority.AllowedTools) {
		return fmt.Errorf("capability authority allowed_tools must be an explicit sorted array")
	}
	for index, tool := range authority.AllowedTools {
		if err := requireIdentifier(
			fmt.Sprintf("capability.authority.allowed_tools[%d]", index),
			tool,
		); err != nil {
			return err
		}
		if index > 0 && tool == authority.AllowedTools[index-1] {
			return fmt.Errorf("capability authority contains duplicate tool %q", tool)
		}
	}
	if authority.ModelEgress != AgentStageModelEgressProviderBrokerOnly {
		return fmt.Errorf(
			"capability authority model_egress must be %q",
			AgentStageModelEgressProviderBrokerOnly,
		)
	}
	if authority.ToolNetwork != AgentStageSideEffectsDeny {
		return fmt.Errorf("capability authority tool_network must be denied")
	}
	if authority.WorkspaceReads != AgentStageWorkspaceReadFrozenInputOnly {
		return fmt.Errorf(
			"capability authority workspace_reads must be %q",
			AgentStageWorkspaceReadFrozenInputOnly,
		)
	}
	if authority.WorkspaceWrites != AgentStageSideEffectsDeny ||
		authority.RemoteWrites != AgentStageSideEffectsDeny {
		return fmt.Errorf("capability authority workspace and remote writes must be denied")
	}
	if authority.MaxDelegationDepth != 0 {
		return fmt.Errorf("capability authority max_delegation_depth must be zero")
	}
	return nil
}

func (failure StageExecutionFailure) validate() error {
	if err := requireIdentifier("failure.code", failure.Code); err != nil {
		return err
	}
	if failure.Message == "" || len(failure.Message) > 4096 {
		return fmt.Errorf("failure.message must be non-empty and bounded")
	}
	return nil
}
