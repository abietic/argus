// Package piexecution implements Argus' formal local Pi PlatformPort. It owns
// provider-side exact idempotency and completion/callback ledgers; it does not
// own ReviewRun, Finding, Decision, Feedback, or evaluation state.
package piexecution

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/execution"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	RuntimeKindLocalPi   = "node-runtime"
	capabilityVerifierID = "argus-local-pi-capability-resolver"
	callbackVerifierID   = "argus-local-pi-completion-ledger"
)

type ArtifactIO interface {
	Resolve(context.Context, contractsv1alpha1.ArtifactBinding) ([]byte, error)
	Publish(
		context.Context,
		string,
		[]byte,
		string,
		time.Time,
	) (contractsv1alpha1.ArtifactBinding, error)
}

type LocalArtifactIO interface {
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
}

type RuntimeManifest struct {
	Runtime       contractsv1alpha1.VersionedRef
	BuildIdentity string
	Agent         contractsv1alpha1.VersionedRef
	Provider      contractsv1alpha1.VersionedRef
	Model         contractsv1alpha1.VersionedRef
	Prompt        contractsv1alpha1.VersionedRef
	ReviewSkills  []contractsv1alpha1.VersionedRef
	Knowledge     []contractsv1alpha1.VersionedRef
	SHA256        string
}

// PricingCeiling is a configured worst-case micro-currency rate. Argus admits
// a plan only when the maximum possible frozen-input and output-token spend is
// within MaxCostMicros; observed post-hoc usage never substitutes for this
// pre-execution ceiling.
type PricingCeiling struct {
	InputMicrosPerMillionTokens  uint64
	OutputMicrosPerMillionTokens uint64
	MaximumBytesPerInputToken    uint64
}

type Config struct {
	Subject        application.AgentPlanningSubject
	NodePath       string
	WorkerScript   string
	Environment    map[string]string
	Manifest       RuntimeManifest
	Pricing        PricingCeiling
	Runner         agentshadowworker.Runner
	Artifacts      ArtifactIO
	LocalArtifacts LocalArtifactIO
	RuntimeFiles   RuntimeFileVerifier
}

type CapabilityResolver struct {
	manifest RuntimeManifest
	pricing  PricingCeiling
}

var _ application.AgentExecutorCapabilityResolver = (*CapabilityResolver)(nil)

func NewCapabilityResolver(
	manifest RuntimeManifest,
	pricing PricingCeiling,
) (*CapabilityResolver, error) {
	if err := validateRuntimeManifest(manifest); err != nil {
		return nil, err
	}
	if err := validatePricing(pricing); err != nil {
		return nil, err
	}
	return &CapabilityResolver{manifest: manifest, pricing: pricing}, nil
}

func (resolver *CapabilityResolver) ResolveAgentExecutorCapability(
	ctx context.Context,
	_ application.AgentPlanningSubject,
	plan contractsv1alpha1.AgentStagePlan,
) (contractsv1alpha1.ExecutorCapabilitySnapshot, error) {
	if ctx == nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, err
	}
	if resolver == nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, fmt.Errorf(
			"Pi capability resolver is not initialized",
		)
	}
	if err := validatePlanAgainstManifest(plan, resolver.manifest, resolver.pricing); err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, err
	}
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind:     resolver.manifest.Runtime.ID,
		RuntimeID:       resolver.manifest.Runtime.ID,
		RuntimeRevision: resolver.manifest.Runtime.Revision,
		RuntimeSHA256:   resolver.manifest.Runtime.SHA256,
		BuildIdentity:   resolver.manifest.BuildIdentity,
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:       append([]string{}, plan.ToolAuthority.Tools...),
			ModelEgress:        plan.ModelAuthority.ModelEgress,
			ToolNetwork:        plan.ToolAuthority.ToolNetwork,
			WorkspaceReads:     plan.ToolAuthority.WorkspaceReads,
			WorkspaceWrites:    plan.ToolAuthority.WorkspaceWrites,
			RemoteWrites:       plan.ToolAuthority.RemoteWrites,
			MaxDelegationDepth: plan.ToolAuthority.MaxDelegationDepth,
		},
		Trust: contractsv1alpha1.ExecutorTrust{
			Authority: contractsv1alpha1.ExecutorTrustAuthorityLocalHost,
			CapabilityVerifier: contractsv1alpha1.VersionedRef{
				ID: capabilityVerifierID, Revision: "v1", SHA256: resolver.manifest.SHA256,
			},
			CallbackVerifier: contractsv1alpha1.VersionedRef{
				ID: callbackVerifierID, Revision: "v1", SHA256: resolver.manifest.SHA256,
			},
		},
	}
	var err error
	capability.SHA256, err = contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, err
	}
	return capability, nil
}

func validateRuntimeManifest(manifest RuntimeManifest) error {
	for name, ref := range map[string]contractsv1alpha1.VersionedRef{
		"runtime": manifest.Runtime, "agent": manifest.Agent,
		"provider": manifest.Provider, "model": manifest.Model, "prompt": manifest.Prompt,
	} {
		if ref.ID == "" || ref.Revision == "" || !lowerSHA256(ref.SHA256) {
			return fmt.Errorf("Pi runtime manifest %s ref is incomplete", name)
		}
	}
	if manifest.Runtime.ID != RuntimeKindLocalPi ||
		!supportedBuildRevision(manifest.Runtime, "binary") ||
		manifest.Agent.ID != "pi-agent" ||
		!supportedBuildRevision(manifest.Agent, "worker-v0") ||
		manifest.Provider.Revision != "v1" || manifest.Model.Revision != "provider" ||
		manifest.Prompt.ID != "pi-review-prompts" || strings.EqualFold(manifest.Prompt.Revision, "latest") ||
		manifest.BuildIdentity == "" || !lowerSHA256(manifest.SHA256) ||
		len(manifest.ReviewSkills) == 0 {
		return fmt.Errorf("Pi runtime manifest identity is unsupported or incomplete")
	}
	return nil
}

