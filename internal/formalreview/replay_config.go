package formalreview

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/workflow"
)

var formalBudgetReplayChangedFields = []string{
	"agent_review.budget.timeout_ms",
	"budget.stage_timeout_ms",
}

var formalModelReplayChangedFields = []string{"agent_review.model"}

var formalPromptReplayChangedFields = []string{"agent_review.prompt"}

var formalSkillPackReplayChangedFields = []string{"agent_review.skill_packs"}
var formalKnowledgePackReplayChangedFields = []string{"agent_review.knowledge_packs"}
var formalRulePackReplayChangedFields = []string{"rule_pack"}
var formalFilterPolicyReplayChangedFields = []string{"finding_governance"}

func FormalBudgetReplayChangedFields() []string {
	return slices.Clone(formalBudgetReplayChangedFields)
}

func FormalModelReplayChangedFields() []string {
	return slices.Clone(formalModelReplayChangedFields)
}

func FormalPromptReplayChangedFields() []string {
	return slices.Clone(formalPromptReplayChangedFields)
}

func FormalSkillPackReplayChangedFields() []string {
	return slices.Clone(formalSkillPackReplayChangedFields)
}

func FormalKnowledgePackReplayChangedFields() []string {
	return slices.Clone(formalKnowledgePackReplayChangedFields)
}

func FormalRulePackReplayChangedFields() []string {
	return slices.Clone(formalRulePackReplayChangedFields)
}

func FormalFindingGovernanceReplayChangedFields() []string {
	return FormalFilterPolicyReplayChangedFields()
}

func FormalFilterPolicyReplayChangedFields() []string {
	return slices.Clone(formalFilterPolicyReplayChangedFields)
}

// BuildWorkflowReplayConfig derives a ConfigBundle that changes only the
// exact WorkflowDefinition binding. The separately supplied definition bytes
// remain the executable source of truth and are persisted by the initializer.
// Formal v1alpha1 workflow experiments may tighten only stage resources that
// are consumed by AgentStagePlan; graph, retry, executor and authority remain
// frozen.
func BuildWorkflowReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	baselineDefinition workflow.Definition,
	variantDefinition workflow.Definition,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, []string, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil,
			fmt.Errorf("validate formal workflow replay baseline: %w", err)
	}
	fields, err := validateFormalWorkflowVariant(
		baseline, baselineDefinition, variantDefinition,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil, err
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil, err
	}
	digest, err := workflow.DigestDefinition(variantDefinition)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil, err
	}
	variant.Workflow.Definition = reviewconfig.VersionedRef{
		ID: variantDefinition.ID, Revision: variantDefinition.Revision, SHA256: digest,
	}
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil,
			fmt.Errorf("validate formal workflow variant config: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, string(runmodel.ReplayVariableWorkflow), fields,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, nil, err
	}
	return variant, receipt, fields, nil
}

func validateFormalWorkflowVariant(
	baselineBundle reviewconfig.ConfigBundle,
	baseline workflow.Definition,
	variant workflow.Definition,
) ([]string, error) {
	if baselineBundle.AgentReview == nil {
		return nil, fmt.Errorf("formal workflow replay baseline has no agent_review policy")
	}
	baselineDigest, err := workflow.DigestDefinition(baseline)
	if err != nil {
		return nil, fmt.Errorf("validate baseline formal workflow: %w", err)
	}
	if baselineBundle.Workflow.Definition != (reviewconfig.VersionedRef{
		ID: baseline.ID, Revision: baseline.Revision, SHA256: baselineDigest,
	}) {
		return nil, fmt.Errorf("formal workflow replay baseline config does not bind exact definition")
	}
	fields, earliest, err := workflow.DiffReplayExecutionPolicy(baseline, variant)
	if err != nil {
		return nil, fmt.Errorf("validate formal workflow replay definition: %w", err)
	}
	if earliest != "agent_hypothesize" || len(baseline.Stages) != 1 || len(variant.Stages) != 1 {
		return nil, fmt.Errorf("formal workflow replay must affect the single agent_hypothesize stage")
	}
	for _, field := range fields {
		if !strings.HasPrefix(field, "workflow.stages.agent_hypothesize.budget.") &&
			!strings.HasPrefix(field, "workflow.stages.agent_hypothesize.retry.") {
			return nil, fmt.Errorf("formal workflow replay may change only executable stage budget or retry policy")
		}
	}
	policyBudget := baselineBundle.AgentReview.Budget
	if !workflow.ReplayChangesEffectivePolicy(
		baseline,
		variant,
		workflow.RuntimeLimits{
			TimeoutMS:      policyBudget.TimeoutMS,
			MaxInputBytes:  policyBudget.MaxTargetBytes,
			MaxOutputBytes: policyBudget.MaxOutputBytes,
			MaxAttempts:    baselineBundle.Budget.MaxAttempts,
			MaxConcurrency: policyBudget.MaxConcurrency,
		},
	) {
		return nil, fmt.Errorf(
			"formal workflow replay does not change effective AgentStagePlan budget",
		)
	}
	return fields, nil
}

func validateFormalWorkflowConfigVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
	baselineDefinition workflow.Definition,
	variantDefinition workflow.Definition,
) ([]string, error) {
	fields, err := validateFormalWorkflowVariant(
		baseline, baselineDefinition, variantDefinition,
	)
	if err != nil {
		return nil, err
	}
	if err := receipt.ValidateAgainst(variant); err != nil {
		return nil, fmt.Errorf("validate formal workflow variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID ||
		binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableWorkflow) ||
		!slices.Equal(binding.ChangedFields, fields) {
		return nil, fmt.Errorf("formal workflow variant receipt does not bind baseline and exact diff")
	}
	baselineCopy, variantCopy := baseline, variant
	baselineWorkflow, variantWorkflow := baselineCopy.Workflow, variantCopy.Workflow
	baselineCopy.Workflow, variantCopy.Workflow = reviewconfig.WorkflowPolicy{}, reviewconfig.WorkflowPolicy{}
	baselineCopy.BundleID, baselineCopy.SHA256 = "", ""
	variantCopy.BundleID, variantCopy.SHA256 = "", ""
	variantDigest, digestErr := workflow.DigestDefinition(variantDefinition)
	if digestErr != nil {
		return nil, digestErr
	}
	if reflect.DeepEqual(baselineWorkflow, variantWorkflow) ||
		!reflect.DeepEqual(baselineCopy, variantCopy) ||
		variantWorkflow.Definition != (reviewconfig.VersionedRef{
			ID: variantDefinition.ID, Revision: variantDefinition.Revision, SHA256: variantDigest,
		}) {
		return nil, fmt.Errorf("formal workflow replay must change only the exact workflow binding")
	}
	return fields, nil
}

// BuildIndexReplayConfig derives an exact formal variant whose sole behavior
// change is the ordered context-provider policy. Context bytes are materialized
// later from the source run's immutable repository/commit/target paths.
func BuildIndexReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	providers []reviewconfig.ContextProviderDefinition,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal index replay baseline: %w", err)
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.Execution.ContextProviders = slices.Clone(providers)
	fields, err := reviewconfig.DiffReplayIndexPolicy(
		baseline.Execution, variant.Execution,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal index policy: %w", err)
	}
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal index variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, string(runmodel.ReplayVariableIndex), fields,
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// BuildRulePackReplayConfig derives a bundle whose only changed behavior is
// the exact governed defect-rule taxonomy consumed by the Pi worker. The
// supplied pack must already be sealed; normal config publication remains the
// only path for changing unrelated policy.
func BuildRulePackReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	pack reviewconfig.RulePack,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal rule_pack replay baseline: %w", err)
	}
	if err := pack.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal rule_pack: %w", err)
	}
	if reflect.DeepEqual(baseline.RulePack, pack) {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal rule_pack replay requires one changed sealed rule pack")
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	packData, err := json.Marshal(pack)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.RulePack, err = reviewconfig.DecodeRulePack(packData)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal rule_pack variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, string(runmodel.ReplayVariableRulePack),
		FormalRulePackReplayChangedFields(),
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// BuildFilterPolicyReplayConfig derives a bundle whose only changed behavior
// is post-verification finding calibration/suppression. Agent inputs, runtime,
// workflow and every other governed fact stay byte-identical to the baseline.
func BuildFilterPolicyReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	policy reviewconfig.FindingGovernancePolicy,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	return buildFilterPolicyReplayConfig(
		baseline, sourceReceipt, policy, runmodel.ReplayVariableFilterPolicy,
	)
}

// BuildFindingGovernanceReplayConfig retains the legacy wire variable for
// reconstructing and retrying immutable v1alpha1 runs created before
// filter_policy became the canonical public name. New callers should use
// BuildFilterPolicyReplayConfig.
func BuildFindingGovernanceReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	policy reviewconfig.FindingGovernancePolicy,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	return buildFilterPolicyReplayConfig(
		baseline, sourceReceipt, policy, runmodel.ReplayVariableFindingGovernance,
	)
}

func buildFilterPolicyReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	policy reviewconfig.FindingGovernancePolicy,
	variable runmodel.ReplayVariable,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if !variable.IsFilterPolicy() {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal filter policy replay variable is invalid")
	}
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal replay baseline: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal filter policy: %w", err)
	}
	if baseline.FindingGovernance == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal filter_policy replay requires finding governance enabled by normal config publication")
	}
	if reflect.DeepEqual(*baseline.FindingGovernance, policy) {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal filter_policy replay requires one changed finding governance policy")
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	policyCopy := policy
	policyCopy.CalibrationProfile.Points = slices.Clone(policy.CalibrationProfile.Points)
	variant.FindingGovernance = &policyCopy
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal filter_policy variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, string(variable),
		FormalFilterPolicyReplayChangedFields(),
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// BuildBudgetReplayConfig derives one executable experimental bundle from an
// exact governed source. Both the outer stage ceiling and the Pi agent budget
// change together; every other config fact and component identity is frozen.
func BuildBudgetReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	timeoutMS int64,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal replay baseline: %w", err)
	}
	if baseline.AgentReview == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal replay baseline has no agent_review policy")
	}
	if timeoutMS <= 0 || timeoutMS == baseline.Budget.StageTimeoutMS ||
		timeoutMS == baseline.AgentReview.Budget.TimeoutMS {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal budget replay requires one positive changed timeout")
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.Budget.StageTimeoutMS = timeoutMS
	variant.AgentReview.Budget.TimeoutMS = timeoutMS
	variant.AgentReview.SHA256 = ""
	policyDigest, err := reviewconfig.DigestAgentReviewPolicy(*variant.AgentReview)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.SHA256 = policyDigest
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal budget variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, "budget", FormalBudgetReplayChangedFields(),
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// BuildModelReplayConfig derives one experimental bundle whose only behavior
// change is the exact governed model component reference. Runtime, provider,
// prompt, skills, authority, and all budgets remain frozen.
func BuildModelReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	model reviewconfig.VersionedRef,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal replay baseline: %w", err)
	}
	if baseline.AgentReview == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal replay baseline has no agent_review policy")
	}
	if err := model.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal model replay component: %w", err)
	}
	if baseline.AgentReview.Model == model {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal model replay requires one changed model component")
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.Model = model
	variant.AgentReview.SHA256 = ""
	policyDigest, err := reviewconfig.DigestAgentReviewPolicy(*variant.AgentReview)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.SHA256 = policyDigest
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal model variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, "model", FormalModelReplayChangedFields(),
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// BuildPromptReplayConfig derives one experimental bundle whose only behavior
// change is the exact governed prompt component reference. Runtime, model,
// skills, authority, and all budgets remain frozen.
func BuildPromptReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	prompt reviewconfig.VersionedRef,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal replay baseline: %w", err)
	}
	if baseline.AgentReview == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal replay baseline has no agent_review policy")
	}
	if err := prompt.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal prompt replay component: %w", err)
	}
	if baseline.AgentReview.Prompt == prompt {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal prompt replay requires one changed prompt component")
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.Prompt = prompt
	variant.AgentReview.SHA256 = ""
	policyDigest, err := reviewconfig.DigestAgentReviewPolicy(*variant.AgentReview)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.SHA256 = policyDigest
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal prompt variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, "prompt", FormalPromptReplayChangedFields(),
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// BuildSkillPackReplayConfig derives one experimental bundle whose only
// behavior change is one or more governed Agent review skill artifacts. The
// ordered skill identities and phases are deliberately frozen: introducing or
// removing a dimension changes configuration provenance and must go through
// the normal revision publication lifecycle instead of an atomic replay.
func BuildSkillPackReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	skills []reviewconfig.AgentSkillPackDefinition,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal replay baseline: %w", err)
	}
	if baseline.AgentReview == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal replay baseline has no agent_review policy")
	}
	baselineSkills := baseline.AgentReview.SkillPacks
	if len(skills) == 0 || len(skills) != len(baselineSkills) {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal skill_pack replay must preserve the governed skill set")
	}
	changed := false
	for index := range skills {
		if err := skills[index].Validate(); err != nil {
			return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
				fmt.Errorf("validate formal skill_pack replay component %d: %w", index, err)
		}
		if skills[index].ID != baselineSkills[index].ID ||
			skills[index].Phase != baselineSkills[index].Phase {
			return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
				fmt.Errorf("formal skill_pack replay cannot add, remove, reorder, or rephase skills")
		}
		if skills[index].Ref != baselineSkills[index].Ref {
			changed = true
		}
	}
	if !changed {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal skill_pack replay requires at least one changed skill artifact")
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.SkillPacks = slices.Clone(skills)
	variant.AgentReview.SHA256 = ""
	policyDigest, err := reviewconfig.DigestAgentReviewPolicy(*variant.AgentReview)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.SHA256 = policyDigest
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal skill_pack variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant,
		baseline,
		sourceReceipt,
		string(runmodel.ReplayVariableSkillPack),
		FormalSkillPackReplayChangedFields(),
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// BuildKnowledgePackReplayConfig changes only exact governed knowledge
// artifacts. IDs and order remain frozen so the derived replay receipt can
// preserve lifecycle provenance rather than inventing new FieldSources.
func BuildKnowledgePackReplayConfig(
	baseline reviewconfig.ConfigBundle,
	sourceReceipt reviewconfig.ConfigResolutionReceipt,
	knowledge []reviewconfig.AgentKnowledgePackDefinition,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := sourceReceipt.ValidateAgainst(baseline); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal replay baseline: %w", err)
	}
	if baseline.AgentReview == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal replay baseline has no agent_review policy")
	}
	baselineKnowledge := baseline.AgentReview.KnowledgePacks
	if len(knowledge) == 0 || len(knowledge) != len(baselineKnowledge) {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal knowledge_pack replay must preserve the governed knowledge set")
	}
	changed := false
	for index := range knowledge {
		if err := knowledge[index].Validate(); err != nil {
			return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
				fmt.Errorf("validate formal knowledge_pack replay component %d: %w", index, err)
		}
		if knowledge[index].ID != baselineKnowledge[index].ID {
			return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
				fmt.Errorf("formal knowledge_pack replay cannot add, remove, or reorder knowledge")
		}
		if knowledge[index].Ref != baselineKnowledge[index].Ref {
			changed = true
		}
	}
	if !changed {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("formal knowledge_pack replay requires at least one changed knowledge artifact")
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.KnowledgePacks = slices.Clone(knowledge)
	variant.AgentReview.SHA256 = ""
	policyDigest, err := reviewconfig.DigestAgentReviewPolicy(*variant.AgentReview)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.AgentReview.SHA256 = policyDigest
	variant.BundleID, variant.SHA256 = "", ""
	bundleDigest, err := reviewconfig.DigestBundle(variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	variant.SHA256 = bundleDigest
	variant.BundleID = "bundle-" + bundleDigest[:24]
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate formal knowledge_pack variant: %w", err)
	}
	receipt, err := reviewconfig.NewReplayVariantConfigReceipt(
		variant, baseline, sourceReceipt, string(runmodel.ReplayVariableKnowledgePack),
		FormalKnowledgePackReplayChangedFields(),
	)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return variant, receipt, nil
}

