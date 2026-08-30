package runrepo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var (
	ErrAgentStageResultCallbackReceiptConflict        = local.ErrEventConflict
	ErrAgentStageResultCallbackReceiptSubjectMismatch = errors.New(
		"agent-stage result callback receipt belongs to another planning subject",
	)
	ErrAgentStageTerminalGateConflict        = local.ErrEventConflict
	ErrAgentStageTerminalGateSubjectMismatch = errors.New(
		"agent-stage terminal gate belongs to another planning subject",
	)
)

const agentStageResultCallbackReceiptRecordSchemaVersion = "argus.agent_stage_result_callback_receipt_record.v1alpha1"

// agentStageResultCallbackReceiptRecord makes a proof-redacted callback
// verification discoverable after a crash. The receipt itself is semantic
// evidence; ReceiptRef pins the exact canonical local bytes that a later
// terminal completion must reuse.
type agentStageResultCallbackReceiptRecord struct {
	SchemaVersion string                                   `json:"schema_version"`
	Receipt       runmodel.AgentStageResultCallbackReceipt `json:"receipt"`
	ReceiptRef    runmodel.ArtifactRef                     `json:"receipt_artifact_ref"`
}

// AppendAgentStageResultCallbackReceipt durably registers the first verified
// callback for an execution binding. The deterministic ReceiptID makes an
// exact retry idempotent and a different result, proof, verifier, timestamp,
// or artifact reference conflict. This receipt does not decide the terminal
// result/cancel race; only AgentStageTerminalGate does that.
func (repository *Repository) AppendAgentStageResultCallbackReceipt(
	receipt runmodel.AgentStageResultCallbackReceipt,
	receiptRef runmodel.ArtifactRef,
) error {
	record := agentStageResultCallbackReceiptRecord{
		SchemaVersion: agentStageResultCallbackReceiptRecordSchemaVersion,
		Receipt:       receipt,
		ReceiptRef:    receiptRef,
	}
	if err := validateAgentStageResultCallbackReceiptRecord(record); err != nil {
		return fmt.Errorf("validate callback receipt record: %w", err)
	}
	binding, err := repository.LoadAgentStageExecutionBinding(
		receipt.ReviewRunID,
		receipt.BindingID,
		receipt.Subject,
	)
	if err != nil {
		return fmt.Errorf("load execution binding for callback receipt: %w", err)
	}
	if _, err := repository.verifyAgentStageResultCallbackReceiptRecord(
		record,
		binding,
	); err != nil {
		return err
	}
	stream, err := agentStageResultCallbackReceiptStream(receipt.ReviewRunID)
	if err != nil {
		return err
	}
	if _, err := repository.readAgentStageResultCallbackReceipts(
		receipt.ReviewRunID,
	); err != nil {
		return err
	}
	envelope, err := repository.store.AppendJSONL(stream, local.Event{
		ID:      receipt.ReceiptID,
		Schema:  agentStageResultCallbackReceiptRecordSchemaVersion,
		Time:    receipt.VerifiedAt,
		Payload: record,
	})
	if err != nil {
		return fmt.Errorf("append authoritative callback receipt record: %w", err)
	}
	persisted, err := decodeAgentStageResultCallbackReceiptRecord(
		envelope,
		receipt.ReviewRunID,
	)
	if err != nil {
		return fmt.Errorf("verify appended callback receipt record: %w", err)
	}
	if persisted != record {
		return fmt.Errorf("appended callback receipt record differs from request")
	}
	return nil
}

// LookupAgentStageResultCallbackReceipt recovers the sole verified callback
// record for one execution binding. It returns both the semantic receipt and
// its exact local artifact reference so recovery can seal a terminal gate
// without consuming or re-verifying one-time proof material.
func (repository *Repository) LookupAgentStageResultCallbackReceipt(
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (
	runmodel.AgentStageResultCallbackReceipt,
	runmodel.ArtifactRef,
	bool,
	error,
) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false,
			fmt.Errorf("validate planning subject: %w", err)
	}
	receiptID, err := runmodel.AgentStageResultCallbackReceiptID(runID, bindingID)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false, err
	}
	receipt, ref, err := repository.LoadAgentStageResultCallbackReceipt(
		runID,
		receiptID,
		subject,
	)
	if errors.Is(err, os.ErrNotExist) {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false, nil
	}
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false, err
	}
	if receipt.BindingID != bindingID {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false,
			fmt.Errorf("callback receipt ID resolved a different execution binding")
	}
	return receipt, ref, true, nil
}

func (repository *Repository) LoadAgentStageResultCallbackReceipt(
	runID string,
	receiptID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{},
			fmt.Errorf("validate planning subject: %w", err)
	}
	if !strings.HasPrefix(receiptID, runID+"-") {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{},
			fmt.Errorf(
				"callback receipt %q is not namespaced by run %q",
				receiptID,
				runID,
			)
	}
	records, err := repository.readAgentStageResultCallbackReceipts(runID)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, err
	}
	for _, record := range records {
		if record.Receipt.ReceiptID != receiptID {
			continue
		}
		if record.Receipt.Subject != subject {
			return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{},
				fmt.Errorf(
					"%w: run %q stage %q binding %q",
					ErrAgentStageResultCallbackReceiptSubjectMismatch,
					record.Receipt.ReviewRunID,
					record.Receipt.Stage.ID,
					record.Receipt.BindingID,
				)
		}
		return record.Receipt, record.ReceiptRef, nil
	}
	return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, fmt.Errorf(
		"callback receipt %q in run %q: %w",
		receiptID,
		runID,
		os.ErrNotExist,
	)
}

