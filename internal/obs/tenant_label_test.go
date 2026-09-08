package obs

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMiddlewareLabelsResolvedTenant proves an inner handler that calls
// SetRequestTenant makes the resolved tenant appear on the request histogram
// label and the structured log line — the fix for the always-"-" per-tenant
// label (SPEC-10 §2, FR-OBS-02 per-tenant metrics). The obs middleware runs
// OUTERMOST, so it must read the tenant a later layer sets, not its own snapshot.
func TestMiddlewareLabelsResolvedTenant(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	m := NewMetrics()

	h := Middleware(log, m)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRequestTenant(r.Context(), "acme")
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/query", nil)
	h.ServeHTTP(rec, req)

	body := scrape(t, m)
	if !strings.Contains(body, `tenant="acme"`) {
		t.Fatalf("histogram missing resolved tenant label:\n%s", body)
	}

	line := decodeLine(t, buf.Bytes())
	if line["tenant_id"] != "acme" {
		t.Fatalf("log tenant_id = %v, want acme", line["tenant_id"])
	}
}

// TestMiddlewareLabelsUnresolvedTenant proves a request that never resolves a
// tenant keeps the "-" placeholder (no client-supplied tenant leaks in).
func TestMiddlewareLabelsUnresolvedTenant(t *testing.T) {
	log := Logger("ragctl", slog.LevelInfo, &bytes.Buffer{})
	m := NewMetrics()

	h := Middleware(log, m)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if !strings.Contains(scrape(t, m), `tenant="-"`) {
		t.Fatal("unresolved request should carry tenant=- placeholder")
	}
}
