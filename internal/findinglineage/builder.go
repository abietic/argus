package findinglineage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type runSource interface {
	LoadCommittedRunResult(string) (runrepo.CommittedRunResult, error)
	LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error)
	CommittedRunRef(string) (runmodel.ArtifactRef, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
	PutArtifact(string, []byte) (runmodel.ArtifactRef, error)
}

type ancestrySource interface {
	ResolveFindingLineageEvidence(context.Context, string, string, string, string, uint16) (contractsv1alpha1.FindingLineageGitAncestry, []contractsv1alpha1.FindingLineagePathMapping, error)
}

type sourceRun struct {
	binding        contractsv1alpha1.FindingLineageRunBinding
	findings       []contractsv1alpha1.FindingLineageFinding
	repositoryPath string
}

func buildFindingLineage(
	ctx context.Context,
	source runSource,
	ancestry ancestrySource,
	request BuildRequest,
	generatedAt time.Time,
) (contractsv1alpha1.FindingLineage, error) {
	if err := ctx.Err(); err != nil {
		return contractsv1alpha1.FindingLineage{}, err
	}
	baseline, err := loadSourceRun(source, request.BaselineRunID)
	if err != nil {
		return contractsv1alpha1.FindingLineage{}, fmt.Errorf("load baseline: %w", err)
	}
	variant, err := loadSourceRun(source, request.VariantRunID)
	if err != nil {
		return contractsv1alpha1.FindingLineage{}, fmt.Errorf("load variant: %w", err)
	}
	lineage := contractsv1alpha1.FindingLineage{
		Policy: request.Policy, Baseline: baseline.binding, Variant: variant.binding,
		BaselineFindings: baseline.findings, VariantFindings: variant.findings,
		PathMappings: []contractsv1alpha1.FindingLineagePathMapping{},
		GeneratedAt:  generatedAt.UTC(),
	}
	if request.Policy.AncestryAuthority == "local_git_object_graph" {
		if ancestry == nil {
			return contractsv1alpha1.FindingLineage{}, fmt.Errorf("Git-aware lineage evidence source is required")
		}
		evidence, mappings, err := ancestry.ResolveFindingLineageEvidence(ctx, baseline.repositoryPath, variant.repositoryPath, baseline.binding.HeadRevision, variant.binding.HeadRevision, request.Policy.RenameThresholdBPS)
		if err != nil {
			return contractsv1alpha1.FindingLineage{}, fmt.Errorf("resolve Git lineage evidence: %w", err)
		}
		lineage.Ancestry, lineage.PathMappings = &evidence, mappings
	}
	lineage.Relations = matchFindings(baseline.findings, variant.findings, lineage.PathMappings)
	contractsv1alpha1.CanonicalizeFindingLineage(&lineage)
	return contractsv1alpha1.SealFindingLineage(lineage)
}

