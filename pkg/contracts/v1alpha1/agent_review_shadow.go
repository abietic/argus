package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	AgentReviewPlanSchemaVersion                 = "argus.agent_review_plan.v1alpha1"
	ReviewHypothesisSetSchemaVersion             = "argus.review_hypothesis_set.v1alpha1"
	AgentExecutionReceiptSchemaVersion           = "argus.agent_execution_receipt.v1alpha1"
	AgentExecutionReceiptCollectionSchemaVersion = "argus.agent_execution_receipt_collection.v1alpha1"
	AgentExecutionReceiptCollectionContract      = AgentExecutionReceiptCollectionSchemaVersion
	AgentReviewResultManifestSchemaVersion       = "argus.agent_review_result_manifest.v1alpha1"
	AgentReviewObservationSchemaVersion          = "argus.agent_review_observation.v1alpha1"
	AgentReviewExecutionSnapshotContract         = "argus.execution_snapshot.v1alpha1"
	AgentReviewInputContract                     = "argus.review_input.v1alpha1"

	// These are the concrete stdio admission limits enforced by
	// runtime/pi-review/src/protocol.ts. Go, JSON Schema, and TypeScript must
	// change together so a host-admitted request cannot be rejected downstream.
	agentReviewMaxFiles             = uint32(1_000)
	agentReviewMaxGroups            = uint32(256)
	agentReviewMaxCandidates        = uint32(1_000)
	agentReviewMaxModelCalls        = uint32(2_000)
	agentReviewMaxToolCalls         = uint32(100)
	agentReviewMaxOutputTokens      = uint32(32_768)
	agentReviewMaxGroupBytes        = uint64(1 << 20)
	agentReviewMaxTargetBytes       = uint64(64 << 20)
	agentReviewMaxTimeoutMS         = uint64(3_600_000)
	agentReviewMaxConcurrency       = uint32(16)
	agentReviewMaxArtifactBytes     = int64(64 << 20)
	agentReviewMaxReviewDimensions  = AgentReviewWorkerMaxSkillCount
	agentReviewWorkerToolListFiles  = "list_files"
	agentReviewWorkerToolReadFile   = "read_file"
	agentReviewWorkerToolSearchCode = "search_code"
)

type AgentReviewExecutionClass string

const AgentReviewExecutionLocalDirectProviderShadow AgentReviewExecutionClass = "local_direct_provider_shadow"

type AgentReviewAttestation string

const AgentReviewAttestationNonAttested AgentReviewAttestation = "non_attested"

type AgentAPIProtocol string

const AgentAPIProtocolAnthropicMessages AgentAPIProtocol = "anthropic_messages"

type AgentReviewToolPolicy struct {
	AllowedTools    []string `json:"allowed_tools"`
	ToolNetwork     string   `json:"tool_network"`
	WorkspaceWrites string   `json:"workspace_writes"`
	RemoteWrites    string   `json:"remote_writes"`
}

type AgentReviewBudget struct {
	MaxFiles        uint32 `json:"max_files"`
	MaxGroups       uint32 `json:"max_groups"`
	MaxCandidates   uint32 `json:"max_candidates"`
	MaxModelCalls   uint32 `json:"max_model_calls"`
	MaxToolCalls    uint32 `json:"max_tool_calls"`
	MaxOutputTokens uint32 `json:"max_output_tokens"`
	MaxGroupBytes   uint64 `json:"max_group_bytes"`
	MaxTargetBytes  uint64 `json:"max_target_bytes"`
	TimeoutMS       uint64 `json:"timeout_ms"`
	MaxConcurrency  uint32 `json:"max_concurrency"`
}

// AgentReviewPlan is a local shadow execution input. It is intentionally not a
// StageExecutionRequest: direct provider execution is non-attested and must not
// be confused with a Hailix-dispatched, fenced execution.
type AgentReviewPlan struct {
	SchemaVersion        string                    `json:"schema_version"`
	PlanID               string                    `json:"plan_id"`
	SourceRunID          string                    `json:"source_run_id"`
	ExecutionID          string                    `json:"execution_id"`
	ReviewRunID          string                    `json:"review_run_id"`
	ExecutionSnapshotRef ArtifactBinding           `json:"execution_snapshot_ref"`
	ReviewInputRef       ArtifactBinding           `json:"review_input_ref"`
	TargetDigest         string                    `json:"target_digest"`
	RulePack             VersionedRef              `json:"rule_pack"`
	Implementation       VersionedRef              `json:"implementation"`
	Normalization        VersionedRef              `json:"normalization"`
	Grouping             VersionedRef              `json:"grouping"`
	ContextDimensions    []VersionedRef            `json:"context_dimensions"`
	ReviewDimensions     []VersionedRef            `json:"review_dimensions"`
	Verifier             VersionedRef              `json:"verifier"`
	Knowledge            []VersionedRef            `json:"knowledge"`
	Runtime              VersionedRef              `json:"runtime"`
	Profile              VersionedRef              `json:"profile"`
	Agent                VersionedRef              `json:"agent"`
	Provider             VersionedRef              `json:"provider"`
	Model                VersionedRef              `json:"model"`
	APIProtocol          AgentAPIProtocol          `json:"api_protocol"`
	ToolPolicy           AgentReviewToolPolicy     `json:"tool_policy"`
	Budget               AgentReviewBudget         `json:"budget"`
	VerificationRequired bool                      `json:"verification_required"`
	ExecutionClass       AgentReviewExecutionClass `json:"execution_class"`
	Attestation          AgentReviewAttestation    `json:"attestation"`
	SideEffects          string                    `json:"side_effects"`
	CreatedAt            time.Time                 `json:"created_at"`
}

type HypothesisSeverity string

const (
	HypothesisSeverityCritical HypothesisSeverity = "critical"
	HypothesisSeverityHigh     HypothesisSeverity = "high"
	HypothesisSeverityMedium   HypothesisSeverity = "medium"
	HypothesisSeverityLow      HypothesisSeverity = "low"
)

type HypothesisAnchorSide string

const (
	HypothesisAnchorOld  HypothesisAnchorSide = "old"
	HypothesisAnchorNew  HypothesisAnchorSide = "new"
	HypothesisAnchorFile HypothesisAnchorSide = "file"
)

type HypothesisSourceAnchor struct {
	Path         string               `json:"path"`
	Side         HypothesisAnchorSide `json:"side"`
	StartLine    uint32               `json:"start_line"`
	EndLine      uint32               `json:"end_line"`
	SourceDigest string               `json:"source_digest"`
}

type HypothesisEvidence struct {
	EvidenceID     string                 `json:"evidence_id"`
	Statement      string                 `json:"statement"`
	Anchor         HypothesisSourceAnchor `json:"anchor"`
	Excerpt        string                 `json:"excerpt"`
	EvidenceDigest string                 `json:"evidence_digest"`
}

type HypothesisVerificationVerdict string

const (
	HypothesisVerificationConfirmed    HypothesisVerificationVerdict = "confirmed"
	HypothesisVerificationRejected     HypothesisVerificationVerdict = "rejected"
	HypothesisVerificationInconclusive HypothesisVerificationVerdict = "inconclusive"
)

// HypothesisVerificationObservation is evidence about a hypothesis, not a
// FindingDecision. Even a confirmed observation remains shadow-only input to
// later Argus governance.
type HypothesisVerificationObservation struct {
	ObservationID string                        `json:"observation_id"`
	Sequence      uint32                        `json:"sequence"`
	Verifier      VersionedRef                  `json:"verifier"`
	Verdict       HypothesisVerificationVerdict `json:"verdict"`
	ReasonCode    string                        `json:"reason_code"`
	Explanation   string                        `json:"explanation"`
	EvidenceIDs   []string                      `json:"evidence_ids"`
}

// ReviewHypothesis is one canonical retained detector/reviewer occurrence.
// Duplicate raw candidates remain in NormalizationDecisions and point at the
// canonical occurrence; they are not materialized as extra hypotheses or
// promoted to Findings.
type ReviewHypothesis struct {
	OccurrenceID           string                              `json:"occurrence_id"`
	ClusterFingerprint     string                              `json:"cluster_fingerprint"`
	GroupID                string                              `json:"group_id"`
	Dimension              VersionedRef                        `json:"dimension"`
	Category               string                              `json:"category"`
	Severity               HypothesisSeverity                  `json:"severity"`
	RawConfidenceAvailable bool                                `json:"raw_confidence_available"`
	RawConfidencePPM       uint32                              `json:"raw_confidence_ppm"`
	Title                  string                              `json:"title"`
	Description            string                              `json:"description"`
	Impact                 string                              `json:"impact"`
	Anchor                 HypothesisSourceAnchor              `json:"anchor"`
	Evidence               []HypothesisEvidence                `json:"evidence"`
	Suggestion             *string                             `json:"suggestion,omitempty"`
	Verification           []HypothesisVerificationObservation `json:"verification_observations"`
}

type HypothesisDedupCluster struct {
	ClusterID             string   `json:"cluster_id"`
	Fingerprint           string   `json:"fingerprint"`
	CanonicalOccurrenceID string   `json:"canonical_occurrence_id"`
	OccurrenceIDs         []string `json:"occurrence_ids"`
}

type HypothesisNormalizationAction string

const (
	HypothesisNormalizationRetained        HypothesisNormalizationAction = "retained"
	HypothesisNormalizationMergedDuplicate HypothesisNormalizationAction = "merged_duplicate"
	HypothesisNormalizationRejectedInvalid HypothesisNormalizationAction = "rejected_invalid"
	HypothesisNormalizationExcludedBudget  HypothesisNormalizationAction = "excluded_budget"
)

// HypothesisNormalizationDecision preserves the fate of every raw reviewer
// candidate without persisting an unbounded/raw model payload. ClaimDigest
// binds the exact raw claim artifact; occurrence lineage is action-dependent.
type HypothesisNormalizationDecision struct {
	RawCandidateID        string                        `json:"raw_candidate_id"`
	ClaimDigest           string                        `json:"claim_digest"`
	Action                HypothesisNormalizationAction `json:"action"`
	ReasonCode            string                        `json:"reason_code"`
	OccurrenceID          *string                       `json:"occurrence_id,omitempty"`
	CanonicalOccurrenceID *string                       `json:"canonical_occurrence_id,omitempty"`
}

type AgentReviewCoveragePhase string

const (
	AgentReviewCoverageCapture      AgentReviewCoveragePhase = "capture"
	AgentReviewCoverageContext      AgentReviewCoveragePhase = "context"
	AgentReviewCoverageReview       AgentReviewCoveragePhase = "review"
	AgentReviewCoverageVerification AgentReviewCoveragePhase = "verification"
)

type AgentReviewCoverageGap struct {
	GapID      string                   `json:"gap_id"`
	Phase      AgentReviewCoveragePhase `json:"phase"`
	SubjectID  string                   `json:"subject_id"`
	ReasonCode string                   `json:"reason_code"`
}

type AgentReviewCoverage struct {
	GroupsTotal          uint32                   `json:"groups_total"`
	GroupsReviewed       uint32                   `json:"groups_reviewed"`
	ReviewTasksTotal     uint32                   `json:"review_tasks_total"`
	ReviewTasksSucceeded uint32                   `json:"review_tasks_succeeded"`
	FilesIncluded        uint32                   `json:"files_included"`
	Gaps                 []AgentReviewCoverageGap `json:"gaps"`
}

type AgentReviewCompleteness string

const (
	AgentReviewComplete AgentReviewCompleteness = "complete"
	AgentReviewPartial  AgentReviewCompleteness = "partial"
)

