// Package formalreview composes the immutable deterministic capture ledger
// with the formal agent execution ledger. It does not run a model or admit a
// result; those responsibilities stay in application and piexecution.
package formalreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type InitializeCommand struct {
	SourceRunID         string
	IdempotencyKey      string
	BuildIdentity       string
	RuntimeProfile      string
	RuntimeEvidenceRefs []runmodel.ArtifactRef
}

type ExactReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
}

type BudgetReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
}

type ModelReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type PromptReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type SkillPackReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type KnowledgePackReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type RulePackReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type WorkflowReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
	Definition     workflow.Definition
}

type FindingGovernanceReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type FilterPolicyReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type IndexReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type componentReplayCommand struct {
	SourceRunID    string
	IdempotencyKey string
	BuildIdentity  string
	CreatedAt      time.Time
}

type componentReplaySpec struct {
	variable                   runmodel.ReplayVariable
	startStage                 string
	requireBuildIdentityChange bool
	changedFields              []string
	workflowDefinition         *workflow.Definition
	validate                   func(reviewconfig.ConfigBundle, reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt) error
}

type InitializedRun struct {
	RunID           string
	SourceRunID     string
	Kind            runmodel.RunKind
	ReplayFromStage string
	ReplayRootRunID string
	ReplayNamespace string
	ReplayVariable  runmodel.ReplayVariable
	Snapshot        runmodel.ExecutionSnapshot
	ReviewSpec      contractsv1alpha1.ReviewSpec
	Recovered       bool
}

type Initializer struct {
	runs              runRepository
	configs           application.GovernedConfigProvider
	workflow          workflow.Definition
	indexMaterializer application.IndexReplayMaterializer
}

type InitializerOption func(*Initializer) error

func WithIndexReplayMaterializer(
	materializer application.IndexReplayMaterializer,
) InitializerOption {
	return func(initializer *Initializer) error {
		if materializer == nil {
			return fmt.Errorf("formal index replay materializer is required")
		}
		if initializer.indexMaterializer != nil {
			return fmt.Errorf("formal index replay materializer is already configured")
		}
		initializer.indexMaterializer = materializer
		return nil
	}
}

type runRepository interface {
	CommittedRunRef(string) (runmodel.ArtifactRef, error)
	ExecutionSnapshotForRun(string) (runmodel.ExecutionSnapshot, error)
	LoadRun(string) (runmodel.ReviewRun, error)
	ReadJSONArtifact(runmodel.ArtifactRef, any) error
	PutJSONArtifact(string, any) (runmodel.ArtifactRef, error)
	SaveExecutionSnapshot(runmodel.ExecutionSnapshot) error
	AppendEvent(string, time.Time, runrepo.RunEvent) error
}

func NewInitializer(
	runs runRepository,
	configs application.GovernedConfigProvider,
	definition workflow.Definition,
	options ...InitializerOption,
) (*Initializer, error) {
	if runs == nil || configs == nil {
		return nil, fmt.Errorf("run repository and governed config provider are required")
	}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("validate formal workflow: %w", err)
	}
	if len(definition.Stages) != 1 || definition.Stages[0].Kind != "agent_hypothesize" {
		return nil, fmt.Errorf("formal review initializer requires one agent_hypothesize stage")
	}
	initializer := &Initializer{runs: runs, configs: configs, workflow: definition}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("formal initializer option %d is nil", index)
		}
		if err := option(initializer); err != nil {
			return nil, err
		}
	}
	return initializer, nil
}

// Initialize derives one immutable formal execution snapshot from a committed
// source capture. The source target and ReviewInput artifacts are reused by
// content reference; the ReviewSpec, config and workflow are newly frozen for
// the deterministic formal run ID. Exact retries return before consulting the
// mutable config lifecycle.
func (initializer *Initializer) Initialize(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command InitializeCommand,
) (InitializedRun, error) {
	if ctx == nil {
		return InitializedRun{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if initializer == nil || initializer.runs == nil || initializer.configs == nil {
		return InitializedRun{}, fmt.Errorf("formal review initializer is not initialized")
	}
	if command.SourceRunID == "" || command.IdempotencyKey == "" ||
		command.BuildIdentity == "" || command.RuntimeProfile == "" ||
		len(command.RuntimeEvidenceRefs) != 1 {
		return InitializedRun{}, fmt.Errorf(
			"source run, idempotency key, build identity, runtime profile, and one runtime evidence ref are required",
		)
	}
	if err := command.RuntimeEvidenceRefs[0].Validate(); err != nil ||
		command.RuntimeEvidenceRefs[0].Contract != runmodel.ContractLocalRuntimeFileManifest {
		return InitializedRun{}, fmt.Errorf("runtime evidence ref is invalid")
	}
	runID := FormalRunID(command.SourceRunID, command.IdempotencyKey)
	_, err := initializer.runs.CommittedRunRef(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve committed source run: %w", err)
	}
	sourceSnapshot, err := initializer.runs.ExecutionSnapshotForRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load source execution snapshot: %w", err)
	}
	if existing, loadErr := initializer.runs.ExecutionSnapshotForRun(runID); loadErr == nil {
		return initializer.recoverExisting(ctx, subject, command, sourceSnapshot, existing)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return InitializedRun{}, fmt.Errorf("lookup existing formal run: %w", loadErr)
	}

	sourceRun, err := initializer.runs.LoadRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load committed source run: %w", err)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return InitializedRun{}, fmt.Errorf("read source ReviewSpec: %w", err)
	}
	if sourceSpec.TenantID != subject.TenantID ||
		sourceSpec.WorkspaceID != subject.WorkspaceID ||
		sourceSpec.Repository.RepositoryID != subject.RepositoryID ||
		subject.OrganizationID == "" {
		return InitializedRun{}, fmt.Errorf("source run does not match authenticated formal subject")
	}
	resolution := reviewconfig.ResolutionContext{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		RepositoryID: subject.RepositoryID, InvocationID: runID,
	}
	if sourceSpec.Target.Mode == contractsv1alpha1.ReviewModeSelection {
		resolution.Path = sourceSpec.Target.Selection.Path
	}
	bundle, receipt, err := initializer.configs.ResolvePublishedWithReceipt(ctx, resolution)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve formal governed config: %w", err)
	}
	if err := receipt.ValidateAgainst(bundle); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal config receipt: %w", err)
	}
	if bundle.AgentReview == nil {
		return InitializedRun{}, fmt.Errorf("published config has no agent_review policy")
	}
	workflowDigest, err := workflow.DigestDefinition(initializer.workflow)
	if err != nil {
		return InitializedRun{}, err
	}
	if bundle.Workflow.Definition.ID != initializer.workflow.ID ||
		bundle.Workflow.Definition.Revision != initializer.workflow.Revision ||
		bundle.Workflow.Definition.SHA256 != workflowDigest {
		return InitializedRun{}, fmt.Errorf("published config does not select the formal workflow")
	}
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(bundle)
	if err != nil {
		return InitializedRun{}, err
	}
	spec := sourceSpec
	spec.RequestID = runID
	spec.IdempotencyKey = runID
	spec.ConfigBundleRef = contractsv1alpha1.VersionedRef{
		ID: bundle.BundleID, Revision: bundle.SHA256[:16], SHA256: configArtifactDigest,
	}
	spec.WorkflowRef = contractsv1alpha1.VersionedRef{
		ID: initializer.workflow.ID, Revision: initializer.workflow.Revision,
		SHA256: workflowDigest,
	}
	if err := spec.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal ReviewSpec: %w", err)
	}
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return InitializedRun{}, err
	}
	specRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal ReviewSpec: %w", err)
	}
	if specRef.SHA256 != specDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal ReviewSpec digest changed")
	}
	workflowRef, err := initializer.runs.PutJSONArtifact(
		runmodel.ContractWorkflowDefinition, initializer.workflow,
	)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal workflow: %w", err)
	}
	if workflowRef.SHA256 != workflowDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal workflow digest changed")
	}
	configRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractConfigBundle, bundle)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal config: %w", err)
	}
	if configRef.SHA256 != configArtifactDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal config digest changed")
	}
	createdAt := sourceSnapshot.CreatedAt.UTC()
	if sourceRun.CompletedAt != nil && !sourceRun.CompletedAt.IsZero() {
		createdAt = sourceRun.CompletedAt.UTC()
	}
	policy := *bundle.AgentReview
	snapshot := runmodel.ExecutionSnapshot{
		SchemaVersion:       runmodel.SnapshotSchemaVersion,
		ExecutionSnapshotID: runID + "-snapshot",
		ReviewSpecSHA256:    specDigest, ReviewSpecRef: specRef,
		TargetSnapshotRef:     sourceSnapshot.TargetSnapshotRef,
		ReviewInputRef:        sourceSnapshot.ReviewInputRef,
		ReplayInputRefs:       []runmodel.ArtifactRef{},
		RuntimeEvidenceRefs:   slices.Clone(command.RuntimeEvidenceRefs),
		WorkflowDefinitionRef: workflowRef, ConfigBundleRef: configRef,
		Workflow: runmodel.WorkflowRef{
			ID: initializer.workflow.ID, Revision: initializer.workflow.Revision,
			SHA256: workflowDigest,
		},
		Config: runmodel.PolicyRef{
			ID: bundle.BundleID, Revision: bundle.SHA256[:16], SHA256: bundle.SHA256,
		},
		RuntimeProfile: command.RuntimeProfile, BuildIdentity: command.BuildIdentity,
		ToolPolicy: runmodel.ToolInvocationPolicy{
			AllowedTools: slices.Clone(policy.Authority.Tools), Network: "deny",
			WorkspaceWrites: "deny", RemoteWrites: "deny",
			PerCallTimeoutMS:   policy.Budget.TimeoutMS,
			MaxOutputBytes:     policy.Budget.MaxOutputBytes,
			MaxConcurrency:     policy.Budget.MaxConcurrency,
			MaxDelegationDepth: policy.Authority.MaxDelegationDepth,
		},
		RemoteWrites: "deny", CreatedAt: createdAt,
	}
	if err := snapshot.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal execution snapshot: %w", err)
	}
	if err := initializer.runs.SaveExecutionSnapshot(snapshot); err != nil {
		return InitializedRun{}, fmt.Errorf("save formal execution snapshot: %w", err)
	}
	if err := initializer.runs.AppendEvent(runID+"-created", createdAt, runrepo.RunEvent{
		RunID: runID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusPending,
		EventType: runrepo.EventRunCreated, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
	}); err != nil {
		return InitializedRun{}, fmt.Errorf("append formal run.created: %w", err)
	}
	if err := initializer.runs.AppendEvent(runID+"-started", createdAt, runrepo.RunEvent{
		RunID: runID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusRunning,
		EventType: runrepo.EventRunStarted, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
	}); err != nil {
		return InitializedRun{}, fmt.Errorf("append formal run.started: %w", err)
	}
	return InitializedRun{
		RunID: runID, SourceRunID: command.SourceRunID,
		Kind:     runmodel.RunKindReview,
		Snapshot: snapshot, ReviewSpec: spec,
	}, nil
}

