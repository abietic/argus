package runrepo

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestAgentStageResultCallbackReceiptRegistryAppendLookupExactRetryAndConflict(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, first)
	if err := second.AppendAgentStageResultCallbackReceipt(
		fixture.receipt,
		fixture.receiptRef,
	); err != nil {
		t.Fatalf("exact callback receipt retry error = %v", err)
	}
	loaded, ref, found, err := second.LookupAgentStageResultCallbackReceipt(
		fixture.gate.ReviewRunID,
		fixture.binding.BindingID,
		fixture.gate.Subject,
	)
	if err != nil || !found || loaded != fixture.receipt || ref != fixture.receiptRef {
		t.Fatalf("callback receipt lookup = (%+v, %+v, %v, %v)", loaded, ref, found, err)
	}

	changedInput := fixture.receipt
	changedInput.ProofSHA256, err = runmodel.AgentStageResultCallbackProofSHA256(
		[]byte("changed-provider-callback-proof"),
	)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := runmodel.SealAgentStageResultCallbackReceipt(changedInput)
	if err != nil {
		t.Fatal(err)
	}
	changedRef, err := first.PutJSONArtifact(
		runmodel.ContractAgentStageResultCallbackReceipt,
		changed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendAgentStageResultCallbackReceipt(changed, changedRef); err == nil ||
		!errors.Is(err, ErrAgentStageResultCallbackReceiptConflict) {
		t.Fatalf("changed callback receipt append error = %v, want conflict", err)
	}

	foreign := fixture.gate.Subject
	foreign.RepositoryID = "repository-2"
	_, _, found, err = second.LookupAgentStageResultCallbackReceipt(
		fixture.gate.ReviewRunID,
		fixture.binding.BindingID,
		foreign,
	)
	if found || !errors.Is(err, ErrAgentStageResultCallbackReceiptSubjectMismatch) {
		t.Fatalf("foreign callback receipt lookup = (%v, %v)", found, err)
	}
}

func TestAgentStageResultCallbackReceiptRegistryRejectsBrokenClosure(t *testing.T) {
	t.Parallel()

	t.Run("missing artifact", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixtureWithReceiptRegistry(t, repository, false)
		missingSHA := strings.Repeat("9", 64)
		missing := runmodel.ArtifactRef{
			URI:       "artifact://local/sha256/" + missingSHA,
			SHA256:    missingSHA,
			SizeBytes: fixture.receiptRef.SizeBytes,
			Contract:  runmodel.ContractAgentStageResultCallbackReceipt,
		}
		if err := repository.AppendAgentStageResultCallbackReceipt(
			fixture.receipt,
			missing,
		); err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing callback receipt artifact error = %v", err)
		}
	})

	t.Run("artifact payload differs from record", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixtureWithReceiptRegistry(t, repository, false)
		other, otherRef := putTerminalCallbackReceipt(t, repository, fixture, func(
			value *runmodel.AgentStageResultCallbackReceipt,
		) {
			value.ProofSHA256 = strings.Repeat("a", 64)
		})
		if other == fixture.receipt {
			t.Fatal("alternate callback receipt fixture did not change")
		}
		if err := repository.AppendAgentStageResultCallbackReceipt(
			fixture.receipt,
			otherRef,
		); err == nil || !strings.Contains(err.Error(), "does not match registry record") {
			t.Fatalf("mismatched callback receipt artifact error = %v", err)
		}
	})

	t.Run("non-canonical artifact", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixtureWithReceiptRegistry(t, repository, false)
		indented, err := json.MarshalIndent(fixture.receipt, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := repository.PutArtifact(
			runmodel.ContractAgentStageResultCallbackReceipt,
			indented,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.AppendAgentStageResultCallbackReceipt(
			fixture.receipt,
			ref,
		); err == nil || !strings.Contains(err.Error(), "not canonical JSON") {
			t.Fatalf("non-canonical callback receipt registry error = %v", err)
		}
	})

	t.Run("wrong binding", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixtureWithReceiptRegistry(t, repository, false)
		receipt, ref := putTerminalCallbackReceipt(t, repository, fixture, func(
			value *runmodel.AgentStageResultCallbackReceipt,
		) {
			value.BindingSHA256 = strings.Repeat("b", 64)
		})
		if err := repository.AppendAgentStageResultCallbackReceipt(receipt, ref); err == nil ||
			!strings.Contains(err.Error(), "exact execution binding and canonical result") {
			t.Fatalf("wrong callback receipt binding error = %v", err)
		}
	})

	t.Run("result does not bind request", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixtureWithReceiptRegistry(t, repository, false)
		wrongResult := fixture.result
		wrongResult.ExecutionID = "another-execution"
		wrongResultRef, err := repository.PutJSONArtifact(
			runmodel.ContractStageExecutionResult,
			wrongResult,
		)
		if err != nil {
			t.Fatal(err)
		}
		receipt, ref := putTerminalCallbackReceipt(t, repository, fixture, func(
			value *runmodel.AgentStageResultCallbackReceipt,
		) {
			value.ResultRef = wrongResultRef
		})
		if err := repository.AppendAgentStageResultCallbackReceipt(receipt, ref); err == nil ||
			!strings.Contains(err.Error(), "exact request fencing") {
			t.Fatalf("wrong callback receipt result error = %v", err)
		}
	})

	for _, test := range []struct {
		name     string
		verified func(terminalCompletionFixture) time.Time
		want     string
	}{
		{
			name: "before binding",
			verified: func(value terminalCompletionFixture) time.Time {
				return value.binding.RecordedAt.Add(-time.Nanosecond)
			},
			want: "predates execution binding",
		},
		{
			name: "before result",
			verified: func(value terminalCompletionFixture) time.Time {
				return value.result.RecordedAt.Add(-time.Nanosecond)
			},
			want: "predates provider result",
		},
		{
			name: "after request deadline",
			verified: func(value terminalCompletionFixture) time.Time {
				return value.request.Deadline.Add(time.Nanosecond)
			},
			want: "after request deadline",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, repository, _ := newAgentDispatchRepositories(t)
			fixture := persistTerminalCompletionFixtureWithReceiptRegistry(t, repository, false)
			receipt, ref := putTerminalCallbackReceipt(t, repository, fixture, func(
				value *runmodel.AgentStageResultCallbackReceipt,
			) {
				value.VerifiedAt = test.verified(fixture)
			})
			if err := repository.AppendAgentStageResultCallbackReceipt(receipt, ref); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("callback receipt registry time error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("receipt bytes corrupt after append", func(t *testing.T) {
		t.Parallel()
		store, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		path := store.Root() + "/artifacts/sha256/" +
			fixture.receiptRef.SHA256[:2] + "/" + fixture.receiptRef.SHA256
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := repository.LookupAgentStageResultCallbackReceipt(
			fixture.gate.ReviewRunID,
			fixture.binding.BindingID,
			fixture.gate.Subject,
		)
		if err == nil || !errors.Is(err, local.ErrCorrupt) {
			t.Fatalf("corrupt callback receipt registry error = %v, want ErrCorrupt", err)
		}
	})
}

