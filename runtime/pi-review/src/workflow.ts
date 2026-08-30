import { createHash } from "node:crypto";

import type { AgentRuntime } from "./agent.js";
import { describeAgentError } from "./agent.js";
import { CliError, errorMessage } from "./errors.js";
import {
  canonicalJson,
  createExecutionEnvelope,
  createExecutionSnapshot,
} from "./execution.js";
import { mergeTaskEvidenceCollections } from "./evidence.js";
import { captureTarget, isTargetStale } from "./git.js";
import { partitionTarget } from "./grouping.js";
import { ConcurrencyLimiter } from "./limit.js";
import { loadKnowledgeBundle, loadSkills } from "./skills.js";
import {
  REPORT_SCHEMA_VERSION,
  type AgentTaskObservation,
  type Candidate,
  type CandidateClaim,
  type CandidateNormalizationDecision,
  type ChangeGroup,
  type ContextBundle,
  type EvidenceRef,
  type Finding,
  type GroupFailure,
  type KnowledgeBundle,
  type ProgressEvent,
  type RawCandidateRecord,
  type ReviewOptions,
  type ReviewReport,
  type SkillDefinition,
  type SourceAnchor,
  type TargetSnapshot,
  type VerificationResult,
  type VerificationCandidate,
} from "./types.js";
import {
  frozenAnchorAuthorized,
  isFrozenTarget,
  splitAddressableSourceLines,
} from "./target.js";
import {
  normalizeAnchorForTarget,
  validateEvidenceRefs,
} from "./source-evidence.js";

interface GroupState {
  group: ChangeGroup;
  context?: ContextBundle;
  reviewersSucceeded: number;
}

export interface WorkflowGroupCheckpoint {
  schemaVersion: "argus.pi-review.group_checkpoint.v1";
  checkpointRevision: number;
  checkpointScopeSha256: string;
  targetDigest: string;
  workflowRevision:
    | "argus-pi-review-workflow-v0"
    | "argus-pi-review-workflow-v1"
    | "argus-pi-review-workflow-v2";
  groupingRevision: "directory-language-v0";
  group: {
    id: string;
    key: string;
    patchDigest: string;
    files: string[];
  };
  context?: ContextBundle;
  reviewersSucceeded: number;
  failures: GroupFailure[];
  rawCandidates: RawCandidateRecord[];
  validatedCandidates: Array<{
    rawCandidateId: string;
    candidate: Candidate;
  }>;
  normalizationDecisions: CandidateNormalizationDecision[];
  dependencyCanceledTasks: AgentTaskObservation[];
  verificationResults: WorkflowVerificationCheckpoint[];
  observations: AgentTaskObservation[];
  taskEvidence: import("./types.js").PiTaskEvidenceCollection;
}

export interface WorkflowVerificationCheckpoint {
  candidateId: string;
  candidateSha256: string;
  result: VerificationResult;
}

export interface WorkflowControl {
  checkpointScopeSha256?: string;
  resumeCheckpoints?: WorkflowGroupCheckpoint[];
  onGroupCheckpoint?: (
    checkpoint: WorkflowGroupCheckpoint,
  ) => void | Promise<void>;
}

export async function runReviewWorkflow(
  options: ReviewOptions,
  runtime: AgentRuntime,
  signal: AbortSignal,
  onProgress: (event: ProgressEvent) => void = () => undefined,
  control: WorkflowControl = {},
): Promise<ReviewReport> {
  onProgress({
    phase: "capture",
    message: "capturing immutable review target",
  });
  const [target, skills, knowledgeBundle] = await Promise.all([
    captureTarget(options, signal),
    loadSkills(options.skills),
    loadKnowledgeBundle(options.knowledgeFiles),
  ]);
  return await runWorkflowOnTarget(
    options,
    runtime,
    signal,
    onProgress,
    target,
    skills,
    knowledgeBundle,
    async () => await isTargetStale(target, options, signal).catch(() => true),
    control,
  );
}

/**
 * Executes against a host-validated immutable target without performing target,
 * skill-selector, or knowledge-file discovery. This is the only workflow entry
 * used by the stdio frozen-input worker.
 */
export async function runInjectedReviewWorkflow(
  options: ReviewOptions,
  runtime: AgentRuntime,
  signal: AbortSignal,
  target: TargetSnapshot,
  skills: SkillDefinition[],
  knowledgeBundle: KnowledgeBundle,
  onProgress: (event: ProgressEvent) => void = () => undefined,
  control: WorkflowControl = {},
): Promise<ReviewReport> {
  if (!isFrozenTarget(target)) {
    throw new Error("injected workflow requires a frozen inline target");
  }
  if (options.knowledgeFiles.length !== 0) {
    throw new Error("injected workflow forbids knowledge file loading");
  }
  return await runWorkflowOnTarget(
    options,
    runtime,
    signal,
    onProgress,
    target,
    skills,
    knowledgeBundle,
    async () => false,
    control,
  );
}

