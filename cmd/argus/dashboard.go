package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"argus.local/argus/internal/analytics"
	"argus.local/argus/internal/analyticsadapter"
	"argus.local/argus/internal/evaluation"
	feedbackdomain "argus.local/argus/internal/feedback"
	"argus.local/argus/internal/findinglineage"
	"argus.local/argus/internal/publication"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

const dashboardUsage = `usage:
  argus dashboard rebuild --store <absolute-dir> --snapshot <id> --tenant <id> --organization <id> --repository <id> --start <RFC3339> --end <RFC3339> --built-at <RFC3339> [--group-by <dimension>...] [--json]
  argus dashboard show --store <absolute-dir> --snapshot <id> [--json]
  argus dashboard export --store <absolute-dir> --snapshot <id> --target <dashboard|facts> --format <json|csv|parquet> --output-dir <absolute-new-dir> [--json]

Dashboard query/export read immutable projection snapshots only. Rebuild is the
only command that scans authoritative run, evaluation, publication, and
feedback/outcome ledgers. Evaluation usage remains diagnostic worker self-report.
Parquet export is available for the facts target and emits nine verified,
strongly typed tables. JSON/CSV manifests continue to report parquet=false.`

var errDashboardPublishOutcomeUnknown = errors.New(
	"dashboard export publish outcome is unknown",
)

type dashboardRebuildFlags struct {
	store        string
	snapshot     string
	tenant       string
	organization string
	repository   string
	start        string
	end          string
	builtAt      string
	groupBy      stringList
	json         bool
}

type dashboardShowFlags struct {
	store    string
	snapshot string
	json     bool
}

type dashboardExportFlags struct {
	store     string
	snapshot  string
	target    string
	format    string
	outputDir string
	json      bool
}

type dashboardSnapshotOutput struct {
	Snapshot         analyticsadapter.ProjectionSnapshot `json:"snapshot"`
	StorePath        string                              `json:"store_path"`
	ParquetSupported bool                                `json:"parquet_supported"`
}

type dashboardExportOutput struct {
	SnapshotID       string                        `json:"snapshot_id"`
	Target           analyticsadapter.ExportTarget `json:"target"`
	OutputDirectory  string                        `json:"output_directory"`
	Manifest         analytics.ExportManifest      `json:"manifest"`
	ParquetSupported bool                          `json:"parquet_supported"`
}

func runDashboard(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return errors.New(dashboardUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, dashboardUsage)
		return err
	}
	switch arguments[0] {
	case "rebuild":
		return runDashboardRebuild(ctx, arguments[1:], stdout)
	case "show":
		return runDashboardShow(arguments[1:], stdout)
	case "export":
		return runDashboardExport(arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown dashboard command %q\n%s", arguments[0], dashboardUsage)
	}
}

func runDashboardRebuild(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	options, err := parseDashboardRebuildFlags(arguments)
	if err != nil {
		return err
	}
	start, err := parseDashboardTime("start", options.start)
	if err != nil {
		return err
	}
	end, err := parseDashboardTime("end", options.end)
	if err != nil {
		return err
	}
	builtAt, err := parseDashboardTime("built-at", options.builtAt)
	if err != nil {
		return err
	}
	groupBy := make([]analytics.DimensionName, len(options.groupBy))
	for index, dimension := range options.groupBy {
		groupBy[index] = analytics.DimensionName(dimension)
	}
	adapter, storePath, err := openDashboardRebuilder(options.store)
	if err != nil {
		return err
	}
	snapshot, err := adapter.Rebuild(ctx, analyticsadapter.RebuildRequest{
		SnapshotID: options.snapshot,
		Scope: analyticsadapter.Scope{
			TenantID:       options.tenant,
			OrganizationID: options.organization,
			RepositoryID:   options.repository,
		},
		Window: analytics.TimeWindow{
			StartInclusive: start,
			EndExclusive:   end,
		},
		GroupBy: groupBy,
		BuiltAt: builtAt,
	})
	if err != nil {
		return err
	}
	return writeDashboardSnapshot(stdout, snapshot, storePath, options.json)
}

