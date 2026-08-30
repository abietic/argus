package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"argus.local/argus/internal/configdefaults"
	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/workflow"
)

func TestPublishedConfigDrivesReviewAndIsFrozenForReplay(t *testing.T) {
	t.Parallel()
	repositoryPath, base, head := reviewFixture(t)
	provider := newPublishedConfigProvider(t)
	first := publishedRuntimeRevision(t, "runtime-policy", "1", false)
	publishRuntimeRevision(t, provider, first, 1)

	service, runRepository := newTestService(t, ServiceOptions{
		ConfigProvider: provider,
	})
	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if outcome.Report == nil || len(outcome.Report.Findings) != 0 {
		t.Fatalf("published disabled rule did not drive review: %+v", outcome.Report)
	}
	sourceSnapshot, sourceBundle := loadRunConfigBundle(
		t,
		runRepository,
		outcome.Run.ExecutionSnapshotID,
	)
	if len(sourceBundle.AppliedRevisions) != 1 ||
		sourceBundle.AppliedRevisions[0].ID != first.ID ||
		sourceBundle.Context.TenantID != localTenantID ||
		sourceBundle.Context.OrganizationID != localOrganizationID ||
		sourceBundle.Context.InvocationID != outcome.Run.RunID ||
		sourceBundle.Context.Path != "" {
		t.Fatalf("frozen published bundle context/lineage = %+v", sourceBundle)
	}

	// Superseding the active revision must not change a replay's frozen config.
	second := publishedRuntimeRevision(t, "runtime-policy", "2", true)
	publishRuntimeRevision(t, provider, second, 10)
	replayed, err := service.Replay(context.Background(), ReplayRequest{
		SourceRunID: outcome.Run.RunID,
		StartStage:  string(reviewcore.StageVerify),
	})
	if err != nil {
		t.Fatalf("Replay() after config supersession error = %v", err)
	}
	replaySnapshot, replayBundle := loadRunConfigBundle(
		t,
		runRepository,
		replayed.Run.ExecutionSnapshotID,
	)
	if replaySnapshot.ConfigBundleRef != sourceSnapshot.ConfigBundleRef ||
		replayBundle.SHA256 != sourceBundle.SHA256 ||
		replayBundle.AppliedRevisions[0].Revision != "1" {
		t.Fatalf("replay did not preserve source published config")
	}
}

func TestConfigProviderFailsClosedBeforeRunCreation(t *testing.T) {
	t.Parallel()
	repositoryPath, base, head := reviewFixture(t)
	provider := newPublishedConfigProvider(t)
	draft := publishedRuntimeRevision(t, "runtime-policy", "draft", false)
	if _, err := provider.Create(
		context.Background(),
		draft,
		configMutation("create-draft", 1),
	); err != nil {
		t.Fatal(err)
	}
	service, runRepository := newTestService(t, ServiceOptions{
		ConfigProvider: provider,
	})
	_, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if !errors.Is(err, configrepo.ErrNoPublishedConfig) {
		t.Fatalf("Review(draft only) error = %v, want ErrNoPublishedConfig", err)
	}
	history, historyErr := runRepository.History(0)
	if historyErr != nil || len(history) != 0 {
		t.Fatalf("failed config admission created run history: %#v, %v", history, historyErr)
	}
}

func TestConfigProviderRejectsRequestLimitOverrides(t *testing.T) {
	t.Parallel()
	repositoryPath, base, head := reviewFixture(t)
	provider := newPublishedConfigProvider(t)
	publishRuntimeRevision(
		t,
		provider,
		publishedRuntimeRevision(t, "runtime-policy", "1", false),
		1,
	)
	service, _ := newTestService(t, ServiceOptions{ConfigProvider: provider})
	_, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
		MaxFiles:       1,
	})
	if err == nil {
		t.Fatal("Review(provider + request override) unexpectedly succeeded")
	}
}

func TestNewServiceRejectsAmbiguousStaticAndProviderConfig(t *testing.T) {
	t.Parallel()
	repositoryPath, _, _ := reviewFixture(t)
	repositoryRoot, err := canonicalRepositoryRoot(repositoryPath)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	provider := newPublishedConfigProvider(t)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runRepository, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	source, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(source, runRepository, ServiceOptions{
		ConfigBundle:   bundle,
		ConfigProvider: provider,
	}); err == nil {
		t.Fatal("NewService accepted both ConfigBundle and ConfigProvider")
	}
}

func newPublishedConfigProvider(t *testing.T) *configrepo.Repository {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider, err := configrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func publishedRuntimeRevision(
	t *testing.T,
	id string,
	revision string,
	explicitBugEnabled bool,
) reviewconfig.Revision {
	t.Helper()
	config := DefaultLocalConfig()
	value, err := configdefaults.Revision(configdefaults.Options{
		ID:             id,
		Revision:       revision,
		MaxFiles:       config.MaxFiles,
		MaxPatchBytes:  config.MaxPatchBytes,
		MaxInputBytes:  config.MaxMaterializedBytes,
		MaxOutputBytes: workflow.DefaultReviewDefinition().Stages[0].Budget.MaxOutputBytes,
		MaxAttempts:    config.MaxAttempts,
		AllowedModes:   []string{"diff", "scope", "selection"},
		TargetInclude:  []string{"**"},
		TargetExclude:  []string{},
	}, workflow.DefaultReviewDefinition())
	if err != nil {
		t.Fatal(err)
	}
	for index := range value.Patch.RulePack.Rules.Upsert {
		rule := &value.Patch.RulePack.Rules.Upsert[index]
		if rule.ID == "explicit-bug-marker" {
			rule.Enabled = explicitBugEnabled
		}
	}
	if err := value.Validate(); err != nil {
		t.Fatalf("mutated runtime revision: %v", err)
	}
	return value
}

func publishRuntimeRevision(
	t *testing.T,
	provider *configrepo.Repository,
	revision reviewconfig.Revision,
	offset int,
) {
	t.Helper()
	if _, err := provider.Create(
		context.Background(),
		revision,
		configMutation("create-"+revision.Revision, offset),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ValidateRevision(
		context.Background(),
		revision.ID,
		revision.Revision,
		configMutation("validate-"+revision.Revision, offset+1),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Publish(
		context.Background(),
		revision.ID,
		revision.Revision,
		configrepo.Rollout{Percentage: 100},
		configMutation("publish-"+revision.Revision, offset+2),
	); err != nil {
		t.Fatal(err)
	}
}

func configMutation(id string, offset int) configrepo.Mutation {
	return configrepo.Mutation{
		IdempotencyKey: id,
		Actor:          "application-test",
		Audit:          "published config integration test",
		At: time.Date(
			2026,
			time.July,
			27,
			15,
			0,
			offset,
			0,
			time.UTC,
		),
	}
}
