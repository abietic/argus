package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	AgentReviewWorkerRequestSchemaVersion         = "argus.agent_review_worker_request.v1alpha1"
	AgentReviewWorkerResultSchemaVersion          = "argus.agent_review_worker_result.v1alpha1"
	AgentReviewWorkerProtocolStdio                = "argus.agent_review_worker_stdio.v1alpha1"
	AgentReviewWorkerFrozenInputBase64            = "review_input_inline_base64"
	agentReviewWorkerMaxJSONInteger               = uint64(1<<53 - 1)
	AgentReviewWorkerMaxGroupCheckpoints          = 256
	AgentReviewWorkerMaxGroupCheckpointBytes      = 16 << 20
	AgentReviewWorkerMaxGroupCheckpointTotalBytes = 64 << 20
	AgentReviewWorkerMaxRulePackBytes             = 256 << 10
)

// AgentReviewWorkerCapability freezes the narrow transport capability the host
// selected for one worker request. It intentionally excludes credentials,
// provider secrets, platform attestation, and remote-write authority.
type AgentReviewWorkerCapability struct {
	Protocol       string `json:"protocol"`
	FrozenInput    string `json:"frozen_input"`
	MaxInputBytes  uint64 `json:"max_input_bytes"`
	MaxOutputBytes uint64 `json:"max_output_bytes"`
	SHA256         string `json:"sha256"`
}

// AgentReviewWorkerRequest is a fenced, bounded host-to-worker envelope. The
// embedded plan remains non-attested shadow intent; fencing only prevents a
// stale local attempt from being confused with the accepted attempt.
type AgentReviewWorkerRequest struct {
	SchemaVersion         string                             `json:"schema_version"`
	WorkItemID            string                             `json:"work_item_id"`
	Attempt               int                                `json:"attempt"`
	Generation            int                                `json:"generation"`
	FencingToken          uint64                             `json:"fencing_token"`
	IdempotencyKey        string                             `json:"idempotency_key"`
	Capability            AgentReviewWorkerCapability        `json:"capability"`
	Deadline              time.Time                          `json:"deadline"`
	Plan                  AgentReviewPlan                    `json:"plan"`
	RulePackBase64        string                             `json:"rule_pack_base64"`
	PromptBundle          AgentReviewWorkerPromptBundle      `json:"prompt_bundle"`
	ReviewSkills          []AgentReviewWorkerSkill           `json:"review_skills"`
	KnowledgePacks        []AgentReviewWorkerKnowledge       `json:"knowledge_packs"`
	ContextArtifacts      []AgentReviewWorkerContextArtifact `json:"context_artifacts"`
	CheckpointScopeSHA256 string                             `json:"checkpoint_scope_sha256"`
	GroupCheckpoints      []AgentReviewWorkerGroupCheckpoint `json:"group_checkpoints"`
	ReviewInputBase64     string                             `json:"review_input_base64"`
}

// AgentReviewWorkerGroupCheckpoint transports one prior-generation Pi
// context/review group result. Its content remains worker self-report; the
// resumed worker revalidates the frozen grouping and the final full report
// still passes the normal host mapper.
type AgentReviewWorkerGroupCheckpoint struct {
	GroupID               string `json:"group_id"`
	CheckpointRevision    uint64 `json:"checkpoint_revision"`
	CheckpointScopeSHA256 string `json:"checkpoint_scope_sha256"`
	ContentSHA256         string `json:"content_sha256"`
	SizeBytes             uint64 `json:"size_bytes"`
	ContentBase64         string `json:"content_base64"`
}

type AgentReviewWorkerStatus string

const (
	AgentReviewWorkerSucceeded AgentReviewWorkerStatus = "succeeded"
	AgentReviewWorkerFailed    AgentReviewWorkerStatus = "failed"
	AgentReviewWorkerCanceled  AgentReviewWorkerStatus = "canceled"
)

type AgentReviewWorkerFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// AgentReviewWorkerResult is worker self-report, not platform attestation. A
// successful report is carried byte-for-byte so report_sha256 can bind the
// exact JSON received by the host before any semantic import.
type AgentReviewWorkerResult struct {
	SchemaVersion    string                    `json:"schema_version"`
	WorkItemID       string                    `json:"work_item_id"`
	Attempt          int                       `json:"attempt"`
	Generation       int                       `json:"generation"`
	FencingToken     uint64                    `json:"fencing_token"`
	IdempotencyKey   string                    `json:"idempotency_key"`
	CapabilitySHA256 string                    `json:"capability_sha256"`
	Status           AgentReviewWorkerStatus   `json:"status"`
	ReportSHA256     *string                   `json:"report_sha256,omitempty"`
	Report           json.RawMessage           `json:"report,omitempty"`
	Failure          *AgentReviewWorkerFailure `json:"failure,omitempty"`
	CompletedAt      time.Time                 `json:"completed_at"`
}

func DecodeAgentReviewWorkerRequest(data []byte) (AgentReviewWorkerRequest, error) {
	var request AgentReviewWorkerRequest
	if err := decodeAgentReviewShadowJSON(data, &request); err != nil {
		return AgentReviewWorkerRequest{}, fmt.Errorf("decode AgentReviewWorkerRequest: %w", err)
	}
	if err := request.Validate(); err != nil {
		return AgentReviewWorkerRequest{}, err
	}
	return request, nil
}

func DecodeAgentReviewWorkerResult(data []byte) (AgentReviewWorkerResult, error) {
	var result AgentReviewWorkerResult
	if err := decodeAgentReviewShadowJSON(data, &result); err != nil {
		return AgentReviewWorkerResult{}, fmt.Errorf("decode AgentReviewWorkerResult: %w", err)
	}
	if err := result.Validate(); err != nil {
		return AgentReviewWorkerResult{}, err
	}
	return result, nil
}

func (capability AgentReviewWorkerCapability) Validate() error {
	if capability.Protocol != AgentReviewWorkerProtocolStdio {
		return fmt.Errorf("unsupported worker capability protocol %q", capability.Protocol)
	}
	if capability.FrozenInput != AgentReviewWorkerFrozenInputBase64 {
		return fmt.Errorf("unsupported worker frozen_input %q", capability.FrozenInput)
	}
	if capability.MaxInputBytes == 0 || capability.MaxOutputBytes == 0 {
		return fmt.Errorf("worker capability IO limits must be positive")
	}
	if capability.MaxInputBytes > agentReviewWorkerMaxJSONInteger ||
		capability.MaxOutputBytes > agentReviewWorkerMaxJSONInteger {
		return fmt.Errorf("worker capability IO limits must be JSON safe integers")
	}
	if err := requireSHA256("capability.sha256", capability.SHA256); err != nil {
		return err
	}
	digest, err := DigestAgentReviewWorkerCapability(capability)
	if err != nil {
		return err
	}
	if digest != capability.SHA256 {
		return fmt.Errorf("capability sha256 does not match its content")
	}
	return nil
}

