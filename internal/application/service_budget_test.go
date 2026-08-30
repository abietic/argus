package application

import (
	"context"
	"strings"
	"testing"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/workflow"
)

func TestServiceFreezesRequestLimitsInConfigBundleAndReplay(t *testing.T) {
	repositoryPath, base, head := reviewFixture(t)
	service, repository := newTestService(t, ServiceOptions{})
	const (
		maxFiles      = 1
		maxPatchBytes = int64(1024)
	)

	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
		MaxFiles:       maxFiles,
		MaxPatchBytes:  maxPatchBytes,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	sourceSnapshot, sourceBundle := loadRunConfigBundle(
		t,
		repository,
		outcome.Run.ExecutionSnapshotID,
	)
	if sourceBundle.Target.MaxFiles != maxFiles ||
		sourceBundle.Target.MaxPatchBytes != maxPatchBytes ||
		sourceBundle.Budget.MaxInputBytes != DefaultLocalConfig().MaxMaterializedBytes ||
		sourceBundle.Budget.MaxOutputBytes !=
			workflow.DefaultReviewDefinition().Stages[0].Budget.MaxOutputBytes {
		t.Fatalf(
			"frozen request limits = max_files %d, max_patch_bytes %d, "+
				"max_input_bytes %d, max_output_bytes %d",
			sourceBundle.Target.MaxFiles,
			sourceBundle.Target.MaxPatchBytes,
			sourceBundle.Budget.MaxInputBytes,
			sourceBundle.Budget.MaxOutputBytes,
		)
	}
	if sourceSnapshot.Config.SHA256 != sourceBundle.SHA256 ||
		sourceSnapshot.ToolPolicy.MaxOutputBytes != sourceBundle.Budget.MaxOutputBytes {
		t.Fatalf("execution snapshot does not project frozen bundle: %+v", sourceSnapshot)
	}

	replayed, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: outcome.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	replaySnapshot, replayBundle := loadRunConfigBundle(
		t,
		repository,
		replayed.Run.ExecutionSnapshotID,
	)
	if replaySnapshot.ConfigBundleRef != sourceSnapshot.ConfigBundleRef ||
		replaySnapshot.Config != sourceSnapshot.Config ||
		replayBundle.SHA256 != sourceBundle.SHA256 ||
		replayBundle.Context != sourceBundle.Context {
		t.Fatalf(
			"replay did not reuse source frozen config:\nsource=%+v\nreplay=%+v",
			sourceSnapshot,
			replaySnapshot,
		)
	}
}

func TestServiceRejectsRequestLimitOverridesForCustomConfigBundle(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReviewRequest)
	}{
		{
			name: "max files",
			mutate: func(request *ReviewRequest) {
				request.MaxFiles = DefaultLocalConfig().MaxFiles - 1
			},
		},
		{
			name: "max patch bytes",
			mutate: func(request *ReviewRequest) {
				request.MaxPatchBytes = DefaultLocalConfig().MaxPatchBytes - 1
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositoryPath, base, head := reviewFixture(t)
			repositoryRoot, err := canonicalRepositoryRoot(repositoryPath)
			if err != nil {
				t.Fatalf("canonicalRepositoryRoot() error = %v", err)
			}
			bundle, err := DefaultConfigBundle(
				DefaultLocalConfig(),
				workflow.DefaultReviewDefinition(),
				reviewconfig.ResolutionContext{
					TenantID:       localTenantID,
					OrganizationID: localOrganizationID,
					RepositoryID:   expectedLocalRepositoryID(repositoryRoot),
					InvocationID:   "run-0001",
				},
			)
			if err != nil {
				t.Fatalf("DefaultConfigBundle() error = %v", err)
			}
			service, _ := newTestService(t, ServiceOptions{ConfigBundle: bundle})
			request := ReviewRequest{
				RepositoryPath: repositoryPath,
				BaseRevision:   base,
				HeadRevision:   head,
			}
			test.mutate(&request)

			_, err = service.Review(context.Background(), request)
			if err == nil || !strings.Contains(
				err.Error(),
				"custom config bundle cannot freeze request-level",
			) {
				t.Fatalf("Review() error = %v, want fail-closed custom bundle rejection", err)
			}
		})
	}
}

func loadRunConfigBundle(
	t *testing.T,
	repository *runrepo.Repository,
	snapshotID string,
) (snapshot runmodel.ExecutionSnapshot, bundle reviewconfig.ConfigBundle) {
	t.Helper()
	snapshot, err := repository.LoadExecutionSnapshot(snapshotID)
	if err != nil {
		t.Fatalf("LoadExecutionSnapshot() error = %v", err)
	}
	data, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		t.Fatalf("ReadArtifact(ConfigBundle) error = %v", err)
	}
	bundle, err = reviewconfig.DecodeBundle(data)
	if err != nil {
		t.Fatalf("DecodeBundle() error = %v", err)
	}
	return snapshot, bundle
}
