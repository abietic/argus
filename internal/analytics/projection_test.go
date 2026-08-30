package analytics

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFactSetValidateReconcilesIndependentFunnelFacts(t *testing.T) {
	facts := completeFunnelFactSet()
	if err := facts.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	facts.ReviewRuns[0].Funnel.Published++
	if err := facts.Validate(); err == nil ||
		!strings.Contains(err.Error(), "does not reconcile") {
		t.Fatalf("Validate() error = %v, want reconciliation rejection", err)
	}
}

func TestHumanQueueFeedbackIsTraceableWithoutInflatingPublishedAcceptance(t *testing.T) {
	facts := completeFunnelFactSet()
	humanQueue := FeedbackOutcomeFact{
		SchemaVersion: FeedbackOutcomeFactSchemaVersion,
		FactID:        "feedback-outcome-human-queue", RunID: "run-1", FindingID: "finding-4",
		Feedback:    FeedbackAccepted,
		FeedbackRef: testSource(SourceFeedback, "feedback-human-queue", "revision-1", 'f'),
		FeedbackAt:  timePointer(analyticsTime(2, 14)), Outcome: OutcomeNoOutcome,
		ResultCompleteness: CompletenessComplete, IncompleteReasons: []string{},
		Dimensions: facts.Findings[3].Dimensions, OccurredAt: analyticsTime(3, 11),
	}
	facts.FeedbackOutcomes = append(facts.FeedbackOutcomes, humanQueue)
	if err := facts.Validate(); err != nil {
		t.Fatalf("human-queue feedback Validate() error = %v", err)
	}
	projection, err := ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCountTile(t, projection, "funnel.accepted.count", 1)

	facts.Findings[3].PublicationEligibility = EligibilitySuppressed
	if err := facts.Validate(); err == nil ||
		!strings.Contains(err.Error(), "outside published or platform human-queue") {
		t.Fatalf("suppressed unpublished feedback error = %v", err)
	}
	facts.Findings[3].PublicationEligibility = EligibilityHumanQueue
	facts.FeedbackOutcomes[1].Feedback = FeedbackNoFeedback
	facts.FeedbackOutcomes[1].FeedbackRef = nil
	facts.FeedbackOutcomes[1].FeedbackAt = nil
	if err := facts.Validate(); err == nil ||
		!strings.Contains(err.Error(), "invents an unobserved human-queue response") {
		t.Fatalf("unobserved human-queue response error = %v", err)
	}
}

func TestProjectDashboardKeepsFunnelFeedbackOutcomeAndNoCandidateDistinct(
	t *testing.T,
) {
	facts := completeFunnelFactSet()
	projection, err := ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatalf("ProjectDashboard() error = %v", err)
	}

	assertCountTile(t, projection, "funnel.candidate.count", 4)
	assertCountTile(t, projection, "funnel.verified.count", 3)
	assertCountTile(t, projection, "funnel.published.count", 2)
	assertCountTile(t, projection, "funnel.accepted.count", 1)
	assertCountTile(t, projection, "funnel.fixed.count", 1)
	assertCountTile(t, projection, "funnel.no_feedback.count", 1)
	assertCountTile(t, projection, "funnel.feedback_unknown.count", 0)
	assertCountTile(t, projection, "funnel.no_outcome.count", 1)
	assertCountTile(t, projection, "funnel.outcome_unknown.count", 0)
	assertCountTile(t, projection, "run.no_candidate.count", 1)

	noCandidate := findTile(t, projection, "run.no_candidate.count", nil)
	if !slices.Contains(
		noCandidate.Warnings,
		"no_candidate_is_not_clean_verdict",
	) || !strings.Contains(noCandidate.Definition, "not a clean-code verdict") {
		t.Fatalf("no-candidate tile can be mistaken for clean verdict: %+v", noCandidate)
	}
	for _, tile := range projection.Tiles {
		if tile.Definition == "" || tile.MetricVersion == "" ||
			tile.Window != projection.Window && !strings.HasPrefix(tile.MetricID, "experiment.") {
			t.Fatalf("tile omits definition/version/window: %+v", tile)
		}
	}
}

