// Package reviewconfig owns the pure domain model and deterministic resolution
// of Argus review configuration. It deliberately has no storage, transport, or
// runtime dependencies.
package reviewconfig

type Scope string

const (
	ScopePlatform     Scope = "platform"
	ScopeTenant       Scope = "tenant"
	ScopeOrganization Scope = "organization"
	ScopeRepository   Scope = "repository"
	ScopePath         Scope = "path"
	ScopeInvocation   Scope = "invocation"
)

type MergeStrategy string

const (
	MergeOverride                MergeStrategy = "override"
	MergeOrderedStableIDOverride MergeStrategy = "ordered_stable_id_override"
	MergeSetUnionSubtract        MergeStrategy = "set_union_subtract"
	MergeDenyWins                MergeStrategy = "deny_wins"
	MergeReplaceOnly             MergeStrategy = "replace_only"
)

type Permission string

const (
	PermissionAllow Permission = "allow"
	PermissionDeny  Permission = "deny"
)

type Selector struct {
	TenantID       string `json:"tenant_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	RepositoryID   string `json:"repository_id,omitempty"`
	PathPrefix     string `json:"path_prefix,omitempty"`
	InvocationID   string `json:"invocation_id,omitempty"`
}

type ResolutionContext struct {
	TenantID       string `json:"tenant_id"`
	OrganizationID string `json:"organization_id"`
	RepositoryID   string `json:"repository_id"`
	Path           string `json:"path"`
	InvocationID   string `json:"invocation_id"`
}

type Specificity struct {
	ScopeRank int
	PathDepth int
}

type Revision struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"id"`
	Revision      string      `json:"revision"`
	Scope         Scope       `json:"scope"`
	Selector      Selector    `json:"selector"`
	Patch         ConfigPatch `json:"patch"`
}

type ConfigPatch struct {
	Target            *TargetPatch            `json:"target,omitempty"`
	RulePack          *RulePackPatch          `json:"rule_pack,omitempty"`
	Workflow          *WorkflowPatch          `json:"workflow,omitempty"`
	Execution         *ExecutionPatch         `json:"execution,omitempty"`
	AgentReview       *AgentReviewPatch       `json:"agent_review,omitempty"`
	Budget            *BudgetPatch            `json:"budget,omitempty"`
	Verification      *VerificationPatch      `json:"verification,omitempty"`
	Adjudication      *AdjudicationPatch      `json:"adjudication,omitempty"`
	FindingGovernance *FindingGovernancePatch `json:"finding_governance,omitempty"`
	Publication       *PublicationPatch       `json:"publication,omitempty"`
	Data              *DataPatch              `json:"data,omitempty"`
}

// SetPatch performs a deterministic union followed by subtraction. Both arrays
// must be explicit, even when empty, so an empty policy still has provenance.
type SetPatch struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

type TargetPatch struct {
	AllowedModes  *SetPatch `json:"allowed_modes,omitempty"`
	Include       *SetPatch `json:"include,omitempty"`
	Exclude       *SetPatch `json:"exclude,omitempty"`
	MaxFiles      *int      `json:"max_files,omitempty"`
	MaxPatchBytes *int64    `json:"max_patch_bytes,omitempty"`
}

type VersionedRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

type SecretRef struct {
	URI string `json:"uri"`
}

type RuleDefinition struct {
	ID            string       `json:"id"`
	Revision      string       `json:"revision"`
	Kind          string       `json:"kind"`
	Detector      VersionedRef `json:"detector"`
	Languages     []string     `json:"languages"`
	PathPrefixes  []string     `json:"path_prefixes"`
	EvidenceKinds []string     `json:"evidence_kinds"`
	Severity      string       `json:"severity"`
	Enabled       bool         `json:"enabled"`
}

type OrderedRulePatch struct {
	Upsert []RuleDefinition `json:"upsert"`
	Remove []string         `json:"remove"`
}

type RulePackPatch struct {
	Rules *OrderedRulePatch `json:"rules,omitempty"`
}

type RulePack struct {
	SchemaVersion string           `json:"schema_version"`
	ID            string           `json:"id"`
	Revision      string           `json:"revision"`
	Rules         []RuleDefinition `json:"rules"`
	SHA256        string           `json:"sha256"`
}

type WorkflowPatch struct {
	Definition *VersionedRef `json:"definition,omitempty"`
}

type ContextProviderDefinition struct {
	ID       string       `json:"id"`
	Revision string       `json:"revision"`
	Kind     string       `json:"kind"`
	Adapter  VersionedRef `json:"adapter"`
}

type OrderedContextProviderPatch struct {
	Upsert []ContextProviderDefinition `json:"upsert"`
	Remove []string                    `json:"remove"`
}

