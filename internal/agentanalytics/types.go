// Package agentanalytics owns diagnostic-only analytical facts and immutable
// projections for imported Agent Review shadow executions.
//
// These types are deliberately separate from internal/analytics. They do not
// create ReviewRunFact, StageFact, FindingFunnelFact, ValueObservation, or ROI
// evidence, and worker self-reports never become billing truth.
package agentanalytics

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"argus.local/argus/internal/agentshadow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	FactSetSchemaVersion            = "argus.agent_execution_fact_set.v1alpha3"
	ExecutionFactSchemaVersion      = "argus.agent_execution_fact.v1alpha3"
	TaskFactSchemaVersion           = "argus.agent_task_execution_fact.v1alpha3"
	ToolUsageFactSchemaVersion      = "argus.agent_tool_usage_fact.v1alpha3"
	ProjectionSchemaVersion         = "argus.agent_execution_projection.v1alpha3"
	MetricDefinitionVersion         = "argus.agent_execution_metric_definitions.v1alpha3"
	ProjectionSnapshotSchemaVersion = "argus.agent_execution_projection_snapshot.v1alpha3"
	ProjectionManifestSchemaVersion = "argus.agent_execution_projection_manifest.v1alpha3"
	ProjectionPolicyRevision        = "argus.agent_execution_projection_policy.v1alpha3"
	ExportManifestSchemaVersion     = "argus.agent_execution_export_manifest.v1alpha3"
	CanonicalJSONExportFormat       = "canonical_json"
	CanonicalCSVExportFormat        = "canonical_csv"
	DiagnosticAuthority             = "diagnostic_only"
	HostObservationProvenance       = "argus_host_observation"
	WorkerSelfReportProvenance      = "worker_self_report"
	ShadowDisposition               = "shadow_only"
	NonAttestedWarning              = "non_attested"
	DiagnosticOnlyWarning           = "diagnostic_only"
	WorkerSelfReportWarning         = "worker_self_report"
	UnknownDimensionValue           = "(unknown)"
	projectionManifestRoot          = "agent-analytics/projection-manifests/"
)

type TimeWindow struct {
	StartInclusive time.Time `json:"start_inclusive"`
	EndExclusive   time.Time `json:"end_exclusive"`
}

type Scope struct {
	TenantID    string `json:"tenant_id"`
	WorkspaceID string `json:"workspace_id"`
}

type DimensionName string

const (
	DimensionTenant    DimensionName = "tenant"
	DimensionWorkspace DimensionName = "workspace"
	DimensionAgent     DimensionName = "agent"
	DimensionProvider  DimensionName = "provider"
	DimensionModel     DimensionName = "model"
)

type DimensionValue struct {
	Name  DimensionName `json:"name"`
	Value string        `json:"value"`
}

// ExecutionUsage is a diagnostic aggregation of receipt counters. Its
// completeness semantics remain identical to AgentTokenUsage: partial values
// are lower bounds and unavailable values are not silently represented as an
// observed zero.
type ExecutionUsage struct {
	Completeness              contractsv1alpha1.AgentTokenUsageCompleteness `json:"completeness"`
	ReportedReceipts          uint64                                        `json:"reported_receipts"`
	PartialReceipts           uint64                                        `json:"partial_receipts"`
	UnavailableReceipts       uint64                                        `json:"unavailable_receipts"`
	InputTokens               uint64                                        `json:"input_tokens"`
	OutputTokens              uint64                                        `json:"output_tokens"`
	CacheReadTokens           uint64                                        `json:"cache_read_tokens"`
	CacheWriteTokens          uint64                                        `json:"cache_write_tokens"`
	ReasoningTokens           uint64                                        `json:"reasoning_tokens"`
	ReasoningReportedReceipts uint64                                        `json:"reasoning_reported_receipts"`
	TotalTokens               uint64                                        `json:"total_tokens"`
}

// ExecutionStatus is the diagnostic host view of one execution. It is wider
// than AgentReviewRunStatus because a worker can terminate before a result
// manifest exists, or leave only an immutable execution intent behind.
type ExecutionStatus string

const (
	ExecutionStatusComplete       ExecutionStatus = "complete"
	ExecutionStatusPartial        ExecutionStatus = "partial"
	ExecutionStatusFailed         ExecutionStatus = "failed"
	ExecutionStatusCanceled       ExecutionStatus = "canceled"
	ExecutionStatusUnknownOutcome ExecutionStatus = "unknown_outcome"
)

