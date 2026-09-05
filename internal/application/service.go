package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/abietic/argus/internal/identity"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const FindingSetSchemaVersion = "argus.finding_set.v1alpha1"

type IDGenerator interface {
	New(string) (string, error)
}

type StageExecutor func(
	context.Context,
	reviewcore.ReviewInput,
	reviewcore.StageName,
	reviewcore.StageResult,
	reviewcore.RuntimePolicy,
) (reviewcore.StageResult, error)

type ServiceOptions struct {
	Config         LocalConfig
	ConfigBundle   reviewconfig.ConfigBundle
	ConfigProvider ConfigProvider
	Workflow       workflow.Definition
	IDs            IDGenerator
	Now            func() time.Time
	// ExecuteStage is a trusted in-process test/embedding hook, not a platform
	// adapter. Its executable identity must be declared explicitly.
	ExecuteStage     StageExecutor
	ExecutorIdentity string
	// ExecutorCapabilities is required for trusted hooks. The caller attests
	// the effective authority of every executor named by the frozen workflow.
	ExecutorCapabilities map[string]workflow.ExecutorCapabilities
	BuildIdentity        string
	// Workloads is the narrow run-level durable scheduling boundary. The
	// synchronous local CLI injects a scheduling.Repository backed by the
	// same state root as run history. It coordinates one whole run; it does
	// not claim stage fan-out or crash-resume semantics.
	Workloads scheduling.WorkloadPort
	WorkerID  string
	// DisableScheduling is intended for focused unit tests and embedders that
	// deliberately own execution coordination outside this Service.
	DisableScheduling bool
	// ExecutionDispatch is the already-authenticated outer ReviewJob lease when
	// this service is embedded under the asynchronous coordinator.
	ExecutionDispatch *scheduling.Dispatch
	// ExecutionAuthority revalidates the current outer lease before durable
	// run facts are appended. It must fail after cancellation or reassignment.
	ExecutionAuthority func(context.Context, scheduling.Dispatch) error
	// ExecutionCancellationAuthority permits only cancellation closure after
	// the scheduler has canceled this exact lease without a successor owner.
	ExecutionCancellationAuthority func(context.Context, scheduling.Dispatch) error
	// WorkloadTimeout freezes the run-level execution deadline. Zero selects
	// the bounded local default.
	WorkloadTimeout time.Duration
	// ContextProviders executes only version-pinned definitions from the
	// resolved ConfigBundle. Missing executors become explicit ContextGaps.
	ContextProviders ContextProviderExecutor
	// ScopeShards enables durable, generation-fenced scope detect fan-out.
	ScopeShards ScopeShardPort
}

type Service struct {
	source                         TargetSource
	repository                     *runrepo.Repository
	config                         LocalConfig
	configSource                   LocalConfig
	configBundle                   reviewconfig.ConfigBundle
	defaultBundle                  bool
	configProvider                 ConfigProvider
	workflow                       workflow.Definition
	ids                            IDGenerator
	now                            func() time.Time
	clockMu                        sync.Mutex
	executeStage                   StageExecutor
	customExecutor                 bool
	executorKind                   string
	executorID                     string
	executorCapabilities           map[string]workflow.ExecutorCapabilities
	buildIdentity                  string
	workloads                      scheduling.WorkloadPort
	workerID                       string
	workloadTimeout                time.Duration
	contextProviders               ContextProviderExecutor
	scopeShards                    ScopeShardPort
	executionDispatch              *scheduling.Dispatch
	executionAuthority             func(context.Context, scheduling.Dispatch) error
	executionCancellationAuthority func(context.Context, scheduling.Dispatch) error
}

type RunOutcome struct {
	Run      runmodel.ReviewRun `json:"run"`
	FinalRef runmodel.ArtifactRef
	Report   *reviewcore.Report `json:"report,omitempty"`
	Markdown string             `json:"markdown,omitempty"`
	Reused   []reviewcore.StageName
}

type FindingSet struct {
	SchemaVersion string                       `json:"schema_version"`
	TargetDigest  string                       `json:"target_digest"`
	Findings      []reviewcore.Finding         `json:"findings"`
	Decisions     []reviewcore.FindingDecision `json:"decisions"`
}

type RunError struct {
	RunID   string
	Failure runmodel.Failure
}

func (err *RunError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("run %s: %s: %s", err.RunID, err.Failure.Code, err.Failure.Message)
}

type RetryableStageError struct {
	Code string
	Err  error
}

func (err *RetryableStageError) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.Code == "" {
		return err.Err.Error()
	}
	return err.Code + ": " + err.Err.Error()
}

