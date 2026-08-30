#!/usr/bin/env node

import { pathToFileURL } from "node:url";

import { PiAgentRuntime, type PiProviderProfile } from "./agent.js";
import { CliError, errorMessage } from "./errors.js";
import { captureTarget, resolveRepository } from "./git.js";
import { partitionTarget } from "./grouping.js";
import { renderText, shouldFail, type FailOn } from "./output.js";
import { loadSkills } from "./skills.js";
import type { ReviewOptions, Severity } from "./types.js";
import { runReviewWorkflow, validateReviewAdmission } from "./workflow.js";

const DEFAULT_MODEL = "claude-sonnet-4-6";
const HELP = `Argus Pi Agent Code Review CLI

Usage:
  argus-review changes [options] [-- <path>...]
  argus-review commits --base <revision> --head <revision> [options] [-- <path>...]
  argus-review files [--revision <revision>] [options] -- <file>...

Targets:
  changes  Review HEAD (or --base) against the current working tree, including untracked files.
  commits  Review an exact base-to-head commit diff.
  files    Review complete, explicitly named files from the working tree or --revision.

Options:
  --repo <path>               Git repository (default: current directory)
  --base <revision>           Base revision for changes/commits
  --head <revision>           Head revision for commits
  --revision <revision>       Revision for files mode
  --path <path>               Target path/filter; repeatable
  --skill <id-or-file>        Builtin ID or explicit Markdown file; repeatable
  --knowledge <file>          Explicit knowledge Markdown; repeatable
  --provider-profile <name>   anthropic-official or deepseek-anthropic-env
  --model <id>                Model ID (ARGUS_MODEL; DeepSeek also accepts ANTHROPIC_MODEL)
  --concurrency <n>           Global concurrent Agent calls (default: 4)
  --timeout-seconds <n>       Per-Agent deadline (default: 180)
  --max-tool-calls <n>        Read-only tool calls per Agent (default: 24)
  --max-group-bytes <n>       Deterministic group patch budget (default: 65536)
  --max-target-bytes <n>      Total base+target source blob budget (default: 4194304)
  --max-files <n>             Maximum admitted target files (default: 32)
  --max-groups <n>            Maximum admitted change groups (default: 8)
  --max-candidates <n>        Maximum normalized candidates retained/verified (default: 32)
  --max-model-calls <n>       Hard total provider/model turn budget (default: 96)
  --max-output-tokens <n>     Output token cap for each model turn (default: 8192)
  --no-verify                 Keep candidates but skip independent verification
  --allow-partial             Exit 0 for partial coverage when --fail-on does not match
  --format <text|json>        Output format (default: text)
  --fail-on <level|none>      Exit 1 at critical/high/medium/low threshold
  --plan                      Capture and show groups without calling a model
  --verbose                   Show tool activity and non-confirmed candidates
  -h, --help                  Show this help

Authentication:
  anthropic-official: set ANTHROPIC_API_KEY.
  deepseek-anthropic-env: set ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY and ANTHROPIC_MODEL.
  Credentials are never accepted as CLI arguments or printed.
`;

interface CliOptions {
  review: ReviewOptions;
  format: "text" | "json";
  failOn: FailOn;
  plan: boolean;
  allowPartial: boolean;
}

