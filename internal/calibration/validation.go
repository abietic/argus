package calibration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/abietic/argus/internal/reviewconfig"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (truth Truth) Validate() error {
	switch truth {
	case TruthValidDefect, TruthFalsePositive:
		return nil
	default:
		return fmt.Errorf("unsupported calibration truth %q", truth)
	}
}

func (authority LabelAuthority) Validate() error {
	switch authority.Kind {
	case AuthorityHumanAdjudication, AuthorityExternalGovernance:
	default:
		return fmt.Errorf("unsupported label authority kind %q", authority.Kind)
	}
	if len(authority.ReviewerIDs) < 2 {
		return fmt.Errorf("label authority requires at least two independent reviewers")
	}
	for index, reviewer := range authority.ReviewerIDs {
		if err := validateID("reviewer_id", reviewer); err != nil {
			return err
		}
		if index > 0 && authority.ReviewerIDs[index-1] >= reviewer {
			return fmt.Errorf("reviewer_ids must be uniquely sorted")
		}
	}
	if err := validateID("adjudicator_id", authority.AdjudicatorID); err != nil {
		return err
	}
	if slices.Contains(authority.ReviewerIDs, authority.AdjudicatorID) {
		return fmt.Errorf("adjudicator must be independent from reviewers")
	}
	if len(authority.EvidenceRefs) == 0 {
		return fmt.Errorf("label authority requires evidence_refs")
	}
	for index, ref := range authority.EvidenceRefs {
		if !strings.HasPrefix(ref, "artifact://") || strings.ContainsAny(ref, " \t\r\n") {
			return fmt.Errorf("evidence_refs[%d] must be a canonical artifact URI", index)
		}
		if index > 0 && authority.EvidenceRefs[index-1] >= ref {
			return fmt.Errorf("evidence_refs must be uniquely sorted")
		}
	}
	if authority.Statement != IndependentAuthorityStatement {
		return fmt.Errorf("label authority must explicitly attest %q", IndependentAuthorityStatement)
	}
	return nil
}

