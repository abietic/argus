import { normalizeRepositoryPath, readRepositoryFile } from "./git.js";
import { isFrozenTarget, splitAddressableSourceLines } from "./target.js";
import type { EvidenceRef, SourceAnchor, TargetSnapshot } from "./types.js";

export async function validateEvidenceRefs(
  target: TargetSnapshot,
  evidence: EvidenceRef[],
  signal: AbortSignal,
): Promise<EvidenceRef[]> {
  const validated: EvidenceRef[] = [];
  const seen = new Set<string>();
  for (const item of evidence) {
    const anchor = normalizeAnchorForTarget(target, item.anchor);
    if (anchor.endLine - anchor.startLine + 1 > 20) {
      throw new Error("evidence range exceeds 20 lines");
    }
    const view = anchor.side === "old" ? "base" : "target";
    const content = await readRepositoryFile(target, anchor.path, view, signal);
    const lines = splitAddressableSourceLines(content);
    if (anchor.endLine > lines.length) {
      throw new Error(`evidence line ${anchor.endLine} exceeds ${anchor.path}`);
    }
    const source = normalizeEvidenceText(
      lines.slice(anchor.startLine - 1, anchor.endLine).join("\n"),
    );
    const excerpt = normalizeEvidenceText(item.excerpt);
    if (excerpt.replace(/\s/gu, "").length < 8) {
      throw new Error(
        "evidence excerpt must contain at least 8 non-whitespace characters",
      );
    }
    if (excerpt !== source) {
      throw new Error(
        `evidence excerpt must equal the cited source range ${anchor.path}:${anchor.startLine}-${anchor.endLine}`,
      );
    }
    const dedupKey = JSON.stringify([anchor, excerpt]);
    if (seen.has(dedupKey)) continue;
    seen.add(dedupKey);
    validated.push({ ...item, anchor, excerpt });
  }
  return validated;
}

export function normalizeAnchorForTarget(
  target: TargetSnapshot,
  anchor: SourceAnchor,
): SourceAnchor {
  const normalized = normalizeAnchor(anchor);
  if (!isFrozenTarget(target)) return normalized;
  if (normalized.side === "old") {
    throw new Error("old-side anchors are unavailable in frozen inline input");
  }
  if (normalized.side !== "new" && normalized.side !== "file") {
    throw new Error(`unsupported frozen anchor side ${normalized.side}`);
  }
  // Frozen input carries exactly one target-side file body. Canonicalize the
  // provider-facing aliases before fingerprinting, checkpointing, evidence
  // validation, and host admission so retries cannot change identity merely
  // by choosing "new" versus "file" for the same bytes.
  const side = target.kind === "commit_diff" ? "new" : "file";
  return { ...normalized, side };
}

function normalizeEvidenceText(value: string): string {
  return normalizeLineEndings(value).trim();
}

function normalizeLineEndings(value: string): string {
  return value.replace(/\r\n?/gu, "\n");
}

function normalizeAnchor(anchor: SourceAnchor): SourceAnchor {
  const path = normalizeRepositoryPath(anchor.path);
  if (
    !Number.isInteger(anchor.startLine) ||
    !Number.isInteger(anchor.endLine)
  ) {
    throw new Error("anchor lines must be integers");
  }
  if (anchor.startLine < 1 || anchor.endLine < anchor.startLine) {
    throw new Error("anchor line range is invalid");
  }
  return { ...anchor, path };
}
