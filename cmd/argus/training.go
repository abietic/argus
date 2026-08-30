package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/training"
)

const trainingUsage = `usage:
  argus training materialize --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus training show --store <absolute-dir> --manifest <id> --access <absolute-json> [--json]
  argus training list --store <absolute-dir> --access <absolute-json> [--json]
  argus training export build --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus training export show --store <absolute-dir> --export <id> --access <absolute-json> [--json]
  argus training export list --store <absolute-dir> --access <absolute-json> [--json]
  argus training export publish --store <absolute-dir> --export <id> --access <absolute-json> --output <absolute-new-directory> [--json]
  argus training job prepare --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus training job observe --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus training job show --store <absolute-dir> --job <id> --access <absolute-json> [--json]
  argus training job list --store <absolute-dir> --access <absolute-json> [--json]`

var errTrainingExportPublishOutcomeUnknown = errors.New(
	"training export publish outcome is unknown",
)

type trainingOutput struct {
	Record    training.Record `json:"record"`
	StorePath string          `json:"store_path"`
}
type trainingListOutput struct {
	Records   []training.Record `json:"records"`
	StorePath string            `json:"store_path"`
}
type trainingExportOutput struct {
	Record     training.ExportRecord `json:"record"`
	StorePath  string                `json:"store_path"`
	OutputPath string                `json:"output_path,omitempty"`
}
type trainingExportListOutput struct {
	Records   []training.ExportRecord `json:"records"`
	StorePath string                  `json:"store_path"`
}
type trainingJobOutput struct {
	Record    training.JobRecord `json:"record"`
	StorePath string             `json:"store_path"`
}
type trainingJobListOutput struct {
	Records   []training.JobRecord `json:"records"`
	StorePath string               `json:"store_path"`
}