type ReviewHypothesisSet struct {
	SchemaVersion          string                            `json:"schema_version"`
	HypothesisSetID        string                            `json:"hypothesis_set_id"`
	PlanID                 string                            `json:"plan_id"`
	SourceRunID            string                            `json:"source_run_id"`
	ExecutionID            string                            `json:"execution_id"`
	ReviewRunID            string                            `json:"review_run_id"`
	TargetDigest           string                            `json:"target_digest"`
	Completeness           AgentReviewCompleteness           `json:"completeness"`
	CompletenessReasons    []string                          `json:"completeness_reasons"`
	NormalizationDecisions []HypothesisNormalizationDecision `json:"normalization_decisions"`
	Hypotheses             []ReviewHypothesis                `json:"hypotheses"`
	DedupClusters          []HypothesisDedupCluster          `json:"dedup_clusters"`
	Coverage               AgentReviewCoverage               `json:"coverage"`
	GeneratedAt            time.Time                         `json:"generated_at"`
}

type AgentReceiptProvenanceClass string

const AgentReceiptProvenanceWorkerSelfReport AgentReceiptProvenanceClass = "worker_self_report"

type AgentReceiptAuthority string

const AgentReceiptAuthorityDiagnosticOnly AgentReceiptAuthority = "diagnostic_only"

type AgentTaskRole string

const (
	AgentTaskContext      AgentTaskRole = "context"
	AgentTaskReview       AgentTaskRole = "review"
	AgentTaskVerification AgentTaskRole = "verification"
)

type AgentTaskStatus string

const (
	AgentTaskSucceeded AgentTaskStatus = "succeeded"
	AgentTaskFailed    AgentTaskStatus = "failed"
	AgentTaskCanceled  AgentTaskStatus = "canceled"
)

type AgentToolUsage struct {
	ToolID          string `json:"tool_id"`
	InvocationCount uint32 `json:"invocation_count"`
	FailureCount    uint32 `json:"failure_count"`
}

type AgentTokenUsage struct {
	Completeness          AgentTokenUsageCompleteness `json:"completeness"`
	UnavailableReasonCode *string                     `json:"unavailable_reason_code,omitempty"`
	InputTokens           uint64                      `json:"input_tokens"`
	OutputTokens          uint64                      `json:"output_tokens"`
	CacheReadTokens       uint64                      `json:"cache_read_tokens"`
	CacheWriteTokens      uint64                      `json:"cache_write_tokens"`
	ReasoningTokens       *uint64                     `json:"reasoning_tokens,omitempty"`
	TotalTokens           uint64                      `json:"total_tokens"`
}

type AgentTokenUsageCompleteness string

const (
	AgentTokenUsageProviderReported AgentTokenUsageCompleteness = "provider_reported"
	AgentTokenUsagePartial          AgentTokenUsageCompleteness = "partial"
	AgentTokenUsageUnavailable      AgentTokenUsageCompleteness = "unavailable"
)

// AgentExecutionReceipt is intentionally diagnostic worker self-report. It
// carries counters and stable identities only; raw prompts, transcripts, tool
// inputs, and tool outputs are not part of this closed contract.
type AgentExecutionReceipt struct {
	SchemaVersion          string                      `json:"schema_version"`
	ReceiptID              string                      `json:"receipt_id"`
	PlanID                 string                      `json:"plan_id"`
	SourceRunID            string                      `json:"source_run_id"`
	ExecutionID            string                      `json:"execution_id"`
	ReviewRunID            string                      `json:"review_run_id"`
	TaskID                 string                      `json:"task_id"`
	GroupID                string                      `json:"group_id"`
	HypothesisOccurrenceID *string                     `json:"hypothesis_occurrence_id,omitempty"`
	TaskRole               AgentTaskRole               `json:"task_role"`
	Dimension              VersionedRef                `json:"dimension"`
	Runtime                VersionedRef                `json:"runtime"`
	Profile                VersionedRef                `json:"profile"`
	Agent                  VersionedRef                `json:"agent"`
	Provider               VersionedRef                `json:"provider"`
	Model                  VersionedRef                `json:"model"`
	APIProtocol            AgentAPIProtocol            `json:"api_protocol"`
	ProvenanceClass        AgentReceiptProvenanceClass `json:"provenance_class"`
	Authority              AgentReceiptAuthority       `json:"authority"`
	Status                 AgentTaskStatus             `json:"status"`
	FailureReasonCode      *string                     `json:"failure_reason_code,omitempty"`
	PromptDigest           *string                     `json:"prompt_digest,omitempty"`
	OutputDigest           *string                     `json:"output_digest,omitempty"`
	StartedAt              time.Time                   `json:"started_at"`
	FinishedAt             time.Time                   `json:"finished_at"`
	ModelTurnsStarted      uint32                      `json:"model_turns_started"`
	ModelTurnsCompleted    uint32                      `json:"model_turns_completed"`
	ToolCalls              uint32                      `json:"tool_calls"`
	ToolUsage              []AgentToolUsage            `json:"tool_usage"`
	Usage                  AgentTokenUsage             `json:"usage"`
}

// AgentExecutionReceiptCollection is the immutable artifact payload referenced
// by AgentReviewResultManifest. Receipts remain individually strict while this
// envelope closes their run lineage and deterministic ordering.
type AgentExecutionReceiptCollection struct {
	SchemaVersion string                  `json:"schema_version"`
	PlanID        string                  `json:"plan_id"`
	SourceRunID   string                  `json:"source_run_id"`
	ExecutionID   string                  `json:"execution_id"`
	ReviewRunID   string                  `json:"review_run_id"`
	Receipts      []AgentExecutionReceipt `json:"receipts"`
}

type AgentReviewRunStatus string

const (
	AgentReviewRunComplete AgentReviewRunStatus = "complete"
	AgentReviewRunPartial  AgentReviewRunStatus = "partial"
	AgentReviewRunFailed   AgentReviewRunStatus = "failed"
)

type AgentReviewResultSummary struct {
	RawCandidates             uint64                      `json:"raw_candidates"`
	NormalizationRetained     uint64                      `json:"normalization_retained"`
	MergedDuplicates          uint64                      `json:"merged_duplicates"`
	RejectedInvalid           uint64                      `json:"rejected_invalid"`
	ExcludedBudget            uint64                      `json:"excluded_budget"`
	HypothesisOccurrences     uint64                      `json:"hypothesis_occurrences"`
	DedupClusters             uint64                      `json:"dedup_clusters"`
	Confirmed                 uint64                      `json:"confirmed"`
	Rejected                  uint64                      `json:"rejected"`
	Inconclusive              uint64                      `json:"inconclusive"`
	Unverified                uint64                      `json:"unverified"`
	Receipts                  uint64                      `json:"receipts"`
	TasksSucceeded            uint64                      `json:"tasks_succeeded"`
	TasksFailed               uint64                      `json:"tasks_failed"`
	TasksCanceled             uint64                      `json:"tasks_canceled"`
	ModelTurnsStarted         uint64                      `json:"model_turns_started"`
	ModelTurnsCompleted       uint64                      `json:"model_turns_completed"`
	ToolCalls                 uint64                      `json:"tool_calls"`
	UsageCompleteness         AgentTokenUsageCompleteness `json:"usage_completeness"`
	UsageReportedReceipts     uint64                      `json:"usage_reported_receipts"`
	UsagePartialReceipts      uint64                      `json:"usage_partial_receipts"`
	UsageUnavailableReceipts  uint64                      `json:"usage_unavailable_receipts"`
	InputTokens               uint64                      `json:"input_tokens"`
	OutputTokens              uint64                      `json:"output_tokens"`
	CacheReadTokens           uint64                      `json:"cache_read_tokens"`
	CacheWriteTokens          uint64                      `json:"cache_write_tokens"`
	ReasoningTokens           uint64                      `json:"reasoning_tokens"`
	ReasoningReportedReceipts uint64                      `json:"reasoning_reported_receipts"`
	TotalTokens               uint64                      `json:"total_tokens"`
}

type AgentReviewResultManifest struct {
	SchemaVersion             string                   `json:"schema_version"`
	ManifestID                string                   `json:"manifest_id"`
	PlanID                    string                   `json:"plan_id"`
	HypothesisSetID           string                   `json:"hypothesis_set_id"`
	SourceRunID               string                   `json:"source_run_id"`
	ExecutionID               string                   `json:"execution_id"`
	ReviewRunID               string                   `json:"review_run_id"`
	TargetDigest              string                   `json:"target_digest"`
	ExecutionSnapshotRef      ArtifactBinding          `json:"execution_snapshot_ref"`
	ReviewInputRef            ArtifactBinding          `json:"review_input_ref"`
	AgentReviewPlanRef        ArtifactBinding          `json:"agent_review_plan_ref"`
	HypothesisSetRef          ArtifactBinding          `json:"hypothesis_set_ref"`
	RawCandidateCollectionRef ArtifactBinding          `json:"raw_candidate_collection_ref"`
	AgentTaskEvidenceRef      ArtifactBinding          `json:"agent_task_evidence_ref"`
	AgentExecutionReceiptRef  ArtifactBinding          `json:"agent_execution_receipt_ref"`
	ProducerClass             string                   `json:"producer_class"`
	Disposition               string                   `json:"disposition"`
	Status                    AgentReviewRunStatus     `json:"status"`
	Summary                   AgentReviewResultSummary `json:"summary"`
	RecordedAt                time.Time                `json:"recorded_at"`
}

type AgentReviewObservationKind string

const AgentReviewObservationShadowResultRecorded AgentReviewObservationKind = "shadow_result_recorded"

// AgentReviewObservation is a redacted append-only analytics fact. The closed
// shape deliberately excludes source, prompt, transcript, credential, and tool
// payload fields.
type AgentReviewObservation struct {
	SchemaVersion string                     `json:"schema_version"`
	ObservationID string                     `json:"observation_id"`
	ManifestID    string                     `json:"manifest_id"`
	SourceRunID   string                     `json:"source_run_id"`
	ExecutionID   string                     `json:"execution_id"`
	ReviewRunID   string                     `json:"review_run_id"`
	Sequence      uint64                     `json:"sequence"`
	Kind          AgentReviewObservationKind `json:"kind"`
	ProducerClass string                     `json:"producer_class"`
	Disposition   string                     `json:"disposition"`
	Status        AgentReviewRunStatus       `json:"status"`
	Agent         VersionedRef               `json:"agent"`
	Provider      VersionedRef               `json:"provider"`
	Model         VersionedRef               `json:"model"`
	Summary       AgentReviewResultSummary   `json:"summary"`
	DurationMS    uint64                     `json:"duration_ms"`
	ReasonCodes   []string                   `json:"reason_codes"`
	RecordedAt    time.Time                  `json:"recorded_at"`
}

func DecodeAgentReviewPlan(data []byte) (AgentReviewPlan, error) {
	var value AgentReviewPlan
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return AgentReviewPlan{}, fmt.Errorf("decode AgentReviewPlan: %w", err)
	}
	if err := value.Validate(); err != nil {
		return AgentReviewPlan{}, err
	}
	return value, nil
}

func DecodeReviewHypothesisSet(data []byte) (ReviewHypothesisSet, error) {
	var value ReviewHypothesisSet
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return ReviewHypothesisSet{}, fmt.Errorf("decode ReviewHypothesisSet: %w", err)
	}
	if err := value.Validate(); err != nil {
		return ReviewHypothesisSet{}, err
	}
	return value, nil
}

func DecodeAgentExecutionReceipt(data []byte) (AgentExecutionReceipt, error) {
	var value AgentExecutionReceipt
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return AgentExecutionReceipt{}, fmt.Errorf("decode AgentExecutionReceipt: %w", err)
	}
	if err := value.Validate(); err != nil {
		return AgentExecutionReceipt{}, err
	}
	return value, nil
}

func DecodeAgentExecutionReceiptCollection(
	data []byte,
) (AgentExecutionReceiptCollection, error) {
	var value AgentExecutionReceiptCollection
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return AgentExecutionReceiptCollection{}, fmt.Errorf(
			"decode AgentExecutionReceiptCollection: %w",
			err,
		)
	}
	if err := value.Validate(); err != nil {
		return AgentExecutionReceiptCollection{}, err
	}
	return value, nil
}

