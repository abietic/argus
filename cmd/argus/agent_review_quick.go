package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
)

const agentReviewQuickUsage = `usage:
  argus agent-review quick --store <absolute-dir> --config-state-dir <absolute-dir> --idempotency-key <key> --at <RFC3339-UTC> --node <absolute-file> --worker-script <absolute-file> --provider-profile <deepseek-anthropic-env|anthropic-official> --model <id> --input-micros-per-million <n> --output-micros-per-million <n> --max-bytes-per-input-token <n> [formal run flags] [--json] -- <review target flags>
  argus agent-review quick --store <absolute-dir> --config-state-dir <absolute-dir> --source-run <id> --idempotency-key <same-key> --at <same-RFC3339-UTC> --node <absolute-file> --worker-script <absolute-file> --provider-profile <deepseek-anthropic-env|anthropic-official> --model <id> --input-micros-per-million <n> --output-micros-per-million <n> --max-bytes-per-input-token <n> [formal run flags] [--json]

The first form materializes one source review, publishes the exact formal configuration, and runs the governed Pi review. Review target flags after -- are the same flags accepted by argus review, except --store, --config-state-dir, and --json, which are owned by quick.

If bootstrap or formal execution fails, the output retains source_run_id. Resume with the second form and the same idempotency key and publication time. Repeating the first form intentionally creates a new source review; it is not an exact retry.`

const quickPendingSourceRun = "quick-source-run-pending"

type agentReviewQuickFlags struct {
	formal        formalAgentRunFlags
	publicationAt string
	rootKey       string
	reviewArgs    []string
	resume        bool
	json          bool
}

type agentReviewQuickOutput struct {
	Phase           string                      `json:"phase"`
	SourceRunID     string                      `json:"source_run_id"`
	Source          *runOutput                  `json:"source,omitempty"`
	Bootstrap       *formalAgentBootstrapOutput `json:"bootstrap,omitempty"`
	Formal          *formalAgentRunOutput       `json:"formal,omitempty"`
	ResumeSourceRun string                      `json:"resume_source_run"`
	StorePath       string                      `json:"store_path"`
}

func parseAgentReviewQuickFlags(arguments []string) (agentReviewQuickFlags, error) {
	formalArgs, reviewArgs, separated := splitQuickArguments(arguments)
	formalArgs, publicationAt, err := removeQuickStringFlag(formalArgs, "at")
	if err != nil {
		return agentReviewQuickFlags{}, err
	}
	if publicationAt == "" {
		return agentReviewQuickFlags{}, fmt.Errorf("--at is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, publicationAt); err != nil {
		return agentReviewQuickFlags{}, fmt.Errorf("--at must be RFC3339: %w", err)
	}
	hasSourceRun := hasCLIFlag(formalArgs, "source-run")
	if !hasSourceRun {
		formalArgs = append(formalArgs, "--source-run", quickPendingSourceRun)
	}
	formal, err := parseFormalAgentRunFlags(formalArgs)
	if err != nil {
		return agentReviewQuickFlags{}, err
	}
	rootKey := formal.idempotencyKey
	formal.idempotencyKey = rootKey + "-run"
	jsonOutput := formal.json
	formal.json = true
	if hasSourceRun {
		if separated || len(reviewArgs) != 0 {
			return agentReviewQuickFlags{}, fmt.Errorf("review target flags are not allowed with --source-run")
		}
		return agentReviewQuickFlags{
			formal: formal, publicationAt: publicationAt, rootKey: rootKey,
			resume: true, json: jsonOutput,
		}, nil
	}
	formal.sourceRun = ""
	if !separated || len(reviewArgs) == 0 {
		return agentReviewQuickFlags{}, fmt.Errorf("review target flags after -- are required without --source-run")
	}
	for _, argument := range reviewArgs {
		for _, reserved := range []string{"store", "config-state-dir", "json"} {
			if argument == "--"+reserved || strings.HasPrefix(argument, "--"+reserved+"=") {
				return agentReviewQuickFlags{}, fmt.Errorf("--%s is owned by agent-review quick", reserved)
			}
		}
	}
	return agentReviewQuickFlags{
		formal: formal, publicationAt: publicationAt, rootKey: rootKey,
		reviewArgs: append([]string(nil), reviewArgs...), json: jsonOutput,
	}, nil
}

func splitQuickArguments(arguments []string) (formalArgs []string, reviewArgs []string, separated bool) {
	for index, argument := range arguments {
		if argument == "--" {
			return append([]string(nil), arguments[:index]...),
				append([]string(nil), arguments[index+1:]...), true
		}
	}
	return append([]string(nil), arguments...), nil, false
}

func removeQuickStringFlag(arguments []string, name string) ([]string, string, error) {
	prefix := "--" + name
	filtered := make([]string, 0, len(arguments))
	var value string
	found := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == prefix {
			if found {
				return nil, "", fmt.Errorf("%s may be specified only once", prefix)
			}
			if index+1 >= len(arguments) {
				return nil, "", fmt.Errorf("%s requires a value", prefix)
			}
			value = arguments[index+1]
			found = true
			index++
			continue
		}
		if strings.HasPrefix(argument, prefix+"=") {
			if found {
				return nil, "", fmt.Errorf("%s may be specified only once", prefix)
			}
			value = strings.TrimPrefix(argument, prefix+"=")
			found = true
			continue
		}
		filtered = append(filtered, argument)
	}
	return filtered, value, nil
}