func runTraining(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(trainingUsage)
	}
	if isHelpArgument(arguments) {
		_, err := fmt.Fprintln(stdout, trainingUsage)
		return err
	}
	switch arguments[0] {
	case "export":
		return runTrainingExport(ctx, arguments[1:], stdout)
	case "job":
		return runTrainingJob(ctx, arguments[1:], stdout)
	case "materialize":
		options, err := parseGovernedWriteFlags("training materialize", arguments[1:], trainingUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "training materialization request", training.DecodeMaterializationRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		repository, storePath, err := openTrainingRepository(options.store)
		if err != nil {
			return err
		}
		record, err := repository.Materialize(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writeTrainingRecord(stdout, record, storePath, options.json)
	case "show":
		options, err := parseGovernedReadFlags("training show", arguments[1:], trainingUsage, "manifest", "training manifest ID")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		repository, storePath, err := openTrainingRepository(options.store)
		if err != nil {
			return err
		}
		record, err := repository.Get(options.id, access)
		if err != nil {
			return err
		}
		return writeTrainingRecord(stdout, record, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags("training list", arguments[1:], trainingUsage, "", "")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		repository, storePath, err := openTrainingRepository(options.store)
		if err != nil {
			return err
		}
		records, err := repository.List(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, trainingListOutput{Records: records, StorePath: storePath})
		}
		for _, record := range records {
			if _, err := fmt.Fprintf(stdout, "manifest=%s dataset=%s@%s samples=%d mode=%s store=%s\n", record.Manifest.ManifestID, record.Manifest.DatasetID, record.Manifest.DatasetRevision, len(record.Manifest.Samples), record.Manifest.ContentMode, storePath); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "manifests=%d store=%s\n", len(records), storePath)
		return err
	default:
		return fmt.Errorf("unknown training command %q\n%s", arguments[0], trainingUsage)
	}
}

func runTrainingJob(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(trainingUsage)
	}
	switch arguments[0] {
	case "prepare":
		options, err := parseGovernedWriteFlags("training job prepare", arguments[1:], trainingUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "training job prepare request", training.DecodeJobPrepareRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		service, storePath, err := openTrainingJobService(options.store)
		if err != nil {
			return err
		}
		record, err := service.Prepare(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writeTrainingJobRecord(stdout, record, storePath, options.json)
	case "observe":
		options, err := parseGovernedWriteFlags("training job observe", arguments[1:], trainingUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "training job observation request", training.DecodeJobObservationRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		service, storePath, err := openTrainingJobService(options.store)
		if err != nil {
			return err
		}
		record, err := service.Observe(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writeTrainingJobRecord(stdout, record, storePath, options.json)
	case "show":
		options, err := parseGovernedReadFlags("training job show", arguments[1:], trainingUsage, "job", "training job ID")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		service, storePath, err := openTrainingJobService(options.store)
		if err != nil {
			return err
		}
		record, err := service.Get(options.id, access)
		if err != nil {
			return err
		}
		return writeTrainingJobRecord(stdout, record, storePath, options.json)
	case "list":
		options, err := parseGovernedReadFlags("training job list", arguments[1:], trainingUsage, "", "")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		service, storePath, err := openTrainingJobService(options.store)
		if err != nil {
			return err
		}
		records, err := service.List(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, trainingJobListOutput{Records: records, StorePath: storePath})
		}
		for _, record := range records {
			if _, err := fmt.Fprintf(stdout, "job=%s export=%s status=%s observations=%d store=%s\n", record.Plan.Request.JobID, record.Plan.Request.ExportID, record.Plan.Status, len(record.Plan.Observations), storePath); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "jobs=%d store=%s\n", len(records), storePath)
		return err
	default:
		return fmt.Errorf("unknown training job command %q\n%s", arguments[0], trainingUsage)
	}
}

func openTrainingJobService(path string) (*training.JobService, string, error) {
	storePath, err := validateExplicitControlStore(path)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", err
	}
	cases, err := evaluation.New(store)
	if err != nil {
		return nil, "", err
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		return nil, "", err
	}
	manifests, err := training.New(store, cases, artifacts)
	if err != nil {
		return nil, "", err
	}
	exporter, err := training.NewExporter(store, manifests, artifacts)
	if err != nil {
		return nil, "", err
	}
	service, err := training.NewJobService(store, exporter, artifacts)
	if err != nil {
		return nil, "", err
	}
	return service, store.Root(), nil
}

func writeTrainingJobRecord(stdout io.Writer, record training.JobRecord, storePath string, asJSON bool) error {
	if asJSON {
		return writeJSON(stdout, trainingJobOutput{Record: record, StorePath: storePath})
	}
	_, err := fmt.Fprintf(stdout, "job=%s export=%s status=%s observations=%d promotion_eligible=%t authority=%s store=%s\n", record.Plan.Request.JobID, record.Plan.Request.ExportID, record.Plan.Status, len(record.Plan.Observations), record.Plan.PromotionEligible, record.Plan.ReceiptAuthority, storePath)
	return err
}

func runTrainingExport(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(trainingUsage)
	}
	switch arguments[0] {
	case "build":
		options, err := parseGovernedWriteFlags("training export build", arguments[1:], trainingUsage)
		if err != nil {
			return err
		}
		request, err := readStrictDescriptor(options.input, "training export request", training.DecodeExportRequest)
		if err != nil {
			return err
		}
		mutation, err := readEvaluationMutation(options.mutation)
		if err != nil {
			return err
		}
		exporter, _, storePath, err := openTrainingExporter(options.store)
		if err != nil {
			return err
		}
		record, err := exporter.Build(ctx, request, mutation)
		if err != nil {
			return err
		}
		return writeTrainingExportRecord(stdout, record, storePath, "", options.json)
	case "show":
		options, err := parseGovernedReadFlags("training export show", arguments[1:], trainingUsage, "export", "training export ID")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		exporter, _, storePath, err := openTrainingExporter(options.store)
		if err != nil {
			return err
		}
		record, err := exporter.Get(options.id, access)
		if err != nil {
			return err
		}
		return writeTrainingExportRecord(stdout, record, storePath, "", options.json)
	case "list":
		options, err := parseGovernedReadFlags("training export list", arguments[1:], trainingUsage, "", "")
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		exporter, _, storePath, err := openTrainingExporter(options.store)
		if err != nil {
			return err
		}
		records, err := exporter.List(access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, trainingExportListOutput{Records: records, StorePath: storePath})
		}
		for _, record := range records {
			if _, err := fmt.Fprintf(stdout, "export=%s manifest=%s samples=%d receipts=%d store=%s\n", record.Bundle.ExportID, record.Bundle.ManifestID, len(record.Bundle.Samples), len(record.Bundle.Receipts), storePath); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "exports=%d store=%s\n", len(records), storePath)
		return err
	case "publish":
		return runTrainingExportPublish(arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown training export command %q\n%s", arguments[0], trainingUsage)
	}
}

type trainingExportPublishFlags struct {
	store, exportID, access, output string
	json                            bool
}

func runTrainingExportPublish(arguments []string, stdout io.Writer) error {
	var options trainingExportPublishFlags
	flags := newFlagSet("training export publish")
	flags.StringVar(&options.store, "store", "", "explicit local state root")
	flags.StringVar(&options.exportID, "export", "", "training export ID")
	flags.StringVar(&options.access, "access", "", "absolute strict JSON access descriptor")
	flags.StringVar(&options.output, "output", "", "absolute new export directory")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || options.exportID == "" {
		return errors.New(trainingUsage)
	}
	if _, err := validateExplicitControlStore(options.store); err != nil {
		return err
	}
	if err := validateDescriptorPath("access", options.access); err != nil {
		return err
	}
	if !filepath.IsAbs(options.output) || filepath.Clean(options.output) != options.output {
		return fmt.Errorf("--output must be a clean absolute path")
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	exporter, artifacts, storePath, err := openTrainingExporter(options.store)
	if err != nil {
		return err
	}
	record, err := exporter.Get(options.exportID, access)
	if err != nil {
		return err
	}
	if err := persistTrainingExport(options.output, storePath, record, artifacts); err != nil {
		return err
	}
	return writeTrainingExportRecord(stdout, record, storePath, options.output, options.json)
}

func openTrainingExporter(path string) (*training.Exporter, *runrepo.Repository, string, error) {
	storePath, err := validateExplicitControlStore(path)
	if err != nil {
		return nil, nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, nil, "", err
	}
	cases, err := evaluation.New(store)
	if err != nil {
		return nil, nil, "", err
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		return nil, nil, "", err
	}
	manifests, err := training.New(store, cases, artifacts)
	if err != nil {
		return nil, nil, "", err
	}
	exporter, err := training.NewExporter(store, manifests, artifacts)
	if err != nil {
		return nil, nil, "", err
	}
	return exporter, artifacts, store.Root(), nil
}

type portableTrainingArtifact struct {
	SourceRef   runmodel.ArtifactRef    `json:"source_ref"`
	RedactedRef runmodel.ArtifactRef    `json:"redacted_ref"`
	Hits        []training.RedactionHit `json:"hits"`
	Content     string                  `json:"content"`
}
type portableTrainingRecord struct {
	SchemaVersion   string                     `json:"schema_version"`
	ExportID        string                     `json:"export_id"`
	DatasetID       string                     `json:"dataset_id"`
	DatasetRevision string                     `json:"dataset_revision"`
	Sample          training.ManifestSample    `json:"sample"`
	Artifacts       []portableTrainingArtifact `json:"artifacts"`
}

func persistTrainingExport(outputDirectory, storePath string, record training.ExportRecord, artifacts *runrepo.Repository) error {
	if artifacts == nil {
		return fmt.Errorf("artifact repository is required")
	}
	if err := record.Bundle.Validate(); err != nil {
		return err
	}
	parent := filepath.Dir(outputDirectory)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect export output parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return fmt.Errorf("export output parent must be a real directory")
	}
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve export parent: %w", err)
	}
	info, err := os.Lstat(canonicalParent)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("export output parent must be a real directory")
	}
	target := filepath.Join(canonicalParent, filepath.Base(outputDirectory))
	relative, err := filepath.Rel(storePath, target)
	if err != nil {
		return err
	}
	if relative == "." || relative == "" || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("training export output must be outside the Argus store")
	}
	manifest, err := json.MarshalIndent(record.Bundle, "", "  ")
	if err != nil {
		return err
	}
	manifest = append(manifest, '\n')
	records, err := buildPortableTrainingRecords(record, artifacts)
	if err != nil {
		return err
	}
	expected := map[string][]byte{"manifest.json": manifest, "records.jsonl": records}
	if _, err := os.Lstat(target); err == nil {
		return validateExistingTrainingExport(target, expected)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging, err := os.MkdirTemp(canonicalParent, ".argus-training-export-*")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	names := []string{"manifest.json", "records.jsonl"}
	for _, name := range names {
		if err := writeExclusiveFile(filepath.Join(staging, name), expected[name]); err != nil {
			return err
		}
	}
	if err := syncDashboardDirectory(staging); err != nil {
		return err
	}
	if err := renameDashboardDirectoryNoReplace(staging, target); err != nil {
		if validationErr := validateExistingTrainingExport(target, expected); validationErr == nil {
			return syncDashboardDirectory(canonicalParent)
		}
		return err
	}
	published = true
	if err := syncDashboardDirectory(canonicalParent); err != nil {
		rollbackErr := renameDashboardDirectoryNoReplace(target, staging)
		if rollbackErr == nil {
			published = false
			if rollbackSyncErr := syncDashboardDirectory(canonicalParent); rollbackSyncErr != nil {
				return fmt.Errorf(
					"%w: publish parent sync failed: %v; rollback sync failed: %v",
					errTrainingExportPublishOutcomeUnknown,
					err,
					rollbackSyncErr,
				)
			}
			return fmt.Errorf("sync export parent: %w", err)
		}
		return fmt.Errorf(
			"%w: parent sync failed: %v; rollback failed: %v",
			errTrainingExportPublishOutcomeUnknown,
			err,
			rollbackErr,
		)
	}
	return nil
}

