import assert from "node:assert/strict";
import { rm } from "node:fs/promises";
import test from "node:test";

import type { AgentRuntime } from "../src/agent.js";
import type {
  Candidate,
  ChangeGroup,
  ContextBundle,
  ReviewOptions,
  SkillDefinition,
  TargetSnapshot,
  VerificationResult,
} from "../src/types.js";
import {
  runReviewWorkflow,
  type WorkflowGroupCheckpoint,
} from "../src/workflow.js";
import { createRepository, writeFixture } from "./helpers.js";

class FakeRuntime implements AgentRuntime {
  readonly provider = "fake" as const;
  readonly providerProfile = "fake@1";
  readonly model = "fixture";
  reviewCalls = 0;
  verificationInputs: Candidate[] = [];

  getExecutionObservations() {
    return [];
  }

  async collectContext(
    _target: TargetSnapshot,
    group: ChangeGroup,
  ): Promise<ContextBundle> {
    return {
      summary: "The changed function dereferences an optional value.",
      relevantFiles: group.files.map((file) => ({
        path: file.path,
        reason: "changed",
      })),
      facts: [],
      gaps: [],
    };
  }

  async review(
    _target: TargetSnapshot,
    group: ChangeGroup,
    _context: ContextBundle,
    skill: SkillDefinition,
  ) {
    this.reviewCalls++;
    return {
      summary: "fixture review",
      candidates:
        skill.id === "correctness"
          ? [
              {
                category: "nil-dereference",
                severity: "high" as const,
                rawConfidencePPM: 900_000,
                title: "Optional value is dereferenced before validation",
                description: "A missing value reaches the property access.",
                impact: "The request crashes.",
                anchor: {
                  path: group.files[0]?.path ?? "src/handler.ts",
                  side: "file" as const,
                  startLine: 2,
                  endLine: 2,
                },
                evidence: [
                  {
                    statement:
                      "Line 2 accesses input.value without checking input.",
                    anchor: {
                      path: group.files[0]?.path ?? "src/handler.ts",
                      side: "file" as const,
                      startLine: 2,
                      endLine: 2,
                    },
                    excerpt: "return input.value;",
                  },
                ],
              },
            ]
          : [],
    };
  }

  async verify(
    _target: TargetSnapshot,
    _group: ChangeGroup,
    _context: ContextBundle,
    candidate: Candidate,
  ): Promise<VerificationResult> {
    this.verificationInputs.push(candidate);
    return {
      candidateId: candidate.id,
      verdict: "confirmed",
      reasonCode: "reachable_crash",
      explanation: "The unchecked access is reachable with undefined input.",
      evidence: [
        {
          statement: "The unchecked access is present on the target side.",
          anchor: candidate.anchor,
          excerpt: "return input.value;",
        },
      ],
    };
  }
}

class EmptyEvidenceRuntime extends FakeRuntime {
  override async verify(
    _target: TargetSnapshot,
    _group: ChangeGroup,
    _context: ContextBundle,
    candidate: Candidate,
  ): Promise<VerificationResult> {
    return {
      candidateId: candidate.id,
      verdict: "confirmed",
      reasonCode: "unsupported_confirmation",
      explanation: "Claims confirmation without source evidence.",
      evidence: [],
    };
  }
}

class InconclusiveRuntime extends FakeRuntime {
  override async verify(
    _target: TargetSnapshot,
    _group: ChangeGroup,
    _context: ContextBundle,
    candidate: Candidate,
  ): Promise<VerificationResult> {
    return {
      candidateId: candidate.id,
      verdict: "inconclusive",
      reasonCode: "caller_contract_unavailable",
      explanation: "The local source is insufficient to prove reachability.",
      evidence: [],
    };
  }
}

class FailedVerificationRuntime extends FakeRuntime {
  override async verify(
    _target: TargetSnapshot,
    _group: ChangeGroup,
    _context: ContextBundle,
    candidate: Candidate,
  ): Promise<VerificationResult> {
    this.verificationInputs.push(candidate);
    throw new Error("fixture verifier failure");
  }
}

class DuplicateAcrossSkillsRuntime extends FakeRuntime {
  override async review(
    target: TargetSnapshot,
    group: ChangeGroup,
    context: ContextBundle,
    skill: SkillDefinition,
  ) {
    return await super.review(target, group, context, {
      ...skill,
      id: "correctness",
    });
  }
}

