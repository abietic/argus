// Package calibration owns governed fitting and validation of Finding
// confidence profiles. It deliberately consumes independently adjudicated
// labels; a ReviewRun or Finding is never accepted as its own truth source.
package calibration

import (
	"errors"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
)

const (
	FitRequestSchemaVersion       = "argus.calibration_fit_request.v1alpha1"
	DatasetManifestSchemaVersion  = "argus.calibration_dataset_manifest.v1alpha1"
	ProfileCandidateSchemaVersion = "argus.calibration_profile_candidate.v1alpha1"
	ReportSchemaVersion           = "argus.calibration_report.v1alpha1"
	RunSchemaVersion              = "argus.calibration_run.v1alpha1"

	ContractDatasetManifest = "argus.calibration_dataset_manifest.v1alpha1+json"
	ContractRun             = "argus.calibration_run.v1alpha1+json"

	streamName  = "calibration/runs"
	eventSchema = "argus.calibration_run_event.v1alpha1"
)

var (
	ErrNotFound     = errors.New("calibration run not found")
	ErrConflict     = errors.New("calibration mutation conflict")
	ErrUnauthorized = errors.New("calibration access unauthorized")
	ErrContaminated = errors.New("calibration dataset contaminated")
	ErrGateFailed   = errors.New("calibration quality gate failed")
	ErrCorrupt      = errors.New("corrupt calibration repository")
)

type Truth string

const (
	TruthValidDefect   Truth = "valid_defect"
	TruthFalsePositive Truth = "false_positive"
)

type AuthorityKind string

const (
	AuthorityHumanAdjudication  AuthorityKind = "human_adjudication"
	AuthorityExternalGovernance AuthorityKind = "external_governance"
)

// LabelAuthority records why a candidate-level truth label is independent
// from Argus output. The exact statement is closed so vague self-attestation
// cannot silently become training authority.
type LabelAuthority struct {
	Kind          AuthorityKind `json:"kind"`
	ReviewerIDs   []string      `json:"reviewer_ids"`
	AdjudicatorID string        `json:"adjudicator_id"`
	EvidenceRefs  []string      `json:"evidence_refs"`
	Statement     string        `json:"statement"`
}

const IndependentAuthorityStatement = "independently_adjudicated_not_argus_output"

// Observation binds one independent binary truth label to the exact raw
// confidence-bearing candidate and the exact governed EvaluationCase state.
type Observation struct {
	ObservationID          string         `json:"observation_id"`
	CaseID                 string         `json:"case_id"`
	CaseGovernanceRevision uint64         `json:"case_governance_revision"`
	CaseLabelRevision      uint64         `json:"case_label_revision"`
	ReviewRunID            string         `json:"review_run_id"`
	CandidateID            string         `json:"candidate_id"`
	ClusterFingerprint     string         `json:"cluster_fingerprint"`
	RepositoryID           string         `json:"repository_id"`
	DimensionID            string         `json:"dimension_id"`
	DimensionRevision      string         `json:"dimension_revision"`
	DimensionSHA256        string         `json:"dimension_sha256"`
	RawConfidencePPM       uint32         `json:"raw_confidence_ppm"`
	Truth                  Truth          `json:"truth"`
	Authority              LabelAuthority `json:"authority"`
}

type GatePolicy struct {
	MinimumTrainingSamples    uint32 `json:"minimum_training_samples"`
	MinimumValidationSamples  uint32 `json:"minimum_validation_samples"`
	MinimumSliceSamples       uint32 `json:"minimum_slice_samples"`
	MaximumValidationBrierPPM uint32 `json:"maximum_validation_brier_ppm"`
	MaximumValidationECEPPM   uint32 `json:"maximum_validation_ece_ppm"`
	MaximumSliceECEPPM        uint32 `json:"maximum_slice_ece_ppm"`
	MaximumLabelRateDriftPPM  uint32 `json:"maximum_label_rate_drift_ppm"`
	MaximumMeanRawDriftPPM    uint32 `json:"maximum_mean_raw_drift_ppm"`
}

