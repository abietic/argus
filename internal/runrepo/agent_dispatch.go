package runrepo

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

var (
	// Both formal dispatch facts reuse the local store's append conflict
	// sentinel so callers can recognize a concurrent changed-payload winner.
	ErrAgentStageDispatchConflict         = local.ErrEventConflict
	ErrAgentStageExecutionBindingConflict = local.ErrEventConflict

	ErrAgentStageDispatchSubjectMismatch = errors.New(
		"agent-stage dispatch intent belongs to another planning subject",
	)
	ErrAgentStageExecutionBindingSubjectMismatch = errors.New(
		"agent-stage execution binding belongs to another planning subject",
	)
)

// ClaimAgentStageDispatchIntent durably persists the exact dispatch intent and
// advances the shared dispatch/terminal generation coordinator. A first claim
// returns true; an exact retry returns false; changed content, stale generation,
// or a terminal-completion winner conflicts.
// Once claimed=true has been returned, every recovery may only retry the exact
// provider Ensure/create-or-return-one-handle primitive. It must never issue a
// non-idempotent Create, even when the first provider outcome is unknown.
func (repository *Repository) ClaimAgentStageDispatchIntent(
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	if err := repository.appendAgentStageDispatchIntent(intent); err != nil {
		return false, err
	}
	return repository.coordinateAgentStageDispatchClaim(intent)
}

// IsAgentStageDispatchIntentClaimed distinguishes a persisted-but-unclaimed
// intent from one whose provider Ensure authority has already been
// acquired. This distinction is required for crash recovery: an intent append
// may survive while the subsequent atomic claim does not.
func (repository *Repository) IsAgentStageDispatchIntentClaimed(
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	if err := intent.Validate(); err != nil {
		return false, fmt.Errorf("validate AgentStageDispatchIntent: %w", err)
	}
	persisted, err := repository.LoadAgentStageDispatchIntent(
		intent.ReviewRunID,
		intent.IntentID,
		intent.Subject,
	)
	if err != nil {
		return false, fmt.Errorf("load agent-stage dispatch intent for claim lookup: %w", err)
	}
	if persisted != intent {
		return false, fmt.Errorf(
			"persisted agent-stage dispatch intent differs from claim lookup: %w",
			local.ErrEventConflict,
		)
	}
	return repository.isAgentStageDispatchClaimed(intent)
}

// AppendAgentStageDispatchIntent persists the append-only intent without
// claiming provider Ensure ownership. Dispatching code must use Claim instead;
// this wrapper is only for persistence/migration tooling that never invokes a
// provider as a consequence of a nil return.
func (repository *Repository) AppendAgentStageDispatchIntent(
	intent runmodel.AgentStageDispatchIntent,
) error {
	return repository.appendAgentStageDispatchIntent(intent)
}

func (repository *Repository) appendAgentStageDispatchIntent(
	intent runmodel.AgentStageDispatchIntent,
) error {
	if err := intent.Validate(); err != nil {
		return fmt.Errorf("validate AgentStageDispatchIntent: %w", err)
	}
	if !strings.HasPrefix(intent.IntentID, intent.ReviewRunID+"-") {
		return fmt.Errorf(
			"agent-stage dispatch intent %q is not namespaced by run %q",
			intent.IntentID,
			intent.ReviewRunID,
		)
	}
	admission, err := repository.LoadAgentStagePlanAdmission(
		intent.ReviewRunID,
		intent.AdmissionID,
		intent.Subject,
	)
	if err != nil {
		return fmt.Errorf("load admitted plan for dispatch intent: %w", err)
	}
	if err := intent.ValidateAgainstAdmission(admission); err != nil {
		return err
	}
	stream, err := agentStageDispatchIntentStream(intent.ReviewRunID)
	if err != nil {
		return err
	}
	// Refuse to extend a corrupt stream. AppendJSONL supplies the authoritative
	// duplicate-ID decision under the cross-process stream lock.
	if _, err := repository.readAgentStageDispatchIntents(intent.ReviewRunID); err != nil {
		return err
	}
	envelope, err := repository.store.AppendJSONL(stream, local.Event{
		ID:      intent.IntentID,
		Schema:  runmodel.AgentStageDispatchIntentSchemaVersion,
		Time:    intent.RecordedAt,
		Payload: intent,
	})
	if err != nil {
		return fmt.Errorf("append authoritative agent-stage dispatch intent: %w", err)
	}
	persisted, err := decodeAgentStageDispatchIntent(envelope, intent.ReviewRunID)
	if err != nil {
		return fmt.Errorf("verify appended agent-stage dispatch intent: %w", err)
	}
	if persisted != intent {
		return fmt.Errorf("appended agent-stage dispatch intent differs from request")
	}
	return nil
}

