package evaluation

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
)

const (
	CorpusSnapshotRequestSchemaVersion = "argus.corpus_snapshot_request.v1alpha1"
	CorpusSnapshotSchemaVersion        = "argus.corpus_snapshot.v1alpha1"
	CorpusSnapshotContract             = CorpusSnapshotSchemaVersion
)

type CorpusPurpose string

const (
	CorpusPurposeDevelopment   CorpusPurpose = "development"
	CorpusPurposeQualityGate   CorpusPurpose = "quality_gate"
	CorpusPurposePromotionGate CorpusPurpose = "promotion_gate"
)

// CorpusSnapshotRequest identifies one governed corpus revision before any
// model sees it. Cases are sorted so the resulting artifact is canonical and
// reusable across baseline, experiment, and repeatability runs.
type CorpusSnapshotRequest struct {
	SchemaVersion string                      `json:"schema_version"`
	CorpusID      string                      `json:"corpus_id"`
	Revision      string                      `json:"revision"`
	Purpose       CorpusPurpose               `json:"purpose"`
	Split         Split                       `json:"split"`
	Cases         []CorpusSnapshotCaseRequest `json:"cases"`
	CreatedAt     time.Time                   `json:"created_at"`
}

type CorpusSnapshotCaseRequest struct {
	CaseID                     string `json:"case_id"`
	ExpectedGovernanceRevision uint64 `json:"expected_governance_revision"`
	ExpectedLabelRevision      uint64 `json:"expected_label_revision"`
	SourceReviewRunID          string `json:"source_review_run_id"`
}

// CorpusSnapshotCase freezes both the independently governed case semantics
// and the exact source execution closure. It deliberately contains no model,
// prompt, skill, or review output labels.
type CorpusSnapshotCase struct {
	CaseID                  string               `json:"case_id"`
	CaseType                CaseType             `json:"case_type"`
	Split                   Split                `json:"split"`
	CloneGroupID            string               `json:"clone_group_id"`
	GovernanceRevision      uint64               `json:"governance_revision"`
	LabelRevision           uint64               `json:"label_revision"`
	CurrentCaseSHA256       string               `json:"current_case_sha256"`
	SourceReviewRunID       string               `json:"source_review_run_id"`
	SourceReviewRunRef      runmodel.ArtifactRef `json:"source_review_run_ref"`
	TargetSnapshotRef       runmodel.ArtifactRef `json:"target_snapshot_ref"`
	ExecutionSnapshotSHA256 string               `json:"execution_snapshot_sha256"`
}

type CorpusSnapshot struct {
	SchemaVersion string               `json:"schema_version"`
	CorpusID      string               `json:"corpus_id"`
	Revision      string               `json:"revision"`
	Purpose       CorpusPurpose        `json:"purpose"`
	Split         Split                `json:"split"`
	Cases         []CorpusSnapshotCase `json:"cases"`
	CreatedAt     time.Time            `json:"created_at"`
	CreatedBy     string               `json:"created_by"`
}

type CorpusSnapshotBuilder struct {
	repository *Repository
	runs       EvaluationRunReader
}

