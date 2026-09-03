# ADR-0036: Structure-aware chunker — heading-bounded chunks, atomic tables/code, embed-only context line, injectable approximate tokeniser

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-ING-03, FR-ING-04, SPEC-05 §3 · **Decisions:** ADR-0008, ADR-0034, ADR-0035

## Context
STORY-05.4 turns a parsed document (`parse.Normalised{Title, Blocks}`, produced by
the Go parsers ADR-0034 or the sidecar ADR-0035) into retrieval chunks per SPEC-05
§3: walk blocks maintaining a `heading_path`, accumulate prose to a token target,
keep tables/code intact, carry a token overlap, and prepend a context line to the
*embedded* text (not the stored text). The store side (STORY-05.1, ADR-0008) already
defines `documents.ChunkInput{Position, HeadingPath, Content, TokenCount,
Embedding, EmbeddingModel}`; the chunker feeds it, but embeddings (STORY-05.5) and
the mapping to `ChunkInput` (the sink, STORY-05.6) come later.

## Options
- **Output type.** (a) emit `documents.ChunkInput` directly — rejected: it carries
  an `Embedding` the chunker cannot produce, and it has no place for the
  embed-only context line. (b) **a dedicated `chunk.Chunk`** (chosen) carrying both
  `Content` (stored verbatim) and `EmbedText` (`Content` prefixed with the context
  line). The embedder embeds `EmbedText`; the store persists `Content`; the sink
  maps `Chunk`+embedding→`ChunkInput`. This is the only type that can hold SPEC-05
  §3's two distinct texts.
- **Heading handling.** SPEC-05 §3 stores one `heading_path` per chunk. (a) let a
  chunk span heading boundaries and record the last path — rejected: the path would
  misdescribe earlier content. (b) **a heading flushes the current chunk and
  re-roots the path** (chosen): every chunk falls under exactly one `heading_path`
  (AC "respects headings"). Headings update the path but are not themselves emitted
  as content — the path column already carries them, avoiding duplication. Trade-off:
  a document of many tiny sections yields many small chunks; overlap and the context
  line keep those retrievable, and merging tiny sections is a future refinement.
- **Tables and code.** Atomic: flushed clear of prose and never split — *unless a
  block alone exceeds twice the target* (SPEC-05 §3), when it is split on safe
  boundaries only: table **rows** (the header row repeated on each part, so no row
  is broken) or code **lines**. Normal-size tables/code are emitted whole.
- **Overlap.** The trailing ~`overlap_tokens` words of a flushed prose chunk seed
  the next chunk (same heading section only). The seed counts toward the next
  chunk's budget, so the target bound still holds; overlap is capped below the
  target (fail-closed) so a chunk can never be seeded to its own limit.
- **Token counting.** SPEC-05 §3 names tiktoken `cl100k_base` but explicitly allows
  an approximation. (a) depend on a Go tiktoken port + embedded/downloaded BPE
  vocab — rejected for one heuristic count (a heavy dependency and a data file). (b)
  **an injectable `Config.Count` defaulting to a regex approximation** (chosen:
  each alphanumeric run and each symbol is one token). No dependency; a real
  tiktoken counter can be injected later without touching the split logic.

## Decision
Add `internal/ingest/chunk`: `Chunk{Position, HeadingPath, Content, EmbedText,
TokenCount}`, `Config{TargetTokens=512, OverlapTokens=64, Count}`, and
`Document(parse.Normalised, Config) []Chunk`. Prose accumulates to `TargetTokens`;
a heading flushes and re-roots `HeadingPath`; tables/code are atomic (split only
above 2×target, on row/line boundaries); consecutive prose chunks overlap by
`OverlapTokens`; `EmbedText` = `"{title} > {heading path}"` + `Content` (empty and
consecutive-duplicate segments dropped, so a title equal to H1 does not repeat).
Exported `parse.RenderTable` lets the chunker re-render split table parts.

## Consequences
- The embedder (STORY-05.5) consumes `EmbedText`; the store (STORY-05.1) persists
  `Content`; the sink (STORY-05.6) supplies `TargetTokens`/`OverlapTokens` from
  tenant settings and maps `Chunk`+embedding→`documents.ChunkInput` — so the split
  is decoupled from embedding and persistence (NFR-MNT-01).
- **Ceiling (ponytail):** the token count is an approximation that undercounts long
  words a real BPE splits, so chunks can run somewhat larger in true model tokens
  than the target; overlap is word-granular, not token-exact. Both are acceptable
  per SPEC-05 §3; the upgrade path is injecting a tiktoken counter via `Config.Count`.
- Size bounds are property-tested: with every input block below the target, no chunk
  (overlap included) exceeds the target; tables/code stay intact or split only on
  safe boundaries. No schema/migration change (`chunks` already exists, ADR-0008).
