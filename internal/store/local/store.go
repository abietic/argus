// Package local provides the append-only local persistence used by the first
// Argus vertical slice. It deliberately exposes domain-neutral records: callers
// remain responsible for mapping these records to Argus domain contracts.
package local

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	artifactURIPrefix = "artifact://local/sha256/"
	maxEventBytes     = 16 << 20
)

var (
	ErrInvalidID       = errors.New("invalid local store id")
	ErrUnsafePath      = errors.New("unsafe local store path")
	ErrCorrupt         = errors.New("corrupt local store data")
	ErrImmutableExists = errors.New("immutable object already exists")
	ErrEventConflict   = errors.New("event id already exists with different content")
)

// ArtifactRef is a logical, content-addressed local artifact reference. URI is
// never fetched directly; ReadArtifact resolves it below the Store root and
// revalidates both SHA-256 and size.
type ArtifactRef struct {
	URI       string `json:"uri"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Event is the caller-supplied portion of an append-only event. ID must be
// stable across retries. A zero Time is replaced with the current UTC time.
type Event struct {
	ID      string
	Schema  string
	Time    time.Time
	Payload any
}

// Envelope is the durable JSONL representation. Sequence starts at one and is
// contiguous within a stream.
type Envelope struct {
	ID       string          `json:"id"`
	Sequence uint64          `json:"sequence"`
	Time     time.Time       `json:"time"`
	Schema   string          `json:"schema"`
	Payload  json.RawMessage `json:"payload"`
}

// Store persists data beneath a canonical root. Open is safe to call multiple
// times for the same root; stream and object locks coordinate Store instances
// and cooperating processes.
type Store struct {
	root string
	now  func() time.Time
}

// New is an alias for Open.
func New(root string) (*Store, error) {
	return Open(root)
}

// Open creates (if necessary) and opens a local store rooted at root.
func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: root is empty", ErrUnsafePath)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve store root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create store root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve store root symlinks: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("stat store root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: root is not a directory", ErrUnsafePath)
	}

	store := &Store{root: filepath.Clean(canonical), now: time.Now}
	for _, relative := range []string{
		"artifacts/sha256",
		"immutable",
		"streams",
		".locks/artifacts",
		".locks/immutable",
		".locks/streams",
	} {
		if _, err := store.ensureDirectory(relative, true); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// Root returns the canonical store root.
func (store *Store) Root() string {
	return store.root
}

// PutArtifact persists content once under its SHA-256 address. Repeated writes
// of identical content return the same reference without replacing the file.
func (store *Store) PutArtifact(content []byte) (ArtifactRef, error) {
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	ref := ArtifactRef{
		URI:       artifactURIPrefix + digest,
		SHA256:    digest,
		SizeBytes: int64(len(content)),
	}
	target, err := store.artifactPath(digest, true)
	if err != nil {
		return ArtifactRef{}, err
	}
	lockPath, err := store.fixedLockPath("artifacts/content-addressed.lock")
	if err != nil {
		return ArtifactRef{}, err
	}
	lock, err := acquireFileLock(lockPath, false)
	if err != nil {
		return ArtifactRef{}, err
	}
	defer releaseFileLock(lock)

	if _, err := os.Lstat(target); err == nil {
		if _, verifyErr := store.readAndVerifyArtifact(target, ref); verifyErr != nil {
			return ArtifactRef{}, verifyErr
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ArtifactRef{}, fmt.Errorf("inspect artifact target: %w", err)
	}
	if err := store.atomicCreate(target, content, 0o444); err != nil {
		return ArtifactRef{}, fmt.Errorf("persist artifact: %w", err)
	}
	return ref, nil
}

// ReadArtifact resolves a local logical reference and verifies its path,
// digest, and declared size before returning bytes.
func (store *Store) ReadArtifact(ref ArtifactRef) ([]byte, error) {
	digest, err := validateArtifactRef(ref)
	if err != nil {
		return nil, err
	}
	target, err := store.artifactPath(digest, false)
	if err != nil {
		return nil, err
	}
	return store.readAndVerifyArtifact(target, ref)
}

func (store *Store) readAndVerifyArtifact(target string, ref ArtifactRef) ([]byte, error) {
	file, err := openRegularNoFollow(target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat artifact: %w", err)
	}
	if info.Size() != ref.SizeBytes {
		return nil, fmt.Errorf(
			"%w: artifact size is %d, want %d",
			ErrCorrupt,
			info.Size(),
			ref.SizeBytes,
		)
	}
	// Bound the read to the immutable reference's declared size even if a
	// non-cooperating writer grows the file after the stat above.
	content, err := io.ReadAll(io.LimitReader(file, ref.SizeBytes))
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if int64(len(content)) != ref.SizeBytes {
		return nil, fmt.Errorf("%w: artifact size is %d, want %d", ErrCorrupt, len(content), ref.SizeBytes)
	}
	var trailing [1]byte
	if read, readErr := file.Read(trailing[:]); readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, fmt.Errorf("verify artifact end: %w", readErr)
	} else if read != 0 {
		return nil, fmt.Errorf("%w: artifact grew while reading", ErrCorrupt)
	}
	sum := sha256.Sum256(content)
	actual := hex.EncodeToString(sum[:])
	if actual != ref.SHA256 {
		return nil, fmt.Errorf("%w: artifact digest is %s, want %s", ErrCorrupt, actual, ref.SHA256)
	}
	return content, nil
}

func validateArtifactRef(ref ArtifactRef) (string, error) {
	if !isLowerSHA256(ref.SHA256) {
		return "", fmt.Errorf("%w: invalid artifact SHA-256", ErrCorrupt)
	}
	if ref.SizeBytes < 0 {
		return "", fmt.Errorf("%w: negative artifact size", ErrCorrupt)
	}
	if ref.URI != artifactURIPrefix+ref.SHA256 {
		return "", fmt.Errorf("%w: artifact URI is not canonical", ErrUnsafePath)
	}
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return "", fmt.Errorf("%w: parse artifact URI: %v", ErrUnsafePath, err)
	}
	if parsed.Scheme != "artifact" || parsed.Host != "local" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", fmt.Errorf("%w: artifact URI is not a local logical reference", ErrUnsafePath)
	}
	wantPath := "/sha256/" + ref.SHA256
	if parsed.EscapedPath() != wantPath || parsed.Path != wantPath {
		return "", fmt.Errorf("%w: artifact URI does not match digest", ErrUnsafePath)
	}
	return ref.SHA256, nil
}

func isLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

// AppendJSONL atomically appends an event to stream and fsyncs it before
// returning. Retrying the same ID/schema/payload is idempotent; reusing an ID
// for different content returns ErrEventConflict.
func (store *Store) AppendJSONL(stream string, event Event) (Envelope, error) {
	return store.appendJSONL(stream, event, nil)
}

// AppendJSONLAtSequence appends only when the stream still contains exactly
// expectedSequence events. Idempotent retries of an existing event ID are
// resolved before the compare-and-swap check.
func (store *Store) AppendJSONLAtSequence(
	stream string,
	expectedSequence uint64,
	event Event,
) (Envelope, error) {
	return store.appendJSONL(stream, event, &expectedSequence)
}

// AppendJSONLAtSequenceWithStatus is the ownership-bearing CAS variant.
// appended is true only for the caller that physically advanced the stream;
// an exact idempotent retry returns the existing envelope with appended=false.
func (store *Store) AppendJSONLAtSequenceWithStatus(
	stream string,
	expectedSequence uint64,
	event Event,
) (envelope Envelope, appended bool, err error) {
	return store.appendJSONLWithStatus(stream, event, &expectedSequence)
}

func (store *Store) appendJSONL(
	stream string,
	event Event,
	expectedSequence *uint64,
) (Envelope, error) {
	envelope, _, err := store.appendJSONLWithStatus(stream, event, expectedSequence)
	return envelope, err
}

func (store *Store) appendJSONLWithStatus(
	stream string,
	event Event,
	expectedSequence *uint64,
) (Envelope, bool, error) {
	if err := validateRelativeID(stream); err != nil {
		return Envelope{}, false, err
	}
	if err := validateLabel("event id", event.ID); err != nil {
		return Envelope{}, false, err
	}
	if err := validateLabel("event schema", event.Schema); err != nil {
		return Envelope{}, false, err
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return Envelope{}, false, fmt.Errorf("marshal event payload: %w", err)
	}
	if err := rejectDuplicateJSONFields(payload); err != nil {
		return Envelope{}, false, fmt.Errorf("validate event payload: %w", err)
	}
	if len(payload) > maxEventBytes {
		return Envelope{}, false, fmt.Errorf("event payload exceeds %d bytes", maxEventBytes)
	}

	target, err := store.streamPath(stream, true)
	if err != nil {
		return Envelope{}, false, err
	}
	lockPath, err := store.lockPath("streams", stream)
	if err != nil {
		return Envelope{}, false, err
	}
	lock, err := acquireFileLock(lockPath, false)
	if err != nil {
		return Envelope{}, false, err
	}
	defer releaseFileLock(lock)

	created := false
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		created = true
	} else if err != nil {
		return Envelope{}, false, fmt.Errorf("inspect stream: %w", err)
	}
	if created && expectedSequence != nil && *expectedSequence != 0 {
		return Envelope{}, false, fmt.Errorf(
			"%w: stream sequence is 0, want %d",
			ErrEventConflict,
			*expectedSequence,
		)
	}
	file, err := openAppendNoFollow(target)
	if err != nil {
		return Envelope{}, false, err
	}
	defer file.Close()

	if _, err := file.Seek(0, 0); err != nil {
		return Envelope{}, false, fmt.Errorf("seek stream: %w", err)
	}
	existing, err := readEnvelopes(file)
	if err != nil {
		return Envelope{}, false, err
	}
	for _, envelope := range existing {
		if envelope.ID != event.ID {
			continue
		}
		sameTime := event.Time.IsZero() || envelope.Time.Equal(event.Time)
		if envelope.Schema == event.Schema && sameTime &&
			bytes.Equal(compactJSON(envelope.Payload), compactJSON(payload)) {
			return envelope, false, nil
		}
		return Envelope{}, false, fmt.Errorf("%w: %q", ErrEventConflict, event.ID)
	}
	if expectedSequence != nil && uint64(len(existing)) != *expectedSequence {
		return Envelope{}, false, fmt.Errorf(
			"%w: stream sequence is %d, want %d",
			ErrEventConflict,
			len(existing),
			*expectedSequence,
		)
	}

	eventTime := event.Time
	if eventTime.IsZero() {
		eventTime = store.now()
	}
	eventTime = eventTime.UTC()
	if eventTime.IsZero() {
		return Envelope{}, false, fmt.Errorf("event time must not be zero")
	}
	envelope := Envelope{
		ID:       event.ID,
		Sequence: uint64(len(existing)) + 1,
		Time:     eventTime,
		Schema:   event.Schema,
		Payload:  payload,
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		return Envelope{}, false, fmt.Errorf("marshal event envelope: %w", err)
	}
	line = append(line, '\n')
	if len(line) > maxEventBytes {
		return Envelope{}, false, fmt.Errorf("event envelope exceeds %d bytes", maxEventBytes)
	}
	if _, err := file.Seek(0, 2); err != nil {
		return Envelope{}, false, fmt.Errorf("seek stream end: %w", err)
	}
	if err := writeAll(file, line); err != nil {
		return Envelope{}, false, fmt.Errorf("append stream: %w", err)
	}
	if err := file.Sync(); err != nil {
		return Envelope{}, false, fmt.Errorf("sync stream: %w", err)
	}
	if created {
		if err := syncDirectory(filepath.Dir(target)); err != nil {
			return Envelope{}, false, fmt.Errorf("sync stream directory: %w", err)
		}
	}
	return envelope, true, nil
}

// ReadJSONL reads a complete stream while holding its shared lock. It rejects
// malformed envelopes, missing final newlines, duplicate IDs, and sequence gaps.
func (store *Store) ReadJSONL(stream string) ([]Envelope, error) {
	target, err := store.streamPath(stream, false)
	if err != nil {
		return nil, err
	}
	lockPath, err := store.lockPath("streams", stream)
	if err != nil {
		return nil, err
	}
	lock, err := acquireFileLock(lockPath, true)
	if err != nil {
		return nil, err
	}
	defer releaseFileLock(lock)

	file, err := openRegularNoFollow(target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readEnvelopes(file)
}

// PutJSON creates an immutable JSON object. Existing IDs are never overwritten,
// even when the new bytes are identical.
func (store *Store) PutJSON(id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal immutable JSON: %w", err)
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return fmt.Errorf("validate immutable JSON: %w", err)
	}
	data = append(data, '\n')
	target, err := store.immutablePath(id, true)
	if err != nil {
		return err
	}
	lockPath, err := store.lockPath("immutable", id)
	if err != nil {
		return err
	}
	lock, err := acquireFileLock(lockPath, false)
	if err != nil {
		return err
	}
	defer releaseFileLock(lock)

	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("%w: %q", ErrImmutableExists, id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect immutable JSON: %w", err)
	}
	if err := store.atomicCreate(target, data, 0o444); err != nil {
		if errors.Is(err, ErrImmutableExists) {
			return fmt.Errorf("%w: %q", ErrImmutableExists, id)
		}
		return fmt.Errorf("persist immutable JSON: %w", err)
	}
	return nil
}

// GetJSON strictly decodes an immutable JSON object into out. Unknown fields,
// duplicate fields, and trailing values are rejected.
func (store *Store) GetJSON(id string, out any) error {
	if out == nil {
		return fmt.Errorf("immutable JSON output must not be nil")
	}
	target, err := store.immutablePath(id, false)
	if err != nil {
		return err
	}
	file, err := openRegularNoFollow(target)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read immutable JSON: %w", err)
	}
	if err := decodeStrictJSON(data, out); err != nil {
		return fmt.Errorf("%w: immutable JSON %q: %v", ErrCorrupt, id, err)
	}
	return nil
}
