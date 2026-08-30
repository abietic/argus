package reviewcore

import (
	"encoding/json"
)

const (
	ReviewInputSchemaVersion = "argus.review_input.v1alpha1"
	ArtifactSchemaVersion    = "argus.reviewcore_artifact.v1alpha1"
	ReportSchemaVersion      = "argus.review_report.v1alpha1"
)

type TargetMode string

const (
	TargetModeDiff      TargetMode = "diff"
	TargetModeSelection TargetMode = "selection"
	TargetModeScope     TargetMode = "scope"
)

type StageName string

const (
	StageMaterializeTarget StageName = "materialize_target"
	StagePlanContext       StageName = "plan_context"
	StageDetect            StageName = "detect"
	StageNormalize         StageName = "normalize"
	StageVerify            StageName = "verify"
	StageAdjudicate        StageName = "adjudicate"
	StageReport            StageName = "report"
	StagePublish           StageName = "publish"
	StageCaptureFeedback   StageName = "capture_feedback"
	StageExportEvaluation  StageName = "export_evaluation"
)

type LifecycleState string

const (
	LifecycleComplete         LifecycleState = "complete"
	LifecyclePartial          LifecycleState = "partial"
	LifecycleRemoteDisabled   LifecycleState = "remote_disabled"
	LifecycleAwaitingFeedback LifecycleState = "awaiting_feedback"
	LifecycleCandidateOnly    LifecycleState = "candidate_only"
)

// LifecycleFact makes non-detector workflow boundaries explicit without
// claiming that a remote publication, user feedback, or governed evaluation
// label has already occurred.
type LifecycleFact struct {
	Kind        string         `json:"kind"`
	State       LifecycleState `json:"state"`
	ReasonCodes []string       `json:"reason_codes"`
	RecordCount uint32         `json:"record_count"`
}

type EvidenceKind string

const (
	EvidencePatchLine   EvidenceKind = "patch_line"
	EvidenceTargetLine  EvidenceKind = "target_line"
	EvidenceFileContent EvidenceKind = "file_content"
)

type DetectionSignalKind string

const (
	DetectionSignalLexicalMarker DetectionSignalKind = "lexical_marker"
	DetectionSignalGoASTPattern  DetectionSignalKind = "go_ast_pattern"
)

type DetectionGapReason string

const (
	DetectionGapContentUnavailable DetectionGapReason = "content_unavailable"
	DetectionGapGoParseFailed      DetectionGapReason = "go_parse_failed"
)

type VerificationStatus string

const (
	VerificationPending      VerificationStatus = "pending"
	VerificationVerified     VerificationStatus = "verified"
	VerificationRejected     VerificationStatus = "rejected"
	VerificationInconclusive VerificationStatus = "inconclusive"
)

type DecisionAction string

const (
	DecisionPublish     DecisionAction = "publish"
	DecisionReject      DecisionAction = "reject"
	DecisionHumanReview DecisionAction = "human_review"
)

type LineageDisposition string

const (
	LineageWinner    LineageDisposition = "winner"
	LineageDuplicate LineageDisposition = "duplicate"
)

// ReviewInput is the complete, immutable input for the deterministic baseline.
// Content is carried by value so stages never perform filesystem or network IO.
type ReviewInput struct {
	SchemaVersion  string              `json:"schema_version"`
	TargetID       string              `json:"target_id"`
	TargetMode     TargetMode          `json:"target_mode"`
	CanonicalPatch string              `json:"canonical_patch"`
	Regions        []ReviewRegion      `json:"regions"`
	Files          []FileManifestEntry `json:"files"`
	Contexts       []ContextBinding    `json:"contexts"`
}

