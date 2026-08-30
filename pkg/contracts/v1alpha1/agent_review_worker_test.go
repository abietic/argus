package v1alpha1

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAgentReviewWorkerContractsStrictRoundTripAndBinding(t *testing.T) {
	request := validAgentReviewWorkerRequest(t)
	result := validAgentReviewWorkerResult(request)

	requestData, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decodedRequest, err := DecodeAgentReviewWorkerRequest(requestData)
	if err != nil {
		t.Fatalf("DecodeAgentReviewWorkerRequest() error = %v", err)
	}
	resultData, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	decodedResult, err := DecodeAgentReviewWorkerResult(resultData)
	if err != nil {
		t.Fatalf("DecodeAgentReviewWorkerResult() error = %v", err)
	}
	if err := ValidateAgentReviewWorkerResultBinding(decodedRequest, decodedResult); err != nil {
		t.Fatalf("ValidateAgentReviewWorkerResultBinding() error = %v", err)
	}

	withUnknown := append(requestData[:len(requestData)-1], []byte(`,"credential":"secret"}`)...)
	if _, err := DecodeAgentReviewWorkerRequest(withUnknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("request decoder accepted unknown credential: %v", err)
	}
	withDuplicate := strings.Replace(
		string(requestData),
		`"attempt":1`,
		`"attempt":1,"attempt":2`,
		1,
	)
	if _, err := DecodeAgentReviewWorkerRequest([]byte(withDuplicate)); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("request decoder accepted duplicate field: %v", err)
	}
	if _, err := DecodeAgentReviewWorkerResult(append(resultData, []byte(` {}`)...)); err == nil ||
		(!strings.Contains(err.Error(), "trailing") && !strings.Contains(err.Error(), "multiple JSON")) {
		t.Fatalf("result decoder accepted trailing JSON: %v", err)
	}
	withNull := append(resultData[:len(resultData)-1], []byte(`,"failure":null}`)...)
	if _, err := DecodeAgentReviewWorkerResult(withNull); err == nil ||
		!strings.Contains(err.Error(), "null") {
		t.Fatalf("result decoder accepted explicit null: %v", err)
	}
}

func TestAgentReviewWorkerRequestClosesLineageAndFrozenInput(t *testing.T) {
	request := validAgentReviewWorkerRequest(t)
	request.WorkItemID = "other-work-item"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "plan.execution_id") {
		t.Fatalf("Validate() accepted mismatched work lineage: %v", err)
	}

	request = validAgentReviewWorkerRequest(t)
	request.Capability.MaxInputBytes++
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "capability sha256") {
		t.Fatalf("Validate() accepted capability tamper: %v", err)
	}

	request = validAgentReviewWorkerRequest(t)
	request.ReviewInputBase64 += "\n"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "canonical standard base64") {
		t.Fatalf("Validate() accepted non-canonical base64: %v", err)
	}

	request = validAgentReviewWorkerRequest(t)
	request.Plan.ReviewInputRef.Ref.SizeBytes++
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "input size") {
		t.Fatalf("Validate() accepted forged review input size: %v", err)
	}

	request = validAgentReviewWorkerRequest(t)
	request.Plan.ReviewInputRef.Ref.SHA256 = testDigest
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "review_input_ref") {
		t.Fatalf("Validate() accepted forged review input digest: %v", err)
	}

	request = validAgentReviewWorkerRequest(t)
	request.Plan.TargetDigest = testDigest
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "target_digest") {
		t.Fatalf("Validate() accepted forged target digest: %v", err)
	}

	request = validAgentReviewWorkerRequest(t)
	request.Capability.MaxInputBytes = 1
	request.Capability.SHA256 = workerCapabilityDigest(t, request.Capability)
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "max_input_bytes") {
		t.Fatalf("Validate() accepted input beyond capability: %v", err)
	}
}

func TestAgentReviewWorkerGroupCheckpointClosesScopeContentAndOrder(t *testing.T) {
	request := validAgentReviewWorkerRequest(t)
	content := []byte(fmt.Sprintf(
		`{"schemaVersion":"argus.pi-review.group_checkpoint.v1","checkpointRevision":0,"checkpointScopeSha256":%q,"group":{"id":"group-001"}}`,
		request.CheckpointScopeSHA256,
	))
	checkpoint := AgentReviewWorkerGroupCheckpoint{
		GroupID: "group-001", CheckpointScopeSHA256: request.CheckpointScopeSHA256,
		ContentSHA256: workerTestDigest(content), SizeBytes: uint64(len(content)),
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
	request.GroupCheckpoints = []AgentReviewWorkerGroupCheckpoint{checkpoint}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate(group checkpoint) error = %v", err)
	}

	tampered := request
	scopeTampered := checkpoint
	scopeTampered.CheckpointScopeSHA256 = strings.Repeat("c", 64)
	tampered.GroupCheckpoints = []AgentReviewWorkerGroupCheckpoint{scopeTampered}
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("Validate() accepted checkpoint scope substitution: %v", err)
	}
	tampered = request
	tampered.GroupCheckpoints = append(tampered.GroupCheckpoints, checkpoint)
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("Validate() accepted duplicate checkpoint group: %v", err)
	}
	tampered = request
	revisionTampered := checkpoint
	revisionTampered.CheckpointRevision = 1
	tampered.GroupCheckpoints = []AgentReviewWorkerGroupCheckpoint{revisionTampered}
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("Validate() accepted checkpoint revision substitution: %v", err)
	}
	tampered = request
	contentTampered := checkpoint
	contentTampered.ContentBase64 = base64.StdEncoding.EncodeToString([]byte(`{"changed":true}`))
	tampered.GroupCheckpoints = []AgentReviewWorkerGroupCheckpoint{contentTampered}
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "size_bytes") {
		t.Fatalf("Validate() accepted checkpoint content substitution: %v", err)
	}
}

