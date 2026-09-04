package webcrawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rag-platform/ragctl/internal/connector"
)

// Sitemap-tree bounds (SPEC-04 §3, STORY-07.5). A sitemap index can nest and fan out;
// these bound the recursion so a hostile or accidentally-huge tree cannot make the
// connector fetch/allocate without limit.
//
// ponytail: fixed ceilings, not configurable. sitemaps.org caps a single sitemap at
// 50 000 URLs / 50 MB and recommends splitting via an index; maxSitemapURLs mirrors the
// per-file cap as a whole-tree budget, maxSitemapDocs bounds the number of sitemap
// files fetched, and maxSitemapDepth bounds index nesting. Upgrade path if a tenant
// legitimately needs more: make these per-source config with the same fail-safe stop.
const (
	maxSitemapDepth = 5     // nested <sitemapindex> levels
	maxSitemapDocs  = 200   // total sitemap/index documents fetched per collection
	maxSitemapURLs  = 50000 // total page URLs collected per collection
)

// sitemapFile decodes either a <urlset> or a <sitemapindex> (the two sitemaps.org
// document types). encoding/xml matches child elements by local name regardless of
// namespace, so the standard xmlns is handled without namespace-qualified tags. A
// urlset populates URLs; a sitemapindex populates Sitemaps; both entry kinds carry a
// <loc> and an optional <lastmod>, so one entry type serves both.
type sitemapFile struct {
	XMLName  xml.Name       `xml:"-"`
	URLs     []sitemapEntry `xml:"url"`
	Sitemaps []sitemapEntry `xml:"sitemap"`
}

type sitemapEntry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

// collectSitemapSeeds fetches the given root sitemap URLs, recursively expands any
// <sitemapindex> entries into their child sitemaps, and returns the flat, de-duplicated
// list of page URL seeds carrying each URL's <lastmod>. It reuses the crawler's egress
// Doer and size cap (so the SSRF guard and 20 MB limit apply to sitemap fetches too),
// and transparently gunzips gzipped sitemaps. A single unreachable/malformed sitemap is
// logged and skipped (the rest still yield seeds); only context cancellation aborts. A
// completely invalid root sitemap URL is a hard error (a config mistake).
func (c *crawler) collectSitemapSeeds(ctx context.Context, roots []string) ([]seed, error) {
	var out []seed
	seenURL := map[string]bool{}        // page-URL dedupe (normalised)
	visitedSitemap := map[string]bool{} // sitemap-doc dedupe (guards cycles)
	docs := 0
	budgetHit := false

	var walk func(rawURL string, depth int)
	walk = func(rawURL string, depth int) {
		if budgetHit || ctx.Err() != nil {
			return
		}
		if depth > maxSitemapDepth {
			c.log.Warn("sitemap: index nesting exceeds bound; subtree skipped", "url", rawURL, "max_depth", maxSitemapDepth)
			return
		}
		if visitedSitemap[rawURL] {
			return
		}
		visitedSitemap[rawURL] = true
		if docs >= maxSitemapDocs {
			c.log.Warn("sitemap: document budget exhausted; further sitemaps skipped", "max_docs", maxSitemapDocs)
			budgetHit = true
			return
		}
		docs++

		body, err := c.fetchSitemap(ctx, rawURL)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("sitemap: fetch failed; skipped", "url", rawURL, "err", err.Error())
			return
		}
		var f sitemapFile
		if err := xml.Unmarshal(body, &f); err != nil {
			c.log.Warn("sitemap: parse failed; skipped", "url", rawURL, "err", err.Error())
			return
		}

		// A sitemap index points at child sitemaps: recurse into each.
		for _, s := range f.Sitemaps {
			if norm, _, err := parseAndNormalize(s.Loc); err == nil {
				walk(norm, depth+1)
			}
		}
		// A urlset lists page URLs: each becomes a depth-0 seed with its lastmod.
		for _, e := range f.URLs {
			norm, u, err := parseAndNormalize(e.Loc)
			if err != nil || seenURL[norm] {
				continue
			}
			if len(out) >= maxSitemapURLs {
				c.log.Warn("sitemap: URL budget reached; remaining URLs skipped", "max_urls", maxSitemapURLs)
				budgetHit = true
				return
			}
			seenURL[norm] = true
			out = append(out, seed{norm: norm, raw: u.String(), lastmod: parseSitemapTime(e.LastMod)})
		}
	}

	for _, r := range roots {
		norm, _, err := parseAndNormalize(r)
		if err != nil {
			return nil, fmt.Errorf("sitemap: invalid sitemap url %q: %w", r, err)
		}
		walk(norm, 0)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchSitemap does a plain GET of one sitemap document through the crawler's egress
// Doer (SSRF-guarded in production) with the User-Agent and the shared response-size
// cap, then transparently gunzips a gzipped body. It deliberately does NOT go through
// the crawl process()/emit path — a sitemap is metadata, not a document — but reuses
// the same egress seam, UA and size limit so no fetch policy is forked.
func (c *crawler) fetchSitemap(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %s", resp.Status)
	}
	limit := int64(c.maxBytes)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds size cap")
	}
	return maybeGunzip(body, limit), nil
}

// maybeGunzip decompresses body when it carries the gzip magic bytes (0x1f 0x8b),
// which real-world .xml.gz sitemaps use regardless of Content-Type/Content-Encoding.
// Decompression is bounded to cap bytes so a gzip bomb cannot exhaust memory; a
// corrupt/partial stream falls back to the raw bytes (the XML parse then fails and the
// sitemap is skipped with a warning).
func maybeGunzip(body []byte, limit int64) []byte {
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil || int64(len(out)) > limit {
		return body
	}
	return out
}

// sitemapTimeLayouts are the W3C-datetime / ISO-8601 forms the sitemaps.org spec
// permits for <lastmod>, from full timestamp down to date-only.
var sitemapTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02",
}

// parseSitemapTime parses a <lastmod> value, returning the zero time on any
// unparseable/empty value. A zero time means "no lastmod signal", so the incremental
// skip does not fire and the URL is fetched (conditionally, per STORY-07.4) — the safe
// default.
func parseSitemapTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range sitemapTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// newSitemapCrawler builds the shared crawl core configured for a sitemap sync: the
// same fetch/extract/conditional/pagestore engine as web_crawl, but with link
// following disabled and max_depth pinned to 0 (the frontier is exactly the sitemap's
// URLs). Tests build one directly with an injected Doer, exactly as the crawl tests do
// with newCrawler.
func newSitemapCrawler(c config, doer Doer) *crawler {
	cr := newCrawler(c.withSitemapDefaults(), doer)
	cr.followLinks = false
	return cr
}

// runSitemap seeds the crawl core's frontier from the parsed sitemap(s) and runs it.
// It is the sitemap connector's entry into the shared engine.
func (c *crawler) runSitemap(ctx context.Context, roots []string, sr connector.SyncRun, sink connector.Sink) (connector.Stats, error) {
	if sr.Log != nil {
		c.log = sr.Log
	}
	seeds, err := c.collectSitemapSeeds(ctx, roots)
	if err != nil {
		return connector.Stats{}, err
	}
	c.seeds = seeds
	return c.run(ctx, sr, sink)
}
