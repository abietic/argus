package publication

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

func TestGrantRepositoryRecordsReservesAndRestoresExactSingleUseGrant(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewGrantRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, binding := grantFixture()
	grant, err := repository.Record(context.Background(), request, mutation, binding)
	if err != nil {
		t.Fatalf("record grant: %v", err)
	}
	if grant.GrantSHA256 == "" || grant.DecisionID != binding.DecisionID {
		t.Fatalf("grant = %+v", grant)
	}
	retry, err := repository.Record(context.Background(), request, mutation, binding)
	if err != nil || retry.GrantSHA256 != grant.GrantSHA256 {
		t.Fatalf("exact record retry = %+v, %v", retry, err)
	}
	reservedAt := mutation.At.Add(time.Minute)
	reservation, err := repository.Reserve(
		context.Background(), grant.GrantID, grant.GrantSHA256,
		grant.PublicationID, grant.PublicationIdempotencyKey, reservedAt,
	)
	if err != nil {
		t.Fatalf("reserve grant: %v", err)
	}
	if reservation.GrantID != grant.GrantID {
		t.Fatalf("reservation = %+v", reservation)
	}
	exact, err := repository.Reserve(
		context.Background(), grant.GrantID, grant.GrantSHA256,
		grant.PublicationID, grant.PublicationIdempotencyKey, reservedAt,
	)
	if err != nil || exact != reservation {
		t.Fatalf("exact reservation retry = %+v, %v", exact, err)
	}
	if _, err := repository.Reserve(
		context.Background(), grant.GrantID, grant.GrantSHA256,
		grant.PublicationID, grant.PublicationIdempotencyKey, reservedAt.Add(time.Second),
	); !errors.Is(err, ErrGrantConsumed) {
		t.Fatalf("second reservation error = %v, want consumed", err)
	}

	restarted, err := NewGrantRepository(store)
	if err != nil {
		t.Fatalf("restore grant repository: %v", err)
	}
	restored, err := restarted.Get(grant.GrantID)
	if err != nil || restored.GrantSHA256 != grant.GrantSHA256 {
		t.Fatalf("restored grant = %+v, %v", restored, err)
	}
	restoredReservation, err := restarted.Reservation(grant.GrantID)
	if err != nil || restoredReservation == nil || *restoredReservation != reservation {
		t.Fatalf("restored reservation = %+v, %v", restoredReservation, err)
	}
}

func TestGrantRepositoryRejectsConflictsExpiryAndCancellation(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewGrantRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, binding := grantFixture()
	grant, err := repository.Record(context.Background(), request, mutation, binding)
	if err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Channel = "pull_request_summary"
	if _, err := repository.Record(context.Background(), changed, mutation, binding); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("changed retry error = %v, want conflict", err)
	}
	if _, err := repository.Reserve(
		context.Background(), grant.GrantID, grant.GrantSHA256,
		grant.PublicationID, grant.PublicationIdempotencyKey, grant.ExpiresAt,
	); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("expired reservation error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	request.GrantID = "grant-canceled"
	request.PublicationID = "publication-canceled"
	request.PublicationIdempotencyKey = "publication-key-canceled"
	mutation.IdempotencyKey = "grant-key-canceled"
	if _, err := repository.Record(canceled, request, mutation, binding); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled record error = %v", err)
	}
	if _, err := repository.Get(request.GrantID); err == nil {
		t.Fatal("canceled grant was persisted")
	}
}

