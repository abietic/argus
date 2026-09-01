package normalizationpromotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/normalizationeval"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type ConfigRepository interface {
	Create(context.Context, reviewconfig.Revision, configrepo.Mutation) (configrepo.Record, error)
	ValidateRevision(context.Context, string, string, configrepo.Mutation) (configrepo.Record, error)
	Publish(context.Context, string, string, configrepo.Rollout, configrepo.Mutation) (configrepo.Record, error)
	AdvanceRollout(context.Context, string, string, configrepo.Rollout, configrepo.Mutation) (configrepo.Record, error)
	Rollback(context.Context, string, string, configrepo.Mutation) (configrepo.Record, error)
	Get(string, string) (configrepo.Record, error)
	History(string, string) ([]configrepo.AuditEntry, error)
	ResolvePublishedWithReceipt(context.Context, reviewconfig.ResolutionContext) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error)
}

type EvaluationRepository interface {
	GetPromotion(string, evaluation.Access) (evaluation.PromotionRecord, error)
	ListPromotions(evaluation.Access) ([]evaluation.PromotionRecord, error)
	RegisterManagedPromotion(context.Context, evaluation.PromotionVariant, evaluation.Mutation) (evaluation.PromotionRecord, error)
	RecordManagedGate(context.Context, evaluation.GateResult, evaluation.PromotionManagementBinding, evaluation.Mutation) (evaluation.PromotionRecord, error)
	RollbackManagedPromotion(context.Context, string, evaluation.PromotionManagementBinding, evaluation.Mutation) (evaluation.PromotionRecord, error)
}

type RunRepository interface {
	PutJSONArtifact(string, any) (runmodel.ArtifactRef, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
	LoadRun(string) (runmodel.ReviewRun, error)
	LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error)
}

type QualityVerifier interface {
	VerifyQuality(context.Context, runmodel.ArtifactRef, evaluation.Access) (evaluation.NormalizationQualityRun, error)
}

type Service struct {
	evaluation EvaluationRepository
	runs       RunRepository
	quality    QualityVerifier
	config     ConfigRepository
}

func New(
	repository *evaluation.Repository,
	runs *runrepo.Repository,
	configs ...ConfigRepository,
) (*Service, error) {
	if repository == nil || runs == nil {
		return nil, fmt.Errorf("evaluation and run repositories are required")
	}
	if len(configs) > 1 {
		return nil, fmt.Errorf("at most one config repository is allowed")
	}
	quality, err := normalizationeval.New(repository, runs)
	if err != nil {
		return nil, err
	}
	service := &Service{evaluation: repository, runs: runs, quality: quality}
	if len(configs) == 1 {
		if configs[0] == nil {
			return nil, fmt.Errorf("config repository is nil")
		}
		service.config = configs[0]
	}
	return service, nil
}

func newService(
	repository EvaluationRepository,
	runs RunRepository,
	quality QualityVerifier,
	config ConfigRepository,
) (*Service, error) {
	if repository == nil || runs == nil || quality == nil || config == nil {
		return nil, fmt.Errorf("all normalization promotion repositories are required")
	}
	return &Service{
		evaluation: repository,
		runs:       runs,
		quality:    quality,
		config:     config,
	}, nil
}

func (service *Service) Get(variantID string, access evaluation.Access) (evaluation.PromotionRecord, error) {
	record, err := service.evaluation.GetPromotion(variantID, access)
	if err != nil {
		return evaluation.PromotionRecord{}, err
	}
	if !isNormalizationPromotion(record) {
		return evaluation.PromotionRecord{}, fmt.Errorf("%w: promotion %q is not managed by normalization", evaluation.ErrNotFound, variantID)
	}
	return record, nil
}

func (service *Service) List(access evaluation.Access) ([]evaluation.PromotionRecord, error) {
	records, err := service.evaluation.ListPromotions(access)
	if err != nil {
		return nil, err
	}
	result := make([]evaluation.PromotionRecord, 0, len(records))
	for _, record := range records {
		if isNormalizationPromotion(record) {
			result = append(result, record)
		}
	}
	return result, nil
}

func isNormalizationPromotion(record evaluation.PromotionRecord) bool {
	return record.Variant.ManagedBinding != nil &&
		record.Variant.ManagedBinding.Kind == evaluation.PromotionManagementNormalizationPolicy
}