type ExecutionFact struct {
	SchemaVersion string `json:"schema_version"`
	FactID        string `json:"fact_id"`
	ObservationID string `json:"observation_id"`
	ManifestID    string `json:"manifest_id"`
	SourceRunID   string `json:"source_run_id"`
	ExecutionID   string `json:"execution_id"`
	ReviewRunID   string `json:"review_run_id"`
	TenantID      string `json:"tenant_id"`
	WorkspaceID   string `json:"workspace_id"`

	Agent       contractsv1alpha1.VersionedRef     `json:"agent"`
	Provider    contractsv1alpha1.VersionedRef     `json:"provider"`
	Model       contractsv1alpha1.VersionedRef     `json:"model"`
	APIProtocol contractsv1alpha1.AgentAPIProtocol `json:"api_protocol"`

	ExecutionClass contractsv1alpha1.AgentReviewExecutionClass `json:"execution_class"`
	Attestation    contractsv1alpha1.AgentReviewAttestation    `json:"attestation"`
	Disposition    string                                      `json:"disposition"`
	Status         ExecutionStatus                             `json:"status"`
	DurationMS     uint64                                      `json:"duration_ms"`
	ReasonCodes    []string                                    `json:"reason_codes"`

	Receipts            uint64         `json:"receipts"`
	TasksSucceeded      uint64         `json:"tasks_succeeded"`
	TasksFailed         uint64         `json:"tasks_failed"`
	TasksCanceled       uint64         `json:"tasks_canceled"`
	ModelTurnsStarted   uint64         `json:"model_turns_started"`
	ModelTurnsCompleted uint64         `json:"model_turns_completed"`
	ToolCalls           uint64         `json:"tool_calls"`
	Usage               ExecutionUsage `json:"usage"`

	ProvenanceClass string    `json:"provenance_class"`
	Authority       string    `json:"authority"`
	ObservedAt      time.Time `json:"observed_at"`
}

type TaskExecutionFact struct {
	SchemaVersion          string  `json:"schema_version"`
	FactID                 string  `json:"fact_id"`
	ExecutionFactID        string  `json:"execution_fact_id"`
	ReceiptID              string  `json:"receipt_id"`
	TaskID                 string  `json:"task_id"`
	GroupID                string  `json:"group_id"`
	HypothesisOccurrenceID *string `json:"hypothesis_occurrence_id,omitempty"`

	Role        contractsv1alpha1.AgentTaskRole    `json:"role"`
	Dimension   contractsv1alpha1.VersionedRef     `json:"dimension"`
	Runtime     contractsv1alpha1.VersionedRef     `json:"runtime"`
	Profile     contractsv1alpha1.VersionedRef     `json:"profile"`
	Agent       contractsv1alpha1.VersionedRef     `json:"agent"`
	Provider    contractsv1alpha1.VersionedRef     `json:"provider"`
	Model       contractsv1alpha1.VersionedRef     `json:"model"`
	APIProtocol contractsv1alpha1.AgentAPIProtocol `json:"api_protocol"`

	Status              contractsv1alpha1.AgentTaskStatus `json:"status"`
	FailureReasonCode   *string                           `json:"failure_reason_code,omitempty"`
	StartedAt           time.Time                         `json:"started_at"`
	FinishedAt          time.Time                         `json:"finished_at"`
	DurationMS          uint64                            `json:"duration_ms"`
	ModelTurnsStarted   uint32                            `json:"model_turns_started"`
	ModelTurnsCompleted uint32                            `json:"model_turns_completed"`
	ToolCalls           uint32                            `json:"tool_calls"`
	Usage               contractsv1alpha1.AgentTokenUsage `json:"usage"`

	ProvenanceClass string    `json:"provenance_class"`
	Authority       string    `json:"authority"`
	ObservedAt      time.Time `json:"observed_at"`
}

type ToolUsageFact struct {
	SchemaVersion   string    `json:"schema_version"`
	FactID          string    `json:"fact_id"`
	ExecutionFactID string    `json:"execution_fact_id"`
	TaskFactID      string    `json:"task_fact_id"`
	ToolID          string    `json:"tool_id"`
	InvocationCount uint32    `json:"invocation_count"`
	FailureCount    uint32    `json:"failure_count"`
	ProvenanceClass string    `json:"provenance_class"`
	Authority       string    `json:"authority"`
	ObservedAt      time.Time `json:"observed_at"`
}