func TestAgentStageTerminalCompletionAppendLoadLookupAndExactRetry(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, first)
	if err := first.AppendAgentStageTerminalGate(fixture.gate); err != nil {
		t.Fatalf("AppendAgentStageTerminalGate() error = %v", err)
	}
	if err := second.AppendAgentStageTerminalGate(fixture.gate); err != nil {
		t.Fatalf("exact terminal completion retry error = %v", err)
	}
	loaded, err := first.LoadAgentStageTerminalGate(
		fixture.gate.ReviewRunID,
		fixture.gate.GateID,
		fixture.gate.Subject,
	)
	if err != nil || loaded.SHA256 != fixture.gate.SHA256 {
		t.Fatalf("LoadAgentStageTerminalGate() = (%+v, %v)", loaded, err)
	}
	lookedUp, found, err := second.LookupAgentStageTerminalGate(
		fixture.gate.ReviewRunID,
		fixture.intent.IntentID,
		fixture.gate.Subject,
	)
	if err != nil || !found || lookedUp.SHA256 != fixture.gate.SHA256 {
		t.Fatalf("LookupAgentStageTerminalGate() = (%+v, %v, %v)", lookedUp, found, err)
	}
}

func TestAgentStageTerminalCancellationRequiresIntentButNotBinding(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	fixture := persistTerminalIntentFixture(t, first, false)
	cancel := sealTerminalCancellation(t, fixture)
	if err := first.AppendAgentStageTerminalGate(cancel); err != nil {
		t.Fatalf("cancel without execution binding error = %v", err)
	}
	if err := second.AppendAgentStageTerminalGate(cancel); err != nil {
		t.Fatalf("exact cancellation retry error = %v", err)
	}
	loaded, found, err := first.LookupAgentStageTerminalGate(
		cancel.ReviewRunID,
		cancel.IntentID,
		cancel.Subject,
	)
	if err != nil || !found || loaded.Kind != runmodel.AgentStageCancellationRequested {
		t.Fatalf("cancellation lookup = (%+v, %v, %v)", loaded, found, err)
	}
}

func TestAgentStageTerminalFirstDurableFactWinsBothOrders(t *testing.T) {
	t.Parallel()

	for _, firstKind := range []runmodel.AgentStageTerminalGateKind{
		runmodel.AgentStageCancellationRequested,
		runmodel.AgentStageSucceededResultAccepted,
	} {
		firstKind := firstKind
		t.Run(string(firstKind), func(t *testing.T) {
			t.Parallel()
			_, repository, _ := newAgentDispatchRepositories(t)
			fixture := persistTerminalCompletionFixture(t, repository)
			completion := fixture.gate
			cancellation := sealTerminalCancellation(t, fixture.terminalIntentFixture)
			first, second := completion, cancellation
			if firstKind == runmodel.AgentStageCancellationRequested {
				first, second = cancellation, completion
			}
			if err := repository.AppendAgentStageTerminalGate(first); err != nil {
				t.Fatalf("append first terminal fact: %v", err)
			}
			if err := repository.AppendAgentStageTerminalGate(second); err == nil ||
				!errors.Is(err, ErrAgentStageTerminalGateConflict) {
				t.Fatalf("opposite terminal branch error = %v, want conflict", err)
			}
			winner, found, err := repository.LookupAgentStageTerminalGate(
				first.ReviewRunID,
				first.IntentID,
				first.Subject,
			)
			if err != nil || !found || winner.Kind != first.Kind {
				t.Fatalf("terminal winner = (%+v, %v, %v)", winner, found, err)
			}
		})
	}
}