// ReviewRegion is an immutable authorization boundary inside a frozen file.
// Diff input derives its authorized lines from CanonicalPatch and therefore
// carries an explicit empty Regions array. Selection and scope input carry
// their exact line ranges here.
type ReviewRegion struct {
	Path      string `json:"path"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
	SHA256    string `json:"sha256"`
}

// FileManifestEntry binds final-file content to its digest. Content may be nil
// when only a manifest is available; verification then reports inconclusive
// rather than silently trusting patch-only evidence.
type FileManifestEntry struct {
	Path      string  `json:"path"`
	SHA256    string  `json:"sha256"`
	SizeBytes int64   `json:"size_bytes"`
	Content   *string `json:"content"`
}

// ContextBinding is a closed union for dynamic inputs outside the authorized
// review target. A successful capture is a ContextRef; an unavailable or
// policy-denied capture is retained as a ContextGap. Context coverage never
// expands ReviewInput.Regions and therefore cannot create a publishable anchor.
type ContextBinding struct {
	Ref *ContextRef `json:"ref,omitempty"`
	Gap *ContextGap `json:"gap,omitempty"`
}

type ContextRef struct {
	ContextID   string            `json:"context_id"`
	Kind        string            `json:"kind"`
	Revision    string            `json:"revision"`
	Digest      string            `json:"digest"`
	Coverage    ContextCoverage   `json:"coverage"`
	Provenance  ContextProvenance `json:"provenance"`
	ArtifactURI string            `json:"artifact_uri"`
	Contract    string            `json:"contract"`
	SizeBytes   int64             `json:"size_bytes"`
}

type ContextGap struct {
	ContextID  string            `json:"context_id"`
	Kind       string            `json:"kind"`
	Revision   string            `json:"revision"`
	Digest     string            `json:"digest"`
	Coverage   ContextCoverage   `json:"coverage"`
	Provenance ContextProvenance `json:"provenance"`
	ReasonCode string            `json:"reason_code"`
}

type ContextCoverage struct {
	Spans   []ContextSpan `json:"spans"`
	Symbols []string      `json:"symbols"`
}

type ContextSpan struct {
	Path      string `json:"path"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
}

type ContextProvenance struct {
	Provider         string `json:"provider"`
	ProducerID       string `json:"producer_id"`
	ProducerRevision string `json:"producer_revision"`
}

type Evidence struct {
	ID           string       `json:"id"`
	Kind         EvidenceKind `json:"kind"`
	SourceDigest string       `json:"source_digest"`
	Path         string       `json:"path"`
	Line         uint32       `json:"line"`
	Excerpt      string       `json:"excerpt"`
	Claim        string       `json:"claim"`
}

type CandidateFinding struct {
	ID               string              `json:"id"`
	DetectorID       string              `json:"detector_id"`
	DetectorRevision string              `json:"detector_revision"`
	RuleID           string              `json:"rule_id"`
	TargetDigest     string              `json:"target_digest"`
	Path             string              `json:"path"`
	StartLine        uint32              `json:"start_line"`
	EndLine          uint32              `json:"end_line"`
	SignalKind       DetectionSignalKind `json:"signal_kind"`
	Signal           string              `json:"signal"`
	SignalOffset     uint32              `json:"signal_offset"`
	Excerpt          string              `json:"excerpt"`
	Evidence         []Evidence          `json:"evidence"`
}

// DetectionGap records why a configured detector could not make a sound
// decision for an authorized target line. It is immutable evidence of partial
// detector coverage, not a synthetic finding.
type DetectionGap struct {
	ID               string             `json:"id"`
	DetectorID       string             `json:"detector_id"`
	DetectorRevision string             `json:"detector_revision"`
	RuleID           string             `json:"rule_id"`
	TargetDigest     string             `json:"target_digest"`
	SourceDigest     string             `json:"source_digest"`
	Path             string             `json:"path"`
	Line             uint32             `json:"line"`
	ReasonCode       DetectionGapReason `json:"reason_code"`
}

type CandidateLineage struct {
	CandidateID string             `json:"candidate_id"`
	Disposition LineageDisposition `json:"disposition"`
	ReasonCode  string             `json:"reason_code"`
}

