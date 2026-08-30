package piexecution

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const piGroupCheckpointEventSchema = "argus.pi_group_checkpoint_event.v1alpha1"

var errPiGroupCheckpointFenced = errors.New("Pi group checkpoint generation fenced")

type piGroupCheckpointEvent struct {
	SchemaVersion         string             `json:"schema_version"`
	Type                  string             `json:"type"`
	CheckpointScopeSHA256 string             `json:"checkpoint_scope_sha256"`
	ReviewRunID           string             `json:"review_run_id"`
	Generation            int                `json:"generation"`
	FencingToken          uint64             `json:"fencing_token"`
	ExecutionID           string             `json:"execution_id"`
	GroupID               string             `json:"group_id,omitempty"`
	CheckpointRevision    uint64             `json:"checkpoint_revision,omitempty"`
	Content               *local.ArtifactRef `json:"content,omitempty"`
	OccurredAt            time.Time          `json:"occurred_at"`
}

type piGroupCheckpointRecord struct {
	active    *piGroupCheckpointEvent
	bound     map[int]piGroupCheckpointEvent
	completed map[string]piGroupCheckpointEvent
}

type piGroupCheckpointRepository struct{ store *local.Store }

func newPiGroupCheckpointRepository(store *local.Store) (*piGroupCheckpointRepository, error) {
	if store == nil {
		return nil, fmt.Errorf("Pi group checkpoint store is required")
	}
	return &piGroupCheckpointRepository{store: store}, nil
}

func (repository *piGroupCheckpointRepository) BindGeneration(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	event := piGroupCheckpointEvent{
		SchemaVersion:         piGroupCheckpointEventSchema,
		Type:                  "generation_bound",
		CheckpointScopeSHA256: checkpointScope(request),
		ReviewRunID:           request.ReviewRunID,
		Generation:            request.Generation,
		FencingToken:          request.FencingToken,
		ExecutionID:           request.ExecutionID,
		OccurredAt:            time.Now().UTC(),
	}
	stream := piGroupCheckpointStream(event.CheckpointScopeSHA256)
	for range 128 {
		envelopes, err := repository.readStream(stream)
		if err != nil {
			return err
		}
		record, err := projectPiGroupCheckpoints(envelopes, event.CheckpointScopeSHA256)
		if err != nil {
			return err
		}
		if record.active != nil {
			if samePiGroupGeneration(*record.active, event) {
				return nil
			}
			if event.Generation <= record.active.Generation {
				return errPiGroupCheckpointFenced
			}
		}
		_, err = repository.store.AppendJSONLAtSequence(stream, uint64(len(envelopes)), local.Event{
			ID:     fmt.Sprintf("generation-%d-%d", event.Generation, event.FencingToken),
			Schema: piGroupCheckpointEventSchema, Time: event.OccurredAt, Payload: event,
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, local.ErrEventConflict) {
			return err
		}
	}
	return fmt.Errorf("Pi group checkpoint generation CAS did not converge")
}

func (repository *piGroupCheckpointRepository) ReusableBefore(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) ([]contractsv1alpha1.AgentReviewWorkerGroupCheckpoint, error) {
	checkpoints, _, err := repository.reusableBefore(ctx, request)
	return checkpoints, err
}

// reusableBefore returns both the exact prior-generation checkpoints and the
// earliest host-recorded generation binding that contributed reusable work.
// The latter is the trusted lower bound of the logical resumed worker window;
// without it, prior-generation task observations would appear to precede a
// freshly derived generation-2 worker plan and fail strict receipt mapping.
func (repository *piGroupCheckpointRepository) reusableBefore(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) ([]contractsv1alpha1.AgentReviewWorkerGroupCheckpoint, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return nil, time.Time{}, err
	}
	scope := checkpointScope(request)
	envelopes, err := repository.readStream(piGroupCheckpointStream(scope))
	if err != nil {
		return nil, time.Time{}, err
	}
	record, err := projectPiGroupCheckpoints(envelopes, scope)
	if err != nil {
		return nil, time.Time{}, err
	}
	result := make([]contractsv1alpha1.AgentReviewWorkerGroupCheckpoint, 0, len(record.completed))
	var earliestBound time.Time
	for _, event := range record.completed {
		if event.Generation >= request.Generation || event.Content == nil {
			continue
		}
		bound, exists := record.bound[event.Generation]
		if !exists || !samePiGroupGeneration(bound, event) {
			return nil, time.Time{}, fmt.Errorf(
				"Pi group checkpoint %q has no exact origin generation binding",
				event.GroupID,
			)
		}
		if earliestBound.IsZero() || bound.OccurredAt.Before(earliestBound) {
			earliestBound = bound.OccurredAt
		}
		content, readErr := repository.store.ReadArtifact(*event.Content)
		if readErr != nil {
			return nil, time.Time{}, readErr
		}
		checkpoint := contractsv1alpha1.AgentReviewWorkerGroupCheckpoint{
			GroupID: event.GroupID, CheckpointRevision: event.CheckpointRevision,
			CheckpointScopeSHA256: scope,
			ContentSHA256:         event.Content.SHA256, SizeBytes: uint64(event.Content.SizeBytes),
			ContentBase64: base64.StdEncoding.EncodeToString(content),
		}
		if err := checkpoint.Validate(); err != nil {
			return nil, time.Time{}, err
		}
		result = append(result, checkpoint)
	}
	slices.SortFunc(result, func(left, right contractsv1alpha1.AgentReviewWorkerGroupCheckpoint) int {
		if left.GroupID < right.GroupID {
			return -1
		}
		if left.GroupID > right.GroupID {
			return 1
		}
		return 0
	})
	return result, earliestBound, nil
}

