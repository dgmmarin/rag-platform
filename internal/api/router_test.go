package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rag-platform/ragctl/internal/obs"
)

// stubMW returns a middleware that either passes through or short-circuits with
// the given status + code envelope, recording that it ran into *ran.
func stubMW(ran *[]string, name string, block int, code string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*ran = append(*ran, name)
			if block != 0 {
				WriteError(w, r, block, code, name+" blocked")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func passMW(ran *[]string, name string) func(http.Handler) http.Handler {
	return stubMW(ran, name, 0, "")
}

// okHandler is a terminal 200 handler recording that it was reached.
func okHandler(ran *[]string, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*ran = append(*ran, name)
		w.WriteHeader(http.StatusOK)
	})
}

// newTestDeps builds a Deps whose middleware are pass-through stubs recording
// into *ran, so the assembled router's chain ORDER and mount points can be
// asserted without a database.
func newTestDeps(ran *[]string) Deps {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return Deps{
		Log:     log,
		Metrics: obs.NewMetrics(),

		RequireSession:       passMW(ran, "session"),
		CSRF:                 passMW(ran, "csrf"),
		RequirePlatformAdmin: passMW(ran, "platform-admin"),
		RequireScopeQuery:    passMW(ran, "scope-query"),
		RequireScopeIngest:   passMW(ran, "scope-ingest"),
		RequireScopeAdmin:    passMW(ran, "scope-admin"),
		RequireRoleAdmin:     passMW(ran, "role-admin"),
		RateLimit:            passMW(ran, "rate-limit"),

		Signup:             okHandler(ran, "signup"),
		Login:              okHandler(ran, "login"),
		Logout:             okHandler(ran, "logout"),
		OIDCStart:          okHandler(ran, "oidc-start"),
		OIDCCallback:       okHandler(ran, "oidc-callback"),
		AuditList:          okHandler(ran, "audit-list"),
		UsageList:          okHandler(ran, "usage-list"),
		ImpersonationStart: okHandler(ran, "impersonation-start"),
		ImpersonationEnd:   okHandler(ran, "impersonation-end"),
	}
}

func TestHealthzOpenNoAuth(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	for _, r := range ran {
		if r == "session" || r == "platform-admin" || r == "rate-limit" {
			t.Fatalf("healthz ran auth middleware %q; must be open", r)
		}
	}
}

func TestReadyzOpenNoAuth(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", rr.Code)
	}
}

// A login POST reaches the login handler through the chain (open route).
func TestLoginRouteReachesHandler(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !contains(ran, "login") {
		t.Fatalf("login handler not reached; ran=%v", ran)
	}
}

// The platform-admin audit route runs the platform-admin gate before the handler.
func TestAuditRouteGuardedByPlatformAdmin(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/audit?tenant=t1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("audit = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if idxOf(ran, "platform-admin") < 0 {
		t.Fatalf("platform-admin gate did not run; ran=%v", ran)
	}
	if idxOf(ran, "platform-admin") > idxOf(ran, "audit-list") {
		t.Fatalf("platform-admin ran after handler; ran=%v", ran)
	}
	if idxOf(ran, "session") > idxOf(ran, "platform-admin") {
		t.Fatalf("session must precede platform-admin; ran=%v", ran)
	}
}

// A blocked platform-admin gate must 403 and NOT reach the handler.
func TestAuditRoutePlatformAdminRejected(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.RequirePlatformAdmin = stubMW(&ran, "platform-admin", http.StatusForbidden, CodeForbidden)
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/audit?tenant=t1", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("audit = %d, want 403", rr.Code)
	}
	if contains(ran, "audit-list") {
		t.Fatalf("handler reached despite 403; ran=%v", ran)
	}
	assertEnvelope(t, rr, CodeForbidden)
}

// The usage route (tenant surface) is guarded by API-key scope + rate limiting,
// in that order (auth precedes rate limit, ADR-0027).
func TestUsageRouteChainOrder(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	r.Header.Set("Authorization", "Bearer rk_x_y")
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("usage = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if idxOf(ran, "scope-admin") < 0 || idxOf(ran, "rate-limit") < 0 {
		t.Fatalf("expected scope + rate-limit to run; ran=%v", ran)
	}
	if idxOf(ran, "scope-admin") > idxOf(ran, "rate-limit") {
		t.Fatalf("auth must precede rate limit; ran=%v", ran)
	}
	if idxOf(ran, "rate-limit") > idxOf(ran, "usage-list") {
		t.Fatalf("rate limit must precede handler; ran=%v", ran)
	}
}

// An over-limit request 429s and never reaches the handler.
func TestUsageRouteOverLimit(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.RateLimit = stubMW(&ran, "rate-limit", http.StatusTooManyRequests, CodeRateLimited)
	h := New(deps)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	r.Header.Set("Authorization", "Bearer rk_x_y")
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("usage = %d, want 429", rr.Code)
	}
	if contains(ran, "usage-list") {
		t.Fatalf("handler reached despite 429; ran=%v", ran)
	}
}

// An unauthenticated tenant route (scope gate blocks) is 401 without reaching
// the handler or the rate limiter.
func TestUsageRouteUnauthenticated(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.RequireScopeAdmin = stubMW(&ran, "scope-admin", http.StatusUnauthorized, CodeUnauthorized)
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("usage = %d, want 401", rr.Code)
	}
	if contains(ran, "rate-limit") || contains(ran, "usage-list") {
		t.Fatalf("blocked request continued past auth; ran=%v", ran)
	}
	assertEnvelope(t, rr, CodeUnauthorized)
}

// An unknown route returns the JSON not_found envelope, not Go's plain 404.
func TestUnknownRouteEnvelope(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d, want 404", rr.Code)
	}
	assertEnvelope(t, rr, CodeNotFound)
}

// A panic anywhere in the chain becomes a 500 envelope (recovery is outer).
func TestPanicBecomesEnvelope(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.Signup = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/signup", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("signup = %d, want 500", rr.Code)
	}
	assertEnvelope(t, rr, CodeInternal)
}

// Seam-only route groups (sources/documents/jobs) return a spec 404 envelope
// until their handlers land (later EPIC-04 stories), NOT a stub 200.
func TestSeamRoutesReturnNotFound(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	for _, p := range []string{"/v1/sources", "/v1/documents", "/v1/jobs", "/admin/tenants"} {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, p, nil)
		r.Header.Set("Authorization", "Bearer rk_x_y")
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (seam)", p, rr.Code)
		}
	}
}

func contains(s []string, v string) bool { return idxOf(s, v) >= 0 }

func idxOf(s []string, v string) int {
	for i := range s {
		if s[i] == v {
			return i
		}
	}
	return -1
}

func assertEnvelope(t *testing.T, rr *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rr.Body.String())
	}
	if body.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q", body.Error.Code, wantCode)
	}
}
