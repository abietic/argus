package analyticsadapter

import (
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/evaluation"
)

func TestUsageExperimentFactPairProjectsExactDiagnosticTokenDelta(t *testing.T) {
	window := analytics.TimeWindow{
		StartInclusive: time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC),
	}
	dimensions := analytics.Dimensions{
		TenantID: "tenant-1", OrganizationID: "local", RepositoryID: "repo-1",
		WorkflowRevision: "workflow-1", ConfigRevision: "config-1",
	}
	group := &usageExperimentGroup{
		dimensions: dimensions, comparisons: 2, complete: true,
		baselineSources: []analytics.SourceRef{{
			Kind: analytics.SourceEvaluation, ID: "evaluation-baseline",
			Revision: "evaluator-1", SHA256: strings.Repeat("a", 64),
		}},
		variantSources: []analytics.SourceRef{{
			Kind: analytics.SourceEvaluation, ID: "evaluation-variant",
			Revision: "evaluator-1", SHA256: strings.Repeat("b", 64),
		}},
	}
	pair, err := usageExperimentFactPair(
		evaluation.ExperimentRun{
			ExperimentRunID: "experiment-1", ExperimentRevision: "revision-1",
			VariantEvaluationRunID: "evaluation-variant",
			RecordedAt:             time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
		},
		window,
		group,
		"total_tokens",
		"Total diagnostic tokens across the exact experiment case set.",
		300,
		240,
	)
	if err != nil {
		t.Fatal(err)
	}
	facts := analytics.FactSet{
		SchemaVersion: analytics.FactSetSchemaVersion,
		Window:        window, Completeness: analytics.CompletenessComplete,
		IncompleteReasons: []string{}, ReviewRuns: []analytics.ReviewRunFact{},
		Stages: []analytics.StageFact{}, ContextProviders: []analytics.ContextProviderFact{},
		Findings:         []analytics.FindingFunnelFact{},
		FeedbackOutcomes: []analytics.FeedbackOutcomeFact{}, Experiments: pair,
		FindingLineages:   []analytics.FindingLineageFact{},
		Repeatability:     []analytics.RepeatabilityFact{},
		ValueObservations: []analytics.ValueObservation{},
	}
	projection, err := analytics.ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tile := range projection.Tiles {
		if tile.MetricID == "experiment.total_tokens.delta" {
			found = tile.Availability == analytics.TileObserved && tile.Value != nil &&
				tile.Value.Amount == -60 && tile.Value.Unit == "tokens"
		}
	}
	if !found {
		t.Fatalf("diagnostic token delta tile missing: %+v", projection.Tiles)
	}

	group.complete = false
	partial, err := usageExperimentFactPair(
		evaluation.ExperimentRun{
			ExperimentRunID: "experiment-2", ExperimentRevision: "revision-1",
			VariantEvaluationRunID: "evaluation-variant",
			RecordedAt:             time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
		},
		window,
		group,
		"total_tokens",
		"Total diagnostic tokens across the exact experiment case set.",
		300,
		240,
	)
	if err != nil || partial[0].ResultCompleteness != analytics.CompletenessPartial ||
		len(partial[0].IncompleteReasons) != 1 {
		t.Fatalf("partial usage facts = %+v, %v", partial, err)
	}
}

