package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"argus.local/argus/internal/agentadapter"
	"argus.local/argus/internal/agentcomponentrepo"
	"argus.local/argus/internal/agentshadowworker"
	"argus.local/argus/internal/application"
	"argus.local/argus/internal/artifactrepo"
	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/contextprovider"
	"argus.local/argus/internal/execution"
	"argus.local/argus/internal/formalevidence"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/hailixexecution"
	"argus.local/argus/internal/piexecution"
	"argus.local/argus/internal/pireviewmap"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/scheduling"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const formalAgentReviewUsage = `usage:
  argus agent-review formal bootstrap --store <absolute-dir> --config-state-dir <absolute-dir> --source-run <id> --idempotency-key <key> --at <RFC3339-UTC> --node <absolute-file> --worker-script <absolute-file> --provider-profile <deepseek-anthropic-env|anthropic-official> --model <id> [--normalization-revision <v0|v1|v2>] [--knowledge <absolute-md>...] [budget flags] [--json]
  argus agent-review formal run --store <absolute-dir> --config-state-dir <absolute-dir> --source-run <id> --idempotency-key <key> --node <absolute-file> --worker-script <absolute-file> --provider-profile <deepseek-anthropic-env|anthropic-official> --model <id> --input-micros-per-million <n> --output-micros-per-million <n> --max-bytes-per-input-token <n> [--normalization-revision <v0|v1|v2>] [--knowledge <absolute-md>...] [budget flags] [--json]
  argus agent-review formal replay --store <absolute-dir> --source-formal-run <id> --idempotency-key <key> --node <absolute-file> --worker-script <absolute-file> --provider-profile <deepseek-anthropic-env|anthropic-official> --model <id> --input-micros-per-million <n> --output-micros-per-million <n> --max-bytes-per-input-token <n> [--knowledge <absolute-md>...] [--change budget --timeout-ms <n> | --change model --at <RFC3339-UTC> | --change prompt --prompt-bundle <absolute-json> --at <RFC3339-UTC> | --change skill_pack --review-skill <absolute-md>... --at <RFC3339-UTC> | --change knowledge_pack --knowledge <absolute-md>... --at <RFC3339-UTC> | --change rule_pack --rule-pack <absolute-json> --at <RFC3339-UTC> | --change workflow --workflow-definition <absolute-json> --at <RFC3339-UTC> | --change index --context-provider <repository_search|go_ast|go_dependencies|go_compile>... --at <RFC3339-UTC> | --change filter_policy --finding-governance <absolute-json> --at <RFC3339-UTC>] [--json]
  argus agent-review formal show --store <absolute-dir> --formal-run <id> [--json]
  argus agent-review formal normalize-preview --store <absolute-dir> --formal-run <id> --json
  argus agent-review formal normalize-compare --store <absolute-dir> --comparison-id <id> --case <case-id>=<formal-run-id> [--case ...] --at <RFC3339-UTC> --json
  argus agent-review formal evidence read --store <absolute-dir> --formal-run <id> --request-id <id> --actor <id> --purpose <local_debug|evaluation_replay|incident_investigation> --at <RFC3339-UTC> --acknowledge-sensitive-output --json
  argus agent-review formal evidence revoke --store <absolute-dir> --formal-run <id> --idempotency-key <id> --actor <id> --reason <text> --at <RFC3339-UTC> [--json]

Bootstrap publishes exact subject-bound runtime/components and one lifecycle-governed formal config revision. It never reads or persists provider credentials.

