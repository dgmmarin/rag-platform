package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

// OpenAPI generation (SPEC-07 §3, ADR-0028).
//
// The OpenAPI 3.1 document is built HERE, in Go, from the same route table the
// router mounts and the same error-code constants WriteError emits — so the spec
// is genuinely derived from code, not a hand-maintained parallel file. It is:
//   - served as JSON at GET /v1/openapi.json (Document -> encoding/json), and
//   - marshalled to the checked-in api/openapi.yaml by `mise run openapi`
//     (`ragctl openapi`), whose freshness a drift-guard test enforces.
//
// SPEC-07 §3 suggests oapi-codegen or swag; ADR-0028 records the deliberate
// divergence — for a small, mostly-seam surface a build-from-Go-value document
// with a drift guard and a jsonschema contract test meets the same intent
// (code-derived spec, served as JSON, responses validated in CI) with no new
// code-generation toolchain dependency (the lazy-senior-dev rung: reuse
// gopkg.in/yaml.v3, already in the module graph, and santhosh-tekuri/jsonschema,
// already a direct dependency).

// OpenAPI is the subset of the OpenAPI 3.1 object model the platform emits. The
// top level uses structs for a stable, human-readable field order; the nested
// collections (paths, responses, schemas, security schemes) are maps, which both
// encoding/json and gopkg.in/yaml.v3 marshal with deterministically sorted keys —
// which is what keeps the generated api/openapi.yaml stable for the drift guard.
type OpenAPI struct {
	OpenAPI    string              `json:"openapi" yaml:"openapi"`
	Info       Info                `json:"info" yaml:"info"`
	Servers    []Server            `json:"servers,omitempty" yaml:"servers,omitempty"`
	Tags       []Tag               `json:"tags,omitempty" yaml:"tags,omitempty"`
	Paths      map[string]PathItem `json:"paths" yaml:"paths"`
	Components Components          `json:"components" yaml:"components"`
}

