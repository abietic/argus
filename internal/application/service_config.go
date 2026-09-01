package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
)

const (
	localTenantID       = "local"
	localOrganizationID = "local"

	maxLocalStageTimeoutMS = int64(24 * time.Hour / time.Millisecond)
)

var baselineEvidenceKinds = []string{"file_content", "patch_line", "target_line"}

func (service *Service) configForReview(
	ctx context.Context,
	runID string,
	request ReviewRequest,
) (reviewconfig.ConfigBundle, LocalConfig, error) {
	resolutionContext, err := ReviewResolutionContext(runID, request)
	if err != nil {
		return reviewconfig.ConfigBundle{}, LocalConfig{}, err
	}

	bundle := service.configBundle
	if service.configProvider != nil {
		if len(request.ContextProviderIDs) != 0 {
			return reviewconfig.ConfigBundle{}, LocalConfig{}, fmt.Errorf(
				"published config owns execution.context_providers; invocation providers are not allowed",
			)
		}
		if request.MaxFiles != 0 || request.MaxPatchBytes != 0 {
			return reviewconfig.ConfigBundle{}, LocalConfig{}, fmt.Errorf(
				"published config cannot freeze request-level max_files or max_patch_bytes; " +
					"publish the limits as a config revision and omit request overrides",
			)
		}
		bundle, err = service.configProvider.ResolvePublished(ctx, resolutionContext)
		if err != nil {
			return reviewconfig.ConfigBundle{}, LocalConfig{},
				fmt.Errorf("resolve published config: %w", err)
		}
	} else if service.defaultBundle {
		invocationConfig, configErr := service.defaultInvocationConfig(request)
		if configErr != nil {
			return reviewconfig.ConfigBundle{}, LocalConfig{}, configErr
		}
		providers, providerErr := invocationContextProviderDefinitions(
			request.ContextProviderIDs,
		)
		if providerErr != nil {
			return reviewconfig.ConfigBundle{}, LocalConfig{}, providerErr
		}
		bundle, err = defaultConfigBundleWithProviders(
			invocationConfig,
			service.workflow,
			providers,
			resolutionContext,
		)
		if err != nil {
			return reviewconfig.ConfigBundle{}, LocalConfig{},
				fmt.Errorf("resolve default config bundle: %w", err)
		}
	} else if request.MaxFiles != 0 || request.MaxPatchBytes != 0 ||
		len(request.ContextProviderIDs) != 0 {
		return reviewconfig.ConfigBundle{}, LocalConfig{}, fmt.Errorf(
			"custom config bundle cannot freeze request-level limits or context providers; " +
				"encode them in the ConfigBundle and omit request overrides",
		)
	}
	if err := validateBundleContext(bundle, resolutionContext); err != nil {
		return reviewconfig.ConfigBundle{}, LocalConfig{}, err
	}
	if err := validateRuntimeEnvelope(bundle, service.workflow); err != nil {
		return reviewconfig.ConfigBundle{}, LocalConfig{}, err
	}
	runtimeConfig, err := runtimeConfigFromBundle(bundle, service.configSource)
	if err != nil {
		return reviewconfig.ConfigBundle{}, LocalConfig{},
			fmt.Errorf("derive review runtime limits: %w", err)
	}
	return bundle, runtimeConfig, nil
}

// ReviewResolutionContext returns the exact project-native configuration
// subject used by Review. Local asynchronous adapters use this before
// admission so the accepted command can freeze the same lifecycle-governed
// ConfigBundle that execution will later consume.
func ReviewResolutionContext(
	runID string,
	request ReviewRequest,
) (reviewconfig.ResolutionContext, error) {
	if runID == "" {
		return reviewconfig.ResolutionContext{}, fmt.Errorf("run id is required")
	}
	if request.RepositoryPath == "" || !filepath.IsAbs(request.RepositoryPath) ||
		filepath.Clean(request.RepositoryPath) != request.RepositoryPath {
		return reviewconfig.ResolutionContext{},
			fmt.Errorf("repository path must be a clean absolute path")
	}
	repositoryRoot, err := canonicalRepositoryRoot(request.RepositoryPath)
	if err != nil {
		return reviewconfig.ResolutionContext{},
			fmt.Errorf("resolve repository for config: %w", err)
	}
	return reviewconfig.ResolutionContext{
		TenantID:       localTenantID,
		OrganizationID: localOrganizationID,
		RepositoryID:   expectedLocalRepositoryID(repositoryRoot),
		Path:           configContextPath(request),
		InvocationID:   runID,
	}, nil
}

