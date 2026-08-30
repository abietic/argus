package agentanalytics

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func (adapter *Adapter) Export(
	snapshotID string,
	target ExportTarget,
	format string,
) (ExportBundle, error) {
	snapshot, err := adapter.Query(snapshotID)
	if err != nil {
		return ExportBundle{}, err
	}
	switch target {
	case ExportFacts:
		switch format {
		case CanonicalJSONExportFormat:
			return ExportFactSetJSON(snapshot.Facts)
		case CanonicalCSVExportFormat:
			return ExportFactSetCSV(snapshot.Facts)
		}
	case ExportProjection:
		switch format {
		case CanonicalJSONExportFormat:
			return ExportProjectionJSON(snapshot.Projection)
		case CanonicalCSVExportFormat:
			return ExportProjectionCSV(snapshot.Projection)
		}
	default:
		return ExportBundle{}, fmt.Errorf("unsupported agent execution export target %q", target)
	}
	return ExportBundle{}, fmt.Errorf("unsupported agent execution export format %q", format)
}

func ExportFactSetJSON(facts FactSet) (ExportBundle, error) {
	if err := facts.Validate(); err != nil {
		return ExportBundle{}, fmt.Errorf("validate facts: %w", err)
	}
	data, err := json.Marshal(facts)
	if err != nil {
		return ExportBundle{}, err
	}
	data = append(data, '\n')
	count, ok := checkedSum(
		uint64(len(facts.Executions)),
		uint64(len(facts.Tasks)),
		uint64(len(facts.ToolUsage)),
	)
	if !ok {
		return ExportBundle{}, fmt.Errorf("fact count overflows")
	}
	return newExportBundle(
		FactSetSchemaVersion,
		CanonicalJSONExportFormat,
		facts.Window,
		count,
		[]exportPayload{{
			name: "agent_execution_facts.json", mediaType: "application/json",
			rowCount: count, data: data,
		}},
	)
}

func ExportFactSetCSV(facts FactSet) (ExportBundle, error) {
	if err := facts.Validate(); err != nil {
		return ExportBundle{}, fmt.Errorf("validate facts: %w", err)
	}
	executions, err := renderExecutionCSV(facts.Executions)
	if err != nil {
		return ExportBundle{}, err
	}
	tasks, err := renderTaskCSV(facts.Tasks)
	if err != nil {
		return ExportBundle{}, err
	}
	tools, err := renderToolCSV(facts.ToolUsage)
	if err != nil {
		return ExportBundle{}, err
	}
	count, ok := checkedSum(
		uint64(len(facts.Executions)),
		uint64(len(facts.Tasks)),
		uint64(len(facts.ToolUsage)),
	)
	if !ok {
		return ExportBundle{}, fmt.Errorf("fact count overflows")
	}
	return newExportBundle(
		FactSetSchemaVersion,
		CanonicalCSVExportFormat,
		facts.Window,
		count,
		[]exportPayload{
			{name: "agent_executions.csv", mediaType: "text/csv; charset=utf-8", rowCount: uint64(len(facts.Executions)), data: executions},
			{name: "agent_tasks.csv", mediaType: "text/csv; charset=utf-8", rowCount: uint64(len(facts.Tasks)), data: tasks},
			{name: "agent_tool_usage.csv", mediaType: "text/csv; charset=utf-8", rowCount: uint64(len(facts.ToolUsage)), data: tools},
		},
	)
}

func ExportProjectionJSON(projection Projection) (ExportBundle, error) {
	if err := projection.Validate(); err != nil {
		return ExportBundle{}, fmt.Errorf("validate projection: %w", err)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return ExportBundle{}, err
	}
	data = append(data, '\n')
	return newExportBundle(
		ProjectionSchemaVersion,
		CanonicalJSONExportFormat,
		projection.Window,
		uint64(len(projection.Tiles)),
		[]exportPayload{{
			name: "agent_execution_projection.json", mediaType: "application/json",
			rowCount: uint64(len(projection.Tiles)), data: data,
		}},
	)
}

