package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/identity"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const localFormalBatchExecutorRevision = "argus-local-formal-pi-replay-1"

const localFormalBatchExecutorTemplateSchemaVersion = "argus.local_formal_batch_executor_template.v1alpha1"

const evaluationBatchUsage = `usage:
  argus evaluation batch run --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json] -- <formal replay variant flags>
  argus evaluation batch resume --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation batch show --store <absolute-dir> --batch <id> --access <absolute-json> [--json]

The formal replay variant flags are the same as "agent-review formal replay" except
--store, --source-formal-run, --idempotency-key, and --json, which the batch owns.`

type experimentBatchOutput struct {
	Result    evaluation.ExperimentBatchResult `json:"result"`
	StorePath string                           `json:"store_path"`
}

type experimentBatchRecordOutput struct {
	Record    evaluation.ExperimentBatchRecord `json:"record"`
	StorePath string                           `json:"store_path"`
}

type localFormalBatchExecutor struct {
	template    formalAgentReplayFlags
	templateRef runmodel.ArtifactRef
	runner      agentshadowworker.Runner
}

// localFormalBatchExecutorTemplate is the credential-free execution closure
// frozen before batch intent admission. Paths remain local adapter inputs,
// while Bootstrap contains the exact inspected runtime/component bytes used to
// reject drift after restart. Transport stores only endpoint and pinned trust
// identity; provider and Hailix credentials are never fields.
type localFormalBatchExecutorTemplate struct {
	SchemaVersion      string                                   `json:"schema_version"`
	ExecutorRevision   string                                   `json:"executor_revision"`
	Variable           runmodel.ReplayVariable                  `json:"variable"`
	Options            formalreview.LocalPiBootstrapOptions     `json:"options"`
	Bootstrap          formalreview.LocalPiBootstrap            `json:"bootstrap"`
	Pricing            piexecution.PricingCeiling               `json:"pricing"`
	Transport          formalExecutionTransportFlags            `json:"transport"`
	ComponentVariant   *formalreview.LocalPiComponentVariant    `json:"component_variant,omitempty"`
	RulePack           *reviewconfig.RulePack                   `json:"rule_pack,omitempty"`
	WorkflowDefinition *workflow.Definition                     `json:"workflow_definition,omitempty"`
	FindingGovernance  *reviewconfig.FindingGovernancePolicy    `json:"finding_governance,omitempty"`
	ContextProviders   []reviewconfig.ContextProviderDefinition `json:"context_providers,omitempty"`
	TimeoutMS          int64                                    `json:"timeout_ms,omitempty"`
	CreatedAt          time.Time                                `json:"created_at,omitempty"`
}

