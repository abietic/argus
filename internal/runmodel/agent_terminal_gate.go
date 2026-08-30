package runmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	AgentStageTerminalGateSchemaVersion          = "argus.agent_stage_terminal_gate.v1alpha1"
	AgentStageResultCallbackReceiptSchemaVersion = "argus.agent_stage_result_callback_receipt.v1alpha1"
	ContractStageExecutionResult                 = "argus.stage_execution_result.v1alpha1"
	ContractReviewHypothesisSet                  = "argus.review_hypothesis_set.v1alpha1"
	ContractStageExecutionTraceManifest          = "hailix.trace_manifest.v1alpha1"
	ContractAgentStageResultCallbackReceipt      = "argus.agent_stage_result_callback_receipt.v1alpha1"
)

type AgentStageResultCallbackReceiptKind string

const AgentStageProviderCallbackVerified AgentStageResultCallbackReceiptKind = "provider_callback_verified"

// AgentStageResultCallbackVerifierRef identifies the exact host verifier that
// authenticated a provider callback. It is intentionally project-native so
// runmodel does not depend on a transport contract package.
type AgentStageResultCallbackVerifierRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

// AgentStageResultCallbackReceipt is the durable, proof-redacted fact that an
// exact provider callback was authenticated for an exact execution binding and
// canonical result. Proof bytes are never persisted; only ProofSHA256 closes
// the verifier decision over them. The verifier contract forbids reusable
// bearer secrets and low-entropy codes as proof: the bytes must be a public
// signed/attested statement, a high-entropy non-secret nonce, or a local
// immutable-ledger decision locator. Otherwise even this digest would enable
// offline guessing and unwanted cross-callback correlation.
type AgentStageResultCallbackReceipt struct {
	SchemaVersion string                              `json:"schema_version"`
	ReceiptID     string                              `json:"receipt_id"`
	SHA256        string                              `json:"sha256"`
	Kind          AgentStageResultCallbackReceiptKind `json:"kind"`

	Subject     AgentPlanningSubject `json:"subject"`
	ReviewRunID string               `json:"review_run_id"`
	Stage       AgentStageRef        `json:"stage"`

	BindingID            string `json:"execution_binding_id"`
	BindingSHA256        string `json:"execution_binding_sha256"`
	ProviderHandleSHA256 string `json:"provider_handle_sha256"`

	ResultRef   ArtifactRef                         `json:"canonical_stage_execution_result_ref"`
	ProofSHA256 string                              `json:"callback_proof_sha256"`
	Verifier    AgentStageResultCallbackVerifierRef `json:"verifier"`
	VerifiedAt  time.Time                           `json:"verified_at"`
}

// AgentStageTerminalGateKind is the closed set of terminal authorities that
// may win for one exact dispatch intent. Both branches deliberately share one
// deterministic GateID, so the first durable append is authoritative.
type AgentStageTerminalGateKind string

const (
	AgentStageSucceededResultAccepted AgentStageTerminalGateKind = "succeeded_result_accepted"
	AgentStageFailedResultAccepted    AgentStageTerminalGateKind = "failed_result_accepted"
	AgentStageCanceledResultAccepted  AgentStageTerminalGateKind = "canceled_result_accepted"
	AgentStageCancellationRequested   AgentStageTerminalGateKind = "cancellation_requested"
)

// AgentStageSucceededResultAcceptance binds the exact provider result bytes
// and the local/governed output projection accepted by the host. AcceptedAt is
// a host observation used for audit and exact retry; ProviderResultRecordedAt
// is provider evidence and never decides the cancel/result race.
type AgentStageSucceededResultAcceptance struct {
	BindingID     string `json:"execution_binding_id"`
	BindingSHA256 string `json:"execution_binding_sha256"`

	ResultRef             ArtifactRef              `json:"stage_execution_result_ref"`
	Output                AgentArtifactProjection  `json:"output"`
	TraceManifest         *AgentArtifactProjection `json:"trace_manifest,omitempty"`
	CallbackReceiptRef    ArtifactRef              `json:"callback_receipt_ref"`
	CallbackReceiptSHA256 string                   `json:"callback_receipt_semantic_sha256"`

	Status            string   `json:"status"`
	Completeness      string   `json:"completeness"`
	CompletenessNotes []string `json:"completeness_notes"`

	ProviderResultRecordedAt time.Time `json:"provider_result_recorded_at"`
	AcceptedAt               time.Time `json:"accepted_at"`
}

// AgentStageExecutionFailure is the bounded typed failure echoed from an
// authenticated StageExecutionResult. It is operational terminal evidence,
// never a review Finding or evaluation label.
type AgentStageExecutionFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// AgentStageNonSucceededResultAcceptance closes a provider failed/canceled
// result over the exact execution binding and callback receipt. No output is
// permitted; an optional trace remains diagnostic provenance only.
type AgentStageNonSucceededResultAcceptance struct {
	BindingID     string `json:"execution_binding_id"`
	BindingSHA256 string `json:"execution_binding_sha256"`

	ResultRef              ArtifactRef              `json:"stage_execution_result_ref"`
	TraceManifest          *AgentArtifactProjection `json:"trace_manifest,omitempty"`
	AgentTaskEvidence      *AgentArtifactProjection `json:"agent_task_evidence,omitempty"`
	AgentExecutionReceipts *AgentArtifactProjection `json:"agent_execution_receipts,omitempty"`
	CallbackReceiptRef     ArtifactRef              `json:"callback_receipt_ref"`
	CallbackReceiptSHA256  string                   `json:"callback_receipt_semantic_sha256"`

	Status            string                     `json:"status"`
	Failure           AgentStageExecutionFailure `json:"failure"`
	Completeness      string                     `json:"completeness"`
	CompletenessNotes []string                   `json:"completeness_notes"`

	ProviderResultRecordedAt time.Time `json:"provider_result_recorded_at"`
	AcceptedAt               time.Time `json:"accepted_at"`
}

