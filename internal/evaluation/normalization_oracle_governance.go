package evaluation

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"argus.local/argus/internal/runmodel"
)

const (
	NormalizationOracleAttestationSchemaVersion  = "argus.normalization_oracle_attestation.v1alpha1"
	NormalizationOracleRegistrationSchemaVersion = "argus.normalization_oracle_registration.v1alpha1"
	NormalizationOracleRevocationSchemaVersion   = "argus.normalization_oracle_revocation.v1alpha1"
)

// NormalizationOracleAttestation is issued outside Argus. Its signature binds
// the exact content-addressed oracle and the exact trust-key revision. Reviewer,
// adjudicator, evidence, and exposure facts are covered through OracleSHA256.
type NormalizationOracleAttestation struct {
	SchemaVersion   string    `json:"schema_version"`
	AttestationID   string    `json:"attestation_id"`
	OracleID        string    `json:"oracle_id"`
	OracleSHA256    string    `json:"oracle_sha256"`
	Authority       string    `json:"authority"`
	KeyID           string    `json:"key_id"`
	KeyRevision     string    `json:"key_revision"`
	PolicyRevision  string    `json:"policy_revision"`
	Decision        string    `json:"decision"`
	IssuedAt        time.Time `json:"issued_at"`
	SignatureBase64 string    `json:"signature_base64"`
}

type normalizationOracleSigningPayload struct {
	SchemaVersion  string    `json:"schema_version"`
	AttestationID  string    `json:"attestation_id"`
	OracleID       string    `json:"oracle_id"`
	OracleSHA256   string    `json:"oracle_sha256"`
	Authority      string    `json:"authority"`
	KeyID          string    `json:"key_id"`
	KeyRevision    string    `json:"key_revision"`
	PolicyRevision string    `json:"policy_revision"`
	Decision       string    `json:"decision"`
	IssuedAt       time.Time `json:"issued_at"`
}

// NormalizationOracleRegistration is the self-contained append-only event
// payload. Keeping Oracle in the event allows repository restoration to verify
// the signature and semantic binding without trusting the artifact store.
type NormalizationOracleRegistration struct {
	SchemaVersion                    string                         `json:"schema_version"`
	Oracle                           NormalizationOracle            `json:"oracle"`
	OracleRef                        runmodel.ArtifactRef           `json:"oracle_ref"`
	Attestation                      NormalizationOracleAttestation `json:"attestation"`
	TrustedKey                       TrustedGovernanceKey           `json:"trusted_key"`
	ExpectedCurrentRevision          uint64                         `json:"expected_current_revision"`
	ExpectedCurrentRegisteredEventID string                         `json:"expected_current_registered_event_id"`
	RegisteredAt                     time.Time                      `json:"registered_at"`
}

type NormalizationOracleRevocation struct {
	SchemaVersion             string    `json:"schema_version"`
	OracleID                  string    `json:"oracle_id"`
	ExpectedRevision          uint64    `json:"expected_revision"`
	ExpectedRegisteredEventID string    `json:"expected_registered_event_id"`
	Reason                    string    `json:"reason"`
	RevokedAt                 time.Time `json:"revoked_at"`
}

// NormalizationOracleBinding is the minimum exact governance closure consumed
// by a quality run. It cannot silently float to a later oracle or trust key.
type NormalizationOracleBinding struct {
	OracleID                  string               `json:"oracle_id"`
	CaseID                    string               `json:"case_id"`
	Revision                  uint64               `json:"revision"`
	RegisteredEventID         string               `json:"registered_event_id"`
	OracleRef                 runmodel.ArtifactRef `json:"oracle_ref"`
	AttestationID             string               `json:"attestation_id"`
	TrustAuthority            string               `json:"trust_authority"`
	TrustKeyID                string               `json:"trust_key_id"`
	TrustKeyRevision          string               `json:"trust_key_revision"`
	TrustKeyRegisteredEventID string               `json:"trust_key_registered_event_id"`
}