func TestAgentReviewWorkerResultUnionDigestAndFence(t *testing.T) {
	request := validAgentReviewWorkerRequest(t)
	result := validAgentReviewWorkerResult(request)
	result.ReportSHA256 = nil
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "requires report") {
		t.Fatalf("Validate() accepted success without report digest: %v", err)
	}

	result = validAgentReviewWorkerResult(request)
	result.ReportSHA256 = stringPointer(testDigest)
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "exact report bytes") {
		t.Fatalf("Validate() accepted forged report digest: %v", err)
	}

	result = validAgentReviewWorkerResult(request)
	result.Report = json.RawMessage(`{"status":"complete","optional":null}`)
	nullDigest := workerTestDigest(result.Report)
	result.ReportSHA256 = &nullDigest
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("Validate() accepted explicit null in raw report: %v", err)
	}

	result = validAgentReviewWorkerResult(request)
	result.Status = AgentReviewWorkerFailed
	result.Failure = &AgentReviewWorkerFailure{
		Code: "provider_error", Message: "provider request failed", Retryable: true,
	}
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "forbids report") {
		t.Fatalf("Validate() accepted failure with report: %v", err)
	}
	result.Report = nil
	result.ReportSHA256 = nil
	if err := result.Validate(); err != nil {
		t.Fatalf("Validate() rejected stable failure result: %v", err)
	}

	result = validAgentReviewWorkerResult(request)
	result.FencingToken++
	if err := ValidateAgentReviewWorkerResultBinding(request, result); err == nil ||
		!strings.Contains(err.Error(), "exact request identity") {
		t.Fatalf("binding accepted stale fence: %v", err)
	}

	result = validAgentReviewWorkerResult(request)
	request.Capability.MaxOutputBytes = 1
	request.Capability.SHA256 = workerCapabilityDigest(t, request.Capability)
	result.CapabilitySHA256 = request.Capability.SHA256
	if err := ValidateAgentReviewWorkerResultBinding(request, result); err == nil ||
		!strings.Contains(err.Error(), "max_output_bytes") {
		t.Fatalf("binding accepted report beyond output limit: %v", err)
	}

	request = validAgentReviewWorkerRequest(t)
	result = validAgentReviewWorkerResult(request)
	result.CompletedAt = request.Deadline.Add(time.Nanosecond)
	if err := ValidateAgentReviewWorkerResultBinding(request, result); err == nil ||
		!strings.Contains(err.Error(), "after request deadline") {
		t.Fatalf("binding accepted late success: %v", err)
	}

	result = validAgentReviewWorkerResult(request)
	result.Status = AgentReviewWorkerCanceled
	result.Report = nil
	result.ReportSHA256 = nil
	result.Failure = &AgentReviewWorkerFailure{
		Code: "deadline_exceeded", Message: "worker stopped", Retryable: true,
	}
	result.CompletedAt = request.Deadline.Add(time.Nanosecond)
	if err := ValidateAgentReviewWorkerResultBinding(request, result); err != nil {
		t.Fatalf("binding rejected canceled result that converged after deadline: %v", err)
	}
}

