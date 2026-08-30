package calibration

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

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type CaseSource interface {
	GetCase(string, evaluation.Access) (evaluation.CaseRecord, error)
	AnnotationHistory(string, evaluation.Access) ([]evaluation.CaseAnnotationEntry, error)
	AdjudicationHistory(string, evaluation.Access) ([]evaluation.CaseAdjudicationEntry, error)
}

type RunSource interface {
	LoadRun(string) (runmodel.ReviewRun, error)
	LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error)
	LoadCommittedRunResult(string) (runrepo.CommittedRunResult, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
	CheckArtifactEligibility(runmodel.ArtifactRef, runrepo.ArtifactUse) error
}

type Repository struct {
	store *local.Store
	cases CaseSource
	runs  RunSource
	mu    sync.Mutex
}

func New(store *local.Store, cases CaseSource, runs RunSource) (*Repository, error) {
	if store == nil || cases == nil || runs == nil {
		return nil, fmt.Errorf("calibration store, case source, and run source are required")
	}
	repository := &Repository{store: store, cases: cases, runs: runs}
	if _, err := repository.loadEvents(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return repository, nil
}

func (repository *Repository) Fit(ctx context.Context, request FitRequest, mutation evaluation.Mutation) (Run, error) {
	if ctx == nil {
		return Run{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	if err := request.Validate(); err != nil {
		return Run{}, fmt.Errorf("validate fit request: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return Run{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return Run{}, fmt.Errorf("request created_at must equal mutation time")
	}
	if !slices.Contains(mutation.Roles, evaluation.RoleDatasetCurator) {
		return Run{}, fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	for _, observation := range append(slices.Clone(request.Training), request.Validation...) {
		if slices.Contains(observation.Authority.ReviewerIDs, mutation.Actor) || observation.Authority.AdjudicatorID == mutation.Actor {
			return Run{}, fmt.Errorf("%w: fitting operator must be independent from label authority", ErrUnauthorized)
		}
	}
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	training, err := repository.materialize(ctx, request.Training, "training", access)
	if err != nil {
		return Run{}, err
	}
	validation, err := repository.materialize(ctx, request.Validation, "validation", access)
	if err != nil {
		return Run{}, err
	}
	if err := validatePartitionIsolation(training, validation); err != nil {
		return Run{}, err
	}
	if !hasBothTruths(training) || !hasBothTruths(validation) {
		return Run{}, fmt.Errorf("training and validation must each contain valid-defect and false-positive truth")
	}
	profile, err := fitProfile(request.ProfileID, request.ProfileRevision, training)
	if err != nil {
		return Run{}, err
	}
	report, err := evaluate(profile, training, validation, request.GatePolicy)
	if err != nil {
		return Run{}, err
	}
	trainingManifest, err := sealManifest(training)
	if err != nil {
		return Run{}, err
	}
	validationManifest, err := sealManifest(validation)
	if err != nil {
		return Run{}, err
	}
	trainingRef, err := repository.putJSONArtifact(trainingManifest, ContractDatasetManifest)
	if err != nil {
		return Run{}, err
	}
	validationRef, err := repository.putJSONArtifact(validationManifest, ContractDatasetManifest)
	if err != nil {
		return Run{}, err
	}
	status := "gate_failed"
	if report.Passed {
		status = "gate_passed"
	}
	candidate, err := sealCandidate(ProfileCandidate{Profile: profile, TrainingManifestRef: trainingRef, ValidationManifestRef: validationRef, Status: status, AutoPublished: false})
	if err != nil {
		return Run{}, err
	}
	run, err := sealRun(Run{RunID: request.RunID, TrainingManifestRef: trainingRef, ValidationManifestRef: validationRef, ProfileCandidate: candidate, Report: report, GatePolicy: request.GatePolicy, CreatedBy: mutation.Actor, CreatedAt: mutation.At.UTC()})
	if err != nil {
		return Run{}, err
	}
	runRef, err := repository.putJSONArtifact(run, ContractRun)
	if err != nil {
		return Run{}, err
	}
	event := runEvent{RunID: run.RunID, RunRef: runRef, Actor: mutation.Actor, Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	events, err := repository.loadEvents()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Run{}, err
	}
	for _, existing := range events {
		if existing.ID == mutation.IdempotencyKey {
			if existing.Schema != eventSchema || !existing.Time.Equal(mutation.At.UTC()) || !jsonEqual(existing.Payload, event) {
				return Run{}, fmt.Errorf("%w: idempotency key reused with different fit", ErrConflict)
			}
			return repository.loadRunRef(event.RunRef)
		}
		var value runEvent
		if err := decodeStrict(existing.Payload, &value); err != nil {
			return Run{}, err
		}
		if value.RunID == request.RunID {
			return Run{}, fmt.Errorf("%w: run_id %q already exists", ErrConflict, request.RunID)
		}
	}
	if _, err := repository.store.AppendJSONL(streamName, local.Event{ID: mutation.IdempotencyKey, Schema: eventSchema, Time: mutation.At.UTC(), Payload: event}); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (repository *Repository) Get(runID string, access evaluation.Access) (Run, error) {
	if err := validateID("run_id", runID); err != nil {
		return Run{}, err
	}
	if err := authorizeRead(access); err != nil {
		return Run{}, err
	}
	events, err := repository.loadEvents()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	for _, envelope := range events {
		var event runEvent
		if err := decodeStrict(envelope.Payload, &event); err != nil {
			return Run{}, err
		}
		if event.RunID == runID {
			return repository.loadRunRef(event.RunRef)
		}
	}
	return Run{}, fmt.Errorf("%w: run %q", ErrNotFound, runID)
}

func (repository *Repository) List(access evaluation.Access) ([]Run, error) {
	if err := authorizeRead(access); err != nil {
		return nil, err
	}
	events, err := repository.loadEvents()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Run{}, nil
		}
		return nil, err
	}
	result := make([]Run, 0, len(events))
	for _, envelope := range events {
		var event runEvent
		if err := decodeStrict(envelope.Payload, &event); err != nil {
			return nil, err
		}
		run, err := repository.loadRunRef(event.RunRef)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].RunID < result[j].RunID
	})
	return result, nil
}

func (repository *Repository) materialize(ctx context.Context, observations []Observation, partition string, access evaluation.Access) ([]ManifestSample, error) {
	result := make([]ManifestSample, 0, len(observations))
	for index, observation := range observations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := repository.cases.GetCase(observation.CaseID, access)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] case: %w", partition, index, err)
		}
		current := record.CurrentCase()
		if record.CurrentGovernance.Revision != observation.CaseGovernanceRevision || record.CurrentLabelRevision != observation.CaseLabelRevision {
			return nil, fmt.Errorf("%s[%d] case revisions drifted: %w", partition, index, ErrContaminated)
		}
		if current.DatasetState != evaluation.DatasetActive || current.ReviewState != evaluation.ReviewApproved {
			return nil, fmt.Errorf("%s[%d] case is not active and approved", partition, index)
		}
		if partition == "training" {
			if current.Split != evaluation.SplitTrain || !current.Eligibility.Training {
				return nil, fmt.Errorf("training case %q is not train/training eligible", current.CaseID)
			}
		} else if (current.Split != evaluation.SplitDev && current.Split != evaluation.SplitTest) || !current.Eligibility.Evaluation {
			return nil, fmt.Errorf("validation case %q must be evaluation-eligible dev/test; holdout is promotion-only", current.CaseID)
		}
		if current.Provenance.RepositoryID != observation.RepositoryID {
			return nil, fmt.Errorf("%s[%d] repository binding mismatch", partition, index)
		}
		if current.Provenance.SourceRunID != "" && current.Provenance.SourceRunID != observation.ReviewRunID {
			return nil, fmt.Errorf("%s[%d] source run binding mismatch", partition, index)
		}
		if err := repository.validateAuthority(record, observation.Authority, access); err != nil {
			return nil, fmt.Errorf("%s[%d] label authority: %w", partition, index, err)
		}
		run, err := repository.runs.LoadRun(observation.ReviewRunID)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] review run: %w", partition, index, err)
		}
		if run.Status != runmodel.RunStatusSucceeded || run.CandidateSetRef == nil {
			return nil, fmt.Errorf("%s[%d] review run has no committed candidate set", partition, index)
		}
		use := runrepo.ArtifactUseEvaluation
		if partition == "training" {
			use = runrepo.ArtifactUseTraining
		}
		if err := repository.runs.CheckArtifactEligibility(*run.CandidateSetRef, use); err != nil {
			return nil, fmt.Errorf("%s[%d] candidate artifact eligibility: %w", partition, index, err)
		}
		snapshot, err := repository.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
		if err != nil {
			return nil, err
		}
		if err := repository.runs.CheckArtifactEligibility(snapshot.ReviewSpecRef, use); err != nil {
			return nil, fmt.Errorf("%s[%d] review spec eligibility: %w", partition, index, err)
		}
		specBytes, err := repository.runs.ReadArtifact(snapshot.ReviewSpecRef)
		if err != nil {
			return nil, err
		}
		spec, err := contractsv1alpha1.DecodeReviewSpec(specBytes)
		if err != nil {
			return nil, err
		}
		if spec.Repository.RepositoryID != observation.RepositoryID || current.InputSnapshotRef != run.TargetSnapshotRef.URI {
			return nil, fmt.Errorf("%s[%d] case/run immutable target binding mismatch", partition, index)
		}
		committed, err := repository.runs.LoadCommittedRunResult(observation.ReviewRunID)
		if err != nil {
			return nil, err
		}
		candidate, ok := findCandidate(committed, observation.CandidateID)
		if !ok {
			return nil, fmt.Errorf("%s[%d] candidate %q not found", partition, index, observation.CandidateID)
		}
		hypothesis := candidate.Hypothesis
		if !hypothesis.RawConfidenceAvailable || hypothesis.RawConfidencePPM != observation.RawConfidencePPM || hypothesis.ClusterFingerprint != observation.ClusterFingerprint || hypothesis.Dimension.ID != observation.DimensionID || hypothesis.Dimension.Revision != observation.DimensionRevision || hypothesis.Dimension.SHA256 != observation.DimensionSHA256 {
			return nil, fmt.Errorf("%s[%d] candidate observation does not match committed raw facts", partition, index)
		}
		if err := validateTruth(current, hypothesis, observation.Truth); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", partition, index, err)
		}
		result = append(result, ManifestSample{Observation: observation, DatasetSplit: string(current.Split), CloneGroupID: current.CloneGroupID})
	}
	return result, nil
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
	annotationByID := map[string]evaluation.CaseAnnotationEntry{}
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

