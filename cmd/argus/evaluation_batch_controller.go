package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"argus.local/argus/internal/agentcomponentrepo"
	"argus.local/argus/internal/agentshadowworker"
	"argus.local/argus/internal/application"
	"argus.local/argus/internal/contextprovider"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/identity"
	"argus.local/argus/internal/platformapi"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

// localEvaluationBatchController is a bounded local-process launcher around
// Argus-owned durable evaluation batch state. It does not introduce another
// task/runtime ledger: exact intent, leases, fencing, checkpoints and terminal
// remain owned by evaluation.Repository.
type localEvaluationBatchController struct {
	ctx             context.Context
	repository      *evaluation.Repository
	runs            *runrepo.Repository
	storePath       string
	runner          agentshadowworker.Runner
	formal          *formalAgentRunFlags
	formalAdmission *localFormalAdmissionValidator

	mu      sync.Mutex
	running map[string]struct{}
	wait    sync.WaitGroup
}

func newLocalEvaluationBatchController(
	ctx context.Context,
	repository *evaluation.Repository,
	runs *runrepo.Repository,
	storePath string,
	runner agentshadowworker.Runner,
	formalProfile ...formalAgentRunFlags,
) (*localEvaluationBatchController, error) {
	if ctx == nil || repository == nil || runs == nil || storePath == "" || runner == nil {
		return nil, fmt.Errorf("local evaluation batch controller dependencies are required")
	}
	if len(formalProfile) > 1 {
		return nil, fmt.Errorf("at most one local formal Pi profile is allowed")
	}
	var formal *formalAgentRunFlags
	if len(formalProfile) == 1 {
		cloned := cloneFormalAgentRunFlags(formalProfile[0])
		formal = &cloned
	}
	var formalAdmission *localFormalAdmissionValidator
	if formal != nil {
		store, err := local.Open(storePath)
		if err != nil {
			return nil, fmt.Errorf("open evaluation batch component store: %w", err)
		}
		formalAdmission, err = newLocalFormalAdmissionValidator(store)
		if err != nil {
			return nil, fmt.Errorf("initialize evaluation batch component admission: %w", err)
		}
	}
	return &localEvaluationBatchController{
		ctx: ctx, repository: repository, runs: runs, storePath: storePath,
		runner: runner, formal: formal, formalAdmission: formalAdmission,
		running: make(map[string]struct{}),
	}, nil
}

func cloneFormalAgentRunFlags(value formalAgentRunFlags) formalAgentRunFlags {
	cloned := value
	cloned.knowledge = append(stringList{}, value.knowledge...)
	cloned.options.ReviewSkillPaths = append([]string{}, value.options.ReviewSkillPaths...)
	cloned.options.KnowledgePaths = append([]string{}, value.options.KnowledgePaths...)
	return cloned
}

