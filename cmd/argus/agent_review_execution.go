package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/abietic/argus/internal/agentshadow"
)

const agentReviewExecutionUsage = `usage:
  argus agent-review execution list --store <absolute-dir> --start <RFC3339-UTC> --end <RFC3339-UTC> [--json]
  argus agent-review execution show --store <absolute-dir> --execution <id> [--json]
  argus agent-review execution reconcile --store <absolute-dir> --execution <id> [--json]

Execution history is host-observed operational evidence. It includes succeeded,
failed, canceled, and unknown-outcome worker attempts; only succeeded attempts
can reference a committed shadow result manifest. Reconcile never reruns a
worker or provider: it only closes an unknown attempt when an exactly bound
committed result already exists.`

type agentExecutionListFlags struct {
	store string
	start string
	end   string
	json  bool
}

type agentExecutionShowFlags struct {
	store       string
	executionID string
	json        bool
}

type agentExecutionReconcileResolution string

const (
	agentExecutionReconciledCommittedResult agentExecutionReconcileResolution = "committed_result_reconciled"
	agentExecutionAlreadyTerminal           agentExecutionReconcileResolution = "already_terminal"
	agentExecutionNoCommittedResult         agentExecutionReconcileResolution = "no_committed_result"
)

type agentExecutionListOutput struct {
	Executions []agentshadow.ExecutionAttempt `json:"executions"`
	StorePath  string                         `json:"store_path"`
}

type agentExecutionShowOutput struct {
	Execution agentshadow.ExecutionAttempt `json:"execution"`
	StorePath string                       `json:"store_path"`
}

type agentExecutionReconcileOutput struct {
	Execution  agentshadow.ExecutionAttempt      `json:"execution"`
	Reconciled bool                              `json:"reconciled"`
	Resolution agentExecutionReconcileResolution `json:"resolution"`
	StorePath  string                            `json:"store_path"`
}

func runAgentReviewExecution(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s", agentReviewExecutionUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, agentReviewExecutionUsage)
		return err
	}
	switch arguments[0] {
	case "list":
		return runAgentExecutionList(ctx, arguments[1:], stdout)
	case "show":
		return runAgentExecutionShow(ctx, arguments[1:], stdout)
	case "reconcile":
		return runAgentExecutionReconcile(ctx, arguments[1:], stdout)
	default:
		return fmt.Errorf(
			"unknown agent-review execution action %q\n%s",
			arguments[0],
			agentReviewExecutionUsage,
		)
	}
}

func runAgentExecutionReconcile(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	var options agentExecutionShowFlags
	flags := newFlagSet("agent-review execution reconcile")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.executionID, "execution", "", "agent review execution ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return controlFlagError(
			"agent-review execution reconcile",
			err,
			agentReviewExecutionUsage,
		)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), agentReviewExecutionUsage)
	}
	if options.store == "" {
		return fmt.Errorf("--store is required")
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return err
	}
	if options.executionID == "" {
		return fmt.Errorf("--execution is required")
	}
	repository, err := openAgentShadowRepository(options.store)
	if err != nil {
		return err
	}
	service, err := agentshadow.NewService(repository)
	if err != nil {
		return err
	}
	execution, reconciled, err := service.ReconcileExecution(
		ctx,
		agentshadow.ExecutionReconcileRequest{
			Scope: agentshadow.Scope{
				TenantID:    agentshadow.LocalTenantID,
				WorkspaceID: agentshadow.LocalWorkspaceID,
			},
			ExecutionID: options.executionID,
		},
	)
	if err != nil {
		return err
	}
	return writeAgentExecutionReconcile(
		stdout,
		options.json,
		options.store,
		execution,
		reconciled,
	)
}

func writeAgentExecutionReconcile(
	stdout io.Writer,
	jsonOutput bool,
	storePath string,
	execution agentshadow.ExecutionAttempt,
	reconciled bool,
) error {
	resolution := agentExecutionAlreadyTerminal
	if reconciled {
		if execution.Status != agentshadow.ExecutionStatusSucceeded {
			return fmt.Errorf(
				"reconciled execution must be succeeded, got %q",
				execution.Status,
			)
		}
		resolution = agentExecutionReconciledCommittedResult
	} else if execution.Status == agentshadow.ExecutionStatusUnknownOutcome {
		resolution = agentExecutionNoCommittedResult
	} else if execution.Status != agentshadow.ExecutionStatusSucceeded &&
		execution.Status != agentshadow.ExecutionStatusFailed &&
		execution.Status != agentshadow.ExecutionStatusCanceled {
		return fmt.Errorf("unrecognized execution status %q", execution.Status)
	}
	if jsonOutput {
		return writeJSON(stdout, agentExecutionReconcileOutput{
			Execution:  execution,
			Reconciled: reconciled,
			Resolution: resolution,
			StorePath:  storePath,
		})
	}
	manifestID := "-"
	if execution.Manifest != nil {
		manifestID = execution.Manifest.ManifestID
	}
	hostDiagnosis := "-"
	if execution.HostFailure != nil {
		hostDiagnosis = string(execution.HostFailure.ReasonCode)
	}
	_, err := fmt.Fprintf(
		stdout,
		"agent execution reconcile reconciled=%t resolution=%s status=%s execution=%s manifest=%s host_diagnosis=%s observed_at=%s\n",
		reconciled,
		resolution,
		execution.Status,
		execution.ExecutionID,
		manifestID,
		hostDiagnosis,
		execution.ObservedAt.Format(time.RFC3339Nano),
	)
	return err
}

