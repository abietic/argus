package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/abietic/argus/internal/agentanalytics"
	"github.com/abietic/argus/internal/agentshadow"
	"github.com/abietic/argus/internal/store/local"
)

const agentReviewAnalyticsUsage = `usage:
  argus agent-review analytics rebuild --store <absolute-dir> --snapshot <id> --start <RFC3339-UTC> --end <RFC3339-UTC> --built-at <RFC3339-UTC> [--group-by <tenant|workspace|agent|provider|model>]... [--json]
  argus agent-review analytics show --store <absolute-dir> --snapshot <id> [--json]
  argus agent-review analytics export --store <absolute-dir> --snapshot <id> --target <facts|projection> --format <json|csv> --output-dir <absolute-new-dir> [--json]

Rebuild reads strict execution attempts plus committed successful shadow
imports. Failed, canceled, and unknown-outcome attempts remain host-observed
operational facts with no fabricated task receipts. Query and export read one
immutable projection snapshot. Outputs are diagnostic-only; they are not
billing, ROI, Finding, promotion, or platform-attestation facts.`

const agentAnalyticsExportPublicationSchemaVersion = "argus.agent_execution_export_publication.v1alpha3"

var errAgentAnalyticsPublishOutcomeUnknown = errors.New(
	"agent execution analytics export publish outcome is unknown",
)

type agentAnalyticsRebuildFlags struct {
	store    string
	snapshot string
	start    string
	end      string
	builtAt  string
	groupBy  stringList
	json     bool
}

type agentAnalyticsShowFlags struct {
	store    string
	snapshot string
	json     bool
}

type agentAnalyticsExportFlags struct {
	store     string
	snapshot  string
	target    string
	format    string
	outputDir string
	json      bool
}

type agentAnalyticsSnapshotOutput struct {
	Snapshot  agentanalytics.ProjectionSnapshot `json:"snapshot"`
	StorePath string                            `json:"store_path"`
}

type agentAnalyticsExportOutput struct {
	SnapshotID      string                        `json:"snapshot_id"`
	SnapshotSHA256  string                        `json:"snapshot_sha256"`
	Target          agentanalytics.ExportTarget   `json:"target"`
	OutputDirectory string                        `json:"output_directory"`
	Manifest        agentanalytics.ExportManifest `json:"manifest"`
}

type agentAnalyticsExportPublication struct {
	SchemaVersion   string                        `json:"schema_version"`
	SnapshotID      string                        `json:"snapshot_id"`
	SnapshotSHA256  string                        `json:"snapshot_sha256"`
	Scope           agentanalytics.Scope          `json:"scope"`
	Target          agentanalytics.ExportTarget   `json:"target"`
	DatasetManifest agentanalytics.ExportManifest `json:"dataset_manifest"`
}

func runAgentReviewAnalytics(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s", agentReviewAnalyticsUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, agentReviewAnalyticsUsage)
		return err
	}
	switch arguments[0] {
	case "rebuild":
		return runAgentAnalyticsRebuild(ctx, arguments[1:], stdout)
	case "show":
		return runAgentAnalyticsShow(arguments[1:], stdout)
	case "export":
		return runAgentAnalyticsExport(arguments[1:], stdout)
	default:
		return fmt.Errorf(
			"unknown agent-review analytics action %q\n%s",
			arguments[0],
			agentReviewAnalyticsUsage,
		)
	}
}

func runAgentAnalyticsRebuild(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	options, err := parseAgentAnalyticsRebuildFlags(arguments)
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
	groupBy, err := parseAgentAnalyticsGroupBy(options.groupBy)
	if err != nil {
		return err
	}
	adapter, storePath, err := openAgentAnalyticsRebuilder(options.store)
	if err != nil {
		return err
	}
	snapshot, err := adapter.Rebuild(ctx, agentanalytics.RebuildRequest{
		SnapshotID: options.snapshot,
		Scope: agentanalytics.Scope{
			TenantID:    agentshadow.LocalTenantID,
			WorkspaceID: agentshadow.LocalWorkspaceID,
		},
		Window: agentanalytics.TimeWindow{
			StartInclusive: start,
			EndExclusive:   end,
		},
		GroupBy: groupBy,
		BuiltAt: builtAt,
	})
	if err != nil {
		return err
	}
	return writeAgentAnalyticsSnapshot(stdout, snapshot, storePath, options.json)
}