func supportedBuildRevision(ref contractsv1alpha1.VersionedRef, base string) bool {
	return ref.Revision == base ||
		ref.Revision == base+"-"+ref.SHA256[:16]
}

func validatePricing(pricing PricingCeiling) error {
	if pricing.InputMicrosPerMillionTokens == 0 ||
		pricing.OutputMicrosPerMillionTokens == 0 ||
		pricing.MaximumBytesPerInputToken == 0 {
		return fmt.Errorf("Pi worst-case pricing ceiling is required")
	}
	return nil
}

func validatePlanAgainstManifest(
	plan contractsv1alpha1.AgentStagePlan,
	manifest RuntimeManifest,
	pricing PricingCeiling,
) error {
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate formal Pi plan: %w", err)
	}
	if plan.Runtime.Ref != manifest.Runtime || plan.Agent.Ref != manifest.Agent ||
		plan.Provider.Ref != manifest.Provider || plan.Model.Ref != manifest.Model ||
		plan.Prompt.Ref != manifest.Prompt || plan.BuildIdentity != manifest.BuildIdentity {
		return fmt.Errorf("formal Pi plan differs from the selected immutable runtime manifest")
	}
	// Context providers are host-side, pre-materialization components. The Pi
	// runtime consumes only the exact context artifacts already frozen in the
	// ReviewInput; application admission verifies adapter artifacts and receipts.
	reviewSkills := make([]contractsv1alpha1.VersionedRef, 0)
	for _, skill := range plan.Skills {
		if skill.Phase == contractsv1alpha1.AgentStageSkillPhaseReview {
			reviewSkills = append(reviewSkills, skill.Ref)
		}
	}
	if !slices.Equal(reviewSkills, manifest.ReviewSkills) {
		return fmt.Errorf("formal Pi review skills differ from the runtime manifest")
	}
	knowledge := make([]contractsv1alpha1.VersionedRef, len(plan.Knowledge))
	for index, binding := range plan.Knowledge {
		knowledge[index] = binding.Ref
	}
	if !slices.Equal(knowledge, manifest.Knowledge) {
		return fmt.Errorf("formal Pi knowledge differs from the runtime manifest")
	}
	if plan.ModelAuthority.APIProtocol.Ref.ID != "anthropic-messages" ||
		plan.ModelAuthority.ModelEgress != contractsv1alpha1.AgentStageModelEgressProviderBrokerOnly ||
		plan.SideEffects != contractsv1alpha1.AgentStageSideEffectsDeny {
		return fmt.Errorf("formal Pi model protocol or side-effect authority is unsupported")
	}
	return validateWorstCaseCost(plan.Budget, pricing)
}

func validateWorstCaseCost(
	budget contractsv1alpha1.AgentStageBudget,
	pricing PricingCeiling,
) error {
	if err := validatePricing(pricing); err != nil {
		return err
	}
	if budget.MaxCostMicros <= 0 || budget.MaxModelCalls <= 0 ||
		budget.MaxTargetBytes <= 0 || budget.MaxOutputTokens <= 0 {
		return fmt.Errorf("formal Pi cost budget is incomplete")
	}
	// The conservative input-token bound assumes at least one byte per token
	// unless a stricter configured maximum is supplied. This overestimates cost
	// but never silently admits an unenforceable ceiling.
	inputTokens := uint64(budget.MaxTargetBytes)
	if pricing.MaximumBytesPerInputToken > 1 {
		inputTokens = uint64(budget.MaxTargetBytes)/pricing.MaximumBytesPerInputToken + 1
	}
	calls := uint64(budget.MaxModelCalls)
	outputTokens := uint64(budget.MaxOutputTokens)
	if calls > math.MaxUint64/max(inputTokens, outputTokens) {
		return fmt.Errorf("formal Pi cost ceiling overflows")
	}
	inputUnits := calls * inputTokens
	outputUnits := calls * outputTokens
	if inputUnits > math.MaxUint64/pricing.InputMicrosPerMillionTokens ||
		outputUnits > math.MaxUint64/pricing.OutputMicrosPerMillionTokens {
		return fmt.Errorf("formal Pi cost ceiling overflows")
	}
	inputCost, ok := ceilMillionProduct(inputUnits, pricing.InputMicrosPerMillionTokens)
	if !ok {
		return fmt.Errorf("formal Pi cost ceiling overflows")
	}
	outputCost, ok := ceilMillionProduct(outputUnits, pricing.OutputMicrosPerMillionTokens)
	if !ok || math.MaxUint64-inputCost < outputCost {
		return fmt.Errorf("formal Pi cost ceiling overflows")
	}
	worstMicros := inputCost + outputCost
	if worstMicros > uint64(budget.MaxCostMicros) {
		return fmt.Errorf(
			"formal Pi worst-case cost %d micros exceeds admitted max_cost_micros %d",
			worstMicros,
			budget.MaxCostMicros,
		)
	}
	return nil
}

func ceilMillionProduct(left uint64, right uint64) (uint64, bool) {
	if left != 0 && right > math.MaxUint64/left {
		return 0, false
	}
	product := left * right
	quotient := product / 1_000_000
	if product%1_000_000 != 0 {
		if quotient == math.MaxUint64 {
			return 0, false
		}
		quotient++
	}
	return quotient, true
}

func lowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

var _ execution.PlatformPort = (*Platform)(nil)