func (err *RetryableStageError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func NewService(
	source TargetSource,
	repository *runrepo.Repository,
	options ServiceOptions,
) (*Service, error) {
	if source == nil || repository == nil {
		return nil, fmt.Errorf("source adapter and run repository are required")
	}
	definition := options.Workflow
	if definition.ID == "" {
		definition = workflow.DefaultReviewDefinition()
	}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("validate service workflow: %w", err)
	}
	if err := validateSupportedWorkflow(definition); err != nil {
		return nil, err
	}
	config := options.Config
	if config.SchemaVersion == "" {
		config = DefaultLocalConfig()
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate service runtime limits: %w", err)
	}
	configSource := config
	bundle := options.ConfigBundle
	if options.ConfigProvider != nil && bundle.SchemaVersion != "" {
		return nil, fmt.Errorf("config_provider and config_bundle are mutually exclusive")
	}
	defaultBundle := options.ConfigProvider == nil && bundle.SchemaVersion == ""
	if bundle.SchemaVersion == "" {
		var err error
		bundle, err = DefaultConfigBundle(config, definition)
		if err != nil {
			return nil, fmt.Errorf("build default config bundle: %w", err)
		}
	} else if err := bundle.Validate(); err != nil {
		return nil, fmt.Errorf("validate service config bundle: %w", err)
	}
	workflowDigest, err := workflow.DigestDefinition(definition)
	if err != nil {
		return nil, err
	}
	if bundle.Workflow.Definition.ID != definition.ID ||
		bundle.Workflow.Definition.Revision != definition.Revision ||
		bundle.Workflow.Definition.SHA256 != workflowDigest {
		return nil, fmt.Errorf("config bundle workflow does not match executable workflow")
	}
	if err := validateRuntimeEnvelope(bundle, definition); err != nil {
		return nil, err
	}
	runtimeConfig, err := runtimeConfigFromBundle(bundle, configSource)
	if err != nil {
		return nil, fmt.Errorf("derive runtime limits from config bundle: %w", err)
	}
	ids := options.IDs
	if ids == nil {
		generator := identity.NewGenerator()
		ids = generator
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if _, err := runtimePolicyFromBundle(bundle); err != nil {
		return nil, fmt.Errorf("derive executable review policy: %w", err)
	}
	executeStage := options.ExecuteStage
	buildIdentity := strings.TrimSpace(options.BuildIdentity)
	executorIdentity := strings.TrimSpace(options.ExecutorIdentity)
	executorKind := "deterministic-local"
	if executeStage != nil {
		if buildIdentity == "" || executorIdentity == "" {
			return nil, fmt.Errorf(
				"trusted custom stage executor requires build_identity and executor_identity",
			)
		}
		if err := validateLocalIdentity("executor_identity", executorIdentity); err != nil {
			return nil, err
		}
		executorKind = "trusted-local-hook"
	} else {
		executorIdentity = "deterministic-local"
	}
	executorCapabilities := cloneExecutorCapabilities(options.ExecutorCapabilities)
	if executorCapabilities == nil {
		if executeStage != nil {
			return nil, fmt.Errorf(
				"trusted custom stage executor requires executor_capabilities",
			)
		}
		executorCapabilities = map[string]workflow.ExecutorCapabilities{
			"deterministic-local": {},
		}
	}
	if err := validateLocalExecutorCapabilities(definition, executorCapabilities); err != nil {
		return nil, err
	}
	if buildIdentity == "" {
		buildIdentity = "argus-dev"
	}
	if options.Workloads != nil && options.DisableScheduling {
		return nil, fmt.Errorf("workloads and disable_scheduling are mutually exclusive")
	}
	if options.ExecutionAuthority != nil && options.ExecutionDispatch == nil {
		return nil, fmt.Errorf("execution_authority requires an outer execution_dispatch")
	}
	if options.ExecutionCancellationAuthority != nil && options.ExecutionAuthority == nil {
		return nil, fmt.Errorf("execution_cancellation_authority requires execution_authority")
	}
	if options.ExecutionDispatch != nil {
		if !options.DisableScheduling || options.Workloads != nil {
			return nil, fmt.Errorf("execution_dispatch requires externally coordinated scheduling")
		}
		if err := options.ExecutionDispatch.Spec.Validate(); err != nil {
			return nil, fmt.Errorf("validate execution dispatch workload: %w", err)
		}
		if err := options.ExecutionDispatch.Lease.Validate(); err != nil {
			return nil, fmt.Errorf("validate execution dispatch lease: %w", err)
		}
		if options.ExecutionDispatch.Spec.WorkloadID != options.ExecutionDispatch.Lease.WorkloadID {
			return nil, fmt.Errorf("execution dispatch workload and lease differ")
		}
	}
	if options.Workloads == nil && !options.DisableScheduling {
		return nil, fmt.Errorf(
			"run-level workload coordinator is required unless scheduling is explicitly disabled",
		)
	}
	workerID := strings.TrimSpace(options.WorkerID)
	workloadTimeout := options.WorkloadTimeout
	if options.Workloads != nil {
		if err := validateLocalIdentity("worker_id", workerID); err != nil {
			return nil, err
		}
		if workloadTimeout == 0 {
			workloadTimeout = defaultRunWorkloadTimeout
		}
		if workloadTimeout <= 0 || workloadTimeout > maxRunWorkloadTimeout {
			return nil, fmt.Errorf(
				"workload_timeout must be positive and at most %s",
				maxRunWorkloadTimeout,
			)
		}
	} else {
		workerID = ""
		workloadTimeout = 0
	}
	return &Service{
		source: source, repository: repository, config: runtimeConfig,
		configSource: configSource, configBundle: bundle, defaultBundle: defaultBundle,
		configProvider: options.ConfigProvider,
		workflow:       definition,
		ids:            ids, now: now, executeStage: executeStage,
		customExecutor: executeStage != nil,
		executorKind:   executorKind, executorID: executorIdentity,
		executorCapabilities:           executorCapabilities,
		buildIdentity:                  buildIdentity,
		workloads:                      options.Workloads,
		workerID:                       workerID,
		workloadTimeout:                workloadTimeout,
		contextProviders:               options.ContextProviders,
		scopeShards:                    options.ScopeShards,
		executionDispatch:              options.ExecutionDispatch,
		executionAuthority:             options.ExecutionAuthority,
		executionCancellationAuthority: options.ExecutionCancellationAuthority,
	}, nil
}

func (service *Service) Review(
	ctx context.Context,
	request ReviewRequest,
) (RunOutcome, error) {
	runID, err := service.ids.New("run")
	if err != nil {
		return RunOutcome{}, err
	}
	if err := service.verifyExecutionAuthority(ctx, runID); err != nil {
		return RunOutcome{}, err
	}
	bundle, runtimeConfig, err := service.configForReview(ctx, runID, request)
	if err != nil {
		return RunOutcome{}, err
	}
	materialized, providerReceiptRefs, err := service.prepareReviewInput(ctx, runID, request, bundle, runtimeConfig)
	if err != nil {
		return RunOutcome{}, err
	}
	if err := validateBundleContext(bundle, reviewconfig.ResolutionContext{
		TenantID:       localTenantID,
		OrganizationID: localOrganizationID,
		RepositoryID:   materialized.Target.Snapshot.Repository.RepositoryID,
		Path:           configContextPath(request),
		InvocationID:   runID,
	}); err != nil {
		return RunOutcome{}, err
	}
	policy, err := runtimePolicyFromBundle(bundle)
	if err != nil {
		return RunOutcome{}, err
	}
	policy.TargetComplete =
		materialized.Target.Snapshot.Completeness == gitadapter.CompletenessComplete
	spec, err := service.buildReviewSpec(runID, materialized, bundle)
	if err != nil {
		return RunOutcome{}, err
	}
	if err := service.admitReview(spec, false); err != nil {
		return RunOutcome{}, err
	}
	var shardManifestRef *runmodel.ArtifactRef
	shardCoverage := ScopeShardCoverage{Complete: true, ReasonCodes: []string{}}
	if materialized.Input.TargetMode == reviewcore.TargetModeScope && service.scopeShards != nil {
		if err := service.verifyExecutionAuthority(ctx, runID); err != nil {
			return RunOutcome{}, err
		}
		plannedAt, err := service.timestamp()
		if err != nil {
			return RunOutcome{}, err
		}
		plan, err := service.scopeShards.PlanScope(ctx, ScopeShardPlanRequest{
			RunID: runID, Input: materialized.Input, InputRef: materialized.InputRef,
			MaxInputBytes: bundle.Budget.MaxInputBytes, CreatedAt: plannedAt,
		})
		if err != nil {
			return RunOutcome{}, fmt.Errorf("plan scope shards: %w", err)
		}
		shardManifestRef = &plan.ManifestRef
		shardCoverage = plan.Coverage
	}
	if err := service.verifyExecutionAuthority(ctx, runID); err != nil {
		return RunOutcome{}, err
	}
	specRef, snapshot, err := service.persistExecutionSnapshot(
		runID,
		spec,
		materialized.TargetRef,
		materialized.InputRef,
		shardManifestRef,
		[]runmodel.ArtifactRef{},
		providerReceiptRefs,
		nil,
		nil,
		service.workflow,
		bundle,
	)
	if err != nil {
		return RunOutcome{}, err
	}
	_ = specRef
	createdAt, err := service.timestamp()
	if err != nil {
		return RunOutcome{}, err
	}
	startedAt, err := service.timestamp()
	if err != nil {
		return RunOutcome{}, err
	}
	run := runmodel.ReviewRun{
		SchemaVersion:       runmodel.RunSchemaVersion,
		RunID:               runID,
		Kind:                runmodel.RunKindReview,
		RepositoryPath:      materialized.RepositoryRoot,
		TargetMode:          runmodel.TargetMode(materialized.Target.Snapshot.Mode),
		BaseRevision:        materialized.Target.Snapshot.Base.CommitOID,
		HeadRevision:        materialized.Target.Snapshot.Head.CommitOID,
		Status:              runmodel.RunStatusRunning,
		ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		TargetSnapshotRef:   materialized.TargetRef,
		StageAttempts:       []runmodel.StageAttempt{},
		Bindings:            []runmodel.PlatformExecutionBinding{},
		Evidence:            []runmodel.RunEvidence{},
		CreatedAt:           createdAt,
		StartedAt:           &startedAt,
	}
	if err := service.appendRunLifecycleEvents(run, createdAt, startedAt); err != nil {
		return RunOutcome{}, err
	}
	workloadClass, err := workloadClassForReview(materialized.Target.Snapshot.Mode)
	if err != nil {
		return service.finalizeTerminal(
			run,
			runmodel.RunStatusFailed,
			&runmodel.Failure{Code: "scheduling_class_invalid", Message: err.Error()},
			nil,
			"",
		)
	}
	workload, err := service.beginRunWorkload(
		ctx,
		run,
		snapshot.ReviewInputRef,
		workloadClass,
	)
	if err != nil {
		status := runmodel.RunStatusFailed
		if errors.Is(err, context.Canceled) {
			status = runmodel.RunStatusCanceled
		}
		return service.finalizeTerminal(run, status, workloadFailure(err), nil, "")
	}
	completeness, completenessNotes := targetCompleteness(materialized.Target)
	completeness, completenessNotes = combineShardCompleteness(
		completeness, completenessNotes, shardCoverage,
	)
	return service.executeAndFinalize(
		ctx,
		run,
		materialized.Input,
		materialized.InputRef,
		snapshot.ReviewShardManifestRef,
		reviewcore.StageMaterializeTarget,
		nil,
		nil,
		completeness,
		completenessNotes,
		policy,
		bundle.Budget,
		service.workflow,
		workload,
		1,
	)
}

// ResumeScopeReview retains the scope-only entry point for existing embedders.
func (service *Service) ResumeScopeReview(
	ctx context.Context,
	runID string,
	repositoryPath string,
) (RunOutcome, error) {
	if service.scopeShards == nil || service.executionDispatch == nil {
		return RunOutcome{}, fmt.Errorf("scope recovery requires shard execution and outer dispatch authority")
	}
	snapshot, err := service.repository.ExecutionSnapshotForRun(runID)
	if err != nil {
		return RunOutcome{}, err
	}
	if snapshot.ReviewShardManifestRef == nil {
		return RunOutcome{}, fmt.Errorf("scope recovery requires a frozen shard manifest")
	}
	return service.ResumeReview(ctx, runID, repositoryPath)
}

// ResumeReview continues the frozen deterministic diff, selection, or sharded
// scope workflow under the caller's current ReviewJob dispatch. All executable
// and authorization checks precede mutations to the abandoned run ledger.
func (service *Service) ResumeReview(
	ctx context.Context,
	runID string,
	repositoryPath string,
) (RunOutcome, error) {
	return service.recoverReview(ctx, runID, repositoryPath, false)
}

// CancelReview closes an already-created run after cancellation won a race
// with a stage or lifecycle write. It only repairs previously durable facts;
// no stage is executed and no newly successful stage output is admitted.
func (service *Service) CancelReview(
	ctx context.Context,
	runID string,
	repositoryPath string,
) (RunOutcome, error) {
	return service.recoverReview(ctx, runID, repositoryPath, true)
}

func (service *Service) recoverReview(
	ctx context.Context,
	runID string,
	repositoryPath string,
	cancellation bool,
) (RunOutcome, error) {
	if err := ctx.Err(); err != nil {
		return RunOutcome{}, err
	}
	if service.executionDispatch == nil || service.executionDispatch.Spec.RunID != runID {
		return RunOutcome{}, fmt.Errorf("review recovery requires outer dispatch authority for the exact run")
	}
	authorize := func() error { return service.verifyExecutionAuthority(ctx, runID) }
	if cancellation {
		authorize = func() error { return service.verifyExecutionCancellation(ctx, runID) }
	}
	if err := authorize(); err != nil {
		return RunOutcome{}, err
	}
	snapshot, err := service.repository.ExecutionSnapshotForRun(runID)
	if err != nil {
		return RunOutcome{}, err
	}
	configuredDigest, err := reviewconfig.DigestBundleArtifact(service.configBundle)
	if err != nil {
		return RunOutcome{}, err
	}
	if configuredDigest != snapshot.ConfigBundleRef.SHA256 {
		return RunOutcome{}, fmt.Errorf("configured bundle differs from frozen recovery snapshot")
	}
	workflowDigest, err := workflow.DigestDefinition(service.workflow)
	if err != nil {
		return RunOutcome{}, err
	}
	if workflowDigest != snapshot.Workflow.SHA256 || service.buildIdentity != snapshot.BuildIdentity {
		return RunOutcome{}, fmt.Errorf("executable workflow or build differs from frozen recovery snapshot")
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := service.repository.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return RunOutcome{}, err
	}
	if err := service.admitReview(spec, false); err != nil {
		return RunOutcome{}, err
	}
	inputData, err := service.repository.ReadArtifact(snapshot.ReviewInputRef)
	if err != nil {
		return RunOutcome{}, err
	}
	input, err := reviewcore.DecodeReviewInput(inputData)
	if err != nil {
		return RunOutcome{}, err
	}
	var target MaterializedTarget
	if err := service.repository.ReadJSONArtifact(snapshot.TargetSnapshotRef, &target); err != nil {
		return RunOutcome{}, err
	}
	if err := target.Validate(); err != nil {
		return RunOutcome{}, err
	}
	if input.TargetMode == reviewcore.TargetModeScope &&
		(service.scopeShards == nil || snapshot.ReviewShardManifestRef == nil) {
		return RunOutcome{}, fmt.Errorf("scope recovery requires shard execution and a frozen manifest")
	}
	policy, err := runtimePolicyFromBundle(service.configBundle)
	if err != nil {
		return RunOutcome{}, err
	}
	policy.TargetComplete = target.Snapshot.Completeness == gitadapter.CompletenessComplete
	completeness, notes := targetCompleteness(target)
	if snapshot.ReviewShardManifestRef != nil {
		shardCoverage, err := service.scopeShards.ScopeCoverage(ctx, runID, *snapshot.ReviewShardManifestRef)
		if err != nil {
			return RunOutcome{}, err
		}
		completeness, notes = combineShardCompleteness(completeness, notes, shardCoverage)
	}
	recoveredAt, err := service.timestamp()
	if err != nil {
		return RunOutcome{}, err
	}
	recovery, err := service.repository.RecoverReviewWithAuthority(runID, recoveredAt, authorize)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("recover deterministic review ledger: %w", err)
	}
	startedAt := recovery.StartedAt
	run := runmodel.ReviewRun{
		SchemaVersion: runmodel.RunSchemaVersion, RunID: runID, Kind: runmodel.RunKindReview,
		RepositoryPath: repositoryPath, TargetMode: runmodel.TargetMode(target.Snapshot.Mode),
		BaseRevision: target.Snapshot.Base.CommitOID, HeadRevision: target.Snapshot.Head.CommitOID,
		Status:              runmodel.RunStatusRunning,
		ExecutionSnapshotID: recovery.Snapshot.ExecutionSnapshotID,
		TargetSnapshotRef:   recovery.Snapshot.TargetSnapshotRef,
		StageAttempts:       recovery.StageAttempts, Bindings: recovery.Bindings,
		Evidence: recovery.Evidence, CreatedAt: recovery.CreatedAt, StartedAt: &startedAt,
	}
	if cancellation {
		return service.finalizeTerminal(run, runmodel.RunStatusCanceled,
			&runmodel.Failure{Code: "canceled", Message: "outer review job was canceled"}, nil, "")
	}
	if recovery.StartStage == "" {
		for index := range recovery.Upstream {
			if recovery.Upstream[index].Stage == reviewcore.StageReport &&
				recovery.Upstream[index].Output.Report != nil {
				outcome, err := service.finalizeSucceeded(run, *recovery.Upstream[index].Output.Report, nil)
				for _, reused := range recovery.Upstream {
					outcome.Reused = append(outcome.Reused, reused.Stage)
				}
				return outcome, err
			}
		}
		return RunOutcome{}, fmt.Errorf("completed recovered prefix contains no report")
	}
	generationFloor := recovery.MaxGeneration + 1
	if service.executionDispatch.Lease.Generation > generationFloor {
		generationFloor = service.executionDispatch.Lease.Generation
	}
	outcome, err := service.executeAndFinalize(
		ctx, run, input, recovery.Snapshot.ReviewInputRef,
		recovery.Snapshot.ReviewShardManifestRef,
		recovery.StartStage, recovery.Upstream, recovery.UpstreamRefs,
		completeness, notes, policy, service.configBundle.Budget, service.workflow, nil,
		generationFloor,
	)
	for _, reused := range recovery.Upstream {
		outcome.Reused = append(outcome.Reused, reused.Stage)
	}
	return outcome, err
}

