package reviewconfig

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

type resolverState struct {
	bundle     ConfigBundle
	fieldSet   map[string]bool
	provenance map[string][]SourceRef
	explain    []MergeExplanation
}

// Resolve deterministically applies every applicable revision from the least
// specific scope to the most specific one. Input order never affects output.
func Resolve(context ResolutionContext, revisions []Revision) (ConfigBundle, error) {
	if err := context.Validate(); err != nil {
		return ConfigBundle{}, fmt.Errorf("validate resolution context: %w", err)
	}
	if revisions == nil {
		return ConfigBundle{}, fmt.Errorf("config revisions must be an explicit array")
	}
	applicable, err := applicableRevisions(context, revisions)
	if err != nil {
		return ConfigBundle{}, err
	}
	if len(applicable) == 0 {
		return ConfigBundle{}, fmt.Errorf("no applicable config revision")
	}

	state := resolverState{
		bundle: ConfigBundle{
			SchemaVersion:    BundleSchemaVersion,
			Context:          context,
			AppliedRevisions: []SourceRef{},
			Target: TargetPolicy{
				AllowedModes: []string{}, Include: []string{}, Exclude: []string{},
			},
			RulePack: RulePack{
				SchemaVersion: RulePackSchemaVersion,
				ID:            "effective",
				Revision:      "resolved",
				Rules:         []RuleDefinition{},
			},
			Execution: ExecutionPolicy{
				AllowedTools:     []string{},
				ContextProviders: []ContextProviderDefinition{},
			},
			Verification: VerificationPolicy{RequiredEvidenceKinds: []string{}},
			Publication:  PublicationPolicy{Channels: []string{}},
			FieldSources: []FieldSource{},
			Explain:      []MergeExplanation{},
		},
		fieldSet:   make(map[string]bool, len(fieldStrategies)),
		provenance: make(map[string][]SourceRef, len(fieldStrategies)),
		explain:    []MergeExplanation{},
	}
	for _, revision := range applicable {
		source := sourceFor(revision, "applied")
		state.bundle.AppliedRevisions = append(state.bundle.AppliedRevisions, source)
		if err := state.apply(revision); err != nil {
			return ConfigBundle{}, fmt.Errorf(
				"apply %s config %s@%s: %w",
				revision.Scope,
				revision.ID,
				revision.Revision,
				err,
			)
		}
	}

	ruleDigest, err := DigestRulePack(state.bundle.RulePack)
	if err != nil {
		return ConfigBundle{}, err
	}
	state.bundle.RulePack.SHA256 = ruleDigest
	if state.bundle.AgentReview != nil {
		agentDigest, err := DigestAgentReviewPolicy(*state.bundle.AgentReview)
		if err != nil {
			return ConfigBundle{}, err
		}
		state.bundle.AgentReview.SHA256 = agentDigest
	}
	if state.bundle.FindingGovernance != nil {
		governanceDigest, err := DigestFindingGovernancePolicy(*state.bundle.FindingGovernance)
		if err != nil {
			return ConfigBundle{}, err
		}
		state.bundle.FindingGovernance.SHA256 = governanceDigest
	}
	state.bundle.FieldSources = state.buildFieldSources()
	state.bundle.Explain = slices.Clone(state.explain)

	if err := state.bundle.validateSemantics(state.fieldSet); err != nil {
		return ConfigBundle{}, fmt.Errorf("validate effective config: %w", err)
	}
	digest, err := DigestBundle(state.bundle)
	if err != nil {
		return ConfigBundle{}, err
	}
	state.bundle.SHA256 = digest
	state.bundle.BundleID = "bundle-" + digest[:24]
	if err := state.bundle.Validate(); err != nil {
		return ConfigBundle{}, err
	}
	return state.bundle, nil
}

func applicableRevisions(context ResolutionContext, revisions []Revision) ([]Revision, error) {
	applicable := make([]Revision, 0, len(revisions))
	identities := make(map[string]struct{}, len(revisions))
	selectors := make(map[string]struct{}, len(revisions))
	for index, revision := range revisions {
		if err := revision.Validate(); err != nil {
			return nil, fmt.Errorf("config revisions[%d]: %w", index, err)
		}
		identity := revision.ID + "\x00" + revision.Revision
		if _, duplicate := identities[identity]; duplicate {
			return nil, fmt.Errorf(
				"config revisions contain ambiguous identity %s@%s",
				revision.ID,
				revision.Revision,
			)
		}
		identities[identity] = struct{}{}
		matches, err := revision.Applicable(context)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}
		selectorIdentity := string(revision.Scope) + "\x00" + selectorKey(revision)
		if _, ambiguous := selectors[selectorIdentity]; ambiguous {
			return nil, fmt.Errorf(
				"multiple applicable revisions have the same %s selector %q",
				revision.Scope,
				selectorKey(revision),
			)
		}
		selectors[selectorIdentity] = struct{}{}
		applicable = append(applicable, revision)
	}
	sort.Slice(applicable, func(left, right int) bool {
		leftSpecificity, _ := applicable[left].Specificity()
		rightSpecificity, _ := applicable[right].Specificity()
		if leftSpecificity.ScopeRank != rightSpecificity.ScopeRank {
			return leftSpecificity.ScopeRank < rightSpecificity.ScopeRank
		}
		if leftSpecificity.PathDepth != rightSpecificity.PathDepth {
			return leftSpecificity.PathDepth < rightSpecificity.PathDepth
		}
		return selectorKey(applicable[left]) < selectorKey(applicable[right])
	})
	return applicable, nil
}

