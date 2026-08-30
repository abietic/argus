package application

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const agentResultCallbackProof = "trusted-provider-callback-proof"

func TestFormalHypothesisEvidenceMayReferenceFrozenFileOutsideSelection(t *testing.T) {
	content := "package fixture\r\n  func Divide(a, b int) int { return a / b }\r\n"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	input := reviewcore.ReviewInput{
		SchemaVersion: reviewcore.ReviewInputSchemaVersion,
		TargetID:      "selection-target", TargetMode: reviewcore.TargetModeSelection,
		CanonicalPatch: "",
		Regions: []reviewcore.ReviewRegion{{
			Path: "review.go", StartLine: 2, EndLine: 2, SHA256: digest,
		}},
		Files: []reviewcore.FileManifestEntry{{
			Path: "review.go", SHA256: digest,
			SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []reviewcore.ContextBinding{},
	}
	if err := input.Validate(); err != nil {
		t.Fatal(err)
	}
	outOfSelection := contractsv1alpha1.HypothesisSourceAnchor{
		Path: "review.go", Side: contractsv1alpha1.HypothesisAnchorFile,
		StartLine: 1, EndLine: 1, SourceDigest: digest,
	}
	if err := validateFormalHypothesisAnchor(
		t.Context(), input, outOfSelection, "evidence anchor", "package fixture", false,
	); err != nil {
		t.Fatalf("frozen supporting evidence outside selected lines was rejected: %v", err)
	}
	selected := outOfSelection
	selected.StartLine, selected.EndLine = 2, 2
	if err := validateFormalHypothesisAnchor(
		t.Context(), input, selected, "evidence anchor",
		"func Divide(a, b int) int { return a / b }", false,
	); err != nil {
		t.Fatalf("outer-trimmed CRLF frozen evidence was rejected: %v", err)
	}
	if err := validateFormalHypothesisAnchor(
		t.Context(), input, outOfSelection, "hypothesis anchor", "", true,
	); err == nil || !strings.Contains(err.Error(), "outside the frozen review target") {
		t.Fatalf("primary hypothesis anchor escaped selected lines: %v", err)
	}
	escaped := outOfSelection
	escaped.Path = "outside.go"
	if err := validateFormalHypothesisAnchor(
		t.Context(), input, escaped, "evidence anchor", "package fixture", false,
	); err == nil || !strings.Contains(err.Error(), "absent from the frozen file manifest") {
		t.Fatalf("supporting evidence escaped the frozen file manifest: %v", err)
	}
}

func TestAgentStageResultAdmitterCommitsFailedAndCanceledTerminalOutcomes(t *testing.T) {
	for _, status := range []contractsv1alpha1.StageExecutionStatus{
		contractsv1alpha1.StageExecutionFailed,
		contractsv1alpha1.StageExecutionCanceled,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			dispatched, fixture, repository := dispatchedAgentResultFixture(t)
			result, resultJSON := formalTerminalResultFixture(t, dispatched, status)
			verifier := &agentResultCallbackVerifierStub{}
			admitter, err := NewAgentStageResultAdmitter(
				repository,
				verifier,
				func() time.Time { return result.RecordedAt.Add(time.Second) },
			)
			if err != nil {
				t.Fatal(err)
			}
			first, err := admitter.AdmitGovernedAgentStageTerminalOutcome(
				context.Background(),
				fixture.subject,
				AdmitAgentStageResultCommand{
					ReviewRunID:   dispatched.Plan.ReviewRunID,
					StageID:       dispatched.Plan.Stage.ID,
					ResultJSON:    resultJSON,
					CallbackProof: []byte(agentResultCallbackProof),
				},
			)
			if err != nil {
				t.Fatalf("AdmitGovernedAgentStageTerminalOutcome() error = %v", err)
			}
			wantKind := runmodel.AgentStageFailedResultAccepted
			if status == contractsv1alpha1.StageExecutionCanceled {
				wantKind = runmodel.AgentStageCanceledResultAccepted
			}
			if first.Recovered || first.Gate.Kind != wantKind || first.Gate.Outcome == nil ||
				first.Gate.Completion != nil || repository.evidence != nil ||
				first.Gate.Outcome.ResultRef.Contract != runmodel.ContractStageExecutionResult {
				t.Fatalf("terminal outcome was not closed without finding evidence: %+v", first)
			}

			indented, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			retry, err := admitter.AdmitGovernedAgentStageTerminalOutcome(
				context.Background(),
				fixture.subject,
				AdmitAgentStageResultCommand{
					ReviewRunID: dispatched.Plan.ReviewRunID,
					StageID:     dispatched.Plan.Stage.ID,
					ResultJSON:  indented,
				},
			)
			if err != nil {
				t.Fatalf("terminal outcome exact retry error = %v", err)
			}
			if !retry.Recovered || retry.Gate.SHA256 != first.Gate.SHA256 || verifier.calls != 1 {
				t.Fatalf("terminal outcome retry did not recover winner: %+v", retry)
			}
		})
	}
}

