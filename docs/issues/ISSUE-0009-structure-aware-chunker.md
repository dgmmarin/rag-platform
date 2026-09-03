# ISSUE-0009: Structure-aware chunker

**Type:** Feature · **Status:** Done · **Story:** STORY-05.4 · **Traces:** FR-ING-03, FR-ING-04, SPEC-05 §3, ADR-0036

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.4 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Split a parsed document (`parse.Normalised`) into retrieval chunks per SPEC-05 §3:
respect headings, keep tables/code intact, configurable target/overlap, populated
`heading_path`, and a context line prepended to the embedded text (not the stored
content).

## Scope
- New `internal/ingest/chunk`, pure (blocks in, chunks out), consuming the shared
  `parse.Normalised` (so the Go-parser and sidecar producers chunk identically).
- Output feeds `documents.ChunkInput` (STORY-05.1) once embedded (STORY-05.5) and
  mapped by the sink (STORY-05.6).

## Resolution
- `chunk.go`: `Chunk{Position, HeadingPath, Content, EmbedText, TokenCount}`,
  `Config{TargetTokens=512, OverlapTokens=64, Count}`, `Document(n, cfg) []Chunk`.
  - Prose accumulates to `TargetTokens`; a heading flushes the current chunk and
    re-roots `HeadingPath`, so every chunk carries exactly one path.
  - Tables/code are atomic (flushed clear of prose, never split) unless a block
    alone exceeds 2×target, when it splits on row (header repeated) or line
    boundaries — no row or line is broken.
  - Consecutive prose chunks overlap by `OverlapTokens` (tail words of the previous
    chunk); the seed counts toward the next budget so the size bound holds.
  - `EmbedText` = `"{title} > {heading path}"` + `Content` (empty and
    consecutive-duplicate segments dropped, so title==H1 does not repeat);
    `Content` never carries the context line.
  - Token counting via an injectable `Config.Count` defaulting to a regex
    approximation (SPEC-05 §3 permits approximation; no tiktoken dependency).
- `parse.RenderTable` exported (thin shim over the existing `markdownTable`) so the
  chunker can re-render split table parts.

## Verification
- `go test ./internal/ingest/chunk` green, including the headline **property test**
  (`TestSizeBoundsProperty`, 50 randomised documents): with every input block below
  the target, no chunk exceeds the target and positions are dense/ordered.
- Targeted tests: heading_path respected across H1/H2/H3, context line on embed text
  only (+ title==H1 dedupe), tables/code kept intact, target/overlap configurable
  with the overlap tail carried, oversize table split by rows (header repeated) and
  oversize code split by lines (order preserved), blank blocks → no chunks,
  `ApproxTokens`.
- `gofmt` clean; `golangci-lint` 0 issues on the package; `parse` tests re-run green
  after the `RenderTable` export.

## Notes / not in scope
- Ceiling (ponytail, ADR-0036): the token count is an approximation (undercounts long
  words vs a real BPE) and overlap is word-granular — acceptable per SPEC-05 §3;
  inject a tiktoken `cl100k_base` counter via `Config.Count` to tighten it.
- The embedder (STORY-05.5) consumes `EmbedText`; the sink (STORY-05.6) supplies
  target/overlap from tenant settings and maps `Chunk`+embedding→`documents.ChunkInput`.
- No schema/OpenAPI/migration change (`chunks` already exists, ADR-0008).