async function runWorkflowOnTarget(
  options: ReviewOptions,
  runtime: AgentRuntime,
  signal: AbortSignal,
  onProgress: (event: ProgressEvent) => void,
  target: TargetSnapshot,
  skills: SkillDefinition[],
  knowledgeBundle: KnowledgeBundle,
  checkStale: () => Promise<boolean>,
  control: WorkflowControl = {},
): Promise<ReviewReport> {
  const knowledge = knowledgeBundle.content;
  const groups = partitionTarget(target, options.maxGroupBytes);
  validateReviewAdmission(options, target, groups, skills.length);
  const executionSnapshot = createExecutionSnapshot(
    options,
    target,
    groups,
    skills,
    knowledgeBundle.refs,
    runtime,
  );
  const resumed = validateResumeCheckpoints(
    control,
    target,
    groups,
    skills,
    options.normalizationRevision,
  );
  const limiter = new ConcurrencyLimiter(options.concurrency);
  let agentTasks = [...resumed.values()].reduce(
    (total, checkpoint) => total + checkpoint.observations.length,
    0,
  );
  const invokeAgent = async <T>(operation: () => Promise<T>): Promise<T> => {
    if (agentTasks >= options.maxModelCalls) {
      throw new Error(
        `provider/model turn budget cannot admit another Agent task (${options.maxModelCalls})`,
      );
    }
    agentTasks++;
    return await limiter.run(operation);
  };
  const failures: GroupFailure[] = [];
  const rawCandidates: RawCandidateRecord[] = [];
  const validatedCandidates: Array<{
    rawCandidateId: string;
    candidate: Candidate;
  }> = [];
  const normalizationDecisions: CandidateNormalizationDecision[] = [];
  const dependencyCanceledTasks: AgentTaskObservation[] = [];
  const groupResults = await Promise.all(
    groups.map(async (group): Promise<WorkflowGroupCheckpoint> => {
      const prior = resumed.get(group.id);
      if (prior) {
        onProgress({
          phase: "context",
          groupId: group.id,
          message: `${group.id}: reusing durable context/review checkpoint`,
        });
        return prior;
      }
      const groupFailures: GroupFailure[] = [];
      const groupRawCandidates: RawCandidateRecord[] = [];
      const groupValidatedCandidates: Array<{
        rawCandidateId: string;
        candidate: Candidate;
      }> = [];
      const groupNormalizationDecisions: CandidateNormalizationDecision[] = [];
      const groupDependencyCanceledTasks: AgentTaskObservation[] = [];
      let context: ContextBundle;
      try {
        onProgress({
          phase: "context",
          groupId: group.id,
          message: `${group.id}: collecting code context`,
        });
        context = await invokeAgent(() =>
          runtime.collectContext(target, group, knowledge, signal),
        );
      } catch (error) {
        if (signal.aborted) throw signal.reason ?? error;
        groupFailures.push({
          groupId: group.id,
          phase: "context",
          error: describeAgentError(error),
        });
        const canceledAt = new Date().toISOString();
        for (const skill of skills) {
          groupDependencyCanceledTasks.push(
            createDependencyCanceledObservation(
              target,
              group,
              skill,
              canceledAt,
            ),
          );
        }
        return await finishGroupCheckpoint({
          control,
          target,
          group,
          reviewersSucceeded: 0,
          failures: groupFailures,
          rawCandidates: groupRawCandidates,
          validatedCandidates: groupValidatedCandidates,
          normalizationDecisions: groupNormalizationDecisions,
          normalizationRevision: options.normalizationRevision,
          dependencyCanceledTasks: groupDependencyCanceledTasks,
          persist: false,
          runtime,
        });
      }

      let reviewersSucceeded = 0;
      const submissions = await Promise.all(
        skills.map(async (skill) => {
          try {
            onProgress({
              phase: "review",
              groupId: group.id,
              skillId: skill.id,
              message: `${group.id}: reviewing ${skill.id}`,
            });
            const submission = await invokeAgent(() =>
              runtime.review(target, group, context, skill, knowledge, signal),
            );
            reviewersSucceeded++;
            return { skill, claims: submission.candidates };
          } catch (error) {
            if (signal.aborted) throw signal.reason ?? error;
            groupFailures.push({
              groupId: group.id,
              phase: "review",
              skillId: skill.id,
              error: describeAgentError(error),
            });
            return { skill, claims: [] as CandidateClaim[] };
          }
        }),
      );

      for (const submission of submissions) {
        for (const [ordinal, claim] of submission.claims.entries()) {
          const raw = createRawCandidateRecord(
            group,
            submission.skill,
            claim,
            ordinal,
          );
          groupRawCandidates.push(raw);
          try {
            groupValidatedCandidates.push({
              rawCandidateId: raw.rawCandidateId,
              candidate: await createCandidate(
                target,
                group,
                submission.skill,
                claim,
                signal,
              ),
            });
          } catch (error) {
            groupNormalizationDecisions.push({
              rawCandidateId: raw.rawCandidateId,
              action: "rejected_invalid",
              reasonCode: "invalid_candidate",
            });
            groupFailures.push({
              groupId: group.id,
              phase: "review",
              skillId: submission.skill.id,
              error: `invalid candidate discarded: ${errorMessage(error)}`,
            });
          }
        }
      }
      return await finishGroupCheckpoint({
        control,
        target,
        group,
        context,
        reviewersSucceeded,
        failures: groupFailures,
        rawCandidates: groupRawCandidates,
        validatedCandidates: groupValidatedCandidates,
        normalizationDecisions: groupNormalizationDecisions,
        normalizationRevision: options.normalizationRevision,
        dependencyCanceledTasks: groupDependencyCanceledTasks,
        persist: reviewersSucceeded === skills.length,
        runtime,
      });
    }),
  );

  const states: GroupState[] = groupResults.map((checkpoint) => ({
    group: groups.find(
      (group) => group.id === checkpoint.group.id,
    ) as ChangeGroup,
    ...(checkpoint.context ? { context: checkpoint.context } : {}),
    reviewersSucceeded: checkpoint.reviewersSucceeded,
  }));
  for (const checkpoint of groupResults) {
    failures.push(...checkpoint.failures);
    rawCandidates.push(...checkpoint.rawCandidates);
    validatedCandidates.push(...checkpoint.validatedCandidates);
    normalizationDecisions.push(...checkpoint.normalizationDecisions);
    dependencyCanceledTasks.push(...checkpoint.dependencyCanceledTasks);
  }

  const candidates = normalizeCandidates(
    validatedCandidates,
    normalizationDecisions,
    options.maxCandidates,
    options.normalizationRevision,
  );
  const candidatesExcludedByBudget = normalizationDecisions.filter(
    (decision) => decision.action === "excluded_budget",
  ).length;
  if (candidatesExcludedByBudget > 0) {
    failures.push({
      groupId: "workflow",
      phase: "review",
      error: `${candidatesExcludedByBudget} distinct candidate(s) exceeded the normalized candidate budget`,
    });
  }
  const stateByGroup = new Map(states.map((state) => [state.group.id, state]));
  const checkpointByGroup = new Map(
    groupResults.map((checkpoint) => [checkpoint.group.id, checkpoint]),
  );
  const verificationEntriesByGroup = new Map(
    groupResults.map((checkpoint) => [
      checkpoint.group.id,
      new Map(
        checkpoint.verificationResults.map((entry) => [
          entry.candidateId,
          entry,
        ]),
      ),
    ]),
  );
  const verificationByCandidate = new Map<string, VerificationResult>();
  let verificationTasksSucceeded = 0;

  const candidateByID = new Map(
    candidates.map((candidate) => [candidate.id, candidate]),
  );
  for (const checkpoint of groupResults) {
    for (const entry of checkpoint.verificationResults) {
      const candidate = candidateByID.get(entry.candidateId);
      if (
        !options.verify ||
        !candidate ||
        candidate.groupId !== checkpoint.group.id ||
        entry.candidateSha256 !== verificationCandidateSHA256(candidate) ||
        entry.result.candidateId !== candidate.id
      ) {
        throw new Error(
          `invalid verification checkpoint ${checkpoint.group.id}/${entry.candidateId}`,
        );
      }
      const evidence = await validateEvidenceRefs(
        target,
        entry.result.evidence,
        signal,
      );
      if (entry.result.verdict === "confirmed" && evidence.length === 0) {
        throw new Error(
          `invalid verification checkpoint ${checkpoint.group.id}/${entry.candidateId}: confirmed verdict requires source evidence`,
        );
      }
      verificationByCandidate.set(candidate.id, {
        ...entry.result,
        evidence,
      });
      verificationTasksSucceeded++;
    }
  }

  await Promise.all(
    candidates.map(async (candidate) => {
      if (verificationByCandidate.has(candidate.id)) {
        onProgress({
          phase: "verification",
          groupId: candidate.groupId,
          candidateId: candidate.id,
          message: `${candidate.id}: reusing durable verification checkpoint`,
        });
        return;
      }
      const state = stateByGroup.get(candidate.groupId);
      if (!state?.context) {
        verificationByCandidate.set(candidate.id, {
          candidateId: candidate.id,
          verdict: "inconclusive",
          reasonCode: "context_unavailable",
          explanation: "The context collection phase did not complete.",
          evidence: [],
        });
        return;
      }
      if (!options.verify) {
        verificationByCandidate.set(candidate.id, {
          candidateId: candidate.id,
          verdict: "inconclusive",
          reasonCode: "verification_disabled",
          explanation: "Independent verification was disabled by the caller.",
          evidence: [],
        });
        return;
      }
      let verified: VerificationResult;
      try {
        onProgress({
          phase: "verification",
          groupId: candidate.groupId,
          candidateId: candidate.id,
          message: `${candidate.id}: independently verifying candidate`,
        });
        const blindCandidate = verificationCandidate(candidate);
        const result = await invokeAgent(() =>
          runtime.verify(
            target,
            state.group,
            state.context as ContextBundle,
            blindCandidate,
            knowledge,
            signal,
          ),
        );
        const evidence = await validateEvidenceRefs(
          target,
          result.evidence,
          signal,
        );
        if (result.verdict === "confirmed" && evidence.length === 0) {
          throw new Error("confirmed verdict requires source evidence");
        }
        verified = { ...result, evidence };
      } catch (error) {
        if (signal.aborted) throw signal.reason ?? error;
        failures.push({
          groupId: candidate.groupId,
          phase: "verification",
          candidateId: candidate.id,
          error: describeAgentError(error),
        });
        verificationByCandidate.set(candidate.id, {
          candidateId: candidate.id,
          verdict: "inconclusive",
          reasonCode: "verifier_error",
          explanation: "Independent verification failed to complete.",
          evidence: [],
        });
        return;
      }
      verificationTasksSucceeded++;
      verificationByCandidate.set(candidate.id, verified);
      const entries = verificationEntriesByGroup.get(candidate.groupId);
      const priorCheckpoint = checkpointByGroup.get(candidate.groupId);
      if (!entries || !priorCheckpoint) {
        throw new Error(`group checkpoint missing for ${candidate.groupId}`);
      }
      entries.set(candidate.id, {
        candidateId: candidate.id,
        candidateSha256: verificationCandidateSHA256(candidate),
        result: verified,
      });
      const observations = mergeObservations(
        priorCheckpoint.observations,
        runtime
          .getExecutionObservations()
          .filter((task) => task.groupId === candidate.groupId),
      );
      const checkpoint: WorkflowGroupCheckpoint = {
        ...priorCheckpoint,
        checkpointRevision: entries.size,
        verificationResults: [...entries.values()].sort((left, right) =>
          left.candidateId.localeCompare(right.candidateId),
        ),
        observations,
        taskEvidence: mergeTaskEvidenceCollections(
          [
            priorCheckpoint.taskEvidence,
            runtime.getExecutionEvidence?.(
              undefined,
              observations.map((task) => task.taskId),
            ) ?? emptyTaskEvidence(observations.length > 0),
          ],
          observations.map((task) => task.taskId),
        ),
      };
      checkpointByGroup.set(candidate.groupId, checkpoint);
      await control.onGroupCheckpoint?.(checkpoint);
    }),
  );

  const staleTarget = await checkStale();
  const projectedCandidates: Array<
    Candidate & { verification: VerificationResult }
  > = candidates.map((candidate) => {
    const verification = verificationByCandidate.get(candidate.id);
    if (!verification) {
      throw new Error(`verification result missing for ${candidate.id}`);
    }
    return { ...candidate, verification };
  });
  const findings = projectedCandidates
    .filter(
      (
        candidate,
      ): candidate is Candidate & {
        verification: VerificationResult & { verdict: "confirmed" };
      } => candidate.verification?.verdict === "confirmed",
    )
    .map(
      (candidate): Finding => ({
        ...candidate,
        verification: candidate.verification,
      }),
    );
  const verdicts = [...verificationByCandidate.values()];
  const contextGaps = [
    ...(target.frozenContextGaps ?? []),
    ...states.flatMap((state) =>
      (state.context?.gaps ?? []).map((gap) => `${state.group.id}: ${gap}`),
    ),
  ];
  const groupsReviewed = states.filter(
    (state) => state.reviewersSucceeded === skills.length,
  ).length;
  const reviewTasksSucceeded = states.reduce(
    (total, state) => total + state.reviewersSucceeded,
    0,
  );
  const reviewTasksTotal = groups.length * skills.length;
  const verificationTasksTotal = candidates.length;
  const verificationIncomplete =
    verificationTasksTotal > 0 &&
    verificationTasksSucceeded !== verificationTasksTotal;
  const status =
    reviewTasksSucceeded === 0
      ? "failed"
      : failures.length > 0 ||
          staleTarget ||
          target.skipped.length > 0 ||
          contextGaps.length > 0 ||
          groupsReviewed !== groups.length ||
          verificationIncomplete
        ? "partial"
        : "complete";
  const executionTasks = mergeObservations(
    [...checkpointByGroup.values()].flatMap(
      (checkpoint) => checkpoint.observations,
    ),
    runtime.getExecutionObservations(),
    dependencyCanceledTasks,
  );
  const report: ReviewReport = {
    schemaVersion: REPORT_SCHEMA_VERSION,
    status,
    provider: runtime.provider,
    providerProfile: runtime.providerProfile,
    model: runtime.model,
    target: publicTarget(target),
    coverage: {
      groupsTotal: groups.length,
      groupsReviewed,
      reviewTasksTotal,
      reviewTasksSucceeded,
      verificationEnabled: options.verify,
      verificationTasksTotal,
      verificationTasksSucceeded,
      filesIncluded: target.files.length,
      skipped: target.skipped,
      contextGaps,
      failures: stableFailures(failures),
      staleTarget,
    },
    summary: {
      candidates: candidates.length,
      confirmed: verdicts.filter((result) => result.verdict === "confirmed")
        .length,
      rejected: verdicts.filter((result) => result.verdict === "rejected")
        .length,
      inconclusive: verdicts.filter(
        (result) => result.verdict === "inconclusive",
      ).length,
    },
    findings: stableCandidates(findings),
    candidates: stableCandidates(projectedCandidates),
    rawCandidates: stableRawCandidates(rawCandidates),
    normalizationDecisions: stableNormalizationDecisions(
      normalizationDecisions,
    ),
    execution: createExecutionEnvelope(
      executionSnapshot,
      executionTasks,
      mergeTaskEvidenceCollections(
        [
          ...[...checkpointByGroup.values()].map(
            (checkpoint) => checkpoint.taskEvidence,
          ),
          runtime.getExecutionEvidence?.(
            undefined,
            runtime.getExecutionObservations().map((task) => task.taskId),
          ) ?? emptyTaskEvidence(),
        ],
        executionTasks.map((task) => task.taskId),
      ),
    ),
  };
  onProgress({
    phase: "complete",
    message: `review ${status}: ${report.summary.confirmed} confirmed finding(s)`,
  });
  return report;
}

