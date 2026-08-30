package evaluation

import (
	"fmt"
	"slices"
	"time"

	"github.com/abietic/argus/internal/runmodel"
)

const (
	ApplyTrialSchemaVersion = "argus.apply_trial.v1alpha1"
	ApplyTrialAuthority     = "local_host_unattested"
	ApplyEditScriptContract = "argus.edit_script.v1alpha1"
	ApplyCheckContract      = "argus.apply_check_evidence.v1alpha1"
)

type ApplyTrialCompleteness string

const (
	ApplyTrialConclusive ApplyTrialCompleteness = "conclusive"
	ApplyTrialPartial    ApplyTrialCompleteness = "partial"
)

type ApplyCheckKind string

const (
	ApplyCheckDryRun  ApplyCheckKind = "dry_run"
	ApplyCheckCompile ApplyCheckKind = "compile"
	ApplyCheckTest    ApplyCheckKind = "test"
)

type ApplyCheckStatus string

const (
	ApplyCheckPassed ApplyCheckStatus = "passed"
	ApplyCheckFailed ApplyCheckStatus = "failed"
)

type ApplyCheck struct {
	Kind          ApplyCheckKind       `json:"kind"`
	Status        ApplyCheckStatus     `json:"status"`
	CommandSHA256 string               `json:"command_sha256"`
	EvidenceRef   runmodel.ArtifactRef `json:"evidence_ref"`
	DurationMS    uint64               `json:"duration_ms"`
}

// ApplyTrial is a local, immutable execution receipt for an edit derived from
// one exact governed Finding suggestion. It is deliberately marked
// unattested: Hailix/provider execution authority must use a future contract.
type ApplyTrial struct {
	SchemaVersion      string                 `json:"schema_version"`
	TrialID            string                 `json:"trial_id"`
	CaseID             string                 `json:"case_id"`
	LabelRevision      uint64                 `json:"label_revision"`
	ReviewRunID        string                 `json:"review_run_id"`
	FindingID          string                 `json:"finding_id"`
	FindingFingerprint string                 `json:"finding_fingerprint"`
	InputSnapshotRef   runmodel.ArtifactRef   `json:"input_snapshot_ref"`
	GovernedReportRef  runmodel.ArtifactRef   `json:"governed_report_ref"`
	SuggestionSHA256   string                 `json:"suggestion_sha256"`
	EditScriptRef      runmodel.ArtifactRef   `json:"edit_script_ref"`
	Authority          string                 `json:"authority"`
	Completeness       ApplyTrialCompleteness `json:"completeness"`
	ReasonCodes        []string               `json:"reason_codes"`
	Checks             []ApplyCheck           `json:"checks"`
	ExecutedAt         time.Time              `json:"executed_at"`
}

func (trial ApplyTrial) Validate() error {
	if trial.SchemaVersion != ApplyTrialSchemaVersion {
		return fmt.Errorf("unsupported apply trial schema %q", trial.SchemaVersion)
	}
	for name, value := range map[string]string{
		"trial_id": trial.TrialID, "case_id": trial.CaseID,
		"review_run_id": trial.ReviewRunID, "finding_id": trial.FindingID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if trial.LabelRevision == 0 {
		return fmt.Errorf("label_revision must be positive")
	}
	if err := validateSHA256("finding_fingerprint", trial.FindingFingerprint); err != nil {
		return err
	}
	if err := validateSHA256("suggestion_sha256", trial.SuggestionSHA256); err != nil {
		return err
	}
	for name, item := range map[string]struct {
		ref      runmodel.ArtifactRef
		contract string
	}{
		"input_snapshot_ref":  {trial.InputSnapshotRef, runmodel.ContractMaterializedTarget},
		"governed_report_ref": {trial.GovernedReportRef, runmodel.ContractGovernedReviewReport},
		"edit_script_ref":     {trial.EditScriptRef, ApplyEditScriptContract},
	} {
		if err := item.ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if item.ref.Contract != item.contract {
			return fmt.Errorf("%s contract is %q, want %q", name, item.ref.Contract, item.contract)
		}
	}
	if trial.Authority != ApplyTrialAuthority {
		return fmt.Errorf("unsupported apply trial authority %q", trial.Authority)
	}
	if trial.ExecutedAt.IsZero() {
		return fmt.Errorf("executed_at is required")
	}
	if trial.Checks == nil || len(trial.Checks) == 0 || trial.ReasonCodes == nil {
		return fmt.Errorf("checks must be non-empty and reason_codes must be an explicit array")
	}
	order := []ApplyCheckKind{ApplyCheckDryRun, ApplyCheckCompile, ApplyCheckTest}
	if len(trial.Checks) > len(order) {
		return fmt.Errorf("checks exceed the closed dry_run/compile/test sequence")
	}
	failed := false
	for index, check := range trial.Checks {
		if failed {
			return fmt.Errorf("checks must stop after the first failed check")
		}
		if check.Kind != order[index] {
			return fmt.Errorf("checks[%d].kind is %q, want %q", index, check.Kind, order[index])
		}
		switch check.Status {
		case ApplyCheckPassed:
		case ApplyCheckFailed:
			failed = true
		default:
			return fmt.Errorf("checks[%d] has unsupported status %q", index, check.Status)
		}
		if err := validateSHA256("check.command_sha256", check.CommandSHA256); err != nil {
			return fmt.Errorf("checks[%d]: %w", index, err)
		}
		if err := check.EvidenceRef.Validate(); err != nil {
			return fmt.Errorf("checks[%d].evidence_ref: %w", index, err)
		}
		if check.EvidenceRef.Contract != ApplyCheckContract {
			return fmt.Errorf("checks[%d].evidence_ref has unsupported contract %q",
				index, check.EvidenceRef.Contract)
		}
	}
	if !slices.IsSorted(trial.ReasonCodes) {
		return fmt.Errorf("reason_codes must be sorted")
	}
	for index, reason := range trial.ReasonCodes {
		if err := validateID("reason_codes", reason); err != nil {
			return err
		}
		if index > 0 && reason == trial.ReasonCodes[index-1] {
			return fmt.Errorf("reason_codes must be unique")
		}
	}
	switch trial.Completeness {
	case ApplyTrialConclusive:
		if len(trial.ReasonCodes) != 0 {
			return fmt.Errorf("conclusive apply trial forbids reason_codes")
		}
		if !failed && len(trial.Checks) != len(order) {
			return fmt.Errorf("conclusive successful apply trial requires all checks")
		}
	case ApplyTrialPartial:
		if len(trial.ReasonCodes) == 0 {
			return fmt.Errorf("partial apply trial requires reason_codes")
		}
		if failed || len(trial.Checks) == len(order) {
			return fmt.Errorf("partial apply trial must be an unfinished all-passing prefix")
		}
	default:
		return fmt.Errorf("unsupported apply trial completeness %q", trial.Completeness)
	}
	return nil
}

func DecodeApplyTrial(data []byte) (ApplyTrial, error) {
	return decodeStrict(data, "ApplyTrial", func(value ApplyTrial) error {
		return value.Validate()
	})
}
