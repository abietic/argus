package analyticsadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	feedbackdomain "github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestBuildGovernedFindingFactsPreservesFormalCandidateFindingAndHumanQueue(t *testing.T) {
	hypotheses := governedProjectionHypothesisSet(t)
	hypotheses.Hypotheses[0].RawConfidenceAvailable = true
	hypotheses.Hypotheses[0].RawConfidencePPM = 900_000
	verification, err := formalreview.BuildCandidateVerificationLedger(hypotheses)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := formalreview.BuildGovernedCandidateSetFromVerification(hypotheses, verification)
	if err != nil {
		t.Fatal(err)
	}
	report, err := formalreview.BuildGovernedReport(hypotheses)
	if err != nil {
		t.Fatal(err)
	}
	calibration, err := formalreview.BuildFindingCalibrationLedger(report, verification)
	if err != nil {
		t.Fatal(err)
	}
	suppression, err := formalreview.BuildFindingSuppressionLedger(report, calibration)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := hypotheses.GeneratedAt.Add(time.Minute)
	frozen := frozenRun{
		run: runmodel.ReviewRun{
			RunID: "formal-run-1", CompletedAt: &completedAt,
			HypothesisSetRef: &runmodel.ArtifactRef{
				Contract: runmodel.ContractReviewHypothesisSet, SHA256: projectionDigest("hypotheses"),
			},
			CandidateSetRef: &runmodel.ArtifactRef{
				Contract: runmodel.ContractGovernedCandidateSet, SHA256: projectionDigest("candidates"),
			},
			VerificationLedgerRef: &runmodel.ArtifactRef{
				Contract: runmodel.ContractCandidateVerificationLedger, SHA256: projectionDigest("verification"),
			},
			CalibrationLedgerRef: &runmodel.ArtifactRef{
				Contract: runmodel.ContractFindingCalibrationLedger, SHA256: projectionDigest("calibration"),
			},
			SuppressionLedgerRef: &runmodel.ArtifactRef{
				Contract: runmodel.ContractFindingSuppressionLedger, SHA256: projectionDigest("suppression"),
			},
			GovernedReportRef: &runmodel.ArtifactRef{
				Contract: runmodel.ContractGovernedReviewReport, SHA256: projectionDigest("report"),
			},
		},
		hypothesisSet: &hypotheses, candidateSet: &candidates,
		verificationLedger: &verification, governedReport: &report,
		calibrationLedger: &calibration, suppressionLedger: &suppression,
	}
	dimensions := analytics.Dimensions{
		TenantID: "tenant-1", OrganizationID: "local", RepositoryID: "repo-1",
		WorkflowRevision: "workflow-1", ConfigRevision: "config-1",
	}
	facts, summary, binding, err := buildGovernedFindingFacts(
		frozen, dimensions, analytics.CompletenessComplete, []string{}, RunSourceBinding{
			RunID: "formal-run-1", FinalRunSHA256: projectionDigest("run"),
			ExecutionSnapshotID: "execution-snapshot-1",
			ReviewSpecSHA256:    projectionDigest("spec"), TargetSnapshotSHA256: projectionDigest("target"),
			ConfigID: "config-1", ConfigRevision: "revision-1", ConfigSHA256: projectionDigest("config"),
			WorkflowID: "workflow-1", WorkflowRevision: "revision-1", WorkflowSHA256: projectionDigest("workflow"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || summary == nil || summary.Candidates != 1 ||
		summary.Normalized != 1 || summary.Verified != 1 || summary.Published != 0 {
		t.Fatalf("formal funnel = facts=%+v summary=%+v", facts, summary)
	}
	fact := facts[0]
	if !fact.Normalized || fact.FindingID == "" ||
		fact.Verification != analytics.VerificationVerified ||
		fact.PublicationEligibility != analytics.EligibilityHumanQueue ||
		fact.Publication != analytics.PublicationNotReached ||
		fact.Dimensions.Language != "go" || fact.Dimensions.RuleID != "correctness" ||
		fact.Dimensions.Path != "review.go" {
		t.Fatalf("formal finding fact = %+v", fact)
	}
	if binding.HypothesisSetSHA256 != frozen.run.HypothesisSetRef.SHA256 ||
		binding.CandidateSetSHA256 != frozen.run.CandidateSetRef.SHA256 ||
		binding.VerificationLedgerSHA256 != frozen.run.VerificationLedgerRef.SHA256 ||
		binding.CalibrationLedgerSHA256 != frozen.run.CalibrationLedgerRef.SHA256 ||
		binding.SuppressionLedgerSHA256 != frozen.run.SuppressionLedgerRef.SHA256 ||
		binding.GovernedReportSHA256 != frozen.run.GovernedReportRef.SHA256 ||
		binding.Decisions != 1 {
		t.Fatalf("formal source binding = %+v", binding)
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("formal source binding validation: %v", err)
	}
	profile, err := reviewconfig.SealCalibrationProfile("analytics-confidence", "1", []reviewconfig.CalibrationPoint{
		{RawPPM: 0, ConfidencePPM: 0}, {RawPPM: 1_000_000, ConfidencePPM: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := reviewconfig.SealFindingGovernancePolicy(profile, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	calibration, err = formalreview.BuildFindingCalibrationLedgerWithPolicy(report, verification, &policy)
	if err != nil {
		t.Fatal(err)
	}
	suppression, err = formalreview.BuildFindingSuppressionLedgerWithPolicy(report, calibration, &policy)
	if err != nil {
		t.Fatal(err)
	}
	frozen.calibrationLedger, frozen.suppressionLedger = &calibration, &suppression
	facts, _, _, err = buildGovernedFindingFacts(
		frozen, dimensions, analytics.CompletenessComplete, []string{}, RunSourceBinding{
			RunID: "formal-run-1", FinalRunSHA256: projectionDigest("run"),
			ExecutionSnapshotID: "execution-snapshot-1",
			ReviewSpecSHA256:    projectionDigest("spec"), TargetSnapshotSHA256: projectionDigest("target"),
			ConfigID: "config-1", ConfigRevision: "revision-1", ConfigSHA256: projectionDigest("config"),
			WorkflowID: "workflow-1", WorkflowRevision: "revision-1", WorkflowSHA256: projectionDigest("workflow"),
		},
	)
	if err != nil || len(facts) != 1 || facts[0].PublicationEligibility != analytics.EligibilitySuppressed {
		t.Fatalf("suppressed formal projection = %+v, %v", facts, err)
	}
	tampered := binding
	tampered.GovernedReportSHA256 = ""
	if err := tampered.Validate(); err == nil {
		t.Fatal("formal source binding accepted a missing governed report digest")
	}
	tampered = binding
	tampered.FindingSetSHA256 = projectionDigest("legacy-finding-set")
	if err := tampered.Validate(); err == nil {
		t.Fatal("formal source binding accepted mixed legacy and governed sources")
	}
}

func TestFormalHumanQueueFeedbackEntersAnalyticsOnlyAfterObservedFact(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := feedbackdomain.New(store)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &Adapter{feedback: ledger}
	completedAt := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	frozen := frozenRun{run: runmodel.ReviewRun{RunID: "formal-run-1", CompletedAt: &completedAt}}
	dimensions := analytics.Dimensions{
		TenantID: "tenant-1", OrganizationID: "local", RepositoryID: "repo-1",
		WorkflowRevision: "workflow-1", ConfigRevision: "config-1",
		Language: "go", RuleID: "correctness", Path: "review.go",
	}
	findingFacts := []analytics.FindingFunnelFact{{
		SchemaVersion: analytics.FindingFunnelFactSchemaVersion,
		FactID:        "formal-finding-fact-1", RunID: frozen.run.RunID,
		CandidateID: "formal-candidate-1", FindingID: "formal-finding-1", Normalized: true,
		Verification:           analytics.VerificationVerified,
		PublicationEligibility: analytics.EligibilityHumanQueue,
		Publication:            analytics.PublicationNotReached,
		ResultCompleteness:     analytics.CompletenessComplete, IncompleteReasons: []string{},
		Dimensions: dimensions, OccurredAt: completedAt,
	}}
	request := RebuildRequest{Window: analytics.TimeWindow{
		StartInclusive: completedAt.Add(-time.Minute), EndExclusive: completedAt.Add(time.Hour),
	}}
	facts, reasons, err := adapter.buildFeedbackFacts(
		context.Background(), request, frozen, findingFacts,
	)
	if err != nil || len(facts) != 0 || len(reasons) != 0 {
		t.Fatalf("unobserved human queue produced feedback facts: %+v, %+v, %v", facts, reasons, err)
	}
	observedAt := completedAt.Add(time.Minute)
	feedbackFact := feedbackdomain.Feedback{
		SchemaVersion: feedbackdomain.FeedbackSchemaVersion,
		FeedbackID:    "formal-feedback-1", FindingID: "formal-finding-1", RunID: frozen.run.RunID,
		Action:     feedbackdomain.FeedbackAccept,
		Actor:      feedbackdomain.ActorRef{Kind: feedbackdomain.ActorHuman, ID: "reviewer-1"},
		Source:     feedbackdomain.Source{Kind: feedbackdomain.SourceUserInterface, ID: "argus-local-ui"},
		OccurredAt: observedAt, RecordedAt: observedAt,
		SourceRefs: []feedbackdomain.SourceRef{{
			Kind: feedbackdomain.SourceRefManualObservation, Authority: "argus-local",
			ID: "formal-observation-1",
		}},
		IdempotencyKey: "formal-feedback-key-1",
	}
	if _, err := ledger.AppendFeedback(context.Background(), feedbackFact); err != nil {
		t.Fatal(err)
	}
	facts, reasons, err = adapter.buildFeedbackFacts(
		context.Background(), request, frozen, findingFacts,
	)
	if err != nil || len(reasons) != 0 || len(facts) != 1 ||
		facts[0].Feedback != analytics.FeedbackAccepted ||
		facts[0].FeedbackRef == nil || facts[0].Outcome != analytics.OutcomeNoOutcome {
		t.Fatalf("formal feedback projection = %+v, reasons=%+v, err=%v", facts, reasons, err)
	}
}

func governedProjectionHypothesisSet(t *testing.T) contractsv1alpha1.ReviewHypothesisSet {
	t.Helper()
	anchor := contractsv1alpha1.HypothesisSourceAnchor{
		Path: "review.go", Side: contractsv1alpha1.HypothesisAnchorFile,
		StartLine: 2, EndLine: 2, SourceDigest: projectionDigest("source"),
	}
	evidence := contractsv1alpha1.HypothesisEvidence{
		EvidenceID: "evidence-1", Statement: "the path reaches a nil dereference",
		Anchor: anchor, Excerpt: "return *possiblyNil", EvidenceDigest: "",
	}
	var err error
	evidence.EvidenceDigest, err = contractsv1alpha1.DigestHypothesisEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID := "occurrence-1"
	hypothesis := contractsv1alpha1.ReviewHypothesis{
		OccurrenceID: occurrenceID, ClusterFingerprint: projectionDigest("cluster"),
		GroupID: "group-1",
		Dimension: contractsv1alpha1.VersionedRef{
			ID: "correctness", Revision: "builtin-v1", SHA256: projectionDigest("skill"),
		},
		Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
		Title: "possible nil dereference", Description: "pointer may be nil",
		Impact: "request can panic", Anchor: anchor,
		Evidence: []contractsv1alpha1.HypothesisEvidence{evidence},
		Verification: []contractsv1alpha1.HypothesisVerificationObservation{{
			ObservationID: "verification-1", Sequence: 1,
			Verifier: contractsv1alpha1.VersionedRef{
				ID: "independent-verifier", Revision: "v1", SHA256: projectionDigest("verifier"),
			},
			Verdict:    contractsv1alpha1.HypothesisVerificationConfirmed,
			ReasonCode: "path_reachable", Explanation: "the frozen path reaches the dereference",
			EvidenceIDs: []string{evidence.EvidenceID},
		}},
	}
	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:   contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "hypothesis-set-1", PlanID: "plan-1",
		SourceRunID: "formal-run-1", ExecutionID: "execution-1", ReviewRunID: "formal-run-1",
		TargetDigest: projectionDigest("target"), Completeness: contractsv1alpha1.AgentReviewComplete,
		CompletenessReasons: []string{},
		NormalizationDecisions: []contractsv1alpha1.HypothesisNormalizationDecision{{
			RawCandidateID: "raw-1", ClaimDigest: projectionDigest("claim"),
			Action:     contractsv1alpha1.HypothesisNormalizationRetained,
			ReasonCode: "normalized_candidate_retained", OccurrenceID: &occurrenceID,
		}},
		Hypotheses: []contractsv1alpha1.ReviewHypothesis{hypothesis},
		DedupClusters: []contractsv1alpha1.HypothesisDedupCluster{{
			ClusterID: "cluster-1", Fingerprint: hypothesis.ClusterFingerprint,
			CanonicalOccurrenceID: occurrenceID, OccurrenceIDs: []string{occurrenceID},
		}},
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			GroupsTotal: 1, GroupsReviewed: 1, ReviewTasksTotal: 1,
			ReviewTasksSucceeded: 1, FilesIncluded: 1,
			Gaps: []contractsv1alpha1.AgentReviewCoverageGap{},
		},
		GeneratedAt: time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("formal hypothesis fixture: %v", err)
	}
	return set
}

func projectionDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
