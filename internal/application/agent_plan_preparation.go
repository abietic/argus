package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"argus.local/argus/internal/agentplan"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/targetmodel"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

// AgentArtifactProjectionRequest contains only application-owned publication
// facts. The adapter fixes authority, roles, allowed uses, actor, and audit
// text; none of those authority-bearing values come from a caller request.
type AgentArtifactProjectionRequest struct {
	Role         string
	ReviewRunID  string
	StageID      string
	Source       runmodel.ArtifactRef
	ExactContent []byte
	FrozenAt     time.Time
}

// AgentPlanArtifactRepository is the application port for the authoritative
// frozen run closure and its read-only execution aliases.
type AgentPlanArtifactRepository interface {
	CommittedRunRef(context.Context, string) (runmodel.ArtifactRef, error)
	ExecutionSnapshotForRun(context.Context, string) (runmodel.ExecutionSnapshot, error)
	ReadLocalArtifact(context.Context, runmodel.ArtifactRef) ([]byte, error)
	PutLocalArtifact(
		context.Context,
		string,
		[]byte,
	) (runmodel.ArtifactRef, error)
	ProjectReadOnly(
		context.Context,
		AgentPlanningSubject,
		AgentArtifactProjectionRequest,
	) (contractsv1alpha1.ArtifactBinding, error)
	VerifyRead(
		context.Context,
		AgentPlanningSubject,
		contractsv1alpha1.ArtifactBinding,
	) ([]byte, error)
}

// AgentStagePlanAdmissionRepository owns the append-only commit point. A
// structurally valid or even published plan is not dispatchable without this
// fact.
type AgentStagePlanAdmissionRepository interface {
	LookupAgentStagePlanAdmission(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStagePlanAdmission, bool, error)
	AppendAgentStagePlanAdmission(
		context.Context,
		runmodel.AgentStagePlanAdmission,
	) error
}

type PrepareAgentStageCommand struct {
	ReviewRunID string
	StageID     string
}

type PreparedAgentStage struct {
	Plan      contractsv1alpha1.AgentStagePlan
	Admission runmodel.AgentStagePlanAdmission
}

// AgentStagePreparer is the sole S2 application entry allowed to commit a
// formal AgentStagePlan admission. Its dependencies are ports so concrete
// run/artifact repositories remain adapters around the application layer.
type AgentStagePreparer struct {
	provider          GovernedConfigProvider
	componentResolver AgentComponentResolver
	artifacts         AgentPlanArtifactRepository
	admissions        AgentStagePlanAdmissionRepository
	now               func() time.Time
}

func NewAgentStagePreparer(
	provider GovernedConfigProvider,
	componentResolver AgentComponentResolver,
	artifacts AgentPlanArtifactRepository,
	admissions AgentStagePlanAdmissionRepository,
	now func() time.Time,
) (*AgentStagePreparer, error) {
	if provider == nil || componentResolver == nil || artifacts == nil || admissions == nil {
		return nil, fmt.Errorf(
			"governed config, component, artifact, and admission ports are required",
		)
	}
	if now == nil {
		now = time.Now
	}
	return &AgentStagePreparer{
		provider: provider, componentResolver: componentResolver,
		artifacts: artifacts, admissions: admissions, now: now,
	}, nil
}

