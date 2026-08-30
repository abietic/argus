package runmodel

import (
	"strings"
	"testing"
	"time"
)

const modelTestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestExecutionSnapshotValidate(t *testing.T) {
	snapshot := validExecutionSnapshot()
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	snapshot.ToolPolicy.Network = "allow"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() accepted network authority")
	}
}

func TestReviewRunValidateRequiresReplayLineage(t *testing.T) {
	run := validReviewRun()
	run.Kind = RunKindReplay
	if err := run.Validate(); err == nil {
		t.Fatal("Validate() accepted replay without lineage")
	}
	run.SourceRunID = "source-1"
	run.ReplayFromStage = "verify"
	run.ReplayRootRunID = "source-1"
	run.ReplayNamespace = "replay-1"
	run.ReplayVariable = ReplayVariableNone
	if err := run.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestReviewRunScopesAgentExecutionReceiptsToSucceededFormalRuns(t *testing.T) {
	now := time.Now().UTC()
	deterministic := succeededReviewRun(now)
	receiptRef := artifactRef(ContractAgentExecutionReceipts)
	deterministic.AgentExecutionReceiptRef = &receiptRef
	if err := deterministic.Validate(); err == nil ||
		!strings.Contains(err.Error(), "exactly one deterministic or formal") {
		t.Fatalf("Validate() deterministic receipt error = %v", err)
	}

	formal := succeededReviewRun(now)
	formal.FindingSetRef = nil
	formal.JSONReportRef = nil
	formal.MarkdownReportRef = nil
	hypotheses := artifactRef(ContractReviewHypothesisSet)
	candidates := artifactRef(ContractGovernedCandidateSet)
	verification := artifactRef(ContractCandidateVerificationLedger)
	calibration := artifactRef(ContractFindingCalibrationLedger)
	suppression := artifactRef(ContractFindingSuppressionLedger)
	report := artifactRef(ContractGovernedReviewReport)
	markdown := artifactRef(ContractGovernedReviewMarkdown)
	formal.HypothesisSetRef = &hypotheses
	formal.AgentExecutionReceiptRef = &receiptRef
	formal.CandidateSetRef = &candidates
	formal.VerificationLedgerRef = &verification
	formal.CalibrationLedgerRef = &calibration
	formal.SuppressionLedgerRef = &suppression
	formal.GovernedReportRef = &report
	formal.GovernedMarkdownRef = &markdown
	if err := formal.Validate(); err != nil {
		t.Fatalf("Validate() formal receipt error = %v", err)
	}
	missingGovernance := formal
	missingGovernance.CalibrationLedgerRef = nil
	if err := missingGovernance.Validate(); err == nil ||
		!strings.Contains(err.Error(), "exactly one deterministic or formal") {
		t.Fatalf("Validate() accepted formal run without calibration ledger: %v", err)
	}
}

func TestReplayChangeSetRequiresExactlyDeclaredDifference(t *testing.T) {
	now := time.Now().UTC()
	change := ReplayChangeSet{
		SchemaVersion:  ReplayChangeSetSchemaVersion,
		Namespace:      "replay-1",
		SourceRunID:    "run-1",
		RootRunID:      "run-1",
		StartStage:     "verify",
		Variable:       ReplayVariableRulePack,
		BaselineSHA256: modelTestDigest,
		VariantSHA256:  strings.Repeat("b", 64),
		ChangedFields:  []string{"rule_pack.rules"},
		RemoteWrites:   "deny",
		CreatedAt:      now,
	}
	if err := change.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	change.ChangedFields = []string{"verification.policy", "rule_pack.rules"}
	if err := change.Validate(); err == nil {
		t.Fatal("Validate() accepted unsorted changed fields")
	}
	change.ChangedFields = []string{}
	if err := change.Validate(); err == nil {
		t.Fatal("Validate() accepted a variant without changed fields")
	}
}

func TestRunEvidenceRequiresExactBinding(t *testing.T) {
	run := validReviewRun()
	run.Evidence = []RunEvidence{{
		EvidenceID:        "evidence-1",
		StageID:           "detect",
		Attempt:           1,
		Generation:        1,
		IdempotencyKey:    "run-1-detect-1",
		ArtifactRef:       testArtifactRef(),
		Completeness:      CompletenessComplete,
		CompletenessNotes: []string{},
		RecordedAt:        time.Now().UTC(),
	}}
	if err := run.Validate(); err == nil || !strings.Contains(err.Error(), "binding_id") {
		t.Fatalf("Validate() error = %v, want binding rejection", err)
	}
}

func TestRunEvidenceMustMatchKnownAttempt(t *testing.T) {
	now := time.Now().UTC()
	run := validReviewRun()
	inputRef := artifactRef(ContractReviewInput)
	outputRef := artifactRef(ContractStageResult)
	run.StageAttempts = []StageAttempt{{
		StageID: "detect", Attempt: 1, Generation: 1, BindingID: "binding-1",
		Status: StageStatusSucceeded, InputRefs: []ArtifactRef{inputRef},
		OutputRef: &outputRef, StartedAt: now,
		FinishedAt: &now,
	}}
	run.Bindings = []PlatformExecutionBinding{
		testBinding(now, "binding-1", "detect", "key-1", 1),
	}
	run.Evidence = []RunEvidence{{
		EvidenceID: "evidence-1", BindingID: "binding-1", StageID: "verify",
		Attempt: 1, Generation: 1, IdempotencyKey: "key-1", FencingToken: 1,
		ArtifactRef: outputRef, Completeness: CompletenessComplete,
		CompletenessNotes: []string{}, RecordedAt: now,
	}}
	if err := run.Validate(); err == nil || !strings.Contains(err.Error(), "exact stage attempt") {
		t.Fatalf("Validate() error = %v, want exact-attempt rejection", err)
	}
}

func TestRunEvidenceRejectsDuplicateBinding(t *testing.T) {
	now := time.Now().UTC()
	run := validReviewRun()
	run.Status = RunStatusSucceeded
	run.StartedAt = &now
	run.CompletedAt = &now
	inputRef := artifactRef(ContractReviewInput)
	outputRef := artifactRef(ContractStageResult)
	findingRef := artifactRef(ContractFindingSet)
	jsonRef := artifactRef(ContractJSONReport)
	markdownRef := artifactRef(ContractMarkdownReport)
	run.FindingSetRef = &findingRef
	run.JSONReportRef = &jsonRef
	run.MarkdownReportRef = &markdownRef
	run.StageAttempts = []StageAttempt{{
		StageID: "detect", Attempt: 1, Generation: 1, BindingID: "binding-1",
		Status: StageStatusSucceeded, InputRefs: []ArtifactRef{inputRef},
		OutputRef: &outputRef, StartedAt: now, FinishedAt: &now,
	}}
	run.Bindings = []PlatformExecutionBinding{
		testBinding(now, "binding-1", "detect", "key-1", 1),
	}
	run.Evidence = []RunEvidence{
		{
			EvidenceID: "evidence-1", BindingID: "binding-1", StageID: "detect",
			Attempt: 1, Generation: 1, IdempotencyKey: "key-1", FencingToken: 1,
			ArtifactRef: outputRef, Completeness: CompletenessComplete,
			CompletenessNotes: []string{}, RecordedAt: now,
		},
		{
			EvidenceID: "evidence-2", BindingID: "binding-1", StageID: "detect",
			Attempt: 1, Generation: 1, IdempotencyKey: "key-1", FencingToken: 1,
			ArtifactRef: outputRef, Completeness: CompletenessComplete,
			CompletenessNotes: []string{}, RecordedAt: now,
		},
	}
	if err := run.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate binding") {
		t.Fatalf("Validate() error = %v, want duplicate-binding rejection", err)
	}
}

