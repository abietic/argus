package analyticsadapter

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/parquet-go/parquet-go"
)

func TestExportFactSetParquetRoundTripsDeterministically(t *testing.T) {
	facts := parquetFixtureFactSet()
	if err := facts.Validate(); err != nil {
		t.Fatalf("fixture validation: %v", err)
	}
	first, err := ExportFactSetParquet(facts)
	if err != nil {
		t.Fatalf("ExportFactSetParquet() error = %v", err)
	}
	reordered := facts
	reordered.Experiments = slices.Clone(facts.Experiments)
	slices.Reverse(reordered.Experiments)
	second, err := ExportFactSetParquet(reordered)
	if err != nil {
		t.Fatalf("ExportFactSetParquet(reordered) error = %v", err)
	}
	if !reflect.DeepEqual(first.Manifest, second.Manifest) {
		t.Fatalf("deterministic manifests differ:\n%+v\n%+v", first.Manifest, second.Manifest)
	}
	if len(first.Files) != 9 || len(second.Files) != 9 {
		t.Fatalf("Parquet file counts = %d, %d", len(first.Files), len(second.Files))
	}
	for index := range first.Files {
		if first.Files[index].Manifest != second.Files[index].Manifest ||
			!bytes.Equal(first.Files[index].Data, second.Files[index].Data) {
			t.Fatalf("Parquet file %d is not deterministic", index)
		}
	}
	if !first.Manifest.Parquet ||
		first.Manifest.Format != analytics.CanonicalParquetExportFormat ||
		first.Manifest.Limitations == nil ||
		len(first.Manifest.Limitations) != 0 ||
		first.Manifest.FactCount != 8 {
		t.Fatalf("Parquet manifest = %+v", first.Manifest)
	}
	expectedContracts := map[string]string{
		"context_providers.parquet":  analytics.ContextProviderParquetSchemaVersion,
		"experiments.parquet":        analytics.ExperimentParquetSchemaVersion,
		"repeatability.parquet":      analytics.RepeatabilityParquetSchemaVersion,
		"feedback_outcomes.parquet":  analytics.FeedbackOutcomeParquetSchemaVersion,
		"finding_funnel.parquet":     analytics.FindingFunnelParquetSchemaVersion,
		"finding_lineages.parquet":   analytics.FindingLineageParquetSchemaVersion,
		"review_runs.parquet":        analytics.ReviewRunParquetSchemaVersion,
		"stages.parquet":             analytics.StageParquetSchemaVersion,
		"value_observations.parquet": analytics.ValueObservationParquetSchemaVersion,
	}
	for _, file := range first.Files {
		if file.Manifest.Contract != expectedContracts[file.Manifest.Name] ||
			file.Manifest.Ref != file.Manifest.Name ||
			file.Manifest.MediaType != parquetMediaType ||
			file.Manifest.SizeBytes != int64(len(file.Data)) ||
			file.Manifest.SHA256 != digestBytes(file.Data) {
			t.Fatalf("Parquet table manifest = %+v", file.Manifest)
		}
		opened, err := parquet.OpenFile(
			bytes.NewReader(file.Data),
			int64(len(file.Data)),
			parquet.SkipPageIndex(true),
			parquet.SkipBloomFilters(true),
		)
		if err != nil {
			t.Fatalf("open %s: %v", file.Manifest.Name, err)
		}
		for _, column := range opened.Schema().Columns() {
			if len(column) != 1 {
				t.Fatalf("%s contains nested column %v", file.Manifest.Name, column)
			}
		}
	}
	reviewRows, err := reviewRunParquetRows(facts.ReviewRuns)
	if err != nil {
		t.Fatal(err)
	}
	if reviewRows[0].StartedAtMicros != facts.ReviewRuns[0].StartedAt.UnixMicro() ||
		reviewRows[0].OccurredAtMicros != facts.ReviewRuns[0].OccurredAt.UnixMicro() {
		t.Fatalf("UTC microsecond mapping = %+v", reviewRows[0])
	}
	reviewFile := exportFileByName(t, first, "review_runs.parquet")
	path := filepath.Join(t.TempDir(), "review_runs.parquet")
	if err := os.WriteFile(path, reviewFile.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	roundTripped, err := verifyParquetFile(path, reviewRows)
	if err != nil {
		t.Fatalf("verifyParquetFile() error = %v", err)
	}
	if !bytes.Equal(roundTripped, reviewFile.Data) {
		t.Fatal("verified bytes differ from manifest payload")
	}
	providerRows, err := contextProviderParquetRows(facts.ContextProviders)
	if err != nil {
		t.Fatal(err)
	}
	providerFile := exportFileByName(t, first, "context_providers.parquet")
	providerPath := filepath.Join(t.TempDir(), "context_providers.parquet")
	if err := os.WriteFile(providerPath, providerFile.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyParquetFile(providerPath, providerRows); err != nil {
		t.Fatalf("verify context provider Parquet: %v", err)
	}
	if providerRows[0].BindingMode != "executed" ||
		providerRows[0].DurationMicros != 42_000 ||
		providerRows[0].Authority != "local_host_observation" {
		t.Fatalf("context provider row = %+v", providerRows[0])
	}
}

func TestExportFactSetParquetEmitsNineValidEmptyTables(t *testing.T) {
	facts := emptyParquetFactSet()
	bundle, err := ExportFactSetParquet(facts)
	if err != nil {
		t.Fatalf("ExportFactSetParquet(empty) error = %v", err)
	}
	if bundle.Manifest.FactCount != 0 || len(bundle.Files) != 9 {
		t.Fatalf("empty Parquet bundle = %+v", bundle.Manifest)
	}
	for _, file := range bundle.Files {
		if file.Manifest.RowCount != 0 || len(file.Data) == 0 {
			t.Fatalf("empty table manifest = %+v", file.Manifest)
		}
		if err := preflightParquetFile(file.Data); err != nil {
			t.Fatalf("preflight %s: %v", file.Manifest.Name, err)
		}
		opened, err := parquet.OpenFile(
			bytes.NewReader(file.Data),
			int64(len(file.Data)),
			parquet.SkipPageIndex(true),
			parquet.SkipBloomFilters(true),
		)
		if err != nil {
			t.Fatalf("open empty %s: %v", file.Manifest.Name, err)
		}
		if opened.NumRows() != 0 || len(opened.RowGroups()) != 0 {
			t.Fatalf(
				"empty %s rows=%d groups=%d",
				file.Manifest.Name,
				opened.NumRows(),
				len(opened.RowGroups()),
			)
		}
		if len(opened.Schema().Columns()) == 0 {
			t.Fatalf("empty %s lost its strong schema", file.Manifest.Name)
		}
	}
}

func TestVerifyParquetFileRejectsCorruptAndOversizedFooter(t *testing.T) {
	facts := parquetFixtureFactSet()
	bundle, err := ExportFactSetParquet(facts)
	if err != nil {
		t.Fatal(err)
	}
	reviewFile := exportFileByName(t, bundle, "review_runs.parquet")
	expected, err := reviewRunParquetRows(facts.ReviewRuns)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*testing.T, []byte){
		"bad header": func(_ *testing.T, data []byte) {
			copy(data[:4], []byte("NOPE"))
		},
		"data page": func(t *testing.T, data []byte) {
			t.Helper()
			footerSize := int(binary.LittleEndian.Uint32(data[len(data)-8 : len(data)-4]))
			dataEnd := len(data) - 8 - footerSize
			needle := []byte(facts.ReviewRuns[0].FactID)
			mutated := 0
			for start := 0; start < dataEnd; {
				offset := bytes.Index(data[start:dataEnd], needle)
				if offset < 0 {
					break
				}
				offset += start
				data[offset] ^= 0xff
				mutated++
				start = offset + len(needle)
			}
			if mutated == 0 {
				t.Fatal("fixture fact_id not found in the uncompressed data page")
			}
		},
		"footer exceeds file": func(_ *testing.T, data []byte) {
			binary.LittleEndian.PutUint32(data[len(data)-8:len(data)-4], ^uint32(0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := slices.Clone(reviewFile.Data)
			mutate(t, data)
			path := filepath.Join(t.TempDir(), "corrupt.parquet")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyParquetFile(path, expected); err == nil {
				t.Fatal("verifyParquetFile() accepted corrupt input")
			}
		})
	}
	t.Run("file budget", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oversized.parquet")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxParquetFileBytes + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyParquetFile(
			path,
			[]reviewRunParquetRow{},
		); err == nil || !strings.Contains(err.Error(), "verification budget") {
			t.Fatalf("oversized verification error = %v", err)
		}
	})
}