// AgentStageCancellationRequest is the durable host cancellation authority.
// Its idempotency key is independent from Create and is copied from the exact
// dispatch intent. No execution binding is required for this branch.
type AgentStageCancellationRequest struct {
	CancelIdempotencyKey string    `json:"cancel_idempotency_key"`
	Actor                string    `json:"actor"`
	Reason               string    `json:"reason"`
	RequestedAt          time.Time `json:"requested_at"`
}

// AgentStageTerminalGate is the sealed, append-only union that arbitrates a
// succeeded provider result against a formal cancellation request. It records
// provenance only and is not a Finding, Decision, Feedback, or evaluation
// label.
type AgentStageTerminalGate struct {
	SchemaVersion string                     `json:"schema_version"`
	GateID        string                     `json:"gate_id"`
	SHA256        string                     `json:"sha256"`
	Kind          AgentStageTerminalGateKind `json:"kind"`

	Subject     AgentPlanningSubject `json:"subject"`
	ReviewRunID string               `json:"review_run_id"`
	Stage       AgentStageRef        `json:"stage"`

	AdmissionID     string `json:"agent_stage_plan_admission_id"`
	AdmissionSHA256 string `json:"agent_stage_plan_admission_sha256"`
	IntentID        string `json:"dispatch_intent_id"`
	IntentSHA256    string `json:"dispatch_intent_sha256"`

	WorkloadID   string `json:"workload_id"`
	LeaseID      string `json:"lease_id"`
	LeaseWorker  string `json:"lease_worker_id"`
	ExecutionID  string `json:"execution_id"`
	Attempt      int    `json:"attempt"`
	Generation   int    `json:"generation"`
	FencingToken uint64 `json:"fencing_token"`

	RequestRef            ArtifactRef `json:"stage_execution_request_ref"`
	RequestSemanticSHA256 string      `json:"request_semantic_sha256"`
	RequestDeadline       time.Time   `json:"request_deadline"`
	CapabilitySHA256      string      `json:"capability_sha256"`

	Completion   *AgentStageSucceededResultAcceptance    `json:"completion,omitempty"`
	Outcome      *AgentStageNonSucceededResultAcceptance `json:"outcome,omitempty"`
	Cancellation *AgentStageCancellationRequest          `json:"cancellation,omitempty"`
}

func SealAgentStageResultCallbackReceipt(
	receipt AgentStageResultCallbackReceipt,
) (AgentStageResultCallbackReceipt, error) {
	if receipt.SchemaVersion != "" &&
		receipt.SchemaVersion != AgentStageResultCallbackReceiptSchemaVersion {
		return AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"unsupported AgentStageResultCallbackReceipt schema %q",
			receipt.SchemaVersion,
		)
	}
	if receipt.Kind != "" && receipt.Kind != AgentStageProviderCallbackVerified {
		return AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"unsupported agent-stage result callback receipt kind %q",
			receipt.Kind,
		)
	}
	receipt.SchemaVersion = AgentStageResultCallbackReceiptSchemaVersion
	receipt.Kind = AgentStageProviderCallbackVerified
	receipt.ReceiptID = ""
	receipt.SHA256 = ""
	if err := receipt.validateContent(); err != nil {
		return AgentStageResultCallbackReceipt{}, err
	}
	receiptID, err := AgentStageResultCallbackReceiptID(
		receipt.ReviewRunID,
		receipt.BindingID,
	)
	if err != nil {
		return AgentStageResultCallbackReceipt{}, err
	}
	receipt.ReceiptID = receiptID
	digest, err := DigestAgentStageResultCallbackReceipt(receipt)
	if err != nil {
		return AgentStageResultCallbackReceipt{}, err
	}
	receipt.SHA256 = digest
	if err := receipt.Validate(); err != nil {
		return AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"validate sealed AgentStageResultCallbackReceipt: %w",
			err,
		)
	}
	return receipt, nil
}

func DecodeAgentStageResultCallbackReceipt(
	data []byte,
) (AgentStageResultCallbackReceipt, error) {
	if err := rejectDuplicateSnapshotFields(data); err != nil {
		return AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"decode AgentStageResultCallbackReceipt structure: %w",
			err,
		)
	}
	var receipt AgentStageResultCallbackReceipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"decode AgentStageResultCallbackReceipt: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return AgentStageResultCallbackReceipt{}, fmt.Errorf(
				"AgentStageResultCallbackReceipt contains multiple JSON values",
			)
		}
		return AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"decode trailing AgentStageResultCallbackReceipt data: %w",
			err,
		)
	}
	if err := receipt.Validate(); err != nil {
		return AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"validate AgentStageResultCallbackReceipt: %w",
			err,
		)
	}
	return receipt, nil
}

