package evaluation

import (
	"context"
	"errors"
	"testing"

	"argus.local/argus/internal/runmodel"
)

func TestEvaluationProbeLedgerIsAppendOnlyAndRejectsBranchingCorrections(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	probe := testEvaluationProbe()
	mutation := testMutation("probe-event-1", probe.RecordedAt, RoleProbeIngest)
	mutation.Actor = "probe-ingest-service"
	entry, err := repository.RecordEvaluationProbe(context.Background(), probe, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := repository.RecordEvaluationProbe(context.Background(), probe, mutation); err != nil || retry.EventID != entry.EventID {
		t.Fatalf("idempotent probe = %+v, %v", retry, err)
	}
	correction := probe
	correction.ProbeID = "probe-2"
	correction.PriorProbeID = probe.ProbeID
	correction.RecordedAt = correction.RecordedAt.Add(1)
	correction.SourceRevision = "source-revision-2"
	correctionMutation := testMutation("probe-event-2", correction.RecordedAt, RoleProbeIngest)
	if _, err := repository.RecordEvaluationProbe(context.Background(), correction, correctionMutation); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ResolveEvaluationProbe(probe.ProbeID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("ResolveEvaluationProbe(superseded) error = %v", err)
	}
	branch := correction
	branch.ProbeID = "probe-3"
	branch.RecordedAt = branch.RecordedAt.Add(1)
	if _, err := repository.RecordEvaluationProbe(context.Background(), branch,
		testMutation("probe-event-3", branch.RecordedAt, RoleProbeIngest)); !errors.Is(err, ErrConflict) {
		t.Fatalf("RecordEvaluationProbe(branch) error = %v", err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := restarted.ResolveEvaluationProbe(correction.ProbeID)
	if err != nil || resolved.SourceRevision != correction.SourceRevision {
		t.Fatalf("resolved correction = %+v, %v", resolved, err)
	}
	listed, err := restarted.ListEvaluationProbes(Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListEvaluationProbes() = %+v, %v", listed, err)
	}
}

func TestEvaluationProbeRequiresIndependentOracle(t *testing.T) {
	probe := testEvaluationProbe()
	probe.Oracle.Authority = probe.SourceAuthority
	if err := probe.Validate(); err == nil {
		t.Fatal("probe accepted source authority as its own oracle")
	}
	receipt := testEvaluationProbeReceipt(probe)
	receipt.BaselineTargetRef = nil
	if err := receipt.Validate(); err == nil {
		t.Fatal("mutation receipt accepted missing baseline")
	}
}

func testEvaluationProbe() EvaluationProbe {
	receiptDigest := testDigest("probe-receipt")
	evidenceDigest := testDigest("probe-oracle-evidence")
	return EvaluationProbe{
		SchemaVersion: EvaluationProbeSchemaVersion,
		ProbeID:       "probe-1", Kind: ProbeMutationDefect, ReviewRunID: "review-run-1",
		Oracle: ProbeOracle{
			Authority: "independent-oracle", Revision: "oracle-v1",
			ExpectedOutcome: OutcomeDefectPresent, Category: "correctness", Severity: "high",
			Anchors:           []LabelAnchor{{Path: "main.go", Side: "new", StartLine: 4, EndLine: 4, SourceDigest: testDigest("mutated-source")}},
			EvidenceArtifacts: []runmodel.ArtifactRef{{URI: "artifact://local/sha256/" + evidenceDigest, SHA256: evidenceDigest, SizeBytes: 8, Contract: "argus.external_oracle_evidence.v1alpha1"}},
		},
		ReceiptRef:      runmodel.ArtifactRef{URI: "artifact://local/sha256/" + receiptDigest, SHA256: receiptDigest, SizeBytes: 8, Contract: EvaluationProbeReceiptSchemaVersion},
		SourceAuthority: "mutation-generator", SourceID: "mutation-42", SourceRevision: "generator-v1",
		RecordedAt: testEpoch.Add(20),
	}
}

func testEvaluationProbeReceipt(probe EvaluationProbe) EvaluationProbeReceipt {
	inputDigest := testDigest("probe-input")
	baselineDigest := testDigest("probe-baseline")
	evidenceDigest := testDigest("probe-receipt-evidence")
	oracleDigest, _ := probe.Oracle.SHA256()
	baseline := runmodel.ArtifactRef{URI: "artifact://local/sha256/" + baselineDigest, SHA256: baselineDigest, SizeBytes: 8, Contract: runmodel.ContractMaterializedTarget}
	return EvaluationProbeReceipt{
		SchemaVersion: EvaluationProbeReceiptSchemaVersion,
		ProbeID:       probe.ProbeID, Kind: probe.Kind, ReviewRunID: probe.ReviewRunID,
		InputTargetRef:    runmodel.ArtifactRef{URI: "artifact://local/sha256/" + inputDigest, SHA256: inputDigest, SizeBytes: 8, Contract: runmodel.ContractMaterializedTarget},
		BaselineTargetRef: &baseline, OracleSHA256: oracleDigest,
		MethodID: "go-ast-mutator", MethodRevision: "v1",
		ExecutorAuthority: "mutation-verifier", ExecutorRevision: "v1",
		ObservedOutcome:   probe.Oracle.ExpectedOutcome,
		EvidenceArtifacts: []runmodel.ArtifactRef{{URI: "artifact://local/sha256/" + evidenceDigest, SHA256: evidenceDigest, SizeBytes: 8, Contract: "argus.probe_execution_evidence.v1alpha1"}},
		RemoteWrites:      "deny", CompletedAt: probe.RecordedAt.Add(-1),
	}
}
