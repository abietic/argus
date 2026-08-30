package runrepo

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const agentStageGenerationDecisionSchemaVersion = "argus.agent_stage_generation_decision.v1alpha1"

type agentStageGenerationDecisionKind string

const (
	agentStageGenerationDispatchClaimed  agentStageGenerationDecisionKind = "dispatch_claimed"
	agentStageGenerationTerminalReserved agentStageGenerationDecisionKind = "terminal_reserved"
)

// agentStageGenerationDecision is the shared compare-and-swap authority for
// provider dispatch and terminal admission. Dispatch intents and terminal
// projections remain separate append-only facts, but neither side may win its
// external authority without first advancing this one stream.
//
// The exact sealed intent/gate is embedded deliberately. A coordinator event
// is therefore independently self-validating even if a process crashes after
// this CAS and before a downstream projection append.
type agentStageGenerationDecision struct {
	SchemaVersion string                             `json:"schema_version"`
	Kind          agentStageGenerationDecisionKind   `json:"kind"`
	Intent        *runmodel.AgentStageDispatchIntent `json:"dispatch_intent,omitempty"`
	Gate          *runmodel.AgentStageTerminalGate   `json:"terminal_gate,omitempty"`
}

func (decision agentStageGenerationDecision) validate(runID string) error {
	if decision.SchemaVersion != agentStageGenerationDecisionSchemaVersion {
		return fmt.Errorf("unsupported agent-stage generation decision schema %q", decision.SchemaVersion)
	}
	switch decision.Kind {
	case agentStageGenerationDispatchClaimed:
		if decision.Intent == nil || decision.Gate != nil {
			return fmt.Errorf("dispatch generation decision requires only dispatch_intent")
		}
		if err := decision.Intent.Validate(); err != nil {
			return fmt.Errorf("validate coordinated dispatch intent: %w", err)
		}
		if decision.Intent.ReviewRunID != runID {
			return fmt.Errorf("coordinated dispatch intent belongs to another run")
		}
	case agentStageGenerationTerminalReserved:
		if decision.Gate == nil || decision.Intent != nil {
			return fmt.Errorf("terminal generation decision requires only terminal_gate")
		}
		if err := decision.Gate.Validate(); err != nil {
			return fmt.Errorf("validate coordinated terminal gate: %w", err)
		}
		if decision.Gate.ReviewRunID != runID {
			return fmt.Errorf("coordinated terminal gate belongs to another run")
		}
	default:
		return fmt.Errorf("unsupported agent-stage generation decision kind %q", decision.Kind)
	}
	return nil
}

func (decision agentStageGenerationDecision) eventID() string {
	if decision.Intent != nil {
		return decision.Intent.IntentID + "-generation-claim"
	}
	if decision.Gate != nil {
		return decision.Gate.GateID + "-generation-terminal"
	}
	return ""
}

func (decision agentStageGenerationDecision) eventTime() (timeValue time.Time) {
	if decision.Intent != nil {
		return decision.Intent.RecordedAt
	}
	if decision.Gate != nil {
		return decision.Gate.EventTime()
	}
	return timeValue
}

func (repository *Repository) coordinateAgentStageDispatchClaim(
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	decision := agentStageGenerationDecision{
		SchemaVersion: agentStageGenerationDecisionSchemaVersion,
		Kind:          agentStageGenerationDispatchClaimed,
		Intent:        &intent,
	}
	for retry := 0; retry < 128; retry++ {
		decisions, err := repository.readAgentStageGenerationDecisions(intent.ReviewRunID)
		if err != nil {
			return false, err
		}
		if err := repository.validateAgentStageRetryDispatch(decisions, intent); err != nil {
			return false, err
		}
		alreadyClaimed, err := validateDispatchGenerationTransition(decisions, intent)
		if err != nil || alreadyClaimed {
			return false, err
		}
		stream, err := agentStageGenerationDecisionStream(intent.ReviewRunID)
		if err != nil {
			return false, err
		}
		_, appended, err := repository.store.AppendJSONLAtSequenceWithStatus(
			stream,
			uint64(len(decisions)),
			local.Event{
				ID: decision.eventID(), Schema: agentStageGenerationDecisionSchemaVersion,
				Time: decision.eventTime(), Payload: decision,
			},
		)
		if err == nil {
			return appended, nil
		}
		if !errors.Is(err, local.ErrEventConflict) {
			return false, fmt.Errorf("advance agent-stage dispatch generation CAS: %w", err)
		}
	}
	return false, fmt.Errorf("agent-stage dispatch generation CAS did not converge")
}

