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

		RequireTenantSourcesRead:  passMW(ran, "tenant-sources-read"),
		RequireTenantSourcesWrite: passMW(ran, "tenant-sources-write"),

		Signup:             okHandler(ran, "signup"),
		Login:              okHandler(ran, "login"),
		Logout:             okHandler(ran, "logout"),
		OIDCStart:          okHandler(ran, "oidc-start"),
		OIDCCallback:       okHandler(ran, "oidc-callback"),
		ConnectorKinds:     okHandler(ran, "connector-kinds"),
		AuditList:          okHandler(ran, "audit-list"),
		UsageList:          okHandler(ran, "usage-list"),
		ImpersonationStart: okHandler(ran, "impersonation-start"),
		ImpersonationEnd:   okHandler(ran, "impersonation-end"),

		SourceList:   okHandler(ran, "source-list"),
		SourceCreate: okHandler(ran, "source-create"),
		SourceGet:    okHandler(ran, "source-get"),
		SourceUpdate: okHandler(ran, "source-update"),
		SourceDelete: okHandler(ran, "source-delete"),
		SourceSync:   okHandler(ran, "source-sync"),
		SourceTest:   okHandler(ran, "source-test"),

		Retrieve: okHandler(ran, "retrieve"),
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

// GET /admin/connector-kinds (SPEC-11 §10, STORY-11.2) runs RequireSession only —
// no platform-admin gate (it is platform-global but every signed-in member reads
// it) and no CSRF (a GET).
func TestConnectorKindsRouteSessionOnly(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/connector-kinds", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("connector-kinds = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !contains(ran, "connector-kinds") {
		t.Fatalf("handler not reached; ran=%v", ran)
	}
	if !contains(ran, "session") {
		t.Fatalf("session middleware did not run; ran=%v", ran)
	}
	if contains(ran, "platform-admin") || contains(ran, "csrf") {
		t.Fatalf("connector-kinds must run neither platform-admin nor csrf; ran=%v", ran)
	}
}

// A rejected session 401s and never reaches the handler.
func TestConnectorKindsRouteSessionRejected(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.RequireSession = stubMW(&ran, "session", http.StatusUnauthorized, CodeUnauthorized)
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/connector-kinds", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("connector-kinds = %d, want 401", rr.Code)
	}
	if contains(ran, "connector-kinds") {
		t.Fatalf("handler reached despite 401; ran=%v", ran)
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
	for _, p := range []string{"/v1/documents", "/v1/jobs", "/admin/tenants"} {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, p, nil)
		r.Header.Set("Authorization", "Bearer rk_x_y")
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (seam)", p, rr.Code)
		}
	}
}

// The sources routes are mounted behind scope-admin -> rate-limit and dispatch by
// method to their handlers (STORY-04.3). Each must run the tenant-scoped chain
// then reach its own handler, and {id} subpaths must route to the right method.
func TestSourcesRoutesChain(t *testing.T) {
	cases := []struct {
		method, path, handler string
	}{
		{http.MethodGet, "/v1/sources", "source-list"},
		{http.MethodPost, "/v1/sources", "source-create"},
		{http.MethodGet, "/v1/sources/abc", "source-get"},
		{http.MethodPatch, "/v1/sources/abc", "source-update"},
		{http.MethodDelete, "/v1/sources/abc", "source-delete"},
		{http.MethodPost, "/v1/sources/abc/sync", "source-sync"},
		{http.MethodPost, "/v1/sources/abc/test", "source-test"},
	}
	for _, c := range cases {
		var ran []string
		h := New(newTestDeps(&ran))
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set("Authorization", "Bearer rk_x_y")
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body=%s", c.method, c.path, rr.Code, rr.Body.String())
		}
		// The credential-keyed rate limiter runs inside the scope gate (ADR-0027).
		if idxOf(ran, "scope-admin") < 0 || idxOf(ran, "rate-limit") < 0 {
			t.Fatalf("%s %s did not run scope-admin -> rate-limit; ran=%v", c.method, c.path, ran)
		}
		if idxOf(ran, "scope-admin") > idxOf(ran, "rate-limit") {
			t.Fatalf("%s %s ran rate-limit before scope; ran=%v", c.method, c.path, ran)
		}
		if !contains(ran, c.handler) {
			t.Fatalf("%s %s did not reach %s; ran=%v", c.method, c.path, c.handler, ran)
		}
	}
}