func (repository *Repository) readAgentStageResultCallbackReceipts(
	runID string,
) ([]agentStageResultCallbackReceiptRecord, error) {
	stream, err := agentStageResultCallbackReceiptStream(runID)
	if err != nil {
		return nil, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []agentStageResultCallbackReceiptRecord{}, nil
		}
		return nil, fmt.Errorf("read callback receipt record stream: %w", err)
	}
	records := make([]agentStageResultCallbackReceiptRecord, 0, len(envelopes))
	for _, envelope := range envelopes {
		record, err := decodeAgentStageResultCallbackReceiptRecord(envelope, runID)
		if err != nil {
			return nil, err
		}
		binding, err := repository.LoadAgentStageExecutionBinding(
			runID,
			record.Receipt.BindingID,
			record.Receipt.Subject,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"load execution binding for persisted callback receipt %q: %w",
				record.Receipt.ReceiptID,
				err,
			)
		}
		if _, err := repository.verifyAgentStageResultCallbackReceiptRecord(
			record,
			binding,
		); err != nil {
			return nil, fmt.Errorf(
				"verify persisted callback receipt %q: %w",
				record.Receipt.ReceiptID,
				err,
			)
		}
		records = append(records, record)
	}
	return records, nil
}

func validateAgentStageResultCallbackReceiptRecord(
	record agentStageResultCallbackReceiptRecord,
) error {
	if record.SchemaVersion != agentStageResultCallbackReceiptRecordSchemaVersion {
		return fmt.Errorf("unsupported callback receipt record schema %q", record.SchemaVersion)
	}
	if err := record.Receipt.Validate(); err != nil {
		return fmt.Errorf("receipt: %w", err)
	}
	if err := record.ReceiptRef.Validate(); err != nil {
		return fmt.Errorf("receipt_artifact_ref: %w", err)
	}
	if record.ReceiptRef.SizeBytes <= 0 ||
		record.ReceiptRef.Contract != runmodel.ContractAgentStageResultCallbackReceipt {
		return fmt.Errorf(
			"receipt_artifact_ref must be a non-empty %q artifact",
			runmodel.ContractAgentStageResultCallbackReceipt,
		)
	}
	return nil
}

func (repository *Repository) verifyAgentStageResultCallbackReceiptRecord(
	record agentStageResultCallbackReceiptRecord,
	binding runmodel.AgentStageExecutionBinding,
) (contractsv1alpha1.StageExecutionResult, error) {
	if err := validateAgentStageResultCallbackReceiptRecord(record); err != nil {
		return contractsv1alpha1.StageExecutionResult{}, err
	}
	persistedReceipt, err := repository.readCanonicalAgentStageResultCallbackReceipt(
		record.ReceiptRef,
	)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, err
	}
	if persistedReceipt != record.Receipt {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"callback receipt artifact does not match registry record",
		)
	}
	if err := record.Receipt.ValidateAgainstBindingAndResult(
		binding,
		record.Receipt.ResultRef,
	); err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"bind callback receipt to exact execution binding and result: %w",
			err,
		)
	}
	intent, err := repository.LoadAgentStageDispatchIntent(
		binding.ReviewRunID,
		binding.IntentID,
		binding.Subject,
	)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"load dispatch intent for callback receipt: %w",
			err,
		)
	}
	admission, err := repository.LoadAgentStagePlanAdmission(
		intent.ReviewRunID,
		intent.AdmissionID,
		intent.Subject,
	)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"load plan admission for callback receipt: %w",
			err,
		)
	}
	request, err := repository.readCanonicalAgentStageExecutionRequest(intent.RequestRef)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"read exact StageExecutionRequest for callback receipt: %w",
			err,
		)
	}
	requestGate := runmodel.AgentStageTerminalGate{
		Subject:               intent.Subject,
		ReviewRunID:           intent.ReviewRunID,
		Stage:                 intent.Stage,
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
	}
	if err := validateAgentStageTerminalRequestBinding(
		requestGate,
		admission,
		intent,
		request,
	); err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"bind callback receipt request to exact intent: %w",
			err,
		)
	}
	result, err := repository.readCanonicalAgentStageExecutionResult(
		record.Receipt.ResultRef,
	)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"read exact StageExecutionResult for callback receipt: %w",
			err,
		)
	}
	if err := contractsv1alpha1.ValidateStageExecutionResultBinding(request, result); err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"bind callback receipt result to exact request: %w",
			err,
		)
	}
	if result.RecordedAt.Before(intent.RecordedAt) {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"callback result recorded_at predates dispatch intent recorded_at",
		)
	}
	if record.Receipt.VerifiedAt.Before(result.RecordedAt) {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"callback receipt verified_at predates provider result recorded_at",
		)
	}
	if record.Receipt.VerifiedAt.After(request.Deadline) {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"callback receipt verified_at is after request deadline",
		)
	}
	return result, nil
}