func (initializer *Initializer) recoverExisting(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command InitializeCommand,
	sourceSnapshot runmodel.ExecutionSnapshot,
	snapshot runmodel.ExecutionSnapshot,
) (InitializedRun, error) {
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if snapshot.TargetSnapshotRef != sourceSnapshot.TargetSnapshotRef ||
		snapshot.ReviewInputRef != sourceSnapshot.ReviewInputRef ||
		snapshot.BuildIdentity != command.BuildIdentity ||
		snapshot.RuntimeProfile != command.RuntimeProfile ||
		!slices.Equal(snapshot.RuntimeEvidenceRefs, command.RuntimeEvidenceRefs) {
		return InitializedRun{}, fmt.Errorf("existing formal run conflicts with immutable initialization")
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return InitializedRun{}, err
	}
	if spec.TenantID != subject.TenantID || spec.WorkspaceID != subject.WorkspaceID ||
		spec.Repository.RepositoryID != subject.RepositoryID ||
		spec.IdempotencyKey != spec.RequestID {
		return InitializedRun{}, fmt.Errorf("existing formal run escaped subject or idempotency binding")
	}
	return InitializedRun{
		RunID: spec.RequestID, SourceRunID: command.SourceRunID,
		Kind:     runmodel.RunKindReview,
		Snapshot: snapshot, ReviewSpec: spec, Recovered: true,
	}, nil
}

// InitializeExactReplay freezes a new formal run from one exact succeeded
// formal source. It reuses source target/config/workflow/runtime refs, changes
// only ReviewSpec request identity, and records an explicit variable=none
// ReplayChangeSet. It never consults mutable config lifecycle state.
func (initializer *Initializer) InitializeExactReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command ExactReplayCommand,
) (InitializedRun, error) {
	if ctx == nil {
		return InitializedRun{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if initializer == nil || initializer.runs == nil {
		return InitializedRun{}, fmt.Errorf("formal review initializer is not initialized")
	}
	if command.SourceRunID == "" || command.IdempotencyKey == "" {
		return InitializedRun{}, fmt.Errorf("source formal run and idempotency key are required")
	}
	runID := formalReplayRunID(command.SourceRunID, command.IdempotencyKey)
	sourceRef, err := initializer.runs.CommittedRunRef(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve committed formal replay source: %w", err)
	}
	source, err := initializer.runs.LoadRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal replay source: %w", err)
	}
	if source.Status != runmodel.RunStatusSucceeded || source.CompletedAt == nil {
		return InitializedRun{}, fmt.Errorf("formal replay source must be a succeeded committed run")
	}
	sourceSnapshot, err := initializer.runs.ExecutionSnapshotForRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal replay source snapshot: %w", err)
	}
	if _, workflowErr := initializer.loadFormalWorkflow(sourceSnapshot); workflowErr != nil ||
		len(sourceSnapshot.RuntimeEvidenceRefs) != 1 || sourceSnapshot.RemoteWrites != "deny" {
		return InitializedRun{}, fmt.Errorf("source run is not an exact formal agent execution")
	}
	if existing, loadErr := initializer.runs.ExecutionSnapshotForRun(runID); loadErr == nil {
		return initializer.recoverExistingExactReplay(
			ctx, subject, command, source, sourceRef, sourceSnapshot, existing,
		)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return InitializedRun{}, fmt.Errorf("lookup existing formal replay: %w", loadErr)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal replay source ReviewSpec: %w", err)
	}
	if sourceSpec.TenantID != subject.TenantID ||
		sourceSpec.WorkspaceID != subject.WorkspaceID ||
		sourceSpec.Repository.RepositoryID != subject.RepositoryID ||
		subject.OrganizationID == "" {
		return InitializedRun{}, fmt.Errorf("formal replay source does not match authenticated subject")
	}
	spec := sourceSpec
	spec.RequestID = runID
	spec.IdempotencyKey = runID
	if err := spec.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate exact replay ReviewSpec: %w", err)
	}
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return InitializedRun{}, err
	}
	specRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist exact replay ReviewSpec: %w", err)
	}
	if specRef.SHA256 != specDigest {
		return InitializedRun{}, fmt.Errorf("persisted exact replay ReviewSpec digest changed")
	}
	rootRunID := source.RunID
	parentReplayRunID := ""
	if source.Kind == runmodel.RunKindReplay {
		rootRunID = source.ReplayRootRunID
		parentReplayRunID = source.RunID
	}
	createdAt := source.CompletedAt.UTC()
	change := runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     runID, SourceRunID: source.RunID, RootRunID: rootRunID,
		ParentReplayRunID: parentReplayRunID, StartStage: "agent_hypothesize",
		Variable:       runmodel.ReplayVariableNone,
		BaselineSHA256: sourceSnapshot.Config.SHA256,
		VariantSHA256:  sourceSnapshot.Config.SHA256,
		ChangedFields:  []string{}, RemoteWrites: "deny", CreatedAt: createdAt,
	}
	if err := change.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate exact formal replay change set: %w", err)
	}
	changeRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReplayChangeSet, change)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist exact formal replay change set: %w", err)
	}
	snapshot := sourceSnapshot
	snapshot.ExecutionSnapshotID = runID + "-snapshot"
	snapshot.ReviewSpecSHA256 = specDigest
	snapshot.ReviewSpecRef = specRef
	snapshot.ReplayInputRefs = []runmodel.ArtifactRef{}
	snapshot.ReplaySourceRunRef = &sourceRef
	snapshot.ReplayChangeSetRef = &changeRef
	snapshot.CreatedAt = createdAt
	if err := snapshot.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate exact formal replay snapshot: %w", err)
	}
	if err := initializer.runs.SaveExecutionSnapshot(snapshot); err != nil {
		return InitializedRun{}, fmt.Errorf("save exact formal replay snapshot: %w", err)
	}
	for _, event := range []struct {
		id, eventType string
		status        runmodel.RunStatus
	}{
		{runID + "-created", runrepo.EventRunCreated, runmodel.RunStatusPending},
		{runID + "-started", runrepo.EventRunStarted, runmodel.RunStatusRunning},
	} {
		if err := initializer.runs.AppendEvent(event.id, createdAt, runrepo.RunEvent{
			RunID: runID, Kind: runmodel.RunKindReplay, Status: event.status,
			EventType: event.eventType, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		}); err != nil {
			return InitializedRun{}, fmt.Errorf("append exact formal replay %s: %w", event.eventType, err)
		}
	}
	return InitializedRun{
		RunID: runID, SourceRunID: source.RunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: "agent_hypothesize", ReplayRootRunID: rootRunID,
		ReplayNamespace: runID, ReplayVariable: runmodel.ReplayVariableNone,
		Snapshot: snapshot, ReviewSpec: spec,
	}, nil
}

// InitializeBudgetReplay derives one formal replay whose sole behavior change
// is the governed stage/Pi timeout pair. The config provider must be frozen to
// a replay_variant_derived receipt; mutable lifecycle latest is never read.
func (initializer *Initializer) InitializeBudgetReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command BudgetReplayCommand,
) (InitializedRun, error) {
	if ctx == nil {
		return InitializedRun{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if initializer == nil || initializer.runs == nil || initializer.configs == nil {
		return InitializedRun{}, fmt.Errorf("formal review initializer is not initialized")
	}
	if command.SourceRunID == "" || command.IdempotencyKey == "" {
		return InitializedRun{}, fmt.Errorf("source formal run and idempotency key are required")
	}
	runID := formalVariantReplayRunID(command.SourceRunID, command.IdempotencyKey)
	sourceRef, err := initializer.runs.CommittedRunRef(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve committed formal budget replay source: %w", err)
	}
	source, err := initializer.runs.LoadRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal budget replay source: %w", err)
	}
	if source.Status != runmodel.RunStatusSucceeded || source.CompletedAt == nil {
		return InitializedRun{}, fmt.Errorf("formal budget replay source must be a succeeded committed run")
	}
	sourceSnapshot, err := initializer.runs.ExecutionSnapshotForRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal budget replay source snapshot: %w", err)
	}
	if _, workflowErr := initializer.loadFormalWorkflow(sourceSnapshot); workflowErr != nil ||
		len(sourceSnapshot.RuntimeEvidenceRefs) != 1 || sourceSnapshot.RemoteWrites != "deny" {
		return InitializedRun{}, fmt.Errorf("source run is not an exact formal agent execution")
	}
	var sourceBundle reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ConfigBundleRef, &sourceBundle); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal budget replay source ConfigBundle: %w", err)
	}
	variant, receipt, err := initializer.configs.ResolvePublishedWithReceipt(ctx, sourceBundle.Context)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve formal budget replay variant: %w", err)
	}
	if err := validateFormalBudgetVariant(sourceBundle, variant, receipt); err != nil {
		return InitializedRun{}, err
	}
	if existing, loadErr := initializer.runs.ExecutionSnapshotForRun(runID); loadErr == nil {
		return initializer.recoverExistingBudgetReplay(
			ctx, subject, command, source, sourceRef, sourceSnapshot, variant, existing,
		)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return InitializedRun{}, fmt.Errorf("lookup existing formal budget replay: %w", loadErr)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal budget replay source ReviewSpec: %w", err)
	}
	if sourceSpec.TenantID != subject.TenantID || sourceSpec.WorkspaceID != subject.WorkspaceID ||
		sourceSpec.Repository.RepositoryID != subject.RepositoryID || subject.OrganizationID == "" {
		return InitializedRun{}, fmt.Errorf("formal budget replay source does not match authenticated subject")
	}
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(variant)
	if err != nil {
		return InitializedRun{}, err
	}
	configRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractConfigBundle, variant)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal budget replay ConfigBundle: %w", err)
	}
	if configRef.SHA256 != configArtifactDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal budget replay ConfigBundle digest changed")
	}
	spec := sourceSpec
	spec.RequestID, spec.IdempotencyKey = runID, runID
	spec.ConfigBundleRef = contractsv1alpha1.VersionedRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: configArtifactDigest,
	}
	if err := spec.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal budget replay ReviewSpec: %w", err)
	}
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return InitializedRun{}, err
	}
	specRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal budget replay ReviewSpec: %w", err)
	}
	if specRef.SHA256 != specDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal budget replay ReviewSpec digest changed")
	}
	rootRunID, parentReplayRunID := source.RunID, ""
	if source.Kind == runmodel.RunKindReplay {
		rootRunID, parentReplayRunID = source.ReplayRootRunID, source.RunID
	}
	createdAt := source.CompletedAt.UTC()
	change := runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     runID, SourceRunID: source.RunID, RootRunID: rootRunID,
		ParentReplayRunID: parentReplayRunID, StartStage: "agent_hypothesize",
		Variable: runmodel.ReplayVariableBudget, BaselineSHA256: sourceBundle.SHA256,
		VariantSHA256: variant.SHA256, ChangedFields: FormalBudgetReplayChangedFields(),
		RemoteWrites: "deny", CreatedAt: createdAt,
	}
	if err := change.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal budget replay change set: %w", err)
	}
	changeRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReplayChangeSet, change)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal budget replay change set: %w", err)
	}
	snapshot := sourceSnapshot
	snapshot.ExecutionSnapshotID = runID + "-snapshot"
	snapshot.ReviewSpecSHA256, snapshot.ReviewSpecRef = specDigest, specRef
	snapshot.ConfigBundleRef = configRef
	snapshot.Config = runmodel.PolicyRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: variant.SHA256,
	}
	snapshot.ToolPolicy.PerCallTimeoutMS = variant.Budget.StageTimeoutMS
	snapshot.ReplayInputRefs = []runmodel.ArtifactRef{}
	snapshot.ReplaySourceRunRef, snapshot.ReplayChangeSetRef = &sourceRef, &changeRef
	snapshot.CreatedAt = createdAt
	if err := snapshot.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal budget replay snapshot: %w", err)
	}
	if err := initializer.runs.SaveExecutionSnapshot(snapshot); err != nil {
		return InitializedRun{}, fmt.Errorf("save formal budget replay snapshot: %w", err)
	}
	for _, event := range []struct {
		id, eventType string
		status        runmodel.RunStatus
	}{
		{runID + "-created", runrepo.EventRunCreated, runmodel.RunStatusPending},
		{runID + "-started", runrepo.EventRunStarted, runmodel.RunStatusRunning},
	} {
		if err := initializer.runs.AppendEvent(event.id, createdAt, runrepo.RunEvent{
			RunID: runID, Kind: runmodel.RunKindReplay, Status: event.status,
			EventType: event.eventType, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		}); err != nil {
			return InitializedRun{}, fmt.Errorf("append formal budget replay %s: %w", event.eventType, err)
		}
	}
	return InitializedRun{
		RunID: runID, SourceRunID: source.RunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: "agent_hypothesize", ReplayRootRunID: rootRunID,
		ReplayNamespace: runID, ReplayVariable: runmodel.ReplayVariableBudget,
		Snapshot: snapshot, ReviewSpec: spec,
	}, nil
}

