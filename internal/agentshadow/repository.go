package agentshadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"argus.local/argus/internal/artifactrepo"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	intentRoot      = "agent-review-shadow/import-intents/"
	importRoot      = "agent-review-shadow/imports/"
	observationRoot = "agent-review-shadow/observations/"
	hostActor       = "argus-go-host"
)

type Repository struct {
	store     *local.Store
	artifacts *artifactrepo.Repository
	runs      *runrepo.Repository
	now       func() time.Time
}

func OpenRepository(store *local.Store, now func() time.Time) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	if now == nil {
		now = time.Now
	}
	artifacts, err := artifactrepo.Open(store, now)
	if err != nil {
		return nil, fmt.Errorf("open governed artifact repository: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, fmt.Errorf("open review run repository: %w", err)
	}
	return &Repository{store: store, artifacts: artifacts, runs: runs, now: now}, nil
}

func (repository *Repository) acquireIntent(
	ctx context.Context,
	scope Scope,
	idempotencyKey string,
	inputDigest string,
	acceptedAt time.Time,
) (importIntent, error) {
	if err := contextError(ctx); err != nil {
		return importIntent{}, err
	}
	intent := importIntent{
		SchemaVersion: importIntentSchemaVersion,
		TenantID:      scope.TenantID, WorkspaceID: scope.WorkspaceID,
		IdempotencyKey: idempotencyKey, InputDigest: inputDigest,
		AcceptedAt: acceptedAt,
	}
	if err := validateIntent(intent); err != nil {
		return importIntent{}, err
	}
	objectID := intentObjectID(scope, idempotencyKey)
	if err := repository.store.PutJSON(objectID, intent); err == nil {
		return intent, nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return importIntent{}, fmt.Errorf("persist import intent: %w", err)
	}
	var existing importIntent
	if err := repository.store.GetJSON(objectID, &existing); err != nil {
		return importIntent{}, fmt.Errorf("%w: load existing import intent: %v", ErrCorrupt, err)
	}
	if err := validateIntent(existing); err != nil {
		return importIntent{}, fmt.Errorf("%w: existing import intent: %v", ErrCorrupt, err)
	}
	if existing.TenantID != scope.TenantID ||
		existing.WorkspaceID != scope.WorkspaceID ||
		existing.IdempotencyKey != idempotencyKey {
		return importIntent{}, fmt.Errorf("%w: import intent scope changed", ErrCorrupt)
	}
	if existing.InputDigest != inputDigest {
		return importIntent{}, fmt.Errorf(
			"%w: idempotency key %q is already bound to another input",
			ErrConflict,
			idempotencyKey,
		)
	}
	return existing, nil
}

func (repository *Repository) normalizedNow() (time.Time, error) {
	now := repository.now()
	if now.IsZero() {
		return time.Time{}, fmt.Errorf("host clock returned a zero timestamp")
	}
	return time.Unix(0, now.UnixNano()).UTC(), nil
}

func (repository *Repository) putArtifact(
	ctx context.Context,
	scope Scope,
	intent importIntent,
	kind string,
	contract string,
	content []byte,
) (contractsv1alpha1.ArtifactBinding, error) {
	if err := contextError(ctx); err != nil {
		return contractsv1alpha1.ArtifactBinding{}, err
	}
	allowedUses := []artifactrepo.Use{artifactrepo.UseRead}
	if contract == contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion {
		allowedUses = []artifactrepo.Use{
			artifactrepo.UseSensitiveProcess,
			artifactrepo.UseSensitiveRead,
		}
	}
	ref, _, err := repository.artifacts.Put(
		ctx,
		artifactSubject(scope),
		artifactrepo.PutRequest{
			Authority: artifactrepo.DefaultAuthority,
			TenantID:  scope.TenantID, WorkspaceID: scope.WorkspaceID,
			Contract: contract, AllowedUses: allowedUses,
			Content: content,
			Mutation: artifactrepo.Mutation{
				IdempotencyKey: mutationKey(intent, kind),
				Actor:          hostActor,
				Audit:          "persist validated agent review shadow " + kind,
				At:             intent.AcceptedAt,
			},
		},
	)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf("persist %s artifact: %w", kind, err)
	}
	return bindingFromRef(ref), nil
}

