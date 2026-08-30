package training

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

type ExportSource interface {
	Get(string, evaluation.Access) (ExportRecord, error)
}

type JobService struct {
	store     *local.Store
	exports   ExportSource
	artifacts ArtifactSource
	mu        *sync.Mutex
}

var jobServiceLocks sync.Map

func NewJobService(store *local.Store, exports ExportSource, artifacts ArtifactSource) (*JobService, error) {
	if store == nil || exports == nil || artifacts == nil {
		return nil, fmt.Errorf("training job store, export source, and artifact source are required")
	}
	value, _ := jobServiceLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	service := &JobService{store: store, exports: exports, artifacts: artifacts, mu: value.(*sync.Mutex)}
	if _, err := service.load(); err != nil {
		return nil, err
	}
	return service, nil
}

func (service *JobService) Prepare(ctx context.Context, request JobPrepareRequest, mutation evaluation.Mutation) (JobRecord, error) {
	if err := validateJobMutation(ctx, mutation, request.CreatedAt); err != nil {
		return JobRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return JobRecord{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	state, err := service.load()
	if err != nil {
		return JobRecord{}, err
	}
	if existing, ok := state.byIdempotency[mutation.IdempotencyKey]; ok {
		if existing.event.Type != "prepared" || !reflect.DeepEqual(existing.event.Prepare, &request) || !reflect.DeepEqual(existing.event.Mutation, mutation) {
			return JobRecord{}, fmt.Errorf("%w: idempotency key was already used", ErrJobConflict)
		}
		return cloneJobRecord(existing.record), nil
	}
	if _, ok := state.byID[request.JobID]; ok {
		return JobRecord{}, fmt.Errorf("%w: job_id already exists", ErrJobConflict)
	}
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	exportRecord, err := service.exports.Get(request.ExportID, access)
	if err != nil {
		return JobRecord{}, err
	}
	if !reflect.DeepEqual(exportRecord.BundleRef, request.ExportBundleRef) || exportRecord.Bundle.ExportID != request.ExportID {
		return JobRecord{}, fmt.Errorf("%w: export bundle binding drifted", ErrContaminated)
	}
	if request.CreatedAt.Before(exportRecord.Bundle.CreatedAt) {
		return JobRecord{}, fmt.Errorf("training job cannot precede export")
	}
	plan, err := sealJobPlan(JobPlan{
		Request: request, ExportBundleSHA256: exportRecord.Bundle.SHA256,
		PortableFormat: PortableRecordsFormat, ExecutionMode: TrainingExecutionExternalManual,
		RemoteSideEffects: "deny", ReceiptAuthority: TrainingReceiptAuthority,
		PromotionEligible: false, Status: JobStatusPrepared, Observations: []JobObservation{},
		CreatedBy: mutation.Actor, UpdatedBy: mutation.Actor, UpdatedAt: mutation.At.UTC(),
	})
	if err != nil {
		return JobRecord{}, err
	}
	return service.persist(state, jobLedgerEvent{SchemaVersion: jobLedgerEventSchemaVersion, Type: "prepared", JobID: request.JobID, Prepare: &request, Mutation: mutation}, plan)
}

func (service *JobService) Observe(ctx context.Context, request JobObservationRequest, mutation evaluation.Mutation) (JobRecord, error) {
	if err := validateJobMutation(ctx, mutation, request.ObservedAt); err != nil {
		return JobRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return JobRecord{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	state, err := service.load()
	if err != nil {
		return JobRecord{}, err
	}
	if existing, ok := state.byIdempotency[mutation.IdempotencyKey]; ok {
		if existing.event.Type != "observed" || !reflect.DeepEqual(existing.event.Observation, &request) || !reflect.DeepEqual(existing.event.Mutation, mutation) {
			return JobRecord{}, fmt.Errorf("%w: idempotency key was already used", ErrJobConflict)
		}
		return cloneJobRecord(existing.record), nil
	}
	current, ok := state.byID[request.JobID]
	if !ok {
		return JobRecord{}, ErrJobNotFound
	}
	if current.Plan.SHA256 != request.ExpectedPlanSHA256 {
		return JobRecord{}, fmt.Errorf("%w: expected plan SHA-256 is stale", ErrJobConflict)
	}
	if current.Plan.Status != JobStatusPrepared && current.Plan.Status != JobStatusSubmitted {
		return JobRecord{}, fmt.Errorf("%w: job is terminal", ErrJobInvalidTransition)
	}
	if current.Plan.Status == JobStatusPrepared && request.Receipt.Status != JobStatusSubmitted {
		return JobRecord{}, fmt.Errorf("%w: first observation must be submitted", ErrJobInvalidTransition)
	}
	if current.Plan.Status == JobStatusSubmitted {
		first := current.Plan.Observations[0].Receipt
		if request.Receipt.Status == JobStatusSubmitted || request.Receipt.ExternalJobID != first.ExternalJobID {
			return JobRecord{}, fmt.Errorf("%w: terminal receipt must bind the submitted external job", ErrJobInvalidTransition)
		}
	}
	receiptData, err := json.Marshal(request.Receipt)
	if err != nil {
		return JobRecord{}, err
	}
	receiptRef, err := service.artifacts.PutArtifact(runmodel.ContractTrainingProviderJobReceipt, receiptData)
	if err != nil {
		return JobRecord{}, fmt.Errorf("persist provider job receipt: %w", err)
	}
	plan := cloneJobPlan(current.Plan)
	plan.Status = request.Receipt.Status
	plan.Observations = append(plan.Observations, JobObservation{ReceiptRef: receiptRef, Receipt: request.Receipt, RecordedBy: mutation.Actor, RecordedAt: mutation.At.UTC()})
	plan.UpdatedBy, plan.UpdatedAt = mutation.Actor, mutation.At.UTC()
	plan, err = sealJobPlan(plan)
	if err != nil {
		return JobRecord{}, err
	}
	return service.persist(state, jobLedgerEvent{SchemaVersion: jobLedgerEventSchemaVersion, Type: "observed", JobID: request.JobID, Observation: &request, Mutation: mutation}, plan)
}

func validateJobMutation(ctx context.Context, mutation evaluation.Mutation, at time.Time) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := mutation.Validate(); err != nil {
		return err
	}
	if !at.Equal(mutation.At.UTC()) {
		return fmt.Errorf("request time must equal mutation time")
	}
	if !slices.Contains(mutation.Roles, evaluation.RoleDatasetCurator) {
		return fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	return nil
}

func (service *JobService) Get(jobID string, access evaluation.Access) (JobRecord, error) {
	if err := validateID("job_id", jobID); err != nil {
		return JobRecord{}, err
	}
	if err := authorizeRead(access); err != nil {
		return JobRecord{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	state, err := service.load()
	if err != nil {
		return JobRecord{}, err
	}
	record, ok := state.byID[jobID]
	if !ok {
		return JobRecord{}, ErrJobNotFound
	}
	return cloneJobRecord(record), nil
}

func (service *JobService) List(access evaluation.Access) ([]JobRecord, error) {
	if err := authorizeRead(access); err != nil {
		return nil, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	state, err := service.load()
	if err != nil {
		return nil, err
	}
	result := make([]JobRecord, 0, len(state.byID))
	for _, record := range state.byID {
		result = append(result, cloneJobRecord(record))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Plan.Request.JobID < result[j].Plan.Request.JobID })
	return result, nil
}

type jobOperation struct {
	event  jobLedgerEvent
	record JobRecord
}
type jobState struct {
	events        []jobLedgerEvent
	byID          map[string]JobRecord
	byIdempotency map[string]jobOperation
}

func (service *JobService) persist(state jobState, event jobLedgerEvent, plan JobPlan) (JobRecord, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return JobRecord{}, err
	}
	planRef, err := service.artifacts.PutArtifact(runmodel.ContractTrainingJobPlan, data)
	if err != nil {
		return JobRecord{}, err
	}
	event.PlanRef = planRef
	_, err = service.store.AppendJSONLAtSequence(jobLedgerStream, uint64(len(state.events)), local.Event{ID: event.Mutation.IdempotencyKey, Schema: jobLedgerEventSchemaVersion, Time: event.Mutation.At.UTC(), Payload: event})
	if err != nil {
		reloaded, loadErr := service.load()
		if loadErr == nil {
			if existing, ok := reloaded.byIdempotency[event.Mutation.IdempotencyKey]; ok && reflect.DeepEqual(existing.event, event) {
				return cloneJobRecord(existing.record), nil
			}
		}
		return JobRecord{}, fmt.Errorf("append training job ledger: %w", err)
	}
	return JobRecord{PlanRef: planRef, Plan: plan, Mutation: event.Mutation}, nil
}

func (service *JobService) load() (jobState, error) {
	state := jobState{events: []jobLedgerEvent{}, byID: map[string]JobRecord{}, byIdempotency: map[string]jobOperation{}}
	envelopes, err := service.store.ReadJSONL(jobLedgerStream)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return jobState{}, fmt.Errorf("%w: read ledger: %v", ErrJobCorrupt, err)
	}
	for _, envelope := range envelopes {
		var event jobLedgerEvent
		if envelope.Schema != jobLedgerEventSchemaVersion || decodeStrict(envelope.Payload, &event) != nil || event.SchemaVersion != jobLedgerEventSchemaVersion || event.Mutation.Validate() != nil || envelope.ID != event.Mutation.IdempotencyKey || !envelope.Time.Equal(event.Mutation.At.UTC()) || event.PlanRef.Validate() != nil || event.PlanRef.Contract != runmodel.ContractTrainingJobPlan {
			return jobState{}, fmt.Errorf("%w: invalid event at sequence %d", ErrJobCorrupt, envelope.Sequence)
		}
		if _, exists := state.byIdempotency[event.Mutation.IdempotencyKey]; exists {
			return jobState{}, fmt.Errorf("%w: duplicate idempotency key", ErrJobCorrupt)
		}
		if err := service.artifacts.CheckArtifactEligibility(event.PlanRef, runrepo.ArtifactUseRead); err != nil {
			return jobState{}, fmt.Errorf("%w: plan eligibility: %v", ErrJobCorrupt, err)
		}
		data, err := service.artifacts.ReadArtifact(event.PlanRef)
		if err != nil {
			return jobState{}, fmt.Errorf("%w: read plan: %v", ErrJobCorrupt, err)
		}
		plan, err := DecodeJobPlan(data)
		if err != nil || plan.Request.JobID != event.JobID {
			return jobState{}, fmt.Errorf("%w: plan binding at sequence %d", ErrJobCorrupt, envelope.Sequence)
		}
		access := evaluation.Access{Actor: event.Mutation.Actor, Roles: slices.Clone(event.Mutation.Roles)}
		exportRecord, err := service.exports.Get(plan.Request.ExportID, access)
		if err != nil || !reflect.DeepEqual(exportRecord.BundleRef, plan.Request.ExportBundleRef) || exportRecord.Bundle.SHA256 != plan.ExportBundleSHA256 {
			return jobState{}, fmt.Errorf("%w: export binding at sequence %d", ErrJobCorrupt, envelope.Sequence)
		}
		previous, existed := state.byID[event.JobID]
		switch event.Type {
		case "prepared":
			if existed || event.Prepare == nil || event.Observation != nil || event.Prepare.Validate() != nil || !reflect.DeepEqual(plan.Request, *event.Prepare) || len(plan.Observations) != 0 || plan.Status != JobStatusPrepared || plan.CreatedBy != event.Mutation.Actor || !plan.UpdatedAt.Equal(event.Mutation.At.UTC()) {
				return jobState{}, fmt.Errorf("%w: invalid prepared event", ErrJobCorrupt)
			}
		case "observed":
			if !existed || event.Prepare != nil || event.Observation == nil || event.Observation.Validate() != nil || event.Observation.JobID != event.JobID || event.Observation.ExpectedPlanSHA256 != previous.Plan.SHA256 || !jobPlanExtends(previous.Plan, plan) || !reflect.DeepEqual(plan.Observations[len(plan.Observations)-1].Receipt, event.Observation.Receipt) || plan.UpdatedBy != event.Mutation.Actor || !plan.UpdatedAt.Equal(event.Mutation.At.UTC()) {
				return jobState{}, fmt.Errorf("%w: invalid observed event", ErrJobCorrupt)
			}
			last := plan.Observations[len(plan.Observations)-1]
			if err := service.artifacts.CheckArtifactEligibility(last.ReceiptRef, runrepo.ArtifactUseRead); err != nil {
				return jobState{}, fmt.Errorf("%w: receipt eligibility: %v", ErrJobCorrupt, err)
			}
			receiptData, err := service.artifacts.ReadArtifact(last.ReceiptRef)
			if err != nil {
				return jobState{}, fmt.Errorf("%w: read receipt: %v", ErrJobCorrupt, err)
			}
			receipt, err := DecodeProviderJobReceipt(receiptData)
			if err != nil || !reflect.DeepEqual(receipt, last.Receipt) {
				return jobState{}, fmt.Errorf("%w: receipt binding", ErrJobCorrupt)
			}
		default:
			return jobState{}, fmt.Errorf("%w: unknown event type", ErrJobCorrupt)
		}
		record := JobRecord{PlanRef: event.PlanRef, Plan: plan, Mutation: event.Mutation}
		state.events = append(state.events, event)
		state.byID[event.JobID] = record
		state.byIdempotency[event.Mutation.IdempotencyKey] = jobOperation{event: event, record: record}
	}
	return state, nil
}

func cloneJobPlan(plan JobPlan) JobPlan {
	plan.Observations = slices.Clone(plan.Observations)
	return plan
}
func cloneJobRecord(record JobRecord) JobRecord {
	record.Plan = cloneJobPlan(record.Plan)
	record.Mutation.Roles = slices.Clone(record.Mutation.Roles)
	return record
}
