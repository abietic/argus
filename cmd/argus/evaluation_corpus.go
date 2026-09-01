package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	localFormalCorpusExecutorRevision              = "argus-local-formal-pi-corpus-1"
	localFormalCorpusExecutorTemplateSchemaVersion = "argus.local_formal_corpus_executor_template.v1alpha1"
)

const evaluationCorpusUsage = `usage:
  argus evaluation corpus snapshot build --store <absolute-dir> --input <absolute-json> --access <absolute-json> [--json]
  argus evaluation corpus snapshot show --store <absolute-dir> --ref <absolute-json> --access <absolute-json> [--json]
  argus evaluation corpus run --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json] -- <formal run flags>
  argus evaluation corpus resume --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation corpus show --store <absolute-dir> --batch <id> --access <absolute-json> [--json]

The formal flags are the same as "agent-review formal run" except --store,
--source-run, --idempotency-key, and --json, which the corpus runner owns.`

type localFormalCorpusExecutorTemplate struct {
	SchemaVersion    string                               `json:"schema_version"`
	ExecutorRevision string                               `json:"executor_revision"`
	ConfigStateDir   string                               `json:"config_state_dir"`
	Options          formalreview.LocalPiBootstrapOptions `json:"options"`
	Bootstrap        formalreview.LocalPiBootstrap        `json:"bootstrap"`
	Pricing          piexecution.PricingCeiling           `json:"pricing"`
	CreatedAt        time.Time                            `json:"created_at"`
}

func (template localFormalCorpusExecutorTemplate) Validate() error {
	if template.SchemaVersion != localFormalCorpusExecutorTemplateSchemaVersion {
		return fmt.Errorf("unsupported local formal corpus executor template schema %q", template.SchemaVersion)
	}
	if template.ExecutorRevision != localFormalCorpusExecutorRevision {
		return fmt.Errorf("unsupported local formal corpus executor revision %q", template.ExecutorRevision)
	}
	if !filepath.IsAbs(template.ConfigStateDir) || filepath.Clean(template.ConfigStateDir) != template.ConfigStateDir {
		return fmt.Errorf("formal corpus config state must be a clean absolute path")
	}
	if template.CreatedAt.IsZero() || template.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("formal corpus executor created_at must be non-zero UTC")
	}
	live, err := formalreview.BuildLocalPiBootstrap(template.Options)
	if err != nil {
		return fmt.Errorf("rebuild local formal corpus bootstrap: %w", err)
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
		return fmt.Errorf("local formal corpus runtime or governed component bytes drifted")
	}
	if _, err := piexecution.NewCapabilityResolver(template.Bootstrap.Manifest, template.Pricing); err != nil {
		return fmt.Errorf("validate local formal corpus pricing: %w", err)
	}
	return nil
}

type localFormalCorpusExecutor struct {
	storePath   string
	template    localFormalCorpusExecutorTemplate
	templateRef runmodel.ArtifactRef
	runner      agentshadowworker.Runner
}

func persistLocalFormalCorpusExecutorTemplate(
	runs *runrepo.Repository,
	formal formalAgentRunFlags,
	createdAt time.Time,
) (runmodel.ArtifactRef, error) {
	if runs == nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("run repository is required")
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(formal.options)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	template := localFormalCorpusExecutorTemplate{
		SchemaVersion:    localFormalCorpusExecutorTemplateSchemaVersion,
		ExecutorRevision: localFormalCorpusExecutorRevision,
		ConfigStateDir:   formal.configState,
		Options:          formal.options,
		Bootstrap:        bootstrap,
		Pricing:          formal.pricing,
		CreatedAt:        createdAt.UTC(),
	}
	if err := template.Validate(); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return runs.PutJSONArtifact(evaluation.FormalCorpusExecutorTemplateContract, template)
}

func loadLocalFormalCorpusExecutor(
	runs *runrepo.Repository,
	storePath string,
	ref runmodel.ArtifactRef,
	runner agentshadowworker.Runner,
) (*localFormalCorpusExecutor, error) {
	if runs == nil || runner == nil {
		return nil, fmt.Errorf("run repository and formal Pi runner are required")
	}
	if ref.Contract != evaluation.FormalCorpusExecutorTemplateContract {
		return nil, fmt.Errorf("unsupported formal corpus executor template contract %q", ref.Contract)
	}
	data, err := runs.ReadArtifact(ref)
	if err != nil {
		return nil, err
	}
	template, err := decodeStrictCommandJSON(data, "LocalFormalCorpusExecutorTemplate", func(value localFormalCorpusExecutorTemplate) error {
		return value.Validate()
	})
	if err != nil {
		return nil, err
	}
	return &localFormalCorpusExecutor{storePath: storePath, template: template, templateRef: ref, runner: runner}, nil
}

