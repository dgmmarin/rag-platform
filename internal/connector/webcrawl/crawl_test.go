package webcrawl

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/connector"
)

// --- test doubles -----------------------------------------------------------

// recSink records the documents a crawl emits, reading and closing each Body.
type recSink struct {
	mu        sync.Mutex
	byID      map[string]connector.Document
	bodies    map[string][]byte
	completes int
}

func newRecSink() *recSink {
	return &recSink{byID: map[string]connector.Document{}, bodies: map[string][]byte{}}
}

func (s *recSink) Put(_ context.Context, doc connector.Document) (bool, error) {
	var body []byte
	if doc.Body != nil {
		body, _ = io.ReadAll(doc.Body)
		_ = doc.Body.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.byID[doc.ExternalID]
	s.byID[doc.ExternalID] = doc
	s.bodies[doc.ExternalID] = body
	return !existed, nil
}

func (s *recSink) Complete(_ context.Context) error {
	s.mu.Lock()
	s.completes++
	s.mu.Unlock()
	return nil
}

func (s *recSink) ids() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for k := range s.byID {
		out[k] = true
	}
	return out
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func syncRun(state connector.StateStore) connector.SyncRun {
	return connector.SyncRun{SourceID: uuid.New(), State: state, Full: true, Log: testLogger()}
}

// --- crawl tests ------------------------------------------------------------

func TestCrawlBFSDepthLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", page(`<a href="/a">a</a><a href="/b">b</a>`))
	mux.HandleFunc("/a", page(`<a href="/deep">deep</a>`))
	mux.HandleFunc("/b", page(`ok`))
	mux.HandleFunc("/deep", page(`too deep`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 1, MaxPages: 100, Concurrency: 2}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	ids := sink.ids()
	for _, want := range []string{srv.URL + "/", srv.URL + "/a", srv.URL + "/b"} {
		if !ids[want] {
			t.Fatalf("depth<=1 page %q not fetched; got %v", want, ids)
		}
	}
	if ids[srv.URL+"/deep"] {
		t.Fatalf("depth-2 page /deep fetched despite max_depth=1")
	}
}

// A clean FULL crawl runs the full-sync reconcile (sink.Complete) exactly once.
func TestCrawlCleanCrawlCompletesOnce(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", page(`<a href="/a">a</a>`))
	mux.HandleFunc("/a", page(`ok`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, Concurrency: 2}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.completes != 1 {
		t.Fatalf("Complete ran %d times on a clean crawl; want 1", sink.completes)
	}
}

// ISSUE-0072: a FULL crawl truncated by a context timeout/cancel must NOT run the
// full-sync delete pass, or it soft-deletes every page it never reached. finish
// skips sink.Complete and returns the context error so the job fails and retries.
func TestCrawlTruncatedSkipsComplete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", page(`<a href="/a">a</a>`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, Concurrency: 2}.withDefaults()
	sink := newRecSink()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // truncate before the crawl reaches the frontier

	cr := newCrawler(cfg, srv.Client())
	_, err := cr.run(ctx, syncRun(newMemPageStore()), sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run err = %v, want context.Canceled", err)
	}
	if sink.completes != 0 {
		t.Fatalf("Complete ran %d times on a truncated crawl; want 0 (ISSUE-0072)", sink.completes)
	}
}

// ISSUE-0075: a FRESH full re-crawl (a new run series) must re-fetch pages fetched
// in a PRIOR run, not resume-skip them. Scoping resume to the run-series start
// (SyncRun.Since) is what stops a re-crawl over fully-fetched state from fetching
// nothing, seeing zero documents, and (before the sink's zero-seen guard) wiping
// the corpus.
func TestCrawlFullReCrawlRefetchesPriorRun(t *testing.T) {
	var hits sync.Map
	count := func(p string) { v, _ := hits.LoadOrStore(p, new(int32)); atomic.AddInt32(v.(*int32), 1) }
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { count("/"); _, _ = io.WriteString(w, "root") })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	hitCount := func(p string) int32 {
		v, ok := hits.Load(p)
		if !ok {
			return 0
		}
		return atomic.LoadInt32(v.(*int32))
	}

	store := newMemPageStore()
	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 1, MaxPages: 10, Concurrency: 1}.withDefaults()

	// Run 1: fetch root (persisted with LastFetchedAt ~ now).
	if _, err := newCrawler(cfg, srv.Client()).run(context.Background(), syncRun(store), newRecSink()); err != nil {
		t.Fatalf("run1: %v", err)
	}
	if hitCount("/") != 1 {
		t.Fatalf("run1 root hits = %d, want 1", hitCount("/"))
	}

	// Run 2: a NEW full run series whose start (Since) is AFTER run 1's fetch, so the
	// persisted root is prior-run state and must be re-fetched.
	run2 := connector.SyncRun{
		SourceID: uuid.New(), State: store, Full: true,
		Since: time.Now().Add(time.Hour), Log: testLogger(),
	}
	if _, err := newCrawler(cfg, srv.Client()).run(context.Background(), run2, newRecSink()); err != nil {
		t.Fatalf("run2: %v", err)
	}
	if hitCount("/") != 2 {
		t.Fatalf("root not refetched on a fresh full re-crawl (hits=%d); want 2 (ISSUE-0075)", hitCount("/"))
	}
}