async function finishGroupCheckpoint(input: {
  control: WorkflowControl;
  target: TargetSnapshot;
  group: ChangeGroup;
  context?: ContextBundle;
  reviewersSucceeded: number;
  failures: GroupFailure[];
  rawCandidates: RawCandidateRecord[];
  validatedCandidates: Array<{ rawCandidateId: string; candidate: Candidate }>;
  normalizationDecisions: CandidateNormalizationDecision[];
  normalizationRevision: "v0" | "v1" | "v2";
  dependencyCanceledTasks: AgentTaskObservation[];
  persist: boolean;
  runtime: AgentRuntime;
}): Promise<WorkflowGroupCheckpoint> {
  const observations = input.runtime
    .getExecutionObservations()
    .filter((task) => task.groupId === input.group.id);
  const checkpoint: WorkflowGroupCheckpoint = {
    schemaVersion: "argus.pi-review.group_checkpoint.v1",
    checkpointRevision: 0,
    checkpointScopeSha256:
      input.control.checkpointScopeSha256 ??
      hashText(`ephemeral:${input.target.digest}`),
    targetDigest: input.target.digest,
    workflowRevision: `argus-pi-review-workflow-${input.normalizationRevision}`,
    groupingRevision: "directory-language-v0",
    group: {
      id: input.group.id,
      key: input.group.key,
      patchDigest: hashText(input.group.patch),
      files: input.group.files.map((file) => file.path).sort(),
    },
    ...(input.context ? { context: input.context } : {}),
    reviewersSucceeded: input.reviewersSucceeded,
    failures: input.failures,
    rawCandidates: input.rawCandidates,
    validatedCandidates: input.validatedCandidates,
    normalizationDecisions: input.normalizationDecisions,
    dependencyCanceledTasks: input.dependencyCanceledTasks,
    verificationResults: [],
    observations,
    taskEvidence:
      input.runtime.getExecutionEvidence?.(
        undefined,
        observations.map((task) => task.taskId),
      ) ?? emptyTaskEvidence(observations.length > 0),
  };
  if (input.persist) {
    await input.control.onGroupCheckpoint?.(checkpoint);
  }
  return checkpoint;
}