func AgentStageResultCallbackReceiptID(runID, bindingID string) (string, error) {
	if err := validateAgentIdentity("review_run_id", runID); err != nil {
		return "", err
	}
	if err := validateAgentStageIdentifier("execution_binding_id", bindingID); err != nil {
		return "", err
	}
	if !strings.HasPrefix(bindingID, runID+"-agent-stage-execution-binding-") {
		return "", fmt.Errorf("execution_binding_id is not namespaced by review_run_id")
	}
	payload, err := json.Marshal(struct {
		ReviewRunID string `json:"review_run_id"`
		BindingID   string `json:"execution_binding_id"`
	}{ReviewRunID: runID, BindingID: bindingID})
	if err != nil {
		return "", fmt.Errorf("marshal agent-stage result callback receipt identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return runID + "-agent-stage-result-callback-receipt-" +
		hex.EncodeToString(digest[:12]), nil
}

func DigestAgentStageResultCallbackReceipt(
	receipt AgentStageResultCallbackReceipt,
) (string, error) {
	copy := receipt
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStageResultCallbackReceipt: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func AgentStageProviderHandleSHA256(providerHandle string) (string, error) {
	if err := validateAgentProviderHandle(providerHandle); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(providerHandle))
	return hex.EncodeToString(digest[:]), nil
}

// AgentStageResultCallbackProofSHA256 is a decision-tuple fingerprint, not a
// redaction mechanism for secrets. Callers must authenticate and classify the
// proof under the receipt contract above before invoking it.
func AgentStageResultCallbackProofSHA256(proof []byte) (string, error) {
	if len(proof) == 0 {
		return "", fmt.Errorf("callback proof must be non-empty")
	}
	digest := sha256.Sum256(proof)
	return hex.EncodeToString(digest[:]), nil
}

func (receipt AgentStageResultCallbackReceipt) Validate() error {
	if err := receipt.validateContent(); err != nil {
		return err
	}
	wantID, err := AgentStageResultCallbackReceiptID(
		receipt.ReviewRunID,
		receipt.BindingID,
	)
	if err != nil {
		return err
	}
	if receipt.ReceiptID != wantID {
		return fmt.Errorf(
			"receipt_id must be %q for the exact execution binding",
			wantID,
		)
	}
	if err := requireSHA256("sha256", receipt.SHA256); err != nil {
		return err
	}
	digest, err := DigestAgentStageResultCallbackReceipt(receipt)
	if err != nil {
		return err
	}
	if receipt.SHA256 != digest {
		return fmt.Errorf("sha256 does not match AgentStageResultCallbackReceipt content")
	}
	return nil
}

func (receipt AgentStageResultCallbackReceipt) validateContent() error {
	if receipt.SchemaVersion != AgentStageResultCallbackReceiptSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentStageResultCallbackReceipt schema %q",
			receipt.SchemaVersion,
		)
	}
	if receipt.Kind != AgentStageProviderCallbackVerified {
		return fmt.Errorf(
			"unsupported agent-stage result callback receipt kind %q",
			receipt.Kind,
		)
	}
	if err := receipt.Subject.Validate(); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	if err := validateAgentIdentity("review_run_id", receipt.ReviewRunID); err != nil {
		return err
	}
	if err := receipt.Stage.Validate(); err != nil {
		return err
	}
	if _, err := AgentStageResultCallbackReceiptID(
		receipt.ReviewRunID,
		receipt.BindingID,
	); err != nil {
		return err
	}
	for name, digest := range map[string]string{
		"execution_binding_sha256": receipt.BindingSHA256,
		"provider_handle_sha256":   receipt.ProviderHandleSHA256,
		"callback_proof_sha256":    receipt.ProofSHA256,
	} {
		if err := requireSHA256(name, digest); err != nil {
			return err
		}
	}
	if err := receipt.ResultRef.Validate(); err != nil {
		return fmt.Errorf("canonical_stage_execution_result_ref: %w", err)
	}
	if receipt.ResultRef.SizeBytes <= 0 ||
		receipt.ResultRef.Contract != ContractStageExecutionResult {
		return fmt.Errorf(
			"canonical_stage_execution_result_ref must be a non-empty %q artifact",
			ContractStageExecutionResult,
		)
	}
	if err := receipt.Verifier.Validate(); err != nil {
		return fmt.Errorf("verifier: %w", err)
	}
	if receipt.VerifiedAt.IsZero() || receipt.VerifiedAt.Location() != time.UTC {
		return fmt.Errorf("verified_at must be a non-zero UTC timestamp")
	}
	return nil
}

// Validate rejects floating verifier identities. A callback verifier must be
// pinned to an exact project-native ID, revision, and content digest before
// any caller-controlled result artifact is dereferenced.
func (reference AgentStageResultCallbackVerifierRef) Validate() error {
	if err := validateAgentStageIdentifier("id", reference.ID); err != nil {
		return err
	}
	if err := validateAgentStageIdentifier("revision", reference.Revision); err != nil {
		return err
	}
	if strings.EqualFold(reference.ID, "latest") ||
		strings.EqualFold(reference.Revision, "latest") {
		return fmt.Errorf("verifier must not use latest")
	}
	return requireSHA256("sha256", reference.SHA256)
}