func TestDimensionExperimentFactPairKeepsGovernedDimensionAndUnits(t *testing.T) {
	window := analytics.TimeWindow{
		StartInclusive: time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC),
	}
	group := &usageExperimentGroup{
		dimensions: analytics.Dimensions{
			TenantID: "tenant-1", OrganizationID: "local", RepositoryID: "repo-1",
			WorkflowRevision: "workflow-1", ConfigRevision: "config-1",
			ReviewDimension: "correctness",
		},
		comparisons: 1,
		complete:    true,
		baselineSources: []analytics.SourceRef{{
			Kind: analytics.SourceEvaluation, ID: "evaluation-baseline",
			Revision: "evaluator-1", SHA256: strings.Repeat("a", 64),
		}},
		variantSources: []analytics.SourceRef{{
			Kind: analytics.SourceEvaluation, ID: "evaluation-variant",
			Revision: "evaluator-1", SHA256: strings.Repeat("b", 64),
		}},
	}
	pair, err := dimensionExperimentFactPair(
		evaluation.ExperimentRun{
			ExperimentRunID: "experiment-dimension", ExperimentRevision: "revision-1",
			VariantEvaluationRunID: "evaluation-variant",
			RecordedAt:             time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
		},
		window,
		group,
		"review_dimension_cumulative_duration_ms",
		"Exact attributable duration.",
		"ms",
		500,
		420,
		true,
		"review_dimension_execution_incomplete",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(pair) != 2 || pair[0].Dimensions.ReviewDimension != "correctness" ||
		pair[0].Value.Unit != "ms" || pair[0].Value.Amount != 500 ||
		pair[1].Value.Amount != 420 || pair[0].FactID == pair[1].FactID {
		t.Fatalf("dimension fact pair = %+v", pair)
	}
	if err := pair[0].Validate(); err != nil {
		t.Fatalf("baseline dimension fact validation = %v", err)
	}
	rows, err := experimentParquetRows(pair)
	if err != nil || len(rows) != 2 || rows[0].ReviewDimension != "correctness" ||
		rows[1].ReviewDimension != "correctness" {
		t.Fatalf("dimension parquet rows = %+v, %v", rows, err)
	}
}

func TestDimensionRateFactUsesExactRawDenominatorAndFailsClosedAtZero(t *testing.T) {
	window := analytics.TimeWindow{
		StartInclusive: time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC),
	}
	group := &usageExperimentGroup{
		dimensions: analytics.Dimensions{
			TenantID: "tenant-1", OrganizationID: "local", RepositoryID: "repo-1",
			WorkflowRevision: "workflow-1", ConfigRevision: "config-1",
			ReviewDimension: "correctness",
		},
		comparisons: 1, dimensionNormalizationComplete: true,
		baselineRawCandidates: 4, variantRawCandidates: 4,
		baselineSources: []analytics.SourceRef{{
			Kind: analytics.SourceEvaluation, ID: "evaluation-baseline",
			Revision: "evaluator-1", SHA256: strings.Repeat("a", 64),
		}},
		variantSources: []analytics.SourceRef{{
			Kind: analytics.SourceEvaluation, ID: "evaluation-variant",
			Revision: "evaluator-1", SHA256: strings.Repeat("b", 64),
		}},
	}
	pair, err := dimensionRateExperimentFactPair(
		evaluation.ExperimentRun{
			ExperimentRunID: "experiment-rate", ExperimentRevision: "revision-1",
			VariantEvaluationRunID: "evaluation-variant",
			RecordedAt:             time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
		},
		window, group,
		"review_dimension_merged_duplicate_rate",
		"Exact raw-candidate denominator rate.",
		2, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(pair) != 2 || pair[0].Value.Amount != 500_000 ||
		pair[1].Value.Amount != 250_000 || pair[0].Value.Scale != 6 ||
		pair[0].Value.Unit != "ratio" ||
		pair[0].ResultCompleteness != analytics.CompletenessComplete {
		t.Fatalf("dimension rate facts = %+v", pair)
	}
	for _, fact := range pair {
		if err := fact.Validate(); err != nil {
			t.Fatalf("dimension rate fact validation = %v", err)
		}
	}

	group.variantRawCandidates = 0
	partial, err := dimensionRateExperimentFactPair(
		evaluation.ExperimentRun{
			ExperimentRunID: "experiment-rate-zero", ExperimentRevision: "revision-1",
			VariantEvaluationRunID: "evaluation-variant",
			RecordedAt:             time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
		},
		window, group,
		"review_dimension_merged_duplicate_rate",
		"Exact raw-candidate denominator rate.",
		2, 0,
	)
	if err != nil || partial[1].ResultCompleteness != analytics.CompletenessPartial ||
		len(partial[1].IncompleteReasons) != 1 ||
		partial[1].IncompleteReasons[0] != "review_dimension_raw_candidate_denominator_zero" {
		t.Fatalf("zero-denominator dimension rate = %+v, error=%v", partial, err)
	}
}