func DecodeAgentReviewResultManifest(data []byte) (AgentReviewResultManifest, error) {
	var value AgentReviewResultManifest
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return AgentReviewResultManifest{}, fmt.Errorf("decode AgentReviewResultManifest: %w", err)
	}
	if err := value.Validate(); err != nil {
		return AgentReviewResultManifest{}, err
	}
	return value, nil
}

func DecodeAgentReviewObservation(data []byte) (AgentReviewObservation, error) {
	var value AgentReviewObservation
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return AgentReviewObservation{}, fmt.Errorf("decode AgentReviewObservation: %w", err)
	}
	if err := value.Validate(); err != nil {
		return AgentReviewObservation{}, err
	}
	return value, nil
}

func decodeAgentReviewShadowJSON(data []byte, target any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	if err := rejectAgentReviewJSONNulls(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return rejectTrailingJSON(decoder)
}

func rejectAgentReviewJSONNulls(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if token == nil {
			return fmt.Errorf("explicit JSON null is not allowed")
		}
	}
}

func (plan AgentReviewPlan) Validate() error {
	if plan.SchemaVersion != AgentReviewPlanSchemaVersion {
		return fmt.Errorf("unsupported AgentReviewPlan schema %q", plan.SchemaVersion)
	}
	for name, value := range map[string]string{
		"plan_id": plan.PlanID, "source_run_id": plan.SourceRunID,
		"execution_id": plan.ExecutionID, "review_run_id": plan.ReviewRunID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := validateAgentReviewPlanArtifactBinding(
		plan.ExecutionSnapshotRef,
		"execution_snapshot_ref",
	); err != nil {
		return err
	}
	if plan.ExecutionSnapshotRef.Contract != AgentReviewExecutionSnapshotContract {
		return fmt.Errorf(
			"execution_snapshot_ref.contract must be %q",
			AgentReviewExecutionSnapshotContract,
		)
	}
	if err := validateAgentReviewPlanArtifactBinding(
		plan.ReviewInputRef,
		"review_input_ref",
	); err != nil {
		return err
	}
	if plan.ReviewInputRef.Contract != AgentReviewInputContract {
		return fmt.Errorf("review_input_ref.contract must be %q", AgentReviewInputContract)
	}
	if err := requireSHA256("target_digest", plan.TargetDigest); err != nil {
		return err
	}
	for name, ref := range map[string]VersionedRef{
		"rule_pack":      plan.RulePack,
		"implementation": plan.Implementation, "grouping": plan.Grouping,
		"verifier": plan.Verifier, "runtime": plan.Runtime,
		"profile": plan.Profile, "agent": plan.Agent,
		"provider": plan.Provider, "model": plan.Model,
	} {
		if err := ref.validate(name); err != nil {
			return err
		}
	}
	if err := ValidateCandidateNormalizationImplementation(
		plan.Normalization,
		"normalization",
	); err != nil {
		return err
	}
	if plan.Normalization.SHA256 != plan.Implementation.SHA256 ||
		plan.Normalization.SHA256 != plan.Agent.SHA256 {
		return fmt.Errorf("normalization must bind the exact worker implementation digest")
	}
	if err := validateVersionedRefs("context_dimensions", plan.ContextDimensions, true); err != nil {
		return err
	}
	if len(plan.ContextDimensions) != 1 {
		return fmt.Errorf("context_dimensions must contain exactly one Pi worker dimension")
	}
	if err := validateVersionedRefs("review_dimensions", plan.ReviewDimensions, true); err != nil {
		return err
	}
	if len(plan.ReviewDimensions) > agentReviewMaxReviewDimensions {
		return fmt.Errorf(
			"review_dimensions exceeds the Pi worker maximum of %d",
			agentReviewMaxReviewDimensions,
		)
	}
	if err := validateVersionedRefs("knowledge", plan.Knowledge, false); err != nil {
		return err
	}
	if len(plan.Knowledge) > AgentReviewWorkerMaxKnowledgeCount {
		return fmt.Errorf("knowledge exceeds the Pi worker maximum of %d", AgentReviewWorkerMaxKnowledgeCount)
	}
	if plan.APIProtocol != AgentAPIProtocolAnthropicMessages {
		return fmt.Errorf("unsupported api_protocol %q", plan.APIProtocol)
	}
	if err := plan.ToolPolicy.validate(); err != nil {
		return err
	}
	if err := plan.Budget.validate(); err != nil {
		return err
	}
	if !plan.VerificationRequired {
		return fmt.Errorf("verification_required must be true for the shadow contract")
	}
	if plan.ExecutionClass != AgentReviewExecutionLocalDirectProviderShadow {
		return fmt.Errorf("unsupported execution_class %q", plan.ExecutionClass)
	}
	if plan.Attestation != AgentReviewAttestationNonAttested {
		return fmt.Errorf("attestation must be %q", AgentReviewAttestationNonAttested)
	}
	if plan.SideEffects != "deny" {
		return fmt.Errorf("side_effects must be deny")
	}
	return validateAgentReviewTime("created_at", plan.CreatedAt)
}

func (policy AgentReviewToolPolicy) validate() error {
	if policy.AllowedTools == nil {
		return fmt.Errorf("tool_policy.allowed_tools must be an explicit array")
	}
	for index, tool := range policy.AllowedTools {
		if err := requireIdentifier(fmt.Sprintf("tool_policy.allowed_tools[%d]", index), tool); err != nil {
			return err
		}
		if index > 0 && tool == policy.AllowedTools[index-1] {
			return fmt.Errorf("tool_policy.allowed_tools contains duplicate %q", tool)
		}
		if isAgentTerminalTool(tool) {
			return fmt.Errorf(
				"tool_policy.allowed_tools must not include terminal submit tool %q",
				tool,
			)
		}
	}
	if !slices.Equal(policy.AllowedTools, []string{
		agentReviewWorkerToolListFiles,
		agentReviewWorkerToolReadFile,
		agentReviewWorkerToolSearchCode,
	}) {
		return fmt.Errorf(
			"frozen-input Pi worker requires tool_policy.allowed_tools exactly %q, %q, %q",
			agentReviewWorkerToolListFiles,
			agentReviewWorkerToolReadFile,
			agentReviewWorkerToolSearchCode,
		)
	}
	if policy.ToolNetwork != "deny" || policy.WorkspaceWrites != "deny" ||
		policy.RemoteWrites != "deny" {
		return fmt.Errorf("tool network, workspace writes, and remote writes must be denied")
	}
	return nil
}

func (budget AgentReviewBudget) validate() error {
	if budget.MaxFiles == 0 || budget.MaxGroups == 0 || budget.MaxCandidates == 0 ||
		budget.MaxModelCalls == 0 || budget.MaxToolCalls == 0 || budget.MaxOutputTokens == 0 ||
		budget.MaxGroupBytes == 0 || budget.MaxTargetBytes == 0 || budget.TimeoutMS == 0 ||
		budget.MaxConcurrency == 0 {
		return fmt.Errorf("all agent review budgets must be positive")
	}
	for _, limit := range []struct {
		name  string
		value uint64
		max   uint64
	}{
		{"max_files", uint64(budget.MaxFiles), uint64(agentReviewMaxFiles)},
		{"max_groups", uint64(budget.MaxGroups), uint64(agentReviewMaxGroups)},
		{"max_candidates", uint64(budget.MaxCandidates), uint64(agentReviewMaxCandidates)},
		{"max_model_calls", uint64(budget.MaxModelCalls), uint64(agentReviewMaxModelCalls)},
		{"max_tool_calls", uint64(budget.MaxToolCalls), uint64(agentReviewMaxToolCalls)},
		{"max_output_tokens", uint64(budget.MaxOutputTokens), uint64(agentReviewMaxOutputTokens)},
		{"max_group_bytes", budget.MaxGroupBytes, agentReviewMaxGroupBytes},
		{"max_target_bytes", budget.MaxTargetBytes, agentReviewMaxTargetBytes},
		{"timeout_ms", budget.TimeoutMS, agentReviewMaxTimeoutMS},
		{"max_concurrency", uint64(budget.MaxConcurrency), uint64(agentReviewMaxConcurrency)},
	} {
		if limit.value > limit.max {
			return fmt.Errorf("budget.%s exceeds the Pi worker maximum %d", limit.name, limit.max)
		}
	}
	if budget.MaxGroupBytes > budget.MaxTargetBytes {
		return fmt.Errorf("budget.max_group_bytes must not exceed max_target_bytes")
	}
	if budget.MaxConcurrency > budget.MaxModelCalls {
		return fmt.Errorf("budget.max_concurrency must not exceed max_model_calls")
	}
	return nil
}

func validateAgentReviewPlanArtifactBinding(
	binding ArtifactBinding,
	name string,
) error {
	if err := binding.validate(name, true); err != nil {
		return err
	}
	if binding.Ref.SizeBytes > agentReviewMaxArtifactBytes {
		return fmt.Errorf(
			"%s.ref.size_bytes exceeds the Pi worker maximum %d",
			name,
			agentReviewMaxArtifactBytes,
		)
	}
	return nil
}

func (set ReviewHypothesisSet) Validate() error {
	if set.SchemaVersion != ReviewHypothesisSetSchemaVersion {
		return fmt.Errorf("unsupported ReviewHypothesisSet schema %q", set.SchemaVersion)
	}
	for name, value := range map[string]string{
		"hypothesis_set_id": set.HypothesisSetID, "plan_id": set.PlanID,
		"source_run_id": set.SourceRunID, "execution_id": set.ExecutionID,
		"review_run_id": set.ReviewRunID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", set.TargetDigest); err != nil {
		return err
	}
	if set.Hypotheses == nil || set.DedupClusters == nil ||
		set.NormalizationDecisions == nil || set.CompletenessReasons == nil {
		return fmt.Errorf(
			"hypotheses, dedup_clusters, normalization_decisions, and completeness_reasons must be explicit arrays",
		)
	}
	if err := validateSortedReasonCodes("completeness_reasons", set.CompletenessReasons); err != nil {
		return err
	}
	occurrences := make(map[string]ReviewHypothesis, len(set.Hypotheses))
	previousOccurrence := ""
	for index, hypothesis := range set.Hypotheses {
		if err := hypothesis.validate(index); err != nil {
			return err
		}
		if index > 0 && hypothesis.OccurrenceID <= previousOccurrence {
			return fmt.Errorf("hypotheses must be uniquely sorted by occurrence_id")
		}
		previousOccurrence = hypothesis.OccurrenceID
		occurrences[hypothesis.OccurrenceID] = hypothesis
	}
	clustered := make(map[string]struct{}, len(occurrences))
	canonicalByOccurrence := make(map[string]string, len(occurrences))
	clusterFingerprints := make(map[string]struct{}, len(occurrences))
	previousCluster := ""
	for index, cluster := range set.DedupClusters {
		if err := cluster.validate(index, occurrences, clustered); err != nil {
			return err
		}
		if _, exists := clusterFingerprints[cluster.Fingerprint]; exists {
			return fmt.Errorf(
				"dedup_clusters contain duplicate canonical fingerprint %q",
				cluster.Fingerprint,
			)
		}
		clusterFingerprints[cluster.Fingerprint] = struct{}{}
		if index > 0 && cluster.ClusterID <= previousCluster {
			return fmt.Errorf("dedup_clusters must be uniquely sorted by cluster_id")
		}
		previousCluster = cluster.ClusterID
		for _, occurrenceID := range cluster.OccurrenceIDs {
			canonicalByOccurrence[occurrenceID] = cluster.CanonicalOccurrenceID
		}
	}
	if len(clustered) != len(occurrences) {
		return fmt.Errorf("every hypothesis occurrence must belong to exactly one dedup cluster")
	}
	decidedOccurrences := make(map[string]struct{}, len(occurrences))
	previousRawCandidate := ""
	for index, decision := range set.NormalizationDecisions {
		if err := decision.validate(
			index,
			occurrences,
			canonicalByOccurrence,
			decidedOccurrences,
		); err != nil {
			return err
		}
		if index > 0 && decision.RawCandidateID <= previousRawCandidate {
			return fmt.Errorf(
				"normalization_decisions must be uniquely sorted by raw_candidate_id",
			)
		}
		previousRawCandidate = decision.RawCandidateID
	}
	if len(decidedOccurrences) != len(occurrences) {
		return fmt.Errorf(
			"every canonical hypothesis occurrence must have exactly one retained normalization decision",
		)
	}
	if err := set.Coverage.validate(); err != nil {
		return err
	}
	switch set.Completeness {
	case AgentReviewComplete:
		if len(set.CompletenessReasons) != 0 || len(set.Coverage.Gaps) != 0 ||
			set.Coverage.GroupsReviewed != set.Coverage.GroupsTotal ||
			set.Coverage.ReviewTasksSucceeded != set.Coverage.ReviewTasksTotal {
			return fmt.Errorf("complete hypothesis set requires full coverage without gaps or reasons")
		}
	case AgentReviewPartial:
		if len(set.CompletenessReasons) == 0 && len(set.Coverage.Gaps) == 0 {
			return fmt.Errorf("partial hypothesis set must retain a reason or coverage gap")
		}
	default:
		return fmt.Errorf("unsupported hypothesis set completeness %q", set.Completeness)
	}
	return validateAgentReviewTime("generated_at", set.GeneratedAt)
}

func (decision HypothesisNormalizationDecision) validate(
	index int,
	occurrences map[string]ReviewHypothesis,
	canonicalByOccurrence map[string]string,
	decidedOccurrences map[string]struct{},
) error {
	name := fmt.Sprintf("normalization_decisions[%d]", index)
	if err := requireIdentifier(name+".raw_candidate_id", decision.RawCandidateID); err != nil {
		return err
	}
	if err := requireSHA256(name+".claim_digest", decision.ClaimDigest); err != nil {
		return err
	}
	if err := requireIdentifier(name+".reason_code", decision.ReasonCode); err != nil {
		return err
	}
	switch decision.Action {
	case HypothesisNormalizationRetained:
		if decision.OccurrenceID == nil || decision.CanonicalOccurrenceID != nil {
			return fmt.Errorf("%s retained action requires occurrence_id only", name)
		}
		occurrenceID := *decision.OccurrenceID
		if err := requireIdentifier(name+".occurrence_id", occurrenceID); err != nil {
			return err
		}
		if _, exists := occurrences[occurrenceID]; !exists {
			return fmt.Errorf("%s references unknown occurrence %q", name, occurrenceID)
		}
		if _, exists := decidedOccurrences[occurrenceID]; exists {
			return fmt.Errorf("hypothesis occurrence %q has multiple normalization decisions", occurrenceID)
		}
		if canonicalByOccurrence[occurrenceID] != occurrenceID {
			return fmt.Errorf("%s retained occurrence must be the cluster canonical", name)
		}
		decidedOccurrences[occurrenceID] = struct{}{}
		return nil
	case HypothesisNormalizationMergedDuplicate:
		if decision.OccurrenceID != nil || decision.CanonicalOccurrenceID == nil {
			return fmt.Errorf(
				"%s merged_duplicate requires canonical_occurrence_id only",
				name,
			)
		}
		canonicalID := *decision.CanonicalOccurrenceID
		if err := requireIdentifier(name+".canonical_occurrence_id", canonicalID); err != nil {
			return err
		}
		if _, exists := occurrences[canonicalID]; !exists ||
			canonicalByOccurrence[canonicalID] != canonicalID {
			return fmt.Errorf("%s does not point to a canonical retained occurrence", name)
		}
		return nil
	case HypothesisNormalizationRejectedInvalid, HypothesisNormalizationExcludedBudget:
		if decision.OccurrenceID != nil || decision.CanonicalOccurrenceID != nil {
			return fmt.Errorf("%s %s forbids occurrence lineage", name, decision.Action)
		}
		return nil
	default:
		return fmt.Errorf("%s has unsupported action %q", name, decision.Action)
	}
}

func (hypothesis ReviewHypothesis) validate(index int) error {
	name := fmt.Sprintf("hypotheses[%d]", index)
	for field, value := range map[string]string{
		name + ".occurrence_id": hypothesis.OccurrenceID,
		name + ".group_id":      hypothesis.GroupID,
		name + ".category":      hypothesis.Category,
	} {
		if err := requireIdentifier(field, value); err != nil {
			return err
		}
	}
	if err := requireSHA256(name+".cluster_fingerprint", hypothesis.ClusterFingerprint); err != nil {
		return err
	}
	if err := hypothesis.Dimension.validate(name + ".dimension"); err != nil {
		return err
	}
	switch hypothesis.Severity {
	case HypothesisSeverityCritical, HypothesisSeverityHigh,
		HypothesisSeverityMedium, HypothesisSeverityLow:
	default:
		return fmt.Errorf("%s has unsupported severity %q", name, hypothesis.Severity)
	}
	if hypothesis.RawConfidencePPM > ConfidenceScalePPM {
		return fmt.Errorf("%s.raw_confidence_ppm exceeds ppm scale", name)
	}
	if !hypothesis.RawConfidenceAvailable && hypothesis.RawConfidencePPM != 0 {
		return fmt.Errorf("%s unavailable raw confidence must be zero", name)
	}
	for field, value := range map[string]string{
		name + ".title": hypothesis.Title, name + ".description": hypothesis.Description,
		name + ".impact": hypothesis.Impact,
	} {
		if err := requireBoundedAgentReviewText(field, value, 8192, true); err != nil {
			return err
		}
	}
	if hypothesis.Suggestion != nil {
		if err := requireBoundedAgentReviewText(name+".suggestion", *hypothesis.Suggestion, 8192, true); err != nil {
			return err
		}
	}
	if err := hypothesis.Anchor.validate(name + ".anchor"); err != nil {
		return err
	}
	if len(hypothesis.Evidence) == 0 {
		return fmt.Errorf("%s.evidence must not be empty", name)
	}
	evidenceIDs := make(map[string]struct{}, len(hypothesis.Evidence))
	previousEvidence := ""
	for evidenceIndex, evidence := range hypothesis.Evidence {
		if err := evidence.validate(fmt.Sprintf("%s.evidence[%d]", name, evidenceIndex)); err != nil {
			return err
		}
		if evidenceIndex > 0 && evidence.EvidenceID <= previousEvidence {
			return fmt.Errorf("%s.evidence must be uniquely sorted by evidence_id", name)
		}
		previousEvidence = evidence.EvidenceID
		evidenceIDs[evidence.EvidenceID] = struct{}{}
	}
	if hypothesis.Verification == nil {
		return fmt.Errorf("%s.verification_observations must be an explicit array", name)
	}
	for observationIndex, observation := range hypothesis.Verification {
		if err := observation.validate(
			fmt.Sprintf("%s.verification_observations[%d]", name, observationIndex),
			uint32(observationIndex+1),
			evidenceIDs,
		); err != nil {
			return err
		}
	}
	return nil
}

func (anchor HypothesisSourceAnchor) validate(name string) error {
	if err := requireRepositoryPath(name+".path", anchor.Path); err != nil {
		return err
	}
	switch anchor.Side {
	case HypothesisAnchorOld, HypothesisAnchorNew, HypothesisAnchorFile:
	default:
		return fmt.Errorf("%s has unsupported side %q", name, anchor.Side)
	}
	if anchor.StartLine == 0 || anchor.EndLine < anchor.StartLine {
		return fmt.Errorf("%s has an invalid line range", name)
	}
	return requireSHA256(name+".source_digest", anchor.SourceDigest)
}

func (evidence HypothesisEvidence) validate(name string) error {
	if err := requireIdentifier(name+".evidence_id", evidence.EvidenceID); err != nil {
		return err
	}
	if err := requireBoundedAgentReviewText(name+".statement", evidence.Statement, 8192, true); err != nil {
		return err
	}
	if err := evidence.Anchor.validate(name + ".anchor"); err != nil {
		return err
	}
	if err := requireBoundedAgentReviewText(name+".excerpt", evidence.Excerpt, 16384, false); err != nil {
		return err
	}
	if countAgentReviewNonWhitespaceRunes(evidence.Excerpt) < 8 {
		return fmt.Errorf("%s.excerpt must contain at least 8 non-whitespace Unicode characters", name)
	}
	if err := requireSHA256(name+".evidence_digest", evidence.EvidenceDigest); err != nil {
		return err
	}
	digest, err := DigestHypothesisEvidence(evidence)
	if err != nil {
		return err
	}
	if digest != evidence.EvidenceDigest {
		return fmt.Errorf("%s.evidence_digest does not match exact evidence content", name)
	}
	return nil
}

func countAgentReviewNonWhitespaceRunes(value string) int {
	count := 0
	for _, character := range value {
		if !unicode.IsSpace(character) {
			count++
		}
	}
	return count
}

func DigestHypothesisEvidence(evidence HypothesisEvidence) (string, error) {
	copy := evidence
	copy.EvidenceDigest = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal hypothesis evidence: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (observation HypothesisVerificationObservation) validate(
	name string,
	expectedSequence uint32,
	evidenceIDs map[string]struct{},
) error {
	if err := requireIdentifier(name+".observation_id", observation.ObservationID); err != nil {
		return err
	}
	if observation.Sequence != expectedSequence {
		return fmt.Errorf("%s.sequence must be contiguous and start at one", name)
	}
	if err := observation.Verifier.validate(name + ".verifier"); err != nil {
		return err
	}
	switch observation.Verdict {
	case HypothesisVerificationConfirmed, HypothesisVerificationRejected,
		HypothesisVerificationInconclusive:
	default:
		return fmt.Errorf("%s has unsupported verdict %q", name, observation.Verdict)
	}
	if err := requireIdentifier(name+".reason_code", observation.ReasonCode); err != nil {
		return err
	}
	if err := requireBoundedAgentReviewText(name+".explanation", observation.Explanation, 8192, true); err != nil {
		return err
	}
	if observation.EvidenceIDs == nil || !slices.IsSorted(observation.EvidenceIDs) {
		return fmt.Errorf("%s.evidence_ids must be an explicit sorted array", name)
	}
	for index, evidenceID := range observation.EvidenceIDs {
		if _, ok := evidenceIDs[evidenceID]; !ok {
			return fmt.Errorf("%s.evidence_ids[%d] does not reference occurrence evidence", name, index)
		}
		if index > 0 && evidenceID == observation.EvidenceIDs[index-1] {
			return fmt.Errorf("%s.evidence_ids contains duplicate %q", name, evidenceID)
		}
	}
	if observation.Verdict == HypothesisVerificationConfirmed && len(observation.EvidenceIDs) == 0 {
		return fmt.Errorf("%s confirmed verdict requires exact evidence", name)
	}
	return nil
}

func (cluster HypothesisDedupCluster) validate(
	index int,
	occurrences map[string]ReviewHypothesis,
	clustered map[string]struct{},
) error {
	name := fmt.Sprintf("dedup_clusters[%d]", index)
	if err := requireIdentifier(name+".cluster_id", cluster.ClusterID); err != nil {
		return err
	}
	if err := requireSHA256(name+".fingerprint", cluster.Fingerprint); err != nil {
		return err
	}
	if err := requireIdentifier(name+".canonical_occurrence_id", cluster.CanonicalOccurrenceID); err != nil {
		return err
	}
	if len(cluster.OccurrenceIDs) != 1 ||
		cluster.OccurrenceIDs[0] != cluster.CanonicalOccurrenceID {
		return fmt.Errorf(
			"%s.occurrence_ids must contain only the canonical retained occurrence",
			name,
		)
	}
	canonicalSeen := false
	for occurrenceIndex, occurrenceID := range cluster.OccurrenceIDs {
		if occurrenceIndex > 0 && occurrenceID == cluster.OccurrenceIDs[occurrenceIndex-1] {
			return fmt.Errorf("%s.occurrence_ids contains duplicate %q", name, occurrenceID)
		}
		occurrence, ok := occurrences[occurrenceID]
		if !ok {
			return fmt.Errorf("%s references unknown occurrence %q", name, occurrenceID)
		}
		if occurrence.ClusterFingerprint != cluster.Fingerprint {
			return fmt.Errorf("%s fingerprint does not bind occurrence %q", name, occurrenceID)
		}
		if _, exists := clustered[occurrenceID]; exists {
			return fmt.Errorf("occurrence %q belongs to more than one dedup cluster", occurrenceID)
		}
		clustered[occurrenceID] = struct{}{}
		canonicalSeen = canonicalSeen || occurrenceID == cluster.CanonicalOccurrenceID
	}
	if !canonicalSeen {
		return fmt.Errorf("%s canonical occurrence must belong to the cluster", name)
	}
	return nil
}

func (coverage AgentReviewCoverage) validate() error {
	if coverage.Gaps == nil {
		return fmt.Errorf("coverage.gaps must be an explicit array")
	}
	if coverage.GroupsReviewed > coverage.GroupsTotal ||
		coverage.ReviewTasksSucceeded > coverage.ReviewTasksTotal {
		return fmt.Errorf("coverage completed counts must not exceed totals")
	}
	previous := ""
	for index, gap := range coverage.Gaps {
		name := fmt.Sprintf("coverage.gaps[%d]", index)
		for field, value := range map[string]string{
			name + ".gap_id": gap.GapID, name + ".subject_id": gap.SubjectID,
			name + ".reason_code": gap.ReasonCode,
		} {
			if err := requireIdentifier(field, value); err != nil {
				return err
			}
		}
		switch gap.Phase {
		case AgentReviewCoverageCapture, AgentReviewCoverageContext,
			AgentReviewCoverageReview, AgentReviewCoverageVerification:
		default:
			return fmt.Errorf("%s has unsupported phase %q", name, gap.Phase)
		}
		if index > 0 && gap.GapID <= previous {
			return fmt.Errorf("coverage.gaps must be uniquely sorted by gap_id")
		}
		previous = gap.GapID
	}
	return nil
}

func (receipt AgentExecutionReceipt) Validate() error {
	if receipt.SchemaVersion != AgentExecutionReceiptSchemaVersion {
		return fmt.Errorf("unsupported AgentExecutionReceipt schema %q", receipt.SchemaVersion)
	}
	for name, value := range map[string]string{
		"receipt_id": receipt.ReceiptID, "plan_id": receipt.PlanID,
		"source_run_id": receipt.SourceRunID, "execution_id": receipt.ExecutionID,
		"review_run_id": receipt.ReviewRunID, "task_id": receipt.TaskID,
		"group_id": receipt.GroupID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	for name, ref := range map[string]VersionedRef{
		"dimension": receipt.Dimension, "runtime": receipt.Runtime,
		"profile": receipt.Profile, "agent": receipt.Agent,
		"provider": receipt.Provider, "model": receipt.Model,
	} {
		if err := ref.validate(name); err != nil {
			return err
		}
	}
	if receipt.APIProtocol != AgentAPIProtocolAnthropicMessages {
		return fmt.Errorf("unsupported api_protocol %q", receipt.APIProtocol)
	}
	if receipt.ProvenanceClass != AgentReceiptProvenanceWorkerSelfReport {
		return fmt.Errorf("provenance_class must be worker_self_report")
	}
	if receipt.Authority != AgentReceiptAuthorityDiagnosticOnly {
		return fmt.Errorf("authority must be diagnostic_only")
	}
	switch receipt.TaskRole {
	case AgentTaskContext, AgentTaskReview:
		if receipt.HypothesisOccurrenceID != nil {
			return fmt.Errorf("only verification receipts may bind a hypothesis occurrence")
		}
	case AgentTaskVerification:
		if receipt.HypothesisOccurrenceID == nil {
			return fmt.Errorf("verification receipt requires hypothesis_occurrence_id")
		}
		if err := requireIdentifier("hypothesis_occurrence_id", *receipt.HypothesisOccurrenceID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported task_role %q", receipt.TaskRole)
	}
	switch receipt.Status {
	case AgentTaskSucceeded:
		if receipt.FailureReasonCode != nil {
			return fmt.Errorf("succeeded receipt forbids failure_reason_code")
		}
		if receipt.PromptDigest == nil || receipt.OutputDigest == nil {
			return fmt.Errorf("succeeded receipt requires prompt_digest and output_digest")
		}
	case AgentTaskFailed, AgentTaskCanceled:
		if receipt.FailureReasonCode == nil {
			return fmt.Errorf("failed/canceled receipt requires failure_reason_code")
		}
		if err := requireIdentifier("failure_reason_code", *receipt.FailureReasonCode); err != nil {
			return err
		}
		if receipt.OutputDigest != nil {
			return fmt.Errorf("failed/canceled receipt forbids output_digest")
		}
	default:
		return fmt.Errorf("unsupported task status %q", receipt.Status)
	}
	if err := validateAgentReviewTime("started_at", receipt.StartedAt); err != nil {
		return err
	}
	if err := validateAgentReviewTime("finished_at", receipt.FinishedAt); err != nil {
		return err
	}
	if receipt.FinishedAt.Before(receipt.StartedAt) {
		return fmt.Errorf("finished_at must not precede started_at")
	}
	if receipt.ModelTurnsCompleted > receipt.ModelTurnsStarted {
		return fmt.Errorf("model_turns_completed must not exceed model_turns_started")
	}
	if receipt.ModelTurnsStarted > 0 && receipt.PromptDigest == nil {
		return fmt.Errorf("started model turn requires prompt_digest")
	}
	if receipt.PromptDigest != nil {
		if err := requireSHA256("prompt_digest", *receipt.PromptDigest); err != nil {
			return err
		}
	}
	if receipt.OutputDigest != nil {
		if err := requireSHA256("output_digest", *receipt.OutputDigest); err != nil {
			return err
		}
	}
	if receipt.ModelTurnsCompleted < receipt.ModelTurnsStarted &&
		receipt.Usage.Completeness == AgentTokenUsageProviderReported {
		return fmt.Errorf(
			"incomplete model turn lifecycle requires partial or unavailable usage",
		)
	}
	if receipt.ToolUsage == nil {
		return fmt.Errorf("tool_usage must be an explicit array")
	}
	var toolCalls uint64
	terminalSuccesses := uint32(0)
	expectedTerminal := agentTerminalTool(receipt.TaskRole)
	previousTool := ""
	for index, usage := range receipt.ToolUsage {
		name := fmt.Sprintf("tool_usage[%d]", index)
		if err := requireIdentifier(name+".tool_id", usage.ToolID); err != nil {
			return err
		}
		if usage.FailureCount > usage.InvocationCount {
			return fmt.Errorf("%s.failure_count must not exceed invocation_count", name)
		}
		if isAgentTerminalTool(usage.ToolID) {
			if usage.ToolID != expectedTerminal {
				return fmt.Errorf(
					"%s terminal tool does not match task_role %q",
					name,
					receipt.TaskRole,
				)
			}
			successes := usage.InvocationCount - usage.FailureCount
			if successes > 1 {
				return fmt.Errorf("%s successful terminal invocation_count must not exceed one", name)
			}
			terminalSuccesses += successes
		}
		if index > 0 && usage.ToolID <= previousTool {
			return fmt.Errorf("tool_usage must be uniquely sorted by tool_id")
		}
		previousTool = usage.ToolID
		toolCalls += uint64(usage.InvocationCount)
	}
	if toolCalls != uint64(receipt.ToolCalls) {
		return fmt.Errorf("tool_calls must equal the sum of tool_usage invocation_count")
	}
	if receipt.Status == AgentTaskSucceeded && terminalSuccesses != 1 {
		return fmt.Errorf("succeeded receipt requires exactly one successful matching terminal submit call")
	}
	return receipt.Usage.validate()
}

func agentTerminalTool(role AgentTaskRole) string {
	switch role {
	case AgentTaskContext:
		return "submit_context"
	case AgentTaskReview:
		return "submit_candidates"
	case AgentTaskVerification:
		return "submit_verdict"
	default:
		return ""
	}
}

func isAgentTerminalTool(toolID string) bool {
	return toolID == "submit_context" || toolID == "submit_candidates" ||
		toolID == "submit_verdict"
}

func (usage AgentTokenUsage) validate() error {
	switch usage.Completeness {
	case AgentTokenUsageProviderReported:
		if usage.UnavailableReasonCode != nil {
			return fmt.Errorf("provider_reported usage forbids unavailable_reason_code")
		}
		return usage.validateObservedCounters("provider_reported", false)
	case AgentTokenUsagePartial:
		if usage.UnavailableReasonCode == nil {
			return fmt.Errorf("partial usage requires unavailable_reason_code")
		}
		if err := requireIdentifier(
			"usage.unavailable_reason_code",
			*usage.UnavailableReasonCode,
		); err != nil {
			return err
		}
		return usage.validateObservedCounters("partial", true)
	case AgentTokenUsageUnavailable:
		if usage.UnavailableReasonCode == nil {
			return fmt.Errorf("unavailable usage requires unavailable_reason_code")
		}
		if err := requireIdentifier(
			"usage.unavailable_reason_code",
			*usage.UnavailableReasonCode,
		); err != nil {
			return err
		}
		if usage.InputTokens != 0 || usage.OutputTokens != 0 ||
			usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 ||
			usage.ReasoningTokens != nil || usage.TotalTokens != 0 {
			return fmt.Errorf("unavailable usage must keep token counters zero and unobserved")
		}
	default:
		return fmt.Errorf("unsupported receipt usage completeness %q", usage.Completeness)
	}
	return nil
}

func (usage AgentTokenUsage) validateObservedCounters(label string, requireObserved bool) error {
	total, ok := checkedAgentReviewSum(
		usage.InputTokens,
		usage.OutputTokens,
		usage.CacheReadTokens,
		usage.CacheWriteTokens,
	)
	if !ok || usage.TotalTokens != total {
		return fmt.Errorf("%s total_tokens must equal observed components", label)
	}
	if requireObserved && usage.TotalTokens == 0 {
		return fmt.Errorf("partial usage requires positive observed token counters")
	}
	if usage.ReasoningTokens != nil && *usage.ReasoningTokens > usage.OutputTokens {
		return fmt.Errorf("reasoning_tokens must not exceed output_tokens")
	}
	return nil
}

func (collection AgentExecutionReceiptCollection) Validate() error {
	if collection.SchemaVersion != AgentExecutionReceiptCollectionSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentExecutionReceiptCollection schema %q",
			collection.SchemaVersion,
		)
	}
	for name, value := range map[string]string{
		"plan_id": collection.PlanID, "source_run_id": collection.SourceRunID,
		"execution_id": collection.ExecutionID, "review_run_id": collection.ReviewRunID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if collection.Receipts == nil {
		return fmt.Errorf("receipts must be an explicit array")
	}
	taskIDs := make(map[string]struct{}, len(collection.Receipts))
	receiptIDs := make(map[string]struct{}, len(collection.Receipts))
	previousKey := ""
	for index, receipt := range collection.Receipts {
		if err := receipt.Validate(); err != nil {
			return fmt.Errorf("validate receipts[%d]: %w", index, err)
		}
		if receipt.PlanID != collection.PlanID ||
			receipt.SourceRunID != collection.SourceRunID ||
			receipt.ExecutionID != collection.ExecutionID ||
			receipt.ReviewRunID != collection.ReviewRunID {
			return fmt.Errorf("receipts[%d] does not match collection lineage", index)
		}
		if _, exists := taskIDs[receipt.TaskID]; exists {
			return fmt.Errorf("receipts contain duplicate task_id %q", receipt.TaskID)
		}
		if _, exists := receiptIDs[receipt.ReceiptID]; exists {
			return fmt.Errorf("receipts contain duplicate receipt_id %q", receipt.ReceiptID)
		}
		taskIDs[receipt.TaskID] = struct{}{}
		receiptIDs[receipt.ReceiptID] = struct{}{}
		key := receipt.TaskID + "\x00" + receipt.ReceiptID
		if index > 0 && key <= previousKey {
			return fmt.Errorf("receipts must be uniquely sorted by task_id and receipt_id")
		}
		previousKey = key
	}
	return nil
}

func (manifest AgentReviewResultManifest) Validate() error {
	if manifest.SchemaVersion != AgentReviewResultManifestSchemaVersion {
		return fmt.Errorf("unsupported AgentReviewResultManifest schema %q", manifest.SchemaVersion)
	}
	for name, value := range map[string]string{
		"manifest_id": manifest.ManifestID, "plan_id": manifest.PlanID,
		"hypothesis_set_id": manifest.HypothesisSetID,
		"source_run_id":     manifest.SourceRunID, "execution_id": manifest.ExecutionID,
		"review_run_id": manifest.ReviewRunID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", manifest.TargetDigest); err != nil {
		return err
	}
	bindings := []struct {
		name     string
		binding  ArtifactBinding
		contract string
	}{
		{"execution_snapshot_ref", manifest.ExecutionSnapshotRef, AgentReviewExecutionSnapshotContract},
		{"review_input_ref", manifest.ReviewInputRef, AgentReviewInputContract},
		{"agent_review_plan_ref", manifest.AgentReviewPlanRef, AgentReviewPlanSchemaVersion},
		{"hypothesis_set_ref", manifest.HypothesisSetRef, ReviewHypothesisSetSchemaVersion},
		{"raw_candidate_collection_ref", manifest.RawCandidateCollectionRef, AgentReviewRawCandidateCollectionSchemaVersion},
		{"agent_task_evidence_ref", manifest.AgentTaskEvidenceRef, AgentReviewTaskEvidenceCollectionSchemaVersion},
		{"agent_execution_receipt_ref", manifest.AgentExecutionReceiptRef, AgentExecutionReceiptCollectionContract},
	}
	for _, item := range bindings {
		if err := item.binding.validate(item.name, true); err != nil {
			return err
		}
		if item.contract != "" && item.binding.Contract != item.contract {
			return fmt.Errorf("%s.contract must be %q", item.name, item.contract)
		}
	}
	if manifest.ProducerClass != "argus_go_host" {
		return fmt.Errorf("producer_class must be argus_go_host")
	}
	if manifest.Disposition != "shadow_only" {
		return fmt.Errorf("disposition must be shadow_only")
	}
	if err := manifest.Summary.validate(); err != nil {
		return err
	}
	switch manifest.Status {
	case AgentReviewRunComplete, AgentReviewRunPartial, AgentReviewRunFailed:
	default:
		return fmt.Errorf("unsupported manifest status %q", manifest.Status)
	}
	return validateAgentReviewTime("recorded_at", manifest.RecordedAt)
}

func (summary AgentReviewResultSummary) validate() error {
	normalizationTotal, ok := checkedAgentReviewSum(
		summary.NormalizationRetained,
		summary.MergedDuplicates,
		summary.RejectedInvalid,
		summary.ExcludedBudget,
	)
	if !ok || normalizationTotal != summary.RawCandidates {
		return fmt.Errorf("summary normalization counts must equal raw_candidates")
	}
	if summary.NormalizationRetained != summary.HypothesisOccurrences {
		return fmt.Errorf("summary retained count must equal canonical hypothesis_occurrences")
	}
	verdictTotal, ok := checkedAgentReviewSum(
		summary.Confirmed,
		summary.Rejected,
		summary.Inconclusive,
		summary.Unverified,
	)
	if !ok || verdictTotal != summary.HypothesisOccurrences {
		return fmt.Errorf("summary hypothesis verdict counts must equal hypothesis_occurrences")
	}
	taskTotal, ok := checkedAgentReviewSum(
		summary.TasksSucceeded,
		summary.TasksFailed,
		summary.TasksCanceled,
	)
	if !ok || taskTotal != summary.Receipts {
		return fmt.Errorf("summary task status counts must equal receipts")
	}
	if summary.DedupClusters > summary.HypothesisOccurrences {
		return fmt.Errorf("summary dedup_clusters must not exceed hypothesis_occurrences")
	}
	if summary.ModelTurnsCompleted > summary.ModelTurnsStarted {
		return fmt.Errorf("summary model_turns_completed must not exceed model_turns_started")
	}
	usageReceiptTotal, ok := checkedAgentReviewSum(
		summary.UsageReportedReceipts,
		summary.UsagePartialReceipts,
		summary.UsageUnavailableReceipts,
	)
	if !ok || usageReceiptTotal != summary.Receipts {
		return fmt.Errorf("summary usage receipt counts must equal receipts")
	}
	switch summary.UsageCompleteness {
	case AgentTokenUsageProviderReported:
		if summary.Receipts == 0 || summary.UsageReportedReceipts != summary.Receipts {
			return fmt.Errorf("provider_reported summary usage requires every receipt")
		}
	case AgentTokenUsagePartial:
		if summary.UsagePartialReceipts == 0 &&
			(summary.UsageReportedReceipts == 0 || summary.UsageUnavailableReceipts == 0) {
			return fmt.Errorf(
				"partial summary usage requires a partial receipt or mixed reported/unavailable receipts",
			)
		}
	case AgentTokenUsageUnavailable:
		if summary.UsageReportedReceipts != 0 || summary.UsagePartialReceipts != 0 {
			return fmt.Errorf("unavailable summary usage forbids observed receipt usage")
		}
	default:
		return fmt.Errorf("unsupported summary usage completeness %q", summary.UsageCompleteness)
	}
	tokenTotal, ok := checkedAgentReviewSum(
		summary.InputTokens,
		summary.OutputTokens,
		summary.CacheReadTokens,
		summary.CacheWriteTokens,
	)
	if !ok || tokenTotal != summary.TotalTokens {
		return fmt.Errorf("summary total_tokens must equal observed token components")
	}
	observedUsageReceipts, ok := checkedAgentReviewSum(
		summary.UsageReportedReceipts,
		summary.UsagePartialReceipts,
	)
	if !ok || summary.ReasoningReportedReceipts > observedUsageReceipts ||
		summary.ReasoningTokens > summary.OutputTokens {
		return fmt.Errorf("summary reasoning usage exceeds observed receipt usage")
	}
	return nil
}

func SummarizeAgentReviewResult(
	set ReviewHypothesisSet,
	receipts []AgentExecutionReceipt,
) (AgentReviewResultSummary, AgentReviewRunStatus, error) {
	if err := set.Validate(); err != nil {
		return AgentReviewResultSummary{}, "", fmt.Errorf("validate hypothesis set: %w", err)
	}
	summary := AgentReviewResultSummary{
		RawCandidates:         uint64(len(set.NormalizationDecisions)),
		HypothesisOccurrences: uint64(len(set.Hypotheses)),
		DedupClusters:         uint64(len(set.DedupClusters)),
		Receipts:              uint64(len(receipts)),
		UsageCompleteness:     AgentTokenUsageUnavailable,
	}
	for _, decision := range set.NormalizationDecisions {
		switch decision.Action {
		case HypothesisNormalizationRetained:
			summary.NormalizationRetained++
		case HypothesisNormalizationMergedDuplicate:
			summary.MergedDuplicates++
		case HypothesisNormalizationRejectedInvalid:
			summary.RejectedInvalid++
		case HypothesisNormalizationExcludedBudget:
			summary.ExcludedBudget++
		}
	}
	for _, hypothesis := range set.Hypotheses {
		if len(hypothesis.Verification) == 0 {
			summary.Unverified++
			continue
		}
		switch hypothesis.Verification[len(hypothesis.Verification)-1].Verdict {
		case HypothesisVerificationConfirmed:
			summary.Confirmed++
		case HypothesisVerificationRejected:
			summary.Rejected++
		case HypothesisVerificationInconclusive:
			summary.Inconclusive++
		}
	}
	taskIDs := make(map[string]struct{}, len(receipts))
	previousTaskID := ""
	for index, receipt := range receipts {
		if err := receipt.Validate(); err != nil {
			return AgentReviewResultSummary{}, "", fmt.Errorf("validate receipt[%d]: %w", index, err)
		}
		if _, exists := taskIDs[receipt.TaskID]; exists {
			return AgentReviewResultSummary{}, "", fmt.Errorf("receipts contain duplicate task_id %q", receipt.TaskID)
		}
		if index > 0 && receipt.TaskID <= previousTaskID {
			return AgentReviewResultSummary{}, "", fmt.Errorf("receipts must be uniquely sorted by task_id")
		}
		taskIDs[receipt.TaskID] = struct{}{}
		previousTaskID = receipt.TaskID
		switch receipt.Status {
		case AgentTaskSucceeded:
			summary.TasksSucceeded++
		case AgentTaskFailed:
			summary.TasksFailed++
		case AgentTaskCanceled:
			summary.TasksCanceled++
		}
		var ok bool
		if summary.ModelTurnsStarted, ok = checkedAgentReviewAdd(
			summary.ModelTurnsStarted,
			uint64(receipt.ModelTurnsStarted),
		); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("started model turn summary overflow")
		}
		if summary.ModelTurnsCompleted, ok = checkedAgentReviewAdd(
			summary.ModelTurnsCompleted,
			uint64(receipt.ModelTurnsCompleted),
		); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("completed model turn summary overflow")
		}
		if summary.ToolCalls, ok = checkedAgentReviewAdd(summary.ToolCalls, uint64(receipt.ToolCalls)); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("tool call summary overflow")
		}
		if summary.InputTokens, ok = checkedAgentReviewAdd(summary.InputTokens, receipt.Usage.InputTokens); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("input token summary overflow")
		}
		if summary.OutputTokens, ok = checkedAgentReviewAdd(summary.OutputTokens, receipt.Usage.OutputTokens); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("output token summary overflow")
		}
		if summary.CacheReadTokens, ok = checkedAgentReviewAdd(summary.CacheReadTokens, receipt.Usage.CacheReadTokens); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("cache read token summary overflow")
		}
		if summary.CacheWriteTokens, ok = checkedAgentReviewAdd(summary.CacheWriteTokens, receipt.Usage.CacheWriteTokens); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("cache write token summary overflow")
		}
		if summary.TotalTokens, ok = checkedAgentReviewAdd(summary.TotalTokens, receipt.Usage.TotalTokens); !ok {
			return AgentReviewResultSummary{}, "", fmt.Errorf("total token summary overflow")
		}
		switch receipt.Usage.Completeness {
		case AgentTokenUsageProviderReported:
			summary.UsageReportedReceipts++
		case AgentTokenUsagePartial:
			summary.UsagePartialReceipts++
		}
		if receipt.Usage.Completeness == AgentTokenUsageProviderReported ||
			receipt.Usage.Completeness == AgentTokenUsagePartial {
			if receipt.Usage.ReasoningTokens != nil {
				summary.ReasoningReportedReceipts++
				if summary.ReasoningTokens, ok = checkedAgentReviewAdd(
					summary.ReasoningTokens,
					*receipt.Usage.ReasoningTokens,
				); !ok {
					return AgentReviewResultSummary{}, "", fmt.Errorf("reasoning token summary overflow")
				}
			}
		}
		if receipt.Usage.Completeness == AgentTokenUsageUnavailable {
			summary.UsageUnavailableReceipts++
		}
	}
	if summary.UsageReportedReceipts == summary.Receipts && summary.Receipts > 0 {
		summary.UsageCompleteness = AgentTokenUsageProviderReported
	} else if summary.UsageReportedReceipts > 0 || summary.UsagePartialReceipts > 0 {
		summary.UsageCompleteness = AgentTokenUsagePartial
	}
	status := AgentReviewRunPartial
	if set.Completeness == AgentReviewComplete && summary.TasksFailed == 0 && summary.TasksCanceled == 0 {
		status = AgentReviewRunComplete
	} else if len(receipts) > 0 && summary.TasksSucceeded == 0 {
		status = AgentReviewRunFailed
	}
	return summary, status, nil
}