func runAgentAnalyticsShow(arguments []string, stdout io.Writer) error {
	options, err := parseAgentAnalyticsShowFlags(arguments)
	if err != nil {
		return err
	}
	adapter, storePath, err := openAgentAnalyticsQuery(options.store)
	if err != nil {
		return err
	}
	snapshot, err := adapter.Query(options.snapshot)
	if err != nil {
		return err
	}
	if err := validateAgentAnalyticsCLIScope(snapshot); err != nil {
		return err
	}
	return writeAgentAnalyticsSnapshot(stdout, snapshot, storePath, options.json)
}

func runAgentAnalyticsExport(arguments []string, stdout io.Writer) error {
	options, err := parseAgentAnalyticsExportFlags(arguments)
	if err != nil {
		return err
	}
	target, err := parseAgentAnalyticsExportTarget(options.target)
	if err != nil {
		return err
	}
	format, err := parseAgentAnalyticsExportFormat(options.format)
	if err != nil {
		return err
	}
	adapter, storePath, err := openAgentAnalyticsQuery(options.store)
	if err != nil {
		return err
	}
	snapshot, err := adapter.Query(options.snapshot)
	if err != nil {
		return err
	}
	if err := validateAgentAnalyticsCLIScope(snapshot); err != nil {
		return err
	}
	outputDirectory, err := canonicalAgentAnalyticsExportPath(
		options.outputDir,
		storePath,
	)
	if err != nil {
		return err
	}
	bundle, err := exportAgentAnalyticsSnapshot(snapshot, target, format)
	if err != nil {
		return err
	}
	publication, err := newAgentAnalyticsExportPublication(snapshot, target, bundle)
	if err != nil {
		return err
	}
	if err := persistAgentAnalyticsExport(outputDirectory, publication, bundle); err != nil {
		return err
	}
	output := agentAnalyticsExportOutput{
		SnapshotID: snapshot.SnapshotID, SnapshotSHA256: publication.SnapshotSHA256,
		Target: target, OutputDirectory: outputDirectory, Manifest: bundle.Manifest,
	}
	var outputErr error
	if options.json {
		outputErr = writeJSON(stdout, output)
	} else {
		_, outputErr = fmt.Fprintf(
			stdout,
			"agent analytics snapshot=%s snapshot_sha256=%s target=%s format=%s files=%d output=%s authority=%s\n",
			output.SnapshotID,
			output.SnapshotSHA256,
			output.Target,
			output.Manifest.Format,
			len(output.Manifest.Files),
			output.OutputDirectory,
			agentanalytics.DiagnosticAuthority,
		)
	}
	if outputErr != nil {
		return fmt.Errorf(
			"%w: export is committed at %q but output acknowledgement failed: %v",
			errAgentAnalyticsPublishOutcomeUnknown,
			outputDirectory,
			outputErr,
		)
	}
	return nil
}

func parseAgentAnalyticsRebuildFlags(
	arguments []string,
) (agentAnalyticsRebuildFlags, error) {
	var options agentAnalyticsRebuildFlags
	flags := newFlagSet("agent-review analytics rebuild")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.snapshot, "snapshot", "", "immutable diagnostic snapshot ID")
	flags.StringVar(&options.start, "start", "", "half-open observation window start")
	flags.StringVar(&options.end, "end", "", "half-open observation window end")
	flags.StringVar(&options.builtAt, "built-at", "", "deterministic snapshot build time")
	flags.Var(&options.groupBy, "group-by", "diagnostic dimension; repeatable")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return agentAnalyticsRebuildFlags{}, controlFlagError(
			"agent-review analytics rebuild", err, agentReviewAnalyticsUsage,
		)
	}
	if flags.NArg() != 0 {
		return agentAnalyticsRebuildFlags{}, controlFlagError(
			"agent-review analytics rebuild",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			agentReviewAnalyticsUsage,
		)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return agentAnalyticsRebuildFlags{}, err
	}
	for _, field := range []struct{ name, value string }{
		{"snapshot", options.snapshot},
		{"start", options.start},
		{"end", options.end},
		{"built-at", options.builtAt},
	} {
		if field.value == "" {
			return agentAnalyticsRebuildFlags{}, fmt.Errorf("--%s is required", field.name)
		}
	}
	if options.groupBy == nil {
		options.groupBy = stringList{}
	}
	return options, nil
}

