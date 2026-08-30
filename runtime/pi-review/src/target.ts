import { isUtf8 } from "node:buffer";
import path from "node:path";

import { parseStrictJson, sha256 } from "./protocol.js";
import { sensitiveInputPathReason } from "./sensitive-path.js";
import type { FileStatus, TargetFile, TargetSnapshot } from "./types.js";

export type FrozenTargetMode = "diff" | "selection" | "scope";

interface FrozenRegion {
  path: string;
  start_line: number;
  end_line: number;
  sha256: string;
}

interface FrozenFile {
  path: string;
  sha256: string;
  size_bytes: number;
  content: string | null;
}

export interface FrozenReviewInput {
  schema_version: "argus.review_input.v1alpha1";
  target_id: string;
  target_mode: FrozenTargetMode;
  canonical_patch: string;
  regions: FrozenRegion[];
  files: FrozenFile[];
  contexts: FrozenContextBinding[];
}

export type FrozenContextBinding =
  | { ref: FrozenContextRef }
  | { gap: FrozenContextGap };

export interface FrozenContextRef {
  context_id: string;
  kind: string;
  revision: string;
  digest: string;
  coverage: FrozenContextCoverage;
  provenance: FrozenContextProvenance;
  artifact_uri: string;
  contract: string;
  size_bytes: number;
}

export interface FrozenContextGap {
  context_id: string;
  kind: string;
  revision: string;
  digest: string;
  coverage: FrozenContextCoverage;
  provenance: FrozenContextProvenance;
  reason_code: string;
}

interface FrozenContextCoverage {
  spans: Array<{ path: string; start_line: number; end_line: number }>;
  symbols: string[];
}

interface FrozenContextProvenance {
  provider: string;
  producer_id: string;
  producer_revision: string;
}

const frozenTargets = new WeakSet<TargetSnapshot>();

export function isFrozenTarget(target: TargetSnapshot): boolean {
  return frozenTargets.has(target);
}

export function materializeFrozenTarget(
  reviewInputBytes: Buffer,
  expectedTargetDigest: string,
  generatedAt = new Date().toISOString(),
  availableContextIDs: readonly string[] = [],
): TargetSnapshot {
  const input = decodeFrozenReviewInput(reviewInputBytes);
  const actualDigest = sha256(reviewInputBytes);
  if (actualDigest !== expectedTargetDigest) {
    throw new Error("frozen target digest does not match review input bytes");
  }

  const regionsByPath = new Map<
    string,
    Array<{ startLine: number; endLine: number }>
  >();
  for (const region of input.regions) {
    const regions = regionsByPath.get(region.path) ?? [];
    regions.push({ startLine: region.start_line, endLine: region.end_line });
    regionsByPath.set(region.path, regions);
  }

  const skipped: TargetSnapshot["skipped"] = [];
  const files: TargetFile[] = [];
  for (const file of input.files) {
    if (file.content === null) {
      skipped.push({
        path: file.path,
        reason: "frozen_target_content_unavailable",
      });
      continue;
    }
    const patch =
      input.target_mode === "diff"
        ? patchSectionForPath(input.canonical_patch, file.path)
        : fullFilePatch(file.path, file.content);
    if (!patch) {
      throw new Error(
        `canonical_patch does not contain target-side section for ${file.path}`,
      );
    }
    const status =
      input.target_mode === "diff" ? patchStatus(patch) : ("full" as const);
    if (status === "deleted") {
      throw new Error(
        `deleted target ${file.path} must not carry final content`,
      );
    }
    files.push({
      path: file.path,
      status,
      patch,
      targetContent: file.content,
      digest: file.sha256,
      targetDigest: `sha256:${file.sha256}`,
      patchDigest: `sha256:${sha256(patch)}`,
      changedLines: countChangedLines(patch),
      ...(input.target_mode === "diff"
        ? {}
        : { authorizedRegions: regionsByPath.get(file.path) ?? [] }),
    });
  }
  if (files.length === 0) {
    throw new Error(
      "review input has no reviewable frozen inline file content",
    );
  }

  files.sort((left, right) => left.path.localeCompare(right.path));
  skipped.sort((left, right) => left.path.localeCompare(right.path));
  const availableContexts = new Set(availableContextIDs);
  const contextGaps = input.contexts.flatMap((binding) => {
    if ("ref" in binding) {
      return availableContexts.has(binding.ref.context_id)
        ? []
        : [
            `context ${binding.ref.context_id} is reference-only and unavailable to the frozen-input worker`,
          ];
    }
    return [
      `context ${binding.gap.context_id} is unavailable: ${binding.gap.reason_code}`,
    ];
  });
  const target: TargetSnapshot = {
    kind: input.target_mode === "diff" ? "commit_diff" : "files",
    repository: `memory://review-input/${encodeURIComponent(input.target_id)}`,
    digest: actualDigest,
    capturedAt: generatedAt,
    generatedAt,
    canonicalPatchDigest: `sha256:${sha256(input.canonical_patch)}`,
    files,
    skipped,
    frozenContextGaps: contextGaps,
  };
  frozenTargets.add(target);
  return target;
}

