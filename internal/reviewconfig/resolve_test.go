package reviewconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestValidatePatternMatchesExecutableScopeGrammar(t *testing.T) {
	for _, pattern := range []string{"**", "src/*.go", "源码/**/*.go"} {
		if err := validatePattern(pattern); err != nil {
			t.Fatalf("validatePattern(%q) error = %v", pattern, err)
		}
	}
	for _, pattern := range []string{
		"src/***",
		"src/?.go",
		"src/[ab].go",
		strings.Repeat("a", 1025),
	} {
		if err := validatePattern(pattern); err == nil {
			t.Fatalf("validatePattern(%q) accepted unsupported grammar", pattern)
		}
	}
}

func TestResolveSixScopesSpecificityAndInvocationOverride(t *testing.T) {
	context := testContext()
	revisions := []Revision{
		completePlatformRevision(),
		maxFilesRevision("tenant-config", ScopeTenant, Selector{
			TenantID: "tenant-1",
		}, 900),
		maxFilesRevision("organization-config", ScopeOrganization, Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
		}, 800),
		maxFilesRevision("repository-config", ScopeRepository, Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1",
		}, 700),
		maxFilesRevision("path-broad", ScopePath, Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1", PathPrefix: "internal",
		}, 600),
		maxFilesRevision("path-specific", ScopePath, Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1", PathPrefix: "internal/reviewconfig",
		}, 500),
		maxFilesRevision("invocation-config", ScopeInvocation, Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1", InvocationID: "invocation-1",
		}, 400),
	}

	bundle, err := Resolve(context, revisions)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if bundle.Target.MaxFiles != 400 {
		t.Fatalf("max_files = %d, want invocation override 400", bundle.Target.MaxFiles)
	}
	wantOrder := []Scope{
		ScopePlatform,
		ScopeTenant,
		ScopeOrganization,
		ScopeRepository,
		ScopePath,
		ScopePath,
		ScopeInvocation,
	}
	gotOrder := make([]Scope, 0, len(bundle.AppliedRevisions))
	for _, revision := range bundle.AppliedRevisions {
		gotOrder = append(gotOrder, revision.Scope)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("applied scope order = %v, want %v", gotOrder, wantOrder)
	}
	source := findFieldSource(t, bundle, "target.max_files")
	if len(source.Sources) != 1 ||
		source.Sources[0].ID != "invocation-config" ||
		source.Sources[0].Scope != ScopeInvocation {
		t.Fatalf("max_files source = %+v", source)
	}
	if countExplain(bundle, "target.max_files") != 7 {
		t.Fatalf("max_files explain count = %d, want 7", countExplain(bundle, "target.max_files"))
	}
}

func TestResolvePathSelectorUsesSegmentBoundary(t *testing.T) {
	context := testContext()
	context.Path = "internalized/file.go"
	revisions := []Revision{
		completePlatformRevision(),
		maxFilesRevision("path-config", ScopePath, Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1", PathPrefix: "internal",
		}, 1),
	}
	bundle, err := Resolve(context, revisions)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if bundle.Target.MaxFiles != 1000 || len(bundle.AppliedRevisions) != 1 {
		t.Fatalf("non-boundary path selector applied: %+v", bundle.AppliedRevisions)
	}
}