func (controller *localEvaluationBatchController) SubmitExperiment(
	ctx context.Context,
	request evaluation.ExperimentBatchRequest,
	variant platformapi.ExperimentBatchExecutionVariant,
	mutation evaluation.Mutation,
) (evaluation.ExperimentBatchRecord, error) {
	if err := request.Validate(); err != nil {
		return evaluation.ExperimentBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return evaluation.ExperimentBatchRecord{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return evaluation.ExperimentBatchRecord{}, fmt.Errorf("batch created_at must equal mutation time")
	}
	if request.ExecutorRevision != localFormalBatchExecutorRevision {
		return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
			"%w: experiment batch executor_revision must be %q",
			evaluation.ErrInvalidTransition, localFormalBatchExecutorRevision,
		)
	}
	if request.Variable != variant.Variable || request.ExecutorTemplateRef != nil {
		return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
			"%w: platform experiment variant must match request and omit caller template",
			evaluation.ErrInvalidTransition,
		)
	}
	var componentVariant *formalreview.LocalPiComponentVariant
	var findingGovernance *reviewconfig.FindingGovernancePolicy
	var rulePack *reviewconfig.RulePack
	var workflowDefinition *workflow.Definition
	modelOverride := ""
	switch variant.Variable {
	case runmodel.ReplayVariableBudget:
		if variant.BudgetTimeoutMS <= 0 || variant.ComponentRefs != nil {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
				"%w: budget experiment requires only a positive timeout",
				evaluation.ErrInvalidTransition,
			)
		}
	case runmodel.ReplayVariableModel:
		if variant.BudgetTimeoutMS != 0 || variant.ComponentRefs != nil || variant.Model == "" {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
				"%w: model experiment requires only an exact model ID",
				evaluation.ErrInvalidTransition,
			)
		}
		if err := controller.preflightModelVariant(ctx, request, variant.Model); err != nil {
			return evaluation.ExperimentBatchRecord{}, err
		}
		modelOverride = variant.Model
	case runmodel.ReplayVariableIndex:
		if variant.BudgetTimeoutMS != 0 || variant.ComponentRefs != nil ||
			variant.Model != "" || variant.FindingGovernance != nil ||
			len(variant.ContextProviders) == 0 {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
				"%w: index experiment requires only ordered context providers",
				evaluation.ErrInvalidTransition,
			)
		}
		if err := controller.preflightIndexVariant(ctx, request, variant.ContextProviders); err != nil {
			return evaluation.ExperimentBatchRecord{}, err
		}
	case runmodel.ReplayVariableRulePack:
		if variant.BudgetTimeoutMS != 0 || variant.ComponentRefs != nil ||
			variant.Model != "" || variant.FindingGovernance != nil ||
			len(variant.ContextProviders) != 0 || variant.RulePack == nil {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
				"%w: rule_pack experiment requires only one sealed pack",
				evaluation.ErrInvalidTransition,
			)
		}
		if err := controller.preflightRulePackVariant(ctx, request, *variant.RulePack); err != nil {
			return evaluation.ExperimentBatchRecord{}, err
		}
		rulePack = cloneRulePack(variant.RulePack)
	case runmodel.ReplayVariableWorkflow:
		if variant.BudgetTimeoutMS != 0 || variant.ComponentRefs != nil || variant.Model != "" ||
			variant.FindingGovernance != nil || len(variant.ContextProviders) != 0 ||
			variant.RulePack != nil || variant.WorkflowDefinition == nil {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
				"%w: workflow experiment requires only one exact definition",
				evaluation.ErrInvalidTransition,
			)
		}
		if err := controller.preflightWorkflowVariant(
			ctx, request, *variant.WorkflowDefinition,
		); err != nil {
			return evaluation.ExperimentBatchRecord{}, err
		}
		workflowDefinition = cloneWorkflowDefinition(variant.WorkflowDefinition)
	case runmodel.ReplayVariableFilterPolicy, runmodel.ReplayVariableFindingGovernance:
		if variant.BudgetTimeoutMS != 0 || variant.ComponentRefs != nil || variant.Model != "" ||
			variant.FindingGovernance == nil {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
				"%w: finding_governance experiment requires only an exact policy",
				evaluation.ErrInvalidTransition,
			)
		}
		if err := variant.FindingGovernance.Validate(); err != nil {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf("%w: %v", evaluation.ErrInvalidTransition, err)
		}
		findingGovernance = cloneFindingGovernancePolicy(variant.FindingGovernance)
	default:
		if variant.BudgetTimeoutMS != 0 || len(variant.ComponentRefs) == 0 {
			return evaluation.ExperimentBatchRecord{}, fmt.Errorf(
				"%w: component experiment requires exact published refs",
				evaluation.ErrInvalidTransition,
			)
		}
		resolved, subjects, resolveErr := controller.resolveComponentVariant(ctx, request, variant)
		if resolveErr != nil {
			return evaluation.ExperimentBatchRecord{}, resolveErr
		}
		componentVariant = &resolved
		if err := controller.preflightComponentVariant(ctx, subjects, resolved); err != nil {
			return evaluation.ExperimentBatchRecord{}, err
		}
	}
	variantCreatedAt := time.Time{}
	if componentVariant != nil || modelOverride != "" || findingGovernance != nil || rulePack != nil ||
		workflowDefinition != nil ||
		len(variant.ContextProviders) != 0 {
		variantCreatedAt = request.CreatedAt
	}
	formal, err := controller.platformReplayFlags(
		variant.Variable, variant.BudgetTimeoutMS, variantCreatedAt, componentVariant, modelOverride,
		rulePack, workflowDefinition, findingGovernance, variant.ContextProviders,
	)
	if err != nil {
		return evaluation.ExperimentBatchRecord{}, err
	}
	ref, err := persistLocalFormalBatchExecutorTemplate(controller.runs, formal)
	if err != nil {
		return evaluation.ExperimentBatchRecord{}, fmt.Errorf("freeze experiment batch executor: %w", err)
	}
	request.ExecutorTemplateRef = &ref
	executor, err := controller.executor(ref)
	if err != nil {
		return evaluation.ExperimentBatchRecord{}, fmt.Errorf("load frozen experiment batch executor: %w", err)
	}
	record, err := controller.repository.BeginExperimentBatch(ctx, request, mutation)
	if err != nil {
		return evaluation.ExperimentBatchRecord{}, err
	}
	if record.Status == evaluation.ExperimentBatchRunning &&
		(record.ActiveLease == nil || !record.ActiveLease.ExpiresAt.After(time.Now().UTC())) {
		controller.launchExperiment(record, executor)
	}
	return record, nil
}