func buildPortableTrainingRecords(record training.ExportRecord, artifacts *runrepo.Repository) ([]byte, error) {
	receiptBySource := map[string]training.RedactionReceipt{}
	for _, receipt := range record.Bundle.Receipts {
		receiptBySource[portableArtifactKey(receipt.SourceRef)] = receipt
	}
	var output bytes.Buffer
	for _, sample := range record.Bundle.Samples {
		portable := portableTrainingRecord{SchemaVersion: "argus.training_portable_record.v1alpha1", ExportID: record.Bundle.ExportID, DatasetID: record.Bundle.DatasetID, DatasetRevision: record.Bundle.DatasetRevision, Sample: sample.GovernedSample, Artifacts: make([]portableTrainingArtifact, 0, len(sample.GovernedSample.ArtifactRefs))}
		for _, source := range sample.GovernedSample.ArtifactRefs {
			receipt, ok := receiptBySource[portableArtifactKey(source)]
			if !ok {
				return nil, fmt.Errorf("missing redaction receipt")
			}
			for _, use := range []runrepo.ArtifactUse{runrepo.ArtifactUseTraining, runrepo.ArtifactUseExport} {
				if err := artifacts.CheckArtifactEligibility(receipt.RedactedRef, use); err != nil {
					return nil, err
				}
			}
			content, err := artifacts.ReadArtifact(receipt.RedactedRef)
			if err != nil {
				return nil, err
			}
			probe, err := training.RedactStrictText(content, record.Bundle.Policy)
			if err != nil || len(probe.Hits) != 0 || !bytes.Equal(probe.Content, content) {
				return nil, fmt.Errorf("redacted artifact failed publication revalidation")
			}
			portable.Artifacts = append(portable.Artifacts, portableTrainingArtifact{SourceRef: source, RedactedRef: receipt.RedactedRef, Hits: receipt.Hits, Content: string(content)})
		}
		line, err := json.Marshal(portable)
		if err != nil {
			return nil, err
		}
		output.Write(line)
		output.WriteByte('\n')
	}
	return output.Bytes(), nil
}