export function frozenAnchorAuthorized(
  target: TargetSnapshot,
  file: TargetFile,
  startLine: number,
  endLine: number,
): boolean {
  if (!isFrozenTarget(target)) return true;
  if (file.authorizedRegions === undefined) return true;
  return file.authorizedRegions.some(
    (region) => startLine >= region.startLine && endLine <= region.endLine,
  );
}

export function decodeFrozenReviewInput(data: Buffer): FrozenReviewInput {
  if (data.length === 0 || !isUtf8(data) || data.includes(0)) {
    throw new Error("ReviewInput must be non-empty UTF-8 JSON without NUL");
  }
  const value = parseStrictJson(data.toString("utf8"));
  const object = exactObject(value, "ReviewInput", [
    "schema_version",
    "target_id",
    "target_mode",
    "canonical_patch",
    "regions",
    "files",
    "contexts",
  ]);
  literal(
    "ReviewInput.schema_version",
    object.schema_version,
    "argus.review_input.v1alpha1",
  );
  const targetMode = oneOf("ReviewInput.target_mode", object.target_mode, [
    "diff",
    "selection",
    "scope",
  ] as const);
  const canonicalPatch = stringValue(
    "ReviewInput.canonical_patch",
    object.canonical_patch,
  );
  if (targetMode === "diff") {
    if (
      canonicalPatch.length === 0 ||
      canonicalPatch.includes("\r") ||
      !canonicalPatch.endsWith("\n") ||
      !canonicalPatch.startsWith("diff --git ")
    ) {
      throw new Error("diff ReviewInput requires a canonical LF Git patch");
    }
  } else if (canonicalPatch !== "") {
    throw new Error(`${targetMode} ReviewInput must not carry a Git patch`);
  }
  const files = arrayValue("ReviewInput.files", object.files).map(
    (item, index): FrozenFile => {
      const file = exactObject(item, `ReviewInput.files[${index}]`, [
        "path",
        "sha256",
        "size_bytes",
        "content",
      ]);
      const content =
        file.content === null
          ? null
          : stringValue(`ReviewInput.files[${index}].content`, file.content);
      const result = {
        path: repositoryPath(`ReviewInput.files[${index}].path`, file.path),
        sha256: digest(`ReviewInput.files[${index}].sha256`, file.sha256),
        size_bytes: nonNegativeInteger(
          `ReviewInput.files[${index}].size_bytes`,
          file.size_bytes,
        ),
        content,
      };
      if (
        content !== null &&
        (Buffer.byteLength(content) !== result.size_bytes ||
          sha256(content) !== result.sha256)
      ) {
        throw new Error(
          `ReviewInput.files[${index}] content does not match size/digest`,
        );
      }
      return result;
    },
  );
  assertSortedUnique(
    "ReviewInput.files",
    files.map((file) => file.path),
  );
  const fileByPath = new Map(files.map((file) => [file.path, file]));

  const regions = arrayValue("ReviewInput.regions", object.regions).map(
    (item, index): FrozenRegion => {
      const region = exactObject(item, `ReviewInput.regions[${index}]`, [
        "path",
        "start_line",
        "end_line",
        "sha256",
      ]);
      const result = {
        path: repositoryPath(`ReviewInput.regions[${index}].path`, region.path),
        start_line: positiveInteger(
          `ReviewInput.regions[${index}].start_line`,
          region.start_line,
        ),
        end_line: positiveInteger(
          `ReviewInput.regions[${index}].end_line`,
          region.end_line,
        ),
        sha256: digest(`ReviewInput.regions[${index}].sha256`, region.sha256),
      };
      if (result.end_line < result.start_line) {
        throw new Error(`ReviewInput.regions[${index}] range is invalid`);
      }
      const file = fileByPath.get(result.path);
      if (!file || file.sha256 !== result.sha256 || file.content === null) {
        throw new Error(
          `ReviewInput.regions[${index}] does not bind inline frozen content`,
        );
      }
      if (result.end_line > lineCount(file.content)) {
        throw new Error(`ReviewInput.regions[${index}] exceeds file content`);
      }
      return result;
    },
  );
  for (let index = 1; index < regions.length; index++) {
    const previous = regions[index - 1] as FrozenRegion;
    const current = regions[index] as FrozenRegion;
    if (
      current.path < previous.path ||
      (current.path === previous.path &&
        current.start_line <= previous.end_line)
    ) {
      throw new Error(
        "ReviewInput.regions must be sorted, unique and non-overlapping",
      );
    }
  }
  if (targetMode === "diff" && regions.length !== 0) {
    throw new Error(
      "diff ReviewInput must derive authorization from its patch",
    );
  }
  if (targetMode === "selection" && regions.length === 0) {
    throw new Error("selection ReviewInput requires authorized regions");
  }

  const contexts = arrayValue("ReviewInput.contexts", object.contexts).map(
    parseContextBinding,
  );
  const contextIDs = contexts.map((binding) =>
    "ref" in binding ? binding.ref.context_id : binding.gap.context_id,
  );
  assertSortedUnique("ReviewInput.contexts", contextIDs);
  return {
    schema_version: "argus.review_input.v1alpha1",
    target_id: identifier("ReviewInput.target_id", object.target_id),
    target_mode: targetMode,
    canonical_patch: canonicalPatch,
    regions,
    files,
    contexts,
  };
}