func TestProjectDashboardIncompleteDatasetDoesNotInferNoFeedbackOrRates(
	t *testing.T,
) {
	facts := completeFunnelFactSet()
	facts.Completeness = CompletenessPartial
	facts.IncompleteReasons = []string{"source_partition_missing"}
	facts.FeedbackOutcomes = nil
	facts.FeedbackOutcomes = []FeedbackOutcomeFact{}

	projection, err := ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatalf("ProjectDashboard() error = %v", err)
	}
	assertCountTile(t, projection, "funnel.no_feedback.count", 0)
	assertCountTile(t, projection, "funnel.feedback_unknown.count", 2)
	assertCountTile(t, projection, "funnel.outcome_unknown.count", 2)

	count := findTile(t, projection, "funnel.published.count", nil)
	if count.Qualifier != ValueLowerBound ||
		!slices.Contains(count.Warnings, "incomplete_fact_set_lower_bound") {
		t.Fatalf("partial count was not marked lower-bound: %+v", count)
	}
	rate := findTile(t, projection, "funnel.published_to_accepted.rate", nil)
	if rate.Availability != TileUnknown || rate.Value != nil ||
		rate.Qualifier != ValueUnavailable ||
		!slices.Contains(rate.Warnings, "partial_fact_set_rate_not_comparable") {
		t.Fatalf("partial rate was presented as comparable: %+v", rate)
	}
}

func TestProjectDashboardGroupsByGovernedDimensionsDeterministically(
	t *testing.T,
) {
	facts := completeFunnelFactSet()
	facts.Findings[3].Dimensions.Language = "java"
	projection, err := ProjectDashboard(
		facts,
		[]DimensionName{DimensionRule, DimensionLanguage},
	)
	if err != nil {
		t.Fatalf("ProjectDashboard() error = %v", err)
	}
	if !slices.Equal(
		projection.GroupBy,
		[]DimensionName{DimensionLanguage, DimensionRule},
	) {
		t.Fatalf("group_by = %v, want canonical dimension order", projection.GroupBy)
	}
	goTile := findTile(
		t,
		projection,
		"funnel.candidate.count",
		[]DimensionValue{
			{Name: DimensionLanguage, Value: "go"},
			{Name: DimensionRule, Value: "rule.logic"},
		},
	)
	javaTile := findTile(
		t,
		projection,
		"funnel.candidate.count",
		[]DimensionValue{
			{Name: DimensionLanguage, Value: "java"},
			{Name: DimensionRule, Value: "rule.logic"},
		},
	)
	if goTile.Value.Amount != 3 || javaTile.Value.Amount != 1 {
		t.Fatalf(
			"dimension counts = go:%d java:%d, want 3/1",
			goTile.Value.Amount,
			javaTile.Value.Amount,
		)
	}
}

func TestProjectDashboardSeparatesProviderExecutionGapAndReplayReuse(t *testing.T) {
	facts := completeFunnelFactSet()
	dimensions := facts.ReviewRuns[0].Dimensions
	facts.ContextProviders = []ContextProviderFact{
		contextProviderFact("provider-fact-1", "receipt-1", ContextProviderExecuted, ContextProviderSucceeded, "", 10_000, dimensions),
		contextProviderFact("provider-fact-2", "receipt-2", ContextProviderExecuted, ContextProviderGap, "provider_timeout", 30_000, dimensions),
		contextProviderFact("provider-fact-3", "receipt-3", ContextProviderReused, ContextProviderSucceeded, "", 10_000, dimensions),
	}
	projection, err := ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatalf("ProjectDashboard() error = %v", err)
	}
	providerDimensions := []DimensionValue{
		{Name: DimensionContextProvider, Value: "go-ast-exact@1"},
		{Name: DimensionContextKind, Value: "go_ast"},
	}
	for metric, want := range map[string]int64{
		"context_provider.attempt.count":   2,
		"context_provider.succeeded.count": 1,
		"context_provider.gap.count":       1,
		"context_provider.reused.count":    1,
	} {
		tile := findTile(t, projection, metric, providerDimensions)
		if tile.Value == nil || tile.Value.Amount != want ||
			!slices.Contains(tile.Warnings, "local_host_observation_not_platform_attestation") {
			t.Fatalf("%s = %+v, want %d with local authority warning", metric, tile, want)
		}
	}
	rate := findTile(t, projection, "context_provider.success.rate", providerDimensions)
	if rate.Value == nil || rate.Value.Amount != 500_000 || rate.Value.Scale != 6 {
		t.Fatalf("provider success rate = %+v, want 0.5", rate)
	}
	p50 := findTile(t, projection, "context_provider.duration.p50", providerDimensions)
	p95 := findTile(t, projection, "context_provider.duration.p95", providerDimensions)
	if p50.Value == nil || p50.Value.Amount != 10_000 ||
		p95.Value == nil || p95.Value.Amount != 30_000 {
		t.Fatalf("provider durations p50=%+v p95=%+v", p50, p95)
	}
	gapDimensions := append(slices.Clone(providerDimensions), DimensionValue{
		Name: DimensionContextGapReason, Value: "provider_timeout",
	})
	gapReason := findTile(t, projection, "context_provider.gap_reason.count", gapDimensions)
	if gapReason.Value == nil || gapReason.Value.Amount != 1 {
		t.Fatalf("provider gap reason = %+v", gapReason)
	}
}

