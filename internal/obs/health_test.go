package obs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthzAlways200 proves the liveness endpoint reports the process is up.
func TestHealthzAlways200(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthzHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: want 200, got %d", rec.Code)
	}
}

// TestReadyzSkeletonReports200 proves the readiness skeleton returns 200 with a
// JSON shape later stories fill with real checks (SPEC-10 §4). With no checks
// registered the aggregate is ready.
func TestReadyzSkeletonReports200(t *testing.T) {
	rec := httptest.NewRecorder()
	ReadyzHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz: want 200, got %d", rec.Code)
	}
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body not JSON: %v\n%s", err, rec.Body.String())
	}
	if !body.Ready {
		t.Fatalf("readyz skeleton should be ready with no checks: %s", rec.Body.String())
	}
	if body.Checks == nil {
		t.Fatalf("readyz body should carry a checks object (the seam): %s", rec.Body.String())
	}
}

// TestReadyzFailingCheckReports503 proves a registered check that is not ready
// flips the aggregate to 503 — the seam later stories (DB/River/provider) use.
func TestReadyzFailingCheckReports503(t *testing.T) {
	h := ReadyzHandler(Check{
		Name: "control_plane_db",
		Probe: func(*http.Request) error {
			return errNotReady
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with failing check: want 503, got %d", rec.Code)
	}
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body not JSON: %v", err)
	}
	if body.Ready {
		t.Fatal("readyz should not be ready when a check fails")
	}
	if body.Checks["control_plane_db"] == "" {
		t.Fatalf("failing check should be named in the body: %s", rec.Body.String())
	}
}
