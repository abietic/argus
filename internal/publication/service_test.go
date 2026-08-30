package publication

import (
	"context"
	"errors"
	"testing"
	"time"

	"argus.local/argus/internal/store/local"
)

type fakeProvider struct {
	revalidation  Revalidation
	revalidateErr error
	publishResult ProviderResult
	publishErr    error
	lookupResult  ProviderResult
	lookupErr     error
	publishCalls  int
	lookupCalls   int
}

type acceptingAuthorizer struct{}

func (acceptingAuthorizer) AuthorizePublication(context.Context, Request) error { return nil }

type controlledAuthorizer struct {
	err error
}

func (authority *controlledAuthorizer) AuthorizePublication(
	context.Context,
	Request,
) error {
	return authority.err
}

type revokingAuthorizer struct {
	calls int
}

func (authority *revokingAuthorizer) AuthorizePublication(
	context.Context,
	Request,
) error {
	authority.calls++
	if authority.calls >= 2 {
		return errors.New("decision authority was superseded")
	}
	return nil
}

func (provider *fakeProvider) Revalidate(context.Context, Request) (Revalidation, error) {
	return provider.revalidation, provider.revalidateErr
}

func (provider *fakeProvider) Publish(
	_ context.Context,
	_ ProviderRequest,
) (ProviderResult, error) {
	provider.publishCalls++
	return provider.publishResult, provider.publishErr
}

func (provider *fakeProvider) Lookup(
	_ context.Context,
	_ ProviderRequest,
) (ProviderResult, error) {
	provider.lookupCalls++
	return provider.lookupResult, provider.lookupErr
}

func TestPublicationRequiresExactFreshRevalidation(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	request := validRequest(now)
	provider := validProvider(request, now)
	provider.revalidation.ProviderHeadRevision = "moved-head"
	authority := &controlledAuthorizer{}
	service := newTestServiceWithAuthority(t, provider, authority, now)

	record, err := service.Publish(context.Background(), request)
	if err == nil || err.Error() != "provider head moved" {
		t.Fatalf("Publish error = %v, want provider head moved", err)
	}
	if record.State != StateRejected || provider.publishCalls != 0 {
		t.Fatalf("record state/calls = %q/%d, want rejected/0",
			record.State, provider.publishCalls)
	}

	again, err := service.Publish(context.Background(), request)
	if err != nil || again.State != StateRejected || provider.publishCalls != 0 {
		t.Fatalf("idempotent rejected retry = %#v, %v, calls=%d",
			again, err, provider.publishCalls)
	}
}