func (controller *localEvaluationBatchController) SubmitRepeatability(
	ctx context.Context,
	request evaluation.RepeatabilityBatchRequest,
	mutation evaluation.Mutation,
) (evaluation.RepeatabilityBatchRecord, error) {
	if err := request.Validate(); err != nil {
		return evaluation.RepeatabilityBatchRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return evaluation.RepeatabilityBatchRecord{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return evaluation.RepeatabilityBatchRecord{}, fmt.Errorf("batch created_at must equal mutation time")
	}
	if request.ExecutorRevision != localFormalBatchExecutorRevision {
		return evaluation.RepeatabilityBatchRecord{}, fmt.Errorf(
			"%w: repeatability batch executor_revision must be %q",
			evaluation.ErrInvalidTransition, localFormalBatchExecutorRevision,
		)
	}
	if request.ExecutorTemplateRef != nil {
		return evaluation.RepeatabilityBatchRecord{}, fmt.Errorf(
			"%w: platform repeatability submission does not accept a caller template",
			evaluation.ErrInvalidTransition,
		)
	}
	formal, err := controller.platformReplayFlags(
		runmodel.ReplayVariableNone, 0, time.Time{}, nil, "", nil, nil, nil, nil,
	)
	if err != nil {
		return evaluation.RepeatabilityBatchRecord{}, err
	}
	ref, err := persistLocalFormalBatchExecutorTemplate(controller.runs, formal)
	if err != nil {
		return evaluation.RepeatabilityBatchRecord{}, fmt.Errorf("freeze repeatability batch executor: %w", err)
	}
	request.ExecutorTemplateRef = &ref
	executor, err := controller.executor(ref)
	if err != nil {
		return evaluation.RepeatabilityBatchRecord{}, fmt.Errorf("load frozen repeatability batch executor: %w", err)
	}
	record, err := controller.repository.BeginRepeatabilityBatch(ctx, request, mutation)
	if err != nil {
		return evaluation.RepeatabilityBatchRecord{}, err
	}
	if record.Status == evaluation.RepeatabilityBatchRunning &&
		(record.ActiveLease == nil || !record.ActiveLease.ExpiresAt.After(time.Now().UTC())) {
		controller.launchRepeatability(record, executor)
	}
	return record, nil
}

func (controller *localEvaluationBatchController) resolveComponentVariant(
	ctx context.Context,
	request evaluation.ExperimentBatchRequest,
	variant platformapi.ExperimentBatchExecutionVariant,
) (formalreview.LocalPiComponentVariant, []application.AgentPlanningSubject, error) {
	if controller.formal == nil || controller.formalAdmission == nil ||
		controller.formalAdmission.components == nil {
		return formalreview.LocalPiComponentVariant{}, nil, fmt.Errorf(
			"%w: local API formal Pi profile is not configured",
			evaluation.ErrInvalidTransition,
		)
	}
	contract := ""
	switch variant.Variable {
	case runmodel.ReplayVariablePrompt:
		contract = contractsv1alpha1.AgentStagePlanPromptContract
	case runmodel.ReplayVariableSkillPack:
		contract = contractsv1alpha1.AgentStagePlanSkillContract
	case runmodel.ReplayVariableKnowledgePack:
		contract = contractsv1alpha1.AgentStagePlanKnowledgeContract
	default:
		return formalreview.LocalPiComponentVariant{}, nil, fmt.Errorf(
			"%w: unsupported platform component variable %q",
			evaluation.ErrInvalidTransition, variant.Variable,
		)
	}
	subjects, err := controller.experimentBatchSubjects(request)
	if err != nil {
		return formalreview.LocalPiComponentVariant{}, nil, err
	}
	components := make([]formalreview.LocalPiGovernedComponent, len(variant.ComponentRefs))
	registrySubject := agentComponentSubject(subjects[0])
	for index, ref := range variant.ComponentRefs {
		resolved, resolveErr := controller.formalAdmission.components.ResolveWithContent(
			ctx, registrySubject, contract, ref,
		)
		if resolveErr != nil {
			return formalreview.LocalPiComponentVariant{}, nil, fmt.Errorf(
				"%w: governed component %s@%s is unavailable for the baseline subject",
				evaluation.ErrInvalidTransition, ref.ID, ref.Revision,
			)
		}
		components[index] = formalreview.LocalPiGovernedComponent{
			Contract: contract, Binding: resolved.Binding,
			Content: append([]byte{}, resolved.Content...),
		}
	}
	resolved := formalreview.LocalPiComponentVariant{
		SchemaVersion: formalreview.LocalPiComponentVariantSchemaVersion,
		Variable:      variant.Variable, Components: components,
	}
	if err := resolved.Validate(); err != nil {
		return formalreview.LocalPiComponentVariant{}, nil, fmt.Errorf(
			"%w: validate governed component variant: %v",
			evaluation.ErrInvalidTransition, err,
		)
	}
	return resolved, subjects, nil
}

func (controller *localEvaluationBatchController) experimentBatchSubjects(
	request evaluation.ExperimentBatchRequest,
) ([]application.AgentPlanningSubject, error) {
	subjects := make([]application.AgentPlanningSubject, 0, len(request.Cases))
	seen := make(map[application.AgentPlanningSubject]struct{}, len(request.Cases))
	for _, item := range request.Cases {
		committed, err := controller.runs.LoadCommittedRunResult(item.BaselineReviewRunID)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: load baseline ReviewRun execution subject",
				evaluation.ErrInvalidTransition,
			)
		}
		snapshot, err := controller.runs.LoadExecutionSnapshot(committed.Run.ExecutionSnapshotID)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: load baseline ReviewRun execution subject",
				evaluation.ErrInvalidTransition,
			)
		}
		var spec contractsv1alpha1.ReviewSpec
		if err := controller.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
			return nil, fmt.Errorf(
				"%w: load baseline ReviewSpec subject",
				evaluation.ErrInvalidTransition,
			)
		}
		subject := application.AgentPlanningSubject{
			TenantID: spec.TenantID, OrganizationID: "local",
			WorkspaceID: spec.WorkspaceID, RepositoryID: spec.Repository.RepositoryID,
		}
		if _, exists := seen[subject]; exists {
			continue
		}
		seen[subject] = struct{}{}
		subjects = append(subjects, subject)
	}
	if len(subjects) == 0 {
		return nil, fmt.Errorf("%w: experiment batch has no baseline subject", evaluation.ErrInvalidTransition)
	}
	return subjects, nil
}