// InitializeModelReplay derives one formal replay whose sole config change is
// the governed model component. The selected Pi manifest build identity must
// change with that component, while runtime files, workflow, input, authority,
// and budgets remain frozen.
func (initializer *Initializer) InitializeModelReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command ModelReplayCommand,
) (InitializedRun, error) {
	if ctx == nil {
		return InitializedRun{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if initializer == nil || initializer.runs == nil || initializer.configs == nil {
		return InitializedRun{}, fmt.Errorf("formal review initializer is not initialized")
	}
	if command.SourceRunID == "" || command.IdempotencyKey == "" ||
		command.BuildIdentity == "" || command.CreatedAt.IsZero() ||
		command.CreatedAt.Location() != time.UTC {
		return InitializedRun{}, fmt.Errorf(
			"source formal run, idempotency key, build identity, and UTC created_at are required",
		)
	}
	runID := formalVariantReplayRunID(command.SourceRunID, command.IdempotencyKey)
	sourceRef, err := initializer.runs.CommittedRunRef(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve committed formal model replay source: %w", err)
	}
	source, err := initializer.runs.LoadRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal model replay source: %w", err)
	}
	if source.Status != runmodel.RunStatusSucceeded || source.CompletedAt == nil {
		return InitializedRun{}, fmt.Errorf("formal model replay source must be a succeeded committed run")
	}
	if command.CreatedAt.Before(*source.CompletedAt) {
		return InitializedRun{}, fmt.Errorf("formal model replay created_at precedes source completion")
	}
	sourceSnapshot, err := initializer.runs.ExecutionSnapshotForRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal model replay source snapshot: %w", err)
	}
	if _, workflowErr := initializer.loadFormalWorkflow(sourceSnapshot); workflowErr != nil ||
		len(sourceSnapshot.RuntimeEvidenceRefs) != 1 || sourceSnapshot.RemoteWrites != "deny" {
		return InitializedRun{}, fmt.Errorf("source run is not an exact formal agent execution")
	}
	if sourceSnapshot.BuildIdentity == command.BuildIdentity {
		return InitializedRun{}, fmt.Errorf("formal model replay requires a changed Pi build identity")
	}
	var sourceBundle reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ConfigBundleRef, &sourceBundle); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal model replay source ConfigBundle: %w", err)
	}
	variant, receipt, err := initializer.configs.ResolvePublishedWithReceipt(ctx, sourceBundle.Context)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve formal model replay variant: %w", err)
	}
	if err := validateFormalModelVariant(sourceBundle, variant, receipt); err != nil {
		return InitializedRun{}, err
	}
	if existing, loadErr := initializer.runs.ExecutionSnapshotForRun(runID); loadErr == nil {
		return initializer.recoverExistingModelReplay(
			ctx, subject, command, source, sourceRef, sourceSnapshot, variant, existing,
		)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return InitializedRun{}, fmt.Errorf("lookup existing formal model replay: %w", loadErr)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal model replay source ReviewSpec: %w", err)
	}
	if sourceSpec.TenantID != subject.TenantID || sourceSpec.WorkspaceID != subject.WorkspaceID ||
		sourceSpec.Repository.RepositoryID != subject.RepositoryID || subject.OrganizationID == "" {
		return InitializedRun{}, fmt.Errorf("formal model replay source does not match authenticated subject")
	}
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(variant)
	if err != nil {
		return InitializedRun{}, err
	}
	configRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractConfigBundle, variant)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal model replay ConfigBundle: %w", err)
	}
	if configRef.SHA256 != configArtifactDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal model replay ConfigBundle digest changed")
	}
	spec := sourceSpec
	spec.RequestID, spec.IdempotencyKey = runID, runID
	spec.ConfigBundleRef = contractsv1alpha1.VersionedRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: configArtifactDigest,
	}
	if err := spec.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal model replay ReviewSpec: %w", err)
	}
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return InitializedRun{}, err
	}
	specRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal model replay ReviewSpec: %w", err)
	}
	if specRef.SHA256 != specDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal model replay ReviewSpec digest changed")
	}
	rootRunID, parentReplayRunID := source.RunID, ""
	if source.Kind == runmodel.RunKindReplay {
		rootRunID, parentReplayRunID = source.ReplayRootRunID, source.RunID
	}
	change := runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     runID, SourceRunID: source.RunID, RootRunID: rootRunID,
		ParentReplayRunID: parentReplayRunID, StartStage: "agent_hypothesize",
		Variable: runmodel.ReplayVariableModel, BaselineSHA256: sourceBundle.SHA256,
		VariantSHA256: variant.SHA256, ChangedFields: FormalModelReplayChangedFields(),
		RemoteWrites: "deny", CreatedAt: command.CreatedAt,
	}
	if err := change.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal model replay change set: %w", err)
	}
	changeRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReplayChangeSet, change)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal model replay change set: %w", err)
	}
	snapshot := sourceSnapshot
	snapshot.ExecutionSnapshotID = runID + "-snapshot"
	snapshot.ReviewSpecSHA256, snapshot.ReviewSpecRef = specDigest, specRef
	snapshot.ConfigBundleRef = configRef
	snapshot.Config = runmodel.PolicyRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: variant.SHA256,
	}
	snapshot.BuildIdentity = command.BuildIdentity
	snapshot.ReplayInputRefs = []runmodel.ArtifactRef{}
	snapshot.ReplaySourceRunRef, snapshot.ReplayChangeSetRef = &sourceRef, &changeRef
	snapshot.CreatedAt = command.CreatedAt
	if err := snapshot.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal model replay snapshot: %w", err)
	}
	if err := initializer.runs.SaveExecutionSnapshot(snapshot); err != nil {
		return InitializedRun{}, fmt.Errorf("save formal model replay snapshot: %w", err)
	}
	for _, event := range []struct {
		id, eventType string
		status        runmodel.RunStatus
	}{
		{runID + "-created", runrepo.EventRunCreated, runmodel.RunStatusPending},
		{runID + "-started", runrepo.EventRunStarted, runmodel.RunStatusRunning},
	} {
		if err := initializer.runs.AppendEvent(event.id, command.CreatedAt, runrepo.RunEvent{
			RunID: runID, Kind: runmodel.RunKindReplay, Status: event.status,
			EventType: event.eventType, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		}); err != nil {
			return InitializedRun{}, fmt.Errorf("append formal model replay %s: %w", event.eventType, err)
		}
	}
	return InitializedRun{
		RunID: runID, SourceRunID: source.RunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: "agent_hypothesize", ReplayRootRunID: rootRunID,
		ReplayNamespace: runID, ReplayVariable: runmodel.ReplayVariableModel,
		Snapshot: snapshot, ReviewSpec: spec,
	}, nil
}

// InitializePromptReplay derives one formal replay whose sole config change
// is the governed prompt component. The runtime file evidence, workflow,
// target, authority, model, and budgets remain frozen; BuildIdentity changes
// because the admitted Pi manifest closes over the prompt component identity.
func (initializer *Initializer) InitializePromptReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command PromptReplayCommand,
) (InitializedRun, error) {
	return initializer.initializeComponentReplay(ctx, subject, componentReplayCommand(command), componentReplaySpec{
		variable: runmodel.ReplayVariablePrompt, startStage: "agent_hypothesize",
		requireBuildIdentityChange: true, changedFields: FormalPromptReplayChangedFields(),
		validate: validateFormalPromptVariant,
	})
}

// InitializeSkillPackReplay changes only the governed ordered Agent skill set.
// All exact skill bytes are published separately by the composition root
// before dispatch; this initializer freezes their config and lineage identity.
func (initializer *Initializer) InitializeSkillPackReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command SkillPackReplayCommand,
) (InitializedRun, error) {
	return initializer.initializeComponentReplay(ctx, subject, componentReplayCommand(command), componentReplaySpec{
		variable: runmodel.ReplayVariableSkillPack, startStage: "agent_hypothesize",
		requireBuildIdentityChange: true, changedFields: FormalSkillPackReplayChangedFields(),
		validate: validateFormalSkillPackVariant,
	})
}

