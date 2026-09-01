package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/abietic/argus/internal/agentcomponentrepo"
	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/analyticsadapter"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/calibration"
	"github.com/abietic/argus/internal/calibrationpromotion"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/contextprovider"
	"github.com/abietic/argus/internal/controlplane"
	"github.com/abietic/argus/internal/evaluation"
	feedbackdomain "github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/findingdecision"
	"github.com/abietic/argus/internal/findinglineage"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/identity"
	"github.com/abietic/argus/internal/normalizationpromotion"
	"github.com/abietic/argus/internal/platformapi"
	"github.com/abietic/argus/internal/promotionmonitor"
	"github.com/abietic/argus/internal/publication"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/reviewshard"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/training"
)

const (
	localAPITokenEnvironment = "ARGUS_LOCAL_API_TOKEN"
	apiUsage                 = `usage:
  ARGUS_LOCAL_API_TOKEN=<secret> argus api serve --store <absolute-dir> --config-state-dir <absolute-dir> --principal <absolute-json> [--listen <loopback-ip:port>] [formal Pi runtime/model/pricing flags]

The local API requires a fixed principal file and a bearer token of at least 32 bytes.
Only literal loopback listen addresses are accepted; the default is 127.0.0.1:7788.
Formal Pi jobs are disabled unless --formal-node, --formal-worker-script,
--formal-provider-profile, --formal-model and all three formal pricing flags are supplied.
Experiment/Repeatability batches may independently select --formal-batch-execution-backend hailix-http
with all formal-batch Hailix endpoint/verifier flags; request-time credentials remain fixed environment inputs.`
)

type apiServeFlags struct {
	store          string
	configState    string
	principal      string
	listen         string
	formal         formalAgentRunFlags
	batchTransport formalExecutionTransportFlags
	formalOn       bool
}

