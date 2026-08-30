package agentshadow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"argus.local/argus/internal/artifactrepo"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type Service struct {
	repository *Repository
}

func NewService(repository *Repository) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("agent review shadow repository is required")
	}
	return &Service{repository: repository}, nil
}

func (service *Service) Import(
	ctx context.Context,
	request ImportRequest,
) (Result, error) {
	if err := contextError(ctx); err != nil {
		return Result{}, err
	}
	if err := validateScope(request.Scope); err != nil {
		return Result{}, err
	}
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return Result{}, err
	}
	if err := validateInputSize("plan", request.Plan, maxPlanBytes); err != nil {
		return Result{}, err
	}
	if err := validateInputSize(
		"hypothesis set",
		request.HypothesisSet,
		maxHypothesisBytes,
	); err != nil {
		return Result{}, err
	}
	if err := validateInputSize(
		"raw candidate collection",
		request.RawCandidateCollection,
		maxRawCandidateBytes,
	); err != nil {
		return Result{}, err
	}
	if err := validateInputSize(
		"receipt collection",
		request.ReceiptCollection,
		maxReceiptBytes,
	); err != nil {
		return Result{}, err
	}
	if err := validateInputSize(
		"task evidence collection",
		request.TaskEvidenceCollection,
		maxTaskEvidenceBytes,
	); err != nil {
		return Result{}, err
	}

	plan, err := contractsv1alpha1.DecodeAgentReviewPlan(request.Plan)
	if err != nil {
		return Result{}, err
	}
	set, err := contractsv1alpha1.DecodeReviewHypothesisSet(request.HypothesisSet)
	if err != nil {
		return Result{}, err
	}
	raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(
		request.RawCandidateCollection,
	)
	if err != nil {
		return Result{}, err
	}
	taskEvidence, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(
		request.TaskEvidenceCollection,
	)
	if err != nil {
		return Result{}, err
	}
	collection, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(
		request.ReceiptCollection,
	)
	if err != nil {
		return Result{}, err
	}
	planData, err := json.Marshal(plan)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize agent review plan: %w", err)
	}
	setData, err := json.Marshal(set)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize hypothesis set: %w", err)
	}
	rawData, err := json.Marshal(raw)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize raw candidate collection: %w", err)
	}
	taskEvidenceData, err := json.Marshal(taskEvidence)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize task evidence collection: %w", err)
	}
	collectionData, err := json.Marshal(collection)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize receipt collection: %w", err)
	}
	inputDigest := digestByteSlices(planData, setData, rawData, taskEvidenceData, collectionData)
	acceptedAt, err := service.repository.normalizedNow()
	if err != nil {
		return Result{}, err
	}
	proposedIntent := importIntent{
		SchemaVersion: importIntentSchemaVersion,
		TenantID:      request.Scope.TenantID, WorkspaceID: request.Scope.WorkspaceID,
		IdempotencyKey: request.IdempotencyKey, InputDigest: inputDigest,
		AcceptedAt: acceptedAt,
	}

	// Validate every cross-contract invariant before creating the durable
	// intent. Preview refs are exact content bindings and are replaced by
	// governed refs after the preflight succeeds.
	previewManifest, err := buildManifest(
		proposedIntent,
		plan,
		set,
		raw,
		taskEvidence,
		collection,
		previewBinding(contractsv1alpha1.AgentReviewPlanSchemaVersion, planData),
		previewBinding(contractsv1alpha1.ReviewHypothesisSetSchemaVersion, setData),
		previewBinding(contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion, rawData),
		previewBinding(contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion, taskEvidenceData),
		previewBinding(
			contractsv1alpha1.AgentExecutionReceiptCollectionContract,
			collectionData,
		),
	)
	if err != nil {
		return Result{}, err
	}
	if err := contractsv1alpha1.ValidateAgentReviewResultManifestBindings(
		previewManifest,
		plan,
		set,
		collection,
		raw,
	); err != nil {
		return Result{}, fmt.Errorf("validate shadow result bindings: %w", err)
	}
	if err := service.validateFrozenInputClosure(
		ctx,
		request.Scope,
		plan,
		set,
	); err != nil {
		return Result{}, err
	}
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(taskEvidence, plan, collection); err != nil {
		return Result{}, fmt.Errorf("validate task evidence bindings: %w", err)
	}

	intent, err := service.repository.acquireIntent(
		ctx,
		request.Scope,
		request.IdempotencyKey,
		inputDigest,
		acceptedAt,
	)
	if err != nil {
		return Result{}, err
	}
	planRef, err := service.repository.putArtifact(
		ctx,
		request.Scope,
		intent,
		"plan",
		contractsv1alpha1.AgentReviewPlanSchemaVersion,
		planData,
	)
	if err != nil {
		return Result{}, err
	}
	setRef, err := service.repository.putArtifact(
		ctx,
		request.Scope,
		intent,
		"hypothesis-set",
		contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		setData,
	)
	if err != nil {
		return Result{}, err
	}
	rawRef, err := service.repository.putArtifact(
		ctx,
		request.Scope,
		intent,
		"raw-candidate-collection",
		contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		rawData,
	)
	if err != nil {
		return Result{}, err
	}
	taskEvidenceRef, err := service.repository.putArtifact(
		ctx,
		request.Scope,
		intent,
		"task-evidence-collection",
		contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		taskEvidenceData,
	)
	if err != nil {
		return Result{}, err
	}
	receiptRef, err := service.repository.putArtifact(
		ctx,
		request.Scope,
		intent,
		"receipt-collection",
		contractsv1alpha1.AgentExecutionReceiptCollectionContract,
		collectionData,
	)
	if err != nil {
		return Result{}, err
	}
	manifest, err := buildManifest(
		intent,
		plan,
		set,
		raw,
		taskEvidence,
		collection,
		planRef,
		setRef,
		rawRef,
		taskEvidenceRef,
		receiptRef,
	)
	if err != nil {
		return Result{}, err
	}
	if err := contractsv1alpha1.ValidateAgentReviewResultManifestBindings(
		manifest,
		plan,
		set,
		collection,
		raw,
	); err != nil {
		return Result{}, fmt.Errorf("validate persisted shadow result bindings: %w", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(taskEvidence, plan, collection); err != nil {
		return Result{}, fmt.Errorf("validate persisted task evidence bindings: %w", err)
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize result manifest: %w", err)
	}
	manifestRef, err := service.repository.putArtifact(
		ctx,
		request.Scope,
		intent,
		"result-manifest",
		contractsv1alpha1.AgentReviewResultManifestSchemaVersion,
		manifestData,
	)
	if err != nil {
		return Result{}, err
	}
	observation := buildObservation(intent, plan, set, collection, manifest)
	if err := contractsv1alpha1.ValidateAgentReviewObservationBinding(
		observation,
		manifest,
		plan,
	); err != nil {
		return Result{}, fmt.Errorf("validate host observation: %w", err)
	}
	record := ImportRecord{
		SchemaVersion: ImportRecordSchemaVersion,
		TenantID:      request.Scope.TenantID, WorkspaceID: request.Scope.WorkspaceID,
		IdempotencyKey: request.IdempotencyKey, InputDigest: inputDigest,
		AcceptedAt: intent.AcceptedAt,
		ManifestID: manifest.ManifestID, ObservationID: observation.ObservationID,
		ObservationStream:  observationStream(request.Scope, manifest.ManifestID),
		AgentReviewPlanRef: planRef, HypothesisSetRef: setRef,
		RawCandidateCollectionRef: rawRef,
		TaskEvidenceCollectionRef: taskEvidenceRef,
		ReceiptCollectionRef:      receiptRef, ManifestRef: manifestRef,
	}
	if err := service.repository.appendObservation(ctx, record, observation); err != nil {
		return Result{}, err
	}
	if _, err := service.repository.commit(record); err != nil {
		return Result{}, err
	}
	return service.queryCommitted(ctx, QueryRequest{
		Scope: request.Scope, ManifestID: manifest.ManifestID,
	}, true)
}

func (service *Service) Query(
	ctx context.Context,
	request QueryRequest,
) (Result, error) {
	result, err := service.queryCommitted(ctx, request, true)
	if errors.Is(err, artifactrepo.ErrTombstoned) {
		return service.queryCommitted(ctx, request, false)
	}
	return result, err
}

// QueryRedacted revalidates the committed result without reading exact task
// evidence bytes. It is the default platform and CLI projection and continues
// to work after retention revocation.
func (service *Service) QueryRedacted(
	ctx context.Context,
	request QueryRequest,
) (Result, error) {
	return service.queryCommitted(ctx, request, false)
}

func (service *Service) queryCommitted(
	ctx context.Context,
	request QueryRequest,
	includeTaskEvidence bool,
) (Result, error) {
	if err := contextError(ctx); err != nil {
		return Result{}, err
	}
	if err := validateScope(request.Scope); err != nil {
		return Result{}, err
	}
	if err := validateIdentifier("manifest_id", request.ManifestID); err != nil {
		return Result{}, err
	}
	record, err := service.repository.loadRecord(request.ManifestID)
	if err != nil {
		return Result{}, err
	}
	if record.TenantID != request.Scope.TenantID ||
		record.WorkspaceID != request.Scope.WorkspaceID {
		return Result{}, artifactrepo.ErrUnauthorized
	}
	intent, err := service.repository.loadIntent(record)
	if err != nil {
		return Result{}, err
	}
	planData, err := service.repository.resolveArtifact(
		ctx,
		request.Scope,
		record.AgentReviewPlanRef,
	)
	if err != nil {
		return Result{}, fmt.Errorf("resolve committed plan: %w", err)
	}
	setData, err := service.repository.resolveArtifact(
		ctx,
		request.Scope,
		record.HypothesisSetRef,
	)
	if err != nil {
		return Result{}, fmt.Errorf("resolve committed hypothesis set: %w", err)
	}
	rawData, err := service.repository.resolveArtifact(
		ctx,
		request.Scope,
		record.RawCandidateCollectionRef,
	)
	if err != nil {
		return Result{}, fmt.Errorf("resolve committed raw candidate collection: %w", err)
	}
	taskEvidenceRecord, err := service.repository.inspectTaskEvidence(
		ctx, request.Scope, record.TaskEvidenceCollectionRef,
	)
	if err != nil {
		return Result{}, fmt.Errorf("inspect committed task evidence collection: %w", err)
	}
	var taskEvidenceData []byte
	var taskEvidence contractsv1alpha1.AgentReviewTaskEvidenceCollection
	if includeTaskEvidence {
		taskEvidenceData, taskEvidenceRecord, err = service.repository.resolveTaskEvidenceForProcessing(
			ctx,
			request.Scope,
			record.TaskEvidenceCollectionRef,
		)
		if err != nil {
			return Result{}, fmt.Errorf("resolve committed task evidence collection: %w", err)
		}
	}
	receiptData, err := service.repository.resolveArtifact(
		ctx,
		request.Scope,
		record.ReceiptCollectionRef,
	)
	if err != nil {
		return Result{}, fmt.Errorf("resolve committed receipt collection: %w", err)
	}
	manifestData, err := service.repository.resolveArtifact(
		ctx,
		request.Scope,
		record.ManifestRef,
	)
	if err != nil {
		return Result{}, fmt.Errorf("resolve committed manifest: %w", err)
	}
	plan, err := contractsv1alpha1.DecodeAgentReviewPlan(planData)
	if err != nil {
		return Result{}, fmt.Errorf("%w: committed plan: %v", ErrCorrupt, err)
	}
	set, err := contractsv1alpha1.DecodeReviewHypothesisSet(setData)
	if err != nil {
		return Result{}, fmt.Errorf("%w: committed hypothesis set: %v", ErrCorrupt, err)
	}
	raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawData)
	if err != nil {
		return Result{}, fmt.Errorf("%w: committed raw candidate collection: %v", ErrCorrupt, err)
	}
	if includeTaskEvidence {
		taskEvidence, err = contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(taskEvidenceData)
		if err != nil {
			return Result{}, fmt.Errorf("%w: committed task evidence collection: %v", ErrCorrupt, err)
		}
	}
	collection, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(receiptData)
	if err != nil {
		return Result{}, fmt.Errorf("%w: committed receipt collection: %v", ErrCorrupt, err)
	}
	manifest, err := contractsv1alpha1.DecodeAgentReviewResultManifest(manifestData)
	if err != nil {
		return Result{}, fmt.Errorf("%w: committed result manifest: %v", ErrCorrupt, err)
	}
	if includeTaskEvidence && digestByteSlices(planData, setData, rawData, taskEvidenceData, receiptData) != record.InputDigest {
		return Result{}, fmt.Errorf("%w: committed input digest changed", ErrCorrupt)
	}
	if manifest.AgentReviewPlanRef != record.AgentReviewPlanRef ||
		manifest.HypothesisSetRef != record.HypothesisSetRef ||
		manifest.RawCandidateCollectionRef != record.RawCandidateCollectionRef ||
		manifest.AgentTaskEvidenceRef != record.TaskEvidenceCollectionRef ||
		manifest.AgentExecutionReceiptRef != record.ReceiptCollectionRef ||
		manifest.ManifestID != record.ManifestID {
		return Result{}, fmt.Errorf("%w: manifest does not match commit record", ErrCorrupt)
	}
	if err := contractsv1alpha1.ValidateAgentReviewResultManifestBindings(
		manifest,
		plan,
		set,
		collection,
		raw,
	); err != nil {
		return Result{}, fmt.Errorf("%w: manifest bindings: %v", ErrCorrupt, err)
	}
	if includeTaskEvidence {
		if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(taskEvidence, plan, collection); err != nil {
			return Result{}, fmt.Errorf("%w: task evidence bindings: %v", ErrCorrupt, err)
		}
	}
	expectedManifest, err := buildManifest(
		intent,
		plan,
		set,
		raw,
		taskEvidence,
		collection,
		record.AgentReviewPlanRef,
		record.HypothesisSetRef,
		record.RawCandidateCollectionRef,
		record.TaskEvidenceCollectionRef,
		record.ReceiptCollectionRef,
	)
	if err != nil || !reflect.DeepEqual(manifest, expectedManifest) {
		return Result{}, fmt.Errorf("%w: manifest is not the host recomputation", ErrCorrupt)
	}
	if err := service.validateFrozenInputClosure(
		ctx,
		request.Scope,
		plan,
		set,
	); err != nil {
		return Result{}, fmt.Errorf("%w: committed frozen input closure: %v", ErrCorrupt, err)
	}
	observation, err := service.repository.loadObservation(record)
	if err != nil {
		return Result{}, err
	}
	if err := contractsv1alpha1.ValidateAgentReviewObservationBinding(
		observation,
		manifest,
		plan,
	); err != nil {
		return Result{}, fmt.Errorf("%w: observation binding: %v", ErrCorrupt, err)
	}
	expectedObservation := buildObservation(intent, plan, set, collection, manifest)
	if !reflect.DeepEqual(observation, expectedObservation) {
		return Result{}, fmt.Errorf("%w: observation is not the host recomputation", ErrCorrupt)
	}
	summary := taskEvidenceSummary(record.TaskEvidenceCollectionRef, taskEvidence, taskEvidenceRecord.State)
	if !includeTaskEvidence {
		summary.Completeness = ""
		summary.ReasonCodes = nil
		summary.TaskExecutionCount = 0
	}
	return Result{
		Record: record, Plan: plan, HypothesisSet: set,
		RawCandidateCollection: raw,
		TaskEvidenceCollection: taskEvidence,
		TaskEvidence:           summary,
		ReceiptCollection:      collection, Manifest: manifest, Observation: observation,
	}, nil
}

