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
	norm    string // normalised URL — the visited/crawl_pages key
	raw     string // URL actually requested (pre-normalisation)
	depth   int
	lastmod time.Time // sitemap <lastmod>, zero for web_crawl (STORY-07.5 incremental skip)
}

// seed is a depth-0 frontier entry supplied by the connector before the BFS runs.
// web_crawl derives its seeds from start_urls (lastmod zero); the sitemap connector
// derives them from parsed sitemap XML, carrying each URL's <lastmod> (STORY-07.5).
// A non-nil crawler.seeds tells run to use these instead of cfg.StartURLs — the one
// seam the two connectors differ on for frontier seeding.
type seed struct {
	norm    string
	raw     string
	lastmod time.Time
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
	// full is SyncRun.Full: a full sync keeps the STORY-07.1 resume-skip; an
	// incremental sync re-visits fetched pages conditionally (STORY-07.4).
	full bool
	// prior is the crawl_pages state loaded at run start, keyed by normalised URL —
	// the source of a page's prior ETag/Last-Modified/content-hash for conditional GET.
	prior map[string]Page

	allowPrefixes []string
	seedHosts     map[string]bool

	// seeds, when non-nil, is the depth-0 frontier the connector supplies (STORY-07.5
	// sitemap); nil means "derive seeds from cfg.StartURLs" (web_crawl). followLinks
	// gates BFS frontier expansion: web_crawl follows discovered links, the sitemap
	// connector does NOT (its frontier is exactly the sitemap's URLs).
	seeds       []seed
	followLinks bool

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
// followLinks defaults true (the web_crawl behaviour); the sitemap connector clears
// it via newSitemapCrawler.
func newCrawler(cfg config, doer Doer) *crawler {
	return &crawler{
		cfg:         cfg,
		doer:        doer,
		ua:          userAgent,
		maxBytes:    maxResponseBytes,
		now:         time.Now,
		sleep:       func(d time.Duration) { time.Sleep(d) },
		followLinks: true,
		log:         discardLogger(),
		visited:     map[string]bool{},
		emitted:     map[string]bool{},
		robots:      map[string]*robotsRules{},
		gates:       map[string]*hostGate{},
	}
}

// discardLogger is a no-op slog logger used as the crawler's default until run wires
// the run-scoped logger; it also keeps pre-run helpers (sitemap seed collection)
// safe to log through.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// run performs the crawl: it resolves the resume state, seeds the frontier, and
// walks it breadth-first honouring depth/pages limits, allow/deny, robots and the
// per-host delay, emitting each fetched page into the sink. It always calls
// sink.Complete at the end.
func (c *crawler) run(ctx context.Context, sr connector.SyncRun, sink connector.Sink) (connector.Stats, error) {
	c.log = sr.Log
	if c.log == nil {
		c.log = discardLogger()
	}
	c.limiter = sr.Limiter
	c.full = sr.Full

	// Resolve the PageStore capability of the run's State (SPEC-04 §2). Absent it,
	// fall back to a non-resumable in-memory store and warn.
	if ps, ok := sr.State.(PageStore); ok && ps != nil {
		c.store = ps
	} else {
		c.store = newMemPageStore()
		c.log.Warn("webcrawl: state store has no crawl-page persistence; crawl is not resumable")
	}

	c.prepareAllow()

	// Load persisted state for resumability and conditional fetch (STORY-07.4 reads
	// the prior ETag/Last-Modified/content-hash from here).
	loaded, err := c.store.Load(ctx)
	if err != nil {
		return c.stats, err
	}
	c.prior = loaded

	frontier := map[int][]item{}
	alreadyFetched := map[string]bool{}
	add := func(norm, raw string, depth int, lastmod time.Time) {
		if c.visited[norm] {
			return
		}
		c.visited[norm] = true
		frontier[depth] = append(frontier[depth], item{norm: norm, raw: raw, depth: depth, lastmod: lastmod})
	}

	// Seeds (depth 0). The frontier source is the one seam the two connectors differ
	// on: a non-nil c.seeds (the sitemap connector) supplies the URLs directly; else
	// they are derived from cfg.StartURLs (web_crawl). Either way seeds are explicitly
	// configured, so they bypass the allowlist but still respect deny.
	seeds := c.seeds
	if seeds == nil {
		for _, s := range c.cfg.StartURLs {
			if norm, u, err := parseAndNormalize(s); err == nil {
				seeds = append(seeds, seed{norm: norm, raw: u.String()})
			}
		}
	}
	for _, s := range seeds {
		if c.denied(s.norm) {
			continue
		}
		add(s.norm, s.raw, 0, s.lastmod)
	}

	// Merge persisted pages. Pending pages (discovered but not fetched) are always
	// re-queued at their recorded depth — this is what makes an interrupted crawl
	// resume instead of restarting. Previously-fetched pages depend on the sync mode
	// (STORY-07.4):
	//   - FULL sync: skipped (the STORY-07.1 resume-skip); deletion detection is on,
	//     and re-seeing a page without re-emitting it would need a sink "mark seen"
	//     signal that the connector.Sink interface does not have (an EPIC-09 concern),
	//     so a full sync keeps re-emitting what it fetches. See ADR-0046.
	//   - INCREMENTAL sync: re-queued for a CONDITIONAL re-visit, so a scheduled
	//     re-crawl detects changes cheaply (a 304 or an identical content hash costs
	//     no parse/emit). Complete is a no-op on an incremental sink, so skipping an
	//     unchanged page can never soft-delete it (the deletion-detection reconciliation).
	for norm, p := range loaded {
		if p.Fetched && c.full {
			alreadyFetched[norm] = true
		}
		if c.visited[norm] {
			continue // already queued (e.g. a seed); a fetched seed is re-visited unless FULL
		}
		c.visited[norm] = true
		if p.Depth > c.cfg.MaxDepth {
			continue
		}
		if !p.Fetched || !c.full {
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
				// Frontier expansion is web_crawl-only: the sitemap connector clears
				// followLinks so a fetched page's links never enter the frontier
				// (STORY-07.5: the frontier is exactly the sitemap's URLs).
				if !c.followLinks || it.depth+1 > c.cfg.MaxDepth {
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

	// lastmod incremental skip (STORY-07.5, FR-SRC-06): on an incremental sync, if the
	// sitemap declares this URL's last-modified time is no newer than our last
	// successful fetch, skip it with NO request at all — cheaper than even the
	// STORY-07.4 conditional GET, which still costs a round trip. Only fires with a
	// sitemap-supplied lastmod and a recorded prior fetch time; web_crawl items carry a
	// zero lastmod, so this is behaviour-preserving for the crawler. The page is counted
	// as seen (not changed); its crawl_pages row is left untouched (its prior
	// last_fetched_at stands — the page was not re-fetched).
	if !c.full && !it.lastmod.IsZero() {
		if p, ok := c.prior[it.norm]; ok && p.Fetched && !p.LastFetchedAt.IsZero() && !it.lastmod.After(p.LastFetchedAt) {
			c.mu.Lock()
			c.stats.DocsSeen++
			c.mu.Unlock()
			return nil, nil
		}
	}

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

	// Conditional fetch (STORY-07.4, FR-ING-02): on an incremental re-crawl, if
	// crawl_pages holds prior validators for this page, ask the server to answer
	// 304 Not Modified when it is unchanged. A conditional GET is one round trip and
	// carries no body when unchanged — strictly better than a separate HEAD+GET
	// (which is two round trips whenever the page HAS changed); see ADR-0046. Full
	// syncs never re-visit a fetched page (they skip it), so conditional headers
	// only fire on the incremental re-see path.
	prior, hasPrior := c.prior[it.norm]
	conditional := !c.full && hasPrior && prior.Fetched
	if conditional {
		if prior.ETag != "" {
			req.Header.Set("If-None-Match", prior.ETag)
		}
		if prior.LastModified != "" {
			req.Header.Set("If-Modified-Since", prior.LastModified)
		}
	}

	resp, err := c.doer.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.recordError(ctx, it, 0, err.Error())
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// 304 Not Modified: the page is unchanged. Do NOT read/parse/extract/emit it —
	// just record that it was re-seen (bump last_fetched_at, keep validators + hash).
	// This is the FR-ING-02 "unchanged pages cost a 304 and no parse" fast path.
	if resp.StatusCode == http.StatusNotModified {
		c.markUnchanged(ctx, it, prior, resp, 0)
		return nil, nil
	}

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

	// Content-hash change detection (STORY-07.4, the no-ETag case): many servers send
	// neither ETag nor Last-Modified, so a 304 is impossible and a 200 is unavoidable.
	// Compare the fetched bytes' hash to the stored one; identical content means the
	// page is unchanged — skip parse/emit (no re-ingest), just refresh last_fetched_at
	// (and any newly supplied validators). Only differing bytes are re-emitted. Gated
	// on an incremental re-visit (prior fetched state present).
	sum := sha256.Sum256(body)
	if conditional && len(prior.ContentHash) > 0 && bytes.Equal(sum[:], prior.ContentHash) {
		c.markUnchanged(ctx, it, prior, resp, len(body))
		return nil, nil
	}

	mime := mimeOf(resp.Header.Get("Content-Type"))
	links := c.emit(ctx, it, u, resp, body, mime, sink)

	// Persist fetched state (etag/last-modified/hash) so the NEXT crawl's request for
	// this page is conditional (STORY-07.4).
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

// markUnchanged records that a re-visited page was unchanged — a 304 Not Modified,
// or a 200 whose content hash matches the stored one (STORY-07.4). It bumps
// last_fetched_at and carries the prior status/content-hash forward, refreshing
// ETag/Last-Modified when the response supplied new ones, so the NEXT crawl stays
// conditional. It performs NO parse and NO emit; the page is counted as seen (not
// changed) in Stats. bodyLen is the fetched byte count (0 for a 304, which has no
// body). A persistence hiccup is logged, not fatal: it only degrades the next
// crawl's conditional-fetch efficiency, and must not abort a live crawl.
func (c *crawler) markUnchanged(ctx context.Context, it item, prior Page, resp *http.Response, bodyLen int) {
	p := prior
	p.URL = it.raw
	p.NormalizedURL = it.norm
	p.Depth = it.depth
	p.Fetched = true
	p.Err = ""
	if v := resp.Header.Get("ETag"); v != "" {
		p.ETag = v
	}
	if v := resp.Header.Get("Last-Modified"); v != "" {
		p.LastModified = v
	}
	if err := c.store.Upsert(ctx, p); err != nil {
		c.log.Warn("webcrawl: persist unchanged page failed", "url", it.norm, "err", err.Error())
	}
	c.mu.Lock()
	c.stats.DocsSeen++
	c.stats.BytesFetched += int64(bodyLen)
	c.mu.Unlock()
}

// emit sends one fetched page into the sink as a connector.Document. For HTML it
// parses the crawl structure (title, canonical, links), then runs the quality
// content extraction (STORY-07.3, content.go): include/exclude selectors and the
// readability fallback, rendered to markdown, which is emitted as the Document
// Text (SPEC-04 §2: "HTML → markdown") — NOT the raw Body. Non-HTML responses
// within the allowlist are passed through as the raw Body for the parse sidecar.
// The canonical URL, when present, becomes the ExternalID (SPEC-04 §2). Returns
// discovered links (HTML only).
func (c *crawler) emit(ctx context.Context, it item, u *url.URL, resp *http.Response, body []byte, mime string, sink connector.Sink) (links []string) {
	extID := it.norm
	title := ""
	markdown := ""
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
		markdown = extractContent(body, c.cfg.IncludeSelectors, c.cfg.ExcludeSelectors)
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
	// For HTML with usable extracted content, carry the clean markdown as Text and
	// drop the raw Body (connector.Document contract: "Body ... nil if Text set").
	// Empty extraction (unparseable HTML) falls back to the raw Body path so a
	// document's content is never lost.
	if isHTML && markdown != "" {
		doc.Text = markdown
		doc.Body = nil
		doc.MimeType = "text/markdown"
	} else if isHTML {
		// Extraction yielded nothing (unparseable/degenerate HTML): fall back to the
		// raw Body so content is never lost. Log identity only — never content.
		c.log.Debug("webcrawl: HTML extraction empty; emitting raw body", "external_id", extID)
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