class SemanticDuplicateAcrossSkillsRuntime extends FakeRuntime {
  override async review(
    target: TargetSnapshot,
    group: ChangeGroup,
    context: ContextBundle,
    skill: SkillDefinition,
  ) {
    const submission = await super.review(target, group, context, {
      ...skill,
      id: "correctness",
    });
    const candidate = submission.candidates[0];
    if (!candidate) return submission;
    return {
      ...submission,
      candidates: [
        skill.id === "correctness"
          ? {
              ...candidate,
              category: "state-machine transition validation",
              title:
                "applyDecision applies invalid transition and overwrites approval state",
            }
          : {
              ...candidate,
              category: "error-contract",
              title:
                "applyDecision permits invalid transition and mutates state without guard",
            },
      ],
    };
  }
}

class RealisticStateTransitionDuplicatesRuntime extends FakeRuntime {
  override async review(
    target: TargetSnapshot,
    group: ChangeGroup,
    context: ContextBundle,
    skill: SkillDefinition,
  ) {
    const submission = await super.review(target, group, context, {
      ...skill,
      id: "correctness",
    });
    const candidate = submission.candidates[0];
    if (!candidate) return submission;
    const titles: Record<string, string> = {
      correctness:
        "applyDecision accepts invalid transitions and overwrites decidedBy instead of rejecting them",
      "error-contract":
        "applyDecision silently applies an illegal rejection and mutates the approval instead of throwing",
      "concurrency-data":
        "applyDecision overwrites an existing decision without a transition guard or CAS",
    };
    const descriptions: Record<string, string> = {
      correctness:
        'applyDecision unconditionally assigns next to approval.state and actorId to approval.decidedBy with no guard for invalid state transitions or already-decided records. Frozen repository-search context shows the concrete trigger in test/approval.test.ts: a fixture with state "approved" and decidedBy "alice" is passed to applyDecision(approval, "rejected", "bob") inside assert.throws, and the test then asserts approval.decidedBy is still "alice". This implementation does not throw and does not preserve the prior value, so the already-approved record is silently changed to rejected and the original decider is overwritten.',
      "error-contract":
        'applyDecision has no transition guard. For any supplied Approval and next state it sets approval.state = next, sets approval.decidedBy = actorId, and returns the same object. The repository test contract exercises applyDecision(approval, "rejected", "bob") where approval is already state "approved" and decidedBy "alice", and expects this call to throw and leave decidedBy as "alice". The implementation returns normally and overwrites both fields, so an already-decided approval is silently flipped to rejected and the original decider is lost.',
      "concurrency-data":
        'applyDecision assigns approval.state = next and approval.decidedBy = actorId unconditionally and returns the same mutable object. There is no check on the current state and no compare-and-set or generation guard, so any caller can overwrite an earlier decision. For example, after an approval is recorded with decidedBy "alice", applyDecision(approval, "rejected", "bob") changes the state to rejected and replaces decidedBy with "bob" instead of rejecting the invalid transition. Frozen repository-search context shows the test expects that exact call to throw and for decidedBy to remain "alice", which the current implementation violates.',
    };
    return {
      ...submission,
      candidates: [
        {
          ...candidate,
          category: skill.id,
          title: titles[skill.id] ?? candidate.title,
          description: descriptions[skill.id] ?? candidate.description,
        },
      ],
    };
  }
}

class DistinctOverlappingCandidatesRuntime extends FakeRuntime {
  override async review(
    target: TargetSnapshot,
    group: ChangeGroup,
    context: ContextBundle,
    skill: SkillDefinition,
  ) {
    const submission = await super.review(target, group, context, {
      ...skill,
      id: "correctness",
    });
    const candidate = submission.candidates[0];
    if (!candidate) return submission;
    return {
      ...submission,
      candidates: [
        skill.id === "correctness"
          ? {
              ...candidate,
              category: "input-validation",
              title: "Missing input validation causes a request panic",
              description:
                "The request handler dereferences an optional input before checking that it exists.",
            }
          : {
              ...candidate,
              category: "audit-contract",
              title:
                "Audit record writes omit the authenticated actor identity",
              description:
                "The audit writer records an event without persisting the authenticated actor identity.",
            },
      ],
    };
  }
}

class TrivialEvidenceRuntime extends FakeRuntime {
  override async review(
    target: TargetSnapshot,
    group: ChangeGroup,
    context: ContextBundle,
    skill: SkillDefinition,
  ) {
    const submission = await super.review(target, group, context, skill);
    const candidate = submission.candidates[0];
    if (candidate) candidate.evidence[0]!.excerpt = "r";
    return submission;
  }
}

