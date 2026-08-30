package runrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

const (
	ArtifactIntegrityEventSchemaVersion  = "argus.artifact_integrity_event.v1alpha1"
	ArtifactIntegrityChangeSchemaVersion = "argus.artifact_integrity_change.v1alpha1"
)

var (
	ErrArtifactQuarantined = errors.New("artifact is quarantined")
	ErrArtifactTombstoned  = errors.New("artifact is tombstoned")
	ErrArtifactIntegrity   = errors.New("artifact integrity ledger is corrupt")
	ErrArtifactTransition  = errors.New("invalid artifact integrity transition")
)

type ArtifactUse string

const (
	ArtifactUseRead          ArtifactUse = "read"
	ArtifactUseCandidatePool ArtifactUse = "candidate_pool"
	ArtifactUsePublication   ArtifactUse = "publication"
	ArtifactUseEvaluation    ArtifactUse = "evaluation"
	ArtifactUseTraining      ArtifactUse = "training"
	ArtifactUseExport        ArtifactUse = "export"
)

type ArtifactIntegrityState string

const (
	ArtifactIntegrityActive      ArtifactIntegrityState = "active"
	ArtifactIntegrityQuarantined ArtifactIntegrityState = "quarantined"
	ArtifactIntegrityTombstoned  ArtifactIntegrityState = "tombstoned"
)

type ArtifactIntegrityMutation struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

type ArtifactIntegrityChange struct {
	SchemaVersion string                    `json:"schema_version"`
	Ref           runmodel.ArtifactRef      `json:"ref"`
	Reason        string                    `json:"reason"`
	Mutation      ArtifactIntegrityMutation `json:"mutation"`
}

type ArtifactIntegrityRecord struct {
	Ref               runmodel.ArtifactRef   `json:"ref"`
	State             ArtifactIntegrityState `json:"state"`
	StateReason       string                 `json:"state_reason,omitempty"`
	StateChangedAt    *time.Time             `json:"state_changed_at,omitempty"`
	LastMutationActor string                 `json:"last_mutation_actor,omitempty"`
	Sequence          uint64                 `json:"sequence"`
}

type artifactIntegrityEventType string

const (
	artifactIntegrityQuarantined artifactIntegrityEventType = "quarantined"
	artifactIntegrityReleased    artifactIntegrityEventType = "quarantine_released"
	artifactIntegrityTombstoned  artifactIntegrityEventType = "tombstoned"
)

type artifactIntegrityEvent struct {
	SchemaVersion string                     `json:"schema_version"`
	Type          artifactIntegrityEventType `json:"type"`
	Ref           runmodel.ArtifactRef       `json:"ref"`
	Reason        string                     `json:"reason"`
	Mutation      ArtifactIntegrityMutation  `json:"mutation"`
}

func (mutation ArtifactIntegrityMutation) Validate() error {
	if err := validateIntegrityText("idempotency_key", mutation.IdempotencyKey); err != nil {
		return err
	}
	if err := validateIntegrityText("actor", mutation.Actor); err != nil {
		return err
	}
	if err := validateIntegrityText("audit", mutation.Audit); err != nil {
		return err
	}
	if mutation.At.IsZero() || mutation.At.Location() != time.UTC {
		return fmt.Errorf("artifact integrity mutation at must be non-zero UTC")
	}
	return nil
}

func (change ArtifactIntegrityChange) Validate() error {
	if change.SchemaVersion != ArtifactIntegrityChangeSchemaVersion {
		return fmt.Errorf("unsupported artifact integrity change schema %q", change.SchemaVersion)
	}
	if err := change.Ref.Validate(); err != nil {
		return err
	}
	if err := validateIntegrityText("reason", change.Reason); err != nil {
		return err
	}
	return change.Mutation.Validate()
}

func DecodeArtifactIntegrityChangeJSON(data []byte) (ArtifactIntegrityChange, error) {
	var change ArtifactIntegrityChange
	if err := decodeStrictJSON(data, &change); err != nil {
		return ArtifactIntegrityChange{}, fmt.Errorf("decode ArtifactIntegrityChange: %w", err)
	}
	if err := change.Validate(); err != nil {
		return ArtifactIntegrityChange{}, fmt.Errorf("validate ArtifactIntegrityChange: %w", err)
	}
	return change, nil
}

func (use ArtifactUse) Validate() error {
	switch use {
	case ArtifactUseRead, ArtifactUseCandidatePool, ArtifactUsePublication, ArtifactUseEvaluation,
		ArtifactUseTraining, ArtifactUseExport:
		return nil
	default:
		return fmt.Errorf("unsupported artifact use %q", use)
	}
}

