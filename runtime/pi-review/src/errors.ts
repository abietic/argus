export class CliError extends Error {
  constructor(
    message: string,
    readonly exitCode = 2,
  ) {
    super(message);
    this.name = "CliError";
  }
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export type AgentTaskFailure = {
  status: "timeout" | "aborted" | "failed";
  code:
    | "timeout"
    | "aborted"
    | "provider_error"
    | "structured_output_missing"
    | "unknown";
};

// Keep task evidence and the worker terminal envelope on one bounded failure
// taxonomy. This classifier never returns the raw message; callers decide
// whether a plan-authorized code may cross their own trust boundary.
export function classifyAgentTaskFailure(
  error: unknown,
  outerSignal?: AbortSignal,
): AgentTaskFailure {
  const message = errorMessage(error).toLowerCase();
  if (
    (error instanceof DOMException && error.name === "TimeoutError") ||
    message.includes("aborted due to timeout") ||
    message.includes("timed out")
  ) {
    return { status: "timeout", code: "timeout" };
  }
  if (
    outerSignal?.aborted ||
    (error instanceof DOMException && error.name === "AbortError")
  ) {
    return { status: "aborted", code: "aborted" };
  }
  if (
    message.includes("without calling") ||
    message.includes("structured result")
  ) {
    return { status: "failed", code: "structured_output_missing" };
  }
  if (
    message.includes("provider") ||
    message.includes("api") ||
    message.includes("pi agent") ||
    /\b(?:400|401|403|429|5\d\d)\b/u.test(message)
  ) {
    return { status: "failed", code: "provider_error" };
  }
  return { status: "failed", code: "unknown" };
}
