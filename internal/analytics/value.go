package analytics

import (
	"fmt"
	"slices"
)

const (
	ROIReasonAttributionPolicyMismatch = "attribution_policy_revision_mismatch"
	ROIReasonConfidenceBelowMinimum    = "confidence_below_minimum"
	ROIReasonEvidenceTierTooLow        = "evidence_tier_below_roi"
	ROIReasonLowerBoundBelowMinimum    = "net_value_lower_bound_below_minimum"
	ROIReasonNetValueBelowMinimum      = "net_value_below_minimum"
	ROIReasonSampleSizeBelowMinimum    = "sample_size_below_minimum"
)

// EvaluateROIGate applies a versioned ROI policy to one immutable value
// observation. E0/E1 always fail, regardless of monetary point estimate.
// Eligibility is based on net value after every required cost category and on
// the uncertainty lower bound, not only on the point estimate.
func EvaluateROIGate(
	observation ValueObservation,
	policy ROIPolicy,
) (ROIGateResult, error) {
	if err := observation.Validate(); err != nil {
		return ROIGateResult{}, fmt.Errorf("validate observation: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return ROIGateResult{}, fmt.Errorf("validate ROI policy: %w", err)
	}
	if !fixedCompatible(observation.NetValue, policy.MinimumNetValue) {
		return ROIGateResult{}, fmt.Errorf(
			"ROI policy and observation must use the same unit and scale",
		)
	}

	reasons := make([]string, 0, 6)
	observationRank, _ := evidenceTierRank(observation.EvidenceTier)
	policyRank, _ := evidenceTierRank(policy.MinimumEvidenceTier)
	if observationRank < 2 || observationRank < policyRank {
		reasons = append(reasons, ROIReasonEvidenceTierTooLow)
	}
	if observation.AttributionPolicyRevision !=
		policy.RequiredAttributionPolicyRevision {
		reasons = append(reasons, ROIReasonAttributionPolicyMismatch)
	}
	if observation.SampleSize < policy.MinimumSampleSize {
		reasons = append(reasons, ROIReasonSampleSizeBelowMinimum)
	}
	if observation.NetValueUncertainty.ConfidenceBPS <
		policy.MinimumConfidenceBPS {
		reasons = append(reasons, ROIReasonConfidenceBelowMinimum)
	}
	if observation.NetValue.Amount < policy.MinimumNetValue.Amount {
		reasons = append(reasons, ROIReasonNetValueBelowMinimum)
	}
	if observation.NetValueUncertainty.Lower.Amount <
		policy.MinimumNetValue.Amount {
		reasons = append(reasons, ROIReasonLowerBoundBelowMinimum)
	}
	slices.Sort(reasons)
	result := ROIGateResult{
		SchemaVersion:  ROIGateResultSchemaVersion,
		ObservationID:  observation.ObservationID,
		PolicyID:       policy.PolicyID,
		PolicyRevision: policy.Revision,
		Eligible:       len(reasons) == 0,
		ReasonCodes:    reasons,
	}
	if err := result.Validate(); err != nil {
		return ROIGateResult{}, err
	}
	return result, nil
}

func (result ROIGateResult) Validate() error {
	if result.SchemaVersion != ROIGateResultSchemaVersion {
		return fmt.Errorf("unsupported ROI gate result schema %q", result.SchemaVersion)
	}
	for name, value := range map[string]string{
		"observation_id":  result.ObservationID,
		"policy_id":       result.PolicyID,
		"policy_revision": result.PolicyRevision,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := validateSortedCodes("reason_codes", result.ReasonCodes, true); err != nil {
		return err
	}
	if result.Eligible != (len(result.ReasonCodes) == 0) {
		return fmt.Errorf("eligible must be true exactly when reason_codes is empty")
	}
	return nil
}