// CheckArtifactEligibility is the common fail-closed gate used before an
// Argus artifact may be read, published, evaluated, trained on, or exported.
// The use is validated even though every non-active state is denied today, so
// future policy expansion cannot silently turn an unknown use into access.
func (repository *Repository) CheckArtifactEligibility(
	ref runmodel.ArtifactRef,
	use ArtifactUse,
) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := use.Validate(); err != nil {
		return err
	}
	record, err := repository.InspectArtifactIntegrity(ref)
	if err != nil {
		return err
	}
	switch record.State {
	case ArtifactIntegrityActive:
		return nil
	case ArtifactIntegrityQuarantined:
		return fmt.Errorf("%w: use %q denied", ErrArtifactQuarantined, use)
	case ArtifactIntegrityTombstoned:
		return fmt.Errorf("%w: use %q denied", ErrArtifactTombstoned, use)
	default:
		return fmt.Errorf("%w: unsupported state %q", ErrArtifactIntegrity, record.State)
	}
}

func (repository *Repository) InspectArtifactIntegrity(
	ref runmodel.ArtifactRef,
) (ArtifactIntegrityRecord, error) {
	if err := ref.Validate(); err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	return repository.loadArtifactIntegrity(ref)
}

func (repository *Repository) QuarantineArtifact(
	ctx context.Context,
	ref runmodel.ArtifactRef,
	reason string,
	mutation ArtifactIntegrityMutation,
) (ArtifactIntegrityRecord, error) {
	return repository.changeArtifactIntegrity(
		ctx, ref, artifactIntegrityQuarantined, reason, mutation,
	)
}

func (repository *Repository) ReleaseArtifactQuarantine(
	ctx context.Context,
	ref runmodel.ArtifactRef,
	reason string,
	mutation ArtifactIntegrityMutation,
) (ArtifactIntegrityRecord, error) {
	if err := repository.validateArtifactIntegrityMutation(ctx, ref, reason, mutation); err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	current, err := repository.loadArtifactIntegrity(ref)
	if err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	if replay, found, replayErr := repository.replayArtifactIntegrityMutation(
		ref, artifactIntegrityReleased, reason, mutation,
	); found || replayErr != nil {
		return replay, replayErr
	}
	if current.State == ArtifactIntegrityTombstoned {
		return ArtifactIntegrityRecord{}, ErrArtifactTombstoned
	}
	if current.State != ArtifactIntegrityQuarantined {
		return ArtifactIntegrityRecord{}, fmt.Errorf(
			"%w: release requires quarantined state", ErrArtifactTransition,
		)
	}
	if _, err := repository.readArtifactWithoutLifecycle(ref); err != nil {
		return ArtifactIntegrityRecord{}, fmt.Errorf("artifact integrity is not restored: %w", err)
	}
	record, err := repository.appendArtifactIntegrityEvent(
		ref, artifactIntegrityReleased, reason, mutation, current.Sequence,
	)
	if err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	if _, err := repository.readArtifactWithoutLifecycle(ref); err != nil {
		_, quarantineErr := repository.QuarantineArtifact(
			context.Background(), ref, "post_release_verification_failed",
			ArtifactIntegrityMutation{
				IdempotencyKey: mutation.IdempotencyKey + "-post-release-quarantine",
				Actor:          "system-integrity", Audit: "post-release checksum verification",
				At: mutation.At,
			},
		)
		if quarantineErr != nil {
			return record, fmt.Errorf("post-release verification failed: %v; quarantine: %w", err, quarantineErr)
		}
		current, loadErr := repository.loadArtifactIntegrity(ref)
		if loadErr != nil {
			return record, fmt.Errorf("post-release verification failed: %v; reload: %w", err, loadErr)
		}
		return current, fmt.Errorf("%w: post-release verification failed", ErrArtifactQuarantined)
	}
	return record, nil
}

func (repository *Repository) TombstoneArtifact(
	ctx context.Context,
	ref runmodel.ArtifactRef,
	reason string,
	mutation ArtifactIntegrityMutation,
) (ArtifactIntegrityRecord, error) {
	return repository.changeArtifactIntegrity(
		ctx, ref, artifactIntegrityTombstoned, reason, mutation,
	)
}

