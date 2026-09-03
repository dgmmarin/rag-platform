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
	Responses   map[string]Response   `json:"responses" yaml:"responses"`
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
)

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
			extra:   []errResp{{"404", "no such source, or the connector framework is not available yet"}}},
	}
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
		},
		Paths: map[string]PathItem{},
		Components: Components{
			Schemas: map[string]any{
				"ErrorEnvelope": errorEnvelopeSchema(),
			},
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

// securityFor maps a route's auth to its OpenAPI security requirement. An open
// route returns nil (no requirement); there is no global security object.
func securityFor(a auth) []map[string][]string {
	switch a {
	case authScopeAdmin:
		return []map[string][]string{{"bearerAuth": {}}}
	case authSession, authPlatformAdmin:
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
	resp := map[string]Response{
		ok: {Description: r.success},
	}
	errRef := map[string]MediaType{
		"application/json": {Schema: map[string]any{"$ref": "#/components/schemas/ErrorEnvelope"}},
	}
	add := func(code, desc string) { resp[code] = Response{Description: desc, Content: errRef} }

	switch r.auth {
	case authScopeAdmin:
		add("401", "missing or invalid API key")
		add("403", "API key lacks the required scope")
		add("429", "rate limit exceeded")
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