func validateTruth(current evaluation.EvaluationCase, hypothesis contractsv1alpha1.ReviewHypothesis, truth Truth) error {
	label := current.Label
	switch truth {
	case TruthFalsePositive:
		if current.Type == evaluation.CaseNegativeClean && label.ExpectedOutcome == evaluation.OutcomeClean {
			return nil
		}
		if current.Type == evaluation.CaseMutationDiagnostic && label.ExpectedOutcome == evaluation.OutcomeClean {
			return nil
		}
		if current.Type == evaluation.CaseFalsePositiveRegression && label.ExpectedOutcome == evaluation.OutcomeFalsePositive {
			for _, target := range label.SuppressionTargets {
				if target.ClusterFingerprint == hypothesis.ClusterFingerprint {
					return nil
				}
			}
		}
		return fmt.Errorf("false-positive truth is not independently bound to the candidate")
	case TruthValidDefect:
		if label.ExpectedOutcome != evaluation.OutcomeDefectPresent && label.ExpectedOutcome != evaluation.OutcomeMissedDefect {
			return fmt.Errorf("case label does not establish a defect")
		}
		if label.Category != hypothesis.Category {
			return fmt.Errorf("case label category does not match candidate")
		}
		for _, anchor := range label.Anchors {
			if anchor.Path == hypothesis.Anchor.Path && string(hypothesis.Anchor.Side) == anchor.Side && anchor.SourceDigest == hypothesis.Anchor.SourceDigest && anchor.StartLine <= hypothesis.Anchor.EndLine && hypothesis.Anchor.StartLine <= anchor.EndLine {
				return nil
			}
		}
		return fmt.Errorf("defect truth is not localized to the candidate anchor")
	default:
		return fmt.Errorf("unsupported truth")
	}
}