func TestResolveSetMergeAndDenyWins(t *testing.T) {
	base := completePlatformRevision()
	base.Patch.Execution.AllowedTools = &SetPatch{
		Add: []string{"ast"}, Remove: []string{},
	}
	base.Patch.Adjudication.AutomaticPublication = permissionPointer(PermissionAllow)
	base.Patch.Publication.RemoteWrites = permissionPointer(PermissionAllow)
	base.Patch.Publication.Channels = &SetPatch{
		Add: []string{"github"}, Remove: []string{},
	}
	ten := 10
	base.Patch.Publication.MaxComments = &ten
	base.Patch.Data.Training = permissionPointer(PermissionAllow)

	tenant := Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "tenant-deny", Revision: "1", Scope: ScopeTenant,
		Selector: Selector{TenantID: "tenant-1"},
		Patch: ConfigPatch{
			Execution: &ExecutionPatch{AllowedTools: &SetPatch{
				Add: []string{"grep"}, Remove: []string{},
			}},
			Adjudication: &AdjudicationPatch{
				AutomaticPublication: permissionPointer(PermissionDeny),
			},
			Publication: &PublicationPatch{
				RemoteWrites: permissionPointer(PermissionDeny),
			},
			Data: &DataPatch{Training: permissionPointer(PermissionDeny)},
		},
	}
	repository := Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "repository-tools", Revision: "1", Scope: ScopeRepository,
		Selector: Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1",
		},
		Patch: ConfigPatch{Execution: &ExecutionPatch{AllowedTools: &SetPatch{
			Add: []string{}, Remove: []string{"ast"},
		}}},
	}
	invocation := Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "invocation-allow", Revision: "1", Scope: ScopeInvocation,
		Selector: Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1", InvocationID: "invocation-1",
		},
		Patch: ConfigPatch{
			Execution: &ExecutionPatch{AllowedTools: &SetPatch{
				Add: []string{"lsp"}, Remove: []string{},
			}},
			Adjudication: &AdjudicationPatch{
				AutomaticPublication: permissionPointer(PermissionAllow),
			},
			Publication: &PublicationPatch{
				RemoteWrites: permissionPointer(PermissionAllow),
			},
			Data: &DataPatch{Training: permissionPointer(PermissionAllow)},
		},
	}

	bundle, err := Resolve(
		testContext(),
		[]Revision{invocation, repository, base, tenant},
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !reflect.DeepEqual(bundle.Execution.AllowedTools, []string{"grep", "lsp"}) {
		t.Fatalf("allowed tools = %v", bundle.Execution.AllowedTools)
	}
	if bundle.Adjudication.AutomaticPublication != PermissionDeny ||
		bundle.Publication.RemoteWrites != PermissionDeny ||
		bundle.Data.Training != PermissionDeny {
		t.Fatalf(
			"deny did not win: adjudication=%s publication=%s training=%s",
			bundle.Adjudication.AutomaticPublication,
			bundle.Publication.RemoteWrites,
			bundle.Data.Training,
		)
	}
	for _, field := range []string{
		"adjudication.automatic_publication",
		"publication.remote_writes",
		"data.training",
	} {
		source := findFieldSource(t, bundle, field)
		if source.Strategy != MergeDenyWins || len(source.Sources) != 3 {
			t.Fatalf("%s provenance = %+v", field, source)
		}
	}
}

func TestResolveStableRuleOverridePreservesOrder(t *testing.T) {
	base := completePlatformRevision()
	base.Patch.RulePack.Rules.Upsert = append(
		base.Patch.RulePack.Rules.Upsert,
		testRule("rule-b", "1", "medium"),
	)
	repository := Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "repository-rules", Revision: "1", Scope: ScopeRepository,
		Selector: Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1",
		},
		Patch: ConfigPatch{RulePack: &RulePackPatch{Rules: &OrderedRulePatch{
			Upsert: []RuleDefinition{testRule("rule-a", "2", "critical")},
			Remove: []string{},
		}}},
	}
	pathRevision := Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "path-rules", Revision: "1", Scope: ScopePath,
		Selector: Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1", PathPrefix: "internal",
		},
		Patch: ConfigPatch{RulePack: &RulePackPatch{Rules: &OrderedRulePatch{
			Upsert: []RuleDefinition{testRule("rule-c", "1", "low")},
			Remove: []string{"rule-b"},
		}}},
	}

	bundle, err := Resolve(testContext(), []Revision{pathRevision, repository, base})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(bundle.RulePack.Rules) != 2 ||
		bundle.RulePack.Rules[0].ID != "rule-a" ||
		bundle.RulePack.Rules[0].Revision != "2" ||
		bundle.RulePack.Rules[0].Severity != "critical" ||
		bundle.RulePack.Rules[1].ID != "rule-c" {
		t.Fatalf("effective rules = %+v", bundle.RulePack.Rules)
	}
	source := findFieldSource(t, bundle, "rule_pack.rules[rule-a]")
	if source.Sources[0].ID != "repository-rules" {
		t.Fatalf("rule-a source = %+v", source)
	}
	if err := bundle.RulePack.Validate(); err != nil {
		t.Fatalf("RulePack.Validate() error = %v", err)
	}
}

func TestResolveIsDeterministicAcrossInputOrder(t *testing.T) {
	revisions := []Revision{
		completePlatformRevision(),
		maxFilesRevision("tenant", ScopeTenant, Selector{TenantID: "tenant-1"}, 900),
		maxFilesRevision("repository", ScopeRepository, Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1",
		}, 800),
	}
	first, err := Resolve(testContext(), revisions)
	if err != nil {
		t.Fatalf("first Resolve() error = %v", err)
	}
	slices.Reverse(revisions)
	second, err := Resolve(testContext(), revisions)
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) || first.SHA256 != second.SHA256 ||
		first.BundleID != second.BundleID {
		t.Fatalf("resolution is input-order dependent:\nfirst=%+v\nsecond=%+v", first, second)
	}
	mutated := first
	mutated.Target.MaxFiles++
	if err := mutated.Validate(); err == nil ||
		!strings.Contains(err.Error(), "identity") {
		t.Fatalf("mutated immutable bundle validation error = %v", err)
	}
}

