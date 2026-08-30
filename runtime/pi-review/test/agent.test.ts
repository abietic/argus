import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import {
  createModels,
  fauxAssistantMessage,
  fauxProvider,
  fauxToolCall,
} from "@earendil-works/pi-ai";

import { normalizeDeepSeekBaseUrl, PiAgentRuntime } from "../src/agent.js";
import { DEFAULT_PROMPT_BUNDLE } from "../src/prompt.js";
import { materializeFrozenTarget } from "../src/target.js";
import type {
  ChangeGroup,
  SkillDefinition,
  TargetSnapshot,
} from "../src/types.js";

const target: TargetSnapshot = {
  kind: "files",
  repository: "/tmp/argus-faux",
  digest: "target",
  capturedAt: "2026-01-01T00:00:00Z",
  files: [
    {
      path: "main.ts",
      status: "full",
      patch: "@@ -0,0 +1,1 @@\n+export const value = 1;",
      targetContent: "export const value = 1;\n",
      digest: "file",
      changedLines: 1,
    },
  ],
  skipped: [],
};

const group: ChangeGroup = {
  id: "group-001",
  key: "root:.ts",
  files: target.files,
  patch: target.files[0]?.patch ?? "",
  changedLines: 1,
};

function frozenDiffFixture(): {
  target: TargetSnapshot;
  group: ChangeGroup;
} {
  const content = [
    "package fixture",
    "",
    "func DisplayName(user *User) string {",
    "\treturn user.Name",
    "}",
    "",
  ].join("\n");
  const patch = [
    "diff --git a/user.go b/user.go",
    "index 1111111..2222222 100644",
    "--- a/user.go",
    "+++ b/user.go",
    "@@ -1,7 +1,5 @@",
    " package fixture",
    " ",
    " func DisplayName(user *User) string {",
    "-\tif user == nil {",
    '-\t\treturn "anonymous"',
    "-\t}",
    " \treturn user.Name",
    " }",
    "",
  ].join("\n");
  const reviewInput = Buffer.from(
    JSON.stringify({
      schema_version: "argus.review_input.v1alpha1",
      target_id: "target-agent-test",
      target_mode: "diff",
      canonical_patch: patch,
      regions: [],
      files: [
        {
          path: "user.go",
          sha256: createHash("sha256").update(content).digest("hex"),
          size_bytes: Buffer.byteLength(content),
          content,
        },
      ],
      contexts: [],
    }),
  );
  const frozenTarget = materializeFrozenTarget(
    reviewInput,
    createHash("sha256").update(reviewInput).digest("hex"),
  );
  return {
    target: frozenTarget,
    group: {
      id: "group-frozen",
      key: "root:.go",
      files: frozenTarget.files,
      patch,
      changedLines: 3,
    },
  };
}

test("Pi runtime accepts a schema-validated terminal tool result", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("submit_context", {
        summary: "Single constant declaration.",
        relevantFiles: [{ path: "main.ts", reason: "changed file" }],
        facts: [],
        gaps: [],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: {
      provider: "fake",
      models,
      model: faux.getModel(),
    },
  });
  const context = await runtime.collectContext(
    target,
    group,
    "",
    new AbortController().signal,
  );
  assert.equal(context.summary, "Single constant declaration.");
  assert.equal(faux.state.callCount, 1);
  assert.equal(faux.getPendingResponseCount(), 0);
  const [observation] = runtime.getExecutionObservations();
  assert.equal(observation?.taskKind, "context");
  assert.equal(observation?.terminalStatus, "succeeded");
  assert.equal(observation?.providerTurnsStarted, 1);
  assert.equal(observation?.providerTurnsCompleted, 1);
  assert.equal(observation?.toolCalls, 1);
  assert.deepEqual(observation?.toolNames, ["submit_context"]);
  assert.deepEqual(observation?.toolUsage, [
    { toolId: "submit_context", invocationCount: 1, failureCount: 0 },
  ]);
  assert.match(observation?.promptDigest ?? "", /^sha256:[0-9a-f]{64}$/u);
  assert.match(observation?.outputDigest ?? "", /^sha256:[0-9a-f]{64}$/u);
});

