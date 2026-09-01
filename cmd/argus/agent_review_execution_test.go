package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/agentshadow"
)

func TestAgentReviewExecutionListEmptyStore(t *testing.T) {
	storePath := t.TempDir()
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"agent-review", "execution", "list",
		"--store", storePath,
		"--start", "2026-08-20T00:00:00Z",
		"--end", "2026-08-21T00:00:00Z",
		"--json",
	}, &output); err != nil {
		t.Fatalf("execution list error = %v", err)
	}
	var result agentExecutionListOutput
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode execution list: %v\n%s", err, output.String())
	}
	if result.Executions == nil || len(result.Executions) != 0 || result.StorePath != storePath {
		t.Fatalf("execution list = %+v", result)
	}
}

func TestAgentReviewExecutionTimeRequiresUTC(t *testing.T) {
	_, err := parseAgentExecutionTime("start", "2026-08-20T08:00:00+08:00")
	if err == nil {
		t.Fatal("non-UTC execution time unexpectedly accepted")
	}
	value, err := parseAgentExecutionTime("start", "2026-08-20T00:00:00Z")
	if err != nil || value.Location().String() != "UTC" {
		t.Fatalf("UTC execution time = %v, %v", value, err)
	}
}

func TestAgentReviewExecutionReconcileOutputDistinguishesOutcomes(t *testing.T) {
	observedAt := time.Date(2026, 8, 24, 2, 3, 4, 0, time.UTC)
	tests := []struct {
		name           string
		status         agentshadow.ExecutionStatus
		reconciled     bool
		wantResolution agentExecutionReconcileResolution
	}{
		{
			name:           "committed result reconciled",
			status:         agentshadow.ExecutionStatusSucceeded,
			reconciled:     true,
			wantResolution: agentExecutionReconciledCommittedResult,
		},
		{
			name:           "already terminal",
			status:         agentshadow.ExecutionStatusFailed,
			wantResolution: agentExecutionAlreadyTerminal,
		},
		{
			name:           "no committed result",
			status:         agentshadow.ExecutionStatusUnknownOutcome,
			wantResolution: agentExecutionNoCommittedResult,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			execution := agentshadow.ExecutionAttempt{
				Status:      test.status,
				ExecutionID: "execution-fixture",
				ObservedAt:  observedAt,
			}
			var output bytes.Buffer
			if err := writeAgentExecutionReconcile(
				&output,
				true,
				"/tmp/argus-store",
				execution,
				test.reconciled,
			); err != nil {
				t.Fatal(err)
			}
			var decoded agentExecutionReconcileOutput
			decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&decoded); err != nil {
				t.Fatalf("decode reconcile output: %v\n%s", err, output.String())
			}
			if decoded.Reconciled != test.reconciled ||
				decoded.Resolution != test.wantResolution ||
				decoded.Execution.Status != test.status ||
				decoded.StorePath != "/tmp/argus-store" {
				t.Fatalf("reconcile output = %+v", decoded)
			}

			output.Reset()
			if err := writeAgentExecutionReconcile(
				&output,
				false,
				"/tmp/argus-store",
				execution,
				test.reconciled,
			); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"reconciled=" + stringValue(test.reconciled),
				"resolution=" + string(test.wantResolution),
				"status=" + string(test.status),
			} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("human reconcile output %q does not contain %q", output.String(), want)
				}
			}
		})
	}
}

func TestAgentReviewExecutionReconcileRequiresFlagsAndWiresService(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "store")
	err := runWithIO(t.Context(), []string{
		"agent-review", "execution", "reconcile", "--store", storePath,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--execution is required") {
		t.Fatalf("missing execution error = %v", err)
	}
	err = runWithIO(t.Context(), []string{
		"agent-review", "execution", "reconcile", "--execution", "execution-missing",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--store is required") {
		t.Fatalf("missing store error = %v", err)
	}
	err = runWithIO(t.Context(), []string{
		"agent-review", "execution", "reconcile",
		"--store", storePath,
		"--execution", "execution-missing",
	}, &bytes.Buffer{})
	if !errors.Is(err, agentshadow.ErrNotFound) {
		t.Fatalf("reconcile service error = %v, want ErrNotFound", err)
	}
}

func TestAgentReviewExecutionReconcileNestedHelp(t *testing.T) {
	var output bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"agent-review", "execution", "reconcile", "--help",
	}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "execution reconcile --store") ||
		!strings.Contains(output.String(), "--execution <id>") {
		t.Fatalf("execution reconcile help = %q", output.String())
	}
}

func TestAgentReviewExecutionReconcileRejectsInconsistentOutput(t *testing.T) {
	err := writeAgentExecutionReconcile(
		&bytes.Buffer{},
		false,
		"/tmp/argus-store",
		agentshadow.ExecutionAttempt{Status: agentshadow.ExecutionStatusUnknownOutcome},
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "must be succeeded") {
		t.Fatalf("inconsistent reconcile output error = %v", err)
	}
}

func stringValue(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