func (receipt AgentStageResultCallbackReceipt) ValidateAgainstBindingAndResult(
	binding AgentStageExecutionBinding,
	resultRef ArtifactRef,
) error {
	if err := receipt.Validate(); err != nil {
		return fmt.Errorf("validate callback receipt: %w", err)
	}
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("validate execution binding: %w", err)
	}
	providerHandleSHA256, err := AgentStageProviderHandleSHA256(binding.ProviderHandle)
	if err != nil {
		return fmt.Errorf("digest execution binding provider handle: %w", err)
	}
	if receipt.Subject != binding.Subject ||
		receipt.ReviewRunID != binding.ReviewRunID ||
		receipt.Stage != binding.Stage ||
		receipt.BindingID != binding.BindingID ||
		receipt.BindingSHA256 != binding.SHA256 ||
		receipt.ProviderHandleSHA256 != providerHandleSHA256 ||
		receipt.ResultRef != resultRef {
		return fmt.Errorf(
			"callback receipt does not match exact execution binding and canonical result",
		)
	}
	if receipt.VerifiedAt.Before(binding.RecordedAt) {
		return fmt.Errorf("callback receipt verified_at predates execution binding recorded_at")
	}
	return nil
}

// SealAgentStageTerminalGate derives the sole terminal authority for an exact
// dispatch intent and seals the complete union payload.
func SealAgentStageTerminalGate(
	gate AgentStageTerminalGate,
) (AgentStageTerminalGate, error) {
	gate = cloneAgentStageTerminalGate(gate)
	if gate.SchemaVersion != "" && gate.SchemaVersion != AgentStageTerminalGateSchemaVersion {
		return AgentStageTerminalGate{}, fmt.Errorf(
			"unsupported AgentStageTerminalGate schema %q",
			gate.SchemaVersion,
		)
	}
	gate.SchemaVersion = AgentStageTerminalGateSchemaVersion
	gate.GateID = ""
	gate.SHA256 = ""
	if err := gate.validateContent(); err != nil {
		return AgentStageTerminalGate{}, err
	}
	gateID, err := AgentStageTerminalGateID(gate.ReviewRunID, gate.IntentID)
	if err != nil {
		return AgentStageTerminalGate{}, err
	}
	gate.GateID = gateID
	digest, err := DigestAgentStageTerminalGate(gate)
	if err != nil {
		return AgentStageTerminalGate{}, err
	}
	gate.SHA256 = digest
	if err := gate.Validate(); err != nil {
		return AgentStageTerminalGate{}, fmt.Errorf(
			"validate sealed AgentStageTerminalGate: %w",
			err,
		)
	}
	return gate, nil
}

func cloneAgentStageTerminalGate(gate AgentStageTerminalGate) AgentStageTerminalGate {
	if gate.Completion != nil {
		completion := *gate.Completion
		if completion.TraceManifest != nil {
			traceManifest := *completion.TraceManifest
			completion.TraceManifest = &traceManifest
		}
		if completion.CompletenessNotes != nil {
			completion.CompletenessNotes = append(
				make([]string, 0, len(completion.CompletenessNotes)),
				completion.CompletenessNotes...,
			)
		}
		gate.Completion = &completion
	}
	if gate.Outcome != nil {
		outcome := *gate.Outcome
		if outcome.TraceManifest != nil {
			traceManifest := *outcome.TraceManifest
			outcome.TraceManifest = &traceManifest
		}
		if outcome.AgentTaskEvidence != nil {
			taskEvidence := *outcome.AgentTaskEvidence
			outcome.AgentTaskEvidence = &taskEvidence
		}
		if outcome.AgentExecutionReceipts != nil {
			receipts := *outcome.AgentExecutionReceipts
			outcome.AgentExecutionReceipts = &receipts
		}
		if outcome.CompletenessNotes != nil {
			outcome.CompletenessNotes = append(
				make([]string, 0, len(outcome.CompletenessNotes)),
				outcome.CompletenessNotes...,
			)
		}
		gate.Outcome = &outcome
	}
	if gate.Cancellation != nil {
		cancellation := *gate.Cancellation
		gate.Cancellation = &cancellation
	}
	return gate
}