func (controller *localEvaluationBatchController) preflightModelVariant(
	ctx context.Context,
	request evaluation.ExperimentBatchRequest,
	model string,
) error {
	if controller.formal == nil || controller.formalAdmission == nil ||
		controller.formalAdmission.components == nil {
		return fmt.Errorf(
			"%w: local API formal Pi profile is not configured",
			evaluation.ErrInvalidTransition,
		)
	}
	profile := cloneFormalAgentRunFlags(*controller.formal)
	profile.options.Model = model
	bootstrap, err := formalreview.BuildLocalPiBootstrap(profile.options)
	if err != nil {
		return fmt.Errorf("%w: build exact model variant: %v", evaluation.ErrInvalidTransition, err)
	}
	selected := reviewconfig.VersionedRef{
		ID: bootstrap.Manifest.Model.ID, Revision: bootstrap.Manifest.Model.Revision,
		SHA256: bootstrap.Manifest.Model.SHA256,
	}
	for _, item := range request.Cases {
		committed, loadErr := controller.runs.LoadCommittedRunResult(item.BaselineReviewRunID)
		if loadErr != nil {
			return fmt.Errorf("%w: load committed model baseline", evaluation.ErrInvalidTransition)
		}
		snapshot, loadErr := controller.runs.LoadExecutionSnapshot(committed.Run.ExecutionSnapshotID)
		if loadErr != nil {
			return fmt.Errorf("%w: load model baseline snapshot", evaluation.ErrInvalidTransition)
		}
		var bundle reviewconfig.ConfigBundle
		if loadErr = controller.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &bundle); loadErr != nil ||
			bundle.Validate() != nil || bundle.AgentReview == nil {
			return fmt.Errorf("%w: load governed model baseline config", evaluation.ErrInvalidTransition)
		}
		if bundle.AgentReview.Model == selected {
			return fmt.Errorf("%w: model experiment must change every baseline model", evaluation.ErrInvalidTransition)
		}
	}
	subjects, err := controller.experimentBatchSubjects(request)
	if err != nil {
		return err
	}
	for _, subject := range subjects {
		registrySubject := agentComponentSubject(subject)
		for _, component := range bootstrap.Components {
			if component.Contract == contractsv1alpha1.AgentStagePlanModelContract {
				continue
			}
			if _, err := controller.formalAdmission.components.Resolve(
				ctx, registrySubject, component.Contract, component.Ref,
			); err != nil {
				return fmt.Errorf(
					"%w: exact model variant base closure is not admitted for every baseline subject",
					evaluation.ErrInvalidTransition,
				)
			}
		}
	}
	return nil
}

