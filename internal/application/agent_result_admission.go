package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	maxFormalAgentStageResultBytes      = 64 << 10
	maxFormalAgentTraceManifestBytes    = 1 << 20
	maxFormalAgentRawCandidateBytes     = 16 << 20
	maxFormalAgentTaskEvidenceBytes     = 16 << 20
	maxFormalAgentExecutionReceiptBytes = 16 << 20
	maxFormalAgentCallbackProofBytes    = 64 << 10
	formalAgentAppendRecoveryTimeout    = 5 * time.Second
)

var ErrAgentStageResultLostTerminalRace = errors.New(
	"formal agent-stage result lost the terminal race",
)

var ErrAgentStageResultTerminalOutcomeUnknown = errors.New(
	"formal agent-stage terminal append outcome is unknown",
)

var ErrAgentStageEvidenceOutcomeUnknown = errors.New(
	"formal agent-stage evidence append outcome is unknown",
)

var ErrAgentStageCallbackReceiptOutcomeUnknown = errors.New(
	"formal agent-stage callback receipt append outcome is unknown",
)

// AgentStageResultRepository is the single trusted persistence boundary used
// by formal result admission. The caller supplies only authenticated scope and
// result bytes; every authority-bearing request, plan, binding, and artifact
// reference is resolved from the immutable local ledger.
type AgentStageResultRepository interface {
	ReadLocalArtifact(context.Context, runmodel.ArtifactRef) ([]byte, error)
	PutLocalArtifact(context.Context, string, []byte) (runmodel.ArtifactRef, error)
	VerifyRead(
		context.Context,
		AgentPlanningSubject,
		contractsv1alpha1.ArtifactBinding,
	) ([]byte, error)
	LookupAgentStagePlanAdmission(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStagePlanAdmission, bool, error)
	LookupAgentStageDispatchIntent(
		context.Context,
		string,
		string,
		int,
		int,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageDispatchIntent, bool, error)
	LookupAgentStageExecutionBinding(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageExecutionBinding, bool, error)
	LookupAgentStageTerminalGate(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageTerminalGate, bool, error)
	AppendAgentStageTerminalGate(context.Context, runmodel.AgentStageTerminalGate) error
	LookupAgentStageResultCallbackReceipt(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef, bool, error)
	AppendAgentStageResultCallbackReceipt(
		context.Context,
		runmodel.AgentStageResultCallbackReceipt,
		runmodel.ArtifactRef,
	) error
	LookupAgentStageHypothesisEvidence(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageHypothesisEvidence, bool, error)
	AppendAgentStageHypothesisEvidence(
		context.Context,
		runmodel.AgentStageHypothesisEvidence,
	) error
}

// AgentStageResultCallbackVerifier authenticates the transport provenance of
// a provider callback after the exact durable binding has been resolved. Echoed
// request fields are fences, not credentials; a repository-scoped caller must
// never be able to manufacture a succeeded result by copying them. Verification
// must be replay-idempotent for the exact subject/binding/result/proof tuple and
// return the same exact verifier ref: it may be retried if the host crashes
// between verification and durable receipt registration. Implementations MUST
// reject reusable bearer credentials and guessable codes as callback proof;
// the receipt persists an ordinary SHA-256 tuple fingerprint, not a keyed
// secret-redaction primitive. Provider handles are likewise opaque identifiers,
// never credentials. ExecutorTrust MUST come from the exact persisted request,
// never from callback input or mutable adapter configuration.
type AgentStageResultCallbackVerifier interface {
	VerifyAgentStageResultCallback(
		context.Context,
		AgentPlanningSubject,
		runmodel.AgentStageExecutionBinding,
		contractsv1alpha1.ExecutorTrust,
		[]byte,
		[]byte,
	) (runmodel.AgentStageResultCallbackVerifierRef, error)
}

type AdmitAgentStageResultCommand struct {
	ReviewRunID   string
	StageID       string
	ResultJSON    []byte
	CallbackProof []byte
}

type AdmittedAgentStageResult struct {
	Result     contractsv1alpha1.StageExecutionResult
	Hypotheses contractsv1alpha1.ReviewHypothesisSet
	Gate       runmodel.AgentStageTerminalGate
	Evidence   runmodel.AgentStageHypothesisEvidence
	Recovered  bool
}

type AgentStageResultAdmitter struct {
	repository AgentStageResultRepository
	callbacks  AgentStageResultCallbackVerifier
	now        func() time.Time
}

func NewAgentStageResultAdmitter(
	repository AgentStageResultRepository,
	callbacks AgentStageResultCallbackVerifier,
	now func() time.Time,
) (*AgentStageResultAdmitter, error) {
	if repository == nil || callbacks == nil {
		return nil, fmt.Errorf(
			"agent-stage result repository and provider callback verifier are required",
		)
	}
	if now == nil {
		now = time.Now
	}
	return &AgentStageResultAdmitter{
		repository: repository,
		callbacks:  callbacks,
		now:        now,
	}, nil
}

