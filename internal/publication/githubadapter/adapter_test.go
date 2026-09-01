package githubadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/publication"
	"github.com/abietic/argus/internal/store/local"
)

type allowAuthorizer struct{}

func (allowAuthorizer) AuthorizePublication(context.Context, publication.Request) error {
	return nil
}

type losePostResponseTransport struct {
	base http.RoundTripper
	once sync.Once
}

func (transport *losePostResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil || request.Method != http.MethodPost {
		return response, err
	}
	lost := false
	transport.once.Do(func() { lost = true })
	if !lost {
		return response, nil
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return nil, errors.New("connection lost after GitHub accepted comment")
}

type staticCredentials struct {
	credential Credential
	err        error
}

func (source staticCredentials) Resolve(context.Context, string) (Credential, error) {
	return source.credential, source.err
}

type recordingPermissions struct {
	mu       sync.Mutex
	requests []PermissionRequest
	decision PermissionDecision
	err      error
}

func (broker *recordingPermissions) Check(
	_ context.Context,
	request PermissionRequest,
) (PermissionDecision, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.requests = append(broker.requests, request)
	return broker.decision, broker.err
}

func TestAdapterRevalidatesExactPullRequestAndPublishesInlineComment(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 1, 0, 0, time.UTC)
	request := githubRequest(now.Add(-time.Minute))
	providerRequest := githubProviderRequest(request)
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Header.Get("Authorization") != "Bearer secret-token" ||
			incoming.Header.Get("X-GitHub-Api-Version") != apiVersion {
			t.Errorf("GitHub headers = %+v", incoming.Header)
		}
		switch {
		case incoming.Method == http.MethodGet && incoming.URL.Path == "/repos/org/repo/pulls/42":
			writeTestJSON(t, writer, http.StatusOK, map[string]any{
				"state": "open", "base": map[string]string{"sha": request.BaseRevision},
				"head": map[string]string{"sha": request.ExpectedHeadRevision},
			})
		case incoming.Method == http.MethodGet && incoming.URL.Path == "/repos/org/repo/pulls/42/files":
			writeTestJSON(t, writer, http.StatusOK, []map[string]string{{
				"filename": request.Anchor.Path,
				"patch":    "@@ -39,2 +40,4 @@\n context\n+added\n+second added\n context",
			}})
		case incoming.Method == http.MethodPost && incoming.URL.Path == "/repos/org/repo/pulls/42/comments":
			if err := json.NewDecoder(incoming.Body).Decode(&posted); err != nil {
				t.Errorf("decode comment request: %v", err)
			}
			writeTestJSON(t, writer, http.StatusCreated, map[string]any{"id": 991})
		default:
			http.NotFound(writer, incoming)
		}
	}))
	defer server.Close()

	permissions := &recordingPermissions{decision: PermissionDecision{
		Granted: true, Revision: request.ExpectedPermissionRevision,
	}}
	adapter, err := newAdapter(server.Client(), server.URL, staticCredentials{credential: Credential{
		Token: "secret-token", PrincipalID: "github-user-1", Revision: "credential-1",
	}}, permissions, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observation, err := adapter.Revalidate(context.Background(), request)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if observation.ProviderBaseRevision != request.BaseRevision ||
		observation.ProviderHeadRevision != request.ExpectedHeadRevision ||
		!observation.PermissionGranted || !observation.FindingCurrent ||
		!observation.AnchorCurrent {
		t.Fatalf("revalidation = %+v", observation)
	}
	result, err := adapter.Publish(context.Background(), providerRequest)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if result.Status != publication.ProviderResultPublished || result.CommentID != "991" {
		t.Fatalf("publish result = %+v", result)
	}
	if posted["commit_id"] != request.ExpectedHeadRevision || posted["path"] != request.Anchor.Path ||
		posted["side"] != "RIGHT" || posted["start_side"] != "RIGHT" ||
		!strings.Contains(posted["body"].(string), publicationMarker(request.IdempotencyKey)) {
		t.Fatalf("posted comment = %+v", posted)
	}
	if len(permissions.requests) != 2 ||
		permissions.requests[1].ExpectedPermissionRevision != request.ExpectedPermissionRevision {
		t.Fatalf("permission checks = %+v", permissions.requests)
	}
}

