package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/configdefaults"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/identity"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
)

const quickActor = "argus-quick"

// These immutable CLI coordinates are deliberately separate from ReviewJob:
// one quick intent owns two jobs, whose commands remain the execution authority.
type quickReviewIdentity struct {
	Key           string                               `json:"key"`
	ConfigState   string                               `json:"config_state"`
	PublicationAt time.Time                            `json:"publication_at"`
	Runtime       formalreview.LocalPiBootstrapOptions `json:"runtime"`
	Pricing       piexecution.PricingCeiling           `json:"pricing"`
	Transport     formalExecutionTransportFlags        `json:"transport"`
}

type quickReviewIntent struct {
	Identity       quickReviewIdentity           `json:"identity"`
	ReviewArgs     []string                      `json:"review_args"`
	Request        reviewjob.Request             `json:"request"`
	SourceRevision reviewconfig.Revision         `json:"source_revision"`
	Runtime        formalreview.LocalPiBootstrap `json:"runtime"`
}

type quickReviewPlan struct {
	IntentSHA256  string                               `json:"intent_sha256"`
	SourceBundle  reviewconfig.ConfigBundle            `json:"source_bundle"`
	SourceReceipt reviewconfig.ConfigResolutionReceipt `json:"source_receipt"`
	FormalBundle  reviewconfig.ConfigBundle            `json:"formal_bundle"`
	FormalReceipt reviewconfig.ConfigResolutionReceipt `json:"formal_receipt"`
	Bootstrap     formalAgentBootstrapOutput           `json:"bootstrap"`
}

type quickImmutable[T any] struct {
	SHA256 string `json:"sha256"`
	Value  T      `json:"value"`
}

