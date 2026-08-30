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
	EvaluationProbeSchemaVersion        = "argus.evaluation_probe.v1alpha1"
	EvaluationProbeReceiptSchemaVersion = "argus.evaluation_probe_receipt.v1alpha1"
	probeEventSchemaVersion             = "argus.evaluation_probe_event.v1alpha1"
	probeStream                         = "evaluation/probes"
)

type ProbeKind string

const (
	ProbeMutationDefect    ProbeKind = "mutation_defect"
	ProbeSyntheticDefect   ProbeKind = "synthetic_defect"
	ProbeSyntheticClean    ProbeKind = "synthetic_clean"
	ProbeWorkflowInvariant ProbeKind = "workflow_invariant"
)

// ProbeOracle is independent source truth. It is deliberately separate from
// the review report and from the construction receipt so Argus output cannot
// label itself. Dataset governance still treats the result as candidate-only.
type ProbeOracle struct {
	Authority         string                 `json:"authority"`
	Revision          string                 `json:"revision"`
	ExpectedOutcome   ExpectedOutcome        `json:"expected_outcome"`
	Category          string                 `json:"category,omitempty"`
	Severity          string                 `json:"severity,omitempty"`
	Anchors           []LabelAnchor          `json:"anchors"`
	InvariantID       string                 `json:"invariant_id,omitempty"`
	EvidenceArtifacts []runmodel.ArtifactRef `json:"evidence_artifacts"`
}

// EvaluationProbe freezes the origin and oracle for a constructed evaluation
// input. ReceiptRef points to the independently produced execution receipt.
// Corrections form one append-only chain; old facts remain queryable.
type EvaluationProbe struct {
	SchemaVersion   string               `json:"schema_version"`
	ProbeID         string               `json:"probe_id"`
	Kind            ProbeKind            `json:"kind"`
	ReviewRunID     string               `json:"review_run_id"`
	Oracle          ProbeOracle          `json:"oracle"`
	ReceiptRef      runmodel.ArtifactRef `json:"receipt_ref"`
	SourceAuthority string               `json:"source_authority"`
	SourceID        string               `json:"source_id"`
	SourceRevision  string               `json:"source_revision"`
	RecordedAt      time.Time            `json:"recorded_at"`
	PriorProbeID    string               `json:"prior_probe_id,omitempty"`
}

// EvaluationProbeReceipt is a replayable, deterministic construction fact.
// For mutation probes BaselineTargetRef is mandatory. Synthetic and workflow
// probes have no baseline. ObservedOutcome must agree with the independent
// oracle before a candidate can be derived.
type EvaluationProbeReceipt struct {
	SchemaVersion     string                 `json:"schema_version"`
	ProbeID           string                 `json:"probe_id"`
	Kind              ProbeKind              `json:"kind"`
	ReviewRunID       string                 `json:"review_run_id"`
	InputTargetRef    runmodel.ArtifactRef   `json:"input_target_ref"`
	BaselineTargetRef *runmodel.ArtifactRef  `json:"baseline_target_ref,omitempty"`
	OracleSHA256      string                 `json:"oracle_sha256"`
	MethodID          string                 `json:"method_id"`
	MethodRevision    string                 `json:"method_revision"`
	ExecutorAuthority string                 `json:"executor_authority"`
	ExecutorRevision  string                 `json:"executor_revision"`
	ObservedOutcome   ExpectedOutcome        `json:"observed_outcome"`
	EvidenceArtifacts []runmodel.ArtifactRef `json:"evidence_artifacts"`
	RemoteWrites      string                 `json:"remote_writes"`
	CompletedAt       time.Time              `json:"completed_at"`
}

type EvaluationProbeEntry struct {
	EventID string          `json:"event_id"`
	Probe   EvaluationProbe `json:"probe"`
	Actor   string          `json:"actor"`
	At      time.Time       `json:"at"`
}

func (kind ProbeKind) Validate() error {
	switch kind {
	case ProbeMutationDefect, ProbeSyntheticDefect, ProbeSyntheticClean, ProbeWorkflowInvariant:
		return nil
	default:
		return fmt.Errorf("unsupported probe kind %q", kind)
	}
}