export async function main(
  argv = process.argv.slice(2),
  environment: NodeJS.ProcessEnv = process.env,
): Promise<number> {
  if (argv.includes("--help") || argv.includes("-h")) {
    process.stdout.write(HELP);
    return 0;
  }
  let parsed: CliOptions;
  try {
    parsed = parseArguments(argv, environment);
  } catch (error) {
    const code = error instanceof CliError ? error.exitCode : 2;
    process.stderr.write(`argus-review: ${errorMessage(error)}\n`);
    return code;
  }
  const controller = new AbortController();
  const onInterrupt = (): void =>
    controller.abort(new CliError("interrupted", 130));
  process.once("SIGINT", onInterrupt);
  process.once("SIGTERM", onInterrupt);
  try {
    parsed.review.repository = await resolveRepository(
      parsed.review.repository,
    );
    if (parsed.plan) {
      const target = await captureTarget(parsed.review, controller.signal);
      const groups = partitionTarget(target, parsed.review.maxGroupBytes);
      const skills = await loadSkills(parsed.review.skills);
      let admission: { accepted: true } | { accepted: false; reason: string };
      try {
        validateReviewAdmission(parsed.review, target, groups, skills.length);
        admission = { accepted: true };
      } catch (error) {
        admission = { accepted: false, reason: errorMessage(error) };
      }
      const plan = {
        schemaVersion: "argus.pi-review.plan.v0",
        providerProfile: parsed.review.providerProfile,
        model: parsed.review.model,
        admission,
        estimatedAgentTasks: {
          context: groups.length,
          review: groups.length * skills.length,
          verificationMaximum: parsed.review.maxCandidates,
        },
        estimatedProviderModelTurns: {
          perTaskMaximum: parsed.review.maxToolCalls + 1,
          unconstrainedMaximum:
            (groups.length * (1 + skills.length) +
              parsed.review.maxCandidates) *
            (parsed.review.maxToolCalls + 1),
          hardLimit: parsed.review.maxModelCalls,
        },
        target: {
          kind: target.kind,
          repository: target.repository,
          digest: target.digest,
          files: target.files.map((file) => ({
            path: file.path,
            status: file.status,
            changedLines: file.changedLines,
          })),
          skipped: target.skipped,
        },
        groups: groups.map((group) => ({
          id: group.id,
          key: group.key,
          files: group.files.map((file) => file.path),
          changedLines: group.changedLines,
          patchBytes: Buffer.byteLength(group.patch),
        })),
      };
      process.stdout.write(
        parsed.format === "json"
          ? `${JSON.stringify(plan, null, 2)}\n`
          : renderPlan(plan),
      );
      return admission.accepted ? 0 : 2;
    }

    const runtime = new PiAgentRuntime({
      model: parsed.review.model,
      profile: resolveProviderProfile(
        parsed.review.providerProfile,
        environment,
      ),
      maxToolCalls: parsed.review.maxToolCalls,
      maxModelCalls: parsed.review.maxModelCalls,
      maxOutputTokens: parsed.review.maxOutputTokens,
      timeoutMs: parsed.review.timeoutMs,
      ...(parsed.review.verbose
        ? {
            verbose: (message: string) =>
              process.stderr.write(`[tool] ${message}\n`),
          }
        : {}),
    });
    const report = await runReviewWorkflow(
      parsed.review,
      runtime,
      controller.signal,
      (event) => process.stderr.write(`[${event.phase}] ${event.message}\n`),
    );
    process.stdout.write(
      parsed.format === "json"
        ? `${JSON.stringify(report, null, 2)}\n`
        : renderText(report, parsed.review.verbose),
    );
    if (report.status === "failed") return 2;
    if (report.status === "partial" && !parsed.allowPartial) return 2;
    return shouldFail(report, parsed.failOn) ? 1 : 0;
  } catch (error) {
    if (controller.signal.aborted) return 130;
    if (error instanceof CliError) {
      process.stderr.write(`argus-review: ${error.message}\n`);
      return error.exitCode;
    }
    process.stderr.write(`argus-review: ${errorMessage(error)}\n`);
    return 2;
  } finally {
    process.off("SIGINT", onInterrupt);
    process.off("SIGTERM", onInterrupt);
  }
}