func (preparer *AgentStagePreparer) PrepareGovernedAgentStage(
	ctx context.Context,
	subject AgentPlanningSubject,
	command PrepareAgentStageCommand,
) (PreparedAgentStage, error) {
	if ctx == nil {
		return PreparedAgentStage{}, fmt.Errorf("context is required")
	}
	if preparer == nil || preparer.provider == nil || preparer.componentResolver == nil ||
		preparer.artifacts == nil || preparer.admissions == nil || preparer.now == nil {
		return PreparedAgentStage{}, fmt.Errorf("agent stage preparer is not initialized")
	}
	if err := subject.validate(); err != nil {
		return PreparedAgentStage{}, fmt.Errorf("validate formal agent planning subject: %w", err)
	}
	ledgerSubject := planningLedgerSubject(subject)
	// The governed artifact repository has an ASCII-safe identity envelope.
	// Enforce that stricter subset before the first repository lookup so a
	// partially projected plan can never be created for an unusable subject.
	if err := ledgerSubject.Validate(); err != nil {
		return PreparedAgentStage{}, fmt.Errorf(
			"validate artifact-compatible formal agent subject: %w",
			err,
		)
	}
	if err := validateLocalIdentity("review_run_id", command.ReviewRunID); err != nil {
		return PreparedAgentStage{}, err
	}
	if command.StageID == "" {
		return PreparedAgentStage{}, fmt.Errorf("stage_id is required")
	}

	input, frozen, err := preparer.loadFrozenAgentClosure(ctx, subject, command)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	if err := preparer.verifyFrozenContextProviderReceipts(
		ctx,
		input.ExecutionSnapshot,
		frozen.localBundle,
		input.Target,
		input.ReviewInput,
	); err != nil {
		return PreparedAgentStage{}, err
	}
	resolutionContext := resolutionContextForAgentStage(subject, input)
	if err := validateAgentPlanningPreflight(ctx, subject, resolutionContext, input); err != nil {
		return PreparedAgentStage{}, err
	}

	existing, found, err := preparer.admissions.LookupAgentStagePlanAdmission(
		ctx,
		command.ReviewRunID,
		command.StageID,
		ledgerSubject,
	)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("lookup formal agent plan admission: %w", err)
	}
	if found {
		return preparer.loadAdmittedAgentStage(ctx, subject, input, frozen, existing)
	}

	// Context artifacts are the only external refs embedded in ReviewInput.
	// Validate all of them before the first local or governed projection write.
	if err := preparer.verifyFrozenContextArtifacts(ctx, subject, input.ReviewInput); err != nil {
		return PreparedAgentStage{}, err
	}

	bundle, receipt, err := resolveGovernedAgentConfig(
		ctx,
		preparer.provider,
		resolutionContext,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("marshal governed config bundle: %w", err)
	}
	if !reflect.DeepEqual(bundle, frozen.localBundle) ||
		!bytes.Equal(bundleBytes, frozen.configBundle) {
		return PreparedAgentStage{}, fmt.Errorf(
			"governed config bundle differs from the frozen execution snapshot artifact",
		)
	}
	components, err := resolveGovernedAgentComponents(
		ctx,
		subject,
		preparer.componentResolver,
		resolutionContext,
		bundle,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	if err := preparer.verifyResolvedComponentArtifacts(ctx, subject, components); err != nil {
		return PreparedAgentStage{}, err
	}

	input.ConfigBundle = bundle
	input.ConfigResolutionReceipt = receipt
	input.Components = components
	input.ExecutionSnapshotRef, err = preparer.putExactLocal(
		ctx,
		runmodel.ContractExecutionSnapshot,
		frozen.executionSnapshot,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	receiptBytes, err := json.Marshal(receipt)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("marshal config resolution receipt: %w", err)
	}
	input.ConfigResolutionReceiptRef, err = preparer.putExactLocal(
		ctx,
		runmodel.ContractConfigResolutionReceipt,
		receiptBytes,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}

	input.ExecutionSnapshotProjection, err = preparer.project(
		ctx, subject, command, "execution-snapshot", input.ExecutionSnapshotRef,
		frozen.executionSnapshot, input.ExecutionSnapshot.CreatedAt,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	input.ConfigBundleProjection, err = preparer.project(
		ctx, subject, command, "config-bundle", input.ExecutionSnapshot.ConfigBundleRef,
		frozen.configBundle, input.ExecutionSnapshot.CreatedAt,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	input.ConfigResolutionReceiptProjection, err = preparer.project(
		ctx, subject, command, "config-resolution-receipt",
		input.ConfigResolutionReceiptRef, receiptBytes, input.ExecutionSnapshot.CreatedAt,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	input.WorkflowProjection, err = preparer.project(
		ctx, subject, command, "workflow", input.ExecutionSnapshot.WorkflowDefinitionRef,
		frozen.workflow, input.ExecutionSnapshot.CreatedAt,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	input.ReviewSpecProjection, err = preparer.project(
		ctx, subject, command, "review-spec", input.ExecutionSnapshot.ReviewSpecRef,
		frozen.reviewSpec, input.ExecutionSnapshot.CreatedAt,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	input.ReviewInputProjection, err = preparer.project(
		ctx, subject, command, "review-input", input.ExecutionSnapshot.ReviewInputRef,
		frozen.reviewInput, input.ExecutionSnapshot.CreatedAt,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}

	plan, err := agentplan.Compile(input)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("compile governed AgentStagePlan: %w", err)
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("marshal governed AgentStagePlan: %w", err)
	}
	planLocal, err := preparer.putExactLocal(ctx, runmodel.ContractAgentStagePlan, planBytes)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	planProjection, err := preparer.project(
		ctx,
		subject,
		command,
		"agent-stage-plan",
		planLocal,
		planBytes,
		input.ExecutionSnapshot.CreatedAt,
	)
	if err != nil {
		return PreparedAgentStage{}, err
	}

	recordedAt := preparer.now().UTC()
	if recordedAt.IsZero() {
		return PreparedAgentStage{}, fmt.Errorf("agent plan admission clock returned zero time")
	}
	admission, err := runmodel.SealAgentStagePlanAdmission(
		runmodel.AgentStagePlanAdmission{
			Subject:               ledgerSubject,
			ReviewRunID:           plan.ReviewRunID,
			Stage:                 admissionStage(plan.Stage),
			PlanID:                plan.PlanID,
			PlanSemanticSHA256:    plan.SHA256,
			PlanBehaviorSHA256:    plan.BehaviorSHA256,
			Plan:                  admissionProjection(planProjection),
			ReceiptID:             receipt.ReceiptID,
			ReceiptSemanticSHA256: receipt.SHA256,
			Sources: runmodel.AgentStageSourceProjections{
				ExecutionSnapshot:       admissionProjection(input.ExecutionSnapshotProjection),
				ConfigBundle:            admissionProjection(input.ConfigBundleProjection),
				ConfigResolutionReceipt: admissionProjection(input.ConfigResolutionReceiptProjection),
				Workflow:                admissionProjection(input.WorkflowProjection),
				ReviewSpec:              admissionProjection(input.ReviewSpecProjection),
				ReviewInput:             admissionProjection(input.ReviewInputProjection),
			},
			RecordedAt: recordedAt,
		},
	)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("seal formal agent plan admission: %w", err)
	}
	if err := preparer.admissions.AppendAgentStagePlanAdmission(ctx, admission); err != nil {
		// A concurrent preparer may have committed the same immutable closure
		// with an earlier observation time. Reload the authoritative winner;
		// never replace it or resolve mutable config again.
		winner, winnerFound, lookupErr := preparer.admissions.LookupAgentStagePlanAdmission(
			ctx,
			command.ReviewRunID,
			command.StageID,
			ledgerSubject,
		)
		if lookupErr == nil && winnerFound {
			return preparer.loadAdmittedAgentStage(ctx, subject, input, frozen, winner)
		}
		if lookupErr != nil {
			return PreparedAgentStage{}, fmt.Errorf(
				"append formal agent plan admission: %v; reload winner: %w",
				err,
				lookupErr,
			)
		}
		return PreparedAgentStage{}, fmt.Errorf("append formal agent plan admission: %w", err)
	}
	return PreparedAgentStage{Plan: plan, Admission: admission}, nil
}

type frozenAgentArtifacts struct {
	executionSnapshot []byte
	configBundle      []byte
	workflow          []byte
	reviewSpec        []byte
	target            []byte
	reviewInput       []byte
	localBundle       reviewconfig.ConfigBundle
}

func (preparer *AgentStagePreparer) loadFrozenAgentClosure(
	ctx context.Context,
	subject AgentPlanningSubject,
	command PrepareAgentStageCommand,
) (agentplan.CompileInput, frozenAgentArtifacts, error) {
	snapshot, err := preparer.artifacts.ExecutionSnapshotForRun(ctx, command.ReviewRunID)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, fmt.Errorf(
			"load authoritative formal agent execution snapshot: %w",
			err,
		)
	}
	snapshotBytes, err := json.Marshal(snapshot)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, fmt.Errorf(
			"marshal authoritative execution snapshot: %w",
			err,
		)
	}
	configBytes, err := preparer.readExpectedLocal(
		ctx,
		snapshot.ConfigBundleRef,
		runmodel.ContractConfigBundle,
	)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	localBundle, err := reviewconfig.DecodeBundle(configBytes)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, fmt.Errorf(
			"decode frozen config bundle: %w",
			err,
		)
	}
	workflowBytes, err := preparer.readExpectedLocal(
		ctx,
		snapshot.WorkflowDefinitionRef,
		runmodel.ContractWorkflowDefinition,
	)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	definition, err := workflow.DecodeDefinition(workflowBytes)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	specBytes, err := preparer.readExpectedLocal(
		ctx,
		snapshot.ReviewSpecRef,
		runmodel.ContractReviewSpec,
	)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specBytes)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	targetBytes, err := preparer.readExpectedLocal(
		ctx,
		snapshot.TargetSnapshotRef,
		runmodel.ContractMaterializedTarget,
	)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	target, err := targetmodel.DecodeMaterializedTarget(targetBytes)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	inputBytes, err := preparer.readExpectedLocal(
		ctx,
		snapshot.ReviewInputRef,
		runmodel.ContractReviewInput,
	)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	reviewInput, err := reviewcore.DecodeReviewInput(inputBytes)
	if err != nil {
		return agentplan.CompileInput{}, frozenAgentArtifacts{}, err
	}
	compileInput := agentplan.CompileInput{
		ReviewRunID:       command.ReviewRunID,
		StageID:           command.StageID,
		ExecutionSnapshot: snapshot,
		ConfigBundle:      localBundle,
		Workflow:          definition,
		ReviewSpec:        spec,
		Target:            target,
		ReviewInput:       reviewInput,
	}
	if snapshot.ReplaySourceRunRef != nil || snapshot.ReplayChangeSetRef != nil ||
		len(snapshot.ReplayInputRefs) != 0 {
		replay, replayErr := preparer.loadFormalReplayClosure(ctx, subject, snapshot)
		if replayErr != nil {
			return agentplan.CompileInput{}, frozenAgentArtifacts{}, replayErr
		}
		compileInput.FormalReplay = &replay
	}
	return compileInput, frozenAgentArtifacts{
		executionSnapshot: snapshotBytes,
		configBundle:      configBytes,
		workflow:          workflowBytes,
		reviewSpec:        specBytes,
		target:            targetBytes,
		reviewInput:       inputBytes,
		localBundle:       localBundle,
	}, nil
}

