package reviewjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
)

type Repository struct {
	store *local.Store
	mu    *sync.Mutex
}

var repositoryLocks sync.Map

type jobEvent struct {
	SchemaVersion string      `json:"schema_version"`
	Type          string      `json:"type"`
	Submission    *Submission `json:"submission,omitempty"`
}

type projection struct {
	byID          map[string]Submission
	byIdempotency map[string]Submission
	sequence      uint64
}

func NewRepository(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	value, _ := repositoryLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &Repository{store: store, mu: value.(*sync.Mutex)}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Create(
	ctx context.Context,
	request Request,
	mutation Mutation,
	bundle reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) (Submission, Command, error) {
	jobID, runID := deterministicIDs(mutation.Actor, mutation.IdempotencyKey)
	return repository.create(ctx, request, mutation, bundle, receipt, jobID, runID, nil)
}

func (repository *Repository) CreateFormal(
	ctx context.Context,
	request Request,
	mutation Mutation,
	bundle reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
	runtime FormalRuntimeBinding,
) (Submission, Command, error) {
	jobID, _ := deterministicIDs(mutation.Actor, mutation.IdempotencyKey)
	runID := formalreview.FormalRunID(request.SourceRunID, mutation.IdempotencyKey)
	// Publications is an in-memory convenience omitted from the immutable
	// command JSON. Normalize the by-value copy before identity comparisons so
	// a concurrent exact submission matches a command loaded from persistence.
	// Components remains the authoritative, content-bearing runtime closure.
	runtime.Bootstrap.Publications = nil
	return repository.create(ctx, request, mutation, bundle, receipt, jobID, runID, &runtime)
}

func (repository *Repository) create(
	ctx context.Context,
	request Request,
	mutation Mutation,
	bundle reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
	jobID string,
	runID string,
	runtime *FormalRuntimeBinding,
) (Submission, Command, error) {
	if err := contextError(ctx); err != nil {
		return Submission{}, Command{}, err
	}
	if err := request.Validate(); err != nil {
		return Submission{}, Command{}, err
	}
	if len(request.Contexts) == 0 {
		request.Contexts = nil
	}
	if err := mutation.Validate(); err != nil {
		return Submission{}, Command{}, err
	}
	command := Command{
		SchemaVersion: CommandSchemaVersion,
		JobID:         jobID, RunID: runID, ExecutionProfile: request.ExecutionProfile,
		Request: request, ConfigBundle: bundle, ConfigReceipt: receipt,
		FormalRuntime: runtime,
		Actor:         mutation.Actor, IdempotencyKey: mutation.IdempotencyKey,
		Audit: mutation.Audit, SubmittedAt: mutation.At,
	}
	if err := command.Validate(); err != nil {
		return Submission{}, Command{}, err
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Submission{}, Command{}, err
	}
	key := idempotencyLookupKey(mutation.Actor, mutation.IdempotencyKey)
	if existing, ok := state.byIdempotency[key]; ok {
		frozen, loadErr := repository.loadCommand(existing.CommandRef)
		if loadErr != nil {
			return Submission{}, Command{}, loadErr
		}
		if !reflect.DeepEqual(frozen, command) {
			return Submission{}, Command{}, fmt.Errorf(
				"%w: idempotency key %q was reused with different intent",
				ErrConflict,
				mutation.IdempotencyKey,
			)
		}
		return existing, frozen, nil
	}
	if _, exists := state.byID[jobID]; exists {
		return Submission{}, Command{}, fmt.Errorf("%w: job_id %q already exists", ErrConflict, jobID)
	}
	commandRef, err := repository.putJSONArtifact(CommandSchemaVersion, command)
	if err != nil {
		return Submission{}, Command{}, err
	}
	bundleRef, err := repository.putJSONArtifact(reviewconfig.BundleSchemaVersion, bundle)
	if err != nil {
		return Submission{}, Command{}, err
	}
	receiptRef, err := repository.putJSONArtifact(
		reviewconfig.ConfigResolutionReceiptSchemaVersion,
		receipt,
	)
	if err != nil {
		return Submission{}, Command{}, err
	}
	submission := Submission{
		JobID: jobID, RunID: runID, WorkloadID: runID + "-workload",
		CommandRef: commandRef, ConfigBundleRef: bundleRef,
		ConfigReceiptRef: receiptRef, Request: request,
		Actor: mutation.Actor, Audit: mutation.Audit, SubmittedAt: mutation.At,
		IdempotencyKey: mutation.IdempotencyKey,
	}
	event := jobEvent{
		SchemaVersion: EventSchemaVersion,
		Type:          "submitted",
		Submission:    &submission,
	}
	envelope, _, err := repository.store.AppendJSONLAtSequenceWithStatus(
		jobStream,
		state.sequence,
		local.Event{
			ID: jobID + "-submitted", Schema: EventSchemaVersion,
			Time: mutation.At, Payload: event,
		},
	)
	if err != nil {
		return Submission{}, Command{}, err
	}
	submission.Sequence = envelope.Sequence
	return submission, command, nil
}

func (repository *Repository) Get(jobID string) (Submission, error) {
	if err := validateID("job_id", jobID); err != nil {
		return Submission{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Submission{}, err
	}
	submission, ok := state.byID[jobID]
	if !ok {
		return Submission{}, fmt.Errorf("%w: job %q", ErrNotFound, jobID)
	}
	return cloneSubmission(submission), nil
}

func (repository *Repository) GetByIdempotency(
	actor string,
	idempotencyKey string,
) (Submission, Command, error) {
	if err := validateID("actor", actor); err != nil {
		return Submission{}, Command{}, err
	}
	if err := validateID("idempotency_key", idempotencyKey); err != nil {
		return Submission{}, Command{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return Submission{}, Command{}, err
	}
	submission, ok := state.byIdempotency[idempotencyLookupKey(actor, idempotencyKey)]
	if !ok {
		return Submission{}, Command{}, fmt.Errorf("%w: idempotency key", ErrNotFound)
	}
	command, err := repository.loadCommand(submission.CommandRef)
	return cloneSubmission(submission), command, err
}

func (repository *Repository) List() ([]Submission, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]Submission, 0, len(state.byID))
	for _, submission := range state.byID {
		result = append(result, cloneSubmission(submission))
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Sequence > result[right].Sequence
	})
	return result, nil
}