// Info is the OpenAPI info object.
type Info struct {
	Title       string `json:"title" yaml:"title"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Server is an OpenAPI server object.
type Server struct {
	URL         string `json:"url" yaml:"url"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Tag groups operations in the rendered spec.
type Tag struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// PathItem holds the operations defined on one path. Only the methods this
// platform uses are modelled; a nil pointer is omitted.
type PathItem struct {
	Get    *Operation `json:"get,omitempty" yaml:"get,omitempty"`
	Post   *Operation `json:"post,omitempty" yaml:"post,omitempty"`
	Patch  *Operation `json:"patch,omitempty" yaml:"patch,omitempty"`
	Delete *Operation `json:"delete,omitempty" yaml:"delete,omitempty"`
}

// Operation is one method on a path.
type Operation struct {
	Summary     string                `json:"summary" yaml:"summary"`
	OperationID string                `json:"operationId" yaml:"operationId"`
	Tags        []string              `json:"tags,omitempty" yaml:"tags,omitempty"`
	Security    []map[string][]string `json:"security,omitempty" yaml:"security,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty" yaml:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses" yaml:"responses"`
}

// RequestBody is an OpenAPI request-body object.
type RequestBody struct {
	Description string               `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool                 `json:"required,omitempty" yaml:"required,omitempty"`
	Content     map[string]MediaType `json:"content" yaml:"content"`
}

// Parameter is a path/query parameter.
type Parameter struct {
	Name        string         `json:"name" yaml:"name"`
	In          string         `json:"in" yaml:"in"`
	Required    bool           `json:"required,omitempty" yaml:"required,omitempty"`
	Description string         `json:"description,omitempty" yaml:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty" yaml:"schema,omitempty"`
}

// Response is one response entry, keyed by status code in the map that holds it.
type Response struct {
	Description string               `json:"description" yaml:"description"`
	Content     map[string]MediaType `json:"content,omitempty" yaml:"content,omitempty"`
}

// MediaType carries the schema for a content type.
type MediaType struct {
	Schema map[string]any `json:"schema,omitempty" yaml:"schema,omitempty"`
}

// Components holds the reusable schemas and security schemes.
type Components struct {
	Schemas         map[string]any            `json:"schemas" yaml:"schemas"`
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty" yaml:"securitySchemes,omitempty"`
}

// SecurityScheme describes an authentication mechanism.
type SecurityScheme struct {
	Type         string `json:"type" yaml:"type"`
	Scheme       string `json:"scheme,omitempty" yaml:"scheme,omitempty"`
	In           string `json:"in,omitempty" yaml:"in,omitempty"`
	Name         string `json:"name,omitempty" yaml:"name,omitempty"`
	Description  string `json:"description,omitempty" yaml:"description,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty" yaml:"bearerFormat,omitempty"`
}

// auth identifies the middleware chain guarding a route; it selects the security
// requirement and the standard error responses documented for the operation.
type auth int

const (
	authNone          auth = iota // open (no credential)
	authSession                   // session cookie
	authPlatformAdmin             // session cookie + platform-admin
	authScopeAdmin                // API key with admin scope + rate limit
	authScopeIngest               // API key with ingest scope + rate limit
	authScopeQuery                // API key with query scope + rate limit
)

// isAPIKeyScope reports whether an auth is one of the Bearer-API-key scopes; they
// share the same security requirement and 401/403/429 error responses.
func isAPIKeyScope(a auth) bool {
	return a == authScopeAdmin || a == authScopeIngest || a == authScopeQuery
}

// route is one row of the live API surface. The router mounts these today; the
// spec is built from the same list so the two stay in step (04.3–04.6 append
// their rows here as their handlers land).
type route struct {
	method      string
	path        string
	tag         string
	summary     string
	operationID string
	auth        auth
	params      []Parameter
	// success is the 2xx response description; okStatus its code (defaults 200).
	success  string
	okStatus string
	// reqSchema/okSchema name component schemas for a typed JSON request body and 2xx
	// JSON response (integration endpoints). Empty leaves the body prose-only.
	reqSchema string
	okSchema  string
	// reqBodyType is the request media type when reqSchema is a non-JSON body
	// (e.g. multipart/form-data for uploads); empty defaults to application/json.
	reqBodyType string
	// extra are documented error responses beyond those the auth chain implies
	// (e.g. 404 on an {id} route, 409 on a conflicting sync/duplicate name).
	extra []errResp
}

// errResp is one extra documented error response for a route.
type errResp struct {
	status string
	desc   string
}

// liveRoutes is the single source of truth for the documented surface. It mirrors
// the routes registered in New (router.go); a route added there must be added
// here or the contract test in test/e2e flags the divergence.
func liveRoutes() []route {
	return []route{
		{method: "GET", path: "/healthz", tag: "operational", summary: "Liveness probe.", operationID: "healthz", auth: authNone, success: "service is alive"},
		{method: "GET", path: "/readyz", tag: "operational", summary: "Readiness probe (control-plane ping).", operationID: "readyz", auth: authNone, success: "service is ready"},
		{method: "GET", path: "/metrics", tag: "operational", summary: "Prometheus metrics.", operationID: "metrics", auth: authNone, success: "metrics exposition"},

		{method: "GET", path: "/v1/openapi.json", tag: "operational", summary: "This OpenAPI document as JSON.", operationID: "openapiJSON", auth: authNone, success: "the OpenAPI 3.1 document"},

		{method: "POST", path: "/v1/auth/signup", tag: "auth", summary: "Create a control-plane user.", operationID: "authSignup", auth: authNone, success: "user created"},
		{method: "POST", path: "/v1/auth/login", tag: "auth", summary: "Start a session (email + password).", operationID: "authLogin", auth: authNone, success: "session established"},
		{method: "POST", path: "/v1/auth/logout", tag: "auth", summary: "Revoke the current session.", operationID: "authLogout", auth: authNone, success: "session revoked"},
		{method: "GET", path: "/v1/auth/oidc/start", tag: "auth", summary: "Begin the OIDC authorization-code flow.", operationID: "oidcStart", auth: authNone, success: "redirect to the identity provider"},
		{method: "GET", path: "/v1/auth/oidc/callback", tag: "auth", summary: "OIDC redirect callback.", operationID: "oidcCallback", auth: authNone, success: "session established"},

		{method: "GET", path: "/admin/audit", tag: "platform", summary: "Read a tenant's audit log.", operationID: "adminAuditList", auth: authPlatformAdmin,
			params:  []Parameter{{Name: "tenant", In: "query", Required: true, Description: "Tenant id to read the audit log for.", Schema: strSchema()}},
			success: "audit entries"},
		{method: "POST", path: "/admin/impersonations", tag: "platform", summary: "Start a platform-admin impersonation grant.", operationID: "adminImpersonationStart", auth: authPlatformAdmin, success: "grant created", okStatus: "201"},
		{method: "DELETE", path: "/admin/impersonations/{id}", tag: "platform", summary: "End an impersonation grant.", operationID: "adminImpersonationEnd", auth: authPlatformAdmin,
			params:   []Parameter{{Name: "id", In: "path", Required: true, Description: "Impersonation grant id.", Schema: strSchema()}},
			success:  "grant ended",
			okStatus: "204"},

		// Admin tenant lifecycle (STORY-04.6, FR-TEN-01/05/07). Platform scope: not
		// tenant-derived; the tenant is a route/body value (SPEC-07 §2).
		{method: "POST", path: "/admin/tenants", tag: "platform", summary: "Enrol a tenant (provisions its database; returns the tenant + provision job id).", operationID: "adminTenantCreate", auth: authPlatformAdmin,
			success: "tenant provisioned", okStatus: "201",
			extra: []errResp{{"400", "invalid tenant body"}}},
		{method: "GET", path: "/admin/tenants", tag: "platform", summary: "List tenants (registry view; no connection secrets).", operationID: "adminTenantList", auth: authPlatformAdmin,
			params: []Parameter{
				{Name: "limit", In: "query", Description: "Page size (default 50, max 200).", Schema: map[string]any{"type": "integer"}},
				{Name: "cursor", In: "query", Description: "Opaque pagination cursor from a prior next_cursor.", Schema: strSchema()},
			},
			success: "a page of tenants ({items, next_cursor})",
			extra:   []errResp{{"400", "invalid limit or cursor"}}},
		{method: "PATCH", path: "/admin/tenants/{id}", tag: "platform", summary: "Update a tenant: status (suspend/resume), db connection (move), and/or settings.", operationID: "adminTenantUpdate", auth: authPlatformAdmin,
			params:  []Parameter{tenantIDParam()},
			success: "the updated tenant",
			extra:   []errResp{{"400", "invalid patch body"}, {"404", "no such tenant"}, {"409", "illegal status transition or immutable settings change"}}},
		{method: "DELETE", path: "/admin/tenants/{id}", tag: "platform", summary: "Schedule a tenant's deletion with a grace period (?grace, default 7 days).", operationID: "adminTenantDelete", auth: authPlatformAdmin,
			params: []Parameter{
				tenantIDParam(),
				{Name: "grace", In: "query", Description: "Grace period before the deletion is irreversible (Go duration, e.g. 168h; default 7 days).", Schema: strSchema()},
			},
			success: "deletion scheduled", okStatus: "202",
			extra: []errResp{{"400", "invalid grace"}, {"404", "no such tenant"}, {"409", "tenant is not in a deletable state"}}},

		{method: "GET", path: "/v1/usage", tag: "usage", summary: "Daily usage rows for the authenticated tenant.", operationID: "usageList", auth: authScopeAdmin,
			params: []Parameter{
				{Name: "from", In: "query", Description: "Inclusive start date (YYYY-MM-DD).", Schema: strSchema()},
				{Name: "to", In: "query", Description: "Inclusive end date (YYYY-MM-DD).", Schema: strSchema()},
			},
			success: "usage rows"},

		// Sources (STORY-04.3, FR-SRC-01/14). Tenant derived from the API key.
		{method: "GET", path: "/v1/sources", tag: "sources", summary: "List the tenant's sources.", operationID: "sourceList", auth: authScopeAdmin,
			params: []Parameter{
				{Name: "limit", In: "query", Description: "Page size (default 50, max 200).", Schema: map[string]any{"type": "integer"}},
				{Name: "cursor", In: "query", Description: "Opaque pagination cursor from a prior next_cursor.", Schema: strSchema()},
			},
			success: "a page of sources ({items, next_cursor})",
			extra:   []errResp{{"400", "invalid limit or cursor"}}},
		{method: "POST", path: "/v1/sources", tag: "sources", summary: "Create a source (config validated by the connector).", operationID: "sourceCreate", auth: authScopeAdmin,
			success: "source created", okStatus: "201",
			extra: []errResp{{"400", "invalid source body"}, {"409", "a source with that name already exists"}}},
		{method: "GET", path: "/v1/sources/{id}", tag: "sources", summary: "Get one source.", operationID: "sourceGet", auth: authScopeAdmin,
			params:  []Parameter{sourceIDParam()},
			success: "the source",
			extra:   []errResp{{"404", "no such source"}}},
		{method: "PATCH", path: "/v1/sources/{id}", tag: "sources", summary: "Update a source (includes pause/resume via status).", operationID: "sourceUpdate", auth: authScopeAdmin,
			params:  []Parameter{sourceIDParam()},
			success: "the updated source",
			extra:   []errResp{{"400", "invalid patch body"}, {"404", "no such source"}, {"409", "a source with that name already exists"}}},
		{method: "DELETE", path: "/v1/sources/{id}", tag: "sources", summary: "Delete a source (enqueues delete_source).", operationID: "sourceDelete", auth: authScopeAdmin,
			params:  []Parameter{sourceIDParam()},
			success: "deletion scheduled", okStatus: "202",
			extra: []errResp{{"404", "no such source"}}},
		{method: "POST", path: "/v1/sources/{id}/sync", tag: "sources", summary: "Start a manual sync (Idempotency-Key honoured).", operationID: "sourceSync", auth: authScopeAdmin,
			params:  []Parameter{sourceIDParam()},
			success: "sync job enqueued", okStatus: "202",
			extra: []errResp{{"404", "no such source"}, {"409", "a sync is already queued or running"}}},
		{method: "POST", path: "/v1/sources/{id}/test", tag: "sources", summary: "Test a source's configuration and credentials.", operationID: "sourceTest", auth: authScopeAdmin,
			params:  []Parameter{sourceIDParam()},
			success: "connection ok",
			extra: []errResp{
				{"400", "the connection or credential test failed; the envelope message is actionable (e.g. unreachable host, bad credentials)"},
				{"404", "no such source, or the connector framework is not available yet"},
			}},

		// Documents (STORY-04.4, FR-SRC-02/FR-ADM-03). Tenant content, tenant
		// derived from the API key (FR-ACC-03). Scopes differ per route (SPEC-07 §2).
		{method: "POST", path: "/v1/documents", tag: "documents", summary: "Upload a file (multipart) and enqueue ingestion (Idempotency-Key honoured).", operationID: "documentIngest", auth: authScopeIngest,
			success: "ingestion enqueued", okStatus: "202",
			extra: []errResp{{"400", "missing file, unsupported type, or upload too large"}, {"404", "object storage is not available yet"}, {"503", "tenant is not available"}}},
		{method: "GET", path: "/v1/documents", tag: "documents", summary: "List the tenant's documents (filter by source, status, q).", operationID: "documentList", auth: authScopeQuery,
			params: []Parameter{
				{Name: "source", In: "query", Description: "Filter by source id.", Schema: strSchema()},
				{Name: "status", In: "query", Description: "Filter by status (active, deleted).", Schema: strSchema()},
				{Name: "q", In: "query", Description: "Free-text match on title or external id.", Schema: strSchema()},
				{Name: "limit", In: "query", Description: "Page size (default 50, max 200).", Schema: map[string]any{"type": "integer"}},
				{Name: "cursor", In: "query", Description: "Opaque pagination cursor from a prior next_cursor.", Schema: strSchema()},
			},
			success: "a page of documents ({items, next_cursor})",
			extra:   []errResp{{"400", "invalid filter, limit or cursor"}, {"503", "tenant is not available"}}},
		{method: "GET", path: "/v1/documents/{id}", tag: "documents", summary: "Get one document with current version metadata (optional ?content=true).", operationID: "documentGet", auth: authScopeQuery,
			params: []Parameter{
				documentIDParam(),
				{Name: "content", In: "query", Description: "Include the current version's full text when true.", Schema: map[string]any{"type": "boolean"}},
			},
			success: "the document",
			extra:   []errResp{{"404", "no such document"}, {"503", "tenant is not available"}}},
		{method: "DELETE", path: "/v1/documents/{id}", tag: "documents", summary: "Soft-delete a document.", operationID: "documentDelete", auth: authScopeIngest,
			params:  []Parameter{documentIDParam()},
			success: "document deleted",
			extra:   []errResp{{"404", "no such document"}, {"503", "tenant is not available"}}},
		{method: "GET", path: "/v1/documents/{id}/chunks", tag: "documents", summary: "List a document's current-version chunks (debugging).", operationID: "documentChunks", auth: authScopeAdmin,
			params: []Parameter{
				documentIDParam(),
				{Name: "limit", In: "query", Description: "Page size (default 50, max 200).", Schema: map[string]any{"type": "integer"}},
				{Name: "cursor", In: "query", Description: "Opaque pagination cursor from a prior next_cursor.", Schema: strSchema()},
			},
			success: "a page of chunks ({items, next_cursor})",
			extra:   []errResp{{"404", "no such document"}, {"503", "tenant is not available"}}},

		// Jobs (STORY-04.5, FR-ADM-02). Control-plane tracking rows; tenant derived
		// from the API key (FR-ACC-03). All admin scope (SPEC-07 §2).
		{method: "GET", path: "/v1/jobs", tag: "jobs", summary: "List the tenant's jobs (filter by status, kind, source).", operationID: "jobList", auth: authScopeAdmin,
			params: []Parameter{
				{Name: "status", In: "query", Description: "Filter by status (queued, running, succeeded, failed, cancelled).", Schema: strSchema()},
				{Name: "kind", In: "query", Description: "Filter by job kind (e.g. sync_source, ingest_document).", Schema: strSchema()},
				{Name: "source", In: "query", Description: "Filter by source id.", Schema: strSchema()},
				{Name: "limit", In: "query", Description: "Page size (default 50, max 200).", Schema: map[string]any{"type": "integer"}},
				{Name: "cursor", In: "query", Description: "Opaque pagination cursor from a prior next_cursor.", Schema: strSchema()},
			},
			success: "a page of jobs ({items, next_cursor})",
			extra:   []errResp{{"400", "invalid filter, limit or cursor"}}},
		{method: "GET", path: "/v1/jobs/{id}", tag: "jobs", summary: "Get one job with status, timing and statistics.", operationID: "jobGet", auth: authScopeAdmin,
			params:  []Parameter{jobIDParam()},
			success: "the job",
			extra:   []errResp{{"404", "no such job"}}},
		{method: "POST", path: "/v1/jobs/{id}/cancel", tag: "jobs", summary: "Cancel a queued job now, or request cooperative cancellation of a running job (202; SPEC-08 §4).", operationID: "jobCancel", auth: authScopeAdmin,
			params:  []Parameter{jobIDParam()},
			success: "job cancelled", okStatus: "200",
			extra: []errResp{{"404", "no such job, or running-job cancellation is not available yet"}, {"409", "job is not in a cancellable state"}}},

		// Retrieve (STORY-08.2, FR-RET-08). Tenant content, tenant derived from the
		// API key (FR-ACC-03). `query` scope. Embeds the query with the tenant's
		// configured provider and returns ranked chunks — no generation (SPEC-06 §2).
		// When the tenant enables reranking (settings.reranker.enabled, STORY-08.3) the
		// results are reordered and `score` is the reranker relevance score (SPEC-06 §3).
		{method: "POST", path: "/v1/retrieve", tag: "retrieval", summary: "Hybrid retrieval: embed the query and return ranked chunks with scores and citation metadata (no generation). score is the fused RRF score, or the reranker score when the tenant enables reranking. Body: {query, top_k?, filters?}.", operationID: "retrieve", auth: authScopeQuery,
			reqSchema: "RetrieveRequest", okSchema: "RetrieveResponse",
			success: "ranked chunks",
			extra:   []errResp{{"400", "missing query or malformed body"}, {"503", "tenant is not available"}}},

		// Query (STORY-08.6, FR-RET-06, SPEC-06 §6). Grounded answering: retrieve +
		// answer, tenant derived from the API key (FR-ACC-03). `query` scope. Two
		// response modes selected by the body's `stream` flag: JSON (the documented
		// success body) or, when stream=true, a text/event-stream of retrieval
		// (citations first) → delta (text) → done (usage) events.
		{method: "POST", path: "/v1/query", tag: "retrieval", summary: "Answer a question grounded in the tenant's content, with [n] citations (SPEC-06 §6). Body: {question, filters?, history?, top_k?, stream?}. stream=false returns JSON {id, answer, grounded, citations[], usage, model}; stream=true returns a text/event-stream of retrieval (citations first), delta (text), done (usage) events. Below the grounding floor: grounded=false with a fixed refusal and no generation. If generation is unavailable the query degrades to retrieval-only rather than failing (NFR-REL-04).", operationID: "query", auth: authScopeQuery,
			reqSchema: "QueryRequest", okSchema: "QueryResponse",
			success: "grounded answer (JSON), or an SSE event stream when stream=true",
			extra:   []errResp{{"400", "missing question or malformed body"}, {"503", "tenant is not available"}}},

		// Query log + feedback (STORY-08.8, FR-RET-09/10). Tenant content, tenant
		// derived from the API key (FR-ACC-03). Feedback is `query` scope; the admin
		// query-log listing is `admin` scope (SPEC-07 §2/§2g).
		{method: "POST", path: "/v1/feedback", tag: "retrieval", summary: "Rate a prior answer (thumbs up/down with optional comment). Body: {query_id, rating, comment?} where rating is 1 (up) or -1 (down); the query_id is the id returned by POST /v1/query. Idempotent per query (last write wins).", operationID: "feedback", auth: authScopeQuery,
			reqSchema: "FeedbackRequest",
			success:   "feedback recorded",
			extra:     []errResp{{"400", "invalid rating, query_id or malformed body"}, {"404", "no such query"}, {"503", "tenant is not available"}}},
		{method: "GET", path: "/v1/queries", tag: "retrieval", summary: "List the tenant's query log (each query's retrieved chunk ids + scores, grounded flag, model, timings, token counts) with any joined user feedback (FR-RET-09/10). Newest first.", operationID: "queryList", auth: authScopeAdmin,
			params: []Parameter{
				{Name: "limit", In: "query", Description: "Page size (default 50, max 200).", Schema: map[string]any{"type": "integer"}},
				{Name: "cursor", In: "query", Description: "Opaque pagination cursor from a prior next_cursor.", Schema: strSchema()},
			},
			success: "a page of query-log entries ({items, next_cursor})",
			extra:   []errResp{{"400", "invalid limit or cursor"}, {"503", "tenant is not available"}}},
	}
}

// jobIDParam is the shared {id} path parameter for the job subresource routes.
func jobIDParam() Parameter {
	return Parameter{Name: "id", In: "path", Required: true, Description: "Job id.", Schema: strSchema()}
}

// tenantIDParam is the shared {id} path parameter for the admin tenant routes.
func tenantIDParam() Parameter {
	return Parameter{Name: "id", In: "path", Required: true, Description: "Tenant id.", Schema: strSchema()}
}

// documentIDParam is the shared {id} path parameter for the document subresource routes.
func documentIDParam() Parameter {
	return Parameter{Name: "id", In: "path", Required: true, Description: "Document id.", Schema: strSchema()}
}

// sourceIDParam is the shared {id} path parameter for the source subresource routes.
func sourceIDParam() Parameter {
	return Parameter{Name: "id", In: "path", Required: true, Description: "Source id.", Schema: strSchema()}
}

// Document builds the in-code OpenAPI 3.1 document describing the live surface.
func Document() *OpenAPI {
	doc := &OpenAPI{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:       "RAG platform public API",
			Version:     "v1",
			Description: "Multi-tenant company-knowledge RAG platform. Tenant is derived from the authenticated principal (FR-ACC-03); there is no tenant_id parameter on tenant-scoped routes (SPEC-07 §1).",
		},
		Servers: []Server{{URL: "/", Description: "Same-origin; base path /v1 (SPEC-07 §1)."}},
		Tags: []Tag{
			{Name: "operational", Description: "Health, readiness, metrics, and this spec."},
			{Name: "auth", Description: "Session and OIDC authentication."},
			{Name: "platform", Description: "Platform-admin surface under /admin (requires is_platform_admin)."},
			{Name: "usage", Description: "Tenant-scoped usage accounting."},
			{Name: "sources", Description: "Tenant content sources (create/update/delete, sync, test)."},
			{Name: "documents", Description: "Tenant documents: upload, list, get, delete, and chunk debugging."},
			{Name: "jobs", Description: "Tenant jobs: list, get, and cancel (the control-plane history/mirror view)."},
			{Name: "retrieval", Description: "Hybrid retrieval: ranked chunks with scores and citation metadata (no generation)."},
		},
		Paths: map[string]PathItem{},
		Components: Components{
			Schemas: integrationSchemas(),
			SecuritySchemes: map[string]SecurityScheme{
				"bearerAuth": {
					Type:         "http",
					Scheme:       "bearer",
					BearerFormat: "rk_<hexprefix>_<secret>",
					Description:  "API key: Authorization: Bearer rk_<prefix>_<secret> (SPEC-07 §2, ADR-0021). Tenant is derived from the key.",
				},
				"sessionCookie": {
					Type:        "apiKey",
					In:          "cookie",
					Name:        "session",
					Description: "Server-side session cookie (SPEC-09 §3). Mutations also require the X-CSRF-Token header.",
				},
			},
		},
	}

	for _, r := range liveRoutes() {
		op := &Operation{
			Summary:     r.summary,
			OperationID: r.operationID,
			Tags:        []string{r.tag},
			Security:    securityFor(r.auth),
			Parameters:  r.params,
			RequestBody: requestBodyFor(r),
			Responses:   responsesFor(r),
		}
		item := doc.Paths[r.path]
		switch r.method {
		case "GET":
			item.Get = op
		case "POST":
			item.Post = op
		case "PATCH":
			item.Patch = op
		case "DELETE":
			item.Delete = op
		}
		doc.Paths[r.path] = item
	}
	return doc
}