// InitializeKnowledgePackReplay changes only the governed ordered knowledge
// set after the composition root has published the exact variant bytes.
func (initializer *Initializer) InitializeKnowledgePackReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command KnowledgePackReplayCommand,
) (InitializedRun, error) {
	return initializer.initializeComponentReplay(ctx, subject, componentReplayCommand(command), componentReplaySpec{
		variable: runmodel.ReplayVariableKnowledgePack, startStage: "agent_hypothesize",
		requireBuildIdentityChange: true, changedFields: FormalKnowledgePackReplayChangedFields(),
		validate: validateFormalKnowledgePackVariant,
	})
}

// InitializeRulePackReplay changes only the sealed defect-rule taxonomy that
// the Pi worker consumes as governed review policy. Runtime files, target,
// workflow, authority, model and build identity remain frozen.
func (initializer *Initializer) InitializeRulePackReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command RulePackReplayCommand,
) (InitializedRun, error) {
	return initializer.initializeComponentReplay(ctx, subject, componentReplayCommand(command), componentReplaySpec{
		variable: runmodel.ReplayVariableRulePack, startStage: "agent_hypothesize",
		changedFields: FormalRulePackReplayChangedFields(),
		validate:      validateFormalRulePackVariant,
	})
}

// InitializeWorkflowReplay changes only the exact formal stage resource
// budget. The workflow bytes and their ConfigBundle binding are both frozen;
// build identity, tool authority, target and every runtime artifact remain
// unchanged.
func (initializer *Initializer) InitializeWorkflowReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command WorkflowReplayCommand,
) (InitializedRun, error) {
	definition := command.Definition
	return initializer.initializeComponentReplay(
		ctx,
		subject,
		componentReplayCommand{
			SourceRunID: command.SourceRunID, IdempotencyKey: command.IdempotencyKey,
			BuildIdentity: command.BuildIdentity, CreatedAt: command.CreatedAt,
		},
		componentReplaySpec{
			variable: runmodel.ReplayVariableWorkflow, startStage: "agent_hypothesize",
			workflowDefinition: &definition,
		},
	)
}

// InitializeIndexReplay re-materializes only governed context providers from
// the exact source repository/commit/target paths. The resulting target,
// ReviewInput and provider receipts are frozen before the Pi stage can be
// planned or dispatched.
func (initializer *Initializer) InitializeIndexReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command IndexReplayCommand,
) (InitializedRun, error) {
	if ctx == nil {
		return InitializedRun{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if initializer == nil || initializer.runs == nil || initializer.configs == nil ||
		initializer.indexMaterializer == nil {
		return InitializedRun{}, fmt.Errorf("formal index replay initializer is not initialized")
	}
	if command.SourceRunID == "" || command.IdempotencyKey == "" ||
		command.BuildIdentity == "" || command.CreatedAt.IsZero() ||
		command.CreatedAt.Location() != time.UTC {
		return InitializedRun{}, fmt.Errorf(
			"source formal run, idempotency key, build identity, and UTC created_at are required",
		)
	}
	runID := formalVariantReplayRunID(command.SourceRunID, command.IdempotencyKey)
	sourceRef, err := initializer.runs.CommittedRunRef(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve committed formal index replay source: %w", err)
	}
	source, err := initializer.runs.LoadRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal index replay source: %w", err)
	}
	if source.Status != runmodel.RunStatusSucceeded || source.CompletedAt == nil {
		return InitializedRun{}, fmt.Errorf("formal index replay source must be a succeeded committed run")
	}
	if command.CreatedAt.Before(*source.CompletedAt) {
		return InitializedRun{}, fmt.Errorf("formal index replay created_at precedes source completion")
	}
	sourceSnapshot, err := initializer.runs.ExecutionSnapshotForRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal index replay source snapshot: %w", err)
	}
	if _, workflowErr := initializer.loadFormalWorkflow(sourceSnapshot); workflowErr != nil ||
		len(sourceSnapshot.RuntimeEvidenceRefs) != 1 || sourceSnapshot.RemoteWrites != "deny" ||
		sourceSnapshot.BuildIdentity != command.BuildIdentity {
		return InitializedRun{}, fmt.Errorf("source run is not an exact compatible formal agent execution")
	}
	var sourceBundle reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ConfigBundleRef, &sourceBundle); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal index replay source ConfigBundle: %w", err)
	}
	variant, receipt, err := initializer.configs.ResolvePublishedWithReceipt(ctx, sourceBundle.Context)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve formal index replay variant: %w", err)
	}
	fields, err := validateFormalIndexVariant(sourceBundle, variant, receipt)
	if err != nil {
		return InitializedRun{}, err
	}
	materializationRequest := application.IndexReplayMaterializationRequest{
		SourceRun: source, SourceSnapshot: sourceSnapshot,
		SourceBundle: sourceBundle, VariantBundle: variant,
	}
	if existing, loadErr := initializer.runs.ExecutionSnapshotForRun(runID); loadErr == nil {
		return initializer.recoverExistingIndexReplay(
			ctx, subject, command, source, sourceRef, sourceSnapshot,
			variant, fields, materializationRequest, existing,
		)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return InitializedRun{}, fmt.Errorf("lookup existing formal index replay: %w", loadErr)
	}

	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal index replay source ReviewSpec: %w", err)
	}
	if sourceSpec.TenantID != subject.TenantID || sourceSpec.WorkspaceID != subject.WorkspaceID ||
		sourceSpec.Repository.RepositoryID != subject.RepositoryID || subject.OrganizationID == "" {
		return InitializedRun{}, fmt.Errorf("formal index replay source does not match authenticated subject")
	}
	materialized, err := initializer.indexMaterializer.MaterializeIndexReplay(ctx, materializationRequest)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("materialize formal index replay: %w", err)
	}
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(variant)
	if err != nil {
		return InitializedRun{}, err
	}
	configRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractConfigBundle, variant)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal index replay ConfigBundle: %w", err)
	}
	if configRef.SHA256 != configArtifactDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal index replay ConfigBundle digest changed")
	}
	spec := sourceSpec
	spec.RequestID, spec.IdempotencyKey = runID, runID
	spec.ConfigBundleRef = contractsv1alpha1.VersionedRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: configArtifactDigest,
	}
	if err := spec.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal index replay ReviewSpec: %w", err)
	}
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return InitializedRun{}, err
	}
	specRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal index replay ReviewSpec: %w", err)
	}
	if specRef.SHA256 != specDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal index replay ReviewSpec digest changed")
	}
	rootRunID, parentReplayRunID := source.RunID, ""
	if source.Kind == runmodel.RunKindReplay {
		rootRunID, parentReplayRunID = source.ReplayRootRunID, source.RunID
	}
	change := runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     runID, SourceRunID: source.RunID, RootRunID: rootRunID,
		ParentReplayRunID: parentReplayRunID,
		StartStage:        string(reviewcore.StageMaterializeTarget),
		Variable:          runmodel.ReplayVariableIndex,
		BaselineSHA256:    sourceBundle.SHA256, VariantSHA256: variant.SHA256,
		ChangedFields: slices.Clone(fields), RemoteWrites: "deny", CreatedAt: command.CreatedAt,
	}
	if err := change.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal index replay change set: %w", err)
	}
	changeRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReplayChangeSet, change)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal index replay change set: %w", err)
	}
	snapshot := sourceSnapshot
	snapshot.ExecutionSnapshotID = runID + "-snapshot"
	snapshot.ReviewSpecSHA256, snapshot.ReviewSpecRef = specDigest, specRef
	snapshot.TargetSnapshotRef = materialized.TargetRef
	snapshot.ReviewInputRef = materialized.ReviewInputRef
	snapshot.ContextProviderReceiptRefs = slices.Clone(materialized.ContextProviderReceiptRefs)
	snapshot.ConfigBundleRef = configRef
	snapshot.Config = runmodel.PolicyRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: variant.SHA256,
	}
	snapshot.ReplayInputRefs = []runmodel.ArtifactRef{}
	snapshot.ReplaySourceRunRef, snapshot.ReplayChangeSetRef = &sourceRef, &changeRef
	snapshot.CreatedAt = command.CreatedAt
	if err := snapshot.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal index replay snapshot: %w", err)
	}
	if err := initializer.runs.SaveExecutionSnapshot(snapshot); err != nil {
		return InitializedRun{}, fmt.Errorf("save formal index replay snapshot: %w", err)
	}
	for _, event := range []struct {
		id, eventType string
		status        runmodel.RunStatus
	}{
		{runID + "-created", runrepo.EventRunCreated, runmodel.RunStatusPending},
		{runID + "-started", runrepo.EventRunStarted, runmodel.RunStatusRunning},
	} {
		if err := initializer.runs.AppendEvent(event.id, command.CreatedAt, runrepo.RunEvent{
			RunID: runID, Kind: runmodel.RunKindReplay, Status: event.status,
			EventType: event.eventType, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		}); err != nil {
			return InitializedRun{}, fmt.Errorf("append formal index replay %s: %w", event.eventType, err)
		}
	}
	return InitializedRun{
		RunID: runID, SourceRunID: source.RunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: string(reviewcore.StageMaterializeTarget), ReplayRootRunID: rootRunID,
		ReplayNamespace: runID, ReplayVariable: runmodel.ReplayVariableIndex,
		Snapshot: snapshot, ReviewSpec: spec,
	}, nil
}

// InitializeFilterPolicyReplay changes only the post-verification finding
// governance policy. The Pi build identity remains exact because the policy is
// not part of provider input or runtime component identity.
func (initializer *Initializer) InitializeFilterPolicyReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command FilterPolicyReplayCommand,
) (InitializedRun, error) {
	return initializer.initializeComponentReplay(ctx, subject, componentReplayCommand(command), componentReplaySpec{
		variable: runmodel.ReplayVariableFilterPolicy, startStage: "finding_governance",
		changedFields: FormalFilterPolicyReplayChangedFields(),
		validate: func(baseline, variant reviewconfig.ConfigBundle, receipt reviewconfig.ConfigResolutionReceipt) error {
			return validateFormalFilterPolicyVariantForVariable(
				baseline, variant, receipt, runmodel.ReplayVariableFilterPolicy,
			)
		},
	})
}

// InitializeFindingGovernanceReplay preserves recovery and exact retry for
// historical runs that used the legacy replay variable.
func (initializer *Initializer) InitializeFindingGovernanceReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command FindingGovernanceReplayCommand,
) (InitializedRun, error) {
	return initializer.initializeComponentReplay(ctx, subject, componentReplayCommand(command), componentReplaySpec{
		variable: runmodel.ReplayVariableFindingGovernance, startStage: "finding_governance",
		changedFields: FormalFilterPolicyReplayChangedFields(),
		validate: func(baseline, variant reviewconfig.ConfigBundle, receipt reviewconfig.ConfigResolutionReceipt) error {
			return validateFormalFilterPolicyVariantForVariable(
				baseline, variant, receipt, runmodel.ReplayVariableFindingGovernance,
			)
		},
	})
}

