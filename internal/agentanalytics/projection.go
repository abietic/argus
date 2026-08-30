package agentanalytics

import (
	"fmt"
	"slices"
	"strings"
)

type metricDefinition struct {
	ID         string
	Definition string
	Unit       string
	Source     string
}

var metricDefinitions = []metricDefinition{
	{"agent_execution.total.count", "Shadow execution facts observed by the Argus host, including accepted intents without a visible completion.", "count", "host"},
	{"agent_execution.complete.count", "Committed shadow executions whose host-recomputed result status is complete.", "count", "host"},
	{"agent_execution.partial.count", "Committed shadow executions whose host-recomputed result status is partial.", "count", "host"},
	{"agent_execution.failed.count", "Shadow executions observed as failed by the Argus host.", "count", "host"},
	{"agent_execution.canceled.count", "Shadow executions observed as canceled by the Argus host.", "count", "host"},
	{"agent_execution.unknown_outcome.count", "Shadow execution intents with no visible immutable completion.", "count", "host"},
	{"agent_execution.duration.p50", "Nearest-rank p50 of worker-reported execution span in milliseconds.", "milliseconds", "worker"},
	{"agent_execution.duration.p95", "Nearest-rank p95 of worker-reported execution span in milliseconds.", "milliseconds", "worker"},
	{"agent_task.total.count", "Diagnostic task receipts reported by shadow workers.", "count", "worker"},
	{"agent_task.succeeded.count", "Diagnostic task receipts reported as succeeded.", "count", "worker"},
	{"agent_task.failed.count", "Diagnostic task receipts reported as failed.", "count", "worker"},
	{"agent_task.canceled.count", "Diagnostic task receipts reported as canceled.", "count", "worker"},
	{"agent_model_turn.started.count", "Provider turns reported as started by shadow workers.", "count", "worker"},
	{"agent_model_turn.completed.count", "Provider turns reported as completed by shadow workers.", "count", "worker"},
	{"agent_tool.invocation.count", "Tool invocations reported by shadow workers, including typed terminal submits.", "count", "worker"},
	{"agent_tool.failure.count", "Tool failures reported by shadow workers.", "count", "worker"},
	{"agent_usage.reported_receipts.count", "Task receipts with provider-reported token usage.", "count", "worker"},
	{"agent_usage.partial_receipts.count", "Task receipts with partial token usage lower bounds.", "count", "worker"},
	{"agent_usage.unavailable_receipts.count", "Task receipts whose token usage is unavailable.", "count", "worker"},
	{"agent_usage.input_tokens", "Worker-reported input token observations; mixed coverage is a lower bound.", "tokens", "usage"},
	{"agent_usage.output_tokens", "Worker-reported output token observations; mixed coverage is a lower bound.", "tokens", "usage"},
	{"agent_usage.cache_read_tokens", "Worker-reported cache-read token observations; mixed coverage is a lower bound.", "tokens", "usage"},
	{"agent_usage.cache_write_tokens", "Worker-reported cache-write token observations; mixed coverage is a lower bound.", "tokens", "usage"},
	{"agent_usage.reasoning_tokens", "Worker-reported reasoning token observations; absent breakdowns are not imputed.", "tokens", "usage"},
	{"agent_usage.total_tokens", "Worker-reported total token observations; mixed coverage is a lower bound.", "tokens", "usage"},
}

type projectionAggregate struct {
	dimensions        []DimensionValue
	executions        uint64
	complete          uint64
	partial           uint64
	failed            uint64
	executionCanceled uint64
	unknownOutcome    uint64
	durations         []uint64
	durationSamples   uint64
	tasks             uint64
	succeeded         uint64
	taskFailed        uint64
	canceled          uint64
	turnStarted       uint64
	turnCompleted     uint64
	toolInvocations   uint64
	toolFailures      uint64
	usage             ExecutionUsage
}

