#!/usr/bin/env node

import { pathToFileURL } from "node:url";

import {
  PiAgentRuntime,
  type AgentRuntime,
  type PiAgentRuntimeOptions,
  type PiProviderProfile,
} from "./agent.js";
import { classifyAgentTaskFailure, errorMessage } from "./errors.js";
import {
  createFailureResult,
  createSucceededResult,
  decodeContextArtifacts,
  decodeGroupCheckpointBytes,
  decodeKnowledgePacks,
  decodePromptBundle,
  decodeReviewSkills,
  decodeRulePack,
  decodeReviewInputBytes,
  decodeWorkerRequest,
  serializeWorkerResult,
  parseStrictJson,
  sha256,
  type AgentReviewPlan,
  type WorkerRequest,
  type WorkerResult,
} from "./protocol.js";
import { decodeFrozenReviewInput, materializeFrozenTarget } from "./target.js";
import type { ProgressEvent, ReviewOptions, SkillDefinition } from "./types.js";
import { fitTaskEvidenceCollection } from "./evidence.js";
import {
  runInjectedReviewWorkflow,
  type WorkflowGroupCheckpoint,
} from "./workflow.js";

const MAX_STDIN_BYTES = 128 * 1024 * 1024;

export interface WorkerProgress {
  schema_version: "argus.agent_review_worker_progress.v1alpha1";
  work_item_id: string;
  phase:
    | "admission"
    | "target"
    | ProgressEvent["phase"]
    | "tool"
    | "checkpoint"
    | "result";
  message: string;
  observed_at: string;
  group_id?: string;
  skill_id?: string;
  candidate_id?: string;
  checkpoint?: {
    group_id: string;
    checkpoint_revision: number;
    checkpoint_scope_sha256: string;
    content_sha256: string;
    size_bytes: number;
    content_base64: string;
  };
}

export interface WorkerDependencies {
  createRuntime?: (options: PiAgentRuntimeOptions) => AgentRuntime;
  loadSkills?: (
    refs: AgentReviewPlan["review_dimensions"],
  ) => Promise<SkillDefinition[]>;
  now?: () => Date;
}

