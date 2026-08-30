package runmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

const (
	agentExecutionSemanticSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	agentExecutionBehaviorSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	agentExecutionArtifactSHA = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestSealAgentStagePlanAdmissionBindsSemanticAndArtifactIdentities(t *testing.T) {
	t.Parallel()

	input := validAgentStagePlanAdmission()
	input.AdmissionID = "caller-controlled"
	input.SHA256 = strings.Repeat("f", 64)
	sealed, err := SealAgentStagePlanAdmission(input)
	if err != nil {
		t.Fatalf("SealAgentStagePlanAdmission() error = %v", err)
	}
	wantID, err := AgentStagePlanAdmissionID(sealed.ReviewRunID, sealed.Stage.ID)
	if err != nil {
		t.Fatalf("AgentStagePlanAdmissionID() error = %v", err)
	}
	if sealed.AdmissionID != wantID {
		t.Fatalf("AdmissionID = %q, want %q", sealed.AdmissionID, wantID)
	}
	if sealed.SHA256 == "" || sealed.SHA256 == input.SHA256 {
		t.Fatalf("SHA256 = %q, want derived self digest", sealed.SHA256)
	}
	if sealed.PlanSemanticSHA256 == sealed.Plan.Local.SHA256 {
		t.Fatal("fixture collapsed semantic plan digest into artifact byte digest")
	}
	if sealed.ReceiptSemanticSHA256 == sealed.Sources.ConfigResolutionReceipt.Local.SHA256 {
		t.Fatal("fixture collapsed receipt semantic digest into artifact byte digest")
	}
	if err := sealed.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	again, err := SealAgentStagePlanAdmission(validAgentStagePlanAdmission())
	if err != nil {
		t.Fatalf("SealAgentStagePlanAdmission(again) error = %v", err)
	}
	if again != sealed {
		t.Fatalf("same admission input was not deterministic: %+v != %+v", again, sealed)
	}

	retry := validAgentStagePlanAdmission()
	retry.RecordedAt = retry.RecordedAt.Add(time.Second)
	resealed, err := SealAgentStagePlanAdmission(retry)
	if err != nil {
		t.Fatalf("SealAgentStagePlanAdmission(retry) error = %v", err)
	}
	if resealed.AdmissionID != sealed.AdmissionID {
		t.Fatalf("run+stage admission ID drifted: %q != %q", resealed.AdmissionID, sealed.AdmissionID)
	}
	if resealed.SHA256 == sealed.SHA256 {
		t.Fatal("recorded_at change did not change the admission self digest")
	}
}

func TestAgentStagePlanAdmissionRejectsBrokenClosure(t *testing.T) {
	t.Parallel()

	sealed, err := SealAgentStagePlanAdmission(validAgentStagePlanAdmission())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*AgentStagePlanAdmission)
		want   string
	}{
		{
			name: "self digest",
			mutate: func(value *AgentStagePlanAdmission) {
				value.SHA256 = strings.Repeat("f", 64)
			},
			want: "does not match",
		},
		{
			name: "run stage identity",
			mutate: func(value *AgentStagePlanAdmission) {
				value.AdmissionID = "run-1-agent-stage-admission-wrong"
			},
			want: "admission_id",
		},
		{
			name: "plan semantic identity",
			mutate: func(value *AgentStagePlanAdmission) {
				value.PlanID = "agent-stage-plan-ffffffffffffffffffffffff"
			},
			want: "plan_id",
		},
		{
			name: "receipt semantic identity",
			mutate: func(value *AgentStagePlanAdmission) {
				value.ReceiptID = "config-resolution-ffffffffffffffffffffffff"
			},
			want: "config_resolution_receipt_id",
		},
		{
			name: "source byte mismatch",
			mutate: func(value *AgentStagePlanAdmission) {
				value.Sources.ReviewInput.Governed.SHA256 = strings.Repeat("d", 64)
			},
			want: "exact same bytes",
		},
		{
			name: "source contract mismatch",
			mutate: func(value *AgentStagePlanAdmission) {
				value.Sources.Workflow.Governed.Contract = ContractReviewInput
			},
			want: "contract",
		},
		{
			name: "foreign tenant governed alias",
			mutate: func(value *AgentStagePlanAdmission) {
				value.Sources.ReviewSpec.Governed.URI = strings.Replace(
					value.Sources.ReviewSpec.Governed.URI,
					"/tenants/tenant-1/",
					"/tenants/tenant-2/",
					1,
				)
			},
			want: "another planning subject",
		},
		{
			name: "foreign workspace governed plan",
			mutate: func(value *AgentStagePlanAdmission) {
				value.Plan.Governed.URI = strings.Replace(
					value.Plan.Governed.URI,
					"/workspaces/workspace-1/",
					"/workspaces/workspace-2/",
					1,
				)
			},
			want: "another planning subject",
		},
		{
			name: "non content addressed governed object",
			mutate: func(value *AgentStagePlanAdmission) {
				prefix := value.Plan.Governed.URI[:strings.LastIndex(value.Plan.Governed.URI, "/")+1]
				value.Plan.Governed.URI = prefix + "mutable-object"
			},
			want: "object identity",
		},
		{
			name: "latest stage",
			mutate: func(value *AgentStagePlanAdmission) {
				value.Stage.Revision = "latest"
			},
			want: "must not use latest",
		},
		{
			name: "non utc timestamp",
			mutate: func(value *AgentStagePlanAdmission) {
				value.RecordedAt = time.Date(
					2026, 8, 24, 1, 2, 3, 0,
					time.FixedZone("offset", 8*60*60),
				)
			},
			want: "UTC",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := sealed
			test.mutate(&value)
			if err := value.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSealAgentStagePlanAdmissionRequiresEverySourceProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*AgentStagePlanAdmission)
		want   string
	}{
		{"execution snapshot", func(value *AgentStagePlanAdmission) {
			value.Sources.ExecutionSnapshot = AgentArtifactProjection{}
		}, "execution_snapshot"},
		{"config bundle", func(value *AgentStagePlanAdmission) {
			value.Sources.ConfigBundle = AgentArtifactProjection{}
		}, "config_bundle"},
		{"config receipt", func(value *AgentStagePlanAdmission) {
			value.Sources.ConfigResolutionReceipt = AgentArtifactProjection{}
		}, "config_resolution_receipt"},
		{"workflow", func(value *AgentStagePlanAdmission) {
			value.Sources.Workflow = AgentArtifactProjection{}
		}, "workflow"},
		{"review spec", func(value *AgentStagePlanAdmission) {
			value.Sources.ReviewSpec = AgentArtifactProjection{}
		}, "review_spec"},
		{"review input", func(value *AgentStagePlanAdmission) {
			value.Sources.ReviewInput = AgentArtifactProjection{}
		}, "review_input"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := validAgentStagePlanAdmission()
			test.mutate(&value)
			if _, err := SealAgentStagePlanAdmission(value); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf(
					"SealAgentStagePlanAdmission() error = %v, want containing %q",
					err,
					test.want,
				)
			}
		})
	}
}