func (repository *Repository) readCanonicalAgentStageResultCallbackReceipt(
	ref runmodel.ArtifactRef,
) (runmodel.AgentStageResultCallbackReceipt, error) {
	if ref.Contract != runmodel.ContractAgentStageResultCallbackReceipt {
		return runmodel.AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"callback receipt artifact contract must be %q",
			runmodel.ContractAgentStageResultCallbackReceipt,
		)
	}
	data, err := repository.ReadArtifact(ref)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"read exact callback receipt artifact: %w",
			err,
		)
	}
	receipt, err := runmodel.DecodeAgentStageResultCallbackReceipt(data)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"strictly decode callback receipt: %w",
			err,
		)
	}
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"marshal canonical callback receipt: %w",
			err,
		)
	}
	if !bytes.Equal(data, canonical) {
		return runmodel.AgentStageResultCallbackReceipt{}, fmt.Errorf(
			"callback receipt artifact is not canonical JSON",
		)
	}
	return receipt, nil
}

// AppendAgentStageTerminalGate commits the first terminal authority for an
// exact dispatch intent. A succeeded result and a cancellation share one
// deterministic event ID: an exact retry is idempotent, while either opposite
// branch or changed content conflicts under the store's stream lock.
func (repository *Repository) AppendAgentStageTerminalGate(
	gate runmodel.AgentStageTerminalGate,
) error {
	if err := gate.Validate(); err != nil {
		return fmt.Errorf("validate AgentStageTerminalGate: %w", err)
	}
	if !strings.HasPrefix(gate.GateID, gate.ReviewRunID+"-") {
		return fmt.Errorf(
			"agent-stage terminal gate %q is not namespaced by run %q",
			gate.GateID,
			gate.ReviewRunID,
		)
	}
	if err := repository.validateAgentStageTerminalGateClosure(gate); err != nil {
		return err
	}
	// Dispatch claim and terminal admission advance the same generation stream.
	// Whichever CAS wins first fences the opposite stale transition across
	// processes before either side performs its downstream append/provider work.
	if err := repository.reserveAgentStageTerminalGate(gate); err != nil {
		return fmt.Errorf("reserve agent-stage terminal generation: %w", err)
	}
	stream, err := agentStageTerminalGateStream(gate.ReviewRunID)
	if err != nil {
		return err
	}
	// Revalidate all prior winners before extending the stream. AppendJSONL is
	// the authoritative first-writer decision across Repository instances.
	if _, err := repository.readAgentStageTerminalGates(gate.ReviewRunID); err != nil {
		return err
	}
	envelope, err := repository.store.AppendJSONL(stream, local.Event{
		ID:      gate.GateID,
		Schema:  runmodel.AgentStageTerminalGateSchemaVersion,
		Time:    gate.EventTime(),
		Payload: gate,
	})
	if err != nil {
		return fmt.Errorf("append authoritative agent-stage terminal gate: %w", err)
	}
	persisted, err := decodeAgentStageTerminalGate(envelope, gate.ReviewRunID)
	if err != nil {
		return fmt.Errorf("verify appended agent-stage terminal gate: %w", err)
	}
	if !agentStageTerminalGatesEqual(persisted, gate) {
		return fmt.Errorf("appended agent-stage terminal gate differs from request")
	}
	return nil
}

// LookupAgentStageTerminalGate returns the sole terminal winner for an exact
// intent. Missing is (zero,false,nil); authorization, lineage, artifact, and
// corruption failures remain explicit errors.
func (repository *Repository) LookupAgentStageTerminalGate(
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageTerminalGate, bool, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageTerminalGate{}, false, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	gateID, err := runmodel.AgentStageTerminalGateID(runID, intentID)
	if err != nil {
		return runmodel.AgentStageTerminalGate{}, false, err
	}
	gate, err := repository.LoadAgentStageTerminalGate(runID, gateID, subject)
	if errors.Is(err, os.ErrNotExist) {
		return runmodel.AgentStageTerminalGate{}, false, nil
	}
	if err != nil {
		return runmodel.AgentStageTerminalGate{}, false, err
	}
	if gate.IntentID != intentID {
		return runmodel.AgentStageTerminalGate{}, false, fmt.Errorf(
			"agent-stage terminal gate ID resolved a different dispatch intent",
		)
	}
	return gate, true, nil
}

// LoadAgentStageTerminalGate strictly reloads the complete terminal stream and
// revalidates the selected winner against current local upstream facts.
func (repository *Repository) LoadAgentStageTerminalGate(
	runID string,
	gateID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageTerminalGate, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	if !strings.HasPrefix(gateID, runID+"-") {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"agent-stage terminal gate %q is not namespaced by run %q",
			gateID,
			runID,
		)
	}
	gates, err := repository.readAgentStageTerminalGates(runID)
	if err != nil {
		return runmodel.AgentStageTerminalGate{}, err
	}
	for _, gate := range gates {
		if gate.GateID != gateID {
			continue
		}
		if gate.Subject != subject {
			return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
				"%w: run %q stage %q intent %q",
				ErrAgentStageTerminalGateSubjectMismatch,
				gate.ReviewRunID,
				gate.Stage.ID,
				gate.IntentID,
			)
		}
		return gate, nil
	}
	return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
		"agent-stage terminal gate %q in run %q: %w",
		gateID,
		runID,
		os.ErrNotExist,
	)
}