func taskEvidenceSummary(
	ref contractsv1alpha1.ArtifactBinding,
	evidence contractsv1alpha1.AgentReviewTaskEvidenceCollection,
	state artifactrepo.State,
) TaskEvidenceSummary {
	return TaskEvidenceSummary{
		Ref:                ref,
		ContentPolicy:      contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		Completeness:       evidence.Completeness,
		ReasonCodes:        slices.Clone(evidence.ReasonCodes),
		TaskExecutionCount: len(evidence.TaskExecutions),
		State:              string(state),
		ExportPolicy:       "deny",
		RetentionPolicy:    "until_explicit_revocation",
		DeletionSemantics:  "logical_tombstone_shared_content_gc_deferred",
	}
}

func buildManifest(
	intent importIntent,
	plan contractsv1alpha1.AgentReviewPlan,
	set contractsv1alpha1.ReviewHypothesisSet,
	raw contractsv1alpha1.AgentReviewRawCandidateCollection,
	taskEvidence contractsv1alpha1.AgentReviewTaskEvidenceCollection,
	collection contractsv1alpha1.AgentExecutionReceiptCollection,
	planRef contractsv1alpha1.ArtifactBinding,
	setRef contractsv1alpha1.ArtifactBinding,
	rawRef contractsv1alpha1.ArtifactBinding,
	taskEvidenceRef contractsv1alpha1.ArtifactBinding,
	receiptRef contractsv1alpha1.ArtifactBinding,
) (contractsv1alpha1.AgentReviewResultManifest, error) {
	summary, status, err := contractsv1alpha1.SummarizeAgentReviewResult(
		set,
		collection.Receipts,
	)
	if err != nil {
		return contractsv1alpha1.AgentReviewResultManifest{}, err
	}
	return contractsv1alpha1.AgentReviewResultManifest{
		SchemaVersion: contractsv1alpha1.AgentReviewResultManifestSchemaVersion,
		ManifestID:    manifestID(intent), PlanID: plan.PlanID,
		HypothesisSetID: set.HypothesisSetID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest:         plan.TargetDigest,
		ExecutionSnapshotRef: plan.ExecutionSnapshotRef,
		ReviewInputRef:       plan.ReviewInputRef,
		AgentReviewPlanRef:   planRef, HypothesisSetRef: setRef,
		RawCandidateCollectionRef: rawRef,
		AgentTaskEvidenceRef:      taskEvidenceRef,
		AgentExecutionReceiptRef:  receiptRef,
		ProducerClass:             "argus_go_host", Disposition: "shadow_only",
		Status: status, Summary: summary,
		RecordedAt: intent.AcceptedAt,
	}, nil
}