func ValidateAgentReviewResultManifestBindings(
	manifest AgentReviewResultManifest,
	plan AgentReviewPlan,
	set ReviewHypothesisSet,
	collection AgentExecutionReceiptCollection,
	rawCollections ...AgentReviewRawCandidateCollection,
) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate plan: %w", err)
	}
	if err := set.Validate(); err != nil {
		return fmt.Errorf("validate hypothesis set: %w", err)
	}
	if err := collection.Validate(); err != nil {
		return fmt.Errorf("validate receipt collection: %w", err)
	}
	if manifest.PlanID != plan.PlanID || manifest.PlanID != set.PlanID ||
		manifest.HypothesisSetID != set.HypothesisSetID ||
		manifest.SourceRunID != plan.SourceRunID || manifest.SourceRunID != set.SourceRunID ||
		manifest.ExecutionID != plan.ExecutionID || manifest.ExecutionID != set.ExecutionID ||
		manifest.ReviewRunID != plan.ReviewRunID || manifest.ReviewRunID != set.ReviewRunID ||
		manifest.TargetDigest != plan.TargetDigest || manifest.TargetDigest != set.TargetDigest {
		return fmt.Errorf("manifest identity does not bind the exact plan and hypothesis set")
	}
	if collection.PlanID != plan.PlanID || collection.SourceRunID != plan.SourceRunID ||
		collection.ExecutionID != plan.ExecutionID || collection.ReviewRunID != plan.ReviewRunID {
		return fmt.Errorf("receipt collection does not bind the exact plan identity")
	}
	if len(rawCollections) > 1 {
		return fmt.Errorf("at most one raw candidate collection may be bound")
	}
	if len(rawCollections) == 1 {
		if err := ValidateAgentReviewRawCandidateBindings(rawCollections[0], plan, set); err != nil {
			return err
		}
	}
	if set.GeneratedAt.Before(plan.CreatedAt) || set.GeneratedAt.After(manifest.RecordedAt) {
		return fmt.Errorf("hypothesis set timing is outside the plan/manifest interval")
	}
	if manifest.ExecutionSnapshotRef != plan.ExecutionSnapshotRef ||
		manifest.ReviewInputRef != plan.ReviewInputRef {
		return fmt.Errorf("manifest execution/review refs do not match the plan")
	}
	if set.Coverage.GroupsTotal > plan.Budget.MaxGroups ||
		set.Coverage.FilesIncluded > plan.Budget.MaxFiles {
		return fmt.Errorf("hypothesis coverage exceeds the frozen plan budget")
	}
	if uint64(len(set.Hypotheses)) > uint64(plan.Budget.MaxCandidates) {
		return fmt.Errorf("hypothesis occurrences exceed the frozen candidate budget")
	}
	excludedByBudget := slices.ContainsFunc(
		set.NormalizationDecisions,
		func(decision HypothesisNormalizationDecision) bool {
			return decision.Action == HypothesisNormalizationExcludedBudget
		},
	)
	if excludedByBudget && uint64(len(set.Hypotheses)) != uint64(plan.Budget.MaxCandidates) {
		return fmt.Errorf(
			"excluded_budget normalization requires the retained candidate budget to be exhausted",
		)
	}
	if set.Completeness == AgentReviewComplete && set.Coverage.GroupsTotal == 0 {
		return fmt.Errorf("complete hypothesis set requires at least one review group")
	}

	type logicalReceiptKey struct {
		role       AgentTaskRole
		groupID    string
		dimension  VersionedRef
		occurrence string
	}
	logicalReceipts := make(map[logicalReceiptKey]struct{}, len(collection.Receipts))
	contextGroups := make(map[string]struct{}, len(collection.Receipts))
	reviewSucceededDimensions := make(
		map[string]map[VersionedRef]struct{},
		len(collection.Receipts),
	)
	verificationByOccurrence := make(map[string]AgentExecutionReceipt, len(set.Hypotheses))
	var contextReceipts uint64
	var contextReceiptsSucceeded uint64
	var reviewReceipts uint64
	var reviewReceiptsSucceeded uint64
	var verificationReceipts uint64
	occurrences := make(map[string]ReviewHypothesis, len(set.Hypotheses))
	for index, hypothesis := range set.Hypotheses {
		if !containsVersionedRef(plan.ReviewDimensions, hypothesis.Dimension) {
			return fmt.Errorf("hypothesis[%d] review dimension is outside the plan", index)
		}
		for observationIndex, observation := range hypothesis.Verification {
			if observation.Verifier != plan.Verifier {
				return fmt.Errorf(
					"hypothesis[%d].verification_observations[%d] verifier does not match the plan",
					index,
					observationIndex,
				)
			}
		}
		occurrences[hypothesis.OccurrenceID] = hypothesis
	}
	for index, receipt := range collection.Receipts {
		if receipt.PlanID != plan.PlanID || receipt.SourceRunID != plan.SourceRunID ||
			receipt.ExecutionID != plan.ExecutionID || receipt.ReviewRunID != plan.ReviewRunID ||
			receipt.Runtime != plan.Runtime || receipt.Profile != plan.Profile ||
			receipt.Agent != plan.Agent || receipt.Provider != plan.Provider ||
			receipt.Model != plan.Model || receipt.APIProtocol != plan.APIProtocol {
			return fmt.Errorf("receipt[%d] does not bind the exact plan identity and dimensions", index)
		}
		var budgetedToolCalls uint64
		expectedTerminal := agentTerminalTool(receipt.TaskRole)
		for _, usage := range receipt.ToolUsage {
			switch {
			case slices.Contains(plan.ToolPolicy.AllowedTools, usage.ToolID):
				var ok bool
				budgetedToolCalls, ok = checkedAgentReviewAdd(
					budgetedToolCalls,
					uint64(usage.InvocationCount),
				)
				if !ok {
					return fmt.Errorf("receipt[%d] budgeted tool call overflow", index)
				}
			case usage.ToolID == expectedTerminal:
				// The one typed terminal submit is observable but is not a
				// repository tool invocation charged to max_tool_calls.
			default:
				return fmt.Errorf(
					"receipt[%d] used tool %q outside the frozen plan",
					index,
					usage.ToolID,
				)
			}
		}
		if budgetedToolCalls > uint64(plan.Budget.MaxToolCalls) {
			return fmt.Errorf("receipt[%d] exceeds the frozen repository-tool budget", index)
		}
		if receipt.StartedAt.Before(plan.CreatedAt) || receipt.FinishedAt.After(manifest.RecordedAt) {
			return fmt.Errorf("receipt[%d] timing is outside the plan/manifest interval", index)
		}
		occurrenceID := ""
		if receipt.HypothesisOccurrenceID != nil {
			occurrenceID = *receipt.HypothesisOccurrenceID
		}
		logicalKey := logicalReceiptKey{
			role:       receipt.TaskRole,
			groupID:    receipt.GroupID,
			dimension:  receipt.Dimension,
			occurrence: occurrenceID,
		}
		if _, exists := logicalReceipts[logicalKey]; exists {
			return fmt.Errorf(
				"receipt[%d] duplicates role/group/dimension/occurrence task identity",
				index,
			)
		}
		logicalReceipts[logicalKey] = struct{}{}
		switch receipt.TaskRole {
		case AgentTaskContext:
			if !containsVersionedRef(plan.ContextDimensions, receipt.Dimension) {
				return fmt.Errorf("receipt[%d] context dimension is outside the plan", index)
			}
			contextReceipts++
			contextGroups[receipt.GroupID] = struct{}{}
			if receipt.Status == AgentTaskSucceeded {
				contextReceiptsSucceeded++
			}
		case AgentTaskReview:
			if !containsVersionedRef(plan.ReviewDimensions, receipt.Dimension) {
				return fmt.Errorf("receipt[%d] review dimension is outside the plan", index)
			}
			reviewReceipts++
			if receipt.Status == AgentTaskSucceeded {
				reviewReceiptsSucceeded++
				dimensions := reviewSucceededDimensions[receipt.GroupID]
				if dimensions == nil {
					dimensions = make(map[VersionedRef]struct{}, len(plan.ReviewDimensions))
					reviewSucceededDimensions[receipt.GroupID] = dimensions
				}
				dimensions[receipt.Dimension] = struct{}{}
			}
		case AgentTaskVerification:
			verificationReceipts++
			if receipt.Dimension != plan.Verifier {
				return fmt.Errorf("receipt[%d] verifier dimension does not match the plan", index)
			}
			occurrence, exists := occurrences[*receipt.HypothesisOccurrenceID]
			if !exists || occurrence.GroupID != receipt.GroupID {
				return fmt.Errorf("receipt[%d] does not bind an exact hypothesis occurrence", index)
			}
			if _, exists := verificationByOccurrence[*receipt.HypothesisOccurrenceID]; exists {
				return fmt.Errorf(
					"hypothesis occurrence %q has more than one verification receipt",
					*receipt.HypothesisOccurrenceID,
				)
			}
			verificationByOccurrence[*receipt.HypothesisOccurrenceID] = receipt
		}
	}
	if verificationReceipts > uint64(plan.Budget.MaxCandidates) {
		return fmt.Errorf("verification receipts exceed the frozen candidate budget")
	}
	expectedContextReceipts, ok := checkedAgentReviewMultiply(
		uint64(set.Coverage.GroupsTotal),
		uint64(len(plan.ContextDimensions)),
	)
	if !ok {
		return fmt.Errorf("expected context receipt count overflows")
	}
	expectedReviewReceipts, ok := checkedAgentReviewMultiply(
		uint64(set.Coverage.GroupsTotal),
		uint64(len(plan.ReviewDimensions)),
	)
	if !ok || expectedReviewReceipts > math.MaxUint32 {
		return fmt.Errorf("expected review receipt count exceeds the coverage contract")
	}
	if contextReceipts != expectedContextReceipts {
		return fmt.Errorf(
			"context receipt count %d does not match groups_total * context_dimensions (%d)",
			contextReceipts,
			expectedContextReceipts,
		)
	}
	if uint64(len(contextGroups)) != uint64(set.Coverage.GroupsTotal) {
		return fmt.Errorf(
			"distinct context receipt groups do not match coverage.groups_total",
		)
	}
	if reviewReceipts != expectedReviewReceipts ||
		uint64(set.Coverage.ReviewTasksTotal) != expectedReviewReceipts {
		return fmt.Errorf(
			"review receipt count and coverage.review_tasks_total must equal groups_total * review_dimensions",
		)
	}
	if uint64(set.Coverage.ReviewTasksSucceeded) != reviewReceiptsSucceeded {
		return fmt.Errorf(
			"coverage.review_tasks_succeeded does not match succeeded review receipts",
		)
	}
	for index, receipt := range collection.Receipts {
		if receipt.TaskRole == AgentTaskContext {
			continue
		}
		if _, exists := contextGroups[receipt.GroupID]; !exists {
			return fmt.Errorf("receipt[%d] group is outside the context receipt groups", index)
		}
	}
	var groupsReviewed uint64
	for groupID := range contextGroups {
		if len(reviewSucceededDimensions[groupID]) == len(plan.ReviewDimensions) {
			groupsReviewed++
		}
	}
	if uint64(set.Coverage.GroupsReviewed) != groupsReviewed {
		return fmt.Errorf("coverage.groups_reviewed does not match fully succeeded review groups")
	}
	if set.Completeness == AgentReviewComplete &&
		(contextReceiptsSucceeded != contextReceipts ||
			reviewReceiptsSucceeded != reviewReceipts) {
		return fmt.Errorf("complete hypothesis set requires every context and review receipt to succeed")
	}
	for _, hypothesis := range set.Hypotheses {
		if _, exists := contextGroups[hypothesis.GroupID]; !exists {
			return fmt.Errorf(
				"hypothesis occurrence %q group is outside the context receipt groups",
				hypothesis.OccurrenceID,
			)
		}
		verificationReceipt, hasReceipt := verificationByOccurrence[hypothesis.OccurrenceID]
		hasObservation := len(hypothesis.Verification) > 0
		if hasObservation && (!hasReceipt || verificationReceipt.Status != AgentTaskSucceeded) {
			return fmt.Errorf(
				"hypothesis occurrence %q verification observations require a succeeded receipt",
				hypothesis.OccurrenceID,
			)
		}
		verified := hasObservation && hasReceipt && verificationReceipt.Status == AgentTaskSucceeded
		if set.Completeness == AgentReviewComplete {
			if !verified {
				return fmt.Errorf(
					"complete hypothesis occurrence %q requires one succeeded verification receipt and an observation",
					hypothesis.OccurrenceID,
				)
			}
			continue
		}
		if !verified && !hasAgentReviewVerificationGap(set.Coverage.Gaps, hypothesis.OccurrenceID) {
			return fmt.Errorf(
				"unverified hypothesis occurrence %q requires a verification coverage gap",
				hypothesis.OccurrenceID,
			)
		}
	}
	summary, status, err := SummarizeAgentReviewResult(set, collection.Receipts)
	if err != nil {
		return err
	}
	if summary.ModelTurnsStarted > uint64(plan.Budget.MaxModelCalls) {
		return fmt.Errorf("started receipt model turns exceed the frozen model-call budget")
	}
	if set.Completeness == AgentReviewComplete && summary.Unverified != 0 {
		return fmt.Errorf("complete manifest summary must not contain unverified hypotheses")
	}
	if manifest.Summary != summary || manifest.Status != status {
		return fmt.Errorf("manifest summary/status is not the host recomputation")
	}
	return nil
}