func (repository *Repository) readAgentStageTerminalGates(
	runID string,
) ([]runmodel.AgentStageTerminalGate, error) {
	stream, err := agentStageTerminalGateStream(runID)
	if err != nil {
		return nil, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			envelopes = []local.Envelope{}
		} else {
			return nil, fmt.Errorf("read agent-stage terminal gate stream: %w", err)
		}
	}
	decisions, err := repository.readAgentStageGenerationDecisions(runID)
	if err != nil {
		return nil, fmt.Errorf("read generation decisions for terminal gates: %w", err)
	}
	coordinatedByID := make(map[string]runmodel.AgentStageTerminalGate)
	for _, decision := range decisions {
		if decision.Gate == nil {
			continue
		}
		coordinatedByID[decision.Gate.GateID] = *decision.Gate
	}
	admissions, err := repository.readAgentStagePlanAdmissions(runID)
	if err != nil {
		return nil, fmt.Errorf("read plan admissions for terminal gates: %w", err)
	}
	intents, err := repository.readAgentStageDispatchIntents(runID)
	if err != nil {
		return nil, fmt.Errorf("read dispatch intents for terminal gates: %w", err)
	}
	bindings, err := repository.readAgentStageExecutionBindings(runID)
	if err != nil {
		return nil, fmt.Errorf("read execution bindings for terminal gates: %w", err)
	}
	admissionsByID := make(map[string]runmodel.AgentStagePlanAdmission, len(admissions))
	for _, admission := range admissions {
		admissionsByID[admission.AdmissionID] = admission
	}
	intentsByID := make(map[string]runmodel.AgentStageDispatchIntent, len(intents))
	for _, intent := range intents {
		intentsByID[intent.IntentID] = intent
	}
	bindingsByID := make(map[string]runmodel.AgentStageExecutionBinding, len(bindings))
	for _, binding := range bindings {
		bindingsByID[binding.BindingID] = binding
	}

	gates := make([]runmodel.AgentStageTerminalGate, 0, len(coordinatedByID))
	projected := make(map[string]struct{}, len(envelopes))
	for _, envelope := range envelopes {
		gate, err := decodeAgentStageTerminalGate(envelope, runID)
		if err != nil {
			return nil, err
		}
		coordinated, ok := coordinatedByID[gate.GateID]
		if !ok || !agentStageTerminalGatesEqual(coordinated, gate) {
			return nil, fmt.Errorf(
				"agent-stage terminal gate %q has no exact generation reservation",
				gate.GateID,
			)
		}
		projected[gate.GateID] = struct{}{}
		gates = append(gates, gate)
	}
	// The generation decision is the terminal authority. If a process crashes
	// after that CAS but before the legacy/query projection append, readers still
	// recover and validate the exact embedded sealed gate instead of reporting a
	// false absence. A later Append retry remains idempotent.
	for gateID, gate := range coordinatedByID {
		if _, ok := projected[gateID]; ok {
			continue
		}
		gates = append(gates, gate)
	}
	sort.Slice(gates, func(left, right int) bool {
		if !gates[left].EventTime().Equal(gates[right].EventTime()) {
			return gates[left].EventTime().Before(gates[right].EventTime())
		}
		return gates[left].GateID < gates[right].GateID
	})

	for _, gate := range gates {
		admission, ok := admissionsByID[gate.AdmissionID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage terminal gate %q has no exact plan admission",
				gate.GateID,
			)
		}
		intent, ok := intentsByID[gate.IntentID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage terminal gate %q has no exact dispatch intent",
				gate.GateID,
			)
		}
		if err := gate.ValidateAgainstAdmissionAndIntent(admission, intent); err != nil {
			return nil, fmt.Errorf(
				"validate persisted agent-stage terminal gate %q: %w",
				gate.GateID,
				err,
			)
		}
		request, err := repository.verifyAgentStageTerminalRequest(
			gate,
			admission,
			intent,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"verify persisted agent-stage terminal gate %q request: %w",
				gate.GateID,
				err,
			)
		}
		if gate.Kind != runmodel.AgentStageCancellationRequested {
			bindingID := ""
			if gate.Completion != nil {
				bindingID = gate.Completion.BindingID
			} else if gate.Outcome != nil {
				bindingID = gate.Outcome.BindingID
			}
			binding, ok := bindingsByID[bindingID]
			if !ok {
				return nil, fmt.Errorf(
					"agent-stage terminal gate %q has no exact execution binding",
					gate.GateID,
				)
			}
			switch gate.Kind {
			case runmodel.AgentStageSucceededResultAccepted:
				if err := gate.ValidateCompletionAgainstBinding(binding); err != nil {
					return nil, fmt.Errorf(
						"validate persisted terminal completion %q: %w",
						gate.GateID,
						err,
					)
				}
				if err := repository.verifyAgentStageTerminalCompletion(
					gate,
					request,
					binding,
				); err != nil {
					return nil, err
				}
			case runmodel.AgentStageFailedResultAccepted,
				runmodel.AgentStageCanceledResultAccepted:
				if err := gate.ValidateOutcomeAgainstBinding(binding); err != nil {
					return nil, fmt.Errorf(
						"validate persisted terminal outcome %q: %w",
						gate.GateID,
						err,
					)
				}
				if err := repository.verifyAgentStageTerminalOutcome(
					gate,
					request,
					binding,
				); err != nil {
					return nil, err
				}
			}
		}
	}
	return gates, nil
}