func (oracle ProbeOracle) Validate(kind ProbeKind) error {
	if err := validateID("oracle.authority", oracle.Authority); err != nil {
		return err
	}
	if err := validateID("oracle.revision", oracle.Revision); err != nil {
		return err
	}
	wantOutcome := OutcomeDefectPresent
	switch kind {
	case ProbeMutationDefect, ProbeSyntheticDefect:
		if err := validateID("oracle.category", oracle.Category); err != nil {
			return err
		}
		switch oracle.Severity {
		case "low", "medium", "high", "critical":
		default:
			return fmt.Errorf("unsupported oracle severity %q", oracle.Severity)
		}
		if len(oracle.Anchors) == 0 {
			return fmt.Errorf("defect probe oracle requires anchors")
		}
		if oracle.InvariantID != "" {
			return fmt.Errorf("defect probe oracle must not set invariant_id")
		}
	case ProbeSyntheticClean:
		wantOutcome = OutcomeClean
		if oracle.Category != "" || oracle.Severity != "" || len(oracle.Anchors) != 0 || oracle.InvariantID != "" {
			return fmt.Errorf("synthetic clean oracle must not contain defect or invariant fields")
		}
	case ProbeWorkflowInvariant:
		if oracle.ExpectedOutcome != OutcomeInvariantPass && oracle.ExpectedOutcome != OutcomeInvariantFail {
			return fmt.Errorf("workflow invariant oracle has invalid expected outcome %q", oracle.ExpectedOutcome)
		}
		wantOutcome = oracle.ExpectedOutcome
		if err := validateID("oracle.invariant_id", oracle.InvariantID); err != nil {
			return err
		}
		if oracle.Category != "" || oracle.Severity != "" || len(oracle.Anchors) != 0 {
			return fmt.Errorf("workflow invariant oracle must not contain defect fields")
		}
	default:
		return kind.Validate()
	}
	if oracle.ExpectedOutcome != wantOutcome {
		return fmt.Errorf("probe kind %q requires oracle outcome %q", kind, wantOutcome)
	}
	if err := validateProbeArtifactRefs("oracle.evidence_artifacts", oracle.EvidenceArtifacts, true); err != nil {
		return err
	}
	for index, anchor := range oracle.Anchors {
		if err := anchor.validate(); err != nil {
			return fmt.Errorf("oracle.anchors[%d]: %w", index, err)
		}
		if index > 0 && !labelAnchorLess(oracle.Anchors[index-1], anchor) {
			return fmt.Errorf("oracle.anchors must be uniquely sorted")
		}
	}
	return nil
}

func (oracle ProbeOracle) SHA256() (string, error) {
	return runmodel.DigestJSON(oracle)
}

func (probe EvaluationProbe) Validate() error {
	if probe.SchemaVersion != EvaluationProbeSchemaVersion {
		return fmt.Errorf("unsupported evaluation probe schema %q", probe.SchemaVersion)
	}
	if err := probe.Kind.Validate(); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"probe_id", probe.ProbeID}, {"review_run_id", probe.ReviewRunID},
		{"source_authority", probe.SourceAuthority}, {"source_id", probe.SourceID},
		{"source_revision", probe.SourceRevision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if probe.PriorProbeID != "" {
		if err := validateID("prior_probe_id", probe.PriorProbeID); err != nil {
			return err
		}
		if probe.PriorProbeID == probe.ProbeID {
			return fmt.Errorf("prior_probe_id must differ from probe_id")
		}
	}
	if err := probe.Oracle.Validate(probe.Kind); err != nil {
		return err
	}
	if probe.Oracle.Authority == probe.SourceAuthority {
		return fmt.Errorf("oracle authority must be independent from source authority")
	}
	if err := probe.ReceiptRef.Validate(); err != nil {
		return fmt.Errorf("receipt_ref: %w", err)
	}
	if probe.ReceiptRef.Contract != EvaluationProbeReceiptSchemaVersion {
		return fmt.Errorf("receipt_ref contract is %q, want %q", probe.ReceiptRef.Contract, EvaluationProbeReceiptSchemaVersion)
	}
	if probe.RecordedAt.IsZero() || probe.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("recorded_at must be non-zero UTC")
	}
	return nil
}

