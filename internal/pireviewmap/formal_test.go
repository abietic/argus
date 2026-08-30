package pireviewmap

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestMapFormalReviewResultClosesFormalLineageAndEvidence(t *testing.T) {
	fixture := newFormalMapFixture(t)
	artifacts, err := MapFormalReviewArtifacts(fixture)
	if err != nil {
		t.Fatalf("MapFormalReviewArtifacts() error = %v", err)
	}
	if artifacts.Receipts.PlanID != fixture.Plan.PlanID ||
		artifacts.Receipts.SourceRunID != fixture.Plan.ReviewRunID ||
		artifacts.Receipts.ReviewRunID != fixture.Plan.ReviewRunID ||
		artifacts.Receipts.ExecutionID != fixture.Request.ExecutionID ||
		len(artifacts.Receipts.Receipts) == 0 {
		t.Fatalf("formal receipt lineage escaped: %+v", artifacts.Receipts)
	}
	if err := contractsv1alpha1.ValidateAgentReviewRawCandidateSetBindings(
		artifacts.RawCandidates,
		artifacts.Hypotheses,
		fixture.WorkerRequest.Plan.ReviewDimensions,
	); err != nil {
		t.Fatalf("formal raw candidate closure = %v", err)
	}
	if len(artifacts.RawCandidates.RawCandidates) != 1 ||
		artifacts.RawCandidates.RawCandidates[0].RawCandidateID != "raw-fixture-1" {
		t.Fatalf("formal raw candidate projection = %+v", artifacts.RawCandidates)
	}

	set, err := MapFormalReviewResult(fixture)
	if err != nil {
		t.Fatalf("MapFormalReviewResult() error = %v", err)
	}
	if set.PlanID != fixture.Plan.PlanID ||
		set.SourceRunID != fixture.Plan.ReviewRunID ||
		set.ReviewRunID != fixture.Plan.ReviewRunID ||
		set.ExecutionID != fixture.Request.ExecutionID ||
		set.TargetDigest != fixture.Plan.TargetDigest {
		t.Fatalf("formal lineage escaped: %+v", set)
	}
	if set.Completeness != contractsv1alpha1.AgentReviewComplete ||
		len(set.Hypotheses) != 1 || len(set.DedupClusters) != 1 ||
		len(set.NormalizationDecisions) != 1 {
		t.Fatalf("mapped defect/dedup projection = %+v", set)
	}
	hypothesis := set.Hypotheses[0]
	if hypothesis.Dimension != fixture.WorkerRequest.Plan.ReviewDimensions[0] ||
		hypothesis.Anchor.StartLine != 4 || hypothesis.Anchor.EndLine != 4 ||
		len(hypothesis.Evidence) != 1 ||
		hypothesis.Evidence[0].Excerpt != "return *ptr" ||
		len(hypothesis.Verification) != 1 ||
		hypothesis.Verification[0].Verifier != fixture.WorkerRequest.Plan.Verifier ||
		hypothesis.Verification[0].Verdict != contractsv1alpha1.HypothesisVerificationConfirmed {
		t.Fatalf("formal skill/anchor/excerpt/verification projection = %+v", hypothesis)
	}
	if set.Coverage.GroupsTotal != 1 || set.Coverage.GroupsReviewed != 1 ||
		set.Coverage.ReviewTasksTotal != 1 || set.Coverage.ReviewTasksSucceeded != 1 ||
		set.Coverage.FilesIncluded != 1 || len(set.Coverage.Gaps) != 0 {
		t.Fatalf("formal coverage projection = %+v", set.Coverage)
	}
}

func TestMapFormalReviewResultAcceptsRejectedTerminalAttemptBeforeSuccess(t *testing.T) {
	fixture := newFormalMapFixture(t)
	report, completedAt := formalPiReport(
		t,
		fixture.WorkerRequest,
		fixture.ReviewInput,
		fixture.Plan.Prompt.Ref.SHA256,
		4,
	)
	for index := range report.Execution.Tasks {
		task := &report.Execution.Tasks[index]
		if task.TaskKind != "review" {
			continue
		}
		task.ToolCalls = 2
		task.ToolUsage[0].InvocationCount = 2
		task.ToolUsage[0].FailureCount = 1
	}
	fixture.WorkerResult = formalWorkerResultAt(
		t,
		fixture.WorkerRequest,
		report,
		completedAt,
	)
	if _, err := MapFormalReviewArtifacts(fixture); err != nil {
		t.Fatalf("MapFormalReviewArtifacts() rejected terminal schema correction retry: %v", err)
	}
}

func TestMapFormalReviewResultRecomputesSemanticDuplicate(t *testing.T) {
	fixture := newFormalMapFixture(t)
	report, completedAt := formalPiReport(
		t,
		fixture.WorkerRequest,
		fixture.ReviewInput,
		fixture.Plan.Prompt.Ref.SHA256,
		4,
	)
	canonical := report.Candidates[0]
	canonical.Title = "applyDecision accepts invalid transitions and overwrites decidedBy instead of rejecting them"
	canonical.Description = "applyDecision unconditionally assigns next to approval.state and actorId to approval.decidedBy with no guard for invalid state transitions or already-decided records. A fixture with state approved and decidedBy alice is passed to applyDecision with rejected and bob inside assert.throws. This implementation does not throw and does not preserve the prior value, so the already-approved record is silently changed to rejected and the original decider is overwritten."
	fingerprint, err := piClusterFingerprint(canonical.PiCandidateClaim)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := "candidate-" + fingerprint[:16]
	canonical.ID = candidateID
	canonical.Fingerprint = fingerprint
	canonical.Verification.CandidateID = candidateID
	report.Candidates[0] = canonical
	report.Findings[0] = canonical
	report.RawCandidates[0].Claim = canonical.PiCandidateClaim
	report.NormalizationDecisions[0].NormalizedCandidateID = &candidateID

	verificationTask := &report.Execution.Tasks[len(report.Execution.Tasks)-1]
	verificationTask.CandidateID = &candidateID
	verificationTask.TaskID = "agent-task-verification-" + candidateID
	verificationData, err := json.Marshal(canonical.Verification)
	if err != nil {
		t.Fatal(err)
	}
	verificationDigest := "sha256:" + digestBytesRaw(verificationData)
	verificationTask.OutputDigest = &verificationDigest

	semanticClaim := canonical.PiCandidateClaim
	semanticClaim.Category = "runtime-safety"
	semanticClaim.Title = "applyDecision silently applies an illegal rejection and mutates the approval instead of throwing"
	semanticClaim.Description = "applyDecision has no transition guard. For any supplied Approval and next state it sets approval.state to next, sets approval.decidedBy to actorId, and returns the same object. A test exercises applyDecision with rejected and bob where approval is already state approved and decidedBy alice, and expects this call to throw and leave decidedBy as alice. The implementation returns normally and overwrites both fields, so an already-decided approval is silently flipped to rejected and the original decider is lost."
	report.RawCandidates = append(report.RawCandidates, piRawCandidate{
		RawCandidateID: "raw-fixture-2",
		GroupID:        canonical.GroupID,
		Skill:          canonical.Skill,
		Ordinal:        1,
		Claim:          semanticClaim,
	})
	report.NormalizationDecisions = append(report.NormalizationDecisions, piNormalizationDecision{
		RawCandidateID:        "raw-fixture-2",
		Action:                "merged_duplicate",
		ReasonCode:            "semantic_duplicate",
		NormalizedCandidateID: &candidateID,
	})
	fixture.WorkerResult = formalWorkerResultAt(t, fixture.WorkerRequest, report, completedAt)

	artifacts, err := MapFormalReviewArtifacts(fixture)
	if err != nil {
		t.Fatalf("MapFormalReviewArtifacts() rejected semantic duplicate: %v", err)
	}
	if len(artifacts.Hypotheses.Hypotheses) != 1 ||
		len(artifacts.RawCandidates.RawCandidates) != 2 ||
		artifacts.RawCandidates.RawCandidates[1].ReasonCode != "semantic_duplicate" {
		t.Fatalf("semantic duplicate projection = %+v / %+v", artifacts.Hypotheses, artifacts.RawCandidates)
	}

	report.RawCandidates[1].Claim.Title = "audit writes omit authenticated actor identity"
	fixture.WorkerResult = formalWorkerResultAt(t, fixture.WorkerRequest, report, completedAt)
	if _, err := MapFormalReviewArtifacts(fixture); err == nil ||
		!strings.Contains(err.Error(), "semantic duplicate decision is not the host recomputation") {
		t.Fatalf("dishonest semantic duplicate error = %v", err)
	}
}

