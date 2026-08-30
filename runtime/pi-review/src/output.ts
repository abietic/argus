import type { ReviewReport, Severity } from "./types.js";

export type FailOn = Severity | "none";

export function renderText(report: ReviewReport, verbose: boolean): string {
  const lines = [
    `Argus Pi Review — ${report.status}`,
    `Target: ${report.target.kind} (${report.target.files.length} files, ${report.coverage.groupsReviewed}/${report.coverage.groupsTotal} groups fully reviewed, ${report.coverage.reviewTasksSucceeded}/${report.coverage.reviewTasksTotal} review tasks)`,
    `Model: ${report.provider}/${report.model}`,
    `Verification: ${report.coverage.verificationTasksSucceeded}/${report.coverage.verificationTasksTotal} tasks${report.coverage.verificationEnabled ? "" : " (disabled)"}`,
    `Result: ${report.summary.confirmed} confirmed, ${report.summary.rejected} rejected, ${report.summary.inconclusive} inconclusive`,
  ];
  if (report.coverage.staleTarget) {
    lines.push(
      "WARNING: target changed while review was running; findings refer to the captured snapshot.",
    );
  }
  if (report.findings.length === 0) {
    lines.push(
      "",
      report.status === "complete"
        ? "No independently confirmed defects found."
        : "No defect was confirmed in the completed portion; incomplete coverage means the target is not proven clean.",
    );
  } else {
    lines.push("", "Confirmed findings:");
    for (const finding of report.findings) {
      lines.push(
        "",
        `[${finding.severity.toUpperCase()}] ${finding.title}`,
        `${finding.anchor.path}:${finding.anchor.startLine}-${finding.anchor.endLine} (${finding.anchor.side})`,
        finding.description,
        `Impact: ${finding.impact}`,
        `Evidence: ${finding.evidence
          .map(
            (item) =>
              `${item.anchor.path}:${item.anchor.startLine}-${item.anchor.endLine} ${item.statement}`,
          )
          .join("; ")}`,
        `Verification: ${finding.verification.explanation}`,
      );
      if (finding.suggestion) lines.push(`Suggestion: ${finding.suggestion}`);
    }
  }
  if (report.coverage.skipped.length > 0) {
    lines.push("", `Skipped targets: ${report.coverage.skipped.length}`);
    for (const skipped of report.coverage.skipped)
      lines.push(`- ${skipped.path}: ${skipped.reason}`);
  }
  if (report.coverage.contextGaps.length > 0) {
    lines.push("", `Context gaps: ${report.coverage.contextGaps.length}`);
    if (verbose) {
      for (const gap of report.coverage.contextGaps) lines.push(`- ${gap}`);
    }
  }
  if (report.coverage.failures.length > 0) {
    lines.push("", `Partial failures: ${report.coverage.failures.length}`);
    for (const failure of report.coverage.failures) {
      lines.push(
        `- ${failure.groupId}/${failure.phase}${failure.skillId ? `/${failure.skillId}` : ""}: ${failure.error}`,
      );
    }
  }
  if (verbose && report.candidates.length > report.findings.length) {
    lines.push("", "Non-confirmed candidates:");
    for (const candidate of report.candidates.filter(
      (item) => item.verification?.verdict !== "confirmed",
    )) {
      lines.push(
        `- ${candidate.id} ${candidate.anchor.path}:${candidate.anchor.startLine} ${candidate.verification?.verdict ?? "unverified"}: ${candidate.title}`,
      );
    }
  }
  return `${lines.join("\n")}\n`;
}

export function shouldFail(report: ReviewReport, threshold: FailOn): boolean {
  if (threshold === "none") return false;
  const rank = { critical: 0, high: 1, medium: 2, low: 3 } as const;
  return report.findings.some(
    (finding) => rank[finding.severity] <= rank[threshold],
  );
}
