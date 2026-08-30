package runmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	RunSchemaVersion             = "argus.review_run.v1alpha1"
	SnapshotSchemaVersion        = "argus.execution_snapshot.v1alpha1"
	ComparisonSchemaVersion      = "argus.run_comparison.v1alpha1"
	ReplayChangeSetSchemaVersion = "argus.replay_change_set.v1alpha1"

	ContractReviewSpec                      = "argus.review_spec.v1alpha1"
	ContractMaterializedTarget              = "argus.materialized_target.v1alpha1"
	ContractReviewInput                     = "argus.review_input.v1alpha1"
	ContractWorkflowDefinition              = "argus.workflow.v1alpha1"
	ContractConfigBundle                    = "argus.config_bundle.v1alpha1"
	ContractCanonicalPatch                  = "argus.git_patch.v1alpha1"
	ContractSelectionContent                = "argus.selection_content.v1alpha1"
	ContractStageResult                     = "argus.reviewcore_artifact.v1alpha1"
	ContractFindingSet                      = "argus.finding_set.v1alpha1"
	ContractJSONReport                      = "argus.review_report.v1alpha1+json"
	ContractMarkdownReport                  = "argus.review_report.v1alpha1+markdown"
	ContractGovernedCandidateSet            = "argus.governed_candidate_set.v1alpha1+json"
	ContractCandidateVerificationLedger     = "argus.candidate_verification_ledger.v1alpha1+json"
	ContractFindingCalibrationLedger        = "argus.finding_calibration_ledger.v1alpha1+json"
	ContractFindingSuppressionLedger        = "argus.finding_suppression_ledger.v1alpha1+json"
	ContractGovernedReviewReport            = "argus.governed_review_report.v1alpha1+json"
	ContractFindingLineage                  = "argus.finding_lineage.v1alpha1+json"
	ContractTrainingDatasetManifest         = "argus.training_dataset_manifest.v1alpha1+json"
	ContractTrainingExportBundle            = "argus.training_export_bundle.v1alpha1+json"
	ContractTrainingRedactedText            = "argus.training_redacted_text.v1alpha1"
	ContractTrainingJobPlan                 = "argus.training_job_plan.v1alpha1+json"
	ContractTrainingProviderJobReceipt      = "argus.training_provider_job_receipt.v1alpha1+json"
	ContractGovernedReviewMarkdown          = "argus.governed_review_report.v1alpha1+markdown"
	ContractReviewRun                       = RunSchemaVersion
	ContractReplayChangeSet                 = ReplayChangeSetSchemaVersion
	ContractExecutionSnapshot               = SnapshotSchemaVersion
	ContractConfigResolutionReceipt         = "argus.config_resolution_receipt.v1alpha1"
	ContractLocalRuntimeFileManifest        = "argus.local_runtime_file_manifest.v1alpha1"
	ContractAgentReviewTaskEvidence         = "argus.agent_review_task_evidence_collection.v1alpha1"
	ContractAgentReviewRawCandidates        = "argus.agent_review_raw_candidate_collection.v1alpha1"
	ContractAgentExecutionReceipts          = "argus.agent_execution_receipt_collection.v1alpha1"
	ContractContextProviderExecutionReceipt = "argus.context_provider_execution_receipt.v1alpha1"
	ContractReviewShardManifest             = "argus.review_shard_manifest.v1alpha1"
	ContractReviewShardAggregate            = "argus.review_shard_aggregate.v1alpha1"
)

type RunKind string

const (
	RunKindReview RunKind = "review"
	RunKindReplay RunKind = "replay"
)

type ReplayVariable string

const (
	ReplayVariableNone              ReplayVariable = "none"
	ReplayVariableRulePack          ReplayVariable = "rule_pack"
	ReplayVariableSkillPack         ReplayVariable = "skill_pack"
	ReplayVariableKnowledgePack     ReplayVariable = "knowledge_pack"
	ReplayVariablePrompt            ReplayVariable = "prompt"
	ReplayVariableModel             ReplayVariable = "model"
	ReplayVariableIndex             ReplayVariable = "index"
	ReplayVariableWorkflow          ReplayVariable = "workflow"
	ReplayVariableFilterPolicy      ReplayVariable = "filter_policy"
	ReplayVariableFindingGovernance ReplayVariable = "finding_governance"
	ReplayVariableBudget            ReplayVariable = "budget"
)

// IsFilterPolicy reports whether the variable represents the governed
// post-verification filtering policy. FindingGovernance is the legacy wire
// name retained so immutable v1alpha1 runs remain readable and replayable.
func (variable ReplayVariable) IsFilterPolicy() bool {
	return variable == ReplayVariableFilterPolicy ||
		variable == ReplayVariableFindingGovernance
}

type TargetMode string

const (
	TargetModeDiff      TargetMode = "diff"
	TargetModeSelection TargetMode = "selection"
	TargetModeScope     TargetMode = "scope"
)

type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCanceled  RunStatus = "canceled"
)

type StageStatus string

const (
	StageStatusPending   StageStatus = "pending"
	StageStatusRunning   StageStatus = "running"
	StageStatusSucceeded StageStatus = "succeeded"
	StageStatusFailed    StageStatus = "failed"
	StageStatusCanceled  StageStatus = "canceled"
)

type Completeness string

const (
	CompletenessComplete Completeness = "complete"
	CompletenessPartial  Completeness = "partial"
)