func (repository *Repository) validateAgentStageTerminalGateClosure(
	gate runmodel.AgentStageTerminalGate,
) error {
	admission, err := repository.LoadAgentStagePlanAdmission(
		gate.ReviewRunID,
		gate.AdmissionID,
		gate.Subject,
	)
	if err != nil {
		return fmt.Errorf("load plan admission for terminal gate: %w", err)
	}
	intent, err := repository.LoadAgentStageDispatchIntent(
		gate.ReviewRunID,
		gate.IntentID,
		gate.Subject,
	)
	if err != nil {
		return fmt.Errorf("load dispatch intent for terminal gate: %w", err)
	}
	if err := gate.ValidateAgainstAdmissionAndIntent(admission, intent); err != nil {
		return err
	}
	request, err := repository.verifyAgentStageTerminalRequest(gate, admission, intent)
	if err != nil {
		return err
	}
	if gate.Kind == runmodel.AgentStageCancellationRequested {
		return nil
	}
	bindingID := ""
	if gate.Completion != nil {
		bindingID = gate.Completion.BindingID
	} else if gate.Outcome != nil {
		bindingID = gate.Outcome.BindingID
	}
	binding, err := repository.LoadAgentStageExecutionBinding(
		gate.ReviewRunID,
		bindingID,
		gate.Subject,
	)
	if err != nil {
		return fmt.Errorf("load execution binding for terminal completion: %w", err)
	}
	switch gate.Kind {
	case runmodel.AgentStageSucceededResultAccepted:
		if err := gate.ValidateCompletionAgainstBinding(binding); err != nil {
			return err
		}
	case runmodel.AgentStageFailedResultAccepted,
		runmodel.AgentStageCanceledResultAccepted:
		if err := gate.ValidateOutcomeAgainstBinding(binding); err != nil {
			return err
		}
	}
	if gate.Kind == runmodel.AgentStageSucceededResultAccepted {
		return repository.verifyAgentStageTerminalCompletion(gate, request, binding)
	}
	return repository.verifyAgentStageTerminalOutcome(gate, request, binding)
}

func (repository *Repository) verifyAgentStageTerminalRequest(
	gate runmodel.AgentStageTerminalGate,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
) (contractsv1alpha1.StageExecutionRequest, error) {
	request, err := repository.readCanonicalAgentStageExecutionRequest(gate.RequestRef)
	if err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"read canonical StageExecutionRequest artifact for terminal gate: %w",
			err,
		)
	}
	if err := validateAgentStageTerminalRequestBinding(
		gate,
		admission,
		intent,
		request,
	); err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, err
	}
	return request, nil
}

func (repository *Repository) readCanonicalAgentStageExecutionRequest(
	ref runmodel.ArtifactRef,
) (contractsv1alpha1.StageExecutionRequest, error) {
	data, err := repository.ReadArtifact(ref)
	if err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, err
	}
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(data)
	if err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"strictly decode StageExecutionRequest: %w",
			err,
		)
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"marshal canonical StageExecutionRequest: %w",
			err,
		)
	}
	if !bytes.Equal(data, canonical) {
		return contractsv1alpha1.StageExecutionRequest{}, fmt.Errorf(
			"StageExecutionRequest artifact is not canonical JSON",
		)
	}
	return request, nil
}

func validateAgentStageTerminalRequestBinding(
	gate runmodel.AgentStageTerminalGate,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	request contractsv1alpha1.StageExecutionRequest,
) error {
	if request.RequestSHA256 != gate.RequestSemanticSHA256 ||
		request.RequestSHA256 != intent.RequestSemanticSHA256 ||
		request.ExecutionID != gate.ExecutionID || request.ExecutionID != intent.ExecutionID ||
		request.ReviewRunID != gate.ReviewRunID ||
		request.TenantID != gate.Subject.TenantID ||
		request.WorkspaceID != gate.Subject.WorkspaceID ||
		request.WorkloadID != gate.WorkloadID || request.WorkloadID != intent.WorkloadID ||
		request.LeaseID != gate.LeaseID || request.LeaseID != intent.LeaseID ||
		request.LeaseWorkerID != gate.LeaseWorker ||
		request.LeaseWorkerID != intent.LeaseWorker ||
		request.Stage.ID != gate.Stage.ID || request.Stage.Revision != gate.Stage.Revision ||
		request.Stage.SHA256 != gate.Stage.SHA256 ||
		request.Attempt != gate.Attempt || request.Attempt != intent.Attempt ||
		request.Generation != gate.Generation || request.Generation != intent.Generation ||
		request.FencingToken != gate.FencingToken ||
		request.FencingToken != intent.FencingToken ||
		request.IdempotencyKey != intent.CreateIdempotencyKey ||
		request.Capability.SHA256 != gate.CapabilitySHA256 ||
		request.Capability.SHA256 != intent.CapabilitySHA256 ||
		!request.Deadline.Equal(gate.RequestDeadline) ||
		request.OutputContract != runmodel.ContractReviewHypothesisSet ||
		!terminalRequestBindingMatches(request.Plan, admission.Plan.Governed) ||
		!terminalRequestBindingMatches(
			request.ExecutionSnapshot,
			admission.Sources.ExecutionSnapshot.Governed,
		) ||
		!terminalRequestBindingMatches(
			request.ReviewInput,
			admission.Sources.ReviewInput.Governed,
		) {
		return fmt.Errorf(
			"StageExecutionRequest does not match exact terminal gate and dispatch intent",
		)
	}
	return nil
}