func TestPublicationRequiresAuthorityAndRechecksBeforeDispatch(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(now)
	provider := validProvider(request, now)
	if _, err := NewService(repository, provider, nil, nil); err == nil {
		t.Fatal("publication service accepted a missing request authorizer")
	}
	authority := &revokingAuthorizer{}
	service, err := NewService(repository, provider, authority, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(context.Background(), request); err == nil ||
		authority.calls != 2 {
		t.Fatalf("Publish error/calls = %v/%d", err, authority.calls)
	}
	if provider.publishCalls != 0 {
		t.Fatalf("provider publish calls = %d, want 0", provider.publishCalls)
	}
	record, err := repository.loadOne(request.PublicationID)
	if err != nil || record.State != StateRequested {
		t.Fatalf("durable pre-dispatch record = %+v, %v", record, err)
	}
}

func TestRepositoryQueriesProviderFactsByExactRunAndFinding(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	request := validRequest(now)
	service := newTestService(t, validProvider(request, now), now)
	record, err := service.Publish(context.Background(), request)
	if err != nil || record.State != StatePublished {
		t.Fatalf("Publish() = %+v, %v", record, err)
	}

	records, err := service.repository.ByFinding(request.RunID, request.FindingID)
	if err != nil {
		t.Fatalf("ByFinding() error = %v", err)
	}
	if len(records) != 1 ||
		records[0].Request.PublicationID != request.PublicationID ||
		records[0].State != StatePublished {
		t.Fatalf("ByFinding() = %+v", records)
	}
	foreign, err := service.repository.ByFinding(request.RunID, "another-finding")
	if err != nil || len(foreign) != 0 {
		t.Fatalf("ByFinding(foreign) = %+v, %v", foreign, err)
	}
}

func TestTerminalPublicationRetryDoesNotRequireCurrentAuthority(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	request := validRequest(now)
	provider := validProvider(request, now)
	authority := &controlledAuthorizer{}
	service := newTestServiceWithAuthority(t, provider, authority, now)
	first, err := service.Publish(context.Background(), request)
	if err != nil || first.State != StatePublished {
		t.Fatalf("first publication = %+v, %v", first, err)
	}
	authority.err = errors.New("authority later revoked")
	retry, err := service.Publish(context.Background(), request)
	if err != nil || retry.State != StatePublished || provider.publishCalls != 1 {
		t.Fatalf("terminal retry = %+v, %v, calls=%d", retry, err, provider.publishCalls)
	}
}

func TestPublicationUnknownOutcomeOnlyReconciles(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	request := validRequest(now)
	provider := validProvider(request, now)
	provider.publishErr = errors.New("connection reset after request body")
	provider.lookupResult = ProviderResult{
		SchemaVersion:     ProviderResultSchema,
		Status:            ProviderResultPublished,
		IdempotencyKey:    request.IdempotencyKey,
		ProviderRequestID: "provider-request-1",
		CommentID:         "comment-1",
		ObservedAt:        now.Add(time.Second),
	}
	authority := &controlledAuthorizer{}
	service := newTestServiceWithAuthority(t, provider, authority, now)

	record, err := service.Publish(context.Background(), request)
	if !errors.Is(err, ErrUnknownOutcome) || record.State != StateUnknown {
		t.Fatalf("first Publish = state %q, err %v", record.State, err)
	}
	if provider.publishCalls != 1 || provider.lookupCalls != 0 {
		t.Fatalf("first calls = publish %d lookup %d", provider.publishCalls,
			provider.lookupCalls)
	}
	authority.err = errors.New("authority revoked after uncertain dispatch")

	record, err = service.Publish(context.Background(), request)
	if err != nil || record.State != StatePublished {
		t.Fatalf("retry Publish = state %q, err %v", record.State, err)
	}
	if provider.publishCalls != 1 || provider.lookupCalls != 1 {
		t.Fatalf("retry blindly published: publish %d lookup %d",
			provider.publishCalls, provider.lookupCalls)
	}

	reopenedRepository, err := NewRepository(openStore(t, service.repository.store.Root()))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewService(reopenedRepository, provider, acceptingAuthorizer{}, func() time.Time {
		return now.Add(2 * time.Second)
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err = reopened.Publish(context.Background(), request)
	if err != nil || record.State != StatePublished ||
		provider.publishCalls != 1 || provider.lookupCalls != 1 {
		t.Fatalf("restart retry = %#v, %v, publish=%d lookup=%d",
			record, err, provider.publishCalls, provider.lookupCalls)
	}
}

func TestPublicationRejectsReplaySelectionCanceledAndChangedIdentity(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name   string
		change func(*Request)
	}{
		{name: "replay", change: func(request *Request) { request.RunKind = RunKindReplay }},
		{name: "selection", change: func(request *Request) {
			request.TargetMode = TargetModeSelection
		}},
		{name: "canceled", change: func(request *Request) {
			request.RunStatus = RunStatusCanceled
		}},
		{name: "remote deny", change: func(request *Request) {
			request.RemoteWrites = "deny"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest(now)
			test.change(&request)
			provider := validProvider(request, now)
			service := newTestService(t, provider, now)
			if _, err := service.Publish(context.Background(), request); err == nil {
				t.Fatal("Publish unexpectedly succeeded")
			}
			if provider.publishCalls != 0 {
				t.Fatalf("provider publish calls = %d, want 0", provider.publishCalls)
			}
		})
	}

	request := validRequest(now)
	provider := validProvider(request, now)
	service := newTestService(t, provider, now)
	if _, err := service.Publish(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Message = "different message"
	if _, err := service.Publish(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotent request error = %v, want conflict", err)
	}
	if provider.publishCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.publishCalls)
	}
}

func TestDisabledProviderFailsBeforeRemoteBoundary(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	request := validRequest(now)
	service := newTestService(t, DisabledProvider{}, now)
	if _, err := service.Publish(context.Background(), request); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Publish error = %v, want disabled", err)
	}
}