func (preparer *AgentStagePreparer) loadFormalReplayClosure(
	ctx context.Context,
	subject AgentPlanningSubject,
	snapshot runmodel.ExecutionSnapshot,
) (agentplan.FormalReplayClosure, error) {
	if len(snapshot.ReplayInputRefs) != 0 || snapshot.ReplaySourceRunRef == nil ||
		snapshot.ReplayChangeSetRef == nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf(
			"formal replay requires source/change refs and no reused stage checkpoints",
		)
	}
	sourceBytes, err := preparer.readExpectedLocal(
		ctx, *snapshot.ReplaySourceRunRef, runmodel.ContractReviewRun,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	sourceRun, err := runmodel.DecodeReviewRun(sourceBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("decode formal replay source run: %w", err)
	}
	committedRef, err := preparer.artifacts.CommittedRunRef(ctx, sourceRun.RunID)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("resolve formal replay source commit: %w", err)
	}
	if committedRef != *snapshot.ReplaySourceRunRef {
		return agentplan.FormalReplayClosure{}, fmt.Errorf(
			"formal replay source ref is not the authoritative committed run",
		)
	}
	sourceSnapshot, err := preparer.artifacts.ExecutionSnapshotForRun(ctx, sourceRun.RunID)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("load formal replay source snapshot: %w", err)
	}
	sourceSpecBytes, err := preparer.readExpectedLocal(
		ctx, sourceSnapshot.ReviewSpecRef, runmodel.ContractReviewSpec,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	sourceSpec, err := contractsv1alpha1.DecodeReviewSpec(sourceSpecBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("decode formal replay source ReviewSpec: %w", err)
	}
	sourceWorkflowBytes, err := preparer.readExpectedLocal(
		ctx, sourceSnapshot.WorkflowDefinitionRef, runmodel.ContractWorkflowDefinition,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	sourceWorkflow, err := workflow.DecodeDefinition(sourceWorkflowBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf(
			"decode formal replay source WorkflowDefinition: %w", err,
		)
	}
	sourceTargetBytes, err := preparer.readExpectedLocal(
		ctx, sourceSnapshot.TargetSnapshotRef, runmodel.ContractMaterializedTarget,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	sourceTarget, err := targetmodel.DecodeMaterializedTarget(sourceTargetBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("decode formal replay source target: %w", err)
	}
	sourceInputBytes, err := preparer.readExpectedLocal(
		ctx, sourceSnapshot.ReviewInputRef, runmodel.ContractReviewInput,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	sourceInput, err := reviewcore.DecodeReviewInput(sourceInputBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("decode formal replay source ReviewInput: %w", err)
	}
	sourceConfigBytes, err := preparer.readExpectedLocal(
		ctx, sourceSnapshot.ConfigBundleRef, runmodel.ContractConfigBundle,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	sourceConfig, err := reviewconfig.DecodeBundle(sourceConfigBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("decode formal replay source ConfigBundle: %w", err)
	}
	sourceAdmission, found, err := preparer.admissions.LookupAgentStagePlanAdmission(
		ctx, sourceRun.RunID, "agent_hypothesize", planningLedgerSubject(subject),
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("source plan admission does not exist")
		}
		return agentplan.FormalReplayClosure{}, fmt.Errorf("resolve formal replay source admission: %w", err)
	}
	sourceReceiptBytes, err := preparer.readExpectedLocal(
		ctx, sourceAdmission.Sources.ConfigResolutionReceipt.Local,
		runmodel.ContractConfigResolutionReceipt,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	sourceReceipt, err := reviewconfig.DecodeConfigResolutionReceipt(sourceReceiptBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("decode formal replay source config receipt: %w", err)
	}
	changeBytes, err := preparer.readExpectedLocal(
		ctx, *snapshot.ReplayChangeSetRef, runmodel.ContractReplayChangeSet,
	)
	if err != nil {
		return agentplan.FormalReplayClosure{}, err
	}
	change, err := runmodel.DecodeReplayChangeSet(changeBytes)
	if err != nil {
		return agentplan.FormalReplayClosure{}, fmt.Errorf("decode formal replay change set: %w", err)
	}
	return agentplan.FormalReplayClosure{
		SourceRun: sourceRun, SourceRunRef: *snapshot.ReplaySourceRunRef,
		SourceSnapshot: sourceSnapshot, SourceReviewSpec: sourceSpec,
		SourceWorkflow: sourceWorkflow,
		SourceTarget:   sourceTarget, SourceReviewInput: sourceInput,
		SourceConfigBundle: sourceConfig, SourceConfigReceipt: sourceReceipt,
		ChangeSet: change, ChangeSetRef: *snapshot.ReplayChangeSetRef,
	}, nil
}

func (preparer *AgentStagePreparer) readExpectedLocal(
	ctx context.Context,
	ref runmodel.ArtifactRef,
	contract string,
) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, fmt.Errorf("validate frozen %s artifact ref: %w", contract, err)
	}
	if ref.Contract != contract {
		return nil, fmt.Errorf("frozen artifact contract is %q, want %q", ref.Contract, contract)
	}
	data, err := preparer.artifacts.ReadLocalArtifact(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("read frozen %s artifact: %w", contract, err)
	}
	if want := localRefForBytes(contract, data); want != ref {
		return nil, fmt.Errorf("frozen %s artifact bytes do not match their local ref", contract)
	}
	return data, nil
}

func (preparer *AgentStagePreparer) putExactLocal(
	ctx context.Context,
	contract string,
	data []byte,
) (runmodel.ArtifactRef, error) {
	ref, err := preparer.artifacts.PutLocalArtifact(ctx, contract, data)
	if err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("persist local %s artifact: %w", contract, err)
	}
	if ref != localRefForBytes(contract, data) {
		return runmodel.ArtifactRef{}, fmt.Errorf(
			"local artifact repository returned a mismatched %s ref",
			contract,
		)
	}
	return ref, nil
}

func (preparer *AgentStagePreparer) project(
	ctx context.Context,
	subject AgentPlanningSubject,
	command PrepareAgentStageCommand,
	role string,
	local runmodel.ArtifactRef,
	data []byte,
	frozenAt time.Time,
) (agentplan.GovernedArtifactProjection, error) {
	if frozenAt.IsZero() || frozenAt.Location() != time.UTC {
		return agentplan.GovernedArtifactProjection{}, fmt.Errorf(
			"formal agent projection frozen time must be non-zero UTC",
		)
	}
	if local != localRefForBytes(local.Contract, data) {
		return agentplan.GovernedArtifactProjection{}, fmt.Errorf(
			"formal agent %s local ref does not bind exact projection bytes",
			role,
		)
	}
	binding, err := preparer.artifacts.ProjectReadOnly(
		ctx,
		subject,
		AgentArtifactProjectionRequest{
			Role: role, ReviewRunID: command.ReviewRunID, StageID: command.StageID,
			Source: local, ExactContent: bytes.Clone(data), FrozenAt: frozenAt,
		},
	)
	if err != nil {
		return agentplan.GovernedArtifactProjection{}, fmt.Errorf(
			"project formal agent %s artifact: %w",
			role,
			err,
		)
	}
	projection := agentplan.GovernedArtifactProjection{Local: local, Governed: binding}
	if err := projection.Validate(); err != nil {
		return agentplan.GovernedArtifactProjection{}, fmt.Errorf(
			"validate formal agent %s projection: %w",
			role,
			err,
		)
	}
	resolved, err := preparer.artifacts.VerifyRead(ctx, subject, binding)
	if err != nil {
		return agentplan.GovernedArtifactProjection{}, fmt.Errorf(
			"verify formal agent %s governed artifact: %w",
			role,
			err,
		)
	}
	if !bytes.Equal(resolved, data) {
		return agentplan.GovernedArtifactProjection{}, fmt.Errorf(
			"formal agent %s governed artifact bytes differ from local source",
			role,
		)
	}
	return projection, nil
}

func (preparer *AgentStagePreparer) verifyFrozenContextArtifacts(
	ctx context.Context,
	_ AgentPlanningSubject,
	input reviewcore.ReviewInput,
) error {
	for index, contextBinding := range input.Contexts {
		if contextBinding.Ref == nil {
			continue
		}
		ref := contextBinding.Ref
		localRef := runmodel.ArtifactRef{
			URI: ref.ArtifactURI, SHA256: ref.Digest,
			SizeBytes: ref.SizeBytes, Contract: ref.Contract,
		}
		if _, err := preparer.artifacts.ReadLocalArtifact(ctx, localRef); err != nil {
			return fmt.Errorf(
				"read frozen ReviewInput context[%d] %q: %w",
				index,
				ref.ContextID,
				err,
			)
		}
	}
	return nil
}

func (preparer *AgentStagePreparer) verifyFrozenContextProviderReceipts(
	ctx context.Context,
	snapshot runmodel.ExecutionSnapshot,
	bundle reviewconfig.ConfigBundle,
	target targetmodel.MaterializedTarget,
	input reviewcore.ReviewInput,
) error {
	definitions := bundle.Execution.ContextProviders
	if len(snapshot.ContextProviderReceiptRefs) != len(definitions) {
		return fmt.Errorf("frozen context provider receipt count does not match governed config")
	}
	if !reflect.DeepEqual(target.Contexts, input.Contexts) {
		return fmt.Errorf("frozen target and ReviewInput contexts differ")
	}
	bindings := make(map[string]reviewcore.ContextBinding, len(input.Contexts))
	for _, binding := range input.Contexts {
		bindings[binding.ContextID()] = binding
	}
	seen := make(map[string]struct{}, len(definitions))
	for index, ref := range snapshot.ContextProviderReceiptRefs {
		data, err := preparer.readExpectedLocal(
			ctx,
			ref,
			runmodel.ContractContextProviderExecutionReceipt,
		)
		if err != nil {
			return fmt.Errorf("read frozen context provider receipt %d: %w", index, err)
		}
		receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
		if err != nil {
			return fmt.Errorf("decode frozen context provider receipt %d: %w", index, err)
		}
		definition := definitions[index]
		if receipt.ProviderID != definition.ID ||
			receipt.ProviderRevision != definition.Revision ||
			receipt.Kind != definition.Kind ||
			receipt.Adapter.ID != definition.Adapter.ID ||
			receipt.Adapter.Revision != definition.Adapter.Revision ||
			receipt.Adapter.SHA256 != definition.Adapter.SHA256 ||
			receipt.RepositoryID != target.Snapshot.Repository.RepositoryID ||
			receipt.CommitOID != target.Snapshot.Head.CommitOID {
			return fmt.Errorf(
				"frozen context provider receipt %d does not match governed config or target",
				index,
			)
		}
		if _, duplicate := seen[receipt.ContextID]; duplicate {
			return fmt.Errorf("frozen context provider receipts alias context %q", receipt.ContextID)
		}
		seen[receipt.ContextID] = struct{}{}
		binding, exists := bindings[receipt.ContextID]
		if !exists {
			return fmt.Errorf(
				"frozen context provider receipt %d has no exact context binding",
				index,
			)
		}
		var kind, revision string
		var provenance reviewcore.ContextProvenance
		if binding.Ref != nil {
			kind, revision, provenance = binding.Ref.Kind, binding.Ref.Revision, binding.Ref.Provenance
		} else if binding.Gap != nil {
			kind, revision, provenance = binding.Gap.Kind, binding.Gap.Revision, binding.Gap.Provenance
		}
		if kind != definition.Kind || revision != receipt.CommitOID ||
			provenance.Provider != definition.Kind ||
			provenance.ProducerID != definition.Adapter.ID ||
			provenance.ProducerRevision != definition.Adapter.Revision {
			return fmt.Errorf(
				"frozen context provider receipt %d provenance does not match binding",
				index,
			)
		}
		switch receipt.Status {
		case contractsv1alpha1.ContextProviderReceiptSucceeded:
			if binding.Ref == nil || binding.Ref.Digest != receipt.ContextDigest ||
				binding.Ref.Contract != receipt.ContextContract {
				return fmt.Errorf(
					"frozen context provider receipt %d success does not match ContextRef",
					index,
				)
			}
		case contractsv1alpha1.ContextProviderReceiptGap:
			if binding.Gap == nil || binding.Gap.Digest != receipt.ContextDigest ||
				binding.Gap.ReasonCode != receipt.ReasonCode {
				return fmt.Errorf(
					"frozen context provider receipt %d gap does not match ContextGap",
					index,
				)
			}
		default:
			return fmt.Errorf(
				"frozen context provider receipt %d has unsupported status %q",
				index,
				receipt.Status,
			)
		}
	}
	return nil
}

func (preparer *AgentStagePreparer) verifyResolvedComponentArtifacts(
	ctx context.Context,
	subject AgentPlanningSubject,
	components agentplan.ResolvedComponents,
) error {
	bindings := []contractsv1alpha1.ArtifactBinding{
		components.Agent.Artifact,
		components.Provider.Artifact,
		components.Model.Artifact,
		components.Runtime.Artifact,
		components.Prompt.Artifact,
		components.APIProtocol.Artifact,
	}
	for _, skill := range components.Skills {
		bindings = append(bindings, skill.Artifact)
	}
	for _, knowledge := range components.Knowledge {
		bindings = append(bindings, knowledge.Artifact)
	}
	for _, provider := range components.ContextProviders {
		bindings = append(bindings, provider.Adapter.Artifact)
	}
	for index, binding := range bindings {
		if _, err := preparer.artifacts.VerifyRead(ctx, subject, binding); err != nil {
			return fmt.Errorf("verify governed agent component[%d]: %w", index, err)
		}
	}
	return nil
}

func (preparer *AgentStagePreparer) loadAdmittedAgentStage(
	ctx context.Context,
	subject AgentPlanningSubject,
	input agentplan.CompileInput,
	frozen frozenAgentArtifacts,
	admission runmodel.AgentStagePlanAdmission,
) (PreparedAgentStage, error) {
	if err := admission.Validate(); err != nil {
		return PreparedAgentStage{}, fmt.Errorf("validate formal agent plan admission: %w", err)
	}
	if admission.Subject != planningLedgerSubject(subject) {
		return PreparedAgentStage{}, fmt.Errorf(
			"formal agent plan admission belongs to another authenticated subject",
		)
	}
	known := map[string]runmodel.ArtifactRef{
		"execution snapshot": localRefForBytes(runmodel.ContractExecutionSnapshot, frozen.executionSnapshot),
		"config bundle":      input.ExecutionSnapshot.ConfigBundleRef,
		"workflow":           input.ExecutionSnapshot.WorkflowDefinitionRef,
		"review spec":        input.ExecutionSnapshot.ReviewSpecRef,
		"review input":       input.ExecutionSnapshot.ReviewInputRef,
	}
	admitted := map[string]runmodel.ArtifactRef{
		"execution snapshot": admission.Sources.ExecutionSnapshot.Local,
		"config bundle":      admission.Sources.ConfigBundle.Local,
		"workflow":           admission.Sources.Workflow.Local,
		"review spec":        admission.Sources.ReviewSpec.Local,
		"review input":       admission.Sources.ReviewInput.Local,
	}
	for name, want := range known {
		if admitted[name] != want {
			return PreparedAgentStage{}, fmt.Errorf(
				"existing formal agent admission %s differs from authoritative run closure",
				name,
			)
		}
	}
	planBytes, err := preparer.artifacts.ReadLocalArtifact(ctx, admission.Plan.Local)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("read admitted local AgentStagePlan: %w", err)
	}
	if localRefForBytes(runmodel.ContractAgentStagePlan, planBytes) != admission.Plan.Local {
		return PreparedAgentStage{}, fmt.Errorf("admitted local AgentStagePlan ref is inconsistent")
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(planBytes)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("decode admitted AgentStagePlan: %w", err)
	}
	if plan.PlanID != admission.PlanID || plan.SHA256 != admission.PlanSemanticSHA256 ||
		plan.BehaviorSHA256 != admission.PlanBehaviorSHA256 ||
		plan.ReviewRunID != admission.ReviewRunID || admission.Stage != admissionStage(plan.Stage) {
		return PreparedAgentStage{}, fmt.Errorf(
			"admitted AgentStagePlan identities do not match the admission fact",
		)
	}
	governedPlan := bindingFromAdmission(admission.Plan.Governed)
	resolvedPlan, err := preparer.artifacts.VerifyRead(ctx, subject, governedPlan)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("verify admitted governed AgentStagePlan: %w", err)
	}
	if !bytes.Equal(resolvedPlan, planBytes) {
		return PreparedAgentStage{}, fmt.Errorf("admitted local and governed plan bytes differ")
	}
	for name, projection := range map[string]runmodel.AgentArtifactProjection{
		"execution snapshot": admission.Sources.ExecutionSnapshot,
		"config bundle":      admission.Sources.ConfigBundle,
		"config receipt":     admission.Sources.ConfigResolutionReceipt,
		"workflow":           admission.Sources.Workflow,
		"review spec":        admission.Sources.ReviewSpec,
		"review input":       admission.Sources.ReviewInput,
	} {
		localBytes, err := preparer.artifacts.ReadLocalArtifact(ctx, projection.Local)
		if err != nil {
			return PreparedAgentStage{}, fmt.Errorf("read admitted %s local artifact: %w", name, err)
		}
		governedBytes, err := preparer.artifacts.VerifyRead(
			ctx,
			subject,
			bindingFromAdmission(projection.Governed),
		)
		if err != nil {
			return PreparedAgentStage{}, fmt.Errorf("verify admitted %s artifact: %w", name, err)
		}
		if !bytes.Equal(localBytes, governedBytes) {
			return PreparedAgentStage{}, fmt.Errorf("admitted %s projection bytes differ", name)
		}
	}
	if plan.ExecutionSnapshot != bindingFromAdmission(admission.Sources.ExecutionSnapshot.Governed) ||
		plan.ConfigBundle != bindingFromAdmission(admission.Sources.ConfigBundle.Governed) ||
		plan.ConfigResolutionReceiptRef != bindingFromAdmission(
			admission.Sources.ConfigResolutionReceipt.Governed,
		) ||
		plan.Workflow != bindingFromAdmission(admission.Sources.Workflow.Governed) ||
		plan.ReviewSpec != bindingFromAdmission(admission.Sources.ReviewSpec.Governed) ||
		plan.ReviewInput != bindingFromAdmission(admission.Sources.ReviewInput.Governed) {
		return PreparedAgentStage{}, fmt.Errorf(
			"admitted AgentStagePlan does not exactly project its admission sources",
		)
	}
	receiptBytes, err := preparer.artifacts.ReadLocalArtifact(
		ctx,
		admission.Sources.ConfigResolutionReceipt.Local,
	)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf("read admitted config receipt: %w", err)
	}
	receipt, err := reviewconfig.DecodeConfigResolutionReceipt(receiptBytes)
	if err != nil {
		return PreparedAgentStage{}, err
	}
	if receipt.ReceiptID != admission.ReceiptID ||
		receipt.SHA256 != admission.ReceiptSemanticSHA256 ||
		receipt.ValidateAgainst(frozen.localBundle) != nil {
		return PreparedAgentStage{}, fmt.Errorf(
			"admitted config receipt does not bind the frozen config bundle",
		)
	}
	// An admitted plan is not trusted merely because its own digests and
	// projection bytes are internally consistent. Re-run the pure compiler from
	// the authoritative frozen closure and the exact admitted components, then
	// require the canonical result to be identical. This closes the retry path
	// against a self-sealed plan that widens authority/budgets or drifts from the
	// frozen workflow while preserving otherwise valid artifact identities.
	recompileInput := input
	recompileInput.ExecutionSnapshotRef = admission.Sources.ExecutionSnapshot.Local
	recompileInput.ExecutionSnapshotProjection = projectionFromAdmission(
		admission.Sources.ExecutionSnapshot,
	)
	recompileInput.ConfigBundle = frozen.localBundle
	recompileInput.ConfigBundleProjection = projectionFromAdmission(
		admission.Sources.ConfigBundle,
	)
	recompileInput.ConfigResolutionReceipt = receipt
	recompileInput.ConfigResolutionReceiptRef = admission.Sources.ConfigResolutionReceipt.Local
	recompileInput.ConfigResolutionReceiptProjection = projectionFromAdmission(
		admission.Sources.ConfigResolutionReceipt,
	)
	recompileInput.WorkflowProjection = projectionFromAdmission(admission.Sources.Workflow)
	recompileInput.ReviewSpecProjection = projectionFromAdmission(admission.Sources.ReviewSpec)
	recompileInput.ReviewInputProjection = projectionFromAdmission(admission.Sources.ReviewInput)
	recompileInput.Components = agentplan.ResolvedComponents{
		Agent: plan.Agent, Provider: plan.Provider, Model: plan.Model,
		Runtime: plan.Runtime, Prompt: plan.Prompt,
		APIProtocol:      plan.ModelAuthority.APIProtocol,
		Skills:           plan.Skills,
		Knowledge:        plan.Knowledge,
		ContextProviders: plan.ContextProviders,
	}
	recompiled, err := agentplan.Compile(recompileInput)
	if err != nil {
		return PreparedAgentStage{}, fmt.Errorf(
			"recompile admitted AgentStagePlan from frozen closure: %w",
			err,
		)
	}
	if !reflect.DeepEqual(recompiled, plan) {
		return PreparedAgentStage{}, fmt.Errorf(
			"admitted AgentStagePlan differs from the canonical frozen-closure compilation",
		)
	}
	if err := preparer.verifyFrozenContextArtifacts(ctx, subject, input.ReviewInput); err != nil {
		return PreparedAgentStage{}, err
	}
	if err := verifyPlanComponentArtifacts(ctx, preparer.artifacts, subject, plan); err != nil {
		return PreparedAgentStage{}, err
	}
	return PreparedAgentStage{Plan: plan, Admission: admission}, nil
}

