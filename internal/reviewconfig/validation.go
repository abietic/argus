package reviewconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var fieldStrategies = map[string]MergeStrategy{
	"target.allowed_modes":                        MergeSetUnionSubtract,
	"target.include":                              MergeSetUnionSubtract,
	"target.exclude":                              MergeSetUnionSubtract,
	"target.max_files":                            MergeOverride,
	"target.max_patch_bytes":                      MergeOverride,
	"rule_pack.rules":                             MergeOrderedStableIDOverride,
	"workflow.definition":                         MergeReplaceOnly,
	"execution.agent_profile":                     MergeReplaceOnly,
	"execution.model_profile":                     MergeReplaceOnly,
	"execution.model_credential_ref":              MergeReplaceOnly,
	"execution.allowed_tools":                     MergeSetUnionSubtract,
	"execution.context_providers":                 MergeOrderedStableIDOverride,
	"execution.context_provider_max_concurrency":  MergeOverride,
	"agent_review.agent":                          MergeReplaceOnly,
	"agent_review.normalization":                  MergeReplaceOnly,
	"agent_review.provider":                       MergeReplaceOnly,
	"agent_review.model":                          MergeReplaceOnly,
	"agent_review.prompt":                         MergeReplaceOnly,
	"agent_review.model_credential_ref":           MergeReplaceOnly,
	"agent_review.skill_packs":                    MergeOrderedStableIDOverride,
	"agent_review.knowledge_packs":                MergeOrderedStableIDOverride,
	"agent_review.api_protocol":                   MergeReplaceOnly,
	"agent_review.authority.model_egress":         MergeReplaceOnly,
	"agent_review.authority.tool_network":         MergeDenyWins,
	"agent_review.authority.workspace_reads":      MergeReplaceOnly,
	"agent_review.authority.workspace_writes":     MergeDenyWins,
	"agent_review.authority.remote_writes":        MergeDenyWins,
	"agent_review.authority.max_delegation_depth": MergeOverride,
	"agent_review.authority.tools":                MergeSetUnionSubtract,
	"agent_review.budget.max_files":               MergeOverride,
	"agent_review.budget.max_groups":              MergeOverride,
	"agent_review.budget.max_hypotheses":          MergeOverride,
	"agent_review.budget.max_model_calls":         MergeOverride,
	"agent_review.budget.max_tool_calls":          MergeOverride,
	"agent_review.budget.max_target_bytes":        MergeOverride,
	"agent_review.budget.max_group_bytes":         MergeOverride,
	"agent_review.budget.max_output_bytes":        MergeOverride,
	"agent_review.budget.max_output_tokens":       MergeOverride,
	"agent_review.budget.max_cost_micros":         MergeOverride,
	"agent_review.budget.timeout_ms":              MergeOverride,
	"agent_review.budget.max_concurrency":         MergeOverride,
	"budget.max_input_bytes":                      MergeOverride,
	"budget.max_output_bytes":                     MergeOverride,
	"budget.max_tokens":                           MergeOverride,
	"budget.max_cost_micros":                      MergeOverride,
	"budget.stage_timeout_ms":                     MergeOverride,
	"budget.max_attempts":                         MergeOverride,
	"budget.max_concurrency":                      MergeOverride,
	"verification.required_evidence_kinds":        MergeSetUnionSubtract,
	"verification.minimum_independent_evidence":   MergeOverride,
	"verification.partial_evidence":               MergeDenyWins,
	"adjudication.minimum_severity":               MergeOverride,
	"adjudication.human_review_inconclusive":      MergeOverride,
	"adjudication.automatic_publication":          MergeDenyWins,
	"finding_governance.calibration_profile":      MergeReplaceOnly,
	"finding_governance.minimum_confidence_ppm":   MergeOverride,
	"finding_governance.max_findings":             MergeOverride,
	"publication.remote_writes":                   MergeDenyWins,
	"publication.channels":                        MergeSetUnionSubtract,
	"publication.max_comments":                    MergeOverride,
	"data.retention_days":                         MergeOverride,
	"data.redaction":                              MergeDenyWins,
	"data.training":                               MergeDenyWins,
	"data.export":                                 MergeDenyWins,
}

// FieldMergeStrategies returns the closed schema of configurable fields. A
// field absent from this map cannot be resolved.
func FieldMergeStrategies() map[string]MergeStrategy {
	copy := make(map[string]MergeStrategy, len(fieldStrategies))
	for field, strategy := range fieldStrategies {
		copy[field] = strategy
	}
	return copy
}

func DecodeRevision(data []byte) (Revision, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return Revision{}, err
	}
	if err := rejectJSONNulls(data); err != nil {
		return Revision{}, err
	}
	var revision Revision
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&revision); err != nil {
		return Revision{}, fmt.Errorf("decode config revision: %w", err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return Revision{}, err
	}
	if err := revision.Validate(); err != nil {
		return Revision{}, err
	}
	return revision, nil
}

func DecodeBundle(data []byte) (ConfigBundle, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return ConfigBundle{}, err
	}
	if err := rejectJSONNulls(data); err != nil {
		return ConfigBundle{}, err
	}
	var bundle ConfigBundle
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return ConfigBundle{}, fmt.Errorf("decode config bundle: %w", err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return ConfigBundle{}, err
	}
	if err := bundle.Validate(); err != nil {
		return ConfigBundle{}, err
	}
	return bundle, nil
}

// DecodeFindingGovernancePolicy strictly decodes one already sealed effective
// policy. Replay callers must supply the complete content-addressed policy;
// partial lifecycle patches are intentionally not accepted here.
func DecodeFindingGovernancePolicy(data []byte) (FindingGovernancePolicy, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return FindingGovernancePolicy{}, err
	}
	if err := rejectJSONNulls(data); err != nil {
		return FindingGovernancePolicy{}, err
	}
	var policy FindingGovernancePolicy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return FindingGovernancePolicy{}, fmt.Errorf("decode finding governance policy: %w", err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return FindingGovernancePolicy{}, err
	}
	if err := policy.Validate(); err != nil {
		return FindingGovernancePolicy{}, err
	}
	return policy, nil
}

