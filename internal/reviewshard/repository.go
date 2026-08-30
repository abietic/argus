package reviewshard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

type Repository struct {
	store *local.Store
	mu    *sync.Mutex
}

var repositoryLocks sync.Map

func NewRepository(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	lock, _ := repositoryLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &Repository{store: store, mu: lock.(*sync.Mutex)}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Create(
	ctx context.Context,
	manifest Manifest,
	mutation Mutation,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := manifest.Validate(); err != nil {
		return Record{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Record{}, err
	}
	if !manifest.CreatedAt.Equal(mutation.At) {
		return Record{}, fmt.Errorf("manifest created_at must equal mutation time")
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return Record{}, err
	}
	manifestRef, err := repository.putArtifact(runmodel.ContractReviewShardManifest, data)
	if err != nil {
		return Record{}, err
	}
	event := shardEvent{
		SchemaVersion: eventSchemaVersion, Type: eventPlanRecorded,
		RunID: manifest.RunID, ManifestRef: &manifestRef,
		Actor: mutation.Actor, Audit: mutation.Audit, OccurredAt: mutation.At,
	}
	return repository.append(ctx, mutation.IdempotencyKey, event)
}

func (repository *Repository) BindGeneration(
	ctx context.Context,
	binding GenerationBinding,
	mutation Mutation,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := binding.Validate(); err != nil {
		return Record{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Record{}, err
	}
	if !binding.BoundAt.Equal(mutation.At) {
		return Record{}, fmt.Errorf("generation bound_at must equal mutation time")
	}
	event := shardEvent{
		SchemaVersion: eventSchemaVersion, Type: eventGenerationBound,
		RunID: binding.RunID, Generation: clonePointer(binding),
		Actor: mutation.Actor, Audit: mutation.Audit, OccurredAt: mutation.At,
	}
	return repository.append(ctx, mutation.IdempotencyKey, event)
}

func (repository *Repository) RecordCheckpoint(
	ctx context.Context,
	checkpoint Checkpoint,
	mutation Mutation,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := checkpoint.Validate(); err != nil {
		return Record{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Record{}, err
	}
	if !checkpoint.CompletedAt.Equal(mutation.At) {
		return Record{}, fmt.Errorf("checkpoint completed_at must equal mutation time")
	}
	event := shardEvent{
		SchemaVersion: eventSchemaVersion, Type: eventCheckpointRecorded,
		RunID: checkpoint.RunID, Checkpoint: clonePointer(checkpoint),
		Actor: mutation.Actor, Audit: mutation.Audit, OccurredAt: mutation.At,
	}
	return repository.append(ctx, mutation.IdempotencyKey, event)
}

func (repository *Repository) Cancel(
	ctx context.Context,
	runID string,
	reason string,
	mutation Mutation,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := validateID("run_id", runID); err != nil {
		return Record{}, err
	}
	if err := validateText("cancel_reason", reason, 1024, false); err != nil {
		return Record{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Record{}, err
	}
	event := shardEvent{
		SchemaVersion: eventSchemaVersion, Type: eventPlanCanceled,
		RunID: runID, CancelReason: reason,
		Actor: mutation.Actor, Audit: mutation.Audit, OccurredAt: mutation.At,
	}
	return repository.append(ctx, mutation.IdempotencyKey, event)
}

func (repository *Repository) Complete(
	ctx context.Context,
	runID string,
	mutation Mutation,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := validateID("run_id", runID); err != nil {
		return Record{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Record{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	if prior, exists := state.events[mutation.IdempotencyKey]; exists {
		if prior.Type != eventAggregateRecorded || prior.RunID != runID ||
			prior.Actor != mutation.Actor || prior.Audit != mutation.Audit ||
			!prior.OccurredAt.Equal(mutation.At) {
			return Record{}, fmt.Errorf("%w: idempotency key reused", ErrConflict)
		}
		return state.record(runID)
	}
	record, err := state.record(runID)
	if err != nil {
		return Record{}, err
	}
	aggregate, err := buildAggregate(record, mutation)
	if err != nil {
		return Record{}, err
	}
	data, err := json.Marshal(aggregate)
	if err != nil {
		return Record{}, err
	}
	ref, err := repository.putArtifact(runmodel.ContractReviewShardAggregate, data)
	if err != nil {
		return Record{}, err
	}
	event := shardEvent{
		SchemaVersion: eventSchemaVersion, Type: eventAggregateRecorded,
		RunID: runID, AggregateRef: &ref,
		Actor: mutation.Actor, Audit: mutation.Audit, OccurredAt: mutation.At,
	}
	return repository.appendLocked(ctx, mutation.IdempotencyKey, event, state)
}

func (repository *Repository) Get(runID string) (Record, error) {
	if err := validateID("run_id", runID); err != nil {
		return Record{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	return state.record(runID)
}

func (repository *Repository) append(
	ctx context.Context,
	id string,
	event shardEvent,
) (Record, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	return repository.appendLocked(ctx, id, event, state)
}

func (repository *Repository) appendLocked(
	ctx context.Context,
	id string,
	event shardEvent,
	state *projection,
) (Record, error) {
	for attempts := 0; attempts < 16; attempts++ {
		if err := checkContext(ctx); err != nil {
			return Record{}, err
		}
		if prior, exists := state.events[id]; exists {
			if !reflect.DeepEqual(prior, event) {
				return Record{}, fmt.Errorf("%w: idempotency key reused", ErrConflict)
			}
			return state.record(event.RunID)
		}
		candidate := state.clone()
		if err := repository.apply(candidate, event); err != nil {
			return Record{}, err
		}
		_, appended, err := repository.store.AppendJSONLAtSequenceWithStatus(
			eventStream,
			state.sequence,
			local.Event{ID: id, Schema: eventSchemaVersion, Time: event.OccurredAt, Payload: event},
		)
		if err == nil {
			if !appended {
				reloaded, loadErr := repository.load()
				if loadErr != nil {
					return Record{}, loadErr
				}
				return reloaded.record(event.RunID)
			}
			return candidate.record(event.RunID)
		}
		if !errors.Is(err, local.ErrEventConflict) {
			return Record{}, err
		}
		state, err = repository.load()
		if err != nil {
			return Record{}, err
		}
	}
	return Record{}, fmt.Errorf("%w: concurrent shard mutations did not converge", ErrConflict)
}

type projection struct {
	sequence uint64
	events   map[string]shardEvent
	records  map[string]Record
}

func newProjection() *projection {
	return &projection{events: map[string]shardEvent{}, records: map[string]Record{}}
}

func (state *projection) clone() *projection {
	copy := newProjection()
	copy.sequence = state.sequence
	for id, event := range state.events {
		copy.events[id] = cloneValue(event)
	}
	for id, record := range state.records {
		copy.records[id] = cloneValue(record)
	}
	return copy
}

func (state *projection) record(runID string) (Record, error) {
	record, exists := state.records[runID]
	if !exists {
		return Record{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	return cloneValue(record), nil
}

func (repository *Repository) load() (*projection, error) {
	state := newProjection()
	envelopes, err := repository.store.ReadJSONL(eventStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	for _, envelope := range envelopes {
		if envelope.Schema != eventSchemaVersion {
			return nil, fmt.Errorf("unsupported shard event envelope schema %q", envelope.Schema)
		}
		event, err := decodeStrict(envelope.Payload, validateEvent)
		if err != nil {
			return nil, fmt.Errorf("decode shard event %q: %w", envelope.ID, err)
		}
		if !event.OccurredAt.Equal(envelope.Time) {
			return nil, fmt.Errorf("shard event %q time differs from envelope", envelope.ID)
		}
		if _, duplicate := state.events[envelope.ID]; duplicate {
			return nil, fmt.Errorf("duplicate shard event id %q", envelope.ID)
		}
		if err := repository.apply(state, event); err != nil {
			return nil, fmt.Errorf("project shard event %q: %w", envelope.ID, err)
		}
		state.events[envelope.ID] = event
		state.sequence = envelope.Sequence
	}
	return state, nil
}

func (repository *Repository) apply(state *projection, event shardEvent) error {
	if err := validateEvent(event); err != nil {
		return err
	}
	switch event.Type {
	case eventPlanRecorded:
		if _, exists := state.records[event.RunID]; exists {
			return fmt.Errorf("%w: run already has a shard plan", ErrConflict)
		}
		manifest, err := repository.loadManifest(*event.ManifestRef)
		if err != nil {
			return err
		}
		if manifest.RunID != event.RunID || !manifest.CreatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("manifest does not bind plan event")
		}
		if err := repository.verifyManifestClosure(manifest); err != nil {
			return err
		}
		state.records[event.RunID] = Record{
			Manifest: manifest, ManifestRef: *event.ManifestRef,
			CheckpointHistory: []Checkpoint{}, Completed: []Checkpoint{},
			PendingShardIDs: shardIDs(manifest.Shards), UpdatedAt: event.OccurredAt,
		}
	case eventGenerationBound:
		record, exists := state.records[event.RunID]
		if !exists {
			return fmt.Errorf("%w: generation references missing plan", ErrNotFound)
		}
		if record.Canceled {
			return ErrCanceled
		}
		if record.Aggregate != nil {
			return fmt.Errorf("%w: aggregate is already terminal", ErrInvalidTransition)
		}
		binding := *event.Generation
		if binding.RunID != event.RunID || !binding.BoundAt.Equal(event.OccurredAt) {
			return fmt.Errorf("generation does not bind event")
		}
		if prior := record.ActiveGeneration; prior != nil &&
			(binding.Generation <= prior.Generation || binding.FencingToken <= prior.FencingToken) {
			return fmt.Errorf("%w: generation/fencing must increase", ErrFenced)
		}
		record.ActiveGeneration = clonePointer(binding)
		record.UpdatedAt = event.OccurredAt
		state.records[event.RunID] = record
	case eventCheckpointRecorded:
		record, exists := state.records[event.RunID]
		if !exists {
			return ErrNotFound
		}
		if record.Canceled {
			return ErrCanceled
		}
		if record.Aggregate != nil {
			return fmt.Errorf("%w: aggregate is already terminal", ErrInvalidTransition)
		}
		checkpoint := *event.Checkpoint
		if record.ActiveGeneration == nil ||
			checkpoint.RunID != event.RunID ||
			checkpoint.Generation != record.ActiveGeneration.Generation ||
			checkpoint.FencingToken != record.ActiveGeneration.FencingToken ||
			checkpoint.WorkerID != record.ActiveGeneration.WorkerID {
			return ErrFenced
		}
		if checkpoint.StartedAt.Before(record.ActiveGeneration.BoundAt) ||
			checkpoint.CompletedAt.Before(record.ActiveGeneration.BoundAt) {
			return fmt.Errorf("%w: checkpoint predates active generation", ErrFenced)
		}
		shard, exists := findShard(record.Manifest.Shards, checkpoint.ShardID)
		if !exists {
			return fmt.Errorf("checkpoint references unknown shard %q", checkpoint.ShardID)
		}
		if checkpoint.ProcessedFiles > len(shard.Files) || checkpoint.ProcessedBytes > shard.FileBytes {
			return fmt.Errorf("checkpoint coverage exceeds shard closure")
		}
		if checkpoint.Status == CheckpointSucceeded &&
			(checkpoint.ProcessedFiles != len(shard.Files) || checkpoint.ProcessedBytes != shard.FileBytes) {
			return fmt.Errorf("succeeded checkpoint must cover the exact shard")
		}
		for _, prior := range record.CheckpointHistory {
			if prior.ShardID == checkpoint.ShardID && prior.Status == CheckpointSucceeded {
				return fmt.Errorf("%w: shard already completed", ErrConflict)
			}
			if prior.ShardID == checkpoint.ShardID && prior.Generation == checkpoint.Generation &&
				prior.Attempt >= checkpoint.Attempt {
				return fmt.Errorf("%w: shard attempt must increase within generation", ErrConflict)
			}
		}
		if checkpoint.Status == CheckpointSucceeded {
			if err := repository.verifyCheckpointOutput(shard, checkpoint); err != nil {
				return err
			}
		}
		record.CheckpointHistory = append(record.CheckpointHistory, checkpoint)
		record.Completed, record.PendingShardIDs = projectCompletion(record.Manifest, record.CheckpointHistory)
		record.UpdatedAt = event.OccurredAt
		state.records[event.RunID] = record
	case eventPlanCanceled:
		record, exists := state.records[event.RunID]
		if !exists {
			return ErrNotFound
		}
		if record.Aggregate != nil {
			return fmt.Errorf("%w: completed plan cannot be canceled", ErrInvalidTransition)
		}
		if record.Canceled {
			return fmt.Errorf("%w: plan is already canceled", ErrConflict)
		}
		record.Canceled = true
		record.CancelReason = event.CancelReason
		record.ActiveGeneration = nil
		record.UpdatedAt = event.OccurredAt
		state.records[event.RunID] = record
	case eventAggregateRecorded:
		record, exists := state.records[event.RunID]
		if !exists {
			return ErrNotFound
		}
		if record.Canceled {
			return ErrCanceled
		}
		if record.Aggregate != nil {
			return fmt.Errorf("%w: aggregate already recorded", ErrConflict)
		}
		aggregate, err := repository.loadAggregate(*event.AggregateRef)
		if err != nil {
			return err
		}
		if err := validateAggregateAgainstRecord(aggregate, record, event); err != nil {
			return err
		}
		record.Aggregate = clonePointer(aggregate)
		record.AggregateRef = clonePointer(*event.AggregateRef)
		record.ActiveGeneration = nil
		record.UpdatedAt = event.OccurredAt
		state.records[event.RunID] = record
	default:
		return fmt.Errorf("unsupported shard event type %q", event.Type)
	}
	return nil
}

func validateEvent(event shardEvent) error {
	if event.SchemaVersion != eventSchemaVersion {
		return fmt.Errorf("unsupported shard event schema %q", event.SchemaVersion)
	}
	if err := validateID("run_id", event.RunID); err != nil {
		return err
	}
	if err := validateID("actor", event.Actor); err != nil {
		return err
	}
	if err := validateText("audit", event.Audit, 4096, false); err != nil {
		return err
	}
	if !isUTC(event.OccurredAt) {
		return fmt.Errorf("occurred_at must be a non-zero UTC timestamp")
	}
	shape := 0
	if event.ManifestRef != nil {
		shape++
	}
	if event.Generation != nil {
		shape++
	}
	if event.Checkpoint != nil {
		shape++
	}
	if event.AggregateRef != nil {
		shape++
	}
	if event.CancelReason != "" {
		shape++
	}
	if shape != 1 {
		return fmt.Errorf("shard event requires exactly one typed payload")
	}
	switch event.Type {
	case eventPlanRecorded:
		if event.ManifestRef == nil {
			return fmt.Errorf("plan event requires manifest_ref")
		}
		return validateRef("manifest_ref", *event.ManifestRef, runmodel.ContractReviewShardManifest)
	case eventGenerationBound:
		if event.Generation == nil {
			return fmt.Errorf("generation event requires generation")
		}
		return event.Generation.Validate()
	case eventCheckpointRecorded:
		if event.Checkpoint == nil {
			return fmt.Errorf("checkpoint event requires checkpoint")
		}
		return event.Checkpoint.Validate()
	case eventPlanCanceled:
		return validateText("cancel_reason", event.CancelReason, 1024, false)
	case eventAggregateRecorded:
		if event.AggregateRef == nil {
			return fmt.Errorf("aggregate event requires aggregate_ref")
		}
		return validateRef("aggregate_ref", *event.AggregateRef, runmodel.ContractReviewShardAggregate)
	default:
		return fmt.Errorf("unsupported shard event type %q", event.Type)
	}
}

func (repository *Repository) putArtifact(contract string, data []byte) (runmodel.ArtifactRef, error) {
	ref, err := repository.store.PutArtifact(data)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return runmodel.ArtifactRef{
		URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes, Contract: contract,
	}, nil
}

func (repository *Repository) readArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	data, err := repository.store.ReadArtifact(local.ArtifactRef{
		URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (repository *Repository) loadManifest(ref runmodel.ArtifactRef) (Manifest, error) {
	if err := validateRef("manifest_ref", ref, runmodel.ContractReviewShardManifest); err != nil {
		return Manifest{}, err
	}
	data, err := repository.readArtifact(ref)
	if err != nil {
		return Manifest{}, err
	}
	return DecodeManifest(data)
}

func (repository *Repository) loadAggregate(ref runmodel.ArtifactRef) (Aggregate, error) {
	if err := validateRef("aggregate_ref", ref, runmodel.ContractReviewShardAggregate); err != nil {
		return Aggregate{}, err
	}
	data, err := repository.readArtifact(ref)
	if err != nil {
		return Aggregate{}, err
	}
	return DecodeAggregate(data)
}

func (repository *Repository) verifyManifestClosure(manifest Manifest) error {
	data, err := repository.readArtifact(manifest.ReviewInputRef)
	if err != nil {
		return fmt.Errorf("read full ReviewInput: %w", err)
	}
	input, err := reviewcore.DecodeReviewInput(data)
	if err != nil {
		return fmt.Errorf("decode full ReviewInput: %w", err)
	}
	digest, err := reviewcore.DigestReviewInput(input)
	if err != nil || digest != manifest.TargetDigest {
		return fmt.Errorf("full ReviewInput digest differs from manifest target")
	}
	for _, shard := range manifest.Shards {
		data, err := repository.readArtifact(shard.InputRef)
		if err != nil {
			return fmt.Errorf("read shard %q input: %w", shard.ShardID, err)
		}
		input, err := reviewcore.DecodeReviewInput(data)
		if err != nil {
			return fmt.Errorf("decode shard %q input: %w", shard.ShardID, err)
		}
		digest, err := reviewcore.DigestReviewInput(input)
		if err != nil || digest != shard.InputSHA256 {
			return fmt.Errorf("shard %q input digest differs", shard.ShardID)
		}
		if !reflect.DeepEqual(shardFiles(input.Files), shard.Files) {
			return fmt.Errorf("shard %q input files differ from manifest", shard.ShardID)
		}
	}
	return nil
}

func (repository *Repository) verifyCheckpointOutput(shard Shard, checkpoint Checkpoint) error {
	data, err := repository.readArtifact(*checkpoint.OutputRef)
	if err != nil {
		return fmt.Errorf("read shard output: %w", err)
	}
	result, err := reviewcore.DecodeStageResult(data)
	if err != nil {
		return fmt.Errorf("decode shard output: %w", err)
	}
	if result.Stage != reviewcore.StageDetect || result.TargetDigest != shard.InputSHA256 {
		return fmt.Errorf("shard output does not bind detect stage and exact shard input")
	}
	return nil
}

func buildAggregate(record Record, mutation Mutation) (Aggregate, error) {
	if record.Canceled {
		return Aggregate{}, ErrCanceled
	}
	if record.Aggregate != nil {
		return Aggregate{}, fmt.Errorf("%w: aggregate already recorded", ErrConflict)
	}
	if len(record.Completed) != len(record.Manifest.Shards) || len(record.PendingShardIDs) != 0 {
		return Aggregate{}, ErrIncomplete
	}
	completed := make(map[string]Checkpoint, len(record.Completed))
	for _, checkpoint := range record.Completed {
		completed[checkpoint.ShardID] = checkpoint
	}
	outputs := make([]ShardOutput, 0, len(record.Manifest.Shards))
	for _, shard := range record.Manifest.Shards {
		checkpoint := completed[shard.ShardID]
		outputs = append(outputs, ShardOutput{
			ShardID: shard.ShardID, Ordinal: shard.Ordinal,
			OutputRef:  *checkpoint.OutputRef,
			Generation: checkpoint.Generation, FencingToken: checkpoint.FencingToken,
		})
	}
	completeness := "complete"
	if len(record.Manifest.Gaps) != 0 {
		completeness = "partial"
	}
	aggregate := Aggregate{
		SchemaVersion: AggregateSchemaVersion,
		RunID:         record.Manifest.RunID, ManifestRef: record.ManifestRef,
		Completeness: completeness, Outputs: outputs,
		Coverage: record.Manifest.Coverage, Gaps: cloneValue(record.Manifest.Gaps),
		CompletedAt: mutation.At, CompletedBy: mutation.Actor,
	}
	return aggregate, aggregate.Validate()
}

func validateAggregateAgainstRecord(aggregate Aggregate, record Record, event shardEvent) error {
	if aggregate.RunID != event.RunID || aggregate.ManifestRef != record.ManifestRef ||
		!aggregate.CompletedAt.Equal(event.OccurredAt) || aggregate.CompletedBy != event.Actor {
		return fmt.Errorf("aggregate does not bind terminal event")
	}
	expected, err := buildAggregate(record, Mutation{Actor: event.Actor, At: event.OccurredAt})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, aggregate) {
		return fmt.Errorf("aggregate does not exactly match durable shard checkpoints")
	}
	return nil
}

func projectCompletion(manifest Manifest, history []Checkpoint) ([]Checkpoint, []string) {
	completedByID := map[string]Checkpoint{}
	for _, checkpoint := range history {
		if checkpoint.Status == CheckpointSucceeded {
			completedByID[checkpoint.ShardID] = checkpoint
		}
	}
	completed := make([]Checkpoint, 0, len(completedByID))
	pending := make([]string, 0, len(manifest.Shards)-len(completedByID))
	for _, shard := range manifest.Shards {
		if checkpoint, exists := completedByID[shard.ShardID]; exists {
			completed = append(completed, checkpoint)
		} else {
			pending = append(pending, shard.ShardID)
		}
	}
	return completed, pending
}

func shardIDs(shards []Shard) []string {
	ids := make([]string, 0, len(shards))
	for _, shard := range shards {
		ids = append(ids, shard.ShardID)
	}
	return ids
}

func findShard(shards []Shard, id string) (Shard, bool) {
	index := sort.Search(len(shards), func(index int) bool { return shards[index].ShardID >= id })
	if index < len(shards) && shards[index].ShardID == id {
		return shards[index], true
	}
	for _, shard := range shards {
		if shard.ShardID == id {
			return shard, true
		}
	}
	return Shard{}, false
}

func clonePointer[T any](value T) *T {
	copy := cloneValue(value)
	return &copy
}

func cloneValue[T any](value T) T {
	data, _ := json.Marshal(value)
	var copy T
	_ = json.Unmarshal(data, &copy)
	return copy
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
