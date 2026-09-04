package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/abietic/argus/internal/agentshadow"
	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const agentReviewUsage = `usage:
  argus agent-review quick ...
  argus agent-review run --store <absolute-dir> --source-run <id> --idempotency-key <key> --node <absolute-file> --worker-script <absolute-file> --provider-profile <deepseek-anthropic-env|anthropic-official> --model <id> [--skill <builtin>]... [--max-files <n>] [--max-groups <n>] [--max-candidates <n>] [--max-model-calls <n>] [--max-tool-calls <n>] [--max-output-tokens <n>] [--max-group-bytes <n>] [--max-target-bytes <n>] [--timeout-ms <n>] [--max-concurrency <n>] [--allow-partial] [--json]
  argus agent-review show --store <absolute-dir> --manifest-id <id> [--json]
  argus agent-review evidence read --store <absolute-dir> --manifest-id <id> --request-id <id> --actor <id> --purpose <local_debug|evaluation_replay|incident_investigation> --at <RFC3339-UTC> --acknowledge-sensitive-output --json
  argus agent-review evidence revoke --store <absolute-dir> --manifest-id <id> --idempotency-key <id> --actor <id> --reason <text> --at <RFC3339-UTC> [--json]
  argus agent-review execution list --store <absolute-dir> --start <RFC3339-UTC> --end <RFC3339-UTC> [--json]
  argus agent-review execution show --store <absolute-dir> --execution <id> [--json]
  argus agent-review execution reconcile --store <absolute-dir> --execution <id> [--json]
  argus agent-review analytics rebuild --store <absolute-dir> --snapshot <id> --start <RFC3339-UTC> --end <RFC3339-UTC> --built-at <RFC3339-UTC> [--group-by <dimension>]... [--json]
  argus agent-review analytics show --store <absolute-dir> --snapshot <id> [--json]
  argus agent-review analytics export --store <absolute-dir> --snapshot <id> --target <facts|projection> --format <json|csv> --output-dir <absolute-new-dir> [--json]
  argus agent-review formal bootstrap ...

The local worker always uses frozen ReviewInput, independent verification, read-only in-memory tools, remote writes denied, and local/local scope.
Agent analytics are diagnostic-only, immutable, non-attested, and never enter the Finding/value funnel.`

type agentReviewRunFlags struct {
	store           string
	sourceRun       string
	idempotencyKey  string
	node            string
	workerScript    string
	providerProfile string
	model           string
	skills          stringList
	budget          contractsv1alpha1.AgentReviewBudget
	json            bool
	allowPartial    bool
}

type agentReviewShowFlags struct {
	store      string
	manifestID string
	json       bool
}

type agentReviewRunAttemptOutput struct {
	Attempt                    agentshadow.ExecutionAttempt         `json:"attempt"`
	UnconfirmedCommittedResult *agentReviewCommittedResultReference `json:"unconfirmed_committed_result,omitempty"`
	Reused                     bool                                 `json:"reused"`
	OutcomeAcknowledged        bool                                 `json:"outcome_acknowledged"`
	StorePath                  string                               `json:"store_path"`
}

type agentReviewCommittedResultReference struct {
	ManifestID    string                                 `json:"manifest_id"`
	ObservationID string                                 `json:"observation_id"`
	Status        contractsv1alpha1.AgentReviewRunStatus `json:"status"`
}

