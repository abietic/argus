package evaluation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"sort"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

var testEpoch = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func newEvaluationRepository(t *testing.T) (*Repository, *local.Store) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return repository, store
}

func testArtifact(name string) string {
	return "artifact://local/" + name
}

func testReplayExecutorTemplateRef() *runmodel.ArtifactRef {
	digest := evaluationDigest("test-replay-executor-template")
	return &runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + digest, SHA256: digest, SizeBytes: 1,
		Contract: ReplayExecutorTemplateContract,
	}
}

func testActiveCase(
	id string,
	repositoryID string,
	split Split,
	observedAt time.Time,
) EvaluationCase {
	allowed := []UseScope{UseEvaluation, UsePromotion, UseTraining}
	eligibility := Eligibility{
		Evaluation:             true,
		Training:               true,
		Promotion:              true,
		ProductionDistribution: true,
	}
	if split == SplitHoldout {
		allowed = []UseScope{UseEvaluation, UsePromotion}
		eligibility.Training = false
	}
	return EvaluationCase{
		SchemaVersion: EvaluationCaseSchemaVersion,
		CaseID:        id,
		Type:          CasePositiveLocalized,
		Provenance: SourceProvenance{
			Kind:         SourceHumanConfirmedFinding,
			RepositoryID: repositoryID,
			SourceID:     "source-" + id,
			FindingID:    "finding-" + id,
			ObservedAt:   observedAt,
			CollectedAt:  observedAt.Add(time.Minute),
			EvidenceRefs: []string{testArtifact("source-" + id)},
		},
		LicenseConsent: LicenseConsent{
			LicenseID:    "internal-license",
			Consent:      ConsentAuthorizedInternal,
			AllowedUses:  allowed,
			Restrictions: []string{},
		},
		Classification:      ClassificationInternal,
		Owner:               "quality-team",
		InputSnapshotRef:    testArtifact("input-" + id),
		Label:               testDefectLabel("medium"),
		LabelPolicyRevision: "label-policy-1",
		ReviewState:         ReviewApproved,
		DatasetState:        DatasetActive,
		Split:               split,
		CloneGroupID:        "clone-" + id,
		Eligibility:         eligibility,
		CreatedAt:           observedAt.Add(2 * time.Minute),
	}
}

func testFeedbackCase(id string, at time.Time) EvaluationCase {
	return EvaluationCase{
		SchemaVersion: EvaluationCaseSchemaVersion,
		CaseID:        id,
		Type:          CasePositiveLocalized,
		Provenance: SourceProvenance{
			Kind:         SourceProductionFeedback,
			RepositoryID: "repo-feedback",
			SourceID:     "feedback-" + id,
			SourceRunID:  "run-" + id,
			ObservedAt:   at.Add(-2 * time.Minute),
			CollectedAt:  at.Add(-time.Minute),
			EvidenceRefs: []string{testArtifact("feedback-" + id)},
		},
		LicenseConsent: LicenseConsent{
			LicenseID:    "feedback-consent",
			Consent:      ConsentAuthorizedInternal,
			AllowedUses:  []UseScope{UseCandidatePool},
			Restrictions: []string{},
		},
		Classification:      ClassificationInternal,
		Owner:               "feedback-service",
		InputSnapshotRef:    testArtifact("input-" + id),
		Label:               testDefectLabel("medium"),
		LabelPolicyRevision: "candidate-policy-1",
		ReviewState:         ReviewPending,
		DatasetState:        DatasetCandidatePool,
		Split:               SplitUnassigned,
		CloneGroupID:        "clone-" + id,
		Eligibility:         Eligibility{},
		CreatedAt:           at,
	}
}

func testDefectLabel(severity string) Label {
	return Label{
		ExpectedOutcome: OutcomeDefectPresent,
		Category:        "correctness",
		Severity:        severity,
		Anchors: []LabelAnchor{{
			Path: "internal/review.go", Side: "new", StartLine: 10, EndLine: 11,
			SourceDigest: evaluationDigest("label-source"),
		}},
		AnchorRefs:         []string{testArtifact("anchor")},
		SuppressionTargets: []SuppressionTarget{},
	}
}

func testExposure(runID string, caseID string, at time.Time, status ExposureStatus) Exposure {
	return Exposure{
		SchemaVersion:   ExposureSchemaVersion,
		EvaluationRunID: runID,
		CaseID:          caseID,
		Observations: []ExposureObservation{
			{Component: ExposurePrompt, Status: status, Revision: "prompt-1"},
			{Component: ExposureRule, Status: status, Revision: "rule-1"},
			{Component: ExposureModel, Status: status, Revision: "model-1"},
			{Component: ExposureIndex, Status: status, Revision: "index-1"},
		},
		ObservedAt: at,
	}
}

