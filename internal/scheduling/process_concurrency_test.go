package scheduling

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

type processRequest struct {
	Operation string   `json:"operation"`
	Round     int      `json:"round"`
	Dispatch  Dispatch `json:"dispatch"`
}

type processResult struct {
	Outcome  string   `json:"outcome"`
	Error    string   `json:"error,omitempty"`
	Dispatch Dispatch `json:"dispatch"`
}

type schedulingProcess struct {
	input  io.WriteCloser
	output *json.Decoder
}

func TestSchedulingMutationsRemainConsistentAcrossTwoProcesses(t *testing.T) {
	// testing cancels t.Context before cleanup callbacks; keep child shutdown
	// alive until their stdin is closed and the explicit Wait has completed.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workers := make([]schedulingProcess, 2)
	for index := range workers {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSchedulingProcessWorker$")
		cmd.Env = append(os.Environ(), "ARGUS_SCHEDULING_PROCESS_STORE="+store.Root(), "ARGUS_SCHEDULING_PROCESS_INDEX="+strconv.Itoa(index))
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = input.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("scheduling child failed: %v %s", err, stderr.String())
			}
		})
		workers[index] = schedulingProcess{input: input, output: json.NewDecoder(output)}
		var ready processResult
		if err := workers[index].output.Decode(&ready); err != nil || ready.Outcome != "ready" {
			t.Fatalf("scheduling child not ready: %+v %v %s", ready, err, stderr.String())
		}
	}
	parallel := func(request processRequest) []processResult {
		t.Helper()
		for _, worker := range workers {
			if err := json.NewEncoder(worker.input).Encode(request); err != nil {
				t.Fatal(err)
			}
		}
		results := make([]processResult, len(workers))
		for index, worker := range workers {
			if err := worker.output.Decode(&results[index]); err != nil {
				t.Fatalf("%s response: %v", request.Operation, err)
			}
			if results[index].Error != "" {
				t.Fatalf("%s failed: %s", request.Operation, results[index].Error)
			}
		}
		return results
	}
	for round := 0; round < 8; round++ {
		parallel(processRequest{Operation: "submit", Round: round})
		for _, phase := range []string{"claim", "reconcile", "claim"} {
			results := parallel(processRequest{Operation: phase, Round: round})
			if phase == "claim" {
				claimed := 0
				for _, result := range results {
					if result.Outcome == "claimed" {
						claimed++
					}
				}
				if claimed != 1 {
					t.Fatalf("round %d exceeded or failed class pool capacity: %+v", round, results)
				}
			}
		}
		repository, err := NewRepository(store, testPolicy())
		if err != nil {
			t.Fatalf("round %d ledger replay: %v", round, err)
		}
		records, err := repository.List()
		if err != nil {
			t.Fatal(err)
		}
		active := 0
		var dispatch Dispatch
		for _, record := range records {
			if record.State == StateLeased {
				active++
				dispatch = Dispatch{Spec: record.Spec, Lease: *record.ActiveLease}
			}
		}
		if active != 1 {
			t.Fatalf("round %d active=%d, want one", round, active)
		}
		parallel(processRequest{Operation: "heartbeat", Round: round, Dispatch: dispatch})
		results := parallel(processRequest{Operation: "complete", Round: round, Dispatch: dispatch})
		accepted := 0
		for _, result := range results {
			if result.Outcome == "completed" {
				accepted++
			}
		}
		if accepted != 1 {
			t.Fatalf("round %d duplicate completion accepted: %+v", round, results)
		}
		parallel(processRequest{Operation: "cancel", Round: round})
		if _, err := NewRepository(store, testPolicy()); err != nil {
			t.Fatalf("terminal ledger replay: %v", err)
		}
	}
}

// This helper runs in actual independent processes, so schedulingLocks cannot
// accidentally serialize the test's workers or hide stale-projection races.
func TestSchedulingProcessWorker(t *testing.T) {
	root := os.Getenv("ARGUS_SCHEDULING_PROCESS_STORE")
	if root == "" {
		t.Skip("subprocess helper")
	}
	index := os.Getenv("ARGUS_SCHEDULING_PROCESS_INDEX")
	store, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	if err := encoder.Encode(processResult{Outcome: "ready"}); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(os.Stdin)
	claimCounts := make(map[int]int)
	for scanner.Scan() {
		var request processRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		base := schedulingEpoch.Add(time.Duration(request.Round) * time.Hour)
		id := fmt.Sprintf("process-%d-%s", request.Round, index)
		result := processResult{}
		var err error
		switch request.Operation {
		case "submit":
			spec := testWorkload(id, "run-"+id, "tenant-"+index, ClassInteractive, base)
			_, err = repository.Submit(t.Context(), spec, testMutation("submit-"+id, base))
			result.Outcome = "submitted"
		case "claim":
			claimCounts[request.Round]++
			count := claimCounts[request.Round]
			at := base.Add(time.Second)
			if count > 1 {
				at = base.Add(11 * time.Minute)
			}
			result.Dispatch, err = repository.Claim(t.Context(), ClaimRequest{
				IdempotencyKey: fmt.Sprintf("claim-%s-%d", id, count), WorkerID: "worker-" + index,
				WorkloadID: id, AllowedWorkloadIDs: []string{id}, SupportedClasses: []WorkloadClass{ClassInteractive}, At: at,
			})
			result.Outcome = "claimed"
			if errors.Is(err, ErrNoWork) {
				result.Outcome = "capacity_busy"
				err = nil
			}
		case "reconcile":
			_, err = repository.ReconcileWorkloads(t.Context(), []string{
				fmt.Sprintf("process-%d-0", request.Round), fmt.Sprintf("process-%d-1", request.Round),
			}, testMutation("reconcile-"+id, base.Add(11*time.Minute)))
			result.Outcome = "reconciled"
		case "heartbeat":
			lease := request.Dispatch.Lease
			_, err = repository.Heartbeat(t.Context(), Heartbeat{
				IdempotencyKey: "heartbeat-" + id, WorkloadID: lease.WorkloadID, LeaseID: lease.LeaseID,
				WorkerID: lease.WorkerID, Attempt: lease.Attempt, Generation: lease.Generation,
				FencingToken: lease.FencingToken, At: base.Add(12 * time.Minute),
			})
			result.Outcome = "heartbeat"
		case "complete":
			_, err = repository.Complete(t.Context(), callbackFor(request.Dispatch, "complete-"+id, CallbackSucceeded, base.Add(13*time.Minute)))
			result.Outcome = "completed"
			if errors.Is(err, ErrFenced) {
				result.Outcome = "fenced"
				err = nil
			}
		case "cancel":
			_, err = repository.CancelRun(t.Context(), "run-"+id, "finish concurrent fixture", testMutation("cancel-"+id, base.Add(14*time.Minute)))
			result.Outcome = "canceled"
		default:
			err = fmt.Errorf("unknown operation %s", request.Operation)
		}
		if err != nil {
			result.Error = err.Error()
		}
		if err := encoder.Encode(result); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