test("terminal submit validates frozen evidence and bounded text before accepting a retry", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  const candidate = {
    category: "correctness",
    severity: "high",
    rawConfidencePPM: 900_000,
    title: "Example defect",
    description: "The changed declaration has a concrete defect.",
    impact: "Callers can observe an incorrect value.",
    anchor: {
      path: "main.ts",
      side: "file",
      startLine: 1,
      endLine: 1,
    },
    evidence: [
      {
        statement: "The changed declaration is the relevant source evidence.",
        anchor: {
          path: "main.ts",
          side: "file",
          startLine: 1,
          endLine: 1,
        },
        excerpt: "export const value = 2;",
      },
    ],
  };
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("submit_candidates", {
        summary: "One candidate found.",
        candidates: [candidate],
      }),
      { stopReason: "toolUse" },
    ),
    fauxAssistantMessage(
      fauxToolCall("submit_candidates", {
        summary: "One candidate found.",
        candidates: [
          {
            ...candidate,
            title: "缺".repeat(100),
            evidence: [
              {
                ...candidate.evidence[0],
                excerpt: "export const value = 1;",
              },
            ],
          },
        ],
      }),
      { stopReason: "toolUse" },
    ),
    fauxAssistantMessage(
      fauxToolCall("submit_candidates", {
        summary: "One candidate found.",
        candidates: [
          {
            ...candidate,
            evidence: [
              {
                ...candidate.evidence[0],
                excerpt: "export const value = 1;",
              },
            ],
          },
        ],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: { provider: "fake", models, model: faux.getModel() },
  });
  const skill: SkillDefinition = {
    id: "correctness",
    revision: "builtin-v2",
    title: "Correctness",
    prompt: "Find concrete correctness defects.",
    source: "builtin:correctness",
  };

  const submission = await runtime.review(
    target,
    group,
    { summary: "Single declaration.", relevantFiles: [], facts: [], gaps: [] },
    skill,
    "",
    new AbortController().signal,
  );

  assert.equal(submission.candidates.length, 1);
  assert.equal(
    submission.candidates[0]?.evidence[0]?.excerpt,
    "export const value = 1;",
  );
  assert.equal(faux.state.callCount, 3);
  const [observation] = runtime.getExecutionObservations();
  assert.deepEqual(observation?.toolUsage, [
    { toolId: "submit_candidates", invocationCount: 3, failureCount: 2 },
  ]);
});

test("frozen review prompt forbids old-side anchors that the host cannot import", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("submit_candidates", {
        summary: "One candidate found.",
        candidates: [
          {
            category: "correctness",
            severity: "high",
            title: "Nil pointer dereference",
            description: "The target dereferences user without a guard.",
            impact: "A nil input panics.",
            anchor: {
              path: "user.go",
              side: "new",
              startLine: 4,
              endLine: 4,
            },
            evidence: [
              {
                statement: "The target dereferences user directly.",
                anchor: {
                  path: "user.go",
                  side: "new",
                  startLine: 4,
                  endLine: 4,
                },
                excerpt: "return user.Name",
              },
            ],
          },
        ],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: { provider: "fake", models, model: faux.getModel() },
  });
  const skill: SkillDefinition = {
    id: "correctness",
    revision: "builtin-v2",
    title: "Correctness",
    prompt: "Find concrete correctness defects.",
    source: "builtin:correctness",
  };
  const fixture = frozenDiffFixture();

  await runtime.review(
    fixture.target,
    fixture.group,
    { summary: "Frozen target.", relevantFiles: [], facts: [], gaps: [] },
    skill,
    "",
    new AbortController().signal,
  );

  const [task] = runtime.getExecutionEvidence(1 << 20).tasks;
  assert.match(
    task?.userPrompt.content ?? "",
    /Only target-side file content is available/u,
  );
  assert.match(task?.userPrompt.content ?? "", /do not use side old/u);
  assert.doesNotMatch(
    task?.userPrompt.content ?? "",
    /removed line on side old/u,
  );
  assert.match(
    task?.userPrompt.content ?? "",
    /hunk only deletes code.*nearest surviving target context line/u,
  );
});

