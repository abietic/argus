package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFindingLineageSealDecodeAndRejectsBrokenClosure(t *testing.T) {
	policy, err := SealFindingLineagePolicy(FindingLineagePolicy{
		PolicyID: "argus-conservative-lineage-matcher", Revision: "1",
		FamilyFields:     []string{"category", "dimension", "path", "title_normalized"},
		ManyToManyAction: "leave_unmatched", AncestryAuthority: "caller_order_unverified",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := testLineageFinding("run-before", "finding-before", strings.Repeat("1", 64), strings.Repeat("a", 64))
	after := testLineageFinding("run-after", "finding-after", strings.Repeat("1", 64), strings.Repeat("b", 64))
	relation := FindingLineageRelation{
		Type: FindingLineageContinued, Method: FindingLineageExactFingerprint,
		ReasonCode: "exact_canonical_fingerprint", FamilyKey: ExactFindingLineageKey(before.Fingerprint),
		BaselineFindingIDs: []string{before.FindingID}, VariantFindingIDs: []string{after.FindingID},
	}
	relation.RelationID = FindingLineageRelationID(relation)
	lineage := FindingLineage{
		Policy:           policy,
		Baseline:         testLineageBinding("run-before", strings.Repeat("c", 64), "base-1", "head-1"),
		Variant:          testLineageBinding("run-after", strings.Repeat("d", 64), "base-2", "head-2"),
		PathMappings:     []FindingLineagePathMapping{},
		BaselineFindings: []FindingLineageFinding{before}, VariantFindings: []FindingLineageFinding{after},
		Relations: []FindingLineageRelation{relation}, GeneratedAt: time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC),
	}
	CanonicalizeFindingLineage(&lineage)
	sealed, err := SealFindingLineage(lineage)
	if err != nil {
		t.Fatalf("SealFindingLineage() error = %v", err)
	}
	data, _ := json.Marshal(sealed)
	decoded, err := DecodeFindingLineage(data)
	if err != nil || decoded.LineageID != sealed.LineageID {
		t.Fatalf("DecodeFindingLineage() = %+v, %v", decoded, err)
	}

	tampered := sealed
	tampered.Relations[0].VariantFindingIDs = []string{}
	if err := tampered.Validate(); err == nil {
		t.Fatal("Validate() accepted broken relation cardinality")
	}
	tampered = sealed
	tampered.BaselineFindings[0].FamilyKey = strings.Repeat("f", 64)
	if err := tampered.Validate(); err == nil {
		t.Fatal("Validate() accepted an invented stable family key")
	}

	unknown := append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeFindingLineage(unknown); err == nil {
		t.Fatal("DecodeFindingLineage() accepted unknown field")
	}
}

func TestFindingLineageGitRenameEvidenceClosesAndRejectsTampering(t *testing.T) {
	policy, err := SealFindingLineagePolicy(FindingLineagePolicy{
		PolicyID: "argus-git-aware-conservative-lineage-matcher", Revision: "1",
		FamilyFields:     []string{"category", "dimension", "path", "title_normalized"},
		ManyToManyAction: "leave_unmatched", AncestryAuthority: "local_git_object_graph",
		RenameThresholdBPS: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineOID, variantOID := strings.Repeat("1", 40), strings.Repeat("2", 40)
	before := testLineageFinding("run-before", "finding-before", strings.Repeat("3", 64), "")
	after := testLineageFinding("run-after", "finding-after", strings.Repeat("4", 64), "")
	before.Anchor.Path, after.Anchor.Path = "internal/old.go", "internal/new.go"
	before.FamilyKey = FindingLineageFamilyKeyForPath(before, before.Anchor.Path)
	after.FamilyKey = FindingLineageFamilyKeyForPath(after, after.Anchor.Path)
	mappings := []FindingLineagePathMapping{{BaselinePath: before.Anchor.Path, VariantPath: after.Anchor.Path, SimilarityBPS: 10000}}
	ancestry, err := SealFindingLineageGitAncestry(FindingLineageGitAncestry{
		Authority: "local_git_object_graph", RepositoryIdentitySHA256: strings.Repeat("5", 64), ObjectFormat: "sha1",
		BaselineHeadOID: baselineOID, VariantHeadOID: variantOID, MergeBaseOID: baselineOID, BaselineIsAncestor: true,
	}, mappings)
	if err != nil {
		t.Fatal(err)
	}
	relation := FindingLineageRelation{
		Type: FindingLineageContinued, Method: FindingLineageGitRenameFamily,
		ReasonCode: "git_rename_dimension_category_title_family", FamilyKey: after.FamilyKey,
		BaselineFindingIDs: []string{before.FindingID}, VariantFindingIDs: []string{after.FindingID},
	}
	lineage := FindingLineage{
		Policy:   policy,
		Baseline: testLineageBinding("run-before", strings.Repeat("6", 64), strings.Repeat("0", 40), baselineOID),
		Variant:  testLineageBinding("run-after", strings.Repeat("7", 64), baselineOID, variantOID),
		Ancestry: &ancestry, PathMappings: mappings,
		BaselineFindings: []FindingLineageFinding{before}, VariantFindings: []FindingLineageFinding{after},
		Relations: []FindingLineageRelation{relation}, GeneratedAt: time.Date(2026, 8, 26, 8, 30, 0, 0, time.UTC),
	}
	CanonicalizeFindingLineage(&lineage)
	sealed, err := SealFindingLineage(lineage)
	if err != nil {
		t.Fatalf("SealFindingLineage() error = %v", err)
	}
	if sealed.Relations[0].Method != FindingLineageGitRenameFamily {
		t.Fatalf("sealed relation = %+v", sealed.Relations[0])
	}

	tampered := sealed
	tampered.PathMappings = append([]FindingLineagePathMapping(nil), sealed.PathMappings...)
	tampered.PathMappings[0].SimilarityBPS = 9000
	if err := tampered.Validate(); err == nil {
		t.Fatal("Validate() accepted rename evidence not bound by ancestry digest")
	}
	tampered = sealed
	tampered.Ancestry = new(FindingLineageGitAncestry)
	*tampered.Ancestry = *sealed.Ancestry
	tampered.Ancestry.BaselineIsAncestor = false
	if err := tampered.Validate(); err == nil {
		t.Fatal("Validate() accepted unproven Git ancestry")
	}
}

func testLineageFinding(runID, findingID, fingerprint, family string) FindingLineageFinding {
	finding := FindingLineageFinding{
		RunID: runID, FindingID: findingID, Fingerprint: fingerprint, FamilyKey: family,
		CandidateID: "candidate-" + findingID, Dimension: VersionedRef{ID: "correctness", Revision: "1", SHA256: testDigest},
		Category: "correctness", Severity: HypothesisSeverityHigh, Title: "nil dereference",
		Anchor: HypothesisSourceAnchor{Path: "internal/review.go", Side: HypothesisAnchorNew, StartLine: 10, EndLine: 10, SourceDigest: testDigest},
	}
	finding.FamilyKey = findingLineageFamilyKey(finding.Category, finding.Dimension, finding.Anchor.Path, finding.Title)
	return finding
}

func testLineageBinding(runID, targetDigest, base, head string) FindingLineageRunBinding {
	return FindingLineageRunBinding{
		RunID: runID, RunSHA256: testDigest, TenantID: "tenant-1", WorkspaceID: "workspace-1",
		Repository: RepositoryRef{Provider: "local-git", RepositoryID: "repository-1"},
		TargetMode: ReviewModeDiff, BaseRevision: base, HeadRevision: head, TargetDigest: targetDigest,
		ReviewSpecSHA256: testDigest, TargetSnapshotSHA256: testDigest, CandidateSetSHA256: testDigest,
		VerificationLedgerSHA256: testDigest, CalibrationLedgerSHA256: testDigest,
		SuppressionLedgerSHA256: testDigest, GovernedReportSHA256: testDigest,
	}
}