func (receipt EvaluationProbeReceipt) Validate() error {
	if receipt.SchemaVersion != EvaluationProbeReceiptSchemaVersion {
		return fmt.Errorf("unsupported evaluation probe receipt schema %q", receipt.SchemaVersion)
	}
	if err := receipt.Kind.Validate(); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"probe_id", receipt.ProbeID}, {"review_run_id", receipt.ReviewRunID},
		{"method_id", receipt.MethodID}, {"method_revision", receipt.MethodRevision},
		{"executor_authority", receipt.ExecutorAuthority}, {"executor_revision", receipt.ExecutorRevision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := receipt.InputTargetRef.Validate(); err != nil {
		return fmt.Errorf("input_target_ref: %w", err)
	}
	if receipt.InputTargetRef.Contract != runmodel.ContractMaterializedTarget {
		return fmt.Errorf("input_target_ref must be a MaterializedTarget")
	}
	if receipt.Kind == ProbeMutationDefect {
		if receipt.BaselineTargetRef == nil {
			return fmt.Errorf("mutation receipt requires baseline_target_ref")
		}
		if err := receipt.BaselineTargetRef.Validate(); err != nil {
			return fmt.Errorf("baseline_target_ref: %w", err)
		}
		if receipt.BaselineTargetRef.Contract != runmodel.ContractMaterializedTarget ||
			*receipt.BaselineTargetRef == receipt.InputTargetRef {
			return fmt.Errorf("mutation baseline must be a distinct MaterializedTarget")
		}
	} else if receipt.BaselineTargetRef != nil {
		return fmt.Errorf("only mutation receipt may set baseline_target_ref")
	}
	if err := validateSHA256("oracle_sha256", receipt.OracleSHA256); err != nil {
		return err
	}
	switch receipt.Kind {
	case ProbeMutationDefect, ProbeSyntheticDefect:
		if receipt.ObservedOutcome != OutcomeDefectPresent {
			return fmt.Errorf("defect probe receipt must observe defect_present")
		}
	case ProbeSyntheticClean:
		if receipt.ObservedOutcome != OutcomeClean {
			return fmt.Errorf("synthetic clean receipt must observe clean")
		}
	case ProbeWorkflowInvariant:
		if receipt.ObservedOutcome != OutcomeInvariantPass && receipt.ObservedOutcome != OutcomeInvariantFail {
			return fmt.Errorf("workflow receipt has invalid observed outcome %q", receipt.ObservedOutcome)
		}
	}
	if err := validateProbeArtifactRefs("evidence_artifacts", receipt.EvidenceArtifacts, true); err != nil {
		return err
	}
	if receipt.RemoteWrites != "deny" {
		return fmt.Errorf("evaluation probe receipt must deny remote writes")
	}
	if receipt.CompletedAt.IsZero() || receipt.CompletedAt.Location() != time.UTC {
		return fmt.Errorf("completed_at must be non-zero UTC")
	}
	return nil
}

func validateProbeArtifactRefs(name string, refs []runmodel.ArtifactRef, nonEmpty bool) error {
	if refs == nil || (nonEmpty && len(refs) == 0) {
		return fmt.Errorf("%s must be a non-empty array", name)
	}
	for index, ref := range refs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%s[%d]: %w", name, index, err)
		}
		if index > 0 && refs[index-1].URI >= ref.URI {
			return fmt.Errorf("%s must be uniquely sorted by URI", name)
		}
	}
	return nil
}

func DecodeEvaluationProbe(data []byte) (EvaluationProbe, error) {
	return decodeStrict(data, "EvaluationProbe", func(value EvaluationProbe) error { return value.Validate() })
}

func DecodeEvaluationProbeReceipt(data []byte) (EvaluationProbeReceipt, error) {
	return decodeStrict(data, "EvaluationProbeReceipt", func(value EvaluationProbeReceipt) error { return value.Validate() })
}

type probeEvent struct {
	SchemaVersion string          `json:"schema_version"`
	Probe         EvaluationProbe `json:"probe"`
	Actor         string          `json:"actor"`
	Roles         []Role          `json:"roles"`
	Audit         string          `json:"audit"`
	OccurredAt    time.Time       `json:"occurred_at"`
}