func DigestAgentReviewWorkerCapability(
	capability AgentReviewWorkerCapability,
) (string, error) {
	// A string-keyed map makes encoding/json emit keys in lexical order. This
	// is the small, closed cross-language canonical form used by the stdio
	// worker; no floating-point or recursively nested values are present.
	canonical := map[string]any{
		"frozen_input":     capability.FrozenInput,
		"max_input_bytes":  capability.MaxInputBytes,
		"max_output_bytes": capability.MaxOutputBytes,
		"protocol":         capability.Protocol,
		"sha256":           "",
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal agent review worker capability: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (request AgentReviewWorkerRequest) Validate() error {
	if request.SchemaVersion != AgentReviewWorkerRequestSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentReviewWorkerRequest schema %q",
			request.SchemaVersion,
		)
	}
	for name, value := range map[string]string{
		"work_item_id": request.WorkItemID, "idempotency_key": request.IdempotencyKey,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if request.Attempt < 1 || request.Generation < 1 || request.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if uint64(request.Attempt) > agentReviewWorkerMaxJSONInteger ||
		uint64(request.Generation) > agentReviewWorkerMaxJSONInteger ||
		request.FencingToken > agentReviewWorkerMaxJSONInteger {
		return fmt.Errorf("attempt, generation, and fencing_token must be JSON safe integers")
	}
	if err := request.Capability.Validate(); err != nil {
		return err
	}
	if err := validateAgentReviewTime("deadline", request.Deadline); err != nil {
		return err
	}
	if err := request.Plan.Validate(); err != nil {
		return fmt.Errorf("validate worker plan: %w", err)
	}
	if _, err := DecodeAgentReviewWorkerRulePack(request); err != nil {
		return err
	}
	if request.WorkItemID != request.Plan.ExecutionID {
		return fmt.Errorf("work_item_id must match plan.execution_id")
	}
	if !request.Deadline.After(request.Plan.CreatedAt) {
		return fmt.Errorf("deadline must be after plan.created_at")
	}
	if err := request.PromptBundle.Validate(); err != nil {
		return fmt.Errorf("validate worker prompt bundle: %w", err)
	}
	if request.ReviewSkills == nil || len(request.ReviewSkills) == 0 ||
		len(request.ReviewSkills) > AgentReviewWorkerMaxSkillCount {
		return fmt.Errorf(
			"review_skills must contain between 1 and %d governed skills",
			AgentReviewWorkerMaxSkillCount,
		)
	}
	if len(request.ReviewSkills) != len(request.Plan.ReviewDimensions) {
		return fmt.Errorf("review_skills must match plan.review_dimensions")
	}
	seenSkills := make(map[string]struct{}, len(request.ReviewSkills))
	for index, skill := range request.ReviewSkills {
		if err := skill.Validate(); err != nil {
			return fmt.Errorf("validate worker review_skills[%d]: %w", index, err)
		}
		if skill.Ref != request.Plan.ReviewDimensions[index] {
			return fmt.Errorf("review_skills[%d] ref does not match plan.review_dimensions", index)
		}
		if _, duplicate := seenSkills[skill.Ref.ID]; duplicate {
			return fmt.Errorf("review_skills contains duplicate id %q", skill.Ref.ID)
		}
		seenSkills[skill.Ref.ID] = struct{}{}
	}
	if request.KnowledgePacks == nil ||
		len(request.KnowledgePacks) > AgentReviewWorkerMaxKnowledgeCount {
		return fmt.Errorf(
			"knowledge_packs must be an explicit array with at most %d entries",
			AgentReviewWorkerMaxKnowledgeCount,
		)
	}
	if len(request.KnowledgePacks) != len(request.Plan.Knowledge) {
		return fmt.Errorf("knowledge_packs must match plan.knowledge")
	}
	seenKnowledge := make(map[string]struct{}, len(request.KnowledgePacks))
	for index, pack := range request.KnowledgePacks {
		if err := pack.Validate(); err != nil {
			return fmt.Errorf("validate worker knowledge_packs[%d]: %w", index, err)
		}
		if pack.Ref != request.Plan.Knowledge[index] {
			return fmt.Errorf("knowledge_packs[%d] ref does not match plan.knowledge", index)
		}
		if _, duplicate := seenKnowledge[pack.Ref.ID]; duplicate {
			return fmt.Errorf("knowledge_packs contains duplicate id %q", pack.Ref.ID)
		}
		seenKnowledge[pack.Ref.ID] = struct{}{}
	}
	if request.ContextArtifacts == nil ||
		len(request.ContextArtifacts) > AgentReviewWorkerMaxContextCount {
		return fmt.Errorf(
			"context_artifacts must be an explicit array with at most %d entries",
			AgentReviewWorkerMaxContextCount,
		)
	}
	seenContexts := make(map[string]struct{}, len(request.ContextArtifacts))
	for index, artifact := range request.ContextArtifacts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("validate worker context_artifacts[%d]: %w", index, err)
		}
		if _, duplicate := seenContexts[artifact.ContextID]; duplicate {
			return fmt.Errorf("context_artifacts contains duplicate id %q", artifact.ContextID)
		}
		seenContexts[artifact.ContextID] = struct{}{}
	}
	if err := requireSHA256("checkpoint_scope_sha256", request.CheckpointScopeSHA256); err != nil {
		return err
	}
	if request.GroupCheckpoints == nil ||
		len(request.GroupCheckpoints) > AgentReviewWorkerMaxGroupCheckpoints {
		return fmt.Errorf(
			"group_checkpoints must be an explicit array with at most %d entries",
			AgentReviewWorkerMaxGroupCheckpoints,
		)
	}
	previousGroupID := ""
	var totalCheckpointBytes uint64
	for index, checkpoint := range request.GroupCheckpoints {
		if err := checkpoint.Validate(); err != nil {
			return fmt.Errorf("validate worker group_checkpoints[%d]: %w", index, err)
		}
		if checkpoint.CheckpointScopeSHA256 != request.CheckpointScopeSHA256 {
			return fmt.Errorf("group_checkpoints[%d] escaped checkpoint scope", index)
		}
		if index > 0 && checkpoint.GroupID <= previousGroupID {
			return fmt.Errorf("group_checkpoints must be uniquely sorted by group_id")
		}
		previousGroupID = checkpoint.GroupID
		if checkpoint.SizeBytes > AgentReviewWorkerMaxGroupCheckpointTotalBytes-totalCheckpointBytes {
			return fmt.Errorf(
				"group_checkpoints exceed %d total bytes",
				AgentReviewWorkerMaxGroupCheckpointTotalBytes,
			)
		}
		totalCheckpointBytes += checkpoint.SizeBytes
	}
	input, err := decodeAgentReviewWorkerInput(request.ReviewInputBase64)
	if err != nil {
		return err
	}
	inputBytes := uint64(len(input))
	if inputBytes > request.Capability.MaxInputBytes {
		return fmt.Errorf("review input exceeds capability.max_input_bytes")
	}
	if inputBytes > request.Plan.Budget.MaxTargetBytes {
		return fmt.Errorf("review input exceeds plan budget.max_target_bytes")
	}
	if request.Plan.ReviewInputRef.Ref.SizeBytes < 0 ||
		inputBytes != uint64(request.Plan.ReviewInputRef.Ref.SizeBytes) {
		return fmt.Errorf("review input size does not match plan.review_input_ref")
	}
	digest := sha256.Sum256(input)
	inputSHA256 := hex.EncodeToString(digest[:])
	if inputSHA256 != request.Plan.ReviewInputRef.Ref.SHA256 {
		return fmt.Errorf("review input digest does not match plan.review_input_ref")
	}
	if inputSHA256 != request.Plan.TargetDigest {
		return fmt.Errorf("review input digest does not match plan.target_digest")
	}
	if !json.Valid(input) || len(bytes.TrimSpace(input)) == 0 || bytes.TrimSpace(input)[0] != '{' {
		return fmt.Errorf("review input must be a JSON object")
	}
	return nil
}

