package main

import (
	"bytes"
	"strings"
	"testing"

	"argus.local/argus/internal/agentshadow"
	"argus.local/argus/internal/artifactrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestGenericAgentReviewJSONCannotExportExactTaskEvidence(t *testing.T) {
	result := agentshadow.LocalRunResult{
		OutcomeAcknowledged: true,
		Result: agentshadow.Result{
			Manifest: contractsv1alpha1.AgentReviewResultManifest{
				Status:     contractsv1alpha1.AgentReviewRunComplete,
				ManifestID: "manifest-sensitive-json",
			},
			HypothesisSet: contractsv1alpha1.ReviewHypothesisSet{
				Hypotheses: []contractsv1alpha1.ReviewHypothesis{},
			},
			TaskEvidenceCollection: contractsv1alpha1.AgentReviewTaskEvidenceCollection{
				SchemaVersion: contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
				ReasonCodes:   []string{"SECRET_SENTINEL_EXACT_PROMPT"},
			},
			TaskEvidence: agentshadow.TaskEvidenceSummary{
				ContentPolicy: contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
				State:         "active", ExportPolicy: "deny",
				RetentionPolicy:   "until_explicit_revocation",
				DeletionSemantics: "logical_tombstone_shared_content_gc_deferred",
			},
		},
	}
	var output bytes.Buffer
	if err := writeAgentReviewRunResult(&output, true, false, result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "SECRET_SENTINEL_EXACT_PROMPT") ||
		strings.Contains(output.String(), `"task_evidence_collection"`) ||
		!strings.Contains(output.String(), `"export_policy":"deny"`) {
		t.Fatalf("generic run JSON leaked or omitted policy: %s", output.String())
	}
}

func TestEvidenceReadRequiresExplicitAcknowledgementAndJSON(t *testing.T) {
	base := []string{
		"agent-review", "evidence", "read",
		"--store", t.TempDir(),
		"--manifest-id", "manifest-1",
		"--request-id", "request-1",
		"--actor", "debugger",
		"--purpose", "local_debug",
		"--at", "2026-08-25T01:02:03Z",
	}
	for _, extra := range [][]string{{}, {"--json"}, {"--acknowledge-sensitive-output"}} {
		err := runWithIO(t.Context(), append(append([]string{}, base...), extra...), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "requires --acknowledge-sensitive-output and --json") {
			t.Fatalf("evidence read flags %v error = %v", extra, err)
		}
	}
	if purpose, err := parseSensitiveAccessPurpose("evaluation_replay"); err != nil ||
		purpose != artifactrepo.SensitiveAccessEvaluationReplay {
		t.Fatalf("parseSensitiveAccessPurpose() = %q, %v", purpose, err)
	}
	if _, err := parseSensitiveAccessPurpose("export"); err == nil {
		t.Fatal("parseSensitiveAccessPurpose() accepted export")
	}
}

func TestFormalEvidenceReadRequiresExplicitAcknowledgementAndJSON(t *testing.T) {
	arguments := []string{
		"agent-review", "formal", "evidence", "read",
		"--store", t.TempDir(), "--formal-run", "formal-run-1",
		"--request-id", "formal-request-1", "--actor", "debugger",
		"--purpose", "incident_investigation",
		"--at", "2026-08-25T01:02:03Z", "--json",
	}
	err := runWithIO(t.Context(), arguments, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "requires --acknowledge-sensitive-output and --json") {
		t.Fatalf("formal evidence read error = %v", err)
	}
}
