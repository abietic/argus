package agentshadow

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/artifactrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestQueryExecutionsPartitionsScopeBoundBatchAndRevalidatesClosure(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		if strings.Contains(request.IdempotencyKey, "unknown") {
			return nil, errors.New("worker transport disappeared")
		}
		output := fakeSucceededWorkerResult(t, request, "clean", 4)
		*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
		return output, nil
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}

	succeeded, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "batch-succeeded"),
	)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "batch-unknown"),
	)
	if err == nil || unknown.Attempt == nil {
		t.Fatalf("unknown execution = %+v, %v", unknown, err)
	}

	foreignRequest := localBridgeRequest(fixture, node, script, "batch-foreign")
	preparedForeign, err := bridge.prepare(t.Context(), foreignRequest)
	if err != nil {
		t.Fatal(err)
	}
	preparedForeign.intent.TenantID = "another-tenant"
	preparedForeign.intent.WorkspaceID = "another-workspace"
	if _, acquired, err := fixture.repository.acquireExecutionIntent(preparedForeign.intent); err != nil || !acquired {
		t.Fatalf("acquire foreign intent = %v, %v", acquired, err)
	}

	missingID := "execution-missing"
	request := ExecutionBatchQueryRequest{
		Scope: fixture.scope,
		ExecutionIDs: []string{
			unknown.Attempt.ExecutionID,
			missingID,
			preparedForeign.intent.Plan.ExecutionID,
			succeeded.Attempt.ExecutionID,
		},
	}
	got, err := fixture.service.QueryExecutions(t.Context(), request)
	if err != nil {
		t.Fatalf("QueryExecutions() error = %v", err)
	}
	if len(got.Attempts) != 2 ||
		got.Attempts[0].ExecutionID != unknown.Attempt.ExecutionID ||
		got.Attempts[1].ExecutionID != succeeded.Attempt.ExecutionID {
		t.Fatalf("matched attempts lost request order: %+v", got.Attempts)
	}
	wantMissing := []string{missingID, preparedForeign.intent.Plan.ExecutionID}
	if !reflect.DeepEqual(got.MissingExecutionIDs, wantMissing) {
		t.Fatalf("missing ids = %v, want %v", got.MissingExecutionIDs, wantMissing)
	}
	for _, attempt := range got.Attempts {
		if err := attempt.Validate(); err != nil {
			t.Fatalf("batch returned invalid attempt: %v", err)
		}
	}

	// A matched success is not just an intent lookup. The batch port must
	// revalidate the complete committed-result closure before returning it.
	ref := succeeded.Result.Record.ReceiptCollectionRef
	artifactPath := filepath.Join(
		fixture.store.Root(), "artifacts", "sha256", ref.Ref.SHA256[:2], ref.Ref.SHA256,
	)
	if err := os.Chmod(artifactPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("tampered batch receipt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.QueryExecutions(t.Context(), ExecutionBatchQueryRequest{
		Scope: fixture.scope, ExecutionIDs: []string{succeeded.Attempt.ExecutionID},
	}); !errors.Is(err, artifactrepo.ErrQuarantined) {
		t.Fatalf("tampered batch closure error = %v, want ErrQuarantined", err)
	}
}

func TestExecutionBatchRequestRejectsDuplicateAndOversizedInputs(t *testing.T) {
	scope := Scope{TenantID: LocalTenantID, WorkspaceID: LocalWorkspaceID}
	duplicate := ExecutionBatchQueryRequest{
		Scope: scope, ExecutionIDs: []string{"execution-one", "execution-one"},
	}
	if err := duplicate.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate request error = %v", err)
	}

	tooMany := make([]string, maxExecutionRecords+1)
	if err := (ExecutionBatchQueryRequest{Scope: scope, ExecutionIDs: tooMany}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized request error = %v", err)
	}

	fixture := newShadowFixture(t)
	empty, err := fixture.service.QueryExecutions(t.Context(), ExecutionBatchQueryRequest{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Attempts == nil || empty.MissingExecutionIDs == nil ||
		len(empty.Attempts) != 0 || len(empty.MissingExecutionIDs) != 0 {
		t.Fatalf("empty batch result = %#v", empty)
	}
}
