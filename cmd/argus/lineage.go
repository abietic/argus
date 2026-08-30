package main

import (
	"context"
	"fmt"
	"io"

	"argus.local/argus/internal/findinglineage"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/store/local"
)

const lineageUsage = `usage:
  argus lineage build --store <absolute-dir> --baseline <run-id> --variant <run-id> [--idempotency-key <key>] [--json]
  argus lineage show --store <absolute-dir> --lineage <lineage-id> [--json]
  argus lineage list --store <absolute-dir> [--run <run-id>] [--repository <repository-id>] [--json]

Lineage accepts committed formal governed ReviewRuns from the same repository
and target mode but different target digests. Build requires exact commit OIDs,
proves baseline is a strict ancestor of variant in the same local Git object
graph, and records Git rename evidence at the fixed 50% threshold.`

type lineageFlags struct {
	store, baseline, variant, idempotencyKey, lineageID, runID, repositoryID string
	json                                                                     bool
}

func runLineage(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s", lineageUsage)
	}
	action := arguments[0]
	if action == "help" {
		if len(arguments) != 1 {
			return fmt.Errorf("%s", lineageUsage)
		}
		_, err := fmt.Fprintln(stdout, lineageUsage)
		return err
	}
	var options lineageFlags
	flags := newFlagSet("lineage " + action)
	flags.StringVar(&options.store, "store", "", "absolute local Argus store directory")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	switch action {
	case "build":
		flags.StringVar(&options.baseline, "baseline", "", "baseline formal ReviewRun ID")
		flags.StringVar(&options.variant, "variant", "", "variant formal ReviewRun ID")
		flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "optional exact retry key")
	case "show":
		flags.StringVar(&options.lineageID, "lineage", "", "finding lineage ID")
	case "list":
		flags.StringVar(&options.runID, "run", "", "filter by either source run ID")
		flags.StringVar(&options.repositoryID, "repository", "", "filter by repository ID")
	default:
		return fmt.Errorf("unknown lineage action %q\n%s", action, lineageUsage)
	}
	if err := flags.Parse(arguments[1:]); err != nil {
		return commandFlagError("lineage", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("lineage %s accepts flags only", action)
	}
	if action == "build" && (options.baseline == "" || options.variant == "") {
		return fmt.Errorf("--baseline and --variant are required")
	}
	if action == "show" && options.lineageID == "" {
		return fmt.Errorf("--lineage is required")
	}
	storePath, err := validateExplicitControlStore(options.store)
	if err != nil {
		return err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	gitSource, err := gitadapter.New()
	if err != nil {
		return err
	}
	repository, err := findinglineage.New(store, runs, nil, gitSource)
	if err != nil {
		return err
	}
	switch action {
	case "build":
		record, err := repository.Build(ctx, findinglineage.BuildRequest{
			SchemaVersion:  findinglineage.BuildRequestSchemaVersion,
			IdempotencyKey: options.idempotencyKey,
			BaselineRunID:  options.baseline, VariantRunID: options.variant,
			Policy: findinglineage.GitAwarePolicy(),
		})
		if err != nil {
			return err
		}
		return writeLineage(stdout, record, storePath, options.json)
	case "show":
		record, err := repository.Get(options.lineageID)
		if err != nil {
			return err
		}
		return writeLineage(stdout, record, storePath, options.json)
	case "list":
		records, err := repository.List(findinglineage.ListFilter{RunID: options.runID, RepositoryID: options.repositoryID})
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, records)
		}
		_, err = fmt.Fprintf(stdout, "# Finding lineages\n\nStore: `%s`\n\n| Lineage | Baseline | Variant | Continued | Split | Merged | Introduced | Resolved |\n|---|---|---|---:|---:|---:|---:|---:|\n", markdownCell(storePath))
		if err != nil {
			return err
		}
		for _, record := range records {
			s := record.Lineage.Summary
			if _, err := fmt.Fprintf(stdout, "| `%s` | `%s` | `%s` | %d | %d | %d | %d | %d |\n", markdownCell(record.Lineage.LineageID), markdownCell(record.Lineage.Baseline.RunID), markdownCell(record.Lineage.Variant.RunID), s.Continued, s.Split, s.Merged, s.Introduced, s.Resolved); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func writeLineage(stdout io.Writer, record findinglineage.Record, storePath string, asJSON bool) error {
	if asJSON {
		return writeJSON(stdout, record)
	}
	lineage := record.Lineage
	summary := lineage.Summary
	ancestry := "unverified caller order"
	if lineage.Ancestry != nil {
		ancestry = fmt.Sprintf(
			"local Git `%s` -> `%s`, merge-base `%s`, %d rename mapping(s)",
			lineage.Ancestry.BaselineHeadOID,
			lineage.Ancestry.VariantHeadOID,
			lineage.Ancestry.MergeBaseOID,
			len(lineage.PathMappings),
		)
	}
	_, err := fmt.Fprintf(stdout,
		"# Finding lineage `%s`\n\n- Baseline: `%s` (`%s`..`%s`)\n- Variant: `%s` (`%s`..`%s`)\n- Repository: `%s`\n- Policy: `%s@%s`\n- Ancestry: %s\n- Store: `%s`\n- Relations: continued %d, split %d, merged %d, introduced %d, resolved %d\n\n| Type | Method | Baseline findings | Variant findings | Reason |\n|---|---|---|---|---|\n",
		markdownCell(lineage.LineageID), markdownCell(lineage.Baseline.RunID), markdownCell(lineage.Baseline.BaseRevision), markdownCell(lineage.Baseline.HeadRevision), markdownCell(lineage.Variant.RunID), markdownCell(lineage.Variant.BaseRevision), markdownCell(lineage.Variant.HeadRevision), markdownCell(lineage.Baseline.Repository.RepositoryID), markdownCell(lineage.Policy.PolicyID), markdownCell(lineage.Policy.Revision), markdownCell(ancestry), markdownCell(storePath), summary.Continued, summary.Split, summary.Merged, summary.Introduced, summary.Resolved)
	if err != nil {
		return err
	}
	for _, relation := range lineage.Relations {
		if _, err := fmt.Fprintf(stdout, "| `%s` | `%s` | %s | %s | `%s` |\n", relation.Type, relation.Method, markdownCell(fmt.Sprint(relation.BaselineFindingIDs)), markdownCell(fmt.Sprint(relation.VariantFindingIDs)), markdownCell(relation.ReasonCode)); err != nil {
			return err
		}
	}
	return nil
}