// HasOperation reports whether the document describes the given method on path.
func (o *OpenAPI) HasOperation(method, path string) bool {
	item, ok := o.Paths[path]
	if !ok {
		return false
	}
	switch strings.ToUpper(method) {
	case "GET":
		return item.Get != nil
	case "POST":
		return item.Post != nil
	case "PATCH":
		return item.Patch != nil
	case "DELETE":
		return item.Delete != nil
	}
	return false
}

// ErrorCodes returns the SPEC-07 §1 error-code vocabulary in a stable order. It is
// the single source the ErrorEnvelope schema's enum is built from, so the spec's
// documented codes cannot drift from the constants WriteError uses.
func ErrorCodes() []string {
	return []string{
		CodeUnauthorized,
		CodeForbidden,
		CodeNotFound,
		CodeValidation,
		CodeRateLimited,
		CodeTenantUnavailable,
		CodeConflict,
		CodeInternal,
	}
}

// errorEnvelopeSchema is the JSON Schema (OpenAPI 3.1 => JSON Schema 2020-12) for
// the SPEC-07 §1 error body, matching the errorEnvelope Go type exactly.
func errorEnvelopeSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"error"},
		"additionalProperties": false,
		"properties": map[string]any{
			"error": map[string]any{
				"type":                 "object",
				"required":             []string{"code", "message"},
				"additionalProperties": false,
				"properties": map[string]any{
					"code":       map[string]any{"type": "string", "enum": ErrorCodes()},
					"message":    map[string]any{"type": "string"},
					"request_id": map[string]any{"type": "string"},
				},
			},
		},
	}
}

