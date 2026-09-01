package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/training"
)

func TestTrainingCLIHelpAndEmptyList(t *testing.T) {
	var help bytes.Buffer
	if err := runWithIO(context.Background(), []string{"training", "--help"}, &help); err != nil {
		t.Fatalf("training --help error = %v", err)
	}
	if !strings.Contains(help.String(), "training materialize") {
		t.Fatalf("help = %q", help.String())
	}
	root := t.TempDir()
	accessPath := filepath.Join(t.TempDir(), "access.json")
	if err := os.WriteFile(accessPath, []byte(`{"actor":"curator-1","roles":["dataset_curator"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{"training", "list", "--store", root, "--access", accessPath, "--json"}, &output); err != nil {
		t.Fatalf("training list error = %v", err)
	}
	if !strings.Contains(output.String(), `"records":[]`) || !strings.Contains(output.String(), root) {
		t.Fatalf("training list output = %s", output.String())
	}
}

func TestPersistTrainingExportPublishesOnlyRedactedPortableFiles(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"source provenance", "frozen input", "independent adjudication"} {
		if _, err := artifacts.PutArtifact(runmodel.ContractTrainingRedactedText, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	bundleBytes, err := os.ReadFile(filepath.Join("..", "..", "examples", "training-export-bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := training.DecodeExportBundle(bundleBytes)
	if err != nil {
		t.Fatal(err)
	}
	record := training.ExportRecord{Bundle: bundle}
	parent := t.TempDir()
	target := filepath.Join(parent, "portable")
	if err := persistTrainingExport(target, store.Root(), record, artifacts); err != nil {
		t.Fatalf("persistTrainingExport() error = %v", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "manifest.json" || entries[1].Name() != "records.jsonl" {
		t.Fatalf("published file set = %v", entries)
	}
	records, err := os.ReadFile(filepath.Join(target, "records.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var portable portableTrainingRecord
	if err := json.Unmarshal(bytes.TrimSpace(records), &portable); err != nil {
		t.Fatalf("decode portable record: %v", err)
	}
	if portable.SchemaVersion != "argus.training_portable_record.v1alpha1" || len(portable.Artifacts) != 3 {
		t.Fatalf("portable record = %+v", portable)
	}
	if err := persistTrainingExport(target, store.Root(), record, artifacts); err != nil {
		t.Fatalf("exact retry error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "records.jsonl"), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistTrainingExport(target, store.Root(), record, artifacts); err == nil {
		t.Fatal("different existing target was accepted")
	}
	if err := persistTrainingExport(filepath.Join(store.Root(), "inside-store"), store.Root(), record, artifacts); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("inside-store error = %v", err)
	}
}

func TestPersistTrainingExportRejectsSymlinkParent(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	realParent := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	bundleBytes, readErr := os.ReadFile(filepath.Join("..", "..", "examples", "training-export-bundle.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	bundle, decodeErr := training.DecodeExportBundle(bundleBytes)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	err = persistTrainingExport(filepath.Join(linkParent, "portable"), store.Root(), training.ExportRecord{Bundle: bundle}, artifacts)
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink-parent error = %v", err)
	}
}
