package configdefaults

import (
	"testing"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/workflow"
)

func TestRevisionAndBundleUseTheSameCompleteBaseline(t *testing.T) {
	t.Parallel()
	options := testOptions()
	definition := workflow.DefaultReviewDefinition()
	revision, err := Revision(options, definition)
	if err != nil {
		t.Fatalf("Revision() error = %v", err)
	}
	bundle, err := Bundle(options, definition)
	if err != nil {
		t.Fatalf("Bundle() error = %v", err)
	}
	resolved, err := reviewconfig.Resolve(bundle.Context, []reviewconfig.Revision{revision})
	if err != nil {
		t.Fatalf("Resolve(Revision) error = %v", err)
	}
	if resolved.SHA256 != bundle.SHA256 ||
		resolved.BundleID != bundle.BundleID ||
		len(resolved.AppliedRevisions) != 1 ||
		resolved.AppliedRevisions[0].ID != options.ID {
		t.Fatalf("Revision and Bundle diverged:\nresolved=%+v\nbundle=%+v", resolved, bundle)
	}
}

func testOptions() Options {
	return Options{
		ID:             "local-default",
		Revision:       "1",
		MaxFiles:       100,
		MaxPatchBytes:  1 << 20,
		MaxInputBytes:  1 << 20,
		MaxOutputBytes: 1 << 20,
		MaxAttempts:    2,
		AllowedModes:   []string{"diff", "scope", "selection"},
		TargetInclude:  []string{"**"},
		TargetExclude:  []string{},
		Context: reviewconfig.ResolutionContext{
			TenantID:       "tenant-1",
			OrganizationID: "organization-1",
			RepositoryID:   "repository-1",
			Path:           "internal/review.go",
			InvocationID:   "invocation-1",
		},
	}
}