func Project(facts FactSet, groupBy []DimensionName) (Projection, error) {
	if err := facts.Validate(); err != nil {
		return Projection{}, fmt.Errorf("validate agent execution facts: %w", err)
	}
	groupBy = canonicalGroupBy(groupBy)
	if err := validateGroupBy(groupBy); err != nil {
		return Projection{}, err
	}
	aggregates := make(map[string]*projectionAggregate)
	if len(groupBy) == 0 {
		aggregates[""] = &projectionAggregate{dimensions: []DimensionValue{}}
	}
	executionByFactID := make(map[string]ExecutionFact, len(facts.Executions))
	for _, fact := range facts.Executions {
		executionByFactID[fact.FactID] = fact
		key, dimensions := executionGroup(fact, groupBy)
		aggregate := getProjectionAggregate(aggregates, key, dimensions)
		if err := addProjectionCounter(&aggregate.executions, 1, "execution count"); err != nil {
			return Projection{}, err
		}
		switch fact.Status {
		case ExecutionStatusComplete:
			if err := addProjectionCounter(&aggregate.complete, 1, "complete execution count"); err != nil {
				return Projection{}, err
			}
		case ExecutionStatusPartial:
			if err := addProjectionCounter(&aggregate.partial, 1, "partial execution count"); err != nil {
				return Projection{}, err
			}
		case ExecutionStatusFailed:
			if err := addProjectionCounter(&aggregate.failed, 1, "failed execution count"); err != nil {
				return Projection{}, err
			}
		case ExecutionStatusCanceled:
			if err := addProjectionCounter(&aggregate.executionCanceled, 1, "canceled execution count"); err != nil {
				return Projection{}, err
			}
		case ExecutionStatusUnknownOutcome:
			if err := addProjectionCounter(&aggregate.unknownOutcome, 1, "unknown-outcome execution count"); err != nil {
				return Projection{}, err
			}
		}
		if fact.ManifestID != "" {
			aggregate.durations = append(aggregate.durations, fact.DurationMS)
			if err := addProjectionCounter(&aggregate.durationSamples, 1, "duration sample count"); err != nil {
				return Projection{}, err
			}
		}
	}
	for _, fact := range facts.Tasks {
		execution := executionByFactID[fact.ExecutionFactID]
		key, dimensions := executionGroup(execution, groupBy)
		aggregate := getProjectionAggregate(aggregates, key, dimensions)
		if err := addProjectionCounter(&aggregate.tasks, 1, "task count"); err != nil {
			return Projection{}, err
		}
		switch fact.Status {
		case "succeeded":
			if err := addProjectionCounter(&aggregate.succeeded, 1, "succeeded task count"); err != nil {
				return Projection{}, err
			}
		case "failed":
			if err := addProjectionCounter(&aggregate.taskFailed, 1, "failed task count"); err != nil {
				return Projection{}, err
			}
		case "canceled":
			if err := addProjectionCounter(&aggregate.canceled, 1, "canceled task count"); err != nil {
				return Projection{}, err
			}
		}
		if err := addProjectionCounter(
			&aggregate.turnStarted,
			uint64(fact.ModelTurnsStarted),
			"started model turn count",
		); err != nil {
			return Projection{}, err
		}
		if err := addProjectionCounter(
			&aggregate.turnCompleted,
			uint64(fact.ModelTurnsCompleted),
			"completed model turn count",
		); err != nil {
			return Projection{}, err
		}
		switch fact.Usage.Completeness {
		case "provider_reported":
			if err := addProjectionCounter(
				&aggregate.usage.ReportedReceipts,
				1,
				"reported usage receipt count",
			); err != nil {
				return Projection{}, err
			}
		case "partial":
			if err := addProjectionCounter(
				&aggregate.usage.PartialReceipts,
				1,
				"partial usage receipt count",
			); err != nil {
				return Projection{}, err
			}
		case "unavailable":
			if err := addProjectionCounter(
				&aggregate.usage.UnavailableReceipts,
				1,
				"unavailable usage receipt count",
			); err != nil {
				return Projection{}, err
			}
		}
		for _, counter := range []struct {
			name   string
			target *uint64
			value  uint64
		}{
			{"input token count", &aggregate.usage.InputTokens, fact.Usage.InputTokens},
			{"output token count", &aggregate.usage.OutputTokens, fact.Usage.OutputTokens},
			{"cache-read token count", &aggregate.usage.CacheReadTokens, fact.Usage.CacheReadTokens},
			{"cache-write token count", &aggregate.usage.CacheWriteTokens, fact.Usage.CacheWriteTokens},
			{"total token count", &aggregate.usage.TotalTokens, fact.Usage.TotalTokens},
		} {
			if err := addProjectionCounter(
				counter.target,
				counter.value,
				counter.name,
			); err != nil {
				return Projection{}, err
			}
		}
		if fact.Usage.ReasoningTokens != nil {
			if err := addProjectionCounter(
				&aggregate.usage.ReasoningReportedReceipts,
				1,
				"reasoning usage receipt count",
			); err != nil {
				return Projection{}, err
			}
			if err := addProjectionCounter(
				&aggregate.usage.ReasoningTokens,
				*fact.Usage.ReasoningTokens,
				"reasoning token count",
			); err != nil {
				return Projection{}, err
			}
		}
	}
	for _, fact := range facts.ToolUsage {
		execution := executionByFactID[fact.ExecutionFactID]
		key, dimensions := executionGroup(execution, groupBy)
		aggregate := getProjectionAggregate(aggregates, key, dimensions)
		if err := addProjectionCounter(
			&aggregate.toolInvocations,
			uint64(fact.InvocationCount),
			"tool invocation count",
		); err != nil {
			return Projection{}, err
		}
		if err := addProjectionCounter(
			&aggregate.toolFailures,
			uint64(fact.FailureCount),
			"tool failure count",
		); err != nil {
			return Projection{}, err
		}
	}

	tiles := make([]MetricTile, 0, len(aggregates)*len(metricDefinitions))
	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		aggregate := aggregates[key]
		for _, definition := range metricDefinitions {
			tile := buildMetricTile(facts.Window, *aggregate, definition)
			tiles = append(tiles, tile)
		}
	}
	slices.SortFunc(tiles, func(left, right MetricTile) int {
		return strings.Compare(left.TileID, right.TileID)
	})
	projection := Projection{
		SchemaVersion: ProjectionSchemaVersion,
		Window:        facts.Window,
		GroupBy:       slices.Clone(groupBy),
		Tiles:         tiles,
	}
	if err := projection.Validate(); err != nil {
		return Projection{}, err
	}
	return projection, nil
}

