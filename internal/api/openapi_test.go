package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The live surface STORY-04.2 documents: the routes the STORY-04.1 router
// actually mounts today, plus the spec endpoint itself. 04.3–04.6 append here as
// their handlers land.
var wantDocumentedRoutes = []struct{ method, path string }{
	{http.MethodGet, "/healthz"},
	{http.MethodGet, "/readyz"},
	{http.MethodGet, "/metrics"},
	{http.MethodPost, "/v1/auth/signup"},
	{http.MethodPost, "/v1/auth/login"},
	{http.MethodPost, "/v1/auth/logout"},
	{http.MethodGet, "/v1/auth/oidc/start"},
	{http.MethodGet, "/v1/auth/oidc/callback"},
	{http.MethodGet, "/admin/audit"},
	{http.MethodPost, "/admin/impersonations"},
	{http.MethodDelete, "/admin/impersonations/{id}"},
	{http.MethodGet, "/v1/usage"},
	{http.MethodGet, "/v1/openapi.json"},
}

// TestDocumentDescribesLiveRoutes: the in-code OpenAPI document describes every
// live route (method + path). A route the router mounts but the spec omits would
// let the spec lie, so the contract test below cross-checks against the router.
func TestDocumentDescribesLiveRoutes(t *testing.T) {
	doc := Document()
	for _, r := range wantDocumentedRoutes {
		if !doc.HasOperation(r.method, r.path) {
			t.Errorf("OpenAPI document is missing %s %s", r.method, r.path)
		}
	}
}

// TestOpenAPIVersionIs31: the document declares OpenAPI 3.1 (its schemas are then
// JSON Schema 2020-12, which the contract test compiles directly).
func TestOpenAPIVersionIs31(t *testing.T) {
	if got := Document().OpenAPI; got != "3.1.0" {
		t.Fatalf("openapi version = %q, want 3.1.0", got)
	}
}

// TestErrorEnvelopeSchemaEnumMatchesCodes: the ErrorEnvelope schema's code enum
// is exactly the SPEC-07 §1 code vocabulary encoded in the Go constants, so the
// spec cannot drift from the codes the router actually emits.
func TestErrorEnvelopeSchemaEnumMatchesCodes(t *testing.T) {
	schema := Document().Components.Schemas["ErrorEnvelope"]
	if schema == nil {
		t.Fatal("ErrorEnvelope schema missing from components")
	}
	enum := errorCodeEnumFromSchema(t, schema)

	want := ErrorCodes()
	if !reflect.DeepEqual(enum, want) {
		t.Fatalf("ErrorEnvelope code enum = %v, want %v", enum, want)
	}
}

// TestOpenAPIHandlerServesJSON: the handler returns the document as JSON with the
// right content type, and it round-trips to the same document the code builds.
func TestOpenAPIHandlerServesJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	OpenAPIHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/openapi.json", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("openapi.json = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var served map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &served); err != nil {
		t.Fatalf("served body is not JSON: %v", err)
	}
	if served["openapi"] != "3.1.0" {
		t.Fatalf("served openapi = %v, want 3.1.0", served["openapi"])
	}
	if _, ok := served["paths"]; !ok {
		t.Fatal("served document has no paths")
	}
}

// TestOpenAPIMountedOpenNoAuth: the spec is served at /v1/openapi.json through the
// assembled router without any authentication (it is a public description).
func TestOpenAPIMountedOpenNoAuth(t *testing.T) {
	var ran []string
	h := New(newTestDeps(&ran))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/openapi.json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("openapi.json through router = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	for _, r := range ran {
		if r == "session" || r == "platform-admin" || r == "scope-admin" || r == "rate-limit" {
			t.Fatalf("openapi.json ran auth middleware %q; must be open", r)
		}
	}
}