func runAPI(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf(apiUsage)
	}
	if arguments[0] == "help" {
		if len(arguments) != 1 {
			return fmt.Errorf(apiUsage)
		}
		_, err := fmt.Fprintln(stdout, apiUsage)
		return err
	}
	if arguments[0] != "serve" {
		return fmt.Errorf("unknown api command %q\n%s", arguments[0], apiUsage)
	}
	options, err := parseAPIServeFlags(arguments[1:])
	if err != nil {
		return err
	}
	storePath, err := validateExplicitControlStore(options.store)
	if err != nil {
		return err
	}
	principal, err := platformapi.LoadPrincipal(options.principal)
	if err != nil {
		return err
	}
	token, exists := os.LookupEnv(localAPITokenEnvironment)
	if !exists {
		return fmt.Errorf("%s is required", localAPITokenEnvironment)
	}
	if err := platformapi.ValidateBearerToken(token); err != nil {
		return err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return fmt.Errorf("open local API store: %w", err)
	}
	repository, err := evaluation.New(store)
	if err != nil {
		return fmt.Errorf("open local API evaluation repository: %w", err)
	}
	configState, err := validateExplicitConfigState(options.configState)
	if err != nil {
		return err
	}
	configStore, err := local.Open(configState)
	if err != nil {
		return fmt.Errorf("open local API config state: %w", err)
	}
	configRepository, err := configrepo.New(configStore)
	if err != nil {
		return fmt.Errorf("open local API config repository: %w", err)
	}
	dashboard, err := analyticsadapter.Open(store)
	if err != nil {
		return fmt.Errorf("open local API dashboard query adapter: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return fmt.Errorf("open local API run repository: %w", err)
	}
	trainingRepository, err := training.New(store, repository, runs)
	if err != nil {
		return fmt.Errorf("open local API training repository: %w", err)
	}
	trainingExporter, err := training.NewExporter(store, trainingRepository, runs)
	if err != nil {
		return fmt.Errorf("open local API training exporter: %w", err)
	}
	trainingJobs, err := training.NewJobService(store, trainingExporter, runs)
	if err != nil {
		return fmt.Errorf("open local API training job service: %w", err)
	}
	calibrationRepository, err := calibration.New(store, repository, runs)
	if err != nil {
		return fmt.Errorf("open local API calibration repository: %w", err)
	}
	calibrationPromotionService, err := calibrationpromotion.New(store, calibrationRepository, configRepository, repository, runs)
	if err != nil {
		return fmt.Errorf("open local API calibration promotion service: %w", err)
	}
	normalizationPromotionService, err := normalizationpromotion.New(repository, runs, configRepository)
	if err != nil {
		return fmt.Errorf("open local API normalization promotion service: %w", err)
	}
	promotionMonitorService, err := promotionmonitor.New(store, calibrationPromotionService, dashboard)
	if err != nil {
		return fmt.Errorf("open local API calibration promotion monitor: %w", err)
	}
	gitSource, err := gitadapter.New()
	if err != nil {
		return fmt.Errorf("initialize local API Git adapter: %w", err)
	}
	lineages, err := findinglineage.New(store, runs, nil, gitSource)
	if err != nil {
		return fmt.Errorf("open local API finding lineage repository: %w", err)
	}
	impactIndex, err := runs.RebuildImpactIndex(ctx)
	if err != nil {
		return fmt.Errorf("rebuild local API review run impact index: %w", err)
	}
	feedbackRepository, err := feedbackdomain.New(store)
	if err != nil {
		return fmt.Errorf("open local API feedback repository: %w", err)
	}
	publicationRepository, err := publication.NewRepository(store)
	if err != nil {
		return fmt.Errorf("open local API publication repository: %w", err)
	}
	decisionRepository, err := findingdecision.New(store)
	if err != nil {
		return fmt.Errorf("open local API finding decision repository: %w", err)
	}
	findingService, err := controlplane.New(
		runs, feedbackRepository, publicationRepository, decisionRepository,
	)
	if err != nil {
		return fmt.Errorf("open local API finding query service: %w", err)
	}
	findingService, err = findingService.WithFindingLineages(lineages)
	if err != nil {
		return fmt.Errorf("configure local API finding lineage evaluation evidence: %w", err)
	}
	workloads, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		return fmt.Errorf("open local API scheduling repository: %w", err)
	}
	jobRepository, err := reviewjob.NewRepository(store)
	if err != nil {
		return fmt.Errorf("open local API review job repository: %w", err)
	}
	shardRepository, err := reviewshard.NewRepository(store)
	if err != nil {
		return fmt.Errorf("open local API scope shard repository: %w", err)
	}
	contextExecutor, err := contextprovider.NewLocalExecutor(gitSource)
	if err != nil {
		return fmt.Errorf("initialize local API context providers: %w", err)
	}
	workerGenerator := identity.NewGenerator()
	workerID, err := workerGenerator.New("apiworker")
	if err != nil {
		return fmt.Errorf("create local API worker identity: %w", err)
	}
	deterministicExecutor := reviewjob.ExecuteFunc(func(
		executionContext context.Context,
		command reviewjob.Command,
	) (application.RunOutcome, error) {
		ids := &reviewJobIDGenerator{
			runID:    command.RunID,
			fallback: identity.NewGenerator(),
		}
		service, serviceErr := application.NewService(gitSource, runs, application.ServiceOptions{
			ConfigBundle: command.ConfigBundle,
			IDs:          ids, BuildIdentity: "argus-" + version,
			DisableScheduling: true, ContextProviders: contextExecutor,
		})
		if serviceErr != nil {
			return application.RunOutcome{}, serviceErr
		}
		return service.Review(executionContext, command.Request.ApplicationRequest())
	})
	deterministicClaimed := func(
		executionContext context.Context,
		command reviewjob.Command,
		dispatch scheduling.Dispatch,
	) (application.RunOutcome, error) {
		ids := &reviewJobIDGenerator{runID: command.RunID, fallback: identity.NewGenerator()}
		shards, shardErr := reviewshard.NewScopeExecutor(
			shardRepository, runs, dispatch, workerID, nil,
		)
		if shardErr != nil {
			return application.RunOutcome{}, shardErr
		}
		service, serviceErr := application.NewService(gitSource, runs, application.ServiceOptions{
			ConfigBundle: command.ConfigBundle,
			IDs:          ids, BuildIdentity: "argus-" + version,
			DisableScheduling: true, ContextProviders: contextExecutor,
			ScopeShards: shards, ExecutionDispatch: &dispatch,
		})
		if serviceErr != nil {
			return application.RunOutcome{}, serviceErr
		}
		history, historyErr := runs.History(0)
		if historyErr != nil {
			return application.RunOutcome{}, historyErr
		}
		for _, entry := range history {
			if entry.RunID == command.RunID {
				return service.ResumeScopeReview(
					executionContext, command.RunID, command.Request.RepositoryPath,
				)
			}
		}
		return service.Review(executionContext, command.Request.ApplicationRequest())
	}
	jobExecutor := &localReviewJobExecutor{
		deterministic: deterministicExecutor, deterministicClaimed: deterministicClaimed,
		runner:    agentshadowworker.NewSubprocessRunner(),
		storePath: store.Root(), workloads: workloads,
	}
	jobService, err := reviewjob.NewService(
		jobRepository, workloads, runs, configRepository, jobExecutor,
		workerID, nil,
	)
	if err != nil {
		return fmt.Errorf("initialize local API review jobs: %w", err)
	}
	if options.formalOn {
		formalAdmission, err := newLocalFormalAdmissionValidator(store)
		if err != nil {
			return fmt.Errorf("initialize formal Pi component admission: %w", err)
		}
		if err := jobService.ConfigureFormalProfile(reviewjob.FormalProfile{
			Options: options.formal.options, Pricing: options.formal.pricing,
			Admission: formalAdmission,
		}); err != nil {
			return fmt.Errorf("configure local API formal Pi jobs: %w", err)
		}
	}
	if err := jobService.Start(ctx); err != nil {
		return fmt.Errorf("start local API review jobs: %w", err)
	}
	batchControllerArguments := []formalAgentRunFlags{}
	if options.formalOn {
		batchFormal := cloneFormalAgentRunFlags(options.formal)
		batchFormal.transport = options.batchTransport
		batchControllerArguments = append(batchControllerArguments, batchFormal)
	}
	evaluationBatches, err := newLocalEvaluationBatchController(
		ctx, repository, runs, store.Root(), agentshadowworker.NewSubprocessRunner(),
		batchControllerArguments...,
	)
	if err != nil {
		return fmt.Errorf("initialize local API evaluation batches: %w", err)
	}
	componentPublisher, err := newLocalAgentComponentPublisher(store, runs)
	if err != nil {
		return fmt.Errorf("initialize local API component publisher: %w", err)
	}
	handler, err := platformapi.NewHandler(platformapi.Services{
		Evaluation:             repository,
		Calibration:            calibrationRepository,
		Training:               trainingRepository,
		TrainingExports:        trainingExporter,
		TrainingJobs:           trainingJobs,
		CalibrationPromotion:   calibrationPromotionService,
		PromotionMonitor:       promotionMonitorService,
		NormalizationPromotion: normalizationPromotionService,
		Config:                 configRepository,
		Dashboard:              dashboard,
		Runs:                   runs,
		Findings:               findingService,
		Lineages:               lineages,
		ReviewJobs:             jobService,
		Workloads:              workloads,
		EvaluationBatches:      evaluationBatches,
		Components:             componentPublisher,
	}, principal, token)
	if err != nil {
		return err
	}
	serveErr := platformapi.Serve(ctx, options.listen, handler, func(address string) error {
		_, err := fmt.Fprintf(
			stdout,
			"Argus local API listening on http://%s profile_revision=%s store=%s config_state=%s impact_index_complete=%t impact_index_appended=%d impact_index_gaps=%d\n",
			address,
			principal.ProfileRevision,
			store.Root(),
			configStore.Root(),
			impactIndex.Complete,
			impactIndex.AppendedFacts,
			len(impactIndex.Gaps),
		)
		return err
	})
	jobService.Wait()
	evaluationBatches.Wait()
	return serveErr
}

