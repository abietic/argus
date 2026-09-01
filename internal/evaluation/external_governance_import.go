package evaluation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/abietic/argus/internal/runmodel"
)

const (
	ExternalGovernedCaseImportSchemaVersion     = "argus.external_governed_case_import.v1alpha1"
	ExternalGovernanceAttestationSchemaVersion  = "argus.external_governance_attestation.v1alpha1"
	TrustedGovernanceKeySchemaVersion           = "argus.trusted_governance_key.v1alpha1"
	GovernanceTrustKeyRegistrationSchemaVersion = "argus.governance_trust_key_registration.v1alpha1"
	GovernanceTrustKeyRevocationSchemaVersion   = "argus.governance_trust_key_revocation.v1alpha1"
)

type TrustedGovernanceKey struct {
	SchemaVersion          string           `json:"schema_version"`
	Authority              string           `json:"authority"`
	KeyID                  string           `json:"key_id"`
	Revision               string           `json:"revision"`
	PublicKeyBase64        string           `json:"public_key_base64"`
	RepositoryIDs          []string         `json:"repository_ids"`
	AllowedClassifications []Classification `json:"allowed_classifications"`
	ValidFrom              time.Time        `json:"valid_from"`
	ValidUntil             time.Time        `json:"valid_until"`
}

type GovernanceTrustKeyRegistration struct {
	SchemaVersion string               `json:"schema_version"`
	Key           TrustedGovernanceKey `json:"key"`
	RegisteredAt  time.Time            `json:"registered_at"`
}

type GovernanceTrustKeyRevocation struct {
	SchemaVersion string    `json:"schema_version"`
	Authority     string    `json:"authority"`
	KeyID         string    `json:"key_id"`
	Revision      string    `json:"revision"`
	Reason        string    `json:"reason"`
	RevokedAt     time.Time `json:"revoked_at"`
}

type GovernanceTrustKeyRecord struct {
	Key               TrustedGovernanceKey `json:"key"`
	RegisteredEventID string               `json:"registered_event_id"`
	RegisteredAt      time.Time            `json:"registered_at"`
	RegisteredBy      string               `json:"registered_by"`
	RevokedEventID    string               `json:"revoked_event_id,omitempty"`
	RevokedAt         *time.Time           `json:"revoked_at,omitempty"`
	RevokedBy         string               `json:"revoked_by,omitempty"`
	RevocationReason  string               `json:"revocation_reason,omitempty"`
}

// ExternalGovernanceAttestation is signed outside Argus. The signature covers
// every field except SignatureBase64, including exact case/evidence identity.
type ExternalGovernanceAttestation struct {
	SchemaVersion   string    `json:"schema_version"`
	AttestationID   string    `json:"attestation_id"`
	CaseID          string    `json:"case_id"`
	CaseSHA256      string    `json:"case_sha256"`
	Authority       string    `json:"authority"`
	KeyID           string    `json:"key_id"`
	PolicyRevision  string    `json:"policy_revision"`
	ReviewerIDs     []string  `json:"reviewer_ids"`
	AdjudicatorID   string    `json:"adjudicator_id"`
	EvidenceRefs    []string  `json:"evidence_refs"`
	Decision        string    `json:"decision"`
	ReviewedAt      time.Time `json:"reviewed_at"`
	IssuedAt        time.Time `json:"issued_at"`
	SignatureBase64 string    `json:"signature_base64"`
}

type ExternalGovernedCaseImport struct {
	SchemaVersion string                        `json:"schema_version"`
	Case          EvaluationCase                `json:"case"`
	Attestation   ExternalGovernanceAttestation `json:"attestation"`
	TrustedKey    TrustedGovernanceKey          `json:"trusted_key"`
	ImportedAt    time.Time                     `json:"imported_at"`
}

type ExternalGovernanceImportRecord struct {
	EventID     string                        `json:"event_id"`
	Attestation ExternalGovernanceAttestation `json:"attestation"`
	TrustedKey  TrustedGovernanceKey          `json:"trusted_key"`
	ImportedAt  time.Time                     `json:"imported_at"`
	ImportedBy  string                        `json:"imported_by"`
}

type externalGovernanceSigningPayload struct {
	SchemaVersion  string    `json:"schema_version"`
	AttestationID  string    `json:"attestation_id"`
	CaseID         string    `json:"case_id"`
	CaseSHA256     string    `json:"case_sha256"`
	Authority      string    `json:"authority"`
	KeyID          string    `json:"key_id"`
	PolicyRevision string    `json:"policy_revision"`
	ReviewerIDs    []string  `json:"reviewer_ids"`
	AdjudicatorID  string    `json:"adjudicator_id"`
	EvidenceRefs   []string  `json:"evidence_refs"`
	Decision       string    `json:"decision"`
	ReviewedAt     time.Time `json:"reviewed_at"`
	IssuedAt       time.Time `json:"issued_at"`
}

