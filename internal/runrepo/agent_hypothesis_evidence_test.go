package runrepo

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
)

const hypothesisEvidenceTargetSHA = "7444444444444444444444444444444444444444444444444444444444444444"

func TestAgentStageHypothesisEvidenceAppendLoadLookupAndExactRetry(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	evidence := sealedHypothesisEvidenceRepoClosure(t, first)
	if err := first.AppendAgentStageHypothesisEvidence(evidence); err != nil {
		t.Fatalf("AppendAgentStageHypothesisEvidence() error = %v", err)
	}
	if err := second.AppendAgentStageHypothesisEvidence(evidence); err != nil {
		t.Fatalf("exact evidence retry error = %v", err)
	}
	loaded, err := first.LoadAgentStageHypothesisEvidence(
		evidence.ReviewRunID,
		evidence.EvidenceID,
		evidence.Subject,
	)
	if err != nil || loaded != evidence {
		t.Fatalf("LoadAgentStageHypothesisEvidence() = (%+v, %v)", loaded, err)
	}
	lookedUp, found, err := second.LookupAgentStageHypothesisEvidence(
		evidence.ReviewRunID,
		evidence.BindingID,
		evidence.Subject,
	)
	if err != nil || !found || lookedUp != evidence {
		t.Fatalf("LookupAgentStageHypothesisEvidence() = (%+v, %v, %v)",
			lookedUp, found, err)
	}

	missingIntentID := mustDispatchIntentID(
		t,
		evidence.ReviewRunID,
		"other-stage",
		1,
		1,
	)
	missingBindingID, err := runmodel.AgentStageExecutionBindingID(
		evidence.ReviewRunID,
		missingIntentID,
	)
	if err != nil {
		t.Fatal(err)
	}
	missing, found, err := first.LookupAgentStageHypothesisEvidence(
		evidence.ReviewRunID,
		missingBindingID,
		evidence.Subject,
	)
	if err != nil || found ||
		missing != (runmodel.AgentStageHypothesisEvidence{}) {
		t.Fatalf("missing evidence lookup = (%+v, %v, %v)", missing, found, err)
	}
}

func TestAgentStageHypothesisEvidenceChangedPayloadConflicts(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	left := sealedHypothesisEvidenceRepoClosure(t, first)
	rightInput := left
	rightInput.HypothesisSetID = "hypothesis-set-changed"
	right, err := runmodel.SealAgentStageHypothesisEvidence(rightInput)
	if err != nil {
		t.Fatal(err)
	}
	if left.EvidenceID != right.EvidenceID || left.SHA256 == right.SHA256 {
		t.Fatal("fixture does not model changed content under one binding authority")
	}
	if err := first.AppendAgentStageHypothesisEvidence(left); err != nil {
		t.Fatal(err)
	}
	if err := second.AppendAgentStageHypothesisEvidence(right); err == nil ||
		!errors.Is(err, ErrAgentStageHypothesisEvidenceConflict) {
		t.Fatalf("changed evidence append error = %v, want conflict", err)
	}
}

func TestAgentStageHypothesisEvidenceLookupRejectsForeignSubject(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	evidence := sealedHypothesisEvidenceRepoClosure(t, repository)
	if err := repository.AppendAgentStageHypothesisEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*runmodel.AgentPlanningSubject)
	}{
		{"tenant", func(value *runmodel.AgentPlanningSubject) { value.TenantID = "tenant-2" }},
		{"organization", func(value *runmodel.AgentPlanningSubject) { value.OrganizationID = "organization-2" }},
		{"workspace", func(value *runmodel.AgentPlanningSubject) { value.WorkspaceID = "workspace-2" }},
		{"repository", func(value *runmodel.AgentPlanningSubject) { value.RepositoryID = "repository-2" }},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			subject := evidence.Subject
			test.mutate(&subject)
			_, found, err := repository.LookupAgentStageHypothesisEvidence(
				evidence.ReviewRunID,
				evidence.BindingID,
				subject,
			)
			if found || !errors.Is(
				err,
				ErrAgentStageHypothesisEvidenceSubjectMismatch,
			) {
				t.Fatalf("foreign lookup = found %v, error %v", found, err)
			}
		})
	}
}

func TestAgentStageHypothesisEvidenceRequiresBoundCurrentClosure(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, repository)
	unbound := sealHypothesisEvidenceRepo(
		t,
		fixture.admission,
		fixture.intent,
		fixture.binding,
		fixture.gate,
		fixture.gate.Completion.ResultRef,
		fixture.gate.Completion.Output,
	)
	if err := repository.AppendAgentStageHypothesisEvidence(unbound); err == nil ||
		!errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evidence without terminal winner error = %v, want os.ErrNotExist", err)
	}

	if err := repository.AppendAgentStageTerminalGate(fixture.gate); err != nil {
		t.Fatal(err)
	}
	staleInput := unbound
	staleInput.TerminalGateSHA256 = strings.Repeat("e", 64)
	stale, err := runmodel.SealAgentStageHypothesisEvidence(staleInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AppendAgentStageHypothesisEvidence(stale); err == nil ||
		!strings.Contains(err.Error(), "accepted terminal closure") {
		t.Fatalf("stale evidence append error = %v", err)
	}
}