func (service *Service) Replay(
	ctx context.Context,
	request ReplayRequest,
) (RunOutcome, error) {
	start, err := parseStage(request.StartStage)
	if err != nil {
		return RunOutcome{}, err
	}
	sourceRun, err := service.repository.LoadRun(request.SourceRunID)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("load source run: %w", err)
	}
	if sourceRun.Status != runmodel.RunStatusSucceeded {
		return RunOutcome{}, fmt.Errorf("source run %q is not successful", sourceRun.RunID)
	}
	sourceRunRef, err := service.repository.CommittedRunRef(sourceRun.RunID)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("load source run artifact: %w", err)
	}
	sourceSnapshot, err := service.repository.LoadExecutionSnapshot(sourceRun.ExecutionSnapshotID)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("load source execution snapshot: %w", err)
	}
	workflowDigest, err := workflow.DigestDefinition(service.workflow)
	if err != nil {
		return RunOutcome{}, err
	}
	sourceConfigData, err := service.repository.ReadArtifact(sourceSnapshot.ConfigBundleRef)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("load source ConfigBundle: %w", err)
	}
	sourceBundle, err := reviewconfig.DecodeBundle(sourceConfigData)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("decode source ConfigBundle: %w", err)
	}
	expectedBundle := service.configBundle
	if service.configProvider != nil {
		expectedBundle = sourceBundle
	} else if service.defaultBundle {
		expectedBundle, err = service.expectedDefaultReplayBundle(sourceBundle)
		if err != nil {
			return RunOutcome{}, fmt.Errorf("validate source ConfigBundle for replay: %w", err)
		}
	}
	if sourceSnapshot.Config.SHA256 != expectedBundle.SHA256 ||
		sourceSnapshot.Workflow.SHA256 != workflowDigest {
		return RunOutcome{}, fmt.Errorf(
			"local M1 replay requires the source config and workflow; variant replay is not enabled",
		)
	}
	executionBundle, executionWorkflow, replayVariable, changedFields, err := service.prepareReplayBundle(
		sourceBundle,
		request,
		start,
	)
	if err != nil {
		return RunOutcome{}, err
	}
	if sourceSnapshot.BuildIdentity != service.buildIdentity {
		return RunOutcome{}, fmt.Errorf(
			"local M1 replay requires source build identity %q; current build is %q",
			sourceSnapshot.BuildIdentity,
			service.buildIdentity,
		)
	}
	inputData, err := service.repository.ReadArtifact(sourceSnapshot.ReviewInputRef)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("load frozen review input: %w", err)
	}
	input, err := reviewcore.DecodeReviewInput(inputData)
	if err != nil {
		return RunOutcome{}, err
	}
	executionInput := input
	executionInputRef := sourceSnapshot.ReviewInputRef
	executionTargetRef := sourceRun.TargetSnapshotRef
	executionReceiptRefs := slices.Clone(sourceSnapshot.ContextProviderReceiptRefs)
	if replayVariable == runmodel.ReplayVariableIndex {
		materialized, materializeErr := service.materializeIndexReplay(
			ctx,
			sourceRun,
			sourceSnapshot,
			sourceBundle,
			executionBundle,
			input,
		)
		if materializeErr != nil {
			return RunOutcome{}, fmt.Errorf("materialize index replay: %w", materializeErr)
		}
		executionInput = materialized.input
		executionInputRef = materialized.inputRef
		executionTargetRef = materialized.targetRef
		executionReceiptRefs = materialized.receiptRefs
	}
	upstream, upstreamRefs, err := service.loadReplayPrefix(
		sourceRun,
		sourceSnapshot,
		start,
	)
	if err != nil {
		return RunOutcome{}, err
	}
	runID, err := service.ids.New("replay")
	if err != nil {
		return RunOutcome{}, err
	}
	sourceSpecData, err := service.repository.ReadArtifact(sourceSnapshot.ReviewSpecRef)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("load source ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(sourceSpecData)
	if err != nil {
		return RunOutcome{}, err
	}
	spec.RequestID = runID
	spec.IdempotencyKey = runID
	configPolicy := bundlePolicyRef(executionBundle)
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(executionBundle)
	if err != nil {
		return RunOutcome{}, err
	}
	spec.ConfigBundleRef = contractsv1alpha1.VersionedRef{
		ID: configPolicy.ID, Revision: configPolicy.Revision, SHA256: configArtifactDigest,
	}
	executionWorkflowDigest, err := workflow.DigestDefinition(executionWorkflow)
	if err != nil {
		return RunOutcome{}, err
	}
	spec.WorkflowRef = contractsv1alpha1.VersionedRef{
		ID: executionWorkflow.ID, Revision: executionWorkflow.Revision,
		SHA256: executionWorkflowDigest,
	}
	if err := service.admitReviewWithWorkflow(spec, executionWorkflow, true); err != nil {
		return RunOutcome{}, err
	}
	rootRunID := sourceRun.RunID
	parentReplayRunID := ""
	if sourceRun.Kind == runmodel.RunKindReplay {
		rootRunID = sourceRun.ReplayRootRunID
		parentReplayRunID = sourceRun.RunID
	}
	changeCreatedAt, err := service.timestamp()
	if err != nil {
		return RunOutcome{}, err
	}
	changeSet := runmodel.ReplayChangeSet{
		SchemaVersion:     runmodel.ReplayChangeSetSchemaVersion,
		Namespace:         runID,
		SourceRunID:       sourceRun.RunID,
		RootRunID:         rootRunID,
		ParentReplayRunID: parentReplayRunID,
		StartStage:        string(start),
		Variable:          replayVariable,
		BaselineSHA256:    sourceBundle.SHA256,
		VariantSHA256:     executionBundle.SHA256,
		ChangedFields:     changedFields,
		RemoteWrites:      "deny",
		CreatedAt:         changeCreatedAt,
	}
	if err := changeSet.Validate(); err != nil {
		return RunOutcome{}, err
	}
	_, snapshot, err := service.persistExecutionSnapshot(
		runID,
		spec,
		executionTargetRef,
		executionInputRef,
		nil,
		upstreamRefs,
		executionReceiptRefs,
		&sourceRunRef,
		&changeSet,
		executionWorkflow,
		executionBundle,
	)
	if err != nil {
		return RunOutcome{}, err
	}
	createdAt, err := service.timestamp()
	if err != nil {
		return RunOutcome{}, err
	}
	startedAt, err := service.timestamp()
	if err != nil {
		return RunOutcome{}, err
	}
	run := runmodel.ReviewRun{
		SchemaVersion:       runmodel.RunSchemaVersion,
		RunID:               runID,
		Kind:                runmodel.RunKindReplay,
		SourceRunID:         sourceRun.RunID,
		ReplayFromStage:     string(start),
		ReplayRootRunID:     rootRunID,
		ReplayNamespace:     runID,
		ReplayVariable:      replayVariable,
		RepositoryPath:      sourceRun.RepositoryPath,
		TargetMode:          sourceRun.TargetMode,
		BaseRevision:        sourceRun.BaseRevision,
		HeadRevision:        sourceRun.HeadRevision,
		Status:              runmodel.RunStatusRunning,
		ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		TargetSnapshotRef:   executionTargetRef,
		StageAttempts:       []runmodel.StageAttempt{},
		Bindings:            []runmodel.PlatformExecutionBinding{},
		Evidence:            []runmodel.RunEvidence{},
		CreatedAt:           createdAt,
		StartedAt:           &startedAt,
	}
	if err := service.appendRunLifecycleEvents(run, createdAt, startedAt); err != nil {
		return RunOutcome{}, err
	}
	workload, err := service.beginRunWorkload(
		ctx,
		run,
		snapshot.ReviewInputRef,
		scheduling.ClassEvalReplay,
	)
	if err != nil {
		status := runmodel.RunStatusFailed
		if errors.Is(err, context.Canceled) {
			status = runmodel.RunStatusCanceled
		}
		return service.finalizeTerminal(run, status, workloadFailure(err), nil, "")
	}
	var target MaterializedTarget
	if err := service.repository.ReadJSONArtifact(executionTargetRef, &target); err != nil {
		return RunOutcome{}, fmt.Errorf("load source materialized target: %w", err)
	}
	if err := target.Validate(); err != nil {
		return RunOutcome{}, fmt.Errorf("validate source materialized target: %w", err)
	}
	completeness, completenessNotes := targetCompleteness(target)
	policy, err := runtimePolicyFromBundle(executionBundle)
	if err != nil {
		return RunOutcome{}, err
	}
	policy.TargetComplete =
		target.Snapshot.Completeness == gitadapter.CompletenessComplete
	outcome, executeErr := service.executeAndFinalize(
		ctx,
		run,
		executionInput,
		executionInputRef,
		nil,
		start,
		upstream,
		upstreamRefs,
		completeness,
		completenessNotes,
		policy,
		executionBundle.Budget,
		executionWorkflow,
		workload,
		1,
	)
	outcome.Reused = replayPrefix(start)
	return outcome, executeErr
}

