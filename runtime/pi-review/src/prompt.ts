import { createHash } from "node:crypto";

export const PROMPT_BUNDLE_SCHEMA_VERSION =
  "argus.agent_review_prompt_bundle.v1alpha1" as const;
export const MAX_PROMPT_BUNDLE_BYTES = 64 * 1024;
export const MAX_PROMPT_FIELD_BYTES = 16 * 1024;

export interface PiReviewPromptBundle {
  schema_version: typeof PROMPT_BUNDLE_SCHEMA_VERSION;
  revision: string;
  context_system_prompt: string;
  review_system_prompt: string;
  verification_system_prompt: string;
  terminal_finalizer_prompt: string;
}

export interface OperationalPromptBundle {
  revision: string;
  digest: string;
  content: PiReviewPromptBundle;
  governed: boolean;
}

export const DEFAULT_PROMPT_BUNDLE: PiReviewPromptBundle = {
  schema_version: PROMPT_BUNDLE_SCHEMA_VERSION,
  revision: "v0",
  context_system_prompt:
    "You are the context collector. Build the smallest evidence-backed context needed to review this change group. Do not decide whether a defect exists.",
  review_system_prompt:
    "You are an evidence-driven code reviewer. Find real defects introduced or exposed by the supplied target, not style issues. A candidate must include a concrete trigger, impact and source anchor inside this change group.",
  verification_system_prompt:
    "You are an independent defect verifier. Try to falsify the supplied claim using source evidence. Confirm only when a concrete triggering path and impact remain after checking relevant code; reject disproven claims; use inconclusive when required evidence is unavailable.",
  terminal_finalizer_prompt:
    "Repository exploration is closed. Call the required terminal submit tool now with the best evidence-backed result allowed by its schema. Do not call any other tool or answer with prose.",
};

export const DEFAULT_OPERATIONAL_PROMPT_BUNDLE: OperationalPromptBundle = {
  revision: DEFAULT_PROMPT_BUNDLE.revision,
  digest: createHash("sha256")
    .update(JSON.stringify(DEFAULT_PROMPT_BUNDLE))
    .digest("hex"),
  content: DEFAULT_PROMPT_BUNDLE,
  governed: false,
};