func (template localFormalBatchExecutorTemplate) Validate() error {
	if template.SchemaVersion != localFormalBatchExecutorTemplateSchemaVersion {
		return fmt.Errorf("unsupported local formal batch executor template schema %q", template.SchemaVersion)
	}
	if template.ExecutorRevision != localFormalBatchExecutorRevision {
		return fmt.Errorf("unsupported local formal batch executor revision %q", template.ExecutorRevision)
	}
	if err := template.Transport.validate(); err != nil {
		return fmt.Errorf("execution transport: %w", err)
	}
	if err := template.Variable.Validate(); err != nil {
		return err
	}
	switch template.Variable {
	case runmodel.ReplayVariableNone:
		if template.TimeoutMS != 0 || template.RulePack != nil || template.FindingGovernance != nil ||
			template.WorkflowDefinition != nil || template.ContextProviders != nil || !template.CreatedAt.IsZero() {
			return fmt.Errorf("exact replay template cannot contain variant time or timeout")
		}
	case runmodel.ReplayVariableBudget:
		if template.TimeoutMS <= 0 || template.RulePack != nil || template.FindingGovernance != nil ||
			template.WorkflowDefinition != nil || template.ContextProviders != nil || !template.CreatedAt.IsZero() {
			return fmt.Errorf("budget replay template requires only a positive timeout")
		}
	case runmodel.ReplayVariableModel, runmodel.ReplayVariablePrompt,
		runmodel.ReplayVariableSkillPack, runmodel.ReplayVariableKnowledgePack:
		if template.TimeoutMS != 0 || template.RulePack != nil || template.FindingGovernance != nil ||
			template.WorkflowDefinition != nil || template.ContextProviders != nil ||
			template.CreatedAt.IsZero() || template.CreatedAt.Location() != time.UTC {
			return fmt.Errorf("component replay template requires only a non-zero UTC creation time")
		}
	case runmodel.ReplayVariableIndex:
		if template.TimeoutMS != 0 || template.ComponentVariant != nil ||
			template.RulePack != nil || template.FindingGovernance != nil || len(template.ContextProviders) == 0 ||
			template.WorkflowDefinition != nil ||
			template.CreatedAt.IsZero() || template.CreatedAt.Location() != time.UTC {
			return fmt.Errorf("index replay template requires ordered context providers and a non-zero UTC creation time")
		}
		seen := make(map[string]struct{}, len(template.ContextProviders))
		for index, provider := range template.ContextProviders {
			if err := provider.Validate(); err != nil {
				return fmt.Errorf("context_providers[%d]: %w", index, err)
			}
			if _, duplicate := seen[provider.ID]; duplicate {
				return fmt.Errorf("context_providers contains duplicate id %q", provider.ID)
			}
			seen[provider.ID] = struct{}{}
		}
	case runmodel.ReplayVariableRulePack:
		if template.TimeoutMS != 0 || template.ComponentVariant != nil ||
			template.RulePack == nil || template.FindingGovernance != nil ||
			template.WorkflowDefinition != nil || template.ContextProviders != nil || template.CreatedAt.IsZero() ||
			template.CreatedAt.Location() != time.UTC {
			return fmt.Errorf("rule_pack replay template requires one sealed pack and a non-zero UTC creation time")
		}
		if err := template.RulePack.Validate(); err != nil {
			return err
		}
	case runmodel.ReplayVariableWorkflow:
		if template.TimeoutMS != 0 || template.ComponentVariant != nil || template.RulePack != nil ||
			template.FindingGovernance != nil || template.ContextProviders != nil ||
			template.WorkflowDefinition == nil || template.CreatedAt.IsZero() ||
			template.CreatedAt.Location() != time.UTC {
			return fmt.Errorf("workflow replay template requires one exact definition and a non-zero UTC creation time")
		}
		if err := template.WorkflowDefinition.Validate(); err != nil {
			return err
		}
	case runmodel.ReplayVariableFilterPolicy, runmodel.ReplayVariableFindingGovernance:
		if template.TimeoutMS != 0 || template.ComponentVariant != nil ||
			template.RulePack != nil || template.WorkflowDefinition != nil ||
			template.ContextProviders != nil || template.FindingGovernance == nil ||
			template.CreatedAt.IsZero() ||
			template.CreatedAt.Location() != time.UTC {
			return fmt.Errorf("finding_governance replay template requires a policy and non-zero UTC creation time")
		}
		if err := template.FindingGovernance.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported local formal batch replay variable %q", template.Variable)
	}
	if template.ComponentVariant != nil && template.ComponentVariant.Variable != template.Variable {
		return fmt.Errorf("component variant does not match replay variable")
	}
	if template.ComponentVariant == nil &&
		(template.Variable == runmodel.ReplayVariablePrompt ||
			template.Variable == runmodel.ReplayVariableSkillPack ||
			template.Variable == runmodel.ReplayVariableKnowledgePack) {
		// CLI path-backed variants remain valid and rebuild from Options. A
		// platform ref-backed variant always carries ComponentVariant.
	} else if template.ComponentVariant != nil {
		if err := template.ComponentVariant.Validate(); err != nil {
			return err
		}
	}
	live, err := buildLocalFormalPiBootstrap(template.Options, template.ComponentVariant)
	if err != nil {
		return fmt.Errorf("rebuild local formal batch bootstrap: %w", err)
	}
	want, err := json.Marshal(template.Bootstrap)
	if err != nil {
		return err
	}
	got, err := json.Marshal(live)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("local formal batch runtime or governed component bytes drifted")
	}
	if _, err := piexecution.NewCapabilityResolver(template.Bootstrap.Manifest, template.Pricing); err != nil {
		return fmt.Errorf("validate local formal batch pricing: %w", err)
	}
	return nil
}