func TestRepeatabilityFactStaysDistinctAndProjectsAvailability(t *testing.T) {
	window := analytics.TimeWindow{
		StartInclusive: time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC),
		EndExclusive:   time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC),
	}
	run := evaluation.RepeatabilityRun{
		RepeatabilityRunID: "repeatability-1", RepeatabilityRevision: "stability-v1",
		RecordedAt: time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
	}
	dimensions := analytics.Dimensions{
		TenantID: "tenant-1", OrganizationID: "local", RepositoryID: "repo-1",
		WorkflowRevision: "workflow-1", ConfigRevision: "config-1", ReviewDimension: "correctness",
	}
	sources := canonicalAnalyticsSourceRefs([]analytics.SourceRef{
		{Kind: analytics.SourceRepeatability, ID: "repeatability-1", Revision: "stability-v1", SHA256: strings.Repeat("b", 64)},
		{Kind: analytics.SourceEvaluation, ID: "evaluation-1", Revision: "evaluator-1", SHA256: strings.Repeat("a", 64)},
	})
	fact, err := buildRepeatabilityFact(
		run, "case-1", dimensions, window, sources, 3,
		repeatabilityMetric{
			id: "total_tokens_range", definition: "Exact diagnostic token range.", unit: "tokens",
			direction: analytics.LowerIsBetter, amount: 120,
			availability: evaluation.MetricAvailability{Available: true},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fact.Value.Amount != 120 || fact.Value.Unit != "tokens" || fact.SampleSize != 3 ||
		fact.Dimensions.ReviewDimension != "correctness" || fact.ResultCompleteness != analytics.CompletenessComplete {
		t.Fatalf("repeatability fact = %+v", fact)
	}
	facts := analytics.FactSet{
		SchemaVersion: analytics.FactSetSchemaVersion, Window: window,
		Completeness: analytics.CompletenessComplete, IncompleteReasons: []string{},
		ReviewRuns: []analytics.ReviewRunFact{}, Stages: []analytics.StageFact{},
		ContextProviders: []analytics.ContextProviderFact{}, Findings: []analytics.FindingFunnelFact{},
		FeedbackOutcomes: []analytics.FeedbackOutcomeFact{}, Experiments: []analytics.ExperimentFact{},
		FindingLineages: []analytics.FindingLineageFact{},
		Repeatability:   []analytics.RepeatabilityFact{fact}, ValueObservations: []analytics.ValueObservation{},
	}
	projection, err := analytics.ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	var repeatabilityTile *analytics.DashboardTile
	for index := range projection.Tiles {
		if projection.Tiles[index].MetricID == "repeatability.total_tokens_range.observation" {
			repeatabilityTile = &projection.Tiles[index]
		}
	}
	if repeatabilityTile == nil || repeatabilityTile.Availability != analytics.TileObserved ||
		repeatabilityTile.Value == nil || repeatabilityTile.Value.Amount != 120 {
		t.Fatalf("repeatability tiles = %+v", projection.Tiles)
	}
	partial, err := buildRepeatabilityFact(
		run, "case-1", dimensions, window, sources, 3,
		repeatabilityMetric{
			id: "candidate_count_range", definition: "Exact candidate range.", unit: "candidates",
			direction:    analytics.LowerIsBetter,
			availability: evaluation.MetricAvailability{ReasonCode: "dimension_execution_sample_unavailable"},
		},
	)
	if err != nil || partial.ResultCompleteness != analytics.CompletenessPartial ||
		len(partial.IncompleteReasons) != 1 {
		t.Fatalf("partial repeatability fact = %+v, %v", partial, err)
	}
	rows, err := repeatabilityParquetRows([]analytics.RepeatabilityFact{fact, partial})
	if err != nil || len(rows) != 2 || rows[0].ReviewDimension != "correctness" ||
		rows[0].RepeatabilityRunID != "repeatability-1" {
		t.Fatalf("repeatability parquet rows = %+v, %v", rows, err)
	}
}