func (controller *localEvaluationBatchController) preflightComponentVariant(
	ctx context.Context,
	subjects []application.AgentPlanningSubject,
	variant formalreview.LocalPiComponentVariant,
) error {
	bootstrap, err := formalreview.BuildLocalPiBootstrapVariant(controller.formal.options, variant)
	if err != nil {
		return fmt.Errorf("%w: build governed component variant: %v", evaluation.ErrInvalidTransition, err)
	}
	for _, subject := range subjects {
		if err := controller.formalAdmission.ValidateFormalAdmission(ctx, subject, bootstrap); err != nil {
			return fmt.Errorf(
				"%w: governed component variant is not admitted for every baseline subject",
				evaluation.ErrInvalidTransition,
			)
		}
	}
	return nil
}

func (controller *localEvaluationBatchController) preflightIndexVariant(
	ctx context.Context,
	request evaluation.ExperimentBatchRequest,
	providers []reviewconfig.ContextProviderDefinition,
) error {
	if controller.formal == nil {
		return fmt.Errorf(
			"%w: local API formal Pi profile is not configured",
			evaluation.ErrInvalidTransition,
		)
	}
	for index, provider := range providers {
		if _, err := contextprovider.LocalAdapterArtifact(provider); err != nil {
			return fmt.Errorf(
				"%w: context provider %d is not an executable publishable local adapter",
				evaluation.ErrInvalidTransition,
				index,
			)
		}
	}
	for _, item := range request.Cases {
		if err := ctx.Err(); err != nil {
			return err
		}
		committed, err := controller.runs.LoadCommittedRunResult(item.BaselineReviewRunID)
		if err != nil {
			return fmt.Errorf("%w: load committed index baseline", evaluation.ErrInvalidTransition)
		}
		snapshot, err := controller.runs.LoadExecutionSnapshot(committed.Run.ExecutionSnapshotID)
		if err != nil {
			return fmt.Errorf("%w: load index baseline snapshot", evaluation.ErrInvalidTransition)
		}
		var bundle reviewconfig.ConfigBundle
		if err := controller.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &bundle); err != nil ||
			bundle.Validate() != nil {
			return fmt.Errorf("%w: load governed index baseline config", evaluation.ErrInvalidTransition)
		}
		variant := bundle.Execution
		variant.ContextProviders = slices.Clone(providers)
		if _, err := reviewconfig.DiffReplayIndexPolicy(bundle.Execution, variant); err != nil {
			return fmt.Errorf(
				"%w: index experiment must atomically change every baseline context policy: %v",
				evaluation.ErrInvalidTransition,
				err,
			)
		}
	}
	return nil
}

func (controller *localEvaluationBatchController) preflightRulePackVariant(
	ctx context.Context,
	request evaluation.ExperimentBatchRequest,
	pack reviewconfig.RulePack,
) error {
	if controller.formal == nil {
		return fmt.Errorf(
			"%w: local API formal Pi profile is not configured",
			evaluation.ErrInvalidTransition,
		)
	}
	if err := pack.Validate(); err != nil {
		return fmt.Errorf("%w: invalid sealed rule_pack: %v", evaluation.ErrInvalidTransition, err)
	}
	for _, item := range request.Cases {
		if err := ctx.Err(); err != nil {
			return err
		}
		committed, err := controller.runs.LoadCommittedRunResult(item.BaselineReviewRunID)
		if err != nil {
			return fmt.Errorf("%w: load committed rule_pack baseline", evaluation.ErrInvalidTransition)
		}
		snapshot, err := controller.runs.LoadExecutionSnapshot(committed.Run.ExecutionSnapshotID)
		if err != nil {
			return fmt.Errorf("%w: load rule_pack baseline snapshot", evaluation.ErrInvalidTransition)
		}
		var bundle reviewconfig.ConfigBundle
		if err := controller.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &bundle); err != nil ||
			bundle.Validate() != nil {
			return fmt.Errorf("%w: load governed rule_pack baseline config", evaluation.ErrInvalidTransition)
		}
		if reflect.DeepEqual(bundle.RulePack, pack) {
			return fmt.Errorf(
				"%w: rule_pack experiment must change every baseline rule pack",
				evaluation.ErrInvalidTransition,
			)
		}
	}
	return nil
}

