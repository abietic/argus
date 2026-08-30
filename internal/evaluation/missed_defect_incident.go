package evaluation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

const (
	MissedDefectIncidentSchemaVersion = "argus.missed_defect_incident.v1alpha1"
	incidentEventSchemaVersion        = "argus.missed_defect_incident_event.v1alpha1"
	incidentStream                    = "evaluation/missed-defect-incidents"
)

type MissedDefectIncident struct {
	SchemaVersion     string                 `json:"schema_version"`
	IncidentID        string                 `json:"incident_id"`
	ReviewRunID       string                 `json:"review_run_id"`
	DefectFingerprint string                 `json:"defect_fingerprint"`
	Category          string                 `json:"category"`
	Severity          string                 `json:"severity"`
	Anchors           []LabelAnchor          `json:"anchors"`
	EvidenceArtifacts []runmodel.ArtifactRef `json:"evidence_artifacts"`
	SourceAuthority   string                 `json:"source_authority"`
	SourceID          string                 `json:"source_id"`
	SourceRevision    string                 `json:"source_revision"`
	OccurredAt        time.Time              `json:"occurred_at"`
	RecordedAt        time.Time              `json:"recorded_at"`
	PriorIncidentID   string                 `json:"prior_incident_id,omitempty"`
}

type MissedDefectIncidentEntry struct {
	EventID  string               `json:"event_id"`
	Incident MissedDefectIncident `json:"incident"`
	Actor    string               `json:"actor"`
	At       time.Time            `json:"at"`
}

