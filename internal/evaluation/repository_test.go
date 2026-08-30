package evaluation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRepositoryCandidateIndependentReviewAdjudicationAndActivation(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	candidate := testFeedbackCase("governed-candidate", testEpoch)
	createCase(t, repository, candidate, RoleFeedbackIngest)
	assignment := CaseReviewAssignment{
		SchemaVersion: CaseReviewAssignmentSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
		ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
		Reason: "assign independent reviewers", AssignedAt: candidate.CreatedAt.Add(1),
	}
	if _, err := repository.AssignCaseReview(context.Background(), assignment,
		testMutation("assign-governed-candidate", assignment.AssignedAt, RoleDatasetCurator)); err != nil {
		t.Fatal(err)
	}

	annotation := CaseAnnotation{
		SchemaVersion:              CaseAnnotationSchemaVersion,
		CaseID:                     candidate.CaseID,
		ExpectedGovernanceRevision: 2,
		ExpectedLabelRevision:      1,
		Verdict:                    AnnotationApprove,
		ProposedLabel:              clonePointer(candidate.Label),
		LabelPolicyRevision:        candidate.LabelPolicyRevision,
		Rationale:                  "source evidence independently confirms the finding",
		EvidenceRefs:               []string{candidate.Provenance.EvidenceRefs[0]},
		ReviewedAt:                 candidate.CreatedAt.Add(2),
	}
	mutationA := testMutation("annotation-a", annotation.ReviewedAt, RoleDatasetReviewer)
	mutationA.Actor = "reviewer-a"
	entryA, err := repository.RecordCaseAnnotation(context.Background(), annotation, mutationA)
	if err != nil {
		t.Fatalf("RecordCaseAnnotation(a) error = %v", err)
	}
	if retry, err := repository.RecordCaseAnnotation(context.Background(), annotation, mutationA); err != nil ||
		!reflect.DeepEqual(retry, entryA) {
		t.Fatalf("RecordCaseAnnotation(idempotent) = %+v, %v", retry, err)
	}

	duplicateReviewer := annotation
	duplicateReviewer.ReviewedAt = candidate.CreatedAt.Add(3)
	duplicateMutation := testMutation("annotation-a-second", duplicateReviewer.ReviewedAt, RoleDatasetReviewer)
	duplicateMutation.Actor = mutationA.Actor
	if _, err := repository.RecordCaseAnnotation(context.Background(), duplicateReviewer, duplicateMutation); !errors.Is(err, ErrConflict) {
		t.Fatalf("RecordCaseAnnotation(duplicate reviewer) error = %v", err)
	}

	annotationB := annotation
	annotationB.Verdict = AnnotationReject
	annotationB.ProposedLabel = nil
	annotationB.LabelPolicyRevision = ""
	annotationB.Rationale = "the source evidence is ambiguous"
	annotationB.ReviewedAt = candidate.CreatedAt.Add(4)
	mutationB := testMutation("annotation-b", annotationB.ReviewedAt, RoleDatasetReviewer)
	mutationB.Actor = "reviewer-b"
	entryB, err := repository.RecordCaseAnnotation(context.Background(), annotationB, mutationB)
	if err != nil {
		t.Fatalf("RecordCaseAnnotation(b) error = %v", err)
	}

	adjudication := CaseAdjudication{
		SchemaVersion:              CaseAdjudicationSchemaVersion,
		CaseID:                     candidate.CaseID,
		ExpectedGovernanceRevision: 2,
		ExpectedLabelRevision:      1,
		AnnotationEventIDs:         []string{entryA.EventID, entryB.EventID},
		Outcome:                    AdjudicationApprove,
		SelectedLabel:              clonePointer(candidate.Label),
		LabelPolicyRevision:        candidate.LabelPolicyRevision,
		Rationale:                  "independent adjudicator resolved the disagreement from source evidence",
		EvidenceRefs:               []string{candidate.Provenance.EvidenceRefs[0]},
		AdjudicatedAt:              candidate.CreatedAt.Add(5),
	}
	reviewerAdjudication := testMutation("adjudicate-by-reviewer", adjudication.AdjudicatedAt, RoleDatasetAdjudicator)
	reviewerAdjudication.Actor = mutationA.Actor
	if _, err := repository.AdjudicateCase(context.Background(), adjudication, reviewerAdjudication); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("AdjudicateCase(reviewer actor) error = %v", err)
	}
	adjudicationMutation := testMutation("adjudicate-candidate", adjudication.AdjudicatedAt, RoleDatasetAdjudicator)
	adjudicationMutation.Actor = "adjudicator-c"
	record, err := repository.AdjudicateCase(context.Background(), adjudication, adjudicationMutation)
	if err != nil {
		t.Fatalf("AdjudicateCase() error = %v", err)
	}
	if record.Case.DatasetState != DatasetCandidatePool ||
		record.CurrentGovernance.DatasetState != DatasetGold ||
		record.CurrentGovernance.ReviewState != ReviewApproved ||
		record.CurrentGovernance.Revision != 3 {
		t.Fatalf("adjudicated projection = %+v", record)
	}

	activation := CaseActivation{
		SchemaVersion:              CaseActivationSchemaVersion,
		CaseID:                     candidate.CaseID,
		ExpectedGovernanceRevision: 3,
		Split:                      SplitTrain,
		Eligibility: Eligibility{
			Evaluation: true, Training: true, Promotion: true,
		},
		LicenseConsent: LicenseConsent{
			LicenseID: candidate.LicenseConsent.LicenseID,
			Consent:   candidate.LicenseConsent.Consent,
			AllowedUses: []UseScope{
				UseCandidatePool, UseEvaluation, UsePromotion, UseTraining,
			},
			Restrictions: []string{},
		},
		Reason:      "curator assigns the governed case to the training split",
		ActivatedAt: candidate.CreatedAt.Add(6),
	}
	activationMutation := testMutation("activate-candidate", activation.ActivatedAt, RoleDatasetCurator)
	activationMutation.Actor = "curator-d"
	record, err = repository.ActivateCase(context.Background(), activation, activationMutation)
	if err != nil {
		t.Fatalf("ActivateCase() error = %v", err)
	}
	if current := record.CurrentCase(); current.DatasetState != DatasetActive ||
		current.Split != SplitTrain || !current.Eligibility.Evaluation ||
		record.CurrentGovernance.Revision != 4 {
		t.Fatalf("activated projection = %+v", record)
	}
	staleActivation := activation
	staleActivation.ActivatedAt = activation.ActivatedAt.Add(1)
	if _, err := repository.ActivateCase(context.Background(), staleActivation,
		testMutation("stale-activation", staleActivation.ActivatedAt, RoleDatasetCurator)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("ActivateCase(stale) error = %v", err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	access := Access{Actor: "curator-d", Roles: []Role{RoleDatasetCurator}}
	annotations, err := restarted.AnnotationHistory(candidate.CaseID, access)
	if err != nil || len(annotations) != 2 {
		t.Fatalf("AnnotationHistory() = %+v, %v", annotations, err)
	}
	adjudications, err := restarted.AdjudicationHistory(candidate.CaseID, access)
	if err != nil || len(adjudications) != 1 {
		t.Fatalf("AdjudicationHistory() = %+v, %v", adjudications, err)
	}
}

func TestRepositoryBlindAssignmentAgreementAndReopen(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	candidate := testFeedbackCase("blind-review-candidate", testEpoch)
	createCase(t, repository, candidate, RoleFeedbackIngest)
	assignment := CaseReviewAssignment{
		SchemaVersion: CaseReviewAssignmentSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
		ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
		Reason:     "two independent reviewers must assess the candidate without seeing each other",
		AssignedAt: candidate.CreatedAt.Add(1),
	}
	assigned, err := repository.AssignCaseReview(context.Background(), assignment,
		testMutation("assign-blind-round-1", assignment.AssignedAt, RoleDatasetCurator))
	if err != nil || assigned.Assignment.CaseID != candidate.CaseID {
		t.Fatalf("AssignCaseReview() = %+v, %v", assigned, err)
	}
	annotation := CaseAnnotation{
		SchemaVersion: CaseAnnotationSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 2, ExpectedLabelRevision: 1,
		Verdict: AnnotationApprove, ProposedLabel: clonePointer(candidate.Label),
		LabelPolicyRevision: candidate.LabelPolicyRevision,
		Rationale:           "independent evidence confirms the provisional label",
		EvidenceRefs:        []string{candidate.Provenance.EvidenceRefs[0]}, ReviewedAt: candidate.CreatedAt.Add(2),
	}
	unassignedMutation := testMutation("annotation-unassigned", annotation.ReviewedAt, RoleDatasetReviewer)
	unassignedMutation.Actor = "reviewer-z"
	if _, err := repository.RecordCaseAnnotation(context.Background(), annotation, unassignedMutation); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unassigned reviewer error = %v", err)
	}
	mutationA := testMutation("blind-annotation-a", annotation.ReviewedAt, RoleDatasetReviewer)
	mutationA.Actor = "reviewer-a"
	entryA, err := repository.RecordCaseAnnotation(context.Background(), annotation, mutationA)
	if err != nil {
		t.Fatal(err)
	}
	visibleToB, err := repository.AnnotationHistory(candidate.CaseID, Access{Actor: "reviewer-b", Roles: []Role{RoleDatasetReviewer}})
	if err != nil || len(visibleToB) != 0 {
		t.Fatalf("blind annotation leakage = %+v, %v", visibleToB, err)
	}
	annotationB := annotation
	annotationB.Verdict = AnnotationReject
	annotationB.ProposedLabel = nil
	annotationB.LabelPolicyRevision = ""
	annotationB.Rationale = "the frozen evidence is insufficient"
	annotationB.ReviewedAt = candidate.CreatedAt.Add(3)
	mutationB := testMutation("blind-annotation-b", annotationB.ReviewedAt, RoleDatasetReviewer)
	mutationB.Actor = "reviewer-b"
	entryB, err := repository.RecordCaseAnnotation(context.Background(), annotationB, mutationB)
	if err != nil {
		t.Fatal(err)
	}
	agreement, err := repository.ReviewAgreement(candidate.CaseID, Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || agreement.VerdictAgreement != ReviewAgreementDisagreement ||
		agreement.CompletedReviews != 2 || agreement.ApproveCount != 1 || agreement.RejectCount != 1 {
		t.Fatalf("ReviewAgreement() = %+v, %v", agreement, err)
	}
	adjudication := CaseAdjudication{
		SchemaVersion: CaseAdjudicationSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 2, ExpectedLabelRevision: 1,
		AnnotationEventIDs: []string{entryA.EventID, entryB.EventID}, Outcome: AdjudicationApprove,
		SelectedLabel: clonePointer(candidate.Label), LabelPolicyRevision: candidate.LabelPolicyRevision,
		Rationale: "adjudicator resolves reviewer disagreement", EvidenceRefs: []string{candidate.Provenance.EvidenceRefs[0]},
		AdjudicatedAt: candidate.CreatedAt.Add(4),
	}
	adjudicator := testMutation("blind-adjudication", adjudication.AdjudicatedAt, RoleDatasetAdjudicator)
	adjudicator.Actor = "adjudicator-c"
	record, err := repository.AdjudicateCase(context.Background(), adjudication, adjudicator)
	if err != nil || record.CurrentGovernance.Revision != 3 || record.CurrentGovernance.DatasetState != DatasetGold {
		t.Fatalf("AdjudicateCase() = %+v, %v", record, err)
	}
	reopen := CaseReopen{
		SchemaVersion: CaseReopenSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 3, Reason: "new independent evidence requires another blind review round",
		EvidenceRefs: []string{candidate.Provenance.EvidenceRefs[0]}, AffectedExperimentIDs: []string{},
		ReopenedAt: candidate.CreatedAt.Add(5),
	}
	record, err = repository.ReopenCase(context.Background(), reopen,
		testMutation("reopen-blind-candidate", reopen.ReopenedAt, RoleDatasetCurator))
	if err != nil || record.CurrentGovernance.Revision != 4 ||
		record.CurrentGovernance.ReviewState != ReviewPending ||
		record.CurrentGovernance.DatasetState != DatasetCandidatePool ||
		record.CurrentGovernance.Eligibility != (Eligibility{}) {
		t.Fatalf("ReopenCase() = %+v, %v", record, err)
	}
	assignment.ExpectedGovernanceRevision = 4
	assignment.AssignedAt = candidate.CreatedAt.Add(6)
	if _, err := repository.AssignCaseReview(context.Background(), assignment,
		testMutation("assign-blind-round-2", assignment.AssignedAt, RoleDatasetCurator)); err != nil {
		t.Fatal(err)
	}
	annotation.ExpectedGovernanceRevision = 5
	annotation.ReviewedAt = candidate.CreatedAt.Add(7)
	mutationA = testMutation("blind-annotation-a-round-2", annotation.ReviewedAt, RoleDatasetReviewer)
	mutationA.Actor = "reviewer-a"
	if _, err := repository.RecordCaseAnnotation(context.Background(), annotation, mutationA); err != nil {
		t.Fatalf("same reviewer could not participate in reopened round: %v", err)
	}
}

func TestRepositoryHoldoutACLExposureAndRestart(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	holdout := testActiveCase("holdout", "repo-holdout", SplitHoldout, testEpoch)
	holdoutImport := testGovernedCaseImport(holdout)
	registerTestGovernanceKey(t, repository, holdoutImport)

	if _, err := repository.ImportGovernedCase(
		context.Background(),
		holdoutImport,
		testMutation("create-holdout-denied", holdout.CreatedAt, RoleDatasetCurator),
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ImportGovernedCase(holdout without role) error = %v", err)
	}
	createCase(t, repository, holdout, RoleHoldoutMaintainer)

	curator := Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}}
	if _, err := repository.GetCase(holdout.CaseID, curator); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("GetCase(holdout as curator) error = %v", err)
	}
	visible, err := repository.ListCases(curator)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("ListCases(curator) disclosed %d holdout cases", len(visible))
	}
	holdoutAccess := Access{Actor: "runner", Roles: []Role{RoleHoldoutRunner}}
	if _, err := repository.GetCase(holdout.CaseID, holdoutAccess); err != nil {
		t.Fatalf("GetCase(holdout runner) error = %v", err)
	}

	incomplete := testExposure(
		"run-incomplete", holdout.CaseID, holdout.CreatedAt.Add(1), ExposureNotSeen,
	)
	incomplete.Observations = incomplete.Observations[:3]
	if _, err := repository.RecordExposure(
		context.Background(),
		incomplete,
		testMutation(
			"exposure-incomplete", holdout.CreatedAt.Add(2), RoleHoldoutRunner,
		),
	); err == nil {
		t.Fatal("RecordExposure(incomplete) unexpectedly succeeded")
	}
	if err := repository.AssertHoldoutExposureComplete(
		"run-missing", []string{holdout.CaseID}, holdoutAccess,
	); !errors.Is(err, ErrContaminated) {
		t.Fatalf("AssertHoldoutExposureComplete(missing) error = %v", err)
	}

	seen := testExposure(
		"run-seen", holdout.CaseID, holdout.CreatedAt.Add(3), ExposureSeen,
	)
	if _, err := repository.RecordExposure(
		context.Background(),
		seen,
		testMutation("exposure-seen", holdout.CreatedAt.Add(4), RoleHoldoutRunner),
	); err != nil {
		t.Fatalf("RecordExposure(seen) error = %v", err)
	}
	if err := repository.AssertHoldoutExposureComplete(
		seen.EvaluationRunID, []string{holdout.CaseID}, holdoutAccess,
	); !errors.Is(err, ErrContaminated) {
		t.Fatalf("AssertHoldoutExposureComplete(seen) error = %v", err)
	}

	clean := testExposure(
		"run-clean", holdout.CaseID, holdout.CreatedAt.Add(5), ExposureNotSeen,
	)
	cleanMutation := testMutation(
		"exposure-clean", holdout.CreatedAt.Add(6), RoleHoldoutRunner,
	)
	if _, err := repository.RecordExposure(
		context.Background(), clean, cleanMutation,
	); err != nil {
		t.Fatalf("RecordExposure(clean) error = %v", err)
	}
	if err := repository.AssertHoldoutExposureComplete(
		clean.EvaluationRunID, []string{holdout.CaseID}, holdoutAccess,
	); err != nil {
		t.Fatalf("AssertHoldoutExposureComplete(clean) error = %v", err)
	}
	if _, err := repository.RecordExposure(
		context.Background(), clean, cleanMutation,
	); err != nil {
		t.Fatalf("RecordExposure(idempotent) error = %v", err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatalf("New(restart) error = %v", err)
	}
	if err := restarted.AssertHoldoutExposureComplete(
		clean.EvaluationRunID, []string{holdout.CaseID}, holdoutAccess,
	); err != nil {
		t.Fatalf("restart holdout exposure error = %v", err)
	}
	history, err := restarted.ExposureHistory(holdout.CaseID, holdoutAccess)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("exposure history length = %d, want 2", len(history))
	}
}

