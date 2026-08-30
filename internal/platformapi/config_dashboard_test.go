package platformapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"argus.local/argus/internal/analytics"
	"argus.local/argus/internal/analyticsadapter"
	"argus.local/argus/internal/configdefaults"
	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/workflow"
)

func TestConfigLifecycleAPIUsesFixedPrincipalAndReturnsCoherentHistory(t *testing.T) {
	configStore, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := configrepo.New(configStore)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "config-maintainer", Roles: []evaluation.Role{},
		Permissions:     []Permission{PermissionConfigRead, PermissionConfigWrite},
		ProfileRevision: "config-maintainer-v1",
	}
	handler, err := NewHandler(Services{Config: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	first := testConfigRevision("platform-default", "1", 100)
	second := testConfigRevision("platform-default", "2", 200)
	createConfigThroughAPI(t, handler, first, "create-config-v1", apiTestEpoch)
	transitionConfigThroughAPI(t, handler, first, "validate", "validate-config-v1", apiTestEpoch.Add(time.Second), nil)
	transitionConfigThroughAPI(t, handler, first, "publish", "publish-config-v1", apiTestEpoch.Add(2*time.Second), &configrepo.Rollout{Percentage: 100})
	createConfigThroughAPI(t, handler, second, "create-config-v2", apiTestEpoch.Add(3*time.Second))
	transitionConfigThroughAPI(t, handler, second, "validate", "validate-config-v2", apiTestEpoch.Add(4*time.Second), nil)
	transitionConfigThroughAPI(t, handler, second, "publish", "publish-config-v2", apiTestEpoch.Add(5*time.Second), &configrepo.Rollout{Percentage: 10, Seed: "api-canary"})
	transitionConfigThroughAPI(t, handler, second, "advance", "advance-config-v2", apiTestEpoch.Add(6*time.Second), &configrepo.Rollout{Percentage: 40, Seed: "api-canary"})
	transitionConfigThroughAPI(t, handler, second, "rollback", "rollback-config-v2", apiTestEpoch.Add(7*time.Second), nil)

	detailResponse := httptest.NewRecorder()
	handler.ServeHTTP(detailResponse, authenticatedRequest(
		http.MethodGet, "/v1/config/revisions/platform-default/2", nil,
	))
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailResponse.Code, detailResponse.Body.String())
	}
	var detail struct {
		Data configrepo.RecordDetail `json:"data"`
	}
	decodeResponse(t, detailResponse.Body.Bytes(), &detail)
	if detail.Data.Record.Status != configrepo.StatusRolledBack || len(detail.Data.History) != 5 ||
		detail.Data.Record.UpdatedBy != principal.Actor {
		t.Fatalf("config detail = %+v", detail.Data)
	}
	for _, entry := range detail.Data.History {
		if entry.Actor != principal.Actor {
			t.Fatalf("history actor = %q, want fixed principal %q", entry.Actor, principal.Actor)
		}
	}
	third, err := configdefaults.Revision(configdefaults.Options{
		ID: "platform-default", Revision: "3", MaxFiles: 300,
		MaxPatchBytes: 1 << 20, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20,
		MaxAttempts: 2, AllowedModes: []string{"diff", "scope", "selection"},
		TargetInclude: []string{"**"}, TargetExclude: []string{},
	}, workflow.DefaultReviewDefinition())
	if err != nil {
		t.Fatal(err)
	}
	createConfigThroughAPI(t, handler, third, "create-config-v3", apiTestEpoch.Add(8*time.Second))
	transitionConfigThroughAPI(t, handler, third, "validate", "validate-config-v3", apiTestEpoch.Add(9*time.Second), nil)
	transitionConfigThroughAPI(t, handler, third, "publish", "publish-config-v3", apiTestEpoch.Add(10*time.Second), &configrepo.Rollout{Percentage: 100})

	resolutionBody, _ := json.Marshal(ConfigResolutionQuery{
		SchemaVersion: ConfigResolutionQuerySchemaVersion,
		Context: reviewconfig.ResolutionContext{
			TenantID: "tenant-local", OrganizationID: "organization-local",
			RepositoryID: "repository-local", Path: "internal/service.go",
			InvocationID: "config-resolution-api-test",
		},
	})
	resolutionResponse := httptest.NewRecorder()
	handler.ServeHTTP(resolutionResponse, authenticatedRequest(
		http.MethodPost, "/v1/config/resolutions", bytes.NewReader(resolutionBody),
	))
	if resolutionResponse.Code != http.StatusOK {
		t.Fatalf("resolution status=%d body=%s", resolutionResponse.Code, resolutionResponse.Body.String())
	}
	var resolution struct {
		Data ConfigResolutionView `json:"data"`
	}
	decodeResponse(t, resolutionResponse.Body.Bytes(), &resolution)
	if resolution.Data.Bundle.Context.InvocationID != "config-resolution-api-test" ||
		resolution.Data.Bundle.Target.MaxFiles != 300 ||
		resolution.Data.Receipt.Context != resolution.Data.Bundle.Context {
		t.Fatalf("config resolution = %+v", resolution.Data)
	}
	if err := resolution.Data.Receipt.ValidateAgainst(resolution.Data.Bundle); err != nil {
		t.Fatalf("resolution receipt = %v", err)
	}

	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, authenticatedRequest(
		http.MethodGet, "/v1/config/revisions?limit=1", nil,
	))
	if listResponse.Code != http.StatusOK || !bytes.Contains(listResponse.Body.Bytes(), []byte(`"next_cursor"`)) {
		t.Fatalf("config list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	readerOnly := principal
	readerOnly.Permissions = []Permission{PermissionConfigRead}
	readerHandler, err := NewHandler(Services{Config: repository}, readerOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := httptest.NewRecorder()
	body, _ := json.Marshal(ConfigTransitionCommand{
		SchemaVersion: ConfigTransitionCommandSchemaVersion,
		Mutation:      MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "forbidden-transition", Audit: "must not execute", At: apiTestEpoch.Add(time.Hour)},
	})
	readerHandler.ServeHTTP(forbidden, authenticatedRequest(
		http.MethodPost, "/v1/config/revisions/platform-default/1/validate", bytes.NewReader(body),
	))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("reader write status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	readResolution := httptest.NewRecorder()
	readerHandler.ServeHTTP(readResolution, authenticatedRequest(
		http.MethodPost, "/v1/config/resolutions", bytes.NewReader(resolutionBody),
	))
	if readResolution.Code != http.StatusOK {
		t.Fatalf("reader resolution status=%d body=%s", readResolution.Code, readResolution.Body.String())
	}

	invalidResolution := httptest.NewRecorder()
	readerHandler.ServeHTTP(invalidResolution, authenticatedRequest(
		http.MethodPost, "/v1/config/resolutions",
		bytes.NewBufferString(`{"schema_version":"argus.local_api_config_resolution_query.v1alpha1","context":{},"actor":"caller"}`),
	))
	if invalidResolution.Code != http.StatusBadRequest {
		t.Fatalf("invalid resolution status=%d body=%s", invalidResolution.Code, invalidResolution.Body.String())
	}
}

func TestDashboardAPIListsAndReadsImmutableProjectionWithoutFacts(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	rebuilder, err := analyticsadapter.New(runs, nil, store, emptyEvaluationSource{})
	if err != nil {
		t.Fatal(err)
	}
	request := analyticsadapter.RebuildRequest{
		SnapshotID: "dashboard-api-snapshot-1",
		Scope:      analyticsadapter.Scope{TenantID: "local", OrganizationID: "local", RepositoryID: "repo-local"},
		Window: analytics.TimeWindow{
			StartInclusive: apiTestEpoch.Add(-time.Hour), EndExclusive: apiTestEpoch,
		},
		GroupBy: []analytics.DimensionName{}, BuiltAt: apiTestEpoch.Add(time.Minute),
	}
	if _, err := rebuilder.Rebuild(context.Background(), request); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	query, err := analyticsadapter.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "dashboard-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionDashboardRead}, ProfileRevision: "dashboard-reader-v1",
	}
	handler, err := NewHandler(Services{Dashboard: query}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedRequest(http.MethodGet, "/v1/dashboard/snapshots", nil))
	if list.Code != http.StatusOK || !bytes.Contains(list.Body.Bytes(), []byte(request.SnapshotID)) {
		t.Fatalf("dashboard list status=%d body=%s", list.Code, list.Body.String())
	}
	show := httptest.NewRecorder()
	handler.ServeHTTP(show, authenticatedRequest(
		http.MethodGet, "/v1/dashboard/snapshots/"+request.SnapshotID, nil,
	))
	if show.Code != http.StatusOK {
		t.Fatalf("dashboard show status=%d body=%s", show.Code, show.Body.String())
	}
	var response struct {
		Data DashboardSnapshotView `json:"data"`
	}
	decodeResponse(t, show.Body.Bytes(), &response)
	if response.Data.SnapshotID != request.SnapshotID || response.Data.Scope.RepositoryID != "repo-local" {
		t.Fatalf("dashboard view = %+v", response.Data)
	}
	var raw map[string]any
	decodeResponse(t, show.Body.Bytes(), &raw)
	data := raw["data"].(map[string]any)
	if _, exists := data["facts"]; exists {
		t.Fatal("dashboard read API exposed raw facts")
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, authenticatedRequest(
		http.MethodGet, "/v1/dashboard/snapshots/missing-snapshot", nil,
	))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing dashboard status=%d body=%s", missing.Code, missing.Body.String())
	}
}

