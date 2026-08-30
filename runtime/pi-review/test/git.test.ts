import assert from "node:assert/strict";
import { mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { sanitizedChildEnv } from "../src/command.js";
import {
  captureTarget,
  isTargetStale,
  normalizeRepositoryPath,
  readRepositoryFile,
} from "../src/git.js";
import type { ReviewOptions } from "../src/types.js";
import {
  commitFile,
  createRepository,
  revParse,
  runGit,
  writeFixture,
} from "./helpers.js";

const defaults = {
  model: "fake",
  providerProfile: "anthropic-official",
  normalizationRevision: "v2",
  concurrency: 2,
  skills: [],
  knowledgeFiles: [],
  verify: true,
  maxToolCalls: 10,
  maxGroupBytes: 64 * 1024,
  maxTargetBytes: 4 * 1024 * 1024,
  maxFiles: 32,
  maxGroups: 8,
  maxCandidates: 32,
  maxModelCalls: 64,
  maxOutputTokens: 8192,
  timeoutMs: 10_000,
  verbose: false,
} satisfies Omit<ReviewOptions, "repository" | "target">;

test("working target captures tracked and untracked files and detects staleness", async () => {
  const repository = await createRepository();
  try {
    await commitFile(repository, "src/value.ts", "export const value = 1;\n");
    await writeFixture(repository, "src/value.ts", "export const value = 2;\n");
    await writeFixture(
      repository,
      "src/new.ts",
      "export const added = true;\n",
    );
    const target = await captureTarget({
      ...defaults,
      repository,
      target: { kind: "working_changes", paths: [] },
    });
    assert.deepEqual(
      target.files.map((file) => [file.path, file.status]),
      [
        ["src/new.ts", "added"],
        ["src/value.ts", "modified"],
      ],
    );
    const options = {
      ...defaults,
      repository,
      target: { kind: "working_changes" as const, paths: [] },
    };
    assert.equal(await isTargetStale(target, options), false);
    await writeFixture(repository, "src/value.ts", "export const value = 3;\n");
    assert.equal(await isTargetStale(target, options), true);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("commit target resolves immutable object IDs", async () => {
  const repository = await createRepository();
  try {
    const base = await commitFile(
      repository,
      "main.go",
      "package main\n\nconst value = 1\n",
    );
    await writeFixture(
      repository,
      "main.go",
      "package main\n\nconst value = 2\n",
    );
    await runGit(repository, ["add", "main.go"]);
    await runGit(repository, ["commit", "--quiet", "-m", "change"]);
    const head = await revParse(repository, "HEAD");
    const target = await captureTarget({
      ...defaults,
      repository,
      target: { kind: "commit_diff", base, head, paths: [] },
    });
    assert.equal(target.baseOid, base);
    assert.equal(target.headOid, head);
    assert.equal(target.files[0]?.status, "modified");
    assert.match(target.files[0]?.patch ?? "", /const value = 2/u);
    assert.equal(
      await isTargetStale(target, {
        ...defaults,
        repository,
        target: { kind: "commit_diff", base, head, paths: [] },
      }),
      false,
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("repository paths and child process environment fail closed", () => {
  assert.throws(
    () => normalizeRepositoryPath("../secret"),
    /unsafe repository path/u,
  );
  assert.throws(
    () => normalizeRepositoryPath(".git/config"),
    /unsafe repository path/u,
  );
  const previous = process.env.ANTHROPIC_API_KEY;
  process.env.ANTHROPIC_API_KEY = "must-not-leak";
  try {
    const environment = sanitizedChildEnv();
    assert.equal(environment.ANTHROPIC_API_KEY, undefined);
    assert.equal(environment.ANTHROPIC_AUTH_TOKEN, undefined);
  } finally {
    if (previous === undefined) delete process.env.ANTHROPIC_API_KEY;
    else process.env.ANTHROPIC_API_KEY = previous;
  }
});

test("unborn repository captures staged files", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/staged.ts",
      "export const staged = true;\n",
    );
    await runGit(repository, ["add", "src/staged.ts"]);
    const target = await captureTarget({
      ...defaults,
      repository,
      target: { kind: "working_changes", paths: [] },
    });
    assert.deepEqual(
      target.files.map((file) => file.path),
      ["src/staged.ts"],
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("staleness detects newly added target paths", async () => {
  const repository = await createRepository();
  try {
    await commitFile(repository, "src/a.ts", "export const a = 1;\n");
    await writeFixture(repository, "src/a.ts", "export const a = 2;\n");
    const options = {
      ...defaults,
      repository,
      target: { kind: "working_changes" as const, paths: [] },
    };
    const target = await captureTarget(options);
    await writeFixture(repository, "src/b.ts", "export const b = 1;\n");
    assert.equal(await isTargetStale(target, options), true);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("staleness includes context files frozen by read_file", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/target.ts",
      "export const target = helper();\n",
    );
    await writeFixture(
      repository,
      "src/helper.ts",
      "export const helper = () => 1;\n",
    );
    const options = {
      ...defaults,
      repository,
      target: { kind: "files" as const, paths: ["src/target.ts"] },
    };
    const target = await captureTarget(options);
    assert.match(
      await readRepositoryFile(target, "src/helper.ts", "target"),
      /helper/u,
    );
    await writeFixture(
      repository,
      "src/helper.ts",
      "export const helper = () => 2;\n",
    );
    assert.equal(await isTargetStale(target, options), true);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("sensitive and oversized files are explicit coverage gaps", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(repository, ".env", "ANTHROPIC_API_KEY=do-not-send\n");
    await writeFixture(
      repository,
      "src/small.ts",
      "export const safe = true;\n",
    );
    await writeFixture(repository, "src/large.ts", "x".repeat(1024 * 1024 + 1));
    const target = await captureTarget({
      ...defaults,
      repository,
      target: { kind: "working_changes", paths: [] },
    });
    assert.deepEqual(
      target.files.map((file) => file.path),
      ["src/small.ts"],
    );
    assert.deepEqual(
      target.skipped.map((item) => [item.path, item.reason]),
      [
        [".env", "sensitive_environment_file"],
        ["src/large.ts", "file_too_large"],
      ],
    );
    await assert.rejects(
      readRepositoryFile(target, ".env", "target"),
      /sensitive repository path/u,
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("parent-directory symlink cannot escape repository", async () => {
  const repository = await createRepository();
  const outside = await mkdtemp(path.join(tmpdir(), "argus-outside-"));
  try {
    await writeFile(
      path.join(outside, "secret.txt"),
      "outside secret\n",
      "utf8",
    );
    await symlink(outside, path.join(repository, "escape"), "dir");
    await assert.rejects(
      captureTarget({
        ...defaults,
        repository,
        target: { kind: "files", paths: ["escape/secret.txt"] },
      }),
      /escapes repository through symlink/u,
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
    await rm(outside, { recursive: true, force: true });
  }
});

test("single-file patch cannot exceed group budget", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(repository, "small.ts", "x\n");
    await writeFixture(
      repository,
      "large.ts",
      `${"large change\n".repeat(100)}`,
    );
    const target = await captureTarget({
      ...defaults,
      maxGroupBytes: 512,
      repository,
      target: { kind: "working_changes", paths: [] },
    });
    assert.deepEqual(
      target.files.map((file) => file.path),
      ["small.ts"],
    );
    assert.deepEqual(target.skipped, [
      { path: "large.ts", reason: "group_patch_budget_exceeded" },
    ]);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("file-count admission runs before target content capture", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(repository, "src/a.ts", "export const a = 1;\n");
    await writeFixture(repository, "src/huge.ts", "x".repeat(1024 * 1024 + 1));
    await assert.rejects(
      captureTarget({
        ...defaults,
        repository,
        maxFiles: 1,
        target: { kind: "files", paths: ["src/a.ts", "src/huge.ts"] },
      }),
      /target has at least 2 reviewable paths; --max-files is 1/u,
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("target byte admission counts UTF-8 bytes before patch construction", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(repository, "src/unicode.ts", "界".repeat(10));
    await assert.rejects(
      captureTarget({
        ...defaults,
        repository,
        maxTargetBytes: 20,
        target: { kind: "files", paths: ["src/unicode.ts"] },
      }),
      /target source blobs exceed --max-target-bytes \(20\)/u,
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});