func EvaluationCaseSHA256(evaluationCase EvaluationCase) (string, error) {
	if err := evaluationCase.Validate(); err != nil {
		return "", err
	}
	return runmodel.DigestJSON(evaluationCase)
}

func (attestation ExternalGovernanceAttestation) SigningBytes() ([]byte, error) {
	payload := externalGovernanceSigningPayload{
		SchemaVersion: attestation.SchemaVersion, AttestationID: attestation.AttestationID,
		CaseID: attestation.CaseID, CaseSHA256: attestation.CaseSHA256,
		Authority: attestation.Authority, KeyID: attestation.KeyID,
		PolicyRevision: attestation.PolicyRevision, ReviewerIDs: slices.Clone(attestation.ReviewerIDs),
		AdjudicatorID: attestation.AdjudicatorID, EvidenceRefs: slices.Clone(attestation.EvidenceRefs),
		Decision: attestation.Decision, ReviewedAt: attestation.ReviewedAt, IssuedAt: attestation.IssuedAt,
	}
	return json.Marshal(payload)
}

func (key TrustedGovernanceKey) Validate() error {
	if key.SchemaVersion != TrustedGovernanceKeySchemaVersion {
		return fmt.Errorf("unsupported trusted governance key schema %q", key.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{"authority", key.Authority}, {"key_id", key.KeyID}, {"revision", key.Revision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	publicKey, err := base64.StdEncoding.DecodeString(key.PublicKeyBase64)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("public_key_base64 must encode an Ed25519 public key")
	}
	if err := validateSortedIDs("repository_ids", key.RepositoryIDs, true); err != nil {
		return err
	}
	if key.AllowedClassifications == nil || len(key.AllowedClassifications) == 0 {
		return fmt.Errorf("allowed_classifications must be a non-empty array")
	}
	for index, classification := range key.AllowedClassifications {
		if err := classification.Validate(); err != nil {
			return fmt.Errorf("allowed_classifications[%d]: %w", index, err)
		}
		if index > 0 && key.AllowedClassifications[index-1] >= classification {
			return fmt.Errorf("allowed_classifications must be sorted and unique")
		}
	}
	if key.ValidFrom.IsZero() || key.ValidUntil.IsZero() || !key.ValidFrom.Before(key.ValidUntil) ||
		key.ValidFrom.Location() != time.UTC || key.ValidUntil.Location() != time.UTC {
		return fmt.Errorf("trusted key validity must be a non-empty UTC interval")
	}
	return nil
}

func (registration GovernanceTrustKeyRegistration) Validate() error {
	if registration.SchemaVersion != GovernanceTrustKeyRegistrationSchemaVersion {
		return fmt.Errorf("unsupported governance trust key registration schema %q", registration.SchemaVersion)
	}
	if err := registration.Key.Validate(); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	if registration.RegisteredAt.IsZero() || registration.RegisteredAt.Location() != time.UTC ||
		registration.RegisteredAt.After(registration.Key.ValidUntil) {
		return fmt.Errorf("registered_at must be UTC and not after key validity")
	}
	return nil
}

func (revocation GovernanceTrustKeyRevocation) Validate() error {
	if revocation.SchemaVersion != GovernanceTrustKeyRevocationSchemaVersion {
		return fmt.Errorf("unsupported governance trust key revocation schema %q", revocation.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{"authority", revocation.Authority}, {"key_id", revocation.KeyID}, {"revision", revocation.Revision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := validateText("reason", revocation.Reason, 4096, true); err != nil {
		return err
	}
	if revocation.RevokedAt.IsZero() || revocation.RevokedAt.Location() != time.UTC {
		return fmt.Errorf("revoked_at must be UTC")
	}
	return nil
}

func (attestation ExternalGovernanceAttestation) Validate() error {
	if attestation.SchemaVersion != ExternalGovernanceAttestationSchemaVersion {
		return fmt.Errorf("unsupported external governance attestation schema %q", attestation.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{"attestation_id", attestation.AttestationID}, {"case_id", attestation.CaseID},
		{"authority", attestation.Authority}, {"key_id", attestation.KeyID},
		{"policy_revision", attestation.PolicyRevision}, {"adjudicator_id", attestation.AdjudicatorID},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := validateSHA256("case_sha256", attestation.CaseSHA256); err != nil {
		return err
	}
	if err := validateSortedIDs("reviewer_ids", attestation.ReviewerIDs, true); err != nil {
		return err
	}
	if len(attestation.ReviewerIDs) < 2 || len(attestation.ReviewerIDs) > 8 {
		return fmt.Errorf("reviewer_ids must contain between two and eight reviewers")
	}
	if slices.Contains(attestation.ReviewerIDs, attestation.AdjudicatorID) {
		return fmt.Errorf("adjudicator must be independent from reviewers")
	}
	if err := validateArtifactRefs("evidence_refs", attestation.EvidenceRefs, true); err != nil {
		return err
	}
	if attestation.Decision != "approved" {
		return fmt.Errorf("external governance attestation must be approved")
	}
	if attestation.ReviewedAt.IsZero() || attestation.IssuedAt.IsZero() ||
		attestation.ReviewedAt.Location() != time.UTC || attestation.IssuedAt.Location() != time.UTC ||
		attestation.IssuedAt.Before(attestation.ReviewedAt) {
		return fmt.Errorf("attestation requires ordered UTC reviewed_at and issued_at")
	}
	signature, err := base64.StdEncoding.DecodeString(attestation.SignatureBase64)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("signature_base64 must encode an Ed25519 signature")
	}
	return nil
}

func (value ExternalGovernedCaseImport) Validate() error {
	if value.SchemaVersion != ExternalGovernedCaseImportSchemaVersion {
		return fmt.Errorf("unsupported external governed case import schema %q", value.SchemaVersion)
	}
	if err := value.Case.Validate(); err != nil {
		return fmt.Errorf("case: %w", err)
	}
	if value.Case.ReviewState != ReviewApproved ||
		(value.Case.DatasetState != DatasetGold && value.Case.DatasetState != DatasetActive) {
		return fmt.Errorf("external import requires an approved gold or active case")
	}
	if err := value.Attestation.Validate(); err != nil {
		return fmt.Errorf("attestation: %w", err)
	}
	if err := value.TrustedKey.Validate(); err != nil {
		return fmt.Errorf("trusted_key: %w", err)
	}
	if value.ImportedAt.IsZero() || value.ImportedAt.Location() != time.UTC ||
		value.ImportedAt.Before(value.Attestation.IssuedAt) {
		return fmt.Errorf("imported_at must be UTC and not precede attestation issuance")
	}
	if value.Attestation.ReviewedAt.Before(value.Case.CreatedAt) {
		return fmt.Errorf("attestation reviewed_at cannot precede case creation")
	}
	if value.Attestation.CaseID != value.Case.CaseID ||
		value.Attestation.Authority != value.TrustedKey.Authority ||
		value.Attestation.KeyID != value.TrustedKey.KeyID {
		return fmt.Errorf("attestation identity does not match case and trusted key")
	}
	caseDigest, err := EvaluationCaseSHA256(value.Case)
	if err != nil || caseDigest != value.Attestation.CaseSHA256 {
		return fmt.Errorf("attestation does not bind the exact evaluation case")
	}
	expectedEvidence := append([]string{value.Case.InputSnapshotRef}, value.Case.Provenance.EvidenceRefs...)
	sort.Strings(expectedEvidence)
	expectedEvidence = slices.Compact(expectedEvidence)
	if !slices.Equal(expectedEvidence, value.Attestation.EvidenceRefs) {
		return fmt.Errorf("attestation evidence_refs do not equal case input and provenance closure")
	}
	if !slices.Contains(value.TrustedKey.RepositoryIDs, value.Case.Provenance.RepositoryID) ||
		!slices.Contains(value.TrustedKey.AllowedClassifications, value.Case.Classification) {
		return fmt.Errorf("trusted key scope does not authorize case repository and classification")
	}
	if value.Attestation.IssuedAt.Before(value.TrustedKey.ValidFrom) ||
		value.Attestation.IssuedAt.After(value.TrustedKey.ValidUntil) {
		return fmt.Errorf("attestation was issued outside trusted key validity")
	}
	publicKey, _ := base64.StdEncoding.DecodeString(value.TrustedKey.PublicKeyBase64)
	signature, _ := base64.StdEncoding.DecodeString(value.Attestation.SignatureBase64)
	payload, err := value.Attestation.SigningBytes()
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return fmt.Errorf("external governance attestation signature is invalid")
	}
	return nil
}

func DecodeExternalGovernedCaseImport(data []byte) (ExternalGovernedCaseImport, error) {
	return decodeStrict(data, "ExternalGovernedCaseImport", func(value ExternalGovernedCaseImport) error {
		return value.Validate()
	})
}

func DecodeGovernanceTrustKeyRegistration(data []byte) (GovernanceTrustKeyRegistration, error) {
	return decodeStrict(data, "GovernanceTrustKeyRegistration", func(value GovernanceTrustKeyRegistration) error {
		return value.Validate()
	})
}

func DecodeGovernanceTrustKeyRevocation(data []byte) (GovernanceTrustKeyRevocation, error) {
	return decodeStrict(data, "GovernanceTrustKeyRevocation", func(value GovernanceTrustKeyRevocation) error {
		return value.Validate()
	})
}