func parseAgentAnalyticsShowFlags(arguments []string) (agentAnalyticsShowFlags, error) {
	var options agentAnalyticsShowFlags
	flags := newFlagSet("agent-review analytics show")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.snapshot, "snapshot", "", "immutable diagnostic snapshot ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return agentAnalyticsShowFlags{}, controlFlagError(
			"agent-review analytics show", err, agentReviewAnalyticsUsage,
		)
	}
	if flags.NArg() != 0 {
		return agentAnalyticsShowFlags{}, controlFlagError(
			"agent-review analytics show",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			agentReviewAnalyticsUsage,
		)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return agentAnalyticsShowFlags{}, err
	}
	if options.snapshot == "" {
		return agentAnalyticsShowFlags{}, fmt.Errorf("--snapshot is required")
	}
	return options, nil
}

func parseAgentAnalyticsExportFlags(arguments []string) (agentAnalyticsExportFlags, error) {
	var options agentAnalyticsExportFlags
	flags := newFlagSet("agent-review analytics export")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.snapshot, "snapshot", "", "immutable diagnostic snapshot ID")
	flags.StringVar(&options.target, "target", "", "facts or projection")
	flags.StringVar(&options.format, "format", "", "json or csv")
	flags.StringVar(&options.outputDir, "output-dir", "", "clean absolute new directory")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return agentAnalyticsExportFlags{}, controlFlagError(
			"agent-review analytics export", err, agentReviewAnalyticsUsage,
		)
	}
	if flags.NArg() != 0 {
		return agentAnalyticsExportFlags{}, controlFlagError(
			"agent-review analytics export",
			fmt.Errorf("unexpected argument %q", flags.Arg(0)),
			agentReviewAnalyticsUsage,
		)
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return agentAnalyticsExportFlags{}, err
	}
	for _, field := range []struct{ name, value string }{
		{"snapshot", options.snapshot},
		{"target", options.target},
		{"format", options.format},
		{"output-dir", options.outputDir},
	} {
		if field.value == "" {
			return agentAnalyticsExportFlags{}, fmt.Errorf("--%s is required", field.name)
		}
	}
	if !filepath.IsAbs(options.outputDir) || filepath.Clean(options.outputDir) != options.outputDir {
		return agentAnalyticsExportFlags{}, fmt.Errorf("--output-dir must be a clean absolute path")
	}
	return options, nil
}

func parseAgentAnalyticsGroupBy(values []string) ([]agentanalytics.DimensionName, error) {
	result := make([]agentanalytics.DimensionName, len(values))
	seen := make(map[agentanalytics.DimensionName]struct{}, len(values))
	for index, value := range values {
		dimension := agentanalytics.DimensionName(value)
		switch dimension {
		case agentanalytics.DimensionTenant,
			agentanalytics.DimensionWorkspace,
			agentanalytics.DimensionAgent,
			agentanalytics.DimensionProvider,
			agentanalytics.DimensionModel:
		default:
			return nil, fmt.Errorf(
				"--group-by must be tenant, workspace, agent, provider, or model",
			)
		}
		if _, duplicate := seen[dimension]; duplicate {
			return nil, fmt.Errorf("duplicate --group-by %q", value)
		}
		seen[dimension] = struct{}{}
		result[index] = dimension
	}
	return result, nil
}

func parseAgentAnalyticsExportTarget(value string) (agentanalytics.ExportTarget, error) {
	switch agentanalytics.ExportTarget(value) {
	case agentanalytics.ExportFacts, agentanalytics.ExportProjection:
		return agentanalytics.ExportTarget(value), nil
	default:
		return "", fmt.Errorf("--target must be facts or projection")
	}
}

