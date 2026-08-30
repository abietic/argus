// Package agentadapter wires formal-agent application ports to the local run
// repository and the governed artifact repository. It owns authority-bearing
// defaults; application requests cannot choose roles, allowed uses, actors, or
// the logical artifact authority.
package agentadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	projectionActor = "argus-agent-plan-preparer"
	projectionAudit = "formal agent read-only artifact projection"
)

type Repository struct {
	runs      *runrepo.Repository
	artifacts *artifactrepo.Repository
	authority string
}

var (
	_ application.AgentPlanArtifactRepository       = (*Repository)(nil)
	_ application.AgentStagePlanAdmissionRepository = (*Repository)(nil)
	_ application.AgentStageDispatchRepository      = (*Repository)(nil)
	_ application.AgentStageResultRepository        = (*Repository)(nil)
	_ application.AgentStageHistoricalRepository    = (*Repository)(nil)
)

func New(
	runs *runrepo.Repository,
	artifacts *artifactrepo.Repository,
	authority string,
) (*Repository, error) {
	if runs == nil || artifacts == nil {
		return nil, fmt.Errorf("run and governed artifact repositories are required")
	}
	if authority == "" {
		return nil, fmt.Errorf("governed artifact authority is required")
	}
	return &Repository{runs: runs, artifacts: artifacts, authority: authority}, nil
}

func (repository *Repository) ExecutionSnapshotForRun(
	ctx context.Context,
	runID string,
) (runmodel.ExecutionSnapshot, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.ExecutionSnapshot{}, err
	}
	snapshot, err := repository.runs.ExecutionSnapshotForRun(runID)
	if err != nil {
		return runmodel.ExecutionSnapshot{}, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.ExecutionSnapshot{}, err
	}
	return snapshot, nil
}

func (repository *Repository) CommittedRunRef(
	ctx context.Context,
	runID string,
) (runmodel.ArtifactRef, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	ref, err := repository.runs.CommittedRunRef(runID)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return ref, nil
}

func (repository *Repository) ReadLocalArtifact(
	ctx context.Context,
	ref runmodel.ArtifactRef,
) ([]byte, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	data, err := repository.runs.ReadArtifact(ref)
	if err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

func (repository *Repository) PutLocalArtifact(
	ctx context.Context,
	contract string,
	data []byte,
) (runmodel.ArtifactRef, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	ref, err := repository.runs.PutArtifact(contract, data)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return ref, nil
}

func (repository *Repository) ProjectReadOnly(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	request application.AgentArtifactProjectionRequest,
) (contractsv1alpha1.ArtifactBinding, error) {
	if err := checkContext(ctx); err != nil {
		return contractsv1alpha1.ArtifactBinding{}, err
	}
	if err := (runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}).Validate(); err != nil {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"validate formal agent projection subject: %w",
			err,
		)
	}
	artifactSubject := subjectForArtifacts(subject)
	if err := artifactSubject.Validate(); err != nil {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"validate governed projection subject: %w",
			err,
		)
	}
	if request.Role == "" || request.ReviewRunID == "" || request.StageID == "" {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"projection role, review run, and stage are required",
		)
	}
	if err := request.Source.Validate(); err != nil {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"validate projection source: %w",
			err,
		)
	}
	if request.FrozenAt.IsZero() || request.FrozenAt.Location() != time.UTC {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"projection frozen_at must be a non-zero UTC timestamp",
		)
	}
	digest := sha256.Sum256(request.ExactContent)
	contentSHA256 := hex.EncodeToString(digest[:])
	if request.Source.SHA256 != contentSHA256 ||
		request.Source.SizeBytes != int64(len(request.ExactContent)) {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"projection source does not bind exact content",
		)
	}
	ref, record, err := repository.artifacts.Put(
		ctx,
		artifactSubject,
		artifactrepo.PutRequest{
			Authority: repository.authority,
			TenantID:  subject.TenantID, WorkspaceID: subject.WorkspaceID,
			Contract:    request.Source.Contract,
			AllowedUses: []artifactrepo.Use{artifactrepo.UseRead},
			Content:     bytes.Clone(request.ExactContent),
			Mutation: artifactrepo.Mutation{
				IdempotencyKey: projectionMutationKey(subject, request),
				Actor:          projectionActor, Audit: projectionAudit, At: request.FrozenAt,
			},
		},
	)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, err
	}
	if ref.SHA256 != request.Source.SHA256 || ref.SizeBytes != request.Source.SizeBytes ||
		ref.Contract != request.Source.Contract || record.State != artifactrepo.StateActive ||
		len(record.Metadata.AllowedUses) != 1 ||
		record.Metadata.AllowedUses[0] != artifactrepo.UseRead {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"governed artifact repository returned a mismatched read-only projection",
		)
	}
	return bindingFromRef(ref), nil
}

