package application

import (
	"fmt"
	"slices"

	"argus.local/argus/internal/configdefaults"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/workflow"
)

func DefaultConfigBundle(
	config LocalConfig,
	definition workflow.Definition,
	contexts ...reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, error) {
	return defaultConfigBundleWithProviders(config, definition, nil, contexts...)
}

func defaultConfigBundleWithProviders(
	config LocalConfig,
	definition workflow.Definition,
	providers []reviewconfig.ContextProviderDefinition,
	contexts ...reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, error) {
	if err := config.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, err
	}
	if err := definition.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, err
	}
	allowedModes := make([]string, len(config.AllowedModes))
	for index, mode := range config.AllowedModes {
		allowedModes[index] = string(mode)
	}
	var resolutionContext reviewconfig.ResolutionContext
	if len(contexts) > 1 {
		return reviewconfig.ConfigBundle{}, fmt.Errorf("at most one resolution context is allowed")
	}
	if len(contexts) == 1 {
		resolutionContext = contexts[0]
	}
	maxOutputBytes := definition.Stages[0].Budget.MaxOutputBytes
	for _, stage := range definition.Stages[1:] {
		maxOutputBytes = min(maxOutputBytes, stage.Budget.MaxOutputBytes)
	}
	return configdefaults.Bundle(configdefaults.Options{
		ID:               config.ID,
		Revision:         config.Revision,
		MaxFiles:         config.MaxFiles,
		MaxPatchBytes:    config.MaxPatchBytes,
		MaxInputBytes:    config.MaxMaterializedBytes,
		MaxOutputBytes:   maxOutputBytes,
		MaxAttempts:      config.MaxAttempts,
		AllowedModes:     allowedModes,
		TargetInclude:    slices.Clone(config.TargetInclude),
		TargetExclude:    slices.Clone(config.TargetExclude),
		ContextProviders: slices.Clone(providers),
		Context:          resolutionContext,
	}, definition)
}

func InvocationContextProviderDefinitions(
	ids []string,
) ([]reviewconfig.ContextProviderDefinition, error) {
	providers := make([]reviewconfig.ContextProviderDefinition, 0, len(ids))
	for _, id := range ids {
		switch id {
		case "repository_search":
			providers = append(providers, reviewconfig.ContextProviderDefinition{
				ID: "repository-search-exact", Revision: "1", Kind: "repository_search",
				Adapter: reviewconfig.VersionedRef{
					ID: "argus-repository-search", Revision: "2",
					SHA256: "69190b151bb7b60129e820bd0d687b4d0b79ed00b83915d0795ac5b5f274a66d",
				},
			})
		case "go_ast":
			providers = append(providers, reviewconfig.ContextProviderDefinition{
				ID: "go-ast-exact", Revision: "1", Kind: "go_ast",
				Adapter: reviewconfig.VersionedRef{
					ID: "argus-go-ast", Revision: "2",
					SHA256: "f343930b884436b3cef6e35fda496e4780fa2edfc94285299f23db552821e961",
				},
			})
		case "go_dependencies":
			providers = append(providers, reviewconfig.ContextProviderDefinition{
				ID: "go-dependencies-exact", Revision: "1", Kind: "dependency",
				Adapter: reviewconfig.VersionedRef{
					ID: "argus-go-dependencies", Revision: "1",
					SHA256: "6a94b881359a0e09d371822e83a812f732e0064844495b0948942881716a88ba",
				},
			})
		case "go_compile":
			providers = append(providers, reviewconfig.ContextProviderDefinition{
				ID: "go-compile-exact", Revision: "1", Kind: "compile",
				Adapter: reviewconfig.VersionedRef{
					ID: "argus-go-compile", Revision: "1",
					SHA256: "6edc39a47926bbe71f8b83e2bd6c796d812ae4f935b65bde30bae2f0d59e0e02",
				},
			})
		default:
			return nil, fmt.Errorf("unsupported invocation context provider %q", id)
		}
	}
	return providers, nil
}

func invocationContextProviderDefinitions(ids []string) ([]reviewconfig.ContextProviderDefinition, error) {
	return InvocationContextProviderDefinitions(ids)
}

func runtimeConfigFromBundle(
	bundle reviewconfig.ConfigBundle,
	base LocalConfig,
) (LocalConfig, error) {
	if bundle.AgentReview != nil {
		return LocalConfig{}, fmt.Errorf(
			"agent_review policy requires the formal agent_hypothesize workflow; " +
				"the current deterministic runtime cannot silently ignore it",
		)
	}
	return contextMaterializationConfigFromBundle(bundle, base)
}

func contextMaterializationConfigFromBundle(
	bundle reviewconfig.ConfigBundle,
	base LocalConfig,
) (LocalConfig, error) {
	if err := bundle.Validate(); err != nil {
		return LocalConfig{}, err
	}
	if !slices.Contains(bundle.Target.AllowedModes, "diff") &&
		!slices.Contains(bundle.Target.AllowedModes, "selection") &&
		!slices.Contains(bundle.Target.AllowedModes, "scope") {
		return LocalConfig{}, fmt.Errorf("config bundle allows no supported target mode")
	}
	if bundle.Publication.RemoteWrites != reviewconfig.PermissionDeny {
		return LocalConfig{}, fmt.Errorf("local runtime requires publication.remote_writes=deny")
	}
	base.ID = bundle.BundleID
	base.Revision = bundle.SHA256[:16]
	base.MaxFiles = bundle.Target.MaxFiles
	base.MaxPatchBytes = min(
		base.MaxPatchBytes,
		bundle.Target.MaxPatchBytes,
		bundle.Budget.MaxInputBytes,
	)
	base.MaxFileContentBytes = min(base.MaxFileContentBytes, bundle.Budget.MaxInputBytes)
	base.MaxMaterializedBytes = bundle.Budget.MaxInputBytes
	base.MaxAttempts = bundle.Budget.MaxAttempts
	base.AllowedModes = make([]reviewcore.TargetMode, len(bundle.Target.AllowedModes))
	for index, mode := range bundle.Target.AllowedModes {
		base.AllowedModes[index] = reviewcore.TargetMode(mode)
	}
	base.TargetInclude = slices.Clone(bundle.Target.Include)
	base.TargetExclude = slices.Clone(bundle.Target.Exclude)
	base.RemoteWrites = "deny"
	if err := base.Validate(); err != nil {
		return LocalConfig{}, err
	}
	return base, nil
}

func bundlePolicyRef(bundle reviewconfig.ConfigBundle) runmodelPolicyRef {
	return runmodelPolicyRef{
		ID: bundle.BundleID, Revision: bundle.SHA256[:16], SHA256: bundle.SHA256,
	}
}

type runmodelPolicyRef struct {
	ID       string
	Revision string
	SHA256   string
}