func (service *Service) Compare(
	ctx context.Context,
	baselineRunID string,
	variantRunID string,
) (runmodel.RunComparison, error) {
	now, err := service.timestamp()
	if err != nil {
		return runmodel.RunComparison{}, err
	}
	return CompareRuns(ctx, service.repository, baselineRunID, variantRunID, now)
}

func (service *Service) History(limit int) ([]runrepo.HistoryEntry, error) {
	return service.repository.History(limit)
}

func (service *Service) buildReviewSpec(
	runID string,
	materialized Materialization,
	bundle reviewconfig.ConfigBundle,
) (contractsv1alpha1.ReviewSpec, error) {
	configPolicy := bundlePolicyRef(bundle)
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(bundle)
	if err != nil {
		return contractsv1alpha1.ReviewSpec{}, err
	}
	workflowDigest, err := workflow.DigestDefinition(service.workflow)
	if err != nil {
		return contractsv1alpha1.ReviewSpec{}, err
	}
	target := contractsv1alpha1.ReviewTarget{
		Mode: contractsv1alpha1.ReviewMode(materialized.Target.Snapshot.Mode),
	}
	switch materialized.Target.Snapshot.Mode {
	case reviewcore.TargetModeDiff:
		target.Diff = &contractsv1alpha1.DiffTarget{
			BaseRevision: materialized.Target.Snapshot.Base.CommitOID,
			HeadRevision: materialized.Target.Snapshot.Head.CommitOID,
			Patch: contractsv1alpha1.ContentRef{
				URI:       materialized.Target.PatchRef.URI,
				SHA256:    materialized.Target.PatchRef.SHA256,
				SizeBytes: materialized.Target.PatchRef.SizeBytes,
			},
		}
	case reviewcore.TargetModeSelection:
		selection := materialized.Target.Snapshot.Selection
		target.Selection = &contractsv1alpha1.SelectionTarget{
			Revision: materialized.Target.Snapshot.Head.CommitOID,
			Path:     selection.Path,
			Content: contractsv1alpha1.ContentRef{
				URI:       materialized.Target.SelectionContentRef.URI,
				SHA256:    materialized.Target.SelectionContentRef.SHA256,
				SizeBytes: materialized.Target.SelectionContentRef.SizeBytes,
			},
		}
		switch {
		case selection.StartLine != 0:
			target.Selection.StartLine = selection.StartLine
			target.Selection.EndLine = selection.EndLine
		case selection.Symbol != nil:
			target.Selection.Symbol = &contractsv1alpha1.SymbolSelector{
				Language:      selection.Symbol.Language,
				Kind:          selection.Symbol.Kind,
				QualifiedName: selection.Symbol.QualifiedName,
			}
		default:
			target.Selection.Ranges = make(
				[]contractsv1alpha1.SelectionRange,
				0,
				len(selection.EffectiveRanges),
			)
			for _, lineRange := range selection.EffectiveRanges {
				target.Selection.Ranges = append(
					target.Selection.Ranges,
					contractsv1alpha1.SelectionRange{
						StartLine: lineRange.StartLine,
						EndLine:   lineRange.EndLine,
					},
				)
			}
		}
	case reviewcore.TargetModeScope:
		target.Scope = &contractsv1alpha1.ScopeTarget{
			Revision: materialized.Target.Snapshot.Head.CommitOID,
			Include:  slices.Clone(materialized.Target.Snapshot.Scope.Include),
			Exclude:  slices.Clone(materialized.Target.Snapshot.Scope.Exclude),
		}
	default:
		return contractsv1alpha1.ReviewSpec{}, fmt.Errorf(
			"unsupported materialized target mode %q",
			materialized.Target.Snapshot.Mode,
		)
	}
	spec := contractsv1alpha1.ReviewSpec{
		SchemaVersion:  contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:      runID,
		IdempotencyKey: runID,
		TenantID:       "local",
		WorkspaceID:    "local",
		Repository: contractsv1alpha1.RepositoryRef{
			Provider:     "local-git",
			RepositoryID: materialized.Target.Snapshot.Repository.RepositoryID,
		},
		Target: target,
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID:       configPolicy.ID,
			Revision: configPolicy.Revision,
			SHA256:   configArtifactDigest,
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: service.workflow.ID, Revision: service.workflow.Revision, SHA256: workflowDigest,
		},
		RequestedOutput: []string{"findings", "report", "trace"},
		RemoteWrites:    "deny",
	}
	if err := spec.Validate(); err != nil {
		return contractsv1alpha1.ReviewSpec{}, err
	}
	return spec, nil
}

