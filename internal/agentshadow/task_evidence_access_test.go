package agentshadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/artifactrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestTaskEvidenceGenericJSONIsRedactedAndDisclosureIsAudited(t *testing.T) {
	fixture := newShadowFixture(t)
	imported, err := fixture.service.Import(t.Context(), fixture.request(t, "sensitive-governance"))
	if err != nil {
		t.Fatal(err)
	}
	generic, err := json.Marshal(imported)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(generic), `"task_evidence_collection"`) ||
		strings.Contains(string(generic), `"task_executions"`) ||
		!strings.Contains(string(generic), `"export_policy":"deny"`) {
		t.Fatalf("generic JSON did not redact exact evidence: %s", generic)
	}
	redacted, err := fixture.service.QueryRedacted(t.Context(), QueryRequest{
		Scope: fixture.scope, ManifestID: imported.Manifest.ManifestID,
	})
	if err != nil || redacted.TaskEvidenceCollection.SchemaVersion != "" ||
		redacted.TaskEvidence.Ref != imported.Record.TaskEvidenceCollectionRef {
		t.Fatalf("QueryRedacted() = %+v, %v", redacted, err)
	}
	access := artifactrepo.SensitiveAccessRequest{
		RequestID: "task-evidence-read-1", Actor: "local-review-debugger",
		Purpose: artifactrepo.SensitiveAccessLocalDebug,
		At:      fixture.now.Add(time.Minute),
	}
	*fixture.now = access.At
	read, err := fixture.service.ReadTaskEvidence(t.Context(), TaskEvidenceReadRequest{
		Scope: fixture.scope, ManifestID: imported.Manifest.ManifestID, Access: access,
	})
	if err != nil || read.Evidence.SchemaVersion != contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion ||
		read.Proof.AuditSequence != 1 || read.Proof.Receipt.Purpose != access.Purpose {
		t.Fatalf("ReadTaskEvidence() = %+v, %v", read, err)
	}
	replay, err := fixture.service.ReadTaskEvidence(t.Context(), TaskEvidenceReadRequest{
		Scope: fixture.scope, ManifestID: imported.Manifest.ManifestID, Access: access,
	})
	if err != nil || replay.Proof != read.Proof {
		t.Fatalf("idempotent ReadTaskEvidence() = %+v, %v", replay, err)
	}
}

func TestTaskEvidenceRevocationIsIrreversibleButHistoryRemainsQueryable(t *testing.T) {
	fixture := newShadowFixture(t)
	imported, err := fixture.service.Import(t.Context(), fixture.request(t, "retention-revoke"))
	if err != nil {
		t.Fatal(err)
	}
	ref := imported.Record.TaskEvidenceCollectionRef
	contentPath := filepath.Join(
		fixture.store.Root(), "artifacts", "sha256", ref.Ref.SHA256[:2], ref.Ref.SHA256,
	)
	at := fixture.now.Add(2 * time.Minute)
	request := TaskEvidenceRevokeRequest{
		Scope: fixture.scope, ManifestID: imported.Manifest.ManifestID,
		IdempotencyKey: "revoke-task-evidence-1", Actor: "retention-operator",
		Reason: "local retention period ended", At: at,
	}
	summary, err := fixture.service.RevokeTaskEvidence(t.Context(), request)
	if err != nil || summary.State != string(artifactrepo.StateTombstoned) {
		t.Fatalf("RevokeTaskEvidence() = %+v, %v", summary, err)
	}
	replay, err := fixture.service.RevokeTaskEvidence(t.Context(), request)
	if err != nil || !reflect.DeepEqual(replay, summary) {
		t.Fatalf("idempotent RevokeTaskEvidence() = %+v, %v", replay, err)
	}
	if _, err := os.Stat(contentPath); err != nil {
		t.Fatalf("shared content bytes were unsafely removed: %v", err)
	}
	if _, err := fixture.service.ReadTaskEvidence(t.Context(), TaskEvidenceReadRequest{
		Scope: fixture.scope, ManifestID: imported.Manifest.ManifestID,
		Access: artifactrepo.SensitiveAccessRequest{
			RequestID: "read-after-revoke", Actor: "debugger",
			Purpose: artifactrepo.SensitiveAccessIncidentInvestigation,
			At:      at.Add(time.Minute),
		},
	}); !errors.Is(err, artifactrepo.ErrTombstoned) {
		t.Fatalf("ReadTaskEvidence() after revoke error = %v, want ErrTombstoned", err)
	}
	history, err := fixture.service.Query(t.Context(), QueryRequest{
		Scope: fixture.scope, ManifestID: imported.Manifest.ManifestID,
	})
	if err != nil || history.Manifest.ManifestID != imported.Manifest.ManifestID ||
		history.TaskEvidenceCollection.SchemaVersion != "" ||
		history.TaskEvidence.State != string(artifactrepo.StateTombstoned) {
		t.Fatalf("Query() after revoke = %+v, %v", history, err)
	}
	conflict := request
	conflict.Reason = "different reason"
	if _, err := fixture.service.RevokeTaskEvidence(t.Context(), conflict); !errors.Is(err, artifactrepo.ErrConflict) {
		t.Fatalf("conflicting revoke error = %v, want ErrConflict", err)
	}
}