func TestMapFormalReviewArtifactsRetainsDiagnosticsWhenCandidateMappingFails(t *testing.T) {
	fixture := newFormalMapFixture(t)
	report, completedAt := formalPiReport(
		t,
		fixture.WorkerRequest,
		fixture.ReviewInput,
		fixture.Plan.Prompt.Ref.SHA256,
		4,
	)
	report.Candidates[0].Fingerprint = strings.Repeat("f", 64)
	fixture.WorkerResult = formalWorkerResultAt(
		t,
		fixture.WorkerRequest,
		report,
		completedAt,
	)
	artifacts, err := MapFormalReviewArtifacts(fixture)
	if err == nil || !strings.Contains(err.Error(), "fingerprint is not the host recomputation") {
		t.Fatalf("candidate fingerprint rejection error = %v", err)
	}
	if artifacts.TaskEvidence.SchemaVersion !=
		contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion ||
		artifacts.Receipts.SchemaVersion !=
			contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion ||
		len(artifacts.Receipts.Receipts) == 0 ||
		artifacts.Hypotheses.SchemaVersion != "" ||
		artifacts.RawCandidates.SchemaVersion != "" {
		t.Fatalf("candidate failure diagnostics escaped business boundary: %+v", artifacts)
	}
}

func TestPiClusterFingerprintMatchesJSONStringifyForHTMLCharacters(t *testing.T) {
	claim := piCandidateClaim{
		Category: "exception semantics mismatch",
		Title:    "applyDecision silently applies an invalid approved->rejected & <guard> transition instead of throwing",
		Anchor: piSourceAnchor{
			Path: "src/approval.ts", Side: "file", StartLine: 9, EndLine: 17,
		},
	}
	got, err := piClusterFingerprint(claim)
	if err != nil {
		t.Fatal(err)
	}
	const want = "3f409844d92ec81bdb264fc8de7e08f5cb1a56c5a72ef79ae31ad31fbc1b514e"
	if got != want {
		t.Fatalf("piClusterFingerprint() = %s, want JSON.stringify vector %s", got, want)
	}
}

func TestPiProfileForProviderMatchesVersionedWorkerRuntimeIdentity(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "deepseek-anthropic", want: "deepseek-anthropic-env@1"},
		{provider: "anthropic", want: "anthropic-official@1"},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			got, err := piProfileForProvider(test.provider)
			if err != nil {
				t.Fatalf("piProfileForProvider() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("piProfileForProvider() = %q, want worker runtime identity %q", got, test.want)
			}
		})
	}
}

func TestBuildFormalWorkerRequestRoundTripsThroughFormalMapper(t *testing.T) {
	fixture := newFormalMapFixture(t)
	workerRequest, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		fixture.ModelProfile,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		nil,
		nil,
		mustRulePackBytes(t, fixture.WorkerRequest),
	)
	if err != nil {
		t.Fatalf("BuildFormalWorkerRequest() error = %v", err)
	}
	report, completedAt := formalPiReport(
		t,
		workerRequest,
		fixture.ReviewInput,
		fixture.Plan.Prompt.Ref.SHA256,
		4,
	)
	fixture.WorkerRequest = workerRequest
	fixture.WorkerResult = formalWorkerResultAt(t, workerRequest, report, completedAt)
	if _, err := MapFormalReviewResult(fixture); err != nil {
		t.Fatalf("MapFormalReviewResult(built request) error = %v", err)
	}
}

func TestFormalWorkerDeadlineReservesDeterministicCompletionWindow(t *testing.T) {
	createdAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		window time.Duration
		grace  time.Duration
	}{
		{name: "caps long window", window: time.Minute, grace: 5 * time.Second},
		{name: "reserves ten percent", window: 20 * time.Second, grace: 2 * time.Second},
		{name: "preserves short window", window: time.Millisecond, grace: 100 * time.Microsecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			stageDeadline := createdAt.Add(test.window)
			deadline, err := formalWorkerDeadline(createdAt, stageDeadline)
			if err != nil {
				t.Fatal(err)
			}
			if want := stageDeadline.Add(-test.grace); !deadline.Equal(want) {
				t.Fatalf("worker deadline = %s, want %s", deadline, want)
			}
			if !deadline.Before(stageDeadline) || !deadline.After(createdAt) {
				t.Fatalf("worker deadline does not reserve an ordered window: %s", deadline)
			}
		})
	}
	if _, err := formalWorkerDeadline(createdAt, createdAt); err == nil {
		t.Fatal("formalWorkerDeadline accepted an exhausted stage window")
	}
}

func TestFormalMapperRejectsWorkerDeadlineOutsideDeterministicFence(t *testing.T) {
	fixture := newFormalMapFixture(t)
	fixture.WorkerRequest.Deadline = fixture.Request.Deadline
	report, err := decodePiReviewReport(fixture.WorkerResult.Report)
	if err != nil {
		t.Fatal(err)
	}
	fixture.WorkerResult = formalWorkerResultAt(
		t,
		fixture.WorkerRequest,
		report,
		fixture.WorkerResult.CompletedAt,
	)
	if _, err := MapFormalReviewArtifacts(fixture); err == nil ||
		!strings.Contains(err.Error(), "formal execution fence") {
		t.Fatalf("outer-deadline substitution error = %v", err)
	}
}

func TestBuildFormalWorkerRequestPreservesOpaqueWireModelBehindSafeComponentIdentity(t *testing.T) {
	fixture := newFormalMapFixture(t)
	workerRequest, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		fixture.ModelProfile,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		nil,
		nil,
		mustRulePackBytes(t, fixture.WorkerRequest),
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Plan.Model.Ref.ID == workerRequest.Plan.Model.ID ||
		!strings.HasPrefix(fixture.Plan.Model.Ref.ID, "deepseek-anthropic-model-") ||
		workerRequest.Plan.Model.ID != "deepseek-v4-pro[1m]" ||
		workerRequest.Plan.Model.Revision != fixture.Plan.Model.Ref.Revision ||
		workerRequest.Plan.Model.SHA256 != fixture.Plan.Model.Ref.SHA256 {
		t.Fatalf(
			"formal model identity or exact wire lowering escaped: formal=%+v worker=%+v",
			fixture.Plan.Model.Ref,
			workerRequest.Plan.Model,
		)
	}
}

