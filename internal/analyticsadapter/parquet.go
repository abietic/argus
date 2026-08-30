package analyticsadapter

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/abietic/argus/internal/analytics"
	"github.com/parquet-go/parquet-go"
)

const parquetMediaType = "application/vnd.apache.parquet"
const maxParquetFooterBytes = 16 << 20
const maxParquetFileBytes = 256 << 20

type reviewRunParquetRow struct {
	SchemaVersion         string `parquet:"schema_version"`
	FactID                string `parquet:"fact_id"`
	RunID                 string `parquet:"run_id"`
	TenantID              string `parquet:"tenant_id"`
	OrganizationID        string `parquet:"organization_id"`
	RepositoryID          string `parquet:"repository_id"`
	Language              string `parquet:"language"`
	RuleID                string `parquet:"rule_id"`
	Path                  string `parquet:"path"`
	WorkflowRevision      string `parquet:"workflow_revision"`
	ConfigRevision        string `parquet:"config_revision"`
	ReviewDimension       string `parquet:"review_dimension"`
	Status                string `parquet:"status"`
	ResultCompleteness    string `parquet:"result_completeness"`
	IncompleteReasonsJSON string `parquet:"incomplete_reasons_json"`
	FunnelPresent         bool   `parquet:"funnel_present"`
	Candidates            uint64 `parquet:"candidates"`
	Normalized            uint64 `parquet:"normalized"`
	Verified              uint64 `parquet:"verified"`
	Published             uint64 `parquet:"published"`
	StartedAtMicros       int64  `parquet:"started_at_micros"`
	FinishedAtPresent     bool   `parquet:"finished_at_present"`
	FinishedAtMicros      int64  `parquet:"finished_at_micros"`
	OccurredAtMicros      int64  `parquet:"occurred_at_micros"`
}

type stageParquetRow struct {
	SchemaVersion         string `parquet:"schema_version"`
	FactID                string `parquet:"fact_id"`
	RunID                 string `parquet:"run_id"`
	StageID               string `parquet:"stage_id"`
	StageRevision         string `parquet:"stage_revision"`
	Attempt               uint32 `parquet:"attempt"`
	TenantID              string `parquet:"tenant_id"`
	OrganizationID        string `parquet:"organization_id"`
	RepositoryID          string `parquet:"repository_id"`
	Language              string `parquet:"language"`
	RuleID                string `parquet:"rule_id"`
	Path                  string `parquet:"path"`
	WorkflowRevision      string `parquet:"workflow_revision"`
	ConfigRevision        string `parquet:"config_revision"`
	ReviewDimension       string `parquet:"review_dimension"`
	Status                string `parquet:"status"`
	ResultCompleteness    string `parquet:"result_completeness"`
	IncompleteReasonsJSON string `parquet:"incomplete_reasons_json"`
	DurationMicros        uint64 `parquet:"duration_micros"`
	OccurredAtMicros      int64  `parquet:"occurred_at_micros"`
}

type contextProviderParquetRow struct {
	SchemaVersion         string `parquet:"schema_version"`
	FactID                string `parquet:"fact_id"`
	RunID                 string `parquet:"run_id"`
	ReceiptID             string `parquet:"receipt_id"`
	ReceiptSHA256         string `parquet:"receipt_sha256"`
	ReceiptArtifactSHA256 string `parquet:"receipt_artifact_sha256"`
	ProviderID            string `parquet:"provider_id"`
	ProviderRevision      string `parquet:"provider_revision"`
	Kind                  string `parquet:"kind"`
	AdapterID             string `parquet:"adapter_id"`
	AdapterRevision       string `parquet:"adapter_revision"`
	AdapterSHA256         string `parquet:"adapter_sha256"`
	RequestSHA256         string `parquet:"request_sha256"`
	BindingMode           string `parquet:"binding_mode"`
	Status                string `parquet:"status"`
	ReasonCode            string `parquet:"reason_code"`
	ContextID             string `parquet:"context_id"`
	ContextDigest         string `parquet:"context_digest"`
	ContextContract       string `parquet:"context_contract"`
	TargetPathCount       uint64 `parquet:"target_path_count"`
	TimeoutMicros         uint64 `parquet:"timeout_micros"`
	DurationMicros        uint64 `parquet:"duration_micros"`
	Authority             string `parquet:"authority"`
	TenantID              string `parquet:"tenant_id"`
	OrganizationID        string `parquet:"organization_id"`
	RepositoryID          string `parquet:"repository_id"`
	Language              string `parquet:"language"`
	RuleID                string `parquet:"rule_id"`
	Path                  string `parquet:"path"`
	WorkflowRevision      string `parquet:"workflow_revision"`
	ConfigRevision        string `parquet:"config_revision"`
	ReviewDimension       string `parquet:"review_dimension"`
	OccurredAtMicros      int64  `parquet:"occurred_at_micros"`
}

type findingParquetRow struct {
	SchemaVersion          string `parquet:"schema_version"`
	FactID                 string `parquet:"fact_id"`
	RunID                  string `parquet:"run_id"`
	CandidateID            string `parquet:"candidate_id"`
	FindingID              string `parquet:"finding_id"`
	Normalized             bool   `parquet:"normalized"`
	Verification           string `parquet:"verification"`
	PublicationEligibility string `parquet:"publication_eligibility"`
	Publication            string `parquet:"publication"`
	PublicationRefPresent  bool   `parquet:"publication_ref_present"`
	PublicationRefKind     string `parquet:"publication_ref_kind"`
	PublicationRefID       string `parquet:"publication_ref_id"`
	PublicationRefRevision string `parquet:"publication_ref_revision"`
	PublicationRefSHA256   string `parquet:"publication_ref_sha256"`
	PublicationAtPresent   bool   `parquet:"publication_at_present"`
	PublicationAtMicros    int64  `parquet:"publication_at_micros"`
	ResultCompleteness     string `parquet:"result_completeness"`
	IncompleteReasonsJSON  string `parquet:"incomplete_reasons_json"`
	TenantID               string `parquet:"tenant_id"`
	OrganizationID         string `parquet:"organization_id"`
	RepositoryID           string `parquet:"repository_id"`
	Language               string `parquet:"language"`
	RuleID                 string `parquet:"rule_id"`
	Path                   string `parquet:"path"`
	WorkflowRevision       string `parquet:"workflow_revision"`
	ConfigRevision         string `parquet:"config_revision"`
	ReviewDimension        string `parquet:"review_dimension"`
	OccurredAtMicros       int64  `parquet:"occurred_at_micros"`
}