type emptyEvaluationSource struct{}

func (emptyEvaluationSource) EvaluationFacts(
	context.Context,
	analyticsadapter.Scope,
	analytics.TimeWindow,
) (analyticsadapter.EvaluationBatch, error) {
	return analyticsadapter.EvaluationBatch{
		Completeness: analytics.CompletenessComplete, IncompleteReasons: []string{},
		Facts:              []analytics.ExperimentFact{},
		RepeatabilityFacts: []analytics.RepeatabilityFact{},
	}, nil
}

func testConfigRevision(id, revision string, maxFiles int) reviewconfig.Revision {
	return reviewconfig.Revision{
		SchemaVersion: reviewconfig.RevisionSchemaVersion, ID: id, Revision: revision,
		Scope: reviewconfig.ScopePlatform, Selector: reviewconfig.Selector{},
		Patch: reviewconfig.ConfigPatch{Target: &reviewconfig.TargetPatch{MaxFiles: &maxFiles}},
	}
}

func createConfigThroughAPI(
	t *testing.T,
	handler http.Handler,
	revision reviewconfig.Revision,
	idempotencyKey string,
	at time.Time,
) {
	t.Helper()
	command := ConfigCreateCommand{
		SchemaVersion: ConfigCreateCommandSchemaVersion, Revision: revision,
		Mutation: MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: idempotencyKey, Audit: "create configuration revision", At: at},
	}
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodPost, "/v1/config/revisions", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("create config status=%d body=%s", response.Code, response.Body.String())
	}
}

func transitionConfigThroughAPI(
	t *testing.T,
	handler http.Handler,
	revision reviewconfig.Revision,
	action string,
	idempotencyKey string,
	at time.Time,
	rollout *configrepo.Rollout,
) {
	t.Helper()
	command := ConfigTransitionCommand{
		SchemaVersion: ConfigTransitionCommandSchemaVersion,
		Mutation:      MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: idempotencyKey, Audit: action + " configuration revision", At: at},
		Rollout:       rollout,
	}
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/config/revisions/"+revision.ID+"/"+revision.Revision+"/"+action,
		bytes.NewReader(body),
	))
	if response.Code != http.StatusOK {
		t.Fatalf("%s config status=%d body=%s", action, response.Code, response.Body.String())
	}
}