type NormalizationOracleRecord struct {
	Binding                   NormalizationOracleBinding     `json:"binding"`
	Split                     Split                          `json:"split"`
	LabelRevision             uint64                         `json:"label_revision"`
	Attestation               NormalizationOracleAttestation `json:"attestation"`
	TrustedKey                TrustedGovernanceKey           `json:"trusted_key"`
	ReviewerIDs               []string                       `json:"reviewer_ids"`
	AdjudicatorID             string                         `json:"adjudicator_id"`
	PreviousRegisteredEventID string                         `json:"previous_registered_event_id,omitempty"`
	RegisteredAt              time.Time                      `json:"registered_at"`
	RegisteredBy              string                         `json:"registered_by"`
	RevokedEventID            string                         `json:"revoked_event_id,omitempty"`
	RevokedAt                 *time.Time                     `json:"revoked_at,omitempty"`
	RevokedBy                 string                         `json:"revoked_by,omitempty"`
	RevocationReason          string                         `json:"revocation_reason,omitempty"`
}

func (attestation NormalizationOracleAttestation) SigningBytes() ([]byte, error) {
	return json.Marshal(normalizationOracleSigningPayload{
		SchemaVersion: attestation.SchemaVersion, AttestationID: attestation.AttestationID,
		OracleID: attestation.OracleID, OracleSHA256: attestation.OracleSHA256,
		Authority: attestation.Authority, KeyID: attestation.KeyID, KeyRevision: attestation.KeyRevision,
		PolicyRevision: attestation.PolicyRevision, Decision: attestation.Decision, IssuedAt: attestation.IssuedAt,
	})
}

func (attestation NormalizationOracleAttestation) Validate() error {
	if attestation.SchemaVersion != NormalizationOracleAttestationSchemaVersion {
		return fmt.Errorf("unsupported normalization oracle attestation schema %q", attestation.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{"attestation_id", attestation.AttestationID}, {"oracle_id", attestation.OracleID},
		{"authority", attestation.Authority}, {"key_id", attestation.KeyID},
		{"key_revision", attestation.KeyRevision}, {"policy_revision", attestation.PolicyRevision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := validateSHA256("oracle_sha256", attestation.OracleSHA256); err != nil {
		return err
	}
	if attestation.Decision != "approved" {
		return fmt.Errorf("normalization oracle attestation decision must be approved")
	}
	if attestation.IssuedAt.IsZero() || attestation.IssuedAt.Location() != time.UTC {
		return fmt.Errorf("normalization oracle attestation issued_at must be non-zero UTC")
	}
	signature, err := base64.StdEncoding.DecodeString(attestation.SignatureBase64)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("signature_base64 must encode an Ed25519 signature")
	}
	return nil
}

func (registration NormalizationOracleRegistration) Validate() error {
	if registration.SchemaVersion != NormalizationOracleRegistrationSchemaVersion {
		return fmt.Errorf("unsupported normalization oracle registration schema %q", registration.SchemaVersion)
	}
	if err := registration.Oracle.Validate(); err != nil {
		return fmt.Errorf("oracle: %w", err)
	}
	if err := registration.OracleRef.Validate(); err != nil {
		return fmt.Errorf("oracle_ref: %w", err)
	}
	if registration.OracleRef.Contract != NormalizationOracleContract {
		return fmt.Errorf("oracle_ref contract must be %q", NormalizationOracleContract)
	}
	oracleBytes, err := json.Marshal(registration.Oracle)
	if err != nil {
		return fmt.Errorf("marshal normalization oracle: %w", err)
	}
	digest, err := runmodel.DigestJSON(registration.Oracle)
	if err != nil || digest != registration.OracleRef.SHA256 || int64(len(oracleBytes)) != registration.OracleRef.SizeBytes {
		return fmt.Errorf("oracle_ref does not bind the exact canonical oracle")
	}
	if err := registration.Attestation.Validate(); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	if err := registration.TrustedKey.Validate(); err != nil {
		return fmt.Errorf("trusted_key: %w", err)
	}
	if registration.Attestation.OracleID != registration.Oracle.OracleID ||
		registration.Attestation.OracleSHA256 != registration.OracleRef.SHA256 ||
		registration.Attestation.Authority != registration.Oracle.Adjudication.Authority ||
		registration.Attestation.PolicyRevision != registration.Oracle.Adjudication.Revision ||
		registration.Attestation.Authority != registration.TrustedKey.Authority ||
		registration.Attestation.KeyID != registration.TrustedKey.KeyID ||
		registration.Attestation.KeyRevision != registration.TrustedKey.Revision {
		return fmt.Errorf("normalization oracle attestation identity does not match oracle and trusted key")
	}
	if registration.Attestation.IssuedAt.Before(registration.Oracle.CreatedAt) ||
		registration.Attestation.IssuedAt.Before(registration.TrustedKey.ValidFrom) ||
		registration.Attestation.IssuedAt.After(registration.TrustedKey.ValidUntil) {
		return fmt.Errorf("normalization oracle attestation issuance is outside the oracle/key validity interval")
	}
	payload, err := registration.Attestation.SigningBytes()
	if err != nil {
		return err
	}
	publicKey, _ := base64.StdEncoding.DecodeString(registration.TrustedKey.PublicKeyBase64)
	signature, _ := base64.StdEncoding.DecodeString(registration.Attestation.SignatureBase64)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return fmt.Errorf("normalization oracle attestation signature is invalid")
	}
	if registration.ExpectedCurrentRevision == 0 {
		if registration.ExpectedCurrentRegisteredEventID != "" {
			return fmt.Errorf("first registration must use an empty expected current event ID")
		}
	} else if err := validateID("expected_current_registered_event_id", registration.ExpectedCurrentRegisteredEventID); err != nil {
		return err
	}
	if registration.RegisteredAt.IsZero() || registration.RegisteredAt.Location() != time.UTC ||
		registration.RegisteredAt.Before(registration.Attestation.IssuedAt) {
		return fmt.Errorf("registered_at must be UTC and not precede attestation issuance")
	}
	return nil
}

func (revocation NormalizationOracleRevocation) Validate() error {
	if revocation.SchemaVersion != NormalizationOracleRevocationSchemaVersion {
		return fmt.Errorf("unsupported normalization oracle revocation schema %q", revocation.SchemaVersion)
	}
	if err := validateID("oracle_id", revocation.OracleID); err != nil {
		return err
	}
	if revocation.ExpectedRevision == 0 {
		return fmt.Errorf("expected_revision must be positive")
	}
	if err := validateID("expected_registered_event_id", revocation.ExpectedRegisteredEventID); err != nil {
		return err
	}
	if err := validateText("reason", revocation.Reason, 4096, true); err != nil {
		return err
	}
	if revocation.RevokedAt.IsZero() || revocation.RevokedAt.Location() != time.UTC {
		return fmt.Errorf("revoked_at must be non-zero UTC")
	}
	return nil
}

func (binding NormalizationOracleBinding) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"oracle_id", binding.OracleID}, {"case_id", binding.CaseID},
		{"registered_event_id", binding.RegisteredEventID}, {"attestation_id", binding.AttestationID},
		{"trust_authority", binding.TrustAuthority}, {"trust_key_id", binding.TrustKeyID},
		{"trust_key_revision", binding.TrustKeyRevision},
		{"trust_key_registered_event_id", binding.TrustKeyRegisteredEventID},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if binding.Revision == 0 {
		return fmt.Errorf("normalization oracle binding revision must be positive")
	}
	if err := binding.OracleRef.Validate(); err != nil {
		return fmt.Errorf("oracle_ref: %w", err)
	}
	if binding.OracleRef.Contract != NormalizationOracleContract {
		return fmt.Errorf("oracle_ref contract must be %q", NormalizationOracleContract)
	}
	return nil
}

