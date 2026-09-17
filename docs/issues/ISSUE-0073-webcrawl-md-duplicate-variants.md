# ISSUE-0073: Web crawl ingests `.md` duplicate variants, doubling the corpus

**Type:** Bug · **Status:** Mitigated · **Priority:** Medium · **Traces:** FR-SRC-01, SPEC-04 §2

## Resolution
Mitigated by configuration, no code change: the crawler already supports a `deny` URL list
(`internal/connector/webcrawl/crawl.go` `denied`). `deny: [".md"]` was applied to the acme source, so
the next crawl no longer follows the Markdown export variants — halving the crawl and removing the
duplicate documents. The longer-term code enhancements below (suffix/regex deny; canonical-variant
de-dup) remain optional and are deferred; they are not required to solve the reported case.

## Summary
Crawling a GitBook-style site (manual.tourpaq.com) ingests **every page twice**: once as the HTML
page (`/2-factor-authentication`) and once as its Markdown export (`/2-factor-authentication.md`). The
two carry the same content, so retrieval is polluted with near-duplicate chunks and the crawl budget
(`max_pages`) and time are spent twice over.

## Observed
- ~468 canonical pages (`sitemap-pages.xml`, `llms.txt`) → **~960 documents** crawled (HTML + `.md`).
- `max_pages: 1000` is effectively halved to ~500 real pages; on a large site the crawl never reaches
  everything, and the extra fetches roughly double crawl time (compounding ISSUE-0072's timeout).
- Duplicate chunks skew retrieval ranking and citations toward whichever variant scored marginally
  higher.

## Immediate mitigation (no code — already supported)
The crawl config supports a `deny` list (substring match, `internal/connector/webcrawl/crawl.go:542`).
Add `".md"` to `deny` on the source to stop following the Markdown variants:
```json
{ "deny": [".md"] }
```
This halves the crawl and removes the duplicates. Applied to the acme source; see the Sources → Edit
form to set it on any crawl source.

## Proposed longer-term fix (options)
1. **Anchored/suffix deny:** `deny` is substring-only today (the comment flags "a future story can add
   anchored/regex rules"). A suffix/regex form (`\.md$`) is safer than a `.md` substring, which could
   over-match a slug containing `.md`.
2. **Canonical-variant de-dup:** when two URLs differ only by a `.md`/export suffix (or via
   `<link rel="canonical">` / `rel="alternate"`), keep one. Handles GitBook and similar exporters
   without per-source config.
3. **Prefer HTML over export variants** by default, since the HTML is the human-canonical page.

## Tests
- Crawl unit test: with `deny:[".md"]`, `.md` links are not enqueued; HTML pages still are.
- (If option 2) de-dup test: `/x` and `/x.md` collapse to one document.

## Related
- ISSUE-0072 (truncated crawl deletion — `.md` doubling makes the timeout more likely to fire).
