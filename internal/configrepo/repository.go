package configrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/store/local"
)

type Repository struct {
	store *local.Store
	mu    sync.Mutex
}

func New(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	repository := &Repository{store: store}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Create(
	ctx context.Context,
	revision reviewconfig.Revision,
	mutation Mutation,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := revision.Validate(); err != nil {
		return Record{}, fmt.Errorf("validate config revision: %w", err)
	}
	digest, err := reviewconfig.DigestRevision(revision)
	if err != nil {
		return Record{}, err
	}
	event := lifecycleEvent{
		SchemaVersion: lifecycleEventSchemaVersion,
		Type:          EventCreated,
		RevisionID:    revision.ID,
		Revision:      revision.Revision,
		SHA256:        digest,
		Actor:         mutation.Actor,
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
	}
	if err := validateMutation(mutation); err != nil {
		return Record{}, err
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	if existing, ok := state.eventsByID[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, event, mutation); err != nil {
			return Record{}, err
		}
		return state.record(revisionKey{ID: revision.ID, Revision: revision.Revision})
	}
	key := revisionKey{ID: revision.ID, Revision: revision.Revision}
	if _, exists := state.records[key]; exists {
		return Record{}, fmt.Errorf("%w: revision %s@%s already exists",
			ErrConflict, revision.ID, revision.Revision)
	}
	stored := storedRevision{
		SchemaVersion: storedRevisionSchemaVersion,
		SHA256:        digest,
		Revision:      revision,
	}
	if err := repository.persistImmutable(key, stored); err != nil {
		return Record{}, err
	}
	if err := state.apply(syntheticEnvelope(state, mutation, event), repository); err != nil {
		return Record{}, err
	}
	if _, err := repository.store.AppendJSONL(lifecycleStream, local.Event{
		ID: mutation.IdempotencyKey, Schema: lifecycleEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	}); err != nil {
		return Record{}, fmt.Errorf("append config create event: %w", err)
	}
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	return reloaded.record(key)
}

func (repository *Repository) ValidateRevision(
	ctx context.Context,
	id string,
	revision string,
	mutation Mutation,
) (Record, error) {
	return repository.transition(ctx, revisionKey{ID: id, Revision: revision}, mutation, EventValidated, nil)
}

func (repository *Repository) Publish(
	ctx context.Context,
	id string,
	revision string,
	rollout Rollout,
	mutation Mutation,
) (Record, error) {
	if err := rollout.Validate(); err != nil {
		return Record{}, err
	}
	return repository.transition(
		ctx,
		revisionKey{ID: id, Revision: revision},
		mutation,
		EventPublished,
		&rollout,
	)
}

// AdvanceRollout monotonically widens one active percentage rollout without
// changing its immutable revision, assignment seed, or original publish
// frame. Keeping the original frame means a later rollback still restores the
// exact baseline which preceded the canary publication.
func (repository *Repository) AdvanceRollout(
	ctx context.Context,
	id string,
	revision string,
	rollout Rollout,
	mutation Mutation,
) (Record, error) {
	if err := rollout.Validate(); err != nil {
		return Record{}, err
	}
	return repository.transition(
		ctx,
		revisionKey{ID: id, Revision: revision},
		mutation,
		EventRolloutAdvanced,
		&rollout,
	)
}

func (repository *Repository) Rollback(
	ctx context.Context,
	id string,
	revision string,
	mutation Mutation,
) (Record, error) {
	return repository.transition(ctx, revisionKey{ID: id, Revision: revision}, mutation, EventRolledBack, nil)
}

func (repository *Repository) transition(
	ctx context.Context,
	key revisionKey,
	mutation Mutation,
	eventType EventType,
	rollout *Rollout,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := validateMutation(mutation); err != nil {
		return Record{}, err
	}
	if err := validateIdentity(key); err != nil {
		return Record{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	record, exists := state.records[key]
	if !exists {
		return Record{}, fmt.Errorf("%w: revision %s@%s", ErrNotFound, key.ID, key.Revision)
	}
	event := lifecycleEvent{
		SchemaVersion: lifecycleEventSchemaVersion,
		Type:          eventType,
		RevisionID:    key.ID,
		Revision:      key.Revision,
		SHA256:        record.SHA256,
		Actor:         mutation.Actor,
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
		Rollout:       cloneRollout(rollout),
	}
	if existing, ok := state.eventsByID[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, event, mutation); err != nil {
			return Record{}, err
		}
		return state.record(key)
	}
	if err := state.apply(syntheticEnvelope(state, mutation, event), repository); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrInvalidTransition, err)
	}
	if _, err := repository.store.AppendJSONL(lifecycleStream, local.Event{
		ID: mutation.IdempotencyKey, Schema: lifecycleEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	}); err != nil {
		return Record{}, fmt.Errorf("append config %s event: %w", eventType, err)
	}
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	return reloaded.record(key)
}

