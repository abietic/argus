import assert from "node:assert/strict";
import test from "node:test";

import { parseArguments, resolveProviderProfile } from "../src/cli.js";

test("CLI defaults to the pinned Anthropic model without accepting a key argument", () => {
  const parsed = parseArguments(
    ["changes", "--repo", ".", "--path", "src"],
    {},
  );
  assert.equal(parsed.review.model, "claude-sonnet-4-6");
  assert.equal(parsed.review.target.kind, "working_changes");
  assert.deepEqual(parsed.review.target.paths, ["src"]);
  assert.throws(
    () => parseArguments(["changes", "--api-key", "secret"]),
    /unknown option --api-key/u,
  );
});

test("CLI enforces mutually exclusive target contracts", () => {
  assert.throws(
    () => parseArguments(["commits", "--base", "HEAD"]),
    /requires --base and --head/u,
  );
  assert.throws(() => parseArguments(["files"]), /requires at least one path/u);
  const parsed = parseArguments([
    "commits",
    "--base",
    "HEAD~1",
    "--head",
    "HEAD",
    "--",
    "src/main.ts",
  ]);
  assert.deepEqual(parsed.review.target, {
    kind: "commit_diff",
    base: "HEAD~1",
    head: "HEAD",
    paths: ["src/main.ts"],
  });
});

test("DeepSeek profile resolves its model and credentials only from explicit fields", () => {
  const parsed = parseArguments(
    ["changes", "--provider-profile", "deepseek-anthropic-env"],
    { ANTHROPIC_MODEL: "deepseek-v4-pro[1m]" },
  );
  assert.equal(parsed.review.providerProfile, "deepseek-anthropic-env");
  assert.equal(parsed.review.model, "deepseek-v4-pro[1m]");
  assert.deepEqual(
    resolveProviderProfile("deepseek-anthropic-env", {
      ANTHROPIC_BASE_URL: "https://api.deepseek.com/anthropic",
      ANTHROPIC_API_KEY: "fixture-key",
      ANTHROPIC_AUTH_TOKEN: "must-not-be-selected",
    }),
    {
      kind: "deepseek-anthropic-env",
      baseUrl: "https://api.deepseek.com/anthropic",
      apiKey: "fixture-key",
    },
  );
  assert.throws(
    () =>
      resolveProviderProfile("deepseek-anthropic-env", {
        ANTHROPIC_API_KEY: "fixture-key",
      }),
    /ANTHROPIC_BASE_URL is required/u,
  );
});
