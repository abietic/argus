package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/feedback"
	"argus.local/argus/internal/findingdecision"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/publication"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type fakeRuns struct {
	run              runmodel.ReviewRun
	snapshot         runmodel.ExecutionSnapshot
	extraRuns        map[string]runmodel.ReviewRun
	extraSnapshots   map[string]runmodel.ExecutionSnapshot
	artifacts        map[runmodel.ArtifactRef][]byte
	eligibilityError error
}

func (runs *fakeRuns) History(int) ([]runrepo.HistoryEntry, error) {
	return []runrepo.HistoryEntry{{
		RunID: runs.run.RunID, Status: runs.run.Status,
	}}, nil
}

func (runs *fakeRuns) LoadRun(id string) (runmodel.ReviewRun, error) {
	if run, exists := runs.extraRuns[id]; exists {
		return run, nil
	}
	if id != runs.run.RunID {
		return runmodel.ReviewRun{}, context.Canceled
	}
	return runs.run, nil
}

func (runs *fakeRuns) LoadExecutionSnapshot(id string) (runmodel.ExecutionSnapshot, error) {
	if snapshot, exists := runs.extraSnapshots[id]; exists {
		return snapshot, nil
	}
	if id != runs.snapshot.ExecutionSnapshotID {
		return runmodel.ExecutionSnapshot{}, context.Canceled
	}
	return runs.snapshot, nil
}

func (runs *fakeRuns) ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	data, ok := runs.artifacts[ref]
	if !ok {
		return nil, context.Canceled
	}
	return append([]byte(nil), data...), nil
}

func (runs *fakeRuns) CheckArtifactEligibility(
	runmodel.ArtifactRef,
	runrepo.ArtifactUse,
) error {
	return runs.eligibilityError
}

type fakeFeedback struct {
	entries []feedback.Entry
}

func (ledger *fakeFeedback) AppendFeedback(
	_ context.Context,
	fact feedback.Feedback,
) (feedback.Feedback, error) {
	copy := fact
	ledger.entries = append(ledger.entries, feedback.Entry{
		Sequence: uint64(len(ledger.entries) + 1), Kind: feedback.FactFeedback,
		Feedback: &copy,
	})
	return fact, nil
}

func (ledger *fakeFeedback) AppendOutcome(
	_ context.Context,
	fact feedback.Outcome,
) (feedback.Outcome, error) {
	copy := fact
	ledger.entries = append(ledger.entries, feedback.Entry{
		Sequence: uint64(len(ledger.entries) + 1), Kind: feedback.FactOutcome,
		Outcome: &copy,
	})
	return fact, nil
}

func (ledger *fakeFeedback) ByFinding(id string) ([]feedback.Entry, error) {
	result := make([]feedback.Entry, 0)
	for _, entry := range ledger.entries {
		if entry.Feedback != nil && entry.Feedback.FindingID == id ||
			entry.Outcome != nil && entry.Outcome.FindingID == id {
			result = append(result, entry)
		}
	}
	return result, nil
}

type fakePublications struct {
	records []publication.Record
}

func (ledger *fakePublications) ByFinding(
	runID string,
	findingID string,
) ([]publication.Record, error) {
	result := make([]publication.Record, 0)
	for _, record := range ledger.records {
		if record.Request.RunID == runID && record.Request.FindingID == findingID {
			result = append(result, record)
		}
	}
	return result, nil
}