// TestYAMLDriftGuard: the checked-in api/openapi.yaml must equal what the code
// generates, so a code change that is not regenerated (`mise run openapi`) fails
// CI. This is the "generated from code" enforcement (ADR-0028, SPEC-07 §3).
func TestYAMLDriftGuard(t *testing.T) {
	generated, err := MarshalOpenAPIYAML()
	if err != nil {
		t.Fatalf("MarshalOpenAPIYAML: %v", err)
	}
	onDisk := repoFile(t, "api/openapi.yaml")
	if !bytes.Equal(generated, onDisk) {
		t.Fatalf("api/openapi.yaml is stale: regenerate with `mise run openapi`.\n"+
			"generated %d bytes, on disk %d bytes", len(generated), len(onDisk))
	}
}

// TestRecordedErrorResponsesConformToSpec is the contract test: real error
// responses produced by the assembled router validate against the ErrorEnvelope
// schema declared in the served spec (SPEC-07 §3, "validate responses against
// it"). A negative control proves the schema actually rejects a wrong shape.
func TestRecordedErrorResponsesConformToSpec(t *testing.T) {
	schema := compileErrorEnvelopeSchema(t)

	// A router whose scope gate blocks, so /v1/usage yields a real 401 envelope,
	// and whose unknown routes yield the 404 envelope — both from the router itself.
	var ran []string
	deps := newTestDeps(&ran)
	deps.RequireScopeAdmin = stubMW(&ran, "scope-admin", http.StatusUnauthorized, CodeUnauthorized)
	h := New(deps)

	// Record real error responses from the router across several codes.
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"not_found", httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil)},
		{"unauthorized", httptest.NewRequest(http.MethodGet, "/v1/usage", nil)},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, tc.req)
		var body any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body not JSON: %v", tc.name, err)
		}
		if err := schema.Validate(body); err != nil {
			t.Fatalf("%s response does not conform to ErrorEnvelope schema: %v\nbody=%s", tc.name, err, rr.Body.String())
		}
	}

	// Negative control: a wrong shape must be rejected, proving the check has teeth.
	var bad any
	_ = json.Unmarshal([]byte(`{"error":"a bare string"}`), &bad)
	if err := schema.Validate(bad); err == nil {
		t.Fatal("ErrorEnvelope schema accepted a bare-string error body; contract has no teeth")
	}
}

// compileErrorEnvelopeSchema extracts the ErrorEnvelope schema from the served
// spec and compiles it with jsonschema/v6 (the same library the settings package
// uses), so the contract test validates against exactly what the API publishes.
func compileErrorEnvelopeSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := json.Marshal(Document().Components.Schemas["ErrorEnvelope"])
	if err != nil {
		t.Fatalf("marshal ErrorEnvelope: %v", err)
	}
	docAny, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unmarshal ErrorEnvelope schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	const url = "https://rag-platform/schemas/error-envelope.json"
	if err := c.AddResource(url, docAny); err != nil {
		t.Fatalf("add ErrorEnvelope resource: %v", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile ErrorEnvelope schema: %v", err)
	}
	return sch
}

// errorCodeEnumFromSchema digs out components.schemas.ErrorEnvelope
// .properties.error.properties.code.enum as a []string.
func errorCodeEnumFromSchema(t *testing.T, schema any) []string {
	t.Helper()
	m, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("ErrorEnvelope schema is %T, want map", schema)
	}
	props := mapAt(t, m, "properties")
	errProp := mapAt(t, props, "error")
	errProps := mapAt(t, errProp, "properties")
	codeProp := mapAt(t, errProps, "code")
	rawEnum, ok := codeProp["enum"].([]string)
	if !ok {
		// tolerate []any
		anyEnum, ok := codeProp["enum"].([]any)
		if !ok {
			t.Fatalf("code.enum is %T, want []string or []any", codeProp["enum"])
		}
		out := make([]string, len(anyEnum))
		for i, v := range anyEnum {
			out[i] = v.(string)
		}
		return out
	}
	return rawEnum
}

func mapAt(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("expected map at %q, got %T", key, m[key])
	}
	return v
}

// repoFile reads a file relative to the repository root regardless of the working
// directory `go test` ran from (mirrors internal/migrate's drift-guard helper).
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}
