package agentanalytics

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func (facts FactSet) Validate() error {
	if facts.SchemaVersion != FactSetSchemaVersion {
		return fmt.Errorf("unsupported agent execution fact set schema %q", facts.SchemaVersion)
	}
	if err := facts.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if facts.Executions == nil || facts.Tasks == nil || facts.ToolUsage == nil {
		return fmt.Errorf("all diagnostic fact collections must be explicit arrays")
	}
	executions := make(map[string]ExecutionFact, len(facts.Executions))
	executionIDs := make(map[string]string, len(facts.Executions))
	previous := ""
	for index, fact := range facts.Executions {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("executions[%d]: %w", index, err)
		}
		if !facts.Window.contains(fact.ObservedAt) {
			return fmt.Errorf("executions[%d] observed outside fact set window", index)
		}
		if index > 0 && fact.FactID <= previous {
			return fmt.Errorf("execution facts must be uniquely sorted by fact_id")
		}
		previous = fact.FactID
		if prior, exists := executionIDs[fact.ExecutionID]; exists {
			return fmt.Errorf(
				"execution_id %q is bound by both %q and %q",
				fact.ExecutionID,
				prior,
				fact.ManifestID,
			)
		}
		executionIDs[fact.ExecutionID] = fact.ManifestID
		executions[fact.FactID] = fact
	}

	tasks := make(map[string]TaskExecutionFact, len(facts.Tasks))
	taskAggregates := make(map[string]*executionAggregate, len(executions))
	previous = ""
	for index, fact := range facts.Tasks {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("tasks[%d]: %w", index, err)
		}
		if !facts.Window.contains(fact.ObservedAt) {
			return fmt.Errorf("tasks[%d] observed outside fact set window", index)
		}
		if index > 0 && fact.FactID <= previous {
			return fmt.Errorf("task facts must be uniquely sorted by fact_id")
		}
		previous = fact.FactID
		execution, exists := executions[fact.ExecutionFactID]
		if !exists {
			return fmt.Errorf("tasks[%d] references unknown execution fact", index)
		}
		if fact.Agent != execution.Agent || fact.Provider != execution.Provider ||
			fact.Model != execution.Model || fact.APIProtocol != execution.APIProtocol ||
			!fact.ObservedAt.Equal(execution.ObservedAt) {
			return fmt.Errorf("tasks[%d] dimensions do not match its execution", index)
		}
		tasks[fact.FactID] = fact
		aggregate := taskAggregates[fact.ExecutionFactID]
		if aggregate == nil {
			aggregate = &executionAggregate{}
			taskAggregates[fact.ExecutionFactID] = aggregate
		}
		if err := aggregate.addTask(fact); err != nil {
			return fmt.Errorf("tasks[%d]: %w", index, err)
		}
	}

	toolCallsByTask := make(map[string]uint64, len(tasks))
	previous = ""
	for index, fact := range facts.ToolUsage {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("tool_usage[%d]: %w", index, err)
		}
		if !facts.Window.contains(fact.ObservedAt) {
			return fmt.Errorf("tool_usage[%d] observed outside fact set window", index)
		}
		if index > 0 && fact.FactID <= previous {
			return fmt.Errorf("tool usage facts must be uniquely sorted by fact_id")
		}
		previous = fact.FactID
		task, exists := tasks[fact.TaskFactID]
		if !exists || task.ExecutionFactID != fact.ExecutionFactID ||
			!fact.ObservedAt.Equal(task.ObservedAt) {
			return fmt.Errorf("tool_usage[%d] does not bind its task", index)
		}
		next, ok := checkedAdd(toolCallsByTask[fact.TaskFactID], uint64(fact.InvocationCount))
		if !ok {
			return fmt.Errorf("tool_usage[%d] invocation count overflows", index)
		}
		toolCallsByTask[fact.TaskFactID] = next
	}
	for _, task := range facts.Tasks {
		if toolCallsByTask[task.FactID] != uint64(task.ToolCalls) {
			return fmt.Errorf("task %q tool usage does not reconcile", task.FactID)
		}
	}
	for factID, execution := range executions {
		aggregate := taskAggregates[factID]
		if aggregate == nil {
			aggregate = &executionAggregate{usage: ExecutionUsage{
				Completeness: contractsv1alpha1.AgentTokenUsageUnavailable,
			}}
		}
		if err := aggregate.matches(execution); err != nil {
			return fmt.Errorf("execution %q: %w", execution.ExecutionID, err)
		}
	}
	return nil
}

