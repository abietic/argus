package application

import (
	"fmt"
	"reflect"
	"slices"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/workflow"
)

func (service *Service) prepareReplayBundle(
	source reviewconfig.ConfigBundle,
	request ReplayRequest,
	start reviewcore.StageName,
) (
	reviewconfig.ConfigBundle,
	workflow.Definition,
	runmodel.ReplayVariable,
	[]string,
	error,
) {
	if request.VariantConfigBundle == nil {
		if request.Variable != "" && request.Variable != runmodel.ReplayVariableNone {
			return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
				fmt.Errorf("replay variable %q requires a variant ConfigBundle", request.Variable)
		}
		if request.VariantWorkflow != nil {
			return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
				fmt.Errorf("variant workflow requires replay variable %q", runmodel.ReplayVariableWorkflow)
		}
		return source, service.workflow, runmodel.ReplayVariableNone, []string{}, nil
	}
	if request.Variable == "" || request.Variable == runmodel.ReplayVariableNone {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
			fmt.Errorf("variant ConfigBundle requires one explicit replay variable")
	}
	if err := request.Variable.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil, err
	}
	variant := *request.VariantConfigBundle
	if err := variant.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
			fmt.Errorf("validate variant ConfigBundle: %w", err)
	}
	if variant.Context != source.Context {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
			fmt.Errorf("variant ConfigBundle must bind the exact source resolution context")
	}
	if source.SHA256 == variant.SHA256 {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
			fmt.Errorf("variant ConfigBundle does not change behavior")
	}

	actual, changedFields, earliest, err := classifyReplayBundleChange(source, variant)
	if err != nil {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil, err
	}
	if actual != request.Variable {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil, fmt.Errorf(
			"declared replay variable %q does not match exact change %q",
			request.Variable, actual,
		)
	}
	executionWorkflow := service.workflow
	if actual == runmodel.ReplayVariableWorkflow {
		if request.VariantWorkflow == nil {
			return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
				fmt.Errorf("workflow replay requires one exact variant WorkflowDefinition")
		}
		executionWorkflow = *request.VariantWorkflow
		changedFields, earliest, err = validateWorkflowReplayVariant(
			service.workflow,
			executionWorkflow,
			variant.Workflow,
			variant.Budget,
		)
		if err != nil {
			return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil, err
		}
	} else if request.VariantWorkflow != nil {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
			fmt.Errorf("variant WorkflowDefinition is only valid for workflow replay")
	}
	if earliest != "" && stageIndex(start) > stageIndex(earliest) {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil, fmt.Errorf(
			"replay starts at %q but variable %q first affects %q; affected checkpoints cannot be reused",
			start, actual, earliest,
		)
	}
	if _, err := runtimePolicyFromBundle(variant); err != nil {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
			fmt.Errorf("variant is not executable by the local stage implementation: %w", err)
	}
	if _, err := runtimeConfigFromBundle(variant, service.configSource); err != nil {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil,
			fmt.Errorf("variant runtime limits are invalid: %w", err)
	}
	if err := validateRuntimeEnvelope(variant, executionWorkflow); err != nil {
		return reviewconfig.ConfigBundle{}, workflow.Definition{}, "", nil, err
	}
	return variant, executionWorkflow, actual, changedFields, nil
}

