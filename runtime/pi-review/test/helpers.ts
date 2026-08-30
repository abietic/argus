import { execFile } from "node:child_process";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

export async function createRepository(): Promise<string> {
  const repository = await mkdtemp(
    path.join(tmpdir(), "argus-pi-review-test-"),
  );
  await runGit(repository, ["init", "--quiet"]);
  await runGit(repository, ["config", "user.name", "Argus Test"]);
  await runGit(repository, ["config", "user.email", "argus@example.invalid"]);
  return repository;
}

export async function commitFile(
  repository: string,
  relativePath: string,
  content: string,
  message = "fixture",
): Promise<string> {
  await writeFixture(repository, relativePath, content);
  await runGit(repository, ["add", "--", relativePath]);
  await runGit(repository, ["commit", "--quiet", "-m", message]);
  return await revParse(repository, "HEAD");
}

export async function writeFixture(
  repository: string,
  relativePath: string,
  content: string,
): Promise<void> {
  const absolute = path.join(repository, relativePath);
  const { mkdir } = await import("node:fs/promises");
  await mkdir(path.dirname(absolute), { recursive: true });
  await writeFile(absolute, content, "utf8");
}

export async function revParse(
  repository: string,
  revision: string,
): Promise<string> {
  const result = await runGit(repository, ["rev-parse", revision]);
  return result.trim();
}

export async function runGit(
  repository: string,
  args: string[],
): Promise<string> {
  const result = await execFileAsync("git", args, { cwd: repository });
  return result.stdout;
}
