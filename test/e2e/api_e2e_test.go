//go:build e2e

// STORY-07.7 golden path: the HTTP API connector (internal/connector/api) mapping a
// REAL in-process JSON API (httptest) into connector.Documents via text/template and
// persisting its incremental cursor to a REAL enrolled tenant database, reached ONLY
// through a resolver + *tenant.DB (ADR-0003, C-3) via the tenant connector_state
// table (added by tenant migration 00002). What is exercised for real is:
//   - template rendering (body), uri_template (URI) and metadata JSONPath extraction
//     over a live paginated response,
//   - the incremental cursor round trip: a first run stores the max updated_at into
//     connector_state; a second incremental run reads it back, sends updated_since,
//     and only sees the newer record; the cursor then advances.
//
// The egress is the httptest loopback server (the SSRF guard is unit-tested in
// internal/egress); the ingestion Sink is a recording connector.Sink. Assertions use
// the pgxpool / in-process server directly, never `docker compose exec`; cleanup
// drops the tenant DB over the control pool with FORCE.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/connector"
	connapi "github.com/rag-platform/ragctl/internal/connector/api"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// apiE2ESink records emitted documents by ExternalID for an API-connector e2e.
type apiE2ESink struct {
	mu   sync.Mutex
	docs map[string]connector.Document
}

func newAPIE2ESink() *apiE2ESink { return &apiE2ESink{docs: map[string]connector.Document{}} }

func (s *apiE2ESink) Put(_ context.Context, doc connector.Document) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.docs[doc.ExternalID]
	s.docs[doc.ExternalID] = doc
	return !existed, nil
}

func (s *apiE2ESink) Complete(_ context.Context) error { return nil }

func (s *apiE2ESink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.docs) }

func TestAPIConnectorIncrementalCursorPersists(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "apiconn-" + suffix
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		var dbName, role string
		_ = pool.QueryRow(cctx,
			`select d.database_name, d.username from tenant_databases d join tenants t on t.id = d.tenant_id where t.slug = $1`,
			slug).Scan(&dbName, &role)
		if dbName != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		}
		if role != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
		}
		_, _ = pool.Exec(cctx, `DELETE FROM tenants WHERE slug = $1`, slug)
	})

	const dim = 768
	if out, exit := runEnroll(t, ageKey, blob, slug, "APIConn "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})
	db, err := resolver.Open(ctx, tenant.ID(uuid.MustParse(tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}

	// --- The API: a single-page endpoint whose items carry updated_at, filtered by
	// the updated_since query param. It records the last updated_since it received. ---
	items := []map[string]any{
		{"id": "p1", "slug": "widget", "name": "Widget", "category": map[string]any{"name": "Tools"}, "updated_at": "2026-01-01T00:00:00Z"},
		{"id": "p2", "slug": "gadget", "name": "Gadget", "category": map[string]any{"name": "Tools"}, "updated_at": "2026-01-02T00:00:00Z"},
	}
	var mu sync.Mutex
	var lastSince string
	var sinceSeen bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/products", func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("updated_since")
		mu.Lock()
		lastSince = since
		sinceSeen = r.URL.Query().Has("updated_since")
		snapshot := append([]map[string]any(nil), items...)
		mu.Unlock()
		out := make([]map[string]any, 0, len(snapshot))
		for _, it := range snapshot {
			if since == "" || it["updated_at"].(string) > since {
				out = append(out, it)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The connector's default egress is the SSRF guard (blocks loopback); inject the
	// server's permissive client so the e2e reaches it.
	connapi.SetEgressClientForTest(srv.Client())

	sourceID := uuid.New()
	store := connapi.NewTenantStateStore(db, sourceID)

	cfg := json.RawMessage(fmt.Sprintf(`{
		"base_url":%q,
		"auth":{"type":"bearer"},
		"endpoints":[{
			"name":"products","path":"/v1/products","method":"GET",
			"pagination":{"type":"none"},
			"items_path":"$.data","id_path":"$.id","updated_path":"$.updated_at",
			"incremental_param":"updated_since",
			"template":"# {{.name}}\nSlug: {{.slug}}",
			"uri_template":"https://acme.example/p/{{.slug}}",
			"metadata":{"category":"$.category.name"}
		}]
	}`, srv.URL))
	creds := connector.Credentials{"token": "secret-bearer-token"}
	newRun := func(full bool) connector.SyncRun {
		return connector.SyncRun{SourceID: sourceID, Config: cfg, Creds: creds, State: store, Full: full, Log: nil}
	}

	cursorRow := func() (string, bool) {
		var v string
		err := db.QueryRow(ctx,
			`select value from connector_state where source_id = $1 and key = 'api:cursor:products'`,
			sourceID).Scan(&v)
		if err != nil {
			return "", false
		}
		return v, true
	}

	// --- Run 1: incremental, no cursor yet -> enumerate all, map, store cursor. ---
	sink1 := newAPIE2ESink()
	if _, err := connapi.New().Sync(ctx, newRun(false), sink1); err != nil {
		t.Fatalf("run1 Sync: %v", err)
	}
	if sinceSeen {
		t.Fatalf("run1 sent updated_since=%q; the first incremental run must enumerate all", lastSince)
	}
	if sink1.count() != 2 {
		t.Fatalf("run1 emitted %d docs, want 2", sink1.count())
	}
	// Template mapping landed: body, uri, metadata.
	sink1.mu.Lock()
	d := sink1.docs["products/p1"]
	sink1.mu.Unlock()
	if d.Text != "# Widget\nSlug: widget" {
		t.Fatalf("run1 body = %q", d.Text)
	}
	if d.URI != "https://acme.example/p/widget" {
		t.Fatalf("run1 uri = %q", d.URI)
	}
	if got := d.Metadata["category"]; got != "Tools" {
		t.Fatalf("run1 metadata category = %v, want Tools", got)
	}
	if v, ok := cursorRow(); !ok || v != "2026-01-02T00:00:00Z" {
		t.Fatalf("run1 connector_state cursor = %q (ok=%v), want the max updated_at", v, ok)
	}

	// --- Run 2: incremental with the stored cursor -> only the newer item. ---
	mu.Lock()
	items = append(items, map[string]any{
		"id": "p3", "slug": "sprocket", "name": "Sprocket",
		"category": map[string]any{"name": "Parts"}, "updated_at": "2026-01-03T00:00:00Z",
	})
	mu.Unlock()

	sink2 := newAPIE2ESink()
	if _, err := connapi.New().Sync(ctx, newRun(false), sink2); err != nil {
		t.Fatalf("run2 Sync: %v", err)
	}
	if !sinceSeen || lastSince != "2026-01-02T00:00:00Z" {
		t.Fatalf("run2 updated_since = %q (seen=%v), want the stored cursor", lastSince, sinceSeen)
	}
	if sink2.count() != 1 {
		t.Fatalf("run2 emitted %d docs, want 1 (only the newer item)", sink2.count())
	}
	if _, ok := sink2.docs["products/p3"]; !ok {
		t.Fatalf("run2 did not emit the newer item; got %v", sink2.docs)
	}
	if v, ok := cursorRow(); !ok || v != "2026-01-03T00:00:00Z" {
		t.Fatalf("run2 cursor = %q (ok=%v), want it advanced to the new max", v, ok)
	}
}
