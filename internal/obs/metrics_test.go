package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMetricsHandlerExposesHistogram proves the metrics registry exposes the
// representative api_request_duration_seconds histogram in Prometheus text
// after an observation (SPEC-10 §2).
func TestMetricsHandlerExposesHistogram(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest("/healthz", 200, "-", 0.01)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "api_request_duration_seconds") {
		t.Fatalf("metrics output missing api_request_duration_seconds:\n%s", body)
	}
	// The exposed sample must carry the route/status/tenant labels.
	if !strings.Contains(body, `route="/healthz"`) {
		t.Fatalf("metrics output missing route label:\n%s", body)
	}
	if !strings.Contains(body, `tenant="-"`) {
		t.Fatalf("metrics output missing tenant label:\n%s", body)
	}
}
