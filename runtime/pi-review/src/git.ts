import { createHash } from "node:crypto";
import { lstat, readFile, realpath, stat } from "node:fs/promises";
import path from "node:path";

import { runCommand } from "./command.js";
import { CliError } from "./errors.js";
import { sensitiveInputPathReason } from "./sensitive-path.js";
import { isFrozenTarget } from "./target.js";
import type {
  ReviewOptions,
  SkippedTarget,
  TargetFile,
  TargetSnapshot,
} from "./types.js";

const MAX_FILE_BYTES = 1024 * 1024;
const MAX_PATCH_BYTES = 4 * 1024 * 1024;
const frozenContextFiles = new WeakMap<TargetSnapshot, Map<string, string>>();

interface CaptureContext {
  repository: string;
  baseOid?: string;
  headOid?: string;
  workingTree: boolean;
  signal?: AbortSignal;
}

class FileTooLargeError extends Error {}

export async function resolveRepository(repository: string): Promise<string> {
  const requested = await realpath(path.resolve(repository)).catch(
    () => undefined,
  );
  if (!requested)
    throw new CliError(`repository does not exist: ${repository}`);
  const result = await git(requested, ["rev-parse", "--show-toplevel"]);
  const root = await realpath(result.stdout.toString("utf8").trim());
  const rootStat = await stat(root);
  if (!rootStat.isDirectory()) throw new CliError(`not a directory: ${root}`);
  return root;
}

export async function captureTarget(
  options: ReviewOptions,
  signal?: AbortSignal,
): Promise<TargetSnapshot> {
  const repository = await resolveRepository(options.repository);
  const capturedAt = new Date().toISOString();
  let baseOid: string | undefined;
  let headOid: string | undefined;
  let files: TargetFile[] = [];
  let skipped: SkippedTarget[] = [];

  switch (options.target.kind) {
    case "working_changes": {
      baseOid = options.target.base
        ? await resolveCommit(repository, options.target.base, signal)
        : await resolveOptionalHead(repository, signal);
      const context: CaptureContext = {
        repository,
        workingTree: true,
        ...(baseOid ? { baseOid } : {}),
        ...(signal ? { signal } : {}),
      };
      const changed = baseOid
        ? await diffPaths(
            repository,
            baseOid,
            undefined,
            options.target.paths,
            signal,
          )
        : await stagedPaths(repository, options.target.paths, signal);
      const untracked = await untrackedPaths(
        repository,
        options.target.paths,
        signal,
      );
      const allPaths = [...new Set([...changed, ...untracked])].sort();
      await assertCaptureAdmission(context, allPaths, options);
      ({ files, skipped: skipped } = await capturePaths(context, allPaths));
      break;
    }
    case "commit_diff": {
      baseOid = await resolveCommit(repository, options.target.base, signal);
      headOid = await resolveCommit(repository, options.target.head, signal);
      const paths = await diffPaths(
        repository,
        baseOid,
        headOid,
        options.target.paths,
        signal,
      );
      const context: CaptureContext = {
        repository,
        baseOid,
        headOid,
        workingTree: false,
        ...(signal ? { signal } : {}),
      };
      await assertCaptureAdmission(context, paths, options);
      ({ files, skipped: skipped } = await capturePaths(context, paths));
      break;
    }
    case "files": {
      if (options.target.paths.length === 0) {
        throw new CliError("files target requires at least one path");
      }
      if (options.target.revision) {
        headOid = await resolveCommit(
          repository,
          options.target.revision,
          signal,
        );
      }
      await assertCaptureAdmission(
        {
          repository,
          ...(headOid ? { headOid } : {}),
          workingTree: headOid === undefined,
          ...(signal ? { signal } : {}),
        },
        options.target.paths,
        options,
      );
      ({ files, skipped: skipped } = await captureExplicitFiles(
        repository,
        options.target.paths,
        headOid,
        signal,
      ));
      break;
    }
  }

  const withinGroupBudget: TargetFile[] = [];
  for (const file of files) {
    if (Buffer.byteLength(file.patch) > options.maxGroupBytes) {
      skipped.push({ path: file.path, reason: "group_patch_budget_exceeded" });
      continue;
    }
    withinGroupBudget.push(file);
  }
  files = withinGroupBudget;

  if (files.length === 0) {
    const detail =
      skipped.length > 0 ? ` (${skipped.length} target(s) skipped)` : "";
    throw new CliError(`no reviewable text files found${detail}`);
  }
  files.sort((left, right) => left.path.localeCompare(right.path));
  skipped.sort((left, right) => left.path.localeCompare(right.path));
  const digest = hash({
    kind: options.target.kind,
    baseOid: baseOid ?? null,
    headOid: headOid ?? null,
    files: files.map((file) => [
      file.path,
      file.status,
      file.digest,
      file.patch,
    ]),
    skipped,
  });
  const base = {
    repository,
    digest,
    capturedAt,
    files,
    skipped,
  };
  return {
    ...base,
    kind: options.target.kind,
    ...(baseOid ? { baseOid } : {}),
    ...(headOid ? { headOid } : {}),
  };
}

