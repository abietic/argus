package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	feedbackdomain "argus.local/argus/internal/feedback"
	"argus.local/argus/internal/findingdecision"
	"argus.local/argus/internal/publication"
	"argus.local/argus/internal/publication/githubadapter"
	"argus.local/argus/internal/runmodel"
)

type cliUnknownPublicationProvider struct {
	revalidateCalls int
	revalidateFails int
	publishCalls    int
	lookupCalls     int
}

func (provider *cliUnknownPublicationProvider) Revalidate(
	_ context.Context,
	request publication.Request,
) (publication.Revalidation, error) {
	provider.revalidateCalls++
	if provider.revalidateFails > 0 {
		provider.revalidateFails--
		return publication.Revalidation{}, errors.New("temporary GitHub preflight failure")
	}
	return publication.Revalidation{
		SchemaVersion:        publication.RevalidationSchemaVersion,
		ProviderBaseRevision: request.BaseRevision,
		ProviderHeadRevision: request.ExpectedHeadRevision,
		PermissionGranted:    true, PermissionRevision: request.ExpectedPermissionRevision,
		ConfigBundleSHA256:  request.ConfigBundleRef.SHA256,
		FindingSourceSHA256: request.FindingSourceRef.SHA256,
		FindingCurrent:      true, AnchorCurrent: true,
		CheckedAt: request.CreatedAt.Add(time.Second),
	}, nil
}

func (provider *cliUnknownPublicationProvider) Publish(
	_ context.Context,
	request publication.ProviderRequest,
) (publication.ProviderResult, error) {
	provider.publishCalls++
	return publication.ProviderResult{}, errors.New("response lost after provider accepted create")
}

func (provider *cliUnknownPublicationProvider) Lookup(
	_ context.Context,
	request publication.ProviderRequest,
) (publication.ProviderResult, error) {
	provider.lookupCalls++
	return publication.ProviderResult{
		SchemaVersion:     publication.ProviderResultSchema,
		Status:            publication.ProviderResultPublished,
		IdempotencyKey:    request.IdempotencyKey,
		ProviderRequestID: "provider-request-1", CommentID: "comment-1",
		ObservedAt: time.Now().UTC(),
	}, nil
}

func TestControlPlaneFindingFeedbackAndOutcomeCLI(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(
		t,
		repositoryPath,
		"reviewed.go",
		"package fixture\n// ARGUS_BUG control plane\n",
	)
	revision := commitCLITarget(t, repositoryPath, "control-plane target")
	store := t.TempDir()
	var reviewOutput bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{
			"review",
			"--repo", repositoryPath,
			"--mode", "selection",
			"--revision", revision,
			"--path", "reviewed.go",
			"--start-line", "2",
			"--end-line", "2",
			"--store", store,
			"--json",
		},
		&reviewOutput,
	); err != nil {
		t.Fatalf("review CLI error = %v", err)
	}
	var reviewed runOutput
	decodeCLIOutput(t, reviewOutput.Bytes(), &reviewed)
	if reviewed.Report == nil || len(reviewed.Report.Findings) != 1 {
		t.Fatalf("review output has no finding: %+v", reviewed)
	}
	runID := reviewed.Run.RunID
	findingID := reviewed.Report.Findings[0].ID
	at := reviewed.Run.CompletedAt.Add(time.Minute).UTC()
	if reviewed.Run.FindingSetRef == nil {
		t.Fatal("review run has no finding set reference")
	}
	decisionRequest := findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runID, FindingID: findingID,
		Action: findingdecision.ActionHumanReview, ReasonCode: "needs_owner_review",
		EvidenceRefs: []findingdecision.EvidenceRef{{
			Authority: "argus-local", ID: reviewed.Run.FindingSetRef.URI,
			SHA256: reviewed.Run.FindingSetRef.SHA256,
		}},
		OccurredAt: at,
	}
	decisionMutation := findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "decision-cli-key-1",
		Actor:          findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "reviewer-1"},
		Roles:          []findingdecision.Role{findingdecision.RoleFindingReviewer},
		Audit:          "manual review requested owner confirmation", At: at.Add(time.Second),
	}
	decisionPath := writeCLIJSONDescriptor(t, "decision-request.json", decisionRequest)
	decisionMutationPath := writeCLIJSONDescriptor(t, "decision-mutation.json", decisionMutation)
	var firstDecisionID string
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(context.Background(), []string{
			"decision", "record", "--store", store,
			"--input", decisionPath, "--mutation", decisionMutationPath, "--json",
		}, &output); err != nil {
			t.Fatalf("decision attempt %d error = %v", attempt+1, err)
		}
		var decoded decisionRecordOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Decision.Sequence != 2 || decoded.Decision.SourceSHA256 != reviewed.Run.FindingSetRef.SHA256 {
			t.Fatalf("decision output = %+v", decoded)
		}
		if firstDecisionID == "" {
			firstDecisionID = decoded.Decision.DecisionID
		} else if decoded.Decision.DecisionID != firstDecisionID {
			t.Fatalf("decision retry changed ID: %s -> %s", firstDecisionID, decoded.Decision.DecisionID)
		}
	}

	feedback := feedbackdomain.Feedback{
		SchemaVersion: feedbackdomain.FeedbackSchemaVersion,
		FeedbackID:    "feedback-cli-1",
		FindingID:     findingID,
		RunID:         runID,
		Action:        feedbackdomain.FeedbackAccept,
		Actor: feedbackdomain.ActorRef{
			Kind: feedbackdomain.ActorHuman,
			ID:   "reviewer-1",
		},
		Source: feedbackdomain.Source{
			Kind: feedbackdomain.SourceAPI,
			ID:   "argus-cli",
		},
		OccurredAt: at,
		RecordedAt: at.Add(time.Second),
		SourceRefs: []feedbackdomain.SourceRef{{
			Kind:      feedbackdomain.SourceRefManualObservation,
			Authority: "argus-test",
			ID:        "feedback-observation-1",
		}},
		IdempotencyKey: "feedback-cli-key-1",
	}
	feedbackPath := writeCLIJSONDescriptor(t, "feedback.json", feedback)
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(
			context.Background(),
			[]string{
				"feedback", "record",
				"--store", store,
				"--input", feedbackPath,
				"--json",
			},
			&output,
		); err != nil {
			t.Fatalf("feedback attempt %d error = %v", attempt+1, err)
		}
		var decoded feedbackRecordOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Feedback.FeedbackID != feedback.FeedbackID ||
			decoded.Eligibility.Use != feedbackdomain.EvaluationUseCandidateOnly ||
			!decoded.Eligibility.GovernanceReviewRequired {
			t.Fatalf("feedback output = %+v", decoded)
		}
	}

	candidateAt := feedback.RecordedAt.Add(time.Minute)
	candidateRequestPath := writeCLIJSONDescriptor(t, "evaluation-candidate-derive.json",
		evaluation.CandidateDerivationRequest{
			SchemaVersion: evaluation.CandidateDerivationRequestSchemaVersion,
			CaseID:        "candidate-from-feedback-cli", RunID: runID, FindingID: findingID,
			Source: evaluation.CandidateFromProductionFeedback, SourceID: feedback.FeedbackID,
			LicenseID:      "internal-review-data-policy-1",
			Consent:        evaluation.ConsentAuthorizedInternal,
			Classification: evaluation.ClassificationInternal,
			Owner:          "evaluation-governance", LabelPolicyRevision: "candidate-label-policy-1",
			Restrictions: []string{"candidate review required before activation"},
			CollectedAt:  candidateAt,
		})
	candidateMutationPath := writeCLIJSONDescriptor(t, "evaluation-candidate-mutation.json",
		evaluation.Mutation{
			IdempotencyKey: "derive-feedback-candidate-cli", Actor: "curator-1",
			Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
			Audit: "derive exact feedback into candidate pool", At: candidateAt,
		})
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(t.Context(), []string{
			"evaluation", "case", "derive", "--store", store,
			"--input", candidateRequestPath, "--mutation", candidateMutationPath, "--json",
		}, &output); err != nil {
			t.Fatalf("evaluation candidate derive attempt %d: %v", attempt+1, err)
		}
		var decoded evaluationCaseOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Record.Case.DatasetState != evaluation.DatasetCandidatePool ||
			decoded.Record.Case.ReviewState != evaluation.ReviewPending ||
			decoded.Record.Case.Eligibility != (evaluation.Eligibility{}) ||
			decoded.Record.Case.Provenance.SourceID != feedback.FeedbackID ||
			len(decoded.Record.Case.LicenseConsent.AllowedUses) != 1 ||
			decoded.Record.Case.LicenseConsent.AllowedUses[0] != evaluation.UseCandidatePool {
			t.Fatalf("derived CLI candidate = %+v", decoded.Record.Case)
		}
	}

	conflictingFeedback := feedback
	conflictingFeedback.Action = feedbackdomain.FeedbackDismiss
	conflictPath := writeCLIJSONDescriptor(
		t,
		"feedback-conflict.json",
		conflictingFeedback,
	)
	if err := runWithIO(
		context.Background(),
		[]string{
			"feedback", "record",
			"--store", store,
			"--input", conflictPath,
		},
		&bytes.Buffer{},
	); !errors.Is(err, feedbackdomain.ErrConflict) {
		t.Fatalf("conflicting feedback error = %v, want ErrConflict", err)
	}

	outcome := feedbackdomain.Outcome{
		SchemaVersion: feedbackdomain.OutcomeSchemaVersion,
		OutcomeID:     "outcome-cli-1",
		FindingID:     findingID,
		RunID:         runID,
		State:         feedbackdomain.OutcomeFixed,
		Actor: feedbackdomain.ActorRef{
			Kind: feedbackdomain.ActorHuman,
			ID:   "reviewer-1",
		},
		Source: feedbackdomain.Source{
			Kind: feedbackdomain.SourceManual,
			ID:   "argus-cli",
		},
		OccurredAt: at.Add(2 * time.Hour),
		RecordedAt: at.Add(3 * time.Hour),
		Window: feedbackdomain.AttributionWindow{
			Start: at,
			End:   at.Add(24 * time.Hour),
		},
		SourceRefs: []feedbackdomain.SourceRef{{
			Kind:      feedbackdomain.SourceRefManualObservation,
			Authority: "argus-test",
			ID:        "outcome-observation-1",
		}},
		IdempotencyKey: "outcome-cli-key-1",
	}
	outcomePath := writeCLIJSONDescriptor(t, "outcome.json", outcome)
	for attempt := 0; attempt < 2; attempt++ {
		if err := runWithIO(
			context.Background(),
			[]string{
				"outcome", "record",
				"--store", store,
				"--input", outcomePath,
			},
			&bytes.Buffer{},
		); err != nil {
			t.Fatalf("outcome attempt %d error = %v", attempt+1, err)
		}
	}

	var findingOutput bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{
			"finding", "show",
			"--store", store,
			"--run", runID,
			"--finding", findingID,
			"--json",
		},
		&findingOutput,
	); err != nil {
		t.Fatalf("finding show error = %v", err)
	}
	var detail findingShowOutput
	decodeCLIOutput(t, findingOutput.Bytes(), &detail)
	if len(detail.Decisions) == 0 ||
		len(detail.HumanDecisions) != 1 ||
		detail.HumanDecisions[0].DecisionID != firstDecisionID ||
		len(detail.Feedback) != 1 ||
		len(detail.Outcomes) != 1 {
		t.Fatalf("finding detail = %+v", detail)
	}
}

