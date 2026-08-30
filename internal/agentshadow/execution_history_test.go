package agentshadow

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/artifactrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestExecutionHistoryProjectsAllStatusesAndStrictClosure(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		switch {
		case strings.Contains(request.IdempotencyKey, "unknown"):
			return nil, errors.New("transport disappeared after spawn")
		case strings.Contains(request.IdempotencyKey, "failed"):
			output := fakeFailedWorkerResult(t, request, "provider secret")
			*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
			return output, nil
		case strings.Contains(request.IdempotencyKey, "canceled"):
			output := fakeCanceledWorkerResult(t, request)
			*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
			return output, nil
		default:
			output := fakeSucceededWorkerResult(t, request, "clean", 4)
			*fixture.now = request.Plan.CreatedAt.Add(20 * time.Second)
			return output, nil
		}
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}

	succeeded, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "history-succeeded"),
	)
	if err != nil {
		t.Fatalf("succeeded run: %v", err)
	}
	assertExecutionAttemptStatus(t, succeeded.Attempt, ExecutionStatusSucceeded)
	if !succeeded.OutcomeAcknowledged || succeeded.Attempt.Manifest == nil ||
		succeeded.Attempt.Manifest.ManifestID != succeeded.Result.Manifest.ManifestID ||
		succeeded.Attempt.Failure != nil {
		t.Fatalf("succeeded attempt closure = %+v", succeeded.Attempt)
	}

	failed, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "history-failed"),
	)
	var reported *WorkerReportedError
	if !errors.As(err, &reported) {
		t.Fatalf("failed run error = %v, want WorkerReportedError", err)
	}
	assertExecutionAttemptStatus(t, failed.Attempt, ExecutionStatusFailed)
	if !failed.OutcomeAcknowledged || failed.Attempt.Failure == nil ||
		failed.Attempt.Failure.Message != redactedWorkerFailureMessage ||
		failed.Attempt.Manifest != nil {
		t.Fatalf("failed attempt evidence = %+v", failed.Attempt)
	}

	canceled, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "history-canceled"),
	)
	reported = nil
	if !errors.As(err, &reported) || reported.Status != contractsv1alpha1.AgentReviewWorkerCanceled {
		t.Fatalf("canceled run error = %#v, %v", reported, err)
	}
	assertExecutionAttemptStatus(t, canceled.Attempt, ExecutionStatusCanceled)
	if !canceled.OutcomeAcknowledged {
		t.Fatal("canceled terminal outcome was not acknowledged")
	}

	unknown, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "history-unknown"),
	)
	if err == nil || !strings.Contains(err.Error(), "retry is unsafe") {
		t.Fatalf("unknown run error = %v", err)
	}
	assertExecutionAttemptStatus(t, unknown.Attempt, ExecutionStatusUnknownOutcome)
	if unknown.OutcomeAcknowledged || unknown.Attempt.CompletionSHA256 != nil || unknown.Attempt.CompletedAt != nil ||
		unknown.Attempt.Failure != nil || unknown.Attempt.Manifest != nil ||
		unknown.Attempt.HostFailure == nil ||
		!unknown.Attempt.ObservedAt.Equal(unknown.Attempt.HostFailure.ObservedAt) {
		t.Fatalf("unknown attempt has terminal evidence: %+v", unknown.Attempt)
	}
	reusedUnknown, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "history-unknown"),
	)
	if !errors.Is(err, ErrUnknownOutcome) || !reusedUnknown.Reused {
		t.Fatalf("reused unknown = %+v, %v", reusedUnknown, err)
	}
	assertExecutionAttemptStatus(t, reusedUnknown.Attempt, ExecutionStatusUnknownOutcome)
	if reusedUnknown.OutcomeAcknowledged {
		t.Fatal("reused unknown outcome was incorrectly acknowledged")
	}

	for _, result := range []LocalRunResult{succeeded, failed, canceled, unknown} {
		attempt := result.Attempt
		queried, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
			Scope: fixture.scope, ExecutionID: attempt.ExecutionID,
		})
		if err != nil || queried.IntentSHA256 != attempt.IntentSHA256 ||
			queried.Status != attempt.Status {
			t.Fatalf("QueryExecution(%q) = %+v, %v", attempt.ExecutionID, queried, err)
		}
		assertCanonicalExecutionDigests(t, fixture, queried)
	}
	if _, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
		Scope:       Scope{TenantID: "another-tenant", WorkspaceID: "another-workspace"},
		ExecutionID: succeeded.Attempt.ExecutionID,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-scope query error = %v, want ErrNotFound", err)
	}

	listed, err := fixture.service.ListExecutions(t.Context(), ExecutionListRequest{
		Scope:          fixture.scope,
		StartInclusive: succeeded.Attempt.AcceptedAt.Add(-time.Nanosecond),
		EndExclusive:   unknown.Attempt.ObservedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ListExecutions(): %v", err)
	}
	if len(listed) != 4 {
		t.Fatalf("listed attempts = %d, want 4: %+v", len(listed), listed)
	}
	for _, attempt := range listed {
		if attempt.Status == ExecutionStatusUnknownOutcome {
			if attempt.HostFailure == nil || !attempt.ObservedAt.Equal(attempt.HostFailure.ObservedAt) {
				t.Fatalf("unknown observed_at does not bind host failure: %+v", attempt)
			}
		} else if attempt.CompletedAt == nil || attempt.ObservedAt.Before(attempt.AcceptedAt) {
			t.Fatalf("terminal observed_at is not a host observation: %+v", attempt)
		}
	}
	if succeeded.Attempt.CompletedAt == nil ||
		succeeded.Attempt.ObservedAt.Equal(*succeeded.Attempt.CompletedAt) {
		t.Fatalf("succeeded attempt window still trusts worker completed_at: %+v", succeeded.Attempt)
	}

	// A succeeded completion is not trusted on its own. Corrupting one governed
	// receipt artifact must make both execution query and history fail closed.
	ref := succeeded.Result.Record.ReceiptCollectionRef
	path := filepath.Join(
		fixture.store.Root(),
		"artifacts",
		"sha256",
		ref.Ref.SHA256[:2],
		ref.Ref.SHA256,
	)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered execution history receipt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
		Scope: fixture.scope, ExecutionID: succeeded.Attempt.ExecutionID,
	}); !errors.Is(err, artifactrepo.ErrQuarantined) {
		t.Fatalf("tampered QueryExecution error = %v, want ErrQuarantined", err)
	}
	if _, err := fixture.service.ListExecutions(t.Context(), ExecutionListRequest{
		Scope: fixture.scope, StartInclusive: succeeded.Attempt.AcceptedAt.Add(-time.Second),
		EndExclusive: unknown.Attempt.ObservedAt.Add(time.Minute),
	}); !errors.Is(err, artifactrepo.ErrQuarantined) {
		t.Fatalf("tampered ListExecutions error = %v, want ErrQuarantined", err)
	}
}