// AdmitGovernedAgentStageResult is the only formal ReviewHypothesisSet ingress.
// It deliberately accepts no caller-selected request, plan, output URI, or
// execution binding. A succeeded result must win the same immutable terminal
// gate used by cancellation before it can become hypothesis evidence.
func (admitter *AgentStageResultAdmitter) AdmitGovernedAgentStageResult(
	ctx context.Context,
	subject AgentPlanningSubject,
	command AdmitAgentStageResultCommand,
) (AdmittedAgentStageResult, error) {
	if ctx == nil {
		return AdmittedAgentStageResult{}, fmt.Errorf("context is required")
	}
	if admitter == nil || admitter.repository == nil || admitter.callbacks == nil ||
		admitter.now == nil {
		return AdmittedAgentStageResult{}, fmt.Errorf("agent-stage result admitter is not initialized")
	}
	if err := subject.validate(); err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"validate formal agent result subject: %w",
			err,
		)
	}
	ledgerSubject := planningLedgerSubject(subject)
	if err := ledgerSubject.Validate(); err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"validate artifact-compatible formal agent result subject: %w",
			err,
		)
	}
	if err := validateLocalIdentity("review_run_id", command.ReviewRunID); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if command.StageID == "" {
		return AdmittedAgentStageResult{}, fmt.Errorf("stage_id is required")
	}
	if len(command.ResultJSON) == 0 || len(command.ResultJSON) > maxFormalAgentStageResultBytes {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"StageExecutionResult must be between 1 and %d bytes",
			maxFormalAgentStageResultBytes,
		)
	}
	if len(command.CallbackProof) > maxFormalAgentCallbackProofBytes {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"provider callback proof must not exceed %d bytes",
			maxFormalAgentCallbackProofBytes,
		)
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(command.ResultJSON)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"strictly decode formal StageExecutionResult: %w",
			err,
		)
	}
	canonicalResult, err := json.Marshal(result)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
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
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"lookup formal agent-stage plan admission: %w",
			err,
		)
	}
	if !found {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"formal agent-stage plan admission does not exist",
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
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"lookup formal agent-stage dispatch intent: %w",
			err,
		)
	}
	if !found {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"formal agent-stage dispatch intent does not exist",
		)
	}
	binding, found, err := admitter.repository.LookupAgentStageExecutionBinding(
		ctx,
		command.ReviewRunID,
		intent.IntentID,
		ledgerSubject,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"lookup formal agent-stage execution binding: %w",
			err,
		)
	}
	if !found {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"formal agent-stage execution binding does not exist",
		)
	}
	if err := validateAgentStageResultDispatchFence(result, admission, intent, binding); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	request, err := admitter.loadBoundAgentStageRequest(ctx, intent, result)
	if err != nil {
		return AdmittedAgentStageResult{}, err
	}
	var acceptedTerminal *runmodel.AgentStageTerminalGate
	if terminal, terminalFound, terminalErr := admitter.repository.LookupAgentStageTerminalGate(
		ctx,
		command.ReviewRunID,
		intent.IntentID,
		ledgerSubject,
	); terminalErr != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"lookup existing formal terminal winner: %w",
			terminalErr,
		)
	} else if terminalFound && terminal.Kind == runmodel.AgentStageCancellationRequested {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"%w: cancellation %q is authoritative",
			ErrAgentStageResultLostTerminalRace,
			terminal.GateID,
		)
	} else if terminalFound {
		if terminal.Kind != runmodel.AgentStageSucceededResultAccepted || terminal.Completion == nil {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"succeeded StageExecutionResult conflicts with terminal winner %q of kind %q",
				terminal.GateID,
				terminal.Kind,
			)
		}
		if !sameAgentStageAcceptedResult(
			terminal,
			binding,
			result,
			resultRef,
			terminal.Completion.Output,
		) {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"changed StageExecutionResult conflicts with accepted terminal winner %q",
				terminal.GateID,
			)
		}
		if err := admitter.validateAcceptedAgentStageCallbackReceipt(
			ctx,
			terminal,
			binding,
			resultRef,
			result.RecordedAt,
			request.Capability.Trust,
		); err != nil {
			return AdmittedAgentStageResult{}, err
		}
		accepted := terminal
		acceptedTerminal = &accepted
	}

	if evidence, found, lookupErr := admitter.repository.LookupAgentStageHypothesisEvidence(
		ctx,
		command.ReviewRunID,
		binding.BindingID,
		ledgerSubject,
	); lookupErr != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"lookup existing formal hypothesis evidence: %w",
			lookupErr,
		)
	} else if found {
		return admitter.recoverAdmittedAgentStageResult(
			ctx,
			subject,
			result,
			resultRef,
			intent,
			binding,
			evidence,
		)
	}

	var callbackReceipt runmodel.AgentStageResultCallbackReceipt
	var callbackReceiptRef runmodel.ArtifactRef
	var callbackVerifier runmodel.AgentStageResultCallbackVerifierRef
	var callbackVerifiedAt time.Time
	if acceptedTerminal == nil {
		callbackReceipt, callbackReceiptRef, found, err =
			admitter.repository.LookupAgentStageResultCallbackReceipt(
				ctx,
				command.ReviewRunID,
				binding.BindingID,
				ledgerSubject,
			)
		if err != nil {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"lookup authenticated provider callback receipt: %w",
				err,
			)
		}
		if found {
			if err := admitter.validateRecoveredAgentStageCallbackReceipt(
				ctx,
				callbackReceipt,
				callbackReceiptRef,
				binding,
				resultRef,
				result.RecordedAt,
				request.Capability.Trust,
			); err != nil {
				return AdmittedAgentStageResult{}, err
			}
			callbackVerifiedAt = callbackReceipt.VerifiedAt
		} else {
			if len(callbackProof) == 0 ||
				len(callbackProof) > maxFormalAgentCallbackProofBytes {
				return AdmittedAgentStageResult{}, fmt.Errorf(
					"fresh provider callback proof must be between 1 and %d bytes",
					maxFormalAgentCallbackProofBytes,
				)
			}
			callbackVerifier, err = admitter.callbacks.VerifyAgentStageResultCallback(
				ctx,
				subject,
				binding,
				request.Capability.Trust,
				slices.Clone(canonicalResult),
				slices.Clone(callbackProof),
			)
			if err != nil {
				return AdmittedAgentStageResult{}, fmt.Errorf(
					"authenticate formal provider result callback: %w",
					err,
				)
			}
			if err := callbackVerifier.Validate(); err != nil {
				return AdmittedAgentStageResult{}, fmt.Errorf(
					"validate exact provider callback verifier: %w",
					err,
				)
			}
			if err := validateCallbackVerifierAgainstExecutorTrust(
				callbackVerifier,
				request.Capability.Trust,
			); err != nil {
				return AdmittedAgentStageResult{}, err
			}
			callbackVerifiedAt = admitter.now().UTC()
			if callbackVerifiedAt.IsZero() {
				return AdmittedAgentStageResult{}, fmt.Errorf(
					"provider callback verification clock returned zero time",
				)
			}
		}
		if callbackVerifiedAt.Before(binding.RecordedAt) ||
			callbackVerifiedAt.Before(result.RecordedAt) {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"provider callback verification predates binding or provider result",
			)
		}
		if callbackReceipt.ReceiptID == "" {
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
					callbackVerifier,
					callbackVerifiedAt,
				)
			if err != nil {
				return AdmittedAgentStageResult{}, err
			}
		}
	}

	planBytes, err := admitter.repository.ReadLocalArtifact(ctx, admission.Plan.Local)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf("read admitted AgentStagePlan: %w", err)
	}
	if localRefForBytes(runmodel.ContractAgentStagePlan, planBytes) != admission.Plan.Local {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"AgentStagePlan bytes do not match plan admission ref",
		)
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(planBytes)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"strictly decode admitted AgentStagePlan: %w",
			err,
		)
	}
	if err := validateAgentStagePlanAdmissionClosure(plan, admission); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := validateRequestAgainstIntentAndPlan(
		request,
		intent,
		PreparedAgentStage{Plan: plan, Admission: admission},
	); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if result.Output == nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"succeeded StageExecutionResult has no hypothesis output",
		)
	}
	if result.Output.Ref.SizeBytes <= 0 || result.Output.Ref.SizeBytes > plan.Budget.MaxOutputBytes {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"formal hypothesis output exceeds the admitted byte budget",
		)
	}

	inputBytes, err := admitter.repository.ReadLocalArtifact(
		ctx,
		admission.Sources.ReviewInput.Local,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf("read frozen ReviewInput: %w", err)
	}
	if localRefForBytes(runmodel.ContractReviewInput, inputBytes) !=
		admission.Sources.ReviewInput.Local {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"ReviewInput bytes do not match plan admission ref",
		)
	}
	input, err := reviewcore.DecodeReviewInput(inputBytes)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"strictly decode frozen ReviewInput: %w",
			err,
		)
	}
	if acceptedTerminal != nil {
		return admitter.resumeAgentStageEvidenceFromAcceptedTerminal(
			ctx,
			result,
			plan,
			request,
			input,
			admission,
			intent,
			binding,
			*acceptedTerminal,
		)
	}

	outputBytes, err := admitter.repository.VerifyRead(ctx, subject, *result.Output)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"resolve governed formal hypothesis output: %w",
			err,
		)
	}
	if int64(len(outputBytes)) != result.Output.Ref.SizeBytes ||
		int64(len(outputBytes)) > plan.Budget.MaxOutputBytes {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"resolved formal hypothesis output violates its exact or admitted size",
		)
	}
	hypotheses, err := contractsv1alpha1.DecodeReviewHypothesisSet(outputBytes)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"strictly decode formal ReviewHypothesisSet: %w",
			err,
		)
	}
	if err := validateFormalReviewHypothesisSet(
		ctx,
		hypotheses,
		plan,
		request,
		result,
		intent,
		input,
	); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := admitter.admitFormalAgentRawCandidates(
		ctx, subject, result, plan, request, hypotheses,
	); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := admitter.admitFormalAgentTaskEvidence(ctx, subject, result, plan, request); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := admitter.admitFormalAgentExecutionReceipts(ctx, subject, result, plan, request); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	var traceProjection *runmodel.AgentArtifactProjection
	if result.TraceManifest != nil {
		if result.TraceManifest.Ref.SizeBytes <= 0 ||
			result.TraceManifest.Ref.SizeBytes > maxFormalAgentTraceManifestBytes {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"formal trace manifest exceeds the host admission byte limit",
			)
		}
		traceBytes, traceErr := admitter.repository.VerifyRead(
			ctx,
			subject,
			*result.TraceManifest,
		)
		if traceErr != nil {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"resolve governed formal trace manifest: %w",
				traceErr,
			)
		}
		if int64(len(traceBytes)) != result.TraceManifest.Ref.SizeBytes {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"resolved formal trace manifest violates its exact size",
			)
		}
		traceLocal, traceErr := admitter.repository.PutLocalArtifact(
			ctx,
			runmodel.ContractStageExecutionTraceManifest,
			traceBytes,
		)
		if traceErr != nil {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"persist exact formal trace manifest: %w",
				traceErr,
			)
		}
		projection := runmodel.AgentArtifactProjection{
			Local: traceLocal,
			Governed: runmodel.GovernedArtifactBinding{
				URI:       result.TraceManifest.Ref.URI,
				SHA256:    result.TraceManifest.Ref.SHA256,
				SizeBytes: result.TraceManifest.Ref.SizeBytes,
				Contract:  result.TraceManifest.Contract,
			},
		}
		if projection.Local.SHA256 != projection.Governed.SHA256 ||
			projection.Local.SizeBytes != projection.Governed.SizeBytes ||
			projection.Local.Contract != projection.Governed.Contract {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"local and governed trace manifest do not identify exact same bytes",
			)
		}
		traceProjection = &projection
	}

	outputLocalRef, err := admitter.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractReviewHypothesisSet,
		outputBytes,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"persist exact formal ReviewHypothesisSet: %w",
			err,
		)
	}
	outputProjection := runmodel.AgentArtifactProjection{
		Local: outputLocalRef,
		Governed: runmodel.GovernedArtifactBinding{
			URI: result.Output.Ref.URI, SHA256: result.Output.Ref.SHA256,
			SizeBytes: result.Output.Ref.SizeBytes, Contract: result.Output.Contract,
		},
	}
	if outputProjection.Local.SHA256 != outputProjection.Governed.SHA256 ||
		outputProjection.Local.SizeBytes != outputProjection.Governed.SizeBytes ||
		outputProjection.Local.Contract != outputProjection.Governed.Contract {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"local and governed hypothesis output do not identify exact same bytes",
		)
	}
	if callbackReceipt.ReceiptID == "" {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"authenticated provider callback receipt was not registered",
		)
	}

	acceptedAt := admitter.now().UTC()
	if acceptedAt.IsZero() {
		return AdmittedAgentStageResult{}, fmt.Errorf("agent result admission clock returned zero time")
	}
	if acceptedAt.Before(callbackVerifiedAt) {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"agent result acceptance predates callback verification",
		)
	}
	gate, err := sealAgentStageResultTerminalGate(
		admission,
		intent,
		binding,
		request,
		result,
		resultRef,
		outputProjection,
		traceProjection,
		callbackReceiptRef,
		callbackReceipt.SHA256,
		acceptedAt,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"seal formal result terminal gate: %w",
			err,
		)
	}
	gate, err = admitter.appendOrResolveAgentStageResultGate(ctx, gate)
	if err != nil {
		return AdmittedAgentStageResult{}, err
	}

	evidence, err := admitter.commitAgentStageHypothesisEvidence(
		ctx,
		admission,
		intent,
		binding,
		gate,
		hypotheses,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, err
	}
	return AdmittedAgentStageResult{
		Result: result, Hypotheses: hypotheses, Gate: gate, Evidence: evidence,
	}, nil
}