func DecodeRulePack(data []byte) (RulePack, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return RulePack{}, err
	}
	if err := rejectJSONNulls(data); err != nil {
		return RulePack{}, err
	}
	var pack RulePack
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pack); err != nil {
		return RulePack{}, fmt.Errorf("decode rule pack: %w", err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return RulePack{}, err
	}
	if err := pack.Validate(); err != nil {
		return RulePack{}, err
	}
	return pack, nil
}

func DigestRevision(revision Revision) (string, error) {
	if err := revision.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(revision)
	if err != nil {
		return "", fmt.Errorf("marshal config revision: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func SealRulePack(id string, revision string, rules []RuleDefinition) (RulePack, error) {
	pack := RulePack{
		SchemaVersion: RulePackSchemaVersion,
		ID:            id,
		Revision:      revision,
		Rules:         cloneRules(rules),
	}
	if err := pack.validateContent(); err != nil {
		return RulePack{}, err
	}
	digest, err := DigestRulePack(pack)
	if err != nil {
		return RulePack{}, err
	}
	pack.SHA256 = digest
	return pack, nil
}

func (context ResolutionContext) Validate() error {
	for name, value := range map[string]string{
		"tenant_id":       context.TenantID,
		"organization_id": context.OrganizationID,
		"repository_id":   context.RepositoryID,
		"invocation_id":   context.InvocationID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if context.Path != "" {
		if err := validateRepositoryPath("path", context.Path); err != nil {
			return err
		}
	}
	return nil
}

func (revision Revision) Validate() error {
	if revision.SchemaVersion != RevisionSchemaVersion {
		return fmt.Errorf("unsupported config revision schema %q", revision.SchemaVersion)
	}
	if err := validateID("id", revision.ID); err != nil {
		return err
	}
	if err := validateID("revision", revision.Revision); err != nil {
		return err
	}
	if err := revision.Selector.validate(revision.Scope); err != nil {
		return err
	}
	return revision.Patch.validate()
}

func (revision Revision) Applicable(context ResolutionContext) (bool, error) {
	if err := revision.Validate(); err != nil {
		return false, err
	}
	if err := context.Validate(); err != nil {
		return false, err
	}
	selector := revision.Selector
	switch revision.Scope {
	case ScopePlatform:
		return true, nil
	case ScopeTenant:
		return selector.TenantID == context.TenantID, nil
	case ScopeOrganization:
		return selector.TenantID == context.TenantID &&
			selector.OrganizationID == context.OrganizationID, nil
	case ScopeRepository:
		return selector.TenantID == context.TenantID &&
			selector.OrganizationID == context.OrganizationID &&
			selector.RepositoryID == context.RepositoryID, nil
	case ScopePath:
		return selector.TenantID == context.TenantID &&
			selector.OrganizationID == context.OrganizationID &&
			selector.RepositoryID == context.RepositoryID &&
			pathPrefixMatches(selector.PathPrefix, context.Path), nil
	case ScopeInvocation:
		return selector.TenantID == context.TenantID &&
			selector.OrganizationID == context.OrganizationID &&
			selector.RepositoryID == context.RepositoryID &&
			selector.InvocationID == context.InvocationID, nil
	default:
		return false, fmt.Errorf("unsupported scope %q", revision.Scope)
	}
}

func (revision Revision) Specificity() (Specificity, error) {
	if err := revision.Validate(); err != nil {
		return Specificity{}, err
	}
	specificity := Specificity{ScopeRank: scopeRank(revision.Scope)}
	if revision.Scope == ScopePath {
		specificity.PathDepth = strings.Count(revision.Selector.PathPrefix, "/") + 1
	}
	return specificity, nil
}

func (selector Selector) validate(scope Scope) error {
	required := func(name, value string) error {
		if value == "" {
			return fmt.Errorf("%s selector requires %s", scope, name)
		}
		return validateID("selector."+name, value)
	}
	reject := func(name, value string) error {
		if value != "" {
			return fmt.Errorf("%s selector must not contain %s", scope, name)
		}
		return nil
	}
	switch scope {
	case ScopePlatform:
		for name, value := range selectorValues(selector) {
			if err := reject(name, value); err != nil {
				return err
			}
		}
	case ScopeTenant:
		if err := required("tenant_id", selector.TenantID); err != nil {
			return err
		}
		for name, value := range map[string]string{
			"organization_id": selector.OrganizationID,
			"repository_id":   selector.RepositoryID,
			"path_prefix":     selector.PathPrefix,
			"invocation_id":   selector.InvocationID,
		} {
			if err := reject(name, value); err != nil {
				return err
			}
		}
	case ScopeOrganization:
		if err := required("tenant_id", selector.TenantID); err != nil {
			return err
		}
		if err := required("organization_id", selector.OrganizationID); err != nil {
			return err
		}
		for name, value := range map[string]string{
			"repository_id": selector.RepositoryID,
			"path_prefix":   selector.PathPrefix,
			"invocation_id": selector.InvocationID,
		} {
			if err := reject(name, value); err != nil {
				return err
			}
		}
	case ScopeRepository:
		if err := validateSelectorAncestry(selector, false, false); err != nil {
			return err
		}
	case ScopePath:
		if err := validateSelectorAncestry(selector, true, false); err != nil {
			return err
		}
		if err := validatePathPrefix("selector.path_prefix", selector.PathPrefix); err != nil {
			return err
		}
	case ScopeInvocation:
		if err := validateSelectorAncestry(selector, false, true); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported scope %q", scope)
	}
	return nil
}

func validateSelectorAncestry(selector Selector, pathRequired, invocationRequired bool) error {
	for name, value := range map[string]string{
		"tenant_id":       selector.TenantID,
		"organization_id": selector.OrganizationID,
		"repository_id":   selector.RepositoryID,
	} {
		if value == "" {
			return fmt.Errorf("selector requires %s", name)
		}
		if err := validateID("selector."+name, value); err != nil {
			return err
		}
	}
	if pathRequired {
		if selector.PathPrefix == "" {
			return fmt.Errorf("path selector requires path_prefix")
		}
	} else if selector.PathPrefix != "" {
		return fmt.Errorf("selector must not contain path_prefix")
	}
	if invocationRequired {
		if err := validateID("selector.invocation_id", selector.InvocationID); err != nil {
			return err
		}
	} else if selector.InvocationID != "" {
		return fmt.Errorf("selector must not contain invocation_id")
	}
	return nil
}

func selectorValues(selector Selector) map[string]string {
	return map[string]string{
		"tenant_id":       selector.TenantID,
		"organization_id": selector.OrganizationID,
		"repository_id":   selector.RepositoryID,
		"path_prefix":     selector.PathPrefix,
		"invocation_id":   selector.InvocationID,
	}
}

func (patch ConfigPatch) validate() error {
	sections := 0
	if patch.Target != nil {
		sections++
		if err := patch.Target.validate(); err != nil {
			return err
		}
	}
	if patch.RulePack != nil {
		sections++
		if err := patch.RulePack.validate(); err != nil {
			return err
		}
	}
	if patch.Workflow != nil {
		sections++
		if err := patch.Workflow.validate(); err != nil {
			return err
		}
	}
	if patch.Execution != nil {
		sections++
		if err := patch.Execution.validate(); err != nil {
			return err
		}
	}
	if patch.AgentReview != nil {
		sections++
		if err := patch.AgentReview.validate(); err != nil {
			return err
		}
	}
	if patch.Budget != nil {
		sections++
		if err := patch.Budget.validate(); err != nil {
			return err
		}
	}
	if patch.Verification != nil {
		sections++
		if err := patch.Verification.validate(); err != nil {
			return err
		}
	}
	if patch.Adjudication != nil {
		sections++
		if err := patch.Adjudication.validate(); err != nil {
			return err
		}
	}
	if patch.FindingGovernance != nil {
		sections++
		if err := patch.FindingGovernance.validate(); err != nil {
			return err
		}
	}
	if patch.Publication != nil {
		sections++
		if err := patch.Publication.validate(); err != nil {
			return err
		}
	}
	if patch.Data != nil {
		sections++
		if err := patch.Data.validate(); err != nil {
			return err
		}
	}
	if sections == 0 {
		return fmt.Errorf("config patch must change at least one typed section")
	}
	return nil
}

func (patch TargetPatch) validate() error {
	fields := 0
	if patch.AllowedModes != nil {
		fields++
		if err := validateSetPatch("target.allowed_modes", *patch.AllowedModes, validateTargetMode); err != nil {
			return err
		}
	}
	if patch.Include != nil {
		fields++
		if err := validateSetPatch("target.include", *patch.Include, validatePattern); err != nil {
			return err
		}
	}
	if patch.Exclude != nil {
		fields++
		if err := validateSetPatch("target.exclude", *patch.Exclude, validatePattern); err != nil {
			return err
		}
	}
	if patch.MaxFiles != nil {
		fields++
		if *patch.MaxFiles <= 0 {
			return fmt.Errorf("target.max_files must be positive")
		}
	}
	if patch.MaxPatchBytes != nil {
		fields++
		if *patch.MaxPatchBytes <= 0 {
			return fmt.Errorf("target.max_patch_bytes must be positive")
		}
	}
	return requireFields("target", fields)
}

func (patch RulePackPatch) validate() error {
	if patch.Rules == nil {
		return fmt.Errorf("rule_pack patch requires rules")
	}
	if patch.Rules.Upsert == nil || patch.Rules.Remove == nil {
		return fmt.Errorf("rule_pack.rules upsert and remove must be explicit arrays")
	}
	seen := make(map[string]struct{}, len(patch.Rules.Upsert)+len(patch.Rules.Remove))
	for index, rule := range patch.Rules.Upsert {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("rule_pack.rules.upsert[%d]: %w", index, err)
		}
		if _, duplicate := seen[rule.ID]; duplicate {
			return fmt.Errorf("rule_pack.rules contains duplicate or conflicting id %q", rule.ID)
		}
		seen[rule.ID] = struct{}{}
	}
	for _, id := range patch.Rules.Remove {
		if err := validateID("rule_pack.rules.remove", id); err != nil {
			return err
		}
		if _, conflict := seen[id]; conflict {
			return fmt.Errorf("rule_pack.rules contains duplicate or conflicting id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (patch WorkflowPatch) validate() error {
	if patch.Definition == nil {
		return fmt.Errorf("workflow patch requires definition")
	}
	return patch.Definition.Validate()
}

func (patch ExecutionPatch) validate() error {
	fields := 0
	if patch.AgentProfile != nil {
		fields++
		if err := patch.AgentProfile.Validate(); err != nil {
			return fmt.Errorf("execution.agent_profile: %w", err)
		}
	}
	if patch.ModelProfile != nil {
		fields++
		if err := patch.ModelProfile.Validate(); err != nil {
			return fmt.Errorf("execution.model_profile: %w", err)
		}
	}
	if patch.ModelCredential != nil {
		fields++
		if err := patch.ModelCredential.Validate(); err != nil {
			return fmt.Errorf("execution.model_credential_ref: %w", err)
		}
	}
	if patch.AllowedTools != nil {
		fields++
		if err := validateSetPatch("execution.allowed_tools", *patch.AllowedTools, validateSimpleValue); err != nil {
			return err
		}
	}
	if patch.ContextProviders != nil {
		fields++
		if err := patch.ContextProviders.validate(); err != nil {
			return err
		}
	}
	if patch.ContextProviderMaxConcurrency != nil {
		fields++
		if *patch.ContextProviderMaxConcurrency <= 0 || *patch.ContextProviderMaxConcurrency > 16 {
			return fmt.Errorf("execution.context_provider_max_concurrency must be within 1..16")
		}
	}
	return requireFields("execution", fields)
}

func (patch OrderedContextProviderPatch) validate() error {
	if patch.Upsert == nil || patch.Remove == nil {
		return fmt.Errorf("execution.context_providers upsert and remove must be explicit arrays")
	}
	seen := make(map[string]struct{}, len(patch.Upsert)+len(patch.Remove))
	for index, provider := range patch.Upsert {
		if err := provider.Validate(); err != nil {
			return fmt.Errorf("execution.context_providers.upsert[%d]: %w", index, err)
		}
		if _, duplicate := seen[provider.ID]; duplicate {
			return fmt.Errorf("context providers contain duplicate or conflicting id %q", provider.ID)
		}
		seen[provider.ID] = struct{}{}
	}
	for _, id := range patch.Remove {
		if err := validateID("execution.context_providers.remove", id); err != nil {
			return err
		}
		if _, conflict := seen[id]; conflict {
			return fmt.Errorf("context providers contain duplicate or conflicting id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (patch AgentReviewPatch) validate() error {
	fields := 0
	for _, field := range []struct {
		name string
		ref  *VersionedRef
	}{
		{"agent", patch.Agent},
		{"normalization", patch.Normalization},
		{"provider", patch.Provider},
		{"model", patch.Model},
		{"prompt", patch.Prompt},
		{"api_protocol", patch.APIProtocol},
	} {
		if field.ref == nil {
			continue
		}
		fields++
		if err := field.ref.Validate(); err != nil {
			return fmt.Errorf("agent_review.%s: %w", field.name, err)
		}
	}
	if patch.ModelCredential != nil {
		fields++
		if err := patch.ModelCredential.Validate(); err != nil {
			return fmt.Errorf("agent_review.model_credential_ref: %w", err)
		}
	}
	if patch.SkillPacks != nil {
		fields++
		if err := patch.SkillPacks.validate(); err != nil {
			return err
		}
	}
	if patch.KnowledgePacks != nil {
		fields++
		if err := patch.KnowledgePacks.validate(); err != nil {
			return err
		}
	}
	if patch.Authority != nil {
		fields++
		if err := patch.Authority.validate(); err != nil {
			return err
		}
	}
	if patch.Budget != nil {
		fields++
		if err := patch.Budget.validate(); err != nil {
			return err
		}
	}
	return requireFields("agent_review", fields)
}

func (patch OrderedAgentSkillPackPatch) validate() error {
	if patch.Upsert == nil || patch.Remove == nil {
		return fmt.Errorf("agent_review.skill_packs upsert and remove must be explicit arrays")
	}
	seen := make(map[string]struct{}, len(patch.Upsert)+len(patch.Remove))
	for index, pack := range patch.Upsert {
		if err := pack.Validate(); err != nil {
			return fmt.Errorf("agent_review.skill_packs.upsert[%d]: %w", index, err)
		}
		if _, exists := seen[pack.ID]; exists {
			return fmt.Errorf("agent_review.skill_packs contains duplicate or conflicting id %q", pack.ID)
		}
		seen[pack.ID] = struct{}{}
	}
	for _, id := range patch.Remove {
		if err := validateID("agent_review.skill_packs.remove", id); err != nil {
			return err
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("agent_review.skill_packs contains duplicate or conflicting id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (patch OrderedAgentKnowledgePackPatch) validate() error {
	if patch.Upsert == nil || patch.Remove == nil {
		return fmt.Errorf("agent_review.knowledge_packs upsert and remove must be explicit arrays")
	}
	seen := make(map[string]struct{}, len(patch.Upsert)+len(patch.Remove))
	for index, pack := range patch.Upsert {
		if err := pack.Validate(); err != nil {
			return fmt.Errorf("agent_review.knowledge_packs.upsert[%d]: %w", index, err)
		}
		if _, exists := seen[pack.ID]; exists {
			return fmt.Errorf("agent_review.knowledge_packs contains duplicate or conflicting id %q", pack.ID)
		}
		seen[pack.ID] = struct{}{}
	}
	for _, id := range patch.Remove {
		if err := validateID("agent_review.knowledge_packs.remove", id); err != nil {
			return err
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("agent_review.knowledge_packs contains duplicate or conflicting id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (patch AgentAuthorityPatch) validate() error {
	fields := 0
	if patch.ModelEgress != nil {
		fields++
		if *patch.ModelEgress != AgentModelEgressProviderBrokerOnly {
			return fmt.Errorf("agent_review.authority.model_egress must be provider_broker_only")
		}
	}
	if patch.ToolNetwork != nil {
		fields++
		if *patch.ToolNetwork != PermissionDeny {
			return fmt.Errorf("agent_review.authority.tool_network must be deny")
		}
	}
	if patch.WorkspaceReads != nil {
		fields++
		if *patch.WorkspaceReads != AgentWorkspaceReadsFrozenInputOnly {
			return fmt.Errorf("agent_review.authority.workspace_reads must be frozen_input_only")
		}
	}
	if patch.WorkspaceWrites != nil {
		fields++
		if *patch.WorkspaceWrites != PermissionDeny {
			return fmt.Errorf("agent_review.authority.workspace_writes must be deny")
		}
	}
	if patch.RemoteWrites != nil {
		fields++
		if *patch.RemoteWrites != PermissionDeny {
			return fmt.Errorf("agent_review.authority.remote_writes must be deny")
		}
	}
	if patch.MaxDelegationDepth != nil {
		fields++
		if *patch.MaxDelegationDepth != 0 {
			return fmt.Errorf("agent_review.authority.max_delegation_depth must be zero")
		}
	}
	if patch.Tools != nil {
		fields++
		if err := validateSetPatch(
			"agent_review.authority.tools",
			*patch.Tools,
			validateSimpleValue,
		); err != nil {
			return err
		}
	}
	return requireFields("agent_review.authority", fields)
}

func (patch AgentBudgetPatch) validate() error {
	fields := 0
	for name, value := range map[string]*int{
		"max_files":       patch.MaxFiles,
		"max_groups":      patch.MaxGroups,
		"max_hypotheses":  patch.MaxHypotheses,
		"max_model_calls": patch.MaxModelCalls,
		"max_tool_calls":  patch.MaxToolCalls,
		"max_concurrency": patch.MaxConcurrency,
	} {
		if value != nil {
			fields++
			if *value <= 0 {
				return fmt.Errorf("agent_review.budget.%s must be positive", name)
			}
		}
	}
	for name, value := range map[string]*int64{
		"max_target_bytes":  patch.MaxTargetBytes,
		"max_group_bytes":   patch.MaxGroupBytes,
		"max_output_bytes":  patch.MaxOutputBytes,
		"max_output_tokens": patch.MaxOutputTokens,
		"max_cost_micros":   patch.MaxCostMicros,
		"timeout_ms":        patch.TimeoutMS,
	} {
		if value != nil {
			fields++
			if *value <= 0 {
				return fmt.Errorf("agent_review.budget.%s must be positive", name)
			}
		}
	}
	return requireFields("agent_review.budget", fields)
}

func (patch BudgetPatch) validate() error {
	fields := 0
	for name, value := range map[string]*int64{
		"max_input_bytes":  patch.MaxInputBytes,
		"max_output_bytes": patch.MaxOutputBytes,
		"max_tokens":       patch.MaxTokens,
		"max_cost_micros":  patch.MaxCostMicros,
		"stage_timeout_ms": patch.StageTimeoutMS,
	} {
		if value != nil {
			fields++
			if *value <= 0 {
				return fmt.Errorf("budget.%s must be positive", name)
			}
		}
	}
	if patch.MaxConcurrency != nil {
		fields++
		if *patch.MaxConcurrency <= 0 {
			return fmt.Errorf("budget.max_concurrency must be positive")
		}
	}
	if patch.MaxAttempts != nil {
		fields++
		if *patch.MaxAttempts <= 0 {
			return fmt.Errorf("budget.max_attempts must be positive")
		}
	}
	return requireFields("budget", fields)
}

func (patch VerificationPatch) validate() error {
	fields := 0
	if patch.RequiredEvidenceKinds != nil {
		fields++
		if err := validateSetPatch(
			"verification.required_evidence_kinds",
			*patch.RequiredEvidenceKinds,
			validateSimpleValue,
		); err != nil {
			return err
		}
	}
	if patch.MinimumIndependentEvidence != nil {
		fields++
		if *patch.MinimumIndependentEvidence <= 0 {
			return fmt.Errorf("verification.minimum_independent_evidence must be positive")
		}
	}
	if patch.PartialEvidence != nil {
		fields++
		if err := patch.PartialEvidence.Validate(); err != nil {
			return fmt.Errorf("verification.partial_evidence: %w", err)
		}
	}
	return requireFields("verification", fields)
}

func (patch AdjudicationPatch) validate() error {
	fields := 0
	if patch.MinimumSeverity != nil {
		fields++
		if err := validateSeverity(*patch.MinimumSeverity); err != nil {
			return fmt.Errorf("adjudication.minimum_severity: %w", err)
		}
	}
	if patch.HumanReviewInconclusive != nil {
		fields++
	}
	if patch.AutomaticPublication != nil {
		fields++
		if err := patch.AutomaticPublication.Validate(); err != nil {
			return fmt.Errorf("adjudication.automatic_publication: %w", err)
		}
	}
	return requireFields("adjudication", fields)
}

func (patch FindingGovernancePatch) validate() error {
	fields := 0
	if patch.CalibrationProfile != nil {
		fields++
		if err := patch.CalibrationProfile.Validate(); err != nil {
			return fmt.Errorf("finding_governance.calibration_profile: %w", err)
		}
	}
	if patch.MinimumConfidencePPM != nil {
		fields++
		if *patch.MinimumConfidencePPM > 1_000_000 {
			return fmt.Errorf("finding_governance.minimum_confidence_ppm exceeds ppm scale")
		}
	}
	if patch.MaxFindings != nil {
		fields++
		if *patch.MaxFindings < 0 {
			return fmt.Errorf("finding_governance.max_findings must not be negative")
		}
	}
	return requireFields("finding_governance", fields)
}

func (profile CalibrationProfile) Validate() error {
	if profile.SchemaVersion != CalibrationProfileSchemaVersion {
		return fmt.Errorf("unsupported calibration profile schema %q", profile.SchemaVersion)
	}
	if err := validateID("calibration_profile.id", profile.ID); err != nil {
		return err
	}
	if err := validateID("calibration_profile.revision", profile.Revision); err != nil {
		return err
	}
	if len(profile.Points) < 2 {
		return fmt.Errorf("calibration_profile.points requires at least two points")
	}
	var previousRaw, previousConfidence uint32
	for index, point := range profile.Points {
		if point.RawPPM > 1_000_000 || point.ConfidencePPM > 1_000_000 {
			return fmt.Errorf("calibration_profile.points[%d] exceeds ppm scale", index)
		}
		if index > 0 && (point.RawPPM <= previousRaw || point.ConfidencePPM < previousConfidence) {
			return fmt.Errorf("calibration_profile.points must be strictly ordered by raw_ppm and monotonic by confidence_ppm")
		}
		previousRaw, previousConfidence = point.RawPPM, point.ConfidencePPM
	}
	if profile.Points[0].RawPPM != 0 || profile.Points[len(profile.Points)-1].RawPPM != 1_000_000 {
		return fmt.Errorf("calibration_profile.points must cover raw ppm endpoints")
	}
	digest, err := DigestCalibrationProfile(profile)
	if err != nil {
		return err
	}
	if profile.SHA256 != digest {
		return fmt.Errorf("calibration profile digest does not match its content")
	}
	return nil
}

func (policy FindingGovernancePolicy) Validate() error {
	if policy.SchemaVersion != FindingGovernancePolicySchemaVersion ||
		policy.ID != "effective" || policy.Revision != "resolved" {
		return fmt.Errorf("finding governance policy must use effective@resolved identity")
	}
	if err := policy.CalibrationProfile.Validate(); err != nil {
		return err
	}
	if policy.MinimumConfidencePPM > 1_000_000 || policy.MaxFindings < 0 {
		return fmt.Errorf("finding governance policy bounds are invalid")
	}
	digest, err := DigestFindingGovernancePolicy(policy)
	if err != nil {
		return err
	}
	if policy.SHA256 != digest {
		return fmt.Errorf("finding governance policy digest does not match its content")
	}
	return nil
}

func (patch PublicationPatch) validate() error {
	fields := 0
	if patch.RemoteWrites != nil {
		fields++
		if err := patch.RemoteWrites.Validate(); err != nil {
			return fmt.Errorf("publication.remote_writes: %w", err)
		}
	}
	if patch.Channels != nil {
		fields++
		if err := validateSetPatch("publication.channels", *patch.Channels, validateSimpleValue); err != nil {
			return err
		}
	}
	if patch.MaxComments != nil {
		fields++
		if *patch.MaxComments < 0 {
			return fmt.Errorf("publication.max_comments must not be negative")
		}
	}
	return requireFields("publication", fields)
}

func (patch DataPatch) validate() error {
	fields := 0
	if patch.RetentionDays != nil {
		fields++
		if *patch.RetentionDays <= 0 {
			return fmt.Errorf("data.retention_days must be positive")
		}
	}
	if patch.Redaction != nil {
		fields++
		if err := patch.Redaction.Validate(); err != nil {
			return err
		}
	}
	if patch.Training != nil {
		fields++
		if err := patch.Training.Validate(); err != nil {
			return fmt.Errorf("data.training: %w", err)
		}
	}
	if patch.Export != nil {
		fields++
		if err := patch.Export.Validate(); err != nil {
			return fmt.Errorf("data.export: %w", err)
		}
	}
	return requireFields("data", fields)
}

func (ref VersionedRef) Validate() error {
	if err := validateID("id", ref.ID); err != nil {
		return err
	}
	if err := validateID("revision", ref.Revision); err != nil {
		return err
	}
	if strings.EqualFold(ref.ID, "latest") || strings.EqualFold(ref.Revision, "latest") {
		return fmt.Errorf("versioned ref must not use latest")
	}
	return validateSHA256("sha256", ref.SHA256)
}

func (phase AgentSkillPhase) Validate() error {
	switch phase {
	case AgentSkillPhaseGrouping, AgentSkillPhaseContext, AgentSkillPhaseReview,
		AgentSkillPhaseVerification:
		return nil
	default:
		return fmt.Errorf("unsupported agent skill phase %q", phase)
	}
}

func (pack AgentSkillPackDefinition) Validate() error {
	if err := validateID("agent_skill_pack.id", pack.ID); err != nil {
		return err
	}
	if err := pack.Phase.Validate(); err != nil {
		return err
	}
	if err := pack.Ref.Validate(); err != nil {
		return fmt.Errorf("agent skill pack %q ref: %w", pack.ID, err)
	}
	return nil
}

func (pack AgentKnowledgePackDefinition) Validate() error {
	if err := validateID("agent_knowledge_pack.id", pack.ID); err != nil {
		return err
	}
	if err := pack.Ref.Validate(); err != nil {
		return fmt.Errorf("agent knowledge pack %q ref: %w", pack.ID, err)
	}
	return nil
}

func (ref SecretRef) Validate() error {
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return fmt.Errorf("parse secret ref: %w", err)
	}
	if parsed.Scheme != "secret" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" ||
		parsed.EscapedPath() != parsed.Path {
		return fmt.Errorf("secret must be a secret://authority/path reference")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("secret ref contains an unsafe path")
		}
	}
	return nil
}

func (rule RuleDefinition) Validate() error {
	if err := validateID("rule.id", rule.ID); err != nil {
		return err
	}
	if err := validateID("rule.revision", rule.Revision); err != nil {
		return err
	}
	switch rule.Kind {
	case "deterministic", "ast", "lsp", "compile", "test", "static",
		"search", "codegraph", "llm", "agent":
	default:
		return fmt.Errorf("rule %q has unsupported kind %q", rule.ID, rule.Kind)
	}
	if err := rule.Detector.Validate(); err != nil {
		return fmt.Errorf("rule %q detector: %w", rule.ID, err)
	}
	if err := validateSortedSet("rule.languages", rule.Languages, validateSimpleValue, false); err != nil {
		return err
	}
	if err := validateSortedSet("rule.path_prefixes", rule.PathPrefixes, validateOptionalPathPrefix, true); err != nil {
		return err
	}
	if err := validateSortedSet("rule.evidence_kinds", rule.EvidenceKinds, validateSimpleValue, false); err != nil {
		return err
	}
	return validateSeverity(rule.Severity)
}

func (provider ContextProviderDefinition) Validate() error {
	if err := validateID("context_provider.id", provider.ID); err != nil {
		return err
	}
	if err := validateID("context_provider.revision", provider.Revision); err != nil {
		return err
	}
	switch provider.Kind {
	case "repository_search", "codegraph", "lsp", "dependency", "artifact", "go_ast", "compile":
	default:
		return fmt.Errorf("context provider %q has unsupported kind %q", provider.ID, provider.Kind)
	}
	return provider.Adapter.Validate()
}

func (permission Permission) Validate() error {
	switch permission {
	case PermissionAllow, PermissionDeny:
		return nil
	default:
		return fmt.Errorf("unsupported permission %q", permission)
	}
}

func (level RedactionLevel) Validate() error {
	switch level {
	case RedactionNone, RedactionBasic, RedactionStrict:
		return nil
	default:
		return fmt.Errorf("unsupported redaction level %q", level)
	}
}

func (pack RulePack) Validate() error {
	if err := pack.validateContent(); err != nil {
		return err
	}
	digest, err := DigestRulePack(pack)
	if err != nil {
		return err
	}
	if pack.SHA256 != digest {
		return fmt.Errorf("rule pack digest does not match its content")
	}
	return nil
}

func (pack RulePack) validateContent() error {
	if pack.SchemaVersion != RulePackSchemaVersion {
		return fmt.Errorf("unsupported rule pack schema %q", pack.SchemaVersion)
	}
	if err := validateID("rule_pack.id", pack.ID); err != nil {
		return err
	}
	if err := validateID("rule_pack.revision", pack.Revision); err != nil {
		return err
	}
	if pack.Rules == nil || len(pack.Rules) == 0 {
		return fmt.Errorf("rule pack requires rules")
	}
	enabled := 0
	seen := make(map[string]struct{}, len(pack.Rules))
	for index, rule := range pack.Rules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("rules[%d]: %w", index, err)
		}
		if _, duplicate := seen[rule.ID]; duplicate {
			return fmt.Errorf("effective rule pack contains duplicate rule %q", rule.ID)
		}
		seen[rule.ID] = struct{}{}
		if rule.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return fmt.Errorf("rule pack requires at least one enabled rule")
	}
	return nil
}

func (policy AgentReviewPolicy) Validate() error {
	if err := policy.validateContent(); err != nil {
		return err
	}
	digest, err := DigestAgentReviewPolicy(policy)
	if err != nil {
		return err
	}
	if policy.SHA256 != digest {
		return fmt.Errorf("agent review policy digest does not match its content")
	}
	return nil
}

func (policy AgentReviewPolicy) validateContent() error {
	if policy.SchemaVersion != AgentReviewPolicySchemaVersion {
		return fmt.Errorf("unsupported agent review policy schema %q", policy.SchemaVersion)
	}
	if policy.ID != "effective" || policy.Revision != "resolved" {
		return fmt.Errorf("agent review policy must use effective@resolved identity")
	}
	for name, ref := range map[string]VersionedRef{
		"agent":         policy.Agent,
		"normalization": policy.Normalization,
		"provider":      policy.Provider,
		"model":         policy.Model,
		"prompt":        policy.Prompt,
		"api_protocol":  policy.APIProtocol,
	} {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("agent_review.%s: %w", name, err)
		}
	}
	if policy.Normalization.ID != "candidate-normalization" {
		return fmt.Errorf("agent_review.normalization.id must be candidate-normalization")
	}
	switch policy.Normalization.Revision {
	case "v0", "v1", "v2":
	default:
		return fmt.Errorf(
			"agent_review.normalization.revision has unsupported value %q",
			policy.Normalization.Revision,
		)
	}
	if policy.Normalization.SHA256 != policy.Agent.SHA256 {
		return fmt.Errorf("agent_review.normalization must bind the exact agent worker digest")
	}
	if policy.ModelCredential != nil {
		if err := policy.ModelCredential.Validate(); err != nil {
			return fmt.Errorf("agent_review.model_credential_ref: %w", err)
		}
	}
	if policy.SkillPacks == nil || len(policy.SkillPacks) == 0 {
		return fmt.Errorf("agent_review.skill_packs must be an explicit non-empty array")
	}
	skillIDs := make(map[string]struct{}, len(policy.SkillPacks))
	reviewPacks := 0
	for index, pack := range policy.SkillPacks {
		if err := pack.Validate(); err != nil {
			return fmt.Errorf("agent_review.skill_packs[%d]: %w", index, err)
		}
		if _, duplicate := skillIDs[pack.ID]; duplicate {
			return fmt.Errorf("agent_review.skill_packs contains duplicate %q", pack.ID)
		}
		skillIDs[pack.ID] = struct{}{}
		if pack.Phase == AgentSkillPhaseReview {
			reviewPacks++
		}
	}
	if reviewPacks == 0 {
		return fmt.Errorf("agent_review.skill_packs requires at least one review phase pack")
	}
	if policy.KnowledgePacks == nil {
		return fmt.Errorf("agent_review.knowledge_packs must be an explicit array")
	}
	knowledgeIDs := make(map[string]struct{}, len(policy.KnowledgePacks))
	for index, pack := range policy.KnowledgePacks {
		if err := pack.Validate(); err != nil {
			return fmt.Errorf("agent_review.knowledge_packs[%d]: %w", index, err)
		}
		if _, duplicate := knowledgeIDs[pack.ID]; duplicate {
			return fmt.Errorf("agent_review.knowledge_packs contains duplicate %q", pack.ID)
		}
		knowledgeIDs[pack.ID] = struct{}{}
	}
	if policy.Authority.ModelEgress != AgentModelEgressProviderBrokerOnly {
		return fmt.Errorf("agent_review.authority.model_egress must be provider_broker_only")
	}
	if policy.Authority.ToolNetwork != PermissionDeny {
		return fmt.Errorf("agent_review.authority.tool_network must be deny")
	}
	if policy.Authority.WorkspaceReads != AgentWorkspaceReadsFrozenInputOnly {
		return fmt.Errorf("agent_review.authority.workspace_reads must be frozen_input_only")
	}
	if policy.Authority.WorkspaceWrites != PermissionDeny {
		return fmt.Errorf("agent_review.authority.workspace_writes must be deny")
	}
	if policy.Authority.RemoteWrites != PermissionDeny {
		return fmt.Errorf("agent_review.authority.remote_writes must be deny")
	}
	if policy.Authority.MaxDelegationDepth != 0 {
		return fmt.Errorf("agent_review.authority.max_delegation_depth must be zero")
	}
	if err := validateSortedSet(
		"agent_review.authority.tools",
		policy.Authority.Tools,
		validateSimpleValue,
		true,
	); err != nil {
		return err
	}
	budget := policy.Budget
	if budget.MaxFiles <= 0 || budget.MaxGroups <= 0 || budget.MaxHypotheses <= 0 ||
		budget.MaxModelCalls <= 0 || budget.MaxToolCalls <= 0 ||
		budget.MaxTargetBytes <= 0 || budget.MaxGroupBytes <= 0 ||
		budget.MaxOutputBytes <= 0 || budget.MaxOutputTokens <= 0 ||
		budget.MaxCostMicros <= 0 || budget.TimeoutMS <= 0 ||
		budget.MaxConcurrency <= 0 {
		return fmt.Errorf("agent_review budget fields must be positive")
	}
	if budget.MaxGroupBytes > budget.MaxTargetBytes {
		return fmt.Errorf("agent_review.budget.max_group_bytes exceeds max_target_bytes")
	}
	return nil
}

func DigestRulePack(pack RulePack) (string, error) {
	copy := pack
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal rule pack: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func DigestAgentReviewPolicy(policy AgentReviewPolicy) (string, error) {
	copy := policy
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal agent review policy: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func DigestCalibrationProfile(profile CalibrationProfile) (string, error) {
	copy := profile
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal calibration profile: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func SealCalibrationProfile(id, revision string, points []CalibrationPoint) (CalibrationProfile, error) {
	profile := CalibrationProfile{
		SchemaVersion: CalibrationProfileSchemaVersion, ID: id, Revision: revision,
		Points: slices.Clone(points),
	}
	digest, err := DigestCalibrationProfile(profile)
	if err != nil {
		return CalibrationProfile{}, err
	}
	profile.SHA256 = digest
	if err := profile.Validate(); err != nil {
		return CalibrationProfile{}, err
	}
	return profile, nil
}

func DigestFindingGovernancePolicy(policy FindingGovernancePolicy) (string, error) {
	copy := policy
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal finding governance policy: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func SealFindingGovernancePolicy(
	profile CalibrationProfile,
	minimumConfidencePPM uint32,
	maxFindings int,
) (FindingGovernancePolicy, error) {
	policy := FindingGovernancePolicy{
		SchemaVersion: FindingGovernancePolicySchemaVersion,
		ID:            "effective", Revision: "resolved",
		CalibrationProfile:   profile,
		MinimumConfidencePPM: minimumConfidencePPM, MaxFindings: maxFindings,
	}
	digest, err := DigestFindingGovernancePolicy(policy)
	if err != nil {
		return FindingGovernancePolicy{}, err
	}
	policy.SHA256 = digest
	if err := policy.Validate(); err != nil {
		return FindingGovernancePolicy{}, err
	}
	return policy, nil
}

func DigestBundle(bundle ConfigBundle) (string, error) {
	copy := bundle
	copy.BundleID = ""
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal config bundle: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// DigestBundleArtifact hashes the canonical full JSON representation,
// including BundleID and the bundle's semantic SHA256. It is the digest used by
// ArtifactRef; DigestBundle remains the semantic, self-excluding identity.
func DigestBundleArtifact(bundle ConfigBundle) (string, error) {
	if err := bundle.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return "", fmt.Errorf("marshal config bundle artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateSetPatch(name string, patch SetPatch, validate func(string) error) error {
	if patch.Add == nil || patch.Remove == nil {
		return fmt.Errorf("%s add and remove must be explicit arrays", name)
	}
	adds := make(map[string]struct{}, len(patch.Add))
	for _, value := range patch.Add {
		if err := validate(value); err != nil {
			return fmt.Errorf("%s.add: %w", name, err)
		}
		if _, duplicate := adds[value]; duplicate {
			return fmt.Errorf("%s.add contains duplicate %q", name, value)
		}
		adds[value] = struct{}{}
	}
	removes := make(map[string]struct{}, len(patch.Remove))
	for _, value := range patch.Remove {
		if err := validate(value); err != nil {
			return fmt.Errorf("%s.remove: %w", name, err)
		}
		if _, duplicate := removes[value]; duplicate {
			return fmt.Errorf("%s.remove contains duplicate %q", name, value)
		}
		if _, conflict := adds[value]; conflict {
			return fmt.Errorf("%s cannot add and remove %q in one revision", name, value)
		}
		removes[value] = struct{}{}
	}
	return nil
}

func validateSortedSet(
	name string,
	values []string,
	validate func(string) error,
	allowEmpty bool,
) error {
	if values == nil || !allowEmpty && len(values) == 0 {
		return fmt.Errorf("%s must be an explicit non-empty sorted array", name)
	}
	if !slices.IsSorted(values) {
		return fmt.Errorf("%s must be sorted", name)
	}
	for index, value := range values {
		if err := validate(value); err != nil {
			return fmt.Errorf("%s[%d]: %w", name, index, err)
		}
		if index > 0 && value == values[index-1] {
			return fmt.Errorf("%s contains duplicate %q", name, value)
		}
	}
	return nil
}

func validateTargetMode(value string) error {
	switch value {
	case "diff", "selection", "scope":
		return nil
	default:
		return fmt.Errorf("unsupported target mode %q", value)
	}
}

func validateSeverity(value string) error {
	switch value {
	case "info", "low", "medium", "high", "critical":
		return nil
	default:
		return fmt.Errorf("unsupported severity %q", value)
	}
}

func validatePattern(value string) error {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) || strings.HasPrefix(value, "/") ||
		strings.ContainsAny(value, "\\:?[]") || containsControl(value) {
		return fmt.Errorf("pattern %q is unsafe", value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." ||
			strings.Contains(segment, "***") {
			return fmt.Errorf("pattern %q contains an unsafe segment", value)
		}
	}
	return nil
}

func validateOptionalPathPrefix(value string) error {
	if value == "" {
		return nil
	}
	return validatePathPrefix("path", value)
}

func validatePathPrefix(name, value string) error {
	if err := validateRepositoryPath(name, value); err != nil {
		return err
	}
	if strings.ContainsAny(value, "*?[") {
		return fmt.Errorf("%s must be a literal path prefix", name)
	}
	return nil
}

func validateRepositoryPath(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, `\:`) ||
		strings.HasPrefix(value, "/") || containsControl(value) ||
		path.Clean(value) != value {
		return fmt.Errorf("%s must be a clean repository-relative path", name)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("%s must not traverse parent paths", name)
		}
	}
	return nil
}

func validateSimpleValue(value string) error {
	return validateID("value", value)
}

func validateID(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	for _, character := range value {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) &&
			!strings.ContainsRune("._~-", character) {
			return fmt.Errorf("%s must be a non-empty safe identifier", name)
		}
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%s must be a lowercase SHA-256", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256", name)
	}
	return nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func pathPrefixMatches(prefix, target string) bool {
	return target == prefix || strings.HasPrefix(target, prefix+"/")
}

func requireFields(section string, count int) error {
	if count == 0 {
		return fmt.Errorf("%s patch must change at least one field", section)
	}
	return nil
}

func cloneRules(rules []RuleDefinition) []RuleDefinition {
	if rules == nil {
		return nil
	}
	cloned := make([]RuleDefinition, len(rules))
	for index, rule := range rules {
		cloned[index] = rule
		cloned[index].Languages = slices.Clone(rule.Languages)
		cloned[index].PathPrefixes = slices.Clone(rule.PathPrefixes)
		cloned[index].EvidenceKinds = slices.Clone(rule.EvidenceKinds)
	}
	return cloned
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("config revision contains trailing JSON")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	if err := walk(); err != nil {
		return fmt.Errorf("validate config revision JSON: %w", err)
	}
	return nil
}

func rejectJSONNulls(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("validate config JSON nulls: %w", err)
		}
		if token == nil {
			return fmt.Errorf("config JSON must not contain null")
		}
	}
}