// ValidateReviewRequestAgainstBundle applies Review's executable limits to an
// already-frozen bundle without materializing the target. It lets durable
// command admission fail before queueing an invocation that execution would
// deterministically reject.
func ValidateReviewRequestAgainstBundle(
	request ReviewRequest,
	bundle reviewconfig.ConfigBundle,
) error {
	config, err := runtimeConfigFromBundle(bundle, DefaultLocalConfig())
	if err != nil {
		return fmt.Errorf("derive review runtime limits: %w", err)
	}
	return request.Validate(config)
}

func (service *Service) defaultInvocationConfig(request ReviewRequest) (LocalConfig, error) {
	if request.MaxFiles < 0 || request.MaxPatchBytes < 0 {
		return LocalConfig{}, fmt.Errorf("review request limits must not be negative")
	}
	if request.MaxFiles > service.config.MaxFiles {
		return LocalConfig{}, fmt.Errorf(
			"requested max files %d exceeds configured limit %d",
			request.MaxFiles,
			service.config.MaxFiles,
		)
	}
	if request.MaxPatchBytes > service.config.MaxPatchBytes {
		return LocalConfig{}, fmt.Errorf(
			"requested max patch bytes %d exceeds configured limit %d",
			request.MaxPatchBytes,
			service.config.MaxPatchBytes,
		)
	}

	effective := service.configSource
	if request.MaxFiles > 0 {
		effective.MaxFiles = request.MaxFiles
	}
	if request.MaxPatchBytes > 0 {
		effective.MaxPatchBytes = request.MaxPatchBytes
	}
	if err := effective.Validate(); err != nil {
		return LocalConfig{}, fmt.Errorf("validate invocation-local config: %w", err)
	}
	return effective, nil
}

func (service *Service) expectedDefaultReplayBundle(
	source reviewconfig.ConfigBundle,
) (reviewconfig.ConfigBundle, error) {
	baseline, err := defaultConfigBundleWithProviders(
		service.configSource,
		service.workflow,
		source.Execution.ContextProviders,
		source.Context,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, fmt.Errorf(
			"rebuild baseline config bundle for replay: %w",
			err,
		)
	}
	if source.Target.MaxFiles > baseline.Target.MaxFiles {
		return reviewconfig.ConfigBundle{}, fmt.Errorf(
			"source config max_files %d exceeds current baseline %d",
			source.Target.MaxFiles,
			baseline.Target.MaxFiles,
		)
	}
	if source.Target.MaxPatchBytes > baseline.Target.MaxPatchBytes {
		return reviewconfig.ConfigBundle{}, fmt.Errorf(
			"source config max_patch_bytes %d exceeds current baseline %d",
			source.Target.MaxPatchBytes,
			baseline.Target.MaxPatchBytes,
		)
	}

	baselineInput := baseline.Budget.MaxInputBytes
	baselineOutput := baseline.Budget.MaxOutputBytes
	if source.Budget.MaxInputBytes != baselineInput ||
		source.Budget.MaxOutputBytes != baselineOutput {
		return reviewconfig.ConfigBundle{}, fmt.Errorf(
			"source config budgets are not a supported patch-only invocation tightening",
		)
	}

	effective := service.configSource
	effective.MaxFiles = source.Target.MaxFiles
	effective.MaxPatchBytes = source.Target.MaxPatchBytes
	effective.MaxMaterializedBytes = source.Budget.MaxInputBytes
	expected, err := defaultConfigBundleWithProviders(
		effective,
		service.workflow,
		source.Execution.ContextProviders,
		source.Context,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, fmt.Errorf(
			"rebuild invocation-local config bundle for replay: %w",
			err,
		)
	}
	return expected, nil
}

func configContextPath(request ReviewRequest) string {
	if request.normalizedMode() == reviewcore.TargetModeSelection {
		return request.SelectionPath
	}
	return ""
}

func validateBundleContext(
	bundle reviewconfig.ConfigBundle,
	expected reviewconfig.ResolutionContext,
) error {
	if err := bundle.Validate(); err != nil {
		return fmt.Errorf("validate resolved config bundle: %w", err)
	}
	if bundle.Context != expected {
		return fmt.Errorf(
			"config bundle context %q/%q/%q/%q/%q does not match review invocation",
			bundle.Context.TenantID,
			bundle.Context.OrganizationID,
			bundle.Context.RepositoryID,
			bundle.Context.Path,
			bundle.Context.InvocationID,
		)
	}
	return nil
}