func (admitter *AgentStageResultAdmitter) recoverAdmittedAgentStageResult(
	ctx context.Context,
	subject AgentPlanningSubject,
	result contractsv1alpha1.StageExecutionResult,
	resultRef runmodel.ArtifactRef,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	evidence runmodel.AgentStageHypothesisEvidence,
) (AdmittedAgentStageResult, error) {
	if err := admitter.verifyLocalFormalAgentTaskEvidence(ctx, result); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := admitter.verifyLocalFormalAgentRawCandidates(ctx, result); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := admitter.verifyLocalFormalAgentExecutionReceipts(ctx, result); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if evidence.ResultRef != resultRef || evidence.Output.Governed.URI != result.Output.Ref.URI ||
		evidence.Output.Governed.SHA256 != result.Output.Ref.SHA256 ||
		evidence.Output.Governed.SizeBytes != result.Output.Ref.SizeBytes ||
		evidence.Output.Governed.Contract != result.Output.Contract {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"changed StageExecutionResult conflicts with admitted hypothesis evidence",
		)
	}
	gate, found, err := admitter.repository.LookupAgentStageTerminalGate(
		ctx,
		intent.ReviewRunID,
		intent.IntentID,
		intent.Subject,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"lookup accepted formal result terminal gate: %w",
			err,
		)
	}
	if !found || !sameAgentStageAcceptedResult(gate, binding, result, evidence.ResultRef, evidence.Output) {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"formal hypothesis evidence has no exact accepted terminal winner",
		)
	}
	outputBytes, err := admitter.repository.ReadLocalArtifact(ctx, evidence.Output.Local)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"read admitted formal ReviewHypothesisSet: %w",
			err,
		)
	}
	hypotheses, err := contractsv1alpha1.DecodeReviewHypothesisSet(outputBytes)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"strictly decode admitted formal ReviewHypothesisSet: %w",
			err,
		)
	}
	_ = subject // retained in the signature to make authenticated recovery explicit.
	return AdmittedAgentStageResult{
		Result: result, Hypotheses: hypotheses, Gate: gate, Evidence: evidence, Recovered: true,
	}, nil
}

