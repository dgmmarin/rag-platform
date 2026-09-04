package webcrawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/connector"
)

// xmlHandler serves a fixed XML (or any bytes) body.
func rawHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}
}

// urlset renders a <urlset> document from (loc, lastmod) pairs. A blank lastmod is
// omitted.
func urlset(entries ...[2]string) string {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, e := range entries {
		b.WriteString("<url><loc>" + e[0] + "</loc>")
		if e[1] != "" {
			b.WriteString("<lastmod>" + e[1] + "</lastmod>")
		}
		b.WriteString("</url>")
	}
	b.WriteString(`</urlset>`)
	return b.String()
}

// sitemapindex renders a <sitemapindex> pointing at child sitemap locs.
func sitemapindex(locs ...string) string {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, l := range locs {
		b.WriteString("<sitemap><loc>" + l + "</loc></sitemap>")
	}
	b.WriteString(`</sitemapindex>`)
	return b.String()
}

// TestSitemapIndexAndChildParsing is the FR-SRC-06 golden path: a sitemap INDEX that
// points at two child sitemaps, whose <url> entries are the crawl frontier. Every
// listed page must be emitted; the sitemap documents themselves are never emitted.
func TestSitemapIndexAndChildParsing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/sitemap_index.xml", rawHandler(sitemapindex(srv.URL+"/sitemap1.xml", srv.URL+"/sitemap2.xml")))
	mux.HandleFunc("/sitemap1.xml", rawHandler(urlset([2]string{srv.URL + "/a", ""}, [2]string{srv.URL + "/b", ""})))
	mux.HandleFunc("/sitemap2.xml", rawHandler(urlset([2]string{srv.URL + "/c", ""})))
	mux.HandleFunc("/a", page(`page a`))
	mux.HandleFunc("/b", page(`page b`))
	mux.HandleFunc("/c", page(`page c`))

	sink := newRecSink()
	cr := newSitemapCrawler(config{Concurrency: 2}, srv.Client())
	if _, err := cr.runSitemap(context.Background(), []string{srv.URL + "/sitemap_index.xml"}, syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("runSitemap: %v", err)
	}
	ids := sink.ids()
	for _, want := range []string{srv.URL + "/a", srv.URL + "/b", srv.URL + "/c"} {
		if !ids[want] {
			t.Fatalf("page %q from sitemap not emitted; got %v", want, ids)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("emitted %d docs, want exactly 3 (the sitemap files must not be emitted); got %v", len(ids), ids)
	}
}

// TestSitemapGzip proves a gzipped sitemap (real sitemaps are frequently .xml.gz) is
// transparently decompressed and parsed. Detection is by the gzip magic bytes, so it
// works regardless of Content-Type.
func TestSitemapGzip(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(urlset([2]string{srv.URL + "/g1", ""})))
	_ = zw.Close()
	mux.HandleFunc("/sitemap.xml.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(gz.Bytes())
	})
	mux.HandleFunc("/g1", page(`gzipped page`))

	sink := newRecSink()
	cr := newSitemapCrawler(config{Concurrency: 1}, srv.Client())
	if _, err := cr.runSitemap(context.Background(), []string{srv.URL + "/sitemap.xml.gz"}, syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("runSitemap: %v", err)
	}
	if !sink.ids()[srv.URL+"/g1"] {
		t.Fatalf("gzipped sitemap page not emitted; got %v", sink.ids())
	}
}

// TestSitemapNoLinkFollowing proves the sitemap connector does NOT expand the frontier
// with links found on fetched pages (unlike web_crawl): the listed page carries a link
// to /trap, which must never be crawled — the frontier is exactly the sitemap's URLs.
func TestSitemapNoLinkFollowing(t *testing.T) {
	var trapHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/sitemap.xml", rawHandler(urlset([2]string{srv.URL + "/page", ""})))
	mux.HandleFunc("/page", page(`<a href="/trap">trap</a>hello`))
	mux.HandleFunc("/trap", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&trapHits, 1)
		_, _ = io.WriteString(w, "trap")
	})

	sink := newRecSink()
	cr := newSitemapCrawler(config{Concurrency: 1}, srv.Client())
	if _, err := cr.runSitemap(context.Background(), []string{srv.URL + "/sitemap.xml"}, syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("runSitemap: %v", err)
	}
	if !sink.ids()[srv.URL+"/page"] {
		t.Fatalf("listed page not emitted; got %v", sink.ids())
	}
	if atomic.LoadInt32(&trapHits) != 0 {
		t.Fatal("sitemap connector followed an on-page link (/trap); it must not follow links")
	}
	if len(sink.ids()) != 1 {
		t.Fatalf("emitted %d docs, want exactly 1 (only the sitemap URL); got %v", len(sink.ids()), sink.ids())
	}
}