func (controller *localEvaluationBatchController) preflightWorkflowVariant(
	ctx context.Context,
	request evaluation.ExperimentBatchRequest,
	definition workflow.Definition,
) error {
	if controller.formal == nil {
		return fmt.Errorf(
			"%w: local API formal Pi profile is not configured",
			evaluation.ErrInvalidTransition,
		)
	}
	if err := definition.Validate(); err != nil {
		return fmt.Errorf("%w: invalid WorkflowDefinition: %v", evaluation.ErrInvalidTransition, err)
	}
	for _, item := range request.Cases {
		if err := ctx.Err(); err != nil {
			return err
		}
		committed, err := controller.runs.LoadCommittedRunResult(item.BaselineReviewRunID)
		if err != nil {
			return fmt.Errorf("%w: load committed workflow baseline", evaluation.ErrInvalidTransition)
		}
		snapshot, err := controller.runs.LoadExecutionSnapshot(committed.Run.ExecutionSnapshotID)
		if err != nil {
			return fmt.Errorf("%w: load workflow baseline snapshot", evaluation.ErrInvalidTransition)
		}
		var bundle reviewconfig.ConfigBundle
		if err := controller.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &bundle); err != nil {
			return fmt.Errorf("%w: load workflow baseline config", evaluation.ErrInvalidTransition)
		}
		var baseline workflow.Definition
		if err := controller.runs.ReadJSONArtifact(snapshot.WorkflowDefinitionRef, &baseline); err != nil {
			return fmt.Errorf("%w: load workflow baseline definition", evaluation.ErrInvalidTransition)
		}
		fields, earliest, diffErr := workflow.DiffReplayExecutionPolicy(baseline, definition)
		if diffErr != nil || earliest != "agent_hypothesize" || len(fields) == 0 ||
			bundle.AgentReview == nil || !workflow.ReplayChangesEffectivePolicy(
			baseline,
			definition,
			workflow.RuntimeLimits{
				TimeoutMS:      bundle.AgentReview.Budget.TimeoutMS,
				MaxInputBytes:  bundle.AgentReview.Budget.MaxTargetBytes,
				MaxOutputBytes: bundle.AgentReview.Budget.MaxOutputBytes,
				MaxAttempts:    bundle.Budget.MaxAttempts,
				MaxConcurrency: bundle.AgentReview.Budget.MaxConcurrency,
			},
		) {
			return fmt.Errorf(
				"%w: workflow experiment must change every baseline effective stage budget",
				evaluation.ErrInvalidTransition,
			)
		}
		for _, field := range fields {
			if !strings.HasPrefix(field, "workflow.stages.agent_hypothesize.budget.") &&
				!strings.HasPrefix(field, "workflow.stages.agent_hypothesize.retry.") {
				return fmt.Errorf("%w: workflow experiment changed a non-budget/retry field", evaluation.ErrInvalidTransition)
			}
		}
	}
	return nil
}

func agentComponentSubject(subject application.AgentPlanningSubject) agentcomponentrepo.Subject {
	return agentcomponentrepo.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
}

func (controller *localEvaluationBatchController) platformReplayFlags(
	variable runmodel.ReplayVariable,
	timeoutMS int64,
	createdAt time.Time,
	componentVariant *formalreview.LocalPiComponentVariant,
	modelOverride string,
	rulePack *reviewconfig.RulePack,
	workflowDefinition *workflow.Definition,
	findingGovernance *reviewconfig.FindingGovernancePolicy,
	contextProviders []reviewconfig.ContextProviderDefinition,
) (formalAgentReplayFlags, error) {
	if controller.formal == nil {
		return formalAgentReplayFlags{}, fmt.Errorf(
			"%w: local API formal Pi profile is not configured",
			evaluation.ErrInvalidTransition,
		)
	}
	profile := cloneFormalAgentRunFlags(*controller.formal)
	if modelOverride != "" {
		profile.options.Model = modelOverride
	}
	return formalAgentReplayFlags{
		store: controller.storePath,
		node:  profile.options.NodePath, workerScript: profile.options.WorkerScript,
		providerProfile: profile.options.ProviderProfile, model: profile.options.Model,
		change: variable, timeoutMS: timeoutMS, at: createdAt, options: profile.options,
		pricing: profile.pricing, transport: profile.transport, json: true,
		componentVariant:        cloneLocalPiComponentVariant(componentVariant),
		rulePackPolicy:          cloneRulePack(rulePack),
		workflowPolicy:          cloneWorkflowDefinition(workflowDefinition),
		findingGovernancePolicy: cloneFindingGovernancePolicy(findingGovernance),
		indexContextProviders:   slices.Clone(contextProviders),
	}, nil
}

