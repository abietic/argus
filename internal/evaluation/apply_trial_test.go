package evaluation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runmodel"
)

func TestApplyTrialValidationFailsClosed(t *testing.T) {
	valid := testApplyTrial("apply-case", "review-run-apply", "finding-apply", testEpoch)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid ApplyTrial error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ApplyTrial)
	}{
		{"missing checks", func(value *ApplyTrial) { value.Checks = []ApplyCheck{} }},
		{"wrong check order", func(value *ApplyTrial) { value.Checks[0].Kind = ApplyCheckCompile }},
		{"check after failure", func(value *ApplyTrial) { value.Checks[0].Status = ApplyCheckFailed }},
		{"incomplete successful conclusion", func(value *ApplyTrial) { value.Checks = value.Checks[:2] }},
		{"partial conclusive failure", func(value *ApplyTrial) {
			value.Completeness = ApplyTrialPartial
			value.ReasonCodes = []string{"worker_interrupted"}
			value.Checks[0].Status = ApplyCheckFailed
			value.Checks = value.Checks[:1]
		}},
		{"wrong edit contract", func(value *ApplyTrial) { value.EditScriptRef.Contract = "text/plain" }},
		{"unattested authority drift", func(value *ApplyTrial) { value.Authority = "platform_attested" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Checks = append([]ApplyCheck(nil), valid.Checks...)
			candidate.ReasonCodes = append([]string(nil), valid.ReasonCodes...)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("ApplyTrial.Validate() accepted invalid evidence")
			}
		})
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(data), `"trial_id":`, `"raw_command":"secret","trial_id":`, 1)
	if _, err := DecodeApplyTrial([]byte(unknown)); err == nil {
		t.Fatal("DecodeApplyTrial() accepted unknown raw command")
	}
}

func testApplyTrial(caseID, runID, findingID string, executedAt time.Time) ApplyTrial {
	checks := make([]ApplyCheck, 0, 3)
	for _, kind := range []ApplyCheckKind{ApplyCheckDryRun, ApplyCheckCompile, ApplyCheckTest} {
		checks = append(checks, ApplyCheck{
			Kind: kind, Status: ApplyCheckPassed,
			CommandSHA256: evaluationDigest("command-" + string(kind)),
			EvidenceRef:   evaluationArtifactRef("evidence-"+string(kind), ApplyCheckContract),
			DurationMS:    10,
		})
	}
	return ApplyTrial{
		SchemaVersion: ApplyTrialSchemaVersion, TrialID: "trial-1",
		CaseID: caseID, LabelRevision: 1, ReviewRunID: runID,
		FindingID: findingID, FindingFingerprint: evaluationDigest("finding-fingerprint"),
		InputSnapshotRef:  evaluationArtifactRef("target", runmodel.ContractMaterializedTarget),
		GovernedReportRef: evaluationArtifactRef("report", runmodel.ContractGovernedReviewReport),
		SuggestionSHA256:  evaluationDigest("suggestion"),
		EditScriptRef:     evaluationArtifactRef("edit-script", ApplyEditScriptContract),
		Authority:         ApplyTrialAuthority, Completeness: ApplyTrialConclusive,
		ReasonCodes: []string{}, Checks: checks, ExecutedAt: executedAt,
	}
}