func (observation Observation) Validate() error {
	for name, value := range map[string]string{
		"observation_id": observation.ObservationID, "case_id": observation.CaseID,
		"review_run_id": observation.ReviewRunID, "candidate_id": observation.CandidateID,
		"repository_id": observation.RepositoryID, "dimension_id": observation.DimensionID,
		"dimension_revision": observation.DimensionRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if observation.CaseGovernanceRevision == 0 || observation.CaseLabelRevision == 0 {
		return fmt.Errorf("case governance and label revisions must be positive")
	}
	if !shaPattern.MatchString(observation.ClusterFingerprint) || !shaPattern.MatchString(observation.DimensionSHA256) {
		return fmt.Errorf("cluster_fingerprint and dimension_sha256 must be lowercase SHA-256")
	}
	if observation.RawConfidencePPM > 1_000_000 {
		return fmt.Errorf("raw_confidence_ppm exceeds PPM scale")
	}
	if err := observation.Truth.Validate(); err != nil {
		return err
	}
	return observation.Authority.Validate()
}

func (policy GatePolicy) Validate() error {
	if policy.MinimumTrainingSamples < 2 || policy.MinimumValidationSamples == 0 || policy.MinimumSliceSamples == 0 {
		return fmt.Errorf("gate sample minima must be positive and training minimum at least two")
	}
	for name, value := range map[string]uint32{
		"maximum_validation_brier_ppm": policy.MaximumValidationBrierPPM,
		"maximum_validation_ece_ppm":   policy.MaximumValidationECEPPM,
		"maximum_slice_ece_ppm":        policy.MaximumSliceECEPPM,
		"maximum_label_rate_drift_ppm": policy.MaximumLabelRateDriftPPM,
		"maximum_mean_raw_drift_ppm":   policy.MaximumMeanRawDriftPPM,
	} {
		if value > 1_000_000 {
			return fmt.Errorf("%s exceeds PPM scale", name)
		}
	}
	return nil
}

func (request FitRequest) Validate() error {
	if request.SchemaVersion != FitRequestSchemaVersion {
		return fmt.Errorf("unsupported fit request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{"run_id": request.RunID, "profile_id": request.ProfileID, "profile_revision": request.ProfileRevision} {
		if err := validateID(name, value); err != nil {
			return err
		}
		if strings.EqualFold(value, "latest") {
			return fmt.Errorf("%s must not use latest", name)
		}
	}
	if request.Training == nil || request.Validation == nil {
		return fmt.Errorf("training and validation must be explicit arrays")
	}
	if len(request.Training) > 100_000 || len(request.Validation) > 100_000 {
		return fmt.Errorf("calibration dataset exceeds 100000 samples per partition")
	}
	if err := request.GatePolicy.Validate(); err != nil {
		return err
	}
	if len(request.Training) < int(request.GatePolicy.MinimumTrainingSamples) || len(request.Validation) < int(request.GatePolicy.MinimumValidationSamples) {
		return fmt.Errorf("request does not meet declared sample minima")
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	seen := map[string]string{}
	for partition, observations := range map[string][]Observation{"training": request.Training, "validation": request.Validation} {
		previous := ""
		for index, observation := range observations {
			if err := observation.Validate(); err != nil {
				return fmt.Errorf("%s[%d]: %w", partition, index, err)
			}
			if index > 0 && observation.ObservationID <= previous {
				return fmt.Errorf("%s observations must be uniquely sorted by observation_id", partition)
			}
			if prior, ok := seen[observation.ObservationID]; ok {
				return fmt.Errorf("observation %q appears in %s and %s: %w", observation.ObservationID, prior, partition, ErrContaminated)
			}
			seen[observation.ObservationID] = partition
			previous = observation.ObservationID
		}
	}
	return nil
}

func (manifest DatasetManifest) Validate() error {
	if manifest.SchemaVersion != DatasetManifestSchemaVersion || !strings.HasPrefix(manifest.ManifestID, "calibration-dataset-") {
		return fmt.Errorf("invalid calibration dataset identity")
	}
	if manifest.Samples == nil {
		return fmt.Errorf("manifest samples must be an explicit array")
	}
	previous := ""
	for index, sample := range manifest.Samples {
		if err := sample.Observation.Validate(); err != nil {
			return fmt.Errorf("samples[%d]: %w", index, err)
		}
		if sample.DatasetSplit == "" || sample.CloneGroupID == "" {
			return fmt.Errorf("samples[%d] lacks governed split or clone group", index)
		}
		if index > 0 && sample.ObservationID <= previous {
			return fmt.Errorf("manifest samples must be uniquely sorted")
		}
		previous = sample.ObservationID
	}
	digest, err := digestManifest(manifest)
	if err != nil {
		return err
	}
	if manifest.SHA256 != digest || manifest.ManifestID != "calibration-dataset-"+digest[:24] {
		return fmt.Errorf("calibration dataset digest does not match content")
	}
	return nil
}

func (metric Metric) Validate() error {
	if metric.ScopeKind != "overall" && metric.ScopeKind != "repository" && metric.ScopeKind != "dimension" {
		return fmt.Errorf("invalid metric scope_kind %q", metric.ScopeKind)
	}
	if metric.ScopeKind == "overall" && metric.ScopeID != "all" {
		return fmt.Errorf("overall scope_id must be all")
	}
	if metric.ScopeKind != "overall" {
		if err := validateID("scope_id", metric.ScopeID); err != nil {
			return err
		}
	}
	if metric.Dataset != "training" && metric.Dataset != "validation" {
		return fmt.Errorf("invalid metric dataset %q", metric.Dataset)
	}
	if metric.PositiveCount > metric.SampleCount {
		return fmt.Errorf("positive_count exceeds sample_count")
	}
	for _, value := range []uint32{metric.PositiveRatePPM, metric.MeanRawPPM, metric.MeanCalibratedPPM, metric.BrierScorePPM, metric.ECEPPM} {
		if value > 1_000_000 {
			return fmt.Errorf("metric exceeds PPM scale")
		}
	}
	return nil
}

func (report Report) Validate() error {
	if report.SchemaVersion != ReportSchemaVersion || !strings.HasPrefix(report.ReportID, "calibration-report-") {
		return fmt.Errorf("invalid calibration report identity")
	}
	if report.ReasonCodes == nil || report.Metrics == nil || report.Drift == nil {
		return fmt.Errorf("report arrays must be explicit")
	}
	if report.Passed != (len(report.ReasonCodes) == 0) {
		return fmt.Errorf("report pass state does not match reasons")
	}
	if !slices.IsSorted(report.ReasonCodes) || hasDuplicate(report.ReasonCodes) {
		return fmt.Errorf("report reason_codes must be uniquely sorted")
	}
	for _, metric := range report.Metrics {
		if err := metric.Validate(); err != nil {
			return err
		}
	}
	previousMetric := ""
	for _, metric := range report.Metrics {
		key := metric.ScopeKind + "\x00" + metric.ScopeID + "\x00" + metric.Dataset
		if previousMetric != "" && key <= previousMetric {
			return fmt.Errorf("report metrics must be uniquely sorted")
		}
		previousMetric = key
	}
	previousDrift := ""
	for _, item := range report.Drift {
		if item.ScopeKind != "overall" && item.ScopeKind != "repository" && item.ScopeKind != "dimension" {
			return fmt.Errorf("invalid drift scope_kind %q", item.ScopeKind)
		}
		if item.Status != "evaluated" && item.Status != "insufficient_samples" {
			return fmt.Errorf("invalid drift status %q", item.Status)
		}
		if item.LabelRateDriftPPM > 1_000_000 || item.MeanRawDriftPPM > 1_000_000 {
			return fmt.Errorf("drift exceeds PPM scale")
		}
		if item.Status == "insufficient_samples" && (item.LabelRateDriftPPM != 0 || item.MeanRawDriftPPM != 0) {
			return fmt.Errorf("insufficient drift must not claim numeric deltas")
		}
		key := item.ScopeKind + "\x00" + item.ScopeID
		if previousDrift != "" && key <= previousDrift {
			return fmt.Errorf("report drift must be uniquely sorted")
		}
		previousDrift = key
	}
	digest, err := digestReport(report)
	if err != nil {
		return err
	}
	if report.SHA256 != digest || report.ReportID != "calibration-report-"+digest[:24] {
		return fmt.Errorf("calibration report digest does not match content")
	}
	return nil
}

func (candidate ProfileCandidate) Validate() error {
	if candidate.SchemaVersion != ProfileCandidateSchemaVersion || !strings.HasPrefix(candidate.CandidateID, "calibration-profile-candidate-") {
		return fmt.Errorf("invalid profile candidate identity")
	}
	if err := candidate.Profile.Validate(); err != nil {
		return err
	}
	if candidate.Status != "gate_passed" && candidate.Status != "gate_failed" {
		return fmt.Errorf("invalid profile candidate status")
	}
	if candidate.AutoPublished {
		return fmt.Errorf("calibration fitting must not auto-publish a profile")
	}
	if err := candidate.TrainingManifestRef.Validate(); err != nil {
		return fmt.Errorf("training manifest ref: %w", err)
	}
	if err := candidate.ValidationManifestRef.Validate(); err != nil {
		return fmt.Errorf("validation manifest ref: %w", err)
	}
	if candidate.TrainingManifestRef.Contract != ContractDatasetManifest || candidate.ValidationManifestRef.Contract != ContractDatasetManifest {
		return fmt.Errorf("profile candidate uses invalid manifest contract")
	}
	digest, err := digestCandidate(candidate)
	if err != nil {
		return err
	}
	if candidate.SHA256 != digest || candidate.CandidateID != "calibration-profile-candidate-"+digest[:24] {
		return fmt.Errorf("profile candidate digest does not match content")
	}
	return nil
}

func (run Run) Validate() error {
	if run.SchemaVersion != RunSchemaVersion {
		return fmt.Errorf("unsupported calibration run schema %q", run.SchemaVersion)
	}
	if err := validateID("run_id", run.RunID); err != nil {
		return err
	}
	if err := run.ProfileCandidate.Validate(); err != nil {
		return err
	}
	if err := run.Report.Validate(); err != nil {
		return err
	}
	if err := run.GatePolicy.Validate(); err != nil {
		return err
	}
	if run.ProfileCandidate.TrainingManifestRef != run.TrainingManifestRef || run.ProfileCandidate.ValidationManifestRef != run.ValidationManifestRef {
		return fmt.Errorf("profile candidate manifest refs do not match run")
	}
	if (run.ProfileCandidate.Status == "gate_passed") != run.Report.Passed {
		return fmt.Errorf("profile candidate status does not match report")
	}
	if run.CreatedBy == "" || run.CreatedAt.IsZero() || run.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("run audit identity/time is invalid")
	}
	digest, err := digestRun(run)
	if err != nil {
		return err
	}
	if run.SHA256 != digest {
		return fmt.Errorf("calibration run digest does not match content")
	}
	return nil
}

func validateID(name, value string) error {
	if !identifierPattern.MatchString(value) {
		return fmt.Errorf("%s must be a stable identifier", name)
	}
	return nil
}
func hasDuplicate(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return true
		}
	}
	return false
}
func digestValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func digestManifest(value DatasetManifest) (string, error) {
	value.ManifestID, value.SHA256 = "", ""
	return digestValue(value)
}
func digestReport(value Report) (string, error) {
	value.ReportID, value.SHA256 = "", ""
	return digestValue(value)
}
func digestCandidate(value ProfileCandidate) (string, error) {
	value.CandidateID, value.SHA256 = "", ""
	return digestValue(value)
}
func digestRun(value Run) (string, error) { value.SHA256 = ""; return digestValue(value) }

