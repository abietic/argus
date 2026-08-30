package runrepo

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

const (
	dispatchRepoAdmissionSemanticSHA = "6111111111111111111111111111111111111111111111111111111111111111"
	dispatchRepoBehaviorSHA          = "6222222222222222222222222222222222222222222222222222222222222222"
	dispatchRepoArtifactSHA          = "6333333333333333333333333333333333333333333333333333333333333333"
	dispatchRepoRequestSemanticSHA   = "6444444444444444444444444444444444444444444444444444444444444444"
	dispatchRepoCapabilitySHA        = "6555555555555555555555555555555555555555555555555555555555555555"
)

func TestAgentStageDispatchAndBindingAppendLookupExactRetry(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := first.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intent := sealedDispatchRepoIntent(t, admission)
	if err := second.AppendAgentStageDispatchIntent(intent); err != nil {
		t.Fatalf("AppendAgentStageDispatchIntent() error = %v", err)
	}
	claimed, err := first.ClaimAgentStageDispatchIntent(intent)
	if err != nil || !claimed {
		t.Fatalf("first ClaimAgentStageDispatchIntent() = (%v, %v), want true, nil", claimed, err)
	}
	claimed, err = second.ClaimAgentStageDispatchIntent(intent)
	if err != nil || claimed {
		t.Fatalf("retry ClaimAgentStageDispatchIntent() = (%v, %v), want false, nil", claimed, err)
	}
	loaded, found, err := first.LookupAgentStageDispatchIntent(
		intent.ReviewRunID,
		intent.Stage.ID,
		intent.Attempt,
		intent.Generation,
		intent.Subject,
	)
	if err != nil || !found || loaded != intent {
		t.Fatalf("LookupAgentStageDispatchIntent() = (%+v, %v, %v)", loaded, found, err)
	}
	exact, err := second.LoadAgentStageDispatchIntent(
		intent.ReviewRunID,
		intent.IntentID,
		intent.Subject,
	)
	if err != nil || exact != intent {
		t.Fatalf("LoadAgentStageDispatchIntent() = (%+v, %v)", exact, err)
	}

	binding := sealedDispatchRepoBinding(t, intent)
	if err := first.AppendAgentStageExecutionBinding(binding); err != nil {
		t.Fatal(err)
	}
	if err := second.AppendAgentStageExecutionBinding(binding); err != nil {
		t.Fatalf("exact binding retry error = %v", err)
	}
	loadedBinding, found, err := first.LookupAgentStageExecutionBinding(
		binding.ReviewRunID,
		binding.IntentID,
		binding.Subject,
	)
	if err != nil || !found || loadedBinding != binding {
		t.Fatalf("LookupAgentStageExecutionBinding() = (%+v, %v, %v)", loadedBinding, found, err)
	}
	missing, found, err := first.LookupAgentStageExecutionBinding(
		binding.ReviewRunID,
		mustDispatchIntentID(t, binding.ReviewRunID, "other-stage", 1, 1),
		binding.Subject,
	)
	if err != nil || found || missing != (runmodel.AgentStageExecutionBinding{}) {
		t.Fatalf("missing binding lookup = (%+v, %v, %v)", missing, found, err)
	}
}

func TestAgentStageDispatchExactConcurrentClaimHasOneCreateOwner(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := first.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intent := sealedDispatchRepoIntent(t, admission)
	start := make(chan struct{})
	type result struct {
		claimed bool
		err     error
	}
	results := make(chan result, 2)
	for _, repository := range []*Repository{first, second} {
		repository := repository
		go func() {
			<-start
			claimed, err := repository.ClaimAgentStageDispatchIntent(intent)
			results <- result{claimed: claimed, err: err}
		}()
	}
	close(start)
	claimedCount := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent exact claim error = %v", result.err)
		}
		if result.claimed {
			claimedCount++
		}
	}
	if claimedCount != 1 {
		t.Fatalf("claimed=true count = %d, want exactly 1", claimedCount)
	}
	intents, err := first.readAgentStageDispatchIntents(intent.ReviewRunID)
	if err != nil || len(intents) != 1 || intents[0] != intent {
		t.Fatalf("persisted intents = %+v, error %v", intents, err)
	}
}

