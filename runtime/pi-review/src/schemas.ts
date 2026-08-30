import { Type, type Static } from "@earendil-works/pi-ai";

export const AnchorSchema = Type.Object(
  {
    path: Type.String({ minLength: 1, maxLength: 4096 }),
    side: Type.Union([
      Type.Literal("old"),
      Type.Literal("new"),
      Type.Literal("file"),
    ]),
    startLine: Type.Integer({ minimum: 1 }),
    endLine: Type.Integer({ minimum: 1 }),
  },
  { additionalProperties: false },
);

export const ContextBundleSchema = Type.Object(
  {
    summary: Type.String({ minLength: 1, maxLength: 4000 }),
    relevantFiles: Type.Array(
      Type.Object(
        {
          path: Type.String({ minLength: 1 }),
          reason: Type.String({ minLength: 1, maxLength: 1000 }),
        },
        { additionalProperties: false },
      ),
      { maxItems: 30 },
    ),
    facts: Type.Array(
      Type.Object(
        {
          statement: Type.String({ minLength: 1, maxLength: 2000 }),
          anchor: Type.Optional(AnchorSchema),
        },
        { additionalProperties: false },
      ),
      { maxItems: 50 },
    ),
    gaps: Type.Array(Type.String({ minLength: 1, maxLength: 1000 }), {
      maxItems: 20,
    }),
  },
  { additionalProperties: false },
);

export const EvidenceRefSchema = Type.Object(
  {
    statement: Type.String({ minLength: 1, maxLength: 2000 }),
    anchor: AnchorSchema,
    excerpt: Type.String({ minLength: 1, maxLength: 4000 }),
  },
  { additionalProperties: false },
);

export const CandidateClaimSchema = Type.Object(
  {
    category: Type.String({ minLength: 1, maxLength: 100 }),
    severity: Type.Union([
      Type.Literal("critical"),
      Type.Literal("high"),
      Type.Literal("medium"),
      Type.Literal("low"),
    ]),
    rawConfidencePPM: Type.Optional(
      Type.Integer({ minimum: 0, maximum: 1_000_000 }),
    ),
    title: Type.String({ minLength: 1, maxLength: 240 }),
    description: Type.String({ minLength: 1, maxLength: 4000 }),
    impact: Type.String({ minLength: 1, maxLength: 2000 }),
    anchor: AnchorSchema,
    evidence: Type.Array(EvidenceRefSchema, { minItems: 1, maxItems: 12 }),
    suggestion: Type.Optional(Type.String({ minLength: 1, maxLength: 3000 })),
  },
  { additionalProperties: false },
);

export const ReviewSubmissionSchema = Type.Object(
  {
    summary: Type.String({ minLength: 1, maxLength: 3000 }),
    candidates: Type.Array(CandidateClaimSchema, { maxItems: 20 }),
  },
  { additionalProperties: false },
);

export const VerificationSubmissionSchema = Type.Object(
  {
    candidateId: Type.String({ minLength: 1, maxLength: 256 }),
    verdict: Type.Union([
      Type.Literal("confirmed"),
      Type.Literal("rejected"),
      Type.Literal("inconclusive"),
    ]),
    reasonCode: Type.String({ minLength: 1, maxLength: 100 }),
    explanation: Type.String({ minLength: 1, maxLength: 4000 }),
    evidence: Type.Array(EvidenceRefSchema, { maxItems: 12 }),
  },
  { additionalProperties: false },
);

export type ContextSubmission = Static<typeof ContextBundleSchema>;
export type ReviewSubmission = Static<typeof ReviewSubmissionSchema>;
export type VerificationSubmission = Static<
  typeof VerificationSubmissionSchema
>;
