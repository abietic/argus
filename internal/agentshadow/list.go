package agentshadow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

const (
	committedIndexSchemaVersion = "argus.agent_review_committed_index.v1alpha1"
	committedIndexRoot          = "agent-review-shadow/committed-index/"
	maxCommittedRecords         = 100_000
)

// ListRequest selects committed shadow imports by the Go host observation
// time. The window is half-open: StartInclusive <= RecordedAt < EndExclusive.
type ListRequest struct {
	Scope          Scope
	StartInclusive time.Time
	EndExclusive   time.Time
}

type committedIndexEntry struct {
	SchemaVersion  string    `json:"schema_version"`
	TenantID       string    `json:"tenant_id"`
	WorkspaceID    string    `json:"workspace_id"`
	ManifestID     string    `json:"manifest_id"`
	ObservationID  string    `json:"observation_id"`
	InputDigest    string    `json:"input_digest"`
	AcceptedAt     time.Time `json:"accepted_at"`
	ImportObjectID string    `json:"import_object_id"`
}

// List returns only committed imports. Index entries are discovery hints, not
// trust anchors: every committed item is loaded through Query so its complete
// artifact, frozen-input, manifest, receipt, and observation closure is
// revalidated before the host observation window is applied.
func (service *Service) List(
	ctx context.Context,
	request ListRequest,
) ([]Result, error) {
	if service == nil || service.repository == nil {
		return nil, fmt.Errorf("agent review shadow service is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	entries, err := service.repository.listCommitted(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(entries))
	for _, entry := range entries {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		result, queryErr := service.Query(ctx, QueryRequest{
			Scope:      request.Scope,
			ManifestID: entry.ManifestID,
		})
		if queryErr != nil {
			return nil, fmt.Errorf(
				"revalidate committed shadow import %q: %w",
				entry.ManifestID,
				queryErr,
			)
		}
		if !result.Observation.RecordedAt.Equal(entry.AcceptedAt) {
			return nil, fmt.Errorf(
				"%w: committed index host observation time changed",
				ErrCorrupt,
			)
		}
		if result.Observation.RecordedAt.Before(request.StartInclusive) ||
			!result.Observation.RecordedAt.Before(request.EndExclusive) {
			continue
		}
		results = append(results, result)
	}
	slices.SortFunc(results, func(left, right Result) int {
		if compared := left.Observation.RecordedAt.Compare(
			right.Observation.RecordedAt,
		); compared != 0 {
			return compared
		}
		return strings.Compare(left.Manifest.ManifestID, right.Manifest.ManifestID)
	})
	return results, nil
}

func (request ListRequest) Validate() error {
	if err := validateScope(request.Scope); err != nil {
		return err
	}
	for name, value := range map[string]time.Time{
		"start_inclusive": request.StartInclusive,
		"end_exclusive":   request.EndExclusive,
	} {
		if value.IsZero() {
			return fmt.Errorf("%s is required", name)
		}
		_, offset := value.Zone()
		if offset != 0 {
			return fmt.Errorf("%s must use UTC", name)
		}
	}
	if !request.StartInclusive.Before(request.EndExclusive) {
		return fmt.Errorf("committed import window must be non-empty and increasing")
	}
	return nil
}

func (repository *Repository) ensureCommittedIndex(record ImportRecord) error {
	entry := committedIndexEntryFromRecord(record)
	if err := validateCommittedIndexEntry(entry); err != nil {
		return err
	}
	objectID := committedIndexObjectID(record.TenantID, record.WorkspaceID, record.ManifestID)
	if err := repository.store.PutJSON(objectID, entry); err == nil {
		return nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return fmt.Errorf("persist committed shadow index: %w", err)
	}
	var existing committedIndexEntry
	if err := repository.store.GetJSON(objectID, &existing); err != nil {
		return fmt.Errorf("%w: load committed shadow index: %v", ErrCorrupt, err)
	}
	if err := validateCommittedIndexEntry(existing); err != nil {
		return fmt.Errorf("%w: validate committed shadow index: %v", ErrCorrupt, err)
	}
	if !reflect.DeepEqual(existing, entry) {
		return fmt.Errorf("%w: committed shadow index binds different state", ErrConflict)
	}
	return nil
}

func (repository *Repository) listCommitted(
	ctx context.Context,
	scope Scope,
) ([]committedIndexEntry, error) {
	if err := repository.repairCommittedIndex(ctx); err != nil {
		return nil, err
	}
	directory := filepath.Join(
		repository.store.Root(),
		"immutable",
		filepath.FromSlash(strings.TrimSuffix(committedIndexRoot, "/")),
		digestStrings(scope.TenantID, scope.WorkspaceID),
	)
	entries, err := readStrictObjectDirectory(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []committedIndexEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read committed shadow index: %v", ErrCorrupt, err)
	}
	if len(entries) > maxCommittedRecords {
		return nil, fmt.Errorf("committed shadow index exceeds %d records", maxCommittedRecords)
	}
	result := make([]committedIndexEntry, 0, len(entries))
	for _, name := range entries {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		digest := strings.TrimSuffix(name, ".json")
		var entry committedIndexEntry
		objectID := committedIndexRoot +
			digestStrings(scope.TenantID, scope.WorkspaceID) + "/" + digest
		if err := repository.store.GetJSON(objectID, &entry); err != nil {
			return nil, fmt.Errorf("%w: load committed index entry: %v", ErrCorrupt, err)
		}
		if err := validateCommittedIndexEntry(entry); err != nil {
			return nil, fmt.Errorf("%w: committed index entry: %v", ErrCorrupt, err)
		}
		if entry.TenantID != scope.TenantID || entry.WorkspaceID != scope.WorkspaceID ||
			committedIndexObjectID(entry.TenantID, entry.WorkspaceID, entry.ManifestID) != objectID {
			return nil, fmt.Errorf("%w: committed index path does not bind its entry", ErrCorrupt)
		}
		record, err := repository.loadRecord(entry.ManifestID)
		if err != nil {
			return nil, fmt.Errorf("%w: committed index has no valid import: %v", ErrCorrupt, err)
		}
		if !reflect.DeepEqual(entry, committedIndexEntryFromRecord(record)) {
			return nil, fmt.Errorf("%w: committed index does not match import record", ErrCorrupt)
		}
		result = append(result, entry)
	}
	slices.SortFunc(result, func(left, right committedIndexEntry) int {
		if compared := left.AcceptedAt.Compare(right.AcceptedAt); compared != 0 {
			return compared
		}
		return strings.Compare(left.ManifestID, right.ManifestID)
	})
	return result, nil
}

// repairCommittedIndex derives missing index entries only from immutable
// ImportRecord commit points. It never indexes intents, orphan artifacts, or
// observation streams. Repeated repair is content-idempotent.
func (repository *Repository) repairCommittedIndex(ctx context.Context) error {
	directory := filepath.Join(
		repository.store.Root(),
		"immutable",
		filepath.FromSlash(strings.TrimSuffix(importRoot, "/")),
	)
	entries, err := readStrictObjectDirectory(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: enumerate committed shadow imports: %v", ErrCorrupt, err)
	}
	if len(entries) > maxCommittedRecords {
		return fmt.Errorf("committed shadow imports exceed %d records", maxCommittedRecords)
	}
	for _, name := range entries {
		if err := contextError(ctx); err != nil {
			return err
		}
		digest := strings.TrimSuffix(name, ".json")
		var record ImportRecord
		if err := repository.store.GetJSON(importRoot+digest, &record); err != nil {
			return fmt.Errorf("%w: load committed shadow import: %v", ErrCorrupt, err)
		}
		if err := validateImportRecord(record); err != nil {
			return fmt.Errorf("%w: validate committed shadow import: %v", ErrCorrupt, err)
		}
		if importObjectID(record.ManifestID) != importRoot+digest {
			return fmt.Errorf("%w: committed import path does not bind manifest", ErrCorrupt)
		}
		if err := repository.ensureCommittedIndex(record); err != nil {
			return err
		}
	}
	return nil
}

func committedIndexEntryFromRecord(record ImportRecord) committedIndexEntry {
	return committedIndexEntry{
		SchemaVersion:  committedIndexSchemaVersion,
		TenantID:       record.TenantID,
		WorkspaceID:    record.WorkspaceID,
		ManifestID:     record.ManifestID,
		ObservationID:  record.ObservationID,
		InputDigest:    record.InputDigest,
		AcceptedAt:     record.AcceptedAt,
		ImportObjectID: importObjectID(record.ManifestID),
	}
}

func validateCommittedIndexEntry(entry committedIndexEntry) error {
	if entry.SchemaVersion != committedIndexSchemaVersion {
		return fmt.Errorf("unsupported committed index schema %q", entry.SchemaVersion)
	}
	if err := validateScope(Scope{
		TenantID: entry.TenantID, WorkspaceID: entry.WorkspaceID,
	}); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"manifest_id": entry.ManifestID, "observation_id": entry.ObservationID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if !lowerSHA256(entry.InputDigest) {
		return fmt.Errorf("input_digest must be a lowercase SHA-256")
	}
	if entry.AcceptedAt.IsZero() {
		return fmt.Errorf("accepted_at is required")
	}
	_, offset := entry.AcceptedAt.Zone()
	if offset != 0 {
		return fmt.Errorf("accepted_at must use UTC")
	}
	if entry.ImportObjectID != importObjectID(entry.ManifestID) {
		return fmt.Errorf("import_object_id does not bind manifest_id")
	}
	return nil
}

func committedIndexObjectID(tenantID, workspaceID, manifestID string) string {
	return committedIndexRoot + digestStrings(tenantID, workspaceID) + "/" +
		digestStrings(manifestID)
}

func readStrictObjectDirectory(directory string) ([]string, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("object directory is not a real directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		storeTemporary, temporaryErr := isStoreAtomicTemporaryEntry(
			directory,
			info.Mode(),
			entry,
		)
		if temporaryErr != nil {
			return nil, temporaryErr
		}
		if storeTemporary {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() ||
			filepath.Ext(name) != ".json" ||
			!lowerSHA256(strings.TrimSuffix(name, ".json")) {
			return nil, fmt.Errorf("unexpected object directory entry %q", name)
		}
		result = append(result, name)
	}
	slices.Sort(result)
	return result, nil
}

// isStoreAtomicTemporaryEntry recognizes only the intermediate names and file
// modes produced by local.Store.atomicCreate. os.CreateTemp currently replaces
// the wildcard in ".argus-tmp-*" with the canonical decimal representation of
// a uint32. The file is 0600 while it is being written and 0444 after Chmod,
// immediately before rename. Similar names, symlinks, directories, and files
// with any other permissions remain corruption instead of being hidden.
//
// A matching entry may disappear between ReadDir and Lstat when its producer
// commits the rename; that is the normal concurrent-write case and is safe to
// ignore because immutable target discovery happens on a later List call.
func isStoreAtomicTemporaryEntry(
	directory string,
	directoryMode os.FileMode,
	entry os.DirEntry,
) (bool, error) {
	if !isStoreAtomicTemporaryName(entry.Name()) {
		return false, nil
	}
	// Store directories are created as 0700. If a pre-existing directory is
	// writable by another group or user, a matching name is not sufficient
	// provenance and must remain visible as corruption.
	if directoryMode.Perm()&0o022 != 0 {
		return false, nil
	}
	info, err := os.Lstat(filepath.Join(directory, entry.Name()))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect store atomic temporary entry %q: %w", entry.Name(), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil
	}
	permissions := info.Mode().Perm()
	return permissions == 0o600 || permissions == 0o444, nil
}

func isStoreAtomicTemporaryName(name string) bool {
	const prefix = ".argus-tmp-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == "" || len(suffix) > 10 ||
		(len(suffix) > 1 && suffix[0] == '0') {
		return false
	}
	value, err := strconv.ParseUint(suffix, 10, 32)
	return err == nil && strconv.FormatUint(value, 10) == suffix
}
