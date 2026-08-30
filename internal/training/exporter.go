package training

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"sync"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

type ManifestSource interface {
	Get(string, evaluation.Access) (Record, error)
}

type Exporter struct {
	store     *local.Store
	manifests ManifestSource
	artifacts ArtifactSource
	mu        *sync.Mutex
}

var exporterLocks sync.Map

func NewExporter(store *local.Store, manifests ManifestSource, artifacts ArtifactSource) (*Exporter, error) {
	if store == nil || manifests == nil || artifacts == nil {
		return nil, fmt.Errorf("training export store, manifest source, and artifact source are required")
	}
	value, _ := exporterLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	exporter := &Exporter{store: store, manifests: manifests, artifacts: artifacts, mu: value.(*sync.Mutex)}
	if _, err := exporter.load(); err != nil {
		return nil, err
	}
	return exporter, nil
}

func (exporter *Exporter) Build(ctx context.Context, request ExportRequest, mutation evaluation.Mutation) (ExportRecord, error) {
	if ctx == nil {
		return ExportRecord{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return ExportRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return ExportRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return ExportRecord{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return ExportRecord{}, fmt.Errorf("request created_at must equal mutation time")
	}
	if !slices.Contains(mutation.Roles, evaluation.RoleDatasetCurator) {
		return ExportRecord{}, fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	state, err := exporter.load()
	if err != nil {
		return ExportRecord{}, err
	}
	if existing, ok := state.byIdempotency[mutation.IdempotencyKey]; ok {
		if !exactEqual(existing.Request, request) || !exactEqual(existing.Mutation, mutation) {
			return ExportRecord{}, fmt.Errorf("%w: idempotency key was already used", ErrExportConflict)
		}
		return existing, nil
	}
	if _, exists := state.byID[request.ExportID]; exists {
		return ExportRecord{}, fmt.Errorf("%w: export_id already exists", ErrExportConflict)
	}
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	manifestRecord, err := exporter.manifests.Get(request.ManifestID, access)
	if err != nil {
		return ExportRecord{}, err
	}
	if !reflect.DeepEqual(manifestRecord.ManifestRef, request.ManifestRef) {
		return ExportRecord{}, fmt.Errorf("%w: manifest ref drifted", ErrContaminated)
	}
	if request.CreatedAt.Before(manifestRecord.Manifest.CreatedAt) {
		return ExportRecord{}, fmt.Errorf("export cannot precede training manifest")
	}
	receipts, redactedBySource, err := exporter.redactManifest(ctx, manifestRecord.Manifest, request.Policy)
	if err != nil {
		return ExportRecord{}, err
	}
	samples := make([]ExportSample, len(manifestRecord.Manifest.Samples))
	for index, sample := range manifestRecord.Manifest.Samples {
		redacted := make([]runmodel.ArtifactRef, len(sample.ArtifactRefs))
		for refIndex, source := range sample.ArtifactRefs {
			redacted[refIndex] = redactedBySource[artifactRefKey(source)]
		}
		samples[index] = ExportSample{GovernedSample: sample, RedactedRefs: redacted}
	}
	bundle, err := sealExportBundle(ExportBundle{ExportID: request.ExportID, DatasetID: manifestRecord.Manifest.DatasetID, DatasetRevision: manifestRecord.Manifest.DatasetRevision, RepositoryID: manifestRecord.Manifest.RepositoryID, ManifestID: manifestRecord.Manifest.ManifestID, ManifestRef: manifestRecord.ManifestRef, Policy: request.Policy, PortableFormat: PortableRecordsFormat, ContainsUnredactedSourceBytes: false, Receipts: receipts, Samples: samples, CreatedBy: mutation.Actor, CreatedAt: request.CreatedAt})
	if err != nil {
		return ExportRecord{}, err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return ExportRecord{}, err
	}
	bundleRef, err := exporter.artifacts.PutArtifact(runmodel.ContractTrainingExportBundle, data)
	if err != nil {
		return ExportRecord{}, fmt.Errorf("persist training export bundle: %w", err)
	}
	event := exportLedgerEvent{SchemaVersion: ExportLedgerSchemaVersion, Request: request, BundleRef: bundleRef, Mutation: mutation}
	_, err = exporter.store.AppendJSONLAtSequence(exportLedgerStream, uint64(len(state.events)), local.Event{ID: mutation.IdempotencyKey, Schema: ExportLedgerSchemaVersion, Time: mutation.At.UTC(), Payload: event})
	if err != nil {
		reloaded, loadErr := exporter.load()
		if loadErr == nil {
			if existing, ok := reloaded.byIdempotency[mutation.IdempotencyKey]; ok && exactEqual(existing.Request, request) && exactEqual(existing.Mutation, mutation) {
				return existing, nil
			}
		}
		return ExportRecord{}, fmt.Errorf("append training export ledger: %w", err)
	}
	return ExportRecord{Request: request, BundleRef: bundleRef, Bundle: bundle, Mutation: mutation}, nil
}

func (exporter *Exporter) redactManifest(ctx context.Context, manifest DatasetManifest, policy StrictRedactionPolicy) ([]RedactionReceipt, map[string]runmodel.ArtifactRef, error) {
	unique := map[string]runmodel.ArtifactRef{}
	for _, sample := range manifest.Samples {
		for _, ref := range sample.ArtifactRefs {
			unique[artifactRefKey(ref)] = ref
		}
	}
	keys := make([]string, 0, len(unique))
	var total int64
	for key, ref := range unique {
		if ref.SizeBytes > maximumExportSourceBytes-total {
			return nil, nil, fmt.Errorf("training export source bytes exceed %d", maximumExportSourceBytes)
		}
		total += ref.SizeBytes
		keys = append(keys, key)
	}
	sort.Strings(keys)
	receipts := make([]RedactionReceipt, 0, len(keys))
	redacted := make(map[string]runmodel.ArtifactRef, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		source := unique[key]
		for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
			if err := exporter.artifacts.CheckArtifactEligibility(source, use); err != nil {
				return nil, nil, fmt.Errorf("source artifact %s eligibility: %w", use, err)
			}
		}
		content, err := exporter.artifacts.ReadArtifact(source)
		if err != nil {
			return nil, nil, err
		}
		result, err := RedactStrictText(content, policy)
		if err != nil {
			return nil, nil, fmt.Errorf("redact source %q: %w", source.URI, err)
		}
		output, err := exporter.artifacts.PutArtifact(runmodel.ContractTrainingRedactedText, result.Content)
		if err != nil {
			return nil, nil, err
		}
		receipts = append(receipts, RedactionReceipt{SourceRef: source, RedactedRef: output, PolicySHA256: policy.SHA256, Encoding: "utf-8", InputSizeBytes: int64(len(content)), OutputSizeBytes: int64(len(result.Content)), Hits: result.Hits})
		redacted[key] = output
	}
	return receipts, redacted, nil
}

func (exporter *Exporter) Get(exportID string, access evaluation.Access) (ExportRecord, error) {
	if err := validateID("export_id", exportID); err != nil {
		return ExportRecord{}, err
	}
	if err := authorizeRead(access); err != nil {
		return ExportRecord{}, err
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	state, err := exporter.load()
	if err != nil {
		return ExportRecord{}, err
	}
	record, ok := state.byID[exportID]
	if !ok {
		return ExportRecord{}, ErrExportNotFound
	}
	return record, nil
}
func (exporter *Exporter) List(access evaluation.Access) ([]ExportRecord, error) {
	if err := authorizeRead(access); err != nil {
		return nil, err
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	state, err := exporter.load()
	if err != nil {
		return nil, err
	}
	result := make([]ExportRecord, 0, len(state.byID))
	for _, record := range state.byID {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Bundle.ExportID < result[j].Bundle.ExportID })
	return result, nil
}

type exportLedgerState struct {
	events        []exportLedgerEvent
	byIdempotency map[string]ExportRecord
	byID          map[string]ExportRecord
}

func (exporter *Exporter) load() (exportLedgerState, error) {
	state := exportLedgerState{events: []exportLedgerEvent{}, byIdempotency: map[string]ExportRecord{}, byID: map[string]ExportRecord{}}
	envelopes, err := exporter.store.ReadJSONL(exportLedgerStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return exportLedgerState{}, fmt.Errorf("%w: read ledger: %v", ErrExportCorrupt, err)
	}
	for _, envelope := range envelopes {
		if envelope.Schema != ExportLedgerSchemaVersion {
			return exportLedgerState{}, fmt.Errorf("%w: unsupported event schema", ErrExportCorrupt)
		}
		var event exportLedgerEvent
		if err := decodeStrict(envelope.Payload, &event); err != nil {
			return exportLedgerState{}, fmt.Errorf("%w: decode sequence %d: %v", ErrExportCorrupt, envelope.Sequence, err)
		}
		if event.SchemaVersion != ExportLedgerSchemaVersion || event.Request.Validate() != nil || event.Mutation.Validate() != nil || event.BundleRef.Validate() != nil || event.BundleRef.Contract != runmodel.ContractTrainingExportBundle || envelope.ID != event.Mutation.IdempotencyKey || !envelope.Time.Equal(event.Mutation.At.UTC()) || !event.Request.CreatedAt.Equal(event.Mutation.At.UTC()) {
			return exportLedgerState{}, fmt.Errorf("%w: invalid event at sequence %d", ErrExportCorrupt, envelope.Sequence)
		}
		for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
			if err := exporter.artifacts.CheckArtifactEligibility(event.BundleRef, use); err != nil {
				return exportLedgerState{}, fmt.Errorf("%w: bundle %s eligibility: %v", ErrExportCorrupt, use, err)
			}
		}
		data, err := exporter.artifacts.ReadArtifact(event.BundleRef)
		if err != nil {
			return exportLedgerState{}, fmt.Errorf("%w: read bundle: %v", ErrExportCorrupt, err)
		}
		bundle, err := DecodeExportBundle(data)
		if err != nil || !exportBundleBindsRequest(bundle, event.Request, event.Mutation) {
			return exportLedgerState{}, fmt.Errorf("%w: bundle does not bind sequence %d", ErrExportCorrupt, envelope.Sequence)
		}
		access := evaluation.Access{Actor: event.Mutation.Actor, Roles: slices.Clone(event.Mutation.Roles)}
		manifestRecord, err := exporter.manifests.Get(event.Request.ManifestID, access)
		if err != nil || !reflect.DeepEqual(manifestRecord.ManifestRef, event.Request.ManifestRef) || len(bundle.Samples) != len(manifestRecord.Manifest.Samples) {
			return exportLedgerState{}, fmt.Errorf("%w: source manifest does not bind sequence %d", ErrExportCorrupt, envelope.Sequence)
		}
		for index := range bundle.Samples {
			if !reflect.DeepEqual(bundle.Samples[index].GovernedSample, manifestRecord.Manifest.Samples[index]) {
				return exportLedgerState{}, fmt.Errorf("%w: sample does not bind source manifest", ErrExportCorrupt)
			}
		}
		if err := exporter.verifyReceipts(bundle); err != nil {
			return exportLedgerState{}, fmt.Errorf("%w: %v", ErrExportCorrupt, err)
		}
		record := ExportRecord{Request: event.Request, BundleRef: event.BundleRef, Bundle: bundle, Mutation: event.Mutation}
		if _, ok := state.byIdempotency[event.Mutation.IdempotencyKey]; ok {
			return exportLedgerState{}, fmt.Errorf("%w: duplicate idempotency key", ErrExportCorrupt)
		}
		if _, ok := state.byID[bundle.ExportID]; ok {
			return exportLedgerState{}, fmt.Errorf("%w: duplicate export id", ErrExportCorrupt)
		}
		state.events = append(state.events, event)
		state.byIdempotency[event.Mutation.IdempotencyKey] = record
		state.byID[bundle.ExportID] = record
	}
	return state, nil
}

func (exporter *Exporter) verifyReceipts(bundle ExportBundle) error {
	for _, receipt := range bundle.Receipts {
		for _, ref := range []runmodel.ArtifactRef{receipt.SourceRef, receipt.RedactedRef} {
			for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
				if err := exporter.artifacts.CheckArtifactEligibility(ref, use); err != nil {
					return err
				}
			}
		}
		source, err := exporter.artifacts.ReadArtifact(receipt.SourceRef)
		if err != nil {
			return err
		}
		result, err := RedactStrictText(source, bundle.Policy)
		if err != nil {
			return err
		}
		redacted, err := exporter.artifacts.ReadArtifact(receipt.RedactedRef)
		if err != nil {
			return err
		}
		if !bytes.Equal(result.Content, redacted) || !reflect.DeepEqual(result.Hits, receipt.Hits) {
			return fmt.Errorf("redaction receipt output mismatch")
		}
	}
	return nil
}

func exportBundleBindsRequest(bundle ExportBundle, request ExportRequest, mutation evaluation.Mutation) bool {
	return bundle.ExportID == request.ExportID && bundle.ManifestID == request.ManifestID && reflect.DeepEqual(bundle.ManifestRef, request.ManifestRef) && reflect.DeepEqual(bundle.Policy, request.Policy) && bundle.CreatedBy == mutation.Actor && bundle.CreatedAt.Equal(request.CreatedAt)
}