func (service *Service) persistExecutionSnapshot(
	runID string,
	spec contractsv1alpha1.ReviewSpec,
	targetRef runmodel.ArtifactRef,
	inputRef runmodel.ArtifactRef,
	reviewShardManifestRef *runmodel.ArtifactRef,
	replayInputRefs []runmodel.ArtifactRef,
	contextProviderReceiptRefs []runmodel.ArtifactRef,
	replaySourceRunRef *runmodel.ArtifactRef,
	replayChangeSet *runmodel.ReplayChangeSet,
	definition workflow.Definition,
	bundle reviewconfig.ConfigBundle,
) (runmodel.ArtifactRef, runmodel.ExecutionSnapshot, error) {
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	specRef, err := service.repository.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	if specRef.SHA256 != specDigest {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{},
			fmt.Errorf("persisted ReviewSpec digest changed")
	}
	configPolicy := bundlePolicyRef(bundle)
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(bundle)
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	workflowDigest, err := workflow.DigestDefinition(definition)
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	workflowRef, err := service.repository.PutJSONArtifact(
		runmodel.ContractWorkflowDefinition,
		definition,
	)
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	if workflowRef.SHA256 != workflowDigest {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{},
			fmt.Errorf("persisted workflow definition digest changed")
	}
	configRef, err := service.repository.PutJSONArtifact(
		runmodel.ContractConfigBundle,
		bundle,
	)
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	if configRef.SHA256 != configArtifactDigest {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{},
			fmt.Errorf("persisted config bundle digest changed")
	}
	snapshotID, err := service.ids.New("snapshot")
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	now, err := service.timestamp()
	if err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	var frozenReplaySource *runmodel.ArtifactRef
	if replaySourceRunRef != nil {
		copy := *replaySourceRunRef
		frozenReplaySource = &copy
	}
	var replayChangeSetRef *runmodel.ArtifactRef
	if replayChangeSet != nil {
		if err := replayChangeSet.Validate(); err != nil {
			return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
		}
		ref, err := service.repository.PutJSONArtifact(
			runmodel.ContractReplayChangeSet,
			*replayChangeSet,
		)
		if err != nil {
			return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
		}
		replayChangeSetRef = &ref
	}
	snapshot := runmodel.ExecutionSnapshot{
		SchemaVersion:              runmodel.SnapshotSchemaVersion,
		ExecutionSnapshotID:        snapshotID,
		ReviewSpecSHA256:           specDigest,
		ReviewSpecRef:              specRef,
		TargetSnapshotRef:          targetRef,
		ReviewInputRef:             inputRef,
		ReviewShardManifestRef:     reviewShardManifestRef,
		ReplayInputRefs:            slices.Clone(replayInputRefs),
		ContextProviderReceiptRefs: slices.Clone(contextProviderReceiptRefs),
		ReplaySourceRunRef:         frozenReplaySource,
		ReplayChangeSetRef:         replayChangeSetRef,
		WorkflowDefinitionRef:      workflowRef,
		ConfigBundleRef:            configRef,
		Workflow: runmodel.WorkflowRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
		},
		Config: runmodel.PolicyRef{
			ID: configPolicy.ID, Revision: configPolicy.Revision, SHA256: configPolicy.SHA256,
		},
		RuntimeProfile: bundle.Execution.AgentProfile.ID + "@" +
			bundle.Execution.AgentProfile.Revision,
		BuildIdentity: service.buildIdentity,
		ToolPolicy: runmodel.ToolInvocationPolicy{
			AllowedTools:       slices.Clone(bundle.Execution.AllowedTools),
			Network:            "deny",
			WorkspaceWrites:    "deny",
			RemoteWrites:       "deny",
			PerCallTimeoutMS:   bundle.Budget.StageTimeoutMS,
			MaxOutputBytes:     bundle.Budget.MaxOutputBytes,
			MaxConcurrency:     bundle.Budget.MaxConcurrency,
			MaxDelegationDepth: 0,
		},
		RemoteWrites: "deny",
		CreatedAt:    now,
	}
	if err := service.repository.SaveExecutionSnapshot(snapshot); err != nil {
		return runmodel.ArtifactRef{}, runmodel.ExecutionSnapshot{}, err
	}
	return specRef, snapshot, nil
}

func (service *Service) appendRunLifecycleEvents(
	run runmodel.ReviewRun,
	createdAt time.Time,
	startedAt time.Time,
) error {
	if err := service.verifyExecutionAuthority(context.Background(), run.RunID); err != nil {
		return err
	}
	if err := service.repository.AppendEvent(run.RunID+"-created", createdAt, runrepo.RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusPending,
		EventType: runrepo.EventRunCreated, ExecutionSnapshotID: run.ExecutionSnapshotID,
	}); err != nil {
		return err
	}
	if err := service.verifyExecutionAuthority(context.Background(), run.RunID); err != nil {
		return err
	}
	return service.repository.AppendEvent(run.RunID+"-started", startedAt, runrepo.RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: runrepo.EventRunStarted, ExecutionSnapshotID: run.ExecutionSnapshotID,
	})
}