func quickDigest(value any) string {
	data, _ := json.Marshal(value) // all inputs are typed, finite JSON values
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func quickIntentID(key string) string { return "quick-intent-" + quickDigest(key) }

func loadQuickImmutable[T any](store *local.Store, id string) (T, error) {
	var record quickImmutable[T]
	if err := store.GetJSON(id, &record); err != nil {
		return record.Value, err
	}
	if record.SHA256 != quickDigest(record.Value) {
		return record.Value, fmt.Errorf("quick immutable %s: %w", id, local.ErrCorrupt)
	}
	return record.Value, nil
}

func saveQuickImmutable[T any](store *local.Store, id string, value T) (T, error) {
	err := store.PutJSON(id, quickImmutable[T]{SHA256: quickDigest(value), Value: value})
	if err != nil && !errors.Is(err, local.ErrImmutableExists) {
		return value, err
	}
	return loadQuickImmutable[T](store, id)
}

func quickIdentity(options agentReviewQuickFlags) quickReviewIdentity {
	at, _ := time.Parse(time.RFC3339Nano, options.publicationAt)
	return quickReviewIdentity{
		Key: options.rootKey, ConfigState: options.formal.configState, PublicationAt: at.UTC(),
		Runtime: options.formal.options, Pricing: options.formal.pricing, Transport: options.formal.transport,
	}
}

func executeAgentReviewQuickWithRunner(ctx context.Context, options agentReviewQuickFlags, stdout io.Writer, runner agentshadowworker.Runner) error {
	return executeAgentReviewQuickWithPolicy(ctx, options, stdout, runner, scheduling.DefaultLocalPolicy())
}

// Policy injection is a trusted composition/test hook, never a CLI flag. The
// scheduling ledger pins its digest and refuses a different policy on restart.
func executeAgentReviewQuickWithPolicy(ctx context.Context, options agentReviewQuickFlags, stdout io.Writer, runner agentshadowworker.Runner, policy scheduling.Policy) error {
	store, err := local.Open(options.formal.store)
	if err != nil {
		return err
	}
	intentID := quickIntentID(options.rootKey)
	intent, err := loadQuickImmutable[quickReviewIntent](store, intentID)
	if errors.Is(err, os.ErrNotExist) && options.resume {
		return executeLegacyAgentReviewQuickWithRunner(ctx, options, stdout, runner)
	}
	if errors.Is(err, os.ErrNotExist) {
		intent, err = freezeQuickIntent(ctx, store, options)
		if err == nil {
			intent, err = saveQuickImmutable(store, intentID, intent)
		}
	}
	if err != nil {
		return fmt.Errorf("freeze quick intent: %w", err)
	}
	if err := intent.Request.Validate(); err != nil {
		return fmt.Errorf("invalid frozen quick request: %w", err)
	}
	if err := intent.SourceRevision.Validate(); err != nil {
		return fmt.Errorf("invalid frozen quick config: %w", err)
	}
	if quickDigest(intent.Identity) != quickDigest(quickIdentity(options)) ||
		(!options.resume && !slices.Equal(intent.ReviewArgs, options.reviewArgs)) {
		return fmt.Errorf("%w: quick key was reused with different arguments", reviewjob.ErrConflict)
	}
	sourceJobID, sourceRunID := reviewjob.DeterministicIDs(quickActor, options.rootKey+"-source")
	formalJobID, _ := reviewjob.DeterministicIDs(quickActor, options.formal.idempotencyKey)
	if options.resume && options.formal.sourceRun != sourceRunID {
		return fmt.Errorf("%w: --source-run does not match quick intent", reviewjob.ErrConflict)
	}
	output := agentReviewQuickOutput{
		IntentID: intentID, SourceJobID: sourceJobID, FormalJobID: formalJobID,
		Phase: "bootstrap", SourceRunID: sourceRunID, ResumeSourceRun: sourceRunID, StorePath: store.Root(),
	}
	executeErr := executeDurableQuick(ctx, store, intentID, intent, options, &output, runner, policy)
	if err := writeAgentReviewQuickOutput(stdout, options.json, output); err != nil {
		return errors.Join(executeErr, err)
	}
	return executeErr
}

func freezeQuickIntent(ctx context.Context, store *local.Store, options agentReviewQuickFlags) (quickReviewIntent, error) {
	if options.formal.transport.Backend != formalExecutionBackendLocalPi {
		return quickReviewIntent{}, fmt.Errorf("durable quick requires --execution-backend local-pi; use agent-review run for Hailix")
	}
	for _, suffix := range []string{"-source", "-run", "-source-validate", "-bootstrap-validate"} {
		if err := (reviewjob.Mutation{Actor: quickActor, IdempotencyKey: options.rootKey + suffix,
			Audit: "freeze quick review", At: time.Now().UTC()}).Validate(); err != nil {
			return quickReviewIntent{}, err
		}
	}
	args := append(slices.Clone(options.reviewArgs), "--store", store.Root())
	flags, err := parseReviewFlags(args)
	if err != nil {
		return quickReviewIntent{}, err
	}
	for _, path := range []string{store.Root(), options.formal.configState} {
		if err := rejectStoreInsideRepository(path, flags.repository); err != nil {
			return quickReviewIntent{}, err
		}
	}
	request, err := materializeReviewCLIRequest(ctx, flags, store.Root())
	if err != nil {
		return quickReviewIntent{}, err
	}
	source, err := gitadapter.New()
	if err != nil {
		return quickReviewIntent{}, err
	}
	freezeRevision := func(revision *string) error {
		capture, err := source.CaptureRevision(ctx, gitadapter.RevisionRequest{
			RepositoryPath: request.RepositoryPath, Revision: *revision,
		})
		if err != nil {
			return err
		}
		request.RepositoryPath = capture.RepositoryRoot
		*revision = capture.Revision.CommitOID
		return nil
	}
	if request.Mode == reviewcore.TargetModeDiff {
		if err := freezeRevision(&request.BaseRevision); err != nil {
			return quickReviewIntent{}, err
		}
		err = freezeRevision(&request.HeadRevision)
	} else {
		err = freezeRevision(&request.Revision)
	}
	if err != nil {
		return quickReviewIntent{}, err
	}
	providers, err := application.InvocationContextProviderDefinitions(request.ContextProviderIDs)
	if err != nil {
		return quickReviewIntent{}, err
	}
	defaults := application.DefaultLocalConfig()
	definition := workflow.DefaultReviewDefinition()
	revision, err := configdefaults.Revision(configdefaults.Options{
		ID: "quick-source", Revision: "1", MaxFiles: defaults.MaxFiles,
		MaxPatchBytes: defaults.MaxPatchBytes, MaxInputBytes: defaults.MaxMaterializedBytes,
		MaxOutputBytes: definition.Stages[0].Budget.MaxOutputBytes,
		MaxAttempts:    defaults.MaxAttempts, ContextProviders: providers,
	}, definition)
	if err != nil {
		return quickReviewIntent{}, err
	}
	jobRequest := reviewjob.Request{
		SchemaVersion: reviewjob.RequestSchemaVersion, ExecutionProfile: reviewjob.DeterministicExecutionProfile,
		RepositoryPath: request.RepositoryPath, Mode: string(request.Mode),
		BaseRevision: request.BaseRevision, HeadRevision: request.HeadRevision, Revision: request.Revision,
		SelectionPath: request.SelectionPath, StartLine: request.StartLine, EndLine: request.EndLine,
		SelectionRanges: request.SelectionRanges, SelectionSymbol: request.SelectionSymbol,
		OverlayContent: request.OverlayContent, Contexts: request.Contexts,
		Include: request.Include, Exclude: request.Exclude, ExecutionTimeoutSeconds: 86400,
	}
	if err := jobRequest.Validate(); err != nil {
		return quickReviewIntent{}, err
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(options.formal.options)
	if err != nil {
		return quickReviewIntent{}, err
	}
	return quickReviewIntent{Identity: quickIdentity(options), ReviewArgs: slices.Clone(options.reviewArgs),
		Request: jobRequest, SourceRevision: revision, Runtime: bootstrap}, nil
}

func freezeQuickPlan(ctx context.Context, store *local.Store, id string, intent quickReviewIntent) (quickReviewPlan, error) {
	if plan, err := loadQuickImmutable[quickReviewPlan](store, id+"-plan"); err == nil {
		if plan.IntentSHA256 != quickDigest(intent) {
			return plan, fmt.Errorf("quick plan does not bind intent: %w", local.ErrCorrupt)
		}
		return plan, validateQuickPlan(plan)
	} else if !errors.Is(err, os.ErrNotExist) {
		return quickReviewPlan{}, err
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(intent.Identity.Runtime)
	if err != nil {
		return quickReviewPlan{}, err
	}
	if quickDigest(bootstrap) != quickDigest(intent.Runtime) {
		return quickReviewPlan{}, fmt.Errorf("quick runtime changed after intent was frozen")
	}
	_, sourceRunID := reviewjob.DeterministicIDs(quickActor, intent.Identity.Key+"-source")
	resolution, err := application.ReviewResolutionContext(sourceRunID, intent.Request.ApplicationRequest())
	if err != nil {
		return quickReviewPlan{}, err
	}
	base := filepath.Join(intent.Identity.ConfigState, "quick", id)
	configStore, err := local.Open(filepath.Join(base, "source"))
	if err != nil {
		return quickReviewPlan{}, err
	}
	configs, err := configrepo.New(configStore)
	if err != nil {
		return quickReviewPlan{}, err
	}
	mutation := func(suffix, audit string) configrepo.Mutation {
		return configrepo.Mutation{IdempotencyKey: intent.Identity.Key + "-source-" + suffix,
			Actor: quickActor, Audit: audit, At: intent.Identity.PublicationAt}
	}
	if err := resolveOrPublishFormalConfig(ctx, configs, intent.SourceRevision, mutation); err != nil {
		return quickReviewPlan{}, err
	}
	plan := quickReviewPlan{IntentSHA256: quickDigest(intent)}
	plan.SourceBundle, plan.SourceReceipt, err = configs.ResolvePublishedWithReceipt(ctx, resolution)
	if err != nil {
		return plan, err
	}
	plan.Bootstrap, err = publishFormalAgentBootstrap(ctx, application.AgentPlanningSubject{
		TenantID: resolution.TenantID, OrganizationID: resolution.OrganizationID,
		WorkspaceID: "local", RepositoryID: resolution.RepositoryID,
	}, bootstrap, store.Root(), filepath.Join(base, "formal"), intent.Identity.Key+"-bootstrap", intent.Identity.PublicationAt)
	if err != nil {
		return plan, err
	}
	plan.Bootstrap.SourceRunID = sourceRunID
	formalStore, err := local.Open(plan.Bootstrap.ConfigState)
	if err != nil {
		return plan, err
	}
	formalConfigs, err := configrepo.New(formalStore)
	if err != nil {
		return plan, err
	}
	resolution.InvocationID = formalreview.FormalRunID(sourceRunID, intent.Identity.Key+"-run")
	plan.FormalBundle, plan.FormalReceipt, err = formalConfigs.ResolvePublishedWithReceipt(ctx, resolution)
	if err != nil {
		return plan, err
	}
	if err := validateQuickPlan(plan); err != nil {
		return plan, err
	}
	plan, err = saveQuickImmutable(store, id+"-plan", plan)
	if err != nil {
		return plan, err
	}
	if plan.IntentSHA256 != quickDigest(intent) {
		return plan, local.ErrCorrupt
	}
	return plan, validateQuickPlan(plan)
}

func validateQuickPlan(plan quickReviewPlan) error {
	for _, frozen := range []struct {
		bundle  reviewconfig.ConfigBundle
		receipt reviewconfig.ConfigResolutionReceipt
	}{
		{plan.SourceBundle, plan.SourceReceipt}, {plan.FormalBundle, plan.FormalReceipt},
	} {
		if err := frozen.bundle.Validate(); err != nil {
			return err
		}
		if err := frozen.receipt.ValidateAgainst(frozen.bundle); err != nil {
			return err
		}
	}
	return nil
}

func (plan quickReviewPlan) ResolvePublishedWithReceipt(ctx context.Context, resolution reviewconfig.ResolutionContext) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := ctx.Err(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	if reflect.DeepEqual(resolution, plan.SourceBundle.Context) {
		return plan.SourceBundle, plan.SourceReceipt, nil
	}
	if reflect.DeepEqual(resolution, plan.FormalBundle.Context) {
		return plan.FormalBundle, plan.FormalReceipt, nil
	}
	return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, fmt.Errorf("quick config resolution is outside frozen intent")
}

func executeDurableQuick(ctx context.Context, store *local.Store, id string, intent quickReviewIntent, options agentReviewQuickFlags, output *agentReviewQuickOutput, runner agentshadowworker.Runner, policy scheduling.Policy) error {
	plan, err := freezeQuickPlan(ctx, store, id, intent)
	if err != nil {
		return fmt.Errorf("quick bootstrap: %w", err)
	}
	output.Bootstrap = &plan.Bootstrap
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	jobs, err := reviewjob.NewRepository(store)
	if err != nil {
		return err
	}
	workloads, err := scheduling.NewRepository(store, policy)
	if err != nil {
		return err
	}
	runJob := func(request reviewjob.Request, key string, formal bool) (reviewjob.Record, error) {
		workerID, err := identity.NewGenerator().New("quickworker")
		if err != nil {
			return reviewjob.Record{}, err
		}
		executor, err := newLocalReviewJobExecutor(store, runs, workloads, workerID, runner)
		if err != nil {
			return reviewjob.Record{}, err
		}
		service, err := reviewjob.NewService(jobs, workloads, runs, plan, executor, workerID, nil)
		if err != nil {
			return reviewjob.Record{}, err
		}
		mutation := reviewjob.Mutation{Actor: quickActor, IdempotencyKey: key, Audit: "execute frozen quick review", At: time.Now().UTC()}
		_, existing, lookupErr := jobs.GetByIdempotency(quickActor, key)
		if lookupErr == nil {
			mutation.At = existing.SubmittedAt
		} else if !errors.Is(lookupErr, reviewjob.ErrNotFound) {
			return reviewjob.Record{}, lookupErr
		} else {
			mutation, err = saveQuickImmutable(store, quickIntentID(key)+"-submission", mutation)
			if err != nil {
				return reviewjob.Record{}, err
			}
			if mutation.Actor != quickActor || mutation.IdempotencyKey != key || mutation.Audit != "execute frozen quick review" {
				return reviewjob.Record{}, fmt.Errorf("quick submission does not bind job: %w", local.ErrCorrupt)
			}
		}
		if errors.Is(lookupErr, reviewjob.ErrNotFound) && formal {
			live, err := formalreview.BuildLocalPiBootstrap(intent.Identity.Runtime)
			if err != nil {
				return reviewjob.Record{}, err
			}
			if quickDigest(live) != quickDigest(intent.Runtime) {
				return reviewjob.Record{}, fmt.Errorf("quick runtime drifted before formal admission")
			}
			admission, err := newLocalFormalAdmissionValidator(store)
			if err != nil {
				return reviewjob.Record{}, err
			}
			if err := service.ConfigureFormalProfile(reviewjob.FormalProfile{Options: intent.Identity.Runtime, Pricing: intent.Identity.Pricing, Admission: admission}); err != nil {
				return reviewjob.Record{}, err
			}
		}
		record, err := service.Submit(ctx, request, mutation)
		if err != nil {
			return record, err
		}
		jobCtx, cancel := context.WithCancel(ctx)
		if err := service.StartJobs(jobCtx, []string{record.JobID}); err != nil {
			cancel()
			return record, err
		}
		defer func() { cancel(); service.Wait() }()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			record, err = service.Get(record.JobID)
			if err != nil {
				return record, err
			}
			switch record.Workload.State {
			case scheduling.StateSucceeded:
				return record, nil
			case scheduling.StateFailed, scheduling.StateCanceled, scheduling.StateRejected, scheduling.StateThrottled:
				return record, fmt.Errorf("review job %s ended with %s: %s", record.JobID, record.Workload.State, record.Workload.StateReason)
			}
			select {
			case <-ctx.Done():
				return record, ctx.Err()
			case <-ticker.C:
			}
		}
	}
	output.Phase = "source"
	source, err := runJob(intent.Request, intent.Identity.Key+"-source", false)
	if source.Run != nil && !options.resume {
		ref, refErr := runs.CommittedRunRef(source.RunID)
		if refErr != nil {
			return refErr
		}
		output.Source = &runOutput{Run: *source.Run, FinalRef: ref, StorePath: store.Root(), Reused: []reviewcore.StageName{}, Coverage: summarizeRunCoverage(runs, *source.Run)}
		if source.Run.JSONReportRef != nil {
			var report reviewcore.Report
			if err := runs.ReadJSONArtifact(*source.Run.JSONReportRef, &report); err != nil {
				return err
			}
			output.Source.Report = &report
		}
	}
	if err != nil {
		return fmt.Errorf("quick source: %w", err)
	}
	output.Phase = "formal"
	formalRequest := reviewjob.Request{SchemaVersion: reviewjob.RequestSchemaVersion,
		ExecutionProfile: reviewjob.FormalPiExecutionProfile, SourceRunID: output.SourceRunID,
		ExecutionTimeoutSeconds: 86400}
	formal, formalErr := runJob(formalRequest, intent.Identity.Key+"-run", true)
	if formal.Run != nil {
		var buffer bytes.Buffer
		showErr := executeFormalAgentShowWithPolicy(ctx, formalAgentShowFlags{store: store.Root(), formalRun: formal.RunID, json: true}, &buffer, policy)
		if buffer.Len() > 0 {
			var result formalAgentRunOutput
			if err := json.Unmarshal(buffer.Bytes(), &result); err != nil {
				return errors.Join(formalErr, showErr, err)
			}
			output.Formal = &result
		}
		if showErr != nil {
			return errors.Join(formalErr, showErr)
		}
	}
	if formalErr != nil {
		return fmt.Errorf("quick formal review: %w", formalErr)
	}
	if formal.Run == nil || formal.Run.Status != runmodel.RunStatusSucceeded {
		return fmt.Errorf("formal job succeeded without committed ReviewRun")
	}
	output.Phase = "succeeded"
	return nil
}