type ExecutionPatch struct {
	AgentProfile                  *VersionedRef                `json:"agent_profile,omitempty"`
	ModelProfile                  *VersionedRef                `json:"model_profile,omitempty"`
	ModelCredential               *SecretRef                   `json:"model_credential_ref,omitempty"`
	AllowedTools                  *SetPatch                    `json:"allowed_tools,omitempty"`
	ContextProviders              *OrderedContextProviderPatch `json:"context_providers,omitempty"`
	ContextProviderMaxConcurrency *int                         `json:"context_provider_max_concurrency,omitempty"`
}

// AgentSkillPhase is a closed stage grouping. It is intentionally separate
// from RuleDefinition.Kind: skill packs extend agent reasoning, while rule
// packs remain the governed finding/promotion taxonomy.
type AgentSkillPhase string

const (
	AgentSkillPhaseGrouping     AgentSkillPhase = "grouping"
	AgentSkillPhaseContext      AgentSkillPhase = "context"
	AgentSkillPhaseReview       AgentSkillPhase = "review"
	AgentSkillPhaseVerification AgentSkillPhase = "verification"
)

type AgentSkillPackDefinition struct {
	ID    string          `json:"id"`
	Phase AgentSkillPhase `json:"phase"`
	Ref   VersionedRef    `json:"ref"`
}

type OrderedAgentSkillPackPatch struct {
	Upsert []AgentSkillPackDefinition `json:"upsert"`
	Remove []string                   `json:"remove"`
}

type AgentKnowledgePackDefinition struct {
	ID  string       `json:"id"`
	Ref VersionedRef `json:"ref"`
}

type OrderedAgentKnowledgePackPatch struct {
	Upsert []AgentKnowledgePackDefinition `json:"upsert"`
	Remove []string                       `json:"remove"`
}

type AgentModelEgress string

const AgentModelEgressProviderBrokerOnly AgentModelEgress = "provider_broker_only"

type AgentWorkspaceReads string

const AgentWorkspaceReadsFrozenInputOnly AgentWorkspaceReads = "frozen_input_only"

type AgentAuthorityPatch struct {
	ModelEgress        *AgentModelEgress    `json:"model_egress,omitempty"`
	ToolNetwork        *Permission          `json:"tool_network,omitempty"`
	WorkspaceReads     *AgentWorkspaceReads `json:"workspace_reads,omitempty"`
	WorkspaceWrites    *Permission          `json:"workspace_writes,omitempty"`
	RemoteWrites       *Permission          `json:"remote_writes,omitempty"`
	MaxDelegationDepth *int                 `json:"max_delegation_depth,omitempty"`
	Tools              *SetPatch            `json:"tools,omitempty"`
}

type AgentBudgetPatch struct {
	MaxFiles        *int   `json:"max_files,omitempty"`
	MaxGroups       *int   `json:"max_groups,omitempty"`
	MaxHypotheses   *int   `json:"max_hypotheses,omitempty"`
	MaxModelCalls   *int   `json:"max_model_calls,omitempty"`
	MaxToolCalls    *int   `json:"max_tool_calls,omitempty"`
	MaxTargetBytes  *int64 `json:"max_target_bytes,omitempty"`
	MaxGroupBytes   *int64 `json:"max_group_bytes,omitempty"`
	MaxOutputBytes  *int64 `json:"max_output_bytes,omitempty"`
	MaxOutputTokens *int64 `json:"max_output_tokens,omitempty"`
	MaxCostMicros   *int64 `json:"max_cost_micros,omitempty"`
	TimeoutMS       *int64 `json:"timeout_ms,omitempty"`
	MaxConcurrency  *int   `json:"max_concurrency,omitempty"`
}

// AgentReviewPatch is deliberately partial so increasingly specific scopes
// may override one frozen component. Once any agent_review field is present,
// Resolve requires the effective policy to be fully closed.
type AgentReviewPatch struct {
	Agent           *VersionedRef                   `json:"agent,omitempty"`
	Normalization   *VersionedRef                   `json:"normalization,omitempty"`
	Provider        *VersionedRef                   `json:"provider,omitempty"`
	Model           *VersionedRef                   `json:"model,omitempty"`
	Prompt          *VersionedRef                   `json:"prompt,omitempty"`
	ModelCredential *SecretRef                      `json:"model_credential_ref,omitempty"`
	SkillPacks      *OrderedAgentSkillPackPatch     `json:"skill_packs,omitempty"`
	KnowledgePacks  *OrderedAgentKnowledgePackPatch `json:"knowledge_packs,omitempty"`
	APIProtocol     *VersionedRef                   `json:"api_protocol,omitempty"`
	Authority       *AgentAuthorityPatch            `json:"authority,omitempty"`
	Budget          *AgentBudgetPatch               `json:"budget,omitempty"`
}

