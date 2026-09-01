package platformapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/findingdecision"
	"github.com/abietic/argus/internal/store/local"
)

const testToken = "argus-local-api-test-token-000000000000"

var apiTestEpoch = time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)

func TestHandlerAuthenticationPrincipalInjectionPaginationAndNoTokenPersistence(t *testing.T) {
	repository, root := newTestRepository(t)
	for index, id := range []string{"case-a", "case-b", "case-c"} {
		candidate := testCandidate(id, apiTestEpoch.Add(time.Duration(index)*time.Minute))
		_, err := repository.CreateCase(context.Background(), candidate, evaluation.Mutation{
			IdempotencyKey: "ingest-" + id,
			Actor:          "feedback-ingress",
			Roles:          []evaluation.Role{evaluation.RoleFeedbackIngest},
			Audit:          "candidate ingress fixture",
			At:             candidate.CreatedAt,
		})
		if err != nil {
			t.Fatalf("CreateCase(%s) error = %v", id, err)
		}
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "local-curator",
		Roles:           []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions:     []Permission{PermissionEvaluationRead, PermissionEvaluationWrite},
		ProfileRevision: "curator-v1",
	}
	handler, err := NewHandler(Services{Evaluation: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/evaluation/cases", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticated.Code)
	}

	first := authenticatedRequest(http.MethodGet, "/v1/evaluation/cases?limit=2", nil)
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first page status = %d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	var firstPage struct {
		SchemaVersion string `json:"schema_version"`
		Data          struct {
			Items      []evaluation.CaseRecord `json:"items"`
			NextCursor string                  `json:"next_cursor"`
		} `json:"data"`
	}
	decodeResponse(t, firstResponse.Body.Bytes(), &firstPage)
	if firstPage.SchemaVersion != ResponseSchemaVersion || len(firstPage.Data.Items) != 2 ||
		firstPage.Data.Items[0].Case.CaseID != "case-a" || firstPage.Data.NextCursor == "" {
		t.Fatalf("first page = %+v", firstPage)
	}
	second := authenticatedRequest(
		http.MethodGet,
		"/v1/evaluation/cases?limit=2&cursor="+firstPage.Data.NextCursor,
		nil,
	)
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, second)
	var secondPage struct {
		Data struct {
			Items []evaluation.CaseRecord `json:"items"`
		} `json:"data"`
	}
	decodeResponse(t, secondResponse.Body.Bytes(), &secondPage)
	if secondResponse.Code != http.StatusOK || len(secondPage.Data.Items) != 1 ||
		secondPage.Data.Items[0].Case.CaseID != "case-c" {
		t.Fatalf("second page status=%d page=%+v", secondResponse.Code, secondPage)
	}

	at := apiTestEpoch.Add(10 * time.Minute)
	command := GovernanceBatchCommand{
		SchemaVersion: GovernanceBatchCommandSchemaVersion,
		Request: evaluation.GovernanceBatchRequest{
			SchemaVersion: evaluation.GovernanceBatchRequestSchemaVersion,
			BatchID:       "api-assignment-batch",
			Operation:     evaluation.GovernanceBatchAssign,
			Items: []evaluation.GovernanceBatchItem{{
				EventID: "api-assignment-item",
				Assignment: &evaluation.CaseReviewAssignment{
					SchemaVersion:              evaluation.CaseReviewAssignmentSchemaVersion,
					CaseID:                     "case-a",
					ExpectedGovernanceRevision: 1,
					ExpectedLabelRevision:      1,
					ReviewerIDs:                []string{"reviewer-a", "reviewer-b"},
					Blind:                      true,
					Reason:                     "assign through authenticated local API",
					AssignedAt:                 at,
				},
			}},
			TerminalEventID: "api-assignment-terminal",
			SubmittedAt:     at,
		},
		Mutation: MutationInput{
			SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "api-assignment-intent",
			Audit: "operator requested blind assignment", At: at,
		},
	}
	requestBody, _ := json.Marshal(command)
	writeResponse := httptest.NewRecorder()
	handler.ServeHTTP(writeResponse, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/governance-batches", bytes.NewReader(requestBody),
	))
	if writeResponse.Code != http.StatusOK {
		t.Fatalf("batch write status = %d body=%s", writeResponse.Code, writeResponse.Body.String())
	}
	stored, err := repository.GetGovernanceBatch(
		command.Request.BatchID,
		evaluation.Access{Actor: principal.Actor, Roles: principal.Roles},
	)
	if err != nil || stored.Actor != principal.Actor || stored.Status != evaluation.GovernanceBatchSucceeded {
		t.Fatalf("stored batch = %+v, %v", stored, err)
	}

	selfDeclared := strings.Replace(
		string(requestBody),
		`"audit":"operator requested blind assignment"`,
		`"actor":"attacker","roles":["governance_trust_admin"],"audit":"operator requested blind assignment"`,
		1,
	)
	selfDeclaredResponse := httptest.NewRecorder()
	handler.ServeHTTP(selfDeclaredResponse, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/governance-batches", strings.NewReader(selfDeclared),
	))
	if selfDeclaredResponse.Code != http.StatusBadRequest ||
		!strings.Contains(selfDeclaredResponse.Body.String(), "unknown field") {
		t.Fatalf("self-declared authority status=%d body=%s", selfDeclaredResponse.Code, selfDeclaredResponse.Body.String())
	}

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(data, []byte(testToken)) {
			t.Fatalf("bearer token persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHandlerRejectsAmbiguousTransportAndMapsAuthorization(t *testing.T) {
	repository, _ := newTestRepository(t)
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "reviewer-a",
		Roles:           []evaluation.Role{evaluation.RoleDatasetReviewer},
		Permissions:     []Permission{PermissionEvaluationRead, PermissionEvaluationWrite},
		ProfileRevision: "reviewer-v1",
	}
	handler, err := NewHandler(Services{Evaluation: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, method, target, contentType, authorization string
		body                                             string
		want                                             int
	}{
		{name: "wrong token", method: http.MethodGet, target: "/v1/health", authorization: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "unknown query", method: http.MethodGet, target: "/v1/health?debug=true", authorization: "Bearer " + testToken, want: http.StatusBadRequest},
		{name: "encoded path", method: http.MethodGet, target: "/v1/evaluation/cases/case%2Fa", authorization: "Bearer " + testToken, want: http.StatusBadRequest},
		{name: "wrong method", method: http.MethodDelete, target: "/v1/health", authorization: "Bearer " + testToken, want: http.StatusMethodNotAllowed},
		{name: "missing content type", method: http.MethodPost, target: "/v1/evaluation/trust-keys", authorization: "Bearer " + testToken, body: `{}`, want: http.StatusUnsupportedMediaType},
		{name: "duplicate field", method: http.MethodPost, target: "/v1/evaluation/trust-keys", contentType: "application/json", authorization: "Bearer " + testToken, body: `{"schema_version":"a","schema_version":"b"}`, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}

	trustResponse := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodGet, "/v1/evaluation/trust-keys", nil)
	handler.ServeHTTP(trustResponse, request)
	if trustResponse.Code != http.StatusForbidden || strings.Contains(trustResponse.Body.String(), "governance_trust_admin") {
		t.Fatalf("trust-key authorization status=%d body=%s", trustResponse.Code, trustResponse.Body.String())
	}
}

func TestExactEvaluationLookupAddressesDomainValidIDsOutsidePathSegmentSubset(t *testing.T) {
	repository, _ := newTestRepository(t)
	candidate := testCandidate("case/team:1", apiTestEpoch)
	if _, err := repository.CreateCase(context.Background(), candidate, evaluation.Mutation{
		IdempotencyKey: "ingest-complex-case-id", Actor: "feedback-ingress",
		Roles: []evaluation.Role{evaluation.RoleFeedbackIngest}, Audit: "complex case id fixture",
		At: candidate.CreatedAt,
	}); err != nil {
		t.Fatalf("CreateCase() error = %v", err)
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "local-curator",
		Roles:       []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions: []Permission{PermissionEvaluationRead}, ProfileRevision: "curator-v1",
	}
	handler, err := NewHandler(Services{Evaluation: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodGet, "/v1/evaluation/case?case_id=case%2Fteam%3A1", nil,
	))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "case/team:1") {
		t.Fatalf("exact lookup status=%d body=%s", response.Code, response.Body.String())
	}
	missingQuery := httptest.NewRecorder()
	handler.ServeHTTP(missingQuery, authenticatedRequest(http.MethodGet, "/v1/evaluation/case", nil))
	if missingQuery.Code != http.StatusBadRequest {
		t.Fatalf("missing exact query status=%d body=%s", missingQuery.Code, missingQuery.Body.String())
	}
}