func TestAgentStageHypothesisEvidenceRejectsCancellationWinner(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	fixture := persistTerminalCompletionFixture(t, repository)
	cancellation := sealTerminalCancellation(t, fixture.terminalIntentFixture)
	if err := repository.AppendAgentStageTerminalGate(cancellation); err != nil {
		t.Fatal(err)
	}
	evidence := sealHypothesisEvidenceRepo(
		t,
		fixture.admission,
		fixture.intent,
		fixture.binding,
		fixture.gate,
		fixture.gate.Completion.ResultRef,
		fixture.gate.Completion.Output,
	)
	evidence.TerminalGateID = cancellation.GateID
	evidence.TerminalGateSHA256 = cancellation.SHA256
	evidence, err := runmodel.SealAgentStageHypothesisEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AppendAgentStageHypothesisEvidence(evidence); err == nil ||
		!strings.Contains(err.Error(), "succeeded result acceptance") {
		t.Fatalf("evidence after cancellation winner error = %v", err)
	}
}

func TestAgentStageHypothesisEvidenceLoadRejectsTamperedPayload(t *testing.T) {
	t.Parallel()

	store, repository, _ := newAgentDispatchRepositories(t)
	evidence := sealedHypothesisEvidenceRepoClosure(t, repository)
	payload, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(
		payload,
		[]byte(`{"schema_version":`),
		[]byte(`{"unknown":true,"schema_version":`),
		1,
	)
	stream, err := agentStageHypothesisEvidenceStream(evidence.ReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendJSONL(stream, local.Event{
		ID:      evidence.EvidenceID,
		Schema:  runmodel.AgentStageHypothesisEvidenceSchemaVersion,
		Time:    evidence.AdmittedAt,
		Payload: json.RawMessage(payload),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err = repository.LookupAgentStageHypothesisEvidence(
		evidence.ReviewRunID,
		evidence.BindingID,
		evidence.Subject,
	)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("tampered evidence lookup error = %v", err)
	}
}

func TestAgentStageHypothesisEvidenceLoadRevalidatesLocalArtifacts(t *testing.T) {
	t.Parallel()

	store, repository, _ := newAgentDispatchRepositories(t)
	evidence := sealedHypothesisEvidenceRepoClosure(t, repository)
	if err := repository.AppendAgentStageHypothesisEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	artifactPath := store.Root() + "/artifacts/sha256/" +
		evidence.Output.Local.SHA256[:2] + "/" + evidence.Output.Local.SHA256
	if err := os.Chmod(artifactPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := repository.LookupAgentStageHypothesisEvidence(
		evidence.ReviewRunID,
		evidence.BindingID,
		evidence.Subject,
	)
	if err == nil || !errors.Is(err, local.ErrCorrupt) {
		t.Fatalf("artifact-tampered lookup error = %v, want ErrCorrupt", err)
	}
}

func sealedHypothesisEvidenceRepoClosure(
	t *testing.T,
	repository *Repository,
) runmodel.AgentStageHypothesisEvidence {
	t.Helper()
	fixture := persistTerminalCompletionFixture(t, repository)
	if err := repository.AppendAgentStageTerminalGate(fixture.gate); err != nil {
		t.Fatal(err)
	}
	return sealHypothesisEvidenceRepo(
		t,
		fixture.admission,
		fixture.intent,
		fixture.binding,
		fixture.gate,
		fixture.gate.Completion.ResultRef,
		fixture.gate.Completion.Output,
	)
}

func sealHypothesisEvidenceRepo(
	t *testing.T,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
	terminal runmodel.AgentStageTerminalGate,
	resultRef runmodel.ArtifactRef,
	output runmodel.AgentArtifactProjection,
) runmodel.AgentStageHypothesisEvidence {
	t.Helper()
	evidence, err := runmodel.SealAgentStageHypothesisEvidence(
		runmodel.AgentStageHypothesisEvidence{
			SchemaVersion:         runmodel.AgentStageHypothesisEvidenceSchemaVersion,
			Subject:               admission.Subject,
			ReviewRunID:           admission.ReviewRunID,
			Stage:                 admission.Stage,
			AdmissionID:           admission.AdmissionID,
			AdmissionSHA256:       admission.SHA256,
			IntentID:              intent.IntentID,
			IntentSHA256:          intent.SHA256,
			BindingID:             binding.BindingID,
			BindingSHA256:         binding.SHA256,
			TerminalGateID:        terminal.GateID,
			TerminalGateSHA256:    terminal.SHA256,
			RequestRef:            intent.RequestRef,
			RequestSemanticSHA256: intent.RequestSemanticSHA256,
			ResultRef:             resultRef,
			Output:                output,
			PlanID:                admission.PlanID,
			PlanSemanticSHA256:    admission.PlanSemanticSHA256,
			TargetDigest:          hypothesisEvidenceTargetSHA,
			HypothesisSetID:       "hypothesis-set-formal-1",
			AdmittedAt: time.Date(
				2026, 8, 24, 4, 30, 0, 0, time.UTC,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}