func testMutation(key string, at time.Time, roles ...Role) Mutation {
	sortedRoles := append([]Role(nil), roles...)
	sort.Slice(sortedRoles, func(left, right int) bool {
		return sortedRoles[left] < sortedRoles[right]
	})
	return Mutation{
		IdempotencyKey: key,
		Actor:          "test-actor",
		Roles:          sortedRoles,
		Audit:          "test audit",
		At:             at,
	}
}

func createCase(t *testing.T, repository *Repository, evaluationCase EvaluationCase, roles ...Role) {
	t.Helper()
	mutation := testMutation("create-"+evaluationCase.CaseID, evaluationCase.CreatedAt, roles...)
	var err error
	if evaluationCase.DatasetState == DatasetCandidatePool {
		_, err = repository.CreateCase(context.Background(), evaluationCase, mutation)
	} else {
		request := testGovernedCaseImport(evaluationCase)
		registerTestGovernanceKey(t, repository, request)
		_, err = repository.ImportGovernedCase(context.Background(), request, mutation)
	}
	if err != nil {
		t.Fatalf("create/import case %s error = %v", evaluationCase.CaseID, err)
	}
}

func testGovernedCaseImport(evaluationCase EvaluationCase) ExternalGovernedCaseImport {
	seed := sha256.Sum256([]byte("argus-test-external-governance-key"))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	at := evaluationCase.CreatedAt.UTC()
	digest, _ := EvaluationCaseSHA256(evaluationCase)
	evidenceRefs := append([]string{evaluationCase.InputSnapshotRef}, evaluationCase.Provenance.EvidenceRefs...)
	sort.Strings(evidenceRefs)
	attestation := ExternalGovernanceAttestation{
		SchemaVersion: ExternalGovernanceAttestationSchemaVersion,
		AttestationID: "attestation-" + evaluationCase.CaseID, CaseID: evaluationCase.CaseID,
		CaseSHA256: digest, Authority: "test-governance-authority", KeyID: "test-key-" + evaluationCase.CaseID,
		PolicyRevision: "test-review-policy-1", ReviewerIDs: []string{"external-reviewer-a", "external-reviewer-b"},
		AdjudicatorID: "external-adjudicator-c", EvidenceRefs: evidenceRefs,
		Decision: "approved", ReviewedAt: at, IssuedAt: at,
	}
	payload, _ := attestation.SigningBytes()
	attestation.SignatureBase64 = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return ExternalGovernedCaseImport{
		SchemaVersion: ExternalGovernedCaseImportSchemaVersion, Case: evaluationCase,
		Attestation: attestation,
		TrustedKey: TrustedGovernanceKey{
			SchemaVersion: TrustedGovernanceKeySchemaVersion,
			Authority:     attestation.Authority, KeyID: attestation.KeyID, Revision: "test-trust-" + evaluationCase.CaseID,
			PublicKeyBase64:        base64.StdEncoding.EncodeToString(publicKey),
			RepositoryIDs:          []string{evaluationCase.Provenance.RepositoryID},
			AllowedClassifications: []Classification{evaluationCase.Classification},
			ValidFrom:              at.Add(-time.Hour), ValidUntil: at.Add(time.Hour),
		},
		ImportedAt: at,
	}
}

func testGovernedCaseImportWithTrustedKey(
	evaluationCase EvaluationCase,
	key TrustedGovernanceKey,
) ExternalGovernedCaseImport {
	request := testGovernedCaseImport(evaluationCase)
	request.Attestation.Authority = key.Authority
	request.Attestation.KeyID = key.KeyID
	request.TrustedKey = cloneValue(key)
	seed := sha256.Sum256([]byte("argus-test-external-governance-key"))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	payload, _ := request.Attestation.SigningBytes()
	request.Attestation.SignatureBase64 = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return request
}

func registerTestGovernanceKey(
	t *testing.T,
	repository *Repository,
	request ExternalGovernedCaseImport,
) GovernanceTrustKeyRecord {
	t.Helper()
	registeredAt := request.ImportedAt.Add(-time.Second)
	registration := GovernanceTrustKeyRegistration{
		SchemaVersion: GovernanceTrustKeyRegistrationSchemaVersion,
		Key:           request.TrustedKey,
		RegisteredAt:  registeredAt,
	}
	mutation := testMutation(
		"register-"+request.Attestation.AttestationID,
		registeredAt,
		RoleGovernanceTrustAdmin,
	)
	mutation.Actor = "test-trust-admin"
	record, err := repository.RegisterGovernanceTrustKey(context.Background(), registration, mutation)
	if err != nil {
		t.Fatalf("register governance key for %s: %v", request.Case.CaseID, err)
	}
	return record
}
