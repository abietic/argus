// Package analytics defines versioned, provider-neutral facts and deterministic
// read models for review quality, experiments, and attributed value.
//
// The package intentionally owns projections rather than the authoritative
// FindingDecision, Feedback, Outcome, or evaluation ledgers. Callers must
// materialize immutable facts from those ledgers before invoking a projection.
package analytics

import "time"

const (
	FactSetSchemaVersion                       = "argus.analytics_fact_set.v1alpha1"
	ReviewRunFactSchemaVersion                 = "argus.analytics_review_run_fact.v1alpha1"
	StageFactSchemaVersion                     = "argus.analytics_stage_fact.v1alpha1"
	ContextProviderFactSchemaVersion           = "argus.analytics_context_provider_fact.v1alpha1"
	FindingFunnelFactSchemaVersion             = "argus.analytics_finding_funnel_fact.v1alpha1"
	FeedbackOutcomeFactSchemaVersion           = "argus.analytics_feedback_outcome_fact.v1alpha1"
	FindingLineageFactSchemaVersion            = "argus.analytics_finding_lineage_fact.v1alpha1"
	ExperimentFactSchemaVersion                = "argus.analytics_experiment_fact.v1alpha1"
	RepeatabilityFactSchemaVersion             = "argus.analytics_repeatability_fact.v1alpha1"
	ValueObservationSchemaVersion              = "argus.value_observation.v1alpha1"
	DashboardProjectionSchemaVersion           = "argus.analytics_dashboard.v1alpha1"
	DashboardMetricDefinitionVersion           = "argus.analytics_metric_definitions.v1alpha1"
	ROIPolicySchemaVersion                     = "argus.roi_gate_policy.v1alpha1"
	ROIGateResultSchemaVersion                 = "argus.roi_gate_result.v1alpha1"
	ExportManifestSchemaVersion                = "argus.analytics_export_manifest.v1alpha1"
	CanonicalJSONExportFormat                  = "canonical_json"
	CanonicalCSVExportFormat                   = "canonical_csv"
	CanonicalParquetExportFormat               = "parquet"
	ReviewRunParquetSchemaVersion              = "argus.analytics_review_run_parquet.v1alpha1"
	StageParquetSchemaVersion                  = "argus.analytics_stage_parquet.v1alpha1"
	ContextProviderParquetSchemaVersion        = "argus.analytics_context_provider_parquet.v1alpha1"
	FindingFunnelParquetSchemaVersion          = "argus.analytics_finding_funnel_parquet.v1alpha1"
	FeedbackOutcomeParquetSchemaVersion        = "argus.analytics_feedback_outcome_parquet.v1alpha1"
	FindingLineageParquetSchemaVersion         = "argus.analytics_finding_lineage_parquet.v1alpha1"
	ExperimentParquetSchemaVersion             = "argus.analytics_experiment_parquet.v1alpha1"
	RepeatabilityParquetSchemaVersion          = "argus.analytics_repeatability_parquet.v1alpha1"
	ValueObservationParquetSchemaVersion       = "argus.analytics_value_observation_parquet.v1alpha1"
	UnknownDimensionValue                      = "(unknown)"
	parquetNotEmittedLimitation                = "parquet_not_emitted_no_runtime_dependency"
	rateScale                            uint8 = 6
)

// TimeWindow is half-open: StartInclusive <= t < EndExclusive.
type TimeWindow struct {
	StartInclusive time.Time `json:"start_inclusive"`
	EndExclusive   time.Time `json:"end_exclusive"`
}

type Completeness string

const (
	CompletenessComplete Completeness = "complete"
	CompletenessPartial  Completeness = "partial"
	CompletenessUnknown  Completeness = "unknown"
)

type RunStatus string

const (
	RunStatusStarted   RunStatus = "started"
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCanceled  RunStatus = "canceled"
)

type StageStatus string

const (
	StageStatusStarted   StageStatus = "started"
	StageStatusSucceeded StageStatus = "succeeded"
	StageStatusFailed    StageStatus = "failed"
	StageStatusCanceled  StageStatus = "canceled"
)