func (repository *piGroupCheckpointRepository) Record(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
	checkpoint contractsv1alpha1.AgentReviewWorkerGroupCheckpoint,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	scope := checkpointScope(request)
	if checkpoint.CheckpointScopeSHA256 != scope {
		return fmt.Errorf("Pi group checkpoint escaped request scope")
	}
	content, err := base64.StdEncoding.Strict().DecodeString(checkpoint.ContentBase64)
	if err != nil {
		return err
	}
	ref, err := repository.store.PutArtifact(content)
	if err != nil {
		return err
	}
	event := piGroupCheckpointEvent{
		SchemaVersion: piGroupCheckpointEventSchema, Type: "checkpoint_recorded",
		CheckpointScopeSHA256: scope, ReviewRunID: request.ReviewRunID,
		Generation: request.Generation, FencingToken: request.FencingToken,
		ExecutionID: request.ExecutionID, GroupID: checkpoint.GroupID,
		CheckpointRevision: checkpoint.CheckpointRevision,
		Content:            &ref, OccurredAt: time.Now().UTC(),
	}
	stream := piGroupCheckpointStream(scope)
	for range 128 {
		envelopes, readErr := repository.readStream(stream)
		if readErr != nil {
			return readErr
		}
		record, projectErr := projectPiGroupCheckpoints(envelopes, scope)
		if projectErr != nil {
			return projectErr
		}
		if record.active == nil || !samePiGroupGeneration(*record.active, event) {
			return errPiGroupCheckpointFenced
		}
		if prior, exists := record.completed[event.GroupID]; exists {
			if event.CheckpointRevision < prior.CheckpointRevision {
				// Concurrent verifier callbacks can durably arrive out of order.
				// A later cumulative revision already subsumes this one.
				return nil
			}
			if event.CheckpointRevision == prior.CheckpointRevision &&
				prior.Content != nil && *prior.Content == ref {
				return nil
			}
			if event.CheckpointRevision == prior.CheckpointRevision {
				return fmt.Errorf("Pi group checkpoint %q revision %d conflicts with prior content", event.GroupID, event.CheckpointRevision)
			}
		}
		_, appendErr := repository.store.AppendJSONLAtSequence(stream, uint64(len(envelopes)), local.Event{
			ID:     fmt.Sprintf("checkpoint-%s-%d-%s", event.GroupID, event.CheckpointRevision, ref.SHA256),
			Schema: piGroupCheckpointEventSchema, Time: event.OccurredAt, Payload: event,
		})
		if appendErr == nil {
			return nil
		}
		if !errors.Is(appendErr, local.ErrEventConflict) {
			return appendErr
		}
	}
	return fmt.Errorf("Pi group checkpoint CAS did not converge")
}

