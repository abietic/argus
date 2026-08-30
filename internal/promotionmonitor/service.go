package promotionmonitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/analyticsadapter"
	"github.com/abietic/argus/internal/calibrationpromotion"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/store/local"
)

const manifestRoot = "analytics/promotion-observation-manifests/"

type PlanSource interface {
	Get(string, evaluation.Access) (calibrationpromotion.Plan, error)
}

type SnapshotSource interface {
	Query(string) (analyticsadapter.ProjectionSnapshot, error)
}

type Service struct {
	mu        sync.Mutex
	store     *local.Store
	plans     PlanSource
	snapshots SnapshotSource
}

func New(store *local.Store, plans PlanSource, snapshots SnapshotSource) (*Service, error) {
	if store == nil || plans == nil || snapshots == nil {
		return nil, fmt.Errorf("store, promotion plan source, and snapshot source are required")
	}
	return &Service{store: store, plans: plans, snapshots: snapshots}, nil
}

func Open(store *local.Store) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	return &Service{store: store}, nil
}

func (service *Service) Build(ctx context.Context, request BuildRequest, mutation evaluation.Mutation) (Observation, error) {
	if err := request.Validate(); err != nil {
		return Observation{}, err
	}
	if err := authorizeWrite(mutation); err != nil {
		return Observation{}, err
	}
	if !request.ObservedAt.Equal(mutation.At.UTC()) {
		return Observation{}, fmt.Errorf("observed_at must equal mutation at")
	}
	if err := contextErr(ctx); err != nil {
		return Observation{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()

	if existing, err := service.get(request.ObservationID); err == nil {
		if reflect.DeepEqual(existing.Request, request) && existing.ObservedBy == mutation.Actor && existing.Audit == mutation.Audit && existing.IdempotencyKey == mutation.IdempotencyKey {
			return existing, nil
		}
		return Observation{}, fmt.Errorf("%w: observation id already binds different evidence", ErrConflict)
	} else if !errors.Is(err, ErrNotFound) {
		return Observation{}, err
	}

	access := evaluation.Access{Actor: mutation.Actor, Roles: mutation.Roles}
	plan, err := service.plans.Get(request.PlanID, access)
	if err != nil {
		return Observation{}, err
	}
	if err := plan.Validate(); err != nil {
		return Observation{}, fmt.Errorf("%w: invalid promotion plan: %v", ErrEvidenceMismatch, err)
	}
	if plan.Status != calibrationpromotion.StatusActive || plan.Rollout == nil {
		return Observation{}, fmt.Errorf("%w: promotion plan must be active", ErrEvidenceMismatch)
	}
	planDigest, err := digestJSON(plan)
	if err != nil {
		return Observation{}, err
	}
	baseline, err := service.snapshots.Query(request.BaselineSnapshotID)
	if err != nil {
		return Observation{}, fmt.Errorf("load baseline snapshot: %w", err)
	}
	observation, err := service.snapshots.Query(request.ObservationSnapshotID)
	if err != nil {
		return Observation{}, fmt.Errorf("load observation snapshot: %w", err)
	}
	if err := baseline.Validate(); err != nil {
		return Observation{}, fmt.Errorf("%w: invalid baseline snapshot: %v", ErrEvidenceMismatch, err)
	}
	if err := observation.Validate(); err != nil {
		return Observation{}, fmt.Errorf("%w: invalid observation snapshot: %v", ErrEvidenceMismatch, err)
	}
	baselineDigest, err := digestJSON(baseline)
	if err != nil {
		return Observation{}, err
	}
	observationDigest, err := digestJSON(observation)
	if err != nil {
		return Observation{}, err
	}

	result, err := buildObservation(request, mutation, plan, planDigest, baseline, baselineDigest, observation, observationDigest)
	if err != nil {
		return Observation{}, err
	}
	if err := contextErr(ctx); err != nil {
		return Observation{}, err
	}
	current, err := service.plans.Get(request.PlanID, access)
	if err != nil {
		return Observation{}, err
	}
	currentDigest, err := digestJSON(current)
	if err != nil || currentDigest != planDigest {
		return Observation{}, fmt.Errorf("%w: promotion plan changed while observation was built", ErrEvidenceMismatch)
	}
	return service.persist(result)
}

func (service *Service) Get(observationID string, access evaluation.Access) (Observation, error) {
	if err := authorizeRead(access); err != nil {
		return Observation{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.get(observationID)
}

func (service *Service) List(access evaluation.Access) ([]Summary, error) {
	if err := authorizeRead(access); err != nil {
		return nil, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	manifests, err := service.loadManifests()
	if err != nil {
		return nil, err
	}
	result := make([]Summary, 0, len(manifests))
	for _, item := range manifests {
		summary := Summary{ObservationID: item.ObservationID, PlanID: item.PlanID, Status: item.Status, RollbackRecommendation: item.RollbackRecommendation, ObservationSHA256: item.ObservationSHA256, ObservedAt: item.ObservedAt}
		if err := summary.Validate(); err != nil {
			return nil, fmt.Errorf("%w: invalid summary: %v", ErrCorrupt, err)
		}
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ObservationID < result[j].ObservationID })
	return result, nil
}

func buildObservation(request BuildRequest, mutation evaluation.Mutation, plan calibrationpromotion.Plan, planDigest string, baseline analyticsadapter.ProjectionSnapshot, baselineDigest string, current analyticsadapter.ProjectionSnapshot, currentDigest string) (Observation, error) {
	if baseline.Scope != current.Scope || !slices.Equal(baseline.GroupBy, current.GroupBy) {
		return Observation{}, fmt.Errorf("%w: snapshots must use identical scope and group_by", ErrEvidenceMismatch)
	}
	if baseline.Window.EndExclusive.After(plan.UpdatedAt) || current.Window.StartInclusive.Before(plan.UpdatedAt) {
		return Observation{}, fmt.Errorf("%w: snapshots must lie before and after activation", ErrEvidenceMismatch)
	}
	baselineRuns := selectedRunIDs(baseline, plan.BaselineBundleSHA256)
	if _, ok := baselineRuns[plan.Request.BaselineReviewRunID]; !ok {
		return Observation{}, fmt.Errorf("%w: baseline snapshot does not contain the bound baseline ReviewRun", ErrEvidenceMismatch)
	}
	variantRuns := selectedRunIDs(current, plan.VariantBundleSHA256)

	unavailableReasons := snapshotUnavailableReasons(baseline, current)
	rules := map[MetricID]MetricRule{}
	for _, rule := range request.Policy.Rules {
		rules[rule.MetricID] = rule
	}
	baseCounts := deriveCounts(baseline, baselineRuns)
	currentCounts := deriveCounts(current, variantRuns)
	metrics := make([]MetricComparison, 0, len(metricDefinitions))
	metricIDs := make([]MetricID, 0, len(metricDefinitions))
	for id := range metricDefinitions {
		metricIDs = append(metricIDs, id)
	}
	slices.Sort(metricIDs)
	for _, id := range metricIDs {
		base, observed := metricValues(id, baseCounts), metricValues(id, currentCounts)
		metrics = append(metrics, compareMetric(id, base, observed, rules[id], unavailableReasons))
	}
	status, reasons := summarize(metrics)
	result := Observation{
		SchemaVersion: ObservationSchemaVersion, Request: request,
		Plan:             PlanBinding{PlanID: plan.Request.PlanID, PlanSHA256: planDigest, ActivationAt: plan.UpdatedAt, CalibrationRunID: plan.Request.CalibrationRunID, CalibrationRunSHA256: plan.CalibrationRunSHA256, BaselineReviewRunID: plan.Request.BaselineReviewRunID, BaselineBundleSHA256: plan.BaselineBundleSHA256, VariantBundleSHA256: plan.VariantBundleSHA256, VariantConfigRevisionID: plan.VariantConfig.ID, VariantConfigRevision: plan.VariantConfig.Revision, VariantConfigRevisionSHA256: plan.VariantConfigSHA256, PromotionVariantID: plan.PromotionVariant.VariantID, PromotionPolicyRevision: plan.Request.PromotionPolicyRevision},
		BaselineSnapshot: snapshotBinding(baseline, baselineDigest), ObservationSnapshot: snapshotBinding(current, currentDigest), Metrics: metrics, Status: status, RollbackRecommendation: status == StatusRegressed, ReasonCodes: reasons, ObservedBy: mutation.Actor, Audit: mutation.Audit, IdempotencyKey: mutation.IdempotencyKey,
	}
	if err := result.Validate(); err != nil {
		return Observation{}, fmt.Errorf("validate built observation: %w", err)
	}
	return result, nil
}

type counts struct{ runs, succeeded, complete, published, knownOutcomes, fixed, adverse uint64 }

func deriveCounts(snapshot analyticsadapter.ProjectionSnapshot, selected map[string]struct{}) counts {
	var result counts
	for _, fact := range snapshot.Facts.ReviewRuns {
		if _, ok := selected[fact.RunID]; ok {
			result.runs++
			if fact.Status == analytics.RunStatusSucceeded {
				result.succeeded++
			}
			if fact.ResultCompleteness == analytics.CompletenessComplete {
				result.complete++
			}
		}
	}
	published := map[string]struct{}{}
	for _, fact := range snapshot.Facts.Findings {
		if _, ok := selected[fact.RunID]; ok && fact.Publication == analytics.PublicationPublished {
			published[fact.RunID+"\x00"+fact.FindingID] = struct{}{}
		}
	}
	result.published = uint64(len(published))
	for _, fact := range snapshot.Facts.FeedbackOutcomes {
		if _, ok := published[fact.RunID+"\x00"+fact.FindingID]; !ok {
			continue
		}
		switch fact.Outcome {
		case analytics.OutcomeFixed:
			result.knownOutcomes++
			result.fixed++
		case analytics.OutcomeRecurred, analytics.OutcomeEscaped:
			result.knownOutcomes++
			result.adverse++
		}
	}
	return result
}

func metricValues(id MetricID, value counts) MetricValue {
	switch id {
	case MetricRunSuccessRate:
		return rateValue(value.succeeded, value.runs)
	case MetricRunCompleteRate:
		return rateValue(value.complete, value.runs)
	case MetricPublishedOutcomeCoverage:
		return rateValue(value.knownOutcomes, value.published)
	case MetricPublishedFixedRate:
		return rateValue(value.fixed, value.knownOutcomes)
	case MetricPublishedAdverseRate:
		return rateValue(value.adverse, value.knownOutcomes)
	default:
		panic("unsupported metric")
	}
}

func rateValue(numerator, denominator uint64) MetricValue {
	value := MetricValue{Numerator: numerator, Denominator: denominator}
	if denominator > 0 {
		hi, lo := bits.Mul64(numerator, RateScalePPM)
		quotient, _ := bits.Div64(hi, lo, denominator)
		value.RatePPM = &quotient
	}
	return value
}

func compareMetric(id MetricID, baseline, observation MetricValue, rule MetricRule, unavailable []string) MetricComparison {
	definition := metricDefinitions[id]
	result := MetricComparison{MetricID: id, Definition: definition.definition, Direction: definition.direction, Baseline: baseline, Observation: observation, ReasonCodes: []string{}}
	if len(unavailable) > 0 {
		result.Availability = MetricUnavailable
		result.ReasonCodes = slices.Clone(unavailable)
		return result
	}
	reasons := []string{}
	if baseline.Denominator < rule.MinimumBaselineSampleSize {
		reasons = append(reasons, "baseline_sample_below_minimum")
	}
	if observation.Denominator < rule.MinimumObservationSampleSize {
		reasons = append(reasons, "observation_sample_below_minimum")
	}
	if len(reasons) > 0 {
		result.Availability = MetricInsufficientData
		result.ReasonCodes = reasons
		return result
	}
	delta := int64(*observation.RatePPM) - int64(*baseline.RatePPM)
	regression := int64(0)
	if rule.Direction == analytics.HigherIsBetter {
		regression = -delta
	} else {
		regression = delta
	}
	regressionPPM := uint64(0)
	if regression > 0 {
		regressionPPM = uint64(regression)
	}
	result.Availability, result.DeltaPPM, result.RegressionPPM, result.Regression = MetricEvaluated, &delta, &regressionPPM, regressionPPM > rule.MaximumRegressionPPM
	return result
}

func summarize(metrics []MetricComparison) (Status, []string) {
	unavailable, insufficient, regressed := false, false, false
	reasons := []string{}
	for _, metric := range metrics {
		unavailable = unavailable || metric.Availability == MetricUnavailable
		insufficient = insufficient || metric.Availability == MetricInsufficientData
		regressed = regressed || metric.Regression
		for _, reason := range metric.ReasonCodes {
			reasons = append(reasons, string(metric.MetricID)+":"+reason)
		}
		if metric.Regression {
			reasons = append(reasons, string(metric.MetricID)+":regression_exceeds_policy")
		}
	}
	sort.Strings(reasons)
	reasons = slices.Compact(reasons)
	if unavailable {
		return StatusUnavailable, reasons
	}
	if insufficient {
		return StatusInsufficientData, reasons
	}
	if regressed {
		return StatusRegressed, reasons
	}
	return StatusHealthy, reasons
}

func selectedRunIDs(snapshot analyticsadapter.ProjectionSnapshot, configSHA string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, binding := range snapshot.RunBindings {
		if binding.ConfigSHA256 == configSHA {
			result[binding.RunID] = struct{}{}
		}
	}
	return result
}
func snapshotUnavailableReasons(baseline, observation analyticsadapter.ProjectionSnapshot) []string {
	reasons := []string{}
	if baseline.Facts.Completeness != analytics.CompletenessComplete {
		reasons = append(reasons, "baseline_snapshot_incomplete")
	}
	if observation.Facts.Completeness != analytics.CompletenessComplete {
		reasons = append(reasons, "observation_snapshot_incomplete")
	}
	return reasons
}
func snapshotBinding(snapshot analyticsadapter.ProjectionSnapshot, digest string) SnapshotBinding {
	return SnapshotBinding{SnapshotID: snapshot.SnapshotID, SHA256: digest, Scope: snapshot.Scope, Window: snapshot.Window, GroupBy: slices.Clone(snapshot.GroupBy), BuiltAt: snapshot.BuiltAt}
}

func (service *Service) persist(observation Observation) (Observation, error) {
	data, err := json.Marshal(observation)
	if err != nil {
		return Observation{}, err
	}
	ref, err := service.store.PutArtifact(data)
	if err != nil {
		return Observation{}, err
	}
	item := manifest{SchemaVersion: ManifestSchemaVersion, ObservationID: observation.Request.ObservationID, ObservationSHA256: ref.SHA256, ArtifactURI: ref.URI, SizeBytes: ref.SizeBytes, PlanID: observation.Request.PlanID, Status: observation.Status, RollbackRecommendation: observation.RollbackRecommendation, ObservedAt: observation.Request.ObservedAt}
	if err := item.Validate(); err != nil {
		return Observation{}, err
	}
	objectID := manifestObjectID(item.ObservationID)
	var existing manifest
	err = service.store.GetJSON(objectID, &existing)
	if err == nil {
		if !reflect.DeepEqual(existing, item) {
			return Observation{}, fmt.Errorf("%w: observation manifest differs", ErrConflict)
		}
		return observation, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Observation{}, fmt.Errorf("%w: read manifest: %v", ErrCorrupt, err)
	}
	if err := service.store.PutJSON(objectID, item); err != nil {
		if !errors.Is(err, local.ErrImmutableExists) {
			return Observation{}, err
		}
		if err := service.store.GetJSON(objectID, &existing); err != nil || !reflect.DeepEqual(existing, item) {
			return Observation{}, fmt.Errorf("%w: concurrent manifest differs", ErrConflict)
		}
	}
	return observation, nil
}

func (service *Service) get(id string) (Observation, error) {
	if err := validateID("observation_id", id); err != nil {
		return Observation{}, err
	}
	var item manifest
	if err := service.store.GetJSON(manifestObjectID(id), &item); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Observation{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Observation{}, fmt.Errorf("%w: load manifest: %v", ErrCorrupt, err)
	}
	if err := item.Validate(); err != nil || item.ObservationID != id {
		return Observation{}, fmt.Errorf("%w: invalid manifest", ErrCorrupt)
	}
	data, err := service.store.ReadArtifact(local.ArtifactRef{URI: item.ArtifactURI, SHA256: item.ObservationSHA256, SizeBytes: item.SizeBytes})
	if err != nil {
		return Observation{}, fmt.Errorf("%w: read artifact: %v", ErrCorrupt, err)
	}
	observation, err := DecodeObservation(data)
	if err != nil {
		return Observation{}, fmt.Errorf("%w: decode artifact: %v", ErrCorrupt, err)
	}
	if err := observation.Validate(); err != nil || observation.Request.ObservationID != id || observation.Request.PlanID != item.PlanID || observation.Status != item.Status || observation.RollbackRecommendation != item.RollbackRecommendation || !observation.Request.ObservedAt.Equal(item.ObservedAt) {
		return Observation{}, fmt.Errorf("%w: observation artifact mismatch", ErrCorrupt)
	}
	return observation, nil
}

func (service *Service) loadManifests() ([]manifest, error) {
	directory := filepath.Join(service.store.Root(), "immutable", filepath.FromSlash(strings.TrimSuffix(manifestRoot, "/")))
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []manifest{}, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: unsafe manifest directory", ErrCorrupt)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	result := make([]manifest, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil, fmt.Errorf("%w: unexpected manifest entry", ErrCorrupt)
		}
		var item manifest
		digest := strings.TrimSuffix(entry.Name(), ".json")
		if !shaPattern.MatchString(digest) {
			return nil, fmt.Errorf("%w: invalid manifest object", ErrCorrupt)
		}
		if err := service.store.GetJSON(manifestRoot+digest, &item); err != nil {
			return nil, fmt.Errorf("%w: load manifest: %v", ErrCorrupt, err)
		}
		if err := item.Validate(); err != nil || manifestObjectID(item.ObservationID) != manifestRoot+digest {
			return nil, fmt.Errorf("%w: manifest lookup mismatch", ErrCorrupt)
		}
		result = append(result, item)
	}
	return result, nil
}

func manifestObjectID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return manifestRoot + hex.EncodeToString(sum[:])
}
func digestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
func authorizeRead(access evaluation.Access) error {
	if err := access.Validate(); err != nil {
		return err
	}
	if !slices.Contains(access.Roles, evaluation.RoleDatasetCurator) && !slices.Contains(access.Roles, evaluation.RoleDatasetAdjudicator) && !slices.Contains(access.Roles, evaluation.RolePromotionOperator) && !slices.Contains(access.Roles, evaluation.RolePromotionApprover) {
		return ErrUnauthorized
	}
	return nil
}
func authorizeWrite(mutation evaluation.Mutation) error {
	if err := mutation.Validate(); err != nil {
		return err
	}
	if !slices.Contains(mutation.Roles, evaluation.RolePromotionOperator) || !slices.Contains(mutation.Roles, evaluation.RolePromotionApprover) {
		return ErrUnauthorized
	}
	return nil
}
