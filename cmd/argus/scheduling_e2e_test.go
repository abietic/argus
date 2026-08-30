package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/scheduling"
	"argus.local/argus/internal/store/local"
)

func TestCLIUsesSameStoreForReviewAndReplayWorkloadCoordination(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "review.go", "package fixture\n")
	base := commitCLITarget(t, repositoryPath, "base")
	writeCLITargetFile(
		t,
		repositoryPath,
		"review.go",
		"package fixture\n// ARGUS_BUG durable scheduling\n",
	)
	head := commitCLITarget(t, repositoryPath, "head")
	storePath := t.TempDir()

	var reviewOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"review",
		"--repo", repositoryPath,
		"--mode", "diff",
		"--base", base,
		"--head", head,
		"--store", storePath,
		"--json",
	}, &reviewOutput); err != nil {
		t.Fatalf("runWithIO(review) error = %v", err)
	}
	var review runOutput
	if err := json.Unmarshal(reviewOutput.Bytes(), &review); err != nil {
		t.Fatalf("decode review output: %v", err)
	}
	if review.Run.Status != runmodel.RunStatusSucceeded {
		t.Fatalf("review run = %+v", review.Run)
	}

	store, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	workloads, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	reviewWorkload, err := workloads.Get(review.Run.RunID + "-workload")
	if err != nil {
		t.Fatal(err)
	}
	if reviewWorkload.Spec.Class != scheduling.ClassIncrementalMR ||
		reviewWorkload.State != scheduling.StateSucceeded ||
		reviewWorkload.Terminal == nil ||
		reviewWorkload.LastLease == nil ||
		reviewWorkload.LastLease.WorkerID != "argus-local-cli" {
		t.Fatalf("review workload = %+v", reviewWorkload)
	}

	var replayOutput bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"replay",
		"--run", review.Run.RunID,
		"--from", "verify",
		"--store", storePath,
		"--json",
	}, &replayOutput); err != nil {
		t.Fatalf("runWithIO(replay) error = %v", err)
	}
	var replay runOutput
	if err := json.Unmarshal(replayOutput.Bytes(), &replay); err != nil {
		t.Fatalf("decode replay output: %v", err)
	}
	replayWorkload, err := workloads.Get(replay.Run.RunID + "-workload")
	if err != nil {
		t.Fatal(err)
	}
	if replayWorkload.Spec.Class != scheduling.ClassEvalReplay ||
		replayWorkload.State != scheduling.StateSucceeded ||
		replayWorkload.Terminal == nil {
		t.Fatalf("replay workload = %+v", replayWorkload)
	}
}