func (platform *Platform) recordWorkerProgress(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
	line []byte,
) error {
	var envelope struct {
		SchemaVersion string          `json:"schema_version"`
		WorkItemID    string          `json:"work_item_id"`
		Phase         string          `json:"phase"`
		GroupID       string          `json:"group_id"`
		Checkpoint    json.RawMessage `json:"checkpoint"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("decode Pi worker progress: %w", err)
	}
	if envelope.Phase != "checkpoint" {
		return nil
	}
	if envelope.SchemaVersion != "argus.agent_review_worker_progress.v1alpha1" ||
		envelope.WorkItemID != request.ExecutionID || envelope.GroupID == "" ||
		len(envelope.Checkpoint) == 0 {
		return fmt.Errorf("Pi worker checkpoint progress identity is invalid")
	}
	var checkpoint contractsv1alpha1.AgentReviewWorkerGroupCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(envelope.Checkpoint))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return fmt.Errorf("decode Pi worker group checkpoint: %w", err)
	}
	if checkpoint.GroupID != envelope.GroupID {
		return fmt.Errorf("Pi worker checkpoint group identity changed")
	}
	return platform.groupCheckpoints.Record(context.WithoutCancel(ctx), request, checkpoint)
}

func (repository *piGroupCheckpointRepository) readStream(stream string) ([]local.Envelope, error) {
	envelopes, err := repository.store.ReadJSONL(stream)
	if errors.Is(err, os.ErrNotExist) {
		return []local.Envelope{}, nil
	}
	return envelopes, err
}

func projectPiGroupCheckpoints(
	envelopes []local.Envelope,
	scope string,
) (piGroupCheckpointRecord, error) {
	record := piGroupCheckpointRecord{
		bound: make(map[int]piGroupCheckpointEvent), completed: make(map[string]piGroupCheckpointEvent),
	}
	for index, envelope := range envelopes {
		if envelope.Schema != piGroupCheckpointEventSchema {
			return record, fmt.Errorf("Pi group checkpoint event %d has unsupported schema", index)
		}
		var event piGroupCheckpointEvent
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return record, err
		}
		if event.SchemaVersion != piGroupCheckpointEventSchema ||
			event.CheckpointScopeSHA256 != scope || event.Generation < 1 ||
			event.FencingToken == 0 || event.ExecutionID == "" || event.ReviewRunID == "" ||
			event.OccurredAt.IsZero() {
			return record, fmt.Errorf("Pi group checkpoint event %d is invalid", index)
		}
		switch event.Type {
		case "generation_bound":
			if event.GroupID != "" || event.CheckpointRevision != 0 || event.Content != nil ||
				(record.active != nil && event.Generation <= record.active.Generation) ||
				record.bound[event.Generation].Generation != 0 {
				return record, fmt.Errorf("Pi group checkpoint generation transition is invalid")
			}
			copy := event
			record.bound[event.Generation] = copy
			record.active = &copy
		case "checkpoint_recorded":
			bound, boundExists := record.bound[event.Generation]
			if record.active == nil || !boundExists ||
				!samePiGroupGeneration(bound, event) ||
				!samePiGroupGeneration(*record.active, event) ||
				event.OccurredAt.Before(bound.OccurredAt) ||
				event.GroupID == "" || event.Content == nil {
				return record, fmt.Errorf("Pi group checkpoint event is outside active generation")
			}
			if prior, exists := record.completed[event.GroupID]; exists &&
				event.CheckpointRevision <= prior.CheckpointRevision {
				return record, fmt.Errorf("Pi group checkpoint %q revision is not monotonic", event.GroupID)
			}
			record.completed[event.GroupID] = event
		default:
			return record, fmt.Errorf("unsupported Pi group checkpoint event type %q", event.Type)
		}
	}
	return record, nil
}

func samePiGroupGeneration(left, right piGroupCheckpointEvent) bool {
	return left.CheckpointScopeSHA256 == right.CheckpointScopeSHA256 &&
		left.ReviewRunID == right.ReviewRunID && left.Generation == right.Generation &&
		left.FencingToken == right.FencingToken && left.ExecutionID == right.ExecutionID
}

func checkpointScope(request contractsv1alpha1.StageExecutionRequest) string {
	return shaHex([]byte(request.Plan.Ref.SHA256 + "\n" + request.ReviewInput.Ref.SHA256))
}

func piGroupCheckpointStream(scope string) string { return "pi/group-checkpoints/" + scope }