func classifyReplayBundleChange(
	source reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
) (runmodel.ReplayVariable, []string, reviewcore.StageName, error) {
	if !reflect.DeepEqual(source.Target, variant.Target) {
		return "", nil, "", fmt.Errorf("variant replay cannot change target authorization")
	}
	if !reflect.DeepEqual(source.AgentReview, variant.AgentReview) {
		return "", nil, "", fmt.Errorf(
			"agent_review variant requires the formal typed execution adapter",
		)
	}
	if !reflect.DeepEqual(source.Publication, variant.Publication) {
		return "", nil, "", fmt.Errorf("variant replay cannot change publication policy")
	}
	if !reflect.DeepEqual(source.Data, variant.Data) {
		return "", nil, "", fmt.Errorf("variant replay cannot change data policy")
	}
	indexFields := []string{}
	if !reflect.DeepEqual(source.Execution, variant.Execution) {
		var err error
		indexFields, err = reviewconfig.DiffReplayIndexPolicy(
			source.Execution,
			variant.Execution,
		)
		if err != nil {
			return "", nil, "", err
		}
	}

	type category struct {
		variable runmodel.ReplayVariable
		changed  bool
		fields   []string
		earliest reviewcore.StageName
	}
	categories := []category{
		{
			variable: runmodel.ReplayVariableIndex,
			changed:  len(indexFields) != 0,
			fields:   indexFields,
			earliest: reviewcore.StageMaterializeTarget,
		},
		{
			variable: runmodel.ReplayVariableWorkflow,
			changed:  !reflect.DeepEqual(source.Workflow, variant.Workflow),
			fields:   []string{"workflow.definition"},
			earliest: reviewcore.StageDetect,
		},
		{
			variable: runmodel.ReplayVariableRulePack,
			changed:  !reflect.DeepEqual(source.RulePack, variant.RulePack),
			fields:   []string{"rule_pack"},
			earliest: reviewcore.StageDetect,
		},
		filterChange(source, variant),
		budgetChange(source, variant),
	}
	var selected *category
	for index := range categories {
		if !categories[index].changed {
			continue
		}
		if selected != nil {
			return "", nil, "", fmt.Errorf(
				"variant ConfigBundle changes more than one atomic variable: %q and %q",
				selected.variable, categories[index].variable,
			)
		}
		selected = &categories[index]
	}
	if selected == nil {
		return "", nil, "", fmt.Errorf(
			"variant ConfigBundle changes only provenance or identity metadata, not behavior",
		)
	}
	if slices.Contains(selected.fields, "budget.unsupported") {
		return "", nil, "", fmt.Errorf(
			"local budget replay may change stage_timeout_ms only; other budgets are not " +
				"enforced by the deterministic stage implementation",
		)
	}
	fields := slices.Clone(selected.fields)
	slices.Sort(fields)
	return selected.variable, fields, selected.earliest, nil
}

func validateWorkflowReplayVariant(
	baseline workflow.Definition,
	variant workflow.Definition,
	binding reviewconfig.WorkflowPolicy,
	budget reviewconfig.BudgetPolicy,
) ([]string, reviewcore.StageName, error) {
	if err := variant.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate workflow replay definition: %w", err)
	}
	if err := validateSupportedWorkflow(variant); err != nil {
		return nil, "", fmt.Errorf("admit workflow replay definition: %w", err)
	}
	digest, err := workflow.DigestDefinition(variant)
	if err != nil {
		return nil, "", err
	}
	if binding.Definition.ID != variant.ID ||
		binding.Definition.Revision != variant.Revision ||
		binding.Definition.SHA256 != digest {
		return nil, "", fmt.Errorf(
			"variant ConfigBundle workflow ref does not bind exact WorkflowDefinition",
		)
	}
	baselineDigest, err := workflow.DigestDefinition(baseline)
	if err != nil {
		return nil, "", err
	}
	if baselineDigest == digest {
		return nil, "", fmt.Errorf("workflow replay does not change executable definition bytes")
	}

	fields, earliest, err := workflow.DiffReplayExecutionPolicy(baseline, variant)
	if err != nil {
		return nil, "", err
	}
	if !workflow.ReplayChangesEffectivePolicy(
		baseline,
		variant,
		workflow.RuntimeLimits{
			TimeoutMS:      budget.StageTimeoutMS,
			MaxInputBytes:  budget.MaxInputBytes,
			MaxOutputBytes: budget.MaxOutputBytes,
			MaxAttempts:    budget.MaxAttempts,
			MaxConcurrency: budget.MaxConcurrency,
		},
	) {
		return nil, "", fmt.Errorf(
			"workflow replay does not change effective scheduler behavior under frozen runtime limits",
		)
	}
	return fields, reviewcore.StageName(earliest), nil
}

