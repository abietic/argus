package runmodel

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	agentTerminalResultSHA          = "8111111111111111111111111111111111111111111111111111111111111111"
	agentTerminalOutputSHA          = "8222222222222222222222222222222222222222222222222222222222222222"
	agentTerminalTraceSHA           = "8333333333333333333333333333333333333333333333333333333333333333"
	agentTerminalReceiptArtifactSHA = "8444444444444444444444444444444444444444444444444444444444444444"
	agentTerminalReceiptSemanticSHA = "8555555555555555555555555555555555555555555555555555555555555555"
	agentTerminalVerifierSHA        = "8666666666666666666666666666666666666666666666666666666666666666"
)

func TestAgentStageTerminalGateSealsCompletionAndCancellationUnderOneAuthority(t *testing.T) {
	t.Parallel()

	completion, admission, intent, binding := validSealedAgentStageTerminalCompletion(t)
	if err := completion.ValidateAgainstAdmissionAndIntent(admission, intent); err != nil {
		t.Fatalf("ValidateAgainstAdmissionAndIntent() error = %v", err)
	}
	if err := completion.ValidateCompletionAgainstBinding(binding); err != nil {
		t.Fatalf("ValidateCompletionAgainstBinding() error = %v", err)
	}
	cancelInput := validAgentStageTerminalGate(admission, intent, binding)
	cancelInput.Kind = AgentStageCancellationRequested
	cancelInput.Completion = nil
	cancelInput.Cancellation = &AgentStageCancellationRequest{
		CancelIdempotencyKey: intent.CancelIdempotencyKey,
		Actor:                "user-1",
		Reason:               "review canceled by user",
		RequestedAt:          intent.RecordedAt.Add(2 * time.Minute),
	}
	cancellation, err := SealAgentStageTerminalGate(cancelInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := cancellation.ValidateAgainstAdmissionAndIntent(admission, intent); err != nil {
		t.Fatalf("cancel closure error = %v", err)
	}
	if completion.GateID != cancellation.GateID || completion.SHA256 == cancellation.SHA256 {
		t.Fatalf(
			"terminal branches must share ID and differ in digest: completion=%+v cancellation=%+v",
			completion,
			cancellation,
		)
	}
	if !completion.EventTime().Equal(completion.Completion.AcceptedAt) ||
		!cancellation.EventTime().Equal(cancellation.Cancellation.RequestedAt) {
		t.Fatal("EventTime() did not use host-observed branch timestamp")
	}
}

func TestAgentStageTerminalGateSealsFailedAndCanceledProviderOutcomes(t *testing.T) {
	t.Parallel()

	completion, admission, intent, binding := validSealedAgentStageTerminalCompletion(t)
	failedInput := validAgentStageTerminalOutcomeGate(
		admission,
		intent,
		binding,
		AgentStageFailedResultAccepted,
	)
	failed, err := SealAgentStageTerminalGate(failedInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.ValidateAgainstAdmissionAndIntent(admission, intent); err != nil {
		t.Fatalf("failed outcome admission closure error = %v", err)
	}
	if err := failed.ValidateOutcomeAgainstBinding(binding); err != nil {
		t.Fatalf("failed outcome binding closure error = %v", err)
	}
	canceledInput := validAgentStageTerminalOutcomeGate(
		admission,
		intent,
		binding,
		AgentStageCanceledResultAccepted,
	)
	canceled, err := SealAgentStageTerminalGate(canceledInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := canceled.ValidateOutcomeAgainstBinding(binding); err != nil {
		t.Fatalf("canceled outcome binding closure error = %v", err)
	}
	if failed.GateID != completion.GateID || canceled.GateID != completion.GateID ||
		failed.SHA256 == canceled.SHA256 ||
		!failed.EventTime().Equal(failed.Outcome.AcceptedAt) ||
		!canceled.EventTime().Equal(canceled.Outcome.AcceptedAt) {
		t.Fatalf("provider outcomes do not share one exact terminal authority")
	}

	diagnosticInput := validAgentStageTerminalOutcomeGate(
		admission,
		intent,
		binding,
		AgentStageFailedResultAccepted,
	)
	taskEvidence := agentTerminalDiagnosticProjection(
		admission.Subject,
		ContractAgentReviewTaskEvidence,
		strings.Repeat("9", 64),
	)
	receipts := agentTerminalDiagnosticProjection(
		admission.Subject,
		ContractAgentExecutionReceipts,
		strings.Repeat("a", 64),
	)
	diagnosticInput.Outcome.AgentTaskEvidence = &taskEvidence
	diagnosticInput.Outcome.AgentExecutionReceipts = &receipts
	diagnostic, err := SealAgentStageTerminalGate(diagnosticInput)
	if err != nil {
		t.Fatalf("seal diagnostic provider outcome: %v", err)
	}
	if err := diagnostic.ValidateOutcomeAgainstBinding(binding); err != nil {
		t.Fatalf("diagnostic provider outcome binding closure error = %v", err)
	}
	unpaired := cloneAgentStageTerminalGate(diagnostic)
	unpaired.Outcome.AgentExecutionReceipts = nil
	if err := unpaired.Validate(); err == nil || !strings.Contains(err.Error(), "must be present together") {
		t.Fatalf("unpaired outcome diagnostics error = %v", err)
	}

	wrongStatus := failed
	wrongStatus.Outcome = cloneAgentStageTerminalGate(failed).Outcome
	wrongStatus.Outcome.Status = "canceled"
	if err := wrongStatus.Validate(); err == nil || !strings.Contains(err.Error(), "must be failed") {
		t.Fatalf("changed failed outcome status error = %v", err)
	}
	withOutputBranch := failed
	completionCopy := *completion.Completion
	withOutputBranch.Completion = &completionCopy
	if err := withOutputBranch.Validate(); err == nil || !strings.Contains(err.Error(), "forbids") {
		t.Fatalf("mixed provider outcome union error = %v", err)
	}
}

func agentTerminalDiagnosticProjection(
	subject AgentPlanningSubject,
	contract string,
	digest string,
) AgentArtifactProjection {
	return AgentArtifactProjection{
		Local: ArtifactRef{
			URI:    "artifact://local/sha256/" + digest,
			SHA256: digest, SizeBytes: 321, Contract: contract,
		},
		Governed: GovernedArtifactBinding{
			URI: "artifact://argus-local/tenants/" + subject.TenantID +
				"/workspaces/" + subject.WorkspaceID + "/objects/" + digest,
			SHA256: digest, SizeBytes: 321, Contract: contract,
		},
	}
}

func TestAgentStageTerminalGateRejectsUnionAndFenceTampering(t *testing.T) {
	t.Parallel()

	completion, admission, intent, binding := validSealedAgentStageTerminalCompletion(t)
	tests := []struct {
		name   string
		mutate func(*AgentStageTerminalGate)
		want   string
	}{
		{"both branches", func(value *AgentStageTerminalGate) {
			value.Cancellation = &AgentStageCancellationRequest{
				CancelIdempotencyKey: intent.CancelIdempotencyKey,
				Actor:                "user-1", Reason: "cancel", RequestedAt: intent.RecordedAt.Add(time.Minute),
			}
		}, "forbids cancellation"},
		{"missing completion", func(value *AgentStageTerminalGate) {
			value.Completion = nil
		}, "requires completion"},
		{"trace manifest contract", func(value *AgentStageTerminalGate) {
			value.Completion.TraceManifest.Local.Contract = ContractReviewHypothesisSet
			value.Completion.TraceManifest.Governed.Contract = ContractReviewHypothesisSet
		}, ContractStageExecutionTraceManifest},
		{"unsafe fence", func(value *AgentStageTerminalGate) {
			value.FencingToken = 1 << 53
		}, "JSON-safe"},
		{"late provider result", func(value *AgentStageTerminalGate) {
			value.Completion.ProviderResultRecordedAt = value.RequestDeadline.Add(time.Nanosecond)
		}, "after request_deadline"},
		{"late host acceptance", func(value *AgentStageTerminalGate) {
			value.Completion.AcceptedAt = value.RequestDeadline.Add(time.Nanosecond)
		}, "host accepted result after request_deadline"},
		{"host acceptance predates provider", func(value *AgentStageTerminalGate) {
			value.Completion.AcceptedAt = value.Completion.ProviderResultRecordedAt.Add(-time.Nanosecond)
		}, "must not precede"},
		{"wrong request deadline zone", func(value *AgentStageTerminalGate) {
			value.RequestDeadline = value.RequestDeadline.In(time.FixedZone("offset", 8*60*60))
		}, "UTC"},
		{"self digest", func(value *AgentStageTerminalGate) {
			value.SHA256 = strings.Repeat("f", 64)
		}, "does not match"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := completion
			if completion.Completion != nil {
				copy := *completion.Completion
				if copy.TraceManifest != nil {
					trace := *copy.TraceManifest
					copy.TraceManifest = &trace
				}
				copy.CompletenessNotes = append(
					make([]string, 0, len(completion.Completion.CompletenessNotes)),
					completion.Completion.CompletenessNotes...,
				)
				value.Completion = &copy
			}
			test.mutate(&value)
			if err := value.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}

	stale := validAgentStageTerminalGate(admission, intent, binding)
	stale.Generation++
	sealed, err := SealAgentStageTerminalGate(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := sealed.ValidateAgainstAdmissionAndIntent(admission, intent); err == nil ||
		!strings.Contains(err.Error(), "exact admission") {
		t.Fatalf("stale generation closure error = %v", err)
	}

	lateBindingInput := validAgentStageExecutionBinding(intent)
	lateBindingInput.RecordedAt = intent.RecordedAt.Add(4 * time.Minute)
	lateBinding, err := SealAgentStageExecutionBinding(lateBindingInput)
	if err != nil {
		t.Fatal(err)
	}
	causallyInvalid, err := SealAgentStageTerminalGate(
		validAgentStageTerminalGate(admission, intent, lateBinding),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := causallyInvalid.ValidateCompletionAgainstBinding(lateBinding); err == nil ||
		!strings.Contains(err.Error(), "predates execution binding") {
		t.Fatalf("causally inverted binding error = %v", err)
	}
}

func TestAgentStageResultCallbackReceiptSealsStrictlyAndClosesExactResult(t *testing.T) {
	t.Parallel()

	admission := validAgentDispatchAdmission(t)
	intent, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := SealAgentStageExecutionBinding(validAgentStageExecutionBinding(intent))
	if err != nil {
		t.Fatal(err)
	}
	resultRef := validAgentStageTerminalGate(admission, intent, binding).Completion.ResultRef
	providerHandleSHA256, err := AgentStageProviderHandleSHA256(binding.ProviderHandle)
	if err != nil {
		t.Fatal(err)
	}
	proof := []byte("one-time-provider-callback-proof")
	proofSHA256, err := AgentStageResultCallbackProofSHA256(proof)
	if err != nil {
		t.Fatal(err)
	}
	input := AgentStageResultCallbackReceipt{
		SchemaVersion:        AgentStageResultCallbackReceiptSchemaVersion,
		Kind:                 AgentStageProviderCallbackVerified,
		Subject:              binding.Subject,
		ReviewRunID:          binding.ReviewRunID,
		Stage:                binding.Stage,
		BindingID:            binding.BindingID,
		BindingSHA256:        binding.SHA256,
		ProviderHandleSHA256: providerHandleSHA256,
		ResultRef:            resultRef,
		ProofSHA256:          proofSHA256,
		Verifier: AgentStageResultCallbackVerifierRef{
			ID:       "provider-callback-verifier",
			Revision: "v1",
			SHA256:   agentTerminalVerifierSHA,
		},
		VerifiedAt: binding.RecordedAt.Add(time.Minute),
	}
	receipt, err := SealAgentStageResultCallbackReceipt(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.ValidateAgainstBindingAndResult(binding, resultRef); err != nil {
		t.Fatalf("ValidateAgainstBindingAndResult() error = %v", err)
	}
	wantID, err := AgentStageResultCallbackReceiptID(binding.ReviewRunID, binding.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ReceiptID != wantID {
		t.Fatalf("receipt ID = %q, want %q", receipt.ReceiptID, wantID)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAgentStageResultCallbackReceipt(encoded)
	if err != nil {
		t.Fatalf("DecodeAgentStageResultCallbackReceipt() error = %v", err)
	}
	if decoded != receipt {
		t.Fatalf("decoded callback receipt differs: got %+v want %+v", decoded, receipt)
	}
	if bytes.Contains(encoded, proof) {
		t.Fatal("callback receipt persisted raw proof bytes")
	}

	tampered := receipt
	tampered.ProofSHA256 = strings.Repeat("e", 64)
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered receipt Validate() error = %v", err)
	}

	wrongResult := resultRef
	wrongResult.SHA256 = strings.Repeat("d", 64)
	wrongResult.URI = "artifact://local/sha256/" + wrongResult.SHA256
	if err := receipt.ValidateAgainstBindingAndResult(binding, wrongResult); err == nil ||
		!strings.Contains(err.Error(), "exact execution binding and canonical result") {
		t.Fatalf("wrong result closure error = %v", err)
	}

	otherBindingInput := validAgentStageExecutionBinding(intent)
	otherBindingInput.ProviderHandle = "provider/executions/opaque-2"
	otherBinding, err := SealAgentStageExecutionBinding(otherBindingInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.ValidateAgainstBindingAndResult(otherBinding, resultRef); err == nil ||
		!strings.Contains(err.Error(), "exact execution binding and canonical result") {
		t.Fatalf("wrong binding closure error = %v", err)
	}

	tooEarly := input
	tooEarly.VerifiedAt = binding.RecordedAt.Add(-time.Nanosecond)
	tooEarlyReceipt, err := SealAgentStageResultCallbackReceipt(tooEarly)
	if err != nil {
		t.Fatal(err)
	}
	if err := tooEarlyReceipt.ValidateAgainstBindingAndResult(binding, resultRef); err == nil ||
		!strings.Contains(err.Error(), "predates execution binding") {
		t.Fatalf("early receipt closure error = %v", err)
	}

	for _, mutate := range []func(*AgentStageResultCallbackReceipt){
		func(value *AgentStageResultCallbackReceipt) { value.Verifier.Revision = "latest" },
		func(value *AgentStageResultCallbackReceipt) { value.Verifier.SHA256 = "not-a-digest" },
	} {
		invalid := input
		mutate(&invalid)
		if _, err := SealAgentStageResultCallbackReceipt(invalid); err == nil {
			t.Fatal("SealAgentStageResultCallbackReceipt() accepted inexact verifier")
		}
	}
}

func TestAgentStageResultCallbackReceiptStrictDecoderRejectsStructuralDrift(t *testing.T) {
	t.Parallel()

	receipt, _, _, binding := validSealedAgentStageTerminalCompletion(t)
	providerHandleSHA256, err := AgentStageProviderHandleSHA256(binding.ProviderHandle)
	if err != nil {
		t.Fatal(err)
	}
	proofSHA256, err := AgentStageResultCallbackProofSHA256([]byte("proof"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealAgentStageResultCallbackReceipt(AgentStageResultCallbackReceipt{
		Subject:              binding.Subject,
		ReviewRunID:          binding.ReviewRunID,
		Stage:                binding.Stage,
		BindingID:            binding.BindingID,
		BindingSHA256:        binding.SHA256,
		ProviderHandleSHA256: providerHandleSHA256,
		ResultRef:            receipt.Completion.ResultRef,
		ProofSHA256:          proofSHA256,
		Verifier: AgentStageResultCallbackVerifierRef{
			ID: "provider-callback-verifier", Revision: "v1", SHA256: agentTerminalVerifierSHA,
		},
		VerifiedAt: binding.RecordedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := bytes.Replace(
		encoded,
		[]byte(`"receipt_id":`),
		[]byte(`"receipt_id":"duplicate","receipt_id":`),
		1,
	)
	unknown := bytes.Replace(encoded, []byte(`{"schema_version":`), []byte(`{"unknown":true,"schema_version":`), 1)
	for _, data := range [][]byte{
		duplicate,
		unknown,
		[]byte("null"),
		append(append([]byte(nil), encoded...), []byte(` {}`)...),
	} {
		if _, err := DecodeAgentStageResultCallbackReceipt(data); err == nil {
			t.Fatalf("strict decoder accepted %s", data)
		}
	}
}

func validSealedAgentStageTerminalCompletion(
	t *testing.T,
) (
	AgentStageTerminalGate,
	AgentStagePlanAdmission,
	AgentStageDispatchIntent,
	AgentStageExecutionBinding,
) {
	t.Helper()
	admission := validAgentDispatchAdmission(t)
	intent, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := SealAgentStageExecutionBinding(validAgentStageExecutionBinding(intent))
	if err != nil {
		t.Fatal(err)
	}
	gate, err := SealAgentStageTerminalGate(
		validAgentStageTerminalGate(admission, intent, binding),
	)
	if err != nil {
		t.Fatal(err)
	}
	return gate, admission, intent, binding
}

func validAgentStageTerminalGate(
	admission AgentStagePlanAdmission,
	intent AgentStageDispatchIntent,
	binding AgentStageExecutionBinding,
) AgentStageTerminalGate {
	output := ArtifactRef{
		URI:       "artifact://local/sha256/" + agentTerminalOutputSHA,
		SHA256:    agentTerminalOutputSHA,
		SizeBytes: 256,
		Contract:  ContractReviewHypothesisSet,
	}
	traceLocal := ArtifactRef{
		URI:       "artifact://local/sha256/" + agentTerminalTraceSHA,
		SHA256:    agentTerminalTraceSHA,
		SizeBytes: 128,
		Contract:  ContractStageExecutionTraceManifest,
	}
	trace := AgentArtifactProjection{
		Local: traceLocal,
		Governed: GovernedArtifactBinding{
			URI: "artifact://argus-local/tenants/" + admission.Subject.TenantID +
				"/workspaces/" + admission.Subject.WorkspaceID +
				"/objects/" + traceLocal.SHA256,
			SHA256: traceLocal.SHA256, SizeBytes: traceLocal.SizeBytes, Contract: traceLocal.Contract,
		},
	}
	return AgentStageTerminalGate{
		SchemaVersion:         AgentStageTerminalGateSchemaVersion,
		Kind:                  AgentStageSucceededResultAccepted,
		Subject:               admission.Subject,
		ReviewRunID:           admission.ReviewRunID,
		Stage:                 admission.Stage,
		AdmissionID:           admission.AdmissionID,
		AdmissionSHA256:       admission.SHA256,
		IntentID:              intent.IntentID,
		IntentSHA256:          intent.SHA256,
		WorkloadID:            intent.WorkloadID,
		LeaseID:               intent.LeaseID,
		LeaseWorker:           intent.LeaseWorker,
		ExecutionID:           intent.ExecutionID,
		Attempt:               intent.Attempt,
		Generation:            intent.Generation,
		FencingToken:          intent.FencingToken,
		RequestRef:            intent.RequestRef,
		RequestSemanticSHA256: intent.RequestSemanticSHA256,
		RequestDeadline:       intent.RecordedAt.Add(10 * time.Minute),
		CapabilitySHA256:      intent.CapabilitySHA256,
		Completion: &AgentStageSucceededResultAcceptance{
			BindingID:     binding.BindingID,
			BindingSHA256: binding.SHA256,
			ResultRef: ArtifactRef{
				URI:       "artifact://local/sha256/" + agentTerminalResultSHA,
				SHA256:    agentTerminalResultSHA,
				SizeBytes: 512,
				Contract:  ContractStageExecutionResult,
			},
			Output: AgentArtifactProjection{
				Local: output,
				Governed: GovernedArtifactBinding{
					URI: "artifact://argus-local/tenants/" + admission.Subject.TenantID +
						"/workspaces/" + admission.Subject.WorkspaceID +
						"/objects/" + output.SHA256,
					SHA256:    output.SHA256,
					SizeBytes: output.SizeBytes,
					Contract:  output.Contract,
				},
			},
			TraceManifest: &trace,
			CallbackReceiptRef: ArtifactRef{
				URI:       "artifact://local/sha256/" + agentTerminalReceiptArtifactSHA,
				SHA256:    agentTerminalReceiptArtifactSHA,
				SizeBytes: 768,
				Contract:  ContractAgentStageResultCallbackReceipt,
			},
			CallbackReceiptSHA256:    agentTerminalReceiptSemanticSHA,
			Status:                   "succeeded",
			Completeness:             "complete",
			CompletenessNotes:        []string{},
			ProviderResultRecordedAt: intent.RecordedAt.Add(2 * time.Minute),
			AcceptedAt:               intent.RecordedAt.Add(3 * time.Minute),
		},
	}
}

func validAgentStageTerminalOutcomeGate(
	admission AgentStagePlanAdmission,
	intent AgentStageDispatchIntent,
	binding AgentStageExecutionBinding,
	kind AgentStageTerminalGateKind,
) AgentStageTerminalGate {
	completion := validAgentStageTerminalGate(admission, intent, binding)
	status := "failed"
	failureCode := "provider_failed"
	if kind == AgentStageCanceledResultAccepted {
		status = "canceled"
		failureCode = "execution_canceled"
	}
	return AgentStageTerminalGate{
		SchemaVersion:         AgentStageTerminalGateSchemaVersion,
		Kind:                  kind,
		Subject:               admission.Subject,
		ReviewRunID:           admission.ReviewRunID,
		Stage:                 admission.Stage,
		AdmissionID:           admission.AdmissionID,
		AdmissionSHA256:       admission.SHA256,
		IntentID:              intent.IntentID,
		IntentSHA256:          intent.SHA256,
		WorkloadID:            intent.WorkloadID,
		LeaseID:               intent.LeaseID,
		LeaseWorker:           intent.LeaseWorker,
		ExecutionID:           intent.ExecutionID,
		Attempt:               intent.Attempt,
		Generation:            intent.Generation,
		FencingToken:          intent.FencingToken,
		RequestRef:            intent.RequestRef,
		RequestSemanticSHA256: intent.RequestSemanticSHA256,
		RequestDeadline:       completion.RequestDeadline,
		CapabilitySHA256:      intent.CapabilitySHA256,
		Outcome: &AgentStageNonSucceededResultAcceptance{
			BindingID:             binding.BindingID,
			BindingSHA256:         binding.SHA256,
			ResultRef:             completion.Completion.ResultRef,
			TraceManifest:         completion.Completion.TraceManifest,
			CallbackReceiptRef:    completion.Completion.CallbackReceiptRef,
			CallbackReceiptSHA256: completion.Completion.CallbackReceiptSHA256,
			Status:                status,
			Failure: AgentStageExecutionFailure{
				Code: failureCode, Message: "provider returned a typed terminal failure",
				Retryable: kind == AgentStageFailedResultAccepted,
			},
			Completeness: "partial", CompletenessNotes: []string{"provider_terminal_failure"},
			ProviderResultRecordedAt: intent.RecordedAt.Add(2 * time.Minute),
			AcceptedAt:               intent.RecordedAt.Add(3 * time.Minute),
		},
	}
}