func (executor *localFormalCorpusExecutor) ExecuteFormalCorpusCase(
	ctx context.Context,
	request evaluation.FormalCorpusCaseExecutionRequest,
) (string, error) {
	if request.ExecutorRevision != localFormalCorpusExecutorRevision {
		return "", fmt.Errorf("unsupported formal corpus executor revision %q", request.ExecutorRevision)
	}
	if request.ExecutorTemplateRef != executor.templateRef {
		return "", fmt.Errorf("formal corpus executor template differs from admitted intent")
	}
	bootstrapOptions := formalAgentBootstrapFlags{
		store:           executor.storePath,
		configState:     executor.template.ConfigStateDir,
		sourceRun:       request.SourceReviewRunID,
		idempotencyKey:  request.IdempotencyKey + "-bootstrap",
		at:              executor.template.CreatedAt.Format(time.RFC3339Nano),
		node:            executor.template.Options.NodePath,
		workerScript:    executor.template.Options.WorkerScript,
		providerProfile: executor.template.Options.ProviderProfile,
		model:           executor.template.Options.Model,
		json:            true,
		options:         executor.template.Options,
	}
	if err := executeFormalAgentBootstrap(ctx, bootstrapOptions, &bytes.Buffer{}); err != nil {
		return "", fmt.Errorf("bootstrap formal corpus case %q: %w", request.CaseID, err)
	}
	runOptions := formalAgentRunFlags{
		formalAgentBootstrapFlags: bootstrapOptions,
		pricing:                   executor.template.Pricing,
	}
	runOptions.idempotencyKey = request.IdempotencyKey + "-run"
	var output bytes.Buffer
	if err := executeFormalAgentRunWithRunner(ctx, runOptions, &output, executor.runner); err != nil {
		return "", err
	}
	var decoded formalAgentRunOutput
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode formal corpus run output: %w", err)
	}
	if decoded.Status != contractsv1alpha1.StageExecutionSucceeded || decoded.FormalRunID == "" {
		return "", fmt.Errorf("formal corpus case did not return a succeeded ReviewRun")
	}
	return decoded.FormalRunID, nil
}

type formalCorpusOutput struct {
	Result    evaluation.FormalCorpusBatchResult `json:"result"`
	StorePath string                             `json:"store_path"`
}

type formalCorpusRecordOutput struct {
	Record    evaluation.FormalCorpusBatchRecord `json:"record"`
	StorePath string                             `json:"store_path"`
}

type corpusSnapshotOutput struct {
	Snapshot    evaluation.CorpusSnapshot `json:"snapshot"`
	SnapshotRef runmodel.ArtifactRef      `json:"snapshot_ref"`
	StorePath   string                    `json:"store_path"`
}

type corpusSnapshotFlags struct {
	store  string
	input  string
	ref    string
	access string
	json   bool
}

func runEvaluationCorpus(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationCorpusUsage)
	}
	switch arguments[0] {
	case "snapshot":
		return runEvaluationCorpusSnapshot(ctx, arguments[1:], stdout)
	case "run":
		return runEvaluationCorpusExecutionWithRunner(ctx, arguments[1:], stdout, agentshadowworker.NewSubprocessRunner())
	case "resume":
		return runEvaluationCorpusResumeWithRunner(ctx, arguments[1:], stdout, agentshadowworker.NewSubprocessRunner())
	case "show":
		options, err := parseGovernedReadFlags("evaluation corpus show", arguments[1:], evaluationCorpusUsage, "batch", "formal corpus batch ID")
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
		record, err := repository.GetFormalCorpusBatch(options.id, access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, formalCorpusRecordOutput{Record: record, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "formal_corpus_batch=%s status=%s completed_cases=%d store=%s\n", record.Request.BatchID, record.Status, len(record.CompletedCases), storePath)
		return err
	default:
		return fmt.Errorf("unknown evaluation corpus command %q\n%s", arguments[0], evaluationCorpusUsage)
	}
}