export async function executeWorkerRequest(
  request: WorkerRequest,
  environment: NodeJS.ProcessEnv = process.env,
  emit: (progress: WorkerProgress) => void = () => undefined,
  dependencies: WorkerDependencies = {},
  outerSignal?: AbortSignal,
): Promise<WorkerResult> {
  const now = dependencies.now ?? (() => new Date());
  const emitProgress = (
    phase: WorkerProgress["phase"],
    message: string,
    detail: Partial<
      Pick<WorkerProgress, "group_id" | "skill_id" | "candidate_id"> &
        Pick<WorkerProgress, "checkpoint">
    > = {},
  ): void =>
    emit({
      schema_version: "argus.agent_review_worker_progress.v1alpha1",
      work_item_id: request.work_item_id,
      phase,
      message,
      observed_at: now().toISOString(),
      ...detail,
    });

  try {
    emitProgress("admission", "validating frozen worker admission");
    const current = now();
    const deadlineMS = Date.parse(request.deadline);
    const createdMS = Date.parse(request.plan.created_at);
    if (deadlineMS <= createdMS) {
      throw new WorkerAdmissionError(
        "invalid_deadline",
        "request deadline must be after plan.created_at",
      );
    }
    validateSupportedPlan(request.plan);
    if (current.getTime() >= deadlineMS) {
      return createFailureResult(
        request,
        "canceled",
        {
          code: "deadline_exceeded",
          message: "request deadline elapsed before worker execution",
          retryable: true,
        },
        current.toISOString(),
      );
    }
    const inputBytes = decodeReviewInputBytes(request);
    const promptBundle = decodePromptBundle(request);
    const knowledgeBundle = decodeKnowledgePacks(request);
    const governedRulePack = decodeRulePack(request);
    const frozenInput = decodeFrozenReviewInput(inputBytes);
    const frozenContext = decodeContextArtifacts(
      request,
      frozenInput.contexts.flatMap((binding) =>
        "ref" in binding ? [binding.ref] : [],
      ),
    );
    const referenceBundle = {
      ...knowledgeBundle,
      content: [governedRulePack, frozenContext, knowledgeBundle.content]
        .filter(Boolean)
        .join("\n\n"),
    };
    const resumeCheckpoints = decodeGroupCheckpointBytes(request).map(
      (content, index): WorkflowGroupCheckpoint => {
        const value = parseStrictJson(content.toString("utf8"));
        if (
          typeof value !== "object" ||
          value === null ||
          Array.isArray(value)
        ) {
          throw new WorkerAdmissionError(
            "invalid_group_checkpoint",
            `group checkpoint ${index} must be a JSON object`,
          );
        }
        const transport = request.group_checkpoints[index];
        const object = value as Record<string, unknown>;
        const group = object.group as Record<string, unknown> | undefined;
        if (
          !transport ||
          object.checkpointRevision !== transport.checkpoint_revision ||
          object.checkpointScopeSha256 !== transport.checkpoint_scope_sha256 ||
          group?.id !== transport.group_id
        ) {
          throw new WorkerAdmissionError(
            "invalid_group_checkpoint",
            `group checkpoint ${index} transport identity changed`,
          );
        }
        return value as unknown as WorkflowGroupCheckpoint;
      },
    );
    emitProgress("target", "materializing immutable in-memory review target");
    const target = materializeFrozenTarget(
      inputBytes,
      request.plan.target_digest,
      current.toISOString(),
      request.context_artifacts.map((artifact) => artifact.context_id),
    );
    if (target.files.length > request.plan.budget.max_files) {
      throw new WorkerAdmissionError(
        "target_file_budget_exceeded",
        "frozen target exceeds plan.budget.max_files",
      );
    }
    const skills = dependencies.loadSkills
      ? await dependencies.loadSkills(request.plan.review_dimensions)
      : decodeReviewSkills(request);
    const profile = providerProfileForPlan(request.plan, environment);
    const options = reviewOptionsForPlan(request.plan, target);
    const runtimeOptions: PiAgentRuntimeOptions = {
      model: options.model,
      profile,
      maxToolCalls: options.maxToolCalls,
      maxModelCalls: options.maxModelCalls,
      maxOutputTokens: options.maxOutputTokens,
      timeoutMs: options.timeoutMs,
      maxEvidenceBytes: Math.floor(request.capability.max_output_bytes / 2),
      promptBundle,
      initialModelCalls: resumeCheckpoints.reduce(
        (total, checkpoint) =>
          total +
          checkpoint.observations.reduce(
            (groupTotal, observation) =>
              groupTotal + observation.providerTurnsStarted,
            0,
          ),
        0,
      ),
      verbose: (message) => emitProgress("tool", message),
    };
    const runtime = (
      dependencies.createRuntime ??
      ((runtimeConfig) => new PiAgentRuntime(runtimeConfig))
    )(runtimeOptions);

    const deadlineSignal = AbortSignal.timeout(
      Math.min(2_147_483_647, Math.max(1, deadlineMS - now().getTime())),
    );
    const signal = outerSignal
      ? AbortSignal.any([outerSignal, deadlineSignal])
      : deadlineSignal;
    const report = await runInjectedReviewWorkflow(
      options,
      runtime,
      signal,
      target,
      skills,
      referenceBundle,
      (event) =>
        emitProgress(event.phase, event.message, {
          ...(event.groupId ? { group_id: event.groupId } : {}),
          ...(event.skillId ? { skill_id: event.skillId } : {}),
          ...(event.candidateId ? { candidate_id: event.candidateId } : {}),
        }),
      {
        checkpointScopeSha256: request.checkpoint_scope_sha256,
        resumeCheckpoints,
        onGroupCheckpoint(checkpoint) {
          const content = Buffer.from(JSON.stringify(checkpoint));
          emitProgress("checkpoint", `${checkpoint.group.id}: checkpoint`, {
            group_id: checkpoint.group.id,
            checkpoint: {
              group_id: checkpoint.group.id,
              checkpoint_revision: checkpoint.checkpointRevision,
              checkpoint_scope_sha256: request.checkpoint_scope_sha256,
              content_sha256: sha256(content),
              size_bytes: content.length,
              content_base64: content.toString("base64"),
            },
          });
        },
      },
    );
    fitWorkerReportEvidence(report, request.capability.max_output_bytes);
    const completedAt = now();
    if (completedAt.getTime() > deadlineMS) {
      return createFailureResult(
        request,
        "canceled",
        {
          code: "deadline_exceeded",
          message: "request deadline elapsed before report completion",
          retryable: true,
        },
        completedAt.toISOString(),
      );
    }
    emitProgress("result", "serializing bounded worker report");
    return createSucceededResult(
      request,
      report as unknown as Record<string, unknown>,
      completedAt.toISOString(),
    );
  } catch (error) {
    const classified = classifyAgentTaskFailure(error, outerSignal);
    const canceled = classified.status === "aborted";
    const admission = error instanceof WorkerAdmissionError;
    const code = canceled
      ? "execution_canceled"
      : admission
        ? error.code
        : classified.code === "provider_error"
          ? "provider_error"
          : classified.code === "timeout"
            ? "deadline_exceeded"
            : "worker_execution_failed";
    return createFailureResult(
      request,
      canceled ? "canceled" : "failed",
      {
        code,
        message: boundedErrorMessage(error),
        retryable: !admission && !canceled,
      },
      now().toISOString(),
    );
  }
}