type feedbackOutcomeParquetRow struct {
	SchemaVersion         string `parquet:"schema_version"`
	FactID                string `parquet:"fact_id"`
	RunID                 string `parquet:"run_id"`
	FindingID             string `parquet:"finding_id"`
	Feedback              string `parquet:"feedback"`
	FeedbackRefPresent    bool   `parquet:"feedback_ref_present"`
	FeedbackRefKind       string `parquet:"feedback_ref_kind"`
	FeedbackRefID         string `parquet:"feedback_ref_id"`
	FeedbackRefRevision   string `parquet:"feedback_ref_revision"`
	FeedbackRefSHA256     string `parquet:"feedback_ref_sha256"`
	FeedbackAtPresent     bool   `parquet:"feedback_at_present"`
	FeedbackAtMicros      int64  `parquet:"feedback_at_micros"`
	Outcome               string `parquet:"outcome"`
	OutcomeRefPresent     bool   `parquet:"outcome_ref_present"`
	OutcomeRefKind        string `parquet:"outcome_ref_kind"`
	OutcomeRefID          string `parquet:"outcome_ref_id"`
	OutcomeRefRevision    string `parquet:"outcome_ref_revision"`
	OutcomeRefSHA256      string `parquet:"outcome_ref_sha256"`
	OutcomeAtPresent      bool   `parquet:"outcome_at_present"`
	OutcomeAtMicros       int64  `parquet:"outcome_at_micros"`
	ResultCompleteness    string `parquet:"result_completeness"`
	IncompleteReasonsJSON string `parquet:"incomplete_reasons_json"`
	TenantID              string `parquet:"tenant_id"`
	OrganizationID        string `parquet:"organization_id"`
	RepositoryID          string `parquet:"repository_id"`
	Language              string `parquet:"language"`
	RuleID                string `parquet:"rule_id"`
	Path                  string `parquet:"path"`
	WorkflowRevision      string `parquet:"workflow_revision"`
	ConfigRevision        string `parquet:"config_revision"`
	ReviewDimension       string `parquet:"review_dimension"`
	OccurredAtMicros      int64  `parquet:"occurred_at_micros"`
}

type findingLineageParquetRow struct {
	SchemaVersion          string `parquet:"schema_version"`
	FactID                 string `parquet:"fact_id"`
	LineageID              string `parquet:"lineage_id"`
	LineageArtifactSHA256  string `parquet:"lineage_artifact_sha256"`
	RelationID             string `parquet:"relation_id"`
	RelationType           string `parquet:"relation_type"`
	RelationMethod         string `parquet:"relation_method"`
	ReasonCode             string `parquet:"reason_code"`
	FamilyKey              string `parquet:"family_key"`
	PolicyID               string `parquet:"policy_id"`
	PolicyRevision         string `parquet:"policy_revision"`
	PolicySHA256           string `parquet:"policy_sha256"`
	AncestryAuthority      string `parquet:"ancestry_authority"`
	AncestryEvidenceSHA256 string `parquet:"ancestry_evidence_sha256"`
	RenameMappingCount     uint32 `parquet:"rename_mapping_count"`
	BaselineRunID          string `parquet:"baseline_run_id"`
	VariantRunID           string `parquet:"variant_run_id"`
	BaselineFindingIDsJSON string `parquet:"baseline_finding_ids_json"`
	VariantFindingIDsJSON  string `parquet:"variant_finding_ids_json"`
	TenantID               string `parquet:"tenant_id"`
	OrganizationID         string `parquet:"organization_id"`
	WorkspaceID            string `parquet:"workspace_id"`
	RepositoryID           string `parquet:"repository_id"`
	OccurredAtMicros       int64  `parquet:"occurred_at_micros"`
}

type experimentParquetRow struct {
	SchemaVersion         string `parquet:"schema_version"`
	FactID                string `parquet:"fact_id"`
	ExperimentID          string `parquet:"experiment_id"`
	ExperimentRevision    string `parquet:"experiment_revision"`
	Arm                   string `parquet:"arm"`
	VariantID             string `parquet:"variant_id"`
	MetricID              string `parquet:"metric_id"`
	MetricVersion         string `parquet:"metric_version"`
	MetricDefinition      string `parquet:"metric_definition"`
	Direction             string `parquet:"direction"`
	ValueAmount           int64  `parquet:"value_amount"`
	ValueScale            uint32 `parquet:"value_scale"`
	ValueUnit             string `parquet:"value_unit"`
	SampleSize            uint64 `parquet:"sample_size"`
	WindowStartMicros     int64  `parquet:"window_start_micros"`
	WindowEndMicros       int64  `parquet:"window_end_micros"`
	ResultCompleteness    string `parquet:"result_completeness"`
	IncompleteReasonsJSON string `parquet:"incomplete_reasons_json"`
	TenantID              string `parquet:"tenant_id"`
	OrganizationID        string `parquet:"organization_id"`
	RepositoryID          string `parquet:"repository_id"`
	Language              string `parquet:"language"`
	RuleID                string `parquet:"rule_id"`
	Path                  string `parquet:"path"`
	WorkflowRevision      string `parquet:"workflow_revision"`
	ConfigRevision        string `parquet:"config_revision"`
	ReviewDimension       string `parquet:"review_dimension"`
	SourceRefsJSON        string `parquet:"source_refs_json"`
	OccurredAtMicros      int64  `parquet:"occurred_at_micros"`
}

