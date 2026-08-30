package platformapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
)

func TestWorkloadPressureAPIRequiresReviewReadAndExactUTCObservation(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := scheduling.NewRepository(store, scheduling.DefaultLocalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "workload-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionReviewRead}, ProfileRevision: "workload-reader-v1",
	}
	handler, err := NewHandler(Services{Workloads: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 26, 2, 0, 0, 123, time.UTC)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodGet, "/v1/workloads/pressure?at="+url.QueryEscape(at.Format(time.RFC3339Nano)), nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("pressure status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded struct {
		SchemaVersion string                      `json:"schema_version"`
		Data          scheduling.PressureSnapshot `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != ResponseSchemaVersion || !decoded.Data.ObservedAt.Equal(at) ||
		decoded.Data.PolicySHA256 == "" {
		t.Fatalf("pressure response = %+v", decoded)
	}

	for _, path := range []string{
		"/v1/workloads/pressure",
		"/v1/workloads/pressure?at=2026-08-26T10%3A00%3A00%2B08%3A00",
		"/v1/workloads/pressure?at=2026-08-26T02%3A00%3A00Z&at=2026-08-26T02%3A00%3A01Z",
		"/v1/workloads/pressure?at=2026-08-26T02%3A00%3A00Z&tenant=local",
	} {
		invalid := httptest.NewRecorder()
		handler.ServeHTTP(invalid, authenticatedRequest(http.MethodGet, path, nil))
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d body=%s", path, invalid.Code, invalid.Body.String())
		}
	}

	forbidden, err := NewHandler(Services{Workloads: repository}, Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "config-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionConfigRead}, ProfileRevision: "config-reader-v1",
	}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	forbidden.ServeHTTP(denied, authenticatedRequest(
		http.MethodGet, "/v1/workloads/pressure?at=2026-08-26T02%3A00%3A00Z", nil,
	))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("pressure without review_read status=%d", denied.Code)
	}

	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, authenticatedRequest(
		http.MethodPost, "/v1/workloads/pressure?at=2026-08-26T02%3A00%3A00Z", bytes.NewReader(nil),
	))
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("pressure POST status=%d allow=%q", wrongMethod.Code, wrongMethod.Header().Get("Allow"))
	}
}

func TestWorkloadPressureAPIFailsClosedOnCorruptState(t *testing.T) {
	handler, err := NewHandler(Services{Workloads: pressureReaderStub{err: scheduling.ErrCorrupt}}, Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "workload-reader", Roles: []evaluation.Role{},
		Permissions: []Permission{PermissionReviewRead}, ProfileRevision: "workload-reader-v1",
	}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodGet, "/v1/workloads/pressure?at=2026-08-26T02%3A00%3A00Z", nil,
	))
	if response.Code != http.StatusInternalServerError ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"code":"workload_state_corrupt"`)) {
		t.Fatalf("corrupt pressure status=%d body=%s", response.Code, response.Body.String())
	}
}

type pressureReaderStub struct {
	err error
}

func (reader pressureReaderStub) Pressure(time.Time) (scheduling.PressureSnapshot, error) {
	if reader.err != nil {
		return scheduling.PressureSnapshot{}, reader.err
	}
	return scheduling.PressureSnapshot{}, errors.New("unexpected pressure read")
}
