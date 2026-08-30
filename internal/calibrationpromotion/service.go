package calibrationpromotion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"argus.local/argus/internal/calibration"
	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

type Service struct {
	store       *local.Store
	calibration CalibrationRepository
	config      ConfigRepository
	evaluation  EvaluationRepository
	runs        RunRepository
	mu          sync.Mutex
}

type CalibrationRepository interface {
	Get(string, evaluation.Access) (calibration.Run, error)
}

type ConfigRepository interface {
	Create(context.Context, reviewconfig.Revision, configrepo.Mutation) (configrepo.Record, error)
	ValidateRevision(context.Context, string, string, configrepo.Mutation) (configrepo.Record, error)
	Publish(context.Context, string, string, configrepo.Rollout, configrepo.Mutation) (configrepo.Record, error)
	Rollback(context.Context, string, string, configrepo.Mutation) (configrepo.Record, error)
	Get(string, string) (configrepo.Record, error)
	ResolvePublishedWithReceipt(context.Context, reviewconfig.ResolutionContext) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error)
}

type EvaluationRepository interface {
	RegisterManagedPromotion(context.Context, evaluation.PromotionVariant, evaluation.Mutation) (evaluation.PromotionRecord, error)
	RecordManagedGate(context.Context, evaluation.GateResult, evaluation.PromotionManagementBinding, evaluation.Mutation) (evaluation.PromotionRecord, error)
	RollbackManagedPromotion(context.Context, string, evaluation.PromotionManagementBinding, evaluation.Mutation) (evaluation.PromotionRecord, error)
	GetPromotion(string, evaluation.Access) (evaluation.PromotionRecord, error)
	GetExperimentRun(string, evaluation.Access) (evaluation.ExperimentRun, error)
	GetEvaluationRun(string, evaluation.Access) (evaluation.EvaluationRun, error)
}

type RunRepository interface {
	LoadRun(string) (runmodel.ReviewRun, error)
	LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
}

func New(store *local.Store, calibrationRepository CalibrationRepository, config ConfigRepository, evaluationRepository EvaluationRepository, runs RunRepository) (*Service, error) {
	if store == nil || calibrationRepository == nil || config == nil || evaluationRepository == nil || runs == nil {
		return nil, fmt.Errorf("promotion store and all governed repositories are required")
	}
	service := &Service{store: store, calibration: calibrationRepository, config: config, evaluation: evaluationRepository, runs: runs}
	if _, err := service.load(); err != nil {
		return nil, err
	}
	return service, nil
}