// TestSitemapLastmodIncrementalSkip proves the lastmod incremental optimisation: on an
// incremental sync, a URL whose sitemap <lastmod> is NOT newer than our recorded last
// fetch is skipped entirely (no request at all — cheaper than a conditional GET); a URL
// whose lastmod IS newer is fetched.
func TestSitemapLastmodIncrementalSkip(t *testing.T) {
	var pageHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&pageHits, 1)
		_, _ = io.WriteString(w, "<html><body>content</body></html>")
	})

	lastFetched := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	norm := mustNorm(t, srv.URL+"/page")

	newStore := func() *memPageStore {
		s := newMemPageStore()
		_ = s.Upsert(context.Background(), Page{
			URL: srv.URL + "/page", NormalizedURL: norm, Depth: 0,
			Fetched: true, Status: 200, LastFetchedAt: lastFetched,
		})
		return s
	}

	t.Run("older lastmod skips fetch", func(t *testing.T) {
		atomic.StoreInt32(&pageHits, 0)
		mux.HandleFunc("/sitemap-old.xml", rawHandler(urlset([2]string{srv.URL + "/page", "2020-01-01"})))
		sink := newRecSink()
		cr := newSitemapCrawler(config{Concurrency: 1}, srv.Client())
		if _, err := cr.runSitemap(context.Background(), []string{srv.URL + "/sitemap-old.xml"}, syncRunIncremental(newStore()), sink); err != nil {
			t.Fatalf("runSitemap: %v", err)
		}
		if atomic.LoadInt32(&pageHits) != 0 {
			t.Fatalf("older-lastmod page was fetched (%d hits); it must be skipped without a request", atomic.LoadInt32(&pageHits))
		}
		if len(sink.ids()) != 0 {
			t.Fatalf("older-lastmod page emitted %d docs; want none", len(sink.ids()))
		}
	})

	t.Run("newer lastmod fetches", func(t *testing.T) {
		atomic.StoreInt32(&pageHits, 0)
		mux.HandleFunc("/sitemap-new.xml", rawHandler(urlset([2]string{srv.URL + "/page", "2022-01-01"})))
		sink := newRecSink()
		cr := newSitemapCrawler(config{Concurrency: 1}, srv.Client())
		if _, err := cr.runSitemap(context.Background(), []string{srv.URL + "/sitemap-new.xml"}, syncRunIncremental(newStore()), sink); err != nil {
			t.Fatalf("runSitemap: %v", err)
		}
		if atomic.LoadInt32(&pageHits) != 1 {
			t.Fatalf("newer-lastmod page fetched %d times, want exactly 1", atomic.LoadInt32(&pageHits))
		}
		if !sink.ids()[srv.URL+"/page"] {
			t.Fatalf("newer-lastmod page not emitted; got %v", sink.ids())
		}
	})
}

// --- connector-surface tests ------------------------------------------------

func TestSitemapKind(t *testing.T) {
	if k := NewSitemap().Kind(); k != connector.KindSitemap {
		t.Fatalf("Kind() = %q, want %q", k, connector.KindSitemap)
	}
}

func TestSitemapRegisteredInDefaultRegistry(t *testing.T) {
	c, ok := connector.Lookup(connector.KindSitemap)
	if !ok {
		t.Fatal("sitemap connector not registered in the default registry")
	}
	if c.Kind() != connector.KindSitemap {
		t.Fatalf("registered connector Kind() = %q", c.Kind())
	}
}

func TestSitemapValidateConfig(t *testing.T) {
	if err := NewSitemap().ValidateConfig(json.RawMessage(
		`{"sitemap_urls":["https://acme.com/sitemap.xml"],"deny":["/tmp"],"delay_ms":500,"concurrency":4}`)); err != nil {
		t.Fatalf("ValidateConfig(valid) = %v, want nil", err)
	}
	bad := map[string]string{
		"missing sitemap_urls": `{"delay_ms":1}`,
		"empty sitemap_urls":   `{"sitemap_urls":[]}`,
		"unknown field":        `{"sitemap_urls":["https://x/s.xml"],"start_urls":["https://x/"]}`,
		"non-http url":         `{"sitemap_urls":["ftp://x/s.xml"]}`,
		"malformed":            `not json`,
	}
	for name, cfg := range bad {
		t.Run(name, func(t *testing.T) {
			if err := NewSitemap().ValidateConfig(json.RawMessage(cfg)); err == nil {
				t.Fatalf("ValidateConfig(%s) = nil, want error", cfg)
			}
		})
	}
}

// TestSitemapURLBudget is the hostile-tree ceiling: collection stops at the URL budget
// rather than growing unbounded. (Uses a small override to keep the test cheap.)
func TestSitemapURLBudgetHonoured(t *testing.T) {
	if maxSitemapURLs <= 0 || maxSitemapDocs <= 0 || maxSitemapDepth <= 0 {
		t.Fatalf("sitemap bounds must be positive: urls=%d docs=%d depth=%d", maxSitemapURLs, maxSitemapDocs, maxSitemapDepth)
	}
}
