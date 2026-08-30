package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abietic/argus/internal/agentshadowworker"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/identity"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
)

const evaluationRepeatabilityBatchUsage = `usage:
  argus evaluation repeatability batch run --store <absolute-dir> --input <absolute-json> --mutation <absolute-json> [--json] -- <formal exact replay flags>
  argus evaluation repeatability batch resume --store <absolute-dir> --batch <id> --access <absolute-json> [--json]
  argus evaluation repeatability batch show --store <absolute-dir> --batch <id> --access <absolute-json> [--json]

The exact replay flags are the same as "agent-review formal replay" without a
--change flag. The batch owns --store, --source-formal-run, --idempotency-key,
and --json.`

type repeatabilityBatchOutput struct {
	Result    evaluation.RepeatabilityBatchResult `json:"result"`
	StorePath string                              `json:"store_path"`
}

type repeatabilityBatchRecordOutput struct {
	Record    evaluation.RepeatabilityBatchRecord `json:"record"`
	StorePath string                              `json:"store_path"`
}

func runEvaluationRepeatabilityBatch(ctx context.Context, arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(evaluationRepeatabilityBatchUsage)
	}
	switch arguments[0] {
	case "run":
		return runEvaluationRepeatabilityBatchExecution(ctx, arguments[1:], stdout)
	case "resume":
		return runEvaluationRepeatabilityBatchResumeWithRunner(
			ctx, arguments[1:], stdout, agentshadowworker.NewSubprocessRunner(),
		)
	case "show":
		options, err := parseGovernedReadFlags(
			"evaluation repeatability batch show", arguments[1:],
			evaluationRepeatabilityBatchUsage, "batch", "repeatability batch ID",
		)
		if err != nil {
			return err
		}
		access, err := readEvaluationAccess(options.access)
		if err != nil {
			return err
		}
		repository, storePath, err := openEvaluationRepository(options.store)
		if err != nil {
			return err
		}
		record, err := repository.GetRepeatabilityBatch(options.id, access)
		if err != nil {
			return err
		}
		if options.json {
			return writeJSON(stdout, repeatabilityBatchRecordOutput{Record: record, StorePath: storePath})
		}
		_, err = fmt.Fprintf(stdout, "repeatability_batch=%s status=%s completed_samples=%d store=%s\n",
			record.Request.BatchID, record.Status, len(record.CompletedCases), storePath)
		return err
	default:
		return fmt.Errorf("unknown evaluation repeatability batch command %q\n%s", arguments[0], evaluationRepeatabilityBatchUsage)
	}
}

func runEvaluationRepeatabilityBatchResumeWithRunner(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	if runner == nil {
		return fmt.Errorf("formal Pi repeatability batch runner is required")
	}
	options, err := parseGovernedReadFlags(
		"evaluation repeatability batch resume", arguments,
		evaluationRepeatabilityBatchUsage, "batch", "repeatability batch ID",
	)
	if err != nil {
		return err
	}
	access, err := readEvaluationAccess(options.access)
	if err != nil {
		return err
	}
	repository, storePath, err := openEvaluationRepository(options.store)
	if err != nil {
		return err
	}
	record, err := repository.GetRepeatabilityBatch(options.id, access)
	if err != nil {
		return err
	}
	if record.Request.ExecutorTemplateRef == nil {
		return fmt.Errorf("repeatability batch has no durable executor template")
	}
	store, err := local.Open(storePath)
	if err != nil {
		return err
	}
	runs, err := runrepo.New(store)
	if err != nil {
		return err
	}
	executor, err := loadLocalFormalBatchExecutor(
		runs, storePath, *record.Request.ExecutorTemplateRef, runner,
	)
	if err != nil {
		return fmt.Errorf("load frozen formal repeatability executor template: %w", err)
	}
	workerID, err := identity.NewGenerator().New("repeatability-batch-worker")
	if err != nil {
		return fmt.Errorf("create repeatability batch worker identity: %w", err)
	}
	batchRunner, err := evaluation.NewRepeatabilityBatchRunner(
		repository, runs, executor, func() time.Time { return time.Now().UTC() },
		workerID, 5*time.Minute,
	)
	if err != nil {
		return err
	}
	result, err := batchRunner.Run(ctx, record.Request, record.Intent)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, repeatabilityBatchOutput{Result: result, StorePath: storePath})
	}
	_, err = fmt.Fprintf(stdout,
		"repeatability_batch=%s samples=%d cases=%d repeatability=%s store=%s\n",
		result.BatchID, len(result.ReplayEvaluationRunIDs), len(result.Cases),
		result.RepeatabilityRunID, storePath,
	)
	return err
}