func (service *Service) Prepare(
	ctx context.Context,
	request PrepareRequest,
	mutation evaluation.Mutation,
) (Preparation, error) {
	if err := request.Validate(); err != nil {
		return Preparation{}, err
	}
	if err := mutation.Validate(); err != nil {
		return Preparation{}, err
	}
	if !request.CreatedAt.Equal(mutation.At.UTC()) {
		return Preparation{}, fmt.Errorf("created_at must equal mutation time")
	}
	if !slices.Contains(mutation.Roles, evaluation.RolePromotionOperator) {
		return Preparation{}, fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	if service.config == nil {
		return Preparation{}, fmt.Errorf("config repository is required for normalization promotion preparation")
	}
	configFacts, err := service.deriveConfigPreparation(ctx, request)
	if err != nil {
		return Preparation{}, err
	}
	policyRef, err := service.runs.PutJSONArtifact(PolicyContract, request.Policy)
	if err != nil {
		return Preparation{}, err
	}
	configMutation := configrepo.Mutation{
		IdempotencyKey: derivedIdempotencyKey(mutation.IdempotencyKey, "config-create"),
		Actor:          mutation.Actor, Audit: mutation.Audit, At: mutation.At.UTC(),
	}
	if _, err := service.config.Create(ctx, configFacts.variantRevision, configMutation); err != nil {
		return Preparation{}, fmt.Errorf("create normalization variant config: %w", err)
	}
	configMutation.IdempotencyKey = derivedIdempotencyKey(mutation.IdempotencyKey, "config-validate")
	configRecord, err := service.config.ValidateRevision(
		ctx,
		configFacts.variantRevision.ID,
		configFacts.variantRevision.Revision,
		configMutation,
	)
	if err != nil {
		return Preparation{}, fmt.Errorf("validate normalization variant config: %w", err)
	}
	if configRecord.SHA256 != configFacts.variantRevisionSHA256 {
		return Preparation{}, fmt.Errorf("%w: validated normalization config digest changed", ErrEvidenceMismatch)
	}
	binding := evaluation.PromotionManagementBinding{
		Kind:                                evaluation.PromotionManagementNormalizationPolicy,
		BaselineBundleSHA256:                configFacts.baselineBundle.SHA256,
		VariantBundleSHA256:                 configFacts.variantBundle.SHA256,
		BaselineConfigRevisionID:            configFacts.baselineRecord.Revision.ID,
		BaselineConfigRevision:              configFacts.baselineRecord.Revision.Revision,
		BaselineConfigRevisionSHA256:        configFacts.baselineRecord.SHA256,
		ConfigRevisionID:                    configFacts.variantRevision.ID,
		ConfigRevision:                      configFacts.variantRevision.Revision,
		ConfigRevisionSHA256:                configFacts.variantRevisionSHA256,
		NormalizationPolicyRevision:         request.VariantPolicyRevision,
		NormalizationImplementationID:       request.VariantImplementation.ID,
		NormalizationImplementationRevision: request.VariantImplementation.Revision,
		NormalizationImplementationSHA256:   request.VariantImplementation.SHA256,
		NormalizationRollbackRevision:       request.RollbackImplementation.Revision,
		NormalizationRollbackSHA256:         request.RollbackImplementation.SHA256,
		NormalizationGatePolicyID:           request.Policy.PolicyID,
		NormalizationGatePolicyRevision:     request.Policy.Revision,
		NormalizationGatePolicyURI:          policyRef.URI,
		NormalizationGatePolicySizeBytes:    policyRef.SizeBytes,
		NormalizationGatePolicySHA256:       policyRef.SHA256,
	}
	variant := evaluation.PromotionVariant{
		SchemaVersion: evaluation.PromotionVariantSchemaVersion,
		VariantID:     request.PromotionVariantID, Component: evaluation.PromotionWorkflow,
		Revision: request.VariantPolicyRevision, RollbackRevision: request.RollbackPolicyRevision,
		PolicyRevision: request.PromotionPolicyRevision, Origin: evaluation.PromotionExperiment,
		Owner: request.Owner, CreatedAt: request.CreatedAt, ManagedBinding: &binding,
	}
	promotionMutation := mutation
	promotionMutation.IdempotencyKey = derivedIdempotencyKey(mutation.IdempotencyKey, "promotion-register")
	record, err := service.evaluation.RegisterManagedPromotion(ctx, variant, promotionMutation)
	if err != nil {
		return Preparation{}, err
	}
	schemaResult := evaluation.GateResult{
		SchemaVersion: evaluation.GateResultSchemaVersion, VariantID: variant.VariantID,
		Gate: evaluation.GateSchemaContract, Outcome: evaluation.GatePass,
		Evidence: evaluation.GateEvidence{
			Refs: []string{policyRef.URI}, Basis: evaluation.EvidenceDeterministic,
			ChecksPassed: true, HoldoutCaseIDs: []string{},
		},
		Summary: "normalization promotion policy, implementation and candidate config validated",
	}
	schemaMutation := mutation
	schemaMutation.IdempotencyKey = derivedIdempotencyKey(mutation.IdempotencyKey, "schema-gate")
	record, err = service.evaluation.RecordManagedGate(ctx, schemaResult, binding, schemaMutation)
	if err != nil {
		return Preparation{}, err
	}
	return Preparation{
		PolicyRef:            policyRef,
		BaselineBundleSHA256: configFacts.baselineBundle.SHA256,
		VariantBundleSHA256:  configFacts.variantBundle.SHA256,
		BaselineConfigSHA256: configFacts.baselineRecord.SHA256,
		VariantConfig:        configFacts.variantRevision,
		VariantConfigSHA256:  configFacts.variantRevisionSHA256,
		ConfigStatus:         configRecord.Status,
		Promotion:            record,
	}, nil
}

func (service *Service) RecordOperationalGate(
	ctx context.Context,
	request OperationalGateRequest,
	mutation evaluation.Mutation,
) (OperationalGateOutput, error) {
	if err := request.Validate(); err != nil {
		return OperationalGateOutput{}, err
	}
	if err := mutation.Validate(); err != nil {
		return OperationalGateOutput{}, err
	}
	if !request.EvaluatedAt.Equal(mutation.At.UTC()) {
		return OperationalGateOutput{}, fmt.Errorf("evaluated_at must equal mutation time")
	}
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	promotion, err := service.Get(request.VariantID, access)
	if err != nil {
		return OperationalGateOutput{}, err
	}
	if promotion.NextGate == nil || *promotion.NextGate != request.Gate {
		return OperationalGateOutput{}, fmt.Errorf("%w: promotion next gate does not match request", evaluation.ErrInvalidTransition)
	}
	binding := promotion.Variant.ManagedBinding
	if binding == nil {
		return OperationalGateOutput{}, fmt.Errorf("%w: normalization policy binding differs", ErrEvidenceMismatch)
	}
	policy, err := service.loadBoundPolicy(request.PolicyRef, *binding)
	if err != nil {
		return OperationalGateOutput{}, err
	}
	outcome := evaluation.GatePass
	checksPassed := true
	refs := []string{request.PolicyRef.URI}
	summary := "normalization operational gate passed"
	switch request.Gate {
	case evaluation.GateShadowTraffic:
		if uint32(len(request.ReviewRunIDs)) < policy.MinimumShadowRuns {
			outcome, checksPassed, summary = evaluation.GateInconclusive, false, "insufficient shadow runs"
		}
		runRefs, verifyErr := service.verifyOperationalRuns(ctx, request, *binding, false)
		if verifyErr != nil {
			return OperationalGateOutput{}, verifyErr
		}
		refs = append(refs, runRefs...)
	case evaluation.GateCanary:
		if uint32(len(request.ReviewRunIDs)) < policy.MinimumCanaryRuns {
			outcome, checksPassed, summary = evaluation.GateInconclusive, false, "insufficient canary runs"
		}
		runRefs, verifyErr := service.verifyOperationalRuns(ctx, request, *binding, true)
		if verifyErr != nil {
			return OperationalGateOutput{}, verifyErr
		}
		refs = append(refs, runRefs...)
	case evaluation.GateAuthorization:
		if request.Authorization == nil || request.Authorization.AuthorizedBy != mutation.Actor {
			return OperationalGateOutput{}, fmt.Errorf("%w: authorization actor must match mutation actor", ErrUnauthorized)
		}
	case evaluation.GateRollbackMonitor:
		if err := service.verifyRollbackFrame(*binding); err != nil {
			return OperationalGateOutput{}, err
		}
	default:
		return OperationalGateOutput{}, fmt.Errorf("unsupported operational gate %q", request.Gate)
	}
	if request.SafetyEventCount > 0 {
		outcome, checksPassed, summary = evaluation.GateFail, false, "normalization operational safety events observed"
	}
	requestRef, err := service.runs.PutJSONArtifact(OperationalGateRequestContract, request)
	if err != nil {
		return OperationalGateOutput{}, err
	}
	refs = append(refs, requestRef.URI)
	slices.Sort(refs)
	result := evaluation.GateResult{
		SchemaVersion: evaluation.GateResultSchemaVersion, VariantID: request.VariantID,
		Gate: request.Gate, Outcome: outcome,
		Evidence: evaluation.GateEvidence{
			Refs: refs, Basis: evaluation.EvidenceDeterministic, ChecksPassed: checksPassed,
			HoldoutCaseIDs: []string{}, SafetyEventCount: request.SafetyEventCount,
			Authorization:    request.Authorization,
			RollbackVerified: request.Gate == evaluation.GateRollbackMonitor && outcome == evaluation.GatePass,
		},
		Summary: summary,
	}
	if request.Gate == evaluation.GateAuthorization {
		result.Evidence.Basis = evaluation.EvidenceHumanCalibrated
	}
	record, err := service.evaluation.RecordManagedGate(ctx, result, *binding, mutation)
	if err != nil {
		return OperationalGateOutput{}, err
	}
	return OperationalGateOutput{Result: result, EvidenceRef: requestRef, Promotion: record}, nil
}

func (service *Service) StartCanary(
	ctx context.Context,
	variantID string,
	policyRef runmodel.ArtifactRef,
	rollout configrepo.Rollout,
	mutation evaluation.Mutation,
) (LifecycleOutput, error) {
	if err := mutation.Validate(); err != nil {
		return LifecycleOutput{}, err
	}
	if !slices.Contains(mutation.Roles, evaluation.RolePromotionOperator) {
		return LifecycleOutput{}, fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	promotion, err := service.Get(variantID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return LifecycleOutput{}, err
	}
	if promotion.NextGate == nil || *promotion.NextGate != evaluation.GateCanary {
		return LifecycleOutput{}, fmt.Errorf("%w: shadow gate must pass before canary publication", evaluation.ErrInvalidTransition)
	}
	binding := *promotion.Variant.ManagedBinding
	policy, err := service.loadBoundPolicy(policyRef, binding)
	if err != nil {
		return LifecycleOutput{}, err
	}
	if err := rollout.Validate(); err != nil || rollout.Percentage != policy.CanaryPercentage || rollout.Percentage >= 100 {
		return LifecycleOutput{}, fmt.Errorf("canary rollout must match policy percentage and use a valid stable seed")
	}
	record, err := service.config.Get(binding.ConfigRevisionID, binding.ConfigRevision)
	if err != nil || record.SHA256 != binding.ConfigRevisionSHA256 {
		return LifecycleOutput{}, fmt.Errorf("%w: normalization candidate config digest differs", ErrEvidenceMismatch)
	}
	if record.Status == configrepo.StatusPublished {
		_, current, rolloutErr := service.currentCandidateRollout(binding)
		if rolloutErr != nil || current == nil || *current != rollout {
			return LifecycleOutput{}, fmt.Errorf("%w: normalization candidate is published with a different rollout", ErrEvidenceMismatch)
		}
		return LifecycleOutput{Config: record, Rollout: current, Promotion: promotion}, nil
	}
	if record.Status != configrepo.StatusValidated {
		return LifecycleOutput{}, fmt.Errorf("%w: normalization candidate config is not exact validated revision", ErrEvidenceMismatch)
	}
	configMutation := configrepo.Mutation{
		IdempotencyKey: derivedIdempotencyKey(mutation.IdempotencyKey, "config-canary"),
		Actor:          mutation.Actor, Audit: mutation.Audit, At: mutation.At.UTC(),
	}
	record, err = service.config.Publish(ctx, record.Revision.ID, record.Revision.Revision, rollout, configMutation)
	if err != nil {
		return LifecycleOutput{}, err
	}
	return LifecycleOutput{Config: record, Rollout: &rollout, Promotion: promotion}, nil
}

func (service *Service) Activate(
	ctx context.Context,
	variantID string,
	mutation evaluation.Mutation,
) (LifecycleOutput, error) {
	if err := mutation.Validate(); err != nil {
		return LifecycleOutput{}, err
	}
	if !slices.Contains(mutation.Roles, evaluation.RolePromotionOperator) {
		return LifecycleOutput{}, fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	promotion, err := service.Get(variantID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return LifecycleOutput{}, err
	}
	if promotion.Status != evaluation.PromotionActive {
		return LifecycleOutput{}, fmt.Errorf("%w: all managed gates must pass before activation", evaluation.ErrInvalidTransition)
	}
	binding := *promotion.Variant.ManagedBinding
	record, rollout, err := service.currentCandidateRollout(binding)
	if err != nil || rollout == nil {
		return LifecycleOutput{}, fmt.Errorf("%w: candidate is not in exact canary rollout", ErrEvidenceMismatch)
	}
	if rollout.Percentage == 100 {
		return LifecycleOutput{Config: record, Rollout: rollout, Promotion: promotion}, nil
	}
	if rollout.Percentage > 100 {
		return LifecycleOutput{}, fmt.Errorf("%w: candidate rollout percentage is invalid", ErrEvidenceMismatch)
	}
	full := configrepo.Rollout{Percentage: 100, Seed: rollout.Seed}
	configMutation := configrepo.Mutation{
		IdempotencyKey: derivedIdempotencyKey(mutation.IdempotencyKey, "config-activate"),
		Actor:          mutation.Actor, Audit: mutation.Audit, At: mutation.At.UTC(),
	}
	record, err = service.config.AdvanceRollout(ctx, record.Revision.ID, record.Revision.Revision, full, configMutation)
	if err != nil {
		return LifecycleOutput{}, err
	}
	return LifecycleOutput{Config: record, Rollout: &full, Promotion: promotion}, nil
}

func (service *Service) Rollback(
	ctx context.Context,
	variantID string,
	mutation evaluation.Mutation,
) (LifecycleOutput, error) {
	if err := mutation.Validate(); err != nil {
		return LifecycleOutput{}, err
	}
	if !slices.Contains(mutation.Roles, evaluation.RolePromotionOperator) {
		return LifecycleOutput{}, fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	promotion, err := service.Get(variantID, evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles})
	if err != nil {
		return LifecycleOutput{}, err
	}
	binding := *promotion.Variant.ManagedBinding
	record, err := service.config.Get(binding.ConfigRevisionID, binding.ConfigRevision)
	if err != nil {
		return LifecycleOutput{}, err
	}
	if record.SHA256 != binding.ConfigRevisionSHA256 {
		return LifecycleOutput{}, fmt.Errorf("%w: candidate config digest drifted", ErrEvidenceMismatch)
	}
	if record.Status == configrepo.StatusPublished {
		configMutation := configrepo.Mutation{
			IdempotencyKey: derivedIdempotencyKey(mutation.IdempotencyKey, "config-rollback"),
			Actor:          mutation.Actor, Audit: mutation.Audit, At: mutation.At.UTC(),
		}
		record, err = service.config.Rollback(ctx, record.Revision.ID, record.Revision.Revision, configMutation)
		if err != nil {
			return LifecycleOutput{}, err
		}
	}
	if promotion.Status == evaluation.PromotionActive {
		promotionMutation := mutation
		promotionMutation.IdempotencyKey = derivedIdempotencyKey(mutation.IdempotencyKey, "promotion-rollback")
		promotion, err = service.evaluation.RollbackManagedPromotion(ctx, variantID, binding, promotionMutation)
		if err != nil {
			return LifecycleOutput{}, err
		}
	}
	return LifecycleOutput{Config: record, Promotion: promotion}, nil
}

type configPreparation struct {
	baselineBundle        reviewconfig.ConfigBundle
	variantBundle         reviewconfig.ConfigBundle
	baselineRecord        configrepo.Record
	variantRevision       reviewconfig.Revision
	variantRevisionSHA256 string
}

func (service *Service) deriveConfigPreparation(
	ctx context.Context,
	request PrepareRequest,
) (configPreparation, error) {
	baselineRun, err := service.runs.LoadRun(request.BaselineReviewRunID)
	if err != nil {
		return configPreparation{}, fmt.Errorf("load normalization baseline ReviewRun: %w", err)
	}
	if baselineRun.Status != runmodel.RunStatusSucceeded {
		return configPreparation{}, fmt.Errorf("normalization baseline ReviewRun must be succeeded")
	}
	snapshot, err := service.runs.LoadExecutionSnapshot(baselineRun.ExecutionSnapshotID)
	if err != nil {
		return configPreparation{}, fmt.Errorf("load normalization baseline ExecutionSnapshot: %w", err)
	}
	bundleData, err := service.runs.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return configPreparation{}, fmt.Errorf("read normalization baseline ConfigBundle: %w", err)
	}
	baselineBundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		return configPreparation{}, fmt.Errorf("decode normalization baseline ConfigBundle: %w", err)
	}
	if baselineBundle.AgentReview == nil {
		return configPreparation{}, fmt.Errorf("normalization baseline config has no agent_review policy")
	}
	if !sameConfigRef(baselineBundle.AgentReview.Normalization, request.RollbackImplementation) ||
		baselineBundle.AgentReview.Agent.SHA256 != request.RollbackImplementation.SHA256 {
		return configPreparation{}, fmt.Errorf(
			"%w: baseline config does not use exact rollback normalization implementation",
			ErrEvidenceMismatch,
		)
	}
	currentBundle, receipt, err := service.config.ResolvePublishedWithReceipt(ctx, baselineBundle.Context)
	if err != nil {
		return configPreparation{}, fmt.Errorf("resolve current normalization baseline config: %w", err)
	}
	if currentBundle.SHA256 != baselineBundle.SHA256 ||
		snapshot.ConfigBundleRef.SHA256 != artifactDigest(bundleData) {
		return configPreparation{}, fmt.Errorf(
			"%w: baseline ReviewRun config is not the current exact lifecycle resolution",
			ErrEvidenceMismatch,
		)
	}
	baselineRecord, err := service.config.Get(
		request.BaselineConfigRevisionID,
		request.BaselineConfigRevision,
	)
	if err != nil {
		return configPreparation{}, err
	}
	if baselineRecord.Status != configrepo.StatusPublished ||
		!receiptContains(receipt, baselineRecord) ||
		!fieldOwnedBy(
			baselineBundle,
			"agent_review.normalization",
			baselineRecord.Revision.ID,
			baselineRecord.Revision.Revision,
		) {
		return configPreparation{}, fmt.Errorf(
			"%w: baseline config revision is not the active normalization field owner",
			ErrEvidenceMismatch,
		)
	}
	variantRevision, err := cloneConfigRevision(baselineRecord.Revision)
	if err != nil {
		return configPreparation{}, err
	}
	variantRevision.ID = request.VariantConfigRevisionID
	variantRevision.Revision = request.VariantConfigRevision
	if variantRevision.Patch.AgentReview == nil {
		return configPreparation{}, fmt.Errorf(
			"%w: normalization field owner has no agent_review patch",
			ErrEvidenceMismatch,
		)
	}
	variantRevision.Patch.AgentReview.Normalization = &reviewconfig.VersionedRef{
		ID:       request.VariantImplementation.ID,
		Revision: request.VariantImplementation.Revision,
		SHA256:   request.VariantImplementation.SHA256,
	}
	variantRevisionSHA256, err := reviewconfig.DigestRevision(variantRevision)
	if err != nil {
		return configPreparation{}, err
	}
	revisions := make([]reviewconfig.Revision, 0, len(receipt.Revisions))
	for _, published := range receipt.Revisions {
		record, loadErr := service.config.Get(
			published.Source.ID,
			published.Source.Revision,
		)
		if loadErr != nil {
			return configPreparation{}, loadErr
		}
		if record.SHA256 != published.RevisionSHA256 {
			return configPreparation{}, fmt.Errorf(
				"%w: baseline config receipt revision drifted",
				ErrEvidenceMismatch,
			)
		}
		if record.Revision.ID == baselineRecord.Revision.ID &&
			record.Revision.Revision == baselineRecord.Revision.Revision {
			revisions = append(revisions, variantRevision)
		} else {
			revisions = append(revisions, record.Revision)
		}
	}
	variantBundle, err := reviewconfig.Resolve(baselineBundle.Context, revisions)
	if err != nil {
		return configPreparation{}, fmt.Errorf("resolve normalization variant config: %w", err)
	}
	if variantBundle.AgentReview == nil ||
		!sameConfigRef(variantBundle.AgentReview.Normalization, request.VariantImplementation) ||
		variantBundle.AgentReview.Agent.SHA256 != request.VariantImplementation.SHA256 {
		return configPreparation{}, fmt.Errorf(
			"%w: variant config does not resolve exact normalization implementation",
			ErrEvidenceMismatch,
		)
	}
	return configPreparation{
		baselineBundle:        baselineBundle,
		variantBundle:         variantBundle,
		baselineRecord:        baselineRecord,
		variantRevision:       variantRevision,
		variantRevisionSHA256: variantRevisionSHA256,
	}, nil
}

func (service *Service) RecordQualityGate(
	ctx context.Context,
	request GateRequest,
	mutation evaluation.Mutation,
) (GateOutput, error) {
	if err := request.Validate(); err != nil {
		return GateOutput{}, err
	}
	if err := mutation.Validate(); err != nil {
		return GateOutput{}, err
	}
	if !request.EvaluatedAt.Equal(mutation.At.UTC()) {
		return GateOutput{}, fmt.Errorf("evaluated_at must equal mutation time")
	}
	access := evaluation.Access{Actor: mutation.Actor, Roles: slices.Clone(mutation.Roles)}
	promotion, err := service.evaluation.GetPromotion(request.VariantID, access)
	if err != nil {
		return GateOutput{}, err
	}
	binding := promotion.Variant.ManagedBinding
	if binding == nil || binding.Kind != evaluation.PromotionManagementNormalizationPolicy ||
		binding.NormalizationGatePolicySHA256 != request.PolicyRef.SHA256 {
		return GateOutput{}, fmt.Errorf("%w: normalization managed binding or policy ref differs", ErrEvidenceMismatch)
	}
	if promotion.NextGate == nil || *promotion.NextGate != request.Gate {
		return GateOutput{}, fmt.Errorf("%w: promotion next gate does not match request", evaluation.ErrInvalidTransition)
	}
	policy, err := service.loadBoundPolicy(request.PolicyRef, *binding)
	if err != nil {
		return GateOutput{}, err
	}
	if !slices.Contains(mutation.Roles, evaluation.RolePromotionOperator) {
		return GateOutput{}, fmt.Errorf("%w: promotion_operator role is required", ErrUnauthorized)
	}
	requiredSplit, thresholds := evaluation.SplitTest, policy.Test
	if request.Gate == evaluation.GateTargetedRegression {
		if !slices.Contains(mutation.Roles, evaluation.RoleDatasetCurator) {
			return GateOutput{}, fmt.Errorf("%w: dataset_curator role is required for targeted regression", ErrUnauthorized)
		}
	} else {
		requiredSplit, thresholds = evaluation.SplitHoldout, policy.Holdout
		if !slices.Contains(mutation.Roles, evaluation.RoleHoldoutRunner) {
			return GateOutput{}, fmt.Errorf("%w: holdout_runner role is required for fixed holdout", ErrUnauthorized)
		}
		if mutation.Actor == promotion.Variant.Owner {
			return GateOutput{}, fmt.Errorf("%w: holdout runner must be independent from promotion owner", ErrUnauthorized)
		}
	}
	quality, err := service.quality.VerifyQuality(ctx, request.QualityRunRef, access)
	if err != nil {
		return GateOutput{}, err
	}
	if quality.PolicyRevision != binding.NormalizationPolicyRevision || quality.CreatedAt.After(request.EvaluatedAt) {
		return GateOutput{}, fmt.Errorf("%w: quality run does not bind promoted policy and time", ErrEvidenceMismatch)
	}
	caseIDs := make([]string, 0, len(quality.Cases))
	for _, item := range quality.Cases {
		if item.Split != requiredSplit {
			return GateOutput{}, fmt.Errorf("%w: quality run contains split %q, want %q", ErrEvidenceMismatch, item.Split, requiredSplit)
		}
		caseIDs = append(caseIDs, item.CaseID)
	}
	if quality.Summary.PolicyExposedCases != 0 || quality.Summary.IndependentEvidenceCases != quality.Summary.Cases {
		return GateOutput{}, fmt.Errorf("%w: normalization quality evidence is policy-exposed or not independent", evaluation.ErrContaminated)
	}
	decision := evaluateGateDecision(request, quality, requiredSplit, thresholds)
	decisionRef, err := service.runs.PutJSONArtifact(DecisionContract, decision)
	if err != nil {
		return GateOutput{}, err
	}
	refs := []string{request.PolicyRef.URI, request.QualityRunRef.URI, decisionRef.URI}
	slices.Sort(refs)
	evidence := evaluation.GateEvidence{
		Refs: refs, Basis: evaluation.EvidenceHumanCalibrated,
		ChecksPassed:              decision.Outcome == evaluation.GatePass,
		NormalizationQualityRunID: quality.QualityRunID,
		HoldoutCaseIDs:            []string{},
	}
	if request.Gate == evaluation.GateFixedHoldout {
		evidence.HoldoutCaseIDs = caseIDs
	}
	result := evaluation.GateResult{
		SchemaVersion: evaluation.GateResultSchemaVersion, VariantID: request.VariantID,
		Gate: request.Gate, Outcome: decision.Outcome, Evidence: evidence,
		Summary: "normalization quality gate: " + decision.ReasonCodes[0],
	}
	record, err := service.evaluation.RecordManagedGate(ctx, result, *binding, mutation)
	if err != nil {
		return GateOutput{}, err
	}
	return GateOutput{Decision: decision, DecisionRef: decisionRef, Promotion: record}, nil
}

func evaluateGateDecision(
	request GateRequest,
	quality evaluation.NormalizationQualityRun,
	split evaluation.Split,
	thresholds Thresholds,
) GateDecision {
	summary := quality.Summary
	reasons := make([]string, 0, 8)
	outcome := evaluation.GatePass
	oracleDuplicatePairs := summary.TrueDuplicatePairs + summary.MissedDuplicatePairs
	oracleDistinctPairs := summary.FalseDuplicatePairs + summary.TrueDistinctPairs
	if summary.Cases < thresholds.MinimumCases ||
		summary.EligibleRawCandidates < thresholds.MinimumEligibleCandidates ||
		oracleDuplicatePairs < thresholds.MinimumOracleDuplicatePairs ||
		oracleDistinctPairs < thresholds.MinimumOracleDistinctPairs {
		outcome = evaluation.GateInconclusive
		reasons = append(reasons, "insufficient_sample")
	}
	if !summary.PairwisePrecision.Available || !summary.PairwiseRecall.Available || !summary.FalseMergeRate.Available {
		outcome = evaluation.GateInconclusive
		reasons = append(reasons, "required_metric_unavailable")
	}
	exactPPM := uint32((uint64(summary.ExactPartitionMatches)*1_000_000 + uint64(summary.Cases)/2) / uint64(summary.Cases))
	if outcome != evaluation.GateInconclusive &&
		(summary.PairwisePrecision.ValuePPM < thresholds.MinimumPairwisePrecisionPPM ||
			summary.PairwiseRecall.ValuePPM < thresholds.MinimumPairwiseRecallPPM ||
			summary.FalseMergeRate.ValuePPM > thresholds.MaximumFalseMergeRatePPM ||
			exactPPM < thresholds.MinimumExactPartitionPPM) {
		outcome = evaluation.GateFail
		reasons = append(reasons, "quality_threshold_failed")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "quality_thresholds_passed")
	}
	return GateDecision{
		SchemaVersion: DecisionSchemaVersion, VariantID: request.VariantID, Gate: request.Gate,
		PolicyRef: request.PolicyRef, QualityRunRef: request.QualityRunRef,
		QualityRunID: quality.QualityRunID, Split: split, Outcome: outcome,
		ReasonCodes: sortedReasons(reasons), Measured: summary, Thresholds: thresholds,
		EvaluatedAt: request.EvaluatedAt,
	}
}

