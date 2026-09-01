package runrepo

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
)

var (
	ErrAgentStageHypothesisEvidenceConflict        = local.ErrEventConflict
	ErrAgentStageHypothesisEvidenceSubjectMismatch = errors.New(
		"agent-stage hypothesis evidence belongs to another planning subject",
	)
)

// AppendAgentStageHypothesisEvidence records an immutable provenance fact for
// one strictly admitted hypothesis artifact. It re-resolves the full
// admission -> intent -> execution-binding -> accepted-terminal lineage before
// append and refuses missing local result/output bytes. An exact retry is
// idempotent; changed content under the binding-derived evidence ID conflicts.
func (repository *Repository) AppendAgentStageHypothesisEvidence(
	evidence runmodel.AgentStageHypothesisEvidence,
) error {
	if err := evidence.Validate(); err != nil {
		return fmt.Errorf("validate AgentStageHypothesisEvidence: %w", err)
	}
	if !strings.HasPrefix(evidence.EvidenceID, evidence.ReviewRunID+"-") {
		return fmt.Errorf(
			"agent-stage hypothesis evidence %q is not namespaced by run %q",
			evidence.EvidenceID,
			evidence.ReviewRunID,
		)
	}
	if err := repository.validateAgentStageHypothesisEvidenceClosure(evidence); err != nil {
		return err
	}
	if err := repository.verifyAgentStageHypothesisEvidenceArtifacts(evidence); err != nil {
		return err
	}
	stream, err := agentStageHypothesisEvidenceStream(evidence.ReviewRunID)
	if err != nil {
		return err
	}
	// Refuse to extend a corrupt, stale, cross-subject, or artifact-broken
	// stream. AppendJSONL owns the duplicate-ID decision under its file lock.
	if _, err := repository.readAgentStageHypothesisEvidence(
		evidence.ReviewRunID,
	); err != nil {
		return err
	}
	envelope, err := repository.store.AppendJSONL(stream, local.Event{
		ID:      evidence.EvidenceID,
		Schema:  runmodel.AgentStageHypothesisEvidenceSchemaVersion,
		Time:    evidence.AdmittedAt,
		Payload: evidence,
	})
	if err != nil {
		return fmt.Errorf("append authoritative agent-stage hypothesis evidence: %w", err)
	}
	persisted, err := decodeAgentStageHypothesisEvidence(
		envelope,
		evidence.ReviewRunID,
	)
	if err != nil {
		return fmt.Errorf("verify appended agent-stage hypothesis evidence: %w", err)
	}
	if persisted != evidence {
		return fmt.Errorf("appended agent-stage hypothesis evidence differs from request")
	}
	return nil
}

// LookupAgentStageHypothesisEvidence resolves the sole formal hypothesis
// evidence fact for an execution binding. Missing is returned as
// (zero,false,nil); authorization, lineage, and corruption failures remain
// explicit errors.
func (repository *Repository) LookupAgentStageHypothesisEvidence(
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageHypothesisEvidence, bool, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, false, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	evidenceID, err := runmodel.AgentStageHypothesisEvidenceID(runID, bindingID)
	if err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, false, err
	}
	evidence, err := repository.LoadAgentStageHypothesisEvidence(
		runID,
		evidenceID,
		subject,
	)
	if errors.Is(err, os.ErrNotExist) {
		return runmodel.AgentStageHypothesisEvidence{}, false, nil
	}
	if err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, false, err
	}
	if evidence.BindingID != bindingID {
		return runmodel.AgentStageHypothesisEvidence{}, false, fmt.Errorf(
			"agent-stage hypothesis evidence ID resolved a different execution binding",
		)
	}
	return evidence, true, nil
}

