package evaluation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"argus.local/argus/internal/runmodel"
)

func testDigest(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func TestMissedDefectIncidentLedgerIsAppendOnlyAndRejectsBranchingCorrections(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	incident := testMissedDefectIncident()
	mutation := testMutation("incident-event-1", incident.RecordedAt, RoleIncidentIngest)
	mutation.Actor = "incident-service"
	entry, err := repository.RecordMissedDefectIncident(context.Background(), incident, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := repository.RecordMissedDefectIncident(context.Background(), incident, mutation); err != nil || retry.EventID != entry.EventID {
		t.Fatalf("idempotent incident = %+v, %v", retry, err)
	}
	correction := incident
	correction.IncidentID = "incident-2"
	correction.PriorIncidentID = incident.IncidentID
	correction.RecordedAt = correction.RecordedAt.Add(1)
	correction.DefectFingerprint = testDigest("corrected-incident")
	correctionMutation := testMutation("incident-event-2", correction.RecordedAt, RoleIncidentIngest)
	if _, err := repository.RecordMissedDefectIncident(context.Background(), correction, correctionMutation); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ResolveMissedDefectIncident(incident.IncidentID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("ResolveMissedDefectIncident(superseded) error = %v", err)
	}
	branch := correction
	branch.IncidentID = "incident-3"
	branch.RecordedAt = branch.RecordedAt.Add(1)
	if _, err := repository.RecordMissedDefectIncident(context.Background(), branch,
		testMutation("incident-event-3", branch.RecordedAt, RoleIncidentIngest)); !errors.Is(err, ErrConflict) {
		t.Fatalf("RecordMissedDefectIncident(branch) error = %v", err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := restarted.ResolveMissedDefectIncident(correction.IncidentID)
	if err != nil || resolved.DefectFingerprint != correction.DefectFingerprint {
		t.Fatalf("resolved correction = %+v, %v", resolved, err)
	}
	listed, err := restarted.ListMissedDefectIncidents(Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListMissedDefectIncidents() = %+v, %v", listed, err)
	}
}

func testMissedDefectIncident() MissedDefectIncident {
	at := testEpoch.Add(10)
	evidenceDigest := testDigest("evidence")
	return MissedDefectIncident{
		SchemaVersion: MissedDefectIncidentSchemaVersion,
		IncidentID:    "incident-1", ReviewRunID: "review-run-1",
		DefectFingerprint: testDigest("incident-defect"), Category: "correctness", Severity: "high",
		Anchors: []LabelAnchor{{Path: "main.go", Side: "new", StartLine: 10, EndLine: 11, SourceDigest: testDigest("source")}},
		EvidenceArtifacts: []runmodel.ArtifactRef{{
			URI: "artifact://local/sha256/" + evidenceDigest, SHA256: evidenceDigest, SizeBytes: 8,
			Contract: "argus.incident_evidence.v1alpha1",
		}},
		SourceAuthority: "pager-system", SourceID: "INC-42", SourceRevision: "revision-1",
		OccurredAt: at, RecordedAt: at,
	}
}