func strSchema() map[string]any { return map[string]any{"type": "string"} }

// refSchema is a JSON Schema $ref to a component schema by name.
func refSchema(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// requestBodyFor builds the typed request body for a route, or nil when the route
// declares no request schema (its body, if any, stays described in the summary).
func requestBodyFor(r route) *RequestBody {
	if r.reqSchema == "" {
		return nil
	}
	media := r.reqBodyType
	if media == "" {
		media = "application/json"
	}
	return &RequestBody{
		Required: true,
		Content:  map[string]MediaType{media: {Schema: refSchema(r.reqSchema)}},
	}
}

// integrationSchemas is the component-schema set: the SPEC-07 §1 error envelope plus
// typed request/response bodies for the core integration endpoints (retrieve, query,
// feedback) and the objects they share (Filters, Chunk, Citation, Usage). These are
// what an external integrator generates a client from; they mirror the Go DTOs
// exactly (internal/retrieve, internal/answer, internal/querylog), and the contract
// test drives real responses so they cannot silently drift.
func integrationSchemas() map[string]any {
	obj := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "required": required, "properties": props}
	}
	str := map[string]any{"type": "string"}
	strArr := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	objectFree := map[string]any{"type": "object", "additionalProperties": true}

	filters := obj(nil, map[string]any{
		"source_ids": strArr,
		"uri_prefix": str,
		"date_from":  map[string]any{"type": "string", "format": "date-time"},
		"date_to":    map[string]any{"type": "string", "format": "date-time"},
		"metadata":   objectFree,
	})
	filters["description"] = "Optional retrieval filters (FR-RET-02). Any absent field is a no-op."

	return map[string]any{
		"ErrorEnvelope": errorEnvelopeSchema(),

		"Filters": filters,

		"RetrieveRequest": obj([]string{"query"}, map[string]any{
			"query":   map[string]any{"type": "string", "description": "The search query text."},
			"top_k":   map[string]any{"type": "integer", "description": "Max chunks to return; defaults to the tenant's retrieval.final_k."},
			"filters": refSchema("Filters"),
		}),
		"Chunk": obj([]string{"id", "document_id", "source_id", "content", "uri", "title", "heading_path", "score"}, map[string]any{
			"id":           str,
			"document_id":  str,
			"source_id":    str,
			"content":      str,
			"uri":          str,
			"title":        str,
			"heading_path": strArr,
			"metadata":     objectFree,
			"score":        map[string]any{"type": "number", "description": "Fused RRF score, or the reranker relevance score when reranking is enabled."},
		}),
		"RetrieveResponse": obj([]string{"chunks"}, map[string]any{
			"chunks": map[string]any{"type": "array", "items": refSchema("Chunk")},
		}),

		"HistoryTurn": obj([]string{"role", "content"}, map[string]any{
			"role":    map[string]any{"type": "string", "enum": []string{"user", "assistant"}},
			"content": str,
		}),
		"QueryRequest": obj([]string{"question"}, map[string]any{
			"question": map[string]any{"type": "string", "description": "The natural-language question."},
			"filters":  refSchema("Filters"),
			"history":  map[string]any{"type": "array", "items": refSchema("HistoryTurn"), "description": "Prior turns for a follow-up (used for the optional rewrite step)."},
			"top_k":    integer,
			"stream":   map[string]any{"type": "boolean", "description": "When true the response is a text/event-stream (retrieval -> delta -> done) instead of JSON."},
		}),
		"Citation": obj([]string{"n", "document_id", "title", "uri", "heading_path", "snippet"}, map[string]any{
			"n":            map[string]any{"type": "integer", "description": "The [n] marker used in the answer text."},
			"document_id":  str,
			"title":        str,
			"uri":          str,
			"heading_path": strArr,
			"snippet":      str,
		}),
		"Usage": obj([]string{"retrieval_ms", "generation_ms", "in_tokens", "out_tokens"}, map[string]any{
			"retrieval_ms":  integer,
			"generation_ms": integer,
			"in_tokens":     integer,
			"out_tokens":    integer,
		}),
		"QueryResponse": obj([]string{"id", "answer", "grounded", "citations", "usage", "model"}, map[string]any{
			"id":        str,
			"answer":    map[string]any{"type": "string", "description": "The grounded answer with [n] citation markers, or the fixed refusal when grounded is false."},
			"grounded":  boolean,
			"citations": map[string]any{"type": "array", "items": refSchema("Citation")},
			"usage":     refSchema("Usage"),
			"model":     str,
		}),

		"FeedbackRequest": obj([]string{"query_id", "rating"}, map[string]any{
			"query_id": map[string]any{"type": "string", "description": "The id returned by POST /v1/query."},
			"rating":   map[string]any{"type": "integer", "enum": []int{1, -1}, "description": "1 = thumbs up, -1 = thumbs down."},
			"comment":  map[string]any{"type": "string", "description": "Optional free-text comment."},
		}),
	}
}

