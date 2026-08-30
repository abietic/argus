import { createHash } from "node:crypto";

import { canonicalJson } from "./execution.js";
import type {
  AgentTaskKind,
  AgentTaskTerminalStatus,
  PiTaskEvidenceCollection,
  PiTaskEvidenceContent,
  PiTaskExecutionEvidence,
  PiTaskToolEvidence,
} from "./types.js";

interface ToolDraft {
  sequence: number;
  toolName: string;
  arguments: string;
  result?: string;
  isError?: boolean;
}

interface TaskDraft {
  taskId: string;
  taskKind: AgentTaskKind;
  groupId: string;
  skillId?: string;
  candidateId?: string;
  systemPrompt: string;
  userPrompt: string;
  output?: string;
  terminalStatus: AgentTaskTerminalStatus;
  tools: ToolDraft[];
}

export class TaskEvidenceRecorder {
  private readonly tools: ToolDraft[] = [];
  private readonly toolByCallId = new Map<string, ToolDraft>();
  private sequence = 0;

  constructor(
    private readonly identity: Omit<
      TaskDraft,
      "output" | "terminalStatus" | "tools"
    >,
  ) {}

  startTool(callId: string, toolName: string, args: unknown): void {
    if (this.toolByCallId.has(callId)) {
      throw new Error(`duplicate tool execution start for ${toolName}`);
    }
    const draft: ToolDraft = {
      sequence: ++this.sequence,
      toolName,
      arguments: canonicalJson(args),
    };
    this.tools.push(draft);
    this.toolByCallId.set(callId, draft);
  }

  finishTool(
    callId: string,
    toolName: string,
    result: unknown,
    isError: boolean,
  ): void {
    const draft = this.toolByCallId.get(callId);
    if (!draft) {
      throw new Error(`tool execution end without start for ${toolName}`);
    }
    draft.result = canonicalJson(result);
    draft.isError = isError;
  }

  retainTools(callIds: ReadonlySet<string>): void {
    for (const [callId, draft] of this.toolByCallId) {
      if (callIds.has(callId)) continue;
      this.toolByCallId.delete(callId);
      const index = this.tools.indexOf(draft);
      if (index >= 0) this.tools.splice(index, 1);
    }
  }

  finish(terminalStatus: AgentTaskTerminalStatus, output?: unknown): TaskDraft {
    return {
      ...this.identity,
      ...(output !== undefined ? { output: JSON.stringify(output) } : {}),
      terminalStatus,
      tools: this.tools.map((tool, index) => ({
        ...tool,
        sequence: index + 1,
      })),
    };
  }
}

export class TaskEvidenceStore {
  private readonly tasks: TaskDraft[] = [];

  add(task: TaskDraft): void {
    this.tasks.push({
      ...task,
      tools: task.tools.map((tool) => ({ ...tool })),
    });
  }

  collection(
    maximumBytes: number,
    expectedTaskIds: string[] = [],
  ): PiTaskEvidenceCollection {
    const tasks = [...this.tasks]
      .sort((left, right) => left.taskId.localeCompare(right.taskId))
      .map(materializeTask);
    const missing = expectedTaskIds.some(
      (taskId) => !tasks.some((task) => task.taskId === taskId),
    );
    const collection: PiTaskEvidenceCollection = {
      schemaVersion: "argus.pi-review.task_evidence.v0",
      authority: "diagnostic_only",
      provenanceClass: "worker_self_report",
      contentPolicy: "exact_local_sensitive",
      completeness: missing ? "partial" : "complete",
      reasonCodes: missing ? ["task_evidence_unavailable"] : [],
      tasks,
    };
    fitTaskEvidenceCollection(
      collection,
      Math.max(0, Math.floor(maximumBytes)),
    );
    return collection;
  }
}

function materializeTask(task: TaskDraft): PiTaskExecutionEvidence {
  return {
    taskId: task.taskId,
    taskKind: task.taskKind,
    groupId: task.groupId,
    ...(task.skillId ? { skillId: task.skillId } : {}),
    ...(task.candidateId ? { candidateId: task.candidateId } : {}),
    systemPrompt: content(task.systemPrompt),
    userPrompt: content(task.userPrompt),
    ...(task.output !== undefined ? { output: content(task.output) } : {}),
    terminalStatus: task.terminalStatus,
    tools: task.tools.map((tool) => ({
      sequence: tool.sequence,
      toolName: tool.toolName,
      arguments: content(tool.arguments),
      ...(tool.result !== undefined
        ? {
            result: content(tool.result),
            isError: tool.isError ?? false,
          }
        : {}),
    })),
  };
}

function content(value: string): PiTaskEvidenceContent {
  return {
    sha256: createHash("sha256").update(value).digest("hex"),
    sizeBytes: Buffer.byteLength(value),
    content: value,
  };
}

export function fitTaskEvidenceCollection(
  collection: PiTaskEvidenceCollection,
  maximumBytes: number,
): void {
  if (Buffer.byteLength(JSON.stringify(collection)) <= maximumBytes) return;
  const slots: Array<{ priority: number; value: PiTaskEvidenceContent }> = [];
  for (const task of collection.tasks) {
    for (const tool of task.tools) {
      if (tool.result) slots.push({ priority: 0, value: tool.result });
      slots.push({ priority: 1, value: tool.arguments });
    }
    if (task.output) slots.push({ priority: 2, value: task.output });
    slots.push({ priority: 3, value: task.userPrompt });
    slots.push({ priority: 4, value: task.systemPrompt });
  }
  slots.sort((left, right) => {
    if (left.priority !== right.priority) return left.priority - right.priority;
    return (
      (right.value.content?.length ?? 0) - (left.value.content?.length ?? 0)
    );
  });
  for (const slot of slots) {
    if (Buffer.byteLength(JSON.stringify(collection)) <= maximumBytes) break;
    if (slot.value.content === undefined) continue;
    delete slot.value.content;
    slot.value.omissionReason = "evidence_budget_exceeded";
  }
  collection.completeness = "partial";
  if (!collection.reasonCodes.includes("evidence_budget_exceeded")) {
    collection.reasonCodes.push("evidence_budget_exceeded");
    collection.reasonCodes.sort();
  }
}

export function mergeTaskEvidenceCollections(
  collections: PiTaskEvidenceCollection[],
  expectedTaskIds: string[] = [],
): PiTaskEvidenceCollection {
  const tasks = new Map<string, PiTaskExecutionEvidence>();
  const reasons = new Set<
    "evidence_budget_exceeded" | "task_evidence_unavailable"
  >();
  for (const collection of collections) {
    for (const reason of collection.reasonCodes) reasons.add(reason);
    for (const task of collection.tasks) {
      const previous = tasks.get(task.taskId);
      if (previous && canonicalJson(previous) !== canonicalJson(task)) {
        throw new Error(`task evidence conflict for ${task.taskId}`);
      }
      tasks.set(task.taskId, task);
    }
  }
  const missing = expectedTaskIds.some((taskId) => !tasks.has(taskId));
  if (missing) reasons.add("task_evidence_unavailable");
  return {
    schemaVersion: "argus.pi-review.task_evidence.v0",
    authority: "diagnostic_only",
    provenanceClass: "worker_self_report",
    contentPolicy: "exact_local_sensitive",
    completeness: reasons.size === 0 ? "complete" : "partial",
    reasonCodes: [...reasons].sort(),
    tasks: [...tasks.values()].sort((left, right) =>
      left.taskId.localeCompare(right.taskId),
    ),
  };
}