func (repository *Repository) LoadCommand(ref runmodel.ArtifactRef) (Command, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.loadCommand(ref)
}

func (repository *Repository) loadCommand(ref runmodel.ArtifactRef) (Command, error) {
	if ref.Contract != CommandSchemaVersion {
		return Command{}, fmt.Errorf("%w: command artifact contract is %q", ErrCorrupt, ref.Contract)
	}
	data, err := repository.store.ReadArtifact(local.ArtifactRef{
		URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
	})
	if err != nil {
		return Command{}, fmt.Errorf("%w: read command artifact: %v", ErrCorrupt, err)
	}
	var command Command
	if err := decodeStrict(data, &command); err != nil {
		return Command{}, fmt.Errorf("%w: decode command: %v", ErrCorrupt, err)
	}
	if err := command.Validate(); err != nil {
		return Command{}, fmt.Errorf("%w: validate command: %v", ErrCorrupt, err)
	}
	return command, nil
}

func (repository *Repository) putJSONArtifact(
	contract string,
	value any,
) (runmodel.ArtifactRef, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("marshal %s artifact: %w", contract, err)
	}
	stored, err := repository.store.PutArtifact(data)
	if err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("persist %s artifact: %w", contract, err)
	}
	ref := runmodel.ArtifactRef{
		URI: stored.URI, SHA256: stored.SHA256, SizeBytes: stored.SizeBytes,
		Contract: contract,
	}
	if err := ref.Validate(); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return ref, nil
}