type ArtifactRef struct {
	URI       string `json:"uri"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Contract  string `json:"contract"`
}

type WorkflowRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

type PolicyRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

type ToolInvocationPolicy struct {
	AllowedTools       []string `json:"allowed_tools"`
	Network            string   `json:"network"`
	WorkspaceWrites    string   `json:"workspace_writes"`
	RemoteWrites       string   `json:"remote_writes"`
	PerCallTimeoutMS   int64    `json:"per_call_timeout_ms"`
	MaxOutputBytes     int64    `json:"max_output_bytes"`
	MaxConcurrency     int      `json:"max_concurrency"`
	MaxDelegationDepth int      `json:"max_delegation_depth"`
}

type ExecutionSnapshot struct {
	SchemaVersion              string               `json:"schema_version"`
	ExecutionSnapshotID        string               `json:"execution_snapshot_id"`
	ReviewSpecSHA256           string               `json:"review_spec_sha256"`
	ReviewSpecRef              ArtifactRef          `json:"review_spec_ref"`
	TargetSnapshotRef          ArtifactRef          `json:"target_snapshot_ref"`
	ReviewInputRef             ArtifactRef          `json:"review_input_ref"`
	ReviewShardManifestRef     *ArtifactRef         `json:"review_shard_manifest_ref,omitempty"`
	ReplayInputRefs            []ArtifactRef        `json:"replay_input_refs"`
	ReplaySourceRunRef         *ArtifactRef         `json:"replay_source_run_ref,omitempty"`
	ReplayChangeSetRef         *ArtifactRef         `json:"replay_change_set_ref,omitempty"`
	RuntimeEvidenceRefs        []ArtifactRef        `json:"runtime_evidence_refs,omitempty"`
	ContextProviderReceiptRefs []ArtifactRef        `json:"context_provider_receipt_refs,omitempty"`
	WorkflowDefinitionRef      ArtifactRef          `json:"workflow_definition_ref"`
	ConfigBundleRef            ArtifactRef          `json:"config_bundle_ref"`
	Workflow                   WorkflowRef          `json:"workflow"`
	Config                     PolicyRef            `json:"config"`
	RuntimeProfile             string               `json:"runtime_profile"`
	BuildIdentity              string               `json:"build_identity"`
	ToolPolicy                 ToolInvocationPolicy `json:"tool_policy"`
	RemoteWrites               string               `json:"remote_writes"`
	CreatedAt                  time.Time            `json:"created_at"`
}

// ReplayChangeSet is the pre-dispatch experiment fact. Namespace prevents a
// replay from overwriting production or another experiment; SourceRunID is the
// immediate parent while RootRunID preserves the original review lineage.
type ReplayChangeSet struct {
	SchemaVersion     string         `json:"schema_version"`
	Namespace         string         `json:"namespace"`
	SourceRunID       string         `json:"source_run_id"`
	RootRunID         string         `json:"root_run_id"`
	ParentReplayRunID string         `json:"parent_replay_run_id,omitempty"`
	StartStage        string         `json:"start_stage"`
	Variable          ReplayVariable `json:"variable"`
	BaselineSHA256    string         `json:"baseline_sha256"`
	VariantSHA256     string         `json:"variant_sha256"`
	ChangedFields     []string       `json:"changed_fields"`
	RemoteWrites      string         `json:"remote_writes"`
	CreatedAt         time.Time      `json:"created_at"`
}

type StageAttempt struct {
	StageID      string        `json:"stage_id"`
	Attempt      int           `json:"attempt"`
	Generation   int           `json:"generation"`
	BindingID    string        `json:"binding_id"`
	Status       StageStatus   `json:"status"`
	InputRefs    []ArtifactRef `json:"input_refs"`
	OutputRef    *ArtifactRef  `json:"output_ref,omitempty"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
	DurationMS   int64         `json:"duration_ms"`
	ErrorCode    string        `json:"error_code,omitempty"`
	ErrorMessage string        `json:"error_message,omitempty"`
	Retryable    bool          `json:"retryable"`
}

// PlatformExecutionBinding is the independently persisted admission fact that
// binds one logical stage attempt to one concrete runtime execution. The
// fencing token prevents a late result from an older runtime lease from being
// accepted as evidence for a newer execution.
type PlatformExecutionBinding struct {
	BindingID      string    `json:"binding_id"`
	RunID          string    `json:"run_id"`
	StageID        string    `json:"stage_id"`
	Attempt        int       `json:"attempt"`
	Generation     int       `json:"generation"`
	IdempotencyKey string    `json:"idempotency_key"`
	FencingToken   uint64    `json:"fencing_token"`
	RuntimeKind    string    `json:"runtime_kind"`
	RuntimeID      string    `json:"runtime_id"`
	CreatedAt      time.Time `json:"created_at"`
}

type RunEvidence struct {
	EvidenceID        string       `json:"evidence_id"`
	BindingID         string       `json:"platform_execution_binding_id"`
	StageID           string       `json:"stage_id"`
	Attempt           int          `json:"attempt"`
	Generation        int          `json:"generation"`
	IdempotencyKey    string       `json:"idempotency_key"`
	FencingToken      uint64       `json:"fencing_token"`
	ArtifactRef       ArtifactRef  `json:"artifact_ref"`
	Completeness      Completeness `json:"completeness"`
	CompletenessNotes []string     `json:"completeness_reasons"`
	RecordedAt        time.Time    `json:"recorded_at"`
}

type Failure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	StageID   string `json:"stage_id,omitempty"`
	Retryable bool   `json:"retryable"`
}

