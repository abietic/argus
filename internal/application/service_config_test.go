package application

import (
	"strings"
	"testing"

	"argus.local/argus/internal/configdefaults"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/workflow"
)

func TestRuntimeConfigUsesFrozenOutputBudgetForPatchAdmission(t *testing.T) {
	bundle, err := DefaultConfigBundle(
		DefaultLocalConfig(),
		workflow.DefaultReviewDefinition(),
	)
	if err != nil {
		t.Fatalf("DefaultConfigBundle() error = %v", err)
	}
	bundle.Target.MaxPatchBytes = 1024
	resealConfigBundle(t, &bundle)

	config, err := runtimeConfigFromBundle(bundle, DefaultLocalConfig())
	if err != nil {
		t.Fatalf("runtimeConfigFromBundle() error = %v", err)
	}
	if config.MaxPatchBytes != 1024 {
		t.Fatalf("MaxPatchBytes = %d, want 1024", config.MaxPatchBytes)
	}
}

func TestDeterministicRuntimeRejectsAgentReviewPolicy(t *testing.T) {
	definition := workflow.DefaultReviewDefinition()
	base := DefaultLocalConfig()
	revision, err := configdefaults.Revision(configdefaults.Options{
		ID: "agent-enabled", Revision: "1", MaxFiles: base.MaxFiles,
		MaxPatchBytes: base.MaxPatchBytes, MaxInputBytes: base.MaxMaterializedBytes,
		MaxOutputBytes: definition.Stages[0].Budget.MaxOutputBytes,
		MaxAttempts:    base.MaxAttempts, AllowedModes: []string{"diff", "scope", "selection"},
		TargetInclude: []string{"**"}, TargetExclude: []string{},
	}, definition)
	if err != nil {
		t.Fatalf("configdefaults.Revision() error = %v", err)
	}
	ref := func(id string) reviewconfig.VersionedRef {
		return reviewconfig.VersionedRef{
			ID: id, Revision: "1", SHA256: strings.Repeat("a", 64),
		}
	}
	agent, provider, model := ref("agent"), ref("provider"), ref("model")
	normalization := reviewconfig.VersionedRef{
		ID: "candidate-normalization", Revision: "v2", SHA256: agent.SHA256,
	}
	prompt, protocol := ref("prompt"), ref("anthropic-protocol")
	modelEgress := reviewconfig.AgentModelEgressProviderBrokerOnly
	deny := reviewconfig.PermissionDeny
	reads := reviewconfig.AgentWorkspaceReadsFrozenInputOnly
	zero, one := 0, 1
	one64 := int64(1)
	revision.Patch.AgentReview = &reviewconfig.AgentReviewPatch{
		Agent: &agent, Normalization: &normalization,
		Provider: &provider, Model: &model, Prompt: &prompt,
		APIProtocol: &protocol,
		SkillPacks: &reviewconfig.OrderedAgentSkillPackPatch{
			Upsert: []reviewconfig.AgentSkillPackDefinition{{
				ID: "correctness", Phase: reviewconfig.AgentSkillPhaseReview,
				Ref: ref("correctness"),
			}}, Remove: []string{},
		},
		KnowledgePacks: &reviewconfig.OrderedAgentKnowledgePackPatch{
			Upsert: []reviewconfig.AgentKnowledgePackDefinition{}, Remove: []string{},
		},
		Authority: &reviewconfig.AgentAuthorityPatch{
			ModelEgress: &modelEgress, ToolNetwork: &deny, WorkspaceReads: &reads,
			WorkspaceWrites: &deny, RemoteWrites: &deny, MaxDelegationDepth: &zero,
			Tools: &reviewconfig.SetPatch{Add: []string{}, Remove: []string{}},
		},
		Budget: &reviewconfig.AgentBudgetPatch{
			MaxFiles: &one, MaxGroups: &one, MaxHypotheses: &one,
			MaxModelCalls: &one, MaxToolCalls: &one,
			MaxTargetBytes: &one64, MaxGroupBytes: &one64, MaxOutputBytes: &one64,
			MaxOutputTokens: &one64, MaxCostMicros: &one64, TimeoutMS: &one64,
			MaxConcurrency: &one,
		},
	}
	bundle, err := reviewconfig.Resolve(reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "local",
		InvocationID: "agent-policy-test",
	}, []reviewconfig.Revision{revision})
	if err != nil {
		t.Fatalf("reviewconfig.Resolve() error = %v", err)
	}
	if _, err := runtimeConfigFromBundle(bundle, DefaultLocalConfig()); err == nil ||
		!strings.Contains(err.Error(), "cannot silently ignore") {
		t.Fatalf("runtimeConfigFromBundle(agent policy) error = %v", err)
	}
}

func TestRuntimePolicyRejectsClaimedExternalExecutionProfile(t *testing.T) {
	bundle, err := DefaultConfigBundle(
		DefaultLocalConfig(),
		workflow.DefaultReviewDefinition(),
	)
	if err != nil {
		t.Fatalf("DefaultConfigBundle() error = %v", err)
	}
	bundle.Execution.AgentProfile.ID = "claimed-external-agent"
	bundle.Execution.AgentProfile.SHA256 = strings.Repeat("a", 64)
	resealConfigBundle(t, &bundle)

	_, err = runtimePolicyFromBundle(bundle)
	if err == nil || !strings.Contains(
		err.Error(),
		"exact deterministic-local execution profile",
	) {
		t.Fatalf("runtimePolicyFromBundle() error = %v", err)
	}
}

func TestRuntimeEnvelopeRejectsConfigLargerThanWorkflow(t *testing.T) {
	definition := workflow.DefaultReviewDefinition()
	bundle, err := DefaultConfigBundle(DefaultLocalConfig(), definition)
	if err != nil {
		t.Fatalf("DefaultConfigBundle() error = %v", err)
	}
	bundle.Budget.MaxAttempts = definition.Stages[0].Retry.MaxAttempts + 1
	resealConfigBundle(t, &bundle)

	err = validateRuntimeEnvelope(bundle, definition)
	if err == nil || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("validateRuntimeEnvelope() error = %v, want attempts rejection", err)
	}
}

func resealConfigBundle(t *testing.T, bundle *reviewconfig.ConfigBundle) {
	t.Helper()
	bundle.BundleID = ""
	bundle.SHA256 = ""
	digest, err := reviewconfig.DigestBundle(*bundle)
	if err != nil {
		t.Fatalf("DigestBundle() error = %v", err)
	}
	bundle.SHA256 = digest
	bundle.BundleID = "bundle-" + digest[:24]
	if err := bundle.Validate(); err != nil {
		t.Fatalf("mutated ConfigBundle.Validate() error = %v", err)
	}
}
