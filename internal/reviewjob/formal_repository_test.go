package reviewjob

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/store/local"
)

func TestCreateFormalConcurrentExactCallsIgnoreUnpersistedPublications(t *testing.T) {
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
	options := formalreview.LocalPiBootstrapOptions{
		NodePath: node, WorkerScript: worker,
		ProviderProfile: "deepseek-anthropic-env", Model: "deepseek-chat",
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(bootstrap.Publications) == 0 {
		t.Fatal("fixture needs live publication templates")
	}
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := configrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	configMutation := func(key string, second int) configrepo.Mutation {
		value := testMutation(key, second)
		return configrepo.Mutation{IdempotencyKey: value.IdempotencyKey, Actor: value.Actor, Audit: value.Audit, At: value.At}
	}
	if _, err := configs.Create(t.Context(), bootstrap.Revision, configMutation("create-formal", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.ValidateRevision(t.Context(), bootstrap.Revision.ID, bootstrap.Revision.Revision,
		configMutation("validate-formal", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.Publish(t.Context(), bootstrap.Revision.ID, bootstrap.Revision.Revision,
		configrepo.Rollout{Percentage: 100}, configMutation("publish-formal", 3)); err != nil {
		t.Fatal(err)
	}
	mutation := testMutation("concurrent-formal", 10)
	request := Request{
		SchemaVersion: RequestSchemaVersion, ExecutionProfile: FormalPiExecutionProfile,
		SourceRunID: "source-run", ExecutionTimeoutSeconds: 3600,
	}
	bundle, receipt, err := configs.ResolvePublishedWithReceipt(t.Context(), reviewconfig.ResolutionContext{
		TenantID: "local", OrganizationID: "local", RepositoryID: "test-repository",
		InvocationID: formalreview.FormalRunID(request.SourceRunID, mutation.IdempotencyKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := FormalRuntimeBinding{
		Options: options, Bootstrap: bootstrap,
		Pricing: piexecution.PricingCeiling{InputMicrosPerMillionTokens: 1, OutputMicrosPerMillionTokens: 1, MaximumBytesPerInputToken: 4},
	}
	type result struct {
		submission Submission
		command    Command
		err        error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			submission, command, err := repository.CreateFormal(t.Context(), request, mutation, bundle, receipt, runtime)
			results <- result{submission: submission, command: command, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	for _, value := range []result{first, second} {
		if value.err != nil {
			t.Fatalf("concurrent exact formal submission: %v", value.err)
		}
		if value.command.FormalRuntime == nil || len(value.command.FormalRuntime.Bootstrap.Publications) != 0 {
			t.Fatal("returned command did not match its persisted runtime shape")
		}
	}
	if first.submission.CommandRef != second.submission.CommandRef ||
		first.submission.RunID != second.submission.RunID ||
		len(runtime.Bootstrap.Publications) != len(bootstrap.Publications) {
		t.Fatal("exact submission changed identity or mutated caller-owned runtime")
	}
	changed := runtime
	changed.Options.Model = "another-model"
	if _, _, err := repository.CreateFormal(t.Context(), request, mutation, bundle, receipt, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed persisted runtime error = %v, want conflict", err)
	}
}