type FactSet struct {
	SchemaVersion string              `json:"schema_version"`
	Window        TimeWindow          `json:"window"`
	Executions    []ExecutionFact     `json:"executions"`
	Tasks         []TaskExecutionFact `json:"tasks"`
	ToolUsage     []ToolUsageFact     `json:"tool_usage"`
}

type SourceBindingKind string

const (
	SourceBindingResult  SourceBindingKind = "result"
	SourceBindingAttempt SourceBindingKind = "attempt"
)

// ResultSourceBinding closes a rich execution fact over the immutable import
// record, observation, and four exact result artifacts.
type ResultSourceBinding struct {
	ManifestID           string                            `json:"manifest_id"`
	ObservationID        string                            `json:"observation_id"`
	InputDigest          string                            `json:"input_digest"`
	ImportRecordSHA256   string                            `json:"import_record_sha256"`
	ObservationSHA256    string                            `json:"observation_sha256"`
	AgentReviewPlanRef   contractsv1alpha1.ArtifactBinding `json:"agent_review_plan_ref"`
	HypothesisSetRef     contractsv1alpha1.ArtifactBinding `json:"hypothesis_set_ref"`
	ReceiptCollectionRef contractsv1alpha1.ArtifactBinding `json:"receipt_collection_ref"`
	ManifestRef          contractsv1alpha1.ArtifactBinding `json:"manifest_ref"`
}

// AttemptSourceBinding closes a fact over the accepted execution intent and,
// when terminal, its completion. A succeeded attempt additionally carries the
// strict result closure used to build its rich task and tool facts. Failed,
// canceled, and unknown outcomes deliberately have no fabricated result.
type AttemptSourceBinding struct {
	Status           agentshadow.ExecutionStatus `json:"status"`
	ExecutionID      string                      `json:"execution_id"`
	IdempotencyKey   string                      `json:"idempotency_key"`
	SemanticDigest   string                      `json:"semantic_digest"`
	IntentSHA256     string                      `json:"intent_sha256"`
	CompletionSHA256 *string                     `json:"completion_sha256,omitempty"`
	AcceptedAt       time.Time                   `json:"accepted_at"`
	ObservedAt       time.Time                   `json:"observed_at"`
	CompletedAt      *time.Time                  `json:"completed_at,omitempty"`
	HostFailure      *HostFailureSourceBinding   `json:"host_failure,omitempty"`
	Result           *ResultSourceBinding        `json:"result,omitempty"`
}

// HostFailureSourceBinding preserves the complete bounded immutable host
// diagnostic plus its canonical digest. It explains an unknown outcome and
// may remain attached to a terminal attempt when persistence returned an
// unconfirmed error after the completion file became visible. Such a later
// diagnosis does not move the terminal execution's observation window.
// Raw provider/worker errors are never part of this binding.
type HostFailureSourceBinding struct {
	Observation       agentshadow.ExecutionHostFailureObservation `json:"observation"`
	ObservationSHA256 string                                      `json:"observation_sha256"`
}

// SourceBinding is a strict discriminated union. Exactly one of Result or
// Attempt must be present; this prevents result-less attempts from acquiring
// synthetic manifest or observation provenance.
type SourceBinding struct {
	ExecutionFactID string                `json:"execution_fact_id"`
	Kind            SourceBindingKind     `json:"kind"`
	Result          *ResultSourceBinding  `json:"result,omitempty"`
	Attempt         *AttemptSourceBinding `json:"attempt,omitempty"`
}

type TileAvailability string

const (
	TileObserved TileAvailability = "observed"
	TileUnknown  TileAvailability = "unknown"
)

type MetricQualifier string

const (
	QualifierHostObserved   MetricQualifier = "host_observed"
	QualifierWorkerReported MetricQualifier = "worker_reported"
	QualifierLowerBound     MetricQualifier = "lower_bound"
	QualifierUnavailable    MetricQualifier = "unavailable"
)