// ValidateAgentReviewRawCandidateBindings closes the exact raw model claims
// over the governed plan and every normalization decision. It is separate
// from manifest validation so formal StageExecutionResult admission can reuse
// the same invariant before a ReviewRun terminal exists.
func ValidateAgentReviewRawCandidateBindings(
	raw AgentReviewRawCandidateCollection,
	plan AgentReviewPlan,
	set ReviewHypothesisSet,
) error {
	if err := raw.Validate(); err != nil {
		return fmt.Errorf("validate raw candidate collection: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate plan for raw candidates: %w", err)
	}
	if err := set.Validate(); err != nil {
		return fmt.Errorf("validate hypothesis set for raw candidates: %w", err)
	}
	if raw.PlanID != plan.PlanID || raw.SourceRunID != plan.SourceRunID ||
		raw.ExecutionID != plan.ExecutionID || raw.ReviewRunID != plan.ReviewRunID ||
		raw.TargetDigest != plan.TargetDigest || set.PlanID != plan.PlanID ||
		set.SourceRunID != plan.SourceRunID || set.ExecutionID != plan.ExecutionID ||
		set.ReviewRunID != plan.ReviewRunID || set.TargetDigest != plan.TargetDigest {
		return fmt.Errorf("raw candidate collection does not bind the exact plan, set, and target")
	}
	return ValidateAgentReviewRawCandidateSetBindings(raw, set, plan.ReviewDimensions)
}