type Verification struct {
	Status      VerificationStatus `json:"status"`
	ReasonCode  string             `json:"reason_code"`
	EvidenceIDs []string           `json:"evidence_ids"`
}

type Finding struct {
	ID           string             `json:"id"`
	Fingerprint  string             `json:"fingerprint"`
	Anchor       string             `json:"anchor"`
	RuleID       string             `json:"rule_id"`
	Title        string             `json:"title"`
	Severity     string             `json:"severity"`
	Message      string             `json:"message"`
	TargetDigest string             `json:"target_digest"`
	Path         string             `json:"path"`
	StartLine    uint32             `json:"start_line"`
	EndLine      uint32             `json:"end_line"`
	Excerpt      string             `json:"excerpt"`
	Signals      []string           `json:"signals"`
	Evidence     []Evidence         `json:"evidence"`
	Lineage      []CandidateLineage `json:"lineage"`
	Verification Verification       `json:"verification"`
}

// FindingDecision is an append-only adjudication fact. It intentionally has
// no feedback or outcome fields; those are separate domain facts.
type FindingDecision struct {
	ID              string         `json:"id"`
	FindingID       string         `json:"finding_id"`
	Sequence        uint32         `json:"sequence"`
	PriorDecisionID string         `json:"prior_decision_id"`
	Action          DecisionAction `json:"action"`
	ReasonCode      string         `json:"reason_code"`
	EvidenceIDs     []string       `json:"evidence_ids"`
}

type ReportSummary struct {
	Findings     uint32 `json:"findings"`
	Verified     uint32 `json:"verified"`
	Rejected     uint32 `json:"rejected"`
	Inconclusive uint32 `json:"inconclusive"`
	Publish      uint32 `json:"publish"`
	HumanReview  uint32 `json:"human_review"`
}

type Report struct {
	SchemaVersion string            `json:"schema_version"`
	TargetDigest  string            `json:"target_digest"`
	Summary       ReportSummary     `json:"summary"`
	Findings      []Finding         `json:"findings"`
	Decisions     []FindingDecision `json:"decisions"`
}

// StageOutput is a strict discriminated payload. Irrelevant fields must be
// null; the field(s) owned by a stage must be explicit arrays, even when empty.
type StageOutput struct {
	CandidateFindings []CandidateFinding `json:"candidate_findings"`
	DetectionGaps     []DetectionGap     `json:"detection_gaps"`
	Findings          []Finding          `json:"findings"`
	Decisions         []FindingDecision  `json:"decisions"`
	Report            *Report            `json:"report"`
	Lifecycle         *LifecycleFact     `json:"lifecycle"`
}

type StageResult struct {
	SchemaVersion  string      `json:"schema_version"`
	Stage          StageName   `json:"stage"`
	StageRevision  string      `json:"stage_revision"`
	TargetDigest   string      `json:"target_digest"`
	InputDigest    string      `json:"input_digest"`
	ArtifactDigest string      `json:"artifact_digest"`
	Output         StageOutput `json:"output"`
}

// DetectShardResult binds one independently executed scope input to its exact
// detect result. MergeDetectShardResults re-keys shard-local identities to the
// immutable full-scope target before downstream normalization.
type DetectShardResult struct {
	Input  ReviewInput
	Result StageResult
}

// RunOptions starts execution at StartAt and reuses the exact prefix in
// Upstream. For example, StartAt=verify requires detect and normalize results.
type RunOptions struct {
	StartAt  StageName
	Upstream []StageResult
}

type RunResult struct {
	InputDigest    string          `json:"input_digest"`
	Stages         []StageResult   `json:"stages"`
	ReusedStages   []StageName     `json:"reused_stages"`
	Report         Report          `json:"report"`
	ReportJSON     json.RawMessage `json:"report_json"`
	ReportMarkdown string          `json:"report_markdown"`
}