// Dimensions contains only governed dimensions. Free-form labels are
// deliberately excluded so a producer cannot silently create unbounded
// dashboard cardinality.
type Dimensions struct {
	TenantID         string `json:"tenant_id"`
	OrganizationID   string `json:"organization_id"`
	RepositoryID     string `json:"repository_id"`
	Language         string `json:"language,omitempty"`
	RuleID           string `json:"rule_id,omitempty"`
	Path             string `json:"path,omitempty"`
	ReviewDimension  string `json:"review_dimension,omitempty"`
	WorkflowRevision string `json:"workflow_revision"`
	ConfigRevision   string `json:"config_revision"`
}

type DimensionName string

const (
	DimensionTenant           DimensionName = "tenant"
	DimensionOrganization     DimensionName = "organization"
	DimensionRepository       DimensionName = "repository"
	DimensionLanguage         DimensionName = "language"
	DimensionRule             DimensionName = "rule"
	DimensionPath             DimensionName = "path"
	DimensionReviewDimension  DimensionName = "review_dimension"
	DimensionWorkflowRevision DimensionName = "workflow_revision"
	DimensionConfigRevision   DimensionName = "config_revision"
	DimensionExperiment       DimensionName = "experiment"
	DimensionExperimentArm    DimensionName = "experiment_arm"
	DimensionVariant          DimensionName = "variant"
	DimensionRepeatability    DimensionName = "repeatability"
	DimensionEvaluationCase   DimensionName = "evaluation_case"
	DimensionContextProvider  DimensionName = "context_provider"
	DimensionContextKind      DimensionName = "context_kind"
	DimensionContextGapReason DimensionName = "context_gap_reason"
	DimensionLineageRelation  DimensionName = "finding_lineage_relation"
	DimensionLineageMethod    DimensionName = "finding_lineage_method"
	DimensionLineagePolicy    DimensionName = "finding_lineage_policy"
)

type DimensionValue struct {
	Name  DimensionName `json:"name"`
	Value string        `json:"value"`
}

// SourceRef binds an analytical assertion to immutable source evidence.
type SourceRef struct {
	Kind     SourceKind `json:"kind"`
	ID       string     `json:"id"`
	Revision string     `json:"revision"`
	SHA256   string     `json:"sha256"`
}

type SourceKind string

const (
	SourceReviewRun     SourceKind = "review_run"
	SourceStage         SourceKind = "stage"
	SourceFinding       SourceKind = "finding"
	SourcePublication   SourceKind = "publication"
	SourceFeedback      SourceKind = "feedback"
	SourceOutcome       SourceKind = "outcome"
	SourceExperiment    SourceKind = "experiment"
	SourceRepeatability SourceKind = "repeatability"
	SourceEvaluation    SourceKind = "evaluation"
	SourceBaseline      SourceKind = "baseline"
	SourceModelEstimate SourceKind = "model_estimate"
)

type FunnelSummary struct {
	Candidates uint64 `json:"candidates"`
	Normalized uint64 `json:"normalized"`
	Verified   uint64 `json:"verified"`
	Published  uint64 `json:"published"`
}

type ReviewRunFact struct {
	SchemaVersion      string         `json:"schema_version"`
	FactID             string         `json:"fact_id"`
	RunID              string         `json:"run_id"`
	Dimensions         Dimensions     `json:"dimensions"`
	Status             RunStatus      `json:"status"`
	ResultCompleteness Completeness   `json:"result_completeness"`
	IncompleteReasons  []string       `json:"incomplete_reasons"`
	Funnel             *FunnelSummary `json:"funnel,omitempty"`
	StartedAt          time.Time      `json:"started_at"`
	FinishedAt         *time.Time     `json:"finished_at,omitempty"`
	OccurredAt         time.Time      `json:"occurred_at"`
}

type StageFact struct {
	SchemaVersion      string       `json:"schema_version"`
	FactID             string       `json:"fact_id"`
	RunID              string       `json:"run_id"`
	StageID            string       `json:"stage_id"`
	StageRevision      string       `json:"stage_revision"`
	Attempt            uint32       `json:"attempt"`
	Dimensions         Dimensions   `json:"dimensions"`
	Status             StageStatus  `json:"status"`
	ResultCompleteness Completeness `json:"result_completeness"`
	IncompleteReasons  []string     `json:"incomplete_reasons"`
	DurationMicros     uint64       `json:"duration_micros"`
	OccurredAt         time.Time    `json:"occurred_at"`
}