function parseContextBinding(
  value: unknown,
  index: number,
): FrozenContextBinding {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`ReviewInput.contexts[${index}] must be an object`);
  }
  const object = value as Record<string, unknown>;
  const keys = Object.keys(object);
  if (keys.length !== 1 || (keys[0] !== "ref" && keys[0] !== "gap")) {
    throw new Error(
      `ReviewInput.contexts[${index}] must contain exactly one ref or gap`,
    );
  }
  if (keys[0] === "ref") {
    const ref = exactObject(object.ref, `ReviewInput.contexts[${index}].ref`, [
      "context_id",
      "kind",
      "revision",
      "digest",
      "coverage",
      "provenance",
      "artifact_uri",
      "contract",
      "size_bytes",
    ]);
    return {
      ref: {
        ...parseContextIdentity(ref, `ReviewInput.contexts[${index}].ref`),
        artifact_uri: identifier(
          `ReviewInput.contexts[${index}].ref.artifact_uri`,
          ref.artifact_uri,
        ),
        contract: identifier(
          `ReviewInput.contexts[${index}].ref.contract`,
          ref.contract,
        ),
        size_bytes: nonNegativeInteger(
          `ReviewInput.contexts[${index}].ref.size_bytes`,
          ref.size_bytes,
        ),
      },
    };
  }
  const gap = exactObject(object.gap, `ReviewInput.contexts[${index}].gap`, [
    "context_id",
    "kind",
    "revision",
    "digest",
    "coverage",
    "provenance",
    "reason_code",
  ]);
  return {
    gap: {
      ...parseContextIdentity(gap, `ReviewInput.contexts[${index}].gap`),
      reason_code: identifier(
        `ReviewInput.contexts[${index}].gap.reason_code`,
        gap.reason_code,
      ),
    },
  };
}