func sealManifest(samples []ManifestSample) (DatasetManifest, error) {
	manifest := DatasetManifest{SchemaVersion: DatasetManifestSchemaVersion, Samples: slices.Clone(samples)}
	digest, err := digestManifest(manifest)
	if err != nil {
		return DatasetManifest{}, err
	}
	manifest.SHA256, manifest.ManifestID = digest, "calibration-dataset-"+digest[:24]
	return manifest, manifest.Validate()
}

func sealReport(report Report) (Report, error) {
	report.SchemaVersion = ReportSchemaVersion
	slices.Sort(report.ReasonCodes)
	report.ReasonCodes = slices.Compact(report.ReasonCodes)
	digest, err := digestReport(report)
	if err != nil {
		return Report{}, err
	}
	report.SHA256, report.ReportID = digest, "calibration-report-"+digest[:24]
	return report, report.Validate()
}
func sealCandidate(candidate ProfileCandidate) (ProfileCandidate, error) {
	candidate.SchemaVersion = ProfileCandidateSchemaVersion
	digest, err := digestCandidate(candidate)
	if err != nil {
		return ProfileCandidate{}, err
	}
	candidate.SHA256, candidate.CandidateID = digest, "calibration-profile-candidate-"+digest[:24]
	return candidate, candidate.Validate()
}
func sealRun(run Run) (Run, error) {
	run.SchemaVersion = RunSchemaVersion
	digest, err := digestRun(run)
	if err != nil {
		return Run{}, err
	}
	run.SHA256 = digest
	return run, run.Validate()
}

var _ = reviewconfig.CalibrationProfile{}