func runtimePolicyFromBundle(
	bundle reviewconfig.ConfigBundle,
) (reviewcore.RuntimePolicy, error) {
	if err := bundle.Validate(); err != nil {
		return reviewcore.RuntimePolicy{}, err
	}
	if bundle.Execution.AgentProfile.ID != "deterministic-local" ||
		bundle.Execution.AgentProfile.Revision != "1" ||
		bundle.Execution.AgentProfile.SHA256 !=
			descriptorDigest("deterministic-local-agent@1") ||
		bundle.Execution.ModelProfile.ID != "none" ||
		bundle.Execution.ModelProfile.Revision != "1" ||
		bundle.Execution.ModelProfile.SHA256 != descriptorDigest("no-model@1") ||
		bundle.Execution.ModelCredential != nil ||
		len(bundle.Execution.AllowedTools) != 0 ||
		bundle.Budget.MaxConcurrency != 1 ||
		bundle.Budget.StageTimeoutMS > maxLocalStageTimeoutMS {
		return reviewcore.RuntimePolicy{}, fmt.Errorf(
			"local runtime requires the exact deterministic-local execution profile",
		)
	}
	rules := make([]reviewcore.RuntimeRule, len(bundle.RulePack.Rules))
	for index, rule := range bundle.RulePack.Rules {
		if rule.Enabled {
			descriptor, registered := reviewcore.LookupRuleDescriptor(rule.ID)
			if !registered {
				return reviewcore.RuntimePolicy{}, fmt.Errorf(
					"enabled local rule %q is unsupported",
					rule.ID,
				)
			}
			if rule.Revision != descriptor.Revision {
				return reviewcore.RuntimePolicy{}, fmt.Errorf(
					"enabled local rule %q has unsupported revision %q",
					rule.ID,
					rule.Revision,
				)
			}
			if rule.Kind != "deterministic" {
				return reviewcore.RuntimePolicy{}, fmt.Errorf(
					"enabled local rule %q has unsupported kind %q",
					rule.ID,
					rule.Kind,
				)
			}
			if rule.Detector.ID != descriptor.DetectorID ||
				rule.Detector.Revision != descriptor.DetectorRevision ||
				rule.Detector.SHA256 != descriptorDigest(
					descriptor.DetectorID+"@"+descriptor.DetectorRevision,
				) {
				return reviewcore.RuntimePolicy{}, fmt.Errorf(
					"enabled local rule %q has unsupported detector identity",
					rule.ID,
				)
			}
			if !slices.Equal(rule.Languages, []string{"go"}) {
				return reviewcore.RuntimePolicy{}, fmt.Errorf(
					"enabled local rule %q has unsupported languages",
					rule.ID,
				)
			}
			if !slices.Equal(rule.EvidenceKinds, baselineEvidenceKinds) {
				return reviewcore.RuntimePolicy{}, fmt.Errorf(
					"enabled local rule %q has unsupported evidence kinds",
					rule.ID,
				)
			}
		}
		rules[index] = reviewcore.RuntimeRule{
			RuleID:           rule.ID,
			DetectorID:       rule.Detector.ID,
			DetectorRevision: rule.Detector.Revision,
			Enabled:          rule.Enabled,
			Severity:         rule.Severity,
			Languages:        slices.Clone(rule.Languages),
			PathPrefixes:     slices.Clone(rule.PathPrefixes),
		}
	}
	evidenceKinds := make(
		[]reviewcore.EvidenceKind,
		len(bundle.Verification.RequiredEvidenceKinds),
	)
	for index, kind := range bundle.Verification.RequiredEvidenceKinds {
		evidenceKinds[index] = reviewcore.EvidenceKind(kind)
	}
	policy := reviewcore.RuntimePolicy{
		Rules:                      rules,
		RequiredEvidenceKinds:      evidenceKinds,
		MinimumIndependentEvidence: bundle.Verification.MinimumIndependentEvidence,
		AllowPartialEvidence: bundle.Verification.PartialEvidence ==
			reviewconfig.PermissionAllow,
		TargetComplete:          true,
		MinimumSeverity:         bundle.Adjudication.MinimumSeverity,
		HumanReviewInconclusive: bundle.Adjudication.HumanReviewInconclusive,
	}
	if err := policy.Validate(); err != nil {
		return reviewcore.RuntimePolicy{}, err
	}
	return policy, nil
}

func descriptorDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validateLocalIdentity(name string, value string) error {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	for _, character := range value {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) &&
			!strings.ContainsRune("._~-", character) {
			return fmt.Errorf("%s contains an unsafe character", name)
		}
	}
	return nil
}

func (service *Service) executeStageForPolicy(
	ctx context.Context,
	input reviewcore.ReviewInput,
	stage reviewcore.StageName,
	upstream reviewcore.StageResult,
	policy reviewcore.RuntimePolicy,
) (reviewcore.StageResult, error) {
	if service.customExecutor {
		return service.executeStage(ctx, input, stage, upstream, policy)
	}
	return reviewcore.ExecuteStageWithPolicy(ctx, input, stage, upstream, policy)
}
