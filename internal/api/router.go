package api

import (
	"log/slog"
	"net/http"

	"github.com/rag-platform/ragctl/internal/obs"
)

// Deps is everything the router mounts. Middleware are injected as values so the
// assembly is unit-testable with stubs and the real dependencies are built once
// in `ragctl serve` (ADR-0027). A nil middleware is treated as pass-through and a
// nil handler as a not-implemented seam, so a partially-wired server still boots.
type Deps struct {
	// Observability.
	Log     *slog.Logger
	Metrics *obs.Metrics
	// Checks are readiness probes surfaced at /readyz (SPEC-10 §4).
	Checks []obs.Check

	// Authentication / authorization middleware (built from EPIC-03 packages).
	RequireSession       Middleware // resolves the session cookie or 401
	CSRF                 Middleware // double-submit CSRF on mutating session routes
	RequirePlatformAdmin Middleware // tenant-less platform-admin gate (FR-ACC-07)
	RequireScopeQuery    Middleware // API-key `query` scope + tenant resolution
	RequireScopeIngest   Middleware // API-key `ingest` scope + tenant resolution
	RequireScopeAdmin    Middleware // API-key `admin` scope + tenant resolution
	RequireRoleAdmin     Middleware // session role check (SPEC-02 §4)
	RateLimit            Middleware // per-key/per-tenant token bucket (NFR-SEC-07)

	// Handlers that already exist (auth + the 03.x admin handlers).
	Signup             http.Handler
	Login              http.Handler
	Logout             http.Handler
	OIDCStart          http.Handler
	OIDCCallback       http.Handler
	AuditList          http.Handler
	UsageList          http.Handler
	ImpersonationStart http.Handler
	ImpersonationEnd   http.Handler
}

// New assembles the public HTTP handler: the global middleware chain in the
// SPEC-07 §1 order, the health/readiness/metrics endpoints, the open auth
// routes, the platform-admin surface behind RequireSession + RequirePlatformAdmin,
// and the per-tenant authenticated surface behind API-key scope + rate limiting.
// Unregistered paths (including the seam route groups for later EPIC-04 stories)
// fall through to the JSON not_found envelope.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()

	// --- Operational endpoints: open, no auth (SPEC-07 §2, SPEC-10 §4). ---
	mux.Handle("GET /healthz", obs.HealthzHandler())
	mux.Handle("GET /readyz", obs.ReadyzHandler(d.Checks...))
	if d.Metrics != nil {
		mux.Handle("GET /metrics", d.Metrics.Handler())
	}

	// The OpenAPI description is public (open, no auth): it drives client and SDK
	// generation and must be reachable without a credential (SPEC-07 §3).
	mux.Handle("GET /v1/openapi.json", OpenAPIHandler())

	// --- Open auth routes (no session required to obtain one). ---
	mux.Handle("POST /v1/auth/signup", handlerOr(d.Signup))
	mux.Handle("POST /v1/auth/login", handlerOr(d.Login))
	mux.Handle("POST /v1/auth/logout", handlerOr(d.Logout))
	mux.Handle("GET /v1/auth/oidc/start", handlerOr(d.OIDCStart))
	mux.Handle("GET /v1/auth/oidc/callback", handlerOr(d.OIDCCallback))

	// --- Platform-admin surface (/admin): session then platform-admin gate. ---
	platformAdmin := func(h http.Handler) http.Handler {
		return chain(handlerOr(h), mw(d.RequireSession), mw(d.RequirePlatformAdmin))
	}
	mux.Handle("GET /admin/audit", platformAdmin(d.AuditList))
	mux.Handle("POST /admin/impersonations", platformAdmin(mustCSRF(d, d.ImpersonationStart)))
	mux.Handle("DELETE /admin/impersonations/{id}", platformAdmin(mustCSRF(d, d.ImpersonationEnd)))

	// --- Per-tenant authenticated surface: API-key scope then rate limit. ---
	// The tenant is derived from the key by the scope middleware (FR-ACC-03), so
	// the rate limiter and handler read it from context. Auth precedes the rate
	// limiter because the limiter is credential-keyed (ADR-0027).
	tenantScoped := func(scope Middleware, h http.Handler) http.Handler {
		return chain(handlerOr(h), mw(scope), mw(d.RateLimit))
	}
	mux.Handle("GET /v1/usage", tenantScoped(d.RequireScopeAdmin, d.UsageList))

	// Sources/documents/jobs/settings/members/api-keys and the admin tenant
	// routes are later EPIC-04 stories (04.3–04.6). They are intentionally NOT
	// registered here: an unregistered path yields the not_found envelope below,
	// which is the seam their handlers slot into.

	// Global chain (outer -> inner), SPEC-07 §1 order with the credential-keyed
	// rate limiter moved inside per-route auth (ADR-0027):
	//   request-id/logging/tracing/metrics -> recovery -> CORS -> [route].
	root := notFoundFallback(mux)
	return chain(root,
		obs.Middleware(d.Log, d.Metrics),
		Recover(d.Log),
		cors(),
	)
}

// notFoundFallback wraps the mux so that a request matching no route returns the
// SPEC-07 JSON not_found envelope rather than net/http's plain-text 404.
func notFoundFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			WriteError(w, r, http.StatusNotFound, CodeNotFound, "resource not found")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// mw returns m, or a pass-through when m is nil (a partially-wired server boots).
func mw(m Middleware) Middleware {
	if m == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return m
}

// mustCSRF wraps a mutating handler in the CSRF middleware when one is wired.
// Platform-admin mutations ride a session, so they are CSRF-protected like any
// other session-authenticated mutation (SPEC-09 §3).
func mustCSRF(d Deps, h http.Handler) http.Handler {
	return mw(d.CSRF)(handlerOr(h))
}

// handlerOr returns h, or a seam handler returning the not_implemented-shaped
// 404 envelope when h is nil, so a route whose handler is not built yet fails
// closed with a spec response instead of a nil-pointer panic.
func handlerOr(h http.Handler) http.Handler {
	if h != nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusNotFound, CodeNotFound, "resource not found")
	})
}
