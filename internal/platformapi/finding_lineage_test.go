package platformapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/findingdecision"
	"argus.local/argus/internal/findinglineage"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestFindingLineageHTTPBuildListAndGet(t *testing.T) {
	stub := &findingLineageServiceStub{}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "local-reviewer",
		ActorKind:       findingdecision.ActorHuman,
		Roles:           []evaluation.Role{evaluation.RoleDatasetCurator},
		FindingRoles:    []findingdecision.Role{findingdecision.RoleFindingReviewer},
		Permissions:     []Permission{PermissionReviewRead, PermissionReviewWrite},
		ProfileRevision: "reviewer-v1",
	}
	handler, err := NewHandler(Services{Lineages: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	command := findinglineage.BuildRequest{
		SchemaVersion:  findinglineage.BuildRequestSchemaVersion,
		IdempotencyKey: "api-lineage-build", BaselineRunID: "run-before", VariantRunID: "run-after",
		Policy: findinglineage.DefaultPolicy(),
	}
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodPost, "/v1/finding-lineages", bytes.NewReader(body)))
	if response.Code != http.StatusOK || stub.builds != 1 || stub.lastRequest.IdempotencyKey != command.IdempotencyKey {
		t.Fatalf("build status=%d body=%s stub=%+v", response.Code, response.Body.String(), stub)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/v1/finding-lineages?run_id=run-after&repository_id=repository-1&limit=10", nil))
	if response.Code != http.StatusOK || stub.lastFilter.RunID != "run-after" || stub.lastFilter.RepositoryID != "repository-1" {
		t.Fatalf("list status=%d body=%s filter=%+v", response.Code, response.Body.String(), stub.lastFilter)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/v1/finding-lineages/finding-lineage-api", nil))
	if response.Code != http.StatusOK || stub.gets != 1 {
		t.Fatalf("get status=%d body=%s", response.Code, response.Body.String())
	}
}

type findingLineageServiceStub struct {
	builds, gets int
	lastRequest  findinglineage.BuildRequest
	lastFilter   findinglineage.ListFilter
}

func (stub *findingLineageServiceStub) Build(_ context.Context, request findinglineage.BuildRequest) (findinglineage.Record, error) {
	stub.builds++
	stub.lastRequest = request
	return stub.record(), nil
}
func (stub *findingLineageServiceStub) Get(string) (findinglineage.Record, error) {
	stub.gets++
	return stub.record(), nil
}
func (stub *findingLineageServiceStub) List(filter findinglineage.ListFilter) ([]findinglineage.Record, error) {
	stub.lastFilter = filter
	return []findinglineage.Record{stub.record()}, nil
}
func (*findingLineageServiceStub) record() findinglineage.Record {
	return findinglineage.Record{Lineage: structLineage("finding-lineage-api")}
}

func structLineage(id string) (lineage contractsv1alpha1.FindingLineage) {
	lineage.LineageID = id
	return lineage
}
