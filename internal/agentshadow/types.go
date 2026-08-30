// Package agentshadow imports and queries non-attested agent review evidence.
//
// The package deliberately keeps shadow hypotheses and diagnostic receipts
// outside the authoritative ReviewRun, Finding, Decision, and Feedback
// ledgers. A successful import is visible only after its immutable commit
// record has been written.
package agentshadow

import (
	"errors"
	"time"

	"argus.local/argus/internal/artifactrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	importIntentSchemaVersion     = "argus.agent_review_import_intent.v1alpha1"
	ImportRecordSchemaVersion     = "argus.agent_review_import_record.v1alpha1"
	ExecutionAttemptSchemaVersion = "argus.agent_review_execution_attempt.v1alpha3"

	maxPlanBytes         = 1 << 20
	maxHypothesisBytes   = 16 << 20
	maxRawCandidateBytes = 16 << 20
	maxTaskEvidenceBytes = 16 << 20
	maxReceiptBytes      = 16 << 20
)

var (
	ErrConflict       = errors.New("agent review shadow import conflicts with existing state")
	ErrCorrupt        = errors.New("agent review shadow state is corrupt")
	ErrNotFound       = errors.New("agent review shadow import not found")
	ErrUnknownOutcome = errors.New("agent review worker has an orphan execution intent with unknown outcome")
	ErrWorkerReported = errors.New("agent review worker reported a terminal failure")
)

const (
	LocalTenantID    = "local"
	LocalWorkspaceID = "local"
)

type Scope struct {
	TenantID    string `json:"tenant_id"`
	WorkspaceID string `json:"workspace_id"`
}

// ImportRequest contains worker-produced contract bytes. The Go host strictly
// decodes and canonicalizes them before any result artifact is persisted.
// Manifest and Observation are intentionally absent because only the host may
// construct those records.
type ImportRequest struct {
	Scope                  Scope
	IdempotencyKey         string
	Plan                   []byte
	HypothesisSet          []byte
	RawCandidateCollection []byte
	TaskEvidenceCollection []byte
	ReceiptCollection      []byte
}

type QueryRequest struct {
	Scope      Scope
	ManifestID string
}

type ImportRecord struct {
	SchemaVersion             string                            `json:"schema_version"`
	TenantID                  string                            `json:"tenant_id"`
	WorkspaceID               string                            `json:"workspace_id"`
	IdempotencyKey            string                            `json:"idempotency_key"`
	InputDigest               string                            `json:"input_digest"`
	AcceptedAt                time.Time                         `json:"accepted_at"`
	ManifestID                string                            `json:"manifest_id"`
	ObservationID             string                            `json:"observation_id"`
	ObservationStream         string                            `json:"observation_stream"`
	AgentReviewPlanRef        contractsv1alpha1.ArtifactBinding `json:"agent_review_plan_ref"`
	HypothesisSetRef          contractsv1alpha1.ArtifactBinding `json:"hypothesis_set_ref"`
	RawCandidateCollectionRef contractsv1alpha1.ArtifactBinding `json:"raw_candidate_collection_ref"`
	TaskEvidenceCollectionRef contractsv1alpha1.ArtifactBinding `json:"task_evidence_collection_ref"`
	ReceiptCollectionRef      contractsv1alpha1.ArtifactBinding `json:"receipt_collection_ref"`
	ManifestRef               contractsv1alpha1.ArtifactBinding `json:"manifest_ref"`
}

type Result struct {
	Record                 ImportRecord                                        `json:"record"`
	Plan                   contractsv1alpha1.AgentReviewPlan                   `json:"plan"`
	HypothesisSet          contractsv1alpha1.ReviewHypothesisSet               `json:"hypothesis_set"`
	RawCandidateCollection contractsv1alpha1.AgentReviewRawCandidateCollection `json:"raw_candidate_collection"`
	// TaskEvidenceCollection is intentionally excluded from generic JSON. It
	// contains exact local prompts, tool arguments/results, and model output;
	// disclosure must go through the audited evidence read operation.
	TaskEvidenceCollection contractsv1alpha1.AgentReviewTaskEvidenceCollection `json:"-"`
	TaskEvidence           TaskEvidenceSummary                                 `json:"task_evidence"`
	ReceiptCollection      contractsv1alpha1.AgentExecutionReceiptCollection   `json:"receipt_collection"`
	Manifest               contractsv1alpha1.AgentReviewResultManifest         `json:"manifest"`
	Observation            contractsv1alpha1.AgentReviewObservation            `json:"observation"`
}

type TaskEvidenceSummary struct {
	Ref                contractsv1alpha1.ArtifactBinding `json:"ref"`
	ContentPolicy      string                            `json:"content_policy"`
	Completeness       string                            `json:"completeness,omitempty"`
	ReasonCodes        []string                          `json:"reason_codes,omitempty"`
	TaskExecutionCount int                               `json:"task_execution_count,omitempty"`
	State              string                            `json:"state"`
	ExportPolicy       string                            `json:"export_policy"`
	RetentionPolicy    string                            `json:"retention_policy"`
	DeletionSemantics  string                            `json:"deletion_semantics"`
}