func runEvaluationRepeatabilityBatchExecution(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) error {
	return runEvaluationRepeatabilityBatchExecutionWithRunner(
		ctx, arguments, stdout, agentshadowworker.NewSubprocessRunner(),
	)
}

func runEvaluationRepeatabilityBatchExecutionWithRunner(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	runner agentshadowworker.Runner,
) error {
	if runner == nil {
		return fmt.Errorf("formal Pi repeatability batch runner is required")
	}
	separator := -1
	for index, argument := range arguments {
		if argument == "--" {
			if separator >= 0 {
				return fmt.Errorf("evaluation repeatability batch accepts exactly one -- separator")
			}
			separator = index
		}
	}
	if separator < 0 || separator == len(arguments)-1 {
		return fmt.Errorf("formal exact replay flags are required after --\n%s", evaluationRepeatabilityBatchUsage)
	}
	options, err := parseGovernedWriteFlags(
		"evaluation repeatability batch run", arguments[:separator], evaluationRepeatabilityBatchUsage,
	)
	if err != nil {
		return err
	}
	request, err := readStrictDescriptor(
		options.input, "repeatability batch request", evaluation.DecodeRepeatabilityBatchRequest,
	)
	if err != nil {
		return err
	}
	if request.ExecutorTemplateRef != nil {
		return fmt.Errorf("repeatability batch input must omit executor_template_ref; the local adapter freezes it")
	}
	mutation, err := readEvaluationMutation(options.mutation)
	if err != nil {
		return err
	}
	formalArguments := arguments[separator+1:]
	for _, argument := range formalArguments {
		for _, forbidden := range []string{"--store", "--source-formal-run", "--idempotency-key", "--json"} {
			if argument == forbidden || strings.HasPrefix(argument, forbidden+"=") {
				return fmt.Errorf("formal repeatability template must not set %s", forbidden)
			}
		}
	}
	formalArguments = append([]string{
		"--store", options.store,
		"--source-formal-run", "repeatability-source-placeholder",
		"--idempotency-key", "repeatability-idempotency-placeholder",
	}, formalArguments...)
	formal, err := parseFormalAgentReplayFlags(formalArguments)
	if err != nil {
		return fmt.Errorf("formal exact replay template: %w", err)
	}
	if formal.change != runmodel.ReplayVariableNone {
		return fmt.Errorf("repeatability batch requires exact replay with no changed variable")
	}
	if request.ExecutorRevision != localFormalBatchExecutorRevision {
		return fmt.Errorf("repeatability batch executor_revision must be %q", localFormalBatchExecutorRevision)
	}
	repository, storePath, err := openEvaluationRepository(options.store)
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
	templateRef, err := persistLocalFormalBatchExecutorTemplate(runs, formal)
	if err != nil {
		return fmt.Errorf("freeze formal repeatability executor template: %w", err)
	}
	request.ExecutorTemplateRef = &templateRef
	executor, err := loadLocalFormalBatchExecutor(runs, storePath, templateRef, runner)
	if err != nil {
		return fmt.Errorf("load frozen formal repeatability executor template: %w", err)
	}
	workerID, err := identity.NewGenerator().New("repeatability-batch-worker")
	if err != nil {
		return fmt.Errorf("create repeatability batch worker identity: %w", err)
	}
	batchRunner, err := evaluation.NewRepeatabilityBatchRunner(
		repository, runs, executor,
		func() time.Time { return time.Now().UTC() },
		workerID, 5*time.Minute,
	)
	if err != nil {
		return err
	}
	result, err := batchRunner.Run(ctx, request, mutation)
	if err != nil {
		return err
	}
	if options.json {
		return writeJSON(stdout, repeatabilityBatchOutput{Result: result, StorePath: storePath})
	}
	_, err = fmt.Fprintf(stdout,
		"repeatability_batch=%s samples=%d cases=%d repeatability=%s store=%s\n",
		result.BatchID, len(result.ReplayEvaluationRunIDs), len(result.Cases),
		result.RepeatabilityRunID, storePath)
	return err
}