func persistLocalFormalBatchExecutorTemplate(
	runs *runrepo.Repository,
	formal formalAgentReplayFlags,
) (runmodel.ArtifactRef, error) {
	if runs == nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("run repository is required")
	}
	bootstrap, err := buildLocalFormalPiBootstrap(formal.options, formal.componentVariant)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	findingGovernance := cloneFindingGovernancePolicy(formal.findingGovernancePolicy)
	rulePack := cloneRulePack(formal.rulePackPolicy)
	workflowDefinition := cloneWorkflowDefinition(formal.workflowPolicy)
	if formal.change == runmodel.ReplayVariableWorkflow && workflowDefinition == nil {
		data, readErr := os.ReadFile(formal.workflowDefinition)
		if readErr != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("read workflow batch definition: %w", readErr)
		}
		definition, decodeErr := workflow.DecodeDefinition(data)
		if decodeErr != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("decode workflow batch definition: %w", decodeErr)
		}
		workflowDefinition = &definition
	}
	if formal.change == runmodel.ReplayVariableRulePack && rulePack == nil {
		data, readErr := os.ReadFile(formal.rulePack)
		if readErr != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("read rule_pack batch policy: %w", readErr)
		}
		pack, decodeErr := reviewconfig.DecodeRulePack(data)
		if decodeErr != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("decode rule_pack batch policy: %w", decodeErr)
		}
		rulePack = &pack
	}
	if formal.change.IsFilterPolicy() && findingGovernance == nil {
		data, readErr := os.ReadFile(formal.findingGovernance)
		if readErr != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("read finding governance batch policy: %w", readErr)
		}
		policy, decodeErr := reviewconfig.DecodeFindingGovernancePolicy(data)
		if decodeErr != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("decode finding governance batch policy: %w", decodeErr)
		}
		findingGovernance = &policy
	}
	template := localFormalBatchExecutorTemplate{
		SchemaVersion:    localFormalBatchExecutorTemplateSchemaVersion,
		ExecutorRevision: localFormalBatchExecutorRevision, Variable: formal.change,
		Options: formal.options, Bootstrap: bootstrap, Pricing: formal.pricing,
		Transport:          formal.transport,
		ComponentVariant:   cloneLocalPiComponentVariant(formal.componentVariant),
		RulePack:           rulePack,
		WorkflowDefinition: workflowDefinition,
		FindingGovernance:  findingGovernance,
		ContextProviders:   slices.Clone(formal.indexContextProviders),
		TimeoutMS:          formal.timeoutMS, CreatedAt: formal.at,
	}
	if err := template.Validate(); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return runs.PutJSONArtifact(evaluation.ReplayExecutorTemplateContract, template)
}

func loadLocalFormalBatchExecutor(
	runs *runrepo.Repository,
	storePath string,
	ref runmodel.ArtifactRef,
	runner agentshadowworker.Runner,
) (*localFormalBatchExecutor, error) {
	if runs == nil || runner == nil {
		return nil, fmt.Errorf("run repository and formal Pi runner are required")
	}
	if ref.Contract != evaluation.ReplayExecutorTemplateContract {
		return nil, fmt.Errorf("unsupported replay executor template contract %q", ref.Contract)
	}
	data, err := runs.ReadArtifact(ref)
	if err != nil {
		return nil, err
	}
	template, err := decodeStrictCommandJSON(
		data, "LocalFormalBatchExecutorTemplate",
		func(value localFormalBatchExecutorTemplate) error { return value.Validate() },
	)
	if err != nil {
		return nil, err
	}
	formal := formalAgentReplayFlags{
		store: storePath, node: template.Options.NodePath,
		workerScript:    template.Options.WorkerScript,
		providerProfile: template.Options.ProviderProfile, model: template.Options.Model,
		change: template.Variable, timeoutMS: template.TimeoutMS, at: template.CreatedAt,
		json: true, options: template.Options, pricing: template.Pricing,
		transport:               template.Transport,
		componentVariant:        cloneLocalPiComponentVariant(template.ComponentVariant),
		rulePackPolicy:          cloneRulePack(template.RulePack),
		workflowPolicy:          cloneWorkflowDefinition(template.WorkflowDefinition),
		findingGovernancePolicy: cloneFindingGovernancePolicy(template.FindingGovernance),
		indexContextProviders:   slices.Clone(template.ContextProviders),
	}
	return &localFormalBatchExecutor{template: formal, templateRef: ref, runner: runner}, nil
}

func cloneRulePack(value *reviewconfig.RulePack) *reviewconfig.RulePack {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Rules = slices.Clone(value.Rules)
	for index := range cloned.Rules {
		cloned.Rules[index].Languages = slices.Clone(value.Rules[index].Languages)
		cloned.Rules[index].PathPrefixes = slices.Clone(value.Rules[index].PathPrefixes)
		cloned.Rules[index].EvidenceKinds = slices.Clone(value.Rules[index].EvidenceKinds)
	}
	return &cloned
}