func TestPublicationGrantBuildsOneExactRequestWhileReviewPolicyRemainsDeny(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "reviewed.go", "package fixture\n")
	base := commitCLITarget(t, repositoryPath, "publication base")
	writeCLITargetFile(
		t, repositoryPath, "reviewed.go",
		"package fixture\n// ARGUS_BUG publication request\n",
	)
	head := commitCLITarget(t, repositoryPath, "publication head")
	store := t.TempDir()
	var reviewOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"review", "--repo", repositoryPath, "--mode", "diff",
		"--base", base, "--head", head,
		"--store", store, "--json",
	}, &reviewOutput); err != nil {
		t.Fatalf("publication source review: %v", err)
	}
	var reviewed runOutput
	decodeCLIOutput(t, reviewOutput.Bytes(), &reviewed)
	if reviewed.Report == nil || len(reviewed.Report.Findings) != 1 || reviewed.Run.FindingSetRef == nil {
		t.Fatalf("publication source review = %+v", reviewed)
	}
	findingID := reviewed.Report.Findings[0].ID
	at := reviewed.Run.CompletedAt.Add(time.Minute).UTC()
	decisionPath := writeCLIJSONDescriptor(t, "publish-decision.json", findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         reviewed.Run.RunID, FindingID: findingID,
		Action: findingdecision.ActionPublish, ReasonCode: "human_confirmed",
		EvidenceRefs: []findingdecision.EvidenceRef{{
			Authority: "argus-local", ID: reviewed.Run.FindingSetRef.URI,
			SHA256: reviewed.Run.FindingSetRef.SHA256,
		}}, OccurredAt: at,
	})
	mutationPath := writeCLIJSONDescriptor(t, "publish-decision-mutation.json", findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: "publication-cli-decision-key",
		Actor:          findingdecision.Actor{Kind: findingdecision.ActorHuman, ID: "approver-1"},
		Roles:          []findingdecision.Role{findingdecision.RolePublicationApprover},
		Audit:          "approved publication through CLI", At: at,
	})
	var decisionOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"decision", "record", "--store", store,
		"--input", decisionPath, "--mutation", mutationPath, "--json",
	}, &decisionOutput); err != nil {
		t.Fatalf("record publication Decision: %v", err)
	}
	var recorded decisionRecordOutput
	decodeCLIOutput(t, decisionOutput.Bytes(), &recorded)
	if recorded.Decision.Action != findingdecision.ActionPublish {
		t.Fatalf("recorded publication decision = %+v", recorded)
	}

	grantAt := at.Add(time.Minute)
	grantRequestPath := writeCLIJSONDescriptor(t, "publication-grant-request.json", publication.GrantRequest{
		SchemaVersion: publication.GrantRequestSchemaVersion,
		GrantID:       "grant-cli-1", PublicationID: "publication-cli-1",
		PublicationIdempotencyKey: "publication-cli-key-1",
		RunID:                     reviewed.Run.RunID, FindingID: findingID,
		Provider: "github", RepositoryID: "org/repository",
		ChangeKind: "pull_request", ChangeID: "42",
		Channel:                    "pull_request_inline",
		ExpectedPermissionRevision: githubadapter.SingleUserLocalPermissionRevision,
		OccurredAt:                 grantAt, ExpiresAt: grantAt.Add(time.Hour),
	})
	grantMutationPath := writeCLIJSONDescriptor(t, "publication-grant-mutation.json", publication.GrantMutation{
		SchemaVersion:  publication.GrantMutationSchemaVersion,
		IdempotencyKey: "publication-cli-grant-key",
		Actor:          publication.GrantActor{Kind: publication.GrantActorHuman, ID: "approver-1"},
		Roles:          []publication.GrantRole{publication.GrantRolePublicationApprover},
		Audit:          "approve one exact publication through CLI", At: grantAt,
	})
	var grantOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"publication", "grant", "record", "--store", store,
		"--input", grantRequestPath, "--mutation", grantMutationPath, "--json",
	}, &grantOutput); err != nil {
		t.Fatalf("publication grant record: %v", err)
	}
	var granted publicationGrantOutput
	decodeCLIOutput(t, grantOutput.Bytes(), &granted)
	if granted.Grant.GrantID != "grant-cli-1" || granted.Grant.DecisionID != recorded.Decision.DecisionID {
		t.Fatalf("publication grant = %+v", granted)
	}

	intentPath := writeCLIJSONDescriptor(t, "publication-intent.json", publication.Intent{
		SchemaVersion: publication.IntentSchemaVersion,
		GrantID:       granted.Grant.GrantID, CreatedAt: grantAt.Add(time.Minute),
	})
	var requestOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"publication", "request", "build", "--store", store,
		"--input", intentPath, "--json",
	}, &requestOutput); err != nil {
		t.Fatalf("publication request build: %v", err)
	}
	var built publicationRequestOutput
	decodeCLIOutput(t, requestOutput.Bytes(), &built)
	if built.Request.GrantID != granted.Grant.GrantID ||
		built.Request.GrantSHA256 != granted.Grant.GrantSHA256 ||
		built.Request.DecisionID != recorded.Decision.DecisionID ||
		built.Request.RemoteWrites != "allow" {
		t.Fatalf("publication request = %+v", built)
	}

	provider := &cliUnknownPublicationProvider{revalidateFails: 1}
	originalProviderBuilder := buildGitHubPublicationProvider
	buildGitHubPublicationProvider = func() (publication.Provider, error) { return provider, nil }
	defer func() { buildGitHubPublicationProvider = originalProviderBuilder }()
	if err := runWithIO(t.Context(), []string{
		"publication", "github", "dispatch", "--store", store,
		"--input", intentPath, "--single-user-local", "--json",
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "preflight failure") {
		t.Fatalf("transient dispatch error = %v", err)
	}
	if err := runWithIO(t.Context(), []string{
		"publication", "github", "reconcile", "--store", store,
		"--publication", built.Request.PublicationID, "--single-user-local", "--json",
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "cannot initiate a new remote write") {
		t.Fatalf("requested reconcile error = %v", err)
	}
	if provider.publishCalls != 0 || provider.lookupCalls != 0 {
		t.Fatalf("requested reconcile crossed provider boundary: %+v", provider)
	}
	var dispatchOutput bytes.Buffer
	err := runWithIO(t.Context(), []string{
		"publication", "github", "dispatch", "--store", store,
		"--input", intentPath, "--single-user-local", "--json",
	}, &dispatchOutput)
	if !errors.Is(err, publication.ErrUnknownOutcome) {
		t.Fatalf("lost dispatch response error = %v", err)
	}
	var dispatched publicationDispatchOutput
	decodeCLIOutput(t, dispatchOutput.Bytes(), &dispatched)
	if dispatched.Record.State != publication.StateUnknown || provider.publishCalls != 1 {
		t.Fatalf("unknown dispatch = %+v calls=%d", dispatched, provider.publishCalls)
	}

	var reconcileOutput bytes.Buffer
	if err := runWithIO(t.Context(), []string{
		"publication", "github", "reconcile", "--store", store,
		"--publication", built.Request.PublicationID, "--single-user-local", "--json",
	}, &reconcileOutput); err != nil {
		t.Fatalf("publication reconcile: %v", err)
	}
	var reconciled publicationDispatchOutput
	decodeCLIOutput(t, reconcileOutput.Bytes(), &reconciled)
	if reconciled.Record.State != publication.StatePublished ||
		provider.publishCalls != 1 || provider.lookupCalls != 1 {
		t.Fatalf("reconciled publication = %+v publish=%d lookup=%d",
			reconciled, provider.publishCalls, provider.lookupCalls)
	}
}

func TestEvaluationCLIEnforcesStrictInputIdempotencyAndHoldoutACL(t *testing.T) {
	store := t.TempDir()
	at := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	evaluationCase := testCLIEvaluationCase(
		"case-cli-candidate",
		"source-cli-candidate",
		evaluation.SplitUnassigned,
		at,
	)
	inputPath := writeCLIJSONDescriptor(t, "evaluation-case.json", evaluationCase)
	mutation := evaluation.Mutation{
		IdempotencyKey: "evaluation-create-key-1",
		Actor:          "curator-1",
		Roles:          []evaluation.Role{evaluation.RoleDatasetCurator},
		Audit:          "create governed candidate case",
		At:             at,
	}
	mutationPath := writeCLIJSONDescriptor(t, "evaluation-mutation.json", mutation)
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(
			context.Background(),
			[]string{
				"evaluation", "case", "create",
				"--store", store,
				"--input", inputPath,
				"--mutation", mutationPath,
				"--json",
			},
			&output,
		); err != nil {
			t.Fatalf("case create attempt %d error = %v", attempt+1, err)
		}
		var decoded evaluationCaseOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Record.Case.CaseID != evaluationCase.CaseID ||
			decoded.Record.CurrentLabelRevision != 1 {
			t.Fatalf("case output = %+v", decoded)
		}
	}

	conflictingCase := evaluationCase
	conflictingCase.CaseID = "case-cli-conflict"
	conflictingCase.Provenance.SourceID = "source-cli-conflict"
	conflictingCase.CloneGroupID = "clone-cli-conflict"
	conflictingPath := writeCLIJSONDescriptor(t, "evaluation-conflict.json", conflictingCase)
	if err := runWithIO(
		context.Background(),
		[]string{
			"evaluation", "case", "create",
			"--store", store,
			"--input", conflictingPath,
			"--mutation", mutationPath,
		},
		&bytes.Buffer{},
	); !errors.Is(err, evaluation.ErrConflict) {
		t.Fatalf("conflicting evaluation mutation error = %v, want ErrConflict", err)
	}
	correction := evaluation.LabelCorrection{
		SchemaVersion:         evaluation.LabelCorrectionSchemaVersion,
		CaseID:                evaluationCase.CaseID,
		ExpectedLabelRevision: 1,
		Label: evaluation.Label{
			ExpectedOutcome:    evaluation.OutcomeClean,
			Category:           "verified-clean",
			Anchors:            []evaluation.LabelAnchor{},
			AnchorRefs:         []string{},
			SuppressionTargets: []evaluation.SuppressionTarget{},
		},
		LabelPolicyRevision:   "label-policy-2",
		Reason:                "independent review confirmed the clean label",
		AffectedExperimentIDs: []string{},
	}
	correctionMutation := evaluation.Mutation{
		IdempotencyKey: "evaluation-correct-label-key-1",
		Actor:          "curator-1",
		Roles:          []evaluation.Role{evaluation.RoleDatasetCurator},
		Audit:          "record reviewed label correction",
		At:             at.Add(30 * time.Minute),
	}
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(
			context.Background(),
			[]string{
				"evaluation", "case", "correct-label",
				"--store", store,
				"--input", writeCLIJSONDescriptor(
					t,
					"label-correction.json",
					correction,
				),
				"--mutation", writeCLIJSONDescriptor(
					t,
					"label-correction-mutation.json",
					correctionMutation,
				),
				"--json",
			},
			&output,
		); err != nil {
			t.Fatalf("label correction attempt %d error = %v", attempt+1, err)
		}
		var decoded evaluationCaseOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Record.CurrentLabelRevision != 2 ||
			decoded.Record.CurrentLabel.Category != "verified-clean" {
			t.Fatalf("label correction output = %+v", decoded)
		}
	}

	unauthorizedMutation := mutation
	unauthorizedMutation.IdempotencyKey = "evaluation-create-unauthorized"
	unauthorizedMutation.Roles = []evaluation.Role{evaluation.RolePromotionOperator}
	unauthorizedMutation.At = at.Add(time.Hour)
	unauthorizedCase := evaluationCase
	unauthorizedCase.CaseID = "case-cli-unauthorized"
	unauthorizedCase.Provenance.SourceID = "source-cli-unauthorized"
	unauthorizedCase.Provenance.CollectedAt = unauthorizedMutation.At
	unauthorizedCase.CloneGroupID = "clone-cli-unauthorized"
	unauthorizedCase.CreatedAt = unauthorizedMutation.At
	if err := runWithIO(
		context.Background(),
		[]string{
			"evaluation", "case", "create",
			"--store", store,
			"--input", writeCLIJSONDescriptor(
				t,
				"evaluation-unauthorized.json",
				unauthorizedCase,
			),
			"--mutation", writeCLIJSONDescriptor(
				t,
				"evaluation-unauthorized-mutation.json",
				unauthorizedMutation,
			),
		},
		&bytes.Buffer{},
	); !errors.Is(err, evaluation.ErrUnauthorized) {
		t.Fatalf("unauthorized evaluation mutation error = %v, want ErrUnauthorized", err)
	}

	holdoutAt := at.Add(2 * time.Hour)
	holdout := testCLIEvaluationCase(
		"case-cli-holdout",
		"source-cli-holdout",
		evaluation.SplitHoldout,
		holdoutAt,
	)
	holdoutMutation := evaluation.Mutation{
		IdempotencyKey: "evaluation-create-holdout",
		Actor:          "holdout-maintainer-1",
		Roles:          []evaluation.Role{evaluation.RoleHoldoutMaintainer},
		Audit:          "create governed holdout case",
		At:             holdoutAt,
	}
	holdoutImport := testCLIExternalGovernedImport(t, holdout)
	registeredAt := holdoutAt.Add(-time.Minute)
	registration := evaluation.GovernanceTrustKeyRegistration{
		SchemaVersion: evaluation.GovernanceTrustKeyRegistrationSchemaVersion,
		Key:           holdoutImport.TrustedKey,
		RegisteredAt:  registeredAt,
	}
	registrationMutation := evaluation.Mutation{
		IdempotencyKey: "evaluation-register-holdout-key", Actor: "governance-trust-admin-1",
		Roles: []evaluation.Role{evaluation.RoleGovernanceTrustAdmin},
		Audit: "trust external holdout governance key", At: registeredAt,
	}
	if err := runWithIO(context.Background(), []string{
		"evaluation", "trust-key", "register", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "holdout-key-registration.json", registration),
		"--mutation", writeCLIJSONDescriptor(t, "holdout-key-registration-mutation.json", registrationMutation),
		"--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("register holdout governance key error = %v", err)
	}
	if err := runWithIO(
		context.Background(),
		[]string{
			"evaluation", "case", "import",
			"--store", store,
			"--input", writeCLIJSONDescriptor(t, "holdout-import.json", holdoutImport),
			"--mutation", writeCLIJSONDescriptor(
				t,
				"holdout-mutation.json",
				holdoutMutation,
			),
		},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("import holdout error = %v", err)
	}
	trustAccess := evaluation.Access{
		Actor: "governance-trust-auditor", Roles: []evaluation.Role{evaluation.RoleGovernanceTrustAdmin},
	}
	var trustList bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "trust-key", "list", "--store", store,
		"--access", writeCLIJSONDescriptor(t, "trust-access.json", trustAccess), "--json",
	}, &trustList); err != nil {
		t.Fatalf("list governance keys error = %v", err)
	}
	var trustListOutput evaluationTrustKeyListOutput
	decodeCLIOutput(t, trustList.Bytes(), &trustListOutput)
	if len(trustListOutput.TrustKeys) != 1 || trustListOutput.TrustKeys[0].RevokedAt != nil {
		t.Fatalf("active governance key list = %+v", trustListOutput)
	}
	revokedAt := holdoutAt.Add(30 * time.Second)
	revocation := evaluation.GovernanceTrustKeyRevocation{
		SchemaVersion: evaluation.GovernanceTrustKeyRevocationSchemaVersion,
		Authority:     holdoutImport.TrustedKey.Authority, KeyID: holdoutImport.TrustedKey.KeyID,
		Revision: holdoutImport.TrustedKey.Revision, Reason: "rotate external governance authority", RevokedAt: revokedAt,
	}
	revokeMutation := evaluation.Mutation{
		IdempotencyKey: "evaluation-revoke-holdout-key", Actor: "governance-trust-admin-2",
		Roles: []evaluation.Role{evaluation.RoleGovernanceTrustAdmin},
		Audit: "revoke external holdout governance key", At: revokedAt,
	}
	if err := runWithIO(context.Background(), []string{
		"evaluation", "trust-key", "revoke", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "holdout-key-revocation.json", revocation),
		"--mutation", writeCLIJSONDescriptor(t, "holdout-key-revocation-mutation.json", revokeMutation),
		"--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("revoke holdout governance key error = %v", err)
	}
	curatorAccess := evaluation.Access{
		Actor: "curator-1",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
	}
	if err := runWithIO(
		context.Background(),
		[]string{
			"evaluation", "case", "show",
			"--store", store,
			"--case", holdout.CaseID,
			"--access", writeCLIJSONDescriptor(t, "curator-access.json", curatorAccess),
		},
		&bytes.Buffer{},
	); !errors.Is(err, evaluation.ErrUnauthorized) {
		t.Fatalf("holdout disclosure error = %v, want ErrUnauthorized", err)
	}
	runnerAccess := evaluation.Access{
		Actor: "holdout-runner-1",
		Roles: []evaluation.Role{evaluation.RoleHoldoutRunner},
	}
	if err := runWithIO(
		context.Background(),
		[]string{
			"evaluation", "case", "show",
			"--store", store,
			"--case", holdout.CaseID,
			"--access", writeCLIJSONDescriptor(t, "runner-access.json", runnerAccess),
		},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("authorized holdout show error = %v", err)
	}
	exposure := evaluation.Exposure{
		SchemaVersion:   evaluation.ExposureSchemaVersion,
		EvaluationRunID: "evaluation-run-cli-1",
		CaseID:          holdout.CaseID,
		Observations: []evaluation.ExposureObservation{
			{
				Component: evaluation.ExposurePrompt,
				Status:    evaluation.ExposureNotSeen,
				Revision:  "prompt-1",
			},
			{
				Component: evaluation.ExposureRule,
				Status:    evaluation.ExposureNotSeen,
				Revision:  "rule-1",
			},
			{
				Component: evaluation.ExposureModel,
				Status:    evaluation.ExposureNotSeen,
				Revision:  "model-1",
			},
			{
				Component: evaluation.ExposureIndex,
				Status:    evaluation.ExposureNotSeen,
				Revision:  "index-1",
			},
		},
		ObservedAt: holdoutAt.Add(time.Minute),
	}
	exposureMutation := evaluation.Mutation{
		IdempotencyKey: "evaluation-exposure-key-1",
		Actor:          "holdout-runner-1",
		Roles:          []evaluation.Role{evaluation.RoleHoldoutRunner},
		Audit:          "record complete holdout exposure",
		At:             holdoutAt.Add(2 * time.Minute),
	}
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(
			context.Background(),
			[]string{
				"evaluation", "exposure", "record",
				"--store", store,
				"--input", writeCLIJSONDescriptor(
					t,
					"holdout-exposure.json",
					exposure,
				),
				"--mutation", writeCLIJSONDescriptor(
					t,
					"holdout-exposure-mutation.json",
					exposureMutation,
				),
				"--json",
			},
			&output,
		); err != nil {
			t.Fatalf("exposure attempt %d error = %v", attempt+1, err)
		}
		var decoded evaluationExposureOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Entry.Exposure.EvaluationRunID != exposure.EvaluationRunID {
			t.Fatalf("exposure output = %+v", decoded)
		}
	}
	var listed bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{
			"evaluation", "case", "list",
			"--store", store,
			"--access", writeCLIJSONDescriptor(t, "list-access.json", curatorAccess),
			"--json",
		},
		&listed,
	); err != nil {
		t.Fatalf("evaluation case list error = %v", err)
	}
	var listOutput evaluationCaseListOutput
	decodeCLIOutput(t, listed.Bytes(), &listOutput)
	if len(listOutput.Cases) != 1 ||
		listOutput.Cases[0].Case.CaseID != evaluationCase.CaseID {
		t.Fatalf("curator case list disclosed holdout or lost visible case: %+v", listOutput)
	}

	duplicateAccess := filepath.Join(t.TempDir(), "duplicate-access.json")
	if err := os.WriteFile(
		duplicateAccess,
		[]byte(`{"actor":"first","actor":"second","roles":["dataset_curator"]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvaluationAccess(duplicateAccess); err == nil ||
		!strings.Contains(err.Error(), `duplicate JSON field "actor"`) {
		t.Fatalf("duplicate JSON access error = %v", err)
	}
	unknownAccess := filepath.Join(t.TempDir(), "unknown-access.json")
	if err := os.WriteFile(
		unknownAccess,
		[]byte(`{"actor":"curator","roles":["dataset_curator"],"admin":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvaluationAccess(unknownAccess); err == nil ||
		!strings.Contains(err.Error(), `unknown field "admin"`) {
		t.Fatalf("unknown JSON access error = %v", err)
	}
}

func TestEvaluationCLICandidateReviewGovernance(t *testing.T) {
	store := t.TempDir()
	at := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	candidate := testCLIEvaluationCase("case-cli-governed", "source-cli-governed", evaluation.SplitUnassigned, at)
	createMutation := evaluation.Mutation{
		IdempotencyKey: "create-cli-governed", Actor: "ingest-curator",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "ingest candidate", At: at,
	}
	if err := runWithIO(context.Background(), []string{
		"evaluation", "case", "create", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "candidate.json", candidate),
		"--mutation", writeCLIJSONDescriptor(t, "create-mutation.json", createMutation), "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("case create error = %v", err)
	}
	assignment := evaluation.CaseReviewAssignment{
		SchemaVersion: evaluation.CaseReviewAssignmentSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
		ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
		Reason: "assign an explicit blind review round", AssignedAt: at.Add(time.Minute),
	}
	assignmentMutation := evaluation.Mutation{
		IdempotencyKey: "assign-cli-governed", Actor: "assignment-curator",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "assign blind reviewers", At: assignment.AssignedAt,
	}
	if err := runWithIO(context.Background(), []string{
		"evaluation", "case", "assign", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "assignment.json", assignment),
		"--mutation", writeCLIJSONDescriptor(t, "assignment-mutation.json", assignmentMutation), "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("case assign error = %v", err)
	}

	annotationIDs := make([]string, 0, 2)
	for index, actor := range []string{"reviewer-a", "reviewer-b"} {
		reviewedAt := at.Add(time.Duration(index+2) * time.Minute)
		label := candidate.Label
		annotation := evaluation.CaseAnnotation{
			SchemaVersion: evaluation.CaseAnnotationSchemaVersion, CaseID: candidate.CaseID,
			ExpectedGovernanceRevision: 2, ExpectedLabelRevision: 1,
			Verdict: evaluation.AnnotationApprove, ProposedLabel: &label,
			LabelPolicyRevision: candidate.LabelPolicyRevision,
			Rationale:           "independent reviewer confirms the frozen source evidence",
			EvidenceRefs:        candidate.Provenance.EvidenceRefs, ReviewedAt: reviewedAt,
		}
		mutation := evaluation.Mutation{
			IdempotencyKey: "annotation-" + actor, Actor: actor,
			Roles: []evaluation.Role{evaluation.RoleDatasetReviewer}, Audit: "independent annotation", At: reviewedAt,
		}
		var output bytes.Buffer
		if err := runWithIO(context.Background(), []string{
			"evaluation", "case", "annotate", "--store", store,
			"--input", writeCLIJSONDescriptor(t, actor+".json", annotation),
			"--mutation", writeCLIJSONDescriptor(t, actor+"-mutation.json", mutation), "--json",
		}, &output); err != nil {
			t.Fatalf("case annotate %s error = %v", actor, err)
		}
		var decoded evaluationAnnotationOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		annotationIDs = append(annotationIDs, decoded.Annotation.EventID)
	}

	selected := candidate.Label
	adjudication := evaluation.CaseAdjudication{
		SchemaVersion: evaluation.CaseAdjudicationSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 2, ExpectedLabelRevision: 1,
		AnnotationEventIDs: annotationIDs, Outcome: evaluation.AdjudicationApprove,
		SelectedLabel: &selected, LabelPolicyRevision: candidate.LabelPolicyRevision,
		Rationale:    "third-party adjudication accepts independently reviewed label",
		EvidenceRefs: candidate.Provenance.EvidenceRefs, AdjudicatedAt: at.Add(4 * time.Minute),
	}
	adjudicationMutation := evaluation.Mutation{
		IdempotencyKey: "adjudicate-cli-governed", Actor: "adjudicator-c",
		Roles: []evaluation.Role{evaluation.RoleDatasetAdjudicator}, Audit: "adjudicate candidate", At: adjudication.AdjudicatedAt,
	}
	if err := runWithIO(context.Background(), []string{
		"evaluation", "case", "adjudicate", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "adjudication.json", adjudication),
		"--mutation", writeCLIJSONDescriptor(t, "adjudication-mutation.json", adjudicationMutation), "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("case adjudicate error = %v", err)
	}

	activation := evaluation.CaseActivation{
		SchemaVersion: evaluation.CaseActivationSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 3, Split: evaluation.SplitTrain,
		Eligibility: evaluation.Eligibility{Evaluation: true, Training: true},
		LicenseConsent: evaluation.LicenseConsent{
			LicenseID: candidate.LicenseConsent.LicenseID, Consent: candidate.LicenseConsent.Consent,
			AllowedUses:  []evaluation.UseScope{evaluation.UseCandidatePool, evaluation.UseEvaluation, evaluation.UseTraining},
			Restrictions: []string{},
		},
		Reason: "activate reviewed case in training split", ActivatedAt: at.Add(5 * time.Minute),
	}
	activationMutation := evaluation.Mutation{
		IdempotencyKey: "activate-cli-governed", Actor: "curator-d",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "activate governed case", At: activation.ActivatedAt,
	}
	if err := runWithIO(context.Background(), []string{
		"evaluation", "case", "activate", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "activation.json", activation),
		"--mutation", writeCLIJSONDescriptor(t, "activation-mutation.json", activationMutation), "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("case activate error = %v", err)
	}
	reopen := evaluation.CaseReopen{
		SchemaVersion: evaluation.CaseReopenSchemaVersion, CaseID: candidate.CaseID,
		ExpectedGovernanceRevision: 4, Reason: "new evidence requires a fresh blind review",
		EvidenceRefs: candidate.Provenance.EvidenceRefs, AffectedExperimentIDs: []string{},
		ReopenedAt: at.Add(6 * time.Minute),
	}
	reopenMutation := evaluation.Mutation{
		IdempotencyKey: "reopen-cli-governed", Actor: "curator-d",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "reopen governed case", At: reopen.ReopenedAt,
	}
	if err := runWithIO(context.Background(), []string{
		"evaluation", "case", "reopen", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "reopen.json", reopen),
		"--mutation", writeCLIJSONDescriptor(t, "reopen-mutation.json", reopenMutation), "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("case reopen error = %v", err)
	}

	access := evaluation.Access{Actor: "curator-d", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}}
	var shown bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "case", "show", "--store", store, "--case", candidate.CaseID,
		"--access", writeCLIJSONDescriptor(t, "governed-access.json", access), "--json",
	}, &shown); err != nil {
		t.Fatalf("case show error = %v", err)
	}
	var decoded evaluationCaseShowOutput
	decodeCLIOutput(t, shown.Bytes(), &decoded)
	if decoded.Record.CurrentGovernance.DatasetState != evaluation.DatasetCandidatePool ||
		decoded.Record.CurrentGovernance.ReviewState != evaluation.ReviewPending ||
		decoded.Record.CurrentGovernance.Revision != 5 || len(decoded.Assignments) != 1 ||
		len(decoded.Annotations) != 2 || len(decoded.Adjudications) != 1 ||
		decoded.Agreement.VerdictAgreement != evaluation.ReviewAgreementUnanimous {
		t.Fatalf("governed case output = %+v", decoded)
	}
}