type ContextProviderBindingMode string

const (
	ContextProviderExecuted ContextProviderBindingMode = "executed"
	ContextProviderReused   ContextProviderBindingMode = "reused"
)

type ContextProviderStatus string

const (
	ContextProviderSucceeded ContextProviderStatus = "succeeded"
	ContextProviderGap       ContextProviderStatus = "gap"
)

// ContextProviderFact is one immutable run-to-receipt binding. Replay creates
// a reused binding rather than another execution attempt, so latency and
// success metrics cannot double-count the source provider invocation.
type ContextProviderFact struct {
	SchemaVersion         string                     `json:"schema_version"`
	FactID                string                     `json:"fact_id"`
	RunID                 string                     `json:"run_id"`
	ReceiptID             string                     `json:"receipt_id"`
	ReceiptSHA256         string                     `json:"receipt_sha256"`
	ReceiptArtifactSHA256 string                     `json:"receipt_artifact_sha256"`
	ProviderID            string                     `json:"provider_id"`
	ProviderRevision      string                     `json:"provider_revision"`
	Kind                  string                     `json:"kind"`
	AdapterID             string                     `json:"adapter_id"`
	AdapterRevision       string                     `json:"adapter_revision"`
	AdapterSHA256         string                     `json:"adapter_sha256"`
	RequestSHA256         string                     `json:"request_sha256"`
	BindingMode           ContextProviderBindingMode `json:"binding_mode"`
	Status                ContextProviderStatus      `json:"status"`
	ReasonCode            string                     `json:"reason_code"`
	ContextID             string                     `json:"context_id"`
	ContextDigest         string                     `json:"context_digest"`
	ContextContract       string                     `json:"context_contract"`
	TargetPathCount       uint64                     `json:"target_path_count"`
	TimeoutMicros         uint64                     `json:"timeout_micros"`
	DurationMicros        uint64                     `json:"duration_micros"`
	Authority             string                     `json:"authority"`
	Dimensions            Dimensions                 `json:"dimensions"`
	OccurredAt            time.Time                  `json:"occurred_at"`
}

type VerificationState string

const (
	VerificationNotReached   VerificationState = "not_reached"
	VerificationVerified     VerificationState = "verified"
	VerificationRejected     VerificationState = "rejected"
	VerificationInconclusive VerificationState = "inconclusive"
	VerificationUnknown      VerificationState = "unknown"
)

type PublicationState string

const (
	PublicationNotReached  PublicationState = "not_reached"
	PublicationRequested   PublicationState = "requested"
	PublicationDispatching PublicationState = "dispatching"
	PublicationPublished   PublicationState = "published"
	PublicationRejected    PublicationState = "rejected"
	PublicationUnknown     PublicationState = "unknown"
	PublicationNotFound    PublicationState = "not_found"
)

type PublicationEligibility string

const (
	EligibilityNotReached      PublicationEligibility = "not_reached"
	EligibilityPublishEligible PublicationEligibility = "publish_eligible"
	EligibilitySuppressed      PublicationEligibility = "suppressed"
	EligibilityHumanQueue      PublicationEligibility = "human_queue"
)

// FindingFunnelFact is one candidate's immutable analytical projection.
// Candidate, normalized Finding, verification, and publication remain distinct
// states so filtering cannot physically erase earlier funnel stages.
type FindingFunnelFact struct {
	SchemaVersion          string                 `json:"schema_version"`
	FactID                 string                 `json:"fact_id"`
	RunID                  string                 `json:"run_id"`
	CandidateID            string                 `json:"candidate_id"`
	FindingID              string                 `json:"finding_id,omitempty"`
	Normalized             bool                   `json:"normalized"`
	Verification           VerificationState      `json:"verification"`
	PublicationEligibility PublicationEligibility `json:"publication_eligibility"`
	Publication            PublicationState       `json:"publication"`
	PublicationRef         *SourceRef             `json:"publication_ref,omitempty"`
	PublicationAt          *time.Time             `json:"publication_at,omitempty"`
	ResultCompleteness     Completeness           `json:"result_completeness"`
	IncompleteReasons      []string               `json:"incomplete_reasons"`
	Dimensions             Dimensions             `json:"dimensions"`
	OccurredAt             time.Time              `json:"occurred_at"`
}