func (repository *Repository) resolveTaskEvidenceForProcessing(
	ctx context.Context,
	scope Scope,
	binding contractsv1alpha1.ArtifactBinding,
) ([]byte, artifactrepo.Record, error) {
	return repository.artifacts.Resolve(
		ctx,
		artifactProcessingSubject(scope),
		governedArtifactRef(binding),
		artifactrepo.UseSensitiveProcess,
	)
}

func (repository *Repository) inspectTaskEvidence(
	ctx context.Context,
	scope Scope,
	binding contractsv1alpha1.ArtifactBinding,
) (artifactrepo.Record, error) {
	return repository.artifacts.Inspect(
		ctx,
		artifactProcessingSubject(scope),
		governedArtifactRef(binding),
	)
}

func (repository *Repository) resolveTaskEvidenceForDisclosure(
	ctx context.Context,
	scope Scope,
	binding contractsv1alpha1.ArtifactBinding,
	access artifactrepo.SensitiveAccessRequest,
) ([]byte, artifactrepo.Record, artifactrepo.SensitiveAccessProof, error) {
	return repository.artifacts.ResolveSensitive(
		ctx,
		artifactDisclosureSubject(scope),
		governedArtifactRef(binding),
		access,
	)
}

func (repository *Repository) tombstoneTaskEvidence(
	ctx context.Context,
	scope Scope,
	binding contractsv1alpha1.ArtifactBinding,
	reason string,
	mutation artifactrepo.Mutation,
) (artifactrepo.Record, error) {
	return repository.artifacts.Tombstone(
		ctx,
		artifactOperatorSubject(scope),
		governedArtifactRef(binding),
		reason,
		mutation,
	)
}

func governedArtifactRef(binding contractsv1alpha1.ArtifactBinding) artifactrepo.Ref {
	return artifactrepo.Ref{
		URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
		SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
	}
}

func (repository *Repository) resolveArtifact(
	ctx context.Context,
	scope Scope,
	binding contractsv1alpha1.ArtifactBinding,
) ([]byte, error) {
	content, _, err := repository.artifacts.Resolve(
		ctx,
		artifactSubject(scope),
		artifactrepo.Ref{
			URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
			SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
		},
		artifactrepo.UseRead,
	)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// resolveFrozenInput reads an artifact from the existing authoritative local
// run store. These refs predate the governed artifact registry and therefore
// use artifact://local/sha256 URIs. The service separately validates the
// ReviewSpec tenant/workspace binding before accepting them.
func (repository *Repository) resolveFrozenInput(
	binding contractsv1alpha1.ArtifactBinding,
) ([]byte, error) {
	content, err := repository.store.ReadArtifact(local.ArtifactRef{
		URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
		SizeBytes: binding.Ref.SizeBytes,
	})
	if err != nil {
		return nil, err
	}
	return content, nil
}

func (repository *Repository) appendObservation(
	ctx context.Context,
	record ImportRecord,
	observation contractsv1alpha1.AgentReviewObservation,
) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	envelope, err := repository.store.AppendJSONLAtSequence(
		record.ObservationStream,
		0,
		local.Event{
			ID:      observation.ObservationID,
			Schema:  contractsv1alpha1.AgentReviewObservationSchemaVersion,
			Time:    observation.RecordedAt,
			Payload: observation,
		},
	)
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return fmt.Errorf("%w: append shadow observation: %v", ErrConflict, err)
		}
		return fmt.Errorf("append shadow observation: %w", err)
	}
	if envelope.Sequence != 1 || envelope.ID != observation.ObservationID {
		return fmt.Errorf("%w: observation append returned another event", ErrCorrupt)
	}
	return nil
}

func (repository *Repository) commit(record ImportRecord) (ImportRecord, error) {
	if err := validateImportRecord(record); err != nil {
		return ImportRecord{}, err
	}
	objectID := importObjectID(record.ManifestID)
	if err := repository.store.PutJSON(objectID, record); err == nil {
		if err := repository.ensureCommittedIndex(record); err != nil {
			return ImportRecord{}, err
		}
		return record, nil
	} else if !errors.Is(err, local.ErrImmutableExists) {
		return ImportRecord{}, fmt.Errorf("persist import commit record: %w", err)
	}
	var existing ImportRecord
	if err := repository.store.GetJSON(objectID, &existing); err != nil {
		return ImportRecord{}, fmt.Errorf("%w: load existing import record: %v", ErrCorrupt, err)
	}
	if err := validateImportRecord(existing); err != nil {
		return ImportRecord{}, fmt.Errorf("%w: existing import record: %v", ErrCorrupt, err)
	}
	if !reflect.DeepEqual(existing, record) {
		return ImportRecord{}, fmt.Errorf("%w: manifest id is already committed", ErrConflict)
	}
	if err := repository.ensureCommittedIndex(existing); err != nil {
		return ImportRecord{}, err
	}
	return existing, nil
}

