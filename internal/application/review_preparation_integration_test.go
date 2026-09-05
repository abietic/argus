package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/contextprovider"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/reviewshard"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
)

var errPreparationCrash = errors.New("injected process exit before run.created")

func TestScopeReviewRestartsBeforeLifecycleWithoutRecapturingPreparedInput(t *testing.T) {
	for _, boundary := range []string{"before_plan", "after_plan", "after_snapshot"} {
		t.Run(boundary, func(t *testing.T) {
			repositoryPath := t.TempDir()
			git := func(arguments ...string) string {
				t.Helper()
				command := exec.Command("git", arguments...)
				command.Dir = repositoryPath
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", arguments, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git("init", "-q", "-b", "main")
			git("config", "user.name", "Argus Test")
			git("config", "user.email", "argus@example.invalid")
			git("config", "commit.gpgsign", "false")
			if err := os.WriteFile(filepath.Join(repositoryPath, "review.go"), []byte("package fixture\n// ARGUS_BUG: frozen source\nfunc Review() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-q", "-m", "source")
			revision := git("rev-parse", "HEAD")
			storePath := t.TempDir()
			store, err := local.Open(storePath)
			if err != nil {
				t.Fatal(err)
			}
			runs, err := runrepo.New(store)
			if err != nil {
				t.Fatal(err)
			}
			shards, err := reviewshard.NewRepository(store)
			if err != nil {
				t.Fatal(err)
			}
			source, err := gitadapter.New()
			if err != nil {
				t.Fatal(err)
			}
			contexts, err := contextprovider.NewLocalExecutor(source)
			if err != nil {
				t.Fatal(err)
			}
			runID := "run-before-lifecycle"
			dispatch := preparationDispatch(runID, 1)
			firstShards, err := reviewshard.NewScopeExecutor(shards, runs, dispatch, "prepare-worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			firstIDs := &preparationIDs{runID: runID, generation: 1}
			first, err := application.NewService(source, runs, application.ServiceOptions{
				IDs: firstIDs, BuildIdentity: "argus-preparation-test", DisableScheduling: true,
				ExecutionDispatch: &dispatch, ContextProviders: contexts,
				ScopeShards: &crashingPreparationShards{ScopeShardPort: firstShards, boundary: boundary},
				ExecutionAuthority: func(context.Context, scheduling.Dispatch) error {
					if boundary == "after_snapshot" && firstIDs.snapshotAssigned {
						return errPreparationCrash
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := application.ReviewRequest{
				RepositoryPath: repositoryPath, Mode: reviewcore.TargetModeScope, Revision: revision,
				Include: []string{"*.go"}, Exclude: []string{}, ContextProviderIDs: []string{"go_ast"},
			}
			if _, err := first.Review(t.Context(), request); !errors.Is(err, errPreparationCrash) {
				t.Fatalf("did not stop at %s: %v", boundary, err)
			}
			preparationRef, err := runs.ReviewPreparation(runID)
			if err != nil {
				t.Fatal(err)
			}
			var prepared struct {
				InputRef                   runmodel.ArtifactRef   `json:"input_ref"`
				ContextProviderReceiptRefs []runmodel.ArtifactRef `json:"context_provider_receipt_refs"`
			}
			// Decode the immutable evidence with the ordinary JSON decoder;
			// production loading separately enforces its complete strict schema.
			data, err := runs.ReadArtifact(preparationRef)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &prepared); err != nil {
				t.Fatal(err)
			}
			if len(prepared.ContextProviderReceiptRefs) != 1 {
				t.Fatal("configured context receipt was not frozen before planning")
			}
			events, err := runs.Events(runID)
			if err != nil || len(events) != 0 {
				t.Fatalf("crash unexpectedly admitted lifecycle: events=%v error=%v", events, err)
			}
			priorPlan, priorErr := shards.Get(runID)
			if boundary != "before_plan" && priorErr != nil {
				t.Fatal(priorErr)
			}
			// The restarted process opens independent repositories. Neither
			// source nor context ports are available; any call would panic.
			store, err = local.Open(storePath)
			if err != nil {
				t.Fatal(err)
			}
			runs, err = runrepo.New(store)
			if err != nil {
				t.Fatal(err)
			}
			shards, err = reviewshard.NewRepository(store)
			if err != nil {
				t.Fatal(err)
			}
			dispatch = preparationDispatch(runID, 2)
			secondShards, err := reviewshard.NewScopeExecutor(shards, runs, dispatch, "prepare-worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			second, err := application.NewService(unavailablePreparationSource{}, runs, application.ServiceOptions{
				IDs: &preparationIDs{runID: runID, generation: 2}, BuildIdentity: "argus-preparation-test",
				DisableScheduling: true, ExecutionDispatch: &dispatch, ScopeShards: secondShards,
				ContextProviders: unavailablePreparationContexts{},
			})
			if err != nil {
				t.Fatal(err)
			}
			changedRequest := request
			changedRequest.Include = []string{"**"}
			if _, err := second.Review(t.Context(), changedRequest); err == nil ||
				!strings.Contains(err.Error(), "prepared review differs") {
				t.Fatalf("changed prepared intent was admitted: %v", err)
			}
			outcome, err := second.Review(t.Context(), request)
			if err != nil || outcome.Run.Status != runmodel.RunStatusSucceeded {
				t.Fatalf("restart failed: outcome=%+v error=%v", outcome, err)
			}
			finalPlan, err := shards.Get(runID)
			if err != nil {
				t.Fatal(err)
			}
			if finalPlan.Manifest.ReviewInputRef != prepared.InputRef ||
				priorErr == nil && finalPlan.ManifestRef != priorPlan.ManifestRef {
				t.Fatal("restart changed the input or already-published scope manifest")
			}
			snapshot, err := runs.ExecutionSnapshotForRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(snapshot.ContextProviderReceiptRefs, prepared.ContextProviderReceiptRefs) {
				t.Fatal("restart did not reuse frozen context provider receipts")
			}
		})
	}
}

type crashingPreparationShards struct {
	application.ScopeShardPort
	boundary string
}

func (port *crashingPreparationShards) PlanScope(ctx context.Context, request application.ScopeShardPlanRequest) (application.ScopeShardPlan, error) {
	if port.boundary == "before_plan" {
		return application.ScopeShardPlan{}, errPreparationCrash
	}
	plan, err := port.ScopeShardPort.PlanScope(ctx, request)
	if err == nil && port.boundary == "after_plan" {
		return application.ScopeShardPlan{}, errPreparationCrash
	}
	return plan, err
}

type unavailablePreparationSource struct{ application.TargetSource }
type unavailablePreparationContexts struct {
	application.ContextProviderExecutor
}

type preparationIDs struct {
	runID                string
	generation, sequence int
	snapshotAssigned     bool
}

func (ids *preparationIDs) New(prefix string) (string, error) {
	if prefix == "run" {
		return ids.runID, nil
	}
	ids.sequence++
	if prefix == "snapshot" {
		ids.snapshotAssigned = true
	}
	return fmt.Sprintf("%s-prepared-%d-%d", prefix, ids.generation, ids.sequence), nil
}

func preparationDispatch(runID string, generation int) scheduling.Dispatch {
	at := time.Now().UTC()
	return scheduling.Dispatch{
		Spec: scheduling.WorkloadSpec{
			SchemaVersion: scheduling.WorkloadSchemaVersion, WorkloadID: runID + "-workload",
			RunID: runID, TenantID: "local", Class: scheduling.ClassFullScan,
			InputRef:    "artifact://local/sha256/" + strings.Repeat("a", 64),
			SubmittedAt: at, ExecutionDeadline: at.Add(time.Hour),
		},
		Lease: scheduling.DispatchLease{
			LeaseID: fmt.Sprintf("lease-prepared-%d", generation), WorkloadID: runID + "-workload",
			WorkerID: "prepare-worker", Attempt: generation, Generation: generation, FencingToken: uint64(generation),
			AcquiredAt: at, LastHeartbeat: at, ExpiresAt: at.Add(time.Minute),
		},
	}
}