func DecodeNormalizationOracleRegistration(data []byte) (NormalizationOracleRegistration, error) {
	return decodeStrict(data, "NormalizationOracleRegistration", func(value NormalizationOracleRegistration) error {
		return value.Validate()
	})
}

func DecodeNormalizationOracleAttestation(data []byte) (NormalizationOracleAttestation, error) {
	return decodeStrict(data, "NormalizationOracleAttestation", func(value NormalizationOracleAttestation) error {
		return value.Validate()
	})
}

func DecodeNormalizationOracleRevocation(data []byte) (NormalizationOracleRevocation, error) {
	return decodeStrict(data, "NormalizationOracleRevocation", func(value NormalizationOracleRevocation) error {
		return value.Validate()
	})
}

func (repository *Repository) RegisterNormalizationOracle(
	ctx context.Context,
	registration NormalizationOracleRegistration,
	mutation Mutation,
) (NormalizationOracleRecord, error) {
	if err := checkContext(ctx); err != nil {
		return NormalizationOracleRecord{}, err
	}
	if err := registration.Validate(); err != nil {
		return NormalizationOracleRecord{}, fmt.Errorf("validate normalization oracle registration: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return NormalizationOracleRecord{}, err
	}
	if !registration.RegisteredAt.Equal(mutation.At.UTC()) {
		return NormalizationOracleRecord{}, fmt.Errorf("registered_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetNormalizationOracleRegistered,
		NormalizationOracleRegistration: clonePointer(registration), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return NormalizationOracleRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return NormalizationOracleRecord{}, err
		}
		return state.normalizationOracle(registration.Oracle.OracleID)
	}
	if err := state.validateNormalizationOracleRegistration(registration, mutation.Actor, mutation.Roles); err != nil {
		return NormalizationOracleRecord{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return NormalizationOracleRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return NormalizationOracleRecord{}, err
	}
	return reloaded.normalizationOracle(registration.Oracle.OracleID)
}

func (repository *Repository) RevokeNormalizationOracle(
	ctx context.Context,
	revocation NormalizationOracleRevocation,
	mutation Mutation,
) (NormalizationOracleRecord, error) {
	if err := checkContext(ctx); err != nil {
		return NormalizationOracleRecord{}, err
	}
	if err := revocation.Validate(); err != nil {
		return NormalizationOracleRecord{}, fmt.Errorf("validate normalization oracle revocation: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return NormalizationOracleRecord{}, err
	}
	if !revocation.RevokedAt.Equal(mutation.At.UTC()) {
		return NormalizationOracleRecord{}, fmt.Errorf("revoked_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetNormalizationOracleRevoked,
		NormalizationOracleRevocation: clonePointer(revocation), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return NormalizationOracleRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return NormalizationOracleRecord{}, err
		}
		return state.normalizationOracle(revocation.OracleID)
	}
	if err := state.validateNormalizationOracleRevocation(revocation, mutation.Actor, mutation.Roles); err != nil {
		return NormalizationOracleRecord{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return NormalizationOracleRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return NormalizationOracleRecord{}, err
	}
	return reloaded.normalizationOracle(revocation.OracleID)
}

func (repository *Repository) GetNormalizationOracle(
	oracleID string,
	access Access,
) (NormalizationOracleRecord, error) {
	if err := validateID("oracle_id", oracleID); err != nil {
		return NormalizationOracleRecord{}, err
	}
	if err := access.Validate(); err != nil {
		return NormalizationOracleRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return NormalizationOracleRecord{}, err
	}
	record, err := state.normalizationOracle(oracleID)
	if err != nil {
		return NormalizationOracleRecord{}, err
	}
	if err := state.authorizeNormalizationOracleRecord(record, access.Roles, false); err != nil {
		return NormalizationOracleRecord{}, err
	}
	return record, nil
}

func (repository *Repository) ListNormalizationOracles(access Access) ([]NormalizationOracleRecord, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.normalizationOracles))
	for id, record := range state.normalizationOracles {
		if state.authorizeNormalizationOracleRecord(record, access.Roles, false) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]NormalizationOracleRecord, 0, len(ids))
	for _, id := range ids {
		record, err := state.normalizationOracle(id)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

// AuthorizeNormalizationOracleBindings resolves all bindings from one loaded
// projection so a caller cannot accidentally combine different registry views.
func (repository *Repository) AuthorizeNormalizationOracleBindings(
	bindings []NormalizationOracleBinding,
	access Access,
) ([]NormalizationOracleRecord, error) {
	if len(bindings) == 0 || len(bindings) > 1024 {
		return nil, fmt.Errorf("normalization oracle bindings must contain between 1 and 1024 entries")
	}
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]NormalizationOracleRecord, 0, len(bindings))
	for index, binding := range bindings {
		if err := binding.Validate(); err != nil {
			return nil, fmt.Errorf("oracle_bindings[%d]: %w", index, err)
		}
		record, err := state.normalizationOracle(binding.OracleID)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(record.Binding, binding) {
			return nil, fmt.Errorf("%w: normalization oracle binding is not the current exact registration", ErrConflict)
		}
		if err := state.authorizeNormalizationOracleRecord(record, access.Roles, true); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func (state *projectionState) normalizationOracle(id string) (NormalizationOracleRecord, error) {
	record, ok := state.normalizationOracles[id]
	if !ok {
		return NormalizationOracleRecord{}, fmt.Errorf("%w: normalization oracle %q", ErrNotFound, id)
	}
	return cloneValue(record), nil
}

func (state *projectionState) validateNormalizationOracleRegistration(
	registration NormalizationOracleRegistration,
	actor string,
	roles []Role,
) error {
	oracle := registration.Oracle
	caseRecord, ok := state.cases[oracle.CaseID]
	if !ok {
		return fmt.Errorf("%w: normalization oracle case %q", ErrNotFound, oracle.CaseID)
	}
	currentCase := caseRecord.CurrentCase()
	if err := authorizeCaseWrite(currentCase, roles); err != nil {
		return err
	}
	if currentCase.ReviewState != ReviewApproved ||
		(currentCase.DatasetState != DatasetGold && currentCase.DatasetState != DatasetActive) ||
		!currentCase.Eligibility.Evaluation || currentCase.Split != oracle.Split ||
		caseRecord.CurrentLabelRevision != oracle.LabelRevision {
		return fmt.Errorf("%w: normalization oracle requires the current approved evaluation case revision", ErrInvalidTransition)
	}
	if actor == oracle.Adjudication.AdjudicatorID || slices.Contains(oracle.Adjudication.ReviewerIDs, actor) {
		return fmt.Errorf("%w: oracle registration operator must be independent from reviewers and adjudicator", ErrUnauthorized)
	}
	keyIdentity := governanceTrustKeyIdentity(registration.TrustedKey.Authority, registration.TrustedKey.KeyID, registration.TrustedKey.Revision)
	keyRecord, ok := state.governanceTrustKeys[keyIdentity]
	if !ok || !reflect.DeepEqual(keyRecord.Key, registration.TrustedKey) {
		return fmt.Errorf("%w: normalization oracle trust key revision is not registered exactly", ErrUnauthorized)
	}
	if keyRecord.RevokedAt != nil {
		return fmt.Errorf("%w: normalization oracle trust key revision is revoked", ErrUnauthorized)
	}
	if keyRecord.RegisteredAt.After(registration.Attestation.IssuedAt) || keyRecord.RegisteredAt.After(registration.RegisteredAt) {
		return fmt.Errorf("%w: normalization oracle trust key was registered too late", ErrUnauthorized)
	}
	if keyRecord.RegisteredBy == actor || keyRecord.RegisteredBy == oracle.Adjudication.AdjudicatorID ||
		slices.Contains(oracle.Adjudication.ReviewerIDs, keyRecord.RegisteredBy) {
		return fmt.Errorf("%w: trust administrator must be independent from oracle governance actors", ErrUnauthorized)
	}
	if !slices.Contains(registration.TrustedKey.RepositoryIDs, currentCase.Provenance.RepositoryID) ||
		!slices.Contains(registration.TrustedKey.AllowedClassifications, currentCase.Classification) {
		return fmt.Errorf("%w: normalization oracle trust key scope does not authorize case repository and classification", ErrUnauthorized)
	}
	current, exists := state.normalizationOracles[oracle.OracleID]
	if !exists {
		if registration.ExpectedCurrentRevision != 0 || registration.ExpectedCurrentRegisteredEventID != "" {
			return fmt.Errorf("%w: first normalization oracle registration CAS does not match empty state", ErrConflict)
		}
		return nil
	}
	if registration.ExpectedCurrentRevision != current.Binding.Revision ||
		registration.ExpectedCurrentRegisteredEventID != current.Binding.RegisteredEventID {
		return fmt.Errorf("%w: normalization oracle registration CAS does not match current revision", ErrConflict)
	}
	if current.Binding.CaseID != oracle.CaseID {
		return fmt.Errorf("%w: normalization oracle identity cannot move to another case", ErrInvalidTransition)
	}
	if current.Binding.OracleRef == registration.OracleRef {
		return fmt.Errorf("%w: normalization oracle registration does not change the oracle", ErrInvalidTransition)
	}
	if !registration.RegisteredAt.After(current.RegisteredAt) {
		return fmt.Errorf("%w: replacement normalization oracle must be registered later", ErrInvalidTransition)
	}
	return nil
}

func (state *projectionState) validateNormalizationOracleRevocation(
	revocation NormalizationOracleRevocation,
	actor string,
	roles []Role,
) error {
	record, ok := state.normalizationOracles[revocation.OracleID]
	if !ok {
		return fmt.Errorf("%w: normalization oracle %q", ErrNotFound, revocation.OracleID)
	}
	caseRecord, ok := state.cases[record.Binding.CaseID]
	if !ok {
		return fmt.Errorf("normalization oracle references unknown case")
	}
	if err := authorizeCaseWrite(caseRecord.CurrentCase(), roles); err != nil {
		return err
	}
	if revocation.ExpectedRevision != record.Binding.Revision ||
		revocation.ExpectedRegisteredEventID != record.Binding.RegisteredEventID {
		return fmt.Errorf("%w: normalization oracle revocation CAS does not match current revision", ErrConflict)
	}
	if record.RevokedAt != nil {
		return fmt.Errorf("%w: normalization oracle is already revoked", ErrInvalidTransition)
	}
	keyIdentity := governanceTrustKeyIdentity(record.TrustedKey.Authority, record.TrustedKey.KeyID, record.TrustedKey.Revision)
	keyRecord, ok := state.governanceTrustKeys[keyIdentity]
	if !ok {
		return fmt.Errorf("normalization oracle revocation references unknown trust key")
	}
	if actor == record.RegisteredBy || actor == record.AdjudicatorID ||
		actor == keyRecord.RegisteredBy || slices.Contains(record.ReviewerIDs, actor) {
		return fmt.Errorf("%w: normalization oracle revocation requires an independent operator", ErrUnauthorized)
	}
	if !revocation.RevokedAt.After(record.RegisteredAt) {
		return fmt.Errorf("%w: normalization oracle revocation must follow registration", ErrInvalidTransition)
	}
	return nil
}

func (state *projectionState) authorizeNormalizationOracleRecord(
	record NormalizationOracleRecord,
	roles []Role,
	requireActive bool,
) error {
	caseRecord, ok := state.cases[record.Binding.CaseID]
	if !ok {
		return fmt.Errorf("%w: normalization oracle case", ErrNotFound)
	}
	if err := authorizeCaseRead(caseRecord.CurrentCase(), roles); err != nil {
		return err
	}
	if requireActive {
		current := caseRecord.CurrentCase()
		if record.RevokedAt != nil {
			return fmt.Errorf("%w: normalization oracle registration is revoked", ErrUnauthorized)
		}
		if current.ReviewState != ReviewApproved ||
			(current.DatasetState != DatasetGold && current.DatasetState != DatasetActive) ||
			!current.Eligibility.Evaluation || current.Split != record.Split ||
			caseRecord.CurrentLabelRevision != record.LabelRevision {
			return fmt.Errorf("%w: normalization oracle case governance or label revision drifted", ErrContaminated)
		}
		keyIdentity := governanceTrustKeyIdentity(record.TrustedKey.Authority, record.TrustedKey.KeyID, record.TrustedKey.Revision)
		keyRecord, ok := state.governanceTrustKeys[keyIdentity]
		if !ok || keyRecord.RegisteredEventID != record.Binding.TrustKeyRegisteredEventID ||
			!reflect.DeepEqual(keyRecord.Key, record.TrustedKey) || keyRecord.RevokedAt != nil {
			return fmt.Errorf("%w: normalization oracle trust key is absent, changed, or revoked", ErrUnauthorized)
		}
	}
	return nil
}

func normalizationOracleRecordFromRegistration(
	registration NormalizationOracleRegistration,
	eventID string,
	actor string,
	keyRecord GovernanceTrustKeyRecord,
) NormalizationOracleRecord {
	revision := registration.ExpectedCurrentRevision + 1
	return NormalizationOracleRecord{
		Binding: NormalizationOracleBinding{
			OracleID: registration.Oracle.OracleID, CaseID: registration.Oracle.CaseID,
			Revision: revision, RegisteredEventID: eventID, OracleRef: registration.OracleRef,
			AttestationID:  registration.Attestation.AttestationID,
			TrustAuthority: registration.TrustedKey.Authority, TrustKeyID: registration.TrustedKey.KeyID,
			TrustKeyRevision:          registration.TrustedKey.Revision,
			TrustKeyRegisteredEventID: keyRecord.RegisteredEventID,
		},
		Split: registration.Oracle.Split, LabelRevision: registration.Oracle.LabelRevision,
		Attestation: cloneValue(registration.Attestation), TrustedKey: cloneValue(registration.TrustedKey),
		ReviewerIDs:               slices.Clone(registration.Oracle.Adjudication.ReviewerIDs),
		AdjudicatorID:             registration.Oracle.Adjudication.AdjudicatorID,
		PreviousRegisteredEventID: registration.ExpectedCurrentRegisteredEventID,
		RegisteredAt:              registration.RegisteredAt, RegisteredBy: actor,
	}
}