func (preparation Preparation) Validate() error {
	if err := preparation.PolicyRef.Validate(); err != nil || preparation.PolicyRef.Contract != PolicyContract {
		return fmt.Errorf("preparation policy ref is invalid")
	}
	if preparation.Promotion.Variant.ManagedBinding == nil ||
		preparation.Promotion.Variant.ManagedBinding.NormalizationGatePolicySHA256 != preparation.PolicyRef.SHA256 ||
		preparation.Promotion.Variant.ManagedBinding.NormalizationGatePolicyURI != preparation.PolicyRef.URI ||
		preparation.Promotion.Variant.ManagedBinding.NormalizationGatePolicySizeBytes != preparation.PolicyRef.SizeBytes {
		return fmt.Errorf("preparation promotion does not bind policy")
	}
	binding := preparation.Promotion.Variant.ManagedBinding
	if preparation.ConfigStatus != configrepo.StatusValidated ||
		preparation.BaselineBundleSHA256 != binding.BaselineBundleSHA256 ||
		preparation.BaselineConfigSHA256 != binding.BaselineConfigRevisionSHA256 ||
		preparation.VariantConfig.ID != binding.ConfigRevisionID ||
		preparation.VariantConfig.Revision != binding.ConfigRevision ||
		preparation.VariantConfigSHA256 != binding.ConfigRevisionSHA256 {
		return fmt.Errorf("preparation config projection does not match managed binding")
	}
	digest, err := reviewconfig.DigestRevision(preparation.VariantConfig)
	if err != nil || digest != preparation.VariantConfigSHA256 {
		return fmt.Errorf("preparation variant config digest mismatch")
	}
	if len(preparation.VariantBundleSHA256) != sha256.Size*2 {
		return fmt.Errorf("preparation variant bundle digest is invalid")
	}
	return nil
}