// The session admin sources routes (STORY-11.2, ADR-0075) reuse the SAME
// handlers as the Bearer /v1/sources surface, mounted behind session ->
// tenant-access instead of scope -> rate-limit, with {tenantId} distinct from
// the sources handlers' own {id} (the source id). Reads use the read gate,
// mutations the write gate, in RequireSession -> RequireTenant... order.
func TestTenantSourcesRoutesChain(t *testing.T) {
	cases := []struct {
		method, path, handler, gate string
	}{
		{http.MethodGet, "/admin/tenants/t-1/sources", "source-list", "tenant-sources-read"},
		{http.MethodPost, "/admin/tenants/t-1/sources", "source-create", "tenant-sources-write"},
		{http.MethodGet, "/admin/tenants/t-1/sources/abc", "source-get", "tenant-sources-read"},
		{http.MethodPatch, "/admin/tenants/t-1/sources/abc", "source-update", "tenant-sources-write"},
		{http.MethodDelete, "/admin/tenants/t-1/sources/abc", "source-delete", "tenant-sources-write"},
		{http.MethodPost, "/admin/tenants/t-1/sources/abc/sync", "source-sync", "tenant-sources-write"},
		{http.MethodPost, "/admin/tenants/t-1/sources/abc/test", "source-test", "tenant-sources-write"},
	}
	for _, c := range cases {
		var ran []string
		h := New(newTestDeps(&ran))
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, nil)
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body=%s", c.method, c.path, rr.Code, rr.Body.String())
		}
		if idxOf(ran, "session") < 0 || idxOf(ran, c.gate) < 0 {
			t.Fatalf("%s %s did not run session -> %s; ran=%v", c.method, c.path, c.gate, ran)
		}
		if idxOf(ran, "session") > idxOf(ran, c.gate) {
			t.Fatalf("%s %s ran %s before session; ran=%v", c.method, c.path, c.gate, ran)
		}
		if !contains(ran, c.handler) {
			t.Fatalf("%s %s did not reach %s; ran=%v", c.method, c.path, c.handler, ran)
		}
	}
}

// A mutation on the session admin sources surface carries CSRF like every
// other session-cookie mutation (SPEC-09 §3); the corresponding GET does not.
func TestTenantSourcesCSRF(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.CSRF = stubMW(&ran, "csrf", http.StatusForbidden, CodeForbidden)
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/tenants/t-1/sources", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST sources without CSRF = %d, want 403", rr.Code)
	}
	if contains(ran, "source-create") {
		t.Fatalf("handler reached despite CSRF block; ran=%v", ran)
	}

	ran = nil
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/tenants/t-1/sources", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET sources = %d, want 200 (no CSRF on reads)", rr.Code)
	}
	if contains(ran, "csrf") {
		t.Fatalf("CSRF ran on a GET route; ran=%v", ran)
	}
}

