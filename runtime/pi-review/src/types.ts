export const REPORT_SCHEMA_VERSION = "argus.pi-review.v0" as const;

export type TargetKind = "working_changes" | "commit_diff" | "files";
export type FileStatus = "added" | "modified" | "deleted" | "renamed" | "full";
export type AnchorSide = "old" | "new" | "file";
export type Severity = "critical" | "high" | "medium" | "low";
export type Verdict = "confirmed" | "rejected" | "inconclusive";
export type ReviewStatus = "complete" | "partial" | "failed";
export type ProviderProfileName =
  | "anthropic-official"
  | "deepseek-anthropic-env";
export type ReviewProvider = "anthropic" | "deepseek-anthropic" | "fake";

export interface SkippedTarget {
  path: string;
  reason: string;
}

export interface TargetFile {
  path: string;
  oldPath?: string;
  status: FileStatus;
  patch: string;
  baseContent?: string;
  targetContent?: string;
  digest: string;
  changedLines: number;
  /**
   * Frozen-input workers bind these digests directly to the immutable input.
   * Repository-backed CLI targets leave them unset for backwards compatibility.
   */
  targetDigest?: string;
  patchDigest?: string;
  authorizedRegions?: Array<{
    startLine: number;
    endLine: number;
  }>;
}

export interface TargetSnapshot {
  kind: TargetKind;
  repository: string;
  baseOid?: string;
  headOid?: string;
  digest: string;
  capturedAt: string;
  generatedAt?: string;
  canonicalPatchDigest?: string;
  frozenContextGaps?: string[];
  files: TargetFile[];
  skipped: SkippedTarget[];
}

export interface ChangeGroup {
  id: string;
  key: string;
  files: TargetFile[];
  patch: string;
  changedLines: number;
}

export interface SourceAnchor {
  path: string;
  side: AnchorSide;
  startLine: number;
  endLine: number;
}

export interface ContextFact {
  statement: string;
  anchor?: SourceAnchor;
}

export interface EvidenceRef {
  statement: string;
  anchor: SourceAnchor;
  excerpt: string;
}

export interface ContextBundle {
  summary: string;
  relevantFiles: Array<{
    path: string;
    reason: string;
  }>;
  facts: ContextFact[];
  gaps: string[];
}

export interface SkillDefinition {
  id: string;
  revision: string;
  title: string;
  prompt: string;
  source: string;
}

export interface KnowledgeDefinition {
  id: string;
  digest: string;
  bytes: number;
}

export interface KnowledgeBundle {
  content: string;
  refs: KnowledgeDefinition[];
}

export interface CandidateClaim {
  category: string;
  severity: Severity;
  /** Reviewer self-report only. It is never exposed to the independent verifier. */
  rawConfidencePPM?: number;
  title: string;
  description: string;
  impact: string;
  anchor: SourceAnchor;
  evidence: EvidenceRef[];
  suggestion?: string;
}

export interface Candidate extends CandidateClaim {
  id: string;
  fingerprint: string;
  groupId: string;
  skill: {
    id: string;
    revision: string;
  };
}

// VerificationCandidate is the enforced blind-verifier view. Reviewer
// confidence remains in the candidate ledger but never crosses this boundary.
export type VerificationCandidate = Omit<Candidate, "rawConfidencePPM">;

export interface RawCandidateRecord {
  rawCandidateId: string;
  groupId: string;
  skill: {
    id: string;
    revision: string;
  };
  ordinal: number;
  claim: CandidateClaim;
}

export interface CandidateNormalizationDecision {
  rawCandidateId: string;
  action:
    | "retained"
    | "merged_duplicate"
    | "rejected_invalid"
    | "excluded_budget";
  reasonCode:
    | "normalized_candidate_retained"
    | "duplicate_fingerprint"
    | "semantic_duplicate"
    | "invalid_candidate"
    | "candidate_budget_exceeded";
  normalizedCandidateId?: string;
}

export interface VerificationResult {
  candidateId: string;
  verdict: Verdict;
  reasonCode: string;
  explanation: string;
  evidence: EvidenceRef[];
}

export interface Finding extends Candidate {
  verification: VerificationResult & { verdict: "confirmed" };
}

export interface GroupFailure {
  groupId: string;
  phase: "context" | "review" | "verification";
  skillId?: string;
  candidateId?: string;
  error: string;
}

export interface ReviewCoverage {
  groupsTotal: number;
  groupsReviewed: number;
  reviewTasksTotal: number;
  reviewTasksSucceeded: number;
  verificationEnabled: boolean;
  verificationTasksTotal: number;
  verificationTasksSucceeded: number;
  filesIncluded: number;
  skipped: SkippedTarget[];
  contextGaps: string[];
  failures: GroupFailure[];
  staleTarget: boolean;
}

export type AgentTaskKind = "context" | "review" | "verification";
export type AgentTaskTerminalStatus =
  | "succeeded"
  | "failed"
  | "timeout"
  | "aborted"
  | "canceled";

export interface ProviderReportedUsage {
  completeness: "provider_reported" | "partial" | "unavailable";
  unavailableReasonCode?:
    | "provider_usage_not_reported"
    | "provider_turn_incomplete"
    | "one_or_more_task_usage_unavailable";
  inputTokens?: number;
  outputTokens?: number;
  cacheReadTokens?: number;
  cacheWriteTokens?: number;
  reasoningTokens?: number;
  totalTokens?: number;
}