test("task evidence preserves exact prompts, structured output and tool payloads", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("submit_context", {
        summary: "Exact context output.",
        relevantFiles: [{ path: "main.ts", reason: "changed file" }],
        facts: [],
        gaps: [],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: { provider: "fake", models, model: faux.getModel() },
  });

  const output = await runtime.collectContext(
    target,
    group,
    "repository knowledge",
    new AbortController().signal,
  );
  const [observation] = runtime.getExecutionObservations();
  const evidence = runtime.getExecutionEvidence(1 << 20, [
    observation?.taskId ?? "missing",
  ]);
  assert.equal(evidence.completeness, "complete");
  assert.deepEqual(evidence.reasonCodes, []);
  const [task] = evidence.tasks;
  assert.match(task?.systemPrompt.content ?? "", /context collector/u);
  assert.match(task?.userPrompt.content ?? "", /repository knowledge/u);
  assert.equal(task?.output.content, JSON.stringify(output));
  assert.equal(task?.tools.length, 1);
  assert.equal(task?.tools[0]?.toolName, "submit_context");
  assert.match(
    task?.tools[0]?.arguments.content ?? "",
    /"summary":"Exact context output\."/u,
  );
  for (const value of [
    task?.systemPrompt,
    task?.userPrompt,
    task?.output,
    task?.tools[0]?.arguments,
    task?.tools[0]?.result,
  ]) {
    assert.ok(value?.content !== undefined);
    assert.equal(value.sizeBytes, Buffer.byteLength(value.content));
    assert.equal(
      value.sha256,
      createHash("sha256").update(value.content).digest("hex"),
    );
  }
});

test("governed prompt bundle changes the actual Pi system prompt without replacing safety constraints", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("submit_context", {
        summary: "Governed prompt applied.",
        relevantFiles: [],
        facts: [],
        gaps: [],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const marker = "ARGUS_GOVERNED_PROMPT_MARKER";
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    promptBundle: {
      revision: "experiment-v1",
      digest: "a".repeat(64),
      governed: true,
      content: {
        ...DEFAULT_PROMPT_BUNDLE,
        revision: "experiment-v1",
        context_system_prompt: `${marker}: collect only directly relevant type and call-chain facts.`,
      },
    },
    backend: { provider: "fake", models, model: faux.getModel() },
  });

  await runtime.collectContext(target, group, "", new AbortController().signal);
  const [task] = runtime.getExecutionEvidence(1 << 20).tasks;
  assert.match(task?.systemPrompt.content ?? "", new RegExp(marker, "u"));
  assert.match(
    task?.systemPrompt.content ?? "",
    /Never request, infer, reveal or transform credentials/u,
  );
});

test("task evidence budget keeps digests while declaring omitted exact content", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("submit_context", {
        summary: "Budgeted context.",
        relevantFiles: [],
        facts: [],
        gaps: [],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: { provider: "fake", models, model: faux.getModel() },
  });
  await runtime.collectContext(target, group, "", new AbortController().signal);
  const evidence = runtime.getExecutionEvidence(512);
  assert.equal(evidence.completeness, "partial");
  assert.deepEqual(evidence.reasonCodes, ["evidence_budget_exceeded"]);
  const contents = evidence.tasks.flatMap((task) => [
    task.systemPrompt,
    task.userPrompt,
    ...(task.output ? [task.output] : []),
    ...task.tools.flatMap((tool) => [
      tool.arguments,
      ...(tool.result ? [tool.result] : []),
    ]),
  ]);
  assert.ok(contents.some((content) => content.content === undefined));
  for (const content of contents) {
    assert.match(content.sha256, /^[0-9a-f]{64}$/u);
    assert.ok(content.sizeBytes >= 0);
    if (content.content === undefined) {
      assert.equal(content.omissionReason, "evidence_budget_exceeded");
    }
  }
});

