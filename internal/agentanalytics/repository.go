package agentanalytics

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
	"reflect"
	"slices"
	"strings"
	"time"

	"argus.local/argus/internal/agentshadow"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var (
	ErrProjectionConflict = errors.New("agent execution projection snapshot conflict")
	ErrProjectionCorrupt  = errors.New("agent execution projection snapshot corrupt")
)

// CommittedSource preserves the original result-only seam for local fixtures
// and older callers. Production agentshadow.Service additionally implements
// executionAttemptSource and committedResultQuerySource, so Rebuild observes
// terminal failures, cancellations, and orphan intents without weakening the
// strict committed-result path.
type CommittedSource interface {
	List(context.Context, agentshadow.ListRequest) ([]agentshadow.Result, error)
}

type executionAttemptSource interface {
	ListExecutions(
		context.Context,
		agentshadow.ExecutionListRequest,
	) ([]agentshadow.ExecutionAttempt, error)
}

type committedResultQuerySource interface {
	Query(context.Context, agentshadow.QueryRequest) (agentshadow.Result, error)
}

type RebuildRequest struct {
	SnapshotID string          `json:"snapshot_id"`
	Scope      Scope           `json:"scope"`
	Window     TimeWindow      `json:"window"`
	GroupBy    []DimensionName `json:"group_by"`
	BuiltAt    time.Time       `json:"built_at"`
}

type Selector struct {
	Scope   Scope
	Window  TimeWindow
	GroupBy []DimensionName
}

type ExportTarget string

const (
	ExportFacts      ExportTarget = "facts"
	ExportProjection ExportTarget = "projection"
)

type Adapter struct {
	source CommittedSource
	store  *local.Store
}

type projectionManifest struct {
	SchemaVersion string            `json:"schema_version"`
	SnapshotID    string            `json:"snapshot_id"`
	SnapshotRef   local.ArtifactRef `json:"snapshot_ref"`
	Scope         Scope             `json:"scope"`
	Window        TimeWindow        `json:"window"`
	GroupBy       []DimensionName   `json:"group_by"`
	BuiltAt       time.Time         `json:"built_at"`
}

func New(source CommittedSource, store *local.Store) (*Adapter, error) {
	if source == nil || store == nil {
		return nil, fmt.Errorf("committed shadow source and projection store are required")
	}
	return &Adapter{source: source, store: store}, nil
}

// Open creates a query/export-only adapter. It cannot read the shadow source.
func Open(store *local.Store) (*Adapter, error) {
	if store == nil {
		return nil, fmt.Errorf("projection store is required")
	}
	return &Adapter{store: store}, nil
}

func (adapter *Adapter) Rebuild(
	ctx context.Context,
	request RebuildRequest,
) (ProjectionSnapshot, error) {
	if adapter == nil || adapter.source == nil || adapter.store == nil {
		return ProjectionSnapshot{}, fmt.Errorf("adapter is not configured for rebuild")
	}
	request.GroupBy = canonicalGroupBy(request.GroupBy)
	if err := request.Validate(); err != nil {
		return ProjectionSnapshot{}, err
	}
	if err := contextError(ctx); err != nil {
		return ProjectionSnapshot{}, err
	}
	results, err := adapter.source.List(ctx, agentshadow.ListRequest{
		Scope: agentshadow.Scope{
			TenantID: request.Scope.TenantID, WorkspaceID: request.Scope.WorkspaceID,
		},
		StartInclusive: request.Window.StartInclusive,
		EndExclusive:   request.Window.EndExclusive,
	})
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("list committed shadow results: %w", err)
	}
	attempts := []agentshadow.ExecutionAttempt{}
	if source, ok := adapter.source.(executionAttemptSource); ok {
		attempts, err = source.ListExecutions(ctx, agentshadow.ExecutionListRequest{
			Scope: agentshadow.Scope{
				TenantID: request.Scope.TenantID, WorkspaceID: request.Scope.WorkspaceID,
			},
			StartInclusive: request.Window.StartInclusive,
			EndExclusive:   request.Window.EndExclusive,
		})
		if err != nil {
			return ProjectionSnapshot{}, fmt.Errorf("list shadow execution attempts: %w", err)
		}
		results, err = adapter.resolveSucceededAttemptResults(ctx, request, results, attempts)
		if err != nil {
			return ProjectionSnapshot{}, err
		}
	}
	facts, bindings, err := buildFactsWithAttempts(request, results, attempts)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	projection, err := Project(facts, request.GroupBy)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("project diagnostic metrics: %w", err)
	}
	snapshot := ProjectionSnapshot{
		SchemaVersion: ProjectionSnapshotSchemaVersion,
		SnapshotID:    request.SnapshotID, PolicyRevision: ProjectionPolicyRevision,
		Scope: request.Scope, Window: request.Window,
		GroupBy: slices.Clone(request.GroupBy), BuiltAt: request.BuiltAt,
		SourceBindings: bindings, Facts: facts, Projection: projection,
	}
	return adapter.persist(ctx, snapshot)
}

func buildFacts(
	request RebuildRequest,
	results []agentshadow.Result,
) (FactSet, []SourceBinding, error) {
	return buildFactsWithAttempts(request, results, nil)
}