func (admitter *AgentStageResultAdmitter) resumeAgentStageEvidenceFromAcceptedTerminal(
	ctx context.Context,
	result contractsv1alpha1.StageExecutionResult,
	plan contractsv1alpha1.AgentStagePlan,
	request contractsv1alpha1.StageExecutionRequest,
	input reviewcore.ReviewInput,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	gate runmodel.AgentStageTerminalGate,
) (AdmittedAgentStageResult, error) {
	if err := admitter.verifyLocalFormalAgentTaskEvidence(ctx, result); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := admitter.verifyLocalFormalAgentRawCandidates(ctx, result); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if err := admitter.verifyLocalFormalAgentExecutionReceipts(ctx, result); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	if !sameAgentStageAcceptedResult(
		gate,
		binding,
		result,
		gate.Completion.ResultRef,
		gate.Completion.Output,
	) {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"accepted terminal winner no longer matches exact StageExecutionResult",
		)
	}
	outputBytes, err := admitter.repository.ReadLocalArtifact(
		ctx,
		gate.Completion.Output.Local,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"read terminal-local formal ReviewHypothesisSet: %w",
			err,
		)
	}
	if localRefForBytes(runmodel.ContractReviewHypothesisSet, outputBytes) !=
		gate.Completion.Output.Local {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"terminal-local ReviewHypothesisSet bytes do not match accepted projection",
		)
	}
	if gate.Completion.TraceManifest != nil {
		traceBytes, traceErr := admitter.repository.ReadLocalArtifact(
			ctx,
			gate.Completion.TraceManifest.Local,
		)
		if traceErr != nil {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"read terminal-local formal trace manifest: %w",
				traceErr,
			)
		}
		if localRefForBytes(runmodel.ContractStageExecutionTraceManifest, traceBytes) !=
			gate.Completion.TraceManifest.Local {
			return AdmittedAgentStageResult{}, fmt.Errorf(
				"terminal-local trace manifest bytes do not match accepted projection",
			)
		}
	}
	hypotheses, err := contractsv1alpha1.DecodeReviewHypothesisSet(outputBytes)
	if err != nil {
		return AdmittedAgentStageResult{}, fmt.Errorf(
			"strictly decode terminal-local ReviewHypothesisSet: %w",
			err,
		)
	}
	if err := validateFormalReviewHypothesisSet(
		ctx,
		hypotheses,
		plan,
		request,
		result,
		intent,
		input,
	); err != nil {
		return AdmittedAgentStageResult{}, err
	}
	evidence, err := admitter.commitAgentStageHypothesisEvidence(
		ctx,
		admission,
		intent,
		binding,
		gate,
		hypotheses,
	)
	if err != nil {
		return AdmittedAgentStageResult{}, err
	}
	return AdmittedAgentStageResult{
		Result: result, Hypotheses: hypotheses, Gate: gate, Evidence: evidence, Recovered: true,
	}, nil
}

func (admitter *AgentStageResultAdmitter) admitFormalAgentTaskEvidence(
	ctx context.Context,
	subject AgentPlanningSubject,
	result contractsv1alpha1.StageExecutionResult,
	plan contractsv1alpha1.AgentStagePlan,
	request contractsv1alpha1.StageExecutionRequest,
) error {
	if result.AgentTaskEvidence == nil {
		return nil
	}
	binding := *result.AgentTaskEvidence
	if binding.Ref.SizeBytes <= 0 || binding.Ref.SizeBytes > maxFormalAgentTaskEvidenceBytes {
		return fmt.Errorf("formal agent task evidence exceeds the host admission byte limit")
	}
	data, err := admitter.repository.VerifyRead(ctx, subject, binding)
	if err != nil {
		return fmt.Errorf("resolve governed formal agent task evidence: %w", err)
	}
	if int64(len(data)) != binding.Ref.SizeBytes {
		return fmt.Errorf("resolved formal agent task evidence violates its exact size")
	}
	collection, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(data)
	if err != nil {
		return fmt.Errorf("strictly decode formal agent task evidence: %w", err)
	}
	if collection.PlanID != plan.PlanID || collection.SourceRunID != plan.ReviewRunID ||
		collection.ExecutionID != request.ExecutionID || collection.ReviewRunID != plan.ReviewRunID ||
		collection.TargetDigest != plan.TargetDigest {
		return fmt.Errorf("formal agent task evidence escaped the admitted plan, execution, run, or target")
	}
	local, err := admitter.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractAgentReviewTaskEvidence,
		data,
	)
	if err != nil {
		return fmt.Errorf("persist exact formal agent task evidence: %w", err)
	}
	if local.SHA256 != binding.Ref.SHA256 || local.SizeBytes != binding.Ref.SizeBytes ||
		local.Contract != binding.Contract {
		return fmt.Errorf("local and governed agent task evidence do not identify exact same bytes")
	}
	return nil
}

func (admitter *AgentStageResultAdmitter) admitFormalAgentRawCandidates(
	ctx context.Context,
	subject AgentPlanningSubject,
	result contractsv1alpha1.StageExecutionResult,
	plan contractsv1alpha1.AgentStagePlan,
	request contractsv1alpha1.StageExecutionRequest,
	hypotheses contractsv1alpha1.ReviewHypothesisSet,
) error {
	if result.AgentRawCandidates == nil {
		return nil
	}
	binding := *result.AgentRawCandidates
	if binding.Ref.SizeBytes <= 0 || binding.Ref.SizeBytes > maxFormalAgentRawCandidateBytes {
		return fmt.Errorf("formal raw candidate evidence exceeds the host admission byte limit")
	}
	data, err := admitter.repository.VerifyRead(ctx, subject, binding)
	if err != nil {
		return fmt.Errorf("resolve governed formal raw candidate evidence: %w", err)
	}
	if int64(len(data)) != binding.Ref.SizeBytes {
		return fmt.Errorf("resolved formal raw candidate evidence violates its exact size")
	}
	collection, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(data)
	if err != nil {
		return fmt.Errorf("strictly decode formal raw candidate evidence: %w", err)
	}
	if collection.PlanID != plan.PlanID || collection.SourceRunID != plan.ReviewRunID ||
		collection.ExecutionID != request.ExecutionID || collection.ReviewRunID != plan.ReviewRunID ||
		collection.TargetDigest != plan.TargetDigest {
		return fmt.Errorf("formal raw candidate evidence escaped the admitted plan, execution, run, or target")
	}
	reviewDimensions := make([]contractsv1alpha1.VersionedRef, 0)
	for _, skill := range plan.Skills {
		if skill.Phase == contractsv1alpha1.AgentStageSkillPhaseReview {
			reviewDimensions = append(reviewDimensions, skill.Ref)
		}
	}
	if err := contractsv1alpha1.ValidateAgentReviewRawCandidateSetBindings(
		collection, hypotheses, reviewDimensions,
	); err != nil {
		return fmt.Errorf("bind formal raw candidate evidence: %w", err)
	}
	local, err := admitter.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractAgentReviewRawCandidates,
		data,
	)
	if err != nil {
		return fmt.Errorf("persist exact formal raw candidate evidence: %w", err)
	}
	if local.SHA256 != binding.Ref.SHA256 || local.SizeBytes != binding.Ref.SizeBytes ||
		local.Contract != binding.Contract {
		return fmt.Errorf("local and governed raw candidate evidence do not identify exact same bytes")
	}
	return nil
}

