package controlplane

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/abietic/argus/internal/findingdecision"
	"github.com/abietic/argus/internal/publication"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// AuthorizePublication implements publication.RequestAuthorizer. It rebuilds
// the request from its narrow intent and requires byte-equivalent derived
// facts, which catches a superseding Decision or any frozen source/config
// substitution before the provider boundary.
func (service *Service) AuthorizePublication(
	ctx context.Context,
	request publication.Request,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return err
	}
	rebuilt, err := service.BuildPublicationRequest(publication.Intent{
		SchemaVersion: publication.IntentSchemaVersion,
		GrantID:       request.GrantID, CreatedAt: request.CreatedAt,
	})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(rebuilt, request) {
		return fmt.Errorf("publication request does not match current derived authority")
	}
	if service.grants == nil {
		return fmt.Errorf("publication grant ledger is not configured")
	}
	_, err = service.grants.Reserve(
		ctx, request.GrantID, request.GrantSHA256,
		request.PublicationID, request.IdempotencyKey, request.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("reserve publication grant: %w", err)
	}
	return nil
}

// RecordPublicationGrant creates a one-publication post-review authority. The
// review ExecutionSnapshot remains remote-deny; this grant is a separate,
// bounded fact rooted in the latest explicit publish Decision.
func (service *Service) RecordPublicationGrant(
	ctx context.Context,
	request publication.GrantRequest,
	mutation publication.GrantMutation,
) (publication.Grant, error) {
	if service.grants == nil {
		return publication.Grant{}, fmt.Errorf("publication grant ledger is not configured")
	}
	if err := request.Validate(); err != nil {
		return publication.Grant{}, err
	}
	if err := mutation.Validate(); err != nil {
		return publication.Grant{}, err
	}
	authority, err := service.resolvePublicationAuthority(
		request.RunID, request.FindingID, request.OccurredAt,
	)
	if err != nil {
		return publication.Grant{}, err
	}
	binding := publication.GrantBinding{
		TenantID: authority.spec.TenantID, WorkspaceID: authority.spec.WorkspaceID,
		DecisionID:            authority.latest.DecisionID,
		Provider:              request.Provider,
		RepositoryID:          request.RepositoryID,
		BaseRevision:          authority.detail.Run.BaseRevision,
		ExpectedHeadRevision:  authority.detail.Run.HeadRevision,
		TargetSnapshotRef:     publicationContentRef(authority.snapshot.TargetSnapshotRef),
		ConfigBundleRef:       publicationContentRef(authority.snapshot.ConfigBundleRef),
		FindingSourceRef:      authority.sourceRef,
		FindingSourceContract: authority.root.SourceContract,
	}
	return service.grants.Record(ctx, request, mutation, binding)
}