func TestEvaluationCLIRecordsAndQueriesMissedDefectIncident(t *testing.T) {
	store := t.TempDir()
	at := time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	incident := evaluation.MissedDefectIncident{
		SchemaVersion: evaluation.MissedDefectIncidentSchemaVersion,
		IncidentID:    "incident-cli-1", ReviewRunID: "formal-run-cli-1",
		DefectFingerprint: strings.Repeat("b", 64), Category: "correctness", Severity: "high",
		Anchors: []evaluation.LabelAnchor{{Path: "main.go", Side: "new", StartLine: 4, EndLine: 5, SourceDigest: strings.Repeat("c", 64)}},
		EvidenceArtifacts: []runmodel.ArtifactRef{{
			URI: "artifact://local/sha256/" + digest, SHA256: digest, SizeBytes: 1,
			Contract: "argus.external_incident_evidence.v1alpha1",
		}},
		SourceAuthority: "incident-system", SourceID: "INC-CLI-1", SourceRevision: "revision-1",
		OccurredAt: at, RecordedAt: at.Add(time.Minute),
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: "incident-cli-event-1", Actor: "incident-service",
		Roles: []evaluation.Role{evaluation.RoleIncidentIngest}, Audit: "record incident source", At: incident.RecordedAt,
	}
	var recorded bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "incident", "record", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "incident.json", incident),
		"--mutation", writeCLIJSONDescriptor(t, "incident-mutation.json", mutation), "--json",
	}, &recorded); err != nil {
		t.Fatal(err)
	}
	var recordOutput evaluationIncidentOutput
	decodeCLIOutput(t, recorded.Bytes(), &recordOutput)
	if recordOutput.Incident.Incident.IncidentID != incident.IncidentID {
		t.Fatalf("incident record output = %+v", recordOutput)
	}
	access := evaluation.Access{Actor: "curator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}}
	var listed bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "incident", "list", "--store", store,
		"--access", writeCLIJSONDescriptor(t, "incident-access.json", access), "--json",
	}, &listed); err != nil {
		t.Fatal(err)
	}
	var listOutput evaluationIncidentListOutput
	decodeCLIOutput(t, listed.Bytes(), &listOutput)
	if len(listOutput.Incidents) != 1 || listOutput.Incidents[0].Incident.IncidentID != incident.IncidentID {
		t.Fatalf("incident list output = %+v", listOutput)
	}
}

