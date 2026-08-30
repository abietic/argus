package runmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	AgentStageDispatchIntentSchemaVersion   = "argus.agent_stage_dispatch_intent.v1alpha1"
	AgentStageExecutionBindingSchemaVersion = "argus.agent_stage_execution_binding.v1alpha1"

	// ContractStageExecutionRequest is the contract of the complete canonical
	// StageExecutionRequest JSON bytes stored in RequestRef. RequestRef.SHA256
	// hashes those complete bytes; RequestSemanticSHA256 is the request's
	// embedded semantic digest. The two digest classes are not interchangeable.
	ContractStageExecutionRequest = "argus.stage_execution_request.v1alpha1"
	maxAgentJSONSafeInteger       = uint64(1<<53 - 1)
)

// AgentStageDispatchIntent is the durable write-ahead authority for one
// formal agent-stage create operation. Its deterministic ID admits exactly one
// payload for a run+stage+attempt+generation coordinate. A provider create
// must never happen before this fact is committed.
type AgentStageDispatchIntent struct {
	SchemaVersion string `json:"schema_version"`
	IntentID      string `json:"intent_id"`
	SHA256        string `json:"sha256"`

	Subject     AgentPlanningSubject `json:"subject"`
	ReviewRunID string               `json:"review_run_id"`
	Stage       AgentStageRef        `json:"stage"`
	WorkloadID  string               `json:"workload_id"`
	LeaseID     string               `json:"lease_id"`
	LeaseWorker string               `json:"lease_worker_id"`

	AdmissionID        string `json:"agent_stage_plan_admission_id"`
	AdmissionSHA256    string `json:"agent_stage_plan_admission_sha256"`
	PlanID             string `json:"plan_id"`
	PlanSemanticSHA256 string `json:"plan_semantic_sha256"`

	RequestRef            ArtifactRef `json:"stage_execution_request_ref"`
	RequestSemanticSHA256 string      `json:"request_semantic_sha256"`
	CapabilitySHA256      string      `json:"capability_sha256"`

	ExecutionID          string `json:"execution_id"`
	Attempt              int    `json:"attempt"`
	Generation           int    `json:"generation"`
	FencingToken         uint64 `json:"fencing_token"`
	CreateIdempotencyKey string `json:"create_idempotency_key"`
	CancelIdempotencyKey string `json:"cancel_idempotency_key"`

	RecordedAt time.Time `json:"recorded_at"`
}

// AgentStageExecutionBinding records the exact provider acknowledgement for a
// committed dispatch intent. ProviderHandle is opaque provider identity; it
// cannot replace the echoed execution tuple or request/capability digests.
type AgentStageExecutionBinding struct {
	SchemaVersion string `json:"schema_version"`
	BindingID     string `json:"binding_id"`
	SHA256        string `json:"sha256"`

	Subject     AgentPlanningSubject `json:"subject"`
	ReviewRunID string               `json:"review_run_id"`
	Stage       AgentStageRef        `json:"stage"`
	WorkloadID  string               `json:"workload_id"`
	LeaseID     string               `json:"lease_id"`
	LeaseWorker string               `json:"lease_worker_id"`

	IntentID              string      `json:"dispatch_intent_id"`
	IntentSHA256          string      `json:"dispatch_intent_sha256"`
	RequestRef            ArtifactRef `json:"stage_execution_request_ref"`
	RequestSemanticSHA256 string      `json:"request_semantic_sha256"`

	ExecutionID          string `json:"execution_id"`
	Attempt              int    `json:"attempt"`
	Generation           int    `json:"generation"`
	FencingToken         uint64 `json:"fencing_token"`
	CreateIdempotencyKey string `json:"create_idempotency_key"`

	ProviderHandle   string `json:"provider_handle"`
	CapabilitySHA256 string `json:"capability_sha256"`

	RecordedAt time.Time `json:"recorded_at"`
}

