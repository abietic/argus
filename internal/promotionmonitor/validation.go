package promotionmonitor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"regexp"
	"slices"
	"strings"
	"time"

	"argus.local/argus/internal/analytics"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var metricDefinitions = map[MetricID]struct {
	direction  analytics.MetricDirection
	definition string
}{
	MetricRunSuccessRate:           {analytics.HigherIsBetter, "succeeded ReviewRuns divided by selected ReviewRuns"},
	MetricRunCompleteRate:          {analytics.HigherIsBetter, "complete ReviewRuns divided by selected ReviewRuns"},
	MetricPublishedOutcomeCoverage: {analytics.HigherIsBetter, "published Findings with a known Outcome divided by published Findings"},
	MetricPublishedFixedRate:       {analytics.HigherIsBetter, "fixed published Findings divided by published Findings with a known Outcome"},
	MetricPublishedAdverseRate:     {analytics.LowerIsBetter, "recurred or escaped published Findings divided by published Findings with a known Outcome"},
}

func (policy Policy) Validate() error {
	if policy.SchemaVersion != PolicySchemaVersion {
		return fmt.Errorf("unsupported monitor policy schema %q", policy.SchemaVersion)
	}
	if err := validateID("policy_id", policy.PolicyID); err != nil {
		return err
	}
	if err := validateID("revision", policy.Revision); err != nil {
		return err
	}
	if len(policy.Rules) != len(metricDefinitions) {
		return fmt.Errorf("rules must contain every required metric exactly once")
	}
	seen := map[MetricID]struct{}{}
	prior := MetricID("")
	for index, rule := range policy.Rules {
		definition, ok := metricDefinitions[rule.MetricID]
		if !ok {
			return fmt.Errorf("rules[%d] has unsupported metric_id %q", index, rule.MetricID)
		}
		if _, duplicate := seen[rule.MetricID]; duplicate {
			return fmt.Errorf("rules contain duplicate metric_id %q", rule.MetricID)
		}
		seen[rule.MetricID] = struct{}{}
		if prior != "" && prior >= rule.MetricID {
			return fmt.Errorf("rules must be sorted by metric_id")
		}
		prior = rule.MetricID
		if rule.Direction != definition.direction {
			return fmt.Errorf("rule %q direction must be %q", rule.MetricID, definition.direction)
		}
		if rule.MinimumBaselineSampleSize == 0 || rule.MinimumObservationSampleSize == 0 {
			return fmt.Errorf("rule %q minimum sample sizes must be positive", rule.MetricID)
		}
		if rule.MaximumRegressionPPM > RateScalePPM {
			return fmt.Errorf("rule %q maximum_regression_ppm exceeds scale", rule.MetricID)
		}
	}
	return nil
}