func TestEvaluationCLIRecordsAndQueriesProbe(t *testing.T) {
	store := t.TempDir()
	at := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	evidenceDigest := strings.Repeat("d", 64)
	receiptDigest := strings.Repeat("e", 64)
	probe := evaluation.EvaluationProbe{
		SchemaVersion: evaluation.EvaluationProbeSchemaVersion,
		ProbeID:       "probe-cli-1", Kind: evaluation.ProbeSyntheticClean, ReviewRunID: "formal-run-cli-1",
		Oracle: evaluation.ProbeOracle{
			Authority: "clean-corpus-oracle", Revision: "oracle-v1",
			ExpectedOutcome: evaluation.OutcomeClean, Anchors: []evaluation.LabelAnchor{},
			EvidenceArtifacts: []runmodel.ArtifactRef{{
				URI: "artifact://local/sha256/" + evidenceDigest, SHA256: evidenceDigest,
				SizeBytes: 1, Contract: "argus.clean_corpus_attestation.v1alpha1",
			}},
		},
		ReceiptRef: runmodel.ArtifactRef{
			URI: "artifact://local/sha256/" + receiptDigest, SHA256: receiptDigest,
			SizeBytes: 1, Contract: evaluation.EvaluationProbeReceiptSchemaVersion,
		},
		SourceAuthority: "synthetic-corpus-builder", SourceID: "clean-fixture-1",
		SourceRevision: "builder-v1", RecordedAt: at,
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: "probe-cli-event-1", Actor: "probe-ingest-service",
		Roles: []evaluation.Role{evaluation.RoleProbeIngest}, Audit: "record synthetic clean probe", At: at,
	}
	var recorded bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "probe", "record", "--store", store,
		"--input", writeCLIJSONDescriptor(t, "probe.json", probe),
		"--mutation", writeCLIJSONDescriptor(t, "probe-mutation.json", mutation), "--json",
	}, &recorded); err != nil {
		t.Fatal(err)
	}
	var recordOutput evaluationProbeOutput
	decodeCLIOutput(t, recorded.Bytes(), &recordOutput)
	if recordOutput.Probe.Probe.ProbeID != probe.ProbeID {
		t.Fatalf("probe record output = %+v", recordOutput)
	}
	access := evaluation.Access{Actor: "curator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}}
	var listed bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "probe", "list", "--store", store,
		"--access", writeCLIJSONDescriptor(t, "probe-access.json", access), "--json",
	}, &listed); err != nil {
		t.Fatal(err)
	}
	var listOutput evaluationProbeListOutput
	decodeCLIOutput(t, listed.Bytes(), &listOutput)
	if len(listOutput.Probes) != 1 || listOutput.Probes[0].Probe.ProbeID != probe.ProbeID {
		t.Fatalf("probe list output = %+v", listOutput)
	}
}