func runAgentReview(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s", agentReviewUsage)
	}
	switch arguments[0] {
	case "quick":
		options, err := parseAgentReviewQuickFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review quick flags: %w\n%s", err, agentReviewQuickUsage)
		}
		return executeAgentReviewQuick(ctx, options, stdout)
	case "run":
		options, err := parseAgentReviewRunFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review run flags: %w\n%s", err, agentReviewUsage)
		}
		return executeAgentReviewRun(ctx, options, stdout)
	case "show":
		options, err := parseAgentReviewShowFlags(arguments[1:])
		if err != nil {
			return fmt.Errorf("agent-review show flags: %w\n%s", err, agentReviewUsage)
		}
		return executeAgentReviewShow(ctx, options, stdout)
	case "evidence":
		return runAgentReviewEvidence(ctx, arguments[1:], stdout)
	case "analytics":
		return runAgentReviewAnalytics(ctx, arguments[1:], stdout)
	case "execution":
		return runAgentReviewExecution(ctx, arguments[1:], stdout)
	case "formal":
		return runAgentReviewFormal(ctx, arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown agent-review action %q\n%s", arguments[0], agentReviewUsage)
	}
}

func parseAgentReviewRunFlags(arguments []string) (agentReviewRunFlags, error) {
	options := agentReviewRunFlags{
		budget: contractsv1alpha1.AgentReviewBudget{
			MaxFiles: 32, MaxGroups: 8, MaxCandidates: 32,
			MaxModelCalls: 96, MaxToolCalls: 24, MaxOutputTokens: 8192,
			MaxGroupBytes: 65536, MaxTargetBytes: 4194304,
			TimeoutMS: 180000, MaxConcurrency: 4,
		},
	}
	maxFiles := uint(options.budget.MaxFiles)
	maxGroups := uint(options.budget.MaxGroups)
	maxCandidates := uint(options.budget.MaxCandidates)
	maxModelCalls := uint(options.budget.MaxModelCalls)
	maxToolCalls := uint(options.budget.MaxToolCalls)
	maxOutputTokens := uint(options.budget.MaxOutputTokens)
	maxConcurrency := uint(options.budget.MaxConcurrency)
	flags := newFlagSet("agent-review run")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.sourceRun, "source-run", "", "committed source ReviewRun ID")
	flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "immutable execution key")
	flags.StringVar(&options.node, "node", "", "absolute Node.js executable")
	flags.StringVar(&options.workerScript, "worker-script", "", "absolute Pi worker script")
	flags.StringVar(&options.providerProfile, "provider-profile", "", "provider profile")
	flags.StringVar(&options.model, "model", "", "provider model ID")
	flags.Var(&options.skills, "skill", "builtin review dimension; repeatable")
	flags.UintVar(&maxFiles, "max-files", maxFiles, "maximum frozen files")
	flags.UintVar(&maxGroups, "max-groups", maxGroups, "maximum groups")
	flags.UintVar(&maxCandidates, "max-candidates", maxCandidates, "maximum retained candidates")
	flags.UintVar(&maxModelCalls, "max-model-calls", maxModelCalls, "maximum provider turns")
	flags.UintVar(&maxToolCalls, "max-tool-calls", maxToolCalls, "maximum tools per task")
	flags.UintVar(&maxOutputTokens, "max-output-tokens", maxOutputTokens, "maximum output tokens per turn")
	flags.Uint64Var(&options.budget.MaxGroupBytes, "max-group-bytes", options.budget.MaxGroupBytes, "maximum group bytes")
	flags.Uint64Var(&options.budget.MaxTargetBytes, "max-target-bytes", options.budget.MaxTargetBytes, "maximum frozen input bytes")
	flags.Uint64Var(&options.budget.TimeoutMS, "timeout-ms", options.budget.TimeoutMS, "worker deadline in milliseconds")
	flags.UintVar(&maxConcurrency, "max-concurrency", maxConcurrency, "maximum concurrent Agent tasks")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	flags.BoolVar(&options.allowPartial, "allow-partial", false, "return success for partial shadow coverage")
	if err := flags.Parse(arguments); err != nil {
		return agentReviewRunFlags{}, err
	}
	if flags.NArg() != 0 {
		return agentReviewRunFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if maxFiles > uint(^uint32(0)) || maxGroups > uint(^uint32(0)) ||
		maxCandidates > uint(^uint32(0)) || maxModelCalls > uint(^uint32(0)) ||
		maxToolCalls > uint(^uint32(0)) || maxOutputTokens > uint(^uint32(0)) ||
		maxConcurrency > uint(^uint32(0)) {
		return agentReviewRunFlags{}, fmt.Errorf("numeric budget exceeds uint32")
	}
	options.budget.MaxFiles = uint32(maxFiles)
	options.budget.MaxGroups = uint32(maxGroups)
	options.budget.MaxCandidates = uint32(maxCandidates)
	options.budget.MaxModelCalls = uint32(maxModelCalls)
	options.budget.MaxToolCalls = uint32(maxToolCalls)
	options.budget.MaxOutputTokens = uint32(maxOutputTokens)
	options.budget.MaxConcurrency = uint32(maxConcurrency)
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return agentReviewRunFlags{}, err
	}
	for name, value := range map[string]string{
		"--source-run":       options.sourceRun,
		"--idempotency-key":  options.idempotencyKey,
		"--node":             options.node,
		"--worker-script":    options.workerScript,
		"--provider-profile": options.providerProfile,
		"--model":            options.model,
	} {
		if value == "" {
			return agentReviewRunFlags{}, fmt.Errorf("%s is required", name)
		}
	}
	for name, path := range map[string]string{
		"--node":          options.node,
		"--worker-script": options.workerScript,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return agentReviewRunFlags{}, fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	return options, nil
}