func TestTrustKeyRegistrationUsesServerPrincipal(t *testing.T) {
	repository, _ := newTestRepository(t)
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "trust-admin",
		Roles:           []evaluation.Role{evaluation.RoleGovernanceTrustAdmin},
		Permissions:     []Permission{PermissionEvaluationRead, PermissionEvaluationWrite},
		ProfileRevision: "trust-admin-v1",
	}
	handler, err := NewHandler(Services{Evaluation: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("platform-api-trust-key"))
	publicKey := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
	at := apiTestEpoch
	command := TrustKeyCommand{
		SchemaVersion: TrustKeyCommandSchemaVersion,
		Registration: evaluation.GovernanceTrustKeyRegistration{
			SchemaVersion: evaluation.GovernanceTrustKeyRegistrationSchemaVersion,
			Key: evaluation.TrustedGovernanceKey{
				SchemaVersion: evaluation.TrustedGovernanceKeySchemaVersion,
				Authority:     "external-governance", KeyID: "key-1", Revision: "revision-1",
				PublicKeyBase64:        base64.StdEncoding.EncodeToString(publicKey),
				RepositoryIDs:          []string{"repo-feedback"},
				AllowedClassifications: []evaluation.Classification{evaluation.ClassificationInternal},
				ValidFrom:              at.Add(-time.Hour), ValidUntil: at.Add(time.Hour),
			},
			RegisteredAt: at,
		},
		Mutation: MutationInput{
			SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "register-key-1",
			Audit: "register independently governed source", At: at,
		},
	}
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodPost, "/v1/evaluation/trust-keys", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("registration status=%d body=%s", response.Code, response.Body.String())
	}
	records, err := repository.ListGovernanceTrustKeys(principal.Access())
	if err != nil || len(records) != 1 || records[0].RegisteredBy != principal.Actor {
		t.Fatalf("trust records=%+v err=%v", records, err)
	}
}

