package analyticsadapter

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/findinglineage"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// FindingLineageProjectionSource converts immutable Argus lineage artifacts
// into relationship-only analytics facts. It does not infer Outcomes, missed
// defects, or evaluation labels from relation names.
type FindingLineageProjectionSource struct {
	lineages *findinglineage.Repository
	runs     *runrepo.Repository
}

func NewFindingLineageProjectionSource(
	lineages *findinglineage.Repository,
	runs *runrepo.Repository,
) (*FindingLineageProjectionSource, error) {
	if lineages == nil || runs == nil {
		return nil, fmt.Errorf("finding lineage and run repositories are required")
	}
	return &FindingLineageProjectionSource{lineages: lineages, runs: runs}, nil
}

func (source *FindingLineageProjectionSource) FindingLineageFacts(
	ctx context.Context,
	scope Scope,
	window analytics.TimeWindow,
) (FindingLineageBatch, error) {
	if source == nil || source.lineages == nil || source.runs == nil {
		return FindingLineageBatch{}, fmt.Errorf("finding lineage projection source is not configured")
	}
	if err := contextErr(ctx); err != nil {
		return FindingLineageBatch{}, err
	}
	if err := scope.Validate(); err != nil {
		return FindingLineageBatch{}, err
	}
	if err := window.Validate(); err != nil {
		return FindingLineageBatch{}, err
	}
	records, err := source.lineages.List(findinglineage.ListFilter{RepositoryID: scope.RepositoryID})
	if err != nil {
		return FindingLineageBatch{}, fmt.Errorf("list finding lineages: %w", err)
	}
	facts := []analytics.FindingLineageFact{}
	for _, record := range records {
		if err := contextErr(ctx); err != nil {
			return FindingLineageBatch{}, err
		}
		if record.RecordedAt.Before(window.StartInclusive) || !record.RecordedAt.Before(window.EndExclusive) {
			continue
		}
		lineage := record.Lineage
		if lineage.Baseline.TenantID != scope.TenantID || lineage.Baseline.Repository.RepositoryID != scope.RepositoryID {
			continue
		}
		baselineOrganization, err := source.organizationForBinding(lineage.Baseline)
		if err != nil {
			return FindingLineageBatch{}, fmt.Errorf("bind lineage %q baseline scope: %w", lineage.LineageID, err)
		}
		variantOrganization, err := source.organizationForBinding(lineage.Variant)
		if err != nil {
			return FindingLineageBatch{}, fmt.Errorf("bind lineage %q variant scope: %w", lineage.LineageID, err)
		}
		if baselineOrganization != variantOrganization {
			return FindingLineageBatch{}, fmt.Errorf("lineage %q crosses organizations", lineage.LineageID)
		}
		if baselineOrganization != scope.OrganizationID {
			continue
		}
		ancestryDigest := ""
		if lineage.Ancestry != nil {
			ancestryDigest = lineage.Ancestry.EvidenceSHA256
		}
		for _, relation := range lineage.Relations {
			facts = append(facts, analytics.FindingLineageFact{
				SchemaVersion: analytics.FindingLineageFactSchemaVersion,
				FactID:        stableFactID("finding-lineage-fact", lineage.LineageID, relation.RelationID),
				LineageID:     lineage.LineageID, LineageArtifactSHA256: record.LineageRef.SHA256,
				RelationID: relation.RelationID, RelationType: string(relation.Type), RelationMethod: string(relation.Method),
				ReasonCode: relation.ReasonCode, FamilyKey: relation.FamilyKey,
				PolicyID: lineage.Policy.PolicyID, PolicyRevision: lineage.Policy.Revision, PolicySHA256: lineage.Policy.SHA256,
				AncestryAuthority: lineage.Policy.AncestryAuthority, AncestryEvidenceSHA256: ancestryDigest,
				RenameMappingCount: uint32(len(lineage.PathMappings)),
				BaselineRunID:      lineage.Baseline.RunID, VariantRunID: lineage.Variant.RunID,
				BaselineFindingIDs: slices.Clone(relation.BaselineFindingIDs), VariantFindingIDs: slices.Clone(relation.VariantFindingIDs),
				TenantID: lineage.Baseline.TenantID, OrganizationID: baselineOrganization,
				WorkspaceID: lineage.Baseline.WorkspaceID, RepositoryID: lineage.Baseline.Repository.RepositoryID,
				OccurredAt: record.RecordedAt,
			})
		}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].FactID < facts[j].FactID })
	batch := FindingLineageBatch{Completeness: analytics.CompletenessComplete, IncompleteReasons: []string{}, Facts: facts}
	if err := batch.Validate(); err != nil {
		return FindingLineageBatch{}, err
	}
	return batch, nil
}

func (source *FindingLineageProjectionSource) organizationForBinding(
	binding contractsv1alpha1.FindingLineageRunBinding,
) (string, error) {
	run, err := source.runs.LoadRun(binding.RunID)
	if err != nil {
		return "", err
	}
	runRef, err := source.runs.CommittedRunRef(binding.RunID)
	if err != nil {
		return "", err
	}
	if runRef.SHA256 != binding.RunSHA256 || run.HeadRevision != binding.HeadRevision {
		return "", fmt.Errorf("source run no longer binds lineage artifact")
	}
	snapshot, err := source.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return "", err
	}
	if snapshot.ReviewSpecRef.SHA256 != binding.ReviewSpecSHA256 {
		return "", fmt.Errorf("source snapshot does not bind lineage ReviewSpec")
	}
	data, err := source.runs.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return "", err
	}
	bundle, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return "", err
	}
	if bundle.Context.TenantID != binding.TenantID || bundle.Context.RepositoryID != binding.Repository.RepositoryID {
		return "", fmt.Errorf("source ConfigBundle scope does not bind lineage")
	}
	return bundle.Context.OrganizationID, nil
}
