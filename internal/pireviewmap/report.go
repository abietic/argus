package pireviewmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const piReviewReportSchemaVersion = "argus.pi-review.v0"

// PiReviewReportSchemaVersion is exported only so the existing shadow fixture
// builders can keep constructing exact worker reports while the production
// decoder and mapper live in this package.
const PiReviewReportSchemaVersion = piReviewReportSchemaVersion

type piReviewReport struct {
	SchemaVersion          string                     `json:"schemaVersion"`
	Status                 string                     `json:"status"`
	Provider               string                     `json:"provider"`
	ProviderProfile        string                     `json:"providerProfile"`
	Model                  string                     `json:"model"`
	Target                 piTarget                   `json:"target"`
	Coverage               piCoverage                 `json:"coverage"`
	Summary                piSummary                  `json:"summary"`
	Findings               []piCandidate              `json:"findings"`
	Candidates             []piCandidate              `json:"candidates"`
	RawCandidates          []piRawCandidate           `json:"rawCandidates"`
	NormalizationDecisions []piNormalizationDecision  `json:"normalizationDecisions"`
	Execution              piExecutionEnvelope        `json:"execution"`
	verificationRawByID    map[string]json.RawMessage `json:"-"`
}

type piTarget struct {
	Kind                 string         `json:"kind"`
	Repository           string         `json:"repository"`
	BaseOID              *string        `json:"baseOid,omitempty"`
	HeadOID              *string        `json:"headOid,omitempty"`
	Digest               string         `json:"digest"`
	CapturedAt           time.Time      `json:"capturedAt"`
	GeneratedAt          *time.Time     `json:"generatedAt,omitempty"`
	CanonicalPatchDigest *string        `json:"canonicalPatchDigest,omitempty"`
	Files                []piTargetFile `json:"files"`
	Skipped              []piSkipped    `json:"skipped"`
}

type piTargetFile struct {
	Path         string  `json:"path"`
	OldPath      *string `json:"oldPath,omitempty"`
	Status       string  `json:"status"`
	Digest       string  `json:"digest"`
	ChangedLines uint32  `json:"changedLines"`
	TargetDigest *string `json:"targetDigest,omitempty"`
	PatchDigest  *string `json:"patchDigest,omitempty"`
}

type piSkipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type piCoverage struct {
	GroupsTotal                uint32      `json:"groupsTotal"`
	GroupsReviewed             uint32      `json:"groupsReviewed"`
	ReviewTasksTotal           uint32      `json:"reviewTasksTotal"`
	ReviewTasksSucceeded       uint32      `json:"reviewTasksSucceeded"`
	VerificationEnabled        bool        `json:"verificationEnabled"`
	VerificationTasksTotal     uint32      `json:"verificationTasksTotal"`
	VerificationTasksSucceeded uint32      `json:"verificationTasksSucceeded"`
	FilesIncluded              uint32      `json:"filesIncluded"`
	Skipped                    []piSkipped `json:"skipped"`
	ContextGaps                []string    `json:"contextGaps"`
	Failures                   []piFailure `json:"failures"`
	StaleTarget                bool        `json:"staleTarget"`
}

type piFailure struct {
	GroupID     string  `json:"groupId"`
	Phase       string  `json:"phase"`
	SkillID     *string `json:"skillId,omitempty"`
	CandidateID *string `json:"candidateId,omitempty"`
	Error       string  `json:"error"`
}

type piSummary struct {
	Candidates   uint32 `json:"candidates"`
	Confirmed    uint32 `json:"confirmed"`
	Rejected     uint32 `json:"rejected"`
	Inconclusive uint32 `json:"inconclusive"`
}

type piSourceAnchor struct {
	Path      string `json:"path"`
	Side      string `json:"side"`
	StartLine uint32 `json:"startLine"`
	EndLine   uint32 `json:"endLine"`
}

type piEvidence struct {
	Statement string         `json:"statement"`
	Anchor    piSourceAnchor `json:"anchor"`
	Excerpt   string         `json:"excerpt"`
}

type piCandidateClaim struct {
	Category         string         `json:"category"`
	Severity         string         `json:"severity"`
	RawConfidencePPM *uint32        `json:"rawConfidencePPM,omitempty"`
	Title            string         `json:"title"`
	Description      string         `json:"description"`
	Impact           string         `json:"impact"`
	Anchor           piSourceAnchor `json:"anchor"`
	Evidence         []piEvidence   `json:"evidence"`
	Suggestion       *string        `json:"suggestion,omitempty"`
}

type piSkillRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

type piRawCandidate struct {
	RawCandidateID string           `json:"rawCandidateId"`
	GroupID        string           `json:"groupId"`
	Skill          piSkillRef       `json:"skill"`
	Ordinal        uint32           `json:"ordinal"`
	Claim          piCandidateClaim `json:"claim"`
}

type piVerification struct {
	CandidateID string       `json:"candidateId"`
	Verdict     string       `json:"verdict"`
	ReasonCode  string       `json:"reasonCode"`
	Explanation string       `json:"explanation"`
	Evidence    []piEvidence `json:"evidence"`
}

type piCandidate struct {
	PiCandidateClaim
	ID           string          `json:"id"`
	Fingerprint  string          `json:"fingerprint"`
	GroupID      string          `json:"groupId"`
	Skill        piSkillRef      `json:"skill"`
	Verification *piVerification `json:"verification,omitempty"`
}

type piNormalizationDecision struct {
	RawCandidateID        string  `json:"rawCandidateId"`
	Action                string  `json:"action"`
	ReasonCode            string  `json:"reasonCode"`
	NormalizedCandidateID *string `json:"normalizedCandidateId,omitempty"`
}

type piExecutionEnvelope struct {
	Authority       string                   `json:"authority"`
	ProvenanceClass string                   `json:"provenanceClass"`
	Snapshot        piExecutionSnapshot      `json:"snapshot"`
	Tasks           []piTaskObservation      `json:"tasks"`
	TaskEvidence    piTaskEvidenceCollection `json:"taskEvidence"`
	Usage           piUsage                  `json:"usage"`
}

type piTaskEvidenceCollection struct {
	SchemaVersion   string                    `json:"schemaVersion"`
	Authority       string                    `json:"authority"`
	ProvenanceClass string                    `json:"provenanceClass"`
	ContentPolicy   string                    `json:"contentPolicy"`
	Completeness    string                    `json:"completeness"`
	ReasonCodes     []string                  `json:"reasonCodes"`
	Tasks           []piTaskExecutionEvidence `json:"tasks"`
}

type piTaskExecutionEvidence struct {
	TaskID         string                 `json:"taskId"`
	TaskKind       string                 `json:"taskKind"`
	GroupID        string                 `json:"groupId"`
	SkillID        *string                `json:"skillId,omitempty"`
	CandidateID    *string                `json:"candidateId,omitempty"`
	SystemPrompt   piTaskEvidenceContent  `json:"systemPrompt"`
	UserPrompt     piTaskEvidenceContent  `json:"userPrompt"`
	Output         *piTaskEvidenceContent `json:"output,omitempty"`
	TerminalStatus string                 `json:"terminalStatus"`
	Tools          []piTaskToolEvidence   `json:"tools"`
}

type piTaskToolEvidence struct {
	Sequence  uint32                 `json:"sequence"`
	ToolName  string                 `json:"toolName"`
	Arguments piTaskEvidenceContent  `json:"arguments"`
	Result    *piTaskEvidenceContent `json:"result,omitempty"`
	IsError   *bool                  `json:"isError,omitempty"`
}

type piTaskEvidenceContent struct {
	SHA256         string  `json:"sha256"`
	SizeBytes      uint64  `json:"sizeBytes"`
	Content        *string `json:"content,omitempty"`
	OmissionReason *string `json:"omissionReason,omitempty"`
}

type piExecutionSnapshot struct {
	SchemaVersion        string                `json:"schemaVersion"`
	SnapshotDigest       string                `json:"snapshotDigest"`
	CreatedAt            time.Time             `json:"createdAt"`
	Target               piSnapshotTarget      `json:"target"`
	WorkflowRevision     string                `json:"workflowRevision"`
	PromptBundleRevision string                `json:"promptBundleRevision"`
	PromptBundleDigest   string                `json:"promptBundleDigest"`
	Grouping             piSnapshotGrouping    `json:"grouping"`
	Skills               []piSnapshotSkill     `json:"skills"`
	Knowledge            []piSnapshotKnowledge `json:"knowledge"`
	Runtime              piSnapshotRuntime     `json:"runtime"`
	Provider             piSnapshotProvider    `json:"provider"`
	ToolPolicy           piSnapshotToolPolicy  `json:"toolPolicy"`
	Budgets              piSnapshotBudgets     `json:"budgets"`
	VerificationPolicy   string                `json:"verificationPolicy"`
	Replayability        piReplayability       `json:"replayability"`
}

