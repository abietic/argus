package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/configdefaults"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
)

func TestFormalConfigPublicationResumesEveryLifecycleBoundary(t *testing.T) {
	for _, boundary := range []int{0, 1, 2, 3} {
		t.Run([]string{"before_create", "after_create", "after_validate", "after_publish"}[boundary], func(t *testing.T) {
			store, repository, revision := newBootstrapConfigFixture(t)
			mutation := bootstrapConfigTestMutation("bootstrap", time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC))
			createBootstrapConfigPrefix(t, repository, revision, mutation, boundary)
			// Reopen the projection as a fresh command after a process interruption.
			restarted, err := configrepo.New(store)
			if err != nil {
				t.Fatal(err)
			}
			for retry := 0; retry < 2; retry++ {
				if err := resolveOrPublishFormalConfig(t.Context(), restarted, revision, mutation); err != nil {
					t.Fatalf("resume publication: %v", err)
				}
			}
			detail, err := restarted.GetWithHistory(revision.ID, revision.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Record.Status != configrepo.StatusPublished || len(detail.History) != 3 {
				t.Fatalf("resumed lifecycle = %+v", detail)
			}
			for index, step := range []string{"create", "validate", "publish"} {
				expected := mutation(step, "")
				if detail.History[index].EventID != expected.IdempotencyKey ||
					!detail.History[index].At.Equal(expected.At) {
					t.Fatalf("lifecycle[%d] changed recovery identity: %+v", index, detail.History[index])
				}
			}
		})
	}
}

func TestFormalConfigPublicationRejectsChangedPartialRecoveryIdentity(t *testing.T) {
	for _, boundary := range []int{1, 2} {
		for _, changed := range []string{"key", "time", "actor", "audit", "content"} {
			t.Run([]string{"", "draft", "validated"}[boundary]+"_"+changed, func(t *testing.T) {
				_, repository, revision := newBootstrapConfigFixture(t)
				at := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
				mutation := bootstrapConfigTestMutation("bootstrap", at)
				createBootstrapConfigPrefix(t, repository, revision, mutation, boundary)
				before, err := repository.GetWithHistory(revision.ID, revision.Revision)
				if err != nil {
					t.Fatal(err)
				}
				changedMutation := func(suffix, audit string) configrepo.Mutation {
					value := mutation(suffix, audit)
					switch changed {
					case "key":
						value.IdempotencyKey += "-other"
					case "time":
						value.At = value.At.Add(time.Second)
					case "actor":
						value.Actor += "-other"
					case "audit":
						value.Audit += " changed"
					}
					return value
				}
				if changed == "content" {
					limit := *revision.Patch.Target.MaxFiles + 1
					revision.Patch.Target.MaxFiles = &limit
				}
				if err := resolveOrPublishFormalConfig(t.Context(), repository, revision, changedMutation); err == nil {
					t.Fatal("changed recovery identity was accepted")
				}
				after, err := repository.GetWithHistory(revision.ID, revision.Revision)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected recovery changed lifecycle")
				}
			})
		}
	}
}

func TestFormalConfigPublicationDoesNotReviveInactiveRevision(t *testing.T) {
	_, repository, first := newBootstrapConfigFixture(t)
	at := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	firstMutation := bootstrapConfigTestMutation("first", at)
	secondMutation := bootstrapConfigTestMutation("second", at.Add(time.Second))
	if err := resolveOrPublishFormalConfig(t.Context(), repository, first, firstMutation); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Revision = "2"
	if err := resolveOrPublishFormalConfig(t.Context(), repository, second, secondMutation); err != nil {
		t.Fatal(err)
	}
	if err := resolveOrPublishFormalConfig(t.Context(), repository, first, firstMutation); err == nil ||
		!strings.Contains(err.Error(), "superseded") {
		t.Fatalf("superseded config recovery error = %v", err)
	}
	if _, err := repository.Rollback(t.Context(), second.ID, second.Revision,
		bootstrapConfigTestMutation("rollback", at.Add(2*time.Second))("second", "restore baseline")); err != nil {
		t.Fatal(err)
	}
	if err := resolveOrPublishFormalConfig(t.Context(), repository, second, secondMutation); err == nil ||
		!strings.Contains(err.Error(), "rolled_back") {
		t.Fatalf("rolled-back config recovery error = %v", err)
	}
}

func TestPublishFormalAgentBootstrapDoesNotRequireSourceExecution(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(formalreview.LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	subject := application.AgentPlanningSubject{
		TenantID: "local", OrganizationID: "local", WorkspaceID: "local", RepositoryID: "quick-fixture",
	}
	storePath, configState := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	first, err := publishFormalAgentBootstrap(t.Context(), subject, bootstrap,
		storePath, configState, "quick-frozen", at)
	if err != nil {
		t.Fatal(err)
	}
	frozenBytes, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	var frozen formalreview.LocalPiBootstrap
	if err := json.Unmarshal(frozenBytes, &frozen); err != nil {
		t.Fatal(err)
	}
	second, err := publishFormalAgentBootstrap(t.Context(), subject, frozen,
		storePath, configState, "quick-frozen", at)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.SourceRunID != "" ||
		len(first.Components) == 0 || first.ConfigID != bootstrap.Revision.ID {
		t.Fatalf("bootstrap output was not exact: first=%+v second=%+v", first, second)
	}
	store, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	history, err := runs.History(0)
	if err != nil || len(history) != 0 {
		t.Fatalf("bootstrap started source execution: history=%+v error=%v", history, err)
	}
}

func newBootstrapConfigFixture(t *testing.T) (*local.Store, *configrepo.Repository, reviewconfig.Revision) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := configrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := configdefaults.Revision(configdefaults.Options{
		ID: "quick-test-config", Revision: "1", MaxFiles: 100, MaxPatchBytes: 1 << 20,
		MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxAttempts: 1,
	}, workflow.DefaultReviewDefinition())
	if err != nil {
		t.Fatal(err)
	}
	return store, repository, revision
}

func bootstrapConfigTestMutation(key string, at time.Time) func(string, string) configrepo.Mutation {
	return func(suffix, audit string) configrepo.Mutation {
		return configrepo.Mutation{
			IdempotencyKey: key + "-" + suffix, Actor: "argus-local-pi-bootstrap", Audit: audit, At: at,
		}
	}
}

func createBootstrapConfigPrefix(
	t *testing.T,
	repository *configrepo.Repository,
	revision reviewconfig.Revision,
	mutation func(string, string) configrepo.Mutation,
	steps int,
) {
	t.Helper()
	if steps >= 1 {
		if _, err := repository.Create(t.Context(), revision,
			mutation("create", "create formal local Pi config")); err != nil {
			t.Fatal(err)
		}
	}
	if steps >= 2 {
		if _, err := repository.ValidateRevision(t.Context(), revision.ID, revision.Revision,
			mutation("validate", "validate formal local Pi config")); err != nil {
			t.Fatal(err)
		}
	}
	if steps >= 3 {
		if _, err := repository.Publish(t.Context(), revision.ID, revision.Revision,
			configrepo.Rollout{Percentage: 100}, mutation("publish", "publish formal local Pi config")); err != nil {
			t.Fatal(err)
		}
	}
}
