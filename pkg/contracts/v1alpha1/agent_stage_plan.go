package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	AgentStagePlanSchemaVersion = "argus.agent_stage_plan.v1alpha1"

	AgentStagePlanExecutionSnapshotContract       = "argus.execution_snapshot.v1alpha1"
	AgentStagePlanConfigBundleContract            = "argus.config_bundle.v1alpha1"
	AgentStagePlanConfigResolutionReceiptContract = "argus.config_resolution_receipt.v1alpha1"
	AgentStagePlanWorkflowContract                = "argus.workflow.v1alpha1"
	AgentStagePlanReviewSpecContract              = ReviewSpecSchemaVersion
	AgentStagePlanReviewInputContract             = "argus.review_input.v1alpha1"

	AgentStagePlanAgentContract                  = "argus.agent_profile.v1alpha1"
	AgentStagePlanProviderContract               = "argus.provider_profile.v1alpha1"
	AgentStagePlanModelContract                  = "argus.model_profile.v1alpha1"
	AgentStagePlanRuntimeContract                = "argus.runtime_profile.v1alpha1"
	AgentStagePlanPromptContract                 = "argus.prompt_bundle.v1alpha1"
	AgentStagePlanSkillContract                  = "argus.skill_pack.v1alpha1"
	AgentStagePlanKnowledgeContract              = "argus.knowledge_pack.v1alpha1"
	AgentStagePlanAPIProtocolContract            = "argus.model_api_protocol.v1alpha1"
	AgentStagePlanContextProviderAdapterContract = "argus.context_provider_adapter.v1alpha1"

	AgentStagePlanOutputContract = ReviewHypothesisSetSchemaVersion

	AgentStageModelEgressProviderBrokerOnly = "provider_broker_only"
	AgentStageWorkspaceReadFrozenInputOnly  = "frozen_input_only"
	AgentStageDispositionHypothesisOnly     = "hypothesis_only"
	AgentStageSideEffectsDeny               = "deny"
	agentStageMaxJSONInteger                = int64(1<<53 - 1)
)

type AgentStageSkillPhase string

const (
	AgentStageSkillPhaseGrouping     AgentStageSkillPhase = "grouping"
	AgentStageSkillPhaseContext      AgentStageSkillPhase = "context"
	AgentStageSkillPhaseReview       AgentStageSkillPhase = "review"
	AgentStageSkillPhaseVerification AgentStageSkillPhase = "verification"
)

// AgentStageComponentBinding binds a governed component identity to its exact
// immutable content. Ref.SHA256 and Artifact.Ref.SHA256 must be identical.
type AgentStageComponentBinding struct {
	Ref      VersionedRef    `json:"ref"`
	Artifact ArtifactBinding `json:"artifact"`
}

type AgentStageSkillBinding struct {
	ID       string               `json:"id"`
	Phase    AgentStageSkillPhase `json:"phase"`
	Ref      VersionedRef         `json:"ref"`
	Artifact ArtifactBinding      `json:"artifact"`
}

type AgentStageKnowledgeBinding struct {
	ID       string          `json:"id"`
	Ref      VersionedRef    `json:"ref"`
	Artifact ArtifactBinding `json:"artifact"`
}

type AgentStageContextProviderBinding struct {
	ID       string                     `json:"id"`
	Revision string                     `json:"revision"`
	Kind     string                     `json:"kind"`
	Adapter  AgentStageComponentBinding `json:"adapter"`
}

type AgentStageModelAuthority struct {
	ModelEgress string                     `json:"model_egress"`
	APIProtocol AgentStageComponentBinding `json:"api_protocol"`
}

type AgentStageToolAuthority struct {
	Tools              []string `json:"tools"`
	ToolNetwork        string   `json:"tool_network"`
	WorkspaceReads     string   `json:"workspace_reads"`
	WorkspaceWrites    string   `json:"workspace_writes"`
	RemoteWrites       string   `json:"remote_writes"`
	MaxDelegationDepth uint32   `json:"max_delegation_depth"`
}

