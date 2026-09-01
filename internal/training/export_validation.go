package training

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/abietic/argus/internal/runmodel"
)

func (request ExportRequest) Validate() error {
	if request.SchemaVersion != ExportRequestSchemaVersion {
		return fmt.Errorf("unsupported training export request schema %q", request.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{{"export_id", request.ExportID}, {"manifest_id", request.ManifestID}} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := request.ManifestRef.Validate(); err != nil {
		return fmt.Errorf("manifest_ref: %w", err)
	}
	if request.ManifestRef.Contract != runmodel.ContractTrainingDatasetManifest {
		return fmt.Errorf("manifest_ref contract must be %q", runmodel.ContractTrainingDatasetManifest)
	}
	if err := request.Policy.Validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func sealExportBundle(bundle ExportBundle) (ExportBundle, error) {
	bundle.SchemaVersion = ExportBundleSchemaVersion
	bundle.SHA256 = ""
	digest, err := runmodel.DigestJSON(bundle)
	if err != nil {
		return ExportBundle{}, err
	}
	bundle.SHA256 = digest
	if err := bundle.Validate(); err != nil {
		return ExportBundle{}, err
	}
	return bundle, nil
}

func (bundle ExportBundle) Validate() error {
	if bundle.SchemaVersion != ExportBundleSchemaVersion {
		return fmt.Errorf("unsupported training export bundle schema %q", bundle.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{{"export_id", bundle.ExportID}, {"dataset_id", bundle.DatasetID}, {"dataset_revision", bundle.DatasetRevision}, {"repository_id", bundle.RepositoryID}, {"manifest_id", bundle.ManifestID}, {"created_by", bundle.CreatedBy}} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := bundle.ManifestRef.Validate(); err != nil {
		return err
	}
	if bundle.ManifestRef.Contract != runmodel.ContractTrainingDatasetManifest {
		return fmt.Errorf("invalid manifest ref contract")
	}
	if err := bundle.Policy.Validate(); err != nil {
		return err
	}
	if bundle.PortableFormat != PortableRecordsFormat || bundle.ContainsUnredactedSourceBytes {
		return fmt.Errorf("training export safety declarations are invalid")
	}
	if bundle.Receipts == nil || len(bundle.Receipts) == 0 {
		return fmt.Errorf("receipts must be an explicit non-empty array")
	}
	receipts := make(map[string]RedactionReceipt, len(bundle.Receipts))
	previous := ""
	for index, receipt := range bundle.Receipts {
		if err := receipt.Validate(bundle.Policy); err != nil {
			return fmt.Errorf("receipts[%d]: %w", index, err)
		}
		key := artifactRefKey(receipt.SourceRef)
		if index > 0 && previous >= key {
			return fmt.Errorf("receipts must be uniquely sorted by source ref")
		}
		previous = key
		receipts[key] = receipt
	}
	if bundle.Samples == nil || len(bundle.Samples) == 0 {
		return fmt.Errorf("samples must be an explicit non-empty array")
	}
	for index, sample := range bundle.Samples {
		if err := sample.GovernedSample.Validate(bundle.RepositoryID); err != nil {
			return fmt.Errorf("samples[%d]: %w", index, err)
		}
		if index > 0 && bundle.Samples[index-1].GovernedSample.CaseID >= sample.GovernedSample.CaseID {
			return fmt.Errorf("samples must be uniquely sorted by case_id")
		}
		if len(sample.RedactedRefs) != len(sample.GovernedSample.ArtifactRefs) {
			return fmt.Errorf("samples[%d] redacted refs do not cover source refs", index)
		}
		for refIndex, source := range sample.GovernedSample.ArtifactRefs {
			receipt, ok := receipts[artifactRefKey(source)]
			if !ok || !reflect.DeepEqual(receipt.RedactedRef, sample.RedactedRefs[refIndex]) {
				return fmt.Errorf("samples[%d] redacted ref binding mismatch", index)
			}
		}
	}
	if bundle.CreatedAt.IsZero() || bundle.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	copy := bundle
	copy.SHA256 = ""
	digest, err := runmodel.DigestJSON(copy)
	if err != nil {
		return err
	}
	if bundle.SHA256 != digest {
		return fmt.Errorf("training export bundle SHA-256 does not match content")
	}
	return nil
}

func (receipt RedactionReceipt) Validate(policy StrictRedactionPolicy) error {
	if err := receipt.SourceRef.Validate(); err != nil {
		return err
	}
	if err := receipt.RedactedRef.Validate(); err != nil {
		return err
	}
	if receipt.RedactedRef.Contract != runmodel.ContractTrainingRedactedText {
		return fmt.Errorf("redacted ref contract is invalid")
	}
	if receipt.PolicySHA256 != policy.SHA256 || receipt.Encoding != "utf-8" || receipt.InputSizeBytes != receipt.SourceRef.SizeBytes || receipt.OutputSizeBytes != receipt.RedactedRef.SizeBytes {
		return fmt.Errorf("redaction receipt binding is invalid")
	}
	if receipt.Hits == nil {
		return fmt.Errorf("hits must be an explicit array")
	}
	previous := ""
	for _, hit := range receipt.Hits {
		if !slices.Contains(strictRuleIDs, hit.RuleID) || hit.Count == 0 || previous >= hit.RuleID {
			return fmt.Errorf("redaction hits must be sorted, unique, known, and positive")
		}
		previous = hit.RuleID
	}
	return nil
}

func DecodeExportRequest(data []byte) (ExportRequest, error) {
	var request ExportRequest
	if err := decodeStrict(data, &request); err != nil {
		return request, fmt.Errorf("decode training export request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return request, fmt.Errorf("validate training export request: %w", err)
	}
	return request, nil
}
func DecodeExportBundle(data []byte) (ExportBundle, error) {
	var bundle ExportBundle
	if err := decodeStrict(data, &bundle); err != nil {
		return bundle, fmt.Errorf("decode training export bundle: %w", err)
	}
	if err := bundle.Validate(); err != nil {
		return bundle, fmt.Errorf("validate training export bundle: %w", err)
	}
	return bundle, nil
}

func artifactRefKey(ref runmodel.ArtifactRef) string {
	return ref.URI + "\x00" + ref.Contract + "\x00" + fmt.Sprintf("%d", ref.SizeBytes)
}
