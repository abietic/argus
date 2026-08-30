import { createHash } from "node:crypto";

import type { AgentRuntime } from "./agent.js";
import { isFrozenTarget } from "./target.js";
import { DEFAULT_OPERATIONAL_PROMPT_BUNDLE } from "./prompt.js";
import type {
  AgentTaskObservation,
  ChangeGroup,
  KnowledgeDefinition,
  PiExecutionEnvelope,
  PiExecutionSnapshot,
  PiTaskEvidenceCollection,
  ProviderReportedUsage,
  ReviewOptions,
  SkillDefinition,
  TargetSnapshot,
} from "./types.js";

const RUNTIME_VERSION = "0.1.0";
const PI_VERSION = "0.84.1";
export function createExecutionSnapshot(
  options: ReviewOptions,
  target: TargetSnapshot,
  groups: ChangeGroup[],
  skills: SkillDefinition[],
  knowledge: KnowledgeDefinition[],
  runtime: AgentRuntime,
): PiExecutionSnapshot {
  const provider = providerIdentity(runtime);
  const promptBundle =
    runtime.promptBundle ?? DEFAULT_OPERATIONAL_PROMPT_BUNDLE;
  const content = {
    schemaVersion: "argus.pi-review.execution_snapshot.v0" as const,
    target: {
      kind: target.kind,
      digest: target.digest,
      ...(target.baseOid ? { baseOid: target.baseOid } : {}),
      ...(target.headOid ? { headOid: target.headOid } : {}),
    },
    workflowRevision:
      `argus-pi-review-workflow-${options.normalizationRevision}` as const,
    promptBundleRevision: promptBundle.revision,
    promptBundleDigest: promptBundle.digest,
    grouping: {
      implementationRevision: "directory-language-v0" as const,
      groups: groups.map((group) => ({
        id: group.id,
        key: group.key,
        patchDigest: sha256(group.patch),
        files: group.files.map((file) => file.path).sort(),
      })),
    },
    skills: skills.map((skill) => ({
      id: skill.id,
      revision: skill.revision,
      digest: `sha256:${sha256(skill.prompt)}`,
      bytes: Buffer.byteLength(skill.prompt),
    })),
    knowledge: knowledge.map((item) => ({ ...item })),
    runtime: {
      implementation: "@argus/pi-review" as const,
      version: RUNTIME_VERSION,
      node: process.version,
      piAgentCore: PI_VERSION,
      piAi: PI_VERSION,
    },
    provider,
    toolPolicy: {
      mode: "read_only" as const,
      allowedTools: isFrozenTarget(target)
        ? ["list_files", "read_file", "search_code"]
        : ["list_files", "query_codegraph", "read_file", "search_code"],
    },
    budgets: {
      concurrency: options.concurrency,
      maxToolCallsPerTask: options.maxToolCalls,
      maxFiles: options.maxFiles,
      maxGroups: options.maxGroups,
      maxGroupBytes: options.maxGroupBytes,
      maxTargetBytes: options.maxTargetBytes,
      maxCandidates: options.maxCandidates,
      maxProviderTurns: options.maxModelCalls,
      maxOutputTokensPerTurn: options.maxOutputTokens,
      taskTimeoutMs: options.timeoutMs,
    },
    verificationPolicy: options.verify
      ? ("independent_required" as const)
      : ("disabled" as const),
    replayability: {
      status: "non_replayable" as const,
      reasons: [
        "target_content_not_artifactized",
        ...(promptBundle.governed ? [] : ["prompt_content_not_artifactized"]),
        "tool_transcript_not_artifactized",
        ...(runtime.provider === "fake"
          ? []
          : ["direct_provider_execution_not_platform_attested"]),
      ],
    },
  };
  return {
    ...content,
    snapshotDigest: `sha256:${sha256(canonicalJson(content))}`,
    createdAt: new Date().toISOString(),
  };
}