func TestCrawlMaxPagesCap(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {}) // allow all
	// A hub linking to many pages; each fetch increments hits.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `<html><body><a href="/1">1</a><a href="/2">2</a><a href="/3">3</a><a href="/4">4</a></body></html>`)
	})
	for _, p := range []string{"/1", "/2", "/3", "/4"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = io.WriteString(w, "leaf")
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 5, MaxPages: 2, Concurrency: 4}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("max_pages=2 but server received %d fetches", got)
	}
	if n := len(sink.ids()); n != 2 {
		t.Fatalf("emitted %d docs, want exactly 2 (max_pages)", n)
	}
}

func TestCrawlAllowDenyGating(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", page(`<a href="/allowed">ok</a><a href="/search?page=2">search</a><a href="https://external.invalid/x">ext</a>`))
	mux.HandleFunc("/allowed", page(`allowed`))
	mux.HandleFunc("/search", page(`should not be crawled`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{
		StartURLs: []string{srv.URL + "/"},
		Allow:     []string{srv.URL + "/"},
		Deny:      []string{"/search", "?page="},
		MaxDepth:  3, MaxPages: 100, Concurrency: 2,
	}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	ids := sink.ids()
	if !ids[srv.URL+"/allowed"] {
		t.Fatalf("/allowed should have been crawled; got %v", ids)
	}
	if ids[srv.URL+"/search"] {
		t.Fatal("/search fetched despite deny rule")
	}
	for id := range ids {
		if id == "https://external.invalid/x" {
			t.Fatal("external link fetched despite allowlist")
		}
	}
}

func TestCrawlRobotsHonoured(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "User-agent: *\nDisallow: /private\n")
	})
	mux.HandleFunc("/", page(`<a href="/private">no</a><a href="/ok">yes</a>`))
	mux.HandleFunc("/ok", page(`ok`))
	blocked := int32(0)
	mux.HandleFunc("/private", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&blocked, 1)
		_, _ = io.WriteString(w, "secret")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 3, MaxPages: 100, Concurrency: 1}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	if atomic.LoadInt32(&blocked) != 0 {
		t.Fatal("robots-disallowed /private was fetched")
	}
	if !sink.ids()[srv.URL+"/ok"] {
		t.Fatal("/ok should have been crawled")
	}
}

