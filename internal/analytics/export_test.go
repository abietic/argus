package analytics

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestFactExportsAreDeterministicAndDeclareNonParquetLimitation(t *testing.T) {
	facts := completeFunnelFactSet()
	reordered := facts
	reordered.ReviewRuns = slices.Clone(facts.ReviewRuns)
	reordered.Findings = slices.Clone(facts.Findings)
	slices.Reverse(reordered.ReviewRuns)
	slices.Reverse(reordered.Findings)

	firstJSON, err := ExportFactSetJSON(facts)
	if err != nil {
		t.Fatalf("ExportFactSetJSON() error = %v", err)
	}
	secondJSON, err := ExportFactSetJSON(reordered)
	if err != nil {
		t.Fatalf("ExportFactSetJSON(reordered) error = %v", err)
	}
	assertExportEqual(t, firstJSON, secondJSON)
	if firstJSON.Manifest.Parquet ||
		!slices.Equal(
			firstJSON.Manifest.Limitations,
			[]string{parquetNotEmittedLimitation},
		) {
		t.Fatalf("JSON manifest hides Parquet limitation: %+v", firstJSON.Manifest)
	}

	firstCSV, err := ExportFactSetCSV(facts)
	if err != nil {
		t.Fatalf("ExportFactSetCSV() error = %v", err)
	}
	secondCSV, err := ExportFactSetCSV(reordered)
	if err != nil {
		t.Fatalf("ExportFactSetCSV(reordered) error = %v", err)
	}
	assertExportEqual(t, firstCSV, secondCSV)
	if len(firstCSV.Files) != 9 {
		t.Fatalf("CSV files = %d, want nine versioned fact tables", len(firstCSV.Files))
	}
	var findingCSV []byte
	for _, file := range firstCSV.Files {
		if file.Manifest.Name == "finding_funnel.csv" {
			findingCSV = file.Data
			break
		}
	}
	if !bytes.Contains(findingCSV, []byte("publication_eligibility")) ||
		!bytes.Contains(findingCSV, []byte("publication_ref_json")) ||
		!bytes.Contains(findingCSV, []byte("publication_at")) {
		t.Fatalf("finding CSV omits eligibility or publication ledger evidence")
	}
}

func TestDashboardExportsPreserveTileDefinitionsAndSamples(t *testing.T) {
	projection, err := ProjectDashboard(completeFunnelFactSet(), nil)
	if err != nil {
		t.Fatalf("ProjectDashboard() error = %v", err)
	}
	jsonExport, err := ExportDashboardJSON(projection)
	if err != nil {
		t.Fatalf("ExportDashboardJSON() error = %v", err)
	}
	csvExport, err := ExportDashboardCSV(projection)
	if err != nil {
		t.Fatalf("ExportDashboardCSV() error = %v", err)
	}
	if jsonExport.Manifest.FactCount != uint64(len(projection.Tiles)) ||
		csvExport.Manifest.FactCount != uint64(len(projection.Tiles)) {
		t.Fatalf("dashboard export row counts do not match tiles")
	}
	if !bytes.Contains(csvExport.Files[0].Data, []byte("metric_version")) ||
		!bytes.Contains(csvExport.Files[0].Data, []byte("definition")) ||
		!bytes.Contains(csvExport.Files[0].Data, []byte("sample_size")) {
		t.Fatalf("dashboard CSV omits mandatory tile metadata")
	}
}

func TestDecodeFactSetJSONRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	facts := completeFunnelFactSet()
	data, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(`{"unknown":true,`), data[1:]...)
	if _, err := DecodeFactSetJSON(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodeFactSetJSON(unknown) error = %v", err)
	}
	trailing := append(slices.Clone(data), []byte(` {}`)...)
	if _, err := DecodeFactSetJSON(trailing); err == nil ||
		!strings.Contains(err.Error(), "trailing JSON value") {
		t.Fatalf("DecodeFactSetJSON(trailing) error = %v", err)
	}
}

func assertExportEqual(t *testing.T, left, right ExportBundle) {
	t.Helper()
	if err := left.Validate(); err != nil {
		t.Fatalf("left export Validate() error = %v", err)
	}
	if err := right.Validate(); err != nil {
		t.Fatalf("right export Validate() error = %v", err)
	}
	leftManifest, err := json.Marshal(left.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	rightManifest, err := json.Marshal(right.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftManifest, rightManifest) {
		t.Fatalf("export manifests differ:\n%s\n%s", leftManifest, rightManifest)
	}
	if len(left.Files) != len(right.Files) {
		t.Fatalf("file counts differ")
	}
	for index := range left.Files {
		if left.Files[index].Manifest != right.Files[index].Manifest ||
			!bytes.Equal(left.Files[index].Data, right.Files[index].Data) {
			t.Fatalf("export file %d differs", index)
		}
	}
}
