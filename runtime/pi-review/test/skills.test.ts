import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import { loadFrozenBuiltinSkills, loadSkills } from "../src/skills.js";

test("default builtin skills expose six orthogonal versioned dimensions", async () => {
  const skills = await loadSkills([]);
  assert.deepEqual(
    skills.map(({ id, revision }) => ({ id, revision })),
    [
      { id: "concurrency-data", revision: "builtin-v2" },
      { id: "correctness", revision: "builtin-v2" },
      { id: "error-contract", revision: "builtin-v1" },
      { id: "resource-lifecycle", revision: "builtin-v1" },
      { id: "security-contract", revision: "builtin-v1" },
      { id: "transaction-state", revision: "builtin-v1" },
    ],
  );
  const refs = skills.map((skill) => ({
    id: skill.id,
    revision: skill.revision,
    sha256: createHash("sha256").update(skill.prompt).digest("hex"),
  }));
  assert.deepEqual(
    (await loadFrozenBuiltinSkills(refs)).map((skill) => skill.id),
    skills.map((skill) => skill.id),
  );
});

test("frozen builtin loading rejects revision substitution", async () => {
  const [skill] = await loadSkills(["error-contract"]);
  assert.ok(skill);
  await assert.rejects(
    loadFrozenBuiltinSkills([
      {
        id: skill.id,
        revision: "builtin-v2",
        sha256: createHash("sha256").update(skill.prompt).digest("hex"),
      },
    ]),
    /does not match loaded content/u,
  );
});