type localFormalAdmissionValidator struct {
	components *agentcomponentrepo.Repository
}

func newLocalFormalAdmissionValidator(
	store *local.Store,
) (*localFormalAdmissionValidator, error) {
	artifacts, err := artifactrepo.Open(store, nil)
	if err != nil {
		return nil, err
	}
	components, err := agentcomponentrepo.New(
		store, artifacts, artifactrepo.DefaultAuthority,
	)
	if err != nil {
		return nil, err
	}
	return &localFormalAdmissionValidator{components: components}, nil
}

func (validator *localFormalAdmissionValidator) ValidateFormalAdmission(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	bootstrap formalreview.LocalPiBootstrap,
) error {
	if validator == nil || validator.components == nil {
		return fmt.Errorf("formal component admission is not initialized")
	}
	registrySubject := agentcomponentrepo.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	for _, component := range bootstrap.Components {
		binding, err := validator.components.Resolve(
			ctx, registrySubject, component.Contract, component.Ref,
		)
		if err != nil {
			return fmt.Errorf("resolve %s %s@%s: %w",
				component.Contract, component.Ref.ID, component.Ref.Revision, err)
		}
		if binding.Ref.SHA256 != component.Ref.SHA256 ||
			binding.Artifact.Ref.SHA256 != component.Ref.SHA256 {
			return fmt.Errorf("resolved formal component digest changed")
		}
	}
	return nil
}