func (fact ExecutionFact) Validate() error {
	if fact.SchemaVersion != ExecutionFactSchemaVersion {
		return fmt.Errorf("unsupported execution fact schema %q", fact.SchemaVersion)
	}
	if err := validateIdentifier("fact_id", fact.FactID); err != nil {
		return err
	}
	hasManifest := fact.ManifestID != "" || fact.ObservationID != ""
	if (fact.ManifestID == "") != (fact.ObservationID == "") {
		return fmt.Errorf("manifest_id and observation_id must be present together")
	}
	if hasManifest {
		if err := validateOpaque("observation_id", fact.ObservationID, 256); err != nil {
			return err
		}
		if err := validateOpaque("manifest_id", fact.ManifestID, 256); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"source_run_id": fact.SourceRunID, "execution_id": fact.ExecutionID,
		"review_run_id": fact.ReviewRunID, "tenant_id": fact.TenantID,
		"workspace_id": fact.WorkspaceID,
	} {
		if err := validateOpaque(name, value, 256); err != nil {
			return err
		}
	}
	for name, ref := range map[string]contractsv1alpha1.VersionedRef{
		"agent": fact.Agent, "provider": fact.Provider, "model": fact.Model,
	} {
		if err := validateVersionedRef(name, ref); err != nil {
			return err
		}
	}
	if fact.APIProtocol != contractsv1alpha1.AgentAPIProtocolAnthropicMessages {
		return fmt.Errorf("unsupported api_protocol %q", fact.APIProtocol)
	}
	if fact.ExecutionClass !=
		contractsv1alpha1.AgentReviewExecutionLocalDirectProviderShadow {
		return fmt.Errorf("execution_class must remain local direct-provider shadow")
	}
	if fact.Attestation != contractsv1alpha1.AgentReviewAttestationNonAttested {
		return fmt.Errorf("attestation must remain non_attested")
	}
	if fact.Disposition != ShadowDisposition {
		return fmt.Errorf("disposition must remain shadow_only")
	}
	switch fact.Status {
	case ExecutionStatusComplete, ExecutionStatusPartial:
		if !hasManifest {
			return fmt.Errorf("complete/partial execution fact requires result provenance")
		}
	case ExecutionStatusFailed:
	case ExecutionStatusCanceled, ExecutionStatusUnknownOutcome:
		if hasManifest {
			return fmt.Errorf("canceled/unknown execution fact forbids result provenance")
		}
	default:
		return fmt.Errorf("unsupported execution status %q", fact.Status)
	}
	if err := validateSortedCodes("reason_codes", fact.ReasonCodes); err != nil {
		return err
	}
	if fact.ProvenanceClass != HostObservationProvenance ||
		fact.Authority != DiagnosticAuthority {
		return fmt.Errorf("execution fact must be a diagnostic host observation")
	}
	if err := validateUTC("observed_at", fact.ObservedAt); err != nil {
		return err
	}
	if !hasManifest && (fact.Receipts != 0 || fact.TasksSucceeded != 0 ||
		fact.TasksFailed != 0 || fact.TasksCanceled != 0 ||
		fact.ModelTurnsStarted != 0 || fact.ModelTurnsCompleted != 0 ||
		fact.ToolCalls != 0) {
		return fmt.Errorf("result-less execution fact must keep receipt counters zero")
	}
	if fact.Status == ExecutionStatusUnknownOutcome && fact.DurationMS != 0 {
		return fmt.Errorf("unknown outcome duration is not observed")
	}
	return fact.Usage.Validate(fact.Receipts)
}

