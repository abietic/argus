package evaluation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
)

func TestNormalizationOracleRegistryVerifiesSignatureCASRestoreAndRevocation(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	evaluationCase := testActiveCase("normalization-governed-case", "normalization-governed-repo", SplitDev, testEpoch)
	createCase(t, repository, evaluationCase, RoleDatasetCurator)

	oracle := normalizationOracleFixture(t)
	oracle.OracleID = "normalization-governed-oracle"
	oracle.CaseID = evaluationCase.CaseID
	oracle.Split = evaluationCase.Split
	oracle.LabelRevision = 1
	oracle.Adjudication.AdjudicatedAt = testEpoch.Add(2 * time.Minute)
	oracle.CreatedAt = testEpoch.Add(3 * time.Minute)

	seed := sha256.Sum256([]byte("normalization-oracle-governance-test-key"))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	trustedKey := TrustedGovernanceKey{
		SchemaVersion: TrustedGovernanceKeySchemaVersion,
		Authority:     oracle.Adjudication.Authority, KeyID: "normalization-governance-key", Revision: "key-v1",
		PublicKeyBase64:        base64.StdEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
		RepositoryIDs:          []string{evaluationCase.Provenance.RepositoryID},
		AllowedClassifications: []Classification{evaluationCase.Classification},
		ValidFrom:              testEpoch, ValidUntil: testEpoch.Add(time.Hour),
	}
	keyRegisteredAt := testEpoch.Add(time.Minute)
	keyRecord, err := repository.RegisterGovernanceTrustKey(context.Background(), GovernanceTrustKeyRegistration{
		SchemaVersion: GovernanceTrustKeyRegistrationSchemaVersion, Key: trustedKey, RegisteredAt: keyRegisteredAt,
	}, Mutation{
		IdempotencyKey: "normalization-governance-key-register", Actor: "independent-oracle-trust-admin",
		Roles: []Role{RoleGovernanceTrustAdmin}, Audit: "register external oracle key", At: keyRegisteredAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	registration := signedNormalizationOracleRegistration(t, oracle, trustedKey, privateKey, 0, "", testEpoch.Add(5*time.Minute))
	mutation := Mutation{
		IdempotencyKey: "normalization-governance-oracle-register", Actor: "oracle-registry-curator",
		Roles: []Role{RoleDatasetCurator}, Audit: "register independently signed oracle", At: registration.RegisteredAt,
	}
	record, err := repository.RegisterNormalizationOracle(context.Background(), registration, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if record.Binding.Revision != 1 || record.Binding.RegisteredEventID != mutation.IdempotencyKey ||
		record.Binding.TrustKeyRegisteredEventID != keyRecord.RegisteredEventID {
		t.Fatalf("registered normalization oracle = %+v", record)
	}
	if retry, err := repository.RegisterNormalizationOracle(context.Background(), registration, mutation); err != nil ||
		!reflect.DeepEqual(retry, record) {
		t.Fatalf("idempotent normalization oracle registration = %+v, %v", retry, err)
	}

	staleOracle := oracle
	staleOracle.CreatedAt = oracle.CreatedAt.Add(time.Minute)
	staleOracle.Adjudication.AdjudicatedAt = oracle.Adjudication.AdjudicatedAt.Add(time.Minute)
	stale := signedNormalizationOracleRegistration(t, staleOracle, trustedKey, privateKey, 0, "", testEpoch.Add(7*time.Minute))
	if _, err := repository.RegisterNormalizationOracle(context.Background(), stale, Mutation{
		IdempotencyKey: "normalization-governance-stale-register", Actor: "second-oracle-curator",
		Roles: []Role{RoleDatasetCurator}, Audit: "stale CAS replacement", At: stale.RegisteredAt,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale normalization oracle CAS error = %v", err)
	}

	tampered := registration
	tampered.Attestation.SignatureBase64 = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "signature is invalid") {
		t.Fatalf("tampered oracle signature error = %v", err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	access := Access{Actor: "oracle-quality-runner", Roles: []Role{RoleDatasetCurator}}
	restored, err := restarted.AuthorizeNormalizationOracleBindings([]NormalizationOracleBinding{record.Binding}, access)
	if err != nil || len(restored) != 1 || !reflect.DeepEqual(restored[0], record) {
		t.Fatalf("restored normalization oracle = %+v, %v", restored, err)
	}

	revokedAt := registration.RegisteredAt.Add(time.Minute)
	revocation := NormalizationOracleRevocation{
		SchemaVersion: NormalizationOracleRevocationSchemaVersion,
		OracleID:      record.Binding.OracleID, ExpectedRevision: record.Binding.Revision,
		ExpectedRegisteredEventID: record.Binding.RegisteredEventID,
		Reason:                    "adjudication correction required", RevokedAt: revokedAt,
	}
	if _, err := restarted.RevokeNormalizationOracle(context.Background(), revocation, Mutation{
		IdempotencyKey: "normalization-governance-reviewer-revoke", Actor: oracle.Adjudication.ReviewerIDs[0],
		Roles: []Role{RoleDatasetCurator}, Audit: "reviewer must not revoke its oracle", At: revokedAt,
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reviewer oracle revocation error = %v", err)
	}
	revoked, err := restarted.RevokeNormalizationOracle(context.Background(), revocation, Mutation{
		IdempotencyKey: "normalization-governance-oracle-revoke", Actor: "independent-revocation-curator",
		Roles: []Role{RoleDatasetCurator}, Audit: "revoke oracle revision", At: revokedAt,
	})
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke normalization oracle = %+v, %v", revoked, err)
	}
	if _, err := restarted.AuthorizeNormalizationOracleBindings([]NormalizationOracleBinding{record.Binding}, access); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked normalization oracle authorization error = %v", err)
	}
}

func signedNormalizationOracleRegistration(
	t *testing.T,
	oracle NormalizationOracle,
	key TrustedGovernanceKey,
	privateKey ed25519.PrivateKey,
	expectedRevision uint64,
	expectedEventID string,
	registeredAt time.Time,
) NormalizationOracleRegistration {
	t.Helper()
	oracleData, err := json.Marshal(oracle)
	if err != nil {
		t.Fatal(err)
	}
	oracleDigest, err := runmodel.DigestJSON(oracle)
	if err != nil {
		t.Fatal(err)
	}
	oracleRef := runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + oracleDigest, SHA256: oracleDigest,
		SizeBytes: int64(len(oracleData)), Contract: NormalizationOracleContract,
	}
	attestation := NormalizationOracleAttestation{
		SchemaVersion: NormalizationOracleAttestationSchemaVersion,
		AttestationID: "attestation-" + oracle.OracleID + "-" + registeredAt.Format("150405"),
		OracleID:      oracle.OracleID, OracleSHA256: oracleDigest,
		Authority: key.Authority, KeyID: key.KeyID, KeyRevision: key.Revision,
		PolicyRevision: oracle.Adjudication.Revision, Decision: "approved",
		IssuedAt: registeredAt.Add(-time.Minute),
	}
	payload, err := attestation.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	attestation.SignatureBase64 = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	registration := NormalizationOracleRegistration{
		SchemaVersion: NormalizationOracleRegistrationSchemaVersion,
		Oracle:        oracle, OracleRef: oracleRef, Attestation: attestation, TrustedKey: key,
		ExpectedCurrentRevision: expectedRevision, ExpectedCurrentRegisteredEventID: expectedEventID,
		RegisteredAt: registeredAt,
	}
	if err := registration.Validate(); err != nil {
		t.Fatalf("normalization oracle registration fixture: %v", err)
	}
	return registration
}