export async function isTargetStale(
  snapshot: TargetSnapshot,
  options: ReviewOptions,
  signal?: AbortSignal,
): Promise<boolean> {
  if (isFrozenTarget(snapshot)) return false;
  if (snapshot.kind === "commit_diff" || snapshot.headOid) return false;
  const current = await captureTarget(
    { ...options, repository: snapshot.repository },
    signal,
  );
  if (current.digest !== snapshot.digest) return true;
  for (const [relativePath, content] of frozenContextFiles.get(snapshot) ??
    []) {
    const currentContent = await readContainedWorktreeText(
      snapshot.repository,
      relativePath,
    ).catch(() => undefined);
    if (currentContent !== content) return true;
  }
  return false;
}

export async function readRepositoryFile(
  snapshot: TargetSnapshot,
  relativePath: string,
  view: "target" | "base",
  signal?: AbortSignal,
  freeze = true,
): Promise<string> {
  const normalized = normalizeRepositoryPath(relativePath);
  const sensitiveReason = sensitivePathReason(normalized);
  if (sensitiveReason) {
    throw new Error(`sensitive repository path is not readable: ${normalized}`);
  }
  const captured = snapshot.files.find(
    (file) => file.path === normalized || file.oldPath === normalized,
  );
  if (view === "target" && captured?.targetContent !== undefined) {
    return captured.targetContent;
  }
  if (view === "base" && captured?.baseContent !== undefined) {
    return captured.baseContent;
  }
  if (isFrozenTarget(snapshot)) {
    throw new Error(
      `${view} view is not available in frozen inline input: ${normalized}`,
    );
  }
  const revision = view === "base" ? snapshot.baseOid : snapshot.headOid;
  if (revision) {
    const content = await readGitBlob(
      snapshot.repository,
      revision,
      normalized,
      signal,
    );
    if (content === undefined)
      throw new Error(`file not found in ${view}: ${normalized}`);
    return content;
  }
  if (view === "base")
    throw new Error(`base view unavailable for ${normalized}`);
  const frozen = frozenContextFiles.get(snapshot)?.get(normalized);
  if (frozen !== undefined) return frozen;
  const content = await readContainedWorktreeText(
    snapshot.repository,
    normalized,
  );
  if (freeze) freezeContextFile(snapshot, normalized, content);
  return content;
}

export function freezeContextFile(
  snapshot: TargetSnapshot,
  relativePath: string,
  content: string,
): void {
  const normalized = normalizeRepositoryPath(relativePath);
  const files = frozenContextFiles.get(snapshot) ?? new Map<string, string>();
  if (!files.has(normalized)) files.set(normalized, content);
  frozenContextFiles.set(snapshot, files);
}