func (state *resolverState) apply(revision Revision) error {
	patch := revision.Patch
	if patch.Target != nil {
		if patch.Target.AllowedModes != nil {
			if err := state.applySet(
				"target.allowed_modes",
				&state.bundle.Target.AllowedModes,
				*patch.Target.AllowedModes,
				revision,
			); err != nil {
				return err
			}
		}
		if patch.Target.Include != nil {
			if err := state.applySet(
				"target.include",
				&state.bundle.Target.Include,
				*patch.Target.Include,
				revision,
			); err != nil {
				return err
			}
		}
		if patch.Target.Exclude != nil {
			if err := state.applySet(
				"target.exclude",
				&state.bundle.Target.Exclude,
				*patch.Target.Exclude,
				revision,
			); err != nil {
				return err
			}
		}
		if patch.Target.MaxFiles != nil {
			state.bundle.Target.MaxFiles = *patch.Target.MaxFiles
			state.replace("target.max_files", revision, "overrode scalar value")
		}
		if patch.Target.MaxPatchBytes != nil {
			state.bundle.Target.MaxPatchBytes = *patch.Target.MaxPatchBytes
			state.replace("target.max_patch_bytes", revision, "overrode scalar value")
		}
	}
	if patch.RulePack != nil {
		if err := state.applyRules(*patch.RulePack.Rules, revision); err != nil {
			return err
		}
	}
	if patch.Workflow != nil {
		state.bundle.Workflow.Definition = *patch.Workflow.Definition
		state.replace("workflow.definition", revision, "replaced atomic versioned reference")
	}
	if patch.Execution != nil {
		if patch.Execution.AgentProfile != nil {
			state.bundle.Execution.AgentProfile = *patch.Execution.AgentProfile
			state.replace(
				"execution.agent_profile",
				revision,
				"replaced atomic versioned reference",
			)
		}
		if patch.Execution.ModelProfile != nil {
			state.bundle.Execution.ModelProfile = *patch.Execution.ModelProfile
			state.replace(
				"execution.model_profile",
				revision,
				"replaced atomic versioned reference",
			)
		}
		if patch.Execution.ModelCredential != nil {
			value := *patch.Execution.ModelCredential
			state.bundle.Execution.ModelCredential = &value
			state.replace(
				"execution.model_credential_ref",
				revision,
				"replaced secret reference without resolving secret material",
			)
		}
		if patch.Execution.AllowedTools != nil {
			if err := state.applySet(
				"execution.allowed_tools",
				&state.bundle.Execution.AllowedTools,
				*patch.Execution.AllowedTools,
				revision,
			); err != nil {
				return err
			}
		}
		if patch.Execution.ContextProviders != nil {
			if err := state.applyContextProviders(
				*patch.Execution.ContextProviders,
				revision,
			); err != nil {
				return err
			}
		}
		if patch.Execution.ContextProviderMaxConcurrency != nil {
			state.bundle.Execution.ContextProviderMaxConcurrency =
				*patch.Execution.ContextProviderMaxConcurrency
			state.replace(
				"execution.context_provider_max_concurrency",
				revision,
				"overrode bounded Context Provider concurrency",
			)
		}
	}
	if patch.AgentReview != nil {
		if state.bundle.AgentReview == nil {
			state.bundle.AgentReview = &AgentReviewPolicy{
				SchemaVersion:  AgentReviewPolicySchemaVersion,
				ID:             "effective",
				Revision:       "resolved",
				SkillPacks:     []AgentSkillPackDefinition{},
				KnowledgePacks: []AgentKnowledgePackDefinition{},
				Authority: AgentAuthority{
					Tools: []string{},
				},
			}
		}
		if err := state.applyAgentReview(*patch.AgentReview, revision); err != nil {
			return err
		}
	}
	if patch.Budget != nil {
		for _, field := range []struct {
			name   string
			input  *int64
			output *int64
		}{
			{"budget.max_input_bytes", patch.Budget.MaxInputBytes, &state.bundle.Budget.MaxInputBytes},
			{"budget.max_output_bytes", patch.Budget.MaxOutputBytes, &state.bundle.Budget.MaxOutputBytes},
			{"budget.max_tokens", patch.Budget.MaxTokens, &state.bundle.Budget.MaxTokens},
			{"budget.max_cost_micros", patch.Budget.MaxCostMicros, &state.bundle.Budget.MaxCostMicros},
			{"budget.stage_timeout_ms", patch.Budget.StageTimeoutMS, &state.bundle.Budget.StageTimeoutMS},
		} {
			if field.input != nil {
				*field.output = *field.input
				state.replace(field.name, revision, "overrode scalar value")
			}
		}
		if patch.Budget.MaxConcurrency != nil {
			state.bundle.Budget.MaxConcurrency = *patch.Budget.MaxConcurrency
			state.replace("budget.max_concurrency", revision, "overrode scalar value")
		}
		if patch.Budget.MaxAttempts != nil {
			state.bundle.Budget.MaxAttempts = *patch.Budget.MaxAttempts
			state.replace("budget.max_attempts", revision, "overrode scalar value")
		}
	}
	if patch.Verification != nil {
		if patch.Verification.RequiredEvidenceKinds != nil {
			if err := state.applySet(
				"verification.required_evidence_kinds",
				&state.bundle.Verification.RequiredEvidenceKinds,
				*patch.Verification.RequiredEvidenceKinds,
				revision,
			); err != nil {
				return err
			}
		}
		if patch.Verification.MinimumIndependentEvidence != nil {
			state.bundle.Verification.MinimumIndependentEvidence =
				*patch.Verification.MinimumIndependentEvidence
			state.replace(
				"verification.minimum_independent_evidence",
				revision,
				"overrode scalar value",
			)
		}
		if patch.Verification.PartialEvidence != nil {
			state.bundle.Verification.PartialEvidence = state.applyPermission(
				"verification.partial_evidence",
				state.bundle.Verification.PartialEvidence,
				*patch.Verification.PartialEvidence,
				revision,
			)
		}
	}
	if patch.Adjudication != nil {
		if patch.Adjudication.MinimumSeverity != nil {
			state.bundle.Adjudication.MinimumSeverity = *patch.Adjudication.MinimumSeverity
			state.replace(
				"adjudication.minimum_severity",
				revision,
				"overrode scalar value",
			)
		}
		if patch.Adjudication.HumanReviewInconclusive != nil {
			state.bundle.Adjudication.HumanReviewInconclusive =
				*patch.Adjudication.HumanReviewInconclusive
			state.replace(
				"adjudication.human_review_inconclusive",
				revision,
				"overrode scalar value",
			)
		}
		if patch.Adjudication.AutomaticPublication != nil {
			state.bundle.Adjudication.AutomaticPublication = state.applyPermission(
				"adjudication.automatic_publication",
				state.bundle.Adjudication.AutomaticPublication,
				*patch.Adjudication.AutomaticPublication,
				revision,
			)
		}
	}
	if patch.FindingGovernance != nil {
		if state.bundle.FindingGovernance == nil {
			state.bundle.FindingGovernance = &FindingGovernancePolicy{
				SchemaVersion: FindingGovernancePolicySchemaVersion,
				ID:            "effective", Revision: "resolved",
			}
		}
		if patch.FindingGovernance.CalibrationProfile != nil {
			profile := *patch.FindingGovernance.CalibrationProfile
			profile.Points = slices.Clone(profile.Points)
			state.bundle.FindingGovernance.CalibrationProfile = profile
			state.replace("finding_governance.calibration_profile", revision, "replaced immutable calibration profile")
		}
		if patch.FindingGovernance.MinimumConfidencePPM != nil {
			state.bundle.FindingGovernance.MinimumConfidencePPM = *patch.FindingGovernance.MinimumConfidencePPM
			state.replace("finding_governance.minimum_confidence_ppm", revision, "overrode integer confidence threshold")
		}
		if patch.FindingGovernance.MaxFindings != nil {
			state.bundle.FindingGovernance.MaxFindings = *patch.FindingGovernance.MaxFindings
			state.replace("finding_governance.max_findings", revision, "overrode visible finding limit")
		}
	}
	if patch.Publication != nil {
		if patch.Publication.RemoteWrites != nil {
			state.bundle.Publication.RemoteWrites = state.applyPermission(
				"publication.remote_writes",
				state.bundle.Publication.RemoteWrites,
				*patch.Publication.RemoteWrites,
				revision,
			)
		}
		if patch.Publication.Channels != nil {
			if err := state.applySet(
				"publication.channels",
				&state.bundle.Publication.Channels,
				*patch.Publication.Channels,
				revision,
			); err != nil {
				return err
			}
		}
		if patch.Publication.MaxComments != nil {
			state.bundle.Publication.MaxComments = *patch.Publication.MaxComments
			state.replace("publication.max_comments", revision, "overrode scalar value")
		}
	}
	if patch.Data != nil {
		if patch.Data.RetentionDays != nil {
			state.bundle.Data.RetentionDays = *patch.Data.RetentionDays
			state.replace("data.retention_days", revision, "overrode scalar value")
		}
		if patch.Data.Redaction != nil {
			state.bundle.Data.Redaction = state.applyRedaction(
				state.bundle.Data.Redaction,
				*patch.Data.Redaction,
				revision,
			)
		}
		if patch.Data.Training != nil {
			state.bundle.Data.Training = state.applyPermission(
				"data.training",
				state.bundle.Data.Training,
				*patch.Data.Training,
				revision,
			)
		}
		if patch.Data.Export != nil {
			state.bundle.Data.Export = state.applyPermission(
				"data.export",
				state.bundle.Data.Export,
				*patch.Data.Export,
				revision,
			)
		}
	}
	return nil
}