func TestAgentStageDispatchConcurrentChangedPayloadConflicts(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := first.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	left := sealedDispatchRepoIntent(t, admission)
	rightInput := validDispatchRepoIntent(admission)
	rightInput.ExecutionID = "execution-other"
	right, err := runmodel.SealAgentStageDispatchIntent(rightInput)
	if err != nil {
		t.Fatal(err)
	}
	if left.IntentID != right.IntentID || left.SHA256 == right.SHA256 {
		t.Fatal("fixture does not model conflicting content under one intent authority key")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, repository := range []*Repository{first, second} {
		intent := left
		if index == 1 {
			intent = right
		}
		repository := repository
		go func() {
			<-start
			_, err := repository.ClaimAgentStageDispatchIntent(intent)
			results <- err
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentStageDispatchConflict):
			conflicts++
		default:
			t.Fatalf("concurrent changed intent error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}
}

func TestAgentStageExecutionBindingConcurrentChangedPayloadConflicts(t *testing.T) {
	t.Parallel()

	_, first, second := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := first.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intent := sealedDispatchRepoIntent(t, admission)
	if _, err := first.ClaimAgentStageDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	left := sealedDispatchRepoBinding(t, intent)
	rightInput := validDispatchRepoBinding(intent)
	rightInput.ProviderHandle = "provider/executions/other"
	right, err := runmodel.SealAgentStageExecutionBinding(rightInput)
	if err != nil {
		t.Fatal(err)
	}
	if left.BindingID != right.BindingID || left.SHA256 == right.SHA256 {
		t.Fatal("fixture does not model conflicting provider bindings")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, repository := range []*Repository{first, second} {
		binding := left
		if index == 1 {
			binding = right
		}
		repository := repository
		go func() {
			<-start
			results <- repository.AppendAgentStageExecutionBinding(binding)
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentStageExecutionBindingConflict):
			conflicts++
		default:
			t.Fatalf("concurrent changed binding error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}
}

func TestAgentStageDispatchLookupsRequireCompleteSubject(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intent := sealedDispatchRepoIntent(t, admission)
	if _, err := repository.ClaimAgentStageDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	binding := sealedDispatchRepoBinding(t, intent)
	if err := repository.AppendAgentStageExecutionBinding(binding); err != nil {
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
			subject := intent.Subject
			test.mutate(&subject)
			_, found, intentErr := repository.LookupAgentStageDispatchIntent(
				intent.ReviewRunID,
				intent.Stage.ID,
				intent.Attempt,
				intent.Generation,
				subject,
			)
			if found || !errors.Is(intentErr, ErrAgentStageDispatchSubjectMismatch) {
				t.Fatalf("intent lookup = found %v, error %v", found, intentErr)
			}
			_, found, bindingErr := repository.LookupAgentStageExecutionBinding(
				binding.ReviewRunID,
				binding.IntentID,
				subject,
			)
			if found || !errors.Is(
				bindingErr,
				ErrAgentStageExecutionBindingSubjectMismatch,
			) {
				t.Fatalf("binding lookup = found %v, error %v", found, bindingErr)
			}
		})
	}
}

func TestListSupersededClaimedAgentStageDispatchIntentsIncludesBoundHistory(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	first := sealedDispatchRepoIntent(t, admission)
	if claimed, err := repository.ClaimAgentStageDispatchIntent(first); err != nil || !claimed {
		t.Fatalf("ClaimAgentStageDispatchIntent(first) = %v, %v", claimed, err)
	}
	if err := repository.AppendAgentStageExecutionBinding(
		sealedDispatchRepoBinding(t, first),
	); err != nil {
		t.Fatal(err)
	}

	secondInput := validDispatchRepoIntent(admission)
	secondInput.LeaseID = admission.ReviewRunID + "-workload-g2"
	secondInput.ExecutionID = "execution-dispatch-2"
	secondInput.Generation = 2
	secondInput.FencingToken = 10
	secondInput.CreateIdempotencyKey = "create-execution-dispatch-2"
	secondInput.CancelIdempotencyKey = "cancel-execution-dispatch-2"
	secondInput.RecordedAt = first.RecordedAt.Add(time.Minute)
	second, err := runmodel.SealAgentStageDispatchIntent(secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := repository.ClaimAgentStageDispatchIntent(second); err != nil || !claimed {
		t.Fatalf("ClaimAgentStageDispatchIntent(second) = %v, %v", claimed, err)
	}
	allClaimed, err := repository.ListClaimedAgentStageDispatchIntents(
		admission.ReviewRunID,
		admission.Subject,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(allClaimed) != 2 || allClaimed[0] != first || allClaimed[1] != second {
		t.Fatalf("all claimed intents = %+v, want ordered first and second generation", allClaimed)
	}

	intents, err := repository.ListSupersededClaimedAgentStageDispatchIntents(
		admission.ReviewRunID,
		admission.Subject,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0] != first {
		t.Fatalf("superseded claimed intents = %+v, want first generation", intents)
	}
}

func TestAgentStageExecutionBindingRequiresExistingExactIntent(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intent := sealedDispatchRepoIntent(t, admission)
	binding := sealedDispatchRepoBinding(t, intent)
	if err := repository.AppendAgentStageExecutionBinding(binding); err == nil ||
		!errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding-before-intent error = %v, want os.ErrNotExist", err)
	}
	if _, err := repository.ClaimAgentStageDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	mismatchedInput := validDispatchRepoBinding(intent)
	mismatchedInput.FencingToken++
	mismatched, err := runmodel.SealAgentStageExecutionBinding(mismatchedInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AppendAgentStageExecutionBinding(mismatched); err == nil ||
		!strings.Contains(err.Error(), "exact dispatch intent") {
		t.Fatalf("mismatched binding error = %v", err)
	}
}

func TestAgentStageExecutionBindingRequiresCreateClaim(t *testing.T) {
	t.Parallel()

	_, repository, _ := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intent := sealedDispatchRepoIntent(t, admission)
	if err := repository.AppendAgentStageDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	binding := sealedDispatchRepoBinding(t, intent)
	if err := repository.AppendAgentStageExecutionBinding(binding); err == nil ||
		!errors.Is(err, os.ErrNotExist) ||
		!strings.Contains(err.Error(), "dispatch claim") {
		t.Fatalf("binding-before-claim error = %v, want missing claim", err)
	}
	claimed, err := repository.ClaimAgentStageDispatchIntent(intent)
	if err != nil || !claimed {
		t.Fatalf("ClaimAgentStageDispatchIntent() = (%v, %v)", claimed, err)
	}
	if err := repository.AppendAgentStageExecutionBinding(binding); err != nil {
		t.Fatalf("binding after exact claim error = %v", err)
	}
}

func TestAgentStageDispatchLoadRejectsTamperedPayload(t *testing.T) {
	t.Parallel()

	store, repository, _ := newAgentDispatchRepositories(t)
	admission := sealedDispatchRepoAdmission(t)
	if err := repository.AppendAgentStagePlanAdmission(admission); err != nil {
		t.Fatal(err)
	}
	intent := sealedDispatchRepoIntent(t, admission)
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(
		payload,
		[]byte(`{"schema_version":`),
		[]byte(`{"unknown":true,"schema_version":`),
		1,
	)
	stream, err := agentStageDispatchIntentStream(intent.ReviewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendJSONL(stream, local.Event{
		ID: intent.IntentID, Schema: runmodel.AgentStageDispatchIntentSchemaVersion,
		Time: intent.RecordedAt, Payload: json.RawMessage(payload),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err = repository.LookupAgentStageDispatchIntent(
		intent.ReviewRunID,
		intent.Stage.ID,
		intent.Attempt,
		intent.Generation,
		intent.Subject,
	)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("tampered intent lookup error = %v", err)
	}
}

func newAgentDispatchRepositories(
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

func sealedDispatchRepoAdmission(t *testing.T) runmodel.AgentStagePlanAdmission {
	t.Helper()
	subject := runmodel.AgentPlanningSubject{
		TenantID:       "tenant-1",
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		RepositoryID:   "repository-1",
	}
	projection := func(contract string) runmodel.AgentArtifactProjection {
		return runmodel.AgentArtifactProjection{
			Local: runmodel.ArtifactRef{
				URI:       "artifact://local/sha256/" + dispatchRepoArtifactSHA,
				SHA256:    dispatchRepoArtifactSHA,
				SizeBytes: 512,
				Contract:  contract,
			},
			Governed: runmodel.GovernedArtifactBinding{
				URI: "artifact://argus-local/tenants/" + subject.TenantID +
					"/workspaces/" + subject.WorkspaceID +
					"/objects/" + dispatchRepoArtifactSHA,
				SHA256:    dispatchRepoArtifactSHA,
				SizeBytes: 512,
				Contract:  contract,
			},
		}
	}
	admission, err := runmodel.SealAgentStagePlanAdmission(
		runmodel.AgentStagePlanAdmission{
			SchemaVersion: runmodel.AgentStagePlanAdmissionSchemaVersion,
			Subject:       subject,
			ReviewRunID:   "run-dispatch-1",
			Stage: runmodel.AgentStageRef{
				ID:       "agent-hypothesize",
				Revision: "1",
				SHA256:   dispatchRepoAdmissionSemanticSHA,
			},
			PlanID: "agent-stage-plan-" +
				dispatchRepoAdmissionSemanticSHA[:24],
			PlanSemanticSHA256: dispatchRepoAdmissionSemanticSHA,
			PlanBehaviorSHA256: dispatchRepoBehaviorSHA,
			Plan:               projection(runmodel.ContractAgentStagePlan),
			ReceiptID: "config-resolution-" +
				dispatchRepoAdmissionSemanticSHA[:24],
			ReceiptSemanticSHA256: dispatchRepoAdmissionSemanticSHA,
			Sources: runmodel.AgentStageSourceProjections{
				ExecutionSnapshot: projection(runmodel.ContractExecutionSnapshot),
				ConfigBundle:      projection(runmodel.ContractConfigBundle),
				ConfigResolutionReceipt: projection(
					runmodel.ContractConfigResolutionReceipt,
				),
				Workflow:    projection(runmodel.ContractWorkflowDefinition),
				ReviewSpec:  projection(runmodel.ContractReviewSpec),
				ReviewInput: projection(runmodel.ContractReviewInput),
			},
			RecordedAt: time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func sealedDispatchRepoIntent(
	t *testing.T,
	admission runmodel.AgentStagePlanAdmission,
) runmodel.AgentStageDispatchIntent {
	t.Helper()
	intent, err := runmodel.SealAgentStageDispatchIntent(
		validDispatchRepoIntent(admission),
	)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func validDispatchRepoIntent(
	admission runmodel.AgentStagePlanAdmission,
) runmodel.AgentStageDispatchIntent {
	return runmodel.AgentStageDispatchIntent{
		SchemaVersion:      runmodel.AgentStageDispatchIntentSchemaVersion,
		Subject:            admission.Subject,
		ReviewRunID:        admission.ReviewRunID,
		Stage:              admission.Stage,
		WorkloadID:         admission.ReviewRunID + "-workload",
		LeaseID:            admission.ReviewRunID + "-workload-g1",
		LeaseWorker:        "worker-1",
		AdmissionID:        admission.AdmissionID,
		AdmissionSHA256:    admission.SHA256,
		PlanID:             admission.PlanID,
		PlanSemanticSHA256: admission.PlanSemanticSHA256,
		RequestRef: runmodel.ArtifactRef{
			URI:       "artifact://local/sha256/" + dispatchRepoArtifactSHA,
			SHA256:    dispatchRepoArtifactSHA,
			SizeBytes: 512,
			Contract:  runmodel.ContractStageExecutionRequest,
		},
		RequestSemanticSHA256: dispatchRepoRequestSemanticSHA,
		CapabilitySHA256:      dispatchRepoCapabilitySHA,
		ExecutionID:           "execution-dispatch-1",
		Attempt:               1,
		Generation:            1,
		FencingToken:          9,
		CreateIdempotencyKey:  "create-execution-dispatch-1",
		CancelIdempotencyKey:  "cancel-execution-dispatch-1",
		RecordedAt:            time.Date(2026, 8, 24, 3, 1, 0, 0, time.UTC),
	}
}

func sealedDispatchRepoBinding(
	t *testing.T,
	intent runmodel.AgentStageDispatchIntent,
) runmodel.AgentStageExecutionBinding {
	t.Helper()
	binding, err := runmodel.SealAgentStageExecutionBinding(
		validDispatchRepoBinding(intent),
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func validDispatchRepoBinding(
	intent runmodel.AgentStageDispatchIntent,
) runmodel.AgentStageExecutionBinding {
	return runmodel.AgentStageExecutionBinding{
		SchemaVersion:         runmodel.AgentStageExecutionBindingSchemaVersion,
		Subject:               intent.Subject,
		ReviewRunID:           intent.ReviewRunID,
		Stage:                 intent.Stage,
		WorkloadID:            intent.WorkloadID,
		LeaseID:               intent.LeaseID,
		LeaseWorker:           intent.LeaseWorker,
		IntentID:              intent.IntentID,
		IntentSHA256:          intent.SHA256,
		RequestRef:            intent.RequestRef,
		RequestSemanticSHA256: intent.RequestSemanticSHA256,
		ExecutionID:           intent.ExecutionID,
		Attempt:               intent.Attempt,
		Generation:            intent.Generation,
		FencingToken:          intent.FencingToken,
		CreateIdempotencyKey:  intent.CreateIdempotencyKey,
		ProviderHandle:        "provider/executions/dispatch-1",
		CapabilitySHA256:      intent.CapabilitySHA256,
		RecordedAt:            intent.RecordedAt.Add(time.Second),
	}
}

func mustDispatchIntentID(
	t *testing.T,
	runID string,
	stageID string,
	attempt int,
	generation int,
) string {
	t.Helper()
	intentID, err := runmodel.AgentStageDispatchIntentID(
		runID,
		stageID,
		attempt,
		generation,
	)
	if err != nil {
		t.Fatal(err)
	}
	return intentID
}