type TaskEvidenceReadRequest struct {
	Scope      Scope
	ManifestID string
	Access     artifactrepo.SensitiveAccessRequest
}

type TaskEvidenceReadResult struct {
	Evidence contractsv1alpha1.AgentReviewTaskEvidenceCollection `json:"evidence"`
	Proof    artifactrepo.SensitiveAccessProof                   `json:"access_proof"`
}

type TaskEvidenceRevokeRequest struct {
	Scope          Scope
	ManifestID     string
	IdempotencyKey string
	Actor          string
	Reason         string
	At             time.Time
}

type LocalRunRequest struct {
	SourceRunID     string
	IdempotencyKey  string
	NodePath        string
	WorkerScript    string
	ProviderProfile string
	Model           string
	Skills          []string
	Budget          contractsv1alpha1.AgentReviewBudget
	Environment     map[string]string
}

type LocalRunResult struct {
	Result              Result            `json:"result"`
	Attempt             *ExecutionAttempt `json:"attempt,omitempty"`
	Reused              bool              `json:"reused"`
	OutcomeAcknowledged bool              `json:"outcome_acknowledged"`
}

type ExecutionStatus string

const (
	ExecutionStatusSucceeded      ExecutionStatus = "succeeded"
	ExecutionStatusFailed         ExecutionStatus = "failed"
	ExecutionStatusCanceled       ExecutionStatus = "canceled"
	ExecutionStatusUnknownOutcome ExecutionStatus = "unknown_outcome"
)

// ExecutionRuntimeDigests binds an execution attempt to the exact host input,
// executable, worker entrypoint, and bounded worker package observed before
// the immutable execution intent was accepted.
type ExecutionRuntimeDigests struct {
	WorkerRequestSHA256 string `json:"worker_request_sha256"`
	NodeSHA256          string `json:"node_sha256"`
	WorkerScriptSHA256  string `json:"worker_script_sha256"`
	WorkerPackageSHA256 string `json:"worker_package_sha256"`
}

// ExecutionHostFailureStage is a closed host-side pipeline taxonomy. It is
// intentionally not an arbitrary string supplied by an error or provider.
type ExecutionHostFailureStage string

const (
	ExecutionHostFailureWorkerRun        ExecutionHostFailureStage = "worker_run"
	ExecutionHostFailureResultDecode     ExecutionHostFailureStage = "result_decode"
	ExecutionHostFailureResultBinding    ExecutionHostFailureStage = "result_binding"
	ExecutionHostFailureEvidenceMapping  ExecutionHostFailureStage = "evidence_mapping"
	ExecutionHostFailureEvidenceEncoding ExecutionHostFailureStage = "evidence_encoding"
	ExecutionHostFailureShadowImport     ExecutionHostFailureStage = "shadow_import"
	ExecutionHostFailureCompletionCommit ExecutionHostFailureStage = "completion_commit"
)

// ExecutionHostFailureCode is a closed, deliberately coarse diagnosis. Raw
// Go errors, worker stderr, provider payloads, URLs, and credentials are never
// copied into the durable observation.
type ExecutionHostFailureCode string

const (
	ExecutionHostFailureRunnerUnconfirmed     ExecutionHostFailureCode = "runner_outcome_unconfirmed"
	ExecutionHostFailureResultRejected        ExecutionHostFailureCode = "worker_result_rejected"
	ExecutionHostFailureBindingRejected       ExecutionHostFailureCode = "worker_result_binding_rejected"
	ExecutionHostFailureMappingRejected       ExecutionHostFailureCode = "worker_evidence_mapping_rejected"
	ExecutionHostFailureEncodingFailed        ExecutionHostFailureCode = "host_evidence_encoding_failed"
	ExecutionHostFailureImportUnconfirmed     ExecutionHostFailureCode = "shadow_import_unconfirmed"
	ExecutionHostFailureCompletionUnconfirmed ExecutionHostFailureCode = "completion_commit_unconfirmed"
)

type ExecutionHostObservationTimeSource string

const (
	ExecutionHostObservationClock              ExecutionHostObservationTimeSource = "host_clock"
	ExecutionHostObservationAcceptedLowerBound ExecutionHostObservationTimeSource = "accepted_at_lower_bound"
)

// ExecutionHostFailureObservation is the bounded, host-owned fact that a
// post-intent pipeline stage failed without establishing a terminal outcome.
// It never changes the execution status by itself.
type ExecutionHostFailureObservation struct {
	SchemaVersion  string                             `json:"schema_version"`
	Scope          Scope                              `json:"scope"`
	IdempotencyKey string                             `json:"idempotency_key"`
	SemanticDigest string                             `json:"semantic_digest"`
	ExecutionID    string                             `json:"execution_id"`
	IntentSHA256   string                             `json:"intent_sha256"`
	Stage          ExecutionHostFailureStage          `json:"stage"`
	ReasonCode     ExecutionHostFailureCode           `json:"reason_code"`
	ObservedAt     time.Time                          `json:"observed_at"`
	TimeSource     ExecutionHostObservationTimeSource `json:"time_source"`
}