func (state *resolverState) applyAgentReview(
	patch AgentReviewPatch,
	revision Revision,
) error {
	policy := state.bundle.AgentReview
	for _, field := range []struct {
		name   string
		input  *VersionedRef
		output *VersionedRef
	}{
		{"agent_review.agent", patch.Agent, &policy.Agent},
		{"agent_review.normalization", patch.Normalization, &policy.Normalization},
		{"agent_review.provider", patch.Provider, &policy.Provider},
		{"agent_review.model", patch.Model, &policy.Model},
		{"agent_review.prompt", patch.Prompt, &policy.Prompt},
		{"agent_review.api_protocol", patch.APIProtocol, &policy.APIProtocol},
	} {
		if field.input != nil {
			*field.output = *field.input
			state.replace(field.name, revision, "replaced exact versioned reference")
		}
	}
	if patch.ModelCredential != nil {
		value := *patch.ModelCredential
		policy.ModelCredential = &value
		state.replace(
			"agent_review.model_credential_ref",
			revision,
			"replaced secret reference without resolving secret material",
		)
	}
	if patch.SkillPacks != nil {
		if err := state.applyAgentSkillPacks(*patch.SkillPacks, revision); err != nil {
			return err
		}
	}
	if patch.KnowledgePacks != nil {
		if err := state.applyAgentKnowledgePacks(*patch.KnowledgePacks, revision); err != nil {
			return err
		}
	}
	if patch.Authority != nil {
		authority := patch.Authority
		if authority.ModelEgress != nil {
			policy.Authority.ModelEgress = *authority.ModelEgress
			state.replace(
				"agent_review.authority.model_egress",
				revision,
				"replaced brokered model egress authority",
			)
		}
		if authority.ToolNetwork != nil {
			policy.Authority.ToolNetwork = state.applyPermission(
				"agent_review.authority.tool_network",
				policy.Authority.ToolNetwork,
				*authority.ToolNetwork,
				revision,
			)
		}
		if authority.WorkspaceReads != nil {
			policy.Authority.WorkspaceReads = *authority.WorkspaceReads
			state.replace(
				"agent_review.authority.workspace_reads",
				revision,
				"replaced workspace read authority",
			)
		}
		if authority.WorkspaceWrites != nil {
			policy.Authority.WorkspaceWrites = state.applyPermission(
				"agent_review.authority.workspace_writes",
				policy.Authority.WorkspaceWrites,
				*authority.WorkspaceWrites,
				revision,
			)
		}
		if authority.RemoteWrites != nil {
			policy.Authority.RemoteWrites = state.applyPermission(
				"agent_review.authority.remote_writes",
				policy.Authority.RemoteWrites,
				*authority.RemoteWrites,
				revision,
			)
		}
		if authority.MaxDelegationDepth != nil {
			policy.Authority.MaxDelegationDepth = *authority.MaxDelegationDepth
			state.replace(
				"agent_review.authority.max_delegation_depth",
				revision,
				"replaced delegation depth",
			)
		}
		if authority.Tools != nil {
			if err := state.applySet(
				"agent_review.authority.tools",
				&policy.Authority.Tools,
				*authority.Tools,
				revision,
			); err != nil {
				return err
			}
		}
	}
	if patch.Budget != nil {
		budget := patch.Budget
		for _, field := range []struct {
			name   string
			input  *int
			output *int
		}{
			{"agent_review.budget.max_files", budget.MaxFiles, &policy.Budget.MaxFiles},
			{"agent_review.budget.max_groups", budget.MaxGroups, &policy.Budget.MaxGroups},
			{"agent_review.budget.max_hypotheses", budget.MaxHypotheses, &policy.Budget.MaxHypotheses},
			{"agent_review.budget.max_model_calls", budget.MaxModelCalls, &policy.Budget.MaxModelCalls},
			{"agent_review.budget.max_tool_calls", budget.MaxToolCalls, &policy.Budget.MaxToolCalls},
			{"agent_review.budget.max_concurrency", budget.MaxConcurrency, &policy.Budget.MaxConcurrency},
		} {
			if field.input != nil {
				*field.output = *field.input
				state.replace(field.name, revision, "overrode agent budget limit")
			}
		}
		for _, field := range []struct {
			name   string
			input  *int64
			output *int64
		}{
			{"agent_review.budget.max_target_bytes", budget.MaxTargetBytes, &policy.Budget.MaxTargetBytes},
			{"agent_review.budget.max_group_bytes", budget.MaxGroupBytes, &policy.Budget.MaxGroupBytes},
			{"agent_review.budget.max_output_bytes", budget.MaxOutputBytes, &policy.Budget.MaxOutputBytes},
			{"agent_review.budget.max_output_tokens", budget.MaxOutputTokens, &policy.Budget.MaxOutputTokens},
			{"agent_review.budget.max_cost_micros", budget.MaxCostMicros, &policy.Budget.MaxCostMicros},
			{"agent_review.budget.timeout_ms", budget.TimeoutMS, &policy.Budget.TimeoutMS},
		} {
			if field.input != nil {
				*field.output = *field.input
				state.replace(field.name, revision, "overrode agent budget limit")
			}
		}
	}
	return nil
}