func (repository *Repository) changeArtifactIntegrity(
	ctx context.Context,
	ref runmodel.ArtifactRef,
	typeValue artifactIntegrityEventType,
	reason string,
	mutation ArtifactIntegrityMutation,
) (ArtifactIntegrityRecord, error) {
	if err := repository.validateArtifactIntegrityMutation(ctx, ref, reason, mutation); err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	current, err := repository.loadArtifactIntegrity(ref)
	if err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	if replay, found, replayErr := repository.replayArtifactIntegrityMutation(
		ref, typeValue, reason, mutation,
	); found || replayErr != nil {
		return replay, replayErr
	}
	if current.State == ArtifactIntegrityTombstoned {
		return ArtifactIntegrityRecord{}, ErrArtifactTombstoned
	}
	if typeValue == artifactIntegrityQuarantined && current.State != ArtifactIntegrityActive {
		return ArtifactIntegrityRecord{}, fmt.Errorf(
			"%w: quarantine requires active state", ErrArtifactTransition,
		)
	}
	return repository.appendArtifactIntegrityEvent(
		ref, typeValue, reason, mutation, current.Sequence,
	)
}

func (repository *Repository) validateArtifactIntegrityMutation(
	ctx context.Context,
	ref runmodel.ArtifactRef,
	reason string,
	mutation ArtifactIntegrityMutation,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := validateIntegrityText("reason", reason); err != nil {
		return err
	}
	return mutation.Validate()
}

func (repository *Repository) appendArtifactIntegrityEvent(
	ref runmodel.ArtifactRef,
	typeValue artifactIntegrityEventType,
	reason string,
	mutation ArtifactIntegrityMutation,
	expectedSequence uint64,
) (ArtifactIntegrityRecord, error) {
	event := artifactIntegrityEvent{
		SchemaVersion: ArtifactIntegrityEventSchemaVersion,
		Type:          typeValue, Ref: ref, Reason: reason, Mutation: mutation,
	}
	_, err := repository.store.AppendJSONLAtSequence(
		artifactIntegrityStream(ref), expectedSequence, local.Event{
			ID: mutation.IdempotencyKey, Schema: ArtifactIntegrityEventSchemaVersion,
			Time: mutation.At, Payload: event,
		},
	)
	if err != nil {
		if replay, found, replayErr := repository.replayArtifactIntegrityMutation(
			ref, typeValue, reason, mutation,
		); found || replayErr != nil {
			return replay, replayErr
		}
		return ArtifactIntegrityRecord{}, fmt.Errorf("append artifact integrity event: %w", err)
	}
	return repository.loadArtifactIntegrity(ref)
}

func (repository *Repository) replayArtifactIntegrityMutation(
	ref runmodel.ArtifactRef,
	typeValue artifactIntegrityEventType,
	reason string,
	mutation ArtifactIntegrityMutation,
) (ArtifactIntegrityRecord, bool, error) {
	envelopes, err := repository.store.ReadJSONL(artifactIntegrityStream(ref))
	if errors.Is(err, os.ErrNotExist) {
		return ArtifactIntegrityRecord{}, false, nil
	}
	if err != nil {
		return ArtifactIntegrityRecord{}, false, fmt.Errorf("%w: %v", ErrArtifactIntegrity, err)
	}
	want := artifactIntegrityEvent{
		SchemaVersion: ArtifactIntegrityEventSchemaVersion,
		Type:          typeValue, Ref: ref, Reason: reason, Mutation: mutation,
	}
	for _, envelope := range envelopes {
		if envelope.ID != mutation.IdempotencyKey {
			continue
		}
		var got artifactIntegrityEvent
		if err := decodeStrictJSON(envelope.Payload, &got); err != nil {
			return ArtifactIntegrityRecord{}, false, fmt.Errorf("%w: decode idempotent event: %v", ErrArtifactIntegrity, err)
		}
		if !reflect.DeepEqual(got, want) || !envelope.Time.Equal(mutation.At) {
			return ArtifactIntegrityRecord{}, false, fmt.Errorf(
				"%w: idempotency key %q has different content",
				ErrArtifactTransition, mutation.IdempotencyKey,
			)
		}
		record, err := repository.loadArtifactIntegrity(ref)
		return record, true, err
	}
	return ArtifactIntegrityRecord{}, false, nil
}

