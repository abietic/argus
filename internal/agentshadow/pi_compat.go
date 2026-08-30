package agentshadow

import (
	"time"

	"github.com/abietic/argus/internal/pireviewmap"
	"github.com/abietic/argus/internal/reviewcore"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// These aliases keep the existing adversarial shadow fixtures source
// compatible after the production-neutral report decoder and mapper moved to
// pireviewmap. They do not define a second wire protocol.
const piReviewReportSchemaVersion = pireviewmap.PiReviewReportSchemaVersion

type piReviewReport = pireviewmap.PiReviewReport
type piTarget = pireviewmap.PiTarget
type piTargetFile = pireviewmap.PiTargetFile
type piSkipped = pireviewmap.PiSkipped
type piCoverage = pireviewmap.PiCoverage
type piFailure = pireviewmap.PiFailure
type piSummary = pireviewmap.PiSummary
type piSourceAnchor = pireviewmap.PiSourceAnchor
type piEvidence = pireviewmap.PiEvidence
type piCandidateClaim = pireviewmap.PiCandidateClaim
type piSkillRef = pireviewmap.PiSkillRef
type piRawCandidate = pireviewmap.PiRawCandidate
type piVerification = pireviewmap.PiVerification
type piCandidate = pireviewmap.PiCandidate
type piNormalizationDecision = pireviewmap.PiNormalizationDecision
type piExecutionEnvelope = pireviewmap.PiExecutionEnvelope
type piTaskEvidenceCollection = pireviewmap.PiTaskEvidenceCollection
type piTaskExecutionEvidence = pireviewmap.PiTaskExecutionEvidence
type piTaskToolEvidence = pireviewmap.PiTaskToolEvidence
type piTaskEvidenceContent = pireviewmap.PiTaskEvidenceContent
type piExecutionSnapshot = pireviewmap.PiExecutionSnapshot
type piSnapshotTarget = pireviewmap.PiSnapshotTarget
type piSnapshotGrouping = pireviewmap.PiSnapshotGrouping
type piSnapshotGroup = pireviewmap.PiSnapshotGroup
type piSnapshotSkill = pireviewmap.PiSnapshotSkill
type piSnapshotKnowledge = pireviewmap.PiSnapshotKnowledge
type piSnapshotRuntime = pireviewmap.PiSnapshotRuntime
type piSnapshotProvider = pireviewmap.PiSnapshotProvider
type piSnapshotToolPolicy = pireviewmap.PiSnapshotToolPolicy
type piSnapshotBudgets = pireviewmap.PiSnapshotBudgets
type piReplayability = pireviewmap.PiReplayability
type piTaskObservation = pireviewmap.PiTaskObservation
type piToolUsage = pireviewmap.PiToolUsage
type piUsage = pireviewmap.PiUsage

func mapPiReviewReport(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	completedAt time.Time,
	reportData []byte,
) (
	contractsv1alpha1.ReviewHypothesisSet,
	contractsv1alpha1.AgentExecutionReceiptCollection,
	error,
) {
	return pireviewmap.MapShadowReviewReport(plan, input, completedAt, reportData)
}

func decodePiReviewReport(data []byte) (piReviewReport, error) {
	return pireviewmap.DecodePiReviewReport(data)
}

func piClusterFingerprint(claim piCandidateClaim) (string, error) {
	return pireviewmap.PiClusterFingerprint(claim)
}

func piProfileForProvider(provider string) (string, error) {
	return pireviewmap.PiProfileForProvider(provider)
}

func frozenPiFilePatch(
	input reviewcore.ReviewInput,
	path string,
) (string, string, uint32, error) {
	return pireviewmap.FrozenPiFilePatch(input, path)
}

func jsEncodeURIComponent(value string) string {
	return pireviewmap.JSEncodeURIComponent(value)
}