func runDashboardShow(arguments []string, stdout io.Writer) error {
	options, err := parseDashboardShowFlags(arguments)
	if err != nil {
		return err
	}
	adapter, storePath, err := openDashboardQuery(options.store)
	if err != nil {
		return err
	}
	snapshot, err := adapter.Query(options.snapshot)
	if err != nil {
		return err
	}
	return writeDashboardSnapshot(stdout, snapshot, storePath, options.json)
}

func runDashboardExport(arguments []string, stdout io.Writer) error {
	options, err := parseDashboardExportFlags(arguments)
	if err != nil {
		return err
	}
	target, err := parseDashboardExportTarget(options.target)
	if err != nil {
		return err
	}
	format, err := parseDashboardExportFormat(options.format)
	if err != nil {
		return err
	}
	adapter, _, err := openDashboardQuery(options.store)
	if err != nil {
		return err
	}
	bundle, err := adapter.Export(options.snapshot, target, format)
	if err != nil {
		return err
	}
	if err := persistDashboardExport(options.outputDir, bundle); err != nil {
		return err
	}
	output := dashboardExportOutput{
		SnapshotID:       options.snapshot,
		Target:           target,
		OutputDirectory:  options.outputDir,
		Manifest:         bundle.Manifest,
		ParquetSupported: true,
	}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(
		stdout,
		"snapshot=%s target=%s format=%s files=%d output=%s parquet_supported=true limitations=%s\n",
		output.SnapshotID,
		output.Target,
		output.Manifest.Format,
		len(output.Manifest.Files),
		output.OutputDirectory,
		strings.Join(output.Manifest.Limitations, ","),
	)
	return err
}

func parseDashboardRebuildFlags(
	arguments []string,
) (dashboardRebuildFlags, error) {
	var options dashboardRebuildFlags
	flags := newFlagSet("dashboard rebuild")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.snapshot, "snapshot", "", "immutable projection snapshot ID")
	flags.StringVar(&options.tenant, "tenant", "", "tenant ID")
	flags.StringVar(&options.organization, "organization", "", "organization ID")
	flags.StringVar(&options.repository, "repository", "", "repository ID")
	flags.StringVar(&options.start, "start", "", "closed window start in RFC3339")
	flags.StringVar(&options.end, "end", "", "closed window end in RFC3339")
	flags.StringVar(&options.builtAt, "built-at", "", "deterministic build time in RFC3339")
	flags.Var(&options.groupBy, "group-by", "governed dimension; repeatable")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return dashboardRebuildFlags{}, controlFlagError(
			"dashboard rebuild", err, dashboardUsage,
		)
	}
	if flags.NArg() != 0 {
		return dashboardRebuildFlags{}, controlFlagError(
			"dashboard rebuild",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			dashboardUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return dashboardRebuildFlags{}, err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"snapshot", options.snapshot},
		{"tenant", options.tenant},
		{"organization", options.organization},
		{"repository", options.repository},
		{"start", options.start},
		{"end", options.end},
		{"built-at", options.builtAt},
	} {
		if field.value == "" {
			return dashboardRebuildFlags{}, fmt.Errorf("--%s is required", field.name)
		}
	}
	if options.groupBy == nil {
		options.groupBy = stringList{}
	}
	return options, nil
}

func parseDashboardShowFlags(arguments []string) (dashboardShowFlags, error) {
	var options dashboardShowFlags
	flags := newFlagSet("dashboard show")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.snapshot, "snapshot", "", "immutable projection snapshot ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return dashboardShowFlags{}, controlFlagError("dashboard show", err, dashboardUsage)
	}
	if flags.NArg() != 0 {
		return dashboardShowFlags{}, controlFlagError(
			"dashboard show",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			dashboardUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return dashboardShowFlags{}, err
	}
	if options.snapshot == "" {
		return dashboardShowFlags{}, fmt.Errorf("--snapshot is required")
	}
	return options, nil
}