func (service *Service) executeAndFinalize(
	ctx context.Context,
	run runmodel.ReviewRun,
	input reviewcore.ReviewInput,
	inputRef runmodel.ArtifactRef,
	shardManifestRef *runmodel.ArtifactRef,
	start reviewcore.StageName,
	upstream []reviewcore.StageResult,
	upstreamRefs []runmodel.ArtifactRef,
	completeness runmodel.Completeness,
	completenessNotes []string,
	policy reviewcore.RuntimePolicy,
	runtimeBudget reviewconfig.BudgetPolicy,
	definition workflow.Definition,
	workload *runWorkload,
	generationFloor int,
) (RunOutcome, error) {
	stages := supportedStages()
	startIndex := slices.Index(stages, start)
	if startIndex < 0 {
		return RunOutcome{}, fmt.Errorf("unsupported start stage %q", start)
	}
	if len(upstream) != startIndex || len(upstreamRefs) != startIndex {
		return RunOutcome{}, fmt.Errorf("start stage %q has incomplete upstream artifacts", start)
	}
	if generationFloor < 1 {
		return RunOutcome{}, fmt.Errorf("generation floor must be positive")
	}
	var prior reviewcore.StageResult
	var priorRef *runmodel.ArtifactRef
	if len(upstream) > 0 {
		prior = upstream[len(upstream)-1]
		ref := upstreamRefs[len(upstreamRefs)-1]
		priorRef = &ref
	}
	var completedReport *reviewcore.Report
	for index := range upstream {
		if upstream[index].Stage == reviewcore.StageReport &&
			upstream[index].Output.Report != nil {
			copy := *upstream[index].Output.Report
			completedReport = &copy
		}
	}

	for stageIndex := startIndex; stageIndex < len(stages); stageIndex++ {
		stage := stages[stageIndex]
		stageDefinition, err := workflowStage(definition, stage)
		if err != nil {
			return RunOutcome{}, err
		}
		maxAttempts := min(runtimeBudget.MaxAttempts, stageDefinition.Retry.MaxAttempts)
		stageTimeoutMS := min(runtimeBudget.StageTimeoutMS, stageDefinition.Budget.TimeoutMS)
		maxInputBytes := min(
			runtimeBudget.MaxInputBytes,
			stageDefinition.Budget.MaxInputBytes,
		)
		maxOutputBytes := min(
			runtimeBudget.MaxOutputBytes,
			stageDefinition.Budget.MaxOutputBytes,
		)
		maxConcurrency := min(
			runtimeBudget.MaxConcurrency,
			stageDefinition.Budget.MaxConcurrency,
		)
		var stageSucceeded bool
		for attemptNumber := 1; attemptNumber <= maxAttempts; attemptNumber++ {
			if err := ctx.Err(); err != nil {
				return service.finishCanceled(run, stage, err, workload)
			}
			if err := service.heartbeatRunWorkload(
				ctx,
				workload,
				stage,
				attemptNumber,
				"start",
			); err != nil {
				return service.finishCanceled(
					run,
					stage,
					fmt.Errorf("verify run workload lease before stage: %w", err),
					workload,
				)
			}
			generation := generationFloor + attemptNumber - 1
			bindingID, err := service.ids.New("binding")
			if err != nil {
				return RunOutcome{}, err
			}
			idempotencyKey := fmt.Sprintf(
				"%s-%s-%d-%d", run.RunID, stage, attemptNumber, generation,
			)
			now, err := service.timestamp()
			if err != nil {
				return RunOutcome{}, err
			}
			binding := runmodel.PlatformExecutionBinding{
				BindingID: bindingID, RunID: run.RunID, StageID: string(stage),
				Attempt: attemptNumber, Generation: generation,
				IdempotencyKey: idempotencyKey,
				FencingToken:   uint64(len(run.Bindings) + 1),
				RuntimeKind:    service.executorKind,
				RuntimeID:      service.executorID + "-" + run.RunID,
				CreatedAt:      now,
			}
			if workload != nil {
				binding.FencingToken = workload.dispatch.Lease.FencingToken
				binding.RuntimeKind = "local-run-coordinator"
				binding.RuntimeID = workload.dispatch.Lease.WorkerID + "-" +
					workload.dispatch.Lease.LeaseID
			} else if service.executionDispatch != nil {
				binding.FencingToken = service.executionDispatch.Lease.FencingToken
				binding.RuntimeKind = "review-job-coordinator"
				binding.RuntimeID = service.executionDispatch.Lease.WorkerID + "-" +
					service.executionDispatch.Lease.LeaseID
			}
			run.Bindings = append(run.Bindings, binding)
			if err := service.appendStageFact(
				run, stage, attemptNumber, generation, "binding", now,
				runrepo.EventBindingRecorded, nil, &binding, nil, nil,
			); err != nil {
				return RunOutcome{}, err
			}
			if err := service.appendStageFact(
				run, stage, attemptNumber, generation, "started", now,
				runrepo.EventStageStarted, nil, nil, nil, nil,
			); err != nil {
				return RunOutcome{}, err
			}

			inputRefs := []runmodel.ArtifactRef{inputRef}
			if priorRef != nil {
				inputRefs = append(inputRefs, *priorRef)
			}
			attempt := runmodel.StageAttempt{
				StageID: string(stage), Attempt: attemptNumber, Generation: generation,
				BindingID: bindingID, Status: runmodel.StageStatusRunning,
				InputRefs: inputRefs, StartedAt: now,
			}
			var result reviewcore.StageResult
			var outputData []byte
			var executeErr error
			errorCode := ""
			workloadLeaseInvalid := false
			inputBytes, sizeErr := artifactRefsSize(inputRefs)
			if sizeErr != nil {
				executeErr = sizeErr
				errorCode = "stage_input_budget_invalid"
			} else if inputBytes > maxInputBytes {
				executeErr = fmt.Errorf(
					"stage %q input artifacts total %d bytes, limit %d",
					stage,
					inputBytes,
					maxInputBytes,
				)
				errorCode = "stage_input_budget_exceeded"
			} else {
				stageContext, cancelStage := context.WithTimeout(
					ctx,
					time.Duration(stageTimeoutMS)*time.Millisecond,
				)
				if stage == reviewcore.StageDetect &&
					input.TargetMode == reviewcore.TargetModeScope &&
					service.scopeShards != nil && shardManifestRef != nil {
					result, executeErr = service.scopeShards.DetectScope(
						stageContext,
						ScopeShardDetectRequest{
							RunID: run.RunID, Input: input, InputRef: inputRef,
							ManifestRef: *shardManifestRef, Upstream: prior, Policy: policy,
							MaxInputBytes: maxInputBytes, MaxOutputBytes: maxOutputBytes,
							MaxConcurrency: maxConcurrency,
						},
					)
				} else {
					result, executeErr = service.executeStageForPolicy(
						stageContext,
						input,
						stage,
						prior,
						policy,
					)
				}
				stageContextErr := stageContext.Err()
				cancelStage()
				if executeErr == nil && stageContextErr != nil {
					executeErr = stageContextErr
				}
			}
			if executeErr == nil {
				if err := result.Validate(); err != nil {
					executeErr = fmt.Errorf("validate stage output: %w", err)
					errorCode = "invalid_stage_output"
				} else {
					outputData, executeErr = json.Marshal(result)
					if executeErr != nil {
						executeErr = fmt.Errorf("marshal stage output: %w", executeErr)
						errorCode = "invalid_stage_output"
					} else if int64(len(outputData)) > maxOutputBytes {
						executeErr = fmt.Errorf(
							"stage %q output is %d bytes, limit %d",
							stage,
							len(outputData),
							maxOutputBytes,
						)
						errorCode = "stage_output_budget_exceeded"
					}
				}
			}
			// Revalidate the run lease after every attempt, including failed
			// and timed-out attempts. A cancel or reassignment racing with the
			// executor must win before retry or terminal callback scheduling.
			if ctx.Err() == nil {
				if err := service.heartbeatRunWorkload(
					ctx,
					workload,
					stage,
					attemptNumber,
					"finish",
				); err != nil {
					executeErr = fmt.Errorf(
						"verify run workload lease after stage: %w",
						err,
					)
					errorCode = "scheduling_lease_fenced"
					if !errors.Is(err, scheduling.ErrFenced) {
						errorCode = "scheduling_heartbeat_failed"
					}
					workloadLeaseInvalid = true
				}
			}
			finishedAt, timeErr := service.timestamp()
			if timeErr != nil {
				return RunOutcome{}, timeErr
			}
			attempt.FinishedAt = &finishedAt
			attempt.DurationMS = max(finishedAt.Sub(now).Milliseconds(), 0)
			if executeErr != nil {
				timedOut := errors.Is(executeErr, context.DeadlineExceeded) &&
					ctx.Err() == nil
				canceled := ctx.Err() != nil ||
					errors.Is(executeErr, context.Canceled) && !timedOut ||
					workloadLeaseInvalid
				attempt.Status = runmodel.StageStatusFailed
				eventType := runrepo.EventStageFailed
				code := errorCode
				if code == "" {
					code = stageErrorCode(executeErr)
				}
				if timedOut {
					code = "stage_timeout"
				} else if canceled {
					attempt.Status = runmodel.StageStatusCanceled
					eventType = runrepo.EventStageCanceled
					if !workloadLeaseInvalid {
						code = "canceled"
					}
				}
				attempt.ErrorCode = code
				attempt.ErrorMessage = executeErr.Error()
				attempt.Retryable = !canceled &&
					slices.Contains(stageDefinition.Retry.RetryableCodes, code)
				run.StageAttempts = append(run.StageAttempts, attempt)
				failure := &runmodel.Failure{
					Code: code, Message: executeErr.Error(), StageID: string(stage),
					Retryable: attempt.Retryable,
				}
				if err := service.appendStageFact(
					run, stage, attemptNumber, generation, string(attempt.Status),
					finishedAt, eventType, nil, nil, nil, failure,
				); err != nil {
					return RunOutcome{}, err
				}
				if canceled {
					if input.TargetMode == reviewcore.TargetModeScope &&
						service.scopeShards != nil && shardManifestRef != nil {
						if err := service.scopeShards.CancelScope(
							context.WithoutCancel(ctx), run.RunID, executeErr.Error(), finishedAt,
						); err != nil {
							return RunOutcome{}, fmt.Errorf("cancel durable scope shard plan: %w", err)
						}
					}
					return service.finalizeScheduledTerminal(
						run,
						runmodel.RunStatusCanceled,
						failure,
						nil,
						"",
						workload,
					)
				}
				if attempt.Retryable && attemptNumber < maxAttempts {
					if err := waitRetryBackoff(
						ctx,
						time.Duration(stageDefinition.Retry.BackoffMS)*time.Millisecond,
					); err != nil {
						return service.finishCanceled(run, stage, err, workload)
					}
					continue
				}
				return service.finalizeScheduledTerminal(
					run,
					runmodel.RunStatusFailed,
					failure,
					nil,
					"",
					workload,
				)
			}

			outputRef, err := service.repository.PutArtifact(
				runmodel.ContractStageResult,
				outputData,
			)
			if err != nil {
				return RunOutcome{}, err
			}
			attempt.Status = runmodel.StageStatusSucceeded
			attempt.OutputRef = &outputRef
			run.StageAttempts = append(run.StageAttempts, attempt)
			if err := service.appendStageFact(
				run, stage, attemptNumber, generation, "succeeded", finishedAt,
				runrepo.EventStageSucceeded, &outputRef, nil, nil, nil,
			); err != nil {
				return RunOutcome{}, err
			}
			evidenceID, err := service.ids.New("evidence")
			if err != nil {
				return RunOutcome{}, err
			}
			evidence := runmodel.RunEvidence{
				EvidenceID: evidenceID, BindingID: binding.BindingID,
				StageID: string(stage), Attempt: attemptNumber, Generation: generation,
				IdempotencyKey: binding.IdempotencyKey, FencingToken: binding.FencingToken,
				ArtifactRef: outputRef, Completeness: completeness,
				CompletenessNotes: slices.Clone(completenessNotes),
				RecordedAt:        finishedAt,
			}
			run.Evidence = append(run.Evidence, evidence)
			if err := service.appendStageFact(
				run, stage, attemptNumber, generation, "evidence", finishedAt,
				runrepo.EventEvidenceRecorded, &outputRef, nil, &evidence, nil,
			); err != nil {
				return RunOutcome{}, err
			}
			prior = result
			if result.Stage == reviewcore.StageReport && result.Output.Report != nil {
				copy := *result.Output.Report
				completedReport = &copy
			}
			priorRef = &outputRef
			stageSucceeded = true
			break
		}
		if !stageSucceeded {
			return RunOutcome{}, fmt.Errorf("stage %q exhausted without a terminal run", stage)
		}
	}
	if completedReport == nil {
		return RunOutcome{}, fmt.Errorf("report stage produced no report")
	}
	return service.finalizeSucceeded(run, *completedReport, workload)
}

