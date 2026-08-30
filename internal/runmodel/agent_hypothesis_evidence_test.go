package runmodel

import (
	"strings"
	"testing"
	"time"
)

const (
	agentEvidenceResultSHA = "7111111111111111111111111111111111111111111111111111111111111111"
	agentEvidenceOutputSHA = "7222222222222222222222222222222222222222222222222222222222222222"
	agentEvidenceTargetSHA = "7333333333333333333333333333333333333333333333333333333333333333"
)

func TestSealAgentStageHypothesisEvidenceBindsExecutionClosure(t *testing.T) {
	t.Parallel()

	evidence, admission, intent, binding, terminal := validSealedAgentStageHypothesisEvidence(t)
	if err := evidence.ValidateAgainstClosure(admission, intent, binding, terminal); err != nil {
		t.Fatalf("ValidateAgainstClosure() error = %v", err)
	}
	wantID, err := AgentStageHypothesisEvidenceID(
		evidence.ReviewRunID,
		binding.BindingID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.EvidenceID != wantID || evidence.SHA256 == "" {
		t.Fatalf("sealed identity = (%q, %q), want (%q, non-empty)",
			evidence.EvidenceID, evidence.SHA256, wantID)
	}
	if evidence.ResultRef.Contract != ContractStageExecutionResult ||
		evidence.Output.Local.Contract != ContractReviewHypothesisSet ||
		evidence.Output.Local.SHA256 != evidence.Output.Governed.SHA256 {
		t.Fatalf("artifact bindings are not closed: %+v", evidence)
	}

	again, _, _, _, _ := validSealedAgentStageHypothesisEvidence(t)
	if again != evidence {
		t.Fatalf("same evidence input was not deterministic: %+v != %+v", again, evidence)
	}

	changedInput := validAgentStageHypothesisEvidence(admission, intent, binding, terminal)
	changedInput.HypothesisSetID = "hypothesis-set-other"
	changed, err := SealAgentStageHypothesisEvidence(changedInput)
	if err != nil {
		t.Fatal(err)
	}
	if changed.EvidenceID != evidence.EvidenceID || changed.SHA256 == evidence.SHA256 {
		t.Fatal("changed result did not preserve binding authority key and change digest")
	}
}

func TestAgentStageHypothesisEvidenceRejectsTampering(t *testing.T) {
	t.Parallel()

	evidence, _, _, _, _ := validSealedAgentStageHypothesisEvidence(t)
	tests := []struct {
		name   string
		mutate func(*AgentStageHypothesisEvidence)
		want   string
	}{
		{"self digest", func(value *AgentStageHypothesisEvidence) {
			value.SHA256 = strings.Repeat("f", 64)
		}, "does not match"},
		{"authority id", func(value *AgentStageHypothesisEvidence) {
			value.EvidenceID = value.ReviewRunID + "-agent-stage-hypothesis-evidence-wrong"
		}, "evidence_id"},
		{"foreign binding", func(value *AgentStageHypothesisEvidence) {
			value.BindingID = "run-other-agent-stage-execution-binding-123"
		}, "not namespaced"},
		{"terminal gate digest", func(value *AgentStageHypothesisEvidence) {
			value.TerminalGateSHA256 = strings.Repeat("a", 64)
		}, "does not match"},
		{"result contract", func(value *AgentStageHypothesisEvidence) {
			value.ResultRef.Contract = ContractReviewHypothesisSet
		}, ContractStageExecutionResult},
		{"output contract", func(value *AgentStageHypothesisEvidence) {
			value.Output.Local.Contract = ContractStageExecutionResult
			value.Output.Governed.Contract = ContractStageExecutionResult
		}, ContractReviewHypothesisSet},
		{"output byte drift", func(value *AgentStageHypothesisEvidence) {
			value.Output.Governed.SizeBytes++
		}, "exact same bytes"},
		{"target digest", func(value *AgentStageHypothesisEvidence) {
			value.TargetDigest = "bad"
		}, "target_digest"},
		{"non utc", func(value *AgentStageHypothesisEvidence) {
			value.AdmittedAt = time.Date(
				2026, 8, 24, 4, 0, 0, 0,
				time.FixedZone("offset", 8*60*60),
			)
		}, "UTC"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := evidence
			test.mutate(&value)
			if err := value.Validate(); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestAgentStageHypothesisEvidenceRejectsStaleExecutionClosure(t *testing.T) {
	t.Parallel()

	_, admission, intent, binding, terminal := validSealedAgentStageHypothesisEvidence(t)
	tests := []struct {
		name   string
		mutate func(*AgentStageHypothesisEvidence)
	}{
		{"admission digest", func(value *AgentStageHypothesisEvidence) {
			value.AdmissionSHA256 = strings.Repeat("a", 64)
		}},
		{"intent digest", func(value *AgentStageHypothesisEvidence) {
			value.IntentSHA256 = strings.Repeat("b", 64)
		}},
		{"binding digest", func(value *AgentStageHypothesisEvidence) {
			value.BindingSHA256 = strings.Repeat("c", 64)
		}},
		{"terminal digest", func(value *AgentStageHypothesisEvidence) {
			value.TerminalGateSHA256 = strings.Repeat("e", 64)
		}},
		{"request semantic digest", func(value *AgentStageHypothesisEvidence) {
			value.RequestSemanticSHA256 = strings.Repeat("d", 64)
		}},
		{"result ref", func(value *AgentStageHypothesisEvidence) {
			value.ResultRef = ArtifactRef{
				URI:       "artifact://local/sha256/" + agentEvidenceResultSHA,
				SHA256:    agentEvidenceResultSHA,
				SizeBytes: 512,
				Contract:  ContractStageExecutionResult,
			}
		}},
		{"output projection", func(value *AgentStageHypothesisEvidence) {
			local := ArtifactRef{
				URI:       "artifact://local/sha256/" + agentEvidenceOutputSHA,
				SHA256:    agentEvidenceOutputSHA,
				SizeBytes: 1024,
				Contract:  ContractReviewHypothesisSet,
			}
			value.Output = AgentArtifactProjection{
				Local: local,
				Governed: GovernedArtifactBinding{
					URI: "artifact://argus-local/tenants/" + value.Subject.TenantID +
						"/workspaces/" + value.Subject.WorkspaceID +
						"/objects/" + local.SHA256,
					SHA256: local.SHA256, SizeBytes: local.SizeBytes, Contract: local.Contract,
				},
			}
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := validAgentStageHypothesisEvidence(admission, intent, binding, terminal)
			test.mutate(&input)
			stale, err := SealAgentStageHypothesisEvidence(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := stale.ValidateAgainstClosure(
				admission,
				intent,
				binding,
				terminal,
			); err == nil || !strings.Contains(err.Error(), "accepted terminal closure") {
				t.Fatalf("ValidateAgainstClosure() error = %v", err)
			}
		})
	}
}

func validSealedAgentStageHypothesisEvidence(
	t *testing.T,
) (
	AgentStageHypothesisEvidence,
	AgentStagePlanAdmission,
	AgentStageDispatchIntent,
	AgentStageExecutionBinding,
	AgentStageTerminalGate,
) {
	t.Helper()
	admission := validAgentDispatchAdmission(t)
	intent, err := SealAgentStageDispatchIntent(validAgentStageDispatchIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := SealAgentStageExecutionBinding(
		validAgentStageExecutionBinding(intent),
	)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := SealAgentStageTerminalGate(
		validAgentStageTerminalGate(admission, intent, binding),
	)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := SealAgentStageHypothesisEvidence(
		validAgentStageHypothesisEvidence(admission, intent, binding, terminal),
	)
	if err != nil {
		t.Fatal(err)
	}
	return evidence, admission, intent, binding, terminal
}

func validAgentStageHypothesisEvidence(
	admission AgentStagePlanAdmission,
	intent AgentStageDispatchIntent,
	binding AgentStageExecutionBinding,
	terminal AgentStageTerminalGate,
) AgentStageHypothesisEvidence {
	return AgentStageHypothesisEvidence{
		SchemaVersion:         AgentStageHypothesisEvidenceSchemaVersion,
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
		ResultRef:             terminal.Completion.ResultRef,
		Output:                terminal.Completion.Output,
		PlanID:                admission.PlanID,
		PlanSemanticSHA256:    admission.PlanSemanticSHA256,
		TargetDigest:          agentEvidenceTargetSHA,
		HypothesisSetID:       "hypothesis-set-1",
		AdmittedAt:            time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC),
	}
}