func TestBuildFormalWorkerRequestRejectsModelProfileMutationAndProviderSubstitution(t *testing.T) {
	fixture := newFormalMapFixture(t)
	mutated := append([]byte{}, fixture.ModelProfile...)
	mutated[len(mutated)-2] ^= 1
	if _, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		mutated,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		nil,
		nil,
		mustRulePackBytes(t, fixture.WorkerRequest),
	); err == nil || !strings.Contains(err.Error(), "do not bind") {
		t.Fatalf("mutated model profile error = %v", err)
	}

	wrongProvider, err := contractsv1alpha1.NewModelProfile(
		"anthropic",
		"deepseek-v4-pro[1m]",
	)
	if err != nil {
		t.Fatal(err)
	}
	wrongProviderBytes, err := contractsv1alpha1.MarshalModelProfile(wrongProvider)
	if err != nil {
		t.Fatal(err)
	}
	wrongProviderDigest := shaHex(wrongProviderBytes)
	fixture.ModelProfile = wrongProviderBytes
	fixture.Plan.Model.Ref.SHA256 = wrongProviderDigest
	fixture.Plan.Model.Artifact.Ref.SHA256 = wrongProviderDigest
	fixture.Plan.Model.Artifact.Ref.SizeBytes = int64(len(wrongProviderBytes))
	fixture.Plan = sealFormalPlan(t, fixture.Plan)
	rebindFormalRequestPlan(t, &fixture)
	if _, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		fixture.ModelProfile,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		nil,
		nil,
		mustRulePackBytes(t, fixture.WorkerRequest),
	); err == nil || !strings.Contains(err.Error(), "provider does not bind") {
		t.Fatalf("substituted model profile provider error = %v", err)
	}
}

func TestBuildFormalWorkerRequestAllowsHostMaterializedContextProviderProvenance(t *testing.T) {
	fixture := newFormalMapFixture(t)
	digest := testDigest("context-provider")
	fixture.Plan.ContextProviders = []contractsv1alpha1.AgentStageContextProviderBinding{{
		ID: "go-ast", Revision: "1", Kind: "go_ast",
		Adapter: testComponent(
			"argus-go-ast", "2", digest,
			contractsv1alpha1.AgentStagePlanContextProviderAdapterContract,
		),
	}}
	fixture.Plan = sealFormalPlan(t, fixture.Plan)
	rebindFormalRequestPlan(t, &fixture)
	workerRequest, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		fixture.ModelProfile,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		nil,
		nil,
		mustRulePackBytes(t, fixture.WorkerRequest),
	)
	if err != nil {
		t.Fatalf("BuildFormalWorkerRequest(context provider provenance) error = %v", err)
	}
	fixture.WorkerRequest = workerRequest
	if _, err := MapFormalReviewResult(fixture); err != nil {
		t.Fatalf("MapFormalReviewResult(context provider provenance) error = %v", err)
	}
}

func TestMapFormalReviewResultRejectsWireModelSubstitution(t *testing.T) {
	fixture := newFormalMapFixture(t)
	fixture.WorkerRequest.Plan.Model.ID = "another-model"
	if _, err := MapFormalReviewResult(fixture); err == nil ||
		!strings.Contains(err.Error(), "exact formal component closure") {
		t.Fatalf("wire model substitution error = %v", err)
	}
}

func TestFormalReviewRequiresAtLeastOneCompletedReviewGroup(t *testing.T) {
	hypotheses := contractsv1alpha1.ReviewHypothesisSet{
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			GroupsTotal: 1, GroupsReviewed: 0,
		},
	}
	if err := requireFormalReviewedCoverage(hypotheses); !errors.Is(
		err,
		ErrFormalReviewNoCompletedGroups,
	) {
		t.Fatalf("zero reviewed groups error = %v", err)
	}
	hypotheses.Coverage.GroupsReviewed = 1
	if err := requireFormalReviewedCoverage(hypotheses); err != nil {
		t.Fatalf("completed reviewed group rejected: %v", err)
	}
}

func TestMapFormalReviewArtifactsReturnsDiagnosticsWhenNoGroupCompleted(t *testing.T) {
	fixture := newFormalMapFixture(t)
	report, completedAt := formalPiReport(
		t,
		fixture.WorkerRequest,
		fixture.ReviewInput,
		fixture.Plan.Prompt.Ref.SHA256,
		4,
	)
	report.Coverage.GroupsReviewed = 0
	report.Coverage.ReviewTasksSucceeded = 0
	report.Coverage.VerificationTasksTotal = 0
	report.Coverage.VerificationTasksSucceeded = 0
	report.Summary = piSummary{}
	report.Findings = []piCandidate{}
	report.Candidates = []piCandidate{}
	report.RawCandidates = []piRawCandidate{}
	report.NormalizationDecisions = []piNormalizationDecision{}
	report.Execution.Tasks = report.Execution.Tasks[:2]
	reviewTask := &report.Execution.Tasks[1]
	failureCode := "structured_output_missing"
	reviewTask.TerminalStatus = "failed"
	reviewTask.ErrorCode = &failureCode
	reviewTask.OutputDigest = nil
	reviewTask.ToolCalls = 0
	reviewTask.ToolNames = []string{}
	reviewTask.ToolUsage = []piToolUsage{}
	fixture.WorkerResult = formalWorkerResultAt(
		t,
		fixture.WorkerRequest,
		report,
		completedAt,
	)

	artifacts, err := MapFormalReviewArtifacts(fixture)
	if !errors.Is(err, ErrFormalReviewNoCompletedGroups) {
		t.Fatalf("MapFormalReviewArtifacts() error = %v", err)
	}
	if artifacts.Hypotheses.SchemaVersion == "" ||
		artifacts.TaskEvidence.SchemaVersion == "" ||
		artifacts.Receipts.SchemaVersion == "" ||
		len(artifacts.Receipts.Receipts) != 2 {
		t.Fatalf("failed formal diagnostics were discarded: %+v", artifacts)
	}
}

func TestFormalNoReviewedGroupRetryCodeRequiresUniformTransientReviewFailures(t *testing.T) {
	failureReason := func(value string) *string { return &value }
	receipts := contractsv1alpha1.AgentExecutionReceiptCollection{
		Receipts: []contractsv1alpha1.AgentExecutionReceipt{
			{TaskRole: contractsv1alpha1.AgentTaskContext, Status: contractsv1alpha1.AgentTaskSucceeded},
			{TaskRole: contractsv1alpha1.AgentTaskReview, Status: contractsv1alpha1.AgentTaskFailed, FailureReasonCode: failureReason("provider_error")},
			{TaskRole: contractsv1alpha1.AgentTaskReview, Status: contractsv1alpha1.AgentTaskFailed, FailureReasonCode: failureReason("provider_error")},
		},
	}
	if code, ok := formalNoReviewedGroupRetryCode(receipts); !ok || code != "provider_error" {
		t.Fatalf("provider failure classification = (%q, %v)", code, ok)
	}
	receipts.Receipts[2].FailureReasonCode = failureReason("timeout")
	if code, ok := formalNoReviewedGroupRetryCode(receipts); !ok || code != "deadline_exceeded" {
		t.Fatalf("timeout failure classification = (%q, %v)", code, ok)
	}
	receipts.Receipts[2].FailureReasonCode = failureReason("structured_output_missing")
	if code, ok := formalNoReviewedGroupRetryCode(receipts); ok || code != "" {
		t.Fatalf("mixed non-transient classification = (%q, %v)", code, ok)
	}
}

func TestBuildFormalWorkerRequestConsumesExactGovernedKnowledge(t *testing.T) {
	fixture := newFormalMapFixture(t)
	knowledgeBytes := []byte("# Repository invariant\n\nOwnership must survive every state transition.\n")
	knowledgeDigest := shaHex(knowledgeBytes)
	fixture.Plan.Knowledge = []contractsv1alpha1.AgentStageKnowledgeBinding{{
		ID: "repository-invariants",
		Ref: contractsv1alpha1.VersionedRef{
			ID: "repository-invariants", Revision: "sha256-custom", SHA256: knowledgeDigest,
		},
		Artifact: testArtifact(
			"repository-invariants", knowledgeDigest,
			contractsv1alpha1.AgentStagePlanKnowledgeContract, int64(len(knowledgeBytes)),
		),
	}}
	fixture.Plan = sealFormalPlan(t, fixture.Plan)
	rebindFormalRequestPlan(t, &fixture)
	workerRequest, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		fixture.ModelProfile,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		[][]byte{knowledgeBytes},
		nil,
		mustRulePackBytes(t, fixture.WorkerRequest),
	)
	if err != nil {
		t.Fatalf("BuildFormalWorkerRequest(knowledge) error = %v", err)
	}
	if len(workerRequest.KnowledgePacks) != 1 ||
		workerRequest.KnowledgePacks[0].Ref != fixture.Plan.Knowledge[0].Ref {
		t.Fatalf("knowledge transport = %+v", workerRequest.KnowledgePacks)
	}
	report, completedAt := formalPiReport(
		t, workerRequest, fixture.ReviewInput, fixture.Plan.Prompt.Ref.SHA256, 4,
	)
	fixture.WorkerRequest = workerRequest
	fixture.WorkerResult = formalWorkerResultAt(t, workerRequest, report, completedAt)
	if _, err := MapFormalReviewResult(fixture); err != nil {
		t.Fatalf("MapFormalReviewResult(knowledge) error = %v", err)
	}
}

