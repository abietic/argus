package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentanalytics"
	"github.com/abietic/argus/internal/agentshadow"
	"github.com/abietic/argus/internal/store/local"
)

func TestAgentReviewAnalyticsHelpAndFlagParsing(t *testing.T) {
	var output bytes.Buffer
	if err := runWithIO(
		t.Context(),
		[]string{"agent-review", "analytics", "--help"},
		&output,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "analytics rebuild") ||
		!strings.Contains(output.String(), "diagnostic-only") {
		t.Fatalf("agent analytics help = %q", output.String())
	}

	store := filepath.Join(t.TempDir(), "store")
	options, err := parseAgentAnalyticsRebuildFlags([]string{
		"--store", store,
		"--snapshot", "agent-analytics-1",
		"--start", "2026-08-20T00:00:00Z",
		"--end", "2026-08-21T00:00:00Z",
		"--built-at", "2026-08-21T00:00:00Z",
		"--group-by", "provider",
		"--group-by", "model",
		"--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	groupBy, err := parseAgentAnalyticsGroupBy(options.groupBy)
	if err != nil {
		t.Fatal(err)
	}
	if options.snapshot != "agent-analytics-1" || !options.json ||
		len(groupBy) != 2 || groupBy[0] != agentanalytics.DimensionProvider ||
		groupBy[1] != agentanalytics.DimensionModel {
		t.Fatalf("parsed analytics flags = %+v, group_by=%v", options, groupBy)
	}
	if _, err := parseAgentAnalyticsGroupBy([]string{"provider", "provider"}); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate dimension error = %v", err)
	}
	if _, err := parseAgentAnalyticsGroupBy([]string{"repository"}); err == nil ||
		!strings.Contains(err.Error(), "--group-by") {
		t.Fatalf("unknown dimension error = %v", err)
	}
	if _, err := parseAgentAnalyticsExportFormat("parquet"); err == nil {
		t.Fatal("agent analytics unexpectedly advertised parquet")
	}
}

func TestAgentReviewAnalyticsEmptyWindowLifecycle(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	arguments := []string{
		"agent-review", "analytics", "rebuild",
		"--store", store,
		"--snapshot", "agent-analytics-empty",
		"--start", "2026-08-20T00:00:00Z",
		"--end", "2026-08-21T00:00:00Z",
		"--built-at", "2026-08-21T00:00:00Z",
		"--group-by", "provider",
		"--json",
	}
	var rebuildOutput bytes.Buffer
	if err := runWithIO(t.Context(), arguments, &rebuildOutput); err != nil {
		t.Fatalf("rebuild empty agent analytics window: %v", err)
	}
	var rebuilt agentAnalyticsSnapshotOutput
	if err := json.Unmarshal(rebuildOutput.Bytes(), &rebuilt); err != nil {
		t.Fatalf("decode rebuild output: %v", err)
	}
	if rebuilt.Snapshot.SnapshotID != "agent-analytics-empty" ||
		rebuilt.Snapshot.Scope.TenantID != agentshadow.LocalTenantID ||
		rebuilt.Snapshot.Scope.WorkspaceID != agentshadow.LocalWorkspaceID ||
		len(rebuilt.Snapshot.Facts.Executions) != 0 ||
		len(rebuilt.Snapshot.SourceBindings) != 0 {
		t.Fatalf("rebuilt empty snapshot = %+v", rebuilt.Snapshot)
	}

	// The immutable rebuild identity is deterministic when every input,
	// including built_at, is identical.
	if err := runWithIO(t.Context(), arguments, &bytes.Buffer{}); err != nil {
		t.Fatalf("idempotent analytics rebuild: %v", err)
	}
	var showOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"agent-review", "analytics", "show",
		"--store", store,
		"--snapshot", "agent-analytics-empty",
	}, &showOutput); err != nil {
		t.Fatalf("show analytics snapshot: %v", err)
	}
	if !strings.Contains(showOutput.String(), "executions=0") ||
		!strings.Contains(showOutput.String(), "authority=diagnostic_only") {
		t.Fatalf("show output = %q", showOutput.String())
	}

	exportParent := t.TempDir()
	exportDirectory := filepath.Join(exportParent, "facts-export")
	var exportOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"agent-review", "analytics", "export",
		"--store", store,
		"--snapshot", "agent-analytics-empty",
		"--target", "facts",
		"--format", "json",
		"--output-dir", exportDirectory,
		"--json",
	}, &exportOutput); err != nil {
		t.Fatalf("export analytics facts: %v", err)
	}
	var exported agentAnalyticsExportOutput
	if err := json.Unmarshal(exportOutput.Bytes(), &exported); err != nil {
		t.Fatalf("decode export output: %v", err)
	}
	canonicalExport, err := canonicalAgentAnalyticsExportPath(exportDirectory, store)
	if err != nil {
		t.Fatal(err)
	}
	if exported.Manifest.Format != agentanalytics.CanonicalJSONExportFormat ||
		exported.Target != agentanalytics.ExportFacts ||
		exported.OutputDirectory != canonicalExport ||
		len(exported.SnapshotSHA256) != 64 {
		t.Fatalf("export output = %+v", exported)
	}
	for _, name := range []string{"agent_execution_facts.json", "manifest.json"} {
		info, err := os.Lstat(filepath.Join(exportDirectory, name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("export file %q = %+v, %v", name, info, err)
		}
	}
	manifestData, err := os.ReadFile(filepath.Join(exportDirectory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var publication agentAnalyticsExportPublication
	if err := json.Unmarshal(manifestData, &publication); err != nil {
		t.Fatalf("decode publication manifest: %v", err)
	}
	if publication.SnapshotID != rebuilt.Snapshot.SnapshotID ||
		publication.SnapshotSHA256 != exported.SnapshotSHA256 ||
		publication.Scope != rebuilt.Snapshot.Scope ||
		publication.Target != agentanalytics.ExportFacts ||
		!reflect.DeepEqual(publication.DatasetManifest, exported.Manifest) {
		t.Fatalf("publication manifest does not bind immutable snapshot: %+v", publication)
	}

	// An exact retry validates and reuses the already-published directory.
	if err := runWithIO(t.Context(), []string{
		"agent-review", "analytics", "export",
		"--store", store,
		"--snapshot", "agent-analytics-empty",
		"--target", "facts",
		"--format", "json",
		"--output-dir", exportDirectory,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("idempotent repeat export: %v", err)
	}
}

func TestAgentReviewAnalyticsExportIsRecoverableAndCannotOverlapStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	rebuildEmptyAgentAnalyticsSnapshot(t, store, "agent-analytics-export-safety")

	overlap := filepath.Join(store, "immutable", "poison")
	if err := runWithIO(t.Context(), agentAnalyticsExportArguments(
		store,
		"agent-analytics-export-safety",
		overlap,
	), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("store-overlapping export error = %v", err)
	}
	if _, err := os.Lstat(overlap); !os.IsNotExist(err) {
		t.Fatalf("store-overlapping export created state: %v", err)
	}

	outputDirectory := filepath.Join(t.TempDir(), "recoverable-export")
	arguments := agentAnalyticsExportArguments(
		store,
		"agent-analytics-export-safety",
		outputDirectory,
	)
	err := runWithIO(t.Context(), arguments, closedAgentAnalyticsWriter{})
	if !errors.Is(err, errAgentAnalyticsPublishOutcomeUnknown) {
		t.Fatalf("post-publication output error = %v, want marked unknown outcome", err)
	}
	if info, statErr := os.Lstat(outputDirectory); statErr != nil || !info.IsDir() {
		t.Fatalf("failed acknowledgement did not retain published export: %+v, %v", info, statErr)
	}
	var retryOutput bytes.Buffer
	if err := runWithIO(t.Context(), arguments, &retryOutput); err != nil {
		t.Fatalf("recover export acknowledgement by exact retry: %v", err)
	}
	if !strings.Contains(retryOutput.String(), "snapshot_sha256") {
		t.Fatalf("retry output = %q", retryOutput.String())
	}
}

func TestAgentReviewAnalyticsShowAndExportRejectForeignScope(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "store")
	store, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := agentanalytics.New(emptyAgentAnalyticsSource{}, store)
	if err != nil {
		t.Fatal(err)
	}
	start := mustParseAgentAnalyticsTime(t, "2026-08-20T00:00:00Z")
	end := mustParseAgentAnalyticsTime(t, "2026-08-21T00:00:00Z")
	if _, err := adapter.Rebuild(t.Context(), agentanalytics.RebuildRequest{
		SnapshotID: "foreign-scope",
		Scope: agentanalytics.Scope{
			TenantID: "foreign-tenant", WorkspaceID: "foreign-workspace",
		},
		Window:  agentanalytics.TimeWindow{StartInclusive: start, EndExclusive: end},
		GroupBy: []agentanalytics.DimensionName{},
		BuiltAt: end,
	}); err != nil {
		t.Fatal(err)
	}
	for name, arguments := range map[string][]string{
		"show": {
			"agent-review", "analytics", "show", "--store", storePath,
			"--snapshot", "foreign-scope",
		},
		"export": agentAnalyticsExportArguments(
			storePath,
			"foreign-scope",
			filepath.Join(t.TempDir(), "foreign-export"),
		),
	} {
		t.Run(name, func(t *testing.T) {
			if err := runWithIO(t.Context(), arguments, &bytes.Buffer{}); err == nil ||
				!strings.Contains(err.Error(), "local/local") {
				t.Fatalf("foreign-scope %s error = %v", name, err)
			}
		})
	}
}

func TestPersistAgentAnalyticsExportRejectsCorruptBundle(t *testing.T) {
	facts := agentanalytics.FactSet{
		SchemaVersion: agentanalytics.FactSetSchemaVersion,
		Window: agentanalytics.TimeWindow{
			StartInclusive: mustParseAgentAnalyticsTime(t, "2026-08-20T00:00:00Z"),
			EndExclusive:   mustParseAgentAnalyticsTime(t, "2026-08-21T00:00:00Z"),
		},
		Executions: []agentanalytics.ExecutionFact{},
		Tasks:      []agentanalytics.TaskExecutionFact{},
		ToolUsage:  []agentanalytics.ToolUsageFact{},
	}
	bundle, err := agentanalytics.ExportFactSetJSON(facts)
	if err != nil {
		t.Fatal(err)
	}
	publication := testAgentAnalyticsPublication(bundle)
	bundle.Files[0].Data = append(bundle.Files[0].Data, 'x')
	outputDirectory := filepath.Join(t.TempDir(), "corrupt-export")
	if err := persistAgentAnalyticsExport(outputDirectory, publication, bundle); err == nil ||
		!strings.Contains(err.Error(), "validate") {
		t.Fatalf("corrupt bundle error = %v", err)
	}
	if _, err := os.Lstat(outputDirectory); !os.IsNotExist(err) {
		t.Fatalf("corrupt export left output directory: %v", err)
	}
}

type emptyAgentAnalyticsSource struct{}

func (emptyAgentAnalyticsSource) List(
	context.Context,
	agentshadow.ListRequest,
) ([]agentshadow.Result, error) {
	return []agentshadow.Result{}, nil
}

type closedAgentAnalyticsWriter struct{}

func (closedAgentAnalyticsWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func rebuildEmptyAgentAnalyticsSnapshot(t *testing.T, store string, snapshot string) {
	t.Helper()
	if err := runWithIO(t.Context(), []string{
		"agent-review", "analytics", "rebuild",
		"--store", store,
		"--snapshot", snapshot,
		"--start", "2026-08-20T00:00:00Z",
		"--end", "2026-08-21T00:00:00Z",
		"--built-at", "2026-08-21T00:00:00Z",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("rebuild empty agent analytics snapshot: %v", err)
	}
}

func agentAnalyticsExportArguments(store string, snapshot string, output string) []string {
	return []string{
		"agent-review", "analytics", "export",
		"--store", store,
		"--snapshot", snapshot,
		"--target", "facts",
		"--format", "json",
		"--output-dir", output,
		"--json",
	}
}

func testAgentAnalyticsPublication(
	bundle agentanalytics.ExportBundle,
) agentAnalyticsExportPublication {
	return agentAnalyticsExportPublication{
		SchemaVersion:  agentAnalyticsExportPublicationSchemaVersion,
		SnapshotID:     "test-snapshot",
		SnapshotSHA256: strings.Repeat("a", 64),
		Scope: agentanalytics.Scope{
			TenantID: agentshadow.LocalTenantID, WorkspaceID: agentshadow.LocalWorkspaceID,
		},
		Target: agentanalytics.ExportFacts, DatasetManifest: bundle.Manifest,
	}
}

func mustParseAgentAnalyticsTime(t *testing.T, value string) (resultTime time.Time) {
	t.Helper()
	resultTime, err := parseDashboardTime("test-time", value)
	if err != nil {
		t.Fatal(err)
	}
	return resultTime
}
