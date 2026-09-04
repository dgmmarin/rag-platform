package webcrawl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/rag-platform/ragctl/internal/connector"
)

// item is one frontier entry: a URL to (maybe) fetch at a BFS depth.
type item struct {
	norm  string // normalised URL — the visited/crawl_pages key
	raw   string // URL actually requested (pre-normalisation)
	depth int
}

// hostGate serialises and spaces fetches to one host (SPEC-04 §2 per-host delay).
// Holding mu across the politeness sleep makes concurrent same-host fetches wait.
type hostGate struct {
	mu   sync.Mutex
	next time.Time
}

// crawler is one web-crawl run. It is constructed per Sync; its fields carry the
// config, the egress seam and injectable clock/sleep for deterministic tests.
type crawler struct {
	cfg      config
	doer     Doer
	ua       string
	maxBytes int // per-response size cap (SPEC-09 §4); tests set a small value
	now      func() time.Time
	sleep    func(time.Duration)

	// per-run wiring (set in run)
	store   PageStore
	limiter *rate.Limiter
	log     *slog.Logger

	allowPrefixes []string
	seedHosts     map[string]bool

	fetched int32 // atomic: fetch attempts admitted (max_pages cap)

	mu      sync.Mutex
	visited map[string]bool // normalised URLs queued or fetched
	emitted map[string]bool // ExternalIDs already sent to the sink (canonical dedupe)
	stats   connector.Stats

	robotsMu sync.Mutex
	robots   map[string]*robotsRules

	gatesMu sync.Mutex
	gates   map[string]*hostGate
}

// newCrawler builds a crawler with defaults; tests override now/sleep and the Doer.
func newCrawler(cfg config, doer Doer) *crawler {
	return &crawler{
		cfg:      cfg,
		doer:     doer,
		ua:       userAgent,
		maxBytes: maxResponseBytes,
		now:      time.Now,
		sleep:    func(d time.Duration) { time.Sleep(d) },
		visited:  map[string]bool{},
		emitted:  map[string]bool{},
		robots:   map[string]*robotsRules{},
		gates:    map[string]*hostGate{},
	}
}