func TestArtifactRefRejectsTraversal(t *testing.T) {
	ref := artifactRef("argus.test.v1")
	ref.URI = "artifact://local/../secret"
	if err := ref.Validate(); err == nil {
		t.Fatal("Validate() accepted traversal URI")
	}
}

func TestArtifactRefRequiresURIToMatchDigest(t *testing.T) {
	ref := artifactRef("argus.test.v1")
	ref.URI = "artifact://local/sha256/" + strings.Repeat("b", 64)
	if err := ref.Validate(); err == nil || !strings.Contains(err.Error(), "does not match digest") {
		t.Fatalf("Validate() error = %v, want digest mismatch", err)
	}
}

func TestReviewRunRejectsWrongEvidenceFencing(t *testing.T) {
	now := time.Now().UTC()
	run := succeededReviewRun(now)
	run.Evidence[0].FencingToken++
	if err := run.Validate(); err == nil || !strings.Contains(err.Error(), "fencing") {
		t.Fatalf("Validate() error = %v, want fencing mismatch", err)
	}
}

func TestSucceededReviewRunPreservesFailedRetryHistory(t *testing.T) {
	now := time.Now().UTC()
	run := succeededReviewRun(now)
	inputRef := artifactRef(ContractReviewInput)
	failedFinished := now.Add(-time.Second)
	failed := StageAttempt{
		StageID: "detect", Attempt: 1, Generation: 1, BindingID: "binding-old",
		Status: StageStatusFailed, InputRefs: []ArtifactRef{inputRef},
		StartedAt: now.Add(-2 * time.Second), FinishedAt: &failedFinished,
		ErrorCode: "transient", ErrorMessage: "retry", Retryable: true,
	}
	run.StageAttempts[0].Attempt = 2
	run.StageAttempts[0].Generation = 2
	run.StageAttempts[0].BindingID = "binding-2"
	run.Bindings[0].BindingID = "binding-2"
	run.Bindings[0].Attempt = 2
	run.Bindings[0].Generation = 2
	run.Bindings[0].FencingToken = 2
	run.Evidence[0].BindingID = "binding-2"
	run.Evidence[0].Attempt = 2
	run.Evidence[0].Generation = 2
	run.Evidence[0].FencingToken = 2
	run.StageAttempts = append([]StageAttempt{failed}, run.StageAttempts...)
	run.Bindings = append([]PlatformExecutionBinding{
		testBinding(now.Add(-2*time.Second), "binding-old", "detect", "key-old", 1),
	}, run.Bindings...)
	if err := run.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestDigestJSONIsStableAndSensitive(t *testing.T) {
	first, err := DigestJSON(map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("DigestJSON() error = %v", err)
	}
	second, err := DigestJSON(map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("DigestJSON() error = %v", err)
	}
	changed, err := DigestJSON(map[string]string{"key": "other"})
	if err != nil {
		t.Fatalf("DigestJSON() error = %v", err)
	}
	if first != second || first == changed {
		t.Fatalf("digest stability = %q/%q, changed = %q", first, second, changed)
	}
}

func validExecutionSnapshot() ExecutionSnapshot {
	return ExecutionSnapshot{
		SchemaVersion:         SnapshotSchemaVersion,
		ExecutionSnapshotID:   "snapshot-1",
		ReviewSpecSHA256:      modelTestDigest,
		ReviewSpecRef:         artifactRef(ContractReviewSpec),
		TargetSnapshotRef:     artifactRef(ContractMaterializedTarget),
		ReviewInputRef:        artifactRef(ContractReviewInput),
		ReplayInputRefs:       []ArtifactRef{},
		WorkflowDefinitionRef: artifactRef(ContractWorkflowDefinition),
		ConfigBundleRef:       artifactRef(ContractConfigBundle),
		Workflow: WorkflowRef{
			ID: "default-review", Revision: "1", SHA256: modelTestDigest,
		},
		Config: PolicyRef{
			ID: "local-default", Revision: "1", SHA256: modelTestDigest,
		},
		RuntimeProfile: "deterministic-local-v1",
		BuildIdentity:  "argus-dev",
		ToolPolicy: ToolInvocationPolicy{
			AllowedTools:       []string{"git-read"},
			Network:            "deny",
			WorkspaceWrites:    "deny",
			RemoteWrites:       "deny",
			PerCallTimeoutMS:   30_000,
			MaxOutputBytes:     16 << 20,
			MaxConcurrency:     1,
			MaxDelegationDepth: 0,
		},
		RemoteWrites: "deny",
		CreatedAt:    time.Now().UTC(),
	}
}

func validReviewRun() ReviewRun {
	return ReviewRun{
		SchemaVersion:       RunSchemaVersion,
		RunID:               "run-1",
		Kind:                RunKindReview,
		RepositoryPath:      "/tmp/repository",
		TargetMode:          TargetModeDiff,
		BaseRevision:        "base",
		HeadRevision:        "head",
		Status:              RunStatusPending,
		ExecutionSnapshotID: "snapshot-1",
		TargetSnapshotRef:   artifactRef(ContractMaterializedTarget),
		StageAttempts:       []StageAttempt{},
		Bindings:            []PlatformExecutionBinding{},
		Evidence:            []RunEvidence{},
		CreatedAt:           time.Now().UTC(),
	}
}

func artifactRef(contract string) ArtifactRef {
	return ArtifactRef{
		URI:       "artifact://local/sha256/" + modelTestDigest,
		SHA256:    modelTestDigest,
		SizeBytes: 1,
		Contract:  contract,
	}
}

func testArtifactRef() ArtifactRef {
	return artifactRef("argus.test.v1")
}

func testBinding(
	now time.Time,
	bindingID string,
	stageID string,
	idempotencyKey string,
	fencingToken uint64,
) PlatformExecutionBinding {
	return PlatformExecutionBinding{
		BindingID: bindingID, RunID: "run-1", StageID: stageID,
		Attempt: 1, Generation: 1, IdempotencyKey: idempotencyKey,
		FencingToken: fencingToken, RuntimeKind: "local", RuntimeID: "runtime-1",
		CreatedAt: now,
	}
}

func succeededReviewRun(now time.Time) ReviewRun {
	run := validReviewRun()
	inputRef := artifactRef(ContractReviewInput)
	outputRef := artifactRef(ContractStageResult)
	findingRef := artifactRef(ContractFindingSet)
	jsonRef := artifactRef(ContractJSONReport)
	markdownRef := artifactRef(ContractMarkdownReport)
	run.Status = RunStatusSucceeded
	run.StartedAt = &now
	run.CompletedAt = &now
	run.StageAttempts = []StageAttempt{{
		StageID: "detect", Attempt: 1, Generation: 1, BindingID: "binding-1",
		Status: StageStatusSucceeded, InputRefs: []ArtifactRef{inputRef},
		OutputRef: &outputRef, StartedAt: now, FinishedAt: &now,
	}}
	run.Bindings = []PlatformExecutionBinding{
		testBinding(now, "binding-1", "detect", "key-1", 1),
	}
	run.Evidence = []RunEvidence{{
		EvidenceID: "evidence-1", BindingID: "binding-1", StageID: "detect",
		Attempt: 1, Generation: 1, IdempotencyKey: "key-1", FencingToken: 1,
		ArtifactRef: outputRef, Completeness: CompletenessComplete,
		CompletenessNotes: []string{}, RecordedAt: now,
	}}
	run.FindingSetRef = &findingRef
	run.JSONReportRef = &jsonRef
	run.MarkdownReportRef = &markdownRef
	return run
}