func (request BuildRequest) Validate() error {
	if request.SchemaVersion != RequestSchemaVersion {
		return fmt.Errorf("unsupported observation request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{"observation_id": request.ObservationID, "plan_id": request.PlanID, "baseline_snapshot_id": request.BaselineSnapshotID, "observation_snapshot_id": request.ObservationSnapshotID} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.BaselineSnapshotID == request.ObservationSnapshotID {
		return fmt.Errorf("baseline and observation snapshots must differ")
	}
	if err := request.Policy.Validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	return validateUTC("observed_at", request.ObservedAt)
}

func (value MetricValue) Validate() error {
	if value.Numerator > value.Denominator {
		return fmt.Errorf("numerator exceeds denominator")
	}
	if value.Denominator == 0 {
		if value.RatePPM != nil {
			return fmt.Errorf("rate_ppm must be absent when denominator is zero")
		}
		return nil
	}
	if value.RatePPM == nil || *value.RatePPM > RateScalePPM {
		return fmt.Errorf("rate_ppm must be present and within scale when denominator is positive")
	}
	hi, lo := bits.Mul64(value.Numerator, RateScalePPM)
	expected, _ := bits.Div64(hi, lo, value.Denominator)
	if *value.RatePPM != expected {
		return fmt.Errorf("rate_ppm does not match numerator and denominator")
	}
	return nil
}

func (comparison MetricComparison) Validate() error {
	definition, ok := metricDefinitions[comparison.MetricID]
	if !ok || comparison.Definition != definition.definition || comparison.Direction != definition.direction {
		return fmt.Errorf("metric definition binding is invalid")
	}
	if err := comparison.Baseline.Validate(); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	if err := comparison.Observation.Validate(); err != nil {
		return fmt.Errorf("observation: %w", err)
	}
	if err := validateSortedCodes("reason_codes", comparison.ReasonCodes); err != nil {
		return err
	}
	switch comparison.Availability {
	case MetricEvaluated:
		if comparison.DeltaPPM == nil || comparison.RegressionPPM == nil || len(comparison.ReasonCodes) != 0 {
			return fmt.Errorf("evaluated metric must contain deltas and no reason codes")
		}
	case MetricInsufficientData, MetricUnavailable:
		if comparison.DeltaPPM != nil || comparison.RegressionPPM != nil || comparison.Regression || len(comparison.ReasonCodes) == 0 {
			return fmt.Errorf("non-evaluated metric projection is inconsistent")
		}
	default:
		return fmt.Errorf("unsupported metric availability %q", comparison.Availability)
	}
	return nil
}

func (observation Observation) Validate() error {
	if observation.SchemaVersion != ObservationSchemaVersion {
		return fmt.Errorf("unsupported observation schema %q", observation.SchemaVersion)
	}
	if err := observation.Request.Validate(); err != nil {
		return err
	}
	if observation.Plan.PlanID != observation.Request.PlanID {
		return fmt.Errorf("plan binding does not match request")
	}
	for name, value := range map[string]string{"plan_sha256": observation.Plan.PlanSHA256, "calibration_run_sha256": observation.Plan.CalibrationRunSHA256, "baseline_bundle_sha256": observation.Plan.BaselineBundleSHA256, "variant_bundle_sha256": observation.Plan.VariantBundleSHA256, "variant_config_revision_sha256": observation.Plan.VariantConfigRevisionSHA256, "baseline_snapshot_sha256": observation.BaselineSnapshot.SHA256, "observation_snapshot_sha256": observation.ObservationSnapshot.SHA256} {
		if !shaPattern.MatchString(value) {
			return fmt.Errorf("%s must be lowercase SHA-256", name)
		}
	}
	for name, value := range map[string]string{"calibration_run_id": observation.Plan.CalibrationRunID, "baseline_review_run_id": observation.Plan.BaselineReviewRunID, "variant_config_revision_id": observation.Plan.VariantConfigRevisionID, "variant_config_revision": observation.Plan.VariantConfigRevision, "promotion_variant_id": observation.Plan.PromotionVariantID, "promotion_policy_revision": observation.Plan.PromotionPolicyRevision} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := validateUTC("activation_at", observation.Plan.ActivationAt); err != nil {
		return err
	}
	if observation.BaselineSnapshot.SnapshotID != observation.Request.BaselineSnapshotID || observation.ObservationSnapshot.SnapshotID != observation.Request.ObservationSnapshotID {
		return fmt.Errorf("snapshot bindings do not match request")
	}
	if err := observation.BaselineSnapshot.Validate(); err != nil {
		return fmt.Errorf("baseline snapshot binding: %w", err)
	}
	if err := observation.ObservationSnapshot.Validate(); err != nil {
		return fmt.Errorf("observation snapshot binding: %w", err)
	}
	if observation.BaselineSnapshot.Scope != observation.ObservationSnapshot.Scope || !slices.Equal(observation.BaselineSnapshot.GroupBy, observation.ObservationSnapshot.GroupBy) {
		return fmt.Errorf("snapshot scope/group_by mismatch")
	}
	if observation.BaselineSnapshot.Window.EndExclusive.After(observation.Plan.ActivationAt) || observation.ObservationSnapshot.Window.StartInclusive.Before(observation.Plan.ActivationAt) {
		return fmt.Errorf("snapshot windows cross activation boundary")
	}
	if observation.Request.ObservedAt.Before(observation.BaselineSnapshot.BuiltAt) || observation.Request.ObservedAt.Before(observation.ObservationSnapshot.BuiltAt) {
		return fmt.Errorf("observed_at must not precede either snapshot build")
	}
	if len(observation.Metrics) != len(metricDefinitions) {
		return fmt.Errorf("metrics must contain every required metric")
	}
	prior := MetricID("")
	regressed, insufficient, unavailable := false, false, false
	for index, metric := range observation.Metrics {
		if prior != "" && prior >= metric.MetricID {
			return fmt.Errorf("metrics must be sorted by metric_id")
		}
		prior = metric.MetricID
		if err := metric.Validate(); err != nil {
			return fmt.Errorf("metrics[%d]: %w", index, err)
		}
		rule := observation.Request.Policy.Rules[index]
		if rule.MetricID != metric.MetricID || metric.Availability == MetricEvaluated && metric.Regression != (*metric.RegressionPPM > rule.MaximumRegressionPPM) {
			return fmt.Errorf("metrics[%d] policy threshold binding is inconsistent", index)
		}
		regressed = regressed || metric.Regression
		insufficient = insufficient || metric.Availability == MetricInsufficientData
		unavailable = unavailable || metric.Availability == MetricUnavailable
	}
	expected := StatusHealthy
	if unavailable {
		expected = StatusUnavailable
	} else if insufficient {
		expected = StatusInsufficientData
	} else if regressed {
		expected = StatusRegressed
	}
	if observation.Status != expected || observation.RollbackRecommendation != (expected == StatusRegressed) {
		return fmt.Errorf("observation status/recommendation is inconsistent")
	}
	if err := validateSortedCodes("reason_codes", observation.ReasonCodes); err != nil {
		return err
	}
	if strings.TrimSpace(observation.ObservedBy) == "" || strings.ContainsAny(observation.ObservedBy, "\r\n") || strings.TrimSpace(observation.Audit) == "" || strings.ContainsAny(observation.Audit, "\r\n") {
		return fmt.Errorf("observation audit metadata is invalid")
	}
	return validateID("idempotency_key", observation.IdempotencyKey)
}

func (binding SnapshotBinding) Validate() error {
	if err := validateID("snapshot_id", binding.SnapshotID); err != nil {
		return err
	}
	if !shaPattern.MatchString(binding.SHA256) {
		return fmt.Errorf("snapshot sha256 is invalid")
	}
	if err := binding.Scope.Validate(); err != nil {
		return err
	}
	if err := binding.Window.Validate(); err != nil {
		return err
	}
	if binding.GroupBy == nil {
		return fmt.Errorf("group_by must be an explicit array")
	}
	allowed := map[analytics.DimensionName]struct{}{
		analytics.DimensionTenant: {}, analytics.DimensionOrganization: {}, analytics.DimensionRepository: {}, analytics.DimensionLanguage: {}, analytics.DimensionRule: {}, analytics.DimensionPath: {}, analytics.DimensionReviewDimension: {}, analytics.DimensionWorkflowRevision: {}, analytics.DimensionConfigRevision: {}, analytics.DimensionExperiment: {}, analytics.DimensionExperimentArm: {}, analytics.DimensionVariant: {}, analytics.DimensionRepeatability: {}, analytics.DimensionEvaluationCase: {}, analytics.DimensionContextProvider: {}, analytics.DimensionContextKind: {}, analytics.DimensionContextGapReason: {}, analytics.DimensionLineageRelation: {}, analytics.DimensionLineageMethod: {}, analytics.DimensionLineagePolicy: {},
	}
	for index, dimension := range binding.GroupBy {
		if _, ok := allowed[dimension]; !ok {
			return fmt.Errorf("group_by[%d] is unsupported", index)
		}
		if index > 0 && binding.GroupBy[index-1] >= dimension {
			return fmt.Errorf("group_by must be sorted and unique")
		}
	}
	return validateUTC("built_at", binding.BuiltAt)
}

func (item manifest) Validate() error {
	if item.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported observation manifest schema %q", item.SchemaVersion)
	}
	if err := validateID("observation_id", item.ObservationID); err != nil {
		return err
	}
	if err := validateID("plan_id", item.PlanID); err != nil {
		return err
	}
	if !shaPattern.MatchString(item.ObservationSHA256) {
		return fmt.Errorf("observation_sha256 is invalid")
	}
	if item.ArtifactURI == "" || item.SizeBytes <= 0 {
		return fmt.Errorf("artifact binding is invalid")
	}
	return (Summary{ObservationID: item.ObservationID, PlanID: item.PlanID, Status: item.Status, RollbackRecommendation: item.RollbackRecommendation, ObservationSHA256: item.ObservationSHA256, ObservedAt: item.ObservedAt}).Validate()
}

