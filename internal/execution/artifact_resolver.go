package execution

import (
	"context"
	"fmt"

	"github.com/abietic/argus/internal/artifactrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// GovernedArtifactResolver adapts the authority-aware Argus artifact
// repository to the provider-neutral execution boundary.
type GovernedArtifactResolver struct {
	Repository *artifactrepo.Repository
}

func (resolver GovernedArtifactResolver) Verify(
	ctx context.Context,
	tenantID string,
	workspaceID string,
	binding contractsv1alpha1.ArtifactBinding,
	purpose BindingPurpose,
) error {
	if resolver.Repository == nil {
		return fmt.Errorf("governed artifact repository is required")
	}
	switch purpose {
	case PurposeAgentStagePlan, PurposeExecutionSnapshot, PurposeReviewInput, PurposeUpstream,
		PurposeStageOutput, PurposeTraceManifest:
	default:
		return fmt.Errorf("unsupported artifact binding purpose %q", purpose)
	}
	_, _, err := resolver.Repository.Resolve(
		ctx,
		artifactrepo.Subject{
			TenantID: tenantID, WorkspaceID: workspaceID, Roles: []string{},
		},
		artifactrepo.Ref{
			URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
			SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
		},
		artifactrepo.UseRead,
	)
	return err
}