type AgentStageBudget struct {
	MaxFiles        int   `json:"max_files"`
	MaxGroups       int   `json:"max_groups"`
	MaxHypotheses   int   `json:"max_hypotheses"`
	MaxModelCalls   int   `json:"max_model_calls"`
	MaxToolCalls    int   `json:"max_tool_calls"`
	MaxTargetBytes  int64 `json:"max_target_bytes"`
	MaxGroupBytes   int64 `json:"max_group_bytes"`
	MaxOutputBytes  int64 `json:"max_output_bytes"`
	MaxOutputTokens int64 `json:"max_output_tokens"`
	MaxCostMicros   int64 `json:"max_cost_micros"`
	TimeoutMS       int64 `json:"timeout_ms"`
	MaxConcurrency  int   `json:"max_concurrency"`
}

// AgentStageRetryPolicy is the immutable, execution-bound retry authority for
// the formal stage. Unknown provider outcomes always reconcile the same exact
// attempt; only an authenticated failed result whose code is explicitly
// listed may advance to a new attempt.
type AgentStageRetryPolicy struct {
	MaxAttempts    int      `json:"max_attempts"`
	BackoffMS      int64    `json:"backoff_ms"`
	RetryableCodes []string `json:"retryable_codes"`
	UnknownOutcome string   `json:"unknown_outcome"`
}

// AgentStagePlan is the canonical, execution-bound formal agent-stage input.
// It contains only governed identities and immutable artifact bindings: no
// credentials, provider endpoints, repository paths, raw prompts, timestamps,
// or mutable latest references are part of this contract.
type AgentStagePlan struct {
	SchemaVersion  string `json:"schema_version"`
	PlanID         string `json:"plan_id"`
	SHA256         string `json:"sha256"`
	BehaviorSHA256 string `json:"behavior_sha256"`

	ReviewRunID   string       `json:"review_run_id"`
	Stage         VersionedRef `json:"stage"`
	TargetDigest  string       `json:"target_digest"`
	BuildIdentity string       `json:"build_identity"`

	ExecutionSnapshot          ArtifactBinding `json:"execution_snapshot"`
	ConfigBundle               ArtifactBinding `json:"config_bundle"`
	ConfigResolutionReceiptRef ArtifactBinding `json:"config_resolution_receipt_ref"`
	Workflow                   ArtifactBinding `json:"workflow"`
	ReviewSpec                 ArtifactBinding `json:"review_spec"`
	ReviewInput                ArtifactBinding `json:"review_input"`

	AgentReviewPolicy VersionedRef                       `json:"agent_review_policy"`
	RulePack          VersionedRef                       `json:"rule_pack"`
	Normalization     VersionedRef                       `json:"normalization"`
	RulePackBase64    string                             `json:"rule_pack_base64"`
	Agent             AgentStageComponentBinding         `json:"agent"`
	Provider          AgentStageComponentBinding         `json:"provider"`
	Model             AgentStageComponentBinding         `json:"model"`
	Runtime           AgentStageComponentBinding         `json:"runtime"`
	Prompt            AgentStageComponentBinding         `json:"prompt"`
	Skills            []AgentStageSkillBinding           `json:"skills"`
	Knowledge         []AgentStageKnowledgeBinding       `json:"knowledge"`
	ContextProviders  []AgentStageContextProviderBinding `json:"context_providers"`

	ModelAuthority AgentStageModelAuthority `json:"model_authority"`
	ToolAuthority  AgentStageToolAuthority  `json:"tool_authority"`
	Budget         AgentStageBudget         `json:"budget"`
	Retry          AgentStageRetryPolicy    `json:"retry"`

	OutputContract string `json:"output_contract"`
	Disposition    string `json:"disposition"`
	SideEffects    string `json:"side_effects"`
}