type piSnapshotTarget struct {
	Kind    string  `json:"kind"`
	Digest  string  `json:"digest"`
	BaseOID *string `json:"baseOid,omitempty"`
	HeadOID *string `json:"headOid,omitempty"`
}

type piSnapshotGrouping struct {
	ImplementationRevision string            `json:"implementationRevision"`
	Groups                 []piSnapshotGroup `json:"groups"`
}

type piSnapshotGroup struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	PatchDigest string   `json:"patchDigest"`
	Files       []string `json:"files"`
}

type piSnapshotSkill struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
	Bytes    uint64 `json:"bytes"`
}

type piSnapshotKnowledge struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Bytes  uint64 `json:"bytes"`
}

type piSnapshotRuntime struct {
	Implementation string `json:"implementation"`
	Version        string `json:"version"`
	Node           string `json:"node"`
	PiAgentCore    string `json:"piAgentCore"`
	PiAI           string `json:"piAi"`
}

type piSnapshotProvider struct {
	Provider       string  `json:"provider"`
	Profile        string  `json:"profile"`
	Protocol       string  `json:"protocol"`
	Model          string  `json:"model"`
	EndpointDigest *string `json:"endpointDigest,omitempty"`
	CredentialRef  *string `json:"credentialRef,omitempty"`
}

type piSnapshotToolPolicy struct {
	Mode         string   `json:"mode"`
	AllowedTools []string `json:"allowedTools"`
}

type piSnapshotBudgets struct {
	Concurrency            uint32 `json:"concurrency"`
	MaxToolCallsPerTask    uint32 `json:"maxToolCallsPerTask"`
	MaxFiles               uint32 `json:"maxFiles"`
	MaxGroups              uint32 `json:"maxGroups"`
	MaxGroupBytes          uint64 `json:"maxGroupBytes"`
	MaxTargetBytes         uint64 `json:"maxTargetBytes"`
	MaxCandidates          uint32 `json:"maxCandidates"`
	MaxProviderTurns       uint32 `json:"maxProviderTurns"`
	MaxOutputTokensPerTurn uint32 `json:"maxOutputTokensPerTurn"`
	TaskTimeoutMS          uint64 `json:"taskTimeoutMs"`
}

type piReplayability struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}

type piTaskObservation struct {
	TaskID                 string        `json:"taskId"`
	TaskKind               string        `json:"taskKind"`
	GroupID                string        `json:"groupId"`
	SkillID                *string       `json:"skillId,omitempty"`
	CandidateID            *string       `json:"candidateId,omitempty"`
	PromptDigest           string        `json:"promptDigest"`
	ProviderTurnsStarted   uint32        `json:"providerTurnsStarted"`
	ProviderTurnsCompleted uint32        `json:"providerTurnsCompleted"`
	ToolCalls              uint32        `json:"toolCalls"`
	ToolNames              []string      `json:"toolNames"`
	ToolUsage              []piToolUsage `json:"toolUsage"`
	Usage                  piUsage       `json:"usage"`
	StartedAt              time.Time     `json:"startedAt"`
	FinishedAt             time.Time     `json:"finishedAt"`
	DurationMS             uint64        `json:"durationMs"`
	TerminalStatus         string        `json:"terminalStatus"`
	ErrorCode              *string       `json:"errorCode,omitempty"`
	OutputDigest           *string       `json:"outputDigest,omitempty"`
}

type piToolUsage struct {
	ToolID          string `json:"toolId"`
	InvocationCount uint32 `json:"invocationCount"`
	FailureCount    uint32 `json:"failureCount"`
}

type piUsage struct {
	Completeness          string  `json:"completeness"`
	UnavailableReasonCode *string `json:"unavailableReasonCode,omitempty"`
	InputTokens           *uint64 `json:"inputTokens,omitempty"`
	OutputTokens          *uint64 `json:"outputTokens,omitempty"`
	CacheReadTokens       *uint64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens      *uint64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens       *uint64 `json:"reasoningTokens,omitempty"`
	TotalTokens           *uint64 `json:"totalTokens,omitempty"`
}