// ExecutionAttempt is a strict read projection over one immutable execution
// intent, its optional immutable host-failure observation, and its optional
// immutable terminal completion. UnknownOutcome is derived from an accepted
// intent with no valid completion; a host failure adds diagnosis without
// changing that outcome. A succeeded attempt includes its fully revalidated
// shadow result manifest; failed and canceled attempts expose only the
// host-redacted worker failure.
type ExecutionAttempt struct {
	SchemaVersion string          `json:"schema_version"`
	Scope         Scope           `json:"scope"`
	Status        ExecutionStatus `json:"status"`

	IdempotencyKey               string  `json:"idempotency_key"`
	SemanticDigest               string  `json:"semantic_digest"`
	IntentSHA256                 string  `json:"intent_sha256"`
	CompletionSHA256             *string `json:"completion_sha256,omitempty"`
	HostFailureObservationSHA256 *string `json:"host_failure_observation_sha256,omitempty"`

	Plan        contractsv1alpha1.AgentReviewPlan `json:"plan"`
	PlanID      string                            `json:"plan_id"`
	SourceRunID string                            `json:"source_run_id"`
	ExecutionID string                            `json:"execution_id"`
	ReviewRunID string                            `json:"review_run_id"`
	Runtime     ExecutionRuntimeDigests           `json:"runtime_digests"`

	AcceptedAt  time.Time                                    `json:"accepted_at"`
	ObservedAt  time.Time                                    `json:"observed_at"`
	CompletedAt *time.Time                                   `json:"completed_at,omitempty"`
	Failure     *contractsv1alpha1.AgentReviewWorkerFailure  `json:"failure,omitempty"`
	Manifest    *contractsv1alpha1.AgentReviewResultManifest `json:"manifest,omitempty"`
	HostFailure *ExecutionHostFailureObservation             `json:"host_failure,omitempty"`
}

type ExecutionQueryRequest struct {
	Scope       Scope
	ExecutionID string
}

// ExecutionListRequest selects execution attempts by the host-observed time.
// The window is half-open. Unknown outcomes are observed at the first durable
// host failure when present, otherwise at intent acceptance; terminal outcomes
// are observed at their immutable completion commit time.
type ExecutionListRequest struct {
	Scope          Scope
	StartInclusive time.Time
	EndExclusive   time.Time
}

type ExecutionReconcileRequest struct {
	Scope       Scope
	ExecutionID string
}

type WorkerReportedError struct {
	Status  contractsv1alpha1.AgentReviewWorkerStatus
	Failure contractsv1alpha1.AgentReviewWorkerFailure
}

func (failure *WorkerReportedError) Error() string {
	if failure == nil {
		return ErrWorkerReported.Error()
	}
	return ErrWorkerReported.Error() + ": " + failure.Failure.Code
}

func (failure *WorkerReportedError) Unwrap() error { return ErrWorkerReported }

type executionIntent struct {
	SchemaVersion       string                            `json:"schema_version"`
	TenantID            string                            `json:"tenant_id"`
	WorkspaceID         string                            `json:"workspace_id"`
	IdempotencyKey      string                            `json:"idempotency_key"`
	SemanticDigest      string                            `json:"semantic_digest"`
	WorkerRequestSHA256 string                            `json:"worker_request_sha256"`
	NodeSHA256          string                            `json:"node_sha256"`
	WorkerScriptSHA256  string                            `json:"worker_script_sha256"`
	WorkerPackageSHA256 string                            `json:"worker_package_sha256"`
	Plan                contractsv1alpha1.AgentReviewPlan `json:"plan"`
	AcceptedAt          time.Time                         `json:"accepted_at"`
}

type executionCompletion struct {
	SchemaVersion  string                                      `json:"schema_version"`
	TenantID       string                                      `json:"tenant_id"`
	WorkspaceID    string                                      `json:"workspace_id"`
	IdempotencyKey string                                      `json:"idempotency_key"`
	SemanticDigest string                                      `json:"semantic_digest"`
	Status         contractsv1alpha1.AgentReviewWorkerStatus   `json:"status"`
	ManifestID     string                                      `json:"manifest_id,omitempty"`
	Failure        *contractsv1alpha1.AgentReviewWorkerFailure `json:"failure,omitempty"`
	// CompletedAt is reported by the non-attested worker. RecordedAt is the
	// host-owned commit observation and is the only terminal history window
	// timestamp.
	CompletedAt time.Time `json:"completed_at"`
	RecordedAt  time.Time `json:"recorded_at"`
}

type importIntent struct {
	SchemaVersion  string    `json:"schema_version"`
	TenantID       string    `json:"tenant_id"`
	WorkspaceID    string    `json:"workspace_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	InputDigest    string    `json:"input_digest"`
	AcceptedAt     time.Time `json:"accepted_at"`
}
