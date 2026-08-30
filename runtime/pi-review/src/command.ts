import { spawn } from "node:child_process";

const DEFAULT_MAX_OUTPUT_BYTES = 16 * 1024 * 1024;

export interface CommandResult {
  stdout: Buffer;
  stderr: Buffer;
  exitCode: number;
}

export interface CommandOptions {
  cwd: string;
  signal?: AbortSignal;
  allowedExitCodes?: number[];
  maxOutputBytes?: number;
}

export function sanitizedChildEnv(): NodeJS.ProcessEnv {
  const environment: NodeJS.ProcessEnv = {
    PATH: process.env.PATH ?? "/usr/bin:/bin",
    LANG: "C.UTF-8",
    LC_ALL: "C.UTF-8",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_OPTIONAL_LOCKS: "0",
    GIT_TERMINAL_PROMPT: "0",
  };
  if (process.env.TMPDIR) {
    environment.TMPDIR = process.env.TMPDIR;
  }
  return environment;
}

export async function runCommand(
  command: string,
  args: string[],
  options: CommandOptions,
): Promise<CommandResult> {
  const allowedExitCodes = options.allowedExitCodes ?? [0];
  const maxOutputBytes = options.maxOutputBytes ?? DEFAULT_MAX_OUTPUT_BYTES;
  options.signal?.throwIfAborted();

  return await new Promise<CommandResult>((resolve, reject) => {
    const child = spawn(command, args, {
      cwd: options.cwd,
      env: sanitizedChildEnv(),
      stdio: ["ignore", "pipe", "pipe"],
    });
    const stdout: Buffer[] = [];
    const stderr: Buffer[] = [];
    let totalBytes = 0;
    let settled = false;
    let killTimer: NodeJS.Timeout | undefined;

    const finishWithError = (error: Error): void => {
      if (settled) return;
      settled = true;
      child.kill("SIGKILL");
      reject(error);
    };

    const append = (target: Buffer[], chunk: Buffer): void => {
      totalBytes += chunk.length;
      if (totalBytes > maxOutputBytes) {
        finishWithError(
          new Error(`${command} output exceeded ${maxOutputBytes} bytes`),
        );
        return;
      }
      target.push(chunk);
    };

    child.stdout.on("data", (chunk: Buffer) => append(stdout, chunk));
    child.stderr.on("data", (chunk: Buffer) => append(stderr, chunk));
    child.once("error", finishWithError);

    const abort = (): void => {
      if (settled) return;
      settled = true;
      child.kill("SIGTERM");
      killTimer = setTimeout(() => child.kill("SIGKILL"), 1000);
      killTimer.unref();
      reject(options.signal?.reason ?? new Error("command aborted"));
    };
    options.signal?.addEventListener("abort", abort, { once: true });

    child.once("close", (code, closeSignal) => {
      options.signal?.removeEventListener("abort", abort);
      if (killTimer) clearTimeout(killTimer);
      if (settled) return;
      settled = true;
      if (options.signal?.aborted) {
        reject(options.signal.reason ?? new Error("command aborted"));
        return;
      }
      const exitCode = code ?? 128;
      const result = {
        stdout: Buffer.concat(stdout),
        stderr: Buffer.concat(stderr),
        exitCode,
      };
      if (!allowedExitCodes.includes(exitCode)) {
        const detail = result.stderr.toString("utf8").trim();
        reject(
          new Error(
            `${command} exited with ${exitCode}${closeSignal ? ` (${closeSignal})` : ""}${detail ? `: ${detail}` : ""}`,
          ),
        );
        return;
      }
      resolve(result);
    });
  });
}