// The Pi wire types are aliases rather than a second public protocol. They are
// exposed for the existing same-repository adversarial fixtures; production
// callers should pass worker result bytes to MapShadowReviewReport or
// MapFormalReviewResult instead of constructing reports themselves.
type PiReviewReport = piReviewReport
type PiTarget = piTarget
type PiTargetFile = piTargetFile
type PiSkipped = piSkipped
type PiCoverage = piCoverage
type PiFailure = piFailure
type PiSummary = piSummary
type PiSourceAnchor = piSourceAnchor
type PiEvidence = piEvidence
type PiCandidateClaim = piCandidateClaim
type PiSkillRef = piSkillRef
type PiRawCandidate = piRawCandidate
type PiVerification = piVerification
type PiCandidate = piCandidate
type PiNormalizationDecision = piNormalizationDecision
type PiExecutionEnvelope = piExecutionEnvelope
type PiTaskEvidenceCollection = piTaskEvidenceCollection
type PiTaskExecutionEvidence = piTaskExecutionEvidence
type PiTaskToolEvidence = piTaskToolEvidence
type PiTaskEvidenceContent = piTaskEvidenceContent
type PiExecutionSnapshot = piExecutionSnapshot
type PiSnapshotTarget = piSnapshotTarget
type PiSnapshotGrouping = piSnapshotGrouping
type PiSnapshotGroup = piSnapshotGroup
type PiSnapshotSkill = piSnapshotSkill
type PiSnapshotKnowledge = piSnapshotKnowledge
type PiSnapshotRuntime = piSnapshotRuntime
type PiSnapshotProvider = piSnapshotProvider
type PiSnapshotToolPolicy = piSnapshotToolPolicy
type PiSnapshotBudgets = piSnapshotBudgets
type PiReplayability = piReplayability
type PiTaskObservation = piTaskObservation
type PiToolUsage = piToolUsage
type PiUsage = piUsage

func decodePiReviewReport(data []byte) (piReviewReport, error) {
	var report piReviewReport
	if err := decodeStrictAgentShadowJSON(data, &report); err != nil {
		return piReviewReport{}, fmt.Errorf("decode Pi review report: %w", err)
	}
	if report.SchemaVersion != piReviewReportSchemaVersion {
		return piReviewReport{}, fmt.Errorf(
			"unsupported Pi review report schema %q",
			report.SchemaVersion,
		)
	}
	// The verifier output digest is defined over the exact JSON value returned
	// by the model adapter. The worker embeds that same value in candidates, so
	// retain its raw bytes instead of re-encoding a Go struct and accidentally
	// accepting a forged digest with different JSON ordering.
	var raw struct {
		Candidates []struct {
			ID           string          `json:"id"`
			Verification json.RawMessage `json:"verification"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return piReviewReport{}, fmt.Errorf("retain raw Pi verifier outputs: %w", err)
	}
	report.verificationRawByID = make(map[string]json.RawMessage, len(raw.Candidates))
	for _, candidate := range raw.Candidates {
		if len(candidate.Verification) == 0 || bytes.Equal(candidate.Verification, []byte("null")) {
			continue
		}
		if _, duplicate := report.verificationRawByID[candidate.ID]; duplicate {
			return piReviewReport{}, fmt.Errorf("duplicate raw Pi candidate %q", candidate.ID)
		}
		report.verificationRawByID[candidate.ID] = bytes.Clone(candidate.Verification)
	}
	return report, nil
}

// DecodePiReviewReport is a strict fixture/debug decoder. Formal and shadow
// evidence admission should use the high-level mapping functions.
func DecodePiReviewReport(data []byte) (PiReviewReport, error) {
	return decodePiReviewReport(data)
}

func decodeStrictAgentShadowJSON(data []byte, output any) error {
	if output == nil {
		return fmt.Errorf("JSON output is required")
	}
	if err := inspectStrictAgentShadowJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func inspectStrictAgentShadowJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkStrictAgentShadowJSON(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func walkStrictAgentShadowJSON(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return fmt.Errorf("%s contains explicit JSON null", path)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s contains a non-string key", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%s contains duplicate field %q", path, key)
			}
			seen[key] = struct{}{}
			if err := walkStrictAgentShadowJSON(decoder, path+"."+key); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
	case '[':
		index := 0
		for decoder.More() {
			if err := walkStrictAgentShadowJSON(
				decoder,
				fmt.Sprintf("%s[%d]", path, index),
			); err != nil {
				return err
			}
			index++
		}
		_, err = decoder.Token()
	default:
		return fmt.Errorf("%s contains unexpected JSON delimiter %q", path, delimiter)
	}
	return err
}
