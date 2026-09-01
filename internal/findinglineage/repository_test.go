package findinglineage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const lineageTestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestMatchFindingsProducesExactSplitMergedAndFailsClosedOnManyToMany(t *testing.T) {
	makeFinding := func(id, fingerprint, family string) contractsv1alpha1.FindingLineageFinding {
		return contractsv1alpha1.FindingLineageFinding{FindingID: id, Fingerprint: fingerprint, FamilyKey: family}
	}
	baseline := []contractsv1alpha1.FindingLineageFinding{
		makeFinding("b-exact", lineageTestDigest, strings64("1")),
		makeFinding("b-split", strings64("2"), strings64("2")),
		makeFinding("b-merge-1", strings64("3"), strings64("3")),
		makeFinding("b-merge-2", strings64("4"), strings64("3")),
		makeFinding("b-amb-1", strings64("5"), strings64("4")),
		makeFinding("b-amb-2", strings64("6"), strings64("4")),
	}
	variant := []contractsv1alpha1.FindingLineageFinding{
		makeFinding("v-exact", lineageTestDigest, strings64("9")),
		makeFinding("v-split-1", strings64("7"), strings64("2")),
		makeFinding("v-split-2", strings64("8"), strings64("2")),
		makeFinding("v-merge", strings64("9"), strings64("3")),
		makeFinding("v-amb-1", strings64("b"), strings64("4")),
		makeFinding("v-amb-2", strings64("c"), strings64("4")),
	}
	relations := matchFindings(baseline, variant, []contractsv1alpha1.FindingLineagePathMapping{})
	counts := map[contractsv1alpha1.FindingLineageRelationType]int{}
	ambiguous := 0
	for _, relation := range relations {
		counts[relation.Type]++
		if relation.ReasonCode == "ambiguous_many_to_many_family" {
			ambiguous++
		}
	}
	if counts[contractsv1alpha1.FindingLineageContinued] != 1 || counts[contractsv1alpha1.FindingLineageSplit] != 1 || counts[contractsv1alpha1.FindingLineageMerged] != 1 || counts[contractsv1alpha1.FindingLineageResolved] != 2 || counts[contractsv1alpha1.FindingLineageIntroduced] != 2 || ambiguous != 4 {
		t.Fatalf("relation counts = %+v ambiguous=%d", counts, ambiguous)
	}
}

func TestMatchFindingsUsesOnlyExplicitGitRenameMappings(t *testing.T) {
	dimension := contractsv1alpha1.VersionedRef{ID: "correctness", Revision: "1", SHA256: lineageTestDigest}
	before := contractsv1alpha1.FindingLineageFinding{
		FindingID: "before", Fingerprint: strings64("1"), Category: "correctness", Dimension: dimension,
		Title: "nil dereference", Anchor: contractsv1alpha1.HypothesisSourceAnchor{Path: "old/review.go"},
	}
	after := before
	after.FindingID, after.Fingerprint, after.Anchor.Path = "after", strings64("2"), "new/review.go"
	before.FamilyKey = contractsv1alpha1.FindingLineageFamilyKeyForPath(before, before.Anchor.Path)
	after.FamilyKey = contractsv1alpha1.FindingLineageFamilyKeyForPath(after, after.Anchor.Path)

	unmapped := matchFindings(
		[]contractsv1alpha1.FindingLineageFinding{before},
		[]contractsv1alpha1.FindingLineageFinding{after},
		[]contractsv1alpha1.FindingLineagePathMapping{},
	)
	if len(unmapped) != 2 {
		t.Fatalf("unmapped relations = %+v", unmapped)
	}
	mapped := matchFindings(
		[]contractsv1alpha1.FindingLineageFinding{before},
		[]contractsv1alpha1.FindingLineageFinding{after},
		[]contractsv1alpha1.FindingLineagePathMapping{{BaselinePath: "old/review.go", VariantPath: "new/review.go", SimilarityBPS: 10000}},
	)
	if len(mapped) != 1 || mapped[0].Type != contractsv1alpha1.FindingLineageContinued ||
		mapped[0].Method != contractsv1alpha1.FindingLineageGitRenameFamily {
		t.Fatalf("mapped relations = %+v", mapped)
	}
}