func getProjectionAggregate(
	aggregates map[string]*projectionAggregate,
	key string,
	dimensions []DimensionValue,
) *projectionAggregate {
	aggregate := aggregates[key]
	if aggregate == nil {
		aggregate = &projectionAggregate{dimensions: dimensions}
		aggregates[key] = aggregate
	}
	return aggregate
}

func addProjectionCounter(target *uint64, value uint64, name string) error {
	next, ok := checkedAdd(*target, value)
	if !ok {
		return fmt.Errorf("agent execution projection %s overflows", name)
	}
	*target = next
	return nil
}

func executionGroup(
	fact ExecutionFact,
	groupBy []DimensionName,
) (string, []DimensionValue) {
	dimensions := make([]DimensionValue, 0, len(groupBy))
	parts := make([]string, 0, len(groupBy))
	for _, name := range groupBy {
		value := UnknownDimensionValue
		switch name {
		case DimensionTenant:
			value = fact.TenantID
		case DimensionWorkspace:
			value = fact.WorkspaceID
		case DimensionAgent:
			value = versionedDimensionValue(fact.Agent)
		case DimensionProvider:
			value = versionedDimensionValue(fact.Provider)
		case DimensionModel:
			value = versionedDimensionValue(fact.Model)
		}
		dimensions = append(dimensions, DimensionValue{Name: name, Value: value})
		parts = append(parts, string(name), value)
	}
	return strings.Join(parts, "\x00"), dimensions
}

func buildMetricTile(
	window TimeWindow,
	aggregate projectionAggregate,
	definition metricDefinition,
) MetricTile {
	value, sampleSize, qualifier := metricValue(aggregate, definition.ID)
	availability := TileObserved
	if value == nil {
		availability = TileUnknown
	}
	tile := MetricTile{
		MetricID: definition.ID, MetricVersion: MetricDefinitionVersion,
		Definition: definition.Definition, Window: window,
		Dimensions: slices.Clone(aggregate.dimensions), SampleSize: sampleSize,
		Availability: availability, Qualifier: qualifier, Value: value,
		Unit: definition.Unit, Authority: DiagnosticAuthority,
		Warnings: metricWarnings(definition),
	}
	tile.TileID = stableID("agent-execution-tile", tileIdentityParts(tile)...)
	return tile
}