// ValidateAgentReviewRawCandidateSetBindings closes raw claims over an exact
// hypothesis set and the host-admitted review dimensions. The caller owns the
// outer plan identity; this helper is usable by both shadow and formal plans.
func ValidateAgentReviewRawCandidateSetBindings(
	raw AgentReviewRawCandidateCollection,
	set ReviewHypothesisSet,
	reviewDimensions []VersionedRef,
) error {
	if err := raw.Validate(); err != nil {
		return fmt.Errorf("validate raw candidate collection: %w", err)
	}
	if err := set.Validate(); err != nil {
		return fmt.Errorf("validate hypothesis set for raw candidates: %w", err)
	}
	if raw.PlanID != set.PlanID || raw.SourceRunID != set.SourceRunID ||
		raw.ExecutionID != set.ExecutionID || raw.ReviewRunID != set.ReviewRunID ||
		raw.TargetDigest != set.TargetDigest {
		return fmt.Errorf("raw candidate collection does not bind the exact hypothesis set")
	}
	if len(raw.RawCandidates) != len(set.NormalizationDecisions) {
		return fmt.Errorf("raw candidate collection does not close every normalization decision")
	}
	for index, candidate := range raw.RawCandidates {
		decision := set.NormalizationDecisions[index]
		if candidate.RawCandidateID != decision.RawCandidateID ||
			candidate.ClaimDigest != decision.ClaimDigest ||
			candidate.Action != decision.Action || candidate.ReasonCode != decision.ReasonCode {
			return fmt.Errorf("raw candidate[%d] does not bind its normalization decision", index)
		}
		if !containsVersionedRef(reviewDimensions, candidate.Dimension) {
			return fmt.Errorf("raw candidate[%d] review dimension is outside the plan", index)
		}
	}
	return nil
}