// SameAgentStageExecutionDecision reports whether two sealed bindings record
// the same provider acknowledgement. BindingID, SHA256, and RecordedAt are
// host-derived persistence metadata and may differ when concurrent observers
// race; every authority and provider identity field, including the opaque
// ProviderHandle, is part of the provider decision and must remain exact.
func SameAgentStageExecutionDecision(
	left AgentStageExecutionBinding,
	right AgentStageExecutionBinding,
) bool {
	left.BindingID = ""
	left.SHA256 = ""
	left.RecordedAt = time.Time{}
	right.BindingID = ""
	right.SHA256 = ""
	right.RecordedAt = time.Time{}
	return left == right
}

// SealAgentStageDispatchIntent derives the immutable authority ID and record
// digest. RecordedAt must already be an observed non-zero UTC timestamp.
func SealAgentStageDispatchIntent(
	intent AgentStageDispatchIntent,
) (AgentStageDispatchIntent, error) {
	if intent.SchemaVersion != "" &&
		intent.SchemaVersion != AgentStageDispatchIntentSchemaVersion {
		return AgentStageDispatchIntent{}, fmt.Errorf(
			"unsupported AgentStageDispatchIntent schema %q",
			intent.SchemaVersion,
		)
	}
	intent.SchemaVersion = AgentStageDispatchIntentSchemaVersion
	intent.IntentID = ""
	intent.SHA256 = ""
	if err := intent.validateContent(); err != nil {
		return AgentStageDispatchIntent{}, err
	}
	intentID, err := AgentStageDispatchIntentID(
		intent.ReviewRunID,
		intent.Stage.ID,
		intent.Attempt,
		intent.Generation,
	)
	if err != nil {
		return AgentStageDispatchIntent{}, err
	}
	intent.IntentID = intentID
	digest, err := DigestAgentStageDispatchIntent(intent)
	if err != nil {
		return AgentStageDispatchIntent{}, err
	}
	intent.SHA256 = digest
	if err := intent.Validate(); err != nil {
		return AgentStageDispatchIntent{}, fmt.Errorf(
			"validate sealed AgentStageDispatchIntent: %w",
			err,
		)
	}
	return intent, nil
}