func validatePartitionIsolation(training, validation []ManifestSample) error {
	cases, clones := map[string]string{}, map[string]string{}
	candidates := map[string]string{}
	for _, partition := range []struct {
		name   string
		values []ManifestSample
	}{{"training", training}, {"validation", validation}} {
		for _, sample := range partition.values {
			candidateKey := sample.ReviewRunID + "\x00" + sample.CandidateID
			if prior, ok := candidates[candidateKey]; ok {
				return fmt.Errorf("candidate %q is duplicated in %s and %s: %w", candidateKey, prior, partition.name, ErrContaminated)
			}
			if prior, ok := cases[sample.CaseID]; ok && prior != partition.name {
				return fmt.Errorf("case %q crosses training/validation: %w", sample.CaseID, ErrContaminated)
			}
			if prior, ok := clones[sample.CloneGroupID]; ok && prior != partition.name {
				return fmt.Errorf("clone group %q crosses training/validation: %w", sample.CloneGroupID, ErrContaminated)
			}
			cases[sample.CaseID], clones[sample.CloneGroupID] = partition.name, partition.name
			candidates[candidateKey] = partition.name
		}
	}
	return nil
}

func hasBothTruths(samples []ManifestSample) bool {
	positive, negative := false, false
	for _, sample := range samples {
		if sample.Truth == TruthValidDefect {
			positive = true
		}
		if sample.Truth == TruthFalsePositive {
			negative = true
		}
	}
	return positive && negative
}

