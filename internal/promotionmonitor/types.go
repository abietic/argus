// Package promotionmonitor materializes immutable, evidence-bound observations
// for one active calibration promotion. It never mutates promotion or config
// state and can only recommend rollback.
package promotionmonitor

import (
	"errors"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/analyticsadapter"
)

const (
	RequestSchemaVersion     = "argus.calibration_promotion_observation_request.v1alpha1"
	PolicySchemaVersion      = "argus.calibration_promotion_monitor_policy.v1alpha1"
	ObservationSchemaVersion = "argus.calibration_promotion_observation.v1alpha1"
	ManifestSchemaVersion    = "argus.calibration_promotion_observation_manifest.v1alpha1"
)

const RateScalePPM uint64 = 1_000_000

var (
	ErrNotFound         = errors.New("calibration promotion observation not found")
	ErrConflict         = errors.New("calibration promotion observation conflict")
	ErrUnauthorized     = errors.New("calibration promotion observation unauthorized")
	ErrEvidenceMismatch = errors.New("calibration promotion observation evidence mismatch")
	ErrCorrupt          = errors.New("corrupt calibration promotion observation repository")
)

type MetricID string

const (
	MetricRunSuccessRate           MetricID = "review_run_success_rate"
	MetricRunCompleteRate          MetricID = "review_run_complete_rate"
	MetricPublishedOutcomeCoverage MetricID = "published_outcome_coverage_rate"
	MetricPublishedFixedRate       MetricID = "published_fixed_rate"
	MetricPublishedAdverseRate     MetricID = "published_adverse_rate"
)

type MetricRule struct {
	MetricID                     MetricID                  `json:"metric_id"`
	Direction                    analytics.MetricDirection `json:"direction"`
	MinimumBaselineSampleSize    uint64                    `json:"minimum_baseline_sample_size"`
	MinimumObservationSampleSize uint64                    `json:"minimum_observation_sample_size"`
	MaximumRegressionPPM         uint64                    `json:"maximum_regression_ppm"`
}

type Policy struct {
	SchemaVersion string       `json:"schema_version"`
	PolicyID      string       `json:"policy_id"`
	Revision      string       `json:"revision"`
	Rules         []MetricRule `json:"rules"`
}

type BuildRequest struct {
	SchemaVersion         string    `json:"schema_version"`
	ObservationID         string    `json:"observation_id"`
	PlanID                string    `json:"plan_id"`
	BaselineSnapshotID    string    `json:"baseline_snapshot_id"`
	ObservationSnapshotID string    `json:"observation_snapshot_id"`
	Policy                Policy    `json:"policy"`
	ObservedAt            time.Time `json:"observed_at"`
}

type PlanBinding struct {
	PlanID                      string    `json:"plan_id"`
	PlanSHA256                  string    `json:"plan_sha256"`
	ActivationAt                time.Time `json:"activation_at"`
	CalibrationRunID            string    `json:"calibration_run_id"`
	CalibrationRunSHA256        string    `json:"calibration_run_sha256"`
	BaselineReviewRunID         string    `json:"baseline_review_run_id"`
	BaselineBundleSHA256        string    `json:"baseline_bundle_sha256"`
	VariantBundleSHA256         string    `json:"variant_bundle_sha256"`
	VariantConfigRevisionID     string    `json:"variant_config_revision_id"`
	VariantConfigRevision       string    `json:"variant_config_revision"`
	VariantConfigRevisionSHA256 string    `json:"variant_config_revision_sha256"`
	PromotionVariantID          string    `json:"promotion_variant_id"`
	PromotionPolicyRevision     string    `json:"promotion_policy_revision"`
}

type SnapshotBinding struct {
	SnapshotID string                    `json:"snapshot_id"`
	SHA256     string                    `json:"sha256"`
	Scope      analyticsadapter.Scope    `json:"scope"`
	Window     analytics.TimeWindow      `json:"window"`
	GroupBy    []analytics.DimensionName `json:"group_by"`
	BuiltAt    time.Time                 `json:"built_at"`
}

type MetricAvailability string

const (
	MetricEvaluated        MetricAvailability = "evaluated"
	MetricInsufficientData MetricAvailability = "insufficient_data"
	MetricUnavailable      MetricAvailability = "unavailable"
)

type MetricValue struct {
	Numerator   uint64  `json:"numerator"`
	Denominator uint64  `json:"denominator"`
	RatePPM     *uint64 `json:"rate_ppm,omitempty"`
}

type MetricComparison struct {
	MetricID      MetricID                  `json:"metric_id"`
	Definition    string                    `json:"definition"`
	Direction     analytics.MetricDirection `json:"direction"`
	Baseline      MetricValue               `json:"baseline"`
	Observation   MetricValue               `json:"observation"`
	DeltaPPM      *int64                    `json:"delta_ppm,omitempty"`
	RegressionPPM *uint64                   `json:"regression_ppm,omitempty"`
	Availability  MetricAvailability        `json:"availability"`
	Regression    bool                      `json:"regression"`
	ReasonCodes   []string                  `json:"reason_codes"`
}

type Status string

const (
	StatusHealthy          Status = "healthy"
	StatusRegressed        Status = "regressed"
	StatusInsufficientData Status = "insufficient_data"
	StatusUnavailable      Status = "unavailable"
)

type Observation struct {
	SchemaVersion          string             `json:"schema_version"`
	Request                BuildRequest       `json:"request"`
	Plan                   PlanBinding        `json:"plan_binding"`
	BaselineSnapshot       SnapshotBinding    `json:"baseline_snapshot"`
	ObservationSnapshot    SnapshotBinding    `json:"observation_snapshot"`
	Metrics                []MetricComparison `json:"metrics"`
	Status                 Status             `json:"status"`
	RollbackRecommendation bool               `json:"rollback_recommendation"`
	ReasonCodes            []string           `json:"reason_codes"`
	ObservedBy             string             `json:"observed_by"`
	Audit                  string             `json:"audit"`
	IdempotencyKey         string             `json:"idempotency_key"`
}

type Summary struct {
	ObservationID          string    `json:"observation_id"`
	PlanID                 string    `json:"plan_id"`
	Status                 Status    `json:"status"`
	RollbackRecommendation bool      `json:"rollback_recommendation"`
	ObservationSHA256      string    `json:"observation_sha256"`
	ObservedAt             time.Time `json:"observed_at"`
}

type manifest struct {
	SchemaVersion          string    `json:"schema_version"`
	ObservationID          string    `json:"observation_id"`
	ObservationSHA256      string    `json:"observation_sha256"`
	ArtifactURI            string    `json:"artifact_uri"`
	SizeBytes              int64     `json:"size_bytes"`
	PlanID                 string    `json:"plan_id"`
	Status                 Status    `json:"status"`
	RollbackRecommendation bool      `json:"rollback_recommendation"`
	ObservedAt             time.Time `json:"observed_at"`
}