function fitWorkerReportEvidence(
  report: Awaited<ReturnType<typeof runInjectedReviewWorkflow>>,
  maximumBytes: number,
): void {
  let evidenceBudget = Math.floor(maximumBytes / 2);
  for (let attempt = 0; attempt < 6; attempt++) {
    fitTaskEvidenceCollection(report.execution.taskEvidence, evidenceBudget);
    const encodedBytes = Buffer.byteLength(JSON.stringify(report));
    if (encodedBytes <= maximumBytes) return;
    evidenceBudget = Math.max(
      0,
      evidenceBudget - (encodedBytes - maximumBytes) - 4096,
    );
  }
}

export async function main(
  environment: NodeJS.ProcessEnv = process.env,
): Promise<number> {
  const interrupt = new AbortController();
  const onInterrupt = (): void =>
    interrupt.abort(new DOMException("worker interrupted", "AbortError"));
  process.once("SIGINT", onInterrupt);
  process.once("SIGTERM", onInterrupt);
  try {
    const input = await readSingleStdinJSON();
    let request: WorkerRequest;
    try {
      request = decodeWorkerRequest(input);
    } catch (error) {
      writeFatalProgress(error);
      return 2;
    }
    const result = await executeWorkerRequest(
      request,
      environment,
      (progress) => process.stderr.write(`${JSON.stringify(progress)}\n`),
      {},
      interrupt.signal,
    );
    // stdout is reserved for exactly one compact protocol result.
    process.stdout.write(serializeWorkerResult(result));
    return 0;
  } catch (error) {
    writeFatalProgress(error);
    return 2;
  } finally {
    process.off("SIGINT", onInterrupt);
    process.off("SIGTERM", onInterrupt);
  }
}

function providerProfileForPlan(
  plan: AgentReviewPlan,
  environment: NodeJS.ProcessEnv,
): PiProviderProfile {
  const apiKey = environment.ANTHROPIC_API_KEY?.trim();
  if (!apiKey) {
    throw new WorkerAdmissionError(
      "provider_auth_unavailable",
      "ANTHROPIC_API_KEY is required by the selected provider profile",
    );
  }
  if (plan.provider.id === "anthropic") {
    return { kind: "anthropic-official", apiKey };
  }
  if (plan.provider.id === "deepseek-anthropic") {
    const baseUrl = environment.ANTHROPIC_BASE_URL?.trim();
    if (!baseUrl) {
      throw new WorkerAdmissionError(
        "provider_auth_unavailable",
        "ANTHROPIC_BASE_URL is required by deepseek-anthropic-env",
      );
    }
    return {
      kind: "deepseek-anthropic-env",
      baseUrl,
      apiKey,
    };
  }
  throw new WorkerAdmissionError(
    "unsupported_provider_profile",
    `unsupported provider profile ${plan.provider.id}`,
  );
}