func (repository *Repository) LookupAgentStageDispatchIntent(
	runID string,
	stageID string,
	attempt int,
	generation int,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageDispatchIntent, bool, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageDispatchIntent{}, false, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	intentID, err := runmodel.AgentStageDispatchIntentID(
		runID,
		stageID,
		attempt,
		generation,
	)
	if err != nil {
		return runmodel.AgentStageDispatchIntent{}, false, err
	}
	intent, err := repository.LoadAgentStageDispatchIntent(runID, intentID, subject)
	if errors.Is(err, os.ErrNotExist) {
		return runmodel.AgentStageDispatchIntent{}, false, nil
	}
	if err != nil {
		return runmodel.AgentStageDispatchIntent{}, false, err
	}
	if intent.Stage.ID != stageID || intent.Attempt != attempt ||
		intent.Generation != generation {
		return runmodel.AgentStageDispatchIntent{}, false, fmt.Errorf(
			"agent-stage dispatch intent ID resolved a different coordinate",
		)
	}
	return intent, true, nil
}

// ListClaimedAgentStageDispatchIntents returns every intent whose sole local
// Ensure authority was durably claimed. It deliberately includes the newest
// local generation: whether that claim is still current is decided from the
// authoritative scheduling workload, not inferred from the existence of a
// later dispatch ledger entry. This method never grants Ensure authority.
func (repository *Repository) ListClaimedAgentStageDispatchIntents(
	runID string,
	subject runmodel.AgentPlanningSubject,
) ([]runmodel.AgentStageDispatchIntent, error) {
	if err := subject.Validate(); err != nil {
		return nil, fmt.Errorf("validate planning subject: %w", err)
	}
	intents, err := repository.readAgentStageDispatchIntents(runID)
	if err != nil {
		return nil, err
	}
	for _, intent := range intents {
		if intent.Subject != subject {
			return nil, fmt.Errorf(
				"%w: run %q contains intent %q",
				ErrAgentStageDispatchSubjectMismatch,
				runID,
				intent.IntentID,
			)
		}
	}
	claimedIntents := make([]runmodel.AgentStageDispatchIntent, 0, len(intents))
	for _, intent := range intents {
		claimed, claimErr := repository.IsAgentStageDispatchIntentClaimed(intent)
		if claimErr != nil {
			return nil, fmt.Errorf(
				"inspect dispatch intent %q claim: %w",
				intent.IntentID,
				claimErr,
			)
		}
		if claimed {
			claimedIntents = append(claimedIntents, intent)
		}
	}
	sort.Slice(claimedIntents, func(left, right int) bool {
		if claimedIntents[left].Generation != claimedIntents[right].Generation {
			return claimedIntents[left].Generation < claimedIntents[right].Generation
		}
		if claimedIntents[left].Attempt != claimedIntents[right].Attempt {
			return claimedIntents[left].Attempt < claimedIntents[right].Attempt
		}
		return claimedIntents[left].IntentID < claimedIntents[right].IntentID
	})
	return claimedIntents, nil
}

// ListSupersededClaimedAgentStageDispatchIntents is retained as a narrow
// projection for diagnostics. Recovery must use ListClaimed... plus an
// authoritative scheduling staleness decision, because a canceled or expired
// latest claim may have no higher local generation.
func (repository *Repository) ListSupersededClaimedAgentStageDispatchIntents(
	runID string,
	subject runmodel.AgentPlanningSubject,
) ([]runmodel.AgentStageDispatchIntent, error) {
	intents, err := repository.ListClaimedAgentStageDispatchIntents(runID, subject)
	if err != nil {
		return nil, err
	}
	maxGeneration := make(map[string]int, len(intents))
	for _, intent := range intents {
		if intent.Generation > maxGeneration[intent.WorkloadID] {
			maxGeneration[intent.WorkloadID] = intent.Generation
		}
	}
	superseded := make([]runmodel.AgentStageDispatchIntent, 0, len(intents))
	for _, intent := range intents {
		if intent.Generation < maxGeneration[intent.WorkloadID] {
			superseded = append(superseded, intent)
		}
	}
	return superseded, nil
}