func findCandidate(result runrepo.CommittedRunResult, id string) (contractsv1alpha1.GovernedReviewCandidate, bool) {
	if result.CandidateSet == nil {
		return contractsv1alpha1.GovernedReviewCandidate{}, false
	}
	index, found := slices.BinarySearchFunc(result.CandidateSet.Candidates, id, func(candidate contractsv1alpha1.GovernedReviewCandidate, id string) int {
		if candidate.CandidateID < id {
			return -1
		}
		if candidate.CandidateID > id {
			return 1
		}
		return 0
	})
	if !found {
		return contractsv1alpha1.GovernedReviewCandidate{}, false
	}
	return result.CandidateSet.Candidates[index], true
}

func (repository *Repository) putJSONArtifact(value any, contract string) (runmodel.ArtifactRef, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	ref, err := repository.store.PutArtifact(data)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return runmodel.ArtifactRef{URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes, Contract: contract}, nil
}
func (repository *Repository) loadRunRef(ref runmodel.ArtifactRef) (Run, error) {
	if ref.Contract != ContractRun {
		return Run{}, fmt.Errorf("%w: invalid run contract", ErrCorrupt)
	}
	data, err := repository.store.ReadArtifact(local.ArtifactRef{URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes})
	if err != nil {
		return Run{}, err
	}
	var run Run
	if err := decodeStrict(data, &run); err != nil {
		return Run{}, err
	}
	if err := run.Validate(); err != nil {
		return Run{}, err
	}
	for _, manifestRef := range []runmodel.ArtifactRef{run.TrainingManifestRef, run.ValidationManifestRef} {
		data, err := repository.store.ReadArtifact(local.ArtifactRef{URI: manifestRef.URI, SHA256: manifestRef.SHA256, SizeBytes: manifestRef.SizeBytes})
		if err != nil {
			return Run{}, err
		}
		var manifest DatasetManifest
		if err := decodeStrict(data, &manifest); err != nil {
			return Run{}, err
		}
		if err := manifest.Validate(); err != nil {
			return Run{}, err
		}
	}
	return run, nil
}
func (repository *Repository) loadEvents() ([]local.Envelope, error) {
	events, err := repository.store.ReadJSONL(streamName)
	if err != nil {
		return nil, err
	}
	for _, envelope := range events {
		if envelope.Schema != eventSchema {
			return nil, fmt.Errorf("%w: unsupported event schema", ErrCorrupt)
		}
		var event runEvent
		if err := decodeStrict(envelope.Payload, &event); err != nil {
			return nil, err
		}
		if event.RunID == "" || event.Actor == "" || event.Audit == "" || event.OccurredAt.IsZero() || event.OccurredAt.Location() != time.UTC || event.RunRef.Contract != ContractRun {
			return nil, fmt.Errorf("%w: invalid calibration event", ErrCorrupt)
		}
	}
	return events, nil
}
func authorizeRead(access evaluation.Access) error {
	if err := access.Validate(); err != nil {
		return err
	}
	if !slices.Contains(access.Roles, evaluation.RoleDatasetCurator) && !slices.Contains(access.Roles, evaluation.RoleDatasetAdjudicator) {
		return fmt.Errorf("%w: dataset curator or adjudicator role is required", ErrUnauthorized)
	}
	return nil
}
func jsonEqual(raw json.RawMessage, value any) bool {
	data, err := json.Marshal(value)
	return err == nil && bytes.Equal(compact(raw), compact(data))
}
func compact(data []byte) []byte {
	var buffer bytes.Buffer
	if json.Compact(&buffer, data) != nil {
		return nil
	}
	return buffer.Bytes()
}
func decodeStrict(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON", ErrCorrupt)
	}
	return nil
}