export async function listRepositoryFiles(
  snapshot: TargetSnapshot,
  prefix: string,
  suffix: string,
  limit: number,
  signal?: AbortSignal,
): Promise<string[]> {
  const normalizedPrefix = prefix ? normalizeRepositoryPrefix(prefix) : "";
  if (isFrozenTarget(snapshot)) {
    signal?.throwIfAborted();
    return snapshot.files
      .map((file) => file.path)
      .filter(
        (name) =>
          (!normalizedPrefix || name.startsWith(normalizedPrefix)) &&
          (!suffix || name.endsWith(suffix)),
      )
      .sort()
      .slice(0, limit);
  }
  const args = [
    "ls-tree",
    "-r",
    "--name-only",
    "-z",
    snapshot.headOid ?? "HEAD",
  ];
  let names: string[];
  if (
    snapshot.kind === "commit_diff" ||
    (snapshot.kind === "files" && snapshot.headOid)
  ) {
    const revision = snapshot.headOid;
    if (!revision) throw new Error("revision unavailable");
    args[args.length - 1] = revision;
    const result = await git(snapshot.repository, args, signal);
    names = splitNul(result.stdout);
  } else {
    const tracked = await git(snapshot.repository, ["ls-files", "-z"], signal);
    const untracked = await git(
      snapshot.repository,
      ["ls-files", "--others", "--exclude-standard", "-z"],
      signal,
    );
    names = [
      ...splitNul(tracked.stdout),
      ...splitNul(untracked.stdout),
      ...snapshot.files
        .filter((file) => file.targetContent !== undefined)
        .map((file) => file.path),
    ];
  }
  return [...new Set(names)]
    .filter(
      (name) =>
        !sensitivePathReason(name) &&
        (!normalizedPrefix || name.startsWith(normalizedPrefix)) &&
        (!suffix || name.endsWith(suffix)),
    )
    .sort()
    .slice(0, limit);
}

async function assertCaptureAdmission(
  context: CaptureContext,
  paths: string[],
  options: Pick<ReviewOptions, "maxFiles" | "maxTargetBytes">,
): Promise<void> {
  const reviewablePaths = [
    ...new Set(paths.map(normalizeRepositoryPath)),
  ].filter((candidate) => !sensitivePathReason(candidate));
  if (reviewablePaths.length > options.maxFiles) {
    throw new CliError(
      `target has at least ${reviewablePaths.length} reviewable paths; --max-files is ${options.maxFiles}. Narrow the target or explicitly raise the limit`,
    );
  }
  let totalBytes = 0;
  for (const relativePath of reviewablePaths) {
    context.signal?.throwIfAborted();
    const baseSize = context.baseOid
      ? await gitBlobSize(
          context.repository,
          context.baseOid,
          relativePath,
          context.signal,
        )
      : undefined;
    const targetSize = context.workingTree
      ? await containedWorktreeFileSize(context.repository, relativePath)
      : context.headOid
        ? await gitBlobSize(
            context.repository,
            context.headOid,
            relativePath,
            context.signal,
          )
        : undefined;
    if (
      (baseSize ?? 0) > MAX_FILE_BYTES ||
      (targetSize ?? 0) > MAX_FILE_BYTES
    ) {
      continue;
    }
    totalBytes += (baseSize ?? 0) + (targetSize ?? 0);
    if (totalBytes > options.maxTargetBytes) {
      throw new CliError(
        `target source blobs exceed --max-target-bytes (${options.maxTargetBytes}) before content capture`,
      );
    }
  }
}