func buildObservation(
	intent importIntent,
	plan contractsv1alpha1.AgentReviewPlan,
	set contractsv1alpha1.ReviewHypothesisSet,
	collection contractsv1alpha1.AgentExecutionReceiptCollection,
	manifest contractsv1alpha1.AgentReviewResultManifest,
) contractsv1alpha1.AgentReviewObservation {
	return contractsv1alpha1.AgentReviewObservation{
		SchemaVersion: contractsv1alpha1.AgentReviewObservationSchemaVersion,
		ObservationID: observationID(intent), ManifestID: manifest.ManifestID,
		SourceRunID: manifest.SourceRunID, ExecutionID: manifest.ExecutionID,
		ReviewRunID: manifest.ReviewRunID, Sequence: 1,
		Kind:          contractsv1alpha1.AgentReviewObservationShadowResultRecorded,
		ProducerClass: "argus_go_host", Disposition: "shadow_only",
		Status: manifest.Status, Agent: plan.Agent, Provider: plan.Provider,
		Model: plan.Model, Summary: manifest.Summary,
		DurationMS:  durationMS(collection),
		ReasonCodes: observationReasonCodes(set, collection),
		RecordedAt:  manifest.RecordedAt,
	}
}

func observationReasonCodes(
	set contractsv1alpha1.ReviewHypothesisSet,
	collection contractsv1alpha1.AgentExecutionReceiptCollection,
) []string {
	unique := make(map[string]struct{})
	for _, reason := range set.CompletenessReasons {
		unique[reason] = struct{}{}
	}
	for _, gap := range set.Coverage.Gaps {
		unique[gap.ReasonCode] = struct{}{}
	}
	for _, receipt := range collection.Receipts {
		if receipt.FailureReasonCode != nil {
			unique[*receipt.FailureReasonCode] = struct{}{}
		}
		if receipt.Usage.UnavailableReasonCode != nil {
			unique[*receipt.Usage.UnavailableReasonCode] = struct{}{}
		}
	}
	reasons := make([]string, 0, len(unique))
	for reason := range unique {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	return reasons
}

func durationMS(collection contractsv1alpha1.AgentExecutionReceiptCollection) uint64 {
	if len(collection.Receipts) == 0 {
		return 0
	}
	startedAt := collection.Receipts[0].StartedAt
	finishedAt := collection.Receipts[0].FinishedAt
	for _, receipt := range collection.Receipts[1:] {
		if receipt.StartedAt.Before(startedAt) {
			startedAt = receipt.StartedAt
		}
		if receipt.FinishedAt.After(finishedAt) {
			finishedAt = receipt.FinishedAt
		}
	}
	return uint64(finishedAt.Sub(startedAt).Milliseconds())
}

func previewBinding(
	contract string,
	content []byte,
) contractsv1alpha1.ArtifactBinding {
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI:       "artifact://local/sha256/" + sha,
			SHA256:    sha,
			SizeBytes: int64(len(content)),
		},
		Contract: contract,
	}
}