func (fact TaskExecutionFact) Validate() error {
	if fact.SchemaVersion != TaskFactSchemaVersion {
		return fmt.Errorf("unsupported task fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id": fact.FactID, "execution_fact_id": fact.ExecutionFactID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"receipt_id": fact.ReceiptID, "task_id": fact.TaskID, "group_id": fact.GroupID,
	} {
		if err := validateOpaque(name, value, 256); err != nil {
			return err
		}
	}
	for name, ref := range map[string]contractsv1alpha1.VersionedRef{
		"dimension": fact.Dimension, "runtime": fact.Runtime, "profile": fact.Profile,
		"agent": fact.Agent, "provider": fact.Provider, "model": fact.Model,
	} {
		if err := validateVersionedRef(name, ref); err != nil {
			return err
		}
	}
	switch fact.Role {
	case contractsv1alpha1.AgentTaskContext, contractsv1alpha1.AgentTaskReview:
		if fact.HypothesisOccurrenceID != nil {
			return fmt.Errorf("only verification tasks may bind a hypothesis occurrence")
		}
	case contractsv1alpha1.AgentTaskVerification:
		if fact.HypothesisOccurrenceID == nil {
			return fmt.Errorf("verification task requires a hypothesis occurrence")
		}
		if err := validateOpaque("hypothesis_occurrence_id", *fact.HypothesisOccurrenceID, 256); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported task role %q", fact.Role)
	}
	switch fact.Status {
	case contractsv1alpha1.AgentTaskSucceeded:
		if fact.FailureReasonCode != nil {
			return fmt.Errorf("succeeded task forbids failure_reason_code")
		}
	case contractsv1alpha1.AgentTaskFailed, contractsv1alpha1.AgentTaskCanceled:
		if fact.FailureReasonCode == nil {
			return fmt.Errorf("failed/canceled task requires failure_reason_code")
		}
		if err := validateOpaque("failure_reason_code", *fact.FailureReasonCode, 256); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported task status %q", fact.Status)
	}
	if fact.APIProtocol != contractsv1alpha1.AgentAPIProtocolAnthropicMessages {
		return fmt.Errorf("unsupported api_protocol %q", fact.APIProtocol)
	}
	if err := validateUTC("started_at", fact.StartedAt); err != nil {
		return err
	}
	if err := validateUTC("finished_at", fact.FinishedAt); err != nil {
		return err
	}
	if err := validateUTC("observed_at", fact.ObservedAt); err != nil {
		return err
	}
	if fact.FinishedAt.Before(fact.StartedAt) ||
		fact.DurationMS != uint64(fact.FinishedAt.Sub(fact.StartedAt).Milliseconds()) {
		return fmt.Errorf("task duration does not match worker timestamps")
	}
	if fact.ModelTurnsCompleted > fact.ModelTurnsStarted {
		return fmt.Errorf("completed model turns exceed started turns")
	}
	if fact.ModelTurnsCompleted < fact.ModelTurnsStarted &&
		fact.Usage.Completeness == contractsv1alpha1.AgentTokenUsageProviderReported {
		return fmt.Errorf("incomplete model lifecycle cannot claim complete usage")
	}
	if err := validateTokenUsage(fact.Usage); err != nil {
		return err
	}
	if fact.ProvenanceClass != WorkerSelfReportProvenance ||
		fact.Authority != DiagnosticAuthority {
		return fmt.Errorf("task fact must remain diagnostic worker self-report")
	}
	return nil
}

func (fact ToolUsageFact) Validate() error {
	if fact.SchemaVersion != ToolUsageFactSchemaVersion {
		return fmt.Errorf("unsupported tool usage fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"fact_id": fact.FactID, "execution_fact_id": fact.ExecutionFactID,
		"task_fact_id": fact.TaskFactID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := validateOpaque("tool_id", fact.ToolID, 256); err != nil {
		return err
	}
	if fact.FailureCount > fact.InvocationCount {
		return fmt.Errorf("failure_count exceeds invocation_count")
	}
	if fact.ProvenanceClass != WorkerSelfReportProvenance ||
		fact.Authority != DiagnosticAuthority {
		return fmt.Errorf("tool fact must remain diagnostic worker self-report")
	}
	return validateUTC("observed_at", fact.ObservedAt)
}

func (usage ExecutionUsage) Validate(receipts uint64) error {
	total, ok := checkedSum(
		usage.ReportedReceipts,
		usage.PartialReceipts,
		usage.UnavailableReceipts,
	)
	if !ok || total != receipts {
		return fmt.Errorf("usage receipt counts do not reconcile")
	}
	switch usage.Completeness {
	case contractsv1alpha1.AgentTokenUsageProviderReported:
		if receipts == 0 || usage.ReportedReceipts != receipts {
			return fmt.Errorf("provider_reported execution usage requires every receipt reported")
		}
	case contractsv1alpha1.AgentTokenUsagePartial:
		if usage.PartialReceipts == 0 &&
			(usage.ReportedReceipts == 0 || usage.UnavailableReceipts == 0) {
			return fmt.Errorf("partial execution usage requires partial or mixed receipts")
		}
	case contractsv1alpha1.AgentTokenUsageUnavailable:
		if usage.ReportedReceipts != 0 || usage.PartialReceipts != 0 ||
			usage.UnavailableReceipts != receipts {
			return fmt.Errorf("unavailable execution usage cannot claim observed receipts")
		}
		if usage.InputTokens != 0 || usage.OutputTokens != 0 ||
			usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 ||
			usage.ReasoningTokens != 0 || usage.ReasoningReportedReceipts != 0 ||
			usage.TotalTokens != 0 {
			return fmt.Errorf("unavailable execution usage must keep counters zero")
		}
	default:
		return fmt.Errorf("unsupported execution usage completeness %q", usage.Completeness)
	}
	if usage.ReasoningReportedReceipts > usage.ReportedReceipts+usage.PartialReceipts {
		return fmt.Errorf("reasoning token coverage exceeds observed usage receipts")
	}
	return nil
}

func validateTokenUsage(usage contractsv1alpha1.AgentTokenUsage) error {
	sum, ok := checkedSum(
		usage.InputTokens,
		usage.OutputTokens,
		usage.CacheReadTokens,
		usage.CacheWriteTokens,
	)
	if !ok {
		return fmt.Errorf("token counters overflow")
	}
	switch usage.Completeness {
	case contractsv1alpha1.AgentTokenUsageProviderReported:
		if usage.UnavailableReasonCode != nil || usage.TotalTokens != sum {
			return fmt.Errorf("provider_reported usage is inconsistent")
		}
	case contractsv1alpha1.AgentTokenUsagePartial:
		if usage.UnavailableReasonCode == nil || usage.TotalTokens != sum || sum == 0 {
			return fmt.Errorf("partial usage requires a reason and positive observed lower bound")
		}
		if err := validateOpaque("usage.unavailable_reason_code", *usage.UnavailableReasonCode, 256); err != nil {
			return err
		}
	case contractsv1alpha1.AgentTokenUsageUnavailable:
		if usage.UnavailableReasonCode == nil || sum != 0 || usage.TotalTokens != 0 ||
			usage.ReasoningTokens != nil {
			return fmt.Errorf("unavailable usage must keep token counters unobserved")
		}
		if err := validateOpaque("usage.unavailable_reason_code", *usage.UnavailableReasonCode, 256); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported token usage completeness %q", usage.Completeness)
	}
	if usage.ReasoningTokens != nil && *usage.ReasoningTokens > usage.OutputTokens {
		return fmt.Errorf("reasoning_tokens exceeds output_tokens")
	}
	return nil
}

type executionAggregate struct {
	receipts            uint64
	succeeded           uint64
	failed              uint64
	canceled            uint64
	modelTurnsStarted   uint64
	modelTurnsCompleted uint64
	toolCalls           uint64
	usage               ExecutionUsage
}

func (aggregate *executionAggregate) addTask(task TaskExecutionFact) error {
	var ok bool
	if aggregate.receipts, ok = checkedAdd(aggregate.receipts, 1); !ok {
		return fmt.Errorf("receipt count overflows")
	}
	switch task.Status {
	case contractsv1alpha1.AgentTaskSucceeded:
		aggregate.succeeded++
	case contractsv1alpha1.AgentTaskFailed:
		aggregate.failed++
	case contractsv1alpha1.AgentTaskCanceled:
		aggregate.canceled++
	}
	if aggregate.modelTurnsStarted, ok = checkedAdd(
		aggregate.modelTurnsStarted,
		uint64(task.ModelTurnsStarted),
	); !ok {
		return fmt.Errorf("started model turns overflow")
	}
	if aggregate.modelTurnsCompleted, ok = checkedAdd(
		aggregate.modelTurnsCompleted,
		uint64(task.ModelTurnsCompleted),
	); !ok {
		return fmt.Errorf("completed model turns overflow")
	}
	if aggregate.toolCalls, ok = checkedAdd(aggregate.toolCalls, uint64(task.ToolCalls)); !ok {
		return fmt.Errorf("tool calls overflow")
	}
	usage := &aggregate.usage
	switch task.Usage.Completeness {
	case contractsv1alpha1.AgentTokenUsageProviderReported:
		usage.ReportedReceipts++
	case contractsv1alpha1.AgentTokenUsagePartial:
		usage.PartialReceipts++
	case contractsv1alpha1.AgentTokenUsageUnavailable:
		usage.UnavailableReceipts++
	}
	if task.Usage.ReasoningTokens != nil {
		usage.ReasoningReportedReceipts++
		if usage.ReasoningTokens, ok = checkedAdd(
			usage.ReasoningTokens,
			*task.Usage.ReasoningTokens,
		); !ok {
			return fmt.Errorf("reasoning tokens overflow")
		}
	}
	for target, value := range map[*uint64]uint64{
		&usage.InputTokens:      task.Usage.InputTokens,
		&usage.OutputTokens:     task.Usage.OutputTokens,
		&usage.CacheReadTokens:  task.Usage.CacheReadTokens,
		&usage.CacheWriteTokens: task.Usage.CacheWriteTokens,
		&usage.TotalTokens:      task.Usage.TotalTokens,
	} {
		if *target, ok = checkedAdd(*target, value); !ok {
			return fmt.Errorf("usage counters overflow")
		}
	}
	usage.Completeness = aggregateUsageCompleteness(*usage, aggregate.receipts)
	return nil
}

func (aggregate executionAggregate) matches(fact ExecutionFact) error {
	if aggregate.receipts != fact.Receipts || aggregate.succeeded != fact.TasksSucceeded ||
		aggregate.failed != fact.TasksFailed || aggregate.canceled != fact.TasksCanceled ||
		aggregate.modelTurnsStarted != fact.ModelTurnsStarted ||
		aggregate.modelTurnsCompleted != fact.ModelTurnsCompleted ||
		aggregate.toolCalls != fact.ToolCalls || !reflect.DeepEqual(aggregate.usage, fact.Usage) {
		return fmt.Errorf("task facts do not reconcile with execution counters")
	}
	return nil
}

func aggregateUsageCompleteness(
	usage ExecutionUsage,
	receipts uint64,
) contractsv1alpha1.AgentTokenUsageCompleteness {
	if receipts > 0 && usage.ReportedReceipts == receipts {
		return contractsv1alpha1.AgentTokenUsageProviderReported
	}
	if usage.ReportedReceipts > 0 || usage.PartialReceipts > 0 {
		return contractsv1alpha1.AgentTokenUsagePartial
	}
	return contractsv1alpha1.AgentTokenUsageUnavailable
}

func checkedAdd(left, right uint64) (uint64, bool) {
	if math.MaxUint64-left < right {
		return 0, false
	}
	return left + right, true
}

func checkedSum(values ...uint64) (uint64, bool) {
	var result uint64
	for _, value := range values {
		next, ok := checkedAdd(result, value)
		if !ok {
			return 0, false
		}
		result = next
	}
	return result, true
}

func validateGroupBy(groupBy []DimensionName) error {
	if groupBy == nil {
		return fmt.Errorf("group_by must be an explicit array")
	}
	if !slices.Equal(groupBy, canonicalGroupBy(groupBy)) {
		return fmt.Errorf("group_by must be uniquely sorted in canonical order")
	}
	previous := DimensionName("")
	for index, dimension := range groupBy {
		switch dimension {
		case DimensionTenant, DimensionWorkspace, DimensionAgent,
			DimensionProvider, DimensionModel:
		default:
			return fmt.Errorf("unsupported group_by dimension %q", dimension)
		}
		if index > 0 && dimension == previous {
			return fmt.Errorf("duplicate group_by dimension %q", dimension)
		}
		previous = dimension
	}
	return nil
}

func canonicalGroupBy(groupBy []DimensionName) []DimensionName {
	order := map[DimensionName]int{
		DimensionTenant: 0, DimensionWorkspace: 1, DimensionAgent: 2,
		DimensionProvider: 3, DimensionModel: 4,
	}
	result := slices.Clone(groupBy)
	slices.SortFunc(result, func(left, right DimensionName) int {
		return order[left] - order[right]
	})
	return result
}

func versionedDimensionValue(ref contractsv1alpha1.VersionedRef) string {
	return ref.ID + "@" + ref.Revision + "#" + ref.SHA256
}

func sameScope(scope Scope, fact ExecutionFact) bool {
	return scope.TenantID == fact.TenantID && scope.WorkspaceID == fact.WorkspaceID
}

func sortedWarnings(values ...string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

func joinCodes(values []string) string {
	return strings.Join(values, ",")
}
