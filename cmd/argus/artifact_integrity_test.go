package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
)

func TestArtifactIntegrityCLIQuarantineReleaseInspectAndTombstone(t *testing.T) {
	storePath := t.TempDir()
	store, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repository.PutArtifact("argus.test.cli_artifact.v1", []byte("governed bytes"))
	if err != nil {
		t.Fatal(err)
	}
	refPath := writeCLIJSONDescriptor(t, "artifact-ref.json", ref)
	var stdout bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"artifact", "integrity", "inspect", "--store", storePath,
		"--ref", refPath, "--json",
	}, &stdout); err != nil {
		t.Fatalf("artifact integrity inspect error = %v", err)
	}
	var inspected artifactIntegrityOutput
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Record.State != runrepo.ArtifactIntegrityActive || inspected.Record.Sequence != 0 {
		t.Fatalf("initial CLI record = %+v", inspected.Record)
	}

	at := time.Date(2026, 8, 25, 4, 0, 0, 0, time.UTC)
	change := func(key, reason string, offset time.Duration) string {
		return writeCLIJSONDescriptor(t, key+".json", runrepo.ArtifactIntegrityChange{
			SchemaVersion: runrepo.ArtifactIntegrityChangeSchemaVersion,
			Ref:           ref, Reason: reason,
			Mutation: runrepo.ArtifactIntegrityMutation{
				IdempotencyKey: key, Actor: "cli-operator", Audit: "CLI lifecycle test",
				At: at.Add(offset),
			},
		})
	}
	for _, step := range []struct {
		action string
		input  string
		state  runrepo.ArtifactIntegrityState
		seq    uint64
	}{
		{"quarantine", change("cli-quarantine", "manual verification", 0), runrepo.ArtifactIntegrityQuarantined, 1},
		{"release", change("cli-release", "verification passed", time.Minute), runrepo.ArtifactIntegrityActive, 2},
		{"tombstone", change("cli-tombstone", "retention revoked", 2*time.Minute), runrepo.ArtifactIntegrityTombstoned, 3},
	} {
		stdout.Reset()
		if err := runWithIO(context.Background(), []string{
			"artifact", "integrity", step.action, "--store", storePath,
			"--input", step.input, "--json",
		}, &stdout); err != nil {
			t.Fatalf("artifact integrity %s error = %v", step.action, err)
		}
		var output artifactIntegrityOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatal(err)
		}
		if output.Record.State != step.state || output.Record.Sequence != step.seq {
			t.Fatalf("%s output = %+v", step.action, output.Record)
		}
	}
	if _, err := repository.ReadArtifact(ref); !errors.Is(err, runrepo.ErrArtifactTombstoned) {
		t.Fatalf("ReadArtifact() after CLI tombstone error = %v", err)
	}
}