func TestFeedbackRequiresExactFindingButNotFakeRemotePublication(t *testing.T) {
	runs, finding := validRuns(t, reviewcore.DecisionPublish)
	ledger := &fakeFeedback{entries: []feedback.Entry{}}
	service, err := New(runs, ledger, &fakePublications{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fact := feedback.Feedback{
		SchemaVersion: feedback.FeedbackSchemaVersion,
		FeedbackID:    "feedback-1", FindingID: finding.ID, RunID: runs.run.RunID,
		Action:     feedback.FeedbackAccept,
		Actor:      feedback.ActorRef{Kind: feedback.ActorHuman, ID: "user-1"},
		Source:     feedback.Source{Kind: feedback.SourceAPI, ID: "api-1"},
		OccurredAt: now, RecordedAt: now,
		SourceRefs: []feedback.SourceRef{{
			Kind: feedback.SourceRefComment, Authority: "fake", ID: "comment-1",
		}},
		IdempotencyKey: "feedback-key-1",
	}
	if _, err := service.RecordFeedback(context.Background(), fact); err != nil {
		t.Fatal(err)
	}
	detail, err := service.Finding(runs.run.RunID, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Feedback) != 1 || detail.Feedback[0].FeedbackID != fact.FeedbackID {
		t.Fatalf("Finding feedback = %#v", detail.Feedback)
	}

	fact.RunID = "another-run"
	fact.FeedbackID = "feedback-2"
	fact.IdempotencyKey = "feedback-key-2"
	if _, err := service.RecordFeedback(context.Background(), fact); err == nil {
		t.Fatal("RecordFeedback accepted a foreign run")
	}
}

func TestCodeHostFeedbackRequiresPublishedProviderComment(t *testing.T) {
	runs, finding := validRuns(t, reviewcore.DecisionReject)
	service, err := New(runs, &fakeFeedback{}, &fakePublications{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = service.RecordFeedback(context.Background(), feedback.Feedback{
		SchemaVersion: feedback.FeedbackSchemaVersion,
		FeedbackID:    "feedback-1", FindingID: finding.ID, RunID: runs.run.RunID,
		Action:     feedback.FeedbackDismiss,
		Actor:      feedback.ActorRef{Kind: feedback.ActorHuman, ID: "user-1"},
		Source:     feedback.Source{Kind: feedback.SourceCodeHost, ID: "host-1"},
		OccurredAt: now, RecordedAt: now,
		SourceRefs: []feedback.SourceRef{{
			Kind: feedback.SourceRefComment, Authority: "fake", ID: "comment-1",
		}},
		IdempotencyKey: "feedback-key-1",
	})
	if err == nil {
		t.Fatal("RecordFeedback accepted an unproven code-host publication")
	}
}

func TestCodeHostFeedbackAcceptsMatchingPublicationLedgerFact(t *testing.T) {
	runs, finding := validRuns(t, reviewcore.DecisionPublish)
	publications := &fakePublications{records: []publication.Record{{
		State: publication.StatePublished,
		Request: publication.Request{
			RunID: runs.run.RunID, FindingID: finding.ID, Provider: "fake",
		},
		ProviderResult: &publication.ProviderResult{CommentID: "comment-1"},
	}}}
	service, err := New(runs, &fakeFeedback{}, publications)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = service.RecordFeedback(context.Background(), feedback.Feedback{
		SchemaVersion: feedback.FeedbackSchemaVersion,
		FeedbackID:    "feedback-1", FindingID: finding.ID, RunID: runs.run.RunID,
		Action:     feedback.FeedbackAccept,
		Actor:      feedback.ActorRef{Kind: feedback.ActorHuman, ID: "user-1"},
		Source:     feedback.Source{Kind: feedback.SourceCodeHost, ID: "host-1"},
		OccurredAt: now, RecordedAt: now,
		SourceRefs: []feedback.SourceRef{{
			Kind: feedback.SourceRefComment, Authority: "fake", ID: "comment-1",
		}},
		IdempotencyKey: "feedback-key-1",
	})
	if err != nil {
		t.Fatalf("RecordFeedback() error = %v", err)
	}
}

func TestFormalGovernedFindingAcceptsPlatformFeedbackAndRemainsSourceTyped(t *testing.T) {
	runs, finding := validFormalRuns(t)
	ledger := &fakeFeedback{entries: []feedback.Entry{}}
	service, err := New(runs, ledger, &fakePublications{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fact := feedback.Feedback{
		SchemaVersion: feedback.FeedbackSchemaVersion,
		FeedbackID:    "formal-feedback-1", FindingID: finding.FindingID, RunID: runs.run.RunID,
		Action:     feedback.FeedbackAccept,
		Actor:      feedback.ActorRef{Kind: feedback.ActorHuman, ID: "reviewer-1"},
		Source:     feedback.Source{Kind: feedback.SourceUserInterface, ID: "argus-local-ui"},
		OccurredAt: now, RecordedAt: now,
		SourceRefs: []feedback.SourceRef{{
			Kind: feedback.SourceRefManualObservation, Authority: "argus-local",
			ID: "formal-review-observation-1",
		}},
		IdempotencyKey: "formal-feedback-key-1",
	}
	if _, err := service.RecordFeedback(context.Background(), fact); err != nil {
		t.Fatalf("RecordFeedback(formal) error = %v", err)
	}
	detail, err := service.Finding(runs.run.RunID, finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Finding != nil || detail.GovernedFinding == nil ||
		detail.GovernedFinding.FindingID != finding.FindingID ||
		len(detail.Decisions) != 0 || len(detail.GovernedDecisions) != 1 ||
		detail.GovernedDecisions[0].Action != contractsv1alpha1.GovernedFindingQueuedForHuman ||
		len(detail.Feedback) != 1 || detail.Feedback[0].FeedbackID != fact.FeedbackID {
		t.Fatalf("formal Finding detail = %+v", detail)
	}
}

func TestFormalFindingDecisionResolvesImmutableRootAndAppearsInDetail(t *testing.T) {
	runs, finding := validFormalRuns(t)
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
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	request := findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runs.run.RunID, FindingID: finding.FindingID,
		Action: findingdecision.ActionPublish, ReasonCode: "human_confirmed",
		EvidenceRefs: []findingdecision.EvidenceRef{{
			Authority: "argus-local", ID: runs.run.GovernedReportRef.URI,
			SHA256: runs.run.GovernedReportRef.SHA256,
		}},
		OccurredAt: now,
	}
	mutation := findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "formal-decision-key-1",
		Actor:          findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "approver-1"},
		Roles:          []findingdecision.Role{findingdecision.RolePublicationApprover},
		Audit:          "approved after reviewing frozen evidence", At: now.Add(time.Minute),
	}
	recorded, err := service.RecordDecision(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("record formal decision: %v", err)
	}
	if recorded.Sequence != 2 ||
		recorded.PriorDecisionID == "" ||
		recorded.SourceContract != runmodel.ContractGovernedReviewReport ||
		recorded.SourceSHA256 != runs.run.GovernedReportRef.SHA256 {
		t.Fatalf("recorded formal decision = %+v", recorded)
	}
	detail, err := service.Finding(runs.run.RunID, finding.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.GovernedDecisions) != 1 || len(detail.HumanDecisions) != 1 ||
		detail.HumanDecisions[0].DecisionID != recorded.DecisionID {
		t.Fatalf("formal decision detail = %+v", detail)
	}
	retry, err := service.RecordDecision(context.Background(), request, mutation)
	if err != nil || retry.DecisionID != recorded.DecisionID {
		t.Fatalf("formal decision retry = %+v, %v", retry, err)
	}
}

func TestReplayFindingCannotAuthorizePublication(t *testing.T) {
	runs, finding := validFormalRuns(t)
	runs.run.Kind = runmodel.RunKindReplay
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
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	_, err = service.RecordDecision(context.Background(), findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runs.run.RunID, FindingID: finding.FindingID,
		Action: findingdecision.ActionPublish, ReasonCode: "must_not_publish_replay",
		EvidenceRefs: []findingdecision.EvidenceRef{{Authority: "argus", ID: "evidence-1"}},
		OccurredAt:   now,
	}, findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "replay-publish-key",
		Actor:          findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "approver-1"},
		Roles:          []findingdecision.Role{findingdecision.RolePublicationApprover},
		Audit:          "attempted replay publication", At: now,
	})
	if err == nil || !strings.Contains(err.Error(), "replay runs cannot") {
		t.Fatalf("replay publish error = %v", err)
	}
	chain, listErr := decisions.List(runs.run.RunID, finding.FindingID)
	if listErr != nil || len(chain) != 0 {
		t.Fatalf("replay publish wrote decision: %+v, %v", chain, listErr)
	}
}

func TestPublicationRequestUsesLatestPublishDecisionAndFrozenLegacyInputs(t *testing.T) {
	runs, finding := validRuns(t, reviewcore.DecisionPublish)
	attachPublicationInputs(t, runs, false)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := findingdecision.New(store)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := publication.NewGrantRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithPublicationGrants(
		runs, &fakeFeedback{}, &fakePublications{}, decisions, grants,
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 7, 0, 0, 0, time.UTC)
	decision, err := service.RecordDecision(context.Background(), findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runs.run.RunID, FindingID: finding.ID,
		Action: findingdecision.ActionPublish, ReasonCode: "human_confirmed",
		EvidenceRefs: []findingdecision.EvidenceRef{{
			Authority: "argus-local", ID: runs.run.FindingSetRef.URI,
			SHA256: runs.run.FindingSetRef.SHA256,
		}},
		OccurredAt: now,
	}, findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "publication-authority-key-1",
		Actor:          findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "approver-1"},
		Roles:          []findingdecision.Role{findingdecision.RolePublicationApprover},
		Audit:          "approved exact finding publication", At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := recordTestPublicationGrant(
		t, service, runs.run.RunID, finding.ID, "grant-1", "publication-1",
		"publication-key-1", now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("record publication grant: %v", err)
	}
	request, err := service.BuildPublicationRequest(publication.Intent{
		SchemaVersion: publication.IntentSchemaVersion,
		GrantID:       grant.GrantID, CreatedAt: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("build publication request: %v", err)
	}
	if request.DecisionID != decision.DecisionID ||
		request.FindingSourceContract != publication.FindingSourceFindingSet ||
		request.FindingSourceRef.SHA256 != runs.run.FindingSetRef.SHA256 ||
		request.TargetSnapshotRef.SHA256 != runs.snapshot.TargetSnapshotRef.SHA256 ||
		request.ConfigBundleRef.SHA256 != runs.snapshot.ConfigBundleRef.SHA256 ||
		request.Message != finding.Message || request.RemoteWrites != "allow" ||
		request.GrantID != grant.GrantID || request.GrantSHA256 != grant.GrantSHA256 {
		t.Fatalf("publication request = %+v", request)
	}
	if err := service.AuthorizePublication(context.Background(), request); err != nil {
		t.Fatalf("authorize exact publication request: %v", err)
	}
	if err := service.AuthorizePublication(context.Background(), request); err != nil {
		t.Fatalf("authorize exact publication request idempotently: %v", err)
	}
	runs.eligibilityError = runrepo.ErrArtifactQuarantined
	if _, err := service.BuildPublicationRequest(publication.Intent{
		SchemaVersion: publication.IntentSchemaVersion,
		GrantID:       grant.GrantID, CreatedAt: now.Add(2*time.Minute + time.Second),
	}); !errors.Is(err, runrepo.ErrArtifactQuarantined) {
		t.Fatalf("quarantined publication input error = %v", err)
	}
	runs.eligibilityError = nil
	secondRequest, err := service.BuildPublicationRequest(publication.Intent{
		SchemaVersion: publication.IntentSchemaVersion,
		GrantID:       grant.GrantID, CreatedAt: request.CreatedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("build second request projection: %v", err)
	}
	if err := service.AuthorizePublication(context.Background(), secondRequest); err == nil ||
		!strings.Contains(err.Error(), publication.ErrGrantConsumed.Error()) {
		t.Fatalf("second distinct grant consumption error = %v", err)
	}

	_, err = service.RecordDecision(context.Background(), findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runs.run.RunID, FindingID: finding.ID,
		Action: findingdecision.ActionReject, ReasonCode: "approval_withdrawn",
		EvidenceRefs: []findingdecision.EvidenceRef{{Authority: "argus", ID: "withdrawal-1"}},
		OccurredAt:   now.Add(2 * time.Minute),
	}, findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "publication-authority-key-2",
		Actor:          findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "approver-1"},
		Roles:          []findingdecision.Role{findingdecision.RolePublicationApprover},
		Audit:          "withdrew publication approval", At: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BuildPublicationRequest(publication.Intent{
		SchemaVersion: publication.IntentSchemaVersion,
		GrantID:       grant.GrantID, CreatedAt: now.Add(3 * time.Minute),
	}); err == nil || !strings.Contains(err.Error(), "latest finding decision is not publish") {
		t.Fatalf("withdrawn publication build error = %v", err)
	}
	if err := service.AuthorizePublication(context.Background(), request); err == nil {
		t.Fatal("superseded publication request remained authorized")
	}
}

func TestPublicationRequestSupportsGovernedReportWithoutLegacyCoercion(t *testing.T) {
	runs, finding := validFormalRunsWithSide(t, contractsv1alpha1.HypothesisAnchorNew)
	attachPublicationInputs(t, runs, false)
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := findingdecision.New(store)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := publication.NewGrantRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewWithPublicationGrants(
		runs, &fakeFeedback{}, &fakePublications{}, decisions, grants,
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	decision, err := service.RecordDecision(context.Background(), findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runs.run.RunID, FindingID: finding.FindingID,
		Action: findingdecision.ActionPublish, ReasonCode: "formal_human_confirmed",
		EvidenceRefs: []findingdecision.EvidenceRef{{
			Authority: "argus-local", ID: runs.run.GovernedReportRef.URI,
			SHA256: runs.run.GovernedReportRef.SHA256,
		}}, OccurredAt: now,
	}, findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "formal-publication-authority-1",
		Actor:          findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "approver-1"},
		Roles:          []findingdecision.Role{findingdecision.RolePublicationApprover},
		Audit:          "approved governed formal finding", At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := recordTestPublicationGrant(
		t, service, runs.run.RunID, finding.FindingID, "formal-grant-1",
		"formal-publication-1", "formal-publication-key-1", now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("record formal publication grant: %v", err)
	}
	request, err := service.BuildPublicationRequest(publication.Intent{
		SchemaVersion: publication.IntentSchemaVersion,
		GrantID:       grant.GrantID, CreatedAt: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("build formal publication request: %v", err)
	}
	if request.DecisionID != decision.DecisionID ||
		request.FindingSourceContract != publication.FindingSourceGovernedReviewReport ||
		request.FindingSourceRef.SHA256 != runs.run.GovernedReportRef.SHA256 ||
		request.Anchor.Path != finding.Anchor.Path || request.Anchor.Side != "head" ||
		!strings.Contains(request.Message, finding.Title) ||
		!strings.Contains(request.Message, finding.Impact) {
		t.Fatalf("formal publication request = %+v", request)
	}
}

func recordTestPublicationGrant(
	t *testing.T,
	service *Service,
	runID string,
	findingID string,
	grantID string,
	publicationID string,
	publicationKey string,
	at time.Time,
) (publication.Grant, error) {
	t.Helper()
	return service.RecordPublicationGrant(context.Background(), publication.GrantRequest{
		SchemaVersion: publication.GrantRequestSchemaVersion,
		GrantID:       grantID, PublicationID: publicationID,
		PublicationIdempotencyKey: publicationKey,
		RunID:                     runID, FindingID: findingID, Channel: "pull_request_inline",
		Provider: "github", RepositoryID: "org/repository",
		ChangeKind: "pull_request", ChangeID: "42",
		ExpectedPermissionRevision: "permission-1",
		OccurredAt:                 at, ExpiresAt: at.Add(time.Hour),
	}, publication.GrantMutation{
		SchemaVersion:  publication.GrantMutationSchemaVersion,
		IdempotencyKey: "mutation-" + grantID,
		Actor:          publication.GrantActor{Kind: publication.GrantActorHuman, ID: "approver-1"},
		Roles:          []publication.GrantRole{publication.GrantRolePublicationApprover},
		Audit:          "approved exact one-time publication", At: at,
	})
}

func attachPublicationInputs(t *testing.T, runs *fakeRuns, allow bool) {
	t.Helper()
	runs.run.TargetMode = runmodel.TargetModeDiff
	runs.run.BaseRevision = "base-revision"
	runs.run.HeadRevision = "head-revision"
	runs.snapshot.ExecutionSnapshotID = runs.run.ExecutionSnapshotID
	runs.snapshot.TargetSnapshotRef = ref(runmodel.ContractMaterializedTarget, "1")
	runs.snapshot.ReviewSpecRef = ref(runmodel.ContractReviewSpec, "2")
	runs.snapshot.ConfigBundleRef = ref(runmodel.ContractConfigBundle, "3")
	spec := contractsv1alpha1.ReviewSpec{
		SchemaVersion: contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:     "publication-review-request", IdempotencyKey: "publication-review-key",
		TenantID: "tenant-1", WorkspaceID: "workspace-1",
		Repository: contractsv1alpha1.RepositoryRef{Provider: "fake", RepositoryID: "repository-1"},
		Target: contractsv1alpha1.ReviewTarget{
			Mode: contractsv1alpha1.ReviewModeDiff,
			Diff: &contractsv1alpha1.DiffTarget{
				BaseRevision: runs.run.BaseRevision, HeadRevision: runs.run.HeadRevision,
				Patch: contractsv1alpha1.ContentRef{
					URI: "artifact://local/patch", SHA256: digestString("patch"), SizeBytes: 1,
				},
			},
		},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID: "config-1", Revision: "v1", SHA256: digestString("config-ref"),
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: "workflow-1", Revision: "v1", SHA256: digestString("workflow-ref"),
		},
		RequestedOutput: []string{"findings"}, RemoteWrites: "deny",
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	specData, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := os.ReadFile("../../examples/config-bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		t.Fatal(err)
	}
	if allow {
		bundle.Publication = reviewconfig.PublicationPolicy{
			RemoteWrites: reviewconfig.PermissionAllow,
			Channels:     []string{"pull_request_inline"}, MaxComments: 8,
		}
	}
	bundle.SHA256, err = reviewconfig.DigestBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.BundleID = "bundle-" + bundle.SHA256[:24]
	if err := bundle.Validate(); err != nil {
		t.Fatal(err)
	}
	bundleData, err = json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if runs.artifacts == nil {
		runs.artifacts = make(map[runmodel.ArtifactRef][]byte)
	}
	runs.artifacts[runs.snapshot.ReviewSpecRef] = specData
	runs.artifacts[runs.snapshot.ConfigBundleRef] = bundleData
}

func validRuns(
	t *testing.T,
	action reviewcore.DecisionAction,
) (*fakeRuns, reviewcore.Finding) {
	t.Helper()
	line := "// TODO verify this"
	if action == reviewcore.DecisionReject {
		line = `const note = "// TODO test data"`
	}
	lines := []string{"package sample", line}
	content := strings.Join(lines, "\n") + "\n"
	patchLines := []string{
		"diff --git a/main.go b/main.go",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1," + strconv.Itoa(len(lines)) + " @@",
	}
	for _, sourceLine := range lines {
		patchLines = append(patchLines, "+"+sourceLine)
	}
	input := reviewcore.ReviewInput{
		SchemaVersion:  reviewcore.ReviewInputSchemaVersion,
		TargetID:       "target-fixture",
		TargetMode:     reviewcore.TargetModeDiff,
		CanonicalPatch: strings.Join(patchLines, "\n") + "\n",
		Regions:        []reviewcore.ReviewRegion{},
		Contexts:       []reviewcore.ContextBinding{},
		Files: []reviewcore.FileManifestEntry{{
			Path: "main.go", SHA256: digestString(content),
			SizeBytes: int64(len(content)), Content: &content,
		}},
	}
	result, err := reviewcore.Run(context.Background(), input, reviewcore.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Report.Findings) != 1 || len(result.Report.Decisions) != 1 ||
		result.Report.Decisions[0].Action != action {
		t.Fatalf("fixture report = %#v", result.Report)
	}
	finding := result.Report.Findings[0]
	setData, err := json.Marshal(findingSet{
		SchemaVersion: findingSetSchemaVersion,
		TargetDigest:  result.Report.TargetDigest,
		Findings:      result.Report.Findings,
		Decisions:     result.Report.Decisions,
	})
	if err != nil {
		t.Fatal(err)
	}
	findingRef := ref("argus.finding_set.v1alpha1", "b")
	run := runmodel.ReviewRun{
		SchemaVersion: runmodel.RunSchemaVersion,
		RunID:         "run-1", Kind: runmodel.RunKindReview,
		Status:              runmodel.RunStatusSucceeded,
		ExecutionSnapshotID: "snapshot-1",
		FindingSetRef:       &findingRef,
	}
	return &fakeRuns{
		run:       run,
		snapshot:  runmodel.ExecutionSnapshot{ExecutionSnapshotID: "snapshot-1"},
		artifacts: map[runmodel.ArtifactRef][]byte{findingRef: setData},
	}, finding
}

func validFormalRuns(
	t *testing.T,
) (*fakeRuns, contractsv1alpha1.GovernedReviewFinding) {
	return validFormalRunsWithSide(t, contractsv1alpha1.HypothesisAnchorFile)
}

func validFormalRunsWithSide(
	t *testing.T,
	side contractsv1alpha1.HypothesisAnchorSide,
) (*fakeRuns, contractsv1alpha1.GovernedReviewFinding) {
	t.Helper()
	anchor := contractsv1alpha1.HypothesisSourceAnchor{
		Path: "review.go", Side: side,
		StartLine: 2, EndLine: 2, SourceDigest: digestString("formal-source"),
	}
	evidence := contractsv1alpha1.HypothesisEvidence{
		EvidenceID: "formal-evidence-1", Statement: "the path reaches a nil dereference",
		Anchor: anchor, Excerpt: "return *possiblyNil", EvidenceDigest: "",
	}
	var err error
	evidence.EvidenceDigest, err = contractsv1alpha1.DigestHypothesisEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID := "formal-occurrence-1"
	fingerprint := digestString("formal-cluster")
	hypothesis := contractsv1alpha1.ReviewHypothesis{
		OccurrenceID: occurrenceID, ClusterFingerprint: fingerprint, GroupID: "formal-group-1",
		Dimension: contractsv1alpha1.VersionedRef{
			ID: "correctness", Revision: "builtin-v1", SHA256: digestString("formal-skill"),
		},
		Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
		Title: "possible nil dereference", Description: "pointer may be nil",
		Impact: "request can panic", Anchor: anchor,
		Evidence: []contractsv1alpha1.HypothesisEvidence{evidence},
		Verification: []contractsv1alpha1.HypothesisVerificationObservation{{
			ObservationID: "formal-verification-1", Sequence: 1,
			Verifier: contractsv1alpha1.VersionedRef{
				ID: "independent-verifier", Revision: "v1", SHA256: digestString("formal-verifier"),
			},
			Verdict:    contractsv1alpha1.HypothesisVerificationConfirmed,
			ReasonCode: "path_reachable", Explanation: "frozen evidence confirms the path",
			EvidenceIDs: []string{evidence.EvidenceID},
		}},
	}
	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:   contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "formal-hypothesis-set-1", PlanID: "formal-plan-1",
		SourceRunID: "formal-run-1", ExecutionID: "formal-execution-1",
		ReviewRunID: "formal-run-1", TargetDigest: digestString("formal-target"),
		Completeness: contractsv1alpha1.AgentReviewComplete, CompletenessReasons: []string{},
		NormalizationDecisions: []contractsv1alpha1.HypothesisNormalizationDecision{{
			RawCandidateID: "formal-raw-1", ClaimDigest: digestString("formal-claim"),
			Action:     contractsv1alpha1.HypothesisNormalizationRetained,
			ReasonCode: "normalized_candidate_retained", OccurrenceID: &occurrenceID,
		}},
		Hypotheses: []contractsv1alpha1.ReviewHypothesis{hypothesis},
		DedupClusters: []contractsv1alpha1.HypothesisDedupCluster{{
			ClusterID: "formal-cluster-1", Fingerprint: fingerprint,
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
	report, err := formalreview.BuildGovernedReport(set)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportRef := ref(runmodel.ContractGovernedReviewReport, "f")
	run := runmodel.ReviewRun{
		SchemaVersion: runmodel.RunSchemaVersion, RunID: set.ReviewRunID,
		Kind: runmodel.RunKindReview, Status: runmodel.RunStatusSucceeded,
		ExecutionSnapshotID: "formal-snapshot-1", GovernedReportRef: &reportRef,
	}
	return &fakeRuns{run: run, artifacts: map[runmodel.ArtifactRef][]byte{reportRef: data}}, report.Findings[0]
}

func digestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func ref(contract string, letter string) runmodel.ArtifactRef {
	digest := ""
	for len(digest) < 64 {
		digest += letter
	}
	return runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + digest,
		SHA256: digest, SizeBytes: 1, Contract: contract,
	}
}
