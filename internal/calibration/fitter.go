package calibration

import (
	"fmt"
	"slices"
	"sort"

	"argus.local/argus/internal/reviewconfig"
)

type isotonicBlock struct {
	firstRaw uint32
	lastRaw  uint32
	count    uint64
	positive uint64
}

// fitProfile applies deterministic integer-only pool-adjacent-violators
// regression. It never uses floating point, random state, or wall clock time.
func fitProfile(id, revision string, samples []ManifestSample) (reviewconfig.CalibrationProfile, error) {
	if len(samples) < 2 {
		return reviewconfig.CalibrationProfile{}, fmt.Errorf("at least two training samples are required")
	}
	type group struct {
		raw             uint32
		count, positive uint64
	}
	groups := make([]group, 0, len(samples))
	ordered := slices.Clone(samples)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].RawConfidencePPM < ordered[j].RawConfidencePPM })
	for _, sample := range ordered {
		if len(groups) == 0 || groups[len(groups)-1].raw != sample.RawConfidencePPM {
			groups = append(groups, group{raw: sample.RawConfidencePPM})
		}
		current := &groups[len(groups)-1]
		current.count++
		if sample.Truth == TruthValidDefect {
			current.positive++
		}
	}
	if len(groups) < 2 {
		return reviewconfig.CalibrationProfile{}, fmt.Errorf("training data requires at least two distinct raw confidence values")
	}
	blocks := make([]isotonicBlock, 0, len(groups))
	for _, value := range groups {
		blocks = append(blocks, isotonicBlock{firstRaw: value.raw, lastRaw: value.raw, count: value.count, positive: value.positive})
		for len(blocks) >= 2 {
			left, right := blocks[len(blocks)-2], blocks[len(blocks)-1]
			// left positive rate <= right positive rate without floating point.
			if left.positive*right.count <= right.positive*left.count {
				break
			}
			blocks[len(blocks)-2] = isotonicBlock{firstRaw: left.firstRaw, lastRaw: right.lastRaw, count: left.count + right.count, positive: left.positive + right.positive}
			blocks = blocks[:len(blocks)-1]
		}
	}
	points := make([]reviewconfig.CalibrationPoint, 0, len(groups)+2)
	blockIndex := 0
	for _, value := range groups {
		for value.raw > blocks[blockIndex].lastRaw {
			blockIndex++
		}
		confidence := roundedRatioPPM(blocks[blockIndex].positive, blocks[blockIndex].count)
		points = append(points, reviewconfig.CalibrationPoint{RawPPM: value.raw, ConfidencePPM: confidence})
	}
	if points[0].RawPPM != 0 {
		points = append([]reviewconfig.CalibrationPoint{{RawPPM: 0, ConfidencePPM: points[0].ConfidencePPM}}, points...)
	}
	if points[len(points)-1].RawPPM != 1_000_000 {
		points = append(points, reviewconfig.CalibrationPoint{RawPPM: 1_000_000, ConfidencePPM: points[len(points)-1].ConfidencePPM})
	}
	return reviewconfig.SealCalibrationProfile(id, revision, points)
}

func evaluate(profile reviewconfig.CalibrationProfile, training, validation []ManifestSample, policy GatePolicy) (Report, error) {
	allMetrics := make([]Metric, 0)
	for _, dataset := range []struct {
		name    string
		samples []ManifestSample
	}{{"training", training}, {"validation", validation}} {
		allMetrics = append(allMetrics, metricFor(profile, "overall", "all", dataset.name, dataset.samples))
		allMetrics = append(allMetrics, scopedMetrics(profile, "repository", dataset.name, dataset.samples, func(sample ManifestSample) string { return sample.RepositoryID })...)
		allMetrics = append(allMetrics, scopedMetrics(profile, "dimension", dataset.name, dataset.samples, func(sample ManifestSample) string { return sample.DimensionID })...)
	}
	sort.Slice(allMetrics, func(i, j int) bool {
		if allMetrics[i].ScopeKind != allMetrics[j].ScopeKind {
			return allMetrics[i].ScopeKind < allMetrics[j].ScopeKind
		}
		if allMetrics[i].ScopeID != allMetrics[j].ScopeID {
			return allMetrics[i].ScopeID < allMetrics[j].ScopeID
		}
		return allMetrics[i].Dataset < allMetrics[j].Dataset
	})
	drift := buildDrift(allMetrics, policy.MinimumSliceSamples)
	reasons := make([]string, 0)
	validationOverall := findMetric(allMetrics, "overall", "all", "validation")
	if validationOverall.BrierScorePPM > policy.MaximumValidationBrierPPM {
		reasons = append(reasons, "validation_brier_exceeded")
	}
	if validationOverall.ECEPPM > policy.MaximumValidationECEPPM {
		reasons = append(reasons, "validation_ece_exceeded")
	}
	for _, metric := range allMetrics {
		if metric.Dataset == "validation" && metric.ScopeKind != "overall" && metric.SampleCount >= policy.MinimumSliceSamples && metric.ECEPPM > policy.MaximumSliceECEPPM {
			reasons = append(reasons, metric.ScopeKind+"_slice_ece_exceeded:"+metric.ScopeID)
		}
	}
	for _, item := range drift {
		if item.Status != "evaluated" {
			continue
		}
		if item.LabelRateDriftPPM > policy.MaximumLabelRateDriftPPM {
			reasons = append(reasons, item.ScopeKind+"_label_rate_drift_exceeded:"+item.ScopeID)
		}
		if item.MeanRawDriftPPM > policy.MaximumMeanRawDriftPPM {
			reasons = append(reasons, item.ScopeKind+"_mean_raw_drift_exceeded:"+item.ScopeID)
		}
	}
	return sealReport(Report{Passed: len(reasons) == 0, ReasonCodes: reasons, Metrics: allMetrics, Drift: drift})
}