func (admitter *AgentStageResultAdmitter) verifyLocalFormalAgentRawCandidates(
	ctx context.Context,
	result contractsv1alpha1.StageExecutionResult,
) error {
	if result.AgentRawCandidates == nil {
		return nil
	}
	binding := *result.AgentRawCandidates
	local := runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + binding.Ref.SHA256,
		SHA256: binding.Ref.SHA256, SizeBytes: binding.Ref.SizeBytes,
		Contract: binding.Contract,
	}
	data, err := admitter.repository.ReadLocalArtifact(ctx, local)
	if err != nil {
		return fmt.Errorf("read terminal-local formal raw candidate evidence: %w", err)
	}
	if int64(len(data)) != binding.Ref.SizeBytes {
		return fmt.Errorf("terminal-local formal raw candidate evidence violates its exact size")
	}
	if _, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(data); err != nil {
		return fmt.Errorf("strictly decode terminal-local formal raw candidate evidence: %w", err)
	}
	return nil
}

func (admitter *AgentStageResultAdmitter) verifyLocalFormalAgentTaskEvidence(
	ctx context.Context,
	result contractsv1alpha1.StageExecutionResult,
) error {
	if result.AgentTaskEvidence == nil {
		return nil
	}
	binding := *result.AgentTaskEvidence
	local := runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + binding.Ref.SHA256,
		SHA256: binding.Ref.SHA256, SizeBytes: binding.Ref.SizeBytes,
		Contract: binding.Contract,
	}
	data, err := admitter.repository.ReadLocalArtifact(ctx, local)
	if err != nil {
		return fmt.Errorf("read terminal-local formal agent task evidence: %w", err)
	}
	if int64(len(data)) != binding.Ref.SizeBytes {
		return fmt.Errorf("terminal-local formal agent task evidence violates its exact size")
	}
	if _, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(data); err != nil {
		return fmt.Errorf("strictly decode terminal-local formal agent task evidence: %w", err)
	}
	return nil
}

func (admitter *AgentStageResultAdmitter) admitFormalAgentExecutionReceipts(
	ctx context.Context,
	subject AgentPlanningSubject,
	result contractsv1alpha1.StageExecutionResult,
	plan contractsv1alpha1.AgentStagePlan,
	request contractsv1alpha1.StageExecutionRequest,
) error {
	if result.AgentExecutionReceipts == nil {
		return nil
	}
	binding := *result.AgentExecutionReceipts
	if binding.Ref.SizeBytes <= 0 || binding.Ref.SizeBytes > maxFormalAgentExecutionReceiptBytes {
		return fmt.Errorf("formal agent execution receipts exceed the host admission byte limit")
	}
	data, err := admitter.repository.VerifyRead(ctx, subject, binding)
	if err != nil {
		return fmt.Errorf("resolve governed formal agent execution receipts: %w", err)
	}
	if int64(len(data)) != binding.Ref.SizeBytes {
		return fmt.Errorf("resolved formal agent execution receipts violate their exact size")
	}
	collection, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(data)
	if err != nil {
		return fmt.Errorf("strictly decode formal agent execution receipts: %w", err)
	}
	if collection.PlanID != plan.PlanID || collection.SourceRunID != plan.ReviewRunID ||
		collection.ExecutionID != request.ExecutionID || collection.ReviewRunID != plan.ReviewRunID {
		return fmt.Errorf("formal agent execution receipts escaped the admitted plan, execution, or run")
	}
	local, err := admitter.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractAgentExecutionReceipts,
		data,
	)
	if err != nil {
		return fmt.Errorf("persist exact formal agent execution receipts: %w", err)
	}
	if local.SHA256 != binding.Ref.SHA256 || local.SizeBytes != binding.Ref.SizeBytes ||
		local.Contract != binding.Contract {
		return fmt.Errorf("local and governed agent execution receipts do not identify exact same bytes")
	}
	return nil
}

func (admitter *AgentStageResultAdmitter) verifyLocalFormalAgentExecutionReceipts(
	ctx context.Context,
	result contractsv1alpha1.StageExecutionResult,
) error {
	if result.AgentExecutionReceipts == nil {
		return nil
	}
	binding := *result.AgentExecutionReceipts
	local := runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + binding.Ref.SHA256,
		SHA256: binding.Ref.SHA256, SizeBytes: binding.Ref.SizeBytes,
		Contract: binding.Contract,
	}
	data, err := admitter.repository.ReadLocalArtifact(ctx, local)
	if err != nil {
		return fmt.Errorf("read terminal-local formal agent execution receipts: %w", err)
	}
	if int64(len(data)) != binding.Ref.SizeBytes {
		return fmt.Errorf("terminal-local formal agent execution receipts violate their exact size")
	}
	if _, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(data); err != nil {
		return fmt.Errorf("strictly decode terminal-local formal agent execution receipts: %w", err)
	}
	return nil
}

func (admitter *AgentStageResultAdmitter) commitAgentStageHypothesisEvidence(
	ctx context.Context,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	gate runmodel.AgentStageTerminalGate,
	hypotheses contractsv1alpha1.ReviewHypothesisSet,
) (runmodel.AgentStageHypothesisEvidence, error) {
	if gate.Kind != runmodel.AgentStageSucceededResultAccepted || gate.Completion == nil {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"formal hypothesis evidence requires an accepted terminal completion",
		)
	}
	evidence, err := runmodel.SealAgentStageHypothesisEvidence(
		runmodel.AgentStageHypothesisEvidence{
			SchemaVersion:         runmodel.AgentStageHypothesisEvidenceSchemaVersion,
			Subject:               admission.Subject,
			ReviewRunID:           admission.ReviewRunID,
			Stage:                 admission.Stage,
			AdmissionID:           admission.AdmissionID,
			AdmissionSHA256:       admission.SHA256,
			IntentID:              intent.IntentID,
			IntentSHA256:          intent.SHA256,
			BindingID:             binding.BindingID,
			BindingSHA256:         binding.SHA256,
			TerminalGateID:        gate.GateID,
			TerminalGateSHA256:    gate.SHA256,
			RequestRef:            intent.RequestRef,
			RequestSemanticSHA256: intent.RequestSemanticSHA256,
			ResultRef:             gate.Completion.ResultRef,
			Output:                gate.Completion.Output,
			PlanID:                admission.PlanID,
			PlanSemanticSHA256:    admission.PlanSemanticSHA256,
			TargetDigest:          hypotheses.TargetDigest,
			HypothesisSetID:       hypotheses.HypothesisSetID,
			AdmittedAt:            gate.Completion.AcceptedAt,
		},
	)
	if err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"seal formal hypothesis evidence: %w",
			err,
		)
	}
	return admitter.appendOrResolveAgentStageHypothesisEvidence(ctx, evidence)
}