type BudgetPatch struct {
	MaxInputBytes  *int64 `json:"max_input_bytes,omitempty"`
	MaxOutputBytes *int64 `json:"max_output_bytes,omitempty"`
	MaxTokens      *int64 `json:"max_tokens,omitempty"`
	MaxCostMicros  *int64 `json:"max_cost_micros,omitempty"`
	StageTimeoutMS *int64 `json:"stage_timeout_ms,omitempty"`
	MaxAttempts    *int   `json:"max_attempts,omitempty"`
	MaxConcurrency *int   `json:"max_concurrency,omitempty"`
}

type VerificationPatch struct {
	RequiredEvidenceKinds      *SetPatch   `json:"required_evidence_kinds,omitempty"`
	MinimumIndependentEvidence *int        `json:"minimum_independent_evidence,omitempty"`
	PartialEvidence            *Permission `json:"partial_evidence,omitempty"`
}

type AdjudicationPatch struct {
	MinimumSeverity         *string     `json:"minimum_severity,omitempty"`
	HumanReviewInconclusive *bool       `json:"human_review_inconclusive,omitempty"`
	AutomaticPublication    *Permission `json:"automatic_publication,omitempty"`
}

// CalibrationPoint is one monotonic knot in a deterministic integer-only
// piecewise-linear calibration profile.
type CalibrationPoint struct {
	RawPPM        uint32 `json:"raw_ppm"`
	ConfidencePPM uint32 `json:"confidence_ppm"`
}

type CalibrationProfile struct {
	SchemaVersion string             `json:"schema_version"`
	ID            string             `json:"id"`
	Revision      string             `json:"revision"`
	Points        []CalibrationPoint `json:"points"`
	SHA256        string             `json:"sha256"`
}

// FindingGovernancePatch is partial across scopes. If any field resolves, the
// effective policy must contain all three fields before a bundle can seal.
type FindingGovernancePatch struct {
	CalibrationProfile   *CalibrationProfile `json:"calibration_profile,omitempty"`
	MinimumConfidencePPM *uint32             `json:"minimum_confidence_ppm,omitempty"`
	MaxFindings          *int                `json:"max_findings,omitempty"`
}

type PublicationPatch struct {
	RemoteWrites *Permission `json:"remote_writes,omitempty"`
	Channels     *SetPatch   `json:"channels,omitempty"`
	MaxComments  *int        `json:"max_comments,omitempty"`
}

type RedactionLevel string

const (
	RedactionNone   RedactionLevel = "none"
	RedactionBasic  RedactionLevel = "basic"
	RedactionStrict RedactionLevel = "strict"
)

type DataPatch struct {
	RetentionDays *int64          `json:"retention_days,omitempty"`
	Redaction     *RedactionLevel `json:"redaction,omitempty"`
	Training      *Permission     `json:"training,omitempty"`
	Export        *Permission     `json:"export,omitempty"`
}

type TargetPolicy struct {
	AllowedModes  []string `json:"allowed_modes"`
	Include       []string `json:"include"`
	Exclude       []string `json:"exclude"`
	MaxFiles      int      `json:"max_files"`
	MaxPatchBytes int64    `json:"max_patch_bytes"`
}

type WorkflowPolicy struct {
	Definition VersionedRef `json:"definition"`
}

type ExecutionPolicy struct {
	AgentProfile                  VersionedRef                `json:"agent_profile"`
	ModelProfile                  VersionedRef                `json:"model_profile"`
	ModelCredential               *SecretRef                  `json:"model_credential_ref,omitempty"`
	AllowedTools                  []string                    `json:"allowed_tools"`
	ContextProviders              []ContextProviderDefinition `json:"context_providers"`
	ContextProviderMaxConcurrency int                         `json:"context_provider_max_concurrency"`
}

type AgentAuthority struct {
	ModelEgress        AgentModelEgress    `json:"model_egress"`
	ToolNetwork        Permission          `json:"tool_network"`
	WorkspaceReads     AgentWorkspaceReads `json:"workspace_reads"`
	WorkspaceWrites    Permission          `json:"workspace_writes"`
	RemoteWrites       Permission          `json:"remote_writes"`
	MaxDelegationDepth int                 `json:"max_delegation_depth"`
	Tools              []string            `json:"tools"`
}