func (state *resolverState) applyAgentSkillPacks(
	patch OrderedAgentSkillPackPatch,
	revision Revision,
) error {
	field := "agent_review.skill_packs"
	policy := state.bundle.AgentReview
	for _, id := range patch.Remove {
		index := indexAgentSkillPack(policy.SkillPacks, id)
		if index < 0 {
			return fmt.Errorf("%s cannot remove absent stable id %q", field, id)
		}
		policy.SkillPacks = slices.Delete(policy.SkillPacks, index, index+1)
		delete(state.provenance, field+"["+id+"]")
		delete(state.fieldSet, field+"["+id+"]")
		state.explain = append(state.explain, MergeExplanation{
			Field: field + "[" + id + "]", Strategy: MergeOrderedStableIDOverride,
			Source: sourceFor(revision, "remove"), Action: "removed stable-id entry",
			Detail: "skill_pack_id=" + id,
		})
	}
	for _, pack := range patch.Upsert {
		index := indexAgentSkillPack(policy.SkillPacks, pack.ID)
		action := "appended stable-id entry"
		if index >= 0 {
			policy.SkillPacks[index] = pack
			action = "replaced stable-id entry in place"
		} else {
			policy.SkillPacks = append(policy.SkillPacks, pack)
		}
		state.setSource(
			field+"["+pack.ID+"]",
			sourceFor(revision, "upsert"),
			MergeOrderedStableIDOverride,
			action,
			"skill_pack_id="+pack.ID+" phase="+string(pack.Phase),
		)
	}
	state.fieldSet[field] = true
	state.provenance[field] = append(
		state.provenance[field],
		sourceFor(revision, "ordered_stable_id_override"),
	)
	state.explain = append(state.explain, MergeExplanation{
		Field: field, Strategy: MergeOrderedStableIDOverride,
		Source: sourceFor(revision, "ordered_stable_id_override"),
		Action: "merged ordered stable-id entries",
		Detail: fmt.Sprintf("upsert=%d remove=%d", len(patch.Upsert), len(patch.Remove)),
	})
	return nil
}

func (state *resolverState) applyAgentKnowledgePacks(
	patch OrderedAgentKnowledgePackPatch,
	revision Revision,
) error {
	field := "agent_review.knowledge_packs"
	policy := state.bundle.AgentReview
	for _, id := range patch.Remove {
		index := indexAgentKnowledgePack(policy.KnowledgePacks, id)
		if index < 0 {
			return fmt.Errorf("%s cannot remove absent stable id %q", field, id)
		}
		policy.KnowledgePacks = slices.Delete(policy.KnowledgePacks, index, index+1)
		delete(state.provenance, field+"["+id+"]")
		delete(state.fieldSet, field+"["+id+"]")
		state.explain = append(state.explain, MergeExplanation{
			Field: field + "[" + id + "]", Strategy: MergeOrderedStableIDOverride,
			Source: sourceFor(revision, "remove"), Action: "removed stable-id entry",
			Detail: "knowledge_pack_id=" + id,
		})
	}
	for _, pack := range patch.Upsert {
		index := indexAgentKnowledgePack(policy.KnowledgePacks, pack.ID)
		action := "appended stable-id entry"
		if index >= 0 {
			policy.KnowledgePacks[index] = pack
			action = "replaced stable-id entry in place"
		} else {
			policy.KnowledgePacks = append(policy.KnowledgePacks, pack)
		}
		state.setSource(
			field+"["+pack.ID+"]",
			sourceFor(revision, "upsert"),
			MergeOrderedStableIDOverride,
			action,
			"knowledge_pack_id="+pack.ID,
		)
	}
	state.fieldSet[field] = true
	state.provenance[field] = append(
		state.provenance[field],
		sourceFor(revision, "ordered_stable_id_override"),
	)
	state.explain = append(state.explain, MergeExplanation{
		Field: field, Strategy: MergeOrderedStableIDOverride,
		Source: sourceFor(revision, "ordered_stable_id_override"),
		Action: "merged ordered stable-id entries",
		Detail: fmt.Sprintf("upsert=%d remove=%d", len(patch.Upsert), len(patch.Remove)),
	})
	return nil
}

