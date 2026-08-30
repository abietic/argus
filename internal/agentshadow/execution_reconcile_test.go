package agentshadow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/pireviewmap"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestPostIntentHostFailureObservationIsBoundedRedactedAndQueryable(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	const secret = "sk-secret-provider-payload https://credential.example"
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		*fixture.now = request.Plan.CreatedAt.Add(5 * time.Second)
		return nil, errors.New(secret)
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	result, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "host-observation-redaction"),
	)
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("primary diagnostic error was not preserved: %v", err)
	}
	assertExecutionAttemptStatus(t, result.Attempt, ExecutionStatusUnknownOutcome)
	observation := result.Attempt.HostFailure
	if observation == nil ||
		observation.Stage != ExecutionHostFailureWorkerRun ||
		observation.ReasonCode != ExecutionHostFailureRunnerUnconfirmed ||
		observation.TimeSource != ExecutionHostObservationClock ||
		!observation.ObservedAt.Equal(result.Attempt.AcceptedAt.Add(5*time.Second)) ||
		result.Attempt.HostFailureObservationSHA256 == nil {
		t.Fatalf("host failure observation = %+v", result.Attempt)
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxExecutionHostFailureBytes ||
		bytes.Contains(encoded, []byte(secret)) ||
		bytes.Contains(encoded, []byte("credential.example")) {
		t.Fatalf("unsafe persisted host observation (%d bytes): %s", len(encoded), encoded)
	}
	if result.Attempt.CompletionSHA256 != nil || result.Attempt.CompletedAt != nil ||
		result.Attempt.Manifest != nil || result.Attempt.Failure != nil {
		t.Fatalf("host observation fabricated a terminal outcome: %+v", result.Attempt)
	}

	queried, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
		Scope: fixture.scope, ExecutionID: result.Attempt.ExecutionID,
	})
	if err != nil || !reflect.DeepEqual(queried.HostFailure, observation) ||
		queried.HostFailureObservationSHA256 == nil ||
		*queried.HostFailureObservationSHA256 != *result.Attempt.HostFailureObservationSHA256 {
		t.Fatalf("QueryExecution host failure = %+v, %v", queried, err)
	}
	listed, err := fixture.service.ListExecutions(t.Context(), ExecutionListRequest{
		Scope: fixture.scope, StartInclusive: observation.ObservedAt,
		EndExclusive: observation.ObservedAt.Add(time.Nanosecond),
	})
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0].HostFailure, observation) {
		t.Fatalf("ListExecutions host failure = %+v, %v", listed, err)
	}

	// The first valid observation is immutable even if a later caller reports
	// another post-intent stage.
	*fixture.now = fixture.now.Add(time.Minute)
	reused, err := fixture.repository.observeExecutionHostFailure(
		executionIntentForAttempt(t, fixture, *result.Attempt),
		ExecutionHostFailureShadowImport,
		ExecutionHostFailureImportUnconfirmed,
	)
	if err != nil || !reflect.DeepEqual(reused, *observation) {
		t.Fatalf("idempotent host observation = %+v, %v", reused, err)
	}
}