// SealAgentStagePlan canonicalizes the tool set and derives both behavior and
// full-content identities. Skills, knowledge, and context providers retain
// their governed ordered-stable-ID sequence. The same exact ordered closure
// always produces byte-identical JSON when marshaled with encoding/json.
func SealAgentStagePlan(plan AgentStagePlan) (AgentStagePlan, error) {
	if plan.SchemaVersion != "" && plan.SchemaVersion != AgentStagePlanSchemaVersion {
		return AgentStagePlan{}, fmt.Errorf(
			"unsupported AgentStagePlan schema %q",
			plan.SchemaVersion,
		)
	}
	plan.SchemaVersion = AgentStagePlanSchemaVersion
	plan.PlanID = ""
	plan.SHA256 = ""
	plan.BehaviorSHA256 = ""
	if plan.Retry.MaxAttempts == 0 && plan.Retry.BackoffMS == 0 &&
		plan.Retry.RetryableCodes == nil && plan.Retry.UnknownOutcome == "" {
		plan.Retry = AgentStageRetryPolicy{
			MaxAttempts: 1, RetryableCodes: []string{}, UnknownOutcome: "fail",
		}
	}
	plan = canonicalizeAgentStagePlan(plan)
	if err := plan.validateContent(); err != nil {
		return AgentStagePlan{}, err
	}
	behaviorSHA256, err := digestAgentStagePlanBehavior(plan)
	if err != nil {
		return AgentStagePlan{}, err
	}
	plan.BehaviorSHA256 = behaviorSHA256
	fullSHA256, err := digestAgentStagePlan(plan)
	if err != nil {
		return AgentStagePlan{}, err
	}
	plan.SHA256 = fullSHA256
	plan.PlanID = "agent-stage-plan-" + fullSHA256[:24]
	if err := plan.Validate(); err != nil {
		return AgentStagePlan{}, fmt.Errorf("validate sealed AgentStagePlan: %w", err)
	}
	return plan, nil
}

