package calibrationpromotion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/reviewconfig"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (request PrepareRequest) Validate() error {
	if request.SchemaVersion != PrepareRequestSchemaVersion {
		return fmt.Errorf("unsupported prepare request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"plan_id": request.PlanID, "calibration_run_id": request.CalibrationRunID,
		"baseline_review_run_id":      request.BaselineReviewRunID,
		"baseline_config_revision_id": request.BaselineConfigRevisionID,
		"baseline_config_revision":    request.BaselineConfigRevision,
		"variant_config_revision_id":  request.VariantConfigRevisionID,
		"variant_config_revision":     request.VariantConfigRevision,
		"promotion_variant_id":        request.PromotionVariantID,
		"promotion_policy_revision":   request.PromotionPolicyRevision,
	} {
		if !idPattern.MatchString(value) || strings.EqualFold(value, "latest") {
			return fmt.Errorf("%s must be an explicit stable identifier", name)
		}
	}
	if !shaPattern.MatchString(request.ExpectedCalibrationRunSHA256) {
		return fmt.Errorf("expected_calibration_run_sha256 must be lowercase SHA-256")
	}
	if request.BaselineConfigRevisionID == request.VariantConfigRevisionID && request.BaselineConfigRevision == request.VariantConfigRevision {
		return fmt.Errorf("variant config revision must differ from baseline")
	}
	if strings.TrimSpace(request.Owner) == "" || len(request.Owner) > 256 || strings.ContainsAny(request.Owner, "\r\n") {
		return fmt.Errorf("owner is invalid")
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func (request GateRequest) Validate() error {
	if request.SchemaVersion != GateRequestSchemaVersion {
		return fmt.Errorf("unsupported gate request schema %q", request.SchemaVersion)
	}
	if !idPattern.MatchString(request.PlanID) {
		return fmt.Errorf("plan_id is invalid")
	}
	if err := request.Result.Validate(); err != nil {
		return err
	}
	if request.Result.Gate == "targeted_regression" {
		if !idPattern.MatchString(request.ExperimentRunID) {
			return fmt.Errorf("targeted_regression requires experiment_run_id")
		}
	} else if request.ExperimentRunID != "" {
		return fmt.Errorf("experiment_run_id is only valid for targeted_regression")
	}
	return nil
}

func (plan Plan) Validate() error {
	if plan.SchemaVersion != PlanSchemaVersion {
		return fmt.Errorf("unsupported plan schema %q", plan.SchemaVersion)
	}
	if err := plan.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	for name, value := range map[string]string{
		"calibration_run_sha256": plan.CalibrationRunSHA256, "profile_candidate_sha256": plan.ProfileCandidateSHA256,
		"calibration_report_sha256": plan.CalibrationReportSHA256, "baseline_bundle_sha256": plan.BaselineBundleSHA256,
		"variant_bundle_sha256": plan.VariantBundleSHA256, "baseline_config_revision_sha256": plan.BaselineConfigSHA256,
		"variant_config_revision_sha256": plan.VariantConfigSHA256,
	} {
		if !shaPattern.MatchString(value) {
			return fmt.Errorf("%s must be lowercase SHA-256", name)
		}
	}
	if !idPattern.MatchString(plan.ProfileCandidateID) {
		return fmt.Errorf("profile_candidate_id is invalid")
	}
	if err := plan.VariantConfig.Validate(); err != nil {
		return fmt.Errorf("variant config: %w", err)
	}
	digest, err := reviewconfig.DigestRevision(plan.VariantConfig)
	if err != nil || digest != plan.VariantConfigSHA256 {
		return fmt.Errorf("variant config digest mismatch")
	}
	if err := plan.PromotionVariant.Validate(); err != nil {
		return fmt.Errorf("promotion variant: %w", err)
	}
	binding := plan.PromotionVariant.ManagedBinding
	if binding == nil || binding.CalibrationRunID != plan.Request.CalibrationRunID || binding.CalibrationRunSHA256 != plan.CalibrationRunSHA256 || binding.ProfileCandidateID != plan.ProfileCandidateID || binding.ProfileCandidateSHA256 != plan.ProfileCandidateSHA256 || binding.CalibrationReportSHA256 != plan.CalibrationReportSHA256 || binding.BaselineBundleSHA256 != plan.BaselineBundleSHA256 || binding.ConfigRevisionID != plan.VariantConfig.ID || binding.ConfigRevisionSHA256 != plan.VariantConfigSHA256 {
		return fmt.Errorf("managed binding does not match plan")
	}
	switch plan.Status {
	case StatusPreparing, StatusPrepared, StatusGating, StatusGatesPassed, StatusFailed, StatusInconclusive, StatusActivating, StatusActive, StatusRollbackPending, StatusRolledBack:
	default:
		return fmt.Errorf("unsupported plan status %q", plan.Status)
	}
	switch plan.ConfigStatus {
	case configrepo.StatusDraft, configrepo.StatusValidated, configrepo.StatusPublished, configrepo.StatusRolledBack:
	default:
		return fmt.Errorf("unsupported plan config status %q", plan.ConfigStatus)
	}
	switch plan.PromotionStatus {
	case evaluation.PromotionRegistered, evaluation.PromotionInProgress, evaluation.PromotionFailed, evaluation.PromotionInconclusive, evaluation.PromotionActive, evaluation.PromotionRolledBack:
	default:
		return fmt.Errorf("unsupported plan promotion status %q", plan.PromotionStatus)
	}
	if plan.Status == StatusPrepared && (plan.ConfigStatus != configrepo.StatusValidated || plan.PromotionStatus != evaluation.PromotionRegistered || plan.NextGate == nil || *plan.NextGate != evaluation.GateSchemaContract) {
		return fmt.Errorf("prepared plan projection is inconsistent")
	}
	if plan.Status == StatusGatesPassed && (plan.ConfigStatus != configrepo.StatusValidated || plan.PromotionStatus != evaluation.PromotionActive || plan.NextGate != nil) {
		return fmt.Errorf("gates_passed plan projection is inconsistent")
	}
	if plan.Status == StatusFailed && (plan.ConfigStatus != configrepo.StatusValidated || plan.PromotionStatus != evaluation.PromotionFailed || plan.NextGate != nil) {
		return fmt.Errorf("failed plan projection is inconsistent")
	}
	if plan.Status == StatusInconclusive && (plan.ConfigStatus != configrepo.StatusValidated || plan.PromotionStatus != evaluation.PromotionInconclusive || plan.NextGate != nil) {
		return fmt.Errorf("inconclusive plan projection is inconsistent")
	}
	if plan.Status == StatusActive && (plan.ConfigStatus != configrepo.StatusPublished || plan.PromotionStatus != evaluation.PromotionActive || plan.Rollout == nil) {
		return fmt.Errorf("active plan projection is inconsistent")
	}
	if plan.Status == StatusRolledBack && (plan.ConfigStatus != configrepo.StatusRolledBack || plan.PromotionStatus != evaluation.PromotionRolledBack) {
		return fmt.Errorf("rolled_back plan projection is inconsistent")
	}
	if plan.Rollout != nil {
		if err := plan.Rollout.Validate(); err != nil {
			return err
		}
	}
	if plan.UpdatedAt.IsZero() || plan.UpdatedAt.Location() != time.UTC || strings.TrimSpace(plan.UpdatedBy) == "" {
		return fmt.Errorf("plan update audit is invalid")
	}
	return nil
}

func decodeStrict[T any](data []byte, label string, validate func(T) error) (T, error) {
	var value T
	if err := rejectDuplicateFields(data); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return value, fmt.Errorf("decode %s: multiple JSON values", label)
		}
		return value, fmt.Errorf("decode %s trailing JSON: %w", label, err)
	}
	if err := validate(value); err != nil {
		return value, fmt.Errorf("validate %s: %w", label, err)
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

func DecodePrepareRequest(data []byte) (PrepareRequest, error) {
	return decodeStrict(data, "CalibrationPromotionPrepareRequest", func(v PrepareRequest) error { return v.Validate() })
}

func DecodeGateRequest(data []byte) (GateRequest, error) {
	return decodeStrict(data, "CalibrationPromotionGateRequest", func(v GateRequest) error { return v.Validate() })
}
