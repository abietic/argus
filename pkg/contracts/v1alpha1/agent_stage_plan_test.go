package v1alpha1

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestAgentStagePlanSealStrictRoundTripCanonicalToolsAndGovernedOrder(t *testing.T) {
	input := validAgentStagePlanInput()
	sealed, err := SealAgentStagePlan(input)
	if err != nil {
		t.Fatalf("SealAgentStagePlan() error = %v", err)
	}
	data, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAgentStagePlan(data)
	if err != nil {
		t.Fatalf("DecodeAgentStagePlan() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, sealed) {
		t.Fatalf("decoded plan differs from sealed plan\n got: %#v\nwant: %#v", decoded, sealed)
	}

	toolReordered := validAgentStagePlanInput()
	slices.Reverse(toolReordered.ToolAuthority.Tools)
	resealed, err := SealAgentStagePlan(toolReordered)
	if err != nil {
		t.Fatalf("SealAgentStagePlan(reordered) error = %v", err)
	}
	reorderedData, err := json.Marshal(resealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(reorderedData) != string(data) {
		t.Fatalf("same ordered closure did not produce byte-identical JSON\n got: %s\nwant: %s", reorderedData, data)
	}

	orderChanged := validAgentStagePlanInput()
	slices.Reverse(orderChanged.Skills)
	slices.Reverse(orderChanged.Knowledge)
	slices.Reverse(orderChanged.ContextProviders)
	orderChangedSealed := mustSealAgentStagePlan(t, orderChanged)
	if orderChangedSealed.BehaviorSHA256 == sealed.BehaviorSHA256 ||
		orderChangedSealed.SHA256 == sealed.SHA256 {
		t.Fatal("ordered-stable-ID sequence change must alter behavior and full identity")
	}
	if orderChangedSealed.Skills[0].ID != "review-default" ||
		orderChangedSealed.Knowledge[0].ID != "business-b" ||
		orderChangedSealed.ContextProviders[0].ID != "context-b" {
		t.Fatal("SealAgentStagePlan did not preserve governed component order")
	}
	if input.Skills[0].Ref.ID != "skill-grouping" {
		t.Fatal("SealAgentStagePlan mutated the caller's slices")
	}
}

func TestDecodeAgentStagePlanRejectsUnknownDuplicateTrailingAndNull(t *testing.T) {
	sealed := mustSealAgentStagePlan(t, validAgentStagePlanInput())
	data, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "unknown field",
			data: append(
				append([]byte{}, data[:len(data)-1]...),
				[]byte(`,"raw_prompt":"forbidden"}`)...,
			),
			want: "unknown field",
		},
		{
			name: "duplicate field",
			data: append(
				append([]byte{}, data[:len(data)-1]...),
				[]byte(`,"side_effects":"deny"}`)...,
			),
			want: "duplicate field",
		},
		{
			name: "trailing value",
			data: append(append([]byte{}, data...), []byte(` {}`)...),
			want: "multiple JSON values",
		},
		{
			name: "explicit null",
			data: []byte(strings.Replace(
				string(data),
				`"skills":[{`,
				`"skills":null,"discarded_skills":[{`,
				1,
			)),
			want: "null",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeAgentStagePlan(test.data); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeAgentStagePlan() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentStagePlanRejectsOrderDuplicatesClosureAndSealTampering(t *testing.T) {
	valid := mustSealAgentStagePlan(t, validAgentStagePlanInput())
	tests := []struct {
		name   string
		mutate func(*AgentStagePlan)
		want   string
	}{
		{
			name: "empty skills",
			mutate: func(plan *AgentStagePlan) {
				plan.Skills = []AgentStageSkillBinding{}
			},
			want: "non-empty array",
		},
		{
			name: "no review phase skill",
			mutate: func(plan *AgentStagePlan) {
				for index := range plan.Skills {
					plan.Skills[index].Phase = AgentStageSkillPhaseContext
				}
			},
			want: "at least one review phase",
		},
		{
			name: "non-adjacent duplicate skill id",
			mutate: func(plan *AgentStagePlan) {
				plan.Skills[2].ID = plan.Skills[0].ID
			},
			want: "duplicate id",
		},
		{
			name: "duplicate knowledge id",
			mutate: func(plan *AgentStagePlan) {
				plan.Knowledge[1].ID = plan.Knowledge[0].ID
			},
			want: "duplicate id",
		},
		{
			name: "duplicate context provider id",
			mutate: func(plan *AgentStagePlan) {
				plan.ContextProviders[1].ID = plan.ContextProviders[0].ID
			},
			want: "duplicate id",
		},
		{
			name: "nil knowledge",
			mutate: func(plan *AgentStagePlan) {
				plan.Knowledge = nil
			},
			want: "explicit array",
		},
		{
			name: "component closure mismatch",
			mutate: func(plan *AgentStagePlan) {
				plan.Agent.Artifact.Ref.SHA256 = digestOf('0')
			},
			want: "same content SHA-256",
		},
		{
			name: "target and review input mismatch",
			mutate: func(plan *AgentStagePlan) {
				plan.TargetDigest = digestOf('0')
			},
			want: "must equal review_input.ref.sha256",
		},
		{
			name: "component contract mismatch",
			mutate: func(plan *AgentStagePlan) {
				plan.Prompt.Artifact.Contract = AgentStagePlanSkillContract
			},
			want: AgentStagePlanPromptContract,
		},
		{
			name: "mutable latest component",
			mutate: func(plan *AgentStagePlan) {
				plan.Model.Ref.Revision = "latest"
			},
			want: "must not use latest",
		},
		{
			name: "behavior digest tamper",
			mutate: func(plan *AgentStagePlan) {
				plan.BehaviorSHA256 = digestOf('0')
			},
			want: "behavior_sha256",
		},
		{
			name: "full digest tamper",
			mutate: func(plan *AgentStagePlan) {
				plan.SHA256 = digestOf('0')
			},
			want: "sha256 does not match",
		},
		{
			name: "plan id tamper",
			mutate: func(plan *AgentStagePlan) {
				plan.PlanID = "agent-stage-plan-000000000000000000000000"
			},
			want: "plan_id must be",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneAgentStagePlan(valid)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentStagePlanRejectsNonJSONSafeWireLimits(t *testing.T) {
	unsafeLimit := agentStageMaxJSONInteger + 1
	unsafeCount := int(unsafeLimit)
	if int64(unsafeCount) != unsafeLimit {
		t.Skip("platform int cannot represent a value above the JSON-safe boundary")
	}
	tests := []struct {
		name   string
		mutate func(*AgentStagePlan)
	}{
		{"max_files", func(plan *AgentStagePlan) { plan.Budget.MaxFiles = unsafeCount }},
		{"max_groups", func(plan *AgentStagePlan) { plan.Budget.MaxGroups = unsafeCount }},
		{"max_hypotheses", func(plan *AgentStagePlan) { plan.Budget.MaxHypotheses = unsafeCount }},
		{"max_model_calls", func(plan *AgentStagePlan) { plan.Budget.MaxModelCalls = unsafeCount }},
		{"max_tool_calls", func(plan *AgentStagePlan) { plan.Budget.MaxToolCalls = unsafeCount }},
		{"max_target_bytes", func(plan *AgentStagePlan) { plan.Budget.MaxTargetBytes = unsafeLimit }},
		{"max_group_bytes", func(plan *AgentStagePlan) { plan.Budget.MaxGroupBytes = unsafeLimit }},
		{"max_output_bytes", func(plan *AgentStagePlan) { plan.Budget.MaxOutputBytes = unsafeLimit }},
		{"max_output_tokens", func(plan *AgentStagePlan) { plan.Budget.MaxOutputTokens = unsafeLimit }},
		{"max_cost_micros", func(plan *AgentStagePlan) { plan.Budget.MaxCostMicros = unsafeLimit }},
		{"timeout_ms", func(plan *AgentStagePlan) { plan.Budget.TimeoutMS = unsafeLimit }},
		{"max_concurrency", func(plan *AgentStagePlan) { plan.Budget.MaxConcurrency = unsafeCount }},
		{"artifact size", func(plan *AgentStagePlan) {
			plan.ExecutionSnapshot.Ref.SizeBytes = unsafeLimit
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := validAgentStagePlanInput()
			test.mutate(&plan)
			if _, err := SealAgentStagePlan(plan); err == nil ||
				!strings.Contains(err.Error(), "JSON safe integer") {
				t.Fatalf("SealAgentStagePlan() error = %v", err)
			}
		})
	}

	boundary := validAgentStagePlanInput()
	maxLimit := agentStageMaxJSONInteger
	maxCount := int(maxLimit)
	boundary.Budget = AgentStageBudget{
		MaxFiles: maxCount, MaxGroups: maxCount, MaxHypotheses: maxCount,
		MaxModelCalls: maxCount, MaxToolCalls: maxCount,
		MaxTargetBytes:  maxLimit,
		MaxGroupBytes:   maxLimit,
		MaxOutputBytes:  maxLimit,
		MaxOutputTokens: maxLimit,
		MaxCostMicros:   maxLimit,
		TimeoutMS:       maxLimit,
		MaxConcurrency:  maxCount,
	}
	boundary.ExecutionSnapshot.Ref.SizeBytes = maxLimit
	if _, err := SealAgentStagePlan(boundary); err != nil {
		t.Fatalf("SealAgentStagePlan(max safe limits) error = %v", err)
	}
}

func TestAgentStagePlanBehaviorDigestCoversStageTargetPolicyAndEveryComponent(t *testing.T) {
	base := validAgentStagePlanInput()
	baseBehavior := mustSealAgentStagePlan(t, base).BehaviorSHA256
	tests := []struct {
		name   string
		mutate func(*AgentStagePlan)
	}{
		{"stage", func(plan *AgentStagePlan) { plan.Stage.Revision = "2" }},
		{"target and review input", func(plan *AgentStagePlan) {
			plan.TargetDigest = digestOf('0')
			plan.ReviewInput.Ref.SHA256 = digestOf('0')
		}},
		{"build identity", func(plan *AgentStagePlan) { plan.BuildIdentity = "build-2" }},
		{"policy", func(plan *AgentStagePlan) { plan.AgentReviewPolicy.Revision = "2" }},
		{"agent", func(plan *AgentStagePlan) {
			mutateComponentDigest(&plan.Agent, '0')
			plan.Normalization.SHA256 = plan.Agent.Ref.SHA256
		}},
		{"normalization", func(plan *AgentStagePlan) { plan.Normalization.Revision = CandidateNormalizationRevisionV1 }},
		{"provider", func(plan *AgentStagePlan) { mutateComponentDigest(&plan.Provider, '0') }},
		{"model", func(plan *AgentStagePlan) { mutateComponentDigest(&plan.Model, '0') }},
		{"runtime", func(plan *AgentStagePlan) { mutateComponentDigest(&plan.Runtime, '0') }},
		{"prompt", func(plan *AgentStagePlan) { mutateComponentDigest(&plan.Prompt, '0') }},
		{"skill", func(plan *AgentStagePlan) { mutateSkillDigest(&plan.Skills[0], '0') }},
		{"knowledge", func(plan *AgentStagePlan) { mutateKnowledgeDigest(&plan.Knowledge[0], '0') }},
		{"context provider", func(plan *AgentStagePlan) { mutateComponentDigest(&plan.ContextProviders[0].Adapter, '0') }},
		{"api protocol", func(plan *AgentStagePlan) { mutateComponentDigest(&plan.ModelAuthority.APIProtocol, '0') }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneAgentStagePlan(base)
			test.mutate(&candidate)
			sealed := mustSealAgentStagePlan(t, candidate)
			if sealed.BehaviorSHA256 == baseBehavior {
				t.Fatalf("%s mutation did not change behavior_sha256", test.name)
			}
		})
	}
}

func TestAgentStagePlanBehaviorDigestCoversEveryBudgetAndAuthority(t *testing.T) {
	base := validAgentStagePlanInput()
	baseBehavior := mustSealAgentStagePlan(t, base).BehaviorSHA256
	tests := []struct {
		name   string
		mutate func(*AgentStagePlan)
	}{
		{"max files", func(plan *AgentStagePlan) { plan.Budget.MaxFiles++ }},
		{"max groups", func(plan *AgentStagePlan) { plan.Budget.MaxGroups++ }},
		{"max hypotheses", func(plan *AgentStagePlan) { plan.Budget.MaxHypotheses++ }},
		{"max model calls", func(plan *AgentStagePlan) { plan.Budget.MaxModelCalls++ }},
		{"max tool calls", func(plan *AgentStagePlan) { plan.Budget.MaxToolCalls++ }},
		{"max target bytes", func(plan *AgentStagePlan) { plan.Budget.MaxTargetBytes++ }},
		{"max group bytes", func(plan *AgentStagePlan) { plan.Budget.MaxGroupBytes++ }},
		{"max output bytes", func(plan *AgentStagePlan) { plan.Budget.MaxOutputBytes++ }},
		{"max output tokens", func(plan *AgentStagePlan) { plan.Budget.MaxOutputTokens++ }},
		{"max cost", func(plan *AgentStagePlan) { plan.Budget.MaxCostMicros++ }},
		{"timeout", func(plan *AgentStagePlan) { plan.Budget.TimeoutMS++ }},
		{"concurrency", func(plan *AgentStagePlan) { plan.Budget.MaxConcurrency++ }},
		{"allowed tools", func(plan *AgentStagePlan) {
			plan.ToolAuthority.Tools = []string{"read_file", "search_symbols", "type_lookup"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneAgentStagePlan(base)
			test.mutate(&candidate)
			sealed := mustSealAgentStagePlan(t, candidate)
			if sealed.BehaviorSHA256 == baseBehavior {
				t.Fatalf("%s mutation did not change behavior_sha256", test.name)
			}
		})
	}
}

func TestAgentStagePlanBehaviorDigestExcludesRunAndPublicationLineage(t *testing.T) {
	base := mustSealAgentStagePlan(t, validAgentStagePlanInput())
	candidate := validAgentStagePlanInput()
	candidate.ReviewRunID = "run-other"
	publicationBindings := []*ArtifactBinding{
		&candidate.ExecutionSnapshot,
		&candidate.ConfigBundle,
		&candidate.ConfigResolutionReceiptRef,
		&candidate.Workflow,
		&candidate.ReviewSpec,
	}
	for index, binding := range publicationBindings {
		binding.Ref.URI = "artifact://other/publication-lineage-" + string(rune('a'+index))
		binding.Ref.SHA256 = digestOf(byte('0' + index))
		binding.Ref.SizeBytes++
	}
	lineageURIOnlyBindings := []*ArtifactBinding{
		&candidate.ReviewInput,
		&candidate.Agent.Artifact,
		&candidate.Provider.Artifact,
		&candidate.Model.Artifact,
		&candidate.Runtime.Artifact,
		&candidate.Prompt.Artifact,
		&candidate.ModelAuthority.APIProtocol.Artifact,
	}
	for index, binding := range lineageURIOnlyBindings {
		binding.Ref.URI = "artifact://other/lineage-" + string(rune('a'+index))
	}
	for index := range candidate.Skills {
		candidate.Skills[index].Artifact.Ref.URI =
			"artifact://other/skill-lineage-" + string(rune('a'+index))
	}
	for index := range candidate.Knowledge {
		candidate.Knowledge[index].Artifact.Ref.URI =
			"artifact://other/knowledge-lineage-" + string(rune('a'+index))
	}
	for index := range candidate.ContextProviders {
		candidate.ContextProviders[index].Adapter.Artifact.Ref.URI =
			"artifact://other/context-lineage-" + string(rune('a'+index))
	}
	changed := mustSealAgentStagePlan(t, candidate)
	if changed.BehaviorSHA256 != base.BehaviorSHA256 {
		t.Fatalf("lineage-only change altered behavior_sha256: got %s want %s", changed.BehaviorSHA256, base.BehaviorSHA256)
	}
	if changed.SHA256 == base.SHA256 || changed.PlanID == base.PlanID {
		t.Fatal("lineage-only change must alter full content identity")
	}
}

func TestAgentStagePlanAuthorityAndOutputFailClosed(t *testing.T) {
	base := validAgentStagePlanInput()
	tests := []struct {
		name   string
		mutate func(*AgentStagePlan)
		want   string
	}{
		{"direct model", func(plan *AgentStagePlan) { plan.ModelAuthority.ModelEgress = "direct_provider" }, "provider_broker_only"},
		{"tool network", func(plan *AgentStagePlan) { plan.ToolAuthority.ToolNetwork = "allow" }, "must be deny"},
		{"workspace read", func(plan *AgentStagePlan) { plan.ToolAuthority.WorkspaceReads = "repository" }, "frozen_input_only"},
		{"workspace write", func(plan *AgentStagePlan) { plan.ToolAuthority.WorkspaceWrites = "allow" }, "must be deny"},
		{"remote write", func(plan *AgentStagePlan) { plan.ToolAuthority.RemoteWrites = "allow" }, "must be deny"},
		{"delegation", func(plan *AgentStagePlan) { plan.ToolAuthority.MaxDelegationDepth = 1 }, "must be zero"},
		{"output", func(plan *AgentStagePlan) { plan.OutputContract = "argus.candidate_finding.v1alpha1" }, AgentStagePlanOutputContract},
		{"disposition", func(plan *AgentStagePlan) { plan.Disposition = "finding" }, "hypothesis_only"},
		{"side effects", func(plan *AgentStagePlan) { plan.SideEffects = "allow" }, "side_effects"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneAgentStagePlan(base)
			test.mutate(&candidate)
			if _, err := SealAgentStagePlan(candidate); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("SealAgentStagePlan() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentStagePlanWireHasNoFindingCandidateOrSensitiveRuntimeFields(t *testing.T) {
	sealed := mustSealAgentStagePlan(t, validAgentStagePlanInput())
	data, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	wire := strings.ToLower(string(data))
	for _, forbidden := range []string{
		"candidate_id", "finding", "raw_prompt", "secret", "endpoint",
		"repository_path", "created_at", "recorded_at", "timestamp", `"source"`,
	} {
		if strings.Contains(wire, forbidden) {
			t.Fatalf("AgentStagePlan wire contains forbidden field/semantic %q: %s", forbidden, data)
		}
	}
	if _, err := DecodeStageExecutionResult(data); err == nil {
		t.Fatal("StageExecutionResult decoder accepted AgentStagePlan")
	}
}

func validAgentStagePlanInput() AgentStagePlan {
	rulePackRef, rulePackBase64 := validStagePlanRulePack()
	return AgentStagePlan{
		SchemaVersion:     AgentStagePlanSchemaVersion,
		ReviewRunID:       "run-1",
		Stage:             VersionedRef{ID: "agent_hypothesize", Revision: "1", SHA256: digestOf('a')},
		TargetDigest:      digestOf('1'),
		BuildIdentity:     "pi-runtime-build-1",
		ExecutionSnapshot: stagePlanArtifact("artifact://local/execution-snapshot", digestOf('c'), AgentStagePlanExecutionSnapshotContract),
		ConfigBundle:      stagePlanArtifact("artifact://local/config-bundle", digestOf('d'), AgentStagePlanConfigBundleContract),
		ConfigResolutionReceiptRef: stagePlanArtifact(
			"artifact://local/config-resolution-receipt",
			digestOf('e'),
			AgentStagePlanConfigResolutionReceiptContract,
		),
		Workflow:          stagePlanArtifact("artifact://local/workflow", digestOf('e'), AgentStagePlanWorkflowContract),
		ReviewSpec:        stagePlanArtifact("artifact://local/review-spec", digestOf('f'), AgentStagePlanReviewSpecContract),
		ReviewInput:       stagePlanArtifact("artifact://local/review-input", digestOf('1'), AgentStagePlanReviewInputContract),
		AgentReviewPolicy: VersionedRef{ID: "formal-review", Revision: "1", SHA256: digestOf('2')},
		RulePack:          rulePackRef,
		Normalization: VersionedRef{
			ID: CandidateNormalizationImplementationID, Revision: CandidateNormalizationCurrentRevision,
			SHA256: digestOf('3'),
		},
		RulePackBase64: rulePackBase64,
		Agent:          stagePlanComponent("agent", "1", digestOf('3'), AgentStagePlanAgentContract),
		Provider:       stagePlanComponent("provider", "1", digestOf('4'), AgentStagePlanProviderContract),
		Model:          stagePlanComponent("model", "1", digestOf('5'), AgentStagePlanModelContract),
		Runtime:        stagePlanComponent("pi-runtime", "1", digestOf('5'), AgentStagePlanRuntimeContract),
		Prompt:         stagePlanComponent("prompt", "1", digestOf('6'), AgentStagePlanPromptContract),
		Skills: []AgentStageSkillBinding{
			stagePlanSkill(AgentStageSkillPhaseGrouping, "grouping-default", "skill-grouping", digestOf('6')),
			stagePlanSkill(AgentStageSkillPhaseContext, "context-default", "skill-context", digestOf('7')),
			stagePlanSkill(AgentStageSkillPhaseReview, "review-default", "skill-review", digestOf('8')),
		},
		Knowledge: []AgentStageKnowledgeBinding{
			stagePlanKnowledge("business-a", "knowledge-a", digestOf('9')),
			stagePlanKnowledge("business-b", "knowledge-b", digestOf('a')),
		},
		ContextProviders: []AgentStageContextProviderBinding{
			stagePlanContextProvider("context-a", "repository_search", "adapter-a", digestOf('b')),
			stagePlanContextProvider("context-b", "codegraph", "adapter-b", digestOf('c')),
		},
		ModelAuthority: AgentStageModelAuthority{
			ModelEgress: AgentStageModelEgressProviderBrokerOnly,
			APIProtocol: stagePlanComponent(
				"anthropic-messages",
				"2023-06-01",
				digestOf('d'),
				AgentStagePlanAPIProtocolContract,
			),
		},
		ToolAuthority: AgentStageToolAuthority{
			Tools:              []string{"read_file", "search_symbols"},
			ToolNetwork:        AgentStageSideEffectsDeny,
			WorkspaceReads:     AgentStageWorkspaceReadFrozenInputOnly,
			WorkspaceWrites:    AgentStageSideEffectsDeny,
			RemoteWrites:       AgentStageSideEffectsDeny,
			MaxDelegationDepth: 0,
		},
		Budget: AgentStageBudget{
			MaxFiles: 20, MaxGroups: 4, MaxHypotheses: 50,
			MaxModelCalls: 16, MaxToolCalls: 64,
			MaxTargetBytes: 4 << 20, MaxGroupBytes: 1 << 20,
			MaxOutputBytes: 1 << 20, MaxOutputTokens: 100_000,
			MaxCostMicros: 2_000_000,
			TimeoutMS:     120_000, MaxConcurrency: 2,
		},
		OutputContract: AgentStagePlanOutputContract,
		Disposition:    AgentStageDispositionHypothesisOnly,
		SideEffects:    AgentStageSideEffectsDeny,
	}
}

func validStagePlanRulePack() (VersionedRef, string) {
	pack := struct {
		SchemaVersion string          `json:"schema_version"`
		ID            string          `json:"id"`
		Revision      string          `json:"revision"`
		Rules         json.RawMessage `json:"rules"`
		SHA256        string          `json:"sha256"`
	}{
		SchemaVersion: "argus.rule_pack.v1alpha1", ID: "review-rules",
		Revision: "1", Rules: json.RawMessage(`[]`),
	}
	semantic, _ := json.Marshal(pack)
	digest := sha256.Sum256(semantic)
	pack.SHA256 = hex.EncodeToString(digest[:])
	content, _ := json.Marshal(pack)
	return VersionedRef{ID: pack.ID, Revision: pack.Revision, SHA256: pack.SHA256},
		base64.StdEncoding.EncodeToString(content)
}

func stagePlanArtifact(uri, digest, contract string) ArtifactBinding {
	return ArtifactBinding{
		Ref:      ContentRef{URI: uri, SHA256: digest, SizeBytes: 100},
		Contract: contract,
	}
}

func stagePlanComponent(id, revision, digest, contract string) AgentStageComponentBinding {
	return AgentStageComponentBinding{
		Ref:      VersionedRef{ID: id, Revision: revision, SHA256: digest},
		Artifact: stagePlanArtifact("artifact://local/"+id, digest, contract),
	}
}

func stagePlanSkill(
	phase AgentStageSkillPhase,
	id,
	refID,
	digest string,
) AgentStageSkillBinding {
	component := stagePlanComponent(refID, "1", digest, AgentStagePlanSkillContract)
	return AgentStageSkillBinding{
		ID: id, Phase: phase, Ref: component.Ref, Artifact: component.Artifact,
	}
}

func stagePlanKnowledge(id, refID, digest string) AgentStageKnowledgeBinding {
	component := stagePlanComponent(refID, "1", digest, AgentStagePlanKnowledgeContract)
	return AgentStageKnowledgeBinding{ID: id, Ref: component.Ref, Artifact: component.Artifact}
}

func stagePlanContextProvider(
	id,
	kind,
	adapterID,
	digest string,
) AgentStageContextProviderBinding {
	return AgentStageContextProviderBinding{
		ID: id, Revision: "1", Kind: kind,
		Adapter: stagePlanComponent(
			adapterID,
			"1",
			digest,
			AgentStagePlanContextProviderAdapterContract,
		),
	}
}

func digestOf(character byte) string {
	return strings.Repeat(string(character), 64)
}

func mustSealAgentStagePlan(t *testing.T, input AgentStagePlan) AgentStagePlan {
	t.Helper()
	sealed, err := SealAgentStagePlan(input)
	if err != nil {
		t.Fatalf("SealAgentStagePlan() error = %v", err)
	}
	return sealed
}

func cloneAgentStagePlan(plan AgentStagePlan) AgentStagePlan {
	clone := plan
	clone.Skills = slices.Clone(plan.Skills)
	clone.Knowledge = slices.Clone(plan.Knowledge)
	clone.ContextProviders = slices.Clone(plan.ContextProviders)
	clone.ToolAuthority.Tools = slices.Clone(plan.ToolAuthority.Tools)
	return clone
}

func mutateComponentDigest(binding *AgentStageComponentBinding, character byte) {
	digest := digestOf(character)
	binding.Ref.SHA256 = digest
	binding.Artifact.Ref.SHA256 = digest
}

func mutateSkillDigest(binding *AgentStageSkillBinding, character byte) {
	digest := digestOf(character)
	binding.Ref.SHA256 = digest
	binding.Artifact.Ref.SHA256 = digest
}

func mutateKnowledgeDigest(binding *AgentStageKnowledgeBinding, character byte) {
	digest := digestOf(character)
	binding.Ref.SHA256 = digest
	binding.Artifact.Ref.SHA256 = digest
}
