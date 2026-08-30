package evaluation

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

	"github.com/abietic/argus/internal/store/local"
)

type Repository struct {
	store *local.Store
	mu    sync.Mutex
}

func New(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	repository := &Repository{store: store}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) CreateCase(
	ctx context.Context,
	evaluationCase EvaluationCase,
	mutation Mutation,
) (CaseRecord, error) {
	if err := checkContext(ctx); err != nil {
		return CaseRecord{}, err
	}
	if err := evaluationCase.Validate(); err != nil {
		return CaseRecord{}, fmt.Errorf("validate evaluation case: %w", err)
	}
	if evaluationCase.ReviewState != ReviewPending ||
		evaluationCase.DatasetState != DatasetCandidatePool ||
		evaluationCase.Split != SplitUnassigned || evaluationCase.Eligibility != (Eligibility{}) {
		return CaseRecord{}, fmt.Errorf("%w: CreateCase only admits pending, unassigned, ineligible candidate-pool ingress", ErrInvalidTransition)
	}
	if err := mutation.Validate(); err != nil {
		return CaseRecord{}, err
	}
	if !evaluationCase.CreatedAt.Equal(mutation.At.UTC()) {
		return CaseRecord{}, fmt.Errorf("case created_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion,
		Type:          datasetCaseCreated,
		Case:          clonePointer(evaluationCase),
		Actor:         mutation.Actor,
		Roles:         slices.Clone(mutation.Roles),
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, datasetStream, datasetEventSchemaVersion, event, mutation,
		); err != nil {
			return CaseRecord{}, err
		}
		return state.caseRecord(evaluationCase.CaseID)
	}
	if _, exists := state.cases[evaluationCase.CaseID]; exists {
		return CaseRecord{}, fmt.Errorf("%w: case_id %q already exists",
			ErrConflict, evaluationCase.CaseID)
	}
	if err := authorizeCaseCreate(evaluationCase, mutation.Roles); err != nil {
		return CaseRecord{}, err
	}
	if err := state.validateCaseAddition(evaluationCase); err != nil {
		return CaseRecord{}, err
	}
	if _, err := repository.appendDataset(mutation, event); err != nil {
		return CaseRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	return reloaded.caseRecord(evaluationCase.CaseID)
}

func (repository *Repository) ImportGovernedCase(
	ctx context.Context,
	request ExternalGovernedCaseImport,
	mutation Mutation,
) (CaseRecord, error) {
	if err := checkContext(ctx); err != nil {
		return CaseRecord{}, err
	}
	if err := request.Validate(); err != nil {
		return CaseRecord{}, fmt.Errorf("validate external governed case import: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return CaseRecord{}, err
	}
	if !request.ImportedAt.Equal(mutation.At.UTC()) {
		return CaseRecord{}, fmt.Errorf("imported_at must equal mutation time")
	}
	if slices.Contains(request.Attestation.ReviewerIDs, mutation.Actor) ||
		request.Attestation.AdjudicatorID == mutation.Actor {
		return CaseRecord{}, fmt.Errorf("%w: import operator must be independent from external reviewers and adjudicator", ErrUnauthorized)
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetCaseImported,
		Import: clonePointer(request), Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles),
		Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return CaseRecord{}, err
		}
		return state.caseRecord(request.Case.CaseID)
	}
	if _, exists := state.cases[request.Case.CaseID]; exists {
		return CaseRecord{}, fmt.Errorf("%w: case_id %q already exists", ErrConflict, request.Case.CaseID)
	}
	if err := authorizeGovernedCaseImport(request.Case, mutation.Roles); err != nil {
		return CaseRecord{}, err
	}
	if err := state.authorizeGovernanceKeyForImport(request, mutation.Actor); err != nil {
		return CaseRecord{}, err
	}
	if err := state.validateCaseAddition(request.Case); err != nil {
		return CaseRecord{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return CaseRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	return reloaded.caseRecord(request.Case.CaseID)
}

func (repository *Repository) CorrectLabel(
	ctx context.Context,
	correction LabelCorrection,
	mutation Mutation,
) (CaseRecord, error) {
	if err := checkContext(ctx); err != nil {
		return CaseRecord{}, err
	}
	if err := correction.Validate(); err != nil {
		return CaseRecord{}, fmt.Errorf("validate label correction: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return CaseRecord{}, err
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion,
		Type:          datasetLabelCorrected,
		Correction:    clonePointer(correction),
		Actor:         mutation.Actor,
		Roles:         slices.Clone(mutation.Roles),
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, datasetStream, datasetEventSchemaVersion, event, mutation,
		); err != nil {
			return CaseRecord{}, err
		}
		return state.caseRecord(correction.CaseID)
	}
	record, exists := state.cases[correction.CaseID]
	if !exists {
		return CaseRecord{}, fmt.Errorf("%w: case %q", ErrNotFound, correction.CaseID)
	}
	if err := authorizeCaseWrite(record.CurrentCase(), mutation.Roles); err != nil {
		return CaseRecord{}, err
	}
	if record.CurrentGovernance.DatasetState == DatasetRetired {
		return CaseRecord{}, fmt.Errorf("%w: retired case cannot receive label corrections",
			ErrInvalidTransition)
	}
	if record.CurrentGovernance.ReviewState == ReviewInReview {
		return CaseRecord{}, fmt.Errorf("%w: label correction requires reopening or completing the assigned review round", ErrInvalidTransition)
	}
	if correction.ExpectedLabelRevision != record.CurrentLabelRevision {
		return CaseRecord{}, fmt.Errorf("%w: label revision is %d, expected %d",
			ErrInvalidTransition, record.CurrentLabelRevision, correction.ExpectedLabelRevision)
	}
	if err := validateLabelForCase(record.Case.Type, correction.Label); err != nil {
		return CaseRecord{}, err
	}
	if reflect.DeepEqual(correction.Label, record.CurrentLabel) &&
		correction.LabelPolicyRevision == record.CurrentLabelPolicyRevision {
		return CaseRecord{}, fmt.Errorf("%w: label correction is a no-op", ErrInvalidTransition)
	}
	affected := state.affectedExperiments(correction.CaseID)
	if !slices.Equal(affected, correction.AffectedExperimentIDs) {
		return CaseRecord{}, fmt.Errorf(
			"%w: affected_experiment_ids are %v, want %v",
			ErrInvalidTransition, correction.AffectedExperimentIDs, affected,
		)
	}
	if _, err := repository.appendDataset(mutation, event); err != nil {
		return CaseRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	return reloaded.caseRecord(correction.CaseID)
}

func (repository *Repository) RecordCaseAnnotation(
	ctx context.Context,
	annotation CaseAnnotation,
	mutation Mutation,
) (CaseAnnotationEntry, error) {
	if err := checkContext(ctx); err != nil {
		return CaseAnnotationEntry{}, err
	}
	if err := annotation.Validate(); err != nil {
		return CaseAnnotationEntry{}, fmt.Errorf("validate case annotation: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return CaseAnnotationEntry{}, err
	}
	if !annotation.ReviewedAt.Equal(mutation.At.UTC()) {
		return CaseAnnotationEntry{}, fmt.Errorf("annotation reviewed_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetCaseAnnotated,
		Annotation: clonePointer(annotation), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseAnnotationEntry{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return CaseAnnotationEntry{}, err
		}
		entry, exists := state.annotationEvents[mutation.IdempotencyKey]
		if !exists {
			return CaseAnnotationEntry{}, corruptf("idempotent annotation has no projection")
		}
		return cloneValue(entry), nil
	}
	record, exists := state.cases[annotation.CaseID]
	if !exists {
		return CaseAnnotationEntry{}, fmt.Errorf("%w: case %q", ErrNotFound, annotation.CaseID)
	}
	if err := state.validateAnnotation(record, annotation, mutation.Actor, mutation.Roles); err != nil {
		return CaseAnnotationEntry{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return CaseAnnotationEntry{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseAnnotationEntry{}, err
	}
	entry, exists := reloaded.annotationEvents[mutation.IdempotencyKey]
	if !exists {
		return CaseAnnotationEntry{}, corruptf("appended annotation has no projection")
	}
	return cloneValue(entry), nil
}

func (repository *Repository) AssignCaseReview(
	ctx context.Context,
	assignment CaseReviewAssignment,
	mutation Mutation,
) (CaseReviewAssignmentEntry, error) {
	if err := checkContext(ctx); err != nil {
		return CaseReviewAssignmentEntry{}, err
	}
	if err := assignment.Validate(); err != nil {
		return CaseReviewAssignmentEntry{}, fmt.Errorf("validate case review assignment: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return CaseReviewAssignmentEntry{}, err
	}
	if !assignment.AssignedAt.Equal(mutation.At.UTC()) {
		return CaseReviewAssignmentEntry{}, fmt.Errorf("assignment assigned_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetCaseReviewAssigned,
		Assignment: clonePointer(assignment), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseReviewAssignmentEntry{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return CaseReviewAssignmentEntry{}, err
		}
		entries := state.assignments[assignment.CaseID]
		for _, entry := range entries {
			if entry.EventID == mutation.IdempotencyKey {
				return cloneValue(entry), nil
			}
		}
		return CaseReviewAssignmentEntry{}, corruptf("idempotent assignment has no projection")
	}
	record, exists := state.cases[assignment.CaseID]
	if !exists {
		return CaseReviewAssignmentEntry{}, fmt.Errorf("%w: case %q", ErrNotFound, assignment.CaseID)
	}
	if err := state.validateReviewAssignment(record, assignment, mutation.Actor, mutation.Roles); err != nil {
		return CaseReviewAssignmentEntry{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return CaseReviewAssignmentEntry{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseReviewAssignmentEntry{}, err
	}
	entries := reloaded.assignments[assignment.CaseID]
	return cloneValue(entries[len(entries)-1]), nil
}

func (repository *Repository) AdjudicateCase(
	ctx context.Context,
	adjudication CaseAdjudication,
	mutation Mutation,
) (CaseRecord, error) {
	if err := checkContext(ctx); err != nil {
		return CaseRecord{}, err
	}
	if err := adjudication.Validate(); err != nil {
		return CaseRecord{}, fmt.Errorf("validate case adjudication: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return CaseRecord{}, err
	}
	if !adjudication.AdjudicatedAt.Equal(mutation.At.UTC()) {
		return CaseRecord{}, fmt.Errorf("adjudication adjudicated_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetCaseAdjudicated,
		Adjudication: clonePointer(adjudication), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return CaseRecord{}, err
		}
		return state.caseRecord(adjudication.CaseID)
	}
	record, exists := state.cases[adjudication.CaseID]
	if !exists {
		return CaseRecord{}, fmt.Errorf("%w: case %q", ErrNotFound, adjudication.CaseID)
	}
	if err := state.validateAdjudication(record, adjudication, mutation.Actor, mutation.Roles); err != nil {
		return CaseRecord{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return CaseRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	return reloaded.caseRecord(adjudication.CaseID)
}

func (repository *Repository) ActivateCase(
	ctx context.Context,
	activation CaseActivation,
	mutation Mutation,
) (CaseRecord, error) {
	if err := checkContext(ctx); err != nil {
		return CaseRecord{}, err
	}
	if err := activation.Validate(); err != nil {
		return CaseRecord{}, fmt.Errorf("validate case activation: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return CaseRecord{}, err
	}
	if !activation.ActivatedAt.Equal(mutation.At.UTC()) {
		return CaseRecord{}, fmt.Errorf("activation activated_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetCaseActivated,
		Activation: clonePointer(activation), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return CaseRecord{}, err
		}
		return state.caseRecord(activation.CaseID)
	}
	record, exists := state.cases[activation.CaseID]
	if !exists {
		return CaseRecord{}, fmt.Errorf("%w: case %q", ErrNotFound, activation.CaseID)
	}
	if err := state.validateActivation(record, activation, mutation.Roles); err != nil {
		return CaseRecord{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return CaseRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	return reloaded.caseRecord(activation.CaseID)
}

func (repository *Repository) ReopenCase(
	ctx context.Context,
	reopen CaseReopen,
	mutation Mutation,
) (CaseRecord, error) {
	if err := checkContext(ctx); err != nil {
		return CaseRecord{}, err
	}
	if err := reopen.Validate(); err != nil {
		return CaseRecord{}, fmt.Errorf("validate case reopen: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return CaseRecord{}, err
	}
	if !reopen.ReopenedAt.Equal(mutation.At.UTC()) {
		return CaseRecord{}, fmt.Errorf("reopen reopened_at must equal mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion, Type: datasetCaseReopened,
		Reopen: clonePointer(reopen), Actor: mutation.Actor,
		Roles: slices.Clone(mutation.Roles), Audit: mutation.Audit, OccurredAt: mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(existing, datasetStream, datasetEventSchemaVersion, event, mutation); err != nil {
			return CaseRecord{}, err
		}
		return state.caseRecord(reopen.CaseID)
	}
	record, exists := state.cases[reopen.CaseID]
	if !exists {
		return CaseRecord{}, fmt.Errorf("%w: case %q", ErrNotFound, reopen.CaseID)
	}
	if err := state.validateReopen(record, reopen, mutation.Roles); err != nil {
		return CaseRecord{}, err
	}
	if _, err := repository.appendDatasetAtSequence(mutation, event, state.streamSequences[datasetStream]); err != nil {
		return CaseRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	return reloaded.caseRecord(reopen.CaseID)
}

func (repository *Repository) RecordExposure(
	ctx context.Context,
	exposure Exposure,
	mutation Mutation,
) (ExposureEntry, error) {
	if err := checkContext(ctx); err != nil {
		return ExposureEntry{}, err
	}
	if err := exposure.Validate(); err != nil {
		return ExposureEntry{}, fmt.Errorf("validate exposure: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return ExposureEntry{}, err
	}
	if exposure.ObservedAt.After(mutation.At.UTC()) {
		return ExposureEntry{}, fmt.Errorf("exposure observed_at must not be after mutation time")
	}
	event := datasetEvent{
		SchemaVersion: datasetEventSchemaVersion,
		Type:          datasetExposureRecorded,
		Exposure:      clonePointer(exposure),
		Actor:         mutation.Actor,
		Roles:         slices.Clone(mutation.Roles),
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return ExposureEntry{}, err
	}
	key := exposureKey{RunID: exposure.EvaluationRunID, CaseID: exposure.CaseID}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, datasetStream, datasetEventSchemaVersion, event, mutation,
		); err != nil {
			return ExposureEntry{}, err
		}
		entry, exists := state.exposures[key]
		if !exists {
			return ExposureEntry{}, corruptf("idempotent exposure event has no projection")
		}
		return cloneValue(entry), nil
	}
	record, exists := state.cases[exposure.CaseID]
	if !exists {
		return ExposureEntry{}, fmt.Errorf("%w: case %q", ErrNotFound, exposure.CaseID)
	}
	if record.CurrentGovernance.DatasetState != DatasetActive {
		return ExposureEntry{}, fmt.Errorf("%w: exposure requires an active case",
			ErrInvalidTransition)
	}
	if exposure.ObservedAt.Before(record.Case.CreatedAt) {
		return ExposureEntry{}, fmt.Errorf("exposure observed_at predates the case")
	}
	if err := authorizeExposure(record.CurrentCase(), mutation.Roles); err != nil {
		return ExposureEntry{}, err
	}
	if _, exists := state.exposures[key]; exists {
		return ExposureEntry{}, fmt.Errorf("%w: exposure for run %q and case %q already exists",
			ErrConflict, exposure.EvaluationRunID, exposure.CaseID)
	}
	if _, err := repository.appendDataset(mutation, event); err != nil {
		return ExposureEntry{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return ExposureEntry{}, err
	}
	entry, exists := reloaded.exposures[key]
	if !exists {
		return ExposureEntry{}, corruptf("appended exposure has no projection")
	}
	return cloneValue(entry), nil
}

func (repository *Repository) GetCase(caseID string, access Access) (CaseRecord, error) {
	if err := validateID("case_id", caseID); err != nil {
		return CaseRecord{}, err
	}
	if err := access.Validate(); err != nil {
		return CaseRecord{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseRecord{}, err
	}
	record, err := state.caseRecord(caseID)
	if err != nil {
		return CaseRecord{}, err
	}
	if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
		return CaseRecord{}, err
	}
	return record, nil
}

// ListCases returns only cases visible to the caller. In particular, holdout
// identities are not disclosed to callers without a holdout role.
func (repository *Repository) ListCases(access Access) ([]CaseRecord, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.cases))
	for id, record := range state.cases {
		if authorizeCaseRead(record.CurrentCase(), access.Roles) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]CaseRecord, 0, len(ids))
	for _, id := range ids {
		record, err := state.caseRecord(id)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func (repository *Repository) LabelHistory(
	caseID string,
	access Access,
) ([]LabelEntry, error) {
	if err := validateID("case_id", caseID); err != nil {
		return nil, err
	}
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	record, exists := state.cases[caseID]
	if !exists {
		return nil, fmt.Errorf("%w: case %q", ErrNotFound, caseID)
	}
	if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
		return nil, err
	}
	return cloneValue(state.labels[caseID]), nil
}

func (repository *Repository) AnnotationHistory(
	caseID string,
	access Access,
) ([]CaseAnnotationEntry, error) {
	if err := validateID("case_id", caseID); err != nil {
		return nil, err
	}
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	record, exists := state.cases[caseID]
	if !exists {
		return nil, fmt.Errorf("%w: case %q", ErrNotFound, caseID)
	}
	if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
		return nil, err
	}
	entries := state.annotations[caseID]
	if hasRole(access.Roles, RoleDatasetReviewer) &&
		!hasRole(access.Roles, RoleDatasetCurator) &&
		!hasRole(access.Roles, RoleDatasetAdjudicator) {
		filtered := make([]CaseAnnotationEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.Reviewer == access.Actor {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}
	return cloneValue(entries), nil
}

func (repository *Repository) AssignmentHistory(caseID string, access Access) ([]CaseReviewAssignmentEntry, error) {
	if err := validateID("case_id", caseID); err != nil {
		return nil, err
	}
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	record, exists := state.cases[caseID]
	if !exists {
		return nil, fmt.Errorf("%w: case %q", ErrNotFound, caseID)
	}
	if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
		return nil, err
	}
	entries := state.assignments[caseID]
	if hasRole(access.Roles, RoleDatasetReviewer) &&
		!hasRole(access.Roles, RoleDatasetCurator) &&
		!hasRole(access.Roles, RoleDatasetAdjudicator) {
		filtered := make([]CaseReviewAssignmentEntry, 0, len(entries))
		for _, entry := range entries {
			if slices.Contains(entry.Assignment.ReviewerIDs, access.Actor) {
				copy := cloneValue(entry)
				copy.Assignment.ReviewerIDs = []string{access.Actor}
				filtered = append(filtered, copy)
			}
		}
		return filtered, nil
	}
	return cloneValue(entries), nil
}

func (repository *Repository) ReviewAgreement(caseID string, access Access) (CaseReviewAgreement, error) {
	if err := validateID("case_id", caseID); err != nil {
		return CaseReviewAgreement{}, err
	}
	if err := access.Validate(); err != nil {
		return CaseReviewAgreement{}, err
	}
	if !hasRole(access.Roles, RoleDatasetCurator) && !hasRole(access.Roles, RoleDatasetAdjudicator) {
		if hasRole(access.Roles, RoleDatasetReviewer) &&
			!hasRole(access.Roles, RoleHoldoutMaintainer) &&
			!hasRole(access.Roles, RoleHoldoutRunner) {
			return CaseReviewAgreement{CaseID: caseID, VerdictAgreement: ReviewAgreementUnavailable, ExactLabelAgreement: ReviewAgreementUnavailable}, nil
		}
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return CaseReviewAgreement{}, err
	}
	record, exists := state.cases[caseID]
	if !exists {
		return CaseReviewAgreement{}, fmt.Errorf("%w: case %q", ErrNotFound, caseID)
	}
	if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
		return CaseReviewAgreement{}, err
	}
	return state.reviewAgreement(caseID), nil
}

func (repository *Repository) AdjudicationHistory(
	caseID string,
	access Access,
) ([]CaseAdjudicationEntry, error) {
	if err := validateID("case_id", caseID); err != nil {
		return nil, err
	}
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	record, exists := state.cases[caseID]
	if !exists {
		return nil, fmt.Errorf("%w: case %q", ErrNotFound, caseID)
	}
	if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
		return nil, err
	}
	return cloneValue(state.adjudications[caseID]), nil
}

func (repository *Repository) ExposureHistory(
	caseID string,
	access Access,
) ([]ExposureEntry, error) {
	if err := validateID("case_id", caseID); err != nil {
		return nil, err
	}
	if err := access.Validate(); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	record, exists := state.cases[caseID]
	if !exists {
		return nil, fmt.Errorf("%w: case %q", ErrNotFound, caseID)
	}
	if err := authorizeCaseRead(record.CurrentCase(), access.Roles); err != nil {
		return nil, err
	}
	entries := make([]ExposureEntry, 0)
	for key, entry := range state.exposures {
		if key.CaseID == caseID {
			entries = append(entries, cloneValue(entry))
		}
	}
	sort.Slice(entries, func(left, right int) bool {
		if !entries[left].At.Equal(entries[right].At) {
			return entries[left].At.Before(entries[right].At)
		}
		return entries[left].EventID < entries[right].EventID
	})
	return entries, nil
}

// AssertHoldoutExposureComplete requires an explicit frozen holdout case list.
// Missing cases, missing component observations, or any prior exposure is
// contamination; absence is never interpreted as not-seen.
func (repository *Repository) AssertHoldoutExposureComplete(
	evaluationRunID string,
	holdoutCaseIDs []string,
	access Access,
) error {
	if err := validateID("evaluation_run_id", evaluationRunID); err != nil {
		return err
	}
	if err := validateSortedIDs("holdout_case_ids", holdoutCaseIDs, true); err != nil {
		return err
	}
	if err := access.Validate(); err != nil {
		return err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return err
	}
	return state.assertHoldoutExposureComplete(
		evaluationRunID, holdoutCaseIDs, access, time.Time{},
	)
}

func (repository *Repository) RegisterPromotion(
	ctx context.Context,
	variant PromotionVariant,
	mutation Mutation,
) (PromotionRecord, error) {
	if variant.ManagedBinding != nil {
		return PromotionRecord{}, fmt.Errorf("%w: managed promotion must use its domain service", ErrUnauthorized)
	}
	return repository.registerPromotion(ctx, variant, mutation)
}

// RegisterManagedPromotion is intentionally for a domain service which owns
// the typed binding. User-facing generic promotion routes must call
// RegisterPromotion and therefore fail closed for managed variants.
func (repository *Repository) RegisterManagedPromotion(
	ctx context.Context,
	variant PromotionVariant,
	mutation Mutation,
) (PromotionRecord, error) {
	if variant.ManagedBinding == nil {
		return PromotionRecord{}, fmt.Errorf("managed promotion binding is required")
	}
	return repository.registerPromotion(ctx, variant, mutation)
}

func (repository *Repository) registerPromotion(
	ctx context.Context,
	variant PromotionVariant,
	mutation Mutation,
) (PromotionRecord, error) {
	if err := checkContext(ctx); err != nil {
		return PromotionRecord{}, err
	}
	if err := variant.Validate(); err != nil {
		return PromotionRecord{}, fmt.Errorf("validate promotion variant: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return PromotionRecord{}, err
	}
	if !variant.CreatedAt.Equal(mutation.At.UTC()) {
		return PromotionRecord{}, fmt.Errorf("variant created_at must equal mutation time")
	}
	event := promotionEvent{
		SchemaVersion: promotionEventSchemaVersion,
		Type:          promotionRegistered,
		Variant:       clonePointer(variant),
		Actor:         mutation.Actor,
		Roles:         slices.Clone(mutation.Roles),
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return PromotionRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, promotionStream, promotionEventSchemaVersion, event, mutation,
		); err != nil {
			return PromotionRecord{}, err
		}
		return state.promotionRecordForRoles(variant.VariantID, mutation.Roles)
	}
	if !hasRole(mutation.Roles, RolePromotionOperator) {
		return PromotionRecord{}, fmt.Errorf("%w: promotion_operator role is required",
			ErrUnauthorized)
	}
	if _, exists := state.promotions[variant.VariantID]; exists {
		return PromotionRecord{}, fmt.Errorf("%w: variant_id %q already exists",
			ErrConflict, variant.VariantID)
	}
	for _, record := range state.promotions {
		if record.Variant.Component == variant.Component &&
			record.Variant.Revision == variant.Revision {
			return PromotionRecord{}, fmt.Errorf(
				"%w: component %q revision %q already has a promotion record",
				ErrConflict, variant.Component, variant.Revision,
			)
		}
	}
	if _, err := repository.appendPromotion(mutation, event); err != nil {
		return PromotionRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return PromotionRecord{}, err
	}
	return reloaded.promotionRecordForRoles(variant.VariantID, mutation.Roles)
}

func (repository *Repository) RecordGate(
	ctx context.Context,
	result GateResult,
	mutation Mutation,
) (PromotionRecord, error) {
	record, err := repository.GetPromotion(result.VariantID, Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return PromotionRecord{}, err
	}
	if record.Variant.ManagedBinding != nil {
		return PromotionRecord{}, fmt.Errorf("%w: managed promotion gate must use its domain service", ErrUnauthorized)
	}
	return repository.recordGate(ctx, result, mutation)
}

func (repository *Repository) RecordManagedGate(
	ctx context.Context,
	result GateResult,
	binding PromotionManagementBinding,
	mutation Mutation,
) (PromotionRecord, error) {
	record, err := repository.GetPromotion(result.VariantID, Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return PromotionRecord{}, err
	}
	if record.Variant.ManagedBinding == nil || !reflect.DeepEqual(*record.Variant.ManagedBinding, binding) {
		return PromotionRecord{}, fmt.Errorf("%w: managed promotion binding mismatch", ErrConflict)
	}
	return repository.recordGate(ctx, result, mutation)
}

func (repository *Repository) recordGate(
	ctx context.Context,
	result GateResult,
	mutation Mutation,
) (PromotionRecord, error) {
	if err := checkContext(ctx); err != nil {
		return PromotionRecord{}, err
	}
	if err := result.Validate(); err != nil {
		return PromotionRecord{}, fmt.Errorf("validate gate result: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return PromotionRecord{}, err
	}
	event := promotionEvent{
		SchemaVersion: promotionEventSchemaVersion,
		Type:          promotionGateRecorded,
		GateResult:    clonePointer(result),
		Actor:         mutation.Actor,
		Roles:         slices.Clone(mutation.Roles),
		Audit:         mutation.Audit,
		OccurredAt:    mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return PromotionRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, promotionStream, promotionEventSchemaVersion, event, mutation,
		); err != nil {
			return PromotionRecord{}, err
		}
		return state.promotionRecordForRoles(result.VariantID, mutation.Roles)
	}
	if err := state.validateGateEvent(event); err != nil {
		return PromotionRecord{}, err
	}
	if _, err := repository.appendPromotion(mutation, event); err != nil {
		return PromotionRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return PromotionRecord{}, err
	}
	return reloaded.promotionRecordForRoles(result.VariantID, mutation.Roles)
}

func (repository *Repository) RollbackPromotion(
	ctx context.Context,
	variantID string,
	mutation Mutation,
) (PromotionRecord, error) {
	record, err := repository.GetPromotion(variantID, Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return PromotionRecord{}, err
	}
	if record.Variant.ManagedBinding != nil {
		return PromotionRecord{}, fmt.Errorf("%w: managed promotion rollback must use its domain service", ErrUnauthorized)
	}
	return repository.rollbackPromotion(ctx, variantID, mutation)
}

func (repository *Repository) RollbackManagedPromotion(
	ctx context.Context,
	variantID string,
	binding PromotionManagementBinding,
	mutation Mutation,
) (PromotionRecord, error) {
	record, err := repository.GetPromotion(variantID, Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return PromotionRecord{}, err
	}
	if record.Variant.ManagedBinding == nil || !reflect.DeepEqual(*record.Variant.ManagedBinding, binding) {
		return PromotionRecord{}, fmt.Errorf("%w: managed promotion binding mismatch", ErrConflict)
	}
	return repository.rollbackPromotion(ctx, variantID, mutation)
}

func (repository *Repository) rollbackPromotion(
	ctx context.Context,
	variantID string,
	mutation Mutation,
) (PromotionRecord, error) {
	if err := checkContext(ctx); err != nil {
		return PromotionRecord{}, err
	}
	if err := validateID("variant_id", variantID); err != nil {
		return PromotionRecord{}, err
	}
	if err := mutation.Validate(); err != nil {
		return PromotionRecord{}, err
	}
	event := promotionEvent{
		SchemaVersion:     promotionEventSchemaVersion,
		Type:              promotionRolledBack,
		RollbackVariantID: variantID,
		Actor:             mutation.Actor,
		Roles:             slices.Clone(mutation.Roles),
		Audit:             mutation.Audit,
		OccurredAt:        mutation.At.UTC(),
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return PromotionRecord{}, err
	}
	if existing, ok := state.events[mutation.IdempotencyKey]; ok {
		if err := repository.assertIdempotent(
			existing, promotionStream, promotionEventSchemaVersion, event, mutation,
		); err != nil {
			return PromotionRecord{}, err
		}
		return state.promotionRecordForRoles(variantID, mutation.Roles)
	}
	if err := state.validateRollbackEvent(event); err != nil {
		return PromotionRecord{}, err
	}
	if _, err := repository.appendPromotion(mutation, event); err != nil {
		return PromotionRecord{}, err
	}
	reloaded, err := repository.load()
	if err != nil {
		return PromotionRecord{}, err
	}
	return reloaded.promotionRecordForRoles(variantID, mutation.Roles)
}

func (repository *Repository) GetPromotion(
	variantID string,
	access Access,
) (PromotionRecord, error) {
	if err := validateID("variant_id", variantID); err != nil {
		return PromotionRecord{}, err
	}
	if err := access.Validate(); err != nil {
		return PromotionRecord{}, err
	}
	if !hasRole(access.Roles, RolePromotionOperator) &&
		!hasRole(access.Roles, RolePromotionApprover) {
		return PromotionRecord{}, fmt.Errorf(
			"%w: promotion_operator or promotion_approver role is required",
			ErrUnauthorized,
		)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return PromotionRecord{}, err
	}
	return state.promotionRecordForRoles(variantID, access.Roles)
}

func (repository *Repository) ListPromotions(access Access) ([]PromotionRecord, error) {
	if err := access.Validate(); err != nil {
		return nil, err
	}
	if !hasRole(access.Roles, RolePromotionOperator) &&
		!hasRole(access.Roles, RolePromotionApprover) {
		return nil, fmt.Errorf(
			"%w: promotion_operator or promotion_approver role is required",
			ErrUnauthorized,
		)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.promotions))
	for id := range state.promotions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]PromotionRecord, 0, len(ids))
	for _, id := range ids {
		record, err := state.promotionRecordForRoles(id, access.Roles)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func (repository *Repository) ActiveRevision(component PromotionComponent) (string, error) {
	if err := component.Validate(); err != nil {
		return "", err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return "", err
	}
	revision, exists := state.active[component]
	if !exists {
		return "", fmt.Errorf("%w: component %q has no active revision",
			ErrNotFound, component)
	}
	return revision, nil
}

type datasetEventType string

const (
	datasetCaseCreated                   datasetEventType = "case_created"
	datasetCaseImported                  datasetEventType = "case_imported"
	datasetLabelCorrected                datasetEventType = "label_corrected"
	datasetExposureRecorded              datasetEventType = "exposure_recorded"
	datasetCaseAnnotated                 datasetEventType = "case_annotated"
	datasetCaseAdjudicated               datasetEventType = "case_adjudicated"
	datasetCaseActivated                 datasetEventType = "case_activated"
	datasetCaseReviewAssigned            datasetEventType = "case_review_assigned"
	datasetCaseReopened                  datasetEventType = "case_reopened"
	datasetGovernanceTrustKeyRegistered  datasetEventType = "governance_trust_key_registered"
	datasetGovernanceTrustKeyRevoked     datasetEventType = "governance_trust_key_revoked"
	datasetNormalizationOracleRegistered datasetEventType = "normalization_oracle_registered"
	datasetNormalizationOracleRevoked    datasetEventType = "normalization_oracle_revoked"
)

type datasetEvent struct {
	SchemaVersion                   string                           `json:"schema_version"`
	Type                            datasetEventType                 `json:"type"`
	Case                            *EvaluationCase                  `json:"case,omitempty"`
	Import                          *ExternalGovernedCaseImport      `json:"import,omitempty"`
	Correction                      *LabelCorrection                 `json:"correction,omitempty"`
	Exposure                        *Exposure                        `json:"exposure,omitempty"`
	Annotation                      *CaseAnnotation                  `json:"annotation,omitempty"`
	Adjudication                    *CaseAdjudication                `json:"adjudication,omitempty"`
	Activation                      *CaseActivation                  `json:"activation,omitempty"`
	Assignment                      *CaseReviewAssignment            `json:"assignment,omitempty"`
	Reopen                          *CaseReopen                      `json:"reopen,omitempty"`
	TrustKeyRegistration            *GovernanceTrustKeyRegistration  `json:"trust_key_registration,omitempty"`
	TrustKeyRevocation              *GovernanceTrustKeyRevocation    `json:"trust_key_revocation,omitempty"`
	NormalizationOracleRegistration *NormalizationOracleRegistration `json:"normalization_oracle_registration,omitempty"`
	NormalizationOracleRevocation   *NormalizationOracleRevocation   `json:"normalization_oracle_revocation,omitempty"`
	Actor                           string                           `json:"actor"`
	Roles                           []Role                           `json:"roles"`
	Audit                           string                           `json:"audit"`
	OccurredAt                      time.Time                        `json:"occurred_at"`
}

type promotionEventType string

const (
	promotionRegistered   promotionEventType = "registered"
	promotionGateRecorded promotionEventType = "gate_recorded"
	promotionRolledBack   promotionEventType = "rolled_back"
)

type promotionEvent struct {
	SchemaVersion     string             `json:"schema_version"`
	Type              promotionEventType `json:"type"`
	Variant           *PromotionVariant  `json:"variant,omitempty"`
	GateResult        *GateResult        `json:"gate_result,omitempty"`
	RollbackVariantID string             `json:"rollback_variant_id,omitempty"`
	Actor             string             `json:"actor"`
	Roles             []Role             `json:"roles"`
	Audit             string             `json:"audit"`
	OccurredAt        time.Time          `json:"occurred_at"`
}

type evaluationRunEvent struct {
	SchemaVersion string               `json:"schema_version"`
	Request       EvaluationRunRequest `json:"request"`
	Run           EvaluationRun        `json:"run"`
	Actor         string               `json:"actor"`
	Roles         []Role               `json:"roles"`
	Audit         string               `json:"audit"`
	OccurredAt    time.Time            `json:"occurred_at"`
}

type experimentRunEvent struct {
	SchemaVersion string               `json:"schema_version"`
	Request       ExperimentRunRequest `json:"request"`
	Run           ExperimentRun        `json:"run"`
	Actor         string               `json:"actor"`
	Roles         []Role               `json:"roles"`
	Audit         string               `json:"audit"`
	OccurredAt    time.Time            `json:"occurred_at"`
}

type repeatabilityRunEvent struct {
	SchemaVersion string                  `json:"schema_version"`
	Request       RepeatabilityRunRequest `json:"request"`
	Run           RepeatabilityRun        `json:"run"`
	Actor         string                  `json:"actor"`
	Roles         []Role                  `json:"roles"`
	Audit         string                  `json:"audit"`
	OccurredAt    time.Time               `json:"occurred_at"`
}

type experimentBatchEventType string

const (
	experimentBatchIntentRecorded   experimentBatchEventType = "intent_recorded"
	experimentBatchResumeRequested  experimentBatchEventType = "resume_requested"
	experimentBatchAttemptFailed    experimentBatchEventType = "attempt_failed"
	experimentBatchLeaseClaimed     experimentBatchEventType = "lease_claimed"
	experimentBatchLeaseRenewed     experimentBatchEventType = "lease_renewed"
	experimentBatchCaseCompleted    experimentBatchEventType = "case_completed"
	experimentBatchTerminalRecorded experimentBatchEventType = "terminal_recorded"
)

type experimentBatchEvent struct {
	SchemaVersion  string                     `json:"schema_version"`
	Type           experimentBatchEventType   `json:"type"`
	BatchID        string                     `json:"batch_id,omitempty"`
	Request        *ExperimentBatchRequest    `json:"request,omitempty"`
	CaseResult     *ExperimentBatchCaseResult `json:"case_result,omitempty"`
	Result         *ExperimentBatchResult     `json:"result,omitempty"`
	Lease          *ExperimentBatchLease      `json:"lease,omitempty"`
	AttemptFailure *BatchAttemptFailure       `json:"attempt_failure,omitempty"`
	Actor          string                     `json:"actor"`
	Roles          []Role                     `json:"roles"`
	Audit          string                     `json:"audit"`
	OccurredAt     time.Time                  `json:"occurred_at"`
}

type exposureKey struct {
	RunID  string
	CaseID string
}

type storedEvent struct {
	stream   string
	envelope local.Envelope
}

type projectionState struct {
	cases                map[string]CaseRecord
	labels               map[string][]LabelEntry
	annotations          map[string][]CaseAnnotationEntry
	annotationEvents     map[string]CaseAnnotationEntry
	adjudications        map[string][]CaseAdjudicationEntry
	assignments          map[string][]CaseReviewAssignmentEntry
	exposures            map[exposureKey]ExposureEntry
	promotions           map[string]PromotionRecord
	evaluationRuns       map[string]EvaluationRun
	experimentRuns       map[string]ExperimentRun
	repeatabilityRuns    map[string]RepeatabilityRun
	experimentBatches    map[string]ExperimentBatchRecord
	repeatabilityBatches map[string]RepeatabilityBatchRecord
	governanceBatches    map[string]GovernanceBatchRecord
	governanceTrustKeys  map[string]GovernanceTrustKeyRecord
	normalizationOracles map[string]NormalizationOracleRecord
	active               map[PromotionComponent]string
	events               map[string]storedEvent
	streamSequences      map[string]uint64
}

func newProjectionState() *projectionState {
	return &projectionState{
		cases:                make(map[string]CaseRecord),
		labels:               make(map[string][]LabelEntry),
		annotations:          make(map[string][]CaseAnnotationEntry),
		annotationEvents:     make(map[string]CaseAnnotationEntry),
		adjudications:        make(map[string][]CaseAdjudicationEntry),
		assignments:          make(map[string][]CaseReviewAssignmentEntry),
		exposures:            make(map[exposureKey]ExposureEntry),
		promotions:           make(map[string]PromotionRecord),
		evaluationRuns:       make(map[string]EvaluationRun),
		experimentRuns:       make(map[string]ExperimentRun),
		repeatabilityRuns:    make(map[string]RepeatabilityRun),
		experimentBatches:    make(map[string]ExperimentBatchRecord),
		repeatabilityBatches: make(map[string]RepeatabilityBatchRecord),
		governanceBatches:    make(map[string]GovernanceBatchRecord),
		governanceTrustKeys:  make(map[string]GovernanceTrustKeyRecord),
		normalizationOracles: make(map[string]NormalizationOracleRecord),
		active:               make(map[PromotionComponent]string),
		events:               make(map[string]storedEvent),
		streamSequences:      make(map[string]uint64),
	}
}

func (repository *Repository) load() (*projectionState, error) {
	state := newProjectionState()
	datasetEvents, err := readOptionalStream(repository.store, datasetStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read dataset stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range datasetEvents {
		if err := state.reserveEvent(datasetStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyDataset(envelope); err != nil {
			return nil, fmt.Errorf("%w: dataset event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	governanceBatchEvents, err := readOptionalStream(repository.store, governanceBatchStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read governance batch stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range governanceBatchEvents {
		if err := state.reserveEvent(governanceBatchStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyGovernanceBatch(envelope); err != nil {
			return nil, fmt.Errorf("%w: governance batch event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	promotionEvents, err := readOptionalStream(repository.store, promotionStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read promotion stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range promotionEvents {
		if err := state.reserveEvent(promotionStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyPromotion(envelope); err != nil {
			return nil, fmt.Errorf("%w: promotion event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	runEvents, err := readOptionalStream(repository.store, evaluationRunStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read evaluation run stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range runEvents {
		if err := state.reserveEvent(evaluationRunStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyEvaluationRun(envelope); err != nil {
			return nil, fmt.Errorf("%w: evaluation run event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	experimentEvents, err := readOptionalStream(repository.store, experimentRunStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read experiment run stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range experimentEvents {
		if err := state.reserveEvent(experimentRunStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyExperimentRun(envelope); err != nil {
			return nil, fmt.Errorf("%w: experiment event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	repeatabilityEvents, err := readOptionalStream(repository.store, repeatabilityRunStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read repeatability run stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range repeatabilityEvents {
		if err := state.reserveEvent(repeatabilityRunStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyRepeatabilityRun(envelope); err != nil {
			return nil, fmt.Errorf("%w: repeatability event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	repeatabilityBatchEvents, err := readOptionalStream(repository.store, repeatabilityBatchStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read repeatability batch stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range repeatabilityBatchEvents {
		if err := state.reserveEvent(repeatabilityBatchStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyRepeatabilityBatch(envelope); err != nil {
			return nil, fmt.Errorf("%w: repeatability batch event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	batchEvents, err := readOptionalStream(repository.store, experimentBatchStream)
	if err != nil {
		return nil, fmt.Errorf("%w: read experiment batch stream: %v", ErrCorrupt, err)
	}
	for _, envelope := range batchEvents {
		if err := state.reserveEvent(experimentBatchStream, envelope); err != nil {
			return nil, err
		}
		if err := state.applyExperimentBatch(envelope); err != nil {
			return nil, fmt.Errorf("%w: experiment batch event %q sequence %d: %v",
				ErrCorrupt, envelope.ID, envelope.Sequence, err)
		}
	}
	incidents, err := repository.loadIncidents()
	if err != nil {
		return nil, err
	}
	for id, envelope := range incidents.events {
		if _, exists := state.events[id]; exists {
			return nil, corruptf("event id %q appears in multiple evaluation streams", id)
		}
		state.events[id] = storedEvent{stream: incidentStream, envelope: envelope}
	}
	probes, err := repository.loadProbes()
	if err != nil {
		return nil, err
	}
	for id, envelope := range probes.events {
		if _, exists := state.events[id]; exists {
			return nil, corruptf("event id %q appears in multiple evaluation streams", id)
		}
		state.events[id] = storedEvent{stream: probeStream, envelope: envelope}
	}
	return state, nil
}

func (state *projectionState) applyEvaluationRun(envelope local.Envelope) error {
	if envelope.Schema != evaluationRunEventSchemaVersion {
		return fmt.Errorf("unsupported evaluation run event schema %q", envelope.Schema)
	}
	var event evaluationRunEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode evaluation run event: %w", err)
	}
	if event.SchemaVersion != evaluationRunEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
		return err
	}
	if err := event.Request.Validate(); err != nil {
		return err
	}
	if err := event.Run.Validate(); err != nil {
		return err
	}
	if event.Run.EvaluationRunID != event.Request.EvaluationRunID ||
		event.Run.EvaluatorRevision != event.Request.EvaluatorRevision ||
		!event.Run.CreatedAt.Equal(event.Request.CreatedAt.UTC()) ||
		!event.Run.RecordedAt.Equal(event.OccurredAt) || event.Run.RecordedBy != event.Actor ||
		len(event.Run.Results) != len(event.Request.Bindings) {
		return fmt.Errorf("evaluation run does not bind its request and event")
	}
	if _, exists := state.evaluationRuns[event.Run.EvaluationRunID]; exists {
		return fmt.Errorf("duplicate evaluation_run_id %q", event.Run.EvaluationRunID)
	}
	for index, binding := range event.Request.Bindings {
		result := event.Run.Results[index]
		if result.CaseID != binding.CaseID || result.LabelRevision != binding.ExpectedLabelRevision ||
			result.ReviewRunID != binding.ReviewRunID {
			return fmt.Errorf("evaluation run result %d does not bind request", index)
		}
		if binding.ApplyTrial == nil {
			if result.ApplyTrialID != "" || result.ApplyEditScriptRef != nil {
				return fmt.Errorf("evaluation run result %d invented apply evidence", index)
			}
		} else if result.ApplyTrialID != binding.ApplyTrial.TrialID ||
			result.ApplyEditScriptRef == nil ||
			*result.ApplyEditScriptRef != binding.ApplyTrial.EditScriptRef {
			return fmt.Errorf("evaluation run result %d does not bind apply trial", index)
		}
		applyFacts := projectApplyTrial(binding.ApplyTrial)
		if result.ApplyTrialID != applyFacts.trialID ||
			!reflect.DeepEqual(result.ApplyEditScriptRef, applyFacts.editScriptRef) ||
			result.ApplyChecksExecuted != applyFacts.executed ||
			result.ApplyChecksPassed != applyFacts.passed ||
			result.ApplyChecksFailed != applyFacts.failed ||
			result.ApplyAuthority != applyFacts.authority ||
			result.ApplyFidelity != applyFacts.availability {
			return fmt.Errorf("evaluation run result %d changed apply trial projection", index)
		}
		record, exists := state.cases[binding.CaseID]
		if !exists || authorizeExposure(record.CurrentCase(), event.Roles) != nil {
			return fmt.Errorf("evaluation run references unauthorized or unknown case")
		}
	}
	state.evaluationRuns[event.Run.EvaluationRunID] = cloneValue(event.Run)
	return nil
}

func (state *projectionState) applyExperimentRun(envelope local.Envelope) error {
	if envelope.Schema != experimentRunEventSchemaVersion {
		return fmt.Errorf("unsupported experiment event schema %q", envelope.Schema)
	}
	var event experimentRunEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode experiment event: %w", err)
	}
	if event.SchemaVersion != experimentRunEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
		return err
	}
	if err := event.Request.Validate(); err != nil {
		return err
	}
	if err := event.Run.Validate(); err != nil {
		return err
	}
	if event.Run.ExperimentRunID != event.Request.ExperimentRunID ||
		event.Run.ExperimentRevision != event.Request.ExperimentRevision ||
		event.Run.BaselineEvaluationRunID != event.Request.BaselineEvaluationRunID ||
		event.Run.VariantEvaluationRunID != event.Request.VariantEvaluationRunID ||
		event.Run.Variable != event.Request.Variable ||
		!event.Run.CreatedAt.Equal(event.Request.CreatedAt.UTC()) ||
		!event.Run.RecordedAt.Equal(event.OccurredAt) || event.Run.RecordedBy != event.Actor {
		return fmt.Errorf("experiment run does not bind request and event")
	}
	baseline, baselineExists := state.evaluationRuns[event.Run.BaselineEvaluationRunID]
	variant, variantExists := state.evaluationRuns[event.Run.VariantEvaluationRunID]
	if !baselineExists || !variantExists {
		return fmt.Errorf("experiment references missing EvaluationRun")
	}
	if err := authorizeExperimentRuns(state, baseline, variant, event.Roles); err != nil {
		return err
	}
	if err := validateExperimentAgainstEvaluationRuns(event.Run, baseline, variant); err != nil {
		return err
	}
	if _, exists := state.experimentRuns[event.Run.ExperimentRunID]; exists {
		return fmt.Errorf("duplicate experiment_run_id")
	}
	state.experimentRuns[event.Run.ExperimentRunID] = cloneValue(event.Run)
	return nil
}

func (state *projectionState) applyRepeatabilityRun(envelope local.Envelope) error {
	if envelope.Schema != repeatabilityRunEventSchemaVersion {
		return fmt.Errorf("unsupported repeatability event schema %q", envelope.Schema)
	}
	var event repeatabilityRunEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode repeatability event: %w", err)
	}
	if event.SchemaVersion != repeatabilityRunEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
		return err
	}
	if err := event.Request.Validate(); err != nil {
		return err
	}
	if err := event.Run.Validate(); err != nil {
		return err
	}
	if event.Run.RepeatabilityRunID != event.Request.RepeatabilityRunID ||
		event.Run.RepeatabilityRevision != event.Request.RepeatabilityRevision ||
		event.Run.BaselineEvaluationRunID != event.Request.BaselineEvaluationRunID ||
		!slices.Equal(event.Run.ReplayEvaluationRunIDs, event.Request.ReplayEvaluationRunIDs) ||
		!event.Run.CreatedAt.Equal(event.Request.CreatedAt.UTC()) ||
		!event.Run.RecordedAt.Equal(event.OccurredAt) || event.Run.RecordedBy != event.Actor {
		return fmt.Errorf("repeatability run does not bind request and event")
	}
	evaluationRuns, err := loadRepeatabilityEvaluationRuns(state, event.Request)
	if err != nil {
		return err
	}
	if err := authorizeEvaluationRuns(state, evaluationRuns, event.Roles); err != nil {
		return err
	}
	if err := validateRepeatabilityAgainstEvaluationRuns(event.Run, evaluationRuns); err != nil {
		return err
	}
	if _, exists := state.repeatabilityRuns[event.Run.RepeatabilityRunID]; exists {
		return fmt.Errorf("duplicate repeatability_run_id")
	}
	state.repeatabilityRuns[event.Run.RepeatabilityRunID] = cloneValue(event.Run)
	return nil
}

func (state *projectionState) applyExperimentBatch(envelope local.Envelope) error {
	if envelope.Schema != experimentBatchEventSchemaVersion {
		return fmt.Errorf("unsupported experiment batch event schema %q", envelope.Schema)
	}
	var event experimentBatchEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode experiment batch event: %w", err)
	}
	if event.SchemaVersion != experimentBatchEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt); err != nil {
		return err
	}
	switch event.Type {
	case experimentBatchIntentRecorded:
		if event.Request == nil || event.CaseResult != nil || event.Result != nil || event.Lease != nil || event.AttemptFailure != nil || event.BatchID != "" {
			return fmt.Errorf("intent event must contain only request")
		}
		if err := event.Request.Validate(); err != nil {
			return err
		}
		if !event.Request.CreatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("batch request created_at does not match event")
		}
		if _, exists := state.experimentBatches[event.Request.BatchID]; exists {
			return fmt.Errorf("duplicate experiment batch")
		}
		state.experimentBatches[event.Request.BatchID] = ExperimentBatchRecord{
			Request: cloneValue(*event.Request),
			Intent: Mutation{
				IdempotencyKey: envelope.ID, Actor: event.Actor, Roles: slices.Clone(event.Roles),
				Audit: event.Audit, At: event.OccurredAt,
			},
			Status:         ExperimentBatchRunning,
			CompletedCases: []ExperimentBatchCaseResult{},
			UpdatedAt:      event.OccurredAt, UpdatedBy: event.Actor,
		}
	case experimentBatchResumeRequested:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease != nil || event.AttemptFailure != nil || event.BatchID == "" {
			return fmt.Errorf("resume_requested event shape is invalid")
		}
		record, exists := state.experimentBatches[event.BatchID]
		if !exists || record.Status != ExperimentBatchRunning {
			return fmt.Errorf("resume request references missing or terminal batch")
		}
		if event.OccurredAt.Before(record.UpdatedAt) {
			return fmt.Errorf("resume request predates batch state")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeExperimentRuns(state, baseline, baseline, event.Roles); err != nil {
			return err
		}
		record.LastResume = &Mutation{
			IdempotencyKey: envelope.ID, Actor: event.Actor, Roles: slices.Clone(event.Roles),
			Audit: event.Audit, At: event.OccurredAt,
		}
		record.UpdatedAt = event.OccurredAt
		state.experimentBatches[event.BatchID] = record
	case experimentBatchAttemptFailed:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease == nil || event.AttemptFailure == nil || event.BatchID == "" {
			return fmt.Errorf("attempt_failed event shape is invalid")
		}
		if err := event.Lease.validate(); err != nil {
			return err
		}
		if err := event.AttemptFailure.validate(); err != nil {
			return err
		}
		record, exists := state.experimentBatches[event.BatchID]
		if !exists || record.Status != ExperimentBatchRunning || record.ActiveLease == nil ||
			*record.ActiveLease != *event.Lease {
			return fmt.Errorf("attempt failure references missing, terminal, or stale batch lease")
		}
		failure := event.AttemptFailure
		if event.Actor != failure.WorkerID || event.Lease.WorkerID != failure.WorkerID ||
			event.Lease.LeaseID != failure.LeaseID || event.Lease.Generation != failure.Generation ||
			event.Lease.FencingToken != failure.FencingToken ||
			!event.OccurredAt.Equal(failure.ObservedAt) || event.OccurredAt.Before(record.UpdatedAt) {
			return fmt.Errorf("attempt failure does not bind exact lease and observation")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeExperimentRuns(state, baseline, baseline, event.Roles); err != nil {
			return err
		}
		record.LastFailure = clonePointer(*failure)
		record.UpdatedAt = event.OccurredAt
		state.experimentBatches[event.BatchID] = record
	case experimentBatchLeaseClaimed:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease == nil || event.AttemptFailure != nil || event.BatchID == "" {
			return fmt.Errorf("lease_claimed event shape is invalid")
		}
		if err := event.Lease.validate(); err != nil {
			return err
		}
		record, exists := state.experimentBatches[event.BatchID]
		if !exists || record.Status != ExperimentBatchRunning || event.Lease.BatchID != event.BatchID {
			return fmt.Errorf("lease claim references missing or terminal batch")
		}
		if event.Actor != record.UpdatedBy || !event.Lease.ClaimedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("lease claim authority or time differs")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeExperimentRuns(state, baseline, baseline, event.Roles); err != nil {
			return err
		}
		if record.ActiveLease != nil && record.ActiveLease.ExpiresAt.After(event.OccurredAt) {
			return fmt.Errorf("lease claim overlaps active lease")
		}
		wantGeneration, wantToken := uint64(1), uint64(1)
		if record.LastLease != nil {
			wantGeneration = record.LastLease.Generation + 1
			wantToken = record.LastLease.FencingToken + 1
		}
		if event.Lease.Generation != wantGeneration || event.Lease.FencingToken != wantToken {
			return fmt.Errorf("lease generation or fencing token is not monotonic")
		}
		record.ActiveLease = clonePointer(*event.Lease)
		record.LastLease = clonePointer(*event.Lease)
		record.UpdatedAt = event.OccurredAt
		state.experimentBatches[event.BatchID] = record
	case experimentBatchLeaseRenewed:
		if event.Request != nil || event.CaseResult != nil || event.Result != nil || event.Lease == nil || event.AttemptFailure != nil || event.BatchID == "" {
			return fmt.Errorf("lease_renewed event shape is invalid")
		}
		if err := event.Lease.validate(); err != nil {
			return err
		}
		record, exists := state.experimentBatches[event.BatchID]
		if !exists || record.Status != ExperimentBatchRunning || record.ActiveLease == nil {
			return fmt.Errorf("lease renewal references missing, terminal, or unclaimed batch")
		}
		previous := *record.ActiveLease
		if event.Actor != record.UpdatedBy || event.Lease.BatchID != event.BatchID ||
			event.Lease.LeaseID != previous.LeaseID || event.Lease.WorkerID != previous.WorkerID ||
			event.Lease.Generation != previous.Generation || event.Lease.FencingToken != previous.FencingToken ||
			!event.Lease.ClaimedAt.Equal(previous.ClaimedAt) || event.OccurredAt.After(previous.ExpiresAt) ||
			!event.Lease.ExpiresAt.After(previous.ExpiresAt) {
			return fmt.Errorf("lease renewal does not extend the exact active lease")
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeExperimentRuns(state, baseline, baseline, event.Roles); err != nil {
			return err
		}
		record.ActiveLease = clonePointer(*event.Lease)
		record.LastLease = clonePointer(*event.Lease)
		record.UpdatedAt = event.OccurredAt
		state.experimentBatches[event.BatchID] = record
	case experimentBatchCaseCompleted:
		if event.Request != nil || event.CaseResult == nil || event.Result != nil || event.Lease == nil || event.AttemptFailure != nil {
			return fmt.Errorf("case_completed event must contain only batch_id and case_result")
		}
		if err := validateID("batch_id", event.BatchID); err != nil {
			return err
		}
		if err := event.CaseResult.validate(); err != nil {
			return err
		}
		record, exists := state.experimentBatches[event.BatchID]
		if !exists || record.Status != ExperimentBatchRunning || record.Result != nil {
			return fmt.Errorf("batch case checkpoint references missing or terminal intent")
		}
		if event.Actor != record.UpdatedBy {
			return fmt.Errorf("batch case checkpoint actor differs from intent actor")
		}
		if err := validateBatchLease(record, *event.Lease, event.OccurredAt); err != nil {
			return err
		}
		baseline := state.evaluationRuns[record.Request.BaselineEvaluationRunID]
		if err := authorizeExperimentRuns(state, baseline, baseline, event.Roles); err != nil {
			return err
		}
		if err := validateBatchCaseCheckpoint(record, *event.CaseResult); err != nil {
			return err
		}
		record.CompletedCases = append(record.CompletedCases, cloneValue(*event.CaseResult))
		record.CompletedCases = sortedBatchResults(record.CompletedCases)
		record.UpdatedAt = event.OccurredAt
		state.experimentBatches[event.BatchID] = record
	case experimentBatchTerminalRecorded:
		if event.Result == nil || event.Request != nil || event.CaseResult != nil || event.Lease == nil || event.AttemptFailure != nil || event.BatchID != "" {
			return fmt.Errorf("terminal event must contain only result")
		}
		if err := event.Result.Validate(); err != nil {
			return err
		}
		if !event.Result.CompletedAt.Equal(event.OccurredAt) || event.Result.CompletedBy != event.Actor {
			return fmt.Errorf("batch result completion does not match event")
		}
		record, exists := state.experimentBatches[event.Result.BatchID]
		if !exists || record.Status != ExperimentBatchRunning || record.Result != nil {
			return fmt.Errorf("batch terminal references missing or terminal intent")
		}
		if event.Actor != record.UpdatedBy {
			return fmt.Errorf("batch terminal actor differs from intent actor")
		}
		if err := validateBatchLease(record, *event.Lease, event.OccurredAt); err != nil {
			return err
		}
		if event.Result.VariantEvaluationRunID != record.Request.VariantEvaluationRunID ||
			event.Result.ExperimentRunID != record.Request.ExperimentRunID ||
			len(event.Result.Cases) != len(record.Request.Cases) {
			return fmt.Errorf("batch terminal does not bind request")
		}
		if !reflect.DeepEqual(record.CompletedCases, event.Result.Cases) {
			return fmt.Errorf("batch terminal does not bind durable case checkpoints")
		}
		for index, result := range event.Result.Cases {
			requestCase := record.Request.Cases[index]
			if result.CaseID != requestCase.CaseID ||
				result.BaselineReviewRunID != requestCase.BaselineReviewRunID {
				return fmt.Errorf("batch terminal case %d does not bind request", index)
			}
		}
		record.Status = ExperimentBatchSucceeded
		record.Result = clonePointer(*event.Result)
		record.ActiveLease = nil
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		state.experimentBatches[event.Result.BatchID] = record
	default:
		return fmt.Errorf("unsupported experiment batch event type %q", event.Type)
	}
	return nil
}

func readOptionalStream(store *local.Store, stream string) ([]local.Envelope, error) {
	envelopes, err := store.ReadJSONL(stream)
	if errors.Is(err, os.ErrNotExist) {
		return []local.Envelope{}, nil
	}
	return envelopes, err
}

func (state *projectionState) reserveEvent(stream string, envelope local.Envelope) error {
	if previous, exists := state.events[envelope.ID]; exists {
		return corruptf("event id %q appears in both %s and %s",
			envelope.ID, previous.stream, stream)
	}
	state.events[envelope.ID] = storedEvent{stream: stream, envelope: envelope}
	state.streamSequences[stream] = envelope.Sequence
	return nil
}

func (state *projectionState) applyDataset(envelope local.Envelope) error {
	if envelope.Schema != datasetEventSchemaVersion {
		return fmt.Errorf("unsupported dataset event schema %q", envelope.Schema)
	}
	var event datasetEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode dataset event: %w", err)
	}
	if event.SchemaVersion != datasetEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(
		envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt,
	); err != nil {
		return err
	}
	switch event.Type {
	case datasetCaseCreated:
		if event.Case == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("case_created must contain only case")
		}
		if err := event.Case.Validate(); err != nil {
			return err
		}
		if !event.Case.CreatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("case created_at does not match event time")
		}
		if event.Case.ReviewState != ReviewPending ||
			event.Case.DatasetState != DatasetCandidatePool ||
			event.Case.Split != SplitUnassigned || event.Case.Eligibility != (Eligibility{}) {
			return fmt.Errorf("case_created is restricted to pending candidate-pool ingress")
		}
		if err := authorizeCaseCreate(*event.Case, event.Roles); err != nil {
			return err
		}
		if _, exists := state.cases[event.Case.CaseID]; exists {
			return fmt.Errorf("duplicate case_id %q", event.Case.CaseID)
		}
		if err := state.validateCaseAddition(*event.Case); err != nil {
			return err
		}
		state.cases[event.Case.CaseID] = CaseRecord{
			Case: cloneValue(*event.Case),
			CurrentGovernance: CaseGovernance{
				Revision:       1,
				ReviewState:    event.Case.ReviewState,
				DatasetState:   event.Case.DatasetState,
				Split:          event.Case.Split,
				Eligibility:    event.Case.Eligibility,
				LicenseConsent: cloneValue(event.Case.LicenseConsent),
				UpdatedAt:      event.OccurredAt,
				UpdatedBy:      event.Actor,
			},
			CurrentLabel:               cloneValue(event.Case.Label),
			CurrentLabelPolicyRevision: event.Case.LabelPolicyRevision,
			CurrentLabelRevision:       1,
			UpdatedAt:                  event.OccurredAt,
			UpdatedBy:                  event.Actor,
		}
		state.labels[event.Case.CaseID] = []LabelEntry{{
			Revision:              1,
			EventID:               envelope.ID,
			Label:                 cloneValue(event.Case.Label),
			LabelPolicyRevision:   event.Case.LabelPolicyRevision,
			Reason:                "initial_label",
			AffectedExperimentIDs: []string{},
			Actor:                 event.Actor,
			At:                    event.OccurredAt,
		}}
	case datasetCaseImported:
		if event.Import == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("case_imported must contain only import")
		}
		request := *event.Import
		if err := request.Validate(); err != nil {
			return err
		}
		if !request.ImportedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("imported_at does not match event time")
		}
		if slices.Contains(request.Attestation.ReviewerIDs, event.Actor) ||
			request.Attestation.AdjudicatorID == event.Actor {
			return fmt.Errorf("import operator is not independent from external governance actors")
		}
		if err := authorizeGovernedCaseImport(request.Case, event.Roles); err != nil {
			return err
		}
		if err := state.authorizeGovernanceKeyForImport(request, event.Actor); err != nil {
			return err
		}
		if _, exists := state.cases[request.Case.CaseID]; exists {
			return fmt.Errorf("duplicate case_id %q", request.Case.CaseID)
		}
		if err := state.validateCaseAddition(request.Case); err != nil {
			return err
		}
		governanceRecord := ExternalGovernanceImportRecord{
			EventID: envelope.ID, Attestation: cloneValue(request.Attestation),
			TrustedKey: cloneValue(request.TrustedKey), ImportedAt: request.ImportedAt,
			ImportedBy: event.Actor,
		}
		state.cases[request.Case.CaseID] = CaseRecord{
			Case: cloneValue(request.Case),
			CurrentGovernance: CaseGovernance{
				Revision: 1, ReviewState: request.Case.ReviewState,
				DatasetState: request.Case.DatasetState, Split: request.Case.Split,
				Eligibility:    request.Case.Eligibility,
				LicenseConsent: cloneValue(request.Case.LicenseConsent),
				UpdatedAt:      event.OccurredAt, UpdatedBy: event.Actor,
			},
			CurrentLabel:               cloneValue(request.Case.Label),
			CurrentLabelPolicyRevision: request.Case.LabelPolicyRevision,
			CurrentLabelRevision:       1, UpdatedAt: event.OccurredAt, UpdatedBy: event.Actor,
			ExternalGovernance: clonePointer(governanceRecord),
		}
		state.labels[request.Case.CaseID] = []LabelEntry{{
			Revision: 1, EventID: envelope.ID, Label: cloneValue(request.Case.Label),
			LabelPolicyRevision:   request.Case.LabelPolicyRevision,
			Reason:                "external_governance_import:" + request.Attestation.AttestationID,
			AffectedExperimentIDs: []string{}, Actor: event.Actor, At: event.OccurredAt,
		}}
	case datasetGovernanceTrustKeyRegistered:
		if event.TrustKeyRegistration == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("governance_trust_key_registered must contain only trust_key_registration")
		}
		registration := *event.TrustKeyRegistration
		if err := registration.Validate(); err != nil {
			return err
		}
		if !registration.RegisteredAt.Equal(event.OccurredAt) {
			return fmt.Errorf("registered_at does not match event time")
		}
		if err := authorizeGovernanceTrustAdmin(event.Roles); err != nil {
			return err
		}
		identity := governanceTrustKeyIdentity(registration.Key.Authority, registration.Key.KeyID, registration.Key.Revision)
		if _, exists := state.governanceTrustKeys[identity]; exists {
			return fmt.Errorf("duplicate governance trust key %q/%q/%q", registration.Key.Authority, registration.Key.KeyID, registration.Key.Revision)
		}
		state.governanceTrustKeys[identity] = GovernanceTrustKeyRecord{
			Key: cloneValue(registration.Key), RegisteredEventID: envelope.ID,
			RegisteredAt: registration.RegisteredAt, RegisteredBy: event.Actor,
		}
	case datasetGovernanceTrustKeyRevoked:
		if event.TrustKeyRevocation == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("governance_trust_key_revoked must contain only trust_key_revocation")
		}
		revocation := *event.TrustKeyRevocation
		if err := revocation.Validate(); err != nil {
			return err
		}
		if !revocation.RevokedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("revoked_at does not match event time")
		}
		if err := authorizeGovernanceTrustAdmin(event.Roles); err != nil {
			return err
		}
		identity := governanceTrustKeyIdentity(revocation.Authority, revocation.KeyID, revocation.Revision)
		record, exists := state.governanceTrustKeys[identity]
		if !exists {
			return fmt.Errorf("governance trust key revocation references unknown key")
		}
		if record.RevokedAt != nil {
			return fmt.Errorf("governance trust key is already revoked")
		}
		if revocation.RevokedAt.Before(record.RegisteredAt) {
			return fmt.Errorf("governance trust key revocation predates registration")
		}
		revokedAt := revocation.RevokedAt
		record.RevokedEventID = envelope.ID
		record.RevokedAt = &revokedAt
		record.RevokedBy = event.Actor
		record.RevocationReason = revocation.Reason
		state.governanceTrustKeys[identity] = record
	case datasetNormalizationOracleRegistered:
		if event.NormalizationOracleRegistration == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("normalization_oracle_registered must contain only normalization_oracle_registration")
		}
		registration := *event.NormalizationOracleRegistration
		if err := registration.Validate(); err != nil {
			return err
		}
		if !registration.RegisteredAt.Equal(event.OccurredAt) {
			return fmt.Errorf("normalization oracle registered_at does not match event time")
		}
		if err := state.validateNormalizationOracleRegistration(registration, event.Actor, event.Roles); err != nil {
			return err
		}
		keyIdentity := governanceTrustKeyIdentity(
			registration.TrustedKey.Authority,
			registration.TrustedKey.KeyID,
			registration.TrustedKey.Revision,
		)
		keyRecord, exists := state.governanceTrustKeys[keyIdentity]
		if !exists {
			return fmt.Errorf("normalization oracle registration references unknown trust key")
		}
		state.normalizationOracles[registration.Oracle.OracleID] = normalizationOracleRecordFromRegistration(
			registration, envelope.ID, event.Actor, keyRecord,
		)
	case datasetNormalizationOracleRevoked:
		if event.NormalizationOracleRevocation == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("normalization_oracle_revoked must contain only normalization_oracle_revocation")
		}
		revocation := *event.NormalizationOracleRevocation
		if err := revocation.Validate(); err != nil {
			return err
		}
		if !revocation.RevokedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("normalization oracle revoked_at does not match event time")
		}
		if err := state.validateNormalizationOracleRevocation(revocation, event.Actor, event.Roles); err != nil {
			return err
		}
		record := state.normalizationOracles[revocation.OracleID]
		revokedAt := revocation.RevokedAt
		record.RevokedEventID = envelope.ID
		record.RevokedAt = &revokedAt
		record.RevokedBy = event.Actor
		record.RevocationReason = revocation.Reason
		state.normalizationOracles[revocation.OracleID] = record
	case datasetLabelCorrected:
		if event.Correction == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("label_corrected must contain only correction")
		}
		correction := *event.Correction
		if err := correction.Validate(); err != nil {
			return err
		}
		record, exists := state.cases[correction.CaseID]
		if !exists {
			return fmt.Errorf("label correction references unknown case")
		}
		if err := authorizeCaseWrite(record.CurrentCase(), event.Roles); err != nil {
			return err
		}
		if record.CurrentGovernance.DatasetState == DatasetRetired {
			return fmt.Errorf("retired case cannot receive label corrections")
		}
		if record.CurrentGovernance.ReviewState == ReviewInReview {
			return fmt.Errorf("label correction cannot mutate an assigned review round")
		}
		if correction.ExpectedLabelRevision != record.CurrentLabelRevision {
			return fmt.Errorf("label revision is %d, expected %d",
				record.CurrentLabelRevision, correction.ExpectedLabelRevision)
		}
		if err := validateLabelForCase(record.Case.Type, correction.Label); err != nil {
			return err
		}
		if reflect.DeepEqual(correction.Label, record.CurrentLabel) &&
			correction.LabelPolicyRevision == record.CurrentLabelPolicyRevision {
			return fmt.Errorf("label correction is a no-op")
		}
		affected := state.affectedExperiments(correction.CaseID)
		if !slices.Equal(affected, correction.AffectedExperimentIDs) {
			return fmt.Errorf("affected experiments are %v, want %v",
				correction.AffectedExperimentIDs, affected)
		}
		record.CurrentLabel = cloneValue(correction.Label)
		record.CurrentLabelPolicyRevision = correction.LabelPolicyRevision
		record.CurrentLabelRevision++
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		state.cases[correction.CaseID] = record
		state.labels[correction.CaseID] = append(
			state.labels[correction.CaseID],
			LabelEntry{
				Revision:              record.CurrentLabelRevision,
				EventID:               envelope.ID,
				Label:                 cloneValue(correction.Label),
				LabelPolicyRevision:   correction.LabelPolicyRevision,
				Reason:                correction.Reason,
				AffectedExperimentIDs: slices.Clone(correction.AffectedExperimentIDs),
				Actor:                 event.Actor,
				At:                    event.OccurredAt,
			},
		)
	case datasetExposureRecorded:
		if event.Exposure == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("exposure_recorded must contain only exposure")
		}
		exposure := *event.Exposure
		if err := exposure.Validate(); err != nil {
			return err
		}
		if exposure.ObservedAt.After(event.OccurredAt) {
			return fmt.Errorf("exposure observed_at is after event time")
		}
		record, exists := state.cases[exposure.CaseID]
		if !exists {
			return fmt.Errorf("exposure references unknown case")
		}
		if record.CurrentGovernance.DatasetState != DatasetActive {
			return fmt.Errorf("exposure references non-active case")
		}
		if exposure.ObservedAt.Before(record.Case.CreatedAt) {
			return fmt.Errorf("exposure predates case")
		}
		if err := authorizeExposure(record.CurrentCase(), event.Roles); err != nil {
			return err
		}
		key := exposureKey{RunID: exposure.EvaluationRunID, CaseID: exposure.CaseID}
		if _, exists := state.exposures[key]; exists {
			return fmt.Errorf("duplicate exposure for run and case")
		}
		state.exposures[key] = ExposureEntry{
			EventID:  envelope.ID,
			Exposure: cloneValue(exposure),
			Actor:    event.Actor,
			At:       event.OccurredAt,
		}
	case datasetCaseReviewAssigned:
		if event.Assignment == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("case_review_assigned must contain only assignment")
		}
		assignment := *event.Assignment
		if err := assignment.Validate(); err != nil {
			return err
		}
		if !assignment.AssignedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("assignment assigned_at does not match event time")
		}
		record, exists := state.cases[assignment.CaseID]
		if !exists {
			return fmt.Errorf("assignment references unknown case")
		}
		if err := state.validateReviewAssignment(record, assignment, event.Actor, event.Roles); err != nil {
			return err
		}
		entry := CaseReviewAssignmentEntry{EventID: envelope.ID, Assignment: cloneValue(assignment), AssignedBy: event.Actor, At: event.OccurredAt}
		state.assignments[assignment.CaseID] = append(state.assignments[assignment.CaseID], entry)
		record.CurrentGovernance.Revision++
		record.CurrentGovernance.ReviewState = ReviewInReview
		record.CurrentGovernance.UpdatedAt = event.OccurredAt
		record.CurrentGovernance.UpdatedBy = event.Actor
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		state.cases[assignment.CaseID] = record
	case datasetCaseAnnotated:
		if event.Annotation == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("case_annotated must contain only annotation")
		}
		annotation := *event.Annotation
		if err := annotation.Validate(); err != nil {
			return err
		}
		if !annotation.ReviewedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("annotation reviewed_at does not match event time")
		}
		record, exists := state.cases[annotation.CaseID]
		if !exists {
			return fmt.Errorf("annotation references unknown case")
		}
		if err := state.validateAnnotation(record, annotation, event.Actor, event.Roles); err != nil {
			return err
		}
		entry := CaseAnnotationEntry{
			EventID: envelope.ID, Annotation: cloneValue(annotation),
			Reviewer: event.Actor, At: event.OccurredAt,
		}
		state.annotations[annotation.CaseID] = append(state.annotations[annotation.CaseID], entry)
		state.annotationEvents[envelope.ID] = entry
	case datasetCaseAdjudicated:
		if event.Adjudication == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("case_adjudicated must contain only adjudication")
		}
		adjudication := *event.Adjudication
		if err := adjudication.Validate(); err != nil {
			return err
		}
		if !adjudication.AdjudicatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("adjudication adjudicated_at does not match event time")
		}
		record, exists := state.cases[adjudication.CaseID]
		if !exists {
			return fmt.Errorf("adjudication references unknown case")
		}
		if err := state.validateAdjudication(record, adjudication, event.Actor, event.Roles); err != nil {
			return err
		}
		entry := CaseAdjudicationEntry{
			EventID: envelope.ID, Adjudication: cloneValue(adjudication),
			Adjudicator: event.Actor, At: event.OccurredAt,
		}
		state.adjudications[adjudication.CaseID] = append(state.adjudications[adjudication.CaseID], entry)
		if adjudication.Outcome == AdjudicationApprove {
			record.CurrentGovernance.ReviewState = ReviewApproved
			record.CurrentGovernance.DatasetState = DatasetGold
			if !reflect.DeepEqual(record.CurrentLabel, *adjudication.SelectedLabel) ||
				record.CurrentLabelPolicyRevision != adjudication.LabelPolicyRevision {
				record.CurrentLabel = cloneValue(*adjudication.SelectedLabel)
				record.CurrentLabelPolicyRevision = adjudication.LabelPolicyRevision
				record.CurrentLabelRevision++
				state.labels[adjudication.CaseID] = append(state.labels[adjudication.CaseID], LabelEntry{
					Revision: record.CurrentLabelRevision, EventID: envelope.ID,
					Label:                 cloneValue(*adjudication.SelectedLabel),
					LabelPolicyRevision:   adjudication.LabelPolicyRevision,
					Reason:                "candidate_adjudication: " + adjudication.Rationale,
					AffectedExperimentIDs: []string{}, Actor: event.Actor, At: event.OccurredAt,
				})
			}
		} else {
			record.CurrentGovernance.ReviewState = ReviewRejected
			record.CurrentGovernance.DatasetState = DatasetRetired
		}
		record.CurrentGovernance.Split = SplitUnassigned
		record.CurrentGovernance.Eligibility = Eligibility{}
		record.CurrentGovernance.Revision++
		record.CurrentGovernance.UpdatedAt = event.OccurredAt
		record.CurrentGovernance.UpdatedBy = event.Actor
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		state.cases[adjudication.CaseID] = record
	case datasetCaseActivated:
		if event.Activation == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("case_activated must contain only activation")
		}
		activation := *event.Activation
		if err := activation.Validate(); err != nil {
			return err
		}
		if !activation.ActivatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("activation activated_at does not match event time")
		}
		record, exists := state.cases[activation.CaseID]
		if !exists {
			return fmt.Errorf("activation references unknown case")
		}
		if err := state.validateActivation(record, activation, event.Roles); err != nil {
			return err
		}
		record.CurrentGovernance.Revision++
		record.CurrentGovernance.DatasetState = DatasetActive
		record.CurrentGovernance.Split = activation.Split
		record.CurrentGovernance.Eligibility = activation.Eligibility
		record.CurrentGovernance.LicenseConsent = cloneValue(activation.LicenseConsent)
		record.CurrentGovernance.UpdatedAt = event.OccurredAt
		record.CurrentGovernance.UpdatedBy = event.Actor
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		state.cases[activation.CaseID] = record
	case datasetCaseReopened:
		if event.Reopen == nil || datasetPayloadCount(event) != 1 {
			return fmt.Errorf("case_reopened must contain only reopen")
		}
		reopen := *event.Reopen
		if err := reopen.Validate(); err != nil {
			return err
		}
		if !reopen.ReopenedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("reopen reopened_at does not match event time")
		}
		record, exists := state.cases[reopen.CaseID]
		if !exists {
			return fmt.Errorf("reopen references unknown case")
		}
		if err := state.validateReopen(record, reopen, event.Roles); err != nil {
			return err
		}
		record.CurrentGovernance.Revision++
		record.CurrentGovernance.ReviewState = ReviewPending
		record.CurrentGovernance.DatasetState = DatasetCandidatePool
		record.CurrentGovernance.Split = SplitUnassigned
		record.CurrentGovernance.Eligibility = Eligibility{}
		record.CurrentGovernance.LicenseConsent = cloneValue(record.Case.LicenseConsent)
		record.CurrentGovernance.UpdatedAt = event.OccurredAt
		record.CurrentGovernance.UpdatedBy = event.Actor
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		state.cases[reopen.CaseID] = record
	default:
		return fmt.Errorf("unsupported dataset event type %q", event.Type)
	}
	return nil
}

func datasetPayloadCount(event datasetEvent) int {
	count := 0
	for _, present := range []bool{
		event.Case != nil, event.Import != nil, event.Correction != nil, event.Exposure != nil,
		event.Annotation != nil, event.Adjudication != nil, event.Activation != nil,
		event.Assignment != nil, event.Reopen != nil, event.TrustKeyRegistration != nil,
		event.TrustKeyRevocation != nil, event.NormalizationOracleRegistration != nil,
		event.NormalizationOracleRevocation != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func (state *projectionState) applyPromotion(envelope local.Envelope) error {
	if envelope.Schema != promotionEventSchemaVersion {
		return fmt.Errorf("unsupported promotion event schema %q", envelope.Schema)
	}
	var event promotionEvent
	if err := decodeEventStrict(envelope.Payload, &event); err != nil {
		return fmt.Errorf("decode promotion event: %w", err)
	}
	if event.SchemaVersion != promotionEventSchemaVersion {
		return fmt.Errorf("payload schema does not match envelope schema")
	}
	if err := validateEventAudit(
		envelope, event.Actor, event.Roles, event.Audit, event.OccurredAt,
	); err != nil {
		return err
	}
	switch event.Type {
	case promotionRegistered:
		if event.Variant == nil || event.GateResult != nil || event.RollbackVariantID != "" {
			return fmt.Errorf("registered event must contain only variant")
		}
		if err := event.Variant.Validate(); err != nil {
			return err
		}
		if !event.Variant.CreatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("variant created_at does not match event time")
		}
		if !hasRole(event.Roles, RolePromotionOperator) {
			return fmt.Errorf("promotion_operator role is required")
		}
		if _, exists := state.promotions[event.Variant.VariantID]; exists {
			return fmt.Errorf("duplicate promotion variant")
		}
		for _, record := range state.promotions {
			if record.Variant.Component == event.Variant.Component &&
				record.Variant.Revision == event.Variant.Revision {
				return fmt.Errorf("duplicate component revision promotion")
			}
		}
		first := promotionGateOrder[0]
		state.promotions[event.Variant.VariantID] = PromotionRecord{
			Variant:   cloneValue(*event.Variant),
			Status:    PromotionRegistered,
			NextGate:  &first,
			Gates:     []GateEntry{},
			UpdatedAt: event.OccurredAt,
			UpdatedBy: event.Actor,
		}
	case promotionGateRecorded:
		if event.GateResult == nil || event.Variant != nil || event.RollbackVariantID != "" {
			return fmt.Errorf("gate_recorded event must contain only gate_result")
		}
		if err := state.validateGateEvent(event); err != nil {
			return err
		}
		result := *event.GateResult
		record := state.promotions[result.VariantID]
		record.Gates = append(record.Gates, GateEntry{
			Sequence: envelope.Sequence,
			EventID:  envelope.ID,
			Result:   cloneValue(result),
			Actor:    event.Actor,
			At:       event.OccurredAt,
		})
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		switch result.Outcome {
		case GateFail:
			record.Status = PromotionFailed
			record.NextGate = nil
		case GateInconclusive:
			record.Status = PromotionInconclusive
			record.NextGate = nil
		case GatePass:
			nextIndex := len(record.Gates)
			if nextIndex == len(promotionGateOrder) {
				record.Status = PromotionActive
				record.NextGate = nil
				state.active[record.Variant.Component] = record.Variant.Revision
			} else {
				record.Status = PromotionInProgress
				next := promotionGateOrder[nextIndex]
				record.NextGate = &next
			}
		}
		state.promotions[result.VariantID] = record
	case promotionRolledBack:
		if event.RollbackVariantID == "" || event.Variant != nil || event.GateResult != nil {
			return fmt.Errorf("rolled_back event must contain only rollback_variant_id")
		}
		if err := state.validateRollbackEvent(event); err != nil {
			return err
		}
		record := state.promotions[event.RollbackVariantID]
		record.Status = PromotionRolledBack
		record.UpdatedAt = event.OccurredAt
		record.UpdatedBy = event.Actor
		state.promotions[event.RollbackVariantID] = record
		state.active[record.Variant.Component] = record.Variant.RollbackRevision
	default:
		return fmt.Errorf("unsupported promotion event type %q", event.Type)
	}
	return nil
}

func (state *projectionState) validateGateEvent(event promotionEvent) error {
	if event.GateResult == nil {
		return fmt.Errorf("gate result is required")
	}
	result := *event.GateResult
	if err := result.Validate(); err != nil {
		return err
	}
	record, exists := state.promotions[result.VariantID]
	if !exists {
		return fmt.Errorf("%w: promotion variant %q", ErrNotFound, result.VariantID)
	}
	if record.NextGate == nil ||
		record.Status == PromotionFailed ||
		record.Status == PromotionInconclusive ||
		record.Status == PromotionActive ||
		record.Status == PromotionRolledBack {
		return fmt.Errorf("%w: promotion status %q cannot accept another gate",
			ErrInvalidTransition, record.Status)
	}
	if *record.NextGate != result.Gate {
		return fmt.Errorf("%w: next gate is %q, got %q",
			ErrInvalidTransition, *record.NextGate, result.Gate)
	}
	if result.Gate == GateAuthorization && result.Outcome == GatePass {
		if !hasRole(event.Roles, RolePromotionApprover) {
			return fmt.Errorf("%w: promotion_approver role is required",
				ErrUnauthorized)
		}
		if result.Evidence.Authorization == nil ||
			result.Evidence.Authorization.AuthorizedBy != event.Actor {
			return fmt.Errorf("%w: authorization actor must match mutation actor",
				ErrUnauthorized)
		}
	} else if !hasRole(event.Roles, RolePromotionOperator) {
		return fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	if result.Gate == GateFixedHoldout && result.Outcome == GatePass {
		if !hasRole(event.Roles, RoleHoldoutRunner) &&
			!hasRole(event.Roles, RoleHoldoutMaintainer) {
			return fmt.Errorf("%w: holdout role is required", ErrUnauthorized)
		}
		if result.Evidence.NormalizationQualityRunID != "" {
			if record.Variant.ManagedBinding == nil ||
				record.Variant.ManagedBinding.Kind != PromotionManagementNormalizationPolicy {
				return fmt.Errorf("%w: normalization quality holdout evidence requires a managed normalization variant", ErrUnauthorized)
			}
		} else {
			if err := state.assertHoldoutExposureComplete(
				result.Evidence.EvaluationRunID,
				result.Evidence.HoldoutCaseIDs,
				Access{Actor: event.Actor, Roles: event.Roles},
				event.OccurredAt,
			); err != nil {
				return err
			}
		}
	}
	if result.Evidence.NormalizationQualityRunID != "" &&
		(record.Variant.ManagedBinding == nil || record.Variant.ManagedBinding.Kind != PromotionManagementNormalizationPolicy) {
		return fmt.Errorf("%w: normalization quality evidence requires a managed normalization variant", ErrUnauthorized)
	}
	if result.Gate == GateRollbackMonitor && result.Outcome == GatePass {
		active, exists := state.active[record.Variant.Component]
		if exists && active != record.Variant.RollbackRevision {
			return fmt.Errorf("%w: active revision %q does not match rollback revision %q",
				ErrInvalidTransition, active, record.Variant.RollbackRevision)
		}
	}
	return nil
}

func (state *projectionState) validateRollbackEvent(event promotionEvent) error {
	if !hasRole(event.Roles, RolePromotionOperator) {
		return fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	record, exists := state.promotions[event.RollbackVariantID]
	if !exists {
		return fmt.Errorf("%w: promotion variant %q",
			ErrNotFound, event.RollbackVariantID)
	}
	if record.Status != PromotionActive {
		return fmt.Errorf("%w: rollback requires active status, got %q",
			ErrInvalidTransition, record.Status)
	}
	if active, exists := state.active[record.Variant.Component]; !exists ||
		active != record.Variant.Revision {
		return fmt.Errorf("%w: promotion variant is not the active revision",
			ErrInvalidTransition)
	}
	return nil
}

func (state *projectionState) assertHoldoutExposureComplete(
	evaluationRunID string,
	holdoutCaseIDs []string,
	access Access,
	before time.Time,
) error {
	if !hasRole(access.Roles, RoleHoldoutRunner) &&
		!hasRole(access.Roles, RoleHoldoutMaintainer) {
		return fmt.Errorf("%w: holdout role is required", ErrUnauthorized)
	}
	for _, caseID := range holdoutCaseIDs {
		record, exists := state.cases[caseID]
		if !exists {
			return fmt.Errorf("%w: holdout case %q", ErrNotFound, caseID)
		}
		if record.CurrentGovernance.DatasetState != DatasetActive ||
			record.CurrentGovernance.Split != SplitHoldout ||
			!record.CurrentGovernance.Eligibility.Promotion {
			return fmt.Errorf("%w: case %q is not an active promotion holdout",
				ErrInvalidTransition, caseID)
		}
		entry, exists := state.exposures[exposureKey{
			RunID: evaluationRunID, CaseID: caseID,
		}]
		if !exists {
			return fmt.Errorf("%w: holdout case %q has no exposure record for run %q",
				ErrContaminated, caseID, evaluationRunID)
		}
		if !before.IsZero() &&
			(entry.At.After(before) || entry.Exposure.ObservedAt.After(before)) {
			return fmt.Errorf("%w: holdout exposure for case %q was recorded after gate evidence",
				ErrContaminated, caseID)
		}
		for _, observation := range entry.Exposure.Observations {
			if observation.Status != ExposureNotSeen {
				return fmt.Errorf("%w: holdout case %q was seen by %s revision %q",
					ErrContaminated, caseID, observation.Component, observation.Revision)
			}
		}
	}
	return nil
}

func (state *projectionState) validateCaseAddition(evaluationCase EvaluationCase) error {
	cases := make([]EvaluationCase, 0, len(state.cases)+1)
	for _, record := range state.cases {
		cases = append(cases, record.CurrentCase())
	}
	cases = append(cases, evaluationCase)
	return ValidateDatasetContamination(cases)
}

func (state *projectionState) validateAnnotation(
	record CaseRecord,
	annotation CaseAnnotation,
	actor string,
	roles []Role,
) error {
	if !hasRole(roles, RoleDatasetReviewer) {
		return fmt.Errorf("%w: dataset_reviewer role is required", ErrUnauthorized)
	}
	if record.CurrentGovernance.DatasetState != DatasetCandidatePool ||
		record.CurrentGovernance.ReviewState != ReviewInReview {
		return fmt.Errorf("%w: annotations require an explicitly assigned candidate-pool review round", ErrInvalidTransition)
	}
	if annotation.ExpectedGovernanceRevision != record.CurrentGovernance.Revision ||
		annotation.ExpectedLabelRevision != record.CurrentLabelRevision {
		return fmt.Errorf("%w: case revisions are governance=%d label=%d, expected governance=%d label=%d",
			ErrInvalidTransition, record.CurrentGovernance.Revision, record.CurrentLabelRevision,
			annotation.ExpectedGovernanceRevision, annotation.ExpectedLabelRevision)
	}
	if annotation.ReviewedAt.Before(record.Case.CreatedAt) {
		return fmt.Errorf("%w: annotation predates case creation", ErrInvalidTransition)
	}
	assignment, exists := state.currentReviewAssignment(annotation.CaseID, record.CurrentGovernance.Revision)
	if !exists || assignment.Assignment.ExpectedLabelRevision != annotation.ExpectedLabelRevision ||
		!slices.Contains(assignment.Assignment.ReviewerIDs, actor) {
		return fmt.Errorf("%w: reviewer %q is not assigned to this blind review round", ErrUnauthorized, actor)
	}
	if annotation.ProposedLabel != nil {
		if err := validateLabelForCase(record.Case.Type, *annotation.ProposedLabel); err != nil {
			return err
		}
		if err := validateGovernanceEvidenceRefs(record, annotation.ProposedLabel.AnchorRefs); err != nil {
			return fmt.Errorf("%w: proposed label: %v", ErrInvalidTransition, err)
		}
	}
	if err := validateGovernanceEvidenceRefs(record, annotation.EvidenceRefs); err != nil {
		return fmt.Errorf("%w: annotation: %v", ErrInvalidTransition, err)
	}
	for _, existing := range state.annotations[annotation.CaseID] {
		if existing.Reviewer == actor &&
			existing.Annotation.ExpectedGovernanceRevision == annotation.ExpectedGovernanceRevision {
			return fmt.Errorf("%w: reviewer %q already annotated case %q at this governance revision",
				ErrConflict, actor, annotation.CaseID)
		}
	}
	return nil
}

func (state *projectionState) validateReviewAssignment(
	record CaseRecord,
	assignment CaseReviewAssignment,
	actor string,
	roles []Role,
) error {
	if !hasRole(roles, RoleDatasetCurator) {
		return fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	if record.CurrentGovernance.DatasetState != DatasetCandidatePool ||
		record.CurrentGovernance.ReviewState != ReviewPending {
		return fmt.Errorf("%w: assignment requires a pending candidate-pool case", ErrInvalidTransition)
	}
	if assignment.ExpectedGovernanceRevision != record.CurrentGovernance.Revision ||
		assignment.ExpectedLabelRevision != record.CurrentLabelRevision {
		return fmt.Errorf("%w: case revisions are governance=%d label=%d, expected governance=%d label=%d",
			ErrInvalidTransition, record.CurrentGovernance.Revision, record.CurrentLabelRevision,
			assignment.ExpectedGovernanceRevision, assignment.ExpectedLabelRevision)
	}
	if assignment.AssignedAt.Before(record.CurrentGovernance.UpdatedAt) {
		return fmt.Errorf("%w: assignment predates current governance state", ErrInvalidTransition)
	}
	if slices.Contains(assignment.ReviewerIDs, actor) {
		return fmt.Errorf("%w: assignment curator cannot assign themselves as reviewer", ErrUnauthorized)
	}
	return nil
}

func (state *projectionState) validateAdjudication(
	record CaseRecord,
	adjudication CaseAdjudication,
	actor string,
	roles []Role,
) error {
	if !hasRole(roles, RoleDatasetAdjudicator) {
		return fmt.Errorf("%w: dataset_adjudicator role is required", ErrUnauthorized)
	}
	if record.CurrentGovernance.DatasetState != DatasetCandidatePool ||
		record.CurrentGovernance.ReviewState != ReviewInReview {
		return fmt.Errorf("%w: adjudication requires an explicitly assigned candidate-pool review round", ErrInvalidTransition)
	}
	if adjudication.ExpectedGovernanceRevision != record.CurrentGovernance.Revision ||
		adjudication.ExpectedLabelRevision != record.CurrentLabelRevision {
		return fmt.Errorf("%w: case revisions are governance=%d label=%d, expected governance=%d label=%d",
			ErrInvalidTransition, record.CurrentGovernance.Revision, record.CurrentLabelRevision,
			adjudication.ExpectedGovernanceRevision, adjudication.ExpectedLabelRevision)
	}
	reviewers := make(map[string]struct{}, len(adjudication.AnnotationEventIDs))
	selectedWasReviewed := false
	hasReject := false
	for _, eventID := range adjudication.AnnotationEventIDs {
		entry, exists := state.annotationEvents[eventID]
		if !exists {
			return fmt.Errorf("%w: annotation event %q", ErrNotFound, eventID)
		}
		annotation := entry.Annotation
		if annotation.CaseID != adjudication.CaseID ||
			annotation.ExpectedGovernanceRevision != adjudication.ExpectedGovernanceRevision ||
			annotation.ExpectedLabelRevision != adjudication.ExpectedLabelRevision {
			return fmt.Errorf("%w: annotation event %q is not bound to the adjudicated case revision",
				ErrInvalidTransition, eventID)
		}
		if entry.Reviewer == actor {
			return fmt.Errorf("%w: adjudicator must be independent from every reviewer", ErrUnauthorized)
		}
		if entry.At.After(adjudication.AdjudicatedAt) {
			return fmt.Errorf("%w: annotation event %q occurs after adjudication",
				ErrInvalidTransition, eventID)
		}
		if _, duplicate := reviewers[entry.Reviewer]; duplicate {
			return fmt.Errorf("%w: annotations are not from distinct reviewers", ErrInvalidTransition)
		}
		reviewers[entry.Reviewer] = struct{}{}
		if annotation.Verdict == AnnotationReject {
			hasReject = true
		}
		if adjudication.Outcome == AdjudicationApprove && annotation.Verdict == AnnotationApprove &&
			reflect.DeepEqual(annotation.ProposedLabel, adjudication.SelectedLabel) &&
			annotation.LabelPolicyRevision == adjudication.LabelPolicyRevision {
			selectedWasReviewed = true
		}
	}
	if err := validateGovernanceEvidenceRefs(record, adjudication.EvidenceRefs); err != nil {
		return fmt.Errorf("%w: adjudication: %v", ErrInvalidTransition, err)
	}
	if len(reviewers) < 2 {
		return fmt.Errorf("%w: adjudication requires two distinct reviewers", ErrInvalidTransition)
	}
	assignment, exists := state.currentReviewAssignment(adjudication.CaseID, record.CurrentGovernance.Revision)
	if !exists || assignment.Assignment.ExpectedLabelRevision != adjudication.ExpectedLabelRevision ||
		len(reviewers) != len(assignment.Assignment.ReviewerIDs) {
		return fmt.Errorf("%w: adjudication must include every assigned reviewer", ErrInvalidTransition)
	}
	for _, reviewer := range assignment.Assignment.ReviewerIDs {
		if _, exists := reviewers[reviewer]; !exists {
			return fmt.Errorf("%w: adjudication omits assigned reviewer %q", ErrInvalidTransition, reviewer)
		}
	}
	if adjudication.Outcome == AdjudicationApprove {
		if !selectedWasReviewed {
			return fmt.Errorf("%w: selected label was not proposed by a referenced reviewer",
				ErrInvalidTransition)
		}
		if err := validateLabelForCase(record.Case.Type, *adjudication.SelectedLabel); err != nil {
			return err
		}
	} else if !hasReject {
		return fmt.Errorf("%w: rejection requires at least one referenced reject annotation",
			ErrInvalidTransition)
	}
	return nil
}

func (state *projectionState) validateReopen(record CaseRecord, reopen CaseReopen, roles []Role) error {
	if !hasRole(roles, RoleDatasetCurator) {
		return fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	if record.CurrentGovernance.DatasetState != DatasetGold &&
		record.CurrentGovernance.DatasetState != DatasetActive &&
		record.CurrentGovernance.DatasetState != DatasetRetired {
		return fmt.Errorf("%w: only adjudicated gold, active, or retired cases can be reopened", ErrInvalidTransition)
	}
	if record.CurrentGovernance.Split == SplitHoldout && !hasRole(roles, RoleHoldoutMaintainer) {
		return fmt.Errorf("%w: holdout_maintainer role is required to reopen holdout", ErrUnauthorized)
	}
	if reopen.ExpectedGovernanceRevision != record.CurrentGovernance.Revision {
		return fmt.Errorf("%w: governance revision is %d, expected %d", ErrInvalidTransition,
			record.CurrentGovernance.Revision, reopen.ExpectedGovernanceRevision)
	}
	if reopen.ReopenedAt.Before(record.CurrentGovernance.UpdatedAt) {
		return fmt.Errorf("%w: reopen predates current governance state", ErrInvalidTransition)
	}
	if err := validateGovernanceEvidenceRefs(record, reopen.EvidenceRefs); err != nil {
		return fmt.Errorf("%w: reopen: %v", ErrInvalidTransition, err)
	}
	affected := state.affectedExperiments(reopen.CaseID)
	if !slices.Equal(affected, reopen.AffectedExperimentIDs) {
		return fmt.Errorf("%w: affected_experiment_ids are %v, want %v", ErrInvalidTransition,
			reopen.AffectedExperimentIDs, affected)
	}
	return nil
}

func (state *projectionState) currentReviewAssignment(caseID string, governanceRevision uint64) (CaseReviewAssignmentEntry, bool) {
	entries := state.assignments[caseID]
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Assignment.ExpectedGovernanceRevision+1 == governanceRevision {
			return entry, true
		}
	}
	return CaseReviewAssignmentEntry{}, false
}

func (state *projectionState) reviewAgreement(caseID string) CaseReviewAgreement {
	record := state.cases[caseID]
	result := CaseReviewAgreement{
		CaseID: caseID, GovernanceRevision: record.CurrentGovernance.Revision,
		LabelRevision:       record.CurrentLabelRevision,
		VerdictAgreement:    ReviewAgreementUnavailable,
		ExactLabelAgreement: ReviewAgreementUnavailable,
	}
	assignments := state.assignments[caseID]
	if len(assignments) == 0 {
		return result
	}
	assignment := assignments[len(assignments)-1]
	reviewRevision := assignment.Assignment.ExpectedGovernanceRevision + 1
	result.GovernanceRevision = reviewRevision
	result.LabelRevision = assignment.Assignment.ExpectedLabelRevision
	result.AssignedReviewers = len(assignment.Assignment.ReviewerIDs)
	var firstLabel *Label
	labelsAgree := true
	for _, entry := range state.annotations[caseID] {
		if entry.Annotation.ExpectedGovernanceRevision != reviewRevision ||
			!slices.Contains(assignment.Assignment.ReviewerIDs, entry.Reviewer) {
			continue
		}
		result.CompletedReviews++
		if entry.Annotation.Verdict == AnnotationApprove {
			result.ApproveCount++
			if firstLabel == nil {
				firstLabel = clonePointer(*entry.Annotation.ProposedLabel)
			} else if !reflect.DeepEqual(*firstLabel, *entry.Annotation.ProposedLabel) {
				labelsAgree = false
			}
		} else {
			result.RejectCount++
		}
	}
	if result.CompletedReviews < result.AssignedReviewers {
		result.VerdictAgreement = ReviewAgreementIncomplete
		result.ExactLabelAgreement = ReviewAgreementIncomplete
		return result
	}
	if result.ApproveCount == result.AssignedReviewers || result.RejectCount == result.AssignedReviewers {
		result.VerdictAgreement = ReviewAgreementUnanimous
	} else {
		result.VerdictAgreement = ReviewAgreementDisagreement
	}
	if result.ApproveCount == 0 {
		result.ExactLabelAgreement = ReviewAgreementUnavailable
	} else if labelsAgree && result.ApproveCount == result.AssignedReviewers {
		result.ExactLabelAgreement = ReviewAgreementUnanimous
	} else {
		result.ExactLabelAgreement = ReviewAgreementDisagreement
	}
	return result
}

func (state *projectionState) validateActivation(
	record CaseRecord,
	activation CaseActivation,
	roles []Role,
) error {
	if record.CurrentGovernance.DatasetState != DatasetGold ||
		record.CurrentGovernance.ReviewState != ReviewApproved {
		return fmt.Errorf("%w: activation requires an approved gold case", ErrInvalidTransition)
	}
	if activation.ExpectedGovernanceRevision != record.CurrentGovernance.Revision {
		return fmt.Errorf("%w: governance revision is %d, expected %d", ErrInvalidTransition,
			record.CurrentGovernance.Revision, activation.ExpectedGovernanceRevision)
	}
	if activation.ActivatedAt.Before(record.CurrentGovernance.UpdatedAt) {
		return fmt.Errorf("%w: activation predates current governance state", ErrInvalidTransition)
	}
	if activation.Split == SplitHoldout {
		if !hasRole(roles, RoleHoldoutMaintainer) {
			return fmt.Errorf("%w: holdout_maintainer role is required", ErrUnauthorized)
		}
	} else if !hasRole(roles, RoleDatasetCurator) {
		return fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	current := record.CurrentCase()
	current.DatasetState = DatasetActive
	current.Split = activation.Split
	current.Eligibility = activation.Eligibility
	current.LicenseConsent = cloneValue(activation.LicenseConsent)
	if err := current.validateStateAndEligibility(); err != nil {
		return err
	}
	if (current.Provenance.Kind == SourceMutation || current.Provenance.Kind == SourceSynthetic) &&
		current.Eligibility.ProductionDistribution {
		return fmt.Errorf("%s case cannot represent production distribution", current.Provenance.Kind)
	}
	cases := make([]EvaluationCase, 0, len(state.cases))
	for caseID, candidate := range state.cases {
		if caseID == activation.CaseID {
			cases = append(cases, current)
		} else {
			cases = append(cases, candidate.CurrentCase())
		}
	}
	return ValidateDatasetContamination(cases)
}

func validateGovernanceEvidenceRefs(record CaseRecord, refs []string) error {
	allowed := make(map[string]struct{}, len(record.Case.Provenance.EvidenceRefs)+len(record.CurrentLabel.AnchorRefs)+1)
	allowed[record.Case.InputSnapshotRef] = struct{}{}
	for _, ref := range record.Case.Provenance.EvidenceRefs {
		allowed[ref] = struct{}{}
	}
	for _, ref := range record.CurrentLabel.AnchorRefs {
		allowed[ref] = struct{}{}
	}
	for _, ref := range refs {
		if _, exists := allowed[ref]; !exists {
			return fmt.Errorf("evidence ref %q is not frozen in the candidate provenance", ref)
		}
	}
	return nil
}

func (state *projectionState) affectedExperiments(caseID string) []string {
	seen := make(map[string]struct{})
	for key := range state.exposures {
		if key.CaseID == caseID {
			seen[key.RunID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func (state *projectionState) caseRecord(caseID string) (CaseRecord, error) {
	record, exists := state.cases[caseID]
	if !exists {
		return CaseRecord{}, fmt.Errorf("%w: case %q", ErrNotFound, caseID)
	}
	return cloneValue(record), nil
}

func (state *projectionState) promotionRecord(
	variantID string,
) (PromotionRecord, error) {
	record, exists := state.promotions[variantID]
	if !exists {
		return PromotionRecord{}, fmt.Errorf("%w: promotion variant %q",
			ErrNotFound, variantID)
	}
	return cloneValue(record), nil
}

func (state *projectionState) evaluationRun(runID string) (EvaluationRun, error) {
	run, exists := state.evaluationRuns[runID]
	if !exists {
		return EvaluationRun{}, fmt.Errorf("%w: evaluation run %q", ErrNotFound, runID)
	}
	return cloneValue(run), nil
}

func (state *projectionState) experimentRun(id string) (ExperimentRun, error) {
	run, exists := state.experimentRuns[id]
	if !exists {
		return ExperimentRun{}, fmt.Errorf("%w: experiment run %q", ErrNotFound, id)
	}
	return cloneValue(run), nil
}

func (state *projectionState) repeatabilityRun(id string) (RepeatabilityRun, error) {
	run, exists := state.repeatabilityRuns[id]
	if !exists {
		return RepeatabilityRun{}, fmt.Errorf("%w: repeatability run %q", ErrNotFound, id)
	}
	return cloneValue(run), nil
}

func (state *projectionState) repeatabilityBatch(id string) (RepeatabilityBatchRecord, error) {
	record, exists := state.repeatabilityBatches[id]
	if !exists {
		return RepeatabilityBatchRecord{}, fmt.Errorf("%w: repeatability batch %q", ErrNotFound, id)
	}
	return cloneValue(record), nil
}

func (state *projectionState) experimentBatch(id string) (ExperimentBatchRecord, error) {
	record, exists := state.experimentBatches[id]
	if !exists {
		return ExperimentBatchRecord{}, fmt.Errorf("%w: experiment batch %q", ErrNotFound, id)
	}
	return cloneValue(record), nil
}

func (state *projectionState) promotionRecordForRoles(
	variantID string,
	roles []Role,
) (PromotionRecord, error) {
	record, err := state.promotionRecord(variantID)
	if err != nil {
		return PromotionRecord{}, err
	}
	if hasRole(roles, RoleHoldoutMaintainer) ||
		hasRole(roles, RoleHoldoutRunner) {
		return record, nil
	}
	for index := range record.Gates {
		if record.Gates[index].Result.Gate != GateFixedHoldout {
			continue
		}
		record.Gates[index].Result.Evidence = GateEvidence{}
		record.Gates[index].Result.Summary = ""
		record.Gates[index].EvidenceRedacted = true
	}
	return record, nil
}

func (repository *Repository) appendDataset(
	mutation Mutation,
	event datasetEvent,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONL(datasetStream, local.Event{
		ID:      mutation.IdempotencyKey,
		Schema:  datasetEventSchemaVersion,
		Time:    mutation.At.UTC(),
		Payload: event,
	})
	if err != nil {
		return local.Envelope{}, fmt.Errorf("append evaluation dataset event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) appendDatasetAtSequence(
	mutation Mutation,
	event datasetEvent,
	expectedSequence uint64,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONLAtSequence(datasetStream, expectedSequence, local.Event{
		ID: mutation.IdempotencyKey, Schema: datasetEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return local.Envelope{}, fmt.Errorf("%w: evaluation dataset stream changed concurrently", ErrConflict)
		}
		return local.Envelope{}, fmt.Errorf("append evaluation dataset event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) appendPromotion(
	mutation Mutation,
	event promotionEvent,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONL(promotionStream, local.Event{
		ID:      mutation.IdempotencyKey,
		Schema:  promotionEventSchemaVersion,
		Time:    mutation.At.UTC(),
		Payload: event,
	})
	if err != nil {
		return local.Envelope{}, fmt.Errorf("append promotion event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) appendEvaluationRun(
	mutation Mutation,
	event evaluationRunEvent,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONL(evaluationRunStream, local.Event{
		ID: mutation.IdempotencyKey, Schema: evaluationRunEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if err != nil {
		return local.Envelope{}, fmt.Errorf("append evaluation run event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) appendExperimentRun(
	mutation Mutation,
	event experimentRunEvent,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONL(experimentRunStream, local.Event{
		ID: mutation.IdempotencyKey, Schema: experimentRunEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if err != nil {
		return local.Envelope{}, fmt.Errorf("append experiment event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) appendRepeatabilityRun(
	mutation Mutation,
	event repeatabilityRunEvent,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONL(repeatabilityRunStream, local.Event{
		ID: mutation.IdempotencyKey, Schema: repeatabilityRunEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if err != nil {
		return local.Envelope{}, fmt.Errorf("append repeatability event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) appendRepeatabilityBatch(
	mutation Mutation,
	event repeatabilityBatchEvent,
	expectedSequence uint64,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONLAtSequence(
		repeatabilityBatchStream, expectedSequence, local.Event{
			ID: mutation.IdempotencyKey, Schema: repeatabilityBatchEventSchemaVersion,
			Time: mutation.At.UTC(), Payload: event,
		},
	)
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return local.Envelope{}, fmt.Errorf("%w: repeatability batch stream changed concurrently", ErrConflict)
		}
		return local.Envelope{}, fmt.Errorf("append repeatability batch event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) appendExperimentBatch(
	mutation Mutation,
	event experimentBatchEvent,
	expectedSequence uint64,
) (local.Envelope, error) {
	envelope, err := repository.store.AppendJSONLAtSequence(experimentBatchStream, expectedSequence, local.Event{
		ID: mutation.IdempotencyKey, Schema: experimentBatchEventSchemaVersion,
		Time: mutation.At.UTC(), Payload: event,
	})
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return local.Envelope{}, fmt.Errorf("%w: experiment batch stream changed concurrently", ErrConflict)
		}
		return local.Envelope{}, fmt.Errorf("append experiment batch event: %w", err)
	}
	return envelope, nil
}

func (repository *Repository) assertIdempotent(
	existing storedEvent,
	stream string,
	schema string,
	event any,
	mutation Mutation,
) error {
	if existing.stream != stream {
		return fmt.Errorf("%w: idempotency key %q is already used in %s",
			ErrConflict, mutation.IdempotencyKey, existing.stream)
	}
	_, err := repository.store.AppendJSONL(stream, local.Event{
		ID:      mutation.IdempotencyKey,
		Schema:  schema,
		Time:    mutation.At.UTC(),
		Payload: event,
	})
	if err != nil {
		return fmt.Errorf("%w: idempotency key %q: %v",
			ErrConflict, mutation.IdempotencyKey, err)
	}
	return nil
}

func validateEventAudit(
	envelope local.Envelope,
	actor string,
	roles []Role,
	audit string,
	occurredAt time.Time,
) error {
	mutation := Mutation{
		IdempotencyKey: envelope.ID,
		Actor:          actor,
		Roles:          roles,
		Audit:          audit,
		At:             occurredAt,
	}
	if err := mutation.Validate(); err != nil {
		return err
	}
	if !occurredAt.Equal(envelope.Time.UTC()) {
		return fmt.Errorf("payload time does not match envelope time")
	}
	return nil
}

func authorizeCaseCreate(evaluationCase EvaluationCase, roles []Role) error {
	if evaluationCase.Split == SplitHoldout {
		if !hasRole(roles, RoleHoldoutMaintainer) {
			return fmt.Errorf("%w: holdout_maintainer role is required",
				ErrUnauthorized)
		}
		return nil
	}
	if evaluationCase.Provenance.Kind == SourceProductionFeedback {
		if !hasRole(roles, RoleFeedbackIngest) &&
			!hasRole(roles, RoleDatasetCurator) {
			return fmt.Errorf("%w: feedback_ingest or dataset_curator role is required",
				ErrUnauthorized)
		}
		return nil
	}
	if !hasRole(roles, RoleDatasetCurator) {
		return fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	return nil
}

func authorizeGovernedCaseImport(evaluationCase EvaluationCase, roles []Role) error {
	if evaluationCase.Split == SplitHoldout {
		if !hasRole(roles, RoleHoldoutMaintainer) {
			return fmt.Errorf("%w: holdout_maintainer role is required", ErrUnauthorized)
		}
		return nil
	}
	if !hasRole(roles, RoleDatasetCurator) {
		return fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	return nil
}

func authorizeCaseWrite(evaluationCase EvaluationCase, roles []Role) error {
	if evaluationCase.Split == SplitHoldout {
		if !hasRole(roles, RoleHoldoutMaintainer) {
			return fmt.Errorf("%w: holdout_maintainer role is required",
				ErrUnauthorized)
		}
		return nil
	}
	if !hasRole(roles, RoleDatasetCurator) {
		return fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	return nil
}

func authorizeCaseRead(evaluationCase EvaluationCase, roles []Role) error {
	if evaluationCase.Split == SplitHoldout &&
		!hasRole(roles, RoleHoldoutMaintainer) &&
		!hasRole(roles, RoleHoldoutRunner) {
		return fmt.Errorf("%w: holdout role is required", ErrUnauthorized)
	}
	return nil
}

func authorizeExposure(evaluationCase EvaluationCase, roles []Role) error {
	if evaluationCase.Split == SplitHoldout {
		if !hasRole(roles, RoleHoldoutRunner) &&
			!hasRole(roles, RoleHoldoutMaintainer) {
			return fmt.Errorf("%w: holdout role is required", ErrUnauthorized)
		}
		return nil
	}
	if !hasRole(roles, RoleDatasetCurator) &&
		!hasRole(roles, RoleHoldoutRunner) {
		return fmt.Errorf("%w: evaluation runner role is required", ErrUnauthorized)
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}

func decodeEventStrict(data []byte, out any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("multiple JSON values are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func clonePointer[T any](value T) *T {
	cloned := cloneValue(value)
	return &cloned
}

func cloneValue[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var cloned T
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}

func corruptf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, arguments...))
}
