import assert from "node:assert/strict";
import test from "node:test";

import { classifyAgentTaskFailure } from "../src/errors.js";

test("shared failure classifier preserves only stable provider and timeout codes", () => {
  assert.deepEqual(
    classifyAgentTaskFailure(new Error("provider API returned 503")),
    { status: "failed", code: "provider_error" },
  );
  assert.deepEqual(
    classifyAgentTaskFailure(new DOMException("timed out", "TimeoutError")),
    { status: "timeout", code: "timeout" },
  );
  assert.deepEqual(classifyAgentTaskFailure(new Error("unexpected failure")), {
    status: "failed",
    code: "unknown",
  });
});

test("shared failure classifier honors the outer cancellation fence", () => {
  const controller = new AbortController();
  controller.abort();
  assert.deepEqual(
    classifyAgentTaskFailure(new Error("operation stopped"), controller.signal),
    { status: "aborted", code: "aborted" },
  );
});