func (repository *Repository) LoadAgentStageDispatchIntent(
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageDispatchIntent, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	if !strings.HasPrefix(intentID, runID+"-") {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"agent-stage dispatch intent %q is not namespaced by run %q",
			intentID,
			runID,
		)
	}
	intents, err := repository.readAgentStageDispatchIntents(runID)
	if err != nil {
		return runmodel.AgentStageDispatchIntent{}, err
	}
	for _, intent := range intents {
		if intent.IntentID != intentID {
			continue
		}
		if intent.Subject != subject {
			return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
				"%w: run %q stage %q attempt %d generation %d",
				ErrAgentStageDispatchSubjectMismatch,
				intent.ReviewRunID,
				intent.Stage.ID,
				intent.Attempt,
				intent.Generation,
			)
		}
		admission, err := repository.LoadAgentStagePlanAdmission(
			intent.ReviewRunID,
			intent.AdmissionID,
			subject,
		)
		if err != nil {
			return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
				"load admitted plan for persisted dispatch intent: %w",
				err,
			)
		}
		if err := intent.ValidateAgainstAdmission(admission); err != nil {
			return runmodel.AgentStageDispatchIntent{}, err
		}
		return intent, nil
	}
	return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
		"agent-stage dispatch intent %q in run %q: %w",
		intentID,
		runID,
		os.ErrNotExist,
	)
}

// AppendAgentStageExecutionBinding records the sole provider acknowledgement
// for a committed exact intent. A binding cannot be appended before its intent
// and every echoed request/fencing/capability field is revalidated.
func (repository *Repository) AppendAgentStageExecutionBinding(
	binding runmodel.AgentStageExecutionBinding,
) error {
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("validate AgentStageExecutionBinding: %w", err)
	}
	if !strings.HasPrefix(binding.BindingID, binding.ReviewRunID+"-") {
		return fmt.Errorf(
			"agent-stage execution binding %q is not namespaced by run %q",
			binding.BindingID,
			binding.ReviewRunID,
		)
	}
	intent, err := repository.LoadAgentStageDispatchIntent(
		binding.ReviewRunID,
		binding.IntentID,
		binding.Subject,
	)
	if err != nil {
		return fmt.Errorf("load dispatch intent for execution binding: %w", err)
	}
	if err := binding.ValidateAgainstIntent(intent); err != nil {
		return err
	}
	if err := repository.verifyAgentStageDispatchClaim(intent); err != nil {
		return fmt.Errorf(
			"verify dispatch claim for execution binding: %w",
			err,
		)
	}
	stream, err := agentStageExecutionBindingStream(binding.ReviewRunID)
	if err != nil {
		return err
	}
	if _, err := repository.readAgentStageExecutionBindings(binding.ReviewRunID); err != nil {
		return err
	}
	envelope, err := repository.store.AppendJSONL(stream, local.Event{
		ID:      binding.BindingID,
		Schema:  runmodel.AgentStageExecutionBindingSchemaVersion,
		Time:    binding.RecordedAt,
		Payload: binding,
	})
	if err != nil {
		return fmt.Errorf("append authoritative agent-stage execution binding: %w", err)
	}
	persisted, err := decodeAgentStageExecutionBinding(envelope, binding.ReviewRunID)
	if err != nil {
		return fmt.Errorf("verify appended agent-stage execution binding: %w", err)
	}
	if persisted != binding {
		return fmt.Errorf("appended agent-stage execution binding differs from request")
	}
	return nil
}