function validateResumeCheckpoints(
  control: WorkflowControl,
  target: TargetSnapshot,
  groups: ChangeGroup[],
  skills: SkillDefinition[],
  normalizationRevision: "v0" | "v1" | "v2",
): Map<string, WorkflowGroupCheckpoint> {
  const result = new Map<string, WorkflowGroupCheckpoint>();
  for (const checkpointValue of control.resumeCheckpoints ?? []) {
    if (
      !isRecord(checkpointValue) ||
      !isRecord(checkpointValue.group) ||
      typeof checkpointValue.group.id !== "string"
    ) {
      throw new Error("invalid group checkpoint object");
    }
    const checkpoint = checkpointValue as WorkflowGroupCheckpoint;
    const group = groups.find(
      (candidate) => candidate.id === checkpoint.group.id,
    );
    if (
      !control.checkpointScopeSha256 ||
      checkpoint.schemaVersion !== "argus.pi-review.group_checkpoint.v1" ||
      !Number.isSafeInteger(checkpoint.checkpointRevision) ||
      checkpoint.checkpointRevision < 0 ||
      checkpoint.checkpointScopeSha256 !== control.checkpointScopeSha256 ||
      checkpoint.targetDigest !== target.digest ||
      checkpoint.workflowRevision !==
        `argus-pi-review-workflow-${normalizationRevision}` ||
      checkpoint.groupingRevision !== "directory-language-v0" ||
      !group ||
      checkpoint.group.key !== group.key ||
      checkpoint.group.patchDigest !== hashText(group.patch) ||
      JSON.stringify(checkpoint.group.files) !==
        JSON.stringify(group.files.map((file) => file.path).sort()) ||
      !Array.isArray(checkpoint.failures) ||
      !Array.isArray(checkpoint.rawCandidates) ||
      !Array.isArray(checkpoint.validatedCandidates) ||
      !Array.isArray(checkpoint.normalizationDecisions) ||
      !Array.isArray(checkpoint.dependencyCanceledTasks) ||
      !Array.isArray(checkpoint.verificationResults) ||
      !Array.isArray(checkpoint.observations) ||
      checkpoint.checkpointRevision !== checkpoint.verificationResults.length ||
      !Number.isSafeInteger(checkpoint.reviewersSucceeded) ||
      checkpoint.reviewersSucceeded < 0 ||
      checkpoint.reviewersSucceeded > skills.length ||
      checkpoint.observations.some(
        (task) => !isRecord(task) || task.groupId !== group.id,
      ) ||
      checkpoint.rawCandidates.some(
        (candidate) => !isRecord(candidate) || candidate.groupId !== group.id,
      ) ||
      checkpoint.validatedCandidates.some(
        (candidate) =>
          !isRecord(candidate) ||
          !isRecord(candidate.candidate) ||
          candidate.candidate.groupId !== group.id,
      ) ||
      !isRecord(checkpoint.taskEvidence) ||
      !Array.isArray(checkpoint.taskEvidence.tasks) ||
      !validVerificationCheckpointShape(checkpoint.verificationResults) ||
      result.has(group.id)
    ) {
      throw new Error(
        `invalid or conflicting group checkpoint ${checkpoint.group.id}`,
      );
    }
    result.set(group.id, checkpoint);
  }
  return result;
}