func TestPromotionCLIEnforcesIdempotencyAndPermissions(t *testing.T) {
	store := t.TempDir()
	at := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	variant := evaluation.PromotionVariant{
		SchemaVersion:    evaluation.PromotionVariantSchemaVersion,
		VariantID:        "variant-cli-1",
		Component:        evaluation.PromotionRulePack,
		Revision:         "rule-pack-2",
		RollbackRevision: "rule-pack-1",
		PolicyRevision:   "promotion-policy-1",
		Origin:           evaluation.PromotionConfigurationChange,
		Owner:            "review-platform",
		CreatedAt:        at,
	}
	variantPath := writeCLIJSONDescriptor(t, "promotion-variant.json", variant)
	mutation := evaluation.Mutation{
		IdempotencyKey: "promotion-register-key-1",
		Actor:          "promotion-operator-1",
		Roles:          []evaluation.Role{evaluation.RolePromotionOperator},
		Audit:          "register rule-pack candidate",
		At:             at,
	}
	mutationPath := writeCLIJSONDescriptor(t, "promotion-mutation.json", mutation)
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(
			context.Background(),
			[]string{
				"promotion", "register",
				"--store", store,
				"--input", variantPath,
				"--mutation", mutationPath,
				"--json",
			},
			&output,
		); err != nil {
			t.Fatalf("promotion register attempt %d error = %v", attempt+1, err)
		}
		var decoded promotionOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Record.Variant.VariantID != variant.VariantID ||
			decoded.Record.Status != evaluation.PromotionRegistered {
			t.Fatalf("promotion output = %+v", decoded)
		}
	}
	gate := evaluation.GateResult{
		SchemaVersion: evaluation.GateResultSchemaVersion,
		VariantID:     variant.VariantID,
		Gate:          evaluation.GateSchemaContract,
		Outcome:       evaluation.GatePass,
		Evidence: evaluation.GateEvidence{
			Refs:             []string{"artifact://argus-test/promotion/schema-contract"},
			Basis:            evaluation.EvidenceDeterministic,
			ChecksPassed:     true,
			HoldoutCaseIDs:   []string{},
			SafetyEventCount: 0,
		},
		Summary: "schema and contract checks passed",
	}
	gateMutation := evaluation.Mutation{
		IdempotencyKey: "promotion-gate-key-1",
		Actor:          "promotion-operator-1",
		Roles:          []evaluation.Role{evaluation.RolePromotionOperator},
		Audit:          "record schema contract promotion gate",
		At:             at.Add(time.Minute),
	}
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(
			context.Background(),
			[]string{
				"promotion", "gate",
				"--store", store,
				"--input", writeCLIJSONDescriptor(t, "promotion-gate.json", gate),
				"--mutation", writeCLIJSONDescriptor(
					t,
					"promotion-gate-mutation.json",
					gateMutation,
				),
				"--json",
			},
			&output,
		); err != nil {
			t.Fatalf("promotion gate attempt %d error = %v", attempt+1, err)
		}
		var decoded promotionOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Record.Status != evaluation.PromotionInProgress ||
			decoded.Record.NextGate == nil ||
			*decoded.Record.NextGate != evaluation.GateTargetedRegression {
			t.Fatalf("promotion gate output = %+v", decoded)
		}
	}
	rollbackMutation := evaluation.Mutation{
		IdempotencyKey: "promotion-rollback-before-active",
		Actor:          "promotion-operator-1",
		Roles:          []evaluation.Role{evaluation.RolePromotionOperator},
		Audit:          "verify inactive variants cannot be rolled back",
		At:             at.Add(2 * time.Minute),
	}
	if err := runWithIO(
		context.Background(),
		[]string{
			"promotion", "rollback",
			"--store", store,
			"--variant", variant.VariantID,
			"--mutation", writeCLIJSONDescriptor(
				t,
				"promotion-rollback-mutation.json",
				rollbackMutation,
			),
		},
		&bytes.Buffer{},
	); !errors.Is(err, evaluation.ErrInvalidTransition) {
		t.Fatalf("inactive rollback error = %v, want ErrInvalidTransition", err)
	}

	unauthorized := variant
	unauthorized.VariantID = "variant-cli-unauthorized"
	unauthorized.Revision = "rule-pack-3"
	unauthorized.CreatedAt = at.Add(time.Hour)
	unauthorizedMutation := evaluation.Mutation{
		IdempotencyKey: "promotion-register-unauthorized",
		Actor:          "curator-1",
		Roles:          []evaluation.Role{evaluation.RoleDatasetCurator},
		Audit:          "unauthorized promotion attempt",
		At:             unauthorized.CreatedAt,
	}
	if err := runWithIO(
		context.Background(),
		[]string{
			"promotion", "register",
			"--store", store,
			"--input", writeCLIJSONDescriptor(
				t,
				"promotion-unauthorized.json",
				unauthorized,
			),
			"--mutation", writeCLIJSONDescriptor(
				t,
				"promotion-unauthorized-mutation.json",
				unauthorizedMutation,
			),
		},
		&bytes.Buffer{},
	); !errors.Is(err, evaluation.ErrUnauthorized) {
		t.Fatalf("unauthorized promotion error = %v, want ErrUnauthorized", err)
	}

	curatorAccess := evaluation.Access{
		Actor: "curator-1",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
	}
	if err := runWithIO(
		context.Background(),
		[]string{
			"promotion", "show",
			"--store", store,
			"--variant", variant.VariantID,
			"--access", writeCLIJSONDescriptor(
				t,
				"promotion-curator-access.json",
				curatorAccess,
			),
		},
		&bytes.Buffer{},
	); !errors.Is(err, evaluation.ErrUnauthorized) {
		t.Fatalf("unauthorized promotion show error = %v, want ErrUnauthorized", err)
	}
	operatorAccess := evaluation.Access{
		Actor: "promotion-operator-1",
		Roles: []evaluation.Role{evaluation.RolePromotionOperator},
	}
	if err := runWithIO(
		context.Background(),
		[]string{
			"promotion", "show",
			"--store", store,
			"--variant", variant.VariantID,
			"--access", writeCLIJSONDescriptor(
				t,
				"promotion-operator-access.json",
				operatorAccess,
			),
		},
		&bytes.Buffer{},
	); err != nil {
		t.Fatalf("authorized promotion show error = %v", err)
	}
}