type FeedbackState string

const (
	FeedbackNoFeedback FeedbackState = "no_feedback"
	FeedbackAccepted   FeedbackState = "accepted"
	FeedbackDismissed  FeedbackState = "dismissed"
	FeedbackWontFix    FeedbackState = "wont_fix"
	FeedbackOutdated   FeedbackState = "outdated"
	FeedbackUnknown    FeedbackState = "unknown"
)

type OutcomeState string

const (
	OutcomeNoOutcome OutcomeState = "no_outcome"
	OutcomeFixed     OutcomeState = "fixed"
	OutcomeRecurred  OutcomeState = "recurred"
	OutcomeEscaped   OutcomeState = "escaped"
	OutcomeUnknown   OutcomeState = "unknown"
)

// FeedbackOutcomeFact is a read-only join of two independent ledger
// projections. FeedbackRef and OutcomeRef preserve their separate provenance.
type FeedbackOutcomeFact struct {
	SchemaVersion      string        `json:"schema_version"`
	FactID             string        `json:"fact_id"`
	RunID              string        `json:"run_id"`
	FindingID          string        `json:"finding_id"`
	Feedback           FeedbackState `json:"feedback"`
	FeedbackRef        *SourceRef    `json:"feedback_ref,omitempty"`
	FeedbackAt         *time.Time    `json:"feedback_at,omitempty"`
	Outcome            OutcomeState  `json:"outcome"`
	OutcomeRef         *SourceRef    `json:"outcome_ref,omitempty"`
	OutcomeAt          *time.Time    `json:"outcome_at,omitempty"`
	ResultCompleteness Completeness  `json:"result_completeness"`
	IncompleteReasons  []string      `json:"incomplete_reasons"`
	Dimensions         Dimensions    `json:"dimensions"`
	OccurredAt         time.Time     `json:"occurred_at"`
}

// FindingLineageFact is one immutable relation projection. It deliberately
// carries relationship evidence only: resolved is not a fixed Outcome and
// introduced is not an escaped defect or evaluation label.
type FindingLineageFact struct {
	SchemaVersion          string    `json:"schema_version"`
	FactID                 string    `json:"fact_id"`
	LineageID              string    `json:"lineage_id"`
	LineageArtifactSHA256  string    `json:"lineage_artifact_sha256"`
	RelationID             string    `json:"relation_id"`
	RelationType           string    `json:"relation_type"`
	RelationMethod         string    `json:"relation_method"`
	ReasonCode             string    `json:"reason_code"`
	FamilyKey              string    `json:"family_key"`
	PolicyID               string    `json:"policy_id"`
	PolicyRevision         string    `json:"policy_revision"`
	PolicySHA256           string    `json:"policy_sha256"`
	AncestryAuthority      string    `json:"ancestry_authority"`
	AncestryEvidenceSHA256 string    `json:"ancestry_evidence_sha256,omitempty"`
	RenameMappingCount     uint32    `json:"rename_mapping_count"`
	BaselineRunID          string    `json:"baseline_run_id"`
	VariantRunID           string    `json:"variant_run_id"`
	BaselineFindingIDs     []string  `json:"baseline_finding_ids"`
	VariantFindingIDs      []string  `json:"variant_finding_ids"`
	TenantID               string    `json:"tenant_id"`
	OrganizationID         string    `json:"organization_id"`
	WorkspaceID            string    `json:"workspace_id"`
	RepositoryID           string    `json:"repository_id"`
	OccurredAt             time.Time `json:"occurred_at"`
}

// FixedPoint is an exact decimal: Amount / 10^Scale in Unit.
type FixedPoint struct {
	Amount int64  `json:"amount"`
	Scale  uint8  `json:"scale"`
	Unit   string `json:"unit"`
}

type MetricDirection string

const (
	HigherIsBetter MetricDirection = "higher_is_better"
	LowerIsBetter  MetricDirection = "lower_is_better"
)