func (repository *Repository) LookupAgentStageExecutionBinding(
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageExecutionBinding, bool, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageExecutionBinding{}, false, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	bindingID, err := runmodel.AgentStageExecutionBindingID(runID, intentID)
	if err != nil {
		return runmodel.AgentStageExecutionBinding{}, false, err
	}
	binding, err := repository.LoadAgentStageExecutionBinding(
		runID,
		bindingID,
		subject,
	)
	if errors.Is(err, os.ErrNotExist) {
		return runmodel.AgentStageExecutionBinding{}, false, nil
	}
	if err != nil {
		return runmodel.AgentStageExecutionBinding{}, false, err
	}
	if binding.IntentID != intentID {
		return runmodel.AgentStageExecutionBinding{}, false, fmt.Errorf(
			"agent-stage execution binding ID resolved a different intent",
		)
	}
	return binding, true, nil
}

func (repository *Repository) LoadAgentStageExecutionBinding(
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageExecutionBinding, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	if !strings.HasPrefix(bindingID, runID+"-") {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"agent-stage execution binding %q is not namespaced by run %q",
			bindingID,
			runID,
		)
	}
	bindings, err := repository.readAgentStageExecutionBindings(runID)
	if err != nil {
		return runmodel.AgentStageExecutionBinding{}, err
	}
	for _, binding := range bindings {
		if binding.BindingID != bindingID {
			continue
		}
		if binding.Subject != subject {
			return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
				"%w: run %q stage %q intent %q",
				ErrAgentStageExecutionBindingSubjectMismatch,
				binding.ReviewRunID,
				binding.Stage.ID,
				binding.IntentID,
			)
		}
		intent, err := repository.LoadAgentStageDispatchIntent(
			binding.ReviewRunID,
			binding.IntentID,
			subject,
		)
		if err != nil {
			return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
				"load exact intent for persisted execution binding: %w",
				err,
			)
		}
		if err := binding.ValidateAgainstIntent(intent); err != nil {
			return runmodel.AgentStageExecutionBinding{}, err
		}
		return binding, nil
	}
	return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
		"agent-stage execution binding %q in run %q: %w",
		bindingID,
		runID,
		os.ErrNotExist,
	)
}

func (repository *Repository) readAgentStageDispatchIntents(
	runID string,
) ([]runmodel.AgentStageDispatchIntent, error) {
	stream, err := agentStageDispatchIntentStream(runID)
	if err != nil {
		return nil, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []runmodel.AgentStageDispatchIntent{}, nil
		}
		return nil, fmt.Errorf("read agent-stage dispatch intent stream: %w", err)
	}
	admissions, err := repository.readAgentStagePlanAdmissions(runID)
	if err != nil {
		return nil, fmt.Errorf(
			"read plan admissions for agent-stage dispatch intents: %w",
			err,
		)
	}
	admissionsByID := make(map[string]runmodel.AgentStagePlanAdmission, len(admissions))
	for _, admission := range admissions {
		admissionsByID[admission.AdmissionID] = admission
	}
	intents := make([]runmodel.AgentStageDispatchIntent, 0, len(envelopes))
	for _, envelope := range envelopes {
		intent, err := decodeAgentStageDispatchIntent(envelope, runID)
		if err != nil {
			return nil, err
		}
		admission, ok := admissionsByID[intent.AdmissionID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage dispatch intent %q has no exact plan admission",
				intent.IntentID,
			)
		}
		if err := intent.ValidateAgainstAdmission(admission); err != nil {
			return nil, fmt.Errorf(
				"validate persisted agent-stage dispatch intent %q: %w",
				intent.IntentID,
				err,
			)
		}
		intents = append(intents, intent)
	}
	return intents, nil
}