func parseAgentAnalyticsExportFormat(value string) (string, error) {
	switch value {
	case "json", agentanalytics.CanonicalJSONExportFormat:
		return agentanalytics.CanonicalJSONExportFormat, nil
	case "csv", agentanalytics.CanonicalCSVExportFormat:
		return agentanalytics.CanonicalCSVExportFormat, nil
	default:
		return "", fmt.Errorf("--format must be json or csv")
	}
}

func openAgentAnalyticsRebuilder(
	requestedStore string,
) (*agentanalytics.Adapter, string, error) {
	store, err := local.Open(requestedStore)
	if err != nil {
		return nil, "", fmt.Errorf("open agent analytics state: %w", err)
	}
	repository, err := agentshadow.OpenRepository(store, time.Now)
	if err != nil {
		return nil, "", fmt.Errorf("open committed agent shadow repository: %w", err)
	}
	service, err := agentshadow.NewService(repository)
	if err != nil {
		return nil, "", fmt.Errorf("open committed agent shadow service: %w", err)
	}
	adapter, err := agentanalytics.New(service, store)
	if err != nil {
		return nil, "", fmt.Errorf("initialize agent analytics rebuild adapter: %w", err)
	}
	return adapter, store.Root(), nil
}

func openAgentAnalyticsQuery(
	requestedStore string,
) (*agentanalytics.Adapter, string, error) {
	store, err := local.Open(requestedStore)
	if err != nil {
		return nil, "", fmt.Errorf("open agent analytics state: %w", err)
	}
	adapter, err := agentanalytics.Open(store)
	if err != nil {
		return nil, "", fmt.Errorf("open agent analytics projection adapter: %w", err)
	}
	return adapter, store.Root(), nil
}

func writeAgentAnalyticsSnapshot(
	stdout io.Writer,
	snapshot agentanalytics.ProjectionSnapshot,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		return writeJSON(stdout, agentAnalyticsSnapshotOutput{
			Snapshot: snapshot, StorePath: storePath,
		})
	}
	_, err := fmt.Fprintf(
		stdout,
		"agent analytics snapshot=%s executions=%d tasks=%d tools=%d tiles=%d store=%s authority=%s\n",
		snapshot.SnapshotID,
		len(snapshot.Facts.Executions),
		len(snapshot.Facts.Tasks),
		len(snapshot.Facts.ToolUsage),
		len(snapshot.Projection.Tiles),
		storePath,
		agentanalytics.DiagnosticAuthority,
	)
	return err
}

func validateAgentAnalyticsCLIScope(
	snapshot agentanalytics.ProjectionSnapshot,
) error {
	if snapshot.Scope.TenantID != agentshadow.LocalTenantID ||
		snapshot.Scope.WorkspaceID != agentshadow.LocalWorkspaceID {
		return fmt.Errorf(
			"agent-review analytics CLI only reads the fixed local/local scope",
		)
	}
	return nil
}

func exportAgentAnalyticsSnapshot(
	snapshot agentanalytics.ProjectionSnapshot,
	target agentanalytics.ExportTarget,
	format string,
) (agentanalytics.ExportBundle, error) {
	switch target {
	case agentanalytics.ExportFacts:
		switch format {
		case agentanalytics.CanonicalJSONExportFormat:
			return agentanalytics.ExportFactSetJSON(snapshot.Facts)
		case agentanalytics.CanonicalCSVExportFormat:
			return agentanalytics.ExportFactSetCSV(snapshot.Facts)
		}
	case agentanalytics.ExportProjection:
		switch format {
		case agentanalytics.CanonicalJSONExportFormat:
			return agentanalytics.ExportProjectionJSON(snapshot.Projection)
		case agentanalytics.CanonicalCSVExportFormat:
			return agentanalytics.ExportProjectionCSV(snapshot.Projection)
		}
	}
	return agentanalytics.ExportBundle{}, fmt.Errorf(
		"unsupported agent analytics export target/format %q/%q",
		target,
		format,
	)
}