func ExportProjectionCSV(projection Projection) (ExportBundle, error) {
	if err := projection.Validate(); err != nil {
		return ExportBundle{}, fmt.Errorf("validate projection: %w", err)
	}
	data, err := renderProjectionCSV(projection.Tiles)
	if err != nil {
		return ExportBundle{}, err
	}
	return newExportBundle(
		ProjectionSchemaVersion,
		CanonicalCSVExportFormat,
		projection.Window,
		uint64(len(projection.Tiles)),
		[]exportPayload{{
			name: "agent_execution_projection.csv", mediaType: "text/csv; charset=utf-8",
			rowCount: uint64(len(projection.Tiles)), data: data,
		}},
	)
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
			Name: payload.name, MediaType: payload.mediaType, RowCount: payload.rowCount,
			SizeBytes: int64(len(payload.data)), SHA256: digestBytes(payload.data),
		}
		manifests = append(manifests, manifest)
		files = append(files, ExportFile{Manifest: manifest, Data: slices.Clone(payload.data)})
	}
	bundle := ExportBundle{
		Manifest: ExportManifest{
			SchemaVersion:        ExportManifestSchemaVersion,
			DatasetSchemaVersion: datasetSchemaVersion, Format: format,
			Window: window, FactCount: factCount, Files: manifests,
		},
		Files: files,
	}
	if err := bundle.Validate(); err != nil {
		return ExportBundle{}, err
	}
	return bundle, nil
}

func (bundle ExportBundle) Validate() error {
	if err := bundle.Manifest.Validate(); err != nil {
		return err
	}
	if bundle.Files == nil || len(bundle.Files) != len(bundle.Manifest.Files) {
		return fmt.Errorf("payload files do not match export manifest")
	}
	for index, file := range bundle.Files {
		if file.Manifest != bundle.Manifest.Files[index] ||
			int64(len(file.Data)) != file.Manifest.SizeBytes ||
			digestBytes(file.Data) != file.Manifest.SHA256 {
			return fmt.Errorf("export payload %d does not match its manifest", index)
		}
	}
	return nil
}

func (manifest ExportManifest) Validate() error {
	if manifest.SchemaVersion != ExportManifestSchemaVersion {
		return fmt.Errorf("unsupported export manifest schema %q", manifest.SchemaVersion)
	}
	switch manifest.DatasetSchemaVersion {
	case FactSetSchemaVersion, ProjectionSchemaVersion:
	default:
		return fmt.Errorf("unsupported export dataset schema %q", manifest.DatasetSchemaVersion)
	}
	switch manifest.Format {
	case CanonicalJSONExportFormat, CanonicalCSVExportFormat:
	default:
		return fmt.Errorf("unsupported export format %q", manifest.Format)
	}
	if err := manifest.Window.Validate(); err != nil {
		return err
	}
	expectedFiles, err := expectedExportFiles(
		manifest.DatasetSchemaVersion,
		manifest.Format,
	)
	if err != nil {
		return err
	}
	if manifest.Files == nil || len(manifest.Files) != len(expectedFiles) {
		return fmt.Errorf("export files do not match the canonical dataset file set")
	}
	var rows uint64
	for index, file := range manifest.Files {
		if file.Name != expectedFiles[index].name ||
			file.MediaType != expectedFiles[index].mediaType {
			return fmt.Errorf("files[%d] does not match the canonical export file contract", index)
		}
		if file.SizeBytes <= 0 {
			return fmt.Errorf("files[%d] must bind a non-empty payload", index)
		}
		if err := validateSHA256("files.sha256", file.SHA256); err != nil {
			return err
		}
		var ok bool
		rows, ok = checkedAdd(rows, file.RowCount)
		if !ok {
			return fmt.Errorf("export row count overflows")
		}
	}
	if manifest.Format == CanonicalJSONExportFormat {
		if len(manifest.Files) != 1 || manifest.Files[0].RowCount != manifest.FactCount {
			return fmt.Errorf("canonical JSON requires one file covering fact_count")
		}
	} else if rows != manifest.FactCount {
		return fmt.Errorf("CSV row counts do not equal fact_count")
	}
	return nil
}