export function parseArguments(
  argv: string[],
  environment: NodeJS.ProcessEnv = process.env,
): CliOptions {
  if (argv.length === 0) throw new CliError(HELP.trimEnd());
  const command = argv[0];
  if (command !== "changes" && command !== "commits" && command !== "files") {
    throw new CliError(`unknown target ${command}\n\n${HELP}`);
  }
  const values = new Map<string, string[]>();
  const booleans = new Set<string>();
  const positional: string[] = [];
  const valueFlags = new Set([
    "--repo",
    "--base",
    "--head",
    "--revision",
    "--path",
    "--skill",
    "--knowledge",
    "--provider-profile",
    "--model",
    "--concurrency",
    "--timeout-seconds",
    "--max-tool-calls",
    "--max-group-bytes",
    "--max-target-bytes",
    "--max-files",
    "--max-groups",
    "--max-candidates",
    "--max-model-calls",
    "--max-output-tokens",
    "--format",
    "--fail-on",
  ]);
  const booleanFlags = new Set([
    "--no-verify",
    "--allow-partial",
    "--plan",
    "--verbose",
  ]);
  let afterSeparator = false;
  for (let index = 1; index < argv.length; index++) {
    const argument = argv[index] as string;
    if (afterSeparator) {
      positional.push(argument);
      continue;
    }
    if (argument === "--") {
      afterSeparator = true;
      continue;
    }
    if (booleanFlags.has(argument)) {
      booleans.add(argument);
      continue;
    }
    if (!valueFlags.has(argument))
      throw new CliError(`unknown option ${argument}`);
    const value = argv[++index];
    if (value === undefined || value.startsWith("--")) {
      throw new CliError(`${argument} requires a value`);
    }
    const existing = values.get(argument) ?? [];
    existing.push(value);
    values.set(argument, existing);
  }
  const one = (flag: string): string | undefined => {
    const found = values.get(flag) ?? [];
    if (
      found.length > 1 &&
      flag !== "--path" &&
      flag !== "--skill" &&
      flag !== "--knowledge"
    ) {
      throw new CliError(`${flag} may be specified only once`);
    }
    return found.at(-1);
  };
  const paths = [...(values.get("--path") ?? []), ...positional];
  const base = one("--base");
  const head = one("--head");
  const revision = one("--revision");
  let target: ReviewOptions["target"];
  if (command === "changes") {
    if (head || revision)
      throw new CliError("changes does not accept --head or --revision");
    target = { kind: "working_changes", paths, ...(base ? { base } : {}) };
  } else if (command === "commits") {
    if (!base || !head)
      throw new CliError("commits requires --base and --head");
    if (revision) throw new CliError("commits does not accept --revision");
    target = { kind: "commit_diff", base, head, paths };
  } else {
    if (base || head)
      throw new CliError("files does not accept --base or --head");
    if (paths.length === 0)
      throw new CliError("files requires at least one path after --");
    target = { kind: "files", paths, ...(revision ? { revision } : {}) };
  }
  const format = one("--format") ?? "text";
  if (format !== "text" && format !== "json")
    throw new CliError("--format must be text or json");
  const failOn = one("--fail-on") ?? "none";
  if (!["none", "critical", "high", "medium", "low"].includes(failOn)) {
    throw new CliError("--fail-on must be none, critical, high, medium or low");
  }
  const providerProfile =
    one("--provider-profile") ??
    environment.ARGUS_PROVIDER_PROFILE?.trim() ??
    "anthropic-official";
  if (
    providerProfile !== "anthropic-official" &&
    providerProfile !== "deepseek-anthropic-env"
  ) {
    throw new CliError(
      "--provider-profile must be anthropic-official or deepseek-anthropic-env",
    );
  }
  const model =
    one("--model") ??
    environment.ARGUS_MODEL?.trim() ??
    (providerProfile === "deepseek-anthropic-env"
      ? environment.ANTHROPIC_MODEL?.trim()
      : DEFAULT_MODEL);
  if (!model) {
    throw new CliError(
      "DeepSeek profile requires --model, ARGUS_MODEL or ANTHROPIC_MODEL",
    );
  }
  return {
    review: {
      repository: one("--repo") ?? process.cwd(),
      target,
      model,
      providerProfile,
      normalizationRevision: "v2",
      concurrency: positiveInteger(
        one("--concurrency") ?? "4",
        "--concurrency",
        16,
      ),
      skills: values.get("--skill") ?? [],
      knowledgeFiles: values.get("--knowledge") ?? [],
      verify: !booleans.has("--no-verify"),
      maxToolCalls: positiveInteger(
        one("--max-tool-calls") ?? "24",
        "--max-tool-calls",
        100,
      ),
      maxGroupBytes: positiveInteger(
        one("--max-group-bytes") ?? "65536",
        "--max-group-bytes",
        1024 * 1024,
      ),
      maxTargetBytes: positiveInteger(
        one("--max-target-bytes") ?? "4194304",
        "--max-target-bytes",
        64 * 1024 * 1024,
      ),
      maxFiles: positiveInteger(
        one("--max-files") ?? "32",
        "--max-files",
        1000,
      ),
      maxGroups: positiveInteger(
        one("--max-groups") ?? "8",
        "--max-groups",
        256,
      ),
      maxCandidates: positiveInteger(
        one("--max-candidates") ?? "32",
        "--max-candidates",
        1000,
      ),
      maxModelCalls: positiveInteger(
        one("--max-model-calls") ?? "96",
        "--max-model-calls",
        2000,
      ),
      maxOutputTokens: positiveInteger(
        one("--max-output-tokens") ?? "8192",
        "--max-output-tokens",
        32_768,
      ),
      timeoutMs:
        positiveInteger(
          one("--timeout-seconds") ?? "180",
          "--timeout-seconds",
          3600,
        ) * 1000,
      verbose: booleans.has("--verbose"),
    },
    format,
    failOn: failOn as FailOn,
    plan: booleans.has("--plan"),
    allowPartial: booleans.has("--allow-partial"),
  };
}

