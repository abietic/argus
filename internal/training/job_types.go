package training

import (
	"errors"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runmodel"
)

const (
	JobPrepareRequestSchemaVersion     = "argus.training_job_prepare_request.v1alpha1"
	ProviderJobReceiptSchemaVersion    = "argus.training_provider_job_receipt.v1alpha1"
	JobObservationRequestSchemaVersion = "argus.training_job_observation_request.v1alpha1"
	JobPlanSchemaVersion               = "argus.training_job_plan.v1alpha1"
	jobLedgerEventSchemaVersion        = "argus.training_job_ledger_event.v1alpha1"
	jobLedgerStream                    = "training/jobs"
	TrainingObjectiveSFT               = "supervised_fine_tuning"
	TrainingExecutionExternalManual    = "external_manual"
	TrainingReceiptAuthority           = "operator_recorded_unattested"
)

var (
	ErrJobNotFound          = errors.New("training job not found")
	ErrJobConflict          = errors.New("training job conflict")
	ErrJobCorrupt           = errors.New("corrupt training job repository")
	ErrJobInvalidTransition = errors.New("invalid training job transition")
)

type TrainingComponentRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

type JobHyperparameters struct {
	Epochs             uint32 `json:"epochs"`
	BatchSize          uint32 `json:"batch_size"`
	LearningRateMicros uint32 `json:"learning_rate_micros"`
	Seed               uint64 `json:"seed"`
}

type JobPrepareRequest struct {
	SchemaVersion           string               `json:"schema_version"`
	JobID                   string               `json:"job_id"`
	ExportID                string               `json:"export_id"`
	ExportBundleRef         runmodel.ArtifactRef `json:"export_bundle_ref"`
	ProviderID              string               `json:"provider_id"`
	ProviderProfileRevision string               `json:"provider_profile_revision"`
	BaseModel               TrainingComponentRef `json:"base_model"`
	Objective               string               `json:"objective"`
	Hyperparameters         JobHyperparameters   `json:"hyperparameters"`
	CreatedAt               time.Time            `json:"created_at"`
}

type JobStatus string

const (
	JobStatusPrepared  JobStatus = "prepared"
	JobStatusSubmitted JobStatus = "submitted"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCanceled  JobStatus = "canceled"
)

type ProviderJobReceipt struct {
	SchemaVersion  string    `json:"schema_version"`
	ProviderID     string    `json:"provider_id"`
	ExternalJobID  string    `json:"external_job_id"`
	Status         JobStatus `json:"status"`
	OutputModelID  string    `json:"output_model_id,omitempty"`
	Authority      string    `json:"authority"`
	ContainsSecret bool      `json:"contains_secret"`
	ObservedAt     time.Time `json:"observed_at"`
	SHA256         string    `json:"sha256"`
}

type JobObservationRequest struct {
	SchemaVersion      string             `json:"schema_version"`
	JobID              string             `json:"job_id"`
	ExpectedPlanSHA256 string             `json:"expected_plan_sha256"`
	Receipt            ProviderJobReceipt `json:"receipt"`
	ObservedAt         time.Time          `json:"observed_at"`
}

type JobObservation struct {
	ReceiptRef runmodel.ArtifactRef `json:"receipt_ref"`
	Receipt    ProviderJobReceipt   `json:"receipt"`
	RecordedBy string               `json:"recorded_by"`
	RecordedAt time.Time            `json:"recorded_at"`
}

type JobPlan struct {
	SchemaVersion      string            `json:"schema_version"`
	Request            JobPrepareRequest `json:"request"`
	ExportBundleSHA256 string            `json:"export_bundle_sha256"`
	PortableFormat     string            `json:"portable_format"`
	ExecutionMode      string            `json:"execution_mode"`
	RemoteSideEffects  string            `json:"remote_side_effects"`
	ReceiptAuthority   string            `json:"receipt_authority"`
	PromotionEligible  bool              `json:"promotion_eligible"`
	Status             JobStatus         `json:"status"`
	Observations       []JobObservation  `json:"observations"`
	CreatedBy          string            `json:"created_by"`
	UpdatedBy          string            `json:"updated_by"`
	UpdatedAt          time.Time         `json:"updated_at"`
	SHA256             string            `json:"sha256"`
}

type JobRecord struct {
	PlanRef  runmodel.ArtifactRef `json:"plan_ref"`
	Plan     JobPlan              `json:"plan"`
	Mutation evaluation.Mutation  `json:"mutation"`
}

type jobLedgerEvent struct {
	SchemaVersion string                 `json:"schema_version"`
	Type          string                 `json:"type"`
	JobID         string                 `json:"job_id"`
	Prepare       *JobPrepareRequest     `json:"prepare,omitempty"`
	Observation   *JobObservationRequest `json:"observation,omitempty"`
	PlanRef       runmodel.ArtifactRef   `json:"plan_ref"`
	Mutation      evaluation.Mutation    `json:"mutation"`
}