func buildFactsWithAttempts(
	request RebuildRequest,
	results []agentshadow.Result,
	attempts []agentshadow.ExecutionAttempt,
) (FactSet, []SourceBinding, error) {
	executions := make([]ExecutionFact, 0, len(results))
	tasks := make([]TaskExecutionFact, 0)
	tools := make([]ToolUsageFact, 0)
	bindings := make([]SourceBinding, 0, len(results)+len(attempts))
	attemptByExecutionID := make(map[string]agentshadow.ExecutionAttempt, len(attempts))
	for index, attempt := range attempts {
		if err := validateExecutionAttempt(request, attempt); err != nil {
			return FactSet{}, nil, fmt.Errorf("execution attempts[%d]: %w", index, err)
		}
		if _, duplicate := attemptByExecutionID[attempt.ExecutionID]; duplicate {
			return FactSet{}, nil, fmt.Errorf(
				"execution attempts[%d] duplicates execution_id %q",
				index,
				attempt.ExecutionID,
			)
		}
		attemptByExecutionID[attempt.ExecutionID] = attempt
	}
	seenExecutions := make(map[string]string, len(results))
	for index, result := range results {
		attempt, hasAttempt := attemptByExecutionID[result.Plan.ExecutionID]
		var validateErr error
		if hasAttempt {
			validateErr = validateCommittedResultClosure(request.Scope, result)
		} else {
			validateErr = validateCommittedResult(request, result)
		}
		if validateErr != nil {
			return FactSet{}, nil, fmt.Errorf("committed results[%d]: %w", index, validateErr)
		}
		if hasAttempt {
			if err := validateAttemptResultBinding(attempt, result); err != nil {
				return FactSet{}, nil, fmt.Errorf("committed results[%d]: %w", index, err)
			}
		}
		if prior, exists := seenExecutions[result.Plan.ExecutionID]; exists {
			return FactSet{}, nil, fmt.Errorf(
				"execution_id %q is committed by both %q and %q",
				result.Plan.ExecutionID,
				prior,
				result.Manifest.ManifestID,
			)
		}
		seenExecutions[result.Plan.ExecutionID] = result.Manifest.ManifestID
		observedAt := result.Observation.RecordedAt
		if hasAttempt {
			observedAt = attempt.ObservedAt
		}
		executionFactID := stableID(
			"agent-execution-fact",
			result.Record.TenantID,
			result.Record.WorkspaceID,
			result.Observation.ObservationID,
		)
		summary := result.Observation.Summary
		execution := ExecutionFact{
			SchemaVersion: ExecutionFactSchemaVersion,
			FactID:        executionFactID, ObservationID: result.Observation.ObservationID,
			ManifestID: result.Manifest.ManifestID, SourceRunID: result.Plan.SourceRunID,
			ExecutionID: result.Plan.ExecutionID, ReviewRunID: result.Plan.ReviewRunID,
			TenantID: result.Record.TenantID, WorkspaceID: result.Record.WorkspaceID,
			Agent: result.Plan.Agent, Provider: result.Plan.Provider, Model: result.Plan.Model,
			APIProtocol:    result.Plan.APIProtocol,
			ExecutionClass: result.Plan.ExecutionClass,
			Attestation:    result.Plan.Attestation, Disposition: result.Manifest.Disposition,
			Status: executionStatusFromResult(result.Manifest.Status), DurationMS: result.Observation.DurationMS,
			ReasonCodes: slices.Clone(result.Observation.ReasonCodes),
			Receipts:    summary.Receipts, TasksSucceeded: summary.TasksSucceeded,
			TasksFailed: summary.TasksFailed, TasksCanceled: summary.TasksCanceled,
			ModelTurnsStarted:   summary.ModelTurnsStarted,
			ModelTurnsCompleted: summary.ModelTurnsCompleted, ToolCalls: summary.ToolCalls,
			Usage: ExecutionUsage{
				Completeness:        summary.UsageCompleteness,
				ReportedReceipts:    summary.UsageReportedReceipts,
				PartialReceipts:     summary.UsagePartialReceipts,
				UnavailableReceipts: summary.UsageUnavailableReceipts,
				InputTokens:         summary.InputTokens, OutputTokens: summary.OutputTokens,
				CacheReadTokens:           summary.CacheReadTokens,
				CacheWriteTokens:          summary.CacheWriteTokens,
				ReasoningTokens:           summary.ReasoningTokens,
				ReasoningReportedReceipts: summary.ReasoningReportedReceipts,
				TotalTokens:               summary.TotalTokens,
			},
			ProvenanceClass: HostObservationProvenance,
			Authority:       DiagnosticAuthority, ObservedAt: observedAt,
		}
		executions = append(executions, execution)
		for _, receipt := range result.ReceiptCollection.Receipts {
			taskFactID := stableID(
				"agent-task-fact",
				executionFactID,
				receipt.ReceiptID,
			)
			duration := receipt.FinishedAt.Sub(receipt.StartedAt).Milliseconds()
			if duration < 0 {
				return FactSet{}, nil, fmt.Errorf("receipt %q has negative duration", receipt.ReceiptID)
			}
			task := TaskExecutionFact{
				SchemaVersion: TaskFactSchemaVersion, FactID: taskFactID,
				ExecutionFactID: executionFactID, ReceiptID: receipt.ReceiptID,
				TaskID: receipt.TaskID, GroupID: receipt.GroupID,
				HypothesisOccurrenceID: cloneStringPointer(receipt.HypothesisOccurrenceID),
				Role:                   receipt.TaskRole, Dimension: receipt.Dimension,
				Runtime: receipt.Runtime, Profile: receipt.Profile,
				Agent: receipt.Agent, Provider: receipt.Provider, Model: receipt.Model,
				APIProtocol: receipt.APIProtocol, Status: receipt.Status,
				FailureReasonCode: cloneStringPointer(receipt.FailureReasonCode),
				StartedAt:         receipt.StartedAt, FinishedAt: receipt.FinishedAt,
				DurationMS: uint64(duration), ModelTurnsStarted: receipt.ModelTurnsStarted,
				ModelTurnsCompleted: receipt.ModelTurnsCompleted, ToolCalls: receipt.ToolCalls,
				Usage:           cloneTokenUsage(receipt.Usage),
				ProvenanceClass: string(receipt.ProvenanceClass),
				Authority:       string(receipt.Authority), ObservedAt: observedAt,
			}
			tasks = append(tasks, task)
			for _, usage := range receipt.ToolUsage {
				tools = append(tools, ToolUsageFact{
					SchemaVersion:   ToolUsageFactSchemaVersion,
					FactID:          stableID("agent-tool-fact", taskFactID, usage.ToolID),
					ExecutionFactID: executionFactID, TaskFactID: taskFactID,
					ToolID: usage.ToolID, InvocationCount: usage.InvocationCount,
					FailureCount:    usage.FailureCount,
					ProvenanceClass: string(receipt.ProvenanceClass),
					Authority:       string(receipt.Authority),
					ObservedAt:      observedAt,
				})
			}
		}
		binding, err := sourceBinding(executionFactID, result)
		if err != nil {
			return FactSet{}, nil, err
		}
		if hasAttempt {
			binding, err = attemptSourceBinding(executionFactID, attempt, &result)
			if err != nil {
				return FactSet{}, nil, err
			}
		}
		bindings = append(bindings, binding)
	}
	for _, attempt := range attempts {
		if _, alreadyBuilt := seenExecutions[attempt.ExecutionID]; alreadyBuilt {
			continue
		}
		if attempt.Status == agentshadow.ExecutionStatusSucceeded {
			return FactSet{}, nil, fmt.Errorf(
				"succeeded execution attempt %q has no committed result",
				attempt.ExecutionID,
			)
		}
		execution, binding, err := sparseAttemptFact(request, attempt)
		if err != nil {
			return FactSet{}, nil, err
		}
		executions = append(executions, execution)
		bindings = append(bindings, binding)
		seenExecutions[attempt.ExecutionID] = string(attempt.Status)
	}
	slices.SortFunc(executions, func(left, right ExecutionFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(tasks, func(left, right TaskExecutionFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(tools, func(left, right ToolUsageFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(bindings, func(left, right SourceBinding) int {
		return strings.Compare(left.ExecutionFactID, right.ExecutionFactID)
	})
	facts := FactSet{
		SchemaVersion: FactSetSchemaVersion, Window: request.Window,
		Executions: executions, Tasks: tasks, ToolUsage: tools,
	}
	if err := facts.Validate(); err != nil {
		return FactSet{}, nil, fmt.Errorf("validate rebuilt agent execution facts: %w", err)
	}
	return facts, bindings, nil
}

func validateCommittedResult(request RebuildRequest, result agentshadow.Result) error {
	if err := validateCommittedResultClosure(request.Scope, result); err != nil {
		return err
	}
	if !request.Window.contains(result.Observation.RecordedAt) ||
		!result.Record.AcceptedAt.Equal(result.Observation.RecordedAt) {
		return fmt.Errorf("result is outside the host observation window")
	}
	return nil
}

func validateCommittedResultClosure(scope Scope, result agentshadow.Result) error {
	if result.Record.TenantID != scope.TenantID ||
		result.Record.WorkspaceID != scope.WorkspaceID {
		return fmt.Errorf("result escapes requested scope")
	}
	if !result.Record.AcceptedAt.Equal(result.Observation.RecordedAt) {
		return fmt.Errorf("result commit time does not match its host observation")
	}
	if err := result.Plan.Validate(); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if err := result.HypothesisSet.Validate(); err != nil {
		return fmt.Errorf("hypothesis set: %w", err)
	}
	if err := result.ReceiptCollection.Validate(); err != nil {
		return fmt.Errorf("receipt collection: %w", err)
	}
	if result.TaskEvidenceCollection.SchemaVersion != "" {
		if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(
			result.TaskEvidenceCollection,
			result.Plan,
			result.ReceiptCollection,
		); err != nil {
			return fmt.Errorf("task evidence collection: %w", err)
		}
	} else if result.TaskEvidence.Ref != result.Record.TaskEvidenceCollectionRef ||
		result.TaskEvidence.ContentPolicy != contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy ||
		result.TaskEvidence.State == "" || result.TaskEvidence.ExportPolicy != "deny" {
		return fmt.Errorf("redacted task evidence summary does not bind its governed artifact")
	}
	if err := result.Manifest.Validate(); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if err := result.Observation.Validate(); err != nil {
		return fmt.Errorf("observation: %w", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewResultManifestBindings(
		result.Manifest,
		result.Plan,
		result.HypothesisSet,
		result.ReceiptCollection,
	); err != nil {
		return fmt.Errorf("manifest bindings: %w", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewObservationBinding(
		result.Observation,
		result.Manifest,
		result.Plan,
	); err != nil {
		return fmt.Errorf("observation binding: %w", err)
	}
	if result.Record.ManifestID != result.Manifest.ManifestID ||
		result.Record.ObservationID != result.Observation.ObservationID ||
		result.Record.AgentReviewPlanRef != result.Manifest.AgentReviewPlanRef ||
		result.Record.HypothesisSetRef != result.Manifest.HypothesisSetRef ||
		result.Record.TaskEvidenceCollectionRef != result.Manifest.AgentTaskEvidenceRef ||
		result.Record.ReceiptCollectionRef != result.Manifest.AgentExecutionReceiptRef {
		return fmt.Errorf("commit record does not bind the validated result")
	}
	return nil
}

func sourceBinding(
	executionFactID string,
	result agentshadow.Result,
) (SourceBinding, error) {
	resultBinding, err := resultSourceBinding(result)
	if err != nil {
		return SourceBinding{}, err
	}
	return SourceBinding{
		ExecutionFactID: executionFactID,
		Kind:            SourceBindingResult,
		Result:          &resultBinding,
	}, nil
}

func resultSourceBinding(result agentshadow.Result) (ResultSourceBinding, error) {
	recordData, err := json.Marshal(result.Record)
	if err != nil {
		return ResultSourceBinding{}, fmt.Errorf("marshal import record provenance: %w", err)
	}
	observationData, err := json.Marshal(result.Observation)
	if err != nil {
		return ResultSourceBinding{}, fmt.Errorf("marshal observation provenance: %w", err)
	}
	return ResultSourceBinding{
		ManifestID:    result.Manifest.ManifestID,
		ObservationID: result.Observation.ObservationID, InputDigest: result.Record.InputDigest,
		ImportRecordSHA256: digestBytes(recordData), ObservationSHA256: digestBytes(observationData),
		AgentReviewPlanRef:   result.Record.AgentReviewPlanRef,
		HypothesisSetRef:     result.Record.HypothesisSetRef,
		ReceiptCollectionRef: result.Record.ReceiptCollectionRef,
		ManifestRef:          result.Record.ManifestRef,
	}, nil
}

func attemptSourceBinding(
	executionFactID string,
	attempt agentshadow.ExecutionAttempt,
	result *agentshadow.Result,
) (SourceBinding, error) {
	var resultBinding *ResultSourceBinding
	if result != nil {
		value, err := resultSourceBinding(*result)
		if err != nil {
			return SourceBinding{}, err
		}
		resultBinding = &value
	}
	var hostFailureBinding *HostFailureSourceBinding
	if attempt.HostFailure != nil {
		if attempt.HostFailureObservationSHA256 == nil {
			return SourceBinding{}, fmt.Errorf(
				"execution attempt %q has host failure without immutable digest",
				attempt.ExecutionID,
			)
		}
		hostFailureBinding = &HostFailureSourceBinding{
			Observation:       *attempt.HostFailure,
			ObservationSHA256: *attempt.HostFailureObservationSHA256,
		}
	} else if attempt.HostFailureObservationSHA256 != nil {
		return SourceBinding{}, fmt.Errorf(
			"execution attempt %q has host failure digest without observation",
			attempt.ExecutionID,
		)
	}
	return SourceBinding{
		ExecutionFactID: executionFactID,
		Kind:            SourceBindingAttempt,
		Attempt: &AttemptSourceBinding{
			Status: attempt.Status, ExecutionID: attempt.ExecutionID,
			IdempotencyKey: attempt.IdempotencyKey,
			SemanticDigest: attempt.SemanticDigest, IntentSHA256: attempt.IntentSHA256,
			CompletionSHA256: cloneStringPointer(attempt.CompletionSHA256),
			AcceptedAt:       attempt.AcceptedAt, ObservedAt: attempt.ObservedAt,
			CompletedAt: cloneTimePointer(attempt.CompletedAt),
			HostFailure: hostFailureBinding, Result: resultBinding,
		},
	}, nil
}

func sparseAttemptFact(
	request RebuildRequest,
	attempt agentshadow.ExecutionAttempt,
) (ExecutionFact, SourceBinding, error) {
	status := ExecutionStatusFailed
	switch attempt.Status {
	case agentshadow.ExecutionStatusFailed:
		status = ExecutionStatusFailed
	case agentshadow.ExecutionStatusCanceled:
		status = ExecutionStatusCanceled
	case agentshadow.ExecutionStatusUnknownOutcome:
		status = ExecutionStatusUnknownOutcome
	default:
		return ExecutionFact{}, SourceBinding{}, fmt.Errorf(
			"execution attempt %q cannot produce a sparse fact with status %q",
			attempt.ExecutionID,
			attempt.Status,
		)
	}
	durationMS := uint64(0)
	reasonCodes := []string{"execution_intent_without_completion"}
	if attempt.HostFailure != nil {
		reasonCodes = []string{string(attempt.HostFailure.ReasonCode)}
	}
	if attempt.CompletedAt != nil {
		duration := attempt.CompletedAt.Sub(attempt.AcceptedAt).Milliseconds()
		if duration < 0 {
			return ExecutionFact{}, SourceBinding{}, fmt.Errorf(
				"execution attempt %q has negative duration",
				attempt.ExecutionID,
			)
		}
		durationMS = uint64(duration)
		reasonCodes = []string{attempt.Failure.Code}
	}
	factID := stableID(
		"agent-execution-attempt-fact",
		request.Scope.TenantID,
		request.Scope.WorkspaceID,
		attempt.ExecutionID,
		attempt.IntentSHA256,
	)
	fact := ExecutionFact{
		SchemaVersion: ExecutionFactSchemaVersion, FactID: factID,
		SourceRunID: attempt.SourceRunID, ExecutionID: attempt.ExecutionID,
		ReviewRunID: attempt.ReviewRunID,
		TenantID:    request.Scope.TenantID, WorkspaceID: request.Scope.WorkspaceID,
		Agent: attempt.Plan.Agent, Provider: attempt.Plan.Provider, Model: attempt.Plan.Model,
		APIProtocol: attempt.Plan.APIProtocol, ExecutionClass: attempt.Plan.ExecutionClass,
		Attestation: attempt.Plan.Attestation, Disposition: ShadowDisposition,
		Status: status, DurationMS: durationMS, ReasonCodes: reasonCodes,
		Usage: ExecutionUsage{
			Completeness: contractsv1alpha1.AgentTokenUsageUnavailable,
		},
		ProvenanceClass: HostObservationProvenance,
		Authority:       DiagnosticAuthority, ObservedAt: attempt.ObservedAt,
	}
	binding, err := attemptSourceBinding(factID, attempt, nil)
	return fact, binding, err
}

func executionStatusFromResult(status contractsv1alpha1.AgentReviewRunStatus) ExecutionStatus {
	switch status {
	case contractsv1alpha1.AgentReviewRunComplete:
		return ExecutionStatusComplete
	case contractsv1alpha1.AgentReviewRunPartial:
		return ExecutionStatusPartial
	default:
		return ExecutionStatusFailed
	}
}

func validateExecutionAttempt(request RebuildRequest, attempt agentshadow.ExecutionAttempt) error {
	if err := attempt.Validate(); err != nil {
		return fmt.Errorf("attempt closure: %w", err)
	}
	if attempt.Scope.TenantID != request.Scope.TenantID ||
		attempt.Scope.WorkspaceID != request.Scope.WorkspaceID {
		return fmt.Errorf("attempt escapes requested scope")
	}
	if !request.Window.contains(attempt.ObservedAt) {
		return fmt.Errorf("attempt is outside the host observation window")
	}
	return nil
}

func validateAttemptResultBinding(
	attempt agentshadow.ExecutionAttempt,
	result agentshadow.Result,
) error {
	if attempt.Status != agentshadow.ExecutionStatusSucceeded || attempt.Manifest == nil {
		return fmt.Errorf("committed result is bound to a non-succeeded attempt")
	}
	if !reflect.DeepEqual(attempt.Plan, result.Plan) ||
		!reflect.DeepEqual(*attempt.Manifest, result.Manifest) ||
		attempt.Scope.TenantID != result.Record.TenantID ||
		attempt.Scope.WorkspaceID != result.Record.WorkspaceID ||
		attempt.IdempotencyKey != result.Record.IdempotencyKey {
		return fmt.Errorf("committed result does not bind its exact execution attempt")
	}
	return nil
}

func (adapter *Adapter) Query(snapshotID string) (ProjectionSnapshot, error) {
	if adapter == nil || adapter.store == nil {
		return ProjectionSnapshot{}, fmt.Errorf("projection adapter is not initialized")
	}
	if err := validateIdentifier("snapshot_id", snapshotID); err != nil {
		return ProjectionSnapshot{}, err
	}
	manifest, err := adapter.loadManifest(snapshotID)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	return adapter.loadSnapshot(manifest)
}

func (adapter *Adapter) persist(
	ctx context.Context,
	snapshot ProjectionSnapshot,
) (ProjectionSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return ProjectionSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("validate projection snapshot: %w", err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("marshal projection snapshot: %w", err)
	}
	ref, err := adapter.store.PutArtifact(data)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("persist projection artifact: %w", err)
	}
	manifest := projectionManifest{
		SchemaVersion: ProjectionManifestSchemaVersion,
		SnapshotID:    snapshot.SnapshotID, SnapshotRef: ref,
		Scope: snapshot.Scope, Window: snapshot.Window,
		GroupBy: slices.Clone(snapshot.GroupBy), BuiltAt: snapshot.BuiltAt,
	}
	if err := manifest.Validate(); err != nil {
		return ProjectionSnapshot{}, err
	}
	objectID := projectionManifestID(snapshot.SnapshotID)
	var existing projectionManifest
	err = adapter.store.PutJSON(objectID, manifest)
	if err == nil {
		return snapshot, nil
	}
	if !errors.Is(err, local.ErrImmutableExists) {
		return ProjectionSnapshot{}, fmt.Errorf("persist projection manifest: %w", err)
	}
	if err := adapter.store.GetJSON(objectID, &existing); err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("%w: load existing manifest: %v", ErrProjectionCorrupt, err)
	}
	if !reflect.DeepEqual(existing, manifest) {
		return ProjectionSnapshot{}, fmt.Errorf(
			"%w: snapshot id %q already binds different facts",
			ErrProjectionConflict,
			snapshot.SnapshotID,
		)
	}
	return snapshot, nil
}

func (adapter *Adapter) loadManifest(snapshotID string) (projectionManifest, error) {
	var manifest projectionManifest
	if err := adapter.store.GetJSON(projectionManifestID(snapshotID), &manifest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return projectionManifest{}, fmt.Errorf("snapshot %q: %w", snapshotID, os.ErrNotExist)
		}
		return projectionManifest{}, fmt.Errorf("%w: load manifest: %v", ErrProjectionCorrupt, err)
	}
	if manifest.SnapshotID != snapshotID {
		return projectionManifest{}, fmt.Errorf("%w: manifest lookup changed identity", ErrProjectionCorrupt)
	}
	if err := manifest.Validate(); err != nil {
		return projectionManifest{}, fmt.Errorf("%w: invalid manifest: %v", ErrProjectionCorrupt, err)
	}
	return manifest, nil
}

func (adapter *Adapter) loadSnapshot(
	manifest projectionManifest,
) (ProjectionSnapshot, error) {
	data, err := adapter.store.ReadArtifact(manifest.SnapshotRef)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("%w: read snapshot artifact: %v", ErrProjectionCorrupt, err)
	}
	snapshot, err := decodeProjectionSnapshot(data)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("%w: decode snapshot: %v", ErrProjectionCorrupt, err)
	}
	if snapshot.SnapshotID != manifest.SnapshotID || snapshot.Scope != manifest.Scope ||
		snapshot.Window != manifest.Window || !slices.Equal(snapshot.GroupBy, manifest.GroupBy) ||
		!snapshot.BuiltAt.Equal(manifest.BuiltAt) {
		return ProjectionSnapshot{}, fmt.Errorf("%w: snapshot does not match manifest", ErrProjectionCorrupt)
	}
	return snapshot, nil
}

func decodeProjectionSnapshot(data []byte) (ProjectionSnapshot, error) {
	if err := rejectDuplicateProjectionJSONFields(data); err != nil {
		return ProjectionSnapshot{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot ProjectionSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return ProjectionSnapshot{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ProjectionSnapshot{}, fmt.Errorf("multiple JSON values")
		}
		return ProjectionSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return ProjectionSnapshot{}, err
	}
	return snapshot, nil
}

func (request RebuildRequest) Validate() error {
	if err := validateIdentifier("snapshot_id", request.SnapshotID); err != nil {
		return err
	}
	if err := request.Scope.Validate(); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	if err := request.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if err := validateGroupBy(request.GroupBy); err != nil {
		return err
	}
	if err := validateUTC("built_at", request.BuiltAt); err != nil {
		return err
	}
	if request.BuiltAt.Before(request.Window.EndExclusive) {
		return fmt.Errorf("built_at must not precede the closed observation window")
	}
	return nil
}

func (snapshot ProjectionSnapshot) Validate() error {
	if snapshot.SchemaVersion != ProjectionSnapshotSchemaVersion {
		return fmt.Errorf("unsupported projection snapshot schema %q", snapshot.SchemaVersion)
	}
	request := RebuildRequest{
		SnapshotID: snapshot.SnapshotID, Scope: snapshot.Scope,
		Window: snapshot.Window, GroupBy: snapshot.GroupBy, BuiltAt: snapshot.BuiltAt,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if snapshot.PolicyRevision != ProjectionPolicyRevision {
		return fmt.Errorf("unsupported projection policy %q", snapshot.PolicyRevision)
	}
	if snapshot.SourceBindings == nil {
		return fmt.Errorf("source_bindings must be an explicit array")
	}
	if err := snapshot.Facts.Validate(); err != nil {
		return fmt.Errorf("facts: %w", err)
	}
	if snapshot.Facts.Window != snapshot.Window {
		return fmt.Errorf("fact window does not match snapshot")
	}
	if len(snapshot.SourceBindings) != len(snapshot.Facts.Executions) {
		return fmt.Errorf("every execution fact requires one source binding")
	}
	for index, binding := range snapshot.SourceBindings {
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("source_bindings[%d]: %w", index, err)
		}
		if err := binding.reconcile(snapshot.Facts.Executions[index]); err != nil {
			return fmt.Errorf("source_bindings[%d]: %w", index, err)
		}
		if index > 0 && binding.ExecutionFactID <= snapshot.SourceBindings[index-1].ExecutionFactID {
			return fmt.Errorf("source bindings must be uniquely sorted")
		}
	}
	for _, fact := range snapshot.Facts.Executions {
		if !sameScope(snapshot.Scope, fact) {
			return fmt.Errorf("execution fact escapes snapshot scope")
		}
	}
	expected, err := Project(snapshot.Facts, snapshot.GroupBy)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, snapshot.Projection) {
		return fmt.Errorf("projection does not exactly match frozen facts")
	}
	return nil
}

func (binding SourceBinding) Validate() error {
	if err := validateIdentifier("execution_fact_id", binding.ExecutionFactID); err != nil {
		return err
	}
	switch binding.Kind {
	case SourceBindingResult:
		if binding.Result == nil || binding.Attempt != nil {
			return fmt.Errorf("result source binding requires result only")
		}
		return binding.Result.Validate()
	case SourceBindingAttempt:
		if binding.Attempt == nil || binding.Result != nil {
			return fmt.Errorf("attempt source binding requires attempt only")
		}
		return binding.Attempt.Validate()
	default:
		return fmt.Errorf("unsupported source binding kind %q", binding.Kind)
	}
}

func (binding ResultSourceBinding) Validate() error {
	for name, value := range map[string]string{
		"manifest_id": binding.ManifestID, "observation_id": binding.ObservationID,
	} {
		if err := validateOpaque(name, value, 256); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"input_digest":         binding.InputDigest,
		"import_record_sha256": binding.ImportRecordSHA256,
		"observation_sha256":   binding.ObservationSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]struct {
		binding  contractsv1alpha1.ArtifactBinding
		contract string
	}{
		"agent_review_plan_ref":  {binding.AgentReviewPlanRef, contractsv1alpha1.AgentReviewPlanSchemaVersion},
		"hypothesis_set_ref":     {binding.HypothesisSetRef, contractsv1alpha1.ReviewHypothesisSetSchemaVersion},
		"receipt_collection_ref": {binding.ReceiptCollectionRef, contractsv1alpha1.AgentExecutionReceiptCollectionContract},
		"manifest_ref":           {binding.ManifestRef, contractsv1alpha1.AgentReviewResultManifestSchemaVersion},
	} {
		if value.binding.Contract != value.contract || value.binding.Ref.URI == "" ||
			value.binding.Ref.SizeBytes <= 0 {
			return fmt.Errorf("%s does not bind its expected contract", name)
		}
		if err := validateSHA256(name+".sha256", value.binding.Ref.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func (binding AttemptSourceBinding) Validate() error {
	if err := validateOpaque("execution_id", binding.ExecutionID, 256); err != nil {
		return err
	}
	if err := validateOpaque("idempotency_key", binding.IdempotencyKey, 256); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"semantic_digest": binding.SemanticDigest,
		"intent_sha256":   binding.IntentSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if err := validateUTC("accepted_at", binding.AcceptedAt); err != nil {
		return err
	}
	if err := validateUTC("observed_at", binding.ObservedAt); err != nil {
		return err
	}
	if binding.ObservedAt.Before(binding.AcceptedAt) {
		return fmt.Errorf("attempt source binding observed_at precedes accepted_at")
	}
	if binding.HostFailure != nil {
		if err := binding.HostFailure.Validate(); err != nil {
			return fmt.Errorf("host_failure: %w", err)
		}
		observation := binding.HostFailure.Observation
		if observation.IdempotencyKey != binding.IdempotencyKey ||
			observation.SemanticDigest != binding.SemanticDigest ||
			observation.ExecutionID != binding.ExecutionID ||
			observation.IntentSHA256 != binding.IntentSHA256 {
			return fmt.Errorf("host failure observation does not bind the execution intent")
		}
		if observation.ObservedAt.Before(binding.AcceptedAt) {
			return fmt.Errorf("host failure observation precedes attempt acceptance")
		}
		if observation.TimeSource ==
			agentshadow.ExecutionHostObservationAcceptedLowerBound &&
			!observation.ObservedAt.Equal(binding.AcceptedAt) {
			return fmt.Errorf("host failure accepted_at lower bound changed")
		}
	}
	if binding.Status == agentshadow.ExecutionStatusUnknownOutcome {
		if binding.CompletionSHA256 != nil || binding.CompletedAt != nil ||
			binding.Result != nil {
			return fmt.Errorf("unknown-outcome source binding forbids terminal evidence")
		}
		wantObservedAt := binding.AcceptedAt
		if binding.HostFailure != nil {
			wantObservedAt = binding.HostFailure.Observation.ObservedAt
		}
		if !binding.ObservedAt.Equal(wantObservedAt) {
			return fmt.Errorf("unknown-outcome source binding has invalid observation time")
		}
		return nil
	}
	if binding.CompletionSHA256 == nil || binding.CompletedAt == nil {
		return fmt.Errorf("terminal attempt source binding requires completion evidence")
	}
	if err := validateSHA256("completion_sha256", *binding.CompletionSHA256); err != nil {
		return err
	}
	if err := validateUTC("completed_at", *binding.CompletedAt); err != nil {
		return err
	}
	if binding.CompletedAt.Before(binding.AcceptedAt) ||
		binding.CompletedAt.After(binding.ObservedAt) {
		return fmt.Errorf("attempt source binding completion time is inconsistent")
	}
	switch binding.Status {
	case agentshadow.ExecutionStatusSucceeded:
		if binding.Result == nil {
			return fmt.Errorf("succeeded attempt source binding requires result closure")
		}
		return binding.Result.Validate()
	case agentshadow.ExecutionStatusFailed, agentshadow.ExecutionStatusCanceled:
		if binding.Result != nil {
			return fmt.Errorf("failed/canceled source binding forbids result closure")
		}
		return nil
	default:
		return fmt.Errorf("unsupported attempt source status %q", binding.Status)
	}
}

func (binding HostFailureSourceBinding) Validate() error {
	if err := validateSHA256("observation_sha256", binding.ObservationSHA256); err != nil {
		return err
	}
	observationData, err := json.Marshal(binding.Observation)
	if err != nil {
		return fmt.Errorf("marshal host failure observation: %w", err)
	}
	if len(observationData) > 4<<10 {
		return fmt.Errorf("host failure observation exceeds 4096 bytes")
	}
	if digestBytes(observationData) != binding.ObservationSHA256 {
		return fmt.Errorf("host failure observation digest changed")
	}
	if binding.Observation.SchemaVersion !=
		agentshadow.ExecutionHostFailureObservationSchemaVersion {
		return fmt.Errorf(
			"unsupported host failure observation schema %q",
			binding.Observation.SchemaVersion,
		)
	}
	if err := validateOpaque("tenant_id", binding.Observation.Scope.TenantID, 256); err != nil {
		return err
	}
	if err := validateOpaque("workspace_id", binding.Observation.Scope.WorkspaceID, 256); err != nil {
		return err
	}
	if err := validateOpaque("idempotency_key", binding.Observation.IdempotencyKey, 256); err != nil {
		return err
	}
	if err := validateOpaque("execution_id", binding.Observation.ExecutionID, 256); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"semantic_digest": binding.Observation.SemanticDigest,
		"intent_sha256":   binding.Observation.IntentSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if err := validateUTC("observed_at", binding.Observation.ObservedAt); err != nil {
		return err
	}
	want := map[agentshadow.ExecutionHostFailureStage]agentshadow.ExecutionHostFailureCode{
		agentshadow.ExecutionHostFailureWorkerRun:        agentshadow.ExecutionHostFailureRunnerUnconfirmed,
		agentshadow.ExecutionHostFailureResultDecode:     agentshadow.ExecutionHostFailureResultRejected,
		agentshadow.ExecutionHostFailureResultBinding:    agentshadow.ExecutionHostFailureBindingRejected,
		agentshadow.ExecutionHostFailureEvidenceMapping:  agentshadow.ExecutionHostFailureMappingRejected,
		agentshadow.ExecutionHostFailureEvidenceEncoding: agentshadow.ExecutionHostFailureEncodingFailed,
		agentshadow.ExecutionHostFailureShadowImport:     agentshadow.ExecutionHostFailureImportUnconfirmed,
		agentshadow.ExecutionHostFailureCompletionCommit: agentshadow.ExecutionHostFailureCompletionUnconfirmed,
	}
	if expected, exists := want[binding.Observation.Stage]; !exists ||
		expected != binding.Observation.ReasonCode {
		return fmt.Errorf(
			"unsupported host failure taxonomy %q/%q",
			binding.Observation.Stage,
			binding.Observation.ReasonCode,
		)
	}
	switch binding.Observation.TimeSource {
	case agentshadow.ExecutionHostObservationClock,
		agentshadow.ExecutionHostObservationAcceptedLowerBound:
		return nil
	default:
		return fmt.Errorf(
			"unsupported host failure time source %q",
			binding.Observation.TimeSource,
		)
	}
}

func (binding SourceBinding) reconcile(fact ExecutionFact) error {
	if binding.ExecutionFactID != fact.FactID {
		return fmt.Errorf("source binding does not bind execution fact id")
	}
	if binding.Kind == SourceBindingResult {
		if binding.Result.ManifestID != fact.ManifestID ||
			binding.Result.ObservationID != fact.ObservationID {
			return fmt.Errorf("result source binding does not reconcile with execution fact")
		}
		return nil
	}
	attempt := binding.Attempt
	if attempt.ExecutionID != fact.ExecutionID || !attempt.ObservedAt.Equal(fact.ObservedAt) {
		return fmt.Errorf("attempt source binding does not reconcile identity and observation time")
	}
	if attempt.HostFailure != nil &&
		(attempt.HostFailure.Observation.Scope.TenantID != fact.TenantID ||
			attempt.HostFailure.Observation.Scope.WorkspaceID != fact.WorkspaceID) {
		return fmt.Errorf("host failure source binding escapes execution fact scope")
	}
	if attempt.Result != nil {
		if attempt.Result.ManifestID != fact.ManifestID ||
			attempt.Result.ObservationID != fact.ObservationID {
			return fmt.Errorf("attempt result closure does not reconcile with execution fact")
		}
	} else if fact.ManifestID != "" || fact.ObservationID != "" {
		return fmt.Errorf("result-less attempt cannot bind manifest provenance")
	}
	switch attempt.Status {
	case agentshadow.ExecutionStatusSucceeded:
		if fact.Status != ExecutionStatusComplete && fact.Status != ExecutionStatusPartial &&
			fact.Status != ExecutionStatusFailed {
			return fmt.Errorf("succeeded attempt has incompatible result status")
		}
	case agentshadow.ExecutionStatusFailed:
		if fact.Status != ExecutionStatusFailed {
			return fmt.Errorf("failed attempt has incompatible fact status")
		}
	case agentshadow.ExecutionStatusCanceled:
		if fact.Status != ExecutionStatusCanceled {
			return fmt.Errorf("canceled attempt has incompatible fact status")
		}
	case agentshadow.ExecutionStatusUnknownOutcome:
		if fact.Status != ExecutionStatusUnknownOutcome {
			return fmt.Errorf("unknown attempt has incompatible fact status")
		}
		wantReasonCodes := []string{"execution_intent_without_completion"}
		if attempt.HostFailure != nil {
			wantReasonCodes = []string{string(attempt.HostFailure.Observation.ReasonCode)}
		}
		if !reflect.DeepEqual(fact.ReasonCodes, wantReasonCodes) {
			return fmt.Errorf("unknown attempt reason codes do not bind host diagnosis")
		}
	}
	return nil
}

func (manifest projectionManifest) Validate() error {
	if manifest.SchemaVersion != ProjectionManifestSchemaVersion {
		return fmt.Errorf("unsupported projection manifest schema %q", manifest.SchemaVersion)
	}
	request := RebuildRequest{
		SnapshotID: manifest.SnapshotID, Scope: manifest.Scope,
		Window: manifest.Window, GroupBy: manifest.GroupBy, BuiltAt: manifest.BuiltAt,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if manifest.SnapshotRef.URI != "artifact://local/sha256/"+manifest.SnapshotRef.SHA256 ||
		manifest.SnapshotRef.SizeBytes <= 0 {
		return fmt.Errorf("snapshot_ref is not an exact local content binding")
	}
	return validateSHA256("snapshot_ref.sha256", manifest.SnapshotRef.SHA256)
}

func projectionManifestID(snapshotID string) string {
	return projectionManifestRoot + digestStrings(snapshotID)
}

func stableID(prefix string, values ...string) string {
	return prefix + "-" + digestStrings(values...)
}

func digestStrings(values ...string) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte(fmt.Sprintf("%d:", len(value))))
		_, _ = digest.Write([]byte(value))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTokenUsage(
	usage contractsv1alpha1.AgentTokenUsage,
) contractsv1alpha1.AgentTokenUsage {
	result := usage
	result.UnavailableReasonCode = cloneStringPointer(usage.UnavailableReasonCode)
	if usage.ReasoningTokens != nil {
		copy := *usage.ReasoningTokens
		result.ReasoningTokens = &copy
	}
	return result
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