func (request CorpusSnapshotRequest) Validate() error {
	if request.SchemaVersion != CorpusSnapshotRequestSchemaVersion {
		return fmt.Errorf("unsupported corpus snapshot request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{"corpus_id": request.CorpusID, "revision": request.Revision} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := request.Purpose.validateSplit(request.Split); err != nil {
		return err
	}
	if len(request.Cases) < 1 || len(request.Cases) > 1024 {
		return fmt.Errorf("cases must contain between 1 and 1024 entries")
	}
	previous := ""
	for index, item := range request.Cases {
		if err := item.validate(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		if index > 0 && item.CaseID <= previous {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
		previous = item.CaseID
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func (item CorpusSnapshotCaseRequest) validate() error {
	if err := validateID("case_id", item.CaseID); err != nil {
		return err
	}
	if item.ExpectedGovernanceRevision == 0 || item.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected governance and label revisions must be positive")
	}
	return validateID("source_review_run_id", item.SourceReviewRunID)
}

func (snapshot CorpusSnapshot) Validate() error {
	if snapshot.SchemaVersion != CorpusSnapshotSchemaVersion {
		return fmt.Errorf("unsupported corpus snapshot schema %q", snapshot.SchemaVersion)
	}
	for name, value := range map[string]string{
		"corpus_id": snapshot.CorpusID, "revision": snapshot.Revision, "created_by": snapshot.CreatedBy,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := snapshot.Purpose.validateSplit(snapshot.Split); err != nil {
		return err
	}
	if len(snapshot.Cases) < 1 || len(snapshot.Cases) > 1024 {
		return fmt.Errorf("cases must contain between 1 and 1024 entries")
	}
	previous := ""
	cloneGroups := make(map[string]string, len(snapshot.Cases))
	for index, item := range snapshot.Cases {
		if err := item.validate(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		if index > 0 && item.CaseID <= previous {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
		if item.Split != snapshot.Split {
			return fmt.Errorf("cases[%d] split %q differs from corpus split %q", index, item.Split, snapshot.Split)
		}
		if existing, exists := cloneGroups[item.CloneGroupID]; exists {
			return fmt.Errorf("cases[%d] clone group %q duplicates case %q", index, item.CloneGroupID, existing)
		}
		cloneGroups[item.CloneGroupID] = item.CaseID
		previous = item.CaseID
	}
	if snapshot.CreatedAt.IsZero() || snapshot.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func (purpose CorpusPurpose) validateSplit(split Split) error {
	if err := split.Validate(); err != nil || split == SplitUnassigned {
		return fmt.Errorf("corpus requires an assigned split: %v", err)
	}
	switch purpose {
	case CorpusPurposeDevelopment:
		if split != SplitTrain && split != SplitDev {
			return fmt.Errorf("development corpus requires train or dev split, got %q", split)
		}
	case CorpusPurposeQualityGate:
		if split != SplitTest {
			return fmt.Errorf("quality_gate corpus requires test split, got %q", split)
		}
	case CorpusPurposePromotionGate:
		if split != SplitHoldout {
			return fmt.Errorf("promotion_gate corpus requires holdout split, got %q", split)
		}
	default:
		return fmt.Errorf("unsupported corpus purpose %q", purpose)
	}
	return nil
}

func (item CorpusSnapshotCase) validate() error {
	for name, value := range map[string]string{
		"case_id": item.CaseID, "clone_group_id": item.CloneGroupID,
		"source_review_run_id": item.SourceReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := item.CaseType.Validate(); err != nil {
		return err
	}
	if item.CaseType == CaseFixValidation {
		return fmt.Errorf("fix_validation requires an apply trial and cannot enter formal corpus")
	}
	if err := item.Split.Validate(); err != nil || item.Split == SplitUnassigned {
		return fmt.Errorf("snapshot case requires an assigned split: %v", err)
	}
	if item.GovernanceRevision == 0 || item.LabelRevision == 0 {
		return fmt.Errorf("governance and label revisions must be positive")
	}
	if err := validateSHA256("current_case_sha256", item.CurrentCaseSHA256); err != nil {
		return err
	}
	if err := item.SourceReviewRunRef.Validate(); err != nil {
		return fmt.Errorf("source_review_run_ref: %w", err)
	}
	if item.SourceReviewRunRef.Contract != runmodel.ContractReviewRun {
		return fmt.Errorf("source_review_run_ref must reference a ReviewRun")
	}
	if err := item.TargetSnapshotRef.Validate(); err != nil {
		return fmt.Errorf("target_snapshot_ref: %w", err)
	}
	if item.TargetSnapshotRef.Contract != runmodel.ContractMaterializedTarget {
		return fmt.Errorf("target_snapshot_ref must reference a MaterializedTarget")
	}
	return validateSHA256("execution_snapshot_sha256", item.ExecutionSnapshotSHA256)
}

func DecodeCorpusSnapshotRequest(data []byte) (CorpusSnapshotRequest, error) {
	return decodeStrict(data, "CorpusSnapshotRequest", func(value CorpusSnapshotRequest) error {
		return value.Validate()
	})
}

func DecodeCorpusSnapshot(data []byte) (CorpusSnapshot, error) {
	return decodeStrict(data, "CorpusSnapshot", func(value CorpusSnapshot) error {
		return value.Validate()
	})
}

func NewCorpusSnapshotBuilder(repository *Repository, runs EvaluationRunReader) (*CorpusSnapshotBuilder, error) {
	if repository == nil || runs == nil {
		return nil, fmt.Errorf("evaluation repository and run reader are required")
	}
	return &CorpusSnapshotBuilder{repository: repository, runs: runs}, nil
}

func (builder *CorpusSnapshotBuilder) Build(
	ctx context.Context,
	request CorpusSnapshotRequest,
	access Access,
) (CorpusSnapshot, error) {
	if err := request.Validate(); err != nil {
		return CorpusSnapshot{}, err
	}
	if err := validateID("access.actor", access.Actor); err != nil {
		return CorpusSnapshot{}, err
	}
	result := CorpusSnapshot{
		SchemaVersion: CorpusSnapshotSchemaVersion,
		CorpusID:      request.CorpusID, Revision: request.Revision,
		Purpose: request.Purpose, Split: request.Split,
		Cases:     make([]CorpusSnapshotCase, 0, len(request.Cases)),
		CreatedAt: request.CreatedAt, CreatedBy: access.Actor,
	}
	for _, item := range request.Cases {
		if err := ctx.Err(); err != nil {
			return CorpusSnapshot{}, err
		}
		frozen, err := builder.freezeCase(item, request.Split, access)
		if err != nil {
			return CorpusSnapshot{}, fmt.Errorf("case %q: %w", item.CaseID, err)
		}
		result.Cases = append(result.Cases, frozen)
	}
	if err := result.Validate(); err != nil {
		return CorpusSnapshot{}, fmt.Errorf("validate corpus snapshot: %w", err)
	}
	return result, nil
}

func (builder *CorpusSnapshotBuilder) freezeCase(
	item CorpusSnapshotCaseRequest,
	expectedSplit Split,
	access Access,
) (CorpusSnapshotCase, error) {
	record, err := builder.repository.GetCase(item.CaseID, access)
	if err != nil {
		return CorpusSnapshotCase{}, err
	}
	current := record.CurrentCase()
	if record.CurrentGovernance.Revision != item.ExpectedGovernanceRevision ||
		record.CurrentLabelRevision != item.ExpectedLabelRevision ||
		record.CurrentGovernance.ReviewState != ReviewApproved ||
		record.CurrentGovernance.DatasetState != DatasetActive ||
		!record.CurrentGovernance.Eligibility.Evaluation ||
		!slices.Contains(record.CurrentGovernance.LicenseConsent.AllowedUses, UseEvaluation) {
		return CorpusSnapshotCase{}, fmt.Errorf("case is not active at the expected governance and label revisions")
	}
	if current.Type == CaseFixValidation {
		return CorpusSnapshotCase{}, fmt.Errorf("fix_validation requires an apply trial and cannot enter formal corpus")
	}
	if current.Split != expectedSplit {
		return CorpusSnapshotCase{}, fmt.Errorf("case split %q differs from corpus split %q", current.Split, expectedSplit)
	}
	if err := authorizeExposure(current, access.Roles); err != nil {
		return CorpusSnapshotCase{}, fmt.Errorf("authorize corpus exposure: %w", err)
	}
	if current.Split == SplitHoldout {
		history, err := builder.repository.ExposureHistory(item.CaseID, access)
		if err != nil {
			return CorpusSnapshotCase{}, fmt.Errorf("load holdout exposure history: %w", err)
		}
		if err := rejectSeenExposure(item.CaseID, history); err != nil {
			return CorpusSnapshotCase{}, err
		}
	}
	caseSHA, err := EvaluationCaseSHA256(current)
	if err != nil {
		return CorpusSnapshotCase{}, fmt.Errorf("digest current case: %w", err)
	}
	run, err := builder.runs.LoadRun(item.SourceReviewRunID)
	if err != nil {
		return CorpusSnapshotCase{}, fmt.Errorf("load source ReviewRun: %w", err)
	}
	if run.Kind != runmodel.RunKindReview || run.Status != runmodel.RunStatusSucceeded ||
		run.SourceRunID != "" || run.TargetSnapshotRef.URI != current.InputSnapshotRef {
		return CorpusSnapshotCase{}, fmt.Errorf("source ReviewRun does not bind the governed case input")
	}
	executionSnapshot, err := builder.runs.ExecutionSnapshotForRun(run.RunID)
	if err != nil {
		return CorpusSnapshotCase{}, fmt.Errorf("load source ExecutionSnapshot: %w", err)
	}
	if executionSnapshot.RemoteWrites != "deny" || executionSnapshot.ToolPolicy.RemoteWrites != "deny" {
		return CorpusSnapshotCase{}, fmt.Errorf("source execution does not deny remote writes")
	}
	executionSHA, err := runmodel.DigestJSON(executionSnapshot)
	if err != nil {
		return CorpusSnapshotCase{}, fmt.Errorf("digest source ExecutionSnapshot: %w", err)
	}
	runRef, err := builder.runs.CommittedRunRef(run.RunID)
	if err != nil {
		return CorpusSnapshotCase{}, fmt.Errorf("load committed source ReviewRun ref: %w", err)
	}
	for _, ref := range []runmodel.ArtifactRef{runRef, run.TargetSnapshotRef} {
		if err := builder.runs.CheckArtifactEligibility(ref, runrepo.ArtifactUseEvaluation); err != nil {
			return CorpusSnapshotCase{}, fmt.Errorf("source artifact is not evaluation eligible: %w", err)
		}
	}
	return CorpusSnapshotCase{
		CaseID: item.CaseID, CaseType: current.Type, Split: current.Split,
		CloneGroupID:       current.CloneGroupID,
		GovernanceRevision: record.CurrentGovernance.Revision,
		LabelRevision:      record.CurrentLabelRevision, CurrentCaseSHA256: caseSHA,
		SourceReviewRunID: run.RunID, SourceReviewRunRef: runRef,
		TargetSnapshotRef: run.TargetSnapshotRef, ExecutionSnapshotSHA256: executionSHA,
	}, nil
}

func rejectSeenExposure(caseID string, history []ExposureEntry) error {
	for _, entry := range history {
		for _, observation := range entry.Exposure.Observations {
			if observation.Status == ExposureSeen {
				return fmt.Errorf("%w: holdout case %q was historically seen by %s revision %q in evaluation run %q",
					ErrContaminated, caseID, observation.Component, observation.Revision, entry.Exposure.EvaluationRunID)
			}
		}
	}
	return nil
}

func (builder *CorpusSnapshotBuilder) Authorize(snapshot CorpusSnapshot, access Access) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	for _, item := range snapshot.Cases {
		if _, err := builder.repository.GetCase(item.CaseID, access); err != nil {
			return fmt.Errorf("case %q: %w", item.CaseID, err)
		}
	}
	return nil
}