type repeatabilityParquetRow struct {
	SchemaVersion         string `parquet:"schema_version"`
	FactID                string `parquet:"fact_id"`
	RepeatabilityRunID    string `parquet:"repeatability_run_id"`
	RepeatabilityRevision string `parquet:"repeatability_revision"`
	CaseID                string `parquet:"case_id"`
	MetricID              string `parquet:"metric_id"`
	MetricVersion         string `parquet:"metric_version"`
	MetricDefinition      string `parquet:"metric_definition"`
	Direction             string `parquet:"direction"`
	ValueAmount           int64  `parquet:"value_amount"`
	ValueScale            uint32 `parquet:"value_scale"`
	ValueUnit             string `parquet:"value_unit"`
	SampleSize            uint64 `parquet:"sample_size"`
	WindowStartMicros     int64  `parquet:"window_start_micros"`
	WindowEndMicros       int64  `parquet:"window_end_micros"`
	ResultCompleteness    string `parquet:"result_completeness"`
	IncompleteReasonsJSON string `parquet:"incomplete_reasons_json"`
	TenantID              string `parquet:"tenant_id"`
	OrganizationID        string `parquet:"organization_id"`
	RepositoryID          string `parquet:"repository_id"`
	Language              string `parquet:"language"`
	RuleID                string `parquet:"rule_id"`
	Path                  string `parquet:"path"`
	WorkflowRevision      string `parquet:"workflow_revision"`
	ConfigRevision        string `parquet:"config_revision"`
	ReviewDimension       string `parquet:"review_dimension"`
	SourceRefsJSON        string `parquet:"source_refs_json"`
	OccurredAtMicros      int64  `parquet:"occurred_at_micros"`
}

type valueObservationParquetRow struct {
	SchemaVersion                    string `parquet:"schema_version"`
	ObservationID                    string `parquet:"observation_id"`
	TenantID                         string `parquet:"tenant_id"`
	OrganizationID                   string `parquet:"organization_id"`
	RepositoryID                     string `parquet:"repository_id"`
	Language                         string `parquet:"language"`
	RuleID                           string `parquet:"rule_id"`
	Path                             string `parquet:"path"`
	WorkflowRevision                 string `parquet:"workflow_revision"`
	ConfigRevision                   string `parquet:"config_revision"`
	ReviewDimension                  string `parquet:"review_dimension"`
	MetricID                         string `parquet:"metric_id"`
	EvidenceTier                     string `parquet:"evidence_tier"`
	BaselineCohortID                 string `parquet:"baseline_cohort_id"`
	BaselineRevision                 string `parquet:"baseline_revision"`
	BaselineWindowStartMicros        int64  `parquet:"baseline_window_start_micros"`
	BaselineWindowEndMicros          int64  `parquet:"baseline_window_end_micros"`
	BaselineValueAmount              int64  `parquet:"baseline_value_amount"`
	BaselineValueScale               uint32 `parquet:"baseline_value_scale"`
	BaselineValueUnit                string `parquet:"baseline_value_unit"`
	BaselineSampleSize               uint64 `parquet:"baseline_sample_size"`
	BaselineSourceRefsJSON           string `parquet:"baseline_source_refs_json"`
	ObservationWindowStartMicros     int64  `parquet:"observation_window_start_micros"`
	ObservationWindowEndMicros       int64  `parquet:"observation_window_end_micros"`
	AttributionWindowStartMicros     int64  `parquet:"attribution_window_start_micros"`
	AttributionWindowEndMicros       int64  `parquet:"attribution_window_end_micros"`
	SampleSize                       uint64 `parquet:"sample_size"`
	GrossValueAmount                 int64  `parquet:"gross_value_amount"`
	GrossValueScale                  uint32 `parquet:"gross_value_scale"`
	GrossValueUnit                   string `parquet:"gross_value_unit"`
	ModelComputeAmount               int64  `parquet:"model_compute_amount"`
	ModelComputeScale                uint32 `parquet:"model_compute_scale"`
	ModelComputeUnit                 string `parquet:"model_compute_unit"`
	HumanVerificationAmount          int64  `parquet:"human_verification_amount"`
	HumanVerificationScale           uint32 `parquet:"human_verification_scale"`
	HumanVerificationUnit            string `parquet:"human_verification_unit"`
	FalsePositiveInterruptionAmount  int64  `parquet:"false_positive_interruption_amount"`
	FalsePositiveInterruptionScale   uint32 `parquet:"false_positive_interruption_scale"`
	FalsePositiveInterruptionUnit    string `parquet:"false_positive_interruption_unit"`
	PlatformOperationsAmount         int64  `parquet:"platform_operations_amount"`
	PlatformOperationsScale          uint32 `parquet:"platform_operations_scale"`
	PlatformOperationsUnit           string `parquet:"platform_operations_unit"`
	StorageAmount                    int64  `parquet:"storage_amount"`
	StorageScale                     uint32 `parquet:"storage_scale"`
	StorageUnit                      string `parquet:"storage_unit"`
	NetValueAmount                   int64  `parquet:"net_value_amount"`
	NetValueScale                    uint32 `parquet:"net_value_scale"`
	NetValueUnit                     string `parquet:"net_value_unit"`
	NetValueUncertaintyLowerAmount   int64  `parquet:"net_value_uncertainty_lower_amount"`
	NetValueUncertaintyLowerScale    uint32 `parquet:"net_value_uncertainty_lower_scale"`
	NetValueUncertaintyLowerUnit     string `parquet:"net_value_uncertainty_lower_unit"`
	NetValueUncertaintyUpperAmount   int64  `parquet:"net_value_uncertainty_upper_amount"`
	NetValueUncertaintyUpperScale    uint32 `parquet:"net_value_uncertainty_upper_scale"`
	NetValueUncertaintyUpperUnit     string `parquet:"net_value_uncertainty_upper_unit"`
	NetValueUncertaintyConfidenceBPS uint32 `parquet:"net_value_uncertainty_confidence_bps"`
	NetValueUncertaintyMethod        string `parquet:"net_value_uncertainty_method"`
	NetValueUncertaintyRevision      string `parquet:"net_value_uncertainty_revision"`
	SourceRefsJSON                   string `parquet:"source_refs_json"`
	AttributionPolicyRevision        string `parquet:"attribution_policy_revision"`
	ObservedAtMicros                 int64  `parquet:"observed_at_micros"`
}