// BuildPublicationRequest converts a narrow caller Intent into a fully bound,
// immutable publication request. It performs no remote side effect.
func (service *Service) BuildPublicationRequest(
	intent publication.Intent,
) (publication.Request, error) {
	if err := intent.Validate(); err != nil {
		return publication.Request{}, err
	}
	if service.grants == nil {
		return publication.Request{}, fmt.Errorf("publication grant ledger is not configured")
	}
	grant, err := service.grants.Get(intent.GrantID)
	if err != nil {
		return publication.Request{}, err
	}
	if intent.CreatedAt.Before(grant.GrantedAt) || !intent.CreatedAt.Before(grant.ExpiresAt) {
		return publication.Request{}, publication.ErrGrantExpired
	}
	authority, err := service.resolvePublicationAuthority(
		grant.RunID, grant.FindingID, intent.CreatedAt,
	)
	if err != nil {
		return publication.Request{}, err
	}
	wantBinding := publication.GrantBinding{
		TenantID: authority.spec.TenantID, WorkspaceID: authority.spec.WorkspaceID,
		DecisionID:            authority.latest.DecisionID,
		Provider:              grant.Provider,
		RepositoryID:          grant.RepositoryID,
		BaseRevision:          authority.detail.Run.BaseRevision,
		ExpectedHeadRevision:  authority.detail.Run.HeadRevision,
		TargetSnapshotRef:     publicationContentRef(authority.snapshot.TargetSnapshotRef),
		ConfigBundleRef:       publicationContentRef(authority.snapshot.ConfigBundleRef),
		FindingSourceRef:      authority.sourceRef,
		FindingSourceContract: authority.root.SourceContract,
	}
	if !reflect.DeepEqual(grant.Binding(), wantBinding) {
		return publication.Request{}, fmt.Errorf("publication grant no longer matches committed authority")
	}

	projection, err := service.publicationProjection(authority.detail)
	if err != nil {
		return publication.Request{}, err
	}
	request := publication.Request{
		SchemaVersion:  publication.RequestSchemaVersion,
		PublicationID:  grant.PublicationID,
		IdempotencyKey: grant.PublicationIdempotencyKey,
		GrantID:        grant.GrantID, GrantSHA256: grant.GrantSHA256,
		TenantID: grant.TenantID, WorkspaceID: grant.WorkspaceID,
		RunID: grant.RunID, RunKind: publication.RunKindReview,
		RunStatus: publication.RunStatusSucceeded, TargetMode: publication.TargetModeDiff,
		Provider: grant.Provider, RepositoryID: grant.RepositoryID,
		ChangeKind: grant.ChangeKind, ChangeID: grant.ChangeID,
		BaseRevision: grant.BaseRevision, ExpectedHeadRevision: grant.ExpectedHeadRevision,
		TargetSnapshotRef: grant.TargetSnapshotRef, ConfigBundleRef: grant.ConfigBundleRef,
		FindingSourceRef:      grant.FindingSourceRef,
		FindingSourceContract: grant.FindingSourceContract,
		FindingID:             grant.FindingID, Fingerprint: projection.fingerprint,
		DecisionID: grant.DecisionID, TargetDigest: projection.targetDigest,
		Anchor: projection.anchor, Channel: grant.Channel, Message: projection.message,
		RemoteWrites:               "allow",
		ExpectedPermissionRevision: grant.ExpectedPermissionRevision,
		GrantExpiresAt:             grant.ExpiresAt,
		CreatedAt:                  intent.CreatedAt,
	}
	if err := request.Validate(); err != nil {
		return publication.Request{}, fmt.Errorf("build publication request: %w", err)
	}
	return request, nil
}

type publicationAuthority struct {
	detail    FindingDetail
	root      findingdecision.Root
	latest    findingdecision.Decision
	snapshot  runmodel.ExecutionSnapshot
	spec      contractsv1alpha1.ReviewSpec
	sourceRef publication.ContentRef
}

func (service *Service) resolvePublicationAuthority(
	runID string,
	findingID string,
	at time.Time,
) (publicationAuthority, error) {
	if service.decisions == nil {
		return publicationAuthority{}, fmt.Errorf("finding decision ledger is not configured")
	}
	detail, err := service.finding(runID, findingID)
	if err != nil {
		return publicationAuthority{}, err
	}
	if detail.Run.Kind != runmodel.RunKindReview ||
		detail.Run.Status != runmodel.RunStatusSucceeded ||
		detail.Run.TargetMode != runmodel.TargetModeDiff {
		return publicationAuthority{}, fmt.Errorf("only a succeeded non-replay diff review may publish")
	}
	root, err := decisionRoot(detail)
	if err != nil {
		return publicationAuthority{}, err
	}
	decisions, err := service.decisions.List(runID, findingID)
	if err != nil {
		return publicationAuthority{}, fmt.Errorf("load publication decision authority: %w", err)
	}
	if len(decisions) == 0 {
		return publicationAuthority{}, fmt.Errorf("finding has no post-review publication decision")
	}
	latest := decisions[len(decisions)-1]
	if latest.Action != findingdecision.ActionPublish {
		return publicationAuthority{}, fmt.Errorf("latest finding decision is not publish")
	}
	if latest.Root() != root {
		return publicationAuthority{}, fmt.Errorf("latest finding decision does not bind the committed source root")
	}
	if at.Before(latest.RecordedAt) {
		return publicationAuthority{}, fmt.Errorf("publication authority predates its publish decision")
	}
	snapshot, err := service.runs.LoadExecutionSnapshot(detail.Run.ExecutionSnapshotID)
	if err != nil {
		return publicationAuthority{}, fmt.Errorf("load publication execution snapshot: %w", err)
	}
	sourceRef, err := publicationSourceRef(detail.Run, root)
	if err != nil {
		return publicationAuthority{}, err
	}
	for _, input := range []struct {
		name string
		ref  runmodel.ArtifactRef
	}{
		{name: "ReviewSpec", ref: snapshot.ReviewSpecRef},
		{name: "ConfigBundle", ref: snapshot.ConfigBundleRef},
		{name: "TargetSnapshot", ref: snapshot.TargetSnapshotRef},
		{name: "FindingSource", ref: publicationRunArtifactRef(sourceRef, root.SourceContract)},
	} {
		if err := service.runs.CheckArtifactEligibility(input.ref, runrepo.ArtifactUsePublication); err != nil {
			return publicationAuthority{}, fmt.Errorf("%s is not publication eligible: %w", input.name, err)
		}
	}
	specData, err := service.runs.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return publicationAuthority{}, fmt.Errorf("read publication ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specData)
	if err != nil {
		return publicationAuthority{}, fmt.Errorf("decode publication ReviewSpec: %w", err)
	}
	if spec.Target.Mode != contractsv1alpha1.ReviewModeDiff || spec.Target.Diff == nil ||
		spec.Target.Diff.BaseRevision != detail.Run.BaseRevision ||
		spec.Target.Diff.HeadRevision != detail.Run.HeadRevision {
		return publicationAuthority{}, fmt.Errorf("committed ReviewSpec does not bind the run diff")
	}
	configData, err := service.runs.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return publicationAuthority{}, fmt.Errorf("read publication ConfigBundle: %w", err)
	}
	if _, err := reviewconfig.DecodeBundle(configData); err != nil {
		return publicationAuthority{}, fmt.Errorf("decode publication ConfigBundle: %w", err)
	}
	return publicationAuthority{
		detail: detail, root: root, latest: latest,
		snapshot: snapshot, spec: spec, sourceRef: sourceRef,
	}, nil
}