// AgentStageTerminalGateID returns one shared ID for both terminal branches.
func AgentStageTerminalGateID(runID, intentID string) (string, error) {
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
		return "", fmt.Errorf("marshal agent-stage terminal gate identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return runID + "-agent-stage-terminal-gate-" + hex.EncodeToString(digest[:12]), nil
}

func DigestAgentStageTerminalGate(gate AgentStageTerminalGate) (string, error) {
	copy := gate
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStageTerminalGate: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (gate AgentStageTerminalGate) Validate() error {
	if err := gate.validateContent(); err != nil {
		return err
	}
	wantID, err := AgentStageTerminalGateID(gate.ReviewRunID, gate.IntentID)
	if err != nil {
		return err
	}
	if gate.GateID != wantID {
		return fmt.Errorf("gate_id must be %q for the exact dispatch intent", wantID)
	}
	if err := requireSHA256("sha256", gate.SHA256); err != nil {
		return err
	}
	digest, err := DigestAgentStageTerminalGate(gate)
	if err != nil {
		return err
	}
	if gate.SHA256 != digest {
		return fmt.Errorf("sha256 does not match AgentStageTerminalGate content")
	}
	return nil
}

func (gate AgentStageTerminalGate) validateContent() error {
	if gate.SchemaVersion != AgentStageTerminalGateSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentStageTerminalGate schema %q",
			gate.SchemaVersion,
		)
	}
	if err := gate.Subject.Validate(); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	if err := validateAgentIdentity("review_run_id", gate.ReviewRunID); err != nil {
		return err
	}
	if err := gate.Stage.Validate(); err != nil {
		return err
	}
	wantAdmissionID, err := AgentStagePlanAdmissionID(gate.ReviewRunID, gate.Stage.ID)
	if err != nil {
		return err
	}
	if gate.AdmissionID != wantAdmissionID {
		return fmt.Errorf(
			"agent_stage_plan_admission_id must be %q for the exact run and stage",
			wantAdmissionID,
		)
	}
	if err := requireSHA256("agent_stage_plan_admission_sha256", gate.AdmissionSHA256); err != nil {
		return err
	}
	if _, err := AgentStageTerminalGateID(gate.ReviewRunID, gate.IntentID); err != nil {
		return err
	}
	if err := requireSHA256("dispatch_intent_sha256", gate.IntentSHA256); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"workload_id": gate.WorkloadID, "lease_id": gate.LeaseID,
		"lease_worker_id": gate.LeaseWorker, "execution_id": gate.ExecutionID,
	} {
		if err := validateAgentIdentity(name, value); err != nil {
			return err
		}
	}
	if gate.Attempt < 1 || gate.Generation < 1 ||
		uint64(gate.Attempt) > maxAgentJSONSafeInteger ||
		uint64(gate.Generation) > maxAgentJSONSafeInteger || gate.FencingToken == 0 ||
		gate.FencingToken > maxAgentJSONSafeInteger {
		return fmt.Errorf(
			"attempt, generation, and fencing_token must be JSON-safe positive integers",
		)
	}
	if err := gate.RequestRef.Validate(); err != nil {
		return fmt.Errorf("stage_execution_request_ref: %w", err)
	}
	if gate.RequestRef.SizeBytes <= 0 ||
		gate.RequestRef.Contract != ContractStageExecutionRequest {
		return fmt.Errorf(
			"stage_execution_request_ref must be a non-empty %q artifact",
			ContractStageExecutionRequest,
		)
	}
	if err := requireSHA256("request_semantic_sha256", gate.RequestSemanticSHA256); err != nil {
		return err
	}
	if gate.RequestDeadline.IsZero() || gate.RequestDeadline.Location() != time.UTC {
		return fmt.Errorf("request_deadline must be a non-zero UTC timestamp")
	}
	if err := requireSHA256("capability_sha256", gate.CapabilitySHA256); err != nil {
		return err
	}

	switch gate.Kind {
	case AgentStageSucceededResultAccepted:
		if gate.Completion == nil || gate.Outcome != nil || gate.Cancellation != nil {
			return fmt.Errorf(
				"succeeded_result_accepted gate requires completion and forbids cancellation or outcome",
			)
		}
		if err := gate.Completion.validate(gate); err != nil {
			return err
		}
	case AgentStageFailedResultAccepted, AgentStageCanceledResultAccepted:
		if gate.Outcome == nil || gate.Completion != nil || gate.Cancellation != nil {
			return fmt.Errorf(
				"%s gate requires outcome and forbids completion/cancellation",
				gate.Kind,
			)
		}
		if err := gate.Outcome.validate(gate); err != nil {
			return err
		}
	case AgentStageCancellationRequested:
		if gate.Cancellation == nil || gate.Completion != nil || gate.Outcome != nil {
			return fmt.Errorf(
				"cancellation_requested gate requires cancellation and forbids provider result",
			)
		}
		if err := gate.Cancellation.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported agent-stage terminal gate kind %q", gate.Kind)
	}
	return nil
}

func (gate AgentStageTerminalGate) ValidateOutcomeAgainstBinding(
	binding AgentStageExecutionBinding,
) error {
	if (gate.Kind != AgentStageFailedResultAccepted &&
		gate.Kind != AgentStageCanceledResultAccepted) || gate.Outcome == nil {
		return fmt.Errorf("terminal gate is not a failed/canceled result acceptance")
	}
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("validate execution binding: %w", err)
	}
	if gate.Subject != binding.Subject || gate.ReviewRunID != binding.ReviewRunID ||
		gate.Stage != binding.Stage || gate.WorkloadID != binding.WorkloadID ||
		gate.LeaseID != binding.LeaseID || gate.LeaseWorker != binding.LeaseWorker ||
		gate.IntentID != binding.IntentID || gate.IntentSHA256 != binding.IntentSHA256 ||
		gate.RequestRef != binding.RequestRef ||
		gate.RequestSemanticSHA256 != binding.RequestSemanticSHA256 ||
		gate.ExecutionID != binding.ExecutionID || gate.Attempt != binding.Attempt ||
		gate.Generation != binding.Generation || gate.FencingToken != binding.FencingToken ||
		gate.CapabilitySHA256 != binding.CapabilitySHA256 ||
		gate.Outcome.BindingID != binding.BindingID ||
		gate.Outcome.BindingSHA256 != binding.SHA256 {
		return fmt.Errorf("outcome does not match exact execution binding")
	}
	if gate.Outcome.AcceptedAt.Before(binding.RecordedAt) {
		return fmt.Errorf("accepted_at predates execution binding recorded_at")
	}
	return nil
}

