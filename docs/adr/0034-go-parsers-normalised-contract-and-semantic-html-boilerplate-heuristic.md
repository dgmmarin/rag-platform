# ADR-0034: Go parsers — shared Normalised contract, registry dispatch, and a semantic HTML boilerplate heuristic

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-ING-01, FR-ING-11, SPEC-05 §1–2, NFR-MNT-01 · **Decisions:** ADR-0006, ADR-0008, ADR-0033

## Context
STORY-05.2 delivers the native-Go half of the parse stage (SPEC-05 §1–2): turn
the raw bytes of the five formats a Go process can read (HTML, Markdown, plain
text, CSV, JSON) into the normalised representation the rest of the ingestion
pipeline consumes. The heavy formats (PDF/DOCX/PPTX/XLSX) are parsed by the Python
sidecar over `POST /parse` (ADR-0006, STORY-05.3), which returns
`{title, blocks:[{type, level, text, rows}]}`. Downstream, the normalise/hash step
(SPEC-05 §1) and the structure-aware chunker (SPEC-05 §3, STORY-05.4) walk that
block list maintaining a `heading_path`. The parsers are pure functions over an
in-memory body (documents are bounded by `MAX_UPLOAD_BYTES`, STORY-04.4); they
touch neither tenant storage nor the network.

## Options
- **Output shape.** (a) each parser returns its own format-specific tree — rejected:
  the chunker would need one walker per format and the Go and sidecar paths would
  diverge. (b) **A single `Normalised{Title, Blocks}` mirroring the sidecar JSON
  exactly** (chosen): the same closed block set (`heading`/`paragraph`/`table`/
  `list`/`code`), tables carried as both structured `rows` *and* GFM markdown
  `text`, headings carrying a 1–6 `level`. A Go parser and the sidecar are then
  interchangeable to one chunker (NFR-MNT-01). `Normalised.Markdown()` re-renders
  the blocks to the normalised text SPEC-05 §1 hashes, so a document authored in
  any format hashes through one code path.
- **Dispatch.** A `Registry` maps a *canonical* MIME type to a `Parser`, and
  `Default()` wires the five Go formats. The caller passes the MIME already
  resolved by the upload allowlist / content-type detection (STORY-04.4); parsers
  never sniff. Parameters (`; charset=…`) are stripped before lookup. The sidecar
  formats are deliberately **absent** from `Default()` — an unregistered MIME
  returns `ErrUnsupportedMIME` (a sentinel, `errors.Is`-matchable), which the sink
  routes to the sidecar rather than failing the document.
- **HTML boilerplate removal.** The AC is "HTML boilerplate removed (readability),
  headings preserved, tables → markdown". Options: (a) a maintained Go readability
  port with full Mozilla-style text-density scoring — rejected: the actively
  maintained ports force a newer Go toolchain than the repo pins, and pull a
  transitive tree for one stage; (b) an html-to-markdown library — rejected: it
  collapses the heading tree and table structure we specifically need as blocks;
  (c) **a semantic heuristic over `golang.org/x/net/html`** (an already-present,
  transitive stdlib-adjacent dependency) (chosen): pick the `<main>`/`<article>`
  content root, else `<body>`; skip known-chrome subtrees (nav/aside, and
  header/footer at body level, plus script/style/form and a class/id/role token
  regex — cookie/consent/advert/share/related/comment/…); walk the rest into
  blocks. If chrome removal empties the document, fall back to the whole `<body>`
  so content is never dropped.
- **Markdown/text/CSV/JSON.** Markdown and plain text share a line scanner (ATX
  headings, fenced code, pipe tables, lists; text degrades to blank-line-delimited
  paragraphs) — a full CommonMark parser buys inline richness the chunker does not
  use. CSV → one markdown table. JSON → a table when the top level is a uniform
  array of flat objects (columns are the sorted union of keys, for determinism),
  otherwise a flattened `key.path[i]: value` code block.

## Decision
Add `internal/ingest/parse`: `Normalised{Title, Blocks}` and `Block{Type, Level,
Text, Rows}` (JSON-tagged to match the sidecar), a `Parser` interface, a `Registry`
with canonical-MIME dispatch and an `ErrUnsupportedMIME` sentinel, and `Default()`
registering `htmlParser`, `markdownParser`, `textParser`, `csvParser`,
`jsonParser`. HTML uses the semantic content-root + chrome-skip heuristic above;
Markdown/text use a shared line scanner; CSV/JSON map rows/records to a markdown
table or flattened key-value text. All parsers are pure and stateless.

## Consequences
- The chunker (STORY-05.4) and the sink (STORY-05.6) depend only on `Normalised`,
  so the Go parsers and the Python sidecar are drop-in alternatives behind one MIME
  dispatch; adding a format is registering a `Parser` (NFR-MNT-01).
- **Ceiling (ponytail):** the HTML heuristic is semantic, not density-scored — a
  page that buries its article in an unmarked `<div>` beside sibling chrome `<div>`s
  keeps some chrome, and an unmarked comments section survives. The cost is extra
  *paragraphs*, never lost article content (the `<body>` fallback guarantees it).
  Upgrade path: add text-density scoring, or adopt a maintained readability library
  once one no longer forces a toolchain bump. This is recorded in-code and exercised
  by the golden fixtures.
- No new module dependency beyond `golang.org/x/net/html` (already transitive), no
  network, no tenant storage, no schema change — nothing for the drift or coverage
  gates to react to.
- Acceptance is a golden-file suite over 11 representative fixtures
  (`testdata/golden/*.json`) plus targeted tests (boilerplate removal, heading
  preservation, table→markdown, CSV, JSON records/nested, text paragraphs, registry
  dispatch and unsupported-MIME), regenerable with `-update`.