func (repository *Repository) reserveAgentStageTerminalGate(
	gate runmodel.AgentStageTerminalGate,
) error {
	decision := agentStageGenerationDecision{
		SchemaVersion: agentStageGenerationDecisionSchemaVersion,
		Kind:          agentStageGenerationTerminalReserved,
		Gate:          &gate,
	}
	for retry := 0; retry < 128; retry++ {
		decisions, err := repository.readAgentStageGenerationDecisions(gate.ReviewRunID)
		if err != nil {
			return err
		}
		alreadyReserved, err := validateTerminalGenerationTransition(decisions, gate)
		if err != nil {
			return err
		}
		if alreadyReserved {
			return nil
		}
		stream, err := agentStageGenerationDecisionStream(gate.ReviewRunID)
		if err != nil {
			return err
		}
		_, err = repository.store.AppendJSONLAtSequence(
			stream,
			uint64(len(decisions)),
			local.Event{
				ID: decision.eventID(), Schema: agentStageGenerationDecisionSchemaVersion,
				Time: decision.eventTime(), Payload: decision,
			},
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, local.ErrEventConflict) {
			return fmt.Errorf("advance agent-stage terminal generation CAS: %w", err)
		}
	}
	return fmt.Errorf("agent-stage terminal generation CAS did not converge")
}

func validateDispatchGenerationTransition(
	decisions []agentStageGenerationDecision,
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	var head *runmodel.AgentStageDispatchIntent
	for _, decision := range decisions {
		if decision.Intent != nil {
			candidate := decision.Intent
			if candidate.IntentID == intent.IntentID {
				if *candidate != intent {
					return false, fmt.Errorf("coordinated dispatch claim differs from exact intent: %w", local.ErrEventConflict)
				}
				return true, nil
			}
			if sameAgentStageWorkload(*candidate, intent) &&
				(head == nil || candidate.Generation > head.Generation) {
				head = candidate
			}
			continue
		}
		gate := decision.Gate
		if gate.IntentID == intent.IntentID {
			return false, fmt.Errorf("agent-stage dispatch intent already has terminal authority: %w", local.ErrEventConflict)
		}
		if sameAgentStageGateWorkload(*gate, intent) &&
			gate.Kind != runmodel.AgentStageCancellationRequested {
			if gate.Kind != runmodel.AgentStageFailedResultAccepted || gate.Outcome == nil ||
				!gate.Outcome.Failure.Retryable || intent.Attempt != gate.Attempt+1 {
				return false, fmt.Errorf("agent-stage workload already has terminal completion at generation %d: %w", gate.Generation, local.ErrEventConflict)
			}
		}
	}
	if head == nil {
		return false, nil
	}
	if intent.Generation < head.Generation {
		return false, fmt.Errorf("dispatch generation %d is stale behind coordinated generation %d: %w", intent.Generation, head.Generation, local.ErrEventConflict)
	}
	if intent.Generation == head.Generation {
		return false, fmt.Errorf("dispatch generation %d already belongs to intent %q: %w", intent.Generation, head.IntentID, local.ErrEventConflict)
	}
	return false, nil
}