func validateExistingTrainingExport(directory string, expected map[string][]byte) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("existing export target is not a real directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("existing export file set differs")
	}
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateExactRegularFile(filepath.Join(directory, name), expected[name]); err != nil {
			return err
		}
	}
	return nil
}
func portableArtifactKey(ref runmodel.ArtifactRef) string {
	data, _ := json.Marshal(ref)
	return string(data)
}
func writeTrainingExportRecord(stdout io.Writer, record training.ExportRecord, storePath, outputPath string, asJSON bool) error {
	if asJSON {
		return writeJSON(stdout, trainingExportOutput{Record: record, StorePath: storePath, OutputPath: outputPath})
	}
	_, err := fmt.Fprintf(stdout, "export=%s manifest=%s samples=%d receipts=%d format=%s store=%s output=%s\n", record.Bundle.ExportID, record.Bundle.ManifestID, len(record.Bundle.Samples), len(record.Bundle.Receipts), record.Bundle.PortableFormat, storePath, outputPath)
	return err
}

func openTrainingRepository(path string) (*training.Repository, string, error) {
	storePath, err := validateExplicitControlStore(path)
	if err != nil {
		return nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, "", err
	}
	cases, err := evaluation.New(store)
	if err != nil {
		return nil, "", err
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		return nil, "", err
	}
	repository, err := training.New(store, cases, artifacts)
	if err != nil {
		return nil, "", err
	}
	return repository, store.Root(), nil
}

func writeTrainingRecord(stdout io.Writer, record training.Record, storePath string, asJSON bool) error {
	if asJSON {
		return writeJSON(stdout, trainingOutput{Record: record, StorePath: storePath})
	}
	_, err := fmt.Fprintf(stdout, "manifest=%s dataset=%s@%s samples=%d mode=%s contains_source_bytes=%t self_labels_allowed=%t store=%s\n", record.Manifest.ManifestID, record.Manifest.DatasetID, record.Manifest.DatasetRevision, len(record.Manifest.Samples), record.Manifest.ContentMode, record.Manifest.ContainsSourceBytes, record.Manifest.SelfLabelsAllowed, storePath)
	return err
}