async function capturePaths(
  context: CaptureContext,
  paths: string[],
): Promise<{ files: TargetFile[]; skipped: SkippedTarget[] }> {
  const files: TargetFile[] = [];
  const skipped: SkippedTarget[] = [];
  for (const relativePath of paths) {
    context.signal?.throwIfAborted();
    const normalized = normalizeRepositoryPath(relativePath);
    const sensitiveReason = sensitivePathReason(normalized);
    if (sensitiveReason) {
      skipped.push({ path: normalized, reason: sensitiveReason });
      continue;
    }
    let baseContent: string | undefined;
    let targetContent: string | undefined;
    try {
      baseContent = context.baseOid
        ? await readGitBlob(
            context.repository,
            context.baseOid,
            normalized,
            context.signal,
          )
        : undefined;
      targetContent = context.workingTree
        ? await readWorktreeBlob(context.repository, normalized)
        : context.headOid
          ? await readGitBlob(
              context.repository,
              context.headOid,
              normalized,
              context.signal,
            )
          : undefined;
    } catch (error) {
      if (error instanceof FileTooLargeError) {
        skipped.push({ path: normalized, reason: "file_too_large" });
        continue;
      }
      throw error;
    }
    if (baseContent === undefined && targetContent === undefined) {
      skipped.push({ path: normalized, reason: "missing_on_both_sides" });
      continue;
    }
    if (isBinary(baseContent) || isBinary(targetContent)) {
      skipped.push({ path: normalized, reason: "binary" });
      continue;
    }
    if (
      (baseContent?.length ?? 0) > MAX_FILE_BYTES ||
      (targetContent?.length ?? 0) > MAX_FILE_BYTES
    ) {
      skipped.push({ path: normalized, reason: "file_too_large" });
      continue;
    }
    const patch = await patchForPath(
      context,
      normalized,
      baseContent,
      targetContent,
    );
    if (Buffer.byteLength(patch) > MAX_PATCH_BYTES) {
      skipped.push({ path: normalized, reason: "patch_too_large" });
      continue;
    }
    const status =
      baseContent === undefined
        ? "added"
        : targetContent === undefined
          ? "deleted"
          : "modified";
    const payload = {
      path: normalized,
      status,
      patch,
      digest: hash([normalized, baseContent ?? null, targetContent ?? null]),
      changedLines: countChangedLines(patch),
      ...(baseContent !== undefined ? { baseContent } : {}),
      ...(targetContent !== undefined ? { targetContent } : {}),
    } satisfies TargetFile;
    files.push(payload);
  }
  return { files, skipped };
}

async function captureExplicitFiles(
  repository: string,
  requestedPaths: string[],
  revision: string | undefined,
  signal?: AbortSignal,
): Promise<{ files: TargetFile[]; skipped: SkippedTarget[] }> {
  const files: TargetFile[] = [];
  const skipped: SkippedTarget[] = [];
  for (const requested of requestedPaths) {
    const relativePath = normalizeRepositoryPath(requested);
    const sensitiveReason = sensitivePathReason(relativePath);
    if (sensitiveReason) {
      skipped.push({ path: relativePath, reason: sensitiveReason });
      continue;
    }
    let content: string | undefined;
    try {
      if (revision) {
        content = await readGitBlob(repository, revision, relativePath, signal);
      } else {
        const absolute = safeAbsolutePath(repository, relativePath);
        const info = await lstat(absolute).catch(() => undefined);
        if (info?.isSymbolicLink()) {
          skipped.push({ path: relativePath, reason: "symlink" });
          continue;
        }
        if (info && !info.isFile()) {
          skipped.push({ path: relativePath, reason: "not_a_regular_file" });
          continue;
        }
        content = await readWorktreeBlob(repository, relativePath);
      }
    } catch (error) {
      if (error instanceof FileTooLargeError) {
        skipped.push({ path: relativePath, reason: "file_too_large" });
        continue;
      }
      throw error;
    }
    if (content === undefined) {
      skipped.push({ path: relativePath, reason: "missing" });
      continue;
    }
    if (isBinary(content)) {
      skipped.push({ path: relativePath, reason: "binary" });
      continue;
    }
    if (Buffer.byteLength(content) > MAX_FILE_BYTES) {
      skipped.push({ path: relativePath, reason: "file_too_large" });
      continue;
    }
    const patch = fullFilePatch(relativePath, content);
    files.push({
      path: relativePath,
      status: "full",
      patch,
      targetContent: content,
      digest: hash([relativePath, content]),
      changedLines: content.split("\n").length,
    });
  }
  return { files, skipped };
}

async function patchForPath(
  context: CaptureContext,
  relativePath: string,
  baseContent: string | undefined,
  targetContent: string | undefined,
): Promise<string> {
  if (!context.baseOid || (baseContent === undefined && context.workingTree)) {
    return fullFilePatch(relativePath, targetContent ?? "");
  }
  const args = [
    "diff",
    "--no-ext-diff",
    "--no-textconv",
    "--no-renames",
    "--unified=40",
    context.baseOid,
  ];
  if (!context.workingTree && context.headOid) args.push(context.headOid);
  args.push("--", relativePath);
  const result = await git(context.repository, args, context.signal);
  const patch = result.stdout.toString("utf8");
  if (patch) return patch;
  if (targetContent !== undefined)
    return fullFilePatch(relativePath, targetContent);
  return `diff --git a/${relativePath} b/${relativePath}\ndeleted file\n`;
}