func manifestID(intent importIntent) string {
	return "agent-review-manifest-" + digestStrings(
		intent.TenantID,
		intent.WorkspaceID,
		intent.IdempotencyKey,
		intent.InputDigest,
	)
}

func observationID(intent importIntent) string {
	return "agent-review-observation-" + digestStrings(manifestID(intent))
}

func digestByteSlices(values ...[]byte) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte(fmt.Sprintf("%d:", len(value))))
		_, _ = digest.Write(value)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func validateScope(scope Scope) error {
	if err := artifactSubject(scope).Validate(); err != nil {
		return fmt.Errorf("validate import scope: %w", err)
	}
	return nil
}

func validateIdempotencyKey(value string) error {
	return validateIdentifier("idempotency_key", value)
}

func validateIdentifier(name string, value string) error {
	mutation := artifactrepo.Mutation{
		IdempotencyKey: value,
		Actor:          hostActor,
		Audit:          "validate portable identifier",
		At:             time.Unix(1, 0).UTC(),
	}
	if err := mutation.Validate(); err != nil {
		return fmt.Errorf("%s is invalid: %w", name, err)
	}
	return nil
}

func validateInputSize(name string, data []byte, maximum int) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("%s input is empty", name)
	}
	if len(data) > maximum {
		return fmt.Errorf("%s input exceeds %d bytes", name, maximum)
	}
	return nil
}