type expectedExportFile struct {
	name      string
	mediaType string
}

func expectedExportFiles(datasetSchemaVersion, format string) ([]expectedExportFile, error) {
	const (
		jsonMediaType = "application/json"
		csvMediaType  = "text/csv; charset=utf-8"
	)
	switch {
	case datasetSchemaVersion == FactSetSchemaVersion && format == CanonicalJSONExportFormat:
		return []expectedExportFile{{"agent_execution_facts.json", jsonMediaType}}, nil
	case datasetSchemaVersion == FactSetSchemaVersion && format == CanonicalCSVExportFormat:
		return []expectedExportFile{
			{"agent_executions.csv", csvMediaType},
			{"agent_tasks.csv", csvMediaType},
			{"agent_tool_usage.csv", csvMediaType},
		}, nil
	case datasetSchemaVersion == ProjectionSchemaVersion && format == CanonicalJSONExportFormat:
		return []expectedExportFile{{"agent_execution_projection.json", jsonMediaType}}, nil
	case datasetSchemaVersion == ProjectionSchemaVersion && format == CanonicalCSVExportFormat:
		return []expectedExportFile{{"agent_execution_projection.csv", csvMediaType}}, nil
	default:
		return nil, fmt.Errorf(
			"unsupported export dataset/format combination %q/%q",
			datasetSchemaVersion,
			format,
		)
	}
}

func renderExecutionCSV(facts []ExecutionFact) ([]byte, error) {
	header := []string{
		"schema_version", "fact_id", "observation_id", "manifest_id", "source_run_id",
		"execution_id", "review_run_id", "tenant_id", "workspace_id", "agent_json",
		"provider_json", "model_json", "api_protocol", "execution_class", "attestation",
		"disposition", "status", "duration_ms", "reason_codes_json", "receipts",
		"tasks_succeeded", "tasks_failed", "tasks_canceled", "model_turns_started",
		"model_turns_completed", "tool_calls", "usage_completeness", "usage_reported_receipts",
		"usage_partial_receipts", "usage_unavailable_receipts", "input_tokens", "output_tokens",
		"cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
		"reasoning_reported_receipts", "total_tokens", "provenance_class", "authority",
		"observed_at",
	}
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		rows = append(rows, []string{
			fact.SchemaVersion, fact.FactID, fact.ObservationID, fact.ManifestID,
			fact.SourceRunID, fact.ExecutionID, fact.ReviewRunID, fact.TenantID,
			fact.WorkspaceID, jsonField(fact.Agent), jsonField(fact.Provider),
			jsonField(fact.Model), string(fact.APIProtocol), string(fact.ExecutionClass),
			string(fact.Attestation), fact.Disposition, string(fact.Status),
			strconv.FormatUint(fact.DurationMS, 10), jsonField(fact.ReasonCodes),
			strconv.FormatUint(fact.Receipts, 10), strconv.FormatUint(fact.TasksSucceeded, 10),
			strconv.FormatUint(fact.TasksFailed, 10), strconv.FormatUint(fact.TasksCanceled, 10),
			strconv.FormatUint(fact.ModelTurnsStarted, 10),
			strconv.FormatUint(fact.ModelTurnsCompleted, 10), strconv.FormatUint(fact.ToolCalls, 10),
			string(fact.Usage.Completeness), strconv.FormatUint(fact.Usage.ReportedReceipts, 10),
			strconv.FormatUint(fact.Usage.PartialReceipts, 10),
			strconv.FormatUint(fact.Usage.UnavailableReceipts, 10),
			strconv.FormatUint(fact.Usage.InputTokens, 10),
			strconv.FormatUint(fact.Usage.OutputTokens, 10),
			strconv.FormatUint(fact.Usage.CacheReadTokens, 10),
			strconv.FormatUint(fact.Usage.CacheWriteTokens, 10),
			strconv.FormatUint(fact.Usage.ReasoningTokens, 10),
			strconv.FormatUint(fact.Usage.ReasoningReportedReceipts, 10),
			strconv.FormatUint(fact.Usage.TotalTokens, 10), fact.ProvenanceClass,
			fact.Authority, fact.ObservedAt.Format(canonicalTimeFormat),
		})
	}
	return renderCSV(header, rows)
}

