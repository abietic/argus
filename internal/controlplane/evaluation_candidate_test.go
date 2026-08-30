package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/findingdecision"
	"github.com/abietic/argus/internal/findinglineage"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/targetmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestDeriveHumanDecisionEvaluationCandidateIsPendingAndCandidateOnly(t *testing.T) {
	runs, finding := validFormalRuns(t)
	attachPublicationInputs(t, runs, false)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := findingdecision.New(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(runs, &fakeFeedback{}, &fakePublications{}, decisions)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	decision, err := service.RecordDecision(context.Background(), findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runs.run.RunID, FindingID: finding.FindingID,
		Action: findingdecision.ActionPublish, ReasonCode: "human_confirmed",
		EvidenceRefs: []findingdecision.EvidenceRef{{
			Authority: "argus-local", ID: runs.run.GovernedReportRef.URI,
			SHA256: runs.run.GovernedReportRef.SHA256,
		}},
		OccurredAt: now,
	}, findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "candidate-source-decision", At: now,
		Actor: findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "reviewer-1"},
		Roles: []findingdecision.Role{findingdecision.RolePublicationApprover},
		Audit: "confirmed defect before dataset governance",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := candidateRequest(
		"candidate-human-1", runs.run.RunID, finding.FindingID,
		evaluation.CandidateFromHumanDecision, decision.DecisionID, now.Add(time.Minute),
	)
	candidate, err := service.DeriveEvaluationCandidate(request)
	if err != nil {
		t.Fatalf("DeriveEvaluationCandidate() error = %v", err)
	}
	if candidate.Provenance.Kind != evaluation.SourceHumanConfirmedFinding ||
		candidate.Type != evaluation.CasePositiveLocalized ||
		candidate.Label.ExpectedOutcome != evaluation.OutcomeDefectPresent ||
		candidate.ReviewState != evaluation.ReviewPending ||
		candidate.DatasetState != evaluation.DatasetCandidatePool ||
		candidate.Split != evaluation.SplitUnassigned ||
		candidate.Eligibility != (evaluation.Eligibility{}) ||
		len(candidate.LicenseConsent.AllowedUses) != 1 ||
		candidate.LicenseConsent.AllowedUses[0] != evaluation.UseCandidatePool ||
		len(candidate.Label.Anchors) != 1 || len(candidate.Label.AnchorRefs) != 1 {
		t.Fatalf("derived human candidate = %+v", candidate)
	}
	repository, err := evaluation.New(store)
	if err != nil {
		t.Fatal(err)
	}
	record, err := repository.CreateCase(context.Background(), candidate, evaluation.Mutation{
		IdempotencyKey: "persist-derived-human-candidate", Actor: "dataset-curator",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
		Audit: "accepted into candidate pool only", At: candidate.CreatedAt,
	})
	if err != nil || record.Case.CaseID != candidate.CaseID {
		t.Fatalf("CreateCase(derived) = %+v, %v", record, err)
	}
	runs.eligibilityError = runrepo.ErrArtifactQuarantined
	if _, err := service.DeriveEvaluationCandidate(request); !errors.Is(err, runrepo.ErrArtifactQuarantined) {
		t.Fatalf("quarantined candidate source error = %v", err)
	}
}