func (repository *Repository) Get(
	id string,
	revision string,
) (Record, error) {
	if err := validateIdentity(revisionKey{ID: id, Revision: revision}); err != nil {
		return Record{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	return state.record(revisionKey{ID: id, Revision: revision})
}

func (repository *Repository) GetWithHistory(
	id string,
	revision string,
) (RecordDetail, error) {
	key := revisionKey{ID: id, Revision: revision}
	if err := validateIdentity(key); err != nil {
		return RecordDetail{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return RecordDetail{}, err
	}
	record, err := state.record(key)
	if err != nil {
		return RecordDetail{}, err
	}
	history := make([]AuditEntry, 0, len(state.revisionAudit[key]))
	for _, entry := range state.revisionAudit[key] {
		history = append(history, cloneAuditEntry(entry))
	}
	return RecordDetail{Record: record, History: history}, nil
}

func (repository *Repository) List() ([]Record, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	keys := make([]revisionKey, 0, len(state.records))
	for key := range state.records {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].ID != keys[right].ID {
			return keys[left].ID < keys[right].ID
		}
		return keys[left].Revision < keys[right].Revision
	})
	records := make([]Record, 0, len(keys))
	for _, key := range keys {
		record, err := state.record(key)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (repository *Repository) History(
	id string,
	revision string,
) ([]AuditEntry, error) {
	key := revisionKey{ID: id, Revision: revision}
	if err := validateIdentity(key); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	if _, exists := state.records[key]; !exists {
		return nil, fmt.Errorf("%w: revision %s@%s", ErrNotFound, id, revision)
	}
	history := make([]AuditEntry, 0)
	for _, entry := range state.history {
		if entry.RevisionID == id && entry.Revision == revision {
			history = append(history, cloneAuditEntry(entry))
		}
	}
	return history, nil
}

// ActiveRevisions returns at most one revision for each selector applicable to
// context. Percentage rollouts use a stable assignment; a miss returns the
// selector's previous published baseline.
func (repository *Repository) ActiveRevisions(
	resolutionContext reviewconfig.ResolutionContext,
) ([]reviewconfig.Revision, error) {
	if err := resolutionContext.Validate(); err != nil {
		return nil, fmt.Errorf("validate resolution context: %w", err)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	selections, err := selectActiveRevisions(state, resolutionContext)
	if err != nil {
		return nil, err
	}
	selected := make([]reviewconfig.Revision, 0, len(selections))
	for _, selection := range selections {
		selected = append(selected, selection.Revision)
	}
	return selected, nil
}

type activeRevisionSelection struct {
	Revision   reviewconfig.Revision
	Record     Record
	Selector   string
	Projection activeProjection
	Chosen     activation
}

func selectActiveRevisions(
	state *projectionState,
	resolutionContext reviewconfig.ResolutionContext,
) ([]activeRevisionSelection, error) {
	selected := make([]activeRevisionSelection, 0, len(state.active))
	for selector, projection := range state.active {
		if projection.Baseline == nil {
			return nil, corruptf("selector %q has no published baseline", selector)
		}
		chosen := projection.Baseline
		if projection.Rollout != nil &&
			rolloutHit(resolutionContext, selector, *projection.Rollout) {
			chosen = projection.Rollout
		}
		record, exists := state.records[chosen.Key]
		if !exists || record.Status != StatusPublished {
			return nil, corruptf("selector %q references a non-published revision", selector)
		}
		matches, err := record.Revision.Applicable(resolutionContext)
		if err != nil {
			return nil, corruptf("validate active revision %s@%s: %v",
				chosen.Key.ID, chosen.Key.Revision, err)
		}
		if matches {
			selected = append(selected, activeRevisionSelection{
				Revision: record.Revision, Record: record, Selector: selector,
				Projection: cloneProjection(projection), Chosen: *chosen,
			})
		}
	}
	sort.Slice(selected, func(left, right int) bool {
		leftSpecificity, _ := selected[left].Revision.Specificity()
		rightSpecificity, _ := selected[right].Revision.Specificity()
		if leftSpecificity.ScopeRank != rightSpecificity.ScopeRank {
			return leftSpecificity.ScopeRank < rightSpecificity.ScopeRank
		}
		if leftSpecificity.PathDepth != rightSpecificity.PathDepth {
			return leftSpecificity.PathDepth < rightSpecificity.PathDepth
		}
		leftSelector := selectorIdentity(selected[left].Revision)
		rightSelector := selectorIdentity(selected[right].Revision)
		if leftSelector != rightSelector {
			return leftSelector < rightSelector
		}
		if selected[left].Revision.ID != selected[right].Revision.ID {
			return selected[left].Revision.ID < selected[right].Revision.ID
		}
		return selected[left].Revision.Revision < selected[right].Revision.Revision
	})
	return selected, nil
}

// ResolvePublished loads only lifecycle-published revisions applicable to the
// exact context and resolves them into one immutable ConfigBundle. Draft,
// validated-only, superseded, rolled-back, and selector-mismatched revisions
// are never passed to the domain resolver.
func (repository *Repository) ResolvePublished(
	ctx context.Context,
	resolutionContext reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, error) {
	bundle, _, err := repository.ResolvePublishedWithReceipt(ctx, resolutionContext)
	return bundle, err
}

// ResolvePublishedWithReceipt resolves one atomic lifecycle projection and
// emits host-owned provenance for the exact published revisions and rollout
// assignments used by the effective bundle. The receipt is not a signature;
// callers must preserve its binding to this trusted repository result.
func (repository *Repository) ResolvePublishedWithReceipt(
	ctx context.Context,
	resolutionContext reviewconfig.ResolutionContext,
) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	if err := checkContext(ctx); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	if err := resolutionContext.Validate(); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("validate resolution context: %w", err)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	if err := checkContext(ctx); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	selections, err := selectActiveRevisions(state, resolutionContext)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	if len(selections) == 0 {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, fmt.Errorf(
			"%w: %s/%s/%s/%s/%s",
			ErrNoPublishedConfig,
			resolutionContext.TenantID,
			resolutionContext.OrganizationID,
			resolutionContext.RepositoryID,
			resolutionContext.Path,
			resolutionContext.InvocationID,
		)
	}
	active := make([]reviewconfig.Revision, 0, len(selections))
	for _, selection := range selections {
		active = append(active, selection.Revision)
	}
	bundle, err := reviewconfig.Resolve(resolutionContext, active)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, fmt.Errorf(
			"resolve published config revisions: %w",
			err,
		)
	}
	if err := checkContext(ctx); err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	bindings, err := publishedRevisionBindings(bundle, selections, state)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{}, err
	}
	receipt, err := reviewconfig.NewConfigResolutionReceipt(bundle, bindings)
	if err != nil {
		return reviewconfig.ConfigBundle{}, reviewconfig.ConfigResolutionReceipt{},
			fmt.Errorf("seal published config resolution receipt: %w", err)
	}
	return bundle, receipt, nil
}

func publishedRevisionBindings(
	bundle reviewconfig.ConfigBundle,
	selections []activeRevisionSelection,
	state *projectionState,
) ([]reviewconfig.PublishedRevisionBinding, error) {
	byKey := make(map[revisionKey]activeRevisionSelection, len(selections))
	for _, selection := range selections {
		byKey[selection.Chosen.Key] = selection
	}
	bindings := make([]reviewconfig.PublishedRevisionBinding, 0, len(bundle.AppliedRevisions))
	for _, source := range bundle.AppliedRevisions {
		key := revisionKey{ID: source.ID, Revision: source.Revision}
		selection, exists := byKey[key]
		if !exists || selection.Selector != string(source.Scope)+"\x00"+source.Selector {
			return nil, corruptf("resolved bundle source %s@%s has no active selection", source.ID, source.Revision)
		}
		sequence := selection.Chosen.PublishSequence
		if sequence == 0 || sequence > uint64(len(state.events)) {
			return nil, corruptf("published revision %s@%s has invalid publish sequence", source.ID, source.Revision)
		}
		envelope := state.events[sequence-1]
		if envelope.Sequence != sequence {
			return nil, corruptf("published revision %s@%s event sequence changed", source.ID, source.Revision)
		}
		var event lifecycleEvent
		if err := decodeStrict(envelope.Payload, &event); err != nil {
			return nil, corruptf("decode published revision event: %v", err)
		}
		if event.Type != EventPublished || event.RevisionID != source.ID ||
			event.Revision != source.Revision || event.SHA256 != selection.Record.SHA256 {
			return nil, corruptf("published revision %s@%s event binding changed", source.ID, source.Revision)
		}
		bindings = append(bindings, reviewconfig.PublishedRevisionBinding{
			Source: source, RevisionSHA256: selection.Record.SHA256,
			PublishEventID: envelope.ID, PublishSequence: sequence,
			PublishedAt: envelope.Time.UTC(),
			AssignmentSHA256: resolutionAssignmentDigest(
				bundle.Context, selection.Selector, selection.Projection, selection.Chosen,
			),
		})
	}
	return bindings, nil
}

func resolutionAssignmentDigest(
	context reviewconfig.ResolutionContext,
	selector string,
	projection activeProjection,
	chosen activation,
) string {
	activationValue := func(value *activation) string {
		if value == nil {
			return "-"
		}
		return strings.Join([]string{
			value.Key.ID, value.Key.Revision, fmt.Sprintf("%d", value.Percentage),
			value.Seed, fmt.Sprintf("%d", value.PublishSequence),
		}, "\x00")
	}
	value := strings.Join([]string{
		context.TenantID, context.OrganizationID, context.RepositoryID, context.Path,
		context.InvocationID, selector, activationValue(projection.Baseline),
		activationValue(projection.Rollout), chosen.Key.ID, chosen.Key.Revision,
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (rollout Rollout) Validate() error {
	if rollout.Percentage < 1 || rollout.Percentage > 100 {
		return fmt.Errorf("rollout percentage must be between 1 and 100")
	}
	if rollout.Percentage < 100 {
		if err := validateSafeText("rollout seed", rollout.Seed, 256, false); err != nil {
			return err
		}
	} else if rollout.Seed != "" {
		if err := validateSafeText("rollout seed", rollout.Seed, 256, false); err != nil {
			return err
		}
	}
	return nil
}

func rolloutHit(
	context reviewconfig.ResolutionContext,
	selector string,
	rollout activation,
) bool {
	if rollout.Percentage >= 100 {
		return true
	}
	value := strings.Join([]string{
		context.TenantID,
		context.OrganizationID,
		context.RepositoryID,
		context.Path,
		context.InvocationID,
		selector,
		rollout.Key.ID,
		rollout.Key.Revision,
		rollout.Seed,
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	bucket := binary.BigEndian.Uint64(digest[:8]) % 100
	return int(bucket) < rollout.Percentage
}

func revisionObjectID(key revisionKey) string {
	digest := sha256.Sum256([]byte(key.ID + "\x00" + key.Revision))
	return "config/revisions/" + hex.EncodeToString(digest[:])
}

func selectorIdentity(revision reviewconfig.Revision) string {
	selector := revision.Selector
	var value string
	switch revision.Scope {
	case reviewconfig.ScopePlatform:
		value = "*"
	case reviewconfig.ScopeTenant:
		value = "tenant=" + selector.TenantID
	case reviewconfig.ScopeOrganization:
		value = "tenant=" + selector.TenantID + ";organization=" + selector.OrganizationID
	case reviewconfig.ScopeRepository:
		value = "tenant=" + selector.TenantID + ";organization=" +
			selector.OrganizationID + ";repository=" + selector.RepositoryID
	case reviewconfig.ScopePath:
		value = "tenant=" + selector.TenantID + ";organization=" +
			selector.OrganizationID + ";repository=" + selector.RepositoryID +
			";path=" + selector.PathPrefix
	case reviewconfig.ScopeInvocation:
		value = "tenant=" + selector.TenantID + ";organization=" +
			selector.OrganizationID + ";repository=" + selector.RepositoryID +
			";invocation=" + selector.InvocationID
	default:
		value = "invalid"
	}
	return string(revision.Scope) + "\x00" + value
}

func validateMutation(mutation Mutation) error {
	if err := validateSafeText(
		"idempotency key", mutation.IdempotencyKey, 256, false,
	); err != nil {
		return err
	}
	if err := validateSafeText("actor", mutation.Actor, 256, false); err != nil {
		return err
	}
	if err := validateSafeText("audit", mutation.Audit, 4096, true); err != nil {
		return err
	}
	if mutation.At.IsZero() {
		return fmt.Errorf("mutation time is required")
	}
	return nil
}

func validateIdentity(key revisionKey) error {
	if err := validateSafeText("revision id", key.ID, 256, false); err != nil {
		return err
	}
	return validateSafeText("revision", key.Revision, 256, false)
}

func validateSafeText(name, value string, maxBytes int, allowSpace bool) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be non-empty, trimmed UTF-8 within %d bytes", name, maxBytes)
	}
	for _, character := range value {
		if unicode.IsControl(character) ||
			!allowSpace && !(unicode.IsLetter(character) || unicode.IsDigit(character) ||
				strings.ContainsRune("._~-", character)) {
			return fmt.Errorf("%s contains unsafe characters", name)
		}
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}

func cloneRollout(rollout *Rollout) *Rollout {
	if rollout == nil {
		return nil
	}
	copy := *rollout
	return &copy
}

func cloneAuditEntry(entry AuditEntry) AuditEntry {
	entry.Rollout = cloneRollout(entry.Rollout)
	return entry
}

func corruptf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, arguments...))
}

func decodeStrict(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func syntheticEnvelope(
	state *projectionState,
	mutation Mutation,
	event lifecycleEvent,
) local.Envelope {
	return local.Envelope{
		ID:       mutation.IdempotencyKey,
		Sequence: uint64(len(state.events)) + 1,
		Time:     mutation.At.UTC(),
		Schema:   lifecycleEventSchemaVersion,
		Payload:  mustMarshal(event),
	}
}

func mustMarshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func (repository *Repository) persistImmutable(
	key revisionKey,
	stored storedRevision,
) error {
	id := revisionObjectID(key)
	err := repository.store.PutJSON(id, stored)
	if err == nil {
		return nil
	}
	if !errors.Is(err, local.ErrImmutableExists) {
		return fmt.Errorf("persist immutable config revision: %w", err)
	}
	var existing storedRevision
	if getErr := repository.store.GetJSON(id, &existing); getErr != nil {
		return fmt.Errorf("load existing immutable config revision: %w", getErr)
	}
	if existing.SchemaVersion != stored.SchemaVersion ||
		existing.SHA256 != stored.SHA256 ||
		!equalRevision(existing.Revision, stored.Revision) {
		return fmt.Errorf("%w: immutable revision %s@%s has different content",
			ErrConflict, key.ID, key.Revision)
	}
	return nil
}

func equalRevision(left, right reviewconfig.Revision) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (repository *Repository) assertIdempotent(
	envelope local.Envelope,
	event lifecycleEvent,
	mutation Mutation,
) error {
	_, err := repository.store.AppendJSONL(lifecycleStream, local.Event{
		ID: mutation.IdempotencyKey, Schema: lifecycleEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if err != nil {
		return fmt.Errorf("%w: idempotency key %q: %v",
			ErrConflict, mutation.IdempotencyKey, err)
	}
	if !envelope.Time.Equal(mutation.At.UTC()) {
		return fmt.Errorf("%w: idempotency key %q changed event time",
			ErrConflict, mutation.IdempotencyKey)
	}
	return nil
}