type dimensionValues struct {
	tenantID         string
	organizationID   string
	repositoryID     string
	language         string
	ruleID           string
	path             string
	workflowRevision string
	configRevision   string
	reviewDimension  string
}

type sourceRefValues struct {
	present  bool
	kind     string
	id       string
	revision string
	sha256   string
}

// ExportFactSetParquet emits one flat, strongly typed Parquet table per FactSet
// collection. parquet-go remains behind this adapter boundary.
func ExportFactSetParquet(facts analytics.FactSet) (analytics.ExportBundle, error) {
	canonical, err := analytics.CanonicalFactSet(facts)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	tempDirectory, err := os.MkdirTemp("", "argus-analytics-parquet-*")
	if err != nil {
		return analytics.ExportBundle{}, fmt.Errorf("create Parquet staging directory: %w", err)
	}
	defer os.RemoveAll(tempDirectory)

	files := make([]analytics.ExportFile, 0, 9)
	reviewRuns, err := reviewRunParquetRows(canonical.ReviewRuns)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"review_runs.parquet",
		analytics.ReviewRunParquetSchemaVersion,
		reviewRuns,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	repeatability, err := repeatabilityParquetRows(canonical.Repeatability)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"repeatability.parquet",
		analytics.RepeatabilityParquetSchemaVersion,
		repeatability,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	contextProviders, err := contextProviderParquetRows(canonical.ContextProviders)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"context_providers.parquet",
		analytics.ContextProviderParquetSchemaVersion,
		contextProviders,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	stages, err := stageParquetRows(canonical.Stages)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"stages.parquet",
		analytics.StageParquetSchemaVersion,
		stages,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	findings, err := findingParquetRows(canonical.Findings)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"finding_funnel.parquet",
		analytics.FindingFunnelParquetSchemaVersion,
		findings,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	feedbackOutcomes, err := feedbackOutcomeParquetRows(canonical.FeedbackOutcomes)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"feedback_outcomes.parquet",
		analytics.FeedbackOutcomeParquetSchemaVersion,
		feedbackOutcomes,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	lineages, err := findingLineageParquetRows(canonical.FindingLineages)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory, "finding_lineages.parquet", analytics.FindingLineageParquetSchemaVersion, lineages,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	experiments, err := experimentParquetRows(canonical.Experiments)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"experiments.parquet",
		analytics.ExperimentParquetSchemaVersion,
		experiments,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}
	valueObservations, err := valueObservationParquetRows(canonical.ValueObservations)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	if file, err := writeVerifiedParquet(
		tempDirectory,
		"value_observations.parquet",
		analytics.ValueObservationParquetSchemaVersion,
		valueObservations,
	); err != nil {
		return analytics.ExportBundle{}, err
	} else {
		files = append(files, file)
	}

	slices.SortFunc(files, func(left, right analytics.ExportFile) int {
		return bytes.Compare(
			[]byte(left.Manifest.Name),
			[]byte(right.Manifest.Name),
		)
	})
	manifests := make([]analytics.ExportFileManifest, len(files))
	for index := range files {
		manifests[index] = files[index].Manifest
	}
	count, err := analytics.FactCount(canonical)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	bundle := analytics.ExportBundle{
		Manifest: analytics.ExportManifest{
			SchemaVersion:        analytics.ExportManifestSchemaVersion,
			DatasetSchemaVersion: analytics.FactSetSchemaVersion,
			Format:               analytics.CanonicalParquetExportFormat,
			Window:               canonical.Window,
			FactCount:            count,
			Parquet:              true,
			Limitations:          []string{},
			Files:                manifests,
		},
		Files: files,
	}
	if err := bundle.Validate(); err != nil {
		return analytics.ExportBundle{}, fmt.Errorf("validate Parquet export bundle: %w", err)
	}
	return bundle, nil
}

func writeVerifiedParquet[T any](
	directory string,
	name string,
	contract string,
	rows []T,
) (analytics.ExportFile, error) {
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return analytics.ExportFile{}, fmt.Errorf("create Parquet table %q: %w", name, err)
	}
	writer, err := newParquetWriter[T](file)
	if err != nil {
		_ = file.Close()
		return analytics.ExportFile{}, fmt.Errorf(
			"initialize Parquet table %q: %w",
			name,
			err,
		)
	}
	written, writeErr := writer.Write(rows)
	writerCloseErr := writer.Close()
	var syncErr error
	if writeErr == nil && writerCloseErr == nil {
		syncErr = file.Sync()
	}
	fileCloseErr := file.Close()
	if err := errors.Join(writeErr, writerCloseErr, syncErr, fileCloseErr); err != nil {
		return analytics.ExportFile{}, fmt.Errorf("write Parquet table %q: %w", name, err)
	}
	if written != len(rows) {
		return analytics.ExportFile{}, fmt.Errorf(
			"write Parquet table %q: wrote %d rows, want %d",
			name,
			written,
			len(rows),
		)
	}
	data, err := verifyParquetFile(path, rows)
	if err != nil {
		return analytics.ExportFile{}, fmt.Errorf("verify Parquet table %q: %w", name, err)
	}
	manifest := analytics.ExportFileManifest{
		Name:      name,
		MediaType: parquetMediaType,
		Contract:  contract,
		Ref:       name,
		RowCount:  uint64(len(rows)),
		SizeBytes: int64(len(data)),
		SHA256:    digestParquetBytes(data),
	}
	return analytics.ExportFile{
		Manifest: manifest,
		Data:     data,
	}, nil
}