func TestResolveRejectsUnknownAmbiguousAndConflictingInput(t *testing.T) {
	t.Run("unknown JSON", func(t *testing.T) {
		data, err := json.Marshal(completePlatformRevision())
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.TrimSuffix(string(data), "}") + `,"unknown":true}`)
		if _, err := DecodeRevision(data); err == nil ||
			!strings.Contains(err.Error(), "unknown") {
			t.Fatalf("DecodeRevision() error = %v", err)
		}
	})
	t.Run("duplicate JSON", func(t *testing.T) {
		data, err := json.Marshal(completePlatformRevision())
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.Replace(
			string(data),
			`"id":"platform-default"`,
			`"id":"platform-default","id":"duplicate"`,
			1,
		))
		if _, err := DecodeRevision(data); err == nil ||
			!strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("DecodeRevision() error = %v", err)
		}
	})
	t.Run("ambiguous selector", func(t *testing.T) {
		first := maxFilesRevision(
			"tenant-a",
			ScopeTenant,
			Selector{TenantID: "tenant-1"},
			100,
		)
		second := maxFilesRevision(
			"tenant-b",
			ScopeTenant,
			Selector{TenantID: "tenant-1"},
			200,
		)
		if _, err := Resolve(
			testContext(),
			[]Revision{completePlatformRevision(), first, second},
		); err == nil || !strings.Contains(err.Error(), "same tenant selector") {
			t.Fatalf("Resolve() ambiguity error = %v", err)
		}
	})
	t.Run("set conflict", func(t *testing.T) {
		revision := maxFilesRevision(
			"tenant",
			ScopeTenant,
			Selector{TenantID: "tenant-1"},
			100,
		)
		revision.Patch.Target = &TargetPatch{AllowedModes: &SetPatch{
			Add: []string{"diff"}, Remove: []string{"diff"},
		}}
		if _, err := Resolve(
			testContext(),
			[]Revision{completePlatformRevision(), revision},
		); err == nil || !strings.Contains(err.Error(), "add and remove") {
			t.Fatalf("Resolve() conflict error = %v", err)
		}
	})
	t.Run("invalid selector", func(t *testing.T) {
		revision := maxFilesRevision(
			"bad",
			ScopeTenant,
			Selector{TenantID: "tenant-1", RepositoryID: "unexpected"},
			100,
		)
		if _, err := Resolve(
			testContext(),
			[]Revision{completePlatformRevision(), revision},
		); err == nil || !strings.Contains(err.Error(), "must not contain repository_id") {
			t.Fatalf("Resolve() selector error = %v", err)
		}
	})
}

func TestResolveFailsClosedOnIncompleteOrInconsistentPolicy(t *testing.T) {
	incomplete := Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "incomplete", Revision: "1", Scope: ScopePlatform,
		Selector: Selector{},
		Patch: ConfigPatch{Target: &TargetPatch{
			MaxFiles: intPointer(1),
		}},
	}
	if _, err := Resolve(testContext(), []Revision{incomplete}); err == nil ||
		!strings.Contains(err.Error(), "has no source") {
		t.Fatalf("incomplete Resolve() error = %v", err)
	}

	inconsistent := completePlatformRevision()
	inconsistent.Patch.Adjudication.AutomaticPublication =
		permissionPointer(PermissionAllow)
	if _, err := Resolve(testContext(), []Revision{inconsistent}); err == nil ||
		!strings.Contains(err.Error(), "automatic publication") {
		t.Fatalf("inconsistent Resolve() error = %v", err)
	}
}

func TestSecretMaterialCanOnlyBeReferenced(t *testing.T) {
	base := completePlatformRevision()
	base.Patch.Execution.ModelCredential = &SecretRef{
		URI: "secret://vault/model/provider-key",
	}
	bundle, err := Resolve(testContext(), []Revision{base})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "secret://vault/model/provider-key") {
		t.Fatalf("bundle omitted secret ref: %s", encoded)
	}

	for _, invalid := range []string{
		"plaintext-api-key",
		"https://vault/model/key",
		"secret://vault/../key",
	} {
		base.Patch.Execution.ModelCredential = &SecretRef{URI: invalid}
		if _, err := Resolve(testContext(), []Revision{base}); err == nil {
			t.Fatalf("Resolve() accepted invalid secret material/ref %q", invalid)
		}
	}
}

