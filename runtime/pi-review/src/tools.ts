import { lstat } from "node:fs/promises";
import path from "node:path";

import type { AgentTool } from "@earendil-works/pi-agent-core";
import { Type } from "@earendil-works/pi-ai";

import { runCommand } from "./command.js";
import {
  freezeContextFile,
  listRepositoryFiles,
  normalizeRepositoryPath,
  readRepositoryFile,
} from "./git.js";
import { isFrozenTarget, splitAddressableSourceLines } from "./target.js";
import type { ChangeGroup, TargetSnapshot } from "./types.js";

const MAX_READ_LINES = 500;
const MAX_SEARCH_FILES = 600;
const MAX_SEARCH_RESULTS = 100;
const MAX_TOOL_OUTPUT_BYTES = 64 * 1024;

export interface ToolAudit {
  calls: number;
  names: string[];
}

export function createReadOnlyTools(
  target: TargetSnapshot,
  group: ChangeGroup,
  maxToolCalls: number,
  verbose?: (message: string) => void,
): { tools: AgentTool[]; audit: ToolAudit } {
  const audit: ToolAudit = { calls: 0, names: [] };
  const guard = (name: string, signal?: AbortSignal): void => {
    signal?.throwIfAborted();
    audit.calls++;
    audit.names.push(name);
    if (audit.calls > maxToolCalls)
      throw new Error(`tool-call budget exceeded (${maxToolCalls})`);
    verbose?.(`${group.id}: tool ${name}`);
  };

  const readFileParameters = Type.Object(
    {
      path: Type.String({ minLength: 1 }),
      view: Type.Union([Type.Literal("target"), Type.Literal("base")]),
      startLine: Type.Optional(Type.Integer({ minimum: 1 })),
      endLine: Type.Optional(Type.Integer({ minimum: 1 })),
    },
    { additionalProperties: false },
  );
  const readFileTool: AgentTool<typeof readFileParameters> = {
    name: "read_file",
    label: "Read repository file",
    description:
      "Read a bounded line range from a repository file. Use target for current/head content and base for the old side.",
    parameters: readFileParameters,
    async execute(_callId, params, signal) {
      guard("read_file", signal);
      const content = await readRepositoryFile(
        target,
        params.path,
        params.view,
        signal,
      );
      const lines = splitAddressableSourceLines(content);
      const start = params.startLine ?? 1;
      const requestedEnd =
        params.endLine ?? Math.min(lines.length, start + MAX_READ_LINES - 1);
      if (requestedEnd < start)
        throw new Error("endLine must be greater than or equal to startLine");
      const end = Math.min(
        requestedEnd,
        start + MAX_READ_LINES - 1,
        lines.length,
      );
      const text = lines
        .slice(start - 1, end)
        .map((line, index) => `${start + index}: ${line}`)
        .join("\n");
      return resultText(text || "<empty range>", {
        path: params.path,
        view: params.view,
        start,
        end,
      });
    },
  };

  const listFilesParameters = Type.Object(
    {
      prefix: Type.Optional(Type.String()),
      suffix: Type.Optional(Type.String()),
      limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 200 })),
    },
    { additionalProperties: false },
  );
  const listFilesTool: AgentTool<typeof listFilesParameters> = {
    name: "list_files",
    label: "List repository files",
    description:
      "List repository-relative file paths by optional directory prefix and suffix.",
    parameters: listFilesParameters,
    async execute(_callId, params, signal) {
      guard("list_files", signal);
      const files = await listRepositoryFiles(
        target,
        params.prefix ?? "",
        params.suffix ?? "",
        params.limit ?? 100,
        signal,
      );
      return resultText(files.join("\n") || "<no files>", {
        count: files.length,
      });
    },
  };

  const searchCodeParameters = Type.Object(
    {
      query: Type.String({ minLength: 2, maxLength: 200 }),
      prefix: Type.Optional(Type.String()),
      suffix: Type.Optional(Type.String()),
      limit: Type.Optional(
        Type.Integer({ minimum: 1, maximum: MAX_SEARCH_RESULTS }),
      ),
    },
    { additionalProperties: false },
  );
  const searchCodeTool: AgentTool<typeof searchCodeParameters> = {
    name: "search_code",
    label: "Search repository code",
    description:
      "Search for a literal string in repository text files. This is not a regular-expression search.",
    parameters: searchCodeParameters,
    async execute(_callId, params, signal) {
      guard("search_code", signal);
      const files = await listRepositoryFiles(
        target,
        params.prefix ?? "",
        params.suffix ?? "",
        MAX_SEARCH_FILES,
        signal,
      );
      const results: string[] = [];
      const limit = params.limit ?? 50;
      for (const file of files) {
        signal?.throwIfAborted();
        let content: string;
        try {
          content = await readRepositoryFile(
            target,
            file,
            "target",
            signal,
            false,
          );
        } catch (error) {
          if (signal?.aborted) throw signal.reason ?? error;
          continue;
        }
        let matched = false;
        for (const [index, line] of splitAddressableSourceLines(
          content,
        ).entries()) {
          if (!line.includes(params.query)) continue;
          matched = true;
          results.push(`${file}:${index + 1}:${line}`);
          if (results.length >= limit) break;
        }
        if (matched) freezeContextFile(target, file, content);
        if (results.length >= limit) break;
      }
      return resultText(results.join("\n") || "<no matches>", {
        matches: results.length,
        filesScanned: files.length,
      });
    },
  };

  const tools: AgentTool[] = [readFileTool, listFilesTool, searchCodeTool];
  const codegraphPath = path.join(target.repository, ".codegraph");
  const codegraphParameters = Type.Object(
    { question: Type.String({ minLength: 3, maxLength: 1000 }) },
    { additionalProperties: false },
  );
  const codegraphTool: AgentTool<typeof codegraphParameters> = {
    name: "query_codegraph",
    label: "Query CodeGraph",
    description:
      "Query the repository's existing CodeGraph index for symbol source, callers, callees and type relationships. Fails when no index exists.",
    parameters: codegraphParameters,
    async execute(_callId, params, signal) {
      guard("query_codegraph", signal);
      if (target.kind === "commit_diff" || target.headOid) {
        throw new Error(
          "CodeGraph is disabled for revision-bound targets because the ambient index revision cannot be proven",
        );
      }
      const info = await lstat(codegraphPath).catch(() => undefined);
      if (!info?.isDirectory())
        throw new Error("repository has no .codegraph index");
      const output = await runCommand(
        "codegraph",
        ["explore", params.question],
        {
          cwd: target.repository,
          ...(signal ? { signal } : {}),
          maxOutputBytes: MAX_TOOL_OUTPUT_BYTES,
        },
      );
      return resultText(
        `Navigation-only ambient index; confirm decisive evidence with read_file.\n\n${output.stdout.toString("utf8")}`,
        { indexed: true, revisionBound: false },
      );
    },
  };
  if (!isFrozenTarget(target)) tools.push(codegraphTool);
  return { tools, audit };
}

function resultText(text: string, details: Record<string, unknown>) {
  const bytes = Buffer.byteLength(text);
  const bounded =
    bytes <= MAX_TOOL_OUTPUT_BYTES
      ? text
      : `${Buffer.from(text).subarray(0, MAX_TOOL_OUTPUT_BYTES).toString("utf8")}\n<output truncated>`;
  return {
    content: [{ type: "text" as const, text: bounded }],
    details,
  };
}