type ReviewRun struct {
	SchemaVersion             string                     `json:"schema_version"`
	RunID                     string                     `json:"run_id"`
	Kind                      RunKind                    `json:"kind"`
	SourceRunID               string                     `json:"source_run_id,omitempty"`
	ReplayFromStage           string                     `json:"replay_from_stage,omitempty"`
	ReplayRootRunID           string                     `json:"replay_root_run_id,omitempty"`
	ReplayNamespace           string                     `json:"replay_namespace,omitempty"`
	ReplayVariable            ReplayVariable             `json:"replay_variable,omitempty"`
	RepositoryPath            string                     `json:"repository_path"`
	TargetMode                TargetMode                 `json:"target_mode"`
	BaseRevision              string                     `json:"base_revision"`
	HeadRevision              string                     `json:"head_revision"`
	Status                    RunStatus                  `json:"status"`
	ExecutionSnapshotID       string                     `json:"execution_snapshot_id"`
	TargetSnapshotRef         ArtifactRef                `json:"target_snapshot_ref"`
	StageAttempts             []StageAttempt             `json:"stage_attempts"`
	Bindings                  []PlatformExecutionBinding `json:"platform_execution_bindings"`
	Evidence                  []RunEvidence              `json:"run_evidence"`
	FindingSetRef             *ArtifactRef               `json:"finding_set_ref,omitempty"`
	JSONReportRef             *ArtifactRef               `json:"json_report_ref,omitempty"`
	MarkdownReportRef         *ArtifactRef               `json:"markdown_report_ref,omitempty"`
	HypothesisSetRef          *ArtifactRef               `json:"hypothesis_set_ref,omitempty"`
	RawCandidateCollectionRef *ArtifactRef               `json:"raw_candidate_collection_ref,omitempty"`
	AgentTaskEvidenceRef      *ArtifactRef               `json:"agent_task_evidence_ref,omitempty"`
	AgentExecutionReceiptRef  *ArtifactRef               `json:"agent_execution_receipt_ref,omitempty"`
	CandidateSetRef           *ArtifactRef               `json:"candidate_set_ref,omitempty"`
	VerificationLedgerRef     *ArtifactRef               `json:"verification_ledger_ref,omitempty"`
	CalibrationLedgerRef      *ArtifactRef               `json:"calibration_ledger_ref,omitempty"`
	SuppressionLedgerRef      *ArtifactRef               `json:"suppression_ledger_ref,omitempty"`
	GovernedReportRef         *ArtifactRef               `json:"governed_report_ref,omitempty"`
	GovernedMarkdownRef       *ArtifactRef               `json:"governed_markdown_ref,omitempty"`
	Failure                   *Failure                   `json:"failure,omitempty"`
	CreatedAt                 time.Time                  `json:"created_at"`
	StartedAt                 *time.Time                 `json:"started_at,omitempty"`
	CompletedAt               *time.Time                 `json:"completed_at,omitempty"`
}