func runEvaluationCorpusSnapshot(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 || (arguments[0] != "build" && arguments[0] != "show") {
		return fmt.Errorf("unknown evaluation corpus snapshot command\n%s", evaluationCorpusUsage)
	}
	action := arguments[0]
	var options corpusSnapshotFlags
	flags := newFlagSet("evaluation corpus snapshot " + action)
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	if action == "build" {
		flags.StringVar(&options.input, "input", "", "absolute strict JSON CorpusSnapshotRequest")
	} else {
		flags.StringVar(&options.ref, "ref", "", "absolute strict JSON CorpusSnapshot ArtifactRef")
	}
	flags.StringVar(&options.access, "access", "", "absolute strict JSON evaluation access descriptor")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return controlFlagError("evaluation corpus snapshot "+action, err, evaluationCorpusUsage)
	}
	if flags.NArg() != 0 {
		return controlFlagError(
			"evaluation corpus snapshot "+action,
			fmt.Errorf("unexpected argument %q", flags.Arg(0)), evaluationCorpusUsage,
		)
	}
	storePath, err := validateExplicitControlStore(options.store)
	if err != nil {
		return err
	}
	if err := validateDescriptorPath("access", options.access); err != nil {
		return err
	}
	if action == "build" {
		if err := validateDescriptorPath("input", options.input); err != nil {
			return err
		}
	} else if err := validateDescriptorPath("ref", options.ref); err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, _, err := openEvaluationRepository(storePath)
	if err != nil {
		return err
	}
	state, err := local.Open(storePath)
	if err != nil {
		return fmt.Errorf("open corpus snapshot store: %w", err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		return err
	}
	builder, err := evaluation.NewCorpusSnapshotBuilder(repository, runs)
	if err != nil {
		return err
	}
	var snapshot evaluation.CorpusSnapshot
	var ref runmodel.ArtifactRef
	if action == "build" {
		request, readErr := readStrictDescriptor(
			options.input, "corpus snapshot request", evaluation.DecodeCorpusSnapshotRequest,
		)
		if readErr != nil {
			return readErr
		}
		snapshot, err = builder.Build(ctx, request, access)
		if err != nil {
			return err
		}
		ref, err = runs.PutJSONArtifact(evaluation.CorpusSnapshotContract, snapshot)
		if err != nil {
			return fmt.Errorf("persist corpus snapshot: %w", err)
		}
	} else {
		ref, err = readStrictDescriptor(options.ref, "corpus snapshot ref", func(data []byte) (runmodel.ArtifactRef, error) {
			return decodeStrictCommandJSON(data, "CorpusSnapshotRef", func(value runmodel.ArtifactRef) error {
				if err := value.Validate(); err != nil {
					return err
				}
				if value.Contract != evaluation.CorpusSnapshotContract {
					return fmt.Errorf("corpus snapshot ref has unsupported contract %q", value.Contract)
				}
				return nil
			})
		})
		if err != nil {
			return err
		}
		data, readErr := runs.ReadArtifact(ref)
		if readErr != nil {
			return readErr
		}
		snapshot, err = evaluation.DecodeCorpusSnapshot(data)
		if err != nil {
			return err
		}
		if err := builder.Authorize(snapshot, access); err != nil {
			return err
		}
	}
	output := corpusSnapshotOutput{Snapshot: snapshot, SnapshotRef: ref, StorePath: state.Root()}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout, "corpus=%s revision=%s cases=%d sha256=%s store=%s\n",
		snapshot.CorpusID, snapshot.Revision, len(snapshot.Cases), ref.SHA256, state.Root(),
	)
	return err
}