func (repository *Repository) loadArtifactIntegrity(
	ref runmodel.ArtifactRef,
) (ArtifactIntegrityRecord, error) {
	record := ArtifactIntegrityRecord{Ref: ref, State: ArtifactIntegrityActive}
	envelopes, err := repository.store.ReadJSONL(artifactIntegrityStream(ref))
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return ArtifactIntegrityRecord{}, fmt.Errorf("%w: read ledger: %v", ErrArtifactIntegrity, err)
	}
	for index, envelope := range envelopes {
		if envelope.Schema != ArtifactIntegrityEventSchemaVersion {
			return ArtifactIntegrityRecord{}, fmt.Errorf("%w: event %d schema mismatch", ErrArtifactIntegrity, index)
		}
		var event artifactIntegrityEvent
		if err := decodeStrictJSON(envelope.Payload, &event); err != nil {
			return ArtifactIntegrityRecord{}, fmt.Errorf("%w: decode event %d: %v", ErrArtifactIntegrity, index, err)
		}
		if envelope.ID != event.Mutation.IdempotencyKey ||
			!envelope.Time.Equal(event.Mutation.At) || !reflect.DeepEqual(event.Ref, ref) {
			return ArtifactIntegrityRecord{}, fmt.Errorf("%w: event %d binding mismatch", ErrArtifactIntegrity, index)
		}
		record, err = applyArtifactIntegrityEvent(record, event, envelope.Sequence)
		if err != nil {
			return ArtifactIntegrityRecord{}, fmt.Errorf("%w: event %d: %v", ErrArtifactIntegrity, index, err)
		}
	}
	return record, nil
}

func applyArtifactIntegrityEvent(
	record ArtifactIntegrityRecord,
	event artifactIntegrityEvent,
	sequence uint64,
) (ArtifactIntegrityRecord, error) {
	if event.SchemaVersion != ArtifactIntegrityEventSchemaVersion {
		return ArtifactIntegrityRecord{}, fmt.Errorf("unsupported schema %q", event.SchemaVersion)
	}
	if err := event.Ref.Validate(); err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	if err := event.Mutation.Validate(); err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	if err := validateIntegrityText("reason", event.Reason); err != nil {
		return ArtifactIntegrityRecord{}, err
	}
	if sequence != record.Sequence+1 {
		return ArtifactIntegrityRecord{}, fmt.Errorf("sequence is %d, want %d", sequence, record.Sequence+1)
	}
	switch event.Type {
	case artifactIntegrityQuarantined:
		if record.State != ArtifactIntegrityActive {
			return ArtifactIntegrityRecord{}, fmt.Errorf("quarantine requires active state")
		}
		record.State = ArtifactIntegrityQuarantined
	case artifactIntegrityReleased:
		if record.State != ArtifactIntegrityQuarantined {
			return ArtifactIntegrityRecord{}, fmt.Errorf("release requires quarantined state")
		}
		record.State = ArtifactIntegrityActive
	case artifactIntegrityTombstoned:
		if record.State == ArtifactIntegrityTombstoned {
			return ArtifactIntegrityRecord{}, fmt.Errorf("tombstone is irreversible")
		}
		record.State = ArtifactIntegrityTombstoned
	default:
		return ArtifactIntegrityRecord{}, fmt.Errorf("unsupported event type %q", event.Type)
	}
	at := event.Mutation.At
	record.Ref = event.Ref
	record.StateReason = event.Reason
	record.StateChangedAt = &at
	record.LastMutationActor = event.Mutation.Actor
	record.Sequence = sequence
	return record, nil
}

func (repository *Repository) autoQuarantineArtifact(ref runmodel.ArtifactRef) error {
	current, err := repository.loadArtifactIntegrity(ref)
	if err != nil {
		return err
	}
	if current.State != ArtifactIntegrityActive {
		return nil
	}
	at := repository.now().UTC()
	if at.IsZero() {
		return fmt.Errorf("repository clock returned zero")
	}
	mutation := ArtifactIntegrityMutation{
		IdempotencyKey: "auto-quarantine-" + artifactIntegrityIdentity(ref) + "-" +
			strconv.FormatUint(current.Sequence, 10),
		Actor: "system-integrity", Audit: "automatic content checksum verification",
		At: at,
	}
	_, err = repository.QuarantineArtifact(
		context.Background(), ref, "content_verification_failed", mutation,
	)
	if errors.Is(err, ErrArtifactTransition) {
		reloaded, reloadErr := repository.loadArtifactIntegrity(ref)
		if reloadErr == nil && reloaded.State != ArtifactIntegrityActive {
			return nil
		}
	}
	return err
}

func (repository *Repository) readArtifactWithoutLifecycle(
	ref runmodel.ArtifactRef,
) ([]byte, error) {
	return repository.store.ReadArtifact(local.ArtifactRef{
		URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
	})
}

func artifactIntegrityStream(ref runmodel.ArtifactRef) string {
	return "runrepo/artifact-integrity/" + artifactIntegrityIdentity(ref)
}

func artifactIntegrityIdentity(ref runmodel.ArtifactRef) string {
	value := strings.Join([]string{
		ref.URI, ref.SHA256, strconv.FormatInt(ref.SizeBytes, 10), ref.Contract,
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validateIntegrityText(name, value string) error {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) ||
		len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s must be non-empty, trimmed, single-line text of at most 1024 bytes", name)
	}
	return nil
}
