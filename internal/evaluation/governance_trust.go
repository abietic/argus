package evaluation

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
)

func (repository *Repository) RegisterGovernanceTrustKey(
	ctx context.Context,
	registration GovernanceTrustKeyRegistration,
	mutation Mutation,
) (GovernanceTrustKeyRecord, error) {
	if err := checkContext(ctx); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	if err := registration.Validate(); err != nil {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("validate governance trust key registration: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	if !registration.RegisteredAt.Equal(mutation.At.UTC()) {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("registered_at must equal mutation time")
	}
	if err := authorizeGovernanceTrustAdmin(mutation.Roles); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetGovernanceTrustKeyRegistered,
		TrustKeyRegistration: clonePointer(registration), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	identity := governanceTrustKeyIdentity(
		registration.Key.Authority, registration.Key.KeyID, registration.Key.Revision,
	)

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return GovernanceTrustKeyRecord{}, err
		}
		return state.governanceTrustKey(identity)
	}
	if _, exists := state.governanceTrustKeys[identity]; exists {
		return GovernanceTrustKeyRecord{}, fmt.Errorf(
			"%w: governance trust key %q/%q/%q already exists",
			ErrConflict, registration.Key.Authority, registration.Key.KeyID, registration.Key.Revision,
		)
	}
	if _, err := repository.appendDatasetAtSequence(
		mutation, event, state.streamSequences[datasetStream],
	); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	return reloaded.governanceTrustKey(identity)
}

func (repository *Repository) RevokeGovernanceTrustKey(
	ctx context.Context,
	revocation GovernanceTrustKeyRevocation,
	mutation Mutation,
) (GovernanceTrustKeyRecord, error) {
	if err := checkContext(ctx); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	if err := revocation.Validate(); err != nil {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("validate governance trust key revocation: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	if !revocation.RevokedAt.Equal(mutation.At.UTC()) {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("revoked_at must equal mutation time")
	}
	if err := authorizeGovernanceTrustAdmin(mutation.Roles); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetGovernanceTrustKeyRevoked,
		TrustKeyRevocation: clonePointer(revocation), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	identity := governanceTrustKeyIdentity(revocation.Authority, revocation.KeyID, revocation.Revision)

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return GovernanceTrustKeyRecord{}, err
		}
		return state.governanceTrustKey(identity)
	}
	record, exists := state.governanceTrustKeys[identity]
	if !exists {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("%w: governance trust key", ErrNotFound)
	}
	if record.RevokedAt != nil {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("%w: governance trust key is already revoked", ErrInvalidTransition)
	}
	if revocation.RevokedAt.Before(record.RegisteredAt) {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("%w: revocation predates registration", ErrInvalidTransition)
	}
	if _, err := repository.appendDatasetAtSequence(
		mutation, event, state.streamSequences[datasetStream],
	); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	return reloaded.governanceTrustKey(identity)
}

func (repository *Repository) GetGovernanceTrustKey(
	authority string,
	keyID string,
	revision string,
	access Access,
) (GovernanceTrustKeyRecord, error) {
	for _, field := range []struct{ name, value string }{
		{"authority", authority}, {"key_id", keyID}, {"revision", revision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return GovernanceTrustKeyRecord{}, err
		}
	}
	if err := access.Validate(); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	if err := authorizeGovernanceTrustAdmin(access.Roles); err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return GovernanceTrustKeyRecord{}, err
	}
	return state.governanceTrustKey(governanceTrustKeyIdentity(authority, keyID, revision))
}

func (repository *Repository) ListGovernanceTrustKeys(access Access) ([]GovernanceTrustKeyRecord, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	if err := authorizeGovernanceTrustAdmin(access.Roles); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	identities := make([]string, 0, len(state.governanceTrustKeys))
	for identity := range state.governanceTrustKeys {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	result := make([]GovernanceTrustKeyRecord, 0, len(identities))
	for _, identity := range identities {
		record, err := state.governanceTrustKey(identity)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func (state *projectionState) governanceTrustKey(identity string) (GovernanceTrustKeyRecord, error) {
	record, exists := state.governanceTrustKeys[identity]
	if !exists {
		return GovernanceTrustKeyRecord{}, fmt.Errorf("%w: governance trust key", ErrNotFound)
	}
	return cloneValue(record), nil
}

func (state *projectionState) authorizeGovernanceKeyForImport(
	request ExternalGovernedCaseImport,
	importActor string,
) error {
	identity := governanceTrustKeyIdentity(
		request.TrustedKey.Authority, request.TrustedKey.KeyID, request.TrustedKey.Revision,
	)
	record, exists := state.governanceTrustKeys[identity]
	if !exists {
		return fmt.Errorf("%w: external governance key revision is not registered", ErrUnauthorized)
	}
	if !reflect.DeepEqual(record.Key, request.TrustedKey) {
		return fmt.Errorf("%w: external governance key does not match registered revision", ErrUnauthorized)
	}
	if record.RevokedAt != nil {
		return fmt.Errorf("%w: external governance key revision is revoked", ErrUnauthorized)
	}
	if record.RegisteredAt.After(request.ImportedAt) {
		return fmt.Errorf("%w: external governance key was registered after import", ErrUnauthorized)
	}
	if record.RegisteredBy == importActor ||
		record.RegisteredBy == request.Attestation.AdjudicatorID ||
		slices.Contains(request.Attestation.ReviewerIDs, record.RegisteredBy) {
		return fmt.Errorf("%w: trust administrator must be independent from import and external review actors", ErrUnauthorized)
	}
	return nil
}

func governanceTrustKeyIdentity(authority, keyID, revision string) string {
	return authority + "\x00" + keyID + "\x00" + revision
}

func authorizeGovernanceTrustAdmin(roles []Role) error {
	if !hasRole(roles, RoleGovernanceTrustAdmin) {
		return fmt.Errorf("%w: governance_trust_admin role is required", ErrUnauthorized)
	}
	return nil
}