func TestAdapterLookupFindsIdempotencyMarkerWithoutPermissionWriteCheck(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 2, 0, 0, time.UTC)
	request := githubProviderRequest(githubRequest(now.Add(-time.Minute)))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method != http.MethodGet ||
			incoming.URL.Path != "/repos/org/repo/pulls/42/comments" {
			http.NotFound(writer, incoming)
			return
		}
		writeTestJSON(t, writer, http.StatusOK, []map[string]any{{
			"id": 992, "body": "message\n\n" + publicationMarker(request.IdempotencyKey),
		}})
	}))
	defer server.Close()
	permissions := &recordingPermissions{decision: PermissionDecision{Granted: false, Revision: "revoked"}}
	adapter, err := newAdapter(server.Client(), server.URL, staticCredentials{credential: Credential{
		Token: "secret-token", PrincipalID: "github-user-1", Revision: "credential-1",
	}}, permissions, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Lookup(context.Background(), request)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.Status != publication.ProviderResultPublished || result.CommentID != "992" {
		t.Fatalf("lookup result = %+v", result)
	}
	if len(permissions.requests) != 0 {
		t.Fatalf("lookup performed write permission check: %+v", permissions.requests)
	}
}

func TestPublicationServiceRecoversLostGitHubCreateResponseByMarkerLookup(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 2, 0, 0, time.UTC)
	request := githubRequest(now.Add(-time.Minute))
	var storedBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		switch {
		case incoming.Method == http.MethodGet && incoming.URL.Path == "/repos/org/repo/pulls/42":
			writeTestJSON(t, writer, http.StatusOK, map[string]any{
				"state": "open", "base": map[string]string{"sha": request.BaseRevision},
				"head": map[string]string{"sha": request.ExpectedHeadRevision},
			})
		case incoming.Method == http.MethodGet && incoming.URL.Path == "/repos/org/repo/pulls/42/files":
			writeTestJSON(t, writer, http.StatusOK, []map[string]string{{
				"filename": request.Anchor.Path,
				"patch":    "@@ -39,2 +40,4 @@\n context\n+added\n+second added\n context",
			}})
		case incoming.Method == http.MethodPost && incoming.URL.Path == "/repos/org/repo/pulls/42/comments":
			var payload map[string]any
			if err := json.NewDecoder(incoming.Body).Decode(&payload); err != nil {
				t.Errorf("decode create payload: %v", err)
			}
			storedBody, _ = payload["body"].(string)
			writeTestJSON(t, writer, http.StatusCreated, map[string]any{"id": 993})
		case incoming.Method == http.MethodGet && incoming.URL.Path == "/repos/org/repo/pulls/42/comments":
			comments := []map[string]any{}
			if storedBody != "" {
				comments = append(comments, map[string]any{"id": 993, "body": storedBody})
			}
			writeTestJSON(t, writer, http.StatusOK, comments)
		default:
			http.NotFound(writer, incoming)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = &losePostResponseTransport{base: client.Transport}
	permissions := &recordingPermissions{decision: PermissionDecision{
		Granted: true, Revision: request.ExpectedPermissionRevision,
	}}
	provider, err := newAdapter(client, server.URL, staticCredentials{credential: Credential{
		Token: "secret-token", PrincipalID: "github-user-1", Revision: "credential-1",
	}}, permissions, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := publication.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := publication.NewService(
		repository, provider, allowAuthorizer{}, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Publish(context.Background(), request)
	if !errors.Is(err, publication.ErrUnknownOutcome) || first.State != publication.StateUnknown {
		t.Fatalf("lost response state/error = %s, %v", first.State, err)
	}
	reconciled, err := service.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reconciled.State != publication.StatePublished ||
		reconciled.ProviderResult == nil || reconciled.ProviderResult.CommentID != "993" {
		t.Fatalf("reconciled record = %+v", reconciled)
	}
	if err := filepath.WalkDir(store.Root(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "secret-token") {
			t.Fatalf("publication store leaked GitHub token in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("scan publication store: %v", err)
	}
}

func TestAdapterFailsClosedForMovedBaseStaleAnchorAndPermissionRevision(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 3, 0, 0, time.UTC)
	request := githubRequest(now.Add(-time.Minute))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		switch {
		case incoming.URL.Path == "/repos/org/repo/pulls/42":
			writeTestJSON(t, writer, http.StatusOK, map[string]any{
				"state": "open", "base": map[string]string{"sha": strings.Repeat("e", 40)},
				"head": map[string]string{"sha": request.ExpectedHeadRevision},
			})
		case incoming.URL.Path == "/repos/org/repo/pulls/42/files":
			writeTestJSON(t, writer, http.StatusOK, []map[string]string{})
		default:
			http.NotFound(writer, incoming)
		}
	}))
	defer server.Close()
	permissions := &recordingPermissions{decision: PermissionDecision{
		Granted: true, Revision: request.ExpectedPermissionRevision,
	}}
	adapter, err := newAdapter(server.Client(), server.URL, staticCredentials{credential: Credential{
		Token: "secret-token", PrincipalID: "github-user-1", Revision: "credential-1",
	}}, permissions, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observation, err := adapter.Revalidate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ProviderBaseRevision == request.BaseRevision || observation.AnchorCurrent {
		t.Fatalf("moved base observation = %+v", observation)
	}

	permissions.decision = PermissionDecision{Granted: true, Revision: "permission-new"}
	observation, err = adapter.Revalidate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.PermissionGranted || observation.PermissionRevision != "permission-new" ||
		observation.AnchorCurrent {
		t.Fatalf("permission drift observation = %+v", observation)
	}
}

func TestPatchContainsHeadRange(t *testing.T) {
	patch := "@@ -1,3 +1,4 @@\n same\n-old\n+new\n+newer\n tail"
	for _, test := range []struct {
		start uint32
		end   uint32
		want  bool
	}{{1, 1, true}, {2, 3, true}, {4, 4, true}, {5, 5, false}, {3, 2, false}} {
		if got := patchContainsHeadRange(patch, test.start, test.end); got != test.want {
			t.Fatalf("range %d:%d = %v, want %v", test.start, test.end, got, test.want)
		}
	}
}

func githubRequest(createdAt time.Time) publication.Request {
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	targetDigest := strings.Repeat("c", 64)
	return publication.Request{
		SchemaVersion: publication.RequestSchemaVersion,
		PublicationID: "publication-1", IdempotencyKey: "publication-key-1",
		GrantID: "grant-1", GrantSHA256: strings.Repeat("d", 64),
		TenantID: "tenant-1", WorkspaceID: "workspace-1", RunID: "run-1",
		RunKind: publication.RunKindReview, RunStatus: publication.RunStatusSucceeded,
		TargetMode: publication.TargetModeDiff,
		Provider:   ProviderID, RepositoryID: "org/repo",
		ChangeKind: "pull_request", ChangeID: "42",
		BaseRevision: base, ExpectedHeadRevision: head,
		TargetSnapshotRef:     testContentRef("target", 'a'),
		ConfigBundleRef:       testContentRef("config", 'b'),
		FindingSourceRef:      testContentRef("finding", 'c'),
		FindingSourceContract: publication.FindingSourceFindingSet,
		FindingID:             "finding-1", Fingerprint: "fingerprint-1", DecisionID: "decision-1",
		TargetDigest: targetDigest,
		Anchor: publication.StableAnchor{
			Path: "internal/review.go", Side: "head", StartLine: 41, EndLine: 42,
			TargetDigest: targetDigest,
		},
		Channel: "pull_request_inline", Message: "verified finding", RemoteWrites: "allow",
		ExpectedPermissionRevision: "permission-1",
		GrantExpiresAt:             createdAt.Add(time.Hour), CreatedAt: createdAt,
	}
}

func githubProviderRequest(request publication.Request) publication.ProviderRequest {
	return publication.ProviderRequest{
		SchemaVersion: publication.ProviderRequestSchema,
		PublicationID: request.PublicationID, IdempotencyKey: request.IdempotencyKey,
		GrantID: request.GrantID, Provider: request.Provider, RepositoryID: request.RepositoryID,
		ChangeKind: request.ChangeKind, ChangeID: request.ChangeID,
		BaseRevision: request.BaseRevision, HeadRevision: request.ExpectedHeadRevision,
		FindingID: request.FindingID, Fingerprint: request.Fingerprint,
		DecisionID: request.DecisionID, PermissionRevision: request.ExpectedPermissionRevision,
		Anchor: request.Anchor, Channel: request.Channel, Message: request.Message,
		RequestSHA256: strings.Repeat("e", 64),
	}
}

func testContentRef(name string, character byte) publication.ContentRef {
	return publication.ContentRef{
		URI: "artifact://local/" + name, SHA256: strings.Repeat(string(character), 64),
		SizeBytes: 1,
	}
}

func writeTestJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode test response: %v", err)
	}
}