func TestExecutionHistoryRejectsHostFailurePathScopeSymlinkAndTamper(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *shadowFixture, string)
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, fixture *shadowFixture, path string) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(fixture.store.Root(), "host-observation-copy.json")
				if err := os.WriteFile(target, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "forged path",
			mutate: func(t *testing.T, _ *shadowFixture, path string) {
				forged := filepath.Join(filepath.Dir(path), strings.Repeat("f", 64)+".json")
				if forged == path {
					forged = filepath.Join(filepath.Dir(path), strings.Repeat("e", 64)+".json")
				}
				if err := os.Rename(path, forged); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "forged scope",
			mutate: func(t *testing.T, _ *shadowFixture, path string) {
				var observation ExecutionHostFailureObservation
				readStrictTestJSON(t, path, &observation)
				observation.Scope = Scope{TenantID: "forged-tenant", WorkspaceID: "forged-workspace"}
				replaceImmutableTestJSON(t, path, observation)
			},
		},
		{
			name: "taxonomy tamper",
			mutate: func(t *testing.T, _ *shadowFixture, path string) {
				var observation ExecutionHostFailureObservation
				readStrictTestJSON(t, path, &observation)
				observation.ReasonCode = ExecutionHostFailureImportUnconfirmed
				replaceImmutableTestJSON(t, path, observation)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, attempt := createUnknownHostFailureAttempt(t, "host-path-"+strings.ReplaceAll(test.name, " ", "-"))
			path := immutableExecutionObjectPath(
				fixture,
				executionHostFailureObjectID(fixture.scope, attempt.IdempotencyKey),
			)
			test.mutate(t, fixture, path)
			if _, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
				Scope: fixture.scope, ExecutionID: attempt.ExecutionID,
			}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("QueryExecution error = %v, want ErrCorrupt", err)
			}
			if _, err := fixture.service.ListExecutions(t.Context(), ExecutionListRequest{
				Scope: fixture.scope, StartInclusive: attempt.AcceptedAt.Add(-time.Second),
				EndExclusive: attempt.ObservedAt.Add(time.Second),
			}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ListExecutions error = %v, want ErrCorrupt", err)
			}
			if _, err := fixture.service.QueryExecutions(t.Context(), ExecutionBatchQueryRequest{
				Scope: fixture.scope, ExecutionIDs: []string{attempt.ExecutionID},
			}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("QueryExecutions error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestPreauthorizedCommittedImportReconcilesWithoutPostLinkOrWorkerRerun(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("worker must not be called during reconciliation")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "reconcile-committed-import")
	intent, committed := commitBridgeResultWithoutCompletion(t, fixture, bridge, request)
	if _, err := fixture.repository.observeExecutionHostFailure(
		intent,
		ExecutionHostFailureCompletionCommit,
		ExecutionHostFailureCompletionUnconfirmed,
	); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
		Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID,
	})
	if err != nil || before.Status != ExecutionStatusUnknownOutcome || before.HostFailure == nil {
		t.Fatalf("pre-reconcile attempt = %+v, %v", before, err)
	}

	resolved, err := bridge.Run(t.Context(), request)
	if err != nil || !resolved.Reused || resolved.Attempt == nil ||
		resolved.Attempt.Status != ExecutionStatusSucceeded ||
		resolved.Result.Manifest.ManifestID != committed.Manifest.ManifestID ||
		resolved.Attempt.HostFailure == nil {
		t.Fatalf("reconciled bridge result = %+v, %v", resolved, err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("reconciliation reran worker/provider %d times", runner.callCount())
	}
	if resolved.Attempt.CompletedAt == nil ||
		!resolved.Attempt.CompletedAt.Equal(committed.HypothesisSet.GeneratedAt) {
		t.Fatalf("reconciled completion time = %+v, want %s", resolved.Attempt.CompletedAt, committed.HypothesisSet.GeneratedAt)
	}

	again, reconciled, err := fixture.service.ReconcileExecution(t.Context(), ExecutionReconcileRequest{
		Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID,
	})
	if err != nil || reconciled || again.Status != ExecutionStatusSucceeded ||
		again.Manifest == nil || again.Manifest.ManifestID != committed.Manifest.ManifestID {
		t.Fatalf("idempotent reconcile = %+v, reconciled=%v, err=%v", again, reconciled, err)
	}
}

func TestLocalBridgeImportAcknowledgementLossRecoversWithoutWorkerRerun(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		output := fakeSucceededWorkerResult(t, request, "clean", 4)
		*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
		return output, nil
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "import-acknowledgement-loss")
	delegateImport := bridge.importShadow
	injected := errors.New("injected import acknowledgement loss")
	seamCalls := 0
	bridge.importShadow = func(ctx context.Context, importRequest ImportRequest) (Result, error) {
		seamCalls++
		var intent executionIntent
		if err := fixture.store.GetJSON(
			executionIntentObjectID(importRequest.Scope, importRequest.IdempotencyKey),
			&intent,
		); err != nil {
			return Result{}, fmt.Errorf("load execution intent at import seam: %w", err)
		}
		authorization, exists, err := fixture.repository.loadExecutionImportAuthorization(intent)
		if err != nil || !exists {
			return Result{}, fmt.Errorf("pre-import authorization missing at seam: exists=%v err=%v", exists, err)
		}
		if authorization.ImportInputDigest != digestByteSlices(
			importRequest.Plan,
			importRequest.HypothesisSet,
			importRequest.RawCandidateCollection,
			importRequest.TaskEvidenceCollection,
			importRequest.ReceiptCollection,
		) {
			return Result{}, errors.New("pre-import authorization does not bind seam payload")
		}
		committed, err := delegateImport(ctx, importRequest)
		if err != nil {
			return Result{}, err
		}
		if committed.Manifest.ManifestID != authorization.ExpectedManifestID {
			return Result{}, errors.New("committed import does not match pre-authorization")
		}
		return Result{}, injected
	}

	first, err := bridge.Run(t.Context(), request)
	if !errors.Is(err, injected) || first.OutcomeAcknowledged || first.Attempt == nil ||
		first.Attempt.Status != ExecutionStatusUnknownOutcome ||
		first.Attempt.HostFailure == nil ||
		first.Attempt.HostFailure.Stage != ExecutionHostFailureShadowImport {
		t.Fatalf("first acknowledgement-lost run = %+v, %v", first, err)
	}
	if runner.callCount() != 1 || seamCalls != 1 {
		t.Fatalf("first acknowledgement-lost calls: worker=%d seam=%d", runner.callCount(), seamCalls)
	}

	second, err := bridge.Run(t.Context(), request)
	if err != nil || !second.Reused || !second.OutcomeAcknowledged || second.Attempt == nil ||
		second.Attempt.Status != ExecutionStatusSucceeded ||
		second.Result.Manifest.ManifestID == "" {
		t.Fatalf("recovered acknowledgement-lost run = %+v, %v", second, err)
	}
	if runner.callCount() != 1 || seamCalls != 1 {
		t.Fatalf("recovery reran side effects: worker=%d seam=%d", runner.callCount(), seamCalls)
	}
}