func (completion AgentStageSucceededResultAcceptance) validate(
	gate AgentStageTerminalGate,
) error {
	wantBindingID, err := AgentStageExecutionBindingID(gate.ReviewRunID, gate.IntentID)
	if err != nil {
		return err
	}
	if completion.BindingID != wantBindingID {
		return fmt.Errorf(
			"execution_binding_id must be %q for the exact dispatch intent",
			wantBindingID,
		)
	}
	if err := requireSHA256("execution_binding_sha256", completion.BindingSHA256); err != nil {
		return err
	}
	if err := completion.ResultRef.Validate(); err != nil {
		return fmt.Errorf("stage_execution_result_ref: %w", err)
	}
	if completion.ResultRef.SizeBytes <= 0 ||
		completion.ResultRef.Contract != ContractStageExecutionResult {
		return fmt.Errorf(
			"stage_execution_result_ref must be a non-empty %q artifact",
			ContractStageExecutionResult,
		)
	}
	if err := completion.CallbackReceiptRef.Validate(); err != nil {
		return fmt.Errorf("callback_receipt_ref: %w", err)
	}
	if completion.CallbackReceiptRef.SizeBytes <= 0 ||
		completion.CallbackReceiptRef.Contract != ContractAgentStageResultCallbackReceipt {
		return fmt.Errorf(
			"callback_receipt_ref must be a non-empty %q artifact",
			ContractAgentStageResultCallbackReceipt,
		)
	}
	if err := requireSHA256(
		"callback_receipt_semantic_sha256",
		completion.CallbackReceiptSHA256,
	); err != nil {
		return err
	}
	if err := completion.Output.validate(
		"output",
		ContractReviewHypothesisSet,
		gate.Subject,
	); err != nil {
		return err
	}
	if completion.TraceManifest != nil {
		if err := completion.TraceManifest.validate(
			"trace_manifest",
			ContractStageExecutionTraceManifest,
			gate.Subject,
		); err != nil {
			return err
		}
	}
	if completion.Status != "succeeded" {
		return fmt.Errorf("completion status must be succeeded")
	}
	if completion.CompletenessNotes == nil {
		return fmt.Errorf("completion completeness_notes must be an explicit array")
	}
	if len(completion.CompletenessNotes) > 32 {
		return fmt.Errorf("completion completeness_notes must contain at most 32 entries")
	}
	for index, note := range completion.CompletenessNotes {
		if err := validateAgentTerminalNote(note); err != nil {
			return fmt.Errorf("completion completeness_notes[%d]: %w", index, err)
		}
	}
	switch completion.Completeness {
	case "complete":
		if len(completion.CompletenessNotes) != 0 {
			return fmt.Errorf("complete terminal result must not have completeness notes")
		}
	case "partial":
		if len(completion.CompletenessNotes) == 0 {
			return fmt.Errorf("partial terminal result must explain its completeness")
		}
	default:
		return fmt.Errorf("unsupported terminal result completeness %q", completion.Completeness)
	}
	if completion.ProviderResultRecordedAt.IsZero() ||
		completion.ProviderResultRecordedAt.Location() != time.UTC {
		return fmt.Errorf("provider_result_recorded_at must be a non-zero UTC timestamp")
	}
	if completion.ProviderResultRecordedAt.After(gate.RequestDeadline) {
		return fmt.Errorf("provider result was recorded after request_deadline")
	}
	if completion.AcceptedAt.IsZero() || completion.AcceptedAt.Location() != time.UTC {
		return fmt.Errorf("accepted_at must be a non-zero UTC timestamp")
	}
	if completion.AcceptedAt.Before(completion.ProviderResultRecordedAt) {
		return fmt.Errorf("accepted_at must not precede provider_result_recorded_at")
	}
	if completion.AcceptedAt.After(gate.RequestDeadline) {
		return fmt.Errorf("host accepted result after request_deadline")
	}
	return nil
}