function parseContextIdentity(
  object: Record<string, unknown>,
  name: string,
): Omit<FrozenContextRef, "artifact_uri" | "contract" | "size_bytes"> {
  const coverage = exactObject(object.coverage, `${name}.coverage`, [
    "spans",
    "symbols",
  ]);
  const spans = arrayValue(`${name}.coverage.spans`, coverage.spans).map(
    (item, index) => {
      const span = exactObject(item, `${name}.coverage.spans[${index}]`, [
        "path",
        "start_line",
        "end_line",
      ]);
      const start = positiveInteger(
        `${name}.coverage.spans[${index}].start_line`,
        span.start_line,
      );
      const end = positiveInteger(
        `${name}.coverage.spans[${index}].end_line`,
        span.end_line,
      );
      if (end < start) throw new Error(`${name}.coverage span is invalid`);
      return {
        path: repositoryPath(
          `${name}.coverage.spans[${index}].path`,
          span.path,
        ),
        start_line: start,
        end_line: end,
      };
    },
  );
  const symbols = arrayValue(`${name}.coverage.symbols`, coverage.symbols).map(
    (item, index) => identifier(`${name}.coverage.symbols[${index}]`, item),
  );
  if (spans.length === 0 && symbols.length === 0) {
    throw new Error(`${name}.coverage must not be empty`);
  }
  const provenance = exactObject(object.provenance, `${name}.provenance`, [
    "provider",
    "producer_id",
    "producer_revision",
  ]);
  return {
    context_id: identifier(`${name}.context_id`, object.context_id),
    kind: identifier(`${name}.kind`, object.kind),
    revision: identifier(`${name}.revision`, object.revision),
    digest: digest(`${name}.digest`, object.digest),
    coverage: { spans, symbols },
    provenance: {
      provider: identifier(`${name}.provenance.provider`, provenance.provider),
      producer_id: identifier(
        `${name}.provenance.producer_id`,
        provenance.producer_id,
      ),
      producer_revision: identifier(
        `${name}.provenance.producer_revision`,
        provenance.producer_revision,
      ),
    },
  };
}

function patchSectionForPath(patch: string, filePath: string): string {
  const targetMarker = `+++ b/${filePath}\n`;
  const marker = patch.indexOf(targetMarker);
  if (marker < 0) return "";
  const sectionStartMarker = patch.lastIndexOf("\ndiff --git ", marker);
  const start = sectionStartMarker < 0 ? 0 : sectionStartMarker + 1;
  const next = patch.indexOf("\ndiff --git ", marker + targetMarker.length);
  return patch.slice(start, next < 0 ? patch.length : next + 1);
}

function patchStatus(patch: string): FileStatus {
  if (/^new file mode /mu.test(patch)) return "added";
  if (/^deleted file mode /mu.test(patch)) return "deleted";
  return "modified";
}

function fullFilePatch(filePath: string, content: string): string {
  const lines = content.split("\n");
  const body = lines.map((line) => `+${line}`).join("\n");
  return [
    `diff --git a/${filePath} b/${filePath}`,
    "new file mode 100644",
    "--- /dev/null",
    `+++ b/${filePath}`,
    `@@ -0,0 +1,${lines.length} @@`,
    body,
  ].join("\n");
}

