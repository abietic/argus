package normalizationpromotion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$`)

func (thresholds Thresholds) Validate() error {
	if thresholds.MinimumCases == 0 || thresholds.MinimumEligibleCandidates == 0 ||
		thresholds.MinimumOracleDuplicatePairs == 0 || thresholds.MinimumOracleDistinctPairs == 0 {
		return fmt.Errorf("normalization promotion sample thresholds must be positive")
	}
	for name, value := range map[string]uint32{
		"minimum_pairwise_precision_ppm": thresholds.MinimumPairwisePrecisionPPM,
		"minimum_pairwise_recall_ppm":    thresholds.MinimumPairwiseRecallPPM,
		"maximum_false_merge_rate_ppm":   thresholds.MaximumFalseMergeRatePPM,
		"minimum_exact_partition_ppm":    thresholds.MinimumExactPartitionPPM,
	} {
		if value > 1_000_000 {
			return fmt.Errorf("%s must not exceed 1000000", name)
		}
	}
	return nil
}

func (policy Policy) Validate() error {
	if policy.SchemaVersion != PolicySchemaVersion {
		return fmt.Errorf("unsupported normalization promotion policy schema %q", policy.SchemaVersion)
	}
	for name, value := range map[string]string{"policy_id": policy.PolicyID, "revision": policy.Revision} {
		if !idPattern.MatchString(value) || strings.EqualFold(value, "latest") {
			return fmt.Errorf("%s must be an explicit stable identifier", name)
		}
	}
	if err := policy.Test.Validate(); err != nil {
		return fmt.Errorf("test thresholds: %w", err)
	}
	if err := policy.Holdout.Validate(); err != nil {
		return fmt.Errorf("holdout thresholds: %w", err)
	}
	if policy.MinimumShadowRuns == 0 || policy.MinimumCanaryRuns == 0 {
		return fmt.Errorf("minimum shadow and canary runs must be positive")
	}
	if policy.CanaryPercentage < 1 || policy.CanaryPercentage >= 100 {
		return fmt.Errorf("canary_percentage must be between 1 and 99")
	}
	if policy.CreatedAt.IsZero() || policy.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("policy created_at must be non-zero UTC")
	}
	return nil
}

func (request OperationalGateRequest) Validate() error {
	if request.SchemaVersion != OperationalGateRequestSchemaVersion {
		return fmt.Errorf("unsupported normalization operational gate request schema %q", request.SchemaVersion)
	}
	if !idPattern.MatchString(request.VariantID) {
		return fmt.Errorf("variant_id is invalid")
	}
	if request.Gate != evaluation.GateShadowTraffic && request.Gate != evaluation.GateCanary &&
		request.Gate != evaluation.GateAuthorization && request.Gate != evaluation.GateRollbackMonitor {
		return fmt.Errorf("unsupported normalization operational gate %q", request.Gate)
	}
	if err := request.PolicyRef.Validate(); err != nil || request.PolicyRef.Contract != PolicyContract {
		return fmt.Errorf("policy_ref is invalid")
	}
	if request.SafetyEventCount < 0 {
		return fmt.Errorf("safety_event_count must not be negative")
	}
	if request.ReviewRunIDs == nil {
		return fmt.Errorf("review_run_ids must be an explicit array")
	}
	for index, id := range request.ReviewRunIDs {
		if !idPattern.MatchString(id) || index > 0 && id <= request.ReviewRunIDs[index-1] {
			return fmt.Errorf("review_run_ids must be uniquely sorted stable identifiers")
		}
	}
	if request.Gate == evaluation.GateShadowTraffic || request.Gate == evaluation.GateCanary {
		if len(request.ReviewRunIDs) == 0 || request.Authorization != nil {
			return fmt.Errorf("shadow and canary gates require runs and forbid authorization")
		}
	} else if len(request.ReviewRunIDs) != 0 {
		return fmt.Errorf("authorization and rollback gates must not contain review runs")
	}
	if request.Gate == evaluation.GateAuthorization {
		if request.Authorization == nil {
			return fmt.Errorf("authorization gate requires authorization")
		}
		if err := request.Authorization.Validate(); err != nil {
			return err
		}
	} else if request.Authorization != nil {
		return fmt.Errorf("authorization is only valid for promotion_authorization")
	}
	if request.EvaluatedAt.IsZero() || request.EvaluatedAt.Location() != time.UTC {
		return fmt.Errorf("evaluated_at must be non-zero UTC")
	}
	return nil
}

func (request PrepareRequest) Validate() error {
	if request.SchemaVersion != PrepareRequestSchemaVersion {
		return fmt.Errorf("unsupported normalization promotion prepare request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"promotion_variant_id":        request.PromotionVariantID,
		"baseline_review_run_id":      request.BaselineReviewRunID,
		"baseline_config_revision_id": request.BaselineConfigRevisionID,
		"baseline_config_revision":    request.BaselineConfigRevision,
		"variant_config_revision_id":  request.VariantConfigRevisionID,
		"variant_config_revision":     request.VariantConfigRevision,
		"variant_policy_revision":     request.VariantPolicyRevision,
		"rollback_policy_revision":    request.RollbackPolicyRevision,
		"promotion_policy_revision":   request.PromotionPolicyRevision,
	} {
		if !idPattern.MatchString(value) || strings.EqualFold(value, "latest") {
			return fmt.Errorf("%s must be an explicit stable identifier", name)
		}
	}
	if request.VariantPolicyRevision == request.RollbackPolicyRevision {
		return fmt.Errorf("variant and rollback policy revisions must differ")
	}
	if request.BaselineConfigRevisionID == request.VariantConfigRevisionID &&
		request.BaselineConfigRevision == request.VariantConfigRevision {
		return fmt.Errorf("variant config revision must differ from baseline")
	}
	if err := contractsv1alpha1.ValidateCandidateNormalizationImplementation(
		request.VariantImplementation,
		"variant_implementation",
	); err != nil {
		return err
	}
	if err := contractsv1alpha1.ValidateCandidateNormalizationImplementation(
		request.RollbackImplementation,
		"rollback_implementation",
	); err != nil {
		return err
	}
	variantPolicy, err := contractsv1alpha1.CandidateNormalizationWorkflowRevision(
		request.VariantImplementation.Revision,
	)
	if err != nil || variantPolicy != request.VariantPolicyRevision {
		return fmt.Errorf("variant implementation does not match variant_policy_revision")
	}
	rollbackPolicy, err := contractsv1alpha1.CandidateNormalizationWorkflowRevision(
		request.RollbackImplementation.Revision,
	)
	if err != nil || rollbackPolicy != request.RollbackPolicyRevision {
		return fmt.Errorf("rollback implementation does not match rollback_policy_revision")
	}
	if request.VariantImplementation.Revision == request.RollbackImplementation.Revision {
		return fmt.Errorf("variant and rollback implementation revisions must differ")
	}
	if request.VariantImplementation.SHA256 != request.RollbackImplementation.SHA256 {
		return fmt.Errorf("variant and rollback implementations must use the same worker SHA-256")
	}
	if strings.TrimSpace(request.Owner) == "" || len(request.Owner) > 256 || strings.ContainsAny(request.Owner, "\r\n") {
		return fmt.Errorf("owner is invalid")
	}
	if err := request.Policy.Validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC || request.Policy.CreatedAt.After(request.CreatedAt) {
		return fmt.Errorf("created_at must be UTC and not precede policy creation")
	}
	return nil
}

func (request GateRequest) Validate() error {
	if request.SchemaVersion != GateRequestSchemaVersion {
		return fmt.Errorf("unsupported normalization promotion gate request schema %q", request.SchemaVersion)
	}
	if !idPattern.MatchString(request.VariantID) {
		return fmt.Errorf("variant_id is invalid")
	}
	if request.Gate != evaluation.GateTargetedRegression && request.Gate != evaluation.GateFixedHoldout {
		return fmt.Errorf("normalization quality gate must be targeted_regression or fixed_holdout")
	}
	if err := request.PolicyRef.Validate(); err != nil || request.PolicyRef.Contract != PolicyContract {
		return fmt.Errorf("policy_ref is invalid")
	}
	if err := request.QualityRunRef.Validate(); err != nil || request.QualityRunRef.Contract != evaluation.NormalizationQualityRunContract {
		return fmt.Errorf("quality_run_ref is invalid")
	}
	if request.EvaluatedAt.IsZero() || request.EvaluatedAt.Location() != time.UTC {
		return fmt.Errorf("evaluated_at must be non-zero UTC")
	}
	return nil
}

func (decision GateDecision) Validate() error {
	if decision.SchemaVersion != DecisionSchemaVersion {
		return fmt.Errorf("unsupported normalization promotion decision schema %q", decision.SchemaVersion)
	}
	if !idPattern.MatchString(decision.VariantID) || !idPattern.MatchString(decision.QualityRunID) {
		return fmt.Errorf("normalization promotion decision identity is invalid")
	}
	if decision.Gate != evaluation.GateTargetedRegression && decision.Gate != evaluation.GateFixedHoldout {
		return fmt.Errorf("normalization promotion decision gate is invalid")
	}
	if err := decision.PolicyRef.Validate(); err != nil || decision.PolicyRef.Contract != PolicyContract {
		return fmt.Errorf("decision policy_ref is invalid")
	}
	if err := decision.QualityRunRef.Validate(); err != nil || decision.QualityRunRef.Contract != evaluation.NormalizationQualityRunContract {
		return fmt.Errorf("decision quality_run_ref is invalid")
	}
	if err := decision.Split.Validate(); err != nil || decision.Split == evaluation.SplitUnassigned {
		return fmt.Errorf("decision split is invalid")
	}
	if err := decision.Outcome.Validate(); err != nil {
		return err
	}
	if decision.ReasonCodes == nil || len(decision.ReasonCodes) == 0 {
		return fmt.Errorf("reason_codes must be non-empty")
	}
	for index, reason := range decision.ReasonCodes {
		if !idPattern.MatchString(reason) || (index > 0 && reason <= decision.ReasonCodes[index-1]) {
			return fmt.Errorf("reason_codes must be uniquely sorted stable identifiers")
		}
	}
	if err := decision.Thresholds.Validate(); err != nil {
		return err
	}
	if err := decision.Measured.ValidateAggregate(); err != nil {
		return fmt.Errorf("measured summary: %w", err)
	}
	if decision.EvaluatedAt.IsZero() || decision.EvaluatedAt.Location() != time.UTC {
		return fmt.Errorf("evaluated_at must be non-zero UTC")
	}
	return nil
}

func DecodePolicy(data []byte) (Policy, error) {
	return decodeStrict(data, "NormalizationPromotionPolicy", func(value Policy) error { return value.Validate() })
}

func DecodePrepareRequest(data []byte) (PrepareRequest, error) {
	return decodeStrict(data, "NormalizationPromotionPrepareRequest", func(value PrepareRequest) error { return value.Validate() })
}

func DecodeGateRequest(data []byte) (GateRequest, error) {
	return decodeStrict(data, "NormalizationPromotionGateRequest", func(value GateRequest) error { return value.Validate() })
}

func DecodeOperationalGateRequest(data []byte) (OperationalGateRequest, error) {
	return decodeStrict(data, "NormalizationPromotionOperationalGateRequest", func(value OperationalGateRequest) error { return value.Validate() })
}

func DecodeGateDecision(data []byte) (GateDecision, error) {
	return decodeStrict(data, "NormalizationPromotionGateDecision", func(value GateDecision) error { return value.Validate() })
}

func decodeStrict[T any](data []byte, name string, validate func(T) error) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := inspectJSON(decoder); err != nil {
		return value, fmt.Errorf("decode %s structure: %w", name, err)
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", name, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return value, fmt.Errorf("%s contains multiple JSON values", name)
		}
		return value, fmt.Errorf("decode trailing %s: %w", name, err)
	}
	if err := validate(value); err != nil {
		return value, fmt.Errorf("validate %s: %w", name, err)
	}
	return value, nil
}

func inspectJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return fmt.Errorf("explicit null is not allowed")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key := keyToken.(string)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := inspectJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return nil
	}
}

func sortedReasons(reasons []string) []string {
	slices.Sort(reasons)
	return slices.Compact(reasons)
}
