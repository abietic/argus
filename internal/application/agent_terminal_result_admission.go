package application

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// AdmittedAgentStageTerminalOutcome is an authenticated operational failure or
// provider cancellation. It deliberately has no hypothesis evidence: an
// execution failure is not a review Finding, Decision, or evaluation label.
type AdmittedAgentStageTerminalOutcome struct {
	Result                 contractsv1alpha1.StageExecutionResult
	Gate                   runmodel.AgentStageTerminalGate
	DiagnosticTaskEvidence *runmodel.AgentArtifactProjection
	DiagnosticReceipts     *runmodel.AgentArtifactProjection
	Recovered              bool
}

// AdmitGovernedAgentStageTerminalOutcome is the formal failed/canceled result
// ingress. It applies the same binding, callback, immutable request, terminal
// race, and optional trace checks as succeeded result admission, while never
// manufacturing an empty ReviewHypothesisSet.
func (admitter *AgentStageResultAdmitter) AdmitGovernedAgentStageTerminalOutcome(
	ctx context.Context,
	subject AgentPlanningSubject,
	command AdmitAgentStageResultCommand,
) (AdmittedAgentStageTerminalOutcome, error) {
	if ctx == nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	if admitter == nil || admitter.repository == nil || admitter.callbacks == nil ||
		admitter.now == nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"agent-stage result admitter is not initialized",
		)
	}
	if err := subject.validate(); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"validate formal agent terminal outcome subject: %w",
			err,
		)
	}
	ledgerSubject := planningLedgerSubject(subject)
	if err := ledgerSubject.Validate(); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"validate artifact-compatible formal terminal outcome subject: %w",
			err,
		)
	}
	if err := validateLocalIdentity("review_run_id", command.ReviewRunID); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	if command.StageID == "" {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf("stage_id is required")
	}
	if len(command.ResultJSON) == 0 || len(command.ResultJSON) > maxFormalAgentStageResultBytes {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"StageExecutionResult must be between 1 and %d bytes",
			maxFormalAgentStageResultBytes,
		)
	}
	if len(command.CallbackProof) > maxFormalAgentCallbackProofBytes {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"provider callback proof must not exceed %d bytes",
			maxFormalAgentCallbackProofBytes,
		)
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(command.ResultJSON)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"strictly decode formal StageExecutionResult: %w",
			err,
		)
	}
	if result.Status != contractsv1alpha1.StageExecutionFailed &&
		result.Status != contractsv1alpha1.StageExecutionCanceled {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"terminal outcome admission requires failed or canceled status, got %q",
			result.Status,
		)
	}
	if result.Failure == nil || result.Output != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"failed/canceled StageExecutionResult requires failure and forbids output",
		)
	}
	canonicalResult, err := json.Marshal(result)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"marshal canonical formal StageExecutionResult: %w",
			err,
		)
	}
	resultRef := localRefForBytes(runmodel.ContractStageExecutionResult, canonicalResult)
	callbackProof := slices.Clone(command.CallbackProof)

	admission, found, err := admitter.repository.LookupAgentStagePlanAdmission(
		ctx,
		command.ReviewRunID,
		command.StageID,
		ledgerSubject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("formal agent-stage plan admission does not exist")
		}
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"lookup formal agent-stage plan admission: %w",
			err,
		)
	}
	intent, found, err := admitter.repository.LookupAgentStageDispatchIntent(
		ctx,
		command.ReviewRunID,
		command.StageID,
		result.Attempt,
		result.Generation,
		ledgerSubject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("formal agent-stage dispatch intent does not exist")
		}
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"lookup formal agent-stage dispatch intent: %w",
			err,
		)
	}
	binding, found, err := admitter.repository.LookupAgentStageExecutionBinding(
		ctx,
		command.ReviewRunID,
		intent.IntentID,
		ledgerSubject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("formal agent-stage execution binding does not exist")
		}
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"lookup formal agent-stage execution binding: %w",
			err,
		)
	}
	if err := validateAgentStageResultExecutionFence(result, admission, intent, binding); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	request, err := admitter.loadBoundAgentStageRequest(ctx, intent, result)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}

	if terminal, terminalFound, terminalErr := admitter.repository.LookupAgentStageTerminalGate(
		ctx,
		command.ReviewRunID,
		intent.IntentID,
		ledgerSubject,
	); terminalErr != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"lookup existing formal terminal winner: %w",
			terminalErr,
		)
	} else if terminalFound && terminal.Kind == runmodel.AgentStageCancellationRequested {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"%w: cancellation %q is authoritative",
			ErrAgentStageResultLostTerminalRace,
			terminal.GateID,
		)
	} else if terminalFound {
		if !sameAgentStageAcceptedOutcome(terminal, binding, result, resultRef) {
			return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
				"changed StageExecutionResult conflicts with terminal winner %q of kind %q",
				terminal.GateID,
				terminal.Kind,
			)
		}
		if err := admitter.verifyRecoveredAgentStageTerminalOutcome(
			ctx,
			binding,
			result,
			canonicalResult,
			terminal,
			request.Capability.Trust,
		); err != nil {
			return AdmittedAgentStageTerminalOutcome{}, err
		}
		return AdmittedAgentStageTerminalOutcome{
			Result: result, Gate: terminal,
			DiagnosticTaskEvidence: terminal.Outcome.AgentTaskEvidence,
			DiagnosticReceipts:     terminal.Outcome.AgentExecutionReceipts,
			Recovered:              true,
		}, nil
	}

	callbackReceipt, callbackReceiptRef, callbackFound, err :=
		admitter.repository.LookupAgentStageResultCallbackReceipt(
			ctx,
			command.ReviewRunID,
			binding.BindingID,
			ledgerSubject,
		)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"lookup authenticated provider callback receipt: %w",
			err,
		)
	}
	var callbackVerifiedAt time.Time
	if callbackFound {
		if err := admitter.validateRecoveredAgentStageCallbackReceipt(
			ctx,
			callbackReceipt,
			callbackReceiptRef,
			binding,
			resultRef,
			result.RecordedAt,
			request.Capability.Trust,
		); err != nil {
			return AdmittedAgentStageTerminalOutcome{}, err
		}
		callbackVerifiedAt = callbackReceipt.VerifiedAt
	} else {
		if len(callbackProof) == 0 {
			return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
				"fresh provider callback proof must be between 1 and %d bytes",
				maxFormalAgentCallbackProofBytes,
			)
		}
		verifier, verifyErr := admitter.callbacks.VerifyAgentStageResultCallback(
			ctx,
			subject,
			binding,
			request.Capability.Trust,
			slices.Clone(canonicalResult),
			slices.Clone(callbackProof),
		)
		if verifyErr != nil {
			return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
				"authenticate formal provider result callback: %w",
				verifyErr,
			)
		}
		if err := verifier.Validate(); err != nil {
			return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
				"validate exact provider callback verifier: %w",
				err,
			)
		}
		if err := validateCallbackVerifierAgainstExecutorTrust(
			verifier,
			request.Capability.Trust,
		); err != nil {
			return AdmittedAgentStageTerminalOutcome{}, err
		}
		callbackVerifiedAt = admitter.now().UTC()
		if callbackVerifiedAt.IsZero() || callbackVerifiedAt.Before(binding.RecordedAt) ||
			callbackVerifiedAt.Before(result.RecordedAt) {
			return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
				"provider callback verification predates binding or provider result",
			)
		}
		receiptContext, cancelReceipt := context.WithTimeout(
			context.WithoutCancel(ctx),
			formalAgentAppendRecoveryTimeout,
		)
		defer cancelReceipt()
		callbackReceipt, callbackReceiptRef, err =
			admitter.persistFreshAgentStageCallbackReceipt(
				receiptContext,
				intent,
				binding,
				resultRef,
				canonicalResult,
				callbackProof,
				verifier,
				callbackVerifiedAt,
			)
		if err != nil {
			return AdmittedAgentStageTerminalOutcome{}, err
		}
	}

	planBytes, err := admitter.repository.ReadLocalArtifact(ctx, admission.Plan.Local)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"read admitted AgentStagePlan: %w",
			err,
		)
	}
	if localRefForBytes(runmodel.ContractAgentStagePlan, planBytes) != admission.Plan.Local {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"AgentStagePlan bytes do not match plan admission ref",
		)
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(planBytes)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"strictly decode admitted AgentStagePlan: %w",
			err,
		)
	}
	if err := validateAgentStagePlanAdmissionClosure(plan, admission); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	if err := validateRequestAgainstIntentAndPlan(
		request,
		intent,
		PreparedAgentStage{Plan: plan, Admission: admission},
	); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	if err := admitter.admitFormalAgentTaskEvidence(ctx, subject, result, plan, request); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	if err := admitter.admitFormalAgentExecutionReceipts(ctx, subject, result, plan, request); err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	diagnosticTaskEvidence := agentStageDiagnosticProjection(result.AgentTaskEvidence)
	diagnosticReceipts := agentStageDiagnosticProjection(result.AgentExecutionReceipts)

	traceProjection, err := admitter.projectAgentStageTerminalTrace(ctx, subject, result)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	if callbackReceipt.ReceiptID == "" {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"authenticated provider callback receipt was not registered",
		)
	}
	acceptedAt := admitter.now().UTC()
	if acceptedAt.IsZero() || acceptedAt.Before(callbackVerifiedAt) {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"agent terminal outcome acceptance predates callback verification",
		)
	}
	gate, err := sealAgentStageTerminalOutcomeGate(
		admission,
		intent,
		binding,
		request,
		result,
		resultRef,
		traceProjection,
		diagnosticTaskEvidence,
		diagnosticReceipts,
		callbackReceiptRef,
		callbackReceipt.SHA256,
		acceptedAt,
	)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, fmt.Errorf(
			"seal formal terminal outcome gate: %w",
			err,
		)
	}
	gate, err = admitter.appendOrResolveAgentStageResultGate(ctx, gate)
	if err != nil {
		return AdmittedAgentStageTerminalOutcome{}, err
	}
	return AdmittedAgentStageTerminalOutcome{
		Result: result, Gate: gate,
		DiagnosticTaskEvidence: diagnosticTaskEvidence,
		DiagnosticReceipts:     diagnosticReceipts,
	}, nil
}

