package analytics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
)

type metricDefinition struct {
	ID         string
	Definition string
}

var standardMetrics = map[string]metricDefinition{
	"run.total.count": {
		ID:         "run.total.count",
		Definition: "Review run facts observed in the window.",
	},
	"run.succeeded.count": {
		ID:         "run.succeeded.count",
		Definition: "Review runs whose terminal status is succeeded.",
	},
	"run.partial.count": {
		ID:         "run.partial.count",
		Definition: "Review runs whose result completeness is explicitly partial.",
	},
	"run.unknown.count": {
		ID:         "run.unknown.count",
		Definition: "Review runs whose result completeness is explicitly unknown.",
	},
	"run.no_candidate.count": {
		ID: "run.no_candidate.count",
		Definition: "Succeeded, complete review runs with zero candidate facts in their " +
			"authoritative funnel summary; this is not a clean-code verdict.",
	},
	"run.candidate_state_unknown.count": {
		ID: "run.candidate_state_unknown.count",
		Definition: "Review runs whose incomplete or missing funnel cannot establish " +
			"whether candidates were absent.",
	},
	"stage.total.count": {
		ID:         "stage.total.count",
		Definition: "Stage attempt facts observed in the window.",
	},
	"stage.failed.count": {
		ID:         "stage.failed.count",
		Definition: "Stage attempts whose terminal status is failed.",
	},
	"stage.partial.count": {
		ID:         "stage.partial.count",
		Definition: "Stage attempts whose result completeness is explicitly partial.",
	},
	"stage.unknown.count": {
		ID:         "stage.unknown.count",
		Definition: "Stage attempts whose result completeness is explicitly unknown.",
	},
	"stage.duration.p50": {
		ID: "stage.duration.p50",
		Definition: "Nearest-rank p50 of persisted stage-attempt duration in microseconds " +
			"for the observed cohort.",
	},
	"stage.duration.p95": {
		ID: "stage.duration.p95",
		Definition: "Nearest-rank p95 of persisted stage-attempt duration in microseconds " +
			"for the observed cohort.",
	},
	"context_provider.attempt.count": {
		ID: "context_provider.attempt.count",
		Definition: "Local-host-observed Context Provider execution receipts; replay reuse " +
			"is excluded from this attempt count.",
	},
	"context_provider.succeeded.count": {
		ID: "context_provider.succeeded.count",
		Definition: "Local Context Provider execution attempts that produced a frozen " +
			"ContextRef rather than a typed ContextGap.",
	},
	"context_provider.gap.count": {
		ID: "context_provider.gap.count",
		Definition: "Local Context Provider execution attempts that produced an explicit " +
			"typed ContextGap.",
	},
	"context_provider.reused.count": {
		ID: "context_provider.reused.count",
		Definition: "Replay bindings that reused an immutable Context Provider receipt " +
			"without executing the provider again.",
	},
	"context_provider.success.rate": {
		ID: "context_provider.success.rate",
		Definition: "Succeeded local Context Provider execution attempts divided by all " +
			"local execution attempts; replay reuse is excluded.",
	},
	"context_provider.duration.p50": {
		ID: "context_provider.duration.p50",
		Definition: "Nearest-rank p50 of local Context Provider execution duration in " +
			"microseconds; replay reuse is excluded.",
	},
	"context_provider.duration.p95": {
		ID: "context_provider.duration.p95",
		Definition: "Nearest-rank p95 of local Context Provider execution duration in " +
			"microseconds; replay reuse is excluded.",
	},
	"context_provider.gap_reason.count": {
		ID: "context_provider.gap_reason.count",
		Definition: "Local Context Provider execution gaps classified by their persisted " +
			"typed reason code.",
	},
	"finding_lineage.relation.count": {
		ID: "finding_lineage.relation.count",
		Definition: "Immutable cross-revision Finding relations observed in the window; " +
			"relation names do not assert an Outcome or evaluation label.",
	},
	"finding_lineage.baseline_finding_reference.count": {
		ID: "finding_lineage.baseline_finding_reference.count",
		Definition: "Baseline Finding references covered by immutable lineage relations; " +
			"resolved references are not fixed Outcomes.",
	},
	"finding_lineage.variant_finding_reference.count": {
		ID: "finding_lineage.variant_finding_reference.count",
		Definition: "Variant Finding references covered by immutable lineage relations; " +
			"introduced references are not escaped defects or labels.",
	},
	"funnel.candidate.count": {
		ID: "funnel.candidate.count",
		Definition: "Candidate facts before normalization; candidates are hypotheses, " +
			"not verified defects.",
	},
	"funnel.normalized.count": {
		ID:         "funnel.normalized.count",
		Definition: "Candidate facts that produced a normalized Finding identity.",
	},
	"funnel.verified.count": {
		ID:         "funnel.verified.count",
		Definition: "Normalized Findings whose verification ledger projection is verified.",
	},
	"funnel.published.count": {
		ID: "funnel.published.count",
		Definition: "Verified Findings whose independent publication decision is " +
			"published.",
	},
	"funnel.accepted.count": {
		ID: "funnel.accepted.count",
		Definition: "Published Findings with an explicit accepted feedback fact; missing " +
			"feedback is never counted as acceptance.",
	},
	"funnel.fixed.count": {
		ID: "funnel.fixed.count",
		Definition: "Published Findings with an explicit fixed outcome fact, independent " +
			"of whether accepted feedback exists.",
	},
	"funnel.accepted_fixed.count": {
		ID: "funnel.accepted_fixed.count",
		Definition: "Published Findings that have both accepted feedback and a fixed " +
			"outcome.",
	},
	"funnel.publication_unknown.count": {
		ID:         "funnel.publication_unknown.count",
		Definition: "Candidate facts whose publication state is explicitly unknown.",
	},
	"funnel.no_feedback.count": {
		ID: "funnel.no_feedback.count",
		Definition: "Published Findings with explicit no_feedback, plus absent feedback " +
			"only when the fact set is complete.",
	},
	"funnel.feedback_unknown.count": {
		ID: "funnel.feedback_unknown.count",
		Definition: "Published Findings with explicit unknown feedback, or missing " +
			"feedback in an incomplete fact set.",
	},
	"funnel.no_outcome.count": {
		ID: "funnel.no_outcome.count",
		Definition: "Published Findings with explicit no_outcome, plus absent outcomes " +
			"only when the fact set is complete.",
	},
	"funnel.outcome_unknown.count": {
		ID: "funnel.outcome_unknown.count",
		Definition: "Published Findings with explicit unknown outcome, or missing outcome " +
			"in an incomplete fact set.",
	},
	"funnel.candidate_to_verified.rate": {
		ID: "funnel.candidate_to_verified.rate",
		Definition: "Verified Findings divided by candidate facts for the same governed " +
			"dimension cohort.",
	},
	"funnel.verified_to_published.rate": {
		ID: "funnel.verified_to_published.rate",
		Definition: "Published Findings divided by verified Findings; this does not measure " +
			"detector precision.",
	},
	"funnel.published_to_accepted.rate": {
		ID: "funnel.published_to_accepted.rate",
		Definition: "Explicit accepted feedback divided by published Findings; no-feedback " +
			"and unknown feedback are not treated as acceptance.",
	},
	"funnel.accepted_to_fixed.rate": {
		ID: "funnel.accepted_to_fixed.rate",
		Definition: "Findings with both accepted feedback and fixed outcome divided by " +
			"accepted Findings.",
	},
	"funnel.published_to_fixed.rate": {
		ID: "funnel.published_to_fixed.rate",
		Definition: "Explicit fixed outcomes divided by published Findings; this outcome " +
			"projection is independent of feedback.",
	},
}