func DecodeAgentStagePlan(data []byte) (AgentStagePlan, error) {
	var plan AgentStagePlan
	if err := rejectDuplicateJSONFields(data); err != nil {
		return AgentStagePlan{}, fmt.Errorf("decode AgentStagePlan: %w", err)
	}
	if err := rejectAgentStagePlanJSONNulls(data); err != nil {
		return AgentStagePlan{}, fmt.Errorf("decode AgentStagePlan: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return AgentStagePlan{}, fmt.Errorf("decode AgentStagePlan: %w", err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return AgentStagePlan{}, fmt.Errorf("decode AgentStagePlan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return AgentStagePlan{}, err
	}
	return plan, nil
}

func (plan AgentStagePlan) Validate() error {
	if err := plan.validateContent(); err != nil {
		return err
	}
	if err := requireIdentifier("plan_id", plan.PlanID); err != nil {
		return err
	}
	if err := requireSHA256("sha256", plan.SHA256); err != nil {
		return err
	}
	if err := requireSHA256("behavior_sha256", plan.BehaviorSHA256); err != nil {
		return err
	}
	behaviorSHA256, err := digestAgentStagePlanBehavior(plan)
	if err != nil {
		return err
	}
	if plan.BehaviorSHA256 != behaviorSHA256 {
		return fmt.Errorf("behavior_sha256 does not match AgentStagePlan behavior")
	}
	fullSHA256, err := digestAgentStagePlan(plan)
	if err != nil {
		return err
	}
	if plan.SHA256 != fullSHA256 {
		return fmt.Errorf("sha256 does not match AgentStagePlan content")
	}
	wantID := "agent-stage-plan-" + fullSHA256[:24]
	if plan.PlanID != wantID {
		return fmt.Errorf("plan_id must be %q for the exact AgentStagePlan content", wantID)
	}
	return nil
}

func (plan AgentStagePlan) validateContent() error {
	if plan.SchemaVersion != AgentStagePlanSchemaVersion {
		return fmt.Errorf("unsupported AgentStagePlan schema %q", plan.SchemaVersion)
	}
	if err := requireIdentifier("review_run_id", plan.ReviewRunID); err != nil {
		return err
	}
	if err := validateAgentStageExactVersionedRef(plan.Stage, "stage"); err != nil {
		return err
	}
	if err := requireSHA256("target_digest", plan.TargetDigest); err != nil {
		return err
	}
	if err := requireIdentifier("build_identity", plan.BuildIdentity); err != nil {
		return err
	}
	for _, item := range []struct {
		name     string
		binding  ArtifactBinding
		contract string
	}{
		{"execution_snapshot", plan.ExecutionSnapshot, AgentStagePlanExecutionSnapshotContract},
		{"config_bundle", plan.ConfigBundle, AgentStagePlanConfigBundleContract},
		{
			"config_resolution_receipt_ref",
			plan.ConfigResolutionReceiptRef,
			AgentStagePlanConfigResolutionReceiptContract,
		},
		{"workflow", plan.Workflow, AgentStagePlanWorkflowContract},
		{"review_spec", plan.ReviewSpec, AgentStagePlanReviewSpecContract},
		{"review_input", plan.ReviewInput, AgentStagePlanReviewInputContract},
	} {
		if err := validateAgentStageArtifactBinding(
			item.binding,
			item.name,
			true,
		); err != nil {
			return err
		}
		if item.binding.Contract != item.contract {
			return fmt.Errorf("%s.contract must be %q", item.name, item.contract)
		}
	}
	if plan.TargetDigest != plan.ReviewInput.Ref.SHA256 {
		return fmt.Errorf("target_digest must equal review_input.ref.sha256")
	}
	if err := validateAgentStageExactVersionedRef(
		plan.AgentReviewPolicy,
		"agent_review_policy",
	); err != nil {
		return err
	}
	if err := validateAgentStageExactVersionedRef(plan.RulePack, "rule_pack"); err != nil {
		return err
	}
	if err := ValidateCandidateNormalizationImplementation(
		plan.Normalization,
		"normalization",
	); err != nil {
		return err
	}
	if plan.Normalization.SHA256 != plan.Agent.Ref.SHA256 {
		return fmt.Errorf("normalization must bind the exact agent worker implementation digest")
	}
	if _, err := decodeAgentReviewRulePack(plan.RulePack, plan.RulePackBase64); err != nil {
		return fmt.Errorf("validate rule_pack_base64: %w", err)
	}
	for _, item := range []struct {
		name     string
		binding  AgentStageComponentBinding
		contract string
	}{
		{"agent", plan.Agent, AgentStagePlanAgentContract},
		{"provider", plan.Provider, AgentStagePlanProviderContract},
		{"model", plan.Model, AgentStagePlanModelContract},
		{"runtime", plan.Runtime, AgentStagePlanRuntimeContract},
		{"prompt", plan.Prompt, AgentStagePlanPromptContract},
	} {
		if err := item.binding.validate(item.name, item.contract); err != nil {
			return err
		}
	}
	if err := validateAgentStageSkills(plan.Skills); err != nil {
		return err
	}
	if err := validateAgentStageKnowledge(plan.Knowledge); err != nil {
		return err
	}
	if err := validateAgentStageContextProviders(plan.ContextProviders); err != nil {
		return err
	}
	if err := plan.ModelAuthority.validate(); err != nil {
		return err
	}
	if err := plan.ToolAuthority.validate(); err != nil {
		return err
	}
	if err := plan.Budget.validate(); err != nil {
		return err
	}
	if err := plan.Retry.validate(); err != nil {
		return err
	}
	if plan.OutputContract != AgentStagePlanOutputContract {
		return fmt.Errorf("output_contract must be %q", AgentStagePlanOutputContract)
	}
	if plan.Disposition != AgentStageDispositionHypothesisOnly {
		return fmt.Errorf("disposition must be %q", AgentStageDispositionHypothesisOnly)
	}
	if plan.SideEffects != AgentStageSideEffectsDeny {
		return fmt.Errorf("side_effects must be %q", AgentStageSideEffectsDeny)
	}
	return nil
}

func (binding AgentStageComponentBinding) validate(name, contract string) error {
	if err := validateAgentStageExactVersionedRef(binding.Ref, name+".ref"); err != nil {
		return err
	}
	if err := validateAgentStageArtifactBinding(
		binding.Artifact,
		name+".artifact",
		true,
	); err != nil {
		return err
	}
	if binding.Artifact.Contract != contract {
		return fmt.Errorf("%s.artifact.contract must be %q", name, contract)
	}
	if binding.Ref.SHA256 != binding.Artifact.Ref.SHA256 {
		return fmt.Errorf("%s ref and artifact must bind the same content SHA-256", name)
	}
	return nil
}

func (binding AgentStageSkillBinding) validate(name string) error {
	if err := requireIdentifier(name+".id", binding.ID); err != nil {
		return err
	}
	switch binding.Phase {
	case AgentStageSkillPhaseGrouping,
		AgentStageSkillPhaseContext,
		AgentStageSkillPhaseReview,
		AgentStageSkillPhaseVerification:
	default:
		return fmt.Errorf("%s.phase has unsupported value %q", name, binding.Phase)
	}
	component := AgentStageComponentBinding{Ref: binding.Ref, Artifact: binding.Artifact}
	return component.validate(name, AgentStagePlanSkillContract)
}

func (authority AgentStageModelAuthority) validate() error {
	if authority.ModelEgress != AgentStageModelEgressProviderBrokerOnly {
		return fmt.Errorf(
			"model_authority.model_egress must be %q",
			AgentStageModelEgressProviderBrokerOnly,
		)
	}
	return authority.APIProtocol.validate(
		"model_authority.api_protocol",
		AgentStagePlanAPIProtocolContract,
	)
}

func (authority AgentStageToolAuthority) validate() error {
	if authority.Tools == nil || !slices.IsSorted(authority.Tools) {
		return fmt.Errorf("tool_authority.tools must be an explicit sorted array")
	}
	for index, tool := range authority.Tools {
		if err := requireIdentifier(
			fmt.Sprintf("tool_authority.tools[%d]", index),
			tool,
		); err != nil {
			return err
		}
		if index > 0 && tool == authority.Tools[index-1] {
			return fmt.Errorf("tool_authority.tools contains duplicate %q", tool)
		}
	}
	if authority.ToolNetwork != AgentStageSideEffectsDeny ||
		authority.WorkspaceWrites != AgentStageSideEffectsDeny ||
		authority.RemoteWrites != AgentStageSideEffectsDeny {
		return fmt.Errorf(
			"tool_authority tool_network, workspace_writes, and remote_writes must be deny",
		)
	}
	if authority.WorkspaceReads != AgentStageWorkspaceReadFrozenInputOnly {
		return fmt.Errorf(
			"tool_authority.workspace_reads must be %q",
			AgentStageWorkspaceReadFrozenInputOnly,
		)
	}
	if authority.MaxDelegationDepth != 0 {
		return fmt.Errorf("tool_authority.max_delegation_depth must be zero")
	}
	return nil
}

func (budget AgentStageBudget) validate() error {
	if budget.MaxFiles <= 0 || budget.MaxGroups <= 0 || budget.MaxHypotheses <= 0 ||
		budget.MaxModelCalls <= 0 || budget.MaxToolCalls <= 0 ||
		budget.MaxTargetBytes <= 0 || budget.MaxGroupBytes <= 0 ||
		budget.MaxOutputBytes <= 0 || budget.MaxOutputTokens <= 0 ||
		budget.MaxCostMicros <= 0 || budget.TimeoutMS <= 0 ||
		budget.MaxConcurrency <= 0 {
		return fmt.Errorf("all agent stage budget limits must be positive")
	}
	for _, limit := range []struct {
		name  string
		value int64
	}{
		{"max_files", int64(budget.MaxFiles)},
		{"max_groups", int64(budget.MaxGroups)},
		{"max_hypotheses", int64(budget.MaxHypotheses)},
		{"max_model_calls", int64(budget.MaxModelCalls)},
		{"max_tool_calls", int64(budget.MaxToolCalls)},
		{"max_target_bytes", budget.MaxTargetBytes},
		{"max_group_bytes", budget.MaxGroupBytes},
		{"max_output_bytes", budget.MaxOutputBytes},
		{"max_output_tokens", budget.MaxOutputTokens},
		{"max_cost_micros", budget.MaxCostMicros},
		{"timeout_ms", budget.TimeoutMS},
		{"max_concurrency", int64(budget.MaxConcurrency)},
	} {
		if limit.value > agentStageMaxJSONInteger {
			return fmt.Errorf("budget.%s must be a JSON safe integer", limit.name)
		}
	}
	if budget.MaxGroupBytes > budget.MaxTargetBytes {
		return fmt.Errorf("budget.max_group_bytes must not exceed max_target_bytes")
	}
	if budget.MaxConcurrency > budget.MaxGroups {
		return fmt.Errorf("budget.max_concurrency must not exceed max_groups")
	}
	return nil
}

func (policy AgentStageRetryPolicy) validate() error {
	if policy.MaxAttempts < 1 || int64(policy.MaxAttempts) > agentStageMaxJSONInteger {
		return fmt.Errorf("retry.max_attempts must be a JSON safe positive integer")
	}
	if policy.BackoffMS < 0 || policy.BackoffMS > agentStageMaxJSONInteger {
		return fmt.Errorf("retry.backoff_ms must be a JSON safe non-negative integer")
	}
	if policy.UnknownOutcome != "reconcile" && policy.UnknownOutcome != "fail" {
		return fmt.Errorf("retry.unknown_outcome must be %q or %q", "reconcile", "fail")
	}
	if policy.RetryableCodes == nil {
		return fmt.Errorf("retry.retryable_codes must be an explicit array")
	}
	previous := ""
	for index, code := range policy.RetryableCodes {
		if err := requireIdentifier(fmt.Sprintf("retry.retryable_codes[%d]", index), code); err != nil {
			return err
		}
		if code <= previous {
			return fmt.Errorf("retry.retryable_codes must be sorted and unique")
		}
		previous = code
	}
	if policy.MaxAttempts > 1 && len(policy.RetryableCodes) == 0 {
		return fmt.Errorf("retry.retryable_codes must be non-empty when max_attempts exceeds one")
	}
	return nil
}

func validateAgentStageArtifactBinding(
	binding ArtifactBinding,
	name string,
	nonEmpty bool,
) error {
	if err := binding.validate(name, nonEmpty); err != nil {
		return err
	}
	if binding.Ref.SizeBytes > agentStageMaxJSONInteger {
		return fmt.Errorf("%s.ref.size_bytes must be a JSON safe integer", name)
	}
	return nil
}

func validateAgentStageSkills(skills []AgentStageSkillBinding) error {
	if skills == nil || len(skills) == 0 {
		return fmt.Errorf("skills must be an explicit non-empty array")
	}
	seenIDs := make(map[string]struct{}, len(skills))
	reviewSkills := 0
	for index, skill := range skills {
		name := fmt.Sprintf("skills[%d]", index)
		if err := skill.validate(name); err != nil {
			return err
		}
		if _, duplicate := seenIDs[skill.ID]; duplicate {
			return fmt.Errorf("skills contains duplicate id %q", skill.ID)
		}
		seenIDs[skill.ID] = struct{}{}
		if skill.Phase == AgentStageSkillPhaseReview {
			reviewSkills++
		}
	}
	if reviewSkills == 0 {
		return fmt.Errorf("skills must contain at least one review phase binding")
	}
	return nil
}

func validateAgentStageKnowledge(bindings []AgentStageKnowledgeBinding) error {
	const name = "knowledge"
	if bindings == nil {
		return fmt.Errorf("%s must be an explicit array", name)
	}
	seenIDs := make(map[string]struct{}, len(bindings))
	for index, binding := range bindings {
		itemName := fmt.Sprintf("%s[%d]", name, index)
		if err := requireIdentifier(itemName+".id", binding.ID); err != nil {
			return err
		}
		component := AgentStageComponentBinding{Ref: binding.Ref, Artifact: binding.Artifact}
		if err := component.validate(itemName, AgentStagePlanKnowledgeContract); err != nil {
			return err
		}
		if _, duplicate := seenIDs[binding.ID]; duplicate {
			return fmt.Errorf("%s contains duplicate id %q", name, binding.ID)
		}
		seenIDs[binding.ID] = struct{}{}
	}
	return nil
}

func validateAgentStageContextProviders(bindings []AgentStageContextProviderBinding) error {
	if bindings == nil {
		return fmt.Errorf("context_providers must be an explicit array")
	}
	seenIDs := make(map[string]struct{}, len(bindings))
	for index, binding := range bindings {
		name := fmt.Sprintf("context_providers[%d]", index)
		if err := requireIdentifier(name+".id", binding.ID); err != nil {
			return err
		}
		if err := requireIdentifier(name+".revision", binding.Revision); err != nil {
			return err
		}
		if strings.EqualFold(binding.Revision, "latest") {
			return fmt.Errorf("%s.revision must not use latest", name)
		}
		switch binding.Kind {
		case "repository_search", "codegraph", "lsp", "dependency", "artifact", "go_ast", "compile":
		default:
			return fmt.Errorf("%s.kind has unsupported value %q", name, binding.Kind)
		}
		if err := binding.Adapter.validate(
			name+".adapter",
			AgentStagePlanContextProviderAdapterContract,
		); err != nil {
			return err
		}
		if _, duplicate := seenIDs[binding.ID]; duplicate {
			return fmt.Errorf("context_providers contains duplicate id %q", binding.ID)
		}
		seenIDs[binding.ID] = struct{}{}
	}
	return nil
}

func canonicalizeAgentStagePlan(plan AgentStagePlan) AgentStagePlan {
	plan.Skills = slices.Clone(plan.Skills)
	plan.Knowledge = slices.Clone(plan.Knowledge)
	plan.ContextProviders = slices.Clone(plan.ContextProviders)
	plan.ToolAuthority.Tools = slices.Clone(plan.ToolAuthority.Tools)
	slices.Sort(plan.ToolAuthority.Tools)
	return plan
}

func validateAgentStageExactVersionedRef(ref VersionedRef, name string) error {
	if err := ref.validate(name); err != nil {
		return err
	}
	if strings.EqualFold(ref.ID, "latest") || strings.EqualFold(ref.Revision, "latest") {
		return fmt.Errorf("%s must not use latest", name)
	}
	return nil
}

func digestAgentStagePlan(plan AgentStagePlan) (string, error) {
	copy := plan
	copy.PlanID = ""
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStagePlan for digest: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

type agentStageBehaviorArtifact struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Contract  string `json:"contract"`
}

type agentStageBehaviorComponent struct {
	Ref      VersionedRef               `json:"ref"`
	Artifact agentStageBehaviorArtifact `json:"artifact"`
}

type agentStageBehaviorSkill struct {
	ID       string                     `json:"id"`
	Phase    AgentStageSkillPhase       `json:"phase"`
	Ref      VersionedRef               `json:"ref"`
	Artifact agentStageBehaviorArtifact `json:"artifact"`
}

type agentStageBehaviorKnowledge struct {
	ID       string                     `json:"id"`
	Ref      VersionedRef               `json:"ref"`
	Artifact agentStageBehaviorArtifact `json:"artifact"`
}

type agentStageBehaviorContextProvider struct {
	ID       string                      `json:"id"`
	Revision string                      `json:"revision"`
	Kind     string                      `json:"kind"`
	Adapter  agentStageBehaviorComponent `json:"adapter"`
}

type agentStageBehaviorModelAuthority struct {
	ModelEgress string                      `json:"model_egress"`
	APIProtocol agentStageBehaviorComponent `json:"api_protocol"`
}

type agentStageBehavior struct {
	SchemaVersion string       `json:"schema_version"`
	Stage         VersionedRef `json:"stage"`
	TargetDigest  string       `json:"target_digest"`
	BuildIdentity string       `json:"build_identity"`

	ReviewInput agentStageBehaviorArtifact `json:"review_input"`

	AgentReviewPolicy VersionedRef                        `json:"agent_review_policy"`
	RulePack          VersionedRef                        `json:"rule_pack"`
	Normalization     VersionedRef                        `json:"normalization"`
	Agent             agentStageBehaviorComponent         `json:"agent"`
	Provider          agentStageBehaviorComponent         `json:"provider"`
	Model             agentStageBehaviorComponent         `json:"model"`
	Runtime           agentStageBehaviorComponent         `json:"runtime"`
	Prompt            agentStageBehaviorComponent         `json:"prompt"`
	Skills            []agentStageBehaviorSkill           `json:"skills"`
	Knowledge         []agentStageBehaviorKnowledge       `json:"knowledge"`
	ContextProviders  []agentStageBehaviorContextProvider `json:"context_providers"`

	ModelAuthority agentStageBehaviorModelAuthority `json:"model_authority"`
	ToolAuthority  AgentStageToolAuthority          `json:"tool_authority"`
	Budget         AgentStageBudget                 `json:"budget"`
	Retry          AgentStageRetryPolicy            `json:"retry"`

	OutputContract string `json:"output_contract"`
	Disposition    string `json:"disposition"`
	SideEffects    string `json:"side_effects"`
}

func digestAgentStagePlanBehavior(plan AgentStagePlan) (string, error) {
	behavior := agentStageBehavior{
		SchemaVersion: plan.SchemaVersion,
		Stage:         plan.Stage, TargetDigest: plan.TargetDigest,
		BuildIdentity:     plan.BuildIdentity,
		ReviewInput:       behaviorArtifact(plan.ReviewInput),
		AgentReviewPolicy: plan.AgentReviewPolicy,
		RulePack:          plan.RulePack,
		Normalization:     plan.Normalization,
		Agent:             behaviorComponent(plan.Agent),
		Provider:          behaviorComponent(plan.Provider),
		Model:             behaviorComponent(plan.Model),
		Runtime:           behaviorComponent(plan.Runtime),
		Prompt:            behaviorComponent(plan.Prompt),
		Skills:            make([]agentStageBehaviorSkill, len(plan.Skills)),
		Knowledge:         make([]agentStageBehaviorKnowledge, len(plan.Knowledge)),
		ContextProviders:  make([]agentStageBehaviorContextProvider, len(plan.ContextProviders)),
		ModelAuthority: agentStageBehaviorModelAuthority{
			ModelEgress: plan.ModelAuthority.ModelEgress,
			APIProtocol: behaviorComponent(plan.ModelAuthority.APIProtocol),
		},
		ToolAuthority:  plan.ToolAuthority,
		Budget:         plan.Budget,
		Retry:          plan.Retry,
		OutputContract: plan.OutputContract,
		Disposition:    plan.Disposition,
		SideEffects:    plan.SideEffects,
	}
	for index, skill := range plan.Skills {
		behavior.Skills[index] = agentStageBehaviorSkill{
			ID:       skill.ID,
			Phase:    skill.Phase,
			Ref:      skill.Ref,
			Artifact: behaviorArtifact(skill.Artifact),
		}
	}
	for index, binding := range plan.Knowledge {
		behavior.Knowledge[index] = agentStageBehaviorKnowledge{
			ID: binding.ID, Ref: binding.Ref,
			Artifact: behaviorArtifact(binding.Artifact),
		}
	}
	for index, binding := range plan.ContextProviders {
		behavior.ContextProviders[index] = agentStageBehaviorContextProvider{
			ID: binding.ID, Revision: binding.Revision, Kind: binding.Kind,
			Adapter: behaviorComponent(binding.Adapter),
		}
	}
	data, err := json.Marshal(behavior)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStagePlan behavior for digest: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func behaviorArtifact(binding ArtifactBinding) agentStageBehaviorArtifact {
	return agentStageBehaviorArtifact{
		SHA256:    binding.Ref.SHA256,
		SizeBytes: binding.Ref.SizeBytes,
		Contract:  binding.Contract,
	}
}

func behaviorComponent(binding AgentStageComponentBinding) agentStageBehaviorComponent {
	return agentStageBehaviorComponent{
		Ref:      binding.Ref,
		Artifact: behaviorArtifact(binding.Artifact),
	}
}

func rejectAgentStagePlanJSONNulls(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if token == nil {
			return fmt.Errorf("explicit JSON null is not allowed")
		}
	}
}
