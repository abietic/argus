package agentshadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/artifactrepo"
)

func TestListCommittedUsesHostWindowAndRepairsMissingIndex(t *testing.T) {
	fixture := newShadowFixture(t)
	request := fixture.request(t, "list-idempotent")
	result, err := fixture.service.Import(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// An idempotent import retry must not create another visible execution.
	if _, err := fixture.service.Import(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	window := ListRequest{
		Scope:          fixture.scope,
		StartInclusive: result.Observation.RecordedAt.Add(-time.Nanosecond),
		EndExclusive:   result.Observation.RecordedAt.Add(time.Nanosecond),
	}
	listed, err := fixture.service.List(t.Context(), window)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed) != 1 || !reflect.DeepEqual(listed[0], result) {
		t.Fatalf("List() = %+v, want one committed result", listed)
	}

	indexPath := filepath.Join(
		fixture.store.Root(),
		"immutable",
		filepath.FromSlash(committedIndexObjectID(
			fixture.scope.TenantID,
			fixture.scope.WorkspaceID,
			result.Manifest.ManifestID,
		))+".json",
	)
	if err := os.Remove(indexPath); err != nil {
		t.Fatalf("remove recoverable index entry: %v", err)
	}
	listed, err = fixture.service.List(t.Context(), window)
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0], result) {
		t.Fatalf("List() after index repair = %+v, %v", listed, err)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("List() did not repair committed index: %v", err)
	}

	for _, outside := range []ListRequest{
		{
			Scope:          fixture.scope,
			StartInclusive: result.Observation.RecordedAt.Add(time.Nanosecond),
			EndExclusive:   result.Observation.RecordedAt.Add(time.Second),
		},
		{
			Scope:          fixture.scope,
			StartInclusive: result.Observation.RecordedAt.Add(-time.Second),
			EndExclusive:   result.Observation.RecordedAt,
		},
	} {
		listed, err := fixture.service.List(t.Context(), outside)
		if err != nil || len(listed) != 0 {
			t.Fatalf("List(outside host window) = %+v, %v", listed, err)
		}
	}
}

func TestListCommittedRevalidatesBeforeApplyingHostWindow(t *testing.T) {
	fixture := newShadowFixture(t)
	result, err := fixture.service.Import(
		t.Context(),
		fixture.request(t, "list-revalidate-before-window"),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Move both discovery timestamps outside the requested window. Their
	// agreement lets enumeration succeed, but Query must still discover that
	// the import record no longer binds the immutable intent. Filtering by the
	// untrusted discovery timestamp first would silently return an empty list.
	tamperedRecord := result.Record
	tamperedRecord.AcceptedAt = result.Record.AcceptedAt.Add(24 * time.Hour)
	tamperedIndex := committedIndexEntryFromRecord(tamperedRecord)
	rewriteImmutableJSON(
		t,
		fixture.store.Root(),
		importObjectID(result.Manifest.ManifestID),
		tamperedRecord,
	)
	rewriteImmutableJSON(
		t,
		fixture.store.Root(),
		committedIndexObjectID(
			fixture.scope.TenantID,
			fixture.scope.WorkspaceID,
			result.Manifest.ManifestID,
		),
		tamperedIndex,
	)

	_, err = fixture.service.List(t.Context(), ListRequest{
		Scope:          fixture.scope,
		StartInclusive: result.Observation.RecordedAt.Add(-time.Second),
		EndExclusive:   result.Observation.RecordedAt.Add(time.Second),
	})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("List() error = %v, want ErrCorrupt instead of a silent miss", err)
	}
}

