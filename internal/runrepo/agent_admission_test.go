package runrepo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

const (
	agentAdmissionSemanticSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	agentAdmissionBehaviorSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	agentAdmissionArtifactSHA = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestExecutionSnapshotForRunUsesOnlyAuthoritativeCreatedLineage(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentAdmissionRepositories(t)
	now := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	snapshot := validAgentAdmissionExecutionSnapshot(now)
	if err := repository.SaveExecutionSnapshot(snapshot); err != nil {
		t.Fatalf("SaveExecutionSnapshot() error = %v", err)
	}
	created := RunEvent{
		RunID: "run-1", Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusPending, EventType: EventRunCreated,
		ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
	}
	if err := repository.AppendEvent("run-1-created", now, created); err != nil {
		t.Fatalf("AppendEvent(created) error = %v", err)
	}
	started := RunEvent{
		RunID: "run-1", Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusRunning, EventType: EventRunStarted,
		ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
	}
	if err := repository.AppendEvent("run-1-started", now.Add(time.Second), started); err != nil {
		t.Fatalf("AppendEvent(started) error = %v", err)
	}

	loaded, err := repository.ExecutionSnapshotForRun("run-1")
	if err != nil {
		t.Fatalf("ExecutionSnapshotForRun() error = %v", err)
	}
	if loaded.ExecutionSnapshotID != snapshot.ExecutionSnapshotID || !reflect.DeepEqual(loaded, snapshot) {
		t.Fatalf("ExecutionSnapshotForRun() = %+v, want exact snapshot", loaded)
	}
}

func TestExecutionSnapshotForRunRejectsMissingAmbiguousAndDriftedLineage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name  string
		setup func(*testing.T, *Repository)
		want  string
	}{
		{
			name:  "no events",
			setup: func(*testing.T, *Repository) {},
			want:  "no authoritative events",
		},
		{
			name: "no created event",
			setup: func(t *testing.T, repository *Repository) {
				t.Helper()
				if err := repository.AppendEvent("run-1-started", now, RunEvent{
					RunID: "run-1", Kind: runmodel.RunKindReview,
					Status: runmodel.RunStatusRunning, EventType: EventRunStarted,
					ExecutionSnapshotID: "snapshot-1",
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: "no authoritative run.created",
		},
		{
			name: "created without snapshot id",
			setup: func(t *testing.T, repository *Repository) {
				t.Helper()
				if err := repository.AppendEvent("run-1-created", now, RunEvent{
					RunID: "run-1", Kind: runmodel.RunKindReview,
					Status: runmodel.RunStatusPending, EventType: EventRunCreated,
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: "has no execution_snapshot_id",
		},
		{
			name: "duplicate created",
			setup: func(t *testing.T, repository *Repository) {
				t.Helper()
				for _, eventID := range []string{"run-1-created-a", "run-1-created-b"} {
					if err := repository.AppendEvent(eventID, now, RunEvent{
						RunID: "run-1", Kind: runmodel.RunKindReview,
						Status: runmodel.RunStatusPending, EventType: EventRunCreated,
						ExecutionSnapshotID: "snapshot-1",
					}); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: "multiple run.created",
		},
		{
			name: "snapshot lineage drift",
			setup: func(t *testing.T, repository *Repository) {
				t.Helper()
				if err := repository.AppendEvent("run-1-created", now, RunEvent{
					RunID: "run-1", Kind: runmodel.RunKindReview,
					Status: runmodel.RunStatusPending, EventType: EventRunCreated,
					ExecutionSnapshotID: "snapshot-1",
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.AppendEvent("run-1-started", now.Add(time.Second), RunEvent{
					RunID: "run-1", Kind: runmodel.RunKindReview,
					Status: runmodel.RunStatusRunning, EventType: EventRunStarted,
					ExecutionSnapshotID: "snapshot-2",
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: "lineage drifted",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, repository, _ := newAgentAdmissionRepositories(t)
			test.setup(t, repository)
			_, err := repository.ExecutionSnapshotForRun("run-1")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ExecutionSnapshotForRun() error = %v, want containing %q", err, test.want)
			}
			if test.name == "no events" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ExecutionSnapshotForRun() error = %v, want os.ErrNotExist", err)
			}
		})
	}
}

func TestAgentStagePlanAdmissionAppendIsCrossInstanceIdempotent(t *testing.T) {
	t.Parallel()

	store, first, second := newAgentAdmissionRepositories(t)
	admission := sealedAgentStagePlanAdmission(t)
	repositories := []*Repository{first, second}
	const writers = 32
	start := make(chan struct{})
	errorsByWriter := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			errorsByWriter <- repositories[index%len(repositories)].AppendAgentStagePlanAdmission(
				admission,
			)
		}(index)
	}
	close(start)
	wait.Wait()
	close(errorsByWriter)
	for err := range errorsByWriter {
		if err != nil {
			t.Errorf("AppendAgentStagePlanAdmission() error = %v", err)
		}
	}

	stream, err := agentStageAdmissionStream(admission.ReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	envelopes, err := store.ReadJSONL(stream)
	if err != nil {
		t.Fatalf("ReadJSONL() error = %v", err)
	}
	if len(envelopes) != 1 {
		t.Fatalf("persisted %d admissions, want 1", len(envelopes))
	}

	loaded, found, err := second.LookupAgentStagePlanAdmission(
		admission.ReviewRunID,
		admission.Stage.ID,
		admission.Subject,
	)
	if err != nil {
		t.Fatalf("LookupAgentStagePlanAdmission() error = %v", err)
	}
	if !found || loaded != admission {
		t.Fatalf("LookupAgentStagePlanAdmission() = (%+v, %v), want exact admission", loaded, found)
	}
	exact, err := first.LoadAgentStagePlanAdmission(
		admission.ReviewRunID,
		admission.AdmissionID,
		admission.Subject,
	)
	if err != nil || exact != admission {
		t.Fatalf("LoadAgentStagePlanAdmission() = (%+v, %v), want exact admission", exact, err)
	}
	_, found, err = first.LookupAgentStagePlanAdmission(
		admission.ReviewRunID,
		"other-stage",
		admission.Subject,
	)
	if err != nil || found {
		t.Fatalf("missing LookupAgentStagePlanAdmission() = (%v, %v), want false, nil", found, err)
	}
}

func TestAgentStagePlanAdmissionConcurrentConflictSelectsOneWinner(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentAdmissionRepositories(t)
	left := sealedAgentStagePlanAdmission(t)
	rightInput := validAgentStagePlanAdmission()
	rightInput.RecordedAt = rightInput.RecordedAt.Add(time.Second)
	right, err := runmodel.SealAgentStagePlanAdmission(rightInput)
	if err != nil {
		t.Fatal(err)
	}
	if left.AdmissionID != right.AdmissionID || left.SHA256 == right.SHA256 {
		t.Fatal("fixture does not model a conflicting retry of the same run+stage")
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- first.AppendAgentStagePlanAdmission(left)
	}()
	go func() {
		<-start
		results <- second.AppendAgentStagePlanAdmission(right)
	}()
	close(start)
	firstErr, secondErr := <-results, <-results
	successes, conflicts := 0, 0
	for _, err := range []error{firstErr, secondErr} {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentStageAdmissionConflict) &&
			errors.Is(err, local.ErrEventConflict):
			conflicts++
		default:
			t.Fatalf("concurrent append error = %v, want nil or ErrEventConflict", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}

	winner, found, err := first.LookupAgentStagePlanAdmission(
		left.ReviewRunID,
		left.Stage.ID,
		left.Subject,
	)
	if err != nil || !found {
		t.Fatalf("LookupAgentStagePlanAdmission() = found %v, error %v", found, err)
	}
	if winner != left && winner != right {
		t.Fatalf("persisted admission is neither concurrent contender: %+v", winner)
	}
}

func TestAgentStagePlanAdmissionLookupRequiresCompleteSubject(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentAdmissionRepositories(t)
	admission := sealedAgentStagePlanAdmission(t)
	if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*runmodel.AgentPlanningSubject)
	}{
		{"tenant", func(value *runmodel.AgentPlanningSubject) { value.TenantID = "tenant-2" }},
		{"organization", func(value *runmodel.AgentPlanningSubject) { value.OrganizationID = "organization-2" }},
		{"workspace", func(value *runmodel.AgentPlanningSubject) { value.WorkspaceID = "workspace-2" }},
		{"repository", func(value *runmodel.AgentPlanningSubject) { value.RepositoryID = "repository-2" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			subject := admission.Subject
			test.mutate(&subject)
			_, found, err := repository.LookupAgentStagePlanAdmission(
				admission.ReviewRunID,
				admission.Stage.ID,
				subject,
			)
			if found || !errors.Is(err, ErrAgentStageAdmissionSubjectMismatch) {
				t.Fatalf("LookupAgentStagePlanAdmission() = found %v, error %v, want subject mismatch", found, err)
			}
		})
	}
}

func TestAgentStagePlanAdmissionStreamSupportsDistinctStages(t *testing.T) {
	t.Parallel()

	store, repository, _ := newAgentAdmissionRepositories(t)
	first := sealedAgentStagePlanAdmission(t)
	secondInput := validAgentStagePlanAdmission()
	secondInput.Stage.ID = "agent-verify"
	second, err := runmodel.SealAgentStagePlanAdmission(secondInput)
	if err != nil {
		t.Fatal(err)
	}
	for _, admission := range []runmodel.AgentStagePlanAdmission{first, second} {
		if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
			t.Fatalf("AppendAgentStagePlanAdmission(%s) error = %v", admission.Stage.ID, err)
		}
	}
	stream, err := agentStageAdmissionStream(first.ReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	envelopes, err := store.ReadJSONL(stream)
	if err != nil || len(envelopes) != 2 {
		t.Fatalf("ReadJSONL() = %d events, error %v, want 2", len(envelopes), err)
	}
	for _, stageID := range []string{first.Stage.ID, second.Stage.ID} {
		if _, found, err := repository.LookupAgentStagePlanAdmission(
			first.ReviewRunID,
			stageID,
			first.Subject,
		); err != nil || !found {
			t.Fatalf("lookup stage %q = found %v, error %v", stageID, found, err)
		}
	}
}

func TestAgentStagePlanAdmissionLoadRejectsForeignNamespaceAndUnknownFields(t *testing.T) {
	t.Parallel()

	t.Run("foreign run payload", func(t *testing.T) {
		t.Parallel()
		store, repository, _ := newAgentAdmissionRepositories(t)
		foreignInput := validAgentStagePlanAdmission()
		foreignInput.ReviewRunID = "run-2"
		foreign, err := runmodel.SealAgentStagePlanAdmission(foreignInput)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := agentStageAdmissionStream("run-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendJSONL(stream, local.Event{
			ID: foreign.AdmissionID, Schema: runmodel.AgentStagePlanAdmissionSchemaVersion,
			Time: foreign.RecordedAt, Payload: foreign,
		}); err != nil {
			t.Fatal(err)
		}
		_, _, err = repository.LookupAgentStagePlanAdmission(
			"run-1", "agent-hypothesize", foreign.Subject,
		)
		if err == nil || !strings.Contains(err.Error(), "not namespaced") {
			t.Fatalf("LookupAgentStagePlanAdmission() error = %v, want namespace rejection", err)
		}
	})

	t.Run("unknown payload field", func(t *testing.T) {
		t.Parallel()
		store, repository, _ := newAgentAdmissionRepositories(t)
		admission := sealedAgentStagePlanAdmission(t)
		payload, err := json.Marshal(admission)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.Replace(payload, []byte(`{"schema_version":`), []byte(`{"unknown":true,"schema_version":`), 1)
		stream, err := agentStageAdmissionStream(admission.ReviewRunID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendJSONL(stream, local.Event{
			ID: admission.AdmissionID, Schema: runmodel.AgentStagePlanAdmissionSchemaVersion,
			Time: admission.RecordedAt, Payload: json.RawMessage(payload),
		}); err != nil {
			t.Fatal(err)
		}
		_, _, err = repository.LookupAgentStagePlanAdmission(
			admission.ReviewRunID, admission.Stage.ID, admission.Subject,
		)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("LookupAgentStagePlanAdmission() error = %v, want strict decode rejection", err)
		}
	})
}

func TestLoadAgentStagePlanAdmissionRejectsForeignAdmissionIDBeforeRead(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentAdmissionRepositories(t)
	admission := sealedAgentStagePlanAdmission(t)
	_, err := repository.LoadAgentStagePlanAdmission(
		admission.ReviewRunID,
		"run-2-agent-stage-admission-aaaaaaaaaaaaaaaaaaaaaaaa",
		admission.Subject,
	)
	if err == nil || errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "not namespaced") {
		t.Fatalf("LoadAgentStagePlanAdmission() error = %v, want namespace rejection", err)
	}
}

func newAgentAdmissionRepositories(
	t *testing.T,
) (*local.Store, *Repository, *Repository) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := local.Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(secondStore)
	if err != nil {
		t.Fatal(err)
	}
	return store, first, second
}

func sealedAgentStagePlanAdmission(t *testing.T) runmodel.AgentStagePlanAdmission {
	t.Helper()
	admission, err := runmodel.SealAgentStagePlanAdmission(validAgentStagePlanAdmission())
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func validAgentStagePlanAdmission() runmodel.AgentStagePlanAdmission {
	subject := runmodel.AgentPlanningSubject{
		TenantID:       "tenant-1",
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		RepositoryID:   "repository-1",
	}
	projection := func(contract, object string) runmodel.AgentArtifactProjection {
		objectDigest := sha256.Sum256([]byte(object))
		return runmodel.AgentArtifactProjection{
			Local: runmodel.ArtifactRef{
				URI:       "artifact://local/sha256/" + agentAdmissionArtifactSHA,
				SHA256:    agentAdmissionArtifactSHA,
				SizeBytes: 128,
				Contract:  contract,
			},
			Governed: runmodel.GovernedArtifactBinding{
				URI: "artifact://argus-local/tenants/" + subject.TenantID +
					"/workspaces/" + subject.WorkspaceID + "/objects/" +
					hex.EncodeToString(objectDigest[:]),
				SHA256:    agentAdmissionArtifactSHA,
				SizeBytes: 128,
				Contract:  contract,
			},
		}
	}
	return runmodel.AgentStagePlanAdmission{
		SchemaVersion: runmodel.AgentStagePlanAdmissionSchemaVersion,
		Subject:       subject,
		ReviewRunID:   "run-1",
		Stage: runmodel.AgentStageRef{
			ID: "agent-hypothesize", Revision: "1", SHA256: agentAdmissionSemanticSHA,
		},
		PlanID:                "agent-stage-plan-" + agentAdmissionSemanticSHA[:24],
		PlanSemanticSHA256:    agentAdmissionSemanticSHA,
		PlanBehaviorSHA256:    agentAdmissionBehaviorSHA,
		Plan:                  projection(runmodel.ContractAgentStagePlan, "plan-object"),
		ReceiptID:             "config-resolution-" + agentAdmissionSemanticSHA[:24],
		ReceiptSemanticSHA256: agentAdmissionSemanticSHA,
		Sources: runmodel.AgentStageSourceProjections{
			ExecutionSnapshot:       projection(runmodel.ContractExecutionSnapshot, "snapshot-object"),
			ConfigBundle:            projection(runmodel.ContractConfigBundle, "config-object"),
			ConfigResolutionReceipt: projection(runmodel.ContractConfigResolutionReceipt, "receipt-object"),
			Workflow:                projection(runmodel.ContractWorkflowDefinition, "workflow-object"),
			ReviewSpec:              projection(runmodel.ContractReviewSpec, "spec-object"),
			ReviewInput:             projection(runmodel.ContractReviewInput, "input-object"),
		},
		RecordedAt: time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
	}
}

func validAgentAdmissionExecutionSnapshot(now time.Time) runmodel.ExecutionSnapshot {
	ref := func(contract string) runmodel.ArtifactRef {
		return runmodel.ArtifactRef{
			URI:       "artifact://local/sha256/" + agentAdmissionArtifactSHA,
			SHA256:    agentAdmissionArtifactSHA,
			SizeBytes: 1,
			Contract:  contract,
		}
	}
	return runmodel.ExecutionSnapshot{
		SchemaVersion:         runmodel.SnapshotSchemaVersion,
		ExecutionSnapshotID:   "snapshot-1",
		ReviewSpecSHA256:      agentAdmissionArtifactSHA,
		ReviewSpecRef:         ref(runmodel.ContractReviewSpec),
		TargetSnapshotRef:     ref(runmodel.ContractMaterializedTarget),
		ReviewInputRef:        ref(runmodel.ContractReviewInput),
		ReplayInputRefs:       []runmodel.ArtifactRef{},
		WorkflowDefinitionRef: ref(runmodel.ContractWorkflowDefinition),
		ConfigBundleRef:       ref(runmodel.ContractConfigBundle),
		Workflow: runmodel.WorkflowRef{
			ID: "workflow", Revision: "1", SHA256: agentAdmissionArtifactSHA,
		},
		Config: runmodel.PolicyRef{
			ID: "config", Revision: "1", SHA256: agentAdmissionSemanticSHA,
		},
		RuntimeProfile: "runtime-1",
		BuildIdentity:  "build-1",
		ToolPolicy: runmodel.ToolInvocationPolicy{
			AllowedTools: []string{}, Network: "deny", WorkspaceWrites: "deny",
			RemoteWrites: "deny", PerCallTimeoutMS: 1, MaxOutputBytes: 1,
			MaxConcurrency: 1, MaxDelegationDepth: 0,
		},
		RemoteWrites: "deny",
		CreatedAt:    now,
	}
}