func TestEvaluationCLIGovernanceBatchRunShowListAndRetry(t *testing.T) {
	store := t.TempDir()
	at := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	cases := []evaluation.EvaluationCase{
		testCLIEvaluationCase("case-cli-batch-a", "source-cli-batch-a", evaluation.SplitUnassigned, at),
		testCLIEvaluationCase("case-cli-batch-b", "source-cli-batch-b", evaluation.SplitUnassigned, at),
	}
	for index, candidate := range cases {
		mutation := evaluation.Mutation{
			IdempotencyKey: "create-cli-batch-" + string(rune('a'+index)),
			Actor:          "batch-ingest-curator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
			Audit: "create candidate for batch governance", At: at,
		}
		if err := runWithIO(context.Background(), []string{
			"evaluation", "case", "create", "--store", store,
			"--input", writeCLIJSONDescriptor(t, "batch-candidate.json", candidate),
			"--mutation", writeCLIJSONDescriptor(t, "batch-create-mutation.json", mutation), "--json",
		}, &bytes.Buffer{}); err != nil {
			t.Fatalf("create batch candidate %d: %v", index, err)
		}
	}
	batchAt := at.Add(time.Minute)
	request := evaluation.GovernanceBatchRequest{
		SchemaVersion: evaluation.GovernanceBatchRequestSchemaVersion,
		BatchID:       "cli-governance-assignment-batch", Operation: evaluation.GovernanceBatchAssign,
		Items: []evaluation.GovernanceBatchItem{
			{EventID: "cli-governance-assignment-a", Assignment: &evaluation.CaseReviewAssignment{
				SchemaVersion: evaluation.CaseReviewAssignmentSchemaVersion, CaseID: cases[0].CaseID,
				ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
				ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
				Reason: "batch assign independent reviewers", AssignedAt: batchAt,
			}},
			{EventID: "cli-governance-assignment-b", Assignment: &evaluation.CaseReviewAssignment{
				SchemaVersion: evaluation.CaseReviewAssignmentSchemaVersion, CaseID: cases[1].CaseID,
				ExpectedGovernanceRevision: 1, ExpectedLabelRevision: 1,
				ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, Blind: true,
				Reason: "batch assign independent reviewers", AssignedAt: batchAt,
			}},
		},
		TerminalEventID: "cli-governance-assignment-terminal", SubmittedAt: batchAt,
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: "cli-governance-assignment-intent", Actor: "batch-assignment-curator",
		Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Audit: "run assignment batch", At: batchAt,
	}
	requestPath := writeCLIJSONDescriptor(t, "governance-batch-request.json", request)
	mutationPath := writeCLIJSONDescriptor(t, "governance-batch-mutation.json", mutation)
	var first evaluationGovernanceBatchOutput
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := runWithIO(context.Background(), []string{
			"evaluation", "governance-batch", "run", "--store", store,
			"--input", requestPath, "--mutation", mutationPath, "--json",
		}, &output); err != nil {
			t.Fatalf("governance batch attempt %d: %v", attempt+1, err)
		}
		var decoded evaluationGovernanceBatchOutput
		decodeCLIOutput(t, output.Bytes(), &decoded)
		if decoded.Batch.Status != evaluation.GovernanceBatchSucceeded || decoded.Batch.Result == nil {
			t.Fatalf("governance batch output = %+v", decoded)
		}
		if attempt == 0 {
			first = decoded
		} else if !reflect.DeepEqual(decoded.Batch, first.Batch) {
			t.Fatalf("governance batch retry drifted: %+v != %+v", decoded.Batch, first.Batch)
		}
	}
	access := evaluation.Access{Actor: "batch-curator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}}
	accessPath := writeCLIJSONDescriptor(t, "governance-batch-access.json", access)
	var shown bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "governance-batch", "show", "--store", store,
		"--batch", request.BatchID, "--access", accessPath, "--json",
	}, &shown); err != nil {
		t.Fatal(err)
	}
	var showOutput evaluationGovernanceBatchOutput
	decodeCLIOutput(t, shown.Bytes(), &showOutput)
	if !reflect.DeepEqual(showOutput.Batch, first.Batch) {
		t.Fatalf("governance batch show drifted: %+v", showOutput)
	}
	var listed bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"evaluation", "governance-batch", "list", "--store", store,
		"--access", accessPath, "--json",
	}, &listed); err != nil {
		t.Fatal(err)
	}
	var listOutput evaluationGovernanceBatchListOutput
	decodeCLIOutput(t, listed.Bytes(), &listOutput)
	if len(listOutput.Batches) != 1 || listOutput.Batches[0].Request.BatchID != request.BatchID {
		t.Fatalf("governance batch list = %+v", listOutput)
	}
}

