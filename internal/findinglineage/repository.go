package findinglineage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const ledgerStream = "finding-lineage/ledger"

type ledgerEvent struct {
	SchemaVersion string               `json:"schema_version"`
	Request       BuildRequest         `json:"request"`
	LineageRef    runmodel.ArtifactRef `json:"lineage_ref"`
	LineageID     string               `json:"lineage_id"`
	RecordedAt    time.Time            `json:"recorded_at"`
}

type Repository struct {
	store    *local.Store
	source   runSource
	ancestry ancestrySource
	now      func() time.Time
	mu       *sync.Mutex
}

var repositoryLocks sync.Map

func New(store *local.Store, source runSource, now func() time.Time, ancestrySources ...ancestrySource) (*Repository, error) {
	if store == nil || source == nil {
		return nil, fmt.Errorf("local store and run source are required")
	}
	if len(ancestrySources) > 1 {
		return nil, fmt.Errorf("at most one Git ancestry source is supported")
	}
	var ancestry ancestrySource
	if len(ancestrySources) == 1 {
		if ancestrySources[0] == nil {
			return nil, fmt.Errorf("Git ancestry source cannot be nil")
		}
		ancestry = ancestrySources[0]
	}
	if now == nil {
		now = time.Now
	}
	value, _ := repositoryLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &Repository{store: store, source: source, ancestry: ancestry, now: now, mu: value.(*sync.Mutex)}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Build(ctx context.Context, request BuildRequest) (Record, error) {
	if ctx == nil {
		return Record{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if reflect.DeepEqual(request.Policy, contractsv1alpha1.FindingLineagePolicy{}) {
		if repository.ancestry != nil {
			request.Policy = GitAwarePolicy()
		} else {
			request.Policy = DefaultPolicy()
		}
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = DefaultIdempotencyKey(request.BaselineRunID, request.VariantRunID, request.Policy)
	}
	if err := request.Validate(); err != nil {
		return Record{}, err
	}
	if request.Policy.AncestryAuthority == "local_git_object_graph" && repository.ancestry == nil {
		return Record{}, fmt.Errorf("Git-aware lineage policy requires a Git ancestry source")
	}
	if request.Policy.AncestryAuthority != "local_git_object_graph" && repository.ancestry != nil {
		return Record{}, fmt.Errorf("configured Git ancestry source requires the Git-aware lineage policy")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	if existing, ok := state.byIdempotency[request.IdempotencyKey]; ok {
		if !exactEqual(existing.Request, request) {
			return Record{}, fmt.Errorf("%w: idempotency key was already used", ErrConflict)
		}
		return existing, nil
	}
	pairKey := lineagePairKey(request)
	if _, exists := state.byPair[pairKey]; exists {
		return Record{}, fmt.Errorf("%w: run pair and policy already have a lineage", ErrConflict)
	}
	recordedAt := repository.now().UTC()
	if recordedAt.IsZero() {
		return Record{}, fmt.Errorf("lineage clock returned zero time")
	}
	lineage, err := buildFindingLineage(ctx, repository.source, repository.ancestry, request, recordedAt)
	if err != nil {
		return Record{}, err
	}
	data, err := json.Marshal(lineage)
	if err != nil {
		return Record{}, err
	}
	ref, err := repository.source.PutArtifact(runmodel.ContractFindingLineage, data)
	if err != nil {
		return Record{}, fmt.Errorf("persist finding lineage: %w", err)
	}
	event := ledgerEvent{SchemaVersion: LedgerEventSchemaVersion, Request: request, LineageRef: ref, LineageID: lineage.LineageID, RecordedAt: recordedAt}
	_, err = repository.store.AppendJSONLAtSequence(ledgerStream, uint64(len(state.events)), local.Event{ID: request.IdempotencyKey, Schema: LedgerEventSchemaVersion, Time: recordedAt, Payload: event})
	if err != nil {
		reloaded, loadErr := repository.load()
		if loadErr == nil {
			if existing, ok := reloaded.byIdempotency[request.IdempotencyKey]; ok && exactEqual(existing.Request, request) {
				return existing, nil
			}
		}
		return Record{}, fmt.Errorf("append finding lineage: %w", err)
	}
	return Record{Request: request, LineageRef: ref, Lineage: lineage, RecordedAt: recordedAt}, nil
}

func (repository *Repository) Get(lineageID string) (Record, error) {
	if err := validateID("lineage_id", lineageID); err != nil {
		return Record{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	record, ok := state.byID[lineageID]
	if !ok {
		return Record{}, ErrNotFound
	}
	return record, nil
}

func (repository *Repository) List(filter ListFilter) ([]Record, error) {
	if filter.RunID != "" {
		if err := validateID("run_id", filter.RunID); err != nil {
			return nil, err
		}
	}
	if filter.RepositoryID != "" && len(filter.RepositoryID) > 1024 {
		return nil, fmt.Errorf("repository_id is too long")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]Record, 0)
	for _, record := range state.byID {
		if filter.RunID != "" && record.Lineage.Baseline.RunID != filter.RunID && record.Lineage.Variant.RunID != filter.RunID {
			continue
		}
		if filter.RepositoryID != "" && record.Lineage.Baseline.Repository.RepositoryID != filter.RepositoryID {
			continue
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].RecordedAt.Equal(result[j].RecordedAt) {
			return result[i].RecordedAt.After(result[j].RecordedAt)
		}
		return result[i].Lineage.LineageID < result[j].Lineage.LineageID
	})
	return result, nil
}

type ledgerState struct {
	events        []ledgerEvent
	byIdempotency map[string]Record
	byID          map[string]Record
	byPair        map[string]string
}

func (repository *Repository) load() (ledgerState, error) {
	state := ledgerState{events: []ledgerEvent{}, byIdempotency: map[string]Record{}, byID: map[string]Record{}, byPair: map[string]string{}}
	envelopes, err := repository.store.ReadJSONL(ledgerStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return ledgerState{}, fmt.Errorf("%w: read ledger: %v", ErrCorrupt, err)
	}
	for _, envelope := range envelopes {
		if envelope.Schema != LedgerEventSchemaVersion {
			return ledgerState{}, fmt.Errorf("%w: unsupported event schema", ErrCorrupt)
		}
		var event ledgerEvent
		if err := decodeStrict(envelope.Payload, &event); err != nil {
			return ledgerState{}, fmt.Errorf("%w: decode sequence %d: %v", ErrCorrupt, envelope.Sequence, err)
		}
		if err := event.Request.Validate(); err != nil {
			return ledgerState{}, fmt.Errorf("%w: sequence %d request: %v", ErrCorrupt, envelope.Sequence, err)
		}
		if event.SchemaVersion != LedgerEventSchemaVersion {
			return ledgerState{}, fmt.Errorf("%w: sequence %d payload schema is unsupported", ErrCorrupt, envelope.Sequence)
		}
		if event.LineageRef.Contract != runmodel.ContractFindingLineage {
			return ledgerState{}, fmt.Errorf("%w: sequence %d lineage contract is invalid", ErrCorrupt, envelope.Sequence)
		}
		if err := event.LineageRef.Validate(); err != nil {
			return ledgerState{}, fmt.Errorf("%w: sequence %d lineage ref: %v", ErrCorrupt, envelope.Sequence, err)
		}
		if event.RecordedAt.IsZero() || event.RecordedAt.Location() != time.UTC ||
			envelope.ID != event.Request.IdempotencyKey || !envelope.Time.Equal(event.RecordedAt) {
			return ledgerState{}, fmt.Errorf("%w: sequence %d envelope does not bind payload", ErrCorrupt, envelope.Sequence)
		}
		data, err := repository.source.ReadArtifact(event.LineageRef)
		if err != nil {
			return ledgerState{}, fmt.Errorf("%w: read lineage artifact: %v", ErrCorrupt, err)
		}
		lineage, err := contractsv1alpha1.DecodeFindingLineage(data)
		if err != nil || lineage.LineageID != event.LineageID || !lineage.GeneratedAt.Equal(event.RecordedAt) || lineage.Baseline.RunID != event.Request.BaselineRunID || lineage.Variant.RunID != event.Request.VariantRunID || !reflect.DeepEqual(lineage.Policy, event.Request.Policy) {
			return ledgerState{}, fmt.Errorf("%w: lineage artifact does not bind sequence %d", ErrCorrupt, envelope.Sequence)
		}
		record := Record{Request: event.Request, LineageRef: event.LineageRef, Lineage: lineage, RecordedAt: event.RecordedAt}
		if _, exists := state.byIdempotency[event.Request.IdempotencyKey]; exists {
			return ledgerState{}, fmt.Errorf("%w: duplicate idempotency key", ErrCorrupt)
		}
		if _, exists := state.byID[event.LineageID]; exists {
			return ledgerState{}, fmt.Errorf("%w: duplicate lineage id", ErrCorrupt)
		}
		pairKey := lineagePairKey(event.Request)
		if _, exists := state.byPair[pairKey]; exists {
			return ledgerState{}, fmt.Errorf("%w: duplicate run pair and policy", ErrCorrupt)
		}
		state.events = append(state.events, event)
		state.byIdempotency[event.Request.IdempotencyKey] = record
		state.byID[event.LineageID] = record
		state.byPair[pairKey] = event.LineageID
	}
	return state, nil
}

func lineagePairKey(request BuildRequest) string {
	return request.BaselineRunID + "\x00" + request.VariantRunID + "\x00" + request.Policy.SHA256
}

func exactEqual(left, right any) bool {
	a, ae := json.Marshal(left)
	b, be := json.Marshal(right)
	return ae == nil && be == nil && bytes.Equal(a, b)
}