func runEvaluationCorpusExecutionWithRunner(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	if runner == nil {
		return fmt.Errorf("formal Pi corpus runner is required")
	}
	separator := -1
	for index, argument := range arguments {
		if argument == "--" {
			if separator >= 0 {
				return fmt.Errorf("evaluation corpus accepts exactly one -- separator")
			}
			separator = index
		}
	}
	if separator < 0 || separator == len(arguments)-1 {
		return fmt.Errorf("formal run flags are required after --\n%s", evaluationCorpusUsage)
	}
	options, err := parseGovernedWriteFlags("evaluation corpus run", arguments[:separator], evaluationCorpusUsage)
	if err != nil {
		return err
	}
	request, err := readStrictDescriptor(options.input, "formal corpus batch request", evaluation.DecodeFormalCorpusBatchRequest)
	if err != nil {
		return err
	}
	if request.ExecutorTemplateRef != nil {
		return fmt.Errorf("formal corpus input must omit executor_template_ref; the local adapter freezes it")
	}
	if request.ExecutorRevision != localFormalCorpusExecutorRevision {
		return fmt.Errorf("formal corpus executor_revision must be %q", localFormalCorpusExecutorRevision)
	}
	mutation, err := readEvaluationMutation(options.mutation)
	if err != nil {
		return err
	}
	formalArguments := arguments[separator+1:]
	for _, argument := range formalArguments {
		for _, forbidden := range []string{"--store", "--source-run", "--idempotency-key", "--json"} {
			if argument == forbidden || strings.HasPrefix(argument, forbidden+"=") {
				return fmt.Errorf("formal corpus template must not set %s", forbidden)
			}
		}
	}
	formalArguments = append([]string{"--store", options.store, "--source-run", "corpus-source-placeholder", "--idempotency-key", "corpus-idempotency-placeholder"}, formalArguments...)
	formal, err := parseFormalAgentRunFlags(formalArguments)
	if err != nil {
		return fmt.Errorf("formal corpus template: %w", err)
	}
	repository, storePath, err := openEvaluationRepository(options.store)
	if err != nil {
		return err
	}
	state, err := local.Open(storePath)
	if err != nil {
		return err
	}
	runs, err := runrepo.New(state)
	if err != nil {
		return err
	}
	templateRef, err := persistLocalFormalCorpusExecutorTemplate(runs, formal, request.CreatedAt)
	if err != nil {
		return fmt.Errorf("freeze formal corpus executor template: %w", err)
	}
	request.ExecutorTemplateRef = &templateRef
	executor, err := loadLocalFormalCorpusExecutor(runs, storePath, templateRef, runner)
	if err != nil {
		return fmt.Errorf("load frozen formal corpus executor: %w", err)
	}
	batchRunner, err := evaluation.NewFormalCorpusBatchRunner(repository, runs, executor, func() time.Time { return time.Now().UTC() })
	if err != nil {
		return err
	}
	result, err := batchRunner.Run(ctx, request, mutation)
	if err != nil {
		return err
	}
	return writeFormalCorpusResult(stdout, options.json, result, storePath)
}

func runEvaluationCorpusResumeWithRunner(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	if runner == nil {
		return fmt.Errorf("formal Pi corpus runner is required")
	}
	options, err := parseGovernedReadFlags("evaluation corpus resume", arguments, evaluationCorpusUsage, "batch", "formal corpus batch ID")
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
	record, err := repository.GetFormalCorpusBatch(options.id, access)
	if err != nil {
		return err
	}
	if record.Request.ExecutorTemplateRef == nil {
		return fmt.Errorf("formal corpus batch has no durable executor template")
	}
	state, err := local.Open(storePath)
	if err != nil {
		return err
	}
	runs, err := runrepo.New(state)
	if err != nil {
		return err
	}
	executor, err := loadLocalFormalCorpusExecutor(runs, storePath, *record.Request.ExecutorTemplateRef, runner)
	if err != nil {
		return fmt.Errorf("load frozen formal corpus executor: %w", err)
	}
	batchRunner, err := evaluation.NewFormalCorpusBatchRunner(repository, runs, executor, func() time.Time { return time.Now().UTC() })
	if err != nil {
		return err
	}
	result, err := batchRunner.Run(ctx, record.Request, record.Intent)
	if err != nil {
		return err
	}
	return writeFormalCorpusResult(stdout, options.json, result, storePath)
}

func writeFormalCorpusResult(stdout io.Writer, jsonOutput bool, result evaluation.FormalCorpusBatchResult, storePath string) error {
	if jsonOutput {
		return writeJSON(stdout, formalCorpusOutput{Result: result, StorePath: storePath})
	}
	_, err := fmt.Fprintf(stdout, "formal_corpus_batch=%s cases=%d evaluation=%s store=%s\n", result.BatchID, len(result.Cases), result.EvaluationRunID, storePath)
	return err
}