type runAggregate struct {
	dimensions            []DimensionValue
	total                 uint64
	succeeded             uint64
	partial               uint64
	unknown               uint64
	noCandidate           uint64
	candidateStateUnknown uint64
}

type stageAggregate struct {
	dimensions []DimensionValue
	total      uint64
	failed     uint64
	partial    uint64
	unknown    uint64
	durations  []uint64
}

type contextProviderAggregate struct {
	dimensions []DimensionValue
	attempts   uint64
	succeeded  uint64
	gaps       uint64
	reused     uint64
	durations  []uint64
	gapReasons map[string]uint64
}

type findingLineageAggregate struct {
	dimensions          []DimensionValue
	relations           uint64
	baselineFindingRefs uint64
	variantFindingRefs  uint64
}

type funnelAggregate struct {
	dimensions         []DimensionValue
	candidates         uint64
	normalized         uint64
	verified           uint64
	published          uint64
	accepted           uint64
	fixed              uint64
	acceptedFixed      uint64
	publicationUnknown uint64
	noFeedback         uint64
	feedbackUnknown    uint64
	noOutcome          uint64
	outcomeUnknown     uint64
	seenFindings       map[string]struct{}
}

type valueAggregate struct {
	dimensions []DimensionValue
	tiers      map[EvidenceTier]uint64
	total      uint64
}

