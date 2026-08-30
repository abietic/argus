package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
)

func TestWorkloadPressureCommandReadsVersionedBackpressureProjection(t *testing.T) {
	storePath := t.TempDir()
	state, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := scheduling.NewRepository(state, scheduling.DefaultLocalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	submittedAt := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	_, err = repository.Submit(t.Context(), scheduling.WorkloadSpec{
		SchemaVersion: scheduling.WorkloadSchemaVersion,
		WorkloadID:    "pressure-workload", RunID: "pressure-run", TenantID: "local",
		Class: scheduling.ClassFullScan, Priority: 0,
		InputRef:    "artifact://local/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SubmittedAt: submittedAt, ExecutionDeadline: submittedAt.Add(time.Hour),
	}, scheduling.Mutation{
		IdempotencyKey: "submit-pressure-workload", Actor: "test", Audit: "pressure fixture", At: submittedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"workload", "pressure", "--store", storePath,
		"--at", "2026-08-26T01:05:00Z", "--json",
	}, &output); err != nil {
		t.Fatal(err)
	}
	var decoded workloadPressureOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.StorePath != state.Root() || decoded.Snapshot.LedgerSequence != 1 ||
		decoded.Snapshot.Global.QueueDepth != 1 || decoded.Snapshot.States.Pending != 1 ||
		decoded.Snapshot.Classes[2].Class != scheduling.ClassFullScan ||
		decoded.Snapshot.Classes[2].Capacity.QueueDepth != 1 {
		t.Fatalf("workload pressure output = %+v", decoded)
	}
	if _, err := scheduling.DecodePressureSnapshot(mustJSON(t, decoded.Snapshot)); err != nil {
		t.Fatalf("CLI snapshot is not strict: %v", err)
	}
}

func TestWorkloadPressureFlagsFailClosed(t *testing.T) {
	if _, _, err := parseWorkloadPressureFlags([]string{"--store", t.TempDir()}); err == nil {
		t.Fatal("pressure flags accepted missing --at")
	}
	if _, _, err := parseWorkloadPressureFlags([]string{
		"--store", t.TempDir(), "--at", "2026-08-26T09:00:00+08:00",
	}); err == nil {
		t.Fatal("pressure flags accepted non-UTC timestamp")
	}
}

func TestWorkloadPressureCommandDoesNotCreateMissingState(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "missing-state")
	err := runWorkload(context.Background(), []string{
		"pressure", "--store", storePath, "--at", "2026-08-26T01:05:00Z",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("workload pressure accepted a missing state directory")
	}
	if _, statErr := os.Stat(storePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("read-only workload pressure created state: %v", statErr)
	}
}

func TestWorkloadPressureCommandHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runWorkload(ctx, []string{
		"pressure", "--store", t.TempDir(), "--at", "2026-08-26T01:05:00Z",
	}, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runWorkload() error = %v, want context canceled", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
