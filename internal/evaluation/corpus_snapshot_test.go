package evaluation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
)

func TestDecodeCorpusSnapshotRejectsUnsupportedCaseAndUnknownFields(t *testing.T) {
	snapshot := CorpusSnapshot{
		SchemaVersion: CorpusSnapshotSchemaVersion,
		CorpusID:      "corpus-snapshot-validation",
		Revision:      "revision-1",
		Purpose:       CorpusPurposeQualityGate,
		Split:         SplitTest,
		Cases: []CorpusSnapshotCase{{
			CaseID: "case-a", CaseType: CaseFixValidation, Split: SplitTest,
			CloneGroupID: "clone-a", GovernanceRevision: 1, LabelRevision: 1,
			CurrentCaseSHA256: strings.Repeat("a", 64), SourceReviewRunID: "source-run-a",
			SourceReviewRunRef:      evaluationArtifactRef("source-run-a", runmodel.ContractReviewRun),
			TargetSnapshotRef:       evaluationArtifactRef("target-a", runmodel.ContractMaterializedTarget),
			ExecutionSnapshotSHA256: strings.Repeat("b", 64),
		}},
		CreatedAt: testEpoch.Add(time.Minute), CreatedBy: "dataset-curator",
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCorpusSnapshot(data); err == nil || !strings.Contains(err.Error(), "requires an apply trial") {
		t.Fatalf("DecodeCorpusSnapshot() error = %v", err)
	}

	request := `{"schema_version":"argus.corpus_snapshot_request.v1alpha1","corpus_id":"corpus-a","revision":"r1","purpose":"quality_gate","split":"test","cases":[{"case_id":"case-a","expected_governance_revision":1,"expected_label_revision":1,"source_review_run_id":"source-a"}],"created_at":"2025-01-01T00:00:00Z","unexpected":true}`
	if _, err := DecodeCorpusSnapshotRequest([]byte(request)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodeCorpusSnapshotRequest() error = %v", err)
	}
}

func TestCorpusSnapshotRejectsPurposeSplitMismatchAndDuplicateCloneGroup(t *testing.T) {
	request := CorpusSnapshotRequest{
		SchemaVersion: CorpusSnapshotRequestSchemaVersion,
		CorpusID:      "corpus-purpose", Revision: "revision-1",
		Purpose: CorpusPurposePromotionGate, Split: SplitTest,
		Cases: []CorpusSnapshotCaseRequest{{
			CaseID: "case-a", ExpectedGovernanceRevision: 1,
			ExpectedLabelRevision: 1, SourceReviewRunID: "source-a",
		}}, CreatedAt: testEpoch,
	}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "requires holdout") {
		t.Fatalf("CorpusSnapshotRequest.Validate() error = %v", err)
	}

	caseValue := CorpusSnapshotCase{
		CaseID: "case-a", CaseType: CasePositiveLocalized, Split: SplitTest,
		CloneGroupID: "clone-shared", GovernanceRevision: 1, LabelRevision: 1,
		CurrentCaseSHA256: strings.Repeat("a", 64), SourceReviewRunID: "source-a",
		SourceReviewRunRef:      evaluationArtifactRef("source-a", runmodel.ContractReviewRun),
		TargetSnapshotRef:       evaluationArtifactRef("target-a", runmodel.ContractMaterializedTarget),
		ExecutionSnapshotSHA256: strings.Repeat("b", 64),
	}
	duplicate := caseValue
	duplicate.CaseID = "case-b"
	duplicate.SourceReviewRunID = "source-b"
	duplicate.SourceReviewRunRef = evaluationArtifactRef("source-b", runmodel.ContractReviewRun)
	snapshot := CorpusSnapshot{
		SchemaVersion: CorpusSnapshotSchemaVersion,
		CorpusID:      "corpus-duplicates", Revision: "revision-1",
		Purpose: CorpusPurposeQualityGate, Split: SplitTest,
		Cases:     []CorpusSnapshotCase{caseValue, duplicate},
		CreatedAt: testEpoch, CreatedBy: "dataset-curator",
	}
	if err := snapshot.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates case") {
		t.Fatalf("CorpusSnapshot.Validate() error = %v", err)
	}
}

func TestFormalCorpusBatchRequestRequiresCorpusSnapshotRef(t *testing.T) {
	request := FormalCorpusBatchRequest{
		SchemaVersion: FormalCorpusBatchRequestSchemaVersion,
		BatchID:       "batch-a", EvaluationRunID: "evaluation-a",
		EvaluatorRevision: "evaluator-r1", ExecutorRevision: "executor-r1",
		MaxConcurrency: 1,
		Cases: []FormalCorpusBatchCase{{
			CaseID: "case-a", ExpectedLabelRevision: 1, SourceReviewRunID: "source-a",
			ExposureObservations: []ExposureObservation{
				{Component: ExposurePrompt, Status: ExposureNotSeen, Revision: "prompt-r1"},
				{Component: ExposureRule, Status: ExposureNotSeen, Revision: "rule-r1"},
				{Component: ExposureModel, Status: ExposureNotSeen, Revision: "model-r1"},
				{Component: ExposureIndex, Status: ExposureNotSeen, Revision: "index-r1"},
			},
		}},
		CreatedAt: testEpoch.Add(time.Minute),
	}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "corpus_snapshot_ref") {
		t.Fatalf("FormalCorpusBatchRequest.Validate() error = %v", err)
	}
}