class TwoDistinctCandidatesRuntime extends FakeRuntime {
  override async review(
    target: TargetSnapshot,
    group: ChangeGroup,
    context: ContextBundle,
    skill: SkillDefinition,
  ) {
    const submission = await super.review(target, group, context, skill);
    const first = submission.candidates[0];
    if (!first) return submission;
    return {
      ...submission,
      candidates: [
        first,
        {
          ...first,
          category: "contract-violation",
          title: "A second distinct defect candidate",
        },
      ],
    };
  }
}

function reviewOptions(
  repository: string,
  overrides: Partial<ReviewOptions> = {},
): ReviewOptions {
  return {
    repository,
    target: { kind: "files", paths: ["src/handler.ts"] },
    model: "fixture",
    providerProfile: "anthropic-official",
    normalizationRevision: "v2",
    concurrency: 2,
    skills: ["correctness"],
    knowledgeFiles: [],
    verify: true,
    maxToolCalls: 10,
    maxGroupBytes: 65_536,
    maxTargetBytes: 4 * 1024 * 1024,
    maxFiles: 32,
    maxGroups: 8,
    maxCandidates: 32,
    maxModelCalls: 64,
    maxOutputTokens: 8192,
    timeoutMs: 10_000,
    verbose: false,
    ...overrides,
  };
}

