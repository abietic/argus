import assert from "node:assert/strict";
import test from "node:test";

import type { AgentRuntime } from "../src/agent.js";
import {
  createExecutionEnvelope,
  createExecutionSnapshot,
} from "../src/execution.js";
import type {
  AgentTaskObservation,
  ChangeGroup,
  ReviewOptions,
  SkillDefinition,
  TargetSnapshot,
} from "../src/types.js";

const target: TargetSnapshot = {
  kind: "files",
  repository: "/private/repository-must-not-enter-snapshot",
  digest: "target-digest",
  capturedAt: "2026-01-01T00:00:00Z",
  files: [
    {
      path: "src/value.ts",
      status: "full",
      patch: "@@ -0,0 +1 @@\n+export const value = 1;",
      targetContent: "export const value = 1;\n",
      digest: "file-digest",
      changedLines: 1,
    },
  ],
  skipped: [],
};

const groups: ChangeGroup[] = [
  {
    id: "group-001",
    key: "root:.ts",
    files: target.files,
    patch: target.files[0]!.patch,
    changedLines: 1,
  },
];

const skills: SkillDefinition[] = [
  {
    id: "correctness",
    revision: "builtin-v1",
    title: "Correctness",
    prompt: "Find concrete defects.",
    source: "builtin:correctness",
  },
];

const options: ReviewOptions = {
  repository: target.repository,
  target: { kind: "files", paths: ["src/value.ts"] },
  model: "deepseek-v4-pro[1m]",
  providerProfile: "deepseek-anthropic-env",
  normalizationRevision: "v2",
  concurrency: 2,
  skills: ["correctness"],
  knowledgeFiles: [],
  verify: true,
  maxToolCalls: 8,
  maxGroupBytes: 65_536,
  maxTargetBytes: 4 * 1024 * 1024,
  maxFiles: 32,
  maxGroups: 8,
  maxCandidates: 32,
  maxModelCalls: 64,
  maxOutputTokens: 4096,
  timeoutMs: 30_000,
  verbose: false,
};

function runtime(model = options.model): AgentRuntime {
  return {
    provider: "deepseek-anthropic",
    providerProfile: "deepseek-anthropic-env@1",
    model,
    getExecutionObservations: () => [],
  } as unknown as AgentRuntime;
}

test("execution snapshot is content-addressed and contains no secret or repository path", () => {
  const first = createExecutionSnapshot(
    options,
    target,
    groups,
    skills,
    [{ id: "1:rules.md", digest: "sha256:knowledge", bytes: 10 }],
    runtime(),
  );
  const second = createExecutionSnapshot(
    options,
    target,
    groups,
    skills,
    [{ id: "1:rules.md", digest: "sha256:knowledge", bytes: 10 }],
    runtime(),
  );
  assert.equal(first.snapshotDigest, second.snapshotDigest);
  assert.match(first.snapshotDigest, /^sha256:[0-9a-f]{64}$/u);
  const encoded = JSON.stringify(first);
  assert.doesNotMatch(encoded, /repository-must-not-enter-snapshot/u);
  assert.doesNotMatch(encoded, /fixture-secret/u);
  assert.equal(first.provider.credentialRef, "env:ANTHROPIC_API_KEY");
  assert.equal(first.replayability.status, "non_replayable");
});

test("execution snapshot digest changes with model, skill, knowledge or budget", () => {
  const baseline = createExecutionSnapshot(
    options,
    target,
    groups,
    skills,
    [],
    runtime(),
  ).snapshotDigest;
  const variants = [
    createExecutionSnapshot(
      options,
      target,
      groups,
      skills,
      [],
      runtime("other-model"),
    ),
    createExecutionSnapshot(
      options,
      target,
      groups,
      [{ ...skills[0]!, prompt: "Different prompt." }],
      [],
      runtime(),
    ),
    createExecutionSnapshot(
      options,
      target,
      groups,
      skills,
      [{ id: "1:rules.md", digest: "sha256:different", bytes: 4 }],
      runtime(),
    ),
    createExecutionSnapshot(
      { ...options, maxModelCalls: options.maxModelCalls + 1 },
      target,
      groups,
      skills,
      [],
      runtime(),
    ),
    createExecutionSnapshot(
      { ...options, normalizationRevision: "v1" },
      target,
      groups,
      skills,
      [],
      runtime(),
    ),
  ];
  for (const variant of variants)
    assert.notEqual(variant.snapshotDigest, baseline);
});

test("execution envelope aggregates provider-reported task usage", () => {
  const snapshot = createExecutionSnapshot(
    options,
    target,
    groups,
    skills,
    [],
    runtime(),
  );
  const baseObservation: AgentTaskObservation = {
    taskId: "task-1",
    taskKind: "context",
    groupId: "group-001",
    promptDigest: "sha256:prompt",
    providerTurnsStarted: 1,
    providerTurnsCompleted: 1,
    toolCalls: 1,
    toolNames: ["submit_context"],
    toolUsage: [
      { toolId: "submit_context", invocationCount: 1, failureCount: 0 },
    ],
    usage: {
      completeness: "provider_reported",
      inputTokens: 10,
      outputTokens: 5,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
      totalTokens: 15,
    },
    startedAt: "2026-01-01T00:00:00Z",
    finishedAt: "2026-01-01T00:00:00.010Z",
    durationMs: 10,
    terminalStatus: "succeeded",
    outputDigest: "sha256:output",
  };
  const envelope = createExecutionEnvelope(snapshot, [
    baseObservation,
    {
      ...baseObservation,
      taskId: "task-2",
      taskKind: "review",
      usage: { ...baseObservation.usage, inputTokens: 20, totalTokens: 25 },
    },
  ]);
  assert.equal(envelope.authority, "diagnostic_only");
  assert.equal(envelope.provenanceClass, "worker_self_report");
  assert.equal(envelope.usage.inputTokens, 30);
  assert.equal(envelope.usage.totalTokens, 40);
});

test("execution envelope preserves unknown usage instead of reporting zero", () => {
  const snapshot = createExecutionSnapshot(
    options,
    target,
    groups,
    skills,
    [],
    runtime(),
  );
  const envelope = createExecutionEnvelope(snapshot, [
    {
      taskId: "task-incomplete",
      taskKind: "context",
      groupId: "group-001",
      promptDigest: "sha256:prompt",
      providerTurnsStarted: 1,
      providerTurnsCompleted: 0,
      toolCalls: 0,
      toolNames: [],
      toolUsage: [],
      usage: {
        completeness: "unavailable",
        unavailableReasonCode: "provider_turn_incomplete",
      },
      startedAt: "2026-01-01T00:00:00Z",
      finishedAt: "2026-01-01T00:00:00.010Z",
      durationMs: 10,
      terminalStatus: "failed",
      errorCode: "provider_error",
    },
  ]);

  assert.deepEqual(envelope.usage, {
    completeness: "unavailable",
    unavailableReasonCode: "one_or_more_task_usage_unavailable",
  });
});