func parseDashboardExportFlags(arguments []string) (dashboardExportFlags, error) {
	var options dashboardExportFlags
	flags := newFlagSet("dashboard export")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.snapshot, "snapshot", "", "immutable projection snapshot ID")
	flags.StringVar(&options.target, "target", "", "dashboard or facts")
	flags.StringVar(&options.format, "format", "", "json, csv, or parquet")
	flags.StringVar(
		&options.outputDir,
		"output-dir",
		"",
		"clean absolute path to a new export directory",
	)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return dashboardExportFlags{}, controlFlagError(
			"dashboard export", err, dashboardUsage,
		)
	}
	if flags.NArg() != 0 {
		return dashboardExportFlags{}, controlFlagError(
			"dashboard export",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			dashboardUsage,
		)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return dashboardExportFlags{}, err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"snapshot", options.snapshot},
		{"target", options.target},
		{"format", options.format},
		{"output-dir", options.outputDir},
	} {
		if field.value == "" {
			return dashboardExportFlags{}, fmt.Errorf("--%s is required", field.name)
		}
	}
	if !filepath.IsAbs(options.outputDir) ||
		filepath.Clean(options.outputDir) != options.outputDir {
		return dashboardExportFlags{}, fmt.Errorf("--output-dir must be a clean absolute path")
	}
	return options, nil
}

func parseDashboardTime(name, value string) (time.Time, error) {
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

func parseDashboardExportTarget(value string) (analyticsadapter.ExportTarget, error) {
	switch analyticsadapter.ExportTarget(value) {
	case analyticsadapter.ExportDashboard, analyticsadapter.ExportFacts:
		return analyticsadapter.ExportTarget(value), nil
	default:
		return "", fmt.Errorf("--target must be dashboard or facts")
	}
}

func parseDashboardExportFormat(value string) (string, error) {
	switch value {
	case "json", analytics.CanonicalJSONExportFormat:
		return analytics.CanonicalJSONExportFormat, nil
	case "csv", analytics.CanonicalCSVExportFormat:
		return analytics.CanonicalCSVExportFormat, nil
	case "parquet":
		return analytics.CanonicalParquetExportFormat, nil
	default:
		return "", fmt.Errorf("--format must be json, csv, or parquet")
	}
}

func openDashboardRebuilder(
	requestedStore string,
) (*analyticsadapter.Adapter, string, error) {
	storePath, err := validateExplicitControlStore(requestedStore)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", fmt.Errorf("open dashboard state %q: %w", storePath, err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open authoritative run repository: %w", err)
	}
	ledger, err := feedbackdomain.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open authoritative feedback/outcome repository: %w", err)
	}
	publications, err := publication.NewRepository(store)
	if err != nil {
		return nil, "", fmt.Errorf("open authoritative publication repository: %w", err)
	}
	evaluations, err := evaluation.New(store)
	if err != nil {
		return nil, "", fmt.Errorf("open authoritative evaluation repository: %w", err)
	}
	evaluationSource, err := analyticsadapter.NewEvaluationProjectionSource(
		evaluations,
		runs,
		evaluation.Access{
			Actor: "local-dashboard-projection",
			Roles: []evaluation.Role{
				evaluation.RoleDatasetCurator,
				evaluation.RoleHoldoutMaintainer,
			},
		},
	)
	if err != nil {
		return nil, "", fmt.Errorf("initialize evaluation projection source: %w", err)
	}
	lineageRepository, err := findinglineage.New(store, runs, nil)
	if err != nil {
		return nil, "", fmt.Errorf("open authoritative finding lineage repository: %w", err)
	}
	lineageSource, err := analyticsadapter.NewFindingLineageProjectionSource(lineageRepository, runs)
	if err != nil {
		return nil, "", fmt.Errorf("initialize finding lineage projection source: %w", err)
	}
	adapter, err := analyticsadapter.NewWithFindingLineages(runs, ledger, store, evaluationSource, lineageSource, publications)
	if err != nil {
		return nil, "", fmt.Errorf("initialize dashboard rebuild adapter: %w", err)
	}
	return adapter, store.Root(), nil
}

