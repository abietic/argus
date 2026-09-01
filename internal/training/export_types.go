package training

import (
	"errors"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/runmodel"
)

const (
	ExportRequestSchemaVersion = "argus.training_export_request.v1alpha1"
	ExportBundleSchemaVersion  = "argus.training_export_bundle.v1alpha1"
	ExportLedgerSchemaVersion  = "argus.training_export_ledger_event.v1alpha1"
	PortableRecordsFormat      = "argus_training_records_jsonl_v1alpha1"
	exportLedgerStream         = "training/exports"
	maximumExportSourceBytes   = int64(16 << 20)
)

var (
	ErrExportNotFound = errors.New("training export not found")
	ErrExportConflict = errors.New("training export conflict")
	ErrExportCorrupt  = errors.New("corrupt training export repository")
)

type ExportRequest struct {
	SchemaVersion string                `json:"schema_version"`
	ExportID      string                `json:"export_id"`
	ManifestID    string                `json:"manifest_id"`
	ManifestRef   runmodel.ArtifactRef  `json:"manifest_ref"`
	Policy        StrictRedactionPolicy `json:"policy"`
	CreatedAt     time.Time             `json:"created_at"`
}

type RedactionReceipt struct {
	SourceRef       runmodel.ArtifactRef `json:"source_ref"`
	RedactedRef     runmodel.ArtifactRef `json:"redacted_ref"`
	PolicySHA256    string               `json:"policy_sha256"`
	Encoding        string               `json:"encoding"`
	InputSizeBytes  int64                `json:"input_size_bytes"`
	OutputSizeBytes int64                `json:"output_size_bytes"`
	Hits            []RedactionHit       `json:"hits"`
}

type ExportSample struct {
	GovernedSample ManifestSample         `json:"governed_sample"`
	RedactedRefs   []runmodel.ArtifactRef `json:"redacted_refs"`
}

type ExportBundle struct {
	SchemaVersion                 string                `json:"schema_version"`
	ExportID                      string                `json:"export_id"`
	DatasetID                     string                `json:"dataset_id"`
	DatasetRevision               string                `json:"dataset_revision"`
	RepositoryID                  string                `json:"repository_id"`
	ManifestID                    string                `json:"manifest_id"`
	ManifestRef                   runmodel.ArtifactRef  `json:"manifest_ref"`
	Policy                        StrictRedactionPolicy `json:"policy"`
	PortableFormat                string                `json:"portable_format"`
	ContainsUnredactedSourceBytes bool                  `json:"contains_unredacted_source_bytes"`
	Receipts                      []RedactionReceipt    `json:"receipts"`
	Samples                       []ExportSample        `json:"samples"`
	CreatedBy                     string                `json:"created_by"`
	CreatedAt                     time.Time             `json:"created_at"`
	SHA256                        string                `json:"sha256"`
}

type ExportRecord struct {
	Request   ExportRequest        `json:"request"`
	BundleRef runmodel.ArtifactRef `json:"bundle_ref"`
	Bundle    ExportBundle         `json:"bundle"`
	Mutation  evaluation.Mutation  `json:"mutation"`
}

type exportLedgerEvent struct {
	SchemaVersion string               `json:"schema_version"`
	Request       ExportRequest        `json:"request"`
	BundleRef     runmodel.ArtifactRef `json:"bundle_ref"`
	Mutation      evaluation.Mutation  `json:"mutation"`
}