func agentStageDiagnosticProjection(
	binding *contractsv1alpha1.ArtifactBinding,
) *runmodel.AgentArtifactProjection {
	if binding == nil {
		return nil
	}
	return &runmodel.AgentArtifactProjection{
		Local: runmodel.ArtifactRef{
			URI:    "artifact://local/sha256/" + binding.Ref.SHA256,
			SHA256: binding.Ref.SHA256, SizeBytes: binding.Ref.SizeBytes,
			Contract: binding.Contract,
		},
		Governed: runmodel.GovernedArtifactBinding{
			URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
			SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
		},
	}
}

func (admitter *AgentStageResultAdmitter) projectAgentStageTerminalTrace(
	ctx context.Context,
	subject AgentPlanningSubject,
	result contractsv1alpha1.StageExecutionResult,
) (*runmodel.AgentArtifactProjection, error) {
	if result.TraceManifest == nil {
		return nil, nil
	}
	if result.TraceManifest.Ref.SizeBytes <= 0 ||
		result.TraceManifest.Ref.SizeBytes > maxFormalAgentTraceManifestBytes {
		return nil, fmt.Errorf("formal trace manifest exceeds the host admission byte limit")
	}
	traceBytes, err := admitter.repository.VerifyRead(ctx, subject, *result.TraceManifest)
	if err != nil {
		return nil, fmt.Errorf("resolve governed formal trace manifest: %w", err)
	}
	if int64(len(traceBytes)) != result.TraceManifest.Ref.SizeBytes {
		return nil, fmt.Errorf("resolved formal trace manifest violates its exact size")
	}
	traceLocal, err := admitter.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractStageExecutionTraceManifest,
		traceBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("persist exact formal trace manifest: %w", err)
	}
	projection := &runmodel.AgentArtifactProjection{
		Local: traceLocal,
		Governed: runmodel.GovernedArtifactBinding{
			URI: result.TraceManifest.Ref.URI, SHA256: result.TraceManifest.Ref.SHA256,
			SizeBytes: result.TraceManifest.Ref.SizeBytes, Contract: result.TraceManifest.Contract,
		},
	}
	if projection.Local.SHA256 != projection.Governed.SHA256 ||
		projection.Local.SizeBytes != projection.Governed.SizeBytes ||
		projection.Local.Contract != projection.Governed.Contract {
		return nil, fmt.Errorf(
			"local and governed trace manifest do not identify exact same bytes",
		)
	}
	return projection, nil
}

