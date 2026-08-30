package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/controlplane"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/feedback"
	"argus.local/argus/internal/findingdecision"
	"argus.local/argus/internal/publication"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestReviewRunAPIUsesWatermarkedPaginationAndTracksUncommittedRuns(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	appendRunEvent(t, runs, "run-a", apiTestEpoch)
	appendRunEvent(t, runs, "run-b", apiTestEpoch.Add(time.Second))
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "review-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionReviewRead}, ProfileRevision: "review-reader-v1",
	}
	handler, err := NewHandler(Services{Runs: runs}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, authenticatedRequest(http.MethodGet, "/v1/review-runs?limit=1", nil))
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first list status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	var first struct {
		Data struct {
			Items      []runrepo.HistoryEntry `json:"items"`
			NextCursor string                 `json:"next_cursor"`
		} `json:"data"`
	}
	decodeResponse(t, firstResponse.Body.Bytes(), &first)
	if len(first.Data.Items) != 1 || first.Data.Items[0].RunID != "run-b" ||
		first.Data.NextCursor == "" {
		t.Fatalf("first page = %+v", first.Data)
	}

	// A later run sorts before both existing entries. The continuation cursor
	// must retain the original run-index watermark and therefore return run-a,
	// not duplicate run-b or admit run-c into the frozen traversal.
	appendRunEvent(t, runs, "run-c", apiTestEpoch.Add(2*time.Second))
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, authenticatedRequest(
		http.MethodGet, "/v1/review-runs?limit=1&cursor="+first.Data.NextCursor, nil,
	))
	var second struct {
		Data struct {
			Items []runrepo.HistoryEntry `json:"items"`
		} `json:"data"`
	}
	decodeResponse(t, secondResponse.Body.Bytes(), &second)
	if secondResponse.Code != http.StatusOK || len(second.Data.Items) != 1 ||
		second.Data.Items[0].RunID != "run-a" {
		t.Fatalf("second page status=%d page=%+v", secondResponse.Code, second.Data)
	}

	freshResponse := httptest.NewRecorder()
	handler.ServeHTTP(freshResponse, authenticatedRequest(http.MethodGet, "/v1/review-runs?limit=1", nil))
	var fresh struct {
		Data struct {
			Items []runrepo.HistoryEntry `json:"items"`
		} `json:"data"`
	}
	decodeResponse(t, freshResponse.Body.Bytes(), &fresh)
	if freshResponse.Code != http.StatusOK || len(fresh.Data.Items) != 1 ||
		fresh.Data.Items[0].RunID != "run-c" {
		t.Fatalf("fresh page status=%d page=%+v", freshResponse.Code, fresh.Data)
	}

	detailResponse := httptest.NewRecorder()
	handler.ServeHTTP(detailResponse, authenticatedRequest(http.MethodGet, "/v1/review-runs/run-a", nil))
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("pending detail status=%d body=%s", detailResponse.Code, detailResponse.Body.String())
	}
	var detail struct {
		Data ReviewRunDetail `json:"data"`
	}
	decodeResponse(t, detailResponse.Body.Bytes(), &detail)
	if detail.Data.History.RunID != "run-a" || detail.Data.Committed || detail.Data.Run != nil {
		t.Fatalf("pending detail = %+v", detail.Data)
	}
	var raw map[string]any
	decodeResponse(t, detailResponse.Body.Bytes(), &raw)
	if _, exists := raw["data"].(map[string]any)["run"]; exists {
		t.Fatal("pending detail emitted a null run instead of omitting uncommitted data")
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, authenticatedRequest(http.MethodGet, "/v1/review-runs/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
	malformed := httptest.NewRecorder()
	handler.ServeHTTP(malformed, authenticatedRequest(http.MethodGet, "/v1/review-runs?cursor=not-base64!", nil))
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor status=%d body=%s", malformed.Code, malformed.Body.String())
	}
}

