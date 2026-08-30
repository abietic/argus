package application

import (
	"context"
	"fmt"
	"slices"

	"github.com/abietic/argus/internal/agentplan"
	"github.com/abietic/argus/internal/reviewconfig"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// AgentComponentResolutionRequest contains the already-admitted artifact
// subject and governed identities only. It intentionally excludes
// ModelCredential so component artifact resolution cannot read or leak
// provider credentials.
type AgentComponentResolutionRequest struct {
	TenantID         string
	OrganizationID   string
	WorkspaceID      string
	RepositoryID     string
	PolicySHA256     string
	Agent            reviewconfig.VersionedRef
	Provider         reviewconfig.VersionedRef
	Model            reviewconfig.VersionedRef
	Runtime          reviewconfig.VersionedRef
	Prompt           reviewconfig.VersionedRef
	APIProtocol      reviewconfig.VersionedRef
	Skills           []reviewconfig.AgentSkillPackDefinition
	Knowledge        []reviewconfig.AgentKnowledgePackDefinition
	ContextProviders []reviewconfig.ContextProviderDefinition
}

// AgentPlanningSubject is supplied by an authenticated host adapter. Keeping
// it separate from caller-owned ReviewSpec and ResolutionContext prevents
// organization/workspace scope from being inferred from untrusted request
// fields. This value is not itself an authentication token; the composition
// root must construct it from verified platform identity.
type AgentPlanningSubject struct {
	TenantID       string
	OrganizationID string
	WorkspaceID    string
	RepositoryID   string
}

func (subject AgentPlanningSubject) validate() error {
	for name, value := range map[string]string{
		"subject.tenant_id":       subject.TenantID,
		"subject.organization_id": subject.OrganizationID,
		"subject.workspace_id":    subject.WorkspaceID,
		"subject.repository_id":   subject.RepositoryID,
	} {
		if err := validateLocalIdentity(name, value); err != nil {
			return err
		}
	}
	return nil
}

// AgentComponentResolver is the application port for resolving exact,
// content-addressed component artifacts from a governed registry. Formal
// planning never accepts caller-supplied component bindings at this boundary.
type AgentComponentResolver interface {
	ResolveAgentComponents(
		context.Context,
		reviewconfig.ResolutionContext,
		AgentComponentResolutionRequest,
	) (agentplan.ResolvedComponents, error)
}

// CompileGovernedAgentStage is the publication-trust entry for formal S1
// planning. The low-level agentplan compiler proves record integrity only;
// this application boundary obtains ConfigBundle and ConfigResolutionReceipt
// from one call to the stronger governed provider and replaces any caller-
// supplied raw values before compilation.
func CompileGovernedAgentStage(
	ctx context.Context,
	subject AgentPlanningSubject,
	provider GovernedConfigProvider,
	componentResolver AgentComponentResolver,
	resolutionContext reviewconfig.ResolutionContext,
	input agentplan.CompileInput,
) (contractsv1alpha1.AgentStagePlan, error) {
	if err := validateAgentPlanningPreflight(
		ctx,
		subject,
		resolutionContext,
		input,
	); err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	bundle, receipt, components, err := resolveGovernedAgentStageClosure(
		ctx,
		subject,
		provider,
		componentResolver,
		resolutionContext,
	)
	if err != nil {
		return contractsv1alpha1.AgentStagePlan{}, err
	}
	input.ConfigBundle = bundle
	input.ConfigResolutionReceipt = receipt
	input.Components = components
	return agentplan.Compile(input)
}

// validateAgentPlanningPreflight closes every caller-controlled identity
// before a config ledger, component registry, artifact repository, or
// admission ledger is touched. PrepareGovernedAgentStage reuses this gate so
// an idempotent lookup cannot become a cross-workspace existence oracle.
func validateAgentPlanningPreflight(
	ctx context.Context,
	subject AgentPlanningSubject,
	resolutionContext reviewconfig.ResolutionContext,
	input agentplan.CompileInput,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := subject.validate(); err != nil {
		return fmt.Errorf(
			"validate formal agent planning subject: %w",
			err,
		)
	}
	if err := resolutionContext.Validate(); err != nil {
		return fmt.Errorf(
			"validate config resolution context: %w",
			err,
		)
	}
	// Caller-owned identity must be admitted before either the config ledger or
	// component registry is touched. This prevents a fabricated ReviewSpec from
	// turning either provider into a confused deputy for another workspace.
	if err := input.ReviewSpec.Validate(); err != nil {
		return fmt.Errorf(
			"validate formal agent ReviewSpec: %w",
			err,
		)
	}
	expectedConfigInvocation := input.ReviewRunID
	if input.FormalReplay != nil {
		expectedConfigInvocation = input.ConfigBundle.Context.InvocationID
	}
	if input.ReviewRunID != input.ReviewSpec.RequestID ||
		input.ReviewSpec.TenantID != subject.TenantID ||
		resolutionContext.TenantID != subject.TenantID ||
		resolutionContext.OrganizationID != subject.OrganizationID ||
		input.ReviewSpec.WorkspaceID != subject.WorkspaceID ||
		input.ReviewSpec.Repository.RepositoryID != subject.RepositoryID ||
		resolutionContext.RepositoryID != subject.RepositoryID ||
		expectedConfigInvocation != resolutionContext.InvocationID ||
		input.ReviewSpec.Repository.Provider != "local-git" {
		return fmt.Errorf(
			"formal agent ReviewSpec does not match the resolution subject",
		)
	}
	if input.ReviewSpec.Target.Mode == contractsv1alpha1.ReviewModeSelection {
		if resolutionContext.Path != input.ReviewSpec.Target.Selection.Path {
			return fmt.Errorf(
				"formal agent selection path does not match the resolution subject",
			)
		}
	} else if resolutionContext.Path != "" {
		return fmt.Errorf(
			"formal agent non-selection resolution path must be empty",
		)
	}
	return nil
}

func resolveGovernedAgentStageClosure(
	ctx context.Context,
	subject AgentPlanningSubject,
	provider GovernedConfigProvider,
	componentResolver AgentComponentResolver,
	resolutionContext reviewconfig.ResolutionContext,
) (
	reviewconfig.ConfigBundle,
	reviewconfig.ConfigResolutionReceipt,
	agentplan.ResolvedComponents,
	error,
) {
	bundle, receipt, err := resolveGovernedAgentConfig(
		ctx,
		provider,
		resolutionContext,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			agentplan.ResolvedComponents{}, err
	}
	components, err := resolveGovernedAgentComponents(
		ctx,
		subject,
		componentResolver,
		resolutionContext,
		bundle,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			agentplan.ResolvedComponents{}, err
	}
	return bundle, receipt, components, nil
}

func resolveGovernedAgentConfig(
	ctx context.Context,
	provider GovernedConfigProvider,
	resolutionContext reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if provider == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("governed config provider is required")
	}
	bundle, receipt, err := provider.ResolvePublishedWithReceipt(ctx, resolutionContext)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("resolve governed agent config: %w", err)
	}
	if err := receipt.ValidateAgainst(bundle); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, fmt.Errorf(
			"validate governed config resolution: %w",
			err,
		)
	}
	if bundle.Context != resolutionContext {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, fmt.Errorf(
			"governed config resolution returned another context",
		)
	}
	if bundle.AgentReview == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, fmt.Errorf(
			"governed config has no agent_review policy",
		)
	}
	return bundle, receipt, nil
}