// ProjectDashboard builds deterministic read models from immutable facts.
// Counts from incomplete FactSets are marked as lower bounds. Rates are
// unavailable because a partial denominator would be misleading.
func ProjectDashboard(
	facts FactSet,
	groupBy []DimensionName,
) (DashboardProjection, error) {
	if err := facts.Validate(); err != nil {
		return DashboardProjection{}, fmt.Errorf("validate facts: %w", err)
	}
	if err := validateGroupBy(groupBy); err != nil {
		return DashboardProjection{}, err
	}
	groupBy = canonicalGroupBy(groupBy)

	runs := make(map[string]*runAggregate)
	stages := make(map[string]*stageAggregate)
	contextProviders := make(map[string]*contextProviderAggregate)
	findingLineages := make(map[string]*findingLineageAggregate)
	funnels := make(map[string]*funnelAggregate)
	values := make(map[string]*valueAggregate)
	if len(groupBy) == 0 {
		runs[""] = &runAggregate{dimensions: []DimensionValue{}}
		stages[""] = &stageAggregate{dimensions: []DimensionValue{}}
		funnels[""] = &funnelAggregate{
			dimensions:   []DimensionValue{},
			seenFindings: make(map[string]struct{}),
		}
		values[""] = &valueAggregate{
			dimensions: []DimensionValue{},
			tiers:      make(map[EvidenceTier]uint64),
		}
	}

	for _, fact := range facts.ReviewRuns {
		key, dimensions := dimensionGroup(fact.Dimensions, groupBy)
		aggregate := runs[key]
		if aggregate == nil {
			aggregate = &runAggregate{dimensions: dimensions}
			runs[key] = aggregate
		}
		aggregate.total++
		if fact.Status == RunStatusSucceeded {
			aggregate.succeeded++
		}
		switch fact.ResultCompleteness {
		case CompletenessPartial:
			aggregate.partial++
		case CompletenessUnknown:
			aggregate.unknown++
		}
		if fact.Status == RunStatusSucceeded &&
			fact.ResultCompleteness == CompletenessComplete &&
			fact.Funnel != nil && fact.Funnel.Candidates == 0 {
			aggregate.noCandidate++
		} else if fact.ResultCompleteness != CompletenessComplete ||
			fact.Funnel == nil {
			aggregate.candidateStateUnknown++
		}
	}

	for _, fact := range facts.Stages {
		key, dimensions := dimensionGroup(fact.Dimensions, groupBy)
		aggregate := stages[key]
		if aggregate == nil {
			aggregate = &stageAggregate{dimensions: dimensions}
			stages[key] = aggregate
		}
		aggregate.total++
		if fact.Status != StageStatusStarted {
			aggregate.durations = append(aggregate.durations, fact.DurationMicros)
		}
		if fact.Status == StageStatusFailed {
			aggregate.failed++
		}
		switch fact.ResultCompleteness {
		case CompletenessPartial:
			aggregate.partial++
		case CompletenessUnknown:
			aggregate.unknown++
		}
	}

	for _, fact := range facts.ContextProviders {
		groupKey, dimensions := dimensionGroup(fact.Dimensions, groupBy)
		providerValue := fact.ProviderID + "@" + fact.ProviderRevision
		dimensions = append(dimensions,
			DimensionValue{Name: DimensionContextProvider, Value: providerValue},
			DimensionValue{Name: DimensionContextKind, Value: fact.Kind},
		)
		key := strings.Join([]string{groupKey, providerValue, fact.Kind}, "\x00")
		aggregate := contextProviders[key]
		if aggregate == nil {
			aggregate = &contextProviderAggregate{
				dimensions: dimensions,
				gapReasons: make(map[string]uint64),
			}
			contextProviders[key] = aggregate
		}
		if fact.BindingMode == ContextProviderReused {
			aggregate.reused++
			continue
		}
		aggregate.attempts++
		aggregate.durations = append(aggregate.durations, fact.DurationMicros)
		if fact.Status == ContextProviderSucceeded {
			aggregate.succeeded++
		} else {
			aggregate.gaps++
			aggregate.gapReasons[fact.ReasonCode]++
		}
	}

	for _, fact := range facts.FindingLineages {
		lineageDimensions := Dimensions{
			TenantID: fact.TenantID, OrganizationID: fact.OrganizationID,
			RepositoryID:     fact.RepositoryID,
			WorkflowRevision: UnknownDimensionValue,
			ConfigRevision:   UnknownDimensionValue,
		}
		groupKey, dimensions := dimensionGroup(lineageDimensions, groupBy)
		policy := fact.PolicyID + "@" + fact.PolicyRevision
		dimensions = append(dimensions,
			DimensionValue{Name: DimensionLineageRelation, Value: fact.RelationType},
			DimensionValue{Name: DimensionLineageMethod, Value: fact.RelationMethod},
			DimensionValue{Name: DimensionLineagePolicy, Value: policy},
		)
		key := strings.Join(
			[]string{groupKey, fact.RelationType, fact.RelationMethod, policy}, "\x00",
		)
		aggregate := findingLineages[key]
		if aggregate == nil {
			aggregate = &findingLineageAggregate{dimensions: dimensions}
			findingLineages[key] = aggregate
		}
		aggregate.relations++
		aggregate.baselineFindingRefs += uint64(len(fact.BaselineFindingIDs))
		aggregate.variantFindingRefs += uint64(len(fact.VariantFindingIDs))
	}

	feedback := make(map[string]FeedbackOutcomeFact, len(facts.FeedbackOutcomes))
	for _, fact := range facts.FeedbackOutcomes {
		feedback[runEntityKey(fact.RunID, fact.FindingID)] = fact
	}
	for _, fact := range facts.Findings {
		key, dimensions := dimensionGroup(fact.Dimensions, groupBy)
		aggregate := funnels[key]
		if aggregate == nil {
			aggregate = &funnelAggregate{
				dimensions:   dimensions,
				seenFindings: make(map[string]struct{}),
			}
			funnels[key] = aggregate
		}
		aggregate.candidates++
		if !fact.Normalized {
			continue
		}
		findingKey := runEntityKey(fact.RunID, fact.FindingID)
		if _, duplicate := aggregate.seenFindings[findingKey]; duplicate {
			continue
		}
		aggregate.seenFindings[findingKey] = struct{}{}
		aggregate.normalized++
		if fact.Verification == VerificationVerified {
			aggregate.verified++
		}
		if fact.Publication == PublicationUnknown {
			aggregate.publicationUnknown++
		}
		if fact.Publication != PublicationPublished {
			continue
		}
		aggregate.published++
		projection, exists := feedback[findingKey]
		if !exists {
			if facts.Completeness == CompletenessComplete {
				aggregate.noFeedback++
				aggregate.noOutcome++
			} else {
				aggregate.feedbackUnknown++
				aggregate.outcomeUnknown++
			}
			continue
		}
		if projection.Feedback == FeedbackAccepted {
			aggregate.accepted++
		}
		if projection.Outcome == OutcomeFixed {
			aggregate.fixed++
		}
		if projection.Feedback == FeedbackAccepted &&
			projection.Outcome == OutcomeFixed {
			aggregate.acceptedFixed++
		}
		switch projection.Feedback {
		case FeedbackNoFeedback:
			aggregate.noFeedback++
		case FeedbackUnknown:
			aggregate.feedbackUnknown++
		}
		switch projection.Outcome {
		case OutcomeNoOutcome:
			aggregate.noOutcome++
		case OutcomeUnknown:
			aggregate.outcomeUnknown++
		}
	}

	for _, observation := range facts.ValueObservations {
		key, dimensions := dimensionGroup(observation.Dimensions, groupBy)
		aggregate := values[key]
		if aggregate == nil {
			aggregate = &valueAggregate{
				dimensions: dimensions,
				tiers:      make(map[EvidenceTier]uint64),
			}
			values[key] = aggregate
		}
		aggregate.tiers[observation.EvidenceTier]++
		aggregate.total++
	}

	tiles := make([]DashboardTile, 0)
	var err error
	tiles, err = appendRunTiles(tiles, facts, runs)
	if err != nil {
		return DashboardProjection{}, err
	}
	tiles, err = appendStageTiles(tiles, facts, stages)
	if err != nil {
		return DashboardProjection{}, err
	}
	tiles, err = appendContextProviderTiles(tiles, facts, contextProviders)
	if err != nil {
		return DashboardProjection{}, err
	}
	tiles, err = appendFindingLineageTiles(tiles, facts, findingLineages)
	if err != nil {
		return DashboardProjection{}, err
	}
	tiles, err = appendFunnelTiles(tiles, facts, funnels)
	if err != nil {
		return DashboardProjection{}, err
	}
	tiles, err = appendValueTiles(tiles, facts, values)
	if err != nil {
		return DashboardProjection{}, err
	}
	tiles, err = appendExperimentTiles(tiles, facts)
	if err != nil {
		return DashboardProjection{}, err
	}
	tiles, err = appendRepeatabilityTiles(tiles, facts)
	if err != nil {
		return DashboardProjection{}, err
	}

	slices.SortFunc(tiles, func(left, right DashboardTile) int {
		return strings.Compare(tileSortKey(left), tileSortKey(right))
	})
	projection := DashboardProjection{
		SchemaVersion: DashboardProjectionSchemaVersion,
		Window:        facts.Window,
		GroupBy:       groupBy,
		Tiles:         tiles,
	}
	if err := projection.Validate(); err != nil {
		return DashboardProjection{}, fmt.Errorf("validate dashboard projection: %w", err)
	}
	return projection, nil
}