func sealAgentStageResultTerminalGate(
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	request contractsv1alpha1.StageExecutionRequest,
	result contractsv1alpha1.StageExecutionResult,
	resultRef runmodel.ArtifactRef,
	output runmodel.AgentArtifactProjection,
	traceManifest *runmodel.AgentArtifactProjection,
	callbackReceiptRef runmodel.ArtifactRef,
	callbackReceiptSHA256 string,
	acceptedAt time.Time,
) (runmodel.AgentStageTerminalGate, error) {
	return runmodel.SealAgentStageTerminalGate(runmodel.AgentStageTerminalGate{
		SchemaVersion:         runmodel.AgentStageTerminalGateSchemaVersion,
		Kind:                  runmodel.AgentStageSucceededResultAccepted,
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
		RequestDeadline:       request.Deadline,
		CapabilitySHA256:      intent.CapabilitySHA256,
		Completion: &runmodel.AgentStageSucceededResultAcceptance{
			BindingID:             binding.BindingID,
			BindingSHA256:         binding.SHA256,
			ResultRef:             resultRef,
			Output:                output,
			TraceManifest:         traceManifest,
			CallbackReceiptRef:    callbackReceiptRef,
			CallbackReceiptSHA256: callbackReceiptSHA256,
			Status:                string(result.Status),
			Completeness:          result.Completeness,
			CompletenessNotes: append(
				make([]string, 0, len(result.CompletenessNotes)),
				result.CompletenessNotes...,
			),
			ProviderResultRecordedAt: result.RecordedAt,
			AcceptedAt:               acceptedAt,
		},
	})
}

// persistFreshAgentStageCallbackReceipt is deliberately the first fallible
// host work after successful callback authentication. The verifier contract is
// replay-idempotent for the exact tuple, while this method turns that decision
// into a discoverable local fact before output/plan/trace semantic processing.
func (admitter *AgentStageResultAdmitter) persistFreshAgentStageCallbackReceipt(
	ctx context.Context,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	resultRef runmodel.ArtifactRef,
	canonicalResult []byte,
	callbackProof []byte,
	verifier runmodel.AgentStageResultCallbackVerifierRef,
	verifiedAt time.Time,
) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef, error) {
	persistedResultRef, err := admitter.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractStageExecutionResult,
		canonicalResult,
	)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"persist canonical StageExecutionResult for callback receipt: %w",
			err,
		)
	}
	if persistedResultRef != resultRef {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"StageExecutionResult repository returned a mismatched ref",
		)
	}
	providerHandleSHA256, err := runmodel.AgentStageProviderHandleSHA256(
		binding.ProviderHandle,
	)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"digest exact provider execution handle: %w",
			err,
		)
	}
	proofSHA256, err := runmodel.AgentStageResultCallbackProofSHA256(callbackProof)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"digest provider callback proof: %w",
			err,
		)
	}
	receipt, err := runmodel.SealAgentStageResultCallbackReceipt(
		runmodel.AgentStageResultCallbackReceipt{
			SchemaVersion:        runmodel.AgentStageResultCallbackReceiptSchemaVersion,
			Subject:              intent.Subject,
			ReviewRunID:          intent.ReviewRunID,
			Stage:                intent.Stage,
			BindingID:            binding.BindingID,
			BindingSHA256:        binding.SHA256,
			ProviderHandleSHA256: providerHandleSHA256,
			ResultRef:            resultRef,
			ProofSHA256:          proofSHA256,
			Verifier:             verifier,
			VerifiedAt:           verifiedAt,
		},
	)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"seal authenticated provider callback receipt: %w",
			err,
		)
	}
	if err := receipt.ValidateAgainstBindingAndResult(binding, resultRef); err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"close provider callback receipt over exact binding and result: %w",
			err,
		)
	}
	receiptBytes, err := json.Marshal(receipt)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"marshal authenticated provider callback receipt: %w",
			err,
		)
	}
	receiptRef, err := admitter.repository.PutLocalArtifact(
		ctx,
		runmodel.ContractAgentStageResultCallbackReceipt,
		receiptBytes,
	)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"persist authenticated provider callback receipt: %w",
			err,
		)
	}
	if receiptRef != localRefForBytes(
		runmodel.ContractAgentStageResultCallbackReceipt,
		receiptBytes,
	) {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
			"provider callback receipt repository returned a mismatched ref",
		)
	}
	return admitter.appendOrResolveAgentStageCallbackReceipt(ctx, receipt, receiptRef)
}

func (admitter *AgentStageResultAdmitter) validateRecoveredAgentStageCallbackReceipt(
	ctx context.Context,
	receipt runmodel.AgentStageResultCallbackReceipt,
	receiptRef runmodel.ArtifactRef,
	binding runmodel.AgentStageExecutionBinding,
	resultRef runmodel.ArtifactRef,
	providerResultRecordedAt time.Time,
	trust contractsv1alpha1.ExecutorTrust,
) error {
	if err := validateCallbackVerifierAgainstExecutorTrust(receipt.Verifier, trust); err != nil {
		return fmt.Errorf("validate recovered provider callback trust: %w", err)
	}
	if err := receipt.ValidateAgainstBindingAndResult(binding, resultRef); err != nil {
		return fmt.Errorf("validate recovered provider callback receipt: %w", err)
	}
	if receipt.VerifiedAt.Before(providerResultRecordedAt) {
		return fmt.Errorf("recovered provider callback receipt predates provider result")
	}
	receiptBytes, err := admitter.repository.ReadLocalArtifact(ctx, receiptRef)
	if err != nil {
		return fmt.Errorf("read recovered provider callback receipt: %w", err)
	}
	if localRefForBytes(runmodel.ContractAgentStageResultCallbackReceipt, receiptBytes) !=
		receiptRef {
		return fmt.Errorf("recovered provider callback receipt bytes do not match local ref")
	}
	persisted, err := runmodel.DecodeAgentStageResultCallbackReceipt(receiptBytes)
	if err != nil {
		return fmt.Errorf("strictly decode recovered provider callback receipt: %w", err)
	}
	if persisted != receipt {
		return fmt.Errorf("recovered provider callback receipt differs from registered bytes")
	}
	return nil
}

func validateCallbackVerifierAgainstExecutorTrust(
	verifier runmodel.AgentStageResultCallbackVerifierRef,
	trust contractsv1alpha1.ExecutorTrust,
) error {
	if err := trust.Validate(); err != nil {
		return fmt.Errorf("validate frozen executor trust: %w", err)
	}
	if verifier.ID != trust.CallbackVerifier.ID ||
		verifier.Revision != trust.CallbackVerifier.Revision ||
		verifier.SHA256 != trust.CallbackVerifier.SHA256 {
		return fmt.Errorf("provider callback verifier differs from frozen request trust root")
	}
	return nil
}

func (admitter *AgentStageResultAdmitter) loadBoundAgentStageRequest(
	ctx context.Context,
	intent runmodel.AgentStageDispatchIntent,
	result contractsv1alpha1.StageExecutionResult,
) (contractsv1alpha1.StageExecutionRequest, error) {
	requestBytes, err := admitter.repository.ReadLocalArtifact(ctx, intent.RequestRef)
	if err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"read exact formal StageExecutionRequest: %w",
			err,
		)
	}
	if localRefForBytes(runmodel.ContractStageExecutionRequest, requestBytes) != intent.RequestRef {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"formal StageExecutionRequest bytes do not match dispatch intent ref",
		)
	}
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(requestBytes)
	if err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"strictly decode formal StageExecutionRequest: %w",
			err,
		)
	}
	if err := contractsv1alpha1.ValidateStageExecutionResultBinding(request, result); err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"bind formal StageExecutionResult to exact request: %w",
			err,
		)
	}
	return request, nil
}