func (repository *Repository) readAgentStageExecutionBindings(
	runID string,
) ([]runmodel.AgentStageExecutionBinding, error) {
	stream, err := agentStageExecutionBindingStream(runID)
	if err != nil {
		return nil, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []runmodel.AgentStageExecutionBinding{}, nil
		}
		return nil, fmt.Errorf("read agent-stage execution binding stream: %w", err)
	}
	intents, err := repository.readAgentStageDispatchIntents(runID)
	if err != nil {
		return nil, fmt.Errorf(
			"read dispatch intents for agent-stage execution bindings: %w",
			err,
		)
	}
	intentsByID := make(map[string]runmodel.AgentStageDispatchIntent, len(intents))
	for _, intent := range intents {
		intentsByID[intent.IntentID] = intent
	}
	bindings := make([]runmodel.AgentStageExecutionBinding, 0, len(envelopes))
	for _, envelope := range envelopes {
		binding, err := decodeAgentStageExecutionBinding(envelope, runID)
		if err != nil {
			return nil, err
		}
		intent, ok := intentsByID[binding.IntentID]
		if !ok {
			return nil, fmt.Errorf(
				"agent-stage execution binding %q has no exact dispatch intent",
				binding.BindingID,
			)
		}
		if err := binding.ValidateAgainstIntent(intent); err != nil {
			return nil, fmt.Errorf(
				"validate persisted agent-stage execution binding %q: %w",
				binding.BindingID,
				err,
			)
		}
		if err := repository.verifyAgentStageDispatchClaim(intent); err != nil {
			return nil, fmt.Errorf(
				"verify dispatch claim for persisted execution binding %q: %w",
				binding.BindingID,
				err,
			)
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

func decodeAgentStageDispatchIntent(
	envelope local.Envelope,
	runID string,
) (runmodel.AgentStageDispatchIntent, error) {
	if envelope.Schema != runmodel.AgentStageDispatchIntentSchemaVersion {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"unsupported agent-stage dispatch intent event schema %q",
			envelope.Schema,
		)
	}
	var intent runmodel.AgentStageDispatchIntent
	if err := decodeStrictJSON(envelope.Payload, &intent); err != nil {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"decode agent-stage dispatch intent %q: %w",
			envelope.ID,
			err,
		)
	}
	if intent.ReviewRunID != runID || !strings.HasPrefix(envelope.ID, runID+"-") {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"agent-stage dispatch intent event %q is not namespaced by run %q",
			envelope.ID,
			runID,
		)
	}
	if envelope.ID != intent.IntentID {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"agent-stage dispatch intent event ID does not match its payload",
		)
	}
	if !envelope.Time.Equal(intent.RecordedAt) {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"agent-stage dispatch intent event time does not match recorded_at",
		)
	}
	if err := intent.Validate(); err != nil {
		return runmodel.AgentStageDispatchIntent{}, fmt.Errorf(
			"validate agent-stage dispatch intent %q: %w",
			envelope.ID,
			err,
		)
	}
	return intent, nil
}

func decodeAgentStageExecutionBinding(
	envelope local.Envelope,
	runID string,
) (runmodel.AgentStageExecutionBinding, error) {
	if envelope.Schema != runmodel.AgentStageExecutionBindingSchemaVersion {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"unsupported agent-stage execution binding event schema %q",
			envelope.Schema,
		)
	}
	var binding runmodel.AgentStageExecutionBinding
	if err := decodeStrictJSON(envelope.Payload, &binding); err != nil {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"decode agent-stage execution binding %q: %w",
			envelope.ID,
			err,
		)
	}
	if binding.ReviewRunID != runID || !strings.HasPrefix(envelope.ID, runID+"-") {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"agent-stage execution binding event %q is not namespaced by run %q",
			envelope.ID,
			runID,
		)
	}
	if envelope.ID != binding.BindingID {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"agent-stage execution binding event ID does not match its payload",
		)
	}
	if !envelope.Time.Equal(binding.RecordedAt) {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"agent-stage execution binding event time does not match recorded_at",
		)
	}
	if err := binding.Validate(); err != nil {
		return runmodel.AgentStageExecutionBinding{}, fmt.Errorf(
			"validate agent-stage execution binding %q: %w",
			envelope.ID,
			err,
		)
	}
	return binding, nil
}

func agentStageDispatchIntentStream(runID string) (string, error) {
	if _, err := runmodel.AgentStageDispatchIntentID(
		runID,
		"namespace-check",
		1,
		1,
	); err != nil {
		return "", err
	}
	return "runs/" + runID + "/agent-stage-dispatch-intents", nil
}

func agentStageExecutionBindingStream(runID string) (string, error) {
	if _, err := runmodel.AgentStageDispatchIntentID(
		runID,
		"namespace-check",
		1,
		1,
	); err != nil {
		return "", err
	}
	return "runs/" + runID + "/agent-stage-execution-bindings", nil
}

func (repository *Repository) verifyAgentStageDispatchClaim(
	intent runmodel.AgentStageDispatchIntent,
) error {
	claimed, err := repository.isAgentStageDispatchClaimed(intent)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("load exact agent-stage dispatch claim: %w", os.ErrNotExist)
	}
	return nil
}
