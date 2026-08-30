package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

const (
	AgentReviewTaskEvidenceCollectionSchemaVersion = "argus.agent_review_task_evidence_collection.v1alpha1"
	AgentReviewTaskEvidenceAuthority               = "diagnostic_only"
	AgentReviewTaskEvidenceProvenance              = "worker_self_report"
	AgentReviewTaskEvidenceDisposition             = "shadow_only"
	AgentReviewTaskEvidenceContentPolicy           = "exact_local_sensitive"

	AgentReviewTaskEvidenceComplete = "complete"
	AgentReviewTaskEvidencePartial  = "partial"

	AgentReviewTaskEvidenceBudgetExceeded = "evidence_budget_exceeded"
	AgentReviewTaskEvidenceUnavailable    = "task_evidence_unavailable"

	agentReviewMaxTaskEvidenceTasks = 4096
	// The aggregate import limit is lower and remains the primary memory bound.
	// This per-value limit prevents direct decoder users from accepting one
	// pathological string while still permitting a maximum-size local artifact.
	agentReviewMaxTaskEvidenceContentBytes = 16 << 20
)

// AgentReviewTaskEvidenceCollection contains exact local prompts, structured
// task outputs, and validated tool arguments/results when they fit the worker
// output budget. It is repository-source-sensitive, worker self-report and
// diagnostic-only: it is not a provider transcript, Hailix Trace, attestation,
// Finding, evaluation label, or training truth.
type AgentReviewTaskEvidenceCollection struct {
	SchemaVersion  string                             `json:"schema_version"`
	PlanID         string                             `json:"plan_id"`
	SourceRunID    string                             `json:"source_run_id"`
	ExecutionID    string                             `json:"execution_id"`
	ReviewRunID    string                             `json:"review_run_id"`
	TargetDigest   string                             `json:"target_digest"`
	Authority      string                             `json:"authority"`
	Provenance     string                             `json:"provenance_class"`
	Disposition    string                             `json:"disposition"`
	ContentPolicy  string                             `json:"content_policy"`
	Completeness   string                             `json:"completeness"`
	ReasonCodes    []string                           `json:"reason_codes"`
	TaskExecutions []AgentReviewTaskExecutionEvidence `json:"task_executions"`
}

type AgentReviewTaskExecutionEvidence struct {
	TaskID                 string                          `json:"task_id"`
	TaskRole               AgentTaskRole                   `json:"task_role"`
	GroupID                string                          `json:"group_id"`
	SkillID                *string                         `json:"skill_id,omitempty"`
	HypothesisOccurrenceID *string                         `json:"hypothesis_occurrence_id,omitempty"`
	SystemPrompt           AgentReviewTaskEvidenceContent  `json:"system_prompt"`
	UserPrompt             AgentReviewTaskEvidenceContent  `json:"user_prompt"`
	Output                 *AgentReviewTaskEvidenceContent `json:"output,omitempty"`
	TerminalStatus         AgentTaskStatus                 `json:"terminal_status"`
	Tools                  []AgentReviewTaskToolEvidence   `json:"tools"`
}

type AgentReviewTaskToolEvidence struct {
	Sequence  uint32                          `json:"sequence"`
	ToolName  string                          `json:"tool_name"`
	Arguments AgentReviewTaskEvidenceContent  `json:"arguments"`
	Result    *AgentReviewTaskEvidenceContent `json:"result,omitempty"`
	IsError   *bool                           `json:"is_error,omitempty"`
}

type AgentReviewTaskEvidenceContent struct {
	SHA256         string  `json:"sha256"`
	SizeBytes      uint64  `json:"size_bytes"`
	Content        *string `json:"content,omitempty"`
	OmissionReason *string `json:"omission_reason,omitempty"`
}

func DecodeAgentReviewTaskEvidenceCollection(data []byte) (AgentReviewTaskEvidenceCollection, error) {
	var value AgentReviewTaskEvidenceCollection
	if err := decodeAgentReviewShadowJSON(data, &value); err != nil {
		return AgentReviewTaskEvidenceCollection{}, fmt.Errorf("decode AgentReviewTaskEvidenceCollection: %w", err)
	}
	if err := value.Validate(); err != nil {
		return AgentReviewTaskEvidenceCollection{}, err
	}
	return value, nil
}

