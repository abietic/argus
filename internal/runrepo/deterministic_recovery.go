package runrepo

import (
	"fmt"
	"slices"
	"time"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/targetmodel"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// DeterministicRecovery is the exact append-only prefix from which a
// deterministic diff, selection, or sharded scope ReviewRun may continue.
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
	snapshot, err := repository.ExecutionSnapshotForRun(runID)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	if snapshot.ReviewShardManifestRef == nil {
		return DeterministicRecovery{}, fmt.Errorf("run %q has no frozen scope shard manifest", runID)
	}
	return repository.RecoverReview(runID, recoveredAt)
}

// RecoverReview repairs abandoned deterministic stage attempts using their
// immutable inputs and successful prefix. The caller owns the current
// scheduling lease and must validate its executable/configuration before this
// method appends recovery facts.
func (repository *Repository) RecoverReview(
	runID string,
	recoveredAt time.Time,
) (DeterministicRecovery, error) {
	return repository.RecoverReviewWithAuthority(runID, recoveredAt, nil)
}

// RecoverReviewWithAuthority verifies the caller's current lease before each
// recovery ledger write. This does not claim an atomic scheduler/store commit.
func (repository *Repository) RecoverReviewWithAuthority(
	runID string,
	recoveredAt time.Time,
	authority func() error,
) (DeterministicRecovery, error) {
	appendRecoveryEvent := func(id string, at time.Time, event RunEvent) error {
		if authority != nil {
			if err := authority(); err != nil {
				return err
			}
		}
		return repository.AppendEvent(id, at, event)
	}
	if recoveredAt.IsZero() || recoveredAt.Location() != time.UTC {
		return DeterministicRecovery{}, fmt.Errorf("recovered_at must be a non-zero UTC timestamp")
	}
	events, err := repository.readRunEvents(runID)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	if len(events) == 0 {
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
	if startedIndex < 0 && len(events) != 1 {
		return DeterministicRecovery{}, fmt.Errorf("run %q has execution facts without run.started", runID)
	}
	snapshot, err := repository.ExecutionSnapshotForRun(runID)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	inputData, err := repository.ReadArtifact(snapshot.ReviewInputRef)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	input, err := reviewcore.DecodeReviewInput(inputData)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	switch input.TargetMode {
	case reviewcore.TargetModeDiff, reviewcore.TargetModeSelection:
		if snapshot.ReviewShardManifestRef != nil {
			return DeterministicRecovery{}, fmt.Errorf("non-scope run carries a shard manifest")
		}
	case reviewcore.TargetModeScope:
		if snapshot.ReviewShardManifestRef == nil {
			return DeterministicRecovery{}, fmt.Errorf("run %q has no frozen scope shard manifest", runID)
		}
	default:
		return DeterministicRecovery{}, fmt.Errorf("unsupported deterministic recovery target %q", input.TargetMode)
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
	if err := validateDeterministicRecoveryDefinition(definition); err != nil {
		return DeterministicRecovery{}, err
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := repository.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return DeterministicRecovery{}, err
	}
	if err := spec.Validate(); err != nil {
		return DeterministicRecovery{}, fmt.Errorf("validate frozen recovery ReviewSpec: %w", err)
	}
	configData, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	config, err := reviewconfig.DecodeBundle(configData)
	if err != nil {
		return DeterministicRecovery{}, err
	}
	if spec.RequestID != runID || config.Context.InvocationID != runID ||
		string(spec.Target.Mode) != string(input.TargetMode) ||
		spec.WorkflowRef.SHA256 != snapshot.Workflow.SHA256 ||
		spec.WorkflowRef.ID != definition.ID || spec.WorkflowRef.Revision != definition.Revision ||
		config.Workflow.Definition.SHA256 != snapshot.Workflow.SHA256 ||
		spec.ConfigBundleRef.SHA256 != snapshot.ConfigBundleRef.SHA256 ||
		spec.ConfigBundleRef.ID != config.BundleID || spec.ConfigBundleRef.Revision != config.SHA256[:16] ||
		config.Context.OrganizationID != "local" ||
		config.Context.TenantID != spec.TenantID ||
		config.Context.RepositoryID != spec.Repository.RepositoryID {
		return DeterministicRecovery{}, fmt.Errorf("recovery snapshot does not bind review identity and configuration")
	}
	if spec.Target.Mode == contractsv1alpha1.ReviewModeSelection {
		if config.Context.Path != spec.Target.Selection.Path {
			return DeterministicRecovery{}, fmt.Errorf("recovery configuration path differs from selection")
		}
	} else if config.Context.Path != "" {
		return DeterministicRecovery{}, fmt.Errorf("repository-wide recovery requires an empty configuration path")
	}
	projection := runmodel.ReviewRun{
		RunID: runID, Kind: runmodel.RunKindReview,
		TargetMode:   runmodel.TargetMode(input.TargetMode),
		BaseRevision: target.Snapshot.Base.CommitOID, HeadRevision: target.Snapshot.Head.CommitOID,
		TargetSnapshotRef: snapshot.TargetSnapshotRef,
	}
	if err := repository.verifyMaterializedTargetClosure(target, spec, projection, input, config); err != nil {
		return DeterministicRecovery{}, fmt.Errorf("validate frozen recovery target: %w", err)
	}
	targetDigest, err := reviewcore.DigestReviewInput(input)
	if err != nil {
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
	started := persistedRunEvent{Envelope: localEnvelopeAt(recoveredAt)}
	if startedIndex >= 0 {
		started = events[startedIndex]
	} else {
		if err := appendRecoveryEvent(runID+"-started", recoveredAt, RunEvent{
			RunID: runID, Kind: runmodel.RunKindReview, Status: runmodel.RunStatusRunning,
			EventType: EventRunStarted, ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		}); err != nil {
			return DeterministicRecovery{}, err
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
			if err := appendRecoveryEvent(
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
			if err := appendRecoveryEvent(
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
			if stageResult.TargetDigest != targetDigest {
				return DeterministicRecovery{}, fmt.Errorf("recovered %s output targets another review input", binding.StageID)
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
				if err := appendRecoveryEvent(
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

func validateDeterministicRecoveryDefinition(definition workflow.Definition) error {
	if err := definition.Validate(); err != nil {
		return err
	}
	expected := workflow.DefaultReviewDefinition()
	if len(definition.Stages) != len(expected.Stages) {
		return fmt.Errorf("recovery requires the complete deterministic review workflow")
	}
	for index, stage := range definition.Stages {
		want := expected.Stages[index]
		if stage.ID != want.ID || stage.Kind != want.Kind ||
			stage.ImplementationRevision != want.ImplementationRevision ||
			stage.InputContract != want.InputContract || stage.OutputContract != want.OutputContract ||
			!slices.Equal(stage.DependsOn, want.DependsOn) || stage.Executor != want.Executor ||
			len(stage.RequiredCapabilities) != 0 || stage.AuthorityCeiling != nil ||
			stage.Retry.Jitter || stage.Retry.UnknownOutcome != want.Retry.UnknownOutcome ||
			stage.FailurePolicy != want.FailurePolicy || stage.SideEffect != want.SideEffect ||
			stage.ReplayPolicy != want.ReplayPolicy {
			return fmt.Errorf("recovery stage %q is not a supported deterministic implementation", stage.ID)
		}
	}
	return nil
}

func stageCoordinate(stage string, attempt int, generation int) string {
	return fmt.Sprintf("%s:%d:%d", stage, attempt, generation)
}

// localEnvelopeAt supplies only the timestamp needed by the in-memory recovery
// projection after its corresponding fact has durably appended.
func localEnvelopeAt(at time.Time) local.Envelope {
	return local.Envelope{Time: at}
}