func hasAgentReviewVerificationGap(gaps []AgentReviewCoverageGap, occurrenceID string) bool {
	return slices.ContainsFunc(gaps, func(gap AgentReviewCoverageGap) bool {
		return gap.Phase == AgentReviewCoverageVerification && gap.SubjectID == occurrenceID
	})
}

func containsVersionedRef(refs []VersionedRef, target VersionedRef) bool {
	return slices.Contains(refs, target)
}

func (observation AgentReviewObservation) Validate() error {
	if observation.SchemaVersion != AgentReviewObservationSchemaVersion {
		return fmt.Errorf("unsupported AgentReviewObservation schema %q", observation.SchemaVersion)
	}
	for name, value := range map[string]string{
		"observation_id": observation.ObservationID, "manifest_id": observation.ManifestID,
		"source_run_id": observation.SourceRunID, "execution_id": observation.ExecutionID,
		"review_run_id": observation.ReviewRunID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if observation.Sequence == 0 {
		return fmt.Errorf("sequence must be positive")
	}
	if observation.Kind != AgentReviewObservationShadowResultRecorded {
		return fmt.Errorf("unsupported observation kind %q", observation.Kind)
	}
	if observation.ProducerClass != "argus_go_host" {
		return fmt.Errorf("producer_class must be argus_go_host")
	}
	if observation.Disposition != "shadow_only" {
		return fmt.Errorf("disposition must be shadow_only")
	}
	switch observation.Status {
	case AgentReviewRunComplete, AgentReviewRunPartial, AgentReviewRunFailed:
	default:
		return fmt.Errorf("unsupported observation status %q", observation.Status)
	}
	for name, ref := range map[string]VersionedRef{
		"agent": observation.Agent, "provider": observation.Provider,
		"model": observation.Model,
	} {
		if err := ref.validate(name); err != nil {
			return err
		}
	}
	if err := observation.Summary.validate(); err != nil {
		return err
	}
	if err := validateSortedReasonCodes("reason_codes", observation.ReasonCodes); err != nil {
		return err
	}
	return validateAgentReviewTime("recorded_at", observation.RecordedAt)
}

func ValidateAgentReviewObservationBinding(
	observation AgentReviewObservation,
	manifest AgentReviewResultManifest,
	plan AgentReviewPlan,
) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	if observation.ManifestID != manifest.ManifestID ||
		observation.SourceRunID != manifest.SourceRunID ||
		observation.ExecutionID != manifest.ExecutionID ||
		observation.ReviewRunID != manifest.ReviewRunID ||
		observation.Status != manifest.Status || observation.Summary != manifest.Summary ||
		observation.Agent != plan.Agent || observation.Provider != plan.Provider ||
		observation.Model != plan.Model || observation.RecordedAt.Before(manifest.RecordedAt) {
		return fmt.Errorf("observation does not bind the exact manifest and plan dimensions")
	}
	return nil
}