// DecodeAgentReviewWorkerRulePack verifies the exact sealed RulePack bytes
// bound by plan.rule_pack. Semantic validation remains owned by reviewconfig
// before the formal plan is admitted; this transport boundary independently
// rejects content substitution and oversized input.
func DecodeAgentReviewWorkerRulePack(request AgentReviewWorkerRequest) ([]byte, error) {
	return decodeAgentReviewRulePack(request.Plan.RulePack, request.RulePackBase64)
}

func decodeAgentReviewRulePack(ref VersionedRef, contentBase64 string) ([]byte, error) {
	if contentBase64 == "" {
		return nil, fmt.Errorf("rule_pack_base64 is required")
	}
	content, err := base64.StdEncoding.Strict().DecodeString(contentBase64)
	if err != nil {
		return nil, fmt.Errorf("rule_pack_base64 must be canonical base64: %w", err)
	}
	if len(content) == 0 || len(content) > AgentReviewWorkerMaxRulePackBytes {
		return nil, fmt.Errorf("rule_pack_base64 must contain between 1 and %d bytes", AgentReviewWorkerMaxRulePackBytes)
	}
	if !json.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, fmt.Errorf("rule_pack_base64 must encode JSON without NUL bytes")
	}
	var pack struct {
		SchemaVersion string          `json:"schema_version"`
		ID            string          `json:"id"`
		Revision      string          `json:"revision"`
		Rules         json.RawMessage `json:"rules"`
		SHA256        string          `json:"sha256"`
	}
	if err := decodeAgentReviewShadowJSON(content, &pack); err != nil {
		return nil, fmt.Errorf("decode rule_pack_base64: %w", err)
	}
	if pack.SchemaVersion != "argus.rule_pack.v1alpha1" ||
		pack.ID != ref.ID || pack.Revision != ref.Revision ||
		pack.SHA256 != ref.SHA256 || len(pack.Rules) == 0 {
		return nil, fmt.Errorf("rule_pack_base64 identity does not match plan.rule_pack")
	}
	pack.SHA256 = ""
	canonical, err := json.Marshal(pack)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return nil, fmt.Errorf("rule_pack_base64 semantic digest does not match plan.rule_pack")
	}
	return content, nil
}