func verifyPlanComponentArtifacts(
	ctx context.Context,
	artifacts AgentPlanArtifactRepository,
	subject AgentPlanningSubject,
	plan contractsv1alpha1.AgentStagePlan,
) error {
	bindings := []contractsv1alpha1.ArtifactBinding{
		plan.Agent.Artifact,
		plan.Provider.Artifact,
		plan.Model.Artifact,
		plan.Runtime.Artifact,
		plan.Prompt.Artifact,
		plan.ModelAuthority.APIProtocol.Artifact,
	}
	for _, skill := range plan.Skills {
		bindings = append(bindings, skill.Artifact)
	}
	for _, knowledge := range plan.Knowledge {
		bindings = append(bindings, knowledge.Artifact)
	}
	for _, provider := range plan.ContextProviders {
		bindings = append(bindings, provider.Adapter.Artifact)
	}
	for index, binding := range bindings {
		if _, err := artifacts.VerifyRead(ctx, subject, binding); err != nil {
			return fmt.Errorf("verify admitted plan component[%d]: %w", index, err)
		}
	}
	return nil
}

func resolutionContextForAgentStage(
	subject AgentPlanningSubject,
	input agentplan.CompileInput,
) reviewconfig.ResolutionContext {
	path := ""
	if input.ReviewSpec.Target.Mode == contractsv1alpha1.ReviewModeSelection &&
		input.ReviewSpec.Target.Selection != nil {
		path = input.ReviewSpec.Target.Selection.Path
	}
	invocationID := input.ReviewRunID
	if input.FormalReplay != nil {
		invocationID = input.ConfigBundle.Context.InvocationID
	}
	return reviewconfig.ResolutionContext{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		RepositoryID: subject.RepositoryID, Path: path, InvocationID: invocationID,
	}
}