// AgentStageDispatchIntentID returns the sole create authority key for a
// logical stage attempt generation. Execution ID, fence, request, or key drift
// therefore conflicts instead of opening a second create authority.
func AgentStageDispatchIntentID(
	runID string,
	stageID string,
	attempt int,
	generation int,
) (string, error) {
	if err := validateAgentIdentity("review_run_id", runID); err != nil {
		return "", err
	}
	if err := validateAgentStageIdentifier("stage.id", stageID); err != nil {
		return "", err
	}
	if attempt < 1 || generation < 1 ||
		uint64(attempt) > maxAgentJSONSafeInteger ||
		uint64(generation) > maxAgentJSONSafeInteger {
		return "", fmt.Errorf("attempt and generation must be JSON-safe positive integers")
	}
	payload, err := json.Marshal(struct {
		ReviewRunID string `json:"review_run_id"`
		StageID     string `json:"stage_id"`
		Attempt     int    `json:"attempt"`
		Generation  int    `json:"generation"`
	}{
		ReviewRunID: runID,
		StageID:     stageID,
		Attempt:     attempt,
		Generation:  generation,
	})
	if err != nil {
		return "", fmt.Errorf("marshal agent-stage dispatch identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return runID + "-agent-stage-dispatch-" + hex.EncodeToString(digest[:12]), nil
}

func DigestAgentStageDispatchIntent(
	intent AgentStageDispatchIntent,
) (string, error) {
	copy := intent
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStageDispatchIntent: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (intent AgentStageDispatchIntent) Validate() error {
	if err := intent.validateContent(); err != nil {
		return err
	}
	wantID, err := AgentStageDispatchIntentID(
		intent.ReviewRunID,
		intent.Stage.ID,
		intent.Attempt,
		intent.Generation,
	)
	if err != nil {
		return err
	}
	if intent.IntentID != wantID {
		return fmt.Errorf(
			"intent_id must be %q for the exact run, stage, attempt, and generation",
			wantID,
		)
	}
	if err := requireSHA256("sha256", intent.SHA256); err != nil {
		return err
	}
	digest, err := DigestAgentStageDispatchIntent(intent)
	if err != nil {
		return err
	}
	if intent.SHA256 != digest {
		return fmt.Errorf("sha256 does not match AgentStageDispatchIntent content")
	}
	return nil
}

func (intent AgentStageDispatchIntent) validateContent() error {
	if intent.SchemaVersion != AgentStageDispatchIntentSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentStageDispatchIntent schema %q",
			intent.SchemaVersion,
		)
	}
	if err := intent.Subject.Validate(); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	if err := validateAgentIdentity("review_run_id", intent.ReviewRunID); err != nil {
		return err
	}
	if err := intent.Stage.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"workload_id":     intent.WorkloadID,
		"lease_id":        intent.LeaseID,
		"lease_worker_id": intent.LeaseWorker,
	} {
		if err := validateAgentIdentity(name, value); err != nil {
			return err
		}
	}
	wantAdmissionID, err := AgentStagePlanAdmissionID(
		intent.ReviewRunID,
		intent.Stage.ID,
	)
	if err != nil {
		return err
	}
	if intent.AdmissionID != wantAdmissionID {
		return fmt.Errorf(
			"agent_stage_plan_admission_id must be %q for the exact run and stage",
			wantAdmissionID,
		)
	}
	if err := requireSHA256(
		"agent_stage_plan_admission_sha256",
		intent.AdmissionSHA256,
	); err != nil {
		return err
	}
	if err := requireSHA256("plan_semantic_sha256", intent.PlanSemanticSHA256); err != nil {
		return err
	}
	if intent.PlanID != "agent-stage-plan-"+intent.PlanSemanticSHA256[:24] {
		return fmt.Errorf("plan_id does not match plan_semantic_sha256")
	}
	if err := intent.RequestRef.Validate(); err != nil {
		return fmt.Errorf("stage_execution_request_ref: %w", err)
	}
	if intent.RequestRef.SizeBytes <= 0 {
		return fmt.Errorf("stage_execution_request_ref must be non-empty")
	}
	if intent.RequestRef.Contract != ContractStageExecutionRequest {
		return fmt.Errorf(
			"stage_execution_request_ref contract must be %q",
			ContractStageExecutionRequest,
		)
	}
	if err := requireSHA256(
		"request_semantic_sha256",
		intent.RequestSemanticSHA256,
	); err != nil {
		return err
	}
	if err := requireSHA256("capability_sha256", intent.CapabilitySHA256); err != nil {
		return err
	}
	if err := validateAgentIdentity("execution_id", intent.ExecutionID); err != nil {
		return err
	}
	if intent.Attempt < 1 || intent.Generation < 1 ||
		uint64(intent.Attempt) > maxAgentJSONSafeInteger ||
		uint64(intent.Generation) > maxAgentJSONSafeInteger ||
		intent.FencingToken == 0 || intent.FencingToken > maxAgentJSONSafeInteger {
		return fmt.Errorf(
			"attempt, generation, and fencing_token must be JSON-safe positive integers",
		)
	}
	if err := validateAgentIdentity(
		"create_idempotency_key",
		intent.CreateIdempotencyKey,
	); err != nil {
		return err
	}
	if err := validateAgentIdentity(
		"cancel_idempotency_key",
		intent.CancelIdempotencyKey,
	); err != nil {
		return err
	}
	if intent.CancelIdempotencyKey == intent.CreateIdempotencyKey {
		return fmt.Errorf(
			"cancel_idempotency_key must be independent from create_idempotency_key",
		)
	}
	if intent.RecordedAt.IsZero() || intent.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("recorded_at must be a non-zero UTC timestamp")
	}
	return nil
}

