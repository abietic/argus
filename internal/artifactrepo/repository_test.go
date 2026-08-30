package artifactrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

var testTime = time.Date(2026, 7, 27, 2, 0, 0, 0, time.UTC)

func TestRepositoryAuthorizesUseAndRejectsForgedOrCrossTenantRefs(t *testing.T) {
	repository, _ := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	subject.Roles = []string{RoleEvaluationUse}
	ref, record, err := repository.Put(
		context.Background(),
		subject,
		testPutRequest(subject, []Use{UseEvaluation, UseRead}),
	)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if record.State != StateActive ||
		record.Metadata.Content.URI == ref.URI {
		t.Fatalf("record = %+v, ref = %+v", record, ref)
	}
	content, _, err := repository.Resolve(
		context.Background(),
		subject,
		ref,
		UseEvaluation,
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !bytes.Equal(content, []byte("frozen source\n")) {
		t.Fatalf("content = %q", content)
	}
	if _, _, err := repository.Resolve(
		context.Background(),
		subject,
		ref,
		UsePublication,
	); !errors.Is(err, ErrUseDenied) {
		t.Fatalf("publication error = %v, want ErrUseDenied", err)
	}
	otherTenant := testSubject("tenant-b", "workspace-a")
	if _, _, err := repository.Resolve(
		context.Background(),
		otherTenant,
		ref,
		UseRead,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-tenant error = %v, want ErrUnauthorized", err)
	}
	for name, forged := range map[string]Ref{
		"digest": {
			URI: ref.URI, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SizeBytes: ref.SizeBytes, Contract: ref.Contract,
		},
		"contract": {
			URI: ref.URI, SHA256: ref.SHA256,
			SizeBytes: ref.SizeBytes, Contract: "argus.other.v1",
		},
		"direct location": {
			URI: "file:///etc/passwd", SHA256: ref.SHA256,
			SizeBytes: ref.SizeBytes, Contract: ref.Contract,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := repository.Resolve(
				context.Background(),
				subject,
				forged,
				UseRead,
			); err == nil {
				t.Fatal("Resolve() accepted forged ref")
			}
		})
	}
}

func TestCorruptionQuarantinesUntilVerifiedReleaseThenTombstonePersists(t *testing.T) {
	repository, store := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	operator := subject
	operator.Roles = []string{RoleIntegrityOperator}
	ref, record, err := repository.Put(
		context.Background(),
		subject,
		testPutRequest(subject, []Use{UseRead}),
	)
	if err != nil {
		t.Fatal(err)
	}
	contentPath := filepath.Join(
		store.Root(),
		"artifacts",
		"sha256",
		ref.SHA256[:2],
		ref.SHA256,
	)
	if err := os.Chmod(contentPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contentPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.Resolve(
		context.Background(),
		subject,
		ref,
		UseRead,
	); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("Resolve() error = %v, want ErrQuarantined", err)
	}
	record, err = repository.Inspect(context.Background(), subject, ref)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != StateQuarantined ||
		record.StateReason != "checksum_verification_failed" {
		t.Fatalf("quarantine record = %+v", record)
	}
	if _, err := repository.ReleaseQuarantine(
		context.Background(),
		operator,
		ref,
		"operator requested release",
		testMutation("release-too-early", testTime.Add(2*time.Minute)),
	); err == nil {
		t.Fatal("ReleaseQuarantine() accepted corrupt content")
	}
	original := []byte("frozen source\n")
	if err := os.WriteFile(contentPath, original, 0o444); err != nil {
		t.Fatal(err)
	}
	record, err = repository.ReleaseQuarantine(
		context.Background(),
		operator,
		ref,
		"checksum restored",
		testMutation("release", testTime.Add(3*time.Minute)),
	)
	if err != nil {
		t.Fatalf("ReleaseQuarantine() error = %v", err)
	}
	if record.State != StateActive {
		t.Fatalf("released record = %+v", record)
	}
	record, err = repository.Tombstone(
		context.Background(),
		operator,
		ref,
		"retention policy expired",
		testMutation("tombstone", testTime.Add(4*time.Minute)),
	)
	if err != nil {
		t.Fatalf("Tombstone() error = %v", err)
	}
	if record.State != StateTombstoned {
		t.Fatalf("tombstoned record = %+v", record)
	}
	if _, _, err := repository.Resolve(
		context.Background(),
		subject,
		ref,
		UseRead,
	); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("Resolve() after tombstone error = %v", err)
	}
	if _, err := os.Stat(contentPath); err != nil {
		t.Fatalf("content-addressed bytes unexpectedly removed: %v", err)
	}
}