// verifyParquetFile is restricted to files just written in the private
// process-owned staging directory. It is not a general untrusted-file reader.
// The size budget and panic boundary still turn bounded corruption into an
// ordinary error. parquet.Read/NewGenericReader are deliberately not used.
func verifyParquetFile[T any](
	path string,
	expected []T,
) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			data = nil
			err = fmt.Errorf("Parquet verification panicked: %v", recovered)
		}
	}()
	source, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open staged table: %w", err)
	}
	info, err := source.Stat()
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("stat staged table: %w", err)
	}
	if info.Size() < 0 || info.Size() > maxParquetFileBytes {
		_ = source.Close()
		return nil, fmt.Errorf(
			"staged table size %d exceeds %d-byte verification budget",
			info.Size(),
			maxParquetFileBytes,
		)
	}
	data, readErr := io.ReadAll(io.LimitReader(source, maxParquetFileBytes+1))
	closeErr := source.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read staged table: %w", err)
	}
	if int64(len(data)) != info.Size() {
		return nil, fmt.Errorf(
			"staged table size changed while reading: got %d, want %d",
			len(data),
			info.Size(),
		)
	}
	if err := preflightParquetFile(data); err != nil {
		return nil, err
	}
	opened, err := parquet.OpenFile(
		bytes.NewReader(data),
		int64(len(data)),
		parquet.SkipPageIndex(true),
		parquet.SkipBloomFilters(true),
	)
	if err != nil {
		return nil, fmt.Errorf("open Parquet file: %w", err)
	}
	expectedSchema := parquet.SchemaOf(new(T))
	if !parquet.EqualNodes(opened.Schema(), expectedSchema) {
		return nil, fmt.Errorf("Parquet schema does not match the table contract")
	}
	if opened.NumRows() != int64(len(expected)) {
		return nil, fmt.Errorf(
			"Parquet row count is %d, want %d",
			opened.NumRows(),
			len(expected),
		)
	}
	declaredRows := int64(0)
	for groupIndex, group := range opened.RowGroups() {
		groupRows := group.NumRows()
		if groupRows < 0 || groupRows > int64(len(expected))-declaredRows {
			return nil, fmt.Errorf(
				"Parquet row group %d has invalid row count %d",
				groupIndex,
				groupRows,
			)
		}
		declaredRows += groupRows
	}
	if declaredRows != opened.NumRows() {
		return nil, fmt.Errorf(
			"Parquet row groups declare %d rows, file declares %d",
			declaredRows,
			opened.NumRows(),
		)
	}
	expectedRows := make([]parquet.Row, len(expected))
	for index := range expected {
		expectedRows[index] = expectedSchema.Deconstruct(nil, &expected[index])
	}
	actualIndex := 0
	buffer := make([]parquet.Row, 128)
	for groupIndex, group := range opened.RowGroups() {
		rows := group.Rows()
		groupCount := int64(0)
		for {
			count, readErr := rows.ReadRows(buffer)
			for index := 0; index < count; index++ {
				if actualIndex >= len(expectedRows) ||
					!buffer[index].Equal(expectedRows[actualIndex]) {
					_ = rows.Close()
					return nil, fmt.Errorf(
						"Parquet row %d does not round-trip",
						actualIndex,
					)
				}
				actualIndex++
				groupCount++
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				_ = rows.Close()
				return nil, fmt.Errorf(
					"read Parquet row group %d: %w",
					groupIndex,
					readErr,
				)
			}
			if count == 0 {
				_ = rows.Close()
				return nil, fmt.Errorf(
					"read Parquet row group %d made no progress",
					groupIndex,
				)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close Parquet row group %d: %w", groupIndex, err)
		}
		if groupCount != group.NumRows() {
			return nil, fmt.Errorf(
				"Parquet row group %d read %d rows, want %d",
				groupIndex,
				groupCount,
				group.NumRows(),
			)
		}
	}
	if actualIndex != len(expectedRows) {
		return nil, fmt.Errorf(
			"Parquet full read returned %d rows, want %d",
			actualIndex,
			len(expectedRows),
		)
	}
	return data, nil
}

func reviewRunParquetRows(
	facts []analytics.ReviewRunFact,
) ([]reviewRunParquetRow, error) {
	rows := make([]reviewRunParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		reasons, err := marshalFlatJSON(fact.IncompleteReasons)
		if err != nil {
			return nil, fmt.Errorf("review_runs[%d] incomplete reasons: %w", index, err)
		}
		row := reviewRunParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID, RunID: fact.RunID,
			TenantID: dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision,
			ConfigRevision:   dimensions.configRevision,
			ReviewDimension:  dimensions.reviewDimension,
			Status:           string(fact.Status), ResultCompleteness: string(fact.ResultCompleteness),
			IncompleteReasonsJSON: reasons,
			StartedAtMicros:       fact.StartedAt.UnixMicro(),
			OccurredAtMicros:      fact.OccurredAt.UnixMicro(),
		}
		if fact.Funnel != nil {
			row.FunnelPresent = true
			row.Candidates = fact.Funnel.Candidates
			row.Normalized = fact.Funnel.Normalized
			row.Verified = fact.Funnel.Verified
			row.Published = fact.Funnel.Published
		}
		if fact.FinishedAt != nil {
			row.FinishedAtPresent = true
			row.FinishedAtMicros = fact.FinishedAt.UnixMicro()
		}
		rows[index] = row
	}
	return rows, nil
}

func stageParquetRows(facts []analytics.StageFact) ([]stageParquetRow, error) {
	rows := make([]stageParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		reasons, err := marshalFlatJSON(fact.IncompleteReasons)
		if err != nil {
			return nil, fmt.Errorf("stages[%d] incomplete reasons: %w", index, err)
		}
		rows[index] = stageParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID, RunID: fact.RunID,
			StageID: fact.StageID, StageRevision: fact.StageRevision, Attempt: fact.Attempt,
			TenantID: dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision,
			ConfigRevision:   dimensions.configRevision,
			ReviewDimension:  dimensions.reviewDimension,
			Status:           string(fact.Status), ResultCompleteness: string(fact.ResultCompleteness),
			IncompleteReasonsJSON: reasons, DurationMicros: fact.DurationMicros,
			OccurredAtMicros: fact.OccurredAt.UnixMicro(),
		}
	}
	return rows, nil
}