func TestAgentStageTerminalCancelResultConcurrentRaceHasOneWinner(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, first)
	completion := fixture.gate
	cancellation := sealTerminalCancellation(t, fixture.terminalIntentFixture)
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, repository := range []*Repository{first, second} {
		gate := completion
		if index == 1 {
			gate = cancellation
		}
		repository := repository
		go func() {
			<-start
			results <- repository.AppendAgentStageTerminalGate(gate)
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentStageTerminalGateConflict):
			conflicts++
		default:
			t.Fatalf("concurrent terminal error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("terminal successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}
}

func TestAgentStageTerminalLookupRejectsForeignSubject(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, repository)
	if err := repository.AppendAgentStageTerminalGate(fixture.gate); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*runmodel.AgentPlanningSubject)
	}{
		{"tenant", func(value *runmodel.AgentPlanningSubject) { value.TenantID = "tenant-2" }},
		{"organization", func(value *runmodel.AgentPlanningSubject) { value.OrganizationID = "organization-2" }},
		{"workspace", func(value *runmodel.AgentPlanningSubject) { value.WorkspaceID = "workspace-2" }},
		{"repository", func(value *runmodel.AgentPlanningSubject) { value.RepositoryID = "repository-2" }},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			subject := fixture.gate.Subject
			test.mutate(&subject)
			_, found, err := repository.LookupAgentStageTerminalGate(
				fixture.gate.ReviewRunID,
				fixture.gate.IntentID,
				subject,
			)
			if found || !errors.Is(err, ErrAgentStageTerminalGateSubjectMismatch) {
				t.Fatalf("foreign terminal lookup = found %v, error %v", found, err)
			}
		})
	}
}