// LoadAgentStageHypothesisEvidence loads one exact evidence ID only after
// validating every persisted evidence fact and its complete upstream closure.
func (repository *Repository) LoadAgentStageHypothesisEvidence(
	runID string,
	evidenceID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageHypothesisEvidence, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	if !strings.HasPrefix(evidenceID, runID+"-") {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"agent-stage hypothesis evidence %q is not namespaced by run %q",
			evidenceID,
			runID,
		)
	}
	evidenceFacts, err := repository.readAgentStageHypothesisEvidence(runID)
	if err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, err
	}
	for _, evidence := range evidenceFacts {
		if evidence.EvidenceID != evidenceID {
			continue
		}
		if evidence.Subject != subject {
			return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
				"%w: run %q stage %q binding %q",
				ErrAgentStageHypothesisEvidenceSubjectMismatch,
				evidence.ReviewRunID,
				evidence.Stage.ID,
				evidence.BindingID,
			)
		}
		// readAgentStageHypothesisEvidence has already revalidated the
		// admission -> intent -> binding -> accepted-terminal closure and local
		// artifact bytes.
		return evidence, nil
	}
	return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
		"agent-stage hypothesis evidence %q in run %q: %w",
		evidenceID,
		runID,
		os.ErrNotExist,
	)
}

func (repository *Repository) readAgentStageHypothesisEvidence(
	runID string,
) ([]runmodel.AgentStageHypothesisEvidence, error) {
	stream, err := agentStageHypothesisEvidenceStream(runID)
	if err != nil {
		return nil, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []runmodel.AgentStageHypothesisEvidence{}, nil
		}
		return nil, fmt.Errorf("read agent-stage hypothesis evidence stream: %w", err)
	}
	admissions, err := repository.readAgentStagePlanAdmissions(runID)
	if err != nil {
		return nil, fmt.Errorf(
			"read plan admissions for agent-stage hypothesis evidence: %w",
			err,
		)
	}
	intents, err := repository.readAgentStageDispatchIntents(runID)
	if err != nil {
		return nil, fmt.Errorf(
			"read dispatch intents for agent-stage hypothesis evidence: %w",
			err,
		)
	}
	bindings, err := repository.readAgentStageExecutionBindings(runID)
	if err != nil {
		return nil, fmt.Errorf(
			"read execution bindings for agent-stage hypothesis evidence: %w",
			err,
		)
	}
	terminalGates, err := repository.readAgentStageTerminalGates(runID)
	if err != nil {
		return nil, fmt.Errorf(
			"read terminal gates for agent-stage hypothesis evidence: %w",
			err,
		)
	}
	admissionsByID := make(
		map[string]runmodel.AgentStagePlanAdmission,
		len(admissions),
	)
	for _, admission := range admissions {
		admissionsByID[admission.AdmissionID] = admission
	}
	intentsByID := make(
		map[string]runmodel.AgentStageDispatchIntent,
		len(intents),
	)
	for _, intent := range intents {
		intentsByID[intent.IntentID] = intent
	}
	bindingsByID := make(
		map[string]runmodel.AgentStageExecutionBinding,
		len(bindings),
	)
	for _, binding := range bindings {
		bindingsByID[binding.BindingID] = binding
	}
	terminalGatesByID := make(
		map[string]runmodel.AgentStageTerminalGate,
		len(terminalGates),
	)
	for _, gate := range terminalGates {
		terminalGatesByID[gate.GateID] = gate
	}

	evidenceFacts := make(
		[]runmodel.AgentStageHypothesisEvidence,
		0,
		len(envelopes),
	)
	for _, envelope := range envelopes {
		evidence, err := decodeAgentStageHypothesisEvidence(envelope, runID)
		if err != nil {
			return nil, err
		}
		admission, ok := admissionsByID[evidence.AdmissionID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage hypothesis evidence %q has no exact plan admission",
				evidence.EvidenceID,
			)
		}
		intent, ok := intentsByID[evidence.IntentID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage hypothesis evidence %q has no exact dispatch intent",
				evidence.EvidenceID,
			)
		}
		binding, ok := bindingsByID[evidence.BindingID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage hypothesis evidence %q has no exact execution binding",
				evidence.EvidenceID,
			)
		}
		terminal, ok := terminalGatesByID[evidence.TerminalGateID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage hypothesis evidence %q has no exact accepted terminal gate",
				evidence.EvidenceID,
			)
		}
		if err := evidence.ValidateAgainstClosure(
			admission,
			intent,
			binding,
			terminal,
		); err != nil {
			return nil, fmt.Errorf(
				"validate persisted agent-stage hypothesis evidence %q: %w",
				evidence.EvidenceID,
				err,
			)
		}
		if err := repository.verifyAgentStageHypothesisEvidenceArtifacts(
			evidence,
		); err != nil {
			return nil, err
		}
		evidenceFacts = append(evidenceFacts, evidence)
	}
	return evidenceFacts, nil
}