func (checkpoint AgentReviewWorkerGroupCheckpoint) Validate() error {
	if err := requireIdentifier("group_id", checkpoint.GroupID); err != nil {
		return err
	}
	if checkpoint.CheckpointRevision > agentReviewWorkerMaxJSONInteger {
		return fmt.Errorf("group checkpoint checkpoint_revision must be a JSON safe integer")
	}
	if err := requireSHA256("checkpoint_scope_sha256", checkpoint.CheckpointScopeSHA256); err != nil {
		return err
	}
	if err := requireSHA256("content_sha256", checkpoint.ContentSHA256); err != nil {
		return err
	}
	if checkpoint.SizeBytes == 0 || checkpoint.SizeBytes > AgentReviewWorkerMaxGroupCheckpointBytes {
		return fmt.Errorf("group checkpoint size_bytes must be between 1 and %d", AgentReviewWorkerMaxGroupCheckpointBytes)
	}
	content, err := base64.StdEncoding.Strict().DecodeString(checkpoint.ContentBase64)
	if err != nil || base64.StdEncoding.EncodeToString(content) != checkpoint.ContentBase64 {
		return fmt.Errorf("group checkpoint content_base64 must be canonical standard base64")
	}
	if uint64(len(content)) != checkpoint.SizeBytes {
		return fmt.Errorf("group checkpoint size_bytes does not match content")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != checkpoint.ContentSHA256 {
		return fmt.Errorf("group checkpoint content_sha256 does not match content")
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(content) {
		return fmt.Errorf("group checkpoint content must be a JSON object")
	}
	if err := rejectAgentReviewJSONNulls(content); err != nil {
		return fmt.Errorf("group checkpoint content: %w", err)
	}
	var identity struct {
		SchemaVersion         string `json:"schemaVersion"`
		CheckpointRevision    uint64 `json:"checkpointRevision"`
		CheckpointScopeSHA256 string `json:"checkpointScopeSha256"`
		Group                 struct {
			ID string `json:"id"`
		} `json:"group"`
	}
	if err := json.Unmarshal(content, &identity); err != nil {
		return fmt.Errorf("decode group checkpoint content identity: %w", err)
	}
	if identity.SchemaVersion != "argus.pi-review.group_checkpoint.v1" {
		return fmt.Errorf("group checkpoint content schema is unsupported")
	}
	if identity.CheckpointRevision != checkpoint.CheckpointRevision {
		return fmt.Errorf("group checkpoint revision does not match content")
	}
	if identity.CheckpointScopeSHA256 != checkpoint.CheckpointScopeSHA256 {
		return fmt.Errorf("group checkpoint scope does not match content")
	}
	if identity.Group.ID != checkpoint.GroupID {
		return fmt.Errorf("group checkpoint group identity does not match content")
	}
	return nil
}

func decodeAgentReviewWorkerInput(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("review_input_base64 must not be empty")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("review_input_base64 must be canonical standard base64: %w", err)
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("review_input_base64 must decode to non-empty input")
	}
	if base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("review_input_base64 must use canonical standard base64")
	}
	return decoded, nil
}