export interface AgentTaskObservation {
  taskId: string;
  taskKind: AgentTaskKind;
  groupId: string;
  skillId?: string;
  candidateId?: string;
  promptDigest?: string;
  providerTurnsStarted: number;
  providerTurnsCompleted: number;
  toolCalls: number;
  toolNames: string[];
  toolUsage: Array<{
    toolId: string;
    invocationCount: number;
    failureCount: number;
  }>;
  usage: ProviderReportedUsage;
  startedAt: string;
  finishedAt: string;
  durationMs: number;
  terminalStatus: AgentTaskTerminalStatus;
  errorCode?:
    | "timeout"
    | "aborted"
    | "provider_error"
    | "structured_output_missing"
    | "dependency_failed"
    | "unknown";
  outputDigest?: string;
}

export interface PiTaskEvidenceContent {
  sha256: string;
  sizeBytes: number;
  content?: string;
  omissionReason?: "evidence_budget_exceeded";
}

export interface PiTaskToolEvidence {
  sequence: number;
  toolName: string;
  arguments: PiTaskEvidenceContent;
  result?: PiTaskEvidenceContent;
  isError?: boolean;
}

export interface PiTaskExecutionEvidence {
  taskId: string;
  taskKind: AgentTaskKind;
  groupId: string;
  skillId?: string;
  candidateId?: string;
  systemPrompt: PiTaskEvidenceContent;
  userPrompt: PiTaskEvidenceContent;
  output?: PiTaskEvidenceContent;
  terminalStatus: AgentTaskTerminalStatus;
  tools: PiTaskToolEvidence[];
}

export interface PiTaskEvidenceCollection {
  schemaVersion: "argus.pi-review.task_evidence.v0";
  authority: "diagnostic_only";
  provenanceClass: "worker_self_report";
  contentPolicy: "exact_local_sensitive";
  completeness: "complete" | "partial";
  reasonCodes: Array<"evidence_budget_exceeded" | "task_evidence_unavailable">;
  tasks: PiTaskExecutionEvidence[];
}

export interface PiExecutionSnapshot {
  schemaVersion: "argus.pi-review.execution_snapshot.v0";
  snapshotDigest: string;
  createdAt: string;
  target: {
    kind: TargetKind;
    digest: string;
    baseOid?: string;
    headOid?: string;
  };
  workflowRevision:
    | "argus-pi-review-workflow-v0"
    | "argus-pi-review-workflow-v1"
    | "argus-pi-review-workflow-v2";
  promptBundleRevision: string;
  promptBundleDigest: string;
  grouping: {
    implementationRevision: "directory-language-v0";
    groups: Array<{
      id: string;
      key: string;
      patchDigest: string;
      files: string[];
    }>;
  };
  skills: Array<{
    id: string;
    revision: string;
    digest: string;
    bytes: number;
  }>;
  knowledge: Array<{
    id: string;
    digest: string;
    bytes: number;
  }>;
  runtime: {
    implementation: "@argus/pi-review";
    version: string;
    node: string;
    piAgentCore: string;
    piAi: string;
  };
  provider: {
    provider: ReviewProvider;
    profile: string;
    protocol: "anthropic-messages" | "fake";
    model: string;
    endpointDigest?: string;
    credentialRef?: "env:ANTHROPIC_API_KEY";
  };
  toolPolicy: {
    mode: "read_only";
    allowedTools: string[];
  };
  budgets: {
    concurrency: number;
    maxToolCallsPerTask: number;
    maxFiles: number;
    maxGroups: number;
    maxGroupBytes: number;
    maxTargetBytes: number;
    maxCandidates: number;
    maxProviderTurns: number;
    maxOutputTokensPerTurn: number;
    taskTimeoutMs: number;
  };
  verificationPolicy: "independent_required" | "disabled";
  replayability: {
    status: "non_replayable";
    reasons: string[];
  };
}

export interface PiExecutionEnvelope {
  authority: "diagnostic_only";
  provenanceClass: "worker_self_report";
  snapshot: PiExecutionSnapshot;
  tasks: AgentTaskObservation[];
  taskEvidence: PiTaskEvidenceCollection;
  usage: ProviderReportedUsage;
}

export interface ReviewReport {
  schemaVersion: typeof REPORT_SCHEMA_VERSION;
  status: ReviewStatus;
  provider: ReviewProvider;
  providerProfile: string;
  model: string;
  target: Omit<TargetSnapshot, "files"> & {
    files: Array<
      Pick<
        TargetFile,
        | "path"
        | "oldPath"
        | "status"
        | "digest"
        | "changedLines"
        | "targetDigest"
        | "patchDigest"
      >
    >;
    generatedAt?: string;
    canonicalPatchDigest?: string;
  };
  coverage: ReviewCoverage;
  summary: {
    candidates: number;
    confirmed: number;
    rejected: number;
    inconclusive: number;
  };
  findings: Finding[];
  candidates: Array<Candidate & { verification?: VerificationResult }>;
  rawCandidates: RawCandidateRecord[];
  normalizationDecisions: CandidateNormalizationDecision[];
  execution: PiExecutionEnvelope;
}

export interface ReviewOptions {
  repository: string;
  target:
    | { kind: "working_changes"; base?: string; paths: string[] }
    | { kind: "commit_diff"; base: string; head: string; paths: string[] }
    | { kind: "files"; revision?: string; paths: string[] };
  model: string;
  providerProfile: ProviderProfileName;
  normalizationRevision: "v0" | "v1" | "v2";
  concurrency: number;
  skills: string[];
  knowledgeFiles: string[];
  verify: boolean;
  maxToolCalls: number;
  maxGroupBytes: number;
  maxTargetBytes: number;
  maxFiles: number;
  maxGroups: number;
  maxCandidates: number;
  maxModelCalls: number;
  maxOutputTokens: number;
  timeoutMs: number;
  verbose: boolean;
}

export interface ProgressEvent {
  phase: "capture" | "context" | "review" | "verification" | "complete";
  message: string;
  groupId?: string;
  skillId?: string;
  candidateId?: string;
}