// The session admin jobs routes (STORY-11.3, ADR-0075, FR-ADM-02) reuse the SAME
// jobs.Handlers as the Bearer /v1/jobs surface, mounted behind session ->
// tenant-access. Reads use the read gate (PermQuery, any role); cancel uses the
// write gate (PermManageSources) and carries CSRF. {tenantId} is the tenant path
// segment; {id} stays the jobs handlers' own job id.
func TestTenantJobsRoutesChain(t *testing.T) {
	cases := []struct {
		method, path, handler, gate string
	}{
		{http.MethodGet, "/admin/tenants/t-1/jobs", "job-list", "tenant-sources-read"},
		{http.MethodGet, "/admin/tenants/t-1/jobs/abc", "job-get", "tenant-sources-read"},
		{http.MethodPost, "/admin/tenants/t-1/jobs/abc/cancel", "job-cancel", "tenant-sources-write"},
	}
	for _, c := range cases {
		var ran []string
		deps := newTestDeps(&ran)
		// Jobs are a seam-only group in the default test deps; wire the three
		// handlers locally so the tenant-scoped mounts can be asserted reached.
		deps.JobList = okHandler(&ran, "job-list")
		deps.JobGet = okHandler(&ran, "job-get")
		deps.JobCancel = okHandler(&ran, "job-cancel")
		h := New(deps)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body=%s", c.method, c.path, rr.Code, rr.Body.String())
		}
		if idxOf(ran, "session") < 0 || idxOf(ran, c.gate) < 0 {
			t.Fatalf("%s %s did not run session -> %s; ran=%v", c.method, c.path, c.gate, ran)
		}
		if idxOf(ran, "session") > idxOf(ran, c.gate) {
			t.Fatalf("%s %s ran %s before session; ran=%v", c.method, c.path, c.gate, ran)
		}
		if !contains(ran, c.handler) {
			t.Fatalf("%s %s did not reach %s; ran=%v", c.method, c.path, c.handler, ran)
		}
	}
}

// Cancel on the session admin jobs surface carries CSRF like every other
// session-cookie mutation (SPEC-09 §3); the list GET does not.
func TestTenantJobsCSRF(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.JobList = okHandler(&ran, "job-list")
	deps.JobCancel = okHandler(&ran, "job-cancel")
	deps.CSRF = stubMW(&ran, "csrf", http.StatusForbidden, CodeForbidden)
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/tenants/t-1/jobs/abc/cancel", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST cancel without CSRF = %d, want 403", rr.Code)
	}
	if contains(ran, "job-cancel") {
		t.Fatalf("handler reached despite CSRF block; ran=%v", ran)
	}

	ran = nil
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/tenants/t-1/jobs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET jobs = %d, want 200 (no CSRF on reads)", rr.Code)
	}
	if contains(ran, "csrf") {
		t.Fatalf("CSRF ran on a GET route; ran=%v", ran)
	}
}

// The session admin documents routes (STORY-11.4, ADR-0075, FR-ADM-03) reuse the
// SAME documents.Handlers as the Bearer /v1/documents surface, mounted behind
// session -> tenant-access. Every route is a read, so all three use the read gate
// (PermQuery, any role) and none carry CSRF. {tenantId} is the tenant path
// segment; {id} stays the documents handlers' own document id.
func TestTenantDocumentsRoutesChain(t *testing.T) {
	cases := []struct {
		method, path, handler, gate string
	}{
		{http.MethodGet, "/admin/tenants/t-1/documents", "doc-list", "tenant-sources-read"},
		{http.MethodGet, "/admin/tenants/t-1/documents/abc", "doc-get", "tenant-sources-read"},
		{http.MethodGet, "/admin/tenants/t-1/documents/abc/chunks", "doc-chunks", "tenant-sources-read"},
	}
	for _, c := range cases {
		var ran []string
		deps := newTestDeps(&ran)
		// Documents are a seam-only group in the default test deps; wire the read
		// handlers locally so the tenant-scoped mounts can be asserted reached.
		deps.DocumentList = okHandler(&ran, "doc-list")
		deps.DocumentGet = okHandler(&ran, "doc-get")
		deps.DocumentChunks = okHandler(&ran, "doc-chunks")
		h := New(deps)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body=%s", c.method, c.path, rr.Code, rr.Body.String())
		}
		if idxOf(ran, "session") < 0 || idxOf(ran, c.gate) < 0 {
			t.Fatalf("%s %s did not run session -> %s; ran=%v", c.method, c.path, c.gate, ran)
		}
		if idxOf(ran, "session") > idxOf(ran, c.gate) {
			t.Fatalf("%s %s ran %s before session; ran=%v", c.method, c.path, c.gate, ran)
		}
		if !contains(ran, c.handler) {
			t.Fatalf("%s %s did not reach %s; ran=%v", c.method, c.path, c.handler, ran)
		}
	}
}

