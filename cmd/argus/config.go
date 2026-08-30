package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/store/local"
)

const (
	maxConfigRevisionBytes = int64(4 << 20)
	configUsage            = `usage:
  argus config create --state-dir <absolute-dir> --file <absolute-json> <mutation flags> [--json]
  argus config validate --state-dir <absolute-dir> --id <id> --revision <revision> <mutation flags> [--json]
  argus config publish --state-dir <absolute-dir> --id <id> --revision <revision> [--percentage <1-100>] [--seed <seed>] <mutation flags> [--json]
  argus config activate ...  (alias of publish)
  argus config advance --state-dir <absolute-dir> --id <id> --revision <revision> --percentage <1-100> --seed <seed> <mutation flags> [--json]
  argus config rollback --state-dir <absolute-dir> --id <id> --revision <revision> <mutation flags> [--json]
  argus config list --state-dir <absolute-dir> [--json]
  argus config show --state-dir <absolute-dir> --id <id> --revision <revision> [--json]

Mutation flags are required: --idempotency-key, --actor, --audit, and
--at <RFC3339>. Reusing a key requires byte-equivalent inputs and the same time.`
)

type configMutationFlags struct {
	stateDir     string
	id           string
	revision     string
	idempotency  string
	actor        string
	audit        string
	at           string
	json         bool
	percentage   int
	seed         string
	revisionFile string
}

type configMutationOutput struct {
	Record   configrepo.Record `json:"record"`
	StateDir string            `json:"state_dir"`
}

type configShowOutput struct {
	Record   configrepo.Record       `json:"record"`
	History  []configrepo.AuditEntry `json:"history"`
	StateDir string                  `json:"state_dir"`
}

func runConfig(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return errors.New(configUsage)
	}
	if arguments[0] == "help" || arguments[0] == "-h" || arguments[0] == "--help" {
		if len(arguments) != 1 {
			return errors.New(configUsage)
		}
		_, err := fmt.Fprintln(stdout, configUsage)
		return err
	}
	if len(arguments) == 2 && (arguments[1] == "-h" || arguments[1] == "--help") {
		_, err := fmt.Fprintln(stdout, configUsage)
		return err
	}
	switch arguments[0] {
	case "create":
		return runConfigCreate(ctx, arguments[1:], stdout)
	case "validate":
		return runConfigMutation(ctx, "validate", arguments[1:], stdout)
	case "publish", "activate":
		return runConfigMutation(ctx, "publish", arguments[1:], stdout)
	case "advance":
		return runConfigMutation(ctx, "advance", arguments[1:], stdout)
	case "rollback":
		return runConfigMutation(ctx, "rollback", arguments[1:], stdout)
	case "list":
		return runConfigList(arguments[1:], stdout)
	case "show":
		return runConfigShow(arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown config command %q\n%s", arguments[0], configUsage)
	}
}

func runConfigCreate(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	var options configMutationFlags
	flags := newFlagSet("config create")
	addConfigStateFlag(flags, &options)
	flags.StringVar(&options.revisionFile, "file", "", "absolute strict JSON revision file")
	addConfigMutationFlags(flags, &options)
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("config create flags: %w\n%s", err, configUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("config create accepts flags only")
	}
	stateDir, mutation, err := validateConfigMutationFlags(options)
	if err != nil {
		return err
	}
	if options.revisionFile == "" || !filepath.IsAbs(options.revisionFile) ||
		filepath.Clean(options.revisionFile) != options.revisionFile {
		return fmt.Errorf("--file must be a clean absolute path")
	}
	data, err := readRegularFileLimit(options.revisionFile, maxConfigRevisionBytes)
	if err != nil {
		return fmt.Errorf("read config revision: %w", err)
	}
	revision, err := reviewconfig.DecodeRevision(data)
	if err != nil {
		return fmt.Errorf("decode config revision: %w", err)
	}
	repository, canonicalState, err := openConfigRepository(stateDir)
	if err != nil {
		return err
	}
	record, err := repository.Create(ctx, revision, mutation)
	if err != nil {
		return err
	}
	return writeConfigMutationOutput(stdout, record, canonicalState, options.json)
}

func runConfigMutation(
	ctx context.Context,
	action string,
	arguments []string,
	stdout io.Writer,
) error {
	options := configMutationFlags{percentage: 100}
	flags := newFlagSet("config " + action)
	addConfigStateFlag(flags, &options)
	flags.StringVar(&options.id, "id", "", "config revision ID")
	flags.StringVar(&options.revision, "revision", "", "config revision")
	if action == "publish" || action == "advance" {
		flags.IntVar(&options.percentage, "percentage", options.percentage, "rollout percentage")
		flags.StringVar(&options.seed, "seed", "", "stable rollout seed")
	}
	addConfigMutationFlags(flags, &options)
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("config %s flags: %w\n%s", action, err, configUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("config %s accepts flags only", action)
	}
	stateDir, mutation, err := validateConfigMutationFlags(options)
	if err != nil {
		return err
	}
	if options.id == "" || options.revision == "" {
		return fmt.Errorf("--id and --revision are required")
	}
	repository, canonicalState, err := openConfigRepository(stateDir)
	if err != nil {
		return err
	}
	var record configrepo.Record
	switch action {
	case "validate":
		record, err = repository.ValidateRevision(
			ctx,
			options.id,
			options.revision,
			mutation,
		)
	case "publish":
		record, err = repository.Publish(
			ctx,
			options.id,
			options.revision,
			configrepo.Rollout{Percentage: options.percentage, Seed: options.seed},
			mutation,
		)
	case "advance":
		record, err = repository.AdvanceRollout(
			ctx,
			options.id,
			options.revision,
			configrepo.Rollout{Percentage: options.percentage, Seed: options.seed},
			mutation,
		)
	case "rollback":
		record, err = repository.Rollback(
			ctx,
			options.id,
			options.revision,
			mutation,
		)
	default:
		return fmt.Errorf("unsupported config mutation %q", action)
	}
	if err != nil {
		return err
	}
	return writeConfigMutationOutput(stdout, record, canonicalState, options.json)
}