func sealAgentStageTerminalOutcomeGate(
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	request contractsv1alpha1.StageExecutionRequest,
	result contractsv1alpha1.StageExecutionResult,
	resultRef runmodel.ArtifactRef,
	traceManifest *runmodel.AgentArtifactProjection,
	diagnosticTaskEvidence *runmodel.AgentArtifactProjection,
	diagnosticReceipts *runmodel.AgentArtifactProjection,
	callbackReceiptRef runmodel.ArtifactRef,
	callbackReceiptSHA256 string,
	acceptedAt time.Time,
) (runmodel.AgentStageTerminalGate, error) {
	kind := runmodel.AgentStageFailedResultAccepted
	if result.Status == contractsv1alpha1.StageExecutionCanceled {
		kind = runmodel.AgentStageCanceledResultAccepted
	}
	return runmodel.SealAgentStageTerminalGate(runmodel.AgentStageTerminalGate{
		SchemaVersion: runmodel.AgentStageTerminalGateSchemaVersion,
		Kind:          kind, Subject: admission.Subject,
		ReviewRunID: admission.ReviewRunID, Stage: admission.Stage,
		AdmissionID: admission.AdmissionID, AdmissionSHA256: admission.SHA256,
		IntentID: intent.IntentID, IntentSHA256: intent.SHA256,
		WorkloadID: intent.WorkloadID, LeaseID: intent.LeaseID, LeaseWorker: intent.LeaseWorker,
		ExecutionID: intent.ExecutionID, Attempt: intent.Attempt,
		Generation: intent.Generation, FencingToken: intent.FencingToken,
		RequestRef: intent.RequestRef, RequestSemanticSHA256: intent.RequestSemanticSHA256,
		RequestDeadline: request.Deadline, CapabilitySHA256: intent.CapabilitySHA256,
		Outcome: &runmodel.AgentStageNonSucceededResultAcceptance{
			BindingID: binding.BindingID, BindingSHA256: binding.SHA256,
			ResultRef: resultRef, TraceManifest: traceManifest,
			AgentTaskEvidence:      diagnosticTaskEvidence,
			AgentExecutionReceipts: diagnosticReceipts,
			CallbackReceiptRef:     callbackReceiptRef,
			CallbackReceiptSHA256:  callbackReceiptSHA256,
			Status:                 string(result.Status),
			Failure: runmodel.AgentStageExecutionFailure{
				Code: result.Failure.Code, Message: result.Failure.Message,
				Retryable: result.Failure.Retryable,
			},
			Completeness: result.Completeness,
			CompletenessNotes: append(
				make([]string, 0, len(result.CompletenessNotes)),
				result.CompletenessNotes...,
			),
			ProviderResultRecordedAt: result.RecordedAt,
			AcceptedAt:               acceptedAt,
		},
	})
}

