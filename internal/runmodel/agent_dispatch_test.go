package runmodel

import (
	"strings"
	"testing"
	"time"
)

const (
	agentDispatchAdmissionSemanticSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	agentDispatchBehaviorSHA          = "2222222222222222222222222222222222222222222222222222222222222222"
	agentDispatchArtifactSHA          = "3333333333333333333333333333333333333333333333333333333333333333"
	agentDispatchRequestSemanticSHA   = "4444444444444444444444444444444444444444444444444444444444444444"
	agentDispatchCapabilitySHA        = "5555555555555555555555555555555555555555555555555555555555555555"
)

func TestSealAgentStageDispatchIntentSeparatesRequestByteAndSemanticDigests(t *testing.T) {
	t.Parallel()

	input := validAgentStageDispatchIntent(t)
	input.IntentID = "caller-controlled"
	input.SHA256 = strings.Repeat("f", 64)
	sealed, err := SealAgentStageDispatchIntent(input)
	if err != nil {
		t.Fatalf("SealAgentStageDispatchIntent() error = %v", err)
	}
	wantID, err := AgentStageDispatchIntentID(
		sealed.ReviewRunID,
		sealed.Stage.ID,
		sealed.Attempt,
		sealed.Generation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.IntentID != wantID || sealed.SHA256 == "" ||
		sealed.SHA256 == input.SHA256 {
		t.Fatalf("sealed intent identity = (%q, %q)", sealed.IntentID, sealed.SHA256)
	}
	if sealed.RequestRef.SHA256 == sealed.RequestSemanticSHA256 {
		t.Fatal("fixture collapsed complete request byte digest into request semantic digest")
	}
	if sealed.RequestRef.Contract != ContractStageExecutionRequest {
		t.Fatalf("request contract = %q", sealed.RequestRef.Contract)
	}
	if sealed.CreateIdempotencyKey == sealed.CancelIdempotencyKey {
		t.Fatal("create and cancel idempotency keys are not independent")
	}
	if err := sealed.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	again, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	if again != sealed {
		t.Fatalf("same intent input was not deterministic: %+v != %+v", again, sealed)
	}

	changed := validAgentStageDispatchIntent(t)
	changed.ExecutionID = "execution-other"
	changedSealed, err := SealAgentStageDispatchIntent(changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedSealed.IntentID != sealed.IntentID || changedSealed.SHA256 == sealed.SHA256 {
		t.Fatal("execution drift did not preserve the authority key and change payload digest")
	}
}

func TestAgentStageDispatchIntentRejectsTampering(t *testing.T) {
	t.Parallel()

	sealed, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*AgentStageDispatchIntent)
		want   string
	}{
		{"self digest", func(value *AgentStageDispatchIntent) {
			value.SHA256 = strings.Repeat("f", 64)
		}, "does not match"},
		{"authority id", func(value *AgentStageDispatchIntent) {
			value.IntentID = "run-1-agent-stage-dispatch-wrong"
		}, "intent_id"},
		{"admission digest", func(value *AgentStageDispatchIntent) {
			value.AdmissionSHA256 = "bad"
		}, "admission_sha256"},
		{"request local uri", func(value *AgentStageDispatchIntent) {
			value.RequestRef.URI = "artifact://remote/sha256/" + value.RequestRef.SHA256
		}, "artifact URI"},
		{"request contract", func(value *AgentStageDispatchIntent) {
			value.RequestRef.Contract = ContractReviewInput
		}, "contract"},
		{"request semantic sha", func(value *AgentStageDispatchIntent) {
			value.RequestSemanticSHA256 = "bad"
		}, "request_semantic_sha256"},
		{"fencing token", func(value *AgentStageDispatchIntent) {
			value.FencingToken = 0
		}, "positive"},
		{"unsafe attempt", func(value *AgentStageDispatchIntent) {
			value.Attempt = int(maxAgentJSONSafeInteger + 1)
		}, "JSON-safe"},
		{"unsafe generation", func(value *AgentStageDispatchIntent) {
			value.Generation = int(maxAgentJSONSafeInteger + 1)
		}, "JSON-safe"},
		{"shared cancel key", func(value *AgentStageDispatchIntent) {
			value.CancelIdempotencyKey = value.CreateIdempotencyKey
		}, "independent"},
		{"non utc", func(value *AgentStageDispatchIntent) {
			value.RecordedAt = time.Date(
				2026, 8, 24, 1, 0, 0, 0,
				time.FixedZone("offset", 8*60*60),
			)
		}, "UTC"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := sealed
			test.mutate(&value)
			if err := value.Validate(); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestAgentStageDispatchIntentIDRejectsJSONUnsafeCoordinate(t *testing.T) {
	t.Parallel()
	if _, err := AgentStageDispatchIntentID(
		"run-1",
		"stage-1",
		int(maxAgentJSONSafeInteger+1),
		1,
	); err == nil || !strings.Contains(err.Error(), "JSON-safe") {
		t.Fatalf("AgentStageDispatchIntentID() error = %v", err)
	}
}

func TestAgentStageExecutionBindingEchoesExactIntent(t *testing.T) {
	t.Parallel()

	intent, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := SealAgentStageExecutionBinding(
		validAgentStageExecutionBinding(intent),
	)
	if err != nil {
		t.Fatalf("SealAgentStageExecutionBinding() error = %v", err)
	}
	if err := binding.ValidateAgainstIntent(intent); err != nil {
		t.Fatalf("ValidateAgainstIntent() error = %v", err)
	}
	wantID, err := AgentStageExecutionBindingID(intent.ReviewRunID, intent.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.BindingID != wantID {
		t.Fatalf("BindingID = %q, want %q", binding.BindingID, wantID)
	}

	tests := []struct {
		name   string
		mutate func(*AgentStageExecutionBinding)
	}{
		{"intent digest", func(value *AgentStageExecutionBinding) {
			value.IntentSHA256 = strings.Repeat("f", 64)
		}},
		{"request ref", func(value *AgentStageExecutionBinding) {
			value.RequestRef.SizeBytes++
		}},
		{"request semantic sha", func(value *AgentStageExecutionBinding) {
			value.RequestSemanticSHA256 = strings.Repeat("f", 64)
		}},
		{"execution", func(value *AgentStageExecutionBinding) {
			value.ExecutionID = "execution-other"
		}},
		{"attempt", func(value *AgentStageExecutionBinding) {
			value.Attempt++
		}},
		{"generation", func(value *AgentStageExecutionBinding) {
			value.Generation++
		}},
		{"fence", func(value *AgentStageExecutionBinding) {
			value.FencingToken++
		}},
		{"create key", func(value *AgentStageExecutionBinding) {
			value.CreateIdempotencyKey = "create-other"
		}},
		{"capability", func(value *AgentStageExecutionBinding) {
			value.CapabilitySHA256 = strings.Repeat("f", 64)
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := validAgentStageExecutionBinding(intent)
			test.mutate(&input)
			changed, err := SealAgentStageExecutionBinding(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := changed.ValidateAgainstIntent(intent); err == nil ||
				!strings.Contains(err.Error(), "exact dispatch intent") {
				t.Fatalf("ValidateAgainstIntent() error = %v", err)
			}
		})
	}
}

func TestSameAgentStageExecutionDecisionIgnoresOnlyHostPersistenceMetadata(t *testing.T) {
	t.Parallel()

	intent, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := SealAgentStageExecutionBinding(validAgentStageExecutionBinding(intent))
	if err != nil {
		t.Fatal(err)
	}
	same := first
	same.BindingID = "host-observer-binding-id"
	same.SHA256 = strings.Repeat("f", 64)
	same.RecordedAt = same.RecordedAt.Add(time.Second)
	if !SameAgentStageExecutionDecision(first, same) {
		t.Fatal("host persistence metadata changed the provider decision")
	}

	changed := same
	changed.ProviderHandle = "provider/executions/opaque-2"
	if SameAgentStageExecutionDecision(first, changed) {
		t.Fatal("provider handle drift was treated as the same decision")
	}
}

func TestAgentStageExecutionBindingRejectsInvalidProviderHandle(t *testing.T) {
	t.Parallel()

	intent, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"", " leading", "trailing ", "provider\nhandle", strings.Repeat("x", 513)} {
		input := validAgentStageExecutionBinding(intent)
		input.ProviderHandle = handle
		if _, err := SealAgentStageExecutionBinding(input); err == nil ||
			!strings.Contains(err.Error(), "provider_handle") {
			t.Fatalf("handle %q error = %v", handle, err)
		}
	}
}

func TestAgentStageExecutionBindingRejectsJSONUnsafeCoordinate(t *testing.T) {
	t.Parallel()
	intent, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	binding := validAgentStageExecutionBinding(intent)
	binding.Generation = int(maxAgentJSONSafeInteger + 1)
	if _, err := SealAgentStageExecutionBinding(binding); err == nil ||
		!strings.Contains(err.Error(), "JSON-safe") {
		t.Fatalf("SealAgentStageExecutionBinding() error = %v", err)
	}
}

func TestAgentDispatchClosureRejectsNonMonotonicRecordedTimes(t *testing.T) {
	t.Parallel()

	admission := validAgentDispatchAdmission(t)
	intentInput := validAgentStageDispatchIntent(t)
	intentInput.RecordedAt = admission.RecordedAt.Add(-time.Second)
	intent, err := SealAgentStageDispatchIntent(intentInput)
	if err != nil {
		t.Fatalf("SealAgentStageDispatchIntent() error = %v", err)
	}
	if err := intent.ValidateAgainstAdmission(admission); err == nil ||
		!strings.Contains(err.Error(), "precedes") {
		t.Fatalf("ValidateAgainstAdmission() error = %v", err)
	}

	intent, err = SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	bindingInput := validAgentStageExecutionBinding(intent)
	bindingInput.RecordedAt = intent.RecordedAt.Add(-time.Second)
	binding, err := SealAgentStageExecutionBinding(bindingInput)
	if err != nil {
		t.Fatalf("SealAgentStageExecutionBinding() error = %v", err)
	}
	if err := binding.ValidateAgainstIntent(intent); err == nil ||
		!strings.Contains(err.Error(), "precedes") {
		t.Fatalf("ValidateAgainstIntent() error = %v", err)
	}
}

func validAgentStageDispatchIntent(t *testing.T) AgentStageDispatchIntent {
	t.Helper()
	admission := validAgentDispatchAdmission(t)
	return AgentStageDispatchIntent{
		SchemaVersion:      AgentStageDispatchIntentSchemaVersion,
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
		RequestRef: ArtifactRef{
			URI:       "artifact://local/sha256/" + agentDispatchArtifactSHA,
			SHA256:    agentDispatchArtifactSHA,
			SizeBytes: 384,
			Contract:  ContractStageExecutionRequest,
		},
		RequestSemanticSHA256: agentDispatchRequestSemanticSHA,
		CapabilitySHA256:      agentDispatchCapabilitySHA,
		ExecutionID:           "execution-1",
		Attempt:               1,
		Generation:            1,
		FencingToken:          7,
		CreateIdempotencyKey:  "create-execution-1-attempt-1-generation-1",
		CancelIdempotencyKey:  "cancel-execution-1-attempt-1-generation-1",
		RecordedAt:            time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC),
	}
}

func validAgentStageExecutionBinding(
	intent AgentStageDispatchIntent,
) AgentStageExecutionBinding {
	return AgentStageExecutionBinding{
		SchemaVersion:         AgentStageExecutionBindingSchemaVersion,
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
		ProviderHandle:        "provider/executions/opaque-1",
		CapabilitySHA256:      intent.CapabilitySHA256,
		RecordedAt:            intent.RecordedAt.Add(time.Second),
	}
}

func validAgentDispatchAdmission(t *testing.T) AgentStagePlanAdmission {
	t.Helper()
	subject := AgentPlanningSubject{
		TenantID:       "tenant-1",
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		RepositoryID:   "repository-1",
	}
	projection := func(contract string) AgentArtifactProjection {
		return AgentArtifactProjection{
			Local: ArtifactRef{
				URI:       "artifact://local/sha256/" + agentDispatchArtifactSHA,
				SHA256:    agentDispatchArtifactSHA,
				SizeBytes: 384,
				Contract:  contract,
			},
			Governed: GovernedArtifactBinding{
				URI: "artifact://argus-local/tenants/" + subject.TenantID +
					"/workspaces/" + subject.WorkspaceID +
					"/objects/" + agentDispatchArtifactSHA,
				SHA256:    agentDispatchArtifactSHA,
				SizeBytes: 384,
				Contract:  contract,
			},
		}
	}
	admission, err := SealAgentStagePlanAdmission(AgentStagePlanAdmission{
		SchemaVersion: AgentStagePlanAdmissionSchemaVersion,
		Subject:       subject,
		ReviewRunID:   "run-1",
		Stage: AgentStageRef{
			ID:       "agent-hypothesize",
			Revision: "1",
			SHA256:   agentDispatchAdmissionSemanticSHA,
		},
		PlanID:                "agent-stage-plan-" + agentDispatchAdmissionSemanticSHA[:24],
		PlanSemanticSHA256:    agentDispatchAdmissionSemanticSHA,
		PlanBehaviorSHA256:    agentDispatchBehaviorSHA,
		Plan:                  projection(ContractAgentStagePlan),
		ReceiptID:             "config-resolution-" + agentDispatchAdmissionSemanticSHA[:24],
		ReceiptSemanticSHA256: agentDispatchAdmissionSemanticSHA,
		Sources: AgentStageSourceProjections{
			ExecutionSnapshot:       projection(ContractExecutionSnapshot),
			ConfigBundle:            projection(ContractConfigBundle),
			ConfigResolutionReceipt: projection(ContractConfigResolutionReceipt),
			Workflow:                projection(ContractWorkflowDefinition),
			ReviewSpec:              projection(ContractReviewSpec),
			ReviewInput:             projection(ContractReviewInput),
		},
		RecordedAt: time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return admission
}
