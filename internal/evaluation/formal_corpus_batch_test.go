package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type formalCorpusExecutorStub struct {
	runID string
	err   error
	calls int
}

func (stub *formalCorpusExecutorStub) ExecuteFormalCorpusCase(
	_ context.Context,
	request FormalCorpusCaseExecutionRequest,
) (string, error) {
	stub.calls++
	if request.IdempotencyKey == "" || request.ExecutorTemplateRef.Contract != FormalCorpusExecutorTemplateContract {
		return "", errors.New("missing exact corpus execution binding")
	}
	if stub.err != nil {
		return "", stub.err
	}
	return stub.runID, nil
}

func TestFormalCorpusBatchRunsActiveOracleAndIsExactIdempotent(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	caseAt := testEpoch.Add(2 * time.Hour)
	targetRef := evaluationArtifactRef("formal-corpus-target", runmodel.ContractMaterializedTarget)
	evaluationCase := testActiveCase("formal-corpus-case", "repo-corpus", SplitHoldout, caseAt.Add(-3*time.Minute))
	evaluationCase.InputSnapshotRef = targetRef.URI
	roles := []Role{RoleHoldoutMaintainer, RoleHoldoutRunner}
	createCase(t, repository, evaluationCase, roles...)

	const sourceRunID = "formal-corpus-source"
	const formalRunID = "formal-corpus-result"
	reader := newEvaluationRunReaderStub(t, formalRunID, targetRef, contractsv1alpha1.AgentReviewComplete)
	formalRun := reader.runs[formalRunID]
	formalRun.Kind = runmodel.RunKindReview
	reader.runs[formalRunID] = formalRun
	reader.runs[sourceRunID] = runmodel.ReviewRun{
		RunID: sourceRunID, Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusSucceeded, TargetSnapshotRef: targetRef,
	}
	reader.snapshots[sourceRunID] = runmodel.ExecutionSnapshot{
		RemoteWrites: "deny", ToolPolicy: runmodel.ToolInvocationPolicy{RemoteWrites: "deny"},
	}
	reader.runRefs[sourceRunID] = evaluationArtifactRef(sourceRunID+"-run", runmodel.ContractReviewRun)
	corpusRef := buildFormalCorpusSnapshotRef(
		t, repository, reader, caseAt.Add(-time.Minute), roles,
		CorpusSnapshotCaseRequest{
			CaseID: evaluationCase.CaseID, ExpectedGovernanceRevision: 1,
			ExpectedLabelRevision: 1, SourceReviewRunID: sourceRunID,
		},
	)

	templateRef := evaluationArtifactRef("formal-corpus-template", FormalCorpusExecutorTemplateContract)
	request := FormalCorpusBatchRequest{
		SchemaVersion: FormalCorpusBatchRequestSchemaVersion,
		BatchID:       "formal-corpus-batch", EvaluationRunID: "formal-corpus-evaluation",
		EvaluatorRevision: "formal-corpus-evaluator-1", ExecutorRevision: "executor-1",
		CorpusSnapshotRef: corpusRef, ExecutorTemplateRef: &templateRef, MaxConcurrency: 1,
		Cases: []FormalCorpusBatchCase{{
			CaseID: evaluationCase.CaseID, ExpectedLabelRevision: 1,
			SourceReviewRunID:    sourceRunID,
			ExposureObservations: testExposure("unused", evaluationCase.CaseID, caseAt, ExposureNotSeen).Observations,
		}},
		CreatedAt: caseAt,
	}
	mutation := testMutation("formal-corpus-intent", caseAt, roles...)
	clock := newFormalCorpusTestClock(caseAt.Add(time.Second))
	executor := &formalCorpusExecutorStub{runID: formalRunID}
	runner, err := NewFormalCorpusBatchRunner(repository, reader, executor, clock)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runner.Run(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if first.BatchID != request.BatchID || first.EvaluationRunID != request.EvaluationRunID ||
		len(first.Cases) != 1 || first.Cases[0].FormalReviewRunID != formalRunID || executor.calls != 1 {
		t.Fatalf("Run() result=%+v calls=%d", first, executor.calls)
	}
	retry, err := runner.Run(context.Background(), request, mutation)
	if err != nil || !reflect.DeepEqual(retry, first) || executor.calls != 1 {
		t.Fatalf("retry=%+v error=%v calls=%d", retry, err, executor.calls)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	record, err := restarted.GetFormalCorpusBatch(request.BatchID, Access{Actor: mutation.Actor, Roles: roles})
	if err != nil || record.Status != FormalCorpusBatchSucceeded || record.Result == nil ||
		!reflect.DeepEqual(record.Intent, mutation) || !reflect.DeepEqual(*record.Result, first) ||
		len(record.CompletedCases) != 1 || record.FinalizingAt == nil {
		t.Fatalf("restarted record=%+v error=%v", record, err)
	}
	evaluationRun, err := restarted.GetEvaluationRun(request.EvaluationRunID, Access{Actor: mutation.Actor, Roles: roles})
	if err != nil || len(evaluationRun.Results) != 1 || evaluationRun.Results[0].ReviewRunID != formalRunID {
		t.Fatalf("evaluation run=%+v error=%v", evaluationRun, err)
	}
}

func TestFormalCorpusBatchPreflightsBeforeProviderAndPersistsRedactedFailure(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	caseAt := testEpoch.Add(3 * time.Hour)
	targetRef := evaluationArtifactRef("formal-corpus-failure-target", runmodel.ContractMaterializedTarget)
	evaluationCase := testActiveCase("formal-corpus-failure-case", "repo-corpus", SplitHoldout, caseAt.Add(-3*time.Minute))
	evaluationCase.InputSnapshotRef = targetRef.URI
	roles := []Role{RoleHoldoutMaintainer, RoleHoldoutRunner}
	createCase(t, repository, evaluationCase, roles...)
	reader := &evaluationRunReaderStub{
		runs: map[string]runmodel.ReviewRun{"wrong-source": {
			RunID: "wrong-source", Kind: runmodel.RunKindReview,
			Status: runmodel.RunStatusSucceeded, TargetSnapshotRef: targetRef,
		}},
		snapshots: map[string]runmodel.ExecutionSnapshot{"wrong-source": {
			RemoteWrites: "deny", ToolPolicy: runmodel.ToolInvocationPolicy{RemoteWrites: "deny"},
		}},
		artifacts: map[string][]byte{},
		runRefs: map[string]runmodel.ArtifactRef{
			"wrong-source": evaluationArtifactRef("wrong-source-run", runmodel.ContractReviewRun),
		},
	}
	corpusRef := buildFormalCorpusSnapshotRef(
		t, repository, reader, caseAt.Add(-time.Minute), roles,
		CorpusSnapshotCaseRequest{
			CaseID: evaluationCase.CaseID, ExpectedGovernanceRevision: 1,
			ExpectedLabelRevision: 1, SourceReviewRunID: "wrong-source",
		},
	)
	wrongRun := reader.runs["wrong-source"]
	wrongRun.TargetSnapshotRef = evaluationArtifactRef("different-target", runmodel.ContractMaterializedTarget)
	reader.runs["wrong-source"] = wrongRun
	templateRef := evaluationArtifactRef("formal-corpus-failure-template", FormalCorpusExecutorTemplateContract)
	request := FormalCorpusBatchRequest{
		SchemaVersion: FormalCorpusBatchRequestSchemaVersion,
		BatchID:       "formal-corpus-preflight-failure", EvaluationRunID: "formal-corpus-failure-evaluation",
		EvaluatorRevision: "formal-corpus-evaluator-1", ExecutorRevision: "executor-1",
		CorpusSnapshotRef: corpusRef, ExecutorTemplateRef: &templateRef, MaxConcurrency: 1,
		Cases: []FormalCorpusBatchCase{{
			CaseID: evaluationCase.CaseID, ExpectedLabelRevision: 1, SourceReviewRunID: "wrong-source",
			ExposureObservations: testExposure("unused", evaluationCase.CaseID, caseAt, ExposureNotSeen).Observations,
		}}, CreatedAt: caseAt,
	}
	mutation := testMutation("formal-corpus-failure-intent", caseAt, roles...)
	executor := &formalCorpusExecutorStub{err: errors.New("provider secret and endpoint must never persist")}
	runner, err := NewFormalCorpusBatchRunner(repository, reader, executor, newFormalCorpusTestClock(caseAt.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), request, mutation); err == nil || executor.calls != 0 {
		t.Fatalf("preflight error=%v provider_calls=%d", err, executor.calls)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	record, err := restarted.GetFormalCorpusBatch(request.BatchID, Access{Actor: mutation.Actor, Roles: roles})
	if err != nil || record.Status != FormalCorpusBatchFailed || record.Failure == nil ||
		record.Failure.Code != "formal_corpus_preflight_failed" || record.Result != nil {
		t.Fatalf("failed record=%+v error=%v", record, err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "provider secret") || strings.Contains(string(encoded), "endpoint") {
		t.Fatalf("durable failure leaked raw cause: %s", encoded)
	}
	if _, err := runner.Run(context.Background(), request, mutation); err == nil || executor.calls != 0 {
		t.Fatalf("failed retry error=%v provider_calls=%d", err, executor.calls)
	}
}

func TestFormalCorpusBatchRejectsHoldoutSeenAfterSnapshot(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	caseAt := testEpoch.Add(4 * time.Hour)
	targetRef := evaluationArtifactRef("formal-corpus-contaminated-target", runmodel.ContractMaterializedTarget)
	evaluationCase := testActiveCase("formal-corpus-contaminated-case", "repo-contaminated", SplitHoldout, caseAt.Add(-3*time.Minute))
	evaluationCase.InputSnapshotRef = targetRef.URI
	roles := []Role{RoleHoldoutMaintainer, RoleHoldoutRunner}
	createCase(t, repository, evaluationCase, roles...)

	const sourceRunID = "formal-corpus-contaminated-source"
	reader := &evaluationRunReaderStub{
		runs: map[string]runmodel.ReviewRun{sourceRunID: {
			RunID: sourceRunID, Kind: runmodel.RunKindReview,
			Status: runmodel.RunStatusSucceeded, TargetSnapshotRef: targetRef,
		}},
		snapshots: map[string]runmodel.ExecutionSnapshot{sourceRunID: {
			RemoteWrites: "deny", ToolPolicy: runmodel.ToolInvocationPolicy{RemoteWrites: "deny"},
		}},
		artifacts: map[string][]byte{},
		runRefs: map[string]runmodel.ArtifactRef{
			sourceRunID: evaluationArtifactRef(sourceRunID+"-run", runmodel.ContractReviewRun),
		},
	}
	corpusRef := buildFormalCorpusSnapshotRef(
		t, repository, reader, caseAt.Add(-time.Minute), roles,
		CorpusSnapshotCaseRequest{
			CaseID: evaluationCase.CaseID, ExpectedGovernanceRevision: 1,
			ExpectedLabelRevision: 1, SourceReviewRunID: sourceRunID,
		},
	)
	if _, err := repository.RecordExposure(context.Background(),
		testExposure("earlier-diagnostic", evaluationCase.CaseID, caseAt, ExposureSeen),
		testMutation("record-earlier-diagnostic", caseAt, roles...)); err != nil {
		t.Fatal(err)
	}

	templateRef := evaluationArtifactRef("formal-corpus-contaminated-template", FormalCorpusExecutorTemplateContract)
	request := FormalCorpusBatchRequest{
		SchemaVersion: FormalCorpusBatchRequestSchemaVersion,
		BatchID:       "formal-corpus-contaminated-batch", EvaluationRunID: "formal-corpus-clean-evaluation",
		EvaluatorRevision: "formal-corpus-evaluator-1", ExecutorRevision: "executor-1",
		CorpusSnapshotRef: corpusRef, ExecutorTemplateRef: &templateRef, MaxConcurrency: 1,
		Cases: []FormalCorpusBatchCase{{
			CaseID: evaluationCase.CaseID, ExpectedLabelRevision: 1, SourceReviewRunID: sourceRunID,
			ExposureObservations: testExposure("unused", evaluationCase.CaseID, caseAt.Add(time.Minute), ExposureNotSeen).Observations,
		}}, CreatedAt: caseAt.Add(time.Minute),
	}
	executor := &formalCorpusExecutorStub{runID: "must-not-run"}
	runner, err := NewFormalCorpusBatchRunner(repository, reader, executor, newFormalCorpusTestClock(caseAt.Add(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), request,
		testMutation("formal-corpus-contaminated-intent", caseAt.Add(time.Minute), roles...))
	if !errors.Is(err, ErrContaminated) || executor.calls != 0 {
		t.Fatalf("Run() error=%v provider_calls=%d", err, executor.calls)
	}
}

func buildFormalCorpusSnapshotRef(
	t *testing.T,
	repository *Repository,
	reader *evaluationRunReaderStub,
	at time.Time,
	roles []Role,
	cases ...CorpusSnapshotCaseRequest,
) runmodel.ArtifactRef {
	t.Helper()
	builder, err := NewCorpusSnapshotBuilder(repository, reader)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := builder.Build(context.Background(), CorpusSnapshotRequest{
		SchemaVersion: CorpusSnapshotRequestSchemaVersion,
		CorpusID:      "formal-corpus-test", Revision: "revision-1",
		Purpose: CorpusPurposePromotionGate, Split: SplitHoldout,
		Cases: cases, CreatedAt: at,
	}, Access{Actor: "test-actor", Roles: roles})
	if err != nil {
		t.Fatalf("build corpus snapshot: %v", err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	encoded := hex.EncodeToString(digest[:])
	ref := runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + encoded, SHA256: encoded,
		SizeBytes: int64(len(data)), Contract: CorpusSnapshotContract,
	}
	reader.artifacts[ref.URI] = data
	return ref
}

func newFormalCorpusTestClock(start time.Time) func() time.Time {
	next := start
	return func() time.Time {
		current := next
		next = next.Add(time.Second)
		return current
	}
}