func TestProjectDashboardSurfacesFindingLineageRelationsWithoutOutcomeInference(t *testing.T) {
	facts := emptyCompleteFactSet()
	facts.FindingLineages = []FindingLineageFact{
		findingLineageFact(
			"lineage-fact-1", "relation-continued", "continued", "exact_fingerprint",
			[]string{"finding-baseline-1"}, []string{"finding-variant-1"},
		),
		findingLineageFact(
			"lineage-fact-2", "relation-split", "split", "stable_family",
			[]string{"finding-baseline-2"}, []string{"finding-variant-2", "finding-variant-3"},
		),
	}
	projection, err := ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	continuedDimensions := []DimensionValue{
		{Name: DimensionLineageRelation, Value: "continued"},
		{Name: DimensionLineageMethod, Value: "exact_fingerprint"},
		{Name: DimensionLineagePolicy, Value: "finding-lineage-git-aware@1"},
	}
	for metric, want := range map[string]int64{
		"finding_lineage.relation.count":                   1,
		"finding_lineage.baseline_finding_reference.count": 1,
		"finding_lineage.variant_finding_reference.count":  1,
	} {
		tile := findTile(t, projection, metric, continuedDimensions)
		if tile.Value == nil || tile.Value.Amount != want ||
			!slices.Contains(tile.Warnings, "lineage_relation_is_not_outcome_or_label") {
			t.Fatalf("%s = %+v, want %d relationship-only", metric, tile, want)
		}
	}
	splitDimensions := []DimensionValue{
		{Name: DimensionLineageRelation, Value: "split"},
		{Name: DimensionLineageMethod, Value: "stable_family"},
		{Name: DimensionLineagePolicy, Value: "finding-lineage-git-aware@1"},
	}
	variantRefs := findTile(
		t, projection, "finding_lineage.variant_finding_reference.count", splitDimensions,
	)
	if variantRefs.Value == nil || variantRefs.Value.Amount != 2 {
		t.Fatalf("split variant Finding references = %+v", variantRefs)
	}
	for _, tile := range projection.Tiles {
		if strings.HasPrefix(tile.MetricID, "finding_lineage.") &&
			(strings.Contains(tile.MetricID, "fixed") || strings.Contains(tile.MetricID, "escaped") ||
				strings.Contains(tile.MetricID, "label")) {
			t.Fatalf("lineage projection inferred a governed outcome: %+v", tile)
		}
	}
}

func TestExperimentFactsRequireComparableBaselineAndVariant(t *testing.T) {
	facts := emptyCompleteFactSet()
	baseline, variant := experimentPair()
	facts.Experiments = []ExperimentFact{variant}
	if err := facts.Validate(); err == nil || !strings.Contains(err.Error(), "no baseline") {
		t.Fatalf("Validate() error = %v, want missing baseline rejection", err)
	}

	facts.Experiments = []ExperimentFact{baseline, variant}
	if err := facts.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	projection, err := ProjectDashboard(facts, nil)
	if err != nil {
		t.Fatalf("ProjectDashboard() error = %v", err)
	}
	delta := findTile(
		t,
		projection,
		"experiment.published_precision.delta",
		append(governedDimensionValues(testDimensions()),
			DimensionValue{Name: DimensionExperiment, Value: "experiment-1@revision-1"},
			DimensionValue{Name: DimensionExperimentArm, Value: "comparison"},
			DimensionValue{Name: DimensionVariant, Value: "candidate"},
		),
	)
	if delta.Value == nil || delta.Value.Amount != 500 {
		t.Fatalf("experiment delta = %+v, want 500 basis points", delta.Value)
	}

	facts.Experiments[1].Value.Unit = "milliseconds"
	if err := facts.Validate(); err == nil ||
		!strings.Contains(err.Error(), "not comparable") {
		t.Fatalf("Validate() error = %v, want unit mismatch rejection", err)
	}
}

