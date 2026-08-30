package evaluation

import (
	"testing"
	"time"
)

func TestCandidateDerivationRequestFixRunIsSourceSpecific(t *testing.T) {
	request := CandidateDerivationRequest{
		SchemaVersion: CandidateDerivationRequestSchemaVersion,
		CaseID:        "case-1", RunID: "defect-run-1", FindingID: "finding-1",
		Source: CandidateFromReviewedBugFixPair, SourceID: "outcome-1", FixRunID: "fix-run-1",
		LicenseID: "license-1", Consent: ConsentAuthorizedInternal,
		Classification: ClassificationInternal, Owner: "evaluation-team",
		LabelPolicyRevision: "label-policy-1", Restrictions: []string{},
		CollectedAt: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate(reviewed bug-fix pair) error = %v", err)
	}
	missing := request
	missing.FixRunID = ""
	if err := missing.Validate(); err == nil {
		t.Fatal("Validate(reviewed bug-fix pair without fix run) unexpectedly succeeded")
	}
	unrelated := request
	unrelated.Source = CandidateFromHumanDecision
	if err := unrelated.Validate(); err == nil {
		t.Fatal("Validate(human decision with fix run) unexpectedly succeeded")
	}
	sameRun := request
	sameRun.FixRunID = sameRun.RunID
	if err := sameRun.Validate(); err == nil {
		t.Fatal("Validate(same defect/fix run) unexpectedly succeeded")
	}
	incident := request
	incident.Source = CandidateFromMissedDefectIncident
	incident.FindingID = ""
	incident.FixRunID = ""
	if err := incident.Validate(); err != nil {
		t.Fatalf("Validate(incident missed defect) error = %v", err)
	}
	incident.FindingID = "finding-must-not-exist"
	if err := incident.Validate(); err == nil {
		t.Fatal("Validate(incident with finding_id) unexpectedly succeeded")
	}
}
