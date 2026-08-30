import assert from "node:assert/strict";
import test from "node:test";

import { partitionTarget } from "../src/grouping.js";
import type { TargetFile, TargetSnapshot } from "../src/types.js";

function file(path: string, digest: string): TargetFile {
  return {
    path,
    status: "modified",
    patch: `diff --git a/${path} b/${path}\n+change`,
    targetContent: "change\n",
    digest,
    changedLines: 1,
  };
}

test("partitioning is deterministic across input completion order", () => {
  const files = [
    file("src/a.ts", "a"),
    file("src/b.ts", "b"),
    file("pkg/c.go", "c"),
  ];
  const target = (ordered: TargetFile[]): TargetSnapshot => ({
    kind: "files",
    repository: "/tmp/repo",
    digest: "target",
    capturedAt: "2026-01-01T00:00:00Z",
    files: ordered,
    skipped: [],
  });
  const first = partitionTarget(target(files), 65_536);
  const second = partitionTarget(target([...files].reverse()), 65_536);
  assert.deepEqual(first, second);
  assert.equal(first.length, 2);
});