func renderTaskCSV(facts []TaskExecutionFact) ([]byte, error) {
	header := []string{
		"schema_version", "fact_id", "execution_fact_id", "receipt_id", "task_id",
		"group_id", "hypothesis_occurrence_id", "role", "dimension_json", "runtime_json",
		"profile_json", "agent_json", "provider_json", "model_json", "api_protocol",
		"status", "failure_reason_code", "started_at", "finished_at", "duration_ms",
		"model_turns_started", "model_turns_completed", "tool_calls", "usage_json",
		"provenance_class", "authority", "observed_at",
	}
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		rows = append(rows, []string{
			fact.SchemaVersion, fact.FactID, fact.ExecutionFactID, fact.ReceiptID,
			fact.TaskID, fact.GroupID, optionalString(fact.HypothesisOccurrenceID),
			string(fact.Role), jsonField(fact.Dimension), jsonField(fact.Runtime),
			jsonField(fact.Profile), jsonField(fact.Agent), jsonField(fact.Provider),
			jsonField(fact.Model), string(fact.APIProtocol), string(fact.Status),
			optionalString(fact.FailureReasonCode), fact.StartedAt.Format(canonicalTimeFormat),
			fact.FinishedAt.Format(canonicalTimeFormat), strconv.FormatUint(fact.DurationMS, 10),
			strconv.FormatUint(uint64(fact.ModelTurnsStarted), 10),
			strconv.FormatUint(uint64(fact.ModelTurnsCompleted), 10),
			strconv.FormatUint(uint64(fact.ToolCalls), 10), jsonField(fact.Usage),
			fact.ProvenanceClass, fact.Authority, fact.ObservedAt.Format(canonicalTimeFormat),
		})
	}
	return renderCSV(header, rows)
}

func renderToolCSV(facts []ToolUsageFact) ([]byte, error) {
	header := []string{
		"schema_version", "fact_id", "execution_fact_id", "task_fact_id", "tool_id",
		"invocation_count", "failure_count", "provenance_class", "authority", "observed_at",
	}
	rows := make([][]string, 0, len(facts))
	for _, fact := range facts {
		rows = append(rows, []string{
			fact.SchemaVersion, fact.FactID, fact.ExecutionFactID, fact.TaskFactID,
			fact.ToolID, strconv.FormatUint(uint64(fact.InvocationCount), 10),
			strconv.FormatUint(uint64(fact.FailureCount), 10), fact.ProvenanceClass,
			fact.Authority, fact.ObservedAt.Format(canonicalTimeFormat),
		})
	}
	return renderCSV(header, rows)
}

func renderProjectionCSV(tiles []MetricTile) ([]byte, error) {
	header := []string{
		"tile_id", "metric_id", "metric_version", "definition", "window_start",
		"window_end", "dimensions_json", "sample_size", "availability", "qualifier",
		"value", "unit", "authority", "warnings_json",
	}
	rows := make([][]string, 0, len(tiles))
	for _, tile := range tiles {
		value := ""
		if tile.Value != nil {
			value = strconv.FormatUint(*tile.Value, 10)
		}
		rows = append(rows, []string{
			tile.TileID, tile.MetricID, tile.MetricVersion, tile.Definition,
			tile.Window.StartInclusive.Format(canonicalTimeFormat),
			tile.Window.EndExclusive.Format(canonicalTimeFormat), jsonField(tile.Dimensions),
			strconv.FormatUint(tile.SampleSize, 10), string(tile.Availability),
			string(tile.Qualifier), value, tile.Unit, tile.Authority, jsonField(tile.Warnings),
		})
	}
	return renderCSV(header, rows)
}

const canonicalTimeFormat = "2006-01-02T15:04:05.999999999Z07:00"

func renderCSV(header []string, rows [][]string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write(header); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := writer.Write(row); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func jsonField(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("validated export value cannot be marshaled: %v", err))
	}
	return string(data)
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
