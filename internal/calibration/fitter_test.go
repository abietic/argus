package calibration

import (
	"testing"

	"github.com/abietic/argus/internal/reviewconfig"
)

func TestFitProfileUsesMonotonicIntegerPAV(t *testing.T) {
	samples := []ManifestSample{
		{Observation: Observation{RawConfidencePPM: 100_000, Truth: TruthValidDefect}},
		{Observation: Observation{RawConfidencePPM: 200_000, Truth: TruthFalsePositive}},
		{Observation: Observation{RawConfidencePPM: 800_000, Truth: TruthValidDefect}},
		{Observation: Observation{RawConfidencePPM: 900_000, Truth: TruthValidDefect}},
	}
	profile, err := fitProfile("profile", "1", samples)
	if err != nil {
		t.Fatalf("fitProfile() error = %v", err)
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("profile.Validate() error = %v", err)
	}
	// The decreasing 1.0 then 0.0 observations are pooled to 0.5.
	if got := profile.Points[1].ConfidencePPM; got != 500_000 {
		t.Fatalf("first fitted confidence = %d, want 500000", got)
	}
	if got := profile.Points[2].ConfidencePPM; got != 500_000 {
		t.Fatalf("second fitted confidence = %d, want 500000", got)
	}
	for index := 1; index < len(profile.Points); index++ {
		if profile.Points[index].ConfidencePPM < profile.Points[index-1].ConfidencePPM {
			t.Fatalf("profile is not monotonic: %+v", profile.Points)
		}
	}
}

func TestEvaluateUsesIntegerMetricsAndSliceDrift(t *testing.T) {
	profile, err := reviewconfig.SealCalibrationProfile("profile", "1", []reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	training := []ManifestSample{{Observation: Observation{RepositoryID: "repo", DimensionID: "correctness", RawConfidencePPM: 100_000, Truth: TruthFalsePositive}}, {Observation: Observation{RepositoryID: "repo", DimensionID: "correctness", RawConfidencePPM: 900_000, Truth: TruthValidDefect}}}
	validation := []ManifestSample{{Observation: Observation{RepositoryID: "repo", DimensionID: "correctness", RawConfidencePPM: 200_000, Truth: TruthFalsePositive}}, {Observation: Observation{RepositoryID: "repo", DimensionID: "correctness", RawConfidencePPM: 800_000, Truth: TruthValidDefect}}}
	report, err := evaluate(profile, training, validation, GatePolicy{MinimumTrainingSamples: 2, MinimumValidationSamples: 2, MinimumSliceSamples: 1, MaximumValidationBrierPPM: 1_000_000, MaximumValidationECEPPM: 1_000_000, MaximumSliceECEPPM: 1_000_000, MaximumLabelRateDriftPPM: 1_000_000, MaximumMeanRawDriftPPM: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || len(report.Metrics) != 6 || len(report.Drift) != 3 {
		t.Fatalf("report = %+v", report)
	}
	metric := findMetric(report.Metrics, "overall", "all", "validation")
	if metric.BrierScorePPM != 40_000 || metric.ECEPPM != 200_000 {
		t.Fatalf("validation metric = %+v", metric)
	}
}