func TestLocalBridgeCompletionAcknowledgementLossIsExplicit(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		output := fakeSucceededWorkerResult(t, request, "clean", 4)
		*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
		return output, nil
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "completion-acknowledgement-loss")
	delegateComplete := bridge.completeExecution
	injected := errors.New("injected completion acknowledgement loss")
	completionCalls := 0
	bridge.completeExecution = func(
		intent executionIntent,
		completion executionCompletion,
	) (executionCompletion, bool, error) {
		completionCalls++
		committed, created, err := delegateComplete(intent, completion)
		if err != nil {
			return executionCompletion{}, false, err
		}
		return committed, created, injected
	}

	first, err := bridge.Run(t.Context(), request)
	if !errors.Is(err, injected) || first.OutcomeAcknowledged || first.Attempt == nil ||
		first.Attempt.Status != ExecutionStatusSucceeded ||
		first.Result.Manifest.ManifestID == "" {
		t.Fatalf("completion acknowledgement-lost run = %+v, %v", first, err)
	}
	if runner.callCount() != 1 || completionCalls != 1 {
		t.Fatalf("completion acknowledgement-lost calls: worker=%d completion=%d", runner.callCount(), completionCalls)
	}

	second, err := bridge.Run(t.Context(), request)
	if err != nil || !second.Reused || !second.OutcomeAcknowledged || second.Attempt == nil ||
		second.Attempt.Status != ExecutionStatusSucceeded {
		t.Fatalf("completion acknowledgement retry = %+v, %v", second, err)
	}
	if runner.callCount() != 1 || completionCalls != 1 {
		t.Fatalf("completion retry reran side effects: worker=%d completion=%d", runner.callCount(), completionCalls)
	}
}