type FitRequest struct {
	SchemaVersion   string        `json:"schema_version"`
	RunID           string        `json:"run_id"`
	ProfileID       string        `json:"profile_id"`
	ProfileRevision string        `json:"profile_revision"`
	Training        []Observation `json:"training"`
	Validation      []Observation `json:"validation"`
	GatePolicy      GatePolicy    `json:"gate_policy"`
	CreatedAt       time.Time     `json:"created_at"`
}

type ManifestSample struct {
	Observation
	DatasetSplit string `json:"dataset_split"`
	CloneGroupID string `json:"clone_group_id"`
}

type DatasetManifest struct {
	SchemaVersion string           `json:"schema_version"`
	ManifestID    string           `json:"manifest_id"`
	Samples       []ManifestSample `json:"samples"`
	SHA256        string           `json:"sha256"`
}

type Metric struct {
	ScopeKind         string `json:"scope_kind"`
	ScopeID           string `json:"scope_id"`
	Dataset           string `json:"dataset"`
	SampleCount       uint32 `json:"sample_count"`
	PositiveCount     uint32 `json:"positive_count"`
	PositiveRatePPM   uint32 `json:"positive_rate_ppm"`
	MeanRawPPM        uint32 `json:"mean_raw_ppm"`
	MeanCalibratedPPM uint32 `json:"mean_calibrated_ppm"`
	BrierScorePPM     uint32 `json:"brier_score_ppm"`
	ECEPPM            uint32 `json:"ece_ppm"`
}

type DriftMetric struct {
	ScopeKind         string `json:"scope_kind"`
	ScopeID           string `json:"scope_id"`
	TrainingSamples   uint32 `json:"training_samples"`
	ValidationSamples uint32 `json:"validation_samples"`
	LabelRateDriftPPM uint32 `json:"label_rate_drift_ppm"`
	MeanRawDriftPPM   uint32 `json:"mean_raw_drift_ppm"`
	Status            string `json:"status"`
}

type Report struct {
	SchemaVersion string        `json:"schema_version"`
	ReportID      string        `json:"report_id"`
	Passed        bool          `json:"passed"`
	ReasonCodes   []string      `json:"reason_codes"`
	Metrics       []Metric      `json:"metrics"`
	Drift         []DriftMetric `json:"drift"`
	SHA256        string        `json:"sha256"`
}

type ProfileCandidate struct {
	SchemaVersion         string                          `json:"schema_version"`
	CandidateID           string                          `json:"candidate_id"`
	Profile               reviewconfig.CalibrationProfile `json:"profile"`
	TrainingManifestRef   runmodel.ArtifactRef            `json:"training_manifest_ref"`
	ValidationManifestRef runmodel.ArtifactRef            `json:"validation_manifest_ref"`
	Status                string                          `json:"status"`
	AutoPublished         bool                            `json:"auto_published"`
	SHA256                string                          `json:"sha256"`
}

type Run struct {
	SchemaVersion         string               `json:"schema_version"`
	RunID                 string               `json:"run_id"`
	TrainingManifestRef   runmodel.ArtifactRef `json:"training_manifest_ref"`
	ValidationManifestRef runmodel.ArtifactRef `json:"validation_manifest_ref"`
	ProfileCandidate      ProfileCandidate     `json:"profile_candidate"`
	Report                Report               `json:"report"`
	GatePolicy            GatePolicy           `json:"gate_policy"`
	CreatedBy             string               `json:"created_by"`
	CreatedAt             time.Time            `json:"created_at"`
	SHA256                string               `json:"sha256"`
}

type runEvent struct {
	RunID      string               `json:"run_id"`
	RunRef     runmodel.ArtifactRef `json:"run_ref"`
	Actor      string               `json:"actor"`
	Audit      string               `json:"audit"`
	OccurredAt time.Time            `json:"occurred_at"`
}