test("receipt and task evidence count admitted tool executions rather than blocked requests", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      [
        fauxToolCall("read_file", {
          path: "main.ts",
          view: "target",
          startLine: 1,
          endLine: 1,
        }),
        fauxToolCall("search_code", {
          query: "value",
          paths: ["main.ts"],
          maxResults: 10,
        }),
      ],
      { stopReason: "toolUse" },
    ),
    fauxAssistantMessage(
      fauxToolCall("submit_context", {
        summary: "Single constant declaration.",
        relevantFiles: [{ path: "main.ts", reason: "changed file" }],
        facts: [],
        gaps: ["Search was not admitted by the read-tool budget."],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 1,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: {
      provider: "fake",
      models,
      model: faux.getModel(),
    },
  });

  await runtime.collectContext(target, group, "", new AbortController().signal);
  const [observation] = runtime.getExecutionObservations();
  assert.equal(observation?.toolCalls, 2);
  assert.deepEqual(observation?.toolNames, ["read_file", "submit_context"]);
  assert.deepEqual(observation?.toolUsage, [
    { toolId: "read_file", invocationCount: 1, failureCount: 0 },
    { toolId: "submit_context", invocationCount: 1, failureCount: 0 },
  ]);
  const evidence = runtime.getExecutionEvidence(1 << 20);
  assert.equal(evidence.tasks.length, 1);
  assert.deepEqual(
    evidence.tasks[0]?.tools.map((tool) => ({
      sequence: tool.sequence,
      toolName: tool.toolName,
      isError: tool.isError,
    })),
    [
      { sequence: 1, toolName: "read_file", isError: false },
      { sequence: 2, toolName: "submit_context", isError: false },
    ],
  );
});

test("receipt records failures for admitted tool executions", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("read_file", {
        path: "missing.ts",
        view: "target",
        startLine: 1,
        endLine: 1,
      }),
      { stopReason: "toolUse" },
    ),
    fauxAssistantMessage(
      fauxToolCall("submit_context", {
        summary: "The requested context file was unavailable.",
        relevantFiles: [],
        facts: [],
        gaps: ["missing.ts is outside the frozen target."],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 1,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: {
      provider: "fake",
      models,
      model: faux.getModel(),
    },
  });

  await runtime.collectContext(target, group, "", new AbortController().signal);
  const [observation] = runtime.getExecutionObservations();
  assert.deepEqual(observation?.toolUsage, [
    { toolId: "read_file", invocationCount: 1, failureCount: 1 },
    { toolId: "submit_context", invocationCount: 1, failureCount: 0 },
  ]);
});

test("production Pi runtime rejects an empty API key before ambient auth fallback", () => {
  assert.throws(
    () =>
      new PiAgentRuntime({
        model: "claude-sonnet-4-6",
        profile: { kind: "anthropic-official", apiKey: "   " },
        maxToolCalls: 4,
        maxModelCalls: 4,
        maxOutputTokens: 1024,
        timeoutMs: 5_000,
      }),
    /API key must not be empty/u,
  );
});