type AgentBudget struct {
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

type AgentReviewPolicy struct {
	SchemaVersion   string                         `json:"schema_version"`
	ID              string                         `json:"id"`
	Revision        string                         `json:"revision"`
	Agent           VersionedRef                   `json:"agent"`
	Normalization   VersionedRef                   `json:"normalization"`
	Provider        VersionedRef                   `json:"provider"`
	Model           VersionedRef                   `json:"model"`
	Prompt          VersionedRef                   `json:"prompt"`
	ModelCredential *SecretRef                     `json:"model_credential_ref,omitempty"`
	SkillPacks      []AgentSkillPackDefinition     `json:"skill_packs"`
	KnowledgePacks  []AgentKnowledgePackDefinition `json:"knowledge_packs"`
	APIProtocol     VersionedRef                   `json:"api_protocol"`
	Authority       AgentAuthority                 `json:"authority"`
	Budget          AgentBudget                    `json:"budget"`
	SHA256          string                         `json:"sha256"`
}

type BudgetPolicy struct {
	MaxInputBytes  int64 `json:"max_input_bytes"`
	MaxOutputBytes int64 `json:"max_output_bytes"`
	MaxTokens      int64 `json:"max_tokens"`
	MaxCostMicros  int64 `json:"max_cost_micros"`
	StageTimeoutMS int64 `json:"stage_timeout_ms"`
	MaxAttempts    int   `json:"max_attempts"`
	MaxConcurrency int   `json:"max_concurrency"`
}

type VerificationPolicy struct {
	RequiredEvidenceKinds      []string   `json:"required_evidence_kinds"`
	MinimumIndependentEvidence int        `json:"minimum_independent_evidence"`
	PartialEvidence            Permission `json:"partial_evidence"`
}

type AdjudicationPolicy struct {
	MinimumSeverity         string     `json:"minimum_severity"`
	HumanReviewInconclusive bool       `json:"human_review_inconclusive"`
	AutomaticPublication    Permission `json:"automatic_publication"`
}

type FindingGovernancePolicy struct {
	SchemaVersion        string             `json:"schema_version"`
	ID                   string             `json:"id"`
	Revision             string             `json:"revision"`
	CalibrationProfile   CalibrationProfile `json:"calibration_profile"`
	MinimumConfidencePPM uint32             `json:"minimum_confidence_ppm"`
	MaxFindings          int                `json:"max_findings"`
	SHA256               string             `json:"sha256"`
}

type PublicationPolicy struct {
	RemoteWrites Permission `json:"remote_writes"`
	Channels     []string   `json:"channels"`
	MaxComments  int        `json:"max_comments"`
}

type DataPolicy struct {
	RetentionDays int64          `json:"retention_days"`
	Redaction     RedactionLevel `json:"redaction"`
	Training      Permission     `json:"training"`
	Export        Permission     `json:"export"`
}

type SourceRef struct {
	Scope     Scope  `json:"scope"`
	Selector  string `json:"selector"`
	ID        string `json:"id"`
	Revision  string `json:"revision"`
	Operation string `json:"operation"`
}

type FieldSource struct {
	Field    string        `json:"field"`
	Strategy MergeStrategy `json:"strategy"`
	Sources  []SourceRef   `json:"sources"`
}

type MergeExplanation struct {
	Field    string        `json:"field"`
	Strategy MergeStrategy `json:"strategy"`
	Source   SourceRef     `json:"source"`
	Action   string        `json:"action"`
	Detail   string        `json:"detail"`
}

type ConfigBundle struct {
	SchemaVersion     string                   `json:"schema_version"`
	BundleID          string                   `json:"bundle_id"`
	SHA256            string                   `json:"sha256"`
	Context           ResolutionContext        `json:"context"`
	AppliedRevisions  []SourceRef              `json:"applied_revisions"`
	Target            TargetPolicy             `json:"target"`
	RulePack          RulePack                 `json:"rule_pack"`
	Workflow          WorkflowPolicy           `json:"workflow"`
	Execution         ExecutionPolicy          `json:"execution"`
	AgentReview       *AgentReviewPolicy       `json:"agent_review,omitempty"`
	Budget            BudgetPolicy             `json:"budget"`
	Verification      VerificationPolicy       `json:"verification"`
	Adjudication      AdjudicationPolicy       `json:"adjudication"`
	FindingGovernance *FindingGovernancePolicy `json:"finding_governance,omitempty"`
	Publication       PublicationPolicy        `json:"publication"`
	Data              DataPolicy               `json:"data"`
	FieldSources      []FieldSource            `json:"field_sources"`
	Explain           []MergeExplanation       `json:"explain"`
}

const (
	RevisionSchemaVersion                = "argus.config_revision.v1alpha1"
	RulePackSchemaVersion                = "argus.rule_pack.v1alpha1"
	AgentReviewPolicySchemaVersion       = "argus.agent_review_policy.v1alpha1"
	CalibrationProfileSchemaVersion      = "argus.calibration_profile.v1alpha1"
	FindingGovernancePolicySchemaVersion = "argus.finding_governance_policy.v1alpha1"
	BundleSchemaVersion                  = "argus.config_bundle.v1alpha1"
)
