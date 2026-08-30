package analytics

import (
	"slices"
	"strings"
	"testing"
)

func TestEvaluateROIGateAcceptsCostAdjustedE2AndE3Evidence(t *testing.T) {
	for _, tier := range []EvidenceTier{EvidenceTierE2, EvidenceTierE3} {
		t.Run(string(tier), func(t *testing.T) {
			observation := valueObservation(tier)
			policy := roiPolicy(EvidenceTierE2)
			result, err := EvaluateROIGate(observation, policy)
			if err != nil {
				t.Fatalf("EvaluateROIGate() error = %v", err)
			}
			if !result.Eligible || len(result.ReasonCodes) != 0 {
				t.Fatalf("ROI result = %+v, want eligible", result)
			}
		})
	}
}

func TestEvaluateROIGateRejectsE0AndE1RegardlessOfPositiveEstimate(
	t *testing.T,
) {
	for _, tier := range []EvidenceTier{EvidenceTierE0, EvidenceTierE1} {
		t.Run(string(tier), func(t *testing.T) {
			observation := valueObservation(tier)
			result, err := EvaluateROIGate(observation, roiPolicy(EvidenceTierE2))
			if err != nil {
				t.Fatalf("EvaluateROIGate() error = %v", err)
			}
			if result.Eligible ||
				!slices.Contains(result.ReasonCodes, ROIReasonEvidenceTierTooLow) {
				t.Fatalf("ROI result = %+v, want evidence-tier rejection", result)
			}
		})
	}
}

func TestEvaluateROIGateUsesUncertaintyLowerBoundAndAllCosts(t *testing.T) {
	observation := valueObservation(EvidenceTierE2)
	observation.NetValueUncertainty.Lower.Amount = 499
	result, err := EvaluateROIGate(observation, roiPolicy(EvidenceTierE2))
	if err != nil {
		t.Fatalf("EvaluateROIGate() error = %v", err)
	}
	if result.Eligible ||
		!slices.Contains(result.ReasonCodes, ROIReasonLowerBoundBelowMinimum) {
		t.Fatalf("ROI result = %+v, want lower-bound rejection", result)
	}

	observation = valueObservation(EvidenceTierE2)
	observation.Costs.Storage.Amount = 101
	if err := observation.Validate(); err == nil ||
		!strings.Contains(err.Error(), "does not equal gross value") {
		t.Fatalf("Validate() error = %v, want incomplete cost deduction rejection", err)
	}
}

func TestROIPolicyCannotTreatE0OrE1AsFormalROI(t *testing.T) {
	for _, tier := range []EvidenceTier{EvidenceTierE0, EvidenceTierE1} {
		policy := roiPolicy(tier)
		if err := policy.Validate(); err == nil ||
			!strings.Contains(err.Error(), "must be E2 or E3") {
			t.Fatalf("Validate(%s) error = %v, want policy rejection", tier, err)
		}
	}
}

func TestValueObservationRequiresTierSpecificImmutableEvidence(t *testing.T) {
	observation := valueObservation(EvidenceTierE2)
	observation.SourceRefs = []SourceRef{
		*testSource(SourceFeedback, "feedback-1", "revision-1", 'b'),
	}
	if err := observation.Validate(); err == nil ||
		!strings.Contains(err.Error(), "requires outcome evidence") {
		t.Fatalf("Validate() error = %v, want E2 provenance rejection", err)
	}
}

func valueObservation(tier EvidenceTier) ValueObservation {
	unit := func(amount int64) FixedPoint {
		return FixedPoint{Amount: amount, Scale: 0, Unit: "CNY_micros"}
	}
	sourceKind := map[EvidenceTier]SourceKind{
		EvidenceTierE0: SourceModelEstimate,
		EvidenceTierE1: SourceFeedback,
		EvidenceTierE2: SourceOutcome,
		EvidenceTierE3: SourceExperiment,
	}[tier]
	return ValueObservation{
		SchemaVersion: ValueObservationSchemaVersion,
		ObservationID: "value-observation-" + string(tier),
		Dimensions:    testDimensions(),
		MetricID:      "net_review_value",
		EvidenceTier:  tier,
		Baseline: Baseline{
			CohortID: "baseline-cohort",
			Revision: "baseline-v1",
			Window: TimeWindow{
				StartInclusive: analyticsTime(1, 0).AddDate(0, -1, 0),
				EndExclusive:   analyticsTime(1, 0),
			},
			Value:      unit(600),
			SampleSize: 100,
			SourceRefs: []SourceRef{
				*testSource(SourceBaseline, "baseline-1", "baseline-v1", 'a'),
			},
		},
		ObservationWindow: TimeWindow{
			StartInclusive: analyticsTime(1, 0),
			EndExclusive:   analyticsTime(4, 0),
		},
		AttributionWindow: TimeWindow{
			StartInclusive: analyticsTime(1, 0),
			EndExclusive:   analyticsTime(5, 0),
		},
		SampleSize: 20,
		GrossValue: unit(1_000),
		Costs: CostBreakdown{
			ModelCompute:       unit(50),
			HumanVerification:  unit(50),
			FalsePositive:      unit(40),
			PlatformOperations: unit(40),
			Storage:            unit(20),
		},
		NetValue: unit(800),
		NetValueUncertainty: Uncertainty{
			Lower:         unit(600),
			Upper:         unit(1_000),
			ConfidenceBPS: 9_500,
			Method:        "bootstrap",
			Revision:      "uncertainty-v1",
		},
		SourceRefs: []SourceRef{
			*testSource(sourceKind, "tier-source-"+string(tier), "revision-1", 'f'),
		},
		AttributionPolicyRevision: "attribution-v1",
		ObservedAt:                analyticsTime(5, 0),
	}
}

func roiPolicy(minimumTier EvidenceTier) ROIPolicy {
	return ROIPolicy{
		SchemaVersion:                     ROIPolicySchemaVersion,
		PolicyID:                          "roi-policy",
		Revision:                          "roi-v1",
		RequiredAttributionPolicyRevision: "attribution-v1",
		MinimumEvidenceTier:               minimumTier,
		MinimumSampleSize:                 10,
		MinimumNetValue: FixedPoint{
			Amount: 500,
			Scale:  0,
			Unit:   "CNY_micros",
		},
		MinimumConfidenceBPS: 9_000,
	}
}