func assertCountTile(
	t *testing.T,
	projection DashboardProjection,
	metricID string,
	want int64,
) {
	t.Helper()
	tile := findTile(t, projection, metricID, nil)
	if tile.Availability != TileObserved || tile.Value == nil ||
		tile.Value.Amount != want || tile.Value.Unit != "count" {
		t.Fatalf("%s = %+v, want count %d", metricID, tile, want)
	}
}

func findTile(
	t *testing.T,
	projection DashboardProjection,
	metricID string,
	dimensions []DimensionValue,
) DashboardTile {
	t.Helper()
	for _, tile := range projection.Tiles {
		if tile.MetricID == metricID &&
			(dimensions == nil || slices.Equal(tile.Dimensions, dimensions)) {
			return tile
		}
	}
	t.Fatalf("tile %q with dimensions %+v not found", metricID, dimensions)
	return DashboardTile{}
}

func completeFunnelFactSet() FactSet {
	facts := emptyCompleteFactSet()
	startedAt := analyticsTime(2, 10)
	finishedAt := analyticsTime(2, 11)
	baseDimensions := testDimensions()
	facts.ReviewRuns = []ReviewRunFact{
		{
			SchemaVersion:      ReviewRunFactSchemaVersion,
			FactID:             "run-fact-1",
			RunID:              "run-1",
			Dimensions:         baseDimensions,
			Status:             RunStatusSucceeded,
			ResultCompleteness: CompletenessComplete,
			IncompleteReasons:  []string{},
			Funnel: &FunnelSummary{
				Candidates: 4,
				Normalized: 4,
				Verified:   3,
				Published:  2,
			},
			StartedAt:  startedAt,
			FinishedAt: &finishedAt,
			OccurredAt: finishedAt,
		},
		{
			SchemaVersion:      ReviewRunFactSchemaVersion,
			FactID:             "run-fact-2",
			RunID:              "run-2",
			Dimensions:         baseDimensions,
			Status:             RunStatusSucceeded,
			ResultCompleteness: CompletenessComplete,
			IncompleteReasons:  []string{},
			Funnel:             &FunnelSummary{},
			StartedAt:          analyticsTime(2, 12),
			FinishedAt:         timePointer(analyticsTime(2, 13)),
			OccurredAt:         analyticsTime(2, 13),
		},
	}
	facts.Stages = []StageFact{
		{
			SchemaVersion:      StageFactSchemaVersion,
			FactID:             "stage-fact-1",
			RunID:              "run-1",
			StageID:            "verify",
			StageRevision:      "stage-v1",
			Attempt:            1,
			Dimensions:         baseDimensions,
			Status:             StageStatusSucceeded,
			ResultCompleteness: CompletenessComplete,
			IncompleteReasons:  []string{},
			DurationMicros:     1_000,
			OccurredAt:         finishedAt,
		},
	}
	findingDimensions := baseDimensions
	findingDimensions.Language = "go"
	findingDimensions.RuleID = "rule.logic"
	findingDimensions.Path = "internal/review.go"
	facts.Findings = []FindingFunnelFact{
		publishedFindingFact("candidate-1", "finding-1", "finding-fact-1", findingDimensions),
		publishedFindingFact("candidate-2", "finding-2", "finding-fact-2", findingDimensions),
		{
			SchemaVersion:          FindingFunnelFactSchemaVersion,
			FactID:                 "finding-fact-3",
			RunID:                  "run-1",
			CandidateID:            "candidate-3",
			FindingID:              "finding-3",
			Normalized:             true,
			Verification:           VerificationVerified,
			PublicationEligibility: EligibilitySuppressed,
			Publication:            PublicationNotReached,
			ResultCompleteness:     CompletenessComplete,
			IncompleteReasons:      []string{},
			Dimensions:             findingDimensions,
			OccurredAt:             finishedAt,
		},
		{
			SchemaVersion:          FindingFunnelFactSchemaVersion,
			FactID:                 "finding-fact-4",
			RunID:                  "run-1",
			CandidateID:            "candidate-4",
			FindingID:              "finding-4",
			Normalized:             true,
			Verification:           VerificationInconclusive,
			PublicationEligibility: EligibilityHumanQueue,
			Publication:            PublicationNotReached,
			ResultCompleteness:     CompletenessComplete,
			IncompleteReasons:      []string{},
			Dimensions:             findingDimensions,
			OccurredAt:             finishedAt,
		},
	}
	facts.FeedbackOutcomes = []FeedbackOutcomeFact{
		{
			SchemaVersion: FeedbackOutcomeFactSchemaVersion,
			FactID:        "feedback-outcome-fact-1",
			RunID:         "run-1",
			FindingID:     "finding-1",
			Feedback:      FeedbackAccepted,
			FeedbackRef: testSource(
				SourceFeedback,
				"feedback-1",
				"revision-1",
				'b',
			),
			FeedbackAt: timePointer(analyticsTime(2, 12)),
			Outcome:    OutcomeFixed,
			OutcomeRef: testSource(
				SourceOutcome,
				"outcome-1",
				"revision-1",
				'c',
			),
			OutcomeAt:          timePointer(analyticsTime(2, 13)),
			ResultCompleteness: CompletenessComplete,
			IncompleteReasons:  []string{},
			Dimensions:         findingDimensions,
			OccurredAt:         analyticsTime(3, 10),
		},
	}
	return facts
}

