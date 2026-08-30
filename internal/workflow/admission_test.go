package workflow

import (
	"strings"
	"testing"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const admissionDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAdmitReviewAcceptsSafeExecutor(t *testing.T) {
	spec := admissionReviewSpec()
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "workflow", Revision: "1",
		Stages: []Stage{
			testStage("detect", "detect", []string{}, "safe-agent"),
		},
	}
	policy := AdmissionPolicy{
		Replay:           true,
		DefinitionSHA256: admissionDigest,
		Executors: map[string]ExecutorCapabilities{
			"safe-agent": {},
		},
	}
	if err := AdmitReview(spec, definition, policy); err != nil {
		t.Fatalf("AdmitReview() error = %v", err)
	}
}

func TestAdmitReviewRejectsRemoteStageWhenReviewDeniesWrites(t *testing.T) {
	spec := admissionReviewSpec()
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "workflow", Revision: "1",
		Stages: []Stage{
			func() Stage {
				stage := testStage("publish", "publish", []string{}, "publisher")
				stage.SideEffect = SideEffectRemotePublish
				stage.ReplayPolicy = ReplayPolicyForbidden
				stage.Retry.UnknownOutcome = UnknownOutcomeReconcile
				return stage
			}(),
		},
	}
	policy := AdmissionPolicy{
		DefinitionSHA256: admissionDigest,
		Executors: map[string]ExecutorCapabilities{
			"publisher": {RemoteWrite: true},
		},
	}
	if err := AdmitReview(spec, definition, policy); err == nil ||
		!strings.Contains(err.Error(), "denied by ReviewSpec") {
		t.Fatalf("AdmitReview() error = %v, want remote write rejection", err)
	}
}

func TestAdmitReviewRejectsUndeclaredOrUnsafeAuthority(t *testing.T) {
	tests := []struct {
		name         string
		capabilities ExecutorCapabilities
	}{
		{name: "remote write", capabilities: ExecutorCapabilities{RemoteWrite: true}},
		{name: "workspace write", capabilities: ExecutorCapabilities{WorkspaceWrite: true}},
		{name: "unrestricted network", capabilities: ExecutorCapabilities{UnrestrictedNetwork: true}},
		{name: "project config", capabilities: ExecutorCapabilities{ProjectConfigLoad: true}},
		{name: "background", capabilities: ExecutorCapabilities{BackgroundExecution: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := Definition{
				SchemaVersion: DefinitionSchemaVersion, ID: "workflow", Revision: "1",
				Stages: []Stage{
					testStage("detect", "detect", []string{}, "agent"),
				},
			}
			policy := AdmissionPolicy{
				DefinitionSHA256: admissionDigest,
				Executors:        map[string]ExecutorCapabilities{"agent": test.capabilities},
			}
			if err := AdmitReview(admissionReviewSpec(), definition, policy); err == nil {
				t.Fatal("AdmitReview() accepted unsafe authority")
			}
		})
	}
}

func TestAdmitReviewRejectsNonReplayableStage(t *testing.T) {
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "workflow", Revision: "1",
		Stages: []Stage{
			func() Stage {
				stage := testStage("detect", "detect", []string{}, "agent")
				stage.ReplayPolicy = ReplayPolicyForbidden
				return stage
			}(),
		},
	}
	policy := AdmissionPolicy{
		Replay:           true,
		DefinitionSHA256: admissionDigest,
		Executors:        map[string]ExecutorCapabilities{"agent": {}},
	}
	if err := AdmitReview(admissionReviewSpec(), definition, policy); err == nil ||
		!strings.Contains(err.Error(), "not safe for replay") {
		t.Fatalf("AdmitReview() error = %v, want replay rejection", err)
	}
}

func TestAdmitReviewRejectsWorkflowIdentityOrDigestMismatch(t *testing.T) {
	baseDefinition := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "workflow", Revision: "1",
		Stages: []Stage{
			testStage("detect", "detect", []string{}, "agent"),
		},
	}
	tests := []struct {
		name       string
		definition Definition
		digest     string
	}{
		{name: "id", definition: Definition{
			SchemaVersion: DefinitionSchemaVersion,
			ID:            "other",
			Revision:      baseDefinition.Revision,
			Stages:        baseDefinition.Stages,
		}, digest: admissionDigest},
		{name: "revision", definition: Definition{
			SchemaVersion: DefinitionSchemaVersion,
			ID:            baseDefinition.ID,
			Revision:      "2",
			Stages:        baseDefinition.Stages,
		}, digest: admissionDigest},
		{name: "digest", definition: baseDefinition, digest: strings.Repeat("b", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := AdmissionPolicy{
				DefinitionSHA256: test.digest,
				Executors:        map[string]ExecutorCapabilities{"agent": {}},
			}
			if err := AdmitReview(admissionReviewSpec(), test.definition, policy); err == nil {
				t.Fatal("AdmitReview() accepted a mismatched workflow")
			}
		})
	}
}

func admissionReviewSpec() contractsv1alpha1.ReviewSpec {
	return contractsv1alpha1.ReviewSpec{
		SchemaVersion:  contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:      "request-1",
		IdempotencyKey: "request-1",
		TenantID:       "tenant-local",
		WorkspaceID:    "workspace-argus",
		Repository:     contractsv1alpha1.RepositoryRef{Provider: "local", RepositoryID: "argus"},
		Target: contractsv1alpha1.ReviewTarget{
			Mode: contractsv1alpha1.ReviewModeScope,
			Scope: &contractsv1alpha1.ScopeTarget{
				Revision: "head", Include: []string{"**"}, Exclude: []string{},
			},
		},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID: "config", Revision: "1", SHA256: admissionDigest,
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: "workflow", Revision: "1", SHA256: admissionDigest,
		},
		RequestedOutput: []string{"findings"},
		RemoteWrites:    "deny",
	}
}