func TestAgentExecutionReceiptBindsPlanRuntimeProfileAndContentDigests(t *testing.T) {
	plan := validAgentReviewPlan()
	set := validReviewHypothesisSet(t)
	collection := validAgentExecutionReceiptCollection()
	manifest := agentReviewManifestFor(t, plan, set, collection)
	collection.Receipts[0].Runtime = testVersionedRef("other-runtime")
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "exact plan identity") {
		t.Fatalf("binding accepted mismatched runtime: %v", err)
	}

	receipt := validAgentExecutionReceipt()
	receipt.PromptDigest = nil
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "prompt_digest") {
		t.Fatalf("Validate() accepted success without prompt digest: %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.Status = AgentTaskFailed
	reason := "provider_error"
	receipt.FailureReasonCode = &reason
	receipt.OutputDigest = nil
	receipt.PromptDigest = nil
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "started model turn") {
		t.Fatalf("Validate() accepted started turn without prompt digest: %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.Status = AgentTaskCanceled
	receipt.FailureReasonCode = stringPointer("deadline_exceeded")
	receipt.PromptDigest = nil
	receipt.OutputDigest = nil
	receipt.ModelTurnsStarted = 0
	receipt.ModelTurnsCompleted = 0
	receipt.ToolCalls = 0
	receipt.ToolUsage = []AgentToolUsage{}
	receipt.Usage = AgentTokenUsage{
		Completeness:          AgentTokenUsageUnavailable,
		UnavailableReasonCode: stringPointer("provider_not_started"),
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("Validate() rejected zero-turn canceled receipt: %v", err)
	}
}

func validAgentReviewWorkerRequest(t *testing.T) AgentReviewWorkerRequest {
	t.Helper()
	input := []byte(`{"schema_version":"argus.review_input.v1alpha1"}`)
	inputDigest := workerTestDigest(input)
	plan := validAgentReviewPlan()
	rulePack := struct {
		SchemaVersion string          `json:"schema_version"`
		ID            string          `json:"id"`
		Revision      string          `json:"revision"`
		Rules         json.RawMessage `json:"rules"`
		SHA256        string          `json:"sha256"`
	}{
		SchemaVersion: "argus.rule_pack.v1alpha1", ID: "worker-review-rules",
		Revision: "1", Rules: json.RawMessage(`[]`),
	}
	semanticRulePack, err := json.Marshal(rulePack)
	if err != nil {
		t.Fatal(err)
	}
	rulePack.SHA256 = workerTestDigest(semanticRulePack)
	rulePackBytes, err := json.Marshal(rulePack)
	if err != nil {
		t.Fatal(err)
	}
	plan.RulePack = VersionedRef{
		ID: rulePack.ID, Revision: rulePack.Revision, SHA256: rulePack.SHA256,
	}
	plan.TargetDigest = inputDigest
	plan.ReviewInputRef.Ref.SHA256 = inputDigest
	plan.ReviewInputRef.Ref.SizeBytes = int64(len(input))
	capability := AgentReviewWorkerCapability{
		Protocol: AgentReviewWorkerProtocolStdio, FrozenInput: AgentReviewWorkerFrozenInputBase64,
		MaxInputBytes: 1024, MaxOutputBytes: 4096,
	}
	capability.SHA256 = workerCapabilityDigest(t, capability)
	promptBytes, err := MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		t.Fatal(err)
	}
	promptDigest := workerTestDigest(promptBytes)
	promptBundle, err := NewAgentReviewWorkerPromptBundle(
		VersionedRef{ID: "pi-review-prompts", Revision: "v0", SHA256: promptDigest},
		ArtifactBinding{
			Ref: ContentRef{
				URI: "artifact://test/prompt-bundles/default", SHA256: promptDigest,
				SizeBytes: int64(len(promptBytes)),
			},
			Contract: AgentStagePlanPromptContract,
		},
		promptBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	skillBytes := []byte("# Correctness\n\nFind concrete correctness defects.\n")
	skillDigest := workerTestDigest(skillBytes)
	plan.ReviewDimensions[0] = VersionedRef{
		ID: "correctness", Revision: "test-v1", SHA256: skillDigest,
	}
	reviewSkill, err := NewAgentReviewWorkerSkill(
		plan.ReviewDimensions[0],
		ArtifactBinding{
			Ref: ContentRef{
				URI: "artifact://test/review-skills/correctness", SHA256: skillDigest,
				SizeBytes: int64(len(skillBytes)),
			},
			Contract: AgentStagePlanSkillContract,
		},
		skillBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	return AgentReviewWorkerRequest{
		SchemaVersion: AgentReviewWorkerRequestSchemaVersion,
		WorkItemID:    plan.ExecutionID, Attempt: 1, Generation: 1, FencingToken: 7,
		IdempotencyKey: "execution-1-attempt-1", Capability: capability,
		Deadline: plan.CreatedAt.Add(time.Minute), Plan: plan,
		RulePackBase64:        base64.StdEncoding.EncodeToString(rulePackBytes),
		PromptBundle:          promptBundle,
		ReviewSkills:          []AgentReviewWorkerSkill{reviewSkill},
		KnowledgePacks:        []AgentReviewWorkerKnowledge{},
		ContextArtifacts:      []AgentReviewWorkerContextArtifact{},
		CheckpointScopeSHA256: workerTestDigest([]byte("checkpoint-scope")),
		GroupCheckpoints:      []AgentReviewWorkerGroupCheckpoint{},
		ReviewInputBase64:     base64.StdEncoding.EncodeToString(input),
	}
}

func validAgentReviewWorkerResult(request AgentReviewWorkerRequest) AgentReviewWorkerResult {
	report := json.RawMessage(`{"schema_version":"argus.pi-review.v0","status":"complete"}`)
	reportDigest := workerTestDigest(report)
	return AgentReviewWorkerResult{
		SchemaVersion: AgentReviewWorkerResultSchemaVersion,
		WorkItemID:    request.WorkItemID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, CapabilitySHA256: request.Capability.SHA256,
		Status: AgentReviewWorkerSucceeded, ReportSHA256: &reportDigest, Report: report,
		CompletedAt: request.Plan.CreatedAt.Add(30 * time.Second),
	}
}

func workerCapabilityDigest(t *testing.T, capability AgentReviewWorkerCapability) string {
	t.Helper()
	digest, err := DigestAgentReviewWorkerCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func workerTestDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