func TestReconcileWithoutCommittedResultStaysUnknownAndNeverRunsWorker(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("worker must not be called")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "reconcile-no-result")
	prepared, err := bridge.prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	intent, acquired, err := fixture.repository.acquireExecutionIntent(prepared.intent)
	if err != nil || !acquired {
		t.Fatalf("acquire intent = %+v, %v, %v", intent, acquired, err)
	}
	attempt, reconciled, err := fixture.service.ReconcileExecution(t.Context(), ExecutionReconcileRequest{
		Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID,
	})
	if err != nil || reconciled || attempt.Status != ExecutionStatusUnknownOutcome ||
		attempt.CompletionSHA256 != nil {
		t.Fatalf("empty reconcile = %+v, reconciled=%v, err=%v", attempt, reconciled, err)
	}
	result, err := bridge.Run(t.Context(), request)
	if !errors.Is(err, ErrUnknownOutcome) || !result.Reused ||
		result.Attempt == nil || result.Attempt.Status != ExecutionStatusUnknownOutcome {
		t.Fatalf("bridge retry without result = %+v, %v", result, err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("unknown reconciliation spawned %d workers", runner.callCount())
	}
}

func TestPreauthorizedImportWithoutCommitStaysUnknownAndNeverRunsWorker(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("worker must not be called")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "reconcile-preauthorized-no-import")
	payload := prepareBridgeImportPayload(t, fixture, bridge, request)
	authorization, err := bridge.authorizeExecutionImport(
		payload.intent,
		payload.plan,
		payload.hypothesisSet,
		payload.rawCandidates,
		payload.taskEvidence,
		payload.receiptCollection,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Query(t.Context(), QueryRequest{
		Scope: fixture.scope, ManifestID: authorization.ExpectedManifestID,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("preauthorized fixture unexpectedly committed import: %v", err)
	}

	attempt, reconciled, err := fixture.service.ReconcileExecution(
		t.Context(),
		ExecutionReconcileRequest{
			Scope: fixture.scope, ExecutionID: payload.intent.Plan.ExecutionID,
		},
	)
	if err != nil || reconciled || attempt.Status != ExecutionStatusUnknownOutcome {
		t.Fatalf("preauthorized no-import reconcile = %+v, reconciled=%v, err=%v", attempt, reconciled, err)
	}
	reused, err := bridge.Run(t.Context(), request)
	if !errors.Is(err, ErrUnknownOutcome) || !reused.Reused ||
		reused.Attempt == nil || reused.Attempt.Status != ExecutionStatusUnknownOutcome {
		t.Fatalf("preauthorized no-import bridge retry = %+v, %v", reused, err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("preauthorized no-import retry spawned %d workers", runner.callCount())
	}
}

func TestDirectExactImportCannotBeGraftedOntoRuntimeExecution(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("worker must not be called")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "reconcile-direct-exact-import")
	intent, committed := importBridgeResultWithoutCompletion(
		t,
		fixture,
		bridge,
		request,
		false,
	)
	if committed.Record.IdempotencyKey != intent.IdempotencyKey ||
		!reflect.DeepEqual(committed.Plan, intent.Plan) {
		t.Fatalf("direct import is not the intended exact-plan fixture: %+v", committed.Record)
	}

	attempt, reconciled, err := fixture.service.ReconcileExecution(
		t.Context(),
		ExecutionReconcileRequest{Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID},
	)
	if err != nil || reconciled || attempt.Status != ExecutionStatusUnknownOutcome {
		t.Fatalf("unlinked direct import reconcile = %+v, reconciled=%v, err=%v", attempt, reconciled, err)
	}
	if _, exists, err := fixture.repository.loadExecutionCompletion(intent); err != nil || exists {
		t.Fatalf("unlinked direct import created completion = %v, %v", exists, err)
	}
	reused, err := bridge.Run(t.Context(), request)
	if !errors.Is(err, ErrUnknownOutcome) || !reused.Reused ||
		reused.Attempt == nil || reused.Attempt.Status != ExecutionStatusUnknownOutcome {
		t.Fatalf("bridge retry grafted unlinked direct import = %+v, %v", reused, err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("unlinked direct import retry spawned %d workers", runner.callCount())
	}
}

func TestReconcileFailsClosedWhenHostClockPredatesImportCommit(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("worker must not be called")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "reconcile-clock-regression")
	intent, committed := commitBridgeResultWithoutCompletion(t, fixture, bridge, request)
	*fixture.now = committed.Record.AcceptedAt.Add(-time.Nanosecond)

	if _, reconciled, err := fixture.service.ReconcileExecution(
		t.Context(),
		ExecutionReconcileRequest{Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID},
	); err == nil || reconciled || !strings.Contains(err.Error(), "host clock regressed") {
		t.Fatalf("clock-regressed reconcile = reconciled=%v, err=%v", reconciled, err)
	}
	if _, exists, err := fixture.repository.loadExecutionCompletion(intent); err != nil || exists {
		t.Fatalf("clock regression created completion = %v, %v", exists, err)
	}
	attempt, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
		Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID,
	})
	if err != nil || attempt.Status != ExecutionStatusUnknownOutcome {
		t.Fatalf("clock-regressed attempt = %+v, %v", attempt, err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("clock-regressed reconcile spawned %d workers", runner.callCount())
	}
}