func (service *Service) Prepare(ctx context.Context, request PrepareRequest, mutation evaluation.Mutation) (Plan, error) {
	if err := request.Validate(); err != nil {
		return Plan{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Plan{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return Plan{}, fmt.Errorf("created_at must equal mutation time")
	}
	if !hasRoles(mutation.Roles, evaluation.RoleDatasetCurator, evaluation.RolePromotionOperator) {
		return Plan{}, fmt.Errorf("%w: dataset_curator and promotion_operator roles are required", ErrUnauthorized)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	plans, err := service.load()
	if err != nil {
		return Plan{}, err
	}
	plan, exists := plans[request.PlanID]
	if exists {
		if !reflect.DeepEqual(plan.Request, request) {
			return Plan{}, fmt.Errorf("%w: plan_id reused with different request", ErrConflict)
		}
	} else {
		plan, err = service.derivePlan(ctx, request, mutation)
		if err != nil {
			return Plan{}, err
		}
		if err := service.appendPlan(mutation.IdempotencyKey+":plan-preparing", "preparing", plan, mutation); err != nil {
			return Plan{}, err
		}
	}
	configMutation := configrepo.Mutation{Actor: mutation.Actor, Audit: mutation.Audit, At: mutation.At.UTC()}
	configMutation.IdempotencyKey = mutation.IdempotencyKey + ":config-create"
	if _, err := service.config.Create(ctx, plan.VariantConfig, configMutation); err != nil {
		return Plan{}, fmt.Errorf("create variant config: %w", err)
	}
	configMutation.IdempotencyKey = mutation.IdempotencyKey + ":config-validate"
	configRecord, err := service.config.ValidateRevision(ctx, plan.VariantConfig.ID, plan.VariantConfig.Revision, configMutation)
	if err != nil {
		return Plan{}, fmt.Errorf("validate variant config: %w", err)
	}
	promotionMutation := mutation
	promotionMutation.IdempotencyKey = mutation.IdempotencyKey + ":promotion-register"
	promotion, err := service.evaluation.RegisterManagedPromotion(ctx, plan.PromotionVariant, promotionMutation)
	if err != nil {
		return Plan{}, fmt.Errorf("register managed promotion: %w", err)
	}
	plan.Status, plan.ConfigStatus, plan.PromotionStatus = StatusPrepared, configRecord.Status, promotion.Status
	plan.NextGate = promotion.NextGate
	plan.UpdatedAt, plan.UpdatedBy = mutation.At.UTC(), mutation.Actor
	if err := service.appendPlan(mutation.IdempotencyKey+":plan-prepared", "prepared", plan, mutation); err != nil {
		return Plan{}, err
	}
	return clonePlan(plan), nil
}

func (service *Service) derivePlan(ctx context.Context, request PrepareRequest, mutation evaluation.Mutation) (Plan, error) {
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	calibrationRun, err := service.calibration.Get(request.CalibrationRunID, access)
	if err != nil {
		return Plan{}, fmt.Errorf("load calibration run: %w", err)
	}
	if calibrationRun.SHA256 != request.ExpectedCalibrationRunSHA256 || !calibrationRun.Report.Passed || calibrationRun.ProfileCandidate.Status != "gate_passed" || calibrationRun.ProfileCandidate.AutoPublished {
		return Plan{}, fmt.Errorf("%w: calibration run is not the exact passed unpublished candidate", ErrEvidenceMismatch)
	}
	baselineRun, err := service.runs.LoadRun(request.BaselineReviewRunID)
	if err != nil {
		return Plan{}, fmt.Errorf("load baseline ReviewRun: %w", err)
	}
	if baselineRun.Status != runmodel.RunStatusSucceeded {
		return Plan{}, fmt.Errorf("baseline ReviewRun must be succeeded")
	}
	snapshot, err := service.runs.LoadExecutionSnapshot(baselineRun.ExecutionSnapshotID)
	if err != nil {
		return Plan{}, err
	}
	bundleData, err := service.runs.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return Plan{}, err
	}
	baselineBundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		return Plan{}, err
	}
	if baselineBundle.FindingGovernance == nil {
		return Plan{}, fmt.Errorf("baseline config has no finding governance")
	}
	currentBundle, receipt, err := service.config.ResolvePublishedWithReceipt(ctx, baselineBundle.Context)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve current baseline config: %w", err)
	}
	if currentBundle.SHA256 != baselineBundle.SHA256 || snapshot.ConfigBundleRef.SHA256 != artifactDigest(bundleData) {
		return Plan{}, fmt.Errorf("%w: baseline ReviewRun config is not the current exact lifecycle resolution", ErrEvidenceMismatch)
	}
	baselineRecord, err := service.config.Get(request.BaselineConfigRevisionID, request.BaselineConfigRevision)
	if err != nil {
		return Plan{}, err
	}
	if baselineRecord.Status != configrepo.StatusPublished || !receiptContains(receipt, baselineRecord) {
		return Plan{}, fmt.Errorf("%w: baseline config revision is not active in the exact resolution", ErrEvidenceMismatch)
	}
	policy, err := reviewconfig.SealFindingGovernancePolicy(calibrationRun.ProfileCandidate.Profile, baselineBundle.FindingGovernance.MinimumConfidencePPM, baselineBundle.FindingGovernance.MaxFindings)
	if err != nil {
		return Plan{}, err
	}
	variantBundle, _, err := formalreview.BuildFindingGovernanceReplayConfig(baselineBundle, receipt, policy)
	if err != nil {
		return Plan{}, err
	}
	minimum, maximum := policy.MinimumConfidencePPM, policy.MaxFindings
	profile := policy.CalibrationProfile
	variantRevision := reviewconfig.Revision{
		SchemaVersion: reviewconfig.RevisionSchemaVersion, ID: request.VariantConfigRevisionID,
		Revision: request.VariantConfigRevision, Scope: baselineRecord.Revision.Scope,
		Selector: baselineRecord.Revision.Selector,
		Patch:    reviewconfig.ConfigPatch{FindingGovernance: &reviewconfig.FindingGovernancePatch{CalibrationProfile: &profile, MinimumConfidencePPM: &minimum, MaxFindings: &maximum}},
	}
	variantRevisionSHA, err := reviewconfig.DigestRevision(variantRevision)
	if err != nil {
		return Plan{}, err
	}
	binding := evaluation.PromotionManagementBinding{
		Kind: "calibration_config", CalibrationRunID: calibrationRun.RunID,
		CalibrationRunSHA256:    calibrationRun.SHA256,
		ProfileCandidateID:      calibrationRun.ProfileCandidate.CandidateID,
		ProfileCandidateSHA256:  calibrationRun.ProfileCandidate.SHA256,
		CalibrationReportSHA256: calibrationRun.Report.SHA256,
		BaselineBundleSHA256:    baselineBundle.SHA256,
		ConfigRevisionID:        variantRevision.ID, ConfigRevision: variantRevision.Revision,
		ConfigRevisionSHA256: variantRevisionSHA,
	}
	promotion := evaluation.PromotionVariant{
		SchemaVersion: evaluation.PromotionVariantSchemaVersion, VariantID: request.PromotionVariantID,
		Component:        evaluation.PromotionFilter,
		Revision:         configIdentity(variantRevision.ID, variantRevision.Revision),
		RollbackRevision: configIdentity(baselineRecord.Revision.ID, baselineRecord.Revision.Revision),
		PolicyRevision:   request.PromotionPolicyRevision, Origin: evaluation.PromotionExperiment,
		Owner: request.Owner, CreatedAt: request.CreatedAt, ManagedBinding: &binding,
	}
	// PromotionVariant.Revision is the globally unique projection identity; the
	// binding separately carries the exact config lifecycle identity.
	binding.ConfigRevision = promotion.Revision
	promotion.ManagedBinding = &binding
	if err := promotion.Validate(); err != nil {
		return Plan{}, err
	}
	return Plan{
		SchemaVersion: PlanSchemaVersion, Request: request, Status: StatusPreparing,
		CalibrationRunSHA256:    calibrationRun.SHA256,
		ProfileCandidateID:      calibrationRun.ProfileCandidate.CandidateID,
		ProfileCandidateSHA256:  calibrationRun.ProfileCandidate.SHA256,
		CalibrationReportSHA256: calibrationRun.Report.SHA256,
		BaselineBundleSHA256:    baselineBundle.SHA256, VariantBundleSHA256: variantBundle.SHA256,
		BaselineConfigSHA256: baselineRecord.SHA256, VariantConfig: variantRevision,
		VariantConfigSHA256: variantRevisionSHA, PromotionVariant: promotion,
		PromotionStatus: evaluation.PromotionRegistered, ConfigStatus: configrepo.StatusDraft,
		NextGate:  func() *evaluation.PromotionGate { gate := evaluation.GateSchemaContract; return &gate }(),
		UpdatedAt: mutation.At.UTC(), UpdatedBy: mutation.Actor,
	}, nil
}