function reviewOptionsForPlan(
  plan: AgentReviewPlan,
  target: ReturnType<typeof materializeFrozenTarget>,
): ReviewOptions {
  const providerProfile =
    plan.provider.id === "anthropic"
      ? "anthropic-official"
      : plan.provider.id === "deepseek-anthropic"
        ? "deepseek-anthropic-env"
        : undefined;
  if (!providerProfile) {
    throw new WorkerAdmissionError(
      "unsupported_provider_profile",
      `unsupported provider ${plan.provider.id}`,
    );
  }
  return {
    repository: target.repository,
    target: { kind: "files", paths: target.files.map((file) => file.path) },
    model: plan.model.id,
    providerProfile,
    normalizationRevision: plan.normalization.revision as "v0" | "v1" | "v2",
    concurrency: plan.budget.max_concurrency,
    skills: plan.review_dimensions.map((dimension) => dimension.id),
    knowledgeFiles: [],
    verify: true,
    maxToolCalls: plan.budget.max_tool_calls,
    maxGroupBytes: plan.budget.max_group_bytes,
    maxTargetBytes: plan.budget.max_target_bytes,
    maxFiles: plan.budget.max_files,
    maxGroups: plan.budget.max_groups,
    maxCandidates: plan.budget.max_candidates,
    maxModelCalls: plan.budget.max_model_calls,
    maxOutputTokens: plan.budget.max_output_tokens,
    timeoutMs: plan.budget.timeout_ms,
    verbose: false,
  };
}

function validateSupportedPlan(plan: AgentReviewPlan): void {
  const expected = [
    ["implementation", plan.implementation, "pi-review-worker", "v0"],
    ["grouping", plan.grouping, "directory-language", "v0", true],
    [
      "context_dimensions[0]",
      plan.context_dimensions[0],
      "code-context",
      "v0",
      true,
    ],
    ["verifier", plan.verifier, "independent-verifier", "v0", true],
    ["agent", plan.agent, "pi-agent", "worker-v0", true],
  ] as const;
  for (const [name, ref, id, revision, buildBearing = false] of expected) {
    if (
      ref?.id !== id ||
      (buildBearing
        ? !supportsBuildRevision(ref, revision)
        : ref.revision !== revision)
    ) {
      throw new WorkerAdmissionError(
        "unsupported_plan_component",
        `plan.${name} is not supported by this worker`,
      );
    }
  }
  const workerDigest = plan.implementation.sha256;
  if (
    plan.normalization.id !== "candidate-normalization" ||
    !["v0", "v1", "v2"].includes(plan.normalization.revision) ||
    plan.normalization.sha256 !== workerDigest ||
    plan.grouping.sha256 !== workerDigest ||
    plan.context_dimensions[0]?.sha256 !== workerDigest ||
    plan.verifier.sha256 !== workerDigest ||
    plan.agent.sha256 !== workerDigest
  ) {
    throw new WorkerAdmissionError(
      "plan_component_digest_mismatch",
      "worker-owned plan components must bind one worker implementation digest",
    );
  }
  if (
    plan.runtime.id !== "node-runtime" ||
    !supportsBuildRevision(plan.runtime, "binary") ||
    plan.profile.id !== "local-shadow-stdio" ||
    plan.profile.revision !== "v1" ||
    plan.provider.revision !== "v1" ||
    plan.model.revision !== "provider"
  ) {
    throw new WorkerAdmissionError(
      "unsupported_runtime_profile",
      "plan runtime/profile/provider/model revisions do not match the local stdio worker",
    );
  }
}

function supportsBuildRevision(
  ref: AgentReviewPlan["runtime"],
  base: string,
): boolean {
  return (
    ref.revision === base ||
    ref.revision === `${base}-${ref.sha256.slice(0, 16)}`
  );
}

async function readSingleStdinJSON(): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let bytes = 0;
  for await (const raw of process.stdin) {
    const chunk = Buffer.isBuffer(raw) ? raw : Buffer.from(raw as string);
    bytes += chunk.length;
    if (bytes > MAX_STDIN_BYTES) {
      throw new Error(`worker stdin exceeds ${MAX_STDIN_BYTES} bytes`);
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

function writeFatalProgress(error: unknown): void {
  process.stderr.write(
    `${JSON.stringify({
      schema_version: "argus.agent_review_worker_progress.v1alpha1",
      work_item_id: "unbound",
      phase: "result",
      message: boundedErrorMessage(error),
      observed_at: new Date().toISOString(),
    })}\n`,
  );
}

function boundedErrorMessage(error: unknown): string {
  const message = errorMessage(error).trim() || "worker execution failed";
  return Buffer.byteLength(message) <= 4096
    ? message
    : `${Buffer.from(message).subarray(0, 4000).toString("utf8")}...`;
}

class WorkerAdmissionError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

const isEntrypoint =
  process.argv[1] !== undefined &&
  import.meta.url === pathToFileURL(process.argv[1]).href;
if (isEntrypoint) process.exitCode = await main();