func resolveGovernedAgentComponents(
	ctx context.Context,
	subject AgentPlanningSubject,
	componentResolver AgentComponentResolver,
	resolutionContext reviewconfig.ResolutionContext,
	bundle reviewconfig.ConfigBundle,
) (agentplan.ResolvedComponents, error) {
	if componentResolver == nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf(
			"agent component resolver is required",
		)
	}
	policy := bundle.AgentReview
	components, err := componentResolver.ResolveAgentComponents(
		ctx,
		resolutionContext,
		AgentComponentResolutionRequest{
			TenantID:       subject.TenantID,
			OrganizationID: subject.OrganizationID,
			WorkspaceID:    subject.WorkspaceID,
			RepositoryID:   subject.RepositoryID,
			PolicySHA256:   policy.SHA256,
			Agent:          policy.Agent,
			Provider:       policy.Provider,
			Model:          policy.Model,
			Runtime:        bundle.Execution.AgentProfile,
			Prompt:         policy.Prompt,
			APIProtocol:    policy.APIProtocol,
			Skills:         slices.Clone(policy.SkillPacks),
			Knowledge:      slices.Clone(policy.KnowledgePacks),
			ContextProviders: slices.Clone(
				bundle.Execution.ContextProviders,
			),
		},
	)
	if err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf(
			"resolve governed agent components: %w",
			err,
		)
	}
	return components, nil
}
