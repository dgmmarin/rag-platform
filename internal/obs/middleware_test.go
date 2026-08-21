package obs

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMiddlewareGeneratesRequestID proves the middleware mints a request_id when
// none is supplied, stashes it in context (visible to the handler), echoes it on
// the response, and logs one structured line carrying it with a duration.
func TestMiddlewareGeneratesRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	m := NewMetrics()

	var seen string
	h := Middleware(log, m)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("handler saw no request_id in context")
	}
	if got := rec.Header().Get("X-Request-Id"); got != seen {
		t.Fatalf("response X-Request-Id %q != context request_id %q", got, seen)
	}

	m2 := decodeLine(t, buf.Bytes())
	if m2["request_id"] != seen {
		t.Fatalf("log request_id %v != generated %q", m2["request_id"], seen)
	}
	if _, ok := m2["duration_ms"]; !ok {
		t.Fatalf("log line missing duration_ms: %s", buf.String())
	}
	if _, ok := m2["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms should be numeric, got %T", m2["duration_ms"])
	}
}

// TestMiddlewarePropagatesInboundRequestID proves an inbound X-Request-Id is
// respected rather than replaced (trace continuity across hops).
func TestMiddlewarePropagatesInboundRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	m := NewMetrics()

	h := Middleware(log, m)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "inbound-42")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "inbound-42" {
		t.Fatalf("inbound request id not propagated: got %q", got)
	}
	if m2 := decodeLine(t, buf.Bytes()); m2["request_id"] != "inbound-42" {
		t.Fatalf("log did not use inbound request_id: %v", m2["request_id"])
	}
}

// TestMiddlewareObservesMetric proves the request histogram is fed with the
// captured status label per request.
func TestMiddlewareObservesMetric(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	m := NewMetrics()

	h := Middleware(log, m)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/things", nil))

	mrec := httptest.NewRecorder()
	m.Handler().ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := mrec.Body.String()
	if !strings.Contains(body, `status="201"`) {
		t.Fatalf("metric not observed with status 201:\n%s", body)
	}
	if !strings.Contains(body, `route="/things"`) {
		t.Fatalf("metric not observed with route /things:\n%s", body)
	}
}

// TestMiddlewareDefaultsStatusTo200 proves a handler that writes a body without
// calling WriteHeader is recorded as 200 (net/http's implicit status).
func TestMiddlewareDefaultsStatusTo200(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	m := NewMetrics()

	h := Middleware(log, m)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	m2 := decodeLine(t, buf.Bytes())
	if m2["status"] != float64(200) {
		t.Fatalf("implicit status: want 200, got %v", m2["status"])
	}
}