Run and replay default to --execution-backend local-pi. hailix-http additionally requires --hailix-base-url plus pinned capability/callback verifier id, revision, and sha256 flags. Its request-time bearer credential is read only from ARGUS_HAILIX_BEARER_TOKEN and ARGUS_HAILIX_CREDENTIAL_REVISION.`

const (
	formalExecutionBackendLocalPi    = "local-pi"
	formalExecutionBackendHailixHTTP = "hailix-http"
)

type formalExecutionTransportFlags struct {
	Backend                      string
	HailixBaseURL                string
	HailixCapabilityVerifierID   string
	HailixCapabilityVerifierRev  string
	HailixCapabilityVerifierHash string
	HailixCallbackVerifierID     string
	HailixCallbackVerifierRev    string
	HailixCallbackVerifierHash   string
}

type formalAgentBootstrapFlags struct {
	store            string
	configState      string
	sourceRun        string
	idempotencyKey   string
	at               string
	node             string
	workerScript     string
	providerProfile  string
	model            string
	knowledge        stringList
	json             bool
	options          formalreview.LocalPiBootstrapOptions
	componentVariant *formalreview.LocalPiComponentVariant
}

type formalAgentRunFlags struct {
	formalAgentBootstrapFlags
	pricing   piexecution.PricingCeiling
	transport formalExecutionTransportFlags
}

type formalAgentReplayFlags struct {
	store                   string
	sourceFormalRun         string
	idempotencyKey          string
	node                    string
	workerScript            string
	providerProfile         string
	model                   string
	promptBundle            string
	rulePack                string
	workflowDefinition      string
	findingGovernance       string
	contextProviders        contextProviderList
	indexContextProviders   []reviewconfig.ContextProviderDefinition
	reviewSkills            stringList
	knowledge               stringList
	change                  runmodel.ReplayVariable
	timeoutMS               int64
	at                      time.Time
	json                    bool
	options                 formalreview.LocalPiBootstrapOptions
	pricing                 piexecution.PricingCeiling
	transport               formalExecutionTransportFlags
	componentVariant        *formalreview.LocalPiComponentVariant
	rulePackPolicy          *reviewconfig.RulePack
	workflowPolicy          *workflow.Definition
	findingGovernancePolicy *reviewconfig.FindingGovernancePolicy
}

func registerFormalExecutionTransportFlags(
	flags *flag.FlagSet,
	options *formalExecutionTransportFlags,
) {
	flags.StringVar(
		&options.Backend,
		"execution-backend",
		formalExecutionBackendLocalPi,
		"formal execution backend (local-pi or hailix-http)",
	)
	flags.StringVar(&options.HailixBaseURL, "hailix-base-url", "", "clean Hailix platform-execution base URL")
	flags.StringVar(&options.HailixCapabilityVerifierID, "hailix-capability-verifier-id", "", "pinned Hailix capability verifier ID")
	flags.StringVar(&options.HailixCapabilityVerifierRev, "hailix-capability-verifier-revision", "", "pinned Hailix capability verifier revision")
	flags.StringVar(&options.HailixCapabilityVerifierHash, "hailix-capability-verifier-sha256", "", "pinned Hailix capability verifier SHA-256")
	flags.StringVar(&options.HailixCallbackVerifierID, "hailix-callback-verifier-id", "", "pinned Hailix callback verifier ID")
	flags.StringVar(&options.HailixCallbackVerifierRev, "hailix-callback-verifier-revision", "", "pinned Hailix callback verifier revision")
	flags.StringVar(&options.HailixCallbackVerifierHash, "hailix-callback-verifier-sha256", "", "pinned Hailix callback verifier SHA-256")
}

func (options formalExecutionTransportFlags) validate() error {
	return options.validateWithPrefix("")
}

func (options formalExecutionTransportFlags) validateWithPrefix(prefix string) error {
	flagName := func(name string) string { return "--" + prefix + name }
	hailixValues := map[string]string{
		flagName("hailix-base-url"):                     options.HailixBaseURL,
		flagName("hailix-capability-verifier-id"):       options.HailixCapabilityVerifierID,
		flagName("hailix-capability-verifier-revision"): options.HailixCapabilityVerifierRev,
		flagName("hailix-capability-verifier-sha256"):   options.HailixCapabilityVerifierHash,
		flagName("hailix-callback-verifier-id"):         options.HailixCallbackVerifierID,
		flagName("hailix-callback-verifier-revision"):   options.HailixCallbackVerifierRev,
		flagName("hailix-callback-verifier-sha256"):     options.HailixCallbackVerifierHash,
	}
	switch options.Backend {
	case "", formalExecutionBackendLocalPi:
		for name, value := range hailixValues {
			if value != "" {
				return fmt.Errorf("%s is valid only with %s hailix-http", name, flagName("execution-backend"))
			}
		}
		return nil
	case formalExecutionBackendHailixHTTP:
		for name, value := range hailixValues {
			if value == "" {
				return fmt.Errorf("%s is required with %s hailix-http", name, flagName("execution-backend"))
			}
		}
		return nil
	default:
		return fmt.Errorf("%s must be local-pi or hailix-http", flagName("execution-backend"))
	}
}

// formalExecutionBackend keeps the governed review composition independent
// from one execution transport. nil selects the local Pi implementation;
// tests and future composition roots may inject one subject-bound Hailix
// adapter implementing the same four application boundaries.
type formalExecutionBackend struct {
	Capabilities application.AgentExecutorCapabilityResolver
	Platform     execution.PlatformPort
	Executor     application.FormalAgentStageExecutor
	Callbacks    application.AgentStageResultCallbackVerifier
}

func (backend *formalExecutionBackend) validate() error {
	if backend == nil {
		return nil
	}
	if backend.Capabilities == nil || backend.Platform == nil || backend.Executor == nil ||
		backend.Callbacks == nil {
		return fmt.Errorf("formal execution backend must provide capability, platform, executor, and callback ports")
	}
	return nil
}

func buildConfiguredFormalExecutionBackend(
	ctx context.Context,
	options formalExecutionTransportFlags,
	subject application.AgentPlanningSubject,
) (*formalExecutionBackend, error) {
	if options.Backend == "" || options.Backend == formalExecutionBackendLocalPi {
		return nil, nil
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	credentials := hailixexecution.NewEnvironmentCredentialSource()
	if _, err := credentials.Resolve(ctx, hailixexecution.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}); err != nil {
		return nil, fmt.Errorf("preflight Hailix request-time credential: %w", err)
	}
	client, err := hailixexecution.NewHTTPClient(hailixexecution.HTTPClientConfig{
		BaseURL: options.HailixBaseURL, Credentials: credentials,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize Hailix HTTP transport: %w", err)
	}
	adapter, err := hailixexecution.New(client, subject, hailixexecution.TrustConfig{
		CapabilityVerifier: hailixexecution.VerifierRef{
			ID: options.HailixCapabilityVerifierID, Revision: options.HailixCapabilityVerifierRev,
			SHA256: options.HailixCapabilityVerifierHash,
		},
		CallbackVerifier: hailixexecution.VerifierRef{
			ID: options.HailixCallbackVerifierID, Revision: options.HailixCallbackVerifierRev,
			SHA256: options.HailixCallbackVerifierHash,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize Hailix execution adapter: %w", err)
	}
	return &formalExecutionBackend{
		Capabilities: adapter, Platform: adapter, Executor: adapter, Callbacks: adapter,
	}, nil
}

type formalReplayExecution struct {
	Variable                runmodel.ReplayVariable
	TimeoutMS               int64
	CreatedAt               time.Time
	FindingGovernancePolicy *reviewconfig.FindingGovernancePolicy
	RulePackPolicy          *reviewconfig.RulePack
	WorkflowDefinition      *workflow.Definition
	IndexContextProviders   []reviewconfig.ContextProviderDefinition
}

type formalAgentShowFlags struct {
	store     string
	formalRun string
	json      bool
}

type formalNormalizationPreviewOutput struct {
	Preview   pireviewmap.NormalizationPreview `json:"preview"`
	RawRef    runmodel.ArtifactRef             `json:"raw_candidate_collection_ref"`
	StorePath string                           `json:"store_path"`
}

type formalNormalizationComparisonFlags struct {
	store        string
	comparisonID string
	cases        stringList
	at           time.Time
	json         bool
}

type formalNormalizationComparisonOutput struct {
	Comparison    pireviewmap.NormalizationPolicyComparison `json:"comparison"`
	ComparisonRef runmodel.ArtifactRef                      `json:"comparison_ref"`
	StorePath     string                                    `json:"store_path"`
}

type formalAgentBootstrapOutput struct {
	SourceRunID            string                           `json:"source_run_id"`
	ConfigState            string                           `json:"config_state_dir"`
	StorePath              string                           `json:"store_path"`
	ConfigID               string                           `json:"config_id"`
	ConfigRevision         string                           `json:"config_revision"`
	Manifest               any                              `json:"runtime_manifest"`
	Components             []contractsv1alpha1.VersionedRef `json:"components"`
	RuntimeFileManifestRef runmodel.ArtifactRef             `json:"runtime_file_manifest_ref"`
}

type formalAgentRunOutput struct {
	FormalRunID            string                                         `json:"formal_run_id"`
	SourceRunID            string                                         `json:"source_run_id,omitempty"`
	ExecutionID            string                                         `json:"execution_id"`
	Status                 contractsv1alpha1.StageExecutionStatus         `json:"status"`
	Hypotheses             *contractsv1alpha1.ReviewHypothesisSet         `json:"hypotheses,omitempty"`
	Failure                *contractsv1alpha1.StageExecutionFailure       `json:"failure,omitempty"`
	CandidateSet           *contractsv1alpha1.GovernedCandidateSet        `json:"candidate_set,omitempty"`
	CandidateSetRef        *runmodel.ArtifactRef                          `json:"candidate_set_ref,omitempty"`
	VerificationLedger     *contractsv1alpha1.CandidateVerificationLedger `json:"verification_ledger,omitempty"`
	VerificationLedgerRef  *runmodel.ArtifactRef                          `json:"verification_ledger_ref,omitempty"`
	CalibrationLedger      *contractsv1alpha1.FindingCalibrationLedger    `json:"calibration_ledger,omitempty"`
	CalibrationLedgerRef   *runmodel.ArtifactRef                          `json:"calibration_ledger_ref,omitempty"`
	SuppressionLedger      *contractsv1alpha1.FindingSuppressionLedger    `json:"suppression_ledger,omitempty"`
	SuppressionLedgerRef   *runmodel.ArtifactRef                          `json:"suppression_ledger_ref,omitempty"`
	Report                 *contractsv1alpha1.GovernedReviewReport        `json:"report,omitempty"`
	ReportRef              *runmodel.ArtifactRef                          `json:"report_ref,omitempty"`
	MarkdownReportRef      *runmodel.ArtifactRef                          `json:"markdown_report_ref,omitempty"`
	RuntimeFileManifestRef *runmodel.ArtifactRef                          `json:"runtime_file_manifest_ref,omitempty"`
	TaskEvidence           *formalevidence.Summary                        `json:"task_evidence,omitempty"`
	ReviewRun              *runmodel.ReviewRun                            `json:"review_run,omitempty"`
	FinalRunRef            *runmodel.ArtifactRef                          `json:"final_run_ref,omitempty"`
	TerminalAuthority      string                                         `json:"terminal_authority"`
	WorkloadState          scheduling.WorkloadState                       `json:"workload_state"`
	RecoveredDispatch      bool                                           `json:"recovered_dispatch"`
	RecoveredAdmission     bool                                           `json:"recovered_admission"`
	RecoveredFinalRun      bool                                           `json:"recovered_final_run"`
	StorePath              string                                         `json:"store_path"`
}

func runAgentReviewFormal(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s", formalAgentReviewUsage)
	}
	switch arguments[0] {
	case "bootstrap":
		options, err := parseFormalAgentBootstrapFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review formal bootstrap flags: %w\n%s", err, formalAgentReviewUsage)
		}
		return executeFormalAgentBootstrap(ctx, options, stdout)
	case "run":
		options, err := parseFormalAgentRunFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review formal run flags: %w\n%s", err, formalAgentReviewUsage)
		}
		return executeFormalAgentRunWithRunner(
			ctx, options, stdout, agentshadowworker.NewSubprocessRunner(),
		)
	case "replay":
		options, err := parseFormalAgentReplayFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review formal replay flags: %w\n%s", err, formalAgentReviewUsage)
		}
		return executeFormalAgentReplayWithRunner(
			ctx, options, stdout, agentshadowworker.NewSubprocessRunner(),
		)
	case "show":
		options, err := parseFormalAgentShowFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review formal show flags: %w\n%s", err, formalAgentReviewUsage)
		}
		return executeFormalAgentShow(ctx, options, stdout)
	case "normalize-preview":
		options, err := parseFormalAgentShowFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review formal normalize-preview flags: %w\n%s", err, formalAgentReviewUsage)
		}
		if !options.json {
			return fmt.Errorf("agent-review formal normalize-preview requires --json")
		}
		return executeFormalNormalizationPreview(options, stdout)
	case "normalize-compare":
		options, err := parseFormalNormalizationComparisonFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review formal normalize-compare flags: %w\n%s", err, formalAgentReviewUsage)
		}
		return executeFormalNormalizationComparison(options, stdout)
	case "evidence":
		return runFormalAgentEvidence(ctx, arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown agent-review formal action %q\n%s", arguments[0], formalAgentReviewUsage)
	}
}

func parseFormalNormalizationComparisonFlags(arguments []string) (formalNormalizationComparisonFlags, error) {
	var options formalNormalizationComparisonFlags
	at := ""
	flags := newFlagSet("agent-review formal normalize-compare")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.comparisonID, "comparison-id", "", "stable diagnostic comparison ID")
	flags.Var(&options.cases, "case", "case-id=formal-run-id; repeatable")
	flags.StringVar(&at, "at", "", "comparison creation time in RFC3339 UTC")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return formalNormalizationComparisonFlags{}, err
	}
	if flags.NArg() != 0 {
		return formalNormalizationComparisonFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if options.store == "" || options.comparisonID == "" || len(options.cases) == 0 || at == "" {
		return formalNormalizationComparisonFlags{}, fmt.Errorf("--store, --comparison-id, repeatable --case, and --at are required")
	}
	if !options.json {
		return formalNormalizationComparisonFlags{}, fmt.Errorf("normalize-compare requires --json")
	}
	if !filepath.IsAbs(options.store) || filepath.Clean(options.store) != options.store {
		return formalNormalizationComparisonFlags{}, fmt.Errorf("--store must be a clean absolute path")
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || parsed.Location() != time.UTC {
		return formalNormalizationComparisonFlags{}, fmt.Errorf("--at must be RFC3339 UTC")
	}
	options.at = parsed
	return options, nil
}

func parseFormalAgentReplayFlags(arguments []string) (formalAgentReplayFlags, error) {
	var options formalAgentReplayFlags
	flags := newFlagSet("agent-review formal replay")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.sourceFormalRun, "source-formal-run", "", "succeeded formal ReviewRun ID")
	flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "stable exact replay key")
	flags.StringVar(&options.node, "node", "", "absolute Node.js executable")
	flags.StringVar(&options.workerScript, "worker-script", "", "absolute Pi worker script")
	flags.StringVar(&options.providerProfile, "provider-profile", "", "provider profile")
	flags.StringVar(&options.model, "model", "", "provider model ID")
	change := ""
	at := ""
	flags.StringVar(&change, "change", "", "one formal replay variable (budget, model, prompt, skill_pack, knowledge_pack, rule_pack, workflow, index, or filter_policy)")
	flags.Int64Var(&options.timeoutMS, "timeout-ms", 0, "variant stage and Pi timeout in milliseconds")
	flags.StringVar(&at, "at", "", "model variant creation time in RFC3339 UTC")
	flags.StringVar(&options.promptBundle, "prompt-bundle", "", "absolute governed prompt bundle JSON")
	flags.StringVar(&options.rulePack, "rule-pack", "", "absolute sealed rule pack JSON")
	flags.StringVar(&options.workflowDefinition, "workflow-definition", "", "absolute exact WorkflowDefinition JSON")
	flags.StringVar(&options.findingGovernance, "finding-governance", "", "absolute sealed finding governance policy JSON")
	flags.Var(&options.contextProviders, "context-provider", "ordered built-in context provider for index replay; repeatable")
	flags.Var(&options.reviewSkills, "review-skill", "absolute governed review skill Markdown; repeatable")
	flags.Var(&options.knowledge, "knowledge", "absolute governed knowledge Markdown; repeatable")
	flags.Uint64Var(&options.pricing.InputMicrosPerMillionTokens, "input-micros-per-million", 0, "governed worst-case input rate")
	flags.Uint64Var(&options.pricing.OutputMicrosPerMillionTokens, "output-micros-per-million", 0, "governed worst-case output rate")
	flags.Uint64Var(&options.pricing.MaximumBytesPerInputToken, "max-bytes-per-input-token", 0, "conservative input token conversion")
	registerFormalExecutionTransportFlags(flags, &options.transport)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return formalAgentReplayFlags{}, err
	}
	if flags.NArg() != 0 {
		return formalAgentReplayFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	for name, value := range map[string]string{
		"--store": options.store, "--source-formal-run": options.sourceFormalRun,
		"--idempotency-key": options.idempotencyKey, "--node": options.node,
		"--worker-script": options.workerScript, "--provider-profile": options.providerProfile,
		"--model": options.model,
	} {
		if value == "" {
			return formalAgentReplayFlags{}, fmt.Errorf("%s is required", name)
		}
	}
	for name, path := range map[string]string{
		"--store": options.store, "--node": options.node, "--worker-script": options.workerScript,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return formalAgentReplayFlags{}, fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	for _, path := range options.knowledge {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return formalAgentReplayFlags{}, fmt.Errorf("--knowledge must be a clean absolute path")
		}
	}
	for _, path := range options.reviewSkills {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return formalAgentReplayFlags{}, fmt.Errorf("--review-skill must be a clean absolute path")
		}
	}
	if options.pricing.InputMicrosPerMillionTokens == 0 ||
		options.pricing.OutputMicrosPerMillionTokens == 0 ||
		options.pricing.MaximumBytesPerInputToken == 0 {
		return formalAgentReplayFlags{}, fmt.Errorf("all governed pricing ceiling flags are required and positive")
	}
	if err := options.transport.validate(); err != nil {
		return formalAgentReplayFlags{}, err
	}
	if len(options.contextProviders) != 0 && change != string(runmodel.ReplayVariableIndex) {
		return formalAgentReplayFlags{}, fmt.Errorf("--context-provider is valid only with --change index")
	}
	if options.rulePack != "" && change != string(runmodel.ReplayVariableRulePack) {
		return formalAgentReplayFlags{}, fmt.Errorf("--rule-pack is valid only with --change rule_pack")
	}
	if options.workflowDefinition != "" && change != string(runmodel.ReplayVariableWorkflow) {
		return formalAgentReplayFlags{}, fmt.Errorf("--workflow-definition is valid only with --change workflow")
	}
	switch {
	case change == "" && options.timeoutMS == 0 && at == "" && options.promptBundle == "" && options.findingGovernance == "":
		options.change = runmodel.ReplayVariableNone
	case change == string(runmodel.ReplayVariableBudget) && options.timeoutMS > 0 && at == "" && options.promptBundle == "" && options.findingGovernance == "":
		options.change = runmodel.ReplayVariableBudget
	case change == string(runmodel.ReplayVariableModel) && options.timeoutMS == 0 && at != "" && options.promptBundle == "" && options.findingGovernance == "":
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for model replay")
		}
		options.change = runmodel.ReplayVariableModel
		options.at = parsed
	case change == string(runmodel.ReplayVariablePrompt) && options.timeoutMS == 0 && at != "" && options.promptBundle != "" && options.findingGovernance == "":
		if !filepath.IsAbs(options.promptBundle) || filepath.Clean(options.promptBundle) != options.promptBundle {
			return formalAgentReplayFlags{}, fmt.Errorf("--prompt-bundle must be a clean absolute path")
		}
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for prompt replay")
		}
		options.change = runmodel.ReplayVariablePrompt
		options.at = parsed
		options.options.PromptBundlePath = options.promptBundle
	case change == string(runmodel.ReplayVariableSkillPack) && options.timeoutMS == 0 && at != "" && options.promptBundle == "" && options.findingGovernance == "" && len(options.reviewSkills) > 0:
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for skill_pack replay")
		}
		options.change = runmodel.ReplayVariableSkillPack
		options.at = parsed
		options.options.ReviewSkillPaths = slices.Clone(options.reviewSkills)
	case change == string(runmodel.ReplayVariableKnowledgePack) && options.timeoutMS == 0 && at != "" && options.promptBundle == "" && options.findingGovernance == "" && len(options.knowledge) > 0:
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for knowledge_pack replay")
		}
		options.change = runmodel.ReplayVariableKnowledgePack
		options.at = parsed
	case change == string(runmodel.ReplayVariableRulePack) && options.timeoutMS == 0 && at != "" &&
		options.promptBundle == "" && options.findingGovernance == "" && options.rulePack != "" &&
		len(options.reviewSkills) == 0 && len(options.knowledge) == 0 && len(options.contextProviders) == 0:
		if !filepath.IsAbs(options.rulePack) || filepath.Clean(options.rulePack) != options.rulePack {
			return formalAgentReplayFlags{}, fmt.Errorf("--rule-pack must be a clean absolute path")
		}
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for rule_pack replay")
		}
		options.change = runmodel.ReplayVariableRulePack
		options.at = parsed
	case change == string(runmodel.ReplayVariableWorkflow) && options.timeoutMS == 0 && at != "" &&
		options.promptBundle == "" && options.findingGovernance == "" && options.rulePack == "" &&
		options.workflowDefinition != "" && len(options.reviewSkills) == 0 &&
		len(options.knowledge) == 0 && len(options.contextProviders) == 0:
		if !filepath.IsAbs(options.workflowDefinition) ||
			filepath.Clean(options.workflowDefinition) != options.workflowDefinition {
			return formalAgentReplayFlags{}, fmt.Errorf("--workflow-definition must be a clean absolute path")
		}
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for workflow replay")
		}
		options.change = runmodel.ReplayVariableWorkflow
		options.at = parsed
	case change == string(runmodel.ReplayVariableIndex) && options.timeoutMS == 0 && at != "" &&
		options.promptBundle == "" && options.findingGovernance == "" &&
		len(options.reviewSkills) == 0 && len(options.contextProviders) > 0:
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for index replay")
		}
		providers, err := application.InvocationContextProviderDefinitions(options.contextProviders)
		if err != nil {
			return formalAgentReplayFlags{}, err
		}
		options.change = runmodel.ReplayVariableIndex
		options.at = parsed
		options.indexContextProviders = providers
	case (change == string(runmodel.ReplayVariableFilterPolicy) ||
		change == string(runmodel.ReplayVariableFindingGovernance)) &&
		options.timeoutMS == 0 && at != "" &&
		options.promptBundle == "" && options.findingGovernance != "" &&
		len(options.reviewSkills) == 0 && len(options.knowledge) == 0:
		if !filepath.IsAbs(options.findingGovernance) || filepath.Clean(options.findingGovernance) != options.findingGovernance {
			return formalAgentReplayFlags{}, fmt.Errorf("--finding-governance must be a clean absolute path")
		}
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || parsed.Location() != time.UTC {
			return formalAgentReplayFlags{}, fmt.Errorf("--at must be an RFC3339 UTC timestamp for filter_policy replay")
		}
		options.change = runmodel.ReplayVariable(change)
		options.at = parsed
	default:
		return formalAgentReplayFlags{}, fmt.Errorf(
			"formal replay supports exact mode, --change budget with one positive --timeout-ms, --change model with --at, --change prompt with --prompt-bundle and --at, --change skill_pack with repeatable --review-skill and --at, --change knowledge_pack with repeatable --knowledge and --at, --change rule_pack with --rule-pack and --at, --change workflow with --workflow-definition and --at, --change index with repeatable --context-provider and --at, or --change filter_policy with --finding-governance and --at (legacy finding_governance remains accepted)",
		)
	}
	options.options.NodePath = options.node
	options.options.WorkerScript = options.workerScript
	options.options.ProviderProfile = options.providerProfile
	options.options.Model = options.model
	options.options.ReviewSkillPaths = slices.Clone(options.reviewSkills)
	options.options.KnowledgePaths = slices.Clone(options.knowledge)
	return options, nil
}

func parseFormalAgentShowFlags(arguments []string) (formalAgentShowFlags, error) {
	var options formalAgentShowFlags
	flags := newFlagSet("agent-review formal show")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.formalRun, "formal-run", "", "formal ReviewRun ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return formalAgentShowFlags{}, err
	}
	if flags.NArg() != 0 {
		return formalAgentShowFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if options.store == "" || options.formalRun == "" {
		return formalAgentShowFlags{}, fmt.Errorf("--store and --formal-run are required")
	}
	if !filepath.IsAbs(options.store) || filepath.Clean(options.store) != options.store {
		return formalAgentShowFlags{}, fmt.Errorf("--store must be a clean absolute path")
	}
	return options, nil
}

func parseFormalAgentRunFlags(arguments []string) (formalAgentRunFlags, error) {
	var options formalAgentRunFlags
	flags := newFlagSet("agent-review formal run")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.configState, "config-state-dir", "", "absolute config lifecycle state")
	flags.StringVar(&options.sourceRun, "source-run", "", "committed source ReviewRun ID")
	flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "stable formal run key")
	flags.StringVar(&options.node, "node", "", "absolute Node.js executable")
	flags.StringVar(&options.workerScript, "worker-script", "", "absolute Pi worker script")
	flags.StringVar(&options.providerProfile, "provider-profile", "", "provider profile")
	flags.StringVar(&options.model, "model", "", "provider model ID")
	flags.StringVar(
		&options.options.NormalizationRevision,
		"normalization-revision",
		contractsv1alpha1.CandidateNormalizationCurrentRevision,
		"exact candidate normalization revision (v0, v1, or v2)",
	)
	flags.Var(&options.knowledge, "knowledge", "absolute governed knowledge Markdown; repeatable")
	flags.IntVar(&options.options.MaxFiles, "max-files", 32, "maximum frozen files")
	flags.IntVar(&options.options.MaxGroups, "max-groups", 8, "maximum change groups")
	flags.IntVar(&options.options.MaxHypotheses, "max-hypotheses", 32, "maximum retained hypotheses")
	flags.IntVar(&options.options.MaxModelCalls, "max-model-calls", 96, "maximum provider turns")
	flags.IntVar(&options.options.MaxToolCalls, "max-tool-calls", 24, "maximum tools per task")
	flags.Int64Var(&options.options.MaxTargetBytes, "max-target-bytes", 4<<20, "maximum frozen target bytes")
	flags.Int64Var(&options.options.MaxGroupBytes, "max-group-bytes", 64<<10, "maximum group bytes")
	flags.Int64Var(&options.options.MaxOutputBytes, "max-output-bytes", 1<<20, "maximum output bytes")
	flags.Int64Var(&options.options.MaxOutputTokens, "max-output-tokens", 8192, "maximum output tokens per turn")
	flags.Int64Var(&options.options.MaxCostMicros, "max-cost-micros", 1_000_000, "maximum worst-case cost micros")
	flags.Int64Var(&options.options.TimeoutMS, "timeout-ms", 180_000, "formal stage timeout")
	flags.IntVar(&options.options.MaxConcurrency, "max-concurrency", 4, "maximum concurrent tasks")
	flags.IntVar(&options.options.MaxAttempts, "max-attempts", 1, "maximum authenticated formal attempts")
	flags.Uint64Var(&options.pricing.InputMicrosPerMillionTokens, "input-micros-per-million", 0, "governed worst-case input rate")
	flags.Uint64Var(&options.pricing.OutputMicrosPerMillionTokens, "output-micros-per-million", 0, "governed worst-case output rate")
	flags.Uint64Var(&options.pricing.MaximumBytesPerInputToken, "max-bytes-per-input-token", 0, "conservative input token conversion")
	registerFormalExecutionTransportFlags(flags, &options.transport)
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return formalAgentRunFlags{}, err
	}
	if flags.NArg() != 0 {
		return formalAgentRunFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	for name, value := range map[string]string{
		"--store": options.store, "--config-state-dir": options.configState,
		"--source-run": options.sourceRun, "--idempotency-key": options.idempotencyKey,
		"--node": options.node, "--worker-script": options.workerScript,
		"--provider-profile": options.providerProfile, "--model": options.model,
	} {
		if value == "" {
			return formalAgentRunFlags{}, fmt.Errorf("%s is required", name)
		}
	}
	for name, path := range map[string]string{
		"--store": options.store, "--config-state-dir": options.configState,
		"--node": options.node, "--worker-script": options.workerScript,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return formalAgentRunFlags{}, fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	for _, path := range options.knowledge {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return formalAgentRunFlags{}, fmt.Errorf("--knowledge must be a clean absolute path")
		}
	}
	if options.pricing.InputMicrosPerMillionTokens == 0 ||
		options.pricing.OutputMicrosPerMillionTokens == 0 ||
		options.pricing.MaximumBytesPerInputToken == 0 {
		return formalAgentRunFlags{}, fmt.Errorf("all governed pricing ceiling flags are required and positive")
	}
	if err := options.transport.validate(); err != nil {
		return formalAgentRunFlags{}, err
	}
	options.options.NodePath = options.node
	options.options.WorkerScript = options.workerScript
	options.options.ProviderProfile = options.providerProfile
	options.options.Model = options.model
	options.options.KnowledgePaths = slices.Clone(options.knowledge)
	return options, nil
}

func parseFormalAgentBootstrapFlags(arguments []string) (formalAgentBootstrapFlags, error) {
	var options formalAgentBootstrapFlags
	flags := newFlagSet("agent-review formal bootstrap")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.configState, "config-state-dir", "", "absolute config lifecycle state")
	flags.StringVar(&options.sourceRun, "source-run", "", "committed source ReviewRun ID")
	flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "stable bootstrap key")
	flags.StringVar(&options.at, "at", "", "immutable publication time in RFC3339")
	flags.StringVar(&options.node, "node", "", "absolute Node.js executable")
	flags.StringVar(&options.workerScript, "worker-script", "", "absolute Pi worker script")
	flags.StringVar(&options.providerProfile, "provider-profile", "", "provider profile")
	flags.StringVar(&options.model, "model", "", "provider model ID")
	flags.StringVar(
		&options.options.NormalizationRevision,
		"normalization-revision",
		contractsv1alpha1.CandidateNormalizationCurrentRevision,
		"exact candidate normalization revision (v0, v1, or v2)",
	)
	flags.Var(&options.knowledge, "knowledge", "absolute governed knowledge Markdown; repeatable")
	flags.IntVar(&options.options.MaxFiles, "max-files", 32, "maximum frozen files")
	flags.IntVar(&options.options.MaxGroups, "max-groups", 8, "maximum change groups")
	flags.IntVar(&options.options.MaxHypotheses, "max-hypotheses", 32, "maximum retained hypotheses")
	flags.IntVar(&options.options.MaxModelCalls, "max-model-calls", 96, "maximum provider turns")
	flags.IntVar(&options.options.MaxToolCalls, "max-tool-calls", 24, "maximum tools per task")
	flags.Int64Var(&options.options.MaxTargetBytes, "max-target-bytes", 4<<20, "maximum frozen target bytes")
	flags.Int64Var(&options.options.MaxGroupBytes, "max-group-bytes", 64<<10, "maximum group bytes")
	flags.Int64Var(&options.options.MaxOutputBytes, "max-output-bytes", 1<<20, "maximum output bytes")
	flags.Int64Var(&options.options.MaxOutputTokens, "max-output-tokens", 8192, "maximum output tokens per turn")
	flags.Int64Var(&options.options.MaxCostMicros, "max-cost-micros", 1_000_000, "maximum worst-case cost micros")
	flags.Int64Var(&options.options.TimeoutMS, "timeout-ms", 180_000, "formal stage timeout")
	flags.IntVar(&options.options.MaxConcurrency, "max-concurrency", 4, "maximum concurrent tasks")
	flags.IntVar(&options.options.MaxAttempts, "max-attempts", 1, "maximum authenticated formal attempts")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return formalAgentBootstrapFlags{}, err
	}
	if flags.NArg() != 0 {
		return formalAgentBootstrapFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	for name, value := range map[string]string{
		"--store": options.store, "--config-state-dir": options.configState,
		"--source-run": options.sourceRun, "--idempotency-key": options.idempotencyKey,
		"--at": options.at, "--node": options.node, "--worker-script": options.workerScript,
		"--provider-profile": options.providerProfile, "--model": options.model,
	} {
		if value == "" {
			return formalAgentBootstrapFlags{}, fmt.Errorf("%s is required", name)
		}
	}
	for name, path := range map[string]string{
		"--store": options.store, "--config-state-dir": options.configState,
		"--node": options.node, "--worker-script": options.workerScript,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return formalAgentBootstrapFlags{}, fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	for _, path := range options.knowledge {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return formalAgentBootstrapFlags{}, fmt.Errorf("--knowledge must be a clean absolute path")
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, options.at); err != nil {
		return formalAgentBootstrapFlags{}, fmt.Errorf("--at must be RFC3339: %w", err)
	}
	options.options.NodePath = options.node
	options.options.WorkerScript = options.workerScript
	options.options.ProviderProfile = options.providerProfile
	options.options.Model = options.model
	options.options.KnowledgePaths = slices.Clone(options.knowledge)
	return options, nil
}

func executeFormalAgentBootstrap(
	ctx context.Context,
	options formalAgentBootstrapFlags,
	stdout io.Writer,
) error {
	at, _ := time.Parse(time.RFC3339Nano, options.at)
	at = at.UTC()
	store, err := local.Open(options.store)
	if err != nil {
		return fmt.Errorf("open Argus store: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	sourceSnapshot, err := runs.ExecutionSnapshotForRun(options.sourceRun)
	if err != nil {
		return fmt.Errorf("load committed source snapshot: %w", err)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return fmt.Errorf("load committed source ReviewSpec: %w", err)
	}
	subject := agentcomponentrepo.Subject{
		TenantID: sourceSpec.TenantID, OrganizationID: "local",
		WorkspaceID:  sourceSpec.WorkspaceID,
		RepositoryID: sourceSpec.Repository.RepositoryID,
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(options.options)
	if err != nil {
		return err
	}
	runtimeEvidenceRef, err := persistFormalRuntimeEvidence(runs, bootstrap)
	if err != nil {
		return err
	}
	artifacts, err := artifactrepo.Open(store, nil)
	if err != nil {
		return err
	}
	components, err := agentcomponentrepo.New(store, artifacts, artifactrepo.DefaultAuthority)
	if err != nil {
		return err
	}
	componentRefs := make([]contractsv1alpha1.VersionedRef, 0, len(bootstrap.Publications))
	for _, publication := range bootstrap.Publications {
		publication.PublishedAt = at
		binding, publishErr := resolveOrPublishFormalComponent(
			ctx,
			components,
			subject,
			publication,
		)
		if publishErr != nil {
			return fmt.Errorf("publish component %s@%s: %w", publication.Ref.ID, publication.Ref.Revision, publishErr)
		}
		componentRefs = append(componentRefs, binding.Ref)
	}
	configStore, err := local.Open(options.configState)
	if err != nil {
		return fmt.Errorf("open config state: %w", err)
	}
	configs, err := configrepo.New(configStore)
	if err != nil {
		return err
	}
	mutation := func(suffix string, audit string) configrepo.Mutation {
		return configrepo.Mutation{
			IdempotencyKey: options.idempotencyKey + "-" + suffix,
			Actor:          "argus-local-pi-bootstrap", Audit: audit, At: at,
		}
	}
	if err := resolveOrPublishFormalConfig(ctx, configs, bootstrap.Revision, mutation); err != nil {
		return err
	}
	output := formalAgentBootstrapOutput{
		SourceRunID: options.sourceRun, ConfigState: configStore.Root(), StorePath: store.Root(),
		ConfigID: bootstrap.Revision.ID, ConfigRevision: bootstrap.Revision.Revision,
		Manifest: bootstrap.Manifest, Components: componentRefs,
		RuntimeFileManifestRef: runtimeEvidenceRef,
	}
	if options.json {
		return writeJSON(stdout, output)
	}
	_, err = fmt.Fprintf(stdout,
		"formal Pi bootstrap published config=%s@%s components=%d source_run=%s store=%s config_state=%s\n",
		output.ConfigID, output.ConfigRevision, len(output.Components), output.SourceRunID,
		output.StorePath, output.ConfigState,
	)
	return err
}

func resolveOrPublishFormalConfig(
	ctx context.Context,
	repository *configrepo.Repository,
	revision reviewconfig.Revision,
	mutation func(string, string) configrepo.Mutation,
) error {
	if repository == nil {
		return fmt.Errorf("formal config repository is required")
	}
	record, err := repository.Get(revision.ID, revision.Revision)
	if err == nil {
		digest, digestErr := reviewconfig.DigestRevision(revision)
		if digestErr != nil {
			return fmt.Errorf("digest formal config revision: %w", digestErr)
		}
		if record.SHA256 != digest || record.Revision.ID != revision.ID ||
			record.Revision.Revision != revision.Revision {
			return fmt.Errorf("formal config %s@%s exists with different content",
				revision.ID, revision.Revision)
		}
		if record.Status != configrepo.StatusPublished {
			return fmt.Errorf("formal config %s@%s exists with status %s, want published",
				revision.ID, revision.Revision, record.Status)
		}
		return nil
	}
	if !errors.Is(err, configrepo.ErrNotFound) {
		return fmt.Errorf("resolve formal config revision: %w", err)
	}
	if _, err := repository.Create(ctx, revision,
		mutation("create", "create formal local Pi config")); err != nil {
		return fmt.Errorf("create formal config revision: %w", err)
	}
	if _, err := repository.ValidateRevision(ctx, revision.ID, revision.Revision,
		mutation("validate", "validate formal local Pi config")); err != nil {
		return fmt.Errorf("validate formal config revision: %w", err)
	}
	if _, err := repository.Publish(ctx, revision.ID, revision.Revision,
		configrepo.Rollout{Percentage: 100},
		mutation("publish", "publish formal local Pi config")); err != nil {
		return fmt.Errorf("publish formal config revision: %w", err)
	}
	return nil
}

func executeFormalAgentShow(
	ctx context.Context,
	options formalAgentShowFlags,
	stdout io.Writer,
) error {
	store, err := local.Open(options.store)
	if err != nil {
		return fmt.Errorf("open Argus store: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	if committed, loadErr := runs.LoadRun(options.formalRun); loadErr == nil &&
		committed.Kind == runmodel.RunKindReplay &&
		committed.ReplayVariable.IsFilterPolicy() {
		output, outputErr := loadFindingGovernanceReplayOutput(
			runs, committed, store.Root(), true,
		)
		if outputErr != nil {
			return outputErr
		}
		return writeFormalAgentRunOutput(stdout, options.json, output)
	} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return fmt.Errorf("load formal ReviewRun terminal: %w", loadErr)
	}
	snapshot, err := runs.ExecutionSnapshotForRun(options.formalRun)
	if err != nil {
		return fmt.Errorf("load formal execution snapshot: %w", err)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return fmt.Errorf("load formal ReviewSpec: %w", err)
	}
	subject := application.AgentPlanningSubject{
		TenantID: spec.TenantID, OrganizationID: "local",
		WorkspaceID: spec.WorkspaceID, RepositoryID: spec.Repository.RepositoryID,
	}
	artifacts, err := artifactrepo.Open(store, nil)
	if err != nil {
		return err
	}
	agentRepository, err := agentadapter.New(runs, artifacts, artifactrepo.DefaultAuthority)
	if err != nil {
		return err
	}
	workloads, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		return err
	}
	record, err := workloads.Get(options.formalRun + "-workload")
	if err != nil {
		return fmt.Errorf("load formal workload: %w", err)
	}
	output, _, err := recoverFormalAgentRunOutput(
		ctx, agentRepository, runs, subject, record, options.formalRun, "", store.Root(),
	)
	if err != nil {
		return err
	}
	if len(snapshot.RuntimeEvidenceRefs) != 1 {
		return fmt.Errorf("formal execution snapshot has no exact runtime file manifest")
	}
	output.RuntimeFileManifestRef = &snapshot.RuntimeEvidenceRefs[0]
	if committed, loadErr := runs.LoadRun(options.formalRun); loadErr == nil {
		ref, refErr := runs.CommittedRunRef(options.formalRun)
		if refErr != nil {
			return fmt.Errorf("load formal final run ref: %w", refErr)
		}
		output.ReviewRun = &committed
		output.FinalRunRef = &ref
		output.RecoveredFinalRun = true
		if err := attachFormalTaskEvidenceSummary(ctx, artifacts, runs, &output); err != nil {
			return err
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return fmt.Errorf("load formal ReviewRun terminal: %w", loadErr)
	}
	return writeFormalAgentRunOutput(stdout, options.json, output)
}

func executeFormalNormalizationPreview(
	options formalAgentShowFlags,
	stdout io.Writer,
) error {
	store, err := local.Open(options.store)
	if err != nil {
		return fmt.Errorf("open Argus store: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	run, err := runs.LoadRun(options.formalRun)
	if err != nil {
		return fmt.Errorf("load committed formal ReviewRun: %w", err)
	}
	if run.Status != runmodel.RunStatusSucceeded || run.RawCandidateCollectionRef == nil {
		return fmt.Errorf("normalization preview requires a succeeded formal run with raw candidates")
	}
	rawBytes, err := runs.ReadArtifact(*run.RawCandidateCollectionRef)
	if err != nil {
		return fmt.Errorf("read committed raw candidate collection: %w", err)
	}
	raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawBytes)
	if err != nil {
		return err
	}
	if raw.ReviewRunID != run.RunID {
		return fmt.Errorf("raw candidate collection escaped the committed formal run")
	}
	preview, err := pireviewmap.PreviewNormalization(raw)
	if err != nil {
		return err
	}
	return writeJSON(stdout, formalNormalizationPreviewOutput{
		Preview: preview, RawRef: *run.RawCandidateCollectionRef, StorePath: store.Root(),
	})
}

func executeFormalNormalizationComparison(
	options formalNormalizationComparisonFlags,
	stdout io.Writer,
) error {
	store, err := local.Open(options.store)
	if err != nil {
		return fmt.Errorf("open Argus store: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	inputs := make([]pireviewmap.NormalizationPolicyComparisonInput, 0, len(options.cases))
	seenCases := make(map[string]struct{}, len(options.cases))
	for _, binding := range options.cases {
		caseID, formalRunID, ok := strings.Cut(binding, "=")
		if !ok || caseID == "" || formalRunID == "" || strings.Contains(formalRunID, "=") {
			return fmt.Errorf("--case %q must be case-id=formal-run-id", binding)
		}
		if _, duplicate := seenCases[caseID]; duplicate {
			return fmt.Errorf("--case id %q is duplicated", caseID)
		}
		seenCases[caseID] = struct{}{}
		run, err := runs.LoadRun(formalRunID)
		if err != nil {
			return fmt.Errorf("load committed formal ReviewRun %q: %w", formalRunID, err)
		}
		if run.RunID != formalRunID || run.Status != runmodel.RunStatusSucceeded || run.RawCandidateCollectionRef == nil {
			return fmt.Errorf("case %q requires a succeeded formal run with raw candidates", caseID)
		}
		runRef, err := runs.CommittedRunRef(formalRunID)
		if err != nil {
			return fmt.Errorf("resolve committed formal ReviewRun %q: %w", formalRunID, err)
		}
		rawBytes, err := runs.ReadArtifact(*run.RawCandidateCollectionRef)
		if err != nil {
			return fmt.Errorf("read raw candidate collection for %q: %w", formalRunID, err)
		}
		raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawBytes)
		if err != nil {
			return err
		}
		if raw.ReviewRunID != run.RunID || raw.TargetDigest == "" {
			return fmt.Errorf("raw candidate collection escaped committed formal run %q", formalRunID)
		}
		inputs = append(inputs, pireviewmap.NormalizationPolicyComparisonInput{
			CaseID: caseID, SourceReviewRunRef: runRef,
			RawCandidateCollectionRef: *run.RawCandidateCollectionRef,
			RawCandidates:             raw,
		})
	}
	comparison, err := pireviewmap.BuildNormalizationPolicyComparison(options.comparisonID, inputs, options.at)
	if err != nil {
		return err
	}
	ref, err := runs.PutJSONArtifact(pireviewmap.NormalizationPolicyComparisonContract, comparison)
	if err != nil {
		return fmt.Errorf("persist normalization policy comparison: %w", err)
	}
	return writeJSON(stdout, formalNormalizationComparisonOutput{
		Comparison: comparison, ComparisonRef: ref, StorePath: store.Root(),
	})
}

func executeFormalAgentRunWithRunner(
	ctx context.Context,
	options formalAgentRunFlags,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	return executeFormalAgentRunMode(ctx, options, stdout, runner, nil, nil, nil, nil, nil)
}

func executeFormalAgentReplayWithRunner(
	ctx context.Context,
	options formalAgentReplayFlags,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	var workflowPolicy *workflow.Definition
	if options.change == runmodel.ReplayVariableWorkflow {
		if options.workflowPolicy != nil {
			definition := *options.workflowPolicy
			definition.Stages = slices.Clone(options.workflowPolicy.Stages)
			workflowPolicy = &definition
		} else {
			data, err := os.ReadFile(options.workflowDefinition)
			if err != nil {
				return fmt.Errorf("read workflow replay definition: %w", err)
			}
			definition, err := workflow.DecodeDefinition(data)
			if err != nil {
				return fmt.Errorf("decode workflow replay definition: %w", err)
			}
			workflowPolicy = &definition
		}
	}
	var rulePackPolicy *reviewconfig.RulePack
	if options.change == runmodel.ReplayVariableRulePack {
		if options.rulePackPolicy != nil {
			pack := *options.rulePackPolicy
			rulePackPolicy = &pack
		} else {
			data, err := os.ReadFile(options.rulePack)
			if err != nil {
				return fmt.Errorf("read rule_pack replay policy: %w", err)
			}
			pack, err := reviewconfig.DecodeRulePack(data)
			if err != nil {
				return fmt.Errorf("decode rule_pack replay policy: %w", err)
			}
			rulePackPolicy = &pack
		}
	}
	var findingGovernancePolicy *reviewconfig.FindingGovernancePolicy
	if options.change.IsFilterPolicy() {
		if options.findingGovernancePolicy != nil {
			policy := *options.findingGovernancePolicy
			policy.CalibrationProfile.Points = slices.Clone(policy.CalibrationProfile.Points)
			findingGovernancePolicy = &policy
		} else {
			data, err := os.ReadFile(options.findingGovernance)
			if err != nil {
				return fmt.Errorf("read finding governance replay policy: %w", err)
			}
			policy, err := reviewconfig.DecodeFindingGovernancePolicy(data)
			if err != nil {
				return fmt.Errorf("decode finding governance replay policy: %w", err)
			}
			findingGovernancePolicy = &policy
		}
	}
	return executeFormalAgentRunMode(ctx, formalAgentRunFlags{
		formalAgentBootstrapFlags: formalAgentBootstrapFlags{
			store: options.store, sourceRun: options.sourceFormalRun,
			idempotencyKey: options.idempotencyKey, node: options.node,
			workerScript: options.workerScript, providerProfile: options.providerProfile,
			model: options.model, json: options.json, options: options.options,
			componentVariant: cloneLocalPiComponentVariant(options.componentVariant),
		},
		pricing: options.pricing, transport: options.transport,
	}, stdout, runner, &formalReplayExecution{
		Variable: options.change, TimeoutMS: options.timeoutMS, CreatedAt: options.at,
		RulePackPolicy:          rulePackPolicy,
		WorkflowDefinition:      workflowPolicy,
		FindingGovernancePolicy: findingGovernancePolicy,
		IndexContextProviders:   slices.Clone(options.indexContextProviders),
	}, nil, nil, nil, nil)
}

func executeFormalAgentRunMode(
	ctx context.Context,
	options formalAgentRunFlags,
	stdout io.Writer,
	runner agentshadowworker.Runner,
	replay *formalReplayExecution,
	frozenConfig application.GovernedConfigProvider,
	claimed *scheduling.Dispatch,
	claimedWorkloads *scheduling.Repository,
	backend *formalExecutionBackend,
) error {
	if runner == nil {
		return fmt.Errorf("formal Pi runner is required")
	}
	if err := backend.validate(); err != nil {
		return err
	}
	store, err := local.Open(options.store)
	if err != nil {
		return fmt.Errorf("open Argus store: %w", err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	sourceSnapshot, err := runs.ExecutionSnapshotForRun(options.sourceRun)
	if err != nil {
		return fmt.Errorf("load source snapshot: %w", err)
	}
	var sourceSpec contractsv1alpha1.ReviewSpec
	if err := runs.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &sourceSpec); err != nil {
		return fmt.Errorf("load source ReviewSpec: %w", err)
	}
	subject := application.AgentPlanningSubject{
		TenantID: sourceSpec.TenantID, OrganizationID: "local",
		WorkspaceID:  sourceSpec.WorkspaceID,
		RepositoryID: sourceSpec.Repository.RepositoryID,
	}
	if backend != nil && options.transport.Backend != "" &&
		options.transport.Backend != formalExecutionBackendLocalPi {
		return fmt.Errorf("injected formal execution backend conflicts with --execution-backend")
	}
	if backend == nil {
		backend, err = buildConfiguredFormalExecutionBackend(ctx, options.transport, subject)
		if err != nil {
			return err
		}
	}
	if err := backend.validate(); err != nil {
		return err
	}
	bootstrap, err := buildLocalFormalPiBootstrap(options.options, options.componentVariant)
	if err != nil {
		return err
	}
	var capabilities application.AgentExecutorCapabilityResolver
	if backend != nil {
		capabilities = backend.Capabilities
	} else {
		capabilities, err = piexecution.NewCapabilityResolver(bootstrap.Manifest, options.pricing)
		if err != nil {
			return err
		}
	}
	runtimeEvidenceRef, err := persistFormalRuntimeEvidence(runs, bootstrap)
	if err != nil {
		return err
	}
	var configProvider application.GovernedConfigProvider
	if replay != nil {
		if len(sourceSnapshot.RuntimeEvidenceRefs) != 1 ||
			sourceSnapshot.RuntimeEvidenceRefs[0] != runtimeEvidenceRef {
			return fmt.Errorf("current local Pi runtime differs from exact replay source")
		}
		frozen, loadErr := loadFrozenFormalReplayConfig(
			runs, options.sourceRun, sourceSnapshot, subject,
		)
		if loadErr != nil {
			return loadErr
		}
		configProvider = frozen.Provider
		if replay.Variable == runmodel.ReplayVariableWorkflow {
			if replay.WorkflowDefinition == nil {
				return fmt.Errorf("formal workflow replay requires an exact WorkflowDefinition")
			}
			sourceWorkflowBytes, readErr := runs.ReadArtifact(sourceSnapshot.WorkflowDefinitionRef)
			if readErr != nil {
				return fmt.Errorf("read workflow replay source definition: %w", readErr)
			}
			sourceWorkflow, decodeErr := workflow.DecodeDefinition(sourceWorkflowBytes)
			if decodeErr != nil {
				return fmt.Errorf("decode workflow replay source definition: %w", decodeErr)
			}
			variant, receipt, _, variantErr := formalreview.BuildWorkflowReplayConfig(
				frozen.Bundle, frozen.Receipt, sourceWorkflow, *replay.WorkflowDefinition,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable == runmodel.ReplayVariableBudget {
			variant, receipt, variantErr := formalreview.BuildBudgetReplayConfig(
				frozen.Bundle, frozen.Receipt, replay.TimeoutMS,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable == runmodel.ReplayVariableModel {
			model := reviewconfig.VersionedRef{
				ID: bootstrap.Manifest.Model.ID, Revision: bootstrap.Manifest.Model.Revision,
				SHA256: bootstrap.Manifest.Model.SHA256,
			}
			variant, receipt, variantErr := formalreview.BuildModelReplayConfig(
				frozen.Bundle, frozen.Receipt, model,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable == runmodel.ReplayVariablePrompt {
			prompt := reviewconfig.VersionedRef{
				ID: bootstrap.Manifest.Prompt.ID, Revision: bootstrap.Manifest.Prompt.Revision,
				SHA256: bootstrap.Manifest.Prompt.SHA256,
			}
			variant, receipt, variantErr := formalreview.BuildPromptReplayConfig(
				frozen.Bundle, frozen.Receipt, prompt,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable == runmodel.ReplayVariableSkillPack {
			if bootstrap.Revision.Patch.AgentReview == nil ||
				bootstrap.Revision.Patch.AgentReview.SkillPacks == nil {
				return fmt.Errorf("formal bootstrap omitted governed skill definitions")
			}
			variant, receipt, variantErr := formalreview.BuildSkillPackReplayConfig(
				frozen.Bundle,
				frozen.Receipt,
				bootstrap.Revision.Patch.AgentReview.SkillPacks.Upsert,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable == runmodel.ReplayVariableKnowledgePack {
			if bootstrap.Revision.Patch.AgentReview == nil ||
				bootstrap.Revision.Patch.AgentReview.KnowledgePacks == nil {
				return fmt.Errorf("formal bootstrap omitted governed knowledge definitions")
			}
			variant, receipt, variantErr := formalreview.BuildKnowledgePackReplayConfig(
				frozen.Bundle,
				frozen.Receipt,
				bootstrap.Revision.Patch.AgentReview.KnowledgePacks.Upsert,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable == runmodel.ReplayVariableRulePack {
			if replay.RulePackPolicy == nil {
				return fmt.Errorf("formal rule_pack replay requires an exact sealed rule pack")
			}
			variant, receipt, variantErr := formalreview.BuildRulePackReplayConfig(
				frozen.Bundle, frozen.Receipt, *replay.RulePackPolicy,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable == runmodel.ReplayVariableIndex {
			variant, receipt, variantErr := formalreview.BuildIndexReplayConfig(
				frozen.Bundle,
				frozen.Receipt,
				replay.IndexContextProviders,
			)
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		} else if replay.Variable.IsFilterPolicy() {
			if replay.FindingGovernancePolicy == nil {
				return fmt.Errorf("formal filter_policy replay requires an exact finding governance policy")
			}
			var variant reviewconfig.ConfigBundle
			var receipt reviewconfig.ConfigResolutionReceipt
			var variantErr error
			if replay.Variable == runmodel.ReplayVariableFilterPolicy {
				variant, receipt, variantErr = formalreview.BuildFilterPolicyReplayConfig(
					frozen.Bundle, frozen.Receipt, *replay.FindingGovernancePolicy,
				)
			} else {
				variant, receipt, variantErr = formalreview.BuildFindingGovernanceReplayConfig(
					frozen.Bundle, frozen.Receipt, *replay.FindingGovernancePolicy,
				)
			}
			if variantErr != nil {
				return variantErr
			}
			configProvider, err = formalreview.NewFrozenReplayConfigProvider(variant, receipt)
			if err != nil {
				return err
			}
		}
		if replay.Variable != runmodel.ReplayVariableModel &&
			replay.Variable != runmodel.ReplayVariablePrompt &&
			replay.Variable != runmodel.ReplayVariableSkillPack &&
			replay.Variable != runmodel.ReplayVariableKnowledgePack &&
			replay.Variable != runmodel.ReplayVariableRulePack &&
			!replay.Variable.IsFilterPolicy() {
			if err := preflightFormalReplayCapability(
				ctx, runs, subject, options.sourceRun, capabilities,
			); err != nil {
				return err
			}
		}
	} else if frozenConfig != nil {
		configProvider = frozenConfig
	} else {
		configStore, openErr := local.Open(options.configState)
		if openErr != nil {
			return fmt.Errorf("open config state: %w", openErr)
		}
		configs, configErr := configrepo.New(configStore)
		if configErr != nil {
			return configErr
		}
		configProvider = configs
	}
	initializerOptions := make([]formalreview.InitializerOption, 0, 1)
	if replay != nil && replay.Variable == runmodel.ReplayVariableIndex {
		sourceAdapter, adapterErr := gitadapter.New()
		if adapterErr != nil {
			return fmt.Errorf("initialize formal index replay source adapter: %w", adapterErr)
		}
		providerExecutor, executorErr := contextprovider.NewLocalExecutor(sourceAdapter)
		if executorErr != nil {
			return fmt.Errorf("initialize formal index replay context providers: %w", executorErr)
		}
		materializer, materializerErr := application.NewService(
			sourceAdapter,
			runs,
			application.ServiceOptions{
				DisableScheduling: true,
				ContextProviders:  providerExecutor,
				BuildIdentity:     "argus-formal-index-materializer",
			},
		)
		if materializerErr != nil {
			return fmt.Errorf("initialize formal index replay materializer: %w", materializerErr)
		}
		initializerOptions = append(
			initializerOptions,
			formalreview.WithIndexReplayMaterializer(materializer),
		)
	}
	initializer, err := formalreview.NewInitializer(
		runs, configProvider, workflow.FormalAgentReviewDefinition(), initializerOptions...,
	)
	if err != nil {
		return err
	}
	var initialized formalreview.InitializedRun
	if replay != nil && replay.Variable == runmodel.ReplayVariableNone {
		initialized, err = initializer.InitializeExactReplay(ctx, subject, formalreview.ExactReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariableBudget {
		initialized, err = initializer.InitializeBudgetReplay(ctx, subject, formalreview.BudgetReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariableModel {
		initialized, err = initializer.InitializeModelReplay(ctx, subject, formalreview.ModelReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariablePrompt {
		initialized, err = initializer.InitializePromptReplay(ctx, subject, formalreview.PromptReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariableSkillPack {
		initialized, err = initializer.InitializeSkillPackReplay(ctx, subject, formalreview.SkillPackReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariableKnowledgePack {
		initialized, err = initializer.InitializeKnowledgePackReplay(ctx, subject, formalreview.KnowledgePackReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariableRulePack {
		initialized, err = initializer.InitializeRulePackReplay(ctx, subject, formalreview.RulePackReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariableWorkflow {
		if replay.WorkflowDefinition == nil {
			return fmt.Errorf("formal workflow replay requires an exact WorkflowDefinition")
		}
		initialized, err = initializer.InitializeWorkflowReplay(ctx, subject, formalreview.WorkflowReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
			Definition: *replay.WorkflowDefinition,
		})
	} else if replay != nil && replay.Variable == runmodel.ReplayVariableIndex {
		initialized, err = initializer.InitializeIndexReplay(ctx, subject, formalreview.IndexReplayCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
		})
	} else if replay != nil && replay.Variable.IsFilterPolicy() {
		if replay.Variable == runmodel.ReplayVariableFilterPolicy {
			initialized, err = initializer.InitializeFilterPolicyReplay(ctx, subject, formalreview.FilterPolicyReplayCommand{
				SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
				BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
			})
		} else {
			initialized, err = initializer.InitializeFindingGovernanceReplay(ctx, subject, formalreview.FindingGovernanceReplayCommand{
				SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
				BuildIdentity: bootstrap.Manifest.BuildIdentity, CreatedAt: replay.CreatedAt,
			})
		}
	} else {
		initialized, err = initializer.Initialize(ctx, subject, formalreview.InitializeCommand{
			SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
			BuildIdentity:       bootstrap.Manifest.BuildIdentity,
			RuntimeProfile:      bootstrap.Manifest.Runtime.ID + "@" + bootstrap.Manifest.Runtime.Revision,
			RuntimeEvidenceRefs: []runmodel.ArtifactRef{runtimeEvidenceRef},
		})
	}
	if err != nil {
		return err
	}
	if replay != nil && replay.Variable.IsFilterPolicy() {
		if replay.FindingGovernancePolicy == nil {
			return fmt.Errorf("formal filter_policy replay requires an exact finding governance policy")
		}
		result, finalizeErr := formalreview.FinalizeFilterPolicyReplay(
			ctx, runs, initialized, *replay.FindingGovernancePolicy,
		)
		if finalizeErr != nil {
			return finalizeErr
		}
		output, outputErr := loadFindingGovernanceReplayOutput(
			runs, result.Run, store.Root(), result.Recovered,
		)
		if outputErr != nil {
			return outputErr
		}
		return writeFormalAgentRunOutput(stdout, options.json, output)
	}

	artifacts, err := artifactrepo.Open(store, nil)
	if err != nil {
		return err
	}
	componentRepository, err := agentcomponentrepo.New(
		store, artifacts, artifactrepo.DefaultAuthority,
	)
	if err != nil {
		return err
	}
	if replay != nil && replay.Variable == runmodel.ReplayVariableModel {
		if err := ensureFormalReplayModelComponent(
			ctx, componentRepository, subject, bootstrap, initialized.Snapshot.CreatedAt,
		); err != nil {
			return err
		}
	}
	if replay != nil && replay.Variable == runmodel.ReplayVariablePrompt {
		if err := ensureFormalReplayPromptComponent(
			ctx, componentRepository, subject, bootstrap, initialized.Snapshot.CreatedAt,
		); err != nil {
			return err
		}
	}
	if replay != nil && replay.Variable == runmodel.ReplayVariableSkillPack {
		if err := ensureFormalReplaySkillComponents(
			ctx, componentRepository, subject, bootstrap, initialized.Snapshot.CreatedAt,
		); err != nil {
			return err
		}
	}
	if replay != nil && replay.Variable == runmodel.ReplayVariableKnowledgePack {
		if err := ensureFormalReplayKnowledgeComponents(
			ctx, componentRepository, subject, bootstrap, initialized.Snapshot.CreatedAt,
		); err != nil {
			return err
		}
	}
	if replay != nil && replay.Variable == runmodel.ReplayVariableIndex {
		if err := ensureFormalReplayContextProviderComponents(
			ctx,
			componentRepository,
			subject,
			replay.IndexContextProviders,
			initialized.Snapshot.CreatedAt,
		); err != nil {
			return err
		}
	}
	componentResolver, err := agentcomponentrepo.NewResolver(
		componentRepository,
		agentcomponentrepo.Subject{
			TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
			WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
		},
	)
	if err != nil {
		return err
	}
	agentRepository, err := agentadapter.New(runs, artifacts, artifactrepo.DefaultAuthority)
	if err != nil {
		return err
	}
	workloads := claimedWorkloads
	if workloads == nil {
		workloads, err = scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
		if err != nil {
			return err
		}
	}
	if record, getErr := workloads.Get(initialized.RunID + "-workload"); getErr == nil {
		if isFormalWorkloadTerminal(record.State) {
			output, terminal, recoverErr := recoverFormalAgentRunOutput(
				ctx, agentRepository, runs, subject, record, initialized.RunID,
				options.sourceRun, store.Root(),
			)
			if recoverErr != nil {
				return recoverErr
			}
			output.RuntimeFileManifestRef = &runtimeEvidenceRef
			if finalizeErr := finalizeFormalReviewRun(
				ctx, runs, initialized, subject, terminal, &output,
			); finalizeErr != nil {
				return finalizeErr
			}
			return writeFormalAgentRunOutput(stdout, options.json, output)
		}
	} else if !errors.Is(getErr, scheduling.ErrNotFound) {
		return fmt.Errorf("inspect formal workload: %w", getErr)
	}
	preparer, err := application.NewAgentStagePreparer(
		configProvider, componentResolver, agentRepository, agentRepository, nil,
	)
	if err != nil {
		return err
	}
	prepared, err := preparer.PrepareGovernedAgentStage(
		ctx, subject, application.PrepareAgentStageCommand{
			ReviewRunID: initialized.RunID, StageID: "agent_hypothesize",
		},
	)
	if err != nil {
		return fmt.Errorf("preflight formal agent plan before workload claim: %w", err)
	}
	if _, err := capabilities.ResolveAgentExecutorCapability(ctx, subject, prepared.Plan); err != nil {
		return fmt.Errorf("preflight formal agent capability before workload claim: %w", err)
	}

	workerID := "argus-formal-local-pi"
	if backend != nil {
		workerID = "argus-formal-hailix-http"
	}
	timeoutMS := options.options.TimeoutMS
	if replay != nil {
		timeoutMS = initialized.Snapshot.ToolPolicy.PerCallTimeoutMS
	}
	var dispatchLease scheduling.DispatchLease
	if claimed != nil {
		if claimed.Spec.RunID != initialized.RunID ||
			claimed.Spec.WorkloadID != initialized.RunID+"-workload" {
			return fmt.Errorf("claimed formal workload does not bind initialized run")
		}
		dispatchLease = claimed.Lease
		workerID = claimed.Lease.WorkerID
	} else {
		dispatchLease, err = ensureFormalWorkload(
			ctx, workloads, initialized, subject, workerID, timeoutMS,
		)
		if err != nil {
			return err
		}
	}
	authorities, err := agentadapter.NewSchedulingExecutionAuthorityResolver(
		workloads, workerID, nil,
	)
	if err != nil {
		return err
	}
	var platform execution.PlatformPort
	var executor application.FormalAgentStageExecutor
	var callbacks application.AgentStageResultCallbackVerifier
	if backend != nil {
		platform = backend.Platform
		executor = backend.Executor
		callbacks = backend.Callbacks
	} else {
		artifactIO, artifactIOErr := piexecution.NewGovernedArtifactIO(
			artifacts, subject, artifactrepo.DefaultAuthority,
		)
		if artifactIOErr != nil {
			return artifactIOErr
		}
		localPlatform, platformErr := piexecution.NewPlatform(store, piexecution.Config{
			Subject: subject, NodePath: options.node, WorkerScript: options.workerScript,
			Environment: agentReviewEnvironment(), Manifest: bootstrap.Manifest,
			Pricing: options.pricing, Runner: runner,
			Artifacts: artifactIO, LocalArtifacts: runs,
		})
		if platformErr != nil {
			return platformErr
		}
		platform = localPlatform
		executor = localPlatform
		callbacks = localPlatform
	}
	claimAuthorizer, err := agentadapter.NewExecutionClaimAuthorizer(agentRepository, subject)
	if err != nil {
		return err
	}
	gateway, err := execution.NewGateway(
		platform, execution.GovernedArtifactResolver{Repository: artifacts}, claimAuthorizer,
	)
	if err != nil {
		return err
	}
	dispatcher, err := application.NewAgentStageDispatcher(
		preparer, authorities, capabilities, gateway, agentRepository, nil,
	)
	if err != nil {
		return err
	}
	admitter, err := application.NewAgentStageResultAdmitter(agentRepository, callbacks, nil)
	if err != nil {
		return err
	}
	orchestrator, err := application.NewFormalAgentReviewOrchestrator(
		dispatcher, executor, admitter,
	)
	if err != nil {
		return err
	}
	outcome, err := orchestrator.Run(ctx, subject, application.RunFormalAgentReviewCommand{
		ReviewRunID: initialized.RunID, StageID: "agent_hypothesize",
	})
	if err != nil {
		return err
	}
	result := outcome.TerminalResult()
	if result == nil {
		return fmt.Errorf("formal orchestrator returned no terminal result")
	}
	if claimed == nil {
		if err := completeFormalWorkload(ctx, workloads, dispatchLease, *result); err != nil {
			return fmt.Errorf("commit formal scheduling terminal: %w", err)
		}
	}
	output := formalAgentRunOutput{
		FormalRunID: initialized.RunID, SourceRunID: options.sourceRun,
		ExecutionID: result.ExecutionID, Status: result.Status,
		Failure: result.Failure, TerminalAuthority: string(outcomeTerminalKind(outcome)),
		WorkloadState:          schedulingStateForExecutionStatus(result.Status),
		RecoveredDispatch:      outcome.Dispatch.Recovered,
		StorePath:              store.Root(),
		RuntimeFileManifestRef: &runtimeEvidenceRef,
	}
	if outcome.Success != nil {
		set := outcome.Success.Hypotheses
		output.Hypotheses = &set
		output.RecoveredAdmission = outcome.Success.Recovered
	} else if outcome.Terminal != nil {
		output.RecoveredAdmission = outcome.Terminal.Recovered
	}
	if err := attachGovernedFormalReport(runs, &output); err != nil {
		return err
	}
	terminal, found, err := application.ReadFormalAgentReviewTerminal(
		ctx, agentRepository, subject, application.ReadFormalAgentReviewTerminalCommand{
			ReviewRunID: initialized.RunID, StageID: "agent_hypothesize",
			Attempt:    outcome.Dispatch.Request.Attempt,
			Generation: outcome.Dispatch.Request.Generation,
		},
	)
	if err != nil {
		return fmt.Errorf("read admitted formal terminal for finalization: %w", err)
	}
	if !found {
		return fmt.Errorf("admitted formal terminal disappeared before finalization")
	}
	if err := finalizeFormalReviewRun(
		ctx, runs, initialized, subject, terminal, &output,
	); err != nil {
		return err
	}
	if err := attachFormalTaskEvidenceSummary(ctx, artifacts, runs, &output); err != nil {
		return err
	}
	return writeFormalAgentRunOutput(stdout, options.json, output)
}

func resolveOrPublishFormalComponent(
	ctx context.Context,
	repository *agentcomponentrepo.Repository,
	subject agentcomponentrepo.Subject,
	publication agentcomponentrepo.Publication,
) (contractsv1alpha1.AgentStageComponentBinding, error) {
	if repository == nil {
		return contractsv1alpha1.AgentStageComponentBinding{},
			fmt.Errorf("formal component repository is required")
	}
	binding, err := repository.Resolve(
		ctx,
		subject,
		publication.Contract,
		publication.Ref,
	)
	if err == nil {
		return binding, nil
	}
	if !errors.Is(err, agentcomponentrepo.ErrComponentNotFound) {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	return repository.Publish(ctx, subject, publication)
}

func preflightFormalReplayCapability(
	ctx context.Context,
	repository *runrepo.Repository,
	subject application.AgentPlanningSubject,
	sourceRunID string,
	capabilities application.AgentExecutorCapabilityResolver,
) error {
	if repository == nil || capabilities == nil {
		return fmt.Errorf("formal replay capability preflight is not initialized")
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	admission, found, err := repository.LookupAgentStagePlanAdmission(
		sourceRunID, "agent_hypothesize", ledgerSubject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("source plan admission does not exist")
		}
		return fmt.Errorf("resolve formal replay source plan admission: %w", err)
	}
	planBytes, err := repository.ReadArtifact(admission.Plan.Local)
	if err != nil {
		return fmt.Errorf("read formal replay source AgentStagePlan: %w", err)
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(planBytes)
	if err != nil {
		return fmt.Errorf("decode formal replay source AgentStagePlan: %w", err)
	}
	if _, err := capabilities.ResolveAgentExecutorCapability(ctx, subject, plan); err != nil {
		return fmt.Errorf("preflight formal replay runtime capability: %w", err)
	}
	return nil
}

func attachFormalTaskEvidenceSummary(
	ctx context.Context,
	artifacts *artifactrepo.Repository,
	runs *runrepo.Repository,
	output *formalAgentRunOutput,
) error {
	service, err := formalevidence.New(runs, artifacts)
	if err != nil {
		return err
	}
	summary, found, err := service.Describe(ctx, output.FormalRunID)
	if err != nil {
		return fmt.Errorf("describe formal task evidence: %w", err)
	}
	if found {
		output.TaskEvidence = &summary
	}
	return nil
}

func persistFormalRuntimeEvidence(
	repository *runrepo.Repository,
	bootstrap formalreview.LocalPiBootstrap,
) (runmodel.ArtifactRef, error) {
	if repository == nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("formal runtime evidence repository is required")
	}
	for _, component := range bootstrap.Components {
		if component.Ref.ID != bootstrap.Manifest.Runtime.ID ||
			component.Ref.Revision != bootstrap.Manifest.Runtime.Revision ||
			component.Ref.SHA256 != bootstrap.Manifest.Runtime.SHA256 {
			continue
		}
		if component.Contract != contractsv1alpha1.AgentStagePlanRuntimeContract {
			return runmodel.ArtifactRef{}, fmt.Errorf("formal runtime component contract changed")
		}
		ref, err := repository.PutArtifact(
			runmodel.ContractLocalRuntimeFileManifest, component.Content,
		)
		if err != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("persist local runtime file manifest: %w", err)
		}
		if ref.SHA256 != bootstrap.Manifest.Runtime.SHA256 {
			return runmodel.ArtifactRef{}, fmt.Errorf("persisted runtime file manifest digest changed")
		}
		return ref, nil
	}
	return runmodel.ArtifactRef{}, fmt.Errorf("formal bootstrap omitted runtime file manifest component")
}

func ensureFormalReplayModelComponent(
	ctx context.Context,
	repository *agentcomponentrepo.Repository,
	subject application.AgentPlanningSubject,
	bootstrap formalreview.LocalPiBootstrap,
	publishedAt time.Time,
) error {
	if repository == nil || publishedAt.IsZero() || publishedAt.Location() != time.UTC {
		return fmt.Errorf("formal model replay component publication is not initialized")
	}
	componentSubject := agentcomponentrepo.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	modelRef := reviewconfig.VersionedRef{
		ID: bootstrap.Manifest.Model.ID, Revision: bootstrap.Manifest.Model.Revision,
		SHA256: bootstrap.Manifest.Model.SHA256,
	}
	if _, err := repository.Resolve(
		ctx, componentSubject, contractsv1alpha1.AgentStagePlanModelContract, modelRef,
	); err == nil {
		return nil
	}
	for _, component := range bootstrap.Components {
		if component.Ref != modelRef {
			continue
		}
		if component.Contract != contractsv1alpha1.AgentStagePlanModelContract {
			return fmt.Errorf("formal model replay component contract changed")
		}
		_, err := repository.Publish(ctx, componentSubject, agentcomponentrepo.Publication{
			Ref: component.Ref, Contract: component.Contract,
			Content: component.Content, PublishedAt: publishedAt,
		})
		if err == nil {
			return nil
		}
		if errors.Is(err, agentcomponentrepo.ErrComponentConflict) {
			if _, resolveErr := repository.Resolve(
				ctx, componentSubject, component.Contract, component.Ref,
			); resolveErr == nil {
				return nil
			}
		}
		return fmt.Errorf("publish formal model replay component: %w", err)
	}
	return fmt.Errorf("formal bootstrap omitted selected model component")
}

func ensureFormalReplayPromptComponent(
	ctx context.Context,
	repository *agentcomponentrepo.Repository,
	subject application.AgentPlanningSubject,
	bootstrap formalreview.LocalPiBootstrap,
	publishedAt time.Time,
) error {
	if repository == nil || publishedAt.IsZero() || publishedAt.Location() != time.UTC {
		return fmt.Errorf("formal prompt replay component publication is not initialized")
	}
	componentSubject := agentcomponentrepo.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	promptRef := reviewconfig.VersionedRef{
		ID: bootstrap.Manifest.Prompt.ID, Revision: bootstrap.Manifest.Prompt.Revision,
		SHA256: bootstrap.Manifest.Prompt.SHA256,
	}
	if _, err := repository.Resolve(
		ctx, componentSubject, contractsv1alpha1.AgentStagePlanPromptContract, promptRef,
	); err == nil {
		return nil
	}
	for _, component := range bootstrap.Components {
		if component.Ref != promptRef {
			continue
		}
		if component.Contract != contractsv1alpha1.AgentStagePlanPromptContract {
			return fmt.Errorf("formal prompt replay component contract changed")
		}
		_, err := repository.Publish(ctx, componentSubject, agentcomponentrepo.Publication{
			Ref: component.Ref, Contract: component.Contract,
			Content: component.Content, PublishedAt: publishedAt,
		})
		if err == nil {
			return nil
		}
		if errors.Is(err, agentcomponentrepo.ErrComponentConflict) {
			if _, resolveErr := repository.Resolve(
				ctx, componentSubject, component.Contract, component.Ref,
			); resolveErr == nil {
				return nil
			}
		}
		return fmt.Errorf("publish formal prompt replay component: %w", err)
	}
	return fmt.Errorf("formal bootstrap omitted selected prompt component")
}

func ensureFormalReplaySkillComponents(
	ctx context.Context,
	repository *agentcomponentrepo.Repository,
	subject application.AgentPlanningSubject,
	bootstrap formalreview.LocalPiBootstrap,
	publishedAt time.Time,
) error {
	if repository == nil || publishedAt.IsZero() || publishedAt.Location() != time.UTC ||
		len(bootstrap.Manifest.ReviewSkills) == 0 {
		return fmt.Errorf("formal skill_pack replay component publication is not initialized")
	}
	componentSubject := agentcomponentrepo.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	for _, manifestRef := range bootstrap.Manifest.ReviewSkills {
		ref := reviewconfig.VersionedRef{
			ID: manifestRef.ID, Revision: manifestRef.Revision, SHA256: manifestRef.SHA256,
		}
		if _, err := repository.Resolve(
			ctx, componentSubject, contractsv1alpha1.AgentStagePlanSkillContract, ref,
		); err == nil {
			continue
		}
		found := false
		for _, component := range bootstrap.Components {
			if component.Ref != ref {
				continue
			}
			found = true
			if component.Contract != contractsv1alpha1.AgentStagePlanSkillContract {
				return fmt.Errorf("formal skill_pack replay component contract changed")
			}
			_, err := repository.Publish(ctx, componentSubject, agentcomponentrepo.Publication{
				Ref: component.Ref, Contract: component.Contract,
				Content: component.Content, PublishedAt: publishedAt,
			})
			if err == nil {
				break
			}
			if errors.Is(err, agentcomponentrepo.ErrComponentConflict) {
				if _, resolveErr := repository.Resolve(
					ctx, componentSubject, component.Contract, component.Ref,
				); resolveErr == nil {
					break
				}
			}
			return fmt.Errorf("publish formal skill_pack replay component %q: %w", ref.ID, err)
		}
		if !found {
			return fmt.Errorf("formal bootstrap omitted selected review skill component %q", ref.ID)
		}
	}
	return nil
}

func ensureFormalReplayKnowledgeComponents(
	ctx context.Context,
	repository *agentcomponentrepo.Repository,
	subject application.AgentPlanningSubject,
	bootstrap formalreview.LocalPiBootstrap,
	publishedAt time.Time,
) error {
	if repository == nil || publishedAt.IsZero() || publishedAt.Location() != time.UTC ||
		len(bootstrap.Manifest.Knowledge) == 0 {
		return fmt.Errorf("formal knowledge_pack replay component publication is not initialized")
	}
	componentSubject := agentcomponentrepo.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	for _, manifestRef := range bootstrap.Manifest.Knowledge {
		ref := reviewconfig.VersionedRef{
			ID: manifestRef.ID, Revision: manifestRef.Revision, SHA256: manifestRef.SHA256,
		}
		if _, err := repository.Resolve(
			ctx, componentSubject, contractsv1alpha1.AgentStagePlanKnowledgeContract, ref,
		); err == nil {
			continue
		}
		found := false
		for _, component := range bootstrap.Components {
			if component.Ref != ref {
				continue
			}
			found = true
			if component.Contract != contractsv1alpha1.AgentStagePlanKnowledgeContract {
				return fmt.Errorf("formal knowledge_pack replay component contract changed")
			}
			_, err := repository.Publish(ctx, componentSubject, agentcomponentrepo.Publication{
				Ref: component.Ref, Contract: component.Contract,
				Content: component.Content, PublishedAt: publishedAt,
			})
			if err == nil {
				break
			}
			if errors.Is(err, agentcomponentrepo.ErrComponentConflict) {
				if _, resolveErr := repository.Resolve(
					ctx, componentSubject, component.Contract, component.Ref,
				); resolveErr == nil {
					break
				}
			}
			return fmt.Errorf("publish formal knowledge_pack replay component %q: %w", ref.ID, err)
		}
		if !found {
			return fmt.Errorf("formal bootstrap omitted selected knowledge component %q", ref.ID)
		}
	}
	return nil
}

func ensureFormalReplayContextProviderComponents(
	ctx context.Context,
	repository *agentcomponentrepo.Repository,
	subject application.AgentPlanningSubject,
	definitions []reviewconfig.ContextProviderDefinition,
	publishedAt time.Time,
) error {
	if repository == nil || publishedAt.IsZero() || publishedAt.Location() != time.UTC ||
		len(definitions) == 0 {
		return fmt.Errorf("formal index replay context provider publication is not initialized")
	}
	componentSubject := agentcomponentrepo.Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	for _, definition := range definitions {
		content, err := contextprovider.LocalAdapterArtifact(definition)
		if err != nil {
			return fmt.Errorf("resolve formal index replay provider %q: %w", definition.ID, err)
		}
		if _, err := resolveOrPublishFormalComponent(
			ctx,
			repository,
			componentSubject,
			agentcomponentrepo.Publication{
				Ref:      definition.Adapter,
				Contract: contractsv1alpha1.AgentStagePlanContextProviderAdapterContract,
				Content:  content, PublishedAt: publishedAt,
			},
		); err != nil {
			return fmt.Errorf("publish formal index replay provider %q: %w", definition.ID, err)
		}
	}
	return nil
}

type frozenFormalReplayConfig struct {
	Bundle   reviewconfig.ConfigBundle
	Receipt  reviewconfig.ConfigResolutionReceipt
	Provider application.GovernedConfigProvider
}

func loadFrozenFormalReplayConfig(
	repository *runrepo.Repository,
	sourceRunID string,
	snapshot runmodel.ExecutionSnapshot,
	subject application.AgentPlanningSubject,
) (frozenFormalReplayConfig, error) {
	if repository == nil {
		return frozenFormalReplayConfig{}, fmt.Errorf("formal replay repository is required")
	}
	bundleBytes, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return frozenFormalReplayConfig{}, fmt.Errorf("read exact replay source config: %w", err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleBytes)
	if err != nil {
		return frozenFormalReplayConfig{}, fmt.Errorf("decode exact replay source config: %w", err)
	}
	ledgerSubject := runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
	admission, found, err := repository.LookupAgentStagePlanAdmission(
		sourceRunID, "agent_hypothesize", ledgerSubject,
	)
	if err != nil || !found {
		sourceRun, loadErr := repository.LoadRun(sourceRunID)
		if loadErr == nil && sourceRun.Kind == runmodel.RunKindReplay &&
			sourceRun.ReplayVariable.IsFilterPolicy() &&
			bundle.FindingGovernance != nil {
			parentSnapshot, parentSnapshotErr := repository.ExecutionSnapshotForRun(sourceRun.SourceRunID)
			if parentSnapshotErr != nil {
				return frozenFormalReplayConfig{}, fmt.Errorf(
					"load finding_governance replay parent snapshot: %w", parentSnapshotErr,
				)
			}
			parent, parentErr := loadFrozenFormalReplayConfig(
				repository, sourceRun.SourceRunID, parentSnapshot, subject,
			)
			if parentErr != nil {
				return frozenFormalReplayConfig{}, parentErr
			}
			var derived reviewconfig.ConfigBundle
			var receipt reviewconfig.ConfigResolutionReceipt
			var deriveErr error
			if sourceRun.ReplayVariable == runmodel.ReplayVariableFilterPolicy {
				derived, receipt, deriveErr = formalreview.BuildFilterPolicyReplayConfig(
					parent.Bundle, parent.Receipt, *bundle.FindingGovernance,
				)
			} else {
				derived, receipt, deriveErr = formalreview.BuildFindingGovernanceReplayConfig(
					parent.Bundle, parent.Receipt, *bundle.FindingGovernance,
				)
			}
			if deriveErr != nil {
				return frozenFormalReplayConfig{}, fmt.Errorf(
					"reconstruct finding_governance source receipt: %w", deriveErr,
				)
			}
			if derived.SHA256 != bundle.SHA256 || derived.BundleID != bundle.BundleID {
				return frozenFormalReplayConfig{}, fmt.Errorf(
					"reconstructed finding_governance config differs from committed source",
				)
			}
			provider, providerErr := formalreview.NewFrozenReplayConfigProvider(bundle, receipt)
			if providerErr != nil {
				return frozenFormalReplayConfig{}, providerErr
			}
			return frozenFormalReplayConfig{Bundle: bundle, Receipt: receipt, Provider: provider}, nil
		}
		if err == nil {
			err = fmt.Errorf("source plan admission does not exist")
		}
		return frozenFormalReplayConfig{}, fmt.Errorf("resolve exact replay source plan admission: %w", err)
	}
	receiptBytes, err := repository.ReadArtifact(admission.Sources.ConfigResolutionReceipt.Local)
	if err != nil {
		return frozenFormalReplayConfig{}, fmt.Errorf("read exact replay source config receipt: %w", err)
	}
	receipt, err := reviewconfig.DecodeConfigResolutionReceipt(receiptBytes)
	if err != nil {
		return frozenFormalReplayConfig{}, fmt.Errorf("decode exact replay source config receipt: %w", err)
	}
	provider, err := formalreview.NewFrozenReplayConfigProvider(bundle, receipt)
	if err != nil {
		return frozenFormalReplayConfig{}, err
	}
	return frozenFormalReplayConfig{Bundle: bundle, Receipt: receipt, Provider: provider}, nil
}

func recoverFormalAgentRunOutput(
	ctx context.Context,
	repository application.FormalAgentReviewTerminalRepository,
	artifacts *runrepo.Repository,
	subject application.AgentPlanningSubject,
	record scheduling.WorkloadRecord,
	formalRunID string,
	sourceRunID string,
	storePath string,
) (formalAgentRunOutput, application.FormalAgentReviewTerminal, error) {
	if !isFormalWorkloadTerminal(record.State) || record.LastLease == nil || record.Terminal == nil {
		return formalAgentRunOutput{}, application.FormalAgentReviewTerminal{}, fmt.Errorf(
			"formal workload has no complete terminal coordinate: state=%s", record.State,
		)
	}
	terminal, found, err := application.ReadLatestFormalAgentReviewTerminal(
		ctx,
		repository,
		subject,
		formalRunID,
		"agent_hypothesize",
	)
	if err != nil {
		return formalAgentRunOutput{}, application.FormalAgentReviewTerminal{}, fmt.Errorf("recover admitted formal terminal: %w", err)
	}
	if !found {
		return formalAgentRunOutput{}, application.FormalAgentReviewTerminal{}, fmt.Errorf(
			"terminal scheduling workload has no admitted formal terminal",
		)
	}
	output := formalAgentRunOutput{
		FormalRunID: formalRunID, SourceRunID: sourceRunID,
		ExecutionID: terminal.Result.ExecutionID, Status: terminal.Result.Status,
		Hypotheses: terminal.Hypotheses, Failure: terminal.Result.Failure,
		TerminalAuthority: string(terminal.Gate.Kind), WorkloadState: record.State,
		RecoveredDispatch: true, RecoveredAdmission: true, StorePath: storePath,
	}
	if err := attachGovernedFormalReport(artifacts, &output); err != nil {
		return formalAgentRunOutput{}, application.FormalAgentReviewTerminal{}, err
	}
	return output, terminal, nil
}

func loadFindingGovernanceReplayOutput(
	repository *runrepo.Repository,
	run runmodel.ReviewRun,
	storePath string,
	recovered bool,
) (formalAgentRunOutput, error) {
	if repository == nil || run.Kind != runmodel.RunKindReplay ||
		!run.ReplayVariable.IsFilterPolicy() ||
		run.Status != runmodel.RunStatusSucceeded || run.HypothesisSetRef == nil ||
		run.CandidateSetRef == nil || run.VerificationLedgerRef == nil ||
		run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil ||
		run.GovernedReportRef == nil || run.GovernedMarkdownRef == nil {
		return formalAgentRunOutput{}, fmt.Errorf("complete committed finding_governance replay is required")
	}
	var hypotheses contractsv1alpha1.ReviewHypothesisSet
	var candidates contractsv1alpha1.GovernedCandidateSet
	var verification contractsv1alpha1.CandidateVerificationLedger
	var calibration contractsv1alpha1.FindingCalibrationLedger
	var suppression contractsv1alpha1.FindingSuppressionLedger
	var report contractsv1alpha1.GovernedReviewReport
	for _, item := range []struct {
		ref    runmodel.ArtifactRef
		target any
	}{
		{*run.HypothesisSetRef, &hypotheses},
		{*run.CandidateSetRef, &candidates},
		{*run.VerificationLedgerRef, &verification},
		{*run.CalibrationLedgerRef, &calibration},
		{*run.SuppressionLedgerRef, &suppression},
		{*run.GovernedReportRef, &report},
	} {
		if err := repository.ReadJSONArtifact(item.ref, item.target); err != nil {
			return formalAgentRunOutput{}, err
		}
	}
	ref, err := repository.CommittedRunRef(run.RunID)
	if err != nil {
		return formalAgentRunOutput{}, err
	}
	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return formalAgentRunOutput{}, err
	}
	if len(snapshot.RuntimeEvidenceRefs) != 1 {
		return formalAgentRunOutput{}, fmt.Errorf("formal execution snapshot has no exact runtime file manifest")
	}
	return formalAgentRunOutput{
		FormalRunID: run.RunID, SourceRunID: run.SourceRunID,
		ExecutionID: report.ExecutionID, Status: contractsv1alpha1.StageExecutionSucceeded,
		Hypotheses:   &hypotheses,
		CandidateSet: &candidates, CandidateSetRef: run.CandidateSetRef,
		VerificationLedger: &verification, VerificationLedgerRef: run.VerificationLedgerRef,
		CalibrationLedger: &calibration, CalibrationLedgerRef: run.CalibrationLedgerRef,
		SuppressionLedger: &suppression, SuppressionLedgerRef: run.SuppressionLedgerRef,
		Report: &report, ReportRef: run.GovernedReportRef,
		MarkdownReportRef:      run.GovernedMarkdownRef,
		RuntimeFileManifestRef: &snapshot.RuntimeEvidenceRefs[0],
		ReviewRun:              runPtr(run), FinalRunRef: &ref,
		TerminalAuthority: "finding_governance_replay_derived",
		WorkloadState:     scheduling.StateSucceeded,
		RecoveredFinalRun: recovered, StorePath: storePath,
	}, nil
}

func runPtr(run runmodel.ReviewRun) *runmodel.ReviewRun {
	copy := run
	return &copy
}

func finalizeFormalReviewRun(
	ctx context.Context,
	repository *runrepo.Repository,
	initialized formalreview.InitializedRun,
	subject application.AgentPlanningSubject,
	terminal application.FormalAgentReviewTerminal,
	output *formalAgentRunOutput,
) error {
	if output == nil {
		return fmt.Errorf("formal run output is required")
	}
	finalizer, err := formalreview.NewFinalizer(repository)
	if err != nil {
		return err
	}
	finalized, err := finalizer.Finalize(ctx, formalreview.FinalizeCommand{
		Initialized: initialized, Subject: subject, Terminal: terminal,
		CandidateSet:       output.CandidateSetRef,
		VerificationLedger: output.VerificationLedgerRef,
		CalibrationLedger:  output.CalibrationLedgerRef,
		SuppressionLedger:  output.SuppressionLedgerRef,
		GovernedReport:     output.ReportRef, GovernedMarkdown: output.MarkdownReportRef,
	})
	if err != nil {
		return fmt.Errorf("finalize formal ReviewRun: %w", err)
	}
	output.ReviewRun = &finalized.Run
	output.FinalRunRef = &finalized.Ref
	output.RecoveredFinalRun = finalized.Recovered
	return nil
}

func attachGovernedFormalReport(
	repository *runrepo.Repository,
	output *formalAgentRunOutput,
) error {
	if repository == nil || output == nil {
		return fmt.Errorf("formal report repository and output are required")
	}
	if output.Status != contractsv1alpha1.StageExecutionSucceeded {
		return nil
	}
	if output.Hypotheses == nil {
		return fmt.Errorf("succeeded formal terminal has no admitted hypotheses")
	}
	governancePolicy, err := loadFrozenFindingGovernancePolicy(repository, output.FormalRunID)
	if err != nil {
		return err
	}
	verificationLedger, err := formalreview.BuildCandidateVerificationLedger(*output.Hypotheses)
	if err != nil {
		return fmt.Errorf("govern admitted formal verification: %w", err)
	}
	verificationLedgerRef, err := repository.PutJSONArtifact(
		runmodel.ContractCandidateVerificationLedger, verificationLedger,
	)
	if err != nil {
		return fmt.Errorf("persist governed formal verification ledger: %w", err)
	}
	candidateSet, err := formalreview.BuildGovernedCandidateSetFromVerification(
		*output.Hypotheses, verificationLedger,
	)
	if err != nil {
		return fmt.Errorf("govern admitted formal candidates: %w", err)
	}
	candidateSetRef, err := repository.PutJSONArtifact(
		runmodel.ContractGovernedCandidateSet, candidateSet,
	)
	if err != nil {
		return fmt.Errorf("persist governed formal candidate set: %w", err)
	}
	report, err := formalreview.BuildGovernedReportFromVerification(
		*output.Hypotheses, verificationLedger,
	)
	if err != nil {
		return fmt.Errorf("govern admitted formal hypotheses: %w", err)
	}
	reportRef, err := repository.PutJSONArtifact(runmodel.ContractGovernedReviewReport, report)
	if err != nil {
		return fmt.Errorf("persist governed formal report: %w", err)
	}
	calibration, err := formalreview.BuildFindingCalibrationLedgerWithPolicy(
		report, verificationLedger, governancePolicy,
	)
	if err != nil {
		return fmt.Errorf("calibrate governed formal findings: %w", err)
	}
	calibrationRef, err := repository.PutJSONArtifact(
		runmodel.ContractFindingCalibrationLedger, calibration,
	)
	if err != nil {
		return fmt.Errorf("persist governed formal calibration ledger: %w", err)
	}
	suppression, err := formalreview.BuildFindingSuppressionLedgerWithPolicy(
		report, calibration, governancePolicy,
	)
	if err != nil {
		return fmt.Errorf("project governed formal suppression: %w", err)
	}
	suppressionRef, err := repository.PutJSONArtifact(
		runmodel.ContractFindingSuppressionLedger, suppression,
	)
	if err != nil {
		return fmt.Errorf("persist governed formal suppression ledger: %w", err)
	}
	markdown, err := formalreview.RenderGovernedReportMarkdown(report)
	if err != nil {
		return fmt.Errorf("render governed formal report: %w", err)
	}
	markdownRef, err := repository.PutArtifact(
		runmodel.ContractGovernedReviewMarkdown, []byte(markdown),
	)
	if err != nil {
		return fmt.Errorf("persist governed formal Markdown report: %w", err)
	}
	output.Report = &report
	output.CandidateSet = &candidateSet
	output.CandidateSetRef = &candidateSetRef
	output.VerificationLedger = &verificationLedger
	output.VerificationLedgerRef = &verificationLedgerRef
	output.CalibrationLedger = &calibration
	output.CalibrationLedgerRef = &calibrationRef
	output.SuppressionLedger = &suppression
	output.SuppressionLedgerRef = &suppressionRef
	output.ReportRef = &reportRef
	output.MarkdownReportRef = &markdownRef
	return nil
}

func loadFrozenFindingGovernancePolicy(
	repository *runrepo.Repository,
	formalRunID string,
) (*reviewconfig.FindingGovernancePolicy, error) {
	snapshot, err := repository.ExecutionSnapshotForRun(formalRunID)
	if err != nil {
		return nil, fmt.Errorf("load formal execution snapshot for finding governance: %w", err)
	}
	data, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return nil, fmt.Errorf("read exact formal config for finding governance: %w", err)
	}
	bundle, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return nil, fmt.Errorf("decode exact formal config for finding governance: %w", err)
	}
	if bundle.FindingGovernance == nil {
		return nil, nil
	}
	policy := *bundle.FindingGovernance
	policy.CalibrationProfile.Points = slices.Clone(policy.CalibrationProfile.Points)
	return &policy, nil
}

func isFormalWorkloadTerminal(state scheduling.WorkloadState) bool {
	return state == scheduling.StateSucceeded || state == scheduling.StateFailed ||
		state == scheduling.StateCanceled
}

func outcomeTerminalKind(
	outcome application.FormalAgentReviewOutcome,
) runmodel.AgentStageTerminalGateKind {
	if outcome.Success != nil {
		return outcome.Success.Gate.Kind
	}
	if outcome.Terminal != nil {
		return outcome.Terminal.Gate.Kind
	}
	return ""
}

func schedulingStateForExecutionStatus(
	status contractsv1alpha1.StageExecutionStatus,
) scheduling.WorkloadState {
	switch status {
	case contractsv1alpha1.StageExecutionSucceeded:
		return scheduling.StateSucceeded
	case contractsv1alpha1.StageExecutionCanceled:
		// The current scheduling callback contract has succeeded/failed/unknown;
		// provider cancellation is therefore an admitted failed workload while
		// the exact provider status remains "canceled" in the formal terminal.
		return scheduling.StateFailed
	default:
		return scheduling.StateFailed
	}
}

func writeFormalAgentRunOutput(
	stdout io.Writer,
	jsonOutput bool,
	output formalAgentRunOutput,
) error {
	if jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			return err
		}
	} else {
		hypotheses := 0
		if output.Hypotheses != nil {
			hypotheses = len(output.Hypotheses.Hypotheses)
		}
		findings := 0
		if output.Report != nil {
			findings = len(output.Report.Findings)
		}
		if _, err := fmt.Fprintf(stdout,
			"formal agent review %s run=%s execution=%s hypotheses=%d findings=%d terminal=%s workload=%s recovered_dispatch=%t recovered_admission=%t store=%s\n",
			output.Status, output.FormalRunID, output.ExecutionID, hypotheses, findings,
			output.TerminalAuthority, output.WorkloadState,
			output.RecoveredDispatch, output.RecoveredAdmission, output.StorePath,
		); err != nil {
			return err
		}
	}
	if output.Status != contractsv1alpha1.StageExecutionSucceeded {
		return fmt.Errorf("formal agent review ended with %s", output.Status)
	}
	return nil
}

func ensureFormalWorkload(
	ctx context.Context,
	repository *scheduling.Repository,
	initialized formalreview.InitializedRun,
	subject application.AgentPlanningSubject,
	workerID string,
	timeoutMS int64,
) (scheduling.DispatchLease, error) {
	workloadID := initialized.RunID + "-workload"
	record, err := repository.Get(workloadID)
	if errors.Is(err, scheduling.ErrNotFound) {
		now := time.Now().UTC()
		deadline := now.Add(time.Duration(timeoutMS) * time.Millisecond)
		workloadClass := scheduling.ClassInteractive
		if initialized.Kind == runmodel.RunKindReplay {
			workloadClass = scheduling.ClassEvalReplay
		}
		record, err = repository.Submit(ctx, scheduling.WorkloadSpec{
			SchemaVersion: scheduling.WorkloadSchemaVersion,
			WorkloadID:    workloadID, RunID: initialized.RunID,
			TenantID: subject.TenantID, Class: workloadClass,
			Priority: 100, InputRef: initialized.Snapshot.ReviewInputRef.URI,
			SubmittedAt: now, ExecutionDeadline: deadline,
		}, scheduling.Mutation{
			IdempotencyKey: initialized.RunID + "-submit",
			Actor:          workerID, Audit: "submit formal local Pi review", At: now,
		})
	}
	if err != nil {
		return scheduling.DispatchLease{}, fmt.Errorf("resolve formal workload: %w", err)
	}
	if record.State == scheduling.StateLeased && record.ActiveLease != nil {
		if record.ActiveLease.WorkerID != workerID {
			return scheduling.DispatchLease{}, fmt.Errorf("formal workload is leased by another worker")
		}
		return *record.ActiveLease, nil
	}
	if record.State != scheduling.StatePending {
		return scheduling.DispatchLease{}, fmt.Errorf("formal workload is not dispatchable: %s", record.State)
	}
	supportedClass := scheduling.ClassInteractive
	if initialized.Kind == runmodel.RunKindReplay {
		supportedClass = scheduling.ClassEvalReplay
	}
	dispatch, err := repository.Claim(ctx, scheduling.ClaimRequest{
		IdempotencyKey: initialized.RunID + "-claim", WorkloadID: workloadID,
		WorkerID: workerID, SupportedClasses: []scheduling.WorkloadClass{supportedClass},
		At: time.Now().UTC(),
	})
	if err != nil {
		return scheduling.DispatchLease{}, fmt.Errorf("claim formal workload: %w", err)
	}
	return dispatch.Lease, nil
}

func completeFormalWorkload(
	ctx context.Context,
	repository *scheduling.Repository,
	lease scheduling.DispatchLease,
	result contractsv1alpha1.StageExecutionResult,
) error {
	status := scheduling.CallbackFailed
	outputRefs := []string{}
	failureCode := "formal_agent_failed"
	if result.Status == contractsv1alpha1.StageExecutionSucceeded {
		status = scheduling.CallbackSucceeded
		failureCode = ""
		outputRefs = []string{result.Output.Ref.URI}
	} else if result.Failure != nil {
		failureCode = result.Failure.Code
	}
	_, err := repository.Complete(ctx, scheduling.Callback{
		SchemaVersion:  scheduling.CallbackSchemaVersion,
		IdempotencyKey: result.ExecutionID + "-scheduling-terminal",
		LeaseID:        lease.LeaseID, WorkloadID: lease.WorkloadID, WorkerID: lease.WorkerID,
		Attempt: lease.Attempt, Generation: lease.Generation, FencingToken: lease.FencingToken,
		Status: status, OutputRefs: outputRefs, FailureCode: failureCode,
		OccurredAt: result.RecordedAt,
	})
	return err
}