func parseAgentReviewShowFlags(arguments []string) (agentReviewShowFlags, error) {
	var options agentReviewShowFlags
	flags := newFlagSet("agent-review show")
	flags.StringVar(&options.store, "store", "", "absolute local Argus store")
	flags.StringVar(&options.manifestID, "manifest-id", "", "shadow result manifest ID")
	flags.BoolVar(&options.json, "json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return agentReviewShowFlags{}, err
	}
	if flags.NArg() != 0 {
		return agentReviewShowFlags{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if err := validateRequiredAbsoluteStore(options.store); err != nil {
		return agentReviewShowFlags{}, err
	}
	if options.manifestID == "" {
		return agentReviewShowFlags{}, fmt.Errorf("--manifest-id is required")
	}
	return options, nil
}

func executeAgentReviewRun(
	ctx context.Context,
	options agentReviewRunFlags,
	stdout io.Writer,
) error {
	repository, err := openAgentShadowRepository(options.store)
	if err != nil {
		return err
	}
	bridge, err := agentshadow.NewLocalBridge(
		repository,
		agentshadowworker.NewSubprocessRunner(),
	)
	if err != nil {
		return err
	}
	result, runErr := bridge.Run(ctx, agentshadow.LocalRunRequest{
		SourceRunID: options.sourceRun, IdempotencyKey: options.idempotencyKey,
		NodePath: options.node, WorkerScript: options.workerScript,
		ProviderProfile: options.providerProfile, Model: options.model,
		Skills: []string(options.skills), Budget: options.budget,
		Environment: agentReviewEnvironment(),
	})
	if runErr != nil {
		if result.Attempt != nil {
			if err := writeAgentReviewRunAttempt(
				stdout,
				options.json,
				options.store,
				result,
			); err != nil {
				return fmt.Errorf("%w; write execution attempt: %v", runErr, err)
			}
		}
		return runErr
	}
	return writeAgentReviewRunResult(stdout, options.json, options.allowPartial, result)
}

func writeAgentReviewRunAttempt(
	stdout io.Writer,
	jsonOutput bool,
	storePath string,
	result agentshadow.LocalRunResult,
) error {
	if result.Attempt == nil {
		return fmt.Errorf("execution attempt is required")
	}
	var committed *agentReviewCommittedResultReference
	if result.Result.Manifest.ManifestID != "" {
		committed = &agentReviewCommittedResultReference{
			ManifestID:    result.Result.Manifest.ManifestID,
			ObservationID: result.Result.Observation.ObservationID,
			Status:        result.Result.Manifest.Status,
		}
	}
	if jsonOutput {
		return writeJSON(stdout, agentReviewRunAttemptOutput{
			Attempt:                    *result.Attempt,
			UnconfirmedCommittedResult: committed,
			Reused:                     result.Reused,
			OutcomeAcknowledged:        result.OutcomeAcknowledged,
			StorePath:                  storePath,
		})
	}
	failureCode := "-"
	if result.Attempt.Failure != nil {
		failureCode = result.Attempt.Failure.Code
	}
	hostDiagnosis := "-"
	if result.Attempt.HostFailure != nil {
		hostDiagnosis = string(result.Attempt.HostFailure.ReasonCode)
	}
	unconfirmedManifest := "-"
	if committed != nil {
		unconfirmedManifest = committed.ManifestID
	}
	_, err := fmt.Fprintf(
		stdout,
		"agent execution %s execution=%s source_run=%s failure=%s host_diagnosis=%s reused=%t outcome_acknowledged=%t unconfirmed_manifest=%s observed_at=%s\n",
		result.Attempt.Status,
		result.Attempt.ExecutionID,
		result.Attempt.SourceRunID,
		failureCode,
		hostDiagnosis,
		result.Reused,
		result.OutcomeAcknowledged,
		unconfirmedManifest,
		result.Attempt.ObservedAt.Format(time.RFC3339Nano),
	)
	return err
}

func writeAgentReviewRunResult(
	stdout io.Writer,
	jsonOutput bool,
	allowPartial bool,
	result agentshadow.LocalRunResult,
) error {
	if jsonOutput {
		err := writeJSON(stdout, result)
		if err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(
			stdout,
			"agent review %s manifest=%s hypotheses=%d reused=%t outcome_acknowledged=%t\n",
			result.Result.Manifest.Status,
			result.Result.Manifest.ManifestID,
			len(result.Result.HypothesisSet.Hypotheses),
			result.Reused,
			result.OutcomeAcknowledged,
		); err != nil {
			return err
		}
	}
	if result.Result.Manifest.Status == contractsv1alpha1.AgentReviewRunFailed {
		return fmt.Errorf("agent review failed")
	}
	if result.Result.Manifest.Status == contractsv1alpha1.AgentReviewRunPartial &&
		!allowPartial {
		return fmt.Errorf("agent review coverage is partial; inspect the committed manifest or pass --allow-partial")
	}
	return nil
}

func executeAgentReviewShow(
	ctx context.Context,
	options agentReviewShowFlags,
	stdout io.Writer,
) error {
	repository, err := openAgentShadowRepository(options.store)
	if err != nil {
		return err
	}
	service, err := agentshadow.NewService(repository)
	if err != nil {
		return err
	}
	result, err := service.QueryRedacted(ctx, agentshadow.QueryRequest{
		Scope: agentshadow.Scope{
			TenantID:    agentshadow.LocalTenantID,
			WorkspaceID: agentshadow.LocalWorkspaceID,
		},
		ManifestID: options.manifestID,
	})
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, result)
	}
	_, err = fmt.Fprintf(
		stdout,
		"agent review %s manifest=%s source_run=%s hypotheses=%d\n",
		result.Manifest.Status,
		result.Manifest.ManifestID,
		result.Manifest.SourceRunID,
		len(result.HypothesisSet.Hypotheses),
	)
	return err
}

func openAgentShadowRepository(storePath string) (*agentshadow.Repository, error) {
	store, err := local.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("open local store: %w", err)
	}
	repository, err := agentshadow.OpenRepository(store, time.Now)
	if err != nil {
		return nil, err
	}
	return repository, nil
}

func validateRequiredAbsoluteStore(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("--store must be a clean absolute path")
	}
	return nil
}

func agentReviewEnvironment() map[string]string {
	result := make(map[string]string)
	for _, key := range []string{
		"LANG", "LC_ALL", "PATH", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL",
	} {
		if value, exists := os.LookupEnv(key); exists {
			result[key] = value
		}
	}
	return result
}
