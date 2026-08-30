package application

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"argus.local/argus/internal/agentplan"
	"argus.local/argus/internal/reviewconfig"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestCompileGovernedAgentStageUsesOnlyStrongerSameCallProvider(t *testing.T) {
	resolutionContext := reviewconfig.ResolutionContext{
		TenantID:       "tenant-1",
		OrganizationID: "org-1",
		RepositoryID:   "repo-1",
		InvocationID:   "run-1",
	}
	provider := &governedConfigProviderStub{}
	resolver := &agentComponentResolverStub{}
	_, err := CompileGovernedAgentStage(
		context.Background(),
		governedAgentTestSubject(),
		provider,
		resolver,
		resolutionContext,
		agentplan.CompileInput{
			ReviewRunID:  "run-1",
			ReviewSpec:   governedAgentTestReviewSpec(),
			ConfigBundle: reviewconfig.ConfigBundle{SchemaVersion: "caller-fabricated"},
			ConfigResolutionReceipt: reviewconfig.ConfigResolutionReceipt{
				SchemaVersion: "caller-fabricated",
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "validate governed config resolution") {
		t.Fatalf("CompileGovernedAgentStage() error = %v", err)
	}
	if provider.governedCalls != 1 || provider.legacyCalls != 0 {
		t.Fatalf(
			"provider calls governed=%d legacy=%d, want 1/0",
			provider.governedCalls,
			provider.legacyCalls,
		)
	}
	if resolver.calls != 0 {
		t.Fatalf("component resolver called before governed config validation: %d", resolver.calls)
	}

	legacy := &legacyConfigProviderStub{}
	if _, ok := any(legacy).(GovernedConfigProvider); ok {
		t.Fatal("legacy ConfigProvider unexpectedly satisfies formal agent trust boundary")
	}
}

func TestCompileGovernedAgentStagePropagatesGovernedResolutionFailure(t *testing.T) {
	want := errors.New("ledger unavailable")
	provider := &governedConfigProviderStub{err: want}
	resolver := &agentComponentResolverStub{}
	_, err := CompileGovernedAgentStage(
		context.Background(),
		governedAgentTestSubject(),
		provider,
		resolver,
		reviewconfig.ResolutionContext{
			TenantID:       "tenant-1",
			OrganizationID: "org-1",
			RepositoryID:   "repo-1",
			InvocationID:   "run-1",
		},
		agentplan.CompileInput{
			ReviewRunID: "run-1",
			ReviewSpec:  governedAgentTestReviewSpec(),
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("CompileGovernedAgentStage() error = %v, want %v", err, want)
	}
	if provider.governedCalls != 1 || provider.legacyCalls != 0 {
		t.Fatalf("provider calls governed=%d legacy=%d", provider.governedCalls, provider.legacyCalls)
	}
	if resolver.calls != 0 {
		t.Fatalf("component resolver called after provider failure: %d", resolver.calls)
	}
}

func TestCompileGovernedAgentStageRejectsUnadmittedSubjectBeforeProviders(t *testing.T) {
	provider := &governedConfigProviderStub{}
	resolver := &agentComponentResolverStub{}
	spec := governedAgentTestReviewSpec()
	spec.WorkspaceID = ""
	_, err := CompileGovernedAgentStage(
		context.Background(),
		governedAgentTestSubject(),
		provider,
		resolver,
		reviewconfig.ResolutionContext{
			TenantID:       "tenant-1",
			OrganizationID: "org-1",
			RepositoryID:   "repo-1",
			InvocationID:   "run-1",
		},
		agentplan.CompileInput{ReviewRunID: "run-1", ReviewSpec: spec},
	)
	if err == nil || !strings.Contains(err.Error(), "ReviewSpec") {
		t.Fatalf("CompileGovernedAgentStage() error = %v, want ReviewSpec rejection", err)
	}
	if provider.governedCalls != 0 || provider.legacyCalls != 0 || resolver.calls != 0 {
		t.Fatalf(
			"unadmitted subject reached providers: governed=%d legacy=%d resolver=%d",
			provider.governedCalls,
			provider.legacyCalls,
			resolver.calls,
		)
	}
}

func TestCompileGovernedAgentStageRejectsPathScopeConfusionBeforeProviders(t *testing.T) {
	provider := &governedConfigProviderStub{}
	resolver := &agentComponentResolverStub{}
	_, err := CompileGovernedAgentStage(
		context.Background(),
		governedAgentTestSubject(),
		provider,
		resolver,
		reviewconfig.ResolutionContext{
			TenantID:       "tenant-1",
			OrganizationID: "org-1",
			RepositoryID:   "repo-1",
			Path:           "private/path.go",
			InvocationID:   "run-1",
		},
		agentplan.CompileInput{
			ReviewRunID: "run-1",
			ReviewSpec:  governedAgentTestReviewSpec(),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "non-selection resolution path") {
		t.Fatalf("CompileGovernedAgentStage() error = %v, want path-scope rejection", err)
	}
	if provider.governedCalls != 0 || provider.legacyCalls != 0 || resolver.calls != 0 {
		t.Fatalf(
			"path-confused subject reached providers: governed=%d legacy=%d resolver=%d",
			provider.governedCalls,
			provider.legacyCalls,
			resolver.calls,
		)
	}
}

func TestCompileGovernedAgentStageRejectsEverySubjectMismatchBeforeProviders(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*agentplan.CompileInput, *reviewconfig.ResolutionContext)
	}{
		{
			name: "review run",
			mutate: func(input *agentplan.CompileInput, _ *reviewconfig.ResolutionContext) {
				input.ReviewRunID = "other-run"
			},
		},
		{
			name: "tenant",
			mutate: func(input *agentplan.CompileInput, _ *reviewconfig.ResolutionContext) {
				input.ReviewSpec.TenantID = "other-tenant"
			},
		},
		{
			name: "organization",
			mutate: func(_ *agentplan.CompileInput, resolution *reviewconfig.ResolutionContext) {
				resolution.OrganizationID = "other-organization"
			},
		},
		{
			name: "workspace",
			mutate: func(input *agentplan.CompileInput, _ *reviewconfig.ResolutionContext) {
				input.ReviewSpec.WorkspaceID = "other-workspace"
			},
		},
		{
			name: "repository",
			mutate: func(input *agentplan.CompileInput, _ *reviewconfig.ResolutionContext) {
				input.ReviewSpec.Repository.RepositoryID = "other-repository"
			},
		},
		{
			name: "invocation",
			mutate: func(_ *agentplan.CompileInput, resolution *reviewconfig.ResolutionContext) {
				resolution.InvocationID = "other-run"
			},
		},
		{
			name: "repository provider",
			mutate: func(input *agentplan.CompileInput, _ *reviewconfig.ResolutionContext) {
				input.ReviewSpec.Repository.Provider = "github"
			},
		},
		{
			name: "non-selection path",
			mutate: func(_ *agentplan.CompileInput, resolution *reviewconfig.ResolutionContext) {
				resolution.Path = "private/path.go"
			},
		},
		{
			name: "selection path",
			mutate: func(input *agentplan.CompileInput, resolution *reviewconfig.ResolutionContext) {
				input.ReviewSpec.Target = governedAgentSelectionTarget("allowed/path.go")
				resolution.Path = "other/path.go"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &governedConfigProviderStub{}
			resolver := &agentComponentResolverStub{}
			input := agentplan.CompileInput{
				ReviewRunID: "run-1",
				ReviewSpec:  governedAgentTestReviewSpec(),
			}
			resolution := reviewconfig.ResolutionContext{
				TenantID:       "tenant-1",
				OrganizationID: "org-1",
				RepositoryID:   "repo-1",
				InvocationID:   "run-1",
			}
			test.mutate(&input, &resolution)
			_, err := CompileGovernedAgentStage(
				context.Background(), governedAgentTestSubject(),
				provider, resolver, resolution, input,
			)
			if err == nil || !strings.Contains(err.Error(), "formal agent") {
				t.Fatalf("CompileGovernedAgentStage() error = %v, want preflight rejection", err)
			}
			if provider.governedCalls != 0 || provider.legacyCalls != 0 || resolver.calls != 0 {
				t.Fatalf(
					"mismatched subject reached providers: governed=%d legacy=%d resolver=%d",
					provider.governedCalls,
					provider.legacyCalls,
					resolver.calls,
				)
			}
		})
	}
}

func TestCompileGovernedAgentStageForwardsAdmittedSubjectAndGovernedIdentities(t *testing.T) {
	bundleData, err := os.ReadFile("../../examples/config-bundle.agent-review.json")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		t.Fatal(err)
	}
	receiptData, err := os.ReadFile("../../examples/config-resolution-receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := reviewconfig.DecodeConfigResolutionReceipt(receiptData)
	if err != nil {
		t.Fatal(err)
	}
	provider := &governedConfigProviderStub{bundle: bundle, receipt: receipt}
	resolver := &agentComponentResolverStub{}
	spec := governedAgentTestReviewSpec()
	spec.RequestID = bundle.Context.InvocationID
	spec.IdempotencyKey = bundle.Context.InvocationID
	spec.TenantID = bundle.Context.TenantID
	spec.WorkspaceID = "workspace-example"
	spec.Repository.RepositoryID = bundle.Context.RepositoryID
	spec.Target = governedAgentSelectionTarget(bundle.Context.Path)
	_, err = CompileGovernedAgentStage(
		context.Background(),
		AgentPlanningSubject{
			TenantID:       bundle.Context.TenantID,
			OrganizationID: bundle.Context.OrganizationID,
			WorkspaceID:    spec.WorkspaceID,
			RepositoryID:   bundle.Context.RepositoryID,
		},
		provider,
		resolver,
		bundle.Context,
		agentplan.CompileInput{
			ReviewRunID: bundle.Context.InvocationID,
			StageID:     "agent_hypothesize",
			ReviewSpec:  spec,
			Components: agentplan.ResolvedComponents{
				Agent: contractsv1alpha1.AgentStageComponentBinding{
					Ref: contractsv1alpha1.VersionedRef{ID: "caller-fabricated"},
				},
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "validate execution snapshot") {
		t.Fatalf("CompileGovernedAgentStage() error = %v, want late compiler rejection", err)
	}
	if provider.governedCalls != 1 || provider.legacyCalls != 0 || resolver.calls != 1 {
		t.Fatalf(
			"provider/resolver calls governed=%d legacy=%d resolver=%d, want 1/0/1",
			provider.governedCalls,
			provider.legacyCalls,
			resolver.calls,
		)
	}
	request := resolver.lastRequest
	policy := *bundle.AgentReview
	if request.TenantID != spec.TenantID ||
		request.OrganizationID != bundle.Context.OrganizationID ||
		request.WorkspaceID != spec.WorkspaceID ||
		request.RepositoryID != spec.Repository.RepositoryID ||
		request.PolicySHA256 != policy.SHA256 || request.Agent != policy.Agent ||
		request.Provider != policy.Provider || request.Model != policy.Model ||
		request.Runtime != bundle.Execution.AgentProfile || request.Prompt != policy.Prompt ||
		request.APIProtocol != policy.APIProtocol ||
		!reflect.DeepEqual(request.Skills, policy.SkillPacks) ||
		!reflect.DeepEqual(request.Knowledge, policy.KnowledgePacks) ||
		!reflect.DeepEqual(request.ContextProviders, bundle.Execution.ContextProviders) {
		t.Fatalf("resolver request does not contain exact governed subject/identities: %+v", request)
	}
}

func governedAgentTestReviewSpec() contractsv1alpha1.ReviewSpec {
	return contractsv1alpha1.ReviewSpec{
		SchemaVersion:  contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:      "run-1",
		IdempotencyKey: "run-1",
		TenantID:       "tenant-1",
		WorkspaceID:    "workspace-1",
		Repository: contractsv1alpha1.RepositoryRef{
			Provider: "local-git", RepositoryID: "repo-1",
		},
		Target: contractsv1alpha1.ReviewTarget{
			Mode: contractsv1alpha1.ReviewModeScope,
			Scope: &contractsv1alpha1.ScopeTarget{
				Revision: "head", Include: []string{"**"}, Exclude: []string{},
			},
		},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID: "config-1", Revision: "1", SHA256: strings.Repeat("a", 64),
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: "workflow-1", Revision: "1", SHA256: strings.Repeat("b", 64),
		},
		RequestedOutput: []string{"findings"},
		RemoteWrites:    "deny",
	}
}

func governedAgentTestSubject() AgentPlanningSubject {
	return AgentPlanningSubject{
		TenantID:       "tenant-1",
		OrganizationID: "org-1",
		WorkspaceID:    "workspace-1",
		RepositoryID:   "repo-1",
	}
}

func governedAgentSelectionTarget(path string) contractsv1alpha1.ReviewTarget {
	return contractsv1alpha1.ReviewTarget{
		Mode: contractsv1alpha1.ReviewModeSelection,
		Selection: &contractsv1alpha1.SelectionTarget{
			Revision:  "head",
			Path:      path,
			StartLine: 1,
			EndLine:   1,
			Content: contractsv1alpha1.ContentRef{
				URI:       "artifact://selection/content",
				SHA256:    strings.Repeat("c", 64),
				SizeBytes: 1,
			},
		},
	}
}

type governedConfigProviderStub struct {
	bundle        reviewconfig.ConfigBundle
	receipt       reviewconfig.ConfigResolutionReceipt
	err           error
	legacyCalls   int
	governedCalls int
}

func (provider *governedConfigProviderStub) ResolvePublished(
	context.Context,
	reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, error) {
	provider.legacyCalls++
	return provider.bundle, provider.err
}

func (provider *governedConfigProviderStub) ResolvePublishedWithReceipt(
	context.Context,
	reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	provider.governedCalls++
	return provider.bundle, provider.receipt, provider.err
}

type legacyConfigProviderStub struct{}

func (*legacyConfigProviderStub) ResolvePublished(
	context.Context,
	reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, error) {
	return reviewconfig.ConfigBundle{}, nil
}

type agentComponentResolverStub struct {
	components  agentplan.ResolvedComponents
	err         error
	calls       int
	lastRequest AgentComponentResolutionRequest
}

func (resolver *agentComponentResolverStub) ResolveAgentComponents(
	_ context.Context,
	_ reviewconfig.ResolutionContext,
	request AgentComponentResolutionRequest,
) (agentplan.ResolvedComponents, error) {
	resolver.calls++
	resolver.lastRequest = request
	return resolver.components, resolver.err
}