type probeProjection struct {
	byID       map[string]EvaluationProbeEntry
	superseded map[string]string
	events     map[string]local.Envelope
	sequence   uint64
}

func newProbeProjection() probeProjection {
	return probeProjection{byID: map[string]EvaluationProbeEntry{}, superseded: map[string]string{}, events: map[string]local.Envelope{}}
}

func (repository *Repository) loadProbes() (probeProjection, error) {
	state := newProbeProjection()
	envelopes, err := readOptionalStream(repository.store, probeStream)
	if err != nil {
		return state, fmt.Errorf("%w: read probe stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range envelopes {
		if envelope.Schema != probeEventSchemaVersion {
			return state, corruptf("probe event %q has unsupported schema", envelope.ID)
		}
		var event probeEvent
		if err := decodeEventStrict(envelope.Payload, &event); err != nil {
			return state, corruptf("decode probe event %q: %v", envelope.ID, err)
		}
		if event.SchemaVersion != envelope.Schema || !event.OccurredAt.Equal(envelope.Time.UTC()) {
			return state, corruptf("probe event %q envelope mismatch", envelope.ID)
		}
		if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
			return state, corruptf("probe event %q audit: %v", envelope.ID, err)
		}
		if !hasRole(event.Roles, RoleProbeIngest) {
			return state, corruptf("probe event %q lacks probe_ingest role", envelope.ID)
		}
		if err := event.Probe.Validate(); err != nil || !event.Probe.RecordedAt.Equal(event.OccurredAt) {
			return state, corruptf("probe event %q invalid probe: %v", envelope.ID, err)
		}
		if _, exists := state.events[envelope.ID]; exists {
			return state, corruptf("duplicate probe event id %q", envelope.ID)
		}
		if _, exists := state.byID[event.Probe.ProbeID]; exists {
			return state, corruptf("duplicate probe id %q", event.Probe.ProbeID)
		}
		if prior := event.Probe.PriorProbeID; prior != "" {
			previous, exists := state.byID[prior]
			if !exists || previous.Probe.ReviewRunID != event.Probe.ReviewRunID ||
				event.Probe.RecordedAt.Before(previous.Probe.RecordedAt) {
				return state, corruptf("probe %q has invalid prior", event.Probe.ProbeID)
			}
			if _, exists := state.superseded[prior]; exists {
				return state, corruptf("probe %q correction branches", prior)
			}
			state.superseded[prior] = event.Probe.ProbeID
		}
		entry := EvaluationProbeEntry{EventID: envelope.ID, Probe: cloneValue(event.Probe), Actor: event.Actor, At: event.OccurredAt}
		state.byID[event.Probe.ProbeID] = entry
		state.events[envelope.ID] = envelope
		state.sequence = envelope.Sequence
	}
	return state, nil
}