func TestGrantRepositoryRejectsCorruptReservationOnRestore(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewGrantRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, binding := grantFixture()
	grant, err := repository.Record(context.Background(), request, mutation, binding)
	if err != nil {
		t.Fatal(err)
	}
	bad := GrantReservation{
		SchemaVersion: GrantReservationSchemaVersion,
		GrantID:       grant.GrantID, GrantSHA256: grant.GrantSHA256,
		PublicationID: "wrong-publication", IdempotencyKey: grant.PublicationIdempotencyKey,
		ReservedAt: mutation.At.Add(time.Minute),
	}
	if _, err := store.AppendJSONL(grantLedgerStream, local.Event{
		ID:     grantReservationEventID(grant.GrantID),
		Schema: GrantReservationSchemaVersion, Time: bad.ReservedAt, Payload: bad,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGrantRepository(store); !errors.Is(err, ErrGrantCorrupt) {
		t.Fatalf("restore error = %v, want corrupt", err)
	}
}

func TestGrantStrictDecodersRejectUnknownAndDuplicateFields(t *testing.T) {
	requestJSON := []byte(`{"schema_version":"argus.publication_grant_request.v1alpha1","grant_id":"grant-1","publication_id":"publication-1","publication_idempotency_key":"publication-key-1","run_id":"run-1","finding_id":"finding-1","provider":"github","repository_id":"org/repository","change_kind":"pull_request","change_id":"42","channel":"pull_request_inline","expected_permission_revision":"permission-1","occurred_at":"2026-08-25T06:00:00Z","expires_at":"2026-08-25T07:00:00Z"}`)
	if _, err := DecodeGrantRequestJSON(requestJSON); err != nil {
		t.Fatalf("decode grant request: %v", err)
	}
	mutationJSON := []byte(`{"schema_version":"argus.publication_grant_mutation.v1alpha1","idempotency_key":"grant-key-1","actor":{"kind":"human","id":"approver-1"},"roles":["publication_approver"],"audit":"approved exact publication","at":"2026-08-25T06:01:00Z"}`)
	if _, err := DecodeGrantMutationJSON(mutationJSON); err != nil {
		t.Fatalf("decode grant mutation: %v", err)
	}
	unknown := append(requestJSON[:len(requestJSON)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeGrantRequestJSON(unknown); err == nil {
		t.Fatal("unknown grant request field was accepted")
	}
	duplicate := []byte(`{"schema_version":"argus.publication_grant_request.v1alpha1","grant_id":"grant-1","grant_id":"grant-2"}`)
	if _, err := DecodeGrantRequestJSON(duplicate); err == nil {
		t.Fatal("duplicate grant request field was accepted")
	}
}

func grantFixture() (GrantRequest, GrantMutation, GrantBinding) {
	at := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	return GrantRequest{
			SchemaVersion: GrantRequestSchemaVersion,
			GrantID:       "grant-1", PublicationID: "publication-1",
			PublicationIdempotencyKey: "publication-key-1",
			RunID:                     "run-1", FindingID: "finding-1", Channel: "pull_request_inline",
			Provider: "github", RepositoryID: "org/repository",
			ChangeKind: "pull_request", ChangeID: "42",
			ExpectedPermissionRevision: "single-user-local-v1",
			OccurredAt:                 at, ExpiresAt: at.Add(time.Hour),
		}, GrantMutation{
			SchemaVersion:  GrantMutationSchemaVersion,
			IdempotencyKey: "grant-key-1",
			Actor:          GrantActor{Kind: GrantActorHuman, ID: "approver-1"},
			Roles:          []GrantRole{GrantRolePublicationApprover},
			Audit:          "approved exact publication", At: at.Add(time.Minute),
		}, GrantBinding{
			TenantID: "tenant-1", WorkspaceID: "workspace-1", DecisionID: "decision-1",
			Provider: "github", RepositoryID: "org/repository",
			BaseRevision: "base-1", ExpectedHeadRevision: "head-1",
			TargetSnapshotRef:     grantContentRef("target", 'a'),
			ConfigBundleRef:       grantContentRef("config", 'b'),
			FindingSourceRef:      grantContentRef("findings", 'c'),
			FindingSourceContract: FindingSourceGovernedReviewReport,
		}
}

func grantContentRef(name string, character byte) ContentRef {
	digest := make([]byte, 64)
	for index := range digest {
		digest[index] = character
	}
	return ContentRef{
		URI: "artifact://local/" + name, SHA256: string(digest), SizeBytes: 1,
	}
}