func (output GateOutput) Validate() error {
	if err := output.Decision.Validate(); err != nil {
		return err
	}
	if err := output.DecisionRef.Validate(); err != nil || output.DecisionRef.Contract != DecisionContract {
		return fmt.Errorf("decision ref is invalid")
	}
	if output.DecisionRef.SHA256 == "" || output.Promotion.Variant.VariantID != output.Decision.VariantID {
		return fmt.Errorf("gate output identity mismatch")
	}
	return nil
}

func (output OperationalGateOutput) Validate() error {
	if err := output.Result.Validate(); err != nil {
		return err
	}
	if err := output.EvidenceRef.Validate(); err != nil || output.EvidenceRef.Contract != OperationalGateRequestContract {
		return fmt.Errorf("operational gate evidence ref is invalid")
	}
	if output.Promotion.Variant.VariantID != output.Result.VariantID {
		return fmt.Errorf("operational gate output identity mismatch")
	}
	return nil
}

func sameConfigRef(
	actual reviewconfig.VersionedRef,
	expected contractsv1alpha1.VersionedRef,
) bool {
	return actual.ID == expected.ID &&
		actual.Revision == expected.Revision &&
		actual.SHA256 == expected.SHA256
}

func cloneConfigRevision(revision reviewconfig.Revision) (reviewconfig.Revision, error) {
	data, err := json.Marshal(revision)
	if err != nil {
		return reviewconfig.Revision{}, err
	}
	var clone reviewconfig.Revision
	if err := json.Unmarshal(data, &clone); err != nil {
		return reviewconfig.Revision{}, err
	}
	return clone, nil
}

