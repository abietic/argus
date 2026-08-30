package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPutAndReadArtifactIsContentAddressed(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	content := []byte("immutable review input\n")

	first, err := store.PutArtifact(content)
	if err != nil {
		t.Fatalf("PutArtifact first: %v", err)
	}
	second, err := store.PutArtifact(content)
	if err != nil {
		t.Fatalf("PutArtifact duplicate: %v", err)
	}
	if first != second {
		t.Fatalf("duplicate artifact refs differ: %#v != %#v", first, second)
	}
	if first.URI != artifactURIPrefix+first.SHA256 {
		t.Fatalf("unexpected URI %q", first.URI)
	}
	actual, err := store.ReadArtifact(first)
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if !bytes.Equal(actual, content) {
		t.Fatalf("artifact content = %q, want %q", actual, content)
	}

	target, err := store.artifactPath(first.SHA256, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadArtifact(first); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadArtifact corruption error = %v, want ErrCorrupt", err)
	}
	if _, err := store.PutArtifact(content); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("PutArtifact over corruption error = %v, want ErrCorrupt", err)
	}
}

func TestReadArtifactRejectsForgedReferences(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ref, err := store.PutArtifact([]byte("safe"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []ArtifactRef{
		{URI: "file:///etc/passwd", SHA256: ref.SHA256, SizeBytes: ref.SizeBytes},
		{URI: artifactURIPrefix + "../" + ref.SHA256, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes},
		{URI: ref.URI + "?source=remote", SHA256: ref.SHA256, SizeBytes: ref.SizeBytes},
		{URI: ref.URI, SHA256: strings.Repeat("A", 64), SizeBytes: ref.SizeBytes},
	}
	for _, test := range tests {
		if _, err := store.ReadArtifact(test); err == nil {
			t.Fatalf("ReadArtifact(%q) unexpectedly succeeded", test.URI)
		}
	}
}

func TestReadArtifactRejectsSymlink(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	content := []byte("same bytes outside the store")
	ref, err := store.PutArtifact(content)
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.artifactPath(ref.SHA256, false)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-artifact")
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadArtifact(ref); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ReadArtifact symlink error = %v, want ErrUnsafePath", err)
	}
}

func TestReadArtifactRejectsGrowthBeforeReadingContent(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ref, err := store.PutArtifact([]byte("small"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.artifactPath(ref.SHA256, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, bytes.Repeat([]byte{'x'}, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadArtifact(ref); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadArtifact grown file error = %v, want ErrCorrupt", err)
	}
}

func TestAppendJSONLConcurrentAndIdempotent(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	secondStore, err := Open(store.Root())
	if err != nil {
		t.Fatalf("open second Store: %v", err)
	}
	stores := []*Store{store, secondStore}
	const count = 64
	start := make(chan struct{})
	results := make(chan Envelope, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			envelope, err := stores[index%len(stores)].AppendJSONL("runs/run-1/events", Event{
				ID:     fmt.Sprintf("event-%03d", index),
				Schema: "argus.test_event.v1",
				Payload: map[string]any{
					"index": index,
				},
			})
			if err != nil {
				errs <- err
				return
			}
			results <- envelope
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("AppendJSONL: %v", err)
	}

	assigned := make(map[uint64]struct{}, count)
	for result := range results {
		assigned[result.Sequence] = struct{}{}
	}
	if len(assigned) != count {
		t.Fatalf("assigned %d unique sequences, want %d", len(assigned), count)
	}
	envelopes, err := store.ReadJSONL("runs/run-1/events")
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if len(envelopes) != count {
		t.Fatalf("ReadJSONL returned %d events, want %d", len(envelopes), count)
	}
	for index, envelope := range envelopes {
		if envelope.Sequence != uint64(index+1) {
			t.Fatalf("sequence[%d] = %d", index, envelope.Sequence)
		}
	}

	retry := Event{
		ID:      envelopes[0].ID,
		Schema:  envelopes[0].Schema,
		Payload: json.RawMessage(envelopes[0].Payload),
	}
	retried, err := store.AppendJSONL("runs/run-1/events", retry)
	if err != nil {
		t.Fatalf("idempotent AppendJSONL: %v", err)
	}
	if retried.Sequence != envelopes[0].Sequence {
		t.Fatalf("retry sequence = %d, want %d", retried.Sequence, envelopes[0].Sequence)
	}
	retry.Payload = map[string]string{"different": "payload"}
	if _, err := store.AppendJSONL("runs/run-1/events", retry); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("conflicting retry error = %v, want ErrEventConflict", err)
	}
}

func TestAppendJSONLAtSequenceFencesConcurrentTransition(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	if _, err := store.AppendJSONLAtSequence("state/object-1", 0, Event{
		ID: "created", Schema: "argus.test.v1", Payload: map[string]string{"state": "active"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendJSONLAtSequence("state/object-1", 0, Event{
		ID: "stale", Schema: "argus.test.v1", Payload: map[string]string{"state": "stale"},
	}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("stale CAS error = %v, want ErrEventConflict", err)
	}
	event := Event{
		ID: "quarantine", Schema: "argus.test.v1",
		Payload: map[string]string{"state": "quarantined"},
	}
	first, err := store.AppendJSONLAtSequence("state/object-1", 1, event)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := store.AppendJSONLAtSequence("state/object-1", 1, event)
	if err != nil {
		t.Fatalf("idempotent CAS retry error = %v", err)
	}
	if !reflect.DeepEqual(retried, first) {
		t.Fatalf("retry = %+v, want %+v", retried, first)
	}
}

func TestAppendJSONLAtSequenceWithStatusDistinguishesOwnerFromExactRetry(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	event := Event{
		ID: "owner", Schema: "argus.test.v1", Time: time.Now().UTC(),
		Payload: map[string]string{"decision": "claimed"},
	}
	first, appended, err := store.AppendJSONLAtSequenceWithStatus("state/owner", 0, event)
	if err != nil || !appended {
		t.Fatalf("first CAS = (%+v, %v, %v), want appended owner", first, appended, err)
	}
	retried, appended, err := store.AppendJSONLAtSequenceWithStatus("state/owner", 0, event)
	if err != nil || appended || !reflect.DeepEqual(retried, first) {
		t.Fatalf("exact retry CAS = (%+v, %v, %v), want same non-owner", retried, appended, err)
	}
}

func TestAppendJSONLAtSequenceCASAcrossStoreInstances(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "store")
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AppendJSONLAtSequence("state/object-cas", 0, Event{
		ID: "created", Schema: "argus.test.v1", Payload: map[string]string{"state": "active"},
	}); err != nil {
		t.Fatal(err)
	}
	const writers = 20
	start := make(chan struct{})
	results := make(chan error, writers)
	var wait sync.WaitGroup
	for index := range writers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			store := first
			if index%2 == 1 {
				store = second
			}
			_, err := store.AppendJSONLAtSequence("state/object-cas", 1, Event{
				ID:      fmt.Sprintf("transition-%d", index),
				Schema:  "argus.test.v1",
				Payload: map[string]int{"writer": index},
			})
			results <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrEventConflict):
			conflicts++
		default:
			t.Errorf("CAS error = %v", err)
		}
	}
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("CAS results successes=%d conflicts=%d", successes, conflicts)
	}
	envelopes, err := first.ReadJSONL("state/object-cas")
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != 2 {
		t.Fatalf("CAS stream length = %d, want 2", len(envelopes))
	}
}

func TestAppendJSONLAtSequenceMismatchDoesNotCreateEmptyStream(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	stream := "state/missing-object"
	if _, err := store.AppendJSONLAtSequence(stream, 1, Event{
		ID: "stale", Schema: "argus.test.v1", Payload: map[string]bool{"stale": true},
	}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("missing stream CAS error = %v", err)
	}
	if _, err := store.ReadJSONL(stream); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched CAS materialized stream: %v", err)
	}
}

