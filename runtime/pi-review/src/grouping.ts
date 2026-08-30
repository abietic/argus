import { createHash } from "node:crypto";
import path from "node:path";

import type { ChangeGroup, TargetFile, TargetSnapshot } from "./types.js";

const MAX_GROUP_FILES = 8;

export function partitionTarget(
  target: TargetSnapshot,
  maxGroupBytes: number,
): ChangeGroup[] {
  const buckets = new Map<string, TargetFile[]>();
  for (const file of target.files) {
    const key = groupingKey(file.path);
    const bucket = buckets.get(key) ?? [];
    bucket.push(file);
    buckets.set(key, bucket);
  }

  const groups: ChangeGroup[] = [];
  for (const [key, files] of [...buckets.entries()].sort(([left], [right]) =>
    left.localeCompare(right),
  )) {
    let current: TargetFile[] = [];
    let currentBytes = 0;
    const flush = (): void => {
      if (current.length === 0) return;
      groups.push(createGroup(key, groups.length, current));
      current = [];
      currentBytes = 0;
    };
    for (const file of files.sort((left, right) =>
      left.path.localeCompare(right.path),
    )) {
      const size = Buffer.byteLength(file.patch);
      if (
        current.length > 0 &&
        (current.length >= MAX_GROUP_FILES ||
          currentBytes + size > maxGroupBytes)
      ) {
        flush();
      }
      current.push(file);
      currentBytes += size;
    }
    flush();
  }
  return groups;
}

function groupingKey(filePath: string): string {
  const directory = path.posix.dirname(filePath);
  const extension = path.posix.extname(filePath).toLowerCase() || "noext";
  return `${directory === "." ? "root" : directory}:${extension}`;
}

function createGroup(
  key: string,
  index: number,
  files: TargetFile[],
): ChangeGroup {
  const stable = createHash("sha256")
    .update(
      JSON.stringify([key, files.map((file) => [file.path, file.digest])]),
    )
    .digest("hex")
    .slice(0, 12);
  return {
    id: `group-${String(index + 1).padStart(3, "0")}-${stable}`,
    key,
    files: [...files],
    patch: files.map((file) => file.patch).join("\n\n"),
    changedLines: files.reduce((total, file) => total + file.changedLines, 0),
  };
}
