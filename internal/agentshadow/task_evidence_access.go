package agentshadow

import (
	"context"
	"fmt"

	"github.com/abietic/argus/internal/artifactrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// ReadTaskEvidence is the only user-facing disclosure path for exact task
// evidence. The governed artifact repository persists the purpose-bound audit
// receipt before these bytes are returned.
func (service *Service) ReadTaskEvidence(
	ctx context.Context,
	request TaskEvidenceReadRequest,
) (TaskEvidenceReadResult, error) {
	if service == nil || service.repository == nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("agent review shadow service is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return TaskEvidenceReadResult{}, err
	}
	if err := validateScope(request.Scope); err != nil {
		return TaskEvidenceReadResult{}, err
	}
	if err := validateIdentifier("manifest_id", request.ManifestID); err != nil {
		return TaskEvidenceReadResult{}, err
	}
	if err := request.Access.Validate(); err != nil {
		return TaskEvidenceReadResult{}, err
	}
	record, err := service.repository.loadRecord(request.ManifestID)
	if err != nil {
		return TaskEvidenceReadResult{}, err
	}
	if record.TenantID != request.Scope.TenantID ||
		record.WorkspaceID != request.Scope.WorkspaceID {
		return TaskEvidenceReadResult{}, artifactrepo.ErrUnauthorized
	}
	data, _, proof, err := service.repository.resolveTaskEvidenceForDisclosure(
		ctx,
		request.Scope,
		record.TaskEvidenceCollectionRef,
		request.Access,
	)
	if err != nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("disclose committed task evidence: %w", err)
	}
	evidence, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(data)
	if err != nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("%w: committed task evidence: %v", ErrCorrupt, err)
	}
	planData, err := service.repository.resolveArtifact(ctx, request.Scope, record.AgentReviewPlanRef)
	if err != nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("resolve task evidence plan: %w", err)
	}
	plan, err := contractsv1alpha1.DecodeAgentReviewPlan(planData)
	if err != nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("%w: task evidence plan: %v", ErrCorrupt, err)
	}
	receiptData, err := service.repository.resolveArtifact(ctx, request.Scope, record.ReceiptCollectionRef)
	if err != nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("resolve task evidence receipts: %w", err)
	}
	receipts, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(receiptData)
	if err != nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("%w: task evidence receipts: %v", ErrCorrupt, err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(evidence, plan, receipts); err != nil {
		return TaskEvidenceReadResult{}, fmt.Errorf("%w: task evidence bindings: %v", ErrCorrupt, err)
	}
	return TaskEvidenceReadResult{Evidence: evidence, Proof: proof}, nil
}

// RevokeTaskEvidence applies the MVP retention policy: evidence is retained
// until an explicit, audited tombstone. The logical object becomes
// irreversibly unreadable; shared content-addressed bytes are left for a future
// reference-aware garbage collector rather than being unsafely unlinked.
func (service *Service) RevokeTaskEvidence(
	ctx context.Context,
	request TaskEvidenceRevokeRequest,
) (TaskEvidenceSummary, error) {
	if service == nil || service.repository == nil {
		return TaskEvidenceSummary{}, fmt.Errorf("agent review shadow service is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return TaskEvidenceSummary{}, err
	}
	if err := validateScope(request.Scope); err != nil {
		return TaskEvidenceSummary{}, err
	}
	if err := validateIdentifier("manifest_id", request.ManifestID); err != nil {
		return TaskEvidenceSummary{}, err
	}
	record, err := service.repository.loadRecord(request.ManifestID)
	if err != nil {
		return TaskEvidenceSummary{}, err
	}
	if record.TenantID != request.Scope.TenantID ||
		record.WorkspaceID != request.Scope.WorkspaceID {
		return TaskEvidenceSummary{}, artifactrepo.ErrUnauthorized
	}
	mutation := artifactrepo.Mutation{
		IdempotencyKey: request.IdempotencyKey,
		Actor:          request.Actor,
		Audit:          "revoke exact local task evidence",
		At:             request.At,
	}
	state, err := service.repository.tombstoneTaskEvidence(
		ctx,
		request.Scope,
		record.TaskEvidenceCollectionRef,
		request.Reason,
		mutation,
	)
	if err != nil {
		return TaskEvidenceSummary{}, fmt.Errorf("revoke task evidence: %w", err)
	}
	return TaskEvidenceSummary{
		Ref:           record.TaskEvidenceCollectionRef,
		ContentPolicy: contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		State:         string(state.State), ExportPolicy: "deny",
		RetentionPolicy:   "until_explicit_revocation",
		DeletionSemantics: "logical_tombstone_shared_content_gc_deferred",
	}, nil
}