func TestReadArtifactRejectsNonCanonicalLogicalURI(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ref, err := store.PutArtifact([]byte("canonical"))
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"?", "#"} {
		forged := ref
		forged.URI += suffix
		if _, err := store.ReadArtifact(forged); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("ReadArtifact(%q) error = %v", forged.URI, err)
		}
	}
}

func TestReadJSONLDetectsCorruptionAndSequenceGap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "sequence gap",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`"sequence":2`), []byte(`"sequence":3`), 1)
			},
		},
		{
			name: "missing newline",
			mutate: func(data []byte) []byte {
				return bytes.TrimSuffix(data, []byte{'\n'})
			},
		},
		{
			name: "unknown field",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`"payload":`), []byte(`"unknown":true,"payload":`), 1)
			},
		},
		{
			name: "duplicate field",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`"id":`), []byte(`"id":"duplicate","id":`), 1)
			},
		},
		{
			name: "non-UTC event time",
			mutate: func(data []byte) []byte {
				return bytes.Replace(
					data,
					[]byte(`"time":"2026-07-26T01:01:00Z"`),
					[]byte(`"time":"2026-07-26T09:01:00+08:00"`),
					1,
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			for index := 1; index <= 2; index++ {
				_, err := store.AppendJSONL("runs/run-1/events", Event{
					ID:      fmt.Sprintf("event-%d", index),
					Schema:  "argus.test.v1",
					Time:    time.Date(2026, 7, 26, 1, index, 0, 0, time.UTC),
					Payload: map[string]int{"index": index},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			target, err := store.streamPath("runs/run-1/events", false)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			mutated := test.mutate(data)
			if bytes.Equal(mutated, data) {
				t.Fatal("corruption mutation did not change stream bytes")
			}
			if err := os.WriteFile(target, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReadJSONL("runs/run-1/events"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ReadJSONL error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestReadJSONLRejectsOversizedLine(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	stream := "runs/run-oversized/events"
	target, err := store.streamPath(stream, true)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte{'x'}, maxEventBytes+1)
	line = append(line, '\n')
	if err := os.WriteFile(target, line, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadJSONL(stream); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadJSONL oversized line error = %v, want ErrCorrupt", err)
	}
}

func TestImmutableJSONRejectsOverwriteAndStrictlyDecodes(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	type metadata struct {
		RunID    string `json:"run_id"`
		Revision int    `json:"revision"`
	}
	want := metadata{RunID: "run-1", Revision: 1}
	if err := store.PutJSON("runs/run-1/metadata", want); err != nil {
		t.Fatalf("PutJSON: %v", err)
	}
	if err := store.PutJSON("runs/run-1/metadata", want); !errors.Is(err, ErrImmutableExists) {
		t.Fatalf("second PutJSON error = %v, want ErrImmutableExists", err)
	}
	var got metadata
	if err := store.GetJSON("runs/run-1/metadata", &got); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if got != want {
		t.Fatalf("GetJSON = %#v, want %#v", got, want)
	}

	target, err := store.immutablePath("runs/run-1/metadata", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"run_id":"run-1","revision":1,"unknown":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.GetJSON("runs/run-1/metadata", &got); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("strict GetJSON error = %v, want ErrCorrupt", err)
	}
}

func TestStoreRejectsTraversalAndSymlinkEscape(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	for _, id := range []string{
		"../escape",
		"/absolute",
		"runs/../../escape",
		`runs\escape`,
		"C:/escape",
		"runs//escape",
	} {
		if _, err := store.AppendJSONL(id, Event{
			ID: "event-1", Schema: "argus.test.v1", Payload: nil,
		}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("AppendJSONL(%q) error = %v, want ErrInvalidID", id, err)
		}
		if err := store.PutJSON(id, map[string]bool{"unsafe": true}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("PutJSON(%q) error = %v, want ErrInvalidID", id, err)
		}
	}

	outside := t.TempDir()
	link := filepath.Join(store.Root(), "streams", "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendJSONL("linked/events", Event{
		ID: "event-1", Schema: "argus.test.v1", Payload: nil,
	}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink AppendJSONL error = %v, want ErrUnsafePath", err)
	}
	if entries, err := os.ReadDir(outside); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("symlink escape wrote %d outside entries", len(entries))
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}