func sameAgentStageAcceptedOutcome(
	gate runmodel.AgentStageTerminalGate,
	binding runmodel.AgentStageExecutionBinding,
	result contractsv1alpha1.StageExecutionResult,
	resultRef runmodel.ArtifactRef,
) bool {
	if gate.Outcome == nil || result.Failure == nil || result.Output != nil ||
		(gate.Kind != runmodel.AgentStageFailedResultAccepted &&
			gate.Kind != runmodel.AgentStageCanceledResultAccepted) {
		return false
	}
	wantKind := runmodel.AgentStageFailedResultAccepted
	if result.Status == contractsv1alpha1.StageExecutionCanceled {
		wantKind = runmodel.AgentStageCanceledResultAccepted
	}
	outcome := gate.Outcome
	return gate.Kind == wantKind && outcome.BindingID == binding.BindingID &&
		outcome.BindingSHA256 == binding.SHA256 && outcome.ResultRef == resultRef &&
		outcome.Status == string(result.Status) && outcome.Failure.Code == result.Failure.Code &&
		outcome.Failure.Message == result.Failure.Message &&
		outcome.Failure.Retryable == result.Failure.Retryable &&
		outcome.Completeness == result.Completeness &&
		slices.Equal(outcome.CompletenessNotes, result.CompletenessNotes) &&
		outcome.ProviderResultRecordedAt.Equal(result.RecordedAt) &&
		agentStageTraceMatchesResult(outcome.TraceManifest, result.TraceManifest) &&
		agentStageTraceMatchesResult(outcome.AgentTaskEvidence, result.AgentTaskEvidence) &&
		agentStageTraceMatchesResult(outcome.AgentExecutionReceipts, result.AgentExecutionReceipts)
}

