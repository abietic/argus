package analytics

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DecodeFactSetJSON rejects unknown fields, trailing values, and semantically
// invalid facts. It is suitable for an ingestion boundary.
func DecodeFactSetJSON(data []byte) (FactSet, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var facts FactSet
	if err := decoder.Decode(&facts); err != nil {
		return FactSet{}, fmt.Errorf("decode fact set: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return FactSet{}, fmt.Errorf("decode fact set: trailing JSON value")
		}
		return FactSet{}, fmt.Errorf("decode fact set trailing data: %w", err)
	}
	if err := facts.Validate(); err != nil {
		return FactSet{}, fmt.Errorf("validate fact set: %w", err)
	}
	return facts, nil
}

func ExportFactSetJSON(facts FactSet) (ExportBundle, error) {
	canonical, err := canonicalFactSet(facts)
	if err != nil {
		return ExportBundle{}, err
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return ExportBundle{}, fmt.Errorf("marshal canonical fact set: %w", err)
	}
	data = append(data, '\n')
	count, err := factCount(canonical)
	if err != nil {
		return ExportBundle{}, err
	}
	return newExportBundle(
		FactSetSchemaVersion,
		CanonicalJSONExportFormat,
		canonical.Window,
		count,
		[]exportPayload{
			{
				name:      "analytics_facts.json",
				mediaType: "application/json",
				rowCount:  count,
				data:      data,
			},
		},
	)
}

func ExportFactSetCSV(facts FactSet) (ExportBundle, error) {
	canonical, err := canonicalFactSet(facts)
	if err != nil {
		return ExportBundle{}, err
	}
	payloads := make([]exportPayload, 0, 9)
	for _, table := range []struct {
		name   string
		rows   uint64
		render func() ([]byte, error)
	}{
		{"experiments.csv", uint64(len(canonical.Experiments)), func() ([]byte, error) {
			return renderExperimentCSV(canonical.Experiments)
		}},
		{"repeatability.csv", uint64(len(canonical.Repeatability)), func() ([]byte, error) {
			return renderRepeatabilityCSV(canonical.Repeatability)
		}},
		{"context_providers.csv", uint64(len(canonical.ContextProviders)), func() ([]byte, error) {
			return renderContextProviderCSV(canonical.ContextProviders)
		}},
		{
			"feedback_outcomes.csv",
			uint64(len(canonical.FeedbackOutcomes)),
			func() ([]byte, error) {
				return renderFeedbackOutcomeCSV(canonical.FeedbackOutcomes)
			},
		},
		{"finding_funnel.csv", uint64(len(canonical.Findings)), func() ([]byte, error) {
			return renderFindingCSV(canonical.Findings)
		}},
		{"finding_lineages.csv", uint64(len(canonical.FindingLineages)), func() ([]byte, error) {
			return renderFindingLineageCSV(canonical.FindingLineages)
		}},
		{"review_runs.csv", uint64(len(canonical.ReviewRuns)), func() ([]byte, error) {
			return renderReviewRunCSV(canonical.ReviewRuns)
		}},
		{"stages.csv", uint64(len(canonical.Stages)), func() ([]byte, error) {
			return renderStageCSV(canonical.Stages)
		}},
		{
			"value_observations.csv",
			uint64(len(canonical.ValueObservations)),
			func() ([]byte, error) {
				return renderValueObservationCSV(canonical.ValueObservations)
			},
		},
	} {
		data, renderErr := table.render()
		if renderErr != nil {
			return ExportBundle{}, fmt.Errorf("render %s: %w", table.name, renderErr)
		}
		payloads = append(payloads, exportPayload{
			name:      table.name,
			mediaType: "text/csv; charset=utf-8",
			rowCount:  table.rows,
			data:      data,
		})
	}
	count, err := factCount(canonical)
	if err != nil {
		return ExportBundle{}, err
	}
	return newExportBundle(
		FactSetSchemaVersion,
		CanonicalCSVExportFormat,
		canonical.Window,
		count,
		payloads,
	)
}

