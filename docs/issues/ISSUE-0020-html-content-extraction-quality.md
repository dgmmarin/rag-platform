# ISSUE-0020: HTML content extraction quality

**Type:** Feature · **Status:** Done · **Story:** STORY-07.3 · **Traces:** FR-SRC-05, SPEC-04 §2/§2b, ADR-0045 (Decisions: ADR-0034, ADR-0043)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.3 for traceability; the backlog story
> remains the authoritative work item. STORY-07.3 advances EPIC-07 to 16/39.

## Summary
Add quality HTML content extraction to the `web_crawl` connector behind the
STORY-07.1 parse seam: per-source `include_selectors`/`exclude_selectors` (CSS), a
readability fallback, `<title>`→`og:title`→`<h1>` title precedence, and HTML→markdown.
An HTML page is now emitted as `Document.Text` (markdown, `MimeType`
`text/markdown`); non-HTML within the allowlist still passes as raw `Body` for the
parse sidecar.

## Scope
- **`internal/connector/webcrawl/content.go` (new)** — `extractContent(body,
  include, exclude)`: compile CSS selectors with `cascadia`; drop `exclude`
  subtrees; keep top-most `include` matches, else fall back to the whole
  exclude-applied document; render to markdown by reusing the platform's existing
  semantic HTML engine `parse.htmlParser` (ADR-0034) via
  `parse.Default().Parse("text/html", …)`.
- **`extract.go`** — title precedence `<title>` → `og:title` → first `<h1>`,
  collected in the existing single crawl-structure walk.
- **`crawl.go` `emit`** — for HTML with usable extracted content, set
  `Document.Text` = markdown, `MimeType` = `text/markdown`, `Body` = nil; raw `Body`
  path preserved for non-HTML and for the empty-extraction fallback.
- **Dependency** — `github.com/andybalholm/cascadia v1.3.2` (only new dependency;
  needs only the vendored `golang.org/x/net/html`; `go 1.16` — no toolchain bump).
  See ADR-0045 for why NOT `go-readability`/`html-to-markdown` (ADR-0034 + the
  `go 1.22` pin).
- **Golden corpus** — `testdata/corpus/` (20 synthetic-but-representative pages,
  `manifest.json`, committed `expected/*.md` baselines) + `TestGoldenCorpus` with a
  reproducible boilerplate-removal metric.

## Out of scope
Conditional fetch/304 (07.4), sitemap (07.5), HTTP API connector (07.6/07.7), SSRF
(07.2, done). No schema, no migration, no OpenAPI change. `parse.htmlParser` is
reused unchanged (no edits outside the `webcrawl` package + one `go.mod` line).

## Acceptance / DoD evidence
- TDD: RED unit tests for `extractContent` (include/exclude/fallback), title
  precedence, and the emit-as-markdown path preceded the implementation.
- `TestGoldenCorpus` over 20 pages: **mean boilerplate removal = 1.00** (threshold
  0.90), **content retention = 1.00** (no page drops a content marker). Hermetic —
  no network, no DB.
- `go vet ./...` clean (lint bar; golangci-lint binary unavailable here). Build
  clean. `webcrawl` coverage 81.5% (≥ 70% gate).
- Metric definition and human-spot-review caveat recorded in ADR-0045 and SPEC-04
  §2b.

## Caveat
The corpus is synthetic and known by construction; the committed baselines have not
had a human review pass in this run. A human should spot-review the corpus once
before it is trusted as the long-term regression baseline (ADR-0045).