func (service *Service) finalizeSucceeded(
	run runmodel.ReviewRun,
	report reviewcore.Report,
	workload *runWorkload,
) (RunOutcome, error) {
	findingSet := FindingSet{
		SchemaVersion: FindingSetSchemaVersion,
		TargetDigest:  report.TargetDigest,
		Findings:      slices.Clone(report.Findings),
		Decisions:     slices.Clone(report.Decisions),
	}
	findingRef, err := service.repository.PutJSONArtifact(runmodel.ContractFindingSet, findingSet)
	if err != nil {
		return RunOutcome{}, err
	}
	reportJSON, err := reviewcore.MarshalReportJSON(report)
	if err != nil {
		return RunOutcome{}, err
	}
	jsonRef, err := service.repository.PutArtifact(runmodel.ContractJSONReport, reportJSON)
	if err != nil {
		return RunOutcome{}, err
	}
	markdown, err := reviewcore.RenderReportMarkdown(context.Background(), report)
	if err != nil {
		return RunOutcome{}, err
	}
	markdownRef, err := service.repository.PutArtifact(
		runmodel.ContractMarkdownReport,
		[]byte(markdown),
	)
	if err != nil {
		return RunOutcome{}, err
	}
	run.FindingSetRef = &findingRef
	run.JSONReportRef = &jsonRef
	run.MarkdownReportRef = &markdownRef
	outcome, err := service.finalizeScheduledTerminal(
		run,
		runmodel.RunStatusSucceeded,
		nil,
		&report,
		markdown,
		workload,
	)
	return outcome, err
}

func (service *Service) finishCanceled(
	run runmodel.ReviewRun,
	stage reviewcore.StageName,
	err error,
	workload *runWorkload,
) (RunOutcome, error) {
	code := "canceled"
	if errors.Is(err, scheduling.ErrFenced) {
		code = "scheduling_lease_fenced"
	}
	failure := &runmodel.Failure{
		Code: code, Message: err.Error(), StageID: string(stage), Retryable: false,
	}
	return service.finalizeScheduledTerminal(
		run,
		runmodel.RunStatusCanceled,
		failure,
		nil,
		"",
		workload,
	)
}

func (service *Service) finalizeTerminal(
	run runmodel.ReviewRun,
	status runmodel.RunStatus,
	failure *runmodel.Failure,
	report *reviewcore.Report,
	markdown string,
) (RunOutcome, error) {
	if err := service.verifyExecutionWriteAuthority(context.Background(), run.RunID, status == runmodel.RunStatusCanceled); err != nil {
		return RunOutcome{}, err
	}
	completedAt, err := service.timestamp()
	if err != nil {
		return RunOutcome{}, err
	}
	run.Status = status
	run.Failure = failure
	run.CompletedAt = &completedAt
	finalRef, err := service.repository.FinalizeRun(
		runrepo.TerminalEventID(run.RunID),
		completedAt,
		run,
	)
	outcome := RunOutcome{
		Run: run, FinalRef: finalRef, Report: report, Markdown: markdown,
	}
	if err != nil {
		return outcome, err
	}
	if status == runmodel.RunStatusSucceeded {
		return outcome, nil
	}
	if failure == nil {
		failure = &runmodel.Failure{Code: string(status), Message: string(status)}
	}
	return outcome, &RunError{RunID: run.RunID, Failure: *failure}
}

func (service *Service) appendStageFact(
	run runmodel.ReviewRun,
	stage reviewcore.StageName,
	attempt int,
	generation int,
	suffix string,
	eventTime time.Time,
	eventType string,
	artifact *runmodel.ArtifactRef,
	binding *runmodel.PlatformExecutionBinding,
	evidence *runmodel.RunEvidence,
	failure *runmodel.Failure,
) error {
	if err := service.verifyExecutionWriteAuthority(context.Background(), run.RunID, eventType == runrepo.EventStageCanceled); err != nil {
		return err
	}
	eventID := fmt.Sprintf(
		"%s-%s-%d-%d-%s", run.RunID, stage, attempt, generation, suffix,
	)
	return service.repository.AppendEvent(eventID, eventTime, runrepo.RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: eventType, ExecutionSnapshotID: run.ExecutionSnapshotID,
		StageID: string(stage), Attempt: attempt, Generation: generation,
		Artifact: artifact, Binding: binding, Evidence: evidence, Failure: failure,
	})
}

func (service *Service) verifyExecutionAuthority(ctx context.Context, runID string) error {
	return service.verifyExecutionWriteAuthority(ctx, runID, false)
}

func (service *Service) verifyExecutionWriteAuthority(ctx context.Context, runID string, cancellation bool) error {
	if service.executionDispatch == nil {
		return nil
	}
	if service.executionDispatch.Spec.RunID != runID {
		return fmt.Errorf("execution dispatch does not authorize run %q", runID)
	}
	if service.executionAuthority == nil {
		return nil
	}
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := service.executionAuthority(checkContext, *service.executionDispatch); err != nil {
		if cancellation && service.executionCancellationAuthority != nil {
			if cancelErr := service.executionCancellationAuthority(checkContext, *service.executionDispatch); cancelErr == nil {
				return nil
			}
		}
		return fmt.Errorf("verify outer execution authority: %w", err)
	}
	return nil
}

func (service *Service) verifyExecutionCancellation(ctx context.Context, runID string) error {
	if service.executionDispatch == nil || service.executionDispatch.Spec.RunID != runID ||
		service.executionCancellationAuthority == nil {
		return fmt.Errorf("cancellation recovery requires exact canceled lease authority")
	}
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := service.executionCancellationAuthority(checkContext, *service.executionDispatch); err != nil {
		return fmt.Errorf("verify canceled execution authority: %w", err)
	}
	return nil
}

func (service *Service) loadReplayPrefix(
	source runmodel.ReviewRun,
	sourceSnapshot runmodel.ExecutionSnapshot,
	start reviewcore.StageName,
) ([]reviewcore.StageResult, []runmodel.ArtifactRef, error) {
	prefix := replayPrefix(start)
	authoritative := make(map[reviewcore.StageName]runmodel.ArtifactRef)
	resultsByRef := make(map[runmodel.ArtifactRef]reviewcore.StageResult)
	for index, ref := range sourceSnapshot.ReplayInputRefs {
		data, err := service.repository.ReadArtifact(ref)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"read source replay checkpoint %d: %w",
				index,
				err,
			)
		}
		result, err := reviewcore.DecodeStageResult(data)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decode source replay checkpoint %d: %w",
				index,
				err,
			)
		}
		if _, duplicate := authoritative[result.Stage]; duplicate {
			return nil, nil, fmt.Errorf(
				"source replay lineage contains duplicate %s checkpoint",
				result.Stage,
			)
		}
		authoritative[result.Stage] = ref
		resultsByRef[ref] = result
	}
	latestAttempts := make(map[string]runmodel.StageAttempt)
	for _, attempt := range source.StageAttempts {
		latest, exists := latestAttempts[attempt.StageID]
		if !exists || attempt.Generation > latest.Generation ||
			attempt.Generation == latest.Generation && attempt.Attempt > latest.Attempt {
			latestAttempts[attempt.StageID] = attempt
		}
	}
	for stageID, attempt := range latestAttempts {
		if attempt.Status != runmodel.StageStatusSucceeded || attempt.OutputRef == nil {
			continue
		}
		data, err := service.repository.ReadArtifact(*attempt.OutputRef)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"read source %s checkpoint: %w",
				stageID,
				err,
			)
		}
		result, err := reviewcore.DecodeStageResult(data)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decode source %s checkpoint: %w",
				stageID,
				err,
			)
		}
		if string(result.Stage) != stageID {
			return nil, nil, fmt.Errorf(
				"source checkpoint contract says %q, want %q",
				result.Stage,
				stageID,
			)
		}
		if _, inherited := authoritative[result.Stage]; inherited {
			return nil, nil, fmt.Errorf(
				"source replay lineage both reuses and executes stage %s",
				result.Stage,
			)
		}
		authoritative[result.Stage] = *attempt.OutputRef
		resultsByRef[*attempt.OutputRef] = result
	}
	results := make([]reviewcore.StageResult, 0, len(prefix))
	refs := make([]runmodel.ArtifactRef, 0, len(prefix))
	seenRefs := make(map[runmodel.ArtifactRef]struct{}, len(prefix))
	for _, stage := range prefix {
		ref, exists := authoritative[stage]
		if !exists {
			return nil, nil, fmt.Errorf(
				"source run %q has no successful %s checkpoint", source.RunID, stage,
			)
		}
		result := resultsByRef[ref]
		if result.Stage != stage {
			return nil, nil, fmt.Errorf("checkpoint contract says %q, want %q", result.Stage, stage)
		}
		if _, duplicate := seenRefs[ref]; duplicate {
			return nil, nil, fmt.Errorf(
				"source replay prefix aliases the %s checkpoint",
				stage,
			)
		}
		seenRefs[ref] = struct{}{}
		results = append(results, result)
		refs = append(refs, ref)
	}
	return results, refs, nil
}

func (service *Service) timestamp() (time.Time, error) {
	service.clockMu.Lock()
	defer service.clockMu.Unlock()
	value := service.now().UTC()
	if value.IsZero() {
		return time.Time{}, fmt.Errorf("service clock returned zero time")
	}
	return value, nil
}

