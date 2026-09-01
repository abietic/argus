package artifactrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

type Repository struct {
	store     *local.Store
	authority string
	now       func() time.Time
}

type refIdentity struct {
	authority   string
	tenantID    string
	workspaceID string
	objectID    string
}

func (identity refIdentity) path() string {
	return "/tenants/" + identity.tenantID +
		"/workspaces/" + identity.workspaceID +
		"/objects/" + identity.objectID
}

func Open(store *local.Store, now func() time.Time) (*Repository, error) {
	return OpenForAuthority(store, DefaultAuthority, now)
}

// OpenForAuthority binds one repository instance to one canonical logical
// authority. A store may host multiple authority-scoped repositories, but one
// instance can never mint or resolve another authority's references.
func OpenForAuthority(
	store *local.Store,
	authority string,
	now func() time.Time,
) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("artifact repository store is required")
	}
	if err := validateID("authority", authority); err != nil {
		return nil, err
	}
	if authority != strings.ToLower(authority) {
		return nil, fmt.Errorf("authority must be lowercase canonical form")
	}
	if now == nil {
		now = time.Now
	}
	return &Repository{store: store, authority: authority, now: now}, nil
}

func (repository *Repository) Put(
	ctx context.Context,
	subject Subject,
	request PutRequest,
) (Ref, Record, error) {
	if err := checkContext(ctx); err != nil {
		return Ref{}, Record{}, err
	}
	if err := subject.Validate(); err != nil {
		return Ref{}, Record{}, err
	}
	if request.TenantID != subject.TenantID ||
		request.WorkspaceID != subject.WorkspaceID {
		return Ref{}, Record{}, ErrUnauthorized
	}
	if err := validateID("authority", request.Authority); err != nil {
		return Ref{}, Record{}, err
	}
	if request.Authority != repository.authority {
		return Ref{}, Record{}, ErrUnauthorized
	}
	if err := validateText("contract", request.Contract); err != nil {
		return Ref{}, Record{}, err
	}
	if len(request.AllowedUses) == 0 ||
		!slices.IsSorted(request.AllowedUses) ||
		hasAdjacentDuplicate(request.AllowedUses) {
		return Ref{}, Record{}, fmt.Errorf("allowed_uses must be a sorted unique non-empty array")
	}
	for _, use := range request.AllowedUses {
		if err := use.Validate(); err != nil {
			return Ref{}, Record{}, err
		}
		if err := authorizeUseRole(subject, use); err != nil {
			return Ref{}, Record{}, err
		}
	}
	if err := validateReadClass(request.AllowedUses); err != nil {
		return Ref{}, Record{}, err
	}
	if err := request.Mutation.Validate(); err != nil {
		return Ref{}, Record{}, err
	}
	contentDigest := sha256.Sum256(request.Content)
	contentSHA256 := hex.EncodeToString(contentDigest[:])
	id := objectID(
		request.Authority,
		request.TenantID,
		request.WorkspaceID,
		request.Contract,
		contentSHA256,
		int64(len(request.Content)),
		request.AllowedUses,
	)
	identity := refIdentity{
		authority: request.Authority, tenantID: request.TenantID,
		workspaceID: request.WorkspaceID, objectID: id,
	}
	if err := repository.reserveMutation(mutationBinding{
		SchemaVersion: mutationSchemaVersion,
		Operation:     mutationPut,
		Authority:     identity.authority,
		TenantID:      identity.tenantID,
		WorkspaceID:   identity.workspaceID,
		ObjectID:      identity.objectID,
		Mutation:      request.Mutation,
	}); err != nil {
		return Ref{}, Record{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Ref{}, Record{}, err
	}
	contentRef, err := repository.store.PutArtifact(request.Content)
	if err != nil {
		return Ref{}, Record{}, fmt.Errorf("persist artifact content: %w", err)
	}
	if contentRef.SHA256 != contentSHA256 ||
		contentRef.SizeBytes != int64(len(request.Content)) {
		return Ref{}, Record{}, fmt.Errorf(
			"%w: content store returned a mismatched reference",
			ErrCorrupt,
		)
	}
	if err := checkContext(ctx); err != nil {
		return Ref{}, Record{}, err
	}
	metadata := Metadata{
		SchemaVersion: metadataSchemaVersion,
		ObjectID:      id,
		Authority:     request.Authority,
		TenantID:      request.TenantID,
		WorkspaceID:   request.WorkspaceID,
		Contract:      request.Contract,
		AllowedUses:   slices.Clone(request.AllowedUses),
		Content:       contentRef,
		CreatedBy:     request.Mutation.Actor,
		CreatedAt:     request.Mutation.At,
	}
	if err := metadata.Validate(); err != nil {
		return Ref{}, Record{}, err
	}
	event := artifactEvent{
		SchemaVersion: eventSchemaVersion,
		Type:          eventCreated,
		Metadata:      &metadata,
		Mutation:      request.Mutation,
	}
	stream := objectStream(id)
	if existing, loadErr := repository.load(stream); loadErr == nil {
		if !sameImmutableMetadata(existing.Metadata, metadata) {
			return Ref{}, Record{}, fmt.Errorf("%w: immutable metadata differs", ErrConflict)
		}
		return refFor(metadata), existing, nil
	} else if !errors.Is(loadErr, ErrNotFound) {
		return Ref{}, Record{}, loadErr
	}
	_, err = repository.store.AppendJSONLAtSequence(stream, 0, local.Event{
		ID:      request.Mutation.IdempotencyKey,
		Schema:  eventSchemaVersion,
		Time:    request.Mutation.At,
		Payload: event,
	})
	if err != nil {
		// A concurrent creator may have won with the same immutable metadata.
		existing, loadErr := repository.load(stream)
		if loadErr == nil && sameImmutableMetadata(existing.Metadata, metadata) {
			return refFor(metadata), existing, nil
		}
		if loadErr != nil {
			return Ref{}, Record{}, fmt.Errorf(
				"append artifact creation: %v; reload: %w",
				err,
				loadErr,
			)
		}
		return Ref{}, Record{}, fmt.Errorf("%w: append artifact creation: %v", ErrConflict, err)
	}
	record, err := repository.load(stream)
	if err != nil {
		return Ref{}, Record{}, err
	}
	return refFor(metadata), record, nil
}

func (repository *Repository) Resolve(
	ctx context.Context,
	subject Subject,
	ref Ref,
	use Use,
) ([]byte, Record, error) {
	if err := checkContext(ctx); err != nil {
		return nil, Record{}, err
	}
	if err := use.Validate(); err != nil {
		return nil, Record{}, err
	}
	if use == UseSensitiveRead {
		return nil, Record{}, fmt.Errorf(
			"%w: sensitive_read requires ResolveSensitive audit",
			ErrUseDenied,
		)
	}
	return repository.resolveAuthorized(ctx, subject, ref, use)
}

func (repository *Repository) resolveAuthorized(
	ctx context.Context,
	subject Subject,
	ref Ref,
	use Use,
) ([]byte, Record, error) {
	record, identity, err := repository.authorize(subject, ref)
	if err != nil {
		return nil, Record{}, err
	}
	switch record.State {
	case StateQuarantined:
		return nil, record, ErrQuarantined
	case StateTombstoned:
		return nil, record, ErrTombstoned
	case StateActive:
	default:
		return nil, Record{}, fmt.Errorf("%w: unsupported state %q", ErrCorrupt, record.State)
	}
	if !slices.Contains(record.Metadata.AllowedUses, use) {
		return nil, record, ErrUseDenied
	}
	if err := authorizeUseRole(subject, use); err != nil {
		return nil, record, err
	}
	content, err := repository.store.ReadArtifact(record.Metadata.Content)
	if err != nil {
		quarantineErr := repository.autoQuarantine(identity)
		current, loadErr := repository.load(objectStream(identity.objectID))
		if loadErr != nil {
			return nil, record, fmt.Errorf(
				"verify artifact content: %v; reload state: %w",
				err,
				loadErr,
			)
		}
		switch current.State {
		case StateQuarantined:
			return nil, current, fmt.Errorf("%w: content verification failed", ErrQuarantined)
		case StateTombstoned:
			return nil, current, ErrTombstoned
		}
		if quarantineErr != nil {
			return nil, current, fmt.Errorf(
				"verify artifact content: %v; quarantine: %w",
				err,
				quarantineErr,
			)
		}
		return nil, current, fmt.Errorf("read artifact content: %w", err)
	}
	return content, record, nil
}

func (repository *Repository) Inspect(
	ctx context.Context,
	subject Subject,
	ref Ref,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	record, _, err := repository.authorize(subject, ref)
	return record, err
}

func (repository *Repository) Quarantine(
	ctx context.Context,
	subject Subject,
	ref Ref,
	reason string,
	mutation Mutation,
) (Record, error) {
	return repository.changeState(
		ctx,
		subject,
		ref,
		eventQuarantined,
		reason,
		mutation,
		true,
	)
}

func (repository *Repository) ReleaseQuarantine(
	ctx context.Context,
	subject Subject,
	ref Ref,
	reason string,
	mutation Mutation,
) (Record, error) {
	record, identity, err := repository.authorizeOperator(subject, ref, mutation)
	if err != nil {
		return Record{}, err
	}
	if replay, found, replayErr := repository.replayStateMutation(
		identity,
		eventQuarantineReleased,
		reason,
		mutation,
	); found || replayErr != nil {
		return replay, replayErr
	}
	if record.State == StateTombstoned {
		return Record{}, ErrTombstoned
	}
	if record.State != StateQuarantined {
		return Record{}, fmt.Errorf("%w: release requires quarantined state", ErrConflict)
	}
	if _, err := repository.store.ReadArtifact(record.Metadata.Content); err != nil {
		return Record{}, fmt.Errorf("artifact integrity is not restored: %w", err)
	}
	released, err := repository.appendStateEvent(
		ctx,
		identity,
		eventQuarantineReleased,
		reason,
		mutation,
	)
	if err != nil {
		return Record{}, err
	}
	if _, verifyErr := repository.store.ReadArtifact(released.Metadata.Content); verifyErr != nil {
		if quarantineErr := repository.autoQuarantine(identity); quarantineErr != nil {
			return released, fmt.Errorf(
				"post-release artifact verification failed: %v; quarantine: %w",
				verifyErr,
				quarantineErr,
			)
		}
		current, loadErr := repository.load(objectStream(identity.objectID))
		if loadErr != nil {
			return released, fmt.Errorf(
				"post-release artifact verification failed: %v; reload: %w",
				verifyErr,
				loadErr,
			)
		}
		return current, fmt.Errorf("%w: post-release verification failed", ErrQuarantined)
	}
	return released, nil
}

func (repository *Repository) Tombstone(
	ctx context.Context,
	subject Subject,
	ref Ref,
	reason string,
	mutation Mutation,
) (Record, error) {
	return repository.changeState(
		ctx,
		subject,
		ref,
		eventTombstoned,
		reason,
		mutation,
		false,
	)
}

func (repository *Repository) changeState(
	ctx context.Context,
	subject Subject,
	ref Ref,
	eventType eventType,
	reason string,
	mutation Mutation,
	requireActive bool,
) (Record, error) {
	record, identity, err := repository.authorizeOperator(subject, ref, mutation)
	if err != nil {
		return Record{}, err
	}
	if replay, found, replayErr := repository.replayStateMutation(
		identity,
		eventType,
		reason,
		mutation,
	); found || replayErr != nil {
		return replay, replayErr
	}
	if record.State == StateTombstoned {
		return Record{}, ErrTombstoned
	}
	if requireActive && record.State != StateActive {
		return Record{}, fmt.Errorf("%w: operation requires active state", ErrConflict)
	}
	return repository.appendStateEvent(ctx, identity, eventType, reason, mutation)
}

func (repository *Repository) authorizeOperator(
	subject Subject,
	ref Ref,
	mutation Mutation,
) (Record, refIdentity, error) {
	if err := mutation.Validate(); err != nil {
		return Record{}, refIdentity{}, err
	}
	if !slices.Contains(subject.Roles, RoleIntegrityOperator) {
		return Record{}, refIdentity{}, ErrUnauthorized
	}
	return repository.authorize(subject, ref)
}

func (repository *Repository) authorize(
	subject Subject,
	ref Ref,
) (Record, refIdentity, error) {
	if err := subject.Validate(); err != nil {
		return Record{}, refIdentity{}, err
	}
	identity, err := validateRef(ref)
	if err != nil {
		return Record{}, refIdentity{}, err
	}
	if identity.authority != repository.authority ||
		identity.tenantID != subject.TenantID ||
		identity.workspaceID != subject.WorkspaceID {
		return Record{}, refIdentity{}, ErrUnauthorized
	}
	record, err := repository.load(objectStream(identity.objectID))
	if err != nil {
		return Record{}, refIdentity{}, err
	}
	metadata := record.Metadata
	if metadata.Authority != identity.authority ||
		metadata.TenantID != identity.tenantID ||
		metadata.WorkspaceID != identity.workspaceID ||
		metadata.ObjectID != identity.objectID ||
		metadata.Content.SHA256 != ref.SHA256 ||
		metadata.Content.SizeBytes != ref.SizeBytes ||
		metadata.Contract != ref.Contract {
		return Record{}, refIdentity{}, fmt.Errorf("%w: ref does not match metadata", ErrCorrupt)
	}
	return record, identity, nil
}

func (repository *Repository) appendStateEvent(
	ctx context.Context,
	identity refIdentity,
	eventType eventType,
	reason string,
	mutation Mutation,
) (Record, error) {
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	if err := validateText("reason", reason); err != nil {
		return Record{}, err
	}
	event := artifactEvent{
		SchemaVersion: eventSchemaVersion,
		Type:          eventType,
		Reason:        reason,
		Mutation:      mutation,
	}
	if err := repository.reserveMutation(stateMutationBinding(
		identity,
		eventType,
		reason,
		mutation,
	)); err != nil {
		return Record{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	stream := objectStream(identity.objectID)
	current, sequence, err := repository.loadWithSequence(stream)
	if err != nil {
		return Record{}, err
	}
	if current.State == StateTombstoned {
		return Record{}, ErrTombstoned
	}
	projected := current
	if err := applyEvent(&projected, event, int(sequence)); err != nil {
		return Record{}, fmt.Errorf("%w: invalid state transition: %v", ErrConflict, err)
	}
	if err := checkContext(ctx); err != nil {
		return Record{}, err
	}
	_, err = repository.store.AppendJSONLAtSequence(stream, sequence, local.Event{
		ID:      mutation.IdempotencyKey,
		Schema:  eventSchemaVersion,
		Time:    mutation.At,
		Payload: event,
	})
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return Record{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return Record{}, fmt.Errorf("append artifact state event: %w", err)
	}
	return repository.load(stream)
}

func (repository *Repository) replayStateMutation(
	identity refIdentity,
	eventType eventType,
	reason string,
	mutation Mutation,
) (Record, bool, error) {
	if err := repository.reserveMutation(stateMutationBinding(
		identity,
		eventType,
		reason,
		mutation,
	)); err != nil {
		return Record{}, false, err
	}
	stream := objectStream(identity.objectID)
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		return Record{}, false, fmt.Errorf("%w: read artifact ledger: %v", ErrCorrupt, err)
	}
	expected := artifactEvent{
		SchemaVersion: eventSchemaVersion,
		Type:          eventType,
		Reason:        reason,
		Mutation:      mutation,
	}
	for _, envelope := range envelopes {
		if envelope.ID != mutation.IdempotencyKey {
			continue
		}
		var existing artifactEvent
		if err := decodeStrictJSON(envelope.Payload, &existing); err != nil {
			return Record{}, false, fmt.Errorf("%w: decode idempotent event: %v", ErrCorrupt, err)
		}
		if !reflect.DeepEqual(existing, expected) {
			return Record{}, false, fmt.Errorf(
				"%w: idempotency key %q has different content",
				ErrConflict,
				mutation.IdempotencyKey,
			)
		}
		record, err := repository.load(stream)
		return record, true, err
	}
	return Record{}, false, nil
}

func stateMutationBinding(
	identity refIdentity,
	eventType eventType,
	reason string,
	mutation Mutation,
) mutationBinding {
	operation := mutationOperation("")
	switch eventType {
	case eventQuarantined:
		operation = mutationQuarantine
	case eventQuarantineReleased:
		operation = mutationReleaseQuarantine
	case eventTombstoned:
		operation = mutationTombstone
	}
	return mutationBinding{
		SchemaVersion: mutationSchemaVersion,
		Operation:     operation,
		Authority:     identity.authority,
		TenantID:      identity.tenantID,
		WorkspaceID:   identity.workspaceID,
		ObjectID:      identity.objectID,
		Reason:        reason,
		Mutation:      mutation,
	}
}

func (repository *Repository) autoQuarantine(identity refIdentity) error {
	current, sequence, err := repository.loadWithSequence(objectStream(identity.objectID))
	if err != nil {
		return err
	}
	if current.State != StateActive {
		return nil
	}
	at := repository.now().UTC()
	if at.IsZero() {
		return fmt.Errorf("repository clock returned zero")
	}
	if at.Before(current.StateChangedAt) {
		at = current.StateChangedAt
	}
	mutation := Mutation{
		IdempotencyKey: "integrity-" + identity.objectID + "-" +
			strconv.FormatUint(sequence, 10) + "-" +
			strconv.FormatInt(at.UnixNano(), 10),
		Actor: "system-integrity",
		Audit: "automatic checksum verification",
		At:    at,
	}
	_, err = repository.appendStateEvent(
		context.Background(),
		identity,
		eventQuarantined,
		"checksum_verification_failed",
		mutation,
	)
	if errors.Is(err, ErrConflict) {
		current, loadErr := repository.load(objectStream(identity.objectID))
		if loadErr == nil &&
			(current.State == StateQuarantined || current.State == StateTombstoned) {
			return nil
		}
	}
	return err
}

func (repository *Repository) load(stream string) (Record, error) {
	record, _, err := repository.loadWithSequence(stream)
	return record, err
}

func (repository *Repository) loadWithSequence(
	stream string,
) (Record, uint64, error) {
	expectedObjectID, ok := strings.CutPrefix(stream, "artifactrepo/objects/")
	if !ok || !lowerSHA256(expectedObjectID) {
		return Record{}, 0, fmt.Errorf("%w: invalid object stream identity", ErrCorrupt)
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, 0, ErrNotFound
		}
		return Record{}, 0, fmt.Errorf("%w: read artifact ledger: %v", ErrCorrupt, err)
	}
	if len(envelopes) == 0 {
		return Record{}, 0, fmt.Errorf("%w: artifact ledger is empty", ErrCorrupt)
	}
	var record Record
	for index, envelope := range envelopes {
		if envelope.Schema != eventSchemaVersion {
			return Record{}, 0, fmt.Errorf("%w: event %d schema mismatch", ErrCorrupt, index)
		}
		var event artifactEvent
		if err := decodeStrictJSON(envelope.Payload, &event); err != nil {
			return Record{}, 0, fmt.Errorf("%w: decode event %d: %v", ErrCorrupt, index, err)
		}
		if !envelope.Time.Equal(event.Mutation.At) {
			return Record{}, 0, fmt.Errorf(
				"%w: event %d envelope time does not bind mutation",
				ErrCorrupt,
				index,
			)
		}
		expectedEventID := event.Mutation.IdempotencyKey
		if event.Type == eventCreated {
			if event.Metadata == nil || event.Metadata.ObjectID != expectedObjectID {
				return Record{}, 0, fmt.Errorf(
					"%w: event %d metadata does not bind object stream",
					ErrCorrupt,
					index,
				)
			}
		}
		if envelope.ID != expectedEventID {
			return Record{}, 0, fmt.Errorf(
				"%w: event %d envelope id does not bind payload",
				ErrCorrupt,
				index,
			)
		}
		if err := applyEvent(&record, event, index); err != nil {
			return Record{}, 0, fmt.Errorf("%w: event %d: %v", ErrCorrupt, index, err)
		}
	}
	return record, uint64(len(envelopes)), nil
}

func applyEvent(record *Record, event artifactEvent, index int) error {
	if event.SchemaVersion != eventSchemaVersion {
		return fmt.Errorf("unsupported event schema %q", event.SchemaVersion)
	}
	if err := event.Mutation.Validate(); err != nil {
		return err
	}
	if index > 0 && event.Mutation.At.Before(record.StateChangedAt) {
		return fmt.Errorf("mutation time precedes the current state")
	}
	switch event.Type {
	case eventCreated:
		if index != 0 || event.Metadata == nil || event.Reason != "" {
			return fmt.Errorf("created must be the first event with metadata and no reason")
		}
		if err := event.Metadata.Validate(); err != nil {
			return err
		}
		if event.Metadata.CreatedBy != event.Mutation.Actor ||
			!event.Metadata.CreatedAt.Equal(event.Mutation.At) {
			return fmt.Errorf("created metadata does not bind its mutation")
		}
		record.Metadata = *event.Metadata
		record.State = StateActive
	case eventQuarantined:
		if index == 0 || event.Metadata != nil || record.State != StateActive {
			return fmt.Errorf("quarantine requires active state")
		}
		record.State = StateQuarantined
	case eventQuarantineReleased:
		if index == 0 || event.Metadata != nil || record.State != StateQuarantined {
			return fmt.Errorf("quarantine release requires quarantined state")
		}
		record.State = StateActive
	case eventTombstoned:
		if index == 0 || event.Metadata != nil ||
			record.State == StateTombstoned {
			return fmt.Errorf("tombstone requires a live object")
		}
		record.State = StateTombstoned
	default:
		return fmt.Errorf("unsupported event type %q", event.Type)
	}
	if event.Type != eventCreated {
		if err := validateText("reason", event.Reason); err != nil {
			return err
		}
	}
	record.StateReason = event.Reason
	record.StateChangedAt = event.Mutation.At
	record.LastMutationActor = event.Mutation.Actor
	return nil
}

func objectStream(objectID string) string {
	return "artifactrepo/objects/" + objectID
}

func mutationStream(identity refIdentity) string {
	return "artifactrepo/mutations/" + identity.authority + "/" +
		identity.tenantID + "/" + identity.workspaceID
}

func (repository *Repository) reserveMutation(binding mutationBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	identity := refIdentity{
		authority: binding.Authority, tenantID: binding.TenantID,
		workspaceID: binding.WorkspaceID, objectID: binding.ObjectID,
	}
	_, err := repository.store.AppendJSONL(mutationStream(identity), local.Event{
		ID:      binding.Mutation.IdempotencyKey,
		Schema:  mutationSchemaVersion,
		Time:    binding.Mutation.At,
		Payload: binding,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, local.ErrEventConflict) {
		return fmt.Errorf(
			"%w: idempotency key %q is already bound to another mutation",
			ErrConflict,
			binding.Mutation.IdempotencyKey,
		)
	}
	if errors.Is(err, local.ErrCorrupt) {
		return fmt.Errorf("%w: mutation ledger: %v", ErrCorrupt, err)
	}
	return fmt.Errorf("reserve artifact mutation: %w", err)
}

func canonicalURI(identity refIdentity) string {
	return (&url.URL{
		Scheme: "artifact",
		Host:   identity.authority,
		Path:   identity.path(),
	}).String()
}

func refFor(metadata Metadata) Ref {
	identity := refIdentity{
		authority: metadata.Authority,
		tenantID:  metadata.TenantID, workspaceID: metadata.WorkspaceID,
		objectID: metadata.ObjectID,
	}
	return Ref{
		URI:       canonicalURI(identity),
		SHA256:    metadata.Content.SHA256,
		SizeBytes: metadata.Content.SizeBytes,
		Contract:  metadata.Contract,
	}
}

func sameImmutableMetadata(left Metadata, right Metadata) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.ObjectID == right.ObjectID &&
		left.Authority == right.Authority &&
		left.TenantID == right.TenantID &&
		left.WorkspaceID == right.WorkspaceID &&
		left.Contract == right.Contract &&
		slices.Equal(left.AllowedUses, right.AllowedUses) &&
		left.Content == right.Content
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