func cloneWorkflowDefinition(value *workflow.Definition) *workflow.Definition {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Stages = slices.Clone(value.Stages)
	for index := range cloned.Stages {
		cloned.Stages[index].DependsOn = slices.Clone(value.Stages[index].DependsOn)
		cloned.Stages[index].RequiredCapabilities = slices.Clone(value.Stages[index].RequiredCapabilities)
		cloned.Stages[index].Retry.RetryableCodes = slices.Clone(value.Stages[index].Retry.RetryableCodes)
		if value.Stages[index].AuthorityCeiling != nil {
			ceiling := *value.Stages[index].AuthorityCeiling
			ceiling.AllowedTools = slices.Clone(value.Stages[index].AuthorityCeiling.AllowedTools)
			cloned.Stages[index].AuthorityCeiling = &ceiling
		}
	}
	return &cloned
}

func cloneFindingGovernancePolicy(
	value *reviewconfig.FindingGovernancePolicy,
) *reviewconfig.FindingGovernancePolicy {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.CalibrationProfile.Points = slices.Clone(value.CalibrationProfile.Points)
	return &cloned
}

func buildLocalFormalPiBootstrap(
	options formalreview.LocalPiBootstrapOptions,
	variant *formalreview.LocalPiComponentVariant,
) (formalreview.LocalPiBootstrap, error) {
	if variant == nil {
		return formalreview.BuildLocalPiBootstrap(options)
	}
	return formalreview.BuildLocalPiBootstrapVariant(options, *variant)
}

func cloneLocalPiComponentVariant(
	value *formalreview.LocalPiComponentVariant,
) *formalreview.LocalPiComponentVariant {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Components = make([]formalreview.LocalPiGovernedComponent, len(value.Components))
	for index, component := range value.Components {
		cloned.Components[index] = component
		cloned.Components[index].Content = append([]byte{}, component.Content...)
	}
	return &cloned
}

func (executor *localFormalBatchExecutor) ExecuteReplay(
	ctx context.Context,
	request evaluation.ReplayCaseExecutionRequest,
) (string, error) {
	if request.ExecutorRevision != localFormalBatchExecutorRevision {
		return "", fmt.Errorf("unsupported batch executor revision %q", request.ExecutorRevision)
	}
	if request.ExecutorTemplateRef != executor.templateRef {
		return "", fmt.Errorf("batch executor template differs from admitted intent")
	}
	options := executor.template
	options.sourceFormalRun = request.SourceReviewRunID
	options.idempotencyKey = request.IdempotencyKey
	options.json = true
	var output bytes.Buffer
	if err := executeFormalAgentReplayWithRunner(ctx, options, &output, executor.runner); err != nil {
		return "", err
	}
	var decoded formalAgentRunOutput
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode formal replay output: %w", err)
	}
	if decoded.Status != contractsv1alpha1.StageExecutionSucceeded || decoded.FormalRunID == "" {
		return "", fmt.Errorf("formal replay did not return a succeeded ReviewRun")
	}
	return decoded.FormalRunID, nil
}

func runEvaluationBatch(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationBatchUsage)
	}
	switch arguments[0] {
	case "run":
		return runEvaluationBatchExecution(ctx, arguments[1:], stdout)
	case "resume":
		return runEvaluationBatchResumeWithRunner(
			ctx, arguments[1:], stdout, agentshadowworker.NewSubprocessRunner(),
		)
	case "show":
		options, err := parseGovernedReadFlags("evaluation batch show", arguments[1:],
			evaluationBatchUsage, "batch", "experiment batch ID")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		repository, storePath, err := openEvaluationRepository(options.store)
		if err != nil {
			return err
		}
		record, err := repository.GetExperimentBatch(options.id, access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, experimentBatchRecordOutput{Record: record, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "experiment_batch=%s status=%s store=%s\n",
			record.Request.BatchID, record.Status, storePath)
		return err
	default:
		return fmt.Errorf("unknown evaluation batch command %q\n%s", arguments[0], evaluationBatchUsage)
	}
}