func TestSucceededProjectionRejectsImportCommitAfterHostCompletion(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	bridge, err := NewLocalBridge(fixture.repository, &fakeShadowRunner{behavior: func(
		contractsv1alpha1.AgentReviewWorkerRequest,
	) ([]byte, error) {
		return nil, errors.New("not called")
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "projection-import-after-completion")
	intent, committed := commitBridgeResultWithoutCompletion(t, fixture, bridge, request)
	*fixture.now = committed.Record.AcceptedAt.Add(time.Second)
	if _, reconciled, err := fixture.service.ReconcileExecution(
		t.Context(),
		ExecutionReconcileRequest{Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID},
	); err != nil || !reconciled {
		t.Fatalf("seed reconcile = reconciled=%v, err=%v", reconciled, err)
	}

	completionPath := immutableExecutionObjectPath(
		fixture,
		executionCompletionObjectID(fixture.scope, intent.IdempotencyKey),
	)
	var completion executionCompletion
	readStrictTestJSON(t, completionPath, &completion)
	completion.RecordedAt = committed.Record.AcceptedAt.Add(-time.Nanosecond)
	replaceImmutableTestJSON(t, completionPath, completion)
	if _, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
		Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID,
	}); !errors.Is(err, ErrCorrupt) ||
		!strings.Contains(err.Error(), "result commit is later") {
		t.Fatalf("projection timing error = %v, want ErrCorrupt result-commit ordering", err)
	}
}