func loadSourceRun(source runSource, runID string) (sourceRun, error) {
	result, err := source.LoadCommittedRunResult(runID)
	if err != nil {
		return sourceRun{}, err
	}
	run := result.Run
	snapshot, err := source.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return sourceRun{}, fmt.Errorf("load ExecutionSnapshot: %w", err)
	}
	if run.Status != runmodel.RunStatusSucceeded || result.GovernedReport == nil ||
		run.CandidateSetRef == nil || run.VerificationLedgerRef == nil ||
		run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil || run.GovernedReportRef == nil {
		return sourceRun{}, fmt.Errorf("run %q is not a committed formal governed review", runID)
	}
	specData, err := source.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return sourceRun{}, fmt.Errorf("read ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specData)
	if err != nil {
		return sourceRun{}, err
	}
	specDigest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return sourceRun{}, err
	}
	if spec.RequestID != run.RunID || specDigest != snapshot.ReviewSpecRef.SHA256 ||
		spec.Target.Mode != contractsv1alpha1.ReviewMode(run.TargetMode) ||
		result.GovernedReport.ReviewRunID != run.RunID ||
		result.GovernedReport.TargetDigest == "" {
		return sourceRun{}, fmt.Errorf("ReviewSpec does not bind committed run")
	}
	base, head, ok := reviewSpecRevisions(spec)
	if !ok || base != run.BaseRevision || head != run.HeadRevision {
		return sourceRun{}, fmt.Errorf("ReviewSpec target revisions do not bind committed run")
	}
	runRef, err := source.CommittedRunRef(runID)
	if err != nil {
		return sourceRun{}, fmt.Errorf("resolve committed run: %w", err)
	}
	report := result.GovernedReport
	binding := contractsv1alpha1.FindingLineageRunBinding{
		RunID: run.RunID, RunSHA256: runRef.SHA256,
		TenantID: spec.TenantID, WorkspaceID: spec.WorkspaceID, Repository: spec.Repository,
		TargetMode:   contractsv1alpha1.ReviewMode(run.TargetMode),
		BaseRevision: run.BaseRevision, HeadRevision: run.HeadRevision,
		TargetDigest:             report.TargetDigest,
		ReviewSpecSHA256:         snapshot.ReviewSpecRef.SHA256,
		TargetSnapshotSHA256:     run.TargetSnapshotRef.SHA256,
		CandidateSetSHA256:       run.CandidateSetRef.SHA256,
		VerificationLedgerSHA256: run.VerificationLedgerRef.SHA256,
		CalibrationLedgerSHA256:  run.CalibrationLedgerRef.SHA256,
		SuppressionLedgerSHA256:  run.SuppressionLedgerRef.SHA256,
		GovernedReportSHA256:     run.GovernedReportRef.SHA256,
	}
	findings := make([]contractsv1alpha1.FindingLineageFinding, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, contractsv1alpha1.FindingLineageFinding{
			RunID: runID, FindingID: finding.FindingID, Fingerprint: finding.Fingerprint,
			FamilyKey:   contractsv1alpha1.FindingLineageFamilyKey(finding),
			CandidateID: finding.CandidateID, Dimension: finding.Dimension,
			Category: finding.Category, Severity: finding.Severity, Title: finding.Title,
			Anchor: finding.Anchor,
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].FindingID < findings[j].FindingID })
	return sourceRun{binding: binding, findings: findings, repositoryPath: run.RepositoryPath}, nil
}

func reviewSpecRevisions(spec contractsv1alpha1.ReviewSpec) (string, string, bool) {
	switch spec.Target.Mode {
	case contractsv1alpha1.ReviewModeDiff:
		if spec.Target.Diff == nil {
			return "", "", false
		}
		return spec.Target.Diff.BaseRevision, spec.Target.Diff.HeadRevision, true
	case contractsv1alpha1.ReviewModeSelection:
		if spec.Target.Selection == nil {
			return "", "", false
		}
		return spec.Target.Selection.Revision, spec.Target.Selection.Revision, true
	case contractsv1alpha1.ReviewModeScope:
		if spec.Target.Scope == nil {
			return "", "", false
		}
		return spec.Target.Scope.Revision, spec.Target.Scope.Revision, true
	default:
		return "", "", false
	}
}

func matchFindings(
	baseline []contractsv1alpha1.FindingLineageFinding,
	variant []contractsv1alpha1.FindingLineageFinding,
	mappings []contractsv1alpha1.FindingLineagePathMapping,
) []contractsv1alpha1.FindingLineageRelation {
	baselineByFingerprint := make(map[string]contractsv1alpha1.FindingLineageFinding, len(baseline))
	variantByFingerprint := make(map[string]contractsv1alpha1.FindingLineageFinding, len(variant))
	for _, finding := range baseline {
		baselineByFingerprint[finding.Fingerprint] = finding
	}
	for _, finding := range variant {
		variantByFingerprint[finding.Fingerprint] = finding
	}
	usedBaseline := make(map[string]bool)
	usedVariant := make(map[string]bool)
	relations := make([]contractsv1alpha1.FindingLineageRelation, 0)
	for fingerprint, before := range baselineByFingerprint {
		if after, exists := variantByFingerprint[fingerprint]; exists {
			relations = append(relations, relation(
				contractsv1alpha1.FindingLineageContinued,
				contractsv1alpha1.FindingLineageExactFingerprint,
				"exact_canonical_fingerprint",
				contractsv1alpha1.ExactFindingLineageKey(fingerprint),
				[]string{before.FindingID}, []string{after.FindingID},
			))
			usedBaseline[before.FindingID], usedVariant[after.FindingID] = true, true
		}
	}
	stableBefore, stableAfter := remainingFamilies(baseline, usedBaseline), remainingFamilies(variant, usedVariant)
	relations = append(relations, matchCommonFamilies(stableBefore, stableAfter, usedBaseline, usedVariant, contractsv1alpha1.FindingLineageStableFamily, "dimension_category_path_title_family")...)

	renames := map[string]string{}
	for _, mapping := range mappings {
		renames[mapping.BaselinePath] = mapping.VariantPath
	}
	renameBefore := map[string][]contractsv1alpha1.FindingLineageFinding{}
	for _, finding := range baseline {
		if !usedBaseline[finding.FindingID] {
			if mapped, ok := renames[finding.Anchor.Path]; ok {
				family := contractsv1alpha1.FindingLineageFamilyKeyForPath(finding, mapped)
				renameBefore[family] = append(renameBefore[family], finding)
			}
		}
	}
	renameAfter := remainingFamilies(variant, usedVariant)
	relations = append(relations, matchCommonFamilies(renameBefore, renameAfter, usedBaseline, usedVariant, contractsv1alpha1.FindingLineageGitRenameFamily, "git_rename_dimension_category_title_family")...)

	for _, finding := range baseline {
		if !usedBaseline[finding.FindingID] {
			relations = append(relations, relation(contractsv1alpha1.FindingLineageResolved, contractsv1alpha1.FindingLineageUnmatched, "no_conservative_match", finding.FamilyKey, []string{finding.FindingID}, []string{}))
		}
	}
	for _, finding := range variant {
		if !usedVariant[finding.FindingID] {
			relations = append(relations, relation(contractsv1alpha1.FindingLineageIntroduced, contractsv1alpha1.FindingLineageUnmatched, "no_conservative_match", finding.FamilyKey, []string{}, []string{finding.FindingID}))
		}
	}
	return relations
}