func terminalRequestBindingMatches(
	binding contractsv1alpha1.ArtifactBinding,
	governed runmodel.GovernedArtifactBinding,
) bool {
	return binding.Ref.URI == governed.URI && binding.Ref.SHA256 == governed.SHA256 &&
		binding.Ref.SizeBytes == governed.SizeBytes && binding.Contract == governed.Contract
}

func (repository *Repository) verifyAgentStageTerminalCompletion(
	gate runmodel.AgentStageTerminalGate,
	request contractsv1alpha1.StageExecutionRequest,
	binding runmodel.AgentStageExecutionBinding,
) error {
	completion := gate.Completion
	result, err := repository.readCanonicalAgentStageExecutionResult(completion.ResultRef)
	if err != nil {
		return fmt.Errorf(
			"read canonical StageExecutionResult artifact for terminal gate: %w",
			err,
		)
	}
	if err := contractsv1alpha1.ValidateStageExecutionResultBinding(request, result); err != nil {
		return fmt.Errorf("bind StageExecutionResult to exact request: %w", err)
	}
	if result.Status != contractsv1alpha1.StageExecutionSucceeded || result.Output == nil {
		return fmt.Errorf("terminal completion requires a succeeded StageExecutionResult")
	}
	if string(result.Status) != completion.Status ||
		result.Completeness != completion.Completeness ||
		!equalStrings(result.CompletenessNotes, completion.CompletenessNotes) ||
		!result.RecordedAt.Equal(completion.ProviderResultRecordedAt) ||
		result.Output.Ref.URI != completion.Output.Governed.URI ||
		result.Output.Ref.SHA256 != completion.Output.Governed.SHA256 ||
		result.Output.Ref.SizeBytes != completion.Output.Governed.SizeBytes ||
		result.Output.Contract != completion.Output.Governed.Contract {
		return fmt.Errorf(
			"StageExecutionResult does not match exact terminal completion and output projection",
		)
	}
	if (result.TraceManifest == nil) != (completion.TraceManifest == nil) {
		return fmt.Errorf(
			"StageExecutionResult trace manifest presence does not match terminal completion",
		)
	}
	if err := repository.verifyAgentStageResultCallbackReceipt(
		completion.CallbackReceiptRef,
		completion.CallbackReceiptSHA256,
		completion.ResultRef,
		completion.AcceptedAt,
		binding,
		result.RecordedAt,
	); err != nil {
		return err
	}
	if result.TraceManifest != nil {
		trace := completion.TraceManifest.Governed
		if result.TraceManifest.Ref.URI != trace.URI ||
			result.TraceManifest.Ref.SHA256 != trace.SHA256 ||
			result.TraceManifest.Ref.SizeBytes != trace.SizeBytes ||
			result.TraceManifest.Contract != trace.Contract {
			return fmt.Errorf(
				"StageExecutionResult trace manifest does not match exact terminal projection",
			)
		}
		if _, err := repository.ReadArtifact(completion.TraceManifest.Local); err != nil {
			return fmt.Errorf("read exact terminal trace manifest artifact: %w", err)
		}
	}
	if result.AgentTaskEvidence != nil {
		binding := result.AgentTaskEvidence
		local := runmodel.ArtifactRef{
			URI:    "artifact://local/sha256/" + binding.Ref.SHA256,
			SHA256: binding.Ref.SHA256, SizeBytes: binding.Ref.SizeBytes,
			Contract: binding.Contract,
		}
		data, err := repository.ReadArtifact(local)
		if err != nil {
			return fmt.Errorf("read exact terminal agent task evidence artifact: %w", err)
		}
		if _, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(data); err != nil {
			return fmt.Errorf("decode exact terminal agent task evidence artifact: %w", err)
		}
	}
	if _, err := repository.ReadArtifact(completion.Output.Local); err != nil {
		return fmt.Errorf(
			"read exact terminal output artifact: %w",
			err,
		)
	}
	return nil
}