// securityFor maps a route's auth to its OpenAPI security requirement. An open
// route returns nil (no requirement); there is no global security object.
func securityFor(a auth) []map[string][]string {
	switch {
	case isAPIKeyScope(a):
		return []map[string][]string{{"bearerAuth": {}}}
	case a == authSession, a == authPlatformAdmin:
		return []map[string][]string{{"sessionCookie": {}}}
	default:
		return nil
	}
}

// responsesFor builds the documented responses for a route: its success code plus
// the error responses its middleware chain can produce, all referencing
// ErrorEnvelope so every error the client sees has one documented shape.
func responsesFor(r route) map[string]Response {
	ok := r.okStatus
	if ok == "" {
		ok = "200"
	}
	okResp := Response{Description: r.success}
	if r.okSchema != "" {
		okResp.Content = map[string]MediaType{"application/json": {Schema: refSchema(r.okSchema)}}
	}
	resp := map[string]Response{
		ok: okResp,
	}
	errRef := map[string]MediaType{
		"application/json": {Schema: map[string]any{"$ref": "#/components/schemas/ErrorEnvelope"}},
	}
	add := func(code, desc string) { resp[code] = Response{Description: desc, Content: errRef} }

	if isAPIKeyScope(r.auth) {
		add("401", "missing or invalid API key")
		add("403", "API key lacks the required scope")
		add("429", "rate limit exceeded")
	}
	switch r.auth {
	case authPlatformAdmin:
		add("401", "no session")
		add("403", "not a platform admin")
	case authSession:
		add("401", "no session")
	}
	// readyz can report not-ready.
	if r.operationID == "readyz" {
		add("503", "a readiness check failed")
	}
	// Per-route extras (404/409/400 for resource routes).
	for _, e := range r.extra {
		add(e.status, e.desc)
	}
	return resp
}