func TestResolveAgentReviewPolicyClosesAndBindsDigest(t *testing.T) {
	base := completePlatformRevision()
	base.Patch.Execution.AllowedTools = &SetPatch{
		Add: []string{"codegraph"}, Remove: []string{},
	}
	base.Patch.AgentReview = completeAgentReviewPatch()

	bundle, err := Resolve(testContext(), []Revision{base})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if bundle.AgentReview == nil {
		t.Fatal("Resolve() did not enable agent review")
	}
	policy := bundle.AgentReview
	if policy.SchemaVersion != AgentReviewPolicySchemaVersion ||
		policy.ID != "effective" || policy.Revision != "resolved" {
		t.Fatalf("agent policy identity = %+v", policy)
	}
	if len(policy.SkillPacks) != 3 || policy.SkillPacks[1].Phase != AgentSkillPhaseReview {
		t.Fatalf("agent skill packs = %+v", policy.SkillPacks)
	}
	if len(policy.KnowledgePacks) != 1 ||
		!reflect.DeepEqual(policy.Authority.Tools, []string{"codegraph"}) {
		t.Fatalf("agent policy packs/authority = %+v", policy)
	}
	for _, field := range []string{
		"agent_review.agent",
		"agent_review.skill_packs",
		"agent_review.skill_packs[review-core]",
		"agent_review.knowledge_packs",
		"agent_review.knowledge_packs[business-default]",
		"agent_review.authority.tool_network",
		"agent_review.budget.max_model_calls",
	} {
		findFieldSource(t, bundle, field)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("AgentReviewPolicy.Validate() error = %v", err)
	}
	mutated := bundle
	policyCopy := *bundle.AgentReview
	policyCopy.Budget.MaxModelCalls++
	mutated.AgentReview = &policyCopy
	if err := mutated.Validate(); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered agent policy validation error = %v", err)
	}
}

func TestResolveAgentReviewStablePackOverrideAndRemoval(t *testing.T) {
	base := completePlatformRevision()
	base.Patch.Execution.AllowedTools = &SetPatch{
		Add: []string{"codegraph", "lsp"}, Remove: []string{},
	}
	base.Patch.AgentReview = completeAgentReviewPatch()
	repository := Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "repository-agent", Revision: "1", Scope: ScopeRepository,
		Selector: Selector{
			TenantID: "tenant-1", OrganizationID: "organization-1",
			RepositoryID: "repository-1",
		},
		Patch: ConfigPatch{AgentReview: &AgentReviewPatch{
			SkillPacks: &OrderedAgentSkillPackPatch{
				Upsert: []AgentSkillPackDefinition{
					agentSkillPack("review-core", AgentSkillPhaseReview, "2"),
					agentSkillPack("review-security", AgentSkillPhaseReview, "1"),
				},
				Remove: []string{"group-changes"},
			},
			KnowledgePacks: &OrderedAgentKnowledgePackPatch{
				Upsert: []AgentKnowledgePackDefinition{agentKnowledgePack("repository-guide", "1")},
				Remove: []string{"business-default"},
			},
			Authority: &AgentAuthorityPatch{Tools: &SetPatch{
				Add: []string{"lsp"}, Remove: []string{"codegraph"},
			}},
		}},
	}

	first, err := Resolve(testContext(), []Revision{repository, base})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	second, err := Resolve(testContext(), []Revision{base, repository})
	if err != nil {
		t.Fatalf("Resolve(reverse) error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("agent policy resolution depends on input order")
	}
	policy := first.AgentReview
	if got := []string{
		policy.SkillPacks[0].ID,
		policy.SkillPacks[1].ID,
		policy.SkillPacks[2].ID,
	}; !reflect.DeepEqual(got, []string{"review-core", "verify-candidates", "review-security"}) ||
		policy.SkillPacks[0].Ref.Revision != "2" {
		t.Fatalf("stable skill pack order = %+v", policy.SkillPacks)
	}
	if len(policy.KnowledgePacks) != 1 || policy.KnowledgePacks[0].ID != "repository-guide" {
		t.Fatalf("knowledge packs = %+v", policy.KnowledgePacks)
	}
	if !reflect.DeepEqual(policy.Authority.Tools, []string{"lsp"}) {
		t.Fatalf("agent tools = %v", policy.Authority.Tools)
	}
	if findFieldSource(t, first, "agent_review.skill_packs[review-core]").Sources[0].ID !=
		"repository-agent" {
		t.Fatal("skill override source was not replaced")
	}
}