func TestRepositoryBuildIsDurableExactIdempotent(t *testing.T) {
	store, err := local.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	source := newFakeRunSource(t)
	source.addRun(t, "run-before", "base-1", "head-1", strings64("1"), "finding-before", lineageTestDigest)
	source.addRun(t, "run-after", "base-2", "head-2", strings64("2"), "finding-after", lineageTestDigest)
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	repository, err := New(store, source, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := BuildRequest{SchemaVersion: BuildRequestSchemaVersion, BaselineRunID: "run-before", VariantRunID: "run-after", Policy: DefaultPolicy()}
	first, err := repository.Build(t.Context(), request)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	second, err := repository.Build(t.Context(), request)
	if err != nil {
		t.Fatalf("retry Build() error = %v", err)
	}
	if first.Lineage.LineageID != second.Lineage.LineageID || first.LineageRef != second.LineageRef || first.Lineage.Summary.Continued != 1 {
		t.Fatalf("idempotent results differ: %+v %+v", first, second)
	}
	reopened, err := New(store, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Get(first.Lineage.LineageID)
	if err != nil || loaded.LineageRef != first.LineageRef {
		t.Fatalf("Get() = %+v, %v", loaded, err)
	}
	listed, err := reopened.List(ListFilter{RunID: "run-after"})
	if err != nil || len(listed) != 1 {
		t.Fatalf("List() = %+v, %v", listed, err)
	}
	conflict := request
	conflict.IdempotencyKey = "different-key"
	if _, err := reopened.Build(t.Context(), conflict); err == nil {
		t.Fatal("Build() accepted duplicate pair with a different idempotency key")
	}
}

type fakeRunSource struct {
	results   map[string]runrepo.CommittedRunResult
	snapshots map[string]runmodel.ExecutionSnapshot
	committed map[string]runmodel.ArtifactRef
	artifacts map[string][]byte
}

func newFakeRunSource(t *testing.T) *fakeRunSource {
	t.Helper()
	return &fakeRunSource{results: map[string]runrepo.CommittedRunResult{}, snapshots: map[string]runmodel.ExecutionSnapshot{}, committed: map[string]runmodel.ArtifactRef{}, artifacts: map[string][]byte{}}
}

func (source *fakeRunSource) addRun(t *testing.T, runID, base, head, targetDigest, findingID, fingerprint string) {
	t.Helper()
	spec := contractsv1alpha1.ReviewSpec{
		SchemaVersion: contractsv1alpha1.ReviewSpecSchemaVersion, RequestID: runID, IdempotencyKey: runID,
		TenantID: "tenant-1", WorkspaceID: "workspace-1", Repository: contractsv1alpha1.RepositoryRef{Provider: "local-git", RepositoryID: "repository-1"},
		Target:          contractsv1alpha1.ReviewTarget{Mode: contractsv1alpha1.ReviewModeDiff, Diff: &contractsv1alpha1.DiffTarget{BaseRevision: base, HeadRevision: head, Patch: contractsv1alpha1.ContentRef{URI: "artifact://argus/patch-" + runID, SHA256: lineageTestDigest, SizeBytes: 1}}},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{ID: "config", Revision: "1", SHA256: lineageTestDigest}, WorkflowRef: contractsv1alpha1.VersionedRef{ID: "workflow", Revision: "1", SHA256: lineageTestDigest},
		RequestedOutput: []string{"report"}, RemoteWrites: "deny",
	}
	specData, _ := json.Marshal(spec)
	specDigest, _ := contractsv1alpha1.DigestReviewSpec(spec)
	specRef := runmodel.ArtifactRef{Contract: runmodel.ContractReviewSpec, URI: "artifact://local/" + specDigest, SHA256: specDigest, SizeBytes: int64(len(specData))}
	source.artifacts[specRef.SHA256] = specData
	ref := func(contract, seed string) *runmodel.ArtifactRef {
		digest := sha256.Sum256([]byte(seed))
		value := runmodel.ArtifactRef{Contract: contract, URI: "artifact://local/" + hex.EncodeToString(digest[:]), SHA256: hex.EncodeToString(digest[:]), SizeBytes: 1}
		return &value
	}
	run := runmodel.ReviewRun{RunID: runID, Status: runmodel.RunStatusSucceeded, TargetMode: runmodel.TargetModeDiff, BaseRevision: base, HeadRevision: head, ExecutionSnapshotID: "snapshot-" + runID, TargetSnapshotRef: *ref("argus.target_snapshot.v1alpha1+json", "target-"+runID), CandidateSetRef: ref(runmodel.ContractGovernedCandidateSet, "candidate-"+runID), VerificationLedgerRef: ref(runmodel.ContractCandidateVerificationLedger, "verification-"+runID), CalibrationLedgerRef: ref(runmodel.ContractFindingCalibrationLedger, "calibration-"+runID), SuppressionLedgerRef: ref(runmodel.ContractFindingSuppressionLedger, "suppression-"+runID), GovernedReportRef: ref(runmodel.ContractGovernedReviewReport, "report-"+runID)}
	report := &contractsv1alpha1.GovernedReviewReport{ReviewRunID: runID, TargetDigest: targetDigest, Findings: []contractsv1alpha1.GovernedReviewFinding{{FindingID: findingID, Fingerprint: fingerprint, CandidateID: "candidate-" + findingID, Dimension: contractsv1alpha1.VersionedRef{ID: "correctness", Revision: "1", SHA256: lineageTestDigest}, Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh, Title: "nil dereference", Anchor: contractsv1alpha1.HypothesisSourceAnchor{Path: "internal/review.go", Side: contractsv1alpha1.HypothesisAnchorNew, StartLine: 10, EndLine: 10, SourceDigest: lineageTestDigest}}}}
	source.results[runID] = runrepo.CommittedRunResult{Run: run, GovernedReport: report}
	source.snapshots[run.ExecutionSnapshotID] = runmodel.ExecutionSnapshot{ReviewSpecRef: specRef}
	source.committed[runID] = *ref(runmodel.RunSchemaVersion, "committed-"+runID)
}

func (source *fakeRunSource) LoadCommittedRunResult(id string) (runrepo.CommittedRunResult, error) {
	value, ok := source.results[id]
	if !ok {
		return runrepo.CommittedRunResult{}, fmt.Errorf("missing run")
	}
	return value, nil
}
func (source *fakeRunSource) LoadExecutionSnapshot(id string) (runmodel.ExecutionSnapshot, error) {
	value, ok := source.snapshots[id]
	if !ok {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf("missing snapshot")
	}
	return value, nil
}
func (source *fakeRunSource) CommittedRunRef(id string) (runmodel.ArtifactRef, error) {
	value, ok := source.committed[id]
	if !ok {
		return runmodel.ArtifactRef{}, fmt.Errorf("missing committed ref")
	}
	return value, nil
}
func (source *fakeRunSource) ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	value, ok := source.artifacts[ref.SHA256]
	if !ok {
		return nil, fmt.Errorf("missing artifact %s", ref.SHA256)
	}
	return value, nil
}
func (source *fakeRunSource) PutArtifact(contract string, data []byte) (runmodel.ArtifactRef, error) {
	digest := sha256.Sum256(data)
	sha := hex.EncodeToString(digest[:])
	source.artifacts[sha] = append([]byte(nil), data...)
	return runmodel.ArtifactRef{Contract: contract, URI: "artifact://local/sha256/" + sha, SHA256: sha, SizeBytes: int64(len(data))}, nil
}

func strings64(character string) string {
	result := ""
	for range 64 {
		result += character
	}
	return result
}