func receiptContains(
	receipt reviewconfig.ConfigResolutionReceipt,
	record configrepo.Record,
) bool {
	for _, binding := range receipt.Revisions {
		if binding.Source.ID == record.Revision.ID &&
			binding.Source.Revision == record.Revision.Revision &&
			binding.RevisionSHA256 == record.SHA256 {
			return true
		}
	}
	return false
}

func fieldOwnedBy(
	bundle reviewconfig.ConfigBundle,
	field string,
	id string,
	revision string,
) bool {
	for _, source := range bundle.FieldSources {
		if source.Field != field || len(source.Sources) == 0 {
			continue
		}
		owner := source.Sources[len(source.Sources)-1]
		return owner.ID == id && owner.Revision == revision
	}
	return false
}

func artifactDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func derivedIdempotencyKey(parent string, operation string) string {
	candidate := parent + "-" + operation
	if len(candidate) <= 256 {
		return candidate
	}
	digest := sha256.Sum256([]byte(parent))
	return "normalization-promotion-" + hex.EncodeToString(digest[:]) + "-" + operation
}

func (service *Service) loadBoundPolicy(
	ref runmodel.ArtifactRef,
	binding evaluation.PromotionManagementBinding,
) (Policy, error) {
	if err := ref.Validate(); err != nil || ref.Contract != PolicyContract ||
		ref.SHA256 != binding.NormalizationGatePolicySHA256 ||
		ref.URI != binding.NormalizationGatePolicyURI ||
		ref.SizeBytes != binding.NormalizationGatePolicySizeBytes {
		return Policy{}, fmt.Errorf("%w: normalization policy ref differs", ErrEvidenceMismatch)
	}
	data, err := service.runs.ReadArtifact(ref)
	if err != nil {
		return Policy{}, err
	}
	policy, err := DecodePolicy(data)
	if err != nil {
		return Policy{}, err
	}
	if policy.PolicyID != binding.NormalizationGatePolicyID ||
		policy.Revision != binding.NormalizationGatePolicyRevision {
		return Policy{}, fmt.Errorf("%w: normalization policy identity differs", ErrEvidenceMismatch)
	}
	return policy, nil
}