func (service *Service) admitReview(
	spec contractsv1alpha1.ReviewSpec,
	replay bool,
) error {
	return service.admitReviewWithWorkflow(spec, service.workflow, replay)
}

func (service *Service) admitReviewWithWorkflow(
	spec contractsv1alpha1.ReviewSpec,
	definition workflow.Definition,
	replay bool,
) error {
	digest, err := workflow.DigestDefinition(definition)
	if err != nil {
		return err
	}
	if err := workflow.AdmitReview(spec, definition, workflow.AdmissionPolicy{
		Replay:           replay,
		DefinitionSHA256: digest,
		Executors:        cloneExecutorCapabilities(service.executorCapabilities),
	}); err != nil {
		return fmt.Errorf("admit review execution: %w", err)
	}
	return nil
}

func workflowStage(
	definition workflow.Definition,
	stage reviewcore.StageName,
) (workflow.Stage, error) {
	for _, candidate := range definition.Stages {
		if candidate.ID == string(stage) {
			return candidate, nil
		}
	}
	return workflow.Stage{}, fmt.Errorf("workflow has no stage %q", stage)
}

func cloneExecutorCapabilities(
	input map[string]workflow.ExecutorCapabilities,
) map[string]workflow.ExecutorCapabilities {
	if input == nil {
		return nil
	}
	output := make(map[string]workflow.ExecutorCapabilities, len(input))
	for executor, capabilities := range input {
		output[executor] = capabilities
	}
	return output
}

func validateLocalExecutorCapabilities(
	definition workflow.Definition,
	capabilities map[string]workflow.ExecutorCapabilities,
) error {
	if capabilities == nil {
		return fmt.Errorf("executor capability snapshots are required")
	}
	for _, stage := range definition.Stages {
		effective, exists := capabilities[stage.Executor]
		if !exists {
			return fmt.Errorf(
				"workflow stage %q executor %q has no capability snapshot",
				stage.ID,
				stage.Executor,
			)
		}
		if effective.WorkspaceWrite || effective.RemoteWrite ||
			effective.UnrestrictedNetwork || effective.ProjectConfigLoad ||
			effective.BackgroundExecution {
			return fmt.Errorf(
				"workflow stage %q executor %q exceeds local review-safe authority",
				stage.ID,
				stage.Executor,
			)
		}
	}
	return nil
}

func validateRuntimeEnvelope(
	bundle reviewconfig.ConfigBundle,
	definition workflow.Definition,
) error {
	if err := bundle.Validate(); err != nil {
		return fmt.Errorf("validate runtime ConfigBundle: %w", err)
	}
	digest, err := workflow.DigestDefinition(definition)
	if err != nil {
		return err
	}
	if bundle.Workflow.Definition.ID != definition.ID ||
		bundle.Workflow.Definition.Revision != definition.Revision ||
		bundle.Workflow.Definition.SHA256 != digest {
		return fmt.Errorf("ConfigBundle workflow does not match executable workflow")
	}
	for _, stage := range definition.Stages {
		switch {
		case bundle.Budget.StageTimeoutMS > stage.Budget.TimeoutMS:
			return fmt.Errorf(
				"ConfigBundle stage timeout %d exceeds workflow stage %q limit %d",
				bundle.Budget.StageTimeoutMS,
				stage.ID,
				stage.Budget.TimeoutMS,
			)
		case bundle.Budget.MaxInputBytes > stage.Budget.MaxInputBytes:
			return fmt.Errorf(
				"ConfigBundle input budget %d exceeds workflow stage %q limit %d",
				bundle.Budget.MaxInputBytes,
				stage.ID,
				stage.Budget.MaxInputBytes,
			)
		case bundle.Budget.MaxOutputBytes > stage.Budget.MaxOutputBytes:
			return fmt.Errorf(
				"ConfigBundle output budget %d exceeds workflow stage %q limit %d",
				bundle.Budget.MaxOutputBytes,
				stage.ID,
				stage.Budget.MaxOutputBytes,
			)
		case bundle.Budget.MaxConcurrency > stage.Budget.MaxConcurrency:
			return fmt.Errorf(
				"ConfigBundle concurrency %d exceeds workflow stage %q limit %d",
				bundle.Budget.MaxConcurrency,
				stage.ID,
				stage.Budget.MaxConcurrency,
			)
		case bundle.Budget.MaxAttempts > stage.Retry.MaxAttempts:
			return fmt.Errorf(
				"ConfigBundle attempts %d exceeds workflow stage %q limit %d",
				bundle.Budget.MaxAttempts,
				stage.ID,
				stage.Retry.MaxAttempts,
			)
		}
	}
	return nil
}

func artifactRefsSize(refs []runmodel.ArtifactRef) (int64, error) {
	var total int64
	for index, ref := range refs {
		if err := ref.Validate(); err != nil {
			return 0, fmt.Errorf("input artifact %d: %w", index, err)
		}
		if ref.SizeBytes > int64(^uint64(0)>>1)-total {
			return 0, fmt.Errorf("input artifact sizes overflow int64")
		}
		total += ref.SizeBytes
	}
	return total, nil
}

func stageErrorCode(err error) string {
	var retryable *RetryableStageError
	if errors.As(err, &retryable) && strings.TrimSpace(retryable.Code) != "" {
		return retryable.Code
	}
	return "stage_execution_failed"
}

func waitRetryBackoff(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateSupportedWorkflow(definition workflow.Definition) error {
	order, err := definition.TopologicalOrder()
	if err != nil {
		return err
	}
	want := supportedStages()
	if len(order) != len(want) {
		return fmt.Errorf("local runtime requires the complete deterministic review workflow")
	}
	for index := range want {
		if order[index] != string(want[index]) {
			return fmt.Errorf("local runtime workflow stage %d is %q, want %q",
				index, order[index], want[index])
		}
		stage := definition.Stages[index]
		expectedDependencies := []string{}
		if index > 0 {
			expectedDependencies = []string{string(want[index-1])}
		}
		expectedInputContract := runmodel.ContractStageResult
		if index == 0 {
			expectedInputContract = runmodel.ContractReviewInput
		}
		if stage.ID != string(want[index]) ||
			stage.Kind != string(want[index]) ||
			stage.ImplementationRevision != "1" ||
			stage.InputContract != expectedInputContract ||
			stage.OutputContract != runmodel.ContractStageResult ||
			!slices.Equal(stage.DependsOn, expectedDependencies) ||
			stage.Executor != "deterministic-local" ||
			len(stage.RequiredCapabilities) != 0 ||
			stage.AuthorityCeiling != nil ||
			stage.Retry.Jitter ||
			stage.Retry.UnknownOutcome != workflow.UnknownOutcomeFail ||
			stage.FailurePolicy != workflow.FailurePolicyFailRun ||
			stage.SideEffect != workflow.SideEffectNone ||
			stage.ReplayPolicy != workflow.ReplayPolicyCheckpoint {
			return fmt.Errorf(
				"local runtime stage %q has unsupported execution semantics",
				stage.ID,
			)
		}
	}
	return nil
}

func supportedStages() []reviewcore.StageName {
	return []reviewcore.StageName{
		reviewcore.StageMaterializeTarget,
		reviewcore.StagePlanContext,
		reviewcore.StageDetect,
		reviewcore.StageNormalize,
		reviewcore.StageVerify,
		reviewcore.StageAdjudicate,
		reviewcore.StageReport,
		reviewcore.StagePublish,
		reviewcore.StageCaptureFeedback,
		reviewcore.StageExportEvaluation,
	}
}

func parseStage(value string) (reviewcore.StageName, error) {
	stage := reviewcore.StageName(value)
	if slices.Index(supportedStages(), stage) < 0 {
		return "", fmt.Errorf("unsupported replay start stage %q", value)
	}
	return stage, nil
}

func replayPrefix(start reviewcore.StageName) []reviewcore.StageName {
	stages := supportedStages()
	index := slices.Index(stages, start)
	if index <= 0 {
		return []reviewcore.StageName{}
	}
	return slices.Clone(stages[:index])
}

func targetCompleteness(target MaterializedTarget) (runmodel.Completeness, []string) {
	if target.Snapshot.Completeness == gitadapter.CompletenessComplete {
		return runmodel.CompletenessComplete, []string{}
	}
	notes := make([]string, 0, len(target.Snapshot.CompletenessReason))
	for _, reason := range target.Snapshot.CompletenessReason {
		notes = append(notes, formatTargetReason("", reason))
	}
	if len(notes) == 0 {
		notes = append(notes, "materialized target is partial")
	}
	return runmodel.CompletenessPartial, notes
}

func combineShardCompleteness(
	completeness runmodel.Completeness,
	notes []string,
	coverage ScopeShardCoverage,
) (runmodel.Completeness, []string) {
	if coverage.Complete {
		return completeness, notes
	}
	result := slices.Clone(notes)
	for _, reason := range coverage.ReasonCodes {
		result = append(result, "scope shard gap: "+reason)
	}
	slices.Sort(result)
	result = slices.Compact(result)
	return runmodel.CompletenessPartial, result
}

func formatTargetReason(path string, reason gitadapter.Reason) string {
	value := string(reason.Code)
	if path != "" {
		value = path + ": " + value
	}
	if reason.Detail != "" {
		value += ": " + reason.Detail
	}
	return value
}