func (summary Summary) Validate() error {
	if err := validateID("observation_id", summary.ObservationID); err != nil {
		return err
	}
	if err := validateID("plan_id", summary.PlanID); err != nil {
		return err
	}
	if !shaPattern.MatchString(summary.ObservationSHA256) {
		return fmt.Errorf("observation_sha256 is invalid")
	}
	switch summary.Status {
	case StatusHealthy, StatusRegressed, StatusInsufficientData, StatusUnavailable:
	default:
		return fmt.Errorf("unsupported status")
	}
	if summary.RollbackRecommendation != (summary.Status == StatusRegressed) {
		return fmt.Errorf("summary recommendation mismatch")
	}
	return validateUTC("observed_at", summary.ObservedAt)
}

func validateID(name, value string) error {
	if !idPattern.MatchString(value) || strings.EqualFold(value, "latest") {
		return fmt.Errorf("%s must be an explicit stable identifier", name)
	}
	return nil
}
func validateUTC(name string, value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC {
		return fmt.Errorf("%s must be non-zero UTC", name)
	}
	return nil
}
func validateSortedCodes(name string, values []string) error {
	if values == nil {
		return fmt.Errorf("%s must be an explicit array", name)
	}
	for i, v := range values {
		if !idPattern.MatchString(v) {
			return fmt.Errorf("%s[%d] is invalid", name, i)
		}
		if i > 0 && values[i-1] >= v {
			return fmt.Errorf("%s must be sorted and unique", name)
		}
	}
	return nil
}

func DecodeBuildRequest(data []byte) (BuildRequest, error) {
	var value BuildRequest
	if err := rejectDuplicateFields(data); err != nil {
		return value, fmt.Errorf("decode observation request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode observation request: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return value, fmt.Errorf("decode observation request: trailing JSON")
	}
	if err := value.Validate(); err != nil {
		return value, fmt.Errorf("validate observation request: %w", err)
	}
	return value, nil
}

func DecodeObservation(data []byte) (Observation, error) {
	var value Observation
	if err := rejectDuplicateFields(data); err != nil {
		return value, fmt.Errorf("decode observation: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode observation: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return value, fmt.Errorf("decode observation: trailing JSON")
	}
	if err := value.Validate(); err != nil {
		return value, fmt.Errorf("validate observation: %w", err)
	}
	return value, nil
}

func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON token")
		}
		return err
	}
	return nil
}

func inspectJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectJSON(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("invalid object closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSON(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("invalid array closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
