package configrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/store/local"
)

type projectionState struct {
	records       map[revisionKey]Record
	active        map[string]activeProjection
	frames        map[uint64]publishFrame
	events        []local.Envelope
	eventsByID    map[string]local.Envelope
	history       []AuditEntry
	revisionAudit map[revisionKey][]AuditEntry
}

func newProjectionState() *projectionState {
	return &projectionState{
		records:       make(map[revisionKey]Record),
		active:        make(map[string]activeProjection),
		frames:        make(map[uint64]publishFrame),
		events:        []local.Envelope{},
		eventsByID:    make(map[string]local.Envelope),
		history:       []AuditEntry{},
		revisionAudit: make(map[revisionKey][]AuditEntry),
	}
}

func (repository *Repository) load() (*projectionState, error) {
	envelopes, err := repository.store.ReadJSONL(lifecycleStream)
	if errors.Is(err, os.ErrNotExist) {
		return newProjectionState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read lifecycle stream: %v", ErrCorrupt, err)
	}
	state := newProjectionState()
	for _, envelope := range envelopes {
		if err := state.apply(envelope, repository); err != nil {
			return nil, fmt.Errorf("%w: event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
		state.events = append(state.events, envelope)
		state.eventsByID[envelope.ID] = envelope
	}
	return state, nil
}

func (state *projectionState) apply(
	envelope local.Envelope,
	repository *Repository,
) error {
	if envelope.Schema != lifecycleEventSchemaVersion {
		return fmt.Errorf("unsupported lifecycle event schema %q", envelope.Schema)
	}
	var event lifecycleEvent
	if err := decodeStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode lifecycle event: %w", err)
	}
	if event.SchemaVersion != lifecycleEventSchemaVersion {
		return fmt.Errorf("payload schema %q does not match event schema", event.SchemaVersion)
	}
	if event.OccurredAt.IsZero() || !event.OccurredAt.Equal(envelope.Time.UTC()) {
		return fmt.Errorf("payload time does not match envelope time")
	}
	if err := validateSafeText("actor", event.Actor, 256, false); err != nil {
		return err
	}
	if err := validateSafeText("audit", event.Audit, 4096, true); err != nil {
		return err
	}
	key := revisionKey{ID: event.RevisionID, Revision: event.Revision}
	if err := validateIdentity(key); err != nil {
		return err
	}
	beforeStatuses := make(map[revisionKey]Status, len(state.records))
	for recordKey, record := range state.records {
		beforeStatuses[recordKey] = record.Status
	}
	switch event.Type {
	case EventCreated:
		if event.Rollout != nil {
			return fmt.Errorf("created event must not contain rollout")
		}
		if _, duplicate := state.records[key]; duplicate {
			return fmt.Errorf("duplicate created revision %s@%s", key.ID, key.Revision)
		}
		stored, err := repository.loadStoredRevision(key)
		if err != nil {
			return err
		}
		if stored.SHA256 != event.SHA256 {
			return fmt.Errorf("created event digest does not match immutable revision")
		}
		record := Record{
			Revision:  stored.Revision,
			SHA256:    stored.SHA256,
			Status:    StatusDraft,
			CreatedAt: envelope.Time.UTC(),
			CreatedBy: event.Actor,
			UpdatedAt: envelope.Time.UTC(),
			UpdatedBy: event.Actor,
		}
		state.records[key] = record
	case EventValidated:
		if event.Rollout != nil {
			return fmt.Errorf("validated event must not contain rollout")
		}
		record, exists := state.records[key]
		if !exists {
			return fmt.Errorf("validated event references unknown revision")
		}
		if record.SHA256 != event.SHA256 {
			return fmt.Errorf("validated event digest does not match immutable revision")
		}
		if record.Status != StatusDraft {
			return fmt.Errorf("validate requires draft, got %s", record.Status)
		}
		record.Status = StatusValidated
		record.UpdatedAt = envelope.Time.UTC()
		record.UpdatedBy = event.Actor
		state.records[key] = record
	case EventPublished:
		if event.Rollout == nil {
			return fmt.Errorf("published event requires rollout")
		}
		if err := event.Rollout.Validate(); err != nil {
			return err
		}
		record, exists := state.records[key]
		if !exists {
			return fmt.Errorf("published event references unknown revision")
		}
		if record.SHA256 != event.SHA256 {
			return fmt.Errorf("published event digest does not match immutable revision")
		}
		if record.Status != StatusValidated {
			return fmt.Errorf("publish requires validated, got %s", record.Status)
		}
		selector := selectorIdentity(record.Revision)
		before := cloneProjection(state.active[selector])
		if event.Rollout.Percentage < 100 && before.Baseline == nil {
			return fmt.Errorf("percentage rollout requires a previous published baseline")
		}
		state.frames[envelope.Sequence] = publishFrame{Before: before}
		next := cloneProjection(before)
		newActivation := &activation{
			Key: key, Percentage: event.Rollout.Percentage,
			Seed: event.Rollout.Seed, PublishSequence: envelope.Sequence,
		}
		if event.Rollout.Percentage == 100 {
			state.supersedeActivation(next.Baseline, key, envelope, event.Actor)
			state.supersedeActivation(next.Rollout, key, envelope, event.Actor)
			next.Baseline = newActivation
			next.Rollout = nil
		} else {
			state.supersedeActivation(next.Rollout, key, envelope, event.Actor)
			next.Rollout = newActivation
		}
		record.Status = StatusPublished
		record.UpdatedAt = envelope.Time.UTC()
		record.UpdatedBy = event.Actor
		state.records[key] = record
		state.active[selector] = next
	case EventRolloutAdvanced:
		if event.Rollout == nil {
			return fmt.Errorf("rollout_advanced event requires rollout")
		}
		if err := event.Rollout.Validate(); err != nil {
			return err
		}
		record, exists := state.records[key]
		if !exists {
			return fmt.Errorf("rollout_advanced event references unknown revision")
		}
		if record.SHA256 != event.SHA256 {
			return fmt.Errorf("rollout_advanced event digest does not match immutable revision")
		}
		if record.Status != StatusPublished {
			return fmt.Errorf("rollout advance requires published, got %s", record.Status)
		}
		selector := selectorIdentity(record.Revision)
		current, exists := state.active[selector]
		if !exists || current.Rollout == nil || current.Rollout.Key != key {
			return fmt.Errorf("rollout advance target is not the active percentage rollout")
		}
		prior := current.Rollout
		if event.Rollout.Percentage <= prior.Percentage {
			return fmt.Errorf(
				"rollout advance must increase percentage above %d",
				prior.Percentage,
			)
		}
		if event.Rollout.Seed != prior.Seed {
			return fmt.Errorf("rollout advance must preserve assignment seed")
		}
		next := cloneProjection(current)
		advanced := &activation{
			Key: key, Percentage: event.Rollout.Percentage,
			Seed: prior.Seed, PublishSequence: prior.PublishSequence,
		}
		if event.Rollout.Percentage == 100 {
			state.supersedeActivation(next.Baseline, key, envelope, event.Actor)
			next.Baseline = advanced
			next.Rollout = nil
		} else {
			next.Rollout = advanced
		}
		record.UpdatedAt = envelope.Time.UTC()
		record.UpdatedBy = event.Actor
		state.records[key] = record
		state.active[selector] = next
	case EventRolledBack:
		if event.Rollout != nil {
			return fmt.Errorf("rolled_back event must not contain rollout")
		}
		record, exists := state.records[key]
		if !exists {
			return fmt.Errorf("rollback event references unknown revision")
		}
		if record.SHA256 != event.SHA256 {
			return fmt.Errorf("rollback event digest does not match immutable revision")
		}
		if record.Status != StatusPublished {
			return fmt.Errorf("rollback requires published, got %s", record.Status)
		}
		selector := selectorIdentity(record.Revision)
		current, exists := state.active[selector]
		if !exists {
			return fmt.Errorf("rollback target has no active selector projection")
		}
		targetActivation, isBaseline := findActivation(current, key)
		if targetActivation == nil {
			return fmt.Errorf("rollback target is not active")
		}
		if isBaseline && current.Rollout != nil {
			return fmt.Errorf("rollback baseline while a rollout is active is ambiguous")
		}
		frame, exists := state.frames[targetActivation.PublishSequence]
		if !exists {
			return fmt.Errorf("rollback target has no prior publish frame")
		}
		restore := cloneProjection(frame.Before)
		if restore.Baseline == nil {
			return fmt.Errorf("rollback target has no previous published revision")
		}
		record.Status = StatusRolledBack
		record.UpdatedAt = envelope.Time.UTC()
		record.UpdatedBy = event.Actor
		state.records[key] = record
		state.restoreActivation(restore.Baseline, envelope, event.Actor)
		state.restoreActivation(restore.Rollout, envelope, event.Actor)
		state.active[selector] = restore
	default:
		return fmt.Errorf("unsupported lifecycle event type %q", event.Type)
	}
	if _, exists := state.records[key]; !exists {
		return fmt.Errorf("event did not project a revision record")
	}
	changed := make([]revisionKey, 0)
	for recordKey, record := range state.records {
		before, existed := beforeStatuses[recordKey]
		if !existed || before != record.Status {
			changed = append(changed, recordKey)
		}
	}
	if event.Type == EventRolloutAdvanced {
		found := false
		for _, changedKey := range changed {
			if changedKey == key {
				found = true
				break
			}
		}
		if !found {
			changed = append(changed, key)
		}
	}
	sort.Slice(changed, func(left, right int) bool {
		if changed[left].ID != changed[right].ID {
			return changed[left].ID < changed[right].ID
		}
		return changed[left].Revision < changed[right].Revision
	})
	for _, changedKey := range changed {
		record := state.records[changedKey]
		entry := AuditEntry{
			Sequence: envelope.Sequence, EventID: envelope.ID, Type: event.Type,
			RevisionID: changedKey.ID, Revision: changedKey.Revision,
			SHA256: record.SHA256, Status: record.Status,
			Actor: event.Actor, Audit: event.Audit,
			At: envelope.Time.UTC(), Rollout: cloneRollout(event.Rollout),
		}
		state.history = append(state.history, entry)
		state.revisionAudit[changedKey] = append(state.revisionAudit[changedKey], entry)
	}
	return nil
}

func (repository *Repository) loadStoredRevision(
	key revisionKey,
) (storedRevision, error) {
	var stored storedRevision
	if err := repository.store.GetJSON(revisionObjectID(key), &stored); err != nil {
		return storedRevision{}, fmt.Errorf("load immutable config revision: %w", err)
	}
	if stored.SchemaVersion != storedRevisionSchemaVersion {
		return storedRevision{}, fmt.Errorf("unsupported stored revision schema %q", stored.SchemaVersion)
	}
	if err := stored.Revision.Validate(); err != nil {
		return storedRevision{}, fmt.Errorf("validate immutable config revision: %w", err)
	}
	if stored.Revision.ID != key.ID || stored.Revision.Revision != key.Revision {
		return storedRevision{}, fmt.Errorf("immutable revision identity does not match lifecycle event")
	}
	digest, err := reviewconfig.DigestRevision(stored.Revision)
	if err != nil {
		return storedRevision{}, err
	}
	if digest != stored.SHA256 {
		return storedRevision{}, fmt.Errorf("immutable revision digest does not match content")
	}
	return stored, nil
}

func (state *projectionState) supersedeActivation(
	active *activation,
	except revisionKey,
	envelope local.Envelope,
	actor string,
) {
	if active == nil || active.Key == except {
		return
	}
	record := state.records[active.Key]
	record.Status = StatusSuperseded
	record.UpdatedAt = envelope.Time.UTC()
	record.UpdatedBy = actor
	state.records[active.Key] = record
}

func (state *projectionState) restoreActivation(
	active *activation,
	envelope local.Envelope,
	actor string,
) {
	if active == nil {
		return
	}
	record := state.records[active.Key]
	record.Status = StatusPublished
	record.UpdatedAt = envelope.Time.UTC()
	record.UpdatedBy = actor
	state.records[active.Key] = record
}

func findActivation(
	projection activeProjection,
	key revisionKey,
) (*activation, bool) {
	if projection.Baseline != nil && projection.Baseline.Key == key {
		return projection.Baseline, true
	}
	if projection.Rollout != nil && projection.Rollout.Key == key {
		return projection.Rollout, false
	}
	return nil, false
}

func cloneProjection(projection activeProjection) activeProjection {
	result := activeProjection{}
	if projection.Baseline != nil {
		copy := *projection.Baseline
		result.Baseline = &copy
	}
	if projection.Rollout != nil {
		copy := *projection.Rollout
		result.Rollout = &copy
	}
	return result
}

func (state *projectionState) record(key revisionKey) (Record, error) {
	record, exists := state.records[key]
	if !exists {
		return Record{}, fmt.Errorf("%w: revision %s@%s", ErrNotFound, key.ID, key.Revision)
	}
	return cloneRecord(record), nil
}

func cloneRecord(record Record) Record {
	data, err := json.Marshal(record.Revision)
	if err == nil {
		var revision reviewconfig.Revision
		if decodeErr := json.Unmarshal(data, &revision); decodeErr == nil {
			record.Revision = revision
		}
	}
	return record
}
