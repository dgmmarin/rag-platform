# ADR-0045: HTML content extraction — CSS include/exclude selectors, engine reuse over new readability/markdown dependencies, and a synthetic golden corpus

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-05, SPEC-04 §2/§2b, NFR-MNT-01, C-2 · **Decisions:** ADR-0034, ADR-0043

## Context
STORY-07.1 (ADR-0043) shipped the web crawler with the *extraction* seam left
open: an HTML page was emitted to the sink as its raw `Body`, and only
title/canonical/links were parsed. STORY-07.3 must add quality content extraction
(FR-SRC-05, SPEC-04 §2): per-source `include_selectors`/`exclude_selectors`, a
readability fallback, `<title>`→`og:title`→`<h1>` title precedence, HTML→markdown,
and a 20-page golden corpus proving ≥ 90 % boilerplate removal (manual review once,
then regression).

A prior decision constrains the design. **ADR-0034** already chose the platform's
HTML boilerplate-removal engine for the ingestion parse stage: a semantic
content-root + chrome-skip heuristic over `golang.org/x/net/html`
(`internal/ingest/parse` `htmlParser`), producing the shared `Normalised{Title,
Blocks}` and a markdown rendering. ADR-0034 explicitly **rejected** (a) a maintained
Go readability port — "the actively maintained ports force a newer Go toolchain than
the repo pins" — and (b) an html-to-markdown library — "it collapses the heading
tree and table structure we specifically need". Its recorded upgrade path was to
adopt a readability library "once one no longer forces a toolchain bump".

That condition still does not hold: `github.com/go-shiori/go-readability` HEAD
requires `go 1.23` (mise pins `go 1.22`) **and** is now deprecated; the newest
`html-to-markdown/v2` requires `go 1.25`. Pulling either would force a
GOTOOLCHAIN download and contradict ADR-0034.

## Options
- **Extraction engine.** (a) Add `go-readability` + `html-to-markdown` as SPEC-04 §2
  literally names — rejected: contradicts ADR-0034, forces a Go-toolchain bump past
  the `go 1.22` pin, and adds a large transitive tree (goquery/goldmark/termenv/…)
  for a second, divergent extraction path. (b) Hand-roll a readability scorer —
  rejected: genuinely hard to do well, and unnecessary. (c) **Reuse the existing
  `parse.htmlParser` engine (ADR-0034) for readability + markdown, and add only a
  CSS-selector layer** (chosen): the platform keeps ONE extraction engine; the
  connector's genuinely new capability (per-source selectors) is the only new code
  and the only new dependency.
- **CSS selectors.** `github.com/andybalholm/cascadia` **v1.3.2** (chosen): compiles
  CSS selectors against `golang.org/x/net/html` nodes — the exact node type the
  crawler and `parse.htmlParser` already use — and its only requirement is the
  already-vendored `x/net` (`go 1.16` directive, no toolchain bump). Alternatives:
  `goquery` (rejected: wraps cascadia and pulls a heavier API we do not need);
  hand-rolling a selector engine (rejected: CSS selector parsing is fiddly and
  cascadia is tiny and battle-tested — the lazy-correct rung is the maintained lib).
- **Document shape.** For HTML, emit the extracted markdown as `Document.Text`
  (`MimeType` `text/markdown`) and nil the `Body`, per the `connector.Document`
  contract ("Body … nil if Text set"); non-HTML within the allowlist still passes as
  raw `Body` for the parse sidecar. An empty extraction (unparseable HTML) falls back
  to the raw `Body` path so content is never lost.
- **Corpus & metric.** A real human "manual review once" cannot happen in an
  automated run. (a) Scrape real pages — rejected: non-hermetic, non-deterministic,
  licensing. (b) **A synthetic-but-representative corpus whose content/boilerplate
  split is known by construction** (chosen): 20 pages across five archetypes
  (blog/docs/news/product/knowledge-base) with realistic chrome (header/nav,
  sidebar, cookie banner, ad slots, comments, footer) around a known article, each
  region tagged with sentinel marker tokens. The metric is the mean fraction of
  known chrome markers ABSENT from the output, **guarded by a content-retention ==
  1.0 assertion** so a high removal score can never be earned by dropping content
  (the degenerate "extract nothing" cheat). Committed markdown baselines pin the
  output for deterministic regression.

## Decision
Add `internal/connector/webcrawl/content.go`: `extractContent(body, include,
exclude)` compiles the CSS selectors with cascadia, drops `exclude` subtrees, keeps
top-most `include` matches (or falls back to the whole exclude-applied document),
and renders to markdown by reusing `parse.Default().Parse("text/html", …)`
(the ADR-0034 `htmlParser`). Extend `extract.go` for the `<title>`→`og:title`→`<h1>`
title precedence. `crawl.go` `emit` now emits HTML as `Document.Text` markdown with
`MimeType` `text/markdown` (raw `Body` for non-HTML, and as the empty-extraction
fallback). Add `github.com/andybalholm/cascadia v1.3.2` — the only new dependency.
Acceptance is the 20-page golden corpus (`testdata/corpus/`, `manifest.json`,
`expected/*.md`) and `TestGoldenCorpus` computing the boilerplate-removal metric.

**Deviation from SPEC-04 §2 (recorded).** The spec prose said "then
`html-to-markdown`". SPEC-04 §2/§2b are updated in this change to reflect the
engine-reuse decision. This ADR is the *why*; the code and spec ship together
(NFR-MNT-04).

## Consequences
- One extraction engine platform-wide: crawled HTML and uploaded HTML normalise
  through the same `parse.htmlParser`, so behaviour and future improvements are
  shared, not forked.
- Adding the connector's selector capability touched only the `webcrawl` package
  plus one `go.mod` line (NFR-MNT-01); no schema, no OpenAPI, no `internal/cli`
  change. The crawl config already carried `include_selectors`/`exclude_selectors`
  (STORY-07.1 schema), so no config-contract change either.
- No Go-toolchain bump: cascadia v1.3.2 (`go 1.16`) needs only the vendored
  `x/net`. Pinned exactly; `x/*` versions unchanged.
- **Ceiling (ponytail):** the readability fallback is ADR-0034's semantic heuristic,
  not text-density scoring — a page burying its article in an unmarked `<div>` beside
  sibling chrome `<div>`s (and no configured `include_selectors`) keeps some chrome.
  The corpus exercises exactly this case via the knowledge-base archetype, which is
  why those pages carry an `include` selector. Upgrade path is unchanged from
  ADR-0034 (density scoring, or a readability lib once one stops forcing a toolchain
  bump).
- **Corpus caveat (human review):** the 20 fixtures are synthetic and known by
  construction; the committed `expected/*.md` baselines and the marker split have NOT
  been reviewed by a human in this run. A human should spot-review the corpus once
  and confirm it is representative before it is trusted as the long-term regression
  baseline; thereafter it is pure regression. Measured mean boilerplate removal =
  1.00 (threshold 0.90), content retention = 1.00 across all 20 pages.