// ValidateAgainstAdmission closes the dispatch intent over the sole admitted
// plan for its run+stage. It intentionally compares the admission self digest
// and plan semantic digest, not either artifact byte digest.
func (intent AgentStageDispatchIntent) ValidateAgainstAdmission(
	admission AgentStagePlanAdmission,
) error {
	if err := intent.Validate(); err != nil {
		return fmt.Errorf("validate dispatch intent: %w", err)
	}
	if err := admission.Validate(); err != nil {
		return fmt.Errorf("validate agent-stage plan admission: %w", err)
	}
	if intent.Subject != admission.Subject ||
		intent.ReviewRunID != admission.ReviewRunID ||
		intent.Stage != admission.Stage ||
		intent.AdmissionID != admission.AdmissionID ||
		intent.AdmissionSHA256 != admission.SHA256 ||
		intent.PlanID != admission.PlanID ||
		intent.PlanSemanticSHA256 != admission.PlanSemanticSHA256 {
		return fmt.Errorf("dispatch intent does not match exact agent-stage plan admission")
	}
	if intent.RecordedAt.Before(admission.RecordedAt) {
		return fmt.Errorf("dispatch intent recorded_at precedes its plan admission")
	}
	return nil
}

func SealAgentStageExecutionBinding(
	binding AgentStageExecutionBinding,
) (AgentStageExecutionBinding, error) {
	if binding.SchemaVersion != "" &&
		binding.SchemaVersion != AgentStageExecutionBindingSchemaVersion {
		return AgentStageExecutionBinding{}, fmt.Errorf(
			"unsupported AgentStageExecutionBinding schema %q",
			binding.SchemaVersion,
		)
	}
	binding.SchemaVersion = AgentStageExecutionBindingSchemaVersion
	binding.BindingID = ""
	binding.SHA256 = ""
	if err := binding.validateContent(); err != nil {
		return AgentStageExecutionBinding{}, err
	}
	bindingID, err := AgentStageExecutionBindingID(
		binding.ReviewRunID,
		binding.IntentID,
	)
	if err != nil {
		return AgentStageExecutionBinding{}, err
	}
	binding.BindingID = bindingID
	digest, err := DigestAgentStageExecutionBinding(binding)
	if err != nil {
		return AgentStageExecutionBinding{}, err
	}
	binding.SHA256 = digest
	if err := binding.Validate(); err != nil {
		return AgentStageExecutionBinding{}, fmt.Errorf(
			"validate sealed AgentStageExecutionBinding: %w",
			err,
		)
	}
	return binding, nil
}