func TestDeriveDismissFeedbackCandidateRejectsSupersededAndAmbiguousFacts(t *testing.T) {
	runs, finding := validFormalRuns(t)
	attachPublicationInputs(t, runs, false)
	ledger := &fakeFeedback{}
	service, err := New(runs, ledger, &fakePublications{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	dismiss := feedback.Feedback{
		SchemaVersion: feedback.FeedbackSchemaVersion,
		FeedbackID:    "feedback-dismiss-1", FindingID: finding.FindingID, RunID: runs.run.RunID,
		Action:     feedback.FeedbackDismiss,
		Actor:      feedback.ActorRef{Kind: feedback.ActorHuman, ID: "reviewer-1"},
		Source:     feedback.Source{Kind: feedback.SourceAPI, ID: "review-api"},
		OccurredAt: now, RecordedAt: now,
		SourceRefs: []feedback.SourceRef{{
			Kind: feedback.SourceRefManualObservation, Authority: "argus-local", ID: "review-1",
		}},
		IdempotencyKey: "feedback-dismiss-key-1",
	}
	if _, err := service.RecordFeedback(context.Background(), dismiss); err != nil {
		t.Fatal(err)
	}
	request := candidateRequest(
		"candidate-feedback-1", runs.run.RunID, finding.FindingID,
		evaluation.CandidateFromProductionFeedback, dismiss.FeedbackID, now.Add(time.Minute),
	)
	candidate, err := service.DeriveEvaluationCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Provenance.Kind != evaluation.SourceProductionFeedback ||
		candidate.Type != evaluation.CaseFalsePositiveRegression ||
		candidate.Label.ExpectedOutcome != evaluation.OutcomeFalsePositive ||
		len(candidate.Label.SuppressionTargets) != 1 ||
		candidate.Label.SuppressionTargets[0].ClusterFingerprint != finding.Fingerprint ||
		len(candidate.Label.Anchors) != 0 || len(candidate.Label.AnchorRefs) != 0 {
		t.Fatalf("derived dismissal candidate = %+v", candidate)
	}

	correction := dismiss
	correction.FeedbackID = "feedback-dismiss-correction"
	correction.Action = feedback.FeedbackNeedsDiscussion
	correction.PriorFeedbackID = dismiss.FeedbackID
	correction.RecordedAt = now.Add(2 * time.Minute)
	correction.OccurredAt = correction.RecordedAt
	correction.IdempotencyKey = "feedback-dismiss-correction-key"
	if _, err := service.RecordFeedback(context.Background(), correction); err != nil {
		t.Fatal(err)
	}
	request.CollectedAt = now.Add(3 * time.Minute)
	if _, err := service.DeriveEvaluationCandidate(request); err == nil {
		t.Fatal("superseded feedback derived an evaluation candidate")
	}
	request.SourceID = correction.FeedbackID
	if _, err := service.DeriveEvaluationCandidate(request); err == nil {
		t.Fatal("ambiguous feedback derived a provisional label")
	}
}

func TestDeriveReviewedBugFixPairRequiresExactFixReviewAndLatestFixedOutcome(t *testing.T) {
	runs, finding := validFormalRuns(t)
	attachPublicationInputs(t, runs, false)
	defectCompletedAt := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	runs.run.CompletedAt = &defectCompletedAt

	fixCompletedAt := defectCompletedAt.Add(10 * time.Minute)
	fixRun := runs.run
	fixRun.RunID = "formal-fix-run-1"
	fixRun.ExecutionSnapshotID = "formal-fix-snapshot-1"
	fixRun.BaseRevision = runs.run.HeadRevision
	fixRun.HeadRevision = "fix-head-revision"
	fixRun.CompletedAt = &fixCompletedAt
	fixTargetRef := ref(runmodel.ContractMaterializedTarget, "fix-target")
	fixSpecRef := ref(runmodel.ContractReviewSpec, "fix-spec")
	fixReportRef := ref(runmodel.ContractGovernedReviewReport, "fix-report")
	fixRun.TargetSnapshotRef = fixTargetRef
	fixRun.GovernedReportRef = &fixReportRef
	fixSnapshot := runs.snapshot
	fixSnapshot.ExecutionSnapshotID = fixRun.ExecutionSnapshotID
	fixSnapshot.TargetSnapshotRef = fixTargetRef
	fixSnapshot.ReviewSpecRef = fixSpecRef

	defectSpecData := runs.artifacts[runs.snapshot.ReviewSpecRef]
	defectSpec, err := contractsv1alpha1.DecodeReviewSpec(defectSpecData)
	if err != nil {
		t.Fatal(err)
	}
	defectSpec.RequestID = "fix-review-request"
	defectSpec.IdempotencyKey = "fix-review-key"
	defectSpec.Target.Diff.BaseRevision = fixRun.BaseRevision
	defectSpec.Target.Diff.HeadRevision = fixRun.HeadRevision
	if err := defectSpec.Validate(); err != nil {
		t.Fatal(err)
	}
	fixSpecData, err := json.Marshal(defectSpec)
	if err != nil {
		t.Fatal(err)
	}
	runs.extraRuns = map[string]runmodel.ReviewRun{fixRun.RunID: fixRun}
	runs.extraSnapshots = map[string]runmodel.ExecutionSnapshot{fixSnapshot.ExecutionSnapshotID: fixSnapshot}
	runs.artifacts[fixSpecRef] = fixSpecData
	runs.artifacts[fixTargetRef] = []byte("frozen fix target")
	runs.artifacts[fixReportRef] = []byte("frozen fix review report")

	ledger := &fakeFeedback{}
	service, err := New(runs, ledger, &fakePublications{})
	if err != nil {
		t.Fatal(err)
	}
	lineageRef := ref(runmodel.ContractFindingLineage, "fix-lineage")
	service, err = service.WithFindingLineages(&fakeFindingLineages{records: []findinglineage.Record{{
		LineageRef: lineageRef,
		Lineage: contractsv1alpha1.FindingLineage{
			Policy:   contractsv1alpha1.FindingLineagePolicy{AncestryAuthority: "local_git_object_graph"},
			Baseline: contractsv1alpha1.FindingLineageRunBinding{RunID: runs.run.RunID},
			Variant:  contractsv1alpha1.FindingLineageRunBinding{RunID: fixRun.RunID},
			Relations: []contractsv1alpha1.FindingLineageRelation{{
				Type: contractsv1alpha1.FindingLineageResolved, BaselineFindingIDs: []string{finding.FindingID}, VariantFindingIDs: []string{},
			}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	outcomeAt := fixCompletedAt.Add(time.Minute)
	outcome := feedback.Outcome{
		SchemaVersion: feedback.OutcomeSchemaVersion, OutcomeID: "fixed-outcome-1",
		FindingID: finding.FindingID, RunID: runs.run.RunID, State: feedback.OutcomeFixed,
		Actor:      feedback.ActorRef{Kind: feedback.ActorService, ID: "ci-service"},
		Source:     feedback.Source{Kind: feedback.SourceCI, ID: "ci-system"},
		OccurredAt: outcomeAt, RecordedAt: outcomeAt,
		Window: feedback.AttributionWindow{Start: defectCompletedAt, End: outcomeAt},
		SourceRefs: []feedback.SourceRef{
			{Kind: feedback.SourceRefChange, Authority: "code-host", ID: "change-42", Revision: fixRun.HeadRevision},
			{Kind: feedback.SourceRefCIRun, Authority: "ci-system", ID: "ci-42", Revision: fixRun.HeadRevision},
		},
		IdempotencyKey: "fixed-outcome-key-1",
	}
	if _, err := service.RecordOutcome(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}
	request := candidateRequest(
		"candidate-fix-pair-1", runs.run.RunID, finding.FindingID,
		evaluation.CandidateFromReviewedBugFixPair, outcome.OutcomeID, outcomeAt.Add(time.Minute),
	)
	request.FixRunID = fixRun.RunID
	candidate, err := service.DeriveEvaluationCandidate(request)
	if err != nil {
		t.Fatalf("DeriveEvaluationCandidate(fix pair) error = %v", err)
	}
	if candidate.Provenance.Kind != evaluation.SourceReviewedBugFixPair ||
		candidate.Type != evaluation.CaseFixValidation ||
		candidate.Label.ExpectedOutcome != evaluation.OutcomeFixValid ||
		candidate.InputSnapshotRef != fixTargetRef.URI || len(candidate.Provenance.EvidenceRefs) != 5 ||
		!slices.Contains(candidate.Provenance.EvidenceRefs, lineageRef.URI) ||
		candidate.DatasetState != evaluation.DatasetCandidatePool {
		t.Fatalf("derived fix candidate = %+v", candidate)
	}

	badBinding := outcome
	badBinding.OutcomeID = "fixed-outcome-wrong-revision"
	badBinding.IdempotencyKey = "fixed-outcome-wrong-revision-key"
	badBinding.SourceRefs[1].Revision = "another-revision"
	badBinding.OccurredAt = outcomeAt.Add(2 * time.Minute)
	badBinding.RecordedAt = badBinding.OccurredAt
	badBinding.Window.End = badBinding.OccurredAt
	if _, err := service.RecordOutcome(context.Background(), badBinding); err != nil {
		t.Fatal(err)
	}
	request.SourceID = badBinding.OutcomeID
	request.CollectedAt = badBinding.RecordedAt.Add(time.Minute)
	if _, err := service.DeriveEvaluationCandidate(request); err == nil {
		t.Fatal("outcome without exact CI revision binding derived a bug-fix candidate")
	}

	correction := outcome
	correction.OutcomeID = "fixed-outcome-correction"
	correction.PriorOutcomeID = outcome.OutcomeID
	correction.State = feedback.OutcomeRecurred
	correction.IdempotencyKey = "fixed-outcome-correction-key"
	correction.OccurredAt = outcomeAt.Add(4 * time.Minute)
	correction.RecordedAt = correction.OccurredAt
	correction.Window.End = correction.OccurredAt
	if _, err := service.RecordOutcome(context.Background(), correction); err != nil {
		t.Fatal(err)
	}
	request.SourceID = outcome.OutcomeID
	request.CollectedAt = correction.RecordedAt.Add(time.Minute)
	if _, err := service.DeriveEvaluationCandidate(request); err == nil {
		t.Fatal("superseded fixed outcome derived a bug-fix candidate")
	}
}

type fakeFindingLineages struct{ records []findinglineage.Record }

func (source *fakeFindingLineages) List(findinglineage.ListFilter) ([]findinglineage.Record, error) {
	return slices.Clone(source.records), nil
}

func TestDeriveMissedDefectIncidentRequiresCompleteNegativeSearch(t *testing.T) {
	runs, finding := validFormalRuns(t)
	attachPublicationInputs(t, runs, false)
	completedAt := time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)
	runs.run.CompletedAt = &completedAt
	target := incidentSelectionTarget(t, finding.Anchor.SourceDigest)
	targetData, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	runs.artifacts[runs.snapshot.TargetSnapshotRef] = targetData

	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := evaluation.New(store)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDigest := digestString("incident-evidence")
	evidenceRef := runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + evidenceDigest, SHA256: evidenceDigest, SizeBytes: 1,
		Contract: "argus.external_incident_evidence.v1alpha1",
	}
	runs.artifacts[evidenceRef] = []byte("independent incident evidence")
	incident := evaluation.MissedDefectIncident{
		SchemaVersion: evaluation.MissedDefectIncidentSchemaVersion,
		IncidentID:    "incident-missed-1", ReviewRunID: runs.run.RunID,
		DefectFingerprint: digestString("independent-missed-defect"),
		Category:          "correctness", Severity: "critical",
		Anchors: []evaluation.LabelAnchor{{
			Path: finding.Anchor.Path, Side: string(finding.Anchor.Side), StartLine: 10, EndLine: 11,
			SourceDigest: finding.Anchor.SourceDigest,
		}},
		EvidenceArtifacts: []runmodel.ArtifactRef{evidenceRef},
		SourceAuthority:   "incident-system", SourceID: "INC-100", SourceRevision: "incident-revision-1",
		OccurredAt: completedAt.Add(time.Minute), RecordedAt: completedAt.Add(2 * time.Minute),
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: "record-incident-missed-1", Actor: "incident-service",
		Roles: []evaluation.Role{evaluation.RoleIncidentIngest}, Audit: "ingest independent incident evidence",
		At: incident.RecordedAt,
	}
	if _, err := sources.RecordMissedDefectIncident(context.Background(), incident, mutation); err != nil {
		t.Fatal(err)
	}
	service, err := New(runs, &fakeFeedback{}, &fakePublications{})
	if err != nil {
		t.Fatal(err)
	}
	service, err = service.WithEvaluationSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	request := candidateRequest(
		"candidate-missed-1", runs.run.RunID, "",
		evaluation.CandidateFromMissedDefectIncident, incident.IncidentID,
		incident.RecordedAt.Add(time.Minute),
	)
	candidate, err := service.DeriveEvaluationCandidate(request)
	if err != nil {
		t.Fatalf("DeriveEvaluationCandidate(incident) error = %v", err)
	}
	if candidate.Type != evaluation.CaseMissedDefectRegression ||
		candidate.Provenance.Kind != evaluation.SourceIncidentMissedDefect ||
		candidate.Label.ExpectedOutcome != evaluation.OutcomeMissedDefect ||
		candidate.Provenance.FindingID != "" || len(candidate.Label.Anchors) != 1 {
		t.Fatalf("incident candidate = %+v", candidate)
	}

	detected := incident
	detected.IncidentID = "incident-already-detected"
	detected.DefectFingerprint = finding.Fingerprint
	detected.RecordedAt = detected.RecordedAt.Add(2 * time.Minute)
	detected.SourceID = "INC-101"
	mutation.IdempotencyKey = "record-incident-detected"
	mutation.At = detected.RecordedAt
	if _, err := sources.RecordMissedDefectIncident(context.Background(), detected, mutation); err != nil {
		t.Fatal(err)
	}
	request.CaseID = "candidate-detected"
	request.SourceID = detected.IncidentID
	request.CollectedAt = detected.RecordedAt.Add(time.Minute)
	if _, err := service.DeriveEvaluationCandidate(request); err == nil {
		t.Fatal("incident already present in report derived a missed-defect candidate")
	}
}

func TestDeriveEvaluationProbeCandidatesFromIndependentOracleAndReceipt(t *testing.T) {
	runs, finding := validFormalRuns(t)
	attachPublicationInputs(t, runs, false)
	completedAt := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	runs.run.CompletedAt = &completedAt
	runs.run.TargetMode = runmodel.TargetModeSelection
	target := incidentSelectionTarget(t, finding.Anchor.SourceDigest)
	targetData, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	targetRef := probeTestArtifactRef(runmodel.ContractMaterializedTarget, targetData)
	runs.run.TargetSnapshotRef = targetRef
	runs.snapshot.TargetSnapshotRef = targetRef
	runs.artifacts[targetRef] = targetData
	baseline := incidentSelectionTarget(t, digestString("probe-baseline-source"))
	baselineData, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	baselineRef := probeTestArtifactRef(runmodel.ContractMaterializedTarget, baselineData)
	runs.artifacts[baselineRef] = baselineData
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := evaluation.New(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(runs, &fakeFeedback{}, &fakePublications{})
	if err != nil {
		t.Fatal(err)
	}
	service, err = service.WithEvaluationSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		kind           evaluation.ProbeKind
		source         evaluation.CandidateDerivationSource
		outcome        evaluation.ExpectedOutcome
		caseType       evaluation.CaseType
		provenanceKind evaluation.SourceKind
		consent        evaluation.ConsentBasis
	}{
		{"mutation", evaluation.ProbeMutationDefect, evaluation.CandidateFromMutationProbe, evaluation.OutcomeDefectPresent, evaluation.CaseMutationDiagnostic, evaluation.SourceMutation, evaluation.ConsentAuthorizedInternal},
		{"synthetic defect", evaluation.ProbeSyntheticDefect, evaluation.CandidateFromSyntheticProbe, evaluation.OutcomeDefectPresent, evaluation.CaseMutationDiagnostic, evaluation.SourceSynthetic, evaluation.ConsentSynthetic},
		{"synthetic clean", evaluation.ProbeSyntheticClean, evaluation.CandidateFromSyntheticProbe, evaluation.OutcomeClean, evaluation.CaseNegativeClean, evaluation.SourceSynthetic, evaluation.ConsentSynthetic},
		{"workflow invariant", evaluation.ProbeWorkflowInvariant, evaluation.CandidateFromWorkflowProbe, evaluation.OutcomeInvariantPass, evaluation.CaseWorkflowInvariant, evaluation.SourceSynthetic, evaluation.ConsentSynthetic},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probeID := "evaluation-probe-" + string(rune('a'+index))
			oracleContent := []byte("oracle-" + probeID)
			oracleEvidence := probeTestArtifactRef("argus.independent_probe_oracle.v1alpha1", oracleContent)
			runs.artifacts[oracleEvidence] = oracleContent
			oracle := evaluation.ProbeOracle{
				Authority: "oracle-authority-" + string(rune('a'+index)), Revision: "oracle-v1",
				ExpectedOutcome: test.outcome, Anchors: []evaluation.LabelAnchor{},
				EvidenceArtifacts: []runmodel.ArtifactRef{oracleEvidence},
			}
			if test.outcome == evaluation.OutcomeDefectPresent {
				oracle.Category = "correctness"
				oracle.Severity = "high"
				oracle.Anchors = []evaluation.LabelAnchor{{
					Path: "review.go", Side: "file", StartLine: 10, EndLine: 11,
					SourceDigest: finding.Anchor.SourceDigest,
				}}
			}
			if test.kind == evaluation.ProbeWorkflowInvariant {
				oracle.InvariantID = "no-remote-writes"
			}
			oracleSHA, err := oracle.SHA256()
			if err != nil {
				t.Fatal(err)
			}
			receiptContent := []byte("receipt-evidence-" + probeID)
			receiptEvidence := probeTestArtifactRef("argus.probe_execution_evidence.v1alpha1", receiptContent)
			runs.artifacts[receiptEvidence] = receiptContent
			receipt := evaluation.EvaluationProbeReceipt{
				SchemaVersion: evaluation.EvaluationProbeReceiptSchemaVersion,
				ProbeID:       probeID, Kind: test.kind, ReviewRunID: runs.run.RunID,
				InputTargetRef: targetRef, OracleSHA256: oracleSHA,
				MethodID: "probe-constructor", MethodRevision: "constructor-v1",
				ExecutorAuthority: "probe-executor", ExecutorRevision: "executor-v1",
				ObservedOutcome: test.outcome, EvidenceArtifacts: []runmodel.ArtifactRef{receiptEvidence},
				RemoteWrites: "deny", CompletedAt: completedAt.Add(time.Minute),
			}
			if test.kind == evaluation.ProbeMutationDefect {
				receipt.BaselineTargetRef = &baselineRef
			}
			receiptData, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			receiptRef := probeTestArtifactRef(evaluation.EvaluationProbeReceiptSchemaVersion, receiptData)
			runs.artifacts[receiptRef] = receiptData
			probe := evaluation.EvaluationProbe{
				SchemaVersion: evaluation.EvaluationProbeSchemaVersion,
				ProbeID:       probeID, Kind: test.kind, ReviewRunID: runs.run.RunID,
				Oracle: oracle, ReceiptRef: receiptRef,
				SourceAuthority: "probe-source", SourceID: "source-" + probeID,
				SourceRevision: "source-v1", RecordedAt: completedAt.Add(2 * time.Minute),
			}
			mutation := evaluation.Mutation{
				IdempotencyKey: "record-" + probeID, Actor: "probe-ingest-service",
				Roles: []evaluation.Role{evaluation.RoleProbeIngest}, Audit: "record independent evaluation probe",
				At: probe.RecordedAt,
			}
			if _, err := sources.RecordEvaluationProbe(context.Background(), probe, mutation); err != nil {
				t.Fatal(err)
			}
			request := candidateRequest("candidate-"+probeID, runs.run.RunID, "", test.source, probeID, probe.RecordedAt.Add(time.Minute))
			request.Consent = test.consent
			candidate, err := service.DeriveEvaluationCandidate(request)
			if err != nil {
				t.Fatalf("DeriveEvaluationCandidate() error = %v", err)
			}
			if candidate.Type != test.caseType || candidate.Provenance.Kind != test.provenanceKind ||
				candidate.Label.ExpectedOutcome != test.outcome || candidate.DatasetState != evaluation.DatasetCandidatePool ||
				candidate.Eligibility != (evaluation.Eligibility{}) || candidate.Provenance.FindingID != "" {
				t.Fatalf("probe candidate = %+v", candidate)
			}
			if test.outcome == evaluation.OutcomeDefectPresent && len(candidate.Label.Anchors) != 1 {
				t.Fatalf("defect probe candidate lacks oracle anchor: %+v", candidate.Label)
			}
		})
	}
}

func TestEvaluationProbeRejectsReviewOutputAsOracleEvidence(t *testing.T) {
	runs, finding := validFormalRuns(t)
	attachPublicationInputs(t, runs, false)
	completedAt := time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)
	runs.run.CompletedAt = &completedAt
	target := incidentSelectionTarget(t, finding.Anchor.SourceDigest)
	targetData, _ := json.Marshal(target)
	targetRef := probeTestArtifactRef(runmodel.ContractMaterializedTarget, targetData)
	runs.run.TargetSnapshotRef = targetRef
	runs.snapshot.TargetSnapshotRef = targetRef
	runs.artifacts[targetRef] = targetData
	oracle := evaluation.ProbeOracle{
		Authority: "independent-oracle", Revision: "v1", ExpectedOutcome: evaluation.OutcomeDefectPresent,
		Category: "correctness", Severity: "high",
		Anchors:           []evaluation.LabelAnchor{{Path: "review.go", Side: "file", StartLine: 10, EndLine: 10, SourceDigest: finding.Anchor.SourceDigest}},
		EvidenceArtifacts: []runmodel.ArtifactRef{*runs.run.GovernedReportRef},
	}
	oracleSHA, _ := oracle.SHA256()
	receiptEvidence := probeTestArtifactRef("argus.probe_execution_evidence.v1alpha1", []byte("receipt-evidence"))
	receipt := evaluation.EvaluationProbeReceipt{
		SchemaVersion: evaluation.EvaluationProbeReceiptSchemaVersion,
		ProbeID:       "self-label-probe", Kind: evaluation.ProbeSyntheticDefect, ReviewRunID: runs.run.RunID,
		InputTargetRef: targetRef, OracleSHA256: oracleSHA, MethodID: "synthetic-generator", MethodRevision: "v1",
		ExecutorAuthority: "probe-executor", ExecutorRevision: "v1", ObservedOutcome: evaluation.OutcomeDefectPresent,
		EvidenceArtifacts: []runmodel.ArtifactRef{receiptEvidence}, RemoteWrites: "deny", CompletedAt: completedAt.Add(time.Minute),
	}
	receiptData, _ := json.Marshal(receipt)
	receiptRef := probeTestArtifactRef(evaluation.EvaluationProbeReceiptSchemaVersion, receiptData)
	runs.artifacts[receiptRef] = receiptData
	store, _ := local.Open(t.TempDir())
	sources, _ := evaluation.New(store)
	probe := evaluation.EvaluationProbe{
		SchemaVersion: evaluation.EvaluationProbeSchemaVersion, ProbeID: receipt.ProbeID,
		Kind: receipt.Kind, ReviewRunID: runs.run.RunID, Oracle: oracle, ReceiptRef: receiptRef,
		SourceAuthority: "synthetic-source", SourceID: "synthetic-1", SourceRevision: "v1",
		RecordedAt: completedAt.Add(2 * time.Minute),
	}
	if _, err := sources.RecordEvaluationProbe(context.Background(), probe, evaluation.Mutation{
		IdempotencyKey: "record-self-label-probe", Actor: "probe-ingest-service",
		Roles: []evaluation.Role{evaluation.RoleProbeIngest}, Audit: "negative test", At: probe.RecordedAt,
	}); err != nil {
		t.Fatal(err)
	}
	service, _ := New(runs, &fakeFeedback{}, &fakePublications{})
	service, _ = service.WithEvaluationSources(sources)
	request := candidateRequest("candidate-self-label", runs.run.RunID, "", evaluation.CandidateFromSyntheticProbe, probe.ProbeID, probe.RecordedAt.Add(time.Minute))
	request.Consent = evaluation.ConsentSynthetic
	if _, err := service.DeriveEvaluationCandidate(request); err == nil || !strings.Contains(err.Error(), "cannot use Argus review output") {
		t.Fatalf("self-labeled probe error = %v", err)
	}
}

func probeTestArtifactRef(contract string, data []byte) runmodel.ArtifactRef {
	digest := digestString(string(data))
	return runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + digest, SHA256: digest,
		SizeBytes: int64(len(data)), Contract: contract,
	}
}

func incidentSelectionTarget(t *testing.T, sourceDigest string) targetmodel.MaterializedTarget {
	t.Helper()
	manifestDigest := digestString("incident-selection-manifest")
	selectionDigest := digestString("incident-selection-content")
	revision := gitadapter.RevisionSnapshot{Requested: "head-revision", CommitOID: strings.Repeat("d", 40)}
	snapshot, err := targetmodel.SealTargetSnapshot(targetmodel.TargetSnapshot{
		SchemaVersion: targetmodel.TargetSnapshotSchemaVersion, Mode: reviewcore.TargetModeSelection,
		Repository: gitadapter.RepositorySnapshot{Kind: "local_git", RepositoryID: "repository-1", ObjectFormat: "sha1"},
		Base:       revision, Head: revision, ManifestSHA256: manifestDigest,
		Selection: &targetmodel.SelectionSnapshot{
			Path: "review.go", StartLine: 1, EndLine: 20,
			EffectiveRanges: []targetmodel.SelectionRange{{StartLine: 1, EndLine: 20}},
			SourceKind:      targetmodel.SelectionSourceCommit, FileSHA256: sourceDigest, FileSizeBytes: 20,
			SelectionContentSHA256: selectionDigest, SelectionSizeBytes: 20,
		},
		DirtyState: gitadapter.DirtyStateClean, Completeness: gitadapter.CompletenessComplete,
		CompletenessReason: []gitadapter.Reason{}, CapturedAt: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		CapturedBy: "argus-test", GitVersion: "git version test",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactRef := func(contract, digest string, size int64) runmodel.ArtifactRef {
		return runmodel.ArtifactRef{URI: "artifact://local/sha256/" + digest, SHA256: digest, SizeBytes: size, Contract: contract}
	}
	fileRef := artifactRef(targetmodel.ContractFileContent, sourceDigest, 20)
	selectionRef := artifactRef(targetmodel.ContractSelectionContent, selectionDigest, 20)
	target := targetmodel.MaterializedTarget{
		SchemaVersion: targetmodel.MaterializedTargetSchemaVersion, Snapshot: snapshot,
		ManifestRef:         artifactRef(targetmodel.ContractSelectionManifest, manifestDigest, 100),
		SelectionContentRef: &selectionRef, Contexts: []reviewcore.ContextBinding{},
		FileRefs: []targetmodel.TargetFileRef{{
			Path: "review.go", SHA256: sourceDigest, SizeBytes: 20, ContentRef: &fileRef,
			Completeness: gitadapter.CompletenessComplete, Reasons: []gitadapter.Reason{},
		}},
	}
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	return target
}

func candidateRequest(
	caseID string,
	runID string,
	findingID string,
	source evaluation.CandidateDerivationSource,
	sourceID string,
	collectedAt time.Time,
) evaluation.CandidateDerivationRequest {
	return evaluation.CandidateDerivationRequest{
		SchemaVersion: evaluation.CandidateDerivationRequestSchemaVersion,
		CaseID:        caseID, RunID: runID, FindingID: findingID,
		Source: source, SourceID: sourceID,
		LicenseID:      "internal-review-data-policy-1",
		Consent:        evaluation.ConsentAuthorizedInternal,
		Classification: evaluation.ClassificationInternal,
		Owner:          "evaluation-governance", LabelPolicyRevision: "candidate-label-policy-1",
		Restrictions: []string{"candidate review required before activation"},
		CollectedAt:  collectedAt,
	}
}