func (initializer *Initializer) initializeComponentReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command componentReplayCommand,
	replaySpec componentReplaySpec,
) (InitializedRun, error) {
	if ctx == nil {
		return InitializedRun{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if initializer == nil || initializer.runs == nil || initializer.configs == nil {
		return InitializedRun{}, fmt.Errorf("formal review initializer is not initialized")
	}
	workflowReplay := replaySpec.variable == runmodel.ReplayVariableWorkflow &&
		replaySpec.workflowDefinition != nil && replaySpec.validate == nil
	if err := replaySpec.variable.Validate(); err != nil || replaySpec.startStage == "" ||
		(!workflowReplay && (replaySpec.validate == nil || len(replaySpec.changedFields) == 0)) {
		return InitializedRun{}, fmt.Errorf("formal component replay specification is invalid")
	}
	if command.SourceRunID == "" || command.IdempotencyKey == "" ||
		command.BuildIdentity == "" || command.CreatedAt.IsZero() ||
		command.CreatedAt.Location() != time.UTC {
		return InitializedRun{}, fmt.Errorf(
			"source formal run, idempotency key, build identity, and UTC created_at are required",
		)
	}
	runID := formalVariantReplayRunID(command.SourceRunID, command.IdempotencyKey)
	sourceRef, err := initializer.runs.CommittedRunRef(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve committed formal component replay source: %w", err)
	}
	source, err := initializer.runs.LoadRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal component replay source: %w", err)
	}
	if source.Status != runmodel.RunStatusSucceeded || source.CompletedAt == nil {
		return InitializedRun{}, fmt.Errorf("formal component replay source must be a succeeded committed run")
	}
	if command.CreatedAt.Before(*source.CompletedAt) {
		return InitializedRun{}, fmt.Errorf("formal component replay created_at precedes source completion")
	}
	sourceSnapshot, err := initializer.runs.ExecutionSnapshotForRun(command.SourceRunID)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("load formal component replay source snapshot: %w", err)
	}
	sourceWorkflow, workflowErr := initializer.loadFormalWorkflow(sourceSnapshot)
	if workflowErr != nil ||
		len(sourceSnapshot.RuntimeEvidenceRefs) != 1 || sourceSnapshot.RemoteWrites != "deny" {
		return InitializedRun{}, fmt.Errorf("source run is not an exact formal agent execution")
	}
	if replaySpec.requireBuildIdentityChange && sourceSnapshot.BuildIdentity == command.BuildIdentity {
		return InitializedRun{}, fmt.Errorf("formal component replay requires a changed Pi build identity")
	}
	if !replaySpec.requireBuildIdentityChange && sourceSnapshot.BuildIdentity != command.BuildIdentity {
		return InitializedRun{}, fmt.Errorf("formal replay variable %q must preserve Pi build identity", replaySpec.variable)
	}
	var sourceBundle reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ConfigBundleRef, &sourceBundle); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal component replay source ConfigBundle: %w", err)
	}
	variant, receipt, err := initializer.configs.ResolvePublishedWithReceipt(ctx, sourceBundle.Context)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("resolve formal component replay variant: %w", err)
	}
	changedFields := slices.Clone(replaySpec.changedFields)
	if workflowReplay {
		changedFields, err = validateFormalWorkflowConfigVariant(
			sourceBundle, variant, receipt, sourceWorkflow, *replaySpec.workflowDefinition,
		)
	} else {
		err = replaySpec.validate(sourceBundle, variant, receipt)
	}
	if err != nil {
		return InitializedRun{}, err
	}
	replaySpec.changedFields = changedFields
	if existing, loadErr := initializer.runs.ExecutionSnapshotForRun(runID); loadErr == nil {
		return initializer.recoverExistingComponentReplay(
			ctx, subject, command, replaySpec, source, sourceRef, sourceSnapshot, variant, existing,
		)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return InitializedRun{}, fmt.Errorf("lookup existing formal component replay: %w", loadErr)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return InitializedRun{}, fmt.Errorf("read formal component replay source ReviewSpec: %w", err)
	}
	if sourceSpec.TenantID != subject.TenantID || sourceSpec.WorkspaceID != subject.WorkspaceID ||
		sourceSpec.Repository.RepositoryID != subject.RepositoryID || subject.OrganizationID == "" {
		return InitializedRun{}, fmt.Errorf("formal component replay source does not match authenticated subject")
	}
	configArtifactDigest, err := reviewconfig.DigestBundleArtifact(variant)
	if err != nil {
		return InitializedRun{}, err
	}
	configRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractConfigBundle, variant)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal component replay ConfigBundle: %w", err)
	}
	if configRef.SHA256 != configArtifactDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal component replay ConfigBundle digest changed")
	}
	spec := sourceSpec
	spec.RequestID, spec.IdempotencyKey = runID, runID
	spec.ConfigBundleRef = contractsv1alpha1.VersionedRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: configArtifactDigest,
	}
	workflowRef := sourceSnapshot.WorkflowDefinitionRef
	if workflowReplay {
		definition := *replaySpec.workflowDefinition
		workflowDigest, digestErr := workflow.DigestDefinition(definition)
		if digestErr != nil {
			return InitializedRun{}, digestErr
		}
		workflowRef, err = initializer.runs.PutJSONArtifact(
			runmodel.ContractWorkflowDefinition, definition,
		)
		if err != nil {
			return InitializedRun{}, fmt.Errorf("persist formal workflow replay definition: %w", err)
		}
		if workflowRef.SHA256 != workflowDigest {
			return InitializedRun{}, fmt.Errorf("persisted workflow replay definition digest changed")
		}
		spec.WorkflowRef = contractsv1alpha1.VersionedRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
		}
	}
	if err := spec.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal component replay ReviewSpec: %w", err)
	}
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return InitializedRun{}, err
	}
	specRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReviewSpec, spec)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal component replay ReviewSpec: %w", err)
	}
	if specRef.SHA256 != specDigest {
		return InitializedRun{}, fmt.Errorf("persisted formal component replay ReviewSpec digest changed")
	}
	rootRunID, parentReplayRunID := source.RunID, ""
	if source.Kind == runmodel.RunKindReplay {
		rootRunID, parentReplayRunID = source.ReplayRootRunID, source.RunID
	}
	change := runmodel.ReplayChangeSet{
		SchemaVersion: runmodel.ReplayChangeSetSchemaVersion,
		Namespace:     runID, SourceRunID: source.RunID, RootRunID: rootRunID,
		ParentReplayRunID: parentReplayRunID, StartStage: replaySpec.startStage,
		Variable: replaySpec.variable, BaselineSHA256: sourceBundle.SHA256,
		VariantSHA256: variant.SHA256, ChangedFields: slices.Clone(replaySpec.changedFields),
		RemoteWrites: "deny", CreatedAt: command.CreatedAt,
	}
	if err := change.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal component replay change set: %w", err)
	}
	changeRef, err := initializer.runs.PutJSONArtifact(runmodel.ContractReplayChangeSet, change)
	if err != nil {
		return InitializedRun{}, fmt.Errorf("persist formal component replay change set: %w", err)
	}
	snapshot := sourceSnapshot
	snapshot.ExecutionSnapshotID = runID + "-snapshot"
	snapshot.ReviewSpecSHA256, snapshot.ReviewSpecRef = specDigest, specRef
	snapshot.ConfigBundleRef = configRef
	snapshot.Config = runmodel.PolicyRef{
		ID: variant.BundleID, Revision: variant.SHA256[:16], SHA256: variant.SHA256,
	}
	if workflowReplay {
		definition := *replaySpec.workflowDefinition
		workflowDigest, _ := workflow.DigestDefinition(definition)
		snapshot.WorkflowDefinitionRef = workflowRef
		snapshot.Workflow = runmodel.WorkflowRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
		}
	}
	snapshot.BuildIdentity = command.BuildIdentity
	snapshot.ReplayInputRefs = []runmodel.ArtifactRef{}
	snapshot.ReplaySourceRunRef, snapshot.ReplayChangeSetRef = &sourceRef, &changeRef
	snapshot.CreatedAt = command.CreatedAt
	if err := snapshot.Validate(); err != nil {
		return InitializedRun{}, fmt.Errorf("validate formal component replay snapshot: %w", err)
	}
	if err := initializer.runs.SaveExecutionSnapshot(snapshot); err != nil {
		return InitializedRun{}, fmt.Errorf("save formal component replay snapshot: %w", err)
	}
	for _, event := range []struct {
		id, eventType string
		status        runmodel.RunStatus
	}{
		{runID + "-created", runrepo.EventRunCreated, runmodel.RunStatusPending},
		{runID + "-started", runrepo.EventRunStarted, runmodel.RunStatusRunning},
	} {
		if err := initializer.runs.AppendEvent(event.id, command.CreatedAt, runrepo.RunEvent{
			RunID: runID, Kind: runmodel.RunKindReplay, Status: event.status,
			EventType: event.eventType, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		}); err != nil {
			return InitializedRun{}, fmt.Errorf("append formal component replay %s: %w", event.eventType, err)
		}
	}
	return InitializedRun{
		RunID: runID, SourceRunID: source.RunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: replaySpec.startStage, ReplayRootRunID: rootRunID,
		ReplayNamespace: runID, ReplayVariable: replaySpec.variable,
		Snapshot: snapshot, ReviewSpec: spec,
	}, nil
}

func (initializer *Initializer) recoverExistingExactReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command ExactReplayCommand,
	source runmodel.ReviewRun,
	sourceRef runmodel.ArtifactRef,
	sourceSnapshot runmodel.ExecutionSnapshot,
	snapshot runmodel.ExecutionSnapshot,
) (InitializedRun, error) {
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if snapshot.ReplaySourceRunRef == nil || *snapshot.ReplaySourceRunRef != sourceRef ||
		snapshot.ReplayChangeSetRef == nil || len(snapshot.ReplayInputRefs) != 0 ||
		snapshot.TargetSnapshotRef != sourceSnapshot.TargetSnapshotRef ||
		snapshot.ReviewInputRef != sourceSnapshot.ReviewInputRef ||
		snapshot.ConfigBundleRef != sourceSnapshot.ConfigBundleRef ||
		snapshot.WorkflowDefinitionRef != sourceSnapshot.WorkflowDefinitionRef ||
		snapshot.RuntimeProfile != sourceSnapshot.RuntimeProfile ||
		snapshot.BuildIdentity != sourceSnapshot.BuildIdentity ||
		!slices.Equal(snapshot.RuntimeEvidenceRefs, sourceSnapshot.RuntimeEvidenceRefs) {
		return InitializedRun{}, fmt.Errorf("existing formal replay conflicts with exact source closure")
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return InitializedRun{}, err
	}
	if spec.TenantID != subject.TenantID || spec.WorkspaceID != subject.WorkspaceID ||
		spec.Repository.RepositoryID != subject.RepositoryID || spec.RequestID != spec.IdempotencyKey {
		return InitializedRun{}, fmt.Errorf("existing formal replay escaped subject or idempotency binding")
	}
	rootRunID := source.RunID
	if source.Kind == runmodel.RunKindReplay {
		rootRunID = source.ReplayRootRunID
	}
	return InitializedRun{
		RunID: spec.RequestID, SourceRunID: command.SourceRunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: "agent_hypothesize", ReplayRootRunID: rootRunID,
		ReplayNamespace: spec.RequestID, ReplayVariable: runmodel.ReplayVariableNone,
		Snapshot: snapshot, ReviewSpec: spec, Recovered: true,
	}, nil
}