func (repository *Repository) VerifyRead(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	binding contractsv1alpha1.ArtifactBinding,
) ([]byte, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := (runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}).Validate(); err != nil {
		return nil, fmt.Errorf("validate formal agent read subject: %w", err)
	}
	use := artifactrepo.UseRead
	if binding.Contract == contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion {
		use = artifactrepo.UseSensitiveProcess
	}
	data, _, err := repository.artifacts.Resolve(
		ctx,
		subjectForArtifacts(subject),
		artifactrepo.Ref{
			URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
			SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
		},
		use,
	)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (repository *Repository) LookupAgentStagePlanAdmission(
	ctx context.Context,
	runID string,
	stageID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStagePlanAdmission, bool, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStagePlanAdmission{}, false, err
	}
	return repository.runs.LookupAgentStagePlanAdmission(runID, stageID, subject)
}

func (repository *Repository) AppendAgentStagePlanAdmission(
	ctx context.Context,
	admission runmodel.AgentStagePlanAdmission,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := repository.runs.AppendAgentStagePlanAdmission(admission); err != nil {
		return err
	}
	return checkContext(ctx)
}

func (repository *Repository) LookupAgentStageDispatchIntent(
	ctx context.Context,
	runID string,
	stageID string,
	attempt int,
	generation int,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageDispatchIntent, bool, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageDispatchIntent{}, false, err
	}
	intent, found, err := repository.runs.LookupAgentStageDispatchIntent(
		runID,
		stageID,
		attempt,
		generation,
		subject,
	)
	if err != nil {
		return runmodel.AgentStageDispatchIntent{}, false, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageDispatchIntent{}, false, err
	}
	return intent, found, nil
}

func (repository *Repository) ListSupersededClaimedAgentStageDispatchIntents(
	ctx context.Context,
	runID string,
	subject runmodel.AgentPlanningSubject,
) ([]runmodel.AgentStageDispatchIntent, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	intents, err := repository.runs.ListSupersededClaimedAgentStageDispatchIntents(
		runID,
		subject,
	)
	if err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	return intents, nil
}

func (repository *Repository) ListClaimedAgentStageDispatchIntents(
	ctx context.Context,
	runID string,
	subject runmodel.AgentPlanningSubject,
) ([]runmodel.AgentStageDispatchIntent, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	intents, err := repository.runs.ListClaimedAgentStageDispatchIntents(runID, subject)
	if err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	return intents, nil
}

func (repository *Repository) ClaimAgentStageDispatchIntent(
	ctx context.Context,
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	claimed, err := repository.runs.ClaimAgentStageDispatchIntent(intent)
	if err != nil {
		return false, err
	}
	// Do not reinterpret a committed claim as failure when caller cancellation
	// races the local append. The application owns a deadline-bounded durable
	// post-claim operation context; reporting false here would obscure which
	// exact provider Ensure is authorized for idempotent recovery.
	return claimed, nil
}

func (repository *Repository) IsAgentStageDispatchIntentClaimed(
	ctx context.Context,
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	claimed, err := repository.runs.IsAgentStageDispatchIntentClaimed(intent)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	return claimed, nil
}