func TestCrawlCanonicalExternalID(t *testing.T) {
	mux := http.NewServeMux()
	// Two URLs that both declare the same canonical; the crawler should emit the
	// canonical ExternalID and not duplicate it.
	canonicalBody := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><link rel="canonical" href="`+canonHref+`"></head><body>widget</body></html>`)
	}
	mux.HandleFunc("/", page(`<a href="/widget?utm_source=nav">w1</a><a href="/widget?sid=2">w2</a>`))
	mux.HandleFunc("/widget", canonicalBody)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// canonHref must be absolute for this server; rewrite via a closure variable.
	canonHref = srv.URL + "/widget"

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, Concurrency: 2}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.ids()[srv.URL+"/widget"] {
		t.Fatalf("canonical ExternalID %q not emitted; got %v", srv.URL+"/widget", sink.ids())
	}
	// The two query-string variants must collapse to one canonical document.
	count := 0
	for id := range sink.ids() {
		if id == srv.URL+"/widget" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("canonical document emitted %d times, want 1", count)
	}
}

func TestCrawlNonHTMLPassedAsBody(t *testing.T) {
	pdf := []byte("%PDF-1.4\n...binary...")
	mux := http.NewServeMux()
	mux.HandleFunc("/", page(`<a href="/doc.pdf">pdf</a>`))
	mux.HandleFunc("/doc.pdf", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdf)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, Concurrency: 2}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	id := srv.URL + "/doc.pdf"
	doc, ok := sink.byID[id]
	if !ok {
		t.Fatalf("pdf not emitted; got %v", sink.ids())
	}
	if doc.MimeType != "application/pdf" {
		t.Fatalf("pdf MimeType = %q, want application/pdf", doc.MimeType)
	}
	if string(sink.bodies[id]) != string(pdf) {
		t.Fatal("pdf body not passed through verbatim")
	}
}

func TestCrawlResumesFromPersistedState(t *testing.T) {
	var hits sync.Map // path -> count
	count := func(p string) {
		v, _ := hits.LoadOrStore(p, new(int32))
		atomic.AddInt32(v.(*int32), 1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {}) // allow all
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		count("/")
		_, _ = io.WriteString(w, `<html><body><a href="/a">a</a><a href="/b">b</a></body></html>`)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, _ *http.Request) { count("/a"); _, _ = io.WriteString(w, "a") })
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) { count("/b"); _, _ = io.WriteString(w, "b") })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store := newMemPageStore()

	// Run 1: cap at 1 page -> only root is fetched; /a and /b are discovered and
	// persisted as pending (unfetched) frontier.
	cfg1 := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 1, Concurrency: 1}.withDefaults()
	if _, err := newCrawler(cfg1, srv.Client()).run(context.Background(), syncRun(store), newRecSink()); err != nil {
		t.Fatalf("run1: %v", err)
	}
	hitCount := func(p string) int32 {
		v, ok := hits.Load(p)
		if !ok {
			return 0
		}
		return atomic.LoadInt32(v.(*int32))
	}
	if hitCount("/") != 1 {
		t.Fatalf("run1 root hits = %d, want 1", hitCount("/"))
	}

	// Run 2: no cap, same store -> root must NOT be refetched (already done), while
	// the pending /a and /b are fetched.
	cfg2 := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, Concurrency: 1}.withDefaults()
	sink2 := newRecSink()
	if _, err := newCrawler(cfg2, srv.Client()).run(context.Background(), syncRun(store), sink2); err != nil {
		t.Fatalf("run2: %v", err)
	}
	if hitCount("/") != 1 {
		t.Fatalf("root was refetched on resume (hits=%d); resume must skip fetched pages", hitCount("/"))
	}
	if hitCount("/a") != 1 || hitCount("/b") != 1 {
		t.Fatalf("resume did not fetch pending frontier: /a=%d /b=%d", hitCount("/a"), hitCount("/b"))
	}
}

func TestCrawlPerHostDelay(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", page(`<a href="/a">a</a><a href="/b">b</a>`))
	mux.HandleFunc("/a", page(`a`))
	mux.HandleFunc("/b", page(`b`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var mu sync.Mutex
	var sleeps []time.Duration
	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, DelayMS: 300, Concurrency: 1}.withDefaults()
	cr := newCrawler(cfg, srv.Client())
	cr.sleep = func(d time.Duration) {
		if d > 0 {
			mu.Lock()
			sleeps = append(sleeps, d)
			mu.Unlock()
		}
	}
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), newRecSink()); err != nil {
		t.Fatalf("run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) == 0 {
		t.Fatal("per-host delay never applied between same-host fetches")
	}
	if sleeps[0] <= 0 || sleeps[0] > 300*time.Millisecond {
		t.Fatalf("delay = %v, want (0, 300ms]", sleeps[0])
	}
}

// syncRunIncremental is an incremental (non-full) SyncRun: deletion detection is
// off, so a conditional re-crawl re-visits previously-fetched pages cheaply
// (STORY-07.4). syncRun (Full: true) keeps the STORY-07.1 resume-skip semantics.
func syncRunIncremental(state connector.StateStore) connector.SyncRun {
	return connector.SyncRun{SourceID: uuid.New(), State: state, Full: false, Log: testLogger()}
}

// mustNorm normalises a raw URL for a test, failing on error.
func mustNorm(t *testing.T, raw string) string {
	t.Helper()
	norm, _, err := parseAndNormalize(raw)
	if err != nil {
		t.Fatalf("normalise %q: %v", raw, err)
	}
	return norm
}

// TestCrawlConditional304SkipsParseAndEmit is the FR-ING-02 golden path: a page
// with a prior ETag in crawl_pages is re-fetched with If-None-Match on an
// incremental crawl; a 304 Not Modified means unchanged — NO parse, NO extract, NO
// emit — only last_fetched_at is bumped (validators kept). Proof of "no parse": the
// 304 response still carries a <a href="/trap"> body, and /trap must never be
// crawled (the crawler did not read/parse the body).
func TestCrawlConditional304SkipsParseAndEmit(t *testing.T) {
	var condSeen, trapHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "etag-1")
		if r.Header.Get("If-None-Match") == "etag-1" {
			atomic.AddInt32(&condSeen, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// A body with a trap link — if the crawler parses a 304, it would follow it.
		_, _ = io.WriteString(w, `<html><body><a href="/trap">t</a>fresh</body></html>`)
	})
	mux.HandleFunc("/trap", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&trapHits, 1)
		_, _ = io.WriteString(w, "trap")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	norm := mustNorm(t, srv.URL+"/")
	store := newMemPageStore()
	// Prior crawl state: the page was fetched with ETag "etag-1" and some content hash.
	_ = store.Upsert(context.Background(), Page{
		URL: srv.URL + "/", NormalizedURL: norm, Depth: 0,
		Fetched: true, Status: 200, ETag: "etag-1", ContentHash: []byte("prior-hash"),
	})

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, Concurrency: 1}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRunIncremental(store), sink); err != nil {
		t.Fatalf("run: %v", err)
	}

	if atomic.LoadInt32(&condSeen) == 0 {
		t.Fatal("crawler never sent a conditional If-None-Match request")
	}
	if n := len(sink.ids()); n != 0 {
		t.Fatalf("a 304 page was emitted to the sink (%d docs); want none", n)
	}
	if atomic.LoadInt32(&trapHits) != 0 {
		t.Fatal("304 body was parsed: the trap link was followed")
	}
	loaded, _ := store.Load(context.Background())
	p := loaded[norm]
	if !p.Fetched || p.ETag != "etag-1" || string(p.ContentHash) != "prior-hash" {
		t.Fatalf("304 must keep fetched state + validators + hash; got %+v", p)
	}
}

// TestCrawlConditionalContentHashSkipsReEmit covers the no-ETag case (FR-ING-02):
// many servers send no ETag/Last-Modified, so change detection falls back to the
// content hash. Identical bytes on a re-crawl must NOT re-emit and must NOT re-parse
// (the trap link is not followed). Only changed bytes re-emit (next test).
func TestCrawlConditionalContentHashSkipsReEmit(t *testing.T) {
	const body = `<html><body><a href="/trap">t</a>unchanged</body></html>`
	var rootHits, trapHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&rootHits, 1)
		_, _ = io.WriteString(w, body) // no ETag, no Last-Modified
	})
	mux.HandleFunc("/trap", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&trapHits, 1)
		_, _ = io.WriteString(w, "trap")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	norm := mustNorm(t, srv.URL+"/")
	sum := sha256.Sum256([]byte(body))
	store := newMemPageStore()
	_ = store.Upsert(context.Background(), Page{
		URL: srv.URL + "/", NormalizedURL: norm, Depth: 0,
		Fetched: true, Status: 200, ContentHash: sum[:], // no ETag
	})

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 2, MaxPages: 100, Concurrency: 1}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRunIncremental(store), sink); err != nil {
		t.Fatalf("run: %v", err)
	}

	if atomic.LoadInt32(&rootHits) != 1 {
		t.Fatalf("root fetched %d times, want exactly 1 (a plain GET; no ETag)", atomic.LoadInt32(&rootHits))
	}
	if n := len(sink.ids()); n != 0 {
		t.Fatalf("identical-bytes page re-emitted (%d docs); want none", n)
	}
	if atomic.LoadInt32(&trapHits) != 0 {
		t.Fatal("unchanged page was parsed: the trap link was followed")
	}
}

// TestCrawlConditionalReEmitsOnChange is the negative control: when the bytes differ
// from the stored content hash, the page IS re-emitted (change detection must not
// suppress real changes).
func TestCrawlConditionalReEmitsOnChange(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(_ http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><body>brand new content</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	norm := mustNorm(t, srv.URL+"/")
	store := newMemPageStore()
	_ = store.Upsert(context.Background(), Page{
		URL: srv.URL + "/", NormalizedURL: norm, Depth: 0,
		Fetched: true, Status: 200, ContentHash: []byte("a-different-old-hash"),
	})

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 1, MaxPages: 100, Concurrency: 1}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	if _, err := cr.run(context.Background(), syncRunIncremental(store), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.ids()[srv.URL+"/"] {
		t.Fatalf("changed page not re-emitted; got %v", sink.ids())
	}
}

// page is an http.HandlerFunc serving a minimal HTML document wrapping body.
func page(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html><body>"+body+"</body></html>")
	}
}

// canonHref is set per-test to the test server's canonical URL.
var canonHref string