func (service *Service) validateFrozenInputClosure(
	ctx context.Context,
	scope Scope,
	plan contractsv1alpha1.AgentReviewPlan,
	set contractsv1alpha1.ReviewHypothesisSet,
) error {
	if plan.ExecutionSnapshotRef.Contract != runmodel.SnapshotSchemaVersion {
		return fmt.Errorf(
			"execution snapshot contract is %q, want %q",
			plan.ExecutionSnapshotRef.Contract,
			runmodel.SnapshotSchemaVersion,
		)
	}
	if plan.ReviewInputRef.Contract != runmodel.ContractReviewInput {
		return fmt.Errorf(
			"review input contract is %q, want %q",
			plan.ReviewInputRef.Contract,
			runmodel.ContractReviewInput,
		)
	}
	sourceRun, err := service.repository.runs.LoadRun(plan.SourceRunID)
	if err != nil {
		return fmt.Errorf("load committed source run %q: %w", plan.SourceRunID, err)
	}
	snapshotData, err := service.repository.resolveFrozenInput(plan.ExecutionSnapshotRef)
	if err != nil {
		return fmt.Errorf("resolve execution snapshot: %w", err)
	}
	snapshot, err := runmodel.DecodeExecutionSnapshot(snapshotData)
	if err != nil {
		return err
	}
	committedSnapshot, err := service.repository.runs.LoadExecutionSnapshot(
		sourceRun.ExecutionSnapshotID,
	)
	if err != nil {
		return fmt.Errorf("load committed execution snapshot: %w", err)
	}
	if snapshot.ExecutionSnapshotID != sourceRun.ExecutionSnapshotID {
		return fmt.Errorf(
			"plan execution snapshot_id does not match the committed source run",
		)
	}
	if snapshot.TargetSnapshotRef != sourceRun.TargetSnapshotRef {
		return fmt.Errorf(
			"plan execution snapshot target_snapshot_ref does not match the committed source run",
		)
	}
	if snapshot.ReviewInputRef != committedSnapshot.ReviewInputRef {
		return fmt.Errorf(
			"plan execution snapshot review_input_ref does not match the committed snapshot",
		)
	}
	if !reflect.DeepEqual(snapshot, committedSnapshot) {
		return fmt.Errorf(
			"plan execution snapshot artifact does not exactly match the committed snapshot",
		)
	}
	if !artifactBindingMatchesRunRef(plan.ReviewInputRef, snapshot.ReviewInputRef) {
		return fmt.Errorf("execution snapshot does not bind the exact plan review input")
	}
	reviewInputData, err := service.repository.resolveFrozenInput(plan.ReviewInputRef)
	if err != nil {
		return fmt.Errorf("resolve review input: %w", err)
	}
	input, err := reviewcore.DecodeReviewInput(reviewInputData)
	if err != nil {
		return err
	}
	targetDigest, err := reviewcore.DigestReviewInput(input)
	if err != nil {
		return fmt.Errorf("digest review input: %w", err)
	}
	if targetDigest != plan.TargetDigest {
		return fmt.Errorf("review input digest does not match plan target_digest")
	}
	specData, err := service.repository.resolveFrozenInput(
		bindingFromRunRef(snapshot.ReviewSpecRef),
	)
	if err != nil {
		return fmt.Errorf("resolve review spec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specData)
	if err != nil {
		return fmt.Errorf("decode review spec: %w", err)
	}
	if spec.TenantID != scope.TenantID || spec.WorkspaceID != scope.WorkspaceID {
		return artifactrepo.ErrUnauthorized
	}
	if spec.RequestID != plan.SourceRunID {
		return fmt.Errorf("source_run_id does not match the frozen ReviewSpec request")
	}
	if err := validateHypothesisEvidence(ctx, input, set); err != nil {
		return err
	}
	return nil
}

func validateHypothesisEvidence(
	ctx context.Context,
	input reviewcore.ReviewInput,
	set contractsv1alpha1.ReviewHypothesisSet,
) error {
	files := make(map[string]reviewcore.FileManifestEntry, len(input.Files))
	for _, file := range input.Files {
		files[file.Path] = file
	}
	for hypothesisIndex, hypothesis := range set.Hypotheses {
		if _, err := validateFrozenAnchor(
			ctx,
			input,
			files,
			hypothesis.Anchor,
			fmt.Sprintf("hypotheses[%d].anchor", hypothesisIndex),
			true,
		); err != nil {
			return err
		}
		for evidenceIndex, evidence := range hypothesis.Evidence {
			content, err := validateFrozenAnchor(
				ctx,
				input,
				files,
				evidence.Anchor,
				fmt.Sprintf(
					"hypotheses[%d].evidence[%d].anchor",
					hypothesisIndex,
					evidenceIndex,
				),
				false,
			)
			if err != nil {
				return err
			}
			if evidence.Anchor.EndLine-evidence.Anchor.StartLine+1 > 20 {
				return fmt.Errorf(
					"hypotheses[%d].evidence[%d] range exceeds 20 lines",
					hypothesisIndex,
					evidenceIndex,
				)
			}
			excerpt, ok := exactFrozenExcerpt(
				content,
				evidence.Anchor.StartLine,
				evidence.Anchor.EndLine,
			)
			if !ok || evidence.Excerpt != excerpt {
				return fmt.Errorf(
					"hypotheses[%d].evidence[%d] excerpt does not equal the frozen source range",
					hypothesisIndex,
					evidenceIndex,
				)
			}
		}
	}
	return nil
}

func validateFrozenAnchor(
	ctx context.Context,
	input reviewcore.ReviewInput,
	files map[string]reviewcore.FileManifestEntry,
	anchor contractsv1alpha1.HypothesisSourceAnchor,
	name string,
	requireTargetAuthorization bool,
) (string, error) {
	if anchor.Side == contractsv1alpha1.HypothesisAnchorOld {
		return "", fmt.Errorf("%s old-side source is not trusted by the M1.3 host", name)
	}
	file, exists := files[anchor.Path]
	if !exists || file.Content == nil {
		if !requireTargetAuthorization && contextRefAuthorizesAnchor(input, anchor) {
			return "", fmt.Errorf(
				"%s is bound only by ContextRef; M1.3 local shadow import cannot validate referenced context content",
				name,
			)
		}
		return "", fmt.Errorf("%s does not bind frozen file content", name)
	}
	if file.SHA256 != anchor.SourceDigest {
		return "", fmt.Errorf("%s source_digest does not match the frozen file", name)
	}
	if requireTargetAuthorization && !reviewcore.InputAuthorizesAnchor(
		ctx,
		input,
		anchor.Path,
		anchor.StartLine,
		anchor.EndLine,
	) {
		return "", fmt.Errorf("%s is outside the authorized frozen target", name)
	}
	if _, ok := exactFrozenExcerpt(*file.Content, anchor.StartLine, anchor.EndLine); !ok {
		return "", fmt.Errorf("%s extends beyond frozen file content", name)
	}
	return *file.Content, nil
}

func contextRefAuthorizesAnchor(
	input reviewcore.ReviewInput,
	anchor contractsv1alpha1.HypothesisSourceAnchor,
) bool {
	for _, binding := range input.Contexts {
		if binding.Ref == nil {
			continue
		}
		for _, span := range binding.Ref.Coverage.Spans {
			if span.Path == anchor.Path && anchor.StartLine >= span.StartLine &&
				anchor.EndLine <= span.EndLine {
				return true
			}
		}
	}
	return false
}

func exactFrozenExcerpt(content string, startLine, endLine uint32) (string, bool) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(normalized, "\n")
	if strings.HasSuffix(normalized, "\n") {
		lines = lines[:len(lines)-1]
	}
	if startLine == 0 || endLine < startLine || uint64(endLine) > uint64(len(lines)) {
		return "", false
	}
	excerpt := strings.TrimSpace(strings.Join(lines[startLine-1:endLine], "\n"))
	if excerpt == "" {
		return "", false
	}
	return excerpt, true
}

func artifactBindingMatchesRunRef(
	binding contractsv1alpha1.ArtifactBinding,
	ref runmodel.ArtifactRef,
) bool {
	return binding.Contract == ref.Contract && binding.Ref.URI == ref.URI &&
		binding.Ref.SHA256 == ref.SHA256 && binding.Ref.SizeBytes == ref.SizeBytes
}

func bindingFromRunRef(ref runmodel.ArtifactRef) contractsv1alpha1.ArtifactBinding {
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
		},
		Contract: ref.Contract,
	}
}
