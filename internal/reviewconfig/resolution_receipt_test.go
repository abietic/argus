package reviewconfig

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConfigResolutionReceiptStandaloneValidateAndDecode(t *testing.T) {
	bundle, receipt := testConfigResolutionReceipt(t)
	if err := receipt.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeConfigResolutionReceipt(data)
	if err != nil {
		t.Fatalf("DecodeConfigResolutionReceipt() error = %v", err)
	}
	if err := decoded.ValidateAgainst(bundle); err != nil {
		t.Fatalf("ValidateAgainst() error = %v", err)
	}

	tampered := receipt
	tampered.Revisions = append([]PublishedRevisionBinding{}, receipt.Revisions...)
	tampered.Revisions[0].AssignmentSHA256 = strings.Repeat("f", 64)
	tamperedData, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeConfigResolutionReceipt(tamperedData); err == nil ||
		!strings.Contains(err.Error(), "identity") {
		t.Fatalf("DecodeConfigResolutionReceipt(tampered) error = %v", err)
	}
}

func TestConfigResolutionReceiptStandaloneFailsClosed(t *testing.T) {
	_, valid := testConfigResolutionReceipt(t)
	for _, test := range []struct {
		name string
		edit func(*ConfigResolutionReceipt)
		want string
	}{
		{"origin", func(value *ConfigResolutionReceipt) { value.Origin = "manual" }, "origin"},
		{"context", func(value *ConfigResolutionReceipt) { value.Context.TenantID = "" }, "context"},
		{"bundle id", func(value *ConfigResolutionReceipt) { value.BundleID = "bundle-floating" }, "bundle_id"},
		{"empty revisions", func(value *ConfigResolutionReceipt) { value.Revisions = []PublishedRevisionBinding{} }, "non-empty"},
		{"source operation", func(value *ConfigResolutionReceipt) { value.Revisions[0].Source.Operation = "draft" }, "operation"},
		{"source selector", func(value *ConfigResolutionReceipt) { value.Revisions[0].Source.Selector = "tenant=other" }, "selector"},
		{"publish sequence", func(value *ConfigResolutionReceipt) { value.Revisions[0].PublishSequence = 0 }, "publication observation"},
		{"published timezone", func(value *ConfigResolutionReceipt) {
			value.Revisions[0].PublishedAt = value.Revisions[0].PublishedAt.In(time.FixedZone("offset", 3600))
		}, "publication observation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Revisions = append([]PublishedRevisionBinding{}, valid.Revisions...)
			test.edit(&candidate)
			resealConfigResolutionReceipt(t, &candidate)
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfigResolutionReceiptStrictDecodeStandalone(t *testing.T) {
	for name, data := range map[string][]byte{
		"null":      []byte("null"),
		"unknown":   []byte(`{"unknown":true}`),
		"duplicate": []byte(`{"schema_version":"a","schema_version":"b"}`),
		"trailing":  []byte(`{} {}`),
		"unsealed":  []byte(`{"schema_version":"argus.config_resolution_receipt.v1alpha1"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConfigResolutionReceipt(data); err == nil {
				t.Fatal("DecodeConfigResolutionReceipt() accepted invalid JSON")
			}
		})
	}
}

func TestReplayVariantConfigReceiptBindsGovernedBaselineAndAtomicChange(t *testing.T) {
	baseline, source := testConfigResolutionReceipt(t)
	variant := baseline
	variant.Budget.StageTimeoutMS--
	variant.BundleID, variant.SHA256 = "", ""
	digest, err := DigestBundle(variant)
	if err != nil {
		t.Fatal(err)
	}
	variant.SHA256 = digest
	variant.BundleID = "bundle-" + digest[:24]
	receipt, err := NewReplayVariantConfigReceipt(
		variant, baseline, source, "budget",
		[]string{"budget.stage_timeout_ms"},
	)
	if err != nil {
		t.Fatalf("NewReplayVariantConfigReceipt() error = %v", err)
	}
	if receipt.Origin != ConfigResolutionOriginReplayVariant || receipt.ReplayVariant == nil ||
		receipt.ReplayVariant.SourceReceiptID != source.ReceiptID ||
		receipt.ReplayVariant.BaselineSHA256 != baseline.SHA256 {
		t.Fatalf("replay variant receipt = %+v", receipt)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeConfigResolutionReceipt(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateAgainst(variant); err != nil {
		t.Fatal(err)
	}

	tampered := receipt
	binding := *receipt.ReplayVariant
	binding.Variable = "model"
	tampered.ReplayVariant = &binding
	resealConfigResolutionReceipt(t, &tampered)
	if err := tampered.ValidateAgainst(variant); err != nil {
		t.Fatalf("standalone receipt should remain self-consistent before formal closure: %v", err)
	}

	invalidID := receipt
	invalidBinding := *receipt.ReplayVariant
	invalidBinding.SourceReceiptID = "config-resolution-gggggggggggggggggggggggg"
	invalidID.ReplayVariant = &invalidBinding
	resealConfigResolutionReceipt(t, &invalidID)
	if err := invalidID.Validate(); err == nil || !strings.Contains(err.Error(), "source_receipt_id") {
		t.Fatalf("Validate() invalid source receipt ID error = %v", err)
	}
}

func testConfigResolutionReceipt(t *testing.T) (ConfigBundle, ConfigResolutionReceipt) {
	t.Helper()
	bundle, err := Resolve(testContext(), []Revision{completePlatformRevision()})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	receipt, err := NewConfigResolutionReceipt(bundle, []PublishedRevisionBinding{
		{
			Source: bundle.AppliedRevisions[0], RevisionSHA256: testSHA,
			PublishEventID: "publish-event-1", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: testSHA,
		},
	})
	if err != nil {
		t.Fatalf("NewConfigResolutionReceipt() error = %v", err)
	}
	return bundle, receipt
}

func resealConfigResolutionReceipt(t *testing.T, receipt *ConfigResolutionReceipt) {
	t.Helper()
	receipt.ReceiptID = ""
	receipt.SHA256 = ""
	digest, err := DigestConfigResolutionReceipt(*receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.SHA256 = digest
	receipt.ReceiptID = "config-resolution-" + digest[:24]
}
