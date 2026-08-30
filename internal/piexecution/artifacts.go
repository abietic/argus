package piexecution

import (
	"context"
	"fmt"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/artifactrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type GovernedArtifactIO struct {
	repository *artifactrepo.Repository
	subject    artifactrepo.Subject
	authority  string
}

var _ ArtifactIO = (*GovernedArtifactIO)(nil)

func NewGovernedArtifactIO(
	repository *artifactrepo.Repository,
	subject application.AgentPlanningSubject,
	authority string,
) (*GovernedArtifactIO, error) {
	if repository == nil || authority == "" {
		return nil, fmt.Errorf("governed artifact repository and authority are required")
	}
	artifactSubject := artifactrepo.Subject{
		TenantID:    subject.TenantID,
		WorkspaceID: subject.WorkspaceID,
		Roles: []string{
			artifactrepo.RoleSensitiveProcessor,
			artifactrepo.RoleSensitiveReader,
		},
	}
	if err := artifactSubject.Validate(); err != nil {
		return nil, fmt.Errorf("validate Pi governed artifact subject: %w", err)
	}
	return &GovernedArtifactIO{
		repository: repository, subject: artifactSubject, authority: authority,
	}, nil
}

func (adapter *GovernedArtifactIO) Resolve(
	ctx context.Context,
	binding contractsv1alpha1.ArtifactBinding,
) ([]byte, error) {
	if adapter == nil || adapter.repository == nil {
		return nil, fmt.Errorf("governed Pi artifact IO is not initialized")
	}
	content, _, err := adapter.repository.Resolve(
		ctx,
		adapter.subject,
		artifactrepo.Ref{
			URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
			SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
		},
		artifactrepo.UseRead,
	)
	return content, err
}

func (adapter *GovernedArtifactIO) Publish(
	ctx context.Context,
	contract string,
	content []byte,
	idempotencyKey string,
	at time.Time,
) (contractsv1alpha1.ArtifactBinding, error) {
	if adapter == nil || adapter.repository == nil {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"governed Pi artifact IO is not initialized",
		)
	}
	allowedUses := []artifactrepo.Use{artifactrepo.UseRead}
	if contract == contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion {
		allowedUses = []artifactrepo.Use{
			artifactrepo.UseSensitiveProcess,
			artifactrepo.UseSensitiveRead,
		}
	}
	ref, record, err := adapter.repository.Put(
		ctx,
		adapter.subject,
		artifactrepo.PutRequest{
			Authority: adapter.authority,
			TenantID:  adapter.subject.TenantID, WorkspaceID: adapter.subject.WorkspaceID,
			Contract: contract, AllowedUses: allowedUses,
			Content: content,
			Mutation: artifactrepo.Mutation{
				IdempotencyKey: idempotencyKey,
				Actor:          "argus-local-pi-platform",
				Audit:          "publish immutable formal Pi execution output",
				At:             at.UTC(),
			},
		},
	)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, err
	}
	if record.State != artifactrepo.StateActive {
		return contractsv1alpha1.ArtifactBinding{}, fmt.Errorf(
			"published formal Pi output is not active",
		)
	}
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
		},
		Contract: ref.Contract,
	}, nil
}