func publishedFindingFact(
	candidateID string,
	findingID string,
	factID string,
	dimensions Dimensions,
) FindingFunnelFact {
	return FindingFunnelFact{
		SchemaVersion:          FindingFunnelFactSchemaVersion,
		FactID:                 factID,
		RunID:                  "run-1",
		CandidateID:            candidateID,
		FindingID:              findingID,
		Normalized:             true,
		Verification:           VerificationVerified,
		PublicationEligibility: EligibilityPublishEligible,
		Publication:            PublicationPublished,
		PublicationRef: testSource(
			SourcePublication,
			"publication-"+findingID,
			"published",
			'd',
		),
		PublicationAt:      timePointer(analyticsTime(2, 11)),
		ResultCompleteness: CompletenessComplete,
		IncompleteReasons:  []string{},
		Dimensions:         dimensions,
		OccurredAt:         analyticsTime(2, 11),
	}
}

func emptyCompleteFactSet() FactSet {
	return FactSet{
		SchemaVersion: FactSetSchemaVersion,
		Window: TimeWindow{
			StartInclusive: analyticsTime(1, 0),
			EndExclusive:   analyticsTime(31, 0),
		},
		Completeness:      CompletenessComplete,
		IncompleteReasons: []string{},
		ReviewRuns:        []ReviewRunFact{},
		Stages:            []StageFact{},
		ContextProviders:  []ContextProviderFact{},
		Findings:          []FindingFunnelFact{},
		FeedbackOutcomes:  []FeedbackOutcomeFact{},
		FindingLineages:   []FindingLineageFact{},
		Experiments:       []ExperimentFact{},
		Repeatability:     []RepeatabilityFact{},
		ValueObservations: []ValueObservation{},
	}
}

