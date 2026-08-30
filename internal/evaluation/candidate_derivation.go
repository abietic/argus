package evaluation

import (
	"fmt"
	"time"
)

const CandidateDerivationRequestSchemaVersion = "argus.evaluation_candidate_derivation_request.v1alpha1"

type CandidateDerivationSource string

const (
	CandidateFromHumanDecision        CandidateDerivationSource = "human_decision"
	CandidateFromProductionFeedback   CandidateDerivationSource = "production_feedback"
	CandidateFromReviewedBugFixPair   CandidateDerivationSource = "reviewed_bug_fix_pair"
	CandidateFromMissedDefectIncident CandidateDerivationSource = "incident_missed_defect"
	CandidateFromMutationProbe        CandidateDerivationSource = "mutation_probe"
	CandidateFromSyntheticProbe       CandidateDerivationSource = "synthetic_probe"
	CandidateFromWorkflowProbe        CandidateDerivationSource = "workflow_invariant_probe"
)

// CandidateDerivationRequest contains only governance facts that cannot be
// inferred from a ReviewRun. It deliberately has no dataset state, split,
// eligibility, allowed-use, label, anchor, repository, or evidence-ref fields:
// the control plane derives those server-side and always creates a pending,
// unassigned, candidate-only case.
type CandidateDerivationRequest struct {
	SchemaVersion       string                    `json:"schema_version"`
	CaseID              string                    `json:"case_id"`
	RunID               string                    `json:"run_id"`
	FindingID           string                    `json:"finding_id"`
	Source              CandidateDerivationSource `json:"source"`
	SourceID            string                    `json:"source_id"`
	FixRunID            string                    `json:"fix_run_id,omitempty"`
	LicenseID           string                    `json:"license_id"`
	Consent             ConsentBasis              `json:"consent"`
	Classification      Classification            `json:"classification"`
	Owner               string                    `json:"owner"`
	LabelPolicyRevision string                    `json:"label_policy_revision"`
	Restrictions        []string                  `json:"restrictions"`
	CollectedAt         time.Time                 `json:"collected_at"`
}

func (request CandidateDerivationRequest) Validate() error {
	if request.SchemaVersion != CandidateDerivationRequestSchemaVersion {
		return fmt.Errorf("unsupported candidate derivation request schema %q", request.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{name: "case_id", value: request.CaseID},
		{name: "run_id", value: request.RunID},
		{name: "source_id", value: request.SourceID},
		{name: "label_policy_revision", value: request.LabelPolicyRevision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	switch request.Source {
	case CandidateFromHumanDecision, CandidateFromProductionFeedback:
		if err := validateID("finding_id", request.FindingID); err != nil {
			return err
		}
		if request.FixRunID != "" {
			return fmt.Errorf("fix_run_id is only valid for reviewed_bug_fix_pair")
		}
	case CandidateFromReviewedBugFixPair:
		if err := validateID("finding_id", request.FindingID); err != nil {
			return err
		}
		if err := validateID("fix_run_id", request.FixRunID); err != nil {
			return err
		}
		if request.FixRunID == request.RunID {
			return fmt.Errorf("fix_run_id must differ from the defect run_id")
		}
	case CandidateFromMissedDefectIncident:
		if request.FindingID != "" || request.FixRunID != "" {
			return fmt.Errorf("incident_missed_defect must not set finding_id or fix_run_id")
		}
	case CandidateFromMutationProbe, CandidateFromSyntheticProbe, CandidateFromWorkflowProbe:
		if request.FindingID != "" || request.FixRunID != "" {
			return fmt.Errorf("probe source must not set finding_id or fix_run_id")
		}
	default:
		return fmt.Errorf("unsupported candidate derivation source %q", request.Source)
	}
	if err := validateText("license_id", request.LicenseID, 512, false); err != nil {
		return err
	}
	if err := request.Consent.Validate(); err != nil {
		return err
	}
	if err := request.Classification.Validate(); err != nil {
		return err
	}
	if err := validateText("owner", request.Owner, 256, false); err != nil {
		return err
	}
	if request.Restrictions == nil {
		return fmt.Errorf("restrictions must be an array")
	}
	for index, restriction := range request.Restrictions {
		if err := validateText(fmt.Sprintf("restrictions[%d]", index), restriction, 1024, true); err != nil {
			return err
		}
		if index > 0 && request.Restrictions[index-1] >= restriction {
			return fmt.Errorf("restrictions must be sorted and unique")
		}
	}
	if request.CollectedAt.IsZero() || request.CollectedAt.Location() != time.UTC {
		return fmt.Errorf("collected_at must be non-zero UTC")
	}
	return nil
}

func DecodeCandidateDerivationRequest(data []byte) (CandidateDerivationRequest, error) {
	return decodeStrict(data, "CandidateDerivationRequest", func(value CandidateDerivationRequest) error {
		return value.Validate()
	})
}