// No CSRF is mounted on the session admin documents surface: it is read-only, and
// CSRF guards session-cookie mutations only (SPEC-09 §3).
func TestTenantDocumentsNoCSRF(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.DocumentList = okHandler(&ran, "doc-list")
	deps.CSRF = stubMW(&ran, "csrf", http.StatusForbidden, CodeForbidden)
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/tenants/t-1/documents", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET documents = %d, want 200 (no CSRF on reads)", rr.Code)
	}
	if contains(ran, "csrf") {
		t.Fatalf("CSRF ran on a GET route; ran=%v", ran)
	}
}

// The session admin query playground routes (STORY-11.6, ADR-0075, FR-RET-06/09)
// reuse the SAME query/feedback handlers as the Bearer /v1/query and /v1/feedback
// surfaces, mounted behind session -> tenant-access. Both map to PermQuery (any
// role), so both use the read gate; both are POSTs and carry CSRF. {tenantId} is
// the tenant path segment.
func TestTenantQueryRoutesChain(t *testing.T) {
	cases := []struct {
		method, path, handler, gate string
	}{
		{http.MethodPost, "/admin/tenants/t-1/query", "query", "tenant-sources-read"},
		{http.MethodPost, "/admin/tenants/t-1/feedback", "feedback", "tenant-sources-read"},
	}
	for _, c := range cases {
		var ran []string
		deps := newTestDeps(&ran)
		// Query/feedback are a seam-only group in the default test deps; wire the
		// handlers locally so the tenant-scoped mounts can be asserted reached.
		deps.Query = okHandler(&ran, "query")
		deps.Feedback = okHandler(&ran, "feedback")
		h := New(deps)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body=%s", c.method, c.path, rr.Code, rr.Body.String())
		}
		if idxOf(ran, "session") < 0 || idxOf(ran, c.gate) < 0 {
			t.Fatalf("%s %s did not run session -> %s; ran=%v", c.method, c.path, c.gate, ran)
		}
		if idxOf(ran, "session") > idxOf(ran, c.gate) {
			t.Fatalf("%s %s ran %s before session; ran=%v", c.method, c.path, c.gate, ran)
		}
		if !contains(ran, c.handler) {
			t.Fatalf("%s %s did not reach %s; ran=%v", c.method, c.path, c.handler, ran)
		}
	}
}

// Both session query routes are POSTs and carry CSRF like every other session-cookie
// mutation (SPEC-09 §3): a blocked CSRF check stops the handler being reached.
func TestTenantQueryCSRF(t *testing.T) {
	for _, path := range []string{"/admin/tenants/t-1/query", "/admin/tenants/t-1/feedback"} {
		var ran []string
		deps := newTestDeps(&ran)
		deps.Query = okHandler(&ran, "query")
		deps.Feedback = okHandler(&ran, "feedback")
		deps.CSRF = stubMW(&ran, "csrf", http.StatusForbidden, CodeForbidden)
		h := New(deps)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("POST %s without CSRF = %d, want 403", path, rr.Code)
		}
		if contains(ran, "query") || contains(ran, "feedback") {
			t.Fatalf("handler reached despite CSRF block on %s; ran=%v", path, ran)
		}
	}
}