func TestParquetManifestRejectsCapabilityContractAndRefTampering(t *testing.T) {
	bundle, err := ExportFactSetParquet(emptyParquetFactSet())
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*analytics.ExportBundle){
		"capability": func(bundle *analytics.ExportBundle) {
			bundle.Manifest.Parquet = false
		},
		"contract": func(bundle *analytics.ExportBundle) {
			bundle.Manifest.Files[0].Contract = "argus.forged.v1"
		},
		"ref": func(bundle *analytics.ExportBundle) {
			bundle.Manifest.Files[0].Ref = "../escape.parquet"
		},
	} {
		t.Run(name, func(t *testing.T) {
			tampered := bundle
			tampered.Manifest.Files = slices.Clone(bundle.Manifest.Files)
			tampered.Files = slices.Clone(bundle.Files)
			mutate(&tampered)
			if err := tampered.Validate(); err == nil {
				t.Fatal("ExportBundle.Validate() accepted tampered Parquet manifest")
			}
		})
	}
}

func exportFileByName(
	t *testing.T,
	bundle analytics.ExportBundle,
	name string,
) analytics.ExportFile {
	t.Helper()
	for _, file := range bundle.Files {
		if file.Manifest.Name == name {
			return file
		}
	}
	t.Fatalf("export file %q not found", name)
	return analytics.ExportFile{}
}