func TestResolveAgentReviewAllowsExplicitEmptyKnowledge(t *testing.T) {
	base := completePlatformRevision()
	patch := completeAgentReviewPatch()
	patch.KnowledgePacks = &OrderedAgentKnowledgePackPatch{
		Upsert: []AgentKnowledgePackDefinition{}, Remove: []string{},
	}
	patch.Authority.Tools = &SetPatch{Add: []string{}, Remove: []string{}}
	base.Patch.AgentReview = patch
	bundle, err := Resolve(testContext(), []Revision{base})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if bundle.AgentReview.KnowledgePacks == nil || len(bundle.AgentReview.KnowledgePacks) != 0 {
		t.Fatalf("explicit empty knowledge packs = %#v", bundle.AgentReview.KnowledgePacks)
	}
}

func TestResolveAgentReviewFailsClosed(t *testing.T) {
	t.Run("partial policy", func(t *testing.T) {
		base := completePlatformRevision()
		base.Patch.AgentReview = &AgentReviewPatch{Model: refPointer("model", "1")}
		if _, err := Resolve(testContext(), []Revision{base}); err == nil ||
			!strings.Contains(err.Error(), "agent_review.agent") {
			t.Fatalf("partial agent policy error = %v", err)
		}
	})

	t.Run("requires review phase", func(t *testing.T) {
		base := completePlatformRevision()
		patch := completeAgentReviewPatch()
		patch.SkillPacks = &OrderedAgentSkillPackPatch{
			Upsert: []AgentSkillPackDefinition{
				agentSkillPack("context", AgentSkillPhaseContext, "1"),
			},
			Remove: []string{},
		}
		patch.Authority.Tools = &SetPatch{Add: []string{}, Remove: []string{}}
		base.Patch.AgentReview = patch
		if _, err := Resolve(testContext(), []Revision{base}); err == nil ||
			!strings.Contains(err.Error(), "review phase") {
			t.Fatalf("missing review skill error = %v", err)
		}
	})

	t.Run("outer budget envelope", func(t *testing.T) {
		base := completePlatformRevision()
		patch := completeAgentReviewPatch()
		tooLarge := int64(2 << 20)
		patch.Budget.MaxTargetBytes = &tooLarge
		patch.Authority.Tools = &SetPatch{Add: []string{}, Remove: []string{}}
		base.Patch.AgentReview = patch
		if _, err := Resolve(testContext(), []Revision{base}); err == nil ||
			!strings.Contains(err.Error(), "max_target_bytes exceeds") {
			t.Fatalf("agent budget envelope error = %v", err)
		}
	})

	t.Run("S1 authority", func(t *testing.T) {
		base := completePlatformRevision()
		patch := completeAgentReviewPatch()
		patch.Authority.ToolNetwork = permissionPointer(PermissionAllow)
		base.Patch.AgentReview = patch
		if _, err := Resolve(testContext(), []Revision{base}); err == nil ||
			!strings.Contains(err.Error(), "tool_network must be deny") {
			t.Fatalf("agent authority error = %v", err)
		}
	})

	t.Run("floating version ref", func(t *testing.T) {
		base := completePlatformRevision()
		patch := completeAgentReviewPatch()
		patch.Model.Revision = "latest"
		base.Patch.AgentReview = patch
		if _, err := Resolve(testContext(), []Revision{base}); err == nil ||
			!strings.Contains(err.Error(), "must not use latest") {
			t.Fatalf("floating ref error = %v", err)
		}
	})
}

func TestAgentReviewStrictJSONDecode(t *testing.T) {
	base := completePlatformRevision()
	base.Patch.AgentReview = completeAgentReviewPatch()
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutated := range map[string][]byte{
		"null": bytes.Replace(
			data,
			[]byte(`"agent_review":{`),
			[]byte(`"agent_review":null,"discarded_agent_review":{`),
			1,
		),
		"trailing": append(slices.Clone(data), []byte(` {}`)...),
	} {
		if _, err := DecodeRevision(mutated); err == nil {
			t.Fatalf("DecodeRevision accepted %s JSON: %s", name, mutated)
		}
	}
}

func TestAgentReviewRevisionAndBundleMatchJSONSchema(t *testing.T) {
	base := completePlatformRevision()
	base.Patch.Execution.AllowedTools = &SetPatch{
		Add: []string{"codegraph"}, Remove: []string{},
	}
	base.Patch.AgentReview = completeAgentReviewPatch()
	bundle, err := Resolve(testContext(), []Revision{base})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	for _, contract := range []struct {
		name  string
		path  string
		value any
	}{
		{"revision", "../../api/schema/v1alpha1/config-revision.schema.json", base},
		{"bundle", "../../api/schema/v1alpha1/config-bundle.schema.json", bundle},
	} {
		schema, err := jsonschema.NewCompiler().Compile(contract.path)
		if err != nil {
			t.Fatalf("compile %s schema: %v", contract.name, err)
		}
		data, err := json.Marshal(contract.value)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("validate %s schema: %v", contract.name, err)
		}
	}
}