// run performs the crawl: it resolves the resume state, seeds the frontier, and
// walks it breadth-first honouring depth/pages limits, allow/deny, robots and the
// per-host delay, emitting each fetched page into the sink. It always calls
// sink.Complete at the end.
func (c *crawler) run(ctx context.Context, sr connector.SyncRun, sink connector.Sink) (connector.Stats, error) {
	c.log = sr.Log
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c.limiter = sr.Limiter

	// Resolve the PageStore capability of the run's State (SPEC-04 §2). Absent it,
	// fall back to a non-resumable in-memory store and warn.
	if ps, ok := sr.State.(PageStore); ok && ps != nil {
		c.store = ps
	} else {
		c.store = newMemPageStore()
		c.log.Warn("webcrawl: state store has no crawl-page persistence; crawl is not resumable")
	}

	c.prepareAllow()

	// Load persisted state for resumability.
	loaded, err := c.store.Load(ctx)
	if err != nil {
		return c.stats, err
	}

	frontier := map[int][]item{}
	alreadyFetched := map[string]bool{}
	add := func(norm, raw string, depth int) {
		if c.visited[norm] {
			return
		}
		c.visited[norm] = true
		frontier[depth] = append(frontier[depth], item{norm: norm, raw: raw, depth: depth})
	}

	// Seeds (depth 0). Explicitly configured, so they bypass the allowlist but still
	// respect deny.
	for _, s := range c.cfg.StartURLs {
		norm, u, err := parseAndNormalize(s)
		if err != nil || c.denied(norm) {
			continue
		}
		add(norm, u.String(), 0)
	}

	// Merge persisted pages: fetched ones are skipped on this run; pending ones
	// (discovered but not yet fetched) are re-queued at their recorded depth — this
	// is what makes an interrupted crawl resume instead of restarting.
	for norm, p := range loaded {
		if p.Fetched {
			alreadyFetched[norm] = true
		}
		if c.visited[norm] {
			continue
		}
		c.visited[norm] = true
		if !p.Fetched && p.Depth <= c.cfg.MaxDepth {
			frontier[p.Depth] = append(frontier[p.Depth], item{norm: norm, raw: p.URL, depth: p.Depth})
		}
	}

	// Breadth-first, level by level. Fetching within a level is concurrent (bounded
	// by concurrency); the level barrier keeps the depth accounting exact.
	for d := 0; d <= c.cfg.MaxDepth; d++ {
		items := frontier[d]
		if len(items) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return c.finish(ctx, sink, err)
		}
		var nextMu sync.Mutex
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(c.cfg.Concurrency)
		for _, it := range items {
			it := it
			if alreadyFetched[it.norm] {
				continue // already fetched in a prior run — do not refetch (resume)
			}
			g.Go(func() error {
				links, err := c.process(gctx, it, sink)
				if err != nil {
					return err
				}
				if it.depth+1 > c.cfg.MaxDepth {
					return nil
				}
				nextMu.Lock()
				for _, l := range links {
					if c.visited[l] || c.denied(l) || !c.allowed(l) {
						continue
					}
					c.visited[l] = true
					frontier[it.depth+1] = append(frontier[it.depth+1], item{norm: l, raw: l, depth: it.depth + 1})
					c.persistPending(ctx, l, it.depth+1)
				}
				nextMu.Unlock()
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return c.finish(ctx, sink, err)
		}
	}
	return c.finish(ctx, sink, nil)
}

// process fetches one page (subject to robots, the max_pages cap and the per-host
// delay), emits it into the sink and persists its crawl state. It returns the
// outbound links discovered on an HTML page. A per-page HTTP/parse failure is
// recorded and swallowed (nil error) so the crawl continues; only a context
// cancellation propagates.
func (c *crawler) process(ctx context.Context, it item, sink connector.Sink) ([]string, error) {
	u, err := url.Parse(it.norm)
	if err != nil {
		return nil, nil
	}
	host := u.Host

	// robots.txt (fetched once per host, cached).
	if !c.robotsFor(ctx, u).allowed(u.RequestURI()) {
		c.log.Debug("webcrawl: robots disallow", "url", it.norm)
		return nil, nil
	}

	// max_pages cap: admit at most MaxPages fetches, whatever the concurrency.
	if int(atomic.AddInt32(&c.fetched, 1)) > c.cfg.MaxPages {
		return nil, nil
	}

	// Politeness: global limiter (if the worker supplied one) then the per-host gate.
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, ctx.Err()
		}
	}
	c.gateFor(host).wait(c)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, it.raw, nil)
	if err != nil {
		return nil, nil
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.doer.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.recordError(ctx, it, 0, err.Error())
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// Size cap (SPEC-09 §4: "max response size 20 MB"). Read at most cap+1 bytes so
	// an over-cap body is REJECTED rather than silently truncated into a half-parsed
	// document; capping the read also bounds memory against a hostile/huge response.
	limit := int64(c.maxBytes)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(body)) > limit {
		c.recordError(ctx, it, resp.StatusCode, "response exceeds size cap")
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.recordError(ctx, it, resp.StatusCode, "http status "+resp.Status)
		return nil, nil
	}

	mime := mimeOf(resp.Header.Get("Content-Type"))
	links := c.emit(ctx, it, u, resp, body, mime, sink)

	// Persist fetched state (etag/last-modified/hash captured for STORY-07.4).
	sum := sha256.Sum256(body)
	if err := c.store.Upsert(ctx, Page{
		URL: it.raw, NormalizedURL: it.norm, Depth: it.depth,
		Fetched: true, Status: resp.StatusCode,
		ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"),
		ContentHash: sum[:],
	}); err != nil {
		return nil, err // a crawl_pages write failure is fatal: resumability depends on it
	}
	return links, nil
}

// emit sends one fetched page into the sink as a connector.Document. For HTML it
// parses the minimal structure (title, canonical, links); the canonical URL, when
// present, becomes the ExternalID (SPEC-04 §2). The raw bytes are the Body — the
// content-extraction quality is STORY-07.3. Returns discovered links (HTML only).
func (c *crawler) emit(ctx context.Context, it item, u *url.URL, resp *http.Response, body []byte, mime string, sink connector.Sink) (links []string) {
	extID := it.norm
	title := ""
	var isHTML bool
	if strings.Contains(mime, "html") {
		isHTML = true
		if ex, err := extractHTML(body, u); err == nil {
			links = ex.links
			title = ex.title
			if ex.canonical != "" {
				extID = ex.canonical
			}
		}
	}

	// Canonical de-duplication: two URLs sharing a canonical emit one Document.
	c.mu.Lock()
	if c.emitted[extID] {
		c.mu.Unlock()
		return links
	}
	c.emitted[extID] = true
	c.mu.Unlock()

	if mime == "" {
		mime = "application/octet-stream"
	}
	doc := connector.Document{
		ExternalID: extID,
		Title:      title,
		URI:        extID,
		MimeType:   mime,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Metadata: map[string]any{
			"crawl_url": it.raw,
			"depth":     it.depth,
			"status":    resp.StatusCode,
			"html":      isHTML,
		},
	}
	changed, err := sink.Put(ctx, doc)
	c.mu.Lock()
	c.stats.DocsSeen++
	c.stats.BytesFetched += int64(len(body))
	if err == nil && changed {
		c.stats.DocsChanged++
	}
	c.mu.Unlock()
	if err != nil {
		c.log.Warn("webcrawl: sink rejected document", "external_id", extID, "err", err.Error())
	}
	return links
}