func TestPutIsConcurrentAndRestartSafe(t *testing.T) {
	repository, store := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	const writers = 16
	start := make(chan struct{})
	refs := make(chan Ref, writers)
	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			request := testPutRequest(subject, []Use{UseRead})
			request.Mutation = Mutation{
				IdempotencyKey: fmt.Sprintf("put-%d", index),
				Actor:          fmt.Sprintf("actor-%d", index),
				Audit:          "concurrent content ingestion",
				At:             testTime.Add(time.Duration(index) * time.Second),
			}
			ref, _, err := repository.Put(context.Background(), subject, request)
			if err != nil {
				errs <- err
				return
			}
			refs <- ref
		}(index)
	}
	close(start)
	wait.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		t.Errorf("Put() error = %v", err)
	}
	var expected Ref
	count := 0
	for ref := range refs {
		if count == 0 {
			expected = ref
		} else if ref != expected {
			t.Errorf("ref = %+v, want %+v", ref, expected)
		}
		count++
	}
	if count != writers {
		t.Fatalf("successful writers = %d, want %d", count, writers)
	}
	restarted, err := Open(store, func() time.Time {
		return testTime.Add(time.Hour)
	})
	if err != nil {
		t.Fatal(err)
	}
	content, record, err := restarted.Resolve(
		context.Background(),
		subject,
		expected,
		UseRead,
	)
	if err != nil {
		t.Fatalf("restarted Resolve() error = %v", err)
	}
	if string(content) != "frozen source\n" || record.State != StateActive {
		t.Fatalf("restart content = %q, record = %+v", content, record)
	}
}

func TestStateMutationsRequireOperatorAndIdempotencyConflictsFail(t *testing.T) {
	repository, _ := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	ref, _, err := repository.Put(
		context.Background(),
		subject,
		testPutRequest(subject, []Use{UseRead}),
	)
	if err != nil {
		t.Fatal(err)
	}
	mutation := testMutation("quarantine", testTime.Add(time.Minute))
	if _, err := repository.Quarantine(
		context.Background(),
		subject,
		ref,
		"manual integrity hold",
		mutation,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Quarantine() error = %v, want ErrUnauthorized", err)
	}
	subject.Roles = []string{RoleIntegrityOperator}
	first, err := repository.Quarantine(
		context.Background(),
		subject,
		ref,
		"manual integrity hold",
		mutation,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repository.Quarantine(
		context.Background(),
		subject,
		ref,
		"manual integrity hold",
		mutation,
	)
	if err != nil || second.State != StateQuarantined {
		t.Fatalf("idempotent Quarantine() record = %+v, error = %v", second, err)
	}
	if _, err := repository.Quarantine(
		context.Background(),
		subject,
		ref,
		"different reason",
		mutation,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Quarantine() error = %v, want ErrConflict", err)
	}
	if first.State != StateQuarantined {
		t.Fatalf("record = %+v", first)
	}
}

func newTestRepository(t *testing.T) (*Repository, *local.Store) {
	t.Helper()
	store, err := local.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := Open(store, func() time.Time {
		return testTime.Add(time.Minute)
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository, store
}

func testSubject(tenantID string, workspaceID string) Subject {
	return Subject{
		TenantID: tenantID, WorkspaceID: workspaceID, Roles: []string{},
	}
}

func testPutRequest(subject Subject, uses []Use) PutRequest {
	return PutRequest{
		Authority:   "argus-local",
		TenantID:    subject.TenantID,
		WorkspaceID: subject.WorkspaceID,
		Contract:    "argus.test_artifact.v1",
		AllowedUses: uses,
		Content:     []byte("frozen source\n"),
		Mutation:    testMutation("put", testTime),
	}
}

func testMutation(id string, at time.Time) Mutation {
	return Mutation{
		IdempotencyKey: id,
		Actor:          "artifact-operator",
		Audit:          "artifact repository test",
		At:             at,
	}
}