func (controller *localEvaluationBatchController) ResumeExperiment(
	ctx context.Context,
	batchID string,
	mutation evaluation.Mutation,
) (evaluation.ExperimentBatchRecord, error) {
	record, err := controller.repository.GetExperimentBatch(batchID, evaluation.Access{
		Actor: mutation.Actor, Roles: append([]evaluation.Role{}, mutation.Roles...),
	})
	if err != nil {
		return evaluation.ExperimentBatchRecord{}, err
	}
	if record.Status != evaluation.ExperimentBatchRunning {
		return evaluation.ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch is terminal", evaluation.ErrInvalidTransition)
	}
	if record.Request.ExecutorTemplateRef == nil {
		return evaluation.ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch has no executor template", evaluation.ErrInvalidTransition)
	}
	executor, err := controller.executor(*record.Request.ExecutorTemplateRef)
	if err != nil {
		return evaluation.ExperimentBatchRecord{}, err
	}
	record, err = controller.repository.RequestExperimentBatchResume(ctx, batchID, mutation)
	if err != nil {
		return evaluation.ExperimentBatchRecord{}, err
	}
	if record.ActiveLease != nil && record.ActiveLease.ExpiresAt.After(mutation.At.UTC()) {
		return record, nil
	}
	controller.launchExperiment(record, executor)
	return record, nil
}

func (controller *localEvaluationBatchController) ResumeRepeatability(
	ctx context.Context,
	batchID string,
	mutation evaluation.Mutation,
) (evaluation.RepeatabilityBatchRecord, error) {
	record, err := controller.repository.GetRepeatabilityBatch(batchID, evaluation.Access{
		Actor: mutation.Actor, Roles: append([]evaluation.Role{}, mutation.Roles...),
	})
	if err != nil {
		return evaluation.RepeatabilityBatchRecord{}, err
	}
	if record.Status != evaluation.RepeatabilityBatchRunning {
		return evaluation.RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch is terminal", evaluation.ErrInvalidTransition)
	}
	if record.Request.ExecutorTemplateRef == nil {
		return evaluation.RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch has no executor template", evaluation.ErrInvalidTransition)
	}
	executor, err := controller.executor(*record.Request.ExecutorTemplateRef)
	if err != nil {
		return evaluation.RepeatabilityBatchRecord{}, err
	}
	record, err = controller.repository.RequestRepeatabilityBatchResume(ctx, batchID, mutation)
	if err != nil {
		return evaluation.RepeatabilityBatchRecord{}, err
	}
	if record.ActiveLease != nil && record.ActiveLease.ExpiresAt.After(mutation.At.UTC()) {
		return record, nil
	}
	controller.launchRepeatability(record, executor)
	return record, nil
}

func (controller *localEvaluationBatchController) executor(
	ref runmodel.ArtifactRef,
) (*localFormalBatchExecutor, error) {
	return loadLocalFormalBatchExecutor(
		controller.runs, controller.storePath, ref, controller.runner,
	)
}

func (controller *localEvaluationBatchController) launchExperiment(
	record evaluation.ExperimentBatchRecord,
	executor *localFormalBatchExecutor,
) {
	key := "experiment:" + record.Request.BatchID
	if !controller.begin(key) {
		return
	}
	controller.wait.Add(1)
	go func() {
		defer controller.wait.Done()
		defer controller.end(key)
		workerID, err := identity.NewGenerator().New("api-eval-batch-worker")
		if err != nil {
			return
		}
		runner, err := evaluation.NewExperimentBatchRunner(
			controller.repository, controller.runs, executor,
			func() time.Time { return time.Now().UTC() }, workerID, 5*time.Minute,
		)
		if err != nil {
			return
		}
		if _, runErr := runner.Run(controller.ctx, record.Request, record.Intent); runErr != nil {
			controller.recordExperimentFailure(record, workerID, runErr)
		}
	}()
}