function validVerificationCheckpointShape(
  entries: WorkflowVerificationCheckpoint[],
): boolean {
  let previous = "";
  for (const entry of entries) {
    if (
      !entry ||
      typeof entry !== "object" ||
      typeof entry.candidateId !== "string" ||
      entry.candidateId.length === 0 ||
      entry.candidateId <= previous ||
      !/^[a-f0-9]{64}$/u.test(entry.candidateSha256) ||
      !entry.result ||
      entry.result.candidateId !== entry.candidateId ||
      !["confirmed", "rejected", "inconclusive"].includes(
        entry.result.verdict,
      ) ||
      typeof entry.result.reasonCode !== "string" ||
      entry.result.reasonCode.length === 0 ||
      typeof entry.result.explanation !== "string" ||
      entry.result.explanation.length === 0 ||
      !Array.isArray(entry.result.evidence)
    ) {
      return false;
    }
    previous = entry.candidateId;
  }
  return true;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function mergeObservations(
  ...collections: AgentTaskObservation[][]
): AgentTaskObservation[] {
  const byID = new Map<string, AgentTaskObservation>();
  for (const observation of collections.flat()) {
    const previous = byID.get(observation.taskId);
    if (previous && JSON.stringify(previous) !== JSON.stringify(observation)) {
      throw new Error(`task observation conflict for ${observation.taskId}`);
    }
    byID.set(observation.taskId, observation);
  }
  return [...byID.values()];
}

function emptyTaskEvidence(
  unavailable = false,
): import("./types.js").PiTaskEvidenceCollection {
  return {
    schemaVersion: "argus.pi-review.task_evidence.v0",
    authority: "diagnostic_only",
    provenanceClass: "worker_self_report",
    contentPolicy: "exact_local_sensitive",
    completeness: unavailable ? "partial" : "complete",
    reasonCodes: unavailable ? ["task_evidence_unavailable"] : [],
    tasks: [],
  };
}

function hashText(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

function verificationCandidate(candidate: Candidate): VerificationCandidate {
  const { rawConfidencePPM: _reviewerConfidence, ...blindCandidate } =
    candidate;
  return blindCandidate;
}

function verificationCandidateSHA256(candidate: Candidate): string {
  return hashText(canonicalJson(verificationCandidate(candidate)));
}

export function validateReviewAdmission(
  options: ReviewOptions,
  target: TargetSnapshot,
  groups: ChangeGroup[],
  skillCount: number,
): void {
  if (target.files.length > options.maxFiles) {
    throw new CliError(
      `target has ${target.files.length} files; --max-files is ${options.maxFiles}. Narrow the target or explicitly raise the limit`,
    );
  }
  if (groups.length > options.maxGroups) {
    throw new CliError(
      `target has ${groups.length} groups; --max-groups is ${options.maxGroups}. Narrow the target or explicitly raise the limit`,
    );
  }
  const requiredBaseTasks = groups.length * (1 + skillCount);
  const requiredVerificationTasks = options.verify ? options.maxCandidates : 0;
  const requiredMinimumTasks = requiredBaseTasks + requiredVerificationTasks;
  if (requiredMinimumTasks > options.maxModelCalls) {
    throw new CliError(
      `context, review and reserved verification require at least ${requiredMinimumTasks} Agent tasks (${requiredBaseTasks} base + ${requiredVerificationTasks} verification) and therefore at least that many provider/model turns; --max-model-calls is ${options.maxModelCalls}`,
    );
  }
}

function createRawCandidateRecord(
  group: ChangeGroup,
  skill: SkillDefinition,
  claim: CandidateClaim,
  ordinal: number,
): RawCandidateRecord {
  const rawCandidateId = `raw-candidate-${hash([
    group.id,
    skill.id,
    skill.revision,
    ordinal,
    claim.category,
    claim.title,
    claim.anchor,
  ]).slice(0, 20)}`;
  return {
    rawCandidateId,
    groupId: group.id,
    skill: { id: skill.id, revision: skill.revision },
    ordinal,
    claim,
  };
}

async function createCandidate(
  target: TargetSnapshot,
  group: ChangeGroup,
  skill: SkillDefinition,
  claim: CandidateClaim,
  signal: AbortSignal,
): Promise<Candidate> {
  const anchor = normalizeAnchorForTarget(target, claim.anchor);
  if (
    !group.files.some(
      (file) => file.path === anchor.path || file.oldPath === anchor.path,
    )
  ) {
    throw new Error(`anchor ${anchor.path} is outside change group`);
  }
  const evidence = await validateEvidenceRefs(target, claim.evidence, signal);
  if (evidence.length === 0) {
    throw new Error("candidate requires source evidence");
  }
  const anchoredFile = group.files.find(
    (file) => file.path === anchor.path || file.oldPath === anchor.path,
  );
  if (
    anchoredFile &&
    !frozenAnchorAuthorized(
      target,
      anchoredFile,
      anchor.startLine,
      anchor.endLine,
    )
  ) {
    throw new Error(
      `anchor ${anchor.path}:${anchor.startLine}-${anchor.endLine} is outside the authorized frozen region`,
    );
  }
  const content =
    anchor.side === "old"
      ? anchoredFile?.baseContent
      : (anchoredFile?.targetContent ?? anchoredFile?.baseContent);
  if (
    content !== undefined &&
    anchor.endLine > splitAddressableSourceLines(content).length
  ) {
    throw new Error(`anchor line ${anchor.endLine} exceeds ${anchor.path}`);
  }
  if (
    anchoredFile &&
    !anchorTouchesChangedLine(anchoredFile.patch, anchoredFile.status, anchor)
  ) {
    throw new Error(
      `anchor ${anchor.path}:${anchor.startLine}-${anchor.endLine} does not overlap changed code`,
    );
  }
  const fingerprint = hash([
    anchor.path,
    anchor.side,
    anchor.startLine,
    anchor.endLine,
    claim.category.toLowerCase().trim(),
    normalizeText(claim.title),
  ]);
  return {
    ...claim,
    evidence,
    anchor,
    id: `candidate-${fingerprint.slice(0, 16)}`,
    fingerprint,
    groupId: group.id,
    skill: { id: skill.id, revision: skill.revision },
  };
}

function normalizeCandidates(
  entries: Array<{ rawCandidateId: string; candidate: Candidate }>,
  decisions: CandidateNormalizationDecision[],
  maxCandidates: number,
  normalizationRevision: "v0" | "v1" | "v2",
): Candidate[] {
  const ordered = [...entries].sort(
    (left, right) =>
      compareCandidates(left.candidate, right.candidate) ||
      left.rawCandidateId.localeCompare(right.rawCandidateId),
  );
  const winners = new Map<string, Candidate>();
  for (const entry of ordered) {
    const winner = winners.get(entry.candidate.fingerprint);
    if (winner) {
      decisions.push({
        rawCandidateId: entry.rawCandidateId,
        action: "merged_duplicate",
        reasonCode: "duplicate_fingerprint",
        normalizedCandidateId: winner.id,
      });
      continue;
    }
    const semanticWinner = [...winners.values()].find((candidate) =>
      semanticallyDuplicateCandidates(
        candidate,
        entry.candidate,
        normalizationRevision,
      ),
    );
    if (semanticWinner) {
      decisions.push({
        rawCandidateId: entry.rawCandidateId,
        action: "merged_duplicate",
        reasonCode: "semantic_duplicate",
        normalizedCandidateId: semanticWinner.id,
      });
      continue;
    }
    if (winners.size >= maxCandidates) {
      decisions.push({
        rawCandidateId: entry.rawCandidateId,
        action: "excluded_budget",
        reasonCode: "candidate_budget_exceeded",
      });
      continue;
    }
    winners.set(entry.candidate.fingerprint, entry.candidate);
    decisions.push({
      rawCandidateId: entry.rawCandidateId,
      action: "retained",
      reasonCode: "normalized_candidate_retained",
      normalizedCandidateId: entry.candidate.id,
    });
  }
  return [...winners.values()];
}

const semanticTitleStopWords = new Set([
  "a",
  "an",
  "and",
  "are",
  "as",
  "at",
  "be",
  "been",
  "being",
  "before",
  "but",
  "by",
  "for",
  "from",
  "in",
  "instead",
  "into",
  "is",
  "it",
  "of",
  "on",
  "or",
  "than",
  "that",
  "the",
  "then",
  "this",
  "to",
  "was",
  "were",
  "when",
  "while",
  "without",
  "with",
]);

const semanticTitleAliases = new Map<string, string>([
  ["accept", "permit"],
  ["accepted", "permit"],
  ["accepting", "permit"],
  ["allow", "permit"],
  ["allowed", "permit"],
  ["allowing", "permit"],
  ["applied", "apply"],
  ["applies", "apply"],
  ["applying", "apply"],
  ["approval", "decision_state"],
  ["approvals", "decision_state"],
  ["approved", "decision_state"],
  ["decided", "decision"],
  ["decisions", "decision"],
  ["expected", "expect"],
  ["expecting", "expect"],
  ["expects", "expect"],
  ["overwrites", "overwrite"],
  ["overwriting", "overwrite"],
  ["overwritten", "overwrite"],
  ["rejected", "decision_state"],
  ["rejecting", "decision_state"],
  ["throws", "throw"],
  ["throwing", "throw"],
  ["transitions", "transition"],
]);

function semanticallyDuplicateCandidates(
  left: Candidate,
  right: Candidate,
  normalizationRevision: "v0" | "v1" | "v2",
): boolean {
  if (
    left.groupId !== right.groupId ||
    left.anchor.path !== right.anchor.path ||
    !sameTargetSide(left.anchor.side, right.anchor.side) ||
    left.anchor.startLine > right.anchor.endLine ||
    right.anchor.startLine > left.anchor.endLine
  ) {
    return false;
  }
  const leftTokens = semanticTitleTokens(left.title);
  const rightTokens = semanticTitleTokens(right.title);
  if (leftTokens.length < 4 || rightTokens.length < 4) return false;
  const rightSet = new Set(rightTokens);
  const intersection = leftTokens.filter((token) => rightSet.has(token)).length;
  const union = new Set([...leftTokens, ...rightTokens]).size;
  if (intersection >= 4 && intersection * 4 >= union * 3) return true;
  if (normalizationRevision === "v0") return false;

  // Review dimensions commonly describe the same changed behavior with
  // different vocabulary (for example "allows an approved decision to be
  // overwritten" versus "accepts a rejected decision without throwing").
  // The relaxed path remains precision-first: it requires the same code-like
  // identifier in both titles, overlapping independently validated source
  // evidence, at least four canonical semantic tokens in common, and coverage
  // of at least half of the smaller title token set. Raw claims remain intact.
  const leftIdentifiers = semanticTitleIdentifiers(left.title);
  const rightIdentifiers = new Set(semanticTitleIdentifiers(right.title));
  if (
    !leftIdentifiers.some((identifier) => rightIdentifiers.has(identifier)) ||
    !candidatesShareEvidenceRange(left, right)
  ) {
    return false;
  }
  if (
    intersection >= 4 &&
    intersection * 2 >= Math.min(leftTokens.length, rightTokens.length)
  ) {
    return true;
  }
  if (normalizationRevision === "v1") return false;

  // Titles are intentionally terse and dimension-specific. For example, one
  // reviewer may emphasize an error contract while another emphasizes the
  // state overwrite. The root-cause descriptions are allowed as a second,
  // higher-evidence signal only after the shared identifier/source gates
  // above. A large absolute intersection prevents short generic descriptions
  // from being merged, while the 55% overlap coefficient tolerates bounded
  // dimension-specific impact text.
  const leftDescription = semanticTitleTokens(left.description);
  const rightDescription = semanticTitleTokens(right.description);
  if (leftDescription.length < 12 || rightDescription.length < 12) {
    return false;
  }
  const rightDescriptionSet = new Set(rightDescription);
  const descriptionIntersection = leftDescription.filter((token) =>
    rightDescriptionSet.has(token),
  ).length;
  return (
    descriptionIntersection >= 12 &&
    descriptionIntersection * 20 >=
      Math.min(leftDescription.length, rightDescription.length) * 11
  );
}

function candidatesShareEvidenceRange(
  left: CandidateClaim,
  right: CandidateClaim,
): boolean {
  return left.evidence.some((leftEvidence) =>
    right.evidence.some(
      (rightEvidence) =>
        leftEvidence.anchor.path === rightEvidence.anchor.path &&
        sameTargetSide(leftEvidence.anchor.side, rightEvidence.anchor.side) &&
        leftEvidence.anchor.startLine <= rightEvidence.anchor.endLine &&
        rightEvidence.anchor.startLine <= leftEvidence.anchor.endLine,
    ),
  );
}

function semanticTitleIdentifiers(title: string): string[] {
  const identifiers: string[] = [];
  let current = "";
  let hasLowerToUpperBoundary = false;
  let hasUnderscore = false;
  const flush = (): void => {
    if (current.length > 1 && (hasLowerToUpperBoundary || hasUnderscore)) {
      identifiers.push(current.toLowerCase());
    }
    current = "";
    hasLowerToUpperBoundary = false;
    hasUnderscore = false;
  };
  for (const character of title) {
    const isLower = character >= "a" && character <= "z";
    const isUpper = character >= "A" && character <= "Z";
    const isDigit = character >= "0" && character <= "9";
    if (isLower || isUpper || isDigit || character === "_") {
      const previous = current.at(-1);
      if (
        isUpper &&
        previous !== undefined &&
        previous >= "a" &&
        previous <= "z"
      ) {
        hasLowerToUpperBoundary = true;
      }
      if (character === "_") hasUnderscore = true;
      current += character;
    } else {
      flush();
    }
  }
  flush();
  return [...new Set(identifiers)].sort();
}

function sameTargetSide(
  left: SourceAnchor["side"],
  right: SourceAnchor["side"],
): boolean {
  return left === right || (left !== "old" && right !== "old");
}

function semanticTitleTokens(title: string): string[] {
  const tokens: string[] = [];
  let current = "";
  const flush = (): void => {
    if (!current) return;
    let token = current;
    current = "";
    if (
      semanticTitleStopWords.has(token) ||
      (token.length > 4 && token.endsWith("ly"))
    ) {
      return;
    }
    if (token.length > 3 && token.endsWith("s")) token = token.slice(0, -1);
    token = semanticTitleAliases.get(token) ?? token;
    if (token.length > 1) tokens.push(token);
  };
  for (const character of title.toLowerCase()) {
    if (
      (character >= "a" && character <= "z") ||
      (character >= "0" && character <= "9") ||
      character === "_"
    ) {
      current += character;
    } else {
      flush();
    }
  }
  flush();
  return [...new Set(tokens)].sort();
}

function stableCandidates<T extends Candidate>(candidates: T[]): T[] {
  return [...candidates].sort(compareCandidates);
}

function compareCandidates(left: Candidate, right: Candidate): number {
  const severityRank = { critical: 0, high: 1, medium: 2, low: 3 } as const;
  return (
    severityRank[left.severity] - severityRank[right.severity] ||
    left.anchor.path.localeCompare(right.anchor.path) ||
    left.anchor.startLine - right.anchor.startLine ||
    left.title.localeCompare(right.title) ||
    left.skill.id.localeCompare(right.skill.id)
  );
}

function stableRawCandidates(
  candidates: RawCandidateRecord[],
): RawCandidateRecord[] {
  return [...candidates].sort(
    (left, right) =>
      left.groupId.localeCompare(right.groupId) ||
      left.skill.id.localeCompare(right.skill.id) ||
      left.skill.revision.localeCompare(right.skill.revision) ||
      left.ordinal - right.ordinal ||
      left.rawCandidateId.localeCompare(right.rawCandidateId),
  );
}

function stableNormalizationDecisions(
  decisions: CandidateNormalizationDecision[],
): CandidateNormalizationDecision[] {
  return [...decisions].sort((left, right) =>
    left.rawCandidateId.localeCompare(right.rawCandidateId),
  );
}

function stableFailures(failures: GroupFailure[]): GroupFailure[] {
  return [...failures].sort(
    (left, right) =>
      left.groupId.localeCompare(right.groupId) ||
      left.phase.localeCompare(right.phase) ||
      (left.skillId ?? "").localeCompare(right.skillId ?? "") ||
      (left.candidateId ?? "").localeCompare(right.candidateId ?? ""),
  );
}

function publicTarget(target: TargetSnapshot): ReviewReport["target"] {
  return {
    kind: target.kind,
    repository: target.repository,
    digest: target.digest,
    capturedAt: target.capturedAt,
    ...(target.generatedAt ? { generatedAt: target.generatedAt } : {}),
    ...(target.canonicalPatchDigest
      ? { canonicalPatchDigest: target.canonicalPatchDigest }
      : {}),
    skipped: target.skipped,
    ...(target.baseOid ? { baseOid: target.baseOid } : {}),
    ...(target.headOid ? { headOid: target.headOid } : {}),
    files: target.files.map((file) => ({
      path: file.path,
      status: file.status,
      digest: file.digest,
      changedLines: file.changedLines,
      ...(file.targetDigest ? { targetDigest: file.targetDigest } : {}),
      ...(file.patchDigest ? { patchDigest: file.patchDigest } : {}),
      ...(file.oldPath ? { oldPath: file.oldPath } : {}),
    })),
  };
}

function createDependencyCanceledObservation(
  target: TargetSnapshot,
  group: ChangeGroup,
  skill: SkillDefinition,
  at: string,
): AgentTaskObservation {
  const taskId = `agent-task-${hash([
    target.digest,
    group.id,
    "review",
    skill.id,
    skill.revision,
    "context_dependency_failed",
  ]).slice(0, 20)}`;
  return {
    taskId,
    taskKind: "review",
    groupId: group.id,
    skillId: skill.id,
    providerTurnsStarted: 0,
    providerTurnsCompleted: 0,
    toolCalls: 0,
    toolNames: [],
    toolUsage: [],
    usage: {
      completeness: "unavailable",
      unavailableReasonCode: "provider_usage_not_reported",
    },
    startedAt: at,
    finishedAt: at,
    durationMs: 0,
    terminalStatus: "canceled",
    errorCode: "dependency_failed",
  };
}

function normalizeText(value: string): string {
  return value.toLowerCase().replace(/\s+/gu, " ").trim();
}

function anchorTouchesChangedLine(
  patch: string,
  status: string,
  anchor: SourceAnchor,
): boolean {
  if (status === "full") return anchor.side !== "old";
  const changed = new Set<number>();
  const deletionAnchors = new Set<number>();
  let oldLine = 0;
  let newLine = 0;
  let inHunk = false;
  let hunkHasAddition = false;
  let hunkHasDeletion = false;
  let previousContextLine: number | undefined;
  let deletionNeedsFollowingContext = false;
  let hunkDeletionAnchors = new Set<number>();
  const finishHunk = (): void => {
    if (hunkHasDeletion && !hunkHasAddition && anchor.side !== "old") {
      for (const line of hunkDeletionAnchors) deletionAnchors.add(line);
    }
    hunkHasAddition = false;
    hunkHasDeletion = false;
    previousContextLine = undefined;
    deletionNeedsFollowingContext = false;
    hunkDeletionAnchors = new Set<number>();
  };
  for (const line of patch.split("\n")) {
    const header = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/u.exec(line);
    if (header) {
      if (inHunk) finishHunk();
      oldLine = Number(header[1]);
      newLine = Number(header[2]);
      inHunk = true;
      continue;
    }
    if (!inHunk || line.startsWith("\\ No newline")) continue;
    if (line.startsWith("+")) {
      if (anchor.side !== "old") changed.add(newLine);
      hunkHasAddition = true;
      previousContextLine = undefined;
      deletionNeedsFollowingContext = false;
      newLine++;
      continue;
    }
    if (line.startsWith("-")) {
      if (anchor.side === "old") changed.add(oldLine);
      hunkHasDeletion = true;
      if (previousContextLine !== undefined) {
        hunkDeletionAnchors.add(previousContextLine);
      }
      previousContextLine = undefined;
      deletionNeedsFollowingContext = true;
      oldLine++;
      continue;
    }
    if (line.startsWith(" ")) {
      if (deletionNeedsFollowingContext) {
        hunkDeletionAnchors.add(newLine);
      }
      deletionNeedsFollowingContext = false;
      previousContextLine = newLine;
      oldLine++;
      newLine++;
    }
  }
  if (inHunk) finishHunk();
  for (let line = anchor.startLine; line <= anchor.endLine; line++) {
    if (changed.has(line) || deletionAnchors.has(line)) return true;
  }
  return false;
}

function hash(value: unknown): string {
  return createHash("sha256").update(JSON.stringify(value)).digest("hex");
}
