//go:build e2e

// STORY-04.2 golden path: the OpenAPI spec is served by the REAL public router
// over a real net/http listener against the REAL control-plane Postgres (up via
// `mise run up`), and real recorded error responses are validated against the
// schema the API itself publishes at /v1/openapi.json — the SPEC-07 §3 contract
// ("contract tests in CI validate responses against it"), end to end, no mocks:
//   - GET /v1/openapi.json is open (no auth) and returns a 3.1 document with the
//     live paths and the ErrorEnvelope schema,
//   - a real 401 (anon /admin/audit) and a real 404 (unknown route), both
//     produced by the assembled chain, conform to the ErrorEnvelope schema
//     extracted from the served spec.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestOpenAPIContractGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	// Build the same router serve builds, from the real control-plane pool, so the
	// error responses are produced by the real middleware chain (SPEC-07 §1).
	authSvc := auth.NewService(auth.FromPool(pool))
	authHandlers := &auth.Handlers{Service: authSvc, Secure: false}
	authz := auth.NewAuthzService(auth.MembershipFromPool(pool))
	auditHandlers := audit.NewHandlers(audit.NewService(audit.FromPool(pool)))

	deps := api.Deps{
		Log:                  obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:              obs.NewMetrics(),
		RequireSession:       authHandlers.RequireSession,
		RequirePlatformAdmin: authz.RequirePlatformAdmin(),
		AuditList:            http.HandlerFunc(auditHandlers.List),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()
	client := srv.Client()

	// --- The spec is served, open, at /v1/openapi.json. ---
	resp, err := client.Get(srv.URL + "/v1/openapi.json")
	if err != nil {
		t.Fatalf("GET /v1/openapi.json: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("openapi.json = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("openapi.json content-type = %q, want application/json", ct)
	}
	specBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}

	var spec map[string]any
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("spec is not JSON: %v", err)
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("served openapi = %v, want 3.1.0", spec["openapi"])
	}
	paths, _ := spec["paths"].(map[string]any)
	for _, p := range []string{"/admin/audit", "/v1/usage", "/v1/openapi.json"} {
		if _, ok := paths[p]; !ok {
			t.Fatalf("served spec is missing documented path %q", p)
		}
	}

	// --- Compile the ErrorEnvelope schema straight from the SERVED spec. ---
	envSchema := compileEnvelopeFromServedSpec(t, spec)

	// --- Real recorded error responses conform to that schema. ---
	cases := []struct {
		name string
		url  string
		want int
	}{
		{"unauthorized", srv.URL + "/admin/audit?tenant=" + uuid.NewString(), http.StatusUnauthorized},
		{"not_found", srv.URL + "/v1/does-not-exist", http.StatusNotFound},
	}
	for _, tc := range cases {
		r, err := client.Get(tc.url)
		if err != nil {
			t.Fatalf("%s: GET: %v", tc.name, err)
		}
		if r.StatusCode != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, r.StatusCode, tc.want)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		var body any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: body not JSON: %v; body=%s", tc.name, err, raw)
		}
		if err := envSchema.Validate(body); err != nil {
			t.Fatalf("%s response violates the published ErrorEnvelope schema: %v\nbody=%s", tc.name, err, raw)
		}
	}

	// Negative control: the published schema actually rejects a wrong shape.
	var bad any
	_ = json.Unmarshal([]byte(`{"error":"bare string"}`), &bad)
	if err := envSchema.Validate(bad); err == nil {
		t.Fatal("published ErrorEnvelope schema accepted a bare-string body; contract has no teeth")
	}
}

// compileEnvelopeFromServedSpec digs components.schemas.ErrorEnvelope out of the
// served spec document and compiles it with jsonschema/v6 so responses are
// validated against exactly what the API publishes (not a private copy).
func compileEnvelopeFromServedSpec(t *testing.T, spec map[string]any) *jsonschema.Schema {
	t.Helper()
	comps, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("served spec has no components")
	}
	schemas, ok := comps["schemas"].(map[string]any)
	if !ok {
		t.Fatal("served spec has no components.schemas")
	}
	env, ok := schemas["ErrorEnvelope"]
	if !ok {
		t.Fatal("served spec has no ErrorEnvelope schema")
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal ErrorEnvelope: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unmarshal ErrorEnvelope: %v", err)
	}
	c := jsonschema.NewCompiler()
	const url = "https://rag-platform/schemas/served-error-envelope.json"
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return sch
}