test("DeepSeek profile accepts only the exact guarded Anthropic endpoint", () => {
  assert.equal(
    normalizeDeepSeekBaseUrl("https://api.deepseek.com/anthropic/"),
    "https://api.deepseek.com/anthropic",
  );
  for (const value of [
    "http://api.deepseek.com/anthropic",
    "https://api.deepseek.com/v1",
    "https://api.deepseek.com.evil.example/anthropic",
    "https://user:pass@api.deepseek.com/anthropic",
    "https://api.deepseek.com/anthropic?redirect=1",
  ]) {
    assert.throws(() => normalizeDeepSeekBaseUrl(value), /exact DeepSeek/u);
  }
  const runtime = new PiAgentRuntime({
    model: "deepseek-v4-pro[1m]",
    profile: {
      kind: "deepseek-anthropic-env",
      baseUrl: "https://api.deepseek.com/anthropic",
      apiKey: "fixture-key",
    },
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
  });
  assert.equal(runtime.provider, "deepseek-anthropic");
  assert.equal(runtime.providerProfile, "deepseek-anthropic-env@1");
});

test("Pi runtime enforces the global provider turn budget across tool round trips", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("read_file", {
        path: "main.ts",
        view: "target",
        startLine: 1,
        endLine: 1,
      }),
      { stopReason: "toolUse" },
    ),
    fauxAssistantMessage(
      fauxToolCall("submit_context", {
        summary: "Single constant declaration.",
        relevantFiles: [{ path: "main.ts", reason: "changed file" }],
        facts: [],
        gaps: [],
      }),
      { stopReason: "toolUse" },
    ),
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 1,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: {
      provider: "fake",
      models,
      model: faux.getModel(),
    },
  });

  await assert.rejects(
    runtime.collectContext(target, group, "", new AbortController().signal),
    /provider\/model turn budget exhausted/u,
  );
  assert.equal(faux.state.callCount, 1);
  const [observation] = runtime.getExecutionObservations();
  assert.equal(observation?.terminalStatus, "failed");
  assert.equal(observation?.errorCode, "provider_error");
  assert.equal(observation?.providerTurnsStarted, 1);
  assert.equal(observation?.providerTurnsCompleted, 1);
  assert.equal(observation?.usage.completeness, "provider_reported");
});

test("receipt keeps usage unknown when a provider turn never completes", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    async () => {
      throw new Error("synthetic provider failure");
    },
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: {
      provider: "fake",
      models,
      model: faux.getModel(),
    },
  });

  await assert.rejects(
    runtime.collectContext(target, group, "", new AbortController().signal),
    /synthetic provider failure/u,
  );
  const [observation] = runtime.getExecutionObservations();
  assert.equal(observation?.providerTurnsStarted, 1);
  assert.equal(observation?.providerTurnsCompleted, 0);
  assert.deepEqual(observation?.usage, {
    completeness: "unavailable",
    unavailableReasonCode: "provider_turn_incomplete",
  });
});

test("receipt preserves observed usage as partial after a later turn fails", async () => {
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  faux.setResponses([
    fauxAssistantMessage(
      fauxToolCall("read_file", {
        path: "main.ts",
        view: "target",
        startLine: 1,
        endLine: 1,
      }),
      { stopReason: "toolUse" },
    ),
    async () => {
      throw new Error("provider failed after the first completed turn");
    },
  ]);
  const runtime = new PiAgentRuntime({
    model: "unused",
    maxToolCalls: 4,
    maxModelCalls: 4,
    maxOutputTokens: 1024,
    timeoutMs: 5_000,
    backend: {
      provider: "fake",
      models,
      model: faux.getModel(),
    },
  });

  await assert.rejects(
    runtime.collectContext(target, group, "", new AbortController().signal),
    /provider failed after the first completed turn/u,
  );
  const [observation] = runtime.getExecutionObservations();
  assert.equal(observation?.providerTurnsStarted, 2);
  assert.equal(observation?.providerTurnsCompleted, 1);
  assert.equal(observation?.usage.completeness, "partial");
  assert.equal(
    observation?.usage.unavailableReasonCode,
    "provider_turn_incomplete",
  );
  assert.ok((observation?.usage.totalTokens ?? 0) > 0);
});