func openDashboardQuery(
	requestedStore string,
) (*analyticsadapter.Adapter, string, error) {
	storePath, err := validateExplicitControlStore(requestedStore)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", fmt.Errorf("open dashboard state %q: %w", storePath, err)
	}
	adapter, err := analyticsadapter.Open(store)
	if err != nil {
		return nil, "", fmt.Errorf("open dashboard projection adapter: %w", err)
	}
	return adapter, store.Root(), nil
}

func writeDashboardSnapshot(
	stdout io.Writer,
	snapshot analyticsadapter.ProjectionSnapshot,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, dashboardSnapshotOutput{
			Snapshot: snapshot, StorePath: storePath, ParquetSupported: true,
		})
	}
	_, err := fmt.Fprintf(
		stdout,
		"snapshot=%s runs=%d findings=%d tiles=%d completeness=%s store=%s parquet_supported=true\n",
		snapshot.SnapshotID,
		len(snapshot.Facts.ReviewRuns),
		len(snapshot.Facts.Findings),
		len(snapshot.Dashboard.Tiles),
		snapshot.Facts.Completeness,
		storePath,
	)
	return err
}

func persistDashboardExport(
	outputDirectory string,
	bundle analytics.ExportBundle,
) error {
	if err := bundle.Validate(); err != nil {
		return fmt.Errorf("validate export bundle: %w", err)
	}
	if _, err := os.Lstat(outputDirectory); err == nil {
		return fmt.Errorf("export output directory already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect export output directory: %w", err)
	}
	parent := filepath.Dir(outputDirectory)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect export output parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return fmt.Errorf("export output parent must be a real directory")
	}
	stagingDirectory, err := os.MkdirTemp(parent, ".argus-dashboard-export-*")
	if err != nil {
		return fmt.Errorf("create export staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stagingDirectory)
		}
	}()
	for _, file := range bundle.Files {
		name := file.Manifest.Name
		if filepath.Base(name) != name || name == "." || name == ".." {
			return fmt.Errorf("export contains unsafe file name %q", name)
		}
		if err := writeExclusiveFile(
			filepath.Join(stagingDirectory, name),
			file.Data,
		); err != nil {
			return err
		}
	}
	manifest, err := json.MarshalIndent(bundle.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal export manifest: %w", err)
	}
	manifest = append(manifest, '\n')
	if err := writeExclusiveFile(
		filepath.Join(stagingDirectory, "manifest.json"),
		manifest,
	); err != nil {
		return err
	}
	if err := syncDashboardDirectory(stagingDirectory); err != nil {
		return fmt.Errorf("sync export staging directory: %w", err)
	}
	if _, err := os.Lstat(outputDirectory); err == nil {
		return fmt.Errorf("export output directory already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reinspect export output directory: %w", err)
	}
	if err := renameDashboardDirectoryNoReplace(
		stagingDirectory,
		outputDirectory,
	); err != nil {
		return fmt.Errorf("publish export directory: %w", err)
	}
	published = true
	if err := syncDashboardDirectory(parent); err != nil {
		rollbackErr := renameDashboardDirectoryNoReplace(
			outputDirectory,
			stagingDirectory,
		)
		if rollbackErr == nil {
			published = false
			rollbackSyncErr := syncDashboardDirectory(parent)
			if rollbackSyncErr != nil {
				return fmt.Errorf(
					"%w: publish parent sync failed: %v; rollback sync failed: %v",
					errDashboardPublishOutcomeUnknown,
					err,
					rollbackSyncErr,
				)
			}
			return fmt.Errorf("sync export parent: %w", err)
		}
		return fmt.Errorf(
			"%w: parent sync failed: %v; rollback failed: %v",
			errDashboardPublishOutcomeUnknown,
			err,
			rollbackErr,
		)
	}
	return nil
}

func writeExclusiveFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create export file %q: %w", filepath.Base(path), err)
	}
	if err := writeDashboardBytes(file, data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write export file %q: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync export file %q: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close export file %q: %w", filepath.Base(path), err)
	}
	return nil
}

func writeDashboardBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func syncDashboardDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
