//go:build e2e

// STORY-07.1 golden path: the web-crawl connector (internal/connector/webcrawl)
// crawling a REAL in-process website (httptest) and persisting its crawl state to a
// REAL enrolled tenant database (up via `mise run up`), reached ONLY through a
// resolver + *tenant.DB (ADR-0003, C-3) via the tenant crawl_pages table. The
// egress is the httptest loopback server (SSRF hardening is STORY-07.2), and the
// ingestion Sink is a recording connector.Sink (the real ingest sink has its own
// e2e). What is exercised for real is:
//   - BFS enumeration within the allowlist emits a connector.Document per page,
//   - crawl_pages rows are written through the tenant.DB PageStore (fetched pages
//     carry last_fetched_at + content_hash; discovered-but-unfetched pages are
//     persisted as pending, last_fetched_at NULL),
//   - RESUMABILITY: a capped first run leaves pending frontier rows, and a second
//     run over the SAME crawl_pages continues from them WITHOUT refetching the
//     already-fetched pages (asserted by the website's per-path hit counter).
//
// Assertions use the pgxpool / in-process httptest server directly, never
// `docker compose exec` (ISSUE-0014); cleanup drops the tenant DB over the control
// pool with FORCE.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/connector/webcrawl"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// e2eRecSink records emitted documents (draining each Body) for a crawl e2e.
type e2eRecSink struct {
	mu     sync.Mutex
	ids    map[string]bool
	bodies map[string]int
}

func newE2ERecSink() *e2eRecSink {
	return &e2eRecSink{ids: map[string]bool{}, bodies: map[string]int{}}
}

func (s *e2eRecSink) Put(_ context.Context, doc connector.Document) (bool, error) {
	n := 0
	if doc.Body != nil {
		b, _ := io.ReadAll(doc.Body)
		_ = doc.Body.Close()
		n = len(b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.ids[doc.ExternalID]
	s.ids[doc.ExternalID] = true
	s.bodies[doc.ExternalID] = n
	return !existed, nil
}

func (s *e2eRecSink) Complete(_ context.Context) error { return nil }

func TestWebCrawlPersistsAndResumes(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "webcrawl-" + suffix
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "WebCrawl "+suffix, dim); exit != 0 {
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

	// --- The website: a root that links to three leaves on the same host. ---
	var hits sync.Map
	count := func(p string) {
		v, _ := hits.LoadOrStore(p, new(int32))
		atomic.AddInt32(v.(*int32), 1)
	}
	hitCount := func(p string) int32 {
		if v, ok := hits.Load(p); ok {
			return atomic.LoadInt32(v.(*int32))
		}
		return 0
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {}) // allow all
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		count(r.URL.Path)
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, `<html><head><title>Home</title></head><body>
				<a href="/a">a</a><a href="/b">b</a><a href="/c">c</a></body></html>`)
			return
		}
		_, _ = io.WriteString(w, "<html><body>leaf "+r.URL.Path+"</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Sync's egress default is the SSRF guard (STORY-07.2), which blocks the loopback
	// httptest server. Inject the server's permissive client so the e2e reaches it —
	// the guard itself is unit-tested in internal/egress and internal/connector/webcrawl.
	webcrawl.SetEgressDoerForTest(srv.Client())

	sourceID := uuid.New()
	store := webcrawl.NewTenantPageStore(db, sourceID)

	cfg := func(maxPages int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"start_urls":["%s/"],"allow":["%s/"],"max_depth":2,"max_pages":%d,"concurrency":1}`,
			srv.URL, srv.URL, maxPages))
	}
	newRun := func(c json.RawMessage) connector.SyncRun {
		return connector.SyncRun{SourceID: sourceID, Config: c, State: store, Full: true, Log: nil}
	}

	// crawlPageCount counts rows for the source in the given fetched-state.
	fetchedRows := func() int {
		return tenantIntDB(ctx, t, db,
			`select count(*) from crawl_pages where source_id = $1 and last_fetched_at is not null`, sourceID)
	}
	pendingRows := func() int {
		return tenantIntDB(ctx, t, db,
			`select count(*) from crawl_pages where source_id = $1 and last_fetched_at is null`, sourceID)
	}

	// --- Run 1: cap at 1 page. Only the root is fetched; the three leaves are
	// discovered and persisted as pending frontier rows. ---
	sink1 := newE2ERecSink()
	if _, err := webcrawl.New().Sync(ctx, newRun(cfg(1)), sink1); err != nil {
		t.Fatalf("run1 Sync: %v", err)
	}
	if hitCount("/") != 1 {
		t.Fatalf("run1 root hits = %d, want 1", hitCount("/"))
	}
	if got := fetchedRows(); got != 1 {
		t.Fatalf("run1 fetched crawl_pages = %d, want 1 (root only, capped)", got)
	}
	if got := pendingRows(); got != 3 {
		t.Fatalf("run1 pending crawl_pages = %d, want 3 (a,b,c discovered)", got)
	}
	// The root's fetched row carries a content hash (STORY-07.4 state persisted).
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from crawl_pages where source_id = $1 and content_hash is not null`, sourceID); got != "1" {
		t.Fatalf("run1 rows with content_hash = %s, want 1", got)
	}

	// --- Run 2: no cap, SAME crawl_pages. The pending leaves are fetched; the root
	// is NOT refetched (resumability). ---
	sink2 := newE2ERecSink()
	if _, err := webcrawl.New().Sync(ctx, newRun(cfg(100)), sink2); err != nil {
		t.Fatalf("run2 Sync: %v", err)
	}
	if hitCount("/") != 1 {
		t.Fatalf("root refetched on resume (hits=%d); resume must skip fetched pages", hitCount("/"))
	}
	for _, p := range []string{"/a", "/b", "/c"} {
		if hitCount(p) != 1 {
			t.Fatalf("resume did not fetch pending %s (hits=%d)", p, hitCount(p))
		}
	}
	if got := fetchedRows(); got != 4 {
		t.Fatalf("after resume fetched crawl_pages = %d, want 4 (root+a+b+c)", got)
	}
	if got := pendingRows(); got != 0 {
		t.Fatalf("after resume pending crawl_pages = %d, want 0", got)
	}
	// Run 2 emitted the three leaves into the sink as documents.
	for _, p := range []string{"/a", "/b", "/c"} {
		if !sink2.ids[srv.URL+p] {
			t.Fatalf("resume did not emit document for %s; got %v", p, sink2.ids)
		}
	}
}

// tenantIntDB is tenantScalarDB parsed as an int.
func tenantIntDB(ctx context.Context, t *testing.T, db *tenant.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("tenant query %q: %v", sql, err)
	}
	return n
}