func (initializer *Initializer) recoverExistingBudgetReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command BudgetReplayCommand,
	source runmodel.ReviewRun,
	sourceRef runmodel.ArtifactRef,
	sourceSnapshot runmodel.ExecutionSnapshot,
	variant reviewconfig.ConfigBundle,
	snapshot runmodel.ExecutionSnapshot,
) (InitializedRun, error) {
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if snapshot.ReplaySourceRunRef == nil || *snapshot.ReplaySourceRunRef != sourceRef ||
		snapshot.ReplayChangeSetRef == nil || len(snapshot.ReplayInputRefs) != 0 ||
		snapshot.TargetSnapshotRef != sourceSnapshot.TargetSnapshotRef ||
		snapshot.ReviewInputRef != sourceSnapshot.ReviewInputRef ||
		snapshot.Config.SHA256 != variant.SHA256 ||
		snapshot.RuntimeProfile != sourceSnapshot.RuntimeProfile ||
		snapshot.BuildIdentity != sourceSnapshot.BuildIdentity ||
		!slices.Equal(snapshot.RuntimeEvidenceRefs, sourceSnapshot.RuntimeEvidenceRefs) ||
		snapshot.ToolPolicy.PerCallTimeoutMS != variant.Budget.StageTimeoutMS {
		return InitializedRun{}, fmt.Errorf("existing formal budget replay conflicts with variant closure")
	}
	var persistedVariant reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &persistedVariant); err != nil {
		return InitializedRun{}, err
	}
	if !reflect.DeepEqual(persistedVariant, variant) {
		return InitializedRun{}, fmt.Errorf("existing formal budget replay changed variant ConfigBundle")
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return InitializedRun{}, err
	}
	if spec.TenantID != subject.TenantID || spec.WorkspaceID != subject.WorkspaceID ||
		spec.Repository.RepositoryID != subject.RepositoryID || spec.RequestID != spec.IdempotencyKey {
		return InitializedRun{}, fmt.Errorf("existing formal budget replay escaped subject or idempotency binding")
	}
	var change runmodel.ReplayChangeSet
	if err := initializer.runs.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		return InitializedRun{}, err
	}
	rootRunID := source.RunID
	if source.Kind == runmodel.RunKindReplay {
		rootRunID = source.ReplayRootRunID
	}
	if change.Namespace != spec.RequestID || change.SourceRunID != command.SourceRunID ||
		change.RootRunID != rootRunID || change.Variable != runmodel.ReplayVariableBudget ||
		change.VariantSHA256 != variant.SHA256 ||
		!slices.Equal(change.ChangedFields, FormalBudgetReplayChangedFields()) {
		return InitializedRun{}, fmt.Errorf("existing formal budget replay changed ReplayChangeSet")
	}
	return InitializedRun{
		RunID: spec.RequestID, SourceRunID: command.SourceRunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: "agent_hypothesize", ReplayRootRunID: rootRunID,
		ReplayNamespace: spec.RequestID, ReplayVariable: runmodel.ReplayVariableBudget,
		Snapshot: snapshot, ReviewSpec: spec, Recovered: true,
	}, nil
}

func (initializer *Initializer) recoverExistingModelReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command ModelReplayCommand,
	source runmodel.ReviewRun,
	sourceRef runmodel.ArtifactRef,
	sourceSnapshot runmodel.ExecutionSnapshot,
	variant reviewconfig.ConfigBundle,
	snapshot runmodel.ExecutionSnapshot,
) (InitializedRun, error) {
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if snapshot.ReplaySourceRunRef == nil || *snapshot.ReplaySourceRunRef != sourceRef ||
		snapshot.ReplayChangeSetRef == nil || len(snapshot.ReplayInputRefs) != 0 ||
		snapshot.TargetSnapshotRef != sourceSnapshot.TargetSnapshotRef ||
		snapshot.ReviewInputRef != sourceSnapshot.ReviewInputRef ||
		snapshot.Config.SHA256 != variant.SHA256 ||
		snapshot.RuntimeProfile != sourceSnapshot.RuntimeProfile ||
		snapshot.BuildIdentity != command.BuildIdentity ||
		snapshot.CreatedAt != command.CreatedAt ||
		!slices.Equal(snapshot.RuntimeEvidenceRefs, sourceSnapshot.RuntimeEvidenceRefs) ||
		!reflect.DeepEqual(snapshot.ToolPolicy, sourceSnapshot.ToolPolicy) {
		return InitializedRun{}, fmt.Errorf("existing formal model replay conflicts with variant closure")
	}
	var persistedVariant reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &persistedVariant); err != nil {
		return InitializedRun{}, err
	}
	if !reflect.DeepEqual(persistedVariant, variant) {
		return InitializedRun{}, fmt.Errorf("existing formal model replay changed variant ConfigBundle")
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return InitializedRun{}, err
	}
	if spec.TenantID != subject.TenantID || spec.WorkspaceID != subject.WorkspaceID ||
		spec.Repository.RepositoryID != subject.RepositoryID || spec.RequestID != spec.IdempotencyKey {
		return InitializedRun{}, fmt.Errorf("existing formal model replay escaped subject or idempotency binding")
	}
	var change runmodel.ReplayChangeSet
	if err := initializer.runs.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		return InitializedRun{}, err
	}
	rootRunID := source.RunID
	if source.Kind == runmodel.RunKindReplay {
		rootRunID = source.ReplayRootRunID
	}
	if change.Namespace != spec.RequestID || change.SourceRunID != command.SourceRunID ||
		change.RootRunID != rootRunID || change.Variable != runmodel.ReplayVariableModel ||
		change.VariantSHA256 != variant.SHA256 || change.CreatedAt != command.CreatedAt ||
		!slices.Equal(change.ChangedFields, FormalModelReplayChangedFields()) {
		return InitializedRun{}, fmt.Errorf("existing formal model replay changed ReplayChangeSet")
	}
	return InitializedRun{
		RunID: spec.RequestID, SourceRunID: command.SourceRunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: "agent_hypothesize", ReplayRootRunID: rootRunID,
		ReplayNamespace: spec.RequestID, ReplayVariable: runmodel.ReplayVariableModel,
		Snapshot: snapshot, ReviewSpec: spec, Recovered: true,
	}, nil
}

func (initializer *Initializer) recoverExistingComponentReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command componentReplayCommand,
	replaySpec componentReplaySpec,
	source runmodel.ReviewRun,
	sourceRef runmodel.ArtifactRef,
	sourceSnapshot runmodel.ExecutionSnapshot,
	variant reviewconfig.ConfigBundle,
	snapshot runmodel.ExecutionSnapshot,
) (InitializedRun, error) {
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if snapshot.ReplaySourceRunRef == nil || *snapshot.ReplaySourceRunRef != sourceRef ||
		snapshot.ReplayChangeSetRef == nil || len(snapshot.ReplayInputRefs) != 0 ||
		snapshot.TargetSnapshotRef != sourceSnapshot.TargetSnapshotRef ||
		snapshot.ReviewInputRef != sourceSnapshot.ReviewInputRef ||
		snapshot.Config.SHA256 != variant.SHA256 ||
		snapshot.RuntimeProfile != sourceSnapshot.RuntimeProfile ||
		snapshot.BuildIdentity != command.BuildIdentity ||
		snapshot.CreatedAt != command.CreatedAt ||
		!slices.Equal(snapshot.RuntimeEvidenceRefs, sourceSnapshot.RuntimeEvidenceRefs) ||
		!reflect.DeepEqual(snapshot.ToolPolicy, sourceSnapshot.ToolPolicy) {
		return InitializedRun{}, fmt.Errorf("existing formal component replay conflicts with variant closure")
	}
	workflowReplay := replaySpec.variable == runmodel.ReplayVariableWorkflow &&
		replaySpec.workflowDefinition != nil
	if !workflowReplay && (snapshot.WorkflowDefinitionRef != sourceSnapshot.WorkflowDefinitionRef ||
		snapshot.Workflow != sourceSnapshot.Workflow) {
		return InitializedRun{}, fmt.Errorf("existing formal component replay changed workflow")
	}
	if workflowReplay {
		definition := *replaySpec.workflowDefinition
		digest, err := workflow.DigestDefinition(definition)
		if err != nil {
			return InitializedRun{}, err
		}
		if snapshot.WorkflowDefinitionRef == sourceSnapshot.WorkflowDefinitionRef ||
			snapshot.WorkflowDefinitionRef.SHA256 != digest ||
			snapshot.Workflow != (runmodel.WorkflowRef{
				ID: definition.ID, Revision: definition.Revision, SHA256: digest,
			}) {
			return InitializedRun{}, fmt.Errorf("existing formal workflow replay changed exact definition")
		}
		var persisted workflow.Definition
		if err := initializer.runs.ReadJSONArtifact(snapshot.WorkflowDefinitionRef, &persisted); err != nil {
			return InitializedRun{}, err
		}
		if !reflect.DeepEqual(persisted, definition) {
			return InitializedRun{}, fmt.Errorf("existing formal workflow replay definition bytes changed")
		}
	}
	var persistedVariant reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &persistedVariant); err != nil {
		return InitializedRun{}, err
	}
	if !reflect.DeepEqual(persistedVariant, variant) {
		return InitializedRun{}, fmt.Errorf("existing formal component replay changed variant ConfigBundle")
	}
	var reviewSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &reviewSpec); err != nil {
		return InitializedRun{}, err
	}
	if reviewSpec.TenantID != subject.TenantID || reviewSpec.WorkspaceID != subject.WorkspaceID ||
		reviewSpec.Repository.RepositoryID != subject.RepositoryID || reviewSpec.RequestID != reviewSpec.IdempotencyKey {
		return InitializedRun{}, fmt.Errorf("existing formal component replay escaped subject or idempotency binding")
	}
	if workflowReplay {
		definition := *replaySpec.workflowDefinition
		digest, _ := workflow.DigestDefinition(definition)
		if reviewSpec.WorkflowRef != (contractsv1alpha1.VersionedRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: digest,
		}) {
			return InitializedRun{}, fmt.Errorf("existing formal workflow replay ReviewSpec changed")
		}
	}
	var change runmodel.ReplayChangeSet
	if err := initializer.runs.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		return InitializedRun{}, err
	}
	rootRunID := source.RunID
	if source.Kind == runmodel.RunKindReplay {
		rootRunID = source.ReplayRootRunID
	}
	if change.Namespace != reviewSpec.RequestID || change.SourceRunID != command.SourceRunID ||
		change.RootRunID != rootRunID || change.Variable != replaySpec.variable ||
		change.VariantSHA256 != variant.SHA256 || change.CreatedAt != command.CreatedAt ||
		!slices.Equal(change.ChangedFields, replaySpec.changedFields) {
		return InitializedRun{}, fmt.Errorf("existing formal component replay changed ReplayChangeSet")
	}
	return InitializedRun{
		RunID: reviewSpec.RequestID, SourceRunID: command.SourceRunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: replaySpec.startStage, ReplayRootRunID: rootRunID,
		ReplayNamespace: reviewSpec.RequestID, ReplayVariable: replaySpec.variable,
		Snapshot: snapshot, ReviewSpec: reviewSpec, Recovered: true,
	}, nil
}