func TestAgentStageResultAdmitterPersistsFailedDiagnosticEvidenceWithoutHypotheses(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, _ := formalTerminalResultFixture(
		t,
		dispatched,
		contractsv1alpha1.StageExecutionFailed,
	)
	taskBinding, receiptBinding := attachFormalAgentDiagnosticEvidence(
		t,
		fixture,
		dispatched,
	)
	result.AgentTaskEvidence = &taskBinding
	result.AgentExecutionReceipts = &receiptBinding
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := NewAgentStageResultAdmitter(
		repository,
		&agentResultCallbackVerifierStub{},
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := admitter.AdmitGovernedAgentStageTerminalOutcome(
		t.Context(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err != nil {
		t.Fatalf("AdmitGovernedAgentStageTerminalOutcome() diagnostic error = %v", err)
	}
	if first.Gate.Outcome == nil || first.Gate.Completion != nil || repository.evidence != nil ||
		first.DiagnosticTaskEvidence == nil || first.DiagnosticReceipts == nil ||
		first.Gate.Outcome.AgentTaskEvidence == nil ||
		first.Gate.Outcome.AgentExecutionReceipts == nil {
		t.Fatalf("failed diagnostics escaped terminal-only projection: %+v", first)
	}
	for _, projection := range []*runmodel.AgentArtifactProjection{
		first.DiagnosticTaskEvidence,
		first.DiagnosticReceipts,
	} {
		if _, found := repository.local[projection.Local.URI]; !found {
			t.Fatalf("diagnostic artifact %q was not localized", projection.Local.URI)
		}
	}

	retry, err := admitter.AdmitGovernedAgentStageTerminalOutcome(
		t.Context(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  resultJSON,
		},
	)
	if err != nil || !retry.Recovered || retry.Gate.SHA256 != first.Gate.SHA256 {
		t.Fatalf("failed diagnostic exact retry = %+v, %v", retry, err)
	}

	delete(repository.local, first.DiagnosticTaskEvidence.Local.URI)
	if _, err := admitter.AdmitGovernedAgentStageTerminalOutcome(
		t.Context(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  resultJSON,
		},
	); err == nil || !strings.Contains(err.Error(), "terminal-local formal agent task evidence") {
		t.Fatalf("missing failed diagnostic evidence error = %v", err)
	}
}

func TestAgentStageResultAdmitterRejectsSuccessAfterFailedTerminalWithoutPanic(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	failed, failedJSON := formalTerminalResultFixture(
		t,
		dispatched,
		contractsv1alpha1.StageExecutionFailed,
	)
	admitter, err := NewAgentStageResultAdmitter(
		repository,
		&agentResultCallbackVerifierStub{},
		func() time.Time { return failed.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitter.AdmitGovernedAgentStageTerminalOutcome(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    failedJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	); err != nil {
		t.Fatal(err)
	}
	succeeded, succeededJSON := formalSucceededResultFixture(t, fixture, dispatched)
	admitter.now = func() time.Time { return succeeded.RecordedAt.Add(time.Second) }
	if _, err := admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    succeededJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	); err == nil || !strings.Contains(err.Error(), "conflicts with terminal winner") {
		t.Fatalf("success after failed terminal error = %v", err)
	}
}

func TestAgentStageResultAdmitterCommitsTerminalWinnerAndEvidenceWithExactRetry(
	t *testing.T,
) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	verifier := &agentResultCallbackVerifierStub{}
	admitter, err := NewAgentStageResultAdmitter(
		repository,
		verifier,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}

	first, err := admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err != nil {
		t.Fatalf("AdmitGovernedAgentStageResult() error = %v", err)
	}
	if first.Recovered || repository.terminal == nil || repository.evidence == nil ||
		first.Gate.Kind != runmodel.AgentStageSucceededResultAccepted ||
		first.Evidence.ResultRef != first.Gate.Completion.ResultRef ||
		first.Evidence.Output != first.Gate.Completion.Output ||
		result.AgentRawCandidates == nil {
		t.Fatalf("formal result was not atomically closed into terminal evidence: %+v", first)
	}

	indented, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  indented,
		},
	)
	if err != nil {
		t.Fatalf("AdmitGovernedAgentStageResult(retry) error = %v", err)
	}
	if !retry.Recovered || retry.Gate.SHA256 != first.Gate.SHA256 ||
		retry.Evidence != first.Evidence ||
		retry.Hypotheses.HypothesisSetID != first.Hypotheses.HypothesisSetID ||
		verifier.calls != 1 {
		t.Fatalf("exact semantic retry did not recover immutable evidence: %+v", retry)
	}
}

func TestAgentStageResultAdmitterRejectsRawCandidateThatDoesNotBindNormalization(
	t *testing.T,
) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, _ := formalSucceededResultFixture(t, fixture, dispatched)
	claim := contractsv1alpha1.AgentReviewRawCandidateClaim{
		Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
		Title:       "candidate without normalization decision",
		Description: "the raw claim is valid but is absent from the normalized set",
		Impact:      "the host must reject an unclosed raw candidate",
		Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
			Path: "fixture.go", Side: contractsv1alpha1.HypothesisAnchorFile,
			StartLine: 1, EndLine: 1,
		},
		Evidence: []contractsv1alpha1.AgentReviewRawCandidateEvidence{{
			Statement: "the fixture claim has source-local evidence",
			Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
				Path: "fixture.go", Side: contractsv1alpha1.HypothesisAnchorFile,
				StartLine: 1, EndLine: 1,
			},
			Excerpt: "package fixture",
		}},
	}
	claimDigest, err := contractsv1alpha1.DigestAgentReviewRawCandidateClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	var dimension contractsv1alpha1.VersionedRef
	for _, skill := range dispatched.Plan.Skills {
		if skill.Phase == contractsv1alpha1.AgentStageSkillPhaseReview {
			dimension = skill.Ref
			break
		}
	}
	raw := contractsv1alpha1.AgentReviewRawCandidateCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        dispatched.Plan.PlanID, SourceRunID: dispatched.Plan.ReviewRunID,
		ExecutionID: dispatched.Request.ExecutionID, ReviewRunID: dispatched.Plan.ReviewRunID,
		TargetDigest: dispatched.Plan.TargetDigest,
		Authority:    contractsv1alpha1.AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition:  contractsv1alpha1.AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: []contractsv1alpha1.AgentReviewRawCandidatePayload{{
			RawCandidateID: "raw-unclosed-1", GroupID: "group-unclosed-1",
			Dimension: dimension, ClaimDigest: claimDigest,
			Action:     contractsv1alpha1.HypothesisNormalizationRetained,
			ReasonCode: "normalized_candidate_retained", Claim: claim,
		}},
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawJSON); err != nil {
		t.Fatalf("raw fixture must be independently valid: %v", err)
	}
	binding := fixture.repository.addGoverned(
		fixture.subject, contractsv1alpha1.StageExecutionAgentRawCandidateContract, rawJSON,
	)
	result.AgentRawCandidates = &binding
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeStageExecutionResult(resultJSON); err != nil {
		t.Fatalf("stage result fixture must be independently valid: %v", err)
	}
	admitter, err := newTestAgentStageResultAdmitter(
		t, repository, func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitter.AdmitGovernedAgentStageResult(
		context.Background(), fixture.subject, AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID, StageID: dispatched.Plan.Stage.ID,
			ResultJSON: resultJSON, CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "does not close every normalization decision") ||
		repository.terminal != nil || repository.evidence != nil {
		t.Fatalf("unclosed raw candidate reached terminal evidence: error=%v", err)
	}
}