// MarshalOpenAPIJSON renders the document as indented JSON (the /v1/openapi.json
// body).
func MarshalOpenAPIJSON() ([]byte, error) {
	return json.MarshalIndent(Document(), "", "  ")
}

// MarshalOpenAPIYAML renders the document as YAML for the checked-in
// api/openapi.yaml. It is the exact bytes the drift guard compares against, so
// `mise run openapi` writes precisely this.
func MarshalOpenAPIYAML() ([]byte, error) {
	var buf strings.Builder
	buf.WriteString("# Generated by `mise run openapi` (ragctl openapi) from internal/api/openapi.go.\n")
	buf.WriteString("# Do not edit by hand — the source of truth is the Go code (SPEC-07 §3, ADR-0028).\n")
	enc := yaml.NewEncoder(nopWriter{&buf})
	enc.SetIndent(2)
	if err := enc.Encode(Document()); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// nopWriter adapts a strings.Builder to io.Writer for the yaml encoder.
type nopWriter struct{ b *strings.Builder }

func (w nopWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

// OpenAPIHandler serves the document as JSON at /v1/openapi.json. It is open (no
// auth): the API description is public and drives client generation.
func OpenAPIHandler() http.Handler {
	body, err := MarshalOpenAPIJSON()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			WriteError(w, r, http.StatusInternalServerError, CodeInternal, "openapi document unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}