func contextProviderParquetRows(
	facts []analytics.ContextProviderFact,
) ([]contextProviderParquetRow, error) {
	rows := make([]contextProviderParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		rows[index] = contextProviderParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID, RunID: fact.RunID,
			ReceiptID: fact.ReceiptID, ReceiptSHA256: fact.ReceiptSHA256,
			ReceiptArtifactSHA256: fact.ReceiptArtifactSHA256,
			ProviderID:            fact.ProviderID, ProviderRevision: fact.ProviderRevision,
			Kind: fact.Kind, AdapterID: fact.AdapterID, AdapterRevision: fact.AdapterRevision,
			AdapterSHA256: fact.AdapterSHA256, RequestSHA256: fact.RequestSHA256,
			BindingMode: string(fact.BindingMode), Status: string(fact.Status),
			ReasonCode: fact.ReasonCode, ContextID: fact.ContextID,
			ContextDigest: fact.ContextDigest, ContextContract: fact.ContextContract,
			TargetPathCount: fact.TargetPathCount, TimeoutMicros: fact.TimeoutMicros,
			DurationMicros: fact.DurationMicros, Authority: fact.Authority,
			TenantID: dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision,
			ConfigRevision:   dimensions.configRevision,
			ReviewDimension:  dimensions.reviewDimension,
			OccurredAtMicros: fact.OccurredAt.UnixMicro(),
		}
	}
	return rows, nil
}

func findingParquetRows(
	facts []analytics.FindingFunnelFact,
) ([]findingParquetRow, error) {
	rows := make([]findingParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		publicationRef := flattenSourceRef(fact.PublicationRef)
		reasons, err := marshalFlatJSON(fact.IncompleteReasons)
		if err != nil {
			return nil, fmt.Errorf("finding_funnel[%d] incomplete reasons: %w", index, err)
		}
		row := findingParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID, RunID: fact.RunID,
			CandidateID: fact.CandidateID, FindingID: fact.FindingID,
			Normalized: fact.Normalized, Verification: string(fact.Verification),
			PublicationEligibility: string(fact.PublicationEligibility),
			Publication:            string(fact.Publication),
			PublicationRefPresent:  publicationRef.present,
			PublicationRefKind:     publicationRef.kind,
			PublicationRefID:       publicationRef.id,
			PublicationRefRevision: publicationRef.revision,
			PublicationRefSHA256:   publicationRef.sha256,
			ResultCompleteness:     string(fact.ResultCompleteness),
			IncompleteReasonsJSON:  reasons,
			TenantID:               dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision,
			ConfigRevision:   dimensions.configRevision,
			ReviewDimension:  dimensions.reviewDimension,
			OccurredAtMicros: fact.OccurredAt.UnixMicro(),
		}
		if fact.PublicationAt != nil {
			row.PublicationAtPresent = true
			row.PublicationAtMicros = fact.PublicationAt.UnixMicro()
		}
		rows[index] = row
	}
	return rows, nil
}

func feedbackOutcomeParquetRows(
	facts []analytics.FeedbackOutcomeFact,
) ([]feedbackOutcomeParquetRow, error) {
	rows := make([]feedbackOutcomeParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		reasons, err := marshalFlatJSON(fact.IncompleteReasons)
		if err != nil {
			return nil, fmt.Errorf("feedback_outcomes[%d] incomplete reasons: %w", index, err)
		}
		feedbackRef := flattenSourceRef(fact.FeedbackRef)
		outcomeRef := flattenSourceRef(fact.OutcomeRef)
		row := feedbackOutcomeParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID, RunID: fact.RunID,
			FindingID: fact.FindingID, Feedback: string(fact.Feedback),
			FeedbackRefPresent: feedbackRef.present, FeedbackRefKind: feedbackRef.kind,
			FeedbackRefID: feedbackRef.id, FeedbackRefRevision: feedbackRef.revision,
			FeedbackRefSHA256: feedbackRef.sha256, Outcome: string(fact.Outcome),
			OutcomeRefPresent: outcomeRef.present, OutcomeRefKind: outcomeRef.kind,
			OutcomeRefID: outcomeRef.id, OutcomeRefRevision: outcomeRef.revision,
			OutcomeRefSHA256:      outcomeRef.sha256,
			ResultCompleteness:    string(fact.ResultCompleteness),
			IncompleteReasonsJSON: reasons,
			TenantID:              dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision,
			ConfigRevision:   dimensions.configRevision,
			ReviewDimension:  dimensions.reviewDimension,
			OccurredAtMicros: fact.OccurredAt.UnixMicro(),
		}
		if fact.FeedbackAt != nil {
			row.FeedbackAtPresent = true
			row.FeedbackAtMicros = fact.FeedbackAt.UnixMicro()
		}
		if fact.OutcomeAt != nil {
			row.OutcomeAtPresent = true
			row.OutcomeAtMicros = fact.OutcomeAt.UnixMicro()
		}
		rows[index] = row
	}
	return rows, nil
}

