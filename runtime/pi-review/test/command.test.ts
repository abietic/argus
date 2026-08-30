import assert from "node:assert/strict";
import test from "node:test";

import { runCommand } from "../src/command.js";

test("subprocess cancellation rejects promptly even when SIGTERM is ignored", async () => {
  const started = Date.now();
  await assert.rejects(
    runCommand(
      process.execPath,
      [
        "-e",
        "process.on('SIGTERM',()=>{}); setInterval(()=>{},1000); process.stdout.write('ready')",
      ],
      {
        cwd: process.cwd(),
        signal: AbortSignal.timeout(40),
      },
    ),
    /timeout|aborted/iu,
  );
  assert.ok(Date.now() - started < 500, "abort should not wait for child exit");
});