func (outcome AgentStageNonSucceededResultAcceptance) validate(
	gate AgentStageTerminalGate,
) error {
	wantBindingID, err := AgentStageExecutionBindingID(gate.ReviewRunID, gate.IntentID)
	if err != nil {
		return err
	}
	if outcome.BindingID != wantBindingID {
		return fmt.Errorf(
			"execution_binding_id must be %q for the exact dispatch intent",
			wantBindingID,
		)
	}
	if err := requireSHA256("execution_binding_sha256", outcome.BindingSHA256); err != nil {
		return err
	}
	if err := outcome.ResultRef.Validate(); err != nil {
		return fmt.Errorf("stage_execution_result_ref: %w", err)
	}
	if outcome.ResultRef.SizeBytes <= 0 ||
		outcome.ResultRef.Contract != ContractStageExecutionResult {
		return fmt.Errorf(
			"stage_execution_result_ref must be a non-empty %q artifact",
			ContractStageExecutionResult,
		)
	}
	if err := outcome.CallbackReceiptRef.Validate(); err != nil {
		return fmt.Errorf("callback_receipt_ref: %w", err)
	}
	if outcome.CallbackReceiptRef.SizeBytes <= 0 ||
		outcome.CallbackReceiptRef.Contract != ContractAgentStageResultCallbackReceipt {
		return fmt.Errorf(
			"callback_receipt_ref must be a non-empty %q artifact",
			ContractAgentStageResultCallbackReceipt,
		)
	}
	if err := requireSHA256(
		"callback_receipt_semantic_sha256",
		outcome.CallbackReceiptSHA256,
	); err != nil {
		return err
	}
	if outcome.TraceManifest != nil {
		if err := outcome.TraceManifest.validate(
			"trace_manifest",
			ContractStageExecutionTraceManifest,
			gate.Subject,
		); err != nil {
			return err
		}
	}
	if (outcome.AgentTaskEvidence == nil) != (outcome.AgentExecutionReceipts == nil) {
		return fmt.Errorf("outcome diagnostic task evidence and receipts must be present together")
	}
	if outcome.AgentTaskEvidence != nil {
		if err := outcome.AgentTaskEvidence.validate(
			"agent_task_evidence",
			ContractAgentReviewTaskEvidence,
			gate.Subject,
		); err != nil {
			return err
		}
		if err := outcome.AgentExecutionReceipts.validate(
			"agent_execution_receipts",
			ContractAgentExecutionReceipts,
			gate.Subject,
		); err != nil {
			return err
		}
	}
	wantStatus := "failed"
	if gate.Kind == AgentStageCanceledResultAccepted {
		wantStatus = "canceled"
	}
	if outcome.Status != wantStatus {
		return fmt.Errorf("%s outcome status must be %s", gate.Kind, wantStatus)
	}
	if err := outcome.Failure.validate(); err != nil {
		return fmt.Errorf("failure: %w", err)
	}
	if outcome.CompletenessNotes == nil {
		return fmt.Errorf("outcome completeness_notes must be an explicit array")
	}
	if len(outcome.CompletenessNotes) > 32 {
		return fmt.Errorf("outcome completeness_notes must contain at most 32 entries")
	}
	for index, note := range outcome.CompletenessNotes {
		if err := validateAgentTerminalNote(note); err != nil {
			return fmt.Errorf("outcome completeness_notes[%d]: %w", index, err)
		}
	}
	switch outcome.Completeness {
	case "complete":
		if len(outcome.CompletenessNotes) != 0 {
			return fmt.Errorf("complete terminal outcome must not have completeness notes")
		}
	case "partial":
		if len(outcome.CompletenessNotes) == 0 {
			return fmt.Errorf("partial terminal outcome must explain its completeness")
		}
	default:
		return fmt.Errorf("unsupported terminal outcome completeness %q", outcome.Completeness)
	}
	if outcome.ProviderResultRecordedAt.IsZero() ||
		outcome.ProviderResultRecordedAt.Location() != time.UTC {
		return fmt.Errorf("provider_result_recorded_at must be a non-zero UTC timestamp")
	}
	if outcome.ProviderResultRecordedAt.After(gate.RequestDeadline) {
		return fmt.Errorf("provider result was recorded after request_deadline")
	}
	if outcome.AcceptedAt.IsZero() || outcome.AcceptedAt.Location() != time.UTC {
		return fmt.Errorf("accepted_at must be a non-zero UTC timestamp")
	}
	if outcome.AcceptedAt.Before(outcome.ProviderResultRecordedAt) {
		return fmt.Errorf("accepted_at must not precede provider_result_recorded_at")
	}
	if outcome.AcceptedAt.After(gate.RequestDeadline) {
		return fmt.Errorf("host accepted result after request_deadline")
	}
	return nil
}

func (failure AgentStageExecutionFailure) validate() error {
	if err := validateAgentStageIdentifier("code", failure.Code); err != nil {
		return err
	}
	return validateAgentTerminalFailureMessage(failure.Message)
}

func (cancellation AgentStageCancellationRequest) validate() error {
	if err := validateAgentIdentity(
		"cancel_idempotency_key",
		cancellation.CancelIdempotencyKey,
	); err != nil {
		return err
	}
	if err := validateAgentIdentity("actor", cancellation.Actor); err != nil {
		return err
	}
	if err := validateAgentTerminalReason(cancellation.Reason); err != nil {
		return err
	}
	if cancellation.RequestedAt.IsZero() || cancellation.RequestedAt.Location() != time.UTC {
		return fmt.Errorf("requested_at must be a non-zero UTC timestamp")
	}
	return nil
}

