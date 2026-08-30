package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/analyticsadapter"
)

func TestDashboardCLIRebuildShowAndExportSnapshotOnly(t *testing.T) {
	store := t.TempDir()
	rebuildArguments := []string{
		"dashboard", "rebuild",
		"--store", store,
		"--snapshot", "dashboard-snapshot-1",
		"--tenant", "local",
		"--organization", "local",
		"--repository", "local",
		"--start", "2026-07-27T08:00:00Z",
		"--end", "2026-07-27T10:00:00Z",
		"--built-at", "2026-07-27T11:00:00Z",
		"--json",
	}
	var output bytes.Buffer
	if err := runWithIO(t.Context(), rebuildArguments, &output); err != nil {
		t.Fatalf("dashboard rebuild error = %v", err)
	}
	var rebuilt dashboardSnapshotOutput
	if err := json.Unmarshal(output.Bytes(), &rebuilt); err != nil {
		t.Fatalf("decode rebuild output: %v\n%s", err, output.String())
	}
	if rebuilt.Snapshot.SnapshotID != "dashboard-snapshot-1" ||
		!rebuilt.ParquetSupported ||
		len(rebuilt.Snapshot.Dashboard.Tiles) == 0 {
		t.Fatalf("rebuild output = %+v", rebuilt)
	}
	if len(rebuilt.Snapshot.Coverage) != 8 ||
		rebuilt.Snapshot.Coverage[5].Source !=
			analyticsadapter.CoveragePublication ||
		rebuilt.Snapshot.Coverage[5].Completeness !=
			analytics.CompletenessComplete {
		t.Fatalf("publication ledger was not wired into rebuild: %+v",
			rebuilt.Snapshot.Coverage)
	}

	// Query commands must remain available when the source ledgers disappear.
	// local.Open recreates an empty streams directory; a hidden old directory
	// would catch any accidental attempt to rebuild facts during show/export.
	streams := filepath.Join(store, "streams")
	if err := os.Rename(streams, filepath.Join(store, "source-ledgers-offline")); err != nil {
		t.Fatalf("take source ledgers offline: %v", err)
	}
	output.Reset()
	if err := runWithIO(t.Context(), []string{
		"dashboard", "show",
		"--store", store,
		"--snapshot", "dashboard-snapshot-1",
		"--json",
	}, &output); err != nil {
		t.Fatalf("dashboard show error = %v", err)
	}
	var shown dashboardSnapshotOutput
	if err := json.Unmarshal(output.Bytes(), &shown); err != nil {
		t.Fatalf("decode show output: %v", err)
	}
	if shown.Snapshot.SnapshotID != rebuilt.Snapshot.SnapshotID ||
		len(shown.Snapshot.Dashboard.Tiles) != len(rebuilt.Snapshot.Dashboard.Tiles) {
		t.Fatalf("shown snapshot = %+v, rebuilt = %+v", shown, rebuilt)
	}

	for _, test := range []struct {
		format string
		target string
		files  []string
	}{
		{"json", "dashboard", []string{"dashboard.json", "manifest.json"}},
		{
			"csv",
			"facts",
			[]string{
				"context_providers.csv",
				"experiments.csv",
				"feedback_outcomes.csv",
				"finding_funnel.csv",
				"manifest.json",
				"repeatability.csv",
				"review_runs.csv",
				"stages.csv",
				"value_observations.csv",
			},
		},
		{
			"parquet",
			"facts",
			[]string{
				"context_providers.parquet",
				"experiments.parquet",
				"feedback_outcomes.parquet",
				"finding_funnel.parquet",
				"manifest.json",
				"repeatability.parquet",
				"review_runs.parquet",
				"stages.parquet",
				"value_observations.parquet",
			},
		},
	} {
		exportDirectory := filepath.Join(t.TempDir(), "export-"+test.format)
		output.Reset()
		if err := runWithIO(t.Context(), []string{
			"dashboard", "export",
			"--store", store,
			"--snapshot", "dashboard-snapshot-1",
			"--target", test.target,
			"--format", test.format,
			"--output-dir", exportDirectory,
			"--json",
		}, &output); err != nil {
			t.Fatalf("dashboard export %s error = %v", test.format, err)
		}
		var exported dashboardExportOutput
		if err := json.Unmarshal(output.Bytes(), &exported); err != nil {
			t.Fatalf("decode export output: %v", err)
		}
		if !exported.ParquetSupported {
			t.Fatalf("export capability = %+v", exported)
		}
		if test.format == "parquet" {
			if !exported.Manifest.Parquet ||
				len(exported.Manifest.Limitations) != 0 ||
				len(exported.Manifest.Files) != 9 {
				t.Fatalf("Parquet export capability = %+v", exported)
			}
			for _, file := range exported.Manifest.Files {
				if file.Contract == "" || file.Ref != file.Name {
					t.Fatalf("Parquet file manifest = %+v", file)
				}
			}
		} else if exported.Manifest.Parquet ||
			len(exported.Manifest.Limitations) != 1 ||
			exported.Manifest.Limitations[0] !=
				"parquet_not_emitted_no_runtime_dependency" {
			t.Fatalf("non-Parquet export capability = %+v", exported)
		}
		for _, name := range test.files {
			if _, err := os.Stat(filepath.Join(exportDirectory, name)); err != nil {
				t.Fatalf("export file %q: %v", name, err)
			}
		}
		manifestData, err := os.ReadFile(filepath.Join(exportDirectory, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest analytics.ExportManifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(manifest, exported.Manifest) {
			t.Fatalf("persisted manifest = %+v, output = %+v", manifest, exported.Manifest)
		}
	}
}

func TestDashboardCLIRejectsImplicitStateAndParquetDashboardTarget(t *testing.T) {
	var output bytes.Buffer
	err := runWithIO(t.Context(), []string{
		"dashboard", "show", "--snapshot", "missing",
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "--store is required") {
		t.Fatalf("implicit state error = %v", err)
	}

	store := t.TempDir()
	if err := runWithIO(t.Context(), []string{
		"dashboard", "rebuild",
		"--store", store,
		"--snapshot", "dashboard-snapshot-parquet-target",
		"--tenant", "local",
		"--organization", "local",
		"--repository", "local",
		"--start", "2026-07-27T08:00:00Z",
		"--end", "2026-07-27T10:00:00Z",
		"--built-at", "2026-07-27T11:00:00Z",
	}, &output); err != nil {
		t.Fatal(err)
	}
	err = runWithIO(t.Context(), []string{
		"dashboard", "export",
		"--store", store,
		"--snapshot", "dashboard-snapshot-parquet-target",
		"--target", "dashboard",
		"--format", "parquet",
		"--output-dir", filepath.Join(t.TempDir(), "parquet"),
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "only for the facts target") {
		t.Fatalf("parquet error = %v", err)
	}
}

func TestDashboardHelpDeclaresSnapshotAndParquetSemantics(t *testing.T) {
	var output bytes.Buffer
	if err := runWithIO(t.Context(), []string{"dashboard", "--help"}, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"immutable projection snapshots only",
		"Parquet export is available",
		"dashboard rebuild",
		"dashboard export",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("dashboard help missing %q:\n%s", expected, output.String())
		}
	}
}

func TestPersistDashboardExportRejectsCorruptBundleWithoutPublishing(t *testing.T) {
	facts := analytics.FactSet{
		SchemaVersion: analytics.FactSetSchemaVersion,
		Window: analytics.TimeWindow{
			StartInclusive: mustDashboardTime(t, "2026-07-27T08:00:00Z"),
			EndExclusive:   mustDashboardTime(t, "2026-07-27T10:00:00Z"),
		},
		Completeness:      analytics.CompletenessComplete,
		IncompleteReasons: []string{},
		ReviewRuns:        []analytics.ReviewRunFact{},
		Stages:            []analytics.StageFact{},
		ContextProviders:  []analytics.ContextProviderFact{},
		Findings:          []analytics.FindingFunnelFact{},
		FeedbackOutcomes:  []analytics.FeedbackOutcomeFact{},
		Experiments:       []analytics.ExperimentFact{},
		Repeatability:     []analytics.RepeatabilityFact{},
		ValueObservations: []analytics.ValueObservation{},
		FindingLineages:   []analytics.FindingLineageFact{},
	}
	bundle, err := analytics.ExportFactSetJSON(facts)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Files[0].Data[0] ^= 0xff
	outputDirectory := filepath.Join(t.TempDir(), "must-not-publish")
	if err := persistDashboardExport(outputDirectory, bundle); err == nil {
		t.Fatal("persistDashboardExport() accepted corrupt bundle")
	}
	if _, err := os.Lstat(outputDirectory); !os.IsNotExist(err) {
		t.Fatalf("corrupt export published output: %v", err)
	}
}

func TestRenameDashboardDirectoryNoReplaceDoesNotClobberTarget(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(source, "source-marker"),
		[]byte("source"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(target, "target-marker"),
		[]byte("target"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := renameDashboardDirectoryNoReplace(source, target); err == nil {
		t.Fatal("no-replace rename unexpectedly replaced an existing target")
	}
	for _, path := range []string{
		filepath.Join(source, "source-marker"),
		filepath.Join(target, "target-marker"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("no-replace rename changed %q: %v", path, err)
		}
	}
}

func mustDashboardTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UTC()
}
