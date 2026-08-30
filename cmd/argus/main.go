package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/contextprovider"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/scheduling"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var version = "dev"

const usage = `usage:
  argus version
  argus validate review-spec <file>
  argus config <create|validate|publish|activate|rollback|list|show> --state-dir <absolute-dir> ...
  argus review --repo <absolute-path> --mode diff --base <revision> --head <revision> [--context-provider <repository_search|go_ast|go_dependencies|go_compile>]... [--context-file <kind>@<coverage-symbol>=<absolute-file>...] [--config-state-dir <absolute-dir>] [--store <dir>] [--json]
  argus review --repo <absolute-path> --mode selection --revision <revision> --path <file> (--start-line <n> --end-line <n> | --range <START:END> [--range <START:END>...] | --symbol-language go --symbol-kind <function|method|type> --symbol-qualified-name <name>) [--overlay-file <absolute-path>] [--context-provider <repository_search|go_ast|go_dependencies|go_compile>]... [--context-file <kind>@<coverage-symbol>=<absolute-file>...] [--config-state-dir <absolute-dir>] [--store <dir>] [--json]
  argus review --repo <absolute-path> --mode scope --revision <revision> --include <glob> [--include <glob>...] [--exclude <glob>...] [--context-provider <repository_search|go_ast|go_dependencies|go_compile>]... [--context-file <kind>@<coverage-symbol>=<absolute-file>...] [--config-state-dir <absolute-dir>] [--store <dir>] [--json]
  argus replay --run <id> --from <stage> [--change <variable> --variant-config-bundle <absolute-json> [--variant-workflow <absolute-json>]] [--config-state-dir <absolute-dir>] [--store <dir>] [--json]
  argus compare --baseline <id> --variant <id> [--store <dir>] [--json]
  argus lineage <build|show|list> --store <absolute-dir> ...
  argus history [--limit <n>] [--store <dir>] [--json]
  argus show --run <id> [--store <dir>] [--json]
  argus candidate <list|show> --store <absolute-dir> --run <id> [--candidate <id>] [--json]
  argus finding show --store <absolute-dir> --run <id> --finding <id> [--json]
  argus decision record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus publication grant record --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json]
  argus publication request build --store <absolute-dir> --input <absolute-json> [--json]
  argus publication github dispatch --store <absolute-dir> --input <absolute-json> --single-user-local [--json]
  argus publication github reconcile --store <absolute-dir> --publication <id> --single-user-local [--json]
  argus artifact integrity <inspect|quarantine|release|tombstone> --store <absolute-dir> ... [--json]
  argus workload pressure --store <absolute-dir> --at <RFC3339-UTC> [--json]
  argus feedback record --store <absolute-dir> --input <absolute-json> [--json]
  argus outcome record --store <absolute-dir> --input <absolute-json> [--json]
  argus evaluation <case|incident|exposure|run|experiment|batch> <action> --store <absolute-dir> ...
  argus calibration <fit|show|list|promotion> --store <absolute-dir> ...
  argus training <materialize|show|list> --store <absolute-dir> ...
  argus promotion <register|gate|show|rollback> --store <absolute-dir> ...
  argus dashboard <rebuild|show|export> --store <absolute-dir> ...
  argus agent-review <run|show|evidence|execution|analytics|formal> --store <absolute-dir> ...
  argus api serve --store <absolute-dir> --config-state-dir <absolute-dir> --principal <absolute-json> [--listen <loopback-ip:port>]

The default local store is <user-config-directory>/argus/local-store.
Replay stages: detect, normalize, verify, adjudicate, report.
Control-plane commands never use the default store; --store is required.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithIO(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "argus:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	return runWithIO(context.Background(), arguments, os.Stdout)
}

func runWithIO(ctx context.Context, arguments []string, stdout io.Writer) error {
	if ctx == nil || stdout == nil {
		return fmt.Errorf("command context and output are required")
	}
	if len(arguments) == 0 {
		return errors.New(usage)
	}
	// Every command group owns one complete usage block. Treat help as a
	// successful read-only request at any nesting depth instead of passing it
	// into flag.FlagSet, whose ErrHelp would otherwise be surfaced as a command
	// failure with its output discarded by newFlagSet.
	if len(arguments) >= 2 &&
		(arguments[len(arguments)-1] == "-h" ||
			arguments[len(arguments)-1] == "--help") {
		commandUsage, exists := subcommandUsage(arguments[0])
		if !exists {
			return fmt.Errorf("unknown command %q\n%s", arguments[0], usage)
		}
		_, err := fmt.Fprintln(stdout, commandUsage)
		return err
	}
	switch arguments[0] {
	case "help", "-h", "--help":
		if len(arguments) != 1 {
			return errors.New(usage)
		}
		_, err := fmt.Fprintln(stdout, usage)
		if err == nil {
			if storePath, pathErr := resolveStorePath(""); pathErr == nil {
				_, err = fmt.Fprintf(stdout, "\nResolved default store: %s\n", storePath)
			}
		}
		return err
	case "version":
		if len(arguments) != 1 {
			return fmt.Errorf("version accepts no arguments\n%s", usage)
		}
		_, err := fmt.Fprintf(stdout, "argus %s\n", version)
		return err
	case "validate":
		return runValidate(arguments[1:], stdout)
	case "config":
		return runConfig(ctx, arguments[1:], stdout)
	case "review":
		return runReview(ctx, arguments[1:], stdout)
	case "replay":
		return runReplay(ctx, arguments[1:], stdout)
	case "compare":
		return runCompare(ctx, arguments[1:], stdout)
	case "lineage":
		return runLineage(ctx, arguments[1:], stdout)
	case "history":
		return runHistory(arguments[1:], stdout)
	case "show":
		return runShow(arguments[1:], stdout)
	case "candidate":
		return runCandidate(arguments[1:], stdout)
	case "finding":
		return runFinding(arguments[1:], stdout)
	case "decision":
		return runDecision(ctx, arguments[1:], stdout)
	case "publication":
		return runPublication(ctx, arguments[1:], stdout)
	case "artifact":
		return runArtifact(ctx, arguments[1:], stdout)
	case "workload":
		return runWorkload(ctx, arguments[1:], stdout)
	case "feedback":
		return runFeedback(ctx, arguments[1:], stdout)
	case "outcome":
		return runOutcome(ctx, arguments[1:], stdout)
	case "evaluation":
		return runEvaluation(ctx, arguments[1:], stdout)
	case "calibration":
		return runCalibration(ctx, arguments[1:], stdout)
	case "training":
		return runTraining(ctx, arguments[1:], stdout)
	case "promotion":
		return runPromotion(ctx, arguments[1:], stdout)
	case "dashboard":
		return runDashboard(ctx, arguments[1:], stdout)
	case "agent-review":
		return runAgentReview(ctx, arguments[1:], stdout)
	case "api":
		return runAPI(ctx, arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q\n%s", arguments[0], usage)
	}
}

func runValidate(arguments []string, stdout io.Writer) error {
	if len(arguments) != 2 || arguments[0] != "review-spec" {
		return fmt.Errorf("usage: argus validate review-spec <file>")
	}
	data, err := os.ReadFile(arguments[1])
	if err != nil {
		return fmt.Errorf("read ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(data)
	if err != nil {
		return err
	}
	digest, err := contractsv1alpha1.DigestReviewSpec(spec)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		stdout,
		"valid %s request_id=%s sha256=%s\n",
		spec.SchemaVersion,
		spec.RequestID,
		digest,
	)
	return err
}

type reviewFlags struct {
	repository       string
	mode             string
	base             string
	head             string
	revision         string
	path             string
	startLine        uint
	endLine          uint
	ranges           selectionRangeList
	symbolLang       string
	symbolKind       string
	symbolName       string
	overlay          string
	include          stringList
	exclude          stringList
	contextFiles     contextFileList
	contextProviders contextProviderList
	store            string
	configState      string
	json             bool
}

func parseReviewFlags(arguments []string) (reviewFlags, error) {
	options := reviewFlags{mode: string(reviewcore.TargetModeDiff)}
	flags := newFlagSet("review")
	flags.StringVar(&options.repository, "repo", "", "clean absolute Git repository path")
	flags.StringVar(&options.mode, "mode", options.mode, "target mode: diff, selection, or scope")
	flags.StringVar(&options.base, "base", "", "base revision")
	flags.StringVar(&options.head, "head", "", "head revision")
	flags.StringVar(&options.revision, "revision", "", "selection/scope revision")
	flags.StringVar(&options.path, "path", "", "selection repository-relative path")
	flags.UintVar(&options.startLine, "start-line", 0, "selection first line")
	flags.UintVar(&options.endLine, "end-line", 0, "selection last line")
	flags.Var(
		&options.ranges,
		"range",
		"selection line range START:END; repeatable, maximum 128",
	)
	flags.StringVar(
		&options.symbolLang,
		"symbol-language",
		"",
		"selection symbol language: go",
	)
	flags.StringVar(
		&options.symbolKind,
		"symbol-kind",
		"",
		"selection symbol kind: function, method, or type",
	)
	flags.StringVar(
		&options.symbolName,
		"symbol-qualified-name",
		"",
		"selection symbol qualified name",
	)
	flags.StringVar(&options.overlay, "overlay-file", "", "absolute file containing unsaved selection overlay")
	flags.Var(&options.include, "include", "scope include glob; repeatable")
	flags.Var(&options.exclude, "exclude", "scope exclude glob; repeatable")
	flags.Var(
		&options.contextFiles,
		"context-file",
		"frozen context KIND@COVERAGE_SYMBOL=ABSOLUTE_FILE; repeatable (codegraph, lsp, repository_search, dependency, artifact)",
	)
	flags.Var(
		&options.contextProviders,
		"context-provider",
		"exact-revision automatic context provider; repeatable: repository_search, go_ast, go_dependencies, go_compile",
	)
	flags.StringVar(&options.store, "store", "", "local Argus store directory")
	flags.StringVar(
		&options.configState,
		"config-state-dir",
		"",
		"clean absolute lifecycle configuration state directory",
	)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return reviewFlags{}, err
	}
	if flags.NArg() != 0 {
		return reviewFlags{}, fmt.Errorf("review accepts flags only; unexpected argument %q", flags.Arg(0))
	}
	setFlags := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) {
		setFlags[value.Name] = true
	})
	legacySelectorSet := setFlags["start-line"] || setFlags["end-line"]
	rangeSelectorSet := setFlags["range"]
	symbolSelectorSet := setFlags["symbol-language"] ||
		setFlags["symbol-kind"] ||
		setFlags["symbol-qualified-name"]
	if options.repository == "" || !filepath.IsAbs(options.repository) ||
		filepath.Clean(options.repository) != options.repository {
		return reviewFlags{}, fmt.Errorf("--repo must be a clean absolute path")
	}
	if options.configState != "" &&
		(!filepath.IsAbs(options.configState) ||
			filepath.Clean(options.configState) != options.configState) {
		return reviewFlags{}, fmt.Errorf("--config-state-dir must be a clean absolute path")
	}
	switch reviewcore.TargetMode(options.mode) {
	case reviewcore.TargetModeDiff:
		if options.base == "" || options.head == "" {
			return reviewFlags{}, fmt.Errorf("diff mode requires --base and --head")
		}
		if options.revision != "" || options.path != "" ||
			legacySelectorSet || rangeSelectorSet || symbolSelectorSet ||
			options.overlay != "" ||
			len(options.include) != 0 || len(options.exclude) != 0 {
			return reviewFlags{}, fmt.Errorf("diff mode contains flags from another target mode")
		}
	case reviewcore.TargetModeSelection:
		if options.revision == "" || options.path == "" {
			return reviewFlags{}, fmt.Errorf("selection mode requires --revision and --path")
		}
		selectorForms := 0
		if legacySelectorSet {
			selectorForms++
			if !setFlags["start-line"] || !setFlags["end-line"] ||
				options.startLine == 0 || options.endLine < options.startLine {
				return reviewFlags{}, fmt.Errorf(
					"legacy selection requires a valid --start-line/--end-line pair",
				)
			}
			if uint64(options.endLine) > uint64(^uint32(0)) {
				return reviewFlags{}, fmt.Errorf("selection line range is too large")
			}
		}
		if rangeSelectorSet {
			selectorForms++
		}
		if symbolSelectorSet {
			selectorForms++
			if !setFlags["symbol-language"] ||
				!setFlags["symbol-kind"] ||
				!setFlags["symbol-qualified-name"] ||
				options.symbolLang == "" ||
				options.symbolKind == "" ||
				options.symbolName == "" {
				return reviewFlags{}, fmt.Errorf(
					"symbol selection requires --symbol-language, --symbol-kind, and --symbol-qualified-name",
				)
			}
			selector := application.SymbolSelector{
				Language:      options.symbolLang,
				Kind:          options.symbolKind,
				QualifiedName: options.symbolName,
			}
			if err := selector.Validate(); err != nil {
				return reviewFlags{}, err
			}
		}
		if selectorForms != 1 {
			return reviewFlags{}, fmt.Errorf(
				"selection mode requires exactly one selector: legacy line pair, --range, or symbol",
			)
		}
		if options.base != "" || options.head != "" ||
			len(options.include) != 0 || len(options.exclude) != 0 {
			return reviewFlags{}, fmt.Errorf("selection mode contains flags from another target mode")
		}
		if options.overlay != "" &&
			(!filepath.IsAbs(options.overlay) || filepath.Clean(options.overlay) != options.overlay) {
			return reviewFlags{}, fmt.Errorf("--overlay-file must be a clean absolute path")
		}
	case reviewcore.TargetModeScope:
		if options.revision == "" || len(options.include) == 0 {
			return reviewFlags{}, fmt.Errorf("scope mode requires --revision and at least one --include")
		}
		if options.base != "" || options.head != "" || options.path != "" ||
			legacySelectorSet || rangeSelectorSet || symbolSelectorSet ||
			options.overlay != "" {
			return reviewFlags{}, fmt.Errorf("scope mode contains flags from another target mode")
		}
		if options.exclude == nil {
			options.exclude = stringList{}
		}
	default:
		return reviewFlags{}, fmt.Errorf("--mode must be diff, selection, or scope")
	}
	if len(options.contextProviders) != 0 && options.configState != "" {
		return reviewFlags{}, fmt.Errorf(
			"--context-provider cannot be combined with --config-state-dir until governed provider execution is enabled",
		)
	}
	if len(options.contextProviders) != 0 && options.overlay != "" {
		return reviewFlags{}, fmt.Errorf(
			"--context-provider go_ast cannot type-check an unsaved overlay against a different exact commit",
		)
	}
	if len(options.contextProviders)+len(options.contextFiles) >
		contractsv1alpha1.AgentReviewWorkerMaxContextCount {
		return reviewFlags{}, fmt.Errorf(
			"automatic and file contexts accept at most %d total inputs",
			contractsv1alpha1.AgentReviewWorkerMaxContextCount,
		)
	}
	return options, nil
}

func runReview(ctx context.Context, arguments []string, stdout io.Writer) error {
	options, err := parseReviewFlags(arguments)
	if err != nil {
		return commandFlagError("review", err)
	}
	if err := rejectStoreInsideRepository(options.store, options.repository); err != nil {
		return err
	}
	if options.configState != "" {
		if err := rejectStoreInsideRepository(options.configState, options.repository); err != nil {
			return fmt.Errorf("config state: %w", err)
		}
	}
	repository, service, storePath, err := openRuntimeWithConfig(
		options.store,
		options.configState,
		true,
	)
	if err != nil {
		return err
	}
	contexts, err := publishReviewContextFiles(ctx, storePath, options.contextFiles)
	if err != nil {
		return err
	}
	request := application.ReviewRequest{
		RepositoryPath: options.repository,
		Mode:           reviewcore.TargetMode(options.mode),
		BaseRevision:   options.base,
		HeadRevision:   options.head,
		Revision:       options.revision,
		SelectionPath:  options.path,
		StartLine:      uint32(options.startLine),
		EndLine:        uint32(options.endLine),
		SelectionRanges: append(
			[]application.SelectionRange(nil),
			options.ranges...,
		),
		Include:            append([]string(nil), options.include...),
		Exclude:            append([]string(nil), options.exclude...),
		Contexts:           contexts,
		ContextProviderIDs: append([]string(nil), options.contextProviders...),
	}
	slices.Sort(request.ContextProviderIDs)
	if options.symbolLang != "" {
		request.SelectionSymbol = &application.SymbolSelector{
			Language:      options.symbolLang,
			Kind:          options.symbolKind,
			QualifiedName: options.symbolName,
		}
	}
	if request.Mode == reviewcore.TargetModeScope && request.Exclude == nil {
		request.Exclude = []string{}
	}
	if options.overlay != "" {
		overlay, readErr := readSelectionOverlay(options.overlay)
		if readErr != nil {
			return readErr
		}
		request.OverlayContent = &overlay
	}
	sortContextBindings(request.Contexts)
	outcome, err := service.Review(ctx, request)
	if err != nil {
		if outcome.Run.RunID != "" {
			return fmt.Errorf("review run %s: %w", outcome.Run.RunID, err)
		}
		return err
	}
	return writeRunOutcome(stdout, outcome, repository, storePath, options.json)
}

type stringList []string

type contextFileFlag struct {
	Kind           string
	CoverageSymbol string
	Path           string
}

type contextFileList []contextFileFlag

type contextProviderList []string

func (values *contextProviderList) String() string {
	if values == nil {
		return ""
	}
	return strings.Join(*values, ",")
}

func (values *contextProviderList) Set(value string) error {
	if value != "repository_search" && value != "go_ast" && value != "go_dependencies" && value != "go_compile" {
		return fmt.Errorf("--context-provider %q is unsupported; expected repository_search, go_ast, go_dependencies, or go_compile", value)
	}
	if slices.Contains(*values, value) {
		return fmt.Errorf("--context-provider %q is duplicated", value)
	}
	*values = append(*values, value)
	return nil
}

func (values *contextFileList) String() string {
	if values == nil {
		return ""
	}
	formatted := make([]string, len(*values))
	for index, value := range *values {
		formatted[index] = value.Kind + "@" + value.CoverageSymbol + "=" + value.Path
	}
	return strings.Join(formatted, ",")
}

func (values *contextFileList) Set(value string) error {
	if len(*values) >= contractsv1alpha1.AgentReviewWorkerMaxContextCount {
		return fmt.Errorf(
			"--context-file accepts at most %d files",
			contractsv1alpha1.AgentReviewWorkerMaxContextCount,
		)
	}
	kindAndCoverage, path, found := strings.Cut(value, "=")
	kind, coverageSymbol, coverageFound := strings.Cut(kindAndCoverage, "@")
	if !found || !coverageFound || kind == "" || coverageSymbol == "" || path == "" {
		return fmt.Errorf("--context-file must use KIND@COVERAGE_SYMBOL=ABSOLUTE_FILE")
	}
	if coverageSymbol != strings.TrimSpace(coverageSymbol) ||
		len(coverageSymbol) > 512 || !utf8.ValidString(coverageSymbol) {
		return fmt.Errorf("--context-file coverage symbol must be trimmed UTF-8 without controls")
	}
	for _, character := range coverageSymbol {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("--context-file coverage symbol must be trimmed UTF-8 without controls")
		}
	}
	switch kind {
	case "codegraph", "lsp", "repository_search", "dependency", "artifact":
	default:
		return fmt.Errorf("--context-file kind %q is unsupported", kind)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("--context-file path must be a clean absolute path")
	}
	if explicitContextPathSensitive(path) {
		return fmt.Errorf("--context-file path is classified as credential-sensitive")
	}
	*values = append(*values, contextFileFlag{
		Kind: kind, CoverageSymbol: coverageSymbol, Path: path,
	})
	return nil
}

func explicitContextPathSensitive(value string) bool {
	lower := strings.ToLower(filepath.ToSlash(value))
	segments := strings.Split(lower, "/")
	for _, segment := range segments {
		switch segment {
		case ".ssh", ".aws", ".gnupg", ".kube", "id_rsa", "id_ed25519", "credentials":
			return true
		}
		if segment == ".env" || strings.HasPrefix(segment, ".env.") {
			return true
		}
	}
	switch filepath.Ext(lower) {
	case ".pem", ".key", ".p12", ".pfx":
		return true
	default:
		return false
	}
}

func publishReviewContextFiles(
	ctx context.Context,
	storePath string,
	files []contextFileFlag,
) ([]reviewcore.ContextBinding, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return []reviewcore.ContextBinding{}, nil
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("open context artifact store: %w", err)
	}
	artifacts, err := runrepo.New(store)
	if err != nil {
		return nil, fmt.Errorf("open local context artifact repository: %w", err)
	}
	bindings := make([]reviewcore.ContextBinding, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for index, file := range files {
		content, readErr := readSelectionOverlayWithLimit(
			file.Path,
			contractsv1alpha1.AgentReviewContextArtifactMaxBytes,
		)
		if readErr != nil {
			return nil, fmt.Errorf("read context file %d: %w", index, readErr)
		}
		if content == "" || !utf8.ValidString(content) || strings.IndexByte(content, 0) >= 0 {
			return nil, fmt.Errorf("context file %d must be non-empty UTF-8 without NUL", index)
		}
		contract := "argus.context." + file.Kind + ".v1alpha1"
		ref, putErr := artifacts.PutArtifact(contract, []byte(content))
		if putErr != nil {
			return nil, fmt.Errorf("publish context file %d: %w", index, putErr)
		}
		contextID := file.Kind + "-" + ref.SHA256[:16]
		if _, duplicate := seen[contextID]; duplicate {
			return nil, fmt.Errorf("context files contain duplicate content id %q", contextID)
		}
		seen[contextID] = struct{}{}
		bindings = append(bindings, reviewcore.ContextBinding{Ref: &reviewcore.ContextRef{
			ContextID: contextID,
			Kind:      file.Kind,
			Revision:  "sha256-" + ref.SHA256[:16],
			Digest:    ref.SHA256,
			Coverage: reviewcore.ContextCoverage{
				Spans: []reviewcore.ContextSpan{}, Symbols: []string{file.CoverageSymbol},
			},
			Provenance: reviewcore.ContextProvenance{
				Provider: file.Kind, ProducerID: "argus-local-context-file",
				ProducerRevision: "v1",
			},
			ArtifactURI: ref.URI, Contract: ref.Contract, SizeBytes: ref.SizeBytes,
		}})
	}
	return bindings, nil
}

func sortContextBindings(bindings []reviewcore.ContextBinding) {
	sort.Slice(bindings, func(left, right int) bool {
		return bindings[left].ContextID() < bindings[right].ContextID()
	})
}

type selectionRangeList []application.SelectionRange

func (values *selectionRangeList) String() string {
	if values == nil {
		return ""
	}
	formatted := make([]string, 0, len(*values))
	for _, lineRange := range *values {
		formatted = append(
			formatted,
			fmt.Sprintf("%d:%d", lineRange.StartLine, lineRange.EndLine),
		)
	}
	return strings.Join(formatted, ",")
}

func (values *selectionRangeList) Set(value string) error {
	if len(*values) >= 128 {
		return fmt.Errorf("--range accepts at most 128 ranges")
	}
	startText, endText, found := strings.Cut(value, ":")
	if !found || strings.Contains(endText, ":") {
		return fmt.Errorf("--range %q must use START:END", value)
	}
	start, err := parsePositiveUint32(startText)
	if err != nil {
		return fmt.Errorf("--range %q start: %w", value, err)
	}
	end, err := parsePositiveUint32(endText)
	if err != nil {
		return fmt.Errorf("--range %q end: %w", value, err)
	}
	if end < start {
		return fmt.Errorf("--range %q end must be greater than or equal to start", value)
	}
	*values = append(*values, application.SelectionRange{
		StartLine: start,
		EndLine:   end,
	})
	return nil
}

func parsePositiveUint32(value string) (uint32, error) {
	if value == "" {
		return 0, fmt.Errorf("line must be a positive base-10 integer")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("line must be a positive base-10 integer")
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("line must be between 1 and %d", uint64(^uint32(0)))
	}
	return uint32(parsed), nil
}

func (values *stringList) String() string {
	if values == nil {
		return ""
	}
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	if value == "" {
		return fmt.Errorf("glob must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func readSelectionOverlay(path string) (string, error) {
	return readSelectionOverlayWithLimit(
		path,
		application.DefaultLocalConfig().MaxFileContentBytes,
	)
}

func readSelectionOverlayWithLimit(path string, limit int64) (string, error) {
	if limit < 1 {
		return "", fmt.Errorf("selection overlay byte limit must be positive")
	}
	// O_NONBLOCK lets us inspect and reject FIFOs or other special files
	// without waiting for a peer. It has no effect on regular-file reads.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open selection overlay: %w", err)
	}
	defer file.Close()

	// os.Open intentionally preserves the existing policy of following a
	// symlink. Stat and Read use the same descriptor, so a path replacement
	// cannot switch the file after validation.
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat opened selection overlay: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("selection overlay must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", fmt.Errorf("read selection overlay: %w", err)
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("selection overlay exceeds local limit of %d bytes", limit)
	}
	return string(data), nil
}

type replayFlags struct {
	run                 string
	from                string
	change              string
	variantConfigBundle string
	variantWorkflow     string
	store               string
	configState         string
	json                bool
}

func parseReplayFlags(arguments []string) (replayFlags, error) {
	var options replayFlags
	flags := newFlagSet("replay")
	flags.StringVar(&options.run, "run", "", "source run ID")
	flags.StringVar(&options.from, "from", "", "first stage to execute")
	flags.StringVar(
		&options.change,
		"change",
		"",
		"one atomic replay variable: rule_pack, prompt, model, index, workflow, filter_policy, or budget",
	)
	flags.StringVar(
		&options.variantConfigBundle,
		"variant-config-bundle",
		"",
		"clean absolute path to a strict ConfigBundle JSON descriptor",
	)
	flags.StringVar(
		&options.variantWorkflow,
		"variant-workflow",
		"",
		"clean absolute path to the exact WorkflowDefinition required by workflow replay",
	)
	flags.StringVar(&options.store, "store", "", "local Argus store directory")
	flags.StringVar(
		&options.configState,
		"config-state-dir",
		"",
		"clean absolute lifecycle configuration state directory",
	)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return replayFlags{}, err
	}
	if flags.NArg() != 0 {
		return replayFlags{}, fmt.Errorf("replay accepts flags only; unexpected argument %q", flags.Arg(0))
	}
	if options.run == "" || options.from == "" {
		return replayFlags{}, fmt.Errorf("--run and --from are required")
	}
	if !validReplayStage(options.from) {
		return replayFlags{}, fmt.Errorf(
			"--from must be a deterministic workflow stage from materialize_target through export_evaluation",
		)
	}
	switch {
	case options.change == "" && options.variantConfigBundle == "":
	case options.change == "":
		return replayFlags{}, fmt.Errorf(
			"--variant-config-bundle requires one explicit --change",
		)
	case options.variantConfigBundle == "":
		return replayFlags{}, fmt.Errorf(
			"--change requires --variant-config-bundle",
		)
	default:
		variable := runmodel.ReplayVariable(options.change)
		if variable == runmodel.ReplayVariableNone {
			return replayFlags{}, fmt.Errorf(
				"--change none is not a variant; omit both variant flags for an exact replay",
			)
		}
		if err := variable.Validate(); err != nil {
			return replayFlags{}, fmt.Errorf("--change: %w", err)
		}
		if err := validateDescriptorPath(
			"variant-config-bundle",
			options.variantConfigBundle,
		); err != nil {
			return replayFlags{}, err
		}
	}
	if runmodel.ReplayVariable(options.change) == runmodel.ReplayVariableWorkflow {
		if options.variantWorkflow == "" {
			return replayFlags{}, fmt.Errorf("--change workflow requires --variant-workflow")
		}
		if err := validateDescriptorPath("variant-workflow", options.variantWorkflow); err != nil {
			return replayFlags{}, err
		}
	} else if options.variantWorkflow != "" {
		return replayFlags{}, fmt.Errorf("--variant-workflow is only valid with --change workflow")
	}
	if options.configState != "" &&
		(!filepath.IsAbs(options.configState) ||
			filepath.Clean(options.configState) != options.configState) {
		return replayFlags{}, fmt.Errorf("--config-state-dir must be a clean absolute path")
	}
	return options, nil
}

func runReplay(ctx context.Context, arguments []string, stdout io.Writer) error {
	options, err := parseReplayFlags(arguments)
	if err != nil {
		return commandFlagError("replay", err)
	}
	var variantBundle *reviewconfig.ConfigBundle
	var variantWorkflow *workflow.Definition
	if options.variantConfigBundle != "" {
		bundle, readErr := readStrictDescriptor(
			options.variantConfigBundle,
			"variant config bundle",
			reviewconfig.DecodeBundle,
		)
		if readErr != nil {
			return readErr
		}
		variantBundle = &bundle
	}
	if options.variantWorkflow != "" {
		definition, readErr := readStrictDescriptor(
			options.variantWorkflow,
			"variant workflow",
			workflow.DecodeDefinition,
		)
		if readErr != nil {
			return readErr
		}
		variantWorkflow = &definition
	}
	repository, service, storePath, err := openRuntimeWithConfig(
		options.store,
		options.configState,
		runmodel.ReplayVariable(options.change) == runmodel.ReplayVariableIndex,
	)
	if err != nil {
		return err
	}
	outcome, err := service.Replay(ctx, application.ReplayRequest{
		SourceRunID:         options.run,
		StartStage:          options.from,
		Variable:            runmodel.ReplayVariable(options.change),
		VariantConfigBundle: variantBundle,
		VariantWorkflow:     variantWorkflow,
	})
	if err != nil {
		if outcome.Run.RunID != "" {
			return fmt.Errorf("replay run %s: %w", outcome.Run.RunID, err)
		}
		return err
	}
	return writeRunOutcome(stdout, outcome, repository, storePath, options.json)
}

type compareFlags struct {
	baseline string
	variant  string
	store    string
	json     bool
}

func parseCompareFlags(arguments []string) (compareFlags, error) {
	var options compareFlags
	flags := newFlagSet("compare")
	flags.StringVar(&options.baseline, "baseline", "", "baseline run ID")
	flags.StringVar(&options.variant, "variant", "", "variant run ID")
	flags.StringVar(&options.store, "store", "", "local Argus store directory")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return compareFlags{}, err
	}
	if flags.NArg() != 0 {
		return compareFlags{}, fmt.Errorf("compare accepts flags only; unexpected argument %q", flags.Arg(0))
	}
	if options.baseline == "" || options.variant == "" {
		return compareFlags{}, fmt.Errorf("--baseline and --variant are required")
	}
	if options.baseline == options.variant {
		return compareFlags{}, fmt.Errorf("--baseline and --variant must be different runs")
	}
	return options, nil
}

func runCompare(ctx context.Context, arguments []string, stdout io.Writer) error {
	options, err := parseCompareFlags(arguments)
	if err != nil {
		return commandFlagError("compare", err)
	}
	_, service, storePath, err := openRuntime(options.store, false)
	if err != nil {
		return err
	}
	comparison, err := service.Compare(ctx, options.baseline, options.variant)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, comparison)
	}
	return writeComparisonMarkdown(stdout, comparison, storePath)
}

type historyFlags struct {
	limit int
	store string
	json  bool
}

func parseHistoryFlags(arguments []string) (historyFlags, error) {
	options := historyFlags{limit: 20}
	flags := newFlagSet("history")
	flags.IntVar(&options.limit, "limit", options.limit, "maximum number of runs")
	flags.StringVar(&options.store, "store", "", "local Argus store directory")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return historyFlags{}, err
	}
	if flags.NArg() != 0 {
		return historyFlags{}, fmt.Errorf("history accepts flags only; unexpected argument %q", flags.Arg(0))
	}
	if options.limit < 0 {
		return historyFlags{}, fmt.Errorf("--limit must be zero or greater")
	}
	return options, nil
}

func runHistory(arguments []string, stdout io.Writer) error {
	options, err := parseHistoryFlags(arguments)
	if err != nil {
		return commandFlagError("history", err)
	}
	_, service, storePath, err := openRuntime(options.store, false)
	if err != nil {
		return err
	}
	history, err := service.History(options.limit)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, history)
	}
	return writeHistoryMarkdown(stdout, history, storePath)
}

type showFlags struct {
	run   string
	store string
	json  bool
}

func parseShowFlags(arguments []string) (showFlags, error) {
	var options showFlags
	flags := newFlagSet("show")
	flags.StringVar(&options.run, "run", "", "run ID")
	flags.StringVar(&options.store, "store", "", "local Argus store directory")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return showFlags{}, err
	}
	if flags.NArg() != 0 {
		return showFlags{}, fmt.Errorf("show accepts flags only; unexpected argument %q", flags.Arg(0))
	}
	if options.run == "" {
		return showFlags{}, fmt.Errorf("--run is required")
	}
	return options, nil
}

func runShow(arguments []string, stdout io.Writer) error {
	options, err := parseShowFlags(arguments)
	if err != nil {
		return commandFlagError("show", err)
	}
	repository, _, storePath, err := openRuntime(options.store, false)
	if err != nil {
		return err
	}
	result, err := repository.LoadCommittedRunResult(options.run)
	if err != nil {
		return fmt.Errorf("load run %q: %w", options.run, err)
	}
	run := result.Run
	coverage := summarizeRunCoverage(repository, run)
	if options.json {
		return writeJSON(stdout, showOutput{
			Run: run, Report: result.Report, CandidateSet: result.CandidateSet,
			VerificationLedger: result.VerificationLedger,
			CalibrationLedger:  result.CalibrationLedger,
			SuppressionLedger:  result.SuppressionLedger,
			GovernedReport:     result.GovernedReport,
			Coverage:           coverage, StorePath: storePath,
		})
	}
	if run.MarkdownReportRef != nil {
		data, readErr := repository.ReadArtifact(*run.MarkdownReportRef)
		if readErr != nil {
			return fmt.Errorf("read Markdown report: %w", readErr)
		}
		if _, err := writeRunHeader(stdout, run, repository, storePath); err != nil {
			return err
		}
		_, err = stdout.Write(data)
		return err
	}
	if run.GovernedMarkdownRef != nil {
		data, readErr := repository.ReadArtifact(*run.GovernedMarkdownRef)
		if readErr != nil {
			return fmt.Errorf("read governed Markdown report: %w", readErr)
		}
		if _, err := writeRunHeader(stdout, run, repository, storePath); err != nil {
			return err
		}
		_, err = stdout.Write(data)
		return err
	}
	_, err = fmt.Fprintf(
		stdout,
		"# Argus run `%s`\n\n- Kind: `%s`\n- Status: `%s`\n- Store: `%s`\n",
		run.RunID,
		run.Kind,
		run.Status,
		markdownCell(storePath),
	)
	if err == nil && run.Failure != nil {
		_, err = fmt.Fprintf(
			stdout,
			"- Failure: `%s` — %s\n",
			markdownCell(run.Failure.Code),
			markdownCell(run.Failure.Message),
		)
	}
	if err == nil {
		_, err = writeCoverageMarkdown(
			stdout,
			coverage,
			true,
		)
	}
	return err
}

type runOutput struct {
	Run       runmodel.ReviewRun     `json:"run"`
	FinalRef  runmodel.ArtifactRef   `json:"final_ref"`
	Report    *reviewcore.Report     `json:"report,omitempty"`
	Reused    []reviewcore.StageName `json:"reused_stages"`
	Coverage  runCoverageSummary     `json:"coverage"`
	StorePath string                 `json:"store_path"`
}

type showOutput struct {
	Run                runmodel.ReviewRun                             `json:"run"`
	Report             *reviewcore.Report                             `json:"report,omitempty"`
	CandidateSet       *contractsv1alpha1.GovernedCandidateSet        `json:"candidate_set,omitempty"`
	VerificationLedger *contractsv1alpha1.CandidateVerificationLedger `json:"verification_ledger,omitempty"`
	CalibrationLedger  *contractsv1alpha1.FindingCalibrationLedger    `json:"calibration_ledger,omitempty"`
	SuppressionLedger  *contractsv1alpha1.FindingSuppressionLedger    `json:"suppression_ledger,omitempty"`
	GovernedReport     *contractsv1alpha1.GovernedReviewReport        `json:"governed_report,omitempty"`
	Coverage           runCoverageSummary                             `json:"coverage"`
	StorePath          string                                         `json:"store_path"`
}

func writeRunOutcome(
	stdout io.Writer,
	outcome application.RunOutcome,
	repository *runrepo.Repository,
	storePath string,
	asJSON bool,
) error {
	if asJSON {
		reused := outcome.Reused
		if reused == nil {
			reused = []reviewcore.StageName{}
		}
		return writeJSON(stdout, runOutput{
			Run: outcome.Run, FinalRef: outcome.FinalRef, Report: outcome.Report,
			Reused: reused,
			Coverage: summarizeRunCoverage(
				repository,
				outcome.Run,
			),
			StorePath: storePath,
		})
	}
	if _, err := writeRunHeader(stdout, outcome.Run, repository, storePath); err != nil {
		return err
	}
	if outcome.Markdown == "" {
		_, err := fmt.Fprintln(stdout, "No report was produced.")
		return err
	}
	_, err := io.WriteString(stdout, outcome.Markdown)
	return err
}

func writeRunHeader(
	stdout io.Writer,
	run runmodel.ReviewRun,
	repository *runrepo.Repository,
	storePath string,
) (int, error) {
	written, err := fmt.Fprintf(
		stdout,
		"Run: `%s`  \nKind: `%s`  \nTarget: `%s`  \nStatus: `%s`  \nStore: `%s`\n\n",
		markdownCell(run.RunID),
		run.Kind,
		run.TargetMode,
		run.Status,
		markdownCell(storePath),
	)
	if err != nil {
		return written, err
	}
	count, err := writeCoverageMarkdown(
		stdout,
		summarizeRunCoverage(repository, run),
		false,
	)
	return written + count, err
}

func writeComparisonMarkdown(
	stdout io.Writer,
	comparison runmodel.RunComparison,
	storePath string,
) error {
	if _, err := fmt.Fprintf(
		stdout,
		"# Argus run comparison\n\n- Baseline: `%s`\n- Variant: `%s`\n"+
			"- Store: `%s`\n- Findings: added %d, removed %d, unchanged %d\n"+
			"- Candidates: added %d, removed %d, unchanged %d\n"+
			"- Decision changes: %d\n- Stage latency entries: %d\n"+
			"- Quality: %s\n- Cost: %s\n\n",
		markdownCell(comparison.BaselineRunID),
		markdownCell(comparison.VariantRunID),
		markdownCell(storePath),
		len(comparison.Added),
		len(comparison.Removed),
		len(comparison.Unchanged),
		len(comparison.CandidatesAdded),
		len(comparison.CandidatesRemoved),
		len(comparison.CandidatesUnchanged),
		len(comparison.DecisionChanges),
		len(comparison.StageLatency),
		metricAvailabilityMarkdown(comparison.Quality),
		metricAvailabilityMarkdown(comparison.Cost),
	); err != nil {
		return err
	}
	for _, section := range []struct {
		title   string
		changes []runmodel.FindingChange
	}{
		{"Added", comparison.Added},
		{"Removed", comparison.Removed},
		{"Unchanged", comparison.Unchanged},
	} {
		if _, err := fmt.Fprintf(stdout, "## %s\n\n", section.title); err != nil {
			return err
		}
		if len(section.changes) == 0 {
			if _, err := fmt.Fprint(stdout, "None.\n\n"); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintln(stdout, "| Location | Title | Fingerprint |\n|---|---|---|"); err != nil {
			return err
		}
		for _, change := range section.changes {
			if _, err := fmt.Fprintf(
				stdout,
				"| `%s:%d` | %s | `%s` |\n",
				markdownCell(change.Path),
				change.Line,
				markdownCell(change.Title),
				markdownCell(change.Fingerprint),
			); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(stdout); err != nil {
			return err
		}
	}
	if err := writeCandidateComparisonMarkdown(stdout, comparison); err != nil {
		return err
	}
	if err := writeDecisionComparisonMarkdown(stdout, comparison.DecisionChanges); err != nil {
		return err
	}
	if err := writeStageLatencyMarkdown(stdout, comparison.StageLatency); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "## Metric availability"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		stdout,
		"\n| Metric | Available | Reason |\n|---|---:|---|\n"+
			"| Quality | %t | `%s` |\n| Cost | %t | `%s` |\n",
		comparison.Quality.Available,
		markdownCell(comparison.Quality.ReasonCode),
		comparison.Cost.Available,
		markdownCell(comparison.Cost.ReasonCode),
	); err != nil {
		return err
	}
	return nil
}

func writeCandidateComparisonMarkdown(
	stdout io.Writer,
	comparison runmodel.RunComparison,
) error {
	for _, section := range []struct {
		title   string
		changes []runmodel.CandidateChange
	}{
		{"Candidates added", comparison.CandidatesAdded},
		{"Candidates removed", comparison.CandidatesRemoved},
		{"Candidates unchanged", comparison.CandidatesUnchanged},
	} {
		if _, err := fmt.Fprintf(stdout, "## %s\n\n", section.title); err != nil {
			return err
		}
		if len(section.changes) == 0 {
			if _, err := fmt.Fprint(stdout, "None.\n\n"); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintln(
			stdout,
			"| Baseline candidate | Variant candidate | Fingerprint | Rule | Location |\n|---|---|---|---|---|",
		); err != nil {
			return err
		}
		for _, change := range section.changes {
			if _, err := fmt.Fprintf(
				stdout,
				"| `%s` | `%s` | `%s` | `%s` | `%s:%d` |\n",
				markdownCell(change.CandidateID),
				markdownCell(change.VariantCandidateID),
				markdownCell(change.Fingerprint),
				markdownCell(change.RuleID),
				markdownCell(change.Path),
				change.Line,
			); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(stdout); err != nil {
			return err
		}
	}
	return nil
}

func writeDecisionComparisonMarkdown(
	stdout io.Writer,
	changes []runmodel.DecisionChange,
) error {
	if _, err := fmt.Fprintln(stdout, "## Decision changes"); err != nil {
		return err
	}
	if len(changes) == 0 {
		_, err := fmt.Fprintln(stdout, "\nNone.")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout)
		return err
	}
	if _, err := fmt.Fprintln(
		stdout,
		"\n| Finding | Fingerprint | Baseline | Variant |\n|---|---|---|---|",
	); err != nil {
		return err
	}
	for _, change := range changes {
		if _, err := fmt.Fprintf(
			stdout,
			"| `%s` | `%s` | `%s` | `%s` |\n",
			markdownCell(change.FindingID),
			markdownCell(change.Fingerprint),
			markdownCell(change.BaselineAction),
			markdownCell(change.VariantAction),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(stdout)
	return err
}

func writeStageLatencyMarkdown(
	stdout io.Writer,
	latencies []runmodel.StageLatencyDelta,
) error {
	if _, err := fmt.Fprintln(stdout, "## Stage latency"); err != nil {
		return err
	}
	if len(latencies) == 0 {
		_, err := fmt.Fprintln(stdout, "\nNone.")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout)
		return err
	}
	if _, err := fmt.Fprintln(
		stdout,
		"\n| Stage | Baseline ms | Variant ms | Delta ms | Baseline reused | Variant reused |\n"+
			"|---|---:|---:|---:|---:|---:|",
	); err != nil {
		return err
	}
	for _, latency := range latencies {
		if _, err := fmt.Fprintf(
			stdout,
			"| `%s` | %d | %d | %d | %t | %t |\n",
			markdownCell(latency.StageID),
			latency.BaselineDurationMS,
			latency.VariantDurationMS,
			latency.DeltaMS,
			latency.BaselineReused,
			latency.VariantReused,
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(stdout)
	return err
}

func metricAvailabilityMarkdown(metric runmodel.MetricAvailability) string {
	if metric.Available {
		return "available"
	}
	if metric.ReasonCode == "" {
		return "unavailable"
	}
	return "unavailable (`" + markdownCell(metric.ReasonCode) + "`)"
}

func writeHistoryMarkdown(
	stdout io.Writer,
	history []runrepo.HistoryEntry,
	storePath string,
) error {
	if _, err := fmt.Fprintf(
		stdout,
		"# Argus run history\n\nStore: `%s`\n",
		markdownCell(storePath),
	); err != nil {
		return err
	}
	if len(history) == 0 {
		_, err := fmt.Fprintln(stdout, "\nNo runs.")
		return err
	}
	if _, err := fmt.Fprintln(
		stdout,
		"\n| Run | Kind | Status | Last event | Time |\n|---|---|---|---|---|",
	); err != nil {
		return err
	}
	for _, entry := range history {
		if _, err := fmt.Fprintf(
			stdout,
			"| `%s` | %s | %s | `%s` | %s |\n",
			markdownCell(entry.RunID),
			entry.Kind,
			entry.Status,
			markdownCell(entry.LastEventType),
			entry.LastEventTime.UTC().Format(time.RFC3339),
		); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(stdout io.Writer, value any) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}
	return nil
}

func openRuntime(
	requestedStore string,
	requireGit bool,
) (*runrepo.Repository, *application.Service, string, error) {
	return openRuntimeWithConfig(requestedStore, "", requireGit)
}

func openRuntimeWithConfig(
	requestedStore string,
	configState string,
	requireGit bool,
) (*runrepo.Repository, *application.Service, string, error) {
	storePath, err := resolveStorePath(requestedStore)
	if err != nil {
		return nil, nil, "", err
	}
	store, err := local.Open(storePath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open local store %q: %w", storePath, err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		return nil, nil, "", err
	}
	var source application.TargetSource = unavailableTargetSource{}
	var providerExecutor application.ContextProviderExecutor
	if requireGit {
		adapter, adapterErr := gitadapter.New()
		if adapterErr != nil {
			return nil, nil, "", fmt.Errorf("initialize Git adapter: %w", adapterErr)
		}
		source = adapter
		providerExecutor, adapterErr = contextprovider.NewLocalExecutor(adapter)
		if adapterErr != nil {
			return nil, nil, "", fmt.Errorf("initialize local context providers: %w", adapterErr)
		}
	}
	serviceOptions := application.ServiceOptions{
		BuildIdentity:    "argus-" + version,
		WorkerID:         "argus-local-cli",
		ContextProviders: providerExecutor,
	}
	workloads, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		return nil, nil, "", fmt.Errorf(
			"initialize run-level scheduling coordinator: %w",
			err,
		)
	}
	serviceOptions.Workloads = workloads
	if configState != "" {
		configProvider, _, configErr := openConfigRepository(configState)
		if configErr != nil {
			return nil, nil, "", configErr
		}
		serviceOptions.ConfigProvider = configProvider
	}
	service, err := application.NewService(source, repository, serviceOptions)
	if err != nil {
		return nil, nil, "", fmt.Errorf("initialize application service: %w", err)
	}
	return repository, service, store.Root(), nil
}

type unavailableTargetSource struct{}

func (unavailableTargetSource) MaterializeDiff(
	context.Context,
	gitadapter.Request,
) (gitadapter.Result, error) {
	return gitadapter.Result{}, fmt.Errorf("Git target capture is unavailable for this command")
}

func (unavailableTargetSource) CaptureRevision(
	context.Context,
	gitadapter.RevisionRequest,
) (gitadapter.RevisionCapture, error) {
	return gitadapter.RevisionCapture{},
		fmt.Errorf("Git target capture is unavailable for this command")
}

func (unavailableTargetSource) RevalidateRevision(
	context.Context,
	string,
	string,
	string,
) (gitadapter.RevisionRevalidation, error) {
	return gitadapter.RevisionRevalidation{},
		fmt.Errorf("Git target capture is unavailable for this command")
}

func (unavailableTargetSource) ListFilesAtCommit(
	context.Context,
	string,
	string,
	[]string,
	[]string,
	int,
) (gitadapter.RevisionFileList, error) {
	return gitadapter.RevisionFileList{},
		fmt.Errorf("Git target capture is unavailable for this command")
}

func (unavailableTargetSource) ReadFileAtCommit(
	context.Context,
	string,
	string,
	string,
	int64,
) (gitadapter.FileContent, error) {
	return gitadapter.FileContent{}, fmt.Errorf("Git target capture is unavailable for this command")
}

func resolveStorePath(requested string) (string, error) {
	path := requested
	if path == "" {
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve user config directory: %w", err)
		}
		path = filepath.Join(configDirectory, "argus", "local-store")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve store path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func rejectStoreInsideRepository(requestedStore string, repositoryPath string) error {
	storePath, err := resolveStorePath(requestedStore)
	if err != nil {
		return err
	}
	canonicalRepository, err := canonicalPathForContainment(repositoryPath)
	if err != nil {
		return fmt.Errorf("resolve review repository: %w", err)
	}
	canonicalStore, err := canonicalPathForContainment(storePath)
	if err != nil {
		return fmt.Errorf("resolve local store: %w", err)
	}
	relative, err := filepath.Rel(canonicalRepository, canonicalStore)
	if err != nil {
		return fmt.Errorf("compare repository and store paths: %w", err)
	}
	if relative == "." || relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf(
			"local store %q must be outside reviewed repository %q to avoid changing the target working tree",
			storePath,
			repositoryPath,
		)
	}
	return nil
}

func canonicalPathForContainment(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	current := absolute
	missing := make([]string, 0)
	for {
		canonical, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				canonical = filepath.Join(canonical, missing[index])
			}
			return filepath.Clean(canonical), nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evalErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func commandFlagError(command string, err error) error {
	commandUsage, exists := subcommandUsage(command)
	if !exists {
		commandUsage = "usage: argus " + command + " --help"
	}
	return fmt.Errorf("%s flags: %w\n%s", command, err, commandUsage)
}

func subcommandUsage(command string) (string, bool) {
	usages := map[string]string{
		"version":  "usage: argus version",
		"validate": "usage: argus validate review-spec <file>",
		"config":   configUsage,
		"review": "usage: argus review --repo <absolute-path> " +
			"--mode <diff|selection|scope> <mode-specific flags>\n" +
			"selection selectors (exactly one): " +
			"--start-line <n> --end-line <n> | " +
			"--range <START:END> [--range <START:END>...] | " +
			"--symbol-language go --symbol-kind <function|method|type> " +
			"--symbol-qualified-name <name>\n" +
			"[--config-state-dir <absolute-dir>] [--store <dir>] [--json]",
		"replay": "usage: argus replay --run <id> --from <stage> " +
			"[--change <variable> --variant-config-bundle <absolute-json>] " +
			"[--config-state-dir <absolute-dir>] [--store <dir>] [--json]",
		"compare":     "usage: argus compare --baseline <id> --variant <id> [--store <dir>] [--json]",
		"lineage":     lineageUsage,
		"history":     "usage: argus history [--limit <n>] [--store <dir>] [--json]",
		"show":        "usage: argus show --run <id> [--store <dir>] [--json]",
		"candidate":   candidateUsage,
		"finding":     "usage: argus finding show --store <absolute-dir> --run <id> --finding <id> [--json]",
		"decision":    decisionUsage,
		"publication": publicationUsage,
		"artifact":    artifactUsage,
		"workload":    workloadUsage,
		"feedback": "usage: argus feedback record --store <absolute-dir> " +
			"--input <absolute-json> [--json]",
		"outcome": "usage: argus outcome record --store <absolute-dir> " +
			"--input <absolute-json> [--json]",
		"evaluation":   evaluationUsage,
		"calibration":  calibrationUsage,
		"training":     trainingUsage,
		"promotion":    promotionUsage,
		"dashboard":    dashboardUsage,
		"agent-review": agentReviewUsage,
		"api":          apiUsage,
	}
	value, exists := usages[command]
	return value, exists
}

func validReplayStage(stage string) bool {
	switch reviewcore.StageName(stage) {
	case reviewcore.StageMaterializeTarget,
		reviewcore.StagePlanContext,
		reviewcore.StageDetect,
		reviewcore.StageNormalize,
		reviewcore.StageVerify,
		reviewcore.StageAdjudicate,
		reviewcore.StageReport,
		reviewcore.StagePublish,
		reviewcore.StageCaptureFeedback,
		reviewcore.StageExportEvaluation:
		return true
	default:
		return false
	}
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "|", `\|`)
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return value
}
