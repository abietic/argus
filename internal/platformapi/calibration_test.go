package platformapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"argus.local/argus/internal/calibration"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
)

type unavailableCalibrationCases struct{}

func (unavailableCalibrationCases) GetCase(string, evaluation.Access) (evaluation.CaseRecord, error) {
	return evaluation.CaseRecord{}, evaluation.ErrNotFound
}
func (unavailableCalibrationCases) AnnotationHistory(string, evaluation.Access) ([]evaluation.CaseAnnotationEntry, error) {
	return nil, evaluation.ErrNotFound
}
func (unavailableCalibrationCases) AdjudicationHistory(string, evaluation.Access) ([]evaluation.CaseAdjudicationEntry, error) {
	return nil, evaluation.ErrNotFound
}

type unavailableCalibrationRuns struct{}

func (unavailableCalibrationRuns) LoadRun(string) (runmodel.ReviewRun, error) {
	return runmodel.ReviewRun{}, calibration.ErrNotFound
}
func (unavailableCalibrationRuns) LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error) {
	return runmodel.ExecutionSnapshot{}, calibration.ErrNotFound
}
func (unavailableCalibrationRuns) LoadCommittedRunResult(string) (runrepo.CommittedRunResult, error) {
	return runrepo.CommittedRunResult{}, calibration.ErrNotFound
}
func (unavailableCalibrationRuns) ReadArtifact(runmodel.ArtifactRef) ([]byte, error) {
	return nil, calibration.ErrNotFound
}
func (unavailableCalibrationRuns) CheckArtifactEligibility(runmodel.ArtifactRef, runrepo.ArtifactUse) error {
	return nil
}

func TestCalibrationRoutesAreAuthenticatedAuthorizedAndStrict(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := calibration.New(store, unavailableCalibrationCases{}, unavailableCalibrationRuns{})
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{SchemaVersion: PrincipalSchemaVersion, Actor: "calibration-operator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator}, Permissions: []Permission{PermissionEvaluationRead, PermissionEvaluationWrite}, ProfileRevision: "calibration-v1"}
	handler, err := NewHandler(Services{Calibration: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/calibration/runs", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticated.Code)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedRequest(http.MethodGet, "/v1/calibration/runs", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}

	unknown := authenticatedRequest(http.MethodPost, "/v1/calibration/runs", strings.NewReader(`{"schema_version":"argus.local_api_calibration_fit_command.v1alpha1","unexpected":true}`))
	unknown.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, unknown)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field status=%d body=%s", response.Code, response.Body.String())
	}

	readOnly := principal
	readOnly.Permissions = []Permission{PermissionEvaluationRead}
	readHandler, err := NewHandler(Services{Calibration: repository}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	denied := authenticatedRequest(http.MethodPost, "/v1/calibration/runs", strings.NewReader(`{}`))
	denied.Header.Set("Content-Type", "application/json")
	deniedResponse := httptest.NewRecorder()
	readHandler.ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("read-only post status=%d", deniedResponse.Code)
	}
}