func TestBuildFormalWorkerRequestConsumesExactFrozenContextArtifact(t *testing.T) {
	fixture := newFormalMapFixture(t)
	contextBytes := []byte(`{"calls":[{"caller":"Serve","callee":"Handle"}],"types":["Handle func(Request) Response"]}`)
	contextDigest := shaHex(contextBytes)
	fixture.ReviewInput.Contexts = []reviewcore.ContextBinding{{
		Ref: &reviewcore.ContextRef{
			ContextID: "codegraph-change-context",
			Kind:      "codegraph",
			Revision:  "commit-a",
			Digest:    contextDigest,
			Coverage: reviewcore.ContextCoverage{
				Spans: []reviewcore.ContextSpan{}, Symbols: []string{"Handle"},
			},
			Provenance: reviewcore.ContextProvenance{
				Provider: "codegraph", ProducerID: "argus-local",
				ProducerRevision: "v1",
			},
			ArtifactURI: "artifact://local/contexts/codegraph-change-context",
			Contract:    "argus.context.codegraph.v1alpha1",
			SizeBytes:   int64(len(contextBytes)),
		},
	}}
	inputData, err := json.Marshal(fixture.ReviewInput)
	if err != nil {
		t.Fatal(err)
	}
	inputDigest := shaHex(inputData)
	inputBinding := testArtifact(
		"review-input-context", inputDigest,
		contractsv1alpha1.AgentStagePlanReviewInputContract, int64(len(inputData)),
	)
	fixture.Plan.TargetDigest = inputDigest
	fixture.Plan.ReviewInput = inputBinding
	fixture.Plan = sealFormalPlan(t, fixture.Plan)
	fixture.Request.ReviewInput = inputBinding
	rebindFormalRequestPlan(t, &fixture)
	workerRequest, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		fixture.ModelProfile,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		nil,
		[][]byte{contextBytes},
		mustRulePackBytes(t, fixture.WorkerRequest),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(workerRequest.ContextArtifacts) != 1 ||
		workerRequest.ContextArtifacts[0].ContextID != "codegraph-change-context" ||
		workerRequest.ContextArtifacts[0].Artifact.Ref.SHA256 != contextDigest {
		t.Fatalf("context transports = %+v", workerRequest.ContextArtifacts)
	}
	if _, err := BuildFormalWorkerRequest(
		fixture.Plan,
		fixture.Request,
		fixture.ReviewInput,
		fixture.ModelProfile,
		mustPromptBundleBytes(t, fixture.WorkerRequest.PromptBundle),
		mustReviewSkillBytes(t, fixture.WorkerRequest.ReviewSkills),
		nil,
		nil,
		mustRulePackBytes(t, fixture.WorkerRequest),
	); err == nil || !strings.Contains(err.Error(), "context artifact bytes count") {
		t.Fatalf("missing context bytes error = %v", err)
	}
}

func TestPlanReviewDimensionByIDPreservesCustomArtifactRevision(t *testing.T) {
	want := contractsv1alpha1.VersionedRef{
		ID: "correctness", Revision: "sha256-custom", SHA256: testDigest("custom-review-skill"),
	}
	plan := contractsv1alpha1.AgentReviewPlan{ReviewDimensions: []contractsv1alpha1.VersionedRef{want}}

	got, ok := planReviewDimensionByID(plan, want.ID)
	if !ok || got != want {
		t.Fatalf("planReviewDimensionByID() = %+v, %t, want %+v", got, ok, want)
	}
	if _, ok := planReviewDimensionByID(plan, "outside-plan"); ok {
		t.Fatal("planReviewDimensionByID() admitted an outside-plan skill")
	}
}

func TestMapFormalReviewResultRejectsPromptArtifactMismatch(t *testing.T) {
	fixture := newFormalMapFixture(t)
	report, err := DecodePiReviewReport(fixture.WorkerResult.Report)
	if err != nil {
		t.Fatal(err)
	}
	report.Execution.Snapshot.PromptBundleDigest = strings.Repeat("f", 64)
	fixture.WorkerResult = formalWorkerResult(t, fixture.WorkerRequest, report)

	if _, err := MapFormalReviewResult(fixture); err == nil ||
		!strings.Contains(err.Error(), "prompt bundle digest") {
		t.Fatalf("MapFormalReviewResult() error = %v, want prompt binding rejection", err)
	}
}

func TestMapFormalReviewResultRejectsFalseCompleteTaskEvidenceCoverage(t *testing.T) {
	fixture := newFormalMapFixture(t)
	report, err := DecodePiReviewReport(fixture.WorkerResult.Report)
	if err != nil {
		t.Fatal(err)
	}
	report.Execution.TaskEvidence.Completeness = contractsv1alpha1.AgentReviewTaskEvidenceComplete
	report.Execution.TaskEvidence.ReasonCodes = []string{}
	fixture.WorkerResult = formalWorkerResult(t, fixture.WorkerRequest, report)

	if _, err := MapFormalReviewResult(fixture); err == nil ||
		!strings.Contains(err.Error(), "missing receipt task evidence requires task_evidence_unavailable") {
		t.Fatalf("MapFormalReviewResult() error = %v, want false-complete evidence rejection", err)
	}
}

func TestMapFormalReviewArtifactsAcceptsHostBoundResumedTaskWindow(t *testing.T) {
	fixture := newFormalMapFixture(t)
	report, completedAt := formalPiReport(
		t,
		fixture.WorkerRequest,
		fixture.ReviewInput,
		fixture.Plan.Prompt.Ref.SHA256,
		4,
	)
	freshGenerationStart := fixture.WorkerRequest.Plan.CreatedAt
	priorGenerationBound := freshGenerationStart.Add(-time.Minute)
	for index := range report.Execution.Tasks {
		task := &report.Execution.Tasks[index]
		if task.TaskKind == "verification" {
			continue
		}
		task.StartedAt = priorGenerationBound.Add(2 * time.Second)
		task.FinishedAt = priorGenerationBound.Add(3 * time.Second)
	}

	withoutOrigin := fixture
	withoutOrigin.WorkerResult = formalWorkerResultAt(
		t, withoutOrigin.WorkerRequest, report, completedAt,
	)
	if _, err := MapFormalReviewArtifacts(withoutOrigin); err == nil ||
		!strings.Contains(err.Error(), "timing is outside worker execution") {
		t.Fatalf("resumed observations without host-bound window error = %v", err)
	}

	fixture.WorkerRequest.Plan.CreatedAt = priorGenerationBound
	fixture.WorkerResult = formalWorkerResultAt(
		t, fixture.WorkerRequest, report, completedAt,
	)
	if _, err := MapFormalReviewArtifacts(fixture); err != nil {
		t.Fatalf("host-bound resumed task window rejected: %v", err)
	}
}

