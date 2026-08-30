package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentReviewTaskEvidenceExactBindingAndTamper(t *testing.T) {
	plan := validAgentReviewPlan()
	receipts := singleAgentExecutionReceiptCollection()
	systemPrompt := "collect exact context"
	userPrompt := "review frozen source where value < limit && next > 0"
	output := `{"facts":[],"gaps":[]}`
	promptData := []byte(jsonStringifyStringArray(systemPrompt, userPrompt))
	promptDigest := sha256.Sum256(promptData)
	outputDigest := sha256.Sum256([]byte(output))
	receipts.Receipts[0].PromptDigest = stringPointer(hex.EncodeToString(promptDigest[:]))
	receipts.Receipts[0].OutputDigest = stringPointer(hex.EncodeToString(outputDigest[:]))
	collection := AgentReviewTaskEvidenceCollection{
		SchemaVersion: AgentReviewTaskEvidenceCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest: plan.TargetDigest,
		Authority:    AgentReviewTaskEvidenceAuthority, Provenance: AgentReviewTaskEvidenceProvenance,
		Disposition: AgentReviewTaskEvidenceDisposition, ContentPolicy: AgentReviewTaskEvidenceContentPolicy,
		Completeness: AgentReviewTaskEvidenceComplete, ReasonCodes: []string{},
		TaskExecutions: []AgentReviewTaskExecutionEvidence{{
			TaskID: "task-1", TaskRole: AgentTaskContext, GroupID: "group-1",
			SystemPrompt: taskEvidenceContent(systemPrompt), UserPrompt: taskEvidenceContent(userPrompt),
			Output: taskEvidenceContentPointer(output), TerminalStatus: AgentTaskSucceeded,
			Tools: []AgentReviewTaskToolEvidence{
				{Sequence: 1, ToolName: "read_file", Arguments: taskEvidenceContent(`{"path":"main.go"}`), Result: taskEvidenceContentPointer(`{"content":"source"}`), IsError: boolPointer(false)},
				{Sequence: 2, ToolName: "submit_context", Arguments: taskEvidenceContent(output), Result: taskEvidenceContentPointer(`{"accepted":true}`), IsError: boolPointer(false)},
			},
		}},
	}
	if err := ValidateAgentReviewTaskEvidenceBindings(collection, plan, receipts); err != nil {
		t.Fatalf("exact task evidence binding: %v", err)
	}
	data, err := json.Marshal(collection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAgentReviewTaskEvidenceCollection(data); err != nil {
		t.Fatalf("strict round trip: %v", err)
	}

	tampered := collection
	tampered.TaskExecutions = append([]AgentReviewTaskExecutionEvidence{}, collection.TaskExecutions...)
	tampered.TaskExecutions[0].UserPrompt = taskEvidenceContent("different prompt")
	if err := ValidateAgentReviewTaskEvidenceBindings(tampered, plan, receipts); err == nil ||
		!strings.Contains(err.Error(), "prompt_digest") {
		t.Fatalf("prompt tamper accepted: %v", err)
	}

	omission := AgentReviewTaskEvidenceBudgetExceeded
	partial := collection
	partial.Completeness = AgentReviewTaskEvidencePartial
	partial.ReasonCodes = []string{AgentReviewTaskEvidenceBudgetExceeded}
	partial.TaskExecutions = append([]AgentReviewTaskExecutionEvidence{}, collection.TaskExecutions...)
	partial.TaskExecutions[0].UserPrompt = AgentReviewTaskEvidenceContent{
		SHA256:         collection.TaskExecutions[0].UserPrompt.SHA256,
		SizeBytes:      collection.TaskExecutions[0].UserPrompt.SizeBytes,
		OmissionReason: &omission,
	}
	if err := ValidateAgentReviewTaskEvidenceBindings(partial, plan, receipts); err != nil {
		t.Fatalf("bounded digest-only task evidence: %v", err)
	}
}

func taskEvidenceContent(value string) AgentReviewTaskEvidenceContent {
	digest := sha256.Sum256([]byte(value))
	return AgentReviewTaskEvidenceContent{
		SHA256: hex.EncodeToString(digest[:]), SizeBytes: uint64(len([]byte(value))), Content: &value,
	}
}

func taskEvidenceContentPointer(value string) *AgentReviewTaskEvidenceContent {
	content := taskEvidenceContent(value)
	return &content
}

func boolPointer(value bool) *bool { return &value }