func metricValue(
	aggregate projectionAggregate,
	metricID string,
) (*uint64, uint64, MetricQualifier) {
	host := func(value uint64) (*uint64, uint64, MetricQualifier) {
		return pointer(value), aggregate.executions, QualifierHostObserved
	}
	worker := func(value uint64) (*uint64, uint64, MetricQualifier) {
		return pointer(value), aggregate.tasks, QualifierWorkerReported
	}
	switch metricID {
	case "agent_execution.total.count":
		return host(aggregate.executions)
	case "agent_execution.complete.count":
		return host(aggregate.complete)
	case "agent_execution.partial.count":
		return host(aggregate.partial)
	case "agent_execution.failed.count":
		return host(aggregate.failed)
	case "agent_execution.canceled.count":
		return host(aggregate.executionCanceled)
	case "agent_execution.unknown_outcome.count":
		return host(aggregate.unknownOutcome)
	case "agent_execution.duration.p50":
		if aggregate.durationSamples == 0 {
			return nil, 0, QualifierUnavailable
		}
		return pointer(nearestRank(aggregate.durations, 50)), aggregate.durationSamples,
			QualifierWorkerReported
	case "agent_execution.duration.p95":
		if aggregate.durationSamples == 0 {
			return nil, 0, QualifierUnavailable
		}
		return pointer(nearestRank(aggregate.durations, 95)), aggregate.durationSamples,
			QualifierWorkerReported
	case "agent_task.total.count":
		return worker(aggregate.tasks)
	case "agent_task.succeeded.count":
		return worker(aggregate.succeeded)
	case "agent_task.failed.count":
		return worker(aggregate.taskFailed)
	case "agent_task.canceled.count":
		return worker(aggregate.canceled)
	case "agent_model_turn.started.count":
		return worker(aggregate.turnStarted)
	case "agent_model_turn.completed.count":
		return worker(aggregate.turnCompleted)
	case "agent_tool.invocation.count":
		return worker(aggregate.toolInvocations)
	case "agent_tool.failure.count":
		return worker(aggregate.toolFailures)
	case "agent_usage.reported_receipts.count":
		return worker(aggregate.usage.ReportedReceipts)
	case "agent_usage.partial_receipts.count":
		return worker(aggregate.usage.PartialReceipts)
	case "agent_usage.unavailable_receipts.count":
		return worker(aggregate.usage.UnavailableReceipts)
	}
	usageValue := uint64(0)
	switch metricID {
	case "agent_usage.input_tokens":
		usageValue = aggregate.usage.InputTokens
	case "agent_usage.output_tokens":
		usageValue = aggregate.usage.OutputTokens
	case "agent_usage.cache_read_tokens":
		usageValue = aggregate.usage.CacheReadTokens
	case "agent_usage.cache_write_tokens":
		usageValue = aggregate.usage.CacheWriteTokens
	case "agent_usage.reasoning_tokens":
		if aggregate.usage.ReasoningReportedReceipts == 0 {
			return nil, aggregate.tasks, QualifierUnavailable
		}
		usageValue = aggregate.usage.ReasoningTokens
	case "agent_usage.total_tokens":
		usageValue = aggregate.usage.TotalTokens
	default:
		return nil, 0, QualifierUnavailable
	}
	observed := aggregate.usage.ReportedReceipts + aggregate.usage.PartialReceipts
	if observed == 0 {
		return nil, aggregate.tasks, QualifierUnavailable
	}
	qualifier := QualifierWorkerReported
	if aggregate.usage.PartialReceipts > 0 || aggregate.usage.UnavailableReceipts > 0 ||
		metricID == "agent_usage.reasoning_tokens" &&
			aggregate.usage.ReasoningReportedReceipts < aggregate.tasks {
		qualifier = QualifierLowerBound
	}
	return pointer(usageValue), aggregate.tasks, qualifier
}

func nearestRank(values []uint64, percentile uint64) uint64 {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	rank := (uint64(len(ordered))*percentile + 99) / 100
	if rank == 0 {
		rank = 1
	}
	return ordered[rank-1]
}

func pointer(value uint64) *uint64 {
	copy := value
	return &copy
}

func tileIdentityParts(tile MetricTile) []string {
	parts := []string{tile.MetricID}
	for _, dimension := range tile.Dimensions {
		parts = append(parts, string(dimension.Name), dimension.Value)
	}
	return parts
}