func TestReviewRunImpactAPIReportsUnresolvedLineageAndValidatesSelector(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	appendRunEvent(t, runs, "run-pending", apiTestEpoch)
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "review-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionReviewRead}, ProfileRevision: "review-reader-v1",
	}
	handler, err := NewHandler(Services{Runs: runs}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodGet,
		"/v1/review-run-impacts?kind=workflow&id=argus-default-review&revision=1&limit=5",
		nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("impact query status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded struct {
		Data ReviewRunImpactPage `json:"data"`
	}
	decodeResponse(t, response.Body.Bytes(), &decoded)
	if len(decoded.Data.Items) != 0 || decoded.Data.Coverage.Complete ||
		decoded.Data.Coverage.HistoryEntries != 1 || len(decoded.Data.Coverage.Gaps) != 1 ||
		decoded.Data.Coverage.Gaps[0].RunID != "run-pending" {
		t.Fatalf("impact response = %+v", decoded.Data)
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, authenticatedRequest(
		http.MethodGet,
		"/v1/review-run-impacts?kind=model&id=model-a&revision=1&sha256=not-a-digest",
		nil,
	))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid selector status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	forbidden := httptest.NewRecorder()
	handler.ServeHTTP(forbidden, httptest.NewRequest(
		http.MethodGet,
		"/v1/review-run-impacts?kind=model&id=model-a&revision=1",
		nil,
	))
	if forbidden.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated impact status=%d", forbidden.Code)
	}
}