func runAgentExecutionList(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	var options agentExecutionListFlags
	flags := newFlagSet("agent-review execution list")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.start, "start", "", "inclusive RFC3339 UTC observation time")
	flags.StringVar(&options.end, "end", "", "exclusive RFC3339 UTC observation time")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return controlFlagError(
			"agent-review execution list",
			err,
			agentReviewExecutionUsage,
		)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), agentReviewExecutionUsage)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return err
	}
	start, err := parseAgentExecutionTime("start", options.start)
	if err != nil {
		return err
	}
	end, err := parseAgentExecutionTime("end", options.end)
	if err != nil {
		return err
	}
	repository, err := openAgentShadowRepository(options.store)
	if err != nil {
		return err
	}
	service, err := agentshadow.NewService(repository)
	if err != nil {
		return err
	}
	executions, err := service.ListExecutions(ctx, agentshadow.ExecutionListRequest{
		Scope: agentshadow.Scope{
			TenantID:    agentshadow.LocalTenantID,
			WorkspaceID: agentshadow.LocalWorkspaceID,
		},
		StartInclusive: start,
		EndExclusive:   end,
	})
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, agentExecutionListOutput{
			Executions: executions,
			StorePath:  options.store,
		})
	}
	for _, execution := range executions {
		if _, err := fmt.Fprintf(
			stdout,
			"%s\t%s\t%s\t%s\n",
			execution.ObservedAt.Format(time.RFC3339Nano),
			execution.Status,
			execution.ExecutionID,
			execution.SourceRunID,
		); err != nil {
			return err
		}
	}
	return nil
}

func runAgentExecutionShow(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	var options agentExecutionShowFlags
	flags := newFlagSet("agent-review execution show")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.executionID, "execution", "", "agent review execution ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return controlFlagError(
			"agent-review execution show",
			err,
			agentReviewExecutionUsage,
		)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), agentReviewExecutionUsage)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return err
	}
	if options.executionID == "" {
		return fmt.Errorf("--execution is required")
	}
	repository, err := openAgentShadowRepository(options.store)
	if err != nil {
		return err
	}
	service, err := agentshadow.NewService(repository)
	if err != nil {
		return err
	}
	execution, err := service.QueryExecution(ctx, agentshadow.ExecutionQueryRequest{
		Scope: agentshadow.Scope{
			TenantID:    agentshadow.LocalTenantID,
			WorkspaceID: agentshadow.LocalWorkspaceID,
		},
		ExecutionID: options.executionID,
	})
	if err != nil {
		return err
	}
	return writeAgentExecutionAttempt(stdout, options.json, options.store, execution)
}

func writeAgentExecutionAttempt(
	stdout io.Writer,
	jsonOutput bool,
	storePath string,
	execution agentshadow.ExecutionAttempt,
) error {
	if jsonOutput {
		return writeJSON(stdout, agentExecutionShowOutput{
			Execution: execution,
			StorePath: storePath,
		})
	}
	failureCode := "-"
	if execution.Failure != nil {
		failureCode = execution.Failure.Code
	}
	hostDiagnosis := "-"
	if execution.HostFailure != nil {
		hostDiagnosis = string(execution.HostFailure.ReasonCode)
	}
	manifestID := "-"
	if execution.Manifest != nil {
		manifestID = execution.Manifest.ManifestID
	}
	_, err := fmt.Fprintf(
		stdout,
		"agent execution %s execution=%s source_run=%s manifest=%s failure=%s host_diagnosis=%s observed_at=%s\n",
		execution.Status,
		execution.ExecutionID,
		execution.SourceRunID,
		manifestID,
		failureCode,
		hostDiagnosis,
		execution.ObservedAt.Format(time.RFC3339Nano),
	)
	return err
}

func parseAgentExecutionTime(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("--%s is required", name)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--%s must be RFC3339: %w", name, err)
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return time.Time{}, fmt.Errorf("--%s must use UTC", name)
	}
	return parsed.UTC(), nil
}
