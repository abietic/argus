package gitadapter

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	findingLineageRenameThresholdBPS = uint16(5000)
	maxFindingLineageRenames         = 10_000
)

// ResolveFindingLineageEvidence proves strict baseline-before-variant order
// using one local Git object graph and extracts only Git-admitted renames. It
// accepts exact commit OIDs, never mutable refs, and evaluates the graph from
// an isolated object view so work-tree, index, hooks, and local attributes do
// not affect the evidence.
func (adapter *Adapter) ResolveFindingLineageEvidence(
	ctx context.Context,
	baselineRepositoryPath string,
	variantRepositoryPath string,
	baselineRevision string,
	variantRevision string,
	renameThresholdBPS uint16,
) (
	contractsv1alpha1.FindingLineageGitAncestry,
	[]contractsv1alpha1.FindingLineagePathMapping,
	error,
) {
	if err := validateAdapterContext(adapter, ctx); err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, err
	}
	if renameThresholdBPS != findingLineageRenameThresholdBPS {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, &AdapterError{
			Code: ErrorInvalidRequest, Field: "rename_threshold_bps",
			Err: fmt.Errorf("must equal %d", findingLineageRenameThresholdBPS),
		}
	}

	baselineRoot, baselineFormat, err := adapter.openExactRepository(ctx, baselineRepositoryPath)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("open baseline repository: %w", err)
	}
	variantRoot, variantFormat, err := adapter.openExactRepository(ctx, variantRepositoryPath)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("open variant repository: %w", err)
	}
	if baselineFormat != variantFormat {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("repositories use different Git object formats")
	}
	if !exactOIDMatchesFormat(baselineRevision, baselineFormat) ||
		!exactOIDMatchesFormat(variantRevision, baselineFormat) {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, &AdapterError{
			Code: ErrorInvalidRequest, Field: "revision",
			Err: errors.New("lineage requires exact lowercase commit OIDs matching the repository object format"),
		}
	}

	baselineCommonDir, err := adapter.gitCommonDir(ctx, baselineRoot)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("resolve baseline Git common directory: %w", err)
	}
	variantCommonDir, err := adapter.gitCommonDir(ctx, variantRoot)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("resolve variant Git common directory: %w", err)
	}
	if baselineCommonDir != variantCommonDir {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("lineage runs do not share the same local Git object graph")
	}

	baselineOID, err := adapter.resolveRevision(ctx, baselineRoot, baselineRevision)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("resolve baseline commit: %w", err)
	}
	variantOID, err := adapter.resolveRevision(ctx, variantRoot, variantRevision)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("resolve variant commit: %w", err)
	}
	if baselineOID != baselineRevision || variantOID != variantRevision || baselineOID == variantOID {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("lineage requires two distinct exact commit OIDs")
	}

	objectView, cleanup, err := adapter.newIsolatedObjectView(ctx, baselineRoot, baselineFormat)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("create isolated Git lineage view: %w", err)
	}
	defer cleanup()
	objectViewArg := "--git-dir=" + objectView

	err = adapter.run(ctx, baselineRoot, []string{
		objectViewArg, "merge-base", "--is-ancestor", baselineOID, variantOID,
	}, &anyOutputWriter{})
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("baseline commit is not an ancestor of variant commit")
		}
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("verify Git ancestry: %w", err)
	}
	mergeBaseData, err := adapter.runOutput(
		ctx, baselineRoot, objectViewArg, "merge-base", baselineOID, variantOID,
	)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("resolve Git merge base: %w", err)
	}
	mergeBaseOID := trimLine(mergeBaseData)
	if mergeBaseOID != baselineOID {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("Git merge base does not close strict ancestry evidence")
	}

	renameData, err := adapter.runOutput(
		ctx,
		baselineRoot,
		objectViewArg,
		"diff",
		"--name-status",
		"-z",
		"--find-renames=50%",
		"--diff-filter=R",
		baselineOID,
		variantOID,
		"--",
	)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("read Git rename evidence: %w", err)
	}
	collector := newNameStatusCollector(maxFindingLineageRenames)
	if _, err := collector.Write(renameData); err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("parse Git rename evidence: %w", err)
	}
	if err := collector.finish(); err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("parse Git rename evidence: %w", err)
	}
	if collector.total != len(collector.files) {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("Git rename evidence exceeds %d entries", maxFindingLineageRenames)
	}
	mappings := make([]contractsv1alpha1.FindingLineagePathMapping, 0, len(collector.files))
	for _, change := range collector.files {
		if change.Status != ChangeRenamed || change.SimilarityPercent == nil || !change.Included ||
			!isPortableRepositoryPath(change.OldPath) || !isPortableRepositoryPath(change.Path) {
			return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("Git returned invalid rename evidence")
		}
		similarityBPS := uint16(*change.SimilarityPercent * 100)
		if similarityBPS < renameThresholdBPS {
			return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("Git returned a rename below the admitted threshold")
		}
		mappings = append(mappings, contractsv1alpha1.FindingLineagePathMapping{
			BaselinePath: change.OldPath, VariantPath: change.Path, SimilarityBPS: similarityBPS,
		})
	}
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].BaselinePath != mappings[j].BaselinePath {
			return mappings[i].BaselinePath < mappings[j].BaselinePath
		}
		return mappings[i].VariantPath < mappings[j].VariantPath
	})

	repositoryIdentity := sha256.Sum256([]byte("argus.local-git-common-dir.v1\x00" + baselineCommonDir))
	evidence, err := contractsv1alpha1.SealFindingLineageGitAncestry(
		contractsv1alpha1.FindingLineageGitAncestry{
			Authority: "local_git_object_graph", RepositoryIdentitySHA256: fmt.Sprintf("%x", repositoryIdentity),
			ObjectFormat: baselineFormat, BaselineHeadOID: baselineOID, VariantHeadOID: variantOID,
			MergeBaseOID: mergeBaseOID, BaselineIsAncestor: true,
		},
		mappings,
	)
	if err != nil {
		return contractsv1alpha1.FindingLineageGitAncestry{}, nil, fmt.Errorf("seal Git lineage evidence: %w", err)
	}
	return evidence, mappings, nil
}

func (adapter *Adapter) gitCommonDir(ctx context.Context, root string) (string, error) {
	data, err := adapter.runOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(string(data))
	if commonDir == "" || strings.ContainsAny(commonDir, "\r\n") || !filepath.IsAbs(commonDir) {
		return "", fmt.Errorf("Git returned an invalid common directory")
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(commonDir), nil
}