async function diffPaths(
  repository: string,
  baseOid: string,
  headOid: string | undefined,
  paths: string[],
  signal?: AbortSignal,
): Promise<string[]> {
  const args = [
    "diff",
    "--no-ext-diff",
    "--no-textconv",
    "--no-renames",
    "--name-only",
    "-z",
    baseOid,
  ];
  if (headOid) args.push(headOid);
  args.push("--", ...paths);
  const result = await git(repository, args, signal);
  return splitNul(result.stdout).map(normalizeRepositoryPath);
}

async function untrackedPaths(
  repository: string,
  paths: string[],
  signal?: AbortSignal,
): Promise<string[]> {
  const result = await git(
    repository,
    ["ls-files", "--others", "--exclude-standard", "-z", "--", ...paths],
    signal,
  );
  return splitNul(result.stdout).map(normalizeRepositoryPath);
}

async function stagedPaths(
  repository: string,
  paths: string[],
  signal?: AbortSignal,
): Promise<string[]> {
  const result = await git(
    repository,
    ["diff", "--cached", "--name-only", "-z", "--", ...paths],
    signal,
  );
  return splitNul(result.stdout).map(normalizeRepositoryPath);
}

async function resolveOptionalHead(
  repository: string,
  signal?: AbortSignal,
): Promise<string | undefined> {
  const result = await git(
    repository,
    ["rev-parse", "--verify", "HEAD^{commit}"],
    signal,
    [0, 128],
  );
  if (result.exitCode !== 0) return undefined;
  return validateOid(result.stdout.toString("utf8").trim());
}

async function resolveCommit(
  repository: string,
  revision: string,
  signal?: AbortSignal,
): Promise<string> {
  if (!revision || revision.startsWith("-"))
    throw new CliError(`invalid revision: ${revision}`);
  const result = await git(
    repository,
    ["rev-parse", "--verify", `${revision}^{commit}`],
    signal,
  );
  return validateOid(result.stdout.toString("utf8").trim());
}

function validateOid(value: string): string {
  if (!/^[0-9a-f]{40,64}$/u.test(value))
    throw new CliError(`Git returned an invalid object ID: ${value}`);
  return value;
}

async function readGitBlob(
  repository: string,
  revision: string,
  relativePath: string,
  signal?: AbortSignal,
): Promise<string | undefined> {
  const object = `${revision}:${relativePath}`;
  const size = await gitBlobSize(repository, revision, relativePath, signal);
  if (size === undefined) return undefined;
  if (size > MAX_FILE_BYTES) throw new FileTooLargeError(relativePath);
  const result = await git(
    repository,
    ["show", object],
    signal,
    [0],
    MAX_FILE_BYTES + 1,
  );
  return result.stdout.toString("utf8");
}

async function gitBlobSize(
  repository: string,
  revision: string,
  relativePath: string,
  signal?: AbortSignal,
): Promise<number | undefined> {
  const object = `${revision}:${relativePath}`;
  const sizeResult = await git(
    repository,
    ["cat-file", "-s", object],
    signal,
    [0, 128],
    1024,
  );
  if (sizeResult.exitCode !== 0) return undefined;
  const size = Number(sizeResult.stdout.toString("utf8").trim());
  if (!Number.isSafeInteger(size) || size < 0) {
    throw new Error(`invalid Git blob size for ${relativePath}`);
  }
  return size;
}

async function readWorktreeBlob(
  repository: string,
  relativePath: string,
): Promise<string | undefined> {
  const absolute = safeAbsolutePath(repository, relativePath);
  const info = await lstat(absolute).catch(() => undefined);
  if (!info) return undefined;
  if (info.isSymbolicLink() || !info.isFile()) return undefined;
  return await readContainedWorktreeText(repository, relativePath);
}