func TestAgentStageTerminalRejectsResultAndOutputArtifactDrift(t *testing.T) {
	t.Parallel()

	t.Run("trace manifest presence mismatch", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		input := fixture.gate
		completion := *input.Completion
		completion.TraceManifest = nil
		input.Completion = &completion
		mismatched, err := runmodel.SealAgentStageTerminalGate(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.AppendAgentStageTerminalGate(mismatched); err == nil ||
			!strings.Contains(err.Error(), "trace manifest presence") {
			t.Fatalf("trace-presence terminal append error = %v", err)
		}
	})

	t.Run("non-canonical result JSON", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		indented, err := json.MarshalIndent(fixture.result, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		resultRef, err := repository.PutArtifact(
			runmodel.ContractStageExecutionResult,
			indented,
		)
		if err != nil {
			t.Fatal(err)
		}
		input := fixture.gate
		completion := *input.Completion
		completion.ResultRef = resultRef
		input.Completion = &completion
		nonCanonical, err := runmodel.SealAgentStageTerminalGate(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.AppendAgentStageTerminalGate(nonCanonical); err == nil ||
			!strings.Contains(err.Error(), "not canonical JSON") {
			t.Fatalf("non-canonical result append error = %v", err)
		}
	})

	t.Run("output projection mismatch", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		otherRef, err := repository.PutArtifact(
			runmodel.ContractReviewHypothesisSet,
			[]byte(`{"different":true}`),
		)
		if err != nil {
			t.Fatal(err)
		}
		input := fixture.gate
		completion := *input.Completion
		completion.Output = terminalOutputProjection(otherRef, input.Subject)
		input.Completion = &completion
		drifted, err := runmodel.SealAgentStageTerminalGate(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.AppendAgentStageTerminalGate(drifted); err == nil ||
			!strings.Contains(err.Error(), "output projection") {
			t.Fatalf("output-drift terminal append error = %v", err)
		}
	})

	t.Run("result bytes corrupt after commit", func(t *testing.T) {
		t.Parallel()
		store, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		if err := repository.AppendAgentStageTerminalGate(fixture.gate); err != nil {
			t.Fatal(err)
		}
		path := store.Root() + "/artifacts/sha256/" +
			fixture.gate.Completion.ResultRef.SHA256[:2] + "/" +
			fixture.gate.Completion.ResultRef.SHA256
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := repository.LookupAgentStageTerminalGate(
			fixture.gate.ReviewRunID,
			fixture.gate.IntentID,
			fixture.gate.Subject,
		)
		if err == nil || !errors.Is(err, local.ErrCorrupt) {
			t.Fatalf("corrupt result lookup error = %v, want ErrCorrupt", err)
		}
	})

	t.Run("trace manifest bytes corrupt after commit", func(t *testing.T) {
		t.Parallel()
		store, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		if err := repository.AppendAgentStageTerminalGate(fixture.gate); err != nil {
			t.Fatal(err)
		}
		traceRef := fixture.gate.Completion.TraceManifest.Local
		path := store.Root() + "/artifacts/sha256/" +
			traceRef.SHA256[:2] + "/" + traceRef.SHA256
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := repository.LookupAgentStageTerminalGate(
			fixture.gate.ReviewRunID,
			fixture.gate.IntentID,
			fixture.gate.Subject,
		)
		if err == nil || !errors.Is(err, local.ErrCorrupt) {
			t.Fatalf("corrupt trace lookup error = %v, want ErrCorrupt", err)
		}
	})
}

func TestAgentStageTerminalCompletionRejectsCallbackReceiptDrift(t *testing.T) {
	t.Parallel()

	t.Run("missing receipt artifact", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		input := fixture.gate
		completion := *input.Completion
		missingSHA := strings.Repeat("9", 64)
		completion.CallbackReceiptRef = runmodel.ArtifactRef{
			URI:       "artifact://local/sha256/" + missingSHA,
			SHA256:    missingSHA,
			SizeBytes: fixture.receiptRef.SizeBytes,
			Contract:  runmodel.ContractAgentStageResultCallbackReceipt,
		}
		input.Completion = &completion
		missing := sealTerminalGateWithCompletion(t, input)
		if err := repository.AppendAgentStageTerminalGate(missing); err == nil ||
			!errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing callback receipt append error = %v", err)
		}
	})

	t.Run("non-canonical receipt JSON", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		indented, err := json.MarshalIndent(fixture.receipt, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := repository.PutArtifact(
			runmodel.ContractAgentStageResultCallbackReceipt,
			indented,
		)
		if err != nil {
			t.Fatal(err)
		}
		gate := terminalGateWithReceipt(t, fixture.gate, fixture.receipt, ref)
		if err := repository.AppendAgentStageTerminalGate(gate); err == nil ||
			!strings.Contains(err.Error(), "receipt artifact is not canonical JSON") {
			t.Fatalf("non-canonical callback receipt append error = %v", err)
		}
	})

	t.Run("semantic digest mismatch", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		input := fixture.gate
		completion := *input.Completion
		completion.CallbackReceiptSHA256 = strings.Repeat("f", 64)
		input.Completion = &completion
		mismatched := sealTerminalGateWithCompletion(t, input)
		if err := repository.AppendAgentStageTerminalGate(mismatched); err == nil ||
			!strings.Contains(err.Error(), "semantic digest") {
			t.Fatalf("callback receipt semantic mismatch error = %v", err)
		}
	})

	t.Run("wrong canonical result", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		otherResult := fixture.result
		otherResult.Completeness = "partial"
		otherResult.CompletenessNotes = []string{"fixture alternate result"}
		otherRef, err := repository.PutJSONArtifact(
			runmodel.ContractStageExecutionResult,
			otherResult,
		)
		if err != nil {
			t.Fatal(err)
		}
		receipt, ref := putTerminalCallbackReceipt(t, repository, fixture, func(
			value *runmodel.AgentStageResultCallbackReceipt,
		) {
			value.ResultRef = otherRef
		})
		gate := terminalGateWithReceipt(t, fixture.gate, receipt, ref)
		if err := repository.AppendAgentStageTerminalGate(gate); err == nil ||
			!strings.Contains(err.Error(), "exact execution binding and canonical result") {
			t.Fatalf("wrong callback receipt result error = %v", err)
		}
	})

	t.Run("wrong binding digest", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		receipt, ref := putTerminalCallbackReceipt(t, repository, fixture, func(
			value *runmodel.AgentStageResultCallbackReceipt,
		) {
			value.BindingSHA256 = strings.Repeat("e", 64)
		})
		gate := terminalGateWithReceipt(t, fixture.gate, receipt, ref)
		if err := repository.AppendAgentStageTerminalGate(gate); err == nil ||
			!strings.Contains(err.Error(), "exact execution binding and canonical result") {
			t.Fatalf("wrong callback receipt binding error = %v", err)
		}
	})

	t.Run("inexact verifier", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		invalid := fixture.receipt
		invalid.Verifier.Revision = "latest"
		invalid.SHA256 = ""
		digest, err := runmodel.DigestAgentStageResultCallbackReceipt(invalid)
		if err != nil {
			t.Fatal(err)
		}
		invalid.SHA256 = digest
		ref, err := repository.PutJSONArtifact(
			runmodel.ContractAgentStageResultCallbackReceipt,
			invalid,
		)
		if err != nil {
			t.Fatal(err)
		}
		gate := terminalGateWithReceipt(t, fixture.gate, invalid, ref)
		if err := repository.AppendAgentStageTerminalGate(gate); err == nil ||
			!strings.Contains(err.Error(), "must not use latest") {
			t.Fatalf("inexact callback verifier error = %v", err)
		}
	})

	for _, test := range []struct {
		name     string
		verified func(terminalCompletionFixture) time.Time
		want     string
	}{
		{
			name: "before execution binding",
			verified: func(value terminalCompletionFixture) time.Time {
				return value.binding.RecordedAt.Add(-time.Nanosecond)
			},
			want: "predates execution binding",
		},
		{
			name: "before provider result",
			verified: func(value terminalCompletionFixture) time.Time {
				return value.result.RecordedAt.Add(-time.Nanosecond)
			},
			want: "predates provider result",
		},
		{
			name: "after terminal acceptance",
			verified: func(value terminalCompletionFixture) time.Time {
				return value.gate.Completion.AcceptedAt.Add(time.Nanosecond)
			},
			want: "after terminal accepted_at",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, repository, _ := newAgentDispatchRepositories(t)
			fixture := persistTerminalCompletionFixture(t, repository)
			receipt, ref := putTerminalCallbackReceipt(t, repository, fixture, func(
				value *runmodel.AgentStageResultCallbackReceipt,
			) {
				value.VerifiedAt = test.verified(fixture)
			})
			gate := terminalGateWithReceipt(t, fixture.gate, receipt, ref)
			if err := repository.AppendAgentStageTerminalGate(gate); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("callback receipt time closure error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("receipt bytes corrupt after terminal commit", func(t *testing.T) {
		t.Parallel()
		store, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		if err := repository.AppendAgentStageTerminalGate(fixture.gate); err != nil {
			t.Fatal(err)
		}
		path := store.Root() + "/artifacts/sha256/" +
			fixture.receiptRef.SHA256[:2] + "/" + fixture.receiptRef.SHA256
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("tampered callback receipt"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := repository.LookupAgentStageTerminalGate(
			fixture.gate.ReviewRunID,
			fixture.gate.IntentID,
			fixture.gate.Subject,
		)
		if err == nil || !errors.Is(err, local.ErrCorrupt) {
			t.Fatalf("corrupt callback receipt lookup error = %v, want ErrCorrupt", err)
		}
	})
}

func TestAgentStageTerminalLoadRejectsTamperedPayload(t *testing.T) {
	t.Parallel()

	store, repository, _ := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, repository)
	payload, err := json.Marshal(fixture.gate)
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(
		payload,
		[]byte(`{"schema_version":`),
		[]byte(`{"unknown":true,"schema_version":`),
		1,
	)
	stream, err := agentStageTerminalGateStream(fixture.gate.ReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendJSONL(stream, local.Event{
		ID:      fixture.gate.GateID,
		Schema:  runmodel.AgentStageTerminalGateSchemaVersion,
		Time:    fixture.gate.EventTime(),
		Payload: json.RawMessage(payload),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err = repository.LookupAgentStageTerminalGate(
		fixture.gate.ReviewRunID,
		fixture.gate.IntentID,
		fixture.gate.Subject,
	)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("tampered terminal payload error = %v", err)
	}
}

func TestAgentStageTerminalCompletionRejectsLocallyKnownHigherGeneration(t *testing.T) {
	t.Parallel()

	t.Run("higher generation before append", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		persistHigherTerminalGeneration(t, repository, fixture, true)
		if err := repository.AppendAgentStageTerminalGate(fixture.gate); err == nil ||
			!strings.Contains(err.Error(), "stale") {
			t.Fatalf("stale generation append error = %v", err)
		}
	})

	t.Run("accepted completion remains immutable after later generation", func(t *testing.T) {
		t.Parallel()
		_, repository, _ := newAgentDispatchRepositories(t)
		fixture := persistTerminalCompletionFixture(t, repository)
		if err := repository.AppendAgentStageTerminalGate(fixture.gate); err != nil {
			t.Fatal(err)
		}
		higher := persistHigherTerminalGeneration(t, repository, fixture, false)
		if claimed, err := repository.ClaimAgentStageDispatchIntent(higher); err == nil || claimed ||
			!errors.Is(err, local.ErrEventConflict) {
			t.Fatalf("claim after terminal completion = (%v, %v), want fenced conflict", claimed, err)
		}
		loaded, found, err := repository.LookupAgentStageTerminalGate(
			fixture.gate.ReviewRunID,
			fixture.gate.IntentID,
			fixture.gate.Subject,
		)
		if err != nil || !found || loaded.SHA256 != fixture.gate.SHA256 {
			t.Fatalf(
				"later generation changed accepted terminal fact = (%+v, %v, %v)",
				loaded,
				found,
				err,
			)
		}
	})
}

func TestAgentStageGenerationCoordinatorSerializesHigherClaimAndTerminalCompletion(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, first)
	higher := persistHigherTerminalGeneration(t, first, fixture, false)

	type result struct {
		operation string
		won       bool
		err       error
	}
	results := make(chan result, 2)
	go func() {
		err := first.AppendAgentStageTerminalGate(fixture.gate)
		results <- result{operation: "terminal", won: err == nil, err: err}
	}()
	go func() {
		claimed, err := second.ClaimAgentStageDispatchIntent(higher)
		results <- result{operation: "dispatch", won: err == nil && claimed, err: err}
	}()

	winners := 0
	for range 2 {
		result := <-results
		if result.won {
			winners++
			continue
		}
		if result.err == nil {
			t.Fatalf("%s lost coordinator race without explicit error", result.operation)
		}
	}
	if winners != 1 {
		t.Fatalf("shared generation CAS winners = %d, want exactly one", winners)
	}
}