func TestSealRulePackCopiesInputAndBindsDigest(t *testing.T) {
	rules := []RuleDefinition{testRule("standalone", "1", "high")}
	pack, err := SealRulePack("go-default", "1", rules)
	if err != nil {
		t.Fatalf("SealRulePack() error = %v", err)
	}
	rules[0].Severity = "low"
	rules[0].Languages[0] = "java"
	if pack.Rules[0].Severity != "high" ||
		!reflect.DeepEqual(pack.Rules[0].Languages, []string{"go"}) {
		t.Fatalf("sealed RulePack aliases input: %+v", pack.Rules[0])
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("RulePack.Validate() error = %v", err)
	}
	pack.Rules[0].Severity = "critical"
	if err := pack.Validate(); err == nil ||
		!strings.Contains(err.Error(), "digest") {
		t.Fatalf("mutated RulePack validation error = %v", err)
	}
}

func TestFieldMergeStrategiesAreClosedAndExplicit(t *testing.T) {
	strategies := FieldMergeStrategies()
	for field, strategy := range strategies {
		switch strategy {
		case MergeOverride, MergeOrderedStableIDOverride, MergeSetUnionSubtract,
			MergeDenyWins, MergeReplaceOnly:
		default:
			t.Fatalf("field %q has unsupported merge strategy %q", field, strategy)
		}
	}
	if len(strategies) != 64 {
		t.Fatalf("declared field strategies = %d, want 64", len(strategies))
	}
	strategies["unknown"] = MergeOverride
	if _, leaked := FieldMergeStrategies()["unknown"]; leaked {
		t.Fatal("FieldMergeStrategies returned mutable internal state")
	}
}