func TestNewHandlerAndLoadPrincipalFailClosed(t *testing.T) {
	repository, _ := newTestRepository(t)
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "curator",
		Roles:       []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions: []Permission{PermissionEvaluationRead}, ProfileRevision: "v1",
	}
	if _, err := NewHandler(Services{Evaluation: repository}, principal, "short"); err == nil {
		t.Fatal("short token was accepted")
	}
	if _, err := NewHandler(Services{Evaluation: repository}, principal, strings.Repeat("x", 31)+" "); err == nil {
		t.Fatal("token containing whitespace was accepted")
	}
	configOnly := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "config-only", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionConfigRead}, ProfileRevision: "config-only-v1",
	}
	if _, err := NewHandler(Services{Evaluation: repository}, configOnly, testToken); err != nil {
		t.Fatalf("config-only principal was rejected: %v", err)
	}
	missingRole := configOnly
	missingRole.Permissions = []Permission{PermissionEvaluationRead}
	if _, err := NewHandler(Services{Evaluation: repository}, missingRole, testToken); err == nil ||
		!strings.Contains(err.Error(), "evaluation role") {
		t.Fatalf("evaluation permission without role error = %v", err)
	}
	unsorted := principal
	unsorted.Permissions = []Permission{PermissionEvaluationWrite, PermissionEvaluationRead}
	if _, err := NewHandler(Services{Evaluation: repository}, unsorted, testToken); err == nil ||
		!strings.Contains(err.Error(), "sorted") {
		t.Fatalf("unsorted permissions error = %v", err)
	}
	reviewWriter := configOnly
	reviewWriter.Permissions = []Permission{PermissionReviewWrite}
	if _, err := NewHandler(Services{Evaluation: repository}, reviewWriter, testToken); err == nil ||
		!strings.Contains(err.Error(), "actor_kind") {
		t.Fatalf("review writer without actor kind error = %v", err)
	}
	reviewWriter.ActorKind = findingdecision.ActorHuman
	if _, err := NewHandler(Services{Evaluation: repository}, reviewWriter, testToken); err == nil ||
		!strings.Contains(err.Error(), "finding_roles") {
		t.Fatalf("review writer without finding role error = %v", err)
	}
	reviewWriter.FindingRoles = []findingdecision.Role{
		findingdecision.RolePublicationApprover,
		findingdecision.RoleFindingReviewer,
	}
	if _, err := NewHandler(Services{Evaluation: repository}, reviewWriter, testToken); err == nil ||
		!strings.Contains(err.Error(), "sorted") {
		t.Fatalf("review writer with unsorted finding roles error = %v", err)
	}

	principalPath := filepath.Join(t.TempDir(), "principal.json")
	data, _ := json.Marshal(principal)
	if err := os.WriteFile(principalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPrincipal(principalPath)
	if err != nil || loaded.Actor != principal.Actor {
		t.Fatalf("LoadPrincipal() = %+v, %v", loaded, err)
	}
	symlink := principalPath + ".link"
	if err := os.Symlink(principalPath, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrincipal(symlink); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink principal error = %v", err)
	}
}

