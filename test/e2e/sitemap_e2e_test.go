//go:build e2e

// STORY-07.5 golden path: the sitemap connector (internal/connector/webcrawl,
// KindSitemap) enumerating a REAL in-process website (httptest) whose frontier is a
// sitemap INDEX → child sitemap → pages, persisting crawl state to a REAL enrolled
// tenant database (up via `mise run up`), reached ONLY through a resolver + *tenant.DB
// (ADR-0003, C-3) via the tenant crawl_pages table. What is exercised for real:
//   - sitemap-index + child-sitemap parsing seeds the frontier; every listed page is
//     emitted as a connector.Document and recorded fetched in crawl_pages,
//   - NO link following: a page's on-page <a> link to /trap is never fetched,
//   - lastmod INCREMENTAL skip against the REAL last_fetched_at: a second (incremental)
//     sync whose sitemap <lastmod> is older than the recorded fetch time re-fetches
//     nothing (cheaper than even a conditional GET) and emits nothing.
//
// The egress is the httptest loopback server (SSRF hardening is STORY-07.2); the sink is
// a recording connector.Sink. Assertions use the pgxpool / in-process server directly,
// never `docker compose exec`; cleanup drops the tenant DB over the control pool.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/connector/webcrawl"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestSitemapSyncAndLastmodIncremental(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "sitemap-" + suffix
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "Sitemap "+suffix, dim); exit != 0 {
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

	// --- The website: a sitemap index → one child sitemap listing three pages, each
	// carrying an old <lastmod>. Page /a links to /trap to prove no link following. ---
	var hits, trapHits int32
	pageHit := func() int32 { return atomic.LoadInt32(&hits) }
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sm := func(inner string) string {
		return `<?xml version="1.0" encoding="UTF-8"?>` + inner
	}
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, sm(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`+
			`<sitemap><loc>`+srv.URL+`/sitemap-a.xml</loc></sitemap></sitemapindex>`))
	})
	mux.HandleFunc("/sitemap-a.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, sm(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`+
			`<url><loc>`+srv.URL+`/a</loc><lastmod>2020-01-01</lastmod></url>`+
			`<url><loc>`+srv.URL+`/b</loc><lastmod>2020-01-01</lastmod></url>`+
			`<url><loc>`+srv.URL+`/c</loc><lastmod>2020-01-01</lastmod></url></urlset>`))
	})
	for _, p := range []string{"/a", "/b", "/c"} {
		body := "<html><head><title>Page " + p + "</title></head><body>content " + p + "</body></html>"
		if p == "/a" {
			body = `<html><head><title>Page a</title></head><body><a href="/trap">trap</a>content a</body></html>`
		}
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = io.WriteString(w, body)
		})
	}
	mux.HandleFunc("/trap", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&trapHits, 1)
		_, _ = io.WriteString(w, "trap")
	})

	// Sync's egress default is the SSRF guard (STORY-07.2), which blocks loopback.
	// Inject the server's permissive client so the e2e reaches it.
	webcrawl.SetEgressDoerForTest(srv.Client())

	sourceID := uuid.New()
	store := webcrawl.NewTenantPageStore(db, sourceID)
	cfg := json.RawMessage(fmt.Sprintf(`{"sitemap_urls":["%s/sitemap_index.xml"],"concurrency":2}`, srv.URL))
	run := func(full bool) connector.SyncRun {
		return connector.SyncRun{SourceID: sourceID, Config: cfg, State: store, Full: full, Log: nil}
	}
	fetchedRows := func() int {
		return tenantIntDB(ctx, t, db,
			`select count(*) from crawl_pages where source_id = $1 and last_fetched_at is not null`, sourceID)
	}

	// --- Run 1: a FULL sync fetches all three sitemap pages and records them. ---
	sink1 := newE2ERecSink()
	if _, err := webcrawl.NewSitemap().Sync(ctx, run(true), sink1); err != nil {
		t.Fatalf("run1 Sync: %v", err)
	}
	for _, p := range []string{"/a", "/b", "/c"} {
		if !sink1.ids[srv.URL+p] {
			t.Fatalf("run1 did not emit sitemap page %s; got %v", p, sink1.ids)
		}
	}
	if len(sink1.ids) != 3 {
		t.Fatalf("run1 emitted %d docs, want exactly 3 (sitemap files must not be emitted); got %v", len(sink1.ids), sink1.ids)
	}
	if got := pageHit(); got != 3 {
		t.Fatalf("run1 page fetches = %d, want 3 (a,b,c)", got)
	}
	if atomic.LoadInt32(&trapHits) != 0 {
		t.Fatal("run1 followed an on-page link (/trap); the sitemap connector must not follow links")
	}
	if got := fetchedRows(); got != 3 {
		t.Fatalf("run1 fetched crawl_pages = %d, want 3", got)
	}

	// --- Run 2: an INCREMENTAL sync. Every URL's sitemap <lastmod> (2020-01-01) is
	// older than its real last_fetched_at (just now), so all three are skipped with NO
	// request — cheaper than even a conditional GET — and nothing is emitted. ---
	sink2 := newE2ERecSink()
	if _, err := webcrawl.NewSitemap().Sync(ctx, run(false), sink2); err != nil {
		t.Fatalf("run2 Sync: %v", err)
	}
	if got := pageHit(); got != 3 {
		t.Fatalf("run2 re-fetched pages (total hits now %d, want still 3); lastmod-unchanged URLs must be skipped", got)
	}
	if len(sink2.ids) != 0 {
		t.Fatalf("run2 emitted %d docs; a lastmod-unchanged incremental sync must emit none: %v", len(sink2.ids), sink2.ids)
	}
	if got := fetchedRows(); got != 3 {
		t.Fatalf("run2 fetched crawl_pages = %d, want still 3", got)
	}
}