func (collection AgentReviewTaskEvidenceCollection) Validate() error {
	if collection.SchemaVersion != AgentReviewTaskEvidenceCollectionSchemaVersion {
		return fmt.Errorf("unsupported AgentReviewTaskEvidenceCollection schema %q", collection.SchemaVersion)
	}
	for name, value := range map[string]string{
		"plan_id": collection.PlanID, "source_run_id": collection.SourceRunID,
		"execution_id": collection.ExecutionID, "review_run_id": collection.ReviewRunID,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("target_digest", collection.TargetDigest); err != nil {
		return err
	}
	if collection.Authority != AgentReviewTaskEvidenceAuthority ||
		collection.Provenance != AgentReviewTaskEvidenceProvenance ||
		collection.Disposition != AgentReviewTaskEvidenceDisposition ||
		collection.ContentPolicy != AgentReviewTaskEvidenceContentPolicy {
		return fmt.Errorf("task evidence authority, provenance, disposition or content policy is unsupported")
	}
	if collection.TaskExecutions == nil || collection.ReasonCodes == nil {
		return fmt.Errorf("task_executions and reason_codes must be explicit arrays")
	}
	if len(collection.TaskExecutions) > agentReviewMaxTaskEvidenceTasks {
		return fmt.Errorf("task_executions exceeds %d entries", agentReviewMaxTaskEvidenceTasks)
	}
	if err := validateTaskEvidenceReasons(collection.Completeness, collection.ReasonCodes); err != nil {
		return err
	}
	previous := ""
	omitted := false
	for index, task := range collection.TaskExecutions {
		if err := task.validate(index); err != nil {
			return err
		}
		if index > 0 && task.TaskID <= previous {
			return fmt.Errorf("task_executions must be uniquely sorted by task_id")
		}
		previous = task.TaskID
		omitted = omitted || task.hasOmittedContent()
	}
	if collection.Completeness == AgentReviewTaskEvidenceComplete && omitted {
		return fmt.Errorf("complete task evidence forbids omitted content")
	}
	if omitted && !slices.Contains(collection.ReasonCodes, AgentReviewTaskEvidenceBudgetExceeded) {
		return fmt.Errorf("omitted content requires evidence_budget_exceeded")
	}
	return nil
}

func validateTaskEvidenceReasons(completeness string, reasons []string) error {
	if completeness != AgentReviewTaskEvidenceComplete && completeness != AgentReviewTaskEvidencePartial {
		return fmt.Errorf("unsupported task evidence completeness %q", completeness)
	}
	if completeness == AgentReviewTaskEvidenceComplete && len(reasons) != 0 {
		return fmt.Errorf("complete task evidence forbids reason_codes")
	}
	if completeness == AgentReviewTaskEvidencePartial && len(reasons) == 0 {
		return fmt.Errorf("partial task evidence requires reason_codes")
	}
	previous := ""
	for index, reason := range reasons {
		if reason != AgentReviewTaskEvidenceBudgetExceeded && reason != AgentReviewTaskEvidenceUnavailable {
			return fmt.Errorf("unsupported task evidence reason_code %q", reason)
		}
		if index > 0 && reason <= previous {
			return fmt.Errorf("reason_codes must be uniquely sorted")
		}
		previous = reason
	}
	return nil
}

func (task AgentReviewTaskExecutionEvidence) validate(index int) error {
	name := fmt.Sprintf("task_executions[%d]", index)
	if err := requireIdentifier(name+".task_id", task.TaskID); err != nil {
		return err
	}
	if err := requireIdentifier(name+".group_id", task.GroupID); err != nil {
		return err
	}
	switch task.TaskRole {
	case AgentTaskContext:
		if task.SkillID != nil || task.HypothesisOccurrenceID != nil {
			return fmt.Errorf("%s context identity is invalid", name)
		}
	case AgentTaskReview:
		if task.SkillID == nil || task.HypothesisOccurrenceID != nil {
			return fmt.Errorf("%s review identity is invalid", name)
		}
	case AgentTaskVerification:
		if task.SkillID != nil || task.HypothesisOccurrenceID == nil {
			return fmt.Errorf("%s verification identity is invalid", name)
		}
	default:
		return fmt.Errorf("%s has unsupported task_role %q", name, task.TaskRole)
	}
	for field, value := range map[string]*string{"skill_id": task.SkillID, "hypothesis_occurrence_id": task.HypothesisOccurrenceID} {
		if value != nil {
			if err := requireIdentifier(name+"."+field, *value); err != nil {
				return err
			}
		}
	}
	if err := task.SystemPrompt.validate(name + ".system_prompt"); err != nil {
		return err
	}
	if err := task.UserPrompt.validate(name + ".user_prompt"); err != nil {
		return err
	}
	if task.Output != nil {
		if err := task.Output.validate(name + ".output"); err != nil {
			return err
		}
	}
	switch task.TerminalStatus {
	case AgentTaskSucceeded:
		if task.Output == nil {
			return fmt.Errorf("%s succeeded task requires output", name)
		}
	case AgentTaskFailed, AgentTaskCanceled:
		if task.Output != nil {
			return fmt.Errorf("%s failed/canceled task forbids output", name)
		}
	default:
		return fmt.Errorf("%s has unsupported terminal_status %q", name, task.TerminalStatus)
	}
	if task.Tools == nil {
		return fmt.Errorf("%s.tools must be an explicit array", name)
	}
	for toolIndex, tool := range task.Tools {
		if tool.Sequence != uint32(toolIndex+1) {
			return fmt.Errorf("%s.tools sequence must be contiguous from 1", name)
		}
		if err := tool.validate(fmt.Sprintf("%s.tools[%d]", name, toolIndex)); err != nil {
			return err
		}
	}
	return nil
}

func (tool AgentReviewTaskToolEvidence) validate(name string) error {
	if err := requireIdentifier(name+".tool_name", tool.ToolName); err != nil {
		return err
	}
	if err := tool.Arguments.validate(name + ".arguments"); err != nil {
		return err
	}
	if tool.Result == nil {
		if tool.IsError != nil {
			return fmt.Errorf("%s is_error requires result", name)
		}
		return nil
	}
	if tool.IsError == nil {
		return fmt.Errorf("%s result requires is_error", name)
	}
	return tool.Result.validate(name + ".result")
}

func (content AgentReviewTaskEvidenceContent) validate(name string) error {
	if err := requireSHA256(name+".sha256", content.SHA256); err != nil {
		return err
	}
	if content.SizeBytes > agentReviewMaxTaskEvidenceContentBytes {
		return fmt.Errorf("%s.size_bytes exceeds %d", name, agentReviewMaxTaskEvidenceContentBytes)
	}
	if content.Content != nil {
		if content.OmissionReason != nil {
			return fmt.Errorf("%s content forbids omission_reason", name)
		}
		if uint64(len([]byte(*content.Content))) != content.SizeBytes {
			return fmt.Errorf("%s.size_bytes does not match content", name)
		}
		digest := sha256.Sum256([]byte(*content.Content))
		if hex.EncodeToString(digest[:]) != content.SHA256 {
			return fmt.Errorf("%s.sha256 does not match content", name)
		}
		return nil
	}
	if content.OmissionReason == nil || *content.OmissionReason != AgentReviewTaskEvidenceBudgetExceeded {
		return fmt.Errorf("%s omitted content requires evidence_budget_exceeded", name)
	}
	return nil
}

func (task AgentReviewTaskExecutionEvidence) hasOmittedContent() bool {
	values := []*AgentReviewTaskEvidenceContent{&task.SystemPrompt, &task.UserPrompt, task.Output}
	for _, tool := range task.Tools {
		values = append(values, &tool.Arguments, tool.Result)
	}
	for _, value := range values {
		if value != nil && value.Content == nil {
			return true
		}
	}
	return false
}

// ValidateAgentReviewTaskEvidenceBindings rejects task evidence that does not
// close the same execution facts as the immutable plan and receipt collection.
func ValidateAgentReviewTaskEvidenceBindings(
	collection AgentReviewTaskEvidenceCollection,
	plan AgentReviewPlan,
	receipts AgentExecutionReceiptCollection,
) error {
	if err := collection.Validate(); err != nil {
		return err
	}
	if collection.PlanID != plan.PlanID || collection.SourceRunID != plan.SourceRunID ||
		collection.ExecutionID != plan.ExecutionID || collection.ReviewRunID != plan.ReviewRunID ||
		collection.TargetDigest != plan.TargetDigest {
		return fmt.Errorf("task evidence does not bind the exact plan identity and target")
	}
	byTask := make(map[string]AgentExecutionReceipt, len(receipts.Receipts))
	for _, receipt := range receipts.Receipts {
		byTask[receipt.TaskID] = receipt
	}
	seen := make(map[string]struct{}, len(collection.TaskExecutions))
	for index, task := range collection.TaskExecutions {
		receipt, exists := byTask[task.TaskID]
		if !exists || receipt.TaskRole != task.TaskRole || receipt.GroupID != task.GroupID ||
			receipt.Status != task.TerminalStatus {
			return fmt.Errorf("task_executions[%d] does not bind its receipt identity and status", index)
		}
		if task.TaskRole == AgentTaskReview && task.SkillID != nil && *task.SkillID != receipt.Dimension.ID {
			return fmt.Errorf("task_executions[%d] skill does not bind receipt dimension", index)
		}
		if task.TaskRole == AgentTaskVerification &&
			(task.HypothesisOccurrenceID == nil || receipt.HypothesisOccurrenceID == nil ||
				*task.HypothesisOccurrenceID != *receipt.HypothesisOccurrenceID) {
			return fmt.Errorf("task_executions[%d] occurrence does not bind verification receipt", index)
		}
		if err := validateTaskEvidenceDigests(task, receipt); err != nil {
			return fmt.Errorf("task_executions[%d]: %w", index, err)
		}
		counts := make(map[string]AgentToolUsage)
		for _, tool := range task.Tools {
			usage := counts[tool.ToolName]
			usage.ToolID = tool.ToolName
			usage.InvocationCount++
			if tool.IsError != nil && *tool.IsError {
				usage.FailureCount++
			}
			counts[tool.ToolName] = usage
		}
		actual := make([]AgentToolUsage, 0, len(counts))
		for _, usage := range counts {
			actual = append(actual, usage)
		}
		slices.SortFunc(actual, func(left, right AgentToolUsage) int {
			if left.ToolID < right.ToolID {
				return -1
			}
			if left.ToolID > right.ToolID {
				return 1
			}
			return 0
		})
		if uint32(len(task.Tools)) != receipt.ToolCalls || !slices.Equal(actual, receipt.ToolUsage) {
			return fmt.Errorf("task_executions[%d] tool transcript does not bind receipt counts", index)
		}
		seen[task.TaskID] = struct{}{}
	}
	missing := len(seen) != len(receipts.Receipts)
	if missing && !slices.Contains(collection.ReasonCodes, AgentReviewTaskEvidenceUnavailable) {
		return fmt.Errorf("missing receipt task evidence requires task_evidence_unavailable")
	}
	if !missing && slices.Contains(collection.ReasonCodes, AgentReviewTaskEvidenceUnavailable) {
		return fmt.Errorf("task_evidence_unavailable is false when every receipt is represented")
	}
	return nil
}

func validateTaskEvidenceDigests(task AgentReviewTaskExecutionEvidence, receipt AgentExecutionReceipt) error {
	if task.SystemPrompt.Content != nil && task.UserPrompt.Content != nil {
		data := []byte(jsonStringifyStringArray(*task.SystemPrompt.Content, *task.UserPrompt.Content))
		digest := sha256.Sum256(data)
		if receipt.PromptDigest == nil || *receipt.PromptDigest != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("exact prompts do not bind receipt prompt_digest")
		}
	}
	if task.Output != nil && task.Output.Content != nil {
		if receipt.OutputDigest == nil || *receipt.OutputDigest != task.Output.SHA256 {
			return fmt.Errorf("exact output does not bind receipt output_digest")
		}
	}
	return nil
}

// jsonStringifyStringArray matches JavaScript JSON.stringify for the prompt
// tuple used by the Pi worker. Go's encoding/json HTML-escapes <, > and &, so
// using it here would reject exact prompts containing ordinary source syntax.
func jsonStringifyStringArray(values ...string) string {
	var output strings.Builder
	output.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			output.WriteByte(',')
		}
		output.WriteByte('"')
		for _, character := range value {
			switch character {
			case '"':
				output.WriteString(`\"`)
			case '\\':
				output.WriteString(`\\`)
			case '\b':
				output.WriteString(`\b`)
			case '\f':
				output.WriteString(`\f`)
			case '\n':
				output.WriteString(`\n`)
			case '\r':
				output.WriteString(`\r`)
			case '\t':
				output.WriteString(`\t`)
			default:
				if character < 0x20 {
					fmt.Fprintf(&output, `\u%04x`, character)
				} else {
					output.WriteRune(character)
				}
			}
		}
		output.WriteByte('"')
	}
	output.WriteByte(']')
	return output.String()
}