func TestLocalBridgeReturnsUnknownAttemptForEveryPostIntentFailure(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		switch {
		case strings.Contains(request.IdempotencyKey, "runner"):
			return nil, errors.New("runner transport failure")
		case strings.Contains(request.IdempotencyKey, "decode"):
			return []byte(`{"not":"a worker result"}`), nil
		case strings.Contains(request.IdempotencyKey, "import"):
			output := fakeSucceededWorkerResult(t, request, "clean", 4)
			*fixture.now = time.Time{}
			return output, nil
		default:
			t.Fatalf("unexpected request %q", request.IdempotencyKey)
			return nil, nil
		}
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		key        string
		want       string
		wantStage  ExecutionHostFailureStage
		wantReason ExecutionHostFailureCode
	}{
		{
			key: "post-intent-runner", want: "retry is unsafe",
			wantStage: ExecutionHostFailureWorkerRun, wantReason: ExecutionHostFailureRunnerUnconfirmed,
		},
		{
			key: "post-intent-decode", want: "reject worker result",
			wantStage: ExecutionHostFailureResultDecode, wantReason: ExecutionHostFailureResultRejected,
		},
		{
			key: "post-intent-import", want: "import mapped shadow evidence",
			wantStage: ExecutionHostFailureShadowImport, wantReason: ExecutionHostFailureImportUnconfirmed,
		},
	}
	for _, test := range tests {
		if fixture.now.IsZero() {
			*fixture.now = time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC)
		}
		result, err := bridge.Run(
			t.Context(),
			localBridgeRequest(fixture, node, script, test.key),
		)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s error = %v, want %q", test.key, err, test.want)
		}
		assertExecutionAttemptStatus(t, result.Attempt, ExecutionStatusUnknownOutcome)
		if result.OutcomeAcknowledged || result.Attempt.HostFailure == nil ||
			result.Attempt.HostFailure.Stage != test.wantStage ||
			result.Attempt.HostFailure.ReasonCode != test.wantReason {
			t.Fatalf("%s host observation = %+v", test.key, result.Attempt.HostFailure)
		}
		queried, queryErr := fixture.service.QueryExecution(t.Context(), ExecutionQueryRequest{
			Scope: fixture.scope, ExecutionID: result.Attempt.ExecutionID,
		})
		if queryErr != nil || queried.Status != ExecutionStatusUnknownOutcome {
			t.Fatalf("query %s unknown attempt = %+v, %v", test.key, queried, queryErr)
		}
	}
}