func ExportDashboardJSON(
	projection DashboardProjection,
) (ExportBundle, error) {
	if err := projection.Validate(); err != nil {
		return ExportBundle{}, fmt.Errorf("validate dashboard: %w", err)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return ExportBundle{}, fmt.Errorf("marshal dashboard: %w", err)
	}
	data = append(data, '\n')
	return newExportBundle(
		DashboardProjectionSchemaVersion,
		CanonicalJSONExportFormat,
		projection.Window,
		uint64(len(projection.Tiles)),
		[]exportPayload{
			{
				name:      "dashboard.json",
				mediaType: "application/json",
				rowCount:  uint64(len(projection.Tiles)),
				data:      data,
			},
		},
	)
}

func ExportDashboardCSV(
	projection DashboardProjection,
) (ExportBundle, error) {
	if err := projection.Validate(); err != nil {
		return ExportBundle{}, fmt.Errorf("validate dashboard: %w", err)
	}
	data, err := renderDashboardCSV(projection.Tiles)
	if err != nil {
		return ExportBundle{}, fmt.Errorf("render dashboard CSV: %w", err)
	}
	return newExportBundle(
		DashboardProjectionSchemaVersion,
		CanonicalCSVExportFormat,
		projection.Window,
		uint64(len(projection.Tiles)),
		[]exportPayload{
			{
				name:      "dashboard.csv",
				mediaType: "text/csv; charset=utf-8",
				rowCount:  uint64(len(projection.Tiles)),
				data:      data,
			},
		},
	)
}

func (bundle ExportBundle) Validate() error {
	if err := bundle.Manifest.Validate(); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if bundle.Files == nil || len(bundle.Files) != len(bundle.Manifest.Files) {
		return fmt.Errorf("payload files do not match manifest")
	}
	for index, file := range bundle.Files {
		expected := bundle.Manifest.Files[index]
		if file.Manifest != expected {
			return fmt.Errorf("payload file %d metadata does not match manifest", index)
		}
		if int64(len(file.Data)) != expected.SizeBytes ||
			digestBytes(file.Data) != expected.SHA256 {
			return fmt.Errorf("payload file %q does not match size/digest", expected.Name)
		}
	}
	return nil
}

type exportPayload struct {
	name      string
	mediaType string
	rowCount  uint64
	data      []byte
}

func newExportBundle(
	datasetSchemaVersion string,
	format string,
	window TimeWindow,
	factCount uint64,
	payloads []exportPayload,
) (ExportBundle, error) {
	slices.SortFunc(payloads, func(left, right exportPayload) int {
		return strings.Compare(left.name, right.name)
	})
	files := make([]ExportFile, 0, len(payloads))
	manifests := make([]ExportFileManifest, 0, len(payloads))
	for _, payload := range payloads {
		manifest := ExportFileManifest{
			Name:      payload.name,
			MediaType: payload.mediaType,
			RowCount:  payload.rowCount,
			SizeBytes: int64(len(payload.data)),
			SHA256:    digestBytes(payload.data),
		}
		manifests = append(manifests, manifest)
		files = append(files, ExportFile{
			Manifest: manifest,
			Data:     slices.Clone(payload.data),
		})
	}
	bundle := ExportBundle{
		Manifest: ExportManifest{
			SchemaVersion:        ExportManifestSchemaVersion,
			DatasetSchemaVersion: datasetSchemaVersion,
			Format:               format,
			Window:               window,
			FactCount:            factCount,
			Parquet:              false,
			Limitations:          []string{parquetNotEmittedLimitation},
			Files:                manifests,
		},
		Files: files,
	}
	if err := bundle.Validate(); err != nil {
		return ExportBundle{}, err
	}
	return bundle, nil
}