func TestRepositoryExternalGovernedImportRequiresExactSignatureAndTrustScope(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	active := testActiveCase("external-active", "repo-external", SplitTest, testEpoch)
	mutation := testMutation("import-external-active", active.CreatedAt, RoleDatasetCurator)
	mutation.Actor = "independent-import-operator"
	if _, err := repository.CreateCase(context.Background(), active, mutation); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("CreateCase(active) error = %v", err)
	}
	request := testGovernedCaseImport(active)
	if _, err := repository.ImportGovernedCase(context.Background(), request, mutation); !errors.Is(err, ErrUnauthorized) ||
		!strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unregistered governance key error = %v", err)
	}
	registerTestGovernanceKey(t, repository, request)
	record, err := repository.ImportGovernedCase(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("ImportGovernedCase() error = %v", err)
	}
	if record.ExternalGovernance == nil ||
		record.ExternalGovernance.Attestation.AttestationID != request.Attestation.AttestationID ||
		record.CurrentGovernance.DatasetState != DatasetActive {
		t.Fatalf("imported case record = %+v", record)
	}
	if retry, err := repository.ImportGovernedCase(context.Background(), request, mutation); err != nil ||
		!reflect.DeepEqual(retry.ExternalGovernance, record.ExternalGovernance) {
		t.Fatalf("idempotent import = %+v, %v", retry, err)
	}

	tampered := testActiveCase("external-tampered", "repo-external", SplitTest, testEpoch.Add(1))
	tamperedRequest := testGovernedCaseImport(tampered)
	tamperedRequest.Case.Owner = "tampered-after-signature"
	tamperedMutation := testMutation("import-external-tampered", tampered.CreatedAt, RoleDatasetCurator)
	if _, err := repository.ImportGovernedCase(context.Background(), tamperedRequest, tamperedMutation); err == nil {
		t.Fatal("tampered signed case was imported")
	}

	wrongScope := testActiveCase("external-wrong-scope", "repo-external", SplitTest, testEpoch.Add(2))
	wrongScopeRequest := testGovernedCaseImport(wrongScope)
	wrongScopeRequest.TrustedKey.RepositoryIDs = []string{"another-repository"}
	wrongScopeMutation := testMutation("import-external-wrong-scope", wrongScope.CreatedAt, RoleDatasetCurator)
	if _, err := repository.ImportGovernedCase(context.Background(), wrongScopeRequest, wrongScopeMutation); err == nil {
		t.Fatal("out-of-scope trusted key imported a case")
	}

	invalidChronology := testActiveCase("external-invalid-chronology", "repo-external", SplitTest, testEpoch.Add(3))
	invalidChronologyRequest := testGovernedCaseImport(invalidChronology)
	invalidChronologyRequest.Attestation.ReviewedAt = invalidChronology.CreatedAt.Add(-time.Second)
	invalidChronologyMutation := testMutation("import-external-invalid-chronology", invalidChronology.CreatedAt, RoleDatasetCurator)
	if _, err := repository.ImportGovernedCase(context.Background(), invalidChronologyRequest, invalidChronologyMutation); err == nil ||
		!strings.Contains(err.Error(), "reviewed_at cannot precede case creation") {
		t.Fatalf("invalid external governance chronology error = %v", err)
	}

	operatorConflict := testActiveCase("external-operator-conflict", "repo-external", SplitTest, testEpoch.Add(4))
	operatorRequest := testGovernedCaseImport(operatorConflict)
	operatorMutation := testMutation("import-external-operator-conflict", operatorConflict.CreatedAt, RoleDatasetCurator)
	operatorMutation.Actor = operatorRequest.Attestation.ReviewerIDs[0]
	if _, err := repository.ImportGovernedCase(context.Background(), operatorRequest, operatorMutation); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reviewer import operator error = %v", err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.GetCase(active.CaseID, Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if err != nil || restored.ExternalGovernance == nil || restored.ExternalGovernance.EventID != mutation.IdempotencyKey {
		t.Fatalf("restored external import = %+v, %v", restored, err)
	}
}