func ValidateAgentReviewObservationAppend(
	previous AgentReviewObservation,
	next AgentReviewObservation,
) error {
	if err := previous.Validate(); err != nil {
		return fmt.Errorf("validate previous observation: %w", err)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("validate next observation: %w", err)
	}
	if next.ObservationID == previous.ObservationID || next.ManifestID != previous.ManifestID ||
		next.SourceRunID != previous.SourceRunID || next.ExecutionID != previous.ExecutionID ||
		next.ReviewRunID != previous.ReviewRunID || next.Sequence != previous.Sequence+1 ||
		next.RecordedAt.Before(previous.RecordedAt) || next.Kind != previous.Kind ||
		next.ProducerClass != previous.ProducerClass || next.Disposition != previous.Disposition ||
		next.Status != previous.Status || next.Agent != previous.Agent ||
		next.Provider != previous.Provider || next.Model != previous.Model ||
		next.Summary != previous.Summary || next.DurationMS != previous.DurationMS ||
		!slices.Equal(next.ReasonCodes, previous.ReasonCodes) {
		return fmt.Errorf("next observation is not a valid append-only successor")
	}
	return nil
}

func checkedAgentReviewSum(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		var ok bool
		total, ok = checkedAgentReviewAdd(total, value)
		if !ok {
			return 0, false
		}
	}
	return total, true
}

func checkedAgentReviewAdd(left, right uint64) (uint64, bool) {
	if right > math.MaxUint64-left {
		return 0, false
	}
	return left + right, true
}

func checkedAgentReviewMultiply(left, right uint64) (uint64, bool) {
	if left != 0 && right > math.MaxUint64/left {
		return 0, false
	}
	return left * right, true
}

func validateVersionedRefs(name string, refs []VersionedRef, nonEmpty bool) error {
	if refs == nil || nonEmpty && len(refs) == 0 {
		return fmt.Errorf("%s must be an explicit%s array", name, map[bool]string{true: " non-empty", false: ""}[nonEmpty])
	}
	previous := ""
	for index, ref := range refs {
		if err := ref.validate(fmt.Sprintf("%s[%d]", name, index)); err != nil {
			return err
		}
		key := ref.ID + "\x00" + ref.Revision + "\x00" + ref.SHA256
		if index > 0 && key <= previous {
			return fmt.Errorf("%s must be uniquely sorted by id, revision, and sha256", name)
		}
		previous = key
	}
	return nil
}

func validateSortedReasonCodes(name string, values []string) error {
	if values == nil || !slices.IsSorted(values) {
		return fmt.Errorf("%s must be an explicit sorted array", name)
	}
	for index, value := range values {
		if err := requireIdentifier(fmt.Sprintf("%s[%d]", name, index), value); err != nil {
			return err
		}
		if index > 0 && value == values[index-1] {
			return fmt.Errorf("%s contains duplicate %q", name, value)
		}
	}
	return nil
}

func requireBoundedAgentReviewText(name, value string, maxBytes int, trimmed bool) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		strings.ContainsRune(value, '\x00') || trimmed && value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be non-empty valid UTF-8 bounded to %d bytes", name, maxBytes)
	}
	return nil
}

func validateAgentReviewTime(name string, value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC {
		return fmt.Errorf("%s must be a non-zero UTC timestamp", name)
	}
	return nil
}