func (controller *localEvaluationBatchController) launchRepeatability(
	record evaluation.RepeatabilityBatchRecord,
	executor *localFormalBatchExecutor,
) {
	key := "repeatability:" + record.Request.BatchID
	if !controller.begin(key) {
		return
	}
	controller.wait.Add(1)
	go func() {
		defer controller.wait.Done()
		defer controller.end(key)
		workerID, err := identity.NewGenerator().New("api-repeatability-batch-worker")
		if err != nil {
			return
		}
		runner, err := evaluation.NewRepeatabilityBatchRunner(
			controller.repository, controller.runs, executor,
			func() time.Time { return time.Now().UTC() }, workerID, 5*time.Minute,
		)
		if err != nil {
			return
		}
		if _, runErr := runner.Run(controller.ctx, record.Request, record.Intent); runErr != nil {
			controller.recordRepeatabilityFailure(record, workerID, runErr)
		}
	}()
}

func (controller *localEvaluationBatchController) recordExperimentFailure(
	intent evaluation.ExperimentBatchRecord,
	workerID string,
	runErr error,
) {
	record, err := controller.repository.GetExperimentBatch(intent.Request.BatchID, evaluation.Access{
		Actor: intent.Intent.Actor, Roles: append([]evaluation.Role{}, intent.Intent.Roles...),
	})
	if err != nil || record.Status != evaluation.ExperimentBatchRunning ||
		record.ActiveLease == nil || record.ActiveLease.WorkerID != workerID {
		return
	}
	at := time.Now().UTC()
	failure := batchAttemptFailure(*record.ActiveLease, at, runErr)
	flushContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = controller.repository.RecordExperimentBatchAttemptFailure(
		flushContext, record.Request.BatchID, *record.ActiveLease, failure,
		evaluation.Mutation{
			IdempotencyKey: record.Request.BatchID + "/attempt-failed/" + record.ActiveLease.LeaseID,
			Actor:          workerID, Roles: append([]evaluation.Role{}, record.Intent.Roles...),
			Audit: "record local experiment batch attempt failure", At: at,
		},
	)
}

func (controller *localEvaluationBatchController) recordRepeatabilityFailure(
	intent evaluation.RepeatabilityBatchRecord,
	workerID string,
	runErr error,
) {
	record, err := controller.repository.GetRepeatabilityBatch(intent.Request.BatchID, evaluation.Access{
		Actor: intent.Intent.Actor, Roles: append([]evaluation.Role{}, intent.Intent.Roles...),
	})
	if err != nil || record.Status != evaluation.RepeatabilityBatchRunning ||
		record.ActiveLease == nil || record.ActiveLease.WorkerID != workerID {
		return
	}
	at := time.Now().UTC()
	failure := batchAttemptFailure(*record.ActiveLease, at, runErr)
	flushContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = controller.repository.RecordRepeatabilityBatchAttemptFailure(
		flushContext, record.Request.BatchID, *record.ActiveLease, failure,
		evaluation.Mutation{
			IdempotencyKey: record.Request.BatchID + "/attempt-failed/" + record.ActiveLease.LeaseID,
			Actor:          workerID, Roles: append([]evaluation.Role{}, record.Intent.Roles...),
			Audit: "record local repeatability batch attempt failure", At: at,
		},
	)
}

func batchAttemptFailure(lease any, at time.Time, runErr error) evaluation.BatchAttemptFailure {
	var workerID, leaseID string
	var generation, token uint64
	switch value := lease.(type) {
	case evaluation.ExperimentBatchLease:
		workerID, leaseID = value.WorkerID, value.LeaseID
		generation, token = value.Generation, value.FencingToken
	case evaluation.RepeatabilityBatchLease:
		workerID, leaseID = value.WorkerID, value.LeaseID
		generation, token = value.Generation, value.FencingToken
	}
	code := evaluation.BatchAttemptExecutionFailed
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		code = evaluation.BatchAttemptServiceStopping
	}
	return evaluation.BatchAttemptFailure{
		Code: code, WorkerID: workerID, LeaseID: leaseID,
		Generation: generation, FencingToken: token, ObservedAt: at,
	}
}

func (controller *localEvaluationBatchController) begin(key string) bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if _, exists := controller.running[key]; exists {
		return false
	}
	controller.running[key] = struct{}{}
	return true
}

func (controller *localEvaluationBatchController) end(key string) {
	controller.mu.Lock()
	delete(controller.running, key)
	controller.mu.Unlock()
}

func (controller *localEvaluationBatchController) Wait() {
	controller.wait.Wait()
}
