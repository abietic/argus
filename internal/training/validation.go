package training

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

func (request MaterializationRequest) Validate() error {
	if request.SchemaVersion != MaterializationRequestSchemaVersion {
		return fmt.Errorf("unsupported training materialization request schema %q", request.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{{"dataset_id", request.DatasetID}, {"dataset_revision", request.DatasetRevision}, {"repository_id", request.RepositoryID}} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := request.ConfigBundleRef.Validate(); err != nil {
		return fmt.Errorf("config_bundle_ref: %w", err)
	}
	if request.ConfigBundleRef.Contract != runmodel.ContractConfigBundle {
		return fmt.Errorf("config_bundle_ref contract must be %q", runmodel.ContractConfigBundle)
	}
	if request.Cases == nil || len(request.Cases) == 0 {
		return fmt.Errorf("cases must be an explicit non-empty array")
	}
	for index, binding := range request.Cases {
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("cases[%d]: %w", index, err)
		}
		if index > 0 && request.Cases[index-1].CaseID >= binding.CaseID {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func (binding CaseBinding) Validate() error {
	if err := validateID("case_id", binding.CaseID); err != nil {
		return err
	}
	if binding.ExpectedGovernanceRevision == 0 || binding.ExpectedLabelRevision == 0 {
		return fmt.Errorf("expected governance and label revisions must be positive")
	}
	if binding.ArtifactRefs == nil || len(binding.ArtifactRefs) == 0 {
		return fmt.Errorf("artifact_refs must be an explicit non-empty array")
	}
	for index, ref := range binding.ArtifactRefs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("artifact_refs[%d]: %w", index, err)
		}
		if index > 0 && binding.ArtifactRefs[index-1].URI >= ref.URI {
			return fmt.Errorf("artifact_refs must be uniquely sorted by URI")
		}
	}
	return binding.Authority.Validate()
}

func (authority LabelAuthority) Validate() error {
	switch authority.Kind {
	case AuthorityHumanAdjudication, AuthorityExternalGovernance:
	default:
		return fmt.Errorf("unsupported label authority kind %q", authority.Kind)
	}
	if authority.ReviewerIDs == nil || len(authority.ReviewerIDs) == 0 {
		return fmt.Errorf("reviewer_ids must be an explicit non-empty array")
	}
	for index, reviewer := range authority.ReviewerIDs {
		if err := validateID("reviewer_id", reviewer); err != nil {
			return err
		}
		if index > 0 && authority.ReviewerIDs[index-1] >= reviewer {
			return fmt.Errorf("reviewer_ids must be uniquely sorted")
		}
	}
	if err := validateID("adjudicator_id", authority.AdjudicatorID); err != nil {
		return err
	}
	if slices.Contains(authority.ReviewerIDs, authority.AdjudicatorID) {
		return fmt.Errorf("adjudicator must be independent from reviewers")
	}
	if authority.EvidenceRefs == nil {
		return fmt.Errorf("evidence_refs must be an explicit array")
	}
	for index, ref := range authority.EvidenceRefs {
		if strings.TrimSpace(ref) == "" || len(ref) > 4096 {
			return fmt.Errorf("evidence_refs[%d] is invalid", index)
		}
		if index > 0 && authority.EvidenceRefs[index-1] >= ref {
			return fmt.Errorf("evidence_refs must be uniquely sorted")
		}
	}
	if authority.Statement != IndependentAuthorityStatement {
		return fmt.Errorf("label authority statement must be %q", IndependentAuthorityStatement)
	}
	return nil
}

func sealManifest(manifest DatasetManifest) (DatasetManifest, error) {
	manifest.SchemaVersion = DatasetManifestSchemaVersion
	manifest.ManifestID, manifest.SHA256 = "", ""
	digest, err := runmodel.DigestJSON(manifest)
	if err != nil {
		return DatasetManifest{}, err
	}
	manifest.SHA256 = digest
	manifest.ManifestID = "training-manifest-" + digest[:24]
	if err := manifest.Validate(); err != nil {
		return DatasetManifest{}, err
	}
	return manifest, nil
}

func (manifest DatasetManifest) Validate() error {
	if manifest.SchemaVersion != DatasetManifestSchemaVersion {
		return fmt.Errorf("unsupported training manifest schema %q", manifest.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{{"dataset_id", manifest.DatasetID}, {"dataset_revision", manifest.DatasetRevision}, {"repository_id", manifest.RepositoryID}, {"created_by", manifest.CreatedBy}} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := manifest.ConfigContext.Validate(); err != nil {
		return fmt.Errorf("config_context: %w", err)
	}
	if manifest.ConfigContext.RepositoryID != manifest.RepositoryID {
		return fmt.Errorf("config context repository does not match manifest repository")
	}
	if err := manifest.ConfigBundleRef.Validate(); err != nil {
		return err
	}
	if manifest.ConfigBundleRef.Contract != runmodel.ContractConfigBundle {
		return fmt.Errorf("invalid config bundle contract")
	}
	if manifest.DataPolicy.Training != reviewconfig.PermissionAllow || manifest.DataPolicy.Export != reviewconfig.PermissionAllow || manifest.DataPolicy.Redaction != reviewconfig.RedactionStrict {
		return fmt.Errorf("manifest data policy is not training/export allowed with strict redaction")
	}
	if manifest.DataPolicy.RetentionDays <= 0 {
		return fmt.Errorf("manifest data retention must be positive")
	}
	if manifest.ContentMode != ContentModeReferenceOnly || manifest.ContainsSourceBytes || manifest.SelfLabelsAllowed {
		return fmt.Errorf("manifest content safety declarations are invalid")
	}
	if manifest.Samples == nil || len(manifest.Samples) == 0 {
		return fmt.Errorf("samples must be an explicit non-empty array")
	}
	for index, sample := range manifest.Samples {
		if err := sample.Validate(manifest.RepositoryID); err != nil {
			return fmt.Errorf("samples[%d]: %w", index, err)
		}
		if index > 0 && manifest.Samples[index-1].CaseID >= sample.CaseID {
			return fmt.Errorf("samples must be uniquely sorted by case_id")
		}
	}
	if manifest.CreatedAt.IsZero() || manifest.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	copy := manifest
	copy.ManifestID, copy.SHA256 = "", ""
	digest, err := runmodel.DigestJSON(copy)
	if err != nil {
		return err
	}
	if manifest.SHA256 != digest || manifest.ManifestID != "training-manifest-"+digest[:24] {
		return fmt.Errorf("training manifest identity does not match content")
	}
	return nil
}

func (sample ManifestSample) Validate(repositoryID string) error {
	if err := validateID("case_id", sample.CaseID); err != nil {
		return err
	}
	if sample.CaseGovernanceRevision == 0 || sample.CaseLabelRevision == 0 {
		return fmt.Errorf("case revisions must be positive")
	}
	current := evaluation.EvaluationCase{
		SchemaVersion: evaluation.EvaluationCaseSchemaVersion, CaseID: sample.CaseID,
		Type: sample.CaseType, Provenance: sample.Provenance, LicenseConsent: sample.LicenseConsent,
		Classification: sample.Classification, Owner: sample.Owner, InputSnapshotRef: sample.InputSnapshotRef,
		Label: sample.Label, LabelPolicyRevision: sample.LabelPolicyRevision, ReviewState: sample.ReviewState,
		DatasetState: sample.DatasetState, Split: sample.DatasetSplit, CloneGroupID: sample.CloneGroupID,
		Eligibility: sample.Eligibility, CreatedAt: sample.CaseCreatedAt,
	}
	if err := current.Validate(); err != nil {
		return fmt.Errorf("governed case snapshot: %w", err)
	}
	if sample.Provenance.RepositoryID != repositoryID {
		return fmt.Errorf("sample repository binding mismatch")
	}
	if sample.ReviewState != evaluation.ReviewApproved || sample.DatasetState != evaluation.DatasetActive || sample.DatasetSplit != evaluation.SplitTrain || !sample.Eligibility.Training {
		return fmt.Errorf("sample governance is not active approved training")
	}
	if !slices.Contains(sample.LicenseConsent.AllowedUses, evaluation.UseTraining) {
		return fmt.Errorf("sample license does not allow training")
	}
	if err := sample.Authority.Validate(); err != nil {
		return err
	}
	if sample.ArtifactRefs == nil || len(sample.ArtifactRefs) == 0 {
		return fmt.Errorf("artifact_refs must be non-empty")
	}
	for index, ref := range sample.ArtifactRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
		if index > 0 && sample.ArtifactRefs[index-1].URI >= ref.URI {
			return fmt.Errorf("artifact_refs must be uniquely sorted by URI")
		}
	}
	provided := make([]string, len(sample.ArtifactRefs))
	for index, ref := range sample.ArtifactRefs {
		provided[index] = ref.URI
	}
	if !slices.Equal(requiredURIs(current, sample.Authority), provided) {
		return fmt.Errorf("artifact refs do not exactly cover governed input and evidence URIs")
	}
	return nil
}

func DecodeDatasetManifest(data []byte) (DatasetManifest, error) {
	var manifest DatasetManifest
	if err := decodeStrict(data, &manifest); err != nil {
		return DatasetManifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return DatasetManifest{}, err
	}
	return manifest, nil
}

func DecodeMaterializationRequest(data []byte) (MaterializationRequest, error) {
	var request MaterializationRequest
	if err := decodeStrict(data, &request); err != nil {
		return MaterializationRequest{}, fmt.Errorf("decode training materialization request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return MaterializationRequest{}, fmt.Errorf("validate training materialization request: %w", err)
	}
	return request, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateID(name, value string) error {
	if !idPattern.MatchString(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func requiredURIs(current evaluation.EvaluationCase, authority LabelAuthority) []string {
	refs := []string{current.InputSnapshotRef}
	refs = append(refs, current.Provenance.EvidenceRefs...)
	refs = append(refs, current.Label.AnchorRefs...)
	refs = append(refs, authority.EvidenceRefs...)
	sort.Strings(refs)
	return slices.Compact(refs)
}
