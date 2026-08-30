package agentcomponentrepo

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/abietic/argus/internal/agentplan"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/reviewconfig"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// Resolver is bound to one authenticated composition subject. The application
// still repeats the subject in its request so both sides can fail closed on a
// confused-deputy or wiring error.
type Resolver struct {
	repository *Repository
	subject    Subject
}

var _ application.AgentComponentResolver = (*Resolver)(nil)

func NewResolver(repository *Repository, subject Subject) (*Resolver, error) {
	if repository == nil {
		return nil, fmt.Errorf("component repository is required")
	}
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	return &Resolver{repository: repository, subject: subject}, nil
}

func (resolver *Resolver) ResolveAgentComponents(
	ctx context.Context,
	resolution reviewconfig.ResolutionContext,
	request application.AgentComponentResolutionRequest,
) (agentplan.ResolvedComponents, error) {
	if err := checkContext(ctx); err != nil {
		return agentplan.ResolvedComponents{}, err
	}
	if resolver == nil || resolver.repository == nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf("component resolver is not initialized")
	}
	if err := resolution.Validate(); err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf(
			"validate component resolution context: %w", err,
		)
	}
	if request.TenantID != resolver.subject.TenantID ||
		request.OrganizationID != resolver.subject.OrganizationID ||
		request.WorkspaceID != resolver.subject.WorkspaceID ||
		request.RepositoryID != resolver.subject.RepositoryID ||
		resolution.TenantID != resolver.subject.TenantID ||
		resolution.OrganizationID != resolver.subject.OrganizationID ||
		resolution.RepositoryID != resolver.subject.RepositoryID {
		return agentplan.ResolvedComponents{}, fmt.Errorf(
			"component request does not match authenticated resolver subject",
		)
	}
	if !lowerSHA256(request.PolicySHA256) {
		return agentplan.ResolvedComponents{}, fmt.Errorf("policy_sha256 must be a lowercase SHA-256")
	}
	for name, reference := range map[string]reviewconfig.VersionedRef{
		"agent": request.Agent, "provider": request.Provider, "model": request.Model,
		"runtime": request.Runtime, "prompt": request.Prompt,
		"api_protocol": request.APIProtocol,
	} {
		if err := reference.Validate(); err != nil {
			return agentplan.ResolvedComponents{}, fmt.Errorf("validate %s reference: %w", name, err)
		}
	}

	components := agentplan.ResolvedComponents{}
	var err error
	if components.Agent, err = resolver.resolve(
		ctx, contractsv1alpha1.AgentStagePlanAgentContract, request.Agent,
	); err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf("resolve agent component: %w", err)
	}
	if components.Provider, err = resolver.resolve(
		ctx, contractsv1alpha1.AgentStagePlanProviderContract, request.Provider,
	); err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf("resolve provider component: %w", err)
	}
	if components.Model, err = resolver.resolve(
		ctx, contractsv1alpha1.AgentStagePlanModelContract, request.Model,
	); err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf("resolve model component: %w", err)
	}
	if components.Runtime, err = resolver.resolve(
		ctx, contractsv1alpha1.AgentStagePlanRuntimeContract, request.Runtime,
	); err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf("resolve runtime component: %w", err)
	}
	if components.Prompt, err = resolver.resolve(
		ctx, contractsv1alpha1.AgentStagePlanPromptContract, request.Prompt,
	); err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf("resolve prompt component: %w", err)
	}
	if components.APIProtocol, err = resolver.resolve(
		ctx, contractsv1alpha1.AgentStagePlanAPIProtocolContract, request.APIProtocol,
	); err != nil {
		return agentplan.ResolvedComponents{}, fmt.Errorf("resolve API protocol component: %w", err)
	}

	components.Skills = make(
		[]contractsv1alpha1.AgentStageSkillBinding,
		0,
		len(request.Skills),
	)
	for index, definition := range request.Skills {
		if err := definition.Validate(); err != nil {
			return agentplan.ResolvedComponents{}, fmt.Errorf(
				"validate skill[%d]: %w", index, err,
			)
		}
		binding, err := resolver.resolve(
			ctx, contractsv1alpha1.AgentStagePlanSkillContract, definition.Ref,
		)
		if err != nil {
			return agentplan.ResolvedComponents{}, fmt.Errorf(
				"resolve skill[%d] %q: %w", index, definition.ID, err,
			)
		}
		components.Skills = append(components.Skills, contractsv1alpha1.AgentStageSkillBinding{
			ID:       definition.ID,
			Phase:    contractsv1alpha1.AgentStageSkillPhase(definition.Phase),
			Ref:      binding.Ref,
			Artifact: binding.Artifact,
		})
	}

	components.Knowledge = make(
		[]contractsv1alpha1.AgentStageKnowledgeBinding,
		0,
		len(request.Knowledge),
	)
	for index, definition := range request.Knowledge {
		if err := definition.Validate(); err != nil {
			return agentplan.ResolvedComponents{}, fmt.Errorf(
				"validate knowledge[%d]: %w", index, err,
			)
		}
		binding, err := resolver.resolve(
			ctx, contractsv1alpha1.AgentStagePlanKnowledgeContract, definition.Ref,
		)
		if err != nil {
			return agentplan.ResolvedComponents{}, fmt.Errorf(
				"resolve knowledge[%d] %q: %w", index, definition.ID, err,
			)
		}
		components.Knowledge = append(
			components.Knowledge,
			contractsv1alpha1.AgentStageKnowledgeBinding{
				ID: definition.ID, Ref: binding.Ref, Artifact: binding.Artifact,
			},
		)
	}

	components.ContextProviders = make(
		[]contractsv1alpha1.AgentStageContextProviderBinding,
		0,
		len(request.ContextProviders),
	)
	for index, definition := range request.ContextProviders {
		if err := definition.Validate(); err != nil {
			return agentplan.ResolvedComponents{}, fmt.Errorf(
				"validate context provider[%d]: %w", index, err,
			)
		}
		adapter, err := resolver.resolve(
			ctx,
			contractsv1alpha1.AgentStagePlanContextProviderAdapterContract,
			definition.Adapter,
		)
		if err != nil {
			return agentplan.ResolvedComponents{}, fmt.Errorf(
				"resolve context provider[%d] %q: %w", index, definition.ID, err,
			)
		}
		components.ContextProviders = append(
			components.ContextProviders,
			contractsv1alpha1.AgentStageContextProviderBinding{
				ID: definition.ID, Revision: definition.Revision,
				Kind: definition.Kind, Adapter: adapter,
			},
		)
	}

	// Detach all caller-owned ordered slices before returning the closure.
	components.Skills = slices.Clone(components.Skills)
	components.Knowledge = slices.Clone(components.Knowledge)
	components.ContextProviders = slices.Clone(components.ContextProviders)
	return components, nil
}

func (resolver *Resolver) resolve(
	ctx context.Context,
	contract string,
	reference reviewconfig.VersionedRef,
) (contractsv1alpha1.AgentStageComponentBinding, error) {
	return resolver.repository.Resolve(ctx, resolver.subject, contract, reference)
}

func lowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}