func metricFor(profile reviewconfig.CalibrationProfile, kind, id, dataset string, samples []ManifestSample) Metric {
	metric := Metric{ScopeKind: kind, ScopeID: id, Dataset: dataset, SampleCount: uint32(len(samples))}
	if len(samples) == 0 {
		return metric
	}
	var rawSum, calibratedSum, squaredError, eceNumerator uint64
	type bin struct{ count, positive, predictionSum uint64 }
	bins := make([]bin, 10)
	for _, sample := range samples {
		calibrated := calibrate(profile, sample.RawConfidencePPM)
		rawSum += uint64(sample.RawConfidencePPM)
		calibratedSum += uint64(calibrated)
		target := uint32(0)
		if sample.Truth == TruthValidDefect {
			target = 1_000_000
			metric.PositiveCount++
		}
		delta := absDiff(calibrated, target)
		squaredError += uint64(delta) * uint64(delta)
		index := int(calibrated / 100_000)
		if index == 10 {
			index = 9
		}
		bins[index].count++
		bins[index].predictionSum += uint64(calibrated)
		if target != 0 {
			bins[index].positive++
		}
	}
	count := uint64(len(samples))
	metric.PositiveRatePPM = roundedRatioPPM(uint64(metric.PositiveCount), count)
	metric.MeanRawPPM = uint32((rawSum + count/2) / count)
	metric.MeanCalibratedPPM = uint32((calibratedSum + count/2) / count)
	metric.BrierScorePPM = uint32((squaredError + count*500_000) / (count * 1_000_000))
	for _, value := range bins {
		if value.count == 0 {
			continue
		}
		observedScaled := value.positive * 1_000_000
		if value.predictionSum >= observedScaled {
			eceNumerator += value.predictionSum - observedScaled
		} else {
			eceNumerator += observedScaled - value.predictionSum
		}
	}
	metric.ECEPPM = uint32((eceNumerator + count/2) / count)
	return metric
}

func scopedMetrics(profile reviewconfig.CalibrationProfile, kind, dataset string, samples []ManifestSample, key func(ManifestSample) string) []Metric {
	groups := map[string][]ManifestSample{}
	for _, sample := range samples {
		groups[key(sample)] = append(groups[key(sample)], sample)
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]Metric, 0, len(ids))
	for _, id := range ids {
		result = append(result, metricFor(profile, kind, id, dataset, groups[id]))
	}
	return result
}

func buildDrift(metrics []Metric, minimum uint32) []DriftMetric {
	type key struct{ kind, id string }
	keys := map[key]struct{}{}
	for _, metric := range metrics {
		keys[key{metric.ScopeKind, metric.ScopeID}] = struct{}{}
	}
	ordered := make([]key, 0, len(keys))
	for value := range keys {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].kind != ordered[j].kind {
			return ordered[i].kind < ordered[j].kind
		}
		return ordered[i].id < ordered[j].id
	})
	result := make([]DriftMetric, 0, len(ordered))
	for _, value := range ordered {
		training, validation := findMetric(metrics, value.kind, value.id, "training"), findMetric(metrics, value.kind, value.id, "validation")
		item := DriftMetric{ScopeKind: value.kind, ScopeID: value.id, TrainingSamples: training.SampleCount, ValidationSamples: validation.SampleCount, Status: "insufficient_samples"}
		if training.SampleCount >= minimum && validation.SampleCount >= minimum {
			item.Status = "evaluated"
			item.LabelRateDriftPPM = absDiff(training.PositiveRatePPM, validation.PositiveRatePPM)
			item.MeanRawDriftPPM = absDiff(training.MeanRawPPM, validation.MeanRawPPM)
		}
		result = append(result, item)
	}
	return result
}

func findMetric(metrics []Metric, kind, id, dataset string) Metric {
	for _, metric := range metrics {
		if metric.ScopeKind == kind && metric.ScopeID == id && metric.Dataset == dataset {
			return metric
		}
	}
	return Metric{ScopeKind: kind, ScopeID: id, Dataset: dataset}
}
func calibrate(profile reviewconfig.CalibrationProfile, raw uint32) uint32 {
	points := profile.Points
	for index := 1; index < len(points); index++ {
		if raw > points[index].RawPPM {
			continue
		}
		left, right := points[index-1], points[index]
		if right.RawPPM == left.RawPPM {
			return right.ConfidencePPM
		}
		numerator := uint64(raw-left.RawPPM) * uint64(right.ConfidencePPM-left.ConfidencePPM)
		return left.ConfidencePPM + uint32((numerator+uint64(right.RawPPM-left.RawPPM)/2)/uint64(right.RawPPM-left.RawPPM))
	}
	return points[len(points)-1].ConfidencePPM
}
func roundedRatioPPM(numerator, denominator uint64) uint32 {
	return uint32((numerator*1_000_000 + denominator/2) / denominator)
}
func absDiff(left, right uint32) uint32 {
	if left >= right {
		return left - right
	}
	return right - left
}
