package training

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

type caseFixture struct {
	record        evaluation.CaseRecord
	annotations   []evaluation.CaseAnnotationEntry
	adjudications []evaluation.CaseAdjudicationEntry
}

func (source *caseFixture) GetCase(caseID string, _ evaluation.Access) (evaluation.CaseRecord, error) {
	if source.record.Case.CaseID != caseID {
		return evaluation.CaseRecord{}, evaluation.ErrNotFound
	}
	return source.record, nil
}
func (source *caseFixture) AnnotationHistory(string, evaluation.Access) ([]evaluation.CaseAnnotationEntry, error) {
	return slices.Clone(source.annotations), nil
}
func (source *caseFixture) AdjudicationHistory(string, evaluation.Access) ([]evaluation.CaseAdjudicationEntry, error) {
	return slices.Clone(source.adjudications), nil
}

type trainingFixture struct {
	store      *local.Store
	artifacts  *runrepo.Repository
	cases      *caseFixture
	repository *Repository
	request    MaterializationRequest
	mutation   evaluation.Mutation
	refs       []runmodel.ArtifactRef
}

func newTrainingFixture(t *testing.T) trainingFixture {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	put := func(contract, content string) runmodel.ArtifactRef {
		ref, putErr := artifacts.PutArtifact(contract, []byte(content))
		if putErr != nil {
			t.Fatal(putErr)
		}
		return ref
	}
	input := put("argus.test.input.v1", "frozen input")
	provenance := put("argus.test.provenance.v1", "source provenance")
	authorityEvidence := put("argus.test.adjudication.v1", "independent adjudication")
	refs := []runmodel.ArtifactRef{input, provenance, authorityEvidence}
	sort.Slice(refs, func(i, j int) bool { return refs[i].URI < refs[j].URI })
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)
	label := evaluation.Label{ExpectedOutcome: evaluation.OutcomeClean, Anchors: []evaluation.LabelAnchor{}, AnchorRefs: []string{}, SuppressionTargets: []evaluation.SuppressionTarget{}}
	license := evaluation.LicenseConsent{LicenseID: "internal-license", Consent: evaluation.ConsentAuthorizedInternal, AllowedUses: []evaluation.UseScope{evaluation.UseEvaluation, evaluation.UseTraining}, Restrictions: []string{}}
	caseValue := evaluation.EvaluationCase{
		SchemaVersion: evaluation.EvaluationCaseSchemaVersion, CaseID: "case-training-1", Type: evaluation.CaseNegativeClean,
		Provenance:     evaluation.SourceProvenance{Kind: evaluation.SourceSynthetic, RepositoryID: "repository-example", SourceID: "synthetic-1", ObservedAt: now.Add(-2 * time.Hour), CollectedAt: now.Add(-time.Hour), EvidenceRefs: []string{provenance.URI}},
		LicenseConsent: license, Classification: evaluation.ClassificationInternal, Owner: "quality-team", InputSnapshotRef: input.URI,
		Label: label, LabelPolicyRevision: "label-policy-1", ReviewState: evaluation.ReviewApproved, DatasetState: evaluation.DatasetActive,
		Split: evaluation.SplitTrain, CloneGroupID: "clone-training-1", Eligibility: evaluation.Eligibility{Evaluation: true, Training: true}, CreatedAt: now.Add(-30 * time.Minute),
	}
	if err := caseValue.Validate(); err != nil {
		t.Fatalf("case fixture: %v", err)
	}
	cases := &caseFixture{record: evaluation.CaseRecord{
		Case:              caseValue,
		CurrentGovernance: evaluation.CaseGovernance{Revision: 1, ReviewState: caseValue.ReviewState, DatasetState: caseValue.DatasetState, Split: caseValue.Split, Eligibility: caseValue.Eligibility, LicenseConsent: license, UpdatedAt: now, UpdatedBy: "curator-previous"},
		CurrentLabel:      label, CurrentLabelPolicyRevision: caseValue.LabelPolicyRevision, CurrentLabelRevision: 1, UpdatedAt: now, UpdatedBy: "curator-previous",
		ExternalGovernance: &evaluation.ExternalGovernanceImportRecord{Attestation: evaluation.ExternalGovernanceAttestation{ReviewerIDs: []string{"reviewer-1"}, AdjudicatorID: "adjudicator-1", EvidenceRefs: []string{authorityEvidence.URI}}},
	}}
	bundleBytes, err := os.ReadFile(filepath.Join("..", "..", "examples", "config-bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleBytes)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Context.RepositoryID = "repository-example"
	bundle.Data.Training = reviewconfig.PermissionAllow
	bundle.Data.Export = reviewconfig.PermissionAllow
	bundle.Data.Redaction = reviewconfig.RedactionStrict
	digest, err := reviewconfig.DigestBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.SHA256 = digest
	bundle.BundleID = "bundle-" + digest[:24]
	if err := bundle.Validate(); err != nil {
		t.Fatalf("bundle fixture: %v", err)
	}
	bundleJSON, _ := json.Marshal(bundle)
	bundleRef, err := artifacts.PutArtifact(runmodel.ContractConfigBundle, bundleJSON)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store, cases, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	authority := LabelAuthority{Kind: AuthorityExternalGovernance, ReviewerIDs: []string{"reviewer-1"}, AdjudicatorID: "adjudicator-1", EvidenceRefs: []string{authorityEvidence.URI}, Statement: IndependentAuthorityStatement}
	mutation := evaluation.Mutation{IdempotencyKey: "training-materialize-1", Actor: "curator-1", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "materialize governed reference-only dataset", At: now}
	request := MaterializationRequest{SchemaVersion: MaterializationRequestSchemaVersion, DatasetID: "dataset-review-quality", DatasetRevision: "revision-1", RepositoryID: "repository-example", ConfigBundleRef: bundleRef, Cases: []CaseBinding{{CaseID: caseValue.CaseID, ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1, ArtifactRefs: refs, Authority: authority}}, CreatedAt: now}
	return trainingFixture{store: store, artifacts: artifacts, cases: cases, repository: repository, request: request, mutation: mutation, refs: refs}
}

func TestMaterializeReferenceOnlyManifestAndExactRetry(t *testing.T) {
	fixture := newTrainingFixture(t)
	record, err := fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	if record.Manifest.ContentMode != ContentModeReferenceOnly || record.Manifest.ContainsSourceBytes || record.Manifest.SelfLabelsAllowed {
		t.Fatalf("unsafe manifest declarations: %+v", record.Manifest)
	}
	if len(record.Manifest.Samples) != 1 || !slices.Equal(record.Manifest.Samples[0].ArtifactRefs, fixture.refs) {
		t.Fatalf("manifest did not freeze exact refs: %+v", record.Manifest.Samples)
	}
	data, err := fixture.artifacts.ReadArtifact(record.ManifestRef)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "frozen input") || strings.Contains(string(data), "source provenance") {
		t.Fatal("manifest copied source bytes")
	}
	retry, err := fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if retry.ManifestRef != record.ManifestRef {
		t.Fatalf("retry ref = %+v, want %+v", retry.ManifestRef, record.ManifestRef)
	}
	reopened, err := New(fixture.store, fixture.cases, fixture.artifacts)
	if err != nil {
		t.Fatalf("New(reopen) error = %v", err)
	}
	loaded, err := reopened.Get(record.Manifest.ManifestID, evaluation.Access{Actor: "curator-1", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest.SHA256 != record.Manifest.SHA256 {
		t.Fatalf("loaded SHA = %s, want %s", loaded.Manifest.SHA256, record.Manifest.SHA256)
	}
}

func TestMaterializeRejectsGovernancePolicyAndReferenceDrift(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*trainingFixture)
		want     error
		contains string
	}{
		{name: "revision drift", mutate: func(f *trainingFixture) { f.request.Cases[0].ExpectedLabelRevision = 2 }, want: ErrContaminated},
		{name: "missing exact ref", mutate: func(f *trainingFixture) { f.request.Cases[0].ArtifactRefs = f.request.Cases[0].ArtifactRefs[1:] }, contains: "exactly cover"},
		{name: "wrong authority", mutate: func(f *trainingFixture) { f.request.Cases[0].Authority.AdjudicatorID = "adjudicator-2" }, contains: "external governance"},
		{name: "operator is reviewer", mutate: func(f *trainingFixture) { f.mutation.Actor = "reviewer-1" }, want: ErrUnauthorized},
		{name: "inactive case", mutate: func(f *trainingFixture) { f.cases.record.CurrentGovernance.DatasetState = evaluation.DatasetRetired }, contains: "active, approved"},
		{name: "license denies training", mutate: func(f *trainingFixture) {
			f.cases.record.CurrentGovernance.LicenseConsent.AllowedUses = []evaluation.UseScope{evaluation.UseEvaluation}
		}, want: ErrPolicyDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTrainingFixture(t)
			test.mutate(&fixture)
			_, err := fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
			if err == nil {
				t.Fatal("Materialize() succeeded")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if test.contains != "" && !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want substring %q", err, test.contains)
			}
		})
	}
}