func (admitter *AgentStageResultAdmitter) validateAcceptedAgentStageCallbackReceipt(
	ctx context.Context,
	gate runmodel.AgentStageTerminalGate,
	binding runmodel.AgentStageExecutionBinding,
	resultRef runmodel.ArtifactRef,
	providerResultRecordedAt time.Time,
	trust contractsv1alpha1.ExecutorTrust,
) error {
	if gate.Completion == nil {
		return fmt.Errorf("accepted terminal winner has no completion")
	}
	receipt, receiptRef, found, err := admitter.repository.LookupAgentStageResultCallbackReceipt(
		ctx,
		gate.ReviewRunID,
		binding.BindingID,
		gate.Subject,
	)
	if err != nil {
		return fmt.Errorf("lookup accepted terminal callback receipt: %w", err)
	}
	if !found {
		return fmt.Errorf("accepted terminal callback receipt does not exist")
	}
	if receiptRef != gate.Completion.CallbackReceiptRef ||
		receipt.SHA256 != gate.Completion.CallbackReceiptSHA256 {
		return fmt.Errorf("accepted terminal callback receipt differs from terminal winner")
	}
	return admitter.validateRecoveredAgentStageCallbackReceipt(
		ctx,
		receipt,
		receiptRef,
		binding,
		resultRef,
		providerResultRecordedAt,
		trust,
	)
}

func (admitter *AgentStageResultAdmitter) appendOrResolveAgentStageCallbackReceipt(
	ctx context.Context,
	receipt runmodel.AgentStageResultCallbackReceipt,
	receiptRef runmodel.ArtifactRef,
) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef, error) {
	if err := admitter.repository.AppendAgentStageResultCallbackReceipt(
		ctx,
		receipt,
		receiptRef,
	); err == nil {
		return receipt, receiptRef, nil
	} else {
		lookupContext, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			formalAgentAppendRecoveryTimeout,
		)
		defer cancel()
		winner, winnerRef, found, lookupErr :=
			admitter.repository.LookupAgentStageResultCallbackReceipt(
				lookupContext,
				receipt.ReviewRunID,
				receipt.BindingID,
				receipt.Subject,
			)
		if lookupErr != nil {
			return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
				"%w: append provider callback receipt: %v; reload winner: %v",
				ErrAgentStageCallbackReceiptOutcomeUnknown,
				err,
				lookupErr,
			)
		}
		if !found {
			return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
				"%w: append provider callback receipt: %v; no durable winner found",
				ErrAgentStageCallbackReceiptOutcomeUnknown,
				err,
			)
		}
		if !sameAgentStageCallbackDecision(winner, receipt) {
			return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
				"changed provider callback receipt conflicts with immutable authority",
			)
		}
		// Concurrent exact callbacks may be observed at different host times and
		// therefore produce different content refs. The first durable receipt is
		// authoritative when the authenticated decision tuple is otherwise exact.
		// Return its ref so every subsequent terminal gate closes over one fact.
		return winner, winnerRef, nil
	}
}

func sameAgentStageCallbackDecision(
	left runmodel.AgentStageResultCallbackReceipt,
	right runmodel.AgentStageResultCallbackReceipt,
) bool {
	if err := left.Validate(); err != nil {
		return false
	}
	if err := right.Validate(); err != nil {
		return false
	}
	left.SHA256 = ""
	right.SHA256 = ""
	left.VerifiedAt = time.Time{}
	right.VerifiedAt = time.Time{}
	return left == right
}

func (admitter *AgentStageResultAdmitter) appendOrResolveAgentStageResultGate(
	ctx context.Context,
	gate runmodel.AgentStageTerminalGate,
) (runmodel.AgentStageTerminalGate, error) {
	if err := admitter.repository.AppendAgentStageTerminalGate(ctx, gate); err == nil {
		return gate, nil
	} else {
		lookupContext, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			formalAgentAppendRecoveryTimeout,
		)
		defer cancel()
		winner, found, lookupErr := admitter.repository.LookupAgentStageTerminalGate(
			lookupContext,
			gate.ReviewRunID,
			gate.IntentID,
			gate.Subject,
		)
		if lookupErr != nil {
			return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
				"%w: append formal result terminal gate: %v; reload winner: %v",
				ErrAgentStageResultTerminalOutcomeUnknown,
				err,
				lookupErr,
			)
		}
		if !found {
			return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
				"%w: append formal result terminal gate: %v; no durable winner found",
				ErrAgentStageResultTerminalOutcomeUnknown,
				err,
			)
		}
		if winner.Kind == runmodel.AgentStageCancellationRequested {
			return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
				"%w: cancellation %q is authoritative",
				ErrAgentStageResultLostTerminalRace,
				winner.GateID,
			)
		}
		if !sameAgentStageTerminalResultDecisionIgnoringAcceptedAt(winner, gate) {
			return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
				"changed formal result conflicts with terminal winner %q",
				winner.GateID,
			)
		}
		return winner, nil
	}
}

func sameAgentStageTerminalResultDecisionIgnoringAcceptedAt(
	left runmodel.AgentStageTerminalGate,
	right runmodel.AgentStageTerminalGate,
) bool {
	switch left.Kind {
	case runmodel.AgentStageSucceededResultAccepted:
		return sameAgentStageTerminalCompletionIgnoringAcceptedAt(left, right)
	case runmodel.AgentStageFailedResultAccepted, runmodel.AgentStageCanceledResultAccepted:
		return sameAgentStageTerminalOutcomeIgnoringAcceptedAt(left, right)
	default:
		return false
	}
}