func matchCommonFamilies(beforeFamilies, afterFamilies map[string][]contractsv1alpha1.FindingLineageFinding, usedBefore, usedAfter map[string]bool, method contractsv1alpha1.FindingLineageMatchMethod, reason string) []contractsv1alpha1.FindingLineageRelation {
	relations := []contractsv1alpha1.FindingLineageRelation{}
	for family, before := range beforeFamilies {
		after := afterFamilies[family]
		if len(after) == 0 {
			continue
		}
		switch {
		case len(before) == 1 && len(after) == 1:
			relations = append(relations, relation(contractsv1alpha1.FindingLineageContinued, method, reason, family, findingIDs(before), findingIDs(after)))
		case len(before) == 1 && len(after) > 1:
			relations = append(relations, relation(contractsv1alpha1.FindingLineageSplit, method, reason, family, findingIDs(before), findingIDs(after)))
		case len(before) > 1 && len(after) == 1:
			relations = append(relations, relation(contractsv1alpha1.FindingLineageMerged, method, reason, family, findingIDs(before), findingIDs(after)))
		default:
			for _, finding := range before {
				relations = append(relations, relation(contractsv1alpha1.FindingLineageResolved, contractsv1alpha1.FindingLineageUnmatched, "ambiguous_many_to_many_family", family, []string{finding.FindingID}, []string{}))
			}
			for _, finding := range after {
				relations = append(relations, relation(contractsv1alpha1.FindingLineageIntroduced, contractsv1alpha1.FindingLineageUnmatched, "ambiguous_many_to_many_family", family, []string{}, []string{finding.FindingID}))
			}
		}
		for _, finding := range before {
			usedBefore[finding.FindingID] = true
		}
		for _, finding := range after {
			usedAfter[finding.FindingID] = true
		}
	}
	return relations
}

func remainingFamilies(findings []contractsv1alpha1.FindingLineageFinding, used map[string]bool) map[string][]contractsv1alpha1.FindingLineageFinding {
	result := make(map[string][]contractsv1alpha1.FindingLineageFinding)
	for _, finding := range findings {
		if !used[finding.FindingID] {
			result[finding.FamilyKey] = append(result[finding.FamilyKey], finding)
		}
	}
	return result
}

func findingIDs(findings []contractsv1alpha1.FindingLineageFinding) []string {
	ids := make([]string, len(findings))
	for index := range findings {
		ids[index] = findings[index].FindingID
	}
	sort.Strings(ids)
	return ids
}

func relation(kind contractsv1alpha1.FindingLineageRelationType, method contractsv1alpha1.FindingLineageMatchMethod, reason, family string, before, after []string) contractsv1alpha1.FindingLineageRelation {
	relation := contractsv1alpha1.FindingLineageRelation{
		Type: kind, Method: method, ReasonCode: reason, FamilyKey: family,
		BaselineFindingIDs: before, VariantFindingIDs: after,
	}
	relation.RelationID = contractsv1alpha1.FindingLineageRelationID(relation)
	return relation
}