func canonicalFactSet(facts FactSet) (FactSet, error) {
	if err := facts.Validate(); err != nil {
		return FactSet{}, fmt.Errorf("validate facts: %w", err)
	}
	result := facts
	result.IncompleteReasons = slices.Clone(facts.IncompleteReasons)
	result.ReviewRuns = slices.Clone(facts.ReviewRuns)
	result.Stages = slices.Clone(facts.Stages)
	result.ContextProviders = slices.Clone(facts.ContextProviders)
	result.Findings = slices.Clone(facts.Findings)
	result.FeedbackOutcomes = slices.Clone(facts.FeedbackOutcomes)
	result.FindingLineages = slices.Clone(facts.FindingLineages)
	result.Experiments = slices.Clone(facts.Experiments)
	result.Repeatability = slices.Clone(facts.Repeatability)
	result.ValueObservations = slices.Clone(facts.ValueObservations)
	slices.SortFunc(result.ReviewRuns, func(left, right ReviewRunFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(result.Stages, func(left, right StageFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(result.ContextProviders, func(left, right ContextProviderFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(result.Findings, func(left, right FindingFunnelFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(
		result.FeedbackOutcomes,
		func(left, right FeedbackOutcomeFact) int {
			return strings.Compare(left.FactID, right.FactID)
		},
	)
	slices.SortFunc(result.FindingLineages, func(left, right FindingLineageFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(result.Experiments, func(left, right ExperimentFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(result.Repeatability, func(left, right RepeatabilityFact) int {
		return strings.Compare(left.FactID, right.FactID)
	})
	slices.SortFunc(
		result.ValueObservations,
		func(left, right ValueObservation) int {
			return strings.Compare(left.ObservationID, right.ObservationID)
		},
	)
	return result, nil
}

// CanonicalFactSet returns a validated copy with every fact table sorted by
// its immutable identity. Adapter-layer encoders use this to produce stable
// provider-specific files without importing those providers into analytics.
func CanonicalFactSet(facts FactSet) (FactSet, error) {
	return canonicalFactSet(facts)
}

func factCount(facts FactSet) (uint64, error) {
	count := uint64(0)
	for _, size := range []int{
		len(facts.ReviewRuns),
		len(facts.Stages),
		len(facts.ContextProviders),
		len(facts.Findings),
		len(facts.FeedbackOutcomes),
		len(facts.FindingLineages),
		len(facts.Experiments),
		len(facts.Repeatability),
		len(facts.ValueObservations),
	} {
		next, overflow := addUint64(count, uint64(size))
		if overflow {
			return 0, fmt.Errorf("fact count overflows uint64")
		}
		count = next
	}
	return count, nil
}

func renderFindingLineageCSV(facts []FindingLineageFact) ([]byte, error) {
	header := []string{
		"schema_version", "fact_id", "lineage_id", "lineage_artifact_sha256", "relation_id",
		"relation_type", "relation_method", "reason_code", "family_key", "policy_id",
		"policy_revision", "policy_sha256", "ancestry_authority", "ancestry_evidence_sha256",
		"rename_mapping_count", "baseline_run_id", "variant_run_id", "baseline_finding_ids_json",
		"variant_finding_ids_json", "tenant_id", "organization_id", "workspace_id", "repository_id", "occurred_at",
	}
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		baseline, err := json.Marshal(fact.BaselineFindingIDs)
		if err != nil {
			return nil, err
		}
		variant, err := json.Marshal(fact.VariantFindingIDs)
		if err != nil {
			return nil, err
		}
		rows = append(rows, []string{
			fact.SchemaVersion, fact.FactID, fact.LineageID, fact.LineageArtifactSHA256, fact.RelationID,
			fact.RelationType, fact.RelationMethod, fact.ReasonCode, fact.FamilyKey, fact.PolicyID,
			fact.PolicyRevision, fact.PolicySHA256, fact.AncestryAuthority, fact.AncestryEvidenceSHA256,
			strconv.FormatUint(uint64(fact.RenameMappingCount), 10), fact.BaselineRunID, fact.VariantRunID,
			string(baseline), string(variant), fact.TenantID, fact.OrganizationID, fact.WorkspaceID,
			fact.RepositoryID, formatTime(fact.OccurredAt),
		})
	}
	return renderCSV(header, rows)
}

// FactCount returns the reconciled number of rows across all fact tables.
func FactCount(facts FactSet) (uint64, error) {
	return factCount(facts)
}

func renderReviewRunCSV(facts []ReviewRunFact) ([]byte, error) {
	header := append(
		[]string{
			"schema_version", "fact_id", "run_id",
		},
		dimensionColumns()...,
	)
	header = append(header,
		"status", "result_completeness", "incomplete_reasons_json",
		"funnel_present", "candidates", "normalized", "verified", "published",
		"started_at", "finished_at", "occurred_at",
	)
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		funnelPresent := "false"
		var candidates, normalized, verified, published string
		if fact.Funnel != nil {
			funnelPresent = "true"
			candidates = strconv.FormatUint(fact.Funnel.Candidates, 10)
			normalized = strconv.FormatUint(fact.Funnel.Normalized, 10)
			verified = strconv.FormatUint(fact.Funnel.Verified, 10)
			published = strconv.FormatUint(fact.Funnel.Published, 10)
		}
		finishedAt := ""
		if fact.FinishedAt != nil {
			finishedAt = formatTime(*fact.FinishedAt)
		}
		row := append(
			[]string{fact.SchemaVersion, fact.FactID, fact.RunID},
			dimensionFields(fact.Dimensions)...,
		)
		row = append(row,
			string(fact.Status),
			string(fact.ResultCompleteness),
			mustJSONCell(fact.IncompleteReasons),
			funnelPresent,
			candidates,
			normalized,
			verified,
			published,
			formatTime(fact.StartedAt),
			finishedAt,
			formatTime(fact.OccurredAt),
		)
		rows = append(rows, row)
	}
	return renderCSV(header, rows)
}

func renderStageCSV(facts []StageFact) ([]byte, error) {
	header := append(
		[]string{
			"schema_version", "fact_id", "run_id", "stage_id", "stage_revision",
			"attempt",
		},
		dimensionColumns()...,
	)
	header = append(header,
		"status", "result_completeness", "incomplete_reasons_json",
		"duration_micros", "occurred_at",
	)
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		row := append(
			[]string{
				fact.SchemaVersion,
				fact.FactID,
				fact.RunID,
				fact.StageID,
				fact.StageRevision,
				strconv.FormatUint(uint64(fact.Attempt), 10),
			},
			dimensionFields(fact.Dimensions)...,
		)
		row = append(row,
			string(fact.Status),
			string(fact.ResultCompleteness),
			mustJSONCell(fact.IncompleteReasons),
			strconv.FormatUint(fact.DurationMicros, 10),
			formatTime(fact.OccurredAt),
		)
		rows = append(rows, row)
	}
	return renderCSV(header, rows)
}

func renderContextProviderCSV(facts []ContextProviderFact) ([]byte, error) {
	header := []string{
		"schema_version", "fact_id", "run_id", "receipt_id", "receipt_sha256",
		"receipt_artifact_sha256", "provider_id", "provider_revision", "kind",
		"adapter_id", "adapter_revision", "adapter_sha256", "request_sha256",
		"binding_mode", "status", "reason_code", "context_id", "context_digest",
		"context_contract", "target_path_count", "timeout_micros", "duration_micros",
		"authority", "dimensions_json", "occurred_at",
	}
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		rows = append(rows, []string{
			fact.SchemaVersion, fact.FactID, fact.RunID, fact.ReceiptID,
			fact.ReceiptSHA256, fact.ReceiptArtifactSHA256, fact.ProviderID,
			fact.ProviderRevision, fact.Kind, fact.AdapterID, fact.AdapterRevision,
			fact.AdapterSHA256, fact.RequestSHA256, string(fact.BindingMode),
			string(fact.Status), fact.ReasonCode, fact.ContextID, fact.ContextDigest,
			fact.ContextContract, strconv.FormatUint(fact.TargetPathCount, 10),
			strconv.FormatUint(fact.TimeoutMicros, 10),
			strconv.FormatUint(fact.DurationMicros, 10), fact.Authority,
			mustJSONCell(fact.Dimensions), formatTime(fact.OccurredAt),
		})
	}
	return renderCSV(header, rows)
}

func renderFindingCSV(facts []FindingFunnelFact) ([]byte, error) {
	header := append(
		[]string{
			"schema_version", "fact_id", "run_id", "candidate_id", "finding_id",
			"normalized", "verification", "publication_eligibility", "publication",
			"publication_ref_json", "publication_at", "result_completeness",
			"incomplete_reasons_json",
		},
		dimensionColumns()...,
	)
	header = append(header, "occurred_at")
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		publicationAt := ""
		if fact.PublicationAt != nil {
			publicationAt = formatTime(*fact.PublicationAt)
		}
		row := []string{
			fact.SchemaVersion,
			fact.FactID,
			fact.RunID,
			fact.CandidateID,
			fact.FindingID,
			strconv.FormatBool(fact.Normalized),
			string(fact.Verification),
			string(fact.PublicationEligibility),
			string(fact.Publication),
			mustJSONCell(fact.PublicationRef),
			publicationAt,
			string(fact.ResultCompleteness),
			mustJSONCell(fact.IncompleteReasons),
		}
		row = append(row, dimensionFields(fact.Dimensions)...)
		row = append(row, formatTime(fact.OccurredAt))
		rows = append(rows, row)
	}
	return renderCSV(header, rows)
}

func renderFeedbackOutcomeCSV(facts []FeedbackOutcomeFact) ([]byte, error) {
	header := append(
		[]string{
			"schema_version", "fact_id", "run_id", "finding_id", "feedback",
			"feedback_ref_json", "feedback_at", "outcome", "outcome_ref_json",
			"outcome_at",
			"result_completeness", "incomplete_reasons_json",
		},
		dimensionColumns()...,
	)
	header = append(header, "occurred_at")
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		feedbackAt := ""
		if fact.FeedbackAt != nil {
			feedbackAt = formatTime(*fact.FeedbackAt)
		}
		outcomeAt := ""
		if fact.OutcomeAt != nil {
			outcomeAt = formatTime(*fact.OutcomeAt)
		}
		row := []string{
			fact.SchemaVersion,
			fact.FactID,
			fact.RunID,
			fact.FindingID,
			string(fact.Feedback),
			mustJSONCell(fact.FeedbackRef),
			feedbackAt,
			string(fact.Outcome),
			mustJSONCell(fact.OutcomeRef),
			outcomeAt,
			string(fact.ResultCompleteness),
			mustJSONCell(fact.IncompleteReasons),
		}
		row = append(row, dimensionFields(fact.Dimensions)...)
		row = append(row, formatTime(fact.OccurredAt))
		rows = append(rows, row)
	}
	return renderCSV(header, rows)
}

func renderExperimentCSV(facts []ExperimentFact) ([]byte, error) {
	header := []string{
		"schema_version", "fact_id", "experiment_id", "experiment_revision",
		"arm", "variant_id", "metric_id", "metric_version", "metric_definition",
		"direction", "value_amount", "value_scale", "value_unit", "sample_size",
		"window_start", "window_end", "result_completeness",
		"incomplete_reasons_json",
	}
	header = append(header, dimensionColumns()...)
	header = append(header, "source_refs_json", "occurred_at")
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		row := []string{
			fact.SchemaVersion,
			fact.FactID,
			fact.ExperimentID,
			fact.ExperimentRevision,
			string(fact.Arm),
			fact.VariantID,
			fact.MetricID,
			fact.MetricVersion,
			fact.MetricDefinition,
			string(fact.Direction),
			strconv.FormatInt(fact.Value.Amount, 10),
			strconv.FormatUint(uint64(fact.Value.Scale), 10),
			fact.Value.Unit,
			strconv.FormatUint(fact.SampleSize, 10),
			formatTime(fact.Window.StartInclusive),
			formatTime(fact.Window.EndExclusive),
			string(fact.ResultCompleteness),
			mustJSONCell(fact.IncompleteReasons),
		}
		row = append(row, dimensionFields(fact.Dimensions)...)
		row = append(
			row,
			mustJSONCell(fact.SourceRefs),
			formatTime(fact.OccurredAt),
		)
		rows = append(rows, row)
	}
	return renderCSV(header, rows)
}

func renderRepeatabilityCSV(facts []RepeatabilityFact) ([]byte, error) {
	header := []string{
		"schema_version", "fact_id", "repeatability_run_id", "repeatability_revision",
		"case_id", "metric_id", "metric_version", "metric_definition", "direction",
		"value_amount", "value_scale", "value_unit", "sample_size", "window_start",
		"window_end", "result_completeness", "incomplete_reasons_json",
	}
	header = append(header, dimensionColumns()...)
	header = append(header, "source_refs_json", "occurred_at")
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		row := []string{
			fact.SchemaVersion, fact.FactID, fact.RepeatabilityRunID, fact.RepeatabilityRevision,
			fact.CaseID, fact.MetricID, fact.MetricVersion, fact.MetricDefinition,
			string(fact.Direction), strconv.FormatInt(fact.Value.Amount, 10),
			strconv.FormatUint(uint64(fact.Value.Scale), 10), fact.Value.Unit,
			strconv.FormatUint(fact.SampleSize, 10), formatTime(fact.Window.StartInclusive),
			formatTime(fact.Window.EndExclusive), string(fact.ResultCompleteness),
			mustJSONCell(fact.IncompleteReasons),
		}
		row = append(row, dimensionFields(fact.Dimensions)...)
		row = append(row, mustJSONCell(fact.SourceRefs), formatTime(fact.OccurredAt))
		rows = append(rows, row)
	}
	return renderCSV(header, rows)
}

func renderValueObservationCSV(facts []ValueObservation) ([]byte, error) {
	header := append(
		[]string{
			"schema_version", "observation_id",
		},
		dimensionColumns()...,
	)
	header = append(header,
		"metric_id", "evidence_tier", "baseline_json",
		"observation_window_start", "observation_window_end",
		"attribution_window_start", "attribution_window_end", "sample_size",
		"gross_value_json", "costs_json", "net_value_json",
		"net_value_uncertainty_json", "source_refs_json",
		"attribution_policy_revision", "observed_at",
	)
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		row := append(
			[]string{fact.SchemaVersion, fact.ObservationID},
			dimensionFields(fact.Dimensions)...,
		)
		row = append(row,
			fact.MetricID,
			string(fact.EvidenceTier),
			mustJSONCell(fact.Baseline),
			formatTime(fact.ObservationWindow.StartInclusive),
			formatTime(fact.ObservationWindow.EndExclusive),
			formatTime(fact.AttributionWindow.StartInclusive),
			formatTime(fact.AttributionWindow.EndExclusive),
			strconv.FormatUint(fact.SampleSize, 10),
			mustJSONCell(fact.GrossValue),
			mustJSONCell(fact.Costs),
			mustJSONCell(fact.NetValue),
			mustJSONCell(fact.NetValueUncertainty),
			mustJSONCell(fact.SourceRefs),
			fact.AttributionPolicyRevision,
			formatTime(fact.ObservedAt),
		)
		rows = append(rows, row)
	}
	return renderCSV(header, rows)
}

func renderDashboardCSV(tiles []DashboardTile) ([]byte, error) {
	header := []string{
		"tile_id", "metric_id", "metric_version", "definition",
		"window_start", "window_end", "dimensions_json", "sample_size",
		"availability", "qualifier", "value_json", "warnings_json",
	}
	rows := make([][]string, 0, len(tiles))
	for _, tile := range tiles {
		rows = append(rows, []string{
			tile.TileID,
			tile.MetricID,
			tile.MetricVersion,
			tile.Definition,
			formatTime(tile.Window.StartInclusive),
			formatTime(tile.Window.EndExclusive),
			mustJSONCell(tile.Dimensions),
			strconv.FormatUint(tile.SampleSize, 10),
			string(tile.Availability),
			string(tile.Qualifier),
			mustJSONCell(tile.Value),
			mustJSONCell(tile.Warnings),
		})
	}
	return renderCSV(header, rows)
}

func renderCSV(header []string, rows [][]string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write(header); err != nil {
		return nil, err
	}
	if err := writer.WriteAll(rows); err != nil {
		return nil, err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func dimensionColumns() []string {
	return []string{
		"tenant_id",
		"organization_id",
		"repository_id",
		"language",
		"rule_id",
		"path",
		"review_dimension",
		"workflow_revision",
		"config_revision",
	}
}

func dimensionFields(dimensions Dimensions) []string {
	return []string{
		dimensions.TenantID,
		dimensions.OrganizationID,
		dimensions.RepositoryID,
		dimensions.Language,
		dimensions.RuleID,
		dimensions.Path,
		dimensions.ReviewDimension,
		dimensions.WorkflowRevision,
		dimensions.ConfigRevision,
	}
}

func mustJSONCell(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("analytics: marshal validated CSV cell: %v", err))
	}
	return string(data)
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