async function readContainedWorktreeText(
  repository: string,
  relativePath: string,
): Promise<string> {
  const absolute = safeAbsolutePath(repository, relativePath);
  const info = await lstat(absolute);
  if (info.isSymbolicLink() || !info.isFile()) {
    throw new Error("path is not a regular non-symlink file");
  }
  if (info.size > MAX_FILE_BYTES) {
    throw new FileTooLargeError(relativePath);
  }
  const resolved = await realpath(absolute);
  const prefix = `${repository}${path.sep}`;
  if (!resolved.startsWith(prefix)) {
    throw new Error(`path escapes repository through symlink: ${relativePath}`);
  }
  return await readRegularTextFile(resolved);
}

async function containedWorktreeFileSize(
  repository: string,
  relativePath: string,
): Promise<number | undefined> {
  const absolute = safeAbsolutePath(repository, relativePath);
  const info = await lstat(absolute).catch(() => undefined);
  if (!info || info.isSymbolicLink() || !info.isFile()) return undefined;
  const resolved = await realpath(absolute).catch(() => undefined);
  if (!resolved || !resolved.startsWith(`${repository}${path.sep}`)) {
    return undefined;
  }
  return info.size;
}

async function readRegularTextFile(absolutePath: string): Promise<string> {
  const info = await stat(absolutePath);
  if (!info.isFile()) throw new Error("path is not a regular file");
  if (info.size > MAX_FILE_BYTES)
    throw new Error(`file exceeds ${MAX_FILE_BYTES} bytes`);
  const data = await readFile(absolutePath);
  if (data.includes(0)) throw new Error("binary file is not reviewable");
  return data.toString("utf8");
}

export function normalizeRepositoryPath(value: string): string {
  if (!value || path.isAbsolute(value) || value.includes("\0")) {
    throw new CliError(`unsafe repository path: ${value}`);
  }
  const normalized = path.posix.normalize(value.replaceAll("\\", "/"));
  if (
    normalized === "." ||
    normalized === ".." ||
    normalized.startsWith("../") ||
    normalized === ".git" ||
    normalized.startsWith(".git/")
  ) {
    throw new CliError(`unsafe repository path: ${value}`);
  }
  return normalized.replace(/^\.\//u, "");
}

export function sensitivePathReason(value: string): string | undefined {
  return sensitiveInputPathReason(normalizeRepositoryPath(value));
}

function normalizeRepositoryPrefix(value: string): string {
  const normalized = normalizeRepositoryPath(value);
  return normalized.endsWith("/") ? normalized : `${normalized}/`;
}

export function safeAbsolutePath(
  repository: string,
  relativePath: string,
): string {
  const normalized = normalizeRepositoryPath(relativePath);
  const absolute = path.resolve(repository, normalized);
  const prefix = `${repository}${path.sep}`;
  if (!absolute.startsWith(prefix))
    throw new CliError(`path escapes repository: ${relativePath}`);
  return absolute;
}

function fullFilePatch(relativePath: string, content: string): string {
  const lines = content.split("\n");
  const body = lines.map((line) => `+${line}`).join("\n");
  return [
    `diff --git a/${relativePath} b/${relativePath}`,
    "new file mode 100644",
    "--- /dev/null",
    `+++ b/${relativePath}`,
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

function isBinary(content: string | undefined): boolean {
  return content?.includes("\0") ?? false;
}

function splitNul(buffer: Buffer): string[] {
  return buffer.toString("utf8").split("\0").filter(Boolean);
}

function hash(value: unknown): string {
  return createHash("sha256").update(JSON.stringify(value)).digest("hex");
}

async function git(
  repository: string,
  args: string[],
  signal?: AbortSignal,
  allowedExitCodes: number[] = [0],
  maxOutputBytes?: number,
) {
  return await runCommand(
    "git",
    [
      "--literal-pathspecs",
      "-c",
      "core.fsmonitor=false",
      "-c",
      "core.hooksPath=/dev/null",
      "-c",
      "diff.external=",
      ...args,
    ],
    {
      cwd: repository,
      ...(signal ? { signal } : {}),
      allowedExitCodes,
      ...(maxOutputBytes ? { maxOutputBytes } : {}),
    },
  );
}