// FrozenReplayConfigProvider replays one historical, already-governed config
// resolution. It never reads lifecycle "latest" and only answers the exact
// source ResolutionContext carried by the frozen bundle/receipt.
type FrozenReplayConfigProvider struct {
	bundle  reviewconfig.ConfigBundle
	receipt reviewconfig.ConfigResolutionReceipt
}

var _ application.GovernedConfigProvider = (*FrozenReplayConfigProvider)(nil)

func NewFrozenReplayConfigProvider(
	bundle reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) (*FrozenReplayConfigProvider, error) {
	if err := bundle.Validate(); err != nil {
		return nil, fmt.Errorf("validate frozen replay config: %w", err)
	}
	if err := receipt.ValidateAgainst(bundle); err != nil {
		return nil, fmt.Errorf("validate frozen replay config receipt: %w", err)
	}
	clonedBundle, clonedReceipt, err := cloneReplayConfig(bundle, receipt)
	if err != nil {
		return nil, err
	}
	return &FrozenReplayConfigProvider{bundle: clonedBundle, receipt: clonedReceipt}, nil
}

func (provider *FrozenReplayConfigProvider) ResolvePublished(
	ctx context.Context,
	resolution reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, error) {
	bundle, _, err := provider.ResolvePublishedWithReceipt(ctx, resolution)
	return bundle, err
}

func (provider *FrozenReplayConfigProvider) ResolvePublishedWithReceipt(
	ctx context.Context,
	resolution reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if ctx == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	if provider == nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("frozen replay config provider is not initialized")
	}
	if resolution != provider.bundle.Context {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("replay config resolution context differs from frozen source")
	}
	bundle, receipt, err := cloneReplayConfig(provider.bundle, provider.receipt)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return bundle, receipt, nil
}

func cloneReplayConfig(
	bundle reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("marshal frozen replay config: %w", err)
	}
	clonedBundle, err := reviewconfig.DecodeBundle(bundleBytes)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	receiptBytes, err := json.Marshal(receipt)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("marshal frozen replay config receipt: %w", err)
	}
	clonedReceipt, err := reviewconfig.DecodeConfigResolutionReceipt(receiptBytes)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	return clonedBundle, clonedReceipt, nil
}