// ValidateAgainstAdmissionAndIntent closes every duplicated gate field over
// the authoritative admission and dispatch intent. The completion-specific
// execution binding is validated separately.
func (gate AgentStageTerminalGate) ValidateAgainstAdmissionAndIntent(
	admission AgentStagePlanAdmission,
	intent AgentStageDispatchIntent,
) error {
	if err := gate.Validate(); err != nil {
		return fmt.Errorf("validate agent-stage terminal gate: %w", err)
	}
	if err := intent.ValidateAgainstAdmission(admission); err != nil {
		return fmt.Errorf("validate dispatch intent against admission: %w", err)
	}
	if gate.Subject != admission.Subject || gate.Subject != intent.Subject ||
		gate.ReviewRunID != admission.ReviewRunID || gate.ReviewRunID != intent.ReviewRunID ||
		gate.Stage != admission.Stage || gate.Stage != intent.Stage ||
		gate.AdmissionID != admission.AdmissionID || gate.AdmissionSHA256 != admission.SHA256 ||
		gate.IntentID != intent.IntentID || gate.IntentSHA256 != intent.SHA256 ||
		gate.WorkloadID != intent.WorkloadID || gate.LeaseID != intent.LeaseID ||
		gate.LeaseWorker != intent.LeaseWorker || gate.ExecutionID != intent.ExecutionID ||
		gate.Attempt != intent.Attempt || gate.Generation != intent.Generation ||
		gate.FencingToken != intent.FencingToken || gate.RequestRef != intent.RequestRef ||
		gate.RequestSemanticSHA256 != intent.RequestSemanticSHA256 ||
		gate.CapabilitySHA256 != intent.CapabilitySHA256 {
		return fmt.Errorf(
			"agent-stage terminal gate does not match exact admission and dispatch intent",
		)
	}
	if !gate.RequestDeadline.After(intent.RecordedAt) {
		return fmt.Errorf("request_deadline must be after dispatch intent recorded_at")
	}
	if gate.Completion != nil {
		if gate.Completion.ProviderResultRecordedAt.Before(intent.RecordedAt) {
			return fmt.Errorf("provider result predates dispatch intent")
		}
		if gate.Completion.AcceptedAt.Before(intent.RecordedAt) {
			return fmt.Errorf("accepted_at predates dispatch intent")
		}
	}
	if gate.Outcome != nil {
		if gate.Outcome.ProviderResultRecordedAt.Before(intent.RecordedAt) {
			return fmt.Errorf("provider result predates dispatch intent")
		}
		if gate.Outcome.AcceptedAt.Before(intent.RecordedAt) {
			return fmt.Errorf("accepted_at predates dispatch intent")
		}
	}
	if gate.Cancellation != nil {
		if gate.Cancellation.CancelIdempotencyKey != intent.CancelIdempotencyKey {
			return fmt.Errorf("cancellation does not bind the exact cancel idempotency key")
		}
		if gate.Cancellation.RequestedAt.Before(intent.RecordedAt) {
			return fmt.Errorf("cancellation requested_at predates dispatch intent")
		}
	}
	return nil
}

func (gate AgentStageTerminalGate) ValidateCompletionAgainstBinding(
	binding AgentStageExecutionBinding,
) error {
	if gate.Kind != AgentStageSucceededResultAccepted || gate.Completion == nil {
		return fmt.Errorf("terminal gate is not a succeeded result acceptance")
	}
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("validate execution binding: %w", err)
	}
	if gate.Subject != binding.Subject || gate.ReviewRunID != binding.ReviewRunID ||
		gate.Stage != binding.Stage || gate.WorkloadID != binding.WorkloadID ||
		gate.LeaseID != binding.LeaseID || gate.LeaseWorker != binding.LeaseWorker ||
		gate.IntentID != binding.IntentID || gate.IntentSHA256 != binding.IntentSHA256 ||
		gate.RequestRef != binding.RequestRef ||
		gate.RequestSemanticSHA256 != binding.RequestSemanticSHA256 ||
		gate.ExecutionID != binding.ExecutionID || gate.Attempt != binding.Attempt ||
		gate.Generation != binding.Generation || gate.FencingToken != binding.FencingToken ||
		gate.CapabilitySHA256 != binding.CapabilitySHA256 ||
		gate.Completion.BindingID != binding.BindingID ||
		gate.Completion.BindingSHA256 != binding.SHA256 {
		return fmt.Errorf("completion does not match exact execution binding")
	}
	if gate.Completion.AcceptedAt.Before(binding.RecordedAt) {
		return fmt.Errorf("accepted_at predates execution binding recorded_at")
	}
	return nil
}

// EventTime is the host-observed branch time copied into the append envelope.
// Append order, not this timestamp or the provider timestamp, chooses a winner.
func (gate AgentStageTerminalGate) EventTime() time.Time {
	if gate.Completion != nil {
		return gate.Completion.AcceptedAt
	}
	if gate.Outcome != nil {
		return gate.Outcome.AcceptedAt
	}
	if gate.Cancellation != nil {
		return gate.Cancellation.RequestedAt
	}
	return time.Time{}
}

func validateAgentTerminalReason(value string) error {
	if value == "" || len(value) > 1024 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("reason must be a non-empty trimmed string of at most 1024 bytes")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("reason must not contain control characters")
		}
	}
	return nil
}

func validateAgentTerminalFailureMessage(value string) error {
	if value == "" || len(value) > 4096 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf(
			"message must be a non-empty trimmed string of at most 4096 bytes",
		)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("message must not contain control characters")
		}
	}
	return nil
}

func validateAgentTerminalNote(value string) error {
	if value == "" || len(value) > 1024 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("note must be a non-empty trimmed string of at most 1024 bytes")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("note must not contain control characters")
		}
	}
	return nil
}