func findingLineageParquetRows(facts []analytics.FindingLineageFact) ([]findingLineageParquetRow, error) {
	rows := make([]findingLineageParquetRow, len(facts))
	for index, fact := range facts {
		baseline, err := marshalFlatJSON(fact.BaselineFindingIDs)
		if err != nil {
			return nil, fmt.Errorf("finding_lineages[%d] baseline finding ids: %w", index, err)
		}
		variant, err := marshalFlatJSON(fact.VariantFindingIDs)
		if err != nil {
			return nil, fmt.Errorf("finding_lineages[%d] variant finding ids: %w", index, err)
		}
		rows[index] = findingLineageParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID, LineageID: fact.LineageID,
			LineageArtifactSHA256: fact.LineageArtifactSHA256, RelationID: fact.RelationID,
			RelationType: fact.RelationType, RelationMethod: fact.RelationMethod, ReasonCode: fact.ReasonCode,
			FamilyKey: fact.FamilyKey, PolicyID: fact.PolicyID, PolicyRevision: fact.PolicyRevision,
			PolicySHA256: fact.PolicySHA256, AncestryAuthority: fact.AncestryAuthority,
			AncestryEvidenceSHA256: fact.AncestryEvidenceSHA256, RenameMappingCount: fact.RenameMappingCount,
			BaselineRunID: fact.BaselineRunID, VariantRunID: fact.VariantRunID,
			BaselineFindingIDsJSON: baseline, VariantFindingIDsJSON: variant,
			TenantID: fact.TenantID, OrganizationID: fact.OrganizationID, WorkspaceID: fact.WorkspaceID,
			RepositoryID: fact.RepositoryID, OccurredAtMicros: fact.OccurredAt.UnixMicro(),
		}
	}
	return rows, nil
}

func experimentParquetRows(
	facts []analytics.ExperimentFact,
) ([]experimentParquetRow, error) {
	rows := make([]experimentParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		reasons, err := marshalFlatJSON(fact.IncompleteReasons)
		if err != nil {
			return nil, fmt.Errorf("experiments[%d] incomplete reasons: %w", index, err)
		}
		sources, err := marshalFlatJSON(fact.SourceRefs)
		if err != nil {
			return nil, fmt.Errorf("experiments[%d] source refs: %w", index, err)
		}
		rows[index] = experimentParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID,
			ExperimentID: fact.ExperimentID, ExperimentRevision: fact.ExperimentRevision,
			Arm: string(fact.Arm), VariantID: fact.VariantID, MetricID: fact.MetricID,
			MetricVersion: fact.MetricVersion, MetricDefinition: fact.MetricDefinition,
			Direction: string(fact.Direction), ValueAmount: fact.Value.Amount,
			ValueScale: uint32(fact.Value.Scale), ValueUnit: fact.Value.Unit,
			SampleSize: fact.SampleSize, WindowStartMicros: fact.Window.StartInclusive.UnixMicro(),
			WindowEndMicros:       fact.Window.EndExclusive.UnixMicro(),
			ResultCompleteness:    string(fact.ResultCompleteness),
			IncompleteReasonsJSON: reasons,
			TenantID:              dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision,
			ConfigRevision:   dimensions.configRevision,
			ReviewDimension:  dimensions.reviewDimension,
			SourceRefsJSON:   sources, OccurredAtMicros: fact.OccurredAt.UnixMicro(),
		}
	}
	return rows, nil
}

func repeatabilityParquetRows(
	facts []analytics.RepeatabilityFact,
) ([]repeatabilityParquetRow, error) {
	rows := make([]repeatabilityParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		reasons, err := marshalFlatJSON(fact.IncompleteReasons)
		if err != nil {
			return nil, fmt.Errorf("repeatability[%d] incomplete reasons: %w", index, err)
		}
		sources, err := marshalFlatJSON(fact.SourceRefs)
		if err != nil {
			return nil, fmt.Errorf("repeatability[%d] source refs: %w", index, err)
		}
		rows[index] = repeatabilityParquetRow{
			SchemaVersion: fact.SchemaVersion, FactID: fact.FactID,
			RepeatabilityRunID: fact.RepeatabilityRunID, RepeatabilityRevision: fact.RepeatabilityRevision,
			CaseID: fact.CaseID, MetricID: fact.MetricID, MetricVersion: fact.MetricVersion,
			MetricDefinition: fact.MetricDefinition, Direction: string(fact.Direction),
			ValueAmount: fact.Value.Amount, ValueScale: uint32(fact.Value.Scale), ValueUnit: fact.Value.Unit,
			SampleSize: fact.SampleSize, WindowStartMicros: fact.Window.StartInclusive.UnixMicro(),
			WindowEndMicros:    fact.Window.EndExclusive.UnixMicro(),
			ResultCompleteness: string(fact.ResultCompleteness), IncompleteReasonsJSON: reasons,
			TenantID: dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision, ConfigRevision: dimensions.configRevision,
			ReviewDimension: dimensions.reviewDimension,
			SourceRefsJSON:  sources, OccurredAtMicros: fact.OccurredAt.UnixMicro(),
		}
	}
	return rows, nil
}