func planningLedgerSubject(subject AgentPlanningSubject) runmodel.AgentPlanningSubject {
	return runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
}

func localRefForBytes(contract string, data []byte) runmodel.ArtifactRef {
	digest := sha256.Sum256(data)
	encoded := hex.EncodeToString(digest[:])
	return runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + encoded,
		SHA256: encoded, SizeBytes: int64(len(data)), Contract: contract,
	}
}

func admissionProjection(
	projection agentplan.GovernedArtifactProjection,
) runmodel.AgentArtifactProjection {
	return runmodel.AgentArtifactProjection{
		Local: projection.Local,
		Governed: runmodel.GovernedArtifactBinding{
			URI: projection.Governed.Ref.URI, SHA256: projection.Governed.Ref.SHA256,
			SizeBytes: projection.Governed.Ref.SizeBytes,
			Contract:  projection.Governed.Contract,
		},
	}
}

func projectionFromAdmission(
	projection runmodel.AgentArtifactProjection,
) agentplan.GovernedArtifactProjection {
	return agentplan.GovernedArtifactProjection{
		Local:    projection.Local,
		Governed: bindingFromAdmission(projection.Governed),
	}
}

func bindingFromAdmission(
	binding runmodel.GovernedArtifactBinding,
) contractsv1alpha1.ArtifactBinding {
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI: binding.URI, SHA256: binding.SHA256, SizeBytes: binding.SizeBytes,
		},
		Contract: binding.Contract,
	}
}

func admissionStage(stage contractsv1alpha1.VersionedRef) runmodel.AgentStageRef {
	return runmodel.AgentStageRef{ID: stage.ID, Revision: stage.Revision, SHA256: stage.SHA256}
}