func TestReviewFindingAPIJoinsTypedFactsWithoutPublicationPayloads(t *testing.T) {
	detail := controlplane.FindingDetail{
		Run:               runmodel.ReviewRun{RunID: "run-a"},
		Finding:           &reviewcore.Finding{ID: "finding-a"},
		Decisions:         []reviewcore.FindingDecision{},
		GovernedDecisions: []contractsv1alpha1.GovernedFindingDecision{},
		HumanDecisions: []findingdecision.Decision{{
			DecisionID: "human-decision-a", RunID: "run-a", FindingID: "finding-a",
		}},
		Publications: []publication.Record{{State: publication.StatePublished}},
		Feedback: []feedback.Feedback{{
			FeedbackID: "feedback-a", RunID: "run-a", FindingID: "finding-a",
		}},
		Outcomes: []feedback.Outcome{{
			OutcomeID: "outcome-a", RunID: "run-a", FindingID: "finding-a",
		}},
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "review-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionReviewRead}, ProfileRevision: "review-reader-v1",
	}
	handler, err := NewHandler(Services{Findings: findingReaderStub{detail: detail}}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodGet, "/v1/review-runs/run-a/findings/finding-a", nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("finding status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded struct {
		Data FindingView `json:"data"`
	}
	decodeResponse(t, response.Body.Bytes(), &decoded)
	if decoded.Data.Run.RunID != "run-a" || decoded.Data.Finding == nil ||
		decoded.Data.Finding.ID != "finding-a" || len(decoded.Data.HumanDecisions) != 1 ||
		len(decoded.Data.Feedback) != 1 || len(decoded.Data.Outcomes) != 1 {
		t.Fatalf("finding view = %+v", decoded.Data)
	}
	var raw map[string]any
	decodeResponse(t, response.Body.Bytes(), &raw)
	if _, exists := raw["data"].(map[string]any)["publications"]; exists {
		t.Fatal("finding API exposed provider publication payloads")
	}

	missingHandler, err := NewHandler(Services{Findings: findingReaderStub{
		err: controlplane.ErrFindingNotFound,
	}}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	missing := httptest.NewRecorder()
	missingHandler.ServeHTTP(missing, authenticatedRequest(
		http.MethodGet, "/v1/review-runs/run-a/findings/missing", nil,
	))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing finding status=%d body=%s", missing.Code, missing.Body.String())
	}

	unavailable := httptest.NewRecorder()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	runsOnly, err := NewHandler(Services{Runs: runs}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	runsOnly.ServeHTTP(unavailable, authenticatedRequest(
		http.MethodGet, "/v1/review-runs/run-a/findings/finding-a", nil,
	))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable finding status=%d body=%s", unavailable.Code, unavailable.Body.String())
	}
}

func TestReviewFindingWriteAPIsInjectPrincipalAndKeepFactsSeparate(t *testing.T) {
	now := time.Date(2026, 8, 26, 4, 0, 0, 0, time.UTC)
	service := &findingWriteStub{}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "review-operator",
		ActorKind: findingdecision.ActorHuman, Roles: []evaluation.Role{},
		FindingRoles:    []findingdecision.Role{findingdecision.RoleFindingReviewer},
		Permissions:     []Permission{PermissionReviewRead, PermissionReviewWrite},
		ProfileRevision: "review-writer-v1",
	}
	handler, err := NewHandler(Services{Findings: service}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	mutation := MutationInput{
		SchemaVersion:  MutationInputSchemaVersion,
		IdempotencyKey: "finding-write-1", Audit: "operator reviewed finding evidence", At: now,
	}
	decisionBody, err := json.Marshal(FindingDecisionWriteCommand{
		SchemaVersion: FindingDecisionWriteCommandSchemaVersion,
		Action:        findingdecision.ActionHumanReview, ReasonCode: "manual_review_requested",
		EvidenceRefs: []findingdecision.EvidenceRef{{Authority: "local-ui", ID: "review-note-1"}},
		OccurredAt:   now.Add(-time.Minute), Mutation: mutation,
	})
	if err != nil {
		t.Fatal(err)
	}
	decisionResponse := httptest.NewRecorder()
	handler.ServeHTTP(decisionResponse, authenticatedRequest(
		http.MethodPost,
		"/v1/review-runs/run-a/findings/finding-a/decisions",
		strings.NewReader(string(decisionBody)),
	))
	if decisionResponse.Code != http.StatusCreated || service.decisionRequest.RunID != "run-a" ||
		service.decisionRequest.FindingID != "finding-a" ||
		service.decisionMutation.Actor.ID != principal.Actor ||
		service.decisionMutation.Actor.Kind != principal.ActorKind {
		t.Fatalf("decision write status=%d request=%+v mutation=%+v body=%s",
			decisionResponse.Code, service.decisionRequest, service.decisionMutation,
			decisionResponse.Body.String())
	}

	mutation.IdempotencyKey = "finding-feedback-1"
	feedbackBody, err := json.Marshal(FeedbackWriteCommand{
		SchemaVersion: FeedbackWriteCommandSchemaVersion,
		FeedbackID:    "feedback-api-1", Action: feedback.FeedbackAccept,
		Source:     feedback.Source{Kind: feedback.SourceAPI, ID: "local-platform-api"},
		OccurredAt: now.Add(-time.Minute),
		SourceRefs: []feedback.SourceRef{{
			Kind: feedback.SourceRefManualObservation, Authority: "local-operator",
			ID: "observation-1",
		}},
		Mutation: mutation,
	})
	if err != nil {
		t.Fatal(err)
	}
	feedbackResponse := httptest.NewRecorder()
	handler.ServeHTTP(feedbackResponse, authenticatedRequest(
		http.MethodPost,
		"/v1/review-runs/run-a/findings/finding-a/feedback",
		strings.NewReader(string(feedbackBody)),
	))
	if feedbackResponse.Code != http.StatusCreated || service.feedback.RunID != "run-a" ||
		service.feedback.FindingID != "finding-a" || service.feedback.Actor.ID != principal.Actor ||
		service.feedback.Actor.Kind != feedback.ActorHuman {
		t.Fatalf("feedback write status=%d fact=%+v body=%s",
			feedbackResponse.Code, service.feedback, feedbackResponse.Body.String())
	}
	var feedbackResult struct {
		Data FeedbackWriteResult `json:"data"`
	}
	decodeResponse(t, feedbackResponse.Body.Bytes(), &feedbackResult)
	if feedbackResult.Data.Eligibility.Use != feedback.EvaluationUseCandidateOnly {
		t.Fatalf("feedback eligibility = %+v", feedbackResult.Data.Eligibility)
	}

	mutation.IdempotencyKey = "finding-outcome-1"
	outcomeBody, err := json.Marshal(OutcomeWriteCommand{
		SchemaVersion: OutcomeWriteCommandSchemaVersion,
		OutcomeID:     "outcome-api-1", State: feedback.OutcomeFixed,
		Source:     feedback.Source{Kind: feedback.SourceAPI, ID: "local-platform-api"},
		OccurredAt: now.Add(-time.Minute),
		Window:     feedback.AttributionWindow{Start: now.Add(-time.Hour), End: now},
		SourceRefs: []feedback.SourceRef{{
			Kind: feedback.SourceRefChange, Authority: "local-git", ID: "fix-change-1",
		}},
		Mutation: mutation,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcomeResponse := httptest.NewRecorder()
	handler.ServeHTTP(outcomeResponse, authenticatedRequest(
		http.MethodPost,
		"/v1/review-runs/run-a/findings/finding-a/outcomes",
		strings.NewReader(string(outcomeBody)),
	))
	if outcomeResponse.Code != http.StatusCreated || service.outcome.RunID != "run-a" ||
		service.outcome.FindingID != "finding-a" || service.outcome.Actor.ID != principal.Actor ||
		service.outcome.Actor.Kind != feedback.ActorHuman {
		t.Fatalf("outcome write status=%d fact=%+v body=%s",
			outcomeResponse.Code, service.outcome, outcomeResponse.Body.String())
	}

	selfDeclared := strings.Replace(
		string(feedbackBody), `"feedback_id":"feedback-api-1"`,
		`"feedback_id":"feedback-api-2","actor":{"kind":"service","id":"forged"}`, 1,
	)
	forged := httptest.NewRecorder()
	handler.ServeHTTP(forged, authenticatedRequest(
		http.MethodPost,
		"/v1/review-runs/run-a/findings/finding-a/feedback",
		strings.NewReader(selfDeclared),
	))
	if forged.Code != http.StatusBadRequest {
		t.Fatalf("self-declared actor status=%d body=%s", forged.Code, forged.Body.String())
	}

	readOnly := principal
	readOnly.ActorKind = ""
	readOnly.FindingRoles = nil
	readOnly.Permissions = []Permission{PermissionReviewRead}
	readHandler, err := NewHandler(Services{Findings: service}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := httptest.NewRecorder()
	readHandler.ServeHTTP(forbidden, authenticatedRequest(
		http.MethodPost,
		"/v1/review-runs/run-a/findings/finding-a/feedback",
		strings.NewReader(string(feedbackBody)),
	))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("read-only feedback write status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
}

func TestFindingMutationErrorMappingSeparatesConflictAuthorityAndCorruption(t *testing.T) {
	handler := &Handler{}
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "conflict", err: findingdecision.ErrConflict, status: http.StatusConflict, code: "finding_write_conflict"},
		{name: "invalid correction", err: feedback.ErrInvalidCorrection, status: http.StatusConflict, code: "finding_write_conflict"},
		{name: "unauthorized", err: findingdecision.ErrUnauthorized, status: http.StatusForbidden, code: "finding_write_forbidden"},
		{name: "corrupt", err: feedback.ErrCorrupt, status: http.StatusInternalServerError, code: "internal_error"},
		{name: "validation", err: errors.New("observation evidence is invalid"), status: http.StatusBadRequest, code: "finding_write_rejected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.writeFindingMutationError(response, test.err)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != test.code {
				t.Fatalf("code = %q, want %q", body.Code, test.code)
			}
			if test.status == http.StatusInternalServerError && strings.Contains(body.Message, test.err.Error()) {
				t.Fatalf("internal corruption detail leaked: %q", body.Message)
			}
		})
	}
}

type findingReaderStub struct {
	detail controlplane.FindingDetail
	err    error
}

type findingWriteStub struct {
	decisionRequest  findingdecision.Request
	decisionMutation findingdecision.Mutation
	feedback         feedback.Feedback
	outcome          feedback.Outcome
}

func (stub *findingWriteStub) Finding(string, string) (controlplane.FindingDetail, error) {
	return controlplane.FindingDetail{}, nil
}

func (stub *findingWriteStub) RecordDecision(
	_ context.Context,
	request findingdecision.Request,
	mutation findingdecision.Mutation,
) (findingdecision.Decision, error) {
	stub.decisionRequest = request
	stub.decisionMutation = mutation
	return findingdecision.Decision{
		SchemaVersion: findingdecision.DecisionSchemaVersion,
		DecisionID:    "decision-api-1", RunID: request.RunID, FindingID: request.FindingID,
		Action: request.Action, ReasonCode: request.ReasonCode,
	}, nil
}

func (stub *findingWriteStub) RecordFeedback(
	_ context.Context,
	fact feedback.Feedback,
) (feedback.Feedback, error) {
	stub.feedback = fact
	return fact, nil
}

func (stub *findingWriteStub) RecordOutcome(
	_ context.Context,
	fact feedback.Outcome,
) (feedback.Outcome, error) {
	stub.outcome = fact
	return fact, nil
}

func (stub findingReaderStub) Finding(string, string) (controlplane.FindingDetail, error) {
	return stub.detail, stub.err
}

func (stub findingReaderStub) RecordDecision(
	context.Context,
	findingdecision.Request,
	findingdecision.Mutation,
) (findingdecision.Decision, error) {
	return findingdecision.Decision{}, stub.err
}

func (stub findingReaderStub) RecordFeedback(
	context.Context,
	feedback.Feedback,
) (feedback.Feedback, error) {
	return feedback.Feedback{}, stub.err
}

func (stub findingReaderStub) RecordOutcome(
	context.Context,
	feedback.Outcome,
) (feedback.Outcome, error) {
	return feedback.Outcome{}, stub.err
}

func TestReviewRunPermissionIsIndependentAndUnavailableServiceFailsClosed(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	configOnly := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "config-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionConfigRead}, ProfileRevision: "config-reader-v1",
	}
	forbiddenHandler, err := NewHandler(Services{Runs: runs}, configOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := httptest.NewRecorder()
	forbiddenHandler.ServeHTTP(forbidden, authenticatedRequest(http.MethodGet, "/v1/review-runs", nil))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}

	reviewPrincipal := configOnly
	reviewPrincipal.Actor = "review-reader"
	reviewPrincipal.Permissions = []Permission{PermissionReviewRead}
	reviewPrincipal.ProfileRevision = "review-reader-v1"
	configs, err := configrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	unavailableHandler, err := NewHandler(Services{Config: configs}, reviewPrincipal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	// Prove strict principal JSON accepts the new permission without granting
	// an Evaluation role.
	encoded, err := json.Marshal(reviewPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePrincipal(encoded); err != nil {
		t.Fatalf("DecodePrincipal(review-only) error = %v", err)
	}

	unavailable := httptest.NewRecorder()
	unavailableHandler.ServeHTTP(unavailable, authenticatedRequest(http.MethodGet, "/v1/review-runs", nil))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status=%d body=%s", unavailable.Code, unavailable.Body.String())
	}

	// The same principal cannot cross into a service it was not granted.
	dashboard := httptest.NewRecorder()
	unavailableHandler.ServeHTTP(dashboard, authenticatedRequest(http.MethodGet, "/v1/dashboard/snapshots", nil))
	if dashboard.Code != http.StatusForbidden {
		t.Fatalf("dashboard status=%d body=%s", dashboard.Code, dashboard.Body.String())
	}
}

func appendRunEvent(t *testing.T, repository *runrepo.Repository, runID string, at time.Time) {
	t.Helper()
	if err := repository.AppendEvent(runID+"-created", at, runrepo.RunEvent{
		RunID: runID, Kind: runmodel.RunKindReview,
		Status: runmodel.RunStatusPending, EventType: runrepo.EventRunCreated,
	}); err != nil {
		t.Fatalf("AppendEvent(%s) error = %v", runID, err)
	}
}
