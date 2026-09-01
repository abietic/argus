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

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
)

type CaseSource interface {
	GetCase(string, evaluation.Access) (evaluation.CaseRecord, error)
	AnnotationHistory(string, evaluation.Access) ([]evaluation.CaseAnnotationEntry, error)
	AdjudicationHistory(string, evaluation.Access) ([]evaluation.CaseAdjudicationEntry, error)
}

type ArtifactSource interface {
	PutArtifact(string, []byte) (runmodel.ArtifactRef, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
	CheckArtifactEligibility(runmodel.ArtifactRef, runrepo.ArtifactUse) error
}

type Repository struct {
	store     *local.Store
	cases     CaseSource
	artifacts ArtifactSource
	mu        *sync.Mutex
}

var repositoryLocks sync.Map

func New(store *local.Store, cases CaseSource, artifacts ArtifactSource) (*Repository, error) {
	if store == nil || cases == nil || artifacts == nil {
		return nil, fmt.Errorf("training store, case source, and artifact source are required")
	}
	value, _ := repositoryLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &Repository{store: store, cases: cases, artifacts: artifacts, mu: value.(*sync.Mutex)}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Materialize(ctx context.Context, request MaterializationRequest, mutation evaluation.Mutation) (Record, error) {
	if ctx == nil {
		return Record{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if err := request.Validate(); err != nil {
		return Record{}, fmt.Errorf("validate materialization request: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return Record{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return Record{}, fmt.Errorf("request created_at must equal mutation time")
	}
	if !slices.Contains(mutation.Roles, evaluation.RoleDatasetCurator) {
		return Record{}, fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	for _, binding := range request.Cases {
		if slices.Contains(binding.Authority.ReviewerIDs, mutation.Actor) || binding.Authority.AdjudicatorID == mutation.Actor {
			return Record{}, fmt.Errorf("%w: materialization operator must be independent from label authority", ErrUnauthorized)
		}
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	if existing, ok := state.byIdempotency[mutation.IdempotencyKey]; ok {
		if !exactEqual(existing.Request, request) || !exactEqual(existing.Mutation, mutation) {
			return Record{}, fmt.Errorf("%w: idempotency key was already used", ErrConflict)
		}
		return existing, nil
	}
	datasetKey := request.DatasetID + "\x00" + request.DatasetRevision
	if _, exists := state.byDataset[datasetKey]; exists {
		return Record{}, fmt.Errorf("%w: dataset id and revision already exist", ErrConflict)
	}

	bundle, err := repository.loadPolicyBundle(request)
	if err != nil {
		return Record{}, err
	}
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	samples := make([]ManifestSample, 0, len(request.Cases))
	for index, binding := range request.Cases {
		if err := ctx.Err(); err != nil {
			return Record{}, err
		}
		sample, sampleErr := repository.materializeCase(binding, request.RepositoryID, access)
		if sampleErr != nil {
			return Record{}, fmt.Errorf("cases[%d]: %w", index, sampleErr)
		}
		samples = append(samples, sample)
	}
	manifest, err := sealManifest(DatasetManifest{
		DatasetID: request.DatasetID, DatasetRevision: request.DatasetRevision,
		RepositoryID: request.RepositoryID, ConfigContext: bundle.Context,
		ConfigBundleRef: request.ConfigBundleRef, DataPolicy: bundle.Data,
		ContentMode: ContentModeReferenceOnly, ContainsSourceBytes: false, SelfLabelsAllowed: false,
		Samples: samples, CreatedBy: mutation.Actor, CreatedAt: request.CreatedAt,
	})
	if err != nil {
		return Record{}, err
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return Record{}, err
	}
	manifestRef, err := repository.artifacts.PutArtifact(runmodel.ContractTrainingDatasetManifest, content)
	if err != nil {
		return Record{}, fmt.Errorf("persist training dataset manifest: %w", err)
	}
	event := ledgerEvent{SchemaVersion: LedgerEventSchemaVersion, Request: request, ManifestRef: manifestRef, ManifestID: manifest.ManifestID, Mutation: mutation}
	_, err = repository.store.AppendJSONLAtSequence(ledgerStream, uint64(len(state.events)), local.Event{ID: mutation.IdempotencyKey, Schema: LedgerEventSchemaVersion, Time: mutation.At.UTC(), Payload: event})
	if err != nil {
		reloaded, loadErr := repository.load()
		if loadErr == nil {
			if existing, ok := reloaded.byIdempotency[mutation.IdempotencyKey]; ok && exactEqual(existing.Request, request) && exactEqual(existing.Mutation, mutation) {
				return existing, nil
			}
		}
		return Record{}, fmt.Errorf("append training dataset ledger: %w", err)
	}
	return Record{Request: request, ManifestRef: manifestRef, Manifest: manifest, Mutation: mutation}, nil
}

func (repository *Repository) loadPolicyBundle(request MaterializationRequest) (reviewconfig.ConfigBundle, error) {
	for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
		if err := repository.artifacts.CheckArtifactEligibility(request.ConfigBundleRef, use); err != nil {
			return reviewconfig.ConfigBundle{}, fmt.Errorf("config bundle %s eligibility: %w", use, err)
		}
	}
	data, err := repository.artifacts.ReadArtifact(request.ConfigBundleRef)
	if err != nil {
		return reviewconfig.ConfigBundle{}, fmt.Errorf("read config bundle: %w", err)
	}
	bundle, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return reviewconfig.ConfigBundle{}, fmt.Errorf("decode config bundle: %w", err)
	}
	if bundle.Context.RepositoryID != request.RepositoryID {
		return reviewconfig.ConfigBundle{}, fmt.Errorf("%w: config bundle repository does not match request", ErrPolicyDenied)
	}
	if bundle.Data.Training != reviewconfig.PermissionAllow || bundle.Data.Export != reviewconfig.PermissionAllow || bundle.Data.Redaction != reviewconfig.RedactionStrict {
		return reviewconfig.ConfigBundle{}, fmt.Errorf("%w: config must allow training and export with strict redaction", ErrPolicyDenied)
	}
	return bundle, nil
}

func (repository *Repository) materializeCase(binding CaseBinding, repositoryID string, access evaluation.Access) (ManifestSample, error) {
	record, err := repository.cases.GetCase(binding.CaseID, access)
	if err != nil {
		return ManifestSample{}, err
	}
	current := record.CurrentCase()
	if record.CurrentGovernance.Revision != binding.ExpectedGovernanceRevision || record.CurrentLabelRevision != binding.ExpectedLabelRevision {
		return ManifestSample{}, ErrContaminated
	}
	if current.Provenance.RepositoryID != repositoryID {
		return ManifestSample{}, fmt.Errorf("repository binding mismatch")
	}
	if current.DatasetState != evaluation.DatasetActive || current.ReviewState != evaluation.ReviewApproved || current.Split != evaluation.SplitTrain || !current.Eligibility.Training {
		return ManifestSample{}, fmt.Errorf("case must be active, approved, train split, and training eligible")
	}
	if !slices.Contains(current.LicenseConsent.AllowedUses, evaluation.UseTraining) {
		return ManifestSample{}, fmt.Errorf("%w: case license does not allow training", ErrPolicyDenied)
	}
	if err := repository.validateAuthority(record, binding.Authority, access); err != nil {
		return ManifestSample{}, fmt.Errorf("label authority: %w", err)
	}
	required := requiredURIs(current, binding.Authority)
	provided := make([]string, len(binding.ArtifactRefs))
	for index, ref := range binding.ArtifactRefs {
		provided[index] = ref.URI
	}
	if !slices.Equal(required, provided) {
		return ManifestSample{}, fmt.Errorf("artifact refs do not exactly cover governed input and evidence URIs")
	}
	for _, ref := range binding.ArtifactRefs {
		for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
			if err := repository.artifacts.CheckArtifactEligibility(ref, use); err != nil {
				return ManifestSample{}, fmt.Errorf("artifact %q %s eligibility: %w", ref.URI, use, err)
			}
		}
		if _, err := repository.artifacts.ReadArtifact(ref); err != nil {
			return ManifestSample{}, fmt.Errorf("verify artifact %q content: %w", ref.URI, err)
		}
	}
	return ManifestSample{
		CaseID: current.CaseID, CaseGovernanceRevision: record.CurrentGovernance.Revision, CaseLabelRevision: record.CurrentLabelRevision,
		CaseType: current.Type, Provenance: current.Provenance, LicenseConsent: current.LicenseConsent,
		Classification: current.Classification, Owner: current.Owner, InputSnapshotRef: current.InputSnapshotRef,
		Label: current.Label, LabelPolicyRevision: current.LabelPolicyRevision,
		ReviewState: current.ReviewState, DatasetState: current.DatasetState, DatasetSplit: current.Split,
		CloneGroupID: current.CloneGroupID, Eligibility: current.Eligibility, CaseCreatedAt: current.CreatedAt,
		ArtifactRefs: slices.Clone(binding.ArtifactRefs), Authority: binding.Authority,
	}, nil
}

func (repository *Repository) validateAuthority(record evaluation.CaseRecord, authority LabelAuthority, access evaluation.Access) error {
	if record.ExternalGovernance != nil {
		attestation := record.ExternalGovernance.Attestation
		if authority.Kind != AuthorityExternalGovernance || !slices.Equal(authority.ReviewerIDs, attestation.ReviewerIDs) || authority.AdjudicatorID != attestation.AdjudicatorID || !slices.Equal(authority.EvidenceRefs, attestation.EvidenceRefs) {
			return fmt.Errorf("authority does not match trusted external governance record")
		}
		return nil
	}
	if authority.Kind != AuthorityHumanAdjudication {
		return fmt.Errorf("local case requires human_adjudication authority")
	}
	annotations, err := repository.cases.AnnotationHistory(record.Case.CaseID, access)
	if err != nil {
		return err
	}
	adjudications, err := repository.cases.AdjudicationHistory(record.Case.CaseID, access)
	if err != nil {
		return err
	}
	annotationByID := make(map[string]evaluation.CaseAnnotationEntry, len(annotations))
	for _, entry := range annotations {
		annotationByID[entry.EventID] = entry
	}
	for index := len(adjudications) - 1; index >= 0; index-- {
		entry := adjudications[index]
		if entry.Adjudication.Outcome != evaluation.AdjudicationApprove || entry.Adjudicator != authority.AdjudicatorID || !slices.Equal(entry.Adjudication.EvidenceRefs, authority.EvidenceRefs) || entry.Adjudication.SelectedLabel == nil || !reflect.DeepEqual(*entry.Adjudication.SelectedLabel, record.CurrentLabel) {
			continue
		}
		reviewers := make([]string, 0, len(entry.Adjudication.AnnotationEventIDs))
		valid := true
		for _, id := range entry.Adjudication.AnnotationEventIDs {
			annotation, ok := annotationByID[id]
			if !ok {
				valid = false
				break
			}
			reviewers = append(reviewers, annotation.Reviewer)
		}
		sort.Strings(reviewers)
		reviewers = slices.Compact(reviewers)
		if valid && slices.Equal(reviewers, authority.ReviewerIDs) {
			return nil
		}
	}
	return fmt.Errorf("authority does not match an approved governed adjudication")
}

func (repository *Repository) Get(manifestID string, access evaluation.Access) (Record, error) {
	if err := validateID("manifest_id", manifestID); err != nil {
		return Record{}, err
	}
	if err := authorizeRead(access); err != nil {
		return Record{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	record, ok := state.byID[manifestID]
	if !ok {
		return Record{}, ErrNotFound
	}
	return record, nil
}

func (repository *Repository) List(access evaluation.Access) ([]Record, error) {
	if err := authorizeRead(access); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]Record, 0, len(state.byID))
	for _, record := range state.byID {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Manifest.ManifestID < result[j].Manifest.ManifestID
	})
	return result, nil
}

type ledgerState struct {
	events        []ledgerEvent
	byIdempotency map[string]Record
	byID          map[string]Record
	byDataset     map[string]string
}

func (repository *Repository) load() (ledgerState, error) {
	state := ledgerState{events: []ledgerEvent{}, byIdempotency: map[string]Record{}, byID: map[string]Record{}, byDataset: map[string]string{}}
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
		if event.SchemaVersion != LedgerEventSchemaVersion || event.Request.Validate() != nil || event.Mutation.Validate() != nil {
			return ledgerState{}, fmt.Errorf("%w: invalid event at sequence %d", ErrCorrupt, envelope.Sequence)
		}
		if event.ManifestRef.Contract != runmodel.ContractTrainingDatasetManifest || event.ManifestRef.Validate() != nil || envelope.ID != event.Mutation.IdempotencyKey || !envelope.Time.Equal(event.Mutation.At.UTC()) || !event.Request.CreatedAt.Equal(event.Mutation.At.UTC()) {
			return ledgerState{}, fmt.Errorf("%w: envelope does not bind payload at sequence %d", ErrCorrupt, envelope.Sequence)
		}
		for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
			if err := repository.artifacts.CheckArtifactEligibility(event.ManifestRef, use); err != nil {
				return ledgerState{}, fmt.Errorf("%w: manifest artifact %s eligibility: %v", ErrCorrupt, use, err)
			}
		}
		data, err := repository.artifacts.ReadArtifact(event.ManifestRef)
		if err != nil {
			return ledgerState{}, fmt.Errorf("%w: read manifest artifact: %v", ErrCorrupt, err)
		}
		manifest, err := DecodeDatasetManifest(data)
		if err != nil || manifest.ManifestID != event.ManifestID || manifest.DatasetID != event.Request.DatasetID || manifest.DatasetRevision != event.Request.DatasetRevision || manifest.RepositoryID != event.Request.RepositoryID || !manifest.ConfigBundleRefEqual(event.Request.ConfigBundleRef) || !manifest.CreatedAt.Equal(event.Request.CreatedAt) || manifest.CreatedBy != event.Mutation.Actor || !manifestBindsRequest(manifest, event.Request) {
			return ledgerState{}, fmt.Errorf("%w: manifest does not bind sequence %d", ErrCorrupt, envelope.Sequence)
		}
		bundle, err := repository.loadPolicyBundle(event.Request)
		if err != nil || !reflect.DeepEqual(manifest.ConfigContext, bundle.Context) || !reflect.DeepEqual(manifest.DataPolicy, bundle.Data) {
			return ledgerState{}, fmt.Errorf("%w: policy bundle does not bind sequence %d", ErrCorrupt, envelope.Sequence)
		}
		for _, sample := range manifest.Samples {
			for _, ref := range sample.ArtifactRefs {
				for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
					if err := repository.artifacts.CheckArtifactEligibility(ref, use); err != nil {
						return ledgerState{}, fmt.Errorf("%w: source artifact %s eligibility: %v", ErrCorrupt, use, err)
					}
				}
				if _, err := repository.artifacts.ReadArtifact(ref); err != nil {
					return ledgerState{}, fmt.Errorf("%w: verify source artifact: %v", ErrCorrupt, err)
				}
			}
		}
		record := Record{Request: event.Request, ManifestRef: event.ManifestRef, Manifest: manifest, Mutation: event.Mutation}
		datasetKey := manifest.DatasetID + "\x00" + manifest.DatasetRevision
		if _, ok := state.byIdempotency[event.Mutation.IdempotencyKey]; ok {
			return ledgerState{}, fmt.Errorf("%w: duplicate idempotency key", ErrCorrupt)
		}
		if _, ok := state.byID[manifest.ManifestID]; ok {
			return ledgerState{}, fmt.Errorf("%w: duplicate manifest id", ErrCorrupt)
		}
		if _, ok := state.byDataset[datasetKey]; ok {
			return ledgerState{}, fmt.Errorf("%w: duplicate dataset revision", ErrCorrupt)
		}
		state.events = append(state.events, event)
		state.byIdempotency[event.Mutation.IdempotencyKey] = record
		state.byID[manifest.ManifestID] = record
		state.byDataset[datasetKey] = manifest.ManifestID
	}
	return state, nil
}