func TestAgentStageTerminalReservationSurvivesProjectionCrashWindow(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, first)
	if err := first.reserveAgentStageTerminalGate(fixture.gate); err != nil {
		t.Fatalf("reserveAgentStageTerminalGate() error = %v", err)
	}

	loaded, found, err := second.LookupAgentStageTerminalGate(
		fixture.gate.ReviewRunID,
		fixture.gate.IntentID,
		fixture.gate.Subject,
	)
	if err != nil || !found || !agentStageTerminalGatesEqual(loaded, fixture.gate) {
		t.Fatalf("coordinator-only terminal lookup = (%+v, %v, %v)", loaded, found, err)
	}
	higher := persistHigherTerminalGeneration(t, first, fixture, false)
	if claimed, claimErr := second.ClaimAgentStageDispatchIntent(higher); claimErr == nil || claimed ||
		!errors.Is(claimErr, local.ErrEventConflict) {
		t.Fatalf("higher claim after terminal reservation = (%v, %v)", claimed, claimErr)
	}
	if err := first.AppendAgentStageTerminalGate(fixture.gate); err != nil {
		t.Fatalf("project reserved terminal on exact retry: %v", err)
	}
}

type terminalIntentFixture struct {
	admission runmodel.AgentStagePlanAdmission
	request   contractsv1alpha1.StageExecutionRequest
	intent    runmodel.AgentStageDispatchIntent
}

