package agentshadow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	executionImportAuthorizationSchemaVersion = "argus.agent_review_execution_import_authorization.v1alpha1"
	executionImportAuthorizationRoot          = "agent-review-shadow/execution-import-authorizations/"
	maxExecutionImportAuthorizationBytes      = 12 << 10
)

type executionImportPayloadBinding struct {
	Contract  string `json:"contract"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type executionImportExpectedIdentities struct {
	PlanID          string `json:"plan_id"`
	HypothesisSetID string `json:"hypothesis_set_id"`
	SourceRunID     string `json:"source_run_id"`
	ExecutionID     string `json:"execution_id"`
	ReviewRunID     string `json:"review_run_id"`
}

// executionImportAuthorization is written before Service.Import. It proves
// that LocalBridge already accepted a strictly bound worker result and mapped
// its exact canonical evidence payload. It may validly exist without a
// committed import; only the expected immutable ImportRecord and full Query
// closure can later turn the authorization into a succeeded completion.
type executionImportAuthorization struct {
	SchemaVersion string `json:"schema_version"`
	Scope         Scope  `json:"scope"`

	IdempotencyKey string                  `json:"idempotency_key"`
	SemanticDigest string                  `json:"semantic_digest"`
	IntentSHA256   string                  `json:"intent_sha256"`
	Runtime        ExecutionRuntimeDigests `json:"runtime_digests"`

	Plan                   executionImportPayloadBinding     `json:"plan"`
	HypothesisSet          executionImportPayloadBinding     `json:"hypothesis_set"`
	RawCandidateCollection executionImportPayloadBinding     `json:"raw_candidate_collection"`
	TaskEvidenceCollection executionImportPayloadBinding     `json:"task_evidence_collection"`
	ReceiptCollection      executionImportPayloadBinding     `json:"receipt_collection"`
	ImportInputDigest      string                            `json:"import_input_digest"`
	ExpectedManifestID     string                            `json:"expected_manifest_id"`
	Expected               executionImportExpectedIdentities `json:"expected_identities"`
}

// authorizeExecutionImport is intentionally a LocalBridge method and is
// called only after strict worker-result binding and evidence mapping. The
// bytes must already be the canonical JSON that Service.Import will receive.
func (bridge *LocalBridge) authorizeExecutionImport(
	intent executionIntent,
	planData []byte,
	setData []byte,
	rawData []byte,
	taskEvidenceData []byte,
	receiptData []byte,
) (executionImportAuthorization, error) {
	if bridge == nil || bridge.repository == nil {
		return executionImportAuthorization{}, fmt.Errorf("initialized local agent review bridge is required")
	}
	if err := validateExecutionIntent(intent); err != nil {
		return executionImportAuthorization{}, err
	}
	if err := validateInputSize("plan", planData, maxPlanBytes); err != nil {
		return executionImportAuthorization{}, err
	}
	if err := validateInputSize("hypothesis set", setData, maxHypothesisBytes); err != nil {
		return executionImportAuthorization{}, err
	}
	if err := validateInputSize("raw candidate collection", rawData, maxRawCandidateBytes); err != nil {
		return executionImportAuthorization{}, err
	}
	if err := validateInputSize("receipt collection", receiptData, maxReceiptBytes); err != nil {
		return executionImportAuthorization{}, err
	}
	if err := validateInputSize("task evidence collection", taskEvidenceData, maxTaskEvidenceBytes); err != nil {
		return executionImportAuthorization{}, err
	}
	plan, err := contractsv1alpha1.DecodeAgentReviewPlan(planData)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	set, err := contractsv1alpha1.DecodeReviewHypothesisSet(setData)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawData)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	taskEvidence, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(taskEvidenceData)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	receipts, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(receiptData)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	canonicalPlan, err := json.Marshal(plan)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	canonicalSet, err := json.Marshal(set)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	canonicalRaw, err := json.Marshal(raw)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	canonicalTaskEvidence, err := json.Marshal(taskEvidence)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	canonicalReceipts, err := json.Marshal(receipts)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	if !bytes.Equal(planData, canonicalPlan) || !bytes.Equal(setData, canonicalSet) ||
		!bytes.Equal(rawData, canonicalRaw) ||
		!bytes.Equal(taskEvidenceData, canonicalTaskEvidence) ||
		!bytes.Equal(receiptData, canonicalReceipts) {
		return executionImportAuthorization{}, fmt.Errorf("execution import authorization requires canonical JSON payloads")
	}
	if !reflect.DeepEqual(plan, intent.Plan) {
		return executionImportAuthorization{}, fmt.Errorf("authorized plan does not bind the exact execution intent")
	}
	if set.PlanID != plan.PlanID || set.SourceRunID != plan.SourceRunID ||
		set.ExecutionID != plan.ExecutionID || set.ReviewRunID != plan.ReviewRunID ||
		raw.PlanID != plan.PlanID || raw.SourceRunID != plan.SourceRunID ||
		raw.ExecutionID != plan.ExecutionID || raw.ReviewRunID != plan.ReviewRunID ||
		taskEvidence.PlanID != plan.PlanID || taskEvidence.SourceRunID != plan.SourceRunID ||
		taskEvidence.ExecutionID != plan.ExecutionID || taskEvidence.ReviewRunID != plan.ReviewRunID ||
		receipts.PlanID != plan.PlanID || receipts.SourceRunID != plan.SourceRunID ||
		receipts.ExecutionID != plan.ExecutionID || receipts.ReviewRunID != plan.ReviewRunID {
		return executionImportAuthorization{}, fmt.Errorf("authorized evidence identities do not bind the exact plan")
	}
	if set.GeneratedAt.Before(intent.AcceptedAt) ||
		set.GeneratedAt.After(intent.AcceptedAt.Add(
			time.Duration(intent.Plan.Budget.TimeoutMS)*time.Millisecond,
		)) {
		return executionImportAuthorization{}, fmt.Errorf("authorized worker completion is outside the execution deadline")
	}
	inputDigest := digestByteSlices(canonicalPlan, canonicalSet, canonicalRaw, canonicalTaskEvidence, canonicalReceipts)
	authorizationIntent := importIntent{
		SchemaVersion: importIntentSchemaVersion,
		TenantID:      intent.TenantID, WorkspaceID: intent.WorkspaceID,
		IdempotencyKey: intent.IdempotencyKey,
		InputDigest:    inputDigest,
		// manifestID deliberately does not depend on AcceptedAt. The synthetic
		// preview uses worker completion as its lower-bound recording time so the
		// normal cross-contract timing checks remain meaningful before Import
		// assigns the real host commit time.
		AcceptedAt: set.GeneratedAt,
	}
	previewManifest, err := buildManifest(
		authorizationIntent,
		plan,
		set,
		raw,
		taskEvidence,
		receipts,
		previewBinding(contractsv1alpha1.AgentReviewPlanSchemaVersion, canonicalPlan),
		previewBinding(contractsv1alpha1.ReviewHypothesisSetSchemaVersion, canonicalSet),
		previewBinding(contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion, canonicalRaw),
		previewBinding(contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion, canonicalTaskEvidence),
		previewBinding(contractsv1alpha1.AgentExecutionReceiptCollectionContract, canonicalReceipts),
	)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	if err := contractsv1alpha1.ValidateAgentReviewResultManifestBindings(
		previewManifest,
		plan,
		set,
		receipts,
		raw,
	); err != nil {
		return executionImportAuthorization{}, fmt.Errorf("validate authorized shadow result bindings: %w", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(taskEvidence, plan, receipts); err != nil {
		return executionImportAuthorization{}, fmt.Errorf("validate authorized task evidence bindings: %w", err)
	}
	intentSHA256, err := canonicalObjectSHA256(intent)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	authorization := executionImportAuthorization{
		SchemaVersion:  executionImportAuthorizationSchemaVersion,
		Scope:          Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID},
		IdempotencyKey: intent.IdempotencyKey,
		SemanticDigest: intent.SemanticDigest,
		IntentSHA256:   intentSHA256,
		Runtime: ExecutionRuntimeDigests{
			WorkerRequestSHA256: intent.WorkerRequestSHA256,
			NodeSHA256:          intent.NodeSHA256,
			WorkerScriptSHA256:  intent.WorkerScriptSHA256,
			WorkerPackageSHA256: intent.WorkerPackageSHA256,
		},
		Plan:                   executionImportPayloadBindingFromBytes(contractsv1alpha1.AgentReviewPlanSchemaVersion, canonicalPlan),
		HypothesisSet:          executionImportPayloadBindingFromBytes(contractsv1alpha1.ReviewHypothesisSetSchemaVersion, canonicalSet),
		RawCandidateCollection: executionImportPayloadBindingFromBytes(contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion, canonicalRaw),
		TaskEvidenceCollection: executionImportPayloadBindingFromBytes(contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion, canonicalTaskEvidence),
		ReceiptCollection:      executionImportPayloadBindingFromBytes(contractsv1alpha1.AgentExecutionReceiptCollectionContract, canonicalReceipts),
		ImportInputDigest:      inputDigest,
		ExpectedManifestID:     manifestID(authorizationIntent),
		Expected: executionImportExpectedIdentities{
			PlanID: plan.PlanID, HypothesisSetID: set.HypothesisSetID,
			SourceRunID: plan.SourceRunID, ExecutionID: plan.ExecutionID,
			ReviewRunID: plan.ReviewRunID,
		},
	}
	if err := validateExecutionImportAuthorization(intent, authorization); err != nil {
		return executionImportAuthorization{}, err
	}
	objectID := executionImportAuthorizationObjectID(authorization.Scope, authorization.IdempotencyKey)
	if err := bridge.repository.store.PutJSON(objectID, authorization); err == nil {
		return authorization, nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return executionImportAuthorization{}, fmt.Errorf("persist execution import authorization: %w", err)
	}
	existing, exists, err := bridge.repository.loadExecutionImportAuthorization(intent)
	if err != nil {
		return executionImportAuthorization{}, err
	}
	if !exists {
		return executionImportAuthorization{}, fmt.Errorf("%w: immutable execution import authorization disappeared", ErrCorrupt)
	}
	if !reflect.DeepEqual(existing, authorization) {
		return executionImportAuthorization{}, fmt.Errorf(
			"%w: worker execution is already authorized for another shadow import",
			ErrConflict,
		)
	}
	return existing, nil
}

func executionImportPayloadBindingFromBytes(
	contract string,
	data []byte,
) executionImportPayloadBinding {
	preview := previewBinding(contract, data)
	return executionImportPayloadBinding{
		Contract:  preview.Contract,
		SHA256:    preview.Ref.SHA256,
		SizeBytes: preview.Ref.SizeBytes,
	}
}

func (repository *Repository) loadExecutionImportAuthorization(
	intent executionIntent,
) (executionImportAuthorization, bool, error) {
	objectID := executionImportAuthorizationObjectID(
		Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID},
		intent.IdempotencyKey,
	)
	var authorization executionImportAuthorization
	if err := repository.store.GetJSON(objectID, &authorization); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return executionImportAuthorization{}, false, nil
		}
		return executionImportAuthorization{}, false, fmt.Errorf(
			"%w: load execution import authorization: %v",
			ErrCorrupt,
			err,
		)
	}
	if err := validateExecutionImportAuthorization(intent, authorization); err != nil {
		return executionImportAuthorization{}, false, fmt.Errorf(
			"%w: validate execution import authorization: %v",
			ErrCorrupt,
			err,
		)
	}
	return authorization, true, nil
}

func (repository *Repository) validateExecutionImportAuthorizationDirectory(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	directory := filepath.Join(
		repository.store.Root(),
		"immutable",
		filepath.FromSlash(strings.TrimSuffix(executionImportAuthorizationRoot, "/")),
	)
	entries, err := readStrictObjectDirectory(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: enumerate execution import authorizations: %v", ErrCorrupt, err)
	}
	if len(entries) > maxExecutionRecords {
		return fmt.Errorf("execution import authorizations exceed %d records", maxExecutionRecords)
	}
	for _, name := range entries {
		if err := contextError(ctx); err != nil {
			return err
		}
		objectID := executionImportAuthorizationRoot + strings.TrimSuffix(name, ".json")
		var authorization executionImportAuthorization
		if err := repository.store.GetJSON(objectID, &authorization); err != nil {
			return fmt.Errorf("%w: load execution import authorization: %v", ErrCorrupt, err)
		}
		if executionImportAuthorizationObjectID(
			authorization.Scope,
			authorization.IdempotencyKey,
		) != objectID {
			return fmt.Errorf(
				"%w: execution import authorization path does not bind its scope and idempotency key",
				ErrCorrupt,
			)
		}
		var intent executionIntent
		if err := repository.store.GetJSON(
			executionIntentObjectID(authorization.Scope, authorization.IdempotencyKey),
			&intent,
		); err != nil {
			return fmt.Errorf("%w: execution import authorization has no valid intent: %v", ErrCorrupt, err)
		}
		if err := validateExecutionIntent(intent); err != nil {
			return fmt.Errorf("%w: execution import authorization intent: %v", ErrCorrupt, err)
		}
		if err := validateExecutionImportAuthorization(intent, authorization); err != nil {
			return fmt.Errorf("%w: execution import authorization: %v", ErrCorrupt, err)
		}
	}
	return nil
}

func validateExecutionImportAuthorization(
	intent executionIntent,
	authorization executionImportAuthorization,
) error {
	if authorization.SchemaVersion != executionImportAuthorizationSchemaVersion {
		return fmt.Errorf("unsupported execution import authorization schema %q", authorization.SchemaVersion)
	}
	intentScope := Scope{TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID}
	if err := validateScope(authorization.Scope); err != nil {
		return err
	}
	if authorization.Scope != intentScope ||
		authorization.IdempotencyKey != intent.IdempotencyKey ||
		authorization.SemanticDigest != intent.SemanticDigest ||
		authorization.Expected.ExecutionID != intent.Plan.ExecutionID {
		return fmt.Errorf("execution import authorization does not bind its exact intent")
	}
	if err := validateIdempotencyKey(authorization.IdempotencyKey); err != nil {
		return err
	}
	wantRuntime := ExecutionRuntimeDigests{
		WorkerRequestSHA256: intent.WorkerRequestSHA256,
		NodeSHA256:          intent.NodeSHA256,
		WorkerScriptSHA256:  intent.WorkerScriptSHA256,
		WorkerPackageSHA256: intent.WorkerPackageSHA256,
	}
	if authorization.Runtime != wantRuntime {
		return fmt.Errorf("execution import authorization runtime digests changed")
	}
	for name, value := range map[string]string{
		"semantic_digest":                 authorization.SemanticDigest,
		"intent_sha256":                   authorization.IntentSHA256,
		"worker_request_sha256":           authorization.Runtime.WorkerRequestSHA256,
		"node_sha256":                     authorization.Runtime.NodeSHA256,
		"worker_script_sha256":            authorization.Runtime.WorkerScriptSHA256,
		"worker_package_sha256":           authorization.Runtime.WorkerPackageSHA256,
		"plan_sha256":                     authorization.Plan.SHA256,
		"hypothesis_set_sha256":           authorization.HypothesisSet.SHA256,
		"raw_candidate_collection_sha256": authorization.RawCandidateCollection.SHA256,
		"task_evidence_collection_sha256": authorization.TaskEvidenceCollection.SHA256,
		"receipt_collection_sha256":       authorization.ReceiptCollection.SHA256,
		"import_input_digest":             authorization.ImportInputDigest,
	} {
		if !lowerSHA256(value) {
			return fmt.Errorf("%s must be a lowercase SHA-256", name)
		}
	}
	intentSHA256, err := canonicalObjectSHA256(intent)
	if err != nil {
		return err
	}
	if authorization.IntentSHA256 != intentSHA256 {
		return fmt.Errorf("execution import authorization intent digest changed")
	}
	for name, payload := range map[string]struct {
		binding  executionImportPayloadBinding
		contract string
		maximum  int
	}{
		"plan":                     {authorization.Plan, contractsv1alpha1.AgentReviewPlanSchemaVersion, maxPlanBytes},
		"hypothesis_set":           {authorization.HypothesisSet, contractsv1alpha1.ReviewHypothesisSetSchemaVersion, maxHypothesisBytes},
		"raw_candidate_collection": {authorization.RawCandidateCollection, contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion, maxRawCandidateBytes},
		"task_evidence_collection": {authorization.TaskEvidenceCollection, contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion, maxTaskEvidenceBytes},
		"receipt_collection":       {authorization.ReceiptCollection, contractsv1alpha1.AgentExecutionReceiptCollectionContract, maxReceiptBytes},
	} {
		if payload.binding.Contract != payload.contract ||
			!lowerSHA256(payload.binding.SHA256) || payload.binding.SizeBytes <= 0 ||
			payload.binding.SizeBytes > int64(payload.maximum) {
			return fmt.Errorf("execution import authorization %s payload binding is invalid", name)
		}
	}
	for name, value := range map[string]string{
		"expected_manifest_id": authorization.ExpectedManifestID,
		"plan_id":              authorization.Expected.PlanID,
		"hypothesis_set_id":    authorization.Expected.HypothesisSetID,
		"source_run_id":        authorization.Expected.SourceRunID,
		"execution_id":         authorization.Expected.ExecutionID,
		"review_run_id":        authorization.Expected.ReviewRunID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if authorization.Expected.PlanID != intent.Plan.PlanID ||
		authorization.Expected.SourceRunID != intent.Plan.SourceRunID ||
		authorization.Expected.ExecutionID != intent.Plan.ExecutionID ||
		authorization.Expected.ReviewRunID != intent.Plan.ReviewRunID {
		return fmt.Errorf("execution import authorization expected identities changed")
	}
	authorizationIntent := importIntent{
		SchemaVersion: importIntentSchemaVersion,
		TenantID:      intent.TenantID, WorkspaceID: intent.WorkspaceID,
		IdempotencyKey: intent.IdempotencyKey,
		InputDigest:    authorization.ImportInputDigest,
		AcceptedAt:     intent.AcceptedAt,
	}
	if authorization.ExpectedManifestID != manifestID(authorizationIntent) {
		return fmt.Errorf("execution import authorization expected manifest changed")
	}
	data, err := json.Marshal(authorization)
	if err != nil {
		return err
	}
	if len(data) > maxExecutionImportAuthorizationBytes {
		return fmt.Errorf("execution import authorization exceeds %d bytes", maxExecutionImportAuthorizationBytes)
	}
	return nil
}

func validateAuthorizedCommittedResult(
	intent executionIntent,
	authorization executionImportAuthorization,
	result Result,
) error {
	if err := validateExecutionImportAuthorization(intent, authorization); err != nil {
		return err
	}
	if err := validateCommittedResultForExecutionIntent(intent, result); err != nil {
		return err
	}
	if result.Record.InputDigest != authorization.ImportInputDigest ||
		result.Record.ManifestID != authorization.ExpectedManifestID ||
		result.Manifest.ManifestID != authorization.ExpectedManifestID {
		return fmt.Errorf("committed result does not match its authorized input and manifest")
	}
	for name, actual := range map[string]struct {
		binding  contractsv1alpha1.ArtifactBinding
		expected executionImportPayloadBinding
	}{
		"plan":                     {result.Record.AgentReviewPlanRef, authorization.Plan},
		"hypothesis_set":           {result.Record.HypothesisSetRef, authorization.HypothesisSet},
		"raw_candidate_collection": {result.Record.RawCandidateCollectionRef, authorization.RawCandidateCollection},
		"task_evidence_collection": {result.Record.TaskEvidenceCollectionRef, authorization.TaskEvidenceCollection},
		"receipt_collection":       {result.Record.ReceiptCollectionRef, authorization.ReceiptCollection},
	} {
		if actual.binding.Contract != actual.expected.Contract ||
			actual.binding.Ref.SHA256 != actual.expected.SHA256 ||
			actual.binding.Ref.SizeBytes != actual.expected.SizeBytes {
			return fmt.Errorf("committed %s payload changed from its execution authorization", name)
		}
	}
	if result.Plan.PlanID != authorization.Expected.PlanID ||
		result.HypothesisSet.HypothesisSetID != authorization.Expected.HypothesisSetID ||
		result.Plan.SourceRunID != authorization.Expected.SourceRunID ||
		result.Plan.ExecutionID != authorization.Expected.ExecutionID ||
		result.Plan.ReviewRunID != authorization.Expected.ReviewRunID {
		return fmt.Errorf("committed result identities changed from its execution authorization")
	}
	return nil
}

func executionImportAuthorizationObjectID(scope Scope, idempotencyKey string) string {
	return executionImportAuthorizationRoot + digestStrings(
		scope.TenantID,
		scope.WorkspaceID,
		idempotencyKey,
	)
}
