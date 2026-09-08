//go:build e2e

// STORY-06.1 golden path: the connector framework (interface + registry + config
// validation) wired into the REAL public HTTP router (internal/api) over a real
// net/http listener against the REAL control-plane Postgres (up via `mise run up`),
// no mocks. It drives the sources API through the assembled tenant-scoped chain
// (API-key admin scope -> rate limit -> handler) with a connector registered in a
// fresh registry, proving the ValidateConfig/Test seam is wired end to end:
//   - a source whose kind HAS a registered connector runs that connector's
//     kind-specific JSON-Schema ValidateConfig (a bad config -> 400 validation),
//     and its "test connection" (/test -> the connector's Test result), so adding a
//     connector lights up validation with no change to the sources package
//     (NFR-MNT-01, FR-SRC-13/14);
//   - a source whose kind has NO registered connector defers config validation
//     (create still succeeds) and /test reports the not_found seam envelope
//     (connector.ErrUnsupportedKind -> sources.ErrConnectorUnavailable), the
//     established behaviour before that connector exists.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/sources"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/obs"
)

// e2eCrawlSchema requires a non-empty start_urls array and forbids unknown keys —
// enough to prove the connector's ValidateConfig has teeth through the HTTP chain.
const e2eCrawlSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["start_urls"],
  "properties": {
    "start_urls": {"type": "array", "minItems": 1, "items": {"type": "string"}},
    "max_depth": {"type": "integer", "minimum": 0}
  }
}`

// e2eFakeCrawl is a full connector.Connector used only by this e2e to register a
// web_crawl kind. ValidateConfig checks the schema; Test records that it ran and
// returns nil; Sync is unused here (EPIC-07).
type e2eFakeCrawl struct {
	schema    *connector.SchemaValidator
	testCalls *int
}

func (c e2eFakeCrawl) Kind() connector.Kind { return connector.KindWebCrawl }

func (c e2eFakeCrawl) ValidateConfig(cfg json.RawMessage) error { return c.schema.Validate(cfg) }

func (c e2eFakeCrawl) Test(_ context.Context, _ json.RawMessage, _ connector.Credentials) error {
	*c.testCalls++
	return nil
}

func (c e2eFakeCrawl) Sync(_ context.Context, _ connector.SyncRun, _ connector.Sink) (connector.Stats, error) {
	return connector.Stats{}, nil
}

func (c e2eFakeCrawl) Fields() []connector.FieldSpec { return nil }

func TestConnectorFrameworkGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "connector-" + suffix

	// --- Seed a tenant and an admin-scoped API key that owns the sources. ---
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		slug, "Connector Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	keySvc := auth.NewAPIKeyService(auth.MembershipFromPool(pool))
	_, secret, err := keySvc.Create(ctx, auth.CreateKeyParams{TenantID: tenantID, Name: "admin-token", Scopes: []string{"admin"}})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// --- A fresh registry with a fake web_crawl connector wired into the sources
	// service via the SourcesValidator seam (exactly how buildAPIServer wires the
	// default registry, but isolated to this test). ---
	testCalls := 0
	reg := connector.NewRegistry()
	reg.Register(connector.KindWebCrawl, func() connector.Connector {
		return e2eFakeCrawl{schema: connector.MustSchemaValidator([]byte(e2eCrawlSchema)), testCalls: &testCalls}
	})
	sourcesSvc := sources.NewService(sources.FromPool(pool))
	sourcesSvc.Validator = connector.NewSourcesValidator(reg, sources.ErrConnectorUnavailable)

	// --- Build the SAME router serve builds, from the real control-plane pool. ---
	verifier := auth.NewAPIKeyVerifier(auth.FromPool(pool))
	limiter := ratelimit.New(nil)
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	rl := &ratelimit.Middleware{
		Limiter:     limiter,
		Limit:       ratelimit.LimitFromSettings(settingsSvc, 1000),
		Burst:       1000,
		TenantBurst: 1000,
	}
	sh := sources.NewHandlers(sourcesSvc)
	deps := api.Deps{
		Log:               obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:           obs.NewMetrics(),
		RequireScopeAdmin: verifier.RequireScope(auth.ScopeAdmin),
		RateLimit:         rl.Handler,
		SourceList:        http.HandlerFunc(sh.List),
		SourceCreate:      http.HandlerFunc(sh.Create),
		SourceGet:         http.HandlerFunc(sh.Get),
		SourceUpdate:      http.HandlerFunc(sh.Update),
		SourceDelete:      http.HandlerFunc(sh.Delete),
		SourceSync:        http.HandlerFunc(sh.Sync),
		SourceTest:        http.HandlerFunc(sh.Test),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()

	bearer := "Bearer " + secret
	call := func(method, path, body string) (int, []byte) {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, r)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", bearer)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	// --- A web_crawl config that VIOLATES the connector schema is rejected 400.
	// This proves the kind-specific ValidateConfig runs through the real chain. ---
	badBody := `{"kind":"web_crawl","name":"bad-` + suffix + `","config":{"max_depth":2}}`
	if code, body := call(http.MethodPost, "/v1/sources", badBody); code != http.StatusBadRequest {
		t.Fatalf("create with invalid connector config = %d, want 400; body=%s", code, body)
	}

	// --- A schema-valid web_crawl config is created (201) and persists. ---
	goodBody := `{"kind":"web_crawl","name":"good-` + suffix + `","config":{"start_urls":["https://docs.example/"],"max_depth":2}}`
	code, body := call(http.MethodPost, "/v1/sources", goodBody)
	if code != http.StatusCreated {
		t.Fatalf("create with valid connector config = %d, want 201; body=%s", code, body)
	}
	var created sources.Source
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.ID == "" || created.Kind != "web_crawl" {
		t.Fatalf("unexpected created source: %+v", created)
	}

	// --- /test runs the registered connector's Test (200) and it was called. ---
	if code, body := call(http.MethodPost, "/v1/sources/"+created.ID+"/test", ""); code != http.StatusOK {
		t.Fatalf("test = %d, want 200; body=%s", code, body)
	}
	if testCalls != 1 {
		t.Fatalf("connector.Test called %d times, want 1", testCalls)
	}

	// --- A kind with NO registered connector (api) defers validation: create
	// succeeds even with an arbitrary config, and /test reports the seam 404. ---
	apiBody := `{"kind":"api","name":"api-` + suffix + `","config":{"whatever":true}}`
	code, body = call(http.MethodPost, "/v1/sources", apiBody)
	if code != http.StatusCreated {
		t.Fatalf("create unregistered-kind source = %d, want 201; body=%s", code, body)
	}
	var apiSrc sources.Source
	if err := json.Unmarshal(body, &apiSrc); err != nil {
		t.Fatalf("decode api create: %v", err)
	}
	if code, _ := call(http.MethodPost, "/v1/sources/"+apiSrc.ID+"/test", ""); code != http.StatusNotFound {
		t.Fatalf("test unregistered kind = %d, want 404 seam", code)
	}
}