type terminalCompletionFixture struct {
	terminalIntentFixture
	binding    runmodel.AgentStageExecutionBinding
	result     contractsv1alpha1.StageExecutionResult
	receipt    runmodel.AgentStageResultCallbackReceipt
	receiptRef runmodel.ArtifactRef
	gate       runmodel.AgentStageTerminalGate
}

func persistTerminalIntentFixture(
	t *testing.T,
	repository *Repository,
	claim bool,
) terminalIntentFixture {
	t.Helper()
	admission := sealedDispatchRepoAdmission(t)
	if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intentInput := validDispatchRepoIntent(admission)
	request := terminalStageExecutionRequest(t, admission, intentInput)
	requestRef, err := repository.PutJSONArtifact(
		runmodel.ContractStageExecutionRequest,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	intentInput.RequestRef = requestRef
	intentInput.RequestSemanticSHA256 = request.RequestSHA256
	intentInput.CapabilitySHA256 = request.Capability.SHA256
	intent, err := runmodel.SealAgentStageDispatchIntent(intentInput)
	if err != nil {
		t.Fatal(err)
	}
	if claim {
		claimed, err := repository.ClaimAgentStageDispatchIntent(intent)
		if err != nil || !claimed {
			t.Fatalf("ClaimAgentStageDispatchIntent() = (%v, %v)", claimed, err)
		}
	} else if err := repository.AppendAgentStageDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	return terminalIntentFixture{admission: admission, request: request, intent: intent}
}

func persistTerminalCompletionFixture(
	t *testing.T,
	repository *Repository,
) terminalCompletionFixture {
	t.Helper()
	return persistTerminalCompletionFixtureWithReceiptRegistry(t, repository, true)
}

func persistTerminalCompletionFixtureWithReceiptRegistry(
	t *testing.T,
	repository *Repository,
	registerReceipt bool,
) terminalCompletionFixture {
	t.Helper()
	base := persistTerminalIntentFixture(t, repository, true)
	binding := sealedDispatchRepoBinding(t, base.intent)
	if err := repository.AppendAgentStageExecutionBinding(binding); err != nil {
		t.Fatal(err)
	}
	outputRef, err := repository.PutArtifact(
		runmodel.ContractReviewHypothesisSet,
		[]byte(`{"schema_version":"argus.review_hypothesis_set.v1alpha1","fixture":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	output := terminalOutputProjection(outputRef, base.intent.Subject)
	traceRef, err := repository.PutArtifact(
		runmodel.ContractStageExecutionTraceManifest,
		[]byte(`{"schema_version":"hailix.trace_manifest.v1alpha1","fixture":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	trace := terminalOutputProjection(traceRef, base.intent.Subject)
	result := contractsv1alpha1.StageExecutionResult{
		SchemaVersion:  contractsv1alpha1.StageExecutionResultSchemaVersion,
		RequestSHA256:  base.request.RequestSHA256,
		ExecutionID:    base.request.ExecutionID,
		Attempt:        base.request.Attempt,
		Generation:     base.request.Generation,
		FencingToken:   base.request.FencingToken,
		IdempotencyKey: base.request.IdempotencyKey,
		Status:         contractsv1alpha1.StageExecutionSucceeded,
		Output: &contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:       output.Governed.URI,
				SHA256:    output.Governed.SHA256,
				SizeBytes: output.Governed.SizeBytes,
			},
			Contract: output.Governed.Contract,
		},
		TraceManifest: &contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:       trace.Governed.URI,
				SHA256:    trace.Governed.SHA256,
				SizeBytes: trace.Governed.SizeBytes,
			},
			Contract: trace.Governed.Contract,
		},
		Completeness:      "complete",
		CompletenessNotes: []string{},
		CapabilitySHA256:  base.request.Capability.SHA256,
		RecordedAt:        base.intent.RecordedAt.Add(2 * time.Minute),
	}
	resultRef, err := repository.PutJSONArtifact(
		runmodel.ContractStageExecutionResult,
		result,
	)
	if err != nil {
		t.Fatal(err)
	}
	providerHandleSHA256, err := runmodel.AgentStageProviderHandleSHA256(
		binding.ProviderHandle,
	)
	if err != nil {
		t.Fatal(err)
	}
	proofSHA256, err := runmodel.AgentStageResultCallbackProofSHA256(
		[]byte("fixture-one-time-provider-callback-proof"),
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := runmodel.SealAgentStageResultCallbackReceipt(
		runmodel.AgentStageResultCallbackReceipt{
			SchemaVersion:        runmodel.AgentStageResultCallbackReceiptSchemaVersion,
			Kind:                 runmodel.AgentStageProviderCallbackVerified,
			Subject:              binding.Subject,
			ReviewRunID:          binding.ReviewRunID,
			Stage:                binding.Stage,
			BindingID:            binding.BindingID,
			BindingSHA256:        binding.SHA256,
			ProviderHandleSHA256: providerHandleSHA256,
			ResultRef:            resultRef,
			ProofSHA256:          proofSHA256,
			Verifier: runmodel.AgentStageResultCallbackVerifierRef{
				ID:       "fixture-callback-verifier",
				Revision: "v1",
				SHA256:   strings.Repeat("c", 64),
			},
			VerifiedAt: result.RecordedAt.Add(30 * time.Second),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	receiptRef, err := repository.PutJSONArtifact(
		runmodel.ContractAgentStageResultCallbackReceipt,
		receipt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if registerReceipt {
		if err := repository.AppendAgentStageResultCallbackReceipt(receipt, receiptRef); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := runmodel.SealAgentStageTerminalGate(runmodel.AgentStageTerminalGate{
		SchemaVersion:         runmodel.AgentStageTerminalGateSchemaVersion,
		Kind:                  runmodel.AgentStageSucceededResultAccepted,
		Subject:               base.intent.Subject,
		ReviewRunID:           base.intent.ReviewRunID,
		Stage:                 base.intent.Stage,
		AdmissionID:           base.intent.AdmissionID,
		AdmissionSHA256:       base.intent.AdmissionSHA256,
		IntentID:              base.intent.IntentID,
		IntentSHA256:          base.intent.SHA256,
		WorkloadID:            base.intent.WorkloadID,
		LeaseID:               base.intent.LeaseID,
		LeaseWorker:           base.intent.LeaseWorker,
		ExecutionID:           base.intent.ExecutionID,
		Attempt:               base.intent.Attempt,
		Generation:            base.intent.Generation,
		FencingToken:          base.intent.FencingToken,
		RequestRef:            base.intent.RequestRef,
		RequestSemanticSHA256: base.intent.RequestSemanticSHA256,
		RequestDeadline:       base.request.Deadline,
		CapabilitySHA256:      base.intent.CapabilitySHA256,
		Completion: &runmodel.AgentStageSucceededResultAcceptance{
			BindingID:             binding.BindingID,
			BindingSHA256:         binding.SHA256,
			ResultRef:             resultRef,
			Output:                output,
			TraceManifest:         &trace,
			CallbackReceiptRef:    receiptRef,
			CallbackReceiptSHA256: receipt.SHA256,
			Status:                string(result.Status),
			Completeness:          result.Completeness,
			CompletenessNotes: append(
				make([]string, 0, len(result.CompletenessNotes)),
				result.CompletenessNotes...,
			),
			ProviderResultRecordedAt: result.RecordedAt,
			AcceptedAt:               result.RecordedAt.Add(time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return terminalCompletionFixture{
		terminalIntentFixture: base,
		binding:               binding,
		result:                result,
		receipt:               receipt,
		receiptRef:            receiptRef,
		gate:                  gate,
	}
}

func terminalStageExecutionRequest(
	t *testing.T,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
) contractsv1alpha1.StageExecutionRequest {
	t.Helper()
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind:     "pi-agent",
		RuntimeID:       "pi-agent-1",
		RuntimeRevision: "1",
		RuntimeSHA256:   dispatchRepoCapabilitySHA,
		BuildIdentity:   "build-1",
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:       []string{},
			ModelEgress:        contractsv1alpha1.AgentStageModelEgressProviderBrokerOnly,
			ToolNetwork:        contractsv1alpha1.AgentStageSideEffectsDeny,
			WorkspaceReads:     contractsv1alpha1.AgentStageWorkspaceReadFrozenInputOnly,
			WorkspaceWrites:    contractsv1alpha1.AgentStageSideEffectsDeny,
			RemoteWrites:       contractsv1alpha1.AgentStageSideEffectsDeny,
			MaxDelegationDepth: 0,
		},
		Trust: contractsv1alpha1.ExecutorTrust{
			Authority: contractsv1alpha1.ExecutorTrustAuthorityLocalHost,
			CapabilityVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-capability-verifier", Revision: "1", SHA256: dispatchRepoCapabilitySHA,
			},
			CallbackVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-callback-verifier", Revision: "1", SHA256: dispatchRepoCapabilitySHA,
			},
		},
	}
	digest, err := contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	capability.SHA256 = digest
	binding := func(value runmodel.GovernedArtifactBinding) contractsv1alpha1.ArtifactBinding {
		return contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI: value.URI, SHA256: value.SHA256, SizeBytes: value.SizeBytes,
			},
			Contract: value.Contract,
		}
	}
	request, err := contractsv1alpha1.SealStageExecutionRequest(
		contractsv1alpha1.StageExecutionRequest{
			SchemaVersion: contractsv1alpha1.StageExecutionRequestSchemaVersion,
			ExecutionID:   intent.ExecutionID,
			ReviewRunID:   intent.ReviewRunID,
			TenantID:      intent.Subject.TenantID,
			WorkspaceID:   intent.Subject.WorkspaceID,
			WorkloadID:    intent.WorkloadID,
			LeaseID:       intent.LeaseID,
			LeaseWorkerID: intent.LeaseWorker,
			Stage: contractsv1alpha1.VersionedRef{
				ID: intent.Stage.ID, Revision: intent.Stage.Revision, SHA256: intent.Stage.SHA256,
			},
			Attempt:        intent.Attempt,
			Generation:     intent.Generation,
			FencingToken:   intent.FencingToken,
			IdempotencyKey: intent.CreateIdempotencyKey,
			Plan:           binding(admission.Plan.Governed),
			ExecutionSnapshot: binding(
				admission.Sources.ExecutionSnapshot.Governed,
			),
			ReviewInput:    binding(admission.Sources.ReviewInput.Governed),
			Upstream:       []contractsv1alpha1.ArtifactBinding{},
			OutputContract: runmodel.ContractReviewHypothesisSet,
			Capability:     capability,
			Deadline:       intent.RecordedAt.Add(10 * time.Minute),
			SideEffects:    contractsv1alpha1.AgentStageSideEffectsDeny,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func terminalOutputProjection(
	localRef runmodel.ArtifactRef,
	subject runmodel.AgentPlanningSubject,
) runmodel.AgentArtifactProjection {
	return runmodel.AgentArtifactProjection{
		Local: localRef,
		Governed: runmodel.GovernedArtifactBinding{
			URI: "artifact://argus-local/tenants/" + subject.TenantID +
				"/workspaces/" + subject.WorkspaceID + "/objects/" + localRef.SHA256,
			SHA256:    localRef.SHA256,
			SizeBytes: localRef.SizeBytes,
			Contract:  localRef.Contract,
		},
	}
}

func sealTerminalGateWithCompletion(
	t *testing.T,
	input runmodel.AgentStageTerminalGate,
) runmodel.AgentStageTerminalGate {
	t.Helper()
	gate, err := runmodel.SealAgentStageTerminalGate(input)
	if err != nil {
		t.Fatal(err)
	}
	return gate
}

func terminalGateWithReceipt(
	t *testing.T,
	input runmodel.AgentStageTerminalGate,
	receipt runmodel.AgentStageResultCallbackReceipt,
	ref runmodel.ArtifactRef,
) runmodel.AgentStageTerminalGate {
	t.Helper()
	completion := *input.Completion
	completion.CallbackReceiptRef = ref
	completion.CallbackReceiptSHA256 = receipt.SHA256
	input.Completion = &completion
	return sealTerminalGateWithCompletion(t, input)
}

func putTerminalCallbackReceipt(
	t *testing.T,
	repository *Repository,
	fixture terminalCompletionFixture,
	mutate func(*runmodel.AgentStageResultCallbackReceipt),
) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef) {
	t.Helper()
	input := fixture.receipt
	mutate(&input)
	receipt, err := runmodel.SealAgentStageResultCallbackReceipt(input)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repository.PutJSONArtifact(
		runmodel.ContractAgentStageResultCallbackReceipt,
		receipt,
	)
	if err != nil {
		t.Fatal(err)
	}
	return receipt, ref
}

func sealTerminalCancellation(
	t *testing.T,
	fixture terminalIntentFixture,
) runmodel.AgentStageTerminalGate {
	t.Helper()
	gate, err := runmodel.SealAgentStageTerminalGate(runmodel.AgentStageTerminalGate{
		SchemaVersion:         runmodel.AgentStageTerminalGateSchemaVersion,
		Kind:                  runmodel.AgentStageCancellationRequested,
		Subject:               fixture.intent.Subject,
		ReviewRunID:           fixture.intent.ReviewRunID,
		Stage:                 fixture.intent.Stage,
		AdmissionID:           fixture.intent.AdmissionID,
		AdmissionSHA256:       fixture.intent.AdmissionSHA256,
		IntentID:              fixture.intent.IntentID,
		IntentSHA256:          fixture.intent.SHA256,
		WorkloadID:            fixture.intent.WorkloadID,
		LeaseID:               fixture.intent.LeaseID,
		LeaseWorker:           fixture.intent.LeaseWorker,
		ExecutionID:           fixture.intent.ExecutionID,
		Attempt:               fixture.intent.Attempt,
		Generation:            fixture.intent.Generation,
		FencingToken:          fixture.intent.FencingToken,
		RequestRef:            fixture.intent.RequestRef,
		RequestSemanticSHA256: fixture.intent.RequestSemanticSHA256,
		RequestDeadline:       fixture.request.Deadline,
		CapabilitySHA256:      fixture.intent.CapabilitySHA256,
		Cancellation: &runmodel.AgentStageCancellationRequest{
			CancelIdempotencyKey: fixture.intent.CancelIdempotencyKey,
			Actor:                "user-1",
			Reason:               "review canceled by user",
			RequestedAt:          fixture.intent.RecordedAt.Add(3 * time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return gate
}

func persistHigherTerminalGeneration(
	t *testing.T,
	repository *Repository,
	fixture terminalCompletionFixture,
	claim bool,
) runmodel.AgentStageDispatchIntent {
	t.Helper()
	input := validDispatchRepoIntent(fixture.admission)
	input.LeaseID = input.WorkloadID + "-g2"
	input.LeaseWorker = "worker-2"
	input.ExecutionID = "execution-dispatch-2"
	input.Generation = fixture.intent.Generation + 1
	input.FencingToken = fixture.intent.FencingToken + 1
	input.CreateIdempotencyKey = "create-execution-dispatch-2"
	input.CancelIdempotencyKey = "cancel-execution-dispatch-2"
	input.RecordedAt = fixture.intent.RecordedAt.Add(5 * time.Minute)
	request := terminalStageExecutionRequest(t, fixture.admission, input)
	requestRef, err := repository.PutJSONArtifact(
		runmodel.ContractStageExecutionRequest,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	input.RequestRef = requestRef
	input.RequestSemanticSHA256 = request.RequestSHA256
	input.CapabilitySHA256 = request.Capability.SHA256
	intent, err := runmodel.SealAgentStageDispatchIntent(input)
	if err != nil {
		t.Fatal(err)
	}
	if claim {
		if claimed, err := repository.ClaimAgentStageDispatchIntent(intent); err != nil || !claimed {
			t.Fatalf("ClaimAgentStageDispatchIntent(higher) = (%v, %v)", claimed, err)
		}
	} else if err := repository.AppendAgentStageDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	return intent
}
