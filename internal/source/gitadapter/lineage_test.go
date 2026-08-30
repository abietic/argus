package gitadapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveFindingLineageEvidenceProvesAncestryAndRename(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "internal/old.go", "package internal\n\nfunc Value() int { return 1 }\n")
	baseline := commitAll(t, repository, "baseline")
	if err := os.Rename(
		filepath.Join(repository, "internal", "old.go"),
		filepath.Join(repository, "internal", "new.go"),
	); err != nil {
		t.Fatal(err)
	}
	variant := commitAll(t, repository, "rename")

	adapter := newTestAdapter(t)
	evidence, mappings, err := adapter.ResolveFindingLineageEvidence(
		context.Background(), repository, repository, baseline, variant, 5000,
	)
	if err != nil {
		t.Fatalf("ResolveFindingLineageEvidence() error = %v", err)
	}
	if evidence.Authority != "local_git_object_graph" || !evidence.BaselineIsAncestor ||
		evidence.BaselineHeadOID != baseline || evidence.VariantHeadOID != variant ||
		evidence.MergeBaseOID != baseline || evidence.EvidenceSHA256 == "" {
		t.Fatalf("ancestry evidence = %+v", evidence)
	}
	if len(mappings) != 1 || mappings[0].BaselinePath != "internal/old.go" ||
		mappings[0].VariantPath != "internal/new.go" || mappings[0].SimilarityBPS != 10000 {
		t.Fatalf("rename mappings = %+v", mappings)
	}
}

func TestResolveFindingLineageEvidenceRejectsUnprovenOrderAndRepository(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "review.go", "package fixture\n")
	baseline := commitAll(t, repository, "baseline")
	writeFile(t, repository, "review.go", "package fixture\n// changed\n")
	variant := commitAll(t, repository, "variant")
	adapter := newTestAdapter(t)

	if _, _, err := adapter.ResolveFindingLineageEvidence(
		context.Background(), repository, repository, variant, baseline, 5000,
	); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("reverse ancestry error = %v", err)
	}

	other := newTestRepository(t)
	writeFile(t, other, "review.go", "package fixture\n")
	otherRevision := commitAll(t, other, "other")
	if _, _, err := adapter.ResolveFindingLineageEvidence(
		context.Background(), repository, other, baseline, otherRevision, 5000,
	); err == nil || !strings.Contains(err.Error(), "same local Git object graph") {
		t.Fatalf("different repository error = %v", err)
	}
	if _, _, err := adapter.ResolveFindingLineageEvidence(
		context.Background(), repository, repository, baseline, variant, 6000,
	); err == nil {
		t.Fatal("ResolveFindingLineageEvidence() accepted a drifting rename threshold")
	}
	if _, _, err := adapter.ResolveFindingLineageEvidence(
		context.Background(), repository, repository, "HEAD", variant, 5000,
	); err == nil {
		t.Fatal("ResolveFindingLineageEvidence() accepted a mutable ref")
	}
}

func TestResolveFindingLineageEvidenceAcceptsLinkedWorktrees(t *testing.T) {
	repository := newTestRepository(t)
	writeFile(t, repository, "review.go", "package fixture\n")
	baseline := commitAll(t, repository, "baseline")
	writeFile(t, repository, "review.go", "package fixture\n// changed\n")
	variant := commitAll(t, repository, "variant")

	worktree := filepath.Join(t.TempDir(), "linked")
	runTestGit(t, repository, "worktree", "add", "--detach", worktree, variant)
	t.Cleanup(func() {
		_ = os.RemoveAll(worktree)
		runTestGit(t, repository, "worktree", "prune")
	})
	adapter := newTestAdapter(t)
	evidence, _, err := adapter.ResolveFindingLineageEvidence(
		context.Background(), repository, worktree, baseline, variant, 5000,
	)
	if err != nil {
		t.Fatalf("ResolveFindingLineageEvidence() linked worktree error = %v", err)
	}
	if evidence.MergeBaseOID != baseline {
		t.Fatalf("linked worktree evidence = %+v", evidence)
	}
}