type MetricTile struct {
	TileID        string           `json:"tile_id"`
	MetricID      string           `json:"metric_id"`
	MetricVersion string           `json:"metric_version"`
	Definition    string           `json:"definition"`
	Window        TimeWindow       `json:"window"`
	Dimensions    []DimensionValue `json:"dimensions"`
	SampleSize    uint64           `json:"sample_size"`
	Availability  TileAvailability `json:"availability"`
	Qualifier     MetricQualifier  `json:"qualifier"`
	Value         *uint64          `json:"value,omitempty"`
	Unit          string           `json:"unit"`
	Authority     string           `json:"authority"`
	Warnings      []string         `json:"warnings"`
}

type Projection struct {
	SchemaVersion string          `json:"schema_version"`
	Window        TimeWindow      `json:"window"`
	GroupBy       []DimensionName `json:"group_by"`
	Tiles         []MetricTile    `json:"tiles"`
}

type ProjectionSnapshot struct {
	SchemaVersion  string          `json:"schema_version"`
	SnapshotID     string          `json:"snapshot_id"`
	PolicyRevision string          `json:"policy_revision"`
	Scope          Scope           `json:"scope"`
	Window         TimeWindow      `json:"window"`
	GroupBy        []DimensionName `json:"group_by"`
	BuiltAt        time.Time       `json:"built_at"`
	SourceBindings []SourceBinding `json:"source_bindings"`
	Facts          FactSet         `json:"facts"`
	Projection     Projection      `json:"projection"`
}

type ExportFileManifest struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	RowCount  uint64 `json:"row_count"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type ExportManifest struct {
	SchemaVersion        string               `json:"schema_version"`
	DatasetSchemaVersion string               `json:"dataset_schema_version"`
	Format               string               `json:"format"`
	Window               TimeWindow           `json:"window"`
	FactCount            uint64               `json:"fact_count"`
	Files                []ExportFileManifest `json:"files"`
}

type ExportFile struct {
	Manifest ExportFileManifest
	Data     []byte
}

type ExportBundle struct {
	Manifest ExportManifest
	Files    []ExportFile
}

func (window TimeWindow) Validate() error {
	if err := validateUTC("start_inclusive", window.StartInclusive); err != nil {
		return err
	}
	if err := validateUTC("end_exclusive", window.EndExclusive); err != nil {
		return err
	}
	if !window.StartInclusive.Before(window.EndExclusive) {
		return fmt.Errorf("time window must be non-empty and increasing")
	}
	return nil
}

func (window TimeWindow) contains(value time.Time) bool {
	return !value.Before(window.StartInclusive) && value.Before(window.EndExclusive)
}

func (scope Scope) Validate() error {
	if err := validateOpaque("tenant_id", scope.TenantID, 256); err != nil {
		return err
	}
	return validateOpaque("workspace_id", scope.WorkspaceID, 256)
}

func validateIdentifier(name, value string) error {
	if value == "" || len(value) > 255 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) ||
			strings.ContainsRune("._:@+-", character) {
			continue
		}
		return fmt.Errorf("%s contains unsupported character %q", name, character)
	}
	return nil
}

// validateOpaque mirrors the shadow contract's bounded, trimmed, control-free
// identifier semantics. Provider/model/tool/reason values are external opaque
// text and must not inherit the narrower local-storage key alphabet.
func validateOpaque(name, value string, maximum int) error {
	if value == "" || len(value) > maximum || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf(
			"%s must be a non-empty trimmed string of at most %d bytes",
			name,
			maximum,
		)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != 64 {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
		}
	}
	return nil
}

func validateUTC(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	_, offset := value.Zone()
	if offset != 0 {
		return fmt.Errorf("%s must use UTC", name)
	}
	return nil
}

func validateVersionedRef(name string, ref contractsv1alpha1.VersionedRef) error {
	if err := validateOpaque(name+".id", ref.ID, 256); err != nil {
		return err
	}
	if err := validateOpaque(name+".revision", ref.Revision, 256); err != nil {
		return err
	}
	return validateSHA256(name+".sha256", ref.SHA256)
}

func validateSortedCodes(name string, values []string) error {
	if values == nil {
		return fmt.Errorf("%s must be an explicit array", name)
	}
	if !slices.IsSorted(values) {
		return fmt.Errorf("%s must be sorted", name)
	}
	for index, value := range values {
		if err := validateOpaque(fmt.Sprintf("%s[%d]", name, index), value, 256); err != nil {
			return err
		}
		if index > 0 && value == values[index-1] {
			return fmt.Errorf("%s must be unique", name)
		}
	}
	return nil
}