func (projection Projection) Validate() error {
	if projection.SchemaVersion != ProjectionSchemaVersion {
		return fmt.Errorf("unsupported agent execution projection schema %q", projection.SchemaVersion)
	}
	if err := projection.Window.Validate(); err != nil {
		return err
	}
	if err := validateGroupBy(projection.GroupBy); err != nil {
		return err
	}
	if projection.Tiles == nil {
		return fmt.Errorf("tiles must be an explicit array")
	}
	groupMetrics := make(map[string]map[string]struct{})
	if len(projection.GroupBy) == 0 {
		groupMetrics[""] = make(map[string]struct{}, len(metricDefinitions))
	}
	previous := ""
	for index, tile := range projection.Tiles {
		if err := tile.Validate(); err != nil {
			return fmt.Errorf("tiles[%d]: %w", index, err)
		}
		definition, exists := metricDefinitionByID(tile.MetricID)
		if !exists {
			return fmt.Errorf("tiles[%d] uses unknown metric %q", index, tile.MetricID)
		}
		if tile.Definition != definition.Definition || tile.Unit != definition.Unit ||
			!slices.Equal(tile.Warnings, metricWarnings(definition)) {
			return fmt.Errorf("tiles[%d] metadata does not match metric registry", index)
		}
		if tile.Window != projection.Window {
			return fmt.Errorf("tiles[%d] window does not match projection", index)
		}
		if len(tile.Dimensions) != len(projection.GroupBy) {
			return fmt.Errorf("tiles[%d] dimensions do not match projection group_by", index)
		}
		for dimensionIndex, dimension := range tile.Dimensions {
			if dimension.Name != projection.GroupBy[dimensionIndex] {
				return fmt.Errorf(
					"tiles[%d] dimension[%d] does not match projection group_by",
					index,
					dimensionIndex,
				)
			}
		}
		canonicalTileID := stableID(
			"agent-execution-tile",
			tileIdentityParts(tile)...,
		)
		if tile.TileID != canonicalTileID {
			return fmt.Errorf("tiles[%d] tile_id is not canonical", index)
		}
		groupKey := dimensionGroupKey(tile.Dimensions)
		metrics := groupMetrics[groupKey]
		if metrics == nil {
			metrics = make(map[string]struct{}, len(metricDefinitions))
			groupMetrics[groupKey] = metrics
		}
		if _, duplicate := metrics[tile.MetricID]; duplicate {
			return fmt.Errorf(
				"tiles[%d] duplicates metric %q in one dimension group",
				index,
				tile.MetricID,
			)
		}
		metrics[tile.MetricID] = struct{}{}
		if index > 0 && tile.TileID <= previous {
			return fmt.Errorf("tiles must be uniquely sorted by tile_id")
		}
		previous = tile.TileID
	}
	groupKeys := make([]string, 0, len(groupMetrics))
	for key := range groupMetrics {
		groupKeys = append(groupKeys, key)
	}
	slices.Sort(groupKeys)
	for _, key := range groupKeys {
		metrics := groupMetrics[key]
		for _, definition := range metricDefinitions {
			if _, exists := metrics[definition.ID]; !exists {
				return fmt.Errorf(
					"dimension group is missing registered metric %q",
					definition.ID,
				)
			}
		}
		if len(metrics) != len(metricDefinitions) {
			return fmt.Errorf("dimension group has an unexpected metric set")
		}
	}
	return nil
}

func metricDefinitionByID(metricID string) (metricDefinition, bool) {
	for _, definition := range metricDefinitions {
		if definition.ID == metricID {
			return definition, true
		}
	}
	return metricDefinition{}, false
}

func metricWarnings(definition metricDefinition) []string {
	warnings := sortedWarnings(DiagnosticOnlyWarning, NonAttestedWarning)
	if definition.Source != "host" {
		warnings = sortedWarnings(
			DiagnosticOnlyWarning,
			NonAttestedWarning,
			WorkerSelfReportWarning,
		)
	}
	return warnings
}

func dimensionGroupKey(dimensions []DimensionValue) string {
	parts := make([]string, 0, len(dimensions)*2)
	for _, dimension := range dimensions {
		parts = append(parts, string(dimension.Name), dimension.Value)
	}
	return strings.Join(parts, "\x00")
}

func (tile MetricTile) Validate() error {
	for name, value := range map[string]string{
		"tile_id": tile.TileID, "metric_id": tile.MetricID,
		"metric_version": tile.MetricVersion, "unit": tile.Unit,
		"authority": tile.Authority,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if tile.MetricVersion != MetricDefinitionVersion ||
		tile.Authority != DiagnosticAuthority || tile.Definition == "" {
		return fmt.Errorf("tile metadata is not the diagnostic metric contract")
	}
	if tile.Dimensions == nil || tile.Warnings == nil {
		return fmt.Errorf("dimensions and warnings must be explicit arrays")
	}
	for index, dimension := range tile.Dimensions {
		if err := validateOpaque(
			fmt.Sprintf("dimensions[%d].value", index),
			dimension.Value,
			1024,
		); err != nil {
			return err
		}
	}
	if err := validateSortedCodes("warnings", tile.Warnings); err != nil {
		return err
	}
	switch tile.Availability {
	case TileObserved:
		if tile.Value == nil || tile.Qualifier == QualifierUnavailable {
			return fmt.Errorf("observed tile requires an observed value and qualifier")
		}
	case TileUnknown:
		if tile.Value != nil || tile.Qualifier != QualifierUnavailable {
			return fmt.Errorf("unknown tile must use unavailable without a value")
		}
	default:
		return fmt.Errorf("unsupported tile availability %q", tile.Availability)
	}
	return nil
}