func newAgentAnalyticsExportPublication(
	snapshot agentanalytics.ProjectionSnapshot,
	target agentanalytics.ExportTarget,
	bundle agentanalytics.ExportBundle,
) (agentAnalyticsExportPublication, error) {
	if err := snapshot.Validate(); err != nil {
		return agentAnalyticsExportPublication{}, fmt.Errorf(
			"validate agent analytics export snapshot: %w",
			err,
		)
	}
	if err := validateAgentAnalyticsCLIScope(snapshot); err != nil {
		return agentAnalyticsExportPublication{}, err
	}
	if err := bundle.Validate(); err != nil {
		return agentAnalyticsExportPublication{}, fmt.Errorf(
			"validate agent analytics export bundle: %w",
			err,
		)
	}
	snapshotData, err := json.Marshal(snapshot)
	if err != nil {
		return agentAnalyticsExportPublication{}, fmt.Errorf(
			"marshal agent analytics export snapshot: %w",
			err,
		)
	}
	digest := sha256.Sum256(snapshotData)
	publication := agentAnalyticsExportPublication{
		SchemaVersion:   agentAnalyticsExportPublicationSchemaVersion,
		SnapshotID:      snapshot.SnapshotID,
		SnapshotSHA256:  hex.EncodeToString(digest[:]),
		Scope:           snapshot.Scope,
		Target:          target,
		DatasetManifest: bundle.Manifest,
	}
	if err := publication.Validate(bundle); err != nil {
		return agentAnalyticsExportPublication{}, err
	}
	return publication, nil
}

func (publication agentAnalyticsExportPublication) Validate(
	bundle agentanalytics.ExportBundle,
) error {
	if publication.SchemaVersion != agentAnalyticsExportPublicationSchemaVersion {
		return fmt.Errorf("unsupported agent analytics export publication schema")
	}
	if publication.SnapshotID == "" ||
		publication.SnapshotID != strings.TrimSpace(publication.SnapshotID) {
		return fmt.Errorf("export publication requires a safe snapshot_id")
	}
	if len(publication.SnapshotSHA256) != sha256.Size*2 {
		return fmt.Errorf("export publication requires snapshot_sha256")
	}
	decoded, err := hex.DecodeString(publication.SnapshotSHA256)
	if err != nil || hex.EncodeToString(decoded) != publication.SnapshotSHA256 {
		return fmt.Errorf("export publication snapshot_sha256 must be lowercase SHA-256")
	}
	if publication.Scope.TenantID != agentshadow.LocalTenantID ||
		publication.Scope.WorkspaceID != agentshadow.LocalWorkspaceID {
		return fmt.Errorf("export publication must bind the fixed local/local scope")
	}
	switch publication.Target {
	case agentanalytics.ExportFacts, agentanalytics.ExportProjection:
	default:
		return fmt.Errorf("export publication has unsupported target %q", publication.Target)
	}
	if err := bundle.Validate(); err != nil {
		return fmt.Errorf("validate export publication bundle: %w", err)
	}
	if !reflect.DeepEqual(publication.DatasetManifest, bundle.Manifest) {
		return fmt.Errorf("export publication does not bind its dataset manifest")
	}
	return nil
}

func canonicalAgentAnalyticsExportPath(
	requestedPath string,
	storeRoot string,
) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(requestedPath))
	if err != nil {
		return "", fmt.Errorf("resolve export output parent: %w", err)
	}
	canonicalOutput := filepath.Join(parent, filepath.Base(requestedPath))
	canonicalStore, err := filepath.EvalSymlinks(storeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve agent analytics store: %w", err)
	}
	if pathsOverlap(canonicalOutput, canonicalStore) {
		return "", fmt.Errorf("--output-dir must be outside the Argus store")
	}
	return canonicalOutput, nil
}

func pathsOverlap(left string, right string) bool {
	return sameOrDescendant(left, right) || sameOrDescendant(right, left)
}