func (repository *Repository) LookupAgentStageExecutionBinding(
	ctx context.Context,
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageExecutionBinding, bool, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageExecutionBinding{}, false, err
	}
	binding, found, err := repository.runs.LookupAgentStageExecutionBinding(
		runID,
		intentID,
		subject,
	)
	if err != nil {
		return runmodel.AgentStageExecutionBinding{}, false, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageExecutionBinding{}, false, err
	}
	return binding, found, nil
}

func (repository *Repository) AppendAgentStageExecutionBinding(
	ctx context.Context,
	binding runmodel.AgentStageExecutionBinding,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := repository.runs.AppendAgentStageExecutionBinding(binding); err != nil {
		return err
	}
	return checkContext(ctx)
}

func (repository *Repository) LookupAgentStageTerminalGate(
	ctx context.Context,
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageTerminalGate, bool, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageTerminalGate{}, false, err
	}
	gate, found, err := repository.runs.LookupAgentStageTerminalGate(
		runID,
		intentID,
		subject,
	)
	if err != nil {
		return runmodel.AgentStageTerminalGate{}, false, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageTerminalGate{}, false, err
	}
	return gate, found, nil
}

func (repository *Repository) AppendAgentStageTerminalGate(
	ctx context.Context,
	gate runmodel.AgentStageTerminalGate,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := repository.runs.AppendAgentStageTerminalGate(gate); err != nil {
		return err
	}
	return checkContext(ctx)
}

func (repository *Repository) LookupAgentStageResultCallbackReceipt(
	ctx context.Context,
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef, bool, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false, err
	}
	receipt, receiptRef, found, err :=
		repository.runs.LookupAgentStageResultCallbackReceipt(runID, bindingID, subject)
	if err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false, err
	}
	return receipt, receiptRef, found, nil
}

func (repository *Repository) AppendAgentStageResultCallbackReceipt(
	ctx context.Context,
	receipt runmodel.AgentStageResultCallbackReceipt,
	receiptRef runmodel.ArtifactRef,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := repository.runs.AppendAgentStageResultCallbackReceipt(
		receipt,
		receiptRef,
	); err != nil {
		return err
	}
	return checkContext(ctx)
}

func (repository *Repository) LookupAgentStageHypothesisEvidence(
	ctx context.Context,
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageHypothesisEvidence, bool, error) {
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, false, err
	}
	evidence, found, err := repository.runs.LookupAgentStageHypothesisEvidence(
		runID,
		bindingID,
		subject,
	)
	if err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, false, err
	}
	if err := checkContext(ctx); err != nil {
		return runmodel.AgentStageHypothesisEvidence{}, false, err
	}
	return evidence, found, nil
}

func (repository *Repository) AppendAgentStageHypothesisEvidence(
	ctx context.Context,
	evidence runmodel.AgentStageHypothesisEvidence,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := repository.runs.AppendAgentStageHypothesisEvidence(evidence); err != nil {
		return err
	}
	return checkContext(ctx)
}

func subjectForArtifacts(subject application.AgentPlanningSubject) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID:    subject.TenantID,
		WorkspaceID: subject.WorkspaceID,
		Roles:       []string{artifactrepo.RoleSensitiveProcessor},
	}
}

func bindingFromRef(ref artifactrepo.Ref) contractsv1alpha1.ArtifactBinding {
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
		},
		Contract: ref.Contract,
	}
}

func projectionMutationKey(
	subject application.AgentPlanningSubject,
	request application.AgentArtifactProjectionRequest,
) string {
	digest := sha256.New()
	for _, value := range []string{
		subject.TenantID,
		subject.OrganizationID,
		subject.WorkspaceID,
		subject.RepositoryID,
		request.ReviewRunID,
		request.StageID,
		request.Role,
		request.Source.SHA256,
		request.Source.Contract,
	} {
		_, _ = fmt.Fprintf(digest, "%d:%s", len(value), value)
	}
	_, _ = fmt.Fprintf(digest, "%d", request.Source.SizeBytes)
	return "agp-" + hex.EncodeToString(digest.Sum(nil))
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