// AgentStageExecutionBindingID returns the sole provider acknowledgement key
// for one dispatch intent.
func AgentStageExecutionBindingID(runID, intentID string) (string, error) {
	if err := validateAgentIdentity("review_run_id", runID); err != nil {
		return "", err
	}
	if err := validateAgentStageIdentifier("dispatch_intent_id", intentID); err != nil {
		return "", err
	}
	if !strings.HasPrefix(intentID, runID+"-agent-stage-dispatch-") {
		return "", fmt.Errorf("dispatch_intent_id is not namespaced by review_run_id")
	}
	payload, err := json.Marshal(struct {
		ReviewRunID string `json:"review_run_id"`
		IntentID    string `json:"dispatch_intent_id"`
	}{ReviewRunID: runID, IntentID: intentID})
	if err != nil {
		return "", fmt.Errorf("marshal agent-stage execution binding identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return runID + "-agent-stage-execution-binding-" +
		hex.EncodeToString(digest[:12]), nil
}

func DigestAgentStageExecutionBinding(
	binding AgentStageExecutionBinding,
) (string, error) {
	copy := binding
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStageExecutionBinding: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (binding AgentStageExecutionBinding) Validate() error {
	if err := binding.validateContent(); err != nil {
		return err
	}
	wantID, err := AgentStageExecutionBindingID(
		binding.ReviewRunID,
		binding.IntentID,
	)
	if err != nil {
		return err
	}
	if binding.BindingID != wantID {
		return fmt.Errorf(
			"binding_id must be %q for the exact dispatch intent",
			wantID,
		)
	}
	if err := requireSHA256("sha256", binding.SHA256); err != nil {
		return err
	}
	digest, err := DigestAgentStageExecutionBinding(binding)
	if err != nil {
		return err
	}
	if binding.SHA256 != digest {
		return fmt.Errorf("sha256 does not match AgentStageExecutionBinding content")
	}
	return nil
}

func (binding AgentStageExecutionBinding) validateContent() error {
	if binding.SchemaVersion != AgentStageExecutionBindingSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentStageExecutionBinding schema %q",
			binding.SchemaVersion,
		)
	}
	if err := binding.Subject.Validate(); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	if err := validateAgentIdentity("review_run_id", binding.ReviewRunID); err != nil {
		return err
	}
	if err := binding.Stage.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"workload_id":     binding.WorkloadID,
		"lease_id":        binding.LeaseID,
		"lease_worker_id": binding.LeaseWorker,
	} {
		if err := validateAgentIdentity(name, value); err != nil {
			return err
		}
	}
	if _, err := AgentStageExecutionBindingID(
		binding.ReviewRunID,
		binding.IntentID,
	); err != nil {
		return err
	}
	if err := requireSHA256("dispatch_intent_sha256", binding.IntentSHA256); err != nil {
		return err
	}
	if err := binding.RequestRef.Validate(); err != nil {
		return fmt.Errorf("stage_execution_request_ref: %w", err)
	}
	if binding.RequestRef.SizeBytes <= 0 ||
		binding.RequestRef.Contract != ContractStageExecutionRequest {
		return fmt.Errorf(
			"stage_execution_request_ref must be a non-empty %q artifact",
			ContractStageExecutionRequest,
		)
	}
	if err := requireSHA256(
		"request_semantic_sha256",
		binding.RequestSemanticSHA256,
	); err != nil {
		return err
	}
	if err := validateAgentIdentity("execution_id", binding.ExecutionID); err != nil {
		return err
	}
	if binding.Attempt < 1 || binding.Generation < 1 ||
		uint64(binding.Attempt) > maxAgentJSONSafeInteger ||
		uint64(binding.Generation) > maxAgentJSONSafeInteger ||
		binding.FencingToken == 0 || binding.FencingToken > maxAgentJSONSafeInteger {
		return fmt.Errorf(
			"attempt, generation, and fencing_token must be JSON-safe positive integers",
		)
	}
	if err := validateAgentIdentity(
		"create_idempotency_key",
		binding.CreateIdempotencyKey,
	); err != nil {
		return err
	}
	if err := validateAgentProviderHandle(binding.ProviderHandle); err != nil {
		return err
	}
	if err := requireSHA256("capability_sha256", binding.CapabilitySHA256); err != nil {
		return err
	}
	if binding.RecordedAt.IsZero() || binding.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("recorded_at must be a non-zero UTC timestamp")
	}
	return nil
}

// ValidateAgainstIntent proves that a provider acknowledgement is for the
// exact committed request generation. The opaque provider handle is additive;
// every request identity field must still be echoed exactly.
func (binding AgentStageExecutionBinding) ValidateAgainstIntent(
	intent AgentStageDispatchIntent,
) error {
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("validate execution binding: %w", err)
	}
	if err := intent.Validate(); err != nil {
		return fmt.Errorf("validate dispatch intent: %w", err)
	}
	if binding.Subject != intent.Subject ||
		binding.ReviewRunID != intent.ReviewRunID ||
		binding.Stage != intent.Stage ||
		binding.WorkloadID != intent.WorkloadID ||
		binding.LeaseID != intent.LeaseID ||
		binding.LeaseWorker != intent.LeaseWorker ||
		binding.IntentID != intent.IntentID ||
		binding.IntentSHA256 != intent.SHA256 ||
		binding.RequestRef != intent.RequestRef ||
		binding.RequestSemanticSHA256 != intent.RequestSemanticSHA256 ||
		binding.ExecutionID != intent.ExecutionID ||
		binding.Attempt != intent.Attempt ||
		binding.Generation != intent.Generation ||
		binding.FencingToken != intent.FencingToken ||
		binding.CreateIdempotencyKey != intent.CreateIdempotencyKey ||
		binding.CapabilitySHA256 != intent.CapabilitySHA256 {
		return fmt.Errorf("execution binding does not match exact dispatch intent")
	}
	if binding.RecordedAt.Before(intent.RecordedAt) {
		return fmt.Errorf("execution binding recorded_at precedes its dispatch intent")
	}
	return nil
}

func validateAgentProviderHandle(value string) error {
	if value == "" || len(value) > 512 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("provider_handle must be a non-empty bounded opaque identifier")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("provider_handle must be a non-empty bounded opaque identifier")
		}
	}
	return nil
}