export function createExecutionEnvelope(
  snapshot: PiExecutionSnapshot,
  observations: AgentTaskObservation[],
  taskEvidence: PiTaskEvidenceCollection = emptyTaskEvidenceCollection(
    observations.length > 0,
  ),
): PiExecutionEnvelope {
  const tasks = [...observations].sort((left, right) =>
    left.taskId.localeCompare(right.taskId),
  );
  return {
    authority: "diagnostic_only",
    provenanceClass: "worker_self_report",
    snapshot,
    tasks,
    taskEvidence,
    usage: summarizeUsage(tasks),
  };
}

export function emptyTaskEvidenceCollection(
  unavailable: boolean,
): PiTaskEvidenceCollection {
  return {
    schemaVersion: "argus.pi-review.task_evidence.v0",
    authority: "diagnostic_only",
    provenanceClass: "worker_self_report",
    contentPolicy: "exact_local_sensitive",
    completeness: unavailable ? "partial" : "complete",
    reasonCodes: unavailable ? ["task_evidence_unavailable"] : [],
    tasks: [],
  };
}

function providerIdentity(
  runtime: AgentRuntime,
): PiExecutionSnapshot["provider"] {
  if (runtime.provider === "fake") {
    return {
      provider: "fake",
      profile: runtime.providerProfile,
      protocol: "fake",
      model: runtime.model,
    };
  }
  const endpoint =
    runtime.provider === "deepseek-anthropic"
      ? "https://api.deepseek.com/anthropic"
      : "https://api.anthropic.com";
  return {
    provider: runtime.provider,
    profile: runtime.providerProfile,
    protocol: "anthropic-messages",
    model: runtime.model,
    endpointDigest: `sha256:${sha256(endpoint)}`,
    credentialRef: "env:ANTHROPIC_API_KEY",
  };
}

function summarizeUsage(
  observations: AgentTaskObservation[],
): ProviderReportedUsage {
  if (observations.length === 0) {
    return {
      completeness: "unavailable",
      unavailableReasonCode: "one_or_more_task_usage_unavailable",
    };
  }
  const observationsWithUsage = observations.filter(
    (item) => item.usage.completeness !== "unavailable",
  );
  if (observationsWithUsage.length === 0) {
    return {
      completeness: "unavailable",
      unavailableReasonCode: "one_or_more_task_usage_unavailable",
    };
  }
  const reasoningTokens = sumOptional(observations, "reasoningTokens");
  const complete = observations.every(
    (item) => item.usage.completeness === "provider_reported",
  );
  return {
    completeness: complete ? "provider_reported" : "partial",
    ...(!complete
      ? {
          unavailableReasonCode: "one_or_more_task_usage_unavailable" as const,
        }
      : {}),
    inputTokens: sum(observations, "inputTokens"),
    outputTokens: sum(observations, "outputTokens"),
    cacheReadTokens: sum(observations, "cacheReadTokens"),
    cacheWriteTokens: sum(observations, "cacheWriteTokens"),
    ...(reasoningTokens !== undefined ? { reasoningTokens } : {}),
    totalTokens: sum(observations, "totalTokens"),
  };
}

function sum(
  observations: AgentTaskObservation[],
  field:
    | "inputTokens"
    | "outputTokens"
    | "cacheReadTokens"
    | "cacheWriteTokens"
    | "totalTokens",
): number {
  return observations.reduce(
    (total, observation) => total + (observation.usage[field] ?? 0),
    0,
  );
}

function sumOptional(
  observations: AgentTaskObservation[],
  field: "reasoningTokens",
): number | undefined {
  return observations.some(
    (observation) => observation.usage[field] !== undefined,
  )
    ? observations.reduce(
        (total, observation) => total + (observation.usage[field] ?? 0),
        0,
      )
    : undefined;
}

export function canonicalJson(value: unknown): string {
  return JSON.stringify(canonicalize(value));
}

function canonicalize(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .filter(([, item]) => item !== undefined)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, item]) => [key, canonicalize(item)]),
    );
  }
  return value;
}

function sha256(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}
