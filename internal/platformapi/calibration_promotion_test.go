package platformapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/calibrationpromotion"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/promotionmonitor"
)

type calibrationPromotionStub struct{ plans []calibrationpromotion.Plan }

func (stub *calibrationPromotionStub) Prepare(context.Context, calibrationpromotion.PrepareRequest, evaluation.Mutation) (calibrationpromotion.Plan, error) {
	return calibrationpromotion.Plan{}, nil
}
func (stub *calibrationPromotionStub) RecordGate(context.Context, calibrationpromotion.GateRequest, evaluation.Mutation) (calibrationpromotion.Plan, error) {
	return calibrationpromotion.Plan{}, nil
}
func (stub *calibrationPromotionStub) Activate(context.Context, string, configrepo.Rollout, evaluation.Mutation) (calibrationpromotion.Plan, error) {
	return calibrationpromotion.Plan{}, nil
}
func (stub *calibrationPromotionStub) Rollback(context.Context, string, evaluation.Mutation) (calibrationpromotion.Plan, error) {
	return calibrationpromotion.Plan{}, nil
}
func (stub *calibrationPromotionStub) Get(id string, _ evaluation.Access) (calibrationpromotion.Plan, error) {
	for _, plan := range stub.plans {
		if plan.Request.PlanID == id {
			return plan, nil
		}
	}
	return calibrationpromotion.Plan{}, calibrationpromotion.ErrNotFound
}
func (stub *calibrationPromotionStub) List(evaluation.Access) ([]calibrationpromotion.Plan, error) {
	return stub.plans, nil
}