func valueObservationParquetRows(
	facts []analytics.ValueObservation,
) ([]valueObservationParquetRow, error) {
	rows := make([]valueObservationParquetRow, len(facts))
	for index, fact := range facts {
		dimensions := flattenDimensions(fact.Dimensions)
		baselineSources, err := marshalFlatJSON(fact.Baseline.SourceRefs)
		if err != nil {
			return nil, fmt.Errorf(
				"value_observations[%d] baseline source refs: %w",
				index,
				err,
			)
		}
		sources, err := marshalFlatJSON(fact.SourceRefs)
		if err != nil {
			return nil, fmt.Errorf("value_observations[%d] source refs: %w", index, err)
		}
		rows[index] = valueObservationParquetRow{
			SchemaVersion: fact.SchemaVersion, ObservationID: fact.ObservationID,
			TenantID: dimensions.tenantID, OrganizationID: dimensions.organizationID,
			RepositoryID: dimensions.repositoryID, Language: dimensions.language,
			RuleID: dimensions.ruleID, Path: dimensions.path,
			WorkflowRevision: dimensions.workflowRevision,
			ConfigRevision:   dimensions.configRevision,
			ReviewDimension:  dimensions.reviewDimension,
			MetricID:         fact.MetricID, EvidenceTier: string(fact.EvidenceTier),
			BaselineCohortID:                fact.Baseline.CohortID,
			BaselineRevision:                fact.Baseline.Revision,
			BaselineWindowStartMicros:       fact.Baseline.Window.StartInclusive.UnixMicro(),
			BaselineWindowEndMicros:         fact.Baseline.Window.EndExclusive.UnixMicro(),
			BaselineValueAmount:             fact.Baseline.Value.Amount,
			BaselineValueScale:              uint32(fact.Baseline.Value.Scale),
			BaselineValueUnit:               fact.Baseline.Value.Unit,
			BaselineSampleSize:              fact.Baseline.SampleSize,
			BaselineSourceRefsJSON:          baselineSources,
			ObservationWindowStartMicros:    fact.ObservationWindow.StartInclusive.UnixMicro(),
			ObservationWindowEndMicros:      fact.ObservationWindow.EndExclusive.UnixMicro(),
			AttributionWindowStartMicros:    fact.AttributionWindow.StartInclusive.UnixMicro(),
			AttributionWindowEndMicros:      fact.AttributionWindow.EndExclusive.UnixMicro(),
			SampleSize:                      fact.SampleSize,
			GrossValueAmount:                fact.GrossValue.Amount,
			GrossValueScale:                 uint32(fact.GrossValue.Scale),
			GrossValueUnit:                  fact.GrossValue.Unit,
			ModelComputeAmount:              fact.Costs.ModelCompute.Amount,
			ModelComputeScale:               uint32(fact.Costs.ModelCompute.Scale),
			ModelComputeUnit:                fact.Costs.ModelCompute.Unit,
			HumanVerificationAmount:         fact.Costs.HumanVerification.Amount,
			HumanVerificationScale:          uint32(fact.Costs.HumanVerification.Scale),
			HumanVerificationUnit:           fact.Costs.HumanVerification.Unit,
			FalsePositiveInterruptionAmount: fact.Costs.FalsePositive.Amount,
			FalsePositiveInterruptionScale:  uint32(fact.Costs.FalsePositive.Scale),
			FalsePositiveInterruptionUnit:   fact.Costs.FalsePositive.Unit,
			PlatformOperationsAmount:        fact.Costs.PlatformOperations.Amount,
			PlatformOperationsScale:         uint32(fact.Costs.PlatformOperations.Scale),
			PlatformOperationsUnit:          fact.Costs.PlatformOperations.Unit,
			StorageAmount:                   fact.Costs.Storage.Amount,
			StorageScale:                    uint32(fact.Costs.Storage.Scale),
			StorageUnit:                     fact.Costs.Storage.Unit,
			NetValueAmount:                  fact.NetValue.Amount,
			NetValueScale:                   uint32(fact.NetValue.Scale),
			NetValueUnit:                    fact.NetValue.Unit,
			NetValueUncertaintyLowerAmount:  fact.NetValueUncertainty.Lower.Amount,
			NetValueUncertaintyLowerScale:   uint32(fact.NetValueUncertainty.Lower.Scale),
			NetValueUncertaintyLowerUnit:    fact.NetValueUncertainty.Lower.Unit,
			NetValueUncertaintyUpperAmount:  fact.NetValueUncertainty.Upper.Amount,
			NetValueUncertaintyUpperScale:   uint32(fact.NetValueUncertainty.Upper.Scale),
			NetValueUncertaintyUpperUnit:    fact.NetValueUncertainty.Upper.Unit,
			NetValueUncertaintyConfidenceBPS: uint32(
				fact.NetValueUncertainty.ConfidenceBPS,
			),
			NetValueUncertaintyMethod:   fact.NetValueUncertainty.Method,
			NetValueUncertaintyRevision: fact.NetValueUncertainty.Revision,
			SourceRefsJSON:              sources,
			AttributionPolicyRevision:   fact.AttributionPolicyRevision,
			ObservedAtMicros:            fact.ObservedAt.UnixMicro(),
		}
	}
	return rows, nil
}

func flattenDimensions(value analytics.Dimensions) dimensionValues {
	return dimensionValues{
		tenantID: value.TenantID, organizationID: value.OrganizationID,
		repositoryID: value.RepositoryID, language: value.Language,
		ruleID: value.RuleID, path: value.Path,
		workflowRevision: value.WorkflowRevision,
		configRevision:   value.ConfigRevision,
		reviewDimension:  value.ReviewDimension,
	}
}

func flattenSourceRef(value *analytics.SourceRef) sourceRefValues {
	if value == nil {
		return sourceRefValues{}
	}
	return sourceRefValues{
		present: true, kind: string(value.Kind), id: value.ID,
		revision: value.Revision, sha256: value.SHA256,
	}
}

func marshalFlatJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func digestParquetBytes(data []byte) string {
	return digestBytes(data)
}

func newParquetWriter[T any](output io.Writer) (
	writer *parquet.GenericWriter[T],
	err error,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			writer = nil
			err = fmt.Errorf("parquet writer constructor panicked: %v", recovered)
		}
	}()
	schema := parquet.SchemaOf(new(T))
	config, err := parquet.NewWriterConfig(schema)
	if err != nil {
		return nil, err
	}
	return parquet.NewGenericWriter[T](output, config), nil
}

func preflightParquetFile(data []byte) error {
	if len(data) < 12 {
		return fmt.Errorf("Parquet file is shorter than header/footer minimum")
	}
	headerMagic := string(data[:4])
	if headerMagic != "PAR1" && headerMagic != "PARE" {
		return fmt.Errorf("Parquet header magic is invalid")
	}
	footer := data[len(data)-8:]
	magic := string(footer[4:])
	if magic != "PAR1" && magic != "PARE" {
		return fmt.Errorf("Parquet footer magic is invalid")
	}
	footerSize := int64(binary.LittleEndian.Uint32(footer[:4]))
	if footerSize > int64(len(data))-12 {
		return fmt.Errorf("Parquet footer exceeds file size")
	}
	if footerSize > maxParquetFooterBytes {
		return fmt.Errorf(
			"Parquet footer exceeds %d-byte verification budget",
			maxParquetFooterBytes,
		)
	}
	return nil
}