func (repository *Repository) RecordEvaluationProbe(ctx context.Context, probe EvaluationProbe, mutation Mutation) (EvaluationProbeEntry, error) {
	if err := checkContext(ctx); err != nil {
		return EvaluationProbeEntry{}, err
	}
	if err := probe.Validate(); err != nil {
		return EvaluationProbeEntry{}, err
	}
	if err := mutation.Validate(); err != nil {
		return EvaluationProbeEntry{}, err
	}
	if !probe.RecordedAt.Equal(mutation.At.UTC()) {
		return EvaluationProbeEntry{}, fmt.Errorf("recorded_at must equal mutation time")
	}
	if !hasRole(mutation.Roles, RoleProbeIngest) {
		return EvaluationProbeEntry{}, fmt.Errorf("%w: probe_ingest role is required", ErrUnauthorized)
	}
	event := probeEvent{SchemaVersion: probeEventSchemaVersion, Probe: cloneValue(probe), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadProbes()
	if err != nil {
		return EvaluationProbeEntry{}, err
	}
	dataset, err := repository.load()
	if err != nil {
		return EvaluationProbeEntry{}, err
	}
	if existing, exists := dataset.events[mutation.IdempotencyKey]; exists && existing.stream != probeStream {
		return EvaluationProbeEntry{}, fmt.Errorf("%w: idempotency key is already used in %s", ErrConflict, existing.stream)
	}
	if envelope, exists := state.events[mutation.IdempotencyKey]; exists {
		var stored probeEvent
		if err := decodeEventStrict(envelope.Payload, &stored); err != nil || !reflect.DeepEqual(stored, event) {
			return EvaluationProbeEntry{}, fmt.Errorf("%w: idempotency key has different input", ErrConflict)
		}
		return cloneValue(state.byID[probe.ProbeID]), nil
	}
	if _, exists := state.byID[probe.ProbeID]; exists {
		return EvaluationProbeEntry{}, fmt.Errorf("%w: probe_id already exists", ErrConflict)
	}
	if prior := probe.PriorProbeID; prior != "" {
		previous, exists := state.byID[prior]
		if !exists {
			return EvaluationProbeEntry{}, fmt.Errorf("%w: prior probe", ErrNotFound)
		}
		if previous.Probe.ReviewRunID != probe.ReviewRunID || probe.RecordedAt.Before(previous.Probe.RecordedAt) {
			return EvaluationProbeEntry{}, fmt.Errorf("%w: invalid probe correction", ErrInvalidTransition)
		}
		if _, exists := state.superseded[prior]; exists {
			return EvaluationProbeEntry{}, fmt.Errorf("%w: prior probe already superseded", ErrConflict)
		}
	}
	_, err = repository.store.AppendJSONLAtSequence(probeStream, state.sequence, local.Event{ID: mutation.IdempotencyKey, Schema: probeEventSchemaVersion, Time: mutation.At.UTC(), Payload: event})
	if errors.Is(err, local.ErrEventConflict) {
		return EvaluationProbeEntry{}, fmt.Errorf("%w: probe stream changed concurrently", ErrConflict)
	}
	if err != nil {
		return EvaluationProbeEntry{}, err
	}
	reloaded, err := repository.loadProbes()
	if err != nil {
		return EvaluationProbeEntry{}, err
	}
	return cloneValue(reloaded.byID[probe.ProbeID]), nil
}

func (repository *Repository) ResolveEvaluationProbe(id string) (EvaluationProbe, error) {
	if err := validateID("probe_id", id); err != nil {
		return EvaluationProbe{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadProbes()
	if err != nil {
		return EvaluationProbe{}, err
	}
	entry, exists := state.byID[id]
	if !exists {
		return EvaluationProbe{}, fmt.Errorf("%w: probe %q", ErrNotFound, id)
	}
	if successor, exists := state.superseded[id]; exists {
		return EvaluationProbe{}, fmt.Errorf("%w: probe superseded by %q", ErrInvalidTransition, successor)
	}
	return cloneValue(entry.Probe), nil
}

func (repository *Repository) ListEvaluationProbes(access Access) ([]EvaluationProbeEntry, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	if !hasRole(access.Roles, RoleProbeIngest) && !hasRole(access.Roles, RoleDatasetCurator) {
		return nil, fmt.Errorf("%w: probe role is required", ErrUnauthorized)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadProbes()
	if err != nil {
		return nil, err
	}
	result := make([]EvaluationProbeEntry, 0, len(state.byID))
	for _, entry := range state.byID {
		result = append(result, cloneValue(entry))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Probe.ProbeID < result[j].Probe.ProbeID })
	return result, nil
}

func (repository *Repository) GetEvaluationProbe(id string, access Access) (EvaluationProbeEntry, error) {
	if err := validateID("probe_id", id); err != nil {
		return EvaluationProbeEntry{}, err
	}
	if err := access.Validate(); err != nil {
		return EvaluationProbeEntry{}, err
	}
	if !hasRole(access.Roles, RoleProbeIngest) && !hasRole(access.Roles, RoleDatasetCurator) {
		return EvaluationProbeEntry{}, fmt.Errorf("%w: probe role is required", ErrUnauthorized)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.loadProbes()
	if err != nil {
		return EvaluationProbeEntry{}, err
	}
	entry, exists := state.byID[id]
	if !exists {
		return EvaluationProbeEntry{}, fmt.Errorf("%w: probe %q", ErrNotFound, id)
	}
	return cloneValue(entry), nil
}