func (service *Service) currentCandidateRollout(
	binding evaluation.PromotionManagementBinding,
) (configrepo.Record, *configrepo.Rollout, error) {
	record, err := service.config.Get(binding.ConfigRevisionID, binding.ConfigRevision)
	if err != nil {
		return configrepo.Record{}, nil, err
	}
	if record.SHA256 != binding.ConfigRevisionSHA256 || record.Status != configrepo.StatusPublished {
		return configrepo.Record{}, nil, fmt.Errorf("%w: candidate config is not exact published revision", ErrEvidenceMismatch)
	}
	history, err := service.config.History(binding.ConfigRevisionID, binding.ConfigRevision)
	if err != nil {
		return configrepo.Record{}, nil, err
	}
	for index := len(history) - 1; index >= 0; index-- {
		entry := history[index]
		if (entry.Type == configrepo.EventPublished || entry.Type == configrepo.EventRolloutAdvanced) && entry.Rollout != nil {
			rollout := *entry.Rollout
			return record, &rollout, nil
		}
	}
	return configrepo.Record{}, nil, fmt.Errorf("%w: candidate config has no rollout evidence", ErrEvidenceMismatch)
}

func (service *Service) verifyRollbackFrame(binding evaluation.PromotionManagementBinding) error {
	baseline, err := service.config.Get(binding.BaselineConfigRevisionID, binding.BaselineConfigRevision)
	if err != nil || baseline.SHA256 != binding.BaselineConfigRevisionSHA256 || baseline.Status != configrepo.StatusPublished {
		return fmt.Errorf("%w: exact rollback baseline is not published", ErrEvidenceMismatch)
	}
	_, rollout, err := service.currentCandidateRollout(binding)
	if err != nil || rollout == nil || rollout.Percentage >= 100 {
		return fmt.Errorf("%w: candidate canary rollback frame is not recoverable", ErrEvidenceMismatch)
	}
	return nil
}