type localReviewJobExecutor struct {
	deterministic        reviewjob.Executor
	deterministicClaimed func(context.Context, reviewjob.Command, scheduling.Dispatch) (application.RunOutcome, error)
	runner               agentshadowworker.Runner
	storePath            string
	workloads            *scheduling.Repository
}

func (executor *localReviewJobExecutor) Execute(
	ctx context.Context,
	command reviewjob.Command,
) (application.RunOutcome, error) {
	if command.ExecutionProfile != reviewjob.DeterministicExecutionProfile {
		return application.RunOutcome{}, fmt.Errorf("formal Pi review requires a claimed dispatch")
	}
	return executor.deterministic.Execute(ctx, command)
}

func (executor *localReviewJobExecutor) CanResumeNonterminal(command reviewjob.Command) bool {
	return command.ExecutionProfile == reviewjob.FormalPiExecutionProfile ||
		command.ExecutionProfile == reviewjob.DeterministicExecutionProfile &&
			command.Request.Mode == "scope"
}

func (executor *localReviewJobExecutor) ExecuteClaimed(
	ctx context.Context,
	command reviewjob.Command,
	dispatch scheduling.Dispatch,
) (application.RunOutcome, error) {
	if command.ExecutionProfile == reviewjob.DeterministicExecutionProfile {
		if executor.deterministicClaimed == nil {
			return application.RunOutcome{}, fmt.Errorf("claimed deterministic executor is unavailable")
		}
		return executor.deterministicClaimed(ctx, command, dispatch)
	}
	if command.ExecutionProfile != reviewjob.FormalPiExecutionProfile || command.FormalRuntime == nil {
		return application.RunOutcome{}, fmt.Errorf("unsupported claimed review profile %q", command.ExecutionProfile)
	}
	liveBootstrap, err := formalreview.BuildLocalPiBootstrap(command.FormalRuntime.Options)
	if err != nil {
		return application.RunOutcome{}, fmt.Errorf("revalidate frozen formal runtime: %w", err)
	}
	want, err := json.Marshal(command.FormalRuntime.Bootstrap)
	if err != nil {
		return application.RunOutcome{}, err
	}
	got, err := json.Marshal(liveBootstrap)
	if err != nil {
		return application.RunOutcome{}, err
	}
	if !bytes.Equal(want, got) {
		return application.RunOutcome{}, fmt.Errorf("formal Pi runtime or governed component bytes drifted after admission")
	}
	frozen, err := formalreview.NewFrozenReplayConfigProvider(
		command.ConfigBundle, command.ConfigReceipt,
	)
	if err != nil {
		return application.RunOutcome{}, err
	}
	options := formalAgentRunFlags{
		formalAgentBootstrapFlags: formalAgentBootstrapFlags{
			store: executor.storePath, sourceRun: command.Request.SourceRunID,
			idempotencyKey:  command.IdempotencyKey,
			node:            command.FormalRuntime.Options.NodePath,
			workerScript:    command.FormalRuntime.Options.WorkerScript,
			providerProfile: command.FormalRuntime.Options.ProviderProfile,
			model:           command.FormalRuntime.Options.Model, json: true,
			options: command.FormalRuntime.Options,
		},
		pricing: command.FormalRuntime.Pricing,
	}
	var output bytes.Buffer
	executeErr := executeFormalAgentRunMode(
		ctx, options, &output, executor.runner, nil, frozen, &dispatch, executor.workloads, nil,
	)
	var formalOutput formalAgentRunOutput
	if output.Len() > 0 {
		if err := json.Unmarshal(output.Bytes(), &formalOutput); err != nil {
			return application.RunOutcome{}, fmt.Errorf("decode formal Pi execution output: %w", err)
		}
	}
	outcome := application.RunOutcome{}
	if formalOutput.ReviewRun != nil {
		outcome.Run = *formalOutput.ReviewRun
	}
	if formalOutput.FinalRunRef != nil {
		outcome.FinalRef = *formalOutput.FinalRunRef
	}
	return outcome, executeErr
}

