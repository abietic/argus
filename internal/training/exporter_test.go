package training

import (
	"context"
	"errors"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runrepo"
)

func TestExporterBuildExactRetryAndRestore(t *testing.T) {
	fixture := newTrainingFixture(t)
	manifestRecord, err := fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	exporter, err := NewExporter(fixture.store, fixture.repository, fixture.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	at := fixture.mutation.At.Add(time.Minute)
	request := ExportRequest{SchemaVersion: ExportRequestSchemaVersion, ExportID: "training-export-1", ManifestID: manifestRecord.Manifest.ManifestID, ManifestRef: manifestRecord.ManifestRef, Policy: DefaultStrictRedactionPolicy(), CreatedAt: at}
	mutation := evaluation.Mutation{IdempotencyKey: "training-export-build-1", Actor: "curator-2", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "build deterministic strict-redacted export", At: at}
	record, err := exporter.Build(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if record.Bundle.ContainsUnredactedSourceBytes || record.Bundle.PortableFormat != PortableRecordsFormat || len(record.Bundle.Receipts) != len(fixture.refs) {
		t.Fatalf("bundle = %+v", record.Bundle)
	}
	for _, receipt := range record.Bundle.Receipts {
		if receipt.RedactedRef.Contract != "argus.training_redacted_text.v1alpha1" {
			t.Fatalf("receipt = %+v", receipt)
		}
	}
	retry, err := exporter.Build(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if retry.BundleRef != record.BundleRef {
		t.Fatalf("retry ref = %+v, want %+v", retry.BundleRef, record.BundleRef)
	}
	reopened, err := NewExporter(fixture.store, fixture.repository, fixture.artifacts)
	if err != nil {
		t.Fatalf("NewExporter(reopen) error = %v", err)
	}
	loaded, err := reopened.Get(request.ExportID, evaluation.Access{Actor: "curator-2", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Bundle.SHA256 != record.Bundle.SHA256 {
		t.Fatalf("loaded SHA = %s, want %s", loaded.Bundle.SHA256, record.Bundle.SHA256)
	}
}

func TestExporterRejectsDriftConflictAndQuarantinedOutput(t *testing.T) {
	fixture := newTrainingFixture(t)
	manifestRecord, err := fixture.repository.Materialize(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	exporter, err := NewExporter(fixture.store, fixture.repository, fixture.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	at := fixture.mutation.At.Add(time.Minute)
	request := ExportRequest{SchemaVersion: ExportRequestSchemaVersion, ExportID: "training-export-1", ManifestID: manifestRecord.Manifest.ManifestID, ManifestRef: manifestRecord.ManifestRef, Policy: DefaultStrictRedactionPolicy(), CreatedAt: at}
	mutation := evaluation.Mutation{IdempotencyKey: "training-export-build-1", Actor: "curator-2", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "build export", At: at}
	drift := request
	drift.ManifestRef.SHA256 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	drift.ManifestRef.URI = "artifact://local/sha256/" + drift.ManifestRef.SHA256
	if _, err := exporter.Build(context.Background(), drift, mutation); !errors.Is(err, ErrContaminated) {
		t.Fatalf("drift error = %v", err)
	}
	record, err := exporter.Build(context.Background(), request, mutation)
	if err != nil {
		t.Fatal(err)
	}
	conflict := request
	conflict.ExportID = "training-export-2"
	if _, err := exporter.Build(context.Background(), conflict, mutation); !errors.Is(err, ErrExportConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	redacted := record.Bundle.Receipts[0].RedactedRef
	_, err = fixture.artifacts.QuarantineArtifact(context.Background(), redacted, "test redacted output quarantine", runrepo.ArtifactIntegrityMutation{IdempotencyKey: "quarantine-redacted", Actor: "integrity-admin", Audit: "test", At: at.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewExporter(fixture.store, fixture.repository, fixture.artifacts); !errors.Is(err, ErrExportCorrupt) {
		t.Fatalf("NewExporter error = %v, want ErrExportCorrupt", err)
	}
}