func TestRepositoryLabelCorrectionsAreAppendOnlyAndTraceExperiments(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	evaluationCase := testActiveCase("label", "repo-label", SplitTrain, testEpoch)
	createCase(t, repository, evaluationCase, RoleDatasetCurator)
	exposure := testExposure(
		"experiment-1", evaluationCase.CaseID,
		evaluationCase.CreatedAt.Add(1), ExposureNotSeen,
	)
	if _, err := repository.RecordExposure(
		context.Background(),
		exposure,
		testMutation(
			"exposure-label", evaluationCase.CreatedAt.Add(2), RoleDatasetCurator,
		),
	); err != nil {
		t.Fatalf("RecordExposure() error = %v", err)
	}

	correction := LabelCorrection{
		SchemaVersion:         LabelCorrectionSchemaVersion,
		CaseID:                evaluationCase.CaseID,
		ExpectedLabelRevision: 1,
		Label:                 testDefectLabel("high"),
		LabelPolicyRevision:   "label-policy-2",
		Reason:                "incident evidence raised severity",
		AffectedExperimentIDs: []string{"experiment-1"},
	}
	mutation := testMutation(
		"correct-label", evaluationCase.CreatedAt.Add(3), RoleDatasetCurator,
	)
	const workers = 12
	results := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := repository.CorrectLabel(context.Background(), correction, mutation)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent idempotent CorrectLabel() error = %v", err)
		}
	}
	record, err := repository.GetCase(
		evaluationCase.CaseID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.CurrentLabelRevision != 2 ||
		record.CurrentLabelPolicyRevision != "label-policy-2" ||
		record.CurrentLabel.Severity != "high" {
		t.Fatalf("current label projection = %+v", record)
	}

	conflict := correction
	conflict.Reason = "different retry payload"
	if _, err := repository.CorrectLabel(
		context.Background(), conflict, mutation,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("CorrectLabel(conflicting idempotency key) error = %v", err)
	}
	missingImpact := correction
	missingImpact.ExpectedLabelRevision = 2
	missingImpact.Label = testDefectLabel("critical")
	missingImpact.LabelPolicyRevision = "label-policy-3"
	missingImpact.AffectedExperimentIDs = []string{}
	if _, err := repository.CorrectLabel(
		context.Background(),
		missingImpact,
		testMutation(
			"correct-label-missing-impact",
			evaluationCase.CreatedAt.Add(4),
			RoleDatasetCurator,
		),
	); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("CorrectLabel(missing affected experiment) error = %v", err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatalf("New(restart) error = %v", err)
	}
	history, err := restarted.LabelHistory(
		evaluationCase.CaseID,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Revision != 1 || history[1].Revision != 2 {
		t.Fatalf("label history = %+v", history)
	}
	if len(history[1].AffectedExperimentIDs) != 1 ||
		history[1].AffectedExperimentIDs[0] != "experiment-1" {
		t.Fatalf("label correction impact = %+v", history[1].AffectedExperimentIDs)
	}
}

func TestRepositoryProductionFeedbackCanOnlyEnterCandidatePool(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	feedback := testFeedbackCase("feedback", testEpoch)
	createCase(t, repository, feedback, RoleFeedbackIngest)
	conflictingRetry := feedback
	conflictingRetry.Owner = "different-owner"
	if _, err := repository.CreateCase(
		context.Background(),
		conflictingRetry,
		testMutation(
			"create-"+feedback.CaseID, feedback.CreatedAt, RoleFeedbackIngest,
		),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateCase(conflicting idempotency key) error = %v", err)
	}

	promoted := feedback
	promoted.CaseID = "feedback-active"
	promoted.DatasetState = DatasetActive
	promoted.ReviewState = ReviewApproved
	promoted.Split = SplitTrain
	promoted.Eligibility = Eligibility{
		Evaluation:             true,
		Training:               true,
		Promotion:              true,
		ProductionDistribution: true,
	}
	promoted.LicenseConsent.AllowedUses = []UseScope{
		UseCandidatePool, UseEvaluation, UsePromotion, UseTraining,
	}
	promoted.CreatedAt = promoted.CreatedAt.Add(1)
	if _, err := repository.CreateCase(
		context.Background(),
		promoted,
		testMutation("feedback-active", promoted.CreatedAt, RoleDatasetCurator),
	); err == nil {
		t.Fatal("CreateCase(active production feedback) unexpectedly succeeded")
	}
	variant := testPromotionVariant("cross-stream", feedback.CreatedAt)
	if _, err := repository.RegisterPromotion(
		context.Background(),
		variant,
		testMutation(
			"create-"+feedback.CaseID, feedback.CreatedAt, RolePromotionOperator,
		),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("RegisterPromotion(cross-stream idempotency key) error = %v", err)
	}
}