func TestResolveFindingGovernanceFreezesProfileAndExplainsOverrides(t *testing.T) {
	profile, err := SealCalibrationProfile("review-confidence", "1", []CalibrationPoint{
		{RawPPM: 0, ConfidencePPM: 0},
		{RawPPM: 500_000, ConfidencePPM: 350_000},
		{RawPPM: 1_000_000, ConfidencePPM: 900_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := completePlatformRevision()
	minimum := uint32(600_000)
	maximum := 12
	base.Patch.FindingGovernance = &FindingGovernancePatch{
		CalibrationProfile: &profile, MinimumConfidencePPM: &minimum, MaxFindings: &maximum,
	}
	overriddenMaximum := 5
	repository := Revision{
		SchemaVersion: RevisionSchemaVersion, ID: "repository-governance", Revision: "1",
		Scope:    ScopeRepository,
		Selector: Selector{TenantID: "tenant-1", OrganizationID: "organization-1", RepositoryID: "repository-1"},
		Patch:    ConfigPatch{FindingGovernance: &FindingGovernancePatch{MaxFindings: &overriddenMaximum}},
	}
	bundle, err := Resolve(testContext(), []Revision{repository, base})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if bundle.FindingGovernance == nil || bundle.FindingGovernance.MaxFindings != 5 ||
		bundle.FindingGovernance.MinimumConfidencePPM != minimum ||
		bundle.FindingGovernance.CalibrationProfile.SHA256 != profile.SHA256 {
		t.Fatalf("unexpected finding governance policy: %+v", bundle.FindingGovernance)
	}
	if source := findFieldSource(t, bundle, "finding_governance.max_findings"); len(source.Sources) != 1 || source.Sources[0].ID != repository.ID {
		t.Fatalf("unexpected max_findings provenance: %+v", source)
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBundle(data); err != nil {
		t.Fatalf("DecodeBundle() error = %v", err)
	}
	profile.Points[1].ConfidencePPM++
	if err := profile.Validate(); err == nil {
		t.Fatal("mutated calibration profile retained a valid digest")
	}
}

func TestContextProviderDefinitionAcceptsVersionedCompileOnlyAdapter(t *testing.T) {
	provider := ContextProviderDefinition{
		ID: "go-compile-exact", Revision: "1", Kind: "compile",
		Adapter: VersionedRef{
			ID: "argus-go-compile", Revision: "1",
			SHA256: "fe2f13d7ccda18fdf53c1242b239e0f0fe24d8051b351502f3e30894702ea935",
		},
	}
	if err := provider.Validate(); err != nil {
		t.Fatalf("compile provider Validate() error = %v", err)
	}
	provider.Kind = "test_execution"
	if err := provider.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("unsafe test execution provider error = %v", err)
	}
}

func TestContextProviderDefinitionAcceptsVersionedRepositorySearchAdapter(t *testing.T) {
	provider := ContextProviderDefinition{
		ID: "repository-search-exact", Revision: "1", Kind: "repository_search",
		Adapter: VersionedRef{
			ID: "argus-repository-search", Revision: "1",
			SHA256: "302925847911fb850f025cdf26e1440469f8956417fd04a4d87dd653736749ac",
		},
	}
	if err := provider.Validate(); err != nil {
		t.Fatalf("repository search provider definition rejected: %v", err)
	}
}

func completePlatformRevision() Revision {
	maxFiles := 1000
	maxPatch := int64(1 << 20)
	maxInput := int64(1 << 20)
	maxOutput := int64(1 << 20)
	maxTokens := int64(100_000)
	maxCost := int64(1_000_000)
	timeout := int64(30_000)
	attempts := 2
	concurrency := 4
	contextProviderConcurrency := 4
	minEvidence := 1
	minSeverity := "medium"
	humanReview := true
	maxComments := 0
	retention := int64(30)
	redaction := RedactionStrict
	return Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            "platform-default", Revision: "1", Scope: ScopePlatform,
		Selector: Selector{},
		Patch: ConfigPatch{
			Target: &TargetPatch{
				AllowedModes: &SetPatch{
					Add: []string{"diff", "scope", "selection"}, Remove: []string{},
				},
				Include:       &SetPatch{Add: []string{"**"}, Remove: []string{}},
				Exclude:       &SetPatch{Add: []string{}, Remove: []string{}},
				MaxFiles:      &maxFiles,
				MaxPatchBytes: &maxPatch,
			},
			RulePack: &RulePackPatch{Rules: &OrderedRulePatch{
				Upsert: []RuleDefinition{testRule("rule-a", "1", "high")},
				Remove: []string{},
			}},
			Workflow: &WorkflowPatch{Definition: &VersionedRef{
				ID: "workflow", Revision: "1", SHA256: testSHA,
			}},
			Execution: &ExecutionPatch{
				AgentProfile: &VersionedRef{
					ID: "agent", Revision: "1", SHA256: testSHA,
				},
				ModelProfile: &VersionedRef{
					ID: "model", Revision: "1", SHA256: testSHA,
				},
				AllowedTools: &SetPatch{Add: []string{}, Remove: []string{}},
				ContextProviders: &OrderedContextProviderPatch{
					Upsert: []ContextProviderDefinition{}, Remove: []string{},
				},
				ContextProviderMaxConcurrency: &contextProviderConcurrency,
			},
			Budget: &BudgetPatch{
				MaxInputBytes: &maxInput, MaxOutputBytes: &maxOutput,
				MaxTokens: &maxTokens, MaxCostMicros: &maxCost,
				StageTimeoutMS: &timeout, MaxAttempts: &attempts,
				MaxConcurrency: &concurrency,
			},
			Verification: &VerificationPatch{
				RequiredEvidenceKinds: &SetPatch{
					Add: []string{"file_content", "patch_line"}, Remove: []string{},
				},
				MinimumIndependentEvidence: &minEvidence,
				PartialEvidence:            permissionPointer(PermissionDeny),
			},
			Adjudication: &AdjudicationPatch{
				MinimumSeverity:         &minSeverity,
				HumanReviewInconclusive: &humanReview,
				AutomaticPublication:    permissionPointer(PermissionDeny),
			},
			Publication: &PublicationPatch{
				RemoteWrites: permissionPointer(PermissionDeny),
				Channels:     &SetPatch{Add: []string{}, Remove: []string{}},
				MaxComments:  &maxComments,
			},
			Data: &DataPatch{
				RetentionDays: &retention, Redaction: &redaction,
				Training: permissionPointer(PermissionDeny),
				Export:   permissionPointer(PermissionDeny),
			},
		},
	}
}

func completeAgentReviewPatch() *AgentReviewPatch {
	modelEgress := AgentModelEgressProviderBrokerOnly
	workspaceReads := AgentWorkspaceReadsFrozenInputOnly
	delegationDepth := 0
	maxFiles := 100
	maxGroups := 16
	maxHypotheses := 64
	maxModelCalls := 32
	maxToolCalls := 64
	maxTargetBytes := int64(512 << 10)
	maxGroupBytes := int64(128 << 10)
	maxOutputBytes := int64(128 << 10)
	maxOutputTokens := int64(10_000)
	maxCostMicros := int64(500_000)
	timeoutMS := int64(20_000)
	maxConcurrency := 2
	return &AgentReviewPatch{
		Agent:         refPointer("pi-agent", "1"),
		Normalization: refPointer("candidate-normalization", "v2"),
		Provider:      refPointer("anthropic-compatible", "1"),
		Model:         refPointer("deepseek-chat", "2026-08-01"),
		Prompt:        refPointer("review-prompt", "1"),
		ModelCredential: &SecretRef{
			URI: "secret://env/deepseek-anthropic-key",
		},
		SkillPacks: &OrderedAgentSkillPackPatch{
			Upsert: []AgentSkillPackDefinition{
				agentSkillPack("group-changes", AgentSkillPhaseGrouping, "1"),
				agentSkillPack("review-core", AgentSkillPhaseReview, "1"),
				agentSkillPack("verify-candidates", AgentSkillPhaseVerification, "1"),
			},
			Remove: []string{},
		},
		KnowledgePacks: &OrderedAgentKnowledgePackPatch{
			Upsert: []AgentKnowledgePackDefinition{
				agentKnowledgePack("business-default", "1"),
			},
			Remove: []string{},
		},
		APIProtocol: refPointer("anthropic-messages", "2023-06-01"),
		Authority: &AgentAuthorityPatch{
			ModelEgress:        &modelEgress,
			ToolNetwork:        permissionPointer(PermissionDeny),
			WorkspaceReads:     &workspaceReads,
			WorkspaceWrites:    permissionPointer(PermissionDeny),
			RemoteWrites:       permissionPointer(PermissionDeny),
			MaxDelegationDepth: &delegationDepth,
			Tools: &SetPatch{
				Add: []string{"codegraph"}, Remove: []string{},
			},
		},
		Budget: &AgentBudgetPatch{
			MaxFiles:        &maxFiles,
			MaxGroups:       &maxGroups,
			MaxHypotheses:   &maxHypotheses,
			MaxModelCalls:   &maxModelCalls,
			MaxToolCalls:    &maxToolCalls,
			MaxTargetBytes:  &maxTargetBytes,
			MaxGroupBytes:   &maxGroupBytes,
			MaxOutputBytes:  &maxOutputBytes,
			MaxOutputTokens: &maxOutputTokens,
			MaxCostMicros:   &maxCostMicros,
			TimeoutMS:       &timeoutMS,
			MaxConcurrency:  &maxConcurrency,
		},
	}
}

func agentSkillPack(id string, phase AgentSkillPhase, revision string) AgentSkillPackDefinition {
	return AgentSkillPackDefinition{
		ID: id, Phase: phase,
		Ref: VersionedRef{ID: "skill-" + id, Revision: revision, SHA256: testSHA},
	}
}

func agentKnowledgePack(id, revision string) AgentKnowledgePackDefinition {
	return AgentKnowledgePackDefinition{
		ID:  id,
		Ref: VersionedRef{ID: "knowledge-" + id, Revision: revision, SHA256: testSHA},
	}
}

func refPointer(id, revision string) *VersionedRef {
	return &VersionedRef{ID: id, Revision: revision, SHA256: testSHA}
}

func testRule(id, revision, severity string) RuleDefinition {
	return RuleDefinition{
		ID: id, Revision: revision, Kind: "deterministic",
		Detector: VersionedRef{
			ID: "detector-" + id, Revision: revision, SHA256: testSHA,
		},
		Languages: []string{"go"}, PathPrefixes: []string{},
		EvidenceKinds: []string{"file_content", "patch_line"},
		Severity:      severity, Enabled: true,
	}
}

func maxFilesRevision(
	id string,
	scope Scope,
	selector Selector,
	maxFiles int,
) Revision {
	return Revision{
		SchemaVersion: RevisionSchemaVersion,
		ID:            id, Revision: "1", Scope: scope, Selector: selector,
		Patch: ConfigPatch{Target: &TargetPatch{MaxFiles: &maxFiles}},
	}
}

func testContext() ResolutionContext {
	return ResolutionContext{
		TenantID: "tenant-1", OrganizationID: "organization-1",
		RepositoryID: "repository-1",
		Path:         "internal/reviewconfig/resolve.go",
		InvocationID: "invocation-1",
	}
}

func findFieldSource(
	t *testing.T,
	bundle ConfigBundle,
	field string,
) FieldSource {
	t.Helper()
	for _, source := range bundle.FieldSources {
		if source.Field == field {
			return source
		}
	}
	t.Fatalf("field source %q not found", field)
	return FieldSource{}
}

func countExplain(bundle ConfigBundle, field string) int {
	count := 0
	for _, explanation := range bundle.Explain {
		if explanation.Field == field {
			count++
		}
	}
	return count
}

func intPointer(value int) *int {
	return &value
}

func permissionPointer(value Permission) *Permission {
	return &value
}