func testCLIEvaluationCase(
	caseID string,
	sourceID string,
	split evaluation.Split,
	at time.Time,
) evaluation.EvaluationCase {
	evaluationCase := evaluation.EvaluationCase{
		SchemaVersion: evaluation.EvaluationCaseSchemaVersion,
		CaseID:        caseID,
		Type:          evaluation.CaseNegativeClean,
		Provenance: evaluation.SourceProvenance{
			Kind:         evaluation.SourceSynthetic,
			RepositoryID: "repo-cli-evaluation",
			SourceID:     sourceID,
			ObservedAt:   at.Add(-2 * time.Minute),
			CollectedAt:  at.Add(-time.Minute),
			EvidenceRefs: []string{"artifact://argus-test/evidence/" + sourceID},
		},
		LicenseConsent: evaluation.LicenseConsent{
			LicenseID:    "synthetic-test-license",
			Consent:      evaluation.ConsentSynthetic,
			AllowedUses:  []evaluation.UseScope{evaluation.UseCandidatePool},
			Restrictions: []string{},
		},
		Classification:   evaluation.ClassificationInternal,
		Owner:            "evaluation-team",
		InputSnapshotRef: "artifact://argus-test/input/" + sourceID,
		Label: evaluation.Label{
			ExpectedOutcome: evaluation.OutcomeClean, Anchors: []evaluation.LabelAnchor{},
			AnchorRefs: []string{}, SuppressionTargets: []evaluation.SuppressionTarget{},
		},
		LabelPolicyRevision: "label-policy-1",
		ReviewState:         evaluation.ReviewPending,
		DatasetState:        evaluation.DatasetCandidatePool,
		Split:               split,
		CloneGroupID:        "clone-" + sourceID,
		Eligibility:         evaluation.Eligibility{},
		CreatedAt:           at,
	}
	if split == evaluation.SplitHoldout {
		evaluationCase.LicenseConsent.AllowedUses = []evaluation.UseScope{
			evaluation.UseEvaluation,
			evaluation.UsePromotion,
		}
		evaluationCase.ReviewState = evaluation.ReviewApproved
		evaluationCase.DatasetState = evaluation.DatasetActive
		evaluationCase.Eligibility = evaluation.Eligibility{
			Evaluation: true,
			Promotion:  true,
		}
	}
	return evaluationCase
}