func (state *resolverState) applySet(
	field string,
	current *[]string,
	patch SetPatch,
	revision Revision,
) error {
	values := make(map[string]struct{}, len(*current)+len(patch.Add))
	for _, value := range *current {
		values[value] = struct{}{}
	}
	for _, value := range patch.Add {
		values[value] = struct{}{}
	}
	for _, value := range patch.Remove {
		if _, exists := values[value]; !exists {
			return fmt.Errorf("%s cannot remove absent value %q", field, value)
		}
		delete(values, value)
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	*current = result
	source := sourceFor(revision, "union_subtract")
	state.contribute(
		field,
		source,
		"merged set",
		fmt.Sprintf("add=%s remove=%s", strings.Join(patch.Add, ","), strings.Join(patch.Remove, ",")),
	)
	return nil
}

func (state *resolverState) applyRules(
	patch OrderedRulePatch,
	revision Revision,
) error {
	field := "rule_pack.rules"
	for _, id := range patch.Remove {
		index := indexRule(state.bundle.RulePack.Rules, id)
		if index < 0 {
			return fmt.Errorf("%s cannot remove absent stable id %q", field, id)
		}
		state.bundle.RulePack.Rules = slices.Delete(state.bundle.RulePack.Rules, index, index+1)
		delete(state.provenance, field+"["+id+"]")
		state.explain = append(state.explain, MergeExplanation{
			Field: field + "[" + id + "]", Strategy: MergeOrderedStableIDOverride,
			Source: sourceFor(revision, "remove"), Action: "removed stable-id entry",
			Detail: "removed=" + id,
		})
	}
	for _, rule := range patch.Upsert {
		index := indexRule(state.bundle.RulePack.Rules, rule.ID)
		action := "appended stable-id entry"
		if index >= 0 {
			state.bundle.RulePack.Rules[index] = rule
			action = "replaced stable-id entry in place"
		} else {
			state.bundle.RulePack.Rules = append(state.bundle.RulePack.Rules, rule)
		}
		state.setSource(
			field+"["+rule.ID+"]",
			sourceFor(revision, "upsert"),
			MergeOrderedStableIDOverride,
			action,
			"rule_id="+rule.ID,
		)
	}
	state.fieldSet[field] = true
	state.provenance[field] = append(
		state.provenance[field],
		sourceFor(revision, "ordered_stable_id_override"),
	)
	return nil
}

func (state *resolverState) applyContextProviders(
	patch OrderedContextProviderPatch,
	revision Revision,
) error {
	field := "execution.context_providers"
	for _, id := range patch.Remove {
		index := indexProvider(state.bundle.Execution.ContextProviders, id)
		if index < 0 {
			return fmt.Errorf("%s cannot remove absent stable id %q", field, id)
		}
		state.bundle.Execution.ContextProviders = slices.Delete(
			state.bundle.Execution.ContextProviders,
			index,
			index+1,
		)
		delete(state.provenance, field+"["+id+"]")
		state.explain = append(state.explain, MergeExplanation{
			Field: field + "[" + id + "]", Strategy: MergeOrderedStableIDOverride,
			Source: sourceFor(revision, "remove"), Action: "removed stable-id entry",
			Detail: "provider_id=" + id,
		})
	}
	for _, provider := range patch.Upsert {
		index := indexProvider(state.bundle.Execution.ContextProviders, provider.ID)
		action := "appended stable-id entry"
		if index >= 0 {
			state.bundle.Execution.ContextProviders[index] = provider
			action = "replaced stable-id entry in place"
		} else {
			state.bundle.Execution.ContextProviders = append(
				state.bundle.Execution.ContextProviders,
				provider,
			)
		}
		state.setSource(
			field+"["+provider.ID+"]",
			sourceFor(revision, "upsert"),
			MergeOrderedStableIDOverride,
			action,
			"provider_id="+provider.ID,
		)
	}
	state.fieldSet[field] = true
	state.provenance[field] = append(
		state.provenance[field],
		sourceFor(revision, "ordered_stable_id_override"),
	)
	return nil
}

func (state *resolverState) applyPermission(
	field string,
	current Permission,
	incoming Permission,
	revision Revision,
) Permission {
	result := incoming
	action := "initialized permission"
	if state.fieldSet[field] {
		action = "merged permission"
		if current == PermissionDeny || incoming == PermissionDeny {
			result = PermissionDeny
			action = "deny won"
		}
	}
	state.contribute(
		field,
		sourceFor(revision, string(incoming)),
		action,
		"effective="+string(result),
	)
	return result
}

func (state *resolverState) applyRedaction(
	current RedactionLevel,
	incoming RedactionLevel,
	revision Revision,
) RedactionLevel {
	result := incoming
	action := "initialized redaction"
	if state.fieldSet["data.redaction"] {
		if redactionRank(current) >= redactionRank(incoming) {
			result = current
		}
		action = "most restrictive redaction won"
	}
	state.contribute(
		"data.redaction",
		sourceFor(revision, string(incoming)),
		action,
		"effective="+string(result),
	)
	return result
}

func (state *resolverState) replace(field string, revision Revision, detail string) {
	state.setSource(
		field,
		sourceFor(revision, "replace"),
		fieldStrategies[field],
		"replaced effective value",
		detail,
	)
}

func (state *resolverState) setSource(
	field string,
	source SourceRef,
	strategy MergeStrategy,
	action string,
	detail string,
) {
	state.fieldSet[field] = true
	state.provenance[field] = []SourceRef{source}
	state.explain = append(state.explain, MergeExplanation{
		Field: field, Strategy: strategy, Source: source, Action: action, Detail: detail,
	})
}

func (state *resolverState) contribute(
	field string,
	source SourceRef,
	action string,
	detail string,
) {
	state.fieldSet[field] = true
	state.provenance[field] = append(state.provenance[field], source)
	state.explain = append(state.explain, MergeExplanation{
		Field: field, Strategy: fieldStrategies[field], Source: source,
		Action: action, Detail: detail,
	})
}

func (state *resolverState) buildFieldSources() []FieldSource {
	fields := make([]string, 0, len(state.provenance))
	for field := range state.provenance {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	result := make([]FieldSource, 0, len(fields))
	for _, field := range fields {
		strategy, exists := fieldStrategies[field]
		if !exists {
			if isOrderedStableIDField(field) {
				strategy = MergeOrderedStableIDOverride
			} else {
				panic("reviewconfig: provenance for undeclared field " + field)
			}
		}
		result = append(result, FieldSource{
			Field: field, Strategy: strategy, Sources: slices.Clone(state.provenance[field]),
		})
	}
	return result
}

func (bundle ConfigBundle) Validate() error {
	if bundle.SchemaVersion != BundleSchemaVersion {
		return fmt.Errorf("unsupported config bundle schema %q", bundle.SchemaVersion)
	}
	if err := bundle.Context.Validate(); err != nil {
		return err
	}
	if bundle.AppliedRevisions == nil || len(bundle.AppliedRevisions) == 0 {
		return fmt.Errorf("applied_revisions must be an explicit non-empty array")
	}
	lastScopeRank := -1
	for index, source := range bundle.AppliedRevisions {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("applied_revisions[%d]: %w", index, err)
		}
		if source.Operation != "applied" {
			return fmt.Errorf("applied_revisions[%d] has invalid operation", index)
		}
		rank := scopeRank(source.Scope)
		if rank < lastScopeRank {
			return fmt.Errorf("applied_revisions are not ordered by specificity")
		}
		lastScopeRank = rank
	}
	fieldSet := make(map[string]bool, len(bundle.FieldSources))
	if bundle.FieldSources == nil || !sort.SliceIsSorted(
		bundle.FieldSources,
		func(left, right int) bool {
			return bundle.FieldSources[left].Field < bundle.FieldSources[right].Field
		},
	) {
		return fmt.Errorf("field_sources must be an explicit sorted array")
	}
	for index, source := range bundle.FieldSources {
		strategy, exists := fieldStrategies[source.Field]
		if !exists {
			if isOrderedStableIDField(source.Field) {
				strategy = MergeOrderedStableIDOverride
			} else {
				return fmt.Errorf("field_sources[%d] references unknown field %q", index, source.Field)
			}
		}
		if source.Strategy != strategy || len(source.Sources) == 0 {
			return fmt.Errorf("field_sources[%d] has invalid strategy or sources", index)
		}
		for sourceIndex, contribution := range source.Sources {
			if err := contribution.Validate(); err != nil {
				return fmt.Errorf(
					"field_sources[%d].sources[%d]: %w",
					index,
					sourceIndex,
					err,
				)
			}
		}
		if fieldSet[source.Field] {
			return fmt.Errorf("field_sources contains duplicate field %q", source.Field)
		}
		fieldSet[source.Field] = true
	}
	if bundle.Explain == nil {
		return fmt.Errorf("explain must be an explicit array")
	}
	explainedFields := make(map[string]bool, len(bundle.Explain))
	for index, explanation := range bundle.Explain {
		strategy, exists := fieldStrategies[explanation.Field]
		if !exists {
			if isOrderedStableIDField(explanation.Field) {
				strategy = MergeOrderedStableIDOverride
			} else {
				return fmt.Errorf("explain[%d] references unknown field %q", index, explanation.Field)
			}
		}
		if explanation.Strategy != strategy ||
			explanation.Action == "" || explanation.Detail == "" {
			return fmt.Errorf("explain[%d] is incomplete", index)
		}
		if err := explanation.Source.Validate(); err != nil {
			return fmt.Errorf("explain[%d].source: %w", index, err)
		}
		explainedFields[explanation.Field] = true
	}
	if bundle.AgentReview != nil {
		for field := range fieldSet {
			if strings.HasPrefix(field, "agent_review.") && !explainedFields[field] {
				return fmt.Errorf("agent review field %q has no explanation", field)
			}
		}
	}
	if err := bundle.validateSemantics(fieldSet); err != nil {
		return err
	}
	digest, err := DigestBundle(bundle)
	if err != nil {
		return err
	}
	if bundle.SHA256 != digest || bundle.BundleID != "bundle-"+digest[:24] {
		return fmt.Errorf("config bundle identity does not match its content")
	}
	return nil
}

func (source SourceRef) Validate() error {
	switch source.Scope {
	case ScopePlatform, ScopeTenant, ScopeOrganization, ScopeRepository,
		ScopePath, ScopeInvocation:
	default:
		return fmt.Errorf("unsupported source scope %q", source.Scope)
	}
	if source.Selector == "" || len(source.Selector) > 2048 ||
		containsControl(source.Selector) {
		return fmt.Errorf("source selector is invalid")
	}
	if err := validateID("source.id", source.ID); err != nil {
		return err
	}
	if err := validateID("source.revision", source.Revision); err != nil {
		return err
	}
	if err := validateID("source.operation", source.Operation); err != nil {
		return err
	}
	return nil
}

func (bundle ConfigBundle) validateSemantics(fieldSet map[string]bool) error {
	requiredFields := make([]string, 0, len(fieldStrategies))
	for field := range fieldStrategies {
		if field == "execution.model_credential_ref" ||
			strings.HasPrefix(field, "agent_review.") ||
			strings.HasPrefix(field, "finding_governance.") {
			continue
		}
		requiredFields = append(requiredFields, field)
	}
	sort.Strings(requiredFields)
	for _, field := range requiredFields {
		if !fieldSet[field] {
			return fmt.Errorf("required config field %q has no source", field)
		}
	}
	if err := validateSortedSet(
		"target.allowed_modes",
		bundle.Target.AllowedModes,
		validateTargetMode,
		false,
	); err != nil {
		return err
	}
	if err := validateSortedSet("target.include", bundle.Target.Include, validatePattern, true); err != nil {
		return err
	}
	if err := validateSortedSet("target.exclude", bundle.Target.Exclude, validatePattern, true); err != nil {
		return err
	}
	if overlap := firstOverlap(bundle.Target.Include, bundle.Target.Exclude); overlap != "" {
		return fmt.Errorf("target include and exclude both contain %q", overlap)
	}
	if bundle.Target.MaxFiles <= 0 || bundle.Target.MaxPatchBytes <= 0 {
		return fmt.Errorf("target limits must be positive")
	}
	if err := bundle.RulePack.Validate(); err != nil {
		return err
	}
	if err := bundle.Workflow.Definition.Validate(); err != nil {
		return fmt.Errorf("workflow.definition: %w", err)
	}
	if err := bundle.Execution.AgentProfile.Validate(); err != nil {
		return fmt.Errorf("execution.agent_profile: %w", err)
	}
	if err := bundle.Execution.ModelProfile.Validate(); err != nil {
		return fmt.Errorf("execution.model_profile: %w", err)
	}
	if bundle.Execution.ModelCredential != nil {
		if err := bundle.Execution.ModelCredential.Validate(); err != nil {
			return fmt.Errorf("execution.model_credential_ref: %w", err)
		}
	}
	if err := validateSortedSet(
		"execution.allowed_tools",
		bundle.Execution.AllowedTools,
		validateSimpleValue,
		true,
	); err != nil {
		return err
	}
	if bundle.Execution.ContextProviders == nil {
		return fmt.Errorf("execution.context_providers must be an explicit array")
	}
	if bundle.Execution.ContextProviderMaxConcurrency <= 0 ||
		bundle.Execution.ContextProviderMaxConcurrency > 16 {
		return fmt.Errorf("execution.context_provider_max_concurrency must be within 1..16")
	}
	providerIDs := make(map[string]struct{}, len(bundle.Execution.ContextProviders))
	for index, provider := range bundle.Execution.ContextProviders {
		if err := provider.Validate(); err != nil {
			return fmt.Errorf("execution.context_providers[%d]: %w", index, err)
		}
		if _, duplicate := providerIDs[provider.ID]; duplicate {
			return fmt.Errorf("execution.context_providers contains duplicate %q", provider.ID)
		}
		providerIDs[provider.ID] = struct{}{}
	}
	if bundle.Budget.MaxInputBytes <= 0 || bundle.Budget.MaxOutputBytes <= 0 ||
		bundle.Budget.MaxTokens <= 0 || bundle.Budget.MaxCostMicros <= 0 ||
		bundle.Budget.StageTimeoutMS <= 0 || bundle.Budget.MaxAttempts <= 0 ||
		bundle.Budget.MaxConcurrency <= 0 {
		return fmt.Errorf("budget fields must be positive")
	}
	if err := bundle.validateAgentReview(fieldSet); err != nil {
		return err
	}
	if bundle.FindingGovernance == nil {
		for field := range fieldSet {
			if strings.HasPrefix(field, "finding_governance.") {
				return fmt.Errorf("finding governance provenance exists without a policy")
			}
		}
	} else {
		for _, field := range []string{
			"finding_governance.calibration_profile",
			"finding_governance.minimum_confidence_ppm",
			"finding_governance.max_findings",
		} {
			if !fieldSet[field] {
				return fmt.Errorf("effective finding governance requires field %q", field)
			}
		}
		if err := bundle.FindingGovernance.Validate(); err != nil {
			return err
		}
	}
	if err := validateSortedSet(
		"verification.required_evidence_kinds",
		bundle.Verification.RequiredEvidenceKinds,
		validateSimpleValue,
		false,
	); err != nil {
		return err
	}
	if bundle.Verification.MinimumIndependentEvidence <= 0 ||
		bundle.Verification.MinimumIndependentEvidence >
			len(bundle.Verification.RequiredEvidenceKinds) {
		return fmt.Errorf("verification minimum independent evidence is inconsistent")
	}
	if err := bundle.Verification.PartialEvidence.Validate(); err != nil {
		return err
	}
	if err := validateSeverity(bundle.Adjudication.MinimumSeverity); err != nil {
		return err
	}
	if err := bundle.Adjudication.AutomaticPublication.Validate(); err != nil {
		return err
	}
	if err := bundle.Publication.RemoteWrites.Validate(); err != nil {
		return err
	}
	if err := validateSortedSet(
		"publication.channels",
		bundle.Publication.Channels,
		validateSimpleValue,
		true,
	); err != nil {
		return err
	}
	if bundle.Publication.MaxComments < 0 {
		return fmt.Errorf("publication.max_comments must not be negative")
	}
	if bundle.Adjudication.AutomaticPublication == PermissionAllow &&
		(bundle.Publication.RemoteWrites != PermissionAllow ||
			len(bundle.Publication.Channels) == 0 ||
			bundle.Publication.MaxComments == 0) {
		return fmt.Errorf("automatic publication requires allowed and bounded publication")
	}
	if bundle.Publication.RemoteWrites == PermissionAllow &&
		(len(bundle.Publication.Channels) == 0 || bundle.Publication.MaxComments == 0) {
		return fmt.Errorf("remote publication requires channels and a positive comment limit")
	}
	if bundle.Data.RetentionDays <= 0 {
		return fmt.Errorf("data.retention_days must be positive")
	}
	if err := bundle.Data.Redaction.Validate(); err != nil {
		return err
	}
	if err := bundle.Data.Training.Validate(); err != nil {
		return err
	}
	if err := bundle.Data.Export.Validate(); err != nil {
		return err
	}
	if bundle.Data.Training == PermissionAllow &&
		bundle.Data.Redaction != RedactionStrict {
		return fmt.Errorf("training requires strict redaction")
	}
	return nil
}

func (bundle ConfigBundle) validateAgentReview(fieldSet map[string]bool) error {
	if bundle.AgentReview == nil {
		for field := range fieldSet {
			if strings.HasPrefix(field, "agent_review.") {
				return fmt.Errorf("agent_review field %q exists while policy is disabled", field)
			}
		}
		return nil
	}
	required := make([]string, 0, len(fieldStrategies))
	for field := range fieldStrategies {
		if !strings.HasPrefix(field, "agent_review.") ||
			field == "agent_review.model_credential_ref" {
			continue
		}
		required = append(required, field)
	}
	sort.Strings(required)
	for _, field := range required {
		if !fieldSet[field] {
			return fmt.Errorf("required config field %q has no source", field)
		}
	}
	policy := *bundle.AgentReview
	if err := policy.Validate(); err != nil {
		return err
	}
	for _, pack := range policy.SkillPacks {
		if !fieldSet["agent_review.skill_packs["+pack.ID+"]"] {
			return fmt.Errorf("agent skill pack %q has no source", pack.ID)
		}
	}
	for _, pack := range policy.KnowledgePacks {
		if !fieldSet["agent_review.knowledge_packs["+pack.ID+"]"] {
			return fmt.Errorf("agent knowledge pack %q has no source", pack.ID)
		}
	}
	for field := range fieldSet {
		switch {
		case strings.HasPrefix(field, "agent_review.skill_packs["):
			id := strings.TrimSuffix(
				strings.TrimPrefix(field, "agent_review.skill_packs["),
				"]",
			)
			if indexAgentSkillPack(policy.SkillPacks, id) < 0 {
				return fmt.Errorf("field source references absent agent skill pack %q", id)
			}
		case strings.HasPrefix(field, "agent_review.knowledge_packs["):
			id := strings.TrimSuffix(
				strings.TrimPrefix(field, "agent_review.knowledge_packs["),
				"]",
			)
			if indexAgentKnowledgePack(policy.KnowledgePacks, id) < 0 {
				return fmt.Errorf("field source references absent agent knowledge pack %q", id)
			}
		}
	}
	budget := policy.Budget
	if budget.MaxFiles > bundle.Target.MaxFiles {
		return fmt.Errorf("agent_review.budget.max_files exceeds target.max_files")
	}
	if budget.MaxTargetBytes > bundle.Target.MaxPatchBytes ||
		budget.MaxTargetBytes > bundle.Budget.MaxInputBytes {
		return fmt.Errorf("agent_review.budget.max_target_bytes exceeds target/budget envelope")
	}
	if budget.MaxGroupBytes > bundle.Target.MaxPatchBytes ||
		budget.MaxGroupBytes > bundle.Budget.MaxInputBytes {
		return fmt.Errorf("agent_review.budget.max_group_bytes exceeds target/budget envelope")
	}
	if budget.MaxOutputBytes > bundle.Budget.MaxOutputBytes {
		return fmt.Errorf("agent_review.budget.max_output_bytes exceeds budget.max_output_bytes")
	}
	if budget.MaxOutputTokens > bundle.Budget.MaxTokens {
		return fmt.Errorf("agent_review.budget.max_output_tokens exceeds budget.max_tokens")
	}
	if budget.MaxCostMicros > bundle.Budget.MaxCostMicros {
		return fmt.Errorf("agent_review.budget.max_cost_micros exceeds budget.max_cost_micros")
	}
	if budget.TimeoutMS > bundle.Budget.StageTimeoutMS {
		return fmt.Errorf("agent_review.budget.timeout_ms exceeds budget.stage_timeout_ms")
	}
	if budget.MaxConcurrency > bundle.Budget.MaxConcurrency {
		return fmt.Errorf("agent_review.budget.max_concurrency exceeds budget.max_concurrency")
	}
	for _, tool := range policy.Authority.Tools {
		if !slices.Contains(bundle.Execution.AllowedTools, tool) {
			return fmt.Errorf("agent_review.authority.tools contains %q outside execution.allowed_tools", tool)
		}
	}
	return nil
}

func sourceFor(revision Revision, operation string) SourceRef {
	return SourceRef{
		Scope: revision.Scope, Selector: selectorKey(revision),
		ID: revision.ID, Revision: revision.Revision, Operation: operation,
	}
}

func selectorKey(revision Revision) string {
	selector := revision.Selector
	switch revision.Scope {
	case ScopePlatform:
		return "*"
	case ScopeTenant:
		return "tenant=" + selector.TenantID
	case ScopeOrganization:
		return "tenant=" + selector.TenantID + ";organization=" + selector.OrganizationID
	case ScopeRepository:
		return "tenant=" + selector.TenantID + ";organization=" +
			selector.OrganizationID + ";repository=" + selector.RepositoryID
	case ScopePath:
		return "tenant=" + selector.TenantID + ";organization=" +
			selector.OrganizationID + ";repository=" + selector.RepositoryID +
			";path=" + selector.PathPrefix
	case ScopeInvocation:
		return "tenant=" + selector.TenantID + ";organization=" +
			selector.OrganizationID + ";repository=" + selector.RepositoryID +
			";invocation=" + selector.InvocationID
	default:
		return "invalid"
	}
}

func scopeRank(scope Scope) int {
	switch scope {
	case ScopePlatform:
		return 0
	case ScopeTenant:
		return 1
	case ScopeOrganization:
		return 2
	case ScopeRepository:
		return 3
	case ScopePath:
		return 4
	case ScopeInvocation:
		return 5
	default:
		return 99
	}
}

func indexRule(rules []RuleDefinition, id string) int {
	for index := range rules {
		if rules[index].ID == id {
			return index
		}
	}
	return -1
}

func indexProvider(providers []ContextProviderDefinition, id string) int {
	for index := range providers {
		if providers[index].ID == id {
			return index
		}
	}
	return -1
}

func indexAgentSkillPack(packs []AgentSkillPackDefinition, id string) int {
	for index := range packs {
		if packs[index].ID == id {
			return index
		}
	}
	return -1
}

func indexAgentKnowledgePack(packs []AgentKnowledgePackDefinition, id string) int {
	for index := range packs {
		if packs[index].ID == id {
			return index
		}
	}
	return -1
}

func redactionRank(level RedactionLevel) int {
	switch level {
	case RedactionNone:
		return 0
	case RedactionBasic:
		return 1
	case RedactionStrict:
		return 2
	default:
		return -1
	}
}

func firstOverlap(left, right []string) string {
	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	for _, value := range left {
		if _, exists := rightSet[value]; exists {
			return value
		}
	}
	return ""
}

func isOrderedStableIDField(field string) bool {
	return strings.HasPrefix(field, "rule_pack.rules[") ||
		strings.HasPrefix(field, "execution.context_providers[") ||
		strings.HasPrefix(field, "agent_review.skill_packs[") ||
		strings.HasPrefix(field, "agent_review.knowledge_packs[")
}
