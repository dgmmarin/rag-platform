package obs

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewServeMuxWiresEndpoints proves the assembled mux serves the three
// scaffolding endpoints through the obs middleware: /healthz and /readyz return
// 200, /metrics exposes the request histogram, and every served request gets a
// request id echoed on the response (the middleware seam).
func TestNewServeMuxWiresEndpoints(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	m := NewMetrics()
	h := NewServeMux(log, m)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", path, rec.Code)
		}
		if rec.Header().Get("X-Request-Id") == "" {
			t.Fatalf("%s: middleware did not set X-Request-Id", path)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "api_request_duration_seconds") {
		t.Fatalf("/metrics missing histogram:\n%s", rec.Body.String())
	}
}