func sameOrDescendant(candidate string, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func persistAgentAnalyticsExport(
	outputDirectory string,
	publication agentAnalyticsExportPublication,
	bundle agentanalytics.ExportBundle,
) error {
	if err := publication.Validate(bundle); err != nil {
		return fmt.Errorf("validate agent analytics export publication: %w", err)
	}
	manifest, err := marshalAgentAnalyticsExportPublication(publication)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(outputDirectory); err == nil {
		if err := validateExistingAgentAnalyticsExport(
			outputDirectory,
			publication,
			bundle,
			manifest,
		); err != nil {
			return fmt.Errorf("export output directory already exists with different content: %w", err)
		}
		return syncDashboardDirectory(filepath.Dir(outputDirectory))
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
	stagingDirectory, err := os.MkdirTemp(parent, ".argus-agent-analytics-export-*")
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
		if err := writeExclusiveFile(filepath.Join(stagingDirectory, name), file.Data); err != nil {
			return err
		}
	}
	if err := writeExclusiveFile(filepath.Join(stagingDirectory, "manifest.json"), manifest); err != nil {
		return err
	}
	if err := syncDashboardDirectory(stagingDirectory); err != nil {
		return fmt.Errorf("sync export staging directory: %w", err)
	}
	if _, err := os.Lstat(outputDirectory); err == nil {
		if err := validateExistingAgentAnalyticsExport(
			outputDirectory,
			publication,
			bundle,
			manifest,
		); err != nil {
			return fmt.Errorf("export output directory appeared with different content: %w", err)
		}
		return syncDashboardDirectory(parent)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reinspect export output directory: %w", err)
	}
	if err := renameDashboardDirectoryNoReplace(stagingDirectory, outputDirectory); err != nil {
		if validationErr := validateExistingAgentAnalyticsExport(
			outputDirectory,
			publication,
			bundle,
			manifest,
		); validationErr == nil {
			return syncDashboardDirectory(parent)
		} else {
			return fmt.Errorf(
				"publish export directory: %v; existing output validation: %w",
				err,
				validationErr,
			)
		}
	}
	published = true
	if err := syncDashboardDirectory(parent); err != nil {
		rollbackErr := renameDashboardDirectoryNoReplace(outputDirectory, stagingDirectory)
		if rollbackErr == nil {
			published = false
			if rollbackSyncErr := syncDashboardDirectory(parent); rollbackSyncErr != nil {
				return fmt.Errorf(
					"%w: publish parent sync failed: %v; rollback sync failed: %v",
					errAgentAnalyticsPublishOutcomeUnknown,
					err,
					rollbackSyncErr,
				)
			}
			return fmt.Errorf("sync export parent: %w", err)
		}
		return fmt.Errorf(
			"%w: parent sync failed: %v; rollback failed: %v",
			errAgentAnalyticsPublishOutcomeUnknown,
			err,
			rollbackErr,
		)
	}
	return nil
}

func marshalAgentAnalyticsExportPublication(
	publication agentAnalyticsExportPublication,
) ([]byte, error) {
	manifest, err := json.MarshalIndent(publication, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal agent analytics export publication: %w", err)
	}
	return append(manifest, '\n'), nil
}

func validateExistingAgentAnalyticsExport(
	outputDirectory string,
	publication agentAnalyticsExportPublication,
	bundle agentanalytics.ExportBundle,
	manifest []byte,
) error {
	if err := publication.Validate(bundle); err != nil {
		return err
	}
	info, err := os.Lstat(outputDirectory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("existing export target is not a real directory")
	}
	entries, err := os.ReadDir(outputDirectory)
	if err != nil {
		return fmt.Errorf("read existing export directory: %w", err)
	}
	expected := make(map[string][]byte, len(bundle.Files)+1)
	for _, file := range bundle.Files {
		expected[file.Manifest.Name] = file.Data
	}
	expected["manifest.json"] = manifest
	if len(entries) != len(expected) {
		return fmt.Errorf("existing export file set differs")
	}
	for _, entry := range entries {
		want, exists := expected[entry.Name()]
		if !exists {
			return fmt.Errorf("existing export contains unexpected file %q", entry.Name())
		}
		if err := validateExactRegularFile(
			filepath.Join(outputDirectory, entry.Name()),
			want,
		); err != nil {
			return fmt.Errorf("existing export file %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func validateExactRegularFile(path string, expected []byte) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() != int64(len(expected)) {
		return fmt.Errorf("file is not the expected bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		return fmt.Errorf("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expected) {
		return fmt.Errorf("file content differs")
	}
	return nil
}