func validAgentStagePlanAdmission() AgentStagePlanAdmission {
	subject := AgentPlanningSubject{
		TenantID:       "tenant-1",
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		RepositoryID:   "repository-1",
	}
	projection := func(contract, object string) AgentArtifactProjection {
		objectDigest := sha256.Sum256([]byte(object))
		return AgentArtifactProjection{
			Local: ArtifactRef{
				URI:       "artifact://local/sha256/" + agentExecutionArtifactSHA,
				SHA256:    agentExecutionArtifactSHA,
				SizeBytes: 128,
				Contract:  contract,
			},
			Governed: GovernedArtifactBinding{
				URI: "artifact://argus-local/tenants/" + subject.TenantID +
					"/workspaces/" + subject.WorkspaceID + "/objects/" +
					hex.EncodeToString(objectDigest[:]),
				SHA256:    agentExecutionArtifactSHA,
				SizeBytes: 128,
				Contract:  contract,
			},
		}
	}
	return AgentStagePlanAdmission{
		SchemaVersion: AgentStagePlanAdmissionSchemaVersion,
		Subject:       subject,
		ReviewRunID:   "run-1",
		Stage: AgentStageRef{
			ID: "agent-hypothesize", Revision: "1", SHA256: agentExecutionSemanticSHA,
		},
		PlanID:                "agent-stage-plan-" + agentExecutionSemanticSHA[:24],
		PlanSemanticSHA256:    agentExecutionSemanticSHA,
		PlanBehaviorSHA256:    agentExecutionBehaviorSHA,
		Plan:                  projection(ContractAgentStagePlan, "plan-object"),
		ReceiptID:             "config-resolution-" + agentExecutionSemanticSHA[:24],
		ReceiptSemanticSHA256: agentExecutionSemanticSHA,
		Sources: AgentStageSourceProjections{
			ExecutionSnapshot:       projection(ContractExecutionSnapshot, "snapshot-object"),
			ConfigBundle:            projection(ContractConfigBundle, "config-object"),
			ConfigResolutionReceipt: projection(ContractConfigResolutionReceipt, "receipt-object"),
			Workflow:                projection(ContractWorkflowDefinition, "workflow-object"),
			ReviewSpec:              projection(ContractReviewSpec, "spec-object"),
			ReviewInput:             projection(ContractReviewInput, "input-object"),
		},
		RecordedAt: time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
	}
}