func (repository *Repository) validateAgentStageRetryDispatch(
	decisions []agentStageGenerationDecision,
	intent runmodel.AgentStageDispatchIntent,
) error {
	if intent.Attempt == 1 {
		return nil
	}
	admission, err := repository.LoadAgentStagePlanAdmission(
		intent.ReviewRunID, intent.AdmissionID, intent.Subject,
	)
	if err != nil {
		return fmt.Errorf("load retry-bound AgentStagePlan admission: %w", err)
	}
	planBytes, err := repository.ReadArtifact(admission.Plan.Local)
	if err != nil {
		return fmt.Errorf("read retry-bound AgentStagePlan: %w", err)
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(planBytes)
	if err != nil {
		return fmt.Errorf("decode retry-bound AgentStagePlan: %w", err)
	}
	if intent.Attempt < 1 || intent.Attempt > plan.Retry.MaxAttempts {
		return fmt.Errorf("dispatch attempt %d exceeds admitted retry limit %d: %w", intent.Attempt, plan.Retry.MaxAttempts, local.ErrEventConflict)
	}
	var prior *runmodel.AgentStageTerminalGate
	for _, decision := range decisions {
		if decision.Gate == nil {
			continue
		}
		candidate := decision.Gate
		if !sameAgentStageGateWorkload(*candidate, intent) ||
			candidate.Attempt != intent.Attempt-1 {
			continue
		}
		if prior == nil || candidate.Generation > prior.Generation {
			prior = candidate
		}
	}
	if prior == nil || prior.Kind != runmodel.AgentStageFailedResultAccepted ||
		prior.Outcome == nil || !prior.Outcome.Failure.Retryable ||
		prior.AdmissionID != intent.AdmissionID ||
		prior.AdmissionSHA256 != intent.AdmissionSHA256 ||
		!slices.Contains(plan.Retry.RetryableCodes, prior.Outcome.Failure.Code) ||
		intent.Generation <= prior.Generation {
		return fmt.Errorf("dispatch attempt %d has no policy-authorized prior failure: %w", intent.Attempt, local.ErrEventConflict)
	}
	return nil
}

func validateTerminalGenerationTransition(
	decisions []agentStageGenerationDecision,
	gate runmodel.AgentStageTerminalGate,
) (bool, error) {
	var head *runmodel.AgentStageDispatchIntent
	claimed := false
	for _, decision := range decisions {
		if decision.Intent != nil {
			candidate := decision.Intent
			if candidate.IntentID == gate.IntentID {
				claimed = true
			}
			if sameAgentStageGateWorkload(gate, *candidate) &&
				(head == nil || candidate.Generation > head.Generation) {
				head = candidate
			}
			continue
		}
		candidate := decision.Gate
		if candidate.IntentID == gate.IntentID {
			if agentStageTerminalGatesEqual(*candidate, gate) {
				return true, nil
			}
			return false, fmt.Errorf("agent-stage terminal generation authority differs from existing winner: %w", local.ErrEventConflict)
		}
		if sameAgentStageGateCoordinate(*candidate, gate) &&
			candidate.Kind != runmodel.AgentStageCancellationRequested {
			if candidate.Kind != runmodel.AgentStageFailedResultAccepted ||
				candidate.Outcome == nil || !candidate.Outcome.Failure.Retryable ||
				candidate.Attempt >= gate.Attempt {
				return false, fmt.Errorf("agent-stage workload already has terminal completion at generation %d: %w", candidate.Generation, local.ErrEventConflict)
			}
		}
	}
	if gate.Kind == runmodel.AgentStageCancellationRequested {
		return false, nil
	}
	if !claimed {
		return false, fmt.Errorf("terminal completion has no coordinated dispatch claim")
	}
	if head == nil || head.IntentID != gate.IntentID || head.Generation != gate.Generation {
		generation := 0
		if head != nil {
			generation = head.Generation
		}
		return false, fmt.Errorf("terminal completion generation %d is stale; coordinated dispatch generation is %d", gate.Generation, generation)
	}
	return false, nil
}

func (repository *Repository) isAgentStageDispatchClaimed(
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	decisions, err := repository.readAgentStageGenerationDecisions(intent.ReviewRunID)
	if err != nil {
		return false, err
	}
	for _, decision := range decisions {
		if decision.Intent == nil || decision.Intent.IntentID != intent.IntentID {
			continue
		}
		if *decision.Intent != intent {
			return false, fmt.Errorf("coordinated dispatch claim differs from exact intent: %w", local.ErrEventConflict)
		}
		return true, nil
	}
	return false, nil
}

func (repository *Repository) readAgentStageGenerationDecisions(
	runID string,
) ([]agentStageGenerationDecision, error) {
	stream, err := agentStageGenerationDecisionStream(runID)
	if err != nil {
		return nil, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []agentStageGenerationDecision{}, nil
		}
		return nil, fmt.Errorf("read agent-stage generation coordinator: %w", err)
	}
	decisions := make([]agentStageGenerationDecision, 0, len(envelopes))
	for _, envelope := range envelopes {
		if envelope.Schema != agentStageGenerationDecisionSchemaVersion {
			return nil, fmt.Errorf("unsupported agent-stage generation event schema %q", envelope.Schema)
		}
		var decision agentStageGenerationDecision
		if err := decodeStrictJSON(envelope.Payload, &decision); err != nil {
			return nil, fmt.Errorf("decode agent-stage generation decision %q: %w", envelope.ID, err)
		}
		if err := decision.validate(runID); err != nil {
			return nil, fmt.Errorf("validate agent-stage generation decision %q: %w", envelope.ID, err)
		}
		if err := repository.validateAgentStageGenerationDecisionSource(decision); err != nil {
			return nil, fmt.Errorf("validate agent-stage generation decision %q source: %w", envelope.ID, err)
		}
		if envelope.ID != decision.eventID() || !envelope.Time.Equal(decision.eventTime()) {
			return nil, fmt.Errorf("agent-stage generation event envelope does not match payload")
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func (repository *Repository) validateAgentStageGenerationDecisionSource(
	decision agentStageGenerationDecision,
) error {
	if decision.Intent != nil {
		persisted, err := repository.LoadAgentStageDispatchIntent(
			decision.Intent.ReviewRunID,
			decision.Intent.IntentID,
			decision.Intent.Subject,
		)
		if err != nil {
			return fmt.Errorf("load coordinated dispatch intent: %w", err)
		}
		if persisted != *decision.Intent {
			return fmt.Errorf("coordinated dispatch claim differs from authoritative intent")
		}
		return nil
	}
	gate := *decision.Gate
	intent, err := repository.LoadAgentStageDispatchIntent(
		gate.ReviewRunID,
		gate.IntentID,
		gate.Subject,
	)
	if err != nil {
		return fmt.Errorf("load coordinated terminal intent: %w", err)
	}
	admission, err := repository.LoadAgentStagePlanAdmission(
		gate.ReviewRunID,
		gate.AdmissionID,
		gate.Subject,
	)
	if err != nil {
		return fmt.Errorf("load coordinated terminal admission: %w", err)
	}
	if err := gate.ValidateAgainstAdmissionAndIntent(admission, intent); err != nil {
		return fmt.Errorf("bind coordinated terminal gate to admission and intent: %w", err)
	}
	return nil
}

func agentStageGenerationDecisionStream(runID string) (string, error) {
	if _, err := runmodel.AgentStageDispatchIntentID(runID, "namespace-check", 1, 1); err != nil {
		return "", err
	}
	return "runs/" + runID + "/agent-stage-generation-decisions", nil
}

func sameAgentStageWorkload(
	left runmodel.AgentStageDispatchIntent,
	right runmodel.AgentStageDispatchIntent,
) bool {
	return left.Stage.ID == right.Stage.ID && left.WorkloadID == right.WorkloadID
}

func sameAgentStageGateWorkload(
	gate runmodel.AgentStageTerminalGate,
	intent runmodel.AgentStageDispatchIntent,
) bool {
	return gate.Stage.ID == intent.Stage.ID && gate.WorkloadID == intent.WorkloadID
}

func sameAgentStageGateCoordinate(
	left runmodel.AgentStageTerminalGate,
	right runmodel.AgentStageTerminalGate,
) bool {
	return left.Stage.ID == right.Stage.ID && left.WorkloadID == right.WorkloadID
}