func (repository *Repository) verifyAgentStageTerminalOutcome(
	gate runmodel.AgentStageTerminalGate,
	request contractsv1alpha1.StageExecutionRequest,
	binding runmodel.AgentStageExecutionBinding,
) error {
	outcome := gate.Outcome
	if outcome == nil {
		return fmt.Errorf("terminal outcome is required")
	}
	result, err := repository.readCanonicalAgentStageExecutionResult(outcome.ResultRef)
	if err != nil {
		return fmt.Errorf(
			"read canonical StageExecutionResult artifact for terminal outcome: %w",
			err,
		)
	}
	if err := contractsv1alpha1.ValidateStageExecutionResultBinding(request, result); err != nil {
		return fmt.Errorf("bind StageExecutionResult to exact request: %w", err)
	}
	wantStatus := contractsv1alpha1.StageExecutionFailed
	if gate.Kind == runmodel.AgentStageCanceledResultAccepted {
		wantStatus = contractsv1alpha1.StageExecutionCanceled
	}
	if result.Status != wantStatus || result.Failure == nil || result.Output != nil {
		return fmt.Errorf(
			"terminal outcome requires a %s StageExecutionResult without output",
			wantStatus,
		)
	}
	if string(result.Status) != outcome.Status ||
		result.Failure.Code != outcome.Failure.Code ||
		result.Failure.Message != outcome.Failure.Message ||
		result.Failure.Retryable != outcome.Failure.Retryable ||
		result.Completeness != outcome.Completeness ||
		!equalStrings(result.CompletenessNotes, outcome.CompletenessNotes) ||
		!result.RecordedAt.Equal(outcome.ProviderResultRecordedAt) {
		return fmt.Errorf("StageExecutionResult does not match exact terminal outcome")
	}
	if (result.TraceManifest == nil) != (outcome.TraceManifest == nil) {
		return fmt.Errorf(
			"StageExecutionResult trace manifest presence does not match terminal outcome",
		)
	}
	if (result.AgentTaskEvidence == nil) != (outcome.AgentTaskEvidence == nil) ||
		(result.AgentExecutionReceipts == nil) != (outcome.AgentExecutionReceipts == nil) {
		return fmt.Errorf(
			"StageExecutionResult diagnostic evidence presence does not match terminal outcome",
		)
	}
	if err := repository.verifyAgentStageResultCallbackReceipt(
		outcome.CallbackReceiptRef,
		outcome.CallbackReceiptSHA256,
		outcome.ResultRef,
		outcome.AcceptedAt,
		binding,
		result.RecordedAt,
	); err != nil {
		return err
	}
	if result.TraceManifest != nil {
		trace := outcome.TraceManifest.Governed
		if result.TraceManifest.Ref.URI != trace.URI ||
			result.TraceManifest.Ref.SHA256 != trace.SHA256 ||
			result.TraceManifest.Ref.SizeBytes != trace.SizeBytes ||
			result.TraceManifest.Contract != trace.Contract {
			return fmt.Errorf(
				"StageExecutionResult trace manifest does not match exact terminal outcome projection",
			)
		}
		if _, err := repository.ReadArtifact(outcome.TraceManifest.Local); err != nil {
			return fmt.Errorf("read exact terminal outcome trace manifest artifact: %w", err)
		}
	}
	if result.AgentTaskEvidence != nil {
		if err := repository.verifyAgentStageOutcomeDiagnosticArtifact(
			*result.AgentTaskEvidence,
			*outcome.AgentTaskEvidence,
			true,
		); err != nil {
			return err
		}
		if err := repository.verifyAgentStageOutcomeDiagnosticArtifact(
			*result.AgentExecutionReceipts,
			*outcome.AgentExecutionReceipts,
			false,
		); err != nil {
			return err
		}
	}
	return nil
}

func (repository *Repository) verifyAgentStageOutcomeDiagnosticArtifact(
	binding contractsv1alpha1.ArtifactBinding,
	projection runmodel.AgentArtifactProjection,
	taskEvidence bool,
) error {
	governed := projection.Governed
	if binding.Ref.URI != governed.URI || binding.Ref.SHA256 != governed.SHA256 ||
		binding.Ref.SizeBytes != governed.SizeBytes || binding.Contract != governed.Contract {
		return fmt.Errorf("StageExecutionResult diagnostic artifact does not match terminal outcome projection")
	}
	data, err := repository.ReadArtifact(projection.Local)
	if err != nil {
		return fmt.Errorf("read exact terminal outcome diagnostic artifact: %w", err)
	}
	if taskEvidence {
		if _, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(data); err != nil {
			return fmt.Errorf("decode exact terminal outcome task evidence: %w", err)
		}
		return nil
	}
	if _, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(data); err != nil {
		return fmt.Errorf("decode exact terminal outcome execution receipts: %w", err)
	}
	return nil
}

func (repository *Repository) readCanonicalAgentStageExecutionResult(
	ref runmodel.ArtifactRef,
) (contractsv1alpha1.StageExecutionResult, error) {
	data, err := repository.ReadArtifact(ref)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, err
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(data)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"strictly decode StageExecutionResult: %w",
			err,
		)
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"marshal canonical StageExecutionResult: %w",
			err,
		)
	}
	if !bytes.Equal(data, canonical) {
		return contractsv1alpha1.StageExecutionResult{}, fmt.Errorf(
			"StageExecutionResult artifact is not canonical JSON",
		)
	}
	return result, nil
}