func contextProviderFact(
	factID string,
	receiptID string,
	mode ContextProviderBindingMode,
	status ContextProviderStatus,
	reason string,
	durationMicros uint64,
	dimensions Dimensions,
) ContextProviderFact {
	contract := "argus.context.go_ast.v1alpha1"
	if status == ContextProviderGap {
		contract = ""
	}
	return ContextProviderFact{
		SchemaVersion: ContextProviderFactSchemaVersion,
		FactID:        factID, RunID: "run-1", ReceiptID: receiptID,
		ReceiptSHA256:         strings.Repeat("a", 64),
		ReceiptArtifactSHA256: strings.Repeat("b", 64),
		ProviderID:            "go-ast-exact", ProviderRevision: "1", Kind: "go_ast",
		AdapterID: "argus-go-ast", AdapterRevision: "1",
		AdapterSHA256: strings.Repeat("c", 64), RequestSHA256: strings.Repeat("d", 64),
		BindingMode: mode, Status: status, ReasonCode: reason,
		ContextID: "context-1", ContextDigest: strings.Repeat("e", 64),
		ContextContract: contract, TargetPathCount: 2,
		TimeoutMicros: 1_000_000, DurationMicros: durationMicros,
		Authority: "local_host_observation", Dimensions: dimensions,
		OccurredAt: analyticsTime(2, 10),
	}
}

func findingLineageFact(
	factID string,
	relationID string,
	relationType string,
	relationMethod string,
	baseline []string,
	variant []string,
) FindingLineageFact {
	return FindingLineageFact{
		SchemaVersion: FindingLineageFactSchemaVersion,
		FactID:        factID, LineageID: "finding-lineage-1",
		LineageArtifactSHA256: strings.Repeat("1", 64), RelationID: relationID,
		RelationType: relationType, RelationMethod: relationMethod,
		ReasonCode: "conservative_match", FamilyKey: strings.Repeat("2", 64),
		PolicyID: "finding-lineage-git-aware", PolicyRevision: "1",
		PolicySHA256: strings.Repeat("3", 64), AncestryAuthority: "local_git_object_graph",
		AncestryEvidenceSHA256: strings.Repeat("4", 64), RenameMappingCount: 0,
		BaselineRunID: "run-baseline", VariantRunID: "run-variant",
		BaselineFindingIDs: baseline, VariantFindingIDs: variant,
		TenantID: "tenant", OrganizationID: "organization", WorkspaceID: "workspace",
		RepositoryID: "repository", OccurredAt: analyticsTime(2, 12),
	}
}

func experimentPair() (ExperimentFact, ExperimentFact) {
	base := ExperimentFact{
		SchemaVersion:      ExperimentFactSchemaVersion,
		FactID:             "experiment-baseline-fact",
		ExperimentID:       "experiment-1",
		ExperimentRevision: "revision-1",
		Arm:                ExperimentBaseline,
		VariantID:          "baseline",
		MetricID:           "published_precision",
		MetricVersion:      "metric-v1",
		MetricDefinition:   "Verified accepted findings divided by sampled publications.",
		Direction:          HigherIsBetter,
		Value:              FixedPoint{Amount: 8_000, Scale: 4, Unit: "ratio"},
		SampleSize:         100,
		Window: TimeWindow{
			StartInclusive: analyticsTime(1, 0),
			EndExclusive:   analyticsTime(3, 0),
		},
		ResultCompleteness: CompletenessComplete,
		IncompleteReasons:  []string{},
		Dimensions:         testDimensions(),
		SourceRefs: []SourceRef{
			*testSource(SourceExperiment, "experiment-1", "revision-1", 'd'),
		},
		OccurredAt: analyticsTime(4, 0),
	}
	variant := base
	variant.FactID = "experiment-variant-fact"
	variant.Arm = ExperimentVariant
	variant.VariantID = "candidate"
	variant.Value.Amount = 8_500
	variant.SourceRefs = []SourceRef{
		*testSource(SourceExperiment, "experiment-1-candidate", "revision-1", 'e'),
	}
	return base, variant
}

func testDimensions() Dimensions {
	return Dimensions{
		TenantID:         "tenant-1",
		OrganizationID:   "organization-1",
		RepositoryID:     "repository-1",
		WorkflowRevision: "workflow-v1",
		ConfigRevision:   "config-v1",
	}
}

func testSource(
	kind SourceKind,
	id string,
	revision string,
	digestByte byte,
) *SourceRef {
	return &SourceRef{
		Kind:     kind,
		ID:       id,
		Revision: revision,
		SHA256:   strings.Repeat(string(digestByte), 64),
	}
}

func analyticsTime(day, hour int) time.Time {
	return time.Date(2026, time.July, day, hour, 0, 0, 0, time.UTC)
}

func timePointer(value time.Time) *time.Time {
	return &value
}
