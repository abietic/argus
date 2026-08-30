// Package training owns the governed, reference-only handoff from approved
// EvaluationCases to downstream training systems. It never copies source bytes
// and never treats Argus output as its own gold label.
package training

import (
	"errors"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
)

const (
	MaterializationRequestSchemaVersion = "argus.training_materialization_request.v1alpha1"
	DatasetManifestSchemaVersion        = "argus.training_dataset_manifest.v1alpha1"
	LedgerEventSchemaVersion            = "argus.training_dataset_ledger_event.v1alpha1"

	ContentModeReferenceOnly      = "reference_only_strict_redaction_required"
	IndependentAuthorityStatement = "independently_adjudicated_not_argus_output"

	ledgerStream = "training/datasets"
)

var (
	ErrNotFound     = errors.New("training dataset manifest not found")
	ErrConflict     = errors.New("training materialization conflict")
	ErrUnauthorized = errors.New("training materialization unauthorized")
	ErrContaminated = errors.New("training dataset is contaminated")
	ErrPolicyDenied = errors.New("training data policy denied")
	ErrCorrupt      = errors.New("corrupt training dataset repository")
)

type AuthorityKind string

const (
	AuthorityHumanAdjudication  AuthorityKind = "human_adjudication"
	AuthorityExternalGovernance AuthorityKind = "external_governance"
)

type LabelAuthority struct {
	Kind          AuthorityKind `json:"kind"`
	ReviewerIDs   []string      `json:"reviewer_ids"`
	AdjudicatorID string        `json:"adjudicator_id"`
	EvidenceRefs  []string      `json:"evidence_refs"`
	Statement     string        `json:"statement"`
}

type CaseBinding struct {
	CaseID                     string                 `json:"case_id"`
	ExpectedGovernanceRevision uint64                 `json:"expected_governance_revision"`
	ExpectedLabelRevision      uint64                 `json:"expected_label_revision"`
	ArtifactRefs               []runmodel.ArtifactRef `json:"artifact_refs"`
	Authority                  LabelAuthority         `json:"authority"`
}

type MaterializationRequest struct {
	SchemaVersion   string               `json:"schema_version"`
	DatasetID       string               `json:"dataset_id"`
	DatasetRevision string               `json:"dataset_revision"`
	RepositoryID    string               `json:"repository_id"`
	ConfigBundleRef runmodel.ArtifactRef `json:"config_bundle_ref"`
	Cases           []CaseBinding        `json:"cases"`
	CreatedAt       time.Time            `json:"created_at"`
}

type ManifestSample struct {
	CaseID                 string                      `json:"case_id"`
	CaseGovernanceRevision uint64                      `json:"case_governance_revision"`
	CaseLabelRevision      uint64                      `json:"case_label_revision"`
	CaseType               evaluation.CaseType         `json:"case_type"`
	Provenance             evaluation.SourceProvenance `json:"provenance"`
	LicenseConsent         evaluation.LicenseConsent   `json:"license_consent"`
	Classification         evaluation.Classification   `json:"classification"`
	Owner                  string                      `json:"owner"`
	InputSnapshotRef       string                      `json:"input_snapshot_ref"`
	Label                  evaluation.Label            `json:"label"`
	LabelPolicyRevision    string                      `json:"label_policy_revision"`
	ReviewState            evaluation.ReviewState      `json:"review_state"`
	DatasetState           evaluation.DatasetState     `json:"dataset_state"`
	DatasetSplit           evaluation.Split            `json:"dataset_split"`
	CloneGroupID           string                      `json:"clone_group_id"`
	Eligibility            evaluation.Eligibility      `json:"eligibility"`
	CaseCreatedAt          time.Time                   `json:"case_created_at"`
	ArtifactRefs           []runmodel.ArtifactRef      `json:"artifact_refs"`
	Authority              LabelAuthority              `json:"authority"`
}

type DatasetManifest struct {
	SchemaVersion       string                         `json:"schema_version"`
	ManifestID          string                         `json:"manifest_id"`
	DatasetID           string                         `json:"dataset_id"`
	DatasetRevision     string                         `json:"dataset_revision"`
	RepositoryID        string                         `json:"repository_id"`
	ConfigContext       reviewconfig.ResolutionContext `json:"config_context"`
	ConfigBundleRef     runmodel.ArtifactRef           `json:"config_bundle_ref"`
	DataPolicy          reviewconfig.DataPolicy        `json:"data_policy"`
	ContentMode         string                         `json:"content_mode"`
	ContainsSourceBytes bool                           `json:"contains_source_bytes"`
	SelfLabelsAllowed   bool                           `json:"self_labels_allowed"`
	Samples             []ManifestSample               `json:"samples"`
	CreatedBy           string                         `json:"created_by"`
	CreatedAt           time.Time                      `json:"created_at"`
	SHA256              string                         `json:"sha256"`
}

type Record struct {
	Request     MaterializationRequest `json:"request"`
	ManifestRef runmodel.ArtifactRef   `json:"manifest_ref"`
	Manifest    DatasetManifest        `json:"manifest"`
	Mutation    evaluation.Mutation    `json:"mutation"`
}

type ledgerEvent struct {
	SchemaVersion string                 `json:"schema_version"`
	Request       MaterializationRequest `json:"request"`
	ManifestRef   runmodel.ArtifactRef   `json:"manifest_ref"`
	ManifestID    string                 `json:"manifest_id"`
	Mutation      evaluation.Mutation    `json:"mutation"`
}