func (repository *Repository) verifyAgentStageResultCallbackReceipt(
	receiptArtifact runmodel.ArtifactRef,
	receiptSHA256 string,
	resultRef runmodel.ArtifactRef,
	acceptedAt time.Time,
	binding runmodel.AgentStageExecutionBinding,
	resultRecordedAt time.Time,
) error {
	receipt, err := repository.readCanonicalAgentStageResultCallbackReceipt(
		receiptArtifact,
	)
	if err != nil {
		return fmt.Errorf("read exact callback receipt for terminal gate: %w", err)
	}
	if receipt.SHA256 != receiptSHA256 {
		return fmt.Errorf(
			"callback receipt semantic digest does not match terminal completion",
		)
	}
	if err := receipt.ValidateAgainstBindingAndResult(
		binding,
		resultRef,
	); err != nil {
		return fmt.Errorf(
			"bind callback receipt to exact execution binding and result: %w",
			err,
		)
	}
	if receipt.VerifiedAt.Before(resultRecordedAt) {
		return fmt.Errorf(
			"callback receipt verified_at predates provider result recorded_at",
		)
	}
	if receipt.VerifiedAt.After(acceptedAt) {
		return fmt.Errorf("callback receipt verified_at is after terminal accepted_at")
	}
	persisted, receiptRef, found, err := repository.LookupAgentStageResultCallbackReceipt(
		binding.ReviewRunID,
		binding.BindingID,
		binding.Subject,
	)
	if err != nil {
		return fmt.Errorf("load authoritative callback receipt for terminal gate: %w", err)
	}
	if !found {
		return fmt.Errorf("terminal completion has no authoritative callback receipt")
	}
	if receiptRef != receiptArtifact {
		return fmt.Errorf(
			"callback receipt artifact reference does not match terminal completion",
		)
	}
	if persisted != receipt || persisted.SHA256 != receiptSHA256 {
		return fmt.Errorf(
			"authoritative callback receipt does not match terminal completion",
		)
	}
	return nil
}

func decodeAgentStageResultCallbackReceiptRecord(
	envelope local.Envelope,
	runID string,
) (agentStageResultCallbackReceiptRecord, error) {
	if envelope.Schema != agentStageResultCallbackReceiptRecordSchemaVersion {
		return agentStageResultCallbackReceiptRecord{}, fmt.Errorf(
			"unsupported callback receipt record event schema %q",
			envelope.Schema,
		)
	}
	var record agentStageResultCallbackReceiptRecord
	if err := decodeStrictJSON(envelope.Payload, &record); err != nil {
		return agentStageResultCallbackReceiptRecord{}, fmt.Errorf(
			"decode callback receipt record %q: %w",
			envelope.ID,
			err,
		)
	}
	if err := validateAgentStageResultCallbackReceiptRecord(record); err != nil {
		return agentStageResultCallbackReceiptRecord{}, fmt.Errorf(
			"validate callback receipt record %q: %w",
			envelope.ID,
			err,
		)
	}
	receipt := record.Receipt
	if receipt.ReviewRunID != runID || !strings.HasPrefix(envelope.ID, runID+"-") {
		return agentStageResultCallbackReceiptRecord{}, fmt.Errorf(
			"callback receipt record %q is not namespaced by run %q",
			envelope.ID,
			runID,
		)
	}
	if envelope.ID != receipt.ReceiptID {
		return agentStageResultCallbackReceiptRecord{}, fmt.Errorf(
			"callback receipt record event ID does not match its payload",
		)
	}
	if !envelope.Time.Equal(receipt.VerifiedAt) {
		return agentStageResultCallbackReceiptRecord{}, fmt.Errorf(
			"callback receipt record event time does not match verified_at",
		)
	}
	return record, nil
}

func agentStageResultCallbackReceiptStream(runID string) (string, error) {
	bindingID, err := runmodel.AgentStageExecutionBindingID(
		runID,
		runID+"-agent-stage-dispatch-namespace-check",
	)
	if err != nil {
		return "", err
	}
	if _, err := runmodel.AgentStageResultCallbackReceiptID(runID, bindingID); err != nil {
		return "", err
	}
	return "runs/" + runID + "/agent-stage-result-callback-receipts", nil
}

func decodeAgentStageTerminalGate(
	envelope local.Envelope,
	runID string,
) (runmodel.AgentStageTerminalGate, error) {
	if envelope.Schema != runmodel.AgentStageTerminalGateSchemaVersion {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"unsupported agent-stage terminal gate event schema %q",
			envelope.Schema,
		)
	}
	var gate runmodel.AgentStageTerminalGate
	if err := decodeStrictJSON(envelope.Payload, &gate); err != nil {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"decode agent-stage terminal gate %q: %w",
			envelope.ID,
			err,
		)
	}
	if gate.ReviewRunID != runID || !strings.HasPrefix(envelope.ID, runID+"-") {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"agent-stage terminal gate event %q is not namespaced by run %q",
			envelope.ID,
			runID,
		)
	}
	if envelope.ID != gate.GateID {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"agent-stage terminal gate event ID does not match its payload",
		)
	}
	if !envelope.Time.Equal(gate.EventTime()) {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"agent-stage terminal gate event time does not match host-observed branch time",
		)
	}
	if err := gate.Validate(); err != nil {
		return runmodel.AgentStageTerminalGate{}, fmt.Errorf(
			"validate agent-stage terminal gate %q: %w",
			envelope.ID,
			err,
		)
	}
	return gate, nil
}

func agentStageTerminalGateStream(runID string) (string, error) {
	intentID, err := runmodel.AgentStageDispatchIntentID(
		runID,
		"namespace-check",
		1,
		1,
	)
	if err != nil {
		return "", err
	}
	if _, err := runmodel.AgentStageTerminalGateID(runID, intentID); err != nil {
		return "", err
	}
	return "runs/" + runID + "/agent-stage-terminal-gates", nil
}

// Gate payloads contain slices, so direct struct comparison is unavailable.
// Their sealed self digests cover every field and preserve exact retry identity.
func agentStageTerminalGatesEqual(left, right runmodel.AgentStageTerminalGate) bool {
	return left.GateID == right.GateID && left.SHA256 == right.SHA256
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) || (left == nil) != (right == nil) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