func (result AgentReviewWorkerResult) Validate() error {
	if result.SchemaVersion != AgentReviewWorkerResultSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentReviewWorkerResult schema %q",
			result.SchemaVersion,
		)
	}
	for name, value := range map[string]string{
		"work_item_id": result.WorkItemID, "idempotency_key": result.IdempotencyKey,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if result.Attempt < 1 || result.Generation < 1 || result.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if uint64(result.Attempt) > agentReviewWorkerMaxJSONInteger ||
		uint64(result.Generation) > agentReviewWorkerMaxJSONInteger ||
		result.FencingToken > agentReviewWorkerMaxJSONInteger {
		return fmt.Errorf("attempt, generation, and fencing_token must be JSON safe integers")
	}
	if err := requireSHA256("capability_sha256", result.CapabilitySHA256); err != nil {
		return err
	}
	if err := validateAgentReviewTime("completed_at", result.CompletedAt); err != nil {
		return err
	}
	switch result.Status {
	case AgentReviewWorkerSucceeded:
		if len(result.Report) == 0 || result.ReportSHA256 == nil || result.Failure != nil {
			return fmt.Errorf("succeeded worker result requires report and report_sha256 and forbids failure")
		}
		if err := validateAgentReviewWorkerReport(result.Report, *result.ReportSHA256); err != nil {
			return err
		}
	case AgentReviewWorkerFailed, AgentReviewWorkerCanceled:
		if len(result.Report) != 0 || result.ReportSHA256 != nil || result.Failure == nil {
			return fmt.Errorf("failed/canceled worker result requires failure and forbids report")
		}
		if err := result.Failure.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported agent review worker status %q", result.Status)
	}
	return nil
}

func validateAgentReviewWorkerReport(report json.RawMessage, expectedSHA256 string) error {
	if err := requireSHA256("report_sha256", expectedSHA256); err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(report)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(report) {
		return fmt.Errorf("worker report must be a JSON object")
	}
	if err := rejectAgentReviewJSONNulls(report); err != nil {
		return fmt.Errorf("worker report: %w", err)
	}
	digest := sha256.Sum256(report)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return fmt.Errorf("report_sha256 does not match exact report bytes")
	}
	return nil
}

func (failure AgentReviewWorkerFailure) validate() error {
	if err := requireIdentifier("failure.code", failure.Code); err != nil {
		return err
	}
	if err := requireBoundedAgentReviewText("failure.message", failure.Message, 4096, true); err != nil {
		return err
	}
	return nil
}

func ValidateAgentReviewWorkerResultBinding(
	request AgentReviewWorkerRequest,
	result AgentReviewWorkerResult,
) error {
	if err := request.Validate(); err != nil {
		return fmt.Errorf("validate worker request: %w", err)
	}
	if err := result.Validate(); err != nil {
		return fmt.Errorf("validate worker result: %w", err)
	}
	if result.WorkItemID != request.WorkItemID ||
		result.Attempt != request.Attempt ||
		result.Generation != request.Generation ||
		result.FencingToken != request.FencingToken ||
		result.IdempotencyKey != request.IdempotencyKey ||
		result.CapabilitySHA256 != request.Capability.SHA256 {
		return fmt.Errorf("worker result does not match exact request identity, fencing, and capability")
	}
	if result.CompletedAt.Before(request.Plan.CreatedAt) {
		return fmt.Errorf("worker result completed_at precedes plan.created_at")
	}
	if result.Status == AgentReviewWorkerSucceeded && result.CompletedAt.After(request.Deadline) {
		return fmt.Errorf("succeeded worker result completed after request deadline")
	}
	if uint64(len(result.Report)) > request.Capability.MaxOutputBytes {
		return fmt.Errorf("worker report exceeds capability.max_output_bytes")
	}
	return nil
}