func (repository *Repository) load() (projection, error) {
	state := projection{
		byID: make(map[string]Submission), byIdempotency: make(map[string]Submission),
	}
	envelopes, err := repository.store.ReadJSONL(jobStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return projection{}, err
	}
	for _, envelope := range envelopes {
		if envelope.Schema != EventSchemaVersion {
			return projection{}, fmt.Errorf("%w: unsupported event schema %q", ErrCorrupt, envelope.Schema)
		}
		var event jobEvent
		if err := decodeStrict(envelope.Payload, &event); err != nil {
			return projection{}, fmt.Errorf("%w: decode event %d: %v", ErrCorrupt, envelope.Sequence, err)
		}
		if event.SchemaVersion != EventSchemaVersion || event.Type != "submitted" || event.Submission == nil {
			return projection{}, fmt.Errorf("%w: invalid event %d shape", ErrCorrupt, envelope.Sequence)
		}
		submission := cloneSubmission(*event.Submission)
		if submission.Sequence != 0 || submission.SubmittedAt != envelope.Time ||
			submission.JobID+"-submitted" != envelope.ID {
			return projection{}, fmt.Errorf("%w: event %d identity mismatch", ErrCorrupt, envelope.Sequence)
		}
		if err := validateSubmission(submission); err != nil {
			return projection{}, fmt.Errorf("%w: event %d: %v", ErrCorrupt, envelope.Sequence, err)
		}
		submission.Sequence = envelope.Sequence
		key := idempotencyLookupKey(submission.Actor, submission.IdempotencyKey)
		if _, exists := state.byID[submission.JobID]; exists {
			return projection{}, fmt.Errorf("%w: duplicate job_id %q", ErrCorrupt, submission.JobID)
		}
		if _, exists := state.byIdempotency[key]; exists {
			return projection{}, fmt.Errorf("%w: duplicate idempotency key", ErrCorrupt)
		}
		state.byID[submission.JobID] = submission
		state.byIdempotency[key] = submission
		state.sequence = envelope.Sequence
	}
	return state, nil
}

func validateSubmission(submission Submission) error {
	for name, value := range map[string]string{
		"job_id": submission.JobID, "run_id": submission.RunID,
		"workload_id": submission.WorkloadID,
		"actor":       submission.Actor, "idempotency_key": submission.IdempotencyKey,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	wantJobID, deterministicRunID := deterministicIDs(submission.Actor, submission.IdempotencyKey)
	wantRunID := deterministicRunID
	if submission.Request.ExecutionProfile == FormalPiExecutionProfile {
		wantRunID = formalreview.FormalRunID(
			submission.Request.SourceRunID, submission.IdempotencyKey,
		)
	}
	if submission.WorkloadID != wantRunID+"-workload" {
		return fmt.Errorf("workload_id does not bind run_id")
	}
	if submission.JobID != wantJobID || submission.RunID != wantRunID {
		return fmt.Errorf("job/run identity does not bind actor and idempotency key")
	}
	if err := submission.Request.Validate(); err != nil {
		return err
	}
	if err := submission.CommandRef.Validate(); err != nil {
		return err
	}
	if submission.CommandRef.Contract != CommandSchemaVersion {
		return fmt.Errorf("command_ref has invalid contract")
	}
	if err := submission.ConfigBundleRef.Validate(); err != nil {
		return err
	}
	if submission.ConfigBundleRef.Contract != reviewconfig.BundleSchemaVersion {
		return fmt.Errorf("config_bundle_ref has invalid contract")
	}
	if err := submission.ConfigReceiptRef.Validate(); err != nil {
		return err
	}
	if submission.ConfigReceiptRef.Contract != reviewconfig.ConfigResolutionReceiptSchemaVersion {
		return fmt.Errorf("config_resolution_receipt_ref has invalid contract")
	}
	if err := validateText("audit", submission.Audit, 4096); err != nil {
		return err
	}
	if submission.SubmittedAt.IsZero() || submission.SubmittedAt.Location() != time.UTC {
		return fmt.Errorf("submitted_at must be non-zero UTC")
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func idempotencyLookupKey(actor, key string) string { return actor + "\x00" + key }

func cloneSubmission(submission Submission) Submission {
	submission.Request.SelectionRanges = slices.Clone(submission.Request.SelectionRanges)
	submission.Request.SelectionSymbol = cloneSymbol(submission.Request.SelectionSymbol)
	submission.Request.OverlayContent = cloneString(submission.Request.OverlayContent)
	submission.Request.Contexts = cloneContexts(submission.Request.Contexts)
	submission.Request.Include = slices.Clone(submission.Request.Include)
	submission.Request.Exclude = slices.Clone(submission.Request.Exclude)
	return submission
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