func emptyParquetFactSet() analytics.FactSet {
	return analytics.FactSet{
		SchemaVersion: analytics.FactSetSchemaVersion,
		Window: analytics.TimeWindow{
			StartInclusive: parquetTime(1, 0),
			EndExclusive:   parquetTime(31, 0),
		},
		Completeness:      analytics.CompletenessComplete,
		IncompleteReasons: []string{},
		ReviewRuns:        []analytics.ReviewRunFact{},
		Stages:            []analytics.StageFact{},
		ContextProviders:  []analytics.ContextProviderFact{},
		Findings:          []analytics.FindingFunnelFact{},
		FeedbackOutcomes:  []analytics.FeedbackOutcomeFact{},
		FindingLineages:   []analytics.FindingLineageFact{},
		Experiments:       []analytics.ExperimentFact{},
		Repeatability:     []analytics.RepeatabilityFact{},
		ValueObservations: []analytics.ValueObservation{},
	}
}

func parquetFixtureFactSet() analytics.FactSet {
	facts := emptyParquetFactSet()
	dimensions := analytics.Dimensions{
		TenantID: "tenant-1", OrganizationID: "organization-1",
		RepositoryID: "repository-1", Language: "go", RuleID: "rule.logic",
		Path: "internal/review.go", WorkflowRevision: "workflow-v1",
		ConfigRevision: "config-v1",
	}
	started := parquetTime(2, 10)
	finished := parquetTime(2, 11)
	facts.ReviewRuns = []analytics.ReviewRunFact{{
		SchemaVersion: analytics.ReviewRunFactSchemaVersion,
		FactID:        "run-fact-1", RunID: "run-1", Dimensions: dimensions,
		Status:             analytics.RunStatusSucceeded,
		ResultCompleteness: analytics.CompletenessComplete,
		IncompleteReasons:  []string{},
		Funnel: &analytics.FunnelSummary{
			Candidates: 1, Normalized: 1, Verified: 1, Published: 1,
		},
		StartedAt: started, FinishedAt: &finished, OccurredAt: finished,
	}}
	facts.Stages = []analytics.StageFact{{
		SchemaVersion: analytics.StageFactSchemaVersion,
		FactID:        "stage-fact-1", RunID: "run-1", StageID: "verify",
		StageRevision: "stage-v1", Attempt: 1, Dimensions: dimensions,
		Status:             analytics.StageStatusSucceeded,
		ResultCompleteness: analytics.CompletenessComplete,
		IncompleteReasons:  []string{}, DurationMicros: 1_250,
		OccurredAt: finished,
	}}
	facts.ContextProviders = []analytics.ContextProviderFact{{
		SchemaVersion: analytics.ContextProviderFactSchemaVersion,
		FactID:        "context-provider-fact-1", RunID: "run-1", ReceiptID: "receipt-1",
		ReceiptSHA256:         strings.Repeat("1", 64),
		ReceiptArtifactSHA256: strings.Repeat("2", 64),
		ProviderID:            "go-ast-exact", ProviderRevision: "1", Kind: "go_ast",
		AdapterID: "argus-go-ast", AdapterRevision: "1",
		AdapterSHA256: strings.Repeat("3", 64), RequestSHA256: strings.Repeat("4", 64),
		BindingMode: analytics.ContextProviderExecuted,
		Status:      analytics.ContextProviderSucceeded, ContextID: "context-1",
		ContextDigest:   strings.Repeat("5", 64),
		ContextContract: "argus.context.go_ast.v1alpha1", TargetPathCount: 2,
		TimeoutMicros: 30_000_000, DurationMicros: 42_000,
		Authority: "local_host_observation", Dimensions: dimensions, OccurredAt: finished,
	}}
	facts.Findings = []analytics.FindingFunnelFact{{
		SchemaVersion: analytics.FindingFunnelFactSchemaVersion,
		FactID:        "finding-fact-1", RunID: "run-1", CandidateID: "candidate-1",
		FindingID: "finding-1", Normalized: true,
		Verification:           analytics.VerificationVerified,
		PublicationEligibility: analytics.EligibilityPublishEligible,
		Publication:            analytics.PublicationPublished,
		PublicationRef: parquetSource(
			analytics.SourcePublication,
			"publication-1",
			"published",
			'a',
		),
		PublicationAt:      &finished,
		ResultCompleteness: analytics.CompletenessComplete,
		IncompleteReasons:  []string{}, Dimensions: dimensions, OccurredAt: finished,
	}}
	feedbackAt := parquetTime(2, 12)
	outcomeAt := parquetTime(2, 13)
	facts.FeedbackOutcomes = []analytics.FeedbackOutcomeFact{{
		SchemaVersion: analytics.FeedbackOutcomeFactSchemaVersion,
		FactID:        "feedback-outcome-fact-1", RunID: "run-1", FindingID: "finding-1",
		Feedback: analytics.FeedbackAccepted,
		FeedbackRef: parquetSource(
			analytics.SourceFeedback, "feedback-1", "revision-1", 'b',
		),
		FeedbackAt: &feedbackAt, Outcome: analytics.OutcomeFixed,
		OutcomeRef: parquetSource(
			analytics.SourceOutcome, "outcome-1", "revision-1", 'c',
		),
		OutcomeAt: &outcomeAt, ResultCompleteness: analytics.CompletenessComplete,
		IncompleteReasons: []string{}, Dimensions: dimensions,
		OccurredAt: parquetTime(3, 10),
	}}
	experimentBase := analytics.ExperimentFact{
		SchemaVersion: analytics.ExperimentFactSchemaVersion,
		FactID:        "experiment-baseline-fact", ExperimentID: "experiment-1",
		ExperimentRevision: "revision-1", Arm: analytics.ExperimentBaseline,
		VariantID: "baseline", MetricID: "published_precision",
		MetricVersion:    "metric-v1",
		MetricDefinition: "Accepted publications divided by sampled publications.",
		Direction:        analytics.HigherIsBetter,
		Value:            analytics.FixedPoint{Amount: 8_000, Scale: 4, Unit: "ratio"},
		SampleSize:       100,
		Window: analytics.TimeWindow{
			StartInclusive: parquetTime(1, 0), EndExclusive: parquetTime(3, 0),
		},
		ResultCompleteness: analytics.CompletenessComplete,
		IncompleteReasons:  []string{}, Dimensions: dimensions,
		SourceRefs: []analytics.SourceRef{*parquetSource(
			analytics.SourceExperiment, "experiment-1", "revision-1", 'd',
		)},
		OccurredAt: parquetTime(4, 0),
	}
	experimentVariant := experimentBase
	experimentVariant.FactID = "experiment-variant-fact"
	experimentVariant.Arm = analytics.ExperimentVariant
	experimentVariant.VariantID = "candidate"
	experimentVariant.Value.Amount = 8_500
	experimentVariant.SourceRefs = []analytics.SourceRef{*parquetSource(
		analytics.SourceExperiment, "experiment-1-candidate", "revision-1", 'e',
	)}
	facts.Experiments = []analytics.ExperimentFact{
		experimentBase,
		experimentVariant,
	}
	unit := func(amount int64) analytics.FixedPoint {
		return analytics.FixedPoint{Amount: amount, Scale: 0, Unit: "CNY_micros"}
	}
	facts.ValueObservations = []analytics.ValueObservation{{
		SchemaVersion: analytics.ValueObservationSchemaVersion,
		ObservationID: "value-observation-E2", Dimensions: dimensions,
		MetricID: "net_review_value", EvidenceTier: analytics.EvidenceTierE2,
		Baseline: analytics.Baseline{
			CohortID: "baseline-cohort", Revision: "baseline-v1",
			Window: analytics.TimeWindow{
				StartInclusive: parquetTime(1, 0).AddDate(0, -1, 0),
				EndExclusive:   parquetTime(1, 0),
			},
			Value: unit(600), SampleSize: 100,
			SourceRefs: []analytics.SourceRef{*parquetSource(
				analytics.SourceBaseline, "baseline-1", "baseline-v1", 'a',
			)},
		},
		ObservationWindow: analytics.TimeWindow{
			StartInclusive: parquetTime(1, 0), EndExclusive: parquetTime(4, 0),
		},
		AttributionWindow: analytics.TimeWindow{
			StartInclusive: parquetTime(1, 0), EndExclusive: parquetTime(5, 0),
		},
		SampleSize: 20, GrossValue: unit(1_000),
		Costs: analytics.CostBreakdown{
			ModelCompute: unit(50), HumanVerification: unit(50),
			FalsePositive: unit(40), PlatformOperations: unit(40), Storage: unit(20),
		},
		NetValue: unit(800),
		NetValueUncertainty: analytics.Uncertainty{
			Lower: unit(600), Upper: unit(1_000), ConfidenceBPS: 9_500,
			Method: "bootstrap", Revision: "uncertainty-v1",
		},
		SourceRefs: []analytics.SourceRef{*parquetSource(
			analytics.SourceOutcome, "tier-source-E2", "revision-1", 'f',
		)},
		AttributionPolicyRevision: "attribution-v1",
		ObservedAt:                parquetTime(5, 0),
	}}
	return facts
}

func parquetSource(
	kind analytics.SourceKind,
	id string,
	revision string,
	digestByte byte,
) *analytics.SourceRef {
	return &analytics.SourceRef{
		Kind: kind, ID: id, Revision: revision,
		SHA256: strings.Repeat(string(digestByte), 64),
	}
}

func parquetTime(day, hour int) time.Time {
	return time.Date(2026, time.July, day, hour, 0, 0, 123_456_000, time.UTC)
}