func TestMaterializeRejectsDeniedConfigAndQuarantinedArtifact(t *testing.T) {
	t.Run("config denied", func(t *testing.T) {
		fixture := newTrainingFixture(t)
		data, err := fixture.artifacts.ReadArtifact(fixture.request.ConfigBundleRef)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := reviewconfig.DecodeBundle(data)
		if err != nil {
			t.Fatal(err)
		}
		bundle.Data.Export = reviewconfig.PermissionDeny
		digest, _ := reviewconfig.DigestBundle(bundle)
		bundle.SHA256 = digest
		bundle.BundleID = "bundle-" + digest[:24]
		encoded, _ := json.Marshal(bundle)
		ref, err := fixture.artifacts.PutArtifact(runmodel.ContractConfigBundle, encoded)
		if err != nil {
			t.Fatal(err)
		}
		fixture.request.ConfigBundleRef = ref
		_, err = fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
		if !errors.Is(err, ErrPolicyDenied) {
			t.Fatalf("error = %v, want ErrPolicyDenied", err)
		}
	})
	t.Run("artifact quarantined", func(t *testing.T) {
		fixture := newTrainingFixture(t)
		_, err := fixture.artifacts.QuarantineArtifact(context.Background(), fixture.refs[0], "test quarantine", runrepo.ArtifactIntegrityMutation{IdempotencyKey: "quarantine-1", Actor: "integrity-admin", Audit: "test", At: fixture.mutation.At})
		if err != nil {
			t.Fatal(err)
		}
		_, err = fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
		if !errors.Is(err, runrepo.ErrArtifactQuarantined) {
			t.Fatalf("error = %v, want quarantine", err)
		}
	})
}

func TestMaterializeConflictsAndFailsClosedOnManifestQuarantine(t *testing.T) {
	fixture := newTrainingFixture(t)
	record, err := fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	conflict := fixture.request
	conflict.DatasetRevision = "revision-2"
	_, err = fixture.repository.Materialize(context.Background(), conflict, fixture.mutation)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	mutation := fixture.mutation
	mutation.IdempotencyKey = "training-materialize-2"
	request := fixture.request
	request.CreatedAt = request.CreatedAt.Add(time.Second)
	mutation.At = request.CreatedAt
	_, err = fixture.repository.Materialize(context.Background(), request, mutation)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("dataset conflict error = %v", err)
	}
	_, err = fixture.artifacts.QuarantineArtifact(context.Background(), record.ManifestRef, "manifest quarantine", runrepo.ArtifactIntegrityMutation{IdempotencyKey: "quarantine-manifest", Actor: "integrity-admin", Audit: "test", At: fixture.mutation.At.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New(fixture.store, fixture.cases, fixture.artifacts); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("New() error = %v, want ErrCorrupt", err)
	}
}