func (initializer *Initializer) recoverExistingIndexReplay(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	command IndexReplayCommand,
	source runmodel.ReviewRun,
	sourceRef runmodel.ArtifactRef,
	sourceSnapshot runmodel.ExecutionSnapshot,
	variant reviewconfig.ConfigBundle,
	fields []string,
	materializationRequest application.IndexReplayMaterializationRequest,
	snapshot runmodel.ExecutionSnapshot,
) (InitializedRun, error) {
	if err := ctx.Err(); err != nil {
		return InitializedRun{}, err
	}
	if snapshot.ReplaySourceRunRef == nil || *snapshot.ReplaySourceRunRef != sourceRef ||
		snapshot.ReplayChangeSetRef == nil || len(snapshot.ReplayInputRefs) != 0 ||
		snapshot.TargetSnapshotRef == sourceSnapshot.TargetSnapshotRef ||
		snapshot.ReviewInputRef == sourceSnapshot.ReviewInputRef ||
		snapshot.Config.SHA256 != variant.SHA256 ||
		snapshot.RuntimeProfile != sourceSnapshot.RuntimeProfile ||
		snapshot.BuildIdentity != command.BuildIdentity ||
		snapshot.CreatedAt != command.CreatedAt ||
		!slices.Equal(snapshot.RuntimeEvidenceRefs, sourceSnapshot.RuntimeEvidenceRefs) ||
		!reflect.DeepEqual(snapshot.ToolPolicy, sourceSnapshot.ToolPolicy) ||
		len(snapshot.ContextProviderReceiptRefs) != len(variant.Execution.ContextProviders) {
		return InitializedRun{}, fmt.Errorf("existing formal index replay conflicts with variant closure")
	}
	var persistedVariant reviewconfig.ConfigBundle
	if err := initializer.runs.ReadJSONArtifact(snapshot.ConfigBundleRef, &persistedVariant); err != nil {
		return InitializedRun{}, err
	}
	if !reflect.DeepEqual(persistedVariant, variant) {
		return InitializedRun{}, fmt.Errorf("existing formal index replay changed variant ConfigBundle")
	}
	var reviewSpec contractsv1alpha1.ReviewSpec
	if err := initializer.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &reviewSpec); err != nil {
		return InitializedRun{}, err
	}
	if reviewSpec.TenantID != subject.TenantID || reviewSpec.WorkspaceID != subject.WorkspaceID ||
		reviewSpec.Repository.RepositoryID != subject.RepositoryID ||
		reviewSpec.RequestID != reviewSpec.IdempotencyKey {
		return InitializedRun{}, fmt.Errorf("existing formal index replay escaped subject or idempotency binding")
	}
	var change runmodel.ReplayChangeSet
	if err := initializer.runs.ReadJSONArtifact(*snapshot.ReplayChangeSetRef, &change); err != nil {
		return InitializedRun{}, err
	}
	rootRunID := source.RunID
	if source.Kind == runmodel.RunKindReplay {
		rootRunID = source.ReplayRootRunID
	}
	if change.Namespace != reviewSpec.RequestID || change.SourceRunID != command.SourceRunID ||
		change.RootRunID != rootRunID || change.Variable != runmodel.ReplayVariableIndex ||
		change.StartStage != string(reviewcore.StageMaterializeTarget) ||
		change.VariantSHA256 != variant.SHA256 || change.CreatedAt != command.CreatedAt ||
		!slices.Equal(change.ChangedFields, fields) {
		return InitializedRun{}, fmt.Errorf("existing formal index replay changed ReplayChangeSet")
	}
	materialized := application.IndexReplayMaterialization{
		TargetRef: snapshot.TargetSnapshotRef, ReviewInputRef: snapshot.ReviewInputRef,
		ContextProviderReceiptRefs: slices.Clone(snapshot.ContextProviderReceiptRefs),
	}
	if err := initializer.runs.ReadJSONArtifact(snapshot.ReviewInputRef, &materialized.ReviewInput); err != nil {
		return InitializedRun{}, err
	}
	if err := initializer.indexMaterializer.ValidateIndexReplayMaterialization(
		ctx, materializationRequest, materialized,
	); err != nil {
		return InitializedRun{}, fmt.Errorf("validate existing formal index replay materialization: %w", err)
	}
	return InitializedRun{
		RunID: reviewSpec.RequestID, SourceRunID: command.SourceRunID, Kind: runmodel.RunKindReplay,
		ReplayFromStage: string(reviewcore.StageMaterializeTarget), ReplayRootRunID: rootRunID,
		ReplayNamespace: reviewSpec.RequestID, ReplayVariable: runmodel.ReplayVariableIndex,
		Snapshot: snapshot, ReviewSpec: reviewSpec, Recovered: true,
	}, nil
}

func validateFormalIndexVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) ([]string, error) {
	if err := receipt.ValidateAgainst(variant); err != nil {
		return nil, fmt.Errorf("validate formal index variant receipt: %w", err)
	}
	fields, err := reviewconfig.DiffReplayIndexPolicy(
		baseline.Execution, variant.Execution,
	)
	if err != nil {
		return nil, fmt.Errorf("validate formal index variant: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID ||
		binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableIndex) ||
		!slices.Equal(binding.ChangedFields, fields) {
		return nil, fmt.Errorf("formal index variant receipt does not bind baseline and atomic change")
	}
	baselineCopy, variantCopy := baseline, variant
	baselineCopy.Execution.ContextProviders = nil
	variantCopy.Execution.ContextProviders = nil
	baselineCopy.BundleID, baselineCopy.SHA256 = "", ""
	variantCopy.BundleID, variantCopy.SHA256 = "", ""
	if !reflect.DeepEqual(baselineCopy, variantCopy) {
		return nil, fmt.Errorf("formal index replay must change context providers only")
	}
	return fields, nil
}

func validateFormalBudgetVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) error {
	if err := receipt.ValidateAgainst(variant); err != nil {
		return fmt.Errorf("validate formal budget variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID || binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableBudget) ||
		!slices.Equal(binding.ChangedFields, FormalBudgetReplayChangedFields()) {
		return fmt.Errorf("formal budget variant receipt does not bind baseline and atomic change")
	}
	if baseline.Context != variant.Context ||
		!slices.Equal(baseline.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(baseline.Target, variant.Target) ||
		!reflect.DeepEqual(baseline.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(baseline.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(baseline.Execution, variant.Execution) ||
		!reflect.DeepEqual(baseline.Verification, variant.Verification) ||
		!reflect.DeepEqual(baseline.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(baseline.Publication, variant.Publication) ||
		!reflect.DeepEqual(baseline.Data, variant.Data) ||
		!reflect.DeepEqual(baseline.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(baseline.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(baseline.Explain, variant.Explain) ||
		baseline.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal budget replay changed a non-budget config fact")
	}
	baselineBudget, variantBudget := baseline.Budget, variant.Budget
	baselineBudget.StageTimeoutMS, variantBudget.StageTimeoutMS = 0, 0
	if baseline.Budget.StageTimeoutMS == variant.Budget.StageTimeoutMS ||
		!reflect.DeepEqual(baselineBudget, variantBudget) {
		return fmt.Errorf("formal budget replay must change stage_timeout_ms only")
	}
	baselinePolicy, variantPolicy := *baseline.AgentReview, *variant.AgentReview
	baselinePolicy.SHA256, variantPolicy.SHA256 = "", ""
	baselinePolicy.Budget.TimeoutMS, variantPolicy.Budget.TimeoutMS = 0, 0
	if baseline.AgentReview.Budget.TimeoutMS == variant.AgentReview.Budget.TimeoutMS ||
		!reflect.DeepEqual(baselinePolicy, variantPolicy) ||
		variant.Budget.StageTimeoutMS != variant.AgentReview.Budget.TimeoutMS {
		return fmt.Errorf("formal budget replay must change agent timeout atomically")
	}
	return nil
}

func validateFormalModelVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) error {
	if err := receipt.ValidateAgainst(variant); err != nil {
		return fmt.Errorf("validate formal model variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID || binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableModel) ||
		!slices.Equal(binding.ChangedFields, FormalModelReplayChangedFields()) {
		return fmt.Errorf("formal model variant receipt does not bind baseline and atomic change")
	}
	if baseline.Context != variant.Context ||
		!slices.Equal(baseline.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(baseline.Target, variant.Target) ||
		!reflect.DeepEqual(baseline.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(baseline.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(baseline.Execution, variant.Execution) ||
		!reflect.DeepEqual(baseline.Budget, variant.Budget) ||
		!reflect.DeepEqual(baseline.Verification, variant.Verification) ||
		!reflect.DeepEqual(baseline.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(baseline.Publication, variant.Publication) ||
		!reflect.DeepEqual(baseline.Data, variant.Data) ||
		!reflect.DeepEqual(baseline.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(baseline.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(baseline.Explain, variant.Explain) ||
		baseline.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal model replay changed a non-model config fact")
	}
	baselinePolicy, variantPolicy := *baseline.AgentReview, *variant.AgentReview
	baselineModel, variantModel := baselinePolicy.Model, variantPolicy.Model
	baselinePolicy.Model, variantPolicy.Model = reviewconfig.VersionedRef{}, reviewconfig.VersionedRef{}
	baselinePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if baselineModel == variantModel || !reflect.DeepEqual(baselinePolicy, variantPolicy) {
		return fmt.Errorf("formal model replay must change model component only")
	}
	return nil
}

func validateFormalPromptVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) error {
	if err := receipt.ValidateAgainst(variant); err != nil {
		return fmt.Errorf("validate formal prompt variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID ||
		binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariablePrompt) ||
		!slices.Equal(binding.ChangedFields, FormalPromptReplayChangedFields()) {
		return fmt.Errorf("formal prompt variant receipt does not bind baseline and atomic change")
	}
	if baseline.Context != variant.Context ||
		!slices.Equal(baseline.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(baseline.Target, variant.Target) ||
		!reflect.DeepEqual(baseline.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(baseline.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(baseline.Execution, variant.Execution) ||
		!reflect.DeepEqual(baseline.Budget, variant.Budget) ||
		!reflect.DeepEqual(baseline.Verification, variant.Verification) ||
		!reflect.DeepEqual(baseline.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(baseline.Publication, variant.Publication) ||
		!reflect.DeepEqual(baseline.Data, variant.Data) ||
		!reflect.DeepEqual(baseline.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(baseline.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(baseline.Explain, variant.Explain) ||
		baseline.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal prompt replay changed a non-prompt config fact")
	}
	baselinePolicy, variantPolicy := *baseline.AgentReview, *variant.AgentReview
	baselinePrompt, variantPrompt := baselinePolicy.Prompt, variantPolicy.Prompt
	baselinePolicy.Prompt, variantPolicy.Prompt = reviewconfig.VersionedRef{}, reviewconfig.VersionedRef{}
	baselinePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if baselinePrompt == variantPrompt || !reflect.DeepEqual(baselinePolicy, variantPolicy) {
		return fmt.Errorf("formal prompt replay must change prompt component only")
	}
	return nil
}

func validateFormalSkillPackVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) error {
	if err := receipt.ValidateAgainst(variant); err != nil {
		return fmt.Errorf("validate formal skill_pack variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID ||
		binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableSkillPack) ||
		!slices.Equal(binding.ChangedFields, FormalSkillPackReplayChangedFields()) {
		return fmt.Errorf("formal skill_pack variant receipt does not bind baseline and atomic change")
	}
	if baseline.Context != variant.Context ||
		!slices.Equal(baseline.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(baseline.Target, variant.Target) ||
		!reflect.DeepEqual(baseline.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(baseline.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(baseline.Execution, variant.Execution) ||
		!reflect.DeepEqual(baseline.Budget, variant.Budget) ||
		!reflect.DeepEqual(baseline.Verification, variant.Verification) ||
		!reflect.DeepEqual(baseline.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(baseline.Publication, variant.Publication) ||
		!reflect.DeepEqual(baseline.Data, variant.Data) ||
		!reflect.DeepEqual(baseline.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(baseline.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(baseline.Explain, variant.Explain) ||
		baseline.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal skill_pack replay changed a non-skill config fact")
	}
	baselinePolicy, variantPolicy := *baseline.AgentReview, *variant.AgentReview
	baselineSkills, variantSkills := baselinePolicy.SkillPacks, variantPolicy.SkillPacks
	baselinePolicy.SkillPacks, variantPolicy.SkillPacks = nil, nil
	baselinePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if len(variantSkills) == 0 || reflect.DeepEqual(baselineSkills, variantSkills) ||
		!reflect.DeepEqual(baselinePolicy, variantPolicy) {
		return fmt.Errorf("formal skill_pack replay must change governed skills only")
	}
	return nil
}

func validateFormalKnowledgePackVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) error {
	if err := receipt.ValidateAgainst(variant); err != nil {
		return fmt.Errorf("validate formal knowledge_pack variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID ||
		binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableKnowledgePack) ||
		!slices.Equal(binding.ChangedFields, FormalKnowledgePackReplayChangedFields()) {
		return fmt.Errorf("formal knowledge_pack variant receipt does not bind baseline and atomic change")
	}
	if baseline.Context != variant.Context ||
		!slices.Equal(baseline.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(baseline.Target, variant.Target) ||
		!reflect.DeepEqual(baseline.RulePack, variant.RulePack) ||
		!reflect.DeepEqual(baseline.Workflow, variant.Workflow) ||
		!reflect.DeepEqual(baseline.Execution, variant.Execution) ||
		!reflect.DeepEqual(baseline.Budget, variant.Budget) ||
		!reflect.DeepEqual(baseline.Verification, variant.Verification) ||
		!reflect.DeepEqual(baseline.Adjudication, variant.Adjudication) ||
		!reflect.DeepEqual(baseline.Publication, variant.Publication) ||
		!reflect.DeepEqual(baseline.Data, variant.Data) ||
		!reflect.DeepEqual(baseline.FindingGovernance, variant.FindingGovernance) ||
		!reflect.DeepEqual(baseline.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(baseline.Explain, variant.Explain) ||
		baseline.AgentReview == nil || variant.AgentReview == nil {
		return fmt.Errorf("formal knowledge_pack replay changed a non-knowledge config fact")
	}
	baselinePolicy, variantPolicy := *baseline.AgentReview, *variant.AgentReview
	baselineKnowledge, variantKnowledge := baselinePolicy.KnowledgePacks, variantPolicy.KnowledgePacks
	baselinePolicy.KnowledgePacks, variantPolicy.KnowledgePacks = nil, nil
	baselinePolicy.SHA256, variantPolicy.SHA256 = "", ""
	if len(variantKnowledge) == 0 || reflect.DeepEqual(baselineKnowledge, variantKnowledge) ||
		!reflect.DeepEqual(baselinePolicy, variantPolicy) {
		return fmt.Errorf("formal knowledge_pack replay must change governed knowledge only")
	}
	return nil
}

func validateFormalRulePackVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) error {
	if err := receipt.ValidateAgainst(variant); err != nil {
		return fmt.Errorf("validate formal rule_pack variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID ||
		binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(runmodel.ReplayVariableRulePack) ||
		!slices.Equal(binding.ChangedFields, FormalRulePackReplayChangedFields()) {
		return fmt.Errorf("formal rule_pack variant receipt does not bind baseline and atomic change")
	}
	baselinePack, variantPack := baseline.RulePack, variant.RulePack
	baseline.RulePack, variant.RulePack = reviewconfig.RulePack{}, reviewconfig.RulePack{}
	baseline.BundleID, baseline.SHA256 = "", ""
	variant.BundleID, variant.SHA256 = "", ""
	if reflect.DeepEqual(baselinePack, variantPack) || !reflect.DeepEqual(baseline, variant) {
		return fmt.Errorf("formal rule_pack replay must change the sealed rule pack only")
	}
	return nil
}

func validateFormalFilterPolicyVariant(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,

) error {
	return validateFormalFilterPolicyVariantForVariable(
		baseline, variant, receipt, runmodel.ReplayVariableFindingGovernance,
	)
}

func validateFormalFilterPolicyVariantForVariable(
	baseline reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
	variable runmodel.ReplayVariable,
) error {
	if !variable.IsFilterPolicy() {
		return fmt.Errorf("formal filter_policy replay variable is invalid")
	}
	if err := receipt.ValidateAgainst(variant); err != nil {
		return fmt.Errorf("validate formal filter_policy variant receipt: %w", err)
	}
	binding := receipt.ReplayVariant
	if receipt.Origin != reviewconfig.ConfigResolutionOriginReplayVariant || binding == nil ||
		binding.BaselineBundleID != baseline.BundleID ||
		binding.BaselineSHA256 != baseline.SHA256 ||
		binding.Variable != string(variable) ||
		!slices.Equal(binding.ChangedFields, FormalFilterPolicyReplayChangedFields()) {
		return fmt.Errorf("formal filter_policy variant receipt does not bind baseline and atomic change")
	}
	baselineCopy, variantCopy := baseline, variant
	baselinePolicy, variantPolicy := baselineCopy.FindingGovernance, variantCopy.FindingGovernance
	baselineCopy.FindingGovernance, variantCopy.FindingGovernance = nil, nil
	baselineCopy.BundleID, baselineCopy.SHA256 = "", ""
	variantCopy.BundleID, variantCopy.SHA256 = "", ""
	if variantPolicy == nil || reflect.DeepEqual(baselinePolicy, variantPolicy) ||
		!reflect.DeepEqual(baselineCopy, variantCopy) {
		return fmt.Errorf("formal filter_policy replay must change finding governance only")
	}
	return nil
}

func (initializer *Initializer) loadFormalWorkflow(
	snapshot runmodel.ExecutionSnapshot,
) (workflow.Definition, error) {
	var definition workflow.Definition
	if err := initializer.runs.ReadJSONArtifact(
		snapshot.WorkflowDefinitionRef, &definition,
	); err != nil {
		return workflow.Definition{}, fmt.Errorf("read formal WorkflowDefinition: %w", err)
	}
	digest, err := workflow.DigestDefinition(definition)
	if err != nil {
		return workflow.Definition{}, fmt.Errorf("validate formal WorkflowDefinition: %w", err)
	}
	if snapshot.WorkflowDefinitionRef.Contract != runmodel.ContractWorkflowDefinition ||
		snapshot.WorkflowDefinitionRef.SHA256 != digest ||
		snapshot.Workflow != (runmodel.WorkflowRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: digest,
		}) || definition.ID != initializer.workflow.ID {
		return workflow.Definition{}, fmt.Errorf("formal workflow artifact identity changed")
	}
	if definition.Revision == initializer.workflow.Revision {
		if !reflect.DeepEqual(definition, initializer.workflow) {
			return workflow.Definition{}, fmt.Errorf("canonical formal workflow bytes changed")
		}
		return definition, nil
	}
	fields, earliest, err := workflow.DiffReplayExecutionPolicy(initializer.workflow, definition)
	if err != nil || earliest != "agent_hypothesize" {
		return workflow.Definition{}, fmt.Errorf("formal workflow is not an admitted budget variant")
	}
	for _, field := range fields {
		if !strings.HasPrefix(field, "workflow.stages.agent_hypothesize.budget.") {
			return workflow.Definition{}, fmt.Errorf("formal workflow changes a non-budget field")
		}
	}
	return definition, nil
}

// FormalRunID derives the immutable normal formal-review run identity. It is
// exported so durable admission can bind its scheduling workload to the exact
// run the initializer will create, rather than wrapping formal execution in a
// second workload.
func FormalRunID(sourceRunID string, idempotencyKey string) string {
	payload, _ := json.Marshal([]string{sourceRunID, idempotencyKey})
	digest := sha256.Sum256(payload)
	return "formal-" + hex.EncodeToString(digest[:12])
}

func formalReplayRunID(sourceRunID string, idempotencyKey string) string {
	payload, _ := json.Marshal([]string{"exact-replay", sourceRunID, idempotencyKey})
	digest := sha256.Sum256(payload)
	return "formal-replay-" + hex.EncodeToString(digest[:12])
}

func formalVariantReplayRunID(sourceRunID string, idempotencyKey string) string {
	payload, _ := json.Marshal([]string{"variant-replay", sourceRunID, idempotencyKey})
	digest := sha256.Sum256(payload)
	return "formal-variant-" + hex.EncodeToString(digest[:12])
}
