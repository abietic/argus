package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestDisabledHailixPortFailsClosed(t *testing.T) {
	port := DisabledHailixPort{Reason: "immutable platform API is unavailable"}
	_, err := port.Ensure(context.Background(), contractsv1alpha1.StageExecutionRequest{})
	if err == nil || errors.Is(err, ErrPlatformIntegrationDisabled) {
		// An invalid request must be rejected before the disabled adapter state,
		// proving the boundary never forwards malformed authority.
		t.Fatalf("Create(invalid) error = %v, want request validation", err)
	}
	_, err = port.Ensure(context.Background(), validPlatformRequest(t))
	if !errors.Is(err, ErrPlatformIntegrationDisabled) {
		t.Fatalf("Create(valid) error = %v, want disabled boundary", err)
	}
	err = port.Cancel(context.Background(), CancelRequest{})
	if err == nil || errors.Is(err, ErrPlatformIntegrationDisabled) {
		t.Fatalf("Cancel(incomplete) error = %v, want exact identity rejection", err)
	}
	validCancel := CancelRequest{
		Authority:            terminalCancelAuthority("cancel-1"),
		ProviderHandle:       "provider/executions/opaque-1",
		ExecutionID:          "execution-1",
		Attempt:              1,
		Generation:           1,
		FencingToken:         1,
		CreateIdempotencyKey: "create-1",
		CancelIdempotencyKey: "cancel-1",
		RequestSHA256:        strings.Repeat("a", 64),
		CapabilitySHA256:     strings.Repeat("b", 64),
	}
	maxSafe := maxStageExecutionJSONInteger
	unsafeInt := int(maxSafe)
	if uint64(unsafeInt) != maxSafe {
		t.Skip("platform int cannot represent the JSON-safe boundary")
	}
	unsafeInt++
	for _, mutate := range []func(*CancelRequest){
		func(request *CancelRequest) { request.Attempt = unsafeInt },
		func(request *CancelRequest) { request.Generation = unsafeInt },
		func(request *CancelRequest) { request.FencingToken = maxSafe + 1 },
	} {
		request := validCancel
		mutate(&request)
		err = port.Cancel(context.Background(), request)
		if err == nil || errors.Is(err, ErrPlatformIntegrationDisabled) {
			t.Fatalf("Cancel(unsafe coordinate) error = %v, want exact identity rejection", err)
		}
	}
}

func validPlatformRequest(t *testing.T) contractsv1alpha1.StageExecutionRequest {
	t.Helper()
	digest := strings.Repeat("a", 64)
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind: "codex-acp", RuntimeID: "worker-1", RuntimeRevision: "1",
		RuntimeSHA256: digest, BuildIdentity: "build-1",
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:       []string{},
			ModelEgress:        contractsv1alpha1.AgentStageModelEgressProviderBrokerOnly,
			ToolNetwork:        contractsv1alpha1.AgentStageSideEffectsDeny,
			WorkspaceReads:     contractsv1alpha1.AgentStageWorkspaceReadFrozenInputOnly,
			WorkspaceWrites:    contractsv1alpha1.AgentStageSideEffectsDeny,
			RemoteWrites:       contractsv1alpha1.AgentStageSideEffectsDeny,
			MaxDelegationDepth: 0,
		},
		Trust: contractsv1alpha1.ExecutorTrust{
			Authority: contractsv1alpha1.ExecutorTrustAuthorityLocalHost,
			CapabilityVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-capability-verifier", Revision: "1", SHA256: digest,
			},
			CallbackVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-callback-verifier", Revision: "1", SHA256: digest,
			},
		},
	}
	capabilityDigest, err := contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	capability.SHA256 = capabilityDigest
	artifact := func(name string) contractsv1alpha1.ArtifactBinding {
		return contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:    "artifact://local/sha256/" + name,
				SHA256: digest, SizeBytes: 1,
			},
			Contract: "argus." + name + ".v1alpha1",
		}
	}
	plan := artifact("agent-stage-plan")
	plan.Contract = contractsv1alpha1.AgentStagePlanSchemaVersion
	request := contractsv1alpha1.StageExecutionRequest{
		SchemaVersion: contractsv1alpha1.StageExecutionRequestSchemaVersion,
		ExecutionID:   "execution-1", ReviewRunID: "run-1",
		TenantID: "tenant-1", WorkspaceID: "workspace-1",
		WorkloadID: "workload-1", LeaseID: "lease-1", LeaseWorkerID: "worker-1",
		Stage: contractsv1alpha1.VersionedRef{
			ID: "detect", Revision: "1", SHA256: digest,
		},
		Attempt: 1, Generation: 1, FencingToken: 1,
		IdempotencyKey:    "execution-1-detect-1",
		Plan:              plan,
		ExecutionSnapshot: artifact("execution-snapshot"),
		ReviewInput:       artifact("review-input"),
		Upstream:          []contractsv1alpha1.ArtifactBinding{},
		OutputContract:    "argus.stage-result.v1alpha1",
		Capability:        capability,
		Deadline:          time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC),
		SideEffects:       "deny",
	}
	request.ExecutionSnapshot.Contract = contractsv1alpha1.StageExecutionSnapshotContract
	request.ReviewInput.Contract = contractsv1alpha1.StageExecutionReviewInputContract
	sealed, err := contractsv1alpha1.SealStageExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}