function countChangedLines(patch: string): number {
  return patch
    .split("\n")
    .filter(
      (line) =>
        (line.startsWith("+") && !line.startsWith("+++")) ||
        (line.startsWith("-") && !line.startsWith("---")),
    ).length;
}

function lineCount(content: string): number {
  return splitAddressableSourceLines(content).length;
}

// Source positions address logical file lines. A final line terminator closes
// the preceding line; it does not create another addressable empty line. Keep
// this shared with tools and workflow admission so provider-visible line
// numbers cannot escape the Go host's exact frozen-excerpt semantics.
export function splitAddressableSourceLines(content: string): string[] {
  const normalized = content.replace(/\r\n?/gu, "\n");
  const lines = normalized.split("\n");
  if (normalized.endsWith("\n")) lines.pop();
  return lines;
}

function exactObject(
  value: unknown,
  name: string,
  keys: readonly string[],
): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${name} must be an object`);
  }
  const object = value as Record<string, unknown>;
  if (
    JSON.stringify(Object.keys(object).sort()) !==
    JSON.stringify([...keys].sort())
  ) {
    throw new Error(`${name} has missing or unknown fields`);
  }
  return object;
}

function arrayValue(name: string, value: unknown): unknown[] {
  if (!Array.isArray(value)) throw new Error(`${name} must be an array`);
  return value;
}

function stringValue(name: string, value: unknown): string {
  if (typeof value !== "string" || value.includes("\0")) {
    throw new Error(`${name} must be a UTF-8 string without NUL`);
  }
  return value;
}

function identifier(name: string, value: unknown): string {
  const text = stringValue(name, value);
  if (
    text.length === 0 ||
    text !== text.trim() ||
    Buffer.byteLength(text) > 256 ||
    /[\u0000-\u001f\u007f]/u.test(text)
  ) {
    throw new Error(`${name} must be a bounded trimmed identifier`);
  }
  return text;
}

function digest(name: string, value: unknown): string {
  const text = stringValue(name, value);
  if (!/^[0-9a-f]{64}$/u.test(text)) {
    throw new Error(`${name} must be lowercase SHA-256 hex`);
  }
  return text;
}

function repositoryPath(name: string, value: unknown): string {
  const text = stringValue(name, value);
  const normalized = path.posix.normalize(text.replaceAll("\\", "/"));
  if (
    text !== normalized ||
    normalized === "." ||
    normalized === ".." ||
    normalized.startsWith("../") ||
    normalized.startsWith("/") ||
    normalized.includes(":") ||
    /[\u0000-\u001f\u007f]/u.test(normalized)
  ) {
    throw new Error(`${name} must be a clean repository-relative path`);
  }
  const sensitive = sensitiveInputPathReason(normalized);
  if (sensitive) throw new Error(`${name} is sensitive (${sensitive})`);
  return normalized;
}

function nonNegativeInteger(name: string, value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${name} must be a non-negative integer`);
  }
  return value;
}

function positiveInteger(name: string, value: unknown): number {
  const result = nonNegativeInteger(name, value);
  if (result === 0) throw new Error(`${name} must be positive`);
  return result;
}

function literal<T extends string>(
  name: string,
  value: unknown,
  expected: T,
): T {
  if (value !== expected) throw new Error(`${name} must be ${expected}`);
  return expected;
}

function oneOf<const T extends readonly string[]>(
  name: string,
  value: unknown,
  allowed: T,
): T[number] {
  if (typeof value !== "string" || !allowed.includes(value)) {
    throw new Error(`${name} is unsupported`);
  }
  return value as T[number];
}

function assertSortedUnique(name: string, values: string[]): void {
  for (let index = 1; index < values.length; index++) {
    if ((values[index] as string) <= (values[index - 1] as string)) {
      throw new Error(`${name} must be uniquely sorted`);
    }
  }
}