func TestMapFrozenPiAnchorCanonicalizesTargetSideAlias(t *testing.T) {
	fixture := newFormalMapFixture(t)
	anchor, _, err := mapFrozenPiAnchor(fixture.ReviewInput, piSourceAnchor{
		Path: "fixture.go", Side: "new", StartLine: 4, EndLine: 4,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if anchor.Side != contractsv1alpha1.HypothesisAnchorFile {
		t.Fatalf("scope target canonical anchor side = %q", anchor.Side)
	}
}

func TestMapFormalReviewResultFailsClosedForUnsupportedFormalInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*FormalMapInput)
		want   string
	}{
		{
			name: "knowledge transport substitution",
			mutate: func(input *FormalMapInput) {
				input.Plan.Knowledge = []contractsv1alpha1.AgentStageKnowledgeBinding{{
					ID: "business-rules",
					Ref: contractsv1alpha1.VersionedRef{
						ID: "business-rules", Revision: "v1", SHA256: testDigest("knowledge"),
					},
					Artifact: testArtifact("knowledge", testDigest("knowledge"), contractsv1alpha1.AgentStagePlanKnowledgeContract, 1),
				}}
				input.Plan = sealFormalPlan(t, input.Plan)
				rebindFormalRequestPlan(t, input)
			},
			want: "does not bind all formal knowledge",
		},
		{
			name: "review skill transport substitution",
			mutate: func(input *FormalMapInput) {
				for index := range input.Plan.Skills {
					if input.Plan.Skills[index].Phase == contractsv1alpha1.AgentStageSkillPhaseReview {
						input.Plan.Skills[index].Ref.ID = "business-review"
						input.Plan.Skills[index].Artifact.Ref.URI = "artifact://local/business-review"
					}
				}
				input.Plan = sealFormalPlan(t, input.Plan)
				rebindFormalRequestPlan(t, input)
				input.WorkerRequest.Plan.PlanID = input.Plan.PlanID
				input.WorkerRequest.Plan.ReviewDimensions[0] = input.Plan.Skills[2].Ref
			},
			want: "review_skills[0] ref does not match plan.review_dimensions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFormalMapFixture(t)
			test.mutate(&fixture)
			if _, err := MapFormalReviewResult(fixture); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("MapFormalReviewResult() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestMapFormalReviewResultRejectsRequestThatReferencesAnotherPlan(t *testing.T) {
	fixture := newFormalMapFixture(t)
	fixture.Request.Plan.Ref.SHA256 = testDigest("another-plan")
	request, err := contractsv1alpha1.SealStageExecutionRequest(fixture.Request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Request = request

	if _, err := MapFormalReviewResult(fixture); err == nil ||
		!strings.Contains(err.Error(), "exact AgentStagePlan bytes") {
		t.Fatalf("MapFormalReviewResult() error = %v, want exact plan rejection", err)
	}
}

func newFormalMapFixture(t *testing.T) FormalMapInput {
	t.Helper()
	createdAt := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	content := "package fixture\n\nfunc value(ptr *int) int {\n\treturn *ptr\n}\n"
	fileDigest := shaHex([]byte(content))
	reviewInput := reviewcore.ReviewInput{
		SchemaVersion:  reviewcore.ReviewInputSchemaVersion,
		TargetID:       "formal-target",
		TargetMode:     reviewcore.TargetModeScope,
		CanonicalPatch: "",
		Regions: []reviewcore.ReviewRegion{{
			Path: "fixture.go", StartLine: 1, EndLine: 5, SHA256: fileDigest,
		}},
		Files: []reviewcore.FileManifestEntry{{
			Path: "fixture.go", SHA256: fileDigest, SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []reviewcore.ContextBinding{},
	}
	targetDigest, err := reviewcore.DigestReviewInput(reviewInput)
	if err != nil {
		t.Fatal(err)
	}
	inputData, err := json.Marshal(reviewInput)
	if err != nil {
		t.Fatal(err)
	}

	implementationDigest := testDigest("pi-worker-implementation")
	promptBytes, err := contractsv1alpha1.MarshalDefaultAgentReviewPromptBundle()
	if err != nil {
		t.Fatal(err)
	}
	promptDigest := shaHex(promptBytes)
	reviewSkillBytes := []byte("# Correctness\n\nFind concrete correctness defects.\n")
	reviewSkillDigest := shaHex(reviewSkillBytes)
	modelProfile, err := contractsv1alpha1.NewModelProfile(
		"deepseek-anthropic",
		"deepseek-v4-pro[1m]",
	)
	if err != nil {
		t.Fatal(err)
	}
	modelProfileData, err := contractsv1alpha1.MarshalModelProfile(modelProfile)
	if err != nil {
		t.Fatal(err)
	}
	modelProfileDigest := shaHex(modelProfileData)
	modelComponentID := "deepseek-anthropic-model-" + modelProfileDigest[:16]
	rulePack, err := reviewconfig.SealRulePack("formal-rules", "1", []reviewconfig.RuleDefinition{{
		ID: "correctness", Revision: "1", Kind: "agent",
		Detector: reviewconfig.VersionedRef{
			ID: "pi-review", Revision: "1", SHA256: testDigest("pi-review-rule"),
		},
		Languages: []string{"go"}, PathPrefixes: []string{},
		EvidenceKinds: []string{"file_content", "target_line"},
		Severity:      "high", Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	rulePackData, err := json.Marshal(rulePack)
	if err != nil {
		t.Fatal(err)
	}
	plan := contractsv1alpha1.AgentStagePlan{
		ReviewRunID:                "formal-run",
		Stage:                      contractsv1alpha1.VersionedRef{ID: "agent_hypothesize", Revision: "1", SHA256: testDigest("stage")},
		TargetDigest:               targetDigest,
		BuildIdentity:              "pi-runtime-build",
		ExecutionSnapshot:          testArtifact("execution-snapshot", testDigest("snapshot"), contractsv1alpha1.AgentStagePlanExecutionSnapshotContract, 1),
		ConfigBundle:               testArtifact("config-bundle", testDigest("config"), contractsv1alpha1.AgentStagePlanConfigBundleContract, 1),
		ConfigResolutionReceiptRef: testArtifact("config-receipt", testDigest("config-receipt"), contractsv1alpha1.AgentStagePlanConfigResolutionReceiptContract, 1),
		Workflow:                   testArtifact("workflow", testDigest("workflow"), contractsv1alpha1.AgentStagePlanWorkflowContract, 1),
		ReviewSpec:                 testArtifact("review-spec", testDigest("review-spec"), contractsv1alpha1.AgentStagePlanReviewSpecContract, 1),
		ReviewInput:                testArtifact("review-input", targetDigest, contractsv1alpha1.AgentStagePlanReviewInputContract, int64(len(inputData))),
		AgentReviewPolicy:          contractsv1alpha1.VersionedRef{ID: "formal-review", Revision: "1", SHA256: testDigest("policy")},
		RulePack:                   contractsv1alpha1.VersionedRef{ID: rulePack.ID, Revision: rulePack.Revision, SHA256: rulePack.SHA256},
		Normalization: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationCurrentRevision,
			SHA256:   implementationDigest,
		},
		RulePackBase64: base64.StdEncoding.EncodeToString(rulePackData),
		Agent:          testComponent("pi-agent", "worker-v0", implementationDigest, contractsv1alpha1.AgentStagePlanAgentContract),
		Provider:       testComponent("deepseek-anthropic", "v1", testDigest("provider"), contractsv1alpha1.AgentStagePlanProviderContract),
		Model:          testComponent(modelComponentID, "provider", modelProfileDigest, contractsv1alpha1.AgentStagePlanModelContract),
		Runtime:        testComponent("node-runtime", "binary", testDigest("node-runtime"), contractsv1alpha1.AgentStagePlanRuntimeContract),
		Prompt:         testComponent("pi-review-prompts", "v0", promptDigest, contractsv1alpha1.AgentStagePlanPromptContract),
		Skills: []contractsv1alpha1.AgentStageSkillBinding{
			testSkill(contractsv1alpha1.AgentStageSkillPhaseGrouping, "grouping", "directory-language", "v0", implementationDigest),
			testSkill(contractsv1alpha1.AgentStageSkillPhaseContext, "context", "code-context", "v0", implementationDigest),
			testSkill(contractsv1alpha1.AgentStageSkillPhaseReview, "review-correctness", "correctness", "builtin-v1", reviewSkillDigest),
			testSkill(contractsv1alpha1.AgentStageSkillPhaseVerification, "verification", "independent-verifier", "v0", implementationDigest),
		},
		Knowledge:        []contractsv1alpha1.AgentStageKnowledgeBinding{},
		ContextProviders: []contractsv1alpha1.AgentStageContextProviderBinding{},
		ModelAuthority: contractsv1alpha1.AgentStageModelAuthority{
			ModelEgress: contractsv1alpha1.AgentStageModelEgressProviderBrokerOnly,
			APIProtocol: testComponent("anthropic-messages", "2023-06-01", testDigest("anthropic-protocol"), contractsv1alpha1.AgentStagePlanAPIProtocolContract),
		},
		ToolAuthority: contractsv1alpha1.AgentStageToolAuthority{
			Tools:           append([]string{}, frozenWorkerTools...),
			ToolNetwork:     contractsv1alpha1.AgentStageSideEffectsDeny,
			WorkspaceReads:  contractsv1alpha1.AgentStageWorkspaceReadFrozenInputOnly,
			WorkspaceWrites: contractsv1alpha1.AgentStageSideEffectsDeny,
			RemoteWrites:    contractsv1alpha1.AgentStageSideEffectsDeny,
		},
		Budget: contractsv1alpha1.AgentStageBudget{
			MaxFiles: 10, MaxGroups: 4, MaxHypotheses: 10, MaxModelCalls: 20,
			MaxToolCalls: 10, MaxTargetBytes: 1 << 20, MaxGroupBytes: 1 << 18,
			MaxOutputBytes: 1 << 20, MaxOutputTokens: 4096, MaxCostMicros: 1_000_000,
			TimeoutMS: 60_000, MaxConcurrency: 2,
		},
		OutputContract: contractsv1alpha1.AgentStagePlanOutputContract,
		Disposition:    contractsv1alpha1.AgentStageDispositionHypothesisOnly,
		SideEffects:    contractsv1alpha1.AgentStageSideEffectsDeny,
	}
	plan.Prompt.Artifact.Ref.SizeBytes = int64(len(promptBytes))
	plan.Model.Artifact.Ref.SizeBytes = int64(len(modelProfileData))
	plan.Skills[2].Artifact.Ref.SizeBytes = int64(len(reviewSkillBytes))
	plan = sealFormalPlan(t, plan)

	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind: "local_process", RuntimeID: plan.Runtime.Ref.ID,
		RuntimeRevision: plan.Runtime.Ref.Revision, RuntimeSHA256: plan.Runtime.Ref.SHA256,
		BuildIdentity: plan.BuildIdentity,
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:    append([]string{}, plan.ToolAuthority.Tools...),
			ModelEgress:     plan.ModelAuthority.ModelEgress,
			ToolNetwork:     plan.ToolAuthority.ToolNetwork,
			WorkspaceReads:  plan.ToolAuthority.WorkspaceReads,
			WorkspaceWrites: plan.ToolAuthority.WorkspaceWrites,
			RemoteWrites:    plan.ToolAuthority.RemoteWrites,
		},
		Trust: contractsv1alpha1.ExecutorTrust{
			Authority: contractsv1alpha1.ExecutorTrustAuthorityLocalHost,
			CapabilityVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-capability-verifier", Revision: "1", SHA256: testDigest("capability-verifier"),
			},
			CallbackVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-callback-verifier", Revision: "1", SHA256: testDigest("callback-verifier"),
			},
		},
	}
	capability.SHA256, err = contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	planData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	request := contractsv1alpha1.StageExecutionRequest{
		SchemaVersion: contractsv1alpha1.StageExecutionRequestSchemaVersion,
		ExecutionID:   "formal-execution", ReviewRunID: plan.ReviewRunID,
		TenantID: "tenant", WorkspaceID: "workspace", WorkloadID: "workload",
		LeaseID: "lease", LeaseWorkerID: "worker", Stage: plan.Stage,
		Attempt: 1, Generation: 1, FencingToken: 7, IdempotencyKey: "formal-idempotency",
		Plan:              testArtifact("formal-plan", shaHex(planData), contractsv1alpha1.AgentStagePlanSchemaVersion, int64(len(planData))),
		ExecutionSnapshot: plan.ExecutionSnapshot, ReviewInput: plan.ReviewInput,
		Upstream: []contractsv1alpha1.ArtifactBinding{}, OutputContract: plan.OutputContract,
		Capability: capability, Deadline: createdAt.Add(time.Minute), SideEffects: plan.SideEffects,
	}
	request, err = contractsv1alpha1.SealStageExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}

	workerPlan := contractsv1alpha1.AgentReviewPlan{
		SchemaVersion: contractsv1alpha1.AgentReviewPlanSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.ReviewRunID, ExecutionID: request.ExecutionID,
		ReviewRunID: plan.ReviewRunID, ExecutionSnapshotRef: request.ExecutionSnapshot,
		ReviewInputRef: request.ReviewInput, TargetDigest: plan.TargetDigest,
		RulePack:       plan.RulePack,
		Implementation: contractsv1alpha1.VersionedRef{ID: "pi-review-worker", Revision: "v0", SHA256: implementationDigest},
		Normalization:  plan.Normalization,
		Grouping:       plan.Skills[0].Ref, ContextDimensions: []contractsv1alpha1.VersionedRef{plan.Skills[1].Ref},
		ReviewDimensions: []contractsv1alpha1.VersionedRef{plan.Skills[2].Ref}, Verifier: plan.Skills[3].Ref,
		Knowledge: []contractsv1alpha1.VersionedRef{}, Runtime: plan.Runtime.Ref,
		Profile: contractsv1alpha1.VersionedRef{ID: "local-shadow-stdio", Revision: "v1", SHA256: testDigest("profile")},
		Agent:   plan.Agent.Ref, Provider: plan.Provider.Ref,
		Model: contractsv1alpha1.VersionedRef{
			ID: modelProfile.WireModel, Revision: plan.Model.Ref.Revision,
			SHA256: plan.Model.Ref.SHA256,
		},
		APIProtocol: contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
		ToolPolicy: contractsv1alpha1.AgentReviewToolPolicy{
			AllowedTools: append([]string{}, frozenWorkerTools...), ToolNetwork: "deny", WorkspaceWrites: "deny", RemoteWrites: "deny",
		},
		Budget: formalWorkerBudget(plan.Budget), VerificationRequired: true,
		ExecutionClass: contractsv1alpha1.AgentReviewExecutionLocalDirectProviderShadow,
		Attestation:    contractsv1alpha1.AgentReviewAttestationNonAttested,
		SideEffects:    "deny", CreatedAt: createdAt,
	}
	workerCapability := contractsv1alpha1.AgentReviewWorkerCapability{
		Protocol:       contractsv1alpha1.AgentReviewWorkerProtocolStdio,
		FrozenInput:    contractsv1alpha1.AgentReviewWorkerFrozenInputBase64,
		MaxInputBytes:  uint64(plan.Budget.MaxTargetBytes),
		MaxOutputBytes: uint64(plan.Budget.MaxOutputBytes),
	}
	workerCapability.SHA256, err = contractsv1alpha1.DigestAgentReviewWorkerCapability(workerCapability)
	if err != nil {
		t.Fatal(err)
	}
	promptBundle, err := contractsv1alpha1.NewAgentReviewWorkerPromptBundle(
		plan.Prompt.Ref,
		plan.Prompt.Artifact,
		promptBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	reviewSkill, err := contractsv1alpha1.NewAgentReviewWorkerSkill(
		plan.Skills[2].Ref,
		plan.Skills[2].Artifact,
		reviewSkillBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	workerRequest := contractsv1alpha1.AgentReviewWorkerRequest{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerRequestSchemaVersion,
		WorkItemID:    request.ExecutionID, Attempt: request.Attempt, Generation: request.Generation,
		FencingToken: request.FencingToken, IdempotencyKey: request.IdempotencyKey,
		Capability:       workerCapability,
		Deadline:         mustFormalWorkerDeadline(t, workerPlan.CreatedAt, request.Deadline),
		Plan:             workerPlan,
		RulePackBase64:   base64.StdEncoding.EncodeToString(rulePackData),
		PromptBundle:     promptBundle,
		ReviewSkills:     []contractsv1alpha1.AgentReviewWorkerSkill{reviewSkill},
		KnowledgePacks:   []contractsv1alpha1.AgentReviewWorkerKnowledge{},
		ContextArtifacts: []contractsv1alpha1.AgentReviewWorkerContextArtifact{},
		CheckpointScopeSHA256: digestBytesRaw([]byte(
			request.Plan.Ref.SHA256 + "\n" + request.ReviewInput.Ref.SHA256,
		)),
		GroupCheckpoints:  []contractsv1alpha1.AgentReviewWorkerGroupCheckpoint{},
		ReviewInputBase64: base64.StdEncoding.EncodeToString(inputData),
	}
	if err := workerRequest.Validate(); err != nil {
		t.Fatal(err)
	}
	report, completedAt := formalPiReport(t, workerRequest, reviewInput, promptDigest, 4)
	workerResult := formalWorkerResultAt(t, workerRequest, report, completedAt)
	return FormalMapInput{
		Plan: plan, Request: request, ReviewInput: reviewInput,
		ModelProfile:  modelProfileData,
		WorkerRequest: workerRequest, WorkerResult: workerResult,
	}
}

func mustFormalWorkerDeadline(t *testing.T, createdAt, stageDeadline time.Time) time.Time {
	t.Helper()
	deadline, err := formalWorkerDeadline(createdAt, stageDeadline)
	if err != nil {
		t.Fatal(err)
	}
	return deadline
}

func mustReviewSkillBytes(
	t *testing.T,
	transports []contractsv1alpha1.AgentReviewWorkerSkill,
) [][]byte {
	t.Helper()
	contents := make([][]byte, len(transports))
	for index, transport := range transports {
		content, err := contractsv1alpha1.DecodeAgentReviewWorkerSkill(transport)
		if err != nil {
			t.Fatal(err)
		}
		contents[index] = content
	}
	return contents
}

func mustRulePackBytes(
	t *testing.T,
	request contractsv1alpha1.AgentReviewWorkerRequest,
) []byte {
	t.Helper()
	content, err := contractsv1alpha1.DecodeAgentReviewWorkerRulePack(request)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func mustPromptBundleBytes(
	t *testing.T,
	transport contractsv1alpha1.AgentReviewWorkerPromptBundle,
) []byte {
	t.Helper()
	_, content, err := contractsv1alpha1.DecodeAgentReviewWorkerPromptBundle(transport)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func sealFormalPlan(t *testing.T, plan contractsv1alpha1.AgentStagePlan) contractsv1alpha1.AgentStagePlan {
	t.Helper()
	sealed, err := contractsv1alpha1.SealAgentStagePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func rebindFormalRequestPlan(t *testing.T, input *FormalMapInput) {
	t.Helper()
	data, err := json.Marshal(input.Plan)
	if err != nil {
		t.Fatal(err)
	}
	input.Request.Plan.Ref.SHA256 = shaHex(data)
	input.Request.Plan.Ref.SizeBytes = int64(len(data))
	input.Request, err = contractsv1alpha1.SealStageExecutionRequest(input.Request)
	if err != nil {
		t.Fatal(err)
	}
}

func testArtifact(name, digest, contract string, size int64) contractsv1alpha1.ArtifactBinding {
	return contractsv1alpha1.ArtifactBinding{
		Ref:      contractsv1alpha1.ContentRef{URI: "artifact://local/" + name, SHA256: digest, SizeBytes: size},
		Contract: contract,
	}
}

func testComponent(id, revision, digest, contract string) contractsv1alpha1.AgentStageComponentBinding {
	return contractsv1alpha1.AgentStageComponentBinding{
		Ref:      contractsv1alpha1.VersionedRef{ID: id, Revision: revision, SHA256: digest},
		Artifact: testArtifact(id, digest, contract, 1),
	}
}

func testSkill(phase contractsv1alpha1.AgentStageSkillPhase, id, refID, revision, digest string) contractsv1alpha1.AgentStageSkillBinding {
	component := testComponent(refID, revision, digest, contractsv1alpha1.AgentStagePlanSkillContract)
	return contractsv1alpha1.AgentStageSkillBinding{ID: id, Phase: phase, Ref: component.Ref, Artifact: component.Artifact}
}

func testDigest(value string) string { return shaHex([]byte(value)) }

func shaHex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func formalWorkerResult(t *testing.T, request contractsv1alpha1.AgentReviewWorkerRequest, report piReviewReport) contractsv1alpha1.AgentReviewWorkerResult {
	t.Helper()
	return formalWorkerResultAt(t, request, report, request.Plan.CreatedAt.Add(10*time.Second))
}

func formalWorkerResultAt(t *testing.T, request contractsv1alpha1.AgentReviewWorkerRequest, report piReviewReport, completedAt time.Time) contractsv1alpha1.AgentReviewWorkerResult {
	t.Helper()
	reportData, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest := shaHex(reportData)
	result := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion: contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:    request.WorkItemID, Attempt: request.Attempt, Generation: request.Generation,
		FencingToken: request.FencingToken, IdempotencyKey: request.IdempotencyKey,
		CapabilitySHA256: request.Capability.SHA256, Status: contractsv1alpha1.AgentReviewWorkerSucceeded,
		ReportSHA256: &reportDigest, Report: reportData, CompletedAt: completedAt,
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	return result
}

func formalPiReport(t *testing.T, request contractsv1alpha1.AgentReviewWorkerRequest, input reviewcore.ReviewInput, promptDigest string, anchorLine uint32) (piReviewReport, time.Time) {
	t.Helper()
	patch, status, changedLines, err := frozenPiFilePatch(input, input.Files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := request.Plan.CreatedAt
	generatedAt := createdAt.Add(time.Second)
	completedAt := createdAt.Add(10 * time.Second)
	patchDigest := "sha256:" + digestText(patch)
	targetDigest := "sha256:" + input.Files[0].SHA256
	canonicalPatchDigest := "sha256:" + digestText(input.CanonicalPatch)
	groupID := "group-001-fixture"
	group := piSnapshotGroup{ID: groupID, Key: "internal:.go", PatchDigest: digestText(patch), Files: []string{input.Files[0].Path}}
	skills := []piSnapshotSkill{{
		ID:       request.Plan.ReviewDimensions[0].ID,
		Revision: request.Plan.ReviewDimensions[0].Revision,
		Digest:   "sha256:" + request.Plan.ReviewDimensions[0].SHA256,
		Bytes:    1,
	}}
	knowledge := make([]piSnapshotKnowledge, len(request.Plan.Knowledge))
	for index, ref := range request.Plan.Knowledge {
		knowledge[index] = piSnapshotKnowledge{
			ID: ref.ID, Digest: "sha256:" + ref.SHA256,
			Bytes: uint64(request.KnowledgePacks[index].Artifact.Ref.SizeBytes),
		}
	}
	providerProfile, err := piProfileForProvider(request.Plan.Provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	report := piReviewReport{
		SchemaVersion: piReviewReportSchemaVersion, Status: "complete",
		Provider: request.Plan.Provider.ID, ProviderProfile: providerProfile, Model: request.Plan.Model.ID,
		Target: piTarget{
			Kind: "files", Repository: "memory://review-input/" + jsEncodeURIComponent(input.TargetID),
			Digest: request.Plan.TargetDigest, CapturedAt: generatedAt, GeneratedAt: &generatedAt,
			CanonicalPatchDigest: &canonicalPatchDigest,
			Files:                []piTargetFile{{Path: input.Files[0].Path, Status: status, Digest: input.Files[0].SHA256, ChangedLines: changedLines, TargetDigest: &targetDigest, PatchDigest: &patchDigest}},
			Skipped:              []piSkipped{},
		},
		Coverage: piCoverage{
			GroupsTotal: 1, GroupsReviewed: 1, ReviewTasksTotal: 1, ReviewTasksSucceeded: 1,
			VerificationEnabled: true, VerificationTasksTotal: 1, VerificationTasksSucceeded: 1,
			FilesIncluded: 1, Skipped: []piSkipped{}, ContextGaps: []string{}, Failures: []piFailure{},
		},
		Summary:  piSummary{Candidates: 1, Confirmed: 1},
		Findings: []piCandidate{}, Candidates: []piCandidate{}, RawCandidates: []piRawCandidate{},
		NormalizationDecisions: []piNormalizationDecision{},
		Execution: piExecutionEnvelope{
			Authority: "diagnostic_only", ProvenanceClass: "worker_self_report",
			TaskEvidence: piTaskEvidenceCollection{
				SchemaVersion: "argus.pi-review.task_evidence.v0",
				Authority:     "diagnostic_only", ProvenanceClass: "worker_self_report",
				ContentPolicy: "exact_local_sensitive", Completeness: "partial",
				ReasonCodes: []string{"task_evidence_unavailable"}, Tasks: []piTaskExecutionEvidence{},
			},
			Snapshot: piExecutionSnapshot{
				SchemaVersion: "argus.pi-review.execution_snapshot.v0", SnapshotDigest: "sha256:" + testDigest("snapshot-report"), CreatedAt: createdAt,
				Target:               piSnapshotTarget{Kind: "files", Digest: request.Plan.TargetDigest},
				WorkflowRevision:     "argus-pi-review-workflow-v2",
				PromptBundleRevision: "v0", PromptBundleDigest: promptDigest,
				Grouping: piSnapshotGrouping{ImplementationRevision: "directory-language-v0", Groups: []piSnapshotGroup{group}},
				Skills:   skills, Knowledge: knowledge,
				Runtime:    piSnapshotRuntime{Implementation: "@argus/pi-review", Version: "0.1.0", Node: "v24", PiAgentCore: "0.84.1", PiAI: "0.84.1"},
				Provider:   piSnapshotProvider{Provider: request.Plan.Provider.ID, Profile: providerProfile, Protocol: "anthropic-messages", Model: request.Plan.Model.ID},
				ToolPolicy: piSnapshotToolPolicy{Mode: "read_only", AllowedTools: append([]string{}, request.Plan.ToolPolicy.AllowedTools...)},
				Budgets: piSnapshotBudgets{
					Concurrency: request.Plan.Budget.MaxConcurrency, MaxToolCallsPerTask: request.Plan.Budget.MaxToolCalls,
					MaxFiles: request.Plan.Budget.MaxFiles, MaxGroups: request.Plan.Budget.MaxGroups,
					MaxGroupBytes: request.Plan.Budget.MaxGroupBytes, MaxTargetBytes: request.Plan.Budget.MaxTargetBytes,
					MaxCandidates: request.Plan.Budget.MaxCandidates, MaxProviderTurns: request.Plan.Budget.MaxModelCalls,
					MaxOutputTokensPerTurn: request.Plan.Budget.MaxOutputTokens, TaskTimeoutMS: request.Plan.Budget.TimeoutMS,
				},
				VerificationPolicy: "independent_required",
				Replayability:      piReplayability{Status: "non_replayable", Reasons: []string{"direct_provider_execution_not_platform_attested"}},
			},
			Tasks: []piTaskObservation{}, Usage: observedFormalZeroUsage(),
		},
	}
	report.Execution.Tasks = append(report.Execution.Tasks,
		formalSucceededTask(createdAt, groupID, "context", nil, nil, "submit_context", testDigest("context-output")),
	)
	skillID := request.Plan.ReviewDimensions[0].ID
	report.Execution.Tasks = append(report.Execution.Tasks,
		formalSucceededTask(createdAt, groupID, "review", &skillID, nil, "submit_candidates", testDigest("review-output")),
	)
	claim := piCandidateClaim{
		Category: "correctness", Severity: "high", Title: "possible nil dereference",
		Description: "value can be nil", Impact: "request panic",
		Anchor: piSourceAnchor{Path: input.Files[0].Path, Side: "file", StartLine: anchorLine, EndLine: anchorLine},
		Evidence: []piEvidence{{
			Statement: "the dereference occurs on this source line",
			Anchor:    piSourceAnchor{Path: input.Files[0].Path, Side: "file", StartLine: anchorLine, EndLine: anchorLine},
			Excerpt:   "return *ptr",
		}},
	}
	fingerprint, err := piClusterFingerprint(claim)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := "candidate-" + fingerprint[:16]
	skill := piSkillRef{ID: skillID, Revision: request.Plan.ReviewDimensions[0].Revision}
	raw := piRawCandidate{RawCandidateID: "raw-fixture-1", GroupID: groupID, Skill: skill, Ordinal: 0, Claim: claim}
	verification := piVerification{
		CandidateID: candidateID, Verdict: "confirmed", ReasonCode: "path_reachable",
		Explanation: "the frozen line contains the dereference", Evidence: claim.Evidence,
	}
	candidate := piCandidate{
		PiCandidateClaim: claim, ID: candidateID, Fingerprint: fingerprint,
		GroupID: groupID, Skill: skill, Verification: &verification,
	}
	report.RawCandidates = []piRawCandidate{raw}
	report.NormalizationDecisions = []piNormalizationDecision{{
		RawCandidateID: raw.RawCandidateID, Action: "retained", ReasonCode: "normalized_candidate_retained", NormalizedCandidateID: &candidateID,
	}}
	report.Candidates = []piCandidate{candidate}
	report.Findings = []piCandidate{candidate}
	verificationData, err := json.Marshal(verification)
	if err != nil {
		t.Fatal(err)
	}
	report.Execution.Tasks = append(report.Execution.Tasks,
		formalSucceededTask(createdAt, groupID, "verification", nil, &candidateID, "submit_verdict", digestBytesRaw(verificationData)),
	)
	return report, completedAt
}

func formalSucceededTask(createdAt time.Time, groupID, kind string, skillID, candidateID *string, tool, outputDigest string) piTaskObservation {
	identity := kind
	if skillID != nil {
		identity += "-" + *skillID
	}
	output := "sha256:" + outputDigest
	return piTaskObservation{
		TaskID: "agent-task-" + identity, TaskKind: kind, GroupID: groupID,
		SkillID: skillID, CandidateID: candidateID, PromptDigest: "sha256:" + testDigest("task-prompt"),
		ProviderTurnsStarted: 1, ProviderTurnsCompleted: 1, ToolCalls: 1,
		ToolNames: []string{tool}, ToolUsage: []piToolUsage{{ToolID: tool, InvocationCount: 1}},
		Usage: observedFormalZeroUsage(), StartedAt: createdAt.Add(2 * time.Second), FinishedAt: createdAt.Add(3 * time.Second),
		DurationMS: 1000, TerminalStatus: "succeeded", OutputDigest: &output,
	}
}

func observedFormalZeroUsage() piUsage {
	zero := uint64(0)
	return piUsage{
		Completeness: "provider_reported", InputTokens: &zero, OutputTokens: &zero,
		CacheReadTokens: &zero, CacheWriteTokens: &zero, TotalTokens: &zero,
	}
}