func filterChange(
	source reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
) struct {
	variable runmodel.ReplayVariable
	changed  bool
	fields   []string
	earliest reviewcore.StageName
} {
	fields := make([]string, 0)
	earliest := reviewcore.StageAdjudicate
	if !reflect.DeepEqual(source.Verification, variant.Verification) {
		fields = append(fields, "verification")
		earliest = reviewcore.StageVerify
	}
	if !reflect.DeepEqual(source.Adjudication, variant.Adjudication) {
		fields = append(fields, "adjudication")
	}
	return struct {
		variable runmodel.ReplayVariable
		changed  bool
		fields   []string
		earliest reviewcore.StageName
	}{
		variable: runmodel.ReplayVariableFilterPolicy,
		changed:  len(fields) != 0,
		fields:   fields,
		earliest: earliest,
	}
}

func budgetChange(
	source reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
) struct {
	variable runmodel.ReplayVariable
	changed  bool
	fields   []string
	earliest reviewcore.StageName
} {
	if reflect.DeepEqual(source.Budget, variant.Budget) {
		return struct {
			variable runmodel.ReplayVariable
			changed  bool
			fields   []string
			earliest reviewcore.StageName
		}{variable: runmodel.ReplayVariableBudget}
	}
	sourceWithoutTimeout := source.Budget
	variantWithoutTimeout := variant.Budget
	sourceWithoutTimeout.StageTimeoutMS = 0
	variantWithoutTimeout.StageTimeoutMS = 0
	if !reflect.DeepEqual(sourceWithoutTimeout, variantWithoutTimeout) {
		return struct {
			variable runmodel.ReplayVariable
			changed  bool
			fields   []string
			earliest reviewcore.StageName
		}{
			variable: runmodel.ReplayVariableBudget,
			changed:  true,
			fields:   []string{"budget.unsupported"},
		}
	}
	return struct {
		variable runmodel.ReplayVariable
		changed  bool
		fields   []string
		earliest reviewcore.StageName
	}{
		variable: runmodel.ReplayVariableBudget,
		changed:  true,
		fields:   []string{"budget.stage_timeout_ms"},
	}
}

func classifyUnsupportedExecutionChange(
	source reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
) error {
	switch {
	case !reflect.DeepEqual(source.Execution.ModelProfile, variant.Execution.ModelProfile) ||
		!reflect.DeepEqual(source.Execution.ModelCredential, variant.Execution.ModelCredential):
		return fmt.Errorf("model variant requires the typed external execution adapter")
	case !reflect.DeepEqual(source.Execution.ContextProviders, variant.Execution.ContextProviders):
		return fmt.Errorf("index/context variant requires the typed external execution adapter")
	case !reflect.DeepEqual(source.Execution.AgentProfile, variant.Execution.AgentProfile):
		return fmt.Errorf("prompt/agent variant requires the typed external execution adapter")
	default:
		return fmt.Errorf("tool authority cannot change during replay")
	}
}

func stageIndex(stage reviewcore.StageName) int {
	switch stage {
	case reviewcore.StageMaterializeTarget:
		return 0
	case reviewcore.StagePlanContext:
		return 1
	case reviewcore.StageDetect:
		return 2
	case reviewcore.StageNormalize:
		return 3
	case reviewcore.StageVerify:
		return 4
	case reviewcore.StageAdjudicate:
		return 5
	case reviewcore.StageReport:
		return 6
	case reviewcore.StagePublish:
		return 7
	case reviewcore.StageCaptureFeedback:
		return 8
	case reviewcore.StageExportEvaluation:
		return 9
	default:
		return 1 << 30
	}
}