func sameAgentStageTerminalOutcomeIgnoringAcceptedAt(
	left runmodel.AgentStageTerminalGate,
	right runmodel.AgentStageTerminalGate,
) bool {
	if left.Kind != right.Kind ||
		(left.Kind != runmodel.AgentStageFailedResultAccepted &&
			left.Kind != runmodel.AgentStageCanceledResultAccepted) ||
		left.Outcome == nil || right.Outcome == nil {
		return false
	}
	leftOutcome := left.Outcome
	rightOutcome := right.Outcome
	return left.Subject == right.Subject && left.ReviewRunID == right.ReviewRunID &&
		left.Stage == right.Stage && left.AdmissionID == right.AdmissionID &&
		left.AdmissionSHA256 == right.AdmissionSHA256 && left.IntentID == right.IntentID &&
		left.IntentSHA256 == right.IntentSHA256 && left.WorkloadID == right.WorkloadID &&
		left.LeaseID == right.LeaseID && left.LeaseWorker == right.LeaseWorker &&
		left.ExecutionID == right.ExecutionID && left.Attempt == right.Attempt &&
		left.Generation == right.Generation && left.FencingToken == right.FencingToken &&
		left.RequestRef == right.RequestRef &&
		left.RequestSemanticSHA256 == right.RequestSemanticSHA256 &&
		left.RequestDeadline.Equal(right.RequestDeadline) &&
		left.CapabilitySHA256 == right.CapabilitySHA256 &&
		leftOutcome.BindingID == rightOutcome.BindingID &&
		leftOutcome.BindingSHA256 == rightOutcome.BindingSHA256 &&
		leftOutcome.ResultRef == rightOutcome.ResultRef &&
		leftOutcome.CallbackReceiptRef == rightOutcome.CallbackReceiptRef &&
		leftOutcome.CallbackReceiptSHA256 == rightOutcome.CallbackReceiptSHA256 &&
		agentStageTraceProjectionsEqual(leftOutcome.TraceManifest, rightOutcome.TraceManifest) &&
		agentStageTraceProjectionsEqual(leftOutcome.AgentTaskEvidence, rightOutcome.AgentTaskEvidence) &&
		agentStageTraceProjectionsEqual(leftOutcome.AgentExecutionReceipts, rightOutcome.AgentExecutionReceipts) &&
		leftOutcome.Status == rightOutcome.Status && leftOutcome.Failure == rightOutcome.Failure &&
		leftOutcome.Completeness == rightOutcome.Completeness &&
		slices.Equal(leftOutcome.CompletenessNotes, rightOutcome.CompletenessNotes) &&
		leftOutcome.ProviderResultRecordedAt.Equal(rightOutcome.ProviderResultRecordedAt)
}

func (admitter *AgentStageResultAdmitter) appendOrResolveAgentStageHypothesisEvidence(
	ctx context.Context,
	evidence runmodel.AgentStageHypothesisEvidence,
) (runmodel.AgentStageHypothesisEvidence, error) {
	if err := admitter.repository.AppendAgentStageHypothesisEvidence(ctx, evidence); err == nil {
		return evidence, nil
	} else {
		lookupContext, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			formalAgentAppendRecoveryTimeout,
		)
		defer cancel()
		winner, found, lookupErr := admitter.repository.LookupAgentStageHypothesisEvidence(
			lookupContext,
			evidence.ReviewRunID,
			evidence.BindingID,
			evidence.Subject,
		)
		if lookupErr != nil {
			return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
				"%w: append formal hypothesis evidence: %v; reload winner: %v",
				ErrAgentStageEvidenceOutcomeUnknown,
				err,
				lookupErr,
			)
		}
		if !found {
			return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
				"%w: append formal hypothesis evidence: %v; no durable winner found",
				ErrAgentStageEvidenceOutcomeUnknown,
				err,
			)
		}
		if winner != evidence {
			return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
				"changed formal hypothesis evidence conflicts with its immutable authority",
			)
		}
		return winner, nil
	}
}

func sameAgentStageAcceptedResult(
	gate runmodel.AgentStageTerminalGate,
	binding runmodel.AgentStageExecutionBinding,
	result contractsv1alpha1.StageExecutionResult,
	resultRef runmodel.ArtifactRef,
	output runmodel.AgentArtifactProjection,
) bool {
	return gate.Kind == runmodel.AgentStageSucceededResultAccepted && gate.Completion != nil &&
		result.Output != nil &&
		gate.Completion.BindingID == binding.BindingID &&
		gate.Completion.BindingSHA256 == binding.SHA256 &&
		gate.Completion.ResultRef == resultRef && gate.Completion.Output == output &&
		gate.Completion.Output.Governed.URI == result.Output.Ref.URI &&
		gate.Completion.Output.Governed.SHA256 == result.Output.Ref.SHA256 &&
		gate.Completion.Output.Governed.SizeBytes == result.Output.Ref.SizeBytes &&
		gate.Completion.Output.Governed.Contract == result.Output.Contract &&
		gate.Completion.Status == string(result.Status) &&
		gate.Completion.Completeness == result.Completeness &&
		slices.Equal(gate.Completion.CompletenessNotes, result.CompletenessNotes) &&
		gate.Completion.ProviderResultRecordedAt.Equal(result.RecordedAt) &&
		agentStageTraceMatchesResult(gate.Completion.TraceManifest, result.TraceManifest)
}

func sameAgentStageTerminalCompletionIgnoringAcceptedAt(
	left runmodel.AgentStageTerminalGate,
	right runmodel.AgentStageTerminalGate,
) bool {
	if left.Kind != runmodel.AgentStageSucceededResultAccepted ||
		right.Kind != runmodel.AgentStageSucceededResultAccepted ||
		left.Completion == nil || right.Completion == nil {
		return false
	}
	leftCompletion := left.Completion
	rightCompletion := right.Completion
	return left.Subject == right.Subject && left.ReviewRunID == right.ReviewRunID &&
		left.Stage == right.Stage && left.AdmissionID == right.AdmissionID &&
		left.AdmissionSHA256 == right.AdmissionSHA256 && left.IntentID == right.IntentID &&
		left.IntentSHA256 == right.IntentSHA256 && left.WorkloadID == right.WorkloadID &&
		left.LeaseID == right.LeaseID && left.LeaseWorker == right.LeaseWorker &&
		left.ExecutionID == right.ExecutionID && left.Attempt == right.Attempt &&
		left.Generation == right.Generation && left.FencingToken == right.FencingToken &&
		left.RequestRef == right.RequestRef &&
		left.RequestSemanticSHA256 == right.RequestSemanticSHA256 &&
		left.RequestDeadline.Equal(right.RequestDeadline) &&
		left.CapabilitySHA256 == right.CapabilitySHA256 &&
		leftCompletion.BindingID == rightCompletion.BindingID &&
		leftCompletion.BindingSHA256 == rightCompletion.BindingSHA256 &&
		leftCompletion.ResultRef == rightCompletion.ResultRef &&
		leftCompletion.Output == rightCompletion.Output &&
		leftCompletion.CallbackReceiptRef == rightCompletion.CallbackReceiptRef &&
		leftCompletion.CallbackReceiptSHA256 == rightCompletion.CallbackReceiptSHA256 &&
		agentStageTraceProjectionsEqual(
			leftCompletion.TraceManifest,
			rightCompletion.TraceManifest,
		) &&
		leftCompletion.Status == rightCompletion.Status &&
		leftCompletion.Completeness == rightCompletion.Completeness &&
		slices.Equal(leftCompletion.CompletenessNotes, rightCompletion.CompletenessNotes) &&
		leftCompletion.ProviderResultRecordedAt.Equal(rightCompletion.ProviderResultRecordedAt)
}

func agentStageTraceMatchesResult(
	projection *runmodel.AgentArtifactProjection,
	binding *contractsv1alpha1.ArtifactBinding,
) bool {
	if projection == nil || binding == nil {
		return projection == nil && binding == nil
	}
	return projection.Governed.URI == binding.Ref.URI &&
		projection.Governed.SHA256 == binding.Ref.SHA256 &&
		projection.Governed.SizeBytes == binding.Ref.SizeBytes &&
		projection.Governed.Contract == binding.Contract
}

func agentStageTraceProjectionsEqual(
	left *runmodel.AgentArtifactProjection,
	right *runmodel.AgentArtifactProjection,
) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
