package evaluation

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestGovernanceBatchRecoversFromItemCheckpointAndRunsEveryGovernanceOperation(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	cases := []EvaluationCase{
		testFeedbackCase("batch-governance-a", testEpoch),
		testFeedbackCase("batch-governance-b", testEpoch),
	}
	for _, candidate := range cases {
		createCase(t, repository, candidate, RoleFeedbackIngest)
	}

	assignAt := testEpoch.Add(time.Minute)
	assignRequest := testGovernanceBatchRequest("batch-assign", GovernanceBatchAssign, assignAt,
		[]GovernanceBatchItem{
			{EventID: "batch-assign-item-a", Assignment: &CaseReviewAssignment{
				SchemaVersion: CaseReviewAssignmentSchemaVersion, CaseID: cases[0].CaseID,
				ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
				ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
				Reason: "assign blind batch review", AssignedAt: assignAt,
			}},
			{EventID: "batch-assign-item-b", Assignment: &CaseReviewAssignment{
				SchemaVersion: CaseReviewAssignmentSchemaVersion, CaseID: cases[1].CaseID,
				ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
				ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
				Reason: "assign blind batch review", AssignedAt: assignAt,
			}},
		})
	assignMutation := testMutation("batch-assign-intent", assignAt, RoleDatasetCurator)
	assignMutation.Actor = "assignment-curator"
	if _, err := repository.ensureGovernanceBatchIntent(assignRequest, assignMutation); err != nil {
		t.Fatal(err)
	}
	checkpointMutation := assignMutation
	checkpointMutation.IdempotencyKey = assignRequest.Items[0].EventID
	if _, err := repository.AssignCaseReview(
		context.Background(), *assignRequest.Items[0].Assignment, checkpointMutation,
	); err != nil {
		t.Fatal(err)
	}
	assigned, err := repository.RunGovernanceBatch(context.Background(), assignRequest, assignMutation)
	if err != nil || assigned.Status != GovernanceBatchSucceeded {
		t.Fatalf("resumed assignment batch = %+v, %v", assigned, err)
	}
	if retry, err := repository.RunGovernanceBatch(context.Background(), assignRequest, assignMutation); err != nil ||
		!reflect.DeepEqual(retry, assigned) {
		t.Fatalf("idempotent assignment batch = %+v, %v", retry, err)
	}

	annotationEventIDs := map[string][]string{
		cases[0].CaseID: {},
		cases[1].CaseID: {},
	}
	for reviewerIndex, reviewer := range []string{"reviewer-a", "reviewer-b"} {
		annotateAt := testEpoch.Add(time.Duration(reviewerIndex+2) * time.Minute)
		items := make([]GovernanceBatchItem, 0, len(cases))
		for caseIndex, candidate := range cases {
			eventID := "batch-annotation-" + reviewer + "-" + string(rune('a'+caseIndex))
			annotationEventIDs[candidate.CaseID] = append(annotationEventIDs[candidate.CaseID], eventID)
			label := cloneValue(candidate.Label)
			items = append(items, GovernanceBatchItem{EventID: eventID, Annotation: &CaseAnnotation{
				SchemaVersion: CaseAnnotationSchemaVersion, CaseID: candidate.CaseID,
				ExpectedGovernanceRevision: 2, ExpectedLabelRevision: 1,
				Verdict: AnnotationApprove, ProposedLabel: &label,
				LabelPolicyRevision: candidate.LabelPolicyRevision,
				Rationale:           "independent batch reviewer confirms source evidence",
				EvidenceRefs:        candidate.Provenance.EvidenceRefs, ReviewedAt: annotateAt,
			}})
		}
		request := testGovernanceBatchRequest(
			"batch-annotate-"+reviewer, GovernanceBatchAnnotate, annotateAt, items,
		)
		mutation := testMutation("batch-annotate-intent-"+reviewer, annotateAt, RoleDatasetReviewer)
		mutation.Actor = reviewer
		record, err := repository.RunGovernanceBatch(context.Background(), request, mutation)
		if err != nil || record.Status != GovernanceBatchSucceeded {
			t.Fatalf("annotation batch %s = %+v, %v", reviewer, record, err)
		}
		otherReviewer := "reviewer-b"
		if reviewer == otherReviewer {
			otherReviewer = "reviewer-a"
		}
		if _, err := repository.GetGovernanceBatch(request.BatchID, Access{
			Actor: otherReviewer, Roles: []Role{RoleDatasetReviewer},
		}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("blind batch disclosure for %s error = %v", reviewer, err)
		}
	}

	adjudicateAt := testEpoch.Add(4 * time.Minute)
	adjudicationItems := make([]GovernanceBatchItem, 0, len(cases))
	for caseIndex, candidate := range cases {
		label := cloneValue(candidate.Label)
		adjudicationItems = append(adjudicationItems, GovernanceBatchItem{
			EventID: "batch-adjudicate-item-" + string(rune('a'+caseIndex)),
			Adjudication: &CaseAdjudication{
				SchemaVersion: CaseAdjudicationSchemaVersion, CaseID: candidate.CaseID,
				ExpectedGovernanceRevision: 2, ExpectedLabelRevision: 1,
				AnnotationEventIDs: annotationEventIDs[candidate.CaseID], Outcome: AdjudicationApprove,
				SelectedLabel: &label, LabelPolicyRevision: candidate.LabelPolicyRevision,
				Rationale:    "independent adjudicator approves both blind reviews",
				EvidenceRefs: candidate.Provenance.EvidenceRefs, AdjudicatedAt: adjudicateAt,
			},
		})
	}
	adjudicateRequest := testGovernanceBatchRequest(
		"batch-adjudicate", GovernanceBatchAdjudicate, adjudicateAt, adjudicationItems,
	)
	adjudicateMutation := testMutation("batch-adjudicate-intent", adjudicateAt, RoleDatasetAdjudicator)
	adjudicateMutation.Actor = "adjudicator-c"
	if record, err := repository.RunGovernanceBatch(
		context.Background(), adjudicateRequest, adjudicateMutation,
	); err != nil || record.Status != GovernanceBatchSucceeded {
		t.Fatalf("adjudication batch = %+v, %v", record, err)
	}

	activateAt := testEpoch.Add(5 * time.Minute)
	activationItems := make([]GovernanceBatchItem, 0, len(cases))
	for caseIndex, candidate := range cases {
		activationItems = append(activationItems, GovernanceBatchItem{
			EventID: "batch-activate-item-" + string(rune('a'+caseIndex)),
			Activation: &CaseActivation{
				SchemaVersion: CaseActivationSchemaVersion, CaseID: candidate.CaseID,
				ExpectedGovernanceRevision: 3, Split: SplitTrain,
				Eligibility: Eligibility{Evaluation: true, Training: true, Promotion: true},
				LicenseConsent: LicenseConsent{
					LicenseID: candidate.LicenseConsent.LicenseID, Consent: candidate.LicenseConsent.Consent,
					AllowedUses:  []UseScope{UseCandidatePool, UseEvaluation, UsePromotion, UseTraining},
					Restrictions: []string{},
				},
				Reason: "activate governed batch", ActivatedAt: activateAt,
			},
		})
	}
	activateRequest := testGovernanceBatchRequest(
		"batch-activate", GovernanceBatchActivate, activateAt, activationItems,
	)
	activateMutation := testMutation("batch-activate-intent", activateAt, RoleDatasetCurator)
	activateMutation.Actor = "activation-curator"
	if record, err := repository.RunGovernanceBatch(
		context.Background(), activateRequest, activateMutation,
	); err != nil || record.Status != GovernanceBatchSucceeded {
		t.Fatalf("activation batch = %+v, %v", record, err)
	}

	reopenAt := testEpoch.Add(6 * time.Minute)
	reopenItems := make([]GovernanceBatchItem, 0, len(cases))
	for caseIndex, candidate := range cases {
		reopenItems = append(reopenItems, GovernanceBatchItem{
			EventID: "batch-reopen-item-" + string(rune('a'+caseIndex)),
			Reopen: &CaseReopen{
				SchemaVersion: CaseReopenSchemaVersion, CaseID: candidate.CaseID,
				ExpectedGovernanceRevision: 4, Reason: "new evidence requires another blind round",
				EvidenceRefs:          candidate.Provenance.EvidenceRefs,
				AffectedExperimentIDs: []string{}, ReopenedAt: reopenAt,
			},
		})
	}
	reopenRequest := testGovernanceBatchRequest("batch-reopen", GovernanceBatchReopen, reopenAt, reopenItems)
	reopenMutation := testMutation("batch-reopen-intent", reopenAt, RoleDatasetCurator)
	reopenMutation.Actor = "reopen-curator"
	if record, err := repository.RunGovernanceBatch(
		context.Background(), reopenRequest, reopenMutation,
	); err != nil || record.Status != GovernanceBatchSucceeded {
		t.Fatalf("reopen batch = %+v, %v", record, err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	access := Access{Actor: "dataset-curator", Roles: []Role{RoleDatasetCurator}}
	for _, candidate := range cases {
		record, err := restarted.GetCase(candidate.CaseID, access)
		if err != nil || record.CurrentGovernance.Revision != 5 ||
			record.CurrentGovernance.DatasetState != DatasetCandidatePool ||
			record.CurrentGovernance.ReviewState != ReviewPending {
			t.Fatalf("reopened batch case = %+v, %v", record, err)
		}
	}
	batches, err := restarted.ListGovernanceBatches(access)
	if err != nil || len(batches) != 6 {
		t.Fatalf("restored governance batches = %d, %v", len(batches), err)
	}
}

func TestGovernanceBatchPersistsExplicitPartialFailure(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	cases := []EvaluationCase{
		testFeedbackCase("batch-partial-a", testEpoch),
		testFeedbackCase("batch-partial-b", testEpoch),
	}
	for _, candidate := range cases {
		createCase(t, repository, candidate, RoleFeedbackIngest)
	}
	at := testEpoch.Add(time.Minute)
	collisionRequest := testGovernanceBatchRequest("batch-collision", GovernanceBatchAssign, at,
		[]GovernanceBatchItem{{EventID: "create-" + cases[0].CaseID, Assignment: &CaseReviewAssignment{
			SchemaVersion: CaseReviewAssignmentSchemaVersion, CaseID: cases[0].CaseID,
			ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
			ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
			Reason: "must not reuse an existing event ID", AssignedAt: at,
		}}})
	collisionMutation := testMutation("batch-collision-intent", at, RoleDatasetCurator)
	if _, err := repository.RunGovernanceBatch(
		context.Background(), collisionRequest, collisionMutation,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing item event ID collision error = %v", err)
	}
	if _, err := repository.GetGovernanceBatch(collisionRequest.BatchID, Access{
		Actor: collisionMutation.Actor, Roles: collisionMutation.Roles,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colliding request persisted an intent: %v", err)
	}
	request := testGovernanceBatchRequest("batch-partial", GovernanceBatchAssign, at,
		[]GovernanceBatchItem{
			{EventID: "batch-partial-item-a", Assignment: &CaseReviewAssignment{
				SchemaVersion: CaseReviewAssignmentSchemaVersion, CaseID: cases[0].CaseID,
				ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
				ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
				Reason: "valid first assignment", AssignedAt: at,
			}},
			{EventID: "batch-partial-item-b", Assignment: &CaseReviewAssignment{
				SchemaVersion: CaseReviewAssignmentSchemaVersion, CaseID: cases[1].CaseID,
				ExpectedGovernanceRevision: 99, ExpectedLabelRevision: 1,
				ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
				Reason: "stale second assignment", AssignedAt: at,
			}},
		})
	mutation := testMutation("batch-partial-intent", at, RoleDatasetCurator)
	mutation.Actor = "partial-curator"
	record, err := repository.RunGovernanceBatch(context.Background(), request, mutation)
	if err != nil || record.Status != GovernanceBatchFailed || record.Result == nil ||
		record.Result.FailureReason != GovernanceBatchFailureInvalidTransition ||
		record.Result.Items[0].State != GovernanceBatchItemSucceeded ||
		record.Result.Items[1].State != GovernanceBatchItemFailed {
		t.Fatalf("partial batch record = %+v, %v", record, err)
	}
	if retry, err := repository.RunGovernanceBatch(context.Background(), request, mutation); err != nil ||
		!reflect.DeepEqual(retry, record) {
		t.Fatalf("partial terminal retry = %+v, %v", retry, err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := restarted.GetCase(cases[0].CaseID, Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	second, _ := restarted.GetCase(cases[1].CaseID, Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}})
	if first.CurrentGovernance.ReviewState != ReviewInReview || second.CurrentGovernance.ReviewState != ReviewPending {
		t.Fatalf("partial batch case states = %s, %s", first.CurrentGovernance.ReviewState, second.CurrentGovernance.ReviewState)
	}
}

func testGovernanceBatchRequest(
	batchID string,
	operation GovernanceBatchOperation,
	at time.Time,
	items []GovernanceBatchItem,
) GovernanceBatchRequest {
	return GovernanceBatchRequest{
		SchemaVersion: GovernanceBatchRequestSchemaVersion, BatchID: batchID,
		Operation: operation, Items: items, TerminalEventID: batchID + "-terminal", SubmittedAt: at.UTC(),
	}
}