// finish calls sink.Complete and returns the accumulated stats. A context error
// from the crawl is returned to the caller (the worker) after Complete runs.
func (c *crawler) finish(ctx context.Context, sink connector.Sink, cause error) (connector.Stats, error) {
	if err := sink.Complete(ctx); err != nil && cause == nil {
		cause = err
	}
	c.mu.Lock()
	out := c.stats
	c.mu.Unlock()
	return out, cause
}

// prepareAllow normalises the configured allow prefixes and records the seed hosts
// (the default allowlist when none is configured: stay on the seed hosts).
func (c *crawler) prepareAllow() {
	c.seedHosts = map[string]bool{}
	for _, s := range c.cfg.StartURLs {
		if _, u, err := parseAndNormalize(s); err == nil {
			c.seedHosts[strings.ToLower(u.Host)] = true
		}
	}
	for _, a := range c.cfg.Allow {
		if norm, _, err := parseAndNormalize(a); err == nil {
			c.allowPrefixes = append(c.allowPrefixes, norm)
		}
	}
}

// allowed reports whether a normalised URL is within the crawl scope: it matches an
// allow prefix, or — when no allowlist is configured — shares a host with a seed.
func (c *crawler) allowed(norm string) bool {
	if len(c.allowPrefixes) == 0 {
		u, err := url.Parse(norm)
		return err == nil && c.seedHosts[strings.ToLower(u.Host)]
	}
	for _, p := range c.allowPrefixes {
		if strings.HasPrefix(norm, p) {
			return true
		}
	}
	return false
}

// denied reports whether a URL matches any deny pattern (substring match; SPEC-04
// §2 deny examples like "/search" and "?page=").
//
// ponytail: deny is substring-based, which covers both the path-prefix and
// query-pattern examples in one rule; a future story can add anchored/regex rules.
func (c *crawler) denied(norm string) bool {
	for _, d := range c.cfg.Deny {
		if d != "" && strings.Contains(norm, d) {
			return true
		}
	}
	return false
}

// robotsFor returns the cached robots rules for the host, fetching /robots.txt once.
// A fetch error or non-2xx response is treated as allow-all (the permissive
// convention), so an absent robots.txt never blocks a crawl.
func (c *crawler) robotsFor(ctx context.Context, u *url.URL) *robotsRules {
	c.robotsMu.Lock()
	defer c.robotsMu.Unlock()
	if r, ok := c.robots[u.Host]; ok {
		return r
	}
	var data []byte
	robotsURL := u.Scheme + "://" + u.Host + "/robots.txt"
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil); err == nil {
		req.Header.Set("User-Agent", c.ua)
		if resp, err := c.doer.Do(req); err == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				data, _ = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
			}
			_ = resp.Body.Close()
		}
	}
	r := parseRobots(data, c.ua)
	c.robots[u.Host] = r
	return r
}

// gateFor returns the per-host politeness gate, creating it on first use.
func (c *crawler) gateFor(host string) *hostGate {
	c.gatesMu.Lock()
	defer c.gatesMu.Unlock()
	g, ok := c.gates[host]
	if !ok {
		g = &hostGate{}
		c.gates[host] = g
	}
	return g
}

// wait blocks until this host may be fetched again, then reserves the next slot
// DelayMS ahead (SPEC-04 §2 per-host delay).
func (g *hostGate) wait(c *crawler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := c.cfg.DelayMS; d > 0 {
		if wait := g.next.Sub(c.now()); wait > 0 {
			c.sleep(wait)
		}
		g.next = c.now().Add(time.Duration(d) * time.Millisecond)
	}
}

// persistPending records a discovered-but-unfetched URL so a later run can resume
// from it. It is best-effort: a persistence hiccup degrades resumability but must
// not abort a live crawl.
func (c *crawler) persistPending(ctx context.Context, norm string, depth int) {
	if err := c.store.Upsert(ctx, Page{URL: norm, NormalizedURL: norm, Depth: depth}); err != nil {
		c.log.Warn("webcrawl: persist pending page failed", "url", norm, "err", err.Error())
	}
}

// recordError persists a fetch failure/non-2xx to crawl_pages (best-effort).
func (c *crawler) recordError(ctx context.Context, it item, status int, msg string) {
	_ = c.store.Upsert(ctx, Page{
		URL: it.raw, NormalizedURL: it.norm, Depth: it.depth,
		Fetched: true, Status: status, Err: truncate(msg, 500),
	})
}

// mimeOf strips any parameters from a Content-Type header, returning the bare MIME.
func mimeOf(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(strings.ToLower(ct))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
