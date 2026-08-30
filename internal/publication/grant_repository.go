package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

const grantLedgerStream = "publication-grant-ledger"

var (
	ErrGrantConflict = errors.New("publication grant conflict")
	ErrGrantCorrupt  = errors.New("corrupt publication grant ledger")
	ErrGrantConsumed = errors.New("publication grant is already consumed")
	ErrGrantExpired  = errors.New("publication grant is expired")
)

type GrantRepository struct {
	store *local.Store
	mu    *sync.Mutex
}

var grantLedgerLocks sync.Map

func NewGrantRepository(store *local.Store) (*GrantRepository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	lock, _ := grantLedgerLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &GrantRepository{store: store, mu: lock.(*sync.Mutex)}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *GrantRepository) Record(
	ctx context.Context,
	request GrantRequest,
	mutation GrantMutation,
	binding GrantBinding,
) (Grant, error) {
	if ctx == nil {
		return Grant{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	request = cloneGrantRequest(request)
	mutation = cloneGrantMutation(mutation)
	if err := request.Validate(); err != nil {
		return Grant{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Grant{}, err
	}
	if err := binding.Validate(); err != nil {
		return Grant{}, err
	}
	if request.Provider != binding.Provider || request.RepositoryID != binding.RepositoryID {
		return Grant{}, fmt.Errorf("publication destination does not match resolved grant binding")
	}
	if mutation.At.Before(request.OccurredAt) {
		return Grant{}, fmt.Errorf("grant recording time precedes occurrence time")
	}
	if !mutation.At.Before(request.ExpiresAt) {
		return Grant{}, ErrGrantExpired
	}
	grant := grantFrom(request, mutation, binding)
	digest, err := DigestGrant(grant)
	if err != nil {
		return Grant{}, err
	}
	grant.GrantSHA256 = digest
	if err := grant.Validate(); err != nil {
		return Grant{}, err
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Grant{}, err
	}
	if existing, ok := state.grantsByMutation[mutation.IdempotencyKey]; ok {
		if !exactPublicationJSONEqual(existing, grant) {
			return Grant{}, grantConflictf("idempotency key %q was already used", mutation.IdempotencyKey)
		}
		return cloneGrant(existing), nil
	}
	if existing, ok := state.grants[grant.GrantID]; ok {
		return Grant{}, grantConflictf(
			"grant id %q already exists with mutation %q",
			grant.GrantID, existing.IdempotencyKey,
		)
	}
	if existing, ok := state.grantsByPublication[grant.PublicationID]; ok {
		return Grant{}, grantConflictf(
			"publication id %q is already bound to grant %q",
			grant.PublicationID, existing.GrantID,
		)
	}
	if existing, ok := state.grantsByPublicationKey[grant.PublicationIdempotencyKey]; ok {
		return Grant{}, grantConflictf(
			"publication idempotency key %q is already bound to grant %q",
			grant.PublicationIdempotencyKey, existing.GrantID,
		)
	}
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	_, err = repository.store.AppendJSONLAtSequence(
		grantLedgerStream,
		uint64(state.eventCount),
		local.Event{
			ID: mutation.IdempotencyKey, Schema: GrantSchemaVersion,
			Time: mutation.At, Payload: grant,
		},
	)
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return Grant{}, grantConflictf("grant ledger changed while recording %q", grant.GrantID)
		}
		return Grant{}, fmt.Errorf("append publication grant: %w", err)
	}
	return cloneGrant(grant), nil
}

func (repository *GrantRepository) Get(grantID string) (Grant, error) {
	if err := validateID("grant_id", grantID); err != nil {
		return Grant{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Grant{}, err
	}
	grant, ok := state.grants[grantID]
	if !ok {
		return Grant{}, fmt.Errorf("publication grant %q not found", grantID)
	}
	return cloneGrant(grant), nil
}

func (repository *GrantRepository) Reservation(grantID string) (*GrantReservation, error) {
	if err := validateID("grant_id", grantID); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	reservation, ok := state.reservations[grantID]
	if !ok {
		return nil, nil
	}
	copy := reservation
	return &copy, nil
}

func (repository *GrantRepository) Reserve(
	ctx context.Context,
	grantID string,
	grantSHA256 string,
	publicationID string,
	idempotencyKey string,
	reservedAt time.Time,
) (GrantReservation, error) {
	if ctx == nil {
		return GrantReservation{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return GrantReservation{}, err
	}
	candidate := GrantReservation{
		SchemaVersion: GrantReservationSchemaVersion,
		GrantID:       grantID, GrantSHA256: grantSHA256,
		PublicationID: publicationID, IdempotencyKey: idempotencyKey,
		ReservedAt: reservedAt,
	}
	if err := candidate.Validate(); err != nil {
		return GrantReservation{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return GrantReservation{}, err
	}
	grant, ok := state.grants[grantID]
	if !ok {
		return GrantReservation{}, fmt.Errorf("publication grant %q not found", grantID)
	}
	if candidate.GrantSHA256 != grant.GrantSHA256 ||
		candidate.PublicationID != grant.PublicationID ||
		candidate.IdempotencyKey != grant.PublicationIdempotencyKey {
		return GrantReservation{}, grantConflictf("reservation does not match grant %q", grantID)
	}
	if candidate.ReservedAt.Before(grant.GrantedAt) || !candidate.ReservedAt.Before(grant.ExpiresAt) {
		return GrantReservation{}, ErrGrantExpired
	}
	if existing, exists := state.reservations[grantID]; exists {
		if !exactPublicationJSONEqual(existing, candidate) {
			return GrantReservation{}, fmt.Errorf("%w: grant %q", ErrGrantConsumed, grantID)
		}
		return existing, nil
	}
	if err := ctx.Err(); err != nil {
		return GrantReservation{}, err
	}
	_, err = repository.store.AppendJSONLAtSequence(
		grantLedgerStream,
		uint64(state.eventCount),
		local.Event{
			ID:     grantReservationEventID(grantID),
			Schema: GrantReservationSchemaVersion,
			Time:   candidate.ReservedAt, Payload: candidate,
		},
	)
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return GrantReservation{}, grantConflictf("grant ledger changed while reserving %q", grantID)
		}
		return GrantReservation{}, fmt.Errorf("append grant reservation: %w", err)
	}
	return candidate, nil
}

type grantLedgerState struct {
	eventCount             int
	grants                 map[string]Grant
	grantsByMutation       map[string]Grant
	grantsByPublication    map[string]Grant
	grantsByPublicationKey map[string]Grant
	reservations           map[string]GrantReservation
}

func (repository *GrantRepository) load() (grantLedgerState, error) {
	state := grantLedgerState{
		grants:                 make(map[string]Grant),
		grantsByMutation:       make(map[string]Grant),
		grantsByPublication:    make(map[string]Grant),
		grantsByPublicationKey: make(map[string]Grant),
		reservations:           make(map[string]GrantReservation),
	}
	envelopes, err := repository.store.ReadJSONL(grantLedgerStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return grantLedgerState{}, grantCorruptf("read ledger: %v", err)
	}
	for _, envelope := range envelopes {
		state.eventCount++
		switch envelope.Schema {
		case GrantSchemaVersion:
			grant, err := DecodeGrantJSON(envelope.Payload)
			if err != nil {
				return grantLedgerState{}, grantCorruptf("sequence %d grant: %v", envelope.Sequence, err)
			}
			if envelope.ID != grant.IdempotencyKey || !envelope.Time.Equal(grant.GrantedAt) {
				return grantLedgerState{}, grantCorruptf("sequence %d grant envelope mismatch", envelope.Sequence)
			}
			if _, duplicate := state.grants[grant.GrantID]; duplicate {
				return grantLedgerState{}, grantCorruptf("duplicate grant id %q", grant.GrantID)
			}
			if _, duplicate := state.grantsByMutation[grant.IdempotencyKey]; duplicate {
				return grantLedgerState{}, grantCorruptf("duplicate grant mutation key %q", grant.IdempotencyKey)
			}
			if _, duplicate := state.grantsByPublication[grant.PublicationID]; duplicate {
				return grantLedgerState{}, grantCorruptf("duplicate publication id %q", grant.PublicationID)
			}
			if _, duplicate := state.grantsByPublicationKey[grant.PublicationIdempotencyKey]; duplicate {
				return grantLedgerState{}, grantCorruptf("duplicate publication idempotency key %q", grant.PublicationIdempotencyKey)
			}
			grant = cloneGrant(grant)
			state.grants[grant.GrantID] = grant
			state.grantsByMutation[grant.IdempotencyKey] = grant
			state.grantsByPublication[grant.PublicationID] = grant
			state.grantsByPublicationKey[grant.PublicationIdempotencyKey] = grant
		case GrantReservationSchemaVersion:
			reservation, err := DecodeGrantReservationJSON(envelope.Payload)
			if err != nil {
				return grantLedgerState{}, grantCorruptf("sequence %d reservation: %v", envelope.Sequence, err)
			}
			if envelope.ID != grantReservationEventID(reservation.GrantID) ||
				!envelope.Time.Equal(reservation.ReservedAt) {
				return grantLedgerState{}, grantCorruptf("sequence %d reservation envelope mismatch", envelope.Sequence)
			}
			grant, exists := state.grants[reservation.GrantID]
			if !exists {
				return grantLedgerState{}, grantCorruptf("reservation precedes grant %q", reservation.GrantID)
			}
			if reservation.GrantSHA256 != grant.GrantSHA256 ||
				reservation.PublicationID != grant.PublicationID ||
				reservation.IdempotencyKey != grant.PublicationIdempotencyKey ||
				reservation.ReservedAt.Before(grant.GrantedAt) ||
				!reservation.ReservedAt.Before(grant.ExpiresAt) {
				return grantLedgerState{}, grantCorruptf("reservation does not match grant %q", reservation.GrantID)
			}
			if _, duplicate := state.reservations[reservation.GrantID]; duplicate {
				return grantLedgerState{}, grantCorruptf("duplicate reservation for grant %q", reservation.GrantID)
			}
			state.reservations[reservation.GrantID] = reservation
		default:
			return grantLedgerState{}, grantCorruptf(
				"sequence %d has unsupported schema %q", envelope.Sequence, envelope.Schema,
			)
		}
	}
	return state, nil
}

func grantFrom(request GrantRequest, mutation GrantMutation, binding GrantBinding) Grant {
	return Grant{
		SchemaVersion: GrantSchemaVersion,
		GrantID:       request.GrantID, PublicationID: request.PublicationID,
		PublicationIdempotencyKey: request.PublicationIdempotencyKey,
		RunID:                     request.RunID, FindingID: request.FindingID,
		ChangeKind: request.ChangeKind, ChangeID: request.ChangeID,
		Channel:                    request.Channel,
		ExpectedPermissionRevision: request.ExpectedPermissionRevision,
		TenantID:                   binding.TenantID, WorkspaceID: binding.WorkspaceID,
		DecisionID: binding.DecisionID, Provider: request.Provider,
		RepositoryID: request.RepositoryID, BaseRevision: binding.BaseRevision,
		ExpectedHeadRevision:  binding.ExpectedHeadRevision,
		TargetSnapshotRef:     binding.TargetSnapshotRef,
		ConfigBundleRef:       binding.ConfigBundleRef,
		FindingSourceRef:      binding.FindingSourceRef,
		FindingSourceContract: binding.FindingSourceContract,
		OccurredAt:            request.OccurredAt, GrantedAt: mutation.At, ExpiresAt: request.ExpiresAt,
		GrantedBy: mutation.Actor, Roles: append([]GrantRole(nil), mutation.Roles...),
		Audit: mutation.Audit, IdempotencyKey: mutation.IdempotencyKey,
	}
}

func cloneGrantRequest(request GrantRequest) GrantRequest { return request }

func cloneGrantMutation(mutation GrantMutation) GrantMutation {
	mutation.Roles = append([]GrantRole(nil), mutation.Roles...)
	return mutation
}

func cloneGrant(grant Grant) Grant {
	grant.Roles = append([]GrantRole(nil), grant.Roles...)
	return grant
}

func grantReservationEventID(grantID string) string {
	digest := sha256.Sum256([]byte(grantID))
	return "grant-reservation-" + hex.EncodeToString(digest[:])[:24]
}

func exactPublicationJSONEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func grantConflictf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrGrantConflict, fmt.Sprintf(format, arguments...))
}

func grantCorruptf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrGrantCorrupt, fmt.Sprintf(format, arguments...))
}