func (repository *Repository) validateAgentStageHypothesisEvidenceClosure(
	evidence runmodel.AgentStageHypothesisEvidence,
) error {
	admission, err := repository.LoadAgentStagePlanAdmission(
		evidence.ReviewRunID,
		evidence.AdmissionID,
		evidence.Subject,
	)
	if err != nil {
		return fmt.Errorf("load plan admission for hypothesis evidence: %w", err)
	}
	intent, err := repository.LoadAgentStageDispatchIntent(
		evidence.ReviewRunID,
		evidence.IntentID,
		evidence.Subject,
	)
	if err != nil {
		return fmt.Errorf("load dispatch intent for hypothesis evidence: %w", err)
	}
	binding, err := repository.LoadAgentStageExecutionBinding(
		evidence.ReviewRunID,
		evidence.BindingID,
		evidence.Subject,
	)
	if err != nil {
		return fmt.Errorf("load execution binding for hypothesis evidence: %w", err)
	}
	terminal, err := repository.LoadAgentStageTerminalGate(
		evidence.ReviewRunID,
		evidence.TerminalGateID,
		evidence.Subject,
	)
	if err != nil {
		return fmt.Errorf("load accepted terminal gate for hypothesis evidence: %w", err)
	}
	if err := evidence.ValidateAgainstClosure(
		admission,
		intent,
		binding,
		terminal,
	); err != nil {
		return err
	}
	return nil
}

func (repository *Repository) verifyAgentStageHypothesisEvidenceArtifacts(
	evidence runmodel.AgentStageHypothesisEvidence,
) error {
	if _, err := repository.ReadArtifact(evidence.ResultRef); err != nil {
		return fmt.Errorf(
			"read exact StageExecutionResult artifact for hypothesis evidence: %w",
			err,
		)
	}
	if _, err := repository.ReadArtifact(evidence.Output.Local); err != nil {
		return fmt.Errorf(
			"read exact ReviewHypothesisSet artifact for hypothesis evidence: %w",
			err,
		)
	}
	return nil
}

func decodeAgentStageHypothesisEvidence(
	envelope local.Envelope,
	runID string,
) (runmodel.AgentStageHypothesisEvidence, error) {
	if envelope.Schema != runmodel.AgentStageHypothesisEvidenceSchemaVersion {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"unsupported agent-stage hypothesis evidence event schema %q",
			envelope.Schema,
		)
	}
	var evidence runmodel.AgentStageHypothesisEvidence
	if err := decodeStrictJSON(envelope.Payload, &evidence); err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"decode agent-stage hypothesis evidence %q: %w",
			envelope.ID,
			err,
		)
	}
	if evidence.ReviewRunID != runID ||
		!strings.HasPrefix(envelope.ID, runID+"-") {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"agent-stage hypothesis evidence event %q is not namespaced by run %q",
			envelope.ID,
			runID,
		)
	}
	if envelope.ID != evidence.EvidenceID {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"agent-stage hypothesis evidence event ID does not match its payload",
		)
	}
	if !envelope.Time.Equal(evidence.AdmittedAt) {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"agent-stage hypothesis evidence event time does not match admitted_at",
		)
	}
	if err := evidence.Validate(); err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, fmt.Errorf(
			"validate agent-stage hypothesis evidence %q: %w",
			envelope.ID,
			err,
		)
	}
	return evidence, nil
}

func agentStageHypothesisEvidenceStream(runID string) (string, error) {
	if _, err := runmodel.AgentStagePlanAdmissionID(
		runID,
		"namespace-check",
	); err != nil {
		return "", err
	}
	return "runs/" + runID + "/agent-stage-hypothesis-evidence", nil
}