func TestExecutionImportAuthorizationRejectsOrphanPathSymlinkAndTamper(t *testing.T) {
	tests := []struct {
		name       string
		orphan     bool
		mutateLink func(*testing.T, *shadowFixture, executionIntent, string)
	}{
		{
			name: "symlink",
			mutateLink: func(t *testing.T, fixture *shadowFixture, _ executionIntent, path string) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(fixture.store.Root(), "execution-import-binding-copy.json")
				if err := os.WriteFile(target, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "forged path",
			mutateLink: func(t *testing.T, _ *shadowFixture, _ executionIntent, path string) {
				forged := filepath.Join(filepath.Dir(path), strings.Repeat("f", 64)+".json")
				if forged == path {
					forged = filepath.Join(filepath.Dir(path), strings.Repeat("e", 64)+".json")
				}
				if err := os.Rename(path, forged); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "orphan",
			orphan: true,
			mutateLink: func(t *testing.T, fixture *shadowFixture, intent executionIntent, _ string) {
				intentPath := immutableExecutionObjectPath(
					fixture,
					executionIntentObjectID(fixture.scope, intent.IdempotencyKey),
				)
				if err := os.Remove(intentPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "payload digest tamper",
			mutateLink: func(t *testing.T, _ *shadowFixture, _ executionIntent, path string) {
				var authorization executionImportAuthorization
				readStrictTestJSON(t, path, &authorization)
				authorization.Plan.SHA256 = strings.Repeat("f", 64)
				replaceImmutableTestJSON(t, path, authorization)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newShadowFixture(t)
			node, script := fakeWorkerPackage(t)
			runner := &fakeShadowRunner{behavior: func(
				contractsv1alpha1.AgentReviewWorkerRequest,
			) ([]byte, error) {
				return nil, errors.New("worker must not be called")
			}}
			bridge, err := NewLocalBridge(fixture.repository, runner)
			if err != nil {
				t.Fatal(err)
			}
			request := localBridgeRequest(fixture, node, script, "binding-"+strings.ReplaceAll(test.name, " ", "-"))
			intent, _ := commitBridgeResultWithoutCompletion(t, fixture, bridge, request)
			linkPath := immutableExecutionObjectPath(
				fixture,
				executionImportAuthorizationObjectID(fixture.scope, intent.IdempotencyKey),
			)
			test.mutateLink(t, fixture, intent, linkPath)

			if _, _, err := fixture.service.ReconcileExecution(
				t.Context(),
				ExecutionReconcileRequest{Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID},
			); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ReconcileExecution error = %v, want ErrCorrupt", err)
			}
			if !test.orphan {
				if _, err := bridge.Run(t.Context(), request); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("LocalBridge retry error = %v, want ErrCorrupt", err)
				}
			}
			if runner.callCount() != 0 {
				t.Fatalf("invalid binding spawned %d workers", runner.callCount())
			}
		})
	}
}

func TestLocalBridgeRetryRejectsUnrelatedOrphanImportAuthorization(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		return nil, errors.New("worker must not be called")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	targetRequest := localBridgeRequest(fixture, node, script, "binding-target")
	commitBridgeResultWithoutCompletion(t, fixture, bridge, targetRequest)
	orphanRequest := localBridgeRequest(fixture, node, script, "binding-unrelated-orphan")
	orphanIntent, _ := commitBridgeResultWithoutCompletion(t, fixture, bridge, orphanRequest)
	orphanIntentPath := immutableExecutionObjectPath(
		fixture,
		executionIntentObjectID(fixture.scope, orphanIntent.IdempotencyKey),
	)
	if err := os.Remove(orphanIntentPath); err != nil {
		t.Fatal(err)
	}

	if _, err := bridge.Run(t.Context(), targetRequest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("LocalBridge retry hid unrelated orphan binding: %v", err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("orphan binding retry spawned %d workers", runner.callCount())
	}
}

func TestConcurrentReconcileAcknowledgesOnlyCompletionCreator(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	bridge, err := NewLocalBridge(fixture.repository, &fakeShadowRunner{behavior: func(
		contractsv1alpha1.AgentReviewWorkerRequest,
	) ([]byte, error) {
		return nil, errors.New("not called")
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := localBridgeRequest(fixture, node, script, "reconcile-concurrent-ack")
	intent, committed := commitBridgeResultWithoutCompletion(t, fixture, bridge, request)
	*fixture.now = committed.Record.AcceptedAt.Add(time.Second)

	type outcome struct {
		attempt    ExecutionAttempt
		reconciled bool
		err        error
	}
	const callers = 16
	start := make(chan struct{})
	results := make(chan outcome, callers)
	for range callers {
		go func() {
			<-start
			attempt, reconciled, err := fixture.service.ReconcileExecution(
				context.Background(),
				ExecutionReconcileRequest{Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID},
			)
			results <- outcome{attempt: attempt, reconciled: reconciled, err: err}
		}()
	}
	close(start)
	created := 0
	for range callers {
		result := <-results
		if result.err != nil || result.attempt.Status != ExecutionStatusSucceeded {
			t.Fatalf("concurrent reconcile = %+v, %v", result.attempt, result.err)
		}
		if result.reconciled {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("reconciled=true count = %d, want exactly one completion creator", created)
	}
}

func TestReconcileRejectsTamperedBoundCommittedResult(t *testing.T) {
	t.Run("tampered import record", func(t *testing.T) {
		fixture := newShadowFixture(t)
		node, script := fakeWorkerPackage(t)
		bridge, err := NewLocalBridge(fixture.repository, &fakeShadowRunner{behavior: func(
			contractsv1alpha1.AgentReviewWorkerRequest,
		) ([]byte, error) {
			return nil, errors.New("not called")
		}})
		if err != nil {
			t.Fatal(err)
		}
		request := localBridgeRequest(fixture, node, script, "reconcile-tampered-import-record")
		intent, committed := commitBridgeResultWithoutCompletion(t, fixture, bridge, request)
		recordPath := immutableExecutionObjectPath(
			fixture,
			importObjectID(committed.Record.ManifestID),
		)
		record := committed.Record
		record.InputDigest = strings.Repeat("f", 64)
		replaceImmutableTestJSON(t, recordPath, record)
		if _, _, err := fixture.service.ReconcileExecution(t.Context(), ExecutionReconcileRequest{
			Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID,
		}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("tampered import record reconcile error = %v, want ErrCorrupt", err)
		}
		if _, exists, err := fixture.repository.loadExecutionCompletion(intent); err != nil || exists {
			t.Fatalf("tampered import record created completion = %v, %v", exists, err)
		}
	})

	t.Run("tampered artifact", func(t *testing.T) {
		fixture := newShadowFixture(t)
		node, script := fakeWorkerPackage(t)
		bridge, err := NewLocalBridge(fixture.repository, &fakeShadowRunner{behavior: func(
			contractsv1alpha1.AgentReviewWorkerRequest,
		) ([]byte, error) {
			return nil, errors.New("not called")
		}})
		if err != nil {
			t.Fatal(err)
		}
		request := localBridgeRequest(fixture, node, script, "reconcile-tampered-result")
		intent, committed := commitBridgeResultWithoutCompletion(t, fixture, bridge, request)
		ref := committed.Record.ReceiptCollectionRef.Ref
		path := filepath.Join(fixture.store.Root(), "artifacts", "sha256", ref.SHA256[:2], ref.SHA256)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("tampered committed receipt"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fixture.service.ReconcileExecution(t.Context(), ExecutionReconcileRequest{
			Scope: fixture.scope, ExecutionID: intent.Plan.ExecutionID,
		}); !errors.Is(err, artifactrepo.ErrQuarantined) {
			t.Fatalf("tampered reconcile error = %v, want ErrQuarantined", err)
		}
		if _, exists, err := fixture.repository.loadExecutionCompletion(intent); err != nil || exists {
			t.Fatalf("tampered result created completion = %v, %v", exists, err)
		}
	})
}

func commitBridgeResultWithoutCompletion(
	t *testing.T,
	fixture *shadowFixture,
	bridge *LocalBridge,
	request LocalRunRequest,
) (executionIntent, Result) {
	t.Helper()
	return importBridgeResultWithoutCompletion(t, fixture, bridge, request, true)
}

func importBridgeResultWithoutCompletion(
	t *testing.T,
	fixture *shadowFixture,
	bridge *LocalBridge,
	request LocalRunRequest,
	authorize bool,
) (executionIntent, Result) {
	t.Helper()
	payload := prepareBridgeImportPayload(t, fixture, bridge, request)
	if authorize {
		if _, err := bridge.authorizeExecutionImport(
			payload.intent,
			payload.plan,
			payload.hypothesisSet,
			payload.rawCandidates,
			payload.taskEvidence,
			payload.receiptCollection,
		); err != nil {
			t.Fatalf("authorize crash-window import: %v", err)
		}
	}
	*fixture.now = payload.intent.Plan.CreatedAt.Add(20 * time.Second)
	committed, err := fixture.service.Import(t.Context(), ImportRequest{
		Scope:                  fixture.scope,
		IdempotencyKey:         request.IdempotencyKey,
		Plan:                   payload.plan,
		HypothesisSet:          payload.hypothesisSet,
		RawCandidateCollection: payload.rawCandidates,
		TaskEvidenceCollection: payload.taskEvidence,
		ReceiptCollection:      payload.receiptCollection,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := fixture.repository.loadExecutionCompletion(payload.intent); err != nil || exists {
		t.Fatalf("crash-window fixture already has completion = %v, %v", exists, err)
	}
	return payload.intent, committed
}

type bridgeImportPayload struct {
	intent            executionIntent
	plan              []byte
	hypothesisSet     []byte
	rawCandidates     []byte
	taskEvidence      []byte
	receiptCollection []byte
}

func prepareBridgeImportPayload(
	t *testing.T,
	fixture *shadowFixture,
	bridge *LocalBridge,
	request LocalRunRequest,
) bridgeImportPayload {
	t.Helper()
	prepared, err := bridge.prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	intent, acquired, err := fixture.repository.acquireExecutionIntent(prepared.intent)
	if err != nil || !acquired {
		t.Fatalf("acquire intent = %+v, %v, %v", intent, acquired, err)
	}
	workerData := fakeSucceededWorkerResult(t, prepared.workerRequest, "clean", 4)
	workerResult, err := contractsv1alpha1.DecodeAgentReviewWorkerResult(workerData)
	if err != nil {
		t.Fatal(err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewWorkerResultBinding(
		prepared.workerRequest,
		workerResult,
	); err != nil {
		t.Fatal(err)
	}
	artifacts, err := pireviewmap.MapShadowReviewReportWithArtifacts(
		prepared.plan,
		prepared.reviewInput,
		workerResult.CompletedAt,
		workerResult.Report,
	)
	if err != nil {
		t.Fatal(err)
	}
	set := artifacts.Hypotheses
	receipts := artifacts.Receipts
	rawCandidates := artifacts.RawCandidates
	marshal := func(value any) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	planData := marshal(prepared.plan)
	setData := marshal(set)
	rawCandidateData := marshal(rawCandidates)
	taskEvidenceData := marshal(artifacts.TaskEvidence)
	receiptData := marshal(receipts)
	return bridgeImportPayload{
		intent:            intent,
		plan:              planData,
		hypothesisSet:     setData,
		rawCandidates:     rawCandidateData,
		taskEvidence:      taskEvidenceData,
		receiptCollection: receiptData,
	}
}

func createUnknownHostFailureAttempt(
	t *testing.T,
	key string,
) (*shadowFixture, ExecutionAttempt) {
	t.Helper()
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		*fixture.now = request.Plan.CreatedAt.Add(time.Second)
		return nil, errors.New("redacted by taxonomy")
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	result, err := bridge.Run(t.Context(), localBridgeRequest(fixture, node, script, key))
	if err == nil || result.Attempt == nil || result.Attempt.HostFailure == nil {
		t.Fatalf("create host failure attempt = %+v, %v", result, err)
	}
	return fixture, *result.Attempt
}

func executionIntentForAttempt(
	t *testing.T,
	fixture *shadowFixture,
	attempt ExecutionAttempt,
) executionIntent {
	t.Helper()
	var intent executionIntent
	if err := fixture.store.GetJSON(
		executionIntentObjectID(attempt.Scope, attempt.IdempotencyKey),
		&intent,
	); err != nil {
		t.Fatal(err)
	}
	return intent
}

func readStrictTestJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func replaceImmutableTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