func writeCLIJSONDescriptor(t *testing.T, name string, value any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func testCLIExternalGovernedImport(t *testing.T, evaluationCase evaluation.EvaluationCase) evaluation.ExternalGovernedCaseImport {
	t.Helper()
	seed := sha256.Sum256([]byte("argus-cli-test-external-governance-key"))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	digest, err := evaluation.EvaluationCaseSHA256(evaluationCase)
	if err != nil {
		t.Fatal(err)
	}
	evidenceRefs := append([]string{evaluationCase.InputSnapshotRef}, evaluationCase.Provenance.EvidenceRefs...)
	sort.Strings(evidenceRefs)
	at := evaluationCase.CreatedAt.UTC()
	attestation := evaluation.ExternalGovernanceAttestation{
		SchemaVersion: evaluation.ExternalGovernanceAttestationSchemaVersion,
		AttestationID: "cli-attestation-" + evaluationCase.CaseID, CaseID: evaluationCase.CaseID,
		CaseSHA256: digest, Authority: "cli-test-governance", KeyID: "cli-test-key-1",
		PolicyRevision: "cli-review-policy-1", ReviewerIDs: []string{"cli-external-reviewer-a", "cli-external-reviewer-b"},
		AdjudicatorID: "cli-external-adjudicator-c", EvidenceRefs: evidenceRefs,
		Decision: "approved", ReviewedAt: at, IssuedAt: at,
	}
	payload, err := attestation.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	attestation.SignatureBase64 = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return evaluation.ExternalGovernedCaseImport{
		SchemaVersion: evaluation.ExternalGovernedCaseImportSchemaVersion, Case: evaluationCase,
		Attestation: attestation,
		TrustedKey: evaluation.TrustedGovernanceKey{
			SchemaVersion: evaluation.TrustedGovernanceKeySchemaVersion,
			Authority:     attestation.Authority, KeyID: attestation.KeyID, Revision: "cli-trust-v1",
			PublicKeyBase64:        base64.StdEncoding.EncodeToString(publicKey),
			RepositoryIDs:          []string{evaluationCase.Provenance.RepositoryID},
			AllowedClassifications: []evaluation.Classification{evaluationCase.Classification},
			ValidFrom:              at.Add(-time.Hour), ValidUntil: at.Add(time.Hour),
		},
		ImportedAt: at,
	}
}

func decodeCLIOutput(t *testing.T, data []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode CLI output: %v\n%s", err, data)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("CLI output has invalid trailing data: %v\n%s", err, data)
	}
}