func runEvaluationBatchResumeWithRunner(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	if runner == nil {
		return fmt.Errorf("formal Pi batch runner is required")
	}
	options, err := parseGovernedReadFlags(
		"evaluation batch resume", arguments, evaluationBatchUsage,
		"batch", "experiment batch ID",
	)
	if err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, storePath, err := openEvaluationRepository(options.store)
	if err != nil {
		return err
	}
	record, err := repository.GetExperimentBatch(options.id, access)
	if err != nil {
		return err
	}
	if record.Request.ExecutorTemplateRef == nil {
		return fmt.Errorf("experiment batch has no durable executor template")
	}
	store, err := local.Open(storePath)
	if err != nil {
		return err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	executor, err := loadLocalFormalBatchExecutor(
		runs, storePath, *record.Request.ExecutorTemplateRef, runner,
	)
	if err != nil {
		return fmt.Errorf("load frozen formal batch executor template: %w", err)
	}
	workerID, err := identity.NewGenerator().New("eval-batch-worker")
	if err != nil {
		return fmt.Errorf("create batch worker identity: %w", err)
	}
	batchRunner, err := evaluation.NewExperimentBatchRunner(
		repository, runs, executor, func() time.Time { return time.Now().UTC() },
		workerID, 5*time.Minute,
	)
	if err != nil {
		return err
	}
	result, err := batchRunner.Run(ctx, record.Request, record.Intent)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, experimentBatchOutput{Result: result, StorePath: storePath})
	}
	_, err = fmt.Fprintf(stdout,
		"experiment_batch=%s cases=%d evaluation=%s experiment=%s store=%s\n",
		result.BatchID, len(result.Cases), result.VariantEvaluationRunID,
		result.ExperimentRunID, storePath,
	)
	return err
}

func runEvaluationBatchExecution(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	return runEvaluationBatchExecutionWithRunner(
		ctx, arguments, stdout, agentshadowworker.NewSubprocessRunner(),
	)
}

func runEvaluationBatchExecutionWithRunner(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	if runner == nil {
		return fmt.Errorf("formal Pi batch runner is required")
	}
	separator := -1
	for index, argument := range arguments {
		if argument == "--" {
			if separator >= 0 {
				return fmt.Errorf("evaluation batch accepts exactly one -- separator")
			}
			separator = index
		}
	}
	if separator < 0 || separator == len(arguments)-1 {
		return fmt.Errorf("formal replay variant flags are required after --\n%s", evaluationBatchUsage)
	}
	options, err := parseGovernedWriteFlags("evaluation batch run", arguments[:separator], evaluationBatchUsage)
	if err != nil {
		return err
	}
	request, err := readStrictDescriptor(options.input, "experiment batch request",
		evaluation.DecodeExperimentBatchRequest)
	if err != nil {
		return err
	}
	if request.ExecutorTemplateRef != nil {
		return fmt.Errorf("experiment batch input must omit executor_template_ref; the local adapter freezes it")
	}
	mutation, err := readEvaluationMutation(options.mutation)
	if err != nil {
		return err
	}
	formalArguments := arguments[separator+1:]
	for _, argument := range formalArguments {
		for _, forbidden := range []string{"--store", "--source-formal-run", "--idempotency-key", "--json"} {
			if argument == forbidden || strings.HasPrefix(argument, forbidden+"=") {
				return fmt.Errorf("formal batch template must not set %s", forbidden)
			}
		}
	}
	formalArguments = append([]string{
		"--store", options.store,
		"--source-formal-run", "batch-source-placeholder",
		"--idempotency-key", "batch-idempotency-placeholder",
	}, formalArguments...)
	formal, err := parseFormalAgentReplayFlags(formalArguments)
	if err != nil {
		return fmt.Errorf("formal replay template: %w", err)
	}
	if formal.change != request.Variable {
		return fmt.Errorf("formal replay template changes %q, batch declares %q",
			formal.change, request.Variable)
	}
	if request.ExecutorRevision != localFormalBatchExecutorRevision {
		return fmt.Errorf("batch executor_revision must be %q", localFormalBatchExecutorRevision)
	}
	repository, storePath, err := openEvaluationRepository(options.store)
	if err != nil {
		return err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	templateRef, err := persistLocalFormalBatchExecutorTemplate(runs, formal)
	if err != nil {
		return fmt.Errorf("freeze formal batch executor template: %w", err)
	}
	request.ExecutorTemplateRef = &templateRef
	executor, err := loadLocalFormalBatchExecutor(runs, storePath, templateRef, runner)
	if err != nil {
		return fmt.Errorf("load frozen formal batch executor template: %w", err)
	}
	workerID, err := identity.NewGenerator().New("eval-batch-worker")
	if err != nil {
		return fmt.Errorf("create batch worker identity: %w", err)
	}
	batchRunner, err := evaluation.NewExperimentBatchRunner(
		repository, runs, executor,
		func() time.Time { return time.Now().UTC() },
		workerID, 5*time.Minute,
	)
	if err != nil {
		return err
	}
	result, err := batchRunner.Run(ctx, request, mutation)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, experimentBatchOutput{Result: result, StorePath: storePath})
	}
	_, err = fmt.Fprintf(stdout,
		"experiment_batch=%s cases=%d evaluation=%s experiment=%s store=%s\n",
		result.BatchID, len(result.Cases), result.VariantEvaluationRunID,
		result.ExperimentRunID, storePath)
	return err
}