export function resolveProviderProfile(
  name: ReviewOptions["providerProfile"],
  environment: NodeJS.ProcessEnv = process.env,
): PiProviderProfile {
  const apiKey = environment.ANTHROPIC_API_KEY?.trim();
  if (!apiKey) throw new CliError("ANTHROPIC_API_KEY is required");
  if (name === "anthropic-official") {
    return { kind: name, apiKey };
  }
  const baseUrl = environment.ANTHROPIC_BASE_URL?.trim();
  if (!baseUrl) {
    throw new CliError(
      "ANTHROPIC_BASE_URL is required for deepseek-anthropic-env",
    );
  }
  return { kind: name, baseUrl, apiKey };
}

function positiveInteger(value: string, flag: string, maximum: number): number {
  if (!/^\d+$/u.test(value))
    throw new CliError(`${flag} must be a positive integer`);
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 1 || parsed > maximum) {
    throw new CliError(`${flag} must be between 1 and ${maximum}`);
  }
  return parsed;
}

function renderPlan(plan: {
  admission: { accepted: true } | { accepted: false; reason: string };
  estimatedAgentTasks: {
    context: number;
    review: number;
    verificationMaximum: number;
  };
  estimatedProviderModelTurns: {
    perTaskMaximum: number;
    unconstrainedMaximum: number;
    hardLimit: number;
  };
  target: {
    kind: string;
    files: Array<{ path: string; changedLines: number }>;
    skipped: unknown[];
  };
  groups: Array<{
    id: string;
    key: string;
    files: string[];
    changedLines: number;
    patchBytes: number;
  }>;
}): string {
  const lines = [
    `Argus review plan — ${plan.target.kind}`,
    `${plan.target.files.length} files in ${plan.groups.length} groups; ${plan.target.skipped.length} skipped`,
    `Estimated Agent tasks: ${plan.estimatedAgentTasks.context} context + ${plan.estimatedAgentTasks.review} review + up to ${plan.estimatedAgentTasks.verificationMaximum} verification`,
    `Provider/model turns: up to ${plan.estimatedProviderModelTurns.perTaskMaximum} per task, ${plan.estimatedProviderModelTurns.unconstrainedMaximum} unconstrained worst case; hard limit ${plan.estimatedProviderModelTurns.hardLimit}`,
    plan.admission.accepted
      ? "Admission: accepted"
      : `Admission: rejected — ${plan.admission.reason}`,
  ];
  for (const group of plan.groups) {
    lines.push(
      `- ${group.id} ${group.key}: ${group.files.length} files, ${group.changedLines} changed lines, ${group.patchBytes} bytes`,
      ...group.files.map((file) => `  - ${file}`),
    );
  }
  return `${lines.join("\n")}\n`;
}

const isEntrypoint =
  process.argv[1] !== undefined &&
  import.meta.url === pathToFileURL(process.argv[1]).href;
if (isEntrypoint) {
  process.exitCode = await main();
}