type reviewJobIDGenerator struct {
	runID    string
	usedRun  bool
	fallback identity.Generator
}

func (generator *reviewJobIDGenerator) New(prefix string) (string, error) {
	if prefix == "run" && !generator.usedRun {
		generator.usedRun = true
		return generator.runID, nil
	}
	return generator.fallback.New(prefix)
}

func parseAPIServeFlags(arguments []string) (apiServeFlags, error) {
	options := apiServeFlags{
		listen:         "127.0.0.1:7788",
		batchTransport: formalExecutionTransportFlags{Backend: formalExecutionBackendLocalPi},
	}
	flags := newFlagSet("api serve")
	flags.StringVar(&options.store, "store", "", "absolute local store directory")
	flags.StringVar(&options.configState, "config-state-dir", "", "absolute configuration lifecycle state directory")
	flags.StringVar(&options.principal, "principal", "", "absolute strict JSON principal file")
	flags.StringVar(&options.listen, "listen", options.listen, "literal loopback IP and port")
	flags.StringVar(&options.formal.node, "formal-node", "", "absolute Node.js executable enabling formal Pi jobs")
	flags.StringVar(&options.formal.workerScript, "formal-worker-script", "", "absolute formal Pi worker script")
	flags.StringVar(&options.formal.providerProfile, "formal-provider-profile", "", "formal Pi provider profile")
	flags.StringVar(&options.formal.model, "formal-model", "", "formal Pi provider model ID")
	flags.Var(&options.formal.knowledge, "formal-knowledge", "absolute governed knowledge Markdown; repeatable")
	flags.IntVar(&options.formal.options.MaxFiles, "formal-max-files", 32, "maximum frozen files")
	flags.IntVar(&options.formal.options.MaxGroups, "formal-max-groups", 8, "maximum change groups")
	flags.IntVar(&options.formal.options.MaxHypotheses, "formal-max-hypotheses", 32, "maximum retained hypotheses")
	flags.IntVar(&options.formal.options.MaxModelCalls, "formal-max-model-calls", 96, "maximum provider turns")
	flags.IntVar(&options.formal.options.MaxToolCalls, "formal-max-tool-calls", 24, "maximum tools per task")
	flags.Int64Var(&options.formal.options.MaxTargetBytes, "formal-max-target-bytes", 4<<20, "maximum frozen target bytes")
	flags.Int64Var(&options.formal.options.MaxGroupBytes, "formal-max-group-bytes", 64<<10, "maximum group bytes")
	flags.Int64Var(&options.formal.options.MaxOutputBytes, "formal-max-output-bytes", 1<<20, "maximum output bytes")
	flags.Int64Var(&options.formal.options.MaxOutputTokens, "formal-max-output-tokens", 8192, "maximum output tokens per turn")
	flags.Int64Var(&options.formal.options.MaxCostMicros, "formal-max-cost-micros", 1_000_000, "maximum worst-case cost micros")
	flags.Int64Var(&options.formal.options.TimeoutMS, "formal-timeout-ms", 180_000, "formal stage timeout")
	flags.IntVar(&options.formal.options.MaxConcurrency, "formal-max-concurrency", 4, "maximum concurrent tasks")
	flags.IntVar(&options.formal.options.MaxAttempts, "formal-max-attempts", 1, "maximum authenticated formal attempts")
	flags.Uint64Var(&options.formal.pricing.InputMicrosPerMillionTokens, "formal-input-micros-per-million", 0, "governed worst-case input rate")
	flags.Uint64Var(&options.formal.pricing.OutputMicrosPerMillionTokens, "formal-output-micros-per-million", 0, "governed worst-case output rate")
	flags.Uint64Var(&options.formal.pricing.MaximumBytesPerInputToken, "formal-max-bytes-per-input-token", 0, "conservative input token conversion")
	flags.StringVar(&options.batchTransport.Backend, "formal-batch-execution-backend", formalExecutionBackendLocalPi, "formal batch execution backend (local-pi or hailix-http)")
	flags.StringVar(&options.batchTransport.HailixBaseURL, "formal-batch-hailix-base-url", "", "clean Hailix platform-execution base URL")
	flags.StringVar(&options.batchTransport.HailixCapabilityVerifierID, "formal-batch-hailix-capability-verifier-id", "", "pinned Hailix capability verifier ID")
	flags.StringVar(&options.batchTransport.HailixCapabilityVerifierRev, "formal-batch-hailix-capability-verifier-revision", "", "pinned Hailix capability verifier revision")
	flags.StringVar(&options.batchTransport.HailixCapabilityVerifierHash, "formal-batch-hailix-capability-verifier-sha256", "", "pinned Hailix capability verifier SHA-256")
	flags.StringVar(&options.batchTransport.HailixCallbackVerifierID, "formal-batch-hailix-callback-verifier-id", "", "pinned Hailix callback verifier ID")
	flags.StringVar(&options.batchTransport.HailixCallbackVerifierRev, "formal-batch-hailix-callback-verifier-revision", "", "pinned Hailix callback verifier revision")
	flags.StringVar(&options.batchTransport.HailixCallbackVerifierHash, "formal-batch-hailix-callback-verifier-sha256", "", "pinned Hailix callback verifier SHA-256")
	if err := flags.Parse(arguments); err != nil {
		return options, fmt.Errorf("api serve flags: %w\n%s", err, apiUsage)
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("api serve accepts no positional arguments\n%s", apiUsage)
	}
	if options.principal == "" {
		return options, fmt.Errorf("--principal is required\n%s", apiUsage)
	}
	formalValues := []string{
		options.formal.node, options.formal.workerScript,
		options.formal.providerProfile, options.formal.model,
	}
	formalCount := 0
	for _, value := range formalValues {
		if value != "" {
			formalCount++
		}
	}
	if formalCount != 0 {
		if formalCount != len(formalValues) ||
			options.formal.pricing.InputMicrosPerMillionTokens == 0 ||
			options.formal.pricing.OutputMicrosPerMillionTokens == 0 ||
			options.formal.pricing.MaximumBytesPerInputToken == 0 {
			return options, fmt.Errorf("all formal runtime, model, and pricing flags are required together")
		}
		for _, path := range []string{options.formal.node, options.formal.workerScript} {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return options, fmt.Errorf("formal runtime paths must be clean absolute paths")
			}
		}
		for _, path := range options.formal.knowledge {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return options, fmt.Errorf("formal knowledge paths must be clean absolute paths")
			}
		}
		options.formal.options.NodePath = options.formal.node
		options.formal.options.WorkerScript = options.formal.workerScript
		options.formal.options.ProviderProfile = options.formal.providerProfile
		options.formal.options.Model = options.formal.model
		options.formal.options.KnowledgePaths = slices.Clone(options.formal.knowledge)
		options.formalOn = true
	}
	if err := options.batchTransport.validateWithPrefix("formal-batch-"); err != nil {
		return options, err
	}
	if options.batchTransport.Backend == formalExecutionBackendHailixHTTP && !options.formalOn {
		return options, fmt.Errorf("formal Hailix batch transport requires the complete formal runtime/model/pricing profile")
	}
	if options.configState == "" {
		return options, fmt.Errorf("--config-state-dir is required\n%s", apiUsage)
	}
	if err := platformapi.ValidateListenAddress(options.listen); err != nil {
		return options, err
	}
	return options, nil
}