func (service *Service) RecordGate(ctx context.Context, request GateRequest, mutation evaluation.Mutation) (Plan, error) {
	if err := request.Validate(); err != nil {
		return Plan{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Plan{}, err
	}
	if !hasRoles(mutation.Roles, evaluation.RoleDatasetCurator) {
		return Plan{}, fmt.Errorf("%w: dataset_curator role is required", ErrUnauthorized)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	plan, err := service.getLocked(request.PlanID)
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != StatusPrepared && plan.Status != StatusGating {
		return Plan{}, fmt.Errorf("%w: plan status %q cannot accept gates", ErrInvalidTransition, plan.Status)
	}
	if request.Result.VariantID != plan.PromotionVariant.VariantID {
		return Plan{}, fmt.Errorf("%w: gate variant does not match plan", ErrEvidenceMismatch)
	}
	if err := service.revalidatePlan(plan, mutation); err != nil {
		return Plan{}, err
	}
	if request.Result.Outcome == evaluation.GatePass {
		switch request.Result.Gate {
		case evaluation.GateTargetedRegression:
			if err := service.validateExperiment(plan, request.ExperimentRunID, mutation); err != nil {
				return Plan{}, err
			}
		case evaluation.GateFixedHoldout:
			calibrationRun, loadErr := service.calibration.Get(plan.Request.CalibrationRunID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
			if loadErr != nil {
				return Plan{}, loadErr
			}
			if mutation.Actor == plan.Request.Owner || mutation.Actor == calibrationRun.CreatedBy {
				return Plan{}, fmt.Errorf("%w: holdout runner must be independent from preparation owner", ErrUnauthorized)
			}
			if err := service.validateHoldout(plan, request.Result, mutation); err != nil {
				return Plan{}, err
			}
		}
	}
	record, err := service.evaluation.RecordManagedGate(ctx, request.Result, *plan.PromotionVariant.ManagedBinding, mutation)
	if err != nil {
		return Plan{}, err
	}
	plan.PromotionStatus = record.Status
	plan.NextGate = record.NextGate
	switch record.Status {
	case evaluation.PromotionActive:
		plan.Status = StatusGatesPassed
	case evaluation.PromotionFailed:
		plan.Status = StatusFailed
	case evaluation.PromotionInconclusive:
		plan.Status = StatusInconclusive
	default:
		plan.Status = StatusGating
	}
	plan.UpdatedAt, plan.UpdatedBy = mutation.At.UTC(), mutation.Actor
	if err := service.appendPlan(mutation.IdempotencyKey+":plan-gate", "gate_recorded", plan, mutation); err != nil {
		return Plan{}, err
	}
	return clonePlan(plan), nil
}

func (service *Service) Activate(ctx context.Context, planID string, rollout configrepo.Rollout, mutation evaluation.Mutation) (Plan, error) {
	if err := rollout.Validate(); err != nil {
		return Plan{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Plan{}, err
	}
	if !hasRoles(mutation.Roles, evaluation.RoleDatasetCurator, evaluation.RolePromotionOperator) {
		return Plan{}, fmt.Errorf("%w: dataset_curator and promotion_operator roles are required", ErrUnauthorized)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	plan, err := service.getLocked(planID)
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != StatusGatesPassed && plan.Status != StatusActivating {
		return Plan{}, fmt.Errorf("%w: plan status %q cannot activate", ErrInvalidTransition, plan.Status)
	}
	if err := service.revalidatePlan(plan, mutation); err != nil {
		return Plan{}, err
	}
	promotion, err := service.evaluation.GetPromotion(plan.PromotionVariant.VariantID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil || promotion.Status != evaluation.PromotionActive {
		return Plan{}, fmt.Errorf("%w: managed promotion gates are not active", ErrInvalidTransition)
	}
	if plan.Status != StatusActivating {
		plan.Status, plan.Rollout, plan.UpdatedAt, plan.UpdatedBy = StatusActivating, &rollout, mutation.At.UTC(), mutation.Actor
		if err := service.appendPlan(mutation.IdempotencyKey+":activation-intent", "activation_intent", plan, mutation); err != nil {
			return Plan{}, err
		}
	} else if plan.Rollout == nil || *plan.Rollout != rollout {
		return Plan{}, fmt.Errorf("%w: activation retry changed rollout", ErrConflict)
	}
	configMutation := configrepo.Mutation{IdempotencyKey: mutation.IdempotencyKey + ":config-publish", Actor: mutation.Actor, Audit: mutation.Audit, At: mutation.At.UTC()}
	record, err := service.config.Publish(ctx, plan.VariantConfig.ID, plan.VariantConfig.Revision, rollout, configMutation)
	if err != nil {
		return Plan{}, err
	}
	plan.Status, plan.ConfigStatus, plan.PromotionStatus = StatusActive, record.Status, promotion.Status
	plan.UpdatedAt, plan.UpdatedBy = mutation.At.UTC(), mutation.Actor
	if err := service.appendPlan(mutation.IdempotencyKey+":activation-complete", "activated", plan, mutation); err != nil {
		return Plan{}, err
	}
	return clonePlan(plan), nil
}

func (service *Service) Rollback(ctx context.Context, planID string, mutation evaluation.Mutation) (Plan, error) {
	if err := mutation.Validate(); err != nil {
		return Plan{}, err
	}
	if !hasRoles(mutation.Roles, evaluation.RolePromotionOperator) {
		return Plan{}, fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	plan, err := service.getLocked(planID)
	if err != nil {
		return Plan{}, err
	}
	if plan.Status != StatusActive && plan.Status != StatusRollbackPending {
		return Plan{}, fmt.Errorf("%w: plan status %q cannot rollback", ErrInvalidTransition, plan.Status)
	}
	if plan.Status != StatusRollbackPending {
		plan.Status, plan.UpdatedAt, plan.UpdatedBy = StatusRollbackPending, mutation.At.UTC(), mutation.Actor
		if err := service.appendPlan(mutation.IdempotencyKey+":rollback-intent", "rollback_intent", plan, mutation); err != nil {
			return Plan{}, err
		}
	}
	configMutation := configrepo.Mutation{IdempotencyKey: mutation.IdempotencyKey + ":config-rollback", Actor: mutation.Actor, Audit: mutation.Audit, At: mutation.At.UTC()}
	configRecord, err := service.config.Rollback(ctx, plan.VariantConfig.ID, plan.VariantConfig.Revision, configMutation)
	if err != nil {
		return Plan{}, err
	}
	promotionMutation := mutation
	promotionMutation.IdempotencyKey = mutation.IdempotencyKey + ":promotion-rollback"
	promotion, err := service.evaluation.RollbackManagedPromotion(ctx, plan.PromotionVariant.VariantID, *plan.PromotionVariant.ManagedBinding, promotionMutation)
	if err != nil {
		return Plan{}, err
	}
	plan.Status, plan.ConfigStatus, plan.PromotionStatus = StatusRolledBack, configRecord.Status, promotion.Status
	plan.UpdatedAt, plan.UpdatedBy = mutation.At.UTC(), mutation.Actor
	if err := service.appendPlan(mutation.IdempotencyKey+":rollback-complete", "rolled_back", plan, mutation); err != nil {
		return Plan{}, err
	}
	return clonePlan(plan), nil
}

func (service *Service) Get(planID string, access evaluation.Access) (Plan, error) {
	if err := authorizeRead(access); err != nil {
		return Plan{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.getLocked(planID)
}

func (service *Service) List(access evaluation.Access) ([]Plan, error) {
	if err := authorizeRead(access); err != nil {
		return nil, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	plans, err := service.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(plans))
	for id := range plans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]Plan, 0, len(ids))
	for _, id := range ids {
		result = append(result, clonePlan(plans[id]))
	}
	return result, nil
}

func (service *Service) revalidatePlan(plan Plan, mutation evaluation.Mutation) error {
	run, err := service.calibration.Get(plan.Request.CalibrationRunID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil || run.SHA256 != plan.CalibrationRunSHA256 || run.ProfileCandidate.SHA256 != plan.ProfileCandidateSHA256 || run.Report.SHA256 != plan.CalibrationReportSHA256 || !run.Report.Passed {
		return fmt.Errorf("%w: calibration source drifted", ErrEvidenceMismatch)
	}
	record, err := service.config.Get(plan.VariantConfig.ID, plan.VariantConfig.Revision)
	if err != nil || record.SHA256 != plan.VariantConfigSHA256 || (record.Status != configrepo.StatusValidated && record.Status != configrepo.StatusPublished) {
		return fmt.Errorf("%w: variant config drifted or is not eligible", ErrEvidenceMismatch)
	}
	promotion, err := service.evaluation.GetPromotion(plan.PromotionVariant.VariantID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil || !reflect.DeepEqual(promotion.Variant, plan.PromotionVariant) {
		return fmt.Errorf("%w: promotion binding drifted", ErrEvidenceMismatch)
	}
	return nil
}

func (service *Service) validateExperiment(plan Plan, experimentRunID string, mutation evaluation.Mutation) error {
	run, err := service.evaluation.GetExperimentRun(experimentRunID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return err
	}
	if !run.Variable.IsFilterPolicy() || len(run.Comparisons) == 0 || run.Summary.Regressed != 0 || run.Summary.Inconclusive != 0 {
		return fmt.Errorf("%w: targeted regression is not a conclusive non-regressing filter_policy experiment", ErrEvidenceMismatch)
	}
	for _, comparison := range run.Comparisons {
		if comparison.BaselineConfigSHA256 != plan.BaselineBundleSHA256 || comparison.VariantConfigSHA256 != plan.VariantBundleSHA256 {
			return fmt.Errorf("%w: experiment comparison config binding mismatch", ErrEvidenceMismatch)
		}
	}
	return nil
}

func (service *Service) validateHoldout(plan Plan, result evaluation.GateResult, mutation evaluation.Mutation) error {
	run, err := service.evaluation.GetEvaluationRun(result.Evidence.EvaluationRunID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return err
	}
	caseIDs := make([]string, 0, len(run.Results))
	for _, item := range run.Results {
		caseIDs = append(caseIDs, item.CaseID)
		reviewRun, loadErr := service.runs.LoadRun(item.ReviewRunID)
		if loadErr != nil {
			return loadErr
		}
		snapshot, loadErr := service.runs.LoadExecutionSnapshot(reviewRun.ExecutionSnapshotID)
		if loadErr != nil {
			return loadErr
		}
		data, loadErr := service.runs.ReadArtifact(snapshot.ConfigBundleRef)
		if loadErr != nil {
			return loadErr
		}
		bundle, loadErr := reviewconfig.DecodeBundle(data)
		if loadErr != nil {
			return loadErr
		}
		if bundle.SHA256 != plan.VariantBundleSHA256 {
			return fmt.Errorf("%w: holdout ReviewRun uses a different config", ErrEvidenceMismatch)
		}
	}
	sort.Strings(caseIDs)
	if !slices.Equal(caseIDs, result.Evidence.HoldoutCaseIDs) {
		return fmt.Errorf("%w: holdout EvaluationRun population differs from gate evidence", ErrEvidenceMismatch)
	}
	calibrationRun, loadErr := service.calibration.Get(plan.Request.CalibrationRunID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if loadErr != nil {
		return loadErr
	}
	for _, ref := range []runmodel.ArtifactRef{calibrationRun.TrainingManifestRef, calibrationRun.ValidationManifestRef} {
		data, readErr := service.runs.ReadArtifact(ref)
		if readErr != nil {
			return readErr
		}
		var manifest calibration.DatasetManifest
		if err := json.Unmarshal(data, &manifest); err != nil || manifest.Validate() != nil {
			return fmt.Errorf("%w: calibration manifest is invalid", ErrEvidenceMismatch)
		}
		for _, sample := range manifest.Samples {
			if slices.Contains(caseIDs, sample.CaseID) {
				return fmt.Errorf("%w: holdout case was exposed to fitting", evaluation.ErrContaminated)
			}
		}
	}
	return nil
}

func (service *Service) getLocked(planID string) (Plan, error) {
	if !idPattern.MatchString(planID) {
		return Plan{}, fmt.Errorf("plan_id is invalid")
	}
	plans, err := service.load()
	if err != nil {
		return Plan{}, err
	}
	plan, ok := plans[planID]
	if !ok {
		return Plan{}, fmt.Errorf("%w: %s", ErrNotFound, planID)
	}
	return clonePlan(plan), nil
}

func (service *Service) load() (map[string]Plan, error) {
	envelopes, err := service.store.ReadJSONL(planStream)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Plan{}, nil
	}
	if err != nil {
		return nil, err
	}
	plans := map[string]Plan{}
	for _, envelope := range envelopes {
		if envelope.Schema != planEventSchema {
			return nil, fmt.Errorf("%w: unsupported event schema", ErrCorrupt)
		}
		var event planEvent
		if err := decodePlanEvent(envelope.Payload, &event); err != nil || event.SchemaVersion != planEventSchema || !event.OccurredAt.Equal(envelope.Time.UTC()) || !event.Plan.UpdatedAt.Equal(event.OccurredAt) || event.Plan.UpdatedBy != event.Actor {
			return nil, fmt.Errorf("%w: invalid plan event %q", ErrCorrupt, envelope.ID)
		}
		prior, exists := plans[event.Plan.Request.PlanID]
		if exists && !samePlanIdentity(prior, event.Plan) {
			return nil, fmt.Errorf("%w: plan identity changed", ErrCorrupt)
		}
		if err := validateEventTransition(event.Type, prior, exists, event.Plan); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if err := event.Plan.Validate(); err != nil {
			return nil, fmt.Errorf("%w: invalid plan event %q: %v", ErrCorrupt, envelope.ID, err)
		}
		plans[event.Plan.Request.PlanID] = event.Plan
	}
	return plans, nil
}

func (service *Service) appendPlan(id, eventType string, plan Plan, mutation evaluation.Mutation) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	event := planEvent{SchemaVersion: planEventSchema, Type: eventType, Plan: clonePlan(plan), Actor: mutation.Actor, Audit: mutation.Audit, OccurredAt: mutation.At.UTC()}
	envelopes, err := service.store.ReadJSONL(planStream)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, envelope := range envelopes {
		if envelope.ID != id {
			continue
		}
		var existing planEvent
		if json.Unmarshal(envelope.Payload, &existing) != nil || !reflect.DeepEqual(existing, event) {
			return fmt.Errorf("%w: idempotency key reused", ErrConflict)
		}
		return nil
	}
	_, err = service.store.AppendJSONL(planStream, local.Event{ID: id, Schema: planEventSchema, Time: mutation.At.UTC(), Payload: event})
	return err
}

func decodePlanEvent(data []byte, event *planEvent) error {
	if err := rejectDuplicateFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(event); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing plan event JSON")
	}
	return nil
}

func validateEventTransition(eventType string, prior Plan, exists bool, next Plan) error {
	if !exists {
		if eventType != "preparing" || next.Status != StatusPreparing {
			return fmt.Errorf("first event must be preparing")
		}
		return nil
	}
	valid := false
	switch eventType {
	case "prepared":
		valid = prior.Status == StatusPreparing && next.Status == StatusPrepared
	case "gate_recorded":
		valid = (prior.Status == StatusPrepared || prior.Status == StatusGating) && (next.Status == StatusGating || next.Status == StatusGatesPassed || next.Status == StatusFailed || next.Status == StatusInconclusive)
	case "activation_intent":
		valid = prior.Status == StatusGatesPassed && next.Status == StatusActivating
	case "activated":
		valid = prior.Status == StatusActivating && next.Status == StatusActive
	case "rollback_intent":
		valid = prior.Status == StatusActive && next.Status == StatusRollbackPending
	case "rolled_back":
		valid = prior.Status == StatusRollbackPending && next.Status == StatusRolledBack
	}
	if !valid {
		return fmt.Errorf("invalid %s transition from %s to %s", eventType, prior.Status, next.Status)
	}
	return nil
}

func samePlanIdentity(left, right Plan) bool {
	left.Status, right.Status = "", ""
	left.PromotionStatus, right.PromotionStatus = "", ""
	left.NextGate, right.NextGate = nil, nil
	left.ConfigStatus, right.ConfigStatus = "", ""
	left.Rollout, right.Rollout = nil, nil
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	left.UpdatedBy, right.UpdatedBy = "", ""
	return reflect.DeepEqual(left, right)
}

func receiptContains(receipt reviewconfig.ConfigResolutionReceipt, record configrepo.Record) bool {
	for _, binding := range receipt.Revisions {
		if binding.Source.ID == record.Revision.ID && binding.Source.Revision == record.Revision.Revision && binding.RevisionSHA256 == record.SHA256 {
			return true
		}
	}
	return false
}
func configIdentity(id, revision string) string { return id + ":" + revision }
func artifactDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func hasRoles(actual []evaluation.Role, required ...evaluation.Role) bool {
	for _, role := range required {
		if !slices.Contains(actual, role) {
			return false
		}
	}
	return true
}
func authorizeRead(access evaluation.Access) error {
	if err := access.Validate(); err != nil {
		return err
	}
	if !hasRoles(access.Roles, evaluation.RoleDatasetCurator) && !hasRoles(access.Roles, evaluation.RoleDatasetAdjudicator) && !hasRoles(access.Roles, evaluation.RolePromotionOperator) && !hasRoles(access.Roles, evaluation.RolePromotionApprover) {
		return ErrUnauthorized
	}
	return nil
}
func clonePlan(plan Plan) Plan {
	data, _ := json.Marshal(plan)
	var clone Plan
	_ = json.Unmarshal(data, &clone)
	return clone
}
