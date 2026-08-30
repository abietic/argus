// Package calibrationpromotion owns the explicit bridge from one passed
// calibration candidate to the configuration and promotion lifecycles. It is
// deliberately separate from fitting: fitting never publishes configuration.
package calibrationpromotion

import (
	"errors"
	"time"

	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/reviewconfig"
)

const (
	PrepareRequestSchemaVersion = "argus.calibration_promotion_prepare_request.v1alpha1"
	GateRequestSchemaVersion    = "argus.calibration_promotion_gate_request.v1alpha1"
	PlanSchemaVersion           = "argus.calibration_promotion_plan.v1alpha1"

	planStream      = "calibration/promotion-plans"
	planEventSchema = "argus.calibration_promotion_plan_event.v1alpha1"
)

var (
	ErrNotFound          = errors.New("calibration promotion plan not found")
	ErrConflict          = errors.New("calibration promotion conflict")
	ErrUnauthorized      = errors.New("calibration promotion unauthorized")
	ErrInvalidTransition = errors.New("invalid calibration promotion transition")
	ErrEvidenceMismatch  = errors.New("calibration promotion evidence mismatch")
	ErrCorrupt           = errors.New("corrupt calibration promotion repository")
)

type Status string

const (
	StatusPreparing       Status = "preparing"
	StatusPrepared        Status = "prepared"
	StatusGating          Status = "gating"
	StatusGatesPassed     Status = "gates_passed"
	StatusFailed          Status = "failed"
	StatusInconclusive    Status = "inconclusive"
	StatusActivating      Status = "activating"
	StatusActive          Status = "active"
	StatusRollbackPending Status = "rollback_pending"
	StatusRolledBack      Status = "rolled_back"
)

type PrepareRequest struct {
	SchemaVersion                string    `json:"schema_version"`
	PlanID                       string    `json:"plan_id"`
	CalibrationRunID             string    `json:"calibration_run_id"`
	ExpectedCalibrationRunSHA256 string    `json:"expected_calibration_run_sha256"`
	BaselineReviewRunID          string    `json:"baseline_review_run_id"`
	BaselineConfigRevisionID     string    `json:"baseline_config_revision_id"`
	BaselineConfigRevision       string    `json:"baseline_config_revision"`
	VariantConfigRevisionID      string    `json:"variant_config_revision_id"`
	VariantConfigRevision        string    `json:"variant_config_revision"`
	PromotionVariantID           string    `json:"promotion_variant_id"`
	PromotionPolicyRevision      string    `json:"promotion_policy_revision"`
	Owner                        string    `json:"owner"`
	CreatedAt                    time.Time `json:"created_at"`
}

// GateRequest carries only a governed gate result plus the typed evidence
// handle needed for gates whose exact source cannot be inferred from it.
type GateRequest struct {
	SchemaVersion   string                `json:"schema_version"`
	PlanID          string                `json:"plan_id"`
	Result          evaluation.GateResult `json:"result"`
	ExperimentRunID string                `json:"experiment_run_id,omitempty"`
}

type Plan struct {
	SchemaVersion           string                      `json:"schema_version"`
	Request                 PrepareRequest              `json:"request"`
	Status                  Status                      `json:"status"`
	CalibrationRunSHA256    string                      `json:"calibration_run_sha256"`
	ProfileCandidateID      string                      `json:"profile_candidate_id"`
	ProfileCandidateSHA256  string                      `json:"profile_candidate_sha256"`
	CalibrationReportSHA256 string                      `json:"calibration_report_sha256"`
	BaselineBundleSHA256    string                      `json:"baseline_bundle_sha256"`
	VariantBundleSHA256     string                      `json:"variant_bundle_sha256"`
	BaselineConfigSHA256    string                      `json:"baseline_config_revision_sha256"`
	VariantConfig           reviewconfig.Revision       `json:"variant_config_revision"`
	VariantConfigSHA256     string                      `json:"variant_config_revision_sha256"`
	PromotionVariant        evaluation.PromotionVariant `json:"promotion_variant"`
	PromotionStatus         evaluation.PromotionStatus  `json:"promotion_status"`
	NextGate                *evaluation.PromotionGate   `json:"next_gate,omitempty"`
	ConfigStatus            configrepo.Status           `json:"config_status"`
	Rollout                 *configrepo.Rollout         `json:"rollout,omitempty"`
	UpdatedAt               time.Time                   `json:"updated_at"`
	UpdatedBy               string                      `json:"updated_by"`
}

type planEvent struct {
	SchemaVersion string    `json:"schema_version"`
	Type          string    `json:"type"`
	Plan          Plan      `json:"plan"`
	Actor         string    `json:"actor"`
	Audit         string    `json:"audit"`
	OccurredAt    time.Time `json:"occurred_at"`
}