func publicationRunArtifactRef(
	ref publication.ContentRef,
	contract string,
) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes, Contract: contract,
	}
}

type publicationProjection struct {
	fingerprint  string
	targetDigest string
	anchor       publication.StableAnchor
	message      string
}

func (service *Service) publicationProjection(detail FindingDetail) (publicationProjection, error) {
	if detail.Finding != nil {
		finding := detail.Finding
		return publicationProjection{
			fingerprint: finding.Fingerprint, targetDigest: finding.TargetDigest,
			anchor: publication.StableAnchor{
				Path: finding.Path, Side: "head", StartLine: finding.StartLine,
				EndLine: finding.EndLine, TargetDigest: finding.TargetDigest,
			},
			message: finding.Message,
		}, nil
	}
	if detail.GovernedFinding == nil || detail.Run.GovernedReportRef == nil {
		return publicationProjection{}, fmt.Errorf("finding has no publishable source projection")
	}
	if detail.GovernedFinding.Anchor.Side != contractsv1alpha1.HypothesisAnchorNew {
		return publicationProjection{}, fmt.Errorf("formal finding is not anchored to the diff head")
	}
	data, err := service.runs.ReadArtifact(*detail.Run.GovernedReportRef)
	if err != nil {
		return publicationProjection{}, fmt.Errorf("read governed report for publication: %w", err)
	}
	report, err := contractsv1alpha1.DecodeGovernedReviewReport(data)
	if err != nil {
		return publicationProjection{}, fmt.Errorf("decode governed report for publication: %w", err)
	}
	finding := detail.GovernedFinding
	message := finding.Title + "\n\n" + finding.Description
	if finding.Impact != "" {
		message += "\n\nImpact: " + finding.Impact
	}
	return publicationProjection{
		fingerprint: finding.Fingerprint, targetDigest: report.TargetDigest,
		anchor: publication.StableAnchor{
			Path: finding.Anchor.Path, Side: "head", StartLine: finding.Anchor.StartLine,
			EndLine: finding.Anchor.EndLine, TargetDigest: report.TargetDigest,
		},
		message: strings.TrimSpace(message),
	}, nil
}

func publicationSourceRef(run runmodel.ReviewRun, root findingdecision.Root) (publication.ContentRef, error) {
	var ref *runmodel.ArtifactRef
	switch root.SourceContract {
	case runmodel.ContractFindingSet:
		ref = run.FindingSetRef
	case runmodel.ContractGovernedReviewReport:
		ref = run.GovernedReportRef
	default:
		return publication.ContentRef{}, fmt.Errorf("unsupported finding source contract %q", root.SourceContract)
	}
	if ref == nil || ref.Contract != root.SourceContract || ref.SHA256 != root.SourceSHA256 {
		return publication.ContentRef{}, fmt.Errorf("finding source ref does not match decision root")
	}
	return publicationContentRef(*ref), nil
}

func publicationContentRef(ref runmodel.ArtifactRef) publication.ContentRef {
	return publication.ContentRef{URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes}
}