type FindingChange struct {
	Fingerprint string `json:"fingerprint"`
	Title       string `json:"title"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
}

type CandidateChange struct {
	CandidateID        string `json:"candidate_id"`
	VariantCandidateID string `json:"variant_candidate_id,omitempty"`
	Fingerprint        string `json:"fingerprint,omitempty"`
	RuleID             string `json:"rule_id"`
	Path               string `json:"path"`
	Line               int    `json:"line"`
}

type DecisionChange struct {
	FindingID      string `json:"finding_id"`
	Fingerprint    string `json:"fingerprint"`
	BaselineAction string `json:"baseline_action,omitempty"`
	VariantAction  string `json:"variant_action,omitempty"`
}

type StageLatencyDelta struct {
	StageID            string `json:"stage_id"`
	BaselineDurationMS int64  `json:"baseline_duration_ms"`
	VariantDurationMS  int64  `json:"variant_duration_ms"`
	DeltaMS            int64  `json:"delta_ms"`
	BaselineReused     bool   `json:"baseline_reused"`
	VariantReused      bool   `json:"variant_reused"`
}

type MetricAvailability struct {
	Available  bool   `json:"available"`
	ReasonCode string `json:"reason_code,omitempty"`
}

type RunComparison struct {
	SchemaVersion       string              `json:"schema_version"`
	BaselineRunID       string              `json:"baseline_run_id"`
	VariantRunID        string              `json:"variant_run_id"`
	Added               []FindingChange     `json:"added"`
	Removed             []FindingChange     `json:"removed"`
	Unchanged           []FindingChange     `json:"unchanged"`
	CandidatesAdded     []CandidateChange   `json:"candidates_added"`
	CandidatesRemoved   []CandidateChange   `json:"candidates_removed"`
	CandidatesUnchanged []CandidateChange   `json:"candidates_unchanged"`
	DecisionChanges     []DecisionChange    `json:"decision_changes"`
	StageLatency        []StageLatencyDelta `json:"stage_latency"`
	Quality             MetricAvailability  `json:"quality"`
	Cost                MetricAvailability  `json:"cost"`
	GeneratedAt         time.Time           `json:"generated_at"`
}

func (snapshot ExecutionSnapshot) Validate() error {
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		return fmt.Errorf("unsupported execution snapshot schema %q", snapshot.SchemaVersion)
	}
	if err := requireID("execution_snapshot_id", snapshot.ExecutionSnapshotID); err != nil {
		return err
	}
	if err := requireSHA256("review_spec_sha256", snapshot.ReviewSpecSHA256); err != nil {
		return err
	}
	if err := snapshot.ReviewSpecRef.Validate(); err != nil {
		return fmt.Errorf("review_spec_ref: %w", err)
	}
	if snapshot.ReviewSpecRef.Contract != ContractReviewSpec {
		return fmt.Errorf("review_spec_ref contract is %q, want %q",
			snapshot.ReviewSpecRef.Contract, ContractReviewSpec)
	}
	if snapshot.ReviewSpecRef.SHA256 != snapshot.ReviewSpecSHA256 {
		return fmt.Errorf("review_spec_ref digest does not match review_spec_sha256")
	}
	if err := snapshot.TargetSnapshotRef.Validate(); err != nil {
		return fmt.Errorf("target_snapshot_ref: %w", err)
	}
	if snapshot.TargetSnapshotRef.Contract != ContractMaterializedTarget {
		return fmt.Errorf("target_snapshot_ref contract is %q, want %q",
			snapshot.TargetSnapshotRef.Contract, ContractMaterializedTarget)
	}
	if err := snapshot.ReviewInputRef.Validate(); err != nil {
		return fmt.Errorf("review_input_ref: %w", err)
	}
	if snapshot.ReviewInputRef.Contract != ContractReviewInput {
		return fmt.Errorf("review_input_ref contract is %q, want %q",
			snapshot.ReviewInputRef.Contract, ContractReviewInput)
	}
	if snapshot.ReviewShardManifestRef != nil {
		if err := snapshot.ReviewShardManifestRef.Validate(); err != nil {
			return fmt.Errorf("review_shard_manifest_ref: %w", err)
		}
		if snapshot.ReviewShardManifestRef.Contract != ContractReviewShardManifest {
			return fmt.Errorf("review_shard_manifest_ref contract is %q, want %q",
				snapshot.ReviewShardManifestRef.Contract, ContractReviewShardManifest)
		}
	}
	if snapshot.ReplayInputRefs == nil {
		return fmt.Errorf("replay_input_refs must be an explicit array")
	}
	seenReplayInputs := make(map[ArtifactRef]struct{}, len(snapshot.ReplayInputRefs))
	for index, ref := range snapshot.ReplayInputRefs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("replay_input_refs[%d]: %w", index, err)
		}
		if ref.Contract != ContractStageResult {
			return fmt.Errorf("replay_input_refs[%d] contract is %q, want %q",
				index, ref.Contract, ContractStageResult)
		}
		if _, duplicate := seenReplayInputs[ref]; duplicate {
			return fmt.Errorf("replay_input_refs contains duplicate artifact %q", ref.URI)
		}
		seenReplayInputs[ref] = struct{}{}
	}
	if snapshot.ReplaySourceRunRef != nil {
		if err := snapshot.ReplaySourceRunRef.Validate(); err != nil {
			return fmt.Errorf("replay_source_run_ref: %w", err)
		}
		if snapshot.ReplaySourceRunRef.Contract != ContractReviewRun {
			return fmt.Errorf("replay_source_run_ref contract is %q, want %q",
				snapshot.ReplaySourceRunRef.Contract, ContractReviewRun)
		}
	}
	if snapshot.ReplayChangeSetRef != nil {
		if err := snapshot.ReplayChangeSetRef.Validate(); err != nil {
			return fmt.Errorf("replay_change_set_ref: %w", err)
		}
		if snapshot.ReplayChangeSetRef.Contract != ContractReplayChangeSet {
			return fmt.Errorf("replay_change_set_ref contract is %q, want %q",
				snapshot.ReplayChangeSetRef.Contract, ContractReplayChangeSet)
		}
	}
	seenRuntimeEvidence := make(map[ArtifactRef]struct{}, len(snapshot.RuntimeEvidenceRefs))
	for index, ref := range snapshot.RuntimeEvidenceRefs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("runtime_evidence_refs[%d]: %w", index, err)
		}
		if ref.Contract != ContractLocalRuntimeFileManifest {
			return fmt.Errorf(
				"runtime_evidence_refs[%d] contract is %q, want %q",
				index, ref.Contract, ContractLocalRuntimeFileManifest,
			)
		}
		if _, duplicate := seenRuntimeEvidence[ref]; duplicate {
			return fmt.Errorf("runtime_evidence_refs contains duplicate artifact %q", ref.URI)
		}
		seenRuntimeEvidence[ref] = struct{}{}
	}
	seenProviderReceipts := make(map[ArtifactRef]struct{}, len(snapshot.ContextProviderReceiptRefs))
	for index, ref := range snapshot.ContextProviderReceiptRefs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("context_provider_receipt_refs[%d]: %w", index, err)
		}
		if ref.Contract != ContractContextProviderExecutionReceipt {
			return fmt.Errorf("context_provider_receipt_refs[%d] has unexpected contract %q", index, ref.Contract)
		}
		if _, duplicate := seenProviderReceipts[ref]; duplicate {
			return fmt.Errorf("context_provider_receipt_refs contains duplicate artifact %q", ref.URI)
		}
		seenProviderReceipts[ref] = struct{}{}
	}
	if err := snapshot.WorkflowDefinitionRef.Validate(); err != nil {
		return fmt.Errorf("workflow_definition_ref: %w", err)
	}
	if snapshot.WorkflowDefinitionRef.Contract != ContractWorkflowDefinition {
		return fmt.Errorf("workflow_definition_ref contract is %q, want %q",
			snapshot.WorkflowDefinitionRef.Contract, ContractWorkflowDefinition)
	}
	if err := snapshot.ConfigBundleRef.Validate(); err != nil {
		return fmt.Errorf("config_bundle_ref: %w", err)
	}
	if snapshot.ConfigBundleRef.Contract != ContractConfigBundle {
		return fmt.Errorf("config_bundle_ref contract is %q, want %q",
			snapshot.ConfigBundleRef.Contract, ContractConfigBundle)
	}
	if err := validateWorkflowRef(snapshot.Workflow); err != nil {
		return err
	}
	if snapshot.WorkflowDefinitionRef.SHA256 != snapshot.Workflow.SHA256 {
		return fmt.Errorf("workflow_definition_ref digest does not match workflow.sha256")
	}
	if err := validatePolicyRef(snapshot.Config); err != nil {
		return err
	}
	if snapshot.RuntimeProfile == "" || snapshot.BuildIdentity == "" {
		return fmt.Errorf("runtime_profile and build_identity are required")
	}
	if snapshot.RemoteWrites != "deny" || snapshot.ToolPolicy.RemoteWrites != "deny" {
		return fmt.Errorf("remote writes must be denied")
	}
	if snapshot.ToolPolicy.Network != "deny" || snapshot.ToolPolicy.WorkspaceWrites != "deny" {
		return fmt.Errorf("local deterministic runtime must deny network and workspace writes")
	}
	if snapshot.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func (run ReviewRun) Validate() error {
	if run.SchemaVersion != RunSchemaVersion {
		return fmt.Errorf("unsupported review run schema %q", run.SchemaVersion)
	}
	if err := requireID("run_id", run.RunID); err != nil {
		return err
	}
	switch run.Kind {
	case RunKindReview:
		if run.SourceRunID != "" || run.ReplayFromStage != "" ||
			run.ReplayRootRunID != "" || run.ReplayNamespace != "" ||
			run.ReplayVariable != "" {
			return fmt.Errorf("review run cannot have replay lineage")
		}
	case RunKindReplay:
		if err := requireID("source_run_id", run.SourceRunID); err != nil {
			return err
		}
		if run.SourceRunID == run.RunID {
			return fmt.Errorf("replay source_run_id must differ from run_id")
		}
		if err := requireID("replay_from_stage", run.ReplayFromStage); err != nil {
			return err
		}
		if err := requireID("replay_root_run_id", run.ReplayRootRunID); err != nil {
			return err
		}
		if err := requireID("replay_namespace", run.ReplayNamespace); err != nil {
			return err
		}
		if err := run.ReplayVariable.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported run kind %q", run.Kind)
	}
	if run.RepositoryPath == "" || run.BaseRevision == "" || run.HeadRevision == "" {
		return fmt.Errorf("repository_path, base_revision, and head_revision are required")
	}
	switch run.TargetMode {
	case TargetModeDiff:
		if run.BaseRevision == run.HeadRevision {
			return fmt.Errorf("diff run requires distinct base and head revisions")
		}
	case TargetModeSelection, TargetModeScope:
		if run.BaseRevision != run.HeadRevision {
			return fmt.Errorf("%s run requires one exact revision", run.TargetMode)
		}
	default:
		return fmt.Errorf("unsupported target_mode %q", run.TargetMode)
	}
	switch run.Status {
	case RunStatusPending, RunStatusRunning, RunStatusSucceeded, RunStatusFailed, RunStatusCanceled:
	default:
		return fmt.Errorf("unsupported run status %q", run.Status)
	}
	if run.Status == RunStatusSucceeded || run.Status == RunStatusFailed ||
		run.Status == RunStatusCanceled {
		if run.StartedAt == nil || run.StartedAt.IsZero() ||
			run.CompletedAt == nil || run.CompletedAt.IsZero() {
			return fmt.Errorf("terminal run requires start and completion timestamps")
		}
	}
	if run.Status == RunStatusSucceeded {
		if run.Failure != nil {
			return fmt.Errorf("succeeded run must not contain failure")
		}
		deterministic := run.FindingSetRef != nil && run.JSONReportRef != nil &&
			run.MarkdownReportRef != nil && run.HypothesisSetRef == nil &&
			run.RawCandidateCollectionRef == nil &&
			run.AgentTaskEvidenceRef == nil && run.AgentExecutionReceiptRef == nil &&
			run.CandidateSetRef == nil && run.VerificationLedgerRef == nil &&
			run.CalibrationLedgerRef == nil && run.SuppressionLedgerRef == nil &&
			run.GovernedReportRef == nil && run.GovernedMarkdownRef == nil
		formal := run.FindingSetRef == nil && run.JSONReportRef == nil &&
			run.MarkdownReportRef == nil && run.HypothesisSetRef != nil &&
			run.CandidateSetRef != nil && run.VerificationLedgerRef != nil &&
			run.CalibrationLedgerRef != nil && run.SuppressionLedgerRef != nil &&
			run.GovernedReportRef != nil &&
			run.GovernedMarkdownRef != nil
		if !deterministic && !formal {
			return fmt.Errorf(
				"succeeded run requires exactly one deterministic or formal report artifact family",
			)
		}
	} else if run.RawCandidateCollectionRef != nil || run.AgentTaskEvidenceRef != nil || run.AgentExecutionReceiptRef != nil ||
		run.CandidateSetRef != nil || run.VerificationLedgerRef != nil ||
		run.CalibrationLedgerRef != nil || run.SuppressionLedgerRef != nil ||
		run.GovernedReportRef != nil || run.GovernedMarkdownRef != nil {
		return fmt.Errorf("only a succeeded formal run may reference agent evidence, candidates, or reports")
	}
	if run.Status == RunStatusFailed && run.Failure == nil {
		return fmt.Errorf("failed run requires failure")
	}
	if run.ExecutionSnapshotID == "" {
		return fmt.Errorf("execution_snapshot_id is required")
	}
	if err := run.TargetSnapshotRef.Validate(); err != nil {
		return fmt.Errorf("target_snapshot_ref: %w", err)
	}
	if run.TargetSnapshotRef.Contract != ContractMaterializedTarget {
		return fmt.Errorf("target_snapshot_ref contract is %q, want %q",
			run.TargetSnapshotRef.Contract, ContractMaterializedTarget)
	}
	if run.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	attemptsByBinding := make(map[string]StageAttempt, len(run.StageAttempts))
	attemptCoordinates := make(map[string]struct{}, len(run.StageAttempts))
	for index, attempt := range run.StageAttempts {
		if err := attempt.Validate(); err != nil {
			return fmt.Errorf("stage_attempts[%d]: %w", index, err)
		}
		if _, duplicate := attemptsByBinding[attempt.BindingID]; duplicate {
			return fmt.Errorf("stage_attempts contains duplicate binding_id %q", attempt.BindingID)
		}
		if (run.Status == RunStatusSucceeded || run.Status == RunStatusFailed ||
			run.Status == RunStatusCanceled) &&
			(attempt.Status == StageStatusPending || attempt.Status == StageStatusRunning) {
			return fmt.Errorf("terminal run contains non-terminal stage attempt %q", attempt.StageID)
		}
		coordinate := fmt.Sprintf("%s:%d:%d", attempt.StageID, attempt.Attempt, attempt.Generation)
		if _, duplicate := attemptCoordinates[coordinate]; duplicate {
			return fmt.Errorf("stage_attempts contains duplicate stage attempt %q", coordinate)
		}
		attemptCoordinates[coordinate] = struct{}{}
		attemptsByBinding[attempt.BindingID] = attempt
	}
	bindingsByID := make(map[string]PlatformExecutionBinding, len(run.Bindings))
	for index, binding := range run.Bindings {
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("platform_execution_bindings[%d]: %w", index, err)
		}
		if binding.RunID != run.RunID {
			return fmt.Errorf("platform_execution_bindings[%d] belongs to run %q, want %q",
				index, binding.RunID, run.RunID)
		}
		if _, duplicate := bindingsByID[binding.BindingID]; duplicate {
			return fmt.Errorf("platform_execution_bindings contains duplicate binding_id %q",
				binding.BindingID)
		}
		attempt, exists := attemptsByBinding[binding.BindingID]
		if !exists {
			return fmt.Errorf("platform_execution_bindings[%d] references unknown stage attempt %q",
				index, binding.BindingID)
		}
		if binding.StageID != attempt.StageID || binding.Attempt != attempt.Attempt ||
			binding.Generation != attempt.Generation {
			return fmt.Errorf("platform_execution_bindings[%d] does not match exact stage attempt", index)
		}
		bindingsByID[binding.BindingID] = binding
	}
	if len(run.Bindings) != len(run.StageAttempts) {
		return fmt.Errorf("every stage attempt requires exactly one platform execution binding")
	}
	for bindingID := range attemptsByBinding {
		if _, exists := bindingsByID[bindingID]; !exists {
			return fmt.Errorf("stage attempt %q is missing a platform execution binding", bindingID)
		}
	}
	evidenceIDs := make(map[string]struct{}, len(run.Evidence))
	evidenceByBinding := make(map[string]RunEvidence, len(run.Evidence))
	for index, evidence := range run.Evidence {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("run_evidence[%d]: %w", index, err)
		}
		if _, duplicate := evidenceIDs[evidence.EvidenceID]; duplicate {
			return fmt.Errorf("run_evidence contains duplicate evidence_id %q", evidence.EvidenceID)
		}
		evidenceIDs[evidence.EvidenceID] = struct{}{}
		if _, duplicate := evidenceByBinding[evidence.BindingID]; duplicate {
			return fmt.Errorf("run_evidence contains duplicate binding %q", evidence.BindingID)
		}
		attempt, exists := attemptsByBinding[evidence.BindingID]
		if !exists {
			return fmt.Errorf("run_evidence[%d] references unknown binding %q", index, evidence.BindingID)
		}
		if evidence.StageID != attempt.StageID || evidence.Attempt != attempt.Attempt ||
			evidence.Generation != attempt.Generation {
			return fmt.Errorf("run_evidence[%d] does not match exact stage attempt", index)
		}
		if attempt.Status != StageStatusSucceeded {
			return fmt.Errorf("run_evidence[%d] belongs to a non-succeeded stage attempt", index)
		}
		binding := bindingsByID[evidence.BindingID]
		if evidence.IdempotencyKey != binding.IdempotencyKey ||
			evidence.FencingToken != binding.FencingToken {
			return fmt.Errorf("run_evidence[%d] does not match binding idempotency and fencing", index)
		}
		if attempt.OutputRef == nil || *attempt.OutputRef != evidence.ArtifactRef {
			return fmt.Errorf("run_evidence[%d] does not reference the exact stage output", index)
		}
		evidenceByBinding[evidence.BindingID] = evidence
	}
	for _, attempt := range run.StageAttempts {
		if attempt.Status == StageStatusSucceeded {
			if _, exists := evidenceByBinding[attempt.BindingID]; !exists {
				return fmt.Errorf("succeeded stage attempt %q is missing run evidence", attempt.BindingID)
			}
		}
	}
	if run.Status == RunStatusSucceeded {
		pureFindingGovernanceReplay := run.Kind == RunKindReplay &&
			run.ReplayVariable.IsFilterPolicy() &&
			run.ReplayFromStage == "finding_governance"
		if len(run.StageAttempts) == 0 && !pureFindingGovernanceReplay {
			return fmt.Errorf("succeeded run requires stage attempts")
		}
		latestByStage := make(map[string]StageAttempt)
		for _, attempt := range run.StageAttempts {
			latest, exists := latestByStage[attempt.StageID]
			if !exists || attempt.Generation > latest.Generation ||
				(attempt.Generation == latest.Generation && attempt.Attempt > latest.Attempt) {
				latestByStage[attempt.StageID] = attempt
			}
		}
		for stageID, attempt := range latestByStage {
			if attempt.Status != StageStatusSucceeded {
				return fmt.Errorf("succeeded run latest attempt for stage %q is %q",
					stageID, attempt.Status)
			}
		}
	}
	if run.FindingSetRef != nil {
		if err := run.FindingSetRef.Validate(); err != nil {
			return fmt.Errorf("finding_set_ref: %w", err)
		}
		if run.FindingSetRef.Contract != ContractFindingSet {
			return fmt.Errorf("finding_set_ref contract is %q, want %q",
				run.FindingSetRef.Contract, ContractFindingSet)
		}
	}
	if run.JSONReportRef != nil {
		if err := run.JSONReportRef.Validate(); err != nil {
			return fmt.Errorf("json_report_ref: %w", err)
		}
		if run.JSONReportRef.Contract != ContractJSONReport {
			return fmt.Errorf("json_report_ref contract is %q, want %q",
				run.JSONReportRef.Contract, ContractJSONReport)
		}
	}
	if run.MarkdownReportRef != nil {
		if err := run.MarkdownReportRef.Validate(); err != nil {
			return fmt.Errorf("markdown_report_ref: %w", err)
		}
		if run.MarkdownReportRef.Contract != ContractMarkdownReport {
			return fmt.Errorf("markdown_report_ref contract is %q, want %q",
				run.MarkdownReportRef.Contract, ContractMarkdownReport)
		}
	}
	for name, item := range map[string]struct {
		ref      *ArtifactRef
		contract string
	}{
		"hypothesis_set_ref":           {run.HypothesisSetRef, ContractReviewHypothesisSet},
		"raw_candidate_collection_ref": {run.RawCandidateCollectionRef, ContractAgentReviewRawCandidates},
		"agent_task_evidence_ref":      {run.AgentTaskEvidenceRef, ContractAgentReviewTaskEvidence},
		"agent_execution_receipt_ref":  {run.AgentExecutionReceiptRef, ContractAgentExecutionReceipts},
		"candidate_set_ref":            {run.CandidateSetRef, ContractGovernedCandidateSet},
		"verification_ledger_ref":      {run.VerificationLedgerRef, ContractCandidateVerificationLedger},
		"calibration_ledger_ref":       {run.CalibrationLedgerRef, ContractFindingCalibrationLedger},
		"suppression_ledger_ref":       {run.SuppressionLedgerRef, ContractFindingSuppressionLedger},
		"governed_report_ref":          {run.GovernedReportRef, ContractGovernedReviewReport},
		"governed_markdown_ref":        {run.GovernedMarkdownRef, ContractGovernedReviewMarkdown},
	} {
		if item.ref == nil {
			continue
		}
		if err := item.ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if item.ref.Contract != item.contract {
			return fmt.Errorf("%s contract is %q, want %q", name, item.ref.Contract, item.contract)
		}
	}
	return nil
}

func (change ReplayChangeSet) Validate() error {
	if change.SchemaVersion != ReplayChangeSetSchemaVersion {
		return fmt.Errorf("unsupported replay change set schema %q", change.SchemaVersion)
	}
	for name, value := range map[string]string{
		"namespace":     change.Namespace,
		"source_run_id": change.SourceRunID,
		"root_run_id":   change.RootRunID,
		"start_stage":   change.StartStage,
	} {
		if err := requireID(name, value); err != nil {
			return err
		}
	}
	if change.ParentReplayRunID != "" {
		if err := requireID("parent_replay_run_id", change.ParentReplayRunID); err != nil {
			return err
		}
		if change.ParentReplayRunID != change.SourceRunID {
			return fmt.Errorf("parent_replay_run_id must equal source_run_id")
		}
	}
	if err := change.Variable.Validate(); err != nil {
		return err
	}
	if err := requireSHA256("baseline_sha256", change.BaselineSHA256); err != nil {
		return err
	}
	if err := requireSHA256("variant_sha256", change.VariantSHA256); err != nil {
		return err
	}
	if change.ChangedFields == nil {
		return fmt.Errorf("changed_fields must be an explicit array")
	}
	for index, field := range change.ChangedFields {
		if err := requireID("changed_fields", field); err != nil {
			return err
		}
		if index > 0 && field <= change.ChangedFields[index-1] {
			return fmt.Errorf("changed_fields must be sorted and unique")
		}
	}
	if change.Variable == ReplayVariableNone {
		if change.BaselineSHA256 != change.VariantSHA256 ||
			len(change.ChangedFields) != 0 {
			return fmt.Errorf("exact replay must have identical digests and no changed fields")
		}
	} else if change.BaselineSHA256 == change.VariantSHA256 ||
		len(change.ChangedFields) == 0 {
		return fmt.Errorf("variant replay requires one changed variable with explicit fields")
	}
	if change.RemoteWrites != "deny" {
		return fmt.Errorf("replay change set must deny remote writes")
	}
	if change.CreatedAt.IsZero() || change.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("replay change set requires a UTC created_at")
	}
	return nil
}

func (variable ReplayVariable) Validate() error {
	switch variable {
	case ReplayVariableNone, ReplayVariableRulePack, ReplayVariableSkillPack, ReplayVariableKnowledgePack, ReplayVariablePrompt,
		ReplayVariableModel, ReplayVariableIndex, ReplayVariableWorkflow,
		ReplayVariableFilterPolicy, ReplayVariableFindingGovernance, ReplayVariableBudget:
		return nil
	default:
		return fmt.Errorf("unsupported replay variable %q", variable)
	}
}

func (attempt StageAttempt) Validate() error {
	if err := requireID("stage_id", attempt.StageID); err != nil {
		return err
	}
	if attempt.Attempt < 1 || attempt.Generation < 1 {
		return fmt.Errorf("attempt and generation must be positive")
	}
	if err := requireID("binding_id", attempt.BindingID); err != nil {
		return err
	}
	switch attempt.Status {
	case StageStatusPending, StageStatusRunning, StageStatusSucceeded, StageStatusFailed, StageStatusCanceled:
	default:
		return fmt.Errorf("unsupported stage status %q", attempt.Status)
	}
	if len(attempt.InputRefs) == 0 {
		return fmt.Errorf("stage attempt requires at least one input_ref")
	}
	for index, ref := range attempt.InputRefs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("input_refs[%d]: %w", index, err)
		}
		switch ref.Contract {
		case ContractReviewInput, ContractStageResult, ContractStageExecutionRequest:
		default:
			return fmt.Errorf("input_refs[%d] has unsupported contract %q", index, ref.Contract)
		}
	}
	if attempt.OutputRef != nil {
		if err := attempt.OutputRef.Validate(); err != nil {
			return fmt.Errorf("output_ref: %w", err)
		}
		if attempt.OutputRef.Contract != ContractStageResult &&
			attempt.OutputRef.Contract != ContractReviewHypothesisSet {
			return fmt.Errorf("output_ref contract %q is unsupported", attempt.OutputRef.Contract)
		}
	}
	if attempt.Status == StageStatusSucceeded && attempt.OutputRef == nil {
		return fmt.Errorf("succeeded stage attempt requires output_ref")
	}
	if attempt.Status == StageStatusRunning && attempt.StartedAt.IsZero() {
		return fmt.Errorf("running stage attempt requires started_at")
	}
	if attempt.Status == StageStatusSucceeded || attempt.Status == StageStatusFailed ||
		attempt.Status == StageStatusCanceled {
		if attempt.StartedAt.IsZero() || attempt.FinishedAt == nil || attempt.FinishedAt.IsZero() {
			return fmt.Errorf("terminal stage attempt requires start and finish timestamps")
		}
	}
	return nil
}

func (binding PlatformExecutionBinding) Validate() error {
	for name, value := range map[string]string{
		"binding_id":      binding.BindingID,
		"run_id":          binding.RunID,
		"stage_id":        binding.StageID,
		"idempotency_key": binding.IdempotencyKey,
		"runtime_kind":    binding.RuntimeKind,
		"runtime_id":      binding.RuntimeID,
	} {
		if err := requireID(name, value); err != nil {
			return err
		}
	}
	if binding.Attempt < 1 || binding.Generation < 1 {
		return fmt.Errorf("attempt and generation must be positive")
	}
	if binding.FencingToken == 0 {
		return fmt.Errorf("fencing_token must be positive")
	}
	if binding.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func (evidence RunEvidence) Validate() error {
	for name, value := range map[string]string{
		"evidence_id":     evidence.EvidenceID,
		"binding_id":      evidence.BindingID,
		"stage_id":        evidence.StageID,
		"idempotency_key": evidence.IdempotencyKey,
	} {
		if err := requireID(name, value); err != nil {
			return err
		}
	}
	if evidence.Attempt < 1 || evidence.Generation < 1 {
		return fmt.Errorf("attempt and generation must be positive")
	}
	if evidence.FencingToken == 0 {
		return fmt.Errorf("fencing_token must be positive")
	}
	if err := evidence.ArtifactRef.Validate(); err != nil {
		return err
	}
	switch evidence.Completeness {
	case CompletenessComplete:
		if evidence.CompletenessNotes == nil || len(evidence.CompletenessNotes) != 0 {
			return fmt.Errorf("complete evidence requires an explicit empty reasons array")
		}
	case CompletenessPartial:
		if len(evidence.CompletenessNotes) == 0 {
			return fmt.Errorf("partial evidence requires at least one reason")
		}
	default:
		return fmt.Errorf("unsupported completeness %q", evidence.Completeness)
	}
	if evidence.RecordedAt.IsZero() {
		return fmt.Errorf("recorded_at is required")
	}
	return nil
}

func (ref ArtifactRef) Validate() error {
	if err := requireSHA256("sha256", ref.SHA256); err != nil {
		return err
	}
	wantURI := "artifact://local/sha256/" + ref.SHA256
	if ref.URI != wantURI {
		return fmt.Errorf("artifact URI %q does not match digest; want %q", ref.URI, wantURI)
	}
	if ref.SizeBytes < 0 || ref.Contract == "" {
		return fmt.Errorf("artifact size and contract are invalid")
	}
	return nil
}

func DigestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validateWorkflowRef(ref WorkflowRef) error {
	if err := requireID("workflow.id", ref.ID); err != nil {
		return err
	}
	if err := requireID("workflow.revision", ref.Revision); err != nil {
		return err
	}
	return requireSHA256("workflow.sha256", ref.SHA256)
}

func validatePolicyRef(ref PolicyRef) error {
	if err := requireID("config.id", ref.ID); err != nil {
		return err
	}
	if err := requireID("config.revision", ref.Revision); err != nil {
		return err
	}
	return requireSHA256("config.sha256", ref.SHA256)
}

func requireID(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	return nil
}

func requireSHA256(name, value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	return nil
}