func (service *Service) verifyOperationalRuns(
	ctx context.Context,
	request OperationalGateRequest,
	binding evaluation.PromotionManagementBinding,
	requireLifecycleCanary bool,
) ([]string, error) {
	refs := make([]string, 0, len(request.ReviewRunIDs))
	for _, runID := range request.ReviewRunIDs {
		run, err := service.runs.LoadRun(runID)
		if err != nil {
			return nil, err
		}
		if run.Status != runmodel.RunStatusSucceeded || run.CompletedAt == nil ||
			run.CompletedAt.After(request.EvaluatedAt) {
			return nil, fmt.Errorf("%w: operational run %q is not a completed succeeded run", ErrEvidenceMismatch, runID)
		}
		snapshot, err := service.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
		if err != nil {
			return nil, err
		}
		data, err := service.runs.ReadArtifact(snapshot.ConfigBundleRef)
		if err != nil {
			return nil, err
		}
		bundle, err := reviewconfig.DecodeBundle(data)
		if err != nil {
			return nil, err
		}
		if bundle.SHA256 != binding.VariantBundleSHA256 || snapshot.ConfigBundleRef.SHA256 != artifactDigest(data) {
			return nil, fmt.Errorf("%w: operational run %q does not bind exact variant bundle", ErrEvidenceMismatch, runID)
		}
		if requireLifecycleCanary {
			candidate, rollout, rolloutErr := service.currentCandidateRollout(binding)
			if rolloutErr != nil || rollout == nil || rollout.Percentage >= 100 || run.CompletedAt.Before(candidate.UpdatedAt) {
				return nil, fmt.Errorf("%w: run %q predates or does not bind candidate canary", ErrEvidenceMismatch, runID)
			}
			current, receipt, resolveErr := service.config.ResolvePublishedWithReceipt(ctx, bundle.Context)
			if resolveErr != nil || current.SHA256 != bundle.SHA256 || !receiptContains(receipt, candidate) {
				return nil, fmt.Errorf("%w: run %q context is not assigned to candidate canary", ErrEvidenceMismatch, runID)
			}
		}
		refs = append(refs, snapshot.ConfigBundleRef.URI)
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
}