func (repository *Repository) loadRecord(manifestID string) (ImportRecord, error) {
	var record ImportRecord
	if err := repository.store.GetJSON(importObjectID(manifestID), &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ImportRecord{}, ErrNotFound
		}
		return ImportRecord{}, fmt.Errorf("%w: load import record: %v", ErrCorrupt, err)
	}
	if err := validateImportRecord(record); err != nil {
		return ImportRecord{}, fmt.Errorf("%w: validate import record: %v", ErrCorrupt, err)
	}
	if record.ManifestID != manifestID {
		return ImportRecord{}, fmt.Errorf("%w: import lookup does not bind manifest id", ErrCorrupt)
	}
	return record, nil
}

func (repository *Repository) loadIntent(record ImportRecord) (importIntent, error) {
	var intent importIntent
	scope := Scope{TenantID: record.TenantID, WorkspaceID: record.WorkspaceID}
	if err := repository.store.GetJSON(
		intentObjectID(scope, record.IdempotencyKey),
		&intent,
	); err != nil {
		return importIntent{}, fmt.Errorf("%w: load committed import intent: %v", ErrCorrupt, err)
	}
	if err := validateIntent(intent); err != nil {
		return importIntent{}, fmt.Errorf("%w: validate committed import intent: %v", ErrCorrupt, err)
	}
	if intent.TenantID != record.TenantID || intent.WorkspaceID != record.WorkspaceID ||
		intent.IdempotencyKey != record.IdempotencyKey ||
		intent.InputDigest != record.InputDigest || !intent.AcceptedAt.Equal(record.AcceptedAt) {
		return importIntent{}, fmt.Errorf("%w: import record does not match its intent", ErrCorrupt)
	}
	return intent, nil
}

func (repository *Repository) loadObservation(
	record ImportRecord,
) (contractsv1alpha1.AgentReviewObservation, error) {
	envelopes, err := repository.store.ReadJSONL(record.ObservationStream)
	if err != nil {
		return contractsv1alpha1.AgentReviewObservation{}, fmt.Errorf(
			"%w: read observation stream: %v",
			ErrCorrupt,
			err,
		)
	}
	if len(envelopes) != 1 {
		return contractsv1alpha1.AgentReviewObservation{}, fmt.Errorf(
			"%w: committed import requires exactly one initial observation",
			ErrCorrupt,
		)
	}
	envelope := envelopes[0]
	if envelope.Sequence != 1 || envelope.ID != record.ObservationID ||
		envelope.Schema != contractsv1alpha1.AgentReviewObservationSchemaVersion {
		return contractsv1alpha1.AgentReviewObservation{}, fmt.Errorf(
			"%w: observation envelope identity is inconsistent",
			ErrCorrupt,
		)
	}
	observation, err := contractsv1alpha1.DecodeAgentReviewObservation(envelope.Payload)
	if err != nil {
		return contractsv1alpha1.AgentReviewObservation{}, fmt.Errorf(
			"%w: decode observation: %v",
			ErrCorrupt,
			err,
		)
	}
	if !envelope.Time.Equal(observation.RecordedAt) || observation.Sequence != envelope.Sequence {
		return contractsv1alpha1.AgentReviewObservation{}, fmt.Errorf(
			"%w: observation envelope does not match its payload",
			ErrCorrupt,
		)
	}
	return observation, nil
}

func artifactSubject(scope Scope) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID:    scope.TenantID,
		WorkspaceID: scope.WorkspaceID,
		Roles: []string{
			artifactrepo.RoleSensitiveProcessor,
			artifactrepo.RoleSensitiveReader,
		},
	}
}

func artifactProcessingSubject(scope Scope) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID,
		Roles: []string{artifactrepo.RoleSensitiveProcessor},
	}
}

func artifactDisclosureSubject(scope Scope) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID,
		Roles: []string{artifactrepo.RoleSensitiveReader},
	}
}

func artifactOperatorSubject(scope Scope) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID,
		Roles: []string{artifactrepo.RoleIntegrityOperator},
	}
}