type ExperimentArm string

const (
	ExperimentBaseline ExperimentArm = "baseline"
	ExperimentVariant  ExperimentArm = "variant"
)

// ExperimentFact is one immutable arm/metric observation. Reconciliation
// requires a baseline for every metric before variant comparisons are emitted.
type ExperimentFact struct {
	SchemaVersion      string          `json:"schema_version"`
	FactID             string          `json:"fact_id"`
	ExperimentID       string          `json:"experiment_id"`
	ExperimentRevision string          `json:"experiment_revision"`
	Arm                ExperimentArm   `json:"arm"`
	VariantID          string          `json:"variant_id"`
	MetricID           string          `json:"metric_id"`
	MetricVersion      string          `json:"metric_version"`
	MetricDefinition   string          `json:"metric_definition"`
	Direction          MetricDirection `json:"direction"`
	Value              FixedPoint      `json:"value"`
	SampleSize         uint64          `json:"sample_size"`
	Window             TimeWindow      `json:"window"`
	ResultCompleteness Completeness    `json:"result_completeness"`
	IncompleteReasons  []string        `json:"incomplete_reasons"`
	Dimensions         Dimensions      `json:"dimensions"`
	SourceRefs         []SourceRef     `json:"source_refs"`
	OccurredAt         time.Time       `json:"occurred_at"`
}

// RepeatabilityFact is one immutable stability observation derived only from
// independently executed exact-replay samples. It is intentionally separate
// from ExperimentFact: repeatability diagnoses variance and never claims a
// quality improvement or regression.
type RepeatabilityFact struct {
	SchemaVersion         string          `json:"schema_version"`
	FactID                string          `json:"fact_id"`
	RepeatabilityRunID    string          `json:"repeatability_run_id"`
	RepeatabilityRevision string          `json:"repeatability_revision"`
	CaseID                string          `json:"case_id"`
	MetricID              string          `json:"metric_id"`
	MetricVersion         string          `json:"metric_version"`
	MetricDefinition      string          `json:"metric_definition"`
	Direction             MetricDirection `json:"direction"`
	Value                 FixedPoint      `json:"value"`
	SampleSize            uint64          `json:"sample_size"`
	Window                TimeWindow      `json:"window"`
	ResultCompleteness    Completeness    `json:"result_completeness"`
	IncompleteReasons     []string        `json:"incomplete_reasons"`
	Dimensions            Dimensions      `json:"dimensions"`
	SourceRefs            []SourceRef     `json:"source_refs"`
	OccurredAt            time.Time       `json:"occurred_at"`
}

type EvidenceTier string

const (
	EvidenceTierE0 EvidenceTier = "E0"
	EvidenceTierE1 EvidenceTier = "E1"
	EvidenceTierE2 EvidenceTier = "E2"
	EvidenceTierE3 EvidenceTier = "E3"
)

type Baseline struct {
	CohortID   string      `json:"cohort_id"`
	Revision   string      `json:"revision"`
	Window     TimeWindow  `json:"window"`
	Value      FixedPoint  `json:"value"`
	SampleSize uint64      `json:"sample_size"`
	SourceRefs []SourceRef `json:"source_refs"`
}

type Uncertainty struct {
	Lower         FixedPoint `json:"lower"`
	Upper         FixedPoint `json:"upper"`
	ConfidenceBPS uint16     `json:"confidence_bps"`
	Method        string     `json:"method"`
	Revision      string     `json:"revision"`
}

// CostBreakdown deliberately lists every required ROI cost category.
type CostBreakdown struct {
	ModelCompute       FixedPoint `json:"model_compute"`
	HumanVerification  FixedPoint `json:"human_verification"`
	FalsePositive      FixedPoint `json:"false_positive_interruption"`
	PlatformOperations FixedPoint `json:"platform_operations"`
	Storage            FixedPoint `json:"storage"`
}