func TestTransientRevalidationFailureCanRetryBeforeDispatch(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	request := validRequest(now)
	provider := validProvider(request, now)
	provider.revalidateErr = errors.New("temporary permission service failure")
	service := newTestService(t, provider, now)

	if _, err := service.Publish(context.Background(), request); err == nil {
		t.Fatal("Publish unexpectedly succeeded")
	}
	if provider.publishCalls != 0 {
		t.Fatalf("provider publish calls = %d, want 0", provider.publishCalls)
	}
	provider.revalidateErr = nil
	record, err := service.Publish(context.Background(), request)
	if err != nil || record.State != StatePublished || provider.publishCalls != 1 {
		t.Fatalf("safe retry = state %q, err %v, publish calls %d",
			record.State, err, provider.publishCalls)
	}
}

func TestDecodeEventRejectsDuplicateAndTrailingJSON(t *testing.T) {
	for name, data := range map[string][]byte{
		"duplicate": []byte(`{"schema_version":"x","schema_version":"y"}`),
		"trailing":  []byte(`{} {}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeEvent(data); err == nil {
				t.Fatal("decodeEvent unexpectedly succeeded")
			}
		})
	}
}

func validRequest(now time.Time) Request {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return Request{
		SchemaVersion:        RequestSchemaVersion,
		PublicationID:        "publication-1",
		IdempotencyKey:       "publication-key-1",
		GrantID:              "grant-1",
		GrantSHA256:          "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		TenantID:             "tenant-1",
		WorkspaceID:          "workspace-1",
		RunID:                "run-1",
		RunKind:              RunKindReview,
		RunStatus:            RunStatusSucceeded,
		TargetMode:           TargetModeDiff,
		Provider:             "fake",
		RepositoryID:         "repository-1",
		ChangeKind:           "pull_request",
		ChangeID:             "42",
		BaseRevision:         "base-commit",
		ExpectedHeadRevision: "head-commit",
		TargetSnapshotRef: ContentRef{
			URI: "artifact://argus/targets/1", SHA256: digest, SizeBytes: 10,
		},
		ConfigBundleRef: ContentRef{
			URI:       "artifact://argus/config/1",
			SHA256:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			SizeBytes: 11,
		},
		FindingSourceRef: ContentRef{
			URI:       "artifact://argus/findings/1",
			SHA256:    "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			SizeBytes: 12,
		},
		FindingSourceContract: FindingSourceFindingSet,
		FindingID:             "finding-1",
		Fingerprint:           "fingerprint-1",
		DecisionID:            "decision-1",
		TargetDigest:          digest,
		Anchor: StableAnchor{
			Path: "internal/review.go", Side: "head",
			StartLine: 42, EndLine: 42, TargetDigest: digest,
		},
		Channel:                    "pull_request_inline",
		Message:                    "This finding has independently verified evidence.",
		RemoteWrites:               "allow",
		ExpectedPermissionRevision: "permission-7",
		GrantExpiresAt:             now.Add(time.Hour),
		CreatedAt:                  now,
	}
}

func validProvider(request Request, now time.Time) *fakeProvider {
	return &fakeProvider{
		revalidation: Revalidation{
			SchemaVersion:        RevalidationSchemaVersion,
			ProviderBaseRevision: request.BaseRevision,
			ProviderHeadRevision: request.ExpectedHeadRevision,
			PermissionGranted:    true,
			PermissionRevision:   request.ExpectedPermissionRevision,
			ConfigBundleSHA256:   request.ConfigBundleRef.SHA256,
			FindingSourceSHA256:  request.FindingSourceRef.SHA256,
			FindingCurrent:       true,
			AnchorCurrent:        true,
			CheckedAt:            now,
		},
		publishResult: ProviderResult{
			SchemaVersion:     ProviderResultSchema,
			Status:            ProviderResultPublished,
			IdempotencyKey:    request.IdempotencyKey,
			ProviderRequestID: "provider-request-1",
			CommentID:         "comment-1",
			ObservedAt:        now,
		},
		lookupResult: ProviderResult{
			SchemaVersion:  ProviderResultSchema,
			Status:         ProviderResultNotFound,
			IdempotencyKey: request.IdempotencyKey,
			ObservedAt:     now,
		},
	}
}

func newTestService(t *testing.T, provider Provider, now time.Time) *Service {
	return newTestServiceWithAuthority(t, provider, acceptingAuthorizer{}, now)
}

func newTestServiceWithAuthority(
	t *testing.T,
	provider Provider,
	authority RequestAuthorizer,
	now time.Time,
) *Service {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, provider, authority, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func openStore(t *testing.T, root string) *local.Store {
	t.Helper()
	store, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