func bindingFromRef(ref artifactrepo.Ref) contractsv1alpha1.ArtifactBinding {
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
		},
		Contract: ref.Contract,
	}
}

func intentObjectID(scope Scope, idempotencyKey string) string {
	return intentRoot + digestStrings(scope.TenantID, scope.WorkspaceID, idempotencyKey)
}

func importObjectID(manifestID string) string {
	return importRoot + digestStrings(manifestID)
}

func observationStream(scope Scope, manifestID string) string {
	return observationRoot + digestStrings(scope.TenantID, scope.WorkspaceID, manifestID)
}

func mutationKey(intent importIntent, kind string) string {
	return "ari-" + digestStrings(
		intent.TenantID,
		intent.WorkspaceID,
		intent.IdempotencyKey,
		kind,
	)
}

func digestStrings(values ...string) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte(fmt.Sprintf("%d:", len(value))))
		_, _ = digest.Write([]byte(value))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func validateIntent(intent importIntent) error {
	if intent.SchemaVersion != importIntentSchemaVersion {
		return fmt.Errorf("unsupported import intent schema %q", intent.SchemaVersion)
	}
	subject := artifactSubject(Scope{
		TenantID: intent.TenantID, WorkspaceID: intent.WorkspaceID,
	})
	if err := subject.Validate(); err != nil {
		return fmt.Errorf("validate import scope: %w", err)
	}
	mutation := artifactrepo.Mutation{
		IdempotencyKey: intent.IdempotencyKey,
		Actor:          hostActor, Audit: "validate agent review shadow import intent",
		At: intent.AcceptedAt,
	}
	if err := mutation.Validate(); err != nil {
		return fmt.Errorf("validate import idempotency: %w", err)
	}
	if !lowerSHA256(intent.InputDigest) {
		return fmt.Errorf("input_digest must be a lowercase SHA-256")
	}
	return nil
}

func validateImportRecord(record ImportRecord) error {
	if record.SchemaVersion != ImportRecordSchemaVersion {
		return fmt.Errorf("unsupported import record schema %q", record.SchemaVersion)
	}
	intent := importIntent{
		SchemaVersion: importIntentSchemaVersion,
		TenantID:      record.TenantID, WorkspaceID: record.WorkspaceID,
		IdempotencyKey: record.IdempotencyKey, InputDigest: record.InputDigest,
		AcceptedAt: record.AcceptedAt,
	}
	if err := validateIntent(intent); err != nil {
		return err
	}
	wantManifestID := manifestID(intent)
	wantObservationID := observationID(intent)
	scope := Scope{TenantID: record.TenantID, WorkspaceID: record.WorkspaceID}
	if record.ManifestID != wantManifestID || record.ObservationID != wantObservationID ||
		record.ObservationStream != observationStream(scope, record.ManifestID) {
		return fmt.Errorf("import record derived identities are inconsistent")
	}
	for name, binding := range map[string]contractsv1alpha1.ArtifactBinding{
		"agent review plan":        record.AgentReviewPlanRef,
		"hypothesis set":           record.HypothesisSetRef,
		"raw candidate collection": record.RawCandidateCollectionRef,
		"task evidence collection": record.TaskEvidenceCollectionRef,
		"receipt collection":       record.ReceiptCollectionRef,
		"manifest":                 record.ManifestRef,
	} {
		if strings.TrimSpace(binding.Contract) == "" || !lowerSHA256(binding.Ref.SHA256) ||
			binding.Ref.SizeBytes <= 0 || strings.TrimSpace(binding.Ref.URI) == "" {
			return fmt.Errorf("%s artifact binding is invalid", name)
		}
	}
	if record.AgentReviewPlanRef.Contract != contractsv1alpha1.AgentReviewPlanSchemaVersion ||
		record.HypothesisSetRef.Contract != contractsv1alpha1.ReviewHypothesisSetSchemaVersion ||
		record.RawCandidateCollectionRef.Contract !=
			contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion ||
		record.TaskEvidenceCollectionRef.Contract !=
			contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion ||
		record.ReceiptCollectionRef.Contract !=
			contractsv1alpha1.AgentExecutionReceiptCollectionContract ||
		record.ManifestRef.Contract != contractsv1alpha1.AgentReviewResultManifestSchemaVersion {
		return fmt.Errorf("import record artifact contracts are inconsistent")
	}
	return nil
}

func lowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