type ValueObservation struct {
	SchemaVersion             string        `json:"schema_version"`
	ObservationID             string        `json:"observation_id"`
	Dimensions                Dimensions    `json:"dimensions"`
	MetricID                  string        `json:"metric_id"`
	EvidenceTier              EvidenceTier  `json:"evidence_tier"`
	Baseline                  Baseline      `json:"baseline"`
	ObservationWindow         TimeWindow    `json:"observation_window"`
	AttributionWindow         TimeWindow    `json:"attribution_window"`
	SampleSize                uint64        `json:"sample_size"`
	GrossValue                FixedPoint    `json:"gross_value"`
	Costs                     CostBreakdown `json:"costs"`
	NetValue                  FixedPoint    `json:"net_value"`
	NetValueUncertainty       Uncertainty   `json:"net_value_uncertainty"`
	SourceRefs                []SourceRef   `json:"source_refs"`
	AttributionPolicyRevision string        `json:"attribution_policy_revision"`
	ObservedAt                time.Time     `json:"observed_at"`
}

type FactSet struct {
	SchemaVersion     string                `json:"schema_version"`
	Window            TimeWindow            `json:"window"`
	Completeness      Completeness          `json:"completeness"`
	IncompleteReasons []string              `json:"incomplete_reasons"`
	ReviewRuns        []ReviewRunFact       `json:"review_runs"`
	Stages            []StageFact           `json:"stages"`
	ContextProviders  []ContextProviderFact `json:"context_providers"`
	Findings          []FindingFunnelFact   `json:"finding_funnel"`
	FeedbackOutcomes  []FeedbackOutcomeFact `json:"feedback_outcomes"`
	FindingLineages   []FindingLineageFact  `json:"finding_lineages"`
	Experiments       []ExperimentFact      `json:"experiments"`
	Repeatability     []RepeatabilityFact   `json:"repeatability"`
	ValueObservations []ValueObservation    `json:"value_observations"`
}

type TileAvailability string

const (
	TileObserved TileAvailability = "observed"
	TileUnknown  TileAvailability = "unknown"
)

type ValueQualifier string

const (
	ValueExact       ValueQualifier = "exact"
	ValueLowerBound  ValueQualifier = "lower_bound"
	ValueSampleOnly  ValueQualifier = "sample_only"
	ValueUnavailable ValueQualifier = "unavailable"
)

type DashboardTile struct {
	TileID        string           `json:"tile_id"`
	MetricID      string           `json:"metric_id"`
	MetricVersion string           `json:"metric_version"`
	Definition    string           `json:"definition"`
	Window        TimeWindow       `json:"window"`
	Dimensions    []DimensionValue `json:"dimensions"`
	SampleSize    uint64           `json:"sample_size"`
	Availability  TileAvailability `json:"availability"`
	Qualifier     ValueQualifier   `json:"qualifier"`
	Value         *FixedPoint      `json:"value,omitempty"`
	Warnings      []string         `json:"warnings"`
}

type DashboardProjection struct {
	SchemaVersion string          `json:"schema_version"`
	Window        TimeWindow      `json:"window"`
	GroupBy       []DimensionName `json:"group_by"`
	Tiles         []DashboardTile `json:"tiles"`
}

type ROIPolicy struct {
	SchemaVersion                     string       `json:"schema_version"`
	PolicyID                          string       `json:"policy_id"`
	Revision                          string       `json:"revision"`
	RequiredAttributionPolicyRevision string       `json:"required_attribution_policy_revision"`
	MinimumEvidenceTier               EvidenceTier `json:"minimum_evidence_tier"`
	MinimumSampleSize                 uint64       `json:"minimum_sample_size"`
	MinimumNetValue                   FixedPoint   `json:"minimum_net_value"`
	MinimumConfidenceBPS              uint16       `json:"minimum_confidence_bps"`
}

type ROIGateResult struct {
	SchemaVersion  string   `json:"schema_version"`
	ObservationID  string   `json:"observation_id"`
	PolicyID       string   `json:"policy_id"`
	PolicyRevision string   `json:"policy_revision"`
	Eligible       bool     `json:"eligible"`
	ReasonCodes    []string `json:"reason_codes"`
}

type ExportFileManifest struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Contract  string `json:"contract,omitempty"`
	Ref       string `json:"ref,omitempty"`
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
	Parquet              bool                 `json:"parquet"`
	Limitations          []string             `json:"limitations"`
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