func (admitter *AgentStageResultAdmitter) verifyRecoveredAgentStageTerminalOutcome(
	ctx context.Context,
	binding runmodel.AgentStageExecutionBinding,
	result contractsv1alpha1.StageExecutionResult,
	canonicalResult []byte,
	gate runmodel.AgentStageTerminalGate,
	trust contractsv1alpha1.ExecutorTrust,
) error {
	if gate.Outcome == nil {
		return fmt.Errorf("recovered terminal winner has no outcome")
	}
	resultBytes, err := admitter.repository.ReadLocalArtifact(ctx, gate.Outcome.ResultRef)
	if err != nil {
		return fmt.Errorf("read recovered canonical StageExecutionResult: %w", err)
	}
	if !slices.Equal(resultBytes, canonicalResult) ||
		localRefForBytes(runmodel.ContractStageExecutionResult, resultBytes) != gate.Outcome.ResultRef {
		return fmt.Errorf("recovered canonical StageExecutionResult differs from terminal outcome")
	}
	receipt, receiptRef, found, err := admitter.repository.LookupAgentStageResultCallbackReceipt(
		ctx,
		gate.ReviewRunID,
		binding.BindingID,
		gate.Subject,
	)
	if err != nil {
		return fmt.Errorf("lookup recovered callback receipt: %w", err)
	}
	if !found {
		return fmt.Errorf("recovered callback receipt does not exist")
	}
	if receiptRef != gate.Outcome.CallbackReceiptRef ||
		receipt.SHA256 != gate.Outcome.CallbackReceiptSHA256 {
		return fmt.Errorf("recovered callback receipt differs from terminal outcome")
	}
	if err := admitter.validateRecoveredAgentStageCallbackReceipt(
		ctx,
		receipt,
		receiptRef,
		binding,
		gate.Outcome.ResultRef,
		result.RecordedAt,
		trust,
	); err != nil {
		return err
	}
	if gate.Outcome.TraceManifest != nil {
		traceBytes, err := admitter.repository.ReadLocalArtifact(
			ctx,
			gate.Outcome.TraceManifest.Local,
		)
		if err != nil {
			return fmt.Errorf("read recovered terminal trace manifest: %w", err)
		}
		if localRefForBytes(runmodel.ContractStageExecutionTraceManifest, traceBytes) !=
			gate.Outcome.TraceManifest.Local {
			return fmt.Errorf("recovered terminal trace bytes do not match projection")
		}
	}
	if err := admitter.verifyLocalFormalAgentTaskEvidence(ctx, result); err != nil {
		return err
	}
	if err := admitter.verifyLocalFormalAgentExecutionReceipts(ctx, result); err != nil {
		return err
	}
	return nil
}