func appendFindingLineageTiles(
	tiles []DashboardTile,
	facts FactSet,
	aggregates map[string]*findingLineageAggregate,
) ([]DashboardTile, error) {
	warnings := []string{"lineage_relation_is_not_outcome_or_label"}
	for _, key := range sortedKeys(aggregates) {
		value := aggregates[key]
		for _, metric := range []struct {
			id    string
			count uint64
		}{
			{"finding_lineage.relation.count", value.relations},
			{"finding_lineage.baseline_finding_reference.count", value.baselineFindingRefs},
			{"finding_lineage.variant_finding_reference.count", value.variantFindingRefs},
		} {
			tile, err := countTile(
				standardMetrics[metric.id], facts.Window, value.dimensions,
				metric.count, value.relations, facts.Completeness, warnings,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
	}
	return tiles, nil
}

func appendContextProviderTiles(
	tiles []DashboardTile,
	facts FactSet,
	aggregates map[string]*contextProviderAggregate,
) ([]DashboardTile, error) {
	completeness := contextProviderCompleteness(facts)
	warnings := []string{"local_host_observation_not_platform_attestation"}
	for _, key := range sortedKeys(aggregates) {
		value := aggregates[key]
		for _, metric := range []struct {
			id     string
			count  uint64
			sample uint64
		}{
			{"context_provider.attempt.count", value.attempts, value.attempts},
			{"context_provider.succeeded.count", value.succeeded, value.attempts},
			{"context_provider.gap.count", value.gaps, value.attempts},
			{"context_provider.reused.count", value.reused, value.reused},
		} {
			tile, err := countTile(
				standardMetrics[metric.id], facts.Window, value.dimensions,
				metric.count, metric.sample, completeness, warnings,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
		rate, err := rateTile(
			standardMetrics["context_provider.success.rate"], facts.Window,
			value.dimensions, value.succeeded, value.attempts, completeness,
		)
		if err != nil {
			return nil, err
		}
		rate.Warnings = append(rate.Warnings, warnings...)
		rate, err = finalizeTile(rate)
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, rate)
		for _, percentile := range []uint64{50, 95} {
			metricID := fmt.Sprintf("context_provider.duration.p%d", percentile)
			tile, err := percentileTile(
				standardMetrics[metricID], facts.Window, value.dimensions,
				value.durations, percentile, completeness,
			)
			if err != nil {
				return nil, err
			}
			tile.Warnings = append(tile.Warnings, warnings...)
			tile, err = finalizeTile(tile)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
		for _, reason := range sortedKeys(value.gapReasons) {
			dimensions := append(slices.Clone(value.dimensions), DimensionValue{
				Name: DimensionContextGapReason, Value: reason,
			})
			tile, err := countTile(
				standardMetrics["context_provider.gap_reason.count"], facts.Window,
				dimensions, value.gapReasons[reason], value.attempts,
				completeness, warnings,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
	}
	return tiles, nil
}

func contextProviderCompleteness(facts FactSet) Completeness {
	for _, reason := range facts.IncompleteReasons {
		if reason == "nonterminal_runs_not_projected" {
			return CompletenessPartial
		}
	}
	return CompletenessComplete
}

func appendRunTiles(
	tiles []DashboardTile,
	facts FactSet,
	aggregates map[string]*runAggregate,
) ([]DashboardTile, error) {
	keys := sortedKeys(aggregates)
	for _, key := range keys {
		value := aggregates[key]
		metrics := []struct {
			id       string
			count    uint64
			sample   uint64
			warnings []string
		}{
			{"run.total.count", value.total, value.total, []string{}},
			{"run.succeeded.count", value.succeeded, value.total, []string{}},
			{"run.partial.count", value.partial, value.total, []string{}},
			{"run.unknown.count", value.unknown, value.total, []string{}},
			{
				"run.no_candidate.count",
				value.noCandidate,
				value.total,
				[]string{"no_candidate_is_not_clean_verdict"},
			},
			{
				"run.candidate_state_unknown.count",
				value.candidateStateUnknown,
				value.total,
				[]string{"unknown_is_not_zero"},
			},
		}
		for _, metric := range metrics {
			tile, err := countTile(
				standardMetrics[metric.id],
				facts.Window,
				value.dimensions,
				metric.count,
				metric.sample,
				facts.Completeness,
				metric.warnings,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
	}
	return tiles, nil
}

func appendStageTiles(
	tiles []DashboardTile,
	facts FactSet,
	aggregates map[string]*stageAggregate,
) ([]DashboardTile, error) {
	keys := sortedKeys(aggregates)
	for _, key := range keys {
		value := aggregates[key]
		metrics := []struct {
			id    string
			count uint64
		}{
			{"stage.total.count", value.total},
			{"stage.failed.count", value.failed},
			{"stage.partial.count", value.partial},
			{"stage.unknown.count", value.unknown},
		}
		for _, metric := range metrics {
			tile, err := countTile(
				standardMetrics[metric.id],
				facts.Window,
				value.dimensions,
				metric.count,
				value.total,
				facts.Completeness,
				[]string{},
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
		for _, percentile := range []struct {
			id      string
			percent uint64
		}{
			{"stage.duration.p50", 50},
			{"stage.duration.p95", 95},
		} {
			tile, err := percentileTile(
				standardMetrics[percentile.id],
				facts.Window,
				value.dimensions,
				value.durations,
				percentile.percent,
				facts.Completeness,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
	}
	return tiles, nil
}

func appendFunnelTiles(
	tiles []DashboardTile,
	facts FactSet,
	aggregates map[string]*funnelAggregate,
) ([]DashboardTile, error) {
	keys := sortedKeys(aggregates)
	for _, key := range keys {
		value := aggregates[key]
		counts := []struct {
			id      string
			count   uint64
			sample  uint64
			warning []string
		}{
			{"funnel.candidate.count", value.candidates, value.candidates, []string{}},
			{"funnel.normalized.count", value.normalized, value.candidates, []string{}},
			{"funnel.verified.count", value.verified, value.candidates, []string{}},
			{"funnel.published.count", value.published, value.verified, []string{}},
			{"funnel.accepted.count", value.accepted, value.published, []string{}},
			{"funnel.fixed.count", value.fixed, value.published, []string{}},
			{"funnel.accepted_fixed.count", value.acceptedFixed, value.accepted, []string{}},
			{
				"funnel.publication_unknown.count",
				value.publicationUnknown,
				value.candidates,
				[]string{"unknown_is_not_zero"},
			},
			{"funnel.no_feedback.count", value.noFeedback, value.published, []string{}},
			{
				"funnel.feedback_unknown.count",
				value.feedbackUnknown,
				value.published,
				[]string{"unknown_is_not_no_feedback"},
			},
			{"funnel.no_outcome.count", value.noOutcome, value.published, []string{}},
			{
				"funnel.outcome_unknown.count",
				value.outcomeUnknown,
				value.published,
				[]string{"unknown_is_not_no_outcome"},
			},
		}
		for _, metric := range counts {
			tile, err := countTile(
				standardMetrics[metric.id],
				facts.Window,
				value.dimensions,
				metric.count,
				metric.sample,
				facts.Completeness,
				metric.warning,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
		rates := []struct {
			id          string
			numerator   uint64
			denominator uint64
		}{
			{
				"funnel.candidate_to_verified.rate",
				value.verified,
				value.candidates,
			},
			{
				"funnel.verified_to_published.rate",
				value.published,
				value.verified,
			},
			{
				"funnel.published_to_accepted.rate",
				value.accepted,
				value.published,
			},
			{
				"funnel.accepted_to_fixed.rate",
				value.acceptedFixed,
				value.accepted,
			},
			{
				"funnel.published_to_fixed.rate",
				value.fixed,
				value.published,
			},
		}
		for _, metric := range rates {
			tile, err := rateTile(
				standardMetrics[metric.id],
				facts.Window,
				value.dimensions,
				metric.numerator,
				metric.denominator,
				facts.Completeness,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
	}
	return tiles, nil
}

func appendValueTiles(
	tiles []DashboardTile,
	facts FactSet,
	aggregates map[string]*valueAggregate,
) ([]DashboardTile, error) {
	keys := sortedKeys(aggregates)
	for _, key := range keys {
		value := aggregates[key]
		for _, tier := range []EvidenceTier{
			EvidenceTierE0,
			EvidenceTierE1,
			EvidenceTierE2,
			EvidenceTierE3,
		} {
			definition := metricDefinition{
				ID: "value.evidence." + string(tier) + ".count",
				Definition: fmt.Sprintf(
					"Value observations classified as %s evidence. E0 and E1 are not ROI evidence.",
					tier,
				),
			}
			warnings := []string{}
			if tier == EvidenceTierE0 || tier == EvidenceTierE1 {
				warnings = []string{"e0_e1_not_roi"}
			}
			tile, err := countTile(
				definition,
				facts.Window,
				value.dimensions,
				value.tiers[tier],
				value.total,
				facts.Completeness,
				warnings,
			)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
	}
	return tiles, nil
}

func appendExperimentTiles(
	tiles []DashboardTile,
	facts FactSet,
) ([]DashboardTile, error) {
	type group struct {
		baseline *ExperimentFact
		variants []ExperimentFact
	}
	groups := make(map[string]*group)
	for index := range facts.Experiments {
		fact := &facts.Experiments[index]
		key := strings.Join(
			[]string{
				fact.ExperimentID,
				fact.ExperimentRevision,
				fact.MetricID,
				dimensionsStableKey(fact.Dimensions),
			},
			"\x00",
		)
		current := groups[key]
		if current == nil {
			current = &group{}
			groups[key] = current
		}
		if fact.Arm == ExperimentBaseline {
			current.baseline = fact
		} else {
			current.variants = append(current.variants, *fact)
		}
		tile, err := experimentObservationTile(*fact)
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, tile)
	}
	for _, key := range sortedKeys(groups) {
		current := groups[key]
		if current.baseline == nil {
			continue
		}
		slices.SortFunc(current.variants, func(left, right ExperimentFact) int {
			return strings.Compare(left.VariantID, right.VariantID)
		})
		for _, variant := range current.variants {
			tile, err := experimentDeltaTile(*current.baseline, variant)
			if err != nil {
				return nil, err
			}
			tiles = append(tiles, tile)
		}
	}
	return tiles, nil
}

func appendRepeatabilityTiles(tiles []DashboardTile, facts FactSet) ([]DashboardTile, error) {
	for _, fact := range facts.Repeatability {
		dimensions := append(governedDimensionValues(fact.Dimensions),
			DimensionValue{Name: DimensionRepeatability, Value: fact.RepeatabilityRunID + "@" + fact.RepeatabilityRevision},
			DimensionValue{Name: DimensionEvaluationCase, Value: fact.CaseID},
		)
		tile := DashboardTile{
			MetricID:      "repeatability." + fact.MetricID + ".observation",
			MetricVersion: fact.MetricVersion, Definition: fact.MetricDefinition,
			Window: fact.Window, Dimensions: dimensions, SampleSize: fact.SampleSize,
			Warnings: []string{},
		}
		if fact.ResultCompleteness == CompletenessComplete {
			value := fact.Value
			tile.Availability = TileObserved
			tile.Qualifier = ValueExact
			tile.Value = &value
		} else {
			tile.Availability = TileUnknown
			tile.Qualifier = ValueUnavailable
			tile.Warnings = append([]string{"incomplete_repeatability_observation"}, fact.IncompleteReasons...)
		}
		final, err := finalizeTile(tile)
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, final)
	}
	return tiles, nil
}

func experimentObservationTile(fact ExperimentFact) (DashboardTile, error) {
	dimensions := append(governedDimensionValues(fact.Dimensions),
		DimensionValue{
			Name:  DimensionExperiment,
			Value: fact.ExperimentID + "@" + fact.ExperimentRevision,
		},
		DimensionValue{Name: DimensionExperimentArm, Value: string(fact.Arm)},
		DimensionValue{Name: DimensionVariant, Value: fact.VariantID},
	)
	tile := DashboardTile{
		MetricID:      "experiment." + fact.MetricID + ".observation",
		MetricVersion: fact.MetricVersion,
		Definition:    fact.MetricDefinition,
		Window:        fact.Window,
		Dimensions:    dimensions,
		SampleSize:    fact.SampleSize,
		Warnings:      []string{},
	}
	if fact.ResultCompleteness == CompletenessComplete {
		value := fact.Value
		tile.Availability = TileObserved
		tile.Qualifier = ValueExact
		tile.Value = &value
	} else {
		tile.Availability = TileUnknown
		tile.Qualifier = ValueUnavailable
		tile.Warnings = []string{"incomplete_experiment_observation"}
	}
	return finalizeTile(tile)
}

func experimentDeltaTile(
	baseline ExperimentFact,
	variant ExperimentFact,
) (DashboardTile, error) {
	dimensions := append(governedDimensionValues(variant.Dimensions),
		DimensionValue{
			Name:  DimensionExperiment,
			Value: variant.ExperimentID + "@" + variant.ExperimentRevision,
		},
		DimensionValue{Name: DimensionExperimentArm, Value: "comparison"},
		DimensionValue{Name: DimensionVariant, Value: variant.VariantID},
	)
	tile := DashboardTile{
		MetricID:      "experiment." + variant.MetricID + ".delta",
		MetricVersion: variant.MetricVersion,
		Definition: "Variant minus baseline for " + variant.MetricDefinition +
			"; interpret the sign using the metric direction.",
		Window:     variant.Window,
		Dimensions: dimensions,
		SampleSize: min(variant.SampleSize, baseline.SampleSize),
		Warnings:   []string{},
	}
	if baseline.ResultCompleteness != CompletenessComplete ||
		variant.ResultCompleteness != CompletenessComplete {
		tile.Availability = TileUnknown
		tile.Qualifier = ValueUnavailable
		tile.Warnings = []string{"incomplete_experiment_comparison"}
		return finalizeTile(tile)
	}
	delta, err := checkedSubtract(variant.Value.Amount, baseline.Value.Amount)
	if err != nil {
		return DashboardTile{}, fmt.Errorf("calculate experiment delta: %w", err)
	}
	value := FixedPoint{
		Amount: delta,
		Scale:  variant.Value.Scale,
		Unit:   variant.Value.Unit,
	}
	tile.Availability = TileObserved
	tile.Qualifier = ValueExact
	tile.Value = &value
	return finalizeTile(tile)
}

func countTile(
	definition metricDefinition,
	window TimeWindow,
	dimensions []DimensionValue,
	count uint64,
	sampleSize uint64,
	completeness Completeness,
	warnings []string,
) (DashboardTile, error) {
	if count > math.MaxInt64 {
		return DashboardTile{}, fmt.Errorf("metric %q count overflows int64", definition.ID)
	}
	qualifier := ValueExact
	if completeness != CompletenessComplete {
		qualifier = ValueLowerBound
		warnings = append(slices.Clone(warnings), "incomplete_fact_set_lower_bound")
	}
	slices.Sort(warnings)
	value := FixedPoint{Amount: int64(count), Scale: 0, Unit: "count"}
	return finalizeTile(DashboardTile{
		MetricID:      definition.ID,
		MetricVersion: DashboardMetricDefinitionVersion,
		Definition:    definition.Definition,
		Window:        window,
		Dimensions:    slices.Clone(dimensions),
		SampleSize:    sampleSize,
		Availability:  TileObserved,
		Qualifier:     qualifier,
		Value:         &value,
		Warnings:      warnings,
	})
}

func rateTile(
	definition metricDefinition,
	window TimeWindow,
	dimensions []DimensionValue,
	numerator uint64,
	denominator uint64,
	completeness Completeness,
) (DashboardTile, error) {
	tile := DashboardTile{
		MetricID:      definition.ID,
		MetricVersion: DashboardMetricDefinitionVersion,
		Definition:    definition.Definition,
		Window:        window,
		Dimensions:    slices.Clone(dimensions),
		SampleSize:    denominator,
		Warnings:      []string{},
	}
	if completeness != CompletenessComplete {
		tile.Availability = TileUnknown
		tile.Qualifier = ValueUnavailable
		tile.Warnings = []string{"partial_fact_set_rate_not_comparable"}
		return finalizeTile(tile)
	}
	if denominator == 0 {
		tile.Availability = TileUnknown
		tile.Qualifier = ValueUnavailable
		tile.Warnings = []string{"zero_denominator"}
		return finalizeTile(tile)
	}
	if numerator > denominator {
		return DashboardTile{}, fmt.Errorf(
			"metric %q numerator %d exceeds denominator %d",
			definition.ID,
			numerator,
			denominator,
		)
	}
	scaled := new(big.Int).SetUint64(numerator)
	scaled.Mul(scaled, big.NewInt(1_000_000))
	scaled.Quo(scaled, new(big.Int).SetUint64(denominator))
	value := FixedPoint{
		Amount: scaled.Int64(),
		Scale:  rateScale,
		Unit:   "ratio",
	}
	tile.Availability = TileObserved
	tile.Qualifier = ValueExact
	tile.Value = &value
	return finalizeTile(tile)
}

func percentileTile(
	definition metricDefinition,
	window TimeWindow,
	dimensions []DimensionValue,
	observations []uint64,
	percentile uint64,
	completeness Completeness,
) (DashboardTile, error) {
	tile := DashboardTile{
		MetricID:      definition.ID,
		MetricVersion: DashboardMetricDefinitionVersion,
		Definition:    definition.Definition,
		Window:        window,
		Dimensions:    slices.Clone(dimensions),
		SampleSize:    uint64(len(observations)),
		Warnings:      []string{},
	}
	if len(observations) == 0 {
		tile.Availability = TileUnknown
		tile.Qualifier = ValueUnavailable
		tile.Warnings = []string{"zero_sample"}
		return finalizeTile(tile)
	}
	if percentile == 0 || percentile > 100 {
		return DashboardTile{}, fmt.Errorf("unsupported percentile %d", percentile)
	}
	sorted := slices.Clone(observations)
	slices.Sort(sorted)
	rank := (uint64(len(sorted))*percentile + 99) / 100
	if rank == 0 {
		rank = 1
	}
	selected := sorted[rank-1]
	if selected > math.MaxInt64 {
		return DashboardTile{}, fmt.Errorf(
			"metric %q duration overflows int64",
			definition.ID,
		)
	}
	value := FixedPoint{
		Amount: int64(selected),
		Scale:  0,
		Unit:   "microseconds",
	}
	tile.Availability = TileObserved
	tile.Qualifier = ValueExact
	if completeness != CompletenessComplete {
		tile.Qualifier = ValueSampleOnly
		tile.Warnings = []string{"incomplete_fact_set_sample_only"}
	}
	tile.Value = &value
	return finalizeTile(tile)
}

func finalizeTile(tile DashboardTile) (DashboardTile, error) {
	slices.Sort(tile.Warnings)
	identity := struct {
		MetricID      string
		MetricVersion string
		Window        TimeWindow
		Dimensions    []DimensionValue
	}{
		MetricID:      tile.MetricID,
		MetricVersion: tile.MetricVersion,
		Window:        tile.Window,
		Dimensions:    tile.Dimensions,
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return DashboardTile{}, fmt.Errorf("marshal tile identity: %w", err)
	}
	digest := sha256.Sum256(data)
	tile.TileID = hex.EncodeToString(digest[:])
	if err := tile.Validate(); err != nil {
		return DashboardTile{}, err
	}
	return tile, nil
}

func canonicalGroupBy(groupBy []DimensionName) []DimensionName {
	result := append([]DimensionName{}, groupBy...)
	order := map[DimensionName]int{
		DimensionTenant:           0,
		DimensionOrganization:     1,
		DimensionRepository:       2,
		DimensionLanguage:         3,
		DimensionRule:             4,
		DimensionPath:             5,
		DimensionWorkflowRevision: 6,
		DimensionConfigRevision:   7,
	}
	slices.SortFunc(result, func(left, right DimensionName) int {
		return order[left] - order[right]
	})
	return result
}

func dimensionGroup(
	dimensions Dimensions,
	groupBy []DimensionName,
) (string, []DimensionValue) {
	values := make([]DimensionValue, 0, len(groupBy))
	key := make([]string, 0, len(groupBy))
	for _, name := range groupBy {
		value := dimensionValue(dimensions, name)
		if value == "" {
			value = UnknownDimensionValue
		}
		values = append(values, DimensionValue{Name: name, Value: value})
		key = append(key, string(name), value)
	}
	return strings.Join(key, "\x00"), values
}

func dimensionValue(dimensions Dimensions, name DimensionName) string {
	switch name {
	case DimensionTenant:
		return dimensions.TenantID
	case DimensionOrganization:
		return dimensions.OrganizationID
	case DimensionRepository:
		return dimensions.RepositoryID
	case DimensionLanguage:
		return dimensions.Language
	case DimensionRule:
		return dimensions.RuleID
	case DimensionPath:
		return dimensions.Path
	case DimensionReviewDimension:
		return dimensions.ReviewDimension
	case DimensionWorkflowRevision:
		return dimensions.WorkflowRevision
	case DimensionConfigRevision:
		return dimensions.ConfigRevision
	default:
		return ""
	}
}

func governedDimensionValues(dimensions Dimensions) []DimensionValue {
	names := []DimensionName{
		DimensionTenant,
		DimensionOrganization,
		DimensionRepository,
		DimensionLanguage,
		DimensionRule,
		DimensionPath,
		DimensionReviewDimension,
		DimensionWorkflowRevision,
		DimensionConfigRevision,
	}
	values := make([]DimensionValue, 0, len(names))
	for _, name := range names {
		value := dimensionValue(dimensions, name)
		if value == "" {
			value = UnknownDimensionValue
		}
		values = append(values, DimensionValue{Name: name, Value: value})
	}
	return values
}

func dimensionsStableKey(dimensions Dimensions) string {
	values := governedDimensionValues(dimensions)
	parts := make([]string, 0, len(values)*2)
	for _, value := range values {
		parts = append(parts, string(value.Name), value.Value)
	}
	return strings.Join(parts, "\x00")
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func tileSortKey(tile DashboardTile) string {
	parts := []string{
		tile.MetricID,
		tile.MetricVersion,
		tile.Window.StartInclusive.Format(timeLayout),
		tile.Window.EndExclusive.Format(timeLayout),
	}
	for _, dimension := range tile.Dimensions {
		parts = append(parts, string(dimension.Name), dimension.Value)
	}
	parts = append(parts, tile.TileID)
	return strings.Join(parts, "\x00")
}

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"