func (incident MissedDefectIncident) Validate() error {
	if incident.SchemaVersion != MissedDefectIncidentSchemaVersion {
		return fmt.Errorf("unsupported missed-defect incident schema %q", incident.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{"incident_id", incident.IncidentID}, {"review_run_id", incident.ReviewRunID},
		{"source_authority", incident.SourceAuthority}, {"source_id", incident.SourceID},
		{"source_revision", incident.SourceRevision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if incident.PriorIncidentID != "" {
		if err := validateID("prior_incident_id", incident.PriorIncidentID); err != nil {
			return err
		}
		if incident.PriorIncidentID == incident.IncidentID {
			return fmt.Errorf("prior_incident_id must differ from incident_id")
		}
	}
	if err := validateSHA256("defect_fingerprint", incident.DefectFingerprint); err != nil {
		return err
	}
	if err := validateID("category", incident.Category); err != nil {
		return err
	}
	switch incident.Severity {
	case "low", "medium", "high", "critical":
	default:
		return fmt.Errorf("unsupported severity %q", incident.Severity)
	}
	if len(incident.Anchors) == 0 {
		return fmt.Errorf("anchors must be non-empty")
	}
	for index, anchor := range incident.Anchors {
		if err := anchor.validate(); err != nil {
			return fmt.Errorf("anchors[%d]: %w", index, err)
		}
		if index > 0 && !labelAnchorLess(incident.Anchors[index-1], anchor) {
			return fmt.Errorf("anchors must be uniquely sorted")
		}
	}
	if len(incident.EvidenceArtifacts) == 0 {
		return fmt.Errorf("evidence_artifacts must be non-empty")
	}
	for index, ref := range incident.EvidenceArtifacts {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("evidence_artifacts[%d]: %w", index, err)
		}
		if index > 0 && incident.EvidenceArtifacts[index-1].URI >= ref.URI {
			return fmt.Errorf("evidence_artifacts must be uniquely sorted by URI")
		}
	}
	if incident.OccurredAt.IsZero() || incident.RecordedAt.IsZero() ||
		incident.RecordedAt.Before(incident.OccurredAt) {
		return fmt.Errorf("recorded_at must not precede non-zero occurred_at")
	}
	return nil
}

func DecodeMissedDefectIncident(data []byte) (MissedDefectIncident, error) {
	return decodeStrict(data, "MissedDefectIncident", func(value MissedDefectIncident) error {
		return value.Validate()
	})
}

type incidentEvent struct {
	SchemaVersion string               `json:"schema_version"`
	Incident      MissedDefectIncident `json:"incident"`
	Actor         string               `json:"actor"`
	Roles         []Role               `json:"roles"`
	Audit         string               `json:"audit"`
	OccurredAt    time.Time            `json:"occurred_at"`
}

type incidentProjection struct {
	byID       map[string]MissedDefectIncidentEntry
	superseded map[string]string
	events     map[string]local.Envelope
	sequence   uint64
}

func newIncidentProjection() incidentProjection {
	return incidentProjection{byID: map[string]MissedDefectIncidentEntry{}, superseded: map[string]string{}, events: map[string]local.Envelope{}}
}

func (repository *Repository) loadIncidents() (incidentProjection, error) {
	state := newIncidentProjection()
	envelopes, err := readOptionalStream(repository.store, incidentStream)
	if err != nil {
		return state, fmt.Errorf("%w: read incident stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range envelopes {
		if envelope.Schema != incidentEventSchemaVersion {
			return state, corruptf("incident event %q has unsupported schema", envelope.ID)
		}
		var event incidentEvent
		if err := decodeEventStrict(envelope.Payload, &event); err != nil {
			return state, corruptf("decode incident event %q: %v", envelope.ID, err)
		}
		if event.SchemaVersion != envelope.Schema || !event.OccurredAt.Equal(envelope.Time.UTC()) {
			return state, corruptf("incident event %q envelope mismatch", envelope.ID)
		}
		if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
			return state, corruptf("incident event %q audit: %v", envelope.ID, err)
		}
		if !hasRole(event.Roles, RoleIncidentIngest) {
			return state, corruptf("incident event %q lacks incident_ingest role", envelope.ID)
		}
		if err := event.Incident.Validate(); err != nil || !event.Incident.RecordedAt.Equal(event.OccurredAt) {
			return state, corruptf("incident event %q invalid incident: %v", envelope.ID, err)
		}
		if _, exists := state.events[envelope.ID]; exists {
			return state, corruptf("duplicate incident event id %q", envelope.ID)
		}
		if _, exists := state.byID[event.Incident.IncidentID]; exists {
			return state, corruptf("duplicate incident id %q", event.Incident.IncidentID)
		}
		if prior := event.Incident.PriorIncidentID; prior != "" {
			previous, exists := state.byID[prior]
			if !exists || previous.Incident.ReviewRunID != event.Incident.ReviewRunID ||
				event.Incident.RecordedAt.Before(previous.Incident.RecordedAt) {
				return state, corruptf("incident %q has invalid prior", event.Incident.IncidentID)
			}
			if _, exists := state.superseded[prior]; exists {
				return state, corruptf("incident %q correction branches", prior)
			}
			state.superseded[prior] = event.Incident.IncidentID
		}
		entry := MissedDefectIncidentEntry{EventID: envelope.ID, Incident: cloneValue(event.Incident), Actor: event.Actor, At: event.OccurredAt}
		state.byID[event.Incident.IncidentID] = entry
		state.events[envelope.ID] = envelope
		state.sequence = envelope.Sequence
	}
	return state, nil
}

func (repository *Repository) RecordMissedDefectIncident(ctx context.Context, incident MissedDefectIncident, mutation Mutation) (MissedDefectIncidentEntry, error) {
	if err := checkContext(ctx); err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	if err := incident.Validate(); err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	if err := mutation.Validate(); err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	if !incident.RecordedAt.Equal(mutation.At.UTC()) {
		return MissedDefectIncidentEntry{}, fmt.Errorf("recorded_at must equal mutation time")
	}
	if !hasRole(mutation.Roles, RoleIncidentIngest) {
		return MissedDefectIncidentEntry{}, fmt.Errorf("%w: incident_ingest role is required", ErrUnauthorized)
	}
	event := incidentEvent{SchemaVersion: incidentEventSchemaVersion, Incident: cloneValue(incident), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadIncidents()
	if err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	dataset, err := repository.load()
	if err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	if existing, exists := dataset.events[mutation.IdempotencyKey]; exists && existing.stream != incidentStream {
		return MissedDefectIncidentEntry{}, fmt.Errorf("%w: idempotency key is already used in %s", ErrConflict, existing.stream)
	}
	if envelope, exists := state.events[mutation.IdempotencyKey]; exists {
		var stored incidentEvent
		if err := decodeEventStrict(envelope.Payload, &stored); err != nil || !reflect.DeepEqual(stored, event) {
			return MissedDefectIncidentEntry{}, fmt.Errorf("%w: idempotency key has different input", ErrConflict)
		}
		return cloneValue(state.byID[incident.IncidentID]), nil
	}
	if _, exists := state.byID[incident.IncidentID]; exists {
		return MissedDefectIncidentEntry{}, fmt.Errorf("%w: incident_id already exists", ErrConflict)
	}
	if prior := incident.PriorIncidentID; prior != "" {
		previous, exists := state.byID[prior]
		if !exists {
			return MissedDefectIncidentEntry{}, fmt.Errorf("%w: prior incident", ErrNotFound)
		}
		if previous.Incident.ReviewRunID != incident.ReviewRunID || incident.RecordedAt.Before(previous.Incident.RecordedAt) {
			return MissedDefectIncidentEntry{}, fmt.Errorf("%w: invalid incident correction", ErrInvalidTransition)
		}
		if _, exists := state.superseded[prior]; exists {
			return MissedDefectIncidentEntry{}, fmt.Errorf("%w: prior incident already superseded", ErrConflict)
		}
	}
	_, err = repository.store.AppendJSONLAtSequence(incidentStream, state.sequence, local.Event{ID: mutation.IdempotencyKey, Schema: incidentEventSchemaVersion, Time: mutation.At.UTC(), Payload: event})
	if errors.Is(err, local.ErrEventConflict) {
		return MissedDefectIncidentEntry{}, fmt.Errorf("%w: incident stream changed concurrently", ErrConflict)
	}
	if err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	reloaded, err := repository.loadIncidents()
	if err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	return cloneValue(reloaded.byID[incident.IncidentID]), nil
}

func (repository *Repository) ResolveMissedDefectIncident(id string) (MissedDefectIncident, error) {
	if err := validateID("incident_id", id); err != nil {
		return MissedDefectIncident{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadIncidents()
	if err != nil {
		return MissedDefectIncident{}, err
	}
	entry, exists := state.byID[id]
	if !exists {
		return MissedDefectIncident{}, fmt.Errorf("%w: incident %q", ErrNotFound, id)
	}
	if successor, exists := state.superseded[id]; exists {
		return MissedDefectIncident{}, fmt.Errorf("%w: incident superseded by %q", ErrInvalidTransition, successor)
	}
	return cloneValue(entry.Incident), nil
}

func (repository *Repository) ListMissedDefectIncidents(access Access) ([]MissedDefectIncidentEntry, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	if !hasRole(access.Roles, RoleIncidentIngest) && !hasRole(access.Roles, RoleDatasetCurator) {
		return nil, fmt.Errorf("%w: incident role is required", ErrUnauthorized)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadIncidents()
	if err != nil {
		return nil, err
	}
	result := make([]MissedDefectIncidentEntry, 0, len(state.byID))
	for _, entry := range state.byID {
		result = append(result, cloneValue(entry))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Incident.IncidentID < result[j].Incident.IncidentID })
	return result, nil
}

func (repository *Repository) GetMissedDefectIncident(id string, access Access) (MissedDefectIncidentEntry, error) {
	if err := validateID("incident_id", id); err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	if err := access.Validate(); err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	if !hasRole(access.Roles, RoleIncidentIngest) && !hasRole(access.Roles, RoleDatasetCurator) {
		return MissedDefectIncidentEntry{}, fmt.Errorf("%w: incident role is required", ErrUnauthorized)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadIncidents()
	if err != nil {
		return MissedDefectIncidentEntry{}, err
	}
	entry, exists := state.byID[id]
	if !exists {
		return MissedDefectIncidentEntry{}, fmt.Errorf("%w: incident %q", ErrNotFound, id)
	}
	return cloneValue(entry), nil
}
