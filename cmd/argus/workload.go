package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"argus.local/argus/internal/scheduling"
	"argus.local/argus/internal/store/local"
)

const workloadUsage = `usage:
  argus workload pressure --store <absolute-dir> --at <RFC3339-UTC> [--json]`

type workloadPressureFlags struct {
	store string
	at    string
	json  bool
}

type workloadPressureOutput struct {
	Snapshot  scheduling.PressureSnapshot `json:"snapshot"`
	StorePath string                      `json:"store_path"`
}

func runWorkload(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(workloadUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, workloadUsage)
		return err
	}
	if arguments[0] != "pressure" {
		return fmt.Errorf("unknown workload command %q\n%s", arguments[0], workloadUsage)
	}
	options, observedAt, err := parseWorkloadPressureFlags(arguments[1:])
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(options.store)
	if err != nil {
		return fmt.Errorf("inspect workload state %q: %w", options.store, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workload state %q must be a directory", options.store)
	}
	state, err := local.Open(options.store)
	if err != nil {
		return fmt.Errorf("open workload state %q: %w", options.store, err)
	}
	repository, err := scheduling.NewRepository(state, scheduling.DefaultLocalPolicy())
	if err != nil {
		return fmt.Errorf("open workload scheduler: %w", err)
	}
	snapshot, err := repository.Pressure(observedAt)
	if err != nil {
		return err
	}
	output := workloadPressureOutput{Snapshot: snapshot, StorePath: state.Root()}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"workloads pending=%d active=%d capacity=%s stale_pending=%d stale_active=%d sequence=%d store=%s\n",
		snapshot.Global.QueueDepth, snapshot.Global.ActiveDepth, snapshot.Global.State,
		snapshot.Stale.PendingRequiresReconcile, snapshot.Stale.ActiveRequiresReconcile,
		snapshot.LedgerSequence, state.Root(),
	)
	return err
}

func parseWorkloadPressureFlags(arguments []string) (workloadPressureFlags, time.Time, error) {
	var options workloadPressureFlags
	flags := newFlagSet("workload pressure")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.at, "at", "", "UTC observation timestamp")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return workloadPressureFlags{}, time.Time{}, controlFlagError("workload pressure", err, workloadUsage)
	}
	if flags.NArg() != 0 {
		return workloadPressureFlags{}, time.Time{}, controlFlagError(
			"workload pressure", fmt.Errorf("unexpected argument %q", flags.Arg(0)), workloadUsage,
		)
	}
	storePath, err := validateExplicitControlStore(options.store)
	if err != nil {
		return workloadPressureFlags{}, time.Time{}, err
	}
	if options.at == "" {
		return workloadPressureFlags{}, time.Time{}, fmt.Errorf("--at is required")
	}
	observedAt, err := parseDashboardTime("at", options.at)
	if err != nil {
		return workloadPressureFlags{}, time.Time{}, err
	}
	options.store = storePath
	return options, observedAt, nil
}