func (manifest DatasetManifest) ConfigBundleRefEqual(ref runmodel.ArtifactRef) bool {
	return reflect.DeepEqual(manifest.ConfigBundleRef, ref)
}

func manifestBindsRequest(manifest DatasetManifest, request MaterializationRequest) bool {
	if len(manifest.Samples) != len(request.Cases) {
		return false
	}
	for index, binding := range request.Cases {
		sample := manifest.Samples[index]
		if sample.CaseID != binding.CaseID || sample.CaseGovernanceRevision != binding.ExpectedGovernanceRevision || sample.CaseLabelRevision != binding.ExpectedLabelRevision || !reflect.DeepEqual(sample.ArtifactRefs, binding.ArtifactRefs) || !reflect.DeepEqual(sample.Authority, binding.Authority) {
			return false
		}
	}
	return true
}

func authorizeRead(access evaluation.Access) error {
	if err := access.Validate(); err != nil {
		return err
	}
	if !slices.Contains(access.Roles, evaluation.RoleDatasetCurator) && !slices.Contains(access.Roles, evaluation.RoleDatasetAdjudicator) {
		return fmt.Errorf("%w: dataset_curator or dataset_adjudicator role is required", ErrUnauthorized)
	}
	return nil
}

func exactEqual(left, right any) bool {
	a, ae := json.Marshal(left)
	b, be := json.Marshal(right)
	return ae == nil && be == nil && bytes.Equal(a, b)
}