test("workflow separates candidate review from independent verification", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const runtime = new FakeRuntime();
    const options: ReviewOptions = {
      repository,
      target: { kind: "files", paths: ["src/handler.ts"] },
      model: "fixture",
      providerProfile: "anthropic-official",
      normalizationRevision: "v2",
      concurrency: 2,
      skills: ["correctness"],
      knowledgeFiles: [],
      verify: true,
      maxToolCalls: 10,
      maxGroupBytes: 65_536,
      maxTargetBytes: 4 * 1024 * 1024,
      maxFiles: 32,
      maxGroups: 8,
      maxCandidates: 32,
      maxModelCalls: 64,
      maxOutputTokens: 8192,
      timeoutMs: 10_000,
      verbose: false,
    };
    const report = await runReviewWorkflow(
      options,
      runtime,
      new AbortController().signal,
    );
    assert.equal(
      report.status,
      "complete",
      JSON.stringify({
        coverage: report.coverage,
        candidates: report.candidates,
      }),
    );
    assert.equal(runtime.reviewCalls, 1);
    assert.equal(report.summary.candidates, 1);
    assert.equal(report.summary.confirmed, 1);
    assert.equal(report.findings.length, 1);
    assert.equal(runtime.verificationInputs.length, 1);
    assert.equal(report.candidates[0]?.rawConfidencePPM, 900_000);
    assert.equal(runtime.verificationInputs[0]?.rawConfidencePPM, undefined);
    assert.equal(
      report.findings[0]?.verification.reasonCode,
      "reachable_crash",
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("workflow resumes completed group and candidate verification checkpoints", async () => {
  const repository = await createRepository();
  try {
    const source =
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n";
    await writeFixture(repository, "src/handler.ts", source);
    await writeFixture(repository, "lib/handler.ts", source);
    const options = reviewOptions(repository, {
      target: {
        kind: "files",
        paths: ["lib/handler.ts", "src/handler.ts"],
      },
    });
    const checkpoints: WorkflowGroupCheckpoint[] = [];
    const first = new FakeRuntime();
    const baseline = await runReviewWorkflow(
      options,
      first,
      new AbortController().signal,
      () => undefined,
      {
        checkpointScopeSha256: "a".repeat(64),
        onGroupCheckpoint(checkpoint) {
          checkpoints.push(checkpoint);
        },
      },
    );
    assert.equal(checkpoints.length, 4);
    assert.equal(first.reviewCalls, 2);
    const latestByGroup = new Map<string, WorkflowGroupCheckpoint>();
    for (const checkpoint of checkpoints) {
      latestByGroup.set(checkpoint.group.id, checkpoint);
    }
    const reusable = [...latestByGroup.values()].sort((left, right) =>
      left.group.id.localeCompare(right.group.id),
    )[0]!;
    assert.equal(reusable.checkpointRevision, 1);
    assert.equal(reusable.verificationResults.length, 1);

    const resumed = new FakeRuntime();
    const report = await runReviewWorkflow(
      options,
      resumed,
      new AbortController().signal,
      () => undefined,
      {
        checkpointScopeSha256: "a".repeat(64),
        resumeCheckpoints: [reusable],
      },
    );
    assert.equal(resumed.reviewCalls, 1);
    assert.equal(resumed.verificationInputs.length, 1);
    assert.deepEqual(report.summary, baseline.summary);
    assert.equal(report.coverage.groupsReviewed, 2);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("workflow does not checkpoint a failed verifier and retries it after resume", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const options = reviewOptions(repository, {
      target: { kind: "files", paths: ["src/handler.ts"] },
    });
    const checkpoints: WorkflowGroupCheckpoint[] = [];
    const failedRuntime = new FailedVerificationRuntime();
    const failed = await runReviewWorkflow(
      options,
      failedRuntime,
      new AbortController().signal,
      () => undefined,
      {
        checkpointScopeSha256: "b".repeat(64),
        onGroupCheckpoint(checkpoint) {
          checkpoints.push(checkpoint);
        },
      },
    );
    assert.equal(failed.status, "partial");
    assert.equal(failedRuntime.verificationInputs.length, 1);
    assert.equal(checkpoints.length, 1);
    assert.equal(checkpoints[0]?.checkpointRevision, 0);
    assert.deepEqual(checkpoints[0]?.verificationResults, []);

    const resumedRuntime = new FakeRuntime();
    const resumed = await runReviewWorkflow(
      options,
      resumedRuntime,
      new AbortController().signal,
      () => undefined,
      {
        checkpointScopeSha256: "b".repeat(64),
        resumeCheckpoints: [checkpoints[0]!],
      },
    );
    assert.equal(resumed.status, "complete");
    assert.equal(resumedRuntime.reviewCalls, 0);
    assert.equal(resumedRuntime.verificationInputs.length, 1);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("workflow rejects a verification checkpoint bound to changed candidate bytes", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const options = reviewOptions(repository, {
      target: { kind: "files", paths: ["src/handler.ts"] },
    });
    const checkpoints: WorkflowGroupCheckpoint[] = [];
    await runReviewWorkflow(
      options,
      new FakeRuntime(),
      new AbortController().signal,
      () => undefined,
      {
        checkpointScopeSha256: "c".repeat(64),
        onGroupCheckpoint(checkpoint) {
          checkpoints.push(checkpoint);
        },
      },
    );
    const tampered = structuredClone(checkpoints.at(-1)!);
    tampered.verificationResults[0]!.candidateSha256 = "0".repeat(64);
    const resumedRuntime = new FakeRuntime();
    await assert.rejects(
      runReviewWorkflow(
        options,
        resumedRuntime,
        new AbortController().signal,
        () => undefined,
        {
          checkpointScopeSha256: "c".repeat(64),
          resumeCheckpoints: [tampered],
        },
      ),
      /invalid verification checkpoint/u,
    );
    assert.equal(resumedRuntime.reviewCalls, 0);
    assert.equal(resumedRuntime.verificationInputs.length, 0);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("workflow downgrades a confirmation without verifiable source evidence", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const options: ReviewOptions = {
      repository,
      target: { kind: "files", paths: ["src/handler.ts"] },
      model: "fixture",
      providerProfile: "anthropic-official",
      normalizationRevision: "v2",
      concurrency: 2,
      skills: ["correctness"],
      knowledgeFiles: [],
      verify: true,
      maxToolCalls: 10,
      maxGroupBytes: 65_536,
      maxTargetBytes: 4 * 1024 * 1024,
      maxFiles: 32,
      maxGroups: 8,
      maxCandidates: 32,
      maxModelCalls: 64,
      maxOutputTokens: 8192,
      timeoutMs: 10_000,
      verbose: false,
    };
    const report = await runReviewWorkflow(
      options,
      new EmptyEvidenceRuntime(),
      new AbortController().signal,
    );
    assert.equal(report.status, "partial");
    assert.equal(report.summary.confirmed, 0);
    assert.equal(report.summary.inconclusive, 1);
    assert.equal(
      report.candidates[0]?.verification?.reasonCode,
      "verifier_error",
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("admission reserves model turns for every admitted verification", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export const value = 1;\n",
    );
    const options: ReviewOptions = {
      repository,
      target: { kind: "files", paths: ["src/handler.ts"] },
      model: "fixture",
      providerProfile: "anthropic-official",
      normalizationRevision: "v2",
      concurrency: 2,
      skills: ["correctness"],
      knowledgeFiles: [],
      verify: true,
      maxToolCalls: 10,
      maxGroupBytes: 65_536,
      maxTargetBytes: 4 * 1024 * 1024,
      maxFiles: 32,
      maxGroups: 8,
      maxCandidates: 32,
      maxModelCalls: 1,
      maxOutputTokens: 8192,
      timeoutMs: 10_000,
      verbose: false,
    };
    await assert.rejects(
      runReviewWorkflow(
        options,
        new FakeRuntime(),
        new AbortController().signal,
      ),
      /require at least 34 Agent tasks \(2 base \+ 32 verification\)/u,
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

class ReviewDimensionProbeRuntime extends FakeRuntime {
  readonly reviewedSkillIDs: string[] = [];

  override async review(
    _target: TargetSnapshot,
    group: ChangeGroup,
    _context: ContextBundle,
    skill: SkillDefinition,
  ) {
    this.reviewCalls++;
    this.reviewedSkillIDs.push(skill.id);
    const anchor = {
      path: group.files[0]?.path ?? "src/handler.ts",
      side: "file" as const,
      startLine: 1,
      endLine: 1,
    };
    const excerpt = group.files[0]?.targetContent?.split("\n")[0] ?? "";
    return {
      summary: `${skill.id} candidate`,
      candidates: [
        {
          category: skill.id,
          severity: "high" as const,
          title: `${skill.id} concrete defect`,
          description: `A concrete ${skill.id} trigger reaches this line.`,
          impact: `The ${skill.id} invariant is violated.`,
          anchor,
          evidence: [
            {
              statement: `${skill.id} source evidence`,
              anchor,
              excerpt,
            },
          ],
        },
      ],
    };
  }

  override async verify(
    _target: TargetSnapshot,
    _group: ChangeGroup,
    _context: ContextBundle,
    candidate: Candidate,
  ): Promise<VerificationResult> {
    this.verificationInputs.push(candidate);
    return {
      candidateId: candidate.id,
      verdict: "confirmed",
      reasonCode: "dimension_specific_trigger_confirmed",
      explanation: "The independent verifier retained the concrete trigger.",
      evidence: candidate.evidence,
    };
  }
}

test("new review dimensions run as separate reviewers and retain independent verification", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export const value = 1;\n",
    );
    const runtime = new ReviewDimensionProbeRuntime();
    const skills = [
      "error-contract",
      "resource-lifecycle",
      "transaction-state",
    ];
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        skills,
        maxCandidates: skills.length,
        maxModelCalls: 1 + skills.length * 2,
      }),
      runtime,
      new AbortController().signal,
    );
    assert.equal(
      report.status,
      "complete",
      JSON.stringify({
        coverage: report.coverage,
        candidates: report.candidates,
      }),
    );
    assert.deepEqual(runtime.reviewedSkillIDs, skills);
    assert.equal(runtime.verificationInputs.length, skills.length);
    assert.deepEqual(
      report.candidates.map((candidate) => candidate.skill.id),
      skills,
    );
    assert.equal(report.coverage.reviewTasksTotal, skills.length);
    assert.equal(report.coverage.verificationTasksTotal, skills.length);
    assert.equal(report.coverage.verificationTasksSucceeded, skills.length);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("verification-disabled candidates make coverage partial", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const report = await runReviewWorkflow(
      reviewOptions(repository, { verify: false }),
      new FakeRuntime(),
      new AbortController().signal,
    );
    assert.equal(report.status, "partial");
    assert.equal(report.coverage.verificationEnabled, false);
    assert.equal(report.coverage.verificationTasksTotal, 1);
    assert.equal(report.coverage.verificationTasksSucceeded, 0);
    assert.equal(
      report.candidates[0]?.verification?.reasonCode,
      "verification_disabled",
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("verification-disabled run with no candidates can remain complete", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export const value = 1;\n",
    );
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        verify: false,
        skills: ["security-contract"],
      }),
      new FakeRuntime(),
      new AbortController().signal,
    );
    assert.equal(report.status, "complete");
    assert.equal(report.coverage.verificationTasksTotal, 0);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("a completed inconclusive verifier still counts as completed coverage", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const report = await runReviewWorkflow(
      reviewOptions(repository),
      new InconclusiveRuntime(),
      new AbortController().signal,
    );
    assert.equal(report.status, "complete");
    assert.equal(report.summary.inconclusive, 1);
    assert.equal(report.coverage.verificationTasksSucceeded, 1);
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("raw duplicate candidates and normalization decisions preserve both skills", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        skills: ["correctness", "security-contract"],
      }),
      new DuplicateAcrossSkillsRuntime(),
      new AbortController().signal,
    );
    assert.equal(report.summary.candidates, 1);
    assert.equal(report.rawCandidates.length, 2);
    assert.deepEqual(
      report.rawCandidates.map((candidate) => candidate.skill.id).sort(),
      ["correctness", "security-contract"],
    );
    assert.deepEqual(
      report.normalizationDecisions.map((decision) => decision.action).sort(),
      ["merged_duplicate", "retained"],
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("semantic duplicate candidates across dimensions share one verification", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const runtime = new SemanticDuplicateAcrossSkillsRuntime();
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        skills: ["correctness", "error-contract"],
      }),
      runtime,
      new AbortController().signal,
    );
    assert.equal(report.summary.candidates, 1);
    assert.equal(report.rawCandidates.length, 2);
    assert.equal(runtime.verificationInputs.length, 1);
    assert.deepEqual(
      report.normalizationDecisions
        .map((decision) => decision.reasonCode)
        .sort(),
      ["normalized_candidate_retained", "semantic_duplicate"],
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("normalization v0 preserves candidates that only v1 semantic matching merges", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const runtime = new SemanticDuplicateAcrossSkillsRuntime();
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        skills: ["correctness", "error-contract"],
        normalizationRevision: "v0",
      }),
      runtime,
      new AbortController().signal,
    );
    assert.equal(report.summary.candidates, 2);
    assert.equal(runtime.verificationInputs.length, 2);
    assert.equal(
      report.execution.snapshot.workflowRevision,
      "argus-pi-review-workflow-v0",
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("realistic root-cause descriptions across three dimensions share one verification", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const runtime = new RealisticStateTransitionDuplicatesRuntime();
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        skills: ["correctness", "error-contract", "concurrency-data"],
      }),
      runtime,
      new AbortController().signal,
    );
    assert.equal(report.summary.candidates, 1);
    assert.equal(report.rawCandidates.length, 3);
    assert.equal(runtime.verificationInputs.length, 1);
    assert.equal(
      report.normalizationDecisions.filter(
        (decision) => decision.reasonCode === "semantic_duplicate",
      ).length,
      2,
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("normalization v1 does not use the v2 description-overlap extension", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const runtime = new RealisticStateTransitionDuplicatesRuntime();
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        skills: ["correctness", "error-contract", "concurrency-data"],
        normalizationRevision: "v1",
      }),
      runtime,
      new AbortController().signal,
    );
    assert.equal(report.summary.candidates, 3);
    assert.equal(runtime.verificationInputs.length, 3);
    assert.equal(
      report.execution.snapshot.workflowRevision,
      "argus-pi-review-workflow-v1",
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("overlapping anchors with different defect semantics remain distinct", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const runtime = new DistinctOverlappingCandidatesRuntime();
    const report = await runReviewWorkflow(
      reviewOptions(repository, {
        skills: ["correctness", "error-contract"],
      }),
      runtime,
      new AbortController().signal,
    );
    assert.equal(report.summary.candidates, 2);
    assert.equal(runtime.verificationInputs.length, 2);
    assert.deepEqual(
      report.normalizationDecisions.map((decision) => decision.action),
      ["retained", "retained"],
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("trivial substring evidence is rejected but retained as a raw candidate fact", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const report = await runReviewWorkflow(
      reviewOptions(repository),
      new TrivialEvidenceRuntime(),
      new AbortController().signal,
    );
    assert.equal(report.status, "partial");
    assert.equal(report.summary.candidates, 0);
    assert.equal(report.rawCandidates.length, 1);
    assert.equal(report.normalizationDecisions[0]?.action, "rejected_invalid");
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});

test("candidate budget preserves excluded raw claims without over-admitting verification", async () => {
  const repository = await createRepository();
  try {
    await writeFixture(
      repository,
      "src/handler.ts",
      "export function handle(input?: { value: string }) {\n  return input.value;\n}\n",
    );
    const runtime = new TwoDistinctCandidatesRuntime();
    const report = await runReviewWorkflow(
      reviewOptions(repository, { maxCandidates: 1 }),
      runtime,
      new AbortController().signal,
    );

    assert.equal(report.status, "partial");
    assert.equal(report.rawCandidates.length, 2);
    assert.equal(report.candidates.length, 1);
    assert.equal(report.coverage.verificationTasksTotal, 1);
    assert.equal(runtime.verificationInputs.length, 1);
    assert.deepEqual(
      report.normalizationDecisions.map((decision) => decision.action).sort(),
      ["excluded_budget", "retained"],
    );
  } finally {
    await rm(repository, { recursive: true, force: true });
  }
});
