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

	// Tenant-scoped session middleware (STORY-11.2, ADR-0075): authorises the
	// session user against the tenant named by the {tenantId} path segment —
	// platform-admin or a member role satisfying the route's permission — then
	// injects tenant.WithTenantID so the reused tenant-scoped handlers (e.g.
	// sources.Handlers) read it exactly as they do on the Bearer /v1/* surface.
	// Reads use RequireTenantSourcesRead (any role); writes use
	// RequireTenantSourcesWrite (owner/admin per the SPEC-02 §4 matrix). Each is
	// one auth.AuthzService.RequireTenantAccess(perm) instance, pre-built in
	// api_server.go so this package stays free of cp/auth's concrete types (the
	// same shape as RequireRoleAdmin above).
	RequireTenantSourcesRead  Middleware
	RequireTenantSourcesWrite Middleware

	// Tenant-scoped session middleware for the settings/members/api-keys surface
	// (STORY-11.5, ADR-0075). RequireTenantChangeSettings gates the settings PATCH
	// (PermChangeSettings, owner/admin); RequireTenantManageMembers gates every
	// members/api-keys write plus the sensitive api-keys list (PermManageMembers,
	// owner/admin). Reads of settings/members reuse RequireTenantSourcesRead
	// (PermQuery, any member). Each is one RequireTenantAccess(perm) instance
	// pre-built in api_server.go.
	RequireTenantChangeSettings Middleware
	RequireTenantManageMembers  Middleware

	// Handlers that already exist (auth + the 03.x admin handlers).
	Signup             http.Handler
	Login              http.Handler
	Logout             http.Handler
	OIDCStart          http.Handler
	OIDCCallback       http.Handler
	Me                 http.Handler // GET /v1/auth/me (session required, no CSRF)
	ConnectorKinds     http.Handler // GET /admin/connector-kinds (session required, no CSRF)
	AuditList          http.Handler
	UsageList          http.Handler
	ImpersonationStart http.Handler
	ImpersonationEnd   http.Handler

	// Platform-admin tenant handlers (STORY-04.6, FR-TEN-01/05/07). These sit
	// under /admin behind RequireSession -> RequirePlatformAdmin, with CSRF on the
	// mutations (like impersonations). They operate on the control-plane pool and
	// are NOT tenant-scoped (a platform admin acts across tenants). A nil handler
	// is the not-implemented seam (handlerOr).
	TenantCreate http.Handler // POST /admin/tenants
	TenantList   http.Handler // GET /admin/tenants
	TenantUpdate http.Handler // PATCH /admin/tenants/{id}
	TenantDelete http.Handler // DELETE /admin/tenants/{id}

	// Sources resource handlers (STORY-04.3, FR-SRC-01/14). All are `admin`
	// scope; a nil handler is the not-implemented seam (handlerOr).
	SourceList   http.Handler // GET /v1/sources
	SourceCreate http.Handler // POST /v1/sources
	SourceGet    http.Handler // GET /v1/sources/{id}
	SourceUpdate http.Handler // PATCH /v1/sources/{id}
	SourceDelete http.Handler // DELETE /v1/sources/{id}
	SourceSync   http.Handler // POST /v1/sources/{id}/sync
	SourceTest   http.Handler // POST /v1/sources/{id}/test

	// Documents resource handlers (STORY-04.4, FR-SRC-02/FR-ADM-03). Documents are
	// tenant content reached via tenant.DB (ADR-0003); a nil handler is the
	// not-implemented seam (handlerOr). Scopes differ per route (SPEC-07 §2).
	DocumentIngest http.Handler // POST /v1/documents (ingest scope)
	DocumentList   http.Handler // GET /v1/documents (query scope)
	DocumentGet    http.Handler // GET /v1/documents/{id} (query scope)
	DocumentDelete http.Handler // DELETE /v1/documents/{id} (ingest scope)
	DocumentChunks http.Handler // GET /v1/documents/{id}/chunks (admin scope)

	// Session-admin settings/members/api-keys handlers (STORY-11.5, ADR-0075,
	// ISSUE-0064). SettingsGet/Patch are the reused tenants.SettingsHandlers; the
	// member/key handlers wrap the existing MembershipService/APIKeyService. All
	// read the tenant from context (FR-ACC-03); a nil handler is the seam. Mounted
	// under /admin/tenants/{tenantId} behind session -> tenant-access.
	SettingsGet   http.Handler // GET .../settings
	SettingsPatch http.Handler // PATCH .../settings
	MemberList    http.Handler // GET .../members
	MemberAdd     http.Handler // POST .../members
	MemberSetRole http.Handler // PATCH .../members/{userId}
	MemberRemove  http.Handler // DELETE .../members/{userId}
	KeyList       http.Handler // GET .../api-keys
	KeyCreate     http.Handler // POST .../api-keys
	KeyRevoke     http.Handler // DELETE .../api-keys/{keyId}

	// Jobs resource handlers (STORY-04.5, FR-ADM-02). Jobs are control-plane
	// tracking rows (the history/mirror view, C-3); a nil handler is the
	// not-implemented seam (handlerOr). All are `admin` scope (SPEC-07 §2).
	JobList   http.Handler // GET /v1/jobs
	JobGet    http.Handler // GET /v1/jobs/{id}
	JobCancel http.Handler // POST /v1/jobs/{id}/cancel

	// Retrieve handler (STORY-08.2, FR-RET-08). Reads tenant content via the
	// resolver (ADR-0003); the tenant is derived from the API key (FR-ACC-03).
	// `query` scope (SPEC-07 §2); a nil handler is the not-implemented seam.
	Retrieve http.Handler // POST /v1/retrieve (query scope)

	// Query handler (STORY-08.6, FR-RET-06). The grounded answering endpoint —
	// retrieve + answer, in JSON or SSE form (SPEC-06 §6). Reads tenant content via
	// the resolver (ADR-0003); the tenant is derived from the API key (FR-ACC-03).
	// `query` scope (SPEC-07 §2); a nil handler is the not-implemented seam.
	Query http.Handler // POST /v1/query (query scope)

	// Query log + feedback handlers (STORY-08.8, FR-RET-09/10). query_log and
	// query_feedback are tenant content reached via the resolver (ADR-0003); the
	// tenant is derived from the API key (FR-ACC-03). Feedback is `query` scope, the
	// admin query-log listing is `admin` scope (SPEC-07 §2/§2g). A nil handler is
	// the not-implemented seam.
	Feedback  http.Handler // POST /v1/feedback (query scope)
	QueryList http.Handler // GET /v1/queries (admin scope)

	// Eval report handlers (STORY-12.4, FR-ADM-04). eval_runs/eval_results are
	// tenant content reached via the resolver (ADR-0003); read-only. Session-only
	// (mounted under /admin/tenants/{tenantId}/eval), no Bearer surface. A nil
	// handler is the not-implemented seam.
	EvalRunList http.Handler // GET .../eval/runs
	EvalReport  http.Handler // GET .../eval/runs/{id}
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

	// Session hydration for the admin UI (SPEC-11 §2.1): session required, no
	// platform-admin gate, GET so no CSRF.
	mux.Handle("GET /v1/auth/me", chain(handlerOr(d.Me), mw(d.RequireSession)))

	// Connector-kind form schema (SPEC-11 §10, ADR-0075, STORY-11.2): session
	// required ONLY — platform-global (not tenant-scoped, no RequirePlatformAdmin
	// gate: any signed-in member renders the sources form), GET so no CSRF. The
	// admin UI reads it once to drive every kind's schema-driven create/edit form.
	mux.Handle("GET /admin/connector-kinds", chain(handlerOr(d.ConnectorKinds), mw(d.RequireSession)))

	// --- Platform-admin surface (/admin): session then platform-admin gate. ---
	platformAdmin := func(h http.Handler) http.Handler {
		return chain(handlerOr(h), mw(d.RequireSession), mw(d.RequirePlatformAdmin))
	}
	mux.Handle("GET /admin/audit", platformAdmin(d.AuditList))
	mux.Handle("POST /admin/impersonations", platformAdmin(mustCSRF(d, d.ImpersonationStart)))
	mux.Handle("DELETE /admin/impersonations/{id}", platformAdmin(mustCSRF(d, d.ImpersonationEnd)))

	// Admin tenant lifecycle (STORY-04.6, FR-TEN-01/05/07, SPEC-07 §2). Session +
	// platform-admin gate; the mutations carry CSRF like impersonations. These are
	// deliberately NOT under /v1 and carry no tenant_id-derived scope: the platform
	// admin acts across tenants (FR-ACC-03 governs the tenant-scoped /v1 surface).
	mux.Handle("POST /admin/tenants", platformAdmin(mustCSRF(d, d.TenantCreate)))
	mux.Handle("GET /admin/tenants", platformAdmin(d.TenantList))
	mux.Handle("PATCH /admin/tenants/{id}", platformAdmin(mustCSRF(d, d.TenantUpdate)))
	mux.Handle("DELETE /admin/tenants/{id}", platformAdmin(mustCSRF(d, d.TenantDelete)))

	// --- Per-tenant authenticated surface: API-key scope then rate limit. ---
	// The tenant is derived from the key by the scope middleware (FR-ACC-03), so
	// the rate limiter and handler read it from context. Auth precedes the rate
	// limiter because the limiter is credential-keyed (ADR-0027).
	tenantScoped := func(scope Middleware, h http.Handler) http.Handler {
		return chain(handlerOr(h), mw(scope), mw(d.RateLimit))
	}
	mux.Handle("GET /v1/usage", tenantScoped(d.RequireScopeAdmin, d.UsageList))

	// Sources (STORY-04.3, FR-SRC-01/14, SPEC-07 §2). All admin scope; the tenant
	// is derived from the API key (FR-ACC-03). These are Bearer-authenticated, so
	// no CSRF applies (CSRF guards cookie-session mutations only).
	mux.Handle("GET /v1/sources", tenantScoped(d.RequireScopeAdmin, d.SourceList))
	mux.Handle("POST /v1/sources", tenantScoped(d.RequireScopeAdmin, d.SourceCreate))
	mux.Handle("GET /v1/sources/{id}", tenantScoped(d.RequireScopeAdmin, d.SourceGet))
	mux.Handle("PATCH /v1/sources/{id}", tenantScoped(d.RequireScopeAdmin, d.SourceUpdate))
	mux.Handle("DELETE /v1/sources/{id}", tenantScoped(d.RequireScopeAdmin, d.SourceDelete))
	mux.Handle("POST /v1/sources/{id}/sync", tenantScoped(d.RequireScopeAdmin, d.SourceSync))
	mux.Handle("POST /v1/sources/{id}/test", tenantScoped(d.RequireScopeAdmin, d.SourceTest))

	// Session admin sources (STORY-11.2, ADR-0075, SPEC-11 §10): the SAME
	// sources.Handlers as the Bearer /v1/sources surface above, mounted behind
	// the session tenant-access gate instead of an API key. Session +
	// RequireTenantAccess resolves and authorises {tenantId} from the path (no
	// RateLimit here — that guards credential-keyed Bearer traffic, ADR-0027).
	// {id} in these routes is still the SOURCE id (unchanged from the handlers'
	// own r.PathValue("id") lookup); the tenant path segment is named
	// {tenantId} to avoid colliding with it. GET carries no CSRF; the mutations
	// do (SPEC-09 §3).
	tenantSources := func(access Middleware, h http.Handler) http.Handler {
		return chain(handlerOr(h), mw(d.RequireSession), mw(access))
	}
	mux.Handle("GET /admin/tenants/{tenantId}/sources", tenantSources(d.RequireTenantSourcesRead, d.SourceList))
	mux.Handle("POST /admin/tenants/{tenantId}/sources", tenantSources(d.RequireTenantSourcesWrite, mustCSRF(d, d.SourceCreate)))
	mux.Handle("GET /admin/tenants/{tenantId}/sources/{id}", tenantSources(d.RequireTenantSourcesRead, d.SourceGet))
	mux.Handle("PATCH /admin/tenants/{tenantId}/sources/{id}", tenantSources(d.RequireTenantSourcesWrite, mustCSRF(d, d.SourceUpdate)))
	mux.Handle("DELETE /admin/tenants/{tenantId}/sources/{id}", tenantSources(d.RequireTenantSourcesWrite, mustCSRF(d, d.SourceDelete)))
	mux.Handle("POST /admin/tenants/{tenantId}/sources/{id}/sync", tenantSources(d.RequireTenantSourcesWrite, mustCSRF(d, d.SourceSync)))
	mux.Handle("POST /admin/tenants/{tenantId}/sources/{id}/test", tenantSources(d.RequireTenantSourcesWrite, mustCSRF(d, d.SourceTest)))

	// Session admin jobs (STORY-11.3, ADR-0075, FR-ADM-02): the SAME jobs.Handlers
	// as the Bearer /v1/jobs surface below, mounted behind the session tenant-access
	// gate instead of an API key. The RequireTenantSources{Read,Write} gates are
	// permission gates — read is PermQuery (any role), write is PermManageSources
	// (owner/admin) — reused here for jobs (cancelling a sync is a source-management
	// write); the "Sources" in the field name is historical, not a resource scope.
	// {id} is the job id; {tenantId} is the tenant path segment. Cancel carries CSRF
	// (SPEC-09 §3); the GETs do not.
	mux.Handle("GET /admin/tenants/{tenantId}/jobs", tenantSources(d.RequireTenantSourcesRead, d.JobList))
	mux.Handle("GET /admin/tenants/{tenantId}/jobs/{id}", tenantSources(d.RequireTenantSourcesRead, d.JobGet))
	mux.Handle("POST /admin/tenants/{tenantId}/jobs/{id}/cancel", tenantSources(d.RequireTenantSourcesWrite, mustCSRF(d, d.JobCancel)))

	// Session admin settings/members/api-keys (STORY-11.5, ADR-0075, ISSUE-0064):
	// the reused SettingsHandlers + the members/api-key handlers, mounted behind
	// session -> tenant-access via the same tenantSources helper (session then the
	// per-route permission gate). Reads use the read gate (PermQuery, any member);
	// settings PATCH uses the change-settings gate; every members/keys write, and
	// the sensitive api-keys list, uses the manage-members gate (SPEC-02 §4).
	// {userId}/{keyId} are the handlers' own resource ids; {tenantId} is the tenant
	// path segment. Mutations carry CSRF (SPEC-09 §3); the GETs do not.
	mux.Handle("GET /admin/tenants/{tenantId}/settings", tenantSources(d.RequireTenantSourcesRead, d.SettingsGet))
	mux.Handle("PATCH /admin/tenants/{tenantId}/settings", tenantSources(d.RequireTenantChangeSettings, mustCSRF(d, d.SettingsPatch)))
	mux.Handle("GET /admin/tenants/{tenantId}/members", tenantSources(d.RequireTenantSourcesRead, d.MemberList))
	mux.Handle("POST /admin/tenants/{tenantId}/members", tenantSources(d.RequireTenantManageMembers, mustCSRF(d, d.MemberAdd)))
	mux.Handle("PATCH /admin/tenants/{tenantId}/members/{userId}", tenantSources(d.RequireTenantManageMembers, mustCSRF(d, d.MemberSetRole)))
	mux.Handle("DELETE /admin/tenants/{tenantId}/members/{userId}", tenantSources(d.RequireTenantManageMembers, mustCSRF(d, d.MemberRemove)))
	mux.Handle("GET /admin/tenants/{tenantId}/api-keys", tenantSources(d.RequireTenantManageMembers, d.KeyList))
	mux.Handle("POST /admin/tenants/{tenantId}/api-keys", tenantSources(d.RequireTenantManageMembers, mustCSRF(d, d.KeyCreate)))
	mux.Handle("DELETE /admin/tenants/{tenantId}/api-keys/{keyId}", tenantSources(d.RequireTenantManageMembers, mustCSRF(d, d.KeyRevoke)))

	// Session admin documents (STORY-11.4, ADR-0075, FR-ADM-03): the SAME
	// documents.Handlers as the Bearer /v1/documents surface below, mounted behind
	// the session tenant-access gate. Read-only (list, detail, chunks debug view);
	// upload/delete stay on the Bearer surface only. Every route is a read, so the
	// read gate (RequireTenantSourcesRead, PermQuery, any role) guards all three and
	// none carry CSRF. {id} is the document id; {tenantId} is the tenant path
	// segment.
	mux.Handle("GET /admin/tenants/{tenantId}/documents", tenantSources(d.RequireTenantSourcesRead, d.DocumentList))
	mux.Handle("GET /admin/tenants/{tenantId}/documents/{id}", tenantSources(d.RequireTenantSourcesRead, d.DocumentGet))
	mux.Handle("GET /admin/tenants/{tenantId}/documents/{id}/chunks", tenantSources(d.RequireTenantSourcesRead, d.DocumentChunks))

	// Session admin query playground (STORY-11.6, ADR-0075, FR-RET-06/09): the SAME
	// query.Handlers.Query and querylog.Handlers.Feedback as the Bearer /v1/query and
	// /v1/feedback surfaces, mounted behind the session tenant-access gate. Both map
	// to the query permission (PermQuery, any role) — the same access the Bearer
	// routes require via `query` scope — so both use RequireTenantSourcesRead. Both
	// are session-cookie POSTs, so both carry CSRF (SPEC-09 §3). The query handler
	// serves JSON or SSE off the request's own `stream` flag; the BFF streams the
	// response body through unchanged.
	mux.Handle("POST /admin/tenants/{tenantId}/query", tenantSources(d.RequireTenantSourcesRead, mustCSRF(d, d.Query)))
	mux.Handle("POST /admin/tenants/{tenantId}/feedback", tenantSources(d.RequireTenantSourcesRead, mustCSRF(d, d.Feedback)))

	// Session admin eval report (STORY-12.4, ADR-0075, ADR-0072, FR-ADM-04): the
	// read-only render surface over the `ragctl eval report` data contract. Runs and
	// their per-case results are tenant content (eval_runs/eval_results); both routes
	// are reads, so both use RequireTenantSourcesRead and carry no CSRF. The mutating
	// eval surface (cases CRUD, run) stays on the CLI (STORY-12.1/12.2). {id} is the
	// run id; {tenantId} is the tenant path segment.
	mux.Handle("GET /admin/tenants/{tenantId}/eval/runs", tenantSources(d.RequireTenantSourcesRead, d.EvalRunList))
	mux.Handle("GET /admin/tenants/{tenantId}/eval/runs/{id}", tenantSources(d.RequireTenantSourcesRead, d.EvalReport))

	// Documents (STORY-04.4, FR-SRC-02/FR-ADM-03, SPEC-07 §2). Tenant content
	// reached through the resolver (ADR-0003); the tenant is derived from the API
	// key (FR-ACC-03). Scopes follow SPEC-07 §2: ingest for upload/delete, query
	// for list/get, admin for the chunks debug endpoint. Bearer-authenticated, so
	// no CSRF applies.
	mux.Handle("POST /v1/documents", tenantScoped(d.RequireScopeIngest, d.DocumentIngest))
	mux.Handle("GET /v1/documents", tenantScoped(d.RequireScopeQuery, d.DocumentList))
	mux.Handle("GET /v1/documents/{id}", tenantScoped(d.RequireScopeQuery, d.DocumentGet))
	mux.Handle("DELETE /v1/documents/{id}", tenantScoped(d.RequireScopeIngest, d.DocumentDelete))
	mux.Handle("GET /v1/documents/{id}/chunks", tenantScoped(d.RequireScopeAdmin, d.DocumentChunks))

	// Jobs (STORY-04.5, FR-ADM-02, SPEC-07 §2). Jobs are control-plane tracking
	// rows (the history/mirror view, C-3); the tenant is derived from the API key
	// (FR-ACC-03). All admin scope. Bearer-authenticated, so no CSRF applies.
	mux.Handle("GET /v1/jobs", tenantScoped(d.RequireScopeAdmin, d.JobList))
	mux.Handle("GET /v1/jobs/{id}", tenantScoped(d.RequireScopeAdmin, d.JobGet))
	mux.Handle("POST /v1/jobs/{id}/cancel", tenantScoped(d.RequireScopeAdmin, d.JobCancel))

	// Retrieve (STORY-08.2, FR-RET-08, SPEC-07 §2). Tenant content reached through
	// the resolver (ADR-0003); the tenant is derived from the API key (FR-ACC-03).
	// `query` scope. Bearer-authenticated, so no CSRF applies.
	mux.Handle("POST /v1/retrieve", tenantScoped(d.RequireScopeQuery, d.Retrieve))

	// Query (STORY-08.6, FR-RET-06, SPEC-06 §6, SPEC-07 §2f). The grounded answering
	// endpoint (retrieve + answer), JSON or SSE. Tenant content via the resolver
	// (ADR-0003); tenant derived from the API key (FR-ACC-03). `query` scope.
	// Bearer-authenticated, so no CSRF applies.
	mux.Handle("POST /v1/query", tenantScoped(d.RequireScopeQuery, d.Query))

	// Query log + feedback (STORY-08.8, FR-RET-09/10, SPEC-07 §2/§2g). Tenant
	// content via the resolver (ADR-0003); tenant derived from the API key
	// (FR-ACC-03). Feedback is `query` scope; the admin query-log listing is `admin`
	// scope. Bearer-authenticated, so no CSRF applies.
	mux.Handle("POST /v1/feedback", tenantScoped(d.RequireScopeQuery, d.Feedback))
	mux.Handle("GET /v1/queries", tenantScoped(d.RequireScopeAdmin, d.QueryList))

	// The Bearer /v1/settings, /v1/members and /v1/api-keys surfaces remain later
	// EPIC-04 work (unregistered paths fall through to the not_found envelope
	// below). The session-admin equivalents landed above under
	// /admin/tenants/{tenantId}/... (STORY-11.5).

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