func TestReadStrictObjectDirectoryToleratesStoreAtomicTemporaryLifecycle(t *testing.T) {
	directory := t.TempDir()
	validName := strings.Repeat("a", 64) + ".json"
	if err := os.WriteFile(filepath.Join(directory, validName), []byte("{}\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	writing, err := os.CreateTemp(directory, ".argus-tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	writingPath := writing.Name()
	t.Cleanup(func() {
		_ = writing.Close()
		_ = os.Remove(writingPath)
	})
	assertStrictDirectoryNames(t, directory, []string{validName})
	if _, err := writing.WriteString("staged"); err != nil {
		t.Fatal(err)
	}
	if err := writing.Chmod(0o444); err != nil {
		t.Fatal(err)
	}
	if err := writing.Close(); err != nil {
		t.Fatal(err)
	}
	assertStrictDirectoryNames(t, directory, []string{validName})
	if err := os.Remove(writingPath); err != nil {
		t.Fatal(err)
	}

	// Repeated reads race with the same create/write/chmod/close/remove
	// lifecycle used by local.Store.atomicCreate. In particular, a temp may be
	// renamed or removed after ReadDir but before Lstat.
	producerDone := make(chan error, 1)
	producerStarted := make(chan struct{})
	go func() {
		close(producerStarted)
		for index := 0; index < 500; index++ {
			temporary, createErr := os.CreateTemp(directory, ".argus-tmp-*")
			if createErr != nil {
				producerDone <- createErr
				return
			}
			path := temporary.Name()
			if _, writeErr := temporary.WriteString("staged"); writeErr != nil {
				_ = temporary.Close()
				_ = os.Remove(path)
				producerDone <- writeErr
				return
			}
			if chmodErr := temporary.Chmod(0o444); chmodErr != nil {
				_ = temporary.Close()
				_ = os.Remove(path)
				producerDone <- chmodErr
				return
			}
			if closeErr := temporary.Close(); closeErr != nil {
				_ = os.Remove(path)
				producerDone <- closeErr
				return
			}
			if removeErr := os.Remove(path); removeErr != nil {
				producerDone <- removeErr
				return
			}
		}
		producerDone <- nil
	}()
	<-producerStarted
	for index := 0; index < 500; index++ {
		assertStrictDirectoryNames(t, directory, []string{validName})
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("temporary producer: %v", err)
	}
}

func TestReadStrictObjectDirectoryRejectsUnknownTempLikeEntries(t *testing.T) {
	tests := []struct {
		name        string
		entryName   string
		permissions os.FileMode
		makeDir     bool
	}{
		{name: "unknown suffix", entryName: ".argus-tmp-not-store-owned", permissions: 0o600},
		{name: "noncanonical numeric suffix", entryName: ".argus-tmp-01", permissions: 0o600},
		{name: "out of range suffix", entryName: ".argus-tmp-4294967296", permissions: 0o600},
		{name: "unexpected permissions", entryName: ".argus-tmp-1", permissions: 0o644},
		{name: "directory", entryName: ".argus-tmp-1", permissions: 0o700, makeDir: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, test.entryName)
			var err error
			if test.makeDir {
				err = os.Mkdir(path, test.permissions)
			} else {
				err = os.WriteFile(path, []byte("unknown"), test.permissions)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readStrictObjectDirectory(directory); err == nil ||
				!strings.Contains(err.Error(), test.entryName) {
				t.Fatalf("readStrictObjectDirectory() error = %v, want named corruption", err)
			}
		})
	}

	t.Run("unprotected directory", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, ".argus-tmp-1"), []byte("unknown"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readStrictObjectDirectory(directory); err == nil {
			t.Fatal("readStrictObjectDirectory() accepted a temp-like entry in an unprotected directory")
		}
	})
}

func TestListCommittedDoesNotExposeIntentWithoutCommit(t *testing.T) {
	fixture := newShadowFixture(t)
	digest := digestStrings("uncommitted-input")
	if _, err := fixture.repository.acquireIntent(
		t.Context(),
		fixture.scope,
		"orphan-intent",
		digest,
		*fixture.now,
	); err != nil {
		t.Fatal(err)
	}
	listed, err := fixture.service.List(t.Context(), ListRequest{
		Scope:          fixture.scope,
		StartInclusive: fixture.now.Add(-time.Second),
		EndExclusive:   fixture.now.Add(time.Second),
	})
	if err != nil || len(listed) != 0 {
		t.Fatalf("List() exposed an uncommitted intent: %+v, %v", listed, err)
	}
}

func rewriteImmutableJSON(t *testing.T, root, objectID string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join(
		root,
		"immutable",
		filepath.FromSlash(objectID)+".json",
	)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("make immutable fixture writable: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("rewrite immutable fixture: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("restore immutable fixture mode: %v", err)
	}
}

func assertStrictDirectoryNames(t *testing.T, directory string, want []string) {
	t.Helper()
	got, err := readStrictObjectDirectory(directory)
	if err != nil {
		t.Fatalf("readStrictObjectDirectory() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readStrictObjectDirectory() = %v, want %v", got, want)
	}
}

func TestListCommittedRevalidatesAndQuarantinesTamperedArtifact(t *testing.T) {
	fixture := newShadowFixture(t)
	result, err := fixture.service.Import(
		t.Context(),
		fixture.request(t, "list-tamper"),
	)
	if err != nil {
		t.Fatal(err)
	}
	ref := result.Record.ReceiptCollectionRef
	path := filepath.Join(
		fixture.store.Root(),
		"artifacts",
		"sha256",
		ref.Ref.SHA256[:2],
		ref.Ref.SHA256,
	)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.List(t.Context(), ListRequest{
		Scope:          fixture.scope,
		StartInclusive: result.Observation.RecordedAt.Add(-time.Second),
		EndExclusive:   result.Observation.RecordedAt.Add(time.Second),
	})
	if !errors.Is(err, artifactrepo.ErrQuarantined) {
		t.Fatalf("List() tamper error = %v, want ErrQuarantined", err)
	}
}