func TestLocalBridgeRejectsWorkerCompletionAfterHostObservation(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	runner := &fakeShadowRunner{behavior: func(request contractsv1alpha1.AgentReviewWorkerRequest) ([]byte, error) {
		// The worker timestamp is inside the request deadline but ahead of the
		// host clock at receipt. It cannot own or move the terminal window.
		return fakeFailedWorkerResult(t, request, "untrusted future timestamp"), nil
	}}
	bridge, err := NewLocalBridge(fixture.repository, runner)
	if err != nil {
		t.Fatal(err)
	}
	result, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "history-future-worker-time"),
	)
	if err == nil || !strings.Contains(err.Error(), "invalid time binding") {
		t.Fatalf("future worker timestamp error = %v", err)
	}
	assertExecutionAttemptStatus(t, result.Attempt, ExecutionStatusUnknownOutcome)
	if result.Attempt.CompletedAt != nil || result.Attempt.CompletionSHA256 != nil {
		t.Fatalf("future worker timestamp created terminal evidence: %+v", result.Attempt)
	}
}

func TestExecutionHistoryRejectsSymlinkUnexpectedAndForgedIntentEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *shadowFixture, string)
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, fixture *shadowFixture, objectPath string) {
				data, err := os.ReadFile(objectPath)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(fixture.store.Root(), "intent-copy.json")
				if err := os.WriteFile(target, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(objectPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, objectPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unexpected entry",
			mutate: func(t *testing.T, _ *shadowFixture, objectPath string) {
				if err := os.WriteFile(filepath.Join(filepath.Dir(objectPath), "unexpected.txt"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "forged path",
			mutate: func(t *testing.T, _ *shadowFixture, objectPath string) {
				forged := filepath.Join(filepath.Dir(objectPath), strings.Repeat("f", 64)+".json")
				if forged == objectPath {
					forged = filepath.Join(filepath.Dir(objectPath), strings.Repeat("e", 64)+".json")
				}
				if err := os.Rename(objectPath, forged); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
			request := localBridgeRequest(fixture, node, script, "history-path-"+strings.ReplaceAll(test.name, " ", "-"))
			prepared, err := bridge.prepare(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, acquired, err := fixture.repository.acquireExecutionIntent(prepared.intent); err != nil || !acquired {
				t.Fatalf("acquire intent = %v, %v", acquired, err)
			}
			objectPath := immutableExecutionObjectPath(
				fixture,
				executionIntentObjectID(fixture.scope, request.IdempotencyKey),
			)
			test.mutate(t, fixture, objectPath)
			_, err = fixture.service.ListExecutions(t.Context(), ExecutionListRequest{
				Scope:          fixture.scope,
				StartInclusive: prepared.intent.AcceptedAt.Add(-time.Second),
				EndExclusive:   prepared.intent.AcceptedAt.Add(time.Second),
			})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ListExecutions corruption error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestExecutionListWindowIsHalfOpen(t *testing.T) {
	fixture := newShadowFixture(t)
	node, script := fakeWorkerPackage(t)
	bridge, err := NewLocalBridge(fixture.repository, &fakeShadowRunner{behavior: func(
		contractsv1alpha1.AgentReviewWorkerRequest,
	) ([]byte, error) {
		return nil, errors.New("unknown result")
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bridge.Run(
		t.Context(),
		localBridgeRequest(fixture, node, script, "history-window"),
	)
	if err == nil {
		t.Fatal("unknown runner must fail")
	}
	observed := result.Attempt.ObservedAt
	listed, err := fixture.service.ListExecutions(t.Context(), ExecutionListRequest{
		Scope: fixture.scope, StartInclusive: observed, EndExclusive: observed.Add(time.Nanosecond),
	})
	if err != nil || len(listed) != 1 {
		t.Fatalf("inclusive start list = %+v, %v", listed, err)
	}
	listed, err = fixture.service.ListExecutions(t.Context(), ExecutionListRequest{
		Scope: fixture.scope, StartInclusive: observed.Add(-time.Nanosecond), EndExclusive: observed,
	})
	if err != nil || len(listed) != 0 {
		t.Fatalf("exclusive end list = %+v, %v", listed, err)
	}
}

func assertExecutionAttemptStatus(
	t *testing.T,
	attempt *ExecutionAttempt,
	want ExecutionStatus,
) {
	t.Helper()
	if attempt == nil || attempt.Status != want {
		t.Fatalf("execution attempt = %+v, want status %q", attempt, want)
	}
	if err := validateExecutionAttempt(*attempt); err != nil {
		t.Fatalf("execution attempt is invalid: %v\n%+v", err, attempt)
	}
}

func assertCanonicalExecutionDigests(
	t *testing.T,
	fixture *shadowFixture,
	attempt ExecutionAttempt,
) {
	t.Helper()
	intentPath := immutableExecutionObjectPath(
		fixture,
		executionIntentObjectID(attempt.Scope, attempt.IdempotencyKey),
	)
	intentData, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := digestBytesRaw(bytes.TrimSuffix(intentData, []byte("\n"))); got != attempt.IntentSHA256 {
		t.Fatalf("intent digest = %s, want persisted canonical digest %s", attempt.IntentSHA256, got)
	}
	if attempt.HostFailureObservationSHA256 != nil {
		observationPath := immutableExecutionObjectPath(
			fixture,
			executionHostFailureObjectID(attempt.Scope, attempt.IdempotencyKey),
		)
		observationData, err := os.ReadFile(observationPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := digestBytesRaw(bytes.TrimSuffix(observationData, []byte("\n"))); got != *attempt.HostFailureObservationSHA256 {
			t.Fatalf("host observation digest = %s, want persisted canonical digest %s", *attempt.HostFailureObservationSHA256, got)
		}
	}
	if attempt.CompletionSHA256 == nil {
		return
	}
	completionPath := immutableExecutionObjectPath(
		fixture,
		executionCompletionObjectID(attempt.Scope, attempt.IdempotencyKey),
	)
	completionData, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := digestBytesRaw(bytes.TrimSuffix(completionData, []byte("\n"))); got != *attempt.CompletionSHA256 {
		t.Fatalf("completion digest = %s, want persisted canonical digest %s", *attempt.CompletionSHA256, got)
	}
}

func immutableExecutionObjectPath(fixture *shadowFixture, objectID string) string {
	return filepath.Join(
		fixture.store.Root(),
		"immutable",
		filepath.FromSlash(objectID)+".json",
	)
}

func fakeCanceledWorkerResult(
	t *testing.T,
	request contractsv1alpha1.AgentReviewWorkerRequest,
) []byte {
	t.Helper()
	result := contractsv1alpha1.AgentReviewWorkerResult{
		SchemaVersion:    contractsv1alpha1.AgentReviewWorkerResultSchemaVersion,
		WorkItemID:       request.WorkItemID,
		Attempt:          request.Attempt,
		Generation:       request.Generation,
		FencingToken:     request.FencingToken,
		IdempotencyKey:   request.IdempotencyKey,
		CapabilitySHA256: request.Capability.SHA256,
		Status:           contractsv1alpha1.AgentReviewWorkerCanceled,
		Failure: &contractsv1alpha1.AgentReviewWorkerFailure{
			Code: "deadline_exceeded", Message: "untrusted cancellation detail", Retryable: true,
		},
		CompletedAt: request.Plan.CreatedAt.Add(10 * time.Second),
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