func runConfigList(arguments []string, stdout io.Writer) error {
	var options configMutationFlags
	flags := newFlagSet("config list")
	addConfigStateFlag(flags, &options)
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("config list flags: %w\n%s", err, configUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("config list accepts flags only")
	}
	stateDir, err := validateExplicitConfigState(options.stateDir)
	if err != nil {
		return err
	}
	repository, canonicalState, err := openConfigRepository(stateDir)
	if err != nil {
		return err
	}
	records, err := repository.List()
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, struct {
			Records  []configrepo.Record `json:"records"`
			StateDir string              `json:"state_dir"`
		}{Records: records, StateDir: canonicalState})
	}
	if _, err := fmt.Fprintf(stdout, "State: %s\n", canonicalState); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(
			stdout,
			"%s@%s\t%s\t%s\n",
			record.Revision.ID,
			record.Revision.Revision,
			record.Status,
			record.SHA256,
		); err != nil {
			return err
		}
	}
	return nil
}

func runConfigShow(arguments []string, stdout io.Writer) error {
	var options configMutationFlags
	flags := newFlagSet("config show")
	addConfigStateFlag(flags, &options)
	flags.StringVar(&options.id, "id", "", "config revision ID")
	flags.StringVar(&options.revision, "revision", "", "config revision")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("config show flags: %w\n%s", err, configUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("config show accepts flags only")
	}
	stateDir, err := validateExplicitConfigState(options.stateDir)
	if err != nil {
		return err
	}
	if options.id == "" || options.revision == "" {
		return fmt.Errorf("--id and --revision are required")
	}
	repository, canonicalState, err := openConfigRepository(stateDir)
	if err != nil {
		return err
	}
	record, err := repository.Get(options.id, options.revision)
	if err != nil {
		return err
	}
	history, err := repository.History(options.id, options.revision)
	if err != nil {
		return err
	}
	output := configShowOutput{
		Record: record, History: history, StateDir: canonicalState,
	}
	if options.json {
		return writeJSON(stdout, output)
	}
	if _, err := fmt.Fprintf(
		stdout,
		"%s@%s %s %s\n",
		record.Revision.ID,
		record.Revision.Revision,
		record.Status,
		record.SHA256,
	); err != nil {
		return err
	}
	for _, entry := range history {
		if _, err := fmt.Fprintf(
			stdout,
			"%d\t%s\t%s\t%s\n",
			entry.Sequence,
			entry.Type,
			entry.At.UTC().Format(time.RFC3339Nano),
			entry.Actor,
		); err != nil {
			return err
		}
	}
	return nil
}

func addConfigStateFlag(flags *flag.FlagSet, options *configMutationFlags) {
	flags.StringVar(
		&options.stateDir,
		"state-dir",
		"",
		"required clean absolute configuration state directory",
	)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
}

func addConfigMutationFlags(flags *flag.FlagSet, options *configMutationFlags) {
	flags.StringVar(&options.idempotency, "idempotency-key", "", "stable command key")
	flags.StringVar(&options.actor, "actor", "", "audited actor ID")
	flags.StringVar(&options.audit, "audit", "", "audit reason")
	flags.StringVar(&options.at, "at", "", "event time in RFC3339")
}

func validateConfigMutationFlags(
	options configMutationFlags,
) (string, configrepo.Mutation, error) {
	stateDir, err := validateExplicitConfigState(options.stateDir)
	if err != nil {
		return "", configrepo.Mutation{}, err
	}
	if options.idempotency == "" || options.actor == "" ||
		options.audit == "" || options.at == "" {
		return "", configrepo.Mutation{},
			fmt.Errorf("--idempotency-key, --actor, --audit, and --at are required")
	}
	at, err := time.Parse(time.RFC3339Nano, options.at)
	if err != nil {
		return "", configrepo.Mutation{}, fmt.Errorf("--at must be RFC3339: %w", err)
	}
	return stateDir, configrepo.Mutation{
		IdempotencyKey: options.idempotency,
		Actor:          options.actor,
		Audit:          options.audit,
		At:             at.UTC(),
	}, nil
}

func validateExplicitConfigState(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("--state-dir is required; configuration commands have no implicit state directory")
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", fmt.Errorf("--state-dir must be a clean absolute path")
	}
	return value, nil
}

func openConfigRepository(
	stateDir string,
) (*configrepo.Repository, string, error) {
	stateDir, err := validateExplicitConfigState(stateDir)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(stateDir)
	if err != nil {
		return nil, "", fmt.Errorf("open config state %q: %w", stateDir, err)
	}
	repository, err := configrepo.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open config repository: %w", err)
	}
	return repository, store.Root(), nil
}

func readRegularFileLimit(path string, limit int64) ([]byte, error) {
	if limit < 1 {
		return nil, fmt.Errorf("input byte limit must be positive")
	}
	// A descriptor supplied to a CLI mutation must not be able to block the
	// process merely by being a FIFO. O_NONBLOCK is inert for regular files;
	// Stat below still validates the exact opened descriptor.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("input must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return data, nil
}

func writeConfigMutationOutput(
	stdout io.Writer,
	record configrepo.Record,
	stateDir string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, configMutationOutput{
			Record: record, StateDir: stateDir,
		})
	}
	_, err := fmt.Fprintf(
		stdout,
		"%s@%s %s %s\n",
		record.Revision.ID,
		record.Revision.Revision,
		record.Status,
		record.SHA256,
	)
	return err
}