func TestCalibrationPromotionRoutesRequireBothPermissionFamiliesAndStrictBodies(t *testing.T) {
	stub := &calibrationPromotionStub{plans: []calibrationpromotion.Plan{{Request: calibrationpromotion.PrepareRequest{PlanID: "plan-1"}}}}
	principal := Principal{SchemaVersion: PrincipalSchemaVersion, Actor: "promotion-operator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator, evaluation.RolePromotionOperator}, Permissions: []Permission{PermissionConfigRead, PermissionConfigWrite, PermissionEvaluationRead, PermissionEvaluationWrite}, ProfileRevision: "promotion-v1"}
	handler, err := NewHandler(Services{CalibrationPromotion: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/v1/calibration/promotion/plans?limit=10", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "plan-1") {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	unknown := authenticatedRequest(http.MethodPost, "/v1/calibration/promotion/plans", strings.NewReader(`{"schema_version":"argus.local_api_calibration_promotion_prepare_command.v1alpha1","unexpected":true}`))
	unknown.Header.Set("Content-Type", "application/json")
	strict := httptest.NewRecorder()
	handler.ServeHTTP(strict, unknown)
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict status=%d body=%s", strict.Code, strict.Body.String())
	}
	missingConfig := principal
	missingConfig.Permissions = []Permission{PermissionEvaluationRead, PermissionEvaluationWrite}
	deniedHandler, err := NewHandler(Services{CalibrationPromotion: stub}, missingConfig, testToken)
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	deniedHandler.ServeHTTP(denied, authenticatedRequest(http.MethodGet, "/v1/calibration/promotion/plans", nil))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("missing config permission status=%d", denied.Code)
	}
}

type promotionMonitorStub struct {
	items []promotionmonitor.Summary
	last  promotionmonitor.BuildRequest
}

func (stub *promotionMonitorStub) Build(_ context.Context, request promotionmonitor.BuildRequest, mutation evaluation.Mutation) (promotionmonitor.Observation, error) {
	stub.last = request
	return promotionmonitor.Observation{Request: request, ObservedBy: mutation.Actor}, nil
}
func (stub *promotionMonitorStub) Get(id string, _ evaluation.Access) (promotionmonitor.Observation, error) {
	for _, item := range stub.items {
		if item.ObservationID == id {
			return promotionmonitor.Observation{Request: promotionmonitor.BuildRequest{ObservationID: id, PlanID: item.PlanID}, Status: item.Status}, nil
		}
	}
	return promotionmonitor.Observation{}, promotionmonitor.ErrNotFound
}
func (stub *promotionMonitorStub) List(evaluation.Access) ([]promotionmonitor.Summary, error) {
	return stub.items, nil
}

func TestPromotionObservationRoutesRequireDashboardEvaluationAndConfigPermissions(t *testing.T) {
	stub := &promotionMonitorStub{items: []promotionmonitor.Summary{{ObservationID: "observation-1", PlanID: "plan-1", Status: promotionmonitor.StatusHealthy}}}
	principal := Principal{SchemaVersion: PrincipalSchemaVersion, Actor: "promotion-operator", Roles: []evaluation.Role{evaluation.RolePromotionApprover, evaluation.RolePromotionOperator}, Permissions: []Permission{PermissionConfigRead, PermissionConfigWrite, PermissionDashboardRead, PermissionEvaluationRead, PermissionEvaluationWrite}, ProfileRevision: "promotion-monitor-v1"}
	handler, err := NewHandler(Services{PromotionMonitor: stub}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedRequest(http.MethodGet, "/v1/calibration/promotion/observations?limit=10", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "observation-1") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	show := httptest.NewRecorder()
	handler.ServeHTTP(show, authenticatedRequest(http.MethodGet, "/v1/calibration/promotion/observation?observation_id=observation-1", nil))
	if show.Code != http.StatusOK {
		t.Fatalf("show status=%d body=%s", show.Code, show.Body.String())
	}

	request := testPromotionObservationRequest()
	command := CalibrationPromotionObserveCommand{SchemaVersion: CalibrationPromotionObserveCommandSchemaVersion, Request: request, Mutation: MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "observe-1", Audit: "compare governed quality windows", At: request.ObservedAt}}
	body, _ := json.Marshal(command)
	created := httptest.NewRecorder()
	httpRequest := authenticatedRequest(http.MethodPost, "/v1/calibration/promotion/observations", bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(created, httpRequest)
	if created.Code != http.StatusCreated || stub.last.ObservationID != request.ObservationID {
		t.Fatalf("create status=%d body=%s request=%+v", created.Code, created.Body.String(), stub.last)
	}

	unknown := append(body[:len(body)-1], []byte(`,"caller_actor":"forged"}`)...)
	strict := httptest.NewRecorder()
	strictRequest := authenticatedRequest(http.MethodPost, "/v1/calibration/promotion/observations", bytes.NewReader(unknown))
	strictRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(strict, strictRequest)
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict status=%d body=%s", strict.Code, strict.Body.String())
	}

	missingDashboard := principal
	missingDashboard.Permissions = []Permission{PermissionConfigRead, PermissionConfigWrite, PermissionEvaluationRead, PermissionEvaluationWrite}
	deniedHandler, err := NewHandler(Services{PromotionMonitor: stub}, missingDashboard, testToken)
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	deniedHandler.ServeHTTP(denied, authenticatedRequest(http.MethodGet, "/v1/calibration/promotion/observations", nil))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("missing dashboard permission status=%d", denied.Code)
	}
}

func testPromotionObservationRequest() promotionmonitor.BuildRequest {
	at := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	return promotionmonitor.BuildRequest{SchemaVersion: promotionmonitor.RequestSchemaVersion, ObservationID: "observation-api-1", PlanID: "plan-1", BaselineSnapshotID: "baseline-snapshot", ObservationSnapshotID: "observation-snapshot", Policy: promotionmonitor.Policy{SchemaVersion: promotionmonitor.PolicySchemaVersion, PolicyID: "monitor-policy", Revision: "1", Rules: []promotionmonitor.MetricRule{
		{MetricID: promotionmonitor.MetricPublishedAdverseRate, Direction: analytics.LowerIsBetter, MinimumBaselineSampleSize: 1, MinimumObservationSampleSize: 1},
		{MetricID: promotionmonitor.MetricPublishedFixedRate, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 1, MinimumObservationSampleSize: 1},
		{MetricID: promotionmonitor.MetricPublishedOutcomeCoverage, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 1, MinimumObservationSampleSize: 1},
		{MetricID: promotionmonitor.MetricRunCompleteRate, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 1, MinimumObservationSampleSize: 1},
		{MetricID: promotionmonitor.MetricRunSuccessRate, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 1, MinimumObservationSampleSize: 1},
	}}, ObservedAt: at}
}