func newTestRepository(t *testing.T) (*evaluation.Repository, string) {
	t.Helper()
	root := t.TempDir()
	store, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := evaluation.New(store)
	if err != nil {
		t.Fatal(err)
	}
	return repository, root
}

func testCandidate(id string, at time.Time) evaluation.EvaluationCase {
	digest := sha256.Sum256([]byte("platform-api-label-source"))
	return evaluation.EvaluationCase{
		SchemaVersion: evaluation.EvaluationCaseSchemaVersion,
		CaseID:        id, Type: evaluation.CasePositiveLocalized,
		Provenance: evaluation.SourceProvenance{
			Kind: evaluation.SourceProductionFeedback, RepositoryID: "repo-feedback",
			SourceID: "feedback-" + id, SourceRunID: "run-" + id,
			ObservedAt: at.Add(-2 * time.Minute), CollectedAt: at.Add(-time.Minute),
			EvidenceRefs: []string{"artifact://local/feedback-" + id},
		},
		LicenseConsent: evaluation.LicenseConsent{
			LicenseID: "feedback-consent", Consent: evaluation.ConsentAuthorizedInternal,
			AllowedUses: []evaluation.UseScope{evaluation.UseCandidatePool}, Restrictions: []string{},
		},
		Classification: evaluation.ClassificationInternal, Owner: "feedback-service",
		InputSnapshotRef: "artifact://local/input-" + id,
		Label: evaluation.Label{
			ExpectedOutcome: evaluation.OutcomeDefectPresent, Category: "correctness", Severity: "medium",
			Anchors: []evaluation.LabelAnchor{{
				Path: "internal/review.go", Side: "new", StartLine: 10, EndLine: 11,
				SourceDigest: hex.EncodeToString(digest[:]),
			}},
			AnchorRefs: []string{"artifact://local/anchor"}, SuppressionTargets: []evaluation.SuppressionTarget{},
		},
		LabelPolicyRevision: "candidate-policy-1",
		ReviewState:         evaluation.ReviewPending, DatasetState: evaluation.DatasetCandidatePool,
		Split: evaluation.SplitUnassigned, CloneGroupID: "clone-" + id,
		Eligibility: evaluation.Eligibility{}, CreatedAt: at,
	}
}

func authenticatedRequest(method, target string, body io.Reader) *http.Request {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, body)
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	return request
}

func decodeResponse(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode response %q: %v", data, err)
	}
}