// The session admin settings/members/api-keys routes (STORY-11.5, ADR-0075,
// ISSUE-0064) mount the reused SettingsHandlers + the members/api-key handlers
// behind session -> tenant-access. Reads use the read gate (PermQuery, any
// role); settings PATCH uses the change-settings gate; every members/keys write
// (and the keys list, sensitive) uses the manage-members gate. {tenantId} is the
// tenant path segment; {userId}/{keyId} are the handlers' own resource ids.
func TestTenantMembersKeysSettingsRoutesChain(t *testing.T) {
	cases := []struct {
		method, path, handler, gate string
	}{
		{http.MethodGet, "/admin/tenants/t-1/settings", "settings-get", "tenant-sources-read"},
		{http.MethodPatch, "/admin/tenants/t-1/settings", "settings-patch", "tenant-change-settings"},
		{http.MethodGet, "/admin/tenants/t-1/members", "member-list", "tenant-sources-read"},
		{http.MethodPost, "/admin/tenants/t-1/members", "member-add", "tenant-manage-members"},
		{http.MethodPatch, "/admin/tenants/t-1/members/u-9", "member-setrole", "tenant-manage-members"},
		{http.MethodDelete, "/admin/tenants/t-1/members/u-9", "member-remove", "tenant-manage-members"},
		{http.MethodGet, "/admin/tenants/t-1/api-keys", "key-list", "tenant-manage-members"},
		{http.MethodPost, "/admin/tenants/t-1/api-keys", "key-create", "tenant-manage-members"},
		{http.MethodDelete, "/admin/tenants/t-1/api-keys/k-9", "key-revoke", "tenant-manage-members"},
	}
	for _, c := range cases {
		var ran []string
		deps := newTestDeps(&ran)
		deps.RequireTenantChangeSettings = passMW(&ran, "tenant-change-settings")
		deps.RequireTenantManageMembers = passMW(&ran, "tenant-manage-members")
		deps.SettingsGet = okHandler(&ran, "settings-get")
		deps.SettingsPatch = okHandler(&ran, "settings-patch")
		deps.MemberList = okHandler(&ran, "member-list")
		deps.MemberAdd = okHandler(&ran, "member-add")
		deps.MemberSetRole = okHandler(&ran, "member-setrole")
		deps.MemberRemove = okHandler(&ran, "member-remove")
		deps.KeyList = okHandler(&ran, "key-list")
		deps.KeyCreate = okHandler(&ran, "key-create")
		deps.KeyRevoke = okHandler(&ran, "key-revoke")
		h := New(deps)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body=%s", c.method, c.path, rr.Code, rr.Body.String())
		}
		if idxOf(ran, "session") < 0 || idxOf(ran, c.gate) < 0 {
			t.Fatalf("%s %s did not run session -> %s; ran=%v", c.method, c.path, c.gate, ran)
		}
		if idxOf(ran, "session") > idxOf(ran, c.gate) {
			t.Fatalf("%s %s ran %s before session; ran=%v", c.method, c.path, c.gate, ran)
		}
		if !contains(ran, c.handler) {
			t.Fatalf("%s %s did not reach %s; ran=%v", c.method, c.path, c.handler, ran)
		}
	}
}

// Mutations on the session admin members/keys/settings surface carry CSRF like
// every other session-cookie mutation (SPEC-09 §3); the corresponding GET does
// not.
func TestTenantMembersKeysSettingsCSRF(t *testing.T) {
	var ran []string
	deps := newTestDeps(&ran)
	deps.RequireTenantManageMembers = passMW(&ran, "tenant-manage-members")
	deps.MemberAdd = okHandler(&ran, "member-add")
	deps.MemberList = okHandler(&ran, "member-list")
	deps.CSRF = stubMW(&ran, "csrf", http.StatusForbidden, CodeForbidden)
	h := New(deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/tenants/t-1/members", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST members without CSRF = %d, want 403", rr.Code)
	}
	if contains(ran, "member-add") {
		t.Fatalf("handler reached despite CSRF block; ran=%v", ran)
	}

	ran = nil
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/tenants/t-1/members", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET members = %d, want 200 (no CSRF on reads)", rr.Code)
	}
	if contains(ran, "csrf") {
		t.Fatalf("CSRF ran on a GET route; ran=%v", ran)
	}
}

// The retrieve route is mounted behind the `query` scope -> rate-limit chain and
// reaches its handler (STORY-08.2, FR-RET-08). The tenant is derived from the API
// key by the scope gate (FR-ACC-03) — never a body/param.
func TestRetrieveRouteChain(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/retrieve", nil)
	r.Header.Set("Authorization", "Bearer rk_x_y")
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /v1/retrieve = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if idxOf(ran, "scope-query") < 0 || idxOf(ran, "rate-limit") < 0 {
		t.Fatalf("POST /v1/retrieve did not run scope-query -> rate-limit; ran=%v", ran)
	}
	if idxOf(ran, "scope-query") > idxOf(ran, "rate-limit") {
		t.Fatalf("POST /v1/retrieve ran rate-limit before scope; ran=%v", ran)
	}
	if !contains(ran, "retrieve") {
		t.Fatalf("POST /v1/retrieve did not reach the handler; ran=%v", ran)
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