func TestAgentStageResultAdmitterRejectsMissingOrUntrustedFreshCallbackBeforeOutputRead(
	t *testing.T,
) {
	for _, test := range []struct {
		name  string
		proof []byte
		want  string
	}{
		{name: "missing", want: "fresh provider callback proof"},
		{name: "untrusted", proof: []byte("fabricated-proof"), want: "authenticate formal provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatched, fixture, repository := dispatchedAgentResultFixture(t)
			result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
			delete(repository.governed, result.Output.Ref.URI)
			admitter, err := newTestAgentStageResultAdmitter(
				t,
				repository,
				func() time.Time { return result.RecordedAt.Add(time.Second) },
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admitter.AdmitGovernedAgentStageResult(
				context.Background(),
				fixture.subject,
				AdmitAgentStageResultCommand{
					ReviewRunID:   dispatched.Plan.ReviewRunID,
					StageID:       dispatched.Plan.Stage.ID,
					ResultJSON:    resultJSON,
					CallbackProof: test.proof,
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) ||
				repository.terminal != nil || repository.evidence != nil {
				t.Fatalf("fresh callback authentication error = %v", err)
			}
		})
	}
}

func TestAgentStageResultAdmitterRecoversRegisteredReceiptWithoutReverifyingProof(
	t *testing.T,
) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	firstVerifier := &agentResultCallbackVerifierStub{}
	clockCalls := 0
	first, err := NewAgentStageResultAdmitter(
		repository,
		firstVerifier,
		func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return result.RecordedAt.Add(2 * time.Second)
			}
			// Force a host failure after receipt registration but before the
			// terminal gate can be sealed.
			return result.RecordedAt.Add(time.Second)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = first.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err == nil || repository.callbackReceipt == nil || repository.terminal != nil ||
		firstVerifier.calls != 1 {
		t.Fatalf(
			"failed to preserve receipt-before-terminal recovery window: error=%v receipt=%v terminal=%v calls=%d",
			err,
			repository.callbackReceipt != nil,
			repository.terminal != nil,
			firstVerifier.calls,
		)
	}

	lateVerifier := &agentResultCallbackVerifierStub{
		err: errors.New("registered callback decision must not be verified twice"),
	}
	late, err := NewAgentStageResultAdmitter(
		repository,
		lateVerifier,
		func() time.Time { return result.RecordedAt.Add(3 * time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := late.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  resultJSON,
		},
	)
	if err != nil || recovered.Recovered || repository.terminal == nil ||
		lateVerifier.calls != 0 {
		t.Fatalf(
			"registered receipt was not recovered without proof: result=%+v error=%v verifier_calls=%d",
			recovered,
			err,
			lateVerifier.calls,
		)
	}
}

func TestAgentStageResultAdmitterRejectsCallbackVerifierOutsideFrozenTrust(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	verifier := &agentResultCallbackVerifierStub{
		returnedVerifier: &runmodel.AgentStageResultCallbackVerifierRef{
			ID: "other-callback-verifier", Revision: "1", SHA256: strings.Repeat("b", 64),
		},
	}
	admitter, err := NewAgentStageResultAdmitter(
		repository,
		verifier,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID, StageID: dispatched.Plan.Stage.ID,
			ResultJSON: resultJSON, CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "differs from frozen request trust root") ||
		repository.callbackReceipt != nil {
		t.Fatalf("callback outside frozen trust was admitted: error=%v receipt=%v", err, repository.callbackReceipt)
	}
}

func TestRecoveredCallbackReceiptMustMatchFrozenRequestTrust(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	clockCalls := 0
	admitter, err := NewAgentStageResultAdmitter(
		repository,
		&agentResultCallbackVerifierStub{},
		func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return result.RecordedAt.Add(2 * time.Second)
			}
			return result.RecordedAt.Add(time.Second)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID, StageID: dispatched.Plan.Stage.ID,
			ResultJSON: resultJSON, CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if repository.callbackReceipt == nil || repository.binding == nil {
		t.Fatal("fixture did not persist callback receipt before terminal failure")
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	trust := dispatched.Request.Capability.Trust
	trust.CallbackVerifier.ID = "other-callback-verifier"
	err = admitter.validateRecoveredAgentStageCallbackReceipt(
		context.Background(),
		*repository.callbackReceipt,
		repository.callbackReceiptRef,
		*repository.binding,
		localRefForBytes(runmodel.ContractStageExecutionResult, canonical),
		result.RecordedAt,
		trust,
	)
	if err == nil || !strings.Contains(err.Error(), "differs from frozen request trust root") {
		t.Fatalf("recovered receipt crossed frozen trust root: %v", err)
	}
}

func TestAgentStageResultAdmitterRecoversReceiptAfterOutputUnavailableWithoutReverifyingProof(
	t *testing.T,
) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	storedOutput := repository.governed[result.Output.Ref.URI]
	delete(repository.governed, result.Output.Ref.URI)

	firstVerifier := &agentResultCallbackVerifierStub{}
	first, err := NewAgentStageResultAdmitter(
		repository,
		firstVerifier,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = first.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err == nil || repository.callbackReceipt == nil || repository.terminal != nil ||
		firstVerifier.calls != 1 {
		t.Fatalf(
			"output failure did not preserve authenticated receipt: error=%v receipt=%v terminal=%v calls=%d",
			err,
			repository.callbackReceipt != nil,
			repository.terminal != nil,
			firstVerifier.calls,
		)
	}

	repository.governed[result.Output.Ref.URI] = storedOutput
	lateVerifier := &agentResultCallbackVerifierStub{
		err: errors.New("registered callback decision must not be verified twice"),
	}
	late, err := NewAgentStageResultAdmitter(
		repository,
		lateVerifier,
		func() time.Time { return result.RecordedAt.Add(2 * time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := late.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  resultJSON,
		},
	)
	if err != nil || admitted.Recovered || repository.terminal == nil ||
		repository.evidence == nil || lateVerifier.calls != 0 {
		t.Fatalf(
			"receipt did not resume output processing without proof: result=%+v error=%v verifier_calls=%d",
			admitted,
			err,
			lateVerifier.calls,
		)
	}
}

func TestAgentStageResultAdmitterPreservesVerifiedReceiptAfterCallerCancellation(
	t *testing.T,
) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	repository.rejectCanceledContext = true
	ctx, cancel := context.WithCancel(context.Background())
	firstVerifier := &agentResultCallbackVerifierStub{}
	first, err := NewAgentStageResultAdmitter(
		repository,
		firstVerifier,
		func() time.Time {
			cancel()
			return result.RecordedAt.Add(time.Second)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = first.AdmitGovernedAgentStageResult(
		ctx,
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if !errors.Is(err, context.Canceled) || repository.callbackReceipt == nil ||
		repository.terminal != nil || firstVerifier.calls != 1 {
		t.Fatalf(
			"caller cancellation lost verified callback receipt: error=%v receipt=%v terminal=%v calls=%d",
			err,
			repository.callbackReceipt != nil,
			repository.terminal != nil,
			firstVerifier.calls,
		)
	}

	lateVerifier := &agentResultCallbackVerifierStub{
		err: errors.New("registered callback decision must not be verified twice"),
	}
	late, err := NewAgentStageResultAdmitter(
		repository,
		lateVerifier,
		func() time.Time { return result.RecordedAt.Add(2 * time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := late.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  resultJSON,
		},
	)
	if err != nil || admitted.Recovered || repository.terminal == nil ||
		repository.evidence == nil || lateVerifier.calls != 0 {
		t.Fatalf(
			"cancellation-preserved receipt did not resume without proof: result=%+v error=%v verifier_calls=%d",
			admitted,
			err,
			lateVerifier.calls,
		)
	}
}

func TestAgentStageResultAdmitterConcurrentExactCallbacksConvergeOnFirstReceipt(
	t *testing.T,
) {
	dispatched, fixture, rawRepository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	repository := &lockedAgentStageResultRepository{
		AgentStageResultRepository: rawRepository,
	}
	var ready sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	firstVerifier := &agentResultCallbackVerifierStub{
		beforeReturn: func() { ready.Done(); <-release },
	}
	secondVerifier := &agentResultCallbackVerifierStub{
		beforeReturn: func() { ready.Done(); <-release },
	}
	first, err := NewAgentStageResultAdmitter(
		repository,
		firstVerifier,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAgentStageResultAdmitter(
		repository,
		secondVerifier,
		func() time.Time { return result.RecordedAt.Add(2 * time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result AdmittedAgentStageResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, admitter := range []*AgentStageResultAdmitter{first, second} {
		admitter := admitter
		go func() {
			admitted, admitErr := admitter.AdmitGovernedAgentStageResult(
				context.Background(),
				fixture.subject,
				AdmitAgentStageResultCommand{
					ReviewRunID:   dispatched.Plan.ReviewRunID,
					StageID:       dispatched.Plan.Stage.ID,
					ResultJSON:    resultJSON,
					CallbackProof: []byte(agentResultCallbackProof),
				},
			)
			outcomes <- outcome{result: admitted, err: admitErr}
		}()
	}
	ready.Wait()
	close(release)
	firstOutcome := <-outcomes
	secondOutcome := <-outcomes
	if firstOutcome.err != nil || secondOutcome.err != nil {
		t.Fatalf(
			"concurrent exact callback errors = (%v, %v)",
			firstOutcome.err,
			secondOutcome.err,
		)
	}
	if firstVerifier.calls != 1 || secondVerifier.calls != 1 ||
		rawRepository.callbackReceipt == nil || rawRepository.terminal == nil ||
		rawRepository.evidence == nil ||
		firstOutcome.result.Gate.SHA256 != secondOutcome.result.Gate.SHA256 ||
		firstOutcome.result.Evidence != secondOutcome.result.Evidence {
		t.Fatalf(
			"concurrent exact callbacks did not converge: first=%+v second=%+v calls=%d/%d",
			firstOutcome.result,
			secondOutcome.result,
			firstVerifier.calls,
			secondVerifier.calls,
		)
	}
}

func TestAgentStageResultAdmitterRejectsCancellationWinnerBeforeOutputRead(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	cancelGate, err := runmodel.SealAgentStageTerminalGate(
		runmodel.AgentStageTerminalGate{
			SchemaVersion:         runmodel.AgentStageTerminalGateSchemaVersion,
			Kind:                  runmodel.AgentStageCancellationRequested,
			Subject:               dispatched.Intent.Subject,
			ReviewRunID:           dispatched.Intent.ReviewRunID,
			Stage:                 dispatched.Intent.Stage,
			AdmissionID:           dispatched.Intent.AdmissionID,
			AdmissionSHA256:       dispatched.Intent.AdmissionSHA256,
			IntentID:              dispatched.Intent.IntentID,
			IntentSHA256:          dispatched.Intent.SHA256,
			WorkloadID:            dispatched.Intent.WorkloadID,
			LeaseID:               dispatched.Intent.LeaseID,
			LeaseWorker:           dispatched.Intent.LeaseWorker,
			ExecutionID:           dispatched.Intent.ExecutionID,
			Attempt:               dispatched.Intent.Attempt,
			Generation:            dispatched.Intent.Generation,
			FencingToken:          dispatched.Intent.FencingToken,
			RequestRef:            dispatched.Intent.RequestRef,
			RequestSemanticSHA256: dispatched.Intent.RequestSemanticSHA256,
			RequestDeadline:       dispatched.Request.Deadline,
			CapabilitySHA256:      dispatched.Intent.CapabilitySHA256,
			Cancellation: &runmodel.AgentStageCancellationRequest{
				CancelIdempotencyKey: dispatched.Intent.CancelIdempotencyKey,
				Actor:                "test-operator",
				Reason:               "run canceled before accepting provider result",
				RequestedAt:          dispatched.Intent.RecordedAt.Add(time.Second),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	repository.terminal = &cancelGate
	// Prove the existing terminal winner is checked before dereferencing the
	// provider output by making the otherwise exact artifact unavailable.
	delete(repository.governed, result.Output.Ref.URI)

	admitter, err := newTestAgentStageResultAdmitter(t,
		repository,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  resultJSON,
		},
	)
	if !errors.Is(err, ErrAgentStageResultLostTerminalRace) {
		t.Fatalf("AdmitGovernedAgentStageResult() error = %v", err)
	}
	if repository.evidence != nil {
		t.Fatal("cancellation winner allowed formal hypothesis evidence")
	}
}

func TestAgentStageResultAdmitterResumesEvidenceFromAcceptedLocalTerminalAfterDeadline(
	t *testing.T,
) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, resultJSON := formalSucceededResultFixture(t, fixture, dispatched)
	repository.evidenceAppendErr = errors.New("crash after terminal gate before evidence append")
	firstAdmitter, err := newTestAgentStageResultAdmitter(t,
		repository,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = firstAdmitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if !errors.Is(err, ErrAgentStageEvidenceOutcomeUnknown) ||
		repository.terminal == nil || repository.evidence != nil {
		t.Fatalf(
			"failed to preserve gate-before-evidence crash window: error=%v terminal=%v evidence=%v",
			err,
			repository.terminal != nil,
			repository.evidence != nil,
		)
	}
	delete(repository.governed, result.Output.Ref.URI)
	lateAdmitter, err := newTestAgentStageResultAdmitter(t,
		repository,
		func() time.Time { return dispatched.Request.Deadline.Add(time.Hour) },
	)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := lateAdmitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID,
			StageID:     dispatched.Plan.Stage.ID,
			ResultJSON:  resultJSON,
		},
	)
	if err != nil {
		t.Fatalf("AdmitGovernedAgentStageResult(recover accepted terminal) error = %v", err)
	}
	if !recovered.Recovered || recovered.Gate.SHA256 != repository.terminal.SHA256 ||
		repository.evidence == nil ||
		recovered.Evidence.AdmittedAt != repository.terminal.Completion.AcceptedAt {
		t.Fatalf("accepted terminal did not resume evidence from local bytes: %+v", recovered)
	}
}

func newTestAgentStageResultAdmitter(
	t *testing.T,
	repository AgentStageResultRepository,
	now func() time.Time,
) (*AgentStageResultAdmitter, error) {
	t.Helper()
	return NewAgentStageResultAdmitter(
		repository,
		&agentResultCallbackVerifierStub{},
		now,
	)
}

type agentResultCallbackVerifierStub struct {
	calls            int
	err              error
	beforeReturn     func()
	returnedVerifier *runmodel.AgentStageResultCallbackVerifierRef
}

func (verifier *agentResultCallbackVerifierStub) VerifyAgentStageResultCallback(
	_ context.Context,
	_ AgentPlanningSubject,
	binding runmodel.AgentStageExecutionBinding,
	trust contractsv1alpha1.ExecutorTrust,
	canonicalResult []byte,
	proof []byte,
) (runmodel.AgentStageResultCallbackVerifierRef, error) {
	verifier.calls++
	if verifier.err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, verifier.err
	}
	if binding.ProviderHandle == "" || len(canonicalResult) == 0 ||
		trust.CallbackVerifier.ID == "" ||
		!slices.Equal(proof, []byte(agentResultCallbackProof)) {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"untrusted callback proof or incomplete exact callback binding",
		)
	}
	if verifier.beforeReturn != nil {
		verifier.beforeReturn()
	}
	if verifier.returnedVerifier != nil {
		return *verifier.returnedVerifier, nil
	}
	return runmodel.AgentStageResultCallbackVerifierRef{
		ID:       trust.CallbackVerifier.ID,
		Revision: trust.CallbackVerifier.Revision,
		SHA256:   trust.CallbackVerifier.SHA256,
	}, nil
}

type lockedAgentStageResultRepository struct {
	AgentStageResultRepository
	mu sync.Mutex
}

func (repository *lockedAgentStageResultRepository) ReadLocalArtifact(
	ctx context.Context,
	ref runmodel.ArtifactRef,
) ([]byte, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.ReadLocalArtifact(ctx, ref)
}

func (repository *lockedAgentStageResultRepository) PutLocalArtifact(
	ctx context.Context,
	contract string,
	data []byte,
) (runmodel.ArtifactRef, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.PutLocalArtifact(ctx, contract, data)
}

func (repository *lockedAgentStageResultRepository) LookupAgentStageResultCallbackReceipt(
	ctx context.Context,
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.LookupAgentStageResultCallbackReceipt(
		ctx, runID, bindingID, subject,
	)
}

func (repository *lockedAgentStageResultRepository) AppendAgentStageResultCallbackReceipt(
	ctx context.Context,
	receipt runmodel.AgentStageResultCallbackReceipt,
	ref runmodel.ArtifactRef,
) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.AppendAgentStageResultCallbackReceipt(
		ctx, receipt, ref,
	)
}

func (repository *lockedAgentStageResultRepository) LookupAgentStageTerminalGate(
	ctx context.Context,
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageTerminalGate, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.LookupAgentStageTerminalGate(
		ctx, runID, intentID, subject,
	)
}

func (repository *lockedAgentStageResultRepository) AppendAgentStageTerminalGate(
	ctx context.Context,
	gate runmodel.AgentStageTerminalGate,
) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.AppendAgentStageTerminalGate(ctx, gate)
}

func (repository *lockedAgentStageResultRepository) LookupAgentStageHypothesisEvidence(
	ctx context.Context,
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageHypothesisEvidence, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.LookupAgentStageHypothesisEvidence(
		ctx, runID, bindingID, subject,
	)
}

func (repository *lockedAgentStageResultRepository) AppendAgentStageHypothesisEvidence(
	ctx context.Context,
	evidence runmodel.AgentStageHypothesisEvidence,
) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.AgentStageResultRepository.AppendAgentStageHypothesisEvidence(ctx, evidence)
}

func TestAgentStageResultAdmitterFencesChangedGenerationBeforeEvidence(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, _ := formalSucceededResultFixture(t, fixture, dispatched)
	result.Generation++
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := newTestAgentStageResultAdmitter(t,
		repository,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err == nil || repository.terminal != nil || repository.evidence != nil {
		t.Fatalf("changed generation reached terminal evidence: error=%v", err)
	}
}

func TestAgentStageResultAdmitterLocalizesOptionalTraceManifest(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, _ := formalSucceededResultFixture(t, fixture, dispatched)
	traceBytes := []byte(`{"schema_version":"hailix.trace_manifest.v1alpha1","trace_id":"trace-1"}`)
	trace := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.StageExecutionTraceManifestContract,
		traceBytes,
	)
	result.TraceManifest = &trace
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := newTestAgentStageResultAdmitter(t,
		repository,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err != nil {
		t.Fatalf("AdmitGovernedAgentStageResult() error = %v", err)
	}
	if admitted.Gate.Completion == nil || admitted.Gate.Completion.TraceManifest == nil ||
		admitted.Gate.Completion.TraceManifest.Governed.URI != trace.Ref.URI ||
		admitted.Gate.Completion.TraceManifest.Local.SHA256 != trace.Ref.SHA256 {
		t.Fatalf("trace manifest was not closed into terminal evidence: %+v", admitted.Gate)
	}
}

func TestAgentStageResultAdmitterLocalizesFormalTaskAndReceiptEvidence(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, _ := formalSucceededResultFixture(t, fixture, dispatched)
	collection := contractsv1alpha1.AgentReviewTaskEvidenceCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		PlanID:        dispatched.Plan.PlanID, SourceRunID: dispatched.Plan.ReviewRunID,
		ExecutionID: dispatched.Request.ExecutionID, ReviewRunID: dispatched.Plan.ReviewRunID,
		TargetDigest:   dispatched.Plan.TargetDigest,
		Authority:      contractsv1alpha1.AgentReviewTaskEvidenceAuthority,
		Provenance:     contractsv1alpha1.AgentReviewTaskEvidenceProvenance,
		Disposition:    contractsv1alpha1.AgentReviewTaskEvidenceDisposition,
		ContentPolicy:  contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		Completeness:   contractsv1alpha1.AgentReviewTaskEvidencePartial,
		ReasonCodes:    []string{contractsv1alpha1.AgentReviewTaskEvidenceUnavailable},
		TaskExecutions: []contractsv1alpha1.AgentReviewTaskExecutionEvidence{},
	}
	data, err := json.Marshal(collection)
	if err != nil {
		t.Fatal(err)
	}
	binding := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		data,
	)
	result.AgentTaskEvidence = &binding
	receiptCollection := contractsv1alpha1.AgentExecutionReceiptCollection{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        dispatched.Plan.PlanID, SourceRunID: dispatched.Plan.ReviewRunID,
		ExecutionID: dispatched.Request.ExecutionID, ReviewRunID: dispatched.Plan.ReviewRunID,
		Receipts: []contractsv1alpha1.AgentExecutionReceipt{},
	}
	receiptData, err := json.Marshal(receiptCollection)
	if err != nil {
		t.Fatal(err)
	}
	receiptBinding := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		receiptData,
	)
	result.AgentExecutionReceipts = &receiptBinding
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := newTestAgentStageResultAdmitter(
		t,
		repository,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := admitter.AdmitGovernedAgentStageResult(
		t.Context(), fixture.subject, AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID, StageID: dispatched.Plan.Stage.ID,
			ResultJSON: resultJSON, CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	localURI := "artifact://local/sha256/" + binding.Ref.SHA256
	if first.Result.AgentTaskEvidence == nil || repository.local[localURI].local.Contract !=
		runmodel.ContractAgentReviewTaskEvidence {
		t.Fatalf("formal task evidence was not localized: %+v", first)
	}
	receiptLocalURI := "artifact://local/sha256/" + receiptBinding.Ref.SHA256
	if first.Result.AgentExecutionReceipts == nil ||
		repository.local[receiptLocalURI].local.Contract != runmodel.ContractAgentExecutionReceipts {
		t.Fatalf("formal execution receipts were not localized: %+v", first)
	}
	retry, err := admitter.AdmitGovernedAgentStageResult(
		t.Context(), fixture.subject, AdmitAgentStageResultCommand{
			ReviewRunID: dispatched.Plan.ReviewRunID, StageID: dispatched.Plan.Stage.ID,
			ResultJSON: resultJSON,
		},
	)
	if err != nil || !retry.Recovered {
		t.Fatalf("formal task evidence retry did not recover: %+v %v", retry, err)
	}
}

func TestAgentStageResultAdmitterRejectsUnresolvableTraceBeforeTerminal(t *testing.T) {
	dispatched, fixture, repository := dispatchedAgentResultFixture(t)
	result, _ := formalSucceededResultFixture(t, fixture, dispatched)
	trace := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.StageExecutionTraceManifestContract,
		[]byte(`{"trace_id":"trace-unavailable"}`),
	)
	delete(repository.governed, trace.Ref.URI)
	result.TraceManifest = &trace
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := newTestAgentStageResultAdmitter(t,
		repository,
		func() time.Time { return result.RecordedAt.Add(time.Second) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitter.AdmitGovernedAgentStageResult(
		context.Background(),
		fixture.subject,
		AdmitAgentStageResultCommand{
			ReviewRunID:   dispatched.Plan.ReviewRunID,
			StageID:       dispatched.Plan.Stage.ID,
			ResultJSON:    resultJSON,
			CallbackProof: []byte(agentResultCallbackProof),
		},
	)
	if err == nil || repository.terminal != nil || repository.evidence != nil {
		t.Fatalf("unresolvable trace reached terminal evidence: error=%v", err)
	}
}

func dispatchedAgentResultFixture(
	t *testing.T,
) (DispatchedAgentStage, agentPreparationFixture, *agentDispatchRepositoryStub) {
	t.Helper()
	dispatcher, fixture, repository, _, _, _ := newAgentDispatchFixture(t)
	dispatched, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil {
		t.Fatalf("DispatchGovernedAgentStage() error = %v", err)
	}
	return dispatched, fixture, repository
}

func formalSucceededResultFixture(
	t *testing.T,
	fixture agentPreparationFixture,
	dispatched DispatchedAgentStage,
) (contractsv1alpha1.StageExecutionResult, []byte) {
	t.Helper()
	generatedAt := dispatched.Intent.RecordedAt.Add(5 * time.Second)
	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:          contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID:        "formal-hypothesis-set-1",
		PlanID:                 dispatched.Plan.PlanID,
		SourceRunID:            dispatched.Plan.ReviewRunID,
		ExecutionID:            dispatched.Request.ExecutionID,
		ReviewRunID:            dispatched.Plan.ReviewRunID,
		TargetDigest:           dispatched.Plan.TargetDigest,
		Completeness:           contractsv1alpha1.AgentReviewComplete,
		CompletenessReasons:    []string{},
		NormalizationDecisions: []contractsv1alpha1.HypothesisNormalizationDecision{},
		Hypotheses:             []contractsv1alpha1.ReviewHypothesis{},
		DedupClusters:          []contractsv1alpha1.HypothesisDedupCluster{},
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			Gaps: []contractsv1alpha1.AgentReviewCoverageGap{},
		},
		GeneratedAt: generatedAt,
	}
	setJSON, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeReviewHypothesisSet(setJSON); err != nil {
		t.Fatalf("DecodeReviewHypothesisSet(fixture) error = %v", err)
	}
	output := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		setJSON,
	)
	raw := contractsv1alpha1.AgentReviewRawCandidateCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        dispatched.Plan.PlanID, SourceRunID: dispatched.Plan.ReviewRunID,
		ExecutionID: dispatched.Request.ExecutionID, ReviewRunID: dispatched.Plan.ReviewRunID,
		TargetDigest:  dispatched.Plan.TargetDigest,
		Authority:     contractsv1alpha1.AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition:   contractsv1alpha1.AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: []contractsv1alpha1.AgentReviewRawCandidatePayload{},
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawJSON); err != nil {
		t.Fatalf("DecodeAgentReviewRawCandidateCollection(fixture) error = %v", err)
	}
	rawBinding := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.StageExecutionAgentRawCandidateContract,
		rawJSON,
	)
	result := contractsv1alpha1.StageExecutionResult{
		SchemaVersion:      contractsv1alpha1.StageExecutionResultSchemaVersion,
		RequestSHA256:      dispatched.Request.RequestSHA256,
		ExecutionID:        dispatched.Request.ExecutionID,
		Attempt:            dispatched.Request.Attempt,
		Generation:         dispatched.Request.Generation,
		FencingToken:       dispatched.Request.FencingToken,
		IdempotencyKey:     dispatched.Request.IdempotencyKey,
		Status:             contractsv1alpha1.StageExecutionSucceeded,
		Output:             &output,
		AgentRawCandidates: &rawBinding,
		Completeness:       "complete",
		CompletenessNotes:  []string{},
		CapabilitySHA256:   dispatched.Request.Capability.SHA256,
		RecordedAt:         generatedAt.Add(time.Second),
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeStageExecutionResult(resultJSON); err != nil {
		t.Fatalf("DecodeStageExecutionResult(fixture) error = %v", err)
	}
	return result, resultJSON
}

func formalTerminalResultFixture(
	t *testing.T,
	dispatched DispatchedAgentStage,
	status contractsv1alpha1.StageExecutionStatus,
) (contractsv1alpha1.StageExecutionResult, []byte) {
	t.Helper()
	result := contractsv1alpha1.StageExecutionResult{
		SchemaVersion:  contractsv1alpha1.StageExecutionResultSchemaVersion,
		RequestSHA256:  dispatched.Request.RequestSHA256,
		ExecutionID:    dispatched.Request.ExecutionID,
		Attempt:        dispatched.Request.Attempt,
		Generation:     dispatched.Request.Generation,
		FencingToken:   dispatched.Request.FencingToken,
		IdempotencyKey: dispatched.Request.IdempotencyKey,
		Status:         status,
		Failure: &contractsv1alpha1.StageExecutionFailure{
			Code: "model_execution_failed", Message: "provider model execution failed",
			Retryable: status == contractsv1alpha1.StageExecutionFailed,
		},
		Completeness:      "partial",
		CompletenessNotes: []string{"provider_execution_failed"},
		CapabilitySHA256:  dispatched.Request.Capability.SHA256,
		RecordedAt:        dispatched.Intent.RecordedAt.Add(6 * time.Second),
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contractsv1alpha1.DecodeStageExecutionResult(data); err != nil {
		t.Fatalf("DecodeStageExecutionResult(terminal fixture) error = %v", err)
	}
	return result, data
}

func attachFormalAgentDiagnosticEvidence(
	t *testing.T,
	fixture agentPreparationFixture,
	dispatched DispatchedAgentStage,
) (contractsv1alpha1.ArtifactBinding, contractsv1alpha1.ArtifactBinding) {
	t.Helper()
	taskEvidence := contractsv1alpha1.AgentReviewTaskEvidenceCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		PlanID:        dispatched.Plan.PlanID, SourceRunID: dispatched.Plan.ReviewRunID,
		ExecutionID: dispatched.Request.ExecutionID, ReviewRunID: dispatched.Plan.ReviewRunID,
		TargetDigest:   dispatched.Plan.TargetDigest,
		Authority:      contractsv1alpha1.AgentReviewTaskEvidenceAuthority,
		Provenance:     contractsv1alpha1.AgentReviewTaskEvidenceProvenance,
		Disposition:    contractsv1alpha1.AgentReviewTaskEvidenceDisposition,
		ContentPolicy:  contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		Completeness:   contractsv1alpha1.AgentReviewTaskEvidencePartial,
		ReasonCodes:    []string{contractsv1alpha1.AgentReviewTaskEvidenceUnavailable},
		TaskExecutions: []contractsv1alpha1.AgentReviewTaskExecutionEvidence{},
	}
	taskData, err := json.Marshal(taskEvidence)
	if err != nil {
		t.Fatal(err)
	}
	taskBinding := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		taskData,
	)
	receipts := contractsv1alpha1.AgentExecutionReceiptCollection{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        dispatched.Plan.PlanID, SourceRunID: dispatched.Plan.ReviewRunID,
		ExecutionID: dispatched.Request.ExecutionID, ReviewRunID: dispatched.Plan.ReviewRunID,
		Receipts: []contractsv1alpha1.AgentExecutionReceipt{},
	}
	receiptData, err := json.Marshal(receipts)
	if err != nil {
		t.Fatal(err)
	}
	receiptBinding := fixture.repository.addGoverned(
		fixture.subject,
		contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		receiptData,
	)
	return taskBinding, receiptBinding
}