func hasCLIFlag(arguments []string, name string) bool {
	prefix := "--" + name
	for _, argument := range arguments {
		if argument == prefix || strings.HasPrefix(argument, prefix+"=") {
			return true
		}
	}
	return false
}

func executeAgentReviewQuickWithRunner(
	ctx context.Context,
	options agentReviewQuickFlags,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	output := agentReviewQuickOutput{Phase: "source", StorePath: options.formal.store}
	if options.resume {
		output.SourceRunID = options.formal.sourceRun
	} else {
		reviewArgs := append([]string(nil), options.reviewArgs...)
		reviewArgs = append(reviewArgs, "--store", options.formal.store, "--json")
		var sourceBuffer bytes.Buffer
		if err := runReview(ctx, reviewArgs, &sourceBuffer); err != nil {
			return fmt.Errorf("quick source review: %w", err)
		}
		var source runOutput
		if err := json.Unmarshal(sourceBuffer.Bytes(), &source); err != nil {
			return fmt.Errorf("decode quick source review output: %w", err)
		}
		output.SourceRunID = source.Run.RunID
		output.Source = &source
	}
	output.ResumeSourceRun = output.SourceRunID
	options.formal.sourceRun = output.SourceRunID

	bootstrapFlags := formalAgentBootstrapFlags{
		store: options.formal.store, configState: options.formal.configState,
		sourceRun: output.SourceRunID, idempotencyKey: options.rootKey + "-bootstrap",
		at: options.publicationAt, node: options.formal.node,
		workerScript:    options.formal.workerScript,
		providerProfile: options.formal.providerProfile, model: options.formal.model,
		knowledge: append(stringList(nil), options.formal.knowledge...), json: true,
		options: options.formal.options,
	}
	output.Phase = "bootstrap"
	var bootstrapBuffer bytes.Buffer
	if err := executeFormalAgentBootstrap(ctx, bootstrapFlags, &bootstrapBuffer); err != nil {
		writeErr := writeAgentReviewQuickOutput(stdout, options.json, output)
		if writeErr != nil {
			return fmt.Errorf("quick bootstrap: %w; write quick result: %v", err, writeErr)
		}
		return fmt.Errorf("quick bootstrap: %w", err)
	}
	var bootstrap formalAgentBootstrapOutput
	if err := json.Unmarshal(bootstrapBuffer.Bytes(), &bootstrap); err != nil {
		return fmt.Errorf("decode quick bootstrap output: %w", err)
	}
	output.Bootstrap = &bootstrap

	output.Phase = "formal"
	var formalBuffer bytes.Buffer
	formalErr := executeFormalAgentRunWithRunner(ctx, options.formal, &formalBuffer, runner)
	if formalBuffer.Len() != 0 {
		var formal formalAgentRunOutput
		if err := json.Unmarshal(formalBuffer.Bytes(), &formal); err != nil {
			return fmt.Errorf("decode quick formal output: %w", err)
		}
		output.Formal = &formal
	}
	if formalErr == nil {
		output.Phase = "succeeded"
	}
	if err := writeAgentReviewQuickOutput(stdout, options.json, output); err != nil {
		if formalErr != nil {
			return fmt.Errorf("quick formal review: %w; write quick result: %v", formalErr, err)
		}
		return err
	}
	if formalErr != nil {
		return fmt.Errorf("quick formal review: %w", formalErr)
	}
	return nil
}

func writeAgentReviewQuickOutput(stdout io.Writer, jsonOutput bool, output agentReviewQuickOutput) error {
	if jsonOutput {
		return writeJSON(stdout, output)
	}
	formalRunID := ""
	status := ""
	if output.Formal != nil {
		formalRunID = output.Formal.FormalRunID
		status = string(output.Formal.Status)
	}
	_, err := fmt.Fprintf(
		stdout,
		"quick agent review phase=%s source_run=%s formal_run=%s status=%s resume_source_run=%s store=%s\n",
		output.Phase, output.SourceRunID, formalRunID, status,
		output.ResumeSourceRun, output.StorePath,
	)
	return err
}

func executeAgentReviewQuick(ctx context.Context, options agentReviewQuickFlags, stdout io.Writer) error {
	return executeAgentReviewQuickWithRunner(
		ctx, options, stdout, agentshadowworker.NewSubprocessRunner(),
	)
}
