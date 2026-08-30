package runrepo

import (
	"fmt"
	"slices"
	"time"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/targetmodel"
	"argus.local/argus/internal/workflow"
)

// DeterministicRecovery is the exact append-only prefix from which a scope
// ReviewRun may continue. Recovery is deliberately narrow to deterministic
// scope reviews with a frozen shard manifest, but it closes crash windows at
// every stage of that workflow.
type DeterministicRecovery struct {
	Snapshot      runmodel.ExecutionSnapshot
	CreatedAt     time.Time
	StartedAt     time.Time
	StageAttempts []runmodel.StageAttempt
	Bindings      []runmodel.PlatformExecutionBinding
	Evidence      []runmodel.RunEvidence
	Upstream      []reviewcore.StageResult
	UpstreamRefs  []runmodel.ArtifactRef
	StartStage    reviewcore.StageName
	MaxGeneration int
}

type recoveryCoordinate struct {
	binding  *runmodel.PlatformExecutionBinding
	started  *persistedRunEvent
	terminal *persistedRunEvent
	evidence *runmodel.RunEvidence
}

func (repository *Repository) RecoverScopeReview(
	runID string,
	recoveredAt time.Time,
) (DeterministicRecovery, error) {
	if recoveredAt.IsZero() || recoveredAt.Location() != time.UTC {
		return DeterministicRecovery{}, fmt.Errorf("recovered_at must be a non-zero UTC timestamp")
	}
	events, err := repository.readRunEvents(runID)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	if len(events) < 2 {
		return DeterministicRecovery{}, fmt.Errorf("run %q has no recoverable lifecycle", runID)
	}
	if recoveredAt.Before(events[len(events)-1].Envelope.Time) {
		return DeterministicRecovery{}, fmt.Errorf("recovered_at predates the latest durable run fact")
	}
	if _, terminal, err := firstTerminalEvent(events); err != nil {
		return DeterministicRecovery{}, err
	} else if terminal {
		return DeterministicRecovery{}, fmt.Errorf("run %q is already terminal", runID)
	}
	created := events[0]
	if created.Event.EventType != EventRunCreated || created.Event.Kind != runmodel.RunKindReview {
		return DeterministicRecovery{}, fmt.Errorf("run %q does not begin with review run.created", runID)
	}
	startedIndex := slices.IndexFunc(events, func(event persistedRunEvent) bool {
		return event.Event.EventType == EventRunStarted
	})
	if startedIndex < 0 {
		return DeterministicRecovery{}, fmt.Errorf("run %q has no run.started fact", runID)
	}
	started := events[startedIndex]
	snapshot, err := repository.ExecutionSnapshotForRun(runID)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	if snapshot.ReviewShardManifestRef == nil {
		return DeterministicRecovery{}, fmt.Errorf("run %q has no frozen scope shard manifest", runID)
	}
	inputData, err := repository.ReadArtifact(snapshot.ReviewInputRef)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	input, err := reviewcore.DecodeReviewInput(inputData)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	if input.TargetMode != reviewcore.TargetModeScope {
		return DeterministicRecovery{}, fmt.Errorf("run %q is not a scope review", runID)
	}
	var target targetmodel.MaterializedTarget
	if err := repository.ReadJSONArtifact(snapshot.TargetSnapshotRef, &target); err != nil {
		return DeterministicRecovery{}, err
	}
	if err := target.Validate(); err != nil {
		return DeterministicRecovery{}, err
	}
	definitionData, err := repository.ReadArtifact(snapshot.WorkflowDefinitionRef)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	var definition workflow.Definition
	if err := decodeStrictJSON(definitionData, &definition); err != nil {
		return DeterministicRecovery{}, err
	}
	order, err := definition.TopologicalOrder()
	if err != nil {
		return DeterministicRecovery{}, err
	}

	coordinates := make(map[string]*recoveryCoordinate)
	bindingOrder := make([]string, 0)
	for index := range events {
		persisted := events[index]
		event := persisted.Event
		if event.StageID == "" {
			continue
		}
		key := stageCoordinate(event.StageID, event.Attempt, event.Generation)
		coordinate := coordinates[key]
		if coordinate == nil {
			coordinate = &recoveryCoordinate{}
			coordinates[key] = coordinate
		}
		switch event.EventType {
		case EventBindingRecorded:
			if coordinate.binding != nil {
				return DeterministicRecovery{}, fmt.Errorf("duplicate binding at %s", key)
			}
			copy := *event.Binding
			coordinate.binding = &copy
			bindingOrder = append(bindingOrder, key)
		case EventStageStarted:
			if coordinate.started != nil {
				return DeterministicRecovery{}, fmt.Errorf("duplicate stage.started at %s", key)
			}
			copy := persisted
			coordinate.started = &copy
		case EventStageSucceeded, EventStageFailed, EventStageCanceled:
			if coordinate.terminal != nil {
				return DeterministicRecovery{}, fmt.Errorf("duplicate stage terminal at %s", key)
			}
			copy := persisted
			coordinate.terminal = &copy
		case EventEvidenceRecorded:
			if coordinate.evidence != nil {
				return DeterministicRecovery{}, fmt.Errorf("duplicate evidence at %s", key)
			}
			copy := *event.Evidence
			coordinate.evidence = &copy
		}
	}

	result := DeterministicRecovery{
		Snapshot: snapshot, CreatedAt: created.Envelope.Time, StartedAt: started.Envelope.Time,
		StageAttempts: []runmodel.StageAttempt{}, Bindings: []runmodel.PlatformExecutionBinding{},
		Evidence: []runmodel.RunEvidence{}, Upstream: []reviewcore.StageResult{},
		UpstreamRefs: []runmodel.ArtifactRef{},
	}
	stageIndex := 0
	var predecessorRef *runmodel.ArtifactRef
	for _, key := range bindingOrder {
		coordinate := coordinates[key]
		if coordinate.binding == nil {
			return DeterministicRecovery{}, fmt.Errorf("coordinate %s has no binding", key)
		}
		binding := *coordinate.binding
		result.Bindings = append(result.Bindings, binding)
		result.MaxGeneration = max(result.MaxGeneration, binding.Generation)
		if stageIndex >= len(order) || binding.StageID != order[stageIndex] {
			return DeterministicRecovery{}, fmt.Errorf("binding %q is outside the sequential workflow prefix", binding.BindingID)
		}
		if coordinate.started == nil {
			if err := repository.AppendEvent(
				fmt.Sprintf("%s-%s-%d-%d-started", runID, binding.StageID, binding.Attempt, binding.Generation),
				recoveredAt,
				RunEvent{RunID: runID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusRunning,
					EventType: EventStageStarted, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
					StageID: binding.StageID, Attempt: binding.Attempt, Generation: binding.Generation},
			); err != nil {
				return DeterministicRecovery{}, err
			}
			coordinate.started = &persistedRunEvent{Envelope: localEnvelopeAt(recoveredAt)}
		}
		startedAt := coordinate.started.Envelope.Time
		if coordinate.terminal == nil {
			failure := &runmodel.Failure{
				Code: "recovered_abandoned_attempt", Message: "prior process ended without a terminal stage fact",
				StageID: binding.StageID, Retryable: true,
			}
			if err := repository.AppendEvent(
				fmt.Sprintf("%s-%s-%d-%d-canceled", runID, binding.StageID, binding.Attempt, binding.Generation),
				recoveredAt,
				RunEvent{RunID: runID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusRunning,
					EventType: EventStageCanceled, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
					StageID: binding.StageID, Attempt: binding.Attempt, Generation: binding.Generation,
					Failure: failure},
			); err != nil {
				return DeterministicRecovery{}, err
			}
			coordinate.terminal = &persistedRunEvent{
				Envelope: localEnvelopeAt(recoveredAt),
				Event:    RunEvent{EventType: EventStageCanceled, Failure: failure},
			}
		}
		terminal := coordinate.terminal
		finishedAt := terminal.Envelope.Time
		inputRefs := []runmodel.ArtifactRef{snapshot.ReviewInputRef}
		if predecessorRef != nil {
			inputRefs = append(inputRefs, *predecessorRef)
		}
		attempt := runmodel.StageAttempt{
			StageID: binding.StageID, Attempt: binding.Attempt, Generation: binding.Generation,
			BindingID: binding.BindingID, InputRefs: inputRefs, StartedAt: startedAt,
			FinishedAt: &finishedAt, DurationMS: max(finishedAt.Sub(startedAt).Milliseconds(), 0),
		}
		switch terminal.Event.EventType {
		case EventStageSucceeded:
			if terminal.Event.Artifact == nil {
				return DeterministicRecovery{}, fmt.Errorf("succeeded coordinate %s has no artifact", key)
			}
			attempt.Status = runmodel.StageStatusSucceeded
			outputRef := *terminal.Event.Artifact
			attempt.OutputRef = &outputRef
			data, err := repository.ReadArtifact(outputRef)
			if err != nil {
				return DeterministicRecovery{}, err
			}
			stageResult, err := reviewcore.DecodeStageResult(data)
			if err != nil || string(stageResult.Stage) != binding.StageID {
				return DeterministicRecovery{}, fmt.Errorf("decode recovered %s output: %w", binding.StageID, err)
			}
			if coordinate.evidence == nil {
				completeness, notes := targetEvidenceCompleteness(target)
				evidence := runmodel.RunEvidence{
					EvidenceID: "recovery-evidence-" + binding.BindingID,
					BindingID:  binding.BindingID, StageID: binding.StageID,
					Attempt: binding.Attempt, Generation: binding.Generation,
					IdempotencyKey: binding.IdempotencyKey, FencingToken: binding.FencingToken,
					ArtifactRef: outputRef, Completeness: completeness,
					CompletenessNotes: notes, RecordedAt: finishedAt,
				}
				if err := repository.AppendEvent(
					fmt.Sprintf("%s-%s-%d-%d-evidence", runID, binding.StageID, binding.Attempt, binding.Generation),
					finishedAt,
					RunEvent{RunID: runID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusRunning,
						EventType: EventEvidenceRecorded, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
						StageID: binding.StageID, Attempt: binding.Attempt, Generation: binding.Generation,
						Artifact: &outputRef, Evidence: &evidence},
				); err != nil {
					return DeterministicRecovery{}, err
				}
				coordinate.evidence = &evidence
			}
			result.Evidence = append(result.Evidence, *coordinate.evidence)
			result.Upstream = append(result.Upstream, stageResult)
			result.UpstreamRefs = append(result.UpstreamRefs, outputRef)
			predecessorRef = &outputRef
			stageIndex++
		case EventStageFailed, EventStageCanceled:
			attempt.Status = runmodel.StageStatusFailed
			if terminal.Event.EventType == EventStageCanceled {
				attempt.Status = runmodel.StageStatusCanceled
			}
			if terminal.Event.Failure == nil {
				return DeterministicRecovery{}, fmt.Errorf("terminal coordinate %s has no failure", key)
			}
			attempt.ErrorCode = terminal.Event.Failure.Code
			attempt.ErrorMessage = terminal.Event.Failure.Message
			attempt.Retryable = terminal.Event.Failure.Retryable
		default:
			return DeterministicRecovery{}, fmt.Errorf("coordinate %s has invalid terminal", key)
		}
		result.StageAttempts = append(result.StageAttempts, attempt)
	}
	if stageIndex == len(order) {
		result.StartStage = ""
		return result, nil
	}
	result.StartStage = reviewcore.StageName(order[stageIndex])
	return result, nil
}

func stageCoordinate(stage string, attempt int, generation int) string {
	return fmt.Sprintf("%s:%d:%d", stage, attempt, generation)
}

// localEnvelopeAt supplies only the timestamp needed by the in-memory recovery
// projection after its corresponding fact has durably appended.
func localEnvelopeAt(at time.Time) local.Envelope {
	return local.Envelope{Time: at}
}
