# ISSUE-0007: Go parsers — HTML, Markdown, text, CSV, JSON

**Type:** Feature · **Status:** Done · **Story:** STORY-05.2 · **Traces:** FR-ING-01, SPEC-05 §1–2, ADR-0034

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.2 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Implement the native-Go half of the ingestion parse stage (SPEC-05 §2): turn the
raw bytes of the five formats a Go process can read (HTML, Markdown, plain text,
CSV, JSON) into the `Normalised{Title, Blocks}` representation the normalise/hash
step (SPEC-05 §1) and the structure-aware chunker (STORY-05.4) consume. The heavy
formats (PDF/DOCX/PPTX/XLSX) are handled by the Python sidecar (ADR-0006,
STORY-05.3) and are intentionally out of scope here.

## Scope
- New `internal/ingest/parse` package: pure, stateless, no tenant storage, no
  network (FR-ING-01).
- `Normalised{Title, Blocks}` / `Block{Type, Level, Text, Rows}` mirroring the
  sidecar JSON exactly so a Go parser and the sidecar feed one chunker (ADR-0034).
- A `Registry` mapping a canonical MIME type to a `Parser`, with `Default()` wiring
  the five Go formats; `ErrUnsupportedMIME` sentinel for the sidecar/unknown types.
- HTML: boilerplate removed (readability-style), headings preserved, tables → GFM
  markdown (story AC).
- Golden-file tests for ≥10 representative pages/documents (story AC).

## Resolution
- `parse.go`: `Normalised`, `Block`, `BlockType`, `Parser`, `Registry`,
  `NewRegistry`/`Register`/`Parse`, `Default`, `canonicalMIME`, and the shared
  `markdownTable` / `collapseWS` helpers; `Normalised.Markdown()` re-renders blocks
  to the SPEC-05 §1 normalised (hashable) text.
- `html.go`: content-root selection (`<main>`/`<article>`/`<body>`), semantic
  chrome-skip heuristic (nav/aside, body-level header/footer, script/style/form, and
  a class/id/role boilerplate regex), inline→markdown rendering, tables→GFM, and a
  whole-`<body>` fallback so content is never dropped (ADR-0034). Uses only
  `golang.org/x/net/html` — no external readability/markdown dependency.
- `markdown.go`: line scanner (ATX headings, fenced code, pipe tables, lists,
  paragraphs). `text.go`: blank-line-delimited paragraphs. `csv.go`: rows → one
  markdown table. `json.go`: uniform array of flat objects → table (sorted-union
  columns for determinism), else flattened `key.path[i]: value` code block.
- No HTTP route/OpenAPI change and no schema/migration change (a pure library
  package), so the contract and drift guards stay green.

## Bug fixed in this story
The markdown scanner's ATX-heading branch appended the heading but **never advanced
the line cursor**, so any `#`-led document (e.g. the `changelog.md` fixture) spun in
an infinite loop — the golden-generation run hung until killed. Every other branch
advanced the cursor; the heading branch now does too (`markdown.go`). Caught by the
golden suite the moment goldens were generated.

## Verification
- `go test ./internal/ingest/parse` green: `TestGolden` (11 fixtures →
  `testdata/golden/*.json`, `-update`-regenerable, asserts ≥10 fixtures) plus
  targeted tests — HTML boilerplate removed / headings preserved / tables→markdown,
  markdown headings+title+code, CSV→table, JSON records→table, JSON nested→
  key-value, text paragraphs, unsupported-MIME sentinel, MIME-param stripping.
- Goldens eyeballed: boilerplate (nav/ads/cookie/footer) gone while article +
  heading tree survive; tables carry structured `rows` + GFM `text`; nested lists
  indented; fenced/`<pre>` code captured.
- `gofmt` clean; module-wide `go test ./...` otherwise unchanged.

## Notes / not in scope
- The sidecar parser + Go client (STORY-05.3) covers PDF/DOCX/PPTX/XLSX behind the
  same `Normalised` shape and the `ErrUnsupportedMIME` routing seam.
- The chunker (STORY-05.4), embedder (STORY-05.5) and sink orchestration + job
  stats (STORY-05.6/05.7) consume `Normalised`; they are not built here.
- Ceiling (ponytail, ADR-0034): the HTML heuristic is semantic, not density-scored,
  so an unmarked comments section or article-in-a-bare-`<div>` may keep some chrome
  — extra paragraphs, never lost content.
- Pre-existing local check noise (documented in ISSUE-0003/0006): env-sensitive
  `internal/cli` `*RequiresURL` tests fail under mise `.env` injection (a
  `DATABASE_URL` is present); unrelated to this package, which tests green.
